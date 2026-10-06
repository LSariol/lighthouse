# Changelog

All notable changes to Lighthouse. Versions follow [semantic versioning](https://semver.org).

## [Unreleased] — v1.0.0 in progress

### Changed
- **The CLI** is a separate process: `docker exec -it lighthouse /lighthouse shell` for the prompt, or one command at a time (`docker exec lighthouse /lighthouse status`). It follows the server's CLI conventions: grouped `help`, `help <command>` with usage, flags and examples, guides (`help setup`, `help failed`), line editing, history, Tab completion, data on stdout and messages on stderr, `(y/N)` confirmations with `--yes`, non-zero exit status on failure. No more `docker attach`.
- Commands take project names (case-insensitive) and act only on watched projects. `rebuild` is now `deploy` (the old name still works); `change` / `update url` / `update name` are `set-url` and `rename`.
- `status` reports Lighthouse's health (startup, Cove, GitHub token, automatic deploys) and every project's state, and exits non-zero when something needs attention.
- Settings: `COVE_ADDRESS` is now `COVE_URL`; new `APP_ENV`, `LIGHTHOUSE_VERSION`, `LIGHTHOUSE_POLL_INTERVAL`, `LIGHTHOUSE_CONTROL_SOCKET`. The `.env` file is optional (local development only).
- Docker: no published port, no TTY, no `.env` mount; Go 1.27.1; `alpine:3.24`; the Docker client is `github.com/moby/moby/client`.
- Logs are structured (`log/slog`) with timestamps.

### Fixed
- One failing project no longer stops the others from being checked.
- Unexpected GitHub answers no longer crash Lighthouse.
- The CLI and the check loop no longer race: the watchlist is locked, one scan and one deploy run at a time.
- Startup waits for Cove instead of running with an empty GitHub token, checks Lighthouse's Cove token, and explains every failure in one line.
- A project's last error is cleared after a successful check.
- The staging and download folders can't be set to `/` or a system folder.
- `govulncheck` is clean (the old `docker/docker` and OpenTelemetry advisories are gone).

### Removed
- Dead code, the commented-out orchestrator, old notes, and the unused `lighthouse.example.yaml` manifest sketch.
- Starting Cove and every project at boot (Docker's restart policies do that).
