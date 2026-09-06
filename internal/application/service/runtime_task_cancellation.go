package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	runtimeCancellationTimeout = 2 * time.Hour
	runtimeCancellationTTL     = 24 * time.Hour
	runtimeCancellationMessage = "管理员已取消排队任务"
)

// RuntimeTaskCancellationService runs bounded, best-effort cancellation jobs independently of HTTP requests.
type RuntimeTaskCancellationService struct {
	queue     interfaces.RuntimeTaskCancellationQueue
	redis     *redis.Client
	repo      *repository.RuntimeTaskCancellationRepository
	knowledge interfaces.KnowledgeService
	audit     interfaces.AuditLogService
}

// NewRuntimeTaskCancellationService wires the queue and business-state cancellation paths.
func NewRuntimeTaskCancellationService(
	inspector interfaces.TaskInspector, redisClient *redis.Client,
	repo *repository.RuntimeTaskCancellationRepository, knowledge interfaces.KnowledgeService,
	audit interfaces.AuditLogService,
) *RuntimeTaskCancellationService {
	queue, _ := inspector.(interfaces.RuntimeTaskCancellationQueue)
	return &RuntimeTaskCancellationService{
		queue: queue, redis: redisClient, repo: repo, knowledge: knowledge, audit: audit,
	}
}

func runtimeCancellationKey(queue string) string { return "runtime:cancellation:{" + queue + "}" }

var releaseRuntimeCancellationLock = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0
`)

// Start captures current pending IDs and launches one background job per queue.
func (s *RuntimeTaskCancellationService) Start(
	ctx context.Context, queue string,
) (*types.RuntimeTaskCancellation, error) {
	if s.queue == nil || s.redis == nil {
		return nil, errors.New("task queue is unavailable")
	}
	id := uuid.NewString()
	key := runtimeCancellationKey(queue)
	locked, err := s.redis.SetNX(ctx, key+":lock", id, runtimeCancellationTimeout).Result()
	if err != nil {
		return nil, err
	}
	if !locked {
		job, err := s.Get(ctx, queue)
		if err != nil {
			return nil, err
		}
		if job == nil || job.Status != "running" {
			return nil, errors.New("cancellation is starting")
		}
		return job, nil
	}
	started := time.Now()
	ids, err := s.queue.SnapshotPendingRuntimeTaskIDs(ctx, queue)
	if err != nil {
		_, releaseErr := releaseRuntimeCancellationLock.Run(ctx, s.redis, []string{key + ":lock"}, id).Result()
		return nil, errors.Join(err, releaseErr)
	}
	job := &types.RuntimeTaskCancellation{ID: id, Queue: queue, Status: "running", Total: len(ids), StartedAt: started}
	if err := s.save(ctx, job); err != nil {
		_, releaseErr := releaseRuntimeCancellationLock.Run(ctx, s.redis, []string{key + ":lock"}, id).Result()
		return nil, errors.Join(err, releaseErr)
	}
	result := *job
	go s.run(context.WithoutCancel(ctx), job, ids)
	return &result, nil
}

// Get returns the latest progress, marking expired or interrupted jobs as failed.
func (s *RuntimeTaskCancellationService) Get(
	ctx context.Context, queue string,
) (*types.RuntimeTaskCancellation, error) {
	if s.redis == nil || s.queue == nil {
		return nil, errors.New("task queue is unavailable")
	}
	key := runtimeCancellationKey(queue)
	values, err := s.redis.MGet(ctx, key, key+":lock").Result()
	if err != nil {
		return nil, err
	}
	if values[0] == nil {
		return nil, nil
	}
	var job types.RuntimeTaskCancellation
	if err := json.Unmarshal([]byte(values[0].(string)), &job); err != nil {
		return nil, err
	}
	if job.Status == "running" {
		if values[1] != job.ID || time.Since(job.StartedAt) >= runtimeCancellationTimeout {
			job.Status, job.Error = "failed", "批量取消已中断，未处理任务仍保留在队列中"
		}
	}
	return &job, nil
}

func (s *RuntimeTaskCancellationService) save(ctx context.Context, job *types.RuntimeTaskCancellation) error {
	job.UpdatedAt = time.Now()
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	key := runtimeCancellationKey(job.Queue)
	saved, err := saveRuntimeCancellation.Run(ctx, s.redis, []string{key, key + ":lock"},
		job.ID, data, int(runtimeCancellationTTL/time.Second)).Int()
	if err != nil {
		return err
	}
	if saved == 0 {
		return errors.New("cancellation job was superseded")
	}
	return nil
}

var saveRuntimeCancellation = redis.NewScript(`
local owner = redis.call('GET', KEYS[2])
if owner and owner ~= ARGV[1] then return 0 end
if not owner then
 local current = redis.call('GET', KEYS[1])
 if not current or cjson.decode(current).id ~= ARGV[1] then return 0 end
end
redis.call('SET', KEYS[1], ARGV[2], 'EX', ARGV[3])
return 1
`)

type runtimeCancellationBatch struct {
	targets map[string]types.RuntimeCancelledKnowledge
	results map[string]runtimeCancellationResult
}

type runtimeCancellationResult struct {
	cancelled bool
	err       error
}

func newRuntimeCancellationBatch() *runtimeCancellationBatch {
	return &runtimeCancellationBatch{
		targets: map[string]types.RuntimeCancelledKnowledge{},
		results: map[string]runtimeCancellationResult{},
	}
}

func (s *RuntimeTaskCancellationService) run(ctx context.Context, job *types.RuntimeTaskCancellation, ids []string) {
	ctx, cancel := context.WithDeadline(ctx, job.StartedAt.Add(runtimeCancellationTimeout))
	defer cancel()
	batch := newRuntimeCancellationBatch()
	var runErr error
	for start := 0; start < len(ids); start += 100 {
		if runErr = ctx.Err(); runErr != nil {
			break
		}
		end := min(start+100, len(ids))
		tasks := make([]*types.RuntimeCancellationTask, end-start)
		readErrors := make([]error, len(tasks))
		var wg sync.WaitGroup
		// Eight readers amortize Redis round trips without starting one goroutine per task.
		for worker := 0; worker < min(8, len(tasks)); worker++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				for i := worker; i < len(tasks); i += 8 {
					tasks[i], readErrors[i] = s.queue.GetPendingRuntimeCancellationTask(ctx, job.Queue, ids[start+i])
				}
			}(worker)
		}
		wg.Wait()
		for i, task := range tasks {
			if runErr = ctx.Err(); runErr != nil {
				break
			}
			cancelled, err := false, readErrors[i]
			if err == nil && task != nil {
				cancelled, err = s.cancelTask(ctx, task, job.StartedAt, batch)
			}
			job.Processed++
			switch {
			case err != nil:
				job.Failed++
				logger.Errorf(ctx, "runtime cancellation job=%s task=%s: %v", job.ID, ids[start+i], err)
			case cancelled:
				job.Cancelled++
			default:
				job.Skipped++
			}
			if time.Since(job.UpdatedAt) >= time.Second {
				if runErr = s.save(ctx, job); runErr != nil {
					break
				}
			}
		}
		if runErr != nil {
			break
		}
		if runErr = s.save(ctx, job); runErr != nil {
			break
		}
	}
	// Merge all documents into one scan, including documents cancelled through Wiki.
	if ctx.Err() == nil {
		targets := make([]types.RuntimeCancelledKnowledge, 0, len(batch.targets))
		for _, target := range batch.targets {
			targets = append(targets, target)
		}
		var err error
		job.RelatedDeleted, job.ActiveSignaled, err = s.queue.CancelRuntimeKnowledgeTasks(ctx, targets)
		runErr = errors.Join(runErr, err)
	}
	job.Status = "completed"
	if runErr != nil {
		job.Status, job.Error = "failed", "批量取消未全部完成，请检查失败计数和服务日志"
		logger.Errorf(ctx, "runtime cancellation job=%s: %v", job.ID, runErr)
	}
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer finishCancel()
	if err := s.save(finishCtx, job); err != nil {
		logger.Errorf(finishCtx, "save runtime cancellation job=%s: %v", job.ID, err)
	}
	if err := releaseRuntimeCancellationLock.Run(finishCtx, s.redis,
		[]string{runtimeCancellationKey(job.Queue) + ":lock"}, job.ID).Err(); err != nil {
		logger.Errorf(finishCtx, "release runtime cancellation job=%s: %v", job.ID, err)
	}
}

// CancelOne applies the same business cancellation contract to a single pending task.
func (s *RuntimeTaskCancellationService) CancelOne(ctx context.Context, queue, id string) (bool, error) {
	if s.queue == nil {
		return false, errors.New("task queue is unavailable")
	}
	task, err := s.queue.GetPendingRuntimeCancellationTask(ctx, queue, id)
	if err != nil || task == nil {
		return false, err
	}
	batch := newRuntimeCancellationBatch()
	cancelled, err := s.cancelTask(ctx, task, time.Now(), batch)
	var targets []types.RuntimeCancelledKnowledge
	for _, target := range batch.targets {
		targets = append(targets, target)
	}
	_, _, sweepErr := s.queue.CancelRuntimeKnowledgeTasks(ctx, targets)
	return cancelled, errors.Join(err, sweepErr)
}

type runtimeCancellationPayload struct {
	TenantID        uint64 `json:"tenant_id"`
	KnowledgeID     string `json:"knowledge_id"`
	KnowledgeBaseID string `json:"knowledge_base_id"`
	DocumentID      string `json:"document_id"`
	SubjectID       string `json:"subject_id"`
	SyncLogID       string `json:"sync_log_id"`
	Attempt         int    `json:"attempt"`
}

func (s *RuntimeTaskCancellationService) cancelTask(
	ctx context.Context, task *types.RuntimeCancellationTask, cutoff time.Time, batch *runtimeCancellationBatch,
) (bool, error) {
	if !types.CanCancelPendingRuntimeTask(task.Type) {
		return false, nil
	}
	pendingSince, err := strconv.ParseInt(task.PendingSince, 10, 64)
	if err != nil {
		return false, fmt.Errorf("invalid task enqueue time: %w", err)
	}
	if pendingSince > cutoff.UnixNano() {
		return false, nil
	}
	var p runtimeCancellationPayload
	if err := json.Unmarshal(task.Payload, &p); err != nil {
		return false, errors.Join(types.ErrRuntimeCancellationNotCommitted, err)
	}
	if p.TenantID == 0 {
		return false, fmt.Errorf("%w: task tenant is missing", types.ErrRuntimeCancellationNotCommitted)
	}
	// Leave known skips in place. Reserving and requeueing them would change
	// FIFO order, particularly for consecutive manual edits without attempts.
	switch task.Type {
	case types.TypeDocumentProcess, types.TypeManualProcess, types.TypeImageMultimodal,
		types.TypeKnowledgePostProcess, types.TypeQuestionGeneration, types.TypeSummaryGeneration,
		types.TypeChunkExtract, types.TypeDataTableSummary, types.TypeKnowledgeAutoTag:
		if p.KnowledgeID == "" {
			return false, fmt.Errorf("%w: task knowledge is missing", types.ErrRuntimeCancellationNotCommitted)
		}
		// Initial document uploads get their first attempt only after a worker starts.
		if p.Attempt < 0 || (p.Attempt == 0 && task.Type != types.TypeDocumentProcess) {
			return false, nil
		}
		key := fmt.Sprintf("%d:%s", p.TenantID, p.KnowledgeID)
		if target, ok := batch.targets[key]; ok && p.Attempt != target.Attempt {
			return false, nil
		}
	}
	task.Reservation = uuid.NewString()
	reserved, err := s.queue.ReservePendingRuntimeCancellationTask(ctx, task)
	if err != nil || !reserved {
		return false, err
	}
	cancelled, err := s.cancelBusinessTask(ctx, task, p, cutoff, batch)
	// Wiki tasks wake a durable KB queue. Even after partial commits, restore
	// their trigger on failure so the worker can read and process remaining ops.
	if err != nil && task.Type != types.TypeWikiIngest && !errors.Is(err, types.ErrRuntimeCancellationNotCommitted) {
		// Business changes may already have committed. Keep the task archived
		// on failure so a worker cannot overwrite the cancellation result.
		return false, err
	}
	if err != nil || !cancelled {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return false, errors.Join(err, s.queue.ReleaseRuntimeCancellationTask(releaseCtx, task))
	}
	deleted, err := s.queue.DeleteReservedRuntimeCancellationTask(ctx, task)
	if err == nil && !deleted {
		err = errors.New("reserved cancellation task changed before deletion")
	}
	return deleted, err
}

func (s *RuntimeTaskCancellationService) cancelBusinessTask(
	ctx context.Context, task *types.RuntimeCancellationTask, p runtimeCancellationPayload,
	cutoff time.Time, batch *runtimeCancellationBatch,
) (bool, error) {
	ctx = context.WithValue(ctx, types.TenantIDContextKey, p.TenantID)
	var cancelled bool
	var err error
	switch task.Type {
	case types.TypeDocumentProcess, types.TypeManualProcess, types.TypeImageMultimodal,
		types.TypeKnowledgePostProcess, types.TypeQuestionGeneration, types.TypeSummaryGeneration,
		types.TypeChunkExtract, types.TypeDataTableSummary, types.TypeKnowledgeAutoTag:
		key := fmt.Sprintf("%d:%s", p.TenantID, p.KnowledgeID)
		resultKey := fmt.Sprintf("%s:%d", key, p.Attempt)
		result, exists := batch.results[resultKey]
		if !exists {
			var target *types.RuntimeCancelledKnowledge
			var knowledge *types.Knowledge
			target, knowledge, err = s.repo.CancelKnowledge(ctx, p.TenantID, p.KnowledgeID, p.Attempt, cutoff)
			result = runtimeCancellationResult{cancelled: target != nil, err: err}
			if result.cancelled || result.err != nil {
				batch.results[resultKey] = result
			}
			if target != nil && err == nil {
				// An unstarted upload has no child tasks to sweep.
				if target.Attempt > 0 {
					batch.targets[fmt.Sprintf("%d:%s", p.TenantID, target.ID)] = *target
				}
				if knowledge != nil {
					recordKBActivity(ctx, s.audit, p.TenantID, knowledge.KnowledgeBaseID,
						types.AuditActionKnowledgeParseCanceled, "knowledge", knowledge.ID, types.AuditOutcomeCanceled,
						map[string]any{"title": knowledge.Title, "type": knowledge.Type})
				}
			}
		}
		cancelled, err = result.cancelled, result.err
	case types.TypeTemporaryDocumentProcess:
		if p.DocumentID == "" {
			return false, fmt.Errorf("%w: task document is missing", types.ErrRuntimeCancellationNotCommitted)
		}
		cancelled, err = s.repo.CancelTemporaryDocument(ctx, p.TenantID, p.DocumentID, cutoff)
	case types.TypeDataSourceSync:
		if p.SyncLogID == "" {
			return false, fmt.Errorf("%w: task sync log is missing", types.ErrRuntimeCancellationNotCommitted)
		}
		cancelled, err = s.repo.CancelSync(ctx, p.TenantID, p.SyncLogID, cutoff)
	case types.TypeMemoryExtract:
		if p.SubjectID == "" {
			return false, fmt.Errorf("%w: task subject is missing", types.ErrRuntimeCancellationNotCommitted)
		}
		cancelled, err = s.repo.CancelMemoryExtraction(ctx, p.TenantID, p.SubjectID, cutoff)
	case types.TypeWikiIngest:
		if p.KnowledgeBaseID == "" {
			return false, fmt.Errorf("%w: task knowledge base is missing", types.ErrRuntimeCancellationNotCommitted)
		}
		cancelled, err = s.cancelWiki(ctx, p.TenantID, p.KnowledgeBaseID, cutoff, batch)
	case types.TypeFAQImport, types.TypeKBClone, types.TypeKnowledgeMove:
		cancelled, err = s.cancelProgress(ctx, task)
	case types.TypeKnowledgeListDelete, types.TypeKnowledgeListReparse:
		// These wrappers change documents only after the worker starts.
		cancelled = true
	}
	return cancelled, err
}

func (s *RuntimeTaskCancellationService) cancelWiki(
	ctx context.Context, tenantID uint64, kbID string, cutoff time.Time, batch *runtimeCancellationBatch,
) (bool, error) {
	key := fmt.Sprintf("wiki:%d:%s", tenantID, kbID)
	if result, ok := batch.results[key]; ok {
		return result.cancelled, result.err
	}
	var after int64
	failed := 0
	uncommitted := true
	for {
		targets, next, failures, batchUncommitted, err := s.repo.CancelWikiBatch(ctx, tenantID, kbID, cutoff, after)
		uncommitted = uncommitted && batchUncommitted
		if err != nil {
			if uncommitted {
				err = errors.Join(types.ErrRuntimeCancellationNotCommitted, err)
			}
			batch.results[key] = runtimeCancellationResult{err: err}
			return false, err
		}
		failed += failures
		for _, target := range targets {
			batch.targets[fmt.Sprintf("%d:%s", tenantID, target.ID)] = target
		}
		if next == after {
			break
		}
		after = next
	}
	if failed > 0 {
		err := fmt.Errorf("%d wiki ingest cancellations failed", failed)
		if uncommitted {
			err = errors.Join(types.ErrRuntimeCancellationNotCommitted, err)
		}
		batch.results[key] = runtimeCancellationResult{err: err}
		return false, err
	}
	remaining, err := s.repo.HasWikiWork(ctx, tenantID, kbID)
	if err != nil && uncommitted {
		err = errors.Join(types.ErrRuntimeCancellationNotCommitted, err)
	}
	batch.results[key] = runtimeCancellationResult{cancelled: !remaining, err: err}
	return !remaining, err
}

func (s *RuntimeTaskCancellationService) cancelProgress(
	ctx context.Context, task *types.RuntimeCancellationTask,
) (bool, error) {
	if task.Type == types.TypeFAQImport {
		var p types.FAQImportPayload
		if err := json.Unmarshal(task.Payload, &p); err != nil {
			return false, errors.Join(types.ErrRuntimeCancellationNotCommitted, err)
		}
		canceller, ok := s.knowledge.(interface {
			CancelPendingFAQImport(context.Context, types.FAQImportPayload) (bool, error)
		})
		if !ok {
			return false, fmt.Errorf("%w: FAQ cancellation is unavailable", types.ErrRuntimeCancellationNotCommitted)
		}
		return canceller.CancelPendingFAQImport(ctx, p)
	}
	var p struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(task.Payload, &p); err != nil {
		return false, errors.Join(types.ErrRuntimeCancellationNotCommitted, err)
	}
	if p.TaskID == "" {
		return false, fmt.Errorf("%w: progress task ID is missing", types.ErrRuntimeCancellationNotCommitted)
	}
	key := getKBCloneProgressKey(p.TaskID)
	if task.Type == types.TypeKnowledgeMove {
		key = getKnowledgeMoveProgressKey(p.TaskID)
	}
	n, err := cancelRuntimeProgress.Run(ctx, s.redis, []string{key}, runtimeCancellationMessage).Int()
	return n == 1, err
}

// Preserve partial results and the key's TTL while closing the current progress.
var cancelRuntimeProgress = redis.NewScript(`
local raw = redis.call('GET', KEYS[1])
if not raw then return 0 end
local progress = cjson.decode(raw)
if progress.status ~= 'pending' and progress.status ~= 'processing' then return 0 end
progress.status = 'failed'
progress.message = ARGV[1]
progress.error = ARGV[1]
progress.updated_at = tonumber(redis.call('TIME')[1])
redis.call('SET', KEYS[1], cjson.encode(progress), 'KEEPTTL')
return 1
`)

// CancelPendingFAQImport closes progress, releases its matching marker and removes uploaded entries.
func (s *knowledgeService) CancelPendingFAQImport(ctx context.Context, p types.FAQImportPayload) (bool, error) {
	if p.TaskID == "" || p.KBID == "" {
		return false, fmt.Errorf("%w: FAQ task scope is missing", types.ErrRuntimeCancellationNotCommitted)
	}
	key := getFAQImportRunningKey(p.KBID)
	marker, err := s.redisClient.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, errors.Join(types.ErrRuntimeCancellationNotCommitted, err)
	}
	var info runningFAQImportInfo
	if err := json.Unmarshal([]byte(marker), &info); err != nil {
		return false, errors.Join(types.ErrRuntimeCancellationNotCommitted, err)
	}
	if !runningFAQImportInfoMatches(&info, p.TaskID, p.InstanceID, p.EnqueuedAt) {
		return false, nil
	}
	// Compare the captured marker atomically with the progress update.
	n, err := cancelFAQRuntimeProgress.Run(ctx, s.redisClient,
		[]string{key, getFAQImportProgressKey(p.TaskID)}, marker, runtimeCancellationMessage).Int()
	if err != nil || n == 0 {
		return false, err
	}
	if p.EntriesURL != "" {
		if err := s.fileSvc.DeleteFile(ctx, p.EntriesURL); err != nil {
			return false, err
		}
	}
	return true, nil
}

var cancelFAQRuntimeProgress = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
local raw = redis.call('GET', KEYS[2])
if not raw then return 0 end
local progress = cjson.decode(raw)
if progress.status ~= 'pending' and progress.status ~= 'processing' then return 0 end
progress.status = 'failed'
progress.message = ARGV[2]
progress.error = ARGV[2]
progress.updated_at = tonumber(redis.call('TIME')[1])
redis.call('SET', KEYS[2], cjson.encode(progress), 'KEEPTTL')
redis.call('DEL', KEYS[1])
return 1
`)
