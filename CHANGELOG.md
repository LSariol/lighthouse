# Changelog

All notable changes to Lighthouse. Versions follow [semantic versioning](https://semver.org).

## [Unreleased] — v1.0.0 in progress

### Changed
- **The CLI** is a separate process: `docker exec -it lighthouse /lighthouse shell` for the prompt, or one command at a time (`docker exec lighthouse /lighthouse status`). It follows the server's CLI conventions: grouped `help`, `help <command>` with usage, flags and examples, guides (`help setup`, `help failed`), line editing, history, Tab completion, data on stdout and messages on stderr, `(y/N)` confirmations with `--yes`, non-zero exit status on failure. No more `docker attach`.
- Run modes match Cove: `lighthouse` alone runs the daemon with its CLI on the terminal (local use; `exit` stops it), `lighthouse serve` the daemon only (Docker), `lighthouse shell` a separate prompt.
- Commands take project names (case-insensitive) and act only on watched projects. `rebuild` is now `deploy` (the old name still works); `change` / `update url` / `update name` are `set-url` and `rename`.
- `status` reports Lighthouse's health (startup, Cove, GitHub token, automatic deploys, database, schema) and every project's state, and exits non-zero with one "Needs attention: …" line when something does.
- `add <url>` names the project after the repository; `--name <name>` is only for when that name is taken, and on a terminal `add` asks for another name instead.
- Output matches Cove's CLI: `Key:  value` lines, absolute times (`2006-01-02 15:04`), a count after lists, quoted names in messages, `<Verb> cancelled.`, and no shell banner.
- Settings: `COVE_ADDRESS` is now `COVE_URL`; new `APP_ENV`, `LIGHTHOUSE_VERSION`, `LIGHTHOUSE_POLL_INTERVAL`, `LIGHTHOUSE_CONTROL_SOCKET`. The `.env` file is optional (local development only).
- Docker: no published port, no TTY, no `.env` mount; Go 1.27.1; `alpine:3.24`; the Docker client is `github.com/moby/moby/client`.
- Logs are structured (`log/slog`) with timestamps.
- Internal layout, one job per package: `orchestrator` (when to deploy), `deploy` (how), `watchlist` (the projects and their state), `github`, `docker`, `cove` (startup connection). Each is tested on its own.
- Local development state lives in `.dev/` (gitignored) instead of `config/` and `Server/`.
- `APP_REPO_PATH` and the `repos.json` mount are gone.
- `DOWNLOAD_PATH` and its mount are gone; `STAGING_PATH` holds each running version's files and is mounted at the same path on the host (`/srv/server/staging`). The compose file gives Lighthouse `stop_grace_period: 2m`.
- A `${KEY}` with a default is a setting, not a secret, and isn't fetched from Cove. Projects no longer see any of Lighthouse's own environment.

### Fixed
- A failed deploy no longer takes the project down, and no longer retries forever.
- `$${...}` (Compose's escaped `$`) is no longer mistaken for a secret; files keep their executable bit; downloads have a deadline and are the exact commit that was checked.
- One failing project no longer stops the others from being checked.
- Unexpected GitHub answers no longer crash Lighthouse.
- The CLI and the check loop no longer race: the watchlist is locked, one scan and one deploy run at a time.
- Startup waits for Cove instead of running with an empty GitHub token, checks Lighthouse's Cove token, and explains every failure in one line.
- A project's last error is cleared after a successful check.
- The staging and download folders can't be set to `/` or a system folder.
- `govulncheck` is clean (the old `docker/docker` and OpenTelemetry advisories are gone).

### Added
- **Release mode and tiers, set in the compose file.** An `x-lighthouse` block (`deploy: releases`, `tier: data|infra|app`, `backup: postgres`) is read from the default branch. A release-mode project deploys its newest version tag, and only one newer than any deployed before, so going back by hand sticks. `deploy <name> <version>` deploys a tag.
- **One deploy at a time, in order:** data, then infra, then apps, for checks, the CLI and the reconcile loop alike.
- **Backups before deploying a database** (`backup: postgres`): `pg_dumpall` to `BACKUP_PATH` (`/srv/backups`, mounted), the newest 5 kept; a failed backup stops the deploy. The previous version's secrets are fetched before every swap, so a rollback doesn't need Cove.
- **The reconcile loop:** a deployed project that's down on two passes a minute apart is deployed again; `stop` keeps a project down on purpose (until `start`, `restart` or `deploy`).
- **ETags** on GitHub checks (unchanged answers don't count against the rate limit), and growing waits (1 to 30 minutes) after passing failures.
- `list` shows what each project deploys (commits or releases, and its tier) and what's running; migration `00004`.
- **Deploy rules.** Before anything is built, a deploy checks its compose file: no privileged containers, host namespaces, added capabilities or devices; no host paths outside the repository and `/srv/server/storage/<compose project>/` (the Docker socket included; symlinks are followed); no other project's volumes or networks; only its own Cove keys (`<COMPOSE PROJECT>_*`) and `SHARED_*`; no name another project has on `spark`. Exceptions are in `policy.json`, built into the binary. `help rules` lists them.
- **Test stages.** A Dockerfile stage named `test` is built before every deploy, without secrets; failing tests stop the deploy.
- `check <name>`: runs a project's latest commit through the checks and its test stage without deploying it.
- `STORAGE_PATH` (default `/srv/server/storage`), mounted read-only at the same path in `docker-compose.yml`. The image adds `docker-cli-buildx`.
- **Safe deploys.** Build first, swap last: the running version keeps serving while the new one is downloaded, built and given its secrets; `docker compose up` is the only downtime; then every service must come up healthy (or stay up), and if one doesn't, the previous version is put back automatically.
- **Broken projects.** A commit that fails 3 times for a reason retrying can't fix (it doesn't build, a secret is missing, it doesn't start) isn't tried again until a new commit arrives or `retry <name>`. Problems with GitHub, Cove or the network are just retried.
- **Compose projects with several services.** The compose project's name comes from the compose file; its containers are found by Compose's labels. `start`, `stop`, `restart` and `logs` take `<name>` or `<name>:<service>`; `status` shows each service. `remove` also stops and removes a project's containers; `--keep` leaves them running.
- `report <name> [n]`: one deploy step by step, with each step's output (secret values hidden). `history` shows the step a deploy failed at.
- Private repositories: the exact commit is downloaded through the GitHub API.
- Each deploy cleans up after itself: old deploy folders, old rollback images, unused images, week-old build cache.
- **Postgres** (`lighthouse_db` on sparkdb) holds the projects instead of `repos.json`. The schema comes from goose migrations built into the binary and applied at startup as `lighthouse_migrator`; Lighthouse refuses to run on an older schema. The database URLs are read from Cove (`LIGHTHOUSE_DATABASE_URL`, `LIGHTHOUSE_MIGRATOR_DATABASE_URL`).
- **Deploy history**: every attempt is recorded (trigger, result, commit, times, error); `history <name>` shows it.
- `lighthouse migrate [status|up]` and `lighthouse import <file|->` (moves a pre-1.0 `repos.json` into the database; safe to run twice).
- `status` shows the database and schema, and the startup phase while Lighthouse waits for Cove or the database. Lighthouse waits for an unreachable database instead of exiting.
- Integration tests against real Postgres, locally (`scripts/test-db.sh`) and in CI.
- `scripts/db/check.sql` and `scripts/db/setup.sql`: the one-time Admin setup of Lighthouse's roles and database on sparkdb (DOCUMENTATION.md §10.1).

### Removed
- Dead code, the commented-out orchestrator, old notes, and the unused `lighthouse.example.yaml` manifest sketch.
- Starting Cove and every project at boot (Docker's restart policies do that).
