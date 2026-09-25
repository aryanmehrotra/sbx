package osb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQuantities(t *testing.T) {
	for in, want := range map[string]float64{"500m": 0.5, "1": 1, "0.25": 0.25, "2": 2, "1500m": 1.5} {
		if got, err := parseCPU(in); err != nil || got != want {
			t.Errorf("parseCPU(%q) = %v, %v; want %v", in, got, err, want)
		}
	}

	for _, in := range []string{"", "0", "-1", "abc", "m"} {
		if _, err := parseCPU(in); err == nil {
			t.Errorf("parseCPU(%q) accepted", in)
		}
	}

	for in, want := range map[string]uint64{"512Mi": 512 << 20, "2Gi": 2 << 30, "1G": 1e9, "10485760": 10485760, "64Mi": 64 << 20} {
		if got, err := parseMemory(in); err != nil || got != want {
			t.Errorf("parseMemory(%q) = %v, %v; want %v", in, got, err, want)
		}
	}

	for in, hint := range map[string]string{"512m": "512Mi", "1Ki": "6Mi", "x": "512Mi"} {
		if _, err := parseMemory(in); err == nil || !strings.Contains(err.Error(), hint) {
			t.Errorf("parseMemory(%q) = %v, want an error mentioning %q", in, err, hint)
		}
	}
}

func TestMetadataFollowsKubernetesLabelRules(t *testing.T) {
	good := map[string]string{"team": "ml", "example.com/team": "a.b-c_d", "x": "", "name": strings.Repeat("a", 63)}
	if err := checkMetadata(good); err != nil {
		t.Fatalf("valid metadata refused: %v", err)
	}

	for k, v := range map[string]string{
		"has space":        "v",
		"opensandbox.io/x": "v",
		"-lead":            "v",
		"Upper.COM/x":      "v",
		"k":                strings.Repeat("a", 64),
		"k2":               "Data Processing Sandbox",
	} {
		if err := checkMetadata(map[string]string{k: v}); err == nil {
			t.Errorf("metadata %q=%q accepted", k, v)
		}
	}
}

// A corrupt record must not take the others down with it: every other sandbox's expiry lives
// in its own file, and a daemon that refuses to start loses all of them.
func TestStoreSkipsACorruptRecord(t *testing.T) {
	s := store{dir: t.TempDir()}

	good := &record{ID: "osb-aaaaaaaaaaaa", CreatedAt: time.Now(), State: stateRunning}
	if err := s.save(good); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(s.dir, "osb-bbbbbbbbbbbb.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	recs, errs := s.all()
	if len(recs) != 1 || recs[0].ID != good.ID || len(errs) != 1 {
		t.Fatalf("all() = %d records, %v", len(recs), errs)
	}

	if err := s.save(&record{ID: "../escape"}); err == nil {
		t.Fatal("a record with a path in its id was written")
	}
}

// A dev build is "dev" every time it is rebuilt, so the volume must be named for the content or
// the first build's execd would be served for ever.
func TestExecdVolumeIsKeyedByContent(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")

	_ = os.WriteFile(a, []byte("one build"), 0o755)
	_ = os.WriteFile(b, []byte("another build"), 0o755)

	r := newExecdResolver("dev")

	va, err := r.fromFile(a)
	if err != nil {
		t.Fatal(err)
	}

	vb, _ := r.fromFile(b)

	if va.Volume == vb.Volume || !strings.HasPrefix(va.Volume, "sbx-execd-dev-") || va.File != a {
		t.Fatalf("volumes %q and %q", va.Volume, vb.Volume)
	}

	t.Setenv("SBX_EXECD_BINARY", b)

	got, err := newExecdResolver("dev").resolve(t.Context(), "arm64")
	if err != nil || got.File != b {
		t.Fatalf("SBX_EXECD_BINARY was not used first: %+v %v", got, err)
	}

	if v := volumeName("v1.2.3+meta/x"); v != "sbx-execd-v1.2.3-meta-x" {
		t.Fatalf("volumeName = %q", v)
	}
}
