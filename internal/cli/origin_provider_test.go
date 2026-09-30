package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// writeOrigin puts a record down as-is: another backend's, or one from before records said which.
func writeOrigin(t *testing.T, name, prov string) string {
	t.Helper()

	rememberAs(name, Origin{Template: "postgres", Provider: prov})

	p, err := originPath(name)
	if err != nil || !exists(p) {
		t.Fatalf("record %s not written: %v", name, err)
	}

	return p
}

// A record says which backend its sandbox lives on: the provider's name, and where that provider
// points when one machine can reach several (a docker endpoint, a kubectl context). A snapshot's
// record, copied from its source, keeps the source's.
func TestRememberRecordsWhichBackendTheSandboxIsOn(t *testing.T) {
	paths := origins(t, "src")

	body, err := os.ReadFile(paths["src"])
	if err != nil {
		t.Fatal(err)
	}

	var o Origin
	if err := json.Unmarshal(body, &o); err != nil {
		t.Fatal(err)
	}

	if o.Provider != "docker@unix:///a.sock" {
		t.Errorf("record provider = %q, want docker@unix:///a.sock (body %s)", o.Provider, body)
	}

	Inherit("src", "snap")

	if got, _ := Recall("snap"); got.Provider != o.Provider {
		t.Errorf("an inherited record has provider %q, want its source's %q", got.Provider, o.Provider)
	}
}

// Asked through docker, rm must not clear the record of a sandbox that lives on kubernetes, on
// firecracker, or on another docker engine: that sandbox is not missing, docker just cannot see it.
func TestRmNeverClearsAnotherBackendsRecord(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := &snapStub{}

	for _, prov := range []string{"kubernetes/default@kind-a", "firecracker", "docker@unix:///other.sock"} {
		name := "on-" + strings.NewReplacer("/", "-", "@", "-", ":", "-", ".", "-").Replace(prov)
		path := writeOrigin(t, name, prov)

		out := captureOutput(t, func() {
			if err := RemoveMissing(context.Background(), p, name); err == nil {
				t.Errorf("%s: rm through docker reported success for a record %s owns", name, prov)
			}
		})

		if !exists(path) {
			t.Errorf("rm through docker removed the record of a sandbox on %s", prov)
		}

		if strings.Contains(out, "removed") {
			t.Errorf("%s: rm claimed a removal: %s", name, out)
		}
	}
}

// A record written before records named their backend cannot be attributed, so it is never removed
// for you - rm says where it is and how to remove it by hand.
func TestRmLeavesALegacyRecordAndSaysHowToRemoveIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	path := writeOrigin(t, "old", "")

	err := RemoveMissing(context.Background(), &snapStub{}, "old")
	if err == nil {
		t.Fatal("rm of a legacy record reported success")
	}

	if !exists(path) {
		t.Error("rm removed a record it cannot attribute to a backend")
	}

	for _, want := range []string{path, "rm " + path} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q: %v", want, err)
		}
	}
}

// gc offers only the asking backend's own orphans. Another backend's are not mentioned; legacy ones
// are counted with the manual command, never removed.
func TestGCOffersOnlyTheAskingBackendsOrphans(t *testing.T) {
	paths := origins(t, "mine")
	kube := writeOrigin(t, "theirs", "kubernetes/default@kind-a")
	legacy := writeOrigin(t, "old", "")
	p := &snapStub{}

	var out bytes.Buffer
	if err := gcOrigins(context.Background(), p, &out, true); err != nil {
		t.Fatal(err)
	}

	if exists(paths["mine"]) {
		t.Errorf("--force left this backend's own orphan:\n%s", out.String())
	}

	if !exists(kube) {
		t.Errorf("--force through docker removed a kubernetes sandbox's record:\n%s", out.String())
	}

	if !exists(legacy) {
		t.Errorf("--force removed a record it cannot attribute:\n%s", out.String())
	}

	if strings.Contains(out.String(), "theirs") {
		t.Errorf("another backend's record was offered:\n%s", out.String())
	}

	for _, want := range []string{"1 origin record", "no provider", "rm "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the legacy note does not say %q:\n%s", want, out.String())
		}
	}
}

// 134 orphan records printed 134 full paths. A long set is a count and the first few names, like a
// readable gc, and --force still takes all of them.
func TestGCSummarisesALongListOfOrphanRecords(t *testing.T) {
	names := make([]string, 134)
	for i := range names {
		names[i] = fmt.Sprintf("ci-%03d", i)
	}

	paths := origins(t, names...)
	p := &snapStub{}

	var out bytes.Buffer
	if err := gcOrigins(context.Background(), p, &out, false); err != nil {
		t.Fatal(err)
	}

	if n := strings.Count(out.String(), "\n"); n > 16 {
		t.Errorf("134 orphans printed %d lines:\n%s", n, out.String())
	}

	for _, want := range []string{"134", "ci-000", "and 124 more", "--force"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the summary does not say %q:\n%s", want, out.String())
		}
	}

	out.Reset()

	if err := gcOrigins(context.Background(), p, &out, true); err != nil {
		t.Fatal(err)
	}

	for n, path := range paths {
		if exists(path) {
			t.Fatalf("--force left %s:\n%s", n, out.String())
		}
	}
}
