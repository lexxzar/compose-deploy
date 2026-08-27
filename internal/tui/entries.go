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

// groupProjName names the group at index gi. An out-of-range index yields the
// empty string rather than panicking: svcEntries is rebuilt from svcGroups, so
// the two can only disagree if a caller wrote svcEntries by hand.
func (m Model) groupProjName(gi int) string {
	if gi < 0 || gi >= len(m.svcGroups) {
		return ""
	}
	return m.svcGroups[gi].proj.Name
}

// svcKeyAt returns the qualified key of the row at index i, or "" when the row
// is a group header or the index is out of range. Callers that key selection,
// status or stats off a row index must go through it — the owning group, not
// the model's current project, decides the prefix.
func (m Model) svcKeyAt(i int) string {
	if i < 0 || i >= len(m.svcEntries) {
		return ""
	}
	e := m.svcEntries[i]
	if e.kind != entrySvcService {
		return ""
	}
	return svcKey(m.groupProjName(e.groupIdx), e.name)
}

// ownerProjName names the group that an incoming bare-name payload belongs to.
// The composer that produced the payload serves exactly one project, and the
// drilled path installs exactly one group, so the answer is that group's
// project; the m.projName fallback covers a payload that lands before
// setSingleGroup has run.
func (m Model) ownerProjName() string {
	if len(m.svcGroups) == 1 {
		return m.svcGroups[0].proj.Name
	}
	return m.projName
}

// qualifyMap converts a bare-name map, as every Composer method returns one,
// into the qualified-key form the Model holds. It is the message-boundary
// conversion: qualified keys live only inside the Model, so nothing qualified
// may travel back out to runner or compose.
func qualifyMap[V any](projName string, src map[string]V) map[string]V {
	if src == nil {
		return nil
	}
	out := make(map[string]V, len(src))
	for name, v := range src {
		out[svcKey(projName, name)] = v
	}
	return out
}

// svcRef is one service the container screen owns: the group it belongs to,
// the bare name a Composer knows it by, and the qualified key the Model stores
// it under.
type svcRef struct {
	groupIdx int
	name     string
	key      string
}

// svcRefs enumerates every service in every group, in group order. Fold state
// is deliberately ignored — folding hides ROWS, never services — so selection,
// counting and column-width helpers go through it instead of over svcEntries,
// which would drop a folded group's services from the selection.
func (m Model) svcRefs() []svcRef {
	var refs []svcRef
	for gi, g := range m.svcGroups {
		for _, name := range g.services {
			refs = append(refs, svcRef{groupIdx: gi, name: name, key: svcKey(g.proj.Name, name)})
		}
	}
	return refs
}

// cursorEntry returns the row under the cursor. The cursor indexes svcEntries,
// so it may sit on a group header; callers that need a service must go through
// cursorService.
func (m Model) cursorEntry() (svcEntry, bool) {
	if m.svcCursor < 0 || m.svcCursor >= len(m.svcEntries) {
		return svcEntry{}, false
	}
	return m.svcEntries[m.svcCursor], true
}

// cursorService returns the service name under the cursor, and false when the
// cursor sits on a group header or out of range. Every action key that acts on
// "the service under the cursor" reads it — indexing services by svcCursor is
// wrong now that headers occupy rows of their own.
func (m Model) cursorService() (string, bool) {
	e, ok := m.cursorEntry()
	if !ok || e.kind != entrySvcService {
		return "", false
	}
	return e.name, true
}
