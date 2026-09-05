package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

func (a *asynqTaskInspector) SnapshotPendingRuntimeTaskIDs(ctx context.Context, queue string) ([]string, error) {
	key, _ := runtimeTaskStateKey(queue, types.RuntimeTaskPending)
	// Snapshot IDs only. Payloads are read in bounded batches by the caller.
	return a.redis.LRange(ctx, key, 0, -1).Result()
}

func (a *asynqTaskInspector) GetPendingRuntimeCancellationTask(
	ctx context.Context, queue, id string,
) (*types.RuntimeCancellationTask, error) {
	task, err := a.getRuntimeCancellationTask(ctx, queue, id)
	if err != nil || task == nil {
		return nil, err
	}
	if task.State != "pending" {
		return nil, nil
	}
	return task, nil
}

func (a *asynqTaskInspector) getRuntimeCancellationTask(
	ctx context.Context, queue, id string,
) (*types.RuntimeCancellationTask, error) {
	key := "asynq:{" + queue + "}:t:" + id
	fields, err := a.redis.HMGet(ctx, key, "msg", "pending_since", "state").Result()
	if err != nil {
		return nil, err
	}
	if fields[0] == nil || fields[2] == nil {
		return nil, nil
	}
	message, state := fields[0].(string), fields[2].(string)
	task, err := a.inspector.GetTaskInfo(queue, id)
	if errors.Is(err, asynq.ErrTaskNotFound) || errors.Is(err, asynq.ErrQueueNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	current, err := a.redis.HMGet(ctx, key, "msg", "pending_since", "state").Result()
	if err != nil {
		return nil, err
	}
	for i := range fields {
		if current[i] != fields[i] {
			return nil, nil
		}
	}
	pendingSince, _ := fields[1].(string)
	return &types.RuntimeCancellationTask{
		ID: id, Queue: queue, Type: task.Type, Payload: task.Payload, Message: message,
		PendingSince: pendingSince, State: state,
	}, nil
}

// Follow Asynq's pending deletion and unique-lock cleanup, but require both the
// pending state and the captured message. Never delete a newly reused task ID
// or an entry claimed by a worker while business cancellation was running.
// Asynq task IDs are unique within a queue. Removing the one matching entry
// avoids scanning the rest of a large pending list after finding that ID.
var deletePendingCancellationTask = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'state') ~= 'pending' or
   redis.call('HGET', KEYS[1], 'pending_since') ~= ARGV[3] or
   redis.call('HGET', KEYS[1], 'msg') ~= ARGV[2] then
    return 0
end
if redis.call('LREM', KEYS[2], 1, ARGV[1]) == 0 then
    return redis.error_reply('pending task missing from queue')
end
local unique_key = redis.call('HGET', KEYS[1], 'unique_key')
if unique_key and unique_key ~= '' and redis.call('GET', unique_key) == ARGV[1] then
    redis.call('DEL', unique_key)
end
return redis.call('DEL', KEYS[1])
`)

func (a *asynqTaskInspector) DeletePendingRuntimeCancellationTask(
	ctx context.Context, task *types.RuntimeCancellationTask,
) (bool, error) {
	prefix := "asynq:{" + task.Queue + "}:"
	n, err := deletePendingCancellationTask.Run(ctx, a.redis,
		[]string{prefix + "t:" + task.ID, prefix + "pending"}, task.ID, task.Message, task.PendingSince).Int()
	return n == 1, err
}

// CancelRuntimeKnowledgeTasks snapshots each live queue state once for the
// whole document set, then reads one payload at a time. Deletions cannot shift
// pagination, and memory does not grow with the sum of task payload sizes.
func (a *asynqTaskInspector) CancelRuntimeKnowledgeTasks(
	ctx context.Context, targets []types.RuntimeCancelledKnowledge,
) (int, int, error) {
	if len(targets) == 0 {
		return 0, 0, nil
	}
	byID := make(map[string]types.RuntimeCancelledKnowledge, len(targets))
	for _, target := range targets {
		byID[fmt.Sprintf("%d:%s", target.TenantID, target.ID)] = target
	}
	deleted, signaled, failed := 0, 0, 0
	for _, queue := range queuesScanned {
		for _, state := range []string{"pending", "scheduled", "retry", "active"} {
			key := "asynq:{" + queue + "}:" + state
			var ids []string
			var err error
			if state == "pending" || state == "active" {
				ids, err = a.redis.LRange(ctx, key, 0, -1).Result()
			} else {
				ids, err = a.redis.ZRange(ctx, key, 0, -1).Result()
			}
			if err != nil {
				return deleted, signaled, err
			}
			for _, id := range ids {
				if err := ctx.Err(); err != nil {
					return deleted, signaled, err
				}
				task, err := a.getRuntimeCancellationTask(ctx, queue, id)
				if err != nil {
					failed++
					continue
				}
				if task == nil || task.State != state {
					continue
				}
				_, documentTask := taskTypesForKnowledgeCancel[task.Type]
				if !documentTask && task.Type != types.TypeKnowledgeAutoTag && task.Type != types.TypeDataTableSummary {
					continue
				}
				var p struct {
					TenantID    uint64 `json:"tenant_id"`
					KnowledgeID string `json:"knowledge_id"`
					Attempt     int    `json:"attempt"`
				}
				if json.Unmarshal(task.Payload, &p) != nil {
					continue
				}
				target, ok := byID[fmt.Sprintf("%d:%s", p.TenantID, p.KnowledgeID)]
				if !ok || p.Attempt > target.Attempt {
					continue
				}
				prefix := "asynq:{" + queue + "}:"
				result, err := cancelRelatedRuntimeTask.Run(ctx, a.redis,
					[]string{prefix + "t:" + id, key}, id, task.Message, state, task.PendingSince).Int()
				if err != nil {
					failed++
				} else if result == 1 {
					deleted++
				} else if result == 2 {
					signaled++
				}
			}
		}
	}
	if failed > 0 {
		return deleted, signaled, fmt.Errorf("%d related queue actions failed", failed)
	}
	return deleted, signaled, nil
}

// The identity/state check and mutation are atomic. The active path publishes
// Asynq's cancellation signal and retains the task for the worker to finish.
var cancelRelatedRuntimeTask = redis.NewScript(`
if redis.call('HGET',KEYS[1],'msg') ~= ARGV[2] or
   redis.call('HGET',KEYS[1],'state') ~= ARGV[3] then return 0 end
if ARGV[3] == 'active' then
 redis.call('PUBLISH','asynq:cancel',ARGV[1])
 return 2
end
local removed
if ARGV[3] == 'pending' then
 if redis.call('HGET',KEYS[1],'pending_since') ~= ARGV[4] then return 0 end
 removed = redis.call('LREM',KEYS[2],1,ARGV[1])
else
 removed = redis.call('ZREM',KEYS[2],ARGV[1])
end
if removed == 0 then return redis.error_reply('related task missing from queue state') end
local unique_key = redis.call('HGET',KEYS[1],'unique_key')
if unique_key and unique_key ~= '' and redis.call('GET',unique_key) == ARGV[1] then
 redis.call('DEL',unique_key)
end
return redis.call('DEL',KEYS[1])
`)
