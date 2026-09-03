-- Third-party browser launch and per-user Agent credentials.
DO $$ BEGIN RAISE NOTICE '[Migration 000089] Creating third-party integration tables'; END $$;

CREATE TABLE IF NOT EXISTS integration_applications (
    id                 VARCHAR(36)  PRIMARY KEY,
    client_id          VARCHAR(64)  NOT NULL UNIQUE,
    client_secret_hash VARCHAR(64)  NOT NULL,
    name               VARCHAR(128) NOT NULL,
    description        TEXT         NOT NULL DEFAULT '',
    redirect_uris      JSONB        NOT NULL DEFAULT '[]'::jsonb,
    allowed_scopes     JSONB        NOT NULL DEFAULT '[]'::jsonb,
    enabled            BOOLEAN      NOT NULL DEFAULT TRUE,
    created_by         VARCHAR(36)  NOT NULL,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS tenant_integration_policies (
    id                     VARCHAR(36) PRIMARY KEY,
    application_id         VARCHAR(36) NOT NULL REFERENCES integration_applications(id) ON DELETE CASCADE,
    tenant_id              BIGINT      NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    enabled                BOOLEAN     NOT NULL DEFAULT TRUE,
    allowed_scopes         JSONB       NOT NULL DEFAULT '[]'::jsonb,
    knowledge_base_ids     JSONB       NOT NULL DEFAULT '[]'::jsonb,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (application_id, tenant_id)
);

CREATE TABLE IF NOT EXISTS integration_connections (
    id             VARCHAR(36) PRIMARY KEY,
    application_id VARCHAR(36) NOT NULL REFERENCES integration_applications(id) ON DELETE CASCADE,
    tenant_id      BIGINT      NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id        VARCHAR(36) NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    scopes         JSONB       NOT NULL DEFAULT '[]'::jsonb,
    status         VARCHAR(16) NOT NULL DEFAULT 'active',
    last_used_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (application_id, tenant_id, user_id)
);

CREATE TABLE IF NOT EXISTS integration_connection_knowledge_bases (
    connection_id    VARCHAR(36) NOT NULL REFERENCES integration_connections(id) ON DELETE CASCADE,
    knowledge_base_id VARCHAR(36) NOT NULL REFERENCES knowledge_bases(id) ON DELETE CASCADE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (connection_id, knowledge_base_id)
);

CREATE TABLE IF NOT EXISTS integration_authorization_codes (
    code_hash       VARCHAR(64)  PRIMARY KEY,
    application_id  VARCHAR(36)  NOT NULL REFERENCES integration_applications(id) ON DELETE CASCADE,
    connection_id   VARCHAR(36)  NOT NULL REFERENCES integration_connections(id) ON DELETE CASCADE,
    redirect_uri    TEXT         NOT NULL,
    scopes          JSONB        NOT NULL DEFAULT '[]'::jsonb,
    code_challenge  VARCHAR(128) NOT NULL,
    expires_at      TIMESTAMPTZ  NOT NULL,
    consumed_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS integration_credentials (
    id           VARCHAR(36) PRIMARY KEY,
    connection_id VARCHAR(36) NOT NULL REFERENCES integration_connections(id) ON DELETE CASCADE,
    token_hash    VARCHAR(64) NOT NULL UNIQUE,
    token_prefix  VARCHAR(16) NOT NULL,
    last_used_at  TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_integration_policies_tenant
    ON tenant_integration_policies(tenant_id);
CREATE INDEX IF NOT EXISTS idx_integration_connections_user
    ON integration_connections(user_id, tenant_id);
CREATE INDEX IF NOT EXISTS idx_integration_connections_status
    ON integration_connections(status);
CREATE INDEX IF NOT EXISTS idx_integration_authorization_codes_expires
    ON integration_authorization_codes(expires_at);
CREATE INDEX IF NOT EXISTS idx_integration_credentials_connection
    ON integration_credentials(connection_id);
CREATE INDEX IF NOT EXISTS idx_integration_credentials_revoked
    ON integration_credentials(revoked_at);

DO $$ BEGIN RAISE NOTICE '[Migration 000089] Third-party integration tables ready'; END $$;
