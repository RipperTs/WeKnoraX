package router

import (
	"context"
	"errors"
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
	prepare := func(_ context.Context, _ string, payload []byte) (interfaces.RuntimeTaskCancellationPlan, error) {
		id := string(payload)
		key := "shared"
		if id == "independent" {
			key = id
		}
		return interfaces.RuntimeTaskCancellationPlan{
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
	phase, err := inspector.runtimePurgePhase(ctx, types.QueueDefault, "failed")
	require.NoError(t, err)
	require.Equal(t, runtimePurgeDelete, phase)
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
	prepare := func(context.Context, string, []byte) (interfaces.RuntimeTaskCancellationPlan, error) {
		return interfaces.RuntimeTaskCancellationPlan{
			Finalize: func(context.Context) error {
				calls++
				if calls <= 3 {
					return errors.New("temporary finalizer failure")
				}
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
	phase, err := inspector.runtimePurgePhase(ctx, types.QueueDefault, info.ID)
	require.NoError(t, err)
	require.Equal(t, runtimePurgeFinalize, phase)

	result, err = inspector.purgeArchivedRuntimeTasks(ctx, types.QueueDefault, prepare)
	require.NoError(t, err)
	require.Equal(t, 2, result.Deleted)
	require.Zero(t, result.Failed)
	_, err = inspector.inspector.GetTaskInfo(types.QueueDefault, info.ID)
	require.ErrorIs(t, err, asynq.ErrTaskNotFound)
	_, err = inspector.inspector.GetTaskInfo(types.QueueDefault, ordinary.ID)
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
