// Command sbx gives every branch, task or agent its own copy of a project's backing services,
// and charges nothing for the ones nobody is using. The program lives in internal/app; this
// root file is only the entry point and the one thing that has to be here.
package main

import (
	"embed"
	"os"

	"github.com/aryanmehrotra/sbx/internal/app"
)

// examples are the built-in specs, embedded here rather than in internal/app because //go:embed
// cannot reach a parent directory and examples/ belongs at the top of the repo, where somebody
// browsing it will find it. The FS is handed to app.Main, which reads it exactly as before.
//
//go:embed examples
var examples embed.FS

// version is stamped by scripts/release.sh and .github/workflows/release.yaml through
// -X main.version. It stays in package main because that is the path the release ldflag names;
// "dev" means somebody built it themselves, which is worth knowing when a bug report says the
// wake behaved oddly.
var version = "dev"

func main() {
	os.Exit(app.Main(version, examples, os.Args))
}
