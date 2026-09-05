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
	queue     string
	info      *asynq.TaskInfo
	wasActive bool
}

type runtimeKnowledgeTasksKey struct{}

type runtimeKnowledgeScope struct {
	tenantID    uint64
	knowledgeID string
}

type runtimeKnowledgeTasks struct {
	loaded      bool
	err         error
	seen        map[queueTask]bool
	byKnowledge map[runtimeKnowledgeScope][]runtimeQueuedTask
}

func newRuntimeKnowledgeTasks() *runtimeKnowledgeTasks {
	return &runtimeKnowledgeTasks{
		seen:        make(map[queueTask]bool),
		byKnowledge: make(map[runtimeKnowledgeScope][]runtimeQueuedTask),
	}
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
	if index.err != nil {
		return index.err
	}
	if !index.loaded {
		// A handler can finish while its metadata is being read. One delta
		// pass captures descendants emitted before that first scan finished.
		for range 2 {
			if err := a.refreshRuntimeKnowledgeTasks(ctx, index); err != nil {
				return err
			}
		}
	}
	scope := runtimeKnowledgeScope{tenantID: tenantID, knowledgeID: knowledgeID}
	seen := make(map[queueTask]bool)
	var tasks []runtimeQueuedTask
	deadline := time.Now().Add(5 * time.Second)
	for {
		var candidates []runtimeQueuedTask
		needsRefresh := false
		for _, task := range index.byKnowledge[scope] {
			ref := queueTask{queue: task.queue, id: task.info.ID}
			if seen[ref] {
				continue
			}
			seen[ref] = true
			needsRefresh = needsRefresh || task.wasActive
			if task.info.State == asynq.TaskStateCompleted {
				continue
			}
			if task.info.State == asynq.TaskStateArchived {
				running, err := a.hasRuntimeExecutions(ctx, task.queue, task.info.ID)
				if err != nil {
					return err
				}
				if !running {
					continue
				}
			}
			candidates = append(candidates, task)
		}
		tasks = append(tasks, candidates...)
		hadExecutions, err := a.stopRuntimeKnowledgeTasks(ctx, candidates, deadline)
		if err != nil {
			return err
		}
		if !hadExecutions && !needsRefresh {
			break
		}
		// Only running handlers can emit descendants. Pending-only batches
		// use the initial index; completed handlers require an ID delta scan.
		if err := a.refreshRuntimeKnowledgeTasks(ctx, index); err != nil {
			return err
		}
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

func (a *asynqTaskInspector) refreshRuntimeKnowledgeTasks(
	ctx context.Context, index *runtimeKnowledgeTasks,
) (resultErr error) {
	defer func() { index.err = resultErr }()
	// Capture active handlers across all queues before reading waiting work.
	// If one exits during this scan, its descendants are either in the later
	// waiting snapshot or covered by the recorded handler's delta scan.
	queues := runtimeKnowledgeQueues()
	for _, state := range []types.RuntimeTaskState{
		types.RuntimeTaskActive, types.RuntimeTaskPending, types.RuntimeTaskScheduled, types.RuntimeTaskRetry,
		types.RuntimeTaskArchived,
	} {
		for _, queue := range queues {
			ids, err := a.runtimeStateTaskIDs(ctx, queue, state)
			if err != nil {
				return err
			}
			for _, id := range ids {
				ref := queueTask{queue: queue, id: id}
				if index.seen[ref] {
					continue
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				task, err := a.inspector.GetTaskInfo(queue, id)
				if errors.Is(err, asynq.ErrTaskNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				index.seen[ref] = true
				if !runtimeDocumentTask(task.Type) {
					continue
				}
				var payload runtimeTaskPayloadProbe
				if err := json.Unmarshal(task.Payload, &payload); err != nil {
					continue // An unattributable payload cannot match a document.
				}
				running, err := a.hasRuntimeExecutions(ctx, queue, id)
				if err != nil {
					return err
				}
				scope := runtimeKnowledgeScope{tenantID: payload.TenantID, knowledgeID: payload.KnowledgeID}
				index.byKnowledge[scope] = append(index.byKnowledge[scope], runtimeQueuedTask{
					queue: queue, info: task, wasActive: running || state == types.RuntimeTaskActive,
				})
			}
		}
	}
	index.loaded = true
	return nil
}

func (a *asynqTaskInspector) stopRuntimeKnowledgeTasks(
	ctx context.Context, tasks []runtimeQueuedTask, deadline time.Time,
) (bool, error) {
	var stopErr error
	hadExecutions := false
	for _, task := range tasks {
		running, err := a.hasRuntimeExecutions(ctx, task.queue, task.info.ID)
		if err != nil {
			return false, err
		}
		hadExecutions = hadExecutions || running || task.info.State == asynq.TaskStateActive
		current, err := a.inspector.GetTaskInfo(task.queue, task.info.ID)
		if errors.Is(err, asynq.ErrTaskNotFound) {
			continue
		}
		if err == nil && current.State == asynq.TaskStateActive {
			hadExecutions = true
			err = a.inspector.CancelProcessing(task.info.ID)
		}
		stopErr = errors.Join(stopErr, err)
	}
	for _, task := range tasks {
		if err := a.waitRuntimeTaskStopped(ctx, task.queue, task.info.ID, deadline); err != nil {
			stopErr = errors.Join(stopErr, fmt.Errorf("related task %s/%s: %w", task.queue, task.info.ID, err))
		}
	}
	return hadExecutions, stopErr
}
