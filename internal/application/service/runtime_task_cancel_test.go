package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type runtimeCancellationKnowledgeRepo struct {
	interfaces.KnowledgeRepository
	rows         map[string]*types.Knowledge
	updates      map[string]map[string]interface{}
	beforeUpdate func()
}

func (r *runtimeCancellationKnowledgeRepo) GetKnowledgeByID(
	_ context.Context, _ uint64, id string,
) (*types.Knowledge, error) {
	return r.rows[id], nil
}

func (r *runtimeCancellationKnowledgeRepo) UpdateKnowledgeColumns(
	_ context.Context, id string, columns map[string]interface{},
) error {
	r.updates[id] = columns
	return nil
}

func (r *runtimeCancellationKnowledgeRepo) UpdateKnowledgeColumnsIfUnchanged(
	ctx context.Context, snapshot *types.Knowledge, columns map[string]interface{},
) (bool, error) {
	if r.beforeUpdate != nil {
		r.beforeUpdate()
	}
	current := r.rows[snapshot.ID]
	if current.TenantID != snapshot.TenantID || !current.UpdatedAt.Equal(snapshot.UpdatedAt) ||
		current.ParseStatus != snapshot.ParseStatus {
		return false, nil
	}
	return true, r.UpdateKnowledgeColumns(ctx, snapshot.ID, columns)
}

type runtimeCancellationTracker struct {
	noopSpanTracker
	attempt  int
	attempts map[string]int
	aborted  []int
}

func (t *runtimeCancellationTracker) LatestAttempt(_ context.Context, id string) int {
	if attempt, ok := t.attempts[id]; ok {
		return attempt
	}
	return t.attempt
}

func (t *runtimeCancellationTracker) AbortAttempt(_ context.Context, _ string, attempt int, _, _, _ string) {
	t.aborted = append(t.aborted, attempt)
}

type runtimeCancellationInspector struct {
	interfaces.TaskInspector
	stopped          []string
	snapshotAttempts map[string]map[int]bool
}

func (i *runtimeCancellationInspector) CancelRuntimeKnowledgeTasks(
	ctx context.Context, _ uint64, id string, cancel interfaces.RuntimeTaskCancellation,
) error {
	i.stopped = append(i.stopped, id)
	return cancel(ctx)
}

func (i *runtimeCancellationInspector) RuntimeKnowledgeAttemptSnapshotted(
	_ context.Context, _ uint64, id string, attempt int,
) bool {
	return i.snapshotAttempts[id][attempt]
}

type runtimeCancellationPendingRepo struct {
	interfaces.TaskPendingOpsRepository
	rows          []*types.TaskPendingOp
	snapshotCalls int
	deleteErr     error
}

func (r *runtimeCancellationPendingRepo) SnapshotByScope(
	context.Context, uint64, string, string,
) ([]*types.TaskPendingOp, error) {
	r.snapshotCalls++
	return append([]*types.TaskPendingOp(nil), r.rows...), nil
}

func (r *runtimeCancellationPendingRepo) DeleteByIDs(_ context.Context, ids []int64) error {
	if r.deleteErr != nil {
		return r.deleteErr
	}
	for _, id := range ids {
		for i, row := range r.rows {
			if row.ID == id {
				r.rows = append(r.rows[:i], r.rows[i+1:]...)
				break
			}
		}
	}
	return nil
}

func (r *runtimeCancellationPendingRepo) PendingCount(context.Context, string, string, string) (int64, error) {
	return int64(len(r.rows)), nil
}

func TestRuntimePurgePreservesRequiredCleanup(t *testing.T) {
	svc := &RuntimeTaskCancellationService{}
	for _, taskType := range []string{types.TypeKBDelete, types.TypeIndexDelete} {
		plan, err := svc.CancelBatch()(context.Background(), taskType, nil, nil)
		require.ErrorIs(t, err, types.ErrRuntimeTaskCleanupRequired)
		require.Nil(t, plan.Cancel)
		require.Nil(t, plan.Finalize)
	}
}

func TestRuntimeFAQCancellationReleasesImportDuringFinalize(t *testing.T) {
	ctx := context.Background()
	// No document dependencies: cancelling an import must not cancel its
	// shared, completed FAQ container or require document queue inspection.
	knowledge := &knowledgeService{}
	progress := &types.FAQImportProgress{
		TaskID: "import-1", KBID: "kb-1", KnowledgeID: "faq-container",
		Status: types.FAQImportStatusPending,
	}
	require.NoError(t, knowledge.saveFAQImportProgress(ctx, progress))
	require.NoError(t, knowledge.setRunningFAQImportInfo(ctx, "kb-1", &runningFAQImportInfo{
		TaskID: "import-1", InstanceID: "instance-1", EnqueuedAt: 1,
	}))
	payload, err := json.Marshal(types.FAQImportPayload{
		TenantID: 1, TaskID: "import-1", KBID: "kb-1", KnowledgeID: "faq-container",
		InstanceID: "instance-1", EnqueuedAt: 1,
	})
	require.NoError(t, err)
	svc := &RuntimeTaskCancellationService{knowledge: knowledge}
	plan, err := svc.CancelBatch()(ctx, types.TypeFAQImport, payload, nil)
	require.NoError(t, err)
	require.Nil(t, plan.Cancel)
	require.NotNil(t, plan.Finalize)
	require.Equal(t, types.FAQImportStatusPending, progress.Status)
	running, err := knowledge.getRunningFAQImportInfo(ctx, "kb-1")
	require.NoError(t, err)
	require.NotNil(t, running)

	require.NoError(t, plan.Finalize(ctx))
	require.Equal(t, types.FAQImportStatusFailed, progress.Status)
	running, err = knowledge.getRunningFAQImportInfo(ctx, "kb-1")
	require.NoError(t, err)
	require.Nil(t, running)
}

func TestRuntimeFAQCancellationRejectsMalformedPayloadBeforeFinalize(t *testing.T) {
	plan, err := new(RuntimeTaskCancellationService).CancelBatch()(
		context.Background(), types.TypeFAQImport, []byte(`{"tenant_id":`), nil,
	)
	require.Error(t, err)
	require.Nil(t, plan.Finalize)
}

func TestRuntimePurgePreservesCompletedParseStates(t *testing.T) {
	for _, test := range []struct {
		name     string
		taskType string
		payload  string
		summary  bool
	}{
		{
			name: "summary refresh", taskType: types.TypeSummaryGeneration,
			payload: `{"tenant_id":1,"knowledge_id":"completed","refresh":true,"attempt":1}`, summary: true,
		},
		{
			name: "questions after completion", taskType: types.TypeQuestionGeneration,
			payload: `{"tenant_id":1,"knowledge_id":"completed"}`,
		},
		{
			name: "questions after failure", taskType: types.TypeQuestionGeneration,
			payload: `{"tenant_id":1,"knowledge_id":"failed"}`,
		},
		{
			name: "batch reparse before submission", taskType: types.TypeKnowledgeListReparse,
			payload: `{"tenant_id":1,"knowledge_ids":["completed","failed"]}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &runtimeCancellationKnowledgeRepo{
				rows: map[string]*types.Knowledge{
					"completed": {
						ID: "completed", TenantID: 1, KnowledgeBaseID: "kb-1",
						ParseStatus: types.ParseStatusCompleted, SummaryStatus: types.SummaryStatusPending,
					},
					"failed": {
						ID: "failed", TenantID: 1, KnowledgeBaseID: "kb-1", ParseStatus: types.ParseStatusFailed,
					},
				},
				updates: make(map[string]map[string]interface{}),
			}
			inspector := &runtimeCancellationInspector{}
			knowledge := &knowledgeService{
				repo: repo, taskInspector: inspector, taskPendingRepo: &runtimeCancellationPendingRepo{},
				spanTracker: &runtimeCancellationTracker{attempt: 1},
			}
			svc := &RuntimeTaskCancellationService{knowledge: knowledge}
			plan, err := svc.CancelBatch()(context.Background(), test.taskType, []byte(test.payload), nil)
			require.NoError(t, err)
			require.NoError(t, plan.Cancel(context.Background()))
			require.Equal(t, types.ParseStatusCompleted, repo.rows["completed"].ParseStatus)
			require.Equal(t, types.ParseStatusFailed, repo.rows["failed"].ParseStatus)
			require.Empty(t, inspector.stopped, "finished parses must retain independent sibling tasks")
			if test.summary {
				require.Equal(t, map[string]map[string]interface{}{
					"completed": {"summary_status": types.SummaryStatusFailed},
				}, repo.updates)
			} else {
				require.Empty(t, repo.updates)
			}
		})
	}
}

func TestRuntimePurgeStillCleansCancelledParseSiblings(t *testing.T) {
	repo := &runtimeCancellationKnowledgeRepo{
		rows: map[string]*types.Knowledge{
			"cancelled": {
				ID: "cancelled", TenantID: 1, KnowledgeBaseID: "kb-1", ParseStatus: types.ParseStatusCancelled,
			},
		},
	}
	inspector := &runtimeCancellationInspector{}
	knowledge := &knowledgeService{
		repo: repo, taskInspector: inspector, taskPendingRepo: &runtimeCancellationPendingRepo{},
	}
	svc := &RuntimeTaskCancellationService{knowledge: knowledge}
	plan, err := svc.CancelBatch()(context.Background(), types.TypeQuestionGeneration,
		[]byte(`{"tenant_id":1,"knowledge_id":"cancelled"}`), nil)
	require.NoError(t, err)
	require.NoError(t, plan.Cancel(context.Background()))
	require.Equal(t, []string{"cancelled"}, inspector.stopped)
}

func TestRuntimeBatchReparseFinalizesOnlySnapshottedAttempt(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	repo := &runtimeCancellationKnowledgeRepo{
		rows: map[string]*types.Knowledge{
			"submitted": {
				ID: "submitted", TenantID: 1, KnowledgeBaseID: "kb-1",
				ParseStatus: types.ParseStatusProcessing, UpdatedAt: now,
			},
			"later": {
				ID: "later", TenantID: 1, KnowledgeBaseID: "kb-1",
				ParseStatus: types.ParseStatusPending, UpdatedAt: now,
			},
		},
		updates: make(map[string]map[string]interface{}),
	}
	inspector := &runtimeCancellationInspector{snapshotAttempts: map[string]map[int]bool{
		"submitted": {3: true},
		"later":     {3: true},
	}}
	tracker := &runtimeCancellationTracker{attempts: map[string]int{"submitted": 3, "later": 4}}
	knowledge := &knowledgeService{
		repo: repo, spanTracker: tracker, taskInspector: inspector,
		taskPendingRepo: &runtimeCancellationPendingRepo{},
	}
	svc := &RuntimeTaskCancellationService{knowledge: knowledge}
	payload := []byte(`{"tenant_id":1,"knowledge_ids":["submitted","later"]}`)
	plan, err := svc.CancelBatch()(ctx, types.TypeKnowledgeListReparse, payload, nil)
	require.NoError(t, err)
	require.NoError(t, plan.Cancel(ctx))
	require.Equal(t, []string{"submitted", "later"}, inspector.stopped)
	require.Equal(t, map[string]interface{}{
		"parse_status": types.ParseStatusCancelled, "error_message": runtimeTaskCancelledMessage,
		"pending_subtasks_count": 0,
	}, repo.updates["submitted"])
	require.NotContains(t, repo.updates, "later", "a newer unsnapshotted attempt must retain its state")
	require.Equal(t, []int{3}, tracker.aborted)
}

func TestRuntimeWikiRecoveryKeepsOriginalPendingOps(t *testing.T) {
	ctx := context.Background()
	original := &types.TaskPendingOp{
		ID: 1, TaskType: types.TypeWikiIngest, Op: WikiOpIngest, DedupKey: "doc-1",
		Payload: json.RawMessage(`{"knowledge_id":"doc-1"}`),
	}
	later := &types.TaskPendingOp{ID: 2, TaskType: types.TypeWikiIngest, Op: WikiOpIngest, DedupKey: "doc-1"}
	pending := &runtimeCancellationPendingRepo{rows: []*types.TaskPendingOp{original}}
	enqueuer := &metadataUpdateTaskEnqueuer{}
	document := &types.Knowledge{
		ID: "doc-1", TenantID: 1, KnowledgeBaseID: "kb-1", ParseStatus: types.ParseStatusCancelled,
	}
	inspector := &runtimeCancellationInspector{}
	knowledge := &knowledgeService{
		repo:          &runtimeCancellationKnowledgeRepo{rows: map[string]*types.Knowledge{"doc-1": document}},
		taskInspector: inspector, taskPendingRepo: pending,
	}
	wiki := &wikiIngestService{pendingRepo: pending, knowledgeSvc: knowledge, task: enqueuer}
	svc := &RuntimeTaskCancellationService{knowledge: knowledge, wiki: wiki, pendingOps: pending}
	payload := []byte(`{"tenant_id":1,"knowledge_base_id":"kb-1"}`)
	plan, err := svc.CancelBatch()(ctx, types.TypeWikiIngest, payload, nil)
	require.NoError(t, err)
	pending.deleteErr = errors.New("temporary storage failure")
	require.Error(t, plan.Cancel(ctx))
	pending.rows = append(pending.rows, later)
	pending.deleteErr = nil
	document.ParseStatus = types.ParseStatusProcessing

	recovered, err := svc.CancelBatch()(ctx, types.TypeWikiIngest, payload, plan.Snapshot)
	require.NoError(t, err)
	require.NoError(t, recovered.Cancel(ctx))
	require.Equal(t, []*types.TaskPendingOp{later}, pending.rows)
	require.Equal(t, 1, pending.snapshotCalls, "recovery must not reload the current operation set")
	require.Equal(t, types.ParseStatusProcessing, document.ParseStatus)
	require.Equal(t, []string{"doc-1"}, inspector.stopped, "recovery must not stop the new parse")
	require.JSONEq(t, string(plan.Snapshot), string(recovered.Snapshot))
	require.NoError(t, recovered.Finalize(ctx))
	require.Len(t, enqueuer.tasks, 1, "the later operation must retain an executable trigger")
}

func TestRuntimeRecoveryKeepsSeparateSnapshotsForSameScope(t *testing.T) {
	ctx := context.Background()
	first := &types.TaskPendingOp{ID: 1, TaskType: types.TypeWikiIngest, Op: WikiOpRetract}
	second := &types.TaskPendingOp{ID: 2, TaskType: types.TypeWikiIngest, Op: WikiOpRetract}
	pending := &runtimeCancellationPendingRepo{rows: []*types.TaskPendingOp{first}}
	knowledge := &knowledgeService{}
	wiki := &wikiIngestService{pendingRepo: pending, knowledgeSvc: knowledge}
	svc := &RuntimeTaskCancellationService{knowledge: knowledge, wiki: wiki, pendingOps: pending}
	payload := []byte(`{"tenant_id":1,"knowledge_base_id":"kb-1"}`)
	firstPlan, err := svc.CancelBatch()(ctx, types.TypeWikiIngest, payload, nil)
	require.NoError(t, err)
	pending.rows = append(pending.rows, second)
	secondPlan, err := svc.CancelBatch()(ctx, types.TypeWikiIngest, payload, nil)
	require.NoError(t, err)
	batch := svc.CancelBatch()
	recoveredFirst, err := batch(ctx, types.TypeWikiIngest, payload, firstPlan.Snapshot)
	require.NoError(t, err)
	recoveredSecond, err := batch(ctx, types.TypeWikiIngest, payload, secondPlan.Snapshot)
	require.NoError(t, err)
	require.NoError(t, recoveredFirst.Cancel(ctx))
	require.Equal(t, []*types.TaskPendingOp{second}, pending.rows)
	require.NoError(t, recoveredSecond.Cancel(ctx))
	require.Empty(t, pending.rows)
	require.Equal(t, 2, pending.snapshotCalls)
}

func TestRuntimeDocumentRecoveryPreservesLaterWikiOps(t *testing.T) {
	for _, taskType := range []string{
		types.TypeDocumentProcess, types.TypeQuestionGeneration,
		types.TypeSummaryGeneration, types.TypeKnowledgeListReparse,
	} {
		t.Run(taskType, func(t *testing.T) {
			ctx := context.Background()
			original := &types.TaskPendingOp{ID: 1, TaskType: types.TypeWikiIngest, Op: WikiOpIngest, DedupKey: "doc-1"}
			later := &types.TaskPendingOp{ID: 2, TaskType: types.TypeWikiIngest, Op: WikiOpIngest, DedupKey: "doc-1"}
			pending := &runtimeCancellationPendingRepo{rows: []*types.TaskPendingOp{original}}
			document := &types.Knowledge{
				ID: "doc-1", TenantID: 1, KnowledgeBaseID: "kb-1", ParseStatus: types.ParseStatusCancelled,
				SummaryStatus: types.SummaryStatusPending,
			}
			repo := &runtimeCancellationKnowledgeRepo{
				rows: map[string]*types.Knowledge{"doc-1": document}, updates: make(map[string]map[string]interface{}),
			}
			inspector := &runtimeCancellationInspector{}
			knowledge := &knowledgeService{repo: repo, taskInspector: inspector, taskPendingRepo: pending}
			svc := &RuntimeTaskCancellationService{knowledge: knowledge, pendingOps: pending}
			payload := []byte(`{"tenant_id":1,"knowledge_id":"doc-1","knowledge_ids":["doc-1"]}`)
			plan, err := svc.CancelBatch()(ctx, taskType, payload, nil)
			require.NoError(t, err)
			pending.deleteErr = errors.New("temporary storage failure")
			require.Error(t, plan.Cancel(ctx))
			pending.rows = append(pending.rows, later)
			pending.deleteErr = nil
			document.ParseStatus = types.ParseStatusProcessing
			recovered, err := svc.CancelBatch()(ctx, taskType, payload, plan.Snapshot)
			require.NoError(t, err)
			require.NoError(t, recovered.Cancel(ctx))
			require.Equal(t, []*types.TaskPendingOp{later}, pending.rows)
			require.Equal(t, 1, pending.snapshotCalls)
			require.Equal(t, types.ParseStatusProcessing, document.ParseStatus)
			require.Empty(t, repo.updates, "recovery must not update the new parse or summary")
			require.Equal(t, []string{"doc-1"}, inspector.stopped, "recovery must not stop the new parse")
		})
	}
}

func TestRuntimeDocumentCancellationKeepsLaterParseState(t *testing.T) {
	for _, change := range []string{"unchanged", "before snapshot", "after snapshot", "during update", "no attempt"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			document := &types.Knowledge{
				ID: "doc-1", TenantID: 1, KnowledgeBaseID: "kb-1",
				ParseStatus: types.ParseStatusProcessing, SummaryStatus: types.SummaryStatusPending,
				UpdatedAt: time.Now(),
			}
			repo := &runtimeCancellationKnowledgeRepo{
				rows: map[string]*types.Knowledge{"doc-1": document}, updates: make(map[string]map[string]interface{}),
			}
			tracker := &runtimeCancellationTracker{attempt: 1}
			knowledge := &knowledgeService{
				repo: repo, spanTracker: tracker, taskInspector: &runtimeCancellationInspector{},
				taskPendingRepo: &runtimeCancellationPendingRepo{},
			}
			startNewParse := func() {
				tracker.attempt = 2
				document.UpdatedAt = document.UpdatedAt.Add(time.Second)
			}
			if change == "before snapshot" {
				startNewParse()
			}
			payload := []byte(`{"tenant_id":1,"knowledge_id":"doc-1","attempt":1}`)
			if change == "no attempt" {
				payload = []byte(`{"tenant_id":1,"knowledge_id":"doc-1"}`)
			}
			svc := &RuntimeTaskCancellationService{knowledge: knowledge}
			plan, err := svc.CancelBatch()(ctx, types.TypeSummaryGeneration, payload, nil)
			require.NoError(t, err)
			switch change {
			case "after snapshot":
				startNewParse()
			case "during update":
				repo.beforeUpdate = startNewParse
			}
			require.NoError(t, plan.Cancel(ctx))
			if change == "unchanged" {
				require.Equal(t, types.ParseStatusCancelled, repo.updates["doc-1"]["parse_status"])
				require.Equal(t, types.SummaryStatusFailed, repo.updates["doc-1"]["summary_status"])
				require.Equal(t, []int{1}, tracker.aborted)
			} else {
				require.Empty(t, repo.updates, "a later or unidentified parse must retain its state")
				require.Empty(t, tracker.aborted, "cleanup must not close another attempt's spans")
			}
		})
	}
}

type runtimeCancellationSyncLogRepo struct {
	processSyncSyncLogRepo
	readErr error
}

func (r *runtimeCancellationSyncLogRepo) FindByID(ctx context.Context, id string) (*types.SyncLog, error) {
	if r.readErr != nil {
		return nil, r.readErr
	}
	return r.processSyncSyncLogRepo.FindByID(ctx, id)
}

func TestRuntimeSyncRecoveryFinishesOriginalSync(t *testing.T) {
	ctx := context.Background()
	original := &types.SyncLog{ID: "original", Status: types.SyncLogStatusRunning}
	later := &types.SyncLog{ID: "later", Status: types.SyncLogStatusRunning}
	repo := &runtimeCancellationSyncLogRepo{
		processSyncSyncLogRepo: processSyncSyncLogRepo{logs: map[string]*types.SyncLog{
			original.ID: original, later.ID: later,
		}},
		readErr: errors.New("storage unavailable"),
	}
	svc := &RuntimeTaskCancellationService{
		knowledge: &knowledgeService{}, dataSource: &DataSourceService{syncLogRepo: repo},
	}
	payload := []byte(`{"tenant_id":1,"data_source_id":"source-1","sync_log_id":"original"}`)
	plan, err := svc.CancelBatch()(ctx, types.TypeDataSourceSync, payload, nil)
	require.NoError(t, err)
	for range 3 {
		require.Error(t, plan.Cancel(ctx))
	}
	require.Equal(t, types.SyncLogStatusRunning, original.Status)
	repo.readErr = nil
	recovered, err := svc.CancelBatch()(ctx, types.TypeDataSourceSync, payload, plan.Snapshot)
	require.NoError(t, err)
	require.NoError(t, recovered.Cancel(ctx))
	require.Equal(t, types.SyncLogStatusCanceled, original.Status)
	require.NotNil(t, original.FinishedAt)
	require.Equal(t, types.SyncLogStatusRunning, later.Status)
}

func TestRuntimeTransferRecoveryFinishesOriginalProgress(t *testing.T) {
	for _, taskType := range []string{types.TypeKBClone, types.TypeKnowledgeMove} {
		t.Run(taskType, func(t *testing.T) {
			ctx := context.Background()
			server := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
			t.Cleanup(func() { _ = client.Close() })
			key := getKBCloneProgressKey
			if taskType == types.TypeKnowledgeMove {
				key = getKnowledgeMoveProgressKey
			}
			for _, id := range []string{"original", "later"} {
				data, err := json.Marshal(types.KBCloneProgress{TaskID: id, Status: types.KBCloneStatusProcessing})
				require.NoError(t, err)
				require.NoError(t, client.Set(ctx, key(id), data, time.Hour).Err())
			}
			svc := &RuntimeTaskCancellationService{knowledge: &knowledgeService{redisClient: client}}
			payload := []byte(`{"tenant_id":1,"task_id":"original"}`)
			plan, err := svc.CancelBatch()(ctx, taskType, payload, nil)
			require.NoError(t, err)
			server.SetError("storage unavailable")
			for range 3 {
				require.Error(t, plan.Cancel(ctx))
			}
			server.SetError("")
			recovered, err := svc.CancelBatch()(ctx, taskType, payload, plan.Snapshot)
			require.NoError(t, err)
			require.NoError(t, recovered.Cancel(ctx))
			for id, status := range map[string]types.KBCloneTaskStatus{
				"original": types.KBCloneStatusFailed, "later": types.KBCloneStatusProcessing,
			} {
				data, err := client.Get(ctx, key(id)).Bytes()
				require.NoError(t, err)
				var progress types.KBCloneProgress
				require.NoError(t, json.Unmarshal(data, &progress))
				require.Equal(t, status, progress.Status)
			}
		})
	}
}
