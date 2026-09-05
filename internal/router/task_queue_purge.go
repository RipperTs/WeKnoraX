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
		result.Deleted, err = a.inspector.DeleteAllArchivedTasks(queue)
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
		if cancel.AfterDelete != nil {
			key := cancel.AfterDeleteKey
			if key == "" {
				key = task.ID
			}
			group := finalizers[key]
			if group == nil {
				group = &runtimePurgeFinalizer{finish: cancel.AfterDelete}
				finalizers[key] = group
			}
			group.remaining++
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
	wikiCancellationErrors := make(map[string]error)
	for _, task := range tasks {
		reason := "worker_not_stopped"
		taskErr := stopErrors[task.ID]
		if wikiFailures[runtimeWikiScope(task)] {
			taskErr = errors.New("related Wiki task has not stopped")
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
				if cancel := cancellations[task.ID].BeforeDelete; cancel != nil {
					taskErr = cancel(ctx)
				}
				if kbID != "" {
					wikiCancellationErrors[kbID] = taskErr
				}
			}
			if errors.Is(taskErr, types.ErrRuntimeTaskNotStopped) {
				reason = "worker_not_stopped"
			}
		}
		if taskErr == nil {
			reason = "queue_delete_failed"
			taskErr = a.inspector.DeleteTask(queue, task.ID)
			if errors.Is(taskErr, asynq.ErrTaskNotFound) {
				taskErr = nil // A related document cancellation already removed it.
			}
		}
		if taskErr != nil {
			result.Failed++
			result.FailureReasons[reason]++
			logger.Errorf(ctx, "purge runtime task queue=%s id=%s: %v", queue, task.ID, taskErr)
		} else {
			result.Deleted++
			if group := taskFinalizers[task.ID]; group != nil {
				group.remaining--
			}
		}
	}
	for key, group := range finalizers {
		if group.remaining != 0 {
			continue
		}
		// Confirm every dependency was deleted before changing scheduling
		// state. Finish successful groups even if the caller disconnected.
		finishCtx, cancelFinish := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if finishErr := group.finish(finishCtx); finishErr != nil {
			err = errors.Join(err, fmt.Errorf("finalize runtime task cleanup %s: %w", key, finishErr))
		}
		cancelFinish()
	}
	return result, err
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
