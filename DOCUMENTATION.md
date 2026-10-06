# Lighthouse Documentation

The complete reference for **Lighthouse**, the self-hosted deployer for the `spark` server's projects. It covers how Lighthouse works today, the rules it follows, every known bug and security issue, and the plan for **v1.0.0**.

- [README](README.md): a short, friendly overview.
- [Cove](https://github.com/LSariol/Cove): the secret vault Lighthouse reads from. Its `DOCUMENTATION.md` §9 (connecting a project), §10 (bootstrap) and §14 (operations) are the other half of this document.
- [CoveClient](https://github.com/LSariol/CoveClient): the Go library Lighthouse uses to talk to Cove.

> **Status.** This document describes `release/1.0.0` after steps 1–3 of [§16.8](#168-order-of-work): the foundation, the database, and the deploy pipeline (build first, swap last, roll back; compose projects with several services). Sections 1–13 describe what the code **does today**. Section 14 lists the standards every change must follow. Sections 15–17 list what is still wrong and what v1.0.0 will look like.

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
15. [Known issues](#15-known-issues)
16. [Roadmap to v1.0.0](#16-roadmap-to-v100)
17. [Housekeeping](#17-housekeeping)

---

## 1. Overview

Lighthouse keeps the server's projects running the latest code on their default branch. It is one Go program running in a Docker container next to the projects it manages.

Every 10 seconds (configurable) it asks GitHub for the newest commit of each watched repository. When the commit changes, it deploys it, **building first and swapping last**:

1. downloads that exact commit and unpacks it,
2. reads its compose file: the compose project's name, its services, the `${KEY}` secrets it needs,
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
| Source | The default branch's newest commit, downloaded as a tarball through the API, so private repositories work too |
| Build and run | `docker compose -p <project> build`, then `up -d --no-build --remove-orphans`, on the host's Docker through the mounted socket; one deploy at a time |
| Safety | Nothing running is touched until the new version is built; a version that doesn't come up healthy is rolled back; a commit that fails 3 times marks its project **broken** until a new commit or `retry` |
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
| Watch a new repo | `lh add <name> https://github.com/<owner>/<repo>` |
| Stop watching | `lh remove <name>` (also removes its containers), or `lh remove <name> --keep` (leaves them running) |
| See what's watched | `lh list` |
| Deploy now | `lh deploy <name>` (or `deploy all`) |
| How did its deploys go? | `lh history <name>` |
| Why did a deploy fail? | `lh report <name>` (each step with its output) |
| A broken project, after fixing it outside the repo | `lh retry <name>` |
| Check for new commits now | `lh scan` |
| Freeze automatic deploys | `lh pause` / `lh resume` |
| A project's output | `lh logs <name>` (every service) or `lh logs <name>:<service> [lines]` |
| One service | `lh restart <name>:<service>` (also `start`, `stop`) |
| Lighthouse's own log | `docker logs -f lighthouse` |
| Help | `lh help`, `lh help <command>`, `lh help setup`, `lh help failed` |
| Add a secret a project needs | In Cove: `create <PROJECT>_<PLATFORM>_<TYPE> <value>`, then reference it as `${...}` in the project's compose file |
| Give Lighthouse a new Cove token | Delete `/srv/server/storage/lighthouse/cove/token`, `bootstrap open lighthouse` in Cove, `docker restart lighthouse` |
| Change the GitHub token | In Cove: `update LIGHTHOUSE_GITHUB_TOKEN <token>`, then `docker restart lighthouse` (it's read once at startup) |
| Database migrations | `lh migrate status`; they're applied automatically at startup |

---

## 3. Architecture

```
cmd/lighthouse/main.go     Modes (plain, serve, shell, one command, migrate, import, version),
                           the startup order, and the wiring of the packages below
internal/
  config/                  Every setting, read from the environment once and validated
  cli/                     The command table, help and guides, the shell (line editing,
                           history, Tab completion), one-shot commands, output helpers
  control/                 The CLI ↔ daemon protocol: Service (what the CLI can ask), Serve
                           (HTTP over a Unix socket) and Client (the CLI's side)
  daemon/                  Implements control.Service on the parts below; resolves
                           "<project>[:<service>]"; turns errors into messages that say how
                           to fix them; the startup phase
  orchestrator/            When projects deploy: the check loop, Scan, Deploy, Retry; the
                           broken rule; records every check and deploy in the store
  deploy/                  How a project deploys (one at a time): deploy.go (the steps,
                           rollback), source.go (unpacking), verify.go (health, cleanup),
                           scrub.go (hiding secrets in output)
  compose/                 Runs docker compose (config, variables, build, up, down) with
                           deadlines and a clean environment
  docker/                  A compose project's containers (by label), their state and
                           output, image tags (Docker SDK)
  projects/                Project, Deployment, Step, the Store interface, name rules
    projectstest/          An in-memory Store, and the tests every Store must pass
  database/                Postgres: the pool, goose migrations (migrations/, built in), the
                           schema check, and the Store
  reposjson/               Reads a pre-1.0 repos.json, for `lighthouse import`
  github/                  Repository URLs, the latest commit, a commit's tarball
  cove/                    Connecting to Cove at startup; Lighthouse's own secrets
scripts/
  db/                      Admin SQL for sparkdb, run by hand (§10.1); CI uses it too
  test-db.sh               A throwaway Postgres for the integration tests (§13.1)
Dockerfile                 golang:1.27.1-alpine → alpine:3.24 + docker-cli + docker-cli-compose
docker-compose.yml         Lighthouse's own service: server paths, docker.sock, spark network
.github/workflows/ci.yml   gofmt, vet, tests with -race (Postgres, Docker), govulncheck, builds
```

**Dependencies point one way:**

```
main ─► cli ─► control
main ─► daemon ─► orchestrator ─► projects (Store) ◄── database
           │            │
           │            └─(Deployer)─► deploy ─► compose, docker, github, CoveClient
           └─► docker, compose, github, projects
main ─► cove ─► CoveClient          main ─► reposjson ─► projects, github
```

`cli` only knows `control.Service`. `orchestrator`, `daemon` and `deploy` only know interfaces (`projects.Store`, `Deployer`, `Commits`, `Compose`, `Docker`, `Source`, `Secrets`, `Containers`, `Health`), each tested with a fake in place of the real thing. The in-memory Store passes the same tests as the Postgres one (`projectstest.RunStoreTests`). `compose`, `docker`, `database` and one end-to-end `deploy` test also run against the real things when they're available. Only `config` reads Lighthouse's environment; `compose` passes a project only what docker needs, plus its secrets.

**Where things go:**
- A new CLI command → a `cli/cmd_*.go` function plus one entry in `commandTable()`; help and completion pick it up. If it asks the daemon for something new: a `control.Service` method, its route in `control/server.go`, a `Client` method, and the `daemon` implementation.
- What happens when → `orchestrator`. How a deploy is done → `deploy`.
- Something stored → a `projects.Store` method, implemented in `database/store.go` and `projectstest/store.go`, with a case in `projectstest/contract.go`. A schema change → a new migration ([§14.1](#141-databases-and-migrations)).
- A setting → a field in `config.Config`, read in `Load`, checked in `ValidateServe`. A secret → Cove, read in `cove.ReadSecrets`.

**State and concurrency:** all state is in Postgres; nothing is cached in memory, so the CLI, the check loop and `lighthouse import` always see the same thing. A scan runs at most once at a time (a second is refused); deploys run one at a time (a second waits), whether they come from the check loop or the CLI.

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
8. **Running.** `status` says `running`. The first check starts at once, then one every `LIGHTHOUSE_POLL_INTERVAL`.
9. **Stopping** (`docker stop`, SIGTERM). Lighthouse stops answering the CLI and stops starting new work. A deploy that has already swapped finishes its check (and rollback, if needed); Lighthouse waits up to 100 s for it (`stop_grace_period: 2m` in the compose file), then closes the database and removes the socket. A deploy still building is cancelled, which changes nothing running.

A fatal error after the socket is open (for example a revoked Cove token) exits with status 1. Docker's `restart: unless-stopped` starts Lighthouse again, so the same one-line message repeats in `docker logs` until it's fixed.

### 4.2 The check loop

Unless paused, every `LIGHTHOUSE_POLL_INTERVAL`, `Scan` checks each project in turn:

```
GET /repos/<owner>/<repo>/commits?per_page=1   (Authorization: Bearer <token>)
  ├─ error / non-200 / no commits → that project's last error; go on to the next one
  └─ sha = the newest commit on the default branch

sha == the deployed commit                  → nothing to do; the last error is cleared
project broken and sha == the failing commit → not tried again (no error for the scan)
otherwise                                    → deploy it (§4.3), trigger "check"
record the check (time, count, and the error or none)
```

A scan that had failures logs one line naming the projects; `list` and `status` show each project's last error, `history <name>` every deploy, `report <name>` one deploy step by step.

**Failures and the broken state.** Each failed deploy is one of two kinds:

| Kind | What it means | Examples | What happens next |
|---|---|---|---|
| Transient | Something around the commit had a problem; trying again may work | GitHub, Cove or the network unreachable or erroring, Docker unreachable | Tried again at the next check; doesn't count |
| Permanent | The commit itself has a problem | The compose file is invalid, a secret is missing or forbidden, the build fails, a service doesn't come up | Counted per commit. After **3** in a row for the same commit the project is **broken**: that commit isn't tried again until a new commit arrives or `retry <name>` |

`deploy <name>` always deploys, broken or not. `retry <name>` clears the count and deploys. A success clears everything.

### 4.3 The deploy pipeline (`deploy.Deployer.Deploy`)

Deploys run one at a time; a second waits for the first. Each step has a deadline; its output (the last 16 KB, secret values replaced by `[secret]`) is kept with the deployment and written to Lighthouse's log as it happens.

| Step | What happens | The running version | If it fails |
|---|---|---|---|
| `fetch` (2 min) | Downloads the exact commit as a tarball through the GitHub API (works for private repositories); unpacks it into `<STAGING_PATH>/.incoming/<commit>`, keeping file modes, refusing paths and links that leave the folder, at most 2 GB | Serving | Transient for GitHub or network trouble; permanent for a missing commit or a bad archive |
| `inspect` | `docker compose config` reads the compose project's name (its `name:`, or, without one, the repository's name lowercased) and its services; the project claims the name (two projects can't share one); the files move to `<STAGING_PATH>/<compose project>/<commit>`; `config --variables` lists the `${...}` it uses | Serving | Permanent |
| `secrets` | Every variable **without a default** is fetched from Cove in one batch; one with a default is a setting and isn't fetched (a warning says so) | Serving | Permanent for a missing, forbidden or invalid key; transient for Cove trouble |
| `build` (20 min) | The running images are tagged `<image>:lh-<commit>` for rollback; `docker compose -p <project> build`; the new images get their `lh-<commit>` tag | Serving | Permanent |
| `swap` (5 min) | `docker compose -p <project> up -d --no-build --remove-orphans`: Compose replaces the containers. **The only downtime** | Replaced | Permanent; rolls back |
| `verify` (2 min) | Every service must be up: healthy if it has a healthcheck; else running, without restarting, for 10 s; a one-off service (no restart policy) may also have exited with 0. Unhealthy, crashed or restarting fails at once | New version | Permanent; rolls back |
| `cleanup` | Keeps the new folder and the one it replaced; removes other deploy folders, rollback tags of other commits, images nothing uses, and (daily) build cache unused for a week. Never fails a deploy | New version | Only reported |

**Rollback** (after a failed `swap` or `verify`): the previous images are put back under their usual names, the previous version's secrets are fetched, and `docker compose up` runs from the previous version's folder. The deployment is recorded as `rolled_back`. It isn't possible on a project's first deploy (nothing ran before), or when the previous version wasn't deployed by this Lighthouse (its folder isn't kept); then the deployment is `failed` and says so.

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

`deploy`, `retry` and `scan` wait until they're done (a deploy can take minutes; progress is in `docker logs -f lighthouse`). While Lighthouse is still starting, they're refused with the reason.

---

## 5. Configuration

All settings are environment variables; Lighthouse's secrets (the GitHub token and both database URLs) come from Cove instead ([§8](#8-cove-integration)). In Docker they're written out in `docker-compose.yml`'s `environment:` (none of them is a secret). For local development they can come from a `.env` file ([§13.1](#131-running-locally)).

| Variable | Required for `serve` | Description |
|---|---|---|
| `COVE_URL` | Yes | Cove's base URL, `http://cove:2100` in Docker |
| `COVE_TOKEN_PATH` | Yes | Where Lighthouse's Cove token is kept. Docker: `/app/vault/cove/token` |
| `STAGING_PATH` | Yes | The deploy folders: `<compose project>/<commit>`, the running version and the one before it. Lighthouse owns everything in it, so it can't be `/`, `.` or a system folder. **In Docker it must be mounted at the same path on the host and in the container** (`/srv/server/staging`), so a relative bind mount in a project's compose file means the same files to Compose and to Docker |
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
| `deployed_sha`, `deployed_at` | The last **successful** deploy |
| `last_checked_at`, `check_count` | The last check of GitHub |
| `last_error`, `last_error_at` | The last problem (a check or a deploy), cleared by the next success. Cut at 2,000 characters |
| `failure_count`, `failing_sha`, `broken` | Permanent failures in a row of one commit; at 3, `broken` |

**`deployments`**: one row per deploy attempt, written when it ends.

| Column | Notes |
|---|---|
| `project_id` | Its project; the rows go when the project is removed |
| `sha` | The commit deployed |
| `trigger` | `check` (a new commit found by a check or `scan`) or `manual` (`deploy`, `retry`) |
| `status` | `succeeded`, `failed` (nothing changed) or `rolled_back` (the previous version was put back) |
| `failure_kind`, `failed_step` | For a failure: `transient`/`permanent`, and the step |
| `started_at`, `finished_at`, `error` | |

**`deployment_steps`**: each step of a deployment: `position`, `step`, `status` (`succeeded`, `failed`, `skipped`), times, and `log`, the last 16 KB of its output with secret values hidden. `report <name>` shows them.

**`goose_db_version`**: which migrations have run.

**Who may do what** (migrations `00002`, `00003`): `lighthouse_app` reads and changes `projects`, but can only **add** to `deployments` and `deployment_steps`, never change or delete history, and only reads `goose_db_version`; it can't create anything. `lighthouse_reader` reads everything (pgAdmin). Everything is owned by `lighthouse_owner`.

**From the pre-1.0 Lighthouse:** `lighthouse import` reads its `repos.json` and adds each project with its state: when watching started, the deployed commit, checks, last error. It's safe to run twice, because existing projects are skipped. Steps in [§10](#10-deploying-lighthouse).

---

## 7. Connecting a project (the contract)

What a repository needs for Lighthouse to deploy it. `lighthouse help setup` is the short version.

**Required**

1. **A compose file at the repository's top** (`compose.yaml`, `compose.yml`, `docker-compose.yaml` or `docker-compose.yml`). Lighthouse runs **every service** in it.
2. **The deployable code on the default branch** (usually `main`). Lighthouse deploys its newest commit. Private repositories work, as long as Lighthouse's GitHub token can read them.
3. **Every `${KEY}` without a default exists in Cove** under exactly that name, following the naming standard `PROJECT_PLATFORM[_ROLE]_TYPE` ([§14.2](#142-secrets-cove)). Plain settings are written as values, never `${...}`.
4. **Persistent data in an absolute host path** (`/srv/server/storage/<project>/...:/app/data`) or a named volume. A relative bind mount (`./config.yml:/app/config.yml`) works for files **in the repository**, since each version's folder is kept while it runs, but anything written there is gone with the next version.

**Recommended**

5. **`name:` at the top of the compose file.** It's the compose project's name, which Lighthouse finds the containers by. Without it, the repository's name (lowercased) is used. Two Lighthouse projects can't share one: the second's deploy is refused, naming the first.
6. **A healthcheck on every service that serves something.** Lighthouse waits until it's healthy, and rolls back if it isn't. Without one, a service only has to stay running for 10 seconds.
7. **A `restart:` policy** (`unless-stopped`) on long-running services. A service without one is treated as a one-off job, which may exit with 0.
8. **Unique names on `spark`.** Docker's DNS on a shared network answers every container that has a name: a service called `web` in two projects on `spark` makes `http://web` reach either, at random ([B21](#b21-service-names-can-collide-on-the-spark-network)). Give anything on `spark` a name no other project uses: a `container_name`, or a network alias such as `landing-web`.
9. **Join the external `spark` network** to reach Cove, sparkdb or each other (`sparkdb:5432`, `cove:2100`).

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

Added as `add personalWebsite https://github.com/lsariol/landing`, it's `personalWebsite` in the CLI, `website` to Compose, and `logs personalWebsite:bot` shows the bot.

**A project that writes to Cove** (today only botsuite) also gets `COVE_URL=http://cove:2100` and `COVE_TOKEN=${<PROJECT>_COVE_TOKEN}` and uses CoveClient v1. See Cove's `DOCUMENTATION.md` §9.

**Cove itself** is deployed by Lighthouse. Its secrets are fetched before anything is touched, and Cove keeps running until its new version is built, so a placeholder in Cove's compose file is now safe.

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

**Because the token can read every key, Lighthouse decides which project gets which secret.** Today it gives any project whatever it asks for ([S1](#s1-any-watched-repo-can-read-any-secret-in-cove)).

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
| `docker exec lighthouse /lighthouse migrate [status\|up]` | Show or apply database migrations, as the migrator (they're applied at startup anyway) |
| `docker exec -i lighthouse /lighthouse import -` | Move a pre-1.0 `repos.json` into the database (from standard input, or a file path); safe to run twice |

### Commands

`help` lists them by group; `help <command>` shows every form, flags and examples; `help setup` and `help failed` are guides. Project names aren't case-sensitive. The container commands take `<name>` (every service) or `<name>:<service>` (one).

| Command | Usage | Notes |
|---|---|---|
| `list`, `ls`, `l` | `list` | Every project: repository, deployed commit, last deploy, last check; its last error, and whether it's broken |
| `add` | `add <name> <url>` | Names: letters, digits, `-`, `_`, up to 64. The URL may have `www.`, a trailing `/` or `.git`. Deployed on the next check |
| `remove`, `rm` | `remove <name> [--keep] [--yes]` | Asks first. Stops watching it and removes its containers and networks (`docker compose down`); its data and images stay. `--keep` leaves the containers running, untracked |
| `rename` | `rename <name> <new-name>` | Lighthouse's name only; repository and containers unchanged |
| `set-url` | `set-url <name> <url>` | Watch another repository under the same name |
| `deploy`, `rebuild` | `deploy <name\|all> [--yes]` | Deploy the latest commit now, even if deployed or broken, and wait. `all` asks first |
| `retry` | `retry <name>` | Clear a broken project's failures and deploy it now |
| `history` | `history <name> [count]` | The last deploys (10, up to 100), newest first: #, when, trigger, result (`succeeded`, `failed at <step>`, `rolled back at <step>`), commit, duration, the error's first line |
| `report` | `report <name> [n]` | One deploy (1 = the latest): commit, start, result and its kind, then each step with its status, duration and output |
| `scan` | `scan` | Check every project now and deploy new commits; refused while another scan runs |
| `pause` / `resume` | | Stop / restart automatic checks. `deploy` and `scan` still work. A restart of Lighthouse resumes |
| `start` | `start <name[:service]\|all>` | Start containers |
| `stop` | `stop <name[:service]\|all> [--yes]` | Asks first |
| `restart` | `restart <name[:service]\|all> [--yes]` | `all` asks first |
| `logs` | `logs <name[:service]> [lines]` | The last lines (50, up to 10,000) of each container, on stdout; with a heading per service when there are several |
| `status` | `status` | Version, environment, startup state, automatic deploys, Cove, GitHub token, database and schema; then each project (running, degraded, stopped or missing; its commit, last deploy and check, or `broken`) with a line per service (state and health). Exits 1 and lists what needs attention when something does |
| `help`, `h` | `help [command\|setup\|failed]` | |
| `exit`, `quit` | | Leave the shell |

### Conventions

- **Output:** data (tables, logs, help) on stdout; messages on stderr, each starting with a symbol: `✓` success, `!` warning or wrong usage, `✗` error, `?` question. Color only on a terminal and never with `NO_COLOR`. Wrong arguments show `Usage: <form>`. Errors say how to fix them.
- **Confirmation:** `remove`, `stop`, and the `all` forms of `deploy`, `stop` and `restart` ask `(y/N)`; Enter means no. `--yes` / `-y` skips the question. Without a terminal (a script, or `docker exec` without `-it`) they refuse and point at `--yes` instead of guessing.
- **Scope:** container commands only act on watched projects' containers, found by their compose project, never on anything else on the host (`stop sparkdb` is "no project named sparkdb").
- **Adding a command:** see "Where things go" in [§3](#3-architecture).

---

## 10. Deploying Lighthouse

Lighthouse is deployed **by hand** on the server. It doesn't deploy itself yet: the deploy would replace the very container doing it ([F9](#f9-self-update)).

**First deploy** (on the Debian server, over SSH):

```bash
# The folders the bind mounts need.
sudo mkdir -p /srv/server/storage/lighthouse/cove /srv/server/staging

docker network inspect spark >/dev/null 2>&1 || docker network create spark

# In Cove (docker exec -it cove /cove shell):
#   token create lighthouse --allow '*'
#   create LIGHTHOUSE_GITHUB_TOKEN <a fine-grained, read-only GitHub token>
#   bootstrap open lighthouse
# The database and its URL keys: §10.1.

git clone https://github.com/lsariol/lighthouse.git && cd lighthouse
docker compose up -d --build
docker logs -f lighthouse     # success: "migration applied" (first start only), then "Lighthouse ready"
docker exec lighthouse /lighthouse status   # success: "✓ Everything is healthy." (or the list of what isn't)
```

**Updating:**

```bash
cd ~/lighthouse && git pull && docker compose up -d --build
```

State lives in the database and the bind mounts, so rebuilding is safe. A deploy that has already swapped when Lighthouse stops finishes its check first (up to 2 minutes); one still building is cancelled, which changes nothing running.

**Upgrading from the pre-1.0 Lighthouse** (one time, on the server):

1. The database setup, [§10.1](#101-database-setup-once-by-hand).
2. Empty the old scratch folders: `sudo rm -rf /srv/server/staging/* /srv/server/download`. (The new Lighthouse keeps each running version's files in `/srv/server/staging`, mounted at that same path; nothing there is needed from before.)
3. `git pull && docker compose up -d --build`. The new compose file no longer mounts `.env`, `repos.json` or the download folder, publishes no port, uses `COVE_URL`, and mounts the staging folder at the same path inside. Success: `docker logs lighthouse` shows the migrations applied, then `Lighthouse ready` with `projects=0`.
4. Move the watched projects over, straight from the old file:
   ```bash
   docker exec -i lighthouse /lighthouse import - < /srv/server/storage/lighthouse/repos.json
   ```
   Success: one `✓ Imported <name>` per project, then `N imported, 0 already there, 0 failed.` Their deployed commits come along, so nothing redeploys unless the default branch has moved since.
5. `docker exec lighthouse /lighthouse status`: every project listed, with its services running. A project whose compose file names its project differently from its repository (like `landing` → `website`) shows `missing` until its next deploy teaches Lighthouse the name: `deploy <name>` does it now.
6. Afterwards, `/srv/server/storage/lighthouse/repos.json` and `/srv/server/storage/lighthouse/.env` can be deleted (keep a copy of `repos.json` until you're happy).

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
3. Create its host folders under `/srv/server/storage/<project>/`.
4. `lh add <name> https://github.com/LSariol/<Repo>`, then `lh scan` (or wait for the next check). Watch `docker logs -f lighthouse`.
5. Check: `lh status` shows its services running; `history <one of its keys>` in Cove shows `lighthouse`.

### Removing a project

`lh remove <name>` stops watching it and removes its containers and networks (`docker compose down`); its data under `/srv/server/storage` and its images stay. `lh remove <name> --keep` leaves the containers running, untracked.

### A deploy failed

`lh help failed` is the short version.

1. **Nothing went down.** The running version kept serving while the new one was prepared; if the new one started but didn't come up healthy, the old one was put back (`rolled back` in `lh history`).
2. **See why:** `lh report <name>` shows each step of the last deploy with its output. The error is also on `lh list` and `lh status`.
3. **What happens next:** a transient failure (GitHub, Cove, the network) is tried again at the next check. A permanent one (the commit itself) is tried again too, but after 3 failures of the same commit the project is **broken** and waits.
4. **Fix it:** push a fix (a new commit is always tried), or, for a fix outside the repository such as a missing secret in Cove, `lh retry <name>`.

### Rotating the GitHub token

Create the new token, `update LIGHTHOUSE_GITHUB_TOKEN <token>` in Cove, `docker restart lighthouse`, then revoke the old token on GitHub.

### Rotating Lighthouse's Cove token

Delete `/srv/server/storage/lighthouse/cove/token`, `bootstrap open lighthouse` in Cove, `docker restart lighthouse`. If it leaked: `token revoke lighthouse` first. It can read **every** secret, so every secret should then be considered exposed (Cove `DOCUMENTATION.md` §14). After `token rotate` or `revoke` without a new bootstrap, Lighthouse refuses to start and says how to fix it.

### After a server reboot

Every container restarts by its own `restart:` policy. Lighthouse waits for Cove and for its database (`lh status` shows which), then starts checking. It doesn't start any project itself.

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
| `Can't reach the Lighthouse daemon at /run/lighthouse/control.sock` | Lighthouse isn't running, or you ran the CLI outside its container. Use `docker exec … /lighthouse …`; check `docker ps` and `docker logs lighthouse` |
| `… needs confirmation, and there's no terminal to ask on` | Use `docker exec -it`, or add `--yes` |
| `Lighthouse is still starting (…)` | Every command but `status` waits for startup; `status` and `docker logs lighthouse` say what it's waiting for |
| A project's last check: `GitHub: 401` | The GitHub token is wrong or expired. Update `LIGHTHOUSE_GITHUB_TOKEN`, restart |
| `GitHub: 403 (… rate limit …)` or `429` | Rate limit (5,000 requests an hour per token; each project uses 360 an hour at the default interval). Raise `LIGHTHOUSE_POLL_INTERVAL`. Transient: retried |
| `GitHub: 404` | Wrong URL, a renamed repo (`set-url`), or a private repo the token can't see. Other projects are still checked |
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

What protects what today, and what is left to you. Issues are in [§15](#security-issues).

| Asset | Protection | Gap |
|---|---|---|
| Cove secrets | Lighthouse holds its own read-only token, checked at startup; values only in process environments | The token reads **everything**, and Lighthouse hands any key to any repo that names it ([S1](#s1-any-watched-repo-can-read-any-secret-in-cove)) |
| The host | – | Lighthouse has the Docker socket (= root). Whatever is on `main` of a watched repo runs with whatever privileges its compose file asks for ([S2](#s2-a-push-to-main-is-root-on-the-server)) |
| The GitHub token | Fine-grained and read-only; stored in Cove, held in memory; only its length is logged | – |
| Lighthouse's Cove token | File mode 0600, written atomically by CoveClient, on a host folder | Readable by anyone with root on the server, and usable from any container on `spark` ([S1](#s1-any-watched-repo-can-read-any-secret-in-cove)) |
| The CLI | A Unix socket inside the container, mode 0600, never published; reached only with `docker exec` | Anyone who can `docker exec` can use it (they already have root) |
| What the CLI can touch | Only watched projects' containers, found by their compose project | – |
| Network | No published port; joins `spark` to reach Cove | – |
| Secrets in logs | Values and tokens are never printed; every value fetched for a deploy is replaced by `[secret]` in its stored output, Lighthouse's log and its errors | A value shorter than 4 characters isn't hidden |

**Not protected against:** anyone with root or Docker access on the server, anyone who can push to `main` of a watched repo, and anyone who controls the GitHub account. Those three are each equivalent to full control of the server and every secret in Cove.

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

**Everything stays in the repository.** A local run reads and writes only inside `.dev/`: the staging and download folders, the control socket and the dev Cove token. Its database is `lighthouse_db` on `sparkdb-dev`, through the URLs in the dev Cove. `.dev/` is gitignored and kept out of Docker builds. Go tests use temporary folders that the test runner removes afterwards.

**Checks before a commit** (CI runs the same on every push to `main` and `release/**`):

```bash
gofmt -l .                       # prints nothing
go vet ./...
scripts/test-db.sh up            # once: a throwaway Postgres, set up by scripts/db/setup.sql
eval "$(scripts/test-db.sh env)" # in each new shell: points the integration tests at it
go test -race ./...              # -race needs cgo: gcc on Windows (msys2), or run in CI
go run golang.org/x/vuln/cmd/govulncheck@latest ./...   # "No vulnerabilities found."
scripts/test-db.sh down          # when done
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

- **Migration rules** (same as Cove): files `000NN_description.sql`, starting with `-- +goose Up`; `$$` blocks wrapped in `-- +goose StatementBegin` / `StatementEnd`; **never edit a migration that has run in prod**; forward-only, so a mistake is fixed by the next migration; grants for `_app`/`_reader` live in migrations too. Migrations are embedded in the binary and run at every startup as the migrator (with a Postgres advisory lock); the app refuses to start on a database missing a migration it needs, but a newer database is fine (rolling back the code after an additive migration still works).
- The Admin SQL for Lighthouse is `scripts/db/setup.sql`, run by hand ([§10.1](#101-database-setup-once-by-hand)).
- **Lighthouse's migrations** (`internal/database/migrations/`):

  | Migration | What it does |
  |---|---|
  | `00001_projects` | `projects` and `deployments` ([§6](#6-data-the-database)) |
  | `00002_role_grants` | `lighthouse_app`: read/write `projects`, append-only `deployments`, read `goose_db_version`; `lighthouse_reader`: read all, including future tables |
  | `00003_pipeline` | `projects`: `compose_project` (unique), `failure_count`, `failing_sha`, `broken`; `deployments`: `rolled_back` status, `failure_kind`, `failed_step`; `deployment_steps` (append-only for the app) |

  `lighthouse migrate status` lists them with the time each was applied.

### 14.2 Secrets (Cove)

- Every secret lives in Cove. Nothing secret is hard-coded, committed, or written to a file outside Cove (Lighthouse's own token file excepted).
- **Key names:** `PROJECT_PLATFORM[_ROLE]_TYPE`; capitals, digits and `_` only. `TYPE` ∈ `API_KEY`, `CLIENT_ID`, `CLIENT_SECRET`, `ACCESS_TOKEN`, `REFRESH_TOKEN`, `TOKEN`, `URL`, `PASSWORD`, `SECRET`, `ID`. `SHARED_` for keys several projects use, `SPARK_` for server infrastructure.
- **Only real secrets go in `${...}`** in a compose file Lighthouse deploys. Plain settings are written out as values.
- A project that **writes** to Cove gets its own token, injected as `COVE_TOKEN=${<PROJECT>_COVE_TOKEN}` with `COVE_URL=http://cove:2100`, and uses CoveClient v1. There is no master token.
- **Never print secret values, tokens or `.env` contents** in logs, CLI output or errors. Show lengths or key names only. Temporary copies of secrets stay outside repos and are deleted when done.

### 14.3 Git and releases

- Prod is only updated from `main`. Changes land on short-lived branches, merged into `release/<version>`.
- **Release:** update `CHANGELOG.md`, fast-forward merge `release/<version>` into `main`, tag `vX.Y.Z`, push the tag, then push `main`. Lighthouse is updated by hand on the server ([§10](#10-deploying-lighthouse)). The projects it watches deploy on that push.
- Semantic versioning.

### 14.4 Prod changes

Every change that prod needs (a new host folder, a Cove key, Admin SQL, a compose change on the server) is logged in the prod rollout plan's change log (Appendix B), with what to do and how to undo it, before the release that needs it.

---

## 15. Known issues

Found in a full review of the code on `release/1.0.0` (commit `9fef7fa`). Open issues point at where the code is now; fixed ones keep the locations from the review. Each issue has an ID used in the roadmap. **Severity:** Critical (causes outages or exposes secrets), High (breaks normal use), Medium (breaks an edge case or makes operations painful), Low (polish).

### Bugs

#### B1. A failed deploy takes the project down and retries forever
**Critical.** `internal/deploy/deploy.go` (the stop before unpack), `internal/orchestrator/orchestrator.go` (`check`).
**Status: fixed** in step 3: build first, swap last, roll back; failures are classified, and a commit that fails 3 times marks its project broken instead of retrying forever.
`Build` stops the running container (step 3) **before** it unpacks, resolves secrets or builds. Any later failure, such as a typo in the Dockerfile, a missing Cove key, Cove being down, or a compose error, leaves the project stopped. Then, because the new SHA is only saved on success, the next scan 10 s later sees the same "new" commit and runs the whole deploy again. It downloads the archive again, stops the container again and fetches every secret again (filling Cove's audit log), indefinitely. The same ordering means a project whose compose file uses `${...}` can never be deployed while Cove is down, and Cove itself could not be deployed if it ever used a placeholder.
**Fix:** resolve secrets and validate the compose file first; drop the explicit stop and let `docker compose up -d --build` replace the container only after a successful build; record every attempt, with backoff, and a "broken" state after N failures for the same SHA (from `todo.txt`).

#### B2. One failing repo blocks every repo after it
**High.** `internal/watcher/watcher.go:69-76, 82-86`.
**Status: fixed** in the foundation (`d090826`..`4fc4f79`): a failing project is recorded and skipped; the rest are still checked.
`Scan` `return`s on the first GitHub error or build error. A deleted repo, a typo in a URL, or a broken build means every repo later in the list is never checked again.
**Fix:** record the error on that repo and `continue`.

#### B3. Unexpected GitHub responses crash Lighthouse
**High.** `internal/watcher/github.go:38-42`.
**Status: fixed.** GitHub answers are decoded into a typed struct (one commit, `per_page=1`); unexpected bodies and empty repositories are errors on that project, never an exit. No panics are left in startup.
`log.Fatal` if the body isn't a JSON array (for example an error object or an HTML page from a proxy); a panic on `commits[0]` for an empty repository; a panic on the unchecked `.(string)` type assertion. Each one exits the whole daemon. Also `panic` in `main.go` when Docker inspect of `cove` fails.
**Fix:** typed decoding, return errors, never exit from library code.

#### B4. The CLI and the watcher race each other
**High.** `Watcher.WatchList` is read and written by two goroutines without a lock.
**Status: fixed.** The watchlist is behind a lock and entries are found by name; one scan at a time (a second is refused); one deploy at a time (a second waits). Tests cover removal during a scan. The full queue comes with the orchestrator (step 5).
- `remove` during a scan shifts the slice, and `Scan` then writes repo A's stats into repo B's slot (or panics with index out of range).
- `rebuild` or `scan` from the CLI can run a build **at the same time** as the background loop. Both call `cleanUp()` on the same staging and download folders and delete each other's files mid-deploy.

`go test -race` would flag it immediately.
**Fix:** one owner of state (the database), one deploy queue processed by a worker, a per-project lock; the CLI enqueues instead of building.

#### B5. Startup failures are silent and permanent
**High.** `internal/watcher/watcher.go:41-49`, `cmd/lighthouse/main.go:54`.
**Status: fixed.** Startup waits for Cove (`WaitForReady`), checks the token (`Auth`), and stops with a one-line explanation when the GitHub token is missing or the watchlist can't be loaded. `status` shows startup progress.
- `loadGitCredentials()`'s error is ignored (the `if err != nil` checks the previous `err`). After a reboot where Cove or sparkdb comes up after Lighthouse, Lighthouse sends an empty token forever.
- `go watcher.Run()` discards its error: a missing or corrupt `repos.json` means nothing is watched, with one line in the log and the CLI still running as if all is well.
- No `WaitForReady` on Cove before using it.

**Fix:** wait for Cove (`WaitForReady`) with a timeout, verify the token (`Auth`), fail loudly (exit non-zero so Docker restarts it) or retry, and refresh the GitHub token on 401.

#### B6. The placeholder parser is wrong for common Compose syntax
**High.** `internal/deploy/compose.go` (`placeholder`).
**Status: fixed** in step 3: Compose itself lists the variables (`config --variables`); one with a default isn't a secret, `$${...}` isn't a variable.
The regex `\$\{([^}:]+)(?::[^}]*)?\}`:
- treats `${VAR:-default}` and `${VAR:?err}` as required secrets (the cause of the Cove `v1.0.0-rc.1` failed deploy),
- reads `${VAR-default}` as a key named `VAR-default`,
- matches inside `$${VAR}`, Compose's escape for a literal `$`, which is common in healthchecks (`pg_isready -U $${POSTGRES_USER}`),
- misses `$VAR` without braces. That one is then filled from **Lighthouse's own environment**, or left empty.

**Fix:** ask Compose itself with `docker compose config --variables --format json` (Compose 2.24+). It lists every variable, braced or not, with its default and alternate values, and it already leaves out escaped `$${...}`. Fetch the variables that have no default; skip those with one unless the key exists in the project's scope. Checked on Compose 2.38: `${REQ}` → no default; `${OPT:-x}` → default `x`; `$${ESC}` → not listed; `$BARE` → listed.

#### B7. Relative bind mounts point at the wrong place
**Medium.** `internal/deploy/compose.go`, `docker-compose.yml` (the staging mount).
**Status: fixed** in step 3 for files in the repository: the deploy folder is mounted at the same path on host and container, and each running version's folder is kept. Data must still use an absolute path (§7).
Compose runs **inside** Lighthouse's container, so `./data` in a project's compose file resolves to `/app/server/staging/<repo>-main/data`. The host's Docker daemon then creates that path **on the host** (where it doesn't exist) as an empty directory. The staging copy is deleted after the deploy anyway.
**Fix:** mount the staging folder at the **same path** inside and outside (`/srv/server/staging:/srv/server/staging`), unpack each deploy into a stable per-project folder, and document that persistent data uses absolute paths under `/srv/server/storage/<project>/`.

#### B8. Downloads: no timeout, no status check, wrong branch, wrong folder name
**Medium.** `internal/deploy/source.go` (`download`), `internal/github/github.go` (`LatestCommit`), `internal/deploy/compose.go` (the folder name).
**Status: fixed** in step 3: the exact commit is downloaded through the API (with the token, so private repositories work), with a deadline, checked status, and whatever its top folder is called; the default branch is both checked and deployed.
- `http.Get` uses the default client with **no timeout**. A stalled download freezes all deploys for good.
- The status code isn't checked: a 404 page is saved as `<repo>.zip` and fails later as "not a valid zip file".
- The archive isn't pinned to the detected SHA. If `main` moves between check and download, Lighthouse deploys a commit it didn't record.
- The check reads the **default** branch's commits (`/commits`), while the download is always `main`.
- No auth on the download, so private repos can't be deployed.
- The unpacked folder is assumed to be `<lowercase repo>-main`. GitHub currently names it that way (checked 2026-10-05: `cove-main`, `lighthouse-main`), but it's an undocumented detail, and the API's zipball uses a different name (`<owner>-<repo>-<sha>`).

**Fix:** download `GET /repos/{owner}/{repo}/zipball/{sha}` (or `tarball`) with the PAT and a timeout, check the status, and use the archive's single top-level folder, whatever it's called.

#### B9. Unzipping drops file permissions
**Medium.** `internal/deploy/source.go` (`extract`). `os.Create` makes every file `0666 &^ umask`. Executable scripts committed to a repo lose `+x`, so `RUN ./script.sh` or an `ENTRYPOINT ["./start.sh"]` fails. (The other half of the old finding, a `defer` inside the loop holding every file open, is fixed: each entry is closed as it's written.)
**Status: fixed** in step 3: unpacked from a tarball, keeping file modes.
**Fix:** `os.OpenFile(path, flags, file.Mode().Perm())`, close per iteration. Using the tarball, which keeps modes, is simpler still.

#### B10. No timeouts and no graceful shutdown
**Medium.** `internal/deploy/compose.go` (`exec.Command` without a context), `cmd/lighthouse/main.go` (the 8-second shutdown wait).
**Status: fixed** in step 3: every docker compose call has a deadline; on shutdown a build is cancelled, and a deploy that has swapped finishes its check (stop_grace_period 2m).
`docker compose` runs with `exec.Command` (no context, no timeout). A hung build or pull freezes every deploy. On SIGTERM, `main` returns at once and kills a deploy mid-way. The loop uses `time.Sleep` and ignores the context.
**Fix:** `exec.CommandContext` with a per-deploy timeout; on shutdown stop taking new work, wait for the current deploy (up to Docker's stop timeout, which should be raised with `stop_grace_period`).

#### B11. Lighthouse can't deploy itself, and Cove's deploy is fragile
**Medium.** If `LightHouse` is ever added to its own watchlist, `Build` stops the `lighthouse` container, which kills the process doing the deploy. A stop through the API counts as a manual stop, so `unless-stopped` doesn't bring it back: Lighthouse stays down. Cove deploys only because its compose file has no placeholders ([§7](#7-connecting-a-project-the-contract)).
**Status: partly fixed.** Cove's deploy is safe now (secrets are fetched before anything changes). Lighthouse deploying itself is F9.
**Fix:** B1's fix handles Cove. Self-deploy is wanted for v1.0.0 and needs a helper container ([F9](#f9-self-update)).

#### B12. "Start all" at startup does nothing; `Builder.WatchList` goes stale
**Medium.** `cmd/lighthouse/main.go:52`, `internal/watcher/watchlist.go:234`.
**Status: fixed** by removal: Lighthouse no longer tries to start projects or Cove at boot, and the builder's stale copy of the watchlist is gone (`start all` etc. read the live list).
`StartAllContainers()` runs before the watchlist is loaded, so it loops over nothing, and its error is ignored. `Builder.WatchList` is a copy taken once at load: repos added later are missing from `start all`, `stop all` and `exit all`, and removed ones are still in it. `StartAllContainers`/`StopAllContainers` also stop at the first error. The "start `cove` if it isn't running" step runs after Cove was already needed.
**Fix:** one source of truth for the project list; decide whether Lighthouse starts projects at all (restart policies already do; recommended: it doesn't).

#### B13. Names mean different things in different commands
**Medium.** `models.WatchedRepo.ContainerName` is really the **GitHub repo name** from the URL.
**Status: fixed** in step 3: nickname, repository and compose project are separate; containers are found by Compose's labels; `<name>:<service>` addresses one service; `remove` removes the containers (`--keep` leaves them).
- `start`, `stop`, `restart` and `logs` take a raw **container name**, so `stop sparkdb` or `stop cove` works on unmanaged infrastructure.
- `rebuild` takes a **display name** (case-insensitive); `remove` takes a display name (case-sensitive).
- `status` and `stop all` use `lower(repo name)`.
- Projects with more than one service (app + worker + redis) only have their one same-named container stopped, started or checked.
- `remove` leaves the container running.

**Fix (decided 2026-10-06, in step 3):** keep the nickname, the repository and the compose project apart; the compose project comes from the compose file's `name:`; containers are found by Compose's labels, and `name:service` addresses one service. Details: [16.9](#169-compose-projects-and-services-decided-2026-10-06).

Seen in practice: `lsariol/landing` (compose project `website`, services `web`, `www`, `bot`) deploys, but `stop`, `start`, `restart`, `logs` and `status` look for a container named `landing` and find none.

#### B14. CLI correctness
**Low–Medium.** `internal/cli/cli.go`, `internal/watcher/watchlist.go`.
**Status: mostly fixed** by the new CLI: duplicates are errors, `set-url` updates the container name, URLs with `www.`, `/` or `.git` are accepted, `all` reports each project and fails if any did, `deploy` records the commit, and there's no `docker attach`/Ctrl-C trap. Checking that a repository exists comes with the pipeline.
- `add` on a duplicate prints "already being watched" **and** "is now being watched" (`AddNewRepo` returns `nil`).
- `change` / `update url` keep the old `ContainerName`, so pointing an entry at a different repo deploys into the wrong folder. `ChangeRepoURL` is a near-duplicate that nothing calls.
- `parseURL` rejects `https://github.com/o/r/`, `.../r.git`, `http://`, `www.github.com`; it doesn't check that the repo exists.
- `restart all` prints "All containers restarted" even when some failed. `scan` ignores its error. `exit <anything else>` does nothing.
- `rebuild` doesn't record the SHA or stats.
- In Docker, `exit` is a restart (because of `unless-stopped`), and Ctrl-C in `docker attach` stops Lighthouse.

#### B15. `repos.json` writes are unsafe
**Low** (goes away with the database). Written non-atomically with `os.WriteFile`, once per repo per cycle (about 36 writes a minute with 6 repos). A crash mid-write corrupts it, which silently stops all watching (B5). Atomic rename doesn't work on a single-file bind mount, another reason to move to Postgres.
**Status: fixed** in step 2: there's no watchlist file any more; state is in Postgres.

#### B16. Stats are half-implemented
**Low.** `builds.*`, `downloads.*` and `lastSeenTag` are never written; `lastErrorMessage` is never cleared after a success; `UpdateUpdateStats` runs before the build, so `updateCount` counts attempts, not deploys. `list` shows none of the useful fields (last deploy result, last error).
**Status: fixed** in step 2: every field in the database is written, and every deploy attempt is recorded in `deployments` (`history <name>`).
**Status: partly fixed.** The last error is cleared after a success and shown by `list` and `status`. The rest goes away with the database.

#### B17. A `.env` file is required but not needed
**Low.** `internal/config/envs.go`. `config.Load` panics unless `./.env` or `/app/vault/.env` exists, and it ignores `APP_ENV_PATH`. Everything Lighthouse reads now comes from the compose `environment:`. With the bind mount, a missing host file becomes a directory and Lighthouse crash-loops.
**Status: fixed.** The `.env` file is optional (local development only) and no longer mounted in Docker.
**Fix:** make the `.env` optional (dev convenience only) and remove the mount.

#### B18. Workspace clean-up is unguarded
**Low.** `internal/builder/workspace.go`. `cleanupAll` deletes everything inside `STAGING_PATH` and `DOWNLOAD_PATH`. A typo (`STAGING_PATH=/srv/server/`) would wipe the server's storage. It fails on the very first run if the folders don't exist, creates an unused `Working` folder, and its error message always says `STAGING_PATH`.
**Status: fixed.** Startup refuses `/`, `.` and system folders; each deploy has its own folder, and cleanup only removes folders of its own compose project.
**Fix:** one Lighthouse-owned work root, refuse `/` and anything that isn't a dedicated folder, per-deploy subfolders created with `os.MkdirTemp`.

#### B19. Old images and build cache are never removed
**Medium** (operational). Each deploy leaves the previous image as `<none>:<none>`, plus build cache. On a home server that fills the disk over months.
**Status: fixed** in step 3: each deploy prunes old folders, old rollback tags, unused images and (daily) week-old build cache.
**Fix:** after a successful deploy, prune dangling images for that project (label-filtered) and periodically prune build cache older than N days, keeping the last few images per project for rollback ([F3](#f3-rollback)).

#### B20. Code quality
**Low.** `gofmt` reports `internal/builder/builder.go` and `internal/watcher/watcher.go`. There's dead code (see [§17](#17-housekeeping)), mixed `fmt.Println`/`log`, no timestamps on most lines, a leftover debug line (`GOT TOKEN`), a missing newline in `ERROR IN SCAN: %v`, and no tests.
**Status: fixed.** gofmt-clean, dead code removed, `log/slog` everywhere, tests for every package except the builder (rewritten in step 3), CI on every push.

#### B21. Service names can collide on the `spark` network
**Medium** (a live risk, not a Lighthouse bug). Docker's DNS on a shared network answers to every service name and container name on it. If two projects both have a service called `web` and both join `spark`, `http://web:3000` reaches either one, at random. cloudflared routes by name, so a public site could intermittently serve another project.
**Fix:** a contract rule: anything on `spark` has a name unique on the server (a `container_name`, or a network alias such as `landing-web`); and the step-4 checks refuse a deploy whose names clash with another project's.

### Security issues

#### S1. Any watched repo can read any secret in Cove
**Critical.** `internal/deploy/compose.go` (`composeUp`).
Lighthouse's token reads `*`, and Lighthouse fetches **whatever** `${KEY}` a project's compose file names. If a compose file in one project contains `- X=${SPARK_DATABASE_ADMIN_PASSWORD}` or `${BOTSUITE_COVE_TOKEN}`, that project's container gets the value. The cause could be a mistake, a compromised dependency that edits files, or anyone with push access. This bypasses all of Cove's per-project scoping: Lighthouse acts as a "confused deputy".
A second, related risk: the token **file** (`/srv/server/storage/lighthouse/cove/token`) is a portable credential that reads everything. Every container on `spark` can reach `cove:2100`, so whoever gets a copy of that file (from a backup, a copy taken off the server, or a careless bind mount in another project) reads the whole vault **without** needing root.

**What this does and doesn't protect.** Lighthouse holds the Docker socket, so a compromised Lighthouse *process* owns Cove anyway (`docker exec cove /cove get …`, or Cove's `.env` with the vault key). Narrowing the token can't change that. What it does fix is the risks that don't need root: mistakes (a compose file copied from another project), a stolen token file, and an audit trail that only ever says "lighthouse".

**Interim (v1.0.0 pipeline): a naming rule in Lighthouse.** Step 4 of the pipeline already checks every needed key against the naming standard. It also refuses a key whose `PROJECT_` prefix belongs to a different project: plop may use `PLOP_*` and `SHARED_*`; anything else needs an exception recorded on that project in Lighthouse's database. This costs almost nothing and catches the realistic case, a copied compose file. Keep the token file out of backups that leave the server.

**Deferred (decided 2026-10-05): the full fix in Cove, revisited at the very end of the Lighthouse v1.0.0 release.** Not worth doing now. The design, for when it's revisited:

1. **Every project has a Cove token entry, even if it's injected only.** For example `token create plop --allow 'PLOP_*' --allow SHARED_TMDB_API_KEY`. If the project never calls Cove itself, the token is simply never handed out. `token show plop` and `info KEY` then show who can read what, for injected and direct projects alike.
2. **Reads on behalf of a project.** Lighthouse's token becomes `--allow 'LIGHTHOUSE_*' --delegate` instead of `--allow '*'`. A deploy calls the batch endpoint naming the project (CoveClient `GetSecretsFor(project, keys...)`, sent as a header such as `X-Cove-Project`). Cove applies **that project's** patterns, logs the read as `lighthouse for plop`, and answers `403 forbidden_key` for anything outside them. The deploy then fails before anything is touched. Keys granted to no project (`SPARK_*` admin credentials) become unreachable even to Lighthouse.
3. **Tokens tied to a network address.** `token create … --from <cidr>`: Cove refuses the token from any other source address. Lighthouse gets a fixed IP on `spark` (`ipv4_address` in its compose file), so a stolen token file is useless anywhere else. Botsuite gets the same.

With this in place the interim prefix rule could be dropped, leaving one source of truth, and Cove's audit would say which project each read was for.

#### S2. A push to `main` is root on the server
**High** (inherent, but can be narrowed). Lighthouse deploys whatever is on `main` with the Docker socket. A compose file can say `privileged: true`, mount `/` or `/var/run/docker.sock`, use `network_mode: host` or `pid: host`, or `cap_add: [SYS_ADMIN]`. Any of those turns one compromised repo, PAT with write access, GitHub account or merged pull request into control of the whole server and every secret.
**Fix (layers):**
1. A **policy check** on `docker compose config` output before deploying: refuse privileged, host namespaces, `cap_add`, `devices`, mounts of the Docker socket or of host paths outside `/srv/server/storage/<project>/`, unless the project has an explicit exception recorded in Lighthouse (cloudflared or a monitoring agent might need one).
2. GitHub: 2FA, branch protection on `main` (no force-push), and optionally deploy only **signed** commits or `v*` tags.
3. Keep watched repos to ones you own.

#### S3. The Docker socket and the control prompt
**Medium** (inherent). Lighthouse runs as root with `/var/run/docker.sock`, so anyone who can `docker attach` or `docker exec` into it, or who finds a bug in it, has the host. The prompt can stop or start **any** container (B13).
**Status: partly fixed.** The CLI reaches the daemon through a Unix socket (mode 0600, inside the container, never published) and only acts on watched projects; no TTY or stdin is attached any more. The Docker socket itself is inherent.
**Fix:** restrict commands to managed projects; when adding an API, keep it on a Unix socket or `127.0.0.1`, never public, and authenticated; drop `tty`/`stdin_open` once `shell` and one-shot commands exist ([F7](#f7-serve-shell-and-one-shot-commands)). (A Docker socket proxy doesn't help much here, because `compose up --build` needs most of the API.)

#### S4. The GitHub token may be broader than it needs
**Medium.** Nothing in the code limits it, so its scope is whatever it was created with. A classic PAT with `repo` scope can **write** to every repository you own.
**Fix:** a fine-grained PAT, **read-only** (`Contents: read`, `Metadata: read`), limited to the watched repositories, with an expiry and a reminder to rotate. A separate PAT for dev Lighthouse (already decided).

#### S5. Port 2000 is published with nothing behind it
**Low.** `docker-compose.yml:16-17` publishes `2000:2000` on every interface. Nothing listens today, but anything added later would be exposed on the LAN by default.
**Status: fixed.** The port is no longer published.
**Fix:** remove it. Bind any future health or API port to `127.0.0.1` or keep it on `spark` only.

#### S6. Outdated and vulnerable dependencies
**Medium.** `govulncheck ./...` (2026-10-05) reports **3 reachable vulnerabilities**:
**Status: fixed.** Go 1.27.1 in `go.mod` and the `Dockerfile`, `github.com/moby/moby/client`, dependencies updated, `alpine:3.24` pinned, `.dockerignore` added. `govulncheck ./...` reports no vulnerabilities, and CI runs it on every push.

| Advisory | Module | Fixed in |
|---|---|---|
| GO-2026-4887 (AuthZ plugin bypass with oversized bodies) | `github.com/docker/docker v27.1.2+incompatible` | not in this module path |
| GO-2026-4883 (off-by-one in plugin privilege validation) | `github.com/docker/docker v27.1.2+incompatible` | not in this module path |
| GO-2026-5506 (baggage header extraction) | `go.opentelemetry.io/otel v1.38.0` (indirect) | `v1.41.0` |

The two Moby issues are in daemon-side code, but `github.com/docker/docker` is the **deprecated** import path and gets no further fixes. The Docker client now lives at `github.com/moby/moby/client`. Also:
- the `Dockerfile` builds with `golang:1.25.1` (Cove and your machine are on 1.27.1, and every Go patch release carries security fixes),
- `alpine:latest` is unpinned, so the Compose CLI version changes with each rebuild,
- there's no `.dockerignore`, so `COPY . .` sends `.env`, `cove-token`, `Server/` and `.git` into the build context on a dev machine.

**Fix:** move to `github.com/moby/moby/client`, `go get -u` the rest, Go 1.27.1 in both `go.mod` and the `Dockerfile` (bumped together), pin `alpine:3.x`, add `.dockerignore`, `govulncheck` in CI.

#### S7. Secrets could leak through deploy output
**Low** (today), **Medium** once deploy logs are stored. `docker compose` output is streamed straight into Lighthouse's log. A failing build can echo interpolated values (an `args:` value, a command line). When deploy logs go into the database ([F8](#f8-structured-logs-and-deploy-history)), they must be scrubbed of every value fetched for that deploy before storing.
**Status: fixed** in step 3: every value fetched for a deploy is hidden in its stored output, Lighthouse's log and its errors.

#### S8. A revoked Cove token fails late and unclearly
**Low.** `LoadOrBootstrap` trusts an existing token file without calling `Auth()`. After `token rotate` or `revoke`, Lighthouse starts "fine" and fails on its first secret read with a bare `401`.
**Status: fixed.** `Auth()` runs at startup; a rejected token stops Lighthouse with the fix.
**Fix:** call `Auth()` at startup. On `ErrUnauthorized`, log the fix: delete the token file, `bootstrap open lighthouse`.

#### S9. Leftovers
**Low / informational.**
- Commits `8fb254c` to `a0e1c5b` printed the old Cove client secret (the master token) to stdout. That token was removed from Cove on 2026-10-05, so it's dead. No action, noted for the record.
- The dev token file `cove-token` and `.env` sit inside the repo folder (gitignored, never committed). Per the secrets rule, keep them outside the repo (point `COVE_TOKEN_PATH` elsewhere for dev).
- `RawGitResponse.json` (a captured API response) sits in the working tree; delete it.
- No zip size limit. Low risk with your own repos, but a cap is cheap.

---

## 16. Roadmap to v1.0.0

The goal: Lighthouse becomes the **orchestrator** for the server's projects. It holds the state of what should be running, deploys only what passed its checks, takes a running container down only at the last possible moment, keeps a history, and is operated through `serve` / `shell` / one-shot commands instead of `docker attach`.

The current code is small, about 1,400 lines, and its core flow has structural problems (B1, B4, S1). **The core (watcher, builder, models, config) is rewritten.** The parts that work are kept: the CoveClient usage, the zip-slip check and the CLI's command set.

### 16.1 Decisions (2026-10-05)

| Question | Decision |
|---|---|
| Where state lives | **Postgres** (`lighthouse_db` on sparkdb), schema via goose migrations. `repos.json` is imported once, then retired |
| Deploy strategy | **Build first, swap last.** Everything that can fail happens while the old container keeps serving; stopping it is the last step before the new one starts. **No blue/green** (it would need a router or alias switching, and it's unsafe for sparkdb and bots like botsuite) |
| Testing | **On Lighthouse itself, no GitHub Actions.** Contract checks, an optional Dockerfile `test` stage, an image scan ([16.3](#163-the-deploy-pipeline)) |
| Failures | Classified as **transient** (retried with backoff, not counted) or **permanent** (counted; after 3 for the same version the project is **broken** until a new version or `retry`) |
| What triggers a deploy | Per project: **branch** mode (default `main`, every new commit) or **release-only** mode (only new semver tags; [16.4](#164-release-only-mode)) |
| Lighthouse deploying itself | **Yes**, through a helper container ([F9](#f9-self-update)), in release-only mode |
| Private repos | **Supported** (downloads through the API with the token) |
| GitHub token | **Fine-grained**, read-only: `Contents`, `Metadata` on the watched repos. Expiry is warned about ahead of time ([F5](#f5-efficient-reliable-change-detection)) |
| Lighthouse's Cove token | **Unchanged for now** (`--allow '*'`). Lighthouse refuses keys from another project's prefix. The Cove-side design (delegated reads, network-bound tokens) is **deferred** and revisited at the end of v1.0.0 ([S1](#s1-any-watched-repo-can-read-any-secret-in-cove)) |
| Control | `lighthouse serve` (the daemon; clean logs in `docker logs`), `lighthouse shell` (the prompt, via `docker exec -it`), one-shot commands. No `docker attach`, no TTY on the container |
| Orchestration | A **reconcile loop**: the database says what should be running; Docker says what is; Lighthouse closes the gap ([F16](#f16-reconcile-loop)) |
| Infrastructure projects (sparkdb, Cove, cloudflared) | A separate tier: approval before deploy, backup first, deployed alone ([16.5](#165-infrastructure-projects)) |
| `lighthouse.example.yaml` | **Dropped.** Compose is the manifest. Lighthouse-only settings live in Lighthouse's database, because a repo must not be able to grant itself secrets or privileges |
| Change detection | **Polling with ETags** for v1.0.0; webhooks through cloudflared later |
| Docker SDK or Compose CLI | **Compose CLI** for anything that changes a project (always `-p <project>`); the SDK (`github.com/moby/moby/client`) for reads and the event stream |
| Lighthouse starting Cove or projects at boot | **No.** Restart policies do that. Lighthouse waits for Cove and reconciles |

### 16.2 Features, by priority

| ID | Feature | Priority | Fixes |
|---|---|---|---|
| F1 | Postgres + goose migrations, `import repos.json` | Must | B4, B15, B16 |
| F2 | The deploy pipeline ([16.3](#163-the-deploy-pipeline)) | Must | B1, B2, B6–B10, B19, S1 (interim), S2, S7 |
| F10 | Startup that waits instead of panicking | Must | B5, B17, S8 |
| F14 | Tests, CI, CHANGELOG, `.dockerignore`, Go 1.27.1, `moby/moby/client` | Must | B20, S6 |
| F17 | Checks and tests on Lighthouse ([16.3](#163-the-deploy-pipeline) steps 3–6) | Must | – |
| F15 | Release-only mode ([16.4](#164-release-only-mode)) | Must | – |
| F7 | `serve` / `shell` / one-shot CLI | Must | B13, B14, S3 |
| F3 | Rollback | Must | – |
| F9 | Self-update | Must | B11 |
| F16 | Reconcile loop | Should | B12 |
| F4 | Notifications | Should | – |
| F5 | Efficient change detection, token-expiry warning | Should | – |
| F8 | Structured logs and deploy history | Should | S7 |
| F11 | Health and version | Should | S5 |
| F12 | Base image refresh | Could | – |
| F13 | Unmanaged services | Could | – |

**Progress after the foundation:** F14 is done (Go and image updates, tests, CI). F10 is done: Lighthouse waits for Cove and for the database, checks its token, migrates, checks the schema, and explains every failure in one line. F7's modes (`serve`, `shell`, one-shot) and the current command set are done; `history`, `approve`, `retry`, `rollback`, `check` and `set` arrive with the features they control. F11 is partly done: the published port is gone, and `version` and `status` report `LIGHTHOUSE_VERSION`.

#### F1. Postgres and goose migrations instead of `repos.json`
**Done in step 2.** Required by the standards ([§14.1](#141-databases-and-migrations)). What exists: [§6](#6-data-the-database); what later steps add: [16.7](#167-database-design). `lighthouse import` moves a pre-1.0 `repos.json` over.

#### F2. The deploy pipeline
See [16.3](#163-the-deploy-pipeline). One worker processes a queue; the poller, the reconcile loop and the CLI all enqueue; a per-project lock stops two deploys of the same project.

#### F3. Rollback
Every successful build is tagged `lighthouse/<project>:<version>` (the tag in release-only mode, otherwise the short SHA). The last 3 are kept. `rollback <project> [version]` by hand; automatic when the post-swap check fails.

#### F4. Notifications
A Discord webhook (`LIGHTHOUSE_DISCORD_WEBHOOK_URL` in Cove) on deploy succeeded, failed, broken, rolled back, waiting for approval; on Cove or GitHub unreachable or a token rejected; on a GitHub token expiring within 14 days; and on a container crash-looping (F16). Optionally a daily summary.

#### F5. Efficient, reliable change detection
- Branch mode: `GET /repos/{o}/{r}/commits/{branch}` with `Accept: application/vnd.github.sha` (40 bytes, instead of about 30 full commits).
- Release mode: the tags list ([16.4](#164-release-only-mode)).
- `If-None-Match` with the last ETag: a `304` doesn't count against the rate limit (today each repo costs 360 of the 5,000 requests per hour).
- `X-RateLimit-Remaining` respected; interval configurable (default 30–60 s).
- **Token expiry:** GitHub's `github-authentication-token-expiration` response header gives the fine-grained token's expiry. Lighthouse shows it in `status` and notifies 14 days ahead.
- Later: GitHub webhooks through the Cloudflare tunnel (HMAC-verified, `LIGHTHOUSE_GITHUB_WEBHOOK_SECRET`), polling kept as a fallback.

#### F7. `serve`, `shell` and one-shot commands
Modelled on Cove:

| Run as | What it is |
|---|---|
| `lighthouse serve` (the container's command) | The daemon: poller, worker, reconcile loop. Logs go to stdout (`docker logs lighthouse`) and nothing else does |
| `docker exec -it lighthouse /lighthouse shell` | The prompt, with history and `help` |
| `docker exec lighthouse /lighthouse <command>` | One command, exit status 0 or 1 |
| `lighthouse migrate [status\|up]`, `import <file>`, `version` | Admin modes |

Read commands (`list`, `status`, `history`) read the database directly. Action commands (`deploy`, `retry`, `approve`, `rollback`, `pause`, `stop`) go to the daemon through a Unix socket inside the container, so the one worker stays the only thing that touches projects.

Commands work on **project names only** (B13): no raw container names, nothing outside the managed projects. New commands:
- `add <name> <url> [--release-only] [--prereleases] [--branch <b>] [--infra]`
- `deploy <name> [version]` and `approve <name>`
- `retry <name>` and `rollback <name> [version]`
- `history <name>`
- `check <name>` (steps 1–6 as a dry run, nothing touched)
- `set <name> <setting> <value>`

Output follows clig.dev, like Cove: data on stdout, messages on stderr.

#### F8. Structured logs and deploy history
`log/slog` with timestamps and levels; one line per pipeline step with project and version. The scrubbed tail of each step's output is stored with the deployment (S7); `history <name>` shows it.

#### F9. Self-update
Lighthouse watches its own repo in release-only mode:
1. It builds and tags its new image like any project (steps 1–7 of the pipeline).
2. It finishes the deploy in progress and stops taking new work.
3. It starts a helper container from the **new** image, with the Docker socket mounted, running `lighthouse self-update --from <old tag> --to <new tag>`.
4. The helper runs `docker compose -p lighthouse up -d --no-build`, waits for the new Lighthouse to report healthy, and restarts the old image if it doesn't. Then it exits.

State is in Postgres, so nothing is lost across the swap. The very first deploy of v1.0.0 is by hand.

#### F10. Startup that waits instead of panicking
1. `WaitForReady` on Cove (retrying, with a clear log line).
2. Token loaded, then `Auth()`; on 401 the log says how to re-bootstrap.
3. Database URLs fetched from Cove.
4. Migrations, then the schema version check.
5. The worker and the reconcile loop start.

Errors are one line, `lighthouse: <message>`, with exit status 1, like Cove. No `.env` required.

#### F11. Health and version
`lighthouse version`, `LIGHTHOUSE_VERSION` in the compose file, a Docker healthcheck (`lighthouse status --quiet`), and the published port 2000 removed (S5).

#### F12. Base image refresh
A weekly rebuild with `--pull` (through the normal pipeline, so it's checked, scanned and can roll back), so projects get base-image security fixes without a code change.

#### F13. Unmanaged services
From `todo.txt`: register infrastructure Lighthouse doesn't build (pihole, cloudflared if not from a repo) to show its status and start or stop it. Lighthouse never deploys it.

#### F14. Project engineering
- Unit tests for: the variable resolver, URL and semver parsing, the policy check, failure classification, and the pipeline with a fake compose runner.
- Integration tests against a disposable Postgres, like Cove.
- CI on push and PR (gofmt, vet, `test -race`, govulncheck, build). This is Lighthouse's own repo, where GitHub Actions is fine; the "no Actions" decision is about *deployed projects*. Say if you'd rather not have it here either.
- `CHANGELOG.md`, `.dockerignore`, examples renamed (`exmaple` typo), a LICENSE if the repo is public.

#### F15. Release-only mode
See [16.4](#164-release-only-mode).

#### F16. Reconcile loop
Lighthouse subscribes to Docker's event stream (and checks every few minutes as a backstop). It compares each project's **desired state** (running a given version, or stopped on purpose) with the actual state, read through the `com.docker.compose.project` label: containers, health, image tag.

| Drift | Action |
|---|---|
| Not running, should be | Start it. After 3 restarts in 10 minutes: mark **degraded**, notify, stop trying |
| Unhealthy | Notify; restart if the project opts in |
| Running a different image than recorded | Notify (someone deployed by hand); `adopt` or `deploy` to resolve |
| Stopped by `stop <name>` | Leave it: desired state is "stopped" |

Kept deliberately small: one server, Compose projects, a dependency order. Not a scheduler or a cluster manager.

#### F17. Checks and tests on Lighthouse
Steps 3–6 of the pipeline. Because every project follows the same contract (Compose, Cove, Docker images), most checks are generic. A project adds its own tests with a Dockerfile `test` stage.

### 16.3 The deploy pipeline

```
 PREPARE — the running version keeps serving; any failure here leaves it untouched
  1. Fetch     archive of the exact SHA/tag via the API (token, timeout, status, size cap)
  2. Unpack    <work>/<project>/<version>/, same path on host and in container (B7), modes kept (B9)
  3. Contract  compose config --format json: name/container_name match, on spark, no relative
               bind mounts; policy (S2): no privileged, host namespaces, cap_add, devices,
               docker.sock, mounts outside /srv/server/storage/<project>/ — unless excepted
  4. Secrets?  compose config --variables (B6): needed keys follow the naming standard, and
               exist in Cove (GET /v0/secrets: names only, no reads counted)
               + gitleaks over the source (no committed secrets)
  5. Test      if the Dockerfile has a `test` stage: docker build --target test --network none
               (no secrets, no network, timeout). Go: vet/test/govulncheck; Node: lint/test; …
  6. Build     docker compose -p <project> build --pull, image tagged <project>:<version>
               then Trivy on the image: warn or block per project (default: block on CRITICAL)
  7. Fetch     one GetSecrets(keys…) batch; values held in memory only
  8. Backup    infrastructure projects only (16.5)
  9. Approve   infrastructure projects only: wait for `approve <name>` (notified)

 SWAP — the only downtime
 10. Up        docker compose -p <project> up -d --no-build --remove-orphans
               (Compose stops the old container and starts the new one in one step)

 VERIFY
 11. Check     healthy within N s: the compose healthcheck if defined, else an HTTP check
               over spark (http://plop:3110/<path>) if configured, else "running for 30 s"
               fail → up again with <project>:<previous>, deployment = rolled_back
 12. Record    deployment row (scrubbed logs), desired state = this version, notify
 13. Clean     remove the work folder; keep the last 3 images, prune older ones and old build cache
```

**Failure handling:**

| Kind | Examples | What happens |
|---|---|---|
| Transient | GitHub 5xx or rate limit, Cove or sparkdb unreachable, network timeout, Docker Hub pull failure | Retry with exponential backoff (1 min → 30 min); doesn't count toward broken |
| Permanent | Contract or policy violation, missing secret, test stage failed, build failed, blocked by scan | Counts. A policy violation or missing secret marks the version broken at once (retrying can't help) |
| Failed after swap | Health check failed, crash on start | Rolled back automatically, counts |

After 3 counted failures for the same version, the project is **broken**: no more attempts until a new version appears or you run `retry <name>`, and you're notified. Every step has its own timeout (fetch 2 min, test 15 min, build 20 min, health 2 min; configurable per project).

**Additions to the project contract** ([§7](#7-connecting-a-project-the-contract)) for v1.0.0:
- An optional `test` stage in the Dockerfile.
- An optional health check: a compose `healthcheck:`, or a health URL recorded in Lighthouse.
- Persistent data under `/srv/server/storage/<project>/`.
- Every key is `<PROJECT>_*` or `SHARED_*`, unless an exception is recorded for the project.

### 16.4 Release-only mode

`add plop https://github.com/LSariol/plop --release-only`

- **What counts as a release:** tags that are plain semantic versions, `v1.2.3` or `1.2.3`, compared as versions (so `v1.10.0` > `v1.9.0`). Pre-releases (`v1.1.0-rc.1`) are ignored unless the project has `--prereleases`. Anything else (`latest`, `deploy-test`) is ignored.
- **On adding:** the newest release is deployed (or `--from v1.2.0` to pick one).
- **Afterwards:** only a release **greater** than the deployed one triggers a deploy. Nothing ever downgrades automatically. `deploy <name> v1.1.0` deploys any version by hand, which is also how you roll back past the 3 kept images.
- **A tag that moves** (deleted and re-pushed on another commit) for a version already deployed is ignored, with a warning. A version is never silently redeployed.
- **Detection:** `GET /repos/{o}/{r}/tags` (paginated, with ETag), then the tag resolved to its commit SHA. Annotated tags are peeled to the commit.
- **Images** are tagged with the version (`lighthouse/plop:v1.2.0`), so `history`, `status` and `rollback` show versions instead of SHAs.
- **Your release flow** pushes the tag, then `main`. For a release-only project the tag deploys and `main` does nothing. That's the natural mode for **Lighthouse itself and the infrastructure tier**. Branch mode stays the default for everyday projects.

### 16.5 Infrastructure projects

sparkdb, Cove and cloudflared are added with `--infra`. What happened before: deploying sparkdb stopped it first, and then Cove couldn't read the secrets from its database to finish the deploy (B1). The new pipeline fixes that order. On top of it:

- **Everything is fetched before the swap** (step 7), and from step 10 until sparkdb is healthy, Lighthouse needs nothing that depends on it: no Cove calls, no database writes. The deploy result is kept in memory and written once the database is back.
- **Approval:** the new version is prepared and checked, then waits for `approve <name>`. You get a notification.
- **Backup first** (step 8): for sparkdb, `pg_dumpall` to `/srv/backups/` with mode 0600. The deploy stops if the backup fails.
- **Deployed alone:** nothing else deploys while an infrastructure project is deploying, and the queue waits until it's healthy again.
- **Health:** sparkdb is healthy when `pg_isready` passes; Cove when `/v0/ready` answers.
- **Rollback limits:** going back to the previous image can't undo a data-format upgrade. Pin sparkdb's image to a major version (`postgres:17`), and treat a major upgrade as a manual, planned change.
- **Dependency order:** sparkdb → Cove → everything else, after a reboot and for the queue.

### 16.6 Layout

The layout from the foundation ([§3](#3-architecture)) is the v1.0.0 layout; the steps add to it rather than move things:

| Step | Adds or changes |
|---|---|
| 2. Database — **done** | `projects/` (types, the Store interface, an in-memory Store and its shared tests), `database/` (pgx pool, goose, schema check, the Postgres Store), `reposjson/` (for `import`); `watchlist/` removed; `cove/` reads the database URLs |
| 3. Pipeline — **done** | `deploy/` rewritten (steps, rollback, verify, cleanup, scrubbing); `compose/` (config, variables, build, up, down, with deadlines and a clean environment); `docker/` finds containers by label and manages tags; `github/` downloads commits |
| 4. Checks | `checks/`: contract, policy, naming, gitleaks, test stage, Trivy |
| 5. Orchestrator | `orchestrator/` gains the queue and worker, backoff, release-only mode (`semver/`), the infrastructure tier and the reconcile loop; `docker/` gains the event stream |
| 7. Notifications, self-update | `notify/` (Discord); `lighthouse self-update` mode |

### 16.7 Database design

**Built in step 2:** roles and the database by hand (`scripts/db/setup.sql`, [§10.1](#101-database-setup-once-by-hand)); `projects`, `deployments` and the grants by migrations `00001` and `00002` ([§6](#6-data-the-database)). Connection strings live only in Cove: `LIGHTHOUSE_DATABASE_URL`, `LIGHTHOUSE_MIGRATOR_DATABASE_URL`, `LIGHTHOUSE_READER_DATABASE_URL`.

**Added by later steps**, each as new migrations when its feature lands (never by editing `00001`):

| Step | Adds |
|---|---|
| 3. Pipeline — **done** (`00003`) | `projects`: `compose_project` (empty until a deploy reads it), `failure_count`, `failing_sha`, `broken`; `deployments`: `rolled_back`, `failure_kind`, `failed_step`; `deployment_steps`. (Rollback images are found by their `lh-<commit>` tags, so no image columns are needed.) |
| 4. Checks | `projects`: `secret_prefix`, `scan_policy`, `policy_exceptions text[]`, `secret_exceptions text[]` (keys outside the prefix and `SHARED_*`), `health_url`, `step_timeouts` |
| 5. Orchestrator | `projects`: `mode` (`branch`/`release`), `branch`, `include_prereleases`, `tier` (`normal`/`infra`), `requires_approval`, `desired_state`, `paused`, `deployed_version`, `last_seen_sha`/`_version`, `etag`, `health`, `failure_count`, `failing_version`, `broken`, `next_attempt_at`; `deployments`: `version`, `failure_kind`, more triggers (`retry`, `rollback`, `reconcile`, `self_update`) and statuses (`awaiting_approval`, …) |

### 16.8 Order of work

Each step is a short-lived branch merged into `release/1.0.0`, and prod changes are logged in the rollout plan as they come up.

1. **Foundation — done** (2026-10-05, `aff3d9e`..`4fc4f79`):
   - docs; cleanup (dead code, old files, gofmt)
   - dependencies: Go 1.27.1, `moby/moby/client`, `alpine:3.24`, `.dockerignore`; govulncheck clean
   - config package, slog, `serve` / `shell` / one-shot CLI over a control socket, following the server's CLI conventions
   - tests for every package but the builder, CI
   - fixed along the way: B2, B3, B4, B5, B12, B17, B20, S5, S6, S8; partly B13, B14, B16, B18, S3
2. **Database — done** (2026-10-06):
   - Admin SQL as `scripts/db/setup.sql` (by hand, §10.1), URL-only keys in Cove.
   - Goose migrations `00001`–`00002`, the Postgres store, deploy history (`history`), `lighthouse migrate`, `lighthouse import`.
   - Integration tests against real Postgres, locally (`scripts/test-db.sh`) and in CI; in-memory and Postgres stores pass the same tests.
   - Fixed along the way: B15, B16.
3. **Pipeline — done** (2026-10-06):
   - Build first, swap last, verify, roll back ([§4.3](#43-the-deploy-pipeline-deploydeployerdeploy)); per-step output with secrets hidden; cleanup of folders, tags, images and cache.
   - Failure classification and the broken state (3 permanent failures of one commit); `retry`, `report`.
   - Compose projects and multiple services ([16.9](#169-compose-projects-and-services-decided-2026-10-06)); `name:service`; `remove` takes the containers down (`--keep` leaves them).
   - Migration `00003`; the `compose` package; tests against real Compose and Docker, including a real rollback.
   - Fixed along the way: B1, B6–B10, B13, B18, B19, S7; partly B11.
   - Moved to later steps: the health URL setting and the secret prefix (step 4), the test stage and image scan (step 4), approval and backups for infrastructure projects (step 5).
4. **Checks:**
   - Contract and policy, naming and existence, gitleaks.
   - Test stage, Trivy.
5. **Orchestrator:**
   - Queue and worker, polling with ETags.
   - Release-only mode, the infrastructure tier, the reconcile loop.
6. **CLI:** the full command set (F7).
7. **Notifications and self-update:** F4, F9.
8. **Release:**
   - CHANGELOG, README, this document.
   - Rollout-plan entries: Admin SQL, Cove keys and tokens, compose changes on the server, `import`, the fixed IP on `spark`.
   - **Revisit S1:** decide whether Cove v1.1.0 (delegated reads, `--from` tokens) is worth doing before or after tagging.
   - Merge, tag `v1.0.0`.

### 16.9 Compose projects and services (decided 2026-10-06)

**Built in step 3**, except the secret prefix (step 4).

A project's nickname, its repository, its compose project and its containers are four different things, and only the first two are chosen in Lighthouse.

| Name | Comes from | Example (`add personalWebsite github.com/lsariol/landing`) |
|---|---|---|
| Nickname | `add <name>`; `rename` changes only this | `personalWebsite` |
| Repository | `add … <url>`, `set-url` | `lsariol/landing` |
| Compose project | **The compose file's `name:`**, read at every deploy and stored; the lowercased repo name if there is none. Lighthouse always runs `docker compose -p <it>` | `website` |
| Services | Found through Compose's labels (`com.docker.compose.project` / `.service`), never guessed from names | `web`, `www`, `bot` |

**Commands:** `start`, `stop`, `restart`, `logs` and `status` take `<project>` (every service) or `<project>:<service>` (one), e.g. `logs personalWebsite:bot`; Tab completes the services. `status` shows one line per service. `remove` stops and removes the project's containers too (`--keep` leaves them running).

**Rules that come with it:**
- **A compose project belongs to one Lighthouse project.** A deploy that would take over another project's compose project is refused, naming the other project.
- **Healthy** means every service with a `restart:` policy is running; a service without one (a one-off job such as migrations) may have exited cleanly.
- **The secret prefix** (for the step-4 rule that a project only gets its own keys) is stored per project, defaulting from the nickname (`PERSONALWEBSITE_`) and changeable, since existing Cove keys may use another name.
- **Unique names on `spark`** ([B21](#b21-service-names-can-collide-on-the-spark-network)).
- History stays per project (a deploy covers every service); rollback image tags are per service (`website-bot:<commit>`).

**Migration:** a new migration adds `compose_project` and `secret_prefix` to `projects`; existing rows get the lowercased repo name (today's rule), so nothing changes until a project's next deploy reads its real `name:`.

---

## 17. Housekeeping

Remove or fix before v1.0.0. Done in the foundation unless marked *open*:

| Item | Action |
|---|---|
| `internal/orchestrator/` | Fully commented out. Deleted (a real `orchestrator` package comes in step 5) |
| `Builder.InitilizeContainers` | Unused, misspelled, and returns after the first running container. Delete |
| `InitilizeOriginalPath`, `Builder.LoadPaths`, `BASE_PATH`, `Builder.BasePath`, `Watcher.HomePath` | Unused. Delete |
| `builder.ErrorHandler()` | Empty function called on build failure. Delete |
| `Watcher.ChangeRepoURL`, `checkURLConflicts` | Unused duplicates of `UpdateRepo`. Delete |
| `GetAllContainers`, `GetRunningContainers` | Unused |
| `cleanUp`'s `STAGING_PATH/Working` | Unused folder |
| `APP_ENV_PATH` in compose | Never read |
| `COVE_ADDRESS` | Rename to `COVE_URL` (the server standard) |
| Port `2000:2000`, `RUN mkdir -p /app/lighthouse` | Unused |
| `.env.exmaple`, `config/repos.json.exmaple` | Typo: rename to `.example`; `.env.example` should list only what's needed for local dev |
| `notes.md`, `todo.txt`, `lighthouse.example.yaml` | Fold anything still wanted into this document, then delete |
| `RawGitResponse.json`, `cove-token`, `.env`, `Server/` in the working copy | Done 2026-10-06: `.env` updated, the dev token moved to `.dev/cove-token`. *Open, on your PC only:* delete `RawGitResponse.json` and the old `Server/` folder |
| `.vscode/launch.json` `ENVIRONMENT=dev` | Not read by anything |
| `go.mod` `go 1.25.1` / Dockerfile `golang:1.25.1-alpine` | Bump both to 1.27.1 together |
| Old README claims | `LIGHTHOUSE_GITHUB_PAT`, `COVE_CLIENT_SECRET`, `PROJECT_CONTEXT.md` and the `LuSracol/Cove` link were all out of date. Fixed in the new README |
