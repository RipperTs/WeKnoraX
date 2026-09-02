package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

func withOIDCSSRFWhitelist(t *testing.T, raw string) {
	t.Helper()
	t.Setenv("SSRF_WHITELIST", raw)
	secutils.ResetSSRFWhitelistForTest()
	t.Cleanup(secutils.ResetSSRFWhitelistForTest)
}

func TestOIDCDiscoveryRejectsInternalDiscoveredEndpoint(t *testing.T) {
	withOIDCSSRFWhitelist(t, "127.0.0.1")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(oidcDiscoveryDocument{
			AuthorizationEndpoint: serverURL(r, "/authorize"),
			TokenEndpoint:         "http://169.254.169.254/latest/meta-data/",
		})
	}))
	defer server.Close()

	svc := &userService{}
	cfg := &config.OIDCAuthConfig{DiscoveryURL: server.URL}
	err := svc.populateOIDCEndpoints(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "OIDC token endpoint failed SSRF validation") {
		t.Fatalf("populateOIDCEndpoints error = %v, want token SSRF validation failure", err)
	}
}

func TestOIDCTokenExchangeBlocksRedirectToInternalURL(t *testing.T) {
	withOIDCSSRFWhitelist(t, "127.0.0.1")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer server.Close()

	svc := &userService{}
	cfg := &config.OIDCAuthConfig{TokenEndpoint: server.URL, ClientID: "client-id", ClientSecret: "client-secret"}
	_, err := svc.exchangeOIDCCode(context.Background(), cfg, "code", "https://app.example/callback")
	if err == nil || !strings.Contains(err.Error(), secutils.ErrSSRFRedirectBlocked.Error()) {
		t.Fatalf("exchangeOIDCCode error = %v, want redirect SSRF block", err)
	}
}

func TestOIDCErrorsDoNotEchoSecrets(t *testing.T) {
	withOIDCSSRFWhitelist(t, "127.0.0.1")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("client-secret bearer-token"))
	}))
	defer server.Close()

	svc := &userService{}
	cfg := &config.OIDCAuthConfig{TokenEndpoint: server.URL, ClientID: "client-id", ClientSecret: "client-secret"}
	_, err := svc.exchangeOIDCCode(context.Background(), cfg, "code", "https://app.example/callback")
	if err == nil {
		t.Fatalf("exchangeOIDCCode returned nil error")
	}
	if strings.Contains(err.Error(), "client-secret") {
		t.Fatalf("exchangeOIDCCode error leaked secret: %v", err)
	}

	_, err = svc.fetchOIDCUserInfo(context.Background(), server.URL, "bearer-token")
	if err == nil {
		t.Fatalf("fetchOIDCUserInfo returned nil error")
	}
	if strings.Contains(err.Error(), "bearer-token") {
		t.Fatalf("fetchOIDCUserInfo error leaked bearer token: %v", err)
	}
}

func TestLoginWithOIDCRejectsNewUserWhenAutoRegisterDisabled(t *testing.T) {
	withOIDCSSRFWhitelist(t, "127.0.0.1")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if r.Method != http.MethodPost {
				t.Fatalf("token method = %s, want POST", r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"access_token": "access-token",
				"token_type":   "Bearer",
			})
		case "/userinfo":
			if r.Method != http.MethodGet {
				t.Fatalf("userinfo method = %s, want GET", r.Method)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
				t.Fatalf("Authorization = %q, want bearer token", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"name":  "OIDC User",
				"email": "oidc-user@example.com",
			})
		default:
			t.Fatalf("unexpected OIDC path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	repo := &ssoTestUserRepo{users: map[string]*types.User{}}
	settingSvc := &ssoTestSystemSettingSvc{enabled: false}
	svc := &userService{
		config: &config.Config{OIDCAuth: &config.OIDCAuthConfig{
			Enable:                true,
			AuthorizationEndpoint: server.URL + "/authorize",
			TokenEndpoint:         server.URL + "/token",
			UserInfoEndpoint:      server.URL + "/userinfo",
			ClientID:              "client-id",
			ClientSecret:          "client-secret",
			UserInfoMapping:       &config.OIDCUserInfoMapping{Username: "name", Email: "email"},
		}},
		userRepo:         repo,
		systemSettingSvc: settingSvc,
	}

	response, err := svc.LoginWithOIDC(
		context.Background(),
		"code",
		"https://app.example.com/auth/oidc/callback",
		types.TenantProvisioningCreatePersonal,
	)
	if err != nil {
		t.Fatalf("LoginWithOIDC: %v", err)
	}
	if response == nil || response.Success {
		t.Fatalf("unexpected login response: %#v", response)
	}
	if response.Message != ssoAutoRegisterDisabledPrompt {
		t.Fatalf("message = %q", response.Message)
	}
	if repo.created != nil {
		t.Fatalf("OIDC user should not be created: %#v", repo.created)
	}
	if settingSvc.key != SystemSettingSSOAutoRegisterEnabled {
		t.Fatalf("setting key = %q, want %q", settingSvc.key, SystemSettingSSOAutoRegisterEnabled)
	}
}

func serverURL(r *http.Request, path string) string {
	return "http://" + r.Host + path
}
