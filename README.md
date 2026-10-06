# Lighthouse

**Push to GitHub, and your home server updates itself.**

Lighthouse keeps the projects on a personal server up to date. It watches your GitHub repositories, and when new code lands on `main`, it downloads it, builds it with Docker, hands it the passwords and API keys it needs, and starts it. You don't SSH in to run `git pull`, and there's no cloud CI service to pay for or trust.

It's built for one person running their own projects on their own hardware: a homelab, a spare PC, a small Debian box behind a Cloudflare tunnel.

> **Status:** working and in daily use, currently being polished for its **v1.0.0** release. Expect changes. See [DOCUMENTATION.md](DOCUMENTATION.md) for the details and the roadmap.

---

## What it does

- **Watches your repos.** Every few seconds it checks whether `main` has a new commit.
- **Deploys automatically.** New commit → download → `docker compose up --build`. Your project is running the new version a minute later.
- **Keeps secrets out of your code.** Your project's `docker-compose.yml` names the secrets it needs, like `${MYAPP_DATABASE_URL}`. Lighthouse fetches them from [Cove](https://github.com/LSariol/Cove), a small self-hosted vault, at deploy time. Nothing secret lives in the repo or on disk, and your app only reads normal environment variables.
- **Gives you a control panel.** A simple command prompt to list projects, see what's running, read logs, force a rebuild, or pause deploys.

## How it fits together

```
   You push to main
         │
         ▼
      GitHub  ◄──── Lighthouse checks for new commits
                         │
                         ├──► Cove: "what are this project's secrets?"
                         │
                         └──► Docker: build and start the new version
                                    │
                                    ▼
                       Your apps, running on your server
```

Lighthouse, Cove and your apps all run as Docker containers on the same server, on a shared private Docker network called `spark`.

---

## Getting a project ready for Lighthouse

Your repository needs:

1. **A `docker-compose.yml`** at the top level that builds and runs it.
2. **Its deployable code on `main`.**
3. **Secrets written as placeholders.** Anything secret goes in as `${NAME}`, and you store the real value in Cove under that same name. Settings that aren't secret are just written out normally.

```yaml
name: myapp

services:
  myapp:
    build: .
    container_name: myapp
    restart: unless-stopped
    environment:
      - DATABASE_URL=${MYAPP_DATABASE_URL}   # secret: comes from Cove
      - LOG_LEVEL=info                       # plain setting
    networks:
      - spark

networks:
  spark:
    external: true
```

Then tell Lighthouse about it:

```
add myapp https://github.com/you/myapp
```

That's it. The next push to `main` deploys it. The full list of rules (naming, storage folders, a few gotchas) is in [DOCUMENTATION.md §7](DOCUMENTATION.md#7-connecting-a-project-the-contract).

---

## Running Lighthouse

You'll need a Linux server with Docker and Docker Compose, a running [Cove](https://github.com/LSariol/Cove), and a GitHub personal access token with read-only access to your repos.

1. **In Cove**, give Lighthouse access and store your GitHub token:
   ```
   token create lighthouse --allow '*'
   create LIGHTHOUSE_GITHUB_TOKEN <your token>
   bootstrap open lighthouse
   ```
2. **On the server**, create the folders Lighthouse keeps its state in (see [DOCUMENTATION.md §10](DOCUMENTATION.md#10-deploying-lighthouse) for the exact commands), then start it:
   ```bash
   git clone https://github.com/LSariol/LightHouse.git
   cd LightHouse
   docker compose up -d --build
   ```
3. **Open the control prompt** with `docker attach lighthouse` and add your first project. To leave the prompt without stopping Lighthouse, press **Ctrl-P** then **Ctrl-Q**.

On its first start Lighthouse picks up its Cove access automatically. After that it just runs, and it comes back after a reboot.

## Everyday commands

| Command | What it does |
|---|---|
| `list` | Show the projects being watched |
| `status` | Show which ones are running |
| `add <name> <github-url>` | Start watching a project |
| `remove <name>` | Stop watching a project |
| `rebuild <name>` | Redeploy now, even with no new commit |
| `logs <name>` | Show a project's recent output |
| `pause` / `resume` | Hold all deploys / carry on |
| `help` | Everything else |

---

## Learn more

- **[DOCUMENTATION.md](DOCUMENTATION.md)**: how everything works, configuration, troubleshooting, security, known issues and the v1.0.0 roadmap.
- **[Cove](https://github.com/LSariol/Cove)**: the secret vault Lighthouse gets its secrets from.
- **[CoveClient](https://github.com/LSariol/CoveClient)**: the Go library Lighthouse uses to talk to Cove.

Built with Go and Docker, for one person's home server. It isn't meant to be a general-purpose CI/CD platform, but you're welcome to borrow ideas from it.
