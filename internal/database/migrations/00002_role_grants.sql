-- Grants for the login roles. Requires lighthouse_app and lighthouse_reader to
-- exist (scripts/db/setup.sql, since roles are server-wide).
--
-- lighthouse_app:    reads and changes projects; deployments are append-only
--                    (it can add history, never rewrite it); reads the
--                    migrations table for the startup schema check.
-- lighthouse_reader: reads everything (pgAdmin).
--
-- Privileges are reset first, so this ends in the same state whatever was
-- granted by hand before.

-- +goose Up
REVOKE ALL ON SCHEMA lighthouse FROM PUBLIC;
GRANT USAGE ON SCHEMA lighthouse TO lighthouse_app, lighthouse_reader;

REVOKE ALL ON ALL TABLES IN SCHEMA lighthouse FROM lighthouse_app, lighthouse_reader;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA lighthouse FROM lighthouse_app, lighthouse_reader;

GRANT SELECT, INSERT, UPDATE, DELETE ON lighthouse.projects TO lighthouse_app;
GRANT SELECT, INSERT ON lighthouse.deployments TO lighthouse_app;
GRANT SELECT ON lighthouse.goose_db_version TO lighthouse_app;

GRANT SELECT ON ALL TABLES IN SCHEMA lighthouse TO lighthouse_reader;
ALTER DEFAULT PRIVILEGES FOR ROLE lighthouse_owner IN SCHEMA lighthouse
    GRANT SELECT ON TABLES TO lighthouse_reader;
