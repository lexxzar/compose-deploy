package compose

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
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
