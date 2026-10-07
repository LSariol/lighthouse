# Lighthouse Documentation

The reference for **Lighthouse**, the self-hosted deployer behind every project on the `spark` server: how it works, the rules a project follows, how to run it, and what's left on purpose or for later. It's meant to be lightweight, autonomous and self-sufficient: once set up, projects deploy, recover and update without anyone logging in to the server.

- [README](README.md): a short, friendly overview.
- [Cove](https://github.com/LSariol/Cove): the secret vault Lighthouse reads from. Its `DOCUMENTATION.md` §9 (connecting a project), §10 (bootstrap) and §14 (operations) are the other half of this document.
- [CoveClient](https://github.com/LSariol/CoveClient): the Go library Lighthouse uses to talk to Cove.

This document describes v1.0.0. Sections 1–13 are how Lighthouse works, section 14 the standards every change follows, section 15 its known limits and accepted risks, section 16 what may come later. The history of how it got here is in git and `CHANGELOG.md`.

---

## Contents

1. [Overview](#1-overview)
2. [Quick reference](#2-quick-reference)
3. [Architecture](#3-architecture)
4. [Flows](#4-flows)
5. [Configuration](#5-configuration)
6. [Data: the database](#6-data-the-database)
7. [Connecting a project (the contract)](#7-connecting-a-project-the-contract)
8. [Cove integration](#8-cove-integration)
9. [CLI](#9-cli)
10. [Deploying Lighthouse](#10-deploying-lighthouse)
11. [Operations](#11-operations)
12. [Troubleshooting](#12-troubleshooting)
13. [Security model](#13-security-model)
14. [Standards (mandatory)](#14-standards-mandatory)
15. [Known limits and accepted risks](#15-known-limits-and-accepted-risks)
16. [Future features](#16-future-features)

---

## 1. Overview

Lighthouse keeps the server's projects running the latest code on their default branch. It is one Go program running in a Docker container next to the projects it manages.

Every 10 seconds (configurable) it asks GitHub for the newest commit of each watched repository (or, for a project that deploys releases, its newest version tag). When it changes, it deploys it, **building first and swapping last**:

1. downloads that exact commit and unpacks it,
2. reads its compose file: the compose project's name, its services, the `${KEY}` secrets it needs,
   checks it against the deploy rules and runs its Dockerfile's `test` stage, if it has one,
3. fetches those secrets from Cove in one request,
4. builds the new images, while the running version keeps serving,
5. swaps: `docker compose up` replaces the running containers (the only moment of downtime),
6. checks that every service comes up healthy, and **puts the previous version back** if one doesn't.

The project reads plain environment variables: it needs no Cove code, address or token. Every deploy, with each step's output, is recorded in Lighthouse's database.

**Where it fits:**

```
          GitHub  ◄── checks every 10 s; downloads the exact commit (REST API, token from Cove)
             │
             ▼
   ┌──────────────────┐   GetSecrets(${KEY}...)   ┌─────────┐
   │    lighthouse    │ ────────────────────────► │  cove   │ ──► sparkdb (cove_db)
   │  serve (daemon)  │ ◄──────────────────────── │  :2100  │
   └───┬──────┬───────┘        values             └─────────┘
       │      │      ▲ control socket (CLI: docker exec -it lighthouse /lighthouse shell)
       │      └─────► sparkdb (lighthouse_db): projects, deploy history
       │ /var/run/docker.sock
       ▼
   Host Docker daemon ──► botsuite, marquee, plop, website, cove, ...  (compose projects)
                          on the external `spark` network; public traffic via cloudflared
```

| Property | How |
|---|---|
| Change detection | Polling the GitHub REST API per repo (default every 10 s); no webhooks |
| Source | The default branch's newest commit, or the newest release tag (`x-lighthouse`), downloaded as a tarball through the API, so private repositories work too |
| Build and run | `docker compose -p <project> build`, then `up -d --no-build --remove-orphans`, on the host's Docker through the mounted socket; one deploy at a time |
| Safety | The deploy rules and the project's own tests run first; nothing running is touched until the new version is built; a version that doesn't come up healthy is rolled back; a commit that fails 3 times marks its project **broken** until a new commit or `retry` |
| Autonomy | Projects that go down are brought back; Lighthouse updates itself from its own release tags |
| Secrets | Every `${KEY}` without a default, fetched from Cove in one batch, passed only to `docker compose`; hidden in all output |
| State | Postgres (`lighthouse_db` on sparkdb): the projects, every deploy and its steps |
| Control | `lighthouse serve` is the daemon; the CLI (`shell` or one-shot commands) talks to it through a Unix socket |

---

## 2. Quick reference

On the server. `lh` below stands for `docker exec -it lighthouse /lighthouse`; an alias saves typing: `alias lh='docker exec -it lighthouse /lighthouse'`.

| Task | How |
|---|---|
| Open the prompt | `lh shell` (`exit` or Ctrl-D leaves; Lighthouse keeps running) |
| Is everything healthy? | `lh status` (exits non-zero if something needs attention) |
| Go back to what ran before | `lh rollback <name>`; any tag or commit: `lh deploy <name> <tag or commit>` |
| Watch a new repo | `lh add https://github.com/<owner>/<repo>` (named after the repo; `--name <name>` if that's taken) |
| Stop watching | `lh remove <name>` (also removes its containers), or `lh remove <name> --keep` (leaves them running) |
| See what's watched | `lh list` |
| Deploy now | `lh deploy <name>` (or `deploy all`) |
| How did its deploys go? | `lh history <name>` |
| Why did a deploy fail? | `lh report <name>` (each step with its output) |
| A broken project, after fixing it outside the repo | `lh retry <name>` |
| Check for new commits now | `lh scan` |
| Freeze all automatic work | `lh pause` / `lh resume` (or `pause all` / `resume all`) |
| A project's output | `lh logs <name>` (every service) or `lh logs <name>:<service> [lines]` |
| One service | `lh restart <name>:<service>` (also `start`, `stop`) |
| Lighthouse's own log | `docker logs -f lighthouse` |
| Help | `lh help`, `lh help <command>`, `lh help setup`, `lh help failed`, `lh help rules` |
| Would a project's latest commit deploy? | `lh check <name>` (nothing is changed) |
| Add a secret a project needs | In Cove: `create <PROJECT>_<PLATFORM>_<TYPE> <value>`, then reference it as `${...}` in the project's compose file |
| Give Lighthouse a new Cove token | Delete `/srv/server/storage/lighthouse/cove/token`, `bootstrap open lighthouse` in Cove, `docker restart lighthouse` |
| Change the GitHub token | In Cove: `update LIGHTHOUSE_GITHUB_TOKEN <token>`, then `docker restart lighthouse` (it's read once at startup) |
| Database migrations | `lh migrate status`; they're applied automatically at startup |

---

## 3. Architecture

```
cmd/lighthouse/main.go     Modes (plain, serve, shell, one command, health, self-update,
                           migrate, import, version), the startup order, and the wiring
policy.go, policy.json     The deploy rules' exceptions, built into the binary (§7.1)
internal/
  config/                  Every setting, read from the environment once and validated
  cli/                     The command table, help and guides, the shell (line editing,
                           history, Tab completion), one-shot commands, output helpers
  control/                 The CLI ↔ daemon protocol: Service (what the CLI can ask), Serve
                           (HTTP over a Unix socket) and Client (the CLI's side)
  daemon/                  Implements control.Service on the parts below; resolves
                           "<project>[:<service>]"; turns errors into messages that say how
                           to fix them; the startup phase
  orchestrator/            When projects deploy: checks (ETags, release tags, settings),
                           the turn (one deploy at a time, by tier), waits after failures,
                           Deploy, Rollback, Retry, Check, the reconcile loop, self-update
                           results; records every check and deploy in the store
  deploy/                  How a project deploys: deploy.go (the steps, backup, rollback),
                           source.go (unpacking), verify.go (health, cleanup), scrub.go
                           (hiding secrets), selfupdate.go (the hand-off and the helper)
  policy/                  The deploy rules (§7.1)
  settings/                The x-lighthouse block (§7.2)
  release/                 Which tags are releases, and which is newest
  compose/                 Runs docker compose (config, variables, build, up, down) and a
                           few docker commands, with deadlines and a clean environment
  docker/                  Containers by compose label, their state, output and names on a
                           network; image tags (Docker SDK)
  projects/                Project, Deployment, Step, the Store interface, name rules
    projectstest/          An in-memory Store, and the tests every Store must pass
  database/                Postgres: the pool, goose migrations (migrations/, built in), the
                           schema check, and the Store
  reposjson/               Reads a pre-1.0 repos.json, for `lighthouse import`
  github/                  Repository URLs, commits, tags, a compose file, a commit's tarball
  cove/                    Connecting to Cove at startup; Lighthouse's own secrets
scripts/
  db/                      Admin SQL for sparkdb, run by hand (§10.1); CI uses it too
  test-db.sh               A throwaway Postgres for the integration tests (§13.1)
  e2e-self-update.sh       The self-update end to end against the local Docker (§4.6)
Dockerfile                 golang:1.27.1-alpine → alpine:3.24 + docker-cli, compose, buildx
docker-compose.yml         Lighthouse's own service: mounts, healthcheck, x-lighthouse
.github/workflows/ci.yml   gofmt, vet, tests with -race (Postgres, Docker), govulncheck, builds
```

**Dependencies point one way:**

```
main ─► cli ─► control
main ─► daemon ─► orchestrator ─► projects (Store) ◄── database
           │            │       └─► github, settings, release
           │            └─(Deployer)─► deploy ─► compose, docker, github, policy, settings, CoveClient
           └─► docker, compose, github, projects
main ─► cove ─► CoveClient          main ─► reposjson ─► projects, github
```

`cli` only knows `control.Service`. `orchestrator`, `daemon` and `deploy` only know interfaces (`projects.Store`, `Deployer`, `GitHub`, `Compose`, `Docker`, `Source`, `Secrets`, `Containers`, `Health`), each tested with a fake in place of the real thing. The in-memory Store passes the same tests as the Postgres one (`projectstest.RunStoreTests`). `compose`, `docker`, `database` and one end-to-end `deploy` test also run against the real things when they're available. Only `config` reads Lighthouse's environment; `compose` passes a project only what docker needs, plus its secrets.

**Where things go:**
- A new CLI command → a `cli/cmd_*.go` function plus one entry in `commandTable()`; help and completion pick it up. If it asks the daemon for something new: a `control.Service` method, its route in `control/server.go`, a `Client` method, and the `daemon` implementation.
- What happens when → `orchestrator`. How a deploy is done → `deploy`.
- Something stored → a `projects.Store` method, implemented in `database/store.go` and `projectstest/store.go`, with a case in `projectstest/contract.go`. A schema change → a new migration ([§14.1](#141-databases-and-migrations)).
- A setting → a field in `config.Config`, read in `Load`, checked in `ValidateServe`. A secret → Cove, read in `cove.ReadSecrets`.

**State and concurrency:** all state that matters is in Postgres, so the CLI, the check loop and `lighthouse import` always see the same thing. Only conveniences live in memory and start over with a restart: the last ETags, the waits after failures, and `pause`. A scan runs at most once at a time (a second is refused); deploys run one at a time, by tier ([§4.2](#42-the-check-loop)), whether they come from the check loop, the CLI or the reconcile loop.

---

## 4. Flows

### 4.1 Startup (`lighthouse serve`)

1. **Config.** `config.Load` reads the environment, plus a `.env` file if one exists (local development only: `APP_ENV_PATH`, else `./.env`). `ValidateServe` stops startup when a required setting is missing, `COVE_URL` has no scheme, the poll interval is under 5 s, or `STAGING_PATH` is `/`, `.` or a system folder. Every startup error is one line, `lighthouse: <message>`, with exit status 1.
2. **Logging.** `log/slog` text lines with timestamps on stderr (`docker logs lighthouse`), plus each deploy step's output, prefixed `[<project> <step>]`.
3. **Docker client** from the environment (`/var/run/docker.sock`); the API version is negotiated.
4. **Control socket.** The CLI can connect from here on; `status` shows the startup phase, and everything else answers "still starting".
5. **Cove** (phase `waiting for Cove`), retried until it works:
   - `LoadOrBootstrap(COVE_TOKEN_PATH)` reads the token file, or fetches a token through Cove's bootstrap endpoint the first time and saves it (mode 0600, atomically).
   - While the endpoint is closed, or Cove is unreachable, it logs a warning and retries every 15 s.
   - `WaitForReady` waits until Cove and its database answer; `Auth` checks the token. A rejected token stops startup with the fix: delete the file, `bootstrap open lighthouse`.
6. **Secrets.** One `GetSecrets` batch reads `LIGHTHOUSE_GITHUB_TOKEN`, `LIGHTHOUSE_DATABASE_URL` and `LIGHTHOUSE_MIGRATOR_DATABASE_URL`. If any is missing, startup stops and names every missing key.
7. **Database** (phase `migrating the database`):
   - Pending migrations are applied as `lighthouse_migrator` (acting as `lighthouse_owner`), under a Postgres lock so two Lighthouses can't migrate at once.
   - The pool connects as `lighthouse_app`, and the schema check refuses a database missing a migration this build needs.
   - While the database is unreachable (sparkdb restarting), the phase is `waiting for the database` and it retries every 15 s. Any other database error (a wrong password, a failed migration) stops startup.
8. **Running.** Lighthouse finds its own container (for self-update) and records any self-update the helper finished ([§4.6](#46-self-update)). `status` says `running`. The first check starts at once, then one every `LIGHTHOUSE_POLL_INTERVAL`; the reconcile loop runs every minute.
9. **Stopping** (`docker stop`, SIGTERM). Lighthouse stops answering the CLI and stops starting new work. A deploy that has already swapped finishes its check (and rollback, if needed); Lighthouse waits up to 100 s for it (`stop_grace_period: 2m` in the compose file), then closes the database and removes the socket. A deploy still building is cancelled, which changes nothing running.

A fatal error after the socket is open (for example a revoked Cove token) exits with status 1. Docker's `restart: unless-stopped` starts Lighthouse again, so the same one-line message repeats in `docker logs` until it's fixed.

### 4.2 The check loop

Unless paused, every `LIGHTHOUSE_POLL_INTERVAL`, `Scan` checks each project in turn, **data projects first, then infra, then apps** (their `x-lighthouse` tier, [§7.2](#72-lighthouse-settings-x-lighthouse)):

```
skip it if it was stopped on purpose (stop <name>), or is waiting after a passing failure

GET /repos/<owner>/<repo>/commits?per_page=1   (If-None-Match: the last ETag)
  ├─ 304 Not Modified → the same commit as last time (costs nothing against the rate limit)
  ├─ error            → that project's last error, and a wait (below); go on to the next one
  └─ sha = the newest commit on the default branch

the branch moved → read its compose file (GET …/contents/compose.yaml?ref=<sha>) for x-lighthouse

branch mode (the default):
  sha == the deployed commit → nothing to do
  otherwise                  → deploy sha
release mode (deploy: releases):
  GET /repos/<owner>/<repo>/tags (If-None-Match) → the newest plain version tag, v1.2.3 or 1.2.3
  newer than every release deployed before → deploy it; otherwise nothing
  (a deployed release whose tag moved to another commit isn't redeployed; a warning is logged)

project broken and it's the failing commit → not tried again (no error for the scan)
record the check (time, count, and the error or none)
```

**One deploy at a time, in order.** Checks, the CLI and the reconcile loop all wait for one turn: the next to go is the lowest tier (data, infra, apps), then whoever asked first. So when sparkdb has to deploy or come back, it goes before the next app; and while an infrastructure project deploys, nothing else does until it's verified healthy.

**The reconcile loop.** Every minute (unless paused), each deployed project that isn't stopped on purpose is looked at in Docker. One that's **down**, with no containers at all or none running while a long-running service (one with a restart policy) is stopped, on **two passes in a row** (so one Docker is restarting isn't mistaken for one that's gone), is deployed again at its deployed commit, trigger `reconcile`. Data projects come back first, each waiting until it's healthy. Docker's restart policies still handle crashes and reboots; this covers what they can't, such as removed containers. Stopping one service (`stop <name>:<service>`) isn't the project being down.

A scan that had failures logs one line naming the projects; `list` and `status` show each project's last error, `history <name>` every deploy, `report <name>` one deploy step by step.

**Failures and the broken state.** Each failed deploy is one of two kinds:

| Kind | What it means | Examples | What happens next |
|---|---|---|---|
| Transient | Something around the commit had a problem; trying again may work | GitHub, Cove or the network unreachable or erroring, Docker unreachable, a backup that failed | Automatic deploys of that project **wait** 1 minute, then 2, 4, … up to 30 minutes between tries; doesn't count. A success, or `deploy`, ends the wait |
| Permanent | The commit itself has a problem | The compose file is invalid, a secret is missing or forbidden, the build fails, a service doesn't come up | Counted per commit. After **3** in a row for the same commit the project is **broken**: that commit isn't tried again until a new commit arrives or `retry <name>` |

The waits are kept in memory: a restart of Lighthouse starts them over.

`deploy <name>` always deploys, broken or not. `retry <name>` clears the count and deploys. A success clears everything.

### 4.3 The deploy pipeline (`deploy.Deployer.Deploy`)

Deploys run one at a time; a second waits for the first. Each step has a deadline; its output (the last 16 KB, secret values replaced by `[secret]`) is kept with the deployment and written to Lighthouse's log as it happens.

| Step | What happens | The running version | If it fails |
|---|---|---|---|
| `fetch` (2 min) | Downloads the exact commit as a tarball through the GitHub API (works for private repositories); unpacks it into `<STAGING_PATH>/.incoming/<commit>`, keeping file modes, refusing paths and links that leave the folder, at most 2 GB | Serving | Transient for GitHub or network trouble; permanent for a missing commit or a bad archive |
| `inspect` | `docker compose config` reads the compose project's name (its `name:`, or, without one, the repository's name lowercased) and its services; the project claims the name (two projects can't share one); the files move to `<STAGING_PATH>/<compose project>/<commit>` and the config is read again there (its paths are absolute); `config --variables` lists the `${...}` it uses | Serving | Permanent |
| `check` | The deploy rules ([§7.1](#71-the-deploy-rules)): nothing that reaches outside the project, its own secrets only, no name another project has on `spark`; exceptions from `policy.json`. Each finding is listed in the step's output | Serving | Permanent (transient if Docker can't list the names on `spark`) |
| `test` (20 min) | For each Dockerfile with a stage named `test` (`FROM … AS test`): `docker build --target test`, with nothing from the compose file (no secrets, no build arguments). None: nothing runs | Serving | Permanent |
| `secrets` | Every variable **without a default** is fetched from Cove in one batch; one with a default is a setting and isn't fetched (a warning says so) | Serving | Permanent for a missing, forbidden or invalid key; transient for Cove trouble |
| `build` (20 min) | The running images are tagged `<image>:lh-<commit>` for rollback; `docker compose -p <project> build`; the new images get their `lh-<commit>` tag | Serving | Permanent |
| `backup` | Fetches the previous version's secrets (for a rollback that doesn't need Cove). With `x-lighthouse` `backup: postgres`, dumps the running database to `BACKUP_PATH` ([§7.2](#72-lighthouse-settings-x-lighthouse)) | Serving | Transient |
| `swap` (5 min) | `docker compose -p <project> up -d --no-build --remove-orphans`: Compose replaces the containers. **The only downtime** | Replaced | Permanent; rolls back |
| `verify` (2 min) | Every service must be up: healthy if it has a healthcheck; else running, without restarting, for 10 s; a one-off service (no restart policy) may also have exited with 0. Unhealthy, crashed or restarting fails at once | New version | Permanent; rolls back |
| `cleanup` | Keeps the new folder and the one it replaced; removes other deploy folders, rollback tags of other commits, images nothing uses, and (daily) build cache unused for a week. Never fails a deploy | New version | Only reported |

**Rollback** (after a failed `swap` or `verify`): the previous images are put back under their usual names, the previous version's secrets (fetched before the swap) are given back, and `docker compose up` runs from the previous version's folder. The deployment is recorded as `rolled_back`. It isn't possible on a project's first deploy (nothing ran before), or when the previous version wasn't deployed by this Lighthouse (its folder isn't kept); then the deployment is `failed` and says so.

From `swap` on, a deploy isn't cut off by Lighthouse stopping: a half-replaced project is worse than a late shutdown.

### 4.4 Secret injection, exactly

- **What counts as a secret:** every `${KEY}` in the compose file **without a default**, as `docker compose config --variables` reports it. `${KEY:-default}` is a setting and isn't fetched (it gets its default). `$${KEY}` is Compose's escape for a literal `$`, not a variable. `$KEY` without braces is a variable like `${KEY}`.
- **Lookup:** the key is used exactly as written. Cove keys are case-sensitive.
- **All or nothing:** if any key is missing, `GetSecrets` fails naming every missing key, and the deploy stops before anything changes. If Lighthouse's token can't read a key, Cove answers `403 forbidden_key` without naming it (`docker logs cove` names it).
- **Delivery:** values are passed to `docker compose build` and `up` as `KEY=value` in their environment, which otherwise holds only what docker needs (`PATH`, `HOME`, `DOCKER_*`): none of Lighthouse's own settings reach a project. Compose substitutes them into the file in memory. They never touch disk on Lighthouse's side. They do end up in the running container's environment, where `docker inspect` can see them, as any environment variable does.
- **In output:** every fetched value (4 characters or more) is replaced by `[secret]` in each step's stored output, in Lighthouse's log, and in error messages.
- **Rule from Cove's standard:** only real secrets go in `${...}`. A plain setting is written as a value (`COVE_URL=http://cove:2100`).

### 4.5 A CLI command

```
lighthouse <command>  or  the shell
  → cli: command table → argument checks, confirmation (y/N) for destructive commands
  → control.Client: HTTP over the Unix socket (LIGHTHOUSE_CONTROL_SOCKET)
  → daemon: resolves the project (and service), calls the store, orchestrator or Docker
  → JSON answer: data, or an error with a kind and a message that says how to fix it
  → cli: data on stdout, messages on stderr (✓ ! ✗ ?), exit status 0 or 1
```

`deploy`, `rollback`, `retry` and `scan` wait until they're done (a deploy can take minutes; progress is in `docker logs -f lighthouse`). While Lighthouse is still starting, they're refused with the reason.

### 4.6 Self-update

Lighthouse deploys itself like any project (its compose file says `x-lighthouse: {deploy: releases, tier: infra}`), with one difference at the swap: `docker compose up` would stop the very process doing it. So the deploy runs as usual up to the swap (fetch, inspect, check, test, build, backup), building the new image while the current Lighthouse keeps working, and then **hands off**:

1. **Hand-off** (step `handoff`): Lighthouse writes what the swap needs to `<STAGING_PATH>/.self-update/<commit>.json` (no secrets: its compose file may not use any) and starts the **update helper**, a container called `lighthouse-updater` from the **new** image, running `lighthouse self-update <file>` with the Docker socket and the staging folder. This Lighthouse then takes no more work: it keeps the deploy turn and stops checking.
2. **Swap and verify** (in the helper): `docker compose up` replaces Lighthouse; the helper waits until the new container is **healthy** (its healthcheck, `lighthouse health`, passes once the daemon answers on its control socket, even while it waits for Cove or the database).
3. **Roll back** if it isn't healthy in time: the previous image is put back and started from the previous version's folder (or, the first time after a deploy by hand, from the new folder with the old image).
4. **Record:** the helper writes the result into the file and exits. Whichever Lighthouse runs now (the new one, or the old one after a rollback) records it in `history` at startup and on every scan. A hand-off the helper never finished is recorded as failed after 30 minutes.

`deploy lighthouse` answers "Lighthouse is updating itself…" and the connection ends with the old container. `docker logs -f lighthouse-updater` shows the helper's work; its stopped container stays until the next update removes it. **Migrations must be additive** ([§14.1](#141-databases-and-migrations)): after a rollback, the old version runs on the database the new one migrated.

`scripts/e2e-self-update.sh` runs all of this against the local Docker: a real hand-off, swap, and a rollback of a version that never becomes healthy.

---

## 5. Configuration

All settings are environment variables; Lighthouse's secrets (the GitHub token and both database URLs) come from Cove instead ([§8](#8-cove-integration)). In Docker they're written out in `docker-compose.yml`'s `environment:` (none of them is a secret). For local development they can come from a `.env` file ([§13.1](#131-running-locally)).

| Variable | Required for `serve` | Description |
|---|---|---|
| `COVE_URL` | Yes | Cove's base URL, `http://cove:2100` in Docker |
| `COVE_TOKEN_PATH` | Yes | Where Lighthouse's Cove token is kept. Docker: `/app/vault/cove/token` |
| `STAGING_PATH` | Yes | The deploy folders: `<compose project>/<commit>`, the running version and the one before it. Lighthouse owns everything in it, so it can't be `/`, `.` or a system folder. **In Docker it must be mounted at the same path on the host and in the container** (`/srv/server/staging`), so a relative bind mount in a project's compose file means the same files to Compose and to Docker |
| `STORAGE_PATH` | No | The projects' data folders, default `/srv/server/storage`: a project may mount host paths from `<STORAGE_PATH>/<compose project>/` ([§7.1](#71-the-deploy-rules)). **In Docker, mount it read-only at the same path**, so the rules can follow a symlink a container left in its folder; without it, Lighthouse logs a warning at startup |
| `BACKUP_PATH` | No | Database backups taken before a deploy (`x-lighthouse` `backup: postgres`), default `/srv/backups`: `<compose project>/<project>-<time>-<commit>.sql.gz`. **In Docker, mount it at the same path** (read-write) |
| `APP_ENV` | No | `dev` or `prod`, shown in the shell's prompt (`lighthouse (prod)>`, prod in red) and by `status` |
| `LIGHTHOUSE_VERSION` | No | The version `status` and `version` report. Set in the compose file at release; without it, `dev` (plus the commit for a local build) |
| `LIGHTHOUSE_POLL_INTERVAL` | No | How often GitHub is checked, e.g. `30s`, `1m`. Default `10s`, minimum `5s` |
| `LIGHTHOUSE_CONTROL_SOCKET` | No | Where the daemon listens for the CLI. Default `/run/lighthouse/control.sock` (inside the container; not mounted) |
| `APP_ENV_PATH` | No | Local development: the `.env` file to load instead of `./.env` |

A malformed value stops startup with a message naming the setting.

**Files and mounts** (from `docker-compose.yml`, server side):

| Host path | Container path | Contents |
|---|---|---|
| `/srv/server/storage/lighthouse/cove/` | `/app/vault/cove/` | The Cove token file (`token`, mode 0600) |
| `/srv/server/staging` | `/srv/server/staging` (the same path) | Deploy folders |
| `/srv/server/storage` | `/srv/server/storage` (the same path, read-only) | The projects' data folders, read to check symlinks |
| `/srv/backups` | `/srv/backups` (the same path) | Database backups (`BACKUP_PATH`); must exist before Lighthouse starts |
| `/var/run/docker.sock` | `/var/run/docker.sock` | Full control of the host's Docker (effectively root) |

No port is published and no terminal is attached. `stop_grace_period: 2m` gives a deploy in progress time to finish its check. Lighthouse joins the external `spark` network so it can reach `cove` and `sparkdb`.

---

## 6. Data: the database

Lighthouse's state is in `lighthouse_db` on sparkdb, schema `lighthouse`. Roles, the database and ownership are set up by hand once ([§10.1](#101-database-setup-once-by-hand)); everything inside the schema comes from the goose migrations in `internal/database/migrations/` ([§14.1](#141-databases-and-migrations)).

**`projects`**: one row per watched repository.

| Column | Notes |
|---|---|
| `id` | Identity; never shown |
| `name` | The name in the CLI: letters, digits, `-`, `_`, up to 64. Unique without regard to case |
| `repo_owner`, `repo_name` | `https://github.com/<owner>/<name>`; unique without regard to case |
| `compose_project` | The compose project, as its compose file names it, learned at each deploy; unique, so two projects can't share one. Empty before the first deploy (the repository's name, lowercased, is used meanwhile) |
| `created_at`, `updated_at` | When Lighthouse started watching it; when it was last renamed or pointed elsewhere |
| `deployed_sha`, `deployed_version`, `deployed_at` | The last **successful** deploy, and the release it was (release mode) |
| `highest_version` | The highest release ever deployed successfully: checks only deploy a newer one |
| `deploy_mode`, `tier` | The `x-lighthouse` settings as last read: `branch`/`releases`, `data`/`infra`/`app` |
| `stopped` | Stopped on purpose (`stop <name>`): not deployed or brought back until `start`, `restart` or `deploy` |
| `last_checked_at`, `check_count` | The last check of GitHub |
| `last_error`, `last_error_at` | The last problem (a check or a deploy), cleared by the next success. Cut at 2,000 characters |
| `failure_count`, `failing_sha`, `broken` | Permanent failures in a row of one commit; at 3, `broken` |

**`deployments`**: one row per deploy attempt, written when it ends.

| Column | Notes |
|---|---|
| `project_id` | Its project; the rows go when the project is removed |
| `sha`, `version` | The commit deployed, and the release it is (release mode) |
| `trigger` | `check` (something new found by a check or `scan`), `manual` (`deploy`, `retry`) or `reconcile` (brought back by the reconcile loop) |
| `status` | `succeeded`, `failed` (nothing changed) or `rolled_back` (the previous version was put back) |
| `failure_kind`, `failed_step` | For a failure: `transient`/`permanent`, and the step |
| `started_at`, `finished_at`, `error` | |

**`deployment_steps`**: each step of a deployment: `position`, `step`, `status` (`succeeded`, `failed`, `skipped`), times, and `log`, the last 16 KB of its output with secret values hidden. `report <name>` shows them.

**`goose_db_version`**: which migrations have run.

`projects.held_sha` (migration `00005`): the branch's newest commit when someone went back to an older one; checks don't deploy it again.

**Who may do what** (migrations `00002`–`00004`): `lighthouse_app` reads and changes `projects`, but can only **add** to `deployments` and `deployment_steps`, never change or delete history, and only reads `goose_db_version`; it can't create anything. `lighthouse_reader` reads everything (pgAdmin). Everything is owned by `lighthouse_owner`.

**From the pre-1.0 Lighthouse:** `lighthouse import` reads its `repos.json` and adds each project with its state: when watching started, the deployed commit, checks, last error. It's safe to run twice, because existing projects are skipped. Steps in [§10](#10-deploying-lighthouse).

---

## 7. Connecting a project (the contract)

What a repository needs for Lighthouse to deploy it. `lighthouse help setup` is the short version.

**Required**

1. **A compose file at the repository's top** (`compose.yaml`, `compose.yml`, `docker-compose.yaml` or `docker-compose.yml`). Lighthouse runs **every service** in it.
2. **The deployable code on the default branch** (usually `main`). Lighthouse deploys its newest commit. Private repositories work, as long as Lighthouse's GitHub token can read them.
3. **Every `${KEY}` without a default exists in Cove** under exactly that name, following the naming standard `PROJECT_PLATFORM[_ROLE]_TYPE` ([§14.2](#142-secrets-cove)), where `PROJECT` is the compose project in capitals, or `SHARED` ([§7.1](#71-the-deploy-rules)). Plain settings are written as values, never `${...}`.
4. **Persistent data in the compose project's folder** (`/srv/server/storage/<compose project>/...:/app/data`) or a named volume. Nothing else on the host ([§7.1](#71-the-deploy-rules)).
5. **Nothing the deploy rules refuse** ([§7.1](#71-the-deploy-rules)), unless `policy.json` has an exception. A relative bind mount (`./config.yml:/app/config.yml`) works for files **in the repository**, since each version's folder is kept while it runs, but anything written there is gone with the next version.

**Recommended**

6. **`name:` at the top of the compose file.** It's the compose project's name, which Lighthouse finds the containers by. Without it, the repository's name (lowercased) is used. Two Lighthouse projects can't share one: the second's deploy is refused, naming the first.
7. **A healthcheck on every service that serves something.** Lighthouse waits until it's healthy, and rolls back if it isn't. Without one, a service only has to stay running for 10 seconds.
8. **A `restart:` policy** (`unless-stopped`) on long-running services. A service without one is treated as a one-off job, which may exit with 0.
9. **Unique names on `spark`.** Docker's DNS on a shared network answers every container that has a name: a service called `web` in two projects on `spark` makes `http://web` reach either, at random. A deploy whose service name, `container_name` or alias on `spark` is another project's is refused. Compose adds every service's own name to its networks too, so a service called `web` on `spark` clashes with any other project's `web`.
10. **Join the external `spark` network** to reach Cove, sparkdb or each other (`sparkdb:5432`, `cove:2100`).
11. **A `test` stage in the Dockerfile** (optional): `FROM … AS test` is built before every deploy, with no secrets; if it fails, nothing is deployed. Put the project's tests, linters and scanners there.

**Minimal example** (a site with a web service and a worker):

```yaml
name: website

services:
  web:
    build: .
    container_name: website-web           # unique on spark; cloudflared routes to http://website-web:3000
    restart: unless-stopped
    environment:
      - DATABASE_URL=${WEBSITE_DATABASE_URL}   # secret: fetched from Cove
      - TMDB_API_KEY=${SHARED_TMDB_API_KEY}    # shared secret
      - LOG_LEVEL=info                         # plain setting: a value, not ${...}
    volumes:
      - /srv/server/storage/website/uploads:/app/uploads   # data: an absolute host path
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:3000/health"]
      interval: 10s
      retries: 3
    networks: [spark]

  bot:
    build: ./bot
    container_name: website-bot
    restart: unless-stopped
    networks: [spark]

networks:
  spark:
    external: true
```

Added as `add https://github.com/lsariol/landing`, it's `landing` in the CLI, `website` to Compose (so its keys are `WEBSITE_*` and its data is in `/srv/server/storage/website/`), and `logs landing:bot` shows the bot.

**A project that writes to Cove** (today only botsuite) also gets `COVE_URL=http://cove:2100` and `COVE_TOKEN=${<PROJECT>_COVE_TOKEN}` and uses CoveClient v1. See Cove's `DOCUMENTATION.md` §9.

**Cove and sparkdb** are deployed by Lighthouse too, as infrastructure ([§7.2](#72-lighthouse-settings-x-lighthouse)): their secrets are fetched before anything is touched, and they keep running until their new version is built.

### 7.1 The deploy rules

Lighthouse deploys whatever is on `main` with full control of the server's Docker, so before anything is built, every deploy's compose file is checked (the `check` step; [§4.3](#43-the-deploy-pipeline-deploydeployerdeploy)). The rules are the same for every project, so adding one needs no setup on the server. These stop a deploy; the ID in brackets is what an exception names:

| Refused | ID |
|---|---|
| `privileged: true` | `privileged` |
| Sharing the host's namespace, or another container's: `network_mode`, `pid`, `ipc`, `uts`, `userns_mode`, `cgroup` set to `host` or `container:<x>` | `pid:host`, `network_mode:container:cove`, … |
| `cap_add` (any capability; `cap_drop` is fine) | `cap_add:NET_ADMIN` |
| `devices`, `device_cgroup_rules` | `device:/dev/ttyUSB0`, `device_cgroup_rule:<rule>` |
| A `security_opt` that turns a protection off (`seccomp=unconfined`, `apparmor=unconfined`, `label=disable`; `no-new-privileges` is fine) | `security_opt:seccomp=unconfined` |
| `volumes_from: container:<x>` | `volumes_from:container:cove` |
| A host path outside the repository and `/srv/server/storage/<compose project>/`: bind mounts (the Docker socket included), `env_file`, file-based `secrets` and `configs`, build contexts and Dockerfiles, a volume bound to a host folder. `/etc/localtime` and `/etc/timezone` may be mounted read-only | `mount:/var/run/docker.sock`, `file:/etc/shadow` |
| A path inside those folders that is a symlink leading out of them (left by the repository, or by a container in its data folder) | `link:<path>` |
| A volume or network that isn't the project's own (`<compose project>_…`) or `spark` | `volume:cove_data`, `network:website_default` |
| A `${KEY}` that isn't `<COMPOSE PROJECT>_*` (capitals, `-` as `_`) or `SHARED_*` | `secret:BOTSUITE_COVE_TOKEN` |
| A name on `spark` (service name, `container_name` or alias) that another project's container already answers to: requests for it would reach either | `name:web` |
| `build:` with `privileged`, `network: host` or `entitlements` | `build:privileged`, `build:network:host` |

A port published on every interface (`"2400:2400"`) is only a warning: anything on the local network can reach it, around Cloudflare.

**Exceptions** live in `policy.json` at the top of Lighthouse's repository, built into the binary: a change is reviewed in git and takes effect when Lighthouse is deployed. Each names the compose project, the IDs it allows (an ID ending in `*` allows every ID starting with what comes before it), and why:

```json
{
  "exceptions": [
    {"project": "sonar", "allow": ["mount:/var/run/docker.sock"], "reason": "reads container stats for the dashboard"}
  ]
}
```

A finding an exception allows is still shown, marked `allowed by policy.json: <reason>`. A malformed `policy.json` (an unknown field, an exception without a reason, `"*"`) fails Lighthouse's tests and its startup. `lh check <name>` runs a project's latest commit through the rules and its test stage without deploying it; `lh help rules` is the short version of this section.

### 7.2 Lighthouse settings (`x-lighthouse`)

A project's few Lighthouse settings live in its own compose file, in a top-level `x-lighthouse:` block that Compose ignores. Nothing is set on the server. Without the block, a project deploys every commit on its default branch, as an app.

```yaml
name: sparkdb
x-lighthouse:
  deploy: releases   # branch (default): every commit on the default branch
                     # releases: only version tags, v1.2.3 or 1.2.3
  tier: data         # app (default), infra or data
  backup: postgres   # dump the database before every deploy
services:
  db:
    image: postgres:16.4
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U $${POSTGRES_USER}"]
      interval: 5s
      retries: 12
```

| Setting | Values | What it does |
|---|---|---|
| `deploy` | `branch` (default), `releases` | `releases`: only tags that are plain versions deploy (pre-releases such as `v1.1.0-rc.1` and other tags don't); a newer one than **any release deployed before** deploys automatically, so going back by hand (`deploy <name> v1.0.0`) sticks until a newer release is tagged. Pushing to the branch does nothing. Your release flow (tag, then push `main`) deploys at the tag |
| `tier` | `app` (default), `infra`, `data` | The order deploys and the reconcile loop go in: `data` (sparkdb) first, then `infra` (Cove, cloudflared), then apps. One deploy runs at a time, so an infrastructure deploy has the server to itself until it's healthy |
| `backup` | `postgres` | Before the swap, `pg_dumpall` runs in the project's postgres service (the one whose image is `postgres…`) and is saved gzipped to `/srv/backups/<compose project>/` (mode 0600; the newest 5 are kept). If the backup fails, the deploy stops and nothing changes. On a first deploy, or when the database isn't running, there's nothing to back up |

The settings are read from the default branch's compose file whenever the branch moves (to know whether to watch the branch or the tags), and from the deployed commit's compose config at each deploy (what the deploy does). A setting Lighthouse doesn't know is an error, at the check and at the deploy. The block is read with a small reader, not a full YAML parser: write it as indented `key: value` lines, or `{key: value, ...}` on one line.

**Deploy variables.** Every deploy gives Compose `LIGHTHOUSE_DEPLOY_COMMIT` (the full SHA) and `LIGHTHOUSE_DEPLOY_VERSION` (the release, or the commit's first 7 characters), so a compose file can label what it runs: `- APP_VERSION=${LIGHTHOUSE_DEPLOY_VERSION:-dev}`. They aren't secrets and aren't fetched from Cove.

**Infrastructure deploys.** Everything a swap needs is fetched before it: the new version's secrets, and the previous version's (so a rollback doesn't need Cove, whose database may be what's being swapped). While sparkdb restarts, Lighthouse needs nothing from it; recording the deploy waits (up to 2 minutes) until the database is back. Rollback can't undo a data-format change: pin sparkdb's image to a version and plan a major Postgres upgrade by hand.

---

## 8. Cove integration

| Item | Value |
|---|---|
| Client | CoveClient `v1.0.0` (`github.com/lsariol/coveclient`) |
| Address | `COVE_URL=http://cove:2100` on `spark`; Cove has no published port |
| Token | Lighthouse's own project token, **read-only over everything** (`token create lighthouse --allow '*'`). Fetched once through the bootstrap endpoint and saved to `COVE_TOKEN_PATH`; checked with `Auth` on every start |
| Keys Lighthouse reads for itself | At startup, in one batch: `LIGHTHOUSE_GITHUB_TOKEN`, `LIGHTHOUSE_DATABASE_URL` (as `lighthouse_app`), `LIGHTHOUSE_MIGRATOR_DATABASE_URL` (as `lighthouse_migrator`). All three must exist |
| Keys it reads for projects | Every `${KEY}` without a default in each project's compose file, as one `GetSecrets` batch per deploy (and one more for a rollback) |
| Audit | Every read shows as `lighthouse` in Cove's `history <KEY>` |
| Database keys | One connection string per role that logs in, nothing else: `LIGHTHOUSE_DATABASE_URL` (app), `LIGHTHOUSE_MIGRATOR_DATABASE_URL`, `LIGHTHOUSE_READER_DATABASE_URL` (for pgAdmin). The server's convention is `<PROJECT>_<ROLE>_DATABASE_URL`, the app's without a role. No separate password keys ([§10.1](#101-database-setup-once-by-hand)) |

**First start (bootstrap):**

```
cove> token create lighthouse --allow '*'      # once
cove> bootstrap open lighthouse                # opens for 10 minutes; hands out a NEW token
$ docker compose up -d                         # Lighthouse: LoadOrBootstrap saves the token file
cove> bootstrap status                         # shows the handout
```

While the endpoint is closed, Lighthouse logs `waiting for a Cove token: run "bootstrap open lighthouse"` every 15 s, and `lh status` shows `waiting for Cove`.

**Because the token can read every key, Lighthouse decides which project gets which secret:** a project's own keys (`<PROJECT>_*`) and `SHARED_*`, nothing else ([§7.1](#71-the-deploy-rules)). Making Cove enforce that itself was considered and left out ([§15](#15-known-limits-and-accepted-risks)).

---

## 9. CLI

Lighthouse's CLI follows the server's CLI conventions (clig.dev; modelled on Cove's). It is a separate process from the daemon and reaches it through the control socket, so it never runs on the daemon's terminal and nothing typed ends up in `docker logs`.

| Run as | What it is |
|---|---|
| `docker exec -it lighthouse /lighthouse shell` | The prompt, `lighthouse (prod)>` (prod in red), with line editing, history and Tab completion of commands, projects and `project:service`. `exit`, Ctrl-D or Ctrl-C leave it; Lighthouse keeps running |
| `docker exec lighthouse /lighthouse <command>` | One command; exit status 0 or 1. Add `-it` when it may ask a question |
| `lighthouse help` | Help; works without a running daemon |
| `lighthouse` (no arguments) | The daemon **plus** its CLI on this terminal, for running locally (as `cove` does). The CLI talks to the daemon directly; `exit` or Ctrl+C stops Lighthouse. Plain line input, no Tab completion. If there's no terminal, the CLI steps aside and the daemon keeps running |
| `lighthouse serve` | The daemon only (the container's command) |
| `lighthouse version` | The version |
| `lighthouse health` | Exit 0 if the daemon answers on its control socket: the container's healthcheck |
| `lighthouse self-update <file>` | The update helper's work; Lighthouse starts it itself ([§4.6](#46-self-update)) |
| `docker exec lighthouse /lighthouse migrate [status\|up]` | Show or apply database migrations, as the migrator (they're applied at startup anyway) |
| `docker exec -i lighthouse /lighthouse import -` | Move a pre-1.0 `repos.json` into the database (from standard input, or a file path); safe to run twice |

### Commands

`help` lists them by group; `help <command>` shows every form, flags and examples; `help setup`, `help failed` and `help rules` are guides. Project names aren't case-sensitive. The container commands take `<name>` (every service) or `<name>:<service>` (one).

| Command | Usage | Notes |
|---|---|---|
| `list`, `ls`, `l` | `list` | Every project: repository, what it deploys (commits or releases, and its tier), what runs (version and commit, or stopped), last deploy, last check; its last error, and whether it's broken |
| `add` | `add <url> [--name <name>]` | Named after the repository (lowercase). `--name` only when that name is taken; on a terminal `add` asks instead. Names: letters, digits, `-`, `_`, up to 64. The URL may have `www.`, a trailing `/` or `.git`. Deployed on the next check |
| `remove`, `rm` | `remove <name> [--keep] [--yes]` | Asks first. Stops watching it and removes its containers and networks (`docker compose down`); its data and images stay. `--keep` leaves the containers running, untracked |
| `rename` | `rename <name> <new-name>` | Lighthouse's name only; repository and containers unchanged |
| `set-url` | `set-url <name> <url>` | Watch another repository under the same name |
| `deploy`, `rebuild` | `deploy <name> [tag\|commit]`, `deploy all [--yes]` | Deploy the newest commit (or release) now, even if deployed or broken, and wait; or a given tag or commit (full or short SHA). A stopped project is started again. `all` asks first |
| `retry` | `retry <name>` | Clear a broken project's failures and deploy it now |
| `history` | `history <name> [count]` | The last deploys (10, up to 100), newest first: #, when, trigger, result (`succeeded`, `failed at <step>`, `rolled back at <step>`), commit, duration, the error's first line |
| `rollback` | `rollback <name>` | Deploy again what ran before the current version (the newest earlier successful deploy), for a version that came up healthy but is wrong. Checks don't redeploy what it went back from |
| `check` | `check <name>` | Run the latest commit through the deploy's checks (the rules and the test stage) without deploying or recording it; exits non-zero if a deploy would be refused |
| `report` | `report <name> [n]` | One deploy (1 = the latest): commit, start, result and its kind, then each step with its status, duration and output |
| `scan` | `scan` | Check every project now and deploy new commits; refused while another scan runs |
| `pause` / `resume` | `pause [all]` | Stop / restart all automatic work, for every project: the checks and the reconcile loop. `deploy`, `rollback` and `scan` still work. A restart of Lighthouse, or a self-update, resumes |
| `start` | `start <name[:service]\|all>` | Start containers |
| `stop` | `stop <name[:service]\|all> [--yes]` | Asks first. A whole project stopped stays down on purpose: not deployed or brought back until `start`, `restart` or `deploy` |
| `restart` | `restart <name[:service]\|all> [--yes]` | `all` asks first |
| `logs` | `logs <name[:service]> [lines]` | The last lines (50, up to 10,000) of each container, on stdout; with a heading per service when there are several |
| `status` | `status` | Version, environment, startup state, automatic deploys, Cove, GitHub token, database and schema; then each project (running, degraded, stopped or missing; its commit, last deploy and check, or `broken`) with a line per service (state and health). Exits 1 and lists what needs attention when something does |
| `help`, `h` | `help [command\|setup\|failed\|rules]` | |
| `exit`, `quit` | | Leave the shell |

### Conventions

- **Output:** data (tables, logs, help) on stdout; messages on stderr, each starting with a symbol: `✓` success, `!` warning or wrong usage, `✗` error, `?` question. Color only on a terminal and never with `NO_COLOR`. Wrong arguments show `Usage: <form>`. Errors say how to fix them.
- **Confirmation:** `remove`, `stop`, and the `all` forms of `deploy`, `stop` and `restart` ask `(y/N)`; Enter means no. `--yes` / `-y` skips the question. Without a terminal (a script, or `docker exec` without `-it`) they refuse and point at `--yes` instead of guessing.
- **Scope:** container commands only act on watched projects' containers, found by their compose project, never on anything else on the host (`stop sparkdb` is "no project named sparkdb").
- **Adding a command:** see "Where things go" in [§3](#3-architecture).

---

## 10. Deploying Lighthouse

Lighthouse is deployed **by hand once**; after that it **updates itself** ([§4.6](#46-self-update)) when a new release is tagged, once its own repository is watched (`lh add https://github.com/lsariol/lighthouse`).

**First deploy** (on the Debian server, over SSH):

```bash
# The folders the bind mounts need.
sudo mkdir -p /srv/server/storage/lighthouse/cove /srv/server/staging /srv/backups

docker network inspect spark >/dev/null 2>&1 || docker network create spark

# In Cove (docker exec -it cove /cove shell):
#   token create lighthouse --allow '*'
#   create LIGHTHOUSE_GITHUB_TOKEN <a fine-grained, read-only GitHub token>
#   bootstrap open lighthouse
# The database and its URL keys: §10.1.

git clone https://github.com/lsariol/lighthouse.git && cd lighthouse
git checkout v1.0.0
LIGHTHOUSE_DEPLOY_VERSION=v1.0.0 docker compose up -d --build   # the version status shows; self-updates set it themselves
docker logs -f lighthouse     # success: "migration applied" (first start only), then "Lighthouse ready"
docker exec lighthouse /lighthouse status   # success: exit status 0 (otherwise: "Needs attention: ...")
```

**Updating:** tag a release (`v1.0.1`) and push the tag; Lighthouse deploys it on its next check, through the update helper. By hand, if ever needed (Lighthouse stopped, or not yet watching itself):

```bash
cd ~/lighthouse && git pull && docker compose up -d --build
```

State lives in the database and the bind mounts, so rebuilding is safe. A deploy that has already swapped when Lighthouse stops finishes its check first (up to 2 minutes); one still building is cancelled, which changes nothing running.

**Upgrading from the pre-1.0 Lighthouse** (one time, on the server):

1. The database setup, [§10.1](#101-database-setup-once-by-hand).
2. Empty the old scratch folders: `sudo rm -rf /srv/server/staging/* /srv/server/download`. (The new Lighthouse keeps each running version's files in `/srv/server/staging`, mounted at that same path; nothing there is needed from before.)
3. `git fetch && git checkout v1.0.0 && LIGHTHOUSE_DEPLOY_VERSION=v1.0.0 docker compose up -d --build` (first `sudo mkdir -p /srv/backups` if it doesn't exist). The new compose file no longer mounts `.env`, `repos.json` or the download folder, publishes no port, uses `COVE_URL`, and mounts the staging folder at the same path inside. Success: `docker logs lighthouse` shows the migrations applied, then `Lighthouse ready` with `projects=0`.
4. Move the watched projects over, straight from the old file:
   ```bash
   docker exec -i lighthouse /lighthouse import - < /srv/server/storage/lighthouse/repos.json
   ```
   Success: one `✓ Imported <name>` per project, then `N imported, 0 already there, 0 failed.` Their deployed commits come along, so nothing redeploys unless the default branch has moved since.
5. `docker exec lighthouse /lighthouse status`: every project listed, with its services running. A project whose compose file names its project differently from its repository (like `landing` → `website`) shows `missing` until its next deploy teaches Lighthouse the name: `deploy <name>` does it now.
6. Afterwards, `/srv/server/storage/lighthouse/repos.json` and `/srv/server/storage/lighthouse/.env` can be deleted (keep a copy of `repos.json` until you're happy).
7. **Let Lighthouse update itself:** `lh add https://github.com/lsariol/lighthouse`. It's named `lighthouse`, its compose project is `lighthouse` (which Lighthouse recognises as its own), and it deploys release tags only. Run `lh check lighthouse`: it should pass, with the four mounts shown as allowed by `policy.json`.
8. **Infrastructure:** add the `x-lighthouse` blocks ([§7.2](#72-lighthouse-settings-x-lighthouse)): SparkDB `{deploy: releases, tier: data, backup: postgres}` plus a `pg_isready` healthcheck (then tag it and `lh add` its repository; it stops being deployed by hand), Cove `{deploy: releases, tier: infra}`, cloudflared `{tier: infra}`. `lh check` each.

The CLI is now `docker exec -it lighthouse /lighthouse shell` instead of `docker attach`.

### 10.1 Database setup (once, by hand)

Needed before the v1.0.0 Lighthouse (it keeps its state in Postgres). This is Admin-level work, so it's done by hand, not by a migration ([§14.1](#141-databases-and-migrations)). Two scripts in `scripts/db/` do it:

| Script | What it does |
|---|---|
| `check.sql` | **Read-only.** Shows the `lighthouse_*` roles, `lighthouse_db`'s owner and access, its schemas and tables |
| `setup.sql` | Creates what's missing and fixes ownership and access (below). **Safe to run again; never changes an existing role's password**, and checks every missing password before changing anything |

What `setup.sql` leaves behind:

| Thing | State |
|---|---|
| `lighthouse_owner` | NOLOGIN, no password; owns the database and the `lighthouse` schema |
| `lighthouse_migrator` | LOGIN; member of `lighthouse_owner` with `SET role = 'lighthouse_owner'`, so what migrations create is owned by the owner |
| `lighthouse_app`, `lighthouse_reader` | LOGIN; existing passwords kept |
| `lighthouse_db` | Owned by `lighthouse_owner`; `PUBLIC` may not connect; the three login roles may |
| `public` schema in it | Nobody else may create objects there |

Tables and their grants come later, from Lighthouse's migrations. The script never changes an existing role's password; step 4 below has the one-line reset when one is needed.

**Where passwords live:** only in the connection strings in Cove, one per role that logs in:

| Role | Cove key |
|---|---|
| `lighthouse_app` | `LIGHTHOUSE_DATABASE_URL` |
| `lighthouse_migrator` | `LIGHTHOUSE_MIGRATOR_DATABASE_URL` |
| `lighthouse_reader` | `LIGHTHOUSE_READER_DATABASE_URL` (pgAdmin uses its password) |
| `lighthouse_owner` | none: it can't log in |

A new password is generated straight into its URL on the server, and `setup.sql` reads it back out of the URL. A password is never typed, shown, or stored twice.

**Dev** (`sparkdb-dev` on the PC): the roles and database are already in place (checked 2026-10-05). In the dev Cove, the URLs use `localhost:5000` as the host, because dev Lighthouse runs on the PC. Add `LIGHTHOUSE_READER_DATABASE_URL` too, then delete the old password keys.

**Prod**, all on the Debian server over SSH unless marked:

1. **Copy the scripts to the server** (on your PC, in PowerShell, from the repository folder):
   ```powershell
   scp scripts\db\check.sql scripts\db\setup.sql <you>@<server>:/tmp/
   ```
2. **Look first:**
   ```bash
   docker exec -i sparkdb psql -U Admin -d postgres -f - < /tmp/check.sql
   docker exec cove /cove list LIGHTHOUSE_
   ```
   Note which `lighthouse_*` roles exist, and which old password keys Cove has.
3. **Two shell helpers** for this session (they print nothing):
   ```bash
   newpw() { tr -dc A-Za-z0-9 </dev/urandom | head -c 32; }                         # a new random password
   pw()    { docker exec cove /cove get "$1" | sed -E 's#^postgres://[^:]+:([^@]*)@.*#\1#'; }   # the password inside a URL key
   ```
4. **One URL per login role**, as `postgres://<role>:<password>@sparkdb:5432/lighthouse_db?sslmode=disable`:
   - **A role that doesn't exist yet** (usually `lighthouse_migrator`) gets a new password:
     ```bash
     docker exec cove /cove create LIGHTHOUSE_MIGRATOR_DATABASE_URL "postgres://lighthouse_migrator:$(newpw)@sparkdb:5432/lighthouse_db?sslmode=disable"
     ```
   - **A role that exists, with its password in an old key**, keeps that password. Check it's only letters and digits first (a URL can't hold `@ : / ? #` unescaped; this prints only the verdict):
     ```bash
     docker exec cove /cove get LIGHTHOUSE_APP_PASSWORD | grep -qx '[A-Za-z0-9]*' && echo "plain: fine" || echo "special characters: use a new password, see below"
     docker exec cove /cove create LIGHTHOUSE_DATABASE_URL "postgres://lighthouse_app:$(docker exec cove /cove get LIGHTHOUSE_APP_PASSWORD)@sparkdb:5432/lighthouse_db?sslmode=disable"
     ```
     and the same for `lighthouse_reader` → `LIGHTHOUSE_READER_DATABASE_URL` from `LIGHTHOUSE_READER_PASSWORD`.
   - **A role that exists, with no known password** (or one with special characters): create its URL with `$(newpw)` as for a new role, then set the role to it after step 5:
     ```bash
     docker exec -i sparkdb psql -U Admin -d postgres -v pw="$(pw LIGHTHOUSE_READER_DATABASE_URL)" <<< "ALTER ROLE lighthouse_reader PASSWORD :'pw';"
     ```
5. **Run the setup**, giving it every password. It uses only those of roles it has to create and ignores the rest:
   ```bash
   docker exec -i sparkdb psql -U Admin -d postgres \
     -v migrator_password="$(pw LIGHTHOUSE_MIGRATOR_DATABASE_URL)" \
     -v app_password="$(pw LIGHTHOUSE_DATABASE_URL)" \
     -v reader_password="$(pw LIGHTHOUSE_READER_DATABASE_URL)" \
     -f - < /tmp/setup.sql
   ```
   Success: `Done.` and every row matches its `want` column.
6. **Check every login** against the URL in Cove. Each line should print the name shown:
   ```bash
   for key in LIGHTHOUSE_DATABASE_URL LIGHTHOUSE_MIGRATOR_DATABASE_URL LIGHTHOUSE_READER_DATABASE_URL; do
     docker run --rm --network spark postgres:16 psql "$(docker exec cove /cove get $key)" -Atc "select current_user"
   done
   # lighthouse_app, lighthouse_owner (the migrator acts as the owner), lighthouse_reader
   ```
   This connects from a separate container over `spark`, the way Lighthouse will, so the password is really checked. A connection from inside the `sparkdb` container may skip the password check.
7. **Retire the old password keys**, now that the URLs are proven (Cove can `restore` them if needed):
   ```
   docker exec -it cove /cove delete LIGHTHOUSE_APP_PASSWORD
   docker exec -it cove /cove delete LIGHTHOUSE_OWNER_PASSWORD
   docker exec -it cove /cove delete LIGHTHOUSE_READER_PASSWORD
   ```
8. **Clean up:** `rm /tmp/check.sql /tmp/setup.sql`, and tick the entry in the prod rollout plan. Update pgAdmin's saved password for `lighthouse_reader` if it changed.

**Way back:** nothing reads these yet. To undo the Cove side: `delete` the URL keys and `restore` the old password keys.

---

## 11. Operations

### Adding a project

1. Make the repo meet the contract ([§7](#7-connecting-a-project-the-contract), or `lh help setup`).
2. Create its secrets in Cove under standard names. If the project has a database: roles and `CREATE DATABASE` by hand as Admin, schema via the project's own goose migrations ([§14.1](#141-databases-and-migrations)).
3. If it keeps data on the host: create `/srv/server/storage/<compose project>/` (owned by the user its image runs as, if not root).
4. `lh add https://github.com/LSariol/<Repo>`, then `lh scan` (or wait for the next check). Watch `docker logs -f lighthouse`.
5. Check: `lh status` shows its services running; `history <one of its keys>` in Cove shows `lighthouse`.

### Removing a project

`lh remove <name>` stops watching it and removes its containers and networks (`docker compose down`); its data under `/srv/server/storage` and its images stay. `lh remove <name> --keep` leaves the containers running, untracked.

### A deploy failed

`lh help failed` is the short version.

1. **Nothing went down.** The running version kept serving while the new one was prepared; if the new one started but didn't come up healthy, the old one was put back (`rolled back` in `lh history`).
2. **See why:** `lh report <name>` shows each step of the last deploy with its output. The error is also on `lh list` and `lh status`.
3. **What happens next:** a transient failure (GitHub, Cove, the network) is tried again at the next check. A permanent one (the commit itself) is tried again too, but after 3 failures of the same commit the project is **broken** and waits.
4. **Fix it:** push a fix (a new commit is always tried), or, for a fix outside the repository such as a missing secret in Cove, `lh retry <name>`.

### Going back

`lh rollback <name>` deploys again what ran before the current version (its image and files are kept, so it's quick). `lh deploy <name> <tag or commit>` deploys any one. Either way, checks don't redeploy what you went back from: for a project that deploys its branch, the branch's newest commit is held until a newer one arrives or `lh deploy <name>`; for one that deploys releases, only a release newer than any deployed before is deployed automatically. A version that never came up healthy was already rolled back automatically.

### Pausing everything

`lh pause` (or `pause all`) stops all automatic work for every project: no checks, and the reconcile loop leaves things as they are. Commands you run still work. `lh resume` turns it back on; so does a restart of Lighthouse or a self-update, so a forgotten pause doesn't last.

### If sparkdb or Cove itself is gone

Docker's restart policies bring them back after a crash or a reboot. If a container was **removed**, Lighthouse can't bring it back: deploying either needs a secret from Cove, and Cove needs sparkdb. Start them by hand from their deploy folders, sparkdb first:

```bash
cd "$(ls -dt /srv/server/staging/sparkdb/*/ | head -1)" && docker compose -p sparkdb up -d   # its data is kept, so no password is needed
docker exec sparkdb pg_isready                                                               # "accepting connections"
cd "$(ls -dt /srv/server/staging/cove/*/ | head -1)" && docker compose -p cove up -d
docker exec cove /cove status
```

Lighthouse continues by itself once they answer; the reconcile loop brings back anything else that's down.

### Rotating the GitHub token

Create the new token, `update LIGHTHOUSE_GITHUB_TOKEN <token>` in Cove, `docker restart lighthouse`, then revoke the old token on GitHub.

### Rotating Lighthouse's Cove token

Delete `/srv/server/storage/lighthouse/cove/token`, `bootstrap open lighthouse` in Cove, `docker restart lighthouse`. If it leaked: `token revoke lighthouse` first. It can read **every** secret, so every secret should then be considered exposed (Cove `DOCUMENTATION.md` §14). After `token rotate` or `revoke` without a new bootstrap, Lighthouse refuses to start and says how to fix it.

### After a server reboot

Every container restarts by its own `restart:` policy. Lighthouse waits for Cove and for its database (`lh status` shows which), then starts checking. A project that's still down two minutes later (no container running where one should be) is brought back by the reconcile loop, data projects first ([§4.2](#42-the-check-loop)).

### Disk space

Each deploy cleans up after itself: deploy folders other than the running version and the one before it, rollback image tags of other commits, images nothing uses, and, at most daily, build cache unused for a week. What's left per project is the two versions it can roll between. By hand, if ever needed: `docker image prune -f` and `docker builder prune -f`.

---

## 12. Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `lighthouse: COVE_URL, … are not set` | The compose `environment:` lost lines; compare with the repository's `docker-compose.yml` |
| `lighthouse: STAGING_PATH "/srv/server": isn't a dedicated folder` | Lighthouse owns everything in it; point it at a folder only Lighthouse uses |
| `waiting for a Cove token: run "bootstrap open lighthouse"` repeating | No token file and Cove's bootstrap is closed. Open it in Cove |
| `waiting for Cove to be reachable` repeating | Cove is down or not on `spark`. `docker ps` for `cove`; both containers on `spark` |
| `lighthouse: Cove rejected Lighthouse's token` (restarting) | The token was rotated or revoked. Delete the token file, `bootstrap open lighthouse`, restart |
| `lighthouse: Lighthouse's secrets aren't all in Cove (… missing: KEY…)` | Create the named keys in Cove ([§10.1](#101-database-setup-once-by-hand) for the database URLs), restart |
| `waiting for the database to be reachable` repeating; `status` says `waiting for the database` | sparkdb is down or Lighthouse isn't on `spark`. `docker ps` for `sparkdb`. It continues by itself once sparkdb answers |
| `lighthouse: … password authentication failed for user "lighthouse_…"` | The URL in Cove doesn't match the role's password. Reset the role from the URL ([§10.1](#101-database-setup-once-by-hand) step 4), restart |
| `lighthouse: the database schema is at version N, but this Lighthouse needs M` | Migrations didn't run: check `LIGHTHOUSE_MIGRATOR_DATABASE_URL` logs in as `lighthouse_migrator`; `lh migrate status` shows which are missing |
| `lighthouse: apply migrations: …` | A migration failed; nothing after it ran. The message names it. Usually a missing role or grant from §10.1: rerun `setup.sql` |
| A deploy fails at `check`: `the compose file breaks the deploy rules (…)` | `lh report <name>` lists each finding with its ID. Fix the compose file, or, if the project really needs it, add an exception to `policy.json` ([§7.1](#71-the-deploy-rules)) |
| A deploy fails at `test`: `<service>'s tests failed` | The Dockerfile's `test` stage failed; `lh report <name>` shows its output |
| `lighthouse-updater` is still running, or a self-update never finished | `docker logs lighthouse-updater` says where it is. A hand-off unfinished after 30 minutes is recorded as failed; the next update removes the helper |
| A self-update was rolled back | The new Lighthouse didn't become healthy (`lighthouse health` failed): `lh report lighthouse` has the helper's swap and verify output; `docker logs lighthouse` (of the new version, if it ran) why it didn't start |
| `STORAGE_PATH isn't readable, so symlinks … can't be checked` in the log | Mount `/srv/server/storage` read-only at the same path (the repository's `docker-compose.yml` does) |
| `Can't reach the Lighthouse daemon at /run/lighthouse/control.sock` | Lighthouse isn't running, or you ran the CLI outside its container. Use `docker exec … /lighthouse …`; check `docker ps` and `docker logs lighthouse` |
| `<Verb> cancelled: no answer to the confirmation` | No terminal to ask on: use `docker exec -it`, or add `--yes` |
| `Lighthouse is still starting (…)` | Every command but `status` waits for startup; `status` and `docker logs lighthouse` say what it's waiting for |
| A project's last check: `GitHub: <what it asked>: 401 …` | The GitHub token is wrong or expired. Update `LIGHTHOUSE_GITHUB_TOKEN`, restart |
| `GitHub: 403 (… rate limit …)` or `429` | Rate limit (5,000 requests an hour per token). Checks send the last ETag, and an unchanged answer doesn't count, so this takes many projects changing at once. Raise `LIGHTHOUSE_POLL_INTERVAL`. Transient: retried after a wait |
| A project isn't deployed, and its last error is `x-lighthouse: …` | The compose file's `x-lighthouse` block has a setting Lighthouse doesn't know ([§7.2](#72-lighthouse-settings-x-lighthouse)) |
| A deploy fails at `backup` | `pg_dumpall` failed (the message has its last lines), or `/srv/backups` isn't writable. Nothing changed; it's tried again after a wait |
| `GitHub: <what it asked>: 404 …` | Wrong URL, a renamed repo (`set-url`), or a private repo the token can't see (a fine-grained token must list it). Other projects are still checked |
| `fetch: … isn't a gzipped tarball` / `would be written outside` / `links outside the repository` | The archive is broken or holds a path that leaves its folder. Permanent; fix the repository |
| `inspect: the compose file can't be read` | No compose file at the top, or it's invalid. `report <name>` shows Compose's message |
| `inspect: the compose project "x" belongs to <other> already` | Two compose files use the same `name:`. Change one |
| `secrets: … not found: KEY …` | `KEY` isn't in Cove. Create it (`retry <name>`), or fix the name in the compose file (push). Nothing running changed |
| `secrets: … forbidden` | Lighthouse's token doesn't cover the key; `docker logs cove` names it |
| `build: docker compose build failed …` | The Dockerfile or the build failed. `report <name>` shows the build output |
| `verify: <service> is unhealthy` / `stopped with exit code N` / `keeps restarting` / `not up after 2m0s` | The new version doesn't come up. It was rolled back if the previous one's files were kept; `logs <name>:<service>` shows the old one's output, `report <name>` the deploy's |
| `…; the previous version's files aren't kept …` | The first deploy by this Lighthouse failed after the swap: no automatic rollback. Push a fix, or start the old version by hand |
| `<name> is broken` in `status` | Its latest commit failed 3 times. `report <name>` shows why; push a fix or `retry <name>` |
| `<name> is missing` in `status` | No containers for its compose project: not deployed yet, or its compose file names the project differently from the repository and it hasn't been deployed since the upgrade (`deploy <name>`) |
| `has no service "x". Its services: …` | `<name>:<service>` names a service that doesn't exist; the message lists the ones that do |
| A project's data folder is empty after a deploy | It's a relative bind mount: each version gets a new folder. Use an absolute host path ([§7](#7-connecting-a-project-the-contract) rule 4) |

---

## 13. Security model

What protects what, and what is left to you. The risks accepted on purpose are in [§15](#15-known-limits-and-accepted-risks).

| Asset | Protection | Left open |
|---|---|---|
| Cove secrets | Lighthouse's own token, checked at startup; a project gets only its own keys and `SHARED_*` ([§7.1](#71-the-deploy-rules)); values only in process environments | Lighthouse's token can read every key (accepted, [§15](#15-known-limits-and-accepted-risks)) |
| The host | The deploy rules refuse anything that reaches outside a project: privileges, host namespaces, devices, the Docker socket, other projects' folders, volumes and networks ([§7.1](#71-the-deploy-rules)); exceptions only in `policy.json`, reviewed in git | Lighthouse itself has the Docker socket (= root) |
| The GitHub token | Fine-grained and read-only; stored in Cove, held in memory; only its length is logged | – |
| Lighthouse's Cove token | File mode 0600, written atomically by CoveClient; no project can mount it; useless off the server (Cove publishes no port) | Readable by root on the server |
| The CLI | A Unix socket inside the container, mode 0600, never published; reached only with `docker exec` | Anyone who can `docker exec` can use it (they already have root) |
| What the CLI can touch | Only watched projects' containers, found by their compose project | – |
| Network | No published port; joins `spark` to reach Cove | – |
| Secrets in logs | Values and tokens are never printed; every value fetched for a deploy is replaced by `[secret]` in its stored output, Lighthouse's log and its errors | A value shorter than 4 characters isn't hidden |

**Not protected against:** anyone with root or Docker access on the server, and anyone who controls the GitHub account. Someone who can push to `main` of a watched repo gets whatever that project's compose file may have under the deploy rules, and its own secrets; keep watched repositories to ones you own, with 2FA and branch protection.

### 13.1 Running locally

Needs Go 1.27.1 and Docker. On Windows, run the commands in PowerShell or Git Bash from the repository folder.

```bash
cp .env.example .env        # point COVE_URL at your dev Cove; it holds the dev database URLs
go run ./cmd/lighthouse           # Lighthouse with its CLI on this terminal; "exit" stops it
# or, in two terminals:
go run ./cmd/lighthouse serve     # the daemon only
go run ./cmd/lighthouse shell     # a separate prompt with Tab completion (or: go run ./cmd/lighthouse status)
```

Success: the daemon logs `Lighthouse starting`, then `waiting for …` or `Lighthouse ready`, and `status` answers. In VS Code, F5 runs "Lighthouse" (the first form); "Lighthouse: shell" connects to one already running (`.vscode/launch.json`).

**Everything stays in the repository.** A local run reads and writes only inside `.dev/`: the staging, storage and backup folders, the control socket and the dev Cove token. Its database is `lighthouse_db` on `sparkdb-dev`, through the URLs in the dev Cove. `.dev/` is gitignored and kept out of Docker builds. Go tests use temporary folders that the test runner removes afterwards.

**Checks before a commit** (CI runs the same on every push to `main` and `release/**`):

```bash
gofmt -l .                       # prints nothing
go vet ./...
scripts/test-db.sh up            # once: a throwaway Postgres, set up by scripts/db/setup.sql
eval "$(scripts/test-db.sh env)" # in each new shell: points the integration tests at it
go test -race ./...              # -race needs cgo: gcc on Windows (msys2), or run in CI
go run golang.org/x/vuln/cmd/govulncheck@latest ./...   # "No vulnerabilities found."
scripts/test-db.sh down          # when done
scripts/e2e-self-update.sh       # optional: the self-update end to end against Docker ("PASS")
```

On Windows, run these in Git Bash. Without the test database, the integration tests in `internal/database` are skipped and everything else still runs. They run against real Postgres (never sparkdb), over TCP, so passwords and grants are really checked.

**The Go version is pinned in two places, kept equal:** `go 1.27.1` in `go.mod` and `golang:1.27.1-alpine` in the `Dockerfile`. Bump both in one commit.

---

## 14. Standards (mandatory)

These rules apply to every change, on every project on the server. They come from the server-wide conventions and Cove's documentation. A pull request that breaks one isn't done.

### 14.1 Databases and migrations

- **Every database change is a goose migration**, so prod gets it on deploy. Nothing in a project's schema is created or changed by hand.
- **Only Admin-level work is done by hand:** creating roles, `CREATE DATABASE`, extensions, and ownership changes. Those are server-wide, so a migration can't (and shouldn't) do them.
- **One Postgres server, `sparkdb`**, reached from containers as `sparkdb:5432`. pgAdmin reaches it through an SSH tunnel.
- **Four roles per project** (for Lighthouse: `lighthouse_*`):

  | Role | Login | Used for |
  |---|---|---|
  | `lighthouse_owner` | No | Owns the database, the `lighthouse` schema and everything in it |
  | `lighthouse_migrator` | Yes | Runs migrations; member of `lighthouse_owner` with `SET role = 'lighthouse_owner'`, so what it creates is owned by the owner |
  | `lighthouse_app` | Yes | Lighthouse at runtime; only the grants its migrations give it |
  | `lighthouse_reader` | Yes | Read-only, for pgAdmin |

- **Migration rules** (same as Cove): files `000NN_description.sql`, starting with `-- +goose Up`; `$$` blocks wrapped in `-- +goose StatementBegin` / `StatementEnd`; **never edit a migration that has run in prod**; forward-only, so a mistake is fixed by the next migration; **additive only**: add tables and columns (with defaults), never drop or rename what the previous release reads, because a rolled-back Lighthouse ([§4.6](#46-self-update)) runs on the newer database (a removal waits until a later release no longer uses it); grants for `_app`/`_reader` live in migrations too. Migrations are embedded in the binary and run at every startup as the migrator (with a Postgres advisory lock); the app refuses to start on a database missing a migration it needs, but a newer database is fine (rolling back the code after an additive migration still works).
- The Admin SQL for Lighthouse is `scripts/db/setup.sql`, run by hand ([§10.1](#101-database-setup-once-by-hand)).
- **Lighthouse's migrations** (`internal/database/migrations/`):

  | Migration | What it does |
  |---|---|
  | `00001_projects` | `projects` and `deployments` ([§6](#6-data-the-database)) |
  | `00002_role_grants` | `lighthouse_app`: read/write `projects`, append-only `deployments`, read `goose_db_version`; `lighthouse_reader`: read all, including future tables |
  | `00003_pipeline` | `projects`: `compose_project` (unique), `failure_count`, `failing_sha`, `broken`; `deployments`: `rolled_back` status, `failure_kind`, `failed_step`; `deployment_steps` (append-only for the app) |
  | `00004_orchestrator` | `projects`: `deployed_version`, `highest_version`, `deploy_mode`, `tier`, `stopped`; `deployments`: `version`, the `reconcile` trigger |
  | `00005_held_commit` | `projects.held_sha`: the commit a rollback went back from, which checks don't redeploy |

  `lighthouse migrate status` lists them with the time each was applied.

### 14.2 Secrets (Cove)

- Every secret lives in Cove. Nothing secret is hard-coded, committed, or written to a file outside Cove (Lighthouse's own token file excepted).
- **Key names:** `PROJECT_PLATFORM[_ROLE]_TYPE`; capitals, digits and `_` only. `TYPE` ∈ `API_KEY`, `CLIENT_ID`, `CLIENT_SECRET`, `ACCESS_TOKEN`, `REFRESH_TOKEN`, `TOKEN`, `URL`, `PASSWORD`, `SECRET`, `ID`. `SHARED_` for keys several projects use. `PROJECT` is the compose project's name, since a deploy only gets its own keys ([§7.1](#71-the-deploy-rules)); `SPARK_` is only for server keys no compose file reads.
- **Only real secrets go in `${...}`** in a compose file Lighthouse deploys. Plain settings are written out as values.
- A project that **writes** to Cove gets its own token, injected as `COVE_TOKEN=${<PROJECT>_COVE_TOKEN}` with `COVE_URL=http://cove:2100`, and uses CoveClient v1. There is no master token.
- **Never print secret values, tokens or `.env` contents** in logs, CLI output or errors. Show lengths or key names only. Temporary copies of secrets stay outside repos and are deleted when done.

### 14.3 Git and releases

- Prod is only updated from `main`. Changes land on short-lived branches, merged into `release/<version>`.
- **Release:** update `CHANGELOG.md`, fast-forward merge `release/<version>` into `main`, tag `vX.Y.Z`, push the tag, then push `main`. Lighthouse deploys its own tag ([§4.6](#46-self-update)); a project that deploys its branch deploys on the push to `main`, one that deploys releases on the tag.
- Semantic versioning.

### 14.4 Prod changes

Every change that prod needs (a new host folder, a Cove key, Admin SQL, a compose change on the server) is logged in the prod rollout plan's change log (Appendix B), with what to do and how to undo it, before the release that needs it.

---

## 15. Known limits and accepted risks

**Accepted risks** (decided, not to be fixed):

- **Lighthouse holds the Docker socket**, which is root on the server. Deploying containers needs it; the deploy rules ([§7.1](#71-the-deploy-rules)) keep a project's compose file from reaching outside the project, so a push to `main` isn't root. A compromised Lighthouse, server or GitHub account still is.
- **Lighthouse's Cove token reads every key** (decided 2026-10-07). Lighthouse gives a project only its own keys and `SHARED_*`, which covers the realistic case: a compose file copied from another project. Making Cove enforce it too (reads on a project's behalf, tokens tied to an address) only helps when someone holds the token file without controlling Lighthouse or the server, and that file is useless off the server (Cove publishes no port) and can't be mounted by a project. Treat it like the vault key: root-only, and a backup of `/srv/server/storage` that leaves the server is sensitive.
- **Secret values end up in containers' environments**, where `docker inspect` shows them, as for any environment variable. A value shorter than 4 characters isn't hidden in output.

**Limits:**

- **No notifications yet.** A failed deploy or a broken project shows in `lh status` and `lh list`, but nothing tells you ([§16](#16-future-features)).
- **The base can't rebuild itself.** If the sparkdb or Cove *container* is removed, start them by hand ([§11](#if-sparkdb-or-cove-itself-is-gone)); crashes and reboots are covered by restart policies.
- **No automatic rollback on a project's first deploy by this Lighthouse,** or when the previous version's folder wasn't kept (deployed by hand): there's nothing to go back to. Lighthouse itself is the exception: its update helper falls back to the old image.
- **Rollback can't undo a data-format change.** Pin database images to a version, and plan a major Postgres upgrade by hand.
- **Memory-only conveniences:** the waits after failures, the ETags and `pause` start over when Lighthouse restarts.
- **The `x-lighthouse` reader isn't a full YAML parser:** indented `key: value` lines, or `{key: value}` on one line ([§7.2](#72-lighthouse-settings-x-lighthouse)).
- **One server.** Lighthouse runs the projects on the machine it's on; there's no clustering, by design.

---

## 16. Future features

Not in v1.0.0; each gets designed when it's picked up.

1. **Notifications.** The draft (2026-10-07): a small program of its own on the Windows PC, started at login, with one inbound API (`POST /v1/notify`: title, message, level, source; a bearer token) that shows a Windows notification, for Lighthouse and any other project. Lighthouse would notify on a failed, rolled-back or broken deploy, Cove or GitHub unreachable, a project brought back, a self-update, and the GitHub token close to expiring. Open: its own repository and name; a fixed address for the PC; when the PC is off, retry and drop (recommended; `history` keeps everything) or catch up on wake. With it could come crash-loop and unhealthy alerts from Docker.
2. **An infrastructure review and an agent-ready reference:** the server, `spark`, sparkdb, Cove, cloudflared and Lighthouse documented together; a fully commented example `docker-compose.yml`; and an "infrastructure dependencies" file, every rule one line linking to its full section, to give an agent (or a person) making a project compliant.
3. **Containers that don't run as root:** `check` warns about a service running as root, and Lighthouse creates `/srv/server/storage/<compose project>/` owned by the image's user on the first deploy, so nobody has to `chown` by hand.
4. **Remove `lighthouse import`** and `internal/reposjson` in v1.0.1, once the pre-1.0 `repos.json` is imported.
5. **Smaller ideas, when needed:** GitHub webhooks through cloudflared (polling stays as the fallback); a `pg_dump` of an app's own database before a deploy that migrates it.

**Out of scope** (decided; not planned): refreshing base images on a schedule; managing services that aren't GitHub projects; Cove-side token scoping; blue/green deploys; an approval step for infrastructure deploys; image scanning (Trivy) and secret scanning (gitleaks), which a project can run in its own `test` stage; settings changed through the CLI (they live in the repository); more than one server.

**Decisions that shape Lighthouse** (keep them when adding anything):

- **Hands-off:** nothing is set on the server per project. A project's settings live in its own compose file (`x-lighthouse`); privileges are granted only in Lighthouse's own `policy.json`, reviewed in git.
- **Build first, swap last,** one deploy at a time, and roll back what doesn't come up healthy.
- **State in Postgres** (`lighthouse_db`), schema only through additive goose migrations.
- **Tests run on Lighthouse,** in a project's Dockerfile `test` stage; no GitHub Actions for deployed projects.
- **Polling GitHub with ETags,** the Compose CLI for anything that changes a project, the Docker SDK for reading.
- **Restart policies start containers at boot;** Lighthouse waits for Cove and its database, then reconciles.
