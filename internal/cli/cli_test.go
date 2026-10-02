package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
