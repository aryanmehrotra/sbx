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

// The isolation tier each service runs under, which containers carry on sbx.isolation. A
// sandbox created with --isolation gvisor listed exactly like a runc one, so the only way to
// check a tier was `docker inspect`. A docker unit with no label predates it and was created as
// a container, which is what it reads as.
func TestListShowsIsolation(t *testing.T) {
	units := []provider.Unit{
		{Sandbox: "a", Service: "old", Ref: "sbx-a-old"},
		{Sandbox: "a", Service: "web", Ref: "sbx-a-web", Isolation: provider.IsolationGVisor},
	}

	var buf bytes.Buffer
	if err := listJSON(&buf, units, "docker"); err != nil {
		t.Fatal(err)
	}

	var got []struct {
		Service   string `json:"service"`
		Isolation string `json:"isolation"`
	}

	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 || got[0].Isolation != "container" || got[1].Isolation != "gvisor" {
		t.Errorf("isolation in --json = %+v, want old=container (unlabelled) and web=gvisor", got)
	}

	var table bytes.Buffer
	listTable(&table, units, "docker")

	lines := strings.Split(strings.TrimSpace(table.String()), "\n")
	if !strings.Contains(lines[0], "ISOLATION") {
		t.Errorf("the table has no ISOLATION column:\n%s", table.String())
	}

	if !strings.Contains(lines[1], "container") || !strings.Contains(lines[2], "gvisor") {
		t.Errorf("the table does not show each service's tier:\n%s", table.String())
	}
}
