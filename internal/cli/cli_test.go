package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/zishan044/workrun/internal/session"
	"github.com/zishan044/workrun/internal/tui"
)

func TestHelpDoesNotRequireProjectConfig(t *testing.T) {
	for _, args := range [][]string{nil, {"--help"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Execute(args, strings.NewReader(""), &stdout, &stderr, BuildInfo{})
			if code != 0 {
				t.Fatalf("Execute() code = %d, want 0; stderr: %s", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "Usage:") || !strings.Contains(stdout.String(), "version") {
				t.Fatalf("help output missing usage or version command: %s", stdout.String())
			}
		})
	}
}

func TestVersionDoesNotRequireProjectConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	build := BuildInfo{Version: "1.2.3", Commit: "abc", BuiltAt: "today"}
	code := Execute([]string{"version"}, strings.NewReader(""), &stdout, &stderr, build)
	if code != 0 {
		t.Fatalf("Execute() code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "workrun 1.2.3") || !strings.Contains(stdout.String(), "commit: abc") {
		t.Fatalf("version output missing build information: %s", stdout.String())
	}
}

func TestUnknownCommandAndFlagAreUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"unknown"}, {"--unknown"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Execute(args, strings.NewReader(""), &stdout, &stderr, BuildInfo{})
			if code != 2 {
				t.Fatalf("Execute(%v) code = %d, want 2; stderr: %s", args, code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "workrun --help") {
				t.Fatalf("usage hint missing from stderr: %s", stderr.String())
			}
		})
	}
}

func TestRepeatedExecutionDoesNotLeakCommandState(t *testing.T) {
	configA := filepath.Join(t.TempDir(), "a.yaml")
	configB := filepath.Join(t.TempDir(), "b.yaml")
	if err := os.WriteFile(configA, []byte(`version: 1
tasks:
  beta:
    command: ["echo"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configB, []byte(`version: 1
tasks:
  alpha:
    command: ["echo"]
`), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct{ path, want string }{{configA, "beta\n"}, {configB, "alpha\n"}} {
		var stdout, stderr bytes.Buffer
		code := Execute([]string{"--config", test.path, "list"}, strings.NewReader(""), &stdout, &stderr, BuildInfo{})
		if code != 0 {
			t.Fatalf("Execute() code = %d, want 0; stderr: %s", code, stderr.String())
		}
		if stdout.String() != test.want {
			t.Fatalf("stdout = %q, want %q", stdout.String(), test.want)
		}
	}
}

func TestValidateCommandLoadsConfiguration(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "workrun.yaml")
	if err := os.WriteFile(configPath, []byte(`version: 1
tasks:
  test:
    command: ["go", "test", "./..."]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Execute([]string{"--config", configPath, "validate"}, strings.NewReader(""), &stdout, &stderr, BuildInfo{})
	if code != 0 {
		t.Fatalf("validate exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
}

func TestValidateCommandReportsConfigurationErrorsWithoutUsage(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "workrun.yaml")
	if err := os.WriteFile(configPath, []byte("version: 2\ntasks:\n  test:\n    command: [true]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Execute([]string{"--config", configPath, "validate"}, strings.NewReader(""), &stdout, &stderr, BuildInfo{})
	if code != 1 {
		t.Fatalf("validate exit code = %d, want 1; stderr: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "Run 'workrun --help'") {
		t.Fatalf("configuration error should not print usage hint: %s", stderr.String())
	}
}

func TestTUICommandAppearsInHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Execute([]string{"--help"}, strings.NewReader(""), &stdout, &stderr, BuildInfo{})
	if code != 0 || !strings.Contains(stdout.String(), "tui") {
		t.Fatalf("help does not list tui command: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunConfiguredTUILoadsTasksAndPassesLaunchError(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "workrun.yaml")
	if err := os.WriteFile(configPath, []byte(`version: 1
tasks:
  test:
    command: ["go", "test", "./..."]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("terminal unavailable")
	called := false
	err := runConfiguredTUI(configPath, func(model tui.Model, _ *session.Manager, _ context.Context, _ context.CancelCauseFunc, _ *tui.ShutdownState) error {
		called = true
		updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
		model = updated.(tui.Model)
		if !strings.Contains(model.View().Content, "test") {
			t.Fatalf("loaded model does not show configured task: %q", model.View().Content)
		}
		return wantErr
	})
	if !called || !errors.Is(err, wantErr) {
		t.Fatalf("launch callback called=%v, error=%v; want %v", called, err, wantErr)
	}
}

func TestRequireInteractiveTUIRejectsRedirectedStreams(t *testing.T) {
	in, inWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	defer inWriter.Close()
	outReader, out, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer outReader.Close()
	defer out.Close()

	err = requireInteractiveTUI(in, out, "xterm-256color")
	if err == nil || !strings.Contains(err.Error(), "terminal stdin and stdout") || !strings.Contains(err.Error(), "workrun run TASK") {
		t.Fatalf("redirected streams error = %v, want actionable terminal guidance", err)
	}
}

func TestRequireInteractiveTUIRejectsDumbTerminal(t *testing.T) {
	err := requireInteractiveTUI(strings.NewReader(""), &bytes.Buffer{}, "dumb")
	if err == nil || !strings.Contains(err.Error(), "TERM=dumb") || !strings.Contains(err.Error(), "workrun run TASK") {
		t.Fatalf("TERM=dumb error = %v, want actionable terminal guidance", err)
	}
}

func TestTUICommandRejectsRedirectedStreamsWithoutOpeningTTY(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	var stdout, stderr bytes.Buffer
	code := Execute([]string{"--config", filepath.Join(t.TempDir(), "missing.yaml"), "tui"}, strings.NewReader(""), &stdout, &stderr, BuildInfo{})
	if code != 1 || !strings.Contains(stderr.String(), "terminal stdin and stdout") || !strings.Contains(stderr.String(), "workrun run TASK") {
		t.Fatalf("redirected TUI command: code=%d stderr=%q", code, stderr.String())
	}
}

func TestRunConfiguredTUIReportsConfigurationLoadFailure(t *testing.T) {
	err := runConfiguredTUI(filepath.Join(t.TempDir(), "missing.yaml"), func(tui.Model, *session.Manager, context.Context, context.CancelCauseFunc, *tui.ShutdownState) error {
		t.Fatal("launch callback ran after config load failure")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "read configuration") {
		t.Fatalf("runConfiguredTUI config error = %v", err)
	}
}
