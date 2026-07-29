package compose

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
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
		return nil, fmt.Errorf("parse snapshot: %w", err)
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
// service to its snapshot digest (`image: <repo>@<digest>`). The repo is
// derived from the recorded image reference via stripTag so the tag is dropped
// and the digest ref is unambiguous. Services are emitted in SORTED order,
// built from a sorted slice — never by marshaling a map, whose iteration order
// is random and would make the generated file (and its argv) non-deterministic.
// Only services present in entries are emitted; a requested service missing
// from entries is skipped (the caller validates completeness before pinning).
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
		repo := stripTag(entry.Image)
		fmt.Fprintf(&b, "  %s:\n", name)
		fmt.Fprintf(&b, "    image: %s@%s\n", repo, entry.Digest)
	}
	return []byte(b.String())
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
// image IDs are zipped back to the input container IDs positionally; a
// line-count mismatch is treated as an error.
func (c *Compose) inspectContainerImageIDs(ctx context.Context, containerIDs []string) (map[string]string, error) {
	if len(containerIDs) == 0 {
		return map[string]string{}, nil
	}
	args := append([]string{"inspect", "--format", "{{.Image}}"}, containerIDs...)
	out, err := c.runDockerCmd(ctx, args)
	if err != nil {
		return nil, err
	}
	lines := nonEmptyLines(string(out))
	if len(lines) != len(containerIDs) {
		return nil, fmt.Errorf("docker inspect returned %d image IDs for %d containers", len(lines), len(containerIDs))
	}
	m := make(map[string]string, len(containerIDs))
	for i, id := range containerIDs {
		m[id] = lines[i]
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

// WriteSnapshot merges fresh into the existing on-disk snapshot (if any) and
// writes the result atomically under $HOME/.cdeploy/state/. A corrupt or
// unreadable existing file is ignored (treated as empty) so a single bad state
// file never blocks recording a fresh, good snapshot; the per-service merge
// safety net is best-effort. Not-found is normal (first deploy).
func (c *Compose) WriteSnapshot(ctx context.Context, fresh *Snapshot) error {
	path, err := c.localStatePath()
	if err != nil {
		return err
	}
	existing, _ := c.ReadSnapshot(ctx) // ignore read/parse errors: overwrite a corrupt state
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
