# Workrun

**A Linux task runner in Go.** Define project commands in YAML, run them from the CLI, or use the Bubble Tea TUI to watch output, cancel a task, and run another.

- Strict config validation and project-relative task directories
- Separate CLI stdout/stderr; bounded, sanitized TUI output
- Process-group cleanup on cancellation, timeout, or foreground command exit
- One task at a time; no PTY, child stdin, dependencies, or run history

## Quick start

Install the [Linux amd64 release](https://github.com/zishan044/workrun/releases/tag/v0.1.0), then run from the extracted archive directory:

```sh
workrun --config examples/workrun.yaml list
workrun --config examples/workrun.yaml run hello
workrun --config examples/workrun.yaml tui
```

The included example needs Go 1.26.3 or newer. You can also install with `go install github.com/zishan044/workrun/cmd/workrun@v0.1.0`. See [CHANGELOG](CHANGELOG.md) for v0.1.0 details.
