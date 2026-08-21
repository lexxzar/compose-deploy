---
name: cdeploy
description: Drive the cdeploy CLI to manage Docker Compose deployments on the local host or a remote server over SSH. Use for operations — deploy, restart, stop, or roll back compose services; health-gate a deploy or restart with --wait; list service status, health, ports, CPU/memory stats, or check for available image updates; stream or tail service logs; exec into a running container. Also use for setup and configuration — installing the cdeploy binary, creating or editing ~/.cdeploy/servers.yml (remote servers, groups, and badge colors), setting up key-based SSH access, and ad-hoc --ssh / --identity / --project-dir connections for CI. Triggers include "deploy my app", "restart the nginx container", "roll back the last deploy", "wait until the services are healthy", "check the servers", "are there any image updates", "show me the logs", "set up cdeploy", "add a server to cdeploy", "configure servers.yml".
---

# cdeploy

## 1. What cdeploy is

cdeploy is a dual-mode Docker Compose deploy/restart/stop tool. Running it with no
arguments launches an interactive TUI that **requires a real TTY** — never invoke
the bare `cdeploy` command as an agent, it will hang or error. Always use the
non-interactive CLI subcommands below (`deploy`, `restart`, `stop`, `list`,
`logs`, `exec`), which work locally or against a remote server over SSH.

## 2. Setup & configuration

**Check it is installed:** `cdeploy --version` (prints version, commit, build date).
If missing, install one of these ways:

- Homebrew (if a tap is available): `brew install lexxzar/tap/cdeploy`
- Go toolchain: `go install github.com/lexxzar/compose-deploy@latest` (binary is `compose-deploy`; the docs call it `cdeploy` — rename or symlink if you want the short name)
- Prebuilt binary from the project's GitHub Releases page

**Remote server config — `~/.cdeploy/servers.yml`.** Optional; only needed for the
`-s/--server` shorthand. Ad-hoc `--ssh` (below) needs no config file. Full schema:

```yaml
groups:                 # optional; shared badge color for a set of servers
  - name: production
    color: red          # optional
servers:
  - name: prod          # required, unique
    host: user@prod.example.com   # required; SSH destination, [user@]host
    project_dir: /opt/myapp        # optional; default compose dir on the server
    group: production              # optional; must reference an existing group
    color: green                   # optional; ignored when group is set (group color wins)
  - name: staging
    host: deploy@staging.example.com
    project_dir: /srv/app
```

Valid `color` values (groups and servers): **red, green, yellow, blue, magenta,
cyan, white, gray**. Any other value is rejected.

**Editing the file directly is fine** — cdeploy loads and validates it on startup
(`Load()` then `Validate()`). Validation enforces: every server has a non-empty
`name` and `host`; server names are unique; group names are unique; colors are
from the list above; a server's `group` must match a declared group. A malformed
file makes commands exit with a parse/validation error, so verify with a quick
`cdeploy list -s <name>` after editing. To create the file from scratch: make
`~/.cdeploy/`, write `servers.yml` with the schema above.

**SSH prerequisites.** cdeploy shells out to `ssh` and reuses a ControlMaster
socket. Set up **key-based auth** so no password prompt is needed
(`ssh-copy-id user@host`, or an entry in `~/.ssh/config`). Advanced SSH options
(jump hosts, tunnels, per-host keys) belong in `~/.ssh/config` — cdeploy honors
it automatically. The one exception is `-i/--identity` for CI (below).

**Ad-hoc / CI connections (no config file).** Instead of `-s`, connect directly:

```
cdeploy list  --ssh user@host:2222 --project-dir /opt/myapp
cdeploy deploy web --ssh user@host --project-dir /opt/myapp --identity /run/secrets/deploy_key
```

- `--ssh [user@]host[:port]` — ad-hoc target. Mutually exclusive with `--server`.
  IPv6 literals are not supported. Requires `--project-dir`.
- `--identity <path>` / `-i` — path to a private key. **Only** valid with `--ssh`
  (for CI that writes a key from a secret). Rejected with `--server`.
- `--project-dir <dir>` / `-C` — compose project directory (required with `--ssh`;
  optional otherwise, defaults to the current directory).

## 3. Read-only operations (safe, no confirmation needed)

These never change container state — run them freely to gather facts.

**`cdeploy list`** — service status. Prefer `--json` for machine parsing.

```
cdeploy list --json                      # local, all discovered projects
cdeploy list -C /opt/myapp --json        # one local project
cdeploy list -s prod --json              # remote, all projects on the server
cdeploy list -s prod -C /opt/myapp --json
cdeploy list --ssh user@host --project-dir /opt/myapp --json
cdeploy list -s prod --json --stats      # add CPU/memory (adds ~1.5s)
cdeploy list -s prod --json --updates    # add image-update check (1 registry probe per service)
```

Flags: `--json` (JSON output), `--stats` (CPU/mem columns, off by default because
of latency), `--updates` (per-service image-update check, off by default — one
registry round-trip per service; 20+ services over SSH can add 10s+).
`-s/--server`, `-C/--project-dir`, `--ssh`, `--identity` as above. With **no** `-C`
it discovers **all** compose projects and groups by project; with `-C` it lists
just that one.

**Parsing `list --json`.** Array of service objects. Key fields:

- `service` (string), `project` (string, only in multi-project output)
- `running` (bool)
- `health` — one of `healthy`, `unhealthy`, `starting`, or absent/`""` (no healthcheck)
- `created` (formatted time), `uptime` (compact, e.g. `3h`, `2d`)
- `ports` — array of `{host, host_port, container_port, protocol}` (omitted when none)
- With `--stats`: `cpu_percent` (float), `memory_used`, `memory_limit` (bytes).
  Absent for stopped services.
- With `--updates`: `update_available` is **tri-state** — `true` = a newer image
  exists in the registry, `false` = current, **absent/omitted** = unknown (not
  checked, build-only image, or a per-image error). Do not treat absent as `false`.

**`cdeploy logs <service>`** — stream logs. Follows by default.

```
cdeploy logs nginx                       # follow (Ctrl+C to stop)
cdeploy logs nginx -n 100 --no-follow    # dump last 100 lines and exit
cdeploy logs web -s prod -C /opt/myapp -n 200 --no-follow
```

Flags: `-n/--tail <N>` (historical lines, default 50), `--no-follow` (dump and
exit — **use this when scripting**, otherwise the follow stream never returns).

**Stale-image sweep workflow** (which services need redeploying):

1. `cdeploy list -s prod --json --updates` (or local / `--ssh`).
2. Parse the array; collect every service where `update_available == true`.
3. Report that exact list to the user. If the check failed, note it instead of
   claiming everything is current (see Troubleshooting).
4. Only then, and only with confirmation, deploy those specific services (§4).

## 4. Mutating operations — SAFETY PROTOCOL

`deploy`, `restart`, `stop` change running containers. Follow this every time:

1. **Confirm first.** Before running any mutating command, restate to the user
   the exact operation, the exact service names, and the target (local, or which
   server / `--ssh` host + `--project-dir`). Get explicit confirmation. Do not
   deploy/restart/stop on your own initiative.
2. **Never use `-a/--all` unless the user explicitly said "all".** Always pass the
   specific service names they named. `-a` cannot be combined with explicit names.
3. **Verify after.** Re-run `cdeploy list ... --json` and confirm the affected
   services are `running` (and `health` is `healthy` where applicable).
4. **On failure, gather logs.** If a command errors, tail the failing service:
   `cdeploy logs <service> ... -n 200 --no-follow`, and surface the logfile path
   printed on failure (`For details see logfile: …`).

Commands (name the services explicitly):

```
cdeploy deploy  nginx postgres            # stop → remove → pull → create → start
cdeploy restart nginx postgres            # stop → remove → create → start (no pull)
cdeploy stop    nginx postgres            # stop only
cdeploy deploy  web -s prod -C /opt/myapp
cdeploy deploy  web --ssh user@host --project-dir /opt/myapp --identity /run/secrets/key
```

- **deploy** pulls fresh images then recreates — use for "ship the new image".
- **restart** recreates with the current image (no pull) — use for "bounce it".
- **stop** just stops. `-a/--all` operates on every service (needs explicit "all").

**Health-gated deploys (`--wait`).** Add `--wait` to `deploy` or `restart` to make
cdeploy poll container health after the pipeline and print a per-service verdict
table. Prefer it for CI / unattended deploys — it turns "deployed but unhealthy"
into a distinct exit code.

```
cdeploy deploy web --wait                       # wait up to 2m (default)
cdeploy deploy -a --wait --wait-timeout 90s
cdeploy restart web -s prod -C /opt/myapp --wait
```

- **Exit codes (deploy/restart/rollback):** `0` = success, `1` = a pipeline step
  failed, **`2` = the pipeline finished but the `--wait` health gate failed**
  (a service went unhealthy / exited / restart-looped / timed out). Treat `2` as
  "deployed but not healthy", not as a pipeline error. A failed deploy wait also
  prints `run 'cdeploy rollback' to restore the previous images`.
- Verdicts: `healthy`, `running (no healthcheck)` (ran past a 10s grace window),
  `unhealthy`, `exited`, `exited (never started)`, `restarting`,
  `timed out (still starting)`.
- **Caveat:** `--wait` assumes targeted services are long-running. A one-shot
  run-to-completion service (a migration/seed job that exits after finishing) in
  the target set will fail the wait as `exited`. Don't `--wait` on those.

**Rollback (`cdeploy rollback [services...|-a]`).** Every `deploy` snapshots the
image digest each running container uses to a state file on the docker host
**before** stopping anything. `rollback` re-creates the targeted services pinned
to those digests via a generated compose override (stop → remove → create →
start — **no pull**), so it works even with the registry unreachable as long as
the old image blob is still on the host. Use it to undo a bad deploy.

```
cdeploy rollback web                            # restore one service
cdeploy rollback -a --wait                       # restore all + health-gate
cdeploy rollback web -s prod -C /opt/myapp
```

- Follow the same SAFETY PROTOCOL as deploy/restart: confirm the exact services
  and target first, never assume `-a`, verify with `list` after.
- Rollback **refuses clearly** (does not guess) when no snapshot exists for the
  project or a named service is absent from it — the error names what's missing.
  Report that to the user; it usually means the service was never deployed with
  this cdeploy on that host.
- **Restores images only** against the *current* compose file — env/ports/volumes
  drift is not rewound. It changes which image runs, not the whole config.

**`cdeploy exec <service> [-- command...]`** opens an interactive shell in a
running container (tries bash, falls back to sh) or runs a one-off command. It is
interactive by design — only run it when the user actually wants a shell/command,
not for automated inspection (prefer `logs` / `list` for that).

```
cdeploy exec web                          # interactive shell
cdeploy exec db -- psql -U postgres       # one-off command
cdeploy exec web -s prod -C /opt/myapp
```

## 5. Troubleshooting

Read the message and act — do not silently retry the same command.

- **`registry unreachable: <err>`** (from `list --updates`) — cdeploy could not
  reach the image registry from the host doing the check (DNS failure, connection
  refused, TLS/x509 error, rate limit). The update check is aborted and all update
  cells are blank. Report that updates could **not** be determined; do not claim
  images are current. Retry later or check network/registry credentials.
- **`remote update check transport failure: <err>`** — the SSH transport to the
  remote host broke mid-check (auth failure, host unreachable, ControlMaster
  socket lost). This is an SSH problem, not a registry one. Verify key-based auth
  and connectivity (`ssh user@host`) before retrying.
- **`cdeploy: updates unavailable: <err>`** (on stderr; also `... unavailable for
  "<project>": <err>` in multi-project mode) — a soft failure. The listing still
  printed, but `update_available` cells are blank. Treat updates as unknown, not
  current.
- **`cdeploy: stats unavailable: <err>`** (stderr) — `--stats` fetch failed; the
  rest of the listing is valid, only CPU/mem cells are blank.
- **Compose variant detection failure** ("docker compose"/"docker-compose" not
  found) — Docker or the compose plugin is not installed/reachable on the target
  host. For remote targets confirm Docker is installed and the SSH user can run it.
- **`no compose file found in <dir>`** — no `docker-compose.yml` in the working
  directory. Pass `-C/--project-dir` (or `-s`/`--ssh`) to point at the right place.
- **config parse/validation error** — `~/.cdeploy/servers.yml` is malformed or
  breaks a rule from §2 (missing name/host, duplicate name, unknown color,
  server referencing an undeclared group). Fix the file and retry.
