package main

import (
	"os"
	"runtime/debug"

	"github.com/zishan044/workrun/internal/cli"
)

var (
	version = "dev"
	commit  = "unknown"
	builtAt = "unknown"
)

func main() {
	buildInfo, _ := debug.ReadBuildInfo()
	code := cli.Execute(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, cli.BuildInfo{
		Version: resolvedVersion(version, buildInfo),
		Commit:  commit,
		BuiltAt: builtAt,
	})
	os.Exit(code)
}

func resolvedVersion(linked string, buildInfo *debug.BuildInfo) string {
	if linked != "" && linked != "dev" {
		return linked
	}
	if buildInfo != nil && buildInfo.Main.Version != "" && buildInfo.Main.Version != "(devel)" {
		return buildInfo.Main.Version
	}
	if linked != "" {
		return linked
	}
	return "dev"
}
