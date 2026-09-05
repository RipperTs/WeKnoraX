package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type runtimeCancellationKnowledgeRepo struct {
	interfaces.KnowledgeRepository
	rows    map[string]*types.Knowledge
	updates map[string]map[string]interface{}
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

type runtimeCancellationInspector struct {
	interfaces.TaskInspector
	stopped []string
}

func (i *runtimeCancellationInspector) CancelRuntimeKnowledgeTasks(
	ctx context.Context, _ uint64, id string, cancel interfaces.RuntimeTaskCancellation,
) error {
	i.stopped = append(i.stopped, id)
	return cancel(ctx)
}

type runtimeCancellationPendingRepo struct {
	interfaces.TaskPendingOpsRepository
}

func (*runtimeCancellationPendingRepo) SnapshotByScope(
	context.Context, uint64, string, string,
) ([]*types.TaskPendingOp, error) {
	return nil, nil
}

func (*runtimeCancellationPendingRepo) DeleteByIDs(context.Context, []int64) error {
	return nil
}

func TestRuntimePurgePreservesRequiredCleanup(t *testing.T) {
	svc := &RuntimeTaskCancellationService{}
	for _, taskType := range []string{types.TypeKBDelete, types.TypeIndexDelete} {
		plan, err := svc.CancelBatch()(context.Background(), taskType, nil)
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
	plan, err := svc.CancelBatch()(ctx, types.TypeFAQImport, payload)
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
		context.Background(), types.TypeFAQImport, []byte(`{"tenant_id":`),
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
			payload: `{"tenant_id":1,"knowledge_id":"completed","refresh":true}`, summary: true,
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
			}
			svc := &RuntimeTaskCancellationService{knowledge: knowledge}
			plan, err := svc.CancelBatch()(context.Background(), test.taskType, []byte(test.payload))
			require.NoError(t, err)
			require.NoError(t, plan.Cancel(context.Background()))
			require.Equal(t, types.ParseStatusCompleted, repo.rows["completed"].ParseStatus)
			require.Equal(t, types.ParseStatusFailed, repo.rows["failed"].ParseStatus)
			if test.summary {
				require.Equal(t, map[string]map[string]interface{}{
					"completed": {"summary_status": types.SummaryStatusFailed},
				}, repo.updates)
			} else {
				require.Empty(t, repo.updates)
				require.Equal(t, []string{"completed", "failed"}, inspector.stopped)
			}
		})
	}
}
