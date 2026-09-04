-- Restoring the old model invalidates multi-workspace consent.
DELETE FROM integration_connections;

DROP TABLE IF EXISTS integration_credentials;
DROP TABLE IF EXISTS integration_authorization_codes;
DROP TABLE IF EXISTS integration_connection_tenants;
DROP TABLE IF EXISTS integration_connections;

ALTER TABLE tenant_integration_policies RENAME TO tenant_integration_policies_new;
CREATE TABLE tenant_integration_policies (
    id                 TEXT PRIMARY KEY,
    application_id     TEXT NOT NULL REFERENCES integration_applications(id) ON DELETE CASCADE,
    tenant_id          INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    enabled            INTEGER NOT NULL DEFAULT 1,
    allowed_scopes     TEXT NOT NULL DEFAULT '[]',
    knowledge_base_ids TEXT NOT NULL DEFAULT '[]',
    created_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (application_id, tenant_id)
);
INSERT INTO tenant_integration_policies (
    id, application_id, tenant_id, enabled, allowed_scopes, knowledge_base_ids, created_at, updated_at
)
SELECT id, application_id, tenant_id, enabled, allowed_scopes, '[]', created_at, updated_at
FROM tenant_integration_policies_new;
DROP TABLE tenant_integration_policies_new;

CREATE TABLE integration_connections (
    id             TEXT PRIMARY KEY,
    application_id TEXT NOT NULL REFERENCES integration_applications(id) ON DELETE CASCADE,
    tenant_id      INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    scopes         TEXT NOT NULL DEFAULT '[]',
    status         TEXT NOT NULL DEFAULT 'active',
    last_used_at   DATETIME,
    created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (application_id, tenant_id, user_id)
);

CREATE TABLE integration_connection_knowledge_bases (
    connection_id     TEXT NOT NULL REFERENCES integration_connections(id) ON DELETE CASCADE,
    knowledge_base_id TEXT NOT NULL REFERENCES knowledge_bases(id) ON DELETE CASCADE,
    created_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (connection_id, knowledge_base_id)
);

CREATE TABLE integration_authorization_codes (
    code_hash      TEXT PRIMARY KEY,
    application_id TEXT NOT NULL REFERENCES integration_applications(id) ON DELETE CASCADE,
    connection_id  TEXT NOT NULL REFERENCES integration_connections(id) ON DELETE CASCADE,
    redirect_uri   TEXT NOT NULL,
    scopes         TEXT NOT NULL DEFAULT '[]',
    code_challenge TEXT NOT NULL,
    expires_at     DATETIME NOT NULL,
    consumed_at    DATETIME,
    created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE integration_credentials (
    id            TEXT PRIMARY KEY,
    connection_id TEXT NOT NULL REFERENCES integration_connections(id) ON DELETE CASCADE,
    token_hash    TEXT NOT NULL UNIQUE,
    token_prefix  TEXT NOT NULL,
    last_used_at  DATETIME,
    revoked_at    DATETIME,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_integration_policies_tenant
    ON tenant_integration_policies(tenant_id);
CREATE INDEX idx_integration_connections_user
    ON integration_connections(user_id, tenant_id);
CREATE INDEX idx_integration_connections_status
    ON integration_connections(status);
CREATE INDEX idx_integration_authorization_codes_expires
    ON integration_authorization_codes(expires_at);
CREATE INDEX idx_integration_credentials_connection
    ON integration_credentials(connection_id);
CREATE INDEX idx_integration_credentials_revoked
    ON integration_credentials(revoked_at);
