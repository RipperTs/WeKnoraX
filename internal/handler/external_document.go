package handler

import (
	"context"
	"encoding/json"
	goerrors "errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
	"github.com/gin-gonic/gin"
)

const (
	maxExternalSourceIDLength = 128
	maxExternalIDLength       = 512
)

// UpsertExternalDocument godoc
// @Summary      创建或更新外部文档
// @Description  由 source_id 和 external_id 唯一标识文档；服务端按内容指纹幂等处理
// @Tags         外部文档
// @Accept       multipart/form-data
// @Produce      json
// @Param        id          path      string true  "知识库ID"
// @Param        source_id   formData  string true  "来源系统标识"
// @Param        external_id formData  string true  "来源系统中的文档标识"
// @Param        file        formData  file   true  "文档文件"
// @Param        metadata    formData  string false "业务元数据JSON"
// @Success      200         {object}  map[string]interface{} "文档内容未变化"
// @Success      202         {object}  map[string]interface{} "已提交创建或更新"
// @Failure      400         {object}  errors.AppError
// @Failure      404         {object}  errors.AppError
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /knowledge-bases/{id}/external-documents [put]
func (h *KnowledgeHandler) UpsertExternalDocument(c *gin.Context) {
	ctx, knowledgeBaseID, sourceID, externalID, ok := h.externalDocumentContext(c, true)
	if !ok {
		return
	}

	file, err := c.FormFile("file")
	if err != nil {
		c.Error(errors.NewBadRequestError("file is required").WithDetails(err.Error()))
		return
	}
	maxFileSize := utils.GetMaxFileSizeMB() * 1024 * 1024
	if file.Size > maxFileSize {
		c.Error(errors.NewBadRequestError(fmt.Sprintf("文件大小不能超过%dMB", utils.GetMaxFileSizeMB())))
		return
	}

	metadata := make(map[string]string)
	if rawMetadata := strings.TrimSpace(c.PostForm("metadata")); rawMetadata != "" {
		if err := json.Unmarshal([]byte(rawMetadata), &metadata); err != nil {
			c.Error(errors.NewBadRequestError("metadata must be a JSON object with string values").WithDetails(err.Error()))
			return
		}
		if metadata == nil {
			c.Error(errors.NewBadRequestError("metadata must be a JSON object with string values"))
			return
		}
	}

	result, err := h.externalDocuments.UpsertExternalDocument(
		ctx,
		knowledgeBaseID,
		sourceID,
		externalID,
		file,
		metadata,
	)
	if err != nil {
		h.handleExternalDocumentError(c, err)
		return
	}

	status := http.StatusAccepted
	if result.Action == types.ExternalDocumentActionSkipped {
		status = http.StatusOK
	}
	c.JSON(status, gin.H{"success": true, "data": result})
}

// GetExternalDocument godoc
// @Summary      查询外部文档状态
// @Tags         外部文档
// @Produce      json
// @Param        id          path  string true "知识库ID"
// @Param        source_id   query string true "来源系统标识"
// @Param        external_id query string true "来源系统中的文档标识"
// @Success      200         {object} map[string]interface{}
// @Failure      404         {object} errors.AppError
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /knowledge-bases/{id}/external-documents [get]
func (h *KnowledgeHandler) GetExternalDocument(c *gin.Context) {
	ctx, knowledgeBaseID, sourceID, externalID, ok := h.externalDocumentContext(c, false)
	if !ok {
		return
	}
	result, err := h.externalDocuments.GetExternalDocument(ctx, knowledgeBaseID, sourceID, externalID)
	if err != nil {
		h.handleExternalDocumentError(c, err)
		return
	}
	if result == nil {
		c.Error(errors.NewNotFoundError("external document not found"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

// DeleteExternalDocument godoc
// @Summary      删除外部文档
// @Description  删除操作具有幂等性，文档不存在时返回 skipped
// @Tags         外部文档
// @Produce      json
// @Param        id          path  string true "知识库ID"
// @Param        source_id   query string true "来源系统标识"
// @Param        external_id query string true "来源系统中的文档标识"
// @Success      200         {object} map[string]interface{}
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /knowledge-bases/{id}/external-documents [delete]
func (h *KnowledgeHandler) DeleteExternalDocument(c *gin.Context) {
	ctx, knowledgeBaseID, sourceID, externalID, ok := h.externalDocumentContext(c, false)
	if !ok {
		return
	}
	result, err := h.externalDocuments.DeleteExternalDocument(ctx, knowledgeBaseID, sourceID, externalID)
	if err != nil {
		h.handleExternalDocumentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func (h *KnowledgeHandler) externalDocumentContext(
	c *gin.Context,
	fromForm bool,
) (context.Context, string, string, string, bool) {
	if h.externalDocuments == nil {
		c.Error(errors.NewServiceUnavailableError("external document service is unavailable"))
		return nil, "", "", "", false
	}
	knowledgeBase, knowledgeBaseID, tenantID, permission, err := h.validateKnowledgeBaseAccess(c)
	if err != nil {
		c.Error(err)
		return nil, "", "", "", false
	}
	if knowledgeBase.TenantID != c.GetUint64(types.TenantIDContextKey.String()) {
		c.Error(errors.NewForbiddenError("External document API only supports knowledge bases in the current workspace"))
		return nil, "", "", "", false
	}
	if permission != types.OrgRoleAdmin && permission != types.OrgRoleEditor {
		c.Error(errors.NewForbiddenError("No permission to manage external documents"))
		return nil, "", "", "", false
	}

	sourceID := c.Query("source_id")
	externalID := c.Query("external_id")
	if fromForm {
		sourceID = c.PostForm("source_id")
		externalID = c.PostForm("external_id")
	}
	sourceID = strings.TrimSpace(sourceID)
	externalID = strings.TrimSpace(externalID)
	if sourceID == "" || utf8.RuneCountInString(sourceID) > maxExternalSourceIDLength {
		c.Error(errors.NewBadRequestError("source_id is required and must not exceed 128 characters"))
		return nil, "", "", "", false
	}
	if externalID == "" || utf8.RuneCountInString(externalID) > maxExternalIDLength {
		c.Error(errors.NewBadRequestError("external_id is required and must not exceed 512 characters"))
		return nil, "", "", "", false
	}

	ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, tenantID)
	return ctx, knowledgeBaseID, sourceID, externalID, true
}

func (h *KnowledgeHandler) handleExternalDocumentError(c *gin.Context, err error) {
	var appError *errors.AppError
	if goerrors.As(err, &appError) {
		c.Error(appError)
		return
	}
	if goerrors.Is(err, service.ErrInvalidFileType) {
		c.Error(errors.NewBadRequestError("unsupported file type"))
		return
	}
	var quotaError *types.StorageQuotaExceededError
	if goerrors.As(err, &quotaError) {
		c.Error(errors.NewTooManyRequestsError(quotaError.Message))
		return
	}
	var duplicateError *types.DuplicateKnowledgeError
	if goerrors.As(err, &duplicateError) {
		c.Error(errors.NewConflictError(duplicateError.Message))
		return
	}
	logger.ErrorWithFields(c.Request.Context(), err, nil)
	c.Error(errors.NewInternalServerError("failed to manage external document"))
}
