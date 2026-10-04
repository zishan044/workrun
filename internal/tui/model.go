// Package tui contains the interactive task interface.
package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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

// Model is the Bubble Tea model for the task screen.
type Model struct {
	project       string
	tasks         []task.Spec
	manager       *session.Manager
	appCtx        context.Context
	shutdown      *ShutdownState
	selected      int
	phase         Phase
	focus         Focus
	requestID     uint64
	requestedAt   time.Time
	requestCancel context.CancelCauseFunc
	run           *session.Run
	outputRevision  uint64
	outputTruncated bool
	lastResults   map[string]runner.Result
	outputTask    string
	output        *output.Store
	outputView    viewport.Model
	followOutput  bool
	showHelp      bool
	confirmQuit   bool
	exitAfterRun  bool
	width, height int
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
		lastResults:  make(map[string]runner.Result),
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
		m.lastResults[result.TaskName] = result
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
	snapshot, changed := m.output.SnapshotIfChanged(m.outputRevision)
	if !changed {
		return
	}
	m.outputRevision = snapshot.Revision
	m.outputTruncated = snapshot.EvictedBytes > 0 || snapshot.EvictedRecords > 0
	var lines []string
	for _, record := range snapshot.Records {
		lines = append(lines, record.Text)
	}
	m.outputView.SetContent(strings.Join(lines, "\n"))
	if m.followOutput {
		m.outputView.GotoBottom()
	}
}

func (m *Model) recordStartFailure(err error) {
	result := runner.Result{TaskName: m.outputTask, Status: runner.StartFailed, ExitCode: -1, Err: err, FinishedAt: time.Now()}
	m.lastResults[m.outputTask] = result
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
			m.outputTruncated = false
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
	width := max(1, m.width-6)
	if m.width >= 76 {
		width = max(1, (m.width-8)/2)
	}
	m.outputView.SetWidth(width)
	m.outputView.SetHeight(max(1, m.height-12))
}

// View returns a full-screen v2 view.
func (m Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}

func (m Model) render() string {
	width := m.width
	if width < 24 {
		width = 24
	}
	if m.height > 0 && (m.width < 76 || m.height < 16) {
		return m.renderNarrow(width)
	}
	if m.height == 0 {
		return m.renderNarrow(width)
	}
	leftWidth := max(22, (width-8)/3)
	rightWidth := max(24, width-leftWidth-6)
	height := max(3, m.height-10)
	left := m.panel("TASKS", m.renderTasks(), leftWidth, height, m.focus == Tasks)
	content := m.outputContent()
	right := m.panel("OUTPUT · "+m.outputLabel(), content, rightWidth, height, m.focus == Output)
	header := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7dd3fc")).Render(m.project + "  ·  " + m.phaseName())
	details := m.renderDetails()
	footer := m.renderFooter()
	view := strings.Join([]string{header, lipgloss.JoinHorizontal(lipgloss.Top, left, right), details, footer}, "\n")
	if m.showHelp {
		return view + "\n" + lipgloss.NewStyle().Bold(true).Render("Help: ↑/↓ or j/k select/scroll · Tab focus · Enter run · c stop · q quit · ? close")
	}
	if m.confirmQuit {
		return view + "\n" + lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#fbbf24")).Render("Stop the active task and quit? [y/Enter] yes · [n/Esc] stay")
	}
	return view
}

func (m Model) renderNarrow(width int) string {
	if m.height > 0 && m.height < 8 {
		return lipgloss.NewStyle().Width(width).Render(m.project + " · " + m.phaseName() + "\n" + m.renderFooter())
	}
	taskBox := m.panel("TASKS", m.renderTasks(), width, max(3, min(8, len(m.tasks)+2)), m.focus == Tasks)
	outputBox := m.panel("OUTPUT · "+m.outputLabel(), m.outputContent(), width, max(3, min(8, m.height-12)), m.focus == Output)
	view := strings.Join([]string{
		lipgloss.NewStyle().Bold(true).Render(m.project + " · " + m.phaseName()),
		taskBox,
		m.renderDetails(),
		outputBox,
		m.renderFooter(),
	}, "\n")
	if m.showHelp {
		view += "\n↑/↓ or j/k select/scroll · Tab focus · Enter run · c stop · q quit · Esc close help"
	}
	if m.confirmQuit {
		view += "\nStop the active task and quit? [y/Enter] yes · [n/Esc] stay"
	}
	return view
}

func (m Model) panel(title, content string, width, height int, focused bool) string {
	color := lipgloss.Color("#475569")
	if focused {
		color = lipgloss.Color("#38bdf8")
	}
	style := lipgloss.NewStyle().Width(max(1, width-4)).Height(max(1, height-2)).Border(lipgloss.RoundedBorder()).BorderForeground(color).Padding(0, 1)
	return lipgloss.NewStyle().Render(title) + "\n" + style.Render(content)
}

func (m Model) renderTasks() string {
	lines := make([]string, 0, len(m.tasks))
	for i, spec := range m.tasks {
		marker := "  "
		if i == m.selected {
			marker = "> "
		}
		line := marker + spec.Name
		if result, ok := m.lastResults[spec.Name]; ok {
			line += "  " + resultLabel(result.Status)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "No tasks configured"
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
	return fmt.Sprintf("%s — %s  ·  %s", spec.Name, description, directory)
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
	truncation := "retained"
	if m.outputTruncated {
		truncation = "truncated"
	}
	return fmt.Sprintf("Focus: %s  ·  ↑/↓ or j/k  ·  Tab focus  ·  Enter run  ·  c stop  ·  q quit  ·  %s · %s%s", focusName(m.focus), following, truncation, state)
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
