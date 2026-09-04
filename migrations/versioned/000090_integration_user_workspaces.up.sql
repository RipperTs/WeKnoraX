-- Replace single-workspace, knowledge-base grants with per-user multi-workspace grants.
DO $$ BEGIN RAISE NOTICE '[Migration 000090] Updating third-party integration authorization scope'; END $$;

-- Existing consent covered selected knowledge bases in one workspace. It cannot
-- be widened safely to every visible knowledge base, so require fresh consent.
DELETE FROM integration_connections;

DROP TABLE IF EXISTS integration_connection_knowledge_bases;

ALTER TABLE tenant_integration_policies
    DROP COLUMN IF EXISTS knowledge_base_ids;

DROP INDEX IF EXISTS idx_integration_connections_user;
ALTER TABLE integration_connections
    DROP CONSTRAINT IF EXISTS integration_connections_application_id_tenant_id_user_id_key;
ALTER TABLE integration_connections
    DROP CONSTRAINT IF EXISTS integration_connections_tenant_id_fkey;
ALTER TABLE integration_connections
    DROP COLUMN IF EXISTS tenant_id;
ALTER TABLE integration_connections
    ADD CONSTRAINT uq_integration_connections_application_user UNIQUE (application_id, user_id);

CREATE TABLE integration_connection_tenants (
    connection_id VARCHAR(36) NOT NULL REFERENCES integration_connections(id) ON DELETE CASCADE,
    tenant_id     BIGINT      NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (connection_id, tenant_id)
);

CREATE INDEX idx_integration_connections_user
    ON integration_connections(user_id);
CREATE INDEX idx_integration_connection_tenants_tenant
    ON integration_connection_tenants(tenant_id);

DO $$ BEGIN RAISE NOTICE '[Migration 000090] Third-party integration authorization scope updated'; END $$;
