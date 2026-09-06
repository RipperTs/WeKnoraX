package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// knowledgeTestDDL is the minimal subset of the knowledge schema this
// suite needs. We avoid AutoMigrate because Knowledge carries multiple
// JSONB-tagged fields whose SQLite mapping is fragile.
//
// Table name is `knowledges` (plural) — that's what migration 000000
// creates and what GORM's default pluralization expects when the
// service code uses Model(&types.Knowledge{}).
const knowledgeTestDDL = `
CREATE TABLE IF NOT EXISTS knowledges (
    id              VARCHAR(64) PRIMARY KEY,
    tenant_id       INTEGER NOT NULL DEFAULT 0,
    knowledge_base_id VARCHAR(64),
    parse_status    VARCHAR(32) NOT NULL DEFAULT 'pending',
    summary_status  VARCHAR(32) NOT NULL DEFAULT 'none',
    pending_subtasks_count INTEGER NOT NULL DEFAULT 0,
    error_message   TEXT,
    title           TEXT,
    file_type       TEXT,
    enable_status   TEXT NOT NULL DEFAULT 'enabled',
    type            TEXT NOT NULL DEFAULT 'document',
    embedding_model_id TEXT NOT NULL DEFAULT '',
    storage_size    BIGINT NOT NULL DEFAULT 0,
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    deleted_at      DATETIME
);
`

const housekeepingSpansDDL = `
CREATE TABLE IF NOT EXISTS knowledge_processing_spans (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    knowledge_id    VARCHAR(64) NOT NULL,
    attempt         INTEGER     NOT NULL DEFAULT 1,
    span_id         VARCHAR(64) NOT NULL,
    parent_span_id  VARCHAR(64),
    name            VARCHAR(255) NOT NULL,
    kind            VARCHAR(16) NOT NULL,
    status          VARCHAR(16) NOT NULL,
    input           TEXT,
    output          TEXT,
    metadata        TEXT,
    error_code      VARCHAR(64),
    error_message   TEXT,
    error_detail    TEXT,
    started_at      DATETIME,
    finished_at     DATETIME,
    duration_ms     BIGINT,
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (knowledge_id, attempt, span_id)
);
`

const housekeepingPendingOpsDDL = `
CREATE TABLE IF NOT EXISTS task_pending_ops (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    tenant_id   INTEGER NOT NULL DEFAULT 0,
    task_type   VARCHAR(64) NOT NULL,
    scope       VARCHAR(32) NOT NULL,
    scope_id    VARCHAR(64) NOT NULL,
    op          VARCHAR(32) NOT NULL,
    dedup_key   VARCHAR(128) NOT NULL DEFAULT '',
    payload     TEXT NOT NULL DEFAULT '{}',
    fail_count  INTEGER NOT NULL DEFAULT 0,
    enqueued_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    claimed_at  DATETIME
);
`

func setupHousekeepingDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.Exec(knowledgeTestDDL).Error)
	require.NoError(t, db.Exec(housekeepingSpansDDL).Error)
	require.NoError(t, db.Exec(housekeepingPendingOpsDDL).Error)
	return db
}

// insertWikiPendingOp mirrors newWikiIngestPendingOp: the durable row is
// scoped to the KB but deduplicated on the knowledge ID, which is exactly
// why the per-knowledge asynq probe cannot see it.
func insertWikiPendingOp(t *testing.T, db *gorm.DB, kbID, knowledgeID string) {
	t.Helper()
	require.NoError(t, db.Exec(
		`INSERT INTO task_pending_ops (task_type, scope, scope_id, op, dedup_key, payload)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		wikiTaskType, wikiTaskScope, kbID, WikiOpIngest, knowledgeID,
		`{"op":"ingest","knowledge_id":"`+knowledgeID+`"}`,
	).Error)
}

// insertKnowledge writes a knowledge row at the given updated_at. We
// can't pass updated_at through GORM defaults since CURRENT_TIMESTAMP
// would override our test fixture; raw SQL keeps the timestamp.
func insertKnowledge(t *testing.T, db *gorm.DB, id, status string, updatedAt time.Time) {
	t.Helper()
	require.NoError(t, db.Exec(
		`INSERT INTO knowledges (id, parse_status, updated_at) VALUES (?, ?, ?)`,
		id, status, updatedAt,
	).Error)
}

func insertSpan(t *testing.T, db *gorm.DB, kid string, attempt int, spanID, status string, updatedAt time.Time) {
	t.Helper()
	require.NoError(t, db.Exec(
		`INSERT INTO knowledge_processing_spans (knowledge_id, attempt, span_id, name, kind, status, updated_at)
		 VALUES (?, ?, ?, 'docreader', 'stage', ?, ?)`,
		kid, attempt, spanID, status, updatedAt,
	).Error)
}

// fakeTaskInspector is a controllable TaskInspector for the housekeeping
// suite. queued maps knowledge_id → "still has a queued task"; err forces
// the probe to fail so the fail-safe branch can be exercised.
type fakeTaskInspector struct {
	queued map[string]bool
	err    error
}

func (f fakeTaskInspector) CancelTasksForKnowledge(
	_ context.Context, _ string,
) (int, int, error) {
	return 0, 0, nil
}

func (f fakeTaskInspector) HasQueuedTasksForKnowledge(
	_ context.Context, knowledgeID string,
) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.queued[knowledgeID], nil
}

func (f fakeTaskInspector) QueueStats(
	_ context.Context,
) ([]types.QueueStat, bool, error) {
	return nil, false, nil
}

func (f fakeTaskInspector) WorkerServerStats(
	_ context.Context,
) ([]types.WorkerServerStat, bool, error) {
	return nil, false, nil
}

func newHousekeepingSvcForTest(db *gorm.DB) *HousekeepingService {
	return newHousekeepingSvcWithInspector(db, fakeTaskInspector{})
}

func newHousekeepingSvcWithInspector(db *gorm.DB, inspector interfaces.TaskInspector) *HousekeepingService {
	cfg := &config.Config{KnowledgeBase: &config.KnowledgeBaseConfig{
		// 1h floor + 10min buffer = 70min cutoff. Tight enough to keep
		// the test's relative timestamps in seconds; the production
		// default of 2h+10min is just a constant scale factor.
		DocumentProcessTimeout: 1 * time.Hour,
	}}
	return NewHousekeepingService(db, cfg, inspector)
}

// TestHousekeeping_RecoversAbandoned exercises the happy path: a
// knowledge stuck at "processing" with no recent heartbeat (no spans,
// stale knowledge.updated_at) MUST be flipped to failed.
func TestHousekeeping_RecoversAbandoned(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcForTest(db)
	stale := time.Now().Add(-3 * time.Hour) // well past 70min cutoff
	insertKnowledge(t, db, "kid-abandoned", types.ParseStatusProcessing, stale)

	svc.runSweep(context.Background())

	var status, errMsg string
	require.NoError(t, db.Raw(
		`SELECT parse_status, error_message FROM knowledges WHERE id = ?`, "kid-abandoned",
	).Row().Scan(&status, &errMsg))
	assert.Equal(t, types.ParseStatusFailed, status)
	assert.Contains(t, errMsg, "stuck in processing")
}

func TestHousekeeping_RecoversPendingTaskMissingFromQueue(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcForTest(db)
	stale := time.Now().Add(-3 * time.Hour)
	insertKnowledge(t, db, "kid-pending-orphan", types.ParseStatusPending, stale)

	svc.runSweep(context.Background())

	var status string
	require.NoError(t, db.Raw(
		`SELECT parse_status FROM knowledges WHERE id = ?`, "kid-pending-orphan",
	).Row().Scan(&status))
	assert.Equal(t, types.ParseStatusFailed, status,
		"a stale pending row with no queue task must not remain pending forever")
}

func TestHousekeeping_PreservesPendingTaskStillQueued(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcWithInspector(db, fakeTaskInspector{
		queued: map[string]bool{"kid-pending-queued": true},
	})
	stale := time.Now().Add(-3 * time.Hour)
	insertKnowledge(t, db, "kid-pending-queued", types.ParseStatusPending, stale)

	svc.runSweep(context.Background())

	var status string
	require.NoError(t, db.Raw(
		`SELECT parse_status FROM knowledges WHERE id = ?`, "kid-pending-queued",
	).Row().Scan(&status))
	assert.Equal(t, types.ParseStatusPending, status,
		"backlogged pending work remains owned by the durable queue")
}

// TestHousekeeping_NoFalseKill_ActiveSpan is the regression test for
// the "long DocReader silently runs longer than DocumentProcessTimeout"
// scenario the user flagged. A knowledge whose knowledge.updated_at
// looks stale BUT whose span tree shows recent activity must NOT be
// killed.
func TestHousekeeping_NoFalseKill_ActiveSpan(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcForTest(db)
	stale := time.Now().Add(-3 * time.Hour)
	insertKnowledge(t, db, "kid-active", types.ParseStatusProcessing, stale)
	// Span heartbeat well within the 70min cutoff — it represents
	// "we're STILL working, the worker just hasn't transitioned the
	// parse_status column yet".
	insertSpan(t, db, "kid-active", 1, "docreader-1", types.SpanStatusRunning, time.Now().Add(-2*time.Minute))

	svc.runSweep(context.Background())

	var status string
	require.NoError(t, db.Raw(
		`SELECT parse_status FROM knowledges WHERE id = ?`, "kid-active",
	).Row().Scan(&status))
	assert.Equal(t, types.ParseStatusProcessing, status,
		"knowledge with recent span heartbeat must NOT be flipped to failed")
}

// TestHousekeeping_NoFalseKill_StaleSpanRecovers confirms the inverse:
// a knowledge whose span tree has ALSO gone silent past the threshold
// is genuinely stuck and must be recovered.
func TestHousekeeping_NoFalseKill_StaleSpanRecovers(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcForTest(db)
	stale := time.Now().Add(-3 * time.Hour)
	insertKnowledge(t, db, "kid-stuck", types.ParseStatusProcessing, stale)
	// Span row stale by the same amount — no recent activity anywhere.
	insertSpan(t, db, "kid-stuck", 1, "docreader-1", types.SpanStatusRunning, stale)

	svc.runSweep(context.Background())

	var status string
	require.NoError(t, db.Raw(
		`SELECT parse_status FROM knowledges WHERE id = ?`, "kid-stuck",
	).Row().Scan(&status))
	assert.Equal(t, types.ParseStatusFailed, status,
		"genuinely stuck knowledge (knowledge AND spans both stale) must still be recovered")
}

// TestHousekeeping_NoFalseKill_TasksStillQueued is the regression test
// for the backpressure case: a finalizing row whose span heartbeat has
// gone stale (enrichment subtasks fanned out but no worker has picked
// them up yet) must NOT be killed while its tasks are still queued.
func TestHousekeeping_NoFalseKill_TasksStillQueued(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcWithInspector(db, fakeTaskInspector{
		queued: map[string]bool{"kid-backlogged": true},
	})
	stale := time.Now().Add(-3 * time.Hour)
	// finalizing + stale knowledge + stale span: span-only heuristics
	// would flag this as stuck, but the queue still holds its subtasks.
	insertKnowledge(t, db, "kid-backlogged", types.ParseStatusFinalizing, stale)
	insertSpan(t, db, "kid-backlogged", 1, "post-1", types.SpanStatusRunning, stale)

	svc.runSweep(context.Background())

	var status string
	require.NoError(t, db.Raw(
		`SELECT parse_status FROM knowledges WHERE id = ?`, "kid-backlogged",
	).Row().Scan(&status))
	assert.Equal(t, types.ParseStatusFinalizing, status,
		"finalizing row with tasks still queued must NOT be flipped to failed")
}

// A document whose only outstanding work is a queued Wiki ingest is
// invisible to the asynq probe: the durable op lives in task_pending_ops
// keyed by knowledge ID, while asynq holds only a per-KB trigger, and
// TypeWikiIngest is not in taskTypesForKnowledgeCancel either. The
// inspector below reports "nothing queued" — exactly what production does
// — so without the durable gate the sweep force-fails a healthy row.
func TestHousekeeping_NoFalseKill_DurableWikiIngestPending(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcWithInspector(db, fakeTaskInspector{})
	stale := time.Now().Add(-3 * time.Hour)
	insertKnowledge(t, db, "kid-durable-wiki", types.ParseStatusFinalizing, stale)
	insertSpan(t, db, "kid-durable-wiki", 1, "wiki-1", types.SpanStatusRunning, stale)
	insertWikiPendingOp(t, db, "kb-1", "kid-durable-wiki")

	svc.runSweep(context.Background())

	var status string
	require.NoError(t, db.Raw(
		`SELECT parse_status FROM knowledges WHERE id = ?`, "kid-durable-wiki",
	).Row().Scan(&status))
	assert.Equal(t, types.ParseStatusFinalizing, status,
		"row with a durable wiki ingest op pending must NOT be flipped to failed")
}

// The durable gate must not become a blanket amnesty: a stale row with no
// pending op and nothing in the queue is still genuinely orphaned, and the
// sweep must keep recovering it. This is the regression guard for the gate
// itself.
func TestHousekeeping_StillRecoversWhenNoDurableOp(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcWithInspector(db, fakeTaskInspector{})
	stale := time.Now().Add(-3 * time.Hour)
	insertKnowledge(t, db, "kid-orphan", types.ParseStatusFinalizing, stale)
	insertSpan(t, db, "kid-orphan", 1, "wiki-1", types.SpanStatusRunning, stale)
	// A pending op for a DIFFERENT document must not shield this one.
	insertWikiPendingOp(t, db, "kb-1", "kid-someone-else")

	svc.runSweep(context.Background())

	var status string
	require.NoError(t, db.Raw(
		`SELECT parse_status FROM knowledges WHERE id = ?`, "kid-orphan",
	).Row().Scan(&status))
	assert.Equal(t, types.ParseStatusFailed, status,
		"row with no durable op and nothing queued must still be recovered")
}

// TestHousekeeping_QueueProbeError_FailsSafe confirms the fail-safe
// direction: when the queue probe errors we still recover the row rather
// than leaving it stranded forever.
func TestHousekeeping_QueueProbeError_FailsSafe(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcWithInspector(db, fakeTaskInspector{
		err: errors.New("redis unavailable"),
	})
	stale := time.Now().Add(-3 * time.Hour)
	insertKnowledge(t, db, "kid-probeerr", types.ParseStatusProcessing, stale)

	svc.runSweep(context.Background())

	var status string
	require.NoError(t, db.Raw(
		`SELECT parse_status FROM knowledges WHERE id = ?`, "kid-probeerr",
	).Row().Scan(&status))
	assert.Equal(t, types.ParseStatusFailed, status,
		"queue probe error must fail safe and still recover the stuck row")
}

// TestHousekeeping_PreservesRecentlyTouched: any knowledge whose
// updated_at is within the cutoff is left alone — that's the cheap
// fast path that doesn't even consult the spans table.
func TestHousekeeping_PreservesRecentlyTouched(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcForTest(db)
	insertKnowledge(t, db, "kid-fresh", types.ParseStatusProcessing, time.Now().Add(-30*time.Second))

	svc.runSweep(context.Background())

	var status string
	require.NoError(t, db.Raw(
		`SELECT parse_status FROM knowledges WHERE id = ?`, "kid-fresh",
	).Row().Scan(&status))
	assert.Equal(t, types.ParseStatusProcessing, status,
		"knowledge updated within the cutoff must be left alone")
}

func TestRuntimeCancellationSettlesKnowledgeAndPreservesWikiCleanup(t *testing.T) {
	for _, status := range []string{
		types.ParseStatusPending, types.ParseStatusProcessing, types.ParseStatusFinalizing, types.ParseStatusCompleted,
	} {
		t.Run(status, func(t *testing.T) {
			db := setupHousekeepingDB(t)
			before := time.Now().Add(-time.Minute)
			cutoff := time.Now()
			require.NoError(t, db.Exec(`INSERT INTO knowledges
    (id,tenant_id,knowledge_base_id,parse_status,summary_status,pending_subtasks_count,updated_at)
    VALUES (?,?,?,?,?,?,?)`, "doc", 42, "kb", status, types.SummaryStatusProcessing, 3, before).Error)
			require.NoError(t, db.Exec(`INSERT INTO knowledge_processing_spans
    (knowledge_id,attempt,span_id,name,kind,status) VALUES (?,?,?,?,?,?)`,
				"doc", 2, "root", "parse", "root", types.SpanStatusRunning).Error)
			for _, op := range []types.TaskPendingOp{
				{
					TenantID: 42, TaskType: types.TypeWikiIngest, Scope: types.TaskScopeKnowledgeBase,
					ScopeID: "kb", Op: "ingest", DedupKey: "doc", EnqueuedAt: before,
				},
				{
					TenantID: 42, TaskType: types.TypeWikiIngest, Scope: types.TaskScopeKnowledgeBase,
					ScopeID: "kb", Op: "retract", DedupKey: "doc", EnqueuedAt: before,
				},
				{
					TenantID: 42, TaskType: types.TypeWikiIngest, Scope: types.TaskScopeKnowledgeBase,
					ScopeID: "kb", Op: "ingest", DedupKey: "doc", EnqueuedAt: before, ClaimedAt: &before,
				},
				{
					TenantID: 42, TaskType: types.TypeWikiIngest, Scope: types.TaskScopeKnowledgeBase,
					ScopeID: "kb", Op: "ingest", DedupKey: "doc", EnqueuedAt: cutoff.Add(time.Second),
				},
			} {
				op.Payload = []byte(`{"knowledge_id":"doc"}`)
				require.NoError(t, db.Create(&op).Error)
			}
			repo := repository.NewRuntimeTaskCancellationRepository(db)
			failWikiCleanup := func(tx *gorm.DB) {
				require.Error(t, tx.AddError(errors.New("wiki cleanup failed before commit")))
			}
			require.NoError(t, db.Callback().Delete().Before("gorm:delete").
				Register("fail_wiki_cleanup", failWikiCleanup))
			_, _, err := repo.CancelKnowledge(context.Background(), 42, "doc", 2, cutoff)
			require.ErrorIs(t, err, types.ErrRuntimeCancellationNotCommitted)
			var unchanged types.Knowledge
			require.NoError(t, db.First(&unchanged, "id = ?", "doc").Error)
			assert.Equal(t, status, unchanged.ParseStatus)
			assert.Equal(t, types.SummaryStatusProcessing, unchanged.SummaryStatus)
			assert.Equal(t, 3, unchanged.PendingSubtasksCount)
			var unchangedSpan types.KnowledgeProcessingSpan
			require.NoError(t, db.First(&unchangedSpan).Error)
			assert.Equal(t, types.SpanStatusRunning, unchangedSpan.Status)
			require.NoError(t, db.Callback().Delete().Remove("fail_wiki_cleanup"))
			target, _, err := repo.CancelKnowledge(context.Background(), 42, "doc", 2, cutoff)
			require.NoError(t, err)
			require.NotNil(t, target)
			assert.Equal(t, 2, target.Attempt)
			var knowledge types.Knowledge
			require.NoError(t, db.First(&knowledge, "id = ?", "doc").Error)
			expected := types.ParseStatusCancelled
			if status == types.ParseStatusCompleted {
				expected = types.ParseStatusCompleted
			}
			assert.Equal(t, expected, knowledge.ParseStatus)
			assert.Equal(t, types.SummaryStatusFailed, knowledge.SummaryStatus)
			if status != types.ParseStatusCompleted {
				assert.Zero(t, knowledge.PendingSubtasksCount)
			}
			var span types.KnowledgeProcessingSpan
			require.NoError(t, db.First(&span).Error)
			assert.Equal(t, types.SpanStatusCancelled, span.Status)
			assert.NotNil(t, span.FinishedAt)
			var ops []types.TaskPendingOp
			require.NoError(t, db.Find(&ops).Error)
			assert.Len(t, ops, 3)
		})
	}
}

func TestRuntimeCancellationSkipsNewerKnowledgeAttempt(t *testing.T) {
	db := setupHousekeepingDB(t)
	before := time.Now().Add(-time.Minute)
	require.NoError(t, db.Exec(`INSERT INTO knowledges (id,tenant_id,parse_status,updated_at) VALUES (?,?,?,?)`,
		"doc", 42, types.ParseStatusProcessing, before).Error)
	require.NoError(t, db.Exec(`INSERT INTO knowledge_processing_spans
  (knowledge_id,attempt,span_id,name,kind,status) VALUES (?,?,?,?,?,?)`,
		"doc", 2, "root", "parse", "root", types.SpanStatusRunning).Error)
	repo := repository.NewRuntimeTaskCancellationRepository(db)
	for _, attempt := range []int{0, 1, 3} {
		target, _, err := repo.CancelKnowledge(context.Background(), 42, "doc", attempt, time.Now())
		require.NoError(t, err)
		assert.Nil(t, target)
	}
	target, _, err := repo.CancelKnowledge(context.Background(), 42, "doc", 2, before.Add(-time.Second))
	require.NoError(t, err)
	assert.Nil(t, target)
	var knowledge types.Knowledge
	require.NoError(t, db.First(&knowledge, "id = ?", "doc").Error)
	assert.Equal(t, types.ParseStatusProcessing, knowledge.ParseStatus)
	var span types.KnowledgeProcessingSpan
	require.NoError(t, db.First(&span).Error)
	assert.Equal(t, types.SpanStatusRunning, span.Status)
	// Known skips must not reserve or requeue the task, even after another
	// attempt has populated the batch cache. This service has no queue adapter.
	svc := &RuntimeTaskCancellationService{repo: repo}
	batch := newRuntimeCancellationBatch()
	batch.targets["42:doc"] = types.RuntimeCancelledKnowledge{TenantID: 42, ID: "doc", Attempt: 2}
	batch.results["42:doc:2"] = runtimeCancellationResult{cancelled: true}
	for _, taskType := range []string{
		types.TypeDocumentProcess, types.TypeManualProcess, types.TypeDataTableSummary, types.TypeQuestionGeneration,
	} {
		for _, attempt := range []int{0, 1, 3} {
			payload, err := json.Marshal(runtimeCancellationPayload{TenantID: 42, KnowledgeID: "doc", Attempt: attempt})
			require.NoError(t, err)
			cancelled, err := svc.cancelTask(context.Background(), &types.RuntimeCancellationTask{
				Type: taskType, Payload: payload, PendingSince: "1",
			}, time.Now(), batch)
			require.NoError(t, err)
			assert.False(t, cancelled)
		}
	}
	for _, status := range []string{
		types.ParseStatusPending, types.ParseStatusProcessing, types.ParseStatusFinalizing, types.ParseStatusCompleted,
	} {
		id := "no-attempt-" + status
		require.NoError(t, db.Exec(`INSERT INTO knowledges (id,tenant_id,parse_status,updated_at) VALUES (?,?,?,?)`,
			id, 42, status, before).Error)
		target, _, err := repo.CancelKnowledge(context.Background(), 42, id, 0, time.Now())
		require.NoError(t, err)
		var knowledge types.Knowledge
		require.NoError(t, db.First(&knowledge, "id = ?", id).Error)
		if status == types.ParseStatusPending {
			require.NotNil(t, target)
			assert.Zero(t, target.Attempt)
			assert.Equal(t, types.ParseStatusCancelled, knowledge.ParseStatus)
		} else {
			assert.Nil(t, target)
			assert.Equal(t, status, knowledge.ParseStatus)
		}
	}
	// A pending reparse already has an attempt and must not be cancelled by an old zero-attempt task.
	require.NoError(t, db.Model(&types.Knowledge{}).Where("id = ?", "doc").
		UpdateColumn("parse_status", types.ParseStatusPending).Error)
	target, _, err = repo.CancelKnowledge(context.Background(), 42, "doc", 0, time.Now())
	require.NoError(t, err)
	assert.Nil(t, target)
}

func TestRuntimeCancellationSettlesSyncTemporaryDocumentAndMemory(t *testing.T) {
	db := setupHousekeepingDB(t)
	require.NoError(t, db.Exec(`CREATE TABLE sync_logs
  (id TEXT PRIMARY KEY,tenant_id INTEGER,status TEXT,started_at DATETIME,finished_at DATETIME,
 error_message TEXT,updated_at DATETIME)`).Error)
	require.NoError(t, db.Exec(`CREATE TABLE temporary_documents
  (id TEXT PRIMARY KEY,tenant_id INTEGER,status TEXT,error_message TEXT,
 updated_at DATETIME,deleted_at DATETIME)`).Error)
	require.NoError(t, db.Exec(`CREATE TABLE memory_subjects
  (id TEXT PRIMARY KEY,tenant_id INTEGER,subject_id TEXT,pending_sessions TEXT,
 extract_scheduled_at DATETIME,extract_cursor DATETIME,updated_at DATETIME)`).Error)
	before := time.Now().Add(-time.Minute)
	cutoff := time.Now()
	require.NoError(t, db.Exec(`INSERT INTO sync_logs(id,tenant_id,status,started_at) VALUES (?,?,?,?)`,
		"sync", 42, "running", before).Error)
	require.NoError(t, db.Exec(`INSERT INTO temporary_documents(id,tenant_id,status,updated_at) VALUES (?,?,?,?)`,
		"temp", 42, "uploaded", before).Error)
	require.NoError(t, db.Exec(`INSERT INTO memory_subjects
  (id,tenant_id,subject_id,pending_sessions,extract_scheduled_at,updated_at) VALUES (?,?,?,?,?,?)`,
		"memory", 42, "subject", `["session"]`, before, before).Error)
	repo := repository.NewRuntimeTaskCancellationRepository(db)
	cancelled, err := repo.CancelSync(context.Background(), 42, "sync", cutoff)
	require.NoError(t, err)
	require.True(t, cancelled)
	cancelled, err = repo.CancelTemporaryDocument(context.Background(), 42, "temp", cutoff)
	require.NoError(t, err)
	require.True(t, cancelled)
	cancelled, err = repo.CancelMemoryExtraction(context.Background(), 42, "subject", cutoff)
	require.NoError(t, err)
	require.True(t, cancelled)
	var syncLog types.SyncLog
	require.NoError(t, db.First(&syncLog, "id = ?", "sync").Error)
	assert.Equal(t, "canceled", syncLog.Status)
	assert.NotNil(t, syncLog.FinishedAt)
	var document types.TemporaryDocument
	require.NoError(t, db.First(&document, "id = ?", "temp").Error)
	assert.Equal(t, types.TemporaryDocumentStatusFailed, document.Status)
	var subject types.MemorySubject
	require.NoError(t, db.First(&subject, "id = ?", "memory").Error)
	assert.Empty(t, subject.PendingSessions)
	assert.Nil(t, subject.ExtractScheduledAt)
	require.NotNil(t, subject.ExtractCursor)
	assert.True(t, subject.ExtractCursor.Equal(cutoff))
}

func TestRuntimeCancellationPreservesPartialCloneAndMoveResults(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	svc := &RuntimeTaskCancellationService{redis: client}
	ctx := context.Background()
	for _, taskType := range []string{types.TypeKBClone, types.TypeKnowledgeMove} {
		key := getKBCloneProgressKey("task")
		if taskType == types.TypeKnowledgeMove {
			key = getKnowledgeMoveProgressKey("task")
		}
		require.NoError(t, client.Set(ctx, key, `{"status":"processing","processed":7,"total":10}`, time.Hour).Err())
		cancelled, err := svc.cancelProgress(ctx, &types.RuntimeCancellationTask{
			Type: taskType, Payload: []byte(`{"task_id":"task"}`),
		})
		require.NoError(t, err)
		require.True(t, cancelled)
		raw, err := client.Get(ctx, key).Bytes()
		require.NoError(t, err)
		var progress types.KBCloneProgress
		require.NoError(t, json.Unmarshal(raw, &progress))
		assert.Equal(t, types.KBCloneStatusFailed, progress.Status)
		assert.Equal(t, 7, progress.Processed)
		assert.Equal(t, 10, progress.Total)
		assert.Greater(t, client.TTL(ctx, key).Val(), time.Duration(0))
	}
}

func TestRuntimeCancellationFAQClosesOnlyMatchingImportInstance(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	svc := &knowledgeService{redisClient: client}
	ctx := context.Background()
	payload := types.FAQImportPayload{TenantID: 42, TaskID: "task", KBID: "kb", InstanceID: "instance-1", EnqueuedAt: 1}
	for _, instance := range []string{"instance-2", "instance-1"} {
		marker, err := json.Marshal(runningFAQImportInfo{TaskID: "task", InstanceID: instance, EnqueuedAt: 1})
		require.NoError(t, err)
		require.NoError(t, client.Set(ctx, getFAQImportRunningKey("kb"), marker, time.Hour).Err())
		require.NoError(t, client.Set(ctx, getFAQImportProgressKey("task"),
			`{"task_id":"task","kb_id":"kb","status":"pending","processed":3}`, time.Hour).Err())
		cancelled, err := svc.CancelPendingFAQImport(ctx, payload)
		require.NoError(t, err)
		assert.Equal(t, instance == "instance-1", cancelled)
		progress, err := svc.GetFAQImportProgress(ctx, "task")
		require.NoError(t, err)
		assert.Equal(t, 3, progress.Processed)
		if cancelled {
			assert.Equal(t, types.FAQImportStatusFailed, progress.Status)
			assert.ErrorIs(t, client.Get(ctx, getFAQImportRunningKey("kb")).Err(), redis.Nil)
		} else {
			assert.Equal(t, types.FAQImportStatusPending, progress.Status)
			assert.Equal(t, string(marker), client.Get(ctx, getFAQImportRunningKey("kb")).Val())
		}
	}
}

func TestRuntimeCancellationWikiCancelsIngestAndKeepsRetractionTrigger(t *testing.T) {
	db := setupHousekeepingDB(t)
	before := time.Now().Add(-time.Minute)
	cutoff := time.Now()
	require.NoError(t, db.Exec(`INSERT INTO knowledges
  (id,tenant_id,knowledge_base_id,parse_status,pending_subtasks_count,updated_at) VALUES (?,?,?,?,?,?)`,
		"doc", 42, "kb", types.ParseStatusFinalizing, 1, before).Error)
	insertSpan(t, db, "doc", 2, "root", types.SpanStatusRunning, before)
	require.NoError(t, db.Exec(`INSERT INTO knowledges
  (id,tenant_id,knowledge_base_id,parse_status,updated_at) VALUES (?,?,?,?,?)`,
		"unknown-attempt", 42, "kb", types.ParseStatusProcessing, before).Error)
	insertSpan(t, db, "unknown-attempt", 2, "root", types.SpanStatusRunning, before)
	unknown := types.TaskPendingOp{
		TenantID: 42, TaskType: types.TypeWikiIngest, Scope: types.TaskScopeKnowledgeBase,
		ScopeID: "kb", Op: WikiOpIngest, DedupKey: "unknown-attempt",
		Payload: []byte(`{"knowledge_id":"unknown-attempt"}`), EnqueuedAt: before,
	}
	require.NoError(t, db.Create(&unknown).Error)
	for _, op := range []string{WikiOpIngest, WikiOpRetract} {
		row := types.TaskPendingOp{
			TenantID: 42, TaskType: types.TypeWikiIngest, Scope: types.TaskScopeKnowledgeBase,
			ScopeID: "kb", Op: op, DedupKey: "doc", Payload: []byte(`{"knowledge_id":"doc","attempt":2}`),
			EnqueuedAt: before,
		}
		require.NoError(t, db.Create(&row).Error)
	}
	svc := &RuntimeTaskCancellationService{repo: repository.NewRuntimeTaskCancellationRepository(db)}
	batch := newRuntimeCancellationBatch()
	cancelled, err := svc.cancelWiki(context.Background(), 42, "kb", cutoff, batch)
	require.NoError(t, err)
	require.False(t, cancelled, "the trigger must remain for retraction")
	require.Len(t, batch.targets, 1)
	var knowledge types.Knowledge
	require.NoError(t, db.First(&knowledge, "id = ?", "doc").Error)
	assert.Equal(t, types.ParseStatusCancelled, knowledge.ParseStatus)
	assert.Zero(t, knowledge.PendingSubtasksCount)
	var rows []types.TaskPendingOp
	require.NoError(t, db.Order("id").Find(&rows).Error)
	require.Len(t, rows, 2)
	assert.Equal(t, unknown.ID, rows[0].ID)
	assert.Equal(t, WikiOpRetract, rows[1].Op)
	var untouched types.Knowledge
	require.NoError(t, db.First(&untouched, "id = ?", "unknown-attempt").Error)
	assert.Equal(t, types.ParseStatusProcessing, untouched.ParseStatus)
}

func TestRuntimeCancellationMemoizesFailedWikiScope(t *testing.T) {
	db := setupHousekeepingDB(t)
	row := types.TaskPendingOp{
		TenantID: 42, TaskType: types.TypeWikiIngest, Scope: types.TaskScopeKnowledgeBase,
		ScopeID: "kb", Op: WikiOpIngest, Payload: []byte(`{invalid`), EnqueuedAt: time.Now().Add(-time.Minute),
	}
	require.NoError(t, db.Create(&row).Error)
	svc := &RuntimeTaskCancellationService{repo: repository.NewRuntimeTaskCancellationRepository(db)}
	batch := newRuntimeCancellationBatch()
	cutoff := time.Now()
	_, firstErr := svc.cancelWiki(context.Background(), 42, "kb", cutoff, batch)
	require.Error(t, firstErr)
	require.ErrorIs(t, firstErr, types.ErrRuntimeCancellationNotCommitted)
	require.NoError(t, db.Delete(&row).Error)
	_, secondErr := svc.cancelWiki(context.Background(), 42, "kb", cutoff, batch)
	assert.Equal(t, firstErr, secondErr, "another trigger for the same scope must not repeat failed work")
}
