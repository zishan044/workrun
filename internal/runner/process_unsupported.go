//go:build !linux

package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func processAttributes() *syscall.SysProcAttr { return nil }

func processExitDetails(state *os.ProcessState) (int, int) { return state.ExitCode(), 0 }

func supervise(context.Context, *exec.Cmd, <-chan error) processOutcome {
	return processOutcome{err: fmt.Errorf("%w", ErrUnsupportedPlatform), startedAt: time.Time{}}
}
