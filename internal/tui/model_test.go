package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/zishan044/workrun/internal/runner"
	"github.com/zishan044/workrun/internal/task"
)

func testModel() Model {
	return NewModel("example", []task.Spec{
		{Name: "zeta", Description: "last", Dir: "/tmp/z"},
		{Name: "alpha", Description: "first", Dir: "/tmp/a"},
		{Name: "middle", Description: "middle", Dir: "/tmp/m"},
	})
}

func press(key string) tea.KeyPressMsg {
	switch key {
	case "enter":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter})
	case "tab":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyTab})
	case "esc":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc})
	case "up":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyUp})
	case "down":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyDown})
	case "ctrl+c":
		return tea.KeyPressMsg(tea.Key{Code: 'c', Mod: tea.ModCtrl})
	default:
		return tea.KeyPressMsg(tea.Key{Code: []rune(key)[0], Text: key})
	}
}

func updateKey(m Model, key string) (Model, tea.Cmd) {
	updated, cmd := m.Update(press(key))
	return updated.(Model), cmd
}

func TestTasksAreSortedAndFocusRoutesNavigation(t *testing.T) {
	m := testModel()
	if got := []string{m.tasks[0].Name, m.tasks[1].Name, m.tasks[2].Name}; strings.Join(got, ",") != "alpha,middle,zeta" {
		t.Fatalf("task order = %v", got)
	}
	m, _ = updateKey(m, "down")
	if m.selected != 1 {
		t.Fatalf("selected task index = %d, want 1", m.selected)
	}
	m, _ = updateKey(m, "tab")
	m, _ = updateKey(m, "down")
	if m.selected != 1 || m.focus != Output {
		t.Fatalf("output navigation changed selection or focus: selected=%d focus=%d", m.selected, m.focus)
	}
	m, _ = updateKey(m, "tab")
	m, _ = updateKey(m, "k")
	if m.selected != 0 {
		t.Fatalf("selection after returning to task focus = %d, want 0", m.selected)
	}
}

func TestRunOutputKeepsItsTaskLabelWhenSelectionMoves(t *testing.T) {
	m := testModel()
	var cmd tea.Cmd
	m, cmd = updateKey(m, "enter")
	if cmd == nil || m.phase != Active || m.outputTask != "alpha" {
		t.Fatalf("Enter did not start simulated alpha run: phase=%d outputTask=%q", m.phase, m.outputTask)
	}
	m, _ = updateKey(m, "down")
	if m.selected != 1 || m.outputLabel() != "alpha" {
		t.Fatalf("selection relabeled output: selected=%d output=%q", m.selected, m.outputLabel())
	}
	_, _ = m.Update(CompletionMsg{RequestID: m.requestID, Result: runner.Result{Status: runner.Succeeded}})
	if got := m.lastResults["alpha"].Status; got != runner.Succeeded {
		t.Fatalf("simulated result status = %q, want %q", got, runner.Succeeded)
	}
	if !strings.Contains(m.outputContent(), "Simulated result: succeeded") {
		t.Fatalf("output missing synthetic completion: %q", m.outputContent())
	}
}

func TestHelpAndQuitConfirmationConsumeKeys(t *testing.T) {
	m := testModel()
	m, _ = updateKey(m, "?")
	m, _ = updateKey(m, "enter")
	if !m.showHelp || m.phase != Idle {
		t.Fatalf("Enter escaped help or started a task: help=%v phase=%d", m.showHelp, m.phase)
	}
	m, _ = updateKey(m, "esc")
	m, _ = updateKey(m, "enter")
	if m.phase != Active {
		t.Fatalf("Enter did not accept simulated run: phase=%d", m.phase)
	}
	m, _ = updateKey(m, "q")
	m, _ = updateKey(m, "enter")
	if m.confirmQuit || m.phase != Stopping || !m.exitAfterRun || m.selected != 0 {
		t.Fatalf("confirmation Enter was not handled as quit: confirm=%v phase=%d exit=%v selected=%d", m.confirmQuit, m.phase, m.exitAfterRun, m.selected)
	}
	_, cmd := m.Update(CompletionMsg{RequestID: m.requestID, Result: runner.Result{Status: runner.Succeeded}})
	if cmd == nil || m.lastResults["alpha"].Status != runner.Cancelled {
		t.Fatalf("quit completion did not cancel and exit: command=%v result=%q", cmd != nil, m.lastResults["alpha"].Status)
	}
}

func TestCancelLeavesTUIOpenAndCtrlCExitsAfterCompletion(t *testing.T) {
	m := testModel()
	m, _ = updateKey(m, "enter")
	m, _ = updateKey(m, "c")
	if m.phase != Stopping || m.exitAfterRun {
		t.Fatalf("ordinary cancel state = phase %d exit %v", m.phase, m.exitAfterRun)
	}
	_, cmd := m.Update(CompletionMsg{RequestID: m.requestID, Result: runner.Result{Status: runner.Succeeded}})
	if cmd != nil || m.phase != Idle || m.lastResults["alpha"].Status != runner.Cancelled {
		t.Fatalf("ordinary cancel did not leave TUI open with cancelled result")
	}
	m, _ = updateKey(m, "enter")
	m, _ = updateKey(m, "ctrl+c")
	if !m.exitAfterRun || m.phase != Stopping {
		t.Fatalf("Ctrl+C did not defer exit until run completion")
	}
	_, cmd = m.Update(CompletionMsg{RequestID: m.requestID, Result: runner.Result{Status: runner.Succeeded}})
	if cmd == nil {
		t.Fatal("Ctrl+C completion did not return a quit command")
	}
}

func TestStaleSimulationMessagesAreIgnored(t *testing.T) {
	m := testModel()
	m, _ = updateKey(m, "enter")
	id := m.requestID
	m, _ = updateKey(m, "c")
	m, _ = updateKey(m, "enter") // ignored while stopping
	updated, cmd := m.Update(CompletionMsg{RequestID: id + 1, Result: runner.Result{TaskName: "alpha", Status: runner.Failed}})
	m = updated.(Model)
	if cmd != nil || m.phase != Stopping {
		t.Fatalf("stale completion changed active state: phase=%d", m.phase)
	}
	updated, _ = m.Update(TickMsg{RequestID: id, At: time.Now()})
	if updated.(Model).phase != Stopping {
		t.Fatal("tick changed stopping state")
	}
}
