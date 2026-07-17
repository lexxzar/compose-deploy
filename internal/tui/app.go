package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
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

// ComposerFactory creates a runner.Composer for the given project directory.
type ComposerFactory func(projectDir string) runner.Composer

// ProjectLoader loads the list of projects (local or remote).
type ProjectLoader func(ctx context.Context) ([]compose.Project, error)

// ConnectCallback is called when a remote server is selected. It returns
// the SSH connect command (for tea.ExecProcess), a ComposerFactory,
// a ProjectLoader, and a disconnect function.
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

	// Screen 2: progress
	steps       []stepState
	logContent  string
	logViewport viewport.Model
	spinner     spinner.Model
	done        bool
	failed      bool
	eventCh     <-chan runner.StepEvent
	cancel      context.CancelFunc

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
	logFiltering     bool            // filter bar open, capturing text
	logFilterInput   textinput.Model // (re)constructed lazily in the "f" open handler
	logFilterQuery   string          // last-good committed query; != "" ⇒ filter active
	logFilterIsRegex bool            // live/desired mode; ctrl+r toggles (regex vs. substring)
	logFilterRe      *regexp.Regexp  // compiled matcher when the last-good query is a valid regex; nil ⇒ substring (source of truth for regex-ness in logFilterPred)

	// Screen: logs — search-within-highlight (see clearLogSearch/logSearchPred/recomputeLogSearch)
	logSearching     bool            // search bar open, capturing text
	logSearchInput   textinput.Model // (re)constructed lazily in the "/" open handler
	logSearchQuery   string          // live/committed query; != "" ⇒ highlights + n/N active
	logSearchIsRegex bool            // live/desired mode; ctrl+r toggles (regex vs. substring)
	logSearchRe      *regexp.Regexp  // compiled matcher when the query is a valid regex; nil ⇒ substring
	logSearchMatches []int           // PHYSICAL line indices of matches, ascending (recomputed at SetContent time)
	logSearchCur     int             // index into logSearchMatches (the current match)

	// Screen: config
	configContent  []byte         // raw compose file content
	configResolved []byte         // resolved/interpolated config (cached)
	configViewport viewport.Model // viewport for config content
	configShowRes  bool           // true = showing resolved, false = showing raw
	configErr      error          // error from config operations
	configValid    *bool          // nil = not checked, true = valid, false = invalid
	configValidMsg string         // validation error message
	configSession  uint64         // monotonic counter for stale message rejection

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
		m.updateInFlight = true  // Init() will fire refreshUpdates on the fast-path; updatesMsg arrival clears the flag
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
	// for the maybe-fetch helper here.
	return tea.Batch(m.loadServices(), m.refreshStats(), m.refreshUpdates(), tick)
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
			h := msg.Height - 6
			if h < 3 {
				h = 3
			}
			m.logsViewport.Height = h
			m.fullReformat()
			if following {
				m.logsViewport.GotoBottom()
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
		}
		m.fixSvcOffset()
		// Self-heal: if no fresh cache entry exists (TTL expired or never
		// fetched) and no refreshUpdates is in flight, queue a fresh fetch.
		// Without this, a user who lingers on the container screen past the
		// TTL window would see update glyphs vanish silently on the next
		// periodic statusMsg — the overwrite above wipes UpdateAvailable
		// and the cache lookup misses, so nothing replaces them. Mirror
		// maybeRefreshUpdatesCmd's in-flight guard discipline so we never
		// stack a second fetch.
		if !fresh && !m.updateInFlight && m.composer != nil {
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
			m.clearSearch()
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
		if msg.err != nil {
			m.logsErr = msg.err
			// Flush any in-flight partial line into the buffer so it is not lost.
			if m.logsPartial != "" {
				m.logsRawLines = append(m.logsRawLines, m.logsPartial)
				m.logsPartial = ""
			}
			// The terminal error is filter-exempt: it must render regardless of an
			// active filter, so it lives in a dedicated slot outside the filterable
			// raw-line buffer.
			m.logsErrLine = fmt.Sprintf("Error: %v", msg.err)
			m.applyLogFormat()
			m.logsViewport.GotoBottom() // force the error into view even if paused
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
		if !m.failed {
			m.done = true
		}
		return m, nil

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
		return m, nil
	}
	return m, tea.Quit
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Quit confirmation intercept: when quitting is true, handle y/n/esc
	// and swallow all other keys.
	if m.quitting {
		switch key {
		case "y":
			return m, tea.Quit
		case "n", "esc":
			m.quitting = false
			return m, nil
		}
		return m, nil
	}

	// q acts as a back key inside the app. It quits only when there is
	// no parent screen to navigate to (server-select, or the project /
	// containers screens when standalone). On the settings form, q falls
	// through to the focused textinput so server names like "qa-prod" are
	// typeable — except when the color picker (field 4) is focused, where
	// q acts as back. screenProgress while running is also excluded so q
	// cannot cancel an in-flight operation.
	if key == "q" {
		switch m.screen {
		case screenSelectServer:
			return m, tea.Quit
		case screenSelectProject:
			if len(m.servers) == 0 && m.config == nil {
				return m, tea.Quit
			}
			key = "esc"
		case screenSelectContainers:
			if m.searching {
				// Search bar is capturing text — q is a literal character
				// that must reach the searchInput (mirrors the settings-form
				// field-4 textinput exception above). Leave key untouched.
				break
			}
			if !m.showPicker && !m.confirming {
				return m, tea.Quit
			}
			key = "esc"
		case screenProgress:
			if !m.done && !m.failed {
				return m, nil
			}
			key = "esc"
		case screenLogs:
			if m.logFiltering || m.logSearching {
				// A filter or search bar is capturing text — q is a literal
				// character that must reach the open input (mirrors the
				// container-search and settings-form field-4 exceptions above).
				// Leave key untouched so it falls through to the typing intercept.
				break
			}
			key = "esc"
		case screenSettingsForm:
			if m.settingsField == 4 {
				key = "esc"
			}
			// else fall through — textinput consumes it
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
				m.clearSearch()
				if m.localComposer != nil {
					m.composer = m.localComposer
					m.statsSession++
					m.statusSession++
					m.updatesSession++
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
			m.composer = m.composerFactory(proj.ConfigDir)
			m.statsSession++
			m.statusSession++
			m.updatesSession++
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
			if len(m.services) > 0 {
				m.selected[m.svcCursor] = !m.selected[m.svcCursor]
			}
		case "a":
			allSel := m.allSelected()
			for i := range m.services {
				m.selected[i] = !allSel
			}
		case "r":
			if m.selectedCount() > 0 {
				m.pendingOp = runner.Restart
				m.confirming = true
			} else {
				m.warning = warnNoSelection
			}
			m.fixSvcOffset()
		case "d":
			if m.selectedCount() > 0 {
				m.pendingOp = runner.Deploy
				m.confirming = true
			} else {
				m.warning = warnNoSelection
			}
			m.fixSvcOffset()
		case "s":
			if m.selectedCount() > 0 {
				m.pendingOp = runner.StopOnly
				m.confirming = true
			} else {
				m.warning = warnNoSelection
			}
			m.fixSvcOffset()
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
			if _, ok := m.composer.(ConfigProvider); ok {
				return m.enterConfig()
			}
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
				var cmd tea.Cmd
				m.logFilterInput, cmd = m.logFilterInput.Update(msg)
				m.recomputeLogFilter()
				return m, cmd
			}
		}

		// Search typing intercept — the Task 4 companion to the filter intercept
		// above. While the search bar is open every keystroke routes to
		// logSearchInput so q/n/N type literally instead of firing back/cycle.
		if m.logSearching {
			switch key {
			case "ctrl+c":
				return m.tryQuit()
			case "enter":
				// Commit: close the bar, keep highlights + n/N live. An empty
				// query fully clears so no dead search state lingers.
				if m.logSearchQuery == "" {
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
			m.logsErrLine = ""
			m.clearLogFilter()
			m.clearLogSearch()
			m.screen = screenSelectContainers
			m.statsSession++
			m.statusSession++
			m.updatesSession++
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
			m.logFilterInput = textinput.New()
			m.logFiltering = true
			m.logFilterInput.Focus()
			return m, nil
		case "/":
			// Open search-within-highlight. Early-return on an empty rendered
			// buffer (nothing to search). The input is built lazily here — NOT in
			// NewModel — so Model{} test literals stay valid; it is only rendered
			// while logSearching.
			if m.derivedLogContent() == "" {
				return m, nil
			}
			m.logSearchInput = textinput.New()
			m.logSearching = true
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
				// after a failed deploy. The cache key matches
				// updatesCacheKey()'s format (projDir|serverName) so the
				// next maybeRefreshUpdatesCmd misses, sees no in-flight
				// fetch, and enqueues a fresh refresh. This block runs
				// BEFORE the m.done/m.failed reset below — otherwise
				// reading m.done after clearing it would always evaluate
				// false and the invalidation would never fire.
				if m.done && (m.pendingOp == runner.Deploy || m.pendingOp == runner.Restart) {
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
				m.statsSession++
				m.statusSession++
				m.updatesSession++
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

	go runner.Run(ctx, m.composer, op, containers, logW, events)

	return *m, tea.Batch(m.spinner.Tick, m.waitForEvent())
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
	m.logsErrLine = ""
	m.clearLogFilter()
	m.clearLogSearch()

	vpHeight := m.height - 6
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
// survivor — see the survivor-cursor trap in foldNewRawLines). Task 2 passes a
// nil (all-pass) predicate, so with no filter the behaviour is identical to the
// pre-refactor pipeline. Call fullReformat() when the filter, toggles, or width
// change.
func (m *Model) applyLogFormat() {
	delta, newScanned := foldNewRawLines(m.logsRawLines, m.logsScanned, m.logsViewport.Width, m.logsWrap, m.logsPretty, m.logFilterPred())
	if delta != "" {
		if m.logsFormatted == "" {
			m.logsFormatted = delta
		} else {
			m.logsFormatted += "\n" + delta
		}
	}
	m.logsScanned = newScanned
	m.setLogViewportContent()
}

// derivedLogContent assembles the exact string handed to the log viewport:
// the cached formatted survivors, then the unfiltered in-flight partial line,
// then the filter-exempt terminal error. It is pure over the current Model
// state so tests can assert on the derived content directly.
func (m *Model) derivedLogContent() string {
	content := m.logsFormatted
	if m.logsPartial != "" {
		tail := formatLogLines([]string{m.logsPartial}, m.logsViewport.Width, m.logsWrap, m.logsPretty)
		if content == "" {
			content = tail
		} else {
			content += "\n" + tail
		}
	}
	if m.logsErrLine != "" {
		errFmt := formatLogLines([]string{m.logsErrLine}, m.logsViewport.Width, m.logsWrap, m.logsPretty)
		if content == "" {
			content = errFmt
		} else {
			content += "\n\n" + errFmt // blank line before the terminal error
		}
	}
	return content
}

// fullReformat re-processes all content from scratch. Used when toggles change
// or the viewport is resized, since width/mode changes affect every line.
func (m *Model) fullReformat() {
	m.logsScanned = 0
	m.logsFormatted = ""
	m.applyLogFormat()
}

// logFilterPred reconstructs the last-good filter predicate from the committed
// state. logFilterQuery and logFilterRe are only ever set together (by
// recomputeLogFilter, when buildMatcher succeeded), so this always reproduces a
// valid predicate. Regex-ness is derived from logFilterRe (nil ⇒ substring),
// NOT from the live logFilterIsRegex — so a mid-type ctrl+r into a *bad* regex
// keeps the last-good matcher rather than silently dropping the filter. Returns
// nil (all-pass) when no filter is committed, degrading the derivation pipeline
// to the Task 2 pass-through. The regex is recompiled once per call (reused
// across every line inside deriveFiltered), never per line.
func (m Model) logFilterPred() func(string) bool {
	if m.logFilterQuery == "" {
		return nil
	}
	pred, _, _ := buildMatcher(m.logFilterQuery, m.logFilterRe != nil, true)
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
// skip the re-derive so the view holds steady. logFilterRe carries the compiled
// regex (nil for substring) so logFilterPred can reproduce the exact last-good.
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
			m.logFilterRe = nil
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
	m.logFilterRe = re // nil for substring, non-nil for a valid regex
	m.rederiveLogs()
}

// clearLogFilter resets every filter field to the no-filter default. Called on
// entry to the log screen and on the esc-to-containers cleanup, and used by the
// typing-cancel path to discard an in-progress query.
func (m *Model) clearLogFilter() {
	m.logFiltering = false
	m.logFilterQuery = ""
	m.logFilterIsRegex = false
	m.logFilterRe = nil
	m.logFilterInput.SetValue("")
	m.logFilterInput.Blur()
}

// logSearchPred reconstructs the live search predicate from the committed search
// state. It mirrors logFilterPred but with allowNegate=false — search has no "!"
// exclusion (that is a filter-only affordance). Regex-ness derives from
// logSearchRe (nil ⇒ substring), so a mid-type ctrl+r into a *bad* regex keeps
// the last-good matcher. Returns nil (no highlight) when no query is active.
func (m Model) logSearchPred() func(string) bool {
	if m.logSearchQuery == "" {
		return nil
	}
	pred, _, _ := buildMatcher(m.logSearchQuery, m.logSearchRe != nil, false)
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
		m.logSearchRe = nil
		m.logSearchCur = 0
		m.setLogViewportContent() // clears matches + highlight
		return
	}
	_, re, valid := buildMatcher(newQuery, m.logSearchIsRegex, false)
	if !valid {
		return // bad regex mid-type: keep last-good, no re-highlight
	}
	m.logSearchQuery = newQuery
	m.logSearchRe = re // nil for substring, non-nil for a valid regex
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
// (Task 6 renders the bar; this keeps the counter logic beside the state).
// "(no match)" when the query matched nothing; i is 1-based (logSearchCur+1).
func (m Model) logSearchCounter() string {
	n := len(m.logSearchMatches)
	if n == 0 {
		return "(no match)"
	}
	return fmt.Sprintf("(%d/%d)", m.logSearchCur+1, n)
}

// clearLogSearch resets every search field to the no-search default. Called on
// entry to the log screen and on the esc-to-containers cleanup, and used by the
// typing-cancel path to discard an in-progress query.
func (m *Model) clearLogSearch() {
	m.logSearching = false
	m.logSearchQuery = ""
	m.logSearchIsRegex = false
	m.logSearchRe = nil
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
// is projDir + "|" + serverName. Empty serverName means local. For the
// local-fast-track entry (no project picker), projDir is empty too, so the
// key is just "|".
func (m Model) updatesCacheKey() string {
	return m.projDir + "|" + m.serverName
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
	m.updateInFlight = true
	return m.refreshUpdates()
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
// Footer varies by state: confirming = 3; normal = 3 (one-line help) or 4 (two-line help).
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
	// across search-idle / searching / committed / confirming because the merged
	// bar+margin line is present in every state (blank while confirming, with the
	// confirm prompt taking the footer-text slot).
	var footerLines int
	if m.confirming {
		footerLines = 3
	} else {
		// Compute whether help fits on one line (same logic as viewSelectContainers)
		back := "q quit"
		if m.showPicker {
			back = "q back"
		}
		line1 := fmt.Sprintf("  space toggle  •  a all  •  / search  •  %s", back)
		line2 := "  r restart  •  d deploy  •  s stop  •  l logs  •  c config  •  x exec  •  U updates"
		if m.searching {
			line2 = "  enter jump  •  esc cancel"
		} else if m.searchQuery != "" {
			line2 = "  n/N cycle  •  esc clear"
		}
		oneLine := line1 + "  •  " + line2[2:]
		if m.width >= len(oneLine)+2 {
			footerLines = 3
		} else {
			// two-line help adds one more = 4.
			footerLines = 4
		}
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
		b.WriteString(descStyle.Render(shortenPath(proj.ConfigDir)))
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

func (m Model) viewSelectContainers() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(fmt.Sprintf(
		"%s > services (%d/%d selected)",
		m.breadcrumb(),
		m.selectedCount(),
		len(m.services),
	)))

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
		// Then the "Service" caption sits in the same column as service names.
		header := strings.Repeat(" ", 10) + fmt.Sprintf("%-*s", maxName, "Service")
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

		checkbox := checkboxOff.Render("[ ]")
		if m.selected[i] {
			checkbox = checkboxOn.Render("[x]")
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
	// search-idle / searching / committed / confirming. While confirming the
	// bar is suppressed (blank) because the confirm prompt takes precedence and
	// is shown in the footer-text slot below; otherwise the bar shows the search
	// input (typing), the compact committed summary, or a blank placeholder.
	if m.confirming {
		b.WriteString(gap + "  ")
	} else {
		b.WriteString(gap + m.searchBarLine())
	}

	// Confirming: the confirm prompt takes the footer-text slot (no help keys).
	// The reserved bar line above is blank so the total line count (gap + bar +
	// helpStyle MarginTop blank + confirm text) matches the non-confirming
	// footer exactly.
	if m.confirming {
		if m.pendingExec {
			service := m.services[m.svcCursor]
			b.WriteString(helpStyle.Render(fmt.Sprintf(
				"  Exec into %s?  enter confirm  •  esc cancel",
				service,
			)))
		} else {
			containers := m.selectedContainers()
			b.WriteString(helpStyle.Render(fmt.Sprintf(
				"  %s %s?  enter confirm  •  esc cancel",
				m.pendingOp.String(),
				strings.Join(containers, ", "),
			)))
		}
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

	back := "q quit"
	if m.showPicker {
		back = "q back"
	}
	line1 := fmt.Sprintf("  space toggle  •  a all  •  / search  •  %s", back)
	line2 := "  r restart  •  d deploy  •  s stop  •  l logs  •  c config  •  x exec  •  U updates"
	if m.searching {
		line2 = "  enter jump  •  esc cancel"
	} else if m.searchQuery != "" {
		line2 = "  n/N cycle  •  esc clear"
	}
	oneLine := line1 + "  •  " + line2[2:]
	// helpStyle's MarginTop supplies the single blank line between the reserved
	// bar (or warning) line and the help text.
	if m.width >= len(oneLine)+2 {
		b.WriteString(helpStyle.Render(oneLine))
	} else {
		b.WriteString(helpStyle.Render(line1 + "\n" + line2))
	}
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

	help := "  up/down scroll"
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
	help += "  •  q back"
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

	if m.done || m.failed {
		b.WriteString(helpStyle.Render("\n  q back"))
	} else {
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
