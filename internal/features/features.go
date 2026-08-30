// Package features is the gate in front of anything not yet proven.
//
// It exists to let something ship before it is a promise, and it is deliberately small: sbx has
// one committed file and a handful of flags, and a registry of preferences would undo that.
//
// So a gate is a statement about MATURITY, not a preference. Every gate carries a stability, a
// stable feature has no gate at all, and the entry is deleted when the feature graduates - which
// makes this file a list of what is still unfinished rather than a configuration surface.
//
// It is read from the environment rather than from sandbox.json on purpose. A spec describes the
// project and is committed and shared; whether you are willing to run an unproven code path is
// about you and your machine, and putting it in the spec would commit one person's appetite for
// risk to everybody who checks the repo out.
//
//	SBX_FEATURES=ssh,devcontainer sbx create dev
//	sbx features
package features

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// Stability is how far a feature is from being ungateable.
type Stability string

const (
	// Preview works and is tested. What may still move is the contract - a flag name, an
	// output shape - so it is off until somebody opts in and accepts that.
	Preview Stability = "preview"

	// Experimental has a known hole in it, named in Caveat. Turning one on says so out loud,
	// because finding out later from behaviour is worse than being told now.
	Experimental Stability = "experimental"
)

// Feature is one gate.
type Feature struct {
	Name      string
	Stability Stability

	// Summary is what it does, in one line, for `sbx features`.
	Summary string

	// Caveat is what is unproven, and is required for Experimental. It is printed when the
	// feature is switched on.
	Caveat string
}

// registry is every gate that exists. A stable feature is not in here - it is just on.
//
// Kept sorted by name so `sbx features` is stable output and a diff to this list is readable.
var registry = []Feature{}

// Register adds a gate. Called from an init() beside the feature it guards, so the gate and the
// thing it guards are deleted in the same change.
func Register(f Feature) {
	if f.Stability == Experimental && f.Caveat == "" {
		// A programming error, and worth being loud about: an experimental feature whose
		// caveat is empty is one that switches on silently, which is the whole thing this
		// type exists to prevent.
		panic("features: experimental feature " + f.Name + " has no caveat")
	}

	registry = append(registry, f)

	sort.Slice(registry, func(i, j int) bool { return registry[i].Name < registry[j].Name })
}

// All is every gate, sorted.
func All() []Feature { return append([]Feature(nil), registry...) }

// enabled is the parsed SBX_FEATURES, or nil before the first read.
var enabled map[string]bool

// Enabled reports whether a feature is switched on.
//
// It reads the environment on every call rather than caching, which is not a performance
// question: nothing consults a gate on a per-connection path, and a test that sets the variable
// expects the next call to see it. If a gate ever does end up on the hot path, the connection
// benchmark is what will say so.
func Enabled(name string) bool {
	return parse(os.Getenv(envVar))[name]
}

// envVar is where the list comes from.
const envVar = "SBX_FEATURES"

// parse reads the comma-separated list.
//
// Unknown names are kept rather than rejected: this is read in a dozen places and none of them
// is the right one to fail a command over a typo. `sbx features` reports them instead, in one
// place, where the reader is already looking at the list of real names.
func parse(s string) map[string]bool {
	out := map[string]bool{}

	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out[part] = true
		}
	}

	return out
}

// Unknown is every name in SBX_FEATURES that is not a real feature, sorted.
//
// A typo silently doing nothing is the failure this exists to surface: somebody sets
// SBX_FEATURES=devcontainers, sees no devcontainer support, and concludes the feature is broken
// rather than that the name has an s on the end.
func Unknown() []string {
	known := map[string]bool{}
	for _, f := range registry {
		known[f.Name] = true
	}

	var out []string

	for name := range parse(os.Getenv(envVar)) {
		if !known[name] {
			out = append(out, name)
		}
	}

	sort.Strings(out)

	return out
}

// Note is the line printed when an experimental feature is used, or "" for anything else.
func Note(name string) string {
	for _, f := range registry {
		if f.Name == name && f.Stability == Experimental && Enabled(name) {
			return fmt.Sprintf("note: %s is experimental - %s", f.Name, f.Caveat)
		}
	}

	return ""
}

// Refuse is the error a command returns when the feature behind it is off.
//
// One sentence and the exact thing to type. A gate that says only "not enabled" makes the reader
// go and find the variable's name, which for a feature they just read about in the docs is a
// second search for something the message already knows.
func Refuse(name string) error {
	for _, f := range registry {
		if f.Name != name {
			continue
		}

		return fmt.Errorf("%s is %s and off by default: %s\n     turn it on with SBX_FEATURES=%s",
			f.Name, f.Stability, f.Summary, f.Name)
	}

	return fmt.Errorf("%s is not a feature this build has", name)
}
