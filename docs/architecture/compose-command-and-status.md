# The compose layer: command construction, and what `ps`/`stats` parse into

> Extracted verbatim from `CLAUDE.md`, which is auto-loaded and has a size limit.
> `CLAUDE.md` carries the rule digest; this file carries the full rationale, the
> failure modes it prevents, and the tests that pin them. Read it before you touch `internal/compose/compose.go`, `internal/compose/remote.go`, `internal/compose/stats.go`, `internal/compose/uptime.go`, `internal/compose/ports.go`, `internal/compose/svcagg.go` or the listing half of `cmd/list.go`.

Two threads run through this cluster, and every section below sits on one of them.

**The argv thread**: `Standalone` (set by `Detect`) → `command()` / `remoteCommand()` → `-f` pairs, then `-p <name>`, then the subcommand → `CURRENT_UID` and the project directory. Every compose invocation in the repo rides it. A handful of calls deliberately do NOT, because they are top-level `docker` commands rather than compose subcommands, and each of those builds its own argv — knowing which set a new call belongs to is the whole decision.

**The `ps` thread**: one `docker compose ps -a --format json` → `parseContainerStatus` → `svcAgg`, which folds the replicas of one service into one row → `runner.ServiceStatus{Running, Health, Created, Uptime, Ports}` → the CLI table and the TUI container screen, which render the same fields through the same helpers. `docker stats` is a SECOND call joined onto the same rows by container ID. Break the thread at the aggregator and a scaled service reads differently in the two views; break it at the port dedup and two real bind interfaces collapse into one.

## Every docker call goes through `command()` / `remoteCommand()` — except the top-level ones

**Rule: all docker interaction goes through `Compose.command()` (local) or `RemoteCompose.remoteCommand()` (SSH).** Both resolve the binary from the `Standalone` field: `exec.CommandContext("docker", "compose", …)` in plugin mode, `exec.CommandContext("docker-compose", …)` in standalone mode. On the remote side the same choice is a string — `"docker compose"` or `"docker-compose"` — because `buildRemoteCommand` assembles ONE shell command for ssh to run. Pins: `TestCommand_Standalone`, `TestCommand_Plugin`, `TestRemoteCommand_Standalone`, `TestRemoteCommand_Plugin`.

### `Detect()` must run before first use; `command()` only READS `Standalone`

`Detect(ctx)` probes `docker compose version` first, then falls back to `docker-compose version`, and caches the verdict in `Standalone` plus the unexported `detected` flag. It no-ops once `detected` is set.

**Detection is explicit.** `command()` and `remoteCommand()` never trigger it — they read `Standalone` and nothing else. A composer used without `Detect()` therefore silently assumes plugin mode, which on a standalone-only host fails every command with "docker: 'compose' is not a docker command". Every construction path in `cmd/` calls `Detect()` (or inherits a verdict) before handing the composer on: `prepareLocalComposer`, `resolveSSHRemote`, `resolveServerRemote`, and `runList`'s local multi-project branch.

**`SetStandalone(bool)` inherits an already-known verdict into a new instance** rather than re-probing. That is how the per-project factories build one composer per discovered project off a single detection: `localComposerFor` / `remoteComposerFor` (`cmd/root.go`) and both `runList` factories call it. On a host with twelve projects, re-probing would cost twelve extra round trips — twelve SSH round trips on a remote host.

**Both `Detect` implementations bypass their own `command()` builder**, and must: the probe is what DECIDES `Standalone`, so it cannot be built from it. The local probe is two plain `exec.CommandContext` calls. The remote probe builds its own ssh argv with `-o ControlMaster=no` so it skips the `CURRENT_UID` assignment and the `cd <projectDir>` prefix — the `cd` would fail the probe outright on a host where the project directory does not exist yet (see `docs/architecture/remote-and-servers-config.md`). Pins: `TestDetect_PluginFound`, `TestDetect_StandaloneFound`, `TestDetect_NeitherFound`, `TestDetect_CachesResult`, `TestSetStandalone`, `TestRemoteDetect_PluginFound`, `TestRemoteDetect_StandaloneFound`, `TestRemoteDetect_NeitherFound`, `TestRemoteDetect_CachesResult`, `TestRemoteDetect_SSHArgs`, `TestRemoteSetStandalone`.

### `CURRENT_UID`, and where the working directory comes from

Both transports set `CURRENT_UID`, and they compute it in deliberately different places.

- **Local**: `buildCommand` appends `CURRENT_UID=<uid>:<gid>` to `os.Environ()`, stamped from `os.Getuid()`/`os.Getgid()` at `New()` time. It also sets `cmd.Dir = c.ProjectDir` when the directory is non-empty.
- **Remote**: the shell string carries `CURRENT_UID=$(id -u):$(id -g)` — evaluated ON THE REMOTE HOST, because the local uid/gid is the wrong owner for a bind-mounted volume on the server. The directory becomes a `cd <projectDir> && …` prefix, again only when non-empty.

Pins: `TestCommand_Env`, `TestCommand_Dir`, `TestRemoteCommand_CURRENT_UID_InCommandString`, `TestRemoteCommand_WithoutProjectDir`, `TestNewRemote_NoLocalUID`.

### The compose subcommands, in full

| method | subcommand |
| --- | --- |
| `Stop` | `stop [containers…]` |
| `Remove` | `rm -f [containers…]` |
| `Pull` | `pull [containers…]` |
| `Create` | `up --no-start [containers…]` |
| `Start` | `start [containers…]` |
| `ListServices` | `config --services` |
| `ConfigResolved` | `config` |
| `ValidateConfig` | `config --quiet` |
| `ContainerStatus`, `Inspect`, `ContainerStatsFromBulk` | `ps -a --format json` |
| `Logs` | `logs [--follow] [--tail N] <service>` |
| `ExecCommand` | `exec <service> <command…>` |
| `ListProjects` | `ls -a --format json` (through `hostCommand`, see below) |

**An empty container slice means operate on ALL services.** Every write method is `append([]string{verb}, containers...)`, so a nil slice simply emits the bare verb, which is compose's own "all services" form. The CLI reaches it through `-a`: `runOperationWithPrep` sets `containers = nil` when `all` is true. It also **rejects `-a` combined with explicit container names** (`-a/--all cannot be combined with explicit container names`) rather than silently letting one win, and refuses an invocation with neither. That check runs AFTER `checkRemoteMutex` so a flag conflict reports the same error on every subcommand (`TestRunOperation_MutexBeforeContainerValidation`). Pins: `TestCommand_Args`, `TestStop_ArgsConstruction`, `TestStop_AllContainers`, `TestRemove_UsesForceFlag`, `TestCreate_UsesNoStartFlag`, `TestLogs_ArgsConstruction`, `TestRemoteCommand_AllComposerMethods`.

### `hostCommand` / `remoteHostCommand`: `ls` carries no `-f` and no `-p`

`docker compose ls` is host-wide discovery. Neither flag selects anything there, and `-f` is actively harmful: it points compose at a file it must PARSE, so host-wide discovery fails outright for a project whose files are not readable from the caller's context. `cdeploy list` and the TUI project loader both run `ListProjects` on a composer that may already carry both flags, which is exactly when the difference bites.

`Compose.hostCommand` and `RemoteCompose.remoteHostCommand` are the flagless twins of `command`/`remoteCommand`, and `ListProjects` is their only caller. Pin: `TestListProjects_CarriesNoProjectOrFileFlags` (local argv contains neither `-p` nor `-f`; the remote command string contains neither `-p ` nor `-f `), with `TestListProjects_Standalone` and `TestListProjects_Plugin` covering the binary choice.

### `SetTestHooks`

`SetTestHooks(run, output)` on both `Compose` and `RemoteCompose` injects test doubles for `cmd.Run()` and `cmd.Output()`. Nil hooks mean real exec. Every construction and parsing test in `internal/compose/` drives production code through these hooks, so the package needs no Docker daemon. `RemoteCompose` has a third, richer hook — `SetOutputErrHook(fn) (stdout, stderr, err)` — used only by `runRemoteDockerCmd`, so a test can drive `classifySSHError` with explicit stderr text instead of the `err.Error()` approximation.

## The top-level `docker` commands, and why each bypasses

These are `docker` commands, not `docker compose` subcommands. Routing one through `command()` would prepend `compose` (or switch to the `docker-compose` binary) and produce a malformed argv — `docker compose stats`, `docker compose inspect`.

| call | site | funnel |
| --- | --- | --- |
| `docker stats --no-stream --format json` | `AllContainerStats` / `AllContainerStatsRemote` (`stats.go`) | own `exec.CommandContext` / `rc.sshArgs` |
| `docker inspect <id>` | `Compose.Inspect` / `RemoteCompose.Inspect` | `runDockerCmd` / `runRemoteDockerCmd` |
| `docker inspect --format {{.Image}} <id>` | snapshot capture (`snapshot.go`, `remote.go`) | same |
| `docker image inspect --format …` | `imageInspectArgs` (`updates.go`), `imagePresenceArgs` (`snapshot.go`) | same |
| `docker buildx imagetools inspect` | `imagetoolsInspectArgs` (`updates.go`) | same |
| `docker manifest inspect --verbose` | `manifestInspectArgs` (`updates.go`) | same |
| `docker pull <digest-pinned ref>` | rollback prep (`snapshot.go`, `remote.go`) | `runDockerCmdStream` / `runRemoteDockerCmdStream` |
| `docker ps -a --size=false --format {{json .}}` | `hostPsArgs` (`hostcontainers.go`) | the 3-method `dockerRunner` seam |
| `docker stats --no-stream --format {{json .}}` | `hostStatsArgs` (`hostcontainers.go`) | same |

Four funnels carry all of them: `Compose.runDockerCmd` (capture, defined in `updates.go`) and `Compose.runDockerCmdStream` (stream, `snapshot.go`); `RemoteCompose.runRemoteDockerCmd` and `runRemoteDockerCmdStream` (`remote.go`). The remote pair renders its shell string through `remoteDockerCmdString`, the single home of the per-argument escaping, and every SSH argv comes out of `sshArgs()` so `SSHExtraArgs` land immediately before the host argument — see `docs/architecture/remote-and-servers-config.md`, which owns that splice rule.

**`AllContainerStats` predates the funnels and builds its own `exec.CommandContext("docker", "stats", …)` inline**, honouring the `outputCmd` hook directly. `AllContainerStatsRemote` builds its ssh argv through `rc.sshArgs([]string{"-S", rc.SocketPath, "-o", "ControlMaster=no"}, "docker stats --no-stream --format json")`.

**One correction worth carrying, because the old wording mis-stated it**: the bypass is in `AllContainerStats` / `AllContainerStatsRemote`, NOT in `ContainerStats()`. `ContainerStats()` delegates to the bulk helper and then to `ContainerStatsFromBulk`, which runs `ps -a --format json` through the ordinary `command()`/`remoteCommand()` path. And "first to bypass" is only true of `Compose`: `AllContainerStats` was the first DOCKER call on the local composer to skip `command()`, while on `RemoteCompose` the bypass was already routine — `Detect`, `ConnectCmd`, `Close`, `findRemoteComposeFile`, `ConfigFile`, `EditCommand` and `ExecCommand` all build their own ssh argv, for TTY and probe reasons rather than top-level-command reasons.

**The two `ExecCommand` implementations are deliberately asymmetric.** `Compose.ExecCommand` goes THROUGH `c.command(ctx, "exec", service, cmd…)`, so it inherits the `-f` pairs and `-p <name>` for free. `RemoteCompose.ExecCommand` needs `ssh -t` for TTY allocation and therefore assembles its own argv — which is why it must splice `composeFlags()` itself (next section). Both return the `*exec.Cmd` WITHOUT running it, so the caller owns execution: the TUI hands it to `tea.ExecProcess`, the CLI runs it with the real terminal attached. Pins: `TestExecCommand_DefaultShell`, `TestExecCommand_Standalone`, `TestExecCommand_Env`, `TestExecCommand_Dir`, `TestRemoteExecCommand_SSHArgs`, `TestRemoteExecCommand_DoesNotUseRemoteCommand`.

## The `-p` splice

`docker compose ls` reports a project launched with `docker compose -p` or `COMPOSE_PROJECT_NAME` under its REAL name, while several such projects can share one `ConfigDir`. A composer built from the directory ALONE therefore addresses the directory's DEFAULT project: the picker showed one project and `d`/`r`/`s` stopped and removed another's containers. `cdeploy list` had the same bug in text and in `--json` — identical services, status, stats and update verdicts printed under two different headers.

**`projectNameArgs(name)` (local) and `RemoteCompose.projectFlag()` (remote) are the only producers of the `-p <name>` pair.**

**Ordering: `-p` goes AFTER the `-f` pairs and before the subcommand.** Both are compose top-level flags, so either order works for compose — but keeping `-f` first leaves the `ExtraComposeFiles` splice rule (main file first, immediately after the compose binary) exactly as it was, and that rule is load-bearing for rollback. `Compose.command` composes them as `append(composeFileArgs(c.composeFileList()), projectNameArgs(c.ProjectName)...)`; `RemoteCompose.composeFlags()` renders the same order as a shell fragment, `-f` pairs then `-p`, each with a trailing space.

**On the remote side the name is shell-escaped** (`-p 'it'\''s blue'`), like every other argument that reaches the remote shell.

**`RemoteCompose.ExecCommand` must splice it too**, because it builds its own argv rather than going through `remoteCommand`. `composeFlags()` exists precisely so the two sites cannot disagree about which files and which project a command addresses; it is the single home of the ordering.

**An empty name must keep producing byte-identical argv.** `projectNameArgs("")` returns nil and `projectFlag()` returns `""`, so an unnamed composer emits exactly what it emitted before the field existed. This is not cosmetic: the whole feature shipped as a zero-behaviour-change default, and the pins compare full argv slices across the entire verb set. `TestCommand_ProjectName` has four subtests — `plugin`, `standalone`, `with extra compose files` (asserting `-f … -f … -p blue up --no-start`), and `empty name is byte-identical` over nine verb shapes. `TestRemoteCommand_ProjectName` mirrors it with `plugin`, `standalone with extra files`, `escaped`, `exec splices it too`, and `empty name is byte-identical` covering both `remoteCommand` and `ExecCommand`.

**Five sites stamp the name, and none of them looks it up.** `localComposerFor` / `remoteComposerFor` (`cmd/root.go`) stamp it from the TUI picker row's `compose.Project`; `prepareLocalComposer` / `resolveSSHRemote` / `resolveServerRemote` (`cmd/remote.go`) stamp it from the `--project-name` flag; `runList`'s two per-project factories stamp `proj.Name` from the discovered row, and its `--server` single-project branch stamps the flag. The reason none of them re-derives the name from `docker compose ls` is the storage-key argument in the **Project identity — caller-supplied, never looked up** paragraph of `CLAUDE.md`: that lookup enumerates projects by scanning CONTAINER LABELS, and the deploy pipeline removes the containers. Pins: `TestLocalComposerFor`, `TestRemoteComposerFor`, `TestRunOperation_ProjectNameReachesTheComposer`, `TestProjectNameFlagRegistered`, `TestProjectNameDoesNotOverrideProjectDir`, `TestCollectMultiProject_TwoNamedProjectsInOneDir`.

`cdeploy list` without `-C` refuses `--project-name` outright (`--project-name requires --project-dir (list without -C discovers every project)`): host-wide discovery builds one composer per project, each already carrying its own name, so there is nothing left for the flag to select. Refusing beats silently ignoring a flag the user spelled to narrow the output.

## What one `ps -a --format json` parses into

### Two output shapes, one parser

`parseContainerStatus()` accepts BOTH forms, keyed on the first non-space byte: a JSON array (Docker Compose v2.21+, `strings.HasPrefix(s, "[")`) or NDJSON, one object per line (older versions). Empty output and `"[]"` both return a nil map, not an error. `parseStatsOutput()` and `parsePsIDToService()` follow the same convention deliberately, so a docker version bump never breaks one parser and not the others. Pins: `TestParseContainerStatus` (a large table including `JSON array format`, `JSON array empty`, `whitespace only`, `malformed JSON`), `TestParseStatsOutput_JSONArray`, `TestParseStatsOutput_Empty`.

Entries with an empty `Service` are skipped; the surviving services keep SOURCE order in an `order` slice so the map build is deterministic.

### `svcAgg` — the replica merge rules live in ONE type

`internal/compose/svcagg.go` holds the scaled-service merge rules, and it is shared by the per-project parser (`parseContainerStatus`) and the host-wide grouper (`groupHostContainers` in `hostcontainers.go`, via `mergeHostEntry`). **Two copies of these rules drifted apart once**; one type is what keeps a scaled service reading the same in the drilled and grouped views.

| field | rule across replicas |
| --- | --- |
| `Running` | OR — any running replica means running |
| `Health` | worst case, by `healthPriority`: `unhealthy` (3) > `starting` (2) > `healthy` (1) > `""` (0) |
| `Created` | the OLDEST replica, formatted `"2006-01-02 15:04"` |
| `Uptime` | the LONGEST-RUNNING replica |
| `Ports` | accumulated, then deduped + collapsed + sorted by `dedupAndSortPorts` |

**`Uptime` is picked by duration, not by `CreatedAt`.** A running replica always beats a restarting one (`longestFromRunning` is a separate flag, not a duration comparison), and among running replicas the longest uptime wins. `CreatedAt` is the wrong tiebreaker because it is OLDER after a restart while the actual uptime is SHORTER. A replica whose status is `Restarting …` contributes the literal `"restarting"` only when nothing else has claimed the slot. Pins: `TestParseContainerStatus` subtests `scaled service picks oldest created and longest uptime`, `scaled service some exited picks oldest running`, `scaled service restarted replica has shorter uptime despite older CreatedAt`, `running replica overrides restarting even with zero-duration uptime`, `scaled service mixed parseable and unparseable created at among running replicas`, `scaled service all running with unparseable created at uses longest uptime`.

`parseCreatedAt` parses Docker's `"2006-01-02 15:04:05 -0700 MST"` layout and returns `ok=false` on anything else; an unparseable or absent `CreatedAt` leaves `Created` empty rather than failing the parse (`unparseable created at`, `missing created at field`, `empty created at`).

### `ServiceStatus` is the row every front end renders

`runner.ServiceStatus` carries `Running bool`, `Health string` (`"healthy"` / `"unhealthy"` / `"starting"` / `""` for no healthcheck), `Created string`, `Uptime string`, `Ports []runner.Port`, and the tri-state `UpdateAvailable *bool` (owned by `docs/architecture/update-detection.md`).

Both front ends render the same fields with the same glyphs: health icons `♥` / `✗` / `~` beside the running/stopped dot, then the Created and Uptime columns. The CLI does it in `healthIcon` (`cmd/list.go`); the TUI in `internal/tui/app.go`. A service with no healthcheck gets a blank icon cell in both, not a placeholder.

### `formatUptime` and `parseUptimeDuration`

`formatUptime()` (`internal/compose/uptime.go`) turns Docker's `Status` text into the compact form the columns show:

1. `Restarting …` → `"restarting"`, checked FIRST.
2. Anything not prefixed `"Up "` → `""`. Exited, Created, Paused and Dead statuses have no uptime.
3. Strip the trailing health annotation — `(healthy)`, `(unhealthy)`, `(health: starting)` — via `healthSuffixRe`. **That regex is shared with `parseHealthFromStatus` (`hostcontainers.go`), which reads its capture group**, so the grammar cannot drift between the two readers.
4. Special-case the textual durations: `"About a minute"` → `"~1m"`, `"About an hour"` → `"~1h"`, `"Less than a second"` → `"<1s"`.
5. Otherwise compact the units through `compactDuration`: `seconds`→`s`, `minutes`→`m`, `hours`→`h`, `days`→`d`, `weeks`→`w`, `months`→`mo`, singular and plural, multi-token (`"3 hours 15 minutes"` → `"3h 15m"`). An unrecognised remainder falls through as trimmed raw text rather than being dropped.

`parseUptimeDuration()` is the inverse, and exists only so `svcAgg` can COMPARE two compact strings. It returns 0 for `""` and for `"restarting"`, and `time.Millisecond` for anything non-empty it cannot parse — including `"<1s"` — so a barely-started container still beats a restarting one in the comparison. Pins: `TestFormatUptime`, `TestStripHealthSuffix`, `TestCompactDuration`, `TestParseUptimeDuration`.

### Ports: `Publishers[]` first, the `Ports` text as fallback

`extractPorts(entry)` reads the structured `Publishers []` array (Compose v2). `parseContainerStatus` falls back to `parsePortsString(entry.Ports)` — the comma-separated text field older Compose emits, e.g. `0.0.0.0:8080->80/tcp, :::8080->80/tcp` — **only when the structured pass yielded ZERO ports and the text field is non-empty**. Note the condition is on the RESULT, not on `len(Publishers)`: an array whose every entry is `expose:`-only also falls through, which is harmless because such a `Ports` text has no `->` and is skipped anyway.

**`PublishedPort == 0` entries are skipped.** Those are `expose:`-only / internal ports with no host binding; rendering them would show a `0→80` cell for a port nobody can reach.

**An empty `URL` is normalized to `0.0.0.0`** (Compose's default for an unspecified bind), and **bracketed IPv6 like `[::]:8080` has its brackets stripped on input**. Both go through the one `normalizeHost` helper, shared by `extractPorts` and `splitHostPort`, so the structured and text paths cannot disagree about what a host string looks like.

`parsePortsString` additionally handles shapes the structured array never produces:

- Entries with no `->` (a bare `80/tcp` exposed-only line) are skipped.
- Malformed entries are skipped SILENTLY, best-effort — a single bad token must not blank the whole column.
- Bare-IPv6 host forms are recognised by counting colons: `::8080` (Compose's unspecified shorthand, where `strings.LastIndex` would otherwise leave `host = ":"`), `::1:8443`, `2001:db8::1:8080`.
- **Port RANGES are expanded 1:1**: `0.0.0.0:8080-8090->8080-8090/tcp` becomes eleven `runner.Port` values. Mismatched range widths cause the entry to be skipped, and `maxPortRangeExpansion = 1024` caps a single range so a malformed `1-65535` cannot allocate an unbounded slice.

Pins: `TestExtractPorts`, `TestParsePortsString`, `TestParseContainerStatus_PortsAggregation` (including `Publishers preferred over Ports text when both present` and `older Compose fallback — Ports text only, Publishers nil`).

### Dedup on the FULL tuple; collapse only the wildcard mirror

These are two passes, and merging them would be a bug.

**`mergePort` dedupes on the full `portKey{Host, HostPort, ContainerPort, Protocol}` tuple.** Host is IN the key on purpose: `127.0.0.1:8080->80` and `192.168.1.10:8080->80` are two different user-visible binds, and collapsing them would hide the LAN-vs-loopback distinction that is the whole reason to publish on an explicit address. Only an EXACT repeat is dropped, and insertion order is preserved.

**`collapseIPv6Mirrors` then drops the `::` entry when a `0.0.0.0` sibling exists on the same `(HostPort, ContainerPort, Protocol)` tuple** — and nothing else. That pair is the dual-stack mirror Compose emits for a single all-interfaces publish, so showing both would double every ordinary row. Everything else survives:

- IPv6 loopback `::1`, link-local `fe80::1`, and explicit addresses like `2001:db8::1` survive **even alongside an IPv4 sibling on the same tuple** — they are not mirrors, they are distinct binds.
- Multiple non-wildcard IPv6 entries on the same tuple do NOT collapse against each other; `mergePort`'s full-tuple dedup already removed the exact repeats, and anything left is a real distinct host.
- The direction is fixed: the `::` entry is dropped in favour of `0.0.0.0`, regardless of which arrived first (`IPv6-first then IPv4 still collapses to IPv4`).

`dedupAndSortPorts` runs the same two passes over the ACCUMULATED replica ports in `svcAgg.status()`, then sorts ascending by `HostPort`, then `ContainerPort`, then `Protocol`, then `Host`. Three ephemeral host ports across three replicas therefore render as three distinct sorted cells, while three identical publishers render as one. Pins: `TestDedupAndSortPorts`, and the subtests named `two distinct IPv4 bind interfaces on same port both preserved`, `IPv6 loopback plus IPv4 wildcard: both survive (loopback is not a mirror)`, `IPv6 wildcard plus IPv6 loopback (no IPv4): both survive`, `two distinct non-wildcard IPv6 binds on same tuple: both survive`, `different protocols are not mirrors`, `scaled service ipv4 and ipv6 mirrors collapse across replicas`.

### Rendering: `FormatPort` / `FormatPorts`

`internal/compose/ports.go` owns the display form, and it is the single source for both front ends.

- The arrow is **U+2192 `→`**, not the ASCII `->` that Docker's own text field uses (`TestFormatPort_ArrowIsExactRune` compares the exact rune).
- An IPv6 host — defined as **any host containing a colon** — is wrapped in brackets, so the `host:port` boundary stays readable: `[::1]:8443→443`.
- The `/proto` suffix appears only when `Protocol` is non-empty AND not `tcp`. `/udp` and `/sctp` show; the overwhelmingly common `tcp` does not.
- **Wildcard hosts (`0.0.0.0` and `::`, both meaning "all interfaces") are omitted entirely**, so the common case renders as a bare `8080→80`. Any non-wildcard bind keeps its host.

**The wildcard hiding is DISPLAY ONLY.** `cmd/list.go` serialises `runner.Port` directly into its `--json` output, and the struct's `host` field keeps the original string — a consumer piping through `jq` still sees `"0.0.0.0"`. `runner.Port`'s JSON tags live on the type for exactly this reason; the other `runner` types are wire-format-agnostic. Pins: the twelve `TestFormatPort_*` cases, `TestFormatPorts_EmptySlice`, `TestFormatPorts_MultiPortJoin`, `TestFormatPorts_MixedProtocols`, `TestFormatJSON_IncludesPorts`.

`FormatPorts()` comma-joins and returns `""` for an empty slice.

**Both front ends width-track the column and place it LAST.** The CLI (`formatDots` / `formatDotsGrouped`) tracks `maxPorts` and appends the column after Created, Uptime, CPU and Mem — so with `--stats` off it lands right after Uptime, and with `--stats` on it lands after Mem. The TUI container screen adds a `"Ports"` caption through `hasStatusColumns()`, which returns true when any service in any group has a non-empty `Ports` slice (among its other triggers — see `docs/architecture/tui-refresh-loop.md` for the full predicate and the header-row budget it costs).

**A blank Ports cell means NO PORTS, not "stopped".** Neither renderer gates the cell on `Running`, unlike `formatCPUCell` / `formatMemCell`, which do. A stopped container whose `ps -a` entry still carries a `Publishers` array renders its ports — pinned by `stopped replica with non-empty Publishers — ports still surfaced`, against `stopped container with no Publishers — empty Ports`. The cell is padded whitespace when empty, so column alignment holds either way. Pins: `TestFormatDots_PortsColumn_Mixed`, `TestFormatDots_NoPorts_NoColumn`, `TestFormatDots_PortsColumn_FlattenedMultiProject`, `TestFormatDotsGrouped_PerProjectPortsWidth`.

CLI JSON emits a `ports` array per service with `host` / `host_port` / `container_port` / `protocol`, `omitempty` so a service with none keeps the pre-Ports wire shape.

## `docker stats` and the per-service join

### `runner.ServiceStats` and `parseStatsOutput`

`runner.ServiceStats` is `CPUPercent float64`, `MemoryUsed int64`, `MemoryLimit int64`, populated from host-wide `docker stats --no-stream --format json` — one line per container. `parseStatsOutput()` (`internal/compose/stats.go`) tolerates NDJSON and JSON-array alike, mirroring `parseContainerStatus`. Entries with an empty `ID` are skipped silently; a malformed `CPUPerc` or `MemUsage` is an ERROR that fails the whole parse, because a silently-zeroed CPU column is worse than a visible failure. The result map is keyed by SHORT container ID. Pins: `TestParseStatsOutput`, `TestParseStatsOutput_Malformed`, `TestParseStatsOutput_MalformedArray`, `TestParseStatsOutput_SkipsEmptyID`, `TestParseStatsOutput_PropagatesCPUError`, `TestParseStatsOutput_PropagatesMemError`.

### The short-ID join and the replica sums

`ContainerStats()` runs in this order: **`AllContainerStats` FIRST** (the host-wide `docker stats`), then `ContainerStatsFromBulk`, which runs `docker compose ps -a --format json` for this project and joins.

The join key is the SHORT container ID — `shortContainerID` truncates to 12 characters — because `docker stats` emits short IDs while `docker compose ps` emits full ones. `parsePsIDToService` returns a SLICE of `(shortID, service)` pairs, not a map, so every replica of a scaled service contributes; a map would silently drop the duplicate service key (`TestContainerStats_local_shortIDJoin`).

`aggregateStatsByService` **SUMS `CPUPercent`, `MemoryUsed` and `MemoryLimit` across replicas**, matching the "how much is this service costing me" intuition users budget against: three replicas each saturating a core report ~300% CPU. Pairs whose ID is absent from the stats map are skipped silently — that covers both the expected case (a stopped service never appears in `docker stats`) and the race where a container exits between the two calls. **Only running containers land in the result map; stopped services are absent from it**, which is what makes the front ends' blank CPU/Mem cells correct. Pins: `TestContainerStats_local_singleReplica`, `TestContainerStats_local_scaledService`, `TestContainerStats_local_stoppedServicesAbsent`, `TestContainerStats_local_psIDAbsentFromStats`, `TestContainerStats_local_psFailureReturnsError`, `TestContainerStats_local_statsFailureReturnsError`, `TestContainerStats_remote_passthrough`.

### `parseSize`, `parseCPUPercent`, `FormatBytes` — one source of truth

All three live in `internal/compose/stats.go`, next to each other so the parse/render round trip stays colocated.

- **`parseSize`** handles IEC binary (`B`, `KiB`, `MiB`, `GiB`, `TiB`) and SI decimal (`kB`/`KB`, `MB`, `GB`, `TB`) suffixes, unit letters case-insensitive, because Docker emits either depending on engine settings. A bare number with no suffix is bytes; an unknown suffix is an error.
- **`parseCPUPercent`** strips the trailing `%`; empty input is 0, non-numeric is an error.
- **`FormatBytes`** is EXPORTED and produces the compact `124M` / `1.5G` form: values under 1 KiB render as `<n>B`, larger values pick the largest unit whose leading number is < 1024, and a single decimal appears only when the value is under 10 of that unit AND not an exact multiple (so `1.5G` but `124M`, never `124.0M`).

Both `cmd/list.go` and `internal/tui/app.go` import `FormatBytes` from here — the CLI through `formatMemCell`, the TUI through its own cell builder — so the two tables cannot round differently. Pins: `TestParseSize`, `TestParseCPUPercent`, `TestFormatBytes`.

### Drilled fetches in PARALLEL, grouped fetches strictly SERIAL

**In DRILLED mode the TUI batches the status and stats halves together** — `statusRefreshCmd()` and `statsRefreshCmd()` in one `tea.Batch`, on every entry to `screenSelectContainers` and after every operation completes (return from progress / logs / exec).

**In GROUPED mode the two halves are strictly SERIAL.** `statsRefreshCmd()` returns nil there, and `groupedStatsCmd` is CHAINED off the grouped `servicesMsg` arrival, because it consumes the container listing that arrival carries rather than resolving a second host-wide seam. The mechanics of the chain, the `refreshInFlight` guard around it and the 5-second tick that drives both live in `docs/architecture/tui-refresh-loop.md` and `docs/architecture/tui-multi-project.md`; do not restate them here.

## Multi-project discovery in `cdeploy list`

**With no `-C`, `list` auto-discovers every compose project on the host — locally and over `--server` alike — and prints services grouped under a project header.** With `-C`, only that project's services are shown, in a flat list. `--ssh` always implies a single project, because `resolveSSHRemote` requires `--project-dir`.

**Per-project errors are NON-FATAL.** A failing `ListServices` or `ContainerStatus` prints `warning: skipping project %q: <err>` to stderr and the loop continues; one unreadable project must not cost the user the other eleven. A project whose `ConfigDir` is EMPTY is skipped the same way, with `warning: skipping project %q: no compose file reported for it` — docker reported no compose file for it, so the factory would hand back a composer rooted at cdeploy's own working directory and every service IT listed would print under THIS project's header.

**Stats failures are strictly additive and never drop a project**: they warn (`cdeploy: stats unavailable for %q: <err>`), blank that project's cells, and continue. Update failures behave identically (`cdeploy: updates unavailable for %q: <err>`).

### One host-wide `docker stats`, shared across every project

`runList` calls `compose.AllContainerStats` / `AllContainerStatsRemote` ONCE before the project loop and passes the resulting map into `collectMultiProjectStats`. Each per-project composer satisfies the `bulkStatsAggregator` interface (`ContainerStatsFromBulk`) and runs only `docker compose ps -a --format json` for itself, joining against the shared map. The host pays the single ~1.5s `docker stats` cost regardless of project count — the alternative was N× that, which on a remote SSH hop with a dozen projects is the difference between a usable command and an unusable one.

**On a failed host-wide fetch, `runList` warns ONCE and passes a NON-NIL EMPTY map** — never nil. That is the contract: `collectMultiProjectStats` takes the bulk path whenever `bulkStats != nil`, so an empty map means "the host-wide fetch failed, do NOT retry it per project". Passing nil there would make every project fall back to `ContainerStats()`, which re-runs the host-wide `docker stats` once PER PROJECT — turning one failed 1.5s call into twelve. Pin: `TestCollectMultiProjectStats_EmptyBulkSkipsPerProjectRetry` (asserts `ContainerStats` is called zero times and `ContainerStatsFromBulk` once).

**`bulkStats == nil` is reserved for callers that never requested bulk sharing at all** — `collectMultiProject`'s non-stats wrapper, and test mocks that do not implement `bulkStatsAggregator`. Those fall back to per-project `ContainerStats()` (`TestCollectMultiProjectStats_FallsBackWhenBulkNil`, `TestCollectMultiProjectStats_UsesBulkAggregator`).

### The factory takes the whole `compose.Project`

`listComposerFactory` takes `compose.Project`, not a directory string, mirroring `tui.ComposerFactory`: a directory does not identify a project. Each factory stamps `ProjectName` from the row and `ComposeFiles` from `PinComposeFilesLocal` / `PinComposeFiles`, then inherits the detection verdict with `SetStandalone`. The remote factory also copies `SSHExtraArgs` from the live connection — a composer built without them dials a different endpoint than the connection it was derived from. Pins: `TestCollectMultiProject_Success`, `TestCollectMultiProject_SkipsFailedProject`, `TestCollectMultiProjectStats_SkipsProjectWithNoConfigDir`, `TestCollectMultiProject_TwoNamedProjectsInOneDir`, `TestRunList_LocalMultiProject`, `TestRunList_ServerMultiProject`.
