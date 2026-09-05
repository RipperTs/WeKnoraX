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
	"github.com/hibiken/asynq"
	"go.uber.org/dig"
	"gorm.io/gorm"
)

const runtimeTaskCancelledMessage = "系统管理员已终止任务，已写入的数据保留"

type runtimeTaskCanceller interface {
	CancelRuntimeTask(context.Context, string, []byte) error
}

type runtimeMemoryTaskCanceller interface {
	PrepareRuntimeTaskCancellation(context.Context, []byte) (interfaces.RuntimeTaskCancellationPlan, error)
}

// RuntimeTaskCancellationParams supplies the domains that own queued work.
type RuntimeTaskCancellationParams struct {
	dig.In
	Knowledge         interfaces.KnowledgeService
	DataSource        interfaces.DataSourceService
	TemporaryDocument interfaces.TemporaryDocumentService
	Wiki              interfaces.TaskHandler `name:"wikiIngest"`
	Memory            interfaces.MemoryService
	PendingOps        interfaces.TaskPendingOpsRepository
}

// RuntimeTaskCancellationService routes stopped tasks to their owning domain.
// The queue backend invokes this before deleting any live task record.
type RuntimeTaskCancellationService struct {
	knowledge         runtimeTaskCanceller
	dataSource        runtimeTaskCanceller
	temporaryDocument runtimeTaskCanceller
	wiki              runtimeTaskCanceller
	memory            runtimeMemoryTaskCanceller
	pendingOps        interfaces.TaskPendingOpsRepository
}

// NewRuntimeTaskCancellationService verifies each domain's cancellation capability.
func NewRuntimeTaskCancellationService(p RuntimeTaskCancellationParams) (*RuntimeTaskCancellationService, error) {
	memory, ok := p.Memory.(runtimeMemoryTaskCanceller)
	if !ok {
		return nil, errors.New("memory service does not implement runtime task cancellation")
	}
	s := &RuntimeTaskCancellationService{memory: memory, pendingOps: p.PendingOps}
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

type runtimeCancellationBatch struct {
	knowledges map[string]error
	wikiOps    map[string][]*types.TaskPendingOp
}

// CancelBatch fixes Wiki operation IDs before stopping handlers and shares
// document cancellation results across sibling tasks in the same request.
func (s *RuntimeTaskCancellationService) CancelBatch() interfaces.RuntimeTaskCancellationPreparer {
	batch := &runtimeCancellationBatch{
		knowledges: make(map[string]error), wikiOps: make(map[string][]*types.TaskPendingOp),
	}
	return func(ctx context.Context, taskType string, payload []byte) (interfaces.RuntimeTaskCancellationPlan, error) {
		if taskType == types.TypeMemoryExtract {
			return s.memory.PrepareRuntimeTaskCancellation(ctx, payload)
		}
		var plan interfaces.RuntimeTaskCancellationPlan
		ctx = context.WithValue(ctx, runtimeCancelledKnowledgeKey{}, batch)
		if taskType == types.TypeWikiIngest || taskType == types.TypeWikiFinalize {
			var p WikiIngestPayload
			if err := json.Unmarshal(payload, &p); err != nil {
				return plan, err
			}
			if _, err := runtimePendingSnapshot(ctx, s.pendingOps, p.TenantID, p.KnowledgeBaseID); err != nil {
				return plan, err
			}
			finalizer, ok := s.wiki.(interface {
				finishRuntimeTaskCancellation(context.Context, []byte) error
			})
			if !ok {
				return plan, errors.New("wiki trigger finalization is unavailable")
			}
			plan.AfterDelete = func(finishCtx context.Context) error {
				return finalizer.finishRuntimeTaskCancellation(finishCtx, payload)
			}
		}
		plan.Cancel = func(cancelCtx context.Context) error {
			return s.cancel(context.WithValue(cancelCtx, runtimeCancelledKnowledgeKey{}, batch), taskType, payload)
		}
		return plan, nil
	}
}

func runtimePendingSnapshot(
	ctx context.Context, repo interfaces.TaskPendingOpsRepository, tenantID uint64, kbID string,
) ([]*types.TaskPendingOp, error) {
	batch := ctx.Value(runtimeCancelledKnowledgeKey{}).(*runtimeCancellationBatch)
	key := fmt.Sprintf("%d:%s", tenantID, kbID)
	if rows, exists := batch.wikiOps[key]; exists {
		return rows, nil
	}
	snapshotter, ok := repo.(interfaces.TaskPendingOpsSnapshotter)
	if !ok {
		return nil, errors.New("pending operation snapshots are unavailable")
	}
	rows, err := snapshotter.SnapshotByScope(ctx, tenantID, types.TaskScopeKnowledgeBase, kbID)
	if err != nil {
		return nil, err
	}
	batch.wikiOps[key] = rows
	return rows, nil
}

func (s *RuntimeTaskCancellationService) cancel(ctx context.Context, taskType string, payload []byte) error {
	switch taskType {
	case types.TypeDataSourceSync:
		return s.dataSource.CancelRuntimeTask(ctx, taskType, payload)
	case types.TypeTemporaryDocumentProcess:
		return s.temporaryDocument.CancelRuntimeTask(ctx, taskType, payload)
	case types.TypeWikiIngest, types.TypeWikiFinalize:
		return s.wiki.CancelRuntimeTask(ctx, taskType, payload)
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

func (s *knowledgeService) cancelRuntimeKnowledge(ctx context.Context, id string) (resultErr error) {
	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)
	batch := ctx.Value(runtimeCancelledKnowledgeKey{}).(*runtimeCancellationBatch)
	key := fmt.Sprintf("%d:%s", tenantID, id)
	if err, exists := batch.knowledges[key]; exists {
		return err
	}
	defer func() { batch.knowledges[key] = resultErr }()
	knowledge, err := s.repo.GetKnowledgeByID(ctx, tenantID, id)
	if runtimeBusinessObjectGone(err) {
		return nil
	}
	if err != nil || knowledge == nil {
		return err
	}
	inspector, ok := s.taskInspector.(interfaces.RuntimeKnowledgeTaskCanceller)
	if !ok {
		return errors.New("confirmed document task cancellation is unavailable")
	}
	rows, err := runtimePendingSnapshot(ctx, s.taskPendingRepo, tenantID, knowledge.KnowledgeBaseID)
	if err != nil {
		return err
	}
	return inspector.CancelRuntimeKnowledgeTasks(ctx, tenantID, id, func(stoppedCtx context.Context) error {
		// Stopping a handler can finish/fail its parent. Re-read after all
		// exits, keeping terminal business states while still clearing siblings.
		current, err := s.repo.GetKnowledgeByID(stoppedCtx, tenantID, id)
		if runtimeBusinessObjectGone(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if current != nil {
			switch current.ParseStatus {
			case types.ParseStatusPending, types.ParseStatusProcessing,
				types.ParseStatusFinalizing, types.ParseStatusCancelled:
				if _, err := s.cancelKnowledgeParse(stoppedCtx, id, false); err != nil {
					return err
				}
			}
		}
		var ids []int64
		for _, row := range rows {
			if row.TaskType == types.TypeWikiIngest && row.Op == WikiOpIngest && row.DedupKey == id {
				ids = append(ids, row.ID)
			}
		}
		return s.taskPendingRepo.DeleteByIDs(stoppedCtx, ids)
	})
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
		if err := s.cancelRuntimeKnowledge(ctx, p.KnowledgeID); err != nil {
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
		return s.cancelRuntimeKnowledge(ctx, p.KnowledgeID)
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
			if err := s.cancelRuntimeKnowledge(ctx, id); err != nil {
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
	rows, err := runtimePendingSnapshot(ctx, s.pendingRepo, p.TenantID, p.KnowledgeBaseID)
	if err != nil {
		return err
	}
	canceller, ok := s.knowledgeSvc.(interface {
		cancelRuntimeKnowledge(context.Context, string) error
	})
	if !ok {
		return errors.New("document task cancellation is unavailable")
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		if row.TaskType != types.TypeWikiIngest && row.TaskType != types.TypeWikiFinalize {
			continue
		}
		if row.TaskType == types.TypeWikiIngest && row.Op == WikiOpIngest {
			var op WikiPendingOp
			if err := json.Unmarshal(row.Payload, &op); err != nil {
				return err
			}
			if err := canceller.cancelRuntimeKnowledge(ctx, op.KnowledgeID); err != nil {
				return err
			}
		}
		ids = append(ids, row.ID)
	}
	for len(ids) > 0 {
		size := min(100, len(ids))
		if err := s.pendingRepo.DeleteByIDs(ctx, ids[:size]); err != nil {
			return err
		}
		ids = ids[size:]
	}
	return nil
}

func (s *wikiIngestService) finishRuntimeTaskCancellation(ctx context.Context, data []byte) error {
	var p WikiIngestPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	// Check after deleting old coalesced triggers: a concurrent enqueue before
	// deletion is covered here; an enqueue afterwards creates its own trigger.
	for _, taskType := range []string{types.TypeWikiIngest, types.TypeWikiFinalize} {
		count, err := s.pendingRepo.PendingCount(ctx, taskType, types.TaskScopeKnowledgeBase, p.KnowledgeBaseID)
		if err != nil {
			return err
		}
		if count == 0 {
			continue
		}
		timeout := 60 * time.Minute
		if taskType == types.TypeWikiFinalize {
			timeout = 30 * time.Minute
		}
		task := asynq.NewTask(taskType, data, asynq.Queue(types.QueueWiki),
			asynq.MaxRetry(wikiIngestMaxRetry), asynq.Timeout(timeout), asynq.ProcessIn(wikiIngestDelay))
		if _, err := s.task.Enqueue(task); err != nil {
			return err
		}
	}
	return nil
}
