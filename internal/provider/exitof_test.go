package provider

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// ExitOf reads the engine's own account of a stopped container - the part `docker logs` never
// has, because a process killed by a signal or refused by the OCI runtime prints nothing.
func TestExitOfReadsTheEnginesLastState(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no unix socket here: %v", err)
	}

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/sbx-osb-aaaaaaaaaaaa-sandbox/json" {
			http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"State": map[string]any{"Status": "exited", "ExitCode": 137, "OOMKilled": true,
				"Error": ""},
		})
	})}

	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	d := newDocker(dockerEndpoint{Network: "unix", Address: sock})

	var er ExitReporter = d

	st, err := er.ExitOf(context.Background(), "sbx-osb-aaaaaaaaaaaa-sandbox")
	if err != nil {
		t.Fatal(err)
	}

	if st != (ExitState{Status: "exited", ExitCode: 137, OOMKilled: true}) {
		t.Fatalf("ExitOf = %+v", st)
	}

	if _, err := er.ExitOf(context.Background(), "sbx-osb-bbbbbbbbbbbb-sandbox"); err == nil {
		t.Fatal("an absent container read as an exit state")
	}
}

func TestExitStateStringNamesTheCause(t *testing.T) {
	for _, c := range []struct {
		st   ExitState
		want []string
	}{
		{ExitState{Status: "exited", ExitCode: 137, OOMKilled: true}, []string{"exit code 137", "OOMKilled"}},
		{ExitState{Status: "exited", ExitCode: 137}, []string{"SIGKILL"}},
		{ExitState{Status: "exited", ExitCode: 143}, []string{"SIGTERM"}},
		{ExitState{Status: "created", ExitCode: 128, Error: "OCI runtime create failed"},
			[]string{"never started", "OCI runtime create failed"}},
		{ExitState{}, []string{"state unknown", "exit code 0"}},
	} {
		got := c.st.String()
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%+v.String() = %q, want it to carry %q", c.st, got, w)
			}
		}
	}
}
