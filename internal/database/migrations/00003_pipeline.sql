-- What the v1.0.0 deploy pipeline records.
--
-- projects:    the compose project (the compose file's `name:`, stored at each
--              deploy; one per Lighthouse project), and the failure count
--              behind the "broken" state.
-- deployments: rolled_back as a result, what kind of failure, which step.
-- deployment_steps: each step of a deployment, with the tail of its output
--              (secret values removed before it's stored).

-- +goose Up
ALTER TABLE lighthouse.projects
    ADD COLUMN compose_project text    CHECK (compose_project ~ '^[a-z0-9][a-z0-9_-]*$'),
    ADD COLUMN failure_count   integer NOT NULL DEFAULT 0,
    ADD COLUMN failing_sha     text,
    ADD COLUMN broken          boolean NOT NULL DEFAULT false;

CREATE UNIQUE INDEX projects_compose_project_key ON lighthouse.projects (compose_project);

ALTER TABLE lighthouse.deployments DROP CONSTRAINT deployments_status_check;
ALTER TABLE lighthouse.deployments
    ADD CONSTRAINT deployments_status_check CHECK (status IN ('succeeded', 'failed', 'rolled_back')),
    ADD COLUMN failure_kind text CHECK (failure_kind IN ('transient', 'permanent')),
    ADD COLUMN failed_step  text;

CREATE TABLE lighthouse.deployment_steps (
    deployment_id bigint      NOT NULL REFERENCES lighthouse.deployments (id) ON DELETE CASCADE,
    position      integer     NOT NULL,
    step          text        NOT NULL,
    status        text        NOT NULL CHECK (status IN ('succeeded', 'failed', 'skipped')),
    started_at    timestamptz NOT NULL,
    finished_at   timestamptz NOT NULL,
    log           text,
    PRIMARY KEY (deployment_id, position)
);

-- Append-only for the app, like deployments. The reader gets it through the
-- default privileges from 00002.
GRANT SELECT, INSERT ON lighthouse.deployment_steps TO lighthouse_app;
