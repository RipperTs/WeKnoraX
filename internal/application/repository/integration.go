package repository

import (
	"context"
	"errors"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	// ErrIntegrationApplicationNotFound indicates that no registered application matches the lookup.
	ErrIntegrationApplicationNotFound = errors.New("integration application not found")
	// ErrIntegrationConnectionNotFound indicates that no user connection matches the lookup.
	ErrIntegrationConnectionNotFound = errors.New("integration connection not found")
	// ErrIntegrationCredentialNotFound indicates that no active credential matches the token hash.
	ErrIntegrationCredentialNotFound = errors.New("integration credential not found")
	// ErrIntegrationAuthorizationCode indicates an unknown, expired or consumed authorization code.
	ErrIntegrationAuthorizationCode = errors.New("integration authorization code is invalid or expired")
)

type integrationRepository struct {
	db *gorm.DB
}

// NewIntegrationRepository creates the persistence adapter for third-party integrations.
func NewIntegrationRepository(db *gorm.DB) interfaces.IntegrationRepository {
	return &integrationRepository{db: db}
}

func (r *integrationRepository) CreateApplication(
	ctx context.Context, app *types.IntegrationApplication,
) error {
	return r.db.WithContext(ctx).Create(app).Error
}

func (r *integrationRepository) UpdateApplication(
	ctx context.Context, app *types.IntegrationApplication,
) error {
	result := r.db.WithContext(ctx).Model(&types.IntegrationApplication{}).
		Where("id = ?", app.ID).
		Updates(map[string]any{
			"name":           app.Name,
			"description":    app.Description,
			"redirect_uris":  app.RedirectURIs,
			"allowed_scopes": app.AllowedScopes,
			"enabled":        app.Enabled,
			"updated_at":     app.UpdatedAt,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrIntegrationApplicationNotFound
	}
	return nil
}

func (r *integrationRepository) DeleteApplication(ctx context.Context, id string) error {
	result := r.db.WithContext(ctx).Where("id = ?", id).Delete(&types.IntegrationApplication{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrIntegrationApplicationNotFound
	}
	return nil
}

func (r *integrationRepository) UpdateApplicationSecret(
	ctx context.Context, id, clientSecretHash string, updatedAt time.Time,
) error {
	result := r.db.WithContext(ctx).Model(&types.IntegrationApplication{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"client_secret_hash": clientSecretHash,
			"updated_at":         updatedAt,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrIntegrationApplicationNotFound
	}
	return nil
}

func (r *integrationRepository) GetApplicationByID(
	ctx context.Context, id string,
) (*types.IntegrationApplication, error) {
	var app types.IntegrationApplication
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&app).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrIntegrationApplicationNotFound
		}
		return nil, err
	}
	return &app, nil
}

func (r *integrationRepository) GetApplicationByClientID(
	ctx context.Context, clientID string,
) (*types.IntegrationApplication, error) {
	var app types.IntegrationApplication
	if err := r.db.WithContext(ctx).Where("client_id = ?", clientID).First(&app).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrIntegrationApplicationNotFound
		}
		return nil, err
	}
	return &app, nil
}

func (r *integrationRepository) ListApplications(
	ctx context.Context,
) ([]*types.IntegrationApplication, error) {
	var apps []*types.IntegrationApplication
	err := r.db.WithContext(ctx).Order("created_at DESC").Find(&apps).Error
	return apps, err
}

func (r *integrationRepository) GetTenantPolicy(
	ctx context.Context, applicationID string, tenantID uint64,
) (*types.TenantIntegrationPolicy, error) {
	var policy types.TenantIntegrationPolicy
	err := r.db.WithContext(ctx).
		Where("application_id = ? AND tenant_id = ?", applicationID, tenantID).
		First(&policy).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &policy, nil
}

func (r *integrationRepository) ListTenantPolicies(
	ctx context.Context, tenantID uint64,
) ([]*types.TenantIntegrationPolicy, error) {
	var policies []*types.TenantIntegrationPolicy
	err := r.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Order("created_at ASC").
		Find(&policies).Error
	return policies, err
}

func (r *integrationRepository) UpsertTenantPolicy(
	ctx context.Context, policy *types.TenantIntegrationPolicy,
) error {
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "application_id"}, {Name: "tenant_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"enabled":        policy.Enabled,
			"allowed_scopes": policy.AllowedScopes,
			"updated_at":     policy.UpdatedAt,
		}),
	}).Create(policy).Error
}

func (r *integrationRepository) ListApplicationPolicies(
	ctx context.Context, applicationID string, tenantIDs []uint64,
) ([]*types.TenantIntegrationPolicy, error) {
	if len(tenantIDs) == 0 {
		return nil, nil
	}
	var policies []*types.TenantIntegrationPolicy
	err := r.db.WithContext(ctx).
		Where("application_id = ? AND tenant_id IN ?", applicationID, tenantIDs).
		Find(&policies).Error
	return policies, err
}

func (r *integrationRepository) FindConnection(
	ctx context.Context, applicationID, userID string,
) (*types.IntegrationConnection, error) {
	var connection types.IntegrationConnection
	err := r.db.WithContext(ctx).
		Where("application_id = ? AND user_id = ?", applicationID, userID).
		First(&connection).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &connection, nil
}

func (r *integrationRepository) GetConnectionByID(
	ctx context.Context, id string,
) (*types.IntegrationConnection, error) {
	var connection types.IntegrationConnection
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&connection).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrIntegrationConnectionNotFound
		}
		return nil, err
	}
	return &connection, nil
}

func (r *integrationRepository) ListConnectionsByUser(
	ctx context.Context, userID string,
) ([]*types.IntegrationConnection, error) {
	var connections []*types.IntegrationConnection
	err := r.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("updated_at DESC").
		Find(&connections).Error
	return connections, err
}

func (r *integrationRepository) ListConnectionTenantIDs(
	ctx context.Context, connectionID string,
) ([]uint64, error) {
	var ids []uint64
	err := r.db.WithContext(ctx).
		Model(&types.IntegrationConnectionTenant{}).
		Where("connection_id = ?", connectionID).
		Order("created_at ASC, tenant_id ASC").
		Pluck("tenant_id", &ids).Error
	return ids, err
}

func (r *integrationRepository) SaveAuthorization(
	ctx context.Context,
	connection *types.IntegrationConnection,
	tenantIDs []uint64,
	code *types.IntegrationAuthorizationCode,
) (*types.IntegrationConnection, error) {
	var saved types.IntegrationConnection
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "application_id"}, {Name: "user_id"}},
			DoUpdates: clause.Assignments(map[string]any{
				"scopes":     connection.Scopes,
				"status":     types.IntegrationConnectionActive,
				"updated_at": connection.UpdatedAt,
			}),
		}).Create(connection).Error; err != nil {
			return err
		}

		if err := tx.Where(
			"application_id = ? AND user_id = ?",
			connection.ApplicationID, connection.UserID,
		).First(&saved).Error; err != nil {
			return err
		}

		if err := tx.Where("connection_id = ?", saved.ID).
			Delete(&types.IntegrationConnectionTenant{}).Error; err != nil {
			return err
		}
		if len(tenantIDs) > 0 {
			grants := make([]*types.IntegrationConnectionTenant, 0, len(tenantIDs))
			for _, tenantID := range tenantIDs {
				grants = append(grants, &types.IntegrationConnectionTenant{
					ConnectionID: saved.ID, TenantID: tenantID,
				})
			}
			if err := tx.Create(&grants).Error; err != nil {
				return err
			}
		}

		if err := tx.Where("connection_id = ?", saved.ID).
			Delete(&types.IntegrationAuthorizationCode{}).Error; err != nil {
			return err
		}
		code.ConnectionID = saved.ID
		return tx.Create(code).Error
	})
	if err != nil {
		return nil, err
	}
	return &saved, nil
}

func (r *integrationRepository) RevokeConnection(
	ctx context.Context, connectionID, userID string, revokedAt time.Time,
) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&types.IntegrationConnection{}).
			Where("id = ? AND user_id = ? AND status = ?", connectionID, userID, types.IntegrationConnectionActive).
			Updates(map[string]any{
				"status":     types.IntegrationConnectionRevoked,
				"updated_at": revokedAt,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrIntegrationConnectionNotFound
		}
		return tx.Model(&types.IntegrationCredential{}).
			Where("connection_id = ? AND revoked_at IS NULL", connectionID).
			Update("revoked_at", revokedAt).Error
	})
}

func (r *integrationRepository) GetAuthorizationCode(
	ctx context.Context, codeHash string,
) (*types.IntegrationAuthorizationCode, error) {
	var code types.IntegrationAuthorizationCode
	if err := r.db.WithContext(ctx).Where("code_hash = ?", codeHash).First(&code).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrIntegrationAuthorizationCode
		}
		return nil, err
	}
	return &code, nil
}

func (r *integrationRepository) ConsumeAuthorizationCodeAndCreateCredential(
	ctx context.Context,
	codeHash string,
	now time.Time,
	credential *types.IntegrationCredential,
) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&types.IntegrationAuthorizationCode{}).
			Where("code_hash = ? AND consumed_at IS NULL AND expires_at > ?", codeHash, now).
			Update("consumed_at", now)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrIntegrationAuthorizationCode
		}
		if err := tx.Model(&types.IntegrationCredential{}).
			Where("connection_id = ? AND revoked_at IS NULL", credential.ConnectionID).
			Update("revoked_at", now).Error; err != nil {
			return err
		}
		return tx.Create(credential).Error
	})
}

func (r *integrationRepository) GetCredentialByHash(
	ctx context.Context, tokenHash string,
) (*types.IntegrationCredential, error) {
	var credential types.IntegrationCredential
	err := r.db.WithContext(ctx).
		Where("token_hash = ? AND revoked_at IS NULL", tokenHash).
		First(&credential).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrIntegrationCredentialNotFound
	}
	if err != nil {
		return nil, err
	}
	return &credential, nil
}

func (r *integrationRepository) TouchCredential(
	ctx context.Context, credentialID, connectionID string, usedAt time.Time,
) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&types.IntegrationCredential{}).
			Where("id = ? AND revoked_at IS NULL", credentialID).
			Update("last_used_at", usedAt).Error; err != nil {
			return err
		}
		return tx.Model(&types.IntegrationConnection{}).
			Where("id = ? AND status = ?", connectionID, types.IntegrationConnectionActive).
			Update("last_used_at", usedAt).Error
	})
}
