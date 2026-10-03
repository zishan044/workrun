//go:build linux

package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func processAttributes() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

func processExitDetails(state *os.ProcessState) (int, int) {
	code := state.ExitCode()
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return -1, int(status.Signal())
	}
	return code, 0
}

type waitIDCall func(idType int, id int, info *unix.Siginfo, options int, rusage *unix.Rusage) error

type processOps struct {
	observe    func(pid int) error
	signal     func(pid, pgid int) error
	killDirect func(*os.Process) error
	wait       func(*exec.Cmd) error
}

func supervise(ctx context.Context, cmd *exec.Cmd, writerErrors <-chan error) processOutcome {
	return superviseWithOps(ctx, cmd, writerErrors, processOps{
		observe: observeChildExit,
		signal:  killProcessGroup,
		killDirect: func(process *os.Process) error {
			return process.Kill()
		},
		wait: func(cmd *exec.Cmd) error { return cmd.Wait() },
	})
}

func superviseWithOps(ctx context.Context, cmd *exec.Cmd, writerErrors <-chan error, ops processOps) processOutcome {
	var out processOutcome
	if err := cmd.Start(); err != nil {
		out.startFailed = true
		out.err = fmt.Errorf("start task: %w", err)
		return out
	}
	out.started = true
	out.startedAt = time.Now()
	pid := cmd.Process.Pid
	if pid <= 1 {
		out.err = fmt.Errorf("invalid child process identifier %d", pid)
		out.waitErr = ops.wait(cmd)
		out.processState = cmd.ProcessState
		return out
	}

	exitObserved := make(chan error, 1)
	go func() { exitObserved <- ops.observe(pid) }()

	var accepted bool
	select {
	case observeErr := <-exitObserved:
		accepted = true
		if observeErr != nil {
			out.err = fmt.Errorf("observe child exit: %w", observeErr)
		} else {
			out.cleanupErr = cleanupGroup(cmd.Process, pid, pid, ops)
		}
	case outputErr := <-writerErrors:
		accepted = true
		out.stopReason = StopOutput
		out.outputIncomplete = true
		out.err = fmt.Errorf("write task output: %w", outputErr)
		select {
		case observeErr := <-exitObserved:
			if observeErr != nil {
				out.err = errors.Join(out.err, fmt.Errorf("observe child exit: %w", observeErr))
			} else {
				out.cleanupErr = cleanupGroup(cmd.Process, pid, pid, ops)
			}
		default:
			if err := stopGroup(cmd.Process, pid, pid, ops); err != nil {
				out.cleanupErr = fmt.Errorf("stop process group after output failure: %w", err)
			}
			if observeErr := <-exitObserved; observeErr != nil {
				out.err = errors.Join(out.err, fmt.Errorf("observe child exit: %w", observeErr))
			}
		}
	case <-ctx.Done():
		// Prefer a completion already observed by the helper when events race.
		select {
		case observeErr := <-exitObserved:
			accepted = true
			if observeErr != nil {
				out.err = fmt.Errorf("observe child exit: %w", observeErr)
			} else {
				out.cleanupErr = cleanupGroup(cmd.Process, pid, pid, ops)
			}
		default:
		}
		if !accepted {
			accepted = true
			cause := context.Cause(ctx)
			out.stopReason = stopReasonForCause(cause, ctx.Err())
			if err := stopGroup(cmd.Process, pid, pid, ops); err != nil {
				out.cleanupErr = fmt.Errorf("stop process group: %w", err)
			}
			if observeErr := <-exitObserved; observeErr != nil {
				out.err = errors.Join(out.err, fmt.Errorf("observe child exit: %w", observeErr))
			}
		}
	}

	// Every branch above disables group signalling before crossing this single
	// reap boundary. The exit observer never consumes wait status.
	out.waitErr = ops.wait(cmd)
	out.processState = cmd.ProcessState
	if errors.Is(out.waitErr, exec.ErrWaitDelay) {
		out.outputIncomplete = true
		out.err = fmt.Errorf("drain task output: %w", out.waitErr)
	}
	return out
}

func observeChildExit(pid int) error {
	return waitidUntilExit(pid, unix.Waitid)
}

func waitidUntilExit(pid int, call waitIDCall) error {
	var info unix.Siginfo
	for {
		err := call(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err
	}
}

func killProcessGroup(pid, pgid int) error {
	if pid <= 1 || pgid <= 1 || pid != pgid {
		return fmt.Errorf("refusing unsafe process-group signal (pid=%d pgid=%d)", pid, pgid)
	}
	err := unix.Kill(-pgid, unix.SIGKILL)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

func stopGroup(process *os.Process, pid, pgid int, ops processOps) error {
	if err := ops.signal(pid, pgid); err != nil {
		if process != nil {
			if directErr := ops.killDirect(process); directErr != nil && !errors.Is(directErr, os.ErrProcessDone) {
				return errors.Join(fmt.Errorf("signal process group: %w", err), fmt.Errorf("kill direct child: %w", directErr))
			}
		}
		// A successful direct-child fallback does not prove group cleanup.
		return fmt.Errorf("signal process group: %w", err)
	}
	return nil
}

func cleanupGroup(process *os.Process, pid, pgid int, ops processOps) error {
	if err := stopGroup(process, pid, pgid, ops); err != nil {
		return fmt.Errorf("clean up process group after leader exit: %w", err)
	}
	return nil
}

func stopReasonForCause(cause, ctxErr error) StopReason {
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		return StopTimeout
	}
	switch {
	case errors.Is(cause, ErrShutdown):
		return StopShutdown
	case errors.Is(cause, ErrSIGINT):
		return StopSIGINT
	case errors.Is(cause, ErrSIGTERM):
		return StopSIGTERM
	default:
		return StopUser
	}
}
