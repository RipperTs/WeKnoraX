package handler

import (
	"errors"
	"net/http"
	"strings"

	apprepo "github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
)

// IntegrationHandler exposes third-party application, consent and connection endpoints.
type IntegrationHandler struct {
	service *service.IntegrationService
}

// NewIntegrationHandler creates the third-party integration HTTP handler.
func NewIntegrationHandler(integrationService *service.IntegrationService) *IntegrationHandler {
	return &IntegrationHandler{service: integrationService}
}

// ListApplications godoc
// @Summary 列出第三方应用
// @Tags 第三方集成
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Security Bearer
// @Router /system/admin/integration-applications [get]
func (h *IntegrationHandler) ListApplications(c *gin.Context) {
	apps, err := h.service.ListApplications(c.Request.Context())
	if err != nil {
		writeIntegrationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": apps})
}

// CreateApplication godoc
// @Summary 注册第三方应用
// @Description 注册应用并仅在本次响应中返回客户端密钥
// @Tags 第三方集成
// @Accept json
// @Produce json
// @Param request body service.IntegrationApplicationInput true "应用配置"
// @Success 201 {object} map[string]interface{}
// @Security Bearer
// @Router /system/admin/integration-applications [post]
func (h *IntegrationHandler) CreateApplication(c *gin.Context) {
	preventIntegrationResponseCaching(c)
	var input service.IntegrationApplicationInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request data"})
		return
	}
	userID, _ := types.UserIDFromContext(c.Request.Context())
	result, err := h.service.CreateApplication(c.Request.Context(), userID, input)
	if err != nil {
		writeIntegrationError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": result})
}

// UpdateApplication godoc
// @Summary 更新第三方应用
// @Tags 第三方集成
// @Accept json
// @Produce json
// @Param id path string true "应用 ID"
// @Param request body service.IntegrationApplicationInput true "应用配置"
// @Success 200 {object} map[string]interface{}
// @Security Bearer
// @Router /system/admin/integration-applications/{id} [put]
func (h *IntegrationHandler) UpdateApplication(c *gin.Context) {
	var input service.IntegrationApplicationInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request data"})
		return
	}
	app, err := h.service.UpdateApplication(c.Request.Context(), c.Param("id"), input)
	if err != nil {
		writeIntegrationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": app})
}

// RotateApplicationSecret godoc
// @Summary 轮换第三方应用密钥
// @Description 旧密钥立即失效，新密钥仅在本次响应中返回
// @Tags 第三方集成
// @Produce json
// @Param id path string true "应用 ID"
// @Success 200 {object} map[string]interface{}
// @Security Bearer
// @Router /system/admin/integration-applications/{id}/rotate-secret [post]
func (h *IntegrationHandler) RotateApplicationSecret(c *gin.Context) {
	preventIntegrationResponseCaching(c)
	result, err := h.service.RotateApplicationSecret(c.Request.Context(), c.Param("id"))
	if err != nil {
		writeIntegrationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

// ListTenantApplications godoc
// @Summary 列出当前空间可配置的第三方应用
// @Tags 第三方集成
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Security Bearer
// @Router /integrations/applications [get]
func (h *IntegrationHandler) ListTenantApplications(c *gin.Context) {
	views, err := h.service.ListTenantApplications(
		c.Request.Context(), c.GetUint64(types.TenantIDContextKey.String()),
	)
	if err != nil {
		writeIntegrationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": views})
}

// ListTenantKnowledgeBases godoc
// @Summary 列出当前空间可授权的知识库
// @Tags 第三方集成
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Security Bearer
// @Router /integrations/knowledge-bases [get]
func (h *IntegrationHandler) ListTenantKnowledgeBases(c *gin.Context) {
	knowledgeBases, err := h.service.ListTenantIntegrationKnowledgeBases(
		c.Request.Context(),
		c.GetUint64(types.TenantIDContextKey.String()),
		types.TenantRoleFromContext(c.Request.Context()),
	)
	if err != nil {
		writeIntegrationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": knowledgeBases})
}

// UpsertTenantPolicy godoc
// @Summary 配置第三方应用的空间策略
// @Tags 第三方集成
// @Accept json
// @Produce json
// @Param id path string true "应用 ID"
// @Param request body service.TenantIntegrationPolicyInput true "空间策略"
// @Success 200 {object} map[string]interface{}
// @Security Bearer
// @Router /integrations/applications/{id}/policy [put]
func (h *IntegrationHandler) UpsertTenantPolicy(c *gin.Context) {
	var input service.TenantIntegrationPolicyInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request data"})
		return
	}
	policy, err := h.service.UpsertTenantPolicy(
		c.Request.Context(),
		c.GetUint64(types.TenantIDContextKey.String()),
		types.TenantRoleFromContext(c.Request.Context()),
		c.Param("id"),
		input,
	)
	if err != nil {
		writeIntegrationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": policy})
}

// GetAuthorization godoc
// @Summary 获取第三方连接授权信息
// @Tags 第三方集成
// @Produce json
// @Param client_id query string true "客户端 ID"
// @Param redirect_uri query string true "回调地址"
// @Param state query string true "状态值"
// @Param scope query string true "空格分隔的权限范围"
// @Param code_challenge query string true "PKCE S256 challenge"
// @Param code_challenge_method query string true "固定为 S256"
// @Success 200 {object} map[string]interface{}
// @Security Bearer
// @Router /integrations/authorization [get]
func (h *IntegrationHandler) GetAuthorization(c *gin.Context) {
	params := authorizationParametersFromQuery(c)
	userID, _ := types.UserIDFromContext(c.Request.Context())
	view, err := h.service.GetAuthorizationView(
		c.Request.Context(),
		c.GetUint64(types.TenantIDContextKey.String()),
		userID,
		types.TenantRoleFromContext(c.Request.Context()),
		params,
	)
	if err != nil {
		writeIntegrationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": view})
}

// Authorize godoc
// @Summary 确认或拒绝第三方连接
// @Tags 第三方集成
// @Accept json
// @Produce json
// @Param request body service.IntegrationAuthorizationDecision true "授权决定"
// @Success 200 {object} map[string]interface{}
// @Security Bearer
// @Router /integrations/authorization [post]
func (h *IntegrationHandler) Authorize(c *gin.Context) {
	preventIntegrationResponseCaching(c)
	var decision service.IntegrationAuthorizationDecision
	if err := c.ShouldBindJSON(&decision); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request data"})
		return
	}
	userID, _ := types.UserIDFromContext(c.Request.Context())
	result, err := h.service.Authorize(
		c.Request.Context(),
		c.GetUint64(types.TenantIDContextKey.String()),
		userID,
		types.TenantRoleFromContext(c.Request.Context()),
		decision,
	)
	if err != nil {
		writeIntegrationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

// ExchangeToken godoc
// @Summary 交换第三方用户连接凭证
// @Description 使用一次性授权码、客户端密钥和 PKCE verifier 换取 wkic_ 凭证
// @Tags 第三方集成
// @Accept json,x-www-form-urlencoded
// @Produce json
// @Param request body service.IntegrationTokenExchangeRequest true "授权码交换参数"
// @Success 200 {object} service.IntegrationTokenExchangeResult
// @Router /integrations/token [post]
func (h *IntegrationHandler) ExchangeToken(c *gin.Context) {
	preventIntegrationResponseCaching(c)
	var req service.IntegrationTokenExchangeRequest
	if err := c.ShouldBind(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}
	result, err := h.service.ExchangeAuthorizationCode(c.Request.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrIntegrationInvalidApplication):
			c.JSON(http.StatusUnauthorized, gin.H{"error": integrationOAuthError(err)})
		case errors.Is(err, apprepo.ErrIntegrationAuthorizationCode),
			errors.Is(err, service.ErrIntegrationInvalidPKCE),
			errors.Is(err, service.ErrIntegrationApplicationDisabled),
			errors.Is(err, service.ErrIntegrationPolicyDisabled):
			c.JSON(http.StatusBadRequest, gin.H{"error": integrationOAuthError(err)})
		default:
			logger.ErrorWithFields(c.Request.Context(), err, nil)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "server_error"})
		}
		return
	}
	c.JSON(http.StatusOK, result)
}

// ListConnections godoc
// @Summary 列出当前用户的第三方连接
// @Tags 第三方集成
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Security Bearer
// @Router /integrations/connections [get]
func (h *IntegrationHandler) ListConnections(c *gin.Context) {
	userID, _ := types.UserIDFromContext(c.Request.Context())
	views, err := h.service.ListUserConnections(
		c.Request.Context(),
		c.GetUint64(types.TenantIDContextKey.String()),
		userID,
		types.TenantRoleFromContext(c.Request.Context()),
	)
	if err != nil {
		writeIntegrationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": views})
}

// GetConnection godoc
// @Summary 获取当前用户的第三方连接
// @Tags 第三方集成
// @Produce json
// @Param id path string true "连接 ID"
// @Success 200 {object} map[string]interface{}
// @Security Bearer
// @Router /integrations/connections/{id} [get]
func (h *IntegrationHandler) GetConnection(c *gin.Context) {
	userID, _ := types.UserIDFromContext(c.Request.Context())
	view, err := h.service.GetUserConnection(
		c.Request.Context(),
		c.GetUint64(types.TenantIDContextKey.String()),
		userID,
		c.Param("id"),
		types.TenantRoleFromContext(c.Request.Context()),
	)
	if err != nil {
		writeIntegrationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": view})
}

// RevokeConnection godoc
// @Summary 撤销当前用户的第三方连接
// @Tags 第三方集成
// @Produce json
// @Param id path string true "连接 ID"
// @Success 200 {object} map[string]interface{}
// @Security Bearer
// @Router /integrations/connections/{id} [delete]
func (h *IntegrationHandler) RevokeConnection(c *gin.Context) {
	userID, _ := types.UserIDFromContext(c.Request.Context())
	if err := h.service.RevokeUserConnection(
		c.Request.Context(),
		c.GetUint64(types.TenantIDContextKey.String()),
		userID,
		c.Param("id"),
	); err != nil {
		writeIntegrationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func authorizationParametersFromQuery(c *gin.Context) service.IntegrationAuthorizationParameters {
	return service.IntegrationAuthorizationParameters{
		ClientID:            c.Query("client_id"),
		RedirectURI:         c.Query("redirect_uri"),
		State:               c.Query("state"),
		Scopes:              strings.Fields(c.Query("scope")),
		CodeChallenge:       c.Query("code_challenge"),
		CodeChallengeMethod: c.Query("code_challenge_method"),
	}
}

func integrationOAuthError(err error) string {
	switch {
	case errors.Is(err, service.ErrIntegrationInvalidApplication):
		return "invalid_client"
	case errors.Is(err, service.ErrIntegrationInvalidPKCE):
		return "invalid_grant"
	case errors.Is(err, service.ErrIntegrationPolicyDisabled),
		errors.Is(err, service.ErrIntegrationApplicationDisabled):
		return "access_denied"
	default:
		return "invalid_grant"
	}
}

func writeIntegrationError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	message := "Failed to process integration request"
	switch {
	case errors.Is(err, apprepo.ErrIntegrationApplicationNotFound),
		errors.Is(err, apprepo.ErrIntegrationConnectionNotFound):
		status, message = http.StatusNotFound, "Integration resource not found"
	case errors.Is(err, service.ErrIntegrationInvalidApplication),
		errors.Is(err, service.ErrIntegrationInvalidRedirectURI),
		errors.Is(err, service.ErrIntegrationInvalidScope),
		errors.Is(err, service.ErrIntegrationInvalidState),
		errors.Is(err, service.ErrIntegrationInvalidPKCE):
		status, message = http.StatusBadRequest, err.Error()
	case errors.Is(err, service.ErrIntegrationApplicationDisabled),
		errors.Is(err, service.ErrIntegrationPolicyDisabled),
		errors.Is(err, service.ErrIntegrationAccessDenied),
		errors.Is(err, service.ErrIntegrationConsentRequired):
		status, message = http.StatusForbidden, err.Error()
	}
	c.JSON(status, gin.H{"error": message})
}

func preventIntegrationResponseCaching(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
}
