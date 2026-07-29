package compose

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Deploy snapshots and rollback state.
//
// Every `deploy` records the image digest each *running* container actually
// uses to a per-project state file on the docker host, so `rollback` can
// re-create services pinned to the previously-running digests via a generated
// compose override. This file holds the schema and the pure (no-IO) helpers;
// the capture + state-IO methods live on Compose / RemoteCompose.

// snapshotSchemaVersion is the only schema version this build understands.
// An on-disk snapshot with any other value is refused (never guessed) so a
// future format bump can't be silently misread by an older binary.
const snapshotSchemaVersion = 1

// errSnapshotSchema is returned by parseSnapshot when the on-disk snapshot has
// an unknown schema version. It is distinguishable from a malformed-JSON error
// and from a not-found (missing file) condition so callers can refuse clearly.
var errSnapshotSchema = errors.New("unsupported snapshot schema")

// errSnapshotCorrupt wraps a malformed-JSON parse failure so WriteSnapshot can
// tell "the existing file is garbage, safe to overwrite" apart from a
// future-schema file (errSnapshotSchema) or a transient read/transport failure —
// the latter two must NOT clobber the existing merge history.
var errSnapshotCorrupt = errors.New("corrupt snapshot")

// SnapshotEntry records the digest of the image a single service's running
// container used at snapshot time. recorded_at is per-service because merge
// keeps older entries alive across partial deploys (see mergeSnapshot).
type SnapshotEntry struct {
	Image      string `json:"image"`
	Digest     string `json:"digest"`
	RecordedAt string `json:"recorded_at"`
}

// Snapshot is the on-disk state-file shape (schema 1). Services is keyed by
// compose service name.
type Snapshot struct {
	Schema     int                      `json:"schema"`
	ProjectDir string                   `json:"project_dir"`
	Services   map[string]SnapshotEntry `json:"services"`
}

// snapshotKey derives the 12-hex state-file key from a project directory.
// projectDir MUST already be normalized by the caller (localProjectDir for a
// local host, remoteProjectDir for a remote host) so that equivalent path
// spellings — `./myapp` resolved against cwd vs an explicit absolute path, or
// a trailing slash — collapse to a single key. The hash is taken verbatim.
func snapshotKey(projectDir string) string {
	sum := sha256.Sum256([]byte(projectDir))
	return hex.EncodeToString(sum[:])[:12]
}

// localProjectDir normalizes a LOCAL project directory for keying. filepath.Abs
// resolves a relative path against the current working directory and Clean
// removes redundant separators and any trailing slash, so `-C ./myapp` and the
// equivalent absolute path yield one key. On the (rare) Abs failure it falls
// back to Clean so keying is still stable within a single spelling.
func localProjectDir(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return filepath.Clean(dir)
}

// remoteProjectDir normalizes a REMOTE project directory for keying using POSIX
// path semantics (every supported remote host is POSIX). path.Clean collapses
// redundant separators and strips a trailing slash. filepath.Abs is
// deliberately NOT used: it would resolve against the LOCAL cwd, which is
// meaningless for a path that lives on another host. An empty dir is returned
// verbatim (the caller decides what an unset project dir means).
func remoteProjectDir(dir string) string {
	if dir == "" {
		return dir
	}
	return path.Clean(dir)
}

// stateFileRelPath returns the state-file path relative to the host home
// directory (`.cdeploy/state/<key>.json`). projectDir MUST already be
// normalized (see snapshotKey). POSIX "/" separators are used unconditionally
// because the state lives under the docker host's home directory, which is
// POSIX for every supported target.
func stateFileRelPath(projectDir string) string {
	return path.Join(".cdeploy", "state", snapshotKey(projectDir)+".json")
}

// mergeSnapshot combines an existing on-disk snapshot with a freshly-captured
// one, per-service, fresh entries winning. This is merge-not-replace so a
// partial deploy (`deploy web`) never destroys the rest of the project's
// safety net: services absent from fresh keep their older entry (and their own
// recorded_at). schema and project_dir are refreshed from fresh. Either side
// may be nil (nil existing = first write; nil fresh = defensive no-op).
func mergeSnapshot(existing, fresh *Snapshot) *Snapshot {
	merged := &Snapshot{
		Schema:   snapshotSchemaVersion,
		Services: map[string]SnapshotEntry{},
	}
	if existing != nil {
		merged.ProjectDir = existing.ProjectDir
		for name, entry := range existing.Services {
			merged.Services[name] = entry
		}
	}
	if fresh != nil {
		if fresh.ProjectDir != "" {
			merged.ProjectDir = fresh.ProjectDir
		}
		for name, entry := range fresh.Services {
			merged.Services[name] = entry
		}
	}
	return merged
}

// parseSnapshot decodes a state-file payload strictly. Malformed JSON yields a
// wrapped error; an unknown schema yields errSnapshotSchema (via errors.Is) so
// the caller can refuse clearly rather than guess. A nil Services map is
// normalized to an empty map so callers never nil-panic on lookup.
func parseSnapshot(data []byte) (*Snapshot, error) {
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("%w: %v", errSnapshotCorrupt, err)
	}
	if snap.Schema != snapshotSchemaVersion {
		return nil, fmt.Errorf("%w: %d", errSnapshotSchema, snap.Schema)
	}
	if snap.Services == nil {
		snap.Services = map[string]SnapshotEntry{}
	}
	return &snap, nil
}

// buildOverrideYAML renders a minimal compose override pinning each requested
// service to its snapshot digest (`image: <repo>@<digest>`) AND forcing
// `pull_policy: never`. The repo is derived from the recorded image reference
// via stripTag so the tag is dropped and the digest ref is unambiguous.
//
// pull_policy: never is load-bearing for offline rollback (AC4). The rollback
// pipeline has NO Pull step, but its Create step is the shared
// `docker compose up --no-start`, whose pull behavior is policy-driven. Because
// this override is stacked as the SECOND `-f` file, its `pull_policy: never`
// wins the compose merge over whatever the MAIN compose file declares (e.g.
// `pull_policy: always`), so Create never attempts a registry pull — which
// would fail during a registry outage, exactly the classic rollback trigger.
// This is safe because PrepareRollback already presence-checks and pulls the
// pinned digest blob to the docker host BEFORE the pipeline runs, so the local
// blob is guaranteed present at Create time.
//
// Services are emitted in SORTED order, built from a sorted slice — never by
// marshaling a map, whose iteration order is random and would make the
// generated file (and its argv) non-deterministic. Only services present in
// entries are emitted; a requested service missing from entries is skipped (the
// caller validates completeness before pinning).
func buildOverrideYAML(entries map[string]SnapshotEntry, services []string) []byte {
	names := make([]string, len(services))
	copy(names, services)
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("services:\n")
	for _, name := range names {
		entry, ok := entries[name]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "  %s:\n", name)
		fmt.Fprintf(&b, "    image: %s\n", rollbackImageRef(entry))
		b.WriteString("    pull_policy: never\n")
	}
	return []byte(b.String())
}

// rollbackImageRef returns the digest-pinned image reference (`repo@digest`) a
// snapshot entry rolls back to: the repo is derived from the recorded image ref
// via stripTag (dropping any tag) joined to the recorded digest. It is the
// single source of truth for the ref used in the local/remote presence check,
// the pull-by-digest, AND the generated override YAML, so all three always
// agree on the exact string.
func rollbackImageRef(entry SnapshotEntry) string {
	return stripTag(entry.Image) + "@" + entry.Digest
}

// rollbackTargets resolves the sorted set of services a rollback targets: the
// explicit services slice when non-empty, otherwise every service present in
// entries. Sorting makes the pull order, override generation, and
// ExtraComposeFiles argv deterministic (Go map iteration is randomized).
func rollbackTargets(entries map[string]SnapshotEntry, services []string) []string {
	var targets []string
	if len(services) > 0 {
		targets = append(targets, services...)
	} else {
		for name := range entries {
			targets = append(targets, name)
		}
	}
	sort.Strings(targets)
	return targets
}

// imagePresenceArgs builds a minimal `docker image inspect` argv used only to
// test whether an image (by digest-pinned ref) is already present locally. It
// formats just the image ID so the captured output stays small; a missing image
// makes `docker image inspect` exit non-zero, which the caller treats as
// "absent, must pull by digest". Bypasses command() — `docker image inspect`
// is a top-level docker CLI command, not a compose subcommand.
func imagePresenceArgs(ref string) []string {
	return []string{"image", "inspect", "--format", "{{.Id}}", ref}
}

// snapshotClock returns the timestamp stamped into freshly-captured snapshot
// entries (recorded_at). It is a package var so tests can pin it for
// deterministic output; production uses UTC wall-clock time. UTC keeps the
// recorded_at string stable regardless of the deploying machine's timezone.
var snapshotClock = func() time.Time { return time.Now().UTC() }

// SnapshotResult is the outcome of a capture: the fresh Snapshot plus any
// per-service warnings (service not running, image built locally with no
// registry digest, etc.). Warnings are non-fatal — the caller surfaces them but
// the snapshot of the remaining services is still recorded.
type SnapshotResult struct {
	Snapshot *Snapshot
	Warnings []string
}

// SnapshotServices captures the image digest each *running* container of the
// requested services actually uses, so a later rollback can pin those digests.
//
// Flow (all data-plane calls honor the outputCmd test hook):
//  1. `docker compose config --format json` → service → image ref (reused from
//     updates.go via fetchServiceImages; build-only services are absent).
//  2. `docker compose ps --format json` → the first RUNNING container ID per
//     service.
//  3. one batched `docker inspect --format '{{.Image}}' <ids...>` → the exact
//     image ID each running container was created from (NOT the tag's current
//     digest — a pulled-but-not-deployed newer image must not poison the
//     snapshot).
//  4. per image ID, `docker image inspect` → RepoDigests, filtered against the
//     compose-config image ref via parseLocalDigest.
//
// Steps 3–4 are top-level docker CLI commands, so they bypass c.command() via
// runDockerCmd (same rationale as AllContainerStats). A service that is not
// running, or whose image has no registry digest (locally built), is skipped
// with a warning rather than failing the whole capture. When services is empty
// the capture targets every service that has an image in the compose config.
func (c *Compose) SnapshotServices(ctx context.Context, services []string) (SnapshotResult, error) {
	images, err := c.fetchServiceImages(ctx)
	if err != nil {
		return SnapshotResult{}, err
	}
	running, err := c.runningContainerIDs(ctx)
	if err != nil {
		return SnapshotResult{}, err
	}

	targets := services
	if len(targets) == 0 {
		targets = sortedStringKeys(images)
	}

	// Batch a single `docker inspect` over the container IDs we actually need
	// (targets that both have an image ref AND a running replica).
	var toInspect []string
	seen := map[string]bool{}
	for _, svc := range targets {
		if _, ok := images[svc]; !ok {
			continue
		}
		cid, ok := running[svc]
		if !ok || seen[cid] {
			continue
		}
		seen[cid] = true
		toInspect = append(toInspect, cid)
	}
	imageIDs, err := c.inspectContainerImageIDs(ctx, toInspect)
	if err != nil {
		return SnapshotResult{}, err
	}

	snap := &Snapshot{
		Schema:     snapshotSchemaVersion,
		ProjectDir: localProjectDir(c.ProjectDir),
		Services:   map[string]SnapshotEntry{},
	}
	recordedAt := snapshotClock().Format(time.RFC3339)
	var warnings []string
	for _, svc := range targets {
		imageRef, hasImage := images[svc]
		if !hasImage {
			warnings = append(warnings, fmt.Sprintf("%s: no image in compose config (build-only?), skipped", svc))
			continue
		}
		cid, ok := running[svc]
		if !ok {
			warnings = append(warnings, fmt.Sprintf("%s: not running, skipped", svc))
			continue
		}
		out, derr := c.runDockerCmd(ctx, imageInspectArgs(imageIDs[cid]))
		if derr != nil {
			warnings = append(warnings, fmt.Sprintf("%s: inspecting image failed: %v, skipped", svc, derr))
			continue
		}
		digest := parseLocalDigest(string(out), imageRef)
		if digest == "" {
			warnings = append(warnings, fmt.Sprintf("%s: no repository digest (locally built?), skipped", svc))
			continue
		}
		snap.Services[svc] = SnapshotEntry{
			Image:      imageRef,
			Digest:     digest,
			RecordedAt: recordedAt,
		}
	}
	return SnapshotResult{Snapshot: snap, Warnings: warnings}, nil
}

// runningContainerIDs runs `docker compose ps --format json` and returns a map
// of service name → the FULL container ID of its first running replica.
// Services with no running replica are absent. Full IDs (not the short form)
// are returned because `docker inspect` needs the full ID. The first running
// replica wins for a scaled service: all replicas share the same image, so any
// running one yields the correct digest.
func (c *Compose) runningContainerIDs(ctx context.Context) (map[string]string, error) {
	cmd := c.command(ctx, "ps", "--format", "json")
	var out []byte
	var err error
	if c.outputCmd != nil {
		out, err = c.outputCmd(cmd)
	} else {
		out, err = cmd.Output()
	}
	if err != nil {
		return nil, fmt.Errorf("listing containers for snapshot: %w", withStderr(err))
	}
	return parseRunningContainerIDs(out)
}

// parseRunningContainerIDs parses `docker compose ps --format json` (array or
// NDJSON, mirroring parseContainerStatus) into a service → first-running
// container ID map. Entries with an empty service/ID or a non-running State are
// skipped.
func parseRunningContainerIDs(data []byte) (map[string]string, error) {
	s := strings.TrimSpace(string(data))
	if s == "" || s == "[]" {
		return map[string]string{}, nil
	}
	var entries []psEntry
	if strings.HasPrefix(s, "[") {
		if err := json.Unmarshal([]byte(s), &entries); err != nil {
			return nil, fmt.Errorf("parsing ps for snapshot: %w", err)
		}
	} else {
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var e psEntry
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				return nil, fmt.Errorf("parsing ps for snapshot: %w", err)
			}
			entries = append(entries, e)
		}
	}
	out := make(map[string]string)
	for _, e := range entries {
		if e.Service == "" || e.ID == "" || e.State != "running" {
			continue
		}
		if _, ok := out[e.Service]; !ok {
			out[e.Service] = e.ID
		}
	}
	return out, nil
}

// inspectContainerImageIDs runs a single batched
// `docker inspect --format '{{.Image}}' <ids...>` and returns container ID →
// image ID. Bypasses c.command() because `docker inspect` is a top-level docker
// CLI command, not a compose subcommand (same rationale as runDockerCmd's other
// callers). `docker inspect` preserves argument order in its output, so the
// image IDs are zipped back to the input container IDs positionally.
//
// If the batch fails or returns a mismatched line count — a container that was
// running at `ps` time vanished before `inspect` (docker then exits non-zero and
// drops that ID's line, making positional zipping unsafe) — it falls back to
// inspecting each container individually so a single disappearing container only
// drops its OWN entry instead of failing the whole capture. Containers still
// missing are simply absent from the returned map; SnapshotServices warns
// per-service for those. The returned error is reserved for the (rare) case where
// even the per-container path can't be attempted; a partial result is not an
// error, matching the best-effort, warn-and-proceed snapshot contract.
func (c *Compose) inspectContainerImageIDs(ctx context.Context, containerIDs []string) (map[string]string, error) {
	if len(containerIDs) == 0 {
		return map[string]string{}, nil
	}
	args := append([]string{"inspect", "--format", "{{.Image}}"}, containerIDs...)
	if out, err := c.runDockerCmd(ctx, args); err == nil {
		if lines := nonEmptyLines(string(out)); len(lines) == len(containerIDs) {
			m := make(map[string]string, len(containerIDs))
			for i, id := range containerIDs {
				m[id] = lines[i]
			}
			return m, nil
		}
	}
	// Batch errored or the count didn't line up: a container vanished (or another
	// per-id error). Re-inspect individually so survivors are still captured.
	m := make(map[string]string, len(containerIDs))
	for _, id := range containerIDs {
		out, err := c.runDockerCmd(ctx, []string{"inspect", "--format", "{{.Image}}", id})
		if err != nil {
			continue
		}
		if lines := nonEmptyLines(string(out)); len(lines) == 1 {
			m[id] = lines[0]
		}
	}
	return m, nil
}

// nonEmptyLines splits s on newlines and returns the trimmed, non-empty lines.
func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// sortedStringKeys returns the keys of m in ascending order so callers that
// iterate get deterministic output (Go map iteration is randomized).
func sortedStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// localStatePath returns the absolute path of this project's state file under
// $HOME/.cdeploy/state/. The key is derived from the normalized project dir so
// `-C ./app` and its absolute spelling share one file.
func (c *Compose) localStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	key := snapshotKey(localProjectDir(c.ProjectDir))
	return filepath.Join(home, ".cdeploy", "state", key+".json"), nil
}

// ReadSnapshot reads and parses this project's local state file. A missing file
// returns (nil, nil) — the normal first-deploy case, distinguishable from a
// parse/schema error (which is returned as a typed error via parseSnapshot).
func (c *Compose) ReadSnapshot(ctx context.Context) (*Snapshot, error) {
	path, err := c.localStatePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading snapshot: %w", err)
	}
	return parseSnapshot(data)
}

// existingForMerge decides how WriteSnapshot treats the result of reading the
// current on-disk snapshot. A missing file (nil, nil) or a malformed-JSON file
// (errSnapshotCorrupt) yields a nil existing so the fresh snapshot is written
// standalone — one garbage state file must not block recording a good one. But a
// future-schema file (errSnapshotSchema) or a transient read/transport failure is
// propagated so the write ABORTS rather than clobbering a state file we cannot
// safely interpret: otherwise an older binary would silently downgrade a
// newer-schema file, and a single flaky read would wipe the merge history of
// every service not in the current deploy (defeating merge-not-replace).
func existingForMerge(existing *Snapshot, err error) (*Snapshot, error) {
	if err == nil {
		return existing, nil
	}
	if errors.Is(err, errSnapshotCorrupt) {
		return nil, nil
	}
	return nil, err
}

// lockStateFile takes an exclusive advisory flock on a sidecar `<state>.lock`
// file, serializing concurrent LOCAL deploys of the SAME project around the
// whole read-modify-rename of the shared state file. Without it, two overlapping
// partial deploys could both read the same old file, merge different service
// sets, and the later rename would clobber the earlier deploy's fresh entry —
// defeating merge-not-replace. The returned unlock releases the flock and closes
// the descriptor. The lock is per-project (the state key is per-project) and
// blocks until acquired. The lock file is intentionally NEVER unlinked: removing
// it would race a waiter that already holds a descriptor to the old inode.
//
// Advisory and unix-only (syscall.Flock; this tool is unix-only per the SSH /
// ControlMaster design). Separate os.OpenFile calls yield independent open file
// descriptions, so the flock contends even between goroutines of one process.
// The remote path is deliberately NOT locked — a cross-host distributed lock is
// out of v1 scope (see RemoteCompose.WriteSnapshot's concurrency caveat).
func lockStateFile(statePath string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(statePath+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// WriteSnapshot merges fresh into the existing on-disk snapshot (if any) and
// writes the result atomically under $HOME/.cdeploy/state/. A missing or
// malformed-JSON existing file is treated as empty so one bad state file never
// blocks recording a fresh, good snapshot; a future-schema or transiently
// unreadable file instead ABORTS the write (see existingForMerge) so the merge
// history is preserved rather than clobbered. Not-found is normal (first deploy).
//
// An advisory flock (lockStateFile) serializes the read-modify-rename against
// concurrent local deploys of the same project so their merges can't clobber one
// another. It is best-effort: if the lock cannot be taken the write still
// proceeds unlocked — exactly the prior behavior for the common single-deploy
// case.
func (c *Compose) WriteSnapshot(ctx context.Context, fresh *Snapshot) error {
	path, err := c.localStatePath()
	if err != nil {
		return err
	}
	if unlock, lerr := lockStateFile(path); lerr == nil {
		defer unlock()
	}
	existing, err := existingForMerge(c.ReadSnapshot(ctx))
	if err != nil {
		return fmt.Errorf("refusing to overwrite snapshot state: %w", err)
	}
	merged := mergeSnapshot(existing, fresh)
	return writeSnapshotFile(path, merged)
}

// writeSnapshotFile writes snap as indented JSON to path atomically: a temp
// file in the same directory followed by a rename, so a crashed or interrupted
// write never leaves a half-written state file that parseSnapshot would later
// reject. Parent directories are created as needed. Mirrors config.Save.
func writeSnapshotFile(path string, snap *Snapshot) error {
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling snapshot: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".snapshot-*.json")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("writing snapshot: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("renaming snapshot: %w", err)
	}
	return nil
}

// runDockerCmdStream runs a top-level `docker <args...>` command STREAMING its
// combined stdout+stderr to w, bypassing c.command() (which is compose-
// specific). Unlike runDockerCmd — which CAPTURES output for parsing — this
// primitive exists so `docker pull <repo>@<digest>` shows live progress on the
// op log / progress screen during rollback prep. The runCmd test hook is
// honored so the argv is exercised without invoking Docker.
func (c *Compose) runDockerCmdStream(ctx context.Context, dockerArgs []string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, "docker", dockerArgs...)
	cmd.Stdout = w
	cmd.Stderr = w
	if c.runCmd != nil {
		return c.runCmd(cmd)
	}
	return cmd.Run()
}

// PrepareRollback readies a digest-pinned rollback for the requested services
// WITHOUT touching the running pipeline. For each target service present in
// entries it:
//
//  1. Ensures the snapshot digest's image blob is available locally: a
//     `docker image inspect <repo>@<digest>` presence check and — only when
//     absent — a `docker pull <repo>@<digest>` (streamed to w for live
//     progress). A pull that fails (blob pruned AND registry unreachable)
//     aborts here, BEFORE any override file is written or ExtraComposeFiles is
//     touched, so a failed prep never leaves the pipeline half-configured. This
//     is why offline rollback works: a present blob is never re-pulled.
//  2. Generates a minimal compose override pinning each service to its snapshot
//     digest AND forcing `pull_policy: never` (so the shared `up --no-start`
//     Create step never re-pulls during a registry outage — the digest blob is
//     already present from step 1), writes it to a temp file in os.TempDir(),
//     discovers the project's main compose file, and sets
//     ExtraComposeFiles = [main, override] — main FIRST because `-f` disables
//     compose's file auto-discovery.
//
// Best-effort advisory: when a service's CURRENTLY-running container already
// uses the snapshot digest, an "already at snapshot" line is written to w and
// prep proceeds (a recreate is still meaningful — e.g. after a crash). The
// current-digest probe reuses SnapshotServices and is non-fatal.
//
// On success the returned cleanup removes the temp override file and RESETS
// ExtraComposeFiles to nil; the caller invokes it after the rollback pipeline
// (and, in the TUI, the wait phase) completes — never goroutine-deferred. On
// error cleanup is nil and no state was mutated.
func (c *Compose) PrepareRollback(ctx context.Context, entries map[string]SnapshotEntry, services []string, w io.Writer) (func(), error) {
	targets := rollbackTargets(entries, services)

	// Discover the main compose file first so a project with no compose file
	// fails fast, before any (potentially slow) pull.
	main, err := c.findComposeFile()
	if err != nil {
		return nil, fmt.Errorf("rollback prep: %w", err)
	}
	// findComposeFile returns filepath.Join(ProjectDir, name), which stays
	// RELATIVE when ProjectDir is relative (e.g. `-C ./app`). command() sets
	// cmd.Dir = ProjectDir and docker resolves `-f` against that cwd, so a
	// relative `-f app/compose.yml` would be re-prefixed to ./app/app/compose.yml
	// and fail. Absolutize so the `-f` path is cwd-independent (and the derived
	// project name stays stable). The remote path uses a bare name + `cd`, so
	// only the local side needs this.
	if abs, aerr := filepath.Abs(main); aerr == nil {
		main = abs
	}

	// Advisory same-digest check (best-effort, non-fatal).
	c.warnAlreadyAtSnapshot(ctx, entries, targets, w)

	// Ensure each target digest is present locally, pulling by digest when
	// missing. Abort BEFORE writing the override / mutating ExtraComposeFiles.
	for _, svc := range targets {
		entry, ok := entries[svc]
		if !ok {
			continue
		}
		ref := rollbackImageRef(entry)
		if _, ierr := c.runDockerCmd(ctx, imagePresenceArgs(ref)); ierr != nil {
			fmt.Fprintf(w, "pulling %s (not cached locally)\n", ref)
			if perr := c.runDockerCmdStream(ctx, []string{"pull", ref}, w); perr != nil {
				return nil, fmt.Errorf("rollback prep: %s: image %s unavailable (not cached locally and pull failed): %w", svc, ref, perr)
			}
		}
	}

	override := buildOverrideYAML(entries, targets)
	tmp, err := os.CreateTemp("", "cdeploy-rollback-*.yml")
	if err != nil {
		return nil, fmt.Errorf("rollback prep: creating override file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(override); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("rollback prep: writing override file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("rollback prep: closing override file: %w", err)
	}

	c.ExtraComposeFiles = []string{main, tmpPath}
	cleanup := func() {
		os.Remove(tmpPath)
		c.ExtraComposeFiles = nil
	}
	return cleanup, nil
}

// warnAlreadyAtSnapshot writes an "already at snapshot" advisory to w for each
// target service whose CURRENTLY-running container already uses the snapshot
// digest (the canonical idempotent-rollback case: running rollback twice lands
// on the same state). The current digest is the running container's actual
// digest — captured by reusing SnapshotServices, NOT the tag's current digest,
// so a tag that has since been re-pointed doesn't produce a false negative.
// Entirely best-effort: any capture error (compose config unavailable, nothing
// running) skips the advisory without failing prep.
func (c *Compose) warnAlreadyAtSnapshot(ctx context.Context, entries map[string]SnapshotEntry, targets []string, w io.Writer) {
	cur, err := c.SnapshotServices(ctx, targets)
	if err != nil {
		return
	}
	for _, svc := range targets {
		entry, ok := entries[svc]
		if !ok {
			continue
		}
		if now, ok := cur.Snapshot.Services[svc]; ok && now.Digest == entry.Digest {
			fmt.Fprintf(w, "%s: already at snapshot digest %s\n", svc, entry.Digest)
		}
	}
}
