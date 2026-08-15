package cli

import (
	"fmt"
	"regexp"
)

// What a sandbox may be called.
//
// This existed implicitly: the container runtime rejects anything outside its own naming
// rules, so `sbx create ../../tmp/evil` failed — after allocating a slot, with a hundred-line
// dump of the docker command and an error about container names, for something the user had
// typed thirty characters earlier.
//
// Stating it here makes two things true instead of one. The message names the problem, and
// the constraint that keeps `~/.sbx/origins/<name>.json` inside its directory is written down
// rather than inherited from whatever docker happens to allow.
var sandboxName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// A snapshot name is stricter, because it becomes part of an image tag rather than a
// container name — and image repositories must be lowercase. `sbx snapshot db GOLDEN` failed
// with "repository name must be lowercase" from a docker commit the user never typed.
var snapshotName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// ValidateName rejects a name that could not become a container name, before anything is
// created. `kind` is what the name is — "sandbox" or "service" — so the message names the
// argument the caller actually typed.
func ValidateName(kind, name string) error {
	if name == "" {
		return fmt.Errorf("a %s needs a name", kind)
	}

	if len(name) > 100 {
		return fmt.Errorf("%s name is %d characters; keep it under 100 so the container "+
			"names derived from it stay legal", kind, len(name))
	}

	if !sandboxName.MatchString(name) {
		return fmt.Errorf("%s name %q is not usable: start with a letter or digit, then "+
			"letters, digits, dot, dash or underscore. A branch name with a slash needs "+
			"replacing — `feature/x` becomes `feature-x`", kind, name)
	}

	return nil
}

// ValidateSnapshotName rejects a snapshot name that could not become an image tag.
//
// Separate from ValidateName because the rule really is different: docker container names
// allow uppercase and image repositories do not, so `sbx snapshot db GOLDEN` reached the
// runtime and came back with "repository name (library/sbx-snap-GOLDEN-postgres) must be
// lowercase" — a message about a name the user never typed, from a command they never ran.
func ValidateSnapshotName(name string) error {
	if name == "" {
		return fmt.Errorf("a snapshot needs a name")
	}

	if len(name) > 100 {
		return fmt.Errorf("snapshot name is %d characters; keep it under 100 so the image "+
			"tags derived from it stay legal", len(name))
	}

	if !snapshotName.MatchString(name) {
		return fmt.Errorf("snapshot name %q is not usable: it becomes part of an image tag, "+
			"so it must be lowercase — letters, digits, dot, dash or underscore, starting "+
			"with a letter or digit", name)
	}

	return nil
}
