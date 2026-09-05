package memory

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// PrepareRuntimeTaskCancellation captures pending work before workers stop.
// Scheduling identity prevents an old retry from clearing a newer task's work.
func (s *Service) PrepareRuntimeTaskCancellation(
	ctx context.Context, data []byte,
) (interfaces.RuntimeTaskCancellationPlan, error) {
	var payload types.MemoryExtractPayload
	var plan interfaces.RuntimeTaskCancellationPlan
	if err := json.Unmarshal(data, &payload); err != nil {
		return plan, err
	}
	if payload.ScheduledAt.IsZero() {
		return plan, errors.New("memory extraction scheduling identity is missing")
	}
	scope := interfaces.MemoryScope{TenantID: payload.TenantID, SubjectID: payload.SubjectID}
	subject, err := s.repo.GetSubject(ctx, scope)
	if err != nil {
		return plan, err
	}
	var snapshotUpdatedAt time.Time
	if subject != nil && subject.ExtractScheduledAt != nil && subject.ExtractScheduledAt.Equal(payload.ScheduledAt) {
		snapshotUpdatedAt = subject.UpdatedAt
	}
	plan.Cancel = func(cancelCtx context.Context) error {
		return s.repo.CancelPendingExtraction(cancelCtx, scope, payload.ScheduledAt, snapshotUpdatedAt)
	}
	plan.AfterDelete = func(finishCtx context.Context) error {
		return s.scheduleFollowUpIfNeeded(finishCtx, scope,
			s.workspaceConfig(finishCtx, payload.TenantID), payload, false)
	}
	return plan, nil
}
