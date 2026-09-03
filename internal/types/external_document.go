package types

import "time"

const (
	// ExternalDocumentSourceIDMetadataKey identifies the source system that owns
	// an externally managed document.
	ExternalDocumentSourceIDMetadataKey = "external_source_id"
	// ExternalDocumentIDMetadataKey identifies a document inside its source system.
	ExternalDocumentIDMetadataKey = "external_id"
	// ExternalDocumentFingerprintMetadataKey stores the server-computed content fingerprint.
	ExternalDocumentFingerprintMetadataKey = "external_fingerprint"
	// ExternalDocumentDataSourceIDMetadataKey reuses the existing indexed external-document lookup.
	ExternalDocumentDataSourceIDMetadataKey = "datasource_id"
)

// ExternalDocumentAction describes the result of an external document mutation.
type ExternalDocumentAction string

const (
	ExternalDocumentActionCreated ExternalDocumentAction = "created"
	ExternalDocumentActionUpdated ExternalDocumentAction = "updated"
	ExternalDocumentActionSkipped ExternalDocumentAction = "skipped"
	ExternalDocumentActionDeleted ExternalDocumentAction = "deleted"
)

// ExternalDocumentResult is the stable response returned to external document clients.
type ExternalDocumentResult struct {
	Action             ExternalDocumentAction `json:"action,omitempty"`
	SourceID           string                 `json:"source_id"`
	ExternalID         string                 `json:"external_id"`
	KnowledgeID        string                 `json:"knowledge_id,omitempty"`
	ParseStatus        string                 `json:"parse_status,omitempty"`
	ErrorMessage       string                 `json:"error_message,omitempty"`
	ContentFingerprint string                 `json:"content_fingerprint,omitempty"`
	UpdatedAt          *time.Time             `json:"updated_at,omitempty"`
}
