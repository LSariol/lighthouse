-- The watched projects, and every deployment attempt.
--
-- Runs as lighthouse_migrator, which acts as lighthouse_owner, so the owner
-- owns everything here. Roles and the database itself are created by hand
-- (scripts/db/setup.sql); grants are in 00002.

-- +goose Up
CREATE TABLE lighthouse.projects (
    id              bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- The name used in the CLI. Unique without regard to case (index below).
    name            text        NOT NULL CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$'),
    -- The GitHub repository: https://github.com/<repo_owner>/<repo_name>.
    repo_owner      text        NOT NULL CHECK (repo_owner ~ '^[A-Za-z0-9_.-]+$'),
    repo_name       text        NOT NULL CHECK (repo_name ~ '^[A-Za-z0-9_.-]+$'),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    -- The last successful deployment.
    deployed_sha    text,
    deployed_at     timestamptz,
    -- The last check of GitHub, and the last problem (check or deploy).
    last_checked_at timestamptz,
    check_count     bigint      NOT NULL DEFAULT 0,
    last_error      text,
    last_error_at   timestamptz
);

CREATE UNIQUE INDEX projects_name_key ON lighthouse.projects (lower(name));
CREATE UNIQUE INDEX projects_repo_key ON lighthouse.projects (lower(repo_owner), lower(repo_name));

-- One row per deployment attempt, written when it ends. Append-only for the
-- app (00002); a project's rows go when the project is removed.
CREATE TABLE lighthouse.deployments (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id  bigint      NOT NULL REFERENCES lighthouse.projects (id) ON DELETE CASCADE,
    sha         text,
    trigger     text        NOT NULL CHECK (trigger IN ('check', 'manual')),
    status      text        NOT NULL CHECK (status IN ('succeeded', 'failed')),
    started_at  timestamptz NOT NULL,
    finished_at timestamptz NOT NULL,
    error       text
);

CREATE INDEX deployments_project_started_idx ON lighthouse.deployments (project_id, started_at DESC);
