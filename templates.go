package main

// The built-in templates.
//
// This lives in the root package rather than beside the spec loader for one blunt reason:
// go:embed cannot reach a parent directory, and examples/ belongs at the top of the repo
// where somebody browsing it will find it. Keeping the specs discoverable is worth more
// than keeping every Go file in one place.

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// templates are the built-in specs, embedded so that --template nginx works on a machine
// that has this binary and nothing else.
//
// A person adopting this should not have to author a spec before they can see it work, and
// an agent asked to spin up a Postgres should not have to write JSON to a file first.
//
//go:embed examples
var templates embed.FS

// TemplateNames lists what --template accepts.
func TemplateNames() []string {
	entries, err := templates.ReadDir("examples")
	if err != nil {
		return nil
	}

	var out []string

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		if _, err := templates.ReadFile("examples/" + e.Name() + "/sandbox.json"); err == nil {
			out = append(out, e.Name())
		}
	}

	sort.Strings(out)

	return out
}

// TemplateImages lists every image any template needs, deduplicated.
//
// Used by prewarm, and it reads the specs rather than keeping a second list — a hand-kept
// list of images is one that goes stale the first time somebody edits a template.
func TemplateImages() []string {
	seen := map[string]bool{}

	for _, name := range TemplateNames() {
		body, err := templates.ReadFile("examples/" + name + "/sandbox.json")
		if err != nil {
			continue
		}

		var s struct {
			Services map[string]struct {
				Image string `json:"image"`
			} `json:"services"`
		}

		if json.Unmarshal(body, &s) != nil {
			continue
		}

		for _, svc := range s.Services {
			if svc.Image != "" {
				seen[svc.Image] = true
			}
		}
	}

	out := make([]string, 0, len(seen))
	for img := range seen {
		out = append(out, img)
	}

	sort.Strings(out)

	return out
}

// TemplatesRefreshed is the date the template images were last re-resolved to digests.
//
// Pinned images are reproducible and, for exactly the same reason, they stop receiving
// updates. Printing the date is what keeps that a decision rather than an oversight: a pin
// nobody can see the age of is a pin nobody ever refreshes.
func TemplatesRefreshed() string {
	body, err := templates.ReadFile("examples/pinned.json")
	if err != nil {
		return ""
	}

	var p struct {
		Refreshed string `json:"refreshed"`
	}

	if json.Unmarshal(body, &p) != nil {
		return ""
	}

	return p.Refreshed
}

// MaterializeTemplate writes a built-in template to a directory on disk and returns the
// path to its spec.
//
// Written out rather than parsed in memory because a spec can reference files beside it —
// the analytics template mounts a ClickHouse config — and an embedded template has no
// directory for those to live in. Extracting the whole thing means templates and on-disk
// specs then travel exactly one code path, which is the only reason this is not two.
func MaterializeTemplate(name string) (string, error) {
	entries, err := templates.ReadDir("examples/" + name)
	if err != nil {
		return "", fmt.Errorf("no template %q (have: %s)", name, strings.Join(TemplateNames(), ", "))
	}

	// Under $HOME, not the system temp directory.
	//
	// A VM-backed Docker only shares some host paths — Colima shares $HOME and not
	// /var/folders, which is where MkdirTemp puts things on macOS. A bind mount whose source
	// the VM cannot see does not fail: docker creates an empty directory at the destination
	// instead. ClickHouse then finds a directory where its config should be, and exits
	// during startup with no mention of a mount anywhere in the message.
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	dir := filepath.Join(home, ".sbx", "templates", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		body, err := templates.ReadFile("examples/" + name + "/" + e.Name())
		if err != nil {
			return "", err
		}

		if err := os.WriteFile(filepath.Join(dir, e.Name()), body, 0o644); err != nil {
			return "", err
		}
	}

	return filepath.Join(dir, "sandbox.json"), nil
}
