package interfaces

import (
	"context"
	"mime/multipart"

	"github.com/Tencent/WeKnora/internal/types"
)

// ExternalDocumentService manages documents owned by third-party systems.
type ExternalDocumentService interface {
	UpsertExternalDocument(
		ctx context.Context,
		knowledgeBaseID string,
		sourceID string,
		externalID string,
		file *multipart.FileHeader,
		metadata map[string]string,
	) (*types.ExternalDocumentResult, error)
	GetExternalDocument(
		ctx context.Context,
		knowledgeBaseID string,
		sourceID string,
		externalID string,
	) (*types.ExternalDocumentResult, error)
	DeleteExternalDocument(
		ctx context.Context,
		knowledgeBaseID string,
		sourceID string,
		externalID string,
	) (*types.ExternalDocumentResult, error)
}
