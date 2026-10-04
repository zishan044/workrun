package session

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/zishan044/workrun/internal/runner"
	"github.com/zishan044/workrun/internal/task"
)

const waitLimit = 2 * time.Second

type runControl struct {
	started       chan struct{}
	release       chan struct{}
	cleanup       chan struct{}
	cleanupFinish chan struct{}
	startOnce     sync.Once
	cleanupOnce   sync.Once
}

func newRunControl() *runControl {
	return &runControl{
		started:       make(chan struct{}),
		release:       make(chan struct{}),
		cleanup:       make(chan struct{}),
		cleanupFinish: make(chan struct{}),
	}
}

type controlledRunner struct {
	mu        sync.Mutex
	controls  map[string]*runControl
	requested []task.Spec
}

func newControlledRunner(names ...string) *controlledRunner {
	r := &controlledRunner{controls: make(map[string]*runControl, len(names))}
	for _, name := range names {
		r.controls[name] = newRunControl()
	}
	return r
}

func (r *controlledRunner) Run(ctx context.Context, spec task.Spec, stdout, stderr io.Writer) runner.Result {
	r.mu.Lock()
	control := r.controls[spec.Name]
	r.requested = append(r.requested, cloneSpec(spec))
	r.mu.Unlock()
	if control == nil {
		panic("missing controlled run: " + spec.Name)
	}
	control.startOnce.Do(func() { close(control.started) })
	_, _ = io.WriteString(stdout, "live")
	cancelled := false
	select {
	case <-control.release:
	case <-ctx.Done():
		cancelled = true
		control.cleanupOnce.Do(func() { close(control.cleanup) })
		<-control.cleanupFinish
	}
	_, _ = io.WriteString(stdout, " final\n")
	result := runner.Result{TaskName: spec.Name, ExitCode: 0, ProcessStarted: true}
	if cancelled {
		result.Status = runner.Cancelled
		result.StopReason = runner.StopShutdown
	} else {
		result.Status = runner.Succeeded
	}
	return result
}

func TestSimultaneousStartsAcceptOnlyOne(t *testing.T) {
	r := newControlledRunner("first")
	m := NewManager(r)
	const callers = 12
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	acceptedRun := make(chan *Run, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run, err := m.Start(context.Background(), task.Spec{Name: "first", Argv: []string{"cmd"}})
			if err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
				acceptedRun <- run
			}
		}()
	}
	waitFor(t, r.controls["first"].started)
	wg.Wait()
	mu.Lock()
	got := accepted
	mu.Unlock()
	if got != 1 {
		t.Fatalf("accepted starts = %d, want exactly one", got)
	}
	close(r.controls["first"].release)
	waitFor(t, (<-acceptedRun).Done())
	if err := m.CloseAndWait(); err != nil {
		t.Fatalf("CloseAndWait() error = %v", err)
	}
}

func TestCancelIsSafeTwice(t *testing.T) {
	r := newControlledRunner("cancel")
	m := NewManager(r)
	handle, err := m.Start(context.Background(), task.Spec{Name: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, r.controls["cancel"].started)
	handle.Cancel(runner.ErrUserCancel)
	handle.Cancel(runner.ErrUserCancel)
	close(r.controls["cancel"].cleanupFinish)
	waitFor(t, handle.Done())
	if got := handle.Result().Status; got != runner.Cancelled {
		t.Fatalf("result status = %q, want %q", got, runner.Cancelled)
	}
	if err := m.CloseAndWait(); err != nil {
		t.Fatalf("CloseAndWait() error = %v", err)
	}
}

func TestCompletionIsReadableByMultipleWaiters(t *testing.T) {
	r := newControlledRunner("waiters")
	m := NewManager(r)
	handle, err := m.Start(context.Background(), task.Spec{Name: "waiters"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, r.controls["waiters"].started)
	close(r.controls["waiters"].release)
	const waiters = 8
	var wg sync.WaitGroup
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			waitFor(t, handle.Done())
			if got := handle.Result().Status; got != runner.Succeeded {
				t.Errorf("result status = %q, want %q", got, runner.Succeeded)
			}
		}()
	}
	wg.Wait()
	snapshot, changed := handle.Output.SnapshotIfChanged(0)
	if !changed || len(snapshot.Records) != 1 || snapshot.Records[0].Text != "live final" {
		t.Fatalf("published output snapshot = %#v, changed=%v", snapshot.Records, changed)
	}
	if err := m.CloseAndWait(); err != nil {
		t.Fatalf("CloseAndWait() error = %v", err)
	}
}

func TestShutdownWaitsForCleanupAndClosesAdmissionWithoutHoldingLock(t *testing.T) {
	r := newControlledRunner("shutdown")
	m := NewManager(r)
	handle, err := m.Start(context.Background(), task.Spec{Name: "shutdown"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, r.controls["shutdown"].started)
	closed := make(chan error, 1)
	go func() { closed <- m.CloseAndWait() }()
	waitFor(t, r.controls["shutdown"].cleanup)
	select {
	case err := <-closed:
		t.Fatalf("CloseAndWait returned before runner cleanup: %v", err)
	default:
	}
	if _, err := m.Start(context.Background(), task.Spec{Name: "late"}); err == nil {
		t.Fatal("Start succeeded after shutdown closed admission")
	}
	close(r.controls["shutdown"].cleanupFinish)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("CloseAndWait() error = %v", err)
		}
	case <-time.After(waitLimit):
		t.Fatal("CloseAndWait did not return after cleanup")
	}
	waitFor(t, handle.Done())
}

func TestStartRacingShutdownIsEitherAcceptedThenCleanedOrRejected(t *testing.T) {
	r := newControlledRunner("race")
	m := NewManager(r)
	startResult := make(chan *Run, 1)
	startErr := make(chan error, 1)
	go func() {
		handle, err := m.Start(context.Background(), task.Spec{Name: "race"})
		startResult <- handle
		startErr <- err
	}()
	closeResult := make(chan error, 1)
	go func() { closeResult <- m.CloseAndWait() }()
	handle := <-startResult
	err := <-startErr
	if err != nil && handle != nil {
		t.Fatalf("Start returned handle and error: %v", err)
	}
	if err == nil {
		if handle == nil {
			t.Fatal("accepted Start returned nil handle")
		}
		waitFor(t, r.controls["race"].cleanup)
		close(r.controls["race"].cleanupFinish)
		waitFor(t, handle.Done())
	}
	select {
	case err := <-closeResult:
		if err != nil {
			t.Fatalf("CloseAndWait() error = %v", err)
		}
	case <-time.After(waitLimit):
		t.Fatal("shutdown did not settle the racing start")
	}
}

func TestFinishedRunAllowsAnotherRun(t *testing.T) {
	r := newControlledRunner("one", "two")
	m := NewManager(r)
	first, err := m.Start(context.Background(), task.Spec{Name: "one"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, r.controls["one"].started)
	close(r.controls["one"].release)
	waitFor(t, first.Done())
	second, err := m.Start(context.Background(), task.Spec{Name: "two"})
	if err != nil {
		t.Fatalf("Start after completion: %v", err)
	}
	waitFor(t, r.controls["two"].started)
	close(r.controls["two"].release)
	waitFor(t, second.Done())
	if err := m.CloseAndWait(); err != nil {
		t.Fatalf("CloseAndWait() error = %v", err)
	}
}

func TestCancellingOldHandleDoesNotCancelNewRun(t *testing.T) {
	r := newControlledRunner("old", "new")
	m := NewManager(r)
	old, err := m.Start(context.Background(), task.Spec{Name: "old"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, r.controls["old"].started)
	close(r.controls["old"].release)
	waitFor(t, old.Done())
	current, err := m.Start(context.Background(), task.Spec{Name: "new"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, r.controls["new"].started)
	old.Cancel(runner.ErrUserCancel)
	select {
	case <-r.controls["new"].cleanup:
		t.Fatal("cancelling old handle cancelled the new run")
	default:
	}
	close(r.controls["new"].release)
	waitFor(t, current.Done())
	if got := current.Result().Status; got != runner.Succeeded {
		t.Fatalf("new run status = %q, want success", got)
	}
	if err := m.CloseAndWait(); err != nil {
		t.Fatalf("CloseAndWait() error = %v", err)
	}
}

func TestFinalOutputIsFinishedBeforeCompletionPublication(t *testing.T) {
	r := newControlledRunner("output")
	m := NewManager(r)
	handle, err := m.Start(context.Background(), task.Spec{Name: "output"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, r.controls["output"].started)
	close(r.controls["output"].release)
	waitFor(t, handle.Done())
	snapshot, changed := handle.Output.SnapshotIfChanged(0)
	if !changed || len(snapshot.Records) != 1 || snapshot.Records[0].Partial {
		t.Fatalf("output was not finalized before Done: changed=%v records=%#v", changed, snapshot.Records)
	}
	if snapshot.Records[0].Text != "live final" {
		t.Fatalf("final output = %q, want %q", snapshot.Records[0].Text, "live final")
	}
	if err := m.CloseAndWait(); err != nil {
		t.Fatalf("CloseAndWait() error = %v", err)
	}
}

func TestStartClonesTaskSlicesAndMaps(t *testing.T) {
	r := newControlledRunner("clone")
	m := NewManager(r)
	spec := task.Spec{Name: "clone", Argv: []string{"program", "before"}, Env: map[string]string{"KEY": "before"}}
	handle, err := m.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.Argv[1] = "after"
	spec.Env["KEY"] = "after"
	waitFor(t, r.controls["clone"].started)
	r.mu.Lock()
	got := r.requested[0]
	r.mu.Unlock()
	if got.Argv[1] != "before" || got.Env["KEY"] != "before" {
		t.Fatalf("runner received mutated task: %#v", got)
	}
	close(r.controls["clone"].release)
	waitFor(t, handle.Done())
	if err := m.CloseAndWait(); err != nil {
		t.Fatalf("CloseAndWait() error = %v", err)
	}
}

func TestCloseAndWaitReturnsRunnerCleanupError(t *testing.T) {
	want := errors.New("cleanup failed")
	r := &gatedErrorRunner{err: want, cancelled: make(chan struct{}), release: make(chan struct{})}
	m := NewManager(r)
	if _, err := m.Start(context.Background(), task.Spec{Name: "broken"}); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- m.CloseAndWait() }()
	waitFor(t, r.cancelled)
	close(r.release)
	select {
	case err := <-closed:
		if !errors.Is(err, want) {
			t.Fatalf("CloseAndWait() error = %v, want %v", err, want)
		}
	case <-time.After(waitLimit):
		t.Fatal("CloseAndWait did not return after runner cleanup")
	}
}

type gatedErrorRunner struct {
	err       error
	cancelled chan struct{}
	release   chan struct{}
}

func (r *gatedErrorRunner) Run(ctx context.Context, spec task.Spec, _, _ io.Writer) runner.Result {
	<-ctx.Done()
	close(r.cancelled)
	<-r.release
	return runner.Result{TaskName: spec.Name, Status: runner.RunnerError, Err: r.err}
}

func waitFor(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for controlled lifecycle event")
	}
}
