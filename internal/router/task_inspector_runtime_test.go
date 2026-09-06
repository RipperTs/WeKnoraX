package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/alicebob/miniredis/v2"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestQueueStatsDoesNotWarnForQueuesThatDoNotExist(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	inspector := &asynqTaskInspector{
		inspector: asynq.NewInspectorFromRedisClient(client),
		redis:     client,
	}

	var logs bytes.Buffer
	logger.SetOutput(&logs)
	t.Cleanup(func() { logger.SetOutput(os.Stdout) })

	stats, supported, err := inspector.QueueStats(context.Background())
	if err != nil || !supported {
		t.Fatalf("QueueStats: supported=%v err=%v", supported, err)
	}
	if got, want := len(stats), len(types.QueueDefinitions()); got != want {
		t.Fatalf("QueueStats returned %d rows, want %d", got, want)
	}
	for _, stat := range stats {
		if stat.Size != 0 || stat.Pending != 0 || stat.Active != 0 {
			t.Fatalf("missing queue %q should have zero depth: %+v", stat.Name, stat)
		}
	}
	if strings.Contains(logs.String(), "[TaskInspector] queue info") {
		t.Fatalf("missing queues should not emit warnings:\n%s", logs.String())
	}
}

func TestProjectRuntimeTaskRedactsPayloadAndBuildsSafeActions(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"tenant_id":         42,
		"knowledge_base_id": "kb-1",
		"knowledge_id":      "knowledge-1",
		"file_url":          "secret://signed-document-url",
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Unix(1_700_000_000, 0)
	info, err := projectRuntimeTask(&asynq.TaskInfo{
		ID: "task-1", Queue: types.QueueDefault, Type: types.TypeDocumentProcess,
		Payload: payload, State: asynq.TaskStateActive, MaxRetry: 3, Retried: 1,
	}, runtimeWorkerMetadata{started: started, worker: "worker-a:123"})
	if err != nil {
		t.Fatalf("project task: %v", err)
	}
	if info.State != types.RuntimeTaskActive || info.TenantID != 42 ||
		info.KnowledgeBaseID != "kb-1" || info.KnowledgeID != "knowledge-1" {
		t.Fatalf("safe routing metadata missing: %+v", info)
	}
	if info.StartedAt == nil || !info.StartedAt.Equal(started) || info.Worker != "worker-a:123" {
		t.Fatalf("worker metadata missing: %+v", info)
	}
	if len(info.AllowedActions) != 1 || info.AllowedActions[0] != types.RuntimeTaskActionCancel {
		t.Fatalf("active document actions = %v", info.AllowedActions)
	}
}

func TestProjectRuntimeTaskActionsFollowCurrentState(t *testing.T) {
	payload := []byte(`{"tenant_id":7,"knowledge_id":"knowledge-7"}`)
	cases := []struct {
		state asynq.TaskState
		want  []types.RuntimeTaskAction
	}{
		{asynq.TaskStateScheduled, []types.RuntimeTaskAction{types.RuntimeTaskActionCancel, types.RuntimeTaskActionRunNow}},
		{asynq.TaskStateRetry, []types.RuntimeTaskAction{types.RuntimeTaskActionCancel, types.RuntimeTaskActionRunNow}},
		{asynq.TaskStateArchived, []types.RuntimeTaskAction{types.RuntimeTaskActionRunNow, types.RuntimeTaskActionDelete}},
		{asynq.TaskStateCompleted, []types.RuntimeTaskAction{}},
	}
	for _, tc := range cases {
		info, err := projectRuntimeTask(&asynq.TaskInfo{
			ID: "task", Queue: types.QueueDefault, Type: types.TypeDocumentProcess,
			Payload: payload, State: tc.state,
		}, runtimeWorkerMetadata{})
		if err != nil {
			t.Fatalf("state %v: %v", tc.state, err)
		}
		if len(info.AllowedActions) != len(tc.want) {
			t.Fatalf("state %v actions = %v, want %v", tc.state, info.AllowedActions, tc.want)
		}
		for i := range tc.want {
			if info.AllowedActions[i] != tc.want[i] {
				t.Fatalf("state %v actions = %v, want %v", tc.state, info.AllowedActions, tc.want)
			}
		}
	}
}

func TestProjectRuntimeTaskUsesAllowListedBatchMetadata(t *testing.T) {
	payload := []byte(`{
		"tenant_id":9,
		"task_id":"move-1",
		"source_kb_id":"source-kb",
		"target_kb_id":"target-kb",
		"knowledge_ids":["a","b"],
		"created_at":1700000000,
		"content":"must-not-be-projected"
	}`)
	info, err := projectRuntimeTask(&asynq.TaskInfo{
		ID: "task-move", Queue: types.QueueMaintenance, Type: types.TypeKnowledgeMove,
		Payload: payload, State: asynq.TaskStatePending,
	}, runtimeWorkerMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if info.TaskID != "move-1" || info.SourceKBID != "source-kb" ||
		info.TargetKBID != "target-kb" || info.KnowledgeCount != 2 || info.EnqueuedAt == nil {
		t.Fatalf("batch projection mismatch: %+v", info)
	}
	if len(info.AllowedActions) != 1 || info.AllowedActions[0] != types.RuntimeTaskActionCancel {
		t.Fatalf("pending maintenance task must expose business cancellation: %v", info.AllowedActions)
	}
}

func TestRuntimeTaskCursorUsesStateTimeOrderAndSurvivingAnchors(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	inspector := &asynqTaskInspector{redis: client}
	ctx := context.Background()

	pendingKey, _ := runtimeTaskStateKey(types.QueueDefault, types.RuntimeTaskPending)
	if err := client.LPush(ctx, pendingKey, "old", "middle", "new").Err(); err != nil {
		t.Fatal(err)
	}
	ids, err := inspector.listRuntimeTaskIDs(ctx, types.QueueDefault, types.RuntimeTaskPending, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ids, []string{"new", "middle"}; !equalStrings(got, want) {
		t.Fatalf("pending order = %v, want %v", got, want)
	}
	if err = client.LRem(ctx, pendingKey, 0, "middle").Err(); err != nil {
		t.Fatal(err)
	}
	ids, err = inspector.listRuntimeTaskIDs(
		ctx, types.QueueDefault, types.RuntimeTaskPending, []string{"new", "middle"}, 2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ids, []string{"old"}; !equalStrings(got, want) {
		t.Fatalf("anchor fallback order = %v, want %v", got, want)
	}

	scheduledKey, _ := runtimeTaskStateKey(types.QueueDefault, types.RuntimeTaskScheduled)
	if err = client.ZAdd(ctx, scheduledKey,
		redis.Z{Score: 30, Member: "later"},
		redis.Z{Score: 10, Member: "next"},
		redis.Z{Score: 20, Member: "after-next"},
	).Err(); err != nil {
		t.Fatal(err)
	}
	ids, err = inspector.listRuntimeTaskIDs(ctx, types.QueueDefault, types.RuntimeTaskScheduled, nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ids, []string{"next", "after-next", "later"}; !equalStrings(got, want) {
		t.Fatalf("scheduled order = %v, want %v", got, want)
	}

	archivedKey, _ := runtimeTaskStateKey(types.QueueDefault, types.RuntimeTaskArchived)
	if err = client.ZAdd(ctx, archivedKey,
		redis.Z{Score: 10, Member: "old-failure"},
		redis.Z{Score: 30, Member: "new-failure"},
		redis.Z{Score: 20, Member: "middle-failure"},
	).Err(); err != nil {
		t.Fatal(err)
	}
	ids, err = inspector.listRuntimeTaskIDs(ctx, types.QueueDefault, types.RuntimeTaskArchived, nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ids, []string{"new-failure", "middle-failure", "old-failure"}; !equalStrings(got, want) {
		t.Fatalf("archived order = %v, want %v", got, want)
	}
}

func TestRuntimeTaskCursorIsBoundToQueueAndState(t *testing.T) {
	raw, err := encodeRuntimeTaskCursor(
		types.QueueDefault,
		types.RuntimeTaskArchived,
		[]string{"task-1", "task-2"},
	)
	if err != nil {
		t.Fatal(err)
	}
	anchors, err := decodeRuntimeTaskCursor(raw, types.QueueDefault, types.RuntimeTaskArchived)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(anchors, []string{"task-1", "task-2"}) {
		t.Fatalf("anchors = %v", anchors)
	}
	if _, err = decodeRuntimeTaskCursor(raw, types.QueueDefault, types.RuntimeTaskRetry); err == nil {
		t.Fatal("cursor from another state should be rejected")
	}
	if _, err = decodeRuntimeTaskCursor("not-base64", types.QueueDefault, types.RuntimeTaskArchived); err == nil {
		t.Fatal("malformed cursor should be rejected")
	}
}

func TestListRuntimeTasksPaginatesNewestPendingTasksWithoutOverlap(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	asynqClient := asynq.NewClientFromRedisClient(client)
	inspector := &asynqTaskInspector{
		inspector: asynq.NewInspectorFromRedisClient(client),
		redis:     client,
	}

	for _, id := range []string{"old", "middle", "new"} {
		_, err := asynqClient.Enqueue(
			asynq.NewTask(types.TypeDocumentProcess, []byte(`{"tenant_id":1,"knowledge_id":"knowledge-1"}`)),
			asynq.Queue(types.QueueDefault),
			asynq.TaskID(id),
		)
		if err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
	}
	pendingKey, _ := runtimeTaskStateKey(types.QueueDefault, types.RuntimeTaskPending)
	if err := client.LInsert(context.Background(), pendingKey, "AFTER", "new", "already-gone").Err(); err != nil {
		t.Fatalf("insert stale task id: %v", err)
	}

	first, supported, err := inspector.ListRuntimeTasks(
		context.Background(), types.QueueDefault, types.RuntimeTaskPending, "", 2,
	)
	if err != nil || !supported {
		t.Fatalf("first page: supported=%v err=%v", supported, err)
	}
	if got, want := runtimeTaskIDs(first.Tasks), []string{"new", "middle"}; !equalStrings(got, want) {
		t.Fatalf("first page = %v, want %v", got, want)
	}
	if !first.HasMore || first.NextCursor == "" {
		t.Fatalf("first page cursor missing: %+v", first)
	}

	second, supported, err := inspector.ListRuntimeTasks(
		context.Background(), types.QueueDefault, types.RuntimeTaskPending, first.NextCursor, 2,
	)
	if err != nil || !supported {
		t.Fatalf("second page: supported=%v err=%v", supported, err)
	}
	if got, want := runtimeTaskIDs(second.Tasks), []string{"old"}; !equalStrings(got, want) {
		t.Fatalf("second page = %v, want %v", got, want)
	}
	if second.HasMore || second.NextCursor != "" {
		t.Fatalf("unexpected continuation after final page: %+v", second)
	}
}

func runtimeTaskIDs(tasks []types.RuntimeTaskInfo) []string {
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	return ids
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func TestRuntimeCancellationDeleteChecksTaskIdentityAndUniqueLock(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	inspector := &asynqTaskInspector{redis: client, inspector: asynq.NewInspectorFromRedisClient(client)}
	enqueuer := asynq.NewClientFromRedisClient(client)
	ctx := context.Background()
	payload := []byte(`{"tenant_id":42,"knowledge_id":"doc","attempt":1}`)
	_, err := enqueuer.Enqueue(asynq.NewTask(types.TypeQuestionGeneration, payload),
		asynq.Queue(types.QueueQuestion), asynq.TaskID("reused"))
	require.NoError(t, err)
	captured, err := inspector.GetPendingRuntimeCancellationTask(ctx, types.QueueQuestion, "reused")
	require.NoError(t, err)
	require.NotNil(t, captured)
	require.NoError(t, inspector.inspector.DeleteTask(types.QueueQuestion, "reused"))
	_, err = enqueuer.Enqueue(asynq.NewTask(types.TypeQuestionGeneration, payload),
		asynq.Queue(types.QueueQuestion), asynq.TaskID("reused"), asynq.Unique(time.Minute))
	require.NoError(t, err)
	captured.Reservation = "old-reservation"
	reserved, err := inspector.ReservePendingRuntimeCancellationTask(ctx, captured)
	require.NoError(t, err)
	require.False(t, reserved)
	current, err := inspector.GetPendingRuntimeCancellationTask(ctx, types.QueueQuestion, "reused")
	require.NoError(t, err)
	require.NotNil(t, current)
	uniqueKey, err := client.HGet(ctx, "asynq:{question}:t:reused", "unique_key").Result()
	require.NoError(t, err)
	current.Reservation = "current-reservation"
	reserved, err = inspector.ReservePendingRuntimeCancellationTask(ctx, current)
	require.NoError(t, err)
	require.True(t, reserved)
	// Asynq claims only from pending; the reservation must keep the unique
	// lock and task record while preventing a concurrent worker claim.
	require.ErrorIs(t, client.RPopLPush(ctx, "asynq:{question}:pending", "asynq:{question}:active").Err(), redis.Nil)
	require.Equal(t, "reused", client.Get(ctx, uniqueKey).Val())
	info, _, err := inspector.GetRuntimeTask(ctx, types.QueueQuestion, "reused")
	require.NoError(t, err)
	require.Equal(t, types.RuntimeTaskArchived, info.State)
	require.Empty(t, info.AllowedActions)
	_, err = inspector.RunRuntimeTask(ctx, types.QueueQuestion, "reused")
	require.Error(t, err)
	deadline, err := client.HGet(ctx, "asynq:{question}:t:reused", "runtime_cancel_until").Int64()
	require.NoError(t, err)
	archiveScore, err := client.ZScore(ctx, "asynq:{question}:archived", "reused").Result()
	require.NoError(t, err)
	require.Greater(t, archiveScore, float64(deadline))
	// Asynq v0.26.0 trims the oldest archive entries at its 10,000-task
	// limit. Exercise that native path while this reservation is held.
	const archiveLimit = 10000
	archiveKey := "asynq:{question}:archived"
	archives := make([]redis.Z, archiveLimit)
	archiveIDs := make([]any, archiveLimit)
	for i := range archives {
		id := fmt.Sprintf("z-capacity-%05d", i)
		archives[i] = redis.Z{Score: float64(time.Now().Unix()), Member: id}
		archiveIDs[i] = id
	}
	require.NoError(t, client.ZAdd(ctx, archiveKey, archives...).Err())
	_, err = enqueuer.Enqueue(asynq.NewTask(types.TypeQuestionGeneration, payload),
		asynq.Queue(types.QueueQuestion), asynq.TaskID("zz-capacity-trigger"))
	require.NoError(t, err)
	require.NoError(t, inspector.inspector.ArchiveTask(types.QueueQuestion, "zz-capacity-trigger"))
	_, err = client.ZScore(ctx, archiveKey, "reused").Result()
	require.NoError(t, err, "native archive trimming must retain the reservation")
	require.Equal(t, current.Message, client.HGet(ctx, "asynq:{question}:t:reused", "msg").Val())
	require.NoError(t, client.ZRem(ctx, archiveKey, archiveIDs...).Err())
	require.NoError(t, inspector.inspector.DeleteTask(types.QueueQuestion, "zz-capacity-trigger"))
	for i, deadline := range []int64{time.Now().Add(time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()} {
		require.NoError(t, client.HSet(ctx, "asynq:{question}:t:reused", "runtime_cancel_until", deadline).Err())
		id := fmt.Sprintf("dead-letter-%d", i)
		_, err := enqueuer.Enqueue(asynq.NewTask(types.TypeQuestionGeneration, []byte(id)),
			asynq.Queue(types.QueueQuestion), asynq.TaskID(id), asynq.Unique(time.Minute))
		require.NoError(t, err)
		deadLetterKey := client.HGet(ctx, "asynq:{question}:t:"+id, "unique_key").Val()
		require.NoError(t, inspector.inspector.ArchiveTask(types.QueueQuestion, id))
		deleted, supported, err := inspector.PurgeArchivedRuntimeTasks(ctx, types.QueueQuestion)
		require.NoError(t, err)
		require.True(t, supported)
		require.Equal(t, 1, deleted)
		require.ErrorIs(t, client.Get(ctx, deadLetterKey).Err(), redis.Nil)
		require.Equal(t, []string{"reused"}, client.ZRange(ctx, "asynq:{question}:archived", 0, -1).Val())
		require.Equal(t, current.Message, client.HGet(ctx, "asynq:{question}:t:reused", "msg").Val())
		require.Equal(t, "reused", client.Get(ctx, uniqueKey).Val())
	}
	// Skipping a task releases it without changing its captured identity.
	require.NoError(t, inspector.ReleaseRuntimeCancellationTask(ctx, current))
	require.Equal(t, current.PendingSince, client.HGet(ctx, "asynq:{question}:t:reused", "pending_since").Val())
	reserved, err = inspector.ReservePendingRuntimeCancellationTask(ctx, current)
	require.NoError(t, err)
	require.True(t, reserved)
	deleted, err := inspector.DeleteReservedRuntimeCancellationTask(ctx, captured)
	require.NoError(t, err)
	require.False(t, deleted)
	deleted, err = inspector.DeleteReservedRuntimeCancellationTask(ctx, current)
	require.NoError(t, err)
	require.True(t, deleted)
	require.ErrorIs(t, client.Get(ctx, uniqueKey).Err(), redis.Nil)
	// If the worker wins first, no reservation can be obtained.
	_, err = enqueuer.Enqueue(asynq.NewTask(types.TypeQuestionGeneration, payload),
		asynq.Queue(types.QueueQuestion), asynq.TaskID("claimed"))
	require.NoError(t, err)
	current, err = inspector.GetPendingRuntimeCancellationTask(ctx, types.QueueQuestion, "claimed")
	require.NoError(t, err)
	require.NoError(t, client.RPopLPush(ctx, "asynq:{question}:pending", "asynq:{question}:active").Err())
	require.NoError(t, client.HSet(ctx, "asynq:{question}:t:claimed", "state", "active").Err())
	current.Reservation = "too-late"
	reserved, err = inspector.ReservePendingRuntimeCancellationTask(ctx, current)
	require.NoError(t, err)
	require.False(t, reserved)
}

func TestRuntimeCancellationRelatedSweepScopesTenantAttemptAndState(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	inspector := &asynqTaskInspector{redis: client, inspector: asynq.NewInspectorFromRedisClient(client)}
	enqueuer := asynq.NewClientFromRedisClient(client)
	ctx := context.Background()
	for _, item := range []struct {
		id        string
		tenant    uint64
		attempt   int
		scheduled bool
	}{
		{"pending", 42, 1, false},
		{"scheduled", 42, 1, true},
		{"new-attempt", 42, 2, false},
		{"unknown-attempt", 42, 0, false},
		{"other-tenant", 43, 1, false},
	} {
		payload, err := json.Marshal(map[string]any{
			"tenant_id": item.tenant, "knowledge_id": "doc", "attempt": item.attempt,
		})
		require.NoError(t, err)
		options := []asynq.Option{asynq.Queue(types.QueueQuestion), asynq.TaskID(item.id)}
		if item.scheduled {
			options = append(options, asynq.ProcessIn(time.Hour))
		}
		_, err = enqueuer.Enqueue(asynq.NewTask(types.TypeQuestionGeneration, payload), options...)
		require.NoError(t, err)
	}
	deleted, signaled, err := inspector.CancelRuntimeKnowledgeTasks(ctx,
		[]types.RuntimeCancelledKnowledge{{TenantID: 42, ID: "doc", Attempt: 1}})
	require.NoError(t, err)
	require.Equal(t, 2, deleted)
	require.Zero(t, signaled)
	for _, id := range []string{"new-attempt", "unknown-attempt", "other-tenant"} {
		_, err := inspector.inspector.GetTaskInfo(types.QueueQuestion, id)
		require.NoError(t, err)
	}
}

func TestRuntimeCancellationProcessesSnapshotAfterRequestCloses(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	inspector := &asynqTaskInspector{redis: client, inspector: asynq.NewInspectorFromRedisClient(client)}
	enqueuer := asynq.NewClientFromRedisClient(client)
	for i := 0; i < 201; i++ {
		_, err := enqueuer.Enqueue(asynq.NewTask(types.TypeKnowledgeListReparse, []byte(`{"tenant_id":42}`)),
			asynq.Queue(types.QueueMaintenance))
		require.NoError(t, err)
	}
	for _, task := range []*asynq.Task{
		asynq.NewTask(types.TypeKnowledgeListReparse, []byte(`{broken`)),
		asynq.NewTask(types.TypeKBDelete, []byte(`{"tenant_id":42}`)),
	} {
		_, err := enqueuer.Enqueue(task, asynq.Queue(types.QueueMaintenance))
		require.NoError(t, err)
	}
	svc := service.NewRuntimeTaskCancellationService(inspector, client, nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	job, err := svc.Start(ctx, types.QueueMaintenance)
	require.NoError(t, err)
	require.Equal(t, 203, job.Total)
	cancel()
	_, err = enqueuer.Enqueue(asynq.NewTask(types.TypeKnowledgeListReparse, []byte(`{"tenant_id":42}`)),
		asynq.Queue(types.QueueMaintenance), asynq.TaskID("new-task"))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		job, err = svc.Get(context.Background(), types.QueueMaintenance)
		return err == nil && job.Status != "running"
	}, 20*time.Second, 20*time.Millisecond)
	require.Equal(t, "completed", job.Status)
	require.Equal(t, 203, job.Processed)
	require.Equal(t, 201, job.Cancelled)
	require.Equal(t, 1, job.Skipped)
	require.Equal(t, 1, job.Failed)
	_, err = inspector.inspector.GetTaskInfo(types.QueueMaintenance, "new-task")
	require.NoError(t, err)
	info, err := inspector.inspector.GetQueueInfo(types.QueueMaintenance)
	require.NoError(t, err)
	require.Equal(t, 3, info.Pending)
	require.Zero(t, info.Archived, "invalid payloads must remain in pending without being reserved")

	t.Run("manual_edits_keep_fifo", func(t *testing.T) {
		manualCtx := context.Background()
		const edits = 205 // Cross cancellation batch boundaries.
		for i := 0; i < edits; i++ {
			payload, err := json.Marshal(types.ManualProcessPayload{
				TenantID: 42, KnowledgeID: "doc", KnowledgeBaseID: "kb",
				Content: fmt.Sprintf("edit-%d", i), NeedCleanup: true,
			})
			require.NoError(t, err)
			_, err = enqueuer.Enqueue(asynq.NewTask(types.TypeManualProcess, payload),
				asynq.Queue(types.QueueDefault), asynq.TaskID(fmt.Sprintf("edit-%d", i)))
			require.NoError(t, err)
		}
		pendingKey := "asynq:{default}:pending"
		before, err := client.LRange(manualCtx, pendingKey, 0, -1).Result()
		require.NoError(t, err)
		job, err := svc.Start(manualCtx, types.QueueDefault)
		require.NoError(t, err)
		_, err = enqueuer.Enqueue(asynq.NewTask(types.TypeManualProcess,
			[]byte(`{"tenant_id":42,"knowledge_id":"doc","content":"newest"}`)),
			asynq.Queue(types.QueueDefault), asynq.TaskID("newest-edit"))
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			job, err = svc.Get(manualCtx, types.QueueDefault)
			return err == nil && job.Status != "running"
		}, 20*time.Second, 20*time.Millisecond)
		require.Equal(t, "completed", job.Status)
		require.Equal(t, edits, job.Skipped)
		require.Zero(t, job.Cancelled)
		require.Zero(t, job.Failed)
		cancelled, err := svc.CancelOne(manualCtx, types.QueueDefault, "edit-0")
		require.NoError(t, err)
		require.False(t, cancelled)
		require.Equal(t, append([]string{"newest-edit"}, before...), client.LRange(manualCtx, pendingKey, 0, -1).Val())
		// Asynq consumes from the tail: older edits must still run first.
		for i := 0; i < edits; i++ {
			require.Equal(t, fmt.Sprintf("edit-%d", i), client.RPop(manualCtx, pendingKey).Val())
		}
		require.Equal(t, "newest-edit", client.RPop(manualCtx, pendingKey).Val())
	})

	for _, failure := range []string{"none", "before_update", "after_update", "commit"} {
		t.Run("sync_business_failure_"+failure, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = sqlDB.Close() })
			require.NoError(t, db.Exec("PRAGMA foreign_keys = ON").Error)
			require.NoError(t, db.Exec("CREATE TABLE sync_statuses (status TEXT PRIMARY KEY)").Error)
			require.NoError(t, db.Exec("INSERT INTO sync_statuses VALUES ('running')").Error)
			if failure != "commit" {
				require.NoError(t, db.Exec("INSERT INTO sync_statuses VALUES ('canceled')").Error)
			}
			require.NoError(t, db.Exec(`CREATE TABLE sync_logs
  (id TEXT PRIMARY KEY,tenant_id INTEGER,status TEXT,started_at DATETIME,finished_at DATETIME,
   error_message TEXT,updated_at DATETIME,
   FOREIGN KEY(status) REFERENCES sync_statuses(status) DEFERRABLE INITIALLY DEFERRED)`).Error)
			require.NoError(t, db.Exec(`INSERT INTO sync_logs(id,tenant_id,status,started_at) VALUES (?,?,?,?)`,
				"sync", 42, "running", time.Now().Add(-time.Minute)).Error)
			id := "sync-" + failure
			_, err = enqueuer.Enqueue(asynq.NewTask(types.TypeDataSourceSync,
				[]byte(`{"tenant_id":42,"sync_log_id":"sync"}`)), asynq.Queue(types.QueueSync), asynq.TaskID(id))
			require.NoError(t, err)
			businessReached := false
			assertReserved := func(tx *gorm.DB) {
				businessReached = true
				require.Equal(t, "archived", client.HGet(context.Background(), "asynq:{sync}:t:"+id, "state").Val())
				require.ErrorIs(t, client.RPopLPush(context.Background(),
					"asynq:{sync}:pending", "asynq:{sync}:active").Err(), redis.Nil)
				if failure == "before_update" {
					require.Error(t, tx.AddError(fmt.Errorf("business update failed")))
				}
			}
			require.NoError(t, db.Callback().Update().Before("gorm:update").Register("assert_reserved", assertReserved))
			if failure == "after_update" {
				failAfterUpdate := func(tx *gorm.DB) {
					require.EqualValues(t, 1, tx.RowsAffected)
					require.Error(t, tx.AddError(fmt.Errorf("business update failed before commit")))
				}
				require.NoError(t, db.Callback().Update().After("gorm:update").
					Register("fail_after_update", failAfterUpdate))
			}
			syncSvc := service.NewRuntimeTaskCancellationService(inspector, client,
				repository.NewRuntimeTaskCancellationRepository(db), nil, nil)
			cancelled, err := syncSvc.CancelOne(context.Background(), types.QueueSync, id)
			require.True(t, businessReached)
			if failure != "none" {
				require.Error(t, err)
				require.False(t, cancelled)
				if failure == "commit" {
					require.NotErrorIs(t, err, types.ErrRuntimeCancellationNotCommitted)
				} else {
					require.ErrorIs(t, err, types.ErrRuntimeCancellationNotCommitted)
				}
				task, err := inspector.inspector.GetTaskInfo(types.QueueSync, id)
				require.NoError(t, err)
				if failure == "commit" {
					require.Equal(t, asynq.TaskStateArchived, task.State)
				} else {
					require.Equal(t, asynq.TaskStatePending, task.State)
					pending := client.LRange(context.Background(), "asynq:{sync}:pending", 0, -1).Val()
					require.Equal(t, []string{id}, pending)
					hasToken := client.HExists(context.Background(), "asynq:{sync}:t:"+id, "runtime_cancel_token").Val()
					require.False(t, hasToken)
					var log types.SyncLog
					require.NoError(t, db.First(&log, "id = ?", "sync").Error)
					require.Equal(t, "running", log.Status)
				}
				require.NoError(t, inspector.inspector.DeleteTask(types.QueueSync, id))
			} else {
				require.NoError(t, err)
				require.True(t, cancelled)
				var log types.SyncLog
				require.NoError(t, db.First(&log, "id = ?", "sync").Error)
				require.Equal(t, "canceled", log.Status)
				_, err = inspector.inspector.GetTaskInfo(types.QueueSync, id)
				require.ErrorIs(t, err, asynq.ErrTaskNotFound)
			}
		})
	}
}

// Run against an explicitly supplied disposable local Redis, never the app's
// configured Redis. Database 15 is emptied between benchmark iterations.
func BenchmarkRuntimeTaskCancellation(b *testing.B) {
	addr := os.Getenv("WEKNORA_CANCELLATION_BENCH_REDIS")
	if addr == "" {
		b.Skip("requires disposable local Redis via WEKNORA_CANCELLATION_BENCH_REDIS")
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		b.Fatal("benchmark requires loopback Redis")
	}
	for _, queue := range []string{types.QueueMultimodal, types.QueueQuestion} {
		for _, count := range []int{10000, 50000} {
			b.Run(fmt.Sprintf("%s/%d", queue, count), func(b *testing.B) {
				client := redis.NewClient(&redis.Options{Addr: addr, DB: 15})
				b.Cleanup(func() { _ = client.Close() })
				inspector := &asynqTaskInspector{redis: client, inspector: asynq.NewInspectorFromRedisClient(client)}
				enqueuer := asynq.NewClientFromRedisClient(client)
				ctx := context.Background()
				for iteration := 0; iteration < b.N; iteration++ {
					b.StopTimer()
					require.NoError(b, client.FlushDB(ctx).Err())
					db, err := gorm.Open(sqlite.Open(filepath.Join(b.TempDir(), "cancellation.db")), &gorm.Config{})
					require.NoError(b, err)
					sqlDB, err := db.DB()
					require.NoError(b, err)
					sqlDB.SetMaxOpenConns(1)
					require.NoError(b, db.Exec(`CREATE TABLE knowledges (
      id TEXT PRIMARY KEY,tenant_id INTEGER,knowledge_base_id TEXT,parse_status TEXT,
      summary_status TEXT,pending_subtasks_count INTEGER,error_message TEXT,
      updated_at DATETIME,deleted_at DATETIME)`).Error)
					require.NoError(b, db.AutoMigrate(&types.KnowledgeProcessingSpan{}, &types.TaskPendingOp{}))
					const documents = 1000
					before := time.Now().Add(-time.Minute)
					require.NoError(b, db.Transaction(func(tx *gorm.DB) error {
						for i := 0; i < documents; i++ {
							id := fmt.Sprintf("doc-%d", i)
							if err := tx.Exec(`INSERT INTO knowledges VALUES (?,?,?,?,?,?,?,?,?)`,
								id, 42, "kb", types.ParseStatusFinalizing, types.SummaryStatusPending,
								50, "", before, nil).Error; err != nil {
								return err
							}
							if err := tx.Create(&types.KnowledgeProcessingSpan{
								KnowledgeID: id, Attempt: 1, SpanID: "root", Name: "parse",
								Kind: types.SpanKindRoot, Status: types.SpanStatusRunning,
							}).Error; err != nil {
								return err
							}
						}
						return nil
					}))
					taskType := types.TypeImageMultimodal
					if queue == types.QueueQuestion {
						taskType = types.TypeQuestionGeneration
					}
					for i := 0; i < count; i++ {
						payload := []byte(fmt.Sprintf(`{"tenant_id":42,"knowledge_id":"doc-%d","attempt":1}`,
							i%documents))
						_, err := enqueuer.Enqueue(asynq.NewTask(taskType, payload), asynq.Queue(queue))
						require.NoError(b, err)
					}
					for i := 0; i < documents; i++ {
						payload := []byte(fmt.Sprintf(`{"tenant_id":42,"knowledge_id":"doc-%d","attempt":1}`, i))
						_, err := enqueuer.Enqueue(asynq.NewTask(types.TypeSummaryGeneration, payload),
							asynq.Queue(types.QueueSummary))
						require.NoError(b, err)
					}
					svc := service.NewRuntimeTaskCancellationService(inspector, client,
						repository.NewRuntimeTaskCancellationRepository(db), nil, nil)
					b.StartTimer()
					started := time.Now()
					job, err := svc.Start(ctx, queue)
					require.NoError(b, err)
					acknowledgement := time.Since(started)
					deadline := time.Now().Add(5 * time.Minute)
					for job.Status == "running" && time.Now().Before(deadline) {
						time.Sleep(20 * time.Millisecond)
						job, err = svc.Get(ctx, queue)
						require.NoError(b, err)
					}
					elapsed := time.Since(started)
					b.StopTimer()
					require.Equal(b, "completed", job.Status)
					require.Equal(b, count, job.Total)
					require.Equal(b, count, job.Processed)
					require.Equal(b, count, job.Cancelled)
					require.Zero(b, job.Failed)
					require.Zero(b, job.Skipped)
					require.Equal(b, documents, job.RelatedDeleted)
					var settled int64
					require.NoError(b, db.Model(&types.Knowledge{}).
						Where("parse_status = ? AND summary_status = ? AND pending_subtasks_count = 0",
							types.ParseStatusCancelled, types.SummaryStatusFailed).Count(&settled).Error)
					require.EqualValues(b, documents, settled)
					b.ReportMetric(float64(acknowledgement.Microseconds())/1000, "start_ms")
					b.ReportMetric(elapsed.Seconds(), "seconds/op")
					b.ReportMetric(float64(count)/elapsed.Seconds(), "tasks/s")
					require.NoError(b, sqlDB.Close())
				}
			})
		}
	}
}
