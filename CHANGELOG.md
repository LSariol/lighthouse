# Changelog

All notable changes to Lighthouse. Versions follow [semantic versioning](https://semver.org).

## [1.0.2] — 2026-10-07

### Fixed
- **A project's settings come only from its default branch.** Deploying an older tag whose compose file predates its `x-lighthouse` block flipped the project back to deploying its branch, which then redeployed it. `deploy` and `check` also read the branch's settings first, so a release-mode project that hasn't been checked yet deploys its newest release.

### Removed
- `lighthouse import` and its `repos.json` reader: the move from pre-1.0 is done (v1.0.1's DOCUMENTATION.md §10 has the steps).

### Changed
- Comments trimmed throughout: none inside functions unless they prevent a mistake, one-line descriptions. No behaviour change.

## [1.0.1] — 2026-10-07

### Fixed
- **Self-update could repeat itself and then stall.** The Lighthouse that starts during the swap saw its own project as not yet deployed and handed off again, which removed the first helper mid-check; and when the swap changed nothing, the old Lighthouse waited forever to be replaced, paused. Now nothing deploys Lighthouse while its helper is working (refused before anything is downloaded), and a Lighthouse that isn't replaced records the helper's result and carries on.
- The update helper's container no longer counts as one of Lighthouse's services (it inherited the image's Compose labels).
- A self-update is no longer logged as a failed check.
- `check` no longer says a failure "counts toward broken": a dry run records nothing.

## [1.0.0] — 2026-10-07

A rewrite of the pre-1.0 Lighthouse into a self-sufficient deployer: safe deploys that roll back, rules a project can't break out of, release-tag projects, infrastructure ordering and backups, recovery of projects that go down, and updates of itself. Moving from pre-1.0: DOCUMENTATION.md §10, "Upgrading from the pre-1.0 Lighthouse".

### Added
- **Safe deploys.** Build first, swap last: the running version keeps serving while the new one is downloaded (the exact commit, through the GitHub API, so private repositories work), checked, tested and built; `docker compose up` is the only downtime; every service must then come up healthy (or stay up), and if one doesn't, the previous version is put back. Each deploy cleans up after itself.
- **Deploy rules.** Before anything is built, the compose file is checked: no privileges, host namespaces, devices or Docker socket; no host paths outside the repository and `/srv/server/storage/<compose project>/` (symlinks followed); no other project's volumes or networks; only the project's own Cove keys (`<PROJECT>_*`) and `SHARED_*`; no name another project has on `spark`. Exceptions live in `policy.json`, built into the binary. `help rules` lists them.
- **Test stages.** A Dockerfile stage named `test` is built before every deploy, without secrets; failing tests stop it.
- **Settings in the project's compose file** (`x-lighthouse`): `deploy: releases` deploys only version tags (and only one newer than any deployed before); `tier: data|infra|app` orders deploys; `backup: postgres` dumps the database before each deploy (`BACKUP_PATH`, the newest 5 kept).
- **One deploy at a time, in tier order,** for checks, the CLI and the reconcile loop alike. The previous version's secrets are fetched before every swap, so a rollback doesn't need Cove.
- **Failures:** passing problems (GitHub, Cove, the network) are retried after a growing wait (1 to 30 minutes); a commit that fails 3 times for a reason of its own marks its project **broken** until a new commit or `retry`.
- **The reconcile loop** brings back a deployed project that stays down; `stop` keeps one down on purpose.
- **Self-update.** Lighthouse deploys its own release tags through an update helper container, which waits until the new Lighthouse is healthy (`lighthouse health`, the container's healthcheck) and puts the old one back if it isn't.
- **Going back:** `rollback <name>`, and `deploy <name> <tag or commit>`; checks don't redeploy what you went back from.
- **Compose projects with several services:** named by the compose file, found by Compose's labels; `<name>:<service>` addresses one service.
- **Postgres** (`lighthouse_db` on sparkdb) holds the projects and every deploy with each step's output (secret values hidden): `history`, `report`. The schema comes from goose migrations built into the binary, applied at startup.
- Commands: `check` (a dry run), `report`, `history`, `rollback`, `retry`, `pause [all]` / `resume [all]`, `lighthouse migrate`, `lighthouse import` (a pre-1.0 `repos.json`), `lighthouse health`.
- ETags on GitHub checks (an unchanged answer doesn't count against the rate limit). Every deploy gives Compose `LIGHTHOUSE_DEPLOY_COMMIT` and `LIGHTHOUSE_DEPLOY_VERSION`.
- Tests for every package, against real Postgres, Compose and Docker where available, including an end-to-end self-update (`scripts/e2e-self-update.sh`); CI on every push. `scripts/db/` holds the one-time Admin setup on sparkdb.

### Changed
- **The CLI** is a separate process, following the server's CLI conventions (like Cove's): `docker exec -it lighthouse /lighthouse shell`, or one command at a time. Grouped `help`, `help <command>`, guides; line editing, history, Tab completion; data on stdout, messages on stderr; `(y/N)` confirmations with `--yes`; non-zero exit status on failure. No more `docker attach`.
- Run modes like Cove's: `lighthouse` (daemon and CLI, for local use), `serve` (the daemon, in Docker), `shell`.
- Commands take project names and act only on watched projects. `add <url>` names a project after its repository (`--name` only when that's taken). `rebuild` is now `deploy`; `change` / `update url` / `update name` are `set-url` and `rename`. `remove` also takes the containers down (`--keep` leaves them).
- `status` reports Lighthouse's health and every service, and exits non-zero with one "Needs attention" line when something does.
- Errors say where they come from (GitHub with the request and the usual cause, Docker, the database, Cove) and how to fix it.
- Settings: `COVE_ADDRESS` is `COVE_URL`; new `APP_ENV`, `LIGHTHOUSE_POLL_INTERVAL`, `LIGHTHOUSE_CONTROL_SOCKET`, `STORAGE_PATH`, `BACKUP_PATH`. `STAGING_PATH` is mounted at the same path on the host. The `.env` file is only for local development.
- Docker: no published port, no TTY, no `.env` mount; a healthcheck; Go 1.27.1, `alpine:3.24`, buildx; the Docker SDK is `github.com/moby/moby/client`. Logs are structured (`log/slog`).
- A `${KEY}` with a default is a setting, not a secret. Projects never see Lighthouse's own environment.

### Fixed
- A failed deploy no longer takes the project down, and no longer retries forever.
- One failing project no longer stops the others from being checked; unexpected GitHub answers no longer crash Lighthouse; the CLI and the check loop no longer race.
- `$${...}` is no longer mistaken for a secret; files keep their executable bit; downloads have a deadline and are the exact commit that was checked.
- Startup waits for Cove and the database instead of running half-configured, and explains every failure in one line.
- `govulncheck` is clean.

### Removed
- `repos.json` as the store (imported once with `lighthouse import`), the download folder, `APP_REPO_PATH`, port 2000, starting Cove and every project at boot (restart policies do that), dead code and old notes.
