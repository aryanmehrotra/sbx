package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// snapStub lists fixed units and fixed snapshot images; nothing else of the provider is used.
type snapStub struct {
	provider.Provider

	units  []provider.Unit
	images []string
}

func (s *snapStub) Name() string  { return "docker" }
func (s *snapStub) Where() string { return "unix:///a.sock" }

func (s *snapStub) List(context.Context, string) ([]provider.Unit, error) { return s.units, nil }

func (s *snapStub) Images(_ context.Context, prefix string) ([]string, error) {
	var out []string

	for _, img := range s.images {
		if strings.HasPrefix(img, prefix) {
			out = append(out, img)
		}
	}

	return out, nil
}

func (s *snapStub) Commit(context.Context, string, string, ...string) error { return nil }
func (s *snapStub) CopyVolume(context.Context, string, string) error        { return nil }
func (s *snapStub) VolumeFor(string, string) string                         { return "" }
func (s *snapStub) RemoveImage(context.Context, string) error               { return nil }
func (s *snapStub) RemoveVolume(context.Context, string) error              { return nil }

// origins writes a record for each name under a fresh HOME and returns their paths.
func origins(t *testing.T, names ...string) map[string]string {
	return originsOn(t, &snapStub{}, names...)
}

// originsOn is origins written through p, as create writes them.
func originsOn(t *testing.T, p provider.Provider, names ...string) map[string]string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	paths := map[string]string{}

	for _, n := range names {
		Remember(p, n, "postgres", "")

		p, err := originPath(n)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := os.Stat(p); err != nil {
			t.Fatalf("record for %s was not written: %v", n, err)
		}

		paths[n] = p
	}

	return paths
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// A create that failed and took the sandbox's only container with it left
// ~/.sbx/origins/<name>.json behind, and `sbx rm <name>` then said there was no such sandbox and
// left it forever. rm of a name with no sandbox clears that record, and says so.
func TestRemoveClearsTheOriginOfASandboxThatNoLongerExists(t *testing.T) {
	paths := origins(t, "failed-create")
	p := &snapStub{}

	var err error

	out := captureOutput(t, func() { err = RemoveMissing(context.Background(), p, "failed-create") })
	if err != nil {
		t.Fatalf("rm of a leftover record failed: %v", err)
	}

	if exists(paths["failed-create"]) {
		t.Error("the leftover origin record is still there")
	}

	for _, want := range []string{`no sandbox "failed-create"`, "origin record", paths["failed-create"]} {
		if !strings.Contains(out, want) {
			t.Errorf("rm did not say %q:\n%s", want, out)
		}
	}
}

// A name with neither a sandbox nor a record is still the typo it always was. And a snapshot
// carries its source's record (Inherit), so `sbx rm <snapshot>` must not take it: a fork of it
// needs that record.
func TestRemoveOfAMissingNameKeepsASnapshotsRecordAndStillRefusesATypo(t *testing.T) {
	paths := origins(t, "golden")
	p := &snapStub{images: []string{"sbx-snap-golden-db:latest"}}

	if err := RemoveMissing(context.Background(), p, "golden"); err == nil {
		t.Error("rm of a snapshot's name passed as if it removed something")
	}

	if !exists(paths["golden"]) {
		t.Error("rm of a name with no sandbox removed a snapshot's origin record")
	}

	if err := RemoveMissing(context.Background(), p, "typo"); err == nil || !strings.Contains(err.Error(), `no sandbox "typo"`) {
		t.Errorf("rm of a name with nothing behind it = %v, want the unknown-sandbox error", err)
	}
}

// `sbx gc` lists origin records whose sandbox and snapshot are both gone, and --force removes
// them - the stale-lock rule. A record whose sandbox exists, asleep or awake, or which a snapshot
// carries, is never offered.
func TestGCListsAndRemovesOrphanOriginRecords(t *testing.T) {
	paths := origins(t, "gone", "asleep", "golden")
	p := &snapStub{
		units:  []provider.Unit{{Sandbox: "asleep", Service: "db", Running: false}},
		images: []string{"sbx-snap-golden-db:latest"},
	}

	var out bytes.Buffer
	if err := gcOrigins(context.Background(), p, &out, false); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "    gone\n") {
		t.Errorf("the orphan record is not listed:\n%s", out.String())
	}

	for _, keep := range []string{"asleep", "golden"} {
		if strings.Contains(out.String(), "    "+keep+"\n") {
			t.Errorf("%s's record was offered:\n%s", keep, out.String())
		}
	}

	if !exists(paths["gone"]) {
		t.Fatal("listing removed a record without --force")
	}

	out.Reset()

	if err := gcOrigins(context.Background(), p, &out, true); err != nil {
		t.Fatal(err)
	}

	if exists(paths["gone"]) {
		t.Errorf("--force left the orphan record:\n%s", out.String())
	}

	for _, keep := range []string{"asleep", "golden"} {
		if !exists(paths[keep]) {
			t.Errorf("--force removed %s's record", keep)
		}
	}
}
