-- Lighthouse database: the one-time Admin setup on sparkdb.
--
-- Creates what's missing and fixes ownership and access:
--   roles     lighthouse_owner (NOLOGIN), lighthouse_migrator (member of the owner,
--             acting as it), lighthouse_app, lighthouse_reader
--   database  lighthouse_db, owned by lighthouse_owner; only the three login
--             roles may connect
--   schema    lighthouse, if it exists already: owned by lighthouse_owner
-- Everything inside the schema (tables, grants) comes from Lighthouse's goose
-- migrations, never from here.
--
-- Safe to run again. It never changes the password of a role that exists; a
-- password is only needed for a role that doesn't exist yet, and every missing
-- one is checked before anything changes.
--
-- Run as the Admin superuser; DOCUMENTATION.md §10.1 has the full steps:
--   docker exec -i sparkdb psql -U Admin -d postgres \
--     -v migrator_password="..." -v app_password="..." -v reader_password="..." \
--     -f - < scripts/db/setup.sql

\set ON_ERROR_STOP on
SET client_min_messages = warning;

SELECT NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'lighthouse_owner')    AS need_owner,
       NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'lighthouse_migrator') AS need_migrator,
       NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'lighthouse_app')      AS need_app,
       NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'lighthouse_reader')   AS need_reader,
       NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'lighthouse_db')    AS need_db,
       current_setting('is_superuser') = 'on' AS is_superuser
\gset

-- 1. Preconditions, before anything changes.

\if :is_superuser
\else
DO $$ BEGIN RAISE EXCEPTION 'Run this as the Admin superuser (psql -U Admin).'; END $$;
\endif

\if :need_migrator
\if :{?migrator_password}
\else
DO $$ BEGIN RAISE EXCEPTION 'lighthouse_migrator doesn''t exist: pass its password with -v migrator_password=... Nothing was changed.'; END $$;
\endif
\endif

\if :need_app
\if :{?app_password}
\else
DO $$ BEGIN RAISE EXCEPTION 'lighthouse_app doesn''t exist: pass its password with -v app_password=... Nothing was changed.'; END $$;
\endif
\endif

\if :need_reader
\if :{?reader_password}
\else
DO $$ BEGIN RAISE EXCEPTION 'lighthouse_reader doesn''t exist: pass its password with -v reader_password=... Nothing was changed.'; END $$;
\endif
\endif

-- 2. Roles.

\if :need_owner
\echo 'Creating lighthouse_owner'
CREATE ROLE lighthouse_owner NOLOGIN;
\else
-- Nothing logs in as the owner; its old password (if any) goes too.
ALTER ROLE lighthouse_owner NOLOGIN PASSWORD NULL;
\endif

\if :need_migrator
\echo 'Creating lighthouse_migrator'
CREATE ROLE lighthouse_migrator LOGIN PASSWORD :'migrator_password';
\else
ALTER ROLE lighthouse_migrator LOGIN;
\endif

\if :need_app
\echo 'Creating lighthouse_app'
CREATE ROLE lighthouse_app LOGIN PASSWORD :'app_password';
\else
ALTER ROLE lighthouse_app LOGIN;
\endif

\if :need_reader
\echo 'Creating lighthouse_reader'
CREATE ROLE lighthouse_reader LOGIN PASSWORD :'reader_password';
\else
ALTER ROLE lighthouse_reader LOGIN;
\endif

-- The migrator acts as the owner, so everything the migrations create is
-- owned by lighthouse_owner, not by whoever ran them.
GRANT lighthouse_owner TO lighthouse_migrator;
ALTER ROLE lighthouse_migrator SET role = 'lighthouse_owner';

-- 3. The database.

\if :need_db
\echo 'Creating lighthouse_db'
CREATE DATABASE lighthouse_db OWNER lighthouse_owner;
\else
ALTER DATABASE lighthouse_db OWNER TO lighthouse_owner;
\endif

REVOKE ALL ON DATABASE lighthouse_db FROM PUBLIC;
GRANT CONNECT ON DATABASE lighthouse_db TO lighthouse_migrator, lighthouse_app, lighthouse_reader;

-- 4. Inside the database: the lighthouse schema (if an earlier setup made
-- it) belongs to the owner, and nobody else may create objects in public.

\connect lighthouse_db
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'lighthouse') THEN
    ALTER SCHEMA lighthouse OWNER TO lighthouse_owner;
  END IF;
END $$;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;

\echo
\echo 'Done. Check: every row below matches its "want".'
\echo
\connect postgres
SELECT r.rolname AS role, r.rolcanlogin AS can_login,
       coalesce((SELECT string_agg(b.rolname, ', ') FROM pg_auth_members m JOIN pg_roles b ON b.oid = m.roleid
                  WHERE m.member = r.oid), '') AS member_of,
       coalesce(array_to_string(r.rolconfig, '; '), '') AS settings,
       CASE r.rolname
         WHEN 'lighthouse_owner'    THEN 'NOLOGIN'
         WHEN 'lighthouse_migrator' THEN 'LOGIN, member of lighthouse_owner, role=lighthouse_owner'
         ELSE 'LOGIN'
       END AS want
  FROM pg_roles r
 WHERE r.rolname IN ('lighthouse_owner', 'lighthouse_migrator', 'lighthouse_app', 'lighthouse_reader')
 ORDER BY r.rolname;
SELECT datname AS database, pg_get_userbyid(datdba) AS owner, array_to_string(datacl, ' ') AS access,
       'owner lighthouse_owner; c (connect) for migrator, app, reader' AS want
  FROM pg_database WHERE datname = 'lighthouse_db';
