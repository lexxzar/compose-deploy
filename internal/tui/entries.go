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
	entryService
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
// existed, so the header is a multi-group affordance only. groupsHaveHeaders
// owns the one exception that keeps that suppression from emptying the screen.
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
			entries = append(entries, svcEntry{kind: entryService, groupIdx: gi, name: name})
		}
	}
	return entries
}

// groupsHaveHeaders is the single home of the "does this screen draw group
// headers" rule: more than one group. Every render decision that must follow
// the entry model — the service indent, the caption pad, the scroll indicators
// — reads THIS, never m.grouped, because grouped mode with a single project
// emits no headers and must render byte-identically to the drilled screen.
//
// The one exception: a LONE group that contributes no service rows of its own
// keeps its header. Suppressing it there leaves the screen with zero rows, and
// a screen with no rows has no cursor — nothing to unfold, drill into, select
// or operate on, and no name saying which project the user is even looking at.
// Both shapes are reachable: a sole project whose containers are all gone (an
// empty group), and a folded group that outlived the other groups on the host
// (the 5-second reload rebuilds from what `docker ps` still reports, and fold
// state survives the rebuild by project name).
func groupsHaveHeaders(groups []svcGroup) bool {
	if len(groups) > 1 {
		return true
	}
	if len(groups) == 1 {
		return groups[0].folded || len(groups[0].services) == 0
	}
	return false
}

// sanitizeName makes one docker-derived identifier safe to write to a
// terminal. Project and service names on the host view come from container
// LABELS (com.docker.compose.project / .service), and any `docker run --label`
// can set those to arbitrary bytes — a compose-LOOKING container is enough.
// The grouped screen redraws itself every 5 seconds without the user touching
// a key, so an OSC 52 clipboard write or a CSI sequence hidden in a label
// would be replayed into the terminal on its own; ansi.StringWidth counts an
// escape sequence as zero cells, so the clamps and the column padding pass it
// straight through.
//
// It is sanitizeInspectLine, the pass every decoded inspect line already goes
// through — ONE implementation, so the two screens cannot disagree about what
// is safe to draw. Applying it to the NAME rather than to the rendered line
// keeps the width math honest: every consumer (header, row, breadcrumb,
// confirm prompt, progress title, warnings, search bar) measures and pads the
// same already-safe string.
//
// A real compose project is unaffected: compose rejects anything outside
// [a-zA-Z0-9._-] in a service name and [a-z0-9_-] in a project name, so this
// is a no-op on every name compose itself could have produced.
func sanitizeName(s string) string { return sanitizeInspectLine(s) }

// svcKeySep separates the project half of a qualified key from the service
// half. "/" is safe: docker compose rejects it in both a project name and a
// service name, so no pair can produce an ambiguous key.
const svcKeySep = "/"

// svcKey is the single producer of the qualified key that identifies one
// service inside one project. Selection, status, stats and update verdicts all
// key on it, because a bare service name collides across projects.
//
// Qualified keys live only inside the tui Model. Every message boundary
// converts: nothing qualified may reach runner or compose.
//
// Both halves are sanitized here, which is what keeps the keys matching the
// group rows: buildSvcGroups and setSingleGroup store sanitized names, so a
// map converted through qualifyMap/flattenQualified — whose keys arrive raw
// from the composer — would otherwise miss every row whose label carried an
// escape. sanitizeName is idempotent, so re-keying an already-safe name is
// free.
func svcKey(projName, service string) string {
	return sanitizeName(projName) + svcKeySep + sanitizeName(service)
}

// qualifyMap converts a bare-name map, as every Composer method returns one,
// into the qualified-key form the Model holds. It is the message-boundary
// conversion svcKey describes.
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

// flattenQualified converts a host-wide project → service → value map, the
// shape both grouped fetches return, into the flat qualified-key form the
// Model holds. It is the grouped twin of qualifyMap.
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
	if e.kind != entryService {
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

// svcRef is one service the container screen owns: the group it belongs to,
// the bare name a Composer knows it by, and the qualified key the Model stores
// it under.
type svcRef struct {
	groupIdx int
	name     string
	key      string
}

// eachSvcRef visits every service in every group, in group order, and stops as
// soon as fn returns false.
//
// Fold state is deliberately ignored: folding hides ROWS, never services, so
// selection, counting and column-width helpers go through the refs instead of
// over svcEntries, which would drop a folded group's services.
//
// The count and boolean helpers run on the hot path — fixSvcOffset calls
// hasStatusColumns on every keystroke, and the render pass asks for the
// selection count and the select-all state — so materialising the whole list to
// answer "how many" or "any?" is the one allocation those callers do not need.
// svcRefs stays for the render pass, which genuinely wants the list.
func (m Model) eachSvcRef(fn func(svcRef) bool) {
	for gi, g := range m.svcGroups {
		for _, name := range g.services {
			if !fn(svcRef{groupIdx: gi, name: name, key: svcKey(g.proj.Name, name)}) {
				return
			}
		}
	}
}

// eachSelectableRef is eachSvcRef minus the unmanaged bucket. Selection feeds
// the compose pipelines, and an unmanaged container has no compose project to
// run one against, so `a`, allSelected and the title's denominator all go
// through it — otherwise `a` would appear to select rows whose checkbox is not
// even drawn.
func (m Model) eachSelectableRef(fn func(svcRef) bool) {
	m.eachSvcRef(func(r svcRef) bool {
		if m.groupUnmanaged(r.groupIdx) {
			return true
		}
		return fn(r)
	})
}

// svcRefs is eachSvcRef WITH the slice, for the render pass.
func (m Model) svcRefs() []svcRef {
	var refs []svcRef
	m.eachSvcRef(func(r svcRef) bool {
		refs = append(refs, r)
		return true
	})
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
// wrong now that headers occupy rows of their own, and the false it returns for
// an out-of-range cursor subsumes the empty-list check those keys used to carry
// on top of it.
func (m Model) cursorService() (string, bool) {
	e, ok := m.cursorEntry()
	if !ok || e.kind != entryService {
		return "", false
	}
	return e.name, true
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

// groupCounts totals one group's live state for its header row: how many of its
// services are running, how many report a failing healthcheck, and how many have
// an image update waiting. A service with no status entry counts as none of the
// three — the host-wide fetch reports only containers that exist, and a project
// whose containers were all removed must read "0 up" rather than inflate the
// total.
//
// The update total is read off the same tri-state the row glyph draws, so a
// folded group can only ever report what a scan has already established: nil
// (never checked, or checked and failed) counts as zero, exactly like a false
// verdict. Nothing here triggers a scan.
func groupCounts(g svcGroup, status map[string]runner.ServiceStatus) (up, unhealthy, updates int) {
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
		if st.UpdateAvailable != nil && *st.UpdateAvailable {
			updates++
		}
	}
	return up, unhealthy, updates
}

// buildSvcGroups folds the grouped payload — the project list from the
// ProjectLoader and the host-wide status map from GroupHostStatus — into the row
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
	at := make(map[string]int, len(projects))
	authentic := make(map[string]bool, len(projects))
	// add takes the project as the loader reported it, because `host` is keyed
	// by the RAW label and the lookup has to match. Everything the Model then
	// keeps is the sanitized form — the group's name, its service names, the
	// fold lookup and the dedupe — so no label-derived byte reaches a render
	// site or a qualified key unfiltered. See sanitizeName.
	//
	// COLLISIONS are resolved in favour of the name that needed no sanitizing.
	// Ordering comes from the loader, which sorts RAW names, so a label crafted
	// to sanitize onto a real project's name can sort BEFORE it — and a plain
	// first-wins dedupe then dropped the real project and left the forged row
	// carrying an attacker-chosen ConfigDir under the real project's display
	// name, which is what the factory would have been handed on `d`/`r`/`s`.
	// Compose rejects every byte sanitizeName strips, so `raw == sanitized`
	// holds for anything compose itself could have produced: the authentic row
	// always wins, and only two forged rows can still race each other.
	add := func(p compose.Project) {
		raw := p.Name
		p.Name = sanitizeName(raw)
		if p.Name == "" {
			return
		}
		safe := raw == p.Name
		i, dup := at[p.Name]
		if dup && (authentic[p.Name] || !safe) {
			return
		}
		// Service names collapse the same way, and svcs is sorted but was never
		// deduped — two rows under one svcKey addressed each other's state.
		svcs := make([]string, 0, len(host[raw]))
		seenSvc := make(map[string]bool, len(host[raw]))
		for name := range host[raw] {
			name = sanitizeName(name)
			if name == "" || seenSvc[name] {
				continue
			}
			seenSvc[name] = true
			svcs = append(svcs, name)
		}
		g := svcGroup{proj: p, services: sortServices(svcs), folded: folded[p.Name]}
		authentic[p.Name] = safe
		if dup {
			groups[i] = g
			return
		}
		at[p.Name] = len(groups)
		groups = append(groups, g)
	}
	for _, p := range projects {
		add(p)
	}

	// The leftover pass is the "never leave a running container invisible"
	// safety net, so it pre-filters nothing: add() owns the dedupe, including
	// the authentic-wins rule, and a name already grouped simply returns there.
	var extra []string
	unmanagedExtra := false
	for name := range host {
		if name == "" {
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

// svcRowID names one container-screen row by WHAT it is rather than by where it
// sits. The row list is rebuilt wholesale — by the 5-second grouped reload, by a
// fold, by a search that unfolds a group — so an index is not an identity: a
// container started or removed anywhere on the host slides every index below it.
type svcRowID struct {
	proj    string
	service string // empty on a group header
	header  bool
}

// ok reports whether the ID names a real row. The zero value does not: it is
// what rowIDAt returns for an out-of-range cursor, and restoring from it would
// jump the cursor to whatever unnamed group happens to sort first.
func (id svcRowID) ok() bool { return id.proj != "" || id.service != "" }

// rowIDAt names the row at index i by identity. Out of range yields the zero
// value, which ok() rejects.
func rowIDAt(entries []svcEntry, groups []svcGroup, i int) svcRowID {
	if i < 0 || i >= len(entries) {
		return svcRowID{}
	}
	e := entries[i]
	proj := ""
	if e.groupIdx >= 0 && e.groupIdx < len(groups) {
		proj = groups[e.groupIdx].proj.Name
	}
	return svcRowID{proj: proj, service: e.name, header: e.kind == entrySvcGroupHeader}
}

// indexOfRowID finds the row the ID names in a REBUILT list. It reports false
// when the row is gone (its container was removed, its project disappeared, or
// its group folded), which is the caller's cue to fall back to a clamp.
//
// A header and a service row of the same project are different rows, so the
// kind is part of the match: a single-project host emits no header at all, and
// a header ID must not silently land on that project's first service.
func indexOfRowID(entries []svcEntry, groups []svcGroup, id svcRowID) (int, bool) {
	if !id.ok() {
		return 0, false
	}
	for i, e := range entries {
		if (e.kind == entrySvcGroupHeader) != id.header || e.name != id.service {
			continue
		}
		proj := ""
		if e.groupIdx >= 0 && e.groupIdx < len(groups) {
			proj = groups[e.groupIdx].proj.Name
		}
		if proj == id.proj {
			return i, true
		}
	}
	return 0, false
}
