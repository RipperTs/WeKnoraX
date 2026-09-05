package repository

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrTenantNotFound         = errors.New("tenant not found")
	ErrTenantHasKnowledgeBase = errors.New("tenant has associated knowledge bases")
)

// ErrStorageQuotaNotIncreasable rejects unlimited quotas and additions exceeding int64 capacity.
var ErrStorageQuotaNotIncreasable = errors.New("storage quota is unlimited or would exceed the supported maximum")

// tenantRepository implements tenant repository interface
type tenantRepository struct {
	db *gorm.DB
}

// NewTenantRepository creates a new tenant repository
func NewTenantRepository(db *gorm.DB) interfaces.TenantRepository {
	return &tenantRepository{db: db}
}

// CreateTenant creates tenant
func (r *tenantRepository) CreateTenant(ctx context.Context, tenant *types.Tenant) error {
	return r.db.WithContext(ctx).Create(tenant).Error
}

// GetTenantByID gets tenant by ID
func (r *tenantRepository) GetTenantByID(ctx context.Context, id uint64) (*types.Tenant, error) {
	var tenant types.Tenant
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&tenant).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTenantNotFound
		}
		return nil, err
	}
	return &tenant, nil
}

// GetTenantsByIDs batches GetTenantByID with a single IN-list query.
// Returns a map keyed by tenant ID; missing rows are simply absent from
// the map (no error). An empty input slice short-circuits to an empty map
// without hitting the database.
func (r *tenantRepository) GetTenantsByIDs(ctx context.Context, ids []uint64) (map[uint64]*types.Tenant, error) {
	if len(ids) == 0 {
		return map[uint64]*types.Tenant{}, nil
	}
	var tenants []*types.Tenant
	if err := r.db.WithContext(ctx).Where("id IN ?", ids).Find(&tenants).Error; err != nil {
		return nil, err
	}
	out := make(map[uint64]*types.Tenant, len(tenants))
	for _, t := range tenants {
		if t != nil {
			out[t.ID] = t
		}
	}
	return out, nil
}

// ListTenants lists all tenants
func (r *tenantRepository) ListTenants(ctx context.Context) ([]*types.Tenant, error) {
	var tenants []*types.Tenant
	if err := r.db.WithContext(ctx).Order("created_at DESC").Find(&tenants).Error; err != nil {
		return nil, err
	}
	return tenants, nil
}

// SearchTenants searches tenants with pagination and filters
func (r *tenantRepository) SearchTenants(ctx context.Context, keyword string, tenantID uint64, page, pageSize int) ([]*types.Tenant, int64, error) {
	var tenants []*types.Tenant
	var total int64

	query := r.db.WithContext(ctx).Model(&types.Tenant{})

	// Build search conditions
	if tenantID > 0 && keyword != "" {
		escaped := escapeLikeKeyword(keyword)
		query = query.Where("id = ? OR name LIKE ? OR description LIKE ?", tenantID, "%"+escaped+"%", "%"+escaped+"%")
	} else if tenantID > 0 {
		query = query.Where("id = ?", tenantID)
	} else if keyword != "" {
		escaped := escapeLikeKeyword(keyword)
		query = query.Where("name LIKE ? OR description LIKE ?", "%"+escaped+"%", "%"+escaped+"%")
	}

	// Count total
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	// Apply pagination
	if page > 0 && pageSize > 0 {
		offset := (page - 1) * pageSize
		query = query.Offset(offset).Limit(pageSize)
	}

	// Order by created_at DESC
	query = query.Order("created_at DESC")

	// Execute query
	if err := query.Find(&tenants).Error; err != nil {
		return nil, 0, err
	}

	return tenants, total, nil
}

// ListSystemTenants returns a stable page with active owners, without workspace credentials.
func (r *tenantRepository) ListSystemTenants(
	ctx context.Context, query string, offset, limit int,
) ([]*types.SystemTenant, int64, error) {
	base := r.db.WithContext(ctx).Model(&types.Tenant{})
	if query = strings.TrimSpace(query); query != "" {
		like := "%" + escapeLikePattern(query) + "%"
		owners := r.db.WithContext(ctx).Model(&types.TenantMember{}).
			Select("tenant_members.tenant_id").
			Joins("JOIN users ON users.id = tenant_members.user_id AND users.deleted_at IS NULL").
			Where("tenant_members.role = ? AND tenant_members.status = ?",
				types.TenantRoleOwner, types.TenantMemberStatusActive).
			Where("(LOWER(users.username) LIKE LOWER(?) ESCAPE ? OR LOWER(users.email) LIKE LOWER(?) ESCAPE ?)",
				like, `\`, like, `\`)
		filter := r.db.Where("LOWER(tenants.name) LIKE LOWER(?) ESCAPE ? OR tenants.id IN (?)", like, `\`, owners)
		if id, err := strconv.ParseUint(query, 10, 64); err == nil {
			filter = filter.Or("tenants.id = ?", id)
		}
		base = base.Where(filter)
	}
	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	tenants := make([]*types.SystemTenant, 0)
	if err := base.Select("tenants.id, tenants.name, tenants.storage_quota, tenants.storage_used").
		Order("tenants.created_at DESC, tenants.id ASC").Offset(offset).Limit(limit).Find(&tenants).Error; err != nil {
		return nil, 0, err
	}
	if len(tenants) == 0 {
		return tenants, total, nil
	}
	ids := make([]uint64, 0, len(tenants))
	byID := make(map[uint64]*types.SystemTenant, len(tenants))
	for _, tenant := range tenants {
		tenant.Owners = make([]types.SystemTenantOwner, 0)
		ids = append(ids, tenant.ID)
		byID[tenant.ID] = tenant
	}
	var owners []struct {
		TenantID                uint64
		types.SystemTenantOwner `gorm:"embedded"`
	}
	if err := r.db.WithContext(ctx).Model(&types.TenantMember{}).
		Select("tenant_members.tenant_id, tenant_members.user_id, users.username, users.email").
		Joins("JOIN users ON users.id = tenant_members.user_id AND users.deleted_at IS NULL").
		Where("tenant_members.tenant_id IN ? AND tenant_members.role = ? AND tenant_members.status = ?",
			ids, types.TenantRoleOwner, types.TenantMemberStatusActive).
		Order("tenant_members.joined_at ASC, tenant_members.id ASC").Find(&owners).Error; err != nil {
		return nil, 0, err
	}
	for _, owner := range owners {
		tenant := byID[owner.TenantID]
		tenant.Owners = append(tenant.Owners, owner.SystemTenantOwner)
	}
	return tenants, total, nil
}

// IncreaseStorageQuota updates the quota in SQL so concurrent additions cannot overwrite each other.
func (r *tenantRepository) IncreaseStorageQuota(
	ctx context.Context, tenantID uint64, delta int64,
) (*types.Tenant, error) {
	var tenant types.Tenant
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&types.Tenant{}).
			Where("id = ? AND storage_quota > 0 AND storage_quota <= ?", tenantID, math.MaxInt64-delta).
			Update("storage_quota", gorm.Expr("storage_quota + ?", delta))
		if result.Error != nil {
			return result.Error
		}
		if err := tx.Select("id", "name", "storage_quota").First(&tenant, tenantID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrTenantNotFound
			}
			return err
		}
		if result.RowsAffected == 0 {
			return ErrStorageQuotaNotIncreasable
		}
		return nil
	})
	return &tenant, err
}

// UpdateTenant updates tenant.
func (r *tenantRepository) UpdateTenant(ctx context.Context, tenant *types.Tenant) error {
	// Configuration updates must not restore a stale quota after an administrator increases it.
	return r.db.WithContext(ctx).Model(&types.Tenant{}).Where("id = ?", tenant.ID).
		Omit("storage_quota").Updates(tenant).Error
}

// DeleteTenant soft-deletes the tenant and every active membership row
// for that tenant in one transaction. Without the membership purge,
// /auth/me still lists the defunct tenant (name lookup fails → UI shows
// "#<id>").
func (r *tenantRepository) DeleteTenant(ctx context.Context, id uint64) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("tenant_id = ?", id).Delete(&types.TenantMember{}).Error; err != nil {
			return err
		}
		return tx.Where("id = ?", id).Delete(&types.Tenant{}).Error
	})
}

func (r *tenantRepository) AdjustStorageUsed(ctx context.Context, tenantID uint64, delta int64) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var tenant types.Tenant
		// 使用悲观锁确保并发安全
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&tenant, tenantID).Error; err != nil {
			return err
		}

		tenant.StorageUsed += delta
		// 保存更新并验证业务规则
		if tenant.StorageUsed < 0 {
			logger.Errorf(ctx, "tenant storage used is negative %d: %d", tenant.ID, tenant.StorageUsed)
			tenant.StorageUsed = 0
		}

		return tx.Model(&tenant).Update("storage_used", tenant.StorageUsed).Error
	})
}

// BulkSetStorageQuota only raises finite quotas below the requested value.
func (r *tenantRepository) BulkSetStorageQuota(ctx context.Context, quotaBytes int64) (int64, error) {
	res := r.db.WithContext(ctx).
		Model(&types.Tenant{}).
		Where("storage_quota > 0 AND storage_quota < ?", quotaBytes).
		Update("storage_quota", quotaBytes)
	if res.Error != nil {
		return 0, res.Error
	}
	return res.RowsAffected, nil
}
