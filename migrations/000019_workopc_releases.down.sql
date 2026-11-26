DROP TABLE IF EXISTS workopc_release_audit_logs;
DROP TABLE IF EXISTS workopc_release_artifacts;
DROP TABLE IF EXISTS workopc_releases;
DELETE FROM schema_migrations WHERE version = '000019_workopc_releases';
