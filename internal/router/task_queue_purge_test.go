package router

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/alicebob/miniredis/v2"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newRuntimePurgeTestQueue(t *testing.T) (*asynqTaskInspector, *asynq.Client) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return &asynqTaskInspector{
		inspector: asynq.NewInspectorFromRedisClient(client), redis: client,
	}, asynq.NewClientFromRedisClient(client)
}

func TestRuntimePurgeQuarantinesBeforeCancellation(t *testing.T) {
	inspector, client := newRuntimePurgeTestQueue(t)
	ctx := context.Background()
	var tasks []*asynq.TaskInfo
	for _, id := range []string{"failed", "shared", "independent"} {
		info, err := client.Enqueue(asynq.NewTask("purge:test", []byte(id)), asynq.TaskID(id))
		require.NoError(t, err)
		tasks = append(tasks, info)
	}
	finished := make(map[string]int)
	prepare := func(_ context.Context, _ string, payload []byte,
		_ json.RawMessage,
	) (interfaces.RuntimeTaskCancellationPlan, error) {
		id := string(payload)
		key := "shared"
		if id == "independent" {
			key = id
		}
		return interfaces.RuntimeTaskCancellationPlan{
			Snapshot: json.RawMessage(`{}`),
			Cancel: func(ctx context.Context) error {
				if id == "failed" {
					current, err := inspector.inspector.GetTaskInfo(types.QueueDefault, id)
					require.NoError(t, err)
					require.Equal(t, asynq.TaskStateArchived, current.State)
					// Simulate a late queue-delete failure after cancellation.
					return inspector.redis.HSet(ctx, "asynq:{default}:t:"+id, "state", "active").Err()
				}
				return nil
			},
			FinalizeKey: key,
			Finalize: func(context.Context) error {
				finished[key]++
				return nil
			},
		}, nil
	}
	result, err := inspector.purgeRuntimeTaskSnapshot(ctx, types.QueueDefault, tasks, prepare)
	require.NoError(t, err)
	require.Equal(t, 2, result.Deleted)
	require.Equal(t, 1, result.Failed)
	require.Equal(t, map[string]int{"queue_delete_failed": 1}, result.FailureReasons)
	require.Equal(t, map[string]int{"shared": 1, "independent": 1}, finished)
	_, err = inspector.inspector.GetTaskInfo(types.QueueDefault, "failed")
	require.NoError(t, err)
	recovery, err := inspector.runtimePurgeRecovery(ctx, types.QueueDefault, "failed")
	require.NoError(t, err)
	require.Equal(t, runtimePurgeDelete, recovery.Phase)
	ttl, err := inspector.redis.TTL(ctx, runtimePurgeRecoveryKey(types.QueueDefault, "failed")).Result()
	require.NoError(t, err)
	require.Equal(t, time.Duration(-1), ttl)
	result, err = inspector.purgeArchivedRuntimeTasks(ctx, types.QueueDefault, prepare)
	require.NoError(t, err)
	require.Equal(t, 1, result.Failed)
	recovery, err = inspector.runtimePurgeRecovery(ctx, types.QueueDefault, "failed")
	require.NoError(t, err)
	require.NotNil(t, recovery)
	require.NoError(t, inspector.redis.HSet(ctx, "asynq:{default}:t:failed", "state", "archived").Err())
	task, supported, err := inspector.GetRuntimeTask(ctx, types.QueueDefault, "failed")
	require.NoError(t, err)
	require.True(t, supported)
	require.Empty(t, task.AllowedActions)
	_, err = inspector.RunRuntimeTask(ctx, types.QueueDefault, "failed")
	require.Error(t, err)
	_, err = inspector.DeleteRuntimeTask(ctx, types.QueueDefault, "failed")
	require.Error(t, err)
	_, err = inspector.ForceDeleteRuntimeTask(ctx, types.QueueDefault, "failed")
	require.Error(t, err)
}

func TestRuntimePurgeRecoversFinalizerFromArchivedTask(t *testing.T) {
	inspector, client := newRuntimePurgeTestQueue(t)
	ctx := context.Background()
	info, err := client.Enqueue(asynq.NewTask("purge:test", nil), asynq.TaskID("recover-finalizer"))
	require.NoError(t, err)
	ordinary, err := client.Enqueue(asynq.NewTask("purge:test", nil), asynq.TaskID("ordinary-archived"))
	require.NoError(t, err)
	require.NoError(t, inspector.inspector.ArchiveTask(types.QueueDefault, ordinary.ID))
	calls := 0
	prepare := func(context.Context, string, []byte, json.RawMessage) (interfaces.RuntimeTaskCancellationPlan, error) {
		return interfaces.RuntimeTaskCancellationPlan{
			Snapshot: json.RawMessage(`{}`),
			Finalize: func(context.Context) error {
				calls++
				if calls <= 3 {
					return errors.New("temporary finalizer failure")
				}
				_, getErr := inspector.inspector.GetTaskInfo(types.QueueDefault, ordinary.ID)
				require.NoError(t, getErr, "business recovery must run before ordinary history is deleted")
				return nil
			},
		}, nil
	}

	result, err := inspector.purgeRuntimeTaskSnapshot(ctx, types.QueueDefault, []*asynq.TaskInfo{info}, prepare)
	require.NoError(t, err)
	require.Zero(t, result.Deleted)
	require.Equal(t, 1, result.Failed)
	archived, err := inspector.inspector.GetTaskInfo(types.QueueDefault, info.ID)
	require.NoError(t, err)
	require.Equal(t, asynq.TaskStateArchived, archived.State)
	recovery, err := inspector.runtimePurgeRecovery(ctx, types.QueueDefault, info.ID)
	require.NoError(t, err)
	require.Equal(t, runtimePurgeFinalize, recovery.Phase)
	// Recovery must not depend on Asynq retaining the ID in its capped archive.
	archivedKey, _ := runtimeTaskStateKey(types.QueueDefault, types.RuntimeTaskArchived)
	require.NoError(t, inspector.redis.ZRem(ctx, archivedKey, info.ID).Err())

	result, err = inspector.purgeArchivedRuntimeTasks(ctx, types.QueueDefault, prepare)
	require.NoError(t, err)
	require.Equal(t, 2, result.Deleted)
	require.Zero(t, result.Failed)
	_, err = inspector.inspector.GetTaskInfo(types.QueueDefault, info.ID)
	require.ErrorIs(t, err, asynq.ErrTaskNotFound)
	_, err = inspector.inspector.GetTaskInfo(types.QueueDefault, ordinary.ID)
	require.ErrorIs(t, err, asynq.ErrTaskNotFound)
}

func TestRuntimePurgeRecoversCancellationFromDurableRecord(t *testing.T) {
	inspector, client := newRuntimePurgeTestQueue(t)
	ctx := context.Background()
	info, err := client.Enqueue(asynq.NewTask(
		types.TypeKnowledgeListReparse, []byte(`{"tenant_id":1,"knowledge_ids":["knowledge-1"]}`),
	))
	require.NoError(t, err)
	calls := 0
	prepare := func(context.Context, string, []byte, json.RawMessage) (interfaces.RuntimeTaskCancellationPlan, error) {
		return interfaces.RuntimeTaskCancellationPlan{
			Snapshot: json.RawMessage(`{}`),
			Cancel: func(cancelCtx context.Context) error {
				calls++
				if calls <= 3 {
					require.NotNil(t, cancelCtx.Value(runtimeKnowledgeTasksKey{}))
					return errors.New("temporary cancellation failure")
				}
				require.Nil(t, cancelCtx.Value(runtimeKnowledgeTasksKey{}), "recovery must not inspect current parses")
				for _, queue := range runtimeKnowledgeQueues() {
					if queue != types.QueueDefault {
						require.Zero(t, inspector.redis.Exists(cancelCtx, "asynq:{"+queue+"}:paused").Val())
					}
				}
				return nil
			},
		}, nil
	}

	result, supported, err := inspector.PurgeRuntimeTasks(
		ctx, types.QueueDefault, types.RuntimeTaskPending, prepare,
	)
	require.NoError(t, err)
	require.True(t, supported)
	require.Zero(t, result.Deleted)
	require.Equal(t, 1, result.Failed)
	recovery, err := inspector.runtimePurgeRecovery(ctx, types.QueueDefault, info.ID)
	require.NoError(t, err)
	require.Equal(t, runtimePurgeCancel, recovery.Phase)

	result, supported, err = inspector.PurgeRuntimeTasks(
		ctx, types.QueueDefault, types.RuntimeTaskArchived, prepare,
	)
	require.NoError(t, err)
	require.True(t, supported)
	require.Equal(t, 1, result.Deleted)
	require.Zero(t, result.Failed)
	_, err = inspector.inspector.GetTaskInfo(types.QueueDefault, info.ID)
	require.ErrorIs(t, err, asynq.ErrTaskNotFound)
	recovery, err = inspector.runtimePurgeRecovery(ctx, types.QueueDefault, info.ID)
	require.NoError(t, err)
	require.Nil(t, recovery)
}

func TestRuntimePurgeRecoveryPreventsTaskExecution(t *testing.T) {
	inspector, client := newRuntimePurgeTestQueue(t)
	ctx := context.Background()
	info, err := client.Enqueue(asynq.NewTask("purge:test", nil), asynq.TaskID("quarantined-pending"))
	require.NoError(t, err)
	require.NoError(t, inspector.saveRuntimePurgeRecovery(ctx, types.QueueDefault, &runtimePurgeRecovery{
		TaskID: info.ID, TaskType: "purge:test", Phase: runtimePurgeCancel,
	}))

	worker := asynq.NewServerFromRedisClient(inspector.redis, asynq.Config{
		Concurrency: 1, Queues: map[string]int{types.QueueDefault: 1},
		TaskCheckInterval: 10 * time.Millisecond, ShutdownTimeout: time.Second, LogLevel: asynq.FatalLevel,
	})
	called := make(chan struct{}, 1)
	mux := asynq.NewServeMux()
	mux.Use(runtimeTaskExecutionMiddleware(inspector.redis.(*redis.Client)))
	mux.HandleFunc("purge:test", func(context.Context, *asynq.Task) error {
		called <- struct{}{}
		return nil
	})
	require.NoError(t, worker.Start(mux))
	t.Cleanup(worker.Shutdown)
	require.Eventually(t, func() bool {
		task, getErr := inspector.inspector.GetTaskInfo(types.QueueDefault, info.ID)
		return getErr == nil && task.State == asynq.TaskStateArchived
	}, 2*time.Second, 10*time.Millisecond)
	select {
	case <-called:
		t.Fatal("quarantined task reached its business handler")
	default:
	}
}

func TestRuntimePurgeRecoversTaskBeforeQuarantineCompleted(t *testing.T) {
	inspector, client := newRuntimePurgeTestQueue(t)
	ctx := context.Background()
	info, err := client.Enqueue(asynq.NewTask("purge:test", []byte("payload")), asynq.TaskID("unarchived"))
	require.NoError(t, err)
	require.NoError(t, inspector.saveRuntimePurgeRecovery(ctx, types.QueueDefault, &runtimePurgeRecovery{
		TaskID: info.ID, TaskType: "purge:test", Payload: []byte("payload"), Phase: runtimePurgeCancel,
		Snapshot: json.RawMessage(`{}`),
	}))
	cancelled := 0
	prepare := func(_ context.Context, _ string, payload []byte,
		_ json.RawMessage,
	) (interfaces.RuntimeTaskCancellationPlan, error) {
		require.Equal(t, []byte("payload"), payload)
		return interfaces.RuntimeTaskCancellationPlan{
			Cancel: func(context.Context) error {
				cancelled++
				return nil
			},
		}, nil
	}

	result, err := inspector.purgeArchivedRuntimeTasks(ctx, types.QueueDefault, prepare)
	require.NoError(t, err)
	require.Equal(t, 1, result.Deleted)
	require.Zero(t, result.Failed)
	require.Equal(t, 1, cancelled)
	_, err = inspector.inspector.GetTaskInfo(types.QueueDefault, info.ID)
	require.ErrorIs(t, err, asynq.ErrTaskNotFound)
}

func TestRuntimePurgePreservesActiveResourceCleanup(t *testing.T) {
	inspector, client := newRuntimePurgeTestQueue(t)
	redisClient := inspector.redis.(*redis.Client)
	worker := asynq.NewServerFromRedisClient(inspector.redis, asynq.Config{
		Concurrency: 2, Queues: map[string]int{types.QueueDefault: 1},
		TaskCheckInterval: 10 * time.Millisecond, ShutdownTimeout: time.Second, LogLevel: asynq.FatalLevel,
	})
	started := make(chan struct{}, 2)
	cancelled := make(chan struct{}, 2)
	release := make(chan struct{})
	mux := asynq.NewServeMux()
	mux.Use(runtimeTaskExecutionMiddleware(redisClient))
	for _, taskType := range []string{types.TypeKBDelete, types.TypeIndexDelete} {
		mux.HandleFunc(taskType, func(ctx context.Context, _ *asynq.Task) error {
			started <- struct{}{}
			select {
			case <-ctx.Done():
				cancelled <- struct{}{}
				return ctx.Err()
			case <-release:
				return nil
			}
		})
	}
	require.NoError(t, worker.Start(mux))
	t.Cleanup(func() { close(release); worker.Shutdown() })
	waitForAsynqCancellationSubscriber(t, redisClient)
	for _, taskType := range []string{types.TypeKBDelete, types.TypeIndexDelete} {
		_, err := client.Enqueue(asynq.NewTask(taskType, nil), asynq.TaskID(taskType))
		require.NoError(t, err)
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("cleanup handler did not start")
		}
	}
	preparer := new(service.RuntimeTaskCancellationService).CancelBatch()
	result, supported, err := inspector.PurgeRuntimeTasks(
		context.Background(), types.QueueDefault, types.RuntimeTaskActive, preparer,
	)
	require.NoError(t, err)
	require.True(t, supported)
	require.Zero(t, result.Deleted)
	require.Equal(t, 2, result.Failed)
	require.Equal(t, map[string]int{"cleanup_required": 2}, result.FailureReasons)
	for _, taskType := range []string{types.TypeKBDelete, types.TypeIndexDelete} {
		info, err := inspector.inspector.GetTaskInfo(types.QueueDefault, taskType)
		require.NoError(t, err)
		require.Equal(t, asynq.TaskStateActive, info.State)
	}
	select {
	case <-cancelled:
		t.Fatal("resource cleanup handler received cancellation")
	default:
	}
}

func TestRuntimePurgeDoesNotInferExitFromExpiredHeartbeat(t *testing.T) {
	inspector, client := newRuntimePurgeTestQueue(t)
	info, err := client.Enqueue(asynq.NewTask("purge:test", nil))
	require.NoError(t, err)
	ctx := context.Background()
	key := types.RuntimeTaskExecutionKey(types.QueueDefault, info.ID)
	require.NoError(t, inspector.redis.ZAdd(ctx, key, redis.Z{Score: 1, Member: "lost-execution"}).Err())
	err = inspector.waitRuntimeTaskStopped(ctx, types.QueueDefault, info.ID, time.Now().Add(time.Second))
	require.ErrorIs(t, err, types.ErrRuntimeTaskNotStopped)
	require.EqualValues(t, 1, inspector.redis.ZCard(ctx, key).Val())
	// A later retry's acknowledgement cannot erase the old, unconfirmed execution.
	require.NoError(t, renewRuntimeExecution.Run(ctx, inspector.redis, []string{key},
		"new-execution", runtimeTaskLease.Milliseconds()).Err())
	require.NoError(t, inspector.redis.ZRem(ctx, key, "new-execution").Err())
	err = inspector.waitRuntimeTaskStopped(ctx, types.QueueDefault, info.ID, time.Now().Add(time.Second))
	require.ErrorIs(t, err, types.ErrRuntimeTaskNotStopped)
}

func TestRuntimePurgeWaitsForHandlerAfterAsynqMovesToRetry(t *testing.T) {
	inspector, client := newRuntimePurgeTestQueue(t)
	redisClient := inspector.redis.(*redis.Client)
	worker := asynq.NewServerFromRedisClient(redisClient, asynq.Config{
		Concurrency: 1, Queues: map[string]int{types.QueueDefault: 1},
		TaskCheckInterval: 10 * time.Millisecond, ShutdownTimeout: time.Second, LogLevel: asynq.FatalLevel,
		RetryDelayFunc: func(int, error, *asynq.Task) time.Duration { return time.Hour },
	})
	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	mux := asynq.NewServeMux()
	mux.Use(runtimeTaskExecutionMiddleware(redisClient))
	mux.HandleFunc("purge:test", func(ctx context.Context, _ *asynq.Task) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		return ctx.Err()
	})
	require.NoError(t, worker.Start(mux))
	t.Cleanup(func() { worker.Shutdown() })
	// Always release the handler, including when an earlier assertion fails.
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	waitForAsynqCancellationSubscriber(t, redisClient)
	info, err := client.Enqueue(asynq.NewTask("purge:test", nil), asynq.MaxRetry(3))
	require.NoError(t, err)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start")
	}
	require.NoError(t, inspector.inspector.CancelProcessing(info.ID))
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not receive cancellation")
	}
	waitForTaskState(t, inspector.inspector, info.ID, asynq.TaskStateRetry)
	err = inspector.waitRuntimeTaskStopped(context.Background(), types.QueueDefault, info.ID, time.Now())
	require.ErrorIs(t, err, types.ErrRuntimeTaskNotStopped)
	close(release)
	require.NoError(t, inspector.waitRuntimeTaskStopped(
		context.Background(), types.QueueDefault, info.ID, time.Now().Add(time.Second),
	))
}

type runtimePurgeRedisHook struct {
	process func(context.Context, redis.Cmder, redis.ProcessHook) error
}

func (h runtimePurgeRedisHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h runtimePurgeRedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h runtimePurgeRedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error { return h.process(ctx, cmd, next) }
}

func TestRuntimeHeartbeatFailurePreservesBusinessResult(t *testing.T) {
	for _, test := range []struct {
		name   string
		script *redis.Script
		err    error
	}{
		{name: "renewal failure with success", script: renewRuntimeExecution},
		{
			name: "renewal failure with business failure", script: renewRuntimeExecution,
			err: errors.New("business failure"),
		},
		{name: "registration failure with success", script: startRuntimeExecution},
		{
			name: "registration failure with business failure", script: startRuntimeExecution,
			err: errors.New("business failure"),
		},
		{name: "delayed acknowledgement with success"},
		{name: "delayed acknowledgement with business failure", err: errors.New("business failure")},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			inspector, _ := newRuntimePurgeTestQueue(t)
			client := inspector.redis.(*redis.Client)
			failed := make(chan struct{})
			release := make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			var once sync.Once
			client.AddHook(runtimePurgeRedisHook{process: func(ctx context.Context, cmd redis.Cmder,
				next redis.ProcessHook,
			) error {
				if test.script == nil && cmd.Name() == "zrem" {
					close(failed)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				if test.script != nil && cmd.Name() == "evalsha" && cmd.Args()[1] == test.script.Hash() {
					once.Do(func() { close(failed) })
					return errors.New("injected heartbeat failure")
				}
				return next(ctx, cmd)
			}})
			handler := runtimeTaskExecutionMiddleware(client)(asynq.HandlerFunc(
				func(ctx context.Context, _ *asynq.Task) error {
					if test.script != nil {
						select {
						case <-failed:
						case <-ctx.Done():
							return ctx.Err()
						}
					}
					require.NoError(t, ctx.Err(), "monitoring must not cancel the business context")
					return test.err
				}))
			ctx, cancel := context.WithTimeout(context.Background(), runtimeTaskLeaseRenew+5*time.Second)
			defer cancel()
			startedAt := time.Now()
			err := handler.ProcessTask(ctx, asynq.NewTask("purge:test", nil))
			require.Equal(t, test.err, err, "monitoring must not replace or wrap the business result")
			if test.script == nil {
				require.Less(t, time.Since(startedAt), time.Second, "exit recording must not delay the business result")
				select {
				case <-failed:
				case <-time.After(time.Second):
					t.Fatal("exit recording did not start")
				}
				close(release)
			}
			// Direct invocation has no Asynq ID; its execution entry still must be acknowledged.
			require.Eventually(t, func() bool {
				return client.ZCard(ctx, types.RuntimeTaskExecutionKey("", "")).Val() == 0
			}, time.Second, 10*time.Millisecond)
		})
	}
}

func TestRuntimePurgeAtomicDeleteAllowsTaskIDReuse(t *testing.T) {
	inspector, client := newRuntimePurgeTestQueue(t)
	ctx := context.Background()
	const taskID = "wiki-finalize-kb-1"
	info, err := client.Enqueue(asynq.NewTask("purge:test", []byte("old")),
		asynq.TaskID(taskID), asynq.Unique(time.Hour))
	require.NoError(t, err)
	require.NoError(t, inspector.quarantineRuntimeTask(ctx, types.QueueDefault, info, runtimePurgeDelete, nil))
	redisClient := inspector.redis.(*redis.Client)
	observer := redis.NewClient(redisClient.Options())
	t.Cleanup(func() { _ = observer.Close() })
	successorClient := asynq.NewClientFromRedisClient(observer)
	uniqueKey, err := observer.HGet(ctx, runtimeTaskDataKey(types.QueueDefault, taskID), "unique_key").Result()
	require.NoError(t, err)
	var deleteCommand []interface{}
	responseLost := errors.New("injected lost delete response")
	redisClient.AddHook(runtimePurgeRedisHook{process: func(ctx context.Context, cmd redis.Cmder,
		next redis.ProcessHook,
	) error {
		if err := next(ctx, cmd); err != nil {
			return err
		}
		if deleteCommand != nil || (cmd.Name() != "evalsha" && cmd.Name() != "eval") ||
			observer.Exists(ctx, runtimeTaskDataKey(types.QueueDefault, taskID)).Val() != 0 {
			return nil
		}
		deleteCommand = append([]interface{}(nil), cmd.Args()...)
		require.Zero(t, observer.Exists(ctx, runtimePurgeRecoveryKey(types.QueueDefault, taskID), uniqueKey).Val(),
			"task ID, unique lock and recovery must be released in the same operation")
		require.Zero(t, observer.ZCard(ctx, runtimePurgeRecoveryIndex(types.QueueDefault)).Val())
		_, enqueueErr := successorClient.Enqueue(asynq.NewTask("purge:test", []byte("new")),
			asynq.TaskID(taskID), asynq.ProcessIn(time.Hour))
		require.NoError(t, enqueueErr)
		return responseLost
	}})
	require.ErrorIs(t, inspector.deleteRuntimePurgeTask(ctx, types.QueueDefault, taskID), responseLost)
	require.NotEmpty(t, deleteCommand)
	// A transport retry can replay the successful script after a new task owns the ID.
	require.NoError(t, observer.Do(ctx, deleteCommand...).Err())
	result, supported, err := inspector.PurgeRuntimeTasks(ctx, types.QueueDefault, types.RuntimeTaskArchived, nil)
	require.NoError(t, err)
	require.True(t, supported)
	require.Zero(t, result.Deleted)
	successor, _, err := inspector.GetRuntimeTask(ctx, types.QueueDefault, taskID)
	require.NoError(t, err)
	require.Equal(t, types.RuntimeTaskScheduled, successor.State)
	require.True(t, successor.Allows(types.RuntimeTaskActionRunNow))
}

func TestRuntimePurgePreservesUnfinishedRecovery(t *testing.T) {
	inspector, client := newRuntimePurgeTestQueue(t)
	ctx := context.Background()
	info, err := client.Enqueue(asynq.NewTask("purge:test", nil))
	require.NoError(t, err)
	require.NoError(t, inspector.quarantineRuntimeTask(ctx, types.QueueDefault, info, runtimePurgeCancel, nil))
	require.Error(t, inspector.deleteRuntimePurgeTask(ctx, types.QueueDefault, info.ID))
	_, err = inspector.inspector.GetTaskInfo(types.QueueDefault, info.ID)
	require.NoError(t, err)
	recovery, err := inspector.runtimePurgeRecovery(ctx, types.QueueDefault, info.ID)
	require.NoError(t, err)
	require.Equal(t, runtimePurgeCancel, recovery.Phase)
}

func TestRuntimePurgeStopsHistoryDeletionWhenRequestEnds(t *testing.T) {
	inspector, client := newRuntimePurgeTestQueue(t)
	var history []*asynq.TaskInfo
	for range 3 {
		info, err := client.Enqueue(asynq.NewTask("purge:test", nil))
		require.NoError(t, err)
		require.NoError(t, inspector.inspector.ArchiveTask(types.QueueDefault, info.ID))
		history = append(history, info)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	redisClient := inspector.redis.(*redis.Client)
	observer := redis.NewClient(redisClient.Options())
	t.Cleanup(func() { _ = observer.Close() })
	redisClient.AddHook(runtimePurgeRedisHook{process: func(ctx context.Context, cmd redis.Cmder,
		next redis.ProcessHook,
	) error {
		if err := next(ctx, cmd); err != nil {
			return err
		}
		if observer.Exists(context.Background(), runtimeTaskDataKey(types.QueueDefault, history[0].ID)).Val() == 0 {
			cancel()
		}
		return nil
	}})
	result, err := inspector.purgeArchivedRuntimeTaskRecords(ctx, types.QueueDefault, history, nil, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, result.Deleted)
	for _, task := range history[1:] {
		exists, getErr := observer.Exists(
			context.Background(), runtimeTaskDataKey(types.QueueDefault, task.ID),
		).Result()
		require.NoError(t, getErr)
		require.EqualValues(t, 1, exists)
	}
}

func TestRuntimePurgeQuarantinedActionsSerializeAsArrays(t *testing.T) {
	inspector, client := newRuntimePurgeTestQueue(t)
	ctx := context.Background()
	info, err := client.Enqueue(asynq.NewTask("purge:test", nil))
	require.NoError(t, err)
	require.NoError(t, inspector.quarantineRuntimeTask(ctx, types.QueueDefault, info, runtimePurgeDelete, nil))
	task, _, err := inspector.GetRuntimeTask(ctx, types.QueueDefault, info.ID)
	require.NoError(t, err)
	page, _, err := inspector.ListRuntimeTasks(ctx, types.QueueDefault, types.RuntimeTaskArchived, "", 20)
	require.NoError(t, err)
	require.Len(t, page.Tasks, 1)
	for _, result := range []types.RuntimeTaskInfo{*task, page.Tasks[0]} {
		body, marshalErr := json.Marshal(result)
		require.NoError(t, marshalErr)
		require.Contains(t, string(body), `"allowed_actions":[]`)
	}
}

func TestRuntimePurgeRecoveryCountSurvivesArchiveEviction(t *testing.T) {
	inspector, client := newRuntimePurgeTestQueue(t)
	ctx := context.Background()
	info, err := client.Enqueue(asynq.NewTask("purge:test", nil))
	require.NoError(t, err)
	require.NoError(t, inspector.quarantineRuntimeTask(ctx, types.QueueDefault, info, runtimePurgeDelete, nil))
	archivedKey, _ := runtimeTaskStateKey(types.QueueDefault, types.RuntimeTaskArchived)
	require.NoError(t, inspector.redis.ZRem(ctx, archivedKey, info.ID).Err())
	stats, _, err := inspector.QueueStats(ctx)
	require.NoError(t, err)
	for _, stat := range stats {
		if stat.Name == types.QueueDefault {
			require.Zero(t, stat.Archived)
			require.Equal(t, 1, stat.PurgePending, "the UI must retain its recovery entry when the archive is empty")
		}
	}
	result, _, err := inspector.PurgeRuntimeTasks(ctx, types.QueueDefault, types.RuntimeTaskArchived, nil)
	require.NoError(t, err)
	require.Equal(t, 1, result.Deleted)
	stats, _, err = inspector.QueueStats(ctx)
	require.NoError(t, err)
	for _, stat := range stats {
		require.Zero(t, stat.PurgePending)
	}
}

func TestRuntimePurgePersistsOriginalCancellationSnapshot(t *testing.T) {
	inspector, client := newRuntimePurgeTestQueue(t)
	ctx := context.Background()
	_, err := client.Enqueue(asynq.NewTask("purge:test", nil))
	require.NoError(t, err)
	original := json.RawMessage(`{"operations":[1]}`)
	current := original
	failed := true
	captures := 0
	prepare := func(_ context.Context, _ string, _ []byte,
		snapshot json.RawMessage,
	) (interfaces.RuntimeTaskCancellationPlan, error) {
		if snapshot == nil {
			captures++
			snapshot = current
		}
		return interfaces.RuntimeTaskCancellationPlan{
			Snapshot: snapshot,
			Cancel: func(context.Context) error {
				if failed {
					return errors.New("business temporarily unavailable")
				}
				require.JSONEq(t, string(original), string(snapshot))
				return nil
			},
		}, nil
	}
	result, _, err := inspector.PurgeRuntimeTasks(ctx, types.QueueDefault, types.RuntimeTaskPending, prepare)
	require.NoError(t, err)
	require.Equal(t, 1, result.Failed)
	current = json.RawMessage(`{"operations":[1,2]}`)
	failed = false
	restarted := &asynqTaskInspector{
		inspector: asynq.NewInspectorFromRedisClient(inspector.redis), redis: inspector.redis,
	}
	result, _, err = restarted.PurgeRuntimeTasks(ctx, types.QueueDefault, types.RuntimeTaskArchived, prepare)
	require.NoError(t, err)
	require.Equal(t, 1, result.Deleted)
	require.Zero(t, result.Failed)
	require.Equal(t, 1, captures)
}

func TestRuntimePurgeBlocksRecoveryWithoutOriginalSnapshot(t *testing.T) {
	inspector, client := newRuntimePurgeTestQueue(t)
	ctx := context.Background()
	info, err := client.Enqueue(asynq.NewTask("purge:test", nil))
	require.NoError(t, err)
	require.NoError(t, inspector.quarantineRuntimeTask(ctx, types.QueueDefault, info, runtimePurgeCancel, nil))
	prepare := func(context.Context, string, []byte, json.RawMessage) (interfaces.RuntimeTaskCancellationPlan, error) {
		t.Fatal("missing recovery data must not capture current business work")
		return interfaces.RuntimeTaskCancellationPlan{}, nil
	}
	result, _, err := inspector.PurgeRuntimeTasks(ctx, types.QueueDefault, types.RuntimeTaskArchived, prepare)
	require.NoError(t, err)
	require.Equal(t, 1, result.Failed)
	require.Equal(t, 1, result.FailureReasons["snapshot_missing"])
	_, err = inspector.inspector.GetTaskInfo(types.QueueDefault, info.ID)
	require.NoError(t, err)
}

func TestRuntimePurgePreservesDocumentTasksSubmittedAfterSnapshot(t *testing.T) {
	inspector, client := newRuntimePurgeTestQueue(t)
	ctx := context.Background()
	enqueue := func(queue, taskType, id string, attempt int) {
		payload, err := json.Marshal(types.DocumentProcessPayload{TenantID: 1, KnowledgeID: "doc-1", Attempt: attempt})
		require.NoError(t, err)
		_, err = client.Enqueue(asynq.NewTask(taskType, payload), asynq.Queue(queue), asynq.TaskID(id))
		require.NoError(t, err)
	}
	enqueue(types.QueueDefault, types.TypeDocumentProcess, "original", 1)
	enqueue(types.QueueSummary, types.TypeSummaryGeneration, "original-summary", 1)
	prepare := func(_ context.Context, _ string, _ []byte,
		_ json.RawMessage,
	) (interfaces.RuntimeTaskCancellationPlan, error) {
		// A reparse arrives after task selection and before any cancel callback.
		enqueue(types.QueueDefault, types.TypeDocumentProcess, "later", 2)
		enqueue(types.QueueSummary, types.TypeSummaryGeneration, "later-summary", 2)
		return interfaces.RuntimeTaskCancellationPlan{
			Snapshot: json.RawMessage(`{}`),
			Cancel: func(cancelCtx context.Context) error {
				return inspector.CancelRuntimeKnowledgeTasks(cancelCtx, 1, "doc-1", func(context.Context) error {
					return nil
				})
			},
		}, nil
	}
	result, supported, err := inspector.PurgeRuntimeTasks(ctx, types.QueueDefault, types.RuntimeTaskPending, prepare)
	require.NoError(t, err)
	require.True(t, supported)
	require.Equal(t, 1, result.Deleted)
	require.Zero(t, result.Failed)
	for queue, ids := range map[string][]string{
		types.QueueDefault: {"original", "later"}, types.QueueSummary: {"original-summary", "later-summary"},
	} {
		_, err := inspector.inspector.GetTaskInfo(queue, ids[0])
		require.ErrorIs(t, err, asynq.ErrTaskNotFound)
		later, err := inspector.inspector.GetTaskInfo(queue, ids[1])
		require.NoError(t, err)
		require.Equal(t, asynq.TaskStatePending, later.State)
	}
}

func TestRuntimePurgeNonDocumentTasksKeepsDocumentQueuesRunning(t *testing.T) {
	inspector, client := newRuntimePurgeTestQueue(t)
	_, err := client.Enqueue(asynq.NewTask(types.TypeKBClone, []byte(`{"tenant_id":1,"task_id":"clone-1"}`)),
		asynq.Queue(types.QueueMaintenance))
	require.NoError(t, err)
	prepare := func(ctx context.Context, _ string, _ []byte,
		_ json.RawMessage,
	) (interfaces.RuntimeTaskCancellationPlan, error) {
		for _, queue := range runtimeKnowledgeQueues() {
			paused, err := inspector.redis.Exists(ctx, "asynq:{"+queue+"}:paused").Result()
			require.NoError(t, err)
			require.Zero(t, paused, "non-document cleanup must not pause document queues")
		}
		return interfaces.RuntimeTaskCancellationPlan{Snapshot: json.RawMessage(`{}`)}, nil
	}
	result, supported, err := inspector.PurgeRuntimeTasks(
		context.Background(), types.QueueMaintenance, types.RuntimeTaskPending, prepare,
	)
	require.NoError(t, err)
	require.True(t, supported)
	require.Equal(t, 1, result.Deleted)
}
