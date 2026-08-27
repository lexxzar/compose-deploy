package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/runner"
	"github.com/spf13/cobra"
)

func newRollbackCmd() *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "rollback [containers...]",
		Short: "Roll services back to the previously-deployed image digests",
		Long: `Rolls services back to the image digests recorded by the most recent deploy.

Every 'cdeploy deploy' snapshots the digest each running container uses to a
state file on the docker host. 'rollback' re-creates the targeted services
pinned to those digests via a generated compose override (stop, remove, create,
start — no pull), so it works even when the registry is unreachable as long as
the old image blob is still on the host.

Note: rollback restores IMAGES ONLY against the current compose file; other
config drift (env, ports, volumes) is not rewound.`,
		Example: `  # Roll back a single service
  cdeploy rollback web

  # Roll back all services and wait for them to become healthy
  cdeploy rollback -a --wait

  # Roll back on a remote server
  cdeploy rollback web -s prod -C /opt/myapp`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOperationWithPrep(cmd.Context(), runner.Rollback, all, args,
				rollbackPrep(all, args, os.Stdout))
		},
	}

	cmd.Flags().BoolVarP(&all, "all", "a", false, "roll back all services in the snapshot")
	addWaitFlags(cmd)

	return cmd
}

// rollbackPreparer is the subset of the concrete composers a rollback needs:
// reading the host-side snapshot and preparing the digest-pinned override.
// Both *compose.Compose and *compose.RemoteCompose satisfy it; a composer that
// does not (e.g. a test mock lacking these methods) is rejected with a clear
// error rather than silently skipping the pin.
type rollbackPreparer interface {
	ReadSnapshot(ctx context.Context) (*compose.Snapshot, error)
	PrepareRollback(ctx context.Context, entries map[string]compose.SnapshotEntry, services []string, w io.Writer) (func(), error)
}

// rollbackPrep builds the runOperationWithPrep hook for a rollback: it reads the
// snapshot, applies the refusal rules, prints the plan, and prepares the
// digest-pinned override. The returned cleanup (removing the override file and
// resetting ExtraComposeFiles) is handed back to runOperationWithPrep, which
// defers it. The second return value is the resolved snapshot service set, which
// runOperationWithPrep gates the --wait phase on — for `-a` this is the snapshot
// services, NOT every compose service (a build-only service that was never rolled
// back must not be dragged into the health gate). `all` and `containers` are the
// flag/args captured at command time; out is where the plan lines and pull
// progress are written (os.Stdout in production, a buffer in tests).
func rollbackPrep(all bool, containers []string, out io.Writer) func(context.Context, runner.Composer) (func(), []string, error) {
	return func(ctx context.Context, c runner.Composer) (func(), []string, error) {
		p, ok := c.(rollbackPreparer)
		if !ok {
			return nil, nil, fmt.Errorf("rollback is not supported for this connection")
		}

		snap, err := p.ReadSnapshot(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("reading rollback snapshot: %w", err)
		}

		targets, err := resolveRollbackTargets(snap, all, containers)
		if err != nil {
			return nil, nil, err
		}

		// Rollback pins images only against the CURRENT compose file. Intersect
		// the snapshot targets with the services that actually still exist in the
		// compose file, so a stale snapshot entry for a since-removed service is
		// never re-added by the generated override (which would resurrect it as a
		// minimal image-only service). For `-a` a stale entry is warned-and-
		// skipped; a named target that no longer exists is a hard error.
		live, err := c.ListServices(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("listing current compose services: %w", err)
		}
		targets, err = filterLiveTargets(targets, live, all, out)
		if err != nil {
			return nil, nil, err
		}

		fmt.Fprintln(out, "Rollback plan:")
		for _, svc := range targets {
			fmt.Fprintf(out, "  %s\n", rollbackPlanLine(svc, snap.Services[svc]))
		}

		cleanup, err := p.PrepareRollback(ctx, snap.Services, targets, out)
		if err != nil {
			return nil, nil, err
		}
		return cleanup, targets, nil
	}
}

// resolveRollbackTargets applies the rollback refusal rules and resolves the
// sorted set of services to roll back. It refuses (never guesses) when:
//   - no snapshot exists (nil snap → the state file is missing);
//   - -a is requested but the snapshot records no services;
//   - a named service is absent from the snapshot (the error names exactly the
//     missing services).
//
// The snapshot-missing and schema/unreadable cases are distinguished by the
// caller: schema/unreadable surface as a ReadSnapshot error before this runs,
// while a missing file yields a nil snap handled here.
func resolveRollbackTargets(snap *compose.Snapshot, all bool, containers []string) ([]string, error) {
	if snap == nil {
		return nil, fmt.Errorf("no rollback snapshot found for this project; run 'cdeploy deploy' first to record one, or pass --project-name if the project was deployed under one")
	}

	var targets []string
	if all {
		for name := range snap.Services {
			targets = append(targets, name)
		}
		if len(targets) == 0 {
			return nil, fmt.Errorf("rollback snapshot has no services to restore")
		}
	} else {
		var missing []string
		for _, svc := range containers {
			if _, ok := snap.Services[svc]; !ok {
				missing = append(missing, svc)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return nil, fmt.Errorf("no rollback snapshot for service(s): %s", strings.Join(missing, ", "))
		}
		targets = append(targets, containers...)
	}

	sort.Strings(targets)
	return targets, nil
}

// filterLiveTargets intersects the snapshot-resolved rollback targets with the
// services that CURRENTLY exist in the compose file (from ListServices /
// `config --services`). Rollback pins images only against the current compose
// file (documented caveat), so a snapshot service that has since been removed
// from compose must not be re-added by the generated override. For `-a` a stale
// entry is warned-and-skipped; for an explicitly named target that no longer
// exists it is a hard error naming exactly the missing services — matching the
// refusal-rule style of resolveRollbackTargets (never guesses). The returned
// slice preserves input (sorted) order.
func filterLiveTargets(targets, live []string, all bool, out io.Writer) ([]string, error) {
	liveSet := make(map[string]bool, len(live))
	for _, s := range live {
		liveSet[s] = true
	}

	var kept, missing []string
	for _, t := range targets {
		if liveSet[t] {
			kept = append(kept, t)
			continue
		}
		if all {
			fmt.Fprintf(out, "  %s %s is in the snapshot but no longer in the compose file; skipping\n",
				styleWarning.Render("Warning:"), t)
			continue
		}
		missing = append(missing, t)
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("service(s) no longer in the current compose file: %s", strings.Join(missing, ", "))
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("no snapshot services remain in the current compose file to roll back")
	}
	return kept, nil
}

// rollbackPlanLine formats one plan entry, e.g.
//
//	web  nginx@sha256:ab12cd34ef56... (recorded 2026-07-28 14:03)
//
// The image tag is stripped (the pin is by digest), the digest is truncated for
// a compact preview, and recorded_at is rendered in local-friendly form.
func rollbackPlanLine(service string, entry compose.SnapshotEntry) string {
	return fmt.Sprintf("%s  %s@%s (recorded %s)",
		service, compose.StripTag(entry.Image), shortDigest(entry.Digest), formatRecordedAt(entry.RecordedAt))
}

// shortDigest truncates the hex portion of a digest for a compact plan preview,
// keeping the algorithm prefix (e.g. sha256:). Digests that are already short
// (or lack the expected prefix) are returned unchanged.
func shortDigest(digest string) string {
	const keep = 12 // hex chars to show before the ellipsis
	if i := strings.Index(digest, ":"); i >= 0 {
		hex := digest[i+1:]
		if len(hex) > keep {
			return digest[:i+1] + hex[:keep] + "..."
		}
	}
	return digest
}

// formatRecordedAt renders a snapshot's RFC3339 recorded_at as
// `2006-01-02 15:04`. A value that does not parse (older/foreign format) is
// returned verbatim so the plan line never hides the raw timestamp.
func formatRecordedAt(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Format("2006-01-02 15:04")
	}
	return s
}
