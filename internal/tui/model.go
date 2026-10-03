// Package tui contains the static, simulated task interface. It does not run
// task processes; a later milestone can connect its run messages to session.
package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/zishan044/workrun/internal/output"
	"github.com/zishan044/workrun/internal/runner"
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

// TickMsg updates elapsed time for an accepted simulated run.
type TickMsg struct {
	RequestID uint64
	At        time.Time
}

// CompletionMsg supplies a simulated task result. It is ignored if stale.
type CompletionMsg struct {
	RequestID uint64
	Result    runner.Result
}

// Model is the Bubble Tea model for the task screen.
type Model struct {
	project       string
	tasks         []task.Spec
	selected      int
	phase         Phase
	focus         Focus
	requestID     uint64
	requestedAt   time.Time
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
func NewModel(project string, tasks []task.Spec) Model {
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
		lastResults:  make(map[string]runner.Result),
		output:       output.NewStore(output.DefaultLimits()),
		outputView:   viewport.New(),
		followOutput: true,
	}
}

// Init satisfies tea.Model. The simulation starts only in response to Enter.
func (m Model) Init() tea.Cmd { return nil }

// Update handles model input and synthetic run messages.
func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resizeOutput()
	case TickMsg:
		if msg.RequestID == m.requestID && m.phase == Active {
			return m, tea.Tick(time.Second, func(now time.Time) tea.Msg {
				return TickMsg{RequestID: msg.RequestID, At: now}
			})
		}
	case CompletionMsg:
		if msg.RequestID != m.requestID || m.phase == Idle {
			break
		}
		result := msg.Result
		if m.phase == Stopping {
			result.Status = runner.Cancelled
			result.StopReason = runner.StopUser
		}
		result.TaskName = m.outputTask
		if result.RequestedAt.IsZero() {
			result.RequestedAt = m.requestedAt
		}
		m.lastResults[result.TaskName] = result
		_, _ = m.output.StdoutWriter().Write([]byte("Simulated result: " + string(result.Status) + "\n"))
		m.output.Finish()
		m.refreshOutput()
		m.phase = Idle
		m.confirmQuit = false
		if m.exitAfterRun {
			return m, tea.Quit
		}
	case tea.KeyPressMsg:
		return m.updateKey(msg)
	}
	return m, nil
}

func (m Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if m.confirmQuit {
		switch key {
		case "esc", "n", "q":
			m.confirmQuit = false
		case "enter", "y":
			m.confirmQuit = false
			m.exitAfterRun = true
			m.phase = Stopping
			return m, nil // The synthetic run completion represents cleanup.
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
	if key == "ctrl+c" {
		if m.phase == Idle {
			return m, tea.Quit
		}
		m.exitAfterRun = true
		m.phase = Stopping
		return m, nil
	}
	if key == "q" {
		if m.phase == Idle {
			return m, tea.Quit
		}
		m.confirmQuit = true
		return m, nil
	}
	if key == "c" && m.phase != Idle {
		m.phase = Stopping
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
			m.phase = Active
			m.requestedAt = time.Now()
			m.outputTask = m.tasks[m.selected].Name
			m.output = output.NewStore(output.DefaultLimits())
			_, _ = m.output.StdoutWriter().Write([]byte("Simulated run for " + m.outputTask + "\n"))
			m.followOutput = true
			m.refreshOutput()
			id := m.requestID
			return m, tea.Batch(
				tea.Tick(time.Second, func(now time.Time) tea.Msg { return TickMsg{RequestID: id, At: now} }),
				tea.Tick(3*time.Second, func(now time.Time) tea.Msg {
					return CompletionMsg{RequestID: id, Result: runner.Result{TaskName: m.outputTask, Status: runner.Succeeded, RequestedAt: m.requestedAt, FinishedAt: now}}
				}),
			)
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

func (m *Model) refreshOutput() {
	snapshot := m.output.Snapshot()
	var lines []string
	for _, record := range snapshot.Records {
		lines = append(lines, record.Text)
	}
	m.outputView.SetContent(strings.Join(lines, "\n"))
	if m.followOutput {
		m.outputView.GotoBottom()
	}
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
	if snapshot := m.output.Snapshot(); snapshot.EvictedBytes > 0 || snapshot.EvictedRecords > 0 {
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
		return "stopping"
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
