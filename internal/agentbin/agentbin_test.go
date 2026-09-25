package agentbin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestExplicitBinaryWins(t *testing.T) {
	t.Setenv("SBX_EXECD_BINARY", "/somewhere/sbx")

	src, err := Locate(context.Background(), "arm64", "dev")
	if err != nil || src.File != "/somewhere/sbx" || src.Image != "" {
		t.Fatalf("Locate = %+v, %v", src, err)
	}
}

func TestFindSourceIsThisCheckout(t *testing.T) {
	dir, ok := FindSource()
	if !ok {
		t.Fatal("go test runs inside the checkout, and it was not found")
	}

	if _, err := os.Stat(filepath.Join(dir, "main.go")); err != nil {
		t.Fatalf("%s is not the module root: %v", dir, err)
	}

	// A go.mod naming another module is not ours, however close it is.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "go.mod"), []byte("module example.com/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if isModuleRoot(other) {
		t.Fatal("another module's root was taken for sbx's")
	}
}
