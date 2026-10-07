-- What the orchestrator (v1.0.0 step 5) records.
--
-- projects:    the release deployed (for a project that deploys version
--              tags) and the highest one ever deployed (so going back by
--              hand isn't undone by the next check); the settings read from
--              its compose file's x-lighthouse block (deploy mode and tier);
--              and whether it was stopped on purpose (`stop`), so the
--              reconcile loop leaves it down.
-- deployments: the release deployed, and deploys started by the reconcile
--              loop.

-- +goose Up
ALTER TABLE lighthouse.projects
    ADD COLUMN deployed_version text,
    ADD COLUMN highest_version  text,
    ADD COLUMN deploy_mode      text    NOT NULL DEFAULT 'branch' CHECK (deploy_mode IN ('branch', 'releases')),
    ADD COLUMN tier             text    NOT NULL DEFAULT 'app' CHECK (tier IN ('data', 'infra', 'app')),
    ADD COLUMN stopped          boolean NOT NULL DEFAULT false;

ALTER TABLE lighthouse.deployments
    ADD COLUMN version text;

ALTER TABLE lighthouse.deployments DROP CONSTRAINT deployments_trigger_check;
ALTER TABLE lighthouse.deployments
    ADD CONSTRAINT deployments_trigger_check CHECK (trigger IN ('check', 'manual', 'reconcile'));
