package provider

import (
	"slices"
	"testing"
)

// Which volume belongs to which sandbox, which is the only thing standing between
// `sbx gc --force` and somebody's branch database.
//
// gc offers an artifact for deletion when nothing owns it. So a bug here does not produce a
// wrong listing - it produces a deleted volume, and there is no second chance to notice.
// The dangerous direction is a live sandbox whose volume looks unowned; the harmless one is
// an orphan that never gets reclaimed.
func TestOwnerOf(t *testing.T) {
	live := map[string]bool{"feature-x": true, "main": true, "auth": true}

	cases := []struct {
		volume string
		want   string
	}{
		{"sbx-feature-x-postgres", "feature-x"},
		{"sbx-main-redis", "main"},

		// Gone: the sandbox is not live, so this is reclaimable.
		{"sbx-deleted-branch-postgres", ""},

		{"sbx-auth-postgres", "auth"},

		// Genuinely ambiguous, and resolved conservatively. A volume name is
		// sbx-<sandbox>-<service> and BOTH halves may contain dashes, so
		// "sbx-auth-flow-postgres" reads equally well as sandbox "auth" with a service
		// called "flow-postgres" or as sandbox "auth-flow" with a service called
		// "postgres". With "auth" live, it is attributed to "auth" and therefore not
		// offered for deletion.
		//
		// That is the direction to be wrong in. The cost is an orphan that never gets
		// reclaimed - disk not freed. The other direction costs somebody their database.
		{"sbx-auth-flow-postgres", "auth"},

		// Not ours at all. Something else on the machine called a volume this.
		{"postgres-data", ""},
		{"sbx-", ""},
		{"", ""},
	}

	for _, c := range cases {
		if got := ownerOf(c.volume, live); got != c.want {
			t.Errorf("ownerOf(%q) = %q, want %q", c.volume, got, c.want)
		}
	}
}

// Nothing live means everything is an orphan - which is correct, and is also the shape of
// the worst possible bug: if listing live sandboxes ever fails soft and returns nothing,
// every volume on the machine becomes reclaimable at once. Orphans propagates that error
// rather than treating it as "none", and this pins the distinction.
func TestOwnerOfWithNothingLive(t *testing.T) {
	if got := ownerOf("sbx-feature-x-postgres", map[string]bool{}); got != "" {
		t.Errorf("ownerOf with no live sandboxes = %q, want \"\"", got)
	}

	if got := ownerOf("sbx-feature-x-postgres", nil); got != "" {
		t.Errorf("ownerOf with a nil live set = %q, want \"\"", got)
	}
}

// The invariant gc actually rests on, stated directly rather than inferred from a table:
// whatever a live sandbox's services are called, its volumes are never orphans.
//
// This is the only property that can cost data. Everything else here is about disk not being
// reclaimed, which is recoverable by running gc again after the sandbox is gone.
func TestALiveSandboxNeverLooksLikeAnOrphan(t *testing.T) {
	sandboxes := []string{
		"main", "feature-x", "auth", "auth-flow",
		"release-2026-08", "a", "very-long-branch-name-with-many-dashes",
	}

	services := []string{"postgres", "redis", "flow-postgres", "s", "chrome-headless"}

	live := map[string]bool{}
	for _, s := range sandboxes {
		live[s] = true
	}

	for _, sandbox := range sandboxes {
		for _, service := range services {
			vol := volumeName(sandbox, service)

			if owner := ownerOf(vol, live); owner == "" {
				t.Errorf("volume %q of the live sandbox %q was reported as an orphan - "+
					"`sbx gc --force` would delete it", vol, sandbox)
			}
		}
	}
}

// A pvc volume belongs to whoever asked for it, not to a sandbox, so gc must never read one as
// the data volume of a sandbox that has gone - even when its claim name ends in "-data".
func TestAPIClaimVolumesAreNeverOrphans(t *testing.T) {
	live := map[string]bool{}

	for _, name := range []string{"sbx-osb-pvc-scratch", "sbx-osb-pvc-train-data"} {
		if _, orphan := classifyVolume(name, live); orphan {
			t.Errorf("%s classified as an orphan; gc would delete a caller's persistent volume", name)
		}
	}

	if a, orphan := classifyVolume("sbx-gone-db-data", live); !orphan || a.Sandbox != "" {
		t.Errorf("a dead sandbox's data volume is no longer collected: %+v %v", a, orphan)
	}
}

// The egress filter and the no-NAT bridge a half-finished create leaves behind. `sbx gc` could
// not see either, so a create that failed after starting the filter left a container with a
// published port, a route out and its control token on a label, and nothing on the machine could
// reclaim it.
//
// Unambiguous, unlike a volume name: the prefix is fixed and everything after it is the whole
// sandbox name, dashes and all. So the dangerous direction here is not a mis-split - it is
// offering something a live sandbox is using, and that is what this pins.
func TestClassifyEgressOrphan(t *testing.T) {
	live := map[string]bool{"feature-x": true, "main": true}

	cases := []struct {
		name    string
		kind    string // "" for "not an orphan"
		sandbox string
	}{
		// A live sandbox's filter and bridge are never offered, whatever state it is in:
		// asleep is the normal state here, and a sleeping sandbox still needs both.
		{"sbx-egressfilter-feature-x", "", ""},
		{"sbx-noegress-feature-x", "", ""},
		{"sbx-egressfilter-main", "", ""},

		// Nothing of that sandbox is left, so both are garbage.
		{"sbx-egressfilter-gone-branch", "container", "gone-branch"},
		{"sbx-noegress-gone-branch", "network", "gone-branch"},

		// Dashes in the sandbox name resolve exactly, because the prefix is fixed.
		{"sbx-egressfilter-a-b-c", "container", "a-b-c"},

		// Not ours to judge.
		{"sbx-main-postgres", "", ""},
		{"sbx-egressfilter-", "", ""},
		{"egressfilter-main", "", ""},
		{"bridge", "", ""},
		{"", "", ""},
	}

	for _, c := range cases {
		a, orphan := classifyEgressOrphan(c.name, live)

		if orphan != (c.kind != "") {
			t.Errorf("classifyEgressOrphan(%q) orphan = %v, want %v", c.name, orphan, c.kind != "")
			continue
		}

		if !orphan {
			continue
		}

		if a.Kind != c.kind || a.Name != c.name || a.Sandbox != c.sandbox {
			t.Errorf("classifyEgressOrphan(%q) = %+v, want kind %q sandbox %q",
				c.name, a, c.kind, c.sandbox)
		}
	}
}

// A sandbox is live when it has a SERVICE container, not merely a container carrying its label.
// Its egress filter carries that label too and no sbx.service, so counting it made every
// orphaned filter mark its own sandbox live - which is precisely the sandbox whose leftovers gc
// exists to find, and it also kept that sandbox's data volume uncollectable.
func TestALiveSandboxIsOneWithAService(t *testing.T) {
	// What liveSandboxes asks docker for. The filter is excluded by the second filter rather
	// than by name, which is the same distinction slotOf makes.
	args := liveSandboxArgs()

	if !slices.Contains(args, "label="+labelService) {
		t.Fatalf("liveSandboxes does not exclude containers without a service label, so an "+
			"orphaned egress filter marks its own sandbox live: %v", args)
	}
}
