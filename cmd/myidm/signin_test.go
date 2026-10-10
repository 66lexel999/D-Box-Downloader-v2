package main

import (
	"path/filepath"
	"slices"
	"testing"

	"myidm/internal/config"
)

// TestSignInArgs: the sign-in launch gets exactly the non-default -listen and
// -data of the D BOX the user runs, nothing for a default launch.
func TestSignInArgs(t *testing.T) {
	def := config.Default()
	if got := signInArgs(def); len(got) != 0 {
		t.Fatalf("default launch: %q, want no flags", got)
	}
	custom := config.Default()
	custom.Listen = "127.0.0.1:9090"
	custom.DataDir = filepath.Join(t.TempDir(), "my data")
	want := []string{"-listen=127.0.0.1:9090", "-data=" + custom.DataDir}
	if got := signInArgs(custom); !slices.Equal(got, want) {
		t.Fatalf("custom launch: %q, want %q", got, want)
	}
}
