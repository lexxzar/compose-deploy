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

The TUI has seven main screens, plus an inline settings editor reachable from screen 1:

1. **Server select** — choose a remote server or "Local" (only shown when servers are configured); press `s` to open the settings editor for managing servers
2. **Project select** — pick a Docker Compose project (auto-skipped if the current directory has a compose file). When the host also runs containers that no compose project owns, an extra `(unmanaged)` row appears at the end of the list with the container count — see [Unmanaged containers](#unmanaged-containers-read-only)
3. **Service select** — pick services (`space` toggles one, `a` toggles all) and choose an action (`r` restart, `d` deploy, `s` stop, `R` rollback, `l` logs, `i` inspect, `c` config, `x` exec, `U` re-check updates); press `/` to search-and-jump to a service by name substring and `n`/`N` to cycle through matches (search moves the cursor and highlights matches without filtering the list or touching your selection); also shows CPU% and Mem (used/limit) columns for running services, refreshed on screen entry and after every operation. Services whose registry image is newer than the local copy get a yellow `⇧` marker next to the service name; the indicator is cached for 10 minutes and `U` forces a refresh. `R` reads the host-side deploy snapshot and, when one exists, asks to confirm a digest-pinned rollback of the selected services (the prompt shows how long ago the snapshot was recorded). The footer shows only the most-used keys; press `?` for the full key list for the current screen. On the `(unmanaged)` row this screen is read-only — see [Unmanaged containers](#unmanaged-containers-read-only).
4. **Progress** — watch step-by-step execution with status indicators. After a deploy, restart, or rollback the screen enters a **health-wait** sub-state: it polls each targeted service and shows a live per-service verdict (`♥` healthy, `●` running with no healthcheck, `✗` failed, `~` pending) with a countdown to the timeout. Press `esc` to skip the wait (the operation stays "done"). A failed deploy wait shows the hint `press R on the services screen to roll back`.
5. **Logs** — live-stream logs for the selected service. `w` toggles soft-wrap, `p` toggles JSON pretty-print, and scrolling up pauses the auto-follow (`G` jumps back to the live tail). Press `f` to open a live **filter** (a grep that hides non-matching lines while the stream keeps buffering underneath; a leading `!` excludes matching lines) and `/` to open a **search** that highlights and jumps within the (possibly filtered) view (`n`/`N` cycle through matches). Both use case-insensitive substring matching by default; `ctrl+r` toggles Go regular-expression (RE2) mode. `esc` peels back one layer at a time — closing an open search or filter input, then clearing a committed search, then a committed filter, and finally leaving the screen.
6. **Config** — inspect or edit the compose file, toggle between raw and resolved config, and see validation status
7. **Inspect** — read-only view of what the container under the cursor actually holds: state, health (including the last healthcheck probe output), image id, mounts and environment (plus the update verdict, and which image an available update would move it to), with `r` toggling to the raw `docker inspect` JSON — see [Container Inspect Screen](#container-inspect-screen)

#### Navigation

| Key | Action |
|-----|--------|
| `esc` | Back to the previous screen; cancels confirmation prompts and in-flight operations |
| `q` | Back on nested screens (alias for `esc`); quits on root screens |
| `ctrl+c` | Quit. On remote sessions, prompts to confirm the disconnect (`y` to quit, `n`/`esc` to cancel) |
| `?` | Show the key list for the current screen; `?`, `esc` or `q` closes it |

`q` is typeable inside settings-form text inputs and is a no-op on the progress screen while an operation is in flight — use `esc` to cancel.

`?` is typeable inside the container search bar, the log filter and search bars, and settings-form text inputs, so a regular expression such as `(?:web|db)` reaches the input instead of opening the key list. `?` does nothing while a confirmation prompt is armed. On a very narrow or very short terminal the key list is trimmed to fit and ends with a `▼ N more` marker — resize the window to see the rest.

#### Unmanaged containers (read-only)

Containers started outside Docker Compose — a `docker run` postgres, a watchtower, a monitoring agent — carry no `com.docker.compose.project` label and belong to no project. The project picker gathers them into one synthetic row:

```
cdeploy > prod > select project

> my-app        /srv/my-app
  other-stack   /srv/other
  (unmanaged)   3 containers
```

Select it to get the usual service screen over those containers: status dot, health icon, Created, Uptime, Ports, CPU, Mem, and the `⇧` update indicator, in the same columns as a compose project. This works against a remote server through the existing SSH connection, so you can inspect hand-started containers on production without installing anything there and without opening a second SSH session.

**Reaching the picker.** The row lives on the project-select screen, so you need to get there. `cdeploy` shows the picker when the current directory has no compose file, or when servers are configured (pick the server, or `Local`, and the picker opens). It skips the picker when you run `cdeploy` from inside a project directory with no servers configured — that session goes straight to the compose project's services and has no way back to the picker. Run `cdeploy` from a directory with no compose file, or pass `-C` a directory with none, to get the picker in that case.

**The list is a snapshot.** The row count is taken when the project list loads, and the container list is fetched once when you select the row. Neither refreshes while you stay on the screen: a container started by hand while the screen is open does not appear, and one that was removed keeps its row with a stopped dot. Status, health, ports and CPU/Mem for the containers that ARE listed do refresh every 5 seconds. To resync the list, go back to the server picker and select the server (or `Local`) again, which reloads it; a local session with no configured servers has no server picker, so restart the TUI.

**The screen is read-only.** A container with no compose file cannot be deployed, rolled back, or shown a config, so those keys are not merely refused — they are absent from the footer and from the `?` key list, and the rows carry no selection checkbox:

| Key | On the unmanaged screen |
|-----|-------------------------|
| `l` `i` `x` `U` | Work as usual — logs, inspect, exec, force an update check |
| `/` `n` `N` `esc` `q` `?` arrows | Work as usual — search, navigate, back, key list |
| `d` `r` `s` `R` `c` `space` `a` | Inert and unadvertised — deploy, restart, stop, rollback, config, and multi-select need a compose project |

**Update checks are opt-in here.** The `⇧` glyph does not appear on its own on this screen — press `U`. A compose project bounds the check to its own service list, but the unmanaged list comes from `docker ps -a`, so every distinct image on the host would cost a registry manifest request on every visit; that can exhaust an anonymous registry rate limit and break a later `docker pull` from the same host. `cdeploy list --updates` is opt-in for the same reason.

`i` matters most here: a hand-started container has no compose file, so [inspect](#container-inspect-screen) is the only way to see its environment, mounts, image id and healthcheck output.

To start, stop, or replace an unmanaged container, use the docker CLI on the host. `cdeploy list` covers compose projects only — unmanaged containers are a TUI view.

### CLI Mode

```bash
# Deploy specific containers (stop → remove → pull → create → start)
cdeploy deploy nginx postgres

# Deploy all containers
cdeploy deploy -a

# Deploy and wait for services to become healthy (exit 2 if any fail)
cdeploy deploy -a --wait
cdeploy deploy web --wait --wait-timeout 90s

# Restart specific containers (stop → remove → create → start)
cdeploy restart nginx

# Restart all containers
cdeploy restart -a

# Roll services back to the previously-deployed image digests
cdeploy rollback web
cdeploy rollback -a --wait

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

# Show image-update indicators (opt-in; one registry probe per service)
cdeploy list --updates
cdeploy list -C /opt/myapp --updates
cdeploy list -s prod --updates --json

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

A yellow `⇧` glyph next to a service name in `cdeploy list` and the TUI service-select screen means the image in the registry has a different digest than the locally pulled copy. The check runs `docker compose config --format json` to map services to images, then per-image `docker image inspect` (local RepoDigest) and `docker buildx imagetools inspect` (registry manifest-list digest), falling back to `docker manifest inspect --verbose` when the buildx plugin is unavailable. Build-only services and services whose digest cannot be determined render a blank cell — the indicator is tri-state (unknown / current / update available). On the unmanaged-container screen there is no compose file, so the name-to-image map comes from the `docker ps` output that screen already fetched; the per-image digest comparison is identical, and a container running an untagged image (no repository to ask a registry about) renders the blank cell.

`--updates` is opt-in in both single- and multi-project modes — each service costs one registry round-trip (buildx/manifest-inspect), and projects with many services (especially over SSH) can take 10+ seconds. Omit the flag for fast `cdeploy list` invocations; add it when you actually want to know what's behind.

Failures are non-fatal: `cdeploy: updates unavailable: <err>` (single-project) or `cdeploy: updates unavailable for "<project>": <err>` (multi-project) is written to stderr, the cell stays blank, exit code 0. JSON output adds `update_available: true|false` with `omitempty`; existing JSON consumers see the original wire shape when the flag is absent.

In the TUI, the indicator is cached for 10 minutes per (project, server) context; the unmanaged view gets its own slot rather than sharing the local one. Press `U` on the service-select screen to bypass the cache and re-check immediately. On the unmanaged screen the check never runs on its own — `U` is the only trigger, so the glyphs there go blank when the 10-minute entry expires and come back on the next `U` (see [Unmanaged containers](#unmanaged-containers-read-only)).

**Detail rows on the inspect screen (TUI only).** When a service's verdict is `true`, the TUI fetches a little more about that image so the [inspect screen](#container-inspect-screen)'s IMAGE section can say *which* image is waiting and *when* each side was built: one local `docker image inspect` for the build time and platform, then `docker buildx imagetools inspect` three more times — `--format '{{json .Manifest}}'` to pick the host's platform out of a multi-arch index, `--raw` for the config digest (which is the image ID the image gets once pulled), and `--format '{{json .Image}}'` for its build time. That is three extra registry round-trips per *updated* image, on top of the one the check itself makes. It rides the same cache entry, the same 10-minute TTL, the same `U` key and the same post-deploy invalidation as the glyph, and it runs only for services that actually came back `true` — usually a small minority. `cdeploy list --updates` does not pay this cost; the CLI has no detail view to render it in. A failure here is silent: the `⇧` glyph is unaffected and the extra rows are simply omitted, and any value the parsers cannot fully trust is omitted rather than guessed. On a 32-bit ARM host the index usually carries two entries for `linux/arm` (`v5`+`v7`, or `v6`+`v7`) and the local probe cannot say which variant the host runs, so these rows are omitted rather than guessed; `arm64` and `amd64` carry one entry each and are unaffected. These rows also need the buildx plugin (Docker v23+) — the `docker manifest inspect` fallback the check itself uses does not apply here, so on an older host the glyph still works and the rows never appear. The verdicts are delivered as soon as the check returns, so the `⇧` glyphs paint at their usual speed and these rows arrive a moment later. If you hit `429 Too Many Requests` from Docker Hub, `docker login` on the docker host raises the anonymous quota; a detail-only failure is cached alongside the successful verdicts for the full 10 minutes, so press `U` to retry it sooner.

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
| **Rollback** | stop → remove → create → start (no pull; pinned to the snapshot digests) |
| **Stop** | stop |

## Health-gated deploys (`--wait`)

`deploy`, `restart`, and `rollback` accept `--wait`: after the pipeline finishes, cdeploy polls container health until every targeted service is healthy (or has run continuously past a short grace window when it has no healthcheck), then prints a per-service verdict table.

```bash
cdeploy deploy -a --wait                    # wait up to 2m (default)
cdeploy deploy web --wait --wait-timeout 90s
cdeploy restart -a --wait
cdeploy rollback -a --wait
```

Flags: `--wait` (enable the health gate), `--wait-timeout <dur>` (default `2m`). The poll interval and no-healthcheck grace window are fixed internally (2s / 10s). `--wait-timeout` is a firm deadline: a service that only becomes healthy *after* it elapses is reported as timed out, and the wait never runs more than the timeout (the final poll is scheduled to land at the deadline rather than a poll-interval past it).

Each service resolves to one verdict:

- **healthy** — a healthcheck passed.
- **running (no healthcheck)** — no healthcheck defined, but the container ran continuously past the 10s grace window.
- **unhealthy** — a healthcheck reported unhealthy (fail fast).
- **exited** — the container was running and then stopped (fail fast).
- **exited (never started)** — the container never came up (fail fast after a short debounce).
- **restarting** — the container is stuck in a restart loop (fail fast after 3 consecutive observations).
- **timed out (still starting)** — still not resolved when `--wait-timeout` elapsed.

**Exit codes** (deploy / restart / rollback):

| Code | Meaning |
|------|---------|
| `0` | Success (all services passed, or `--wait` not used and the pipeline finished) |
| `1` | A pipeline step failed (stop/remove/pull/create/start) |
| `2` | The pipeline finished but the `--wait` health gate failed (a service went unhealthy/exited/looped/timed out) |

Exit `2` lets CI tell "deployed but unhealthy" apart from "pipeline broke midway". A failed deploy wait also prints the hint `run 'cdeploy rollback' to restore the previous images`. Automation that treats any non-zero exit as "pipeline failed" should learn to distinguish code `2`.

> **Caveat — long-running services only.** `--wait` assumes every targeted service is long-running. A run-to-completion service in the target set (a migration or seed job that starts, does its work, and exits) is indistinguishable from a crash and will fail the wait as `exited` — `docker compose ps` carries no exit code to tell them apart. Don't `--wait` on one-shot services.

## Rollback

Every `cdeploy deploy` snapshots the image digest each *running* container actually uses to a per-project state file **on the docker host**, before it stops anything. `cdeploy rollback` re-creates the targeted services pinned to those digests via a generated compose override (stop → remove → create → start — **no pull**), so it restores the previous images even when the registry is unreachable, as long as the old image blob is still on the host. The override also forces `pull_policy: never` on each rolled-back service so the create step can't attempt a registry pull even when the main compose file sets `pull_policy: always`.

```bash
# Roll back a single service to its snapshot digest
cdeploy rollback web

# Roll back everything in the snapshot, then wait for health
cdeploy rollback -a --wait

# Roll back on a remote server / ad-hoc host
cdeploy rollback web -s prod -C /opt/myapp
cdeploy rollback web --ssh deploy@host -C /opt/myapp
```

**How the snapshot works:**

- Recorded on **deploy only** — never on restart or rollback, so a bad deploy followed by a restart can't overwrite the good snapshot, and rolling back twice lands on the same state.
- Captured **before** the pipeline stops anything, from the digest each running container is actually using (not the local tag's current digest — a pulled-but-not-deployed newer image never poisons the snapshot).
- **Merge-not-replace**: `deploy web` updates only `web`'s entry and keeps the rest of the project's services in the snapshot, each with its own `recorded_at`.
- Stored on the docker host at `~/.cdeploy/state/<hash-of-project-dir>.json` (`sha256(project dir)`, first 12 hex chars), so a CI deploy and a laptop rollback share one authoritative history.
- Snapshot failure **warns but never blocks the deploy** — a missing digest (a locally-built image with no registry digest) or a not-running service is skipped with a warning, the deploy proceeds.

**Rollback refuses (rather than guessing) when** no snapshot exists for the project, the state file is unreadable or has an unknown schema, or a named service is absent from the snapshot (the error names exactly which services are missing). If the snapshot digest's blob is no longer cached on the host, rollback pulls it by digest first; if that pull fails (blob pruned *and* registry down), it aborts before touching any container.

**Only services still in the compose file are touched.** Rollback intersects the snapshot with the current `docker compose config --services`: a snapshot entry for a service that has since been removed from the compose file is skipped with a warning under `-a`, or refused with a clear error when named explicitly — the generated override never resurrects a removed service.

> **Caveat — images only.** Rollback pins **images only** against the *current* compose file. Other config drift (changed env, ports, volumes) is not rewound. It restores which image runs, not the whole compose configuration.

> **Caveat — concurrent deploys of the same project.** The per-project state file is written with a read-modify-merge. Locally, an advisory file lock serializes concurrent deploys of the same project so their snapshots can't clobber each other. For a **remote** host, two deploys of the same project running at the same time from different machines can race — the merge is host-local between two SSH round-trips and there is no cross-host lock in v1 — so the later write can drop the earlier deploy's fresh entry for an overlapping service. Snapshotting is best-effort, so this only affects rollback precision for that narrow overlap.

## Health Checks

If your services define Docker health checks, cdeploy displays their status alongside the running/stopped indicator:

- **♥** healthy
- **✗** unhealthy
- **~** starting (health check hasn't passed yet)

For scaled services, the worst-case health is shown (unhealthy > starting > healthy). Services without a health check show only the running/stopped dot. The same icons appear in `cdeploy list` output.

To see **why** a container is `✗` unhealthy, press `i` on the service screen: the [inspect screen](#container-inspect-screen) shows the last healthcheck probe's own output, which is the one place cdeploy surfaces it.

## Logging

All docker compose output is logged to `~/.cdeploy/logs/`. Each log file is named `cdeploy_on_{hostname}_{timestamp}.log`, so you get a per-host, timestamped record of every operation. Override the directory with `--log-dir`.

## Compose Config Screen

From the service screen, press `c` to open the compose config viewer/editor. This works for both local projects and remote servers selected through the TUI.

- `r` toggles between the raw compose file and resolved/interpolated `docker compose config` output
- `e` opens the compose file in your editor. Local mode uses `$EDITOR`, then `$VISUAL`, then `vi`; values like `code --wait` are supported. Remote mode runs `${EDITOR:-vi}` over SSH on the target host.
- After the editor exits, cdeploy reloads the raw file, switches back to raw view, and validates it with `docker compose config --quiet`. Validation errors are shown inline in the TUI.

## Container Inspect Screen

From the service screen, press `i` to inspect the container under the cursor. Where `c` shows what the compose file *declares*, inspect shows what the running container actually *holds* — the two disagree whenever a compose file is edited and the service is restarted (`r`) instead of deployed (`d`).

The screen opens on a curated summary with five sections:

- **STATE** — the container name, status, exit code (stopped containers only), docker's own start error, OOM kill, start time, restart policy and restart count. The container name matters on a scaled service: it names which replica you are looking at.
- **HEALTH** — health status, failing streak, the healthcheck definition, and **the last probe's output**, soft-wrapped rather than truncated. This is the answer to "why is this `✗` unhealthy?" and it appears nowhere else in cdeploy. The section is omitted for a container with no healthcheck.
- **IMAGE** — the configured image reference, the image id docker resolved it to, and the command and entrypoint. One extra row, `update`, appears for any service the [update check](#update-available-indicators---updates) reached — `available` or `up to date`, with how old the check behind it is. The other three need an actual update, because they are fetched only for a `true` verdict: `built` (when the local copy of that image was built — the tag, not strictly the running container, so a `docker pull` without a redeploy makes them describe different images), `update id` (the image id the new image will have once pulled — directly comparable with the `image id` row above) and `update built`. A value the parsers cannot trust omits its own row — images built reproducibly report `1970-01-01`, which cdeploy treats as no answer rather than a date — but a *failed* registry call omits `built`, `update id` and `update built` together, since the fetch stops at the first failure. Only `update` survives that, because it needs no fetch beyond the check itself
- **MOUNTS** — type, `source → destination`, and the read-write flag
- **ENV** — the container's environment, one `KEY=VALUE` per line, unmasked

The summary is curated, not complete — networks, labels, resource limits and published ports are not in it (ports have their own column on the service screen). Press `r` for the raw JSON, which carries everything `docker inspect` returns.

Keys: `r` toggles between the summary and the raw `docker inspect` JSON (byte-identical to what the docker CLI prints, apart from the control bytes named below), the arrows and `pgup`/`pgdown` scroll, `←`/`→` scroll sideways through the long lines raw mode does not wrap, and `esc` or `q` returns to the service screen. Both modes are read-only — inspect changes no container state.

**The screen is a snapshot.** The data is read once, when you press `i`; it does not auto-refresh. To re-read a container whose health is still changing, press `esc` and `i` again. The update rows are the one exception: they are read from the update-check cache rather than fetched here, so they appear on their own if a check that was already in flight finishes while you are on the screen.

Inspect works on remote servers selected through the TUI, over the existing SSH connection. It costs two round-trips there — one `docker compose ps` to resolve the container ID, one `docker inspect` for the payload itself, which is typically 10-50KB.

> **Environment values are shown as-is, secrets included.** `docker inspect` prints `POSTGRES_PASSWORD`, `DATABASE_URL=postgres://user:pass@host/db` and API tokens in cleartext, and cdeploy masks nothing in either mode (the summary drops trailing spaces from a line so the pane has no ragged padding, expands tabs to spaces and strips terminal escape sequences and control bytes, which an image can set in an env value to rewrite your terminal; raw mode keeps every byte docker's JSON escaped — an escaped `\u001b` is text, not an escape sequence — and drops only the control bytes JSON leaves raw: DEL and the 8-bit C1 introducers U+0080-U+009F, which a terminal reads as CSI/OSC). This matches lazydocker, k9s and Docker Desktop. The consequence is real: pressing `i` while you share a screen or record a terminal exposes those values to everyone watching. The output is held in memory only — it is not written to `~/.cdeploy/logs/`.

For a scaled service, cdeploy inspects the same replica the Uptime column shows — the longest-running one, and always a running replica over a restarting one. A service with no container reports `no container found for "<service>"` instead of a blank screen; a fetch that fails leaves neither buffer, so the footer drops the `r` token rather than offering a toggle to an empty pane.

Inspect also works on the read-only [unmanaged containers](#unmanaged-containers-read-only) screen, where `c` does not exist. It is a TUI-only view; there is no `cdeploy inspect` subcommand.

## AI agent integration

cdeploy ships an embedded [Agent Skill](https://agentskills.io) (`SKILL.md`) that teaches AI coding agents — Claude Code, plus Codex/Gemini/Amp via the shared `.agents/skills` convention (Cursor and OpenCode read Claude's directories) — how to install, configure, and drive cdeploy safely. The skill content is version-locked to the binary (embedded at build time), so it never drifts from the CLI you have. Install it into your agent's skill directory:

```bash
# Install for Claude Code (dirs also read by Cursor/OpenCode)
cdeploy skill install claude

# Install for Codex/Gemini/Amp (~/.agents/skills + $CODEX_HOME/skills, default ~/.codex)
cdeploy skill install codex

# Install everywhere, deduplicated
cdeploy skill install all
```

Restart your agent (or run `/skills` in Claude Code) afterwards so it picks up the new skill.

**Other verbs:**

```bash
# Print the raw skill to stdout (inspect it, or redirect it somewhere)
cdeploy skill show

# Place it in a repo for project-level, version-controlled distribution
cdeploy skill show > .claude/skills/cdeploy/SKILL.md

# Remove the skill again
cdeploy skill uninstall all
```

Installed files carry a content-hash stamp so cdeploy knows what it owns: a file you edited by hand, or one placed by another installer, is never overwritten or removed without `--force`. `install` is idempotent — an unchanged file is reported as such, an out-of-date one is refreshed, and each destination succeeds or fails independently (non-zero exit if any failed).

**External channels:** because `skills/cdeploy/SKILL.md` lives at the repo root on the default branch, the skill is also installable through the community tooling without cdeploy itself:

```bash
npx skills add lexxzar/compose-deploy
gh skill install lexxzar/compose-deploy
```

**What the skill teaches:** setting up `~/.cdeploy/servers.yml` and key-based SSH from scratch (servers, groups, badge colors, ad-hoc `--ssh`/`--identity`/`--project-dir` for CI); read-only inspection (`list --json` with `--stats`/`--updates`, `logs` with tail, and the stale-image sweep for spotting containers running behind the registry); and a safety protocol for mutating operations — restate the target services and server and confirm before deploying/restarting/stopping/rolling back, never assume `-a`, and verify with `list` afterwards.

## License

[MIT](LICENSE)