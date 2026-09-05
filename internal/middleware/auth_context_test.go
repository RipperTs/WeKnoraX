package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
)

// TestApplyAuthSessionSetsBothSurfaces locks the core invariant of the
// helper: every attached value must be readable from BOTH c.Keys (c.Get)
// and the request context (types.*FromContext). A key present on only one
// surface is the class of bug the helper exists to prevent.
func TestApplyAuthSessionSetsBothSurfaces(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/knowledge-bases", nil)

	user := &types.User{ID: "u1", IsSystemAdmin: true}
	tenant := &types.Tenant{ID: 7}
	principal := types.Principal{Type: types.PrincipalWebUser, ID: "u1"}
	scope := &types.TenantAPIKeyScope{KeyID: 3}
	applyAuthSession(c, authSession{
		User:        user,
		Principal:   principal,
		TenantID:    7,
		Tenant:      tenant,
		Role:        types.TenantRoleAdmin,
		SystemAdmin: true,
		APIKeyScope: scope,
		Extra:       map[types.ContextKey]any{types.EmbedChannelContextKey: &types.EmbedChannel{ID: "ch"}},
	})

	ctx := c.Request.Context()
	if got, ok := types.TenantIDFromContext(ctx); !ok || got != 7 {
		t.Fatalf("ctx tenant id = %d, ok=%v", got, ok)
	}
	if got, ok := c.Get(types.TenantIDContextKey.String()); !ok || got.(uint64) != 7 {
		t.Fatalf("keys tenant id = %v, ok=%v", got, ok)
	}
	if got, ok := types.TenantInfoFromContext(ctx); !ok || got.ID != 7 {
		t.Fatalf("ctx tenant info = %#v, ok=%v", got, ok)
	}
	if got, ok := types.UserIDFromContext(ctx); !ok || got != "u1" {
		t.Fatalf("ctx user id = %q, ok=%v", got, ok)
	}
	if got, ok := c.Get(types.UserContextKey.String()); !ok || got.(*types.User).ID != "u1" {
		t.Fatalf("keys user = %#v, ok=%v", got, ok)
	}
	if got, ok := types.PrincipalFromContext(ctx); !ok || got != principal {
		t.Fatalf("ctx principal = %#v, ok=%v", got, ok)
	}
	if got := types.TenantRoleFromContext(ctx); got != types.TenantRoleAdmin {
		t.Fatalf("ctx role = %q", got)
	}
	if got, ok := c.Get(types.TenantRoleContextKey.String()); !ok || got.(types.TenantRole) != types.TenantRoleAdmin {
		t.Fatalf("keys role = %v, ok=%v", got, ok)
	}
	if !types.IsSystemAdminFromContext(ctx) {
		t.Fatal("ctx system admin flag lost")
	}
	if got, ok := types.TenantAPIKeyScopeFromContext(ctx); !ok || got.KeyID != 3 {
		t.Fatalf("ctx api key scope = %#v, ok=%v", got, ok)
	}
	if ch, ok := EmbedChannelFromContext(ctx); !ok || ch.ID != "ch" {
		t.Fatalf("ctx embed channel = %#v, ok=%v", ch, ok)
	}
	if got, ok := c.Get(types.EmbedChannelContextKey.String()); !ok || got.(*types.EmbedChannel).ID != "ch" {
		t.Fatalf("keys embed channel = %v, ok=%v", got, ok)
	}
}

// TestApplyAuthSessionTenantless checks identity-only JWT authentication and
// system-admin authorization without granting any workspace identity or role.
func TestApplyAuthSessionTenantless(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name         string
		method       string
		path         string
		admin        bool
		homeTenant   uint64
		tenantHeader string
		status       int
	}{
		{
			name: "user profile", method: http.MethodGet, path: "/api/v1/auth/me",
			status: http.StatusOK,
		},
		{
			name: "admin profile", method: http.MethodGet, path: "/api/v1/auth/me", admin: true,
			status: http.StatusOK,
		},
		{
			name: "workspace list", method: http.MethodGet, path: "/api/v1/system/admin/tenants", admin: true,
			status: http.StatusOK,
		},
		{
			name: "quota increase", method: http.MethodPost,
			path: "/api/v1/system/admin/tenants/42/storage-quota/increase", admin: true,
			status: http.StatusOK,
		},
		{
			name: "apply default quota", method: http.MethodPost,
			path: "/api/v1/system/admin/tenants/apply-default-storage-quota", admin: true,
			status: http.StatusOK,
		},
		{
			name: "selected workspace does not constrain administration", method: http.MethodGet,
			path: "/api/v1/system/admin/settings", admin: true, homeTenant: 7, tenantHeader: "42",
			status: http.StatusOK,
		},
		{
			name: "non-admin denied", method: http.MethodGet, path: "/api/v1/system/admin/tenants",
			status: http.StatusForbidden,
		},
		{
			name: "workspace API still requires tenant", method: http.MethodGet,
			path: "/api/v1/knowledge-bases", admin: true, status: http.StatusConflict,
		},
		{
			name: "similar prefix is not a system-admin API", method: http.MethodGet,
			path: "/api/v1/system/admin-foo", admin: true, status: http.StatusConflict,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(tc.method, tc.path, nil)
			if tc.tenantHeader != "" {
				c.Request.Header.Set("X-Tenant-ID", tc.tenantHeader)
			}
			user := &types.User{ID: "u2", TenantID: tc.homeTenant, IsSystemAdmin: tc.admin}
			cfg := cfgWithRBAC(true)
			if authenticateJWTUser(c, &fakeTenantService{}, newFakeMemberService(), cfg, user, tc.homeTenant) &&
				isSystemAdminAPI(tc.path) {
				RequireSystemAdmin(cfg)(c)
			}
			if got := c.Writer.Status(); got != tc.status {
				t.Fatalf("status = %d, want %d", got, tc.status)
			}
			if tc.status != http.StatusOK {
				if !c.IsAborted() {
					t.Fatal("denied request must be aborted")
				}
				return
			}
			if c.IsAborted() {
				t.Fatal("authorized request must not be aborted")
			}
			ctx := c.Request.Context()
			if _, ok := types.TenantIDFromContext(ctx); ok {
				t.Fatal("tenantless session must not carry a tenant id")
			}
			if _, ok := c.Get(types.TenantRoleContextKey.String()); ok {
				t.Fatal("tenantless session must not carry a role key")
			}
			if got, ok := types.UserIDFromContext(ctx); !ok || got != "u2" {
				t.Fatalf("ctx user id = %q, ok=%v", got, ok)
			}
			if got := types.IsSystemAdminFromContext(ctx); got != tc.admin {
				t.Fatalf("system admin = %v, want %v", got, tc.admin)
			}
		})
	}
}

func TestBearerToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		header string
		want   string
		ok     bool
	}{
		{"", "", false},
		{"Basic abc", "", false},
		{"Bearer", "", false},
		{"Bearer abc", "abc", true},
	}
	for _, tc := range cases {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		if tc.header != "" {
			c.Request.Header.Set("Authorization", tc.header)
		}
		got, ok := bearerToken(c)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("bearerToken(%q) = (%q, %v), want (%q, %v)", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}
