package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/common/redislock"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

// PurgeRuntimeTasks snapshots one state while consumption is paused. Producers
// remain enabled, so work enqueued after the snapshot belongs to a later run.
func (a *asynqTaskInspector) PurgeRuntimeTasks(
	ctx context.Context, queue string, state types.RuntimeTaskState,
	prepare interfaces.RuntimeTaskCancellationPreparer,
) (result types.RuntimeQueuePurgeResult, supported bool, err error) {
	if a == nil || a.inspector == nil || a.redis == nil {
		return result, false, nil
	}
	if !state.Valid() {
		return result, true, fmt.Errorf("invalid task state %q", state)
	}
	err = redislock.WithRenewableLock(ctx, a.redis, "runtime:queue:purge", 30*time.Second, 10*time.Second,
		func(lockCtx context.Context) error {
			var purgeErr error
			result, purgeErr = a.purgeRuntimeState(lockCtx, queue, state, prepare)
			return purgeErr
		})
	return result, true, err
}

func (a *asynqTaskInspector) purgeRuntimeState(
	ctx context.Context, queue string, state types.RuntimeTaskState,
	prepare interfaces.RuntimeTaskCancellationPreparer,
) (result types.RuntimeQueuePurgeResult, err error) {
	if state == types.RuntimeTaskArchived {
		err = a.withRuntimeQueuePaused(ctx, queue, func(pauseCtx context.Context) error {
			var purgeErr error
			result, purgeErr = a.purgeArchivedRuntimeTasks(pauseCtx, queue, prepare)
			return purgeErr
		})
		return result, err
	}
	if state == types.RuntimeTaskCompleted {
		result.Deleted, err = a.inspector.DeleteAllCompletedTasks(queue)
		return result, err
	}
	_, err = a.inspector.GetQueueInfo(queue)
	if isAsynqQueueNotFound(err) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	err = a.withRuntimeQueuePaused(ctx, queue, func(pauseCtx context.Context) error {
		var purgeErr error
		result, purgeErr = a.purgeLiveRuntimeTasks(pauseCtx, queue, state, prepare)
		return purgeErr
	})
	return result, err
}

type runtimePausedQueueKey string

// Nested cancellation may reuse a pause owned by its context. A foreign
// temporary pause must expire before this request acquires its own lease.
func (a *asynqTaskInspector) withRuntimeQueuePaused(
	ctx context.Context, queue string, fn func(context.Context) error,
) error {
	if ctx.Value(runtimePausedQueueKey(queue)) != nil {
		return fn(ctx)
	}
	key := "asynq:{" + queue + "}:paused"
	ttl, err := a.redis.PTTL(ctx, key).Result()
	if err != nil {
		return err
	}
	if ttl == -1 { // An existing manual pause has no expiry.
		return fn(ctx)
	}
	return redislock.WithRenewableLock(ctx, a.redis, key, runtimeTaskLease, runtimeTaskLeaseRenew,
		func(pauseCtx context.Context) error {
			return fn(context.WithValue(pauseCtx, runtimePausedQueueKey(queue), true))
		})
}

func (a *asynqTaskInspector) purgeLiveRuntimeTasks(
	ctx context.Context, queue string, state types.RuntimeTaskState,
	prepare interfaces.RuntimeTaskCancellationPreparer,
) (result types.RuntimeQueuePurgeResult, err error) {
	ctx = context.WithValue(ctx, runtimeKnowledgeTasksKey{}, newRuntimeKnowledgeTasks())
	selected, err := a.snapshotRuntimeState(ctx, queue, state)
	if err != nil {
		return result, err
	}
	// Wiki triggers for the same KB share durable pending operations. Stop
	// those sibling triggers too before cancelling their shared business work.
	tasks, err := a.includeWikiPurgeSiblings(ctx, queue, selected)
	if err != nil {
		return result, err
	}
	for _, task := range tasks {
		if runtimeTaskCancelsKnowledge(task.Type) {
			err = a.withRuntimeKnowledgeQueuesPaused(ctx, func(batchCtx context.Context) error {
				var purgeErr error
				result, purgeErr = a.purgeRuntimeTaskSnapshot(batchCtx, queue, tasks, prepare)
				return purgeErr
			})
			return result, err
		}
	}
	return a.purgeRuntimeTaskSnapshot(ctx, queue, tasks, prepare)
}

type runtimePurgeFinalizer struct {
	remaining int
	finish    interfaces.RuntimeTaskCancellation
	tasks     []*asynq.TaskInfo
	err       error
}

const (
	// Asynq expires archived records after 90 days. Keep recovery state one
	// day longer so it cannot disappear while its quarantined record exists.
	runtimePurgePhaseTTL = 91 * 24 * time.Hour
	runtimePurgeCancel   = "cancel"
	runtimePurgeFinalize = "finalize"
	runtimePurgeDelete   = "delete"
)

func runtimePurgePhaseKey(queue, taskID string) string {
	return "runtime:task:purge-phase:{" + queue + "}:" + taskID
}

func (a *asynqTaskInspector) runtimePurgePhase(ctx context.Context, queue, taskID string) (string, error) {
	phase, err := a.redis.Get(ctx, runtimePurgePhaseKey(queue, taskID)).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return phase, err
}

func (a *asynqTaskInspector) setRuntimePurgePhase(ctx context.Context, queue, taskID, phase string) error {
	return runRuntimePurgeCallback(ctx, func(attemptCtx context.Context) error {
		return a.redis.Set(attemptCtx, runtimePurgePhaseKey(queue, taskID), phase, runtimePurgePhaseTTL).Err()
	})
}

func (a *asynqTaskInspector) deleteRuntimePurgeTask(ctx context.Context, queue, taskID string) error {
	err := a.inspector.DeleteTask(queue, taskID)
	if err != nil && !errors.Is(err, asynq.ErrTaskNotFound) {
		return err
	}
	return runRuntimePurgeCallback(ctx, func(attemptCtx context.Context) error {
		return a.redis.Del(attemptCtx, runtimePurgePhaseKey(queue, taskID)).Err()
	})
}

func (a *asynqTaskInspector) quarantineRuntimeTask(ctx context.Context, queue, taskID, phase string) error {
	// Mark first so concurrent runtime actions cannot requeue or delete the
	// record between its state transition and business cancellation.
	if err := a.setRuntimePurgePhase(ctx, queue, taskID, phase); err != nil {
		return err
	}
	if err := a.inspector.ArchiveTask(queue, taskID); err != nil {
		current, getErr := a.inspector.GetTaskInfo(queue, taskID)
		if errors.Is(getErr, asynq.ErrTaskNotFound) {
			clearErr := a.redis.Del(ctx, runtimePurgePhaseKey(queue, taskID)).Err()
			return errors.Join(asynq.ErrTaskNotFound, clearErr)
		}
		if getErr != nil || current.State != asynq.TaskStateArchived {
			if clearErr := a.redis.Del(ctx, runtimePurgePhaseKey(queue, taskID)).Err(); clearErr != nil {
				return errors.Join(err, clearErr)
			}
			return err
		}
	}
	return nil
}

func runRuntimePurgeCallback(ctx context.Context, callback interfaces.RuntimeTaskCancellation) error {
	if callback == nil {
		return nil
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return errors.Join(lastErr, err)
		}
		if lastErr = callback(ctx); lastErr == nil {
			return nil
		}
		if attempt == 2 {
			break
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return errors.Join(lastErr, ctx.Err())
		case <-timer.C:
		}
	}
	return lastErr
}

func (a *asynqTaskInspector) purgeRuntimeTaskSnapshot(
	ctx context.Context, queue string, tasks []*asynq.TaskInfo,
	prepare interfaces.RuntimeTaskCancellationPreparer,
) (result types.RuntimeQueuePurgeResult, err error) {
	cancellations := make(map[string]interfaces.RuntimeTaskCancellationPlan, len(tasks))
	finalizers := make(map[string]*runtimePurgeFinalizer)
	taskFinalizers := make(map[string]*runtimePurgeFinalizer)
	cancellableTasks := make([]*asynq.TaskInfo, 0, len(tasks))
	result.FailureReasons = make(map[string]int)
	for _, task := range tasks {
		cancel, prepareErr := prepare(ctx, task.Type, task.Payload)
		if errors.Is(prepareErr, types.ErrRuntimeTaskCleanupRequired) {
			result.Failed++
			result.FailureReasons["cleanup_required"]++
			continue
		}
		if prepareErr != nil {
			return result, prepareErr
		}
		cancellableTasks = append(cancellableTasks, task)
		cancellations[task.ID] = cancel
		if cancel.Finalize != nil {
			key := cancel.FinalizeKey
			if key == "" {
				key = task.ID
			}
			group := finalizers[key]
			if group == nil {
				group = &runtimePurgeFinalizer{finish: cancel.Finalize}
				finalizers[key] = group
			}
			group.remaining++
			group.tasks = append(group.tasks, task)
			taskFinalizers[task.ID] = group
		}
	}
	// Required resource cleanup is excluded before any cancellation signal,
	// stop wait, domain update, or queue deletion can affect it.
	tasks = cancellableTasks
	stopErrors := make(map[string]error)
	for _, task := range tasks {
		if task.State == asynq.TaskStateActive {
			if signalErr := a.inspector.CancelProcessing(task.ID); signalErr != nil {
				stopErrors[task.ID] = signalErr
			}
		}
	}
	// One deadline for the whole group, rather than waiting once per task.
	stopDeadline := time.Now().Add(5 * time.Second)
	for _, task := range tasks {
		if stopErrors[task.ID] == nil {
			stopErrors[task.ID] = a.waitRuntimeTaskStopped(ctx, queue, task.ID, stopDeadline)
		}
	}
	// Follow-up triggers created while workers stop are left queued. They may
	// own new durable operations outside the domain snapshot taken above.
	wikiFailures := make(map[string]bool)
	for _, task := range tasks {
		if stopErrors[task.ID] != nil {
			if kbID := runtimeWikiScope(task); kbID != "" {
				wikiFailures[kbID] = true
			}
		}
	}
	quarantineErrors := make(map[string]error)
	for _, task := range tasks {
		if stopErrors[task.ID] != nil || wikiFailures[runtimeWikiScope(task)] {
			continue
		}
		phase := runtimePurgeDelete
		if cancellations[task.ID].Cancel != nil {
			phase = runtimePurgeCancel
		} else if cancellations[task.ID].Finalize != nil {
			phase = runtimePurgeFinalize
		}
		quarantineErrors[task.ID] = a.quarantineRuntimeTask(ctx, queue, task.ID, phase)
		if quarantineErrors[task.ID] != nil {
			if kbID := runtimeWikiScope(task); kbID != "" {
				wikiFailures[kbID] = true
			}
		}
	}
	wikiCancellationErrors := make(map[string]error)
	taskErrors := make(map[string]error)
	taskReasons := make(map[string]string)
	for _, task := range tasks {
		reason := "worker_not_stopped"
		taskErr := stopErrors[task.ID]
		if wikiFailures[runtimeWikiScope(task)] {
			taskErr = errors.New("related Wiki task has not stopped")
		}
		if taskErr == nil && quarantineErrors[task.ID] != nil {
			reason = "queue_quarantine_failed"
			taskErr = quarantineErrors[task.ID]
		}
		if taskErr == nil {
			taskErr = ctx.Err()
		}
		if taskErr == nil {
			reason = "business_cancel_failed"
			kbID := runtimeWikiScope(task)
			var done bool
			if kbID != "" {
				taskErr, done = wikiCancellationErrors[kbID]
			}
			if !done {
				taskErr = runRuntimePurgeCallback(ctx, cancellations[task.ID].Cancel)
				if kbID != "" {
					wikiCancellationErrors[kbID] = taskErr
				}
			}
			if errors.Is(taskErr, types.ErrRuntimeTaskNotStopped) {
				reason = "worker_not_stopped"
			}
		}
		if taskErr == nil {
			nextPhase := runtimePurgeDelete
			if cancellations[task.ID].Finalize != nil {
				nextPhase = runtimePurgeFinalize
			}
			taskErr = a.setRuntimePurgePhase(ctx, queue, task.ID, nextPhase)
			reason = "queue_quarantine_failed"
		}
		taskErrors[task.ID], taskReasons[task.ID] = taskErr, reason
		if taskErr == nil {
			if group := taskFinalizers[task.ID]; group != nil {
				group.remaining--
			}
		}
	}
	for key, group := range finalizers {
		if group.remaining != 0 {
			group.err = errors.New("related runtime task cleanup is incomplete")
			for _, task := range group.tasks {
				if taskErrors[task.ID] == nil {
					taskErrors[task.ID] = group.err
					taskReasons[task.ID] = "business_cancel_failed"
					if phaseErr := a.setRuntimePurgePhase(ctx, queue, task.ID, runtimePurgeCancel); phaseErr != nil {
						taskErrors[task.ID] = errors.Join(taskErrors[task.ID], phaseErr)
					}
				}
			}
			continue
		}
		// The tasks are already non-executable. Keep retrying transient cleanup
		// while the purge lock is owned, and retain the archived records on failure.
		finishCtx, cancelFinish := context.WithTimeout(redislock.OwnershipContext(ctx), 5*time.Second)
		group.err = runRuntimePurgeCallback(finishCtx, group.finish)
		cancelFinish()
		if group.err == nil {
			for _, task := range group.tasks {
				if phaseErr := a.setRuntimePurgePhase(ctx, queue, task.ID, runtimePurgeDelete); phaseErr != nil {
					taskErrors[task.ID] = phaseErr
					taskReasons[task.ID] = "queue_quarantine_failed"
				}
			}
		} else {
			logger.Errorf(ctx, "finalize runtime task cleanup %s: %v", key, group.err)
		}
	}
	for _, task := range tasks {
		taskErr, reason := taskErrors[task.ID], taskReasons[task.ID]
		if taskErr == nil {
			if group := taskFinalizers[task.ID]; group != nil && group.err != nil {
				taskErr, reason = group.err, "business_cancel_failed"
			}
		}
		if taskErr == nil {
			reason = "queue_delete_failed"
			taskErr = a.deleteRuntimePurgeTask(ctx, queue, task.ID)
		}
		if taskErr == nil {
			result.Deleted++
			continue
		}
		result.Failed++
		result.FailureReasons[reason]++
		logger.Errorf(ctx, "purge runtime task queue=%s id=%s: %v", queue, task.ID, taskErr)
	}
	return result, err
}

func (a *asynqTaskInspector) purgeArchivedRuntimeTasks(
	ctx context.Context, queue string, prepare interfaces.RuntimeTaskCancellationPreparer,
) (result types.RuntimeQueuePurgeResult, err error) {
	tasks, err := a.snapshotRuntimeState(ctx, queue, types.RuntimeTaskArchived)
	if err != nil {
		return result, err
	}
	result.FailureReasons = make(map[string]int)
	type recoveryGroup struct {
		finish interfaces.RuntimeTaskCancellation
		tasks  []*asynq.TaskInfo
	}
	groups := make(map[string]*recoveryGroup)

	recordFailure := func(task *asynq.TaskInfo, reason string, taskErr error) {
		result.Failed++
		result.FailureReasons[reason]++
		logger.Errorf(ctx, "purge archived runtime task queue=%s id=%s: %v", queue, task.ID, taskErr)
	}
	deleteTask := func(task *asynq.TaskInfo) {
		if deleteErr := a.deleteRuntimePurgeTask(ctx, queue, task.ID); deleteErr != nil {
			recordFailure(task, "queue_delete_failed", deleteErr)
		} else {
			result.Deleted++
		}
	}

	for _, task := range tasks {
		phase, phaseErr := a.runtimePurgePhase(ctx, queue, task.ID)
		if phaseErr != nil {
			recordFailure(task, "queue_quarantine_failed", phaseErr)
			continue
		}
		switch phase {
		case "", runtimePurgeDelete:
			deleteTask(task)
		case runtimePurgeCancel:
			// Cancellation may have partially changed external state. Preserve
			// its payload for explicit operator recovery instead of replaying it.
			recordFailure(task, "business_cancel_failed", errors.New("runtime task cancellation requires recovery"))
		case runtimePurgeFinalize:
			if prepare == nil {
				recordFailure(task, "business_cancel_failed", errors.New("runtime task finalization is unavailable"))
				continue
			}
			plan, prepareErr := prepare(ctx, task.Type, task.Payload)
			if prepareErr != nil {
				reason := "business_cancel_failed"
				if errors.Is(prepareErr, types.ErrRuntimeTaskCleanupRequired) {
					reason = "cleanup_required"
				}
				recordFailure(task, reason, prepareErr)
				continue
			}
			if plan.Finalize == nil {
				recordFailure(task, "business_cancel_failed", errors.New("runtime task finalizer is missing"))
				continue
			}
			key := plan.FinalizeKey
			if key == "" {
				key = task.ID
			}
			group := groups[key]
			if group == nil {
				group = &recoveryGroup{finish: plan.Finalize}
				groups[key] = group
			}
			group.tasks = append(group.tasks, task)
		default:
			recordFailure(task, "business_cancel_failed", fmt.Errorf("unknown runtime purge phase %q", phase))
		}
	}

	for key, group := range groups {
		finishCtx, cancelFinish := context.WithTimeout(redislock.OwnershipContext(ctx), 5*time.Second)
		finishErr := runRuntimePurgeCallback(finishCtx, group.finish)
		cancelFinish()
		if finishErr != nil {
			for _, task := range group.tasks {
				recordFailure(task, "business_cancel_failed", finishErr)
			}
			logger.Errorf(ctx, "recover runtime task finalization %s: %v", key, finishErr)
			continue
		}
		for _, task := range group.tasks {
			if phaseErr := a.setRuntimePurgePhase(ctx, queue, task.ID, runtimePurgeDelete); phaseErr != nil {
				recordFailure(task, "queue_quarantine_failed", phaseErr)
				continue
			}
			deleteTask(task)
		}
	}
	return result, nil
}

var countRuntimeExecutions = redis.NewScript(`
local clock = redis.call('TIME')
local now = clock[1] * 1000 + math.floor(clock[2] / 1000)
if redis.call('ZCOUNT', KEYS[1], '-inf', now) > 0 then return -1 end
return redis.call('ZCARD', KEYS[1])
`)

func (a *asynqTaskInspector) snapshotRuntimeState(
	ctx context.Context, queue string, state types.RuntimeTaskState,
) ([]*asynq.TaskInfo, error) {
	ids, err := a.runtimeStateTaskIDs(ctx, queue, state)
	if err != nil {
		return nil, err
	}
	tasks := make([]*asynq.TaskInfo, 0, len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		task, err := a.inspector.GetTaskInfo(queue, id)
		if errors.Is(err, asynq.ErrTaskNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		current, err := runtimeTaskState(task.State)
		if err != nil {
			return nil, err
		}
		if current == state {
			tasks = append(tasks, task)
		}
	}
	return tasks, nil
}

func (a *asynqTaskInspector) runtimeStateTaskIDs(
	ctx context.Context, queue string, state types.RuntimeTaskState,
) ([]string, error) {
	key, order := runtimeTaskStateKey(queue, state)
	if order == runtimeTaskListNewestFirst {
		return a.redis.LRange(ctx, key, 0, -1).Result()
	}
	return a.redis.ZRange(ctx, key, 0, -1).Result()
}

func runtimeWikiScope(task *asynq.TaskInfo) string {
	if task.Type != types.TypeWikiIngest && task.Type != types.TypeWikiFinalize {
		return ""
	}
	var payload runtimeTaskPayloadProbe
	if json.Unmarshal(task.Payload, &payload) != nil {
		return ""
	}
	return payload.KnowledgeBaseID
}

func (a *asynqTaskInspector) includeWikiPurgeSiblings(
	ctx context.Context, queue string, selected []*asynq.TaskInfo,
) ([]*asynq.TaskInfo, error) {
	scopes, seen := make(map[string]bool), make(map[string]bool)
	tasks := append([]*asynq.TaskInfo(nil), selected...)
	for _, task := range selected {
		seen[task.ID] = true
		if kbID := runtimeWikiScope(task); kbID != "" {
			scopes[kbID] = true
		}
	}
	if len(scopes) == 0 {
		return tasks, nil
	}
	for _, state := range []types.RuntimeTaskState{
		types.RuntimeTaskPending, types.RuntimeTaskScheduled, types.RuntimeTaskRetry, types.RuntimeTaskActive,
		types.RuntimeTaskArchived,
	} {
		candidates, err := a.snapshotRuntimeState(ctx, queue, state)
		if err != nil {
			return nil, err
		}
		for _, task := range candidates {
			if !seen[task.ID] && scopes[runtimeWikiScope(task)] {
				if state == types.RuntimeTaskArchived {
					running, err := a.hasRuntimeExecutions(ctx, queue, task.ID)
					if err != nil {
						return nil, err
					}
					if !running {
						continue
					}
				}
				seen[task.ID] = true
				tasks = append(tasks, task)
			}
		}
	}
	return tasks, nil
}

func (a *asynqTaskInspector) hasRuntimeExecutions(ctx context.Context, queue, taskID string) (bool, error) {
	count, err := countRuntimeExecutions.Run(ctx, a.redis,
		[]string{types.RuntimeTaskExecutionKey(queue, taskID)}).Int64()
	return count != 0, err
}

func (a *asynqTaskInspector) waitRuntimeTaskStopped(
	ctx context.Context, queue, taskID string, deadline time.Time,
) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		task, err := a.inspector.GetTaskInfo(queue, taskID)
		if err != nil && !errors.Is(err, asynq.ErrTaskNotFound) {
			return err
		}
		running, err := countRuntimeExecutions.Run(ctx, a.redis,
			[]string{types.RuntimeTaskExecutionKey(queue, taskID)}).Int64()
		if err != nil {
			return err
		}
		if running < 0 {
			return fmt.Errorf("%w: execution heartbeat expired without an exit acknowledgement",
				types.ErrRuntimeTaskNotStopped)
		}
		if running == 0 && (task == nil || task.State != asynq.TaskStateActive) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w: cancellation deadline exceeded", types.ErrRuntimeTaskNotStopped)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", types.ErrRuntimeTaskNotStopped, ctx.Err())
		case <-ticker.C:
		}
	}
}
