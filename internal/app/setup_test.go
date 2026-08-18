package app

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain injects the built-in specs from disk, standing in for the embed that the root
// package hands to Main at runtime. The path is made absolute because some tests change the
// working directory, and os.DirFS resolves a relative root at open time - "../.." would then
// point somewhere else. Absolute, it always names the repo root, and the "examples/..." paths
// the template code reads resolve exactly as the embed's do.
func TestMain(m *testing.M) {
	root, err := filepath.Abs("../..")
	if err != nil {
		panic(err)
	}

	templates = os.DirFS(root)

	os.Exit(m.Run())
}
