// Command helper provides readiness-controlled child processes for runner tests.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "wait":
		ready(os.Args[2])
		blockForever()
	case "spawn-child":
		spawnChild(os.Args[2], os.Args[3], false)
	case "parent-exits":
		spawnChild(os.Args[2], os.Args[3], true)
	case "output":
		fmt.Fprintln(os.Stdout, "stdout-ready")
		fmt.Fprintln(os.Stderr, "stderr-ready")
		ready(os.Args[2])
		blockForever()
	case "exit":
		code := 0
		if len(os.Args) > 2 {
			_, _ = fmt.Sscanf(os.Args[2], "%d", &code)
		}
		os.Exit(code)
	default:
		os.Exit(3)
	}
}

func spawnChild(helper, readyPath string, exitParent bool) {
	childReady := readyPath + ".child"
	cmd := exec.Command(helper, "wait", childReady)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		os.Exit(4)
	}
	if err := waitFile(childReady, 10*time.Second); err != nil {
		_ = cmd.Process.Kill()
		os.Exit(5)
	}
	_ = os.WriteFile(readyPath+".pid", []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0600)
	ready(readyPath)
	if exitParent {
		return
	}
	_ = cmd.Wait()
}

func blockForever() {
	for {
		time.Sleep(time.Hour)
	}
}

func ready(path string) {
	_ = os.MkdirAll(filepath.Dir(path), 0700)
	_ = os.WriteFile(path, []byte("ready\n"), 0600)
}

func waitFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("readiness file %q was not created", path)
}
