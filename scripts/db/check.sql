-- Lighthouse database: show what exists. Read-only; changes nothing.
--
-- Run as the Admin superuser, e.g. on the server:
--   docker exec -i sparkdb psql -U Admin -d postgres -f - < scripts/db/check.sql

\echo '== Roles (want: owner NOLOGIN; migrator, app, reader LOGIN; migrator member of owner with role=lighthouse_owner)'
SELECT r.rolname AS role,
       r.rolcanlogin AS can_login,
       coalesce((SELECT string_agg(b.rolname, ', ')
                   FROM pg_auth_members m JOIN pg_roles b ON b.oid = m.roleid
                  WHERE m.member = r.oid), '') AS member_of,
       coalesce(array_to_string(r.rolconfig, '; '), '') AS settings
  FROM pg_roles r
 WHERE r.rolname LIKE 'lighthouse\_%'
 ORDER BY r.rolname;

\echo '== Database (want: owner lighthouse_owner; CONNECT for migrator, app, reader only)'
SELECT datname AS database,
       pg_get_userbyid(datdba) AS owner,
       coalesce(array_to_string(datacl, ' '), '(default: PUBLIC may connect)') AS access
  FROM pg_database
 WHERE datname = 'lighthouse_db';

SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'lighthouse_db') AS db_exists \gset
\if :db_exists
\connect lighthouse_db
\echo '== Schemas in lighthouse_db (want: lighthouse owned by lighthouse_owner)'
SELECT nspname AS schema, pg_get_userbyid(nspowner) AS owner
  FROM pg_namespace
 WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'
 ORDER BY 1;
\echo '== Tables in lighthouse_db'
SELECT schemaname AS schema, tablename AS "table", tableowner AS owner
  FROM pg_tables
 WHERE schemaname NOT IN ('pg_catalog', 'information_schema')
 ORDER BY 1, 2;
\else
\echo '== lighthouse_db does not exist yet'
\endif
