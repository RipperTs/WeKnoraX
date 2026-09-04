package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/datasource/connector/confluence"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingDSRepo captures UpdateSyncState calls so checkpoint persistence can
// be asserted without a database.
type recordingDSRepo struct {
	kbDeleteDSRepo
	updated []*types.DataSource
}

type updateTrackingDSRepo struct {
	*kbDeleteDSRepo
	updateCalls int
}

func (r *updateTrackingDSRepo) Update(_ context.Context, _ *types.DataSource) error {
	r.updateCalls++
	return nil
}

func (r *recordingDSRepo) UpdateSyncState(_ context.Context, ds *types.DataSource) error {
	// Snapshot the fields a checkpoint is expected to persist.
	cp := *ds
	r.updated = append(r.updated, &cp)
	return nil
}

func makeConnectorCursor(t *testing.T, spaceNodeTimes map[string]map[string]string) types.JSON {
	t.Helper()
	inner := map[string]interface{}{"space_node_times": spaceNodeTimes}
	b, err := json.Marshal(&types.SyncCursor{ConnectorCursor: inner})
	require.NoError(t, err)
	return types.JSON(b)
}

// A fresh full sync (ForceFull, first attempt) must not pass the recorded cursor
// through the normal FetchStream path. A retry of that same task (attempt > 0)
// must instead resume from the checkpointed cursor.
func TestStreamStartCursor_ForceFullFirstAttemptDropsCursor(t *testing.T) {
	ds := &types.DataSource{
		LastSyncCursor: makeConnectorCursor(t, map[string]map[string]string{"space1": {"nt1": "100"}}),
	}

	fresh, err := streamStartCursor(ds, true /*forceFull*/, 0 /*attempt*/)
	require.NoError(t, err)
	assert.Nil(t, fresh, "fresh ForceFull must drop the cursor to re-fetch everything")

	retry, err := streamStartCursor(ds, true /*forceFull*/, 1 /*attempt*/)
	require.NoError(t, err)
	require.NotNil(t, retry, "a retried ForceFull must resume from the checkpoint")
	assert.NotNil(t, retry.ConnectorCursor["space_node_times"])
}

// Incremental sync always resumes from the recorded cursor regardless of attempt.
func TestStreamStartCursor_IncrementalKeepsCursor(t *testing.T) {
	ds := &types.DataSource{
		LastSyncCursor: makeConnectorCursor(t, map[string]map[string]string{"space1": {"nt1": "100"}}),
	}
	cur, err := streamStartCursor(ds, false /*forceFull*/, 0)
	require.NoError(t, err)
	require.NotNil(t, cur)
	assert.NotNil(t, cur.ConnectorCursor["space_node_times"])
}

func TestUpdateDataSourceValidatesAnonymousConnectorBeforeSaving(t *testing.T) {
	existingConfig, err := (&types.DataSourceConfig{
		Type:     types.ConnectorTypeConfluence,
		Settings: map[string]interface{}{"base_url": "https://confluence.example.com"},
	}).ToJSON()
	require.NoError(t, err)
	existing := &types.DataSource{
		ID:              "ds-confluence",
		TenantID:        1,
		KnowledgeBaseID: "kb-1",
		Type:            types.ConnectorTypeConfluence,
		Config:          existingConfig,
	}
	repo := &updateTrackingDSRepo{kbDeleteDSRepo: newKBDeleteDSRepo(existing.KnowledgeBaseID, existing)}
	registry := datasource.NewConnectorRegistry()
	require.NoError(t, registry.Register(confluence.NewConnector()))
	svc := &DataSourceService{dsRepo: repo, connectorRegistry: registry}

	invalidConfig, err := (&types.DataSourceConfig{
		Type:     types.ConnectorTypeConfluence,
		Settings: map[string]interface{}{"base_url": "not-an-absolute-url"},
	}).ToJSON()
	require.NoError(t, err)
	_, err = svc.UpdateDataSource(context.Background(), &types.DataSource{
		ID:              existing.ID,
		TenantID:        existing.TenantID,
		KnowledgeBaseID: existing.KnowledgeBaseID,
		Type:            existing.Type,
		Config:          invalidConfig,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, datasource.ErrInvalidConfig)
	assert.Zero(t, repo.updateCalls, "invalid anonymous settings must not be persisted")
}

func newStreamHandler(svc *DataSourceService, ds *types.DataSource, result *types.SyncResult, syncLog *types.SyncLog) *streamSyncHandler {
	return &streamSyncHandler{svc: svc, ds: ds, result: result, syncLog: syncLog}
}

// Emit routes items through the same classification as the batch loop: deleted
// items count into result.Deleted (the actual deletion, scoping and failure
// counters are covered by the ProcessSync tests) and connector-reported
// failures (an item carrying only a Metadata["error"]) land in result.Failed
// with a message — never silently lost.
func TestStreamHandler_EmitClassifiesDeletedAndFailed(t *testing.T) {
	ds := &types.DataSource{
		ID: "ds-1", TenantID: 1, KnowledgeBaseID: "kb-1",
		Type: types.ConnectorTypeFeishu, SyncDeletions: true,
	}
	result := &types.SyncResult{}
	knowledgeRepo := &deletionLookupKnowledgeRepo{knowledge: &types.Knowledge{ID: "knowledge-gone"}}
	knowledgeSvc := &sweepFakeKS{repo: knowledgeRepo}
	h := newStreamHandler(&DataSourceService{knowledgeService: knowledgeSvc}, ds, result, &types.SyncLog{})

	handled, err := h.Emit(context.Background(), types.FetchedItem{ExternalID: "gone", IsDeleted: true})
	require.NoError(t, err)
	assert.True(t, handled)
	handled, err = h.Emit(context.Background(), types.FetchedItem{
		ExternalID: "bad", Title: "Broken Doc",
		Metadata: map[string]string{"error": "export failed"},
	})
	require.NoError(t, err)
	assert.False(t, handled)

	assert.Equal(t, 1, result.Deleted)
	assert.Equal(t, 1, result.Failed)
	require.Len(t, result.Errors, 1)
	assert.Equal(t, "Broken Doc", result.Errors[0].Title)
	assert.Contains(t, result.Errors[0].Message, "export failed")
}

// A canceled context aborts the stream: Emit returns the context error so the
// connector stops fetching instead of burning API budget on a doomed run.
func TestStreamHandler_EmitAbortsOnCanceledContext(t *testing.T) {
	h := newStreamHandler(&DataSourceService{}, &types.DataSource{}, &types.SyncResult{}, &types.SyncLog{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handled, err := h.Emit(ctx, types.FetchedItem{ExternalID: "x", Content: []byte("data"), FileName: "x.md"})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, handled)
}

func TestStreamHandler_EmitAbortsOnDeletionFailure(t *testing.T) {
	repo := &deletionLookupKnowledgeRepo{lookupErr: errors.New("lookup failed")}
	h := newStreamHandler(
		&DataSourceService{knowledgeService: &sweepFakeKS{repo: repo}},
		&types.DataSource{
			ID: "ds-1", TenantID: 1, KnowledgeBaseID: "kb-1", SyncDeletions: true,
		},
		&types.SyncResult{},
		&types.SyncLog{},
	)

	handled, err := h.Emit(context.Background(), types.FetchedItem{ExternalID: "gone", IsDeleted: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "external_id=gone")
	assert.False(t, handled)
	assert.Equal(t, 1, h.result.DeletionFailed)
}

func TestStreamHandler_EmitKeepsFailedIngestPending(t *testing.T) {
	repo := &sweepFakeRepo{}
	h := newStreamHandler(
		&DataSourceService{knowledgeService: &sweepFakeKS{
			repo:      repo,
			createErr: errors.New("create failed"),
		}},
		&types.DataSource{
			ID: "ds-1", TenantID: 1, KnowledgeBaseID: "kb-1",
		},
		&types.SyncResult{},
		&types.SyncLog{},
	)

	handled, err := h.Emit(context.Background(), types.FetchedItem{
		ExternalID: "page:1",
		Title:      "Welcome",
		FileName:   "welcome.md",
		Content:    []byte("hello"),
	})
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Equal(t, 1, h.result.Failed)
}

func TestStreamHandler_EmitDefersDeletionWhenDisabled(t *testing.T) {
	h := newStreamHandler(
		&DataSourceService{},
		&types.DataSource{SyncDeletions: false},
		&types.SyncResult{},
		&types.SyncLog{},
	)

	handled, err := h.Emit(context.Background(), types.FetchedItem{
		ExternalID: "page:1",
		IsDeleted:  true,
	})
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Zero(t, h.result.Deleted)
}

// Checkpoint persists the connector cursor onto the data source so a crash
// after it keeps the progress made so far.
func TestStreamHandler_CheckpointPersistsCursor(t *testing.T) {
	dsRepo := &recordingDSRepo{}
	svc := &DataSourceService{dsRepo: dsRepo, syncLogRepo: &processSyncSyncLogRepo{logs: map[string]*types.SyncLog{}}}
	ds := &types.DataSource{ID: "ds-1"}
	result := &types.SyncResult{Created: 3}
	syncLog := &types.SyncLog{ID: "log-1"}
	h := newStreamHandler(svc, ds, result, syncLog)

	cursor := &types.SyncCursor{ConnectorCursor: map[string]interface{}{
		"space_node_times": map[string]map[string]string{"space1": {"nt1": "100"}},
	}}
	require.NoError(t, h.Checkpoint(context.Background(), cursor))

	require.Len(t, dsRepo.updated, 1)
	assert.NotEmpty(t, dsRepo.updated[0].LastSyncCursor, "checkpoint must persist the cursor JSON")
}
