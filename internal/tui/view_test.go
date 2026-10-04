package tui

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
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
	m.lastResults["alpha"] = runner.TimedOut
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(Model)
	if got := m.View().Content; !strings.Contains(got, "TIMEOUT") {
		t.Fatalf("view missing timeout text label: %q", got)
	}
}

func TestLayoutBoundaries(t *testing.T) {
	for _, test := range []struct {
		width, height int
		want          layoutMode
	}{
		{76, 16, wideLayout},
		{75, 16, mediumLayout},
		{52, 16, mediumLayout},
		{51, 16, stackedLayout},
		{24, 13, stackedLayout},
		{23, 13, compactLayout},
		{76, 15, stackedLayout},
		{24, 12, compactLayout},
	} {
		m := testModel()
		updated, _ := m.Update(tea.WindowSizeMsg{Width: test.width, Height: test.height})
		m = updated.(Model)
		if got := m.layout(); got != test.want {
			t.Errorf("layout at %dx%d = %v, want %v", test.width, test.height, got, test.want)
		}
		if got, _ := m.outputDimensions(); got < 1 {
			t.Errorf("output width at %dx%d = %d, want positive", test.width, test.height, got)
		}
		if _, got := m.outputDimensions(); got < 1 {
			t.Errorf("output height at %dx%d = %d, want positive", test.width, test.height, got)
		}
	}
}

func TestViewFitsSelectedTerminalBoundaries(t *testing.T) {
	for _, size := range [][2]int{{76, 16}, {75, 16}, {52, 16}, {51, 16}, {24, 13}, {23, 13}, {76, 15}, {24, 12}} {
		m := testModel()
		updated, _ := m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m = updated.(Model)
		content := m.View().Content
		for _, line := range strings.Split(content, "\n") {
			if got := ansi.StringWidth(line); got > size[0] {
				t.Errorf("line width %d exceeds %dx%d: %q", got, size[0], size[1], line)
			}
		}
		if len(strings.Split(content, "\n")) > size[1] {
			t.Errorf("view has more rows than terminal at %dx%d: %q", size[0], size[1], content)
		}
	}
}

func TestDisplayTextIsSanitizedAndClippedByCellWidth(t *testing.T) {
	unsafe := "\x1b]0;hostile title\x07alpha\r\n\x01"
	clean := safeDisplayText(unsafe)
	if strings.ContainsAny(clean, "\x1b\r\n\x01") || !strings.Contains(clean, "alpha") {
		t.Fatalf("unsafe display text was not sanitized: %q", clean)
	}

	got := clipDisplayText("界e\u0301-tail", 4)
	if !utf8.ValidString(got) {
		t.Fatalf("truncation produced invalid UTF-8: %q", got)
	}
	if width := ansi.StringWidth(got); width > 4 || !strings.Contains(got, "…") {
		t.Fatalf("cell-width truncation = %q (width %d), want marker within 4 cells", got, width)
	}
}

func TestViewSanitizesConfigDisplayFields(t *testing.T) {
	m := testModel()
	m.project = "\x1b[2Jproject"
	m.tasks[0].Name = "\x1b[31malpha"
	m.tasks[0].Description = "safe\x1b]52;c;secret\x07\ntext"
	m.tasks[0].Dir = "/tmp/\x1b[31mhostile"
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	view := updated.(Model).View().Content
	plain := ansi.Strip(view)
	for _, forbidden := range []string{"\x1b", "secret", "\ntext", "[2J"} {
		if strings.Contains(plain, forbidden) {
			t.Errorf("view contains unsanitized config text %q: %q", forbidden, plain)
		}
	}
}
