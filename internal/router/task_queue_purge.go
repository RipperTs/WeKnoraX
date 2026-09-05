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
	runtimePurgeCancel   = "cancel"
	runtimePurgeFinalize = "finalize"
	runtimePurgeDelete   = "delete"
)

type runtimePurgeRecovery struct {
	TaskID   string `json:"task_id"`
	TaskType string `json:"task_type"`
	Payload  []byte `json:"payload"`
	Phase    string `json:"phase"`
}

func runtimePurgeRecoveryKey(queue, taskID string) string {
	return "runtime:task:purge-recovery:{" + queue + "}:" + taskID
}

func runtimePurgeRecoveryIndex(queue string) string {
	return "runtime:task:purge-recoveries:{" + queue + "}"
}

func runtimeTaskDataKey(queue, taskID string) string {
	return "asynq:{" + queue + "}:t:" + taskID
}

func (a *asynqTaskInspector) runtimePurgeRecovery(
	ctx context.Context, queue, taskID string,
) (*runtimePurgeRecovery, error) {
	data, err := a.redis.Get(ctx, runtimePurgeRecoveryKey(queue, taskID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recovery runtimePurgeRecovery
	if err := json.Unmarshal(data, &recovery); err != nil {
		return nil, err
	}
	return &recovery, nil
}

func (a *asynqTaskInspector) saveRuntimePurgeRecovery(
	ctx context.Context, queue string, recovery *runtimePurgeRecovery,
) error {
	data, err := json.Marshal(recovery)
	if err != nil {
		return err
	}
	return runRuntimePurgeCallback(ctx, func(attemptCtx context.Context) error {
		_, err := a.redis.TxPipelined(attemptCtx, func(pipe redis.Pipeliner) error {
			// Unfinished business cleanup must outlive Asynq's capped archive.
			pipe.Set(attemptCtx, runtimePurgeRecoveryKey(queue, recovery.TaskID), data, 0)
			pipe.ZAdd(attemptCtx, runtimePurgeRecoveryIndex(queue), redis.Z{
				Score: float64(time.Now().Unix()), Member: recovery.TaskID,
			})
			return nil
		})
		return err
	})
}

func (a *asynqTaskInspector) setRuntimePurgePhase(ctx context.Context, queue, taskID, phase string) error {
	recovery, err := a.runtimePurgeRecovery(ctx, queue, taskID)
	if err != nil {
		return err
	}
	if recovery == nil {
		return errors.New("runtime purge recovery record is missing")
	}
	recovery.Phase = phase
	return a.saveRuntimePurgeRecovery(ctx, queue, recovery)
}

func (a *asynqTaskInspector) deleteRuntimePurgeRecovery(ctx context.Context, queue, taskID string) error {
	return runRuntimePurgeCallback(ctx, func(attemptCtx context.Context) error {
		_, err := a.redis.TxPipelined(attemptCtx, func(pipe redis.Pipeliner) error {
			pipe.Del(attemptCtx, runtimePurgeRecoveryKey(queue, taskID))
			pipe.ZRem(attemptCtx, runtimePurgeRecoveryIndex(queue), taskID)
			return nil
		})
		return err
	})
}

func (a *asynqTaskInspector) listRuntimePurgeRecoveries(
	ctx context.Context, queue string,
) ([]*runtimePurgeRecovery, error) {
	ids, err := a.redis.ZRange(ctx, runtimePurgeRecoveryIndex(queue), 0, -1).Result()
	if err != nil {
		return nil, err
	}
	recoveries := make([]*runtimePurgeRecovery, 0, len(ids))
	for _, id := range ids {
		recovery, err := a.runtimePurgeRecovery(ctx, queue, id)
		if err != nil {
			return nil, err
		}
		if recovery == nil {
			if err := a.redis.ZRem(ctx, runtimePurgeRecoveryIndex(queue), id).Err(); err != nil {
				return nil, err
			}
			continue
		}
		recoveries = append(recoveries, recovery)
	}
	return recoveries, nil
}

var deleteOrphanedRuntimePurgeTask = redis.NewScript(`
if redis.call("EXISTS", KEYS[1]) == 0 then
	return 0
end
if redis.call("HGET", KEYS[1], "state") ~= "archived" then
	return -1
end
if redis.call("ZSCORE", KEYS[2], ARGV[1]) then
	return -2
end
local unique_key = redis.call("HGET", KEYS[1], "unique_key")
if unique_key and unique_key ~= "" and redis.call("GET", unique_key) == ARGV[1] then
	redis.call("DEL", unique_key)
end
return redis.call("DEL", KEYS[1])
`)

func (a *asynqTaskInspector) deleteOrphanedRuntimePurgeTask(
	ctx context.Context, queue, taskID string,
) error {
	archivedKey, _ := runtimeTaskStateKey(queue, types.RuntimeTaskArchived)
	result, err := deleteOrphanedRuntimePurgeTask.Run(
		ctx, a.redis, []string{runtimeTaskDataKey(queue, taskID), archivedKey}, taskID,
	).Int64()
	if err != nil {
		return err
	}
	switch result {
	case 0, 1:
		return nil
	case -1:
		return errors.New("orphaned runtime purge task is not archived")
	case -2:
		return errors.New("runtime purge task is still present in the archive index")
	default:
		return fmt.Errorf("unexpected orphaned runtime purge delete result %d", result)
	}
}

func (a *asynqTaskInspector) deleteRuntimePurgeTask(ctx context.Context, queue, taskID string) error {
	err := a.inspector.DeleteTask(queue, taskID)
	if err != nil && !errors.Is(err, asynq.ErrTaskNotFound) {
		recovery, recoveryErr := a.runtimePurgeRecovery(ctx, queue, taskID)
		if recoveryErr != nil {
			return errors.Join(err, recoveryErr)
		}
		if recovery == nil || recovery.Phase != runtimePurgeDelete {
			return err
		}
		if orphanErr := a.deleteOrphanedRuntimePurgeTask(ctx, queue, taskID); orphanErr != nil {
			return errors.Join(err, orphanErr)
		}
	}
	return a.deleteRuntimePurgeRecovery(ctx, queue, taskID)
}

func (a *asynqTaskInspector) quarantineRuntimeTask(
	ctx context.Context, queue string, task *asynq.TaskInfo, phase string,
) error {
	// Mark first so concurrent runtime actions cannot requeue or delete the
	// record between its state transition and business cancellation.
	recovery := &runtimePurgeRecovery{
		TaskID: task.ID, TaskType: task.Type, Payload: task.Payload, Phase: phase,
	}
	if err := a.saveRuntimePurgeRecovery(ctx, queue, recovery); err != nil {
		return err
	}
	if err := a.inspector.ArchiveTask(queue, task.ID); err != nil {
		current, getErr := a.inspector.GetTaskInfo(queue, task.ID)
		if errors.Is(getErr, asynq.ErrTaskNotFound) {
			clearErr := a.deleteRuntimePurgeRecovery(ctx, queue, task.ID)
			return errors.Join(asynq.ErrTaskNotFound, clearErr)
		}
		if getErr != nil {
			return errors.Join(err, getErr)
		}
		if current.State != asynq.TaskStateArchived {
			if clearErr := a.deleteRuntimePurgeRecovery(ctx, queue, task.ID); clearErr != nil {
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
		quarantineErrors[task.ID] = a.quarantineRuntimeTask(ctx, queue, task, phase)
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
	recoveries, err := a.listRuntimePurgeRecoveries(ctx, queue)
	if err != nil {
		return result, err
	}
	for _, recovery := range recoveries {
		if recovery.Phase == runtimePurgeCancel && runtimeTaskCancelsKnowledge(recovery.TaskType) {
			ctx = context.WithValue(ctx, runtimeKnowledgeTasksKey{}, newRuntimeKnowledgeTasks())
			err = a.withRuntimeKnowledgeQueuesPaused(ctx, func(batchCtx context.Context) error {
				var purgeErr error
				result, purgeErr = a.purgeArchivedRuntimeTaskRecords(
					batchCtx, queue, tasks, recoveries, prepare,
				)
				return purgeErr
			})
			return result, err
		}
	}
	return a.purgeArchivedRuntimeTaskRecords(ctx, queue, tasks, recoveries, prepare)
}

func (a *asynqTaskInspector) purgeArchivedRuntimeTaskRecords(
	ctx context.Context,
	queue string,
	archivedTasks []*asynq.TaskInfo,
	recoveries []*runtimePurgeRecovery,
	prepare interfaces.RuntimeTaskCancellationPreparer,
) (result types.RuntimeQueuePurgeResult, err error) {
	result.FailureReasons = make(map[string]int)
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

	recoveryByID := make(map[string]*runtimePurgeRecovery, len(recoveries))
	for _, recovery := range recoveries {
		recoveryByID[recovery.TaskID] = recovery
	}
	for _, task := range archivedTasks {
		if recoveryByID[task.ID] == nil {
			deleteTask(task)
		}
	}

	plans := make(map[string]interfaces.RuntimeTaskCancellationPlan, len(recoveries))
	tasks := make([]*asynq.TaskInfo, 0, len(recoveries))
	taskRecoveries := make(map[string]*runtimePurgeRecovery, len(recoveries))
	finalizers := make(map[string]*runtimePurgeFinalizer)
	taskFinalizers := make(map[string]*runtimePurgeFinalizer)
	taskErrors := make(map[string]error)
	taskReasons := make(map[string]string)

	for _, recovery := range recoveries {
		current, getErr := a.inspector.GetTaskInfo(queue, recovery.TaskID)
		if getErr == nil && current.State != asynq.TaskStateArchived {
			archiveErr := a.inspector.ArchiveTask(queue, recovery.TaskID)
			if archiveErr != nil {
				current, getErr = a.inspector.GetTaskInfo(queue, recovery.TaskID)
				if getErr == nil && current.State != asynq.TaskStateArchived {
					recordFailure(current, "queue_quarantine_failed", errors.Join(archiveErr, fmt.Errorf(
						"runtime purge recovery task is in %s state", current.State,
					)))
					continue
				}
				if getErr != nil && !errors.Is(getErr, asynq.ErrTaskNotFound) {
					failureTask := current
					if failureTask == nil {
						failureTask = &asynq.TaskInfo{
							ID: recovery.TaskID, Queue: queue, Type: recovery.TaskType,
						}
					}
					recordFailure(failureTask, "queue_quarantine_failed", errors.Join(archiveErr, getErr))
					continue
				}
			}
		}
		if getErr != nil && !errors.Is(getErr, asynq.ErrTaskNotFound) {
			task := &asynq.TaskInfo{ID: recovery.TaskID, Queue: queue, Type: recovery.TaskType}
			recordFailure(task, "queue_quarantine_failed", getErr)
			continue
		}
		task := &asynq.TaskInfo{
			ID: recovery.TaskID, Queue: queue, Type: recovery.TaskType, Payload: recovery.Payload,
		}
		tasks = append(tasks, task)
		taskRecoveries[task.ID] = recovery
		if recovery.Phase == runtimePurgeDelete {
			continue
		}
		if prepare == nil {
			taskErrors[task.ID] = errors.New("runtime task recovery is unavailable")
			taskReasons[task.ID] = "business_cancel_failed"
			continue
		}
		plan, prepareErr := prepare(ctx, recovery.TaskType, recovery.Payload)
		if prepareErr != nil {
			reason := "business_cancel_failed"
			if errors.Is(prepareErr, types.ErrRuntimeTaskCleanupRequired) {
				reason = "cleanup_required"
			}
			taskErrors[task.ID], taskReasons[task.ID] = prepareErr, reason
			continue
		}
		plans[task.ID] = plan
		if plan.Finalize != nil {
			key := plan.FinalizeKey
			if key == "" {
				key = task.ID
			}
			group := finalizers[key]
			if group == nil {
				group = &runtimePurgeFinalizer{finish: plan.Finalize}
				finalizers[key] = group
			}
			group.remaining++
			group.tasks = append(group.tasks, task)
			taskFinalizers[task.ID] = group
		}
	}

	for _, task := range tasks {
		recovery := taskRecoveries[task.ID]
		if recovery.Phase != runtimePurgeCancel || taskErrors[task.ID] != nil {
			continue
		}
		cancelCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		cancelErr := runRuntimePurgeCallback(cancelCtx, plans[task.ID].Cancel)
		cancel()
		if cancelErr != nil {
			taskErrors[task.ID] = cancelErr
			taskReasons[task.ID] = "business_cancel_failed"
			continue
		}
		nextPhase := runtimePurgeDelete
		if plans[task.ID].Finalize != nil {
			nextPhase = runtimePurgeFinalize
		}
		if phaseErr := a.setRuntimePurgePhase(ctx, queue, task.ID, nextPhase); phaseErr != nil {
			taskErrors[task.ID] = phaseErr
			taskReasons[task.ID] = "queue_quarantine_failed"
			continue
		}
		recovery.Phase = nextPhase
	}

	for _, task := range tasks {
		recovery := taskRecoveries[task.ID]
		if taskErrors[task.ID] == nil && recovery.Phase == runtimePurgeFinalize {
			if group := taskFinalizers[task.ID]; group != nil {
				group.remaining--
			}
		}
	}
	for key, group := range finalizers {
		if group.remaining != 0 {
			group.err = errors.New("related runtime task recovery is incomplete")
			continue
		}
		finishCtx, cancelFinish := context.WithTimeout(redislock.OwnershipContext(ctx), 5*time.Second)
		group.err = runRuntimePurgeCallback(finishCtx, group.finish)
		cancelFinish()
		if group.err != nil {
			logger.Errorf(ctx, "recover runtime task finalization %s: %v", key, group.err)
			continue
		}
		for _, task := range group.tasks {
			if phaseErr := a.setRuntimePurgePhase(ctx, queue, task.ID, runtimePurgeDelete); phaseErr != nil {
				taskErrors[task.ID] = phaseErr
				taskReasons[task.ID] = "queue_quarantine_failed"
				continue
			}
			taskRecoveries[task.ID].Phase = runtimePurgeDelete
		}
	}

	for _, task := range tasks {
		taskErr, reason := taskErrors[task.ID], taskReasons[task.ID]
		if taskErr == nil {
			if group := taskFinalizers[task.ID]; group != nil && group.err != nil {
				taskErr, reason = group.err, "business_cancel_failed"
			}
		}
		if taskErr == nil && taskRecoveries[task.ID].Phase == runtimePurgeDelete {
			deleteTask(task)
			continue
		}
		if taskErr == nil {
			taskErr, reason = errors.New("runtime task recovery phase is incomplete"), "business_cancel_failed"
		}
		recordFailure(task, reason, taskErr)
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
