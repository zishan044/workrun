package main

import (
	"runtime/debug"
	"testing"
)

func TestResolvedVersion(t *testing.T) {
	for _, tc := range []struct {
		name        string
		linked      string
		buildInfo   *debug.BuildInfo
		wantVersion string
	}{
		{
			name:        "linker version wins",
			linked:      "0.1.0",
			buildInfo:   &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}},
			wantVersion: "0.1.0",
		},
		{
			name:        "module version fallback",
			linked:      "dev",
			buildInfo:   &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}},
			wantVersion: "v0.1.0",
		},
		{
			name:        "development build fallback",
			linked:      "dev",
			buildInfo:   &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			wantVersion: "dev",
		},
		{
			name:        "empty build metadata fallback",
			wantVersion: "dev",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvedVersion(tc.linked, tc.buildInfo); got != tc.wantVersion {
				t.Fatalf("resolvedVersion() = %q, want %q", got, tc.wantVersion)
			}
		})
	}
}
