package osb

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// PausedByAPI reports whether sandbox was paused through the API, from its record in dir. The
// CLI's sleep and wake reach the provider directly rather than through the daemon, so this record
// is the only thing that can tell them a pause is the API's to release: stopping such a sandbox
// loses the memory the pause kept, and starting it thaws what the API still reports Paused.
func PausedByAPI(dir, sandbox string) bool {
	if !validID(sandbox) {
		return false
	}

	body, err := os.ReadFile(filepath.Join(dir, sandbox+".json"))
	if err != nil {
		return false
	}

	var r struct {
		PausedByAPI bool `json:"pausedByApi"`
	}

	return json.Unmarshal(body, &r) == nil && r.PausedByAPI
}

// HeldRefusal is what a sleep or wake says about a sandbox paused through the API: why not, and
// the call that releases it.
func HeldRefusal(verb, sandbox string) string {
	return fmt.Sprintf("%s is paused through the OpenSandbox API, so %s would undo that pause "+
		"behind the API (a sleep also loses the memory the pause kept). Resume it through the "+
		"API first: POST /v1/sandboxes/%s/resume, or the SDK's Resume", sandbox, verb, sandbox)
}
