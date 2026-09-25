package provider

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"
)

// UnitOf is one inspect, and reads a container exactly as List would - and an absent container
// is an answer, not an error.
func TestUnitOfInspectsOneContainer(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no unix socket here: %v", err)
	}

	var paths []string

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)

		if r.URL.Path != "/containers/sbx-osb-aaaaaaaaaaaa-sandbox/json" {
			http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"Id": "c1", "Name": "/sbx-osb-aaaaaaaaaaaa-sandbox",
			"State": map[string]any{"Status": "paused"},
			"Config": map[string]any{"Labels": map[string]string{
				labelSandbox: "osb-aaaaaaaaaaaa", labelService: "sandbox", labelSlot: "3",
				labelPorts: "20060:30060",
			}},
		})
	})}

	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	d := newDocker(dockerEndpoint{Network: "unix", Address: sock})

	u, ok, err := d.UnitOf(context.Background(), "osb-aaaaaaaaaaaa", "sandbox")
	if err != nil || !ok {
		t.Fatalf("UnitOf = %v, %v", ok, err)
	}

	if !u.Paused || u.Running || u.Slot != 3 || len(u.Client) != 1 || u.Client[0].Port != 20060 ||
		u.Upstream[0].Port != 30060 || u.Ref != "sbx-osb-aaaaaaaaaaaa-sandbox" {
		t.Fatalf("unit read wrong: %+v", u)
	}

	if _, ok, err := d.UnitOf(context.Background(), "osb-bbbbbbbbbbbb", "sandbox"); ok || err != nil {
		t.Fatalf("absent container: ok %v err %v, want absent and no error", ok, err)
	}

	if len(paths) != 2 {
		t.Fatalf("%d API calls for two lookups: %v", len(paths), paths)
	}
}
