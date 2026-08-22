package compose

import (
	"context"
	"fmt"
)

// UnmanagedProjectName is the name of the synthetic project-picker row that
// stands for the host containers carrying no compose project label. The
// parentheses mark it as not a real project name.
const UnmanagedProjectName = "(unmanaged)"

// CountUnmanaged returns how many containers on the host carry no compose
// project label.
func (h *HostContainers) CountUnmanaged(ctx context.Context) (int, error) {
	entries, err := h.unmanagedEntries(ctx)
	if err != nil {
		return 0, err
	}
	return len(entries), nil
}

// WithUnmanagedRow appends the synthetic unmanaged row to a project list when
// the host has at least one unmanaged container. A count error is swallowed as
// zero: the picker must still show the compose projects that did load.
//
// The append happens after ListProjects deliberately bypasses sortProjects —
// that sorts case-insensitively by name, and "(" sorts before every letter, so
// a sorted row would land first instead of last.
func WithUnmanagedRow(ctx context.Context, hc *HostContainers, projects []Project) []Project {
	if hc == nil {
		return projects
	}
	n, err := hc.CountUnmanaged(ctx)
	if err != nil {
		// Deliberate swallow, kept as its own branch so it reads as a
		// decision rather than sharing the empty-host path: a `docker ps`
		// the SSH user cannot run must not cost the user the compose
		// projects that DID load.
		return projects
	}
	if n == 0 {
		return projects
	}
	unit := "containers"
	if n == 1 {
		unit = "container"
	}
	return append(projects, Project{
		Name:      UnmanagedProjectName,
		Desc:      fmt.Sprintf("%d %s", n, unit),
		Unmanaged: true,
	})
}
