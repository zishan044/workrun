package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"github.com/zishan044/workrun/internal/config"
	"github.com/zishan044/workrun/internal/runner"
	"github.com/zishan044/workrun/internal/session"
	"github.com/zishan044/workrun/internal/tui"
)

// BuildInfo contains values injected by the release build.
type BuildInfo struct {
	Version string
	Commit  string
	BuiltAt string
}

type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

func usage(err error) error { return usageError{err: err} }

type exitStatusError struct {
	code int
	err  error
}

func (e *exitStatusError) Error() string {
	if e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *exitStatusError) Unwrap() error { return e.err }

func noArgs(command string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != 0 {
			return usage(fmt.Errorf("%s accepts no arguments", command))
		}
		return nil
	}
}

// Execute constructs a fresh command tree, runs it, and maps errors to process
// exit codes. A fresh tree keeps flags and arguments isolated between calls.
func Execute(args []string, stdin io.Reader, stdout, stderr io.Writer, build BuildInfo) int {
	root := newRoot(stdin, stdout, stderr, build)
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		return 0
	}
	var exitErr *exitStatusError
	if errors.As(err, &exitErr) {
		if exitErr.err != nil {
			_, _ = fmt.Fprintf(stderr, "workrun: %v\n", exitErr.err)
		}
		return exitErr.code
	}

	var usageErr usageError
	code := 1
	if errors.As(err, &usageErr) || strings.Contains(err.Error(), "unknown command") {
		code = 2
	}
	_, _ = fmt.Fprintf(stderr, "workrun: %v\n", err)
	if code == 2 {
		_, _ = fmt.Fprintln(stderr, "Run 'workrun --help' for usage.")
	}
	return code
}

func newRoot(stdin io.Reader, stdout, stderr io.Writer, build BuildInfo) *cobra.Command {
	var configPath string
	root := &cobra.Command{
		Use:               "workrun",
		Short:             "Run development tasks from a YAML configuration",
		SilenceErrors:     true,
		SilenceUsage:      true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usage(err) })
	root.PersistentFlags().StringVar(&configPath, "config", "workrun.yaml", "path to the task configuration")

	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print build information",
		Args:  noArgs("version"),
		Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "workrun %s\ncommit: %s\nbuilt: %s\n", build.Version, build.Commit, build.BuiltAt)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "validate",
		Short: "Validate the task configuration",
		Args:  noArgs("validate"),
		RunE: func(_ *cobra.Command, _ []string) error {
			_, err := config.Load(configPath)
			return err
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List configured tasks",
		Args:  noArgs("list"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			project, err := config.Load(configPath)
			if err != nil {
				return err
			}
			for _, spec := range project.Tasks() {
				line := spec.Name
				if spec.Description != "" {
					line += "\t" + spec.Description
				}
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), line); err != nil {
					return fmt.Errorf("write task list: %w", err)
				}
			}
			return nil
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "tui",
		Short: "Open the interactive task runner",
		Args:  noArgs("tui"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runConfiguredTUI(configPath, func(model tui.Model, manager *session.Manager, appCtx context.Context, cancelApp context.CancelCauseFunc, shutdown *tui.ShutdownState) error {
				program := tea.NewProgram(model,
					tea.WithInput(cmd.InOrStdin()),
					tea.WithOutput(cmd.OutOrStdout()),
					tea.WithoutSignalHandler(),
				)
				signals := make(chan os.Signal, 1)
				signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
				reason, runErr := runTUIProgram(program, manager, cancelApp, shutdown, signals, func() { signal.Stop(signals) })
				if runErr != nil {
					return &exitStatusError{code: tuiExitCode(reason, runErr), err: fmt.Errorf("run TUI: %w", runErr)}
				}
				if code := tuiExitCode(reason, nil); code != 0 {
					return &exitStatusError{code: code}
				}
				return nil
			})
		},
	})
	return root
}

func runConfiguredTUI(configPath string, launch func(tui.Model, *session.Manager, context.Context, context.CancelCauseFunc, *tui.ShutdownState) error) error {
	project, err := config.Load(configPath)
	if err != nil {
		return err
	}
	manager := session.NewManager(runner.New())
	appCtx, cancelApp := context.WithCancelCause(context.Background())
	defer cancelApp(nil)
	shutdown := &tui.ShutdownState{}
	model := tui.NewModel(project.Path, project.Tasks(), manager, appCtx, shutdown)
	return launch(model, manager, appCtx, cancelApp, shutdown)
}

type teaProgram interface {
	Run() (tea.Model, error)
	Send(tea.Msg)
}

func runTUIProgram(program teaProgram, manager *session.Manager, cancelApp context.CancelCauseFunc, shutdown *tui.ShutdownState, signals <-chan os.Signal, stopSignals func()) (runner.StopReason, error) {
	if stopSignals == nil {
		stopSignals = func() {}
	}
	defer stopSignals()
	stopWatcher := make(chan struct{})
	watcherDone := make(chan struct{})
	handleSignal := func(sig os.Signal) {
		reason, cause := runner.StopNone, error(nil)
		switch sig {
		case syscall.SIGINT:
			reason, cause = runner.StopSIGINT, runner.ErrSIGINT
		case syscall.SIGTERM:
			reason, cause = runner.StopSIGTERM, runner.ErrSIGTERM
		default:
			return
		}
		effective, first := shutdown.Request(reason)
		if first {
			cancelApp(cause)
			go program.Send(tui.ShutdownRequestedMsg{Reason: effective})
		}
	}
	go func() {
		defer close(watcherDone)
		for {
			select {
			case sig := <-signals:
				handleSignal(sig)
			case <-stopWatcher:
				for {
					select {
					case sig := <-signals:
						handleSignal(sig)
					default:
						return
					}
				}
			}
		}
	}()
	_, runErr := program.Run()
	cleanupErr := manager.CloseAndWait()
	stopSignals()
	close(stopWatcher)
	<-watcherDone
	cancelApp(nil)
	return shutdown.Reason(), errors.Join(runErr, cleanupErr)
}

func tuiExitCode(reason runner.StopReason, runErr error) int {
	if runErr != nil {
		return 1
	}
	switch reason {
	case runner.StopSIGINT:
		return 130
	case runner.StopSIGTERM:
		return 143
	default:
		return 0
	}
}
