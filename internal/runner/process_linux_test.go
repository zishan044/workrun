package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

var (
	helperPath string
	helperErr error
)

func TestMain(m *testing.M) {
	var err error
	helperPath, err = os.MkdirTemp("", "workrun-helper-")
	if err == nil {
		binary := filepath.Join(helperPath, "helper")
		cmd := exec.Command("go", "build", "-o", binary, "./testdata/helper")
		err = cmd.Run()
		if err == nil {
			helperPath = binary
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "build runner helper:", err)
		os.Exit(2)
	}
	code := m.Run()
	if filepath.IsAbs(helperPath) {
		_ = os.RemoveAll(filepath.Dir(helperPath))
	}
	os.Exit(code)
}

func TestWaitidRetriesInterruptedCall(t *testing.T) {
	calls := 0
	err := waitidUntilExit(123, func(idType, id int, _ *unix.Siginfo, options int, _ *unix.Rusage) error {
		calls++
		if idType != unix.P_PID || id != 123 || options != unix.WEXITED|unix.WNOWAIT {
			t.Fatalf("waitid args = (%d, %d, %d)", idType, id, options)
		}
		if calls == 1 {
			return unix.EINTR
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("waitid result = (%v, calls %d), want (nil, 2)", err, calls)
	}
}

func TestWaitidOwnershipErrorIsReturned(t *testing.T) {
	err := waitidUntilExit(123, func(int, int, *unix.Siginfo, int, *unix.Rusage) error { return unix.ECHILD })
	if !errors.Is(err, unix.ECHILD) {
		t.Fatalf("waitid error = %v, want ECHILD", err)
	}
}

func TestCancellationStopsReadyProcessGroup(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "ready")
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan Result, 1)
	go func() { done <- New().Run(ctx, helperSpec("wait", readyPath), nil, nil) }()
	waitForFile(t, readyPath)
	cancel(ErrSIGINT)
	result := <-done
	if result.Status != Cancelled || result.StopReason != StopSIGINT || !result.ProcessStarted {
		t.Fatalf("cancellation result = %+v", result)
	}
}

func TestTaskTimeoutStopsReadyProcessGroup(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "ready")
	spec := helperSpec("wait", readyPath)
	spec.Timeout = 2 * time.Second
	done := make(chan Result, 1)
	go func() { done <- New().Run(context.Background(), spec, nil, nil) }()
	waitForFile(t, readyPath)
	result := <-done
	if result.Status != TimedOut || result.StopReason != StopTimeout || !result.ProcessStarted {
		t.Fatalf("timeout result = %+v", result)
	}
}

func TestCancellationStopsParentAndChild(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "ready")
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan Result, 1)
	go func() { done <- New().Run(ctx, helperSpec("spawn-child", helperPath, readyPath), nil, nil) }()
	waitForFile(t, readyPath)
	pid := readPID(t, readyPath+".pid")
	cancel(ErrUserCancel)
	result := <-done
	if result.Status != Cancelled {
		t.Fatalf("cancellation result = %+v", result)
	}
	waitForDeadOrZombie(t, pid)
}

func TestNormalLeaderExitCleansUpChild(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "ready")
	spec := helperSpec("parent-exits", helperPath, readyPath)
	result := New().Run(context.Background(), spec, nil, nil)
	if result.Status != Succeeded {
		t.Fatalf("parent result = %+v", result)
	}
	pid := readPID(t, readyPath+".pid")
	waitForDeadOrZombie(t, pid)
}

func TestOutputWriterFailureStopsProcessGroup(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "ready")
	result := New().Run(context.Background(), helperSpec("output", readyPath), failedWriter{}, nil)
	if result.Status != RunnerError || !result.OutputIncomplete || result.StopReason != StopOutput {
		t.Fatalf("output failure result = %+v", result)
	}
}

func TestCompletionAcceptedBeforeLaterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	result := New().Run(ctx, helperSpec("exit", "0"), nil, nil)
	cancel(ErrShutdown)
	if result.Status != Succeeded || result.StopReason != StopNone {
		t.Fatalf("completed result changed after cancellation: %+v", result)
	}
}

func TestSupervisorSignalsOnlyBeforeWaitReaps(t *testing.T) {
	cmd := helperCommand("exit", "0")
	var events []string
	ops := processOps{
		observe: func(int) error { return nil },
		signal: func(pid, pgid int) error {
			if pid <= 1 || pid != pgid {
				t.Fatalf("unsafe signal target pid=%d pgid=%d", pid, pgid)
			}
			events = append(events, "signal")
			return nil
		},
		killDirect: func(*os.Process) error { return nil },
		wait: func(cmd *exec.Cmd) error {
			events = append(events, "wait")
			return cmd.Wait()
		},
	}
	out := superviseWithOps(context.Background(), cmd, nil, ops)
	if out.waitErr != nil {
		t.Fatalf("wait error: %v", out.waitErr)
	}
	if len(events) != 2 || events[0] != "signal" || events[1] != "wait" {
		t.Fatalf("lifecycle events = %v, want [signal wait]", events)
	}
}

func TestECHILDOwnershipFailureDoesNotSignalGroup(t *testing.T) {
	cmd := helperCommand("exit", "0")
	var signals int
	ops := processOps{
		observe:    func(int) error { return unix.ECHILD },
		signal:     func(int, int) error { signals++; return nil },
		killDirect: func(*os.Process) error { return nil },
		wait:       func(cmd *exec.Cmd) error { return cmd.Wait() },
	}
	out := superviseWithOps(context.Background(), cmd, nil, ops)
	if signals != 0 || out.err == nil {
		t.Fatalf("ECHILD handling: signals=%d outcome=%+v", signals, out)
	}
}

func helperCommand(mode string, args ...string) *exec.Cmd {
	cmd := exec.Command(helperPath, append([]string{mode}, args...)...)
	cmd.SysProcAttr = processAttributes()
	return cmd
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("readiness file %q was not created", path)
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func waitForDeadOrZombie(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err == nil {
			closeParen := strings.LastIndexByte(string(data), ')')
			if closeParen >= 0 && closeParen+2 < len(data) && string(data[closeParen+2]) == "Z" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d remained live", pid)
}

type failedWriter struct{}

func (failedWriter) Write([]byte) (int, error) { return 0, errors.New("writer failed") }
