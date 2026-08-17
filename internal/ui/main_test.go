package ui

// Where this package's tests read their journal from.
//
// The dashboard refreshes by reading the journal, and every test that presses a key causes one -
// wake and sleep both end in a refresh. Left alone that is the developer's own
// ~/.sbx/history.jsonl, which on the machine this was found on had 9,639 lines in it, read again
// by every goroutine a keypress spawns. TestHoldingAKeyDown presses every key forty times in
// every state, so the package came to want five gigabytes under -race and was killed for it.
//
// A unit test must not read the machine it is running on. This is the same fault as the fixture
// that dialled 127.0.0.1:20000 and woke somebody's containers, in a second place.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "sbx-ui-history")
	if err != nil {
		panic(err)
	}

	if err := os.Setenv("SBX_HISTORY", filepath.Join(dir, "history.jsonl")); err != nil {
		panic(err)
	}

	code := m.Run()

	_ = os.RemoveAll(dir)

	os.Exit(code)
}
