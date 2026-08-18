package app

// How `sbx connect`'s arguments become deployments.
//
// This layer had no tests, and it is where the worst failure lived: every test of the client
// itself hands Connect a list of endpoints that was already built, so nothing exercised the
// step that builds it from argv. A flag written between two URLs made the second one vanish -
// no error, no mention in the listing, one deployment connected out of two - which is the one
// outcome the client is otherwise careful to make impossible.

import (
	"strings"
	"testing"
)

func TestAURLAfterAFlagIsRefusedRatherThanDropped(t *testing.T) {
	t.Setenv("SBX_CONNECT_TOKEN", "t")

	err := dispatch("connect", []string{
		"db=http://127.0.0.1:1", "--port-offset", "1000", "cache=http://127.0.0.1:2",
	})

	if err == nil {
		t.Fatal("a URL after a flag was accepted, which means it was silently dropped")
	}

	// It has to name the one that would have been lost: "connect took your first URL" is not
	// something anybody notices, and the port it did not open is left to whatever answers there.
	if !strings.Contains(err.Error(), "cache=http://127.0.0.1:2") {
		t.Errorf("error = %q, want it to name the deployment that was about to be ignored", err)
	}

	// And show the line that would have worked, since the fix is an argument order nobody can
	// be expected to guess from "flags go last".
	if !strings.Contains(err.Error(), "sbx connect db=http://127.0.0.1:1 cache=http://127.0.0.1:2") {
		t.Errorf("error = %q, want it to show the corrected command", err)
	}
}

// The ordering that works must keep working, and must carry every deployment through.
func TestEveryURLBeforeTheFlagsIsKept(t *testing.T) {
	t.Setenv("SBX_CONNECT_TOKEN", "t")

	// Nothing is listening on either, so this fails at the fleet fetch - after argv parsing,
	// which is what is under test. Both names in the error is the evidence that both survived.
	err := dispatch("connect", []string{
		"db=http://127.0.0.1:1", "cache=http://127.0.0.1:2", "--port-offset", "1000",
	})

	if err == nil {
		t.Fatal("two unreachable deployments connected")
	}

	for _, want := range []string{"127.0.0.1:1", "127.0.0.1:2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to have tried %s too", err, want)
		}
	}
}

// A flag before any URL at all is the same mistake, and used to reach the generic usage text
// rather than saying what was wrong with the line.
func TestAFlagBeforeEveryURLSaysWhatIsWrong(t *testing.T) {
	t.Setenv("SBX_CONNECT_TOKEN", "t")

	err := dispatch("connect", []string{"--port-offset", "1000", "db=http://127.0.0.1:1"})

	if err == nil {
		t.Fatal("a URL after a leading flag was accepted")
	}

	if !strings.Contains(err.Error(), "came after a flag") {
		t.Errorf("error = %q, want the argument-order explanation", err)
	}
}
