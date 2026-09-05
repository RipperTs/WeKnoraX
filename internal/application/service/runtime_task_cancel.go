package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/application/repository"
	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"go.uber.org/dig"
	"gorm.io/gorm"
)

const runtimeTaskCancelledMessage = "系统管理员已终止任务，已写入的数据保留"

type runtimeTaskCanceller interface {
	CancelRuntimeTask(context.Context, string, []byte) error
}

// RuntimeTaskCancellationParams supplies the domains that own queued work.
type RuntimeTaskCancellationParams struct {
	dig.In
	Knowledge         interfaces.KnowledgeService
	DataSource        interfaces.DataSourceService
	TemporaryDocument interfaces.TemporaryDocumentService
	Wiki              interfaces.TaskHandler `name:"wikiIngest"`
	Memory            interfaces.MemoryRepository
}

// RuntimeTaskCancellationService routes stopped tasks to their owning domain.
// The queue backend invokes this before deleting any live task record.
type RuntimeTaskCancellationService struct {
	knowledge         runtimeTaskCanceller
	dataSource        runtimeTaskCanceller
	temporaryDocument runtimeTaskCanceller
	wiki              runtimeTaskCanceller
	memory            interfaces.MemoryRepository
}

// NewRuntimeTaskCancellationService verifies each domain's cancellation capability.
func NewRuntimeTaskCancellationService(p RuntimeTaskCancellationParams) (*RuntimeTaskCancellationService, error) {
	s := &RuntimeTaskCancellationService{memory: p.Memory}
	for _, binding := range []struct {
		name    string
		service any
		target  *runtimeTaskCanceller
	}{
		{"knowledge", p.Knowledge, &s.knowledge},
		{"data source", p.DataSource, &s.dataSource},
		{"temporary document", p.TemporaryDocument, &s.temporaryDocument},
		{"wiki", p.Wiki, &s.wiki},
	} {
		canceller, ok := binding.service.(runtimeTaskCanceller)
		if !ok {
			return nil, fmt.Errorf("%s service does not implement runtime task cancellation", binding.name)
		}
		*binding.target = canceller
	}
	return s, nil
}

type runtimeCancelledKnowledgeKey struct{}

// CancelBatch avoids rescanning every queue once per subtask of one document.
func (s *RuntimeTaskCancellationService) CancelBatch() func(context.Context, string, []byte) error {
	knowledges := make(map[string]error)
	return func(ctx context.Context, taskType string, payload []byte) error {
		return s.cancel(context.WithValue(ctx, runtimeCancelledKnowledgeKey{}, knowledges), taskType, payload)
	}
}

func (s *RuntimeTaskCancellationService) cancel(ctx context.Context, taskType string, payload []byte) error {
	switch taskType {
	case types.TypeDataSourceSync:
		return s.dataSource.CancelRuntimeTask(ctx, taskType, payload)
	case types.TypeTemporaryDocumentProcess:
		return s.temporaryDocument.CancelRuntimeTask(ctx, taskType, payload)
	case types.TypeWikiIngest, types.TypeWikiFinalize:
		return s.wiki.CancelRuntimeTask(ctx, taskType, payload)
	case types.TypeMemoryExtract:
		var p types.MemoryExtractPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return err
		}
		scope := interfaces.MemoryScope{TenantID: p.TenantID, SubjectID: p.SubjectID}
		if _, _, err := s.memory.ClaimPendingSessions(ctx, scope); err != nil {
			return err
		}
		return s.memory.ReleaseExtractionSlot(ctx, scope)
	default:
		return s.knowledge.CancelRuntimeTask(ctx, taskType, payload)
	}
}

func runtimeBusinessObjectGone(err error) bool {
	if errors.Is(err, repository.ErrKnowledgeNotFound) || errors.Is(err, gorm.ErrRecordNotFound) {
		return true
	}
	var appErr *apperrors.AppError
	return errors.As(err, &appErr) && appErr.Code == apperrors.ErrNotFound
}

func cancelRuntimeKnowledge(ctx context.Context, svc interfaces.KnowledgeService, id string) (resultErr error) {
	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)
	if cancelled, ok := ctx.Value(runtimeCancelledKnowledgeKey{}).(map[string]error); ok {
		key := fmt.Sprintf("%d:%s", tenantID, id)
		if err, exists := cancelled[key]; exists {
			return err
		}
		defer func() { cancelled[key] = resultErr }()
	}
	knowledge, err := svc.GetRepository().GetKnowledgeByID(ctx, tenantID, id)
	if runtimeBusinessObjectGone(err) {
		return nil
	}
	if err != nil || knowledge == nil {
		return err
	}
	switch knowledge.ParseStatus {
	case types.ParseStatusPending, types.ParseStatusProcessing, types.ParseStatusFinalizing, types.ParseStatusCancelled:
		_, err = svc.CancelKnowledgeParse(ctx, id)
	}
	return err
}

func (s *knowledgeService) CancelRuntimeTask(ctx context.Context, taskType string, data []byte) error {
	var p struct {
		TenantID        uint64   `json:"tenant_id"`
		KnowledgeID     string   `json:"knowledge_id"`
		KnowledgeBaseID string   `json:"knowledge_base_id"`
		KnowledgeIDs    []string `json:"knowledge_ids"`
		TaskID          string   `json:"task_id"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	ctx = context.WithValue(ctx, types.TenantIDContextKey, p.TenantID)
	switch taskType {
	case types.TypeDocumentProcess, types.TypeManualProcess, types.TypeKnowledgePostProcess,
		types.TypeImageMultimodal, types.TypeChunkExtract, types.TypeQuestionGeneration,
		types.TypeSummaryGeneration, types.TypeDataTableSummary, types.TypeKnowledgeAutoTag:
		if err := cancelRuntimeKnowledge(ctx, s, p.KnowledgeID); err != nil {
			return err
		}
		if taskType == types.TypeSummaryGeneration {
			knowledge, err := s.repo.GetKnowledgeByID(ctx, p.TenantID, p.KnowledgeID)
			if runtimeBusinessObjectGone(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if knowledge != nil && knowledge.SummaryStatus != types.SummaryStatusCompleted {
				return s.repo.UpdateKnowledgeColumns(ctx, p.KnowledgeID,
					map[string]interface{}{"summary_status": types.SummaryStatusFailed})
			}
		}
		return nil
	case types.TypeFAQImport:
		var payload types.FAQImportPayload
		if err := json.Unmarshal(data, &payload); err != nil {
			return err
		}
		progress, err := s.GetFAQImportProgress(ctx, p.TaskID)
		if err != nil && !runtimeBusinessObjectGone(err) {
			return err
		}
		if progress != nil && progress.Status != types.FAQImportStatusCompleted {
			progress.Status = types.FAQImportStatusFailed
			progress.Message = runtimeTaskCancelledMessage
			progress.Error = runtimeTaskCancelledMessage
			if err := s.saveFAQImportProgress(ctx, progress); err != nil {
				return err
			}
		}
		if err := s.clearRunningFAQImportInfoIfMatches(
			ctx, payload.KBID, payload.TaskID, payload.InstanceID, payload.EnqueuedAt,
		); err != nil {
			return err
		}
		return cancelRuntimeKnowledge(ctx, s, p.KnowledgeID)
	case types.TypeKBClone:
		progress, err := s.GetKBCloneProgress(ctx, p.TaskID)
		if runtimeBusinessObjectGone(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if progress.Status == types.KBCloneStatusCompleted {
			return nil
		}
		progress.Status, progress.Error = types.KBCloneStatusFailed, runtimeTaskCancelledMessage
		progress.Message, progress.UpdatedAt = runtimeTaskCancelledMessage, time.Now().Unix()
		return s.saveKBCloneProgress(ctx, progress)
	case types.TypeKnowledgeMove:
		progress, err := s.GetKnowledgeMoveProgress(ctx, p.TaskID)
		if runtimeBusinessObjectGone(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if progress.Status == types.KBCloneStatusCompleted {
			return nil
		}
		progress.Status, progress.Error = types.KBCloneStatusFailed, runtimeTaskCancelledMessage
		progress.Message, progress.UpdatedAt = runtimeTaskCancelledMessage, time.Now().Unix()
		return s.saveKnowledgeMoveProgress(ctx, progress)
	case types.TypeKnowledgeListReparse:
		for _, id := range p.KnowledgeIDs {
			if err := cancelRuntimeKnowledge(ctx, s, id); err != nil {
				return err
			}
		}
		return nil
	case types.TypeKBDelete:
		knowledges, err := s.repo.ListKnowledgeByKnowledgeBaseID(ctx, p.TenantID, p.KnowledgeBaseID)
		if err != nil {
			return err
		}
		for _, knowledge := range knowledges {
			p.KnowledgeIDs = append(p.KnowledgeIDs, knowledge.ID)
		}
		fallthrough
	case types.TypeKnowledgeListDelete:
		for _, id := range p.KnowledgeIDs {
			knowledge, err := s.repo.GetKnowledgeByID(ctx, p.TenantID, id)
			if runtimeBusinessObjectGone(err) {
				continue
			}
			if err != nil {
				return err
			}
			if knowledge != nil && knowledge.ParseStatus == types.ParseStatusDeleting {
				if err := s.repo.UpdateKnowledgeColumns(ctx, id, map[string]interface{}{
					"parse_status": types.ParseStatusFailed, "error_message": runtimeTaskCancelledMessage,
				}); err != nil {
					return err
				}
			}
		}
		return nil
	case types.TypeIndexDelete:
		// Index deletion has no separate processing status. Partial deletion
		// is retained, just like partial writes in the other cancelled tasks.
		return nil
	default:
		return fmt.Errorf("unsupported runtime task type %q", taskType)
	}
}

// CancelRuntimeTask closes a stopped synchronization's progress record.
func (s *DataSourceService) CancelRuntimeTask(ctx context.Context, _ string, data []byte) error {
	var p types.DataSourceSyncPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	ctx = context.WithValue(ctx, types.TenantIDContextKey, p.TenantID)
	log, err := s.syncLogRepo.FindByID(ctx, p.SyncLogID)
	if runtimeBusinessObjectGone(err) {
		return nil
	}
	if err != nil || log == nil {
		return err
	}
	if log.Status == types.SyncLogStatusSuccess || log.Status == types.SyncLogStatusPartial {
		return nil
	}
	log.Status, log.ErrorMessage = types.SyncLogStatusCanceled, runtimeTaskCancelledMessage
	log.FinishedAt = timePtr(time.Now().UTC())
	return s.syncLogRepo.Update(ctx, log)
}

func (s *temporaryDocumentService) CancelRuntimeTask(ctx context.Context, _ string, data []byte) error {
	var p types.TemporaryDocumentTaskPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	document, err := s.repo.GetByID(ctx, p.TenantID, p.DocumentID)
	if runtimeBusinessObjectGone(err) {
		return nil
	}
	if err != nil || document == nil {
		return err
	}
	if document.Status == types.TemporaryDocumentStatusReady {
		return nil
	}
	return s.repo.MarkFailed(ctx, p.TenantID, p.DocumentID, runtimeTaskCancelledMessage)
}

func (s *wikiIngestService) CancelRuntimeTask(ctx context.Context, _ string, data []byte) error {
	var p WikiIngestPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	ctx = context.WithValue(ctx, types.TenantIDContextKey, p.TenantID)
	for _, taskType := range []string{types.TypeWikiIngest, types.TypeWikiFinalize} {
		for {
			rows, err := s.pendingRepo.PeekBatch(ctx, taskType, types.TaskScopeKnowledgeBase, p.KnowledgeBaseID, 100)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				break
			}
			ids := make([]int64, 0, len(rows))
			for _, row := range rows {
				if taskType == types.TypeWikiIngest && row.Op == WikiOpIngest {
					var op WikiPendingOp
					if err := json.Unmarshal(row.Payload, &op); err != nil {
						return err
					}
					if err := cancelRuntimeKnowledge(ctx, s.knowledgeSvc, op.KnowledgeID); err != nil {
						return err
					}
				}
				ids = append(ids, row.ID)
			}
			if err := s.pendingRepo.DeleteByIDs(ctx, ids); err != nil {
				return err
			}
		}
	}
	return nil
}
