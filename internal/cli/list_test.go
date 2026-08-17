package cli

// `sbx list` for something that parses.
//
// An agent driving sbx asks what exists before it does anything, and the human table was the
// one surface with no machine-readable form - so the answer was a regex over column widths,
// which works until a sandbox is named something long.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

func TestListJSONCarriesWhatADriverNeeds(t *testing.T) {
	units := []provider.Unit{
		{Sandbox: "work", Service: "redis", Ref: "sbx-work-redis", Running: false},
		{Sandbox: "work", Service: "mysql", Ref: "sbx-work-mysql", Running: true,
			Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20000}}},
	}

	var buf bytes.Buffer
	if err := listJSON(&buf, units, "docker"); err != nil {
		t.Fatal(err)
	}

	var got []struct {
		Sandbox   string   `json:"sandbox"`
		Service   string   `json:"service"`
		Awake     bool     `json:"awake"`
		Addresses []string `json:"addresses"`
		Ref       string   `json:"ref"`
		Provider  string   `json:"provider"`
	}

	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("the output is not JSON: %v\n%s", err, buf.String())
	}

	if len(got) != 2 {
		t.Fatalf("two services listed as %d", len(got))
	}

	// Sorted, so a caller diffing two runs sees what changed rather than what moved.
	if got[0].Service != "mysql" || got[1].Service != "redis" {
		t.Errorf("order = %s, %s; want it sorted", got[0].Service, got[1].Service)
	}

	if !got[0].Awake || len(got[0].Addresses) != 1 || got[0].Addresses[0] != "127.0.0.1:20000" {
		t.Errorf("mysql = %+v, want it awake with its address", got[0])
	}

	// The ref is what every other command takes, and the provider says which backend answered.
	if got[0].Ref != "sbx-work-mysql" || got[0].Provider != "docker" {
		t.Errorf("mysql lost its ref or provider: %+v", got[0])
	}
}

// "there are none" and "I could not look" have to stay different answers. An empty fleet is an
// empty array, so a caller can tell them apart by the exit code rather than by parsing prose.
func TestListJSONOfAnEmptyFleetIsAnEmptyArray(t *testing.T) {
	var buf bytes.Buffer
	if err := listJSON(&buf, nil, "docker"); err != nil {
		t.Fatal(err)
	}

	if got := strings.TrimSpace(buf.String()); got != "[]" {
		t.Errorf("an empty fleet printed %q, want []", got)
	}
}
