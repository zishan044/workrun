package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/zishan044/workrun/internal/runner"
	"github.com/zishan044/workrun/internal/session"
	"github.com/zishan044/workrun/internal/task"
	"github.com/zishan044/workrun/internal/tui"
)

type shutdownTestRunner struct {
	started   chan struct{}
	cancelled chan struct{}
}

type shutdownRunnerFunc func(context.Context, task.Spec, io.Writer, io.Writer) runner.Result

func (f shutdownRunnerFunc) Run(ctx context.Context, spec task.Spec, stdout, stderr io.Writer) runner.Result {
	return f(ctx, spec, stdout, stderr)
}

func (r *shutdownTestRunner) Run(ctx context.Context, spec task.Spec, _, _ io.Writer) runner.Result {
	close(r.started)
	<-ctx.Done()
	close(r.cancelled)
	return runner.Result{TaskName: spec.Name, Status: runner.Cancelled, StopReason: runner.StopShutdown}
}

type shutdownTestProgram struct {
	run      func() error
	msgs     chan tea.Msg
}

func (p *shutdownTestProgram) Run() (tea.Model, error) { return nil, p.run() }
func (p *shutdownTestProgram) Send(msg tea.Msg)        { p.msgs <- msg }

func waitSignalTest(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for managed task cleanup")
	}
}

func TestRunTUIProgramAlwaysClosesManagerAfterProgramReturn(t *testing.T) {
	uiFailure := errors.New("UI failed")
	for _, test := range []struct {
		name   string
		runErr error
	}{{name: "normal"}, {name: "error", runErr: uiFailure}} {
		t.Run(test.name, func(t *testing.T) {
			r := &shutdownTestRunner{started: make(chan struct{}), cancelled: make(chan struct{})}
			manager := session.NewManager(r)
			appCtx, cancelApp := context.WithCancelCause(context.Background())
			defer cancelApp(nil)
			if _, err := manager.Start(appCtx, task.Spec{Name: "active"}); err != nil {
				t.Fatal(err)
			}
			waitSignalTest(t, r.started)
			p := &shutdownTestProgram{run: func() error { return test.runErr }, msgs: make(chan tea.Msg, 1)}
			_, err := runTUIProgram(p, manager, cancelApp, &tui.ShutdownState{}, nil, func() {})
			waitSignalTest(t, r.cancelled)
			if !errors.Is(err, test.runErr) {
				t.Fatalf("runTUIProgram error = %v, want %v", err, test.runErr)
			}
			if _, err := manager.Start(context.Background(), task.Spec{Name: "late"}); err == nil {
				t.Fatal("manager admitted a run after Program.Run returned")
			}
		})
	}
}

func TestRunTUIProgramSignalCancelsSessionAndSendsMessage(t *testing.T) {
	r := &shutdownTestRunner{started: make(chan struct{}), cancelled: make(chan struct{})}
	manager := session.NewManager(r)
	appCtx, cancelApp := context.WithCancelCause(context.Background())
	defer cancelApp(nil)
	if _, err := manager.Start(appCtx, task.Spec{Name: "active"}); err != nil {
		t.Fatal(err)
	}
	waitSignalTest(t, r.started)
	signals := make(chan os.Signal, 1)
	p := &shutdownTestProgram{msgs: make(chan tea.Msg, 1)}
	p.run = func() error {
		signals <- syscall.SIGTERM
		select {
		case <-p.msgs:
			return nil
		case <-time.After(2 * time.Second):
			return errors.New("signal message was not delivered")
		}
	}
	reason, err := runTUIProgram(p, manager, cancelApp, &tui.ShutdownState{}, signals, func() {})
	waitSignalTest(t, r.cancelled)
	if err != nil || reason != runner.StopSIGTERM {
		t.Fatalf("runTUIProgram = (%q, %v), want SIGTERM and nil", reason, err)
	}
}

func TestTUIExitCodes(t *testing.T) {
	for _, test := range []struct {
		reason runner.StopReason
		want   int
	}{{runner.StopShutdown, 0}, {runner.StopSIGINT, 130}, {runner.StopSIGTERM, 143}, {runner.StopNone, 0}} {
		if got := tuiExitCode(test.reason, nil); got != test.want {
			t.Errorf("tuiExitCode(%q) = %d, want %d", test.reason, got, test.want)
		}
	}
	if got := tuiExitCode(runner.StopSIGTERM, errors.New("cleanup failed")); got != 1 {
		t.Fatalf("cleanup failure exit code = %d, want 1", got)
	}
}

func TestCleanupFailureOverridesSignalExitCode(t *testing.T) {
	wantErr := errors.New("task cleanup failed")
	r := shutdownRunnerFunc(func(ctx context.Context, spec task.Spec, _, _ io.Writer) runner.Result {
		<-ctx.Done()
		return runner.Result{TaskName: spec.Name, Status: runner.RunnerError, Err: wantErr}
	})
	manager := session.NewManager(r)
	appCtx, cancelApp := context.WithCancelCause(context.Background())
	defer cancelApp(nil)
	if _, err := manager.Start(appCtx, task.Spec{Name: "active"}); err != nil {
		t.Fatal(err)
	}
	state := &tui.ShutdownState{}
	state.Request(runner.StopSIGTERM)
	p := &shutdownTestProgram{run: func() error { return nil }, msgs: make(chan tea.Msg, 1)}
	reason, err := runTUIProgram(p, manager, cancelApp, state, nil, func() {})
	if !errors.Is(err, wantErr) || tuiExitCode(reason, err) != 1 {
		t.Fatalf("cleanup result reason=%q error=%v exit=%d; want error and code 1", reason, err, tuiExitCode(reason, err))
	}
}
