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

// A URL after a flag is kept, not dropped and not refused. It used to vanish - one deployment
// connected out of two - and was then refused with an argument-order lesson; now flags go
// anywhere, as on every other command.
func TestAURLAfterAFlagIsKept(t *testing.T) {
	t.Setenv("SBX_CONNECT_TOKEN", "t")

	// Nothing is listening on either, so this fails at the fleet fetch - after argv parsing,
	// which is what is under test. Both addresses in the error prove both survived.
	err := dispatch("connect", []string{
		"db=http://127.0.0.1:1", "--port-offset", "1000", "cache=http://127.0.0.1:2",
	})

	assertTriedBoth(t, err)
}

// "could not reach", not merely the address: the old refusal also quoted both URLs, so
// an address alone would pass against the code that dropped one.
func assertTriedBoth(t *testing.T, err error) {
	t.Helper()

	if err == nil {
		t.Fatal("two unreachable deployments connected")
	}

	for _, want := range []string{"could not reach http://127.0.0.1:1", "could not reach http://127.0.0.1:2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to have tried %s too", err, want)
		}
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

// A flag before every URL is the way most commands are typed, and keeps them all.
func TestAFlagBeforeEveryURLKeepsThemAll(t *testing.T) {
	t.Setenv("SBX_CONNECT_TOKEN", "t")

	err := dispatch("connect", []string{"--port-offset", "1000", "db=http://127.0.0.1:1", "cache=http://127.0.0.1:2"})

	assertTriedBoth(t, err)
}
