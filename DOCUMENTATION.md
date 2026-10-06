# Lighthouse Documentation

The complete reference for **Lighthouse**, the self-hosted deployer for the `spark` server's projects. It covers how Lighthouse works today, the rules it follows, every known bug and security issue, and the plan for **v1.0.0**.

- [README](README.md): a short, friendly overview.
- [Cove](https://github.com/LSariol/Cove): the secret vault Lighthouse reads from. Its `DOCUMENTATION.md` §9 (connecting a project), §10 (bootstrap) and §14 (operations) are the other half of this document.
- [CoveClient](https://github.com/LSariol/CoveClient): the Go library Lighthouse uses to talk to Cove.

> **Status.** This document describes `release/1.0.0` after steps 1 and 2 of [§16.8](#168-order-of-work): the foundation (config, CLI, control socket, tests, CI) and the database (projects and deploy history in Postgres). The deploy pipeline itself is still the pre-1.0 one. Sections 1–13 describe what the code **does today**, rough edges included. Section 14 lists the standards every change must follow. Sections 15–17 list what is still wrong and what v1.0.0 will look like.

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

Lighthouse keeps the server's projects running the latest code on their `main` branch. It is one Go program running in a Docker container next to the projects it manages.

Every 10 seconds (configurable) it asks GitHub for the newest commit of each watched repository. When the commit changes, it:

1. downloads the repository as a ZIP,
2. stops the running container,
3. finds every `${KEY}` placeholder in the project's `docker-compose.yml`,
4. fetches those secrets from Cove in one request,
5. runs `docker compose up -d --build` with the secrets in the command's environment.

The project reads plain environment variables: it needs no Cove code, address or token.

**Where it fits:**

```
          GitHub  ◄── checks every 10 s (REST API, token from Cove)
             │
             │ main.zip
             ▼
   ┌──────────────────┐   GetSecrets(${KEY}...)   ┌─────────┐
   │    lighthouse    │ ────────────────────────► │  cove   │ ──► sparkdb (cove_db)
   │  serve (daemon)  │ ◄──────────────────────── │  :2100  │
   └───┬──────────┬───┘        values             └─────────┘
       │          ▲ control socket (CLI: docker exec -it lighthouse /lighthouse shell)
       │ /var/run/docker.sock
       ▼
   Host Docker daemon ──► botsuite, marquee, plop, letterboxd, cove, ...  (sibling containers)
                          all on the external `spark` network; public traffic via cloudflared
```

| Property | How |
|---|---|
| Change detection | Polling the GitHub REST API per repo (default every 10 s); no webhooks |
| Source | Always the `main` branch, as a ZIP archive |
| Build and run | `docker compose up -d --build --remove-orphans` in the unpacked folder, using the host's Docker daemon through the mounted socket; one deploy at a time |
| Secrets | `${KEY}` placeholders resolved from Cove with one `GetSecrets` batch, passed only in the `docker compose` process environment (never written to disk) |
| State | Postgres (`lighthouse_db` on sparkdb): the projects, and a history of every deploy. Schema by goose migrations built into the binary |
| Control | `lighthouse serve` is the daemon; the CLI (`shell` or one-shot commands) is a separate process that talks to it through a Unix socket |

---

## 2. Quick reference

On the server. `lh` below stands for `docker exec -it lighthouse /lighthouse`; an alias saves typing: `alias lh='docker exec -it lighthouse /lighthouse'`.

| Task | How |
|---|---|
| Open the prompt | `lh shell` (`exit` or Ctrl-D leaves; Lighthouse keeps running) |
| Is everything healthy? | `lh status` (exits non-zero if something needs attention) |
| Watch a new repo | `lh add <name> https://github.com/<owner>/<repo>` |
| Stop watching | `lh remove <name>` (the container keeps running) |
| See what's watched | `lh list` |
| Deploy now | `lh deploy <name>` (or `deploy all`) |
| How did its deploys go? | `lh history <name>` |
| Check for new commits now | `lh scan` |
| Freeze automatic deploys | `lh pause` / `lh resume` |
| A project's output | `lh logs <name> [lines]` |
| Lighthouse's own log | `docker logs -f lighthouse` |
| Help | `lh help`, `lh help <command>`, `lh help setup`, `lh help failed` |
| Add a secret a project needs | In Cove: `create <PROJECT>_<PLATFORM>_<TYPE> <value>`, then reference it as `${...}` in the project's compose file |
| Give Lighthouse a new Cove token | Delete `/srv/server/storage/lighthouse/cove/token`, `bootstrap open lighthouse` in Cove, `docker restart lighthouse` |
| Change the GitHub token | In Cove: `update LIGHTHOUSE_GITHUB_TOKEN <token>`, then `docker restart lighthouse` (it's read once at startup) |
| Database migrations | `lh migrate status`; they're applied automatically at startup |

---

## 3. Architecture

```
cmd/lighthouse/main.go     Modes (serve, shell, one command, migrate, import, version), the
                           startup order, and the wiring of the packages below
internal/
  config/                  Every setting, read from the environment once and validated
  cli/                     The command table, help and guides, the shell (line editing,
                           history, Tab completion), one-shot commands, output helpers
  control/                 The CLI ↔ daemon protocol: Service (what the CLI can ask), Serve
                           (HTTP over a Unix socket) and Client (the CLI's side)
  daemon/                  Implements control.Service on the parts below; turns their errors
                           into messages that say how to fix them; the startup phase
  orchestrator/            When projects deploy: the check loop, Scan, Deploy; one scan at a
                           time; records every check and deploy in the store
  deploy/                  How a project deploys (one at a time): download and unpack
                           (source.go), placeholders, secrets and compose up (compose.go),
                           the scratch folders (workspace.go)
  projects/                Project, Deployment, the Store interface, name rules
    projectstest/          An in-memory Store, and the tests every Store must pass
  database/                Postgres: the pool, goose migrations (migrations/, built in), the
                           schema check, and the Store
  reposjson/               Reads a pre-1.0 repos.json, for `lighthouse import`
  github/                  Repository URLs, latest commit
  docker/                  Container start, stop, restart, state, logs (Docker SDK)
  cove/                    Connecting to Cove at startup; Lighthouse's own secrets
scripts/
  db/                      Admin SQL for sparkdb, run by hand (§10.1); CI uses it too
  test-db.sh               A throwaway Postgres for the integration tests (§13.1)
Dockerfile                 golang:1.27.1-alpine → alpine:3.24 + docker-cli + docker-cli-compose
docker-compose.yml         Lighthouse's own service: server paths, docker.sock, spark network
.github/workflows/ci.yml   gofmt, vet, tests with -race (with Postgres), govulncheck, builds
```

**Dependencies point one way:**

```
main ─► cli ─► control
main ─► daemon ─► orchestrator ─► projects (Store) ◄── database
                         │
                         └─(Deployer)─► deploy ─► docker, CoveClient, projects
        daemon ─► docker, github, projects
main ─► cove ─► CoveClient          main ─► reposjson ─► projects, github
```

`cli` only knows `control.Service`; `orchestrator` and `daemon` only know `projects.Store`, and the `Deployer`, `Commits`, `Containers` and `Health` interfaces. Each is tested with a fake in place of the real thing. The in-memory Store passes the same tests as the Postgres one (`projectstest.RunStoreTests`), so the fake can't drift from the real thing. Only `config` reads the environment (the `docker compose` subprocess inherits it).

**Where things go:**
- A new CLI command → a `cli/cmd_*.go` function plus one entry in `commandTable()`; help and completion pick it up. If it asks the daemon for something new: a `control.Service` method, its route in `control/server.go`, a `Client` method, and the `daemon` implementation.
- What happens when → `orchestrator`. How a deploy is done → `deploy`.
- Something stored → a `projects.Store` method, implemented in `database/store.go` and `projectstest/store.go`, with a case in `projectstest/contract.go`. A schema change → a new migration ([§14.1](#141-databases-and-migrations)).
- A setting → a field in `config.Config`, read in `Load`, checked in `ValidateServe`. A secret → Cove, read in `cove.ReadSecrets`.

**State and concurrency:** all state is in Postgres; nothing is cached in memory, so the CLI, the check loop and `lighthouse import` always see the same thing. A scan runs at most once at a time (a second is refused); deploys run one at a time (a second waits), whether they come from the check loop or the CLI.

---

## 4. Flows

### 4.1 Startup (`lighthouse serve`)

1. **Config.** `config.Load` reads the environment, plus a `.env` file if one exists (local development only: `APP_ENV_PATH`, else `./.env`). `ValidateServe` stops startup when a required setting is missing, `COVE_URL` has no scheme, the poll interval is under 5 s, or the staging or download folder is `/`, `.` or a system folder. Every startup error is one line, `lighthouse: <message>`, with exit status 1.
2. **Logging.** `log/slog` text lines with timestamps on stderr (`docker logs lighthouse`).
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
9. **Stopping** (`docker stop`, SIGTERM). Lighthouse stops answering the CLI, waits up to 8 s for a deploy in progress, closes the database and removes the socket. A deploy still running then is cut off ([B10](#b10-no-timeouts-and-no-graceful-shutdown)); its history row is still written.

A fatal error after the socket is open (for example a revoked Cove token) exits with status 1. Docker's `restart: unless-stopped` starts Lighthouse again, so the same one-line message repeats in `docker logs` until it's fixed.

### 4.2 The check loop

Unless paused, every `LIGHTHOUSE_POLL_INTERVAL`, `Scan` checks each project in turn:

```
GET https://api.github.com/repos/<owner>/<repo>/commits?per_page=1   (Authorization: Bearer <token>)
  ├─ error / non-200 / no commits → record the error on that project, go on to the next one
  └─ sha = the newest commit on the repository's default branch

if sha differs from the project's deployed commit (or none is recorded):
    Deployer.Deploy(project)
    record a deployment row (trigger "check", succeeded/failed, times, error)
      ├─ fails → it becomes the last error; the deployed commit is unchanged, so the next check deploys again
      └─ works → it becomes the deployed commit, and the last error is cleared
record the check (time, count, and the error or none)
```

A scan that had failures logs one line naming the projects; `list` and `status` show each project's last error, and `history <name>` every deploy attempt. A successful check clears the last error. A deploy from the CLI (`deploy <name>`) is recorded the same way, with trigger `manual`.

### 4.3 The deploy pipeline (`deploy.Deployer.Deploy`)

Deploys run one at a time; a second waits for the first.

| Step | Code | What happens | On failure |
|---|---|---|---|
| 1. Clean | `cleanUp()` | Empties `STAGING_PATH` and `DOWNLOAD_PATH` (creating them if needed) | Deploy fails |
| 2. Download | `downloadNewCommit` | `http.Get(<repo>/archive/refs/heads/main.zip)` → `DOWNLOAD_PATH/<repo>.zip`. Default HTTP client: no timeout, no auth, status code not checked | Deploy fails |
| 3. **Stop** | `StopContainer` | Stops the container named like the repo, lowercased (10 s grace). "No such container" is ignored | Deploy fails |
| 4. Unpack | `unpackNewProject` | Unzips into `STAGING_PATH`, with a zip-slip check. File modes are **not** kept | Deploy fails, **project stays stopped** |
| 5. Placeholders | `findComposeVars` | `docker compose config --no-interpolate` in `STAGING_PATH/<lowercase repo>-main`, then the regex `\$\{([^}:]+)(?::[^}]*)?\}` over the output | Deploy fails, **project stays stopped** |
| 6. Secrets | `GetSecrets(keys...)` | One CoveClient batch (all or nothing). Zero keys means no request | Deploy fails, **project stays stopped** |
| 7. Up | `docker compose up -d --build --remove-orphans` | Environment = Lighthouse's own environment + `KEY=value` for each secret; output goes to Lighthouse's log | Deploy fails, **project stays stopped** |
| 8. Clean | `cleanUp()` | Empties staging and download again | Reported as failed although the project is running |

Steps 3–7 are why a failed deploy causes an outage ([B1](#b1-a-failed-deploy-takes-the-project-down-and-retries-forever)), which the v1.0.0 pipeline fixes ([16.3](#163-the-deploy-pipeline)).

### 4.4 Secret injection, exactly

- **What counts as a secret:** every `${...}` in the project's resolved compose configuration. `${KEY:-default}` and `${KEY:?msg}` count too: the part before `:` is fetched ([B6](#b6-the-placeholder-parser-is-wrong-for-common-compose-syntax)).
- **Lookup:** the key is used exactly as written. Cove keys are case-sensitive.
- **All or nothing:** if any key is missing, `GetSecrets` fails with a `missing: KEY1, KEY2` error and the deploy stops. If Lighthouse's token can't read a key, Cove answers `403 forbidden_key` without naming it (`docker logs cove` names it).
- **Delivery:** values are appended to the `docker compose` process environment as `KEY=value`. Compose substitutes them into the file in memory. They never touch disk on Lighthouse's side. They do end up in the running container's environment, where `docker inspect` can see them, as any environment variable does.
- **Rule from Cove's standard:** only real secrets go in `${...}`. A plain setting is written as a value (`COVE_URL=http://cove:2100`), because Lighthouse treats every placeholder as a key to fetch.

### 4.5 A CLI command

```
lighthouse <command>  or  the shell
  → cli: command table → argument checks, confirmation (y/N) for destructive commands
  → control.Client: HTTP over the Unix socket (LIGHTHOUSE_CONTROL_SOCKET)
  → daemon: checks the project is watched, calls the store, orchestrator or Docker
  → JSON answer: data, or an error with a kind and a message that says how to fix it
  → cli: data on stdout, messages on stderr (✓ ! ✗ ?), exit status 0 or 1
```

`deploy` and `scan` wait until they're done (a deploy can take minutes; progress is in `docker logs -f lighthouse`). While Lighthouse is still starting, they're refused with the reason.

---

## 5. Configuration

All settings are environment variables; Lighthouse's secrets (the GitHub token and both database URLs) come from Cove instead ([§8](#8-cove-integration)). In Docker they're written out in `docker-compose.yml`'s `environment:` (none of them is a secret). For local development they can come from a `.env` file ([§13.1](#131-running-locally)).

| Variable | Required for `serve` | Description |
|---|---|---|
| `COVE_URL` | Yes | Cove's base URL, `http://cove:2100` in Docker |
| `COVE_TOKEN_PATH` | Yes | Where Lighthouse's Cove token is kept. Docker: `/app/vault/cove/token` |
| `STAGING_PATH` | Yes | Where archives are unpacked. **Emptied on every deploy**, so it can't be `/`, `.` or a system folder. Docker: `/app/server/staging/` |
| `DOWNLOAD_PATH` | Yes | Where archives are downloaded. **Emptied on every deploy**, same rules. Docker: `/app/server/download/` |
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
| `/srv/server/staging/` | `/app/server/staging/` | Scratch space |
| `/srv/server/download/` | `/app/server/download/` | Scratch space |
| `/var/run/docker.sock` | `/var/run/docker.sock` | Full control of the host's Docker (effectively root) |

No port is published and no terminal is attached. Lighthouse joins the external `spark` network so it can reach `cove`.

---

## 6. Data: the database

Lighthouse's state is in `lighthouse_db` on sparkdb, schema `lighthouse`. Roles, the database and ownership are set up by hand once ([§10.1](#101-database-setup-once-by-hand)); everything inside the schema comes from the goose migrations in `internal/database/migrations/` ([§14.1](#141-databases-and-migrations)).

**`projects`**: one row per watched repository.

| Column | Notes |
|---|---|
| `id` | Identity; never shown |
| `name` | The name in the CLI: letters, digits, `-`, `_`, up to 64. Unique without regard to case |
| `repo_owner`, `repo_name` | `https://github.com/<owner>/<name>`; unique without regard to case. The container is `repo_name`, lowercased |
| `created_at`, `updated_at` | When Lighthouse started watching it; when it was last renamed or pointed elsewhere |
| `deployed_sha`, `deployed_at` | The last **successful** deploy. A project is deployed whenever GitHub reports a different commit |
| `last_checked_at`, `check_count` | The last check of GitHub |
| `last_error`, `last_error_at` | The last problem (a check or a deploy), cleared by the next success. Messages are cut at 2,000 characters |

**`deployments`**: one row per deploy attempt, written when it ends.

| Column | Notes |
|---|---|
| `project_id` | Its project; the rows go when the project is removed |
| `sha` | The commit deployed |
| `trigger` | `check` (a new commit found by a check or `scan`) or `manual` (`deploy <name>`) |
| `status` | `succeeded` or `failed` |
| `started_at`, `finished_at`, `error` | |

**`goose_db_version`**: which migrations have run.

**Who may do what** (from migration `00002`): `lighthouse_app` reads and changes `projects`, but can only **add** to `deployments`, never change or delete history, and only reads `goose_db_version`; it can't create anything. `lighthouse_reader` reads everything (pgAdmin). Everything is owned by `lighthouse_owner`.

**From the pre-1.0 Lighthouse:** `lighthouse import` reads its `repos.json` and adds each project with its state: when watching started, the deployed commit, checks, last error. It's safe to run twice, because existing projects are skipped. Steps in [§10](#10-deploying-lighthouse).

---

## 7. Connecting a project (the contract)

What a repository must look like for Lighthouse to deploy it. `lighthouse help setup` is the short version.

**Required**

1. **`docker-compose.yml` at the repo root** (or another name `docker compose` finds by default: `compose.yaml`, `compose.yml`, `docker-compose.yaml`).
2. **Deployable code on `main`.** The archive is always `refs/heads/main`, while the change check reads the repo's **default** branch. If those differ, Lighthouse watches one branch and deploys another.
3. **`name:` at the top of the compose file, equal to the lowercase repo name.** Without it, Compose names the project after the staging folder (`<repo>-main`). Compose then can't recognise its own containers on the next deploy, and a fixed `container_name` collides.
4. **One main container named exactly like the repo, lowercased** (`container_name: cove` for repo `Cove`). Lighthouse stops, starts, checks and reads the logs of the container with that name, and no other. `add` prints the name it expects.
5. **Join the external `spark` network** to reach Cove, sparkdb or each other (`sparkdb:5432`, `cove:2100`).
6. **Every `${KEY}` exists in Cove under exactly that name**, following the naming standard `PROJECT_PLATFORM[_ROLE]_TYPE` ([§14.2](#142-secrets-cove)). Plain settings are written as values, never `${...}`.
7. **Persistent data uses absolute host paths** (`/srv/server/storage/<project>/...:/app/data`) or named volumes. **Relative bind mounts (`./data:/data`) don't work:** Compose runs inside Lighthouse's container and resolves `./data` to `/app/server/staging/<repo>-main/data`, a path that doesn't exist on the host, so Docker creates an empty folder there ([B7](#b7-relative-bind-mounts-point-at-the-wrong-place)).
8. **Don't rely on executable bits** of files in the repo (`RUN ./build.sh`); unzip drops them. Use `RUN sh ./build.sh` or `chmod` in the Dockerfile ([B9](#b9-unzipping-drops-file-permissions)).
9. **Don't use `$${VAR}`** (Compose's escape for a literal `$`, common in healthchecks like `pg_isready -U $${POSTGRES_USER}`). Lighthouse misreads it as the secret `POSTGRES_USER` ([B6](#b6-the-placeholder-parser-is-wrong-for-common-compose-syntax)).
10. **Public repository**, or at least one whose `main.zip` downloads without auth. The download sends no token (private repos come with the v1.0.0 pipeline).

**Minimal example:**

```yaml
name: marquee

services:
  marquee:
    build: .
    container_name: marquee
    restart: unless-stopped
    environment:
      - DATABASE_URL=${MARQUEE_DATABASE_URL}     # secret: fetched from Cove
      - TMDB_API_KEY=${SHARED_TMDB_API_KEY}      # shared secret
      - LOG_LEVEL=info                           # plain setting: a value, not ${...}
    volumes:
      - /srv/server/storage/marquee/data:/app/data   # absolute host path
    networks:
      - spark

networks:
  spark:
    external: true
```

**A project that writes to Cove** (today only botsuite) also gets `COVE_URL=http://cove:2100` and `COVE_TOKEN=${<PROJECT>_COVE_TOKEN}` and uses CoveClient v1. See Cove's `DOCUMENTATION.md` §9.

**Cove itself** is deployed by Lighthouse. That works only because Cove's compose file has **no** `${...}` placeholders. With even one, Lighthouse would stop Cove and then ask the stopped Cove for the secret ([B1](#b1-a-failed-deploy-takes-the-project-down-and-retries-forever)). Keep it that way until B1 is fixed.

---

## 8. Cove integration

| Item | Value |
|---|---|
| Client | CoveClient `v1.0.0` (`github.com/lsariol/coveclient`) |
| Address | `COVE_URL=http://cove:2100` on `spark`; Cove has no published port |
| Token | Lighthouse's own project token, **read-only over everything** (`token create lighthouse --allow '*'`). Fetched once through the bootstrap endpoint and saved to `COVE_TOKEN_PATH`; checked with `Auth` on every start |
| Keys Lighthouse reads for itself | At startup, in one batch: `LIGHTHOUSE_GITHUB_TOKEN`, `LIGHTHOUSE_DATABASE_URL` (as `lighthouse_app`), `LIGHTHOUSE_MIGRATOR_DATABASE_URL` (as `lighthouse_migrator`). All three must exist |
| Keys it reads for projects | Every `${KEY}` in each project's compose file, as one `GetSecrets` batch per deploy |
| Audit | Every read shows as `lighthouse` in Cove's `history <KEY>` |
| Database keys | One connection string per role that logs in, nothing else: `LIGHTHOUSE_DATABASE_URL` (app), `LIGHTHOUSE_MIGRATOR_DATABASE_URL`, `LIGHTHOUSE_READER_DATABASE_URL` (for pgAdmin). The server's convention is `<PROJECT>_<ROLE>_DATABASE_URL`, the app's without a role. No separate password keys: a URL already holds the password, so a second copy could only drift. The old `LIGHTHOUSE_APP_PASSWORD` / `_OWNER_` / `_READER_` keys are retired in [§10.1](#101-database-setup-once-by-hand) |

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
| `docker exec -it lighthouse /lighthouse shell` | The prompt, `lighthouse (prod)>` (prod in red), with line editing, history and Tab completion of commands and project names. `exit`, Ctrl-D or Ctrl-C leave it; Lighthouse keeps running |
| `docker exec lighthouse /lighthouse <command>` | One command; exit status 0 or 1. Add `-it` when it may ask a question |
| `lighthouse help` | Help; works without a running daemon |
| `lighthouse` (no arguments) | The daemon **plus** its CLI on this terminal, for running locally (as `cove` does). The CLI talks to the daemon directly; `exit` or Ctrl+C stops Lighthouse. Plain line input, no Tab completion. If there's no terminal, the CLI steps aside and the daemon keeps running |
| `lighthouse serve` | The daemon only (the container's command) |
| `lighthouse version` | The version |
| `docker exec lighthouse /lighthouse migrate [status\|up]` | Show or apply database migrations, as the migrator (they're applied at startup anyway) |
| `docker exec -i lighthouse /lighthouse import -` | Move a pre-1.0 `repos.json` into the database (from standard input, or a file path); safe to run twice |

### Commands

`help` lists them by group; `help <command>` shows every form, flags and examples; `help setup` and `help failed` are guides. Project names aren't case-sensitive.

| Command | Usage | Notes |
|---|---|---|
| `list`, `ls`, `l` | `list` | Every project: repository, deployed commit, last deploy, last check, and its last error |
| `add` | `add <name> <url>` | Names: letters, digits, `-`, `_`, up to 64. The URL may have `www.`, a trailing `/` or `.git`. Deployed on the next check |
| `remove`, `rm` | `remove <name> [--yes]` | Asks first. The container keeps running |
| `rename` | `rename <name> <new-name>` | Lighthouse's name only; repository and container unchanged |
| `set-url` | `set-url <name> <url>` | Watch another repository under the same name; the container name follows the new repository |
| `deploy`, `rebuild` | `deploy <name\|all> [--yes]` | Deploy the latest commit now and wait. `all` asks first |
| `history` | `history <name> [count]` | The last deploys (10, up to 100), newest first: when, trigger (`check`/`manual`), result, commit, duration, the error's first line |
| `scan` | `scan` | Check every project now and deploy new commits; refused while another scan runs |
| `pause` / `resume` | | Stop / restart automatic checks. `deploy` and `scan` still work. A restart of Lighthouse resumes |
| `start` | `start <name\|all>` | Start a project's container |
| `stop` | `stop <name\|all> [--yes]` | Asks first |
| `restart` | `restart <name\|all> [--yes]` | `all` asks first |
| `logs` | `logs <name> [lines]` | The container's last lines (50, up to 10,000), on stdout |
| `status` | `status` | Version, environment, startup state, automatic deploys, Cove, GitHub token, database and schema, and every project's container state and last check. Exits 1 and lists what needs attention when something does |
| `help`, `h` | `help [command\|setup\|failed]` | |
| `exit`, `quit` | | Leave the shell |

### Conventions

- **Output:** data (tables, logs, help) on stdout; messages on stderr, each starting with a symbol: `✓` success, `!` warning or wrong usage, `✗` error, `?` question. Color only on a terminal and never with `NO_COLOR`. Wrong arguments show `Usage: <form>`. Errors say how to fix them.
- **Confirmation:** `remove`, `stop`, and the `all` forms of `deploy`, `stop` and `restart` ask `(y/N)`; Enter means no. `--yes` / `-y` skips the question. Without a terminal (a script, or `docker exec` without `-it`) they refuse and point at `--yes` instead of guessing.
- **Scope:** container commands only act on watched projects' containers, never on anything else on the host (`stop sparkdb` is "no project named sparkdb").
- **Adding a command:** see "Where things go" in [§3](#3-architecture).

---

## 10. Deploying Lighthouse

Lighthouse is deployed **by hand** on the server. It doesn't deploy itself yet: `Build` would stop the `lighthouse` container, killing the process doing the deploy ([F9](#f9-self-update)).

**First deploy** (on the Debian server, over SSH):

```bash
# The folders the bind mounts need.
sudo mkdir -p /srv/server/storage/lighthouse/cove /srv/server/staging /srv/server/download

docker network inspect spark >/dev/null 2>&1 || docker network create spark

# In Cove (docker exec -it cove /cove shell):
#   token create lighthouse --allow '*'
#   create LIGHTHOUSE_GITHUB_TOKEN <a fine-grained, read-only GitHub token>
#   bootstrap open lighthouse
# The database and its two URL keys: §10.1.

git clone https://github.com/LSariol/LightHouse.git && cd LightHouse
docker compose up -d --build
docker logs -f lighthouse     # success: "migration applied" (first start only), then "Lighthouse ready"
docker exec lighthouse /lighthouse status   # success: "✓ Everything is healthy." (or the list of what isn't)
```

**Updating:**

```bash
cd ~/LightHouse && git pull && docker compose up -d --build
```

State lives in the bind mounts, so rebuilding is safe. A deploy in progress when Lighthouse stops gets 8 seconds, then is cut off: update when `docker logs lighthouse` shows no deploy in progress (`deploy started` without `deploy finished`).

**Upgrading from the pre-1.0 Lighthouse** (one time, on the server):

1. The database setup, [§10.1](#101-database-setup-once-by-hand).
2. `git pull && docker compose up -d --build`. The new compose file no longer mounts `.env` or `repos.json` or publishes port 2000, and uses `COVE_URL`. Success: `docker logs lighthouse` shows the two migrations applied, then `Lighthouse ready` with `projects=0`.
3. Move the watched projects over, straight from the old file:
   ```bash
   docker exec -i lighthouse /lighthouse import - < /srv/server/storage/lighthouse/repos.json
   ```
   Success: one `✓ Imported <name>` per project, then `N imported, 0 already there, 0 failed.` Their deployed commits come along, so nothing redeploys unless `main` has moved since.
4. `docker exec lighthouse /lighthouse status`: every project listed and running.
5. Afterwards, `/srv/server/storage/lighthouse/repos.json` and `/srv/server/storage/lighthouse/.env` can be deleted (keep a copy of `repos.json` until you're happy).

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
5. Check: `lh status` shows it `running`; `history <one of its keys>` in Cove shows `lighthouse`.

### Removing a project

`lh remove <name>`, then on the server `docker compose -p <name> down` (Lighthouse doesn't stop or remove it).

### A deploy failed

`lh help failed` is the short version.

1. `lh list` shows the error, `lh history <name>` every attempt, and `docker logs lighthouse` the full output.
2. **The project is probably stopped** ([B1](#b1-a-failed-deploy-takes-the-project-down-and-retries-forever)). `lh start <name>` brings the previous version back while you fix it.
3. Lighthouse retries the whole deploy on every check until it succeeds. To stop the retries meanwhile: `lh pause`, and `lh resume` afterwards.
4. Fix, push to `main`, then `lh deploy <name>` or wait for the next check.

### Rotating the GitHub token

Create the new token, `update LIGHTHOUSE_GITHUB_TOKEN <token>` in Cove, `docker restart lighthouse`, then revoke the old token on GitHub.

### Rotating Lighthouse's Cove token

Delete `/srv/server/storage/lighthouse/cove/token`, `bootstrap open lighthouse` in Cove, `docker restart lighthouse`. If it leaked: `token revoke lighthouse` first. It can read **every** secret, so every secret should then be considered exposed (Cove `DOCUMENTATION.md` §14). After `token rotate` or `revoke` without a new bootstrap, Lighthouse refuses to start and says how to fix it.

### After a server reboot

Every container restarts by its own `restart:` policy. Lighthouse waits for Cove (`lh status` shows `waiting for Cove`) and starts checking once Cove answers. It doesn't start any project itself.

### Disk space

Every deploy leaves the previous image behind as a dangling image, plus build cache. Nothing cleans them up yet ([B19](#b19-old-images-and-build-cache-are-never-removed)). By hand: `docker image prune -f` and `docker builder prune -f --filter until=168h`.

---

## 12. Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `lighthouse: COVE_URL, … are not set` | The compose `environment:` lost lines; compare with the repository's `docker-compose.yml` |
| `lighthouse: STAGING_PATH "/srv/server": isn't a dedicated folder` | Both work folders are emptied on every deploy; point them at folders only Lighthouse uses |
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
| A project's last check: `GitHub: 401 Unauthorized` | The GitHub token is wrong or expired. Update `LIGHTHOUSE_GITHUB_TOKEN`, restart |
| `GitHub: 403` or `429` | Rate limit (5,000 requests an hour per token; each project uses 360 an hour at the default interval) or no access to the repo. Raise `LIGHTHOUSE_POLL_INTERVAL` |
| `GitHub: 404 Not Found` | Wrong URL, a renamed repo (`set-url`), or a private repo the token can't see. Other projects are still checked |
| `fetch secrets for X: missing: KEY` | `KEY` isn't in Cove. Create it, or fix the name in the compose file. The project is stopped meanwhile |
| `fetch secrets … forbidden_key` | Lighthouse's token doesn't cover the key; `docker logs cove` names it |
| `docker compose config failed … no such file or directory` | The unpacked folder isn't `<lowercase repo>-main`, or there's no compose file at the root |
| `Conflict. The container name "/x" is already in use` | The compose file has no `name:` matching the repo ([§7](#7-connecting-a-project-the-contract) rule 3) |
| `exec ./x.sh: permission denied` during the build | Unzip dropped the executable bit ([B9](#b9-unzipping-drops-file-permissions)) |
| A project's data folder is empty after a deploy | A relative bind mount ([B7](#b7-relative-bind-mounts-point-at-the-wrong-place)); use an absolute host path |
| The same project redeploys on every check | Its deploy keeps failing ([B1](#b1-a-failed-deploy-takes-the-project-down-and-retries-forever)). `lh pause`, fix, `lh resume` |
| Disk filling up | Old images and build cache ([§11](#disk-space)) |

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
| What the CLI can touch | Only watched projects' containers | – |
| Network | No published port; joins `spark` to reach Cove | – |
| Secrets in logs | Values and tokens are never printed | `docker compose` output is streamed raw; a future stored deploy log must scrub values ([S7](#s7-secrets-could-leak-through-deploy-output)) |

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
The regex `\$\{([^}:]+)(?::[^}]*)?\}`:
- treats `${VAR:-default}` and `${VAR:?err}` as required secrets (the cause of the Cove `v1.0.0-rc.1` failed deploy),
- reads `${VAR-default}` as a key named `VAR-default`,
- matches inside `$${VAR}`, Compose's escape for a literal `$`, which is common in healthchecks (`pg_isready -U $${POSTGRES_USER}`),
- misses `$VAR` without braces. That one is then filled from **Lighthouse's own environment**, or left empty.

**Fix:** ask Compose itself with `docker compose config --variables --format json` (Compose 2.24+). It lists every variable, braced or not, with its default and alternate values, and it already leaves out escaped `$${...}`. Fetch the variables that have no default; skip those with one unless the key exists in the project's scope. Checked on Compose 2.38: `${REQ}` → no default; `${OPT:-x}` → default `x`; `$${ESC}` → not listed; `$BARE` → listed.

#### B7. Relative bind mounts point at the wrong place
**Medium.** `internal/deploy/compose.go`, `docker-compose.yml` (the staging mount).
Compose runs **inside** Lighthouse's container, so `./data` in a project's compose file resolves to `/app/server/staging/<repo>-main/data`. The host's Docker daemon then creates that path **on the host** (where it doesn't exist) as an empty directory. The staging copy is deleted after the deploy anyway.
**Fix:** mount the staging folder at the **same path** inside and outside (`/srv/server/staging:/srv/server/staging`), unpack each deploy into a stable per-project folder, and document that persistent data uses absolute paths under `/srv/server/storage/<project>/`.

#### B8. Downloads: no timeout, no status check, wrong branch, wrong folder name
**Medium.** `internal/deploy/source.go` (`download`), `internal/github/github.go` (`LatestCommit`), `internal/deploy/compose.go` (the folder name).
- `http.Get` uses the default client with **no timeout**. A stalled download freezes all deploys for good.
- The status code isn't checked: a 404 page is saved as `<repo>.zip` and fails later as "not a valid zip file".
- The archive isn't pinned to the detected SHA. If `main` moves between check and download, Lighthouse deploys a commit it didn't record.
- The check reads the **default** branch's commits (`/commits`), while the download is always `main`.
- No auth on the download, so private repos can't be deployed.
- The unpacked folder is assumed to be `<lowercase repo>-main`. GitHub currently names it that way (checked 2026-10-05: `cove-main`, `lighthouse-main`), but it's an undocumented detail, and the API's zipball uses a different name (`<owner>-<repo>-<sha>`).

**Fix:** download `GET /repos/{owner}/{repo}/zipball/{sha}` (or `tarball`) with the PAT and a timeout, check the status, and use the archive's single top-level folder, whatever it's called.

#### B9. Unzipping drops file permissions
**Medium.** `internal/deploy/source.go` (`extract`). `os.Create` makes every file `0666 &^ umask`. Executable scripts committed to a repo lose `+x`, so `RUN ./script.sh` or an `ENTRYPOINT ["./start.sh"]` fails. (The other half of the old finding, a `defer` inside the loop holding every file open, is fixed: each entry is closed as it's written.)
**Fix:** `os.OpenFile(path, flags, file.Mode().Perm())`, close per iteration. Using the tarball, which keeps modes, is simpler still.

#### B10. No timeouts and no graceful shutdown
**Medium.** `internal/deploy/compose.go` (`exec.Command` without a context), `cmd/lighthouse/main.go` (the 8-second shutdown wait).
`docker compose` runs with `exec.Command` (no context, no timeout). A hung build or pull freezes every deploy. On SIGTERM, `main` returns at once and kills a deploy mid-way. The loop uses `time.Sleep` and ignores the context.
**Fix:** `exec.CommandContext` with a per-deploy timeout; on shutdown stop taking new work, wait for the current deploy (up to Docker's stop timeout, which should be raised with `stop_grace_period`).

#### B11. Lighthouse can't deploy itself, and Cove's deploy is fragile
**Medium.** If `LightHouse` is ever added to its own watchlist, `Build` stops the `lighthouse` container, which kills the process doing the deploy. A stop through the API counts as a manual stop, so `unless-stopped` doesn't bring it back: Lighthouse stays down. Cove deploys only because its compose file has no placeholders ([§7](#7-connecting-a-project-the-contract)).
**Fix:** B1's fix handles Cove. Self-deploy is wanted for v1.0.0 and needs a helper container ([F9](#f9-self-update)).

#### B12. "Start all" at startup does nothing; `Builder.WatchList` goes stale
**Medium.** `cmd/lighthouse/main.go:52`, `internal/watcher/watchlist.go:234`.
**Status: fixed** by removal: Lighthouse no longer tries to start projects or Cove at boot, and the builder's stale copy of the watchlist is gone (`start all` etc. read the live list).
`StartAllContainers()` runs before the watchlist is loaded, so it loops over nothing, and its error is ignored. `Builder.WatchList` is a copy taken once at load: repos added later are missing from `start all`, `stop all` and `exit all`, and removed ones are still in it. `StartAllContainers`/`StopAllContainers` also stop at the first error. The "start `cove` if it isn't running" step runs after Cove was already needed.
**Fix:** one source of truth for the project list; decide whether Lighthouse starts projects at all (restart policies already do; recommended: it doesn't).

#### B13. Names mean different things in different commands
**Medium.** `models.WatchedRepo.ContainerName` is really the **GitHub repo name** from the URL.
**Status: partly fixed.** Every command takes a project name (not case-sensitive) and container commands only act on watched projects. Multi-container projects and `remove` leaving the container running remain (pipeline and orchestrator steps).
- `start`, `stop`, `restart` and `logs` take a raw **container name**, so `stop sparkdb` or `stop cove` works on unmanaged infrastructure.
- `rebuild` takes a **display name** (case-insensitive); `remove` takes a display name (case-sensitive).
- `status` and `stop all` use `lower(repo name)`.
- Projects with more than one service (app + worker + redis) only have their one same-named container stopped, started or checked.
- `remove` leaves the container running.

**Fix:** one identifier per project, equal to the compose project name; lifecycle through `docker compose -p <project> stop|start|restart|logs|ps`, or Docker filtered by the `com.docker.compose.project` label; refuse names that aren't managed projects.

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
**Status: partly fixed.** Startup refuses `/`, `.` and system folders for both work folders; the folders are created when missing; errors name the right folder. Per-deploy folders come with the pipeline.
**Fix:** one Lighthouse-owned work root, refuse `/` and anything that isn't a dedicated folder, per-deploy subfolders created with `os.MkdirTemp`.

#### B19. Old images and build cache are never removed
**Medium** (operational). Each deploy leaves the previous image as `<none>:<none>`, plus build cache. On a home server that fills the disk over months.
**Fix:** after a successful deploy, prune dangling images for that project (label-filtered) and periodically prune build cache older than N days, keeping the last few images per project for rollback ([F3](#f3-rollback)).

#### B20. Code quality
**Low.** `gofmt` reports `internal/builder/builder.go` and `internal/watcher/watcher.go`. There's dead code (see [§17](#17-housekeeping)), mixed `fmt.Println`/`log`, no timestamps on most lines, a leftover debug line (`GOT TOKEN`), a missing newline in `ERROR IN SCAN: %v`, and no tests.
**Status: fixed.** gofmt-clean, dead code removed, `log/slog` everywhere, tests for every package except the builder (rewritten in step 3), CI on every push.

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
| 3. Pipeline | `deploy/` rewritten (prepare, swap, verify, rollback); `compose/` for running `docker compose` (config, variables, build, up, ps) with timeouts; `github/` gains archive downloads by SHA |
| 4. Checks | `checks/`: contract, policy, naming, gitleaks, test stage, Trivy |
| 5. Orchestrator | `orchestrator/` gains the queue and worker, backoff, release-only mode (`semver/`), the infrastructure tier and the reconcile loop; `docker/` gains the event stream |
| 7. Notifications, self-update | `notify/` (Discord); `lighthouse self-update` mode |

### 16.7 Database design

**Built in step 2:** roles and the database by hand (`scripts/db/setup.sql`, [§10.1](#101-database-setup-once-by-hand)); `projects`, `deployments` and the grants by migrations `00001` and `00002` ([§6](#6-data-the-database)). Connection strings live only in Cove: `LIGHTHOUSE_DATABASE_URL`, `LIGHTHOUSE_MIGRATOR_DATABASE_URL`, `LIGHTHOUSE_READER_DATABASE_URL`.

**Added by later steps**, each as new migrations when its feature lands (never by editing `00001`):

| Step | Adds |
|---|---|
| 3. Pipeline | `deployments`: `image`, `previous_image`, `rolled_back` status; `deployment_steps` (`deployment_id`, `step`, `status`, times, scrubbed `log_tail`); `projects.health_url` |
| 4. Checks | `projects`: `scan_policy`, `policy_exceptions text[]`, `secret_exceptions text[]` (keys outside `<PROJECT>_*`/`SHARED_*`), `step_timeouts` |
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
3. **Pipeline:**
   - Prepare, swap and verify (16.3), with rollback.
   - Failure classification, startup (F10).
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
