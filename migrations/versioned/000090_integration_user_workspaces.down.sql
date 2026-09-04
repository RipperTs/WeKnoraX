DO $$ BEGIN RAISE NOTICE '[Migration 000090 down] Restoring single-workspace integration authorization'; END $$;

-- The new multi-workspace consent cannot be represented by the old model.
DELETE FROM integration_connections;

DROP TABLE IF EXISTS integration_connection_tenants;

DROP INDEX IF EXISTS idx_integration_connections_user;
ALTER TABLE integration_connections
    DROP CONSTRAINT IF EXISTS uq_integration_connections_application_user;
ALTER TABLE integration_connections
    ADD COLUMN tenant_id BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE;
ALTER TABLE integration_connections
    ADD CONSTRAINT integration_connections_application_id_tenant_id_user_id_key
        UNIQUE (application_id, tenant_id, user_id);

ALTER TABLE tenant_integration_policies
    ADD COLUMN knowledge_base_ids JSONB NOT NULL DEFAULT '[]'::jsonb;

CREATE TABLE integration_connection_knowledge_bases (
    connection_id     VARCHAR(36) NOT NULL REFERENCES integration_connections(id) ON DELETE CASCADE,
    knowledge_base_id VARCHAR(36) NOT NULL REFERENCES knowledge_bases(id) ON DELETE CASCADE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (connection_id, knowledge_base_id)
);

CREATE INDEX idx_integration_connections_user
    ON integration_connections(user_id, tenant_id);

DO $$ BEGIN RAISE NOTICE '[Migration 000090 down] Single-workspace integration authorization restored'; END $$;
