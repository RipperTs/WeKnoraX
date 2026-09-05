package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// PrepareRuntimeTaskCancellation captures pending work before workers stop.
// Scheduling identity prevents an old retry from clearing a newer task's work.
func (s *Service) PrepareRuntimeTaskCancellation(
	ctx context.Context, data []byte, snapshot json.RawMessage,
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
	var snapshotUpdatedAt time.Time
	if snapshot == nil {
		subject, err := s.repo.GetSubject(ctx, scope)
		if err != nil {
			return plan, err
		}
		if subject != nil && subject.ExtractScheduledAt != nil &&
			subject.ExtractScheduledAt.Equal(payload.ScheduledAt) {
			snapshotUpdatedAt = subject.UpdatedAt
		}
		plan.Snapshot, err = json.Marshal(snapshotUpdatedAt)
		if err != nil {
			return plan, err
		}
	} else {
		if err := json.Unmarshal(snapshot, &snapshotUpdatedAt); err != nil {
			return plan, err
		}
		plan.Snapshot = snapshot
	}
	plan.FinalizeKey = fmt.Sprintf("memory:%d:%s:%s",
		payload.TenantID, payload.SubjectID, payload.ScheduledAt.UTC().Format(time.RFC3339Nano))
	plan.Finalize = func(finishCtx context.Context) error {
		// Quarantine prevents this trigger from running while its scheduling
		// slot is released and any newly arrived work is rearmed.
		if err := s.repo.CancelPendingExtraction(finishCtx, scope, payload.ScheduledAt, snapshotUpdatedAt); err != nil {
			return err
		}
		return s.scheduleFollowUpIfNeeded(finishCtx, scope,
			s.workspaceConfig(finishCtx, payload.TenantID), payload, false)
	}
	return plan, nil
}
