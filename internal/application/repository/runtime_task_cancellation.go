package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

const runtimeCancellationReason = "管理员已取消排队任务"

// RuntimeTaskCancellationRepository settles persisted business state while the
// queue task is reserved. The cutoff prevents replacing newer work.
type RuntimeTaskCancellationRepository struct{ db *gorm.DB }

// NewRuntimeTaskCancellationRepository creates the persisted-state cancellation adapter.
func NewRuntimeTaskCancellationRepository(db *gorm.DB) *RuntimeTaskCancellationRepository {
	return &RuntimeTaskCancellationRepository{db: db}
}

// CancelKnowledge settles one document attempt and its captured Wiki ingest rows atomically.
func (r *RuntimeTaskCancellationRepository) CancelKnowledge(
	ctx context.Context, tenantID uint64, id string, attempt int, cutoff time.Time,
) (*types.RuntimeCancelledKnowledge, *types.Knowledge, error) {
	var target *types.RuntimeCancelledKnowledge
	var knowledge *types.Knowledge
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		target, knowledge, err = cancelRuntimeKnowledge(tx, tenantID, id, attempt, cutoff)
		return err
	})
	return target, knowledge, err
}

func cancelRuntimeKnowledge(
	tx *gorm.DB, tenantID uint64, id string, attempt int, cutoff time.Time,
) (*types.RuntimeCancelledKnowledge, *types.Knowledge, error) {
	if attempt <= 0 {
		return nil, nil, nil
	}
	var knowledge types.Knowledge
	err := tx.Clauses(forUpdateClause()).Where("tenant_id = ? AND id = ?", tenantID, id).First(&knowledge).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return &types.RuntimeCancelledKnowledge{TenantID: tenantID, ID: id, Attempt: attempt}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if knowledge.UpdatedAt.After(cutoff) || knowledge.ParseStatus == types.ParseStatusDeleting {
		return nil, nil, nil
	}
	spans := NewKnowledgeSpanRepository(tx)
	latest, err := spans.LatestAttempt(tx.Statement.Context, id)
	if err != nil {
		return nil, nil, err
	}
	if latest != attempt {
		return nil, nil, nil
	}
	now := time.Now()
	updates := map[string]any{"updated_at": now}
	switch knowledge.ParseStatus {
	case types.ParseStatusPending, types.ParseStatusProcessing, types.ParseStatusFinalizing:
		updates["parse_status"] = types.ParseStatusCancelled
		updates["error_message"] = runtimeCancellationReason
		updates["pending_subtasks_count"] = 0
		knowledge.ParseStatus = types.ParseStatusCancelled
	}
	if knowledge.SummaryStatus == types.SummaryStatusPending ||
		knowledge.SummaryStatus == types.SummaryStatusProcessing {
		updates["summary_status"] = types.SummaryStatusFailed
	}
	if err := tx.Model(&knowledge).Updates(updates).Error; err != nil {
		return nil, nil, err
	}
	if _, err := spans.CancelAllOpenSpans(tx.Statement.Context, id, latest,
		"USER_CANCELLED", runtimeCancellationReason); err != nil {
		return nil, nil, err
	}
	// Only remove this document's captured, unclaimed ingest work.
	// Retractions must still converge the index.
	if err := tx.Where("tenant_id = ? AND task_type = ? AND scope_id = ? AND dedup_key = ?",
		tenantID, types.TypeWikiIngest, knowledge.KnowledgeBaseID, id).
		Where("op = ? AND claimed_at IS NULL AND enqueued_at <= ?", "ingest", cutoff).
		Delete(&types.TaskPendingOp{}).Error; err != nil {
		return nil, nil, err
	}
	return &types.RuntimeCancelledKnowledge{TenantID: tenantID, ID: id, Attempt: latest}, &knowledge, nil
}

// CancelSync ends the exact pending sync log without affecting later sync runs.
func (r *RuntimeTaskCancellationRepository) CancelSync(
	ctx context.Context, tenantID uint64, id string, cutoff time.Time,
) (bool, error) {
	res := r.db.WithContext(ctx).Model(&types.SyncLog{}).
		Where("tenant_id = ? AND id = ? AND status = ? AND started_at <= ?", tenantID, id, "running", cutoff).
		Updates(map[string]any{
			"status": "canceled", "finished_at": time.Now(), "error_message": runtimeCancellationReason,
		})
	return res.RowsAffected > 0, res.Error
}

// CancelTemporaryDocument ends a waiting chat attachment parse.
func (r *RuntimeTaskCancellationRepository) CancelTemporaryDocument(
	ctx context.Context, tenantID uint64, id string, cutoff time.Time,
) (bool, error) {
	res := r.db.WithContext(ctx).Model(&types.TemporaryDocument{}).
		Where("tenant_id = ? AND id = ? AND updated_at <= ?", tenantID, id, cutoff).
		Where("status IN ?", []string{types.TemporaryDocumentStatusUploaded, types.TemporaryDocumentStatusProcessing}).
		Updates(map[string]any{
			"status":        types.TemporaryDocumentStatusFailed,
			"error_message": runtimeCancellationReason, "updated_at": time.Now(),
		})
	return res.RowsAffected > 0, res.Error
}

// CancelMemoryExtraction discards the captured backlog and advances its extraction watermark.
func (r *RuntimeTaskCancellationRepository) CancelMemoryExtraction(
	ctx context.Context, tenantID uint64, subjectID string, cutoff time.Time,
) (bool, error) {
	res := r.db.WithContext(ctx).Model(&types.MemorySubject{}).
		Where("tenant_id = ? AND subject_id = ? AND updated_at <= ?", tenantID, subjectID, cutoff).
		Where("extract_scheduled_at IS NOT NULL AND (extract_cursor IS NULL OR extract_cursor <= ?)", cutoff).
		Updates(map[string]any{
			"pending_sessions":     types.MemoryPendingSessions{},
			"extract_scheduled_at": nil, "extract_cursor": cutoff, "updated_at": time.Now(),
		})
	return res.RowsAffected > 0, res.Error
}

// CancelWikiBatch uses an ID cursor, so deletion cannot shift later pages.
// Each document and its durable rows commit together; a claimed row survives.
func (r *RuntimeTaskCancellationRepository) CancelWikiBatch(
	ctx context.Context, tenantID uint64, kbID string, cutoff time.Time, after int64,
) ([]types.RuntimeCancelledKnowledge, int64, int, error) {
	var rows []types.TaskPendingOp
	err := r.db.WithContext(ctx).Where("tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ?",
		tenantID, types.TypeWikiIngest, types.TaskScopeKnowledgeBase, kbID).
		Where("op = ? AND claimed_at IS NULL AND enqueued_at <= ? AND id > ?", "ingest", cutoff, after).
		Order("id").Limit(100).Find(&rows).Error
	if err != nil {
		return nil, after, 0, err
	}
	var targets []types.RuntimeCancelledKnowledge
	failed := 0
	for _, row := range rows {
		after = row.ID
		var payload struct {
			KnowledgeID string `json:"knowledge_id"`
			Attempt     int    `json:"attempt"`
		}
		if json.Unmarshal(row.Payload, &payload) != nil || payload.KnowledgeID == "" {
			failed++
			continue
		}
		target, _, err := r.CancelKnowledge(ctx, tenantID, payload.KnowledgeID, payload.Attempt, cutoff)
		if err != nil {
			failed++
			continue
		}
		if target != nil {
			targets = append(targets, *target)
			// Missing knowledge has no parent row to supply a KB/dedup key.
			if err := r.db.WithContext(ctx).Where("id = ? AND claimed_at IS NULL", row.ID).
				Delete(&types.TaskPendingOp{}).Error; err != nil {
				failed++
			}
		}
	}
	return targets, after, failed, nil
}

// HasWikiWork includes claimed work, new ingests and required retractions.
func (r *RuntimeTaskCancellationRepository) HasWikiWork(
	ctx context.Context, tenantID uint64, kbID string,
) (bool, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&types.TaskPendingOp{}).
		Where("tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ?",
			tenantID, types.TypeWikiIngest, types.TaskScopeKnowledgeBase, kbID).Count(&count).Error
	return count > 0, err
}
