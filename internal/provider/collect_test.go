package provider

import "testing"

// Which volume belongs to which sandbox, which is the only thing standing between
// `sbx gc --force` and somebody's branch database.
//
// gc offers an artifact for deletion when nothing owns it. So a bug here does not produce a
// wrong listing — it produces a deleted volume, and there is no second chance to notice.
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
		// reclaimed — disk not freed. The other direction costs somebody their database.
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

// Nothing live means everything is an orphan — which is correct, and is also the shape of
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
				t.Errorf("volume %q of the live sandbox %q was reported as an orphan — "+
					"`sbx gc --force` would delete it", vol, sandbox)
			}
		}
	}
}
