package cli

import (
	"fmt"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// AddIsolation decides which isolation tier `sbx add` creates a service with.
//
// A sandbox has one tier. `--isolation` defaults to "container", so `sbx add` on a gVisor
// sandbox used to put the new service on runc beside services under runsc - with no warning,
// and the sandbox reporting nothing different. The tier each unit was created with is on its
// sbx.isolation label now, so the sandbox's own tier is the default, and an explicit one that
// differs is refused: a sandbox that is gVisor except for one service is not a gVisor sandbox.
//
// Units without the label predate it and were created as containers, which is what they are
// read as. A provider that records no tier at all (firecracker, kubernetes) is left to apply
// the requested one as it always did.
func AddIsolation(providerName string, units []provider.Unit, requested provider.Isolation, explicit bool,
) (provider.Isolation, error) {
	var tier provider.Isolation

	for _, u := range units {
		if u.Isolation != "" {
			tier = u.Isolation
			break
		}
	}

	if tier == "" {
		if providerName != "docker" {
			return requested, nil
		}

		tier = provider.IsolationContainer
	}

	if !explicit {
		return tier, nil
	}

	if requested != tier {
		return "", fmt.Errorf("--isolation %s does not match the sandbox, whose services run with "+
			"isolation %s: a sandbox has one tier. Drop --isolation to add this service as %s, or "+
			"recreate the sandbox with --isolation %s (sbx rm, then sbx create --isolation %s)",
			requested, tier, tier, requested, requested)
	}

	return tier, nil
}
