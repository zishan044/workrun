package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/zishan044/workrun/internal/config"
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
	return root
}
