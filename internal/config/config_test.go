package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workrun.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValidConfigSortsTaskNames(t *testing.T) {
	path := writeConfig(t, `version: 1
tasks:
  zebra:
    command: ["echo", "z"]
  alpha:
    description: first
    command: ["echo", "a"]
`)
	project, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if want := []string{"alpha", "zebra"}; !reflect.DeepEqual(project.TaskNames(), want) {
		t.Fatalf("TaskNames() = %v, want %v", project.TaskNames(), want)
	}
}

func TestLoadRejectsMalformedDocuments(t *testing.T) {
	tests := []struct {
		name, contents, want string
	}{
		{"unknown field", "version: 1\ntasks: {}\nunknown: true\n", "field unknown not found"},
		{"missing command", "version: 1\ntasks:\n  test:\n    description: missing command\n", `task "test": command must be a non-empty array`},
		{"second document", "version: 1\ntasks:\n  test:\n    command: ['true']\n---\nversion: 1\n", "exactly one YAML document"},
		{"trailing malformed YAML", "version: 1\ntasks:\n  test:\n    command: ['true']\n---\n: invalid\n", "trailing YAML data"},
		{"empty file", "", "configuration is empty"},
		{"whitespace only", "  \n \n", "no YAML document"},
		{"null argument", "version: 1\ntasks:\n  test:\n    command: ['true', null]\n", "command arguments must be strings"},
		{"numeric description", "version: 1\ntasks:\n  test:\n    description: 12\n    command: ['true']\n", "field \"description\" must be a string"},
		{"numeric env value", "version: 1\ntasks:\n  test:\n    command: ['true']\n    env: {KEY: 12}\n", "environment values must be strings"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, tt.contents)
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestLoadRejectsOversizedFile(t *testing.T) {
	path := writeConfig(t, strings.Repeat(" ", int(MaxConfigBytes+1)))
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "exceeds the") {
		t.Fatalf("Load() error = %v, want size-limit error", err)
	}
}

func TestLoadRejectsYAMLFeatures(t *testing.T) {
	tests := []struct {
		name, contents, want string
	}{
		{"duplicate key", "version: 1\ntasks:\n  test:\n    command: ['true']\n    command: ['false']\n", "duplicate mapping key"},
		{"anchor", "version: 1\ntasks:\n  test:\n    command: &cmd [true]\n", "anchors are not allowed"},
		{"alias", "version: 1\ntasks:\n  test:\n    command: &cmd ['true']\n  other:\n    command: *cmd\n", "not allowed"},
		{"merge key", "version: 1\ntasks:\n  test:\n    <<: {command: ['true']}\n", "merge keys are not allowed"},
		{"numeric key", "version: 1\ntasks:\n  12:\n    command: ['true']\n", "mapping keys must be strings"},
		{"sequence key", "version: 1\ntasks:\n  ? [nested]\n  : {command: ['true']}\n", "mapping keys must be strings"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, tt.contents)
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestValidateSchemaAndResolution(t *testing.T) {
	base := t.TempDir()
	relativeDir := filepath.Join(base, "src")
	if err := os.Mkdir(relativeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	timeout := "500ms"
	specs, err := Validate(FileConfig{
		Version: 1,
		Tasks: map[string]*TaskConfig{
			"test": {
				Description: "check",
				Command:     []string{"go", "test", "", "./..."},
				Dir:         "src",
				Env:         map[string]string{"EMPTY": "", "LITERAL": "$HOME/bin"},
				Timeout:     &timeout,
			},
		},
	}, base)
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if len(specs) != 1 || specs[0].Dir != relativeDir || specs[0].Timeout != 500*time.Millisecond {
		t.Fatalf("Validate() specs = %#v, want resolved dir %q and 500ms timeout", specs, relativeDir)
	}
	if !reflect.DeepEqual(specs[0].Argv, []string{"go", "test", "", "./..."}) {
		t.Fatalf("Argv = %#v, want empty argument preserved", specs[0].Argv)
	}
	if specs[0].Env["EMPTY"] != "" || specs[0].Env["LITERAL"] != "$HOME/bin" {
		t.Fatalf("Env = %#v, want empty/literal values preserved", specs[0].Env)
	}
}

func TestValidateRejectsInvalidSchema(t *testing.T) {
	base := t.TempDir()
	tests := []struct {
		name string
		raw  FileConfig
		want string
	}{
		{"unsupported version", FileConfig{Version: 2, Tasks: map[string]*TaskConfig{"test": {Command: []string{"true"}}}}, "unsupported configuration version"},
		{"no tasks", FileConfig{Version: 1}, "at least one task"},
		{"null task", FileConfig{Version: 1, Tasks: map[string]*TaskConfig{"test": nil}}, "must not be null"},
		{"invalid task name", FileConfig{Version: 1, Tasks: map[string]*TaskConfig{"bad name": {Command: []string{"true"}}}}, "name must match"},
		{"missing command", FileConfig{Version: 1, Tasks: map[string]*TaskConfig{"test": {}}}, "command must be a non-empty array"},
		{"whitespace executable", FileConfig{Version: 1, Tasks: map[string]*TaskConfig{"test": {Command: []string{"  "}}}}, "must not be empty or whitespace"},
		{"NUL argument", FileConfig{Version: 1, Tasks: map[string]*TaskConfig{"test": {Command: []string{"true", "bad\x00arg"}}}}, "NUL byte"},
		{"missing directory", FileConfig{Version: 1, Tasks: map[string]*TaskConfig{"test": {Command: []string{"true"}, Dir: "missing"}}}, "working directory"},
		{"directory is file", FileConfig{Version: 1, Tasks: map[string]*TaskConfig{"test": {Command: []string{"true"}, Dir: "workrun.yaml"}}}, "is not a directory"},
		{"empty env key", FileConfig{Version: 1, Tasks: map[string]*TaskConfig{"test": {Command: []string{"true"}, Env: map[string]string{"": "value"}}}}, "environment key"},
		{"invalid env key", FileConfig{Version: 1, Tasks: map[string]*TaskConfig{"test": {Command: []string{"true"}, Env: map[string]string{"BAD=KEY": "value"}}}}, "environment key"},
		{"NUL env value", FileConfig{Version: 1, Tasks: map[string]*TaskConfig{"test": {Command: []string{"true"}, Env: map[string]string{"KEY": "bad\x00value"}}}}, "NUL byte"},
	}
	if err := os.WriteFile(filepath.Join(base, "workrun.yaml"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Validate(tt.raw, base); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestValidateTimeoutRules(t *testing.T) {
	for _, tt := range []struct {
		value *string
		want  time.Duration
		bad   bool
	}{
		{nil, 0, false},
		{stringPointer(""), 0, true},
		{stringPointer("two minutes"), 0, true},
		{stringPointer("0s"), 0, true},
		{stringPointer("-1s"), 0, true},
		{stringPointer("2m"), 2 * time.Minute, false},
	} {
		raw := FileConfig{Version: 1, Tasks: map[string]*TaskConfig{"test": {Command: []string{"true"}, Timeout: tt.value}}}
		specs, err := Validate(raw, t.TempDir())
		if tt.bad {
			if err == nil {
				t.Fatalf("Validate(timeout %v) succeeded, want error", tt.value)
			}
			continue
		}
		if err != nil || specs[0].Timeout != tt.want {
			t.Fatalf("Validate(timeout %v) = (%#v, %v), want duration %s", tt.value, specs, err, tt.want)
		}
	}
}

func TestLoadResolvesTaskDirRelativeToConfig(t *testing.T) {
	projectDir := t.TempDir()
	configDir := filepath.Join(projectDir, "config")
	sourceDir := filepath.Join(projectDir, "src")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "workrun.yaml")
	contents := "version: 1\ntasks:\n  test:\n    command: ['true']\n    dir: ../src\n"
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	project, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	spec, ok := project.Task("test")
	if !ok || spec.Dir != sourceDir {
		t.Fatalf("Task(test).Dir = %q, %v; want %q", spec.Dir, ok, sourceDir)
	}
}

func TestLoadUsesRequestedConfigSymlinkDirectory(t *testing.T) {
	requestedDir := t.TempDir()
	targetDir := t.TempDir()
	targetPath := filepath.Join(targetDir, "target.yaml")
	if err := os.WriteFile(targetPath, []byte("version: 1\ntasks:\n  test:\n    command: ['true']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(requestedDir, "link.yaml")
	if err := os.Symlink(targetPath, linkPath); err != nil {
		t.Skipf("cannot create config symlink: %v", err)
	}
	project, err := Load(linkPath)
	if err != nil {
		t.Fatalf("Load() through symlink error = %v", err)
	}
	spec, ok := project.Task("test")
	if !ok || spec.Dir != requestedDir {
		t.Fatalf("Task(test).Dir = %q, %v; want requested config directory %q", spec.Dir, ok, requestedDir)
	}
}

func TestProjectTaskAccessorsReturnCopies(t *testing.T) {
	path := writeConfig(t, `version: 1
tasks:
  test:
    command: ["echo", "original"]
    env: {KEY: original}
`)
	project, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	first, ok := project.Task("test")
	if !ok {
		t.Fatal("Task(test) not found")
	}
	first.Argv[1] = "changed"
	first.Env["KEY"] = "changed"
	second, ok := project.Task("test")
	if !ok || second.Argv[1] != "original" || second.Env["KEY"] != "original" {
		t.Fatalf("task lookup was mutated through returned value: %#v", second)
	}
	names := project.TaskNames()
	names[0] = "changed"
	if project.TaskNames()[0] != "test" {
		t.Fatal("TaskNames returned internal storage")
	}
}

func TestValidateTaskLimit(t *testing.T) {
	tasks := make(map[string]*TaskConfig, MaxTasks+1)
	for i := 0; i < MaxTasks+1; i++ {
		tasks[fmt.Sprintf("t%d", i)] = &TaskConfig{Command: []string{"true"}}
	}
	if _, err := Validate(FileConfig{Version: 1, Tasks: tasks}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "limit is") {
		t.Fatalf("Validate() error = %v, want task-limit error", err)
	}
}

func stringPointer(s string) *string { return &s }
