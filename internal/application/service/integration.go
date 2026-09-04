package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	apprepo "github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
)

const (
	integrationCodeLifetime        = 5 * time.Minute
	integrationCallbackTestTimeout = 5 * time.Second
	integrationCallbackConcurrency = 5
)

var integrationCallbackTestHTTPClient = &http.Client{
	Timeout: integrationCallbackTestTimeout,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

var (
	// ErrIntegrationInvalidApplication indicates invalid client metadata or credentials.
	ErrIntegrationInvalidApplication = errors.New("invalid integration application")
	// ErrIntegrationApplicationDisabled indicates a globally disabled client.
	ErrIntegrationApplicationDisabled = errors.New("integration application is disabled")
	// ErrIntegrationPolicyDisabled indicates a client disabled in the current workspace.
	ErrIntegrationPolicyDisabled = errors.New("integration application is disabled for this tenant")
	// ErrIntegrationInvalidRedirectURI indicates a callback that was not registered exactly.
	ErrIntegrationInvalidRedirectURI = errors.New("invalid integration redirect URI")
	// ErrIntegrationInvalidScope indicates an unsupported or disallowed permission request.
	ErrIntegrationInvalidScope = errors.New("invalid integration scope")
	// ErrIntegrationInvalidState indicates a missing or oversized browser state value.
	ErrIntegrationInvalidState = errors.New("integration state is required")
	// ErrIntegrationInvalidPKCE indicates invalid S256 PKCE parameters.
	ErrIntegrationInvalidPKCE = errors.New("invalid integration PKCE parameters")
	// ErrIntegrationAccessDenied indicates that the requested resource set is not allowed.
	ErrIntegrationAccessDenied = errors.New("integration access denied")
	// ErrIntegrationConsentRequired indicates that an existing connection cannot be reused silently.
	ErrIntegrationConsentRequired = errors.New("integration consent is required")
)

// IntegrationApplicationInput defines the editable configuration for a registered client.
type IntegrationApplicationInput struct {
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	RedirectURIs  []string `json:"redirect_uris"`
	AllowedScopes []string `json:"allowed_scopes"`
	Enabled       bool     `json:"enabled"`
}

// IntegrationApplicationSecretResult returns a client secret exactly once after creation or rotation.
type IntegrationApplicationSecretResult struct {
	Application  *types.IntegrationApplication `json:"application"`
	ClientSecret string                        `json:"client_secret"`
}

// IntegrationCallbackTestResult describes whether a registered callback URL is reachable.
type IntegrationCallbackTestResult struct {
	RedirectURI string `json:"redirect_uri"`
	Reachable   bool   `json:"reachable"`
	StatusCode  int    `json:"status_code,omitempty"`
	Error       string `json:"error,omitempty"`
}

// TenantIntegrationPolicyInput narrows an application's permissions in one workspace.
type TenantIntegrationPolicyInput struct {
	Enabled       bool     `json:"enabled"`
	AllowedScopes []string `json:"allowed_scopes"`
}

// TenantIntegrationApplicationView combines a global application with its optional workspace policy.
type TenantIntegrationApplicationView struct {
	Application *types.IntegrationApplication  `json:"application"`
	Policy      *types.TenantIntegrationPolicy `json:"policy,omitempty"`
}

// IntegrationAuthorizationParameters contains the validated browser authorization request.
type IntegrationAuthorizationParameters struct {
	ClientID            string   `json:"client_id"`
	RedirectURI         string   `json:"redirect_uri"`
	State               string   `json:"state"`
	Scopes              []string `json:"scopes"`
	CodeChallenge       string   `json:"code_challenge"`
	CodeChallengeMethod string   `json:"code_challenge_method"`
	Prompt              string   `json:"prompt,omitempty"`
}

// IntegrationKnowledgeBaseView exposes only metadata needed by integration clients.
type IntegrationKnowledgeBaseView struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Type           string `json:"type"`
	Description    string `json:"description,omitempty"`
	TenantID       uint64 `json:"tenant_id"`
	AccessTenantID uint64 `json:"access_tenant_id"`
}

// IntegrationTenantView exposes the workspace metadata needed by consent and launch pages.
type IntegrationTenantView struct {
	ID          uint64           `json:"id"`
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Role        types.TenantRole `json:"role"`
}

// IntegrationAuthorizationView describes the consent screen and any reusable connection.
type IntegrationAuthorizationView struct {
	Application       *types.IntegrationApplication `json:"application"`
	Scopes            types.StringArray             `json:"scopes"`
	Tenants           []IntegrationTenantView       `json:"tenants"`
	SelectedTenantIDs []uint64                      `json:"selected_tenant_ids"`
	ConnectionID      string                        `json:"connection_id,omitempty"`
	RequiresConsent   bool                          `json:"requires_consent"`
	grantedTenantIDs  []uint64
}

// IntegrationAuthorizationDecision records the user's approval or denial.
type IntegrationAuthorizationDecision struct {
	Parameters    IntegrationAuthorizationParameters `json:"parameters"`
	Approved      bool                               `json:"approved"`
	ReuseExisting bool                               `json:"reuse_existing"`
	TenantIDs     []uint64                           `json:"tenant_ids"`
}

// IntegrationAuthorizationResult contains the exact callback destination after a decision.
type IntegrationAuthorizationResult struct {
	RedirectURI  string `json:"redirect_uri"`
	ConnectionID string `json:"connection_id,omitempty"`
}

// IntegrationTokenExchangeRequest exchanges a confidential-client authorization code.
type IntegrationTokenExchangeRequest struct {
	GrantType    string `json:"grant_type" form:"grant_type"`
	ClientID     string `json:"client_id" form:"client_id"`
	ClientSecret string `json:"client_secret" form:"client_secret"`
	Code         string `json:"code" form:"code"`
	RedirectURI  string `json:"redirect_uri" form:"redirect_uri"`
	CodeVerifier string `json:"code_verifier" form:"code_verifier"`
}

// IntegrationTokenExchangeResult returns the per-user Agent credential and browser launch path.
type IntegrationTokenExchangeResult struct {
	AccessToken  string            `json:"access_token"`
	TokenType    string            `json:"token_type"`
	ConnectionID string            `json:"connection_id"`
	LaunchPath   string            `json:"launch_path"`
	Scopes       types.StringArray `json:"scopes"`
}

// IntegrationConnectionView exposes a connection's current effective permissions.
type IntegrationConnectionView struct {
	Connection                *types.IntegrationConnection   `json:"connection"`
	Application               *types.IntegrationApplication  `json:"application"`
	Tenants                   []IntegrationTenantView        `json:"tenants"`
	KnowledgeBases            []IntegrationKnowledgeBaseView `json:"knowledge_bases"`
	EffectiveKnowledgeBaseIDs types.StringArray              `json:"effective_knowledge_base_ids"`
	EffectiveScopes           types.StringArray              `json:"effective_scopes"`
	Available                 bool                           `json:"available"`
	UnavailableReason         string                         `json:"unavailable_reason,omitempty"`
}

// IntegrationService manages client registration, consent, credentials and effective access.
type IntegrationService struct {
	repo          interfaces.IntegrationRepository
	kbRepo        interfaces.KnowledgeBaseRepository
	kbShareRepo   interfaces.KBShareRepository
	tenantService interfaces.TenantService
	memberService interfaces.TenantMemberService
	lastUsedTouch sync.Map
}

// NewIntegrationService creates the third-party integration service.
func NewIntegrationService(
	repo interfaces.IntegrationRepository,
	kbRepo interfaces.KnowledgeBaseRepository,
	kbShareRepo interfaces.KBShareRepository,
	tenantService interfaces.TenantService,
	memberService interfaces.TenantMemberService,
) *IntegrationService {
	return &IntegrationService{
		repo: repo, kbRepo: kbRepo, kbShareRepo: kbShareRepo,
		tenantService: tenantService, memberService: memberService,
	}
}

// CreateApplication registers a client and returns its plaintext secret once.
func (s *IntegrationService) CreateApplication(
	ctx context.Context, createdBy string, input IntegrationApplicationInput,
) (*IntegrationApplicationSecretResult, error) {
	name, redirectURIs, scopes, err := validateApplicationInput(input)
	if err != nil {
		return nil, err
	}
	clientID, err := generateIntegrationSecret("wkapp_", 18)
	if err != nil {
		return nil, err
	}
	clientSecret, err := generateIntegrationSecret("wks_", 32)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	app := &types.IntegrationApplication{
		ID:               uuid.NewString(),
		ClientID:         clientID,
		ClientSecretHash: hashIntegrationSecret(clientSecret),
		Name:             name,
		Description:      strings.TrimSpace(input.Description),
		RedirectURIs:     redirectURIs,
		AllowedScopes:    scopes,
		Enabled:          input.Enabled,
		CreatedBy:        createdBy,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := s.repo.CreateApplication(ctx, app); err != nil {
		return nil, err
	}
	return &IntegrationApplicationSecretResult{Application: app, ClientSecret: clientSecret}, nil
}

// UpdateApplication replaces a registered client's editable configuration.
func (s *IntegrationService) UpdateApplication(
	ctx context.Context, id string, input IntegrationApplicationInput,
) (*types.IntegrationApplication, error) {
	name, redirectURIs, scopes, err := validateApplicationInput(input)
	if err != nil {
		return nil, err
	}
	app, err := s.repo.GetApplicationByID(ctx, id)
	if err != nil {
		return nil, err
	}
	app.Name = name
	app.Description = strings.TrimSpace(input.Description)
	app.RedirectURIs = redirectURIs
	app.AllowedScopes = scopes
	app.Enabled = input.Enabled
	app.UpdatedAt = time.Now().UTC()
	if err := s.repo.UpdateApplication(ctx, app); err != nil {
		return nil, err
	}
	return app, nil
}

// DeleteApplication removes a registered client and its dependent authorization data.
func (s *IntegrationService) DeleteApplication(ctx context.Context, id string) error {
	return s.repo.DeleteApplication(ctx, id)
}

// RotateApplicationSecret invalidates the old client secret and returns a new one once.
func (s *IntegrationService) RotateApplicationSecret(
	ctx context.Context, id string,
) (*IntegrationApplicationSecretResult, error) {
	app, err := s.repo.GetApplicationByID(ctx, id)
	if err != nil {
		return nil, err
	}
	clientSecret, err := generateIntegrationSecret("wks_", 32)
	if err != nil {
		return nil, err
	}
	app.ClientSecretHash = hashIntegrationSecret(clientSecret)
	app.UpdatedAt = time.Now().UTC()
	if err := s.repo.UpdateApplicationSecret(ctx, app.ID, app.ClientSecretHash, app.UpdatedAt); err != nil {
		return nil, err
	}
	return &IntegrationApplicationSecretResult{Application: app, ClientSecret: clientSecret}, nil
}

// TestApplicationCallbacks checks whether the registered callback URLs accept a network connection.
func (s *IntegrationService) TestApplicationCallbacks(
	ctx context.Context, id string,
) ([]IntegrationCallbackTestResult, error) {
	app, err := s.repo.GetApplicationByID(ctx, id)
	if err != nil {
		return nil, err
	}
	results := make([]IntegrationCallbackTestResult, len(app.RedirectURIs))
	var group errgroup.Group
	group.SetLimit(integrationCallbackConcurrency)
	for index, redirectURI := range app.RedirectURIs {
		group.Go(func() error {
			results[index] = testIntegrationCallback(ctx, redirectURI)
			return nil
		})
	}
	_ = group.Wait()
	return results, nil
}

func testIntegrationCallback(ctx context.Context, redirectURI string) IntegrationCallbackTestResult {
	result := IntegrationCallbackTestResult{RedirectURI: redirectURI}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, redirectURI, nil)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	req.Header.Set("User-Agent", "WeKnora-Integration-Connectivity-Test/1.0")
	resp, err := integrationCallbackTestHTTPClient.Do(req)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Reachable = true
	result.StatusCode = resp.StatusCode
	_ = resp.Body.Close()
	return result
}

// ListApplications returns all registered clients for system administration.
func (s *IntegrationService) ListApplications(ctx context.Context) ([]*types.IntegrationApplication, error) {
	return s.repo.ListApplications(ctx)
}

// ListTenantApplications returns registered clients with policies for one workspace.
func (s *IntegrationService) ListTenantApplications(
	ctx context.Context, tenantID uint64,
) ([]*TenantIntegrationApplicationView, error) {
	apps, err := s.repo.ListApplications(ctx)
	if err != nil {
		return nil, err
	}
	policies, err := s.repo.ListTenantPolicies(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	policyByApp := make(map[string]*types.TenantIntegrationPolicy, len(policies))
	for _, policy := range policies {
		policyByApp[policy.ApplicationID] = policy
	}
	views := make([]*TenantIntegrationApplicationView, 0, len(apps))
	for _, app := range apps {
		views = append(views, &TenantIntegrationApplicationView{
			Application: app,
			Policy:      policyByApp[app.ID],
		})
	}
	return views, nil
}

// UpsertTenantPolicy creates or replaces one workspace's restrictions for a client.
func (s *IntegrationService) UpsertTenantPolicy(
	ctx context.Context,
	tenantID uint64,
	applicationID string,
	input TenantIntegrationPolicyInput,
) (*types.TenantIntegrationPolicy, error) {
	app, err := s.repo.GetApplicationByID(ctx, applicationID)
	if err != nil {
		return nil, err
	}
	scopes, err := validateScopes(input.AllowedScopes)
	if err != nil {
		return nil, err
	}
	if !scopesSubset(scopes, app.AllowedScopes) {
		return nil, ErrIntegrationInvalidScope
	}
	now := time.Now().UTC()
	policy := &types.TenantIntegrationPolicy{
		ID:            uuid.NewString(),
		ApplicationID: applicationID,
		TenantID:      tenantID,
		Enabled:       input.Enabled,
		AllowedScopes: scopes,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.repo.UpsertTenantPolicy(ctx, policy); err != nil {
		return nil, err
	}
	return s.repo.GetTenantPolicy(ctx, applicationID, tenantID)
}

// GetAuthorizationView validates an authorization request and builds its consent state.
func (s *IntegrationService) GetAuthorizationView(
	ctx context.Context,
	userID string,
	params IntegrationAuthorizationParameters,
) (*IntegrationAuthorizationView, error) {
	app, scopes, err := s.validateAuthorizationParameters(ctx, params)
	if err != nil {
		return nil, err
	}
	tenants, err := s.listAuthorizableTenants(ctx, userID, app, scopes)
	if err != nil {
		return nil, err
	}
	connection, err := s.repo.FindConnection(ctx, app.ID, userID)
	if err != nil {
		return nil, err
	}
	view := &IntegrationAuthorizationView{
		Application:     app,
		Scopes:          scopes,
		Tenants:         tenants,
		RequiresConsent: true,
	}
	if connection == nil || connection.Status != types.IntegrationConnectionActive {
		return view, nil
	}
	view.ConnectionID = connection.ID
	grants, err := s.repo.ListConnectionTenantIDs(ctx, connection.ID)
	if err != nil {
		return nil, err
	}
	view.grantedTenantIDs = grants
	accessible := integrationTenantViewIDSet(tenants)
	for _, id := range grants {
		if accessible[id] {
			view.SelectedTenantIDs = append(view.SelectedTenantIDs, id)
		}
	}
	view.RequiresConsent = params.Prompt == "consent" ||
		!scopesSubset(scopes, connection.Scopes) || len(view.SelectedTenantIDs) == 0
	return view, nil
}

func (s *IntegrationService) listAuthorizableTenants(
	ctx context.Context,
	userID string,
	app *types.IntegrationApplication,
	requestedScopes []string,
) ([]IntegrationTenantView, error) {
	memberships, err := s.memberService.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	roleByTenant := make(map[uint64]types.TenantRole, len(memberships))
	tenantIDs := make([]uint64, 0, len(memberships))
	for _, membership := range memberships {
		if membership == nil || membership.Status != types.TenantMemberStatusActive || membership.TenantID == 0 {
			continue
		}
		roleByTenant[membership.TenantID] = membership.Role
		tenantIDs = append(tenantIDs, membership.TenantID)
	}
	policies, err := s.repo.ListApplicationPolicies(ctx, app.ID, tenantIDs)
	if err != nil {
		return nil, err
	}
	policyByTenant := make(map[uint64]*types.TenantIntegrationPolicy, len(policies))
	for _, policy := range policies {
		policyByTenant[policy.TenantID] = policy
	}
	tenantByID, err := s.tenantService.GetTenantsByIDs(ctx, tenantIDs)
	if err != nil {
		return nil, err
	}
	views := make([]IntegrationTenantView, 0, len(tenantIDs))
	for tenantID, role := range roleByTenant {
		policy := policyByTenant[tenantID]
		if policy != nil && (!policy.Enabled || !scopesSubset(requestedScopes, policy.AllowedScopes)) {
			continue
		}
		tenant := tenantByID[tenantID]
		if tenant == nil {
			continue
		}
		views = append(views, IntegrationTenantView{
			ID: tenant.ID, Name: tenant.Name, Description: tenant.Description, Role: role,
		})
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].Name == views[j].Name {
			return views[i].ID < views[j].ID
		}
		return views[i].Name < views[j].Name
	})
	return views, nil
}

// Authorize applies a consent decision and redirects with a single-use code or denial.
func (s *IntegrationService) Authorize(
	ctx context.Context,
	userID string,
	decision IntegrationAuthorizationDecision,
) (*IntegrationAuthorizationResult, error) {
	view, err := s.GetAuthorizationView(ctx, userID, decision.Parameters)
	if err != nil {
		return nil, err
	}
	if !decision.Approved {
		redirectURI, redirectErr := buildIntegrationRedirect(
			decision.Parameters.RedirectURI,
			map[string]string{"error": "access_denied", "state": decision.Parameters.State},
		)
		if redirectErr != nil {
			return nil, redirectErr
		}
		return &IntegrationAuthorizationResult{RedirectURI: redirectURI}, nil
	}
	selectedIDs := normalizeIntegrationTenantIDs(decision.TenantIDs)
	if decision.ReuseExisting {
		if view.RequiresConsent {
			return nil, ErrIntegrationConsentRequired
		}
		selectedIDs = view.grantedTenantIDs
	}
	if len(selectedIDs) == 0 {
		return nil, ErrIntegrationAccessDenied
	}
	if !decision.ReuseExisting {
		allowedTenants := integrationTenantViewIDSet(view.Tenants)
		for _, tenantID := range selectedIDs {
			if !allowedTenants[tenantID] {
				return nil, ErrIntegrationAccessDenied
			}
		}
	}
	codeValue, err := generateIntegrationSecret("wkac_", 32)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	connection := &types.IntegrationConnection{
		ID:            uuid.NewString(),
		ApplicationID: view.Application.ID,
		UserID:        userID,
		Scopes:        view.Scopes,
		Status:        types.IntegrationConnectionActive,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	code := &types.IntegrationAuthorizationCode{
		CodeHash:      hashIntegrationSecret(codeValue),
		ApplicationID: view.Application.ID,
		RedirectURI:   decision.Parameters.RedirectURI,
		Scopes:        view.Scopes,
		CodeChallenge: decision.Parameters.CodeChallenge,
		ExpiresAt:     now.Add(integrationCodeLifetime),
		CreatedAt:     now,
	}
	saved, err := s.repo.SaveAuthorization(ctx, connection, selectedIDs, code)
	if err != nil {
		return nil, err
	}
	redirectURI, err := buildIntegrationRedirect(decision.Parameters.RedirectURI, map[string]string{
		"code": codeValue, "state": decision.Parameters.State,
	})
	if err != nil {
		return nil, err
	}
	return &IntegrationAuthorizationResult{RedirectURI: redirectURI, ConnectionID: saved.ID}, nil
}

// ExchangeAuthorizationCode consumes a code and returns a rotated per-user credential.
func (s *IntegrationService) ExchangeAuthorizationCode(
	ctx context.Context, req IntegrationTokenExchangeRequest,
) (*IntegrationTokenExchangeResult, error) {
	if req.GrantType != "authorization_code" {
		return nil, apprepo.ErrIntegrationAuthorizationCode
	}
	app, err := s.repo.GetApplicationByClientID(ctx, strings.TrimSpace(req.ClientID))
	if errors.Is(err, apprepo.ErrIntegrationApplicationNotFound) {
		return nil, ErrIntegrationInvalidApplication
	}
	if err != nil {
		return nil, err
	}
	if !constantTimeSecretMatch(app.ClientSecretHash, req.ClientSecret) {
		return nil, ErrIntegrationInvalidApplication
	}
	if !app.Enabled {
		return nil, ErrIntegrationApplicationDisabled
	}
	codeHash := hashIntegrationSecret(strings.TrimSpace(req.Code))
	code, err := s.repo.GetAuthorizationCode(ctx, codeHash)
	if err != nil {
		return nil, err
	}
	if !stringInSlice(code.RedirectURI, app.RedirectURIs) || !scopesSubset(code.Scopes, app.AllowedScopes) {
		return nil, apprepo.ErrIntegrationAuthorizationCode
	}
	now := time.Now().UTC()
	if code.ApplicationID != app.ID || code.RedirectURI != req.RedirectURI ||
		code.ConsumedAt != nil || !code.ExpiresAt.After(now) {
		return nil, apprepo.ErrIntegrationAuthorizationCode
	}
	if !validCodeVerifier(req.CodeVerifier) ||
		!constantTimeValueMatch(code.CodeChallenge, pkceChallenge(req.CodeVerifier)) {
		return nil, ErrIntegrationInvalidPKCE
	}
	connection, err := s.repo.GetConnectionByID(ctx, code.ConnectionID)
	if errors.Is(err, apprepo.ErrIntegrationConnectionNotFound) {
		return nil, apprepo.ErrIntegrationAuthorizationCode
	}
	if err != nil {
		return nil, err
	}
	if connection.Status != types.IntegrationConnectionActive {
		return nil, apprepo.ErrIntegrationAuthorizationCode
	}
	session := &types.IntegrationCredentialSession{
		Connection: connection, Application: app,
		Scopes: intersectIntegrationScopes(connection.Scopes, app.AllowedScopes),
	}
	access, err := s.ResolveCredentialAccess(ctx, session)
	if err != nil {
		return nil, err
	}
	if !types.IntegrationScopesContain(access.Scopes, types.IntegrationScopeKnowledgeRead) {
		return nil, ErrIntegrationPolicyDisabled
	}
	token, err := generateIntegrationSecret("wkic_", 32)
	if err != nil {
		return nil, err
	}
	credential := &types.IntegrationCredential{
		ID:           uuid.NewString(),
		ConnectionID: connection.ID,
		TokenHash:    hashIntegrationSecret(token),
		TokenPrefix:  token[:min(len(token), 16)],
		CreatedAt:    now,
	}
	if err := s.repo.ConsumeAuthorizationCodeAndCreateCredential(ctx, codeHash, now, credential); err != nil {
		return nil, err
	}
	return &IntegrationTokenExchangeResult{
		AccessToken:  token,
		TokenType:    "API-Key",
		ConnectionID: connection.ID,
		LaunchPath:   fmt.Sprintf("/integrations/launch/%s", connection.ID),
		Scopes:       access.Scopes,
	}, nil
}

// AuthenticateCredential validates a wkic_ credential and resolves its live scope inputs.
func (s *IntegrationService) AuthenticateCredential(
	ctx context.Context, token string,
) (*types.IntegrationCredentialSession, error) {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, "wkic_") {
		return nil, types.ErrIntegrationInvalidCredential
	}
	credential, err := s.repo.GetCredentialByHash(ctx, hashIntegrationSecret(token))
	if err != nil {
		if errors.Is(err, apprepo.ErrIntegrationCredentialNotFound) {
			return nil, types.ErrIntegrationInvalidCredential
		}
		return nil, err
	}
	connection, err := s.repo.GetConnectionByID(ctx, credential.ConnectionID)
	if err != nil {
		if errors.Is(err, apprepo.ErrIntegrationConnectionNotFound) {
			return nil, types.ErrIntegrationInvalidCredential
		}
		return nil, err
	}
	if connection.Status != types.IntegrationConnectionActive {
		return nil, types.ErrIntegrationInvalidCredential
	}
	app, err := s.repo.GetApplicationByID(ctx, connection.ApplicationID)
	if err != nil {
		if errors.Is(err, apprepo.ErrIntegrationApplicationNotFound) {
			return nil, types.ErrIntegrationInvalidCredential
		}
		return nil, err
	}
	if !app.Enabled {
		return nil, types.ErrIntegrationInvalidCredential
	}
	scopes := intersectIntegrationScopes(connection.Scopes, app.AllowedScopes)
	if !types.IntegrationScopesContain(scopes, types.IntegrationScopeKnowledgeRead) {
		return nil, types.ErrIntegrationInvalidCredential
	}
	s.touchCredentialLastUsed(credential.ID, connection.ID)
	return &types.IntegrationCredentialSession{
		Credential: credential, Connection: connection, Application: app, Scopes: scopes,
	}, nil
}

// ResolveCredentialAccess resolves the selected workspaces against live
// membership, workspace policy and knowledge-base visibility.
func (s *IntegrationService) ResolveCredentialAccess(
	ctx context.Context,
	session *types.IntegrationCredentialSession,
) (*types.IntegrationCredentialAccess, error) {
	access := &types.IntegrationCredentialAccess{
		TenantRoles:                  make(map[uint64]types.TenantRole),
		KnowledgeBases:               make([]*types.KnowledgeBase, 0),
		KnowledgeBaseIDs:             make(types.StringArray, 0),
		KnowledgeBaseTenantIDs:       make(map[string]uint64),
		KnowledgeBaseAccessTenantIDs: make(map[string]uint64),
	}
	grantedTenantIDs, err := s.repo.ListConnectionTenantIDs(ctx, session.Connection.ID)
	if err != nil {
		return nil, err
	}
	memberships, err := s.memberService.ListByUser(ctx, session.Connection.UserID)
	if err != nil {
		return nil, err
	}
	roleByTenant := make(map[uint64]types.TenantRole, len(memberships))
	for _, membership := range memberships {
		if membership != nil && membership.Status == types.TenantMemberStatusActive {
			roleByTenant[membership.TenantID] = membership.Role
		}
	}
	policies, err := s.repo.ListApplicationPolicies(ctx, session.Application.ID, grantedTenantIDs)
	if err != nil {
		return nil, err
	}
	policyByTenant := make(map[uint64]*types.TenantIntegrationPolicy, len(policies))
	for _, policy := range policies {
		policyByTenant[policy.TenantID] = policy
	}
	baseScopes := intersectIntegrationScopes(session.Scopes, session.Application.AllowedScopes)
	byID := make(map[string]*types.KnowledgeBase)
	for _, tenantID := range grantedTenantIDs {
		role, member := roleByTenant[tenantID]
		if !member {
			continue
		}
		policy := policyByTenant[tenantID]
		if policy == nil {
			policy = &types.TenantIntegrationPolicy{
				ApplicationID: session.Application.ID,
				TenantID:      tenantID,
				Enabled:       true,
				AllowedScopes: session.Application.AllowedScopes,
			}
		}
		tenantScopes := intersectIntegrationScopes(baseScopes, policy.AllowedScopes)
		if !policy.Enabled || !types.IntegrationScopesContain(tenantScopes, types.IntegrationScopeKnowledgeRead) {
			continue
		}
		if access.TenantID == 0 {
			access.TenantID = tenantID
			access.Scopes = tenantScopes
		} else {
			access.Scopes = intersectIntegrationScopes(access.Scopes, tenantScopes)
		}
		access.TenantRoles[tenantID] = role
		knowledgeBases, listErr := s.listAccessibleKnowledgeBases(ctx, tenantID)
		if listErr != nil {
			return nil, listErr
		}
		for _, kb := range knowledgeBases {
			if kb != nil {
				byID[kb.ID] = kb
				if access.KnowledgeBaseAccessTenantIDs[kb.ID] == 0 {
					access.KnowledgeBaseAccessTenantIDs[kb.ID] = tenantID
				}
			}
		}
	}
	access.KnowledgeBases = make([]*types.KnowledgeBase, 0, len(byID))
	for _, kb := range byID {
		access.KnowledgeBases = append(access.KnowledgeBases, kb)
	}
	sort.Slice(access.KnowledgeBases, func(i, j int) bool {
		if access.KnowledgeBases[i].Name == access.KnowledgeBases[j].Name {
			return access.KnowledgeBases[i].ID < access.KnowledgeBases[j].ID
		}
		return access.KnowledgeBases[i].Name < access.KnowledgeBases[j].Name
	})
	for _, kb := range access.KnowledgeBases {
		access.KnowledgeBaseIDs = append(access.KnowledgeBaseIDs, kb.ID)
		access.KnowledgeBaseTenantIDs[kb.ID] = kb.TenantID
	}
	return access, nil
}

// ListUserConnections returns every active connection owned by a user.
func (s *IntegrationService) ListUserConnections(
	ctx context.Context, userID string,
) ([]*IntegrationConnectionView, error) {
	connections, err := s.repo.ListConnectionsByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	views := make([]*IntegrationConnectionView, 0, len(connections))
	for _, connection := range connections {
		if connection.Status != types.IntegrationConnectionActive {
			continue
		}
		view, err := s.connectionView(ctx, connection)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

// GetUserConnection returns an active connection only to its owning user.
func (s *IntegrationService) GetUserConnection(
	ctx context.Context, userID, connectionID string,
) (*IntegrationConnectionView, error) {
	connection, err := s.repo.GetConnectionByID(ctx, connectionID)
	if err != nil {
		return nil, err
	}
	if connection.UserID != userID || connection.Status != types.IntegrationConnectionActive {
		return nil, apprepo.ErrIntegrationConnectionNotFound
	}
	view, err := s.connectionView(ctx, connection)
	if err != nil {
		return nil, err
	}
	if !view.Available {
		switch view.UnavailableReason {
		case "application_disabled":
			return nil, ErrIntegrationApplicationDisabled
		case "workspace_unavailable":
			return nil, ErrIntegrationPolicyDisabled
		default:
			return nil, ErrIntegrationAccessDenied
		}
	}
	return view, nil
}

// RevokeUserConnection revokes a connection and all of its active credentials.
func (s *IntegrationService) RevokeUserConnection(
	ctx context.Context, userID, connectionID string,
) error {
	connection, err := s.repo.GetConnectionByID(ctx, connectionID)
	if err != nil {
		return err
	}
	if connection.UserID != userID {
		return apprepo.ErrIntegrationConnectionNotFound
	}
	return s.repo.RevokeConnection(ctx, connectionID, userID, time.Now().UTC())
}

func (s *IntegrationService) connectionView(
	ctx context.Context, connection *types.IntegrationConnection,
) (*IntegrationConnectionView, error) {
	app, err := s.repo.GetApplicationByID(ctx, connection.ApplicationID)
	if err != nil {
		return nil, err
	}
	view := &IntegrationConnectionView{
		Connection:                connection,
		Application:               app,
		Tenants:                   make([]IntegrationTenantView, 0),
		KnowledgeBases:            make([]IntegrationKnowledgeBaseView, 0),
		EffectiveKnowledgeBaseIDs: make(types.StringArray, 0),
		EffectiveScopes:           make(types.StringArray, 0),
	}
	if !app.Enabled {
		view.UnavailableReason = "application_disabled"
		return view, nil
	}
	session := &types.IntegrationCredentialSession{
		Connection: connection, Application: app,
		Scopes: intersectIntegrationScopes(connection.Scopes, app.AllowedScopes),
	}
	access, err := s.ResolveCredentialAccess(ctx, session)
	if err != nil {
		return nil, err
	}
	if !types.IntegrationScopesContain(access.Scopes, types.IntegrationScopeKnowledgeRead) {
		view.UnavailableReason = "scope_unavailable"
		if len(access.TenantRoles) == 0 {
			view.UnavailableReason = "workspace_unavailable"
		}
		return view, nil
	}
	view.Tenants, err = s.buildIntegrationTenantViews(ctx, access.TenantRoles)
	if err != nil {
		return nil, err
	}
	view.KnowledgeBases = BuildIntegrationKnowledgeBaseViews(access.KnowledgeBases)
	for index := range view.KnowledgeBases {
		view.KnowledgeBases[index].AccessTenantID =
			access.KnowledgeBaseAccessTenantIDs[view.KnowledgeBases[index].ID]
	}
	view.EffectiveKnowledgeBaseIDs = access.KnowledgeBaseIDs
	view.EffectiveScopes = access.Scopes
	view.Available = true
	return view, nil
}

func (s *IntegrationService) validateAuthorizationParameters(
	ctx context.Context, params IntegrationAuthorizationParameters,
) (*types.IntegrationApplication, types.StringArray, error) {
	app, err := s.repo.GetApplicationByClientID(ctx, strings.TrimSpace(params.ClientID))
	if errors.Is(err, apprepo.ErrIntegrationApplicationNotFound) {
		return nil, nil, ErrIntegrationInvalidApplication
	}
	if err != nil {
		return nil, nil, err
	}
	if !app.Enabled {
		return nil, nil, ErrIntegrationApplicationDisabled
	}
	if !stringInSlice(params.RedirectURI, app.RedirectURIs) {
		return nil, nil, ErrIntegrationInvalidRedirectURI
	}
	if params.State == "" || len(params.State) > 512 {
		return nil, nil, ErrIntegrationInvalidState
	}
	if params.CodeChallengeMethod != "S256" || !validCodeChallenge(params.CodeChallenge) {
		return nil, nil, ErrIntegrationInvalidPKCE
	}
	scopes, err := validateScopes(params.Scopes)
	if err != nil || !scopesSubset(scopes, app.AllowedScopes) {
		return nil, nil, ErrIntegrationInvalidScope
	}
	return app, scopes, nil
}

func (s *IntegrationService) buildIntegrationTenantViews(
	ctx context.Context, roleByTenant map[uint64]types.TenantRole,
) ([]IntegrationTenantView, error) {
	tenantIDs := make([]uint64, 0, len(roleByTenant))
	for tenantID := range roleByTenant {
		tenantIDs = append(tenantIDs, tenantID)
	}
	tenantByID, err := s.tenantService.GetTenantsByIDs(ctx, tenantIDs)
	if err != nil {
		return nil, err
	}
	views := make([]IntegrationTenantView, 0, len(tenantByID))
	for tenantID, role := range roleByTenant {
		tenant := tenantByID[tenantID]
		if tenant == nil {
			continue
		}
		views = append(views, IntegrationTenantView{
			ID: tenant.ID, Name: tenant.Name, Description: tenant.Description, Role: role,
		})
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].Name == views[j].Name {
			return views[i].ID < views[j].ID
		}
		return views[i].Name < views[j].Name
	})
	return views, nil
}

func (s *IntegrationService) listAccessibleKnowledgeBases(
	ctx context.Context,
	tenantID uint64,
) ([]*types.KnowledgeBase, error) {
	own, err := s.kbRepo.ListKnowledgeBasesByTenantID(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	shared, err := s.kbShareRepo.ListSharedKBsForTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*types.KnowledgeBase, len(own)+len(shared))
	for _, kb := range own {
		if kb != nil && !kb.IsTemporary {
			kb.EnsureDefaults()
			byID[kb.ID] = kb
		}
	}
	for _, share := range shared {
		if share != nil && share.SourceTenantID != tenantID &&
			share.KnowledgeBase != nil && !share.KnowledgeBase.IsTemporary {
			share.KnowledgeBase.EnsureDefaults()
			byID[share.KnowledgeBase.ID] = share.KnowledgeBase
		}
	}
	result := make([]*types.KnowledgeBase, 0, len(byID))
	for _, kb := range byID {
		result = append(result, kb)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name == result[j].Name {
			return result[i].ID < result[j].ID
		}
		return result[i].Name < result[j].Name
	})
	return result, nil
}

func (s *IntegrationService) touchCredentialLastUsed(credentialID, connectionID string) {
	now := time.Now().UTC()
	if last, ok := s.lastUsedTouch.Load(credentialID); ok && now.Sub(last.(time.Time)) < time.Minute {
		return
	}
	s.lastUsedTouch.Store(credentialID, now)
	go func() {
		if err := s.repo.TouchCredential(context.Background(), credentialID, connectionID, now); err != nil {
			logger.Warnf(context.Background(), "failed to update integration credential last_used_at: %v", err)
			s.lastUsedTouch.Delete(credentialID)
		}
	}()
}

func validateApplicationInput(
	input IntegrationApplicationInput,
) (string, types.StringArray, types.StringArray, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > 128 || len(input.Description) > 2000 {
		return "", nil, nil, ErrIntegrationInvalidApplication
	}
	redirectURIs := normalizeIntegrationIDs(input.RedirectURIs)
	if len(redirectURIs) == 0 {
		return "", nil, nil, ErrIntegrationInvalidRedirectURI
	}
	for _, redirectURI := range redirectURIs {
		if err := validateIntegrationRedirectURI(redirectURI); err != nil {
			return "", nil, nil, err
		}
	}
	scopes, err := validateScopes(input.AllowedScopes)
	if err != nil {
		return "", nil, nil, err
	}
	return name, redirectURIs, scopes, nil
}

func validateIntegrationRedirectURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || u.User != nil || u.Fragment != "" ||
		(u.Scheme != "http" && u.Scheme != "https") {
		return ErrIntegrationInvalidRedirectURI
	}
	return nil
}

func validateScopes(scopes []string) (types.StringArray, error) {
	if len(scopes) == 0 {
		return nil, ErrIntegrationInvalidScope
	}
	cleaned := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		switch scope {
		case types.IntegrationScopeKnowledgeRead, types.IntegrationScopeKnowledgeChat:
		default:
			return nil, ErrIntegrationInvalidScope
		}
		cleaned = append(cleaned, scope)
	}
	normalized := types.NormalizeIntegrationScopes(cleaned)
	if !types.IntegrationScopesContain(normalized, types.IntegrationScopeKnowledgeRead) {
		return nil, ErrIntegrationInvalidScope
	}
	return normalized, nil
}

func scopesSubset(requested, allowed []string) bool {
	allowedSet := integrationIDSet(types.NormalizeIntegrationScopes(allowed))
	for _, scope := range types.NormalizeIntegrationScopes(requested) {
		if !allowedSet[scope] {
			return false
		}
	}
	return true
}

func intersectIntegrationScopes(scopeGroups ...[]string) types.StringArray {
	if len(scopeGroups) == 0 {
		return nil
	}
	result := types.NormalizeIntegrationScopes(scopeGroups[0])
	for _, group := range scopeGroups[1:] {
		allowed := integrationIDSet(types.NormalizeIntegrationScopes(group))
		result = filterIntegrationIDs(result, allowed)
	}
	return types.NormalizeIntegrationScopes(result)
}

func normalizeIntegrationIDs(values []string) types.StringArray {
	seen := make(map[string]bool, len(values))
	result := make(types.StringArray, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func normalizeIntegrationTenantIDs(values []uint64) []uint64 {
	seen := make(map[uint64]bool, len(values))
	result := make([]uint64, 0, len(values))
	for _, value := range values {
		if value != 0 && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func integrationIDSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func integrationTenantViewIDSet(tenants []IntegrationTenantView) map[uint64]bool {
	set := make(map[uint64]bool, len(tenants))
	for _, tenant := range tenants {
		set[tenant.ID] = true
	}
	return set
}

// BuildIntegrationKnowledgeBaseViews projects knowledge bases to the public integration contract.
func BuildIntegrationKnowledgeBaseViews(knowledgeBases []*types.KnowledgeBase) []IntegrationKnowledgeBaseView {
	result := make([]IntegrationKnowledgeBaseView, 0, len(knowledgeBases))
	for _, kb := range knowledgeBases {
		if kb == nil {
			continue
		}
		result = append(result, BuildIntegrationKnowledgeBaseView(kb))
	}
	return result
}

// BuildIntegrationKnowledgeBaseView projects one knowledge base to the public integration contract.
func BuildIntegrationKnowledgeBaseView(kb *types.KnowledgeBase) IntegrationKnowledgeBaseView {
	return IntegrationKnowledgeBaseView{
		ID:             kb.ID,
		Name:           kb.Name,
		Type:           kb.Type,
		Description:    kb.Description,
		TenantID:       kb.TenantID,
		AccessTenantID: kb.TenantID,
	}
}

func filterIntegrationIDs(values []string, allowed map[string]bool) types.StringArray {
	result := make(types.StringArray, 0, len(values))
	for _, value := range values {
		if allowed[value] {
			result = append(result, value)
		}
	}
	return result
}

func stringInSlice(value string, allowed []string) bool {
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}

func generateIntegrationSecret(prefix string, size int) (string, error) {
	random := make([]byte, size)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(random), nil
}

func hashIntegrationSecret(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func constantTimeSecretMatch(expectedHash, value string) bool {
	return constantTimeValueMatch(expectedHash, hashIntegrationSecret(strings.TrimSpace(value)))
}

func constantTimeValueMatch(expected, actual string) bool {
	return subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) == 1
}

func validCodeChallenge(challenge string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(challenge)
	return err == nil && len(decoded) == sha256.Size
}

func validCodeVerifier(verifier string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	for _, char := range verifier {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("-._~", char) {
			continue
		}
		return false
	}
	return true
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func buildIntegrationRedirect(base string, values map[string]string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", ErrIntegrationInvalidRedirectURI
	}
	query := u.Query()
	for key, value := range values {
		query.Set(key, value)
	}
	u.RawQuery = query.Encode()
	return u.String(), nil
}
