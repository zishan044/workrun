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
	"syscall"
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
// Cancellation and timeout enforcement after process start are handled by the
// process supervisor milestone; this implementation honors cancellation before
// it starts a child.
type ExecRunner struct{}

// New constructs a runner.
func New() *ExecRunner { return &ExecRunner{} }

// Run starts the task and waits for the direct child exactly once. It preserves
// stdout and stderr as separate streams and does not invoke a shell implicitly.
func (r *ExecRunner) Run(ctx context.Context, spec task.Spec, stdout, stderr io.Writer) Result {
	requestedAt := time.Now()
	result := Result{
		TaskName:    spec.Name,
		ExitCode:    -1,
		RequestedAt: requestedAt,
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		result.FinishedAt = time.Now()
		result.Err = err
		if errors.Is(err, context.DeadlineExceeded) {
			result.Status = TimedOut
			result.StopReason = StopTimeout
		} else {
			result.Status = Cancelled
			result.StopReason = StopUser
		}
		return result
	}

	// Own copies so a caller cannot mutate command arguments or environment while
	// the child is being prepared.
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
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Environ accounts for cmd.Dir (including PWD where supported). Apply task
	// overrides after obtaining the inherited environment.
	cmd.Env = mergeEnvironment(cmd.Environ(), envOverrides)

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

	if err := cmd.Start(); err != nil {
		result.Status = StartFailed
		result.Err = fmt.Errorf("start task %q: %w", spec.Name, err)
		result.FinishedAt = time.Now()
		return result
	}
	result.ProcessStarted = true
	result.StartedAt = time.Now()

	// Wait has a single owner. If a capture writer fails, stop the still-running
	// direct child and then collect the one Wait result.
	waitResult := make(chan error, 1)
	go func() { waitResult <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waitResult:
	case outputErr := <-writerErrors:
		result.StopReason = StopOutput
		result.OutputIncomplete = true
		result.Err = fmt.Errorf("write task output: %w", outputErr)
		if cmd.Process != nil {
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				result.CleanupErr = fmt.Errorf("stop task after output failure: %w", err)
			}
		}
		waitErr = <-waitResult
	}

	if err := tracker.error(); err != nil {
		result.OutputIncomplete = true
		if result.Err == nil {
			result.Err = fmt.Errorf("write task output: %w", err)
		}
	}
	classifyWait(&result, cmd.ProcessState, waitErr)
	result.FinishedAt = time.Now()
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
	// Record process state before applying runner-error precedence so output or
	// cleanup failures do not erase what is known about the child.
	if processState != nil {
		result.ExitCode = processState.ExitCode()
		if status, ok := processState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			result.TermSignal = int(status.Signal())
			result.ExitCode = -1
		}
	}

	if result.Err != nil || result.CleanupErr != nil {
		result.Status = RunnerError
		if result.Err == nil {
			result.Err = result.CleanupErr
		}
	} else if waitErr == nil {
		result.Status = Succeeded
	} else {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			result.Err = waitErr
			result.Status = Failed
		} else {
			result.Status = RunnerError
			result.Err = waitErr
		}
	}
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
