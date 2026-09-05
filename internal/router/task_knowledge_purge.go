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

type runtimeKnowledgeTasksKey struct{}

type runtimeKnowledgeScope struct {
	tenantID    uint64
	knowledgeID string
}

type runtimeKnowledgeTasks struct {
	byKnowledge map[runtimeKnowledgeScope][]runtimeQueuedTask
}

func runtimeDocumentTask(taskType string) bool {
	_, documentTask := taskTypesForKnowledgeCancel[taskType]
	return documentTask || taskType == types.TypeDataTableSummary || taskType == types.TypeKnowledgeAutoTag
}

func runtimeTaskCancelsKnowledge(taskType string) bool {
	if runtimeDocumentTask(taskType) {
		return true
	}
	switch taskType {
	case types.TypeKnowledgeListReparse, types.TypeWikiIngest, types.TypeWikiFinalize:
		return true
	default:
		return false
	}
}

func runtimeKnowledgeQueues() []string {
	var queues []string
	for _, definition := range types.QueueDefinitions() {
		for _, taskType := range definition.TaskTypes {
			if runtimeDocumentTask(taskType) {
				queues = append(queues, definition.Name)
				break
			}
		}
	}
	return queues
}

// Hold document queues for the whole batch so cached pending tasks cannot
// start between document cancellations. Unrelated queue consumers keep running.
func (a *asynqTaskInspector) withRuntimeKnowledgeQueuesPaused(
	ctx context.Context, fn func(context.Context) error,
) error {
	queues := runtimeKnowledgeQueues()
	var pause func(context.Context, int) error
	pause = func(ctx context.Context, index int) error {
		if index == len(queues) {
			return fn(ctx)
		}
		return a.withRuntimeQueuePaused(ctx, queues[index], func(pauseCtx context.Context) error {
			return pause(pauseCtx, index+1)
		})
	}
	return pause(ctx, 0)
}

// CancelRuntimeKnowledgeTasks owns record deletion for a document cancellation.
// The batch keeps its queues paused and shares an index across all documents.
func (a *asynqTaskInspector) CancelRuntimeKnowledgeTasks(
	ctx context.Context, tenantID uint64, knowledgeID string, cancel interfaces.RuntimeTaskCancellation,
) error {
	index := ctx.Value(runtimeKnowledgeTasksKey{}).(*runtimeKnowledgeTasks)
	scope := runtimeKnowledgeScope{tenantID: tenantID, knowledgeID: knowledgeID}
	var tasks []runtimeQueuedTask
	for _, task := range index.byKnowledge[scope] {
		if task.info.State == asynq.TaskStateArchived {
			running, err := a.hasRuntimeExecutions(ctx, task.queue, task.info.ID)
			if err != nil {
				return err
			}
			if !running {
				continue
			}
		}
		tasks = append(tasks, task)
	}
	if err := a.stopRuntimeKnowledgeTasks(ctx, tasks, time.Now().Add(5*time.Second)); err != nil {
		return err
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

func (a *asynqTaskInspector) snapshotRuntimeKnowledgeTasks(ctx context.Context) (*runtimeKnowledgeTasks, error) {
	index := &runtimeKnowledgeTasks{byKnowledge: make(map[runtimeKnowledgeScope][]runtimeQueuedTask)}
	seen := make(map[queueTask]bool)
	// Fix related task IDs before preparing any cancellation. Never rescan
	// after signalling handlers: newly submitted work belongs to a later run.
	queues := runtimeKnowledgeQueues()
	for _, state := range []types.RuntimeTaskState{
		types.RuntimeTaskActive, types.RuntimeTaskPending, types.RuntimeTaskScheduled, types.RuntimeTaskRetry,
		types.RuntimeTaskArchived,
	} {
		for _, queue := range queues {
			ids, err := a.runtimeStateTaskIDs(ctx, queue, state)
			if err != nil {
				return nil, err
			}
			for _, id := range ids {
				ref := queueTask{queue: queue, id: id}
				if seen[ref] {
					continue
				}
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
				seen[ref] = true
				if !runtimeDocumentTask(task.Type) {
					continue
				}
				var payload runtimeTaskPayloadProbe
				if err := json.Unmarshal(task.Payload, &payload); err != nil {
					continue // An unattributable payload cannot match a document.
				}
				scope := runtimeKnowledgeScope{tenantID: payload.TenantID, knowledgeID: payload.KnowledgeID}
				index.byKnowledge[scope] = append(index.byKnowledge[scope], runtimeQueuedTask{
					queue: queue, info: task,
				})
			}
		}
	}
	return index, nil
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
