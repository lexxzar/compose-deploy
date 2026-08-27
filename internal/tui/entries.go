package tui

import "github.com/lexxzar/compose-deploy/internal/compose"

// svcEntryKind distinguishes a group header row from a service row on the
// container screen.
type svcEntryKind int

const (
	entrySvcGroupHeader svcEntryKind = iota
	entrySvcService
)

// svcEntry is one rendered row of the container screen. It is derived state:
// rebuildSvcEntries regenerates the whole slice from svcGroup, so nothing may
// be keyed on an entry index across a rebuild.
type svcEntry struct {
	kind     svcEntryKind
	groupIdx int    // index into the svcGroup slice the entry came from
	name     string // service name; empty on a header
}

// svcGroup is one compose project (or the synthetic unmanaged bucket) together
// with the services it owns. It is the source of truth behind the container
// screen; svcEntry is derived from it.
type svcGroup struct {
	proj     compose.Project
	services []string
	folded   bool
}

// rebuildSvcEntries derives the row list from the groups. One header per group
// followed by its services; a folded group contributes only its header.
//
// A single group emits NO header at all — that degenerate shape is what makes
// the drilled single-project screen render exactly as it did before grouping
// existed, so the header is a multi-group affordance only. A single folded
// group therefore emits nothing.
func rebuildSvcEntries(groups []svcGroup) []svcEntry {
	if len(groups) == 0 {
		return nil
	}
	single := len(groups) == 1

	var entries []svcEntry
	for gi, g := range groups {
		if !single {
			entries = append(entries, svcEntry{kind: entrySvcGroupHeader, groupIdx: gi})
		}
		if g.folded {
			continue
		}
		for _, name := range g.services {
			entries = append(entries, svcEntry{kind: entrySvcService, groupIdx: gi, name: name})
		}
	}
	return entries
}

// svcKeySep separates the project half of a qualified key from the service
// half. "/" is safe: docker compose rejects it in both a project name and a
// service name, so no pair can produce an ambiguous key.
const svcKeySep = "/"

// svcKey is the single producer of the qualified key that identifies one
// service inside one project. Selection, status, stats and update verdicts all
// key on it, because a bare service name collides across projects — two
// projects each owning a "db" is the common case, not the exotic one.
//
// Qualified keys live only inside the tui Model. Every message boundary
// converts: nothing qualified may reach runner or compose.
func svcKey(projName, service string) string {
	return projName + svcKeySep + service
}
