package tui

import (
	"strings"

	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/runner"
)

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
	headers := groupsHaveHeaders(groups)

	var entries []svcEntry
	for gi, g := range groups {
		if headers {
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

// groupsHaveHeaders is the single home of the "does this screen draw group
// headers" rule: more than one group. rebuildSvcEntries emits the header rows
// off it and the renderer takes its 2-cell service indent off the same call, so
// a host that happens to hold exactly one project renders byte-identically to
// the drilled screen.
func groupsHaveHeaders(groups []svcGroup) bool { return len(groups) > 1 }

// hasGroupHeaders reports whether the container screen currently draws group
// header rows. Render decisions that must follow the entry model — the service
// indent, the caption pad, the scroll indicators — read it rather than
// m.grouped: grouped mode with a single project emits no headers.
func (m Model) hasGroupHeaders() bool { return groupsHaveHeaders(m.svcGroups) }

// groupCounts totals one group's live state for its header row: how many of its
// services are running, and how many report a failing healthcheck. A service
// with no status entry counts as neither — the host-wide fetch reports only
// containers that exist, and a project whose containers were all removed must
// read "0 up" rather than inflate the total.
func groupCounts(g svcGroup, status map[string]runner.ServiceStatus) (up, unhealthy int) {
	for _, name := range g.services {
		st, ok := status[svcKey(g.proj.Name, name)]
		if !ok {
			continue
		}
		if st.Running {
			up++
		}
		if st.Health == "unhealthy" {
			unhealthy++
		}
	}
	return up, unhealthy
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

// groupUnmanaged reports whether the group at gi is the synthetic unmanaged
// bucket. Its rows are read-only host containers with no compose file behind
// them, so they carry no checkbox and never enter a selection.
func (m Model) groupUnmanaged(gi int) bool {
	if gi < 0 || gi >= len(m.svcGroups) {
		return false
	}
	return m.svcGroups[gi].proj.Unmanaged
}

// selectableRefs is svcRefs minus the unmanaged bucket. Selection feeds the
// compose pipelines, and an unmanaged container has no compose project to run
// one against, so `a`, allSelected and the title's denominator all read this
// rather than svcRefs — otherwise `a` would appear to select rows whose
// checkbox is not even drawn.
func (m Model) selectableRefs() []svcRef {
	refs := m.svcRefs()
	out := refs[:0:0]
	for _, r := range refs {
		if m.groupUnmanaged(r.groupIdx) {
			continue
		}
		out = append(out, r)
	}
	return out
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

// flattenQualified converts a host-wide project → service → value map, the
// shape both grouped fetches return, into the flat qualified-key form the
// Model holds. It is the grouped twin of qualifyMap and the same message
// boundary: nothing qualified travels back out to runner or compose.
func flattenQualified[V any](host map[string]map[string]V) map[string]V {
	if host == nil {
		return nil
	}
	out := make(map[string]V)
	for proj, svcs := range host {
		for svc, v := range svcs {
			out[svcKey(proj, svc)] = v
		}
	}
	return out
}

// buildSvcGroups folds the grouped payload — the project list from the
// ProjectLoader and the host-wide status map from GroupedStatus — into the row
// model.
//
// Project ORDER comes from the loader, not from the status map: the loader
// already sorts the compose projects and deliberately appends the synthetic
// unmanaged row last, and a map has no order at all. A project the loader
// reported but the host has no containers for yields an empty group, which
// renders as a bare header — that is a real state (every container removed),
// not an error.
//
// The reverse case is the safety net: a project present in the status map that
// the loader did not report. `docker compose ls` and `docker ps` are two calls
// and can disagree (a project created between them, or an unmanaged bucket
// whose count call failed). Those groups are appended after the loader's, in
// name order with the unmanaged bucket last, so a running container is never
// silently invisible.
//
// prev supplies the fold state: folding is UI state the user set, and this
// function also runs on the periodic reload, so a fold must survive a refresh.
// It is matched by project NAME because the group slice is rebuilt wholesale.
func buildSvcGroups(projects []compose.Project, host map[string]map[string]runner.ServiceStatus, prev []svcGroup) []svcGroup {
	folded := make(map[string]bool, len(prev))
	for _, g := range prev {
		if g.folded {
			folded[g.proj.Name] = true
		}
	}

	groups := make([]svcGroup, 0, len(projects))
	seen := make(map[string]bool, len(projects))
	add := func(p compose.Project) {
		if p.Name == "" || seen[p.Name] {
			return
		}
		seen[p.Name] = true
		svcs := make([]string, 0, len(host[p.Name]))
		for name := range host[p.Name] {
			svcs = append(svcs, name)
		}
		groups = append(groups, svcGroup{proj: p, services: sortServices(svcs), folded: folded[p.Name]})
	}
	for _, p := range projects {
		add(p)
	}

	var extra []string
	unmanagedExtra := false
	for name := range host {
		if seen[name] || name == "" {
			continue
		}
		if name == compose.UnmanagedProjectName {
			unmanagedExtra = true
			continue
		}
		extra = append(extra, name)
	}
	for _, name := range sortServices(extra) {
		add(compose.Project{Name: name})
	}
	if unmanagedExtra {
		add(compose.Project{Name: compose.UnmanagedProjectName, Unmanaged: true})
	}
	return groups
}

// cursorGroup returns the group the cursor row belongs to. Unlike cursorService
// it accepts a header row: a header IS its group, and the keys that act on a
// whole project — drill-in, config — are exactly the ones a header row should
// answer.
func (m Model) cursorGroup() (svcGroup, bool) {
	e, ok := m.cursorEntry()
	if !ok || e.groupIdx < 0 || e.groupIdx >= len(m.svcGroups) {
		return svcGroup{}, false
	}
	return m.svcGroups[e.groupIdx], true
}

// opBatch is one project's share of an operation: the project to build a
// composer for, and the BARE service names inside it that the pipeline should
// touch.
//
// An EMPTY services slice is not "nothing" — runner reads it as ALL services,
// resolved by compose itself. That is the only way a whole-project batch can
// reach a service that was never created: the host-wide `docker ps` behind the
// grouped view reports containers, so a never-created service has no row.
type opBatch struct {
	proj     compose.Project
	services []string
}

// partitionSelection splits the current selection into one batch per project,
// in SCREEN order — svcGroups is the row order, so the batches run top to
// bottom, which is the order the confirmation prompt names them in.
//
// The unmanaged bucket never enters a batch: its rows have no compose project
// behind them, so there is no pipeline to run. Its keys cannot reach m.selected
// either (the space handler refuses them), so the skip here is the second half
// of one rule rather than a duplicate check.
//
// An EMPTY selection resolves to ONE batch — the cursor's group, with an empty
// services slice, i.e. the whole project including its never-created services.
// A cursor on the unmanaged bucket (or on no row at all) yields no batch, which
// callers report as "nothing selected".
func (m Model) partitionSelection() []opBatch {
	var batches []opBatch
	for _, g := range m.svcGroups {
		if g.proj.Unmanaged {
			continue
		}
		var svcs []string
		for _, name := range g.services {
			if m.selected[svcKey(g.proj.Name, name)] {
				svcs = append(svcs, name)
			}
		}
		if len(svcs) > 0 {
			batches = append(batches, opBatch{proj: g.proj, services: svcs})
		}
	}
	if len(batches) > 0 {
		return batches
	}
	g, ok := m.cursorGroup()
	if !ok || g.proj.Unmanaged {
		return nil
	}
	return []opBatch{{proj: g.proj}}
}

// formatBatchTargets names the batches for the confirmation prompt:
// "web (nginx, api) → db (all)". A batch with no services reads "(all)"
// because that is what an empty slice means downstream — spelling it out is
// what keeps the prompt honest about how much a whole-group op touches.
func formatBatchTargets(batches []opBatch) string {
	parts := make([]string, 0, len(batches))
	for _, b := range batches {
		if len(b.services) == 0 {
			parts = append(parts, b.proj.Name+" (all)")
			continue
		}
		parts = append(parts, b.proj.Name+" ("+strings.Join(b.services, ", ")+")")
	}
	return strings.Join(parts, " → ")
}
