package osbclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/osbclient"
	"github.com/aryanmehrotra/sbx/internal/osbclient/osbtest"
)

func newPair(t *testing.T) (*osbtest.Server, *osbclient.Client) {
	t.Helper()

	f := osbtest.New()
	f.Key = "k3y"
	t.Cleanup(f.Close)

	c, err := osbclient.New(f.URL, "k3y")
	if err != nil {
		t.Fatal(err)
	}

	return f, c
}

func TestNewNormalizesTheAddress(t *testing.T) {
	for in, want := range map[string]string{
		"":                             "http://localhost:8080/v1",
		"localhost:8080":               "http://localhost:8080/v1",
		"http://127.0.0.1:9/":          "http://127.0.0.1:9/v1",
		"https://osb.example.com/v1":   "https://osb.example.com/v1",
		"https://osb.example.com/api/": "https://osb.example.com/api/v1",
	} {
		c, err := osbclient.New(in, "")
		if err != nil {
			t.Errorf("New(%q): %v", in, err)

			continue
		}

		if c.BaseURL() != want {
			t.Errorf("New(%q).BaseURL() = %q, want %q", in, c.BaseURL(), want)
		}
	}

	for _, bad := range []string{"ftp://x", "http://", "::"} {
		if _, err := osbclient.New(bad, ""); err == nil {
			t.Errorf("New(%q) accepted an address that is not an http server", bad)
		}
	}
}

func TestTheKeyIsSentAndItsAbsenceIsAnAPIError(t *testing.T) {
	f, _ := newPair(t)

	anon, _ := osbclient.New(f.URL, "")

	_, err := anon.ListSandboxes(context.Background(), osbclient.ListOptions{})

	var ae *osbclient.APIError
	if !errors.As(err, &ae) || ae.Status != 401 || ae.Code != "UNAUTHORIZED" {
		t.Fatalf("a call without the key: got %v, want a 401 APIError carrying the server's code", err)
	}
}

func TestLifecycleRoundTrip(t *testing.T) {
	f, c := newPair(t)
	ctx := context.Background()
	ttl := 600

	sb, err := c.CreateSandbox(ctx, osbclient.CreateRequest{
		Image:      &osbclient.ImageSpec{URI: "python:3.12", Auth: &osbclient.ImageAuth{Username: "u", Password: "p"}},
		Timeout:    &ttl,
		Entrypoint: []string{"tail", "-f", "/dev/null"},
		Metadata:   map[string]string{"team": "a&b=c", "who": "me too"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if sb.ID == "" || sb.Status.State != osbclient.StateRunning || sb.ExpiresAt == nil {
		t.Fatalf("create answered %+v", sb)
	}

	// The body goes out in the contract's camelCase, with the image under "uri".
	req, _ := f.Last("POST", "/v1/sandboxes")

	var sent map[string]any

	_ = json.Unmarshal(req.Body, &sent)
	if sent["image"].(map[string]any)["uri"] != "python:3.12" || sent["timeout"] != 600.0 {
		t.Errorf("create body = %s", req.Body)
	}

	// A metadata value holding & and = must survive the double encoding.
	list, err := c.ListSandboxes(ctx, osbclient.ListOptions{
		States: []string{"Running", "Paused"}, Metadata: map[string]string{"team": "a&b=c"}, PageSize: 5,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(list.Items) != 1 || list.Items[0].ID != sb.ID || list.Pagination.PageSize != 5 {
		t.Fatalf("list = %+v", list)
	}

	lr, _ := f.Last("GET", "/v1/sandboxes")
	if got := lr.Query["state"]; len(got) != 2 {
		t.Errorf("states went out as %v, want one state= per value", got)
	}

	none, _ := c.ListSandboxes(ctx, osbclient.ListOptions{Metadata: map[string]string{"team": "a"}})
	if len(none.Items) != 0 || none.Items == nil {
		t.Errorf("a filter matching nothing should be an empty, non-nil page: %+v", none)
	}

	at := time.Now().Add(time.Hour).UTC().Truncate(time.Second)

	got, err := c.RenewExpiration(ctx, sb.ID, at)
	if err != nil || !got.Equal(at) {
		t.Fatalf("renew = %v, %v; want %v", got, err, at)
	}

	if err := c.DeleteSandbox(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetSandbox(ctx, sb.ID)
	if osbclient.StatusOf(err) != 404 {
		t.Fatalf("get after delete: %v, want 404", err)
	}
}

func TestExecdEndpointWithoutASchemeGetsTheServersOne(t *testing.T) {
	f, c := newPair(t)
	id := f.AddSandbox("alpine")

	ex, err := c.Execd(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(ex.BaseURL(), "http://") {
		t.Fatalf("execd base = %q, want the lifecycle server's scheme prefixed", ex.BaseURL())
	}

	if err := ex.Ping(context.Background()); err != nil {
		t.Fatalf("ping with the token from the endpoint answer: %v", err)
	}

	r, _ := f.Last("GET", "/ping")
	if r.Header.Get(osbclient.ExecdTokenHeader) != "tok-"+id {
		t.Errorf("execd call carried token %q", r.Header.Get(osbclient.ExecdTokenHeader))
	}
}

func TestWaitReadyWaitsOutPendingAndGivesUpOnTheDeadline(t *testing.T) {
	f, c := newPair(t)
	f.PendingPolls = 3

	sb, err := c.CreateSandbox(context.Background(), osbclient.CreateRequest{
		Image: &osbclient.ImageSpec{URI: "alpine"}, Entrypoint: []string{"sh"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if sb.Status.State != osbclient.StatePending {
		t.Fatalf("state = %s, want Pending", sb.Status.State)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := c.WaitReady(ctx, sb.ID, time.Millisecond); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	f.PendingPolls = 1 << 30

	sb2, _ := c.CreateSandbox(context.Background(), osbclient.CreateRequest{
		Image: &osbclient.ImageSpec{URI: "alpine"}, Entrypoint: []string{"sh"},
	})

	short, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()

	_, err = c.WaitReady(short, sb2.ID, 5*time.Millisecond)
	if !errors.Is(err, osbclient.ErrNotReady) {
		t.Fatalf("a sandbox that never comes up: %v, want ErrNotReady", err)
	}
}

func TestWaitReadyFailsFastOnAnUnknownSandbox(t *testing.T) {
	_, c := newPair(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()

	_, err := c.WaitReady(ctx, "osb-nope", time.Millisecond)
	if osbclient.StatusOf(err) != 404 || time.Since(start) > time.Second {
		t.Fatalf("WaitReady on a missing sandbox: %v after %v, want an immediate 404", err, time.Since(start))
	}
}

func TestRunCommandInBothFramings(t *testing.T) {
	for _, standard := range []bool{false, true} {
		name := map[bool]string{false: "bare JSON frames", true: "standard SSE"}[standard]

		t.Run(name, func(t *testing.T) {
			f, c := newPair(t)
			f.StandardSSE = standard
			ex, _ := c.Execd(context.Background(), f.AddSandbox("alpine"))

			x, err := ex.RunCommand(context.Background(), osbclient.CommandRequest{
				Command: "echo hello; warn careful; env FOO", Envs: map[string]string{"FOO": "bar"},
			})
			if err != nil {
				t.Fatal(err)
			}

			if x.ID == nil || *x.ID == "" {
				t.Error("no execution id from the init event")
			}

			if len(x.Logs.Stdout) != 2 || x.Logs.Stdout[0].Text != "hello" || x.Logs.Stdout[1].Text != "bar" {
				t.Errorf("stdout = %+v", x.Logs.Stdout)
			}

			if len(x.Logs.Stderr) != 1 || !x.Logs.Stderr[0].IsError {
				t.Errorf("stderr = %+v", x.Logs.Stderr)
			}

			if x.ExitCode == nil || *x.ExitCode != 0 || x.Complete == nil {
				t.Errorf("a clean run: exit %v complete %v, want 0 and a completion", x.ExitCode, x.Complete)
			}

			x, err = ex.RunCommand(context.Background(), osbclient.CommandRequest{Command: "echo x; exit 3"})
			if err != nil {
				t.Fatal(err)
			}

			if x.ExitCode == nil || *x.ExitCode != 3 || x.Error == nil || x.Complete != nil {
				t.Errorf("exit 3: code %v error %+v", x.ExitCode, x.Error)
			}
		})
	}
}

func TestBackgroundStopsAtExecutionComplete(t *testing.T) {
	f, c := newPair(t)
	ex, _ := c.Execd(context.Background(), f.AddSandbox("alpine"))

	// The fake holds the stream open for 5s after execution_complete, as execd does. A client
	// that waits for the end of the stream instead takes that long.
	start := time.Now()

	x, err := ex.RunCommand(context.Background(), osbclient.CommandRequest{Command: "sleep", Background: true})
	if err != nil {
		t.Fatal(err)
	}

	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("background run returned after %v; it should stop at execution_complete", d)
	}

	if x.ID == nil || x.ExitCode != nil {
		t.Errorf("background: id %v exit %v, want an id and no exit code yet", x.ID, x.ExitCode)
	}
}

func TestInterruptEndsARunningCommand(t *testing.T) {
	f, c := newPair(t)
	ex, _ := c.Execd(context.Background(), f.AddSandbox("alpine"))

	ids := make(chan string, 1)
	done := make(chan *osbclient.Execution, 1)

	go func() {
		x := osbclient.NewExecution()
		_ = ex.StreamCommand(context.Background(), osbclient.CommandRequest{Command: "sleep"}, func(ev osbclient.Event) error {
			x.Apply(ev)
			if ev.Type == "init" {
				ids <- ev.Text
			}

			return nil
		})
		x.InferExitCode()
		done <- x
	}()

	id := <-ids
	for f.Running() == 0 {
		time.Sleep(time.Millisecond)
	}

	if err := ex.InterruptCommand(context.Background(), id); err != nil {
		t.Fatal(err)
	}

	select {
	case x := <-done:
		if x.ExitCode == nil || *x.ExitCode != 130 {
			t.Errorf("interrupted command exit = %v, want 130", x.ExitCode)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("interrupt did not end the command")
	}
}

func TestFiles(t *testing.T) {
	f, c := newPair(t)
	id := f.AddSandbox("alpine")
	ex, _ := c.Execd(context.Background(), id)
	ctx := context.Background()

	if err := ex.WriteFile(ctx, "/app/main.py", []byte("print('localhost')\n"), osbclient.Permission{Mode: 644, Owner: "app"}); err != nil {
		t.Fatal(err)
	}

	if got, _ := f.File(id, "/app/main.py"); got != "print('localhost')\n" {
		t.Fatalf("uploaded content = %q", got)
	}

	data, err := ex.ReadFile(ctx, "/app/main.py", "bytes=0-4")
	if err != nil || string(data) != "print" {
		t.Fatalf("range read = %q, %v", data, err)
	}

	counts, err := ex.ReplaceInFiles(ctx, map[string]osbclient.Replacement{"/app/main.py": {Old: "localhost", New: "0.0.0.0"}})
	if err != nil || counts["/app/main.py"] != 1 {
		t.Fatalf("replace = %v, %v", counts, err)
	}

	found, err := ex.SearchFiles(ctx, "/app", "*.py")
	if err != nil || len(found) != 1 || found[0].Path != "/app/main.py" || found[0].Mode != 644 {
		t.Fatalf("search = %+v, %v", found, err)
	}

	st, err := ex.Stat(ctx, "/app/main.py")
	if err != nil || st["/app/main.py"].Owner != "app" {
		t.Fatalf("stat = %+v, %v", st, err)
	}

	if err := ex.MoveFiles(ctx, osbclient.Move{Src: "/app/main.py", Dest: "/app/app.py"}); err != nil {
		t.Fatal(err)
	}

	if _, ok := f.File(id, "/app/app.py"); !ok {
		t.Fatal("move did not land")
	}

	if err := ex.DeleteFiles(ctx, "/app/app.py"); err != nil {
		t.Fatal(err)
	}

	if err := ex.DeleteFiles(ctx, "/app/app.py"); osbclient.StatusOf(err) != 404 {
		t.Fatalf("deleting a missing file: %v, want a 404", err)
	}

	if err := ex.MakeDirs(ctx, map[string]osbclient.Permission{"/work/a": {Mode: 700}}); err != nil {
		t.Fatal(err)
	}

	if m, ok := f.Dir(id, "/work/a"); !ok || m != 700 {
		t.Fatalf("mkdir: mode %d ok %v", m, ok)
	}

	if err := ex.DeleteDirs(ctx, "/work/a"); err != nil {
		t.Fatal(err)
	}

	if _, ok := f.Dir(id, "/work/a"); ok {
		t.Fatal("rmdir did not land")
	}

	m, err := ex.Metrics(ctx)
	if err != nil || m.CPUCount != 2 || m.MemUsedMiB != 256 {
		t.Fatalf("metrics = %+v, %v", m, err)
	}
}

func TestMetadataFilterEncoding(t *testing.T) {
	got := osbclient.EncodeMetadataFilter(map[string]string{"b": "x y", "a": "1&2=3"})
	if got != "a=1%262%3D3&b=x%20y" {
		t.Fatalf("EncodeMetadataFilter = %q", got)
	}

	// And the server's one layer of decoding gives the pairs back.
	v, _ := url.ParseQuery(got)
	if v.Get("a") != "1&2=3" || v.Get("b") != "x y" {
		t.Fatalf("decoded = %v", v)
	}
}
