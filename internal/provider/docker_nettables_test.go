package provider

import (
	"strings"
	"testing"
)

// A pull on first use prints before the tables; it must not be read as an interface.
func TestParseNetTablesIgnoresWhatDockerPrintsFirst(t *testing.T) {
	out := "Unable to find image 'alpine:3' locally\n3: Pulling from library/alpine\n" +
		netTablesMark + "dev\nInter-| Receive\n face |bytes\n    lo: 1 2\n  eth0: 3 4\n" +
		netTablesMark + "tcp\n  sl  local_address\n   0: 00000000:1F90 00000000:0000 0A\n" +
		netTablesMark + "tcp6\n"

	got, err := parseNetTables(out)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(got.Dev, "Pulling") || !strings.Contains(got.Dev, "eth0:") {
		t.Errorf("dev = %q", got.Dev)
	}

	if !strings.Contains(got.TCP, "00000000:1F90") || strings.Contains(got.TCP, "eth0") {
		t.Errorf("tcp = %q", got.TCP)
	}

	if strings.TrimSpace(got.TCP6) != "" {
		t.Errorf("tcp6 = %q, want empty", got.TCP6)
	}
}

// No tables at all is an error, never an empty answer: an empty dev reads as "no interface".
func TestParseNetTablesRefusesOutputWithoutTables(t *testing.T) {
	if _, err := parseNetTables("sh: cat: not found"); err == nil {
		t.Fatal("parsed tables out of output with none")
	}
}
