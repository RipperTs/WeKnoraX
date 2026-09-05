package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
)

type runtimeQueuedTask struct {
	queue string
	info  *asynq.TaskInfo
}

// CancelRuntimeKnowledgeTasks owns record deletion for a document cancellation.
// No record is removed until every related handler has exited and the domain
// callback succeeds. Unlike the interactive cancel API, this is not best-effort.
func (a *asynqTaskInspector) CancelRuntimeKnowledgeTasks(
	ctx context.Context, tenantID uint64, knowledgeID string, cancel interfaces.RuntimeTaskCancellation,
) error {
	paused := make(map[string]bool)
	seen := make(map[queueTask]bool)
	var tasks []runtimeQueuedTask
	deadline := time.Now().Add(5 * time.Second)
	var purge func(context.Context) error
	purge = func(ctx context.Context) error {
		candidates, err := a.snapshotRuntimeKnowledgeTasks(ctx, tenantID, knowledgeID)
		if err != nil {
			return err
		}
		var queues []string
		for _, task := range candidates {
			if !paused[task.queue] {
				paused[task.queue] = true
				queues = append(queues, task.queue)
			}
		}
		if len(queues) > 0 {
			var pause func(context.Context, int) error
			pause = func(ctx context.Context, index int) error {
				if index == len(queues) {
					return purge(ctx)
				}
				return a.withRuntimeQueuePaused(ctx, queues[index], func(pauseCtx context.Context) error {
					return pause(pauseCtx, index+1)
				})
			}
			return pause(ctx, 0)
		}
		start := len(tasks)
		for _, task := range candidates {
			ref := queueTask{queue: task.queue, id: task.info.ID}
			if !seen[ref] {
				seen[ref] = true
				tasks = append(tasks, task)
			}
		}
		if len(tasks) > start {
			if err := a.stopRuntimeKnowledgeTasks(ctx, tasks[start:], deadline); err != nil {
				return err
			}
			// A stopping handler may have emitted a downstream task into a
			// previously empty queue. Pause and stop those descendants too.
			return purge(ctx)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := cancel(ctx); err != nil {
			return err
		}
		for _, task := range tasks {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := a.inspector.DeleteTask(task.queue, task.info.ID); err != nil &&
				!errors.Is(err, asynq.ErrTaskNotFound) {
				return err
			}
		}
		return nil
	}
	return purge(ctx)
}

func (a *asynqTaskInspector) snapshotRuntimeKnowledgeTasks(
	ctx context.Context, tenantID uint64, knowledgeID string,
) ([]runtimeQueuedTask, error) {
	var tasks []runtimeQueuedTask
	for _, queue := range queuesScanned {
		for _, state := range []types.RuntimeTaskState{
			types.RuntimeTaskPending, types.RuntimeTaskScheduled, types.RuntimeTaskRetry, types.RuntimeTaskActive,
			types.RuntimeTaskArchived,
		} {
			candidates, err := a.snapshotRuntimeState(ctx, queue, state)
			if err != nil {
				return nil, err
			}
			for _, task := range candidates {
				_, documentTask := taskTypesForKnowledgeCancel[task.Type]
				if !documentTask && task.Type != types.TypeDataTableSummary && task.Type != types.TypeKnowledgeAutoTag {
					continue
				}
				var payload runtimeTaskPayloadProbe
				if err := json.Unmarshal(task.Payload, &payload); err != nil {
					continue // An unattributable payload cannot match this document.
				}
				if payload.TenantID == tenantID && payload.KnowledgeID == knowledgeID {
					// A cancelled final attempt can already be archived while its
					// handler still runs. Ordinary archived history is untouched.
					if state == types.RuntimeTaskArchived {
						running, err := a.hasRuntimeExecutions(ctx, queue, task.ID)
						if err != nil {
							return nil, err
						}
						if !running {
							continue
						}
					}
					tasks = append(tasks, runtimeQueuedTask{queue: queue, info: task})
				}
			}
		}
	}
	return tasks, nil
}

func (a *asynqTaskInspector) stopRuntimeKnowledgeTasks(
	ctx context.Context, tasks []runtimeQueuedTask, deadline time.Time,
) error {
	var stopErr error
	for _, task := range tasks {
		current, err := a.inspector.GetTaskInfo(task.queue, task.info.ID)
		if errors.Is(err, asynq.ErrTaskNotFound) {
			continue
		}
		if err == nil && current.State == asynq.TaskStateActive {
			err = a.inspector.CancelProcessing(task.info.ID)
		}
		stopErr = errors.Join(stopErr, err)
	}
	for _, task := range tasks {
		if err := a.waitRuntimeTaskStopped(ctx, task.queue, task.info.ID, deadline); err != nil {
			stopErr = errors.Join(stopErr, fmt.Errorf("related task %s/%s: %w", task.queue, task.info.ID, err))
		}
	}
	return stopErr
}
