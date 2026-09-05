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
	PrepareRuntimeTaskCancellation(
		context.Context, []byte, json.RawMessage,
	) (interfaces.RuntimeTaskCancellationPlan, error)
}

type runtimeKnowledgeTaskCanceller interface {
	runtimeTaskCanceller
	snapshotRuntimeTaskCancellation(context.Context, string, []byte) (map[string][]*types.TaskPendingOp, error)
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
	knowledge         runtimeKnowledgeTaskCanceller
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
	knowledge, ok := p.Knowledge.(runtimeKnowledgeTaskCanceller)
	if !ok {
		return nil, errors.New("knowledge service does not implement runtime task cancellation snapshots")
	}
	s := &RuntimeTaskCancellationService{knowledge: knowledge, memory: memory, pendingOps: p.PendingOps}
	for _, binding := range []struct {
		name    string
		service any
		target  *runtimeTaskCanceller
	}{
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
	parses     map[string]runtimeParseSnapshot
}

type runtimeParseSnapshot struct {
	knowledge types.Knowledge
	attempt   int
}

// CancelBatch fixes Wiki operation IDs before stopping handlers and shares
// document cancellation results across sibling tasks in the same request.
func (s *RuntimeTaskCancellationService) CancelBatch() interfaces.RuntimeTaskCancellationPreparer {
	batch := &runtimeCancellationBatch{
		knowledges: make(map[string]error), wikiOps: make(map[string][]*types.TaskPendingOp),
		parses: make(map[string]runtimeParseSnapshot),
	}
	return func(ctx context.Context, taskType string, payload []byte,
		snapshot json.RawMessage,
	) (interfaces.RuntimeTaskCancellationPlan, error) {
		var plan interfaces.RuntimeTaskCancellationPlan
		recovering := snapshot != nil
		// Business deletion has already happened. These tasks must retain their
		// cleanup snapshots and finish, including when their handlers are active.
		if taskType == types.TypeKBDelete || taskType == types.TypeIndexDelete {
			return plan, types.ErrRuntimeTaskCleanupRequired
		}
		if taskType == types.TypeMemoryExtract {
			return s.memory.PrepareRuntimeTaskCancellation(ctx, payload, snapshot)
		}
		ctx = context.WithValue(ctx, runtimeCancelledKnowledgeKey{}, batch)
		taskBatch := &runtimeCancellationBatch{knowledges: batch.knowledges, parses: batch.parses}
		if snapshot != nil {
			if err := json.Unmarshal(snapshot, &taskBatch.wikiOps); err != nil {
				return plan, err
			}
		}
		if taskType == types.TypeWikiIngest || taskType == types.TypeWikiFinalize {
			var p WikiIngestPayload
			if err := json.Unmarshal(payload, &p); err != nil {
				return plan, err
			}
			if snapshot == nil {
				rows, err := loadRuntimePendingSnapshot(ctx, s.pendingOps, p.TenantID, p.KnowledgeBaseID)
				if err != nil {
					return plan, err
				}
				taskBatch.wikiOps = map[string][]*types.TaskPendingOp{
					fmt.Sprintf("%d:%s", p.TenantID, p.KnowledgeBaseID): rows,
				}
			}
			finalizer, ok := s.wiki.(interface {
				finishRuntimeTaskCancellation(context.Context, string, []byte) error
			})
			if !ok {
				return plan, errors.New("wiki trigger finalization is unavailable")
			}
			plan.FinalizeKey = fmt.Sprintf("wiki:%d:%s:%s", p.TenantID, p.KnowledgeBaseID, taskType)
			plan.Finalize = func(finishCtx context.Context) error {
				return finalizer.finishRuntimeTaskCancellation(finishCtx, taskType, payload)
			}
		} else if snapshot == nil {
			// Validate FAQ payloads before accessing any business dependencies.
			if taskType == types.TypeFAQImport {
				var faq types.FAQImportPayload
				if err := json.Unmarshal(payload, &faq); err != nil {
					return plan, err
				}
			}
			var err error
			taskBatch.wikiOps, err = s.knowledge.snapshotRuntimeTaskCancellation(ctx, taskType, payload)
			if err != nil {
				return plan, err
			}
		}
		if snapshot == nil {
			var err error
			snapshot, err = json.Marshal(taskBatch.wikiOps)
			if err != nil {
				return plan, err
			}
		}
		plan.Snapshot = snapshot
		if recovering && runtimeSnapshotOnlyRecovery(taskType) {
			// A document may have been reparsed since cleanup failed. Recovery
			// owns only the captured operations, never its current parse or tasks.
			plan.Cancel = func(cancelCtx context.Context) error {
				var ids []int64
				for _, rows := range taskBatch.wikiOps {
					for _, row := range rows {
						if row.TaskType == types.TypeWikiIngest || row.TaskType == types.TypeWikiFinalize {
							ids = append(ids, row.ID)
						}
					}
				}
				return deleteRuntimePendingOps(cancelCtx, s.pendingOps, ids)
			}
			return plan, nil
		}
		cancel := func(cancelCtx context.Context) error {
			return s.cancel(context.WithValue(cancelCtx, runtimeCancelledKnowledgeKey{}, taskBatch), taskType, payload)
		}
		if taskType == types.TypeFAQImport {
			// Keep the import slot while its trigger can still retry. The FAQ
			// container is shared by imports and has no parse attempt to cancel.
			plan.Finalize = cancel
		} else {
			plan.Cancel = cancel
		}
		return plan, nil
	}
}

func runtimeSnapshotOnlyRecovery(taskType string) bool {
	switch taskType {
	case types.TypeDocumentProcess, types.TypeManualProcess, types.TypeKnowledgePostProcess,
		types.TypeImageMultimodal, types.TypeChunkExtract, types.TypeQuestionGeneration,
		types.TypeSummaryGeneration, types.TypeDataTableSummary, types.TypeKnowledgeAutoTag,
		types.TypeKnowledgeListReparse, types.TypeWikiIngest, types.TypeWikiFinalize:
		return true
	default:
		return false
	}
}

func loadRuntimePendingSnapshot(
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

func runtimePendingSnapshot(ctx context.Context, tenantID uint64, kbID string) ([]*types.TaskPendingOp, error) {
	batch := ctx.Value(runtimeCancelledKnowledgeKey{}).(*runtimeCancellationBatch)
	rows, ok := batch.wikiOps[fmt.Sprintf("%d:%s", tenantID, kbID)]
	if !ok {
		return nil, errors.New("runtime cancellation scope is absent from the original snapshot")
	}
	return rows, nil
}

func deleteRuntimePendingOps(ctx context.Context, repo interfaces.TaskPendingOpsRepository, ids []int64) error {
	for len(ids) > 0 {
		size := min(100, len(ids))
		if err := repo.DeleteByIDs(ctx, ids[:size]); err != nil {
			return err
		}
		ids = ids[size:]
	}
	return nil
}

func (s *knowledgeService) snapshotRuntimeTaskCancellation(
	ctx context.Context, taskType string, data []byte,
) (map[string][]*types.TaskPendingOp, error) {
	snapshot := make(map[string][]*types.TaskPendingOp)
	switch taskType {
	case types.TypeDocumentProcess, types.TypeManualProcess, types.TypeKnowledgePostProcess,
		types.TypeImageMultimodal, types.TypeChunkExtract, types.TypeQuestionGeneration,
		types.TypeSummaryGeneration, types.TypeDataTableSummary, types.TypeKnowledgeAutoTag,
		types.TypeKnowledgeListReparse:
	default:
		return snapshot, nil
	}
	var p struct {
		TenantID     uint64   `json:"tenant_id"`
		KnowledgeID  string   `json:"knowledge_id"`
		KnowledgeIDs []string `json:"knowledge_ids"`
		Attempt      int      `json:"attempt"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	ids := p.KnowledgeIDs
	if taskType != types.TypeKnowledgeListReparse {
		ids = []string{p.KnowledgeID}
	}
	for _, id := range ids {
		knowledge, err := s.repo.GetKnowledgeByID(ctx, p.TenantID, id)
		if runtimeBusinessObjectGone(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if knowledge == nil {
			continue
		}
		batch := ctx.Value(runtimeCancelledKnowledgeKey{}).(*runtimeCancellationBatch)
		parseKey := fmt.Sprintf("%d:%s", p.TenantID, id)
		if _, exists := batch.parses[parseKey]; !exists {
			parse := runtimeParseSnapshot{knowledge: *knowledge}
			if p.Attempt > 0 && s.tracker().LatestAttempt(ctx, id) == p.Attempt {
				parse.attempt = p.Attempt
			}
			batch.parses[parseKey] = parse
		}
		if knowledge.ParseStatus == types.ParseStatusCompleted ||
			knowledge.ParseStatus == types.ParseStatusFailed {
			continue
		}
		rows, err := loadRuntimePendingSnapshot(ctx, s.taskPendingRepo, p.TenantID, knowledge.KnowledgeBaseID)
		if err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%d:%s", p.TenantID, knowledge.KnowledgeBaseID)
		if _, exists := snapshot[key]; !exists {
			snapshot[key] = nil
		}
		// A document owns only its ingest rows, not the entire KB's operation set.
		for _, row := range rows {
			if row.TaskType == types.TypeWikiIngest && row.Op == WikiOpIngest && row.DedupKey == id {
				snapshot[key] = append(snapshot[key], row)
			}
		}
	}
	return snapshot, nil
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
	// Finished parses can have independent enrichment work. Purging one
	// selected task must leave its other queued tasks and business state intact.
	if knowledge.ParseStatus == types.ParseStatusCompleted || knowledge.ParseStatus == types.ParseStatusFailed {
		return nil
	}
	inspector, ok := s.taskInspector.(interfaces.RuntimeKnowledgeTaskCanceller)
	if !ok {
		return errors.New("confirmed document task cancellation is unavailable")
	}
	rows, err := runtimePendingSnapshot(ctx, tenantID, knowledge.KnowledgeBaseID)
	if err != nil {
		return err
	}
	return inspector.CancelRuntimeKnowledgeTasks(ctx, tenantID, id, func(stoppedCtx context.Context) error {
		var ids []int64
		for _, row := range rows {
			if row.TaskType == types.TypeWikiIngest && row.Op == WikiOpIngest && row.DedupKey == id {
				ids = append(ids, row.ID)
			}
		}
		return s.taskPendingRepo.DeleteByIDs(stoppedCtx, ids)
	})
}

// Only an identified, unchanged parse can receive business-state cleanup.
// Missing attempt metadata leaves the interrupted state for manual handling.
func (s *knowledgeService) finishRuntimeKnowledgeCancellation(ctx context.Context, id string, summary bool) error {
	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)
	batch := ctx.Value(runtimeCancelledKnowledgeKey{}).(*runtimeCancellationBatch)
	parse := batch.parses[fmt.Sprintf("%d:%s", tenantID, id)]
	if parse.attempt == 0 || s.tracker().LatestAttempt(ctx, id) != parse.attempt {
		return nil
	}
	columns := make(map[string]interface{})
	switch parse.knowledge.ParseStatus {
	case types.ParseStatusPending, types.ParseStatusProcessing, types.ParseStatusFinalizing:
		columns["parse_status"] = types.ParseStatusCancelled
		columns["error_message"] = runtimeTaskCancelledMessage
		columns["pending_subtasks_count"] = 0
	}
	if summary && parse.knowledge.SummaryStatus != types.SummaryStatusCompleted {
		columns["summary_status"] = types.SummaryStatusFailed
	}
	if len(columns) == 0 {
		return nil
	}
	updated, err := s.repo.UpdateKnowledgeColumnsIfUnchanged(ctx, &parse.knowledge, columns)
	if err != nil {
		return err
	}
	if updated && columns["parse_status"] == types.ParseStatusCancelled {
		s.tracker().AbortAttempt(ctx, id, parse.attempt,
			"USER_CANCELLED", runtimeTaskCancelledMessage, runtimeTaskCancelledMessage)
	}
	return nil
}

func (s *knowledgeService) CancelRuntimeTask(ctx context.Context, taskType string, data []byte) error {
	var p struct {
		TenantID     uint64   `json:"tenant_id"`
		KnowledgeID  string   `json:"knowledge_id"`
		KnowledgeIDs []string `json:"knowledge_ids"`
		TaskID       string   `json:"task_id"`
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
		return s.finishRuntimeKnowledgeCancellation(ctx, p.KnowledgeID, taskType == types.TypeSummaryGeneration)
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
		return s.clearRunningFAQImportInfoIfMatches(
			ctx, payload.KBID, payload.TaskID, payload.InstanceID, payload.EnqueuedAt,
		)
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
				if _, err := s.repo.UpdateActiveDeletingKnowledgeColumns(ctx, id, map[string]interface{}{
					"parse_status": types.ParseStatusFailed, "error_message": runtimeTaskCancelledMessage,
				}); err != nil {
					return err
				}
			}
		}
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
	rows, err := runtimePendingSnapshot(ctx, p.TenantID, p.KnowledgeBaseID)
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
	return deleteRuntimePendingOps(ctx, s.pendingRepo, ids)
}

func (s *wikiIngestService) finishRuntimeTaskCancellation(ctx context.Context, taskType string, data []byte) error {
	var p WikiIngestPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	// Rearm this task type only after all selected triggers are quarantined
	// and their captured operations are cancelled. Other Wiki task types are
	// finalized independently.
	count, err := s.pendingRepo.PendingCount(ctx, taskType, types.TaskScopeKnowledgeBase, p.KnowledgeBaseID)
	if err != nil || count == 0 {
		return err
	}
	timeout := 60 * time.Minute
	if taskType == types.TypeWikiFinalize {
		timeout = 30 * time.Minute
	}
	task := asynq.NewTask(taskType, data, asynq.Queue(types.QueueWiki),
		asynq.MaxRetry(wikiIngestMaxRetry), asynq.Timeout(timeout), asynq.ProcessIn(wikiIngestDelay))
	_, err = s.task.Enqueue(task)
	return err
}
