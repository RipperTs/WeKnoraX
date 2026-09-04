package router

import (
	"github.com/Tencent/WeKnora/internal/handler"
	"github.com/gin-gonic/gin"
)

// RegisterExternalDocumentRoutes registers the knowledge-base scoped push API.
func RegisterExternalDocumentRoutes(r *gin.RouterGroup, handler *handler.KnowledgeHandler, g *rbacGuards) {
	documents := g.apiKeyGroup(
		r.Group("/knowledge-bases/:id/external-documents"),
		apiKeyIngest(apiKeyFullAccess()),
	)
	documents.PUT(
		"",
		g.OwnedKBOrAdmin(),
		g.KBAccessWrite("id"),
		handler.UpsertExternalDocument,
	)
	documents.GET(
		"",
		g.OwnedKBOrAdmin(),
		g.KBAccessWrite("id"),
		handler.GetExternalDocument,
	)
	documents.DELETE(
		"",
		g.OwnedKBOrAdmin(),
		g.KBAccessWrite("id"),
		handler.DeleteExternalDocument,
	)
}
