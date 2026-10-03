// Package runner executes validated task specifications synchronously.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zishan044/workrun/internal/task"
)

// Status describes the process outcome.
type Status string

const (
	Succeeded   Status = "succeeded"
	Failed      Status = "failed"
	StartFailed Status = "start_failed"
	Cancelled   Status = "cancelled"
	TimedOut    Status = "timed_out"
	RunnerError Status = "runner_error"
)

// StopReason records why a run was asked to stop.
type StopReason string

const (
	StopNone     StopReason = ""
	StopUser     StopReason = "user"
	StopTimeout  StopReason = "timeout"
	StopSIGINT   StopReason = "sigint"
	StopSIGTERM  StopReason = "sigterm"
	StopShutdown StopReason = "shutdown"
	StopOutput   StopReason = "output_error"
)

// Exported causes callers can pass to context.CancelCauseFunc.
var (
	ErrUserCancel = errors.New("user cancelled task")
	ErrShutdown   = errors.New("application shutdown")
	ErrSIGINT     = errors.New("received SIGINT")
	ErrSIGTERM    = errors.New("received SIGTERM")
)

// ErrUnsupportedPlatform indicates that process-group execution is available
// only on Linux in this release.
var ErrUnsupportedPlatform = errors.New("task execution is supported only on Linux")

// Result contains the child outcome and any independent runner or cleanup error.
type Result struct {
	TaskName         string
	Status           Status
	StopReason       StopReason
	ProcessStarted   bool
	ExitCode         int
	TermSignal       int
	RequestedAt      time.Time
	StartedAt        time.Time
	FinishedAt       time.Time
	Err              error
	CleanupErr       error
	OutputIncomplete bool
}

// Runner is the blocking task execution contract shared by CLI and TUI callers.
type Runner interface {
	Run(ctx context.Context, spec task.Spec, stdout, stderr io.Writer) Result
}

// ExecRunner runs a task using the operating system process implementation.
type ExecRunner struct{}

// New constructs a runner.
func New() *ExecRunner { return &ExecRunner{} }

// Run executes the task in an owned process group and waits for the direct child
// exactly once. Cancellation force-kills the group and may interrupt child I/O.
func (r *ExecRunner) Run(ctx context.Context, spec task.Spec, stdout, stderr io.Writer) Result {
	result := Result{TaskName: spec.Name, ExitCode: -1, RequestedAt: time.Now()}
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx := ctx
	cancelTimeout := func() {}
	if spec.Timeout > 0 {
		runCtx, cancelTimeout = context.WithTimeout(ctx, spec.Timeout)
	}
	defer cancelTimeout()
	if err := runCtx.Err(); err != nil {
		result = classifyStop(result, context.Cause(runCtx), err)
		result.FinishedAt = time.Now()
		return result
	}

	// Own copies so caller mutation cannot affect command construction.
	argv := append([]string(nil), spec.Argv...)
	envOverrides := make(map[string]string, len(spec.Env))
	for key, value := range spec.Env {
		envOverrides[key] = value
	}
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return finishError(result, RunnerError, fmt.Errorf("task %q has no executable", spec.Name))
	}
	for i, arg := range argv {
		if strings.IndexByte(arg, 0) >= 0 {
			return finishError(result, RunnerError, fmt.Errorf("task %q argument %d contains a NUL byte", spec.Name, i))
		}
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = mergeEnvironment(cmd.Environ(), envOverrides)
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = processAttributes()
	cmd.WaitDelay = 2 * time.Second

	writerErrors := make(chan error, 1)
	tracker := &writeTracker{notify: writerErrors}
	if stdout != nil {
		if _, ok := stdout.(*os.File); !ok {
			cmd.Stdout = trackedWriter{target: stdout, tracker: tracker}
		}
	}
	if stderr != nil {
		if _, ok := stderr.(*os.File); !ok {
			cmd.Stderr = trackedWriter{target: stderr, tracker: tracker}
		}
	}

	outcome := supervise(runCtx, cmd, writerErrors)
	result.ProcessStarted = outcome.started
	result.StartedAt = outcome.startedAt
	if outcome.startFailed {
		result.Status = StartFailed
		result.Err = outcome.err
		result.FinishedAt = time.Now()
		return result
	}
	result.StopReason = outcome.stopReason
	result.Err = outcome.err
	result.CleanupErr = outcome.cleanupErr
	result.OutputIncomplete = outcome.outputIncomplete
	if err := tracker.error(); err != nil {
		result.OutputIncomplete = true
		if result.StopReason == StopNone {
			result.StopReason = StopOutput
		}
		if result.Err == nil {
			result.Err = fmt.Errorf("write task output: %w", err)
		}
	}
	classifyWait(&result, outcome.processState, outcome.waitErr)
	result.FinishedAt = time.Now()
	return result
}

type processOutcome struct {
	started          bool
	startFailed      bool
	startedAt        time.Time
	processState     *os.ProcessState
	waitErr          error
	stopReason       StopReason
	err              error
	cleanupErr       error
	outputIncomplete bool
}

func classifyStop(result Result, cause, ctxErr error) Result {
	result.Err = ctxErr
	switch {
	case errors.Is(ctxErr, context.DeadlineExceeded):
		result.Status = TimedOut
		result.StopReason = StopTimeout
	case errors.Is(cause, ErrShutdown):
		result.Status = Cancelled
		result.StopReason = StopShutdown
	case errors.Is(cause, ErrSIGINT):
		result.Status = Cancelled
		result.StopReason = StopSIGINT
	case errors.Is(cause, ErrSIGTERM):
		result.Status = Cancelled
		result.StopReason = StopSIGTERM
	default:
		result.Status = Cancelled
		result.StopReason = StopUser
	}
	return result
}

func finishError(result Result, status Status, err error) Result {
	result.Status = status
	result.Err = err
	result.FinishedAt = time.Now()
	return result
}

func mergeEnvironment(inherited []string, overrides map[string]string) []string {
	values := make(map[string]string, len(inherited)+len(overrides))
	for _, entry := range inherited {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	for key, value := range overrides {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	merged := make([]string, 0, len(keys))
	for _, key := range keys {
		merged = append(merged, key+"="+values[key])
	}
	return merged
}

func classifyWait(result *Result, processState *os.ProcessState, waitErr error) {
	if processState != nil {
		result.ExitCode, result.TermSignal = processExitDetails(processState)
	}
	if result.Err != nil || result.CleanupErr != nil {
		result.Status = RunnerError
		if result.Err == nil {
			result.Err = result.CleanupErr
		}
		return
	}
	var exitErr *exec.ExitError
	isExitError := errors.As(waitErr, &exitErr)
	if waitErr != nil && !isExitError {
		result.Status = RunnerError
		result.Err = waitErr
		return
	}
	switch result.StopReason {
	case StopTimeout:
		result.Status = TimedOut
		return
	case StopUser, StopShutdown, StopSIGINT, StopSIGTERM:
		result.Status = Cancelled
		return
	}
	if waitErr == nil {
		result.Status = Succeeded
		if processState == nil {
			result.ExitCode = 0
		}
		return
	}
	result.Status = Failed
	result.Err = waitErr
}

type writeTracker struct {
	mu     sync.Mutex
	err    error
	notify chan<- error
}

func (t *writeTracker) record(err error) {
	if err == nil {
		return
	}
	t.mu.Lock()
	first := t.err == nil
	if first {
		t.err = err
	}
	t.mu.Unlock()
	if first {
		select {
		case t.notify <- err:
		default:
		}
	}
}

func (t *writeTracker) error() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

type trackedWriter struct {
	target  io.Writer
	tracker *writeTracker
}

func (w trackedWriter) Write(p []byte) (int, error) {
	n, err := w.target.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.tracker.record(err)
	}
	return n, err
}
