package types

import (
	"errors"
	"time"
)

// ErrIntegrationInvalidCredential indicates an invalid or inactive wkic_ credential.
var ErrIntegrationInvalidCredential = errors.New("invalid integration credential")

const (
	// IntegrationScopeKnowledgeRead grants read and retrieval operations over authorized knowledge bases.
	IntegrationScopeKnowledgeRead = "knowledge.read"
	// IntegrationScopeKnowledgeChat additionally grants knowledge-base chat operations.
	IntegrationScopeKnowledgeChat = "knowledge.chat"

	// IntegrationConnectionActive marks a usable user connection.
	IntegrationConnectionActive = "active"
	// IntegrationConnectionRevoked marks a user connection and its credentials as unusable.
	IntegrationConnectionRevoked = "revoked"
)

// IntegrationApplication is a SystemAdmin-registered OAuth-style client.
// ClientSecretHash is never serialized; the plaintext secret is returned once.
type IntegrationApplication struct {
	ID               string      `json:"id" gorm:"type:varchar(36);primaryKey"`
	ClientID         string      `json:"client_id" gorm:"type:varchar(64);not null;uniqueIndex"`
	ClientSecretHash string      `json:"-" gorm:"type:varchar(64);not null"`
	Name             string      `json:"name" gorm:"type:varchar(128);not null"`
	Description      string      `json:"description" gorm:"type:text;not null;default:''"`
	RedirectURIs     StringArray `json:"redirect_uris" gorm:"type:jsonb;not null;default:'[]'"`
	AllowedScopes    StringArray `json:"allowed_scopes" gorm:"type:jsonb;not null;default:'[]'"`
	Enabled          bool        `json:"enabled" gorm:"not null;default:true"`
	CreatedBy        string      `json:"created_by" gorm:"type:varchar(36);not null"`
	CreatedAt        time.Time   `json:"created_at"`
	UpdatedAt        time.Time   `json:"updated_at"`
}

// TableName returns the integration application table name.
func (IntegrationApplication) TableName() string { return "integration_applications" }

// TenantIntegrationPolicy optionally narrows a globally enabled application.
// An absent row means enabled with the application's scopes and all user-visible KBs.
type TenantIntegrationPolicy struct {
	ID               string      `json:"id" gorm:"type:varchar(36);primaryKey"`
	ApplicationID    string      `json:"application_id" gorm:"type:varchar(36);not null;uniqueIndex:uq_int_policy"`
	TenantID         uint64      `json:"tenant_id" gorm:"not null;uniqueIndex:uq_int_policy"`
	Enabled          bool        `json:"enabled" gorm:"not null;default:true"`
	AllowedScopes    StringArray `json:"allowed_scopes" gorm:"type:jsonb;not null;default:'[]'"`
	KnowledgeBaseIDs StringArray `json:"knowledge_base_ids" gorm:"type:jsonb;not null;default:'[]'"`
	CreatedAt        time.Time   `json:"created_at"`
	UpdatedAt        time.Time   `json:"updated_at"`
}

// TableName returns the workspace integration policy table name.
func (TenantIntegrationPolicy) TableName() string { return "tenant_integration_policies" }

// IntegrationConnection binds one client to one WeKnora user and workspace.
type IntegrationConnection struct {
	ID            string      `json:"id" gorm:"type:varchar(36);primaryKey"`
	ApplicationID string      `json:"application_id" gorm:"type:varchar(36);not null;uniqueIndex:uq_integration_conn"`
	TenantID      uint64      `json:"tenant_id" gorm:"not null;uniqueIndex:uq_integration_conn"`
	UserID        string      `json:"user_id" gorm:"type:varchar(36);not null;uniqueIndex:uq_integration_conn"`
	Scopes        StringArray `json:"scopes" gorm:"type:jsonb;not null;default:'[]'"`
	Status        string      `json:"status" gorm:"type:varchar(16);not null;default:'active'"`
	LastUsedAt    *time.Time  `json:"last_used_at,omitempty"`
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`
}

// TableName returns the integration connection table name.
func (IntegrationConnection) TableName() string { return "integration_connections" }

// IntegrationConnectionKnowledgeBase stores one knowledge-base grant for a connection.
type IntegrationConnectionKnowledgeBase struct {
	ConnectionID    string    `json:"connection_id" gorm:"type:varchar(36);primaryKey"`
	KnowledgeBaseID string    `json:"knowledge_base_id" gorm:"type:varchar(36);primaryKey"`
	CreatedAt       time.Time `json:"created_at"`
}

// TableName returns the integration connection grant table name.
func (IntegrationConnectionKnowledgeBase) TableName() string {
	return "integration_connection_knowledge_bases"
}

// IntegrationAuthorizationCode stores a hashed, short-lived and single-use browser grant.
type IntegrationAuthorizationCode struct {
	CodeHash      string      `json:"-" gorm:"type:varchar(64);primaryKey"`
	ApplicationID string      `json:"application_id" gorm:"type:varchar(36);not null"`
	ConnectionID  string      `json:"connection_id" gorm:"type:varchar(36);not null"`
	RedirectURI   string      `json:"redirect_uri" gorm:"type:text;not null"`
	Scopes        StringArray `json:"scopes" gorm:"type:jsonb;not null;default:'[]'"`
	CodeChallenge string      `json:"-" gorm:"type:varchar(128);not null"`
	ExpiresAt     time.Time   `json:"expires_at"`
	ConsumedAt    *time.Time  `json:"consumed_at,omitempty"`
	CreatedAt     time.Time   `json:"created_at"`
}

// TableName returns the integration authorization-code table name.
func (IntegrationAuthorizationCode) TableName() string {
	return "integration_authorization_codes"
}

// IntegrationCredential stores only the hash and display prefix of a wkic_ credential.
type IntegrationCredential struct {
	ID           string     `json:"id" gorm:"type:varchar(36);primaryKey"`
	ConnectionID string     `json:"connection_id" gorm:"type:varchar(36);not null;index"`
	TokenHash    string     `json:"-" gorm:"type:varchar(64);not null;uniqueIndex"`
	TokenPrefix  string     `json:"token_prefix" gorm:"type:varchar(16);not null"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty" gorm:"index"`
	CreatedAt    time.Time  `json:"created_at"`
}

// TableName returns the integration credential table name.
func (IntegrationCredential) TableName() string { return "integration_credentials" }

// IntegrationCredentialSession contains the verified records needed to project
// an integration credential into the existing request authorization context.
type IntegrationCredentialSession struct {
	Credential  *IntegrationCredential
	Connection  *IntegrationConnection
	Application *IntegrationApplication
	Policy      *TenantIntegrationPolicy
	Scopes      StringArray
}

// NormalizeIntegrationScopes drops unknown values, deduplicates them and
// makes knowledge.chat imply knowledge.read.
func NormalizeIntegrationScopes(scopes []string) StringArray {
	seen := make(map[string]bool, len(scopes)+1)
	for _, scope := range scopes {
		switch scope {
		case IntegrationScopeKnowledgeRead, IntegrationScopeKnowledgeChat:
			seen[scope] = true
		}
	}
	if seen[IntegrationScopeKnowledgeChat] {
		seen[IntegrationScopeKnowledgeRead] = true
	}
	out := make(StringArray, 0, 2)
	for _, scope := range []string{IntegrationScopeKnowledgeRead, IntegrationScopeKnowledgeChat} {
		if seen[scope] {
			out = append(out, scope)
		}
	}
	return out
}

// IntegrationScopesContain reports whether a normalized scope set includes the required scope.
func IntegrationScopesContain(have []string, required string) bool {
	for _, scope := range NormalizeIntegrationScopes(have) {
		if scope == required {
			return true
		}
	}
	return false
}
