package cli

import (
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/hostinfo"
)

// Seen on a Linux box: doctor printed the machine's total and free memory and, on the same row,
// "sbx cannot read memory on linux". The row may only claim it cannot read memory when it did
// not, and must not blame the platform where sbx does support reading it.
func TestMachineRowDoesNotContradictItself(t *testing.T) {
	read := hostinfo.Machine{Cores: 2, MemBytes: 8 << 30, FreeBytes: 3 << 30}

	for _, goos := range []string{"linux", "darwin"} {
		c := machineRow(goos, read)

		if !c.Have || !strings.Contains(c.Detail, "8 GB of memory") || !strings.Contains(c.Detail, "3.0 GB free") {
			t.Errorf("%s, memory read: %+v", goos, c)
		}

		if strings.Contains(c.Meaning, "cannot read") {
			t.Errorf("%s: the row read memory and still says %q", goos, c.Meaning)
		}

		// Unread on a supported platform is a failed read, not an unsupported one.
		c = machineRow(goos, hostinfo.Machine{Cores: 2})
		if c.Have || !strings.Contains(c.Detail, "memory unknown") || c.Meaning == "" ||
			strings.Contains(c.Meaning, "cannot read memory on "+goos) {
			t.Errorf("%s, memory not read: %+v", goos, c)
		}
	}

	c := machineRow("windows", hostinfo.Machine{Cores: 4})
	if c.Have || !strings.Contains(c.Meaning, "cannot read memory on windows") {
		t.Errorf("windows: %+v", c)
	}
}
