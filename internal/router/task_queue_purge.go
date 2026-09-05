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
	"github.com/hibiken/asynq"
)

// PurgeRuntimeTasks snapshots one state while consumption is paused. Producers
// remain enabled, so work enqueued after the snapshot belongs to a later run.
func (a *asynqTaskInspector) PurgeRuntimeTasks(
	ctx context.Context, queue string, state types.RuntimeTaskState,
	cancelTask func(context.Context, string, []byte) error,
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
			result, purgeErr = a.purgeRuntimeState(lockCtx, queue, state, cancelTask)
			return purgeErr
		})
	return result, true, err
}

func (a *asynqTaskInspector) purgeRuntimeState(
	ctx context.Context, queue string, state types.RuntimeTaskState,
	cancelTask func(context.Context, string, []byte) error,
) (result types.RuntimeQueuePurgeResult, err error) {
	if state == types.RuntimeTaskArchived {
		result.Deleted, err = a.inspector.DeleteAllArchivedTasks(queue)
		return result, err
	}
	if state == types.RuntimeTaskCompleted {
		result.Deleted, err = a.inspector.DeleteAllCompletedTasks(queue)
		return result, err
	}
	info, err := a.inspector.GetQueueInfo(queue)
	if isAsynqQueueNotFound(err) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if !info.Paused {
		if err = a.inspector.PauseQueue(queue); err != nil {
			return result, err
		}
		defer func() {
			err = errors.Join(err, a.inspector.UnpauseQueue(queue))
		}()
	}
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
	// A stopping Wiki handler can enqueue its follow-up from a detached
	// cleanup context. Capture those triggers after the handlers have exited.
	previousCount := len(tasks)
	tasks, err = a.includeWikiPurgeSiblings(ctx, queue, tasks)
	if err != nil {
		return result, err
	}
	for _, task := range tasks[previousCount:] {
		stopErrors[task.ID] = a.waitRuntimeTaskStopped(ctx, queue, task.ID, stopDeadline)
	}
	wikiFailures := make(map[string]bool)
	for _, task := range tasks {
		if stopErrors[task.ID] != nil {
			if kbID := runtimeWikiScope(task); kbID != "" {
				wikiFailures[kbID] = true
			}
		}
	}
	result.FailureReasons = make(map[string]int)
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
				taskErr = cancelTask(ctx, task.Type, task.Payload)
				if kbID != "" {
					wikiCancellationErrors[kbID] = taskErr
				}
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
		}
	}
	return result, nil
}

func (a *asynqTaskInspector) snapshotRuntimeState(
	ctx context.Context, queue string, state types.RuntimeTaskState,
) ([]*asynq.TaskInfo, error) {
	key, order := runtimeTaskStateKey(queue, state)
	var ids []string
	var err error
	if order == runtimeTaskListNewestFirst {
		ids, err = a.redis.LRange(ctx, key, 0, -1).Result()
	} else {
		ids, err = a.redis.ZRange(ctx, key, 0, -1).Result()
	}
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
	} {
		candidates, err := a.snapshotRuntimeState(ctx, queue, state)
		if err != nil {
			return nil, err
		}
		for _, task := range candidates {
			if !seen[task.ID] && scopes[runtimeWikiScope(task)] {
				seen[task.ID] = true
				tasks = append(tasks, task)
			}
		}
	}
	return tasks, nil
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
		executions, err := a.redis.SCard(ctx, types.RuntimeTaskExecutionKey(queue, taskID)).Result()
		if err != nil {
			return err
		}
		if executions == 0 && (task == nil || task.State != asynq.TaskStateActive) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return errors.New("worker did not confirm exit before the cancellation deadline")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("worker did not confirm exit: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
