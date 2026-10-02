package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zishan044/workrun/internal/task"
	"go.yaml.in/yaml/v3"
)

const (
	// MaxConfigBytes bounds memory used to read and parse a project config.
	MaxConfigBytes int64 = 1 << 20
	// MaxTasks bounds validation and the per-session task catalog.
	MaxTasks = 256
)

var taskNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// FileConfig is the supported YAML document shape.
type FileConfig struct {
	Version int                    `yaml:"version"`
	Tasks   map[string]*TaskConfig `yaml:"tasks"`
}

// TaskConfig describes one task in the YAML document.
type TaskConfig struct {
	Description string            `yaml:"description"`
	Command     []string          `yaml:"command"`
	Dir         string            `yaml:"dir"`
	Env         map[string]string `yaml:"env"`
	Timeout     *string           `yaml:"timeout"`
}

// Project owns validated task specifications in alphabetical order.
// Its task values are private so callers cannot mutate the lookup catalog.
type Project struct {
	Path   string
	tasks  []task.Spec
	byName map[string]int
}

// TaskNames returns the sorted task names.
func (p *Project) TaskNames() []string {
	names := make([]string, len(p.tasks))
	for i := range p.tasks {
		names[i] = p.tasks[i].Name
	}
	return names
}

// Tasks returns deep copies of all task specifications in display order.
func (p *Project) Tasks() []task.Spec {
	out := make([]task.Spec, len(p.tasks))
	for i := range p.tasks {
		out[i] = cloneSpec(p.tasks[i])
	}
	return out
}

// Task returns a deep copy of the named specification.
func (p *Project) Task(name string) (task.Spec, bool) {
	i, ok := p.byName[name]
	if !ok {
		return task.Spec{}, false
	}
	return cloneSpec(p.tasks[i]), true
}

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

// Load reads a single size-limited YAML document, rejects unsupported YAML
// features, performs strict typed decoding, validates it, and resolves paths.
func Load(path string) (*Project, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve configuration path: %w", err)
	}

	file, err := os.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("read configuration %q: %w", absPath, err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, MaxConfigBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read configuration %q: %w", absPath, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close configuration %q: %w", absPath, closeErr)
	}
	if int64(len(data)) > MaxConfigBytes {
		return nil, fmt.Errorf("configuration %q exceeds the %d-byte limit", absPath, MaxConfigBytes)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%s: configuration is empty", absPath)
	}

	var root yaml.Node
	astDecoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := astDecoder.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: configuration contains no YAML document", absPath)
		}
		return nil, fmt.Errorf("%s: decode configuration: %w", absPath, err)
	}
	if err := validateYAMLNode(&root); err != nil {
		return nil, fmt.Errorf("%s: %w", absPath, err)
	}
	if err := validateConfigNode(&root); err != nil {
		return nil, fmt.Errorf("%s: %w", absPath, err)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var raw FileConfig
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%s: decode configuration: %w", absPath, err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("%s: configuration must contain exactly one YAML document", absPath)
		}
		return nil, fmt.Errorf("%s: trailing YAML data: %w", absPath, err)
	}

	specs, err := Validate(raw, filepath.Dir(absPath))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", absPath, err)
	}
	project := &Project{
		Path:   absPath,
		tasks:  specs,
		byName: make(map[string]int, len(specs)),
	}
	for i := range project.tasks {
		project.byName[project.tasks[i].Name] = i
	}
	return project, nil
}

// Validate checks schema semantics and resolves task directories against
// baseDir. It does not check whether task executables are installed.
func Validate(raw FileConfig, baseDir string) ([]task.Spec, error) {
	if raw.Version != 1 {
		return nil, fmt.Errorf("unsupported configuration version %d (supported: 1)", raw.Version)
	}
	if len(raw.Tasks) == 0 {
		return nil, errors.New("configuration must define at least one task")
	}
	if len(raw.Tasks) > MaxTasks {
		return nil, fmt.Errorf("configuration defines %d tasks; the limit is %d", len(raw.Tasks), MaxTasks)
	}
	base, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("resolve configuration directory: %w", err)
	}
	base = filepath.Clean(base)

	names := make([]string, 0, len(raw.Tasks))
	for name := range raw.Tasks {
		names = append(names, name)
	}
	sort.Strings(names)

	specs := make([]task.Spec, 0, len(names))
	for _, name := range names {
		taskConfig := raw.Tasks[name]
		if !taskNamePattern.MatchString(name) {
			return nil, fmt.Errorf("task %q: name must match [A-Za-z0-9][A-Za-z0-9_-]*", name)
		}
		if taskConfig == nil {
			return nil, fmt.Errorf("task %q: task definition must not be null", name)
		}
		if len(taskConfig.Command) == 0 {
			return nil, fmt.Errorf("task %q: command must be a non-empty array", name)
		}
		if strings.TrimSpace(taskConfig.Command[0]) == "" {
			return nil, fmt.Errorf("task %q: command executable must not be empty or whitespace", name)
		}
		for i, arg := range taskConfig.Command {
			if strings.IndexByte(arg, 0) >= 0 {
				return nil, fmt.Errorf("task %q: command argument %d contains a NUL byte", name, i)
			}
		}

		dir := base
		if taskConfig.Dir != "" {
			if filepath.IsAbs(taskConfig.Dir) {
				dir = filepath.Clean(taskConfig.Dir)
			} else {
				dir = filepath.Clean(filepath.Join(base, taskConfig.Dir))
			}
		}
		info, err := os.Stat(dir)
		if err != nil {
			return nil, fmt.Errorf("task %q: working directory %q: %w", name, dir, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("task %q: working directory %q is not a directory", name, dir)
		}

		for key, value := range taskConfig.Env {
			if key == "" || strings.Contains(key, "=") || strings.IndexByte(key, 0) >= 0 {
				return nil, fmt.Errorf("task %q: environment key %q is invalid", name, key)
			}
			if strings.IndexByte(value, 0) >= 0 {
				return nil, fmt.Errorf("task %q: environment value for %q contains a NUL byte", name, key)
			}
		}

		var timeout time.Duration
		if taskConfig.Timeout != nil {
			if *taskConfig.Timeout == "" {
				return nil, fmt.Errorf("task %q: timeout must not be empty", name)
			}
			timeout, err = time.ParseDuration(*taskConfig.Timeout)
			if err != nil {
				return nil, fmt.Errorf("task %q: timeout %q is invalid; use a value such as %q: %w", name, *taskConfig.Timeout, "2m", err)
			}
			if timeout <= 0 {
				return nil, fmt.Errorf("task %q: timeout %q must be positive", name, *taskConfig.Timeout)
			}
		}

		specs = append(specs, task.Spec{
			Name:        name,
			Description: taskConfig.Description,
			Argv:        append([]string(nil), taskConfig.Command...),
			Dir:         dir,
			Env:         cloneEnv(taskConfig.Env),
			Timeout:     timeout,
		})
	}
	return specs, nil
}

func cloneEnv(env map[string]string) map[string]string {
	if env == nil {
		return nil
	}
	copy := make(map[string]string, len(env))
	for key, value := range env {
		copy[key] = value
	}
	return copy
}

func validateYAMLNode(root *yaml.Node) error {
	type pendingNode struct {
		node  *yaml.Node
		depth int
	}
	const maxYAMLDepth = 128
	stack := []pendingNode{{node: root}}
	for len(stack) > 0 {
		last := len(stack) - 1
		current := stack[last]
		stack = stack[:last]
		node := current.node
		if node == nil {
			continue
		}
		if current.depth > maxYAMLDepth {
			return fmt.Errorf("line %d: YAML nesting exceeds the %d-level limit", node.Line, maxYAMLDepth)
		}
		if node.Anchor != "" {
			return fmt.Errorf("line %d: YAML anchors are not allowed", node.Line)
		}
		if node.Kind == yaml.AliasNode {
			return fmt.Errorf("line %d: YAML aliases are not allowed", node.Line)
		}
		if node.Kind == yaml.MappingNode {
			if len(node.Content)%2 != 0 {
				return fmt.Errorf("line %d: malformed YAML mapping", node.Line)
			}
			seen := make(map[string]struct{}, len(node.Content)/2)
			for i := 0; i < len(node.Content); i += 2 {
				key := node.Content[i]
				if key.Tag == "!!merge" {
					return fmt.Errorf("line %d: YAML merge keys are not allowed", key.Line)
				}
				if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
					return fmt.Errorf("line %d: mapping keys must be strings", key.Line)
				}
				if _, exists := seen[key.Value]; exists {
					return fmt.Errorf("line %d: duplicate mapping key %q", key.Line, key.Value)
				}
				seen[key.Value] = struct{}{}
			}
		}
		for i := len(node.Content) - 1; i >= 0; i-- {
			stack = append(stack, pendingNode{node: node.Content[i], depth: current.depth + 1})
		}
	}
	return nil
}

func validateConfigNode(document *yaml.Node) error {
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 {
		return fmt.Errorf("line %d: configuration must contain one mapping document", document.Line)
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: configuration must be a mapping", root.Line)
	}
	if version := mappingValue(root, "version"); version != nil && (version.Kind != yaml.ScalarNode || version.Tag != "!!int") {
		return fmt.Errorf("line %d: version must be an integer", version.Line)
	}
	tasks := mappingValue(root, "tasks")
	if tasks == nil {
		return nil // Required-field validation provides the more specific error.
	}
	if tasks.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: tasks must be a mapping", tasks.Line)
	}
	for i := 0; i < len(tasks.Content); i += 2 {
		nameNode, taskNode := tasks.Content[i], tasks.Content[i+1]
		name := nameNode.Value
		if taskNode.Kind != yaml.MappingNode {
			return fmt.Errorf("line %d: task %q must be an object", taskNode.Line, name)
		}
		for j := 0; j < len(taskNode.Content); j += 2 {
			field, value := taskNode.Content[j].Value, taskNode.Content[j+1]
			switch field {
			case "description", "dir":
				if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
					return fmt.Errorf("line %d: task %q field %q must be a string", value.Line, name, field)
				}
			case "command":
				if value.Kind != yaml.SequenceNode {
					return fmt.Errorf("line %d: task %q command must be an array of strings", value.Line, name)
				}
				for _, arg := range value.Content {
					if arg.Kind != yaml.ScalarNode || arg.Tag != "!!str" {
						return fmt.Errorf("line %d: task %q command arguments must be strings", arg.Line, name)
					}
				}
			case "env":
				if value.Kind != yaml.MappingNode {
					return fmt.Errorf("line %d: task %q env must be a mapping", value.Line, name)
				}
				for k := 0; k < len(value.Content); k += 2 {
					envValue := value.Content[k+1]
					if envValue.Kind != yaml.ScalarNode || envValue.Tag != "!!str" {
						return fmt.Errorf("line %d: task %q environment values must be strings", envValue.Line, name)
					}
				}
			case "timeout":
				if value.Kind != yaml.ScalarNode || (value.Tag != "!!str" && value.Tag != "!!null") {
					return fmt.Errorf("line %d: task %q timeout must be a string or null", value.Line, name)
				}
			}
		}
	}
	return nil
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}
