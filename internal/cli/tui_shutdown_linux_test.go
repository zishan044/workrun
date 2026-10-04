//go:build linux

package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type ptyOutput struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *ptyOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *ptyOutput) has(s string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Contains(b.Buffer.Bytes(), []byte(s))
}

func TestBuiltTUIShutdownRestoresTerminalAndStopsTaskGroup(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	workrun := filepath.Join(tmp, "workrun")
	helper := filepath.Join(tmp, "helper")
	for _, build := range []struct{ output, pkg string }{{workrun, "./cmd/workrun"}, {helper, "./internal/runner/testdata/helper"}} {
		cmd := exec.Command("go", "build", "-o", build.output, build.pkg)
		cmd.Dir = root
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", build.pkg, err, output)
		}
	}

	for _, test := range []struct {
		name string
		send func(*testing.T, *os.Process, *os.File)
		code int
	}{{
		name: "ctrl-c key", code: 130,
		send: func(t *testing.T, _ *os.Process, master *os.File) {
			if _, err := master.Write([]byte{3}); err != nil {
				t.Fatal(err)
			}
		},
	}, {
		name: "SIGINT", code: 130,
		send: func(t *testing.T, process *os.Process, _ *os.File) {
			if err := process.Signal(syscall.SIGINT); err != nil {
				t.Fatal(err)
			}
		},
	}, {
		name: "SIGTERM", code: 143,
		send: func(t *testing.T, process *os.Process, _ *os.File) {
			if err := process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
		},
	}} {
		t.Run(test.name, func(t *testing.T) {
			ready := filepath.Join(tmp, strings.ReplaceAll(test.name, " ", "_")+".ready")
			config := filepath.Join(tmp, strings.ReplaceAll(test.name, " ", "_")+".yaml")
			data := fmt.Sprintf("version: 1\ntasks:\n  wait:\n    command: [%s, %s, %s, %s]\n", strconv.Quote(helper), strconv.Quote("spawn-child"), strconv.Quote(helper), strconv.Quote(ready))
			if err := os.WriteFile(config, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}

			master, slave := openTestPTY(t)
			before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(workrun, "--config", config, "tui")
			cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			if err := cmd.Start(); err != nil {
				_ = master.Close()
				_ = slave.Close()
				t.Fatal(err)
			}
			waitDone := make(chan error, 1)
			go func() { waitDone <- cmd.Wait() }()
			var output ptyOutput
			readDone := make(chan struct{})
			go func() { _, _ = io.Copy(&output, master); close(readDone) }()
			finished := false
			t.Cleanup(func() {
				if !finished {
					_ = cmd.Process.Signal(syscall.SIGTERM)
					select {
					case <-waitDone:
						finished = true
					case <-time.After(2 * time.Second):
					}
				}
				if pid, err := os.ReadFile(ready + ".pid"); err == nil {
					if n, err := strconv.Atoi(strings.TrimSpace(string(pid))); err == nil {
						if pgid, err := unix.Getpgid(n); err == nil {
							_ = unix.Kill(-pgid, unix.SIGKILL)
						}
					}
				}
				if !finished {
					_ = cmd.Process.Kill()
					<-waitDone
				}
				_ = slave.Close()
				_ = master.Close()
				<-readDone
			})

			waitUntil(t, 10*time.Second, func() bool {
				return output.has("Enter run")
			})
			if _, err := master.Write([]byte("\r")); err != nil {
				t.Fatal(err)
			}
			waitUntil(t, 10*time.Second, func() bool {
				_, err := os.Stat(ready)
				return err == nil
			})
			test.send(t, cmd.Process, master)
			var waitErr error
			select {
			case waitErr = <-waitDone:
				finished = true
			case <-time.After(10 * time.Second):
				t.Fatal("TUI did not exit after shutdown request")
			}
			if got := cmd.ProcessState.ExitCode(); got != test.code {
				t.Fatalf("exit code = %d, want %d (wait error: %v)", got, test.code, waitErr)
			}
			after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("terminal settings were not restored: before=%+v after=%+v", before, after)
			}
			pidData, err := os.ReadFile(ready + ".pid")
			if err != nil {
				t.Fatal(err)
			}
			pid, _ := strconv.Atoi(strings.TrimSpace(string(pidData)))
			waitUntil(t, 2*time.Second, func() bool { return !liveProcess(pid) })
		})
	}
}

func openTestPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	masterFD, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.IoctlSetInt(masterFD, unix.TIOCSPTLCK, 0); err != nil {
		_ = unix.Close(masterFD)
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(masterFD, unix.TIOCGPTN)
	if err != nil {
		_ = unix.Close(masterFD)
		t.Fatal(err)
	}
	slaveFD, err := unix.Open(fmt.Sprintf("/dev/pts/%d", n), unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = unix.Close(masterFD)
		t.Fatal(err)
	}
	if err := unix.IoctlSetWinsize(masterFD, unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80}); err != nil {
		_ = unix.Close(masterFD)
		_ = unix.Close(slaveFD)
		t.Fatal(err)
	}
	return os.NewFile(uintptr(masterFD), "pty-master"), os.NewFile(uintptr(slaveFD), "pty-slave")
}

func waitUntil(t *testing.T, timeout time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for readiness")
}

func liveProcess(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return false
	}
	fields := strings.Fields(string(data[end+2:]))
	return len(fields) > 0 && fields[0] != "Z"
}
