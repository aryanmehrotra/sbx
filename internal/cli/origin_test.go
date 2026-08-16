package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// Remember/Recall write under $HOME, so each test gets its own.
func withHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func TestRemembersATemplate(t *testing.T) {
	withHome(t)

	Remember("branch-x", "postgres", "sandbox.json")

	o, ok := Recall("branch-x")
	if !ok {
		t.Fatal("nothing recalled for a sandbox that was just remembered")
	}

	if o.Template != "postgres" {
		t.Errorf("template = %q, want postgres", o.Template)
	}

	if o.Spec != "" {
		t.Errorf("a template was recorded with a spec path too: %q", o.Spec)
	}
}

// The path is stored absolute: `sbx env` is very often run from a different directory than
// the create that preceded it, and a relative path would resolve somewhere else - which is
// the exact failure this feature exists to prevent.
func TestASpecPathIsRecordedAbsolute(t *testing.T) {
	withHome(t)

	dir := t.TempDir()
	spec := filepath.Join(dir, "sandbox.json")

	if err := os.WriteFile(spec, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Recorded as a relative path, from that directory.
	t.Chdir(dir)
	Remember("branch-y", "", "sandbox.json")

	o, ok := Recall("branch-y")
	if !ok {
		t.Fatal("nothing recalled")
	}

	if !filepath.IsAbs(o.Spec) {
		t.Errorf("spec recorded as %q, which is not absolute", o.Spec)
	}
}

// A spec that has since moved or been deleted is worse than no record at all: it would send
// every later command to a path that does not exist, where falling back to the working
// directory would have worked.
func TestAVanishedSpecIsNotRecalled(t *testing.T) {
	withHome(t)

	dir := t.TempDir()
	spec := filepath.Join(dir, "sandbox.json")

	if err := os.WriteFile(spec, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	Remember("branch-z", "", spec)

	if _, ok := Recall("branch-z"); !ok {
		t.Fatal("not recalled while the spec still existed")
	}

	if err := os.Remove(spec); err != nil {
		t.Fatal(err)
	}

	if o, ok := Recall("branch-z"); ok {
		t.Errorf("recalled a spec that no longer exists: %q", o.Spec)
	}
}

func TestForgetDropsIt(t *testing.T) {
	withHome(t)

	Remember("gone", "postgres", "")

	Forget("gone")

	if _, ok := Recall("gone"); ok {
		t.Error("still recalled after Forget - the directory would accumulate one file per " +
			"sandbox anybody ever made")
	}
}

// Nothing recorded must behave exactly as before this existed, or adding a convenience
// changed the behaviour of every command that does not use it.
func TestAnUnknownSandboxRecallsNothing(t *testing.T) {
	withHome(t)

	if _, ok := Recall("never-created"); ok {
		t.Error("recalled something for a sandbox that was never remembered")
	}
}

// A record must never be a source of truth - a create that worked must not fail because the
// record could not be written.
func TestRememberDoesNotFailOnAnUnwritableHome(t *testing.T) {
	// A home that is a regular file, so creating ~/.sbx/origins under it cannot succeed.
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", file)

	Remember("x", "postgres", "") // must not panic, and must not be reported as an error

	if _, ok := Recall("x"); ok {
		t.Error("recalled something that could not have been written")
	}
}

// A snapshot is not a sandbox and has no origin of its own, but `sbx fork <snapshot> <new>`
// needs the spec the snapshotted sandbox was built from. Without this, the seed-and-fan-out
// workflow makes you name the template three times for one lineage - and getting it wrong on
// the third is a hard failure, not a nudge.
func TestASnapshotInheritsItsSandboxsOrigin(t *testing.T) {
	withHome(t)

	Remember("main", "postgres", "")

	Inherit("main", "golden") // sbx snapshot main golden

	o, ok := Recall("golden")
	if !ok {
		t.Fatal("a snapshot did not inherit its sandbox's origin - `sbx fork golden x` would " +
			"fall back to ./sandbox.json and fail")
	}

	if o.Template != "postgres" {
		t.Errorf("inherited template = %q, want postgres", o.Template)
	}

	// And the fork inherits it in turn.
	Inherit("golden", "agent-1")

	if o, ok := Recall("agent-1"); !ok || o.Template != "postgres" {
		t.Errorf("a fork did not inherit the snapshot's origin: %+v ok=%v", o, ok)
	}
}

// Inheriting from something with no record leaves no record, rather than writing an empty
// one that later reads as "this sandbox came from nowhere".
func TestInheritingFromNothingRecordsNothing(t *testing.T) {
	withHome(t)

	Inherit("never-existed", "child")

	if _, ok := Recall("child"); ok {
		t.Error("inherited a record from a name that had none")
	}
}
