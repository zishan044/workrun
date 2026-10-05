package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zishan044/workrun/internal/task"
)

const helperMarker = "WORKRUN_RUNNER_HELPER"

func TestRunnerProcessHelper(t *testing.T) {
	if os.Getenv(helperMarker) != "1" {
		return
	}

	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		os.Exit(90)
	}
	args := os.Args[separator+1:]
	switch args[0] {
	case "args":
		_ = json.NewEncoder(os.Stdout).Encode(args[1:])
	case "cwd":
		wd, err := os.Getwd()
		if err != nil {
			os.Exit(91)
		}
		fmt.Fprintln(os.Stdout, wd)
	case "env":
		fmt.Fprint(os.Stdout, os.Getenv(args[1]))
	case "streams":
		fmt.Fprint(os.Stdout, "stdout-data")
		fmt.Fprint(os.Stderr, "stderr-data")
	case "exit":
		code := 0
		if len(args) > 1 {
			_, _ = fmt.Sscanf(args[1], "%d", &code)
		}
		os.Exit(code)
	case "output":
		fmt.Fprint(os.Stdout, "output-data")
	default:
		os.Exit(92)
	}
	os.Exit(0)
}

func TestRunForwardsArgumentsWithoutShellProcessing(t *testing.T) {
	want := []string{"has spaces", "", "$HOME", "*.go"}
	var stdout bytes.Buffer
	result := New().Run(context.Background(), helperSpec("args", want...), &stdout, io.Discard)
	if result.Status != Succeeded {
		t.Fatalf("Run status = %q, want %q (err: %v)", result.Status, Succeeded, result.Err)
	}
	var got []string
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode child arguments: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("child arguments = %#v, want %#v", got, want)
	}
}

func TestRunUsesTaskWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	var stdout bytes.Buffer
	result := New().Run(context.Background(), helperSpecIn(dir, "cwd"), &stdout, io.Discard)
	if result.Status != Succeeded {
		t.Fatalf("Run status = %q, want %q (err: %v)", result.Status, Succeeded, result.Err)
	}
	if got := strings.TrimSpace(stdout.String()); got != dir {
		t.Fatalf("child working directory = %q, want %q", got, dir)
	}
}

func TestRunMergesEnvironmentAndPreservesEmptyOverride(t *testing.T) {
	const inheritedKey = "WORKRUN_RUNNER_INHERITED_TEST"
	t.Setenv(inheritedKey, "inherited-value")
	for _, tc := range []struct {
		name string
		env  map[string]string
		key  string
		want string
	}{
		{name: "inherited value retained", key: inheritedKey, want: "inherited-value"},
		{name: "empty override retained", env: map[string]string{"WORKRUN_RUNNER_EMPTY_TEST": ""}, key: "WORKRUN_RUNNER_EMPTY_TEST", want: ""},
		{name: "override replaces inherited", env: map[string]string{inheritedKey: "replacement"}, key: inheritedKey, want: "replacement"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := helperSpec("env", tc.key)
			spec.Env = tc.env
			var stdout bytes.Buffer
			result := New().Run(context.Background(), spec, &stdout, io.Discard)
			if result.Status != Succeeded {
				t.Fatalf("Run status = %q, want %q (err: %v)", result.Status, Succeeded, result.Err)
			}
			if got := stdout.String(); got != tc.want {
				t.Fatalf("child environment value = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunTaskPATHOverrideDoesNotChangeExecutableLookup(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(executable))

	spec := helperSpec("env", "PATH")
	spec.Argv[0] = filepath.Base(executable)
	spec.Env["PATH"] = "child-only-path"
	var stdout bytes.Buffer
	result := New().Run(context.Background(), spec, &stdout, io.Discard)
	if result.Status != Succeeded {
		t.Fatalf("Run status = %q, want %q (err: %v)", result.Status, Succeeded, result.Err)
	}
	if got := stdout.String(); got != "child-only-path" {
		t.Fatalf("child PATH = %q, want task override", got)
	}
}

func TestRunKeepsOutputStreamsSeparate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	result := New().Run(context.Background(), helperSpec("streams"), &stdout, &stderr)
	if result.Status != Succeeded {
		t.Fatalf("Run status = %q, want %q (err: %v)", result.Status, Succeeded, result.Err)
	}
	if stdout.String() != "stdout-data" {
		t.Errorf("stdout = %q, want %q", stdout.String(), "stdout-data")
	}
	if stderr.String() != "stderr-data" {
		t.Errorf("stderr = %q, want %q", stderr.String(), "stderr-data")
	}
}

func TestRunClassifiesSuccessfulAndNonzeroExit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		code     string
		status   Status
		exitCode int
	}{
		{name: "success", code: "0", status: Succeeded, exitCode: 0},
		{name: "nonzero", code: "7", status: Failed, exitCode: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := New().Run(context.Background(), helperSpec("exit", tc.code), io.Discard, io.Discard)
			if result.Status != tc.status {
				t.Fatalf("Run status = %q, want %q (err: %v)", result.Status, tc.status, result.Err)
			}
			if result.ExitCode != tc.exitCode {
				t.Errorf("exit code = %d, want %d", result.ExitCode, tc.exitCode)
			}
		})
	}
}

func TestRunMissingExecutableIsStartFailure(t *testing.T) {
	spec := task.Spec{Name: "missing", Argv: []string{filepath.Join(t.TempDir(), "does-not-exist")}}
	result := New().Run(context.Background(), spec, io.Discard, io.Discard)
	if result.Status != StartFailed {
		t.Fatalf("Run status = %q, want %q", result.Status, StartFailed)
	}
	if result.ProcessStarted || result.ExitCode != -1 || result.Err == nil {
		t.Errorf("unexpected start-failure result: %+v", result)
	}
}

func TestRunOutputWriterFailureIsRunnerError(t *testing.T) {
	result := New().Run(context.Background(), helperSpec("output"), failingWriter{}, io.Discard)
	if result.Status != RunnerError {
		t.Fatalf("Run status = %q, want %q (err: %v)", result.Status, RunnerError, result.Err)
	}
	if !result.OutputIncomplete || result.Err == nil {
		t.Errorf("output failure was not retained: %+v", result)
	}
}

func TestRunShortOutputWriteIsRunnerError(t *testing.T) {
	result := New().Run(context.Background(), helperSpec("output"), shortWriter{}, io.Discard)
	if result.Status != RunnerError {
		t.Fatalf("Run status = %q, want %q (err: %v)", result.Status, RunnerError, result.Err)
	}
	if !result.OutputIncomplete || result.Err == nil {
		t.Errorf("short output write was not retained: %+v", result)
	}
}

func TestRunAlreadyCancelledContextDoesNotStartProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := New().Run(ctx, helperSpec("exit", "0"), io.Discard, io.Discard)
	if result.Status != Cancelled || result.ProcessStarted {
		t.Fatalf("Run result = %+v, want cancelled without a started process", result)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("capture failed") }

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func helperSpec(mode string, args ...string) task.Spec {
	return helperSpecIn("", mode, args...)
}

func helperSpecIn(dir, mode string, args ...string) task.Spec {
	executable, err := os.Executable()
	if err != nil {
		panic(err)
	}
	argv := []string{executable, "-test.run=^TestRunnerProcessHelper$", "--", mode}
	argv = append(argv, args...)
	return task.Spec{
		Name: "runner-test-helper",
		Argv: argv,
		Dir:  dir,
		Env:  map[string]string{helperMarker: "1"},
	}
}
