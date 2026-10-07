# Lighthouse

**Push to GitHub, and your home server updates itself.**

Lighthouse keeps the projects on a personal server up to date. It watches your GitHub repositories, and when new code lands on `main`, it downloads it, builds it with Docker, hands it the passwords and API keys it needs, and starts it. You don't SSH in to run `git pull`, and there's no cloud CI service to pay for or trust.

It's built for one person running their own projects on their own hardware: a homelab, a spare PC, a small Debian box behind a Cloudflare tunnel.

> **Status:** working and in daily use, currently being polished for its **v1.0.0** release. Expect changes. See [DOCUMENTATION.md](DOCUMENTATION.md) for the details and the roadmap.

---

## What it does

- **Watches your repos.** Every few seconds it checks whether `main` has a new commit.
- **Deploys safely.** New commit → download → build → swap. Your old version keeps running while the new one builds, and if the new one doesn't come up healthy, the old one is put back. A commit that keeps failing is set aside until you push a fix.
- **Follows a branch or your releases.** By default every commit on `main` deploys. A project can instead deploy only version tags (`v1.2.3`), set in its own compose file, and infrastructure (the database, the vault) deploys first, alone, after a database backup.
- **Brings things back.** If a project's containers disappear, Lighthouse redeploys what was running.
- **Updates itself.** Tag a Lighthouse release and it deploys it like any project, through a short-lived helper container that puts the old version back if the new one isn't healthy.
- **Goes back when you ask.** `rollback <name>` returns to what ran before, and it stays there until something newer comes along.
- **Checks before it deploys.** A compose file that would reach outside its project (root on the host, another project's files or secrets, the Docker socket) is refused before anything is built, and a Dockerfile `test` stage runs first if there is one. Exceptions are written down in `policy.json`.
- **Keeps secrets out of your code.** Your project's `docker-compose.yml` names the secrets it needs, like `${MYAPP_DATABASE_URL}`. Lighthouse fetches them from [Cove](https://github.com/LSariol/Cove), a small self-hosted vault, at deploy time. Nothing secret lives in the repo or on disk, and your app only reads normal environment variables.
- **Gives you a control panel.** A simple command prompt to list projects, see what's running, read logs, redeploy, see how past deploys went, or pause deploys.
- **Remembers what happened.** Every deploy is recorded in a small Postgres database, so you can see when something changed and why a deploy failed.

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

1. **A `docker-compose.yml`** at the top level that builds and runs it. It can have as many services as you like.
2. **Its deployable code on `main`** (or whatever its default branch is). Private repositories work too.
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

That's it. The next push to `main` deploys it. A healthcheck on your services lets Lighthouse tell whether a new version really works before it keeps it. The full list of rules (naming, storage folders, a few gotchas) is in [DOCUMENTATION.md §7](DOCUMENTATION.md#7-connecting-a-project-the-contract).

---

## Running Lighthouse

You'll need a Linux server with Docker and Docker Compose, a Postgres server for Lighthouse's own small database, a running [Cove](https://github.com/LSariol/Cove), and a GitHub personal access token with read-only access to your repos. The one-time database setup is a script; [DOCUMENTATION.md §10.1](DOCUMENTATION.md#101-database-setup-once-by-hand) walks through it.

1. **In Cove**, give Lighthouse access and store your GitHub token:
   ```
   token create lighthouse --allow '*'
   create LIGHTHOUSE_GITHUB_TOKEN <your token>
   bootstrap open lighthouse
   ```
2. **On the server**, create the folders Lighthouse keeps its state in (see [DOCUMENTATION.md §10](DOCUMENTATION.md#10-deploying-lighthouse) for the exact commands), then start it:
   ```bash
   git clone https://github.com/lsariol/lighthouse.git
   cd lighthouse
   docker compose up -d --build
   ```
3. **Open the control prompt** and add your first project:
   ```bash
   docker exec -it lighthouse /lighthouse shell
   ```
   Type `help` to see everything it can do, and `exit` to leave (Lighthouse keeps running). Single commands work too: `docker exec lighthouse /lighthouse status`.

On its first start Lighthouse picks up its Cove access automatically. After that it just runs, and it comes back after a reboot.

## Everyday commands

| Command | What it does |
|---|---|
| `status` | Is everything healthy? |
| `list` | Show the projects being watched |
| `add <github-url>` | Start watching a project (named after the repository) |
| `remove <name>` | Stop watching a project |
| `deploy <name> [tag or commit]` | Deploy now, even with no new commit; or a given release or commit |
| `rollback <name>` | Go back to what ran before |
| `check <name>` | Would its latest commit deploy? (changes nothing) |
| `logs <name>` | Show a project's recent output |
| `history <name>` | How its recent deploys went |
| `pause` / `resume` | Hold all deploys / carry on |
| `help` | Everything else, with examples (`help setup` walks through preparing a repo; `help rules` lists what a deploy refuses) |

---

## Learn more

- **[DOCUMENTATION.md](DOCUMENTATION.md)**: how everything works, configuration, troubleshooting, security, known issues and the v1.0.0 roadmap.
- **[Cove](https://github.com/LSariol/Cove)**: the secret vault Lighthouse gets its secrets from.
- **[CoveClient](https://github.com/LSariol/CoveClient)**: the Go library Lighthouse uses to talk to Cove.

Built with Go and Docker, for one person's home server. It isn't meant to be a general-purpose CI/CD platform, but you're welcome to borrow ideas from it.
