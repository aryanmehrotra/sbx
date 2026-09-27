package app

import (
	"flag"
	"slices"
	"testing"
)

// `sbx install redis-cli --yes` must install redis-cli, not try to install "--yes".
func TestParseInterleavedTakesFlagsAnywhere(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "")
	dry := fs.Bool("dry-run", false, "")

	got := parseInterleaved(fs, []string{"redis-cli", "--yes", "kubectl", "--dry-run"})

	if !slices.Equal(got, []string{"redis-cli", "kubectl"}) || !*yes || !*dry {
		t.Errorf("positionals %v, yes=%v, dry-run=%v", got, *yes, *dry)
	}
}
