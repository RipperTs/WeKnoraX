package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

type ssoTestUserRepo struct {
	interfaces.UserRepository
	users   map[string]*types.User
	created *types.User
}

func (r *ssoTestUserRepo) GetUserByUsername(_ context.Context, username string) (*types.User, error) {
	return r.users[username], nil
}

func (r *ssoTestUserRepo) GetUserByEmail(context.Context, string) (*types.User, error) {
	return nil, nil
}

func (r *ssoTestUserRepo) CreateUser(_ context.Context, user *types.User) error {
	copy := *user
	r.created = &copy
	r.users[user.Username] = &copy
	return nil
}

type ssoTestTokenRepo struct {
	interfaces.AuthTokenRepository
}

func (*ssoTestTokenRepo) CreateToken(context.Context, *types.AuthToken) error {
	return nil
}

type ssoTestTenantService struct {
	interfaces.TenantService
}

func (*ssoTestTenantService) CreateTenant(context.Context, *types.Tenant) (*types.Tenant, error) {
	return &types.Tenant{ID: 42, Name: "SSO Workspace"}, nil
}

func (*ssoTestTenantService) GetTenantByID(_ context.Context, id uint64) (*types.Tenant, error) {
	return &types.Tenant{ID: id, Name: "SSO Workspace"}, nil
}

type ssoTestSystemSettingSvc struct {
	interfaces.SystemSettingService
	enabled bool
	key     string
}

func (s *ssoTestSystemSettingSvc) GetBool(_ context.Context, key string, _ string, def bool) bool {
	s.key = key
	if key == SystemSettingSSOAutoRegisterEnabled {
		return s.enabled
	}
	return def
}

// TestLoginWithFushunSSOAutoProvisionsWorkIDUser catches a regression where
// the SSO payload is accepted but its empNo is not persisted as the local
// username, which would make later SSO logins create duplicate accounts.
func TestLoginWithFushunSSOAutoProvisionsWorkIDUser(t *testing.T) {
	secutils.SetSSRFWhitelistFromRaw("127.0.0.1,::1,localhost")
	t.Cleanup(func() { secutils.SetSSRFWhitelistFromRaw("") })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		var request struct {
			RemoteToken string `json:"remoteToken"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode SSO request: %v", err)
		}
		if request.RemoteToken != "remote-token" {
			t.Fatalf("remoteToken = %q, want remote-token", request.RemoteToken)
		}
		_, _ = w.Write([]byte(`{"code":200,"data":{"empNo":"10001","chn":"张三"}}`))
	}))
	defer server.Close()

	repo := &ssoTestUserRepo{users: map[string]*types.User{}}
	svc := &userService{
		config: &config.Config{FushunSSO: &config.FushunSSOConfig{
			AuthURL:        "https://auth-sso.fsxgt.cn/sso/auth",
			DoLoginURL:     "https://auth-sso.fsxgt.cn/sso/doLogin",
			GetUserInfoURL: server.URL,
		}},
		userRepo:      repo,
		tokenRepo:     &ssoTestTokenRepo{},
		tenantService: &ssoTestTenantService{},
	}

	response, err := svc.LoginWithFushunSSO(context.Background(), "remote-token", types.TenantProvisioningCreatePersonal)
	if err != nil {
		t.Fatalf("LoginWithFushunSSO: %v", err)
	}
	if !response.Success || response.Token == "" || response.RefreshToken == "" {
		t.Fatalf("unexpected login response: %#v", response)
	}
	if repo.created == nil {
		t.Fatal("SSO user was not created")
	}
	if repo.created.Username != "10001" {
		t.Fatalf("username = %q, want work ID", repo.created.Username)
	}
	if repo.created.Name != "张三" {
		t.Fatalf("name = %q, want SSO Chinese name", repo.created.Name)
	}
	if repo.created.Email != "10001@example.com" {
		t.Fatalf("email = %q, want default SSO email suffix", repo.created.Email)
	}
}

func TestLoginWithFushunSSORejectsNewUserWhenAutoRegisterDisabled(t *testing.T) {
	secutils.SetSSRFWhitelistFromRaw("127.0.0.1,::1,localhost")
	t.Cleanup(func() { secutils.SetSSRFWhitelistFromRaw("") })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		_, _ = w.Write([]byte(`{"code":200,"data":{"empNo":"10001","chn":"张三"}}`))
	}))
	defer server.Close()

	repo := &ssoTestUserRepo{users: map[string]*types.User{}}
	settingSvc := &ssoTestSystemSettingSvc{enabled: false}
	svc := &userService{
		config: &config.Config{FushunSSO: &config.FushunSSOConfig{
			AuthURL:        "https://auth-sso.fsxgt.cn/sso/auth",
			DoLoginURL:     "https://auth-sso.fsxgt.cn/sso/doLogin",
			GetUserInfoURL: server.URL,
		}},
		userRepo:         repo,
		tokenRepo:        &ssoTestTokenRepo{},
		tenantService:    &ssoTestTenantService{},
		systemSettingSvc: settingSvc,
	}

	response, err := svc.LoginWithFushunSSO(context.Background(), "remote-token", types.TenantProvisioningCreatePersonal)
	if err != nil {
		t.Fatalf("LoginWithFushunSSO: %v", err)
	}
	if response == nil || response.Success {
		t.Fatalf("unexpected login response: %#v", response)
	}
	if response.Message != "SSO 新用户自动注册已关闭，请联系管理员开通账号" {
		t.Fatalf("message = %q", response.Message)
	}
	if repo.created != nil {
		t.Fatalf("SSO user should not be created: %#v", repo.created)
	}
	if settingSvc.key != SystemSettingSSOAutoRegisterEnabled {
		t.Fatalf("setting key = %q, want %q", settingSvc.key, SystemSettingSSOAutoRegisterEnabled)
	}
}
