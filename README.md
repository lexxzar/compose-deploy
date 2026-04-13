# cdeploy

A TUI/CLI tool for managing Docker Compose container deployments and restarts — locally or on remote servers via SSH.

Replaces manual `docker compose stop && docker compose rm -f && docker compose pull && docker compose up --no-start && docker compose start` workflows with a single command or interactive interface.

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

The TUI walks through up to five screens:

1. **Server select** — choose a remote server or "Local" (only shown when servers are configured)
2. **Project select** — pick a Docker Compose project (auto-skipped if the current directory has a compose file)
3. **Service select** — pick services and choose an action (`r` restart, `d` deploy, `s` stop, `l` logs)
4. **Progress** — watch step-by-step execution with status indicators
5. **Logs** — live-stream logs for the selected service

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
-C, --project-dir string   Docker compose project directory (default: current directory)
    --log-dir string       Log directory (default ~/.cdeploy/logs/)
```

## Remote Server Configuration

Define remote servers in `~/.cdeploy/servers.yml`:

```yaml
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

| Field | Required | Description |
|-------|----------|-------------|
| `name` | yes | Identifier used in TUI and `--server` flag |
| `host` | yes | SSH destination (`user@hostname`) |
| `project_dir` | no | Default project directory on the remote host |
| `group` | no | Visual group label in the TUI server picker — servers with the same group are displayed together under a shared header |

SSH-specific options (keys, jump hosts, tunnels, ports) belong in `~/.ssh/config` — cdeploy uses the system `ssh` binary and inherits its configuration.

### How it works

cdeploy uses SSH ControlMaster multiplexing:

1. A persistent control socket is established once (password/key prompts happen here)
2. All subsequent docker compose commands reuse the socket with zero auth overhead
3. The socket is torn down on disconnect or TUI quit

In TUI mode, the SSH connect command runs with full terminal access so interactive prompts (passwords, host key verification) work naturally.

## Operations

| Operation | Steps |
|-----------|-------|
| **Deploy** | stop → remove → pull → create → start |
| **Restart** | stop → remove → create → start |
| **Stop** | stop |

## Logging

All docker compose output is logged to `~/.cdeploy/logs/cdeploy_on_{hostname}_{timestamp}.log`. Override with `--log-dir`.

## Requirements

- Go 1.26+
- Docker with Compose v2 plugin (`docker compose`)
- SSH client (for remote server support)
