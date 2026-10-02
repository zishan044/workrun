package main

import (
	"os"

	"github.com/zishan044/workrun/internal/cli"
)

var (
	version = "dev"
	commit  = "unknown"
	builtAt = "unknown"
)

func main() {
	code := cli.Execute(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, cli.BuildInfo{
		Version: version,
		Commit:  commit,
		BuiltAt: builtAt,
	})
	os.Exit(code)
}
