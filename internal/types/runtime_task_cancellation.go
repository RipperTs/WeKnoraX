package types

import (
	"errors"
	"time"
)

// ErrRuntimeCancellationNotCommitted marks failures before any business change
// could commit. Only these errors permit returning a reserved task to workers.
var ErrRuntimeCancellationNotCommitted = errors.New("runtime cancellation did not commit business changes")

// RuntimeTaskCancellation records one best-effort operation. Counts refer to
// the original pending snapshot; related document tasks are counted separately.
type RuntimeTaskCancellation struct {
	ID             string    `json:"id"`
	Queue          string    `json:"queue"`
	Status         string    `json:"status"`
	Total          int       `json:"total"`
	Processed      int       `json:"processed"`
	Cancelled      int       `json:"cancelled"`
	Skipped        int       `json:"skipped"`
	Failed         int       `json:"failed"`
	RelatedDeleted int       `json:"related_deleted"`
	ActiveSignaled int       `json:"active_signaled"`
	Error          string    `json:"error,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// RuntimeCancellationTask is server-only. Payload and queue message must never
// be returned by the runtime HTTP API.
type RuntimeCancellationTask struct {
	ID           string
	Queue        string
	Type         string
	Payload      []byte
	Message      string
	State        string
	PendingSince string
	Reservation  string
}

// RuntimeCancelledKnowledge bounds related-task cancellation to one tenant and attempt.
type RuntimeCancelledKnowledge struct {
	TenantID uint64
	ID       string
	Attempt  int
}

// CanCancelPendingRuntimeTask describes business cancellation support, not raw
// deletion permission. Storage cleanup and Wiki convergence must finish.
func CanCancelPendingRuntimeTask(taskType string) bool {
	switch taskType {
	case TypeDocumentProcess, TypeManualProcess, TypeImageMultimodal,
		TypeKnowledgePostProcess, TypeQuestionGeneration, TypeSummaryGeneration,
		TypeChunkExtract, TypeTemporaryDocumentProcess, TypeDataTableSummary,
		TypeKnowledgeAutoTag, TypeDataSourceSync, TypeFAQImport, TypeKBClone,
		TypeKnowledgeMove, TypeKnowledgeListDelete, TypeKnowledgeListReparse,
		TypeMemoryExtract, TypeWikiIngest:
		return true
	default:
		return false
	}
}
