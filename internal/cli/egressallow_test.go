package cli

import (
	"fmt"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/spec"
)

// Two services with lists of their own share one filter. Each create ensured that filter with its
// own list, so the second replaced the first's hosts away; every service now carries the union.
func TestEveryAllowListServiceIsHandedTheSandboxsUnion(t *testing.T) {
	sp := &spec.Spec{Services: map[string]spec.Service{
		"api":    {EgressAllow: []string{"pypi.org", "api.openai.com"}},
		"worker": {EgressAllow: []string{"github.com:22", "pypi.org"}},
		"extra":  {EgressAllow: []string{"optional.example"}, Optional: true},
		"db":     {},
	}}

	if got := fmt.Sprint(sharedAllowList(sp, false)); got != "[api.openai.com github.com:22 pypi.org]" {
		t.Errorf("union = %s", got)
	}

	if got := fmt.Sprint(sharedAllowList(sp, true)); got != "[api.openai.com github.com:22 optional.example pypi.org]" {
		t.Errorf("union with --optional = %s", got)
	}
}
