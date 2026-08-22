package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/config"
	"github.com/lexxzar/compose-deploy/internal/runner"
)

// statsRefreshInterval is how often the container screen re-fetches status and
// stats while the user is viewing it. 5s balances responsiveness against the
// cost of the host-wide `docker stats --no-stream` call (~1.5s per fetch) plus
// the per-project `docker compose ps` call.
const statsRefreshInterval = 5 * time.Second

// updatesCacheTTL controls how long a CheckUpdates result is kept fresh in
// the in-memory TUI cache before a screen-entry triggers another fetch. The
// `U` key force-refreshes regardless. 10 minutes balances "freshness while
// the user is actively working" against "don't hammer the registry while the
// user re-enters the container screen repeatedly during a deploy session".
const updatesCacheTTL = 10 * time.Minute

// updatesErrorTTL is the short cache window for FAILED CheckUpdates fetches.
// A failure cache entry suppresses retries for this window so a persistent
// fault (SSH dead, registry down, auth issue) cannot drive a 5-second
// refetch loop via the statusMsg self-heal — every 5s tick would otherwise
// see an empty cache, queue a refresh, fail, repeat (10-min lingering user
// generates ~120 doomed CheckUpdates attempts × all services).
//
// The shorter TTL (vs updatesCacheTTL) preserves the "transient errors clear
// quickly" property: after a 30s blip the next screen activity refetches,
// instead of waiting the full 10-min success TTL. Bumped via context-change
// session resets and U keypress just like success entries.
const updatesErrorTTL = 30 * time.Second

// ConfigProvider provides access to docker-compose configuration files.
// Defined in the tui package (not runner) because it returns *exec.Cmd and
// is TUI-only. Both Compose and RemoteCompose implement it.
type ConfigProvider interface {
	ConfigFile(ctx context.Context) ([]byte, error)
	ConfigResolved(ctx context.Context) ([]byte, error)
	EditCommand(ctx context.Context) (*exec.Cmd, error)
	ValidateConfig(ctx context.Context) error
}

// ExecProvider provides interactive exec into a container.
// Defined in the tui package (like ConfigProvider) because it returns *exec.Cmd
// and is TUI/CLI-only — not a pipeline operation. Both Compose and RemoteCompose implement it.
type ExecProvider interface {
	ExecCommand(ctx context.Context, service string, command []string) (*exec.Cmd, error)
}

// Inspector returns the raw `docker inspect` JSON for one service's container.
// Declared in the tui package beside ConfigProvider/ExecProvider and
// type-asserted on the concrete composer — a composer that does not implement
// it (a test mock) makes the `i` key a silent no-op. All three composers
// satisfy it, including the read-only *compose.HostContainers: inspect is
// read-only by nature, so unlike every other recent container key it is NOT
// gated on m.readOnly().
type Inspector interface {
	Inspect(ctx context.Context, service string) ([]byte, error)
}

// Snapshotter records the currently-running image digests before a deploy so
// that `cdeploy rollback` can later restore them. Defined in the tui package
// (like ConfigProvider/ExecProvider) and type-asserted on the concrete composer
// — a composer that does not implement it (e.g. a test mock) simply skips
// snapshotting, so it is a best-effort capability rather than part of the
// runner.Composer contract. Both *compose.Compose and *compose.RemoteCompose
// satisfy it; the method set mirrors the cmd package's `snapshotter` seam.
type Snapshotter interface {
	SnapshotServices(ctx context.Context, services []string) (compose.SnapshotResult, error)
	WriteSnapshot(ctx context.Context, fresh *compose.Snapshot) error
}

// RollbackPreparer exposes the concrete-composer capability the `R` key needs:
// reading the host-side deploy snapshot and preparing a digest-pinned rollback
// (pull missing blobs, generate the override, set ExtraComposeFiles). Type-
// asserted on m.composer like Snapshotter / ConfigProvider / ExecProvider — a
// composer that does not implement it (e.g. a test mock) makes the `R` key a
// no-op, so it is a best-effort capability rather than part of the
// runner.Composer contract. Both *compose.Compose and *compose.RemoteCompose
// satisfy it; the method set mirrors the cmd package's rollbackPreparer seam.
type RollbackPreparer interface {
	ReadSnapshot(ctx context.Context) (*compose.Snapshot, error)
	PrepareRollback(ctx context.Context, entries map[string]compose.SnapshotEntry, services []string, w io.Writer) (func(), error)
}

// ReadOnlyComposer marks a composer whose write methods refuse. The method is
// NAMED and returns a bool rather than being a marker: a method-less interface
// is interface{}, which every composer satisfies, so m.readOnly() would be true
// everywhere. Only *compose.HostContainers implements it; the TUI uses it to
// gate the write keys (d/r/s/R/c/space/a), the selection widgets, the footer
// and the `?` overlay.
type ReadOnlyComposer interface {
	ReadOnlyComposer() bool
}

// ComposerFactory creates a runner.Composer for the given project. It takes the
// whole Project rather than a directory string because the synthetic unmanaged
// row carries Unmanaged: true and an empty ConfigDir, and the factory must
// branch on that to build a read-only *compose.HostContainers instead.
type ComposerFactory func(proj compose.Project) runner.Composer

// ProjectLoader loads the list of projects (local or remote).
type ProjectLoader func(ctx context.Context) ([]compose.Project, error)

// ConnectCallback is called when a remote server is selected. It returns
// the SSH connect command (for tea.ExecProcess), a ComposerFactory that takes
// the selected compose.Project, a ProjectLoader, and a disconnect function.
type ConnectCallback func(server config.Server) (connectCmd *exec.Cmd, factory ComposerFactory, loader ProjectLoader, disconnect func() error)

const warnNoSelection = "No service is selected"

// serverEntryKind distinguishes selectable items from visual group headers.
type serverEntryKind int

const (
	entryLocal       serverEntryKind = iota // "Local (this machine)"
	entryGroupHeader                        // non-selectable group label
	entryServer                             // remote server
)

// serverEntry is one row in the server picker (selectable or header).
type serverEntry struct {
	kind      serverEntryKind
	serverIdx int    // index into Model.servers; valid only for entryServer
	group     string // group label; valid only for entryGroupHeader
}

// buildServerEntries creates the display list: Local first, then servers
// grouped by Server.Group (preserving YAML order of first appearance).
// Ungrouped servers (empty Group) appear right after Local with no header.
func buildServerEntries(servers []config.Server) []serverEntry {
	entries := []serverEntry{{kind: entryLocal}}

	type group struct {
		name    string
		indices []int
	}
	var groups []group
	seen := map[string]int{}

	for i, s := range servers {
		if idx, ok := seen[s.Group]; ok {
			groups[idx].indices = append(groups[idx].indices, i)
		} else {
			seen[s.Group] = len(groups)
			groups = append(groups, group{name: s.Group, indices: []int{i}})
		}
	}

	// Ungrouped servers (empty group) first, right after Local
	for _, g := range groups {
		if g.name == "" {
			for _, idx := range g.indices {
				entries = append(entries, serverEntry{kind: entryServer, serverIdx: idx})
			}
		}
	}
	// Then named groups with headers
	for _, g := range groups {
		if g.name != "" {
			entries = append(entries, serverEntry{kind: entryGroupHeader, group: g.name})
			for _, idx := range g.indices {
				entries = append(entries, serverEntry{kind: entryServer, serverIdx: idx})
			}
		}
	}

	return entries
}

// nextSelectable returns the index of the next selectable entry after current.
func nextSelectable(entries []serverEntry, current int) int {
	for i := current + 1; i < len(entries); i++ {
		if entries[i].kind != entryGroupHeader {
			return i
		}
	}
	return current
}

// prevSelectable returns the index of the previous selectable entry before current.
func prevSelectable(entries []serverEntry, current int) int {
	for i := current - 1; i >= 0; i-- {
		if entries[i].kind != entryGroupHeader {
			return i
		}
	}
	return current
}

// screen represents the current TUI screen.
type screen int

const (
	screenSelectServer screen = iota
	screenSelectProject
	screenSelectContainers
	screenProgress
	screenLogs
	screenConfig
	screenSettingsList
	screenSettingsForm
	screenInspect
)

// Model is the Bubble Tea model for the cdeploy TUI.
type Model struct {
	screen screen

	// Config persistence
	configPath string         // path to servers.yml for Save()
	config     *config.Config // live config for settings editor

	// Screen: server select
	servers           []config.Server
	serverEntries     []serverEntry
	serverCursor      int
	serverErr         error
	serverName        string // selected server name, for breadcrumbs
	serverHost        string // selected server host, for status bar
	serverColor       string // selected server color, for status bar
	connectCb         ConnectCallback
	disconnectFunc    func() error
	projectLoader     ProjectLoader
	preselectedServer int  // index into servers for --server flag
	hasPreselection   bool // true when --server was specified

	// Local state (preserved across server selection changes)
	localComposer      runner.Composer
	localFactory       ComposerFactory
	localProjectLoader ProjectLoader

	// Screen: project select
	projects        []compose.Project
	projCursor      int
	projErr         error
	projName        string // selected project name, for breadcrumbs
	showPicker      bool   // true if the project picker was shown
	composerFactory ComposerFactory
	projectsSession uint64 // bumped at every transition that swaps the projectLoader (server pick, server disconnect, local fast-track, etc.) so a stale loadProjects from server A can't overwrite server B's list

	// Screen 1: service select
	services        []string
	svcStatus       map[string]runner.ServiceStatus // service name → status
	stats           map[string]runner.ServiceStats  // service name → resource usage; populated asynchronously by refreshStats
	statsErr        error                           // last error from ContainerStats; rendered in the same slot as svcErr (svcErr wins)
	statsSession    uint64                          // bumped before every refreshStats() and on context change so older in-flight responses are filtered out
	statusSession   uint64                          // mirror of statsSession for refreshStatus(); without it, periodic-tick statusMsg from project A could overwrite the svcStatus map after the user has navigated to project B
	statsRequested  bool                            // true once refreshStats has been requested for the current container-screen entry; reserves CPU/Mem column widths so captions don't pop in when data arrives
	refreshInFlight bool                            // true while a periodic-tick refreshStats fetch is pending; prevents the next tick from stacking another fetch on top of a slow docker stats / SSH call
	tickCmdOverride func() tea.Cmd                  // test seam: when non-nil, replaces tea.Tick in refreshTick() with a non-blocking Cmd; production code never sets this

	// Update-available indicator state. CheckUpdates is run on entry to
	// screenSelectContainers (when the cache is stale) and explicitly via `U`.
	// Cache is in-memory and process-local — not persisted across cdeploy
	// runs. updateCache key is projectDir + "|" + serverName (empty serverName
	// = local). projDir is empty for the local-fast-track entry (no project
	// picker), giving the local-no-picker context a single cache slot of "".
	updateCache    map[string]updateEntry
	updatesSession uint64 // mirror of statsSession — bumped at the 7 context-change sites so a stale CheckUpdates response can't hydrate UpdateAvailable into a different project's svcStatus map
	updateInFlight bool   // mirror of refreshInFlight for refreshUpdates — prevents a slow CheckUpdates from stacking on the next screen entry / `U` press
	updatesErr     string // last error from CheckUpdates; rendered as soft warning below statsErr (priority: svcErr > statsErr > updatesErr)
	projDir        string // active project's config dir; used for the updateCache key
	selected       map[int]bool
	svcCursor      int
	svcOffset      int // index of first visible service in scroll window
	svcErr         error

	// Confirmation state (within container screen)
	confirming  bool
	pendingOp   runner.Operation
	pendingExec bool
	warning     string

	// Quit confirmation state (for remote connections)
	quitting bool

	// Key-reference overlay (`?`). A flag, not a screen: no back-navigation,
	// no session counter, no departure checklist. View() renders viewHelp()
	// for m.screen while it is set.
	helpOpen bool

	// Screen 2: progress
	steps        []stepState
	logContent   string
	logViewport  viewport.Model
	spinner      spinner.Model
	done         bool
	failed       bool
	eventCh      <-chan runner.StepEvent
	cancel       context.CancelFunc
	opContainers []string // the operation's resolved target services; seeds the wait target set

	// Screen 2: progress — health-wait sub-state. After a Deploy/Restart/Rollback
	// pipeline completes (m.done), the progress screen enters a waiting sub-state
	// that polls ContainerStatus and drives runner.EvaluateWait until every
	// targeted service resolves (or the timeout elapses). StopOnly never waits.
	// All fields are reset via clearWaitState() at every departure site and on
	// esc-skip (which also bumps waitSession to invalidate in-flight polls/ticks).
	waiting          bool             // actively polling for health (esc = skip)
	waitState        runner.WaitState // per-service verdict accumulator; non-empty Services ⇒ verdicts are rendered
	waitSession      uint64           // gates waitTickMsg/waitStatusMsg against a stale poll after esc-skip / departure
	waitDeadline     time.Time        // wall-clock deadline; drives the countdown and the elapsed input to EvaluateWait
	waitTickOverride func() tea.Cmd   // test seam: replaces tea.Tick in waitTick(); production never sets this

	// Rollback (`R` key) support. rollbackSnapshot holds the host-side snapshot
	// fetched by `R` so the confirm prompt can show its age and PrepareRollback
	// can pin the digests. rollbackTargets is the selected service set CAPTURED
	// at `R`-press time — the async snapshot fetch means the live multi-select
	// may change or clear before rollbackSnapshotMsg lands, so the captured set
	// (never the live selection) drives validation, the confirm prompt, prep, and
	// the pipeline; an empty target set must NEVER reach the runner (empty == all
	// services). rollbackFetchSession gates the async rollbackSnapshotMsg (bumped
	// on `R` press and at the same context-change sites as the other sessions),
	// which also protects the captured rollbackTargets from a stale fetch.
	// rollbackCleanup is PrepareRollback's cleanup (override-file removal +
	// ExtraComposeFiles reset), invoked when LEAVING screenProgress — NEVER
	// goroutine-deferred (see the rollback cleanup timing race rule). rollbackErr
	// surfaces a prep failure on the progress screen. rollbackSnapshot and
	// rollbackTargets are cleared together at every documented departure site.
	rollbackSnapshot     *compose.Snapshot
	rollbackTargets      []string
	rollbackFetchSession uint64
	rollbackCleanup      func()
	rollbackErr          string

	// Screen: logs
	logsService  string             // service being viewed
	logsViewport viewport.Model     // dedicated viewport for log screen
	logsCancel   context.CancelFunc // cancels the log goroutine; derived from m.ctx
	logsDone     bool               // true when streaming finished
	logsErr      error              // error from Logs() call
	logsPipeR    io.Reader          // pipe reader for log streaming
	logsSession  uint64             // monotonic counter to discard stale messages from prior sessions
	logsWrap     bool               // soft-wrap long lines at viewport width
	logsPretty   bool               // pretty-print JSON log bodies

	// Screen: logs — raw-line buffer (source of truth for the viewport derivation)
	logsRawLines  []string // complete logical lines, unfiltered
	logsPartial   string   // trailing incomplete line (no newline yet)
	logsScanned   int      // raw-line scan cursor: count of logsRawLines already folded into logsFormatted (a resume point, NOT a survivor count)
	logsFormatted string   // cached formatted output for the scanned raw lines
	logsErrLine   string   // filter-exempt terminal error text; always rendered regardless of an active filter

	// Screen: logs — filter (live grep; see clearLogFilter/logFilterPred/recomputeLogFilter)
	logFiltering            bool            // filter bar open, capturing text
	logFilterInput          textinput.Model // (re)constructed lazily in the "f" open handler
	logFilterQuery          string          // last-good committed query; != "" ⇒ filter active
	logFilterIsRegex        bool            // live/desired mode; ctrl+r toggles (regex vs. substring)
	logFilterCommittedRegex bool            // whether the last-good committed query is a regex; source of truth for regex-ness in logFilterPred (NOT the live logFilterIsRegex)
	logFilterShown          int             // running count of survivor lines folded into logsFormatted; maintained incrementally by applyLogFormat/fullReformat. Read by logFilterCounts as the committed-filter survivor count (O(1) per frame, display-meaningful only when logFilterQuery != ""), AND by applyLogFormat as the seed-vs-append gate (== 0 means logsFormatted holds no survivor yet — a delta, even an empty blank-line delta, seeds it; else it joins with "\n"). logsFormatted == "" is ambiguous (empty when only blank survivors folded), so this count is the authoritative "have we folded any line" signal.

	// Screen: logs — search-within-highlight (see clearLogSearch/logSearchPred/recomputeLogSearch)
	logSearching            bool            // search bar open, capturing text
	logSearchInput          textinput.Model // (re)constructed lazily in the "/" open handler
	logSearchQuery          string          // live/committed query; != "" ⇒ highlights + n/N active
	logSearchIsRegex        bool            // live/desired mode; ctrl+r toggles (regex vs. substring)
	logSearchCommittedRegex bool            // whether the committed query is a regex; source of truth for regex-ness in logSearchPred (NOT the live logSearchIsRegex)
	logSearchMatches        []int           // PHYSICAL line indices of matches, ascending (recomputed at SetContent time)
	logSearchCur            int             // index into logSearchMatches (the current match)

	// Screen: config
	configContent  []byte         // raw compose file content
	configResolved []byte         // resolved/interpolated config (cached)
	configViewport viewport.Model // viewport for config content
	configShowRes  bool           // true = showing resolved, false = showing raw
	configErr      error          // error from config operations
	configValid    *bool          // nil = not checked, true = valid, false = invalid
	configValidMsg string         // validation error message
	configSession  uint64         // monotonic counter for stale message rejection

	// Screen: inspect
	inspectService  string         // service whose container is being inspected
	inspectRaw      []byte         // raw `docker inspect` JSON, verbatim (raw mode)
	inspectSummary  string         // rendered curated summary (cached; rebuilt on resize)
	inspectShowRaw  bool           // false = summary (default), true = raw JSON
	inspectViewport viewport.Model // viewport for whichever mode is active
	inspectErr      error          // fetch or parse failure
	inspectSession  uint64         // monotonic counter for stale message rejection

	// Screen: settings list
	settingsCursor int  // cursor in settings list
	settingsDelete bool // inline delete confirmation active

	// Screen: settings form
	settingsEditing int // -1 = add new, >=0 = edit index into config.Servers
	settingsField   int // focused field: 0-3 = text inputs, 4 = color picker
	settingsInputs  [4]textinput.Model
	settingsColor   string // currently selected color value
	settingsErr     string // validation / save error message

	// Screen: container search (search & jump — see clearSearch/computeMatches)
	searching     bool            // search bar open, capturing text
	searchInput   textinput.Model // (re)constructed in the "/" open handler
	searchQuery   string          // current query; != "" ⇒ highlights + n/N live
	searchMatches []int           // indices into m.services that match (cached)
	searchReturn  int             // svcCursor to restore on esc-while-typing

	// Shared
	ctx       context.Context
	ctxCancel context.CancelFunc
	composer  runner.Composer
	logWriter io.Writer
	width     int
	height    int
}

type stepState struct {
	name   string
	status string // "", "running", "done", "failed"
}

// Messages
type stepEventMsg runner.StepEvent
type projectsMsg struct {
	projects []compose.Project
	err      error
	session  uint64 // captured from m.projectsSession at fetch time; without it, a stale load from server A could overwrite the project list after the user has switched to server B
}
type servicesMsg struct {
	services []string
	status   map[string]runner.ServiceStatus
	err      error
	session  uint64 // reuses m.statusSession: loadServices fetches both list + initial status, so it lives in the same context lifecycle as refreshStatus
}
type statusMsg struct {
	status  map[string]runner.ServiceStatus
	err     error
	session uint64
}
type statsMsg struct {
	stats   map[string]runner.ServiceStats
	err     error
	session uint64
}

// updateEntry holds one cached CheckUpdates result. results is a partial map
// (services with verdicts only) — absent services are tri-state "unknown".
//
// Errors ARE cached, but with a much shorter TTL (updatesErrorTTL ~30s vs.
// updatesCacheTTL 10m). The error entry has results=nil and err=true. This
// gives us two properties at once:
//
//  1. No 5-second tight retry loop on persistent failure. Without the cached
//     failure, every 5s statusMsg tick would self-heal: empty cache →
//     queue refresh → fail → empty cache → repeat. On a slow remote SSH hop
//     or a Docker Hub outage that's ~120 failed CheckUpdates attempts in 10
//     minutes (multiplied by services per project), hammering the registry.
//
//  2. Transient errors clear quickly. After ~30s the cache entry ages out
//     and the next screen activity (or self-heal) refetches — much sooner
//     than the 10-minute success TTL.
//
// A new error entry also overwrites any prior successful entry for the same
// key (don't trust stale success once the latest refresh disagreed). The
// hydration paths (servicesMsg / statusMsg cache-replay) check err and skip
// hydration for failure entries, so cached failures never paint glyphs.
type updateEntry struct {
	fetchedAt time.Time
	results   map[string]bool
	err       bool
	// errMsg is the original error text from CheckUpdates. It's only set when
	// err == true. Stored alongside the failure flag so cache-hit paths
	// (maybeRefreshUpdatesCmd, servicesMsg/statusMsg cache replays) can
	// restore m.updatesErr on re-entry within updatesErrorTTL — otherwise a
	// user who navigates away from the container screen and back (which
	// clears updatesErr) would see blank glyphs with no warning, even though
	// the system knows the data is stale-bad.
	errMsg string
}

// updatesMsg carries the result of a refreshUpdates fetch. session is
// captured at fetch time and compared against m.updatesSession in the
// handler to drop stale responses from a previous project/server context.
type updatesMsg struct {
	results map[string]bool
	err     error
	session uint64
}

// refreshTickMsg fires every statsRefreshInterval to drive periodic
// status+stats refresh on screenSelectContainers. It's a singleton tick: the
// handler always reschedules itself, so exactly one tick is in flight at any
// time regardless of how many transitions into the screen happen.
type refreshTickMsg struct{}

type pipelineDoneMsg struct{}

// waitTickMsg fires after runner.DefaultWaitPoll to schedule the next health
// poll. session is captured at scheduling time and compared against
// m.waitSession in the handler so a tick left over from a skipped/departed wait
// is dropped. Singleton reschedule discipline like refreshTickMsg, but the loop
// stops once the wait resolves (not an unconditional forever-loop).
type waitTickMsg struct{ session uint64 }

// waitStatusMsg carries one ContainerStatus poll result into the wait reducer.
// session mirrors waitTickMsg so a poll from a skipped/departed wait is dropped.
type waitStatusMsg struct {
	status  map[string]runner.ServiceStatus
	err     error
	session uint64
}

// rollbackSnapshotMsg carries the result of the async ReadSnapshot fired by the
// `R` key. session is captured at fetch time and compared against
// m.rollbackFetchSession so a fetch that outlived its container-screen visit
// (the user navigated away, or pressed `R` again) is dropped. live is the CURRENT
// compose service set (from ListServices), fetched alongside the snapshot so the
// handler can drop a selected target that has since been removed from the compose
// file — mirroring the CLI's filterLiveTargets so a stale snapshot entry can't
// resurrect a removed service as an image-only override. It is populated only when
// err == nil and the snapshot is non-empty.
type rollbackSnapshotMsg struct {
	snap    *compose.Snapshot
	live    []string
	err     error
	session uint64
}

// rollbackPreppedMsg carries the outcome of PrepareRollback (run before the
// Rollback pipeline). On success cleanup is the deferred override-file / -f
// reset — stored on the Model and invoked when LEAVING screenProgress, never
// goroutine-deferred (see the rollback cleanup timing race rule). On failure err
// is set and the op is marked failed before any pipeline step runs.
type rollbackPreppedMsg struct {
	cleanup func()
	err     error
}

type logChunkMsg struct {
	data    []byte
	session uint64
}
type logDoneMsg struct {
	err     error
	session uint64
}
type execDoneMsg struct{ err error }
type connectResultMsg struct{ err error }
type preselectedConnectMsg struct{}
type disconnectDoneMsg struct{}
type configFileMsg struct {
	data    []byte
	err     error
	session uint64
}
type configResolvedMsg struct {
	data    []byte
	err     error
	session uint64
}
type configEditDoneMsg struct {
	err     error
	session uint64
}
type configValidateMsg struct {
	err     error
	session uint64
}
type inspectDataMsg struct {
	data    []byte
	err     error
	session uint64
}

// NewModel creates a new TUI model.
//
// Decision table for starting screen:
//
//	servers!=nil               -> screenSelectServer  (always show picker)
//	servers=nil + composer=nil -> screenSelectProject
//	servers=nil + composer!=nil -> screenSelectContainers
//
// Option configures optional Model behavior.
type Option func(*Model)

// WithPreselectedServer makes the TUI skip the server picker and auto-connect
// to the server at the given index. The index refers to the servers slice
// passed to NewModel.
func WithPreselectedServer(idx int) Option {
	return func(m *Model) {
		m.preselectedServer = idx
		m.hasPreselection = true
	}
}

// WithLocalProjectLoader sets the project loader used for local project discovery.
// This replaces the default compose.ListProjects fallback with a loader that
// respects standalone docker-compose detection.
func WithLocalProjectLoader(loader ProjectLoader) Option {
	return func(m *Model) {
		m.localProjectLoader = loader
		m.projectLoader = loader
	}
}

// WithConfigPath sets the file path used by the settings editor to save config changes.
func WithConfigPath(path string) Option {
	return func(m *Model) {
		m.configPath = path
	}
}

// WithConfig sets the live Config used by the settings editor for CRUD operations.
func WithConfig(cfg *config.Config) Option {
	return func(m *Model) {
		m.config = cfg
	}
}

func NewModel(composer runner.Composer, logWriter io.Writer, factory ComposerFactory, servers []config.Server, connectCb ConnectCallback, opts ...Option) Model {
	s := spinner.New()
	s.Spinner = spinner.Dot

	ctx, cancel := context.WithCancel(context.Background())

	m := Model{
		composerFactory: factory,
		localComposer:   composer,
		localFactory:    factory,
		servers:         servers,
		serverEntries:   buildServerEntries(servers),
		connectCb:       connectCb,
		selected:        make(map[int]bool),
		spinner:         s,
		ctx:             ctx,
		ctxCancel:       cancel,
		composer:        composer,
		logWriter:       logWriter,
	}

	for _, opt := range opts {
		opt(&m)
	}

	// Determine start screen after options are applied (config may be set).
	if len(servers) > 0 || m.config != nil {
		m.screen = screenSelectServer
	} else if composer == nil {
		m.screen = screenSelectProject
		m.showPicker = true
	} else {
		m.screen = screenSelectContainers
		m.statsRequested = true
		m.refreshInFlight = true // Init() will fire refreshStats; statsMsg arrival clears the flag
		// Init() fires refreshUpdates on the fast-path only when the composer
		// permits an automatic check; a read-only one waits for U. The flag
		// must track that decision — a true with no fetch behind it never
		// clears, and maybeRefreshUpdatesCmd would refuse every later fetch.
		m.updateInFlight = m.autoUpdatesAllowed()
	}

	return m
}

func (m Model) Init() tea.Cmd {
	// Always start the periodic refresh tick — the handler gates on screen,
	// so it's a no-op until the user reaches screenSelectContainers, but
	// starting from Init means we never have to remember to kick it off in
	// each individual screen transition.
	tick := m.refreshTick()

	if m.screen == screenSelectServer {
		if m.hasPreselection && m.preselectedServer >= 0 && m.preselectedServer < len(m.servers) {
			return tea.Batch(func() tea.Msg { return preselectedConnectMsg{} }, tick)
		}
		return tick // server list is static from config; only the tick is live
	}
	if m.showPicker {
		return tea.Batch(m.loadProjects(), tick)
	}
	// Fast-path: standalone container screen on launch. updateInFlight was
	// set by NewModel for the standalone case (mirrors refreshInFlight), so
	// just fire the cmd directly. Cache is empty on first launch so no need
	// for the maybe-fetch helper here — but the opt-in gate still applies:
	// a read-only composer handed straight to NewModel must not fan out to
	// the registry before the user presses U. NewModel leaves updateInFlight
	// clear in that case, so the two stay in step.
	cmds := []tea.Cmd{m.loadServices(), m.refreshStats()}
	if m.autoUpdatesAllowed() {
		cmds = append(cmds, m.refreshUpdates())
	}
	return tea.Batch(append(cmds, tick)...)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if m.screen == screenProgress {
			m.logViewport.Width = msg.Width - 4
			h := msg.Height - len(m.steps) - 8
			if h < 3 {
				h = 3
			}
			m.logViewport.Height = h
		}
		if m.screen == screenConfig {
			m.configViewport.Width = msg.Width - 4
			h := msg.Height - 6
			if h < 3 {
				h = 3
			}
			m.configViewport.Height = h
		}
		if m.screen == screenInspect {
			// The same floors enterInspect applies, so the two sizing sites
			// cannot disagree: an unguarded width goes negative on a very
			// narrow pane, and buildInspectSummary would then wrap to its
			// 80-column fallback for a viewport that renders nothing.
			w := msg.Width - 4
			if w < 10 {
				w = 40
			}
			m.inspectViewport.Width = w
			// -6, the config sizing. NOT the logs -7, which reserves a row for
			// the log bar the inspect screen does not have.
			h := msg.Height - 6
			if h < 3 {
				h = 3
			}
			m.inspectViewport.Height = h
			// The summary is wrapped to the viewport width, so it has to be
			// rebuilt from the raw bytes — which survive, keeping raw mode
			// byte-identical to `docker inspect` across a resize.
			m.rebuildInspectSummary()
			m.setInspectContent()
		}
		if m.screen == screenSelectContainers {
			m.fixSvcOffset()
			if m.searching {
				// Refresh the search input's horizontal-scroll viewport for the
				// new width — otherwise a narrower resize leaves a stale (right-
				// clipped) viewport until the next keystroke. searchInputWidth()
				// returns 0 when m.width<=0 (unbounded), so this is a safe no-op
				// on a zero-size resize.
				m.searchInput.Width = m.searchInputWidth()
			}
		}
		if m.screen == screenLogs {
			following := m.logsViewport.AtBottom()
			m.logsViewport.Width = msg.Width - 4
			// -7 (not -6): one row reserved for the log bar line. The config
			// branch above keeps -6 — do NOT unify these.
			h := msg.Height - 7
			if h < 3 {
				h = 3
			}
			m.logsViewport.Height = h
			m.fullReformat()
			if following {
				m.logsViewport.GotoBottom()
			}
			// Refresh an open input's horizontal-scroll window for the new width —
			// otherwise a narrower resize leaves a stale (right-clipped) viewport
			// until the next keystroke (mirrors the screenSelectContainers branch).
			if m.logFiltering {
				m.logFilterInput.Width = m.logFilterInputWidth()
			}
			if m.logSearching {
				m.logSearchInput.Width = m.logSearchInputWidth()
			}
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case projectsMsg:
		// Drop responses from a stale projectLoader (server A's load lands after
		// the user has switched to server B). Same race class as
		// statusMsg/statsMsg/servicesMsg — without the guard, m.projects would
		// be populated from server A while m.composerFactory now points at B,
		// so selecting a project would feed B's factory an A-discovered path.
		if m.screen != screenSelectProject || msg.session != m.projectsSession {
			return m, nil
		}
		if msg.err != nil {
			m.projErr = msg.err
			return m, nil
		}
		m.projErr = nil
		m.projects = msg.projects
		m.projCursor = 0
		return m, nil

	case servicesMsg:
		// Drop responses from a previous context (same race class as statusMsg/statsMsg):
		// loadServices runs ListServices + ContainerStatus, so a stale response can
		// overwrite both `services` and `svcStatus` for the new context. Sharing
		// statusSession is intentional — loadServices is the initial-fetch sibling
		// of refreshStatus and lives in the same lifecycle.
		if m.screen != screenSelectContainers || msg.session != m.statusSession {
			return m, nil
		}
		if msg.err != nil {
			m.svcErr = msg.err
			return m, nil
		}
		m.svcErr = nil
		m.services = sortServices(msg.services)
		m.svcStatus = msg.status
		m.selected = make(map[int]bool)
		m.svcCursor = 0
		m.svcOffset = 0
		// Full reload: searchMatches holds indices into the OLD m.services, so a
		// committed search is invalid once the list is replaced. Clear it (search
		// is ephemeral and re-opened with `/` if the user still wants it).
		m.clearSearch()
		// Re-apply any cached update verdicts so cached glyphs survive the
		// status-map overwrite. Race-safe regardless of whether the synthetic
		// cache-hit updatesMsg arrives before or after this messsage. On a
		// fresh CACHED FAILURE entry (within updatesErrorTTL) restore the
		// warning text from the cache too — otherwise periodic re-entries
		// or initial-load arrivals would silently drop the soft warning
		// while the same failure is still known-bad.
		if entry, fresh := m.updatesCacheLookup(); fresh {
			if entry.err {
				m.updatesErr = entry.errMsg
			} else if entry.results != nil {
				m.hydrateUpdates(entry.results)
			}
		}
		return m, nil

	case statusMsg:
		// Drop responses from a previous context — without this guard, a
		// periodic-tick refreshStatus from project A could land after the user
		// has navigated to project B and silently overwrite the new svcStatus
		// map. Mirrors the statsSession filter on statsMsg.
		if m.screen != screenSelectContainers || msg.session != m.statusSession {
			return m, nil
		}
		if msg.err != nil {
			m.svcErr = msg.err
			return m, nil
		}
		m.svcErr = nil
		m.svcStatus = msg.status
		// Re-apply cached update verdicts (status fetch wipes UpdateAvailable
		// fields when overwriting the map). Same rationale as servicesMsg.
		// On a fresh CACHED FAILURE entry (within updatesErrorTTL) restore
		// the warning text so the periodic status tick doesn't silently
		// drop the soft warning while the same failure is still known-bad.
		entry, fresh := m.updatesCacheLookup()
		if fresh {
			if entry.err {
				m.updatesErr = entry.errMsg
			} else if entry.results != nil {
				m.hydrateUpdates(entry.results)
			}
		} else if !m.autoUpdatesAllowed() {
			// The failure entry the warning described has expired and no
			// automatic refetch will replace it here (U is the only trigger),
			// so the warning is no longer current — drop it instead of leaving
			// it on screen for the life of the read-only view. Cleared before
			// fixSvcOffset below, which re-clamps the row the warning freed.
			m.updatesErr = ""
		}
		m.fixSvcOffset()
		// Self-heal: if no fresh cache entry exists (TTL expired or never
		// fetched) and no refreshUpdates is in flight, queue a fresh fetch.
		// Without this, a user who lingers on the container screen past the
		// TTL window would see update glyphs vanish silently on the next
		// periodic statusMsg — the overwrite above wipes UpdateAvailable
		// and the cache lookup misses, so nothing replaces them. Mirror
		// maybeRefreshUpdatesCmd's in-flight guard discipline so we never
		// stack a second fetch, and its autoUpdatesAllowed gate — otherwise
		// the read-only screen would fire the unbounded host-wide registry
		// fan-out on every 5-second status tick instead of never.
		if !fresh && !m.updateInFlight && m.composer != nil && m.autoUpdatesAllowed() {
			m.updateInFlight = true
			return m, m.refreshUpdates()
		}
		return m, nil

	case statsMsg:
		// Stale-session responses are unconditionally dropped; they don't touch
		// in-flight state either (the bump at the context-change site already
		// reset refreshInFlight).
		if msg.session != m.statsSession {
			return m, nil
		}
		// Clear the in-flight guard FIRST, before the screen check. If the user
		// navigated to an off-screen view (config, logs, progress) while a
		// fetch was pending, the response would otherwise be discarded silently
		// and leave refreshInFlight stuck at true, permanently suppressing
		// further periodic refreshes when the user returns. Guard is per-session,
		// so clearing it on the current-session response is always correct.
		m.refreshInFlight = false
		if m.screen != screenSelectContainers {
			return m, nil
		}
		if msg.err != nil {
			// Clear m.stats alongside setting statsErr so the screen never shows
			// stale CPU/Mem cells next to a "Stats unavailable" warning — the
			// contradiction would confuse users into trusting the stale values.
			m.statsErr = msg.err
			m.stats = nil
			m.fixSvcOffset()
			return m, nil
		}
		m.statsErr = nil
		m.stats = msg.stats
		m.fixSvcOffset()
		return m, nil

	case updatesMsg:
		// Clear the in-flight guard FIRST — before BOTH the session check
		// and the screen check. Two scenarios motivate clearing on stale
		// arrivals as well as current ones:
		//   (a) Off-screen current-session: the user opened logs/config/
		//       progress while the fetch was pending. The response arrives
		//       off-screen; without clearing first, the guard would stick
		//       on, silently blocking the next maybeRefreshUpdatesCmd /
		//       U keypress when the user returns to the container screen.
		//   (b) Stale session: the user pressed U (in-flight=true),
		//       navigated away, and the bump moved the session forward.
		//       The original fetch returns with the OLD session; without
		//       clearing first, the guard stays on forever — every
		//       subsequent U keypress is silently swallowed by its own
		//       in-flight guard, and maybeRefreshUpdatesCmd on cache hits
		//       skips the fetch too. Clearing on any arrival is safe
		//       because the cache write below is still gated on session
		//       match (so stale results don't pollute the cache), and the
		//       new-context handler that bumped the session also fires its
		//       own refreshUpdates (which sets the flag back to true if a
		//       new fetch is actually in flight). Worst case: a stale
		//       arrival clears the flag while a new fetch is also pending
		//       — then a U keypress could stack a second fetch, which is
		//       wasteful but harmless (session check still drops the
		//       stale one).
		m.updateInFlight = false
		if msg.session != m.updatesSession {
			return m, nil
		}
		// Cache BOTH successful results and failures, with different TTLs.
		// Success entries live for updatesCacheTTL (10m); failure entries
		// for updatesErrorTTL (~30s). A new entry always overwrites the
		// prior one for the same key — a fresh failure evicts a stale
		// success (don't trust outdated verdicts the latest refresh
		// disagreed with), and a fresh success replaces a cached failure.
		//
		// Caching failures (with a short TTL) prevents the 5-second tight
		// retry loop that would otherwise emerge from the statusMsg
		// self-heal: every 5s tick → empty cache → queue refresh → fail
		// → empty cache → repeat. With a short-TTL failure entry, the
		// self-heal sees a "fresh" hit and skips the refetch until the
		// 30s window expires, at which point a retry is appropriate.
		key := m.updatesCacheKey()
		if m.updateCache == nil {
			m.updateCache = make(map[string]updateEntry)
		}
		if msg.err == nil {
			m.updateCache[key] = updateEntry{
				fetchedAt: time.Now(),
				results:   msg.results,
			}
		} else {
			m.updateCache[key] = updateEntry{
				fetchedAt: time.Now(),
				results:   nil,
				err:       true,
				errMsg:    msg.err.Error(),
			}
		}
		// State mutations below happen regardless of which screen the user is
		// on: m.svcStatus is the source of truth that the next entry to
		// screenSelectContainers (re-)renders from, so leaving stale
		// UpdateAvailable glyphs in it during an off-screen window would let
		// them resurface when the user returns — e.g. from the config screen,
		// which is read-only and does NOT trigger refreshStatus, so nothing
		// downstream would clear them. Similarly updatesErr must be set
		// off-screen so the soft warning is in place the moment the user
		// returns. Only fixSvcOffset is screen-coupled (it touches scroll
		// offsets only meaningful while the container list is rendered).
		if msg.err != nil {
			m.updatesErr = msg.err.Error()
			// Clear UpdateAvailable for every service: when the latest
			// fetch failed we can't trust ANY of the previous verdicts
			// (the registry/SSH path is broken). Leaving stale glyphs
			// alongside the "updates unavailable" warning is confusing
			// — the soft warning says "data is bad" while the glyphs
			// say "here's the data." Discarding partial verdicts on
			// error matches the runner.Composer contract.
			for svc, st := range m.svcStatus {
				st.UpdateAvailable = nil
				m.svcStatus[svc] = st
			}
			// Setting updatesErr adds a soft-warning footer line, which
			// shrinks svcVisibleCount() by one. If the cursor was sitting
			// at the previously-last-visible row, it now falls outside the
			// window. Call fixSvcOffset off-screen too — the helper only
			// touches svcOffset based on svcCursor/services/visibleCount,
			// none of which are screen-coupled — so the user returns to a
			// correctly-scrolled list instead of an off-screen cursor that
			// only snaps back on the next keypress. The config screen path
			// is the worst case (no refreshStatus on return).
			m.fixSvcOffset()
			return m, nil
		}
		m.updatesErr = ""
		// Fresh successful refresh: clear UpdateAvailable for every service
		// in svcStatus BEFORE hydrating so verdicts the new result omits
		// (build-only this time, failed lookup, removed from compose) lose
		// their stale glyph. Cache-replay paths in servicesMsg/statusMsg
		// stay purely additive — they're not introducing new data, just
		// preserving existing verdicts across a status-map overwrite.
		for svc, st := range m.svcStatus {
			st.UpdateAvailable = nil
			m.svcStatus[svc] = st
		}
		m.hydrateUpdates(msg.results)
		// Clearing updatesErr (when previously set) frees a footer line and
		// grows svcVisibleCount() by one. Run fixSvcOffset unconditionally
		// — also covers the symmetric off-screen success case, and the
		// helper is a cheap clamp when nothing actually changed.
		m.fixSvcOffset()
		return m, nil

	case refreshTickMsg:
		// Always reschedule the next tick to keep this a singleton background
		// loop. Only fire actual refreshes when the user is on the container
		// screen, not mid-confirmation, and no previous refresh is still in
		// flight — on a slow remote SSH hop docker stats can take longer than
		// statsRefreshInterval (5s), and without the m.refreshInFlight gate
		// the next tick would stack another pair of fetches on top.
		if m.screen != screenSelectContainers || m.confirming || m.composer == nil || m.refreshInFlight {
			return m, m.refreshTick()
		}
		m.refreshInFlight = true
		return m, tea.Batch(m.refreshStatus(), m.refreshStats(), m.refreshTick())

	case stepEventMsg:
		return m.handleStepEvent(runner.StepEvent(msg))

	case preselectedConnectMsg:
		server := m.servers[m.preselectedServer]
		m.serverName = server.Name
		m.serverHost = server.Host
		m.serverColor = m.resolveServerColor(server)
		connectCmd, factory, loader, disconnect := m.connectCb(server)
		m.composerFactory = factory
		m.projectLoader = loader
		m.projectsSession++
		m.disconnectFunc = disconnect
		return m, tea.ExecProcess(connectCmd, func(err error) tea.Msg {
			return connectResultMsg{err: err}
		})

	case connectResultMsg:
		// Close the overlay on both branches. Neither is reachable with it open
		// today (the connect runs through tea.ExecProcess, which suspends key
		// input, and the overlay swallows the enter that starts a connect), but
		// the success branch reassigns m.screen and an overlay left open across
		// that would silently swap its key table. Departure-site cleanup here is
		// uniform with quitting / clearSearch / clearWaitState below.
		m.helpOpen = false
		if msg.err != nil {
			m.serverErr = msg.err
			m.composerFactory = m.localFactory
			m.projectLoader = m.localProjectLoader
			m.projectsSession++
			m.disconnectFunc = nil
			m.quitting = false
			// Clear ALL transient state from the failed connect attempt: name,
			// host, color, and update-detection flags. Without these, a
			// subsequent connect attempt to a different server would inherit
			// stale serverName (used in updatesCacheKey) and a leaked
			// updateInFlight that would suppress the next periodic refresh.
			m.serverName = ""
			m.serverHost = ""
			m.serverColor = ""
			m.updatesErr = ""
			m.updateInFlight = false
			// Clear rollback state too: a container-screen R fetch cannot have
			// been in flight during a connect attempt, but keep the departure-site
			// cleanup discipline uniform (mirrors clearSearch/clearWaitState).
			m.rollbackSnapshot = nil
			m.rollbackTargets = nil
			m.rollbackCleanup = nil
			m.rollbackFetchSession++
			m.clearSearch()
			m.clearWaitState()
			return m, nil
		}
		m.serverErr = nil
		m.showPicker = true
		m.screen = screenSelectProject
		// Note: only projectsSession is bumped here (above, via the no-op
		// initial reset). statsSession / statusSession / updatesSession are
		// intentionally NOT bumped at this site because the user has not yet
		// entered a context that uses those counters — m.composer is still
		// nil until a project is picked. The bumps for those three counters
		// happen at the project-pick handler (below) where m.composer is
		// swapped in, matching the "session is bumped at every site that
		// swaps the resource the counter guards" discipline.
		return m, m.loadProjects()

	case disconnectDoneMsg:
		return m, nil

	case logChunkMsg:
		if m.screen != screenLogs || msg.session != m.logsSession {
			return m, nil
		}
		following := m.logsViewport.AtBottom() // capture BEFORE appending
		m.appendRawChunk(msg.data)
		m.applyLogFormat() // SetContent preserves YOffset
		if following {
			m.logsViewport.GotoBottom()
		}
		return m, m.readLogChunk()

	case logDoneMsg:
		if m.screen != screenLogs || msg.session != m.logsSession {
			return m, nil
		}
		m.logsDone = true
		// Flush any in-flight partial line (a final line with no trailing newline)
		// into the raw buffer on BOTH clean and error EOF. Without this, the last
		// unterminated line would render (via derivedLogContent's unfiltered
		// partial slot) but permanently bypass the filter; and if it were the sole
		// content, logsRawLines would stay empty and the `f`/`/` open handlers'
		// empty-buffer guard would refuse to open. Capture the follow state before
		// re-deriving so a live tail stays pinned to the (now filtered) bottom.
		following := m.logsViewport.AtBottom()
		flushed := false
		if m.logsPartial != "" {
			m.logsRawLines = append(m.logsRawLines, m.logsPartial)
			m.logsPartial = ""
			flushed = true
		}
		if msg.err != nil {
			m.logsErr = msg.err
			// The terminal error is filter-exempt: it must render regardless of an
			// active filter, so it lives in a dedicated slot outside the filterable
			// raw-line buffer.
			m.logsErrLine = fmt.Sprintf("Error: %v", msg.err)
			m.applyLogFormat()
			m.logsViewport.GotoBottom() // force the error into view even if paused
			return m, nil
		}
		if flushed {
			m.applyLogFormat() // subject the flushed line to the filter
			if following {
				m.logsViewport.GotoBottom()
			}
		}
		return m, nil

	case configFileMsg:
		if m.screen != screenConfig || msg.session != m.configSession {
			return m, nil
		}
		if msg.err != nil {
			m.configErr = msg.err
			return m, nil
		}
		m.configErr = nil
		m.configContent = msg.data
		m.configViewport.SetContent(string(msg.data))
		return m, nil

	case configResolvedMsg:
		if m.screen != screenConfig || msg.session != m.configSession {
			return m, nil
		}
		if msg.err != nil {
			m.configShowRes = false
			if m.configContent != nil {
				m.configViewport.SetContent(string(m.configContent))
				v := false
				m.configValid = &v
				m.configValidMsg = fmt.Sprintf("resolved config failed: %v", msg.err)
			} else {
				m.configErr = msg.err
			}
			return m, nil
		}
		m.configErr = nil
		m.configValid = nil
		m.configValidMsg = ""
		m.configResolved = msg.data
		if m.configShowRes {
			m.configViewport.SetContent(string(msg.data))
		}
		return m, nil

	case configEditDoneMsg:
		if m.screen != screenConfig || msg.session != m.configSession {
			return m, nil
		}
		if msg.err != nil {
			m.configErr = msg.err
			return m, nil
		}
		// Re-fetch raw content and validate concurrently; reset to raw view
		// since the resolved cache is invalidated and raw is being re-fetched.
		m.configResolved = nil
		m.configShowRes = false
		return m, tea.Batch(m.fetchConfigFile(), m.fetchConfigValidate())

	case configValidateMsg:
		if m.screen != screenConfig || msg.session != m.configSession {
			return m, nil
		}
		if msg.err != nil {
			v := false
			m.configValid = &v
			m.configValidMsg = msg.err.Error()
		} else {
			v := true
			m.configValid = &v
			m.configValidMsg = ""
		}
		return m, nil

	case inspectDataMsg:
		if m.screen != screenInspect || msg.session != m.inspectSession {
			return m, nil
		}
		if msg.err != nil {
			m.inspectErr = msg.err
			return m, nil
		}
		if len(msg.data) == 0 {
			// A call that succeeded and printed nothing. rebuildInspectSummary
			// early-returns on empty bytes, so ParseInspect's own "empty
			// output" error never fires and viewInspect would read
			// "Loading..." for ever, with no error and no way to retry.
			m.inspectErr = errors.New("docker inspect returned no output")
			return m, nil
		}
		m.inspectRaw = msg.data
		m.rebuildInspectSummary()
		if m.inspectErr != nil {
			// The parse failed and the raw bytes are the only content there is,
			// so land the user on them instead of on an empty pane under an
			// error line. Done HERE, on the transition into the error state,
			// not in rebuildInspectSummary — that also runs on every resize,
			// which would silently undo a later `r` back to the summary.
			m.inspectShowRaw = true
		}
		m.setInspectContent()
		return m, nil

	case execDoneMsg:
		if m.screen != screenSelectContainers {
			return m, nil
		}
		m.pendingExec = false
		m.confirming = false
		if msg.err != nil {
			m.warning = fmt.Sprintf("exec failed: %v", msg.err)
		}
		m.statsSession++
		m.statusSession++
		m.updatesSession++
		m.rollbackFetchSession++
		m.statsRequested = true
		m.refreshInFlight = true
		// Reset updateInFlight before calling maybeRefreshUpdatesCmd: the
		// session bump above invalidates any pending updatesMsg, so the
		// guard inside maybeRefreshUpdatesCmd must not refuse to fire a
		// fresh fetch just because a now-stale fetch is still in flight.
		m.updateInFlight = false
		cmds := []tea.Cmd{m.refreshStatus(), m.refreshStats()}
		if c := m.maybeRefreshUpdatesCmd(); c != nil {
			cmds = append(cmds, c)
		}
		return m, tea.Batch(cmds...)

	case pipelineDoneMsg:
		if m.failed {
			return m, nil
		}
		m.done = true
		// StopOnly has no health phase; a failed pipeline never reaches here.
		// Deploy/Restart/Rollback enter the waiting sub-state and start the
		// health-poll loop with the first poll firing immediately (containers
		// were just (re)started).
		if m.pendingOp == runner.StopOnly {
			return m, nil
		}
		targets := m.opContainers
		if len(targets) == 0 {
			targets = m.services
		}
		if len(targets) == 0 {
			return m, nil // nothing to wait on
		}
		m.waiting = true
		m.waitSession++
		m.waitState = runner.NewWaitState(targets)
		m.waitDeadline = time.Now().Add(runner.DefaultWaitTimeout)
		return m, m.pollWaitStatus()

	case waitTickMsg:
		// Stale ticks (skipped wait, departed screen, or a superseded session)
		// are dead — drop without rescheduling. A live tick fires the next poll.
		if m.screen != screenProgress || !m.waiting || msg.session != m.waitSession {
			return m, nil
		}
		return m, m.pollWaitStatus()

	case waitStatusMsg:
		if m.screen != screenProgress || !m.waiting || msg.session != m.waitSession {
			return m, nil
		}
		elapsed := m.waitElapsed()
		if msg.err != nil {
			// Transient poll failure: carry the reducer state forward and keep
			// polling. Feeding a nil/zero status map into EvaluateWait would
			// corrupt per-service verdicts (a running service would read as
			// not-running), so instead we sweep pending → timed out directly
			// once the deadline passes — guaranteeing termination even if
			// ContainerStatus keeps failing.
			if elapsed >= runner.DefaultWaitTimeout {
				m.sweepWaitTimeout()
				m.waiting = false
				return m, nil
			}
			return m, m.waitTick()
		}
		var done bool
		m.waitState, done = runner.EvaluateWait(m.waitState, msg.status, runner.WaitOptions{}, elapsed)
		if done {
			// Wait resolved: stop polling but KEEP waitState so the verdict
			// lines (and the rollback hint on a failed deploy) stay rendered.
			m.waiting = false
			return m, nil
		}
		return m, m.waitTick()

	case rollbackSnapshotMsg:
		// Drop a fetch that outlived its container-screen visit (a context change
		// bumped the session) or one that raced ahead of an in-progress
		// confirmation / search (don't clobber an active prompt).
		if m.screen != screenSelectContainers || msg.session != m.rollbackFetchSession {
			return m, nil
		}
		if m.confirming || m.searching {
			return m, nil
		}
		if msg.err != nil {
			m.warning = fmt.Sprintf("rollback unavailable: %v", msg.err)
			m.fixSvcOffset()
			return m, nil
		}
		if msg.snap == nil || len(msg.snap.Services) == 0 {
			m.warning = "No rollback snapshot found — deploy first to record one"
			m.fixSvcOffset()
			return m, nil
		}
		// Use the target set captured at R-press time — NOT the live selection.
		// The user may have changed or cleared the multi-select during the async
		// fetch; re-deriving from the current selection would let an empty set
		// become an unintended all-service rollback (empty == all in the runner).
		// The session check above already rejected a stale fetch, so a matching
		// fetch always carries the capture from this R press.
		targets := m.rollbackTargets
		if len(targets) == 0 {
			// Defensive: a matching-session fetch always carries a non-empty
			// capture (guarded at R-press). Refuse rather than fall through to
			// an all-service rollback.
			m.warning = warnNoSelection
			m.fixSvcOffset()
			return m, nil
		}
		// Every selected service must have a snapshot entry — mirror the CLI
		// refusal, naming exactly what is missing.
		var missing []string
		for _, svc := range targets {
			if _, ok := msg.snap.Services[svc]; !ok {
				missing = append(missing, svc)
			}
		}
		if len(missing) > 0 {
			slices.Sort(missing)
			m.warning = fmt.Sprintf("no rollback snapshot for: %s", strings.Join(missing, ", "))
			m.fixSvcOffset()
			return m, nil
		}
		// Every selected service must ALSO still exist in the current compose file.
		// Rollback pins images only against the live compose file, so a snapshot
		// entry for a since-removed service must not resurrect it as a minimal
		// image-only override (mirrors the CLI filterLiveTargets named-target
		// refusal — the TUI selection is explicit, so a stale target refuses rather
		// than silently proceeding with a subset). msg.live is the fresh
		// ListServices result captured alongside the snapshot.
		liveSet := make(map[string]bool, len(msg.live))
		for _, svc := range msg.live {
			liveSet[svc] = true
		}
		var removed []string
		for _, svc := range targets {
			if !liveSet[svc] {
				removed = append(removed, svc)
			}
		}
		if len(removed) > 0 {
			slices.Sort(removed)
			m.warning = fmt.Sprintf("no longer in the compose file: %s", strings.Join(removed, ", "))
			m.fixSvcOffset()
			return m, nil
		}
		// Snapshot present for all targets — enter the existing confirm flow with
		// the Rollback op. enterProgress (on confirm) reads m.rollbackSnapshot for
		// the prep.
		m.rollbackSnapshot = msg.snap
		m.pendingOp = runner.Rollback
		m.confirming = true
		// This is the one confirmation armed by a message rather than a key, so
		// the `?` open gate cannot have seen it. Close the overlay here instead:
		// otherwise a user who presses R and then ? before the fetch lands gets
		// the overlay drawn over a live rollback prompt, and the single esc that
		// closes it would leave the prompt armed underneath.
		m.helpOpen = false
		m.fixSvcOffset()
		return m, nil

	case rollbackPreppedMsg:
		// Prep runs only for a Rollback launched from screenProgress. If the user
		// left the screen before it returned, invoke any cleanup so the override
		// file / ExtraComposeFiles don't leak, then drop.
		if m.screen != screenProgress {
			if msg.cleanup != nil {
				msg.cleanup()
			}
			return m, nil
		}
		if msg.err != nil {
			// Prep failed before the pipeline ran: mark the op failed and surface
			// the error on the progress screen. No cleanup to store — prep aborts
			// before mutating ExtraComposeFiles on error.
			m.failed = true
			m.rollbackErr = msg.err.Error()
			return m, nil
		}
		// Prep succeeded: the pipeline goroutine is already running (launched
		// inside prepareRollbackCmd). Store the cleanup for invocation on leaving
		// the progress screen (NEVER goroutine-deferred — see the race rule) and
		// start consuming pipeline events.
		m.rollbackCleanup = msg.cleanup
		return m, m.waitForEvent()

	case spinner.TickMsg:
		if m.screen == screenProgress && !m.done && !m.failed {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
		return m, nil
	}

	return m, nil
}

// tryQuit returns tea.Quit immediately for local sessions.
// For remote connections (disconnectFunc != nil), it sets quitting = true
// to show a confirmation prompt instead.
func (m Model) tryQuit() (tea.Model, tea.Cmd) {
	if m.disconnectFunc != nil {
		m.quitting = true
		return m, nil // defer cleanup until the confirm ("y") actually quits
	}
	// Local session quitting for real: run any pending rollback-prep cleanup
	// (override temp-file removal + ExtraComposeFiles reset) on the main
	// goroutine so a hard exit during a rollback wait doesn't leak the file
	// (the remote /tmp override in particular has no reaper). No-op when no
	// rollback is in flight. Runs synchronously in Update — never
	// goroutine-deferred (the documented rollback cleanup-timing race rule).
	m.runRollbackCleanup()
	return m, tea.Quit
}

// runRollbackCleanup invokes and clears the stored rollback-prep cleanup if set.
// Idempotent (nils the field) so a later leave-progress path can't double-invoke.
func (m *Model) runRollbackCleanup() {
	if m.rollbackCleanup != nil {
		m.rollbackCleanup()
		m.rollbackCleanup = nil
	}
}

// confirmPromptArmed reports whether a destructive confirmation prompt is
// drawn on the CURRENT screen, so the `?` intercept can refuse to cover it.
//
// The scoping is load-bearing, not tidiness: `confirming` is armed on the
// container screen and enterProgress does not clear it (only the esc back-nav
// out of screenProgress does), so an unscoped `!m.confirming` gate would keep
// the overlay shut for the entire life of the progress screen.
func (m Model) confirmPromptArmed() bool {
	switch m.screen {
	case screenSelectContainers:
		return m.confirming
	case screenSettingsList:
		return m.settingsDelete
	}
	return false
}

// typingInInput reports whether an open text input is capturing raw runes on
// the current screen. Two callers share it: the `?` intercept skips those
// screens so `?` — a regex metacharacter the log filter accepts in RE2 mode —
// lands in the input, and the q->esc rewrite skips them so server names like
// "qa-prod" stay typeable. A new screen that opens a text input needs one
// case here and nothing else.
func (m Model) typingInInput() bool {
	switch m.screen {
	case screenSelectContainers:
		return m.searching
	case screenLogs:
		return m.logFiltering || m.logSearching
	case screenSettingsForm:
		return m.settingsField < 4 // 0-3 are textinputs; 4 is the color picker
	}
	return false
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Quit confirmation intercept: when quitting is true, handle y/n/esc
	// and swallow all other keys.
	if m.quitting {
		switch key {
		case "y":
			// Confirmed quit on a remote session: run any pending rollback-prep
			// cleanup (remote /tmp override rm -f) before tearing down — the
			// ControlMaster socket is still live here, and Run()'s disconnectFunc
			// (which closes it) only fires after the tea loop exits. Main
			// goroutine, never goroutine-deferred (rollback cleanup race rule).
			m.runRollbackCleanup()
			return m, tea.Quit
		case "n", "esc":
			m.quitting = false
			return m, nil
		}
		return m, nil
	}

	// Key-reference overlay (`?`) intercept. It must precede the open block
	// below, or a `?` pressed while the overlay is open would re-open it and
	// never close it.
	if m.helpOpen {
		switch key {
		case "?", "esc", "q":
			m.helpOpen = false
			return m, nil
		case "ctrl+c":
			// Clear the flag and FALL THROUGH rather than return. That makes
			// each screen's existing ctrl+c semantics apply unchanged: the
			// local quit, the remote disconnect prompt, and the mid-progress
			// no-op. ctrl+c is NOT a hard exit from every screen.
			m.helpOpen = false
		default:
			return m, nil
		}
	}

	// Open the overlay. Skipped while a text input is capturing runes (`?` is
	// a regex metacharacter) and while a destructive confirmation is armed —
	// the overlay would hide the prompt, and the single esc that closes the
	// overlay would leave it armed underneath (confirmPromptArmed is
	// screen-scoped for a reason — see its doc).
	if key == "?" && !m.typingInInput() && !m.confirmPromptArmed() {
		m.helpOpen = true
		return m, nil
	}

	// q acts as a back key inside the app. It quits only when there is
	// no parent screen to navigate to (server-select, or the project /
	// containers screens when standalone). The typingInInput() guard skips
	// the whole rewrite while a text input is open — the container search
	// bar, the log filter/search bars, and the settings-form text fields —
	// so q reaches the input as a literal character (server names like
	// "qa-prod"). screenProgress while running is also excluded so q cannot
	// cancel an in-flight operation.
	if key == "q" && !m.typingInInput() {
		switch m.screen {
		case screenSelectServer:
			return m, tea.Quit
		case screenSelectProject:
			if !m.canGoBack() {
				return m, tea.Quit
			}
			key = "esc"
		case screenSelectContainers:
			// The extra !m.confirming term has no canGoBack analogue: an armed
			// prompt takes q as "cancel" (the esc handler clears it) even on the
			// standalone screen, where q would otherwise quit.
			if !m.canGoBack() && !m.confirming {
				return m, tea.Quit
			}
			key = "esc"
		case screenProgress:
			if !m.done && !m.failed {
				return m, nil
			}
			key = "esc"
		default:
			key = "esc"
		}
	}

	switch m.screen {
	case screenSelectServer:
		switch key {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			m.serverCursor = prevSelectable(m.serverEntries, m.serverCursor)
		case "down", "j":
			m.serverCursor = nextSelectable(m.serverEntries, m.serverCursor)
		case "enter":
			entry := m.serverEntries[m.serverCursor]
			switch entry.kind {
			case entryLocal:
				m.serverName = ""
				m.serverHost = ""
				m.serverColor = ""
				m.composerFactory = m.localFactory
				m.projectLoader = m.localProjectLoader
				m.projectsSession++
				m.disconnectFunc = nil
				m.quitting = false
				// Explicit cleanup for downstream state from any previous
				// server selection — per CLAUDE.md "Backward-navigation state
				// cleanup" discipline. projDir/projName feed the updatesCache
				// key, so leaving stale values would lookup the wrong cache
				// slot. updatesErr left over from a prior remote session
				// would leak into the local view's soft-warning slot.
				m.projDir = ""
				m.projName = ""
				m.updatesErr = ""
				m.rollbackSnapshot = nil
				m.rollbackTargets = nil
				m.rollbackCleanup = nil
				m.clearSearch()
				m.clearWaitState()
				if m.localComposer != nil {
					m.composer = m.localComposer
					m.statsSession++
					m.statusSession++
					m.updatesSession++
					m.rollbackFetchSession++
					m.statsRequested = true
					m.refreshInFlight = true
					// Reset updateInFlight before calling
					// maybeRefreshUpdatesCmd: the session bump above
					// invalidates any pending updatesMsg, so the guard
					// inside maybeRefreshUpdatesCmd must not refuse to fire
					// a fresh fetch just because a now-stale fetch is still
					// in flight.
					m.updateInFlight = false
					m.showPicker = true
					m.screen = screenSelectContainers
					cmds := []tea.Cmd{m.loadServices(), m.refreshStats()}
					if c := m.maybeRefreshUpdatesCmd(); c != nil {
						cmds = append(cmds, c)
					}
					return m, tea.Batch(cmds...)
				}
				m.showPicker = true
				m.screen = screenSelectProject
				return m, m.loadProjects()
			case entryServer:
				server := m.servers[entry.serverIdx]
				m.serverName = server.Name
				m.serverHost = server.Host
				m.serverColor = m.resolveServerColor(server)
				connectCmd, factory, loader, disconnect := m.connectCb(server)
				m.composerFactory = factory
				m.projectLoader = loader
				m.projectsSession++
				m.disconnectFunc = disconnect
				return m, tea.ExecProcess(connectCmd, func(err error) tea.Msg {
					return connectResultMsg{err: err}
				})
			default:
				panic("unhandled default case")
			}
		case "s":
			if m.config != nil {
				m.screen = screenSettingsList
				m.settingsCursor = 0
				m.settingsDelete = false
				return m, nil
			}
		}

	case screenSelectProject:
		switch key {
		case "ctrl+c":
			return m.tryQuit()
		case "esc":
			if len(m.servers) > 0 || m.config != nil {
				// Back to server screen — disconnect if remote
				disconnectFn := m.disconnectFunc
				m.screen = screenSelectServer
				m.serverName = ""
				m.serverHost = ""
				m.serverColor = ""
				m.disconnectFunc = nil
				m.quitting = false
				m.projectLoader = m.localProjectLoader
				m.composerFactory = m.localFactory
				m.composer = nil
				m.statsSession++
				m.statusSession++
				m.updatesSession++
				m.projectsSession++
				m.rollbackFetchSession++
				m.refreshInFlight = false
				m.updateInFlight = false
				m.updatesErr = ""
				m.projName = ""
				m.projDir = ""
				m.projects = nil
				m.projCursor = 0
				m.projErr = nil
				m.showPicker = false
				m.clearSearch()
				m.clearWaitState()
				if disconnectFn != nil {
					return m, func() tea.Msg {
						_ = disconnectFn()
						return disconnectDoneMsg{}
					}
				}
				return m, nil
			}
		case "up", "k":
			if m.projCursor > 0 {
				m.projCursor--
			}
		case "down", "j":
			if m.projCursor < len(m.projects)-1 {
				m.projCursor++
			}
		case "enter":
			if len(m.projects) == 0 {
				return m, nil
			}
			proj := m.projects[m.projCursor]
			m.projName = proj.Name
			m.projDir = proj.ConfigDir
			m.composer = m.composerFactory(proj)
			m.statsSession++
			m.statusSession++
			m.updatesSession++
			m.rollbackFetchSession++
			m.statsRequested = true
			m.refreshInFlight = true
			// Reset updateInFlight before calling maybeRefreshUpdatesCmd: the
			// session bump above invalidates any pending updatesMsg, so the
			// guard inside maybeRefreshUpdatesCmd must not refuse to fire a
			// fresh fetch just because a now-stale fetch is still in flight.
			m.updateInFlight = false
			m.screen = screenSelectContainers
			cmds := []tea.Cmd{m.loadServices(), m.refreshStats()}
			if c := m.maybeRefreshUpdatesCmd(); c != nil {
				cmds = append(cmds, c)
			}
			return m, tea.Batch(cmds...)
		}

	case screenSelectContainers:
		if m.confirming {
			switch key {
			case "ctrl+c":
				return m.tryQuit()
			case "enter":
				if m.pendingExec {
					return m.enterExec()
				}
				containers := m.selectedContainers()
				if m.pendingOp == runner.Rollback {
					// Rollback uses the set captured at R-press, not the live
					// selection (which the async fetch let drift). This keeps the
					// pipeline/prep target identical to what was validated and shown
					// in the confirm prompt.
					containers = m.rollbackTargets
				}
				return m.enterProgress(containers)
			case "esc":
				m.confirming = false
				m.pendingExec = false
				m.fixSvcOffset()
				return m, nil
			}
			return m, nil
		}

		if m.searching {
			switch key {
			case "ctrl+c":
				return m.tryQuit()
			case "enter":
				// Commit: keep the query/matches live (highlights + n/N),
				// close the bar. If nothing matched, drop the query so we
				// don't leave a dead highlight/counter behind.
				m.searching = false
				if len(m.searchMatches) == 0 {
					m.clearSearch()
				}
				return m, nil
			case "esc":
				// Cancel: discard the query and restore the cursor to where
				// the user opened search. No back-navigation. Capture the
				// return cursor before clearSearch() (which resets
				// searchReturn) and delegate the field reset so this stays in
				// lockstep if clearSearch() gains a field.
				ret := m.searchReturn
				m.clearSearch()
				m.svcCursor = ret
				m.fixSvcOffset()
				return m, nil
			default:
				// Set the value-area budget BEFORE Update() so bubbles computes
				// this keystroke's horizontal-scroll offset with the correct width.
				// searchInputWidth() is keystroke-stable (reserves the widest
				// possible counter, not the live one), so the width is already
				// right regardless of how the match count shifts below; setting it
				// AFTER Update() would have bubbles compute the offset with the
				// PREVIOUS width, clipping the newest char/cursor for one frame.
				m.searchInput.Width = m.searchInputWidth()
				var cmd tea.Cmd
				m.searchInput, cmd = m.searchInput.Update(msg)
				m.searchQuery = m.searchInput.Value()
				m.searchMatches = computeMatches(m.services, m.searchQuery)
				if len(m.searchMatches) > 0 {
					m.svcCursor = m.searchMatches[0]
					m.fixSvcOffset()
				}
				return m, cmd
			}
		}

		m.warning = ""

		switch key {
		case "ctrl+c":
			return m.tryQuit()
		case "esc":
			// Two-stage esc: the first esc on a committed search clears the
			// search (highlights + n/N) and stays on the screen; a second esc
			// (no active search) falls through to the existing back-nav.
			if m.searchQuery != "" {
				m.clearSearch()
				m.fixSvcOffset()
				return m, nil
			}
			if m.showPicker {
				m.screen = screenSelectProject
				m.composer = nil
				m.statsSession++
				m.statusSession++
				m.updatesSession++
				m.rollbackFetchSession++
				m.rollbackSnapshot = nil
				m.rollbackTargets = nil
				m.refreshInFlight = false
				m.updateInFlight = false
				m.updatesErr = ""
				m.projName = ""
				m.projDir = ""
				m.services = nil
				m.svcStatus = nil
				m.stats = nil
				m.statsErr = nil
				m.selected = make(map[int]bool)
				m.svcCursor = 0
				m.svcOffset = 0
				m.svcErr = nil
				// Leaving screenSelectContainers: search is ephemeral. The
				// two-stage guard above already cleared any active search before
				// we reach here, but call clearSearch() unconditionally so the
				// ephemeral-on-departure invariant holds regardless of entry path.
				m.clearSearch()
				m.clearWaitState()
				if m.projects == nil {
					return m, m.loadProjects()
				}
				return m, nil
			}
		case "up", "k":
			if m.svcCursor > 0 {
				m.svcCursor--
			}
			m.fixSvcOffset()
		case "down", "j":
			if m.svcCursor < len(m.services)-1 {
				m.svcCursor++
			}
			m.fixSvcOffset()
		case " ":
			// Multi-select exists only to feed d/r/s/R. On a read-only composer
			// those are gated, so toggling would arm nothing — and the row
			// checkbox is not rendered either (see viewSelectContainers).
			//
			// Every gated key below re-clamps before returning: the dispatch
			// clears m.warning above, which frees the warning footer line and
			// grows svcVisibleCount() by one, so a scrolled list would keep a
			// too-large svcOffset and render a blank row at the bottom.
			if m.readOnly() {
				m.fixSvcOffset()
				return m, nil
			}
			if len(m.services) > 0 {
				m.selected[m.svcCursor] = !m.selected[m.svcCursor]
			}
		case "a":
			if m.readOnly() {
				m.fixSvcOffset()
				return m, nil
			}
			allSel := m.allSelected()
			for i := range m.services {
				m.selected[i] = !allSel
			}
		case "r":
			if m.readOnly() {
				m.fixSvcOffset()
				return m, nil
			}
			if m.selectedCount() > 0 {
				m.pendingOp = runner.Restart
				m.confirming = true
			} else {
				m.warning = warnNoSelection
			}
			m.fixSvcOffset()
		case "d":
			if m.readOnly() {
				m.fixSvcOffset()
				return m, nil
			}
			if m.selectedCount() > 0 {
				m.pendingOp = runner.Deploy
				m.confirming = true
			} else {
				m.warning = warnNoSelection
			}
			m.fixSvcOffset()
		case "s":
			if m.readOnly() {
				m.fixSvcOffset()
				return m, nil
			}
			if m.selectedCount() > 0 {
				m.pendingOp = runner.StopOnly
				m.confirming = true
			} else {
				m.warning = warnNoSelection
			}
			m.fixSvcOffset()
		case "R":
			// Roll the selected services back to the last-deployed digests. Needs
			// the concrete-composer RollbackPreparer capability (ReadSnapshot +
			// PrepareRollback); a mock/composer without it makes R a no-op (mirrors
			// the c/x type-assert guards). Guards mirror the action keys: an empty
			// list is ignored (like l/x), an empty selection warns (like r/d/s).
			// A fresh async ReadSnapshot decides warning vs confirmation.
			// The readOnly gate is explicit rather than relying on the type
			// assertion below (HostContainers is not a RollbackPreparer): an
			// inert key that a help table still names is the failure mode this
			// gate pairs with, so the gate and the table move together.
			if m.readOnly() {
				m.fixSvcOffset()
				return m, nil
			}
			if _, ok := m.composer.(RollbackPreparer); !ok {
				return m, nil
			}
			if len(m.services) == 0 {
				return m, nil
			}
			if m.selectedCount() == 0 {
				m.warning = warnNoSelection
				m.fixSvcOffset()
				return m, nil
			}
			// Capture the selected target set NOW, at press time. The snapshot
			// fetch below is async, so the live multi-select can change or clear
			// before rollbackSnapshotMsg lands; carrying the captured set through
			// the whole flow (validation → confirm → prep → pipeline) prevents a
			// since-cleared selection from becoming an all-service rollback
			// (empty == all in the runner). Non-empty here (selectedCount guard).
			m.rollbackTargets = m.selectedContainers()
			// Bump the fetch session first (invalidating any prior in-flight R
			// fetch) and capture it inside the Cmd for staleness rejection; the
			// same session also guards the captured rollbackTargets.
			m.rollbackFetchSession++
			return m, m.fetchRollbackSnapshot()
		case "n":
			// Cycle to the next match (committed search only; no-op otherwise).
			m.cycleMatch(true)
		case "N":
			// Cycle to the previous match (committed search only; no-op otherwise).
			m.cycleMatch(false)
		case "/":
			// Open the search bar. Guard an empty list (mirrors l/x). A fresh
			// textinput is constructed on every open so tests that build Model
			// literals directly (bypassing NewModel) still get a live input.
			if len(m.services) == 0 {
				return m, nil
			}
			// Clear any prior committed search FIRST so reopening starts from a
			// clean slate: without this, the fresh empty input would render
			// alongside stale highlights + a stale counter, and an immediate
			// enter would re-commit the OLD query (searchMatches still non-empty).
			// clearSearch() zeroes searching/searchReturn, so set them AFTER it.
			m.clearSearch()
			m.searchInput = textinput.New()
			m.searching = true
			m.searchReturn = m.svcCursor
			// Set Width on this PERSISTED model so bubbles scrolls the value
			// horizontally to keep the cursor visible (a value-receiver set in
			// searchBarLine would be discarded). Width 0 = unbounded when size
			// is unknown (tests).
			m.searchInput.Width = m.searchInputWidth()
			m.searchInput.Focus()
			return m, nil
		case "l":
			if len(m.services) == 0 {
				return m, nil
			}
			return m.enterLogs()
		case "c":
			// Explicit gate for the same reason as R: HostContainers is not a
			// ConfigProvider, so c already no-ops, but the gate keeps the key's
			// absence from the read-only help table honest.
			if m.readOnly() {
				m.fixSvcOffset()
				return m, nil
			}
			if _, ok := m.composer.(ConfigProvider); ok {
				return m.enterConfig()
			}
		case "i":
			// Deliberately NOT gated on m.readOnly(): inspect is read-only by
			// nature and works identically on both container variants, so it is
			// named in both help tables.
			//
			// Neither no-op path below calls fixSvcOffset(), matching the l/x
			// guards rather than the read-only gates. The dispatch clears
			// m.warning above the switch, so a freed warning line leaves
			// svcOffset unclamped for one render — an inherited hole, adopted
			// knowingly rather than fixed here for one key.
			if len(m.services) == 0 {
				return m, nil
			}
			if _, ok := m.composer.(Inspector); !ok {
				return m, nil
			}
			return m.enterInspect()
		case "x":
			if _, ok := m.composer.(ExecProvider); !ok {
				return m, nil
			}
			if len(m.services) == 0 {
				return m, nil
			}
			svc := m.services[m.svcCursor]
			if st, ok := m.svcStatus[svc]; !ok || !st.Running {
				m.warning = "Container is not running"
				m.fixSvcOffset()
				return m, nil
			}
			m.confirming = true
			m.pendingExec = true
			m.fixSvcOffset()
		case "U":
			// Force-refresh CheckUpdates, bypassing the TTL cache. Matches
			// the periodic-refresh UX of stats: no transient "checking…" —
			// cells just blank until the result arrives. The session is NOT
			// bumped here because the context (project + server) hasn't
			// changed; we just want a fresh result for the same session.
			if m.composer == nil {
				return m, nil
			}
			// Guard against stacking: if a refreshUpdates is already in
			// flight (e.g. the user mashes U or a previous request hasn't
			// completed), the previous response will hydrate the same
			// session. Firing another fetch would only burn registry calls.
			if m.updateInFlight {
				return m, nil
			}
			m.updateInFlight = true
			return m, m.refreshUpdates()
		}

	case screenLogs:
		// Layered esc ladder (peel inner → outer): (1) typing-search cancel,
		// (2) typing-filter cancel, (3) committed-search clear-only, (4)
		// committed-filter clear-only, (5) leave the screen. Rungs 1–2 live in
		// the two typing intercepts below (they own esc while an input is open);
		// rungs 3–5 live in the main switch's `esc` case (reached only when no
		// input is open). Because filter and search bars are mutually exclusive
		// (only one input can be open at a time), the two intercepts never both
		// run, and by the time control reaches the main switch both logFiltering
		// and logSearching are false — so rungs 3/4 need only test the committed
		// query strings.
		//
		// Typing intercept — MUST be the first statement so that while an input
		// bar is open every keystroke routes to it (mirrors the container-screen
		// `if m.searching` intercept). Without this, typing `w`/`p`/`G`/`f`/`q`
		// into the filter query would fire wrap/pretty/gotobottom/filter/back.
		if m.logFiltering {
			switch key {
			case "ctrl+c":
				return m.tryQuit()
			case "enter":
				// Commit: close the bar, keep the last-good query live. If no
				// valid query was entered, fully clear so no dead filter state
				// (an in-progress bad regex, say) lingers.
				if m.logFilterQuery == "" {
					m.clearLogFilter()
				} else {
					m.logFiltering = false
					m.logFilterInput.Blur()
				}
				return m, nil
			case "esc":
				// Ladder rung 2 (typing-filter cancel): discard the in-progress
				// query and restore the full unfiltered view, staying on the
				// screen (no back-nav).
				m.clearLogFilter()
				m.rederiveLogs()
				return m, nil
			case "ctrl+r":
				// Toggle regex vs. substring matching, then re-evaluate the live
				// query under the new mode.
				m.logFilterIsRegex = !m.logFilterIsRegex
				m.recomputeLogFilter()
				return m, nil
			default:
				// Set the value-area budget BEFORE Update() so bubbles computes
				// this keystroke's horizontal-scroll offset with the correct width
				// (setting it after would use the previous width, clipping the
				// newest char/cursor for one frame). logFilterInputWidth() is
				// keystroke-stable, so it's already right regardless of the count.
				m.logFilterInput.Width = m.logFilterInputWidth()
				var cmd tea.Cmd
				m.logFilterInput, cmd = m.logFilterInput.Update(msg)
				m.recomputeLogFilter()
				return m, cmd
			}
		}

		// Search typing intercept — the filter intercept above has an exact
		// companion here. While the search bar is open every keystroke routes to
		// logSearchInput so q/n/N type literally instead of firing back/cycle.
		if m.logSearching {
			switch key {
			case "ctrl+c":
				return m.tryQuit()
			case "enter":
				// Commit: close the bar, keep highlights + n/N live. An empty
				// query — OR a valid-but-non-matching one — fully clears so no
				// dead search state (a live "(no match)" counter with inert n/N)
				// lingers. Mirrors the container search's zero-match drop.
				if m.logSearchQuery == "" || len(m.logSearchMatches) == 0 {
					m.clearLogSearch()
					m.setLogViewportContent()
				} else {
					m.logSearching = false
					m.logSearchInput.Blur()
				}
				return m, nil
			case "esc":
				// Ladder rung 1 (typing-search cancel): discard the in-progress
				// query and clear the highlight, staying on the screen. Mirrors
				// the container search's esc-while-typing, which discards without
				// leaving; the log view has no cursor to restore (searchReturn),
				// so this simply cancels typing and stays.
				m.clearLogSearch()
				m.setLogViewportContent()
				return m, nil
			case "ctrl+r":
				m.logSearchIsRegex = !m.logSearchIsRegex
				m.recomputeLogSearch()
				return m, nil
			default:
				// Set the value-area budget BEFORE Update() (see the filter
				// intercept above for the ordering rationale).
				m.logSearchInput.Width = m.logSearchInputWidth()
				var cmd tea.Cmd
				m.logSearchInput, cmd = m.logSearchInput.Update(msg)
				m.recomputeLogSearch()
				return m, cmd
			}
		}

		switch key {
		case "ctrl+c":
			return m.tryQuit()
		case "esc":
			// Ladder rungs 3–4 (no input open — both intercepts above returned
			// early, so logFiltering/logSearching are false here). Peel the
			// committed search before the committed filter so that with BOTH
			// live, the first esc clears search only, the second clears filter
			// only, and only the third (rung 5) leaves the screen.
			if m.logSearchQuery != "" {
				// Rung 3 (committed-search clear-only): drop the highlight +
				// n/N, stay on the screen.
				m.clearLogSearch()
				m.setLogViewportContent()
				return m, nil
			}
			if m.logFilterQuery != "" {
				// Rung 4 (committed-filter clear-only): drop the filter,
				// re-derive the full view, stay on the screen.
				m.clearLogFilter()
				m.rederiveLogs()
				return m, nil
			}
			// Rung 5 (leave screen): neither active — cancel the log context,
			// clear all log state, and return to the container screen.
			if m.logsCancel != nil {
				m.logsCancel()
			}
			m.logsService = ""
			m.logsCancel = nil
			m.logsDone = false
			m.logsErr = nil
			m.logsPipeR = nil
			m.logsViewport = viewport.Model{}
			m.logsWrap = false
			m.logsPretty = false
			m.logsRawLines = nil
			m.logsPartial = ""
			m.logsScanned = 0
			m.logsFormatted = ""
			m.logFilterShown = 0
			m.logsErrLine = ""
			m.clearLogFilter()
			m.clearLogSearch()
			m.screen = screenSelectContainers
			m.statsSession++
			m.statusSession++
			m.updatesSession++
			m.rollbackFetchSession++
			m.statsRequested = true
			m.refreshInFlight = true
			// Reset updateInFlight before calling maybeRefreshUpdatesCmd: the
			// session bump above invalidates any pending updatesMsg, so the
			// guard inside maybeRefreshUpdatesCmd must not refuse to fire a
			// fresh fetch just because a now-stale fetch is still in flight.
			m.updateInFlight = false
			cmds := []tea.Cmd{m.refreshStatus(), m.refreshStats()}
			if c := m.maybeRefreshUpdatesCmd(); c != nil {
				cmds = append(cmds, c)
			}
			return m, tea.Batch(cmds...)
		case "w":
			following := m.logsViewport.AtBottom()
			m.logsWrap = !m.logsWrap
			if m.logsWrap {
				m.logsViewport.SetHorizontalStep(0)
			} else {
				m.logsViewport.SetHorizontalStep(4)
			}
			m.fullReformat()
			if following {
				m.logsViewport.GotoBottom()
			}
			return m, nil
		case "p":
			following := m.logsViewport.AtBottom()
			m.logsPretty = !m.logsPretty
			m.fullReformat()
			if following {
				m.logsViewport.GotoBottom()
			}
			return m, nil
		case "G":
			m.logsViewport.GotoBottom()
			return m, nil
		case "f":
			// Open the live filter. Early-return on an empty raw buffer (mirrors
			// the l/x empty-list guards on the container screen). The input is
			// built lazily here — NOT in NewModel — so Model{} test literals stay
			// valid (a zero-value textinput is never rendered while closed).
			if len(m.logsRawLines) == 0 {
				return m, nil
			}
			// Reset any prior committed filter FIRST so reopening starts from a
			// clean slate (mirrors the container `/` clearSearch guard): without
			// this the fresh empty input would render while the view stays
			// narrowed and the bar shows the stale N/M, and an immediate enter
			// would re-commit the OLD query. rederiveLogs restores the full view.
			m.clearLogFilter()
			m.rederiveLogs()
			m.logFilterInput = textinput.New()
			m.logFiltering = true
			// Set Width on this PERSISTED model so bubbles scrolls the value to
			// keep the cursor visible (a value-receiver set in logBarLine would be
			// discarded). Width 0 = unbounded when size is unknown (tests).
			m.logFilterInput.Width = m.logFilterInputWidth()
			m.logFilterInput.Focus()
			return m, nil
		case "/":
			// Open search-within-highlight. Early-return when there is genuinely
			// nothing to search: no folded survivor (logFilterShown == 0), no
			// in-flight partial, and no terminal error. Testing derivedLogContent()
			// == "" instead would wrongly refuse to open over a single kept BLANK
			// survivor (logFilterShown == 1) — an empty rendered string that is
			// still a real searchable physical line. The input is built lazily
			// here — NOT in NewModel — so Model{} test literals stay valid; it is
			// only rendered while logSearching.
			if m.logFilterShown == 0 && m.logsPartial == "" && m.logsErrLine == "" {
				return m, nil
			}
			// Reset any prior committed search FIRST so reopening starts from a
			// clean slate (mirrors the container `/` clearSearch guard): stale
			// highlights + counter would otherwise persist and an immediate enter
			// would re-commit the OLD query. setLogViewportContent drops the stale
			// highlights immediately.
			m.clearLogSearch()
			m.setLogViewportContent()
			m.logSearchInput = textinput.New()
			m.logSearching = true
			// Set Width on the PERSISTED model so bubbles scrolls the value to keep
			// the cursor visible (see the filter-open comment above).
			m.logSearchInput.Width = m.logSearchInputWidth()
			m.logSearchInput.Focus()
			return m, nil
		case "n":
			// Cycle to the next match. No-op without a committed search; the
			// viewport does not bind n/N, so swallowing them when idle is a
			// no-op either way.
			m.cycleLogMatch(true)
			return m, nil
		case "N":
			m.cycleLogMatch(false)
			return m, nil
		default:
			var cmd tea.Cmd
			m.logsViewport, cmd = m.logsViewport.Update(msg)
			return m, cmd
		}

	case screenConfig:
		switch key {
		case "ctrl+c":
			return m.tryQuit()
		case "esc":
			m.configContent = nil
			m.configResolved = nil
			m.configViewport = viewport.Model{}
			m.configShowRes = false
			m.configErr = nil
			m.configValid = nil
			m.configValidMsg = ""
			m.screen = screenSelectContainers
			return m, nil
		case "r":
			m.configShowRes = !m.configShowRes
			if m.configShowRes {
				if m.configResolved != nil {
					m.configViewport.SetContent(string(m.configResolved))
				} else {
					return m, m.fetchConfigResolved()
				}
			} else {
				if m.configContent != nil {
					m.configViewport.SetContent(string(m.configContent))
				}
			}
			return m, nil
		case "e":
			cp, ok := m.composer.(ConfigProvider)
			if !ok {
				return m, nil
			}
			cmd, err := cp.EditCommand(m.ctx)
			if err != nil {
				m.configErr = err
				return m, nil
			}
			session := m.configSession
			return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
				return configEditDoneMsg{err: err, session: session}
			})
		default:
			var cmd tea.Cmd
			m.configViewport, cmd = m.configViewport.Update(msg)
			return m, cmd
		}

	case screenInspect:
		switch key {
		case "ctrl+c":
			return m.tryQuit()
		case "esc":
			m.clearInspect()
			// No refreshStatus(): the inspect screen is read-only and changes
			// no container state, so it returns like screenConfig rather than
			// like screenLogs/screenProgress.
			m.screen = screenSelectContainers
			return m, nil
		case "r":
			// Two-way toggle over two buffers already in hand — no refetch,
			// unlike screenConfig's r, whose resolved half is lazily fetched.
			m.inspectShowRaw = !m.inspectShowRaw
			m.setInspectContent()
			m.inspectViewport.GotoTop()
			return m, nil
		default:
			// up/down/pgup/pgdown reach the viewport here, matching screenConfig.
			var cmd tea.Cmd
			m.inspectViewport, cmd = m.inspectViewport.Update(msg)
			return m, cmd
		}

	case screenSettingsList:
		if m.settingsDelete {
			switch key {
			case "y":
				idx := m.settingsCursor
				newServers := slices.Clone(m.config.Servers)
				newServers = slices.Delete(newServers, idx, idx+1)

				// Clean up orphaned groups
				newGroups := cleanOrphanedGroups(m.config.Groups, newServers)

				tmpCfg := &config.Config{Groups: newGroups, Servers: newServers}
				if err := tmpCfg.Save(m.configPath); err != nil {
					m.settingsErr = fmt.Sprintf("save failed: %v", err)
					m.settingsDelete = false
					return m, nil
				}
				m.config.Groups = newGroups
				m.config.Servers = newServers
				m.servers = m.config.Servers
				m.serverEntries = buildServerEntries(m.servers)
				if m.settingsCursor >= len(m.config.Servers) && m.settingsCursor > 0 {
					m.settingsCursor--
				}
				m.fixServerCursor()
				m.settingsDelete = false
				m.settingsErr = ""
			case "n", "esc":
				m.settingsDelete = false
			}
			return m, nil
		}

		switch key {
		case "ctrl+c":
			return m.tryQuit()
		case "esc":
			m.screen = screenSelectServer
			m.settingsErr = ""
			return m, nil
		case "up", "k":
			if m.settingsCursor > 0 {
				m.settingsCursor--
			}
		case "down", "j":
			if m.settingsCursor < len(m.config.Servers)-1 {
				m.settingsCursor++
			}
		case "a":
			m.settingsEditing = -1
			m.settingsField = 0
			m.settingsColor = ""
			m.settingsErr = ""
			m.settingsInputs = initSettingsInputs()
			m.settingsInputs[0].Focus()
			m.screen = screenSettingsForm
			return m, nil
		case "enter", "e":
			if len(m.config.Servers) == 0 {
				return m, nil
			}
			srv := m.config.Servers[m.settingsCursor]
			m.settingsEditing = m.settingsCursor
			m.settingsField = 0
			if srv.Group != "" {
				m.settingsColor = m.config.GroupColor(srv.Group)
			} else {
				m.settingsColor = srv.Color
			}
			m.settingsErr = ""
			m.settingsInputs = initSettingsInputs()
			m.settingsInputs[0].SetValue(srv.Name)
			m.settingsInputs[1].SetValue(srv.Host)
			m.settingsInputs[2].SetValue(srv.ProjectDir)
			m.settingsInputs[3].SetValue(srv.Group)
			m.settingsInputs[0].Focus()
			m.screen = screenSettingsForm
			return m, nil
		case "d":
			if len(m.config.Servers) > 0 {
				m.settingsDelete = true
			}
		}

	case screenSettingsForm:
		switch key {
		case "ctrl+c":
			return m.tryQuit()
		case "esc":
			m.screen = screenSettingsList
			m.settingsErr = ""
			return m, nil
		case "tab", "down":
			if m.settingsField < 4 {
				m.settingsInputs[m.settingsField].Blur()
			}
			m.settingsField = (m.settingsField + 1) % 5
			if m.settingsField < 4 {
				m.settingsInputs[m.settingsField].Focus()
			}
		case "shift+tab", "up":
			if m.settingsField < 4 {
				m.settingsInputs[m.settingsField].Blur()
			}
			m.settingsField = (m.settingsField + 4) % 5
			if m.settingsField < 4 {
				m.settingsInputs[m.settingsField].Focus()
			}
		case "left":
			if m.settingsField == 4 {
				m.settingsColor = cycleColor(m.settingsColor, -1)
			} else {
				var cmd tea.Cmd
				m.settingsInputs[m.settingsField], cmd = m.settingsInputs[m.settingsField].Update(msg)
				return m, cmd
			}
		case "right":
			if m.settingsField == 4 {
				m.settingsColor = cycleColor(m.settingsColor, 1)
			} else {
				var cmd tea.Cmd
				m.settingsInputs[m.settingsField], cmd = m.settingsInputs[m.settingsField].Update(msg)
				return m, cmd
			}
		case "enter":
			srvGroup := strings.TrimSpace(m.settingsInputs[3].Value())
			srv := config.Server{
				Name:       strings.TrimSpace(m.settingsInputs[0].Value()),
				Host:       strings.TrimSpace(m.settingsInputs[1].Value()),
				ProjectDir: strings.TrimSpace(m.settingsInputs[2].Value()),
				Group:      srvGroup,
			}
			// Grouped servers have no per-server color; ungrouped keep it
			if srvGroup == "" {
				srv.Color = m.settingsColor
			}

			// Build temporary config for validation and save
			tmpServers := slices.Clone(m.config.Servers)
			if m.settingsEditing < 0 {
				tmpServers = append(tmpServers, srv)
			} else {
				tmpServers[m.settingsEditing] = srv
			}
			tmpGroups := slices.Clone(m.config.Groups)
			// Auto-create group if server references a new group name
			if srvGroup != "" {
				found := false
				for i, g := range tmpGroups {
					if g.Name == srvGroup {
						found = true
						// Apply settingsColor to the group
						tmpGroups[i].Color = m.settingsColor
						break
					}
				}
				if !found {
					tmpGroups = append(tmpGroups, config.Group{Name: srvGroup, Color: m.settingsColor})
				}
			}
			// Clean up orphaned groups
			tmpGroups = cleanOrphanedGroups(tmpGroups, tmpServers)
			tmpCfg := &config.Config{Groups: tmpGroups, Servers: tmpServers}
			if err := tmpCfg.Validate(); err != nil {
				m.settingsErr = err.Error()
				return m, nil
			}

			// Save first, only mutate live state on success
			if err := tmpCfg.Save(m.configPath); err != nil {
				m.settingsErr = fmt.Sprintf("save failed: %v", err)
				return m, nil
			}
			m.config.Groups = tmpGroups
			m.config.Servers = tmpServers
			m.servers = m.config.Servers
			m.serverEntries = buildServerEntries(m.servers)
			m.fixServerCursor()
			// Fix cursor for add
			if m.settingsEditing < 0 {
				m.settingsCursor = len(m.config.Servers) - 1
			}
			m.settingsErr = ""
			m.screen = screenSettingsList
			return m, nil
		default:
			if m.settingsField < 4 {
				var cmd tea.Cmd
				m.settingsInputs[m.settingsField], cmd = m.settingsInputs[m.settingsField].Update(msg)
				return m, cmd
			}
		}

	case screenProgress:
		switch key {
		case "ctrl+c":
			if m.done || m.failed {
				return m.tryQuit()
			}
		case "esc":
			if m.waiting {
				// Skip the health wait: stop polling and drop the (partial)
				// verdicts, but leave the operation "done". clearWaitState bumps
				// waitSession so any in-flight poll/tick is discarded. The user
				// stays on the progress screen in the plain done state; a second
				// esc falls through to the back-nav cleanup below.
				m.clearWaitState()
				return m, nil
			}
			if m.done || m.failed {
				// Invalidate the update-availability cache for the current
				// project/server on a SUCCESSFUL operation that may have
				// changed image freshness. Deploy pulls a new image (so the
				// previous "update available" glyph is almost certainly
				// stale); Restart picks up a newer image when the user has
				// `pull_policy: always` in compose, and is harmless to
				// invalidate otherwise (worst case: one extra CheckUpdates
				// call). Skipping invalidation on `m.failed` keeps a stale
				// cache rather than spuriously clearing user-visible state
				// after a failed deploy. The key comes from
				// updatesCacheKey(), so the next maybeRefreshUpdatesCmd
				// misses, sees no in-flight fetch, and enqueues a fresh
				// refresh. This block runs
				// BEFORE the m.done/m.failed reset below — otherwise
				// reading m.done after clearing it would always evaluate
				// false and the invalidation would never fire.
				if m.done && (m.pendingOp == runner.Deploy || m.pendingOp == runner.Restart || m.pendingOp == runner.Rollback) {
					if m.updateCache != nil {
						delete(m.updateCache, m.updatesCacheKey())
					}
				}
				m.screen = screenSelectContainers
				m.confirming = false
				m.steps = nil
				m.done = false
				m.failed = false
				m.eventCh = nil
				m.cancel = nil
				m.logContent = ""
				m.rollbackErr = ""
				m.rollbackSnapshot = nil
				m.rollbackTargets = nil
				// Invoke the rollback prep cleanup (override-file removal +
				// ExtraComposeFiles reset) NOW, on the main goroutine, AFTER the
				// wait phase — never goroutine-deferred (see the rollback cleanup
				// timing race rule: a concurrent reset would race the wait poll's
				// ContainerStatus read of ExtraComposeFiles). No-op for non-
				// rollback ops (cleanup is nil).
				m.runRollbackCleanup()
				// Clear any resolved-but-not-skipped wait verdicts as we leave
				// the progress screen (esc-skip already cleared them on the
				// prior press; this covers a natural resolution followed by a
				// single esc). Bumps waitSession to invalidate stragglers.
				m.clearWaitState()
				m.statsSession++
				m.statusSession++
				m.updatesSession++
				m.rollbackFetchSession++
				m.statsRequested = true
				m.refreshInFlight = true
				// Reset updateInFlight before calling maybeRefreshUpdatesCmd:
				// the session bump above invalidates any pending updatesMsg,
				// so the guard inside maybeRefreshUpdatesCmd must not refuse
				// to fire a fresh fetch just because a now-stale fetch is
				// still in flight.
				m.updateInFlight = false
				cmds := []tea.Cmd{m.refreshStatus(), m.refreshStats()}
				if c := m.maybeRefreshUpdatesCmd(); c != nil {
					cmds = append(cmds, c)
				}
				return m, tea.Batch(cmds...)
			}
			if m.cancel != nil {
				m.cancel()
			}
		}
		var cmd tea.Cmd
		m.logViewport, cmd = m.logViewport.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m Model) handleStepEvent(event runner.StepEvent) (tea.Model, tea.Cmd) {
	for i := range m.steps {
		if m.steps[i].name == event.Step {
			m.steps[i].status = event.Status
			break
		}
	}

	if event.Status == runner.StatusFailed {
		m.failed = true
		return m, nil
	}

	if event.Status == runner.StatusRunning {
		return m, tea.Batch(m.spinner.Tick, m.waitForEvent())
	}

	return m, m.waitForEvent()
}

func (m *Model) enterProgress(containers []string) (tea.Model, tea.Cmd) {
	op := m.pendingOp
	m.screen = screenProgress
	m.rollbackErr = ""
	// Leaving screenSelectContainers for an operation: search is ephemeral.
	m.clearSearch()

	stepNames := runner.Steps(op)
	m.steps = make([]stepState, len(stepNames))
	for i, name := range stepNames {
		m.steps[i] = stepState{name: name}
	}

	vpHeight := m.height - len(m.steps) - 8
	if vpHeight < 3 {
		vpHeight = 3
	}
	w := m.width - 4
	if w < 10 {
		w = 40
	}
	m.logViewport = viewport.New(w, vpHeight)

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	events := make(chan runner.StepEvent, 20)
	m.eventCh = events

	logW := m.logWriter
	if logW == nil {
		logW = io.Discard
	}

	// Remember the target set so pipelineDoneMsg can seed the health-wait
	// reducer without re-deriving it (Rollback in particular does not use the
	// container-screen selection).
	m.opContainers = containers

	if op == runner.Rollback {
		// Rollback prep (pull missing blobs, generate the override, set
		// ExtraComposeFiles) MUST run before runner.Run. It runs off the UI
		// thread as prepareRollbackCmd, which launches the pipeline goroutine
		// ITSELF only after a successful prep. Do NOT return waitForEvent here:
		// nothing writes to `events` until prep succeeds, so consuming events
		// starts on the rollbackPreppedMsg success path instead.
		return *m, tea.Batch(m.spinner.Tick, m.prepareRollbackCmd(ctx, containers, logW, events))
	}

	// For a Deploy, snapshot the currently-running image digests FIRST — inside
	// the same goroutine, before runner.Run touches anything (pre-Stop ordering)
	// — so `cdeploy rollback` / the TUI `R` key can restore them. Best-effort:
	// warnings go to the op log writer and never block the pipeline. Composers
	// without the capability (test mocks) skip it entirely. Values are captured
	// into locals so the goroutine never races the returning *Model.
	composer := m.composer
	go func() {
		if op == runner.Deploy {
			if s, ok := composer.(Snapshotter); ok {
				recordDeploySnapshot(ctx, s, containers, logW)
			}
		}
		runner.Run(ctx, composer, op, containers, logW, events)
	}()

	return *m, tea.Batch(m.spinner.Tick, m.waitForEvent())
}

// fetchRollbackSnapshot returns a Cmd that reads the host-side deploy snapshot
// and delivers it as a rollbackSnapshotMsg tagged with the current fetch
// session. The read hits the disk (local) or SSH (remote), so it runs off the UI
// thread. The `R` handler asserts RollbackPreparer before calling; the nil guard
// here keeps the helper total for any future caller.
func (m Model) fetchRollbackSnapshot() tea.Cmd {
	p, ok := m.composer.(RollbackPreparer)
	if !ok {
		return nil
	}
	composer := m.composer
	ctx := m.ctx
	session := m.rollbackFetchSession
	return func() tea.Msg {
		snap, err := p.ReadSnapshot(ctx)
		if err != nil {
			return rollbackSnapshotMsg{err: err, session: session}
		}
		// No snapshot to restore — the handler shows the warning; skip the
		// live-service fetch (nothing to intersect), matching the CLI ordering
		// which refuses on a missing snapshot before calling ListServices.
		if snap == nil || len(snap.Services) == 0 {
			return rollbackSnapshotMsg{snap: snap, session: session}
		}
		// Fetch the CURRENT compose service set so the handler can drop any
		// selected target that has since been removed from the compose file
		// (mirrors the CLI filterLiveTargets — a stale snapshot entry must not
		// resurrect a removed service as an image-only override). Runs in the same
		// off-UI goroutine as ReadSnapshot; a ListServices failure fails closed via
		// the shared err path (the handler shows "rollback unavailable: ...").
		live, err := composer.ListServices(ctx)
		if err != nil {
			return rollbackSnapshotMsg{err: fmt.Errorf("listing current compose services: %w", err), session: session}
		}
		return rollbackSnapshotMsg{snap: snap, live: live, session: session}
	}
}

// prepareRollbackCmd runs the rollback prep off the UI thread: it asserts the
// RollbackPreparer capability, calls PrepareRollback with the fetched snapshot
// entries (which sets ExtraComposeFiles), and — ONLY on success — launches the
// Rollback pipeline goroutine. Reading ExtraComposeFiles in runner.Run is
// race-free: the `go` statement happens-after PrepareRollback's write. The
// returned rollbackPreppedMsg carries the cleanup (stored on the Model, invoked
// on leaving screenProgress) or the prep error (op marked failed). ctx is the
// cancelable pipeline context so an esc-cancel aborts an in-flight pull.
func (m Model) prepareRollbackCmd(ctx context.Context, containers []string, logW io.Writer, events chan runner.StepEvent) tea.Cmd {
	composer := m.composer
	snap := m.rollbackSnapshot
	return func() tea.Msg {
		p, ok := composer.(RollbackPreparer)
		if !ok {
			return rollbackPreppedMsg{err: fmt.Errorf("rollback is not supported for this connection")}
		}
		var entries map[string]compose.SnapshotEntry
		if snap != nil {
			entries = snap.Services
		}
		cleanup, err := p.PrepareRollback(ctx, entries, containers, logW)
		if err != nil {
			return rollbackPreppedMsg{err: err}
		}
		go runner.Run(ctx, composer, runner.Rollback, containers, logW, events)
		return rollbackPreppedMsg{cleanup: cleanup}
	}
}

// rollbackAgeSuffix formats a compact " to snapshot (3h ago)" hint for the
// rollback confirm prompt using the NEWEST recorded_at among the target services
// (merge keeps per-service ages, so the newest is the most representative "last
// deploy"). Returns "" when no snapshot/age is available so the prompt degrades
// to the plain service list.
func (m Model) rollbackAgeSuffix(targets []string) string {
	if m.rollbackSnapshot == nil {
		return ""
	}
	var newest time.Time
	for _, svc := range targets {
		entry, ok := m.rollbackSnapshot.Services[svc]
		if !ok {
			continue
		}
		if t, err := time.Parse(time.RFC3339, entry.RecordedAt); err == nil && t.After(newest) {
			newest = t
		}
	}
	if newest.IsZero() {
		return ""
	}
	return " to snapshot (" + humanizeAge(time.Since(newest)) + ")"
}

// humanizeAge renders a duration as a compact relative age (e.g. "3h ago",
// "2d ago", "moments ago") for the rollback confirm prompt.
func humanizeAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "moments ago"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// recordDeploySnapshot captures the currently-running image digests for the
// targeted services and merge-writes them to the host state file. Strictly
// best-effort (mirrors cmd.recordSnapshot): a capture error, per-service
// warnings, and a write failure are all surfaced to the op log writer without
// ever aborting — a snapshot problem must never block the deploy that follows.
func recordDeploySnapshot(ctx context.Context, s Snapshotter, services []string, warn io.Writer) {
	res, err := s.SnapshotServices(ctx, services)
	if err != nil {
		fmt.Fprintf(warn, "Warning: rollback snapshot skipped: %v\n", err)
		return
	}
	for _, msg := range res.Warnings {
		fmt.Fprintf(warn, "Warning: rollback snapshot: %s\n", msg)
	}
	if err := s.WriteSnapshot(ctx, res.Snapshot); err != nil {
		fmt.Fprintf(warn, "Warning: failed to write rollback snapshot: %v (deploy continues)\n", err)
	}
}

// waitForEvent returns a Cmd that waits for the next StepEvent.
func (m Model) waitForEvent() tea.Cmd {
	ch := m.eventCh
	return func() tea.Msg {
		event, ok := <-ch
		if !ok {
			return pipelineDoneMsg{}
		}
		return stepEventMsg(event)
	}
}

// pollWaitStatus returns a Cmd that fetches one ContainerStatus snapshot and
// delivers it as a waitStatusMsg tagged with the current wait session. It has no
// delay of its own — the poll cadence comes from waitTick between polls.
//
// Each poll is bounded by a context derived from m.waitDeadline (the same wall
// clock the countdown uses), NOT the raw m.ctx: a HUNG Docker daemon or a stalled
// SSH status call would otherwise never return a waitStatusMsg, so the reducer's
// timeout sweep would never run and the progress screen would wait indefinitely.
// With the deadline bound, a hung poll returns a deadline error at ~m.waitDeadline
// and the waitStatusMsg error path (elapsed >= timeout → sweepWaitTimeout) resolves
// the wait as timed out. This mirrors the CLI driver's wait-scoped context (C5/C7).
func (m Model) pollWaitStatus() tea.Cmd {
	ctx := m.ctx
	deadline := m.waitDeadline
	c := m.composer
	session := m.waitSession
	return func() tea.Msg {
		pollCtx, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()
		status, err := c.ContainerStatus(pollCtx)
		return waitStatusMsg{status: status, err: err, session: session}
	}
}

// waitTick returns a Cmd that fires a waitTickMsg after runner.DefaultWaitPoll,
// spacing successive health polls. The session is captured so a tick outliving
// its wait (esc-skip / departure bumped waitSession) is dropped by the handler.
// Tests install waitTickOverride to avoid leaving a real timer goroutine behind.
func (m Model) waitTick() tea.Cmd {
	if m.waitTickOverride != nil {
		return m.waitTickOverride()
	}
	session := m.waitSession
	return tea.Tick(runner.DefaultWaitPoll, func(time.Time) tea.Msg {
		return waitTickMsg{session: session}
	})
}

// clearWaitState resets the health-wait sub-state and bumps waitSession so any
// in-flight poll/tick is invalidated. Called at every departure site and on
// esc-skip. Idempotent — safe to call when no wait is active (it just re-bumps
// the session over already-zero fields).
func (m *Model) clearWaitState() {
	m.waiting = false
	m.waitState = runner.WaitState{}
	m.waitDeadline = time.Time{}
	m.opContainers = nil
	m.waitSession++
}

// waitElapsed returns the time since the wait began, derived from the deadline
// and the fixed default timeout the TUI always uses (no --wait-timeout in the
// TUI). Fed to EvaluateWait, which owns the grace and timeout rules.
func (m Model) waitElapsed() time.Duration {
	return runner.DefaultWaitTimeout - time.Until(m.waitDeadline)
}

// waitResolved reports whether a wait ran to completion (verdicts present but no
// longer actively polling). Distinct from esc-skip, which clears waitState.
func (m Model) waitResolved() bool {
	return !m.waiting && len(m.waitState.Services) > 0
}

// waitFailed reports whether any targeted service holds a non-passing verdict.
// At resolution no verdict is still pending (the timeout sweep converts pending
// → timed out), so this is an accurate pass/fail signal once waitResolved().
func (m Model) waitFailed() bool {
	for _, svc := range m.waitState.Services {
		if !m.waitState.Verdicts[svc].OK() {
			return true
		}
	}
	return false
}

// sweepWaitTimeout marks every still-pending service as timed out. Used on the
// poll-error path once the deadline passes, so a persistent ContainerStatus
// failure resolves the wait instead of hanging (EvaluateWait can't run without
// a trustworthy status map).
func (m *Model) sweepWaitTimeout() {
	for _, svc := range m.waitState.Services {
		if !m.waitState.Verdicts[svc].Terminal() {
			m.waitState.Verdicts[svc] = runner.VerdictTimedOut
		}
	}
}

// waitVerdictStyle picks the lipgloss style for a verdict: green pass, yellow
// pending, red fail.
func waitVerdictStyle(v runner.WaitVerdict) lipgloss.Style {
	switch {
	case v.OK():
		return waitPassStyle
	case v == runner.VerdictPending:
		return waitPendingStyle
	default:
		return waitFailStyle
	}
}

func (m *Model) enterConfig() (tea.Model, tea.Cmd) {
	m.configSession++
	m.configContent = nil
	m.configResolved = nil
	m.configShowRes = false
	m.configErr = nil
	m.configValid = nil
	m.configValidMsg = ""

	vpHeight := m.height - 6
	if vpHeight < 3 {
		vpHeight = 3
	}
	w := m.width - 4
	if w < 10 {
		w = 40
	}
	m.configViewport = viewport.New(w, vpHeight)

	m.screen = screenConfig
	// Leaving screenSelectContainers for the config screen: search is ephemeral.
	m.clearSearch()
	return *m, m.fetchConfigFile()
}

// enterInspect opens the read-only inspect screen for the service under the
// cursor. Modelled on enterConfig: bump the session, reset every inspect field,
// size the viewport with the config sizing (m.height - 6, NOT the logs -7 which
// reserves a row for the log bar), then return the fetch command.
func (m *Model) enterInspect() (tea.Model, tea.Cmd) {
	m.inspectSession++
	m.inspectService = m.services[m.svcCursor]
	m.inspectRaw = nil
	m.inspectSummary = ""
	m.inspectShowRaw = false
	m.inspectErr = nil

	vpHeight := m.height - 6
	if vpHeight < 3 {
		vpHeight = 3
	}
	w := m.width - 4
	if w < 10 {
		w = 40
	}
	m.inspectViewport = viewport.New(w, vpHeight)
	// Raw mode is the escape hatch, and real `docker inspect` output carries
	// lines hundreds of columns wide (a LowerDir list, a JSON-escaped probe
	// Output). The viewport hard-cuts at its width with no wrap, so without a
	// horizontal step left/right are inert and everything past the edge is
	// unreachable. 4 is the step enterLogs uses when wrap is off. An offset
	// left behind by a sideways scroll would blank the wrapped summary, so
	// setInspectContent resets it on every buffer change.
	m.inspectViewport.SetHorizontalStep(4)

	m.screen = screenInspect
	// Leaving screenSelectContainers for the inspect screen: search is ephemeral.
	m.clearSearch()
	return *m, m.fetchInspect(m.inspectSession)
}

func (m *Model) enterExec() (tea.Model, tea.Cmd) {
	ep, ok := m.composer.(ExecProvider)
	if !ok {
		m.confirming = false
		m.pendingExec = false
		return *m, nil
	}
	service := m.services[m.svcCursor]
	cmd, err := ep.ExecCommand(m.ctx, service, nil)
	if err != nil {
		m.warning = fmt.Sprintf("exec failed: %v", err)
		m.confirming = false
		m.pendingExec = false
		m.fixSvcOffset()
		return *m, nil
	}
	// Leaving screenSelectContainers to exec into a container: search is
	// ephemeral. Cleared only on the success path (the early-return failure
	// paths above keep the user on the container screen).
	m.clearSearch()
	return *m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		return execDoneMsg{err: err}
	})
}

func (m Model) fetchConfigFile() tea.Cmd {
	cp, ok := m.composer.(ConfigProvider)
	if !ok {
		return nil
	}
	ctx := m.ctx
	session := m.configSession
	return func() tea.Msg {
		data, err := cp.ConfigFile(ctx)
		return configFileMsg{data: data, err: err, session: session}
	}
}

func (m Model) fetchConfigResolved() tea.Cmd {
	cp, ok := m.composer.(ConfigProvider)
	if !ok {
		return nil
	}
	ctx := m.ctx
	session := m.configSession
	return func() tea.Msg {
		data, err := cp.ConfigResolved(ctx)
		return configResolvedMsg{data: data, err: err, session: session}
	}
}

// rebuildInspectSummary re-parses the cached raw bytes and re-renders the
// curated summary at the viewport's current width. A parse failure is surfaced
// in the error slot and leaves the summary empty — the raw bytes are still
// intact, so the `r` toggle remains a working escape hatch.
func (m *Model) rebuildInspectSummary() {
	if len(m.inspectRaw) == 0 {
		m.inspectSummary = ""
		return
	}
	doc, err := compose.ParseInspect(m.inspectRaw)
	if err != nil {
		m.inspectErr = err
		m.inspectSummary = ""
		// The mode is NOT forced here. The fetch handler switches to raw once,
		// on the transition into the error state; this function also runs on
		// every resize, and forcing it there would undo the user's `r`.
		return
	}
	m.inspectErr = nil
	m.inspectSummary = buildInspectSummary(doc, m.inspectViewport.Width)
}

// clearInspect resets every inspect field on departure. The session is NOT
// bumped here — enterInspect bumps it on the way in, which is what invalidates
// an in-flight fetch; the handler's screen check already discards one that
// lands after this.
func (m *Model) clearInspect() {
	m.inspectService = ""
	m.inspectRaw = nil
	m.inspectSummary = ""
	m.inspectShowRaw = false
	m.inspectViewport = viewport.Model{}
	m.inspectErr = nil
}

// setInspectContent is the single SetContent chokepoint for the inspect
// viewport, so the mode toggle, the fetch handler and the resize branch cannot
// disagree about which buffer is on screen.
func (m *Model) setInspectContent() {
	if m.inspectShowRaw {
		// The raw bytes are filtered, not rewritten: docker's JSON escapes only
		// the C0 block, so DEL and the C1 escape introducers still arrive raw.
		// See sanitizeInspectRaw.
		m.inspectViewport.SetContent(sanitizeInspectRaw(m.inspectRaw))
	} else {
		m.inspectViewport.SetContent(m.inspectSummary)
	}
	// The horizontal offset is reset with every buffer change, and that is
	// load-bearing: SetContent keeps xOffset and GotoTop resets only YOffset,
	// so a sideways scroll through the raw JSON would survive the `r` toggle.
	// The summary is wrapped to the pane, so its longestLineWidth never exceeds
	// the width — visibleLines() would then cut EVERY line at
	// [xOffset, xOffset+width] and render a blank screen with no key that
	// recovers it except an undocumented left. The raw path resets for the
	// same reason on a resize that widens the pane past the longest line.
	m.inspectViewport.SetXOffset(0)
}

// fetchInspect runs `docker inspect` for the service being inspected. The
// session is passed in rather than read off the Model so the value captured is
// the one live at the call site, matching refreshUpdates.
func (m Model) fetchInspect(session uint64) tea.Cmd {
	ins, ok := m.composer.(Inspector)
	if !ok {
		return nil
	}
	ctx := m.ctx
	service := m.inspectService
	return func() tea.Msg {
		data, err := ins.Inspect(ctx, service)
		return inspectDataMsg{data: data, err: err, session: session}
	}
}

func (m Model) fetchConfigValidate() tea.Cmd {
	cp, ok := m.composer.(ConfigProvider)
	if !ok {
		return nil
	}
	ctx := m.ctx
	session := m.configSession
	return func() tea.Msg {
		err := cp.ValidateConfig(ctx)
		return configValidateMsg{err: err, session: session}
	}
}

func (m *Model) enterLogs() (tea.Model, tea.Cmd) {
	service := m.services[m.svcCursor]
	m.logsService = service
	m.logsDone = false
	m.logsErr = nil
	m.logsSession++
	m.logsWrap = true
	m.logsPretty = false
	m.logsRawLines = nil
	m.logsPartial = ""
	m.logsScanned = 0
	m.logsFormatted = ""
	m.logFilterShown = 0
	m.logsErrLine = ""
	m.clearLogFilter()
	m.clearLogSearch()

	// -7 (not -6): one row is reserved below the viewport for the log bar line
	// (blank when idle). The two config sites keep -6 — do NOT unify these.
	vpHeight := m.height - 7
	if vpHeight < 3 {
		vpHeight = 3
	}
	w := m.width - 4
	if w < 10 {
		w = 40
	}
	m.logsViewport = viewport.New(w, vpHeight)
	// Wrap is on by default, so disable horizontal scrolling
	m.logsViewport.SetHorizontalStep(0)

	ctx, cancel := context.WithCancel(m.ctx)
	m.logsCancel = cancel

	pr, pw := io.Pipe()
	m.logsPipeR = pr

	composer := m.composer
	go func() {
		err := composer.Logs(ctx, service, true, 50, pw)
		pw.CloseWithError(err)
	}()

	m.screen = screenLogs
	// Leaving screenSelectContainers for the log viewer: search is ephemeral.
	m.clearSearch()
	return *m, m.readLogChunk()
}

// appendRawChunk splits streamed bytes into complete logical lines — appending
// them to logsRawLines — and retains any trailing bytes with no newline yet in
// logsPartial until a later chunk completes them. Splitting on '\n' only (a
// trailing '\r' stays on the line) preserves the pre-refactor line semantics.
func (m *Model) appendRawChunk(data []byte) {
	s := m.logsPartial + string(data)
	for {
		idx := strings.IndexByte(s, '\n')
		if idx < 0 {
			break
		}
		m.logsRawLines = append(m.logsRawLines, s[:idx])
		s = s[idx+1:]
	}
	m.logsPartial = s
}

// applyLogFormat derives the viewport content from the raw-line buffer. It folds
// only the not-yet-processed tail of logsRawLines (via foldNewRawLines) through
// the filter predicate and the wrap/pretty formatter, appends the result to the
// cached logsFormatted, and advances logsScanned per raw line scanned (NOT per
// survivor — see the survivor-cursor trap in foldNewRawLines). With no filter
// committed a nil (all-pass) predicate is passed, so the behaviour is identical
// to the unfiltered pipeline. Call fullReformat() when the filter, toggles, or
// width change.
func (m *Model) applyLogFormat() {
	delta, newScanned, survivors := foldNewRawLines(m.logsRawLines, m.logsScanned, m.logsViewport.Width, m.logsWrap, m.logsPretty, m.logFilterPred())
	if survivors > 0 {
		// Gate the append on the survivor COUNT, not on delta != "": a KEPT blank
		// raw line (empty / whitespace-only line that passes the filter) formats to
		// an empty delta yet is still a real physical line that must round-trip.
		// Deciding on delta != "" would drop it AND elide the "\n" separator, so the
		// next non-blank line would merge onto its row — making the rendered output
		// (and the search physical-line indices) depend on how the byte stream was
		// chunked. The seed-vs-append choice also can't use logsFormatted == ""
		// (ambiguous: empty both before the first fold and after only blank
		// survivors); logFilterShown — the count of survivors already folded into
		// logsFormatted, read here before the increment below — is the authoritative
		// "have we folded any line yet" signal.
		if m.logFilterShown == 0 {
			m.logsFormatted = delta
		} else {
			m.logsFormatted += "\n" + delta
		}
	}
	m.logsScanned = newScanned
	// Accumulate the survivor count incrementally so logFilterCounts is O(1) per
	// render frame instead of rescanning the whole buffer. fullReformat resets
	// this to 0 before re-accumulating over the full buffer.
	m.logFilterShown += survivors
	m.setLogViewportContent()
}

// derivedLogContent assembles the exact string handed to the log viewport:
// the cached formatted survivors, then the unfiltered in-flight partial line,
// then the filter-exempt terminal error. It is pure over the current Model
// state so tests can assert on the derived content directly.
func (m *Model) derivedLogContent() string {
	content := m.logsFormatted
	// hasContent tracks whether a real physical line already sits in `content`.
	// It CANNOT be derived from content != "": a single kept BLANK survivor folds
	// to logsFormatted == "" yet is a real physical line (logFilterShown == 1), so
	// an empty string is ambiguous. logFilterShown > 0 is the authoritative
	// "have we folded any line" signal; the flag then flips true once the partial
	// is appended so a later error still separates from it correctly (though in
	// practice logDoneMsg flushes logsPartial before setting logsErrLine, so a
	// partial and an error never coexist — the accumulator is the safe superset).
	hasContent := m.logFilterShown > 0
	if m.logsPartial != "" {
		tail := formatLogLines([]string{m.logsPartial}, m.logsViewport.Width, m.logsWrap, m.logsPretty)
		if hasContent {
			content += "\n" + tail
		} else {
			content = tail
		}
		hasContent = true
	}
	if m.logsErrLine != "" {
		errFmt := formatLogLines([]string{m.logsErrLine}, m.logsViewport.Width, m.logsWrap, m.logsPretty)
		if hasContent {
			content += "\n\n" + errFmt // blank line before the terminal error
		} else {
			content = errFmt
		}
	}
	return content
}

// fullReformat re-processes all content from scratch. Used when toggles change
// or the viewport is resized, since width/mode changes affect every line.
func (m *Model) fullReformat() {
	m.logsScanned = 0
	m.logsFormatted = ""
	m.logFilterShown = 0
	m.applyLogFormat()
}

// logFilterPred reconstructs the last-good filter predicate from the committed
// state. logFilterQuery and logFilterCommittedRegex are only ever set together
// (by recomputeLogFilter, when buildMatcher succeeded), so this always
// reproduces a valid predicate. Regex-ness is read from logFilterCommittedRegex,
// NOT from the live logFilterIsRegex — so a mid-type ctrl+r into a *bad* regex
// keeps the last-good matcher rather than silently dropping the filter. Returns
// nil (all-pass) when no filter is committed, degrading the derivation pipeline
// to an unfiltered pass-through. buildMatcher recompiles the regex once per call
// and the returned closure reuses it across every line inside deriveFiltered.
func (m Model) logFilterPred() func(string) bool {
	if m.logFilterQuery == "" {
		return nil
	}
	pred, _, _ := buildMatcher(m.logFilterQuery, m.logFilterCommittedRegex, true)
	return pred
}

// rederiveLogs re-derives the entire viewport content from the full raw buffer
// through the current filter predicate. It is the filter-change analogue of the
// w/p toggle path: capture AtBottom() before and re-pin to the bottom after when
// following, so a live tail stays pinned to the filtered tail across a filter
// change. The raw buffer is never touched — only the derived content narrows.
func (m *Model) rederiveLogs() {
	following := m.logsViewport.AtBottom()
	m.fullReformat()
	if following {
		m.logsViewport.GotoBottom()
	}
}

// recomputeLogFilter rebuilds the committed filter from the live input value and
// the live regex mode, then re-derives. An empty query (or a lone "!") clears the
// filter so every line shows again. A non-empty query that fails to compile as a
// regex is a mid-type error: keep the last-good query/predicate (no thrash) and
// skip the re-derive so the view holds steady. logFilterCommittedRegex records
// whether the last-good query is a regex so logFilterPred can reproduce it.
func (m *Model) recomputeLogFilter() {
	newQuery := m.logFilterInput.Value()
	stripped := newQuery
	if strings.HasPrefix(stripped, "!") {
		stripped = stripped[1:]
	}
	if stripped == "" {
		// Empty (or lone "!") query — clear the filter, reveal everything.
		if m.logFilterQuery != "" {
			m.logFilterQuery = ""
			m.logFilterCommittedRegex = false
			m.rederiveLogs()
		}
		return
	}
	_, re, valid := buildMatcher(newQuery, m.logFilterIsRegex, true)
	if !valid {
		// Bad regex mid-type: keep the last-good predicate, no re-derive.
		return
	}
	m.logFilterQuery = newQuery
	m.logFilterCommittedRegex = re != nil // re non-nil only for a valid regex
	m.rederiveLogs()
}

// clearLogFilter resets every filter field to the no-filter default. Called on
// entry to the log screen and on the esc-to-containers cleanup, and used by the
// typing-cancel path to discard an in-progress query.
func (m *Model) clearLogFilter() {
	m.logFiltering = false
	m.logFilterQuery = ""
	m.logFilterIsRegex = false
	m.logFilterCommittedRegex = false
	m.logFilterInput.SetValue("")
	m.logFilterInput.Blur()
}

// logSearchPred reconstructs the live search predicate from the committed search
// state. It mirrors logFilterPred but with allowNegate=false — search has no "!"
// exclusion (that is a filter-only affordance). Regex-ness is read from
// logSearchCommittedRegex, so a mid-type ctrl+r into a *bad* regex keeps the
// last-good matcher. Returns nil (no highlight) when no query is active.
func (m Model) logSearchPred() func(string) bool {
	if m.logSearchQuery == "" {
		return nil
	}
	pred, _, _ := buildMatcher(m.logSearchQuery, m.logSearchCommittedRegex, false)
	return pred
}

// setLogViewportContent is the single SetContent chokepoint for the log screen.
// It assembles the derived (filtered) content and — when a search is active —
// recomputes logSearchMatches over the PHYSICAL lines and overlays the highlight
// as a SetContent-TIME pass (logsFormatted itself stays UNSTYLED). Routing every
// content write through here means live streaming (applyLogFormat), filter
// re-derivation (rederiveLogs), and w/p/resize reformats all pick up the search
// highlight and fresh match set without duplicating the split/compute/highlight
// logic. Search runs over the filtered survivors: a line hidden by the filter
// is absent from derivedLogContent, so it can never be a match.
func (m *Model) setLogViewportContent() {
	content := m.derivedLogContent()
	// Zero-match placeholder: a committed filter with ZERO survivors
	// (logFilterShown == 0) and no in-flight partial / terminal error to show
	// would otherwise leave a blank viewport. Gate on the survivor COUNT, not on
	// content == "": a filter that keeps exactly one BLANK line has an empty
	// rendered string yet a real searchable physical line (logFilterShown == 1),
	// so content == "" would wrongly show the placeholder and suppress search over
	// that blank line. When zero survivors there is nothing to search, so skip the
	// highlight pass.
	if m.logFilterShown == 0 && m.logFilterQuery != "" && m.logsPartial == "" && m.logsErrLine == "" {
		m.logSearchMatches = nil
		m.logsViewport.SetContent(descStyle.Render("  (no lines match filter)"))
		return
	}
	pred := m.logSearchPred()
	if pred == nil {
		m.logSearchMatches = nil
		m.logsViewport.SetContent(content)
		return
	}
	physical := strings.Split(content, "\n")
	m.logSearchMatches = logComputeMatches(physical, pred)
	// Keep the current-match cursor in range. Append-only streaming preserves
	// existing indices (the prefix is stable), so a committed cursor holds; a
	// filter/toggle/resize that shrinks the set clamps back to the first match.
	if m.logSearchCur >= len(m.logSearchMatches) {
		m.logSearchCur = 0
	}
	cur := -1
	if len(m.logSearchMatches) > 0 {
		cur = m.logSearchMatches[m.logSearchCur]
	}
	m.logsViewport.SetContent(strings.Join(highlightMatches(physical, m.logSearchMatches, cur), "\n"))
}

// recomputeLogSearch rebuilds the live search from the input value and regex
// mode, re-highlights, and live-jumps to the first match. An empty query clears
// the highlight; a bad regex mid-type keeps the last-good matcher (no thrash),
// mirroring recomputeLogFilter. Called on every keystroke while the search bar
// is open and on ctrl+r.
func (m *Model) recomputeLogSearch() {
	newQuery := m.logSearchInput.Value()
	if newQuery == "" {
		m.logSearchQuery = ""
		m.logSearchCommittedRegex = false
		m.logSearchCur = 0
		m.setLogViewportContent() // clears matches + highlight
		return
	}
	_, re, valid := buildMatcher(newQuery, m.logSearchIsRegex, false)
	if !valid {
		return // bad regex mid-type: keep last-good, no re-highlight
	}
	m.logSearchQuery = newQuery
	m.logSearchCommittedRegex = re != nil // re non-nil only for a valid regex
	m.logSearchCur = 0
	m.setLogViewportContent() // recomputes logSearchMatches
	if len(m.logSearchMatches) > 0 {
		m.scrollLogMatchIntoView(m.logSearchMatches[0])
	}
}

// cycleLogMatch moves the current match forward (n) or backward (N) through
// logSearchMatches with wrap-around, scrolls the new match into view, and
// re-highlights so the bold "current" style follows. No-op when no search is
// committed or there are no matches. Mirrors cycleMatch (container search) but
// over physical log-line indices with a dedicated logSearchCur cursor.
func (m *Model) cycleLogMatch(forward bool) {
	n := len(m.logSearchMatches)
	if m.logSearchQuery == "" || n == 0 {
		return
	}
	if forward {
		m.logSearchCur = (m.logSearchCur + 1) % n
	} else {
		m.logSearchCur = (m.logSearchCur - 1 + n) % n
	}
	m.scrollLogMatchIntoView(m.logSearchMatches[m.logSearchCur])
	m.setLogViewportContent()
}

// scrollLogMatchIntoView scrolls the log viewport so the given physical line is
// visible: a no-op when it already sits within the visible window, otherwise it
// pins the line to the top. Scrolling up off the bottom auto-pauses follow (the
// AtBottom heuristic); G resumes. SetYOffset clamps into range, so a match near
// the bottom lands visible without over-scrolling.
func (m *Model) scrollLogMatchIntoView(line int) {
	top := m.logsViewport.YOffset
	h := m.logsViewport.Height
	if line < top || line >= top+h {
		m.logsViewport.SetYOffset(line)
	}
}

// logSearchCounter renders the "(i/N)" position indicator for the search bar
// (logBarLine renders the bar; this keeps the counter logic beside the state).
// "(no match)" when the query matched nothing; i is 1-based (logSearchCur+1).
func (m Model) logSearchCounter() string {
	n := len(m.logSearchMatches)
	if n == 0 {
		return "(no match)"
	}
	return fmt.Sprintf("(%d/%d)", m.logSearchCur+1, n)
}

// logModeTag renders the active match-mode tag for the log bar: "[rx]" for regex,
// "[literal]" for substring. Shown while an input is open so ctrl+r's toggle is
// visible; the committed summary uses a compact "[rx]"-only form (omitted for
// literal) instead.
func logModeTag(isRegex bool) string {
	if isRegex {
		return "[rx]"
	}
	return "[literal]"
}

// logModeTagMaxWidth is the display width of the widest mode tag ("[literal]"),
// reserved by the log-input width budgets so a ctrl+r toggle never shrinks the
// input's horizontal-scroll window (mirrors searchInputWidth reserving the widest
// counter, not the live one).
var logModeTagMaxWidth = ansi.StringWidth(logModeTag(false))

// logInputWidth is the arithmetic core of the log-bar input width budgets — the
// analogue of searchInputWidth. It returns m.width minus the 4-col bar prefix
// ("  / " or "  f "), the textinput's own 2-col prompt ("> "), and a
// keystroke-stable reservation for everything the bar renders AFTER the value
// (suffixWidth). Setting the returned value as textinput.Width on the PERSISTED
// model lets bubbles scroll the value horizontally to keep the cursor visible.
// Returns 0 (bubbles' "unbounded") when m.width <= 0 (unknown size, e.g. tests).
func (m Model) logInputWidth(suffixWidth int) int {
	if m.width <= 0 {
		return 0
	}
	const prefixWidth = 4 // "  / " or "  f "
	const promptWidth = 2 // textinput default prompt "> "
	budget := m.width - prefixWidth - promptWidth - suffixWidth
	if budget < 1 {
		budget = 1
	}
	return budget
}

// logSearchInputWidth returns the horizontal-scroll budget for the open log
// SEARCH input. The suffix reserves " <modeTag> <counter>" at its WIDEST stable
// form — the widest mode tag plus the widest counter for the current raw-line
// count. Magnitudes use len(logsRawLines) (stable per keystroke) rather than the
// live match count so the budget — and thus bubbles' scroll offset — doesn't
// fluctuate as matches change while typing (same rationale as maxCounterWidth).
func (m Model) logSearchInputWidth() int {
	n := len(m.logsRawLines)
	counter := ansi.StringWidth("(no match)")
	if c := ansi.StringWidth(fmt.Sprintf("(%d/%d)", n, n)); c > counter {
		counter = c
	}
	suffix := 1 + logModeTagMaxWidth + 1 + counter // " " modeTag " " counter
	return m.logInputWidth(suffix)
}

// logFilterInputWidth mirrors logSearchInputWidth for the open log FILTER input.
// The suffix reserves the widest of " <modeTag> · N/M shown" (counts at their
// len(logsRawLines) maximum) and " <modeTag> (bad regex)".
func (m Model) logFilterInputWidth() int {
	n := len(m.logsRawLines)
	trailing := ansi.StringWidth(fmt.Sprintf(" · %d/%d shown", n, n))
	if b := ansi.StringWidth(" (bad regex)"); b > trailing {
		trailing = b
	}
	suffix := 1 + logModeTagMaxWidth + trailing // " " modeTag <trailing>
	return m.logInputWidth(suffix)
}

// logFilterCounts returns the survivor and total RAW-line counts for the current
// committed filter predicate. Drives the "N/M shown" segment of the log bar. It
// counts raw logical lines (not pretty-expanded physical lines), matching how
// the filter runs (before pretty-expansion). The survivor count is read from the
// logFilterShown cache (maintained incrementally by applyLogFormat/fullReformat)
// so this is O(1) per render frame rather than an O(buffered lines) rescan. With
// no filter committed every line passes, so survivors == total (and the possibly
// stale cache is never read).
func (m Model) logFilterCounts() (survivors, total int) {
	total = len(m.logsRawLines)
	if m.logFilterQuery == "" {
		return total, total
	}
	return m.logFilterShown, total
}

// logFilterBadRegex reports whether the LIVE filter input is in regex mode with a
// non-empty query that fails to compile — the "(bad regex)" state the typing bar
// surfaces while recomputeLogFilter holds the last-good predicate. A lone "!" (or
// empty input) is not "bad", just incomplete.
func (m Model) logFilterBadRegex() bool {
	if !m.logFilterIsRegex {
		return false
	}
	q := m.logFilterInput.Value()
	if strings.TrimPrefix(q, "!") == "" {
		return false
	}
	_, _, ok := buildMatcher(q, true, true)
	return !ok
}

// logBarLine renders the reserved one-physical-line bar shown below the log
// viewport (the log analogue of the container searchBarLine). Content precedence:
//
//	typing search  → "/ <q> [mode] (i/N)"                (or "(no match)")
//	typing filter  → "f <q> [mode] · N/M shown"          (or "(bad regex)")
//	committed      → "filter: <q> [rx] · N/M · search: <q> (i/N)"  (whichever active)
//	idle           → "  "                                 (blank, line still reserved)
//
// Filter and search inputs are mutually exclusive (only one open at a time), so
// the two typing cases never both apply. Both typing cases render the input's
// .View() so bubbles scrolls a long query within the input (keeping the cursor +
// newest chars visible) rather than right-clipping them; the input's Width is set
// on the persisted model at the open/typing/resize sites (see logSearchInputWidth
// / logFilterInputWidth), mirroring searchBarLine's two mechanisms. The
// unconditional final clampToWidth is the hard guarantee of the one-physical-line
// invariant at any width — a long query, long service name, or dual summary is
// right-truncated rather than wrapped. clampToWidth is a no-op at m.width <= 0
// (unknown test size).
func (m Model) logBarLine() string {
	var line string
	switch {
	case m.logSearching:
		// Typing search — show the live query + mode + live (i/N) counter. Render
		// the input's .View() (not .Value()) so bubbles scrolls the value to keep
		// the cursor + newest chars visible; its Width is set on the PERSISTED
		// model in the '/' open + typing-intercept + resize paths (mirrors
		// searchBarLine — a value-receiver set here would be discarded).
		line = fmt.Sprintf("  / %s %s %s",
			m.logSearchInput.View(), logModeTag(m.logSearchIsRegex), m.logSearchCounter())
	case m.logFiltering:
		// Render .View() for the same reason as the search branch above.
		q := m.logFilterInput.View()
		if m.logFilterBadRegex() {
			line = fmt.Sprintf("  f %s %s (bad regex)", q, logModeTag(true))
		} else {
			survivors, total := m.logFilterCounts()
			line = fmt.Sprintf("  f %s %s · %d/%d shown",
				q, logModeTag(m.logFilterIsRegex), survivors, total)
		}
	case m.logFilterQuery != "" || m.logSearchQuery != "":
		// Committed summary — a compact recap of whichever is active. "[rx]" is
		// appended only in regex mode (omitted for the common literal case).
		var parts []string
		if m.logFilterQuery != "" {
			survivors, total := m.logFilterCounts()
			seg := "filter: " + m.logFilterQuery
			if m.logFilterCommittedRegex {
				seg += " [rx]"
			}
			seg += fmt.Sprintf(" · %d/%d", survivors, total)
			parts = append(parts, seg)
		}
		if m.logSearchQuery != "" {
			seg := "search: " + m.logSearchQuery
			if m.logSearchCommittedRegex {
				seg += " [rx]"
			}
			seg += " " + m.logSearchCounter()
			parts = append(parts, seg)
		}
		line = "  " + strings.Join(parts, " · ")
	default:
		line = "  "
	}
	return clampToWidth(line, m.width)
}

// clearLogSearch resets every search field to the no-search default. Called on
// entry to the log screen and on the esc-to-containers cleanup, and used by the
// typing-cancel path to discard an in-progress query.
func (m *Model) clearLogSearch() {
	m.logSearching = false
	m.logSearchQuery = ""
	m.logSearchIsRegex = false
	m.logSearchCommittedRegex = false
	m.logSearchMatches = nil
	m.logSearchCur = 0
	m.logSearchInput.SetValue("")
	m.logSearchInput.Blur()
}

func (m Model) readLogChunk() tea.Cmd {
	reader := m.logsPipeR
	session := m.logsSession
	return func() tea.Msg {
		buf := make([]byte, 4096)
		n, err := reader.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			return logChunkMsg{data: data, session: session}
		}
		if err != nil {
			if err == io.EOF {
				return logDoneMsg{err: nil, session: session}
			}
			return logDoneMsg{err: err, session: session}
		}
		return logDoneMsg{err: nil, session: session}
	}
}

func (m Model) loadProjects() tea.Cmd {
	loader := m.projectLoader
	ctx := m.ctx
	session := m.projectsSession
	return func() tea.Msg {
		if loader == nil {
			return projectsMsg{err: fmt.Errorf("no project loader configured"), session: session}
		}
		projects, err := loader(ctx)
		return projectsMsg{projects: projects, err: err, session: session}
	}
}

func (m Model) refreshStatus() tea.Cmd {
	ctx := m.ctx
	c := m.composer
	session := m.statusSession
	return func() tea.Msg {
		status, err := c.ContainerStatus(ctx)
		return statusMsg{status: status, err: err, session: session}
	}
}

func (m Model) refreshStats() tea.Cmd {
	ctx := m.ctx
	c := m.composer
	session := m.statsSession
	return func() tea.Msg {
		stats, err := c.ContainerStats(ctx)
		return statsMsg{stats: stats, err: err, session: session}
	}
}

// refreshUpdates fires CheckUpdates for the active composer with an empty
// services slice (= "all services"). The returned updatesMsg carries the
// current updatesSession so a stale response from a previous project/server
// context is dropped by the handler.
func (m Model) refreshUpdates() tea.Cmd {
	ctx := m.ctx
	c := m.composer
	session := m.updatesSession
	return func() tea.Msg {
		results, err := c.CheckUpdates(ctx, nil)
		return updatesMsg{results: results, err: err, session: session}
	}
}

// updatesCacheKey returns the cache key for the current context. The format
// is projDir + "|" + serverName, prefixed with "unmanaged|" for the synthetic
// unmanaged row. Empty serverName means local. For the local-fast-track entry
// (no project picker), projDir is empty too, so the key is just "|".
//
// The prefix is load-bearing: the unmanaged row has an empty ConfigDir, so a
// local unmanaged view would key "|" as well — a direct collision with the
// fast-track slot. A fresh entry from either context would then suppress the
// other's CheckUpdates for the full TTL, and hydrateUpdates would write one
// context's verdicts onto any colliding service name (the phantom guard drops
// only unknown names, not colliding ones).
//
// The prefix is derived from m.readOnly() rather than a parallel bool field:
// the composer IS the context the cache describes, it is assigned and nil'd at
// exactly the sites that would have had to clear such a field, and CLAUDE.md's
// back-navigation cleanup discipline is the repo's most fragile invariant —
// one forgotten site would silently mis-key the cache.
func (m Model) updatesCacheKey() string {
	key := m.projDir + "|" + m.serverName
	if m.readOnly() {
		return "unmanaged|" + key
	}
	return key
}

// hydrateUpdates writes the verdicts from results into m.svcStatus, mutating
// each entry's UpdateAvailable field. Services absent from results retain
// the tri-state nil (unknown). Called only when results is non-nil and the
// user is on screenSelectContainers.
//
// Verdicts for services absent from svcStatus are dropped silently;
// compose config and ContainerStatus may transiently disagree during a
// refresh (e.g. a service added to compose.yml between the two fetches).
// Synthesising a phantom zero-valued svcStatus entry would (a) make any
// future iterator over svcStatus see a fake service, and (b) leak memory
// across project switches if the verdict referenced a service that no
// longer exists in the active project.
func (m *Model) hydrateUpdates(results map[string]bool) {
	if m.svcStatus == nil {
		return
	}
	for svc, avail := range results {
		st, ok := m.svcStatus[svc]
		if !ok {
			continue
		}
		v := avail
		st.UpdateAvailable = &v
		m.svcStatus[svc] = st
	}
}

// updatesCacheLookup returns the cached entry for the current context and a
// freshness flag. Fresh means the entry exists and is still within its TTL.
// Failure entries (entry.err == true) use the shorter updatesErrorTTL window
// so a persistent fault doesn't drive a 5-second refetch loop via the
// statusMsg self-heal; success entries use the longer updatesCacheTTL. A
// fresh failure entry suppresses refetches but also fails to hydrate (its
// results map is nil), so cached failures show as blank glyphs alongside
// the updatesErr warning.
func (m Model) updatesCacheLookup() (updateEntry, bool) {
	if m.updateCache == nil {
		return updateEntry{}, false
	}
	entry, ok := m.updateCache[m.updatesCacheKey()]
	if !ok {
		return updateEntry{}, false
	}
	ttl := updatesCacheTTL
	if entry.err {
		ttl = updatesErrorTTL
	}
	fresh := time.Since(entry.fetchedAt) < ttl
	return entry, fresh
}

// maybeRefreshUpdatesCmd returns refreshUpdates() and marks updateInFlight
// when the cache is stale or missing; otherwise returns nil. The caller is
// expected to compose this into the screen-entry batch alongside refreshStatus
// / refreshStats / loadServices. Cache hits surface via the post-overwrite
// hydration in servicesMsg/statusMsg handlers — no synthetic msg needed.
//
// Cache hits on SUCCESS entries clear updatesErr (the stored success is the
// latest ground truth, so any stale warning from before is no longer valid).
// Cache hits on FAILURE entries leave updatesErr alone — the warning was
// itself produced by the cached failure, and we don't want to clear it just
// because we're skipping the refetch during the error TTL window.
//
// A cache miss (no entry, or stale entry past its TTL) with no in-flight
// fetch enqueues a fresh refresh. Failure entries use the much shorter
// updatesErrorTTL window (~30s) vs success's updatesCacheTTL (10m) — see
// updateEntry doc — so retries happen reasonably soon while the screen
// activity isn't pinned to a 5-second refetch loop.
//
// In-flight guard: when a previous refreshUpdates is still pending
// (updateInFlight == true), returns nil rather than stacking another fetch.
// The pending response's session check will drop it if it's stale (context
// changed), so we avoid burning extra registry calls — same rationale as
// refreshTickMsg's refreshInFlight gate. The U keypress already has its
// own equivalent guard; this is defense-in-depth for all call sites.
//
// Invariant for context-change callers: every site that bumps
// updatesSession (project pick, execDone, esc back from progress/logs,
// entryLocal fast-track) must reset m.updateInFlight = false BEFORE
// calling this helper. Skipping the reset would let a stale in-flight
// fetch from a previous context block the new context's fresh fetch
// (the stale response would still arrive and clear the flag, but the
// new context's fetch would have been dropped at the bump site —
// resulting in no glyphs until the next periodic tick).
func (m *Model) maybeRefreshUpdatesCmd() tea.Cmd {
	if m.updateInFlight {
		return nil
	}
	entry, fresh := m.updatesCacheLookup()
	if fresh {
		if entry.err {
			// Restore the warning text from the cached failure: navigating
			// away clears m.updatesErr, but the cached failure is still the
			// latest ground truth within the error TTL, so re-entry should
			// surface the same warning rather than showing blank glyphs
			// with no explanation.
			m.updatesErr = entry.errMsg
		} else {
			m.updatesErr = ""
		}
		return nil
	}
	if !m.autoUpdatesAllowed() {
		// Same reasoning as the statusMsg branch: the cached failure has
		// expired and U is the only thing that can refresh it, so the stale
		// warning must not survive into the new visit.
		m.updatesErr = ""
		return nil
	}
	m.updateInFlight = true
	return m.refreshUpdates()
}

// autoUpdatesAllowed reports whether the update check may fire on its own.
// It is false on a read-only composer, where U is the only trigger.
//
// A compose project bounds the check to one service list the user is actively
// managing. The unmanaged view has no such bound: it is derived from
// `docker ps -a`, so every leftover container on the host contributes a
// distinct image, and every image costs one local inspect plus one REGISTRY
// manifest request, run sequentially. Firing that on screen entry would spend
// dozens of round-trips per visit and can exhaust the anonymous Docker Hub
// manifest quota, which then breaks a real docker pull from the same host.
//
// The CLI reaches the same conclusion for the same reason — `list --updates`
// is opt-in — so the read-only screen matches it: the glyph column stays blank
// until the user asks for it, and the `?` overlay already advertises
// `U check updates`.
func (m Model) autoUpdatesAllowed() bool {
	return !m.readOnly()
}

// refreshTick returns a Cmd that fires a refreshTickMsg after
// statsRefreshInterval. The handler always reschedules another tick so this
// runs forever as a singleton — there is never more than one pending tick
// because every fired tick is replaced by exactly one new one.
//
// In tests, callers can set m.tickCmdOverride to substitute a non-blocking Cmd
// (avoids leaving a real 5s tea.Tick goroutine running for each test). Production
// code never sets this field — it always falls through to tea.Tick below.
func (m Model) refreshTick() tea.Cmd {
	if m.tickCmdOverride != nil {
		return m.tickCmdOverride()
	}
	return tea.Tick(statsRefreshInterval, func(time.Time) tea.Msg {
		return refreshTickMsg{}
	})
}

func (m Model) loadServices() tea.Cmd {
	ctx := m.ctx
	c := m.composer
	session := m.statusSession
	return func() tea.Msg {
		services, err := c.ListServices(ctx)
		if err != nil {
			return servicesMsg{err: err, session: session}
		}
		status, err := c.ContainerStatus(ctx)
		if err != nil {
			return servicesMsg{err: err, session: session}
		}
		return servicesMsg{services: services, status: status, session: session}
	}
}

func (m Model) allSelected() bool {
	if len(m.services) == 0 {
		return false
	}
	for i := range m.services {
		if !m.selected[i] {
			return false
		}
	}
	return true
}

func (m Model) selectedContainers() []string {
	var result []string
	for i, svc := range m.services {
		if m.selected[i] {
			result = append(result, svc)
		}
	}
	return result
}

func (m Model) selectedCount() int {
	count := 0
	for i := range m.services {
		if m.selected[i] {
			count++
		}
	}
	return count
}

// clearSearch resets all container-search state back to its zero value. Called
// on every transition that leaves screenSelectContainers or reloads m.services,
// so a committed search can never carry stale indices into m.services.
func (m *Model) clearSearch() {
	m.searching = false
	m.searchQuery = ""
	m.searchMatches = nil
	m.searchReturn = 0
	m.searchInput.SetValue("")
	m.searchInput.Blur()
}

// cycleMatch moves svcCursor to the next (forward) or previous (!forward) match
// in m.searchMatches, wrapping around at the boundaries. It is a no-op when there
// is no committed search (searchQuery == "" or no matches). searchMatches is kept
// in ascending index order by computeMatches, which this relies on.
//
// Two cases:
//   - on-match: the cursor currently sits on a match — step ±1 through the match
//     list with wrap-around (last→first for forward, first→last for backward).
//   - off-match: the cursor sits between matches (after a manual j/k) — forward
//     jumps to the first match STRICTLY AFTER the cursor (wrapping to the first
//     match when the cursor is past all matches), backward to the first match
//     STRICTLY BEFORE (wrapping to the last match when the cursor is before all).
func (m *Model) cycleMatch(forward bool) {
	if m.searchQuery == "" || len(m.searchMatches) == 0 {
		return
	}

	// Locate the cursor within the (ascending) match list.
	pos := -1
	for i, idx := range m.searchMatches {
		if idx == m.svcCursor {
			pos = i
			break
		}
	}

	n := len(m.searchMatches)
	if pos >= 0 {
		// On a match: step ±1 with wrap-around.
		if forward {
			pos = (pos + 1) % n
		} else {
			pos = (pos - 1 + n) % n
		}
		m.svcCursor = m.searchMatches[pos]
		m.fixSvcOffset()
		return
	}

	// Off-match: find the neighbouring match relative to the cursor.
	if forward {
		// First match strictly after the cursor; wrap to the first match.
		for _, idx := range m.searchMatches {
			if idx > m.svcCursor {
				m.svcCursor = idx
				m.fixSvcOffset()
				return
			}
		}
		m.svcCursor = m.searchMatches[0]
	} else {
		// First match strictly before the cursor; wrap to the last match.
		for i := n - 1; i >= 0; i-- {
			if m.searchMatches[i] < m.svcCursor {
				m.svcCursor = m.searchMatches[i]
				m.fixSvcOffset()
				return
			}
		}
		m.svcCursor = m.searchMatches[n-1]
	}
	m.fixSvcOffset()
}

// searchCounter renders the "(i/N)" match position for the search bar. When the
// cursor sits on a match, i is its 1-based position within searchMatches; when
// the cursor is off all matches (after a manual j/k), it shows "(N matches)"
// (or the singular "(1 match)" when exactly one match exists) instead of a bogus
// index. Returns "(no match)" when the query matched nothing.
func (m Model) searchCounter() string {
	n := len(m.searchMatches)
	if n == 0 {
		return "(no match)"
	}
	for pos, idx := range m.searchMatches {
		if idx == m.svcCursor {
			return fmt.Sprintf("(%d/%d)", pos+1, n)
		}
	}
	if n == 1 {
		return "(1 match)"
	}
	return fmt.Sprintf("(%d matches)", n)
}

// computeMatches returns the indices into services whose names contain query as
// a case-insensitive substring, preserving list order. An empty query returns
// nil so callers can treat "no query" and "no matches" distinctly. Pure — used
// by the container-screen search (search & jump).
func computeMatches(services []string, query string) []int {
	if query == "" {
		return nil
	}
	q := strings.ToLower(query)
	var matches []int
	for i, svc := range services {
		if strings.Contains(strings.ToLower(svc), q) {
			matches = append(matches, i)
		}
	}
	return matches
}

// hasStatusColumns returns true if any service in m.services has non-empty Created, Uptime,
// Ports, or stats data, OR if stats have been requested for the current container-screen
// entry. The statsRequested short-circuit ensures CPU/Mem column captions render from the
// first frame instead of popping in ~1.5s later when the host-wide docker stats call returns.
// The stats branch must match the render predicate in viewSelectContainers
// (map-presence + running) so svcVisibleCount and the captions row stay in sync.
func (m Model) hasStatusColumns() bool {
	if m.statsRequested {
		return true
	}
	for _, svc := range m.services {
		if st, ok := m.svcStatus[svc]; ok {
			if st.Created != "" || st.Uptime != "" || len(st.Ports) > 0 {
				return true
			}
			// An available update on its own should not change the header-line
			// count (it's rendered inline next to the name, not as a separate
			// column). The Service captions row is governed by the other
			// columns; if none of those are present, no captions row is shown.
		}
		if _, ok := m.stats[svc]; ok {
			if m.svcStatus[svc].Running {
				return true
			}
		}
	}
	return false
}

// svcVisibleCount returns the number of services that fit in the terminal.
// Header: breadcrumb + titleStyle MarginBottom blank + gap/indicator = 3 lines,
// plus 1 more when column captions are shown.
// Footer: 3 lines with a one-line help text, 4 when it wraps to two — the same
// count in every search state and while confirming (containerFooter decides it).
// Warning adds 1 extra line; an active stats/updates soft-warning adds 1 more.
// When m.height is 0 (no WindowSizeMsg received), returns len(m.services) for backward compat.
func (m Model) svcVisibleCount() int {
	if m.height == 0 {
		return len(m.services)
	}

	// headerLines: breadcrumb (1) + titleStyle MarginBottom space-line (1) + gap/indicator (1) = 3
	// +1 when column captions (Created/Uptime) are displayed
	headerLines := 3
	if m.hasStatusColumns() {
		headerLines++
	}

	// Footer layout (both branches): gap-or-indicator (1) + helpStyle MarginTop
	// space-line (1) + footer text (1) = 3. The reserved search-bar line does
	// NOT add a physical row: it is written as `gap + searchBarLine()` with no
	// trailing newline immediately before `helpStyle.Render(...)`, and
	// helpStyle's MarginTop(1) prepends a full-width blank — the bar content and
	// that blank land on the SAME physical line (empirically verified against a
	// windowed 30-service render; the footer occupies 3 rows: indicator, the
	// merged bar+margin line, and the help text). Counting the bar separately
	// over-reserved one row versus the pre-search baseline and left a blank line
	// of slack at the bottom of the terminal. The list height stays constant
	// across search-idle / searching / committed / confirming for two reasons:
	// the merged bar+margin line is present in every state (blank while
	// confirming, with the confirm prompt taking the footer-text slot), and the
	// footer-text slot itself is always containerFooterLines() — which is
	// derived from the idle footer alone and padded out in the other states.
	var footerLines int
	if m.confirming {
		// gap-or-indicator (1) + helpStyle MarginTop space-line (1) + the confirm
		// prompt, padded by the renderer to the same line count the help footer
		// would occupy at this width. Reading that count from
		// containerFooterLines() keeps the confirm state the same height as the
		// idle one: hard-coding 3 here shows one row MORE than idle at every
		// width where the footer wraps.
		footerLines = 2 + m.containerFooterLines()
	} else {
		// gap-or-indicator (1) + helpStyle MarginTop space-line (1) + the help
		// text itself (1 when it fits on one line, 2 when it wraps). The count
		// comes from containerFooterLines(), which containerFooter() also pads
		// every state out to, so the reservation always matches what is drawn.
		footerLines = 2 + m.containerFooterLines()
		if m.warning != "" {
			footerLines++ // warning line
		}
		// Soft-warning slot priority: svcErr > statsErr > updatesErr. svcErr
		// early-returns from the renderer so it doesn't appear here; statsErr
		// and updatesErr are mutually exclusive in the renderer too, so the
		// footer reserves at most one line for the active warning.
		if m.statsErr != nil || m.updatesErr != "" {
			footerLines++ // soft-fail warning line
		}
	}

	visible := m.height - headerLines - footerLines
	if visible < 1 {
		visible = 1
	}
	if visible > len(m.services) {
		visible = len(m.services)
	}
	return visible
}

// fixSvcOffset adjusts svcOffset so that svcCursor is within the visible window.
// fixServerCursor clamps serverCursor to a valid selectable entry after
// serverEntries has been rebuilt (e.g. after settings add/edit/delete).
func (m *Model) fixServerCursor() {
	if len(m.serverEntries) == 0 {
		m.serverCursor = 0
		return
	}
	if m.serverCursor >= len(m.serverEntries) {
		m.serverCursor = len(m.serverEntries) - 1
	}
	// If cursor landed on a group header, move to nearest selectable
	if m.serverEntries[m.serverCursor].kind == entryGroupHeader {
		// Try forward first
		next := nextSelectable(m.serverEntries, m.serverCursor)
		if next != m.serverCursor {
			m.serverCursor = next
		} else {
			m.serverCursor = prevSelectable(m.serverEntries, m.serverCursor)
		}
	}
}

func (m *Model) fixSvcOffset() {
	visible := m.svcVisibleCount()

	// Cursor moved below visible window
	if m.svcCursor >= m.svcOffset+visible {
		m.svcOffset = m.svcCursor - visible + 1
	}
	// Cursor moved above visible window
	if m.svcCursor < m.svcOffset {
		m.svcOffset = m.svcCursor
	}
	// Clamp offset
	maxOffset := len(m.services) - visible
	if maxOffset < 0 {
		maxOffset = 0
	}
	if m.svcOffset > maxOffset {
		m.svcOffset = maxOffset
	}
}

func sortServices(services []string) []string {
	sorted := slices.Clone(services)
	slices.SortFunc(sorted, func(a, b string) int {
		if cmp := strings.Compare(strings.ToLower(a), strings.ToLower(b)); cmp != 0 {
			return cmp
		}
		return strings.Compare(a, b)
	})
	return sorted
}

func initSettingsInputs() [4]textinput.Model {
	var inputs [4]textinput.Model
	placeholders := [4]string{"server-name", "user@hostname", "/path/to/project", "group-name"}
	limits := [4]int{64, 128, 256, 64}
	for i := range inputs {
		inputs[i] = textinput.New()
		inputs[i].Placeholder = placeholders[i]
		inputs[i].CharLimit = limits[i]
		inputs[i].Width = 40
	}
	return inputs
}

// resolveServerColor returns the effective color for a server: group color
// if the server belongs to a group and m.config is available, otherwise the
// server's own Color field.
func (m Model) resolveServerColor(server config.Server) string {
	if server.Group != "" && m.config != nil {
		return m.config.GroupColor(server.Group)
	}
	return server.Color
}

// cleanOrphanedGroups returns groups that are still referenced by at least one server.
func cleanOrphanedGroups(groups []config.Group, servers []config.Server) []config.Group {
	used := make(map[string]bool)
	for _, s := range servers {
		if s.Group != "" {
			used[s.Group] = true
		}
	}
	var result []config.Group
	for _, g := range groups {
		if used[g.Name] {
			result = append(result, g)
		}
	}
	return result
}

// cycleColor moves through the color options: "" → ValidColors[0..N-1] → "" → ...
func cycleColor(current string, dir int) string {
	all := append([]string{""}, config.ValidColors...)
	idx := 0
	for i, c := range all {
		if c == current {
			idx = i
			break
		}
	}
	idx = (idx + dir + len(all)) % len(all)
	return all[idx]
}

func (m Model) View() string {
	if m.quitting {
		return m.viewQuitConfirm()
	}
	if m.helpOpen {
		return m.viewHelp()
	}

	switch m.screen {
	case screenSelectServer:
		return m.viewSelectServer()
	case screenSelectProject:
		return m.viewSelectProject()
	case screenSelectContainers:
		return m.viewSelectContainers()
	case screenProgress:
		return m.viewProgress()
	case screenLogs:
		return m.viewLogs()
	case screenConfig:
		return m.viewConfig()
	case screenInspect:
		return m.viewInspect()
	case screenSettingsList:
		return m.viewSettingsList()
	case screenSettingsForm:
		return m.viewSettingsForm()
	}
	return ""
}

func (m Model) viewQuitConfirm() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("cdeploy"))
	b.WriteString("\n\n")
	b.WriteString(warningStyle.Render(fmt.Sprintf("  Disconnect from %s? (y/n)", m.serverName)))
	b.WriteString("\n")
	return b.String()
}

func (m Model) viewSelectServer() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("cdeploy > select server"))
	b.WriteString("\n\n")

	if m.serverErr != nil {
		b.WriteString(stepFailed.Render(fmt.Sprintf("  Connection failed: %v\n", m.serverErr)))
		b.WriteString("\n")
	}

	// Compute max name width for column alignment
	maxNameLen := len("Local")
	for _, s := range m.servers {
		if len(s.Name) > maxNameLen {
			maxNameLen = len(s.Name)
		}
	}

	for i, entry := range m.serverEntries {
		switch entry.kind {
		case entryGroupHeader:
			b.WriteString("\n")
			b.WriteString(groupHeaderStyle.Render(entry.group))
			b.WriteString("\n")

		case entryLocal:
			cursor := "  "
			style := itemStyle
			if i == m.serverCursor {
				cursor = "> "
				style = selectedItemStyle
			}
			name := fmt.Sprintf("%-*s", maxNameLen, "Local")
			b.WriteString(style.Render(cursor + name))
			b.WriteString("   ")
			b.WriteString(descStyle.Render("(this machine)"))
			b.WriteString("\n")

		case entryServer:
			cursor := "  "
			style := itemStyle
			if i == m.serverCursor {
				cursor = "> "
				style = selectedItemStyle
			}
			srv := m.servers[entry.serverIdx]
			name := fmt.Sprintf("%-*s", maxNameLen, srv.Name)
			b.WriteString(style.Render(cursor + name))
			b.WriteString("   ")
			b.WriteString(descStyle.Render(srv.Host))
			b.WriteString("\n")
		}
	}

	help := "  up/down navigate  •  enter select  •  q quit"
	if m.config != nil {
		help = "  up/down navigate  •  enter select  •  s settings  •  q quit"
	}
	b.WriteString(helpStyle.Render("\n" + help))
	return b.String()
}

func shortenPath(dir string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(dir, home) {
		return "~" + dir[len(home):]
	}
	return dir
}

func (m Model) breadcrumb() string {
	parts := []string{"cdeploy"}
	if m.serverName != "" {
		parts = append(parts, m.serverBadge())
	}
	if m.projName != "" {
		parts = append(parts, m.projName)
	}
	return strings.Join(parts, " > ")
}

func (m Model) serverBadge() string {
	color := m.serverColor
	if color == "" {
		return m.serverName
	}
	style := serverBadgeStyle(color)
	return style.Render(" " + m.serverName + " ")
}

func (m Model) viewSelectProject() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.breadcrumb() + " > select project"))
	b.WriteString("\n\n")

	if m.projects == nil && m.projErr == nil {
		b.WriteString("  Loading projects...\n")
		return b.String()
	}

	if m.projErr != nil {
		b.WriteString(stepFailed.Render(fmt.Sprintf("  Error: %v\n", m.projErr)))
		help := "  q quit"
		if len(m.servers) > 0 || m.config != nil {
			help = "  q back"
		}
		b.WriteString(helpStyle.Render("\n" + help))
		return b.String()
	}

	if len(m.projects) == 0 {
		b.WriteString("  No Docker Compose projects found\n")
		help := "  q quit"
		if len(m.servers) > 0 || m.config != nil {
			help = "  q back"
		}
		b.WriteString(helpStyle.Render("\n" + help))
		return b.String()
	}

	maxNameLen := 0
	for _, proj := range m.projects {
		if len(proj.Name) > maxNameLen {
			maxNameLen = len(proj.Name)
		}
	}

	for i, proj := range m.projects {
		cursor := "  "
		style := itemStyle
		if i == m.projCursor {
			cursor = "> "
			style = selectedItemStyle
		}
		name := fmt.Sprintf("%-*s", maxNameLen, proj.Name)
		b.WriteString(style.Render(cursor + name))
		b.WriteString("   ")
		desc := shortenPath(proj.ConfigDir)
		if proj.Desc != "" {
			desc = proj.Desc
		}
		b.WriteString(descStyle.Render(desc))
		b.WriteString("\n")
	}

	helpText := "\n  up/down navigate  •  enter select  •  q quit"
	if len(m.servers) > 0 || m.config != nil {
		helpText = "\n  up/down navigate  •  enter select  •  q back"
	}
	b.WriteString(helpStyle.Render(helpText))
	return b.String()
}

// healthIndicator returns a fixed-width health icon for the TUI container list.
func healthIndicator(health string) string {
	switch health {
	case "healthy":
		return healthHealthy.Render("♥")
	case "unhealthy":
		return healthUnhealthy.Render("✗")
	case "starting":
		return healthStarting.Render("~")
	default:
		return " "
	}
}

// canGoBack reports whether esc — and the q that rewrites to it — has a parent
// screen to navigate to. It is false on the root screens a standalone run can
// land on, where q QUITS and esc does nothing. The container footer's back
// label and the `?` overlay's LEAVE group both read this one predicate, so
// they cannot contradict each other on the same screen.
func (m Model) canGoBack() bool {
	switch m.screen {
	case screenSelectServer:
		return false
	case screenSelectProject:
		return len(m.servers) > 0 || m.config != nil
	case screenSelectContainers:
		return m.showPicker
	}
	return true
}

// readOnly reports whether the active composer refuses every write. It gates
// the write keys, the selection widgets, the footer and the `?` overlay, so
// nothing advertises a no-op. Nil-safe: a Model{} test literal has no composer
// and is not read-only, matching the compose path.
func (m Model) readOnly() bool {
	if m.composer == nil {
		return false
	}
	ro, ok := m.composer.(ReadOnlyComposer)
	return ok && ro.ReadOnlyComposer()
}

// containerHelpLines returns the IDLE container footer, the pair every other
// footer state is measured against.
//
// A read-only composer gets a different pair: `space toggle` leaves line1
// because multi-select is gated, and line2 swaps the three write-path tokens
// for the two inspection keys that still work. containerFooterLines and
// containerFooter both read this one helper, so the height math and the render
// follow the variant for free.
func (m Model) containerHelpLines() (line1, line2 string) {
	back := "q quit"
	if m.canGoBack() {
		back = "q back"
	}
	if m.readOnly() {
		return fmt.Sprintf("  %s  •  ? keys", back), "  l logs  •  x exec"
	}
	return fmt.Sprintf("  space toggle  •  %s  •  ? keys", back),
		"  d deploy  •  r restart  •  l logs"
}

// containerFooterLines reports how many physical lines the container footer
// occupies. It is decided from the IDLE footer alone, before any search-state
// substitution, and containerFooter pads the other states out to it:
// svcVisibleCount reserves rows from this count, so a footer that shrank while
// the search bar was open would grow the service list by one row and re-flow it
// under the cursor. The count is therefore state-INDEPENDENT by design; do not
// make it read the substituted footer.
//
// On the write path idle is also the widest variant, so the one-line branch it
// picks always holds the substitutions too. The READ-ONLY pair inverts that:
// its line2 (`l logs • x exec`, 19 cells) is SHORTER than the committed-search
// substitution (`n/N cycle • esc clear`, 25), so the one-line branch chosen at
// widths 43-46 does not hold. containerFooter clamps its rendered string for
// that band rather than widening the count here, which would re-flow the list.
//
// line2[2:] strips its leading indent so the two halves join with a single
// separator. ansi.StringWidth, not len: each • is 3 bytes but one display cell,
// so len() over-counts by 2 per separator and splits a footer that fits.
func (m Model) containerFooterLines() int {
	line1, line2 := m.containerHelpLines()
	if m.width >= ansi.StringWidth(line1+"  •  "+line2[2:])+2 {
		return 1
	}
	return 2
}

// containerFooter renders the container screen's help footer: six tokens —
// `space toggle`, the back key and `? keys` on line1, `d deploy`, `r restart`
// and `l logs` on line2, or the read-only pair containerHelpLines returns for a
// composer that refuses every write. Every other binding on the screen lives
// only in the `?` overlay (help.go).
//
// line1 carries what must stay visible while line2 is replaced: a COMMITTED
// search swaps line2 wholesale, and q and `?` still work in that state. While
// the search input is OPEN neither key works, so the whole footer becomes the
// two that do.
//
// Every rendered line is clamped to m.width, the same never-wrap guarantee
// searchBarLine and logBarLine give. containerFooterLines picks one line or two
// from the IDLE pair alone (it must stay state-independent, or the list
// re-flows), so a substitution WIDER than the idle line2 can overrun a width
// the idle pair fit — the read-only pair does exactly that at widths 43-46.
func (m Model) containerFooter() string {
	line1, line2 := m.containerHelpLines()
	switch {
	case m.searching:
		// The typing intercept binds only enter, esc and ctrl+c; every other
		// key, `space`, q and `?` included, lands in the query as a literal rune.
		line1, line2 = "  enter jump  •  esc cancel", ""
	case m.searchQuery != "":
		line2 = "  n/N cycle  •  esc clear"
	}

	if m.containerFooterLines() == 1 {
		if line2 == "" {
			return helpStyle.Render(clampToWidth(line1, m.width))
		}
		return helpStyle.Render(clampToWidth(line1+"  •  "+line2[2:], m.width))
	}
	// A trailing newline pads the searching footer (line2 == "") out to the two
	// reserved rows; lipgloss renders the empty half as a full-width blank line.
	return helpStyle.Render(clampToWidth(line1, m.width) + "\n" + clampToWidth(line2, m.width))
}

func (m Model) viewSelectContainers() string {
	var b strings.Builder
	readOnly := m.readOnly()
	title := m.breadcrumb() + " > services"
	if !readOnly {
		title = fmt.Sprintf("%s (%d/%d selected)", title, m.selectedCount(), len(m.services))
	}
	b.WriteString(titleStyle.Render(title))

	if m.services == nil && m.svcErr == nil {
		b.WriteString("\n\n")
		b.WriteString("  Loading services...\n")
		return b.String()
	}

	if m.svcErr != nil {
		b.WriteString("\n\n")
		b.WriteString(stepFailed.Render(fmt.Sprintf("  Error: %v\n", m.svcErr)))
		if m.showPicker {
			b.WriteString(helpStyle.Render("\n  q back"))
		} else {
			b.WriteString(helpStyle.Render("\n  q quit"))
		}
		return b.String()
	}

	// Windowed rendering
	visible := m.svcVisibleCount()
	start := m.svcOffset
	end := start + visible
	if end > len(m.services) {
		end = len(m.services)
	}

	// Calculate max widths for alignment (across ALL services, not just visible).
	// portsStr caches FormatPorts(...) per service so the render loop below can
	// reuse the formatted strings without re-calling FormatPorts (mirrors the
	// pattern in cmd/list.go formatDots/formatDotsGrouped). cpuStr and memStr
	// cache the formatted CPU/Mem cells per service for the same reason.
	maxName := 0
	maxCreated := 0
	maxUptime := 0
	maxCPU := 0
	maxMem := 0
	maxPorts := 0
	portsStr := make(map[string]string, len(m.services))
	cpuStr := make(map[string]string, len(m.services))
	memStr := make(map[string]string, len(m.services))
	for _, svc := range m.services {
		if len(svc) > maxName {
			maxName = len(svc)
		}
		if st, ok := m.svcStatus[svc]; ok {
			if len(st.Created) > maxCreated {
				maxCreated = len(st.Created)
			}
			if len(st.Uptime) > maxUptime {
				maxUptime = len(st.Uptime)
			}
			s := compose.FormatPorts(st.Ports)
			portsStr[svc] = s
			if w := utf8.RuneCountInString(s); w > maxPorts {
				maxPorts = w
			}
		}
		if stx, ok := m.stats[svc]; ok {
			// Only running services get stats cells; stopped containers leave blanks.
			running := m.svcStatus[svc].Running
			if running {
				cpu := fmt.Sprintf("%.1f%%", stx.CPUPercent)
				mem := fmt.Sprintf("%s/%s", compose.FormatBytes(stx.MemoryUsed), compose.FormatBytes(stx.MemoryLimit))
				cpuStr[svc] = cpu
				memStr[svc] = mem
				if w := utf8.RuneCountInString(cpu); w > maxCPU {
					maxCPU = w
				}
				if w := utf8.RuneCountInString(mem); w > maxMem {
					maxMem = w
				}
			}
		}
	}

	// Always reserve 2 trailing cells in the name column for the inline update
	// glyph (leading space + U+21E7). Reserving unconditionally — not gated on
	// "any service currently has the flag" — keeps following columns from
	// shifting when a verdict arrives or clears mid-poll (cache refresh, U
	// force-refresh, error invalidating glyphs). Services without updates pad
	// with plain spaces; services with updates render `name + " " + glyph`.
	maxName += 2

	// Reserve fixed minimum widths for CPU/Mem columns as soon as stats have
	// been requested. Two goals: (a) captions render on the first frame
	// instead of popping in when the ~1.5s docker stats call returns; (b) the
	// columns don't wiggle on every 5s refresh as values fluctuate (e.g. one
	// service briefly hitting 11% CPU shouldn't push every other column 1
	// char to the right). The minimums are sized for the realistic worst case:
	// 6 chars for CPU ("999.9%" covers a single core at 100% or scaled
	// services aggregating across replicas), 11 chars for Mem ("1024M/1024M"
	// covers values that haven't quite rolled over to the next unit).
	const (
		cpuColMin = 6  // len("999.9%")
		memColMin = 11 // len("1024M/1024M")
	)
	if m.statsRequested {
		if maxCPU < cpuColMin {
			maxCPU = cpuColMin
		}
		if maxMem < memColMin {
			maxMem = memColMin
		}
	}

	// Top gap: show scroll-up indicator or blank line
	if start > 0 {
		b.WriteString("\n")
		b.WriteString(descStyle.Render(fmt.Sprintf("  ▲ %d more", start)))
		b.WriteString("\n")
	} else {
		b.WriteString("\n\n")
	}

	// Column captions row (only when status data exists). Widen each active
	// column to at least its caption width so the caption never overflows and
	// shifts the following columns rightward.
	if maxCreated > 0 || maxUptime > 0 || maxCPU > 0 || maxMem > 0 || maxPorts > 0 {
		if len("Service") > maxName {
			// Ensures the "Service" caption fits when every service name is
			// shorter than the caption — also widens the data rows in lockstep
			// (the same maxName is used in the row builder below).
			maxName = len("Service")
		}
		if maxCreated > 0 && len("Created") > maxCreated {
			maxCreated = len("Created")
		}
		if maxUptime > 0 && len("Uptime") > maxUptime {
			maxUptime = len("Uptime")
		}
		if maxCPU > 0 && len("CPU") > maxCPU {
			maxCPU = len("CPU")
		}
		if maxMem > 0 && len("Mem") > maxMem {
			maxMem = len("Mem")
		}
		if maxPorts > 0 && len("Ports") > maxPorts {
			maxPorts = len("Ports")
		}
		// Left padding: cursor(2) + checkbox(3) + space(1) + health(1) + space(1) + dot(1) + space(1) = 10
		// Read-only drops the checkbox but keeps the space that follows it: 10 - 3 = 7.
		// Then the "Service" caption sits in the same column as service names.
		namePad := 10
		if readOnly {
			namePad = 7
		}
		header := strings.Repeat(" ", namePad) + fmt.Sprintf("%-*s", maxName, "Service")
		if maxCreated > 0 {
			header += fmt.Sprintf("  %-*s", maxCreated, "Created")
		}
		if maxUptime > 0 {
			header += fmt.Sprintf("  %-*s", maxUptime, "Uptime")
		}
		if maxCPU > 0 {
			// Right-aligned: percent sign anchors at the right edge so the
			// caption visually lines up with the column's rightmost digits.
			header += fmt.Sprintf("  %*s", maxCPU, "CPU")
		}
		if maxMem > 0 {
			header += fmt.Sprintf("  %-*s", maxMem, "Mem")
		}
		if maxPorts > 0 {
			header += fmt.Sprintf("  %-*s", maxPorts, "Ports")
		}
		b.WriteString(descStyle.Render(header))
		b.WriteByte('\n')
	}

	for i := start; i < end; i++ {
		svc := m.services[i]
		cursor := "  "
		if i == m.svcCursor {
			cursor = "> "
		}

		// Read-only: no checkbox, and the space that follows it in the line format
		// below keeps the 7-cell caption pad in lockstep.
		checkbox := ""
		if !readOnly {
			checkbox = checkboxOff.Render("[ ]")
			if m.selected[i] {
				checkbox = checkboxOn.Render("[x]")
			}
		}

		st := m.svcStatus[svc]
		health := healthIndicator(st.Health)

		dot := statusStoppedDot.Render("●")
		if st.Running {
			dot = statusRunningDot.Render("●")
		}

		// Build line: cursor + checkbox + health + dot + name [+ created] [+ uptime] [+ cpu] [+ mem] [+ ports]
		// nameCell is padded manually rather than via %-*s so styled glyphs
		// (whose ANSI escapes don't count as display width) line up correctly.
		// utf8.RuneCountInString approximates display width for the glyph
		// because it occupies one terminal cell (U+21E7) — matches the +2
		// reservation logic above.
		nameWidth := utf8.RuneCountInString(svc)
		// Highlight matching rows during an active search. Only the RAW name is
		// wrapped in the style so the ANSI escapes don't disturb the width math
		// below (same rationale as the update glyph): the cursor's current match
		// gets the brighter bold style, other matches the plain match style.
		renderedName := svc
		if m.searchQuery != "" && slices.Contains(m.searchMatches, i) {
			if i == m.svcCursor {
				renderedName = searchCurrentStyle.Render(svc)
			} else {
				renderedName = searchMatchStyle.Render(svc)
			}
		}
		nameCell := renderedName
		if st.UpdateAvailable != nil && *st.UpdateAvailable {
			nameCell = renderedName + " " + updateGlyphStyle.Render(compose.UpdateGlyph)
			nameWidth += 2 // space + glyph cell
		}
		if pad := maxName - nameWidth; pad > 0 {
			nameCell += strings.Repeat(" ", pad)
		}
		line := fmt.Sprintf("%s%s %s %s %s", cursor, checkbox, health, dot, nameCell)
		if maxCreated > 0 {
			line += fmt.Sprintf("  %-*s", maxCreated, st.Created)
		}
		if maxUptime > 0 {
			line += fmt.Sprintf("  %-*s", maxUptime, st.Uptime)
		}
		if maxCPU > 0 {
			// Right-aligned: percent signs stack vertically across rows so the
			// magnitude of each value is readable at a glance. Mem stays
			// left-aligned because it's a composite "used/limit" string and
			// right-aligning the limit looks worse than left-aligning the used.
			line += fmt.Sprintf("  %*s", maxCPU, cpuStr[svc])
		}
		if maxMem > 0 {
			line += fmt.Sprintf("  %-*s", maxMem, memStr[svc])
		}
		if maxPorts > 0 {
			line += fmt.Sprintf("  %-*s", maxPorts, portsStr[svc])
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}

	// Bottom: scroll-down indicator replaces the blank-line gap before the
	// help/confirm bar so the total line count stays constant.
	below := len(m.services) - end
	if below > 0 {
		b.WriteString(descStyle.Render(fmt.Sprintf("  ▼ %d more", below)))
		b.WriteString("\n")
	}

	// The gap slot between the list/indicator and the reserved bar line. When a
	// "▼ N more" indicator was written it already occupies that slot, so skip the
	// leading newline; otherwise emit a blank line (matching the pre-search gap).
	gap := "\n"
	if below > 0 {
		gap = ""
	}

	// Reserved search-bar line — ALWAYS rendered (in both the confirming and
	// non-confirming branches) so the list height never jumps between
	// search-idle / searching / committed / confirming. The footer text below is
	// held to a state-independent line count for the same reason (see
	// containerFooter). While confirming, the bar is suppressed (blank) because
	// the confirm prompt takes precedence and is shown in the footer-text slot
	// below; otherwise the bar shows the search input (typing), the compact
	// committed summary, or a blank placeholder.
	if m.confirming {
		b.WriteString(gap + "  ")
	} else {
		b.WriteString(gap + m.searchBarLine())
	}

	// Confirming: the confirm prompt takes the footer-text slot (no help keys).
	// The reserved bar line above is blank, and the prompt below is padded to the
	// help footer's line count, so the total (gap + bar + helpStyle MarginTop
	// blank + prompt) matches the non-confirming footer exactly at every width.
	if m.confirming {
		var prompt string
		switch {
		case m.pendingExec:
			prompt = fmt.Sprintf("  Exec into %s?  enter confirm  •  esc cancel",
				m.services[m.svcCursor])
		case m.pendingOp == runner.Rollback:
			// Show the captured target set (what will actually roll back), not the
			// live selection which the async fetch may have let drift.
			containers := m.rollbackTargets
			prompt = fmt.Sprintf("  Rollback %s%s?  enter confirm  •  esc cancel",
				strings.Join(containers, ", "), m.rollbackAgeSuffix(containers))
		default:
			prompt = fmt.Sprintf("  %s %s?  enter confirm  •  esc cancel",
				m.pendingOp.String(), strings.Join(m.selectedContainers(), ", "))
		}
		// Pad the prompt to however many lines the help footer occupies at this
		// width (containerFooterLines is the single source for that count), so
		// the list does not gain a row the moment the prompt appears.
		if m.containerFooterLines() == 2 {
			prompt += "\n"
		}
		b.WriteString(helpStyle.Render(prompt))
		return b.String()
	}

	if m.warning != "" {
		b.WriteString("\n  " + warningStyle.Render(m.warning))
	}
	// Soft-warning slot priority: svcErr > statsErr > updatesErr.
	// svcErr early-returns from the renderer above, so when both are set
	// svcErr always wins. statsErr and updatesErr are mutually exclusive
	// here — at most one is shown so the line count stays predictable
	// (svcVisibleCount accounts for at most one extra footer line).
	switch {
	case m.statsErr != nil:
		b.WriteString("\n  " + warningStyle.Render(fmt.Sprintf("Stats unavailable: %v", m.statsErr)))
	case m.updatesErr != "":
		b.WriteString("\n  " + warningStyle.Render(fmt.Sprintf("updates: %s", m.updatesErr)))
	}

	// helpStyle's MarginTop supplies the single blank line between the reserved
	// bar (or warning) line and the help text.
	b.WriteString(m.containerFooter())
	return b.String()
}

// searchBarLine renders the reserved footer bar for container search. It ALWAYS
// occupies exactly ONE physical line regardless of width or content — the
// constant-height invariant that svcVisibleCount() relies on (it reserves a
// single row for the bar; a wrapped bar would push the footer past its reserved
// space and let the list overflow the terminal / the cursor scroll off).
//
// Typing mode shows the open input + counter; committed mode shows a compact
// "↳ <name>  (i/N)" + hint when the cursor sits on a match, or
// `search "<query>"  (N matches)` when the cursor has moved off all matches
// (via j/k) so the non-matching row isn't labelled under the ↳ glyph; idle
// renders whitespace so the line is present but empty. Every non-idle mode
// assembles its line then applies ONE final clampToWidth(line, m.width) — the
// unconditional hard guarantee — so a long query, a long service name, the
// committed-mode hint, OR (at absurdly narrow widths) even the counter suffix
// can never push the bar past m.width and wrap onto a second physical line.
func (m Model) searchBarLine() string {
	var line string
	switch {
	case m.searching:
		// Budget the left (prefix + input) segment so the trailing counter stays
		// visible in the common case — reserve exactly the counter's width and
		// truncate the left part to the rest. bubbles' textinput scrolls its own
		// value horizontally to keep the cursor visible; its Width is set on the
		// PERSISTED model in the '/' open + typing-intercept paths (a value-
		// receiver assignment here would be discarded), so this func only READS.
		prefix := "  / "
		suffix := "  " + m.searchCounter()
		if m.width <= 0 {
			// Unknown terminal size (tests): no bounding, full content.
			line = prefix + m.searchInput.View() + suffix
		} else {
			leftBudget := m.width - ansi.StringWidth(suffix)
			if leftBudget < 0 {
				leftBudget = 0
			}
			left := ansi.Truncate(prefix+m.searchInput.View(), leftBudget, "")
			line = left + suffix
		}
	case m.searchQuery != "":
		var bar string
		if name, ok := m.cursorMatchName(); ok {
			// Cursor sits on a match — the ↳ glyph correctly implies "jumped to
			// this match", so name it.
			bar = fmt.Sprintf("  ↳ %s  %s", name, m.searchCounter())
		} else {
			// Cursor is off all matches (moved away with j/k): don't name the
			// non-matching row under a ↳ glyph — show the query + count instead.
			bar = fmt.Sprintf("  search %q  %s", m.searchQuery, m.searchCounter())
		}
		line = searchMatchStyle.Render(bar) + descStyle.Render("   n next · N prev · esc clear")
	default:
		return "  "
	}
	// UNCONDITIONAL final clamp: the per-mode budgeting above keeps the counter
	// visible at normal widths, but at absurdly narrow widths (m.width smaller
	// than the counter suffix, e.g. "  (no match)") the left segment truncates to
	// nothing and the un-truncated suffix would still exceed m.width and WRAP.
	// This last clamp is the hard guarantee of the one-physical-line invariant —
	// if the terminal is that narrow it's acceptable for the counter itself to be
	// clipped; what must NEVER happen is exceeding m.width. Mirrors clampToWidth's
	// m.width<=0 no-op so unknown-size (test) paths return full content.
	return clampToWidth(line, m.width)
}

// searchInputWidth returns the width budget for the search textinput's value
// area — m.width minus the "  / " prefix, the textinput's own 2-col prompt, and
// the trailing "  <counter>" suffix. Setting textinput.Width to this on the
// PERSISTED model (in the '/' open + typing-intercept paths) lets bubbles scroll
// the value horizontally to keep the cursor visible, instead of the outer clamp
// hiding the most-recently-typed characters. Returns 0 (bubbles' "unbounded")
// when m.width <= 0 (unknown terminal size, e.g. tests).
//
// STABLE budget: the suffix width is reserved for the WIDEST counter the bar can
// show for the current service-set size (maxCounterWidth), NOT the volatile
// current-match counter. If it tracked the live counter, the budget — and thus
// the input's horizontal-scroll window — would shrink/grow as the counter
// changed with each keystroke ("(1/1)" → "(no match)" → "(12/34)"), and bubbles
// computes its overflow offset inside Update() using the PREVIOUS Width, so the
// newest typed char/cursor could clip for one keystroke. A fixed budget lets
// Width be set BEFORE Update() with a value that stays correct across keystrokes.
// The render-time clamp in searchBarLine still holds regardless, so a slightly
// conservative budget can never let the bar wrap.
func (m Model) searchInputWidth() int {
	if m.width <= 0 {
		return 0
	}
	const prefixWidth = 4                  // "  / "
	const promptWidth = 2                  // textinput default prompt "> "
	suffixWidth := 2 + m.maxCounterWidth() // "  " + widest counter
	budget := m.width - prefixWidth - promptWidth - suffixWidth
	if budget < 1 {
		budget = 1
	}
	return budget
}

// maxCounterWidth returns the display width of the WIDEST counter searchCounter()
// can produce for the current service set — a keystroke-stable value used by
// searchInputWidth() so the input's Width does not fluctuate as the live counter
// changes. The candidates are: "(no match)" (empty query / zero matches), the
// widest "(i/N)" position form (both i and N at their max of len(services), the
// most matches possible), and the widest "(N matches)" off-match form. Whichever
// renders widest wins. Uses len(services) rather than len(searchMatches) because
// the match count varies per keystroke while len(services) is fixed for the
// screen — reserving for the theoretical max keeps the budget stable.
func (m Model) maxCounterWidth() int {
	n := len(m.services)
	w := ansi.StringWidth("(no match)")
	if n > 0 {
		if c := ansi.StringWidth(fmt.Sprintf("(%d/%d)", n, n)); c > w {
			w = c
		}
		if c := ansi.StringWidth(fmt.Sprintf("(%d matches)", n)); c > w {
			w = c
		}
	}
	return w
}

// clampToWidth truncates s so its DISPLAY width does not exceed width, in an
// ANSI-aware way (escape sequences are preserved and reset, wide runes counted
// by cell). width <= 0 means "unknown terminal size" (e.g. tests that never set
// m.width) and returns s unchanged, mirroring how svcVisibleCount treats
// m.height == 0. The reserved search bar is the only footer line whose content
// is user-controlled and unbounded, so this guards the constant-height invariant.
func clampToWidth(s string, width int) string {
	if width <= 0 {
		return s
	}
	return ansi.Truncate(s, width, "")
}

// cursorMatchName returns the service name under the cursor and true when the
// cursor sits on a search match; ("", false) when the cursor is off all matches
// (or out of range). Used by searchBarLine to avoid labelling a non-matching row
// with the ↳ "jumped to a match" glyph.
func (m Model) cursorMatchName() (string, bool) {
	if m.svcCursor < 0 || m.svcCursor >= len(m.services) {
		return "", false
	}
	for _, idx := range m.searchMatches {
		if idx == m.svcCursor {
			return m.services[m.svcCursor], true
		}
	}
	return "", false
}

// logTailStatus reports the follow state of the log viewport for the header
// indicator. done (stream ended) → ("", 0): nothing to follow, no indicator.
// At the live bottom → ("following", 0). Otherwise ("paused", N) where N is the
// distance in display rows to the bottom (how far G will jump), clamped at 0.
// Pure: derives everything from viewport geometry, so it needs no Model field
// and stays correct through resize / wrap / pretty reformats.
func logTailStatus(vp viewport.Model, done bool) (label string, below int) {
	if done {
		return "", 0
	}
	if vp.AtBottom() {
		return "following", 0
	}
	below = vp.TotalLineCount() - vp.YOffset - vp.Height
	if below < 0 {
		below = 0
	}
	return "paused", below
}

func (m Model) viewLogs() string {
	var b strings.Builder
	// Render the title with a margin-less copy of titleStyle so the indicator
	// lands on the SAME physical line as the breadcrumb/title. titleStyle's own
	// MarginBottom(1) would otherwise emit a trailing "\n<spaces>" line and push
	// the appended indicator down onto that margin line. lipgloss styles are
	// value types, so this copy does not mutate the shared titleStyle.
	header := titleStyle.UnsetMarginBottom().Render(fmt.Sprintf("%s > logs > %s", m.breadcrumb(), m.logsService))
	if label, below := logTailStatus(m.logsViewport, m.logsDone); label != "" {
		var indicator string
		if label == "following" {
			indicator = logFollowStyle.Render("● following")
		} else {
			indicator = logPauseStyle.Render(fmt.Sprintf("⏸ paused ▲ %d below", below))
		}
		header += "  " + indicator
	}
	b.WriteString(header)
	// Reproduce the vertical spacing that titleStyle's MarginBottom(1) plus the
	// old "\n\n" used to produce: one newline to close the title line, then two
	// more for the margin-equivalent blank line and the separator blank line.
	b.WriteString("\n\n\n")

	b.WriteString(m.logsViewport.View())
	b.WriteString("\n")
	// Reserved one-physical-line bar directly under the viewport (blank when idle
	// so the list height never jumps). The extra "\n" restores the blank
	// separator before the help footer that helpStyle's MarginTop(1) alone used to
	// provide — the net effect is +1 physical line, matching vpHeight's -6→-7.
	b.WriteString(m.logBarLine())
	b.WriteString("\n")

	var help string
	switch {
	case m.logSearching, m.logFiltering:
		// While an input is open, q/n/N type literally — surface only the
		// commit/cancel/regex affordances instead of the action-key legend.
		help = "  enter commit  •  esc cancel  •  ctrl+r regex"
	default:
		help = "  up/down scroll"
		if !m.logsWrap {
			help += "  •  <-/-> scroll"
		}
		help += "  •  G bottom"
		if m.logsWrap {
			help += "  •  w unwrap"
		} else {
			help += "  •  w wrap"
		}
		if m.logsPretty {
			help += "  •  p raw"
		} else {
			help += "  •  p pretty"
		}
		help += "  •  / search  •  f filter"
		if m.logSearchQuery != "" {
			help += "  •  n/N cycle"
		}
		help += "  •  q back"
	}
	b.WriteString(helpStyle.Render(help))
	return b.String()
}

func (m Model) viewConfig() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(fmt.Sprintf("%s > config", m.breadcrumb())))
	b.WriteString("\n\n")

	if m.configErr != nil {
		b.WriteString(stepFailed.Render(fmt.Sprintf("  Error: %v\n", m.configErr)))
	} else if m.configContent == nil && !m.configShowRes {
		b.WriteString("  Loading...\n")
	} else {
		b.WriteString(m.configViewport.View())
		b.WriteString("\n")
	}

	// Validation status line
	if m.configValid != nil {
		if *m.configValid {
			b.WriteString(stepDone.Render("  Config valid"))
			b.WriteString("\n")
		} else {
			b.WriteString(stepFailed.Render(fmt.Sprintf("  Config error: %s", m.configValidMsg)))
			b.WriteString("\n")
		}
	}

	// Help bar
	help := "  "
	if m.configShowRes {
		help += "r raw"
	} else {
		help += "r resolved"
	}
	help += "  •  e edit  •  up/down scroll  •  q back"
	b.WriteString(helpStyle.Render(help))
	return b.String()
}

func (m Model) viewInspect() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(fmt.Sprintf("%s > inspect > %s", m.breadcrumb(), m.inspectService)))
	b.WriteString("\n\n")

	if m.inspectErr != nil {
		// The chrome is budgeted for exactly ONE extra line, but the compose
		// layer feeds raw stderr in here (an SSH banner, a multi-line docker
		// failure), so the message is collapsed to one line and clamped to the
		// pane — the same never-wrap discipline searchBarLine and logBarLine
		// follow. Without it the renderer keeps only the last m.height lines
		// and the title scrolls off.
		oneLine := strings.Join(strings.Fields(m.inspectErr.Error()), " ")
		b.WriteString(stepFailed.Render(clampToWidth("  Error: "+oneLine, m.width)))
		b.WriteString("\n")
	}
	switch {
	case len(m.inspectRaw) > 0:
		// A parse failure keeps the raw bytes, so the viewport stays on screen
		// under the error line and r remains a working escape hatch.
		b.WriteString(m.inspectViewport.View())
		b.WriteString("\n")
	case m.inspectErr == nil:
		b.WriteString("  Loading...\n")
	}

	// Footer names only what the screen binds. pgup/pgdown and the esc alias
	// live in the ? overlay, matching viewConfig's shorter legend.
	help := "  "
	if m.inspectShowRaw {
		help += "r summary"
	} else {
		help += "r raw JSON"
	}
	help += "  •  up/down scroll  •  q back"
	// Clamped for the same reason as the error line above, and the same way
	// containerFooter does it: the footer is 42 cells, so below 42 columns both
	// its line and the helpStyle margin line wrap, add two physical rows the
	// renderer does not know about, and push the title off the top.
	b.WriteString(helpStyle.Render(clampToWidth(help, m.width)))
	return b.String()
}

func (m Model) viewProgress() string {
	var b strings.Builder
	containers := m.selectedContainers()

	b.WriteString(titleStyle.Render(fmt.Sprintf("%s > %s > %s", m.breadcrumb(), m.pendingOp.String(), strings.Join(containers, ", "))))
	b.WriteString("\n\n")

	for _, s := range m.steps {
		var icon, label string
		switch s.status {
		case runner.StatusDone:
			icon = stepDone.Render("✓")
			label = stepDone.Render(s.name)
		case runner.StatusRunning:
			icon = m.spinner.View()
			label = stepRunning.Render(s.name)
		case runner.StatusFailed:
			icon = stepFailed.Render("✗")
			label = stepFailed.Render(s.name)
		default:
			icon = stepWaiting.Render("○")
			label = stepWaiting.Render(s.name)
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", icon, label))
	}

	// Rollback prep failure: shown when PrepareRollback aborted before any
	// pipeline step ran (so no step carries the ✗). The op is marked failed, so
	// the footer already reads "q back".
	if m.rollbackErr != "" {
		b.WriteString("\n")
		b.WriteString(stepFailed.Render("  ✗ rollback prep failed: " + m.rollbackErr))
		b.WriteString("\n")
	}

	// Health-wait sub-state: live per-service verdicts (while polling) or the
	// final verdicts (after natural resolution). esc-skip clears waitState, so
	// this block is silent once a wait has been skipped.
	if m.waiting || m.waitResolved() {
		b.WriteString("\n")
		if m.waiting {
			remaining := time.Until(m.waitDeadline).Round(time.Second)
			if remaining < 0 {
				remaining = 0
			}
			b.WriteString(waitPendingStyle.Render(fmt.Sprintf("  Waiting for health (%s remaining)", remaining)))
		} else {
			b.WriteString(descStyle.Render("  Health check:"))
		}
		b.WriteString("\n")

		width := 0
		for _, svc := range m.waitState.Services {
			if len(svc) > width {
				width = len(svc)
			}
		}
		for _, svc := range m.waitState.Services {
			v := m.waitState.Verdicts[svc]
			label := string(v)
			if v == runner.VerdictPending {
				label = "pending"
			}
			style := waitVerdictStyle(v)
			b.WriteString(fmt.Sprintf("  %s %-*s  %s\n", style.Render(v.Icon()), width, svc, style.Render(label)))
		}

		// Rollback hint on a failed deploy wait only (a restart/rollback failure
		// has no earlier snapshot to fall back to).
		if m.waitResolved() && m.waitFailed() && m.pendingOp == runner.Deploy {
			b.WriteString(waitHintStyle.Render("  press R on the services screen to roll back"))
			b.WriteString("\n")
		}
	}

	switch {
	case m.waiting:
		b.WriteString(helpStyle.Render("\n  esc skip"))
	case m.done || m.failed:
		b.WriteString(helpStyle.Render("\n  q back"))
	default:
		b.WriteString(helpStyle.Render("\n  esc cancel"))
	}

	return b.String()
}

func (m Model) viewSettingsList() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("cdeploy > settings > servers"))
	b.WriteString("\n\n")

	if m.settingsErr != "" {
		b.WriteString(stepFailed.Render("  "+m.settingsErr) + "\n\n")
	}

	if len(m.config.Servers) == 0 {
		b.WriteString(descStyle.Render("  No servers configured. Press 'a' to add one."))
		b.WriteString("\n")
	} else {
		// Compute column widths
		maxName, maxHost, maxGroup := 4, 4, 5 // "Name", "Host", "Group"
		for _, s := range m.config.Servers {
			if len(s.Name) > maxName {
				maxName = len(s.Name)
			}
			if len(s.Host) > maxHost {
				maxHost = len(s.Host)
			}
			if len(s.Group) > maxGroup {
				maxGroup = len(s.Group)
			}
		}

		// Header
		header := fmt.Sprintf("     %-*s  %-*s  %-*s  %s",
			maxName, "Name", maxHost, "Host", maxGroup, "Group", "Color")
		b.WriteString(descStyle.Render(header))
		b.WriteString("\n")

		for i, s := range m.config.Servers {
			cursor := "  "
			style := itemStyle
			if i == m.settingsCursor {
				cursor = "> "
				style = selectedItemStyle
			}

			// Resolve color: group color for grouped servers, server color for ungrouped
			effectiveColor := s.Color
			isGroupColor := false
			if s.Group != "" {
				effectiveColor = m.config.GroupColor(s.Group)
				isGroupColor = true
			}
			colorDisplay := descStyle.Render("-")
			if effectiveColor != "" {
				colorDisplay = serverBadgeStyle(effectiveColor).Render(" " + effectiveColor + " ")
				if isGroupColor {
					colorDisplay += " " + descStyle.Render("(group)")
				}
			}

			groupDisplay := s.Group
			if groupDisplay == "" {
				groupDisplay = "-"
			}

			line := fmt.Sprintf("%s%-*s  %-*s  %-*s  %s",
				cursor, maxName, s.Name, maxHost, s.Host, maxGroup, groupDisplay, colorDisplay)
			b.WriteString(style.Render(line))
			b.WriteString("\n")
		}
	}

	if m.settingsDelete {
		name := m.config.Servers[m.settingsCursor].Name
		b.WriteString("\n")
		b.WriteString(warningStyle.Render(fmt.Sprintf("  Delete server %q? (y/n)", name)))
	}

	b.WriteString(helpStyle.Render("\n  a add  •  enter edit  •  d delete  •  q back"))
	return b.String()
}

func (m Model) viewSettingsForm() string {
	var b strings.Builder

	title := "Add Server"
	if m.settingsEditing >= 0 {
		title = "Edit Server"
	}
	b.WriteString(titleStyle.Render("cdeploy > settings > " + title))
	b.WriteString("\n\n")

	if m.settingsErr != "" {
		b.WriteString(stepFailed.Render("  "+m.settingsErr) + "\n\n")
	}

	labels := [4]string{"Name", "Host", "Project Dir", "Group"}
	for i, label := range labels {
		indicator := "  "
		if i == m.settingsField {
			indicator = "> "
		}
		b.WriteString(fmt.Sprintf("  %s%-12s %s\n", indicator, label+":", m.settingsInputs[i].View()))
	}

	// Color picker
	indicator := "  "
	if m.settingsField == 4 {
		indicator = "> "
	}
	hasGroup := strings.TrimSpace(m.settingsInputs[3].Value()) != ""
	colorVal := "(none)"
	if m.settingsColor != "" {
		colorVal = serverBadgeStyle(m.settingsColor).Render(" " + m.settingsColor + " ")
	}
	if hasGroup {
		if m.settingsColor != "" {
			colorVal += " " + descStyle.Render("(group)")
		} else {
			colorVal = descStyle.Render("(group)")
		}
		b.WriteString(fmt.Sprintf("  %s%-12s %s\n", indicator, "Color:", colorVal))
	} else {
		b.WriteString(fmt.Sprintf("  %s%-12s < %s >\n", indicator, "Color:", colorVal))
	}

	b.WriteString(helpStyle.Render("\n  tab/shift-tab cycle fields  •  ←/→ color  •  enter save  •  esc discard"))
	return b.String()
}

// Run launches the TUI.
func Run(composer runner.Composer, logWriter io.Writer, factory ComposerFactory, servers []config.Server, connectCb ConnectCallback, opts ...Option) error {
	m := NewModel(composer, logWriter, factory, servers, connectCb, opts...)
	p := tea.NewProgram(m, tea.WithAltScreen())
	finalModel, err := p.Run()
	if fm, ok := finalModel.(Model); ok {
		fm.ctxCancel()
		if fm.disconnectFunc != nil {
			_ = fm.disconnectFunc()
		}
	}
	return err
}
