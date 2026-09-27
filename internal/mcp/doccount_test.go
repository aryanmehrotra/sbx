package mcp

// Several pages state how many tools `sbx mcp` serves ("19 tools"). The number is easy to
// write and easy to forget: a tool added here leaves every one of those sentences wrong, and
// no other test notices. This reads the user-facing pages and checks every "N tools" against
// the real list, so the count changes in the same PR as the tools.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/osbclient"
)

func TestTheDocumentedToolCountIsRight(t *testing.T) {
	c, err := osbclient.New("http://127.0.0.1:1", "") // never dialled: Tools only builds descriptors
	if err != nil {
		t.Fatal(err)
	}

	want := len(NewSandboxes(c).Tools())

	pages, err := filepath.Glob("../../docs/*.md")
	if err != nil {
		t.Fatal(err)
	}

	pages = append(pages, "../../README.md")

	// "19 tools", "19 MCP tools". Release notes and design records are pinned to their day
	// and live in subdirectories, so the glob leaves them out.
	re := regexp.MustCompile(`\b(\d+) (?:MCP )?tools\b`)

	for _, p := range pages {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}

		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			if n, _ := strconv.Atoi(m[1]); n != want {
				t.Errorf("%s says %q, but internal/mcp/tools.go defines %d tools: fix the page", p, m[0], want)
			}
		}
	}
}
