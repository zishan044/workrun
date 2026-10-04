package tui

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/zishan044/workrun/internal/output"
	"github.com/zishan044/workrun/internal/runner"
	"github.com/zishan044/workrun/internal/session"
	"github.com/zishan044/workrun/internal/task"
)

type modelRunner struct {
	started   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
}

func newModelRunner() *modelRunner {
	return &modelRunner{started: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
}

func (r *modelRunner) Run(ctx context.Context, spec task.Spec, stdout, _ io.Writer) runner.Result {
	_, _ = io.WriteString(stdout, "live output\n")
	close(r.started)
	result := runner.Result{TaskName: spec.Name, ExitCode: 0, ProcessStarted: true}
	select {
	case <-ctx.Done():
		close(r.cancelled)
		result.Status = runner.Cancelled
		result.StopReason = runner.StopUser
		switch {
		case errors.Is(context.Cause(ctx), runner.ErrSIGINT):
			result.StopReason = runner.StopSIGINT
		case errors.Is(context.Cause(ctx), runner.ErrSIGTERM):
			result.StopReason = runner.StopSIGTERM
		case errors.Is(context.Cause(ctx), runner.ErrShutdown):
			result.StopReason = runner.StopShutdown
		}
	case <-r.release:
		result.Status = runner.Succeeded
	}
	_, _ = io.WriteString(stdout, "final partial output")
	return result
}

func newTestModel() (Model, *modelRunner) {
	r := newModelRunner()
	manager := session.NewManager(r)
	return NewModel("example", []task.Spec{
		{Name: "zeta", Description: "last", Dir: "/tmp/z"},
		{Name: "alpha", Description: "first", Argv: []string{"true"}, Dir: "/tmp/a"},
		{Name: "middle", Description: "middle", Dir: "/tmp/m"},
	}, manager, context.Background(), &ShutdownState{}), r
}

func testModel() Model {
	m, _ := newTestModel()
	return m
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
	case "end":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyEnd})
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

func waitFor(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for task runner")
	}
}

func startModelRun(t *testing.T, m Model) (Model, *session.Run) {
	t.Helper()
	m, cmd := updateKey(m, "enter")
	if cmd == nil || m.phase != Launching {
		t.Fatalf("Enter did not enter launching state: phase=%d cmd=%v", m.phase, cmd != nil)
	}
	accepted := cmd().(runAcceptedMsg)
	updated, followup := m.Update(accepted)
	m = updated.(Model)
	if followup == nil || m.phase != Active || m.run == nil {
		t.Fatalf("run was not accepted: phase=%d run=%v", m.phase, m.run != nil)
	}
	return m, m.run
}

func TestTasksAreSortedAndFocusRoutesNavigation(t *testing.T) {
	m, _ := newTestModel()
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

func TestEnterStartsRealSessionAndDisplaysFinalOutput(t *testing.T) {
	m, fake := newTestModel()
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(Model)
	m, run := startModelRun(t, m)
	waitFor(t, fake.started)
	if run.TaskName != "alpha" {
		t.Fatalf("started task = %q, want alpha", run.TaskName)
	}
	snapshot, _ := run.Output.SnapshotIfChanged(0)
	if len(snapshot.Records) == 0 {
		t.Fatal("runner output was not written to session store")
	}
	close(fake.release)
	waitFor(t, run.Done())
	updated, cmd := m.Update(runFinishedMsg{RequestID: m.requestID, Result: run.Result()})
	m = updated.(Model)
	if cmd != nil || m.phase != Idle || m.lastResults["alpha"] != runner.Succeeded {
		t.Fatalf("completion state: phase=%d status=%q cmd=%v", m.phase, m.lastResults["alpha"], cmd != nil)
	}
	var visible strings.Builder
	snapshot, _ = m.output.SnapshotIfChanged(0)
	for _, record := range snapshot.Records {
		visible.WriteString(record.Text)
	}
	if got := visible.String(); !strings.Contains(got, "live output") || !strings.Contains(got, "final partial output") {
		t.Fatalf("final output missing from retained store: %q", got)
	}
	if got := m.outputView.View(); !strings.Contains(got, "final partial output") {
		t.Fatalf("final partial output missing from viewport: %q", got)
	}
}

func TestStartingAnotherRunClearsPriorViewportContent(t *testing.T) {
	m := testModel()
	m.outputTask = "previous"
	_, _ = io.WriteString(m.output.StdoutWriter(), "old output\n")
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(Model)
	if !strings.Contains(m.outputView.View(), "old output") {
		t.Fatal("test setup did not populate prior output")
	}
	m, cmd := updateKey(m, "enter")
	if cmd == nil || m.phase != Launching {
		t.Fatalf("Enter did not launch the next task: phase=%d", m.phase)
	}
	if strings.Contains(m.outputView.View(), "old output") {
		t.Fatalf("prior run output remained visible while launching: %q", m.outputView.View())
	}
}

func TestPausedOutputAnchorsByRecordAndClampsOnEviction(t *testing.T) {
	m := testModel()
	m.output = output.NewStore(output.Limits{MaxBytes: 128, MaxRecords: 4, MaxRecordBytes: 32})
	_, _ = io.WriteString(m.output.StdoutWriter(), "one\ntwo\nthree\nfour\n")
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 51, Height: 13})
	m = updated.(Model)
	m.followOutput = false
	m.outputView.SetYOffset(1)

	_, _ = io.WriteString(m.output.StdoutWriter(), "five\n")
	m.refreshOutputIfChanged()
	if got := m.outputView.YOffset(); got != 0 {
		t.Fatalf("anchor for retained record moved to offset %d, want 0", got)
	}
	if got := m.outputView.View(); !strings.Contains(got, "two") {
		t.Fatalf("viewport lost retained anchor record: %q", got)
	}

	_, _ = io.WriteString(m.output.StdoutWriter(), "six\n")
	m.refreshOutputIfChanged()
	if got := m.outputView.YOffset(); got != 0 {
		t.Fatalf("evicted anchor offset = %d, want oldest retained row", got)
	}
	if !m.outputEvicted || !strings.Contains(m.outputView.View(), "three") {
		t.Fatalf("eviction did not clamp to oldest retained output: evicted=%v view=%q", m.outputEvicted, m.outputView.View())
	}
}

func TestResizeReclipsRetainedOutputWithoutNewBytes(t *testing.T) {
	m := testModel()
	m.outputTask = "alpha"
	_, _ = io.WriteString(m.output.StdoutWriter(), strings.Repeat("x", 80)+"\n")
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(Model)
	wide := m.outputView.View()
	if !strings.Contains(wide, "…") {
		t.Fatalf("wide viewport did not mark clipped output: %q", wide)
	}

	updated, _ = m.Update(tea.WindowSizeMsg{Width: 52, Height: 16})
	m = updated.(Model)
	narrow := m.outputView.View()
	if !strings.Contains(narrow, "…") || len(narrow) >= len(wide) {
		t.Fatalf("resize did not reclip retained record: wide=%q narrow=%q", wide, narrow)
	}
	if m.outputRenderedWidth != m.outputContentWidth() {
		t.Fatalf("cached output width = %d, want %d", m.outputRenderedWidth, m.outputContentWidth())
	}
}

func TestEndResumesOutputFollowing(t *testing.T) {
	m := testModel()
	_, _ = io.WriteString(m.output.StdoutWriter(), "one\ntwo\nthree\nfour\n")
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 51, Height: 13})
	m = updated.(Model)
	m.focus = Output
	m.followOutput = false
	m.outputView.SetYOffset(0)
	m, _ = updateKey(m, "end")
	if !m.followOutput || !m.outputView.AtBottom() {
		t.Fatal("End did not resume following output")
	}
}

func TestTUIOutputFloodStaysBoundedAndCancelRemainsAvailable(t *testing.T) {
	m := testModel()
	m.output = output.NewStore(output.Limits{MaxBytes: 64, MaxRecords: 4, MaxRecordBytes: 16})
	_, _ = io.WriteString(m.output.StdoutWriter(), strings.Repeat("flood\n", 1000))
	snapshot, changed := m.output.SnapshotIfChanged(0)
	if !changed || snapshot.RetainedBytes > 64 || len(snapshot.Records) > 4 {
		t.Fatalf("flood exceeded output limits: changed=%v bytes=%d records=%d", changed, snapshot.RetainedBytes, len(snapshot.Records))
	}
	m.phase = Active
	m, _ = updateKey(m, "c")
	if m.phase != Stopping {
		t.Fatalf("cancel key was not handled after output flood: phase=%d", m.phase)
	}
}

func TestCompactLayoutKeepsQuitAndCancelKeysAvailable(t *testing.T) {
	m, fake := newTestModel()
	m, run := startModelRun(t, m)
	waitFor(t, fake.started)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 23, Height: 12})
	m = updated.(Model)
	if !strings.Contains(m.View().Content, "Resize") {
		t.Fatalf("compact view missing resize guidance: %q", m.View().Content)
	}
	m, _ = updateKey(m, "q")
	if !m.confirmQuit {
		t.Fatal("q did not open quit confirmation in compact layout")
	}
	m, _ = updateKey(m, "n")
	m, _ = updateKey(m, "c")
	if m.phase != Stopping {
		t.Fatal("c did not cancel in compact layout")
	}
	waitFor(t, fake.cancelled)
	waitFor(t, run.Done())
}

func TestCancelDuringLaunchCancelsAcceptedRun(t *testing.T) {
	m, fake := newTestModel()
	m, start := updateKey(m, "enter")
	m, _ = updateKey(m, "c")
	if m.phase != Stopping || m.exitAfterRun {
		t.Fatalf("cancel while launching state: phase=%d exit=%v", m.phase, m.exitAfterRun)
	}
	accepted := start().(runAcceptedMsg)
	updated, _ := m.Update(accepted)
	m = updated.(Model)
	waitFor(t, fake.cancelled)
	waitFor(t, m.run.Done())
	updated, cmd := m.Update(runFinishedMsg{RequestID: m.requestID, Result: m.run.Result()})
	m = updated.(Model)
	if cmd != nil || m.phase != Idle || m.lastResults["alpha"] != runner.Cancelled {
		t.Fatalf("launch cancellation did not finish as cancelled: phase=%d result=%q", m.phase, m.lastResults["alpha"])
	}
}

func TestCancelActiveRunLeavesTUIOpen(t *testing.T) {
	m, fake := newTestModel()
	m, run := startModelRun(t, m)
	waitFor(t, fake.started)
	m, _ = updateKey(m, "c")
	if m.phase != Stopping || m.exitAfterRun {
		t.Fatalf("active cancel state: phase=%d exit=%v", m.phase, m.exitAfterRun)
	}
	waitFor(t, fake.cancelled)
	waitFor(t, run.Done())
	updated, cmd := m.Update(runFinishedMsg{RequestID: m.requestID, Result: run.Result()})
	m = updated.(Model)
	if cmd != nil || m.phase != Idle || m.lastResults["alpha"] != runner.Cancelled {
		t.Fatalf("cancel completion state: phase=%d result=%q cmd=%v", m.phase, m.lastResults["alpha"], cmd != nil)
	}
}

func TestTaskCancellationDoesNotCancelApplicationContext(t *testing.T) {
	appCtx, cancelApp := context.WithCancelCause(context.Background())
	defer cancelApp(nil)
	fake := newModelRunner()
	manager := session.NewManager(fake)
	m := NewModel("example", []task.Spec{{Name: "alpha"}}, manager, appCtx, &ShutdownState{})
	m, run := startModelRun(t, m)
	waitFor(t, fake.started)
	m, _ = updateKey(m, "c")
	waitFor(t, run.Done())
	if appCtx.Err() != nil {
		t.Fatalf("task cancellation ended application context: %v", appCtx.Err())
	}
}

func TestQuitConfirmationWaitsForActiveCleanup(t *testing.T) {
	m, fake := newTestModel()
	m, run := startModelRun(t, m)
	waitFor(t, fake.started)
	m, _ = updateKey(m, "q")
	m, _ = updateKey(m, "enter")
	if !m.exitAfterRun || m.phase != Stopping {
		t.Fatalf("confirmed quit did not enter stopping state")
	}
	waitFor(t, fake.cancelled)
	waitFor(t, run.Done())
	updated, cmd := m.Update(runFinishedMsg{RequestID: m.requestID, Result: run.Result()})
	if cmd == nil {
		t.Fatal("quit did not return tea.Quit after cleanup")
	}
	if updated.(Model).lastResults["alpha"] != runner.Cancelled {
		t.Fatal("quit result did not preserve runner cancellation outcome")
	}
	if got := updated.(Model).shutdown.Reason(); got != runner.StopShutdown {
		t.Fatalf("quit reason = %q, want ordinary shutdown", got)
	}
}

func TestExternalShutdownMessageQuitsIdleModel(t *testing.T) {
	m, _ := newTestModel()
	updated, cmd := m.Update(ShutdownRequestedMsg{Reason: runner.StopSIGTERM})
	if cmd == nil || updated.(Model).shutdown.Reason() != runner.StopSIGTERM {
		t.Fatalf("idle shutdown did not quit with SIGTERM reason: cmd=%v", cmd != nil)
	}
}

func TestExternalShutdownWaitsForActiveRunCleanup(t *testing.T) {
	m, fake := newTestModel()
	m, run := startModelRun(t, m)
	waitFor(t, fake.started)
	updated, cmd := m.Update(ShutdownRequestedMsg{Reason: runner.StopSIGTERM})
	m = updated.(Model)
	if cmd != nil || m.phase != Stopping || !m.exitAfterRun {
		t.Fatalf("external shutdown state: phase=%d exit=%v cmd=%v", m.phase, m.exitAfterRun, cmd != nil)
	}
	waitFor(t, fake.cancelled)
	waitFor(t, run.Done())
	updated, cmd = m.Update(runFinishedMsg{RequestID: m.requestID, Result: run.Result()})
	if cmd == nil || updated.(Model).shutdown.Reason() != runner.StopSIGTERM {
		t.Fatal("external shutdown did not quit after cleanup")
	}
}

func TestShutdownStateKeepsFirstReason(t *testing.T) {
	state := &ShutdownState{}
	if reason, accepted := state.Request(runner.StopShutdown); !accepted || reason != runner.StopShutdown {
		t.Fatalf("first request = (%q, %v)", reason, accepted)
	}
	if reason, accepted := state.Request(runner.StopSIGTERM); accepted || reason != runner.StopShutdown {
		t.Fatalf("later request = (%q, %v), want first reason", reason, accepted)
	}
}

func TestCtrlCRecordsSIGINT(t *testing.T) {
	m, fake := newTestModel()
	m, run := startModelRun(t, m)
	waitFor(t, fake.started)
	m, _ = updateKey(m, "ctrl+c")
	if m.phase != Stopping || m.shutdown.Reason() != runner.StopSIGINT {
		t.Fatalf("Ctrl+C state: phase=%d reason=%q", m.phase, m.shutdown.Reason())
	}
	waitFor(t, fake.cancelled)
	waitFor(t, run.Done())
}

func TestStaleRunMessagesAreIgnored(t *testing.T) {
	m, fake := newTestModel()
	m, _ = startModelRun(t, m)
	updated, cmd := m.Update(runFinishedMsg{RequestID: m.requestID + 1, Result: runner.Result{Status: runner.Failed}})
	if cmd != nil || updated.(Model).phase != Active {
		t.Fatal("stale completion changed active state")
	}
	updated, cmd = m.Update(refreshMsg{RequestID: m.requestID - 1, At: time.Now()})
	if cmd != nil || updated.(Model).phase != Active {
		t.Fatal("stale refresh changed active state")
	}
	close(fake.release)
	waitFor(t, m.run.Done())
}

func TestStartFailureReturnsToIdleAndRecordsError(t *testing.T) {
	manager := session.NewManager(nil)
	_ = manager.CloseAndWait()
	m := NewModel("example", []task.Spec{{Name: "alpha"}}, manager, context.Background(), &ShutdownState{})
	m, start := updateKey(m, "enter")
	updated, cmd := m.Update(start().(runAcceptedMsg))
	m = updated.(Model)
	if cmd != nil || m.phase != Idle || m.lastResults["alpha"] != runner.StartFailed {
		t.Fatalf("start failure state: phase=%d result=%q cmd=%v", m.phase, m.lastResults["alpha"], cmd != nil)
	}
}
