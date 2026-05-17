# cdeploy [![Go Report Card](https://goreportcard.com/badge/github.com/lexxzar/compose-deploy)](https://goreportcard.com/report/github.com/lexxzar/compose-deploy) [![Coverage Status](https://coveralls.io/repos/github/lexxzar/compose-deploy/badge.svg?branch=main)](https://coveralls.io/github/lexxzar/compose-deploy?branch=main)

cdeploy is a TUI/CLI app for teams and solo developers who deploy and manage Docker Compose stacks — locally or on a few servers over SSH.

Instead of SSH-ing into each machine and running this rollout by hand:

```bash
docker compose stop
docker compose rm -f
docker compose pull
docker compose up --no-start
docker compose start
```

cdeploy wraps it into a single command or terminal UI.

No daemon. No agents to install on your servers. No cluster orchestrator. Single binary. Plain SSH.

## Why cdeploy?

cdeploy is not a replacement for Kubernetes, Docker Swarm, or full deployment platforms like Kamal. It's for teams and solo developers who deploy to a handful of servers with plain `docker compose` and just want a faster, less error-prone way to do it — without installing anything on the servers themselves.

## Requirements

- Go 1.26+ (for `go install` or building from source)
- Docker Compose v2 plugin (`docker compose`) or v1 standalone (`docker-compose`) — auto-detected
- SSH client (for remote server support)

## Install

**Prebuilt binaries** (recommended): grab the latest `.tar.gz`, `.deb`, or `.rpm` for your platform from the [Releases page](https://github.com/lexxzar/compose-deploy/releases). Linux and macOS, amd64 and arm64.

**From source**:

```bash
go install github.com/lexxzar/compose-deploy@latest
```

Or clone and build:

```bash
git clone https://github.com/lexxzar/compose-deploy.git
cd compose-deploy
go build -o cdeploy .
```

## Usage

### TUI Mode

Run without arguments to launch the interactive interface:

```bash
cdeploy
```

After you select a remote server, the server name is shown in the breadcrumb on subsequent screens. If that server has a `color` set in `~/.cdeploy/servers.yml`, the breadcrumb renders it as a colored badge; if `color` is omitted, the breadcrumb stays plain text.

The TUI has six main screens, plus an inline settings editor reachable from screen 1:

1. **Server select** — choose a remote server or "Local" (only shown when servers are configured); press `s` to open the settings editor for managing servers
2. **Project select** — pick a Docker Compose project (auto-skipped if the current directory has a compose file)
3. **Service select** — pick services and choose an action (`r` restart, `d` deploy, `s` stop, `l` logs, `c` config, `x` exec, `U` re-check updates); also shows CPU% and Mem (used/limit) columns for running services, refreshed on screen entry and after every operation. Services whose registry image is newer than the local copy get a yellow `⇧` marker next to the service name; the indicator is cached for 10 minutes and `U` forces a refresh.
4. **Progress** — watch step-by-step execution with status indicators
5. **Logs** — live-stream logs for the selected service
6. **Config** — inspect or edit the compose file, toggle between raw and resolved config, and see validation status

#### Navigation

| Key | Action |
|-----|--------|
| `esc` | Back to the previous screen; cancels confirmation prompts and in-flight operations |
| `q` | Back on nested screens (alias for `esc`); quits on root screens |
| `ctrl+c` | Quit. On remote sessions, prompts to confirm the disconnect (`y` to quit, `n`/`esc` to cancel) |

`q` is typeable inside settings-form text inputs and is a no-op on the progress screen while an operation is in flight — use `esc` to cancel.

### CLI Mode

```bash
# Deploy specific containers (stop → remove → pull → create → start)
cdeploy deploy nginx postgres

# Deploy all containers
cdeploy deploy -a

# Restart specific containers (stop → remove → create → start)
cdeploy restart nginx

# Restart all containers
cdeploy restart -a

# Stop specific containers
cdeploy stop nginx

# List services and their status
cdeploy list

# List services with CPU and memory usage (~1.5s per discovered project)
cdeploy list --stats

# List services as JSON (for scripts and CI)
cdeploy list --json

# Combine stats with JSON output
cdeploy list --stats --json

# Show image-update indicators (single-project mode auto-enables this)
cdeploy list -C /opt/myapp        # single-project: always checks
cdeploy list --updates            # multi-project: opt in explicitly

# Stream logs for a service
cdeploy logs nginx

# Dump last 100 lines and exit
cdeploy logs nginx -n 100 --no-follow

# Exec into a running container (default: tries bash, falls back to sh)
cdeploy exec nginx

# Run a specific command inside a container
cdeploy exec web -- rails console
```

**`list --json` output** (one entry per service, grouped by project):

```json
[
  {
    "project": "myapp",
    "service": "nginx",
    "running": true,
    "health": "healthy",
    "created": "2026-05-14 10:22",
    "uptime": "2d",
    "ports": [
      { "host": "0.0.0.0", "host_port": 8080, "container_port": 80, "protocol": "tcp" }
    ],
    "update_available": true
  }
]
```

`health`, `created`, `uptime`, `ports`, and `update_available` are omitted when not applicable (no healthcheck, stopped container, no published ports, update check not performed or build-only service).

With `--stats`, three additional fields are populated per running service: `cpu_percent` (`100.0` = one full core; sums across replicas for scaled services), `memory_used` (bytes), `memory_limit` (bytes; equals host memory when no explicit limit is set). The fields are omitted entirely when `--stats` is not passed, so existing scripts see byte-identical output. On stats fetch failure the CLI prints `cdeploy: stats unavailable: <err>` to stderr (single-project mode) or `cdeploy: stats unavailable for "<project>": <err>` (multi-project mode), exits 0, and renders blank cells — status is the load-bearing primary view.

### Update-available indicators (`--updates`)

A yellow `⇧` glyph next to a service name in `cdeploy list` and the TUI service-select screen means the image in the registry has a different digest than the locally pulled copy. The check runs `docker compose config --format json` to map services to images, then per-image `docker image inspect` (local RepoDigest) and `docker buildx imagetools inspect` (registry manifest-list digest), falling back to `docker manifest inspect --verbose` when the buildx plugin is unavailable. Build-only services and services whose digest cannot be determined render a blank cell — the indicator is tri-state (unknown / current / update available).

- **Single-project mode** (`-C` specified, or `--ssh` which requires `-C`): always checks for updates.
- **Multi-project mode** (no `-C`): opt-in via `--updates` because each project triggers its own registry probes.

Failures are non-fatal: `cdeploy: updates unavailable: <err>` (single-project) or `cdeploy: updates unavailable for "<project>": <err>` (multi-project) is written to stderr, the cell stays blank, exit code 0. JSON output adds `update_available: true|false` with `omitempty`; existing JSON consumers see the original wire shape when the flag is absent.

In the TUI, the indicator is cached for 10 minutes per (project, server) context. Press `U` on the service-select screen to bypass the cache and re-check immediately.

**Multi-arch images:** for multi-arch images (commonly: `nginx`, `postgres`, `alpine`, `node`, `redis`), the check uses `docker buildx imagetools inspect` which returns the manifest-LIST digest — matching what `docker image inspect` records locally — so multi-arch images are reported correctly. The legacy `docker manifest inspect --verbose` fallback (used only when the buildx plugin is unavailable, i.e. very old Docker installs) returns per-platform descriptor digests that never match the local manifest-list digest and can produce false positives for multi-arch images; upgrading to Docker v23+ (which ships buildx by default) eliminates that case. Run `docker pull` manually to confirm a flagged update before deploying if you suspect a false positive.

**Exit codes**: `0` on success, non-zero on failure (config errors, SSH/Docker failures, validation errors). Suitable for CI gating.

#### Remote servers (CLI)

```bash
# Deploy all containers on a configured remote server
cdeploy -s prod-web deploy -a

# Restart a service on a remote server with explicit project directory
cdeploy -s staging -C /opt/apps/web restart nginx

# List services on a remote server
cdeploy -s prod list

# Stream logs on a remote server
cdeploy logs nginx -s prod -C /opt/myapp

# Exec into a container on a remote server
cdeploy exec nginx -s prod -C /opt/myapp
```

#### Ad-hoc SSH connection (`-S`/`--ssh`)

For one-off remote operations (CI scripts, automation) without a `~/.cdeploy/servers.yml` entry, pass an SSH connection string directly:

```bash
# Deploy against an ad-hoc host (uses default SSH user from ~/.ssh/config)
cdeploy deploy -S host -C /srv/app -a

# Deploy with explicit user
cdeploy deploy -S deploy@host -C /srv/app -a

# Restart with a non-default SSH port
cdeploy restart -S deploy@host:2222 -C /srv/app nginx

# List services on an ad-hoc host
cdeploy list -S deploy@host -C /srv/app

# Stream logs on an ad-hoc host
cdeploy logs nginx -S deploy@host -C /srv/app

# Exec into a container on an ad-hoc host
cdeploy exec nginx -S deploy@host -C /srv/app

# Use an ad-hoc SSH key (CI/automation workflows that write keys from secrets)
cdeploy -S deploy@1.2.3.4 -i ~/.ssh/ci.pem -C /opt/app deploy
```

The connection string format is `[user@]host[:port]`. The `-S`/`--ssh` flag is **mutually exclusive** with `-s`/`--server` and **requires** `-C`/`--project-dir` (no config lookup is performed).

**SSH identity (`-i`/`--identity`):** pass an SSH private key path inline. Only valid alongside `-S`/`--ssh`; intended for CI/ephemeral use where writing a `~/.ssh/config` entry is impractical. For configured servers, use `IdentityFile` in `~/.ssh/config` instead. The path supports `~/` expansion and is validated at parse time (must exist, be a regular file, and be readable).

**CI usage:** `--ssh` requires passwordless SSH authentication on the target host — configure keys via `~/.ssh/config`, `ssh-agent`, or `-i`/`--identity` before running. Host-key verification still applies; either pre-populate `~/.ssh/known_hosts` or use the standard `StrictHostKeyChecking` settings in your SSH config.

### Global Flags

```
-s, --server string        Remote server name from ~/.cdeploy/servers.yml
-S, --ssh string           Ad-hoc SSH connection string [user@]host[:port] (mutually exclusive with --server)
-i, --identity string      Path to SSH private key (requires --ssh)
-C, --project-dir string   Docker compose project directory (default: current directory)
    --log-dir string       Log directory (default ~/.cdeploy/logs/)
```

## Remote Server Configuration

> Need a one-off connection without editing the config file? See [Ad-hoc SSH connection (`-S`/`--ssh`)](#ad-hoc-ssh-connection--s--ssh) above for a CLI-only alternative aimed at scripts and CI.

Define remote servers in `~/.cdeploy/servers.yml`. Colors are defined once per group and inherited by every server in that group:

```yaml
groups:
  - name: Dev
    color: green
  - name: Production
    color: red

servers:
  - name: app.dev
    host: deploy@app.dev
    group: Dev
  - name: discovery.dev
    host: deploy@discovery.dev
    group: Dev
  - name: app.prod
    host: deploy@app.prod
    group: Production
  - name: discovery.prod
    host: deploy@discovery.prod
    project_dir: /opt/apps/web
    group: Production
```

Ungrouped servers may set `color` directly on the server entry. Older configs with per-server colors on grouped servers are auto-migrated on load (first-server-wins per group); rewriting them in the format above is recommended.

**Top-level fields**

| Field | Description |
|-------|-------------|
| `groups[].name` | Group identifier referenced by `servers[].group` |
| `groups[].color` | Breadcrumb badge color shared by every server in the group |
| `servers[]` | List of server entries (see below) |

**Server fields**

| Field | Required | Description |
|-------|----------|-------------|
| `name` | yes | Identifier used in TUI and `--server` flag |
| `host` | yes | SSH destination (`user@hostname`) |
| `project_dir` | no | Default project directory on the remote host |
| `group` | no | Name of a group defined in `groups:` — servers with the same group are displayed together and share the group's color |
| `color` | no | Breadcrumb badge color. Only meaningful for ungrouped servers; ignored when `group` is set (the group's color wins) |

Allowed `color` values: `red`, `green`, `yellow`, `blue`, `magenta`, `cyan`, `white`, `gray`. A common pattern is to mark production servers red so they stand out before you run an operation.

SSH-specific options (keys, jump hosts, tunnels, ports) belong in `~/.ssh/config` — cdeploy uses the system `ssh` binary and inherits its configuration. Exception: for ad-hoc CI/automation use, `-i/--identity` may be passed alongside `-S/--ssh` to supply a key path inline without a `~/.ssh/config` entry.

### How it works

cdeploy uses SSH ControlMaster multiplexing:

1. A persistent control socket is established once (password/key prompts happen here)
2. All subsequent docker compose commands and compose-file access reuse the socket with zero auth overhead
3. The socket is torn down on disconnect or TUI quit

In TUI mode, the SSH connect command runs with full terminal access so interactive prompts (passwords, host key verification) work naturally.

## Operations

| Operation | Steps |
|-----------|-------|
| **Deploy** | stop → remove → pull → create → start |
| **Restart** | stop → remove → create → start |
| **Stop** | stop |

## Health Checks

If your services define Docker health checks, cdeploy displays their status alongside the running/stopped indicator:

- **♥** healthy
- **✗** unhealthy
- **~** starting (health check hasn't passed yet)

For scaled services, the worst-case health is shown (unhealthy > starting > healthy). Services without a health check show only the running/stopped dot. The same icons appear in `cdeploy list` output.

## Logging

All docker compose output is logged to `~/.cdeploy/logs/`. Each log file is named `cdeploy_on_{hostname}_{timestamp}.log`, so you get a per-host, timestamped record of every operation. Override the directory with `--log-dir`.

## Compose Config Screen

From the service screen, press `c` to open the compose config viewer/editor. This works for both local projects and remote servers selected through the TUI.

- `r` toggles between the raw compose file and resolved/interpolated `docker compose config` output
- `e` opens the compose file in your editor. Local mode uses `$EDITOR`, then `$VISUAL`, then `vi`; values like `code --wait` are supported. Remote mode runs `${EDITOR:-vi}` over SSH on the target host.
- After the editor exits, cdeploy reloads the raw file, switches back to raw view, and validates it with `docker compose config --quiet`. Validation errors are shown inline in the TUI.

## License

[MIT](LICENSE)