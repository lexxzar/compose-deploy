package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/runner"
)

// --- subcommand + flag registration ---

func TestRollbackCmd_SubcommandExists(t *testing.T) {
	root := NewRootCmd()
	cmd, _, err := root.Find([]string{"rollback"})
	if err != nil {
		t.Fatalf("rollback command not found: %v", err)
	}
	if cmd.Name() != "rollback" {
		t.Errorf("found command name = %q, want %q", cmd.Name(), "rollback")
	}
}

func TestRollbackCmd_FlagRegistration(t *testing.T) {
	root := NewRootCmd()
	rollback, _, err := root.Find([]string{"rollback"})
	if err != nil {
		t.Fatalf("rollback command not found: %v", err)
	}

	allFlag := rollback.Flags().Lookup("all")
	if allFlag == nil {
		t.Fatal("--all flag not found on rollback command")
	}
	if allFlag.Shorthand != "a" {
		t.Errorf("--all shorthand = %q, want %q", allFlag.Shorthand, "a")
	}

	if rollback.Flags().Lookup("wait") == nil {
		t.Error("--wait flag not found on rollback command")
	}
	wt := rollback.Flags().Lookup("wait-timeout")
	if wt == nil {
		t.Fatal("--wait-timeout flag not found on rollback command")
	}
	if wt.DefValue != runner.DefaultWaitTimeout.String() {
		t.Errorf("--wait-timeout default = %q, want %q", wt.DefValue, runner.DefaultWaitTimeout.String())
	}
}

func TestRollbackCmd_PersistentFlagsInherited(t *testing.T) {
	root := NewRootCmd()
	rollback, _, err := root.Find([]string{"rollback"})
	if err != nil {
		t.Fatalf("rollback command not found: %v", err)
	}
	for _, name := range []string{"server", "ssh", "project-dir", "identity"} {
		if rollback.InheritedFlags().Lookup(name) == nil {
			t.Errorf("--%s persistent flag not inherited by rollback command", name)
		}
	}
}

// --- arg validation (names xor -a) ---

func TestRollbackCmd_NoArgsNoFlag(t *testing.T) {
	oldServer, oldSSH, oldProj := serverName, sshTarget, projectDir
	t.Cleanup(func() { serverName, sshTarget, projectDir = oldServer, oldSSH, oldProj })

	root := NewRootCmd()
	root.SetArgs([]string{"rollback"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when no args and no -a flag")
	}
	if !strings.Contains(err.Error(), "specify container names") {
		t.Errorf("error = %q, want it to mention 'specify container names'", err.Error())
	}
}

func TestRollbackCmd_AllWithContainerNames(t *testing.T) {
	oldServer, oldSSH, oldProj := serverName, sshTarget, projectDir
	t.Cleanup(func() { serverName, sshTarget, projectDir = oldServer, oldSSH, oldProj })

	root := NewRootCmd()
	root.SetArgs([]string{"rollback", "-a", "nginx"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for -a combined with container names")
	}
	if !strings.Contains(err.Error(), "cannot be combined") {
		t.Errorf("error = %q, want it to mention 'cannot be combined'", err.Error())
	}
}

func TestRollbackCmd_SSHAndServerMutex(t *testing.T) {
	oldServer, oldSSH, oldProj := serverName, sshTarget, projectDir
	t.Cleanup(func() { serverName, sshTarget, projectDir = oldServer, oldSSH, oldProj })

	root := NewRootCmd()
	root.SetArgs([]string{"rollback", "-s", "prod", "-S", "user@host", "-C", "/srv/app", "-a"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected mutex error, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %q, want 'mutually exclusive'", err.Error())
	}
}

// --- resolveRollbackTargets refusal rules ---

func snapWith(services ...string) *compose.Snapshot {
	snap := &compose.Snapshot{Schema: 1, Services: map[string]compose.SnapshotEntry{}}
	for _, s := range services {
		snap.Services[s] = compose.SnapshotEntry{
			Image:      s + ":latest",
			Digest:     "sha256:" + strings.Repeat("a", 64),
			RecordedAt: "2026-07-28T14:03:00Z",
		}
	}
	return snap
}

func TestResolveRollbackTargets_NoSnapshot(t *testing.T) {
	_, err := resolveRollbackTargets(nil, false, []string{"web"})
	if err == nil {
		t.Fatal("expected error for missing snapshot")
	}
	if !strings.Contains(err.Error(), "no rollback snapshot found") {
		t.Errorf("error = %q, want 'no rollback snapshot found'", err.Error())
	}
}

func TestResolveRollbackTargets_AllEmpty(t *testing.T) {
	snap := &compose.Snapshot{Schema: 1, Services: map[string]compose.SnapshotEntry{}}
	_, err := resolveRollbackTargets(snap, true, nil)
	if err == nil {
		t.Fatal("expected error for -a with no recorded services")
	}
	if !strings.Contains(err.Error(), "no services") {
		t.Errorf("error = %q, want it to mention 'no services'", err.Error())
	}
}

func TestResolveRollbackTargets_AllReturnsSorted(t *testing.T) {
	snap := snapWith("web", "db", "api")
	targets, err := resolveRollbackTargets(snap, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"api", "db", "web"}
	if len(targets) != len(want) {
		t.Fatalf("targets = %v, want %v", targets, want)
	}
	for i := range want {
		if targets[i] != want[i] {
			t.Errorf("targets = %v, want sorted %v", targets, want)
		}
	}
}

func TestResolveRollbackTargets_NamedPresent(t *testing.T) {
	snap := snapWith("web", "db")
	targets, err := resolveRollbackTargets(snap, false, []string{"web"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 1 || targets[0] != "web" {
		t.Errorf("targets = %v, want [web]", targets)
	}
}

func TestResolveRollbackTargets_MissingServiceNamed(t *testing.T) {
	snap := snapWith("web")
	_, err := resolveRollbackTargets(snap, false, []string{"db", "cache"})
	if err == nil {
		t.Fatal("expected error naming the missing services")
	}
	// The error must name exactly the missing services (sorted), not the present one.
	if !strings.Contains(err.Error(), "cache") || !strings.Contains(err.Error(), "db") {
		t.Errorf("error = %q, want it to name db and cache", err.Error())
	}
	if strings.Contains(err.Error(), "web") {
		t.Errorf("error = %q, should not name the present service web", err.Error())
	}
	// Sorted: cache before db.
	if strings.Index(err.Error(), "cache") > strings.Index(err.Error(), "db") {
		t.Errorf("missing services not sorted: %q", err.Error())
	}
}

// --- plan-line formatting ---

func TestRollbackPlanLine(t *testing.T) {
	entry := compose.SnapshotEntry{
		Image:      "nginx:latest",
		Digest:     "sha256:ab12cd34ef567890aabbccddeeff00112233445566778899aabbccddeeff0011",
		RecordedAt: "2026-07-28T14:03:00Z",
	}
	line := rollbackPlanLine("web", entry)

	// Service name, tag-stripped repo, truncated digest, and a recorded stamp.
	for _, want := range []string{"web", "nginx@sha256:ab12cd34ef56", "...", "recorded", "2026-07-28 14:03"} {
		if !strings.Contains(line, want) {
			t.Errorf("plan line %q missing %q", line, want)
		}
	}
	// The tag must be stripped (no "nginx:latest@").
	if strings.Contains(line, "nginx:latest@") {
		t.Errorf("plan line %q should strip the image tag", line)
	}
}

func TestFormatRecordedAt(t *testing.T) {
	if got := formatRecordedAt("2026-07-28T14:03:00Z"); got != "2026-07-28 14:03" {
		t.Errorf("formatRecordedAt = %q, want %q", got, "2026-07-28 14:03")
	}
	// Unparseable input is returned verbatim.
	if got := formatRecordedAt("not-a-time"); got != "not-a-time" {
		t.Errorf("formatRecordedAt(bad) = %q, want it unchanged", got)
	}
}

func TestShortDigest(t *testing.T) {
	long := "sha256:" + strings.Repeat("a", 64)
	got := shortDigest(long)
	if !strings.HasPrefix(got, "sha256:") || !strings.HasSuffix(got, "...") {
		t.Errorf("shortDigest = %q, want sha256:... form", got)
	}
	// A short digest is returned unchanged.
	if got := shortDigest("sha256:abcd"); got != "sha256:abcd" {
		t.Errorf("shortDigest(short) = %q, want unchanged", got)
	}
}

// --- rollbackPrep hook (drives ReadSnapshot + refusal + PrepareRollback) ---

// rollbackMock implements both runner.Composer (via the embedded opMockComposer)
// and rollbackPreparer, so the prep hook's type assertion succeeds and the
// snapshot/prepare seam can be scripted without docker.
type rollbackMock struct {
	*opMockComposer
	snap        *compose.Snapshot
	readErr     error
	prepErr     error
	prepCalls   int
	prepEntries map[string]compose.SnapshotEntry
	prepSvcs    []string
	cleanupRuns *int
}

func (m *rollbackMock) ReadSnapshot(_ context.Context) (*compose.Snapshot, error) {
	return m.snap, m.readErr
}

func (m *rollbackMock) PrepareRollback(_ context.Context, entries map[string]compose.SnapshotEntry, services []string, w io.Writer) (func(), error) {
	m.prepCalls++
	m.prepEntries = entries
	m.prepSvcs = append([]string(nil), services...)
	if m.prepErr != nil {
		return nil, m.prepErr
	}
	return func() {
		if m.cleanupRuns != nil {
			*m.cleanupRuns++
		}
	}, nil
}

func newRollbackMock() *rollbackMock {
	return &rollbackMock{opMockComposer: &opMockComposer{}}
}

func TestRollbackPrep_NoSnapshot(t *testing.T) {
	m := newRollbackMock() // snap == nil → missing state file
	var buf bytes.Buffer

	_, _, err := rollbackPrep(false, []string{"web"}, &buf)(context.Background(), m)
	if err == nil {
		t.Fatal("expected refusal for missing snapshot")
	}
	if !strings.Contains(err.Error(), "no rollback snapshot found") {
		t.Errorf("error = %q, want 'no rollback snapshot found'", err.Error())
	}
	if m.prepCalls != 0 {
		t.Errorf("PrepareRollback called %d times, want 0 on refusal", m.prepCalls)
	}
}

func TestRollbackPrep_SchemaMismatchPropagates(t *testing.T) {
	// ReadSnapshot surfaces the schema error (matching ReadSnapshot's typed
	// error); the prep hook must wrap and surface it, not swallow it.
	m := newRollbackMock()
	m.readErr = errors.New("unsupported snapshot schema: 2")
	var buf bytes.Buffer

	_, _, err := rollbackPrep(true, nil, &buf)(context.Background(), m)
	if err == nil {
		t.Fatal("expected error for schema mismatch")
	}
	if !strings.Contains(err.Error(), "schema") {
		t.Errorf("error = %q, want it to mention schema", err.Error())
	}
	if m.prepCalls != 0 {
		t.Errorf("PrepareRollback called %d times, want 0 on read error", m.prepCalls)
	}
}

func TestRollbackPrep_MissingServiceRefused(t *testing.T) {
	m := newRollbackMock()
	m.snap = snapWith("web")
	var buf bytes.Buffer

	_, _, err := rollbackPrep(false, []string{"db"}, &buf)(context.Background(), m)
	if err == nil {
		t.Fatal("expected refusal naming the missing service")
	}
	if !strings.Contains(err.Error(), "db") {
		t.Errorf("error = %q, want it to name db", err.Error())
	}
	if m.prepCalls != 0 {
		t.Errorf("PrepareRollback called %d times, want 0 on refusal", m.prepCalls)
	}
}

func TestRollbackPrep_HappyPathPrintsPlanAndPrepares(t *testing.T) {
	m := newRollbackMock()
	m.snap = snapWith("web", "db")
	var buf bytes.Buffer

	cleanup, targets, err := rollbackPrep(true, nil, &buf)(context.Background(), m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cleanup == nil {
		t.Fatal("expected non-nil cleanup from prep")
	}
	if m.prepCalls != 1 {
		t.Errorf("PrepareRollback calls = %d, want 1", m.prepCalls)
	}
	// -a → all snapshot services, sorted.
	if len(m.prepSvcs) != 2 || m.prepSvcs[0] != "db" || m.prepSvcs[1] != "web" {
		t.Errorf("PrepareRollback services = %v, want [db web]", m.prepSvcs)
	}
	// Q2: the wait phase must gate on the resolved snapshot services (not nil,
	// which would resolve to every compose service via ListServices).
	if len(targets) != 2 || targets[0] != "db" || targets[1] != "web" {
		t.Errorf("wait targets = %v, want [db web] (the resolved snapshot services)", targets)
	}
	out := buf.String()
	if !strings.Contains(out, "Rollback plan:") {
		t.Errorf("output missing plan header:\n%s", out)
	}
	for _, svc := range []string{"web", "db"} {
		if !strings.Contains(out, svc) {
			t.Errorf("plan output missing service %q:\n%s", svc, out)
		}
	}
}

func TestRollbackPrep_PrepareFailureSurfaced(t *testing.T) {
	m := newRollbackMock()
	m.snap = snapWith("web")
	m.prepErr = errors.New("image sha256:deadbeef unavailable (pull failed)")
	var buf bytes.Buffer

	_, _, err := rollbackPrep(false, []string{"web"}, &buf)(context.Background(), m)
	if err == nil {
		t.Fatal("expected prepare failure to surface")
	}
	if !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("error = %q, want it to mention the pull failure", err.Error())
	}
}

func TestRollbackPrep_UnsupportedComposer(t *testing.T) {
	// A composer that satisfies runner.Composer but NOT rollbackPreparer is
	// rejected with a clear error (never silently skipped).
	var buf bytes.Buffer
	_, _, err := rollbackPrep(false, []string{"web"}, &buf)(context.Background(), &opMockComposer{})
	if err == nil {
		t.Fatal("expected error for a composer without rollback support")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("error = %q, want it to mention 'not supported'", err.Error())
	}
}
