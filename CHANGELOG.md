# Changelog

## v0.1.0 (unreleased)

Initial Linux release:

- Load and strictly validate a versioned YAML task configuration.
- List, validate, and run one configured command from the CLI, or use the TUI to select tasks, view bounded output, cancel work, and run another task.
- Keep CLI stdout and stderr separate; sanitize and bound output shown in the TUI.
- Stop ordinary process-group members on cancellation, timeout, or foreground command completion.

Known limitations: Linux only; no PTY or child stdin forwarding; no persistent history, dependencies, parallel task scheduling, or containment for processes that deliberately leave Workrun's process group. See the [README](README.md) for usage and support details.
