package mcp

import (
	"strings"
	"testing"
)

// "raise ready_timeout_seconds" is advice for one failure: the wait ran out (osbclient.ErrNotReady).
// It was appended to every failed create, so a sandbox the server reported Failed - an image that
// would not start, an agent that could not be placed - told the caller to wait longer for
// something that was never going to answer. Any other failure is returned as it is.
func TestOnlyATimedOutCreateIsToldToRaiseTheTimeout(t *testing.T) {
	f, c := sandboxServer(t)
	f.FailNew = "no linux/arm64 sbx binary to run as the sandbox agent"

	msg := c.fails("sandbox_create", map[string]any{"image": "alpine", "ready_timeout_seconds": 5, "health_check_polling_interval_ms": 10})

	must(t, strings.Contains(msg, "no linux/arm64 sbx binary") && strings.Contains(msg, "Failed"),
		"the server's own reason was lost: %q", msg)
	must(t, !strings.Contains(msg, "ready_timeout_seconds") && !strings.Contains(msg, "removed;"),
		"a Failed sandbox was told to raise ready_timeout_seconds: %q", msg)

	list := obj(c.ok("sandbox_list", map[string]any{}))
	must(t, len(list["sandbox_infos"].([]any)) == 0, "the failed sandbox was left behind: %v", list)
}
