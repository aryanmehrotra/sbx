package app

import (
	"strings"
	"testing"
)

// `sbx snapshot --rm` takes exactly one name; anything else is refused before a backend is
// touched, so a typo cannot reach the delete.
func TestSnapshotRmTakesOneName(t *testing.T) {
	for _, args := range [][]string{{"--rm"}, {"--rm", "a", "b"}} {
		err := dispatch("snapshot", args)
		if err == nil || !strings.Contains(err.Error(), "--rm <name>") {
			t.Errorf("sbx snapshot %v = %v, want the one-name usage", args, err)
		}
	}
}
