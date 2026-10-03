// Package session coordinates asynchronous task runs for interactive clients.
package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/zishan044/workrun/internal/output"
	"github.com/zishan044/workrun/internal/runner"
	"github.com/zishan044/workrun/internal/task"
)

// Manager admits at most one run at a time and closes admission during shutdown.
type Manager struct {
	mu     sync.Mutex
	runner runner.Runner
	closed bool
	active *Run
	nextID uint64
}

// Run is a handle for one accepted asynchronous task execution.
type Run struct {
	ID          uint64
	TaskName    string
	RequestedAt time.Time
	Output      *output.Store

	manager *Manager
	ctx     context.Context
	cancel  context.CancelCauseFunc
	done    chan struct{}
	result  runner.Result // immutable after Done closes
}

// NewManager constructs a manager using the supplied blocking runner.
func NewManager(r runner.Runner) *Manager { return &Manager{runner: r} }

// Start accepts a cloned task for asynchronous execution.
func (m *Manager) Start(ctx context.Context, spec task.Spec) (*Run, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("session manager is closed")
	}
	if m.active != nil {
		return nil, fmt.Errorf("task %q is already active", m.active.TaskName)
	}
	if m.runner == nil {
		return nil, errors.New("session manager has no runner")
	}

	cloned := cloneSpec(spec)
	runCtx, cancel := context.WithCancelCause(ctx)
	m.nextID++
	run := &Run{
		ID:          m.nextID,
		TaskName:    cloned.Name,
		RequestedAt: time.Now(),
		Output:      output.NewStore(output.DefaultLimits()),
		manager:     m,
		ctx:         runCtx,
		cancel:      cancel,
		done:        make(chan struct{}),
	}
	m.active = run // register before worker launch while admission remains locked
	go run.execute(m.runner, cloned)
	return run, nil
}

func (r *Run) execute(execRunner runner.Runner, spec task.Spec) {
	result := execRunner.Run(r.ctx, spec, r.Output.StdoutWriter(), r.Output.StderrWriter())
	r.Output.Finish()

	r.manager.mu.Lock()
	r.result = result
	if r.manager.active == r {
		r.manager.active = nil
	}
	close(r.done) // publishes result and finished output before another Start proceeds
	r.manager.mu.Unlock()
	r.cancel(nil)
}

// CloseAndWait prevents future starts, cancels active work, and waits for cleanup.
func (m *Manager) CloseAndWait() error {
	m.mu.Lock()
	m.closed = true
	active := m.active
	m.mu.Unlock()
	if active == nil {
		return nil
	}
	active.Cancel(runner.ErrShutdown)
	<-active.Done()
	result := active.Result()
	if result.CleanupErr != nil {
		return result.CleanupErr
	}
	if result.Status == runner.RunnerError && result.Err != nil {
		return result.Err
	}
	return nil
}

// Cancel requests cancellation. A nil cause is normalized to user cancellation.
func (r *Run) Cancel(cause error) {
	if cause == nil {
		cause = runner.ErrUserCancel
	}
	r.cancel(cause)
}

// Done closes after output finalization and result publication.
func (r *Run) Done() <-chan struct{} { return r.done }

// Result returns the immutable final result. Call only after Done closes.
func (r *Run) Result() runner.Result { return r.result }

func cloneSpec(spec task.Spec) task.Spec {
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
