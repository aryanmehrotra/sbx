package daemon

import (
	"os"
	"testing"
)

// Every test that opens the API would otherwise start a real cross-compile of this module in the
// background (go test's version is "dev"), outliving the test that started it. The one test that
// is about the warm-up replaces this itself.
func TestMain(m *testing.M) {
	warmAgent = func(string) {}

	os.Exit(m.Run())
}
