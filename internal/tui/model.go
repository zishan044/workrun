// Package tui contains the interactive task interface.
package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/zishan044/workrun/internal/output"
	"github.com/zishan044/workrun/internal/runner"
	"github.com/zishan044/workrun/internal/session"
	"github.com/zishan044/workrun/internal/task"
)

// Phase describes the UI action state, independently of task result status.
type Phase int

const (
	Idle Phase = iota
	Launching
	Active
	Stopping
)

// Focus identifies the pane receiving navigation keys.
type Focus int

const (
	Tasks Focus = iota
	Output
)

// runAcceptedMsg attaches an asynchronously accepted session run.
type runAcceptedMsg struct {
	RequestID uint64
	Run       *session.Run
	Err       error
}

type runFinishedMsg struct {
	RequestID uint64
	Result    runner.Result
}

type refreshMsg struct {
	RequestID uint64
	At        time.Time
}

const (
	wideMinWidth     = 76
	mediumMinWidth   = 52
	compactMinWidth  = 24
	wideMinHeight    = 16
	compactMinHeight = 13
)

type layoutMode int

const (
	compactLayout layoutMode = iota
	stackedLayout
	mediumLayout
	wideLayout
)

// Model is the Bubble Tea model for the task screen.
type Model struct {
	project             string
	tasks               []task.Spec
	manager             *session.Manager
	appCtx              context.Context
	shutdown            *ShutdownState
	selected            int
	phase               Phase
	focus               Focus
	requestID           uint64
	requestedAt         time.Time
	requestCancel       context.CancelCauseFunc
	run                 *session.Run
	outputRevision      uint64
	outputRecords       []output.Record
	outputRendered      bool
	outputRenderedWidth int
	outputEvicted       bool
	lastResults         map[string]runner.Status
	outputTask          string
	output              *output.Store
	outputView          viewport.Model
	followOutput        bool
	showHelp            bool
	confirmQuit         bool
	exitAfterRun        bool
	width, height       int
}

// NewModel constructs a sorted, idle TUI model from validated task specs.
func NewModel(project string, tasks []task.Spec, manager *session.Manager, appCtx context.Context, shutdown *ShutdownState) Model {
	if appCtx == nil {
		appCtx = context.Background()
	}
	if shutdown == nil {
		shutdown = &ShutdownState{}
	}
	ordered := append([]task.Spec(nil), tasks...)
	for i := range ordered {
		ordered[i].Argv = append([]string(nil), ordered[i].Argv...)
		if ordered[i].Env != nil {
			env := make(map[string]string, len(ordered[i].Env))
			for key, value := range ordered[i].Env {
				env[key] = value
			}
			ordered[i].Env = env
		}
	}
	slicesSortTasks(ordered)
	return Model{
		project:      project,
		tasks:        ordered,
		manager:      manager,
		appCtx:       appCtx,
		shutdown:     shutdown,
		lastResults:  make(map[string]runner.Status),
		output:       output.NewStore(output.DefaultLimits()),
		outputView:   viewport.New(),
		followOutput: true,
	}
}

// Init satisfies tea.Model. A task starts only in response to Enter.
func (m Model) Init() tea.Cmd { return nil }

// Update handles model input and asynchronous session messages.
func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resizeOutput()
		m.refreshOutputIfChanged()
	case ShutdownRequestedMsg:
		reason, _ := m.shutdown.Request(msg.Reason)
		return m.requestShutdown(reason)
	case runAcceptedMsg:
		if msg.RequestID != m.requestID || m.phase == Idle {
			if msg.Run != nil {
				msg.Run.Cancel(runner.ErrShutdown)
			}
			break
		}
		if msg.Err != nil {
			m.recordStartFailure(msg.Err)
			if m.exitAfterRun {
				return m, tea.Quit
			}
			break
		}
		m.run = msg.Run
		m.output = msg.Run.Output
		if m.phase != Stopping {
			m.phase = Active
		}
		return m, tea.Batch(m.waitForRun(msg.RequestID), m.refreshAfter(msg.RequestID))
	case runFinishedMsg:
		if msg.RequestID != m.requestID || m.phase == Idle {
			break
		}
		result := msg.Result
		result.TaskName = m.outputTask
		if result.RequestedAt.IsZero() {
			result.RequestedAt = m.requestedAt
		}
		m.lastResults[result.TaskName] = result.Status
		m.refreshOutputIfChanged()
		m.phase = Idle
		m.confirmQuit = false
		m.run = nil
		if m.requestCancel != nil {
			m.requestCancel(nil)
		}
		m.requestCancel = nil
		if m.exitAfterRun {
			return m, tea.Quit
		}
	case refreshMsg:
		if msg.RequestID != m.requestID || (m.phase != Active && m.phase != Stopping) {
			break
		}
		m.refreshOutputIfChanged()
		return m, m.refreshAfter(msg.RequestID)
	case tea.KeyPressMsg:
		return m.updateKey(msg)
	}
	return m, nil
}

func (m Model) startRun(requestID uint64, ctx context.Context, spec task.Spec) tea.Cmd {
	return func() tea.Msg {
		if m.manager == nil {
			return runAcceptedMsg{RequestID: requestID, Err: fmt.Errorf("task session is unavailable")}
		}
		run, err := m.manager.Start(ctx, spec)
		return runAcceptedMsg{RequestID: requestID, Run: run, Err: err}
	}
}

func (m Model) waitForRun(requestID uint64) tea.Cmd {
	run := m.run
	return func() tea.Msg {
		<-run.Done()
		return runFinishedMsg{RequestID: requestID, Result: run.Result()}
	}
}

func (m Model) refreshAfter(requestID uint64) tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(now time.Time) tea.Msg {
		return refreshMsg{RequestID: requestID, At: now}
	})
}

func (m *Model) refreshOutputIfChanged() {
	anchorID, hadAnchor := m.topVisibleOutputID()
	snapshot, changed := m.output.SnapshotIfChanged(m.outputRevision)
	if changed {
		m.outputRevision = snapshot.Revision
		m.outputRecords = snapshot.Records
		m.outputEvicted = snapshot.EvictedBytes > 0 || snapshot.EvictedRecords > 0
	}
	width := m.outputContentWidth()
	if !changed && m.outputRendered && m.outputRenderedWidth == width {
		return
	}

	var content strings.Builder
	for i, record := range m.outputRecords {
		if i > 0 {
			content.WriteByte('\n')
		}
		content.WriteString(clipDisplayText(record.Text, width))
	}
	m.outputView.SetContent(content.String())
	m.outputRendered = true
	m.outputRenderedWidth = width
	if m.followOutput {
		m.outputView.GotoBottom()
		return
	}
	if !hadAnchor {
		m.outputView.SetYOffset(0)
		return
	}
	for i, record := range m.outputRecords {
		if record.ID == anchorID {
			m.outputView.SetYOffset(i)
			return
		}
	}
	m.outputView.SetYOffset(0)
	if len(m.outputRecords) > 0 {
		m.outputEvicted = true
	}
}

func (m Model) topVisibleOutputID() (uint64, bool) {
	if m.followOutput || m.outputView.YOffset() < 0 || m.outputView.YOffset() >= len(m.outputRecords) {
		return 0, false
	}
	return m.outputRecords[m.outputView.YOffset()].ID, true
}

func (m *Model) recordStartFailure(err error) {
	result := runner.Result{TaskName: m.outputTask, Status: runner.StartFailed, ExitCode: -1, Err: err, FinishedAt: time.Now()}
	m.lastResults[m.outputTask] = result.Status
	_, _ = fmt.Fprintln(m.output.StderrWriter(), err)
	m.output.Finish()
	m.refreshOutputIfChanged()
	m.phase = Idle
	if m.requestCancel != nil {
		m.requestCancel(err)
	}
	m.requestCancel = nil
}

func (m Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key == "ctrl+c" {
		reason, _ := m.shutdown.Request(runner.StopSIGINT)
		if m.phase == Idle {
			return m, tea.Quit
		}
		return m.requestShutdown(reason)
	}
	if m.confirmQuit {
		switch key {
		case "esc", "n", "q":
			m.confirmQuit = false
		case "enter", "y":
			m.confirmQuit = false
			reason, _ := m.shutdown.Request(runner.StopShutdown)
			return m.requestShutdown(reason)
		}
		return m, nil
	}
	if m.showHelp {
		if key == "esc" || key == "?" {
			m.showHelp = !m.showHelp
		}
		return m, nil
	}

	if key == "?" {
		m.showHelp = true
		return m, nil
	}
	if key == "q" {
		if m.phase == Idle {
			m.shutdown.Request(runner.StopShutdown)
			return m, tea.Quit
		}
		m.confirmQuit = true
		return m, nil
	}
	if key == "c" && m.phase != Idle {
		m.phase = Stopping
		m.cancelRun(runner.ErrUserCancel)
		return m, nil
	}
	switch key {
	case "tab":
		if m.focus == Tasks {
			m.focus = Output
		} else {
			m.focus = Tasks
		}
	case "enter":
		if m.focus == Tasks && m.phase == Idle && len(m.tasks) != 0 {
			m.requestID++
			m.phase = Launching
			m.requestedAt = time.Now()
			m.outputTask = m.tasks[m.selected].Name
			m.output = output.NewStore(output.DefaultLimits())
			m.outputRevision = 0
			m.outputRecords = nil
			m.outputRendered = false
			m.outputEvicted = false
			m.outputView.SetContent("")
			ctx, cancel := context.WithCancelCause(m.appCtx)
			m.requestCancel = cancel
			m.followOutput = true
			id := m.requestID
			spec := cloneTask(m.tasks[m.selected])
			return m, m.startRun(id, ctx, spec)
		}
	case "up", "k":
		if m.focus == Tasks {
			m.moveSelection(-1)
		} else {
			m.scrollOutput(-1)
		}
	case "down", "j":
		if m.focus == Tasks {
			m.moveSelection(1)
		} else {
			m.scrollOutput(1)
		}
	case "pgup":
		if m.focus == Output {
			m.outputView.PageUp()
			m.followOutput = false
		}
	case "pgdown":
		if m.focus == Output {
			m.outputView.PageDown()
			m.followOutput = m.outputView.AtBottom()
		}
	case "end":
		if m.focus == Output {
			m.followOutput = true
			m.outputView.GotoBottom()
		}
	}
	return m, nil
}

func (m *Model) cancelRun(cause error) {
	if m.requestCancel != nil {
		m.requestCancel(cause)
	}
	if m.run != nil {
		m.run.Cancel(cause)
	}
}

func (m Model) requestShutdown(reason runner.StopReason) (tea.Model, tea.Cmd) {
	if m.phase == Idle {
		return m, tea.Quit
	}
	m.exitAfterRun = true
	m.confirmQuit = false
	m.phase = Stopping
	m.cancelRun(stopCause(reason))
	return m, nil
}

func stopCause(reason runner.StopReason) error {
	switch reason {
	case runner.StopSIGINT:
		return runner.ErrSIGINT
	case runner.StopSIGTERM:
		return runner.ErrSIGTERM
	default:
		return runner.ErrShutdown
	}
}

func cloneTask(spec task.Spec) task.Spec {
	spec.Argv = append([]string(nil), spec.Argv...)
	if spec.Env != nil {
		env := make(map[string]string, len(spec.Env))
		for key, value := range spec.Env {
			env[key] = value
		}
		spec.Env = env
	}
	return spec
}

func (m *Model) moveSelection(delta int) {
	if len(m.tasks) == 0 {
		return
	}
	m.selected = (m.selected + delta + len(m.tasks)) % len(m.tasks)
}

func (m *Model) scrollOutput(delta int) {
	if delta < 0 {
		m.outputView.ScrollUp(1)
	} else {
		m.outputView.ScrollDown(1)
	}
	m.followOutput = m.outputView.AtBottom()
}

func (m *Model) resizeOutput() {
	width, height := m.outputDimensions()
	m.outputView.SetWidth(width)
	m.outputView.SetHeight(height)
	if m.followOutput {
		m.outputView.GotoBottom()
	}
}

func (m Model) outputDimensions() (int, int) {
	mode := m.layout()
	if mode == compactLayout {
		return 1, 1
	}
	panelWidth := max(1, m.width)
	panelHeight := max(1, m.height-3)
	if mode == wideLayout || mode == mediumLayout {
		leftWidth := m.taskPanelWidth(mode, panelWidth)
		panelWidth = max(1, panelWidth-leftWidth-1)
	}
	if mode == stackedLayout {
		_, panelHeight = m.stackedPanelHeights()
	}
	return max(1, panelWidth-4), max(1, panelHeight-3)
}

func (m Model) outputContentWidth() int {
	width, _ := m.outputDimensions()
	return width
}

func (m Model) stackedPanelHeights() (int, int) {
	available := max(8, m.height-3)
	taskHeight := max(4, min(8, available/2))
	return taskHeight, max(4, available-taskHeight)
}

func (m Model) layout() layoutMode {
	if m.width < compactMinWidth || m.height < compactMinHeight {
		return compactLayout
	}
	if m.height < wideMinHeight || m.width < mediumMinWidth {
		return stackedLayout
	}
	if m.width < wideMinWidth {
		return mediumLayout
	}
	return wideLayout
}

func (m Model) taskPanelWidth(mode layoutMode, width int) int {
	if mode == mediumLayout {
		return max(16, (width-1)/4)
	}
	return max(22, min(30, width/3))
}

// View returns a full-screen v2 view.
func (m Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}

func (m Model) render() string {
	mode := m.layout()
	width := max(0, m.width)
	if mode == compactLayout {
		return m.renderCompact(width, max(0, m.height))
	}
	if mode == stackedLayout {
		return m.renderStacked(width, m.height)
	}
	return m.renderSideBySide(mode, width, m.height)
}

func (m Model) renderSideBySide(mode layoutMode, width, height int) string {
	leftWidth := m.taskPanelWidth(mode, width)
	rightWidth := max(1, width-leftWidth-1)
	panelHeight := max(4, height-3)
	taskPanel := m.panel("TASKS", m.renderTasks(leftWidth-4, panelHeight-3), leftWidth, panelHeight, m.focus == Tasks)
	outputPanel := m.panel("OUTPUT · "+m.outputLabel(), m.outputContent(), rightWidth, panelHeight, m.focus == Output)
	header := clipDisplayText(m.project+"  ·  "+m.phaseName(), width)
	details := clipDisplayText(m.renderDetails(), width)
	return strings.Join([]string{
		lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7dd3fc")).Render(header),
		lipgloss.JoinHorizontal(lipgloss.Top, taskPanel, " ", outputPanel),
		details,
		m.footerOrPrompt(width),
	}, "\n")
}

func (m Model) renderStacked(width, height int) string {
	taskHeight, outputHeight := m.stackedPanelHeights()
	taskPanel := m.panel("TASKS", m.renderTasks(width-4, taskHeight-3), width, taskHeight, m.focus == Tasks)
	outputPanel := m.panel("OUTPUT · "+m.outputLabel(), m.outputContent(), width, outputHeight, m.focus == Output)
	header := lipgloss.NewStyle().Bold(true).Render(clipDisplayText(m.project+" · "+m.phaseName(), width))
	return strings.Join([]string{
		header,
		taskPanel,
		clipDisplayText(m.renderDetails(), width),
		outputPanel,
		m.footerOrPrompt(width),
	}, "\n")
}

func (m Model) renderCompact(width, height int) string {
	if width < 1 {
		width = 24
	}
	status := m.project + " · " + m.phaseName()
	if m.outputEvicted {
		status = "Older output discarded · " + m.phaseName()
	}
	lines := []string{
		status,
		"Resize to ≥24×13 · q quit · c cancel",
	}
	if m.confirmQuit {
		lines[1] = "Stop task and quit? y/Enter · n/Esc"
	} else if m.showHelp {
		lines[1] = "↑/↓ select · Tab focus · Enter run · q quit · c cancel"
	}
	if height == 1 {
		switch {
		case m.confirmQuit:
			lines = []string{"quit? y/Enter · n/Esc"}
		case m.outputEvicted:
			lines[0] = "Older output discarded · q/c"
		default:
			lines = []string{m.phaseName() + " · resize ≥24×13"}
		}
	}
	for i := range lines {
		lines[i] = clipDisplayText(lines[i], width)
	}
	if height > 0 && len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

func (m Model) footerOrPrompt(width int) string {
	if !m.showHelp && !m.confirmQuit {
		return clipDisplayText(m.renderFooter(), width)
	}
	line := ""
	if m.showHelp {
		line = "↑/↓ or j/k navigate · Tab focus · Enter run · c stop · q quit · ? close"
	} else {
		line = "Stop active task and quit? [y/Enter] yes · [n/Esc] stay"
	}
	if m.outputEvicted {
		line = "Older output discarded · " + line
	}
	return clipDisplayText(line, width)
}

func (m Model) panel(title, content string, width, height int, focused bool) string {
	width = max(1, width)
	height = max(4, height)
	title = clipDisplayText(title, width)
	color := lipgloss.Color("#475569")
	if focused {
		color = lipgloss.Color("#38bdf8")
	}
	style := lipgloss.NewStyle().Width(width).Height(height-1).Border(lipgloss.RoundedBorder()).BorderForeground(color).Padding(0, 1)
	return lipgloss.NewStyle().Render(title) + "\n" + style.Render(content)
}

func (m Model) renderTasks(width, height int) string {
	if len(m.tasks) == 0 {
		return "No tasks configured"
	}
	rows := max(1, height)
	start := max(0, m.selected-rows/2)
	if start+rows > len(m.tasks) {
		start = max(0, len(m.tasks)-rows)
	}
	end := min(len(m.tasks), start+rows)
	lines := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		spec := m.tasks[i]
		marker := "  "
		if i == m.selected {
			marker = "> "
		}
		line := marker + safeDisplayText(spec.Name)
		if result, ok := m.lastResults[spec.Name]; ok {
			status := " " + resultLabel(result)
			if statusWidth := ansi.StringWidth(status); statusWidth < width {
				line = ansi.Truncate(line, width-statusWidth, "…") + status
			} else {
				line = ansi.Truncate(resultLabel(result), width, "…")
			}
		} else {
			line = ansi.Truncate(line, max(1, width), "…")
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func (m Model) renderDetails() string {
	if len(m.tasks) == 0 {
		return "No task selected"
	}
	spec := m.tasks[m.selected]
	description := spec.Description
	if description == "" {
		description = "No description"
	}
	directory := spec.Dir
	if directory == "" {
		directory = "."
	}
	return fmt.Sprintf("%s — %s  ·  %s", safeDisplayText(spec.Name), safeDisplayText(description), safeDisplayText(directory))
}

func (m Model) outputContent() string {
	if m.outputTask == "" {
		return "No task output yet"
	}
	return m.outputView.View()
}

func (m Model) outputLabel() string {
	if m.outputTask == "" {
		return "no run"
	}
	return m.outputTask
}

func (m Model) renderFooter() string {
	state := ""
	if m.phase != Idle {
		state = "  ·  elapsed " + time.Since(m.requestedAt).Round(time.Second).String()
	}
	following := "paused"
	if m.followOutput {
		following = "following"
	}
	if m.outputEvicted {
		return fmt.Sprintf("Older output discarded · %s · c stop · q quit%s", following, state)
	}
	return fmt.Sprintf("Focus: %s  ·  ↑/↓ or j/k  ·  Tab focus  ·  Enter run  ·  c stop  ·  q quit  ·  %s · retained%s", focusName(m.focus), following, state)
}

func safeDisplayText(text string) string {
	text = ansi.Strip(strings.ToValidUTF8(text, "�"))
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
}

func clipDisplayText(text string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(safeDisplayText(text), width, "…")
}

func (m Model) phaseName() string {
	switch m.phase {
	case Launching:
		return "launching"
	case Active:
		return "active"
	case Stopping:
		return "Stopping…"
	default:
		return "idle"
	}
}

func focusName(focus Focus) string {
	if focus == Output {
		return "output"
	}
	return "tasks"
}

func resultLabel(status runner.Status) string {
	switch status {
	case runner.Succeeded:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#22c55e")).Render("OK")
	case runner.Failed, runner.StartFailed, runner.RunnerError:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#ef4444")).Render("FAILED")
	case runner.Cancelled:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#fbbf24")).Render("CANCELLED")
	case runner.TimedOut:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#f97316")).Render("TIMEOUT")
	default:
		return string(status)
	}
}

func slicesSortTasks(tasks []task.Spec) {
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Name < tasks[j].Name })
}
