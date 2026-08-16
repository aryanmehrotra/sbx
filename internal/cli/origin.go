package cli

// What a sandbox was created from.
//
// `--template postgres` had to be repeated on `create`, then on `env`, then on `fork`, for
// the same sandbox. Forgetting it produced one of two things, and the second is worse than
// the first:
//
//   - with no sandbox.json in the working directory, `open sandbox.json: no such file` -
//     confusing, but at least a failure;
//   - with an unrelated sandbox.json there, a clean success against the wrong spec. Ordinals
//     are assigned alphabetically over the declared service names, so a different-but-valid
//     spec shifts them and `sbx env` prints a plausible, wrong port. Nothing in the output
//     suggests a problem.
//
// So sbx writes down what a sandbox was made from and uses it when nothing was asked for.
//
// Deliberately a *default*, never a source of truth. An explicit --spec or --template always
// wins, a missing or unreadable record changes nothing about how sbx behaves, and no command
// fails because of it. That is what keeps this a convenience rather than a second place where
// the truth about a sandbox lives - the containers and their labels remain the only one.

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Origin is how a sandbox was specified. Exactly one field is set.
type Origin struct {
	Template string `json:"template,omitempty"`
	Spec     string `json:"spec,omitempty"`
}

func originPath(sandbox string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	// The name is not sanitised because a sandbox name is already constrained to what a
	// container name allows, which excludes a path separator.
	return filepath.Join(home, ".sbx", "origins", sandbox+".json"), nil
}

// Remember records what a sandbox was created from. Best-effort: a failure here must not
// fail a create that otherwise worked.
func Remember(sandbox, template, spec string) {
	path, err := originPath(sandbox)
	if err != nil {
		return
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}

	o := Origin{Template: template}

	// Absolute, because `sbx env` is very often run from a different directory than the
	// `sbx create` that preceded it - a relative path recorded here would resolve somewhere
	// else, which is the exact failure this exists to prevent.
	if template == "" && spec != "" {
		abs, err := filepath.Abs(spec)
		if err != nil {
			return
		}

		o.Spec = abs
	}

	body, err := json.Marshal(o)
	if err != nil {
		return
	}

	_ = os.WriteFile(path, body, 0o644)
}

// Recall reports what a sandbox was created from, if sbx wrote it down.
func Recall(sandbox string) (Origin, bool) {
	path, err := originPath(sandbox)
	if err != nil {
		return Origin{}, false
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return Origin{}, false
	}

	var o Origin
	if json.Unmarshal(body, &o) != nil {
		return Origin{}, false
	}

	if o.Template == "" && o.Spec == "" {
		return Origin{}, false
	}

	// A spec that has since been deleted or moved is worse than no record: it would send
	// every later command to a path that does not exist, where falling back to the working
	// directory would have worked.
	if o.Spec != "" {
		if _, err := os.Stat(o.Spec); err != nil {
			return Origin{}, false
		}
	}

	return o, true
}

// Forget drops the record when a sandbox is destroyed, so the directory does not accumulate
// one file per sandbox anybody ever made.
func Forget(sandbox string) {
	if path, err := originPath(sandbox); err == nil {
		_ = os.Remove(path)
	}
}

// Inherit copies one record onto another name.
//
// A snapshot is not a sandbox, so it has no origin of its own - but `sbx fork <snapshot>
// <new>` needs the same spec the snapshotted sandbox was built from, and asking the user to
// name it again is the repetition this file exists to remove. So `sbx snapshot main golden`
// gives "golden" whatever "main" had, and a fork of golden inherits it in turn.
//
// Best-effort, like everything else here: if the source has no record, the destination
// simply has none either and the caller falls back to --spec.
func Inherit(from, to string) {
	o, ok := Recall(from)
	if !ok {
		return
	}

	Remember(to, o.Template, o.Spec)
}
