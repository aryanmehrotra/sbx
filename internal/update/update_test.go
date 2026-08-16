package update

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestComparingVersions(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
		why             string
	}{
		{"v0.1.0", "v0.2.0", true, "a newer minor"},
		{"v0.1.0", "v0.1.1", true, "a newer patch"},
		{"v0.9.9", "v1.0.0", true, "a newer major"},
		{"v0.2.0", "v0.1.0", false, "an older release is not an update"},
		{"v0.1.0", "v0.1.0", false, "the same release is not an update"},
		{"0.1.0", "0.2.0", true, "the v is optional"},

		// The one that matters most: somebody running a binary they built from source must
		// never be advised to go and download an older one.
		{"dev", "v9.9.9", false, "a development build is never out of date"},

		{"v0.1.0", "", false, "nothing known"},
		{"", "v0.1.0", false, "no current version"},
		{"v0.1.0", "v0.2.0-rc1", false, "a pre-release is not offered"},
		{"v0.1.0", "not-a-version", false, "garbage is not an update"},
		{"v0.1", "v0.2", false, "a two-part version is refused rather than guessed at"},
	}

	for _, c := range cases {
		if got := Newer(c.current, c.latest); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v - %s", c.current, c.latest, got, c.want, c.why)
		}
	}
}

func seed(t *testing.T, c cache) {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("HOME", dir)

	if err := os.MkdirAll(filepath.Join(dir, ".sbx"), 0o700); err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, ".sbx", "update.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAvailableReadsTheCache(t *testing.T) {
	t.Setenv("SBX_NO_UPDATE_CHECK", "")
	t.Setenv("CI", "")
	t.Setenv("GITHUB_ACTIONS", "")

	seed(t, cache{Latest: "v0.2.0", CheckedAt: time.Now()})

	if got := Available("v0.1.0"); got != "v0.2.0" {
		t.Errorf("Available = %q, want v0.2.0", got)
	}

	if got := Available("v0.2.0"); got != "" {
		t.Errorf("Available said %q while already on the latest", got)
	}
}

// The check must be silent where it is turned off, and where nobody is watching.
func TestTheCheckCanBeTurnedOff(t *testing.T) {
	t.Setenv("CI", "")
	t.Setenv("GITHUB_ACTIONS", "")

	seed(t, cache{Latest: "v9.9.9", CheckedAt: time.Now()})

	if Available("v0.1.0") == "" {
		t.Fatal("the fixture is wrong: it should report an update before being disabled")
	}

	t.Setenv("SBX_NO_UPDATE_CHECK", "1")

	if got := Available("v0.1.0"); got != "" {
		t.Errorf("SBX_NO_UPDATE_CHECK=1 still reported %q", got)
	}
}

func TestNoCheckInCI(t *testing.T) {
	t.Setenv("SBX_NO_UPDATE_CHECK", "")

	seed(t, cache{Latest: "v9.9.9", CheckedAt: time.Now()})

	for _, k := range []string{"CI", "GITHUB_ACTIONS", "GITLAB_CI"} {
		t.Run(k, func(t *testing.T) {
			t.Setenv(k, "true")

			if got := Available("v0.1.0"); got != "" {
				t.Errorf("with %s set it still reported %q - a build pipeline is not somebody "+
					"who wants to be told about a release", k, got)
			}
		})
	}
}

// A cache that cannot be parsed, or is not there, must be silence rather than a crash.
func TestABrokenCacheIsSilent(t *testing.T) {
	t.Setenv("SBX_NO_UPDATE_CHECK", "")
	t.Setenv("CI", "")
	t.Setenv("GITHUB_ACTIONS", "")

	dir := t.TempDir()
	t.Setenv("HOME", dir)

	if got := Available("v0.1.0"); got != "" {
		t.Errorf("with no cache at all it reported %q", got)
	}

	if err := os.MkdirAll(filepath.Join(dir, ".sbx"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, ".sbx", "update.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := Available("v0.1.0"); got != "" {
		t.Errorf("with a corrupt cache it reported %q", got)
	}
}

// Refresh is fire-and-forget by design. What must be true is that calling it does not block
// the caller, because it is called from a path that draws frames.
func TestRefreshDoesNotBlock(t *testing.T) {
	t.Setenv("SBX_NO_UPDATE_CHECK", "")
	t.Setenv("CI", "")
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("HOME", t.TempDir())

	done := make(chan struct{})

	go func() {
		Refresh("aryanmehrotra/sbx")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Refresh blocked, so a dashboard calling it would stutter on a slow network")
	}
}

// A fresh cache must not trigger another request.
func TestAFreshCacheIsNotRefreshed(t *testing.T) {
	seed(t, cache{Latest: "v0.2.0", CheckedAt: time.Now()})

	if stale() {
		t.Error("a cache written just now was considered stale")
	}

	seed(t, cache{Latest: "v0.2.0", CheckedAt: time.Now().Add(-2 * Interval)})

	if !stale() {
		t.Error("a cache from two intervals ago was not considered stale")
	}
}
