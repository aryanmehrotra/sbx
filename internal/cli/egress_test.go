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

// `sbx egress <sb> --remove <target>` for a target with no rule exited 0 and printed the policy
// unchanged, so a typo in a lock-down read as success. It is an error naming the rules there are,
// and nothing is changed - not even the other targets on the same command line.
func TestEgressRemoveOfATargetWithNoRuleIsAnError(t *testing.T) {
	start := egress.Policy{DefaultAction: egress.ActionAllow, Egress: []egress.Rule{
		{Action: "deny", Target: "10.0.0.0/8"}, {Action: "deny", Target: "*.pastebin.com"}}}
	f := egress.NewPolicy(start)
	srv := httptest.NewServer(&egress.Control{Filter: f, Token: "k"})

	defer srv.Close()

	p := oneFilter{f: provider.EgressFilter{Sandbox: "s", Services: []string{"a"}, Declared: start,
		Control: strings.TrimPrefix(srv.URL, "http://"), Token: "k"}}
	c := daemon.NewEgressControl(p, t.TempDir())

	var out bytes.Buffer

	err := egressTo(context.Background(), &out, c, "s", "", EgressChange{Remove: []string{"10.0.0.0/8", "pastebin.com"}})
	if err == nil {
		t.Fatal("removing a target with no rule succeeded")
	}

	for _, want := range []string{`"pastebin.com"`, "10.0.0.0/8", "*.pastebin.com"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s: %v", want, err)
		}
	}

	if len(f.Policy().Egress) != 2 {
		t.Errorf("a refused --remove still changed the policy: %+v", f.Policy())
	}

	// Case and surrounding space are not a different target.
	if err := egressTo(context.Background(), &out, c, "s", "", EgressChange{Remove: []string{" *.PasteBin.com "}}); err != nil {
		t.Fatalf("removing a present target: %v", err)
	}
}
