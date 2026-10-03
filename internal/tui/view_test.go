package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/zishan044/workrun/internal/runner"
)

func TestViewUsesAltScreenAndNarrowFallback(t *testing.T) {
	m := testModel()
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 42, Height: 14})
	m = updated.(Model)
	view := m.View()
	if !view.AltScreen {
		t.Fatal("TUI view did not enable the alternate screen")
	}
	for _, want := range []string{"example", "TASKS", "alpha", "OUTPUT", "Focus:"} {
		if !strings.Contains(view.Content, want) {
			t.Errorf("narrow view missing %q: %q", want, view.Content)
		}
	}
}

func TestViewShowsTextStatusLabel(t *testing.T) {
	m := testModel()
	m.lastResults["alpha"] = runner.Result{TaskName: "alpha", Status: runner.TimedOut}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(Model)
	if got := m.View().Content; !strings.Contains(got, "TIMEOUT") {
		t.Fatalf("view missing timeout text label: %q", got)
	}
}
