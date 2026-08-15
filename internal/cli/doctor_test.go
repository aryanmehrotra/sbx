package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The report must be honest about this machine rather than optimistic about a machine.
// Every entry that is absent has to say what its absence costs, because "✗ gvisor" alone
// sends someone to the source to find out whether it matters.
func TestEveryAbsentCapabilityExplainsItself(t *testing.T) {
	rep := Doctor(context.Background())

	if len(rep.Capabilities) == 0 {
		t.Fatal("the report is empty")
	}

	for _, c := range rep.Capabilities {
		if !c.Have && c.Meaning == "" {
			t.Errorf("%q is absent and says nothing about what that costs", c.Name)
		}

		if c.Detail == "" {
			t.Errorf("%q has no detail", c.Name)
		}
	}
}

// The isolation rows are the reason this command exists: sbx refuses rather than silently
// downgrading, and the refusal should not be the first time anyone hears about it.
func TestReportCoversIsolationAndCheckpoint(t *testing.T) {
	rep := Doctor(context.Background())

	want := []string{"isolation gvisor", "isolation kata", "docker checkpoint"}

	for _, w := range want {
		found := false

		for _, c := range rep.Capabilities {
			if c.Name == w {
				found = true
			}
		}

		if !found {
			t.Errorf("the report never mentions %q", w)
		}
	}
}

// A script deciding whether to run something needs this parseable, not scraped.
func TestJSONIsMachineReadable(t *testing.T) {
	var buf bytes.Buffer

	if err := PrintReport(&buf, Doctor(context.Background()), true); err != nil {
		t.Fatalf("PrintReport: %v", err)
	}

	var got Report
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("the JSON does not parse: %v\n%s", err, buf.String())
	}

	if got.Host == "" || len(got.Capabilities) == 0 {
		t.Errorf("the JSON is missing host or capabilities: %s", buf.String())
	}
}

// The human form must mark absence visibly. A report where everything looks the same is
// one nobody reads twice.
func TestHumanFormMarksAbsence(t *testing.T) {
	rep := Report{
		Host: "test/arch",
		Capabilities: []Capability{
			{Name: "present", Have: true, Detail: "here"},
			{Name: "absent", Have: false, Detail: "not on PATH", Meaning: "this is what it costs"},
		},
	}

	var buf bytes.Buffer
	if err := PrintReport(&buf, rep, false); err != nil {
		t.Fatalf("PrintReport: %v", err)
	}

	out := buf.String()
	for _, want := range []string{"✓ present", "✗ absent", "this is what it costs", "test/arch"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not contain %q:\n%s", want, out)
		}
	}
}
