# LightHouse

LightHouse is a self-hosted CI/CD daemon written in Go. It watches your GitHub repositories for new commits, pulls the latest code, builds your Docker containers, injects secrets from a key vault, and keeps everything running — automatically. No webhooks, no external CI runners, no cloud accounts required.

Designed for homelabs, personal servers, and solo developers who want automated deployments with full control over their infrastructure.

---

## How It Works

### 1. Monitoring

LightHouse polls each watched repository every **10 seconds** using the GitHub REST API. It fetches the latest commit SHA and compares it against the last known SHA stored in `config/repos.json`. If they differ, a build is triggered.

GitHub authentication uses a Personal Access Token stored in the Cove key vault under the key `LIGHTHOUSE_GITHUB_PAT`.

### 2. Building & Deploying

When a new commit is detected, LightHouse runs this sequence:

1. **Clean up** — wipes the temporary download and staging directories.
2. **Download** — fetches the repo's `main` branch as a ZIP from GitHub.
3. **Stop** — stops the currently running container for that repo (if any).
4. **Unpack** — extracts the ZIP into the staging directory.
5. **Inject secrets** — parses the repo's `docker-compose.yml`, finds every `${VAR_NAME}` reference, and fetches each value from the Cove key vault.
6. **Build & start** — runs `docker compose up -d --build --remove-orphans` with the fetched secrets injected into the subprocess environment. Secrets are never written to disk.
7. **Clean up** — removes the staging files. The container keeps running on the host.

### 3. Secret Management (Cove Integration)

LightHouse integrates with [Cove](https://github.com/LuSracol/Cove), a companion key vault project that runs as a container on the same Docker network. All sensitive values — GitHub tokens, database URLs, API keys — are stored in Cove and fetched at runtime.

On first run LightHouse bootstraps its own Cove client secret automatically and saves it to `.env`. From that point on, any secret your service needs just needs to exist in Cove under the matching key name.

### 4. Docker Architecture

LightHouse runs as a container with `/var/run/docker.sock` mounted, giving it access to the host Docker daemon. Watched services are built and started as sibling containers on the host — not inside LightHouse's container. The `spark` Docker network is shared between LightHouse, Cove, and all watched services.

---

## Supported Project Requirements

For LightHouse to watch and deploy a repository, the repo must meet these requirements.

### Required Files

| File | Requirement |
|------|-------------|
| `Dockerfile` | Must exist at the repo root. LightHouse uses it to build the image. |
| `docker-compose.yml` | Must exist at the repo root. LightHouse reads it to discover required secrets and runs `docker compose up` from it. |

### docker-compose.yml Format

LightHouse parses `docker-compose.yml` specifically to find environment variable references in the format `${VAR_NAME}` or `${VAR_NAME:-default}`. Every variable referenced this way will be fetched from Cove and injected at build time.

**Minimal example:**

```yaml
services:
  my-service:
    build: .
    restart: unless-stopped
    environment:
      - DATABASE_URL=${DATABASE_URL}
      - API_TOKEN=${API_TOKEN}
    networks:
      - spark

networks:
  spark:
    external: true
```

**Rules:**
- Use `${VAR_NAME}` syntax for any secret or environment-specific value. Plain `VAR=value` literals are fine for non-sensitive config.
- Every `${VAR_NAME}` referenced must exist as a secret in Cove, or the build will fail.
- The service should join the `spark` external network if it needs to communicate with Cove or other LightHouse-managed services.
- The container name in compose should be consistent — LightHouse uses it to stop the old container before rebuilding.

### Branch

LightHouse always downloads and builds from the **`main` branch**. Ensure your deployable code is merged to `main`.

### Cove Secrets

Before adding a repo to LightHouse, ensure all secrets referenced in `docker-compose.yml` are stored in Cove. LightHouse will attempt to fetch each `${VAR_NAME}` from Cove at build time using the exact key name from the compose file.

---

## Configuration

Copy `.env.example` to `.env` and fill in your values:

```env
COVE_ADDRESS=http://cove:2100        # Address of your Cove instance
COVE_CLIENT_SECRET=                  # Leave blank on first run — auto-generated
APP_ENV_PATH=.env
APP_REPO_PATH=config/repos.json
DOWNLOAD_PATH=Server/Download/
STAGING_PATH=Server/Staging/
```

Inside Docker these paths are remapped to `/app/` mount points via the `docker-compose.yml` environment block.

---

## CLI

LightHouse exposes an interactive CLI on stdin (requires `tty: true` in Docker, which is set by default).

| Command | Description |
|---------|-------------|
| `add <name> <github-url>` | Add a repo to the watchlist |
| `remove <name>` | Remove a repo |
| `change <name> <new-url>` | Update a repo's URL |
| `list` | Print all watched repos and their stats |
| `start <name\|ALL>` | Start a container (or all of them) |
| `stop <name\|ALL>` | Stop a container (or all of them) |
| `scan` | Manually trigger one scan cycle immediately |
| `exit [all]` | Shut down LightHouse; `exit all` stops all containers first |

---

## File Structure

```
cmd/lighthouse/main.go          Entry point — wires up all subsystems
internal/
  watcher/
    watcher.go                  Polling loop, commit detection
    github.go                   GitHub API requests
    watchlist.go                CRUD operations on repos.json
    cove.go                     Cove client init, GitHub PAT loading
  builder/
    builder.go                  Build orchestration
    engine.go                   Secret injection, docker compose execution
    docker.go                   Docker API: start / stop / list containers
    workspace.go                Staging and download directory cleanup
  models/
    models.go                   WatchedRepo and RepoStats types
    update.go                   Stats mutation helpers
  config/
    envs.go                     .env loading and patching
  cli/
    cli.go                      Interactive command loop
config/
  repos.json                    Persistent watchlist with per-repo stats
Server/
  Download/                     Temporary storage for repo ZIPs
  Staging/                      Temporary storage for unpacked repos
docker-compose.yml              LightHouse service definition
Dockerfile                      Multi-stage Go build for LightHouse itself
lighthouse.example.yaml         Future service config schema (not yet active)
.env.example                    Required environment variable template
PROJECT_CONTEXT.md              Internal architecture reference for contributors
```

---

## Running LightHouse

```bash
docker compose up -d
```

On first run LightHouse will bootstrap its Cove client secret and save it to `.env`. After that, add repos with the CLI and LightHouse will start watching them immediately.

---

## To Do

- Self-updater (LightHouse watching itself)
- Fix CLI watchlist commands broken after model update
- Wire up `internal/orchestrator/`
- Implement `lighthouse.yaml` service config reader (healthchecks, resource limits, deploy strategies)
