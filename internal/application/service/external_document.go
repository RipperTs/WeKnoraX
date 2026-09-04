package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"

	"github.com/Tencent/WeKnora/internal/common/redislock"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/redis/go-redis/v9"
)

const (
	externalDocumentDataSourcePrefix = "external-api:"
)

type externalDocumentService struct {
	knowledgeService interfaces.KnowledgeService
	redisClient      *redis.Client
}

type externalDocumentUploadContextKey struct{}

type externalDocumentUploadContext struct {
	processingLockKey   string
	replacedKnowledgeID string
}

// NewExternalDocumentService creates the external document synchronization service.
func NewExternalDocumentService(
	knowledgeService interfaces.KnowledgeService,
	redisClient *redis.Client,
) interfaces.ExternalDocumentService {
	return &externalDocumentService{
		knowledgeService: knowledgeService,
		redisClient:      redisClient,
	}
}

func (s *externalDocumentService) UpsertExternalDocument(
	ctx context.Context,
	knowledgeBaseID string,
	sourceID string,
	externalID string,
	file *multipart.FileHeader,
	metadata map[string]string,
) (*types.ExternalDocumentResult, error) {
	if file == nil {
		return nil, fmt.Errorf("external document file is required")
	}
	var result *types.ExternalDocumentResult
	lockKey := externalDocumentLockKey(types.MustTenantIDFromContext(ctx), knowledgeBaseID, sourceID, externalID)
	err := s.withDocumentLock(ctx, lockKey, func(lockCtx context.Context) error {
		fingerprint, err := calculateExternalDocumentFingerprint(file, metadata)
		if err != nil {
			return fmt.Errorf("calculate external document fingerprint: %w", err)
		}

		repo := s.knowledgeService.GetRepository()
		tenantID := types.MustTenantIDFromContext(lockCtx)
		dataSourceID := externalDocumentDataSourceID(sourceID)
		existing, err := repo.FindByDataSourceExternalID(
			lockCtx,
			tenantID,
			knowledgeBaseID,
			dataSourceID,
			externalID,
		)
		if err != nil {
			return fmt.Errorf("find external document: %w", err)
		}

		if externalDocumentCanBeSkipped(existing, fingerprint) {
			result = newExternalDocumentResult(
				types.ExternalDocumentActionSkipped,
				sourceID,
				externalID,
				existing,
			)
			return nil
		}

		processingLockKey := ""
		replacedKnowledgeID := ""
		if existing != nil {
			processingLockKey = lockKey
			replacedKnowledgeID = existing.ID
		}
		knowledge, err := s.knowledgeService.CreateKnowledgeFromFile(
			withExternalDocumentUpload(lockCtx, processingLockKey, replacedKnowledgeID),
			knowledgeBaseID,
			file,
			externalDocumentMetadata(metadata, sourceID, externalID, dataSourceID, fingerprint),
			nil,
			"",
			nil,
			types.ChannelAPI,
			nil,
		)
		if err != nil {
			return err
		}

		if knowledge.ParseStatus == types.ParseStatusFailed {
			createErr := fmt.Errorf(
				"external document %s failed to start processing: %s",
				knowledge.ID,
				knowledge.ErrorMessage,
			)
			if existing != nil {
				return s.rollbackCreatedExternalDocument(lockCtx, tenantID, knowledge.ID, createErr)
			}
			return createErr
		}

		action := types.ExternalDocumentActionCreated
		if existing != nil {
			action = types.ExternalDocumentActionUpdated
			if err := s.knowledgeService.DeleteKnowledge(lockCtx, existing.ID); err != nil {
				logger.Warnf(
					lockCtx,
					"failed to delete replaced external document %s; processing task will retry: %v",
					existing.ID,
					err,
				)
			} else if err := repo.HardDeleteKnowledge(lockCtx, tenantID, existing.ID); err != nil {
				logger.Warnf(lockCtx, "failed to hard-delete replaced external document %s: %v", existing.ID, err)
			}
		}
		result = newExternalDocumentResult(action, sourceID, externalID, knowledge)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *externalDocumentService) rollbackCreatedExternalDocument(
	ctx context.Context,
	tenantID uint64,
	knowledgeID string,
	cause error,
) error {
	rollbackCtx, cancel := externalDocumentRollbackContext(ctx)
	defer cancel()

	if err := s.knowledgeService.DeleteKnowledge(rollbackCtx, knowledgeID); err != nil {
		return errors.Join(cause, fmt.Errorf("rollback external document %s: %w", knowledgeID, err))
	}
	if err := s.knowledgeService.GetRepository().HardDeleteKnowledge(rollbackCtx, tenantID, knowledgeID); err != nil {
		logger.Warnf(rollbackCtx, "failed to hard-delete rolled-back external document %s: %v", knowledgeID, err)
	}
	return cause
}

func externalDocumentRollbackContext(ctx context.Context) (context.Context, context.CancelFunc) {
	rollbackCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopOwnershipWatch := context.AfterFunc(redislock.OwnershipContext(ctx), cancel)
	return rollbackCtx, func() {
		stopOwnershipWatch()
		cancel()
	}
}

func (s *externalDocumentService) GetExternalDocument(
	ctx context.Context,
	knowledgeBaseID string,
	sourceID string,
	externalID string,
) (*types.ExternalDocumentResult, error) {
	var result *types.ExternalDocumentResult
	lockKey := externalDocumentLockKey(types.MustTenantIDFromContext(ctx), knowledgeBaseID, sourceID, externalID)
	err := s.withDocumentLock(ctx, lockKey, func(lockCtx context.Context) error {
		knowledge, err := s.findExternalDocument(lockCtx, knowledgeBaseID, sourceID, externalID)
		if err != nil || knowledge == nil {
			return err
		}
		result = newExternalDocumentResult("", sourceID, externalID, knowledge)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *externalDocumentService) DeleteExternalDocument(
	ctx context.Context,
	knowledgeBaseID string,
	sourceID string,
	externalID string,
) (*types.ExternalDocumentResult, error) {
	var result *types.ExternalDocumentResult
	lockKey := externalDocumentLockKey(types.MustTenantIDFromContext(ctx), knowledgeBaseID, sourceID, externalID)
	err := s.withDocumentLock(ctx, lockKey, func(lockCtx context.Context) error {
		knowledge, err := s.findExternalDocument(lockCtx, knowledgeBaseID, sourceID, externalID)
		if err != nil {
			return err
		}
		if knowledge == nil {
			result = &types.ExternalDocumentResult{
				Action:     types.ExternalDocumentActionSkipped,
				SourceID:   sourceID,
				ExternalID: externalID,
			}
			return nil
		}

		result = newExternalDocumentResult(types.ExternalDocumentActionDeleted, sourceID, externalID, knowledge)
		if err := s.knowledgeService.DeleteKnowledge(lockCtx, knowledge.ID); err != nil {
			return fmt.Errorf("delete external document %s: %w", knowledge.ID, err)
		}
		if err := s.knowledgeService.GetRepository().HardDeleteKnowledge(
			lockCtx,
			types.MustTenantIDFromContext(lockCtx),
			knowledge.ID,
		); err != nil {
			logger.Warnf(lockCtx, "failed to hard-delete external document %s: %v", knowledge.ID, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *externalDocumentService) findExternalDocument(
	ctx context.Context,
	knowledgeBaseID string,
	sourceID string,
	externalID string,
) (*types.Knowledge, error) {
	knowledge, err := s.knowledgeService.GetRepository().FindByDataSourceExternalID(
		ctx,
		types.MustTenantIDFromContext(ctx),
		knowledgeBaseID,
		externalDocumentDataSourceID(sourceID),
		externalID,
	)
	if err != nil {
		return nil, fmt.Errorf("find external document: %w", err)
	}
	return knowledge, nil
}

func (s *externalDocumentService) withDocumentLock(
	ctx context.Context,
	key string,
	fn func(context.Context) error,
) error {
	return withExternalDocumentLock(ctx, s.redisClient, key, fn)
}

func withExternalDocumentLock(
	ctx context.Context,
	redisClient *redis.Client,
	key string,
	fn func(context.Context) error,
) error {
	return withServiceLock(ctx, redisClient, key, fn)
}

func calculateExternalDocumentFingerprint(
	file *multipart.FileHeader,
	metadata map[string]string,
) (string, error) {
	metadataJSON, err := json.Marshal(externalDocumentClientMetadata(metadata))
	if err != nil {
		return "", err
	}

	openedFile, err := file.Open()
	if err != nil {
		return "", err
	}
	defer openedFile.Close()

	hash := sha256.New()
	if _, err := io.WriteString(hash, file.Filename); err != nil {
		return "", err
	}
	if _, err := hash.Write([]byte{0}); err != nil {
		return "", err
	}
	if _, err := hash.Write(metadataJSON); err != nil {
		return "", err
	}
	if _, err := hash.Write([]byte{0}); err != nil {
		return "", err
	}
	if _, err := io.Copy(hash, openedFile); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func externalDocumentMetadata(
	metadata map[string]string,
	sourceID string,
	externalID string,
	dataSourceID string,
	fingerprint string,
) map[string]string {
	result := externalDocumentClientMetadata(metadata)
	result[types.ExternalDocumentSourceIDMetadataKey] = sourceID
	result[types.ExternalDocumentIDMetadataKey] = externalID
	result[types.ExternalDocumentFingerprintMetadataKey] = fingerprint
	result[types.ExternalDocumentDataSourceIDMetadataKey] = dataSourceID
	return result
}

func externalDocumentClientMetadata(metadata map[string]string) map[string]string {
	result := make(map[string]string, len(metadata))
	for key, value := range metadata {
		switch key {
		case types.ExternalDocumentSourceIDMetadataKey,
			types.ExternalDocumentIDMetadataKey,
			types.ExternalDocumentFingerprintMetadataKey,
			types.ExternalDocumentDataSourceIDMetadataKey:
			continue
		default:
			result[key] = value
		}
	}
	return result
}

func externalDocumentCanBeSkipped(knowledge *types.Knowledge, fingerprint string) bool {
	if knowledge == nil {
		return false
	}
	metadata := knowledge.GetMetadata()
	if metadata[types.ExternalDocumentFingerprintMetadataKey] != fingerprint {
		return false
	}
	switch knowledge.ParseStatus {
	case types.ParseStatusPending,
		types.ParseStatusProcessing,
		types.ParseStatusFinalizing,
		types.ParseStatusCompleted:
		return true
	default:
		return false
	}
}

func externalDocumentDataSourceID(sourceID string) string {
	return externalDocumentDataSourcePrefix + sourceID
}

func withExternalDocumentUpload(
	ctx context.Context,
	processingLockKey string,
	replacedKnowledgeID string,
) context.Context {
	return context.WithValue(ctx, externalDocumentUploadContextKey{}, externalDocumentUploadContext{
		processingLockKey:   processingLockKey,
		replacedKnowledgeID: replacedKnowledgeID,
	})
}

func isExternalDocumentUpload(ctx context.Context) bool {
	_, ok := ctx.Value(externalDocumentUploadContextKey{}).(externalDocumentUploadContext)
	return ok
}

func externalDocumentProcessingLockKey(ctx context.Context) string {
	uploadContext, _ := ctx.Value(externalDocumentUploadContextKey{}).(externalDocumentUploadContext)
	return uploadContext.processingLockKey
}

func replacedExternalDocumentKnowledgeID(ctx context.Context) string {
	uploadContext, _ := ctx.Value(externalDocumentUploadContextKey{}).(externalDocumentUploadContext)
	return uploadContext.replacedKnowledgeID
}

func externalDocumentLockKey(tenantID uint64, knowledgeBaseID, sourceID, externalID string) string {
	identity := fmt.Sprintf("%d\x00%s\x00%s\x00%s", tenantID, knowledgeBaseID, sourceID, externalID)
	hash := sha256.Sum256([]byte(identity))
	return "weknora:external-document:" + hex.EncodeToString(hash[:])
}

func newExternalDocumentResult(
	action types.ExternalDocumentAction,
	sourceID string,
	externalID string,
	knowledge *types.Knowledge,
) *types.ExternalDocumentResult {
	result := &types.ExternalDocumentResult{
		Action:     action,
		SourceID:   sourceID,
		ExternalID: externalID,
	}
	if knowledge == nil {
		return result
	}
	metadata := knowledge.GetMetadata()
	updatedAt := knowledge.UpdatedAt
	result.KnowledgeID = knowledge.ID
	result.ParseStatus = knowledge.ParseStatus
	result.ErrorMessage = knowledge.ErrorMessage
	result.ContentFingerprint = metadata[types.ExternalDocumentFingerprintMetadataKey]
	result.UpdatedAt = &updatedAt
	return result
}
