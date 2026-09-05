package interfaces

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
)

// RuntimeTaskCancellationQueue supplies bounded task operations to the business
// cancellation service. Implemented only by the distributed queue backend.
type RuntimeTaskCancellationQueue interface {
	SnapshotPendingRuntimeTaskIDs(ctx context.Context, queue string) ([]string, error)
	GetPendingRuntimeCancellationTask(ctx context.Context, queue, id string) (*types.RuntimeCancellationTask, error)
	DeletePendingRuntimeCancellationTask(ctx context.Context, task *types.RuntimeCancellationTask) (bool, error)
	CancelRuntimeKnowledgeTasks(ctx context.Context, targets []types.RuntimeCancelledKnowledge) (int, int, error)
}
