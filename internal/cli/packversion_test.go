package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func packWith(t *testing.T, build, pin string) (dockerfile string, err error) {
	t.Helper()

	path := specFile(t, `{"version":1,"services":{"db":{"image":"postgres:16","ports":[5432]}}}`)
	out := t.TempDir()

	err = Pack(context.Background(), PackOptions{
		Spec: path, Out: out, Version: build, Pin: pin, Out2: &bytes.Buffer{},
		Inspect: func(string) ([]string, []string, error) { return nil, []string{"postgres"}, nil },
	})
	if err != nil {
		return "", err
	}

	b, rerr := os.ReadFile(filepath.Join(out, "db", "Dockerfile"))
	if rerr != nil {
		t.Fatal(rerr)
	}

	return string(b), nil
}

// The generated image installs sbx with `go install ...@VERSION`, so VERSION must be something
// the module proxy has. A source build wrote `@v0.15.1-dev+ffd872d` - never published, so the
// image did not build. A non-release build refuses and names --version; a release pins itself.
// Never @latest (drifts under a deployment nobody edited) and never @main (the same, later).
func TestPackPinsAReleaseOrRefuses(t *testing.T) {
	df, err := packWith(t, "v0.15.1", "")
	if err != nil || !strings.Contains(df, "github.com/aryanmehrotra/sbx@v0.15.1") {
		t.Fatalf("a release build must pin itself: err %v\n%s", err, df)
	}

	for _, build := range []string{"v0.15.1-dev+ffd872d", "dev", "", "v0.15.1-3-gffd872d"} {
		_, err := packWith(t, build, "")
		if err == nil {
			t.Errorf("build %q: packed with no release to pin", build)
			continue
		}

		if !strings.Contains(err.Error(), "--version") {
			t.Errorf("build %q: the refusal does not say how to pin one: %v", build, err)
		}
	}

	df, err = packWith(t, "v0.15.1-dev+ffd872d", "v0.15.0")
	if err != nil || !strings.Contains(df, "github.com/aryanmehrotra/sbx@v0.15.0") {
		t.Fatalf("--version must pin what it names: err %v\n%s", err, df)
	}

	for _, pin := range []string{"latest", "main", "v0.15.1-dev+ffd872d"} {
		if _, err := packWith(t, "v0.15.1", pin); err == nil {
			t.Errorf("--version %s was accepted; only a release is reproducible", pin)
		}
	}

	for _, line := range strings.Split(df, "\n") {
		if strings.HasPrefix(line, "RUN go install") && (strings.Contains(line, "@latest") || strings.Contains(line, "@main")) {
			t.Errorf("the image installs an unpinned sbx: %s", line)
		}
	}
}

// `go install` stamps nothing, so the packed sbx reported itself as "sbx dev" in every log line
// and `sbx version`. The install passes the same -X main.version the release build does.
func TestThePackedSbxKnowsItsVersion(t *testing.T) {
	df, err := packWith(t, "v0.15.1", "")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(df, `-ldflags "-X main.version=v0.15.1"`) {
		t.Errorf("the install does not stamp the version:\n%s", df)
	}

	// And the comment above it explains the pin it is next to.
	if !strings.Contains(df, "v0.15.1 rather than @latest") {
		t.Errorf("the pin is unexplained:\n%s", df)
	}
}

// The refusal suggested `--version v0.15.1` whatever the build was, so a v0.16 source build was
// told to pack an older sbx than itself. The suggestion is the release the build is based on, or
// a placeholder and the releases page when nothing names one.
func TestPackSuggestsTheReleaseTheBuildIsBasedOn(t *testing.T) {
	for build, want := range map[string]string{
		"v0.16.0-dev+abc1234": "--version v0.16.0",
		"v0.16.0-dev":         "--version v0.16.0",
		"v1.2.3-rc.1":         "--version v1.2.3",
		"v1.2.3-rc1":          "--version v1.2.3",
		"v0.16.0-3-gffd872d":  "--version v0.16.0",
		"dev":                 "--version vX.Y.Z",
		"":                    "--version vX.Y.Z",
		"v0.16.0-beta":        "--version vX.Y.Z",
	} {
		_, err := packWith(t, build, "")
		if err == nil {
			t.Errorf("build %q: packed with no release to pin", build)
			continue
		}

		if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "github.com/aryanmehrotra/sbx/releases") {
			t.Errorf("build %q: refusal %q does not suggest %q and link the releases", build, err, want)
		}

		if build != "" && !strings.Contains(build, "0.15.1") && strings.Contains(err.Error(), "v0.15.1") {
			t.Errorf("build %q: refusal still suggests a hard-coded v0.15.1: %v", build, err)
		}
	}
}
