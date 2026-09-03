package interfaces

import (
	"context"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

// IntegrationRepository owns persistence and the two transactions whose
// atomicity is security-sensitive: consent approval and code exchange.
type IntegrationRepository interface {
	CreateApplication(ctx context.Context, app *types.IntegrationApplication) error
	UpdateApplication(ctx context.Context, app *types.IntegrationApplication) error
	UpdateApplicationSecret(ctx context.Context, id, clientSecretHash string, updatedAt time.Time) error
	GetApplicationByID(ctx context.Context, id string) (*types.IntegrationApplication, error)
	GetApplicationByClientID(ctx context.Context, clientID string) (*types.IntegrationApplication, error)
	ListApplications(ctx context.Context) ([]*types.IntegrationApplication, error)

	GetTenantPolicy(
		ctx context.Context, applicationID string, tenantID uint64,
	) (*types.TenantIntegrationPolicy, error)
	ListTenantPolicies(ctx context.Context, tenantID uint64) ([]*types.TenantIntegrationPolicy, error)
	UpsertTenantPolicy(ctx context.Context, policy *types.TenantIntegrationPolicy) error

	FindConnection(
		ctx context.Context, applicationID string, tenantID uint64, userID string,
	) (*types.IntegrationConnection, error)
	GetConnectionByID(ctx context.Context, id string) (*types.IntegrationConnection, error)
	ListConnectionsByUser(ctx context.Context, userID string) ([]*types.IntegrationConnection, error)
	ListConnectionKnowledgeBaseIDs(ctx context.Context, connectionID string) ([]string, error)
	SaveAuthorization(
		ctx context.Context,
		connection *types.IntegrationConnection,
		knowledgeBaseIDs []string,
		code *types.IntegrationAuthorizationCode,
	) (*types.IntegrationConnection, error)
	RevokeConnection(ctx context.Context, connectionID, userID string, revokedAt time.Time) error

	GetAuthorizationCode(ctx context.Context, codeHash string) (*types.IntegrationAuthorizationCode, error)
	ConsumeAuthorizationCodeAndCreateCredential(
		ctx context.Context,
		codeHash string,
		now time.Time,
		credential *types.IntegrationCredential,
	) error

	GetCredentialByHash(ctx context.Context, tokenHash string) (*types.IntegrationCredential, error)
	TouchCredential(ctx context.Context, credentialID, connectionID string, usedAt time.Time) error
}
