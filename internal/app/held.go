package app

import (
	"errors"

	"github.com/aryanmehrotra/sbx/internal/osb"
)

// refuseAPIPaused stops `sbx sleep`/`wake`/`ready` on a sandbox the OpenSandbox API paused. They
// go straight to the provider rather than through the daemon, so only the API's own record can
// tell them the pause is not theirs to undo.
func refuseAPIPaused(verb, sandbox string) error {
	dir, err := osb.DefaultStateDir()
	if err != nil {
		return nil
	}

	if osb.PausedByAPI(dir, sandbox) {
		return errors.New(osb.HeldRefusal("`sbx "+verb+"`", sandbox))
	}

	return nil
}
