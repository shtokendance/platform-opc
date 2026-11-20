CREATE TABLE space_revocations (
    tenant_id TEXT PRIMARY KEY,
    revoked_at TIMESTAMPTZ NOT NULL
);
