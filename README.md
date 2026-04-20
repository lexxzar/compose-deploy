# cdeploy [![Go Report Card](https://goreportcard.com/badge/github.com/lexxzar/compose-deploy)](https://goreportcard.com/report/github.com/lexxzar/compose-deploy) [![Coverage Status](https://coveralls.io/repos/github/lexxzar/compose-deploy/badge.svg?branch=main)](https://coveralls.io/github/lexxzar/compose-deploy?branch=main)

cdeploy is a TUI/CLI app for teams and solo developers who deploy Docker Compose apps to a few servers over SSH.

Instead of SSH-ing into each machine and running `docker compose stop && docker compose rm -f && docker compose pull && docker compose up --no-start && docker compose start` by hand, cdeploy wraps that rollout into a single command or terminal UI.

No daemon. No agents to install on your servers. No cluster orchestrator. Single binary. Plain SSH.

## Prerequisites

- Go 1.26+
- Docker with either Compose v2 plugin (`docker compose`) or standalone Compose (`docker-compose`)
- SSH client (for remote server support)

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

## Quick Start

### Local project

From a directory that contains a Compose file (`compose.yml`, `compose.yaml`, `docker-compose.yml`, or `docker-compose.yaml`):

```bash
# Launch the TUI
cdeploy

# Or use the CLI directly
cdeploy list
cdeploy deploy -a
```

### Remote server

1. Define one or more servers in `~/.cdeploy/servers.yml`
2. Make sure `ssh <host>` works with your normal SSH config
3. Run a command against the configured server

```bash
cdeploy -s prod list
cdeploy -s prod -C /opt/myapp deploy -a
```

## Usage

### TUI Mode

Run without arguments to launch the interactive interface:

```bash
cdeploy
```

After you select a remote server, the server name is shown in the breadcrumb on subsequent screens. If that server has a `color` set in `~/.cdeploy/servers.yml`, the breadcrumb renders it as a colored badge; if `color` is omitted, the breadcrumb stays plain text.

The TUI walks through up to six screens:

1. **Server select** — choose a remote server or "Local" (only shown when servers are configured)
2. **Project select** — pick a Docker Compose project (auto-skipped if the current directory has a compose file)
3. **Service select** — pick services and choose an action (`r` restart, `d` deploy, `s` stop, `l` logs, `c` config)
4. **Progress** — watch step-by-step execution with status indicators
5. **Logs** — live-stream logs for the selected service
6. **Config** — inspect or edit the compose file, toggle between raw and resolved config, and see validation status

#### TUI Keys

**Service screen**

- `space` toggle selection
- `a` select or clear all
- `r` restart selected services
- `d` deploy selected services
- `s` stop selected services
- `l` open the log viewer for the focused service
- `c` open the config viewer/editor for the current project
- `esc` go back
- `q` quit (shows a confirmation prompt when connected to a remote server)

**Log viewer**

- `w` toggle soft wrap
- `p` toggle JSON pretty-printing
- `G` jump to bottom
- `esc` return to the service screen
- `q` quit (shows a confirmation prompt when connected to a remote server)

**Config screen**

- `r` toggle raw vs resolved `docker compose config`
- `e` edit the compose file in `$EDITOR`
- `esc` return to the service screen
- `q` quit (shows a confirmation prompt when connected to a remote server)

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

# List services as JSON
cdeploy list --json

# Stream logs for a service
cdeploy logs nginx

# Dump last 100 lines and exit
cdeploy logs nginx -n 100 --no-follow
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
```

### Global Flags

```
-s, --server string        Remote server name from ~/.cdeploy/servers.yml
-C, --project-dir string   Docker Compose project directory
    --log-dir string       Log directory (default ~/.cdeploy/logs/)
```

Notes:

- For `deploy`, `restart`, `stop`, and `logs`, omitting `-C` means "use the current directory" in local mode
- For remote `deploy`, `restart`, `stop`, and `logs`, `-C` can come from the CLI flag or `project_dir` in server config
- For `list`, omitting `-C` enables project discovery locally or on the remote host; adding `-C` targets exactly one project

## Remote Server Configuration

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

SSH-specific options (keys, jump hosts, tunnels, ports) belong in `~/.ssh/config` — cdeploy uses the system `ssh` binary and inherits its configuration.

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

Operation output is logged to `~/.cdeploy/logs/`. Each log file is named `cdeploy_on_{hostname}_{timestamp}.log`, so you get a per-host, timestamped record of deploy/restart/stop runs and TUI-triggered operations. Override the directory with `--log-dir`.

## Compose Config Screen

From the service screen, press `c` to open the compose config viewer/editor. This works for both local projects and remote servers selected through the TUI.

- `r` toggles between the raw compose file and resolved/interpolated `docker compose config` output
- `e` opens the compose file in your editor. Local mode uses `$EDITOR`, then `$VISUAL`, then `vi`; values like `code --wait` are supported. Remote mode runs `${EDITOR:-vi}` over SSH on the target host.
- After the editor exits, cdeploy reloads the raw file, switches back to raw view, and validates it with `docker compose config --quiet`. Validation errors are shown inline in the TUI.

## Why cdeploy?

cdeploy is not a replacement for Kubernetes, Docker Swarm, or full deployment platforms like Kamal. It's for teams and solo developers who deploy to a handful of servers with plain `docker compose` and just want a faster, less error-prone way to do it — without installing anything on the servers themselves.

## License

[MIT](LICENSE)

## Troubleshooting

- `neither 'docker compose' nor 'docker-compose' found`: install Docker Compose on the local machine or remote host you are targeting
- `no compose file found`: run from a project directory with a Compose file or pass `-C /path/to/project`
- `--server ... requires --project-dir or project_dir in config`: set `project_dir` in `~/.cdeploy/servers.yml` or pass `-C` explicitly
- SSH prompts or host key verification issues: test the same host with plain `ssh` first, because cdeploy uses the system SSH client and inherits `~/.ssh/config`
