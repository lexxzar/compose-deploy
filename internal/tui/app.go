package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/config"
	"github.com/lexxzar/compose-deploy/internal/runner"
)

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

	// Screen 1: service select
	services  []string
	svcStatus map[string]runner.ServiceStatus // service name → status
	selected  map[int]bool
	svcCursor int
	svcOffset int // index of first visible service in scroll window
	svcErr    error

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
	logsService   string             // service being viewed
	logsContent   string             // accumulated log output
	logsViewport  viewport.Model     // dedicated viewport for log screen
	logsCancel    context.CancelFunc // cancels the log goroutine; derived from m.ctx
	logsDone      bool               // true when streaming finished
	logsErr       error              // error from Logs() call
	logsPipeR     io.Reader          // pipe reader for log streaming
	logsSession   uint64             // monotonic counter to discard stale messages from prior sessions
	logsWrap      bool               // soft-wrap long lines at viewport width
	logsPretty    bool               // pretty-print JSON log bodies
	logsFormatted string             // formatted output for complete raw lines (up to logsRawOff)
	logsRawOff    int                // byte offset into logsContent: everything before this is in logsFormatted

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
}
type servicesMsg struct {
	services []string
	status   map[string]runner.ServiceStatus
	err      error
}
type statusMsg struct {
	status map[string]runner.ServiceStatus
	err    error
}
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
	}

	return m
}

func (m Model) Init() tea.Cmd {
	if m.screen == screenSelectServer {
		if m.hasPreselection && m.preselectedServer >= 0 && m.preselectedServer < len(m.servers) {
			return func() tea.Msg { return preselectedConnectMsg{} }
		}
		return nil // server list is static from config
	}
	if m.showPicker {
		return m.loadProjects()
	}
	return m.loadServices()
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
		}
		if m.screen == screenLogs {
			m.logsViewport.Width = msg.Width - 4
			h := msg.Height - 6
			if h < 3 {
				h = 3
			}
			m.logsViewport.Height = h
			m.fullReformat()
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case projectsMsg:
		if msg.err != nil {
			m.projErr = msg.err
			return m, nil
		}
		m.projErr = nil
		m.projects = msg.projects
		m.projCursor = 0
		return m, nil

	case servicesMsg:
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
		return m, nil

	case statusMsg:
		if msg.err != nil {
			m.svcErr = msg.err
			return m, nil
		}
		m.svcErr = nil
		m.svcStatus = msg.status
		m.fixSvcOffset()
		return m, nil

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
		m.disconnectFunc = disconnect
		return m, tea.ExecProcess(connectCmd, func(err error) tea.Msg {
			return connectResultMsg{err: err}
		})

	case connectResultMsg:
		if msg.err != nil {
			m.serverErr = msg.err
			m.composerFactory = m.localFactory
			m.projectLoader = m.localProjectLoader
			m.disconnectFunc = nil
			m.quitting = false
			m.serverHost = ""
			m.serverColor = ""
			return m, nil
		}
		m.serverErr = nil
		m.showPicker = true
		m.screen = screenSelectProject
		return m, m.loadProjects()

	case disconnectDoneMsg:
		return m, nil

	case logChunkMsg:
		if m.screen != screenLogs || msg.session != m.logsSession {
			return m, nil
		}
		m.logsContent += string(msg.data)
		m.applyLogFormat()
		m.logsViewport.GotoBottom()
		return m, m.readLogChunk()

	case logDoneMsg:
		if m.screen != screenLogs || msg.session != m.logsSession {
			return m, nil
		}
		m.logsDone = true
		if msg.err != nil {
			m.logsErr = msg.err
			m.logsContent += fmt.Sprintf("\n\nError: %v", msg.err)
			m.applyLogFormat()
			m.logsViewport.GotoBottom()
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
		return m, m.refreshStatus()

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
				m.disconnectFunc = nil
				m.quitting = false
				if m.localComposer != nil {
					m.composer = m.localComposer
					m.showPicker = true
					m.screen = screenSelectContainers
					return m, m.loadServices()
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
		case "q", "ctrl+c":
			return m.tryQuit()
		case "esc":
			if len(m.servers) > 0 {
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
				m.projName = ""
				m.projects = nil
				m.projCursor = 0
				m.projErr = nil
				m.showPicker = false
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
			m.composer = m.composerFactory(proj.ConfigDir)
			m.screen = screenSelectContainers
			return m, m.loadServices()
		}

	case screenSelectContainers:
		if m.confirming {
			switch key {
			case "q", "ctrl+c":
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

		m.warning = ""

		switch key {
		case "q", "ctrl+c":
			return m.tryQuit()
		case "esc":
			if m.showPicker {
				m.screen = screenSelectProject
				m.composer = nil
				m.projName = ""
				m.services = nil
				m.svcStatus = nil
				m.selected = make(map[int]bool)
				m.svcCursor = 0
				m.svcOffset = 0
				m.svcErr = nil
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
		}

	case screenLogs:
		switch key {
		case "q", "ctrl+c":
			return m.tryQuit()
		case "esc":
			if m.logsCancel != nil {
				m.logsCancel()
			}
			m.logsService = ""
			m.logsContent = ""
			m.logsCancel = nil
			m.logsDone = false
			m.logsErr = nil
			m.logsPipeR = nil
			m.logsViewport = viewport.Model{}
			m.logsWrap = false
			m.logsPretty = false
			m.logsFormatted = ""
			m.logsRawOff = 0
			m.screen = screenSelectContainers
			return m, m.refreshStatus()
		case "w":
			m.logsWrap = !m.logsWrap
			if m.logsWrap {
				m.logsViewport.SetHorizontalStep(0)
			} else {
				m.logsViewport.SetHorizontalStep(4)
			}
			m.fullReformat()
			return m, nil
		case "p":
			m.logsPretty = !m.logsPretty
			m.fullReformat()
			return m, nil
		case "G":
			m.logsViewport.GotoBottom()
			return m, nil
		default:
			var cmd tea.Cmd
			m.logsViewport, cmd = m.logsViewport.Update(msg)
			return m, cmd
		}

	case screenConfig:
		switch key {
		case "q", "ctrl+c":
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
		case "q", "ctrl+c":
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
		case "q", "ctrl+c":
			if m.done || m.failed {
				return m.tryQuit()
			}
		case "esc":
			if m.done || m.failed {
				m.screen = screenSelectContainers
				m.confirming = false
				m.steps = nil
				m.done = false
				m.failed = false
				m.eventCh = nil
				m.cancel = nil
				m.logContent = ""
				return m, m.refreshStatus()
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
	m.logsContent = ""
	m.logsDone = false
	m.logsErr = nil
	m.logsSession++
	m.logsWrap = true
	m.logsPretty = false
	m.logsFormatted = ""
	m.logsRawOff = 0

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
	return *m, m.readLogChunk()
}

// applyLogFormat incrementally formats only new data since the last call.
// It scans only m.logsContent[m.logsRawOff:] for new complete lines, formats
// them, and appends to the cached logsFormatted. The trailing incomplete line
// is formatted fresh each time. Call fullReformat() when toggles or width change.
func (m *Model) applyLogFormat() {
	remaining := m.logsContent[m.logsRawOff:]

	// Find new complete lines (everything up to the last \n in remaining)
	if lastNL := strings.LastIndex(remaining, "\n"); lastNL >= 0 {
		completePart := remaining[:lastNL]
		newLines := strings.Split(completePart, "\n")
		formatted := formatLogLines(newLines, m.logsViewport.Width, m.logsWrap, m.logsPretty)
		if m.logsFormatted == "" {
			m.logsFormatted = formatted
		} else {
			m.logsFormatted += "\n" + formatted
		}
		m.logsRawOff += lastNL + 1
		remaining = remaining[lastNL+1:]
	}

	// Format the trailing incomplete line (if any) and combine with cache
	if len(remaining) > 0 {
		tail := formatLogLines([]string{remaining}, m.logsViewport.Width, m.logsWrap, m.logsPretty)
		if m.logsFormatted == "" {
			m.logsViewport.SetContent(tail)
		} else {
			m.logsViewport.SetContent(m.logsFormatted + "\n" + tail)
		}
	} else {
		m.logsViewport.SetContent(m.logsFormatted)
	}
}

// fullReformat re-processes all content from scratch. Used when toggles change
// or the viewport is resized, since width/mode changes affect every line.
func (m *Model) fullReformat() {
	m.logsRawOff = 0
	m.logsFormatted = ""
	m.applyLogFormat()
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
	return func() tea.Msg {
		if loader == nil {
			return projectsMsg{err: fmt.Errorf("no project loader configured")}
		}
		projects, err := loader(ctx)
		return projectsMsg{projects: projects, err: err}
	}
}

func (m Model) refreshStatus() tea.Cmd {
	ctx := m.ctx
	c := m.composer
	return func() tea.Msg {
		status, err := c.ContainerStatus(ctx)
		return statusMsg{status: status, err: err}
	}
}

func (m Model) loadServices() tea.Cmd {
	ctx := m.ctx
	c := m.composer
	return func() tea.Msg {
		services, err := c.ListServices(ctx)
		if err != nil {
			return servicesMsg{err: err}
		}
		status, err := c.ContainerStatus(ctx)
		if err != nil {
			return servicesMsg{err: err}
		}
		return servicesMsg{services: services, status: status}
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

// hasStatusColumns returns true if any service in m.services has non-empty Created, Uptime,
// or Ports data, indicating that column captions should be displayed.
func (m Model) hasStatusColumns() bool {
	for _, svc := range m.services {
		if st, ok := m.svcStatus[svc]; ok {
			if st.Created != "" || st.Uptime != "" || len(st.Ports) > 0 {
				return true
			}
		}
	}
	return false
}

// svcVisibleCount returns the number of services that fit in the terminal.
// Header: breadcrumb + blank line = 2 lines, plus 1 more when column captions are shown.
// Footer varies by state: confirming = 2; normal = 2 (one-line help) or 3 (two-line help).
// Warning adds 1 extra line.
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

	var footerLines int
	if m.confirming {
		// helpStyle MarginTop space-line (1) + gap-or-indicator (1) + confirm text (1) = 3
		footerLines = 3
	} else {
		// Compute whether help fits on one line (same logic as viewSelectContainers)
		back := "q quit"
		if m.showPicker {
			back = "esc back"
		}
		line1 := fmt.Sprintf("  space toggle  •  a all  •  %s", back)
		line2 := "  r restart  •  d deploy  •  s stop  •  l logs  •  c config  •  x exec"
		oneLine := line1 + "  •  " + line2[2:]
		if m.width >= len(oneLine)+2 {
			// helpStyle MarginTop (1) + gap-or-indicator (1) + one help line (1) = 3
			footerLines = 3
		} else {
			// helpStyle MarginTop (1) + gap-or-indicator (1) + two help lines (2) = 4
			footerLines = 4
		}
		if m.warning != "" {
			footerLines++ // warning line
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
		if len(m.servers) > 0 {
			help = "  esc back  q quit"
		}
		b.WriteString(helpStyle.Render("\n" + help))
		return b.String()
	}

	if len(m.projects) == 0 {
		b.WriteString("  No Docker Compose projects found\n")
		help := "  q quit"
		if len(m.servers) > 0 {
			help = "  esc back  q quit"
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
	if len(m.servers) > 0 {
		helpText = "\n  up/down navigate  •  enter select  •  esc back  •  q quit"
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
			b.WriteString(helpStyle.Render("\n  esc back  •  q quit"))
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
	// pattern in cmd/list.go formatDots/formatDotsGrouped).
	maxName := 0
	maxCreated := 0
	maxUptime := 0
	maxPorts := 0
	portsStr := make(map[string]string, len(m.services))
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
	if maxCreated > 0 || maxUptime > 0 || maxPorts > 0 {
		if maxCreated > 0 && len("Created") > maxCreated {
			maxCreated = len("Created")
		}
		if maxUptime > 0 && len("Uptime") > maxUptime {
			maxUptime = len("Uptime")
		}
		if maxPorts > 0 && len("Ports") > maxPorts {
			maxPorts = len("Ports")
		}
		// Left padding: cursor(2) + checkbox(3) + space(1) + health(1) + space(1) + dot(1) + space(1) + name
		padding := strings.Repeat(" ", 10+maxName)
		header := padding
		if maxCreated > 0 {
			header += fmt.Sprintf("  %-*s", maxCreated, "Created")
		}
		if maxUptime > 0 {
			header += fmt.Sprintf("  %-*s", maxUptime, "Uptime")
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

		// Build line: cursor + checkbox + health + dot + name [+ created] [+ uptime] [+ ports]
		line := fmt.Sprintf("%s%s %s %s %-*s", cursor, checkbox, health, dot, maxName, svc)
		if maxCreated > 0 {
			line += fmt.Sprintf("  %-*s", maxCreated, st.Created)
		}
		if maxUptime > 0 {
			line += fmt.Sprintf("  %-*s", maxUptime, st.Uptime)
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

	if m.confirming {
		gap := "\n"
		if below > 0 {
			gap = ""
		}
		if m.pendingExec {
			service := m.services[m.svcCursor]
			b.WriteString(helpStyle.Render(fmt.Sprintf(
				"%s  Exec into %s?  enter confirm  •  esc cancel",
				gap,
				service,
			)))
		} else {
			containers := m.selectedContainers()
			b.WriteString(helpStyle.Render(fmt.Sprintf(
				"%s  %s %s?  enter confirm  •  esc cancel",
				gap,
				m.pendingOp.String(),
				strings.Join(containers, ", "),
			)))
		}
	} else {
		if m.warning != "" {
			b.WriteString("\n  " + warningStyle.Render(m.warning))
		}
		back := "q quit"
		if m.showPicker {
			back = "esc back"
		}
		line1 := fmt.Sprintf("  space toggle  •  a all  •  %s", back)
		line2 := "  r restart  •  d deploy  •  s stop  •  l logs  •  c config  •  x exec"
		oneLine := line1 + "  •  " + line2[2:]
		gap := "\n"
		if below > 0 && m.warning == "" {
			gap = ""
		}
		if m.width >= len(oneLine)+2 {
			b.WriteString(helpStyle.Render(gap + oneLine))
		} else {
			b.WriteString(helpStyle.Render(gap + line1 + "\n" + line2))
		}
	}
	return b.String()
}

func (m Model) viewLogs() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(fmt.Sprintf("%s > logs > %s", m.breadcrumb(), m.logsService)))
	b.WriteString("\n\n")

	b.WriteString(m.logsViewport.View())
	b.WriteString("\n")

	help := "  esc back  •  up/down scroll"
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
	help += "  •  q quit"
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
	help := "  esc back  •  "
	if m.configShowRes {
		help += "r raw"
	} else {
		help += "r resolved"
	}
	help += "  •  e edit  •  up/down scroll  •  q quit"
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
		b.WriteString(helpStyle.Render("\n  esc back  •  q quit"))
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

	b.WriteString(helpStyle.Render("\n  a add  •  enter edit  •  d delete  •  esc back"))
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
