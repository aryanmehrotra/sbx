package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/daemon"
	"github.com/aryanmehrotra/sbx/internal/egress"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

type oneFilter struct {
	provider.Provider
	f provider.EgressFilter
}

func (oneFilter) Name() string { return "fake" }

func (o oneFilter) EgressFilter(context.Context, string) (provider.EgressFilter, error) {
	return o.f, nil
}

// The order flags were typed in is the order the rules are matched in.
func TestEgressKeepsTheOrderTheRulesWereTyped(t *testing.T) {
	f := egress.NewPolicy(egress.DenyAll())
	srv := httptest.NewServer(&egress.Control{Filter: f, Token: "k"})

	defer srv.Close()

	p := oneFilter{f: provider.EgressFilter{Sandbox: "s", Services: []string{"a"}, Declared: egress.DenyAll(),
		Control: strings.TrimPrefix(srv.URL, "http://"), Token: "k"}}
	c := daemon.NewEgressControl(p, t.TempDir())

	var out bytes.Buffer

	err := egressTo(context.Background(), &out, c, "s", "", EgressChange{
		Default: "allow",
		Rules:   []egress.Rule{{Action: "deny", Target: "a.example.com"}, {Action: "allow", Target: "*.example.com"}},
		JSON:    true,
	})
	if err != nil {
		t.Fatal(err)
	}

	var st egress.Status
	if err := json.Unmarshal(out.Bytes(), &st); err != nil {
		t.Fatalf("--json is not JSON: %v\n%s", err, out.String())
	}

	if st.Policy.Egress[0].Target != "a.example.com" || f.Permits("a.example.com") || !f.Permits("b.example.com") {
		t.Fatalf("rules were reordered: %+v", st.Policy)
	}

	out.Reset()

	if err := egressTo(context.Background(), &out, c, "s", "", EgressChange{}); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "deny   a.example.com") && !strings.Contains(out.String(), "deny  a.example.com") {
		t.Errorf("the table does not show the rule: %s", out.String())
	}
}
