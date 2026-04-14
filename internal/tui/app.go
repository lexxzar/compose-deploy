package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/config"
	"github.com/lexxzar/compose-deploy/internal/runner"
)

// ComposerFactory creates a runner.Composer for the given project directory.
type ComposerFactory func(projectDir string) runner.Composer

// ProjectLoader loads the list of projects (local or remote).
type ProjectLoader func(ctx context.Context) ([]compose.Project, error)

// ConnectCallback is called when a remote server is selected. It returns
// the SSH connect command (for tea.ExecProcess), a ComposerFactory,
// a ProjectLoader, and a disconnect function.
type ConnectCallback func(server config.Server) (connectCmd *exec.Cmd, factory ComposerFactory, loader ProjectLoader, disconnect func() error)

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

	for _, g := range groups {
		if g.name != "" {
			entries = append(entries, serverEntry{kind: entryGroupHeader, group: g.name})
		}
		for _, idx := range g.indices {
			entries = append(entries, serverEntry{kind: entryServer, serverIdx: idx})
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
)

// Model is the Bubble Tea model for the cdeploy TUI.
type Model struct {
	screen screen

	// Screen: server select
	servers           []config.Server
	serverEntries     []serverEntry
	serverCursor      int
	serverErr         error
	serverName        string // selected server name, for breadcrumbs
	connectCb         ConnectCallback
	disconnectFunc    func() error
	projectLoader     ProjectLoader
	preselectedServer int  // index into servers for --server flag
	hasPreselection   bool // true when --server was specified

	// Local state (preserved across server selection changes)
	localComposer runner.Composer
	localFactory  ComposerFactory

	// Screen: project select
	projects        []compose.Project
	projCursor      int
	projErr         error
	projName        string // selected project name, for breadcrumbs
	showPicker      bool   // true if the project picker was shown
	composerFactory ComposerFactory

	// Screen 1: service select
	services   []string
	svcRunning map[string]bool // service name → running state
	selected   map[int]bool
	svcCursor  int
	svcErr     error

	// Confirmation state (within container screen)
	confirming bool
	pendingOp  runner.Operation

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
	running  map[string]bool
	err      error
}
type statusMsg struct {
	running map[string]bool
	err     error
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
type connectResultMsg struct{ err error }
type preselectedConnectMsg struct{}
type disconnectDoneMsg struct{}

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

func NewModel(composer runner.Composer, logWriter io.Writer, factory ComposerFactory, servers []config.Server, connectCb ConnectCallback, opts ...Option) Model {
	s := spinner.New()
	s.Spinner = spinner.Dot

	startScreen := screenSelectContainers
	showPicker := false

	if len(servers) > 0 {
		startScreen = screenSelectServer
	} else if composer == nil {
		startScreen = screenSelectProject
		showPicker = true
	}

	ctx, cancel := context.WithCancel(context.Background())

	m := Model{
		screen:          startScreen,
		showPicker:      showPicker,
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
		m.svcRunning = msg.running
		m.selected = make(map[int]bool)
		m.svcCursor = 0
		return m, nil

	case statusMsg:
		if msg.err != nil {
			m.svcErr = msg.err
			return m, nil
		}
		m.svcErr = nil
		m.svcRunning = msg.running
		return m, nil

	case stepEventMsg:
		return m.handleStepEvent(runner.StepEvent(msg))

	case preselectedConnectMsg:
		server := m.servers[m.preselectedServer]
		m.serverName = server.Name
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
			m.projectLoader = nil
			m.disconnectFunc = nil
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

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

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
				m.composerFactory = m.localFactory
				m.projectLoader = nil
				m.disconnectFunc = nil
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
		}

	case screenSelectProject:
		switch key {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "esc":
			if len(m.servers) > 0 {
				// Back to server screen — disconnect if remote
				disconnectFn := m.disconnectFunc
				m.screen = screenSelectServer
				m.serverName = ""
				m.disconnectFunc = nil
				m.projectLoader = nil
				m.composerFactory = nil
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
				return m, tea.Quit
			case "enter":
				containers := m.selectedContainers()
				return m.enterProgress(containers)
			case "esc":
				m.confirming = false
				return m, nil
			}
			return m, nil
		}

		switch key {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "esc":
			if m.showPicker {
				m.screen = screenSelectProject
				m.composer = nil
				m.projName = ""
				m.services = nil
				m.svcRunning = nil
				m.selected = make(map[int]bool)
				m.svcCursor = 0
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
		case "down", "j":
			if m.svcCursor < len(m.services)-1 {
				m.svcCursor++
			}
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
			}
		case "d":
			if m.selectedCount() > 0 {
				m.pendingOp = runner.Deploy
				m.confirming = true
			}
		case "s":
			if m.selectedCount() > 0 {
				m.pendingOp = runner.StopOnly
				m.confirming = true
			}
		case "l":
			if len(m.services) == 0 {
				return m, nil
			}
			return m.enterLogs()
		}

	case screenLogs:
		switch key {
		case "q", "ctrl+c":
			return m, tea.Quit
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

	case screenProgress:
		switch key {
		case "q", "ctrl+c":
			if m.done || m.failed {
				return m, tea.Quit
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
		var projects []compose.Project
		var err error
		if loader != nil {
			projects, err = loader(ctx)
		} else {
			projects, err = compose.ListProjects(ctx)
		}
		return projectsMsg{projects: projects, err: err}
	}
}

func (m Model) refreshStatus() tea.Cmd {
	ctx := m.ctx
	c := m.composer
	return func() tea.Msg {
		running, err := c.ContainerStatus(ctx)
		return statusMsg{running: running, err: err}
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
		running, err := c.ContainerStatus(ctx)
		if err != nil {
			return servicesMsg{err: err}
		}
		return servicesMsg{services: services, running: running}
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

func (m Model) View() string {
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
	}
	return ""
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

	b.WriteString(helpStyle.Render("\n  up/down navigate  •  enter select  •  q quit"))
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
		parts = append(parts, m.serverName)
	}
	if m.projName != "" {
		parts = append(parts, m.projName)
	}
	return strings.Join(parts, " > ")
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
		b.WriteString(helpStyle.Render("\n  q quit"))
		return b.String()
	}

	if len(m.projects) == 0 {
		b.WriteString("  No Docker Compose projects found\n")
		b.WriteString(helpStyle.Render("\n  q quit"))
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

func (m Model) viewSelectContainers() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(fmt.Sprintf(
		"%s > services (%d/%d selected)",
		m.breadcrumb(),
		m.selectedCount(),
		len(m.services),
	)))
	b.WriteString("\n\n")

	if m.services == nil && m.svcErr == nil {
		b.WriteString("  Loading services...\n")
		return b.String()
	}

	if m.svcErr != nil {
		b.WriteString(stepFailed.Render(fmt.Sprintf("  Error: %v\n", m.svcErr)))
		if m.showPicker {
			b.WriteString(helpStyle.Render("\n  esc back  •  q quit"))
		} else {
			b.WriteString(helpStyle.Render("\n  q quit"))
		}
		return b.String()
	}

	for i, svc := range m.services {
		cursor := "  "
		if i == m.svcCursor {
			cursor = "> "
		}

		checkbox := checkboxOff.Render("[ ]")
		if m.selected[i] {
			checkbox = checkboxOn.Render("[x]")
		}

		dot := statusStoppedDot.Render("●")
		if m.svcRunning[svc] {
			dot = statusRunningDot.Render("●")
		}

		b.WriteString(fmt.Sprintf("%s%s %s %s\n", cursor, checkbox, dot, svc))
	}

	if m.confirming {
		containers := m.selectedContainers()
		b.WriteString(helpStyle.Render(fmt.Sprintf(
			"\n  %s %s?  enter confirm  •  esc cancel",
			m.pendingOp.String(),
			strings.Join(containers, ", "),
		)))
	} else {
		back := "q quit"
		if m.showPicker {
			back = "esc back"
		}
		line1 := fmt.Sprintf("  space toggle  •  a all  •  %s", back)
		line2 := "  r restart  •  d deploy  •  s stop  •  l logs"
		oneLine := line1 + "  •  " + line2[2:]
		if m.width >= len(oneLine)+2 {
			b.WriteString(helpStyle.Render("\n" + oneLine))
		} else {
			b.WriteString(helpStyle.Render("\n" + line1 + "\n" + line2))
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
