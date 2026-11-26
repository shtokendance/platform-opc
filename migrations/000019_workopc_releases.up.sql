CREATE TABLE IF NOT EXISTS workopc_releases (
    id                    TEXT PRIMARY KEY,
    version               TEXT NOT NULL,
    channel               TEXT NOT NULL,
    status                TEXT NOT NULL DEFAULT 'draft',
    release_notes         TEXT NOT NULL DEFAULT '',
    rollout_percentage    INTEGER NOT NULL DEFAULT 100,
    is_mandatory          BOOLEAN NOT NULL DEFAULT FALSE,
    min_supported_version TEXT NOT NULL DEFAULT '',
    commit_sha            TEXT NOT NULL DEFAULT '',
    published_by          TEXT NOT NULL DEFAULT '',
    published_at          TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (version, channel)
);

CREATE INDEX IF NOT EXISTS idx_workopc_releases_channel_status ON workopc_releases (channel, status, published_at);

CREATE TABLE IF NOT EXISTS workopc_release_artifacts (
    id             TEXT PRIMARY KEY,
    release_id     TEXT NOT NULL REFERENCES workopc_releases(id) ON DELETE CASCADE,
    target         TEXT NOT NULL,
    kind           TEXT NOT NULL,
    filename       TEXT NOT NULL,
    object_key     TEXT NOT NULL UNIQUE,
    size_bytes     BIGINT NOT NULL,
    sha512         TEXT NOT NULL,
    sha256         TEXT NOT NULL,
    signed         BOOLEAN NOT NULL DEFAULT FALSE,
    notarized      BOOLEAN NOT NULL DEFAULT FALSE,
    download_count BIGINT NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (release_id, target, kind)
);

CREATE INDEX IF NOT EXISTS idx_workopc_artifacts_release ON workopc_release_artifacts (release_id);
CREATE INDEX IF NOT EXISTS idx_workopc_artifacts_target_filename ON workopc_release_artifacts (target, filename);

CREATE TABLE IF NOT EXISTS workopc_release_audit_logs (
    id         TEXT PRIMARY KEY,
    release_id TEXT NOT NULL DEFAULT '',
    actor      TEXT NOT NULL,
    action     TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_workopc_audit_created ON workopc_release_audit_logs (created_at);
