//go:build unix && !(linux && (amd64 || arm64))

package execd

import (
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Off Firecracker's platforms the flag is refused with the reason and exit 2, not ignored: a
// guest agent that silently never listens is a VM whose host waits out every dial.
func TestVsockPortRefusedWithAReason(t *testing.T) {
	_, err := listenVsock(44772)
	if !errors.Is(err, errVsockUnsupported) {
		t.Fatalf("got %v, want errVsockUnsupported", err)
	}

	if !strings.Contains(err.Error(), runtime.GOOS) || !strings.Contains(err.Error(), "--addr") {
		t.Fatalf("the refusal should name this platform and the alternative: %v", err)
	}

	cmd, stderr := startExecd(t, nil, "--addr", "127.0.0.1:0", "--vsock-port", "44772")
	if code := waitExit(t, cmd, 20*time.Second); code != 2 {
		t.Fatalf("exit %d, want 2; stderr:\n%s", code, stderr)
	}

	if !strings.Contains(stderr.String(), "linux/amd64") {
		t.Fatalf("stderr does not say where vsock works:\n%s", stderr)
	}
}
