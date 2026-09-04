package router

import (
	"github.com/Tencent/WeKnora/internal/handler"
	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/gin-gonic/gin"
)

// RegisterIntegrationRoutes exposes global client registration, tenant policy,
// user consent/connection management and the public authorization-code exchange.
func RegisterIntegrationRoutes(
	r *gin.RouterGroup,
	handler *handler.IntegrationHandler,
	g *rbacGuards,
) {
	admin := r.Group("/system/admin/integration-applications", g.SystemAdmin())
	{
		admin.GET("", handler.ListApplications)
		admin.POST("", handler.CreateApplication)
		admin.PUT("/:id", handler.UpdateApplication)
		admin.DELETE("/:id", handler.DeleteApplication)
		admin.POST("/:id/rotate-secret", handler.RotateApplicationSecret)
		admin.POST("/:id/test-callbacks", handler.TestApplicationCallbacks)
	}

	integrations := r.Group("/integrations")
	{
		// OAuth-style confidential-client code exchange. Auth skips this exact
		// route; public rate limiting bounds invalid client/code attempts.
		integrations.POST("/token", middleware.PublicAuthRateLimit(), handler.ExchangeToken)

		integrations.GET("/applications", g.Viewer(), handler.ListTenantApplications)
		integrations.PUT("/applications/:id/policy", g.Admin(), handler.UpsertTenantPolicy)

		integrations.GET("/authorization", g.Viewer(), handler.GetAuthorization)
		integrations.POST("/authorization", g.Viewer(), handler.Authorize)

		integrations.GET("/connections", g.Viewer(), handler.ListConnections)
		integrations.GET("/connections/:id", g.Viewer(), handler.GetConnection)
		integrations.DELETE("/connections/:id", g.Viewer(), handler.RevokeConnection)
	}
}
