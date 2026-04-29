# cdeploy [![Go Report Card](https://goreportcard.com/badge/github.com/lexxzar/compose-deploy)](https://goreportcard.com/report/github.com/lexxzar/compose-deploy) [![Coverage Status](https://coveralls.io/repos/github/lexxzar/compose-deploy/badge.svg?branch=main)](https://coveralls.io/github/lexxzar/compose-deploy?branch=main)

cdeploy is a TUI/CLI app for teams and solo developers who deploy Docker Compose apps to a few servers over SSH.

Instead of SSH-ing into each machine and running `docker compose stop && docker compose rm -f && docker compose pull && docker compose up --no-start && docker compose start` by hand, cdeploy wraps that rollout into a single command or terminal UI.

No daemon. No agents to install on your servers. No cluster orchestrator. Single binary. Plain SSH.

Highlights:

- Shows published port mappings (host:port→container) alongside service status in both the TUI list and `cdeploy list` CLI output.

## Install

```bash
go install github.com/lexxzar/compose-deploy@latest
```

Or build from source:

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

The TUI walks through up to six screens:

1. **Server select** — choose a remote server or "Local" (only shown when servers are configured); press `s` to open the inline settings editor for managing servers
2. **Project select** — pick a Docker Compose project (auto-skipped if the current directory has a compose file)
3. **Service select** — pick services and choose an action (`r` restart, `d` deploy, `s` stop, `l` logs, `c` config, `x` exec)
4. **Progress** — watch step-by-step execution with status indicators
5. **Logs** — live-stream logs for the selected service
6. **Config** — inspect or edit the compose file, toggle between raw and resolved config, and see validation status

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

# List services as JSON (each service includes a `ports` array of
# {host, host_port, container_port, protocol} entries when published)
cdeploy list --json

# Stream logs for a service
cdeploy logs nginx

# Dump last 100 lines and exit
cdeploy logs nginx -n 100 --no-follow

# Exec into a running container (default: tries bash, falls back to sh)
cdeploy exec nginx

# Run a specific command inside a container
cdeploy exec web -- rails console
```

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

Define remote servers in `~/.cdeploy/servers.yml`:

```yaml
servers:
  - name: app.dev
    host: deploy@app.dev
    group: Dev
    color: green
  - name: discovery.dev
    host: deploy@discovery.dev
    group: Dev
    color: cyan
  - name: app.prod
    host: deploy@app.prod
    group: Production
    color: red
  - name: discovery.prod
    host: deploy@discovery.prod
    project_dir: /opt/apps/web
    group: Production
    color: red
```

| Field | Required | Description |
|-------|----------|-------------|
| `name` | yes | Identifier used in TUI and `--server` flag |
| `host` | yes | SSH destination (`user@hostname`) |
| `project_dir` | no | Default project directory on the remote host |
| `group` | no | Visual group label in the TUI server picker — servers with the same group are displayed together under a shared header |
| `color` | no | Breadcrumb badge color for the selected server on post-selection TUI screens; when omitted, the breadcrumb uses plain text |

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

For scaled services, the worst-case health is shown (unhealthy > starting > healthy). Services without a health check show only the running/stopped dot.

## Logging

All docker compose output is logged to `~/.cdeploy/logs/`. Each log file is named `cdeploy_on_{hostname}_{timestamp}.log`, so you get a per-host, timestamped record of every operation. Override the directory with `--log-dir`.

## Compose Config Screen

From the service screen, press `c` to open the compose config viewer/editor. This works for both local projects and remote servers selected through the TUI.

- `r` toggles between the raw compose file and resolved/interpolated `docker compose config` output
- `e` opens the compose file in your editor. Local mode uses `$EDITOR`, then `$VISUAL`, then `vi`; values like `code --wait` are supported. Remote mode runs `${EDITOR:-vi}` over SSH on the target host.
- After the editor exits, cdeploy reloads the raw file, switches back to raw view, and validates it with `docker compose config --quiet`. Validation errors are shown inline in the TUI.

## Why cdeploy?

cdeploy is not a replacement for Kubernetes, Docker Swarm, or full deployment platforms like Kamal. It's for teams and solo developers who deploy to a handful of servers with plain `docker compose` and just want a faster, less error-prone way to do it — without installing anything on the servers themselves.

## License

[MIT](LICENSE)

## Requirements

- Go 1.26+
- Docker with Compose v2 plugin (`docker compose`)
- SSH client (for remote server support)