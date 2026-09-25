package osb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/history"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

func TestAuthAcceptsOneKeyInEitherHeader(t *testing.T) {
	h := newHarness(t, func(_ *harness, o *Options) { o.Key = "s3cret" })

	cases := []struct {
		name   string
		hdr    []string
		status int
		code   string
	}{
		{"missing", nil, 401, "MISSING_API_KEY"},
		{"wrong", []string{"OPEN-SANDBOX-API-KEY", "nope"}, 401, "INVALID_API_KEY"},
		{"prefix of the key", []string{"OPEN-SANDBOX-API-KEY", "s3cre"}, 401, "INVALID_API_KEY"},
		{"lifecycle header", []string{"OPEN-SANDBOX-API-KEY", "s3cret"}, 200, ""},
		{"server-proxy header", []string{"X-API-Key", "s3cret"}, 200, ""},
	}

	for _, c := range cases {
		resp := h.do("GET", "/v1/sandboxes", nil, nil, c.hdr...)
		if resp.StatusCode != c.status {
			t.Errorf("%s: status %d, want %d", c.name, resp.StatusCode, c.status)
			continue
		}

		if c.code != "" {
			if e := h.errOf(resp); e.Code != c.code {
				t.Errorf("%s: code %q, want %q", c.name, e.Code, c.code)
			}
		}
	}

	if resp := h.do("GET", "/health", nil, nil); resp.StatusCode != 200 {
		t.Errorf("/health needs no key, got %d", resp.StatusCode)
	}

	if resp := h.do("GET", "/v1/sandboxes", nil, nil); resp.Header.Get("X-Request-ID") == "" {
		t.Error("no X-Request-ID on an error response")
	}
}

func TestNoKeyMeansNoAuthentication(t *testing.T) {
	h := newHarness(t)

	if resp := h.do("GET", "/v1/sandboxes", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("with no key configured a request was refused: %d", resp.StatusCode)
	}
}

func TestCheckBindRefusesAnOpenNonLoopbackAPI(t *testing.T) {
	for addr, key := range map[string]string{"127.0.0.1:8080": "", "localhost:8080": "", "[::1]:8080": "",
		"0.0.0.0:8080": "k", ":8080": "k"} {
		if err := CheckBind(addr, key); err != nil {
			t.Errorf("CheckBind(%q, %q) = %v, want ok", addr, key, err)
		}
	}

	for _, addr := range []string{"0.0.0.0:8080", ":8080", "10.0.0.5:8080"} {
		err := CheckBind(addr, "")
		if err == nil || !strings.Contains(err.Error(), "--osb-key") {
			t.Errorf("CheckBind(%q, \"\") = %v, want a refusal naming --osb-key", addr, err)
		}
	}
}

func TestCreateIsPendingThenRunning(t *testing.T) {
	h := newHarness(t)

	h.mu.Lock()
	h.pingErr = errNotYet
	h.mu.Unlock()

	body := minimalCreate()
	body["env"] = map[string]string{"FOO": "bar"}
	body["metadata"] = map[string]string{"team": "ml"}
	body["extensions"] = map[string]string{"opensandbox.extensions.k": "v"}

	var created sandboxJSON
	resp := h.do("POST", "/v1/sandboxes", body, &created)

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create = %d, want 202", resp.StatusCode)
	}

	if !validID(created.ID) || resp.Header.Get("Location") != "/v1/sandboxes/"+created.ID {
		t.Fatalf("id %q / Location %q", created.ID, resp.Header.Get("Location"))
	}

	if created.Status.State != statePending || created.Image != nil || created.ExpiresAt == nil {
		t.Fatalf("create response: %+v - want Pending, no image, an expiresAt", created)
	}

	if want := h.clock().Add(600 * time.Second); !created.ExpiresAt.Equal(want) {
		t.Fatalf("expiresAt %s, want createdAt+timeout %s", created.ExpiresAt, want)
	}

	// Still Pending while execd does not answer, however long docker took.
	time.Sleep(100 * time.Millisecond)

	var mid sandboxJSON
	h.do("GET", "/v1/sandboxes/"+created.ID, nil, &mid)

	if mid.Status.State != statePending {
		t.Fatalf("state %s before execd answered, want Pending", mid.Status.State)
	}

	h.mu.Lock()
	h.pingErr = nil
	h.mu.Unlock()

	got := h.waitState(created.ID, stateRunning)

	if got.Image == nil || got.Image.URI != "python:3.11-slim" || got.Metadata["team"] != "ml" ||
		got.Extensions["opensandbox.extensions.k"] != "v" {
		t.Fatalf("GET lost what was created: %+v", got)
	}

	if !slices.Equal(got.Entrypoint, []string{"tail", "-f", "/dev/null"}) {
		t.Fatalf("entrypoint reported as %v, want the caller's, not the execd wrapper", got.Entrypoint)
	}

	// The container the API asked for, which is what actually matters.
	svc := h.p.service(created.ID)

	wantEntry := []string{"/opt/sbx/sbx", "execd", "--addr", ":44772", "--", "tail", "-f", "/dev/null"}
	if !slices.Equal(svc.Entrypoint, wantEntry) {
		t.Errorf("container entrypoint %v, want %v", svc.Entrypoint, wantEntry)
	}

	if svc.ReadOnlyVolumes["sbx-execd-test"] != "/opt/sbx" {
		t.Errorf("execd volume not mounted read-only at /opt/sbx: %v", svc.ReadOnlyVolumes)
	}

	// One runc exec per check per sandbox: quick while execd comes up, then once a minute - the
	// wake path runs the check itself and does not wait on docker's.
	if svc.HealthInterval != "60s" || svc.HealthStartInterval != "1s" {
		t.Errorf("health every %q, %q while starting; want 60s, 1s", svc.HealthInterval, svc.HealthStartInterval)
	}

	if svc.Env["FOO"] != "bar" || len(svc.Env[tokenEnv]) < 32 {
		t.Errorf("env %v: want the caller's plus a minted %s", svc.Env, tokenEnv)
	}

	if !slices.Equal(svc.Ports, []int{44772}) || svc.CPU != "0.5" || svc.Memory != "536870912" {
		t.Errorf("ports %v cpu %q memory %q", svc.Ports, svc.CPU, svc.Memory)
	}

	if svc.OnIdle != spec.OnIdleFreeze {
		t.Errorf("on_idle %q: an API sandbox must freeze by default, so background processes survive", svc.OnIdle)
	}

	if !strings.Contains(h.rt.seen(), "refresh") {
		t.Error("the daemon was not asked to front the new sandbox")
	}
}

// The endpoint handed out must work, so Running is decided by execd answering through it -
// and a container whose entrypoint dies is Failed with what it printed, not a timeout later.
func TestCreateFailsWithTheContainersOutputWhenItExits(t *testing.T) {
	h := newHarness(t)
	h.p.exitOnStart = true
	h.p.logs = "python3: can't open file '/app/main.py'\n"

	got := h.create(minimalCreate())

	if got.Status.State != stateFailed || got.Status.Reason != "runtime_error" ||
		!strings.Contains(got.Status.Message, "/app/main.py") {
		t.Fatalf("status %+v, want Failed runtime_error carrying the container's output", got.Status)
	}
}

func TestCreateTimesOutWhenExecdNeverAnswers(t *testing.T) {
	h := newHarness(t, func(_ *harness, o *Options) { o.ReadyTimeout = 200 * time.Millisecond })

	h.mu.Lock()
	h.pingErr = errNotYet
	h.mu.Unlock()

	var created sandboxJSON
	h.do("POST", "/v1/sandboxes", minimalCreate(), &created)

	// The deadline is on the injected clock, so move it.
	time.Sleep(50 * time.Millisecond)
	h.advance(time.Second)

	got := h.waitState(created.ID, stateFailed)
	if got.Status.Reason != "provision_timeout" || !strings.Contains(got.Status.Message, "not listening") {
		t.Fatalf("status %+v, want provision_timeout naming the last error", got.Status)
	}
}

func TestCreateUsesTheImagesCommandWhenNoEntrypointIsGiven(t *testing.T) {
	h := newHarness(t)

	body := minimalCreate()
	delete(body, "entrypoint")

	got := h.create(body)
	if !slices.Equal(got.Entrypoint, []string{"python3"}) {
		t.Fatalf("entrypoint %v, want the image's CMD", got.Entrypoint)
	}
}

func TestCreateRefusesWhatItCannotDo(t *testing.T) {
	h := newHarness(t)

	with := func(k string, v any) map[string]any {
		b := minimalCreate()
		b[k] = v

		return b
	}

	cases := []struct {
		name   string
		body   any
		status int
		code   string
		says   string
	}{
		{"not json", "{", 400, "SANDBOX::INVALID_PARAMETER", "CreateSandboxRequest"},
		{"no image", map[string]any{"entrypoint": []string{"x"}}, 400, "SANDBOX::INVALID_PARAMETER", "image.uri"},
		{"short timeout", with("timeout", 30), 400, "SANDBOX::INVALID_PARAMETER", "60"},
		{"image and snapshot", with("snapshotId", "snap-000000000000"), 400, "SANDBOX::INVALID_PARAMETER", "exactly one"},
		{"image and template", with("templateId", "tpl-000000000000"), 400, "SANDBOX::INVALID_PARAMETER", "exactly one"},
		{"ossfs", with("volumes", []any{map[string]any{"name": "o", "ossfs": map[string]any{}}}), 400, "VOLUME::INVALID_BACKEND", "Alibaba"},
		{"host volume, no roots", with("volumes", []any{map[string]any{"name": "h", "host": map[string]any{"path": "/x"}, "mountPath": "/x"}}), 400, "VOLUME::HOST_PATH_NOT_ALLOWED", "--osb-host-paths"},
		{"bad metadata", with("metadata", map[string]string{"bad key": "v"}), 400, "SANDBOX::INVALID_METADATA_LABEL", "bad key"},
		{"reserved metadata", with("metadata", map[string]string{"opensandbox.io/x": "v"}), 400, "SANDBOX::INVALID_METADATA_LABEL", "reserved"},
		{"unknown limit", with("resourceLimits", map[string]string{"ephemeral-storage": "1Gi"}), 400, "SANDBOX::INVALID_PARAMETER", "ephemeral-storage"},
		{"docker memory", with("resourceLimits", map[string]string{"memory": "512m"}), 400, "SANDBOX::INVALID_PARAMETER", "512Mi"},
		{"token env", with("env", map[string]string{"EXECD_ACCESS_TOKEN": "x"}), 400, "SANDBOX::INVALID_PARAMETER", "reserved"},
		{"idle", with("extensions", map[string]string{"sbx.idle": "nap"}), 400, "SANDBOX::INVALID_PARAMETER", "sleep"},
		{"ports", with("extensions", map[string]string{"sbx.ports": "80,x"}), 400, "SANDBOX::INVALID_PARAMETER", "sbx.ports"},
		{"image auth", with("image", map[string]any{"uri": "x", "auth": map[string]string{"username": "u"}}), 501, "SANDBOX::API_NOT_SUPPORTED", "docker login"},
		{"secure access", with("secureAccess", true), 501, "SANDBOX::API_NOT_SUPPORTED", "v0.11.0"},
		{"windows", with("platform", map[string]string{"os": "windows", "arch": "amd64"}), 400, "SANDBOX::INVALID_PARAMETER", "linux"},
		{"empty entrypoint", with("entrypoint", []string{""}), 400, "SANDBOX::INVALID_ENTRYPOINT", "program"},
	}

	for _, c := range cases {
		resp := h.do("POST", "/v1/sandboxes", c.body, nil)
		if resp.StatusCode != c.status {
			t.Errorf("%s: %d, want %d", c.name, resp.StatusCode, c.status)
			continue
		}

		if e := h.errOf(resp); e.Code != c.code || !strings.Contains(e.Message, c.says) {
			t.Errorf("%s: %+v, want code %s mentioning %q", c.name, e, c.code, c.says)
		}
	}

	// Nothing refused may have left a sandbox behind.
	var list listJSON
	h.do("GET", "/v1/sandboxes", nil, &list)

	if list.Pagination.TotalItems != 0 {
		t.Fatalf("refused creates left %d sandboxes", list.Pagination.TotalItems)
	}

	// And an empty or allow-all policy is what sbx already does, so it is accepted.
	for _, np := range []any{map[string]any{}, map[string]any{"defaultAction": "allow"}} {
		if resp := h.do("POST", "/v1/sandboxes", with("networkPolicy", np), nil); resp.StatusCode != 202 {
			t.Errorf("allow-all networkPolicy %v refused: %d", np, resp.StatusCode)
		}
	}
}

func TestUnknownSandboxIs404InTheSpecShape(t *testing.T) {
	h := newHarness(t)

	for _, id := range []string{"non-existent-sandbox-id-12345", "osb-000000000000", "..%2F..%2Fetc"} {
		resp := h.do("GET", "/v1/sandboxes/"+id, nil, nil)
		if resp.StatusCode != 404 {
			t.Errorf("GET %s = %d, want 404", id, resp.StatusCode)
			continue
		}

		if e := h.errOf(resp); e.Code != "SANDBOX::NOT_FOUND" {
			t.Errorf("GET %s code %q", id, e.Code)
		}
	}

	if resp := h.do("GET", "/v1/nothing", nil, nil); resp.StatusCode != 404 || h.errOf(resp).Code != "NOT_FOUND" {
		t.Errorf("unknown path: %d", resp.StatusCode)
	}

	if resp := h.do("PUT", "/v1/sandboxes", nil, nil); resp.StatusCode != 405 || h.errOf(resp).Code != "METHOD_NOT_ALLOWED" {
		t.Errorf("wrong method: %d", resp.StatusCode)
	}
}

func TestListFiltersByStateAndMetadataAndPaginates(t *testing.T) {
	h := newHarness(t)

	var ids []string

	for _, team := range []string{"a", "b", "a"} {
		b := minimalCreate()
		b["metadata"] = map[string]string{"team": team, "note": "Demo.Test"}
		ids = append(ids, h.create(b).ID)
		h.advance(time.Second) // distinct createdAt, so the order is defined
	}

	h.do("POST", "/v1/sandboxes/"+ids[1]+"/pause", nil, nil)

	var list listJSON
	h.do("GET", "/v1/sandboxes?state=Running", nil, &list)

	if list.Pagination.TotalItems != 2 {
		t.Fatalf("state=Running matched %d, want 2", list.Pagination.TotalItems)
	}

	h.do("GET", "/v1/sandboxes?state=Running&state=Paused", nil, &list)
	if list.Pagination.TotalItems != 3 {
		t.Fatalf("state OR matched %d, want 3", list.Pagination.TotalItems)
	}

	// metadata is a url-encoded query inside the query.
	h.do("GET", "/v1/sandboxes?metadata=team%3Da%26note%3DDemo.Test", nil, &list)
	if list.Pagination.TotalItems != 2 || list.Items[0].ID != ids[0] || list.Items[1].ID != ids[2] {
		t.Fatalf("metadata filter: %+v", list.Pagination)
	}

	h.do("GET", "/v1/sandboxes?page=2&pageSize=2", nil, &list)

	want := paginationJSON{Page: 2, PageSize: 2, TotalItems: 3, TotalPages: 2, HasNextPage: false}
	if list.Pagination != want || len(list.Items) != 1 || list.Items[0].ID != ids[2] {
		t.Fatalf("page 2: %+v with %d items", list.Pagination, len(list.Items))
	}

	h.do("GET", "/v1/sandboxes?page=1&pageSize=2", nil, &list)
	if !list.Pagination.HasNextPage {
		t.Fatal("page 1 of 2 says there is no next page")
	}

	h.do("GET", "/v1/sandboxes?page=9", nil, &list)
	if list.Items == nil || len(list.Items) != 0 {
		t.Fatal("a page past the end must be an empty array, not null")
	}

	if resp := h.do("GET", "/v1/sandboxes?pageSize=0", nil, nil); resp.StatusCode != 400 {
		t.Fatalf("pageSize=0 = %d, want 400", resp.StatusCode)
	}
}

// Sandboxes nobody created through the API - somebody's database stack - are not the API's to
// list, and so are never the API's to delete.
func TestListShowsOnlyAPISandboxes(t *testing.T) {
	h := newHarness(t)
	h.create(minimalCreate())

	_ = h.p.Create(t.Context(), "zopnight", 3, 0, "mysql", spec.Service{Image: "mysql", Ports: []int{3306}}, nil, "", "")

	var list listJSON
	h.do("GET", "/v1/sandboxes", nil, &list)

	if list.Pagination.TotalItems != 1 {
		t.Fatalf("listed %d sandboxes, want only the API's one", list.Pagination.TotalItems)
	}
}

func TestPatchMetadataIsAMergePatch(t *testing.T) {
	h := newHarness(t)

	b := minimalCreate()
	b["metadata"] = map[string]string{"team": "t1", "env": "prod"}
	id := h.create(b).ID

	var got sandboxJSON
	resp := h.do("PATCH", "/v1/sandboxes/"+id+"/metadata", `{"env":"stage","team":null,"new":"x"}`, &got)

	if resp.StatusCode != 200 || got.Metadata["env"] != "stage" || got.Metadata["new"] != "x" {
		t.Fatalf("patch = %d %+v", resp.StatusCode, got.Metadata)
	}

	if _, ok := got.Metadata["team"]; ok {
		t.Fatal("null did not delete the key")
	}

	h.do("GET", "/v1/sandboxes/"+id, nil, &got)
	if got.Metadata["env"] != "stage" {
		t.Fatal("the patch did not persist")
	}

	for _, body := range []string{`{"opensandbox.io/x":"v"}`, `{"k":"has space"}`, `[1]`} {
		if resp := h.do("PATCH", "/v1/sandboxes/"+id+"/metadata", body, nil); resp.StatusCode != 400 {
			t.Errorf("patch %s = %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestRenewOnlyEverExtends(t *testing.T) {
	h := newHarness(t)
	sb := h.create(minimalCreate())
	path := "/v1/sandboxes/" + sb.ID + "/renew-expiration"

	at := func(d time.Duration) map[string]string {
		return map[string]string{"expiresAt": sb.ExpiresAt.Add(d).Format(time.RFC3339)}
	}

	for name, body := range map[string]any{
		"earlier":     at(-time.Minute),
		"same":        at(0),
		"in the past": map[string]string{"expiresAt": h.clock().Add(-time.Hour).Format(time.RFC3339)},
	} {
		resp := h.do("POST", path, body, nil)
		if resp.StatusCode != 400 || h.errOf(resp).Code != "SANDBOX::INVALID_EXPIRATION" {
			t.Errorf("%s: %d, want 400 INVALID_EXPIRATION", name, resp.StatusCode)
		}
	}

	var got renewResponse
	if resp := h.do("POST", path, at(time.Hour), &got); resp.StatusCode != 200 ||
		!got.ExpiresAt.Equal(sb.ExpiresAt.Add(time.Hour)) {
		t.Fatalf("renew = %d %v", resp.StatusCode, got.ExpiresAt)
	}

	var after sandboxJSON
	h.do("GET", "/v1/sandboxes/"+sb.ID, nil, &after)

	if !after.ExpiresAt.Equal(got.ExpiresAt) {
		t.Fatal("the renewed expiry did not stick")
	}

	// No timeout means no expiry, so there is nothing to renew.
	b := minimalCreate()
	delete(b, "timeout")
	forever := h.create(b)

	resp := h.do("POST", "/v1/sandboxes/"+forever.ID+"/renew-expiration", at(time.Hour), nil)
	if resp.StatusCode != 409 {
		t.Fatalf("renewing a sandbox with no expiry = %d, want 409", resp.StatusCode)
	}

	if forever.ExpiresAt != nil {
		t.Fatal("a sandbox created without timeout reports an expiresAt")
	}
}

func TestPauseAndResumeGoThroughTheDaemon(t *testing.T) {
	h := newHarness(t)
	id := h.create(minimalCreate()).ID

	if resp := h.do("POST", "/v1/sandboxes/"+id+"/pause", nil, nil); resp.StatusCode != 202 {
		t.Fatalf("pause = %d", resp.StatusCode)
	}

	if got := h.waitState(id, statePaused); got.Status.Reason != "user_pause" {
		t.Fatalf("paused status %+v", got.Status)
	}

	if resp := h.do("POST", "/v1/sandboxes/"+id+"/pause", nil, nil); resp.StatusCode != 409 {
		t.Fatalf("pausing twice = %d, want 409", resp.StatusCode)
	}

	if resp := h.do("POST", "/v1/sandboxes/"+id+"/resume", nil, nil); resp.StatusCode != 202 {
		t.Fatalf("resume = %d", resp.StatusCode)
	}

	h.waitState(id, stateRunning)

	if resp := h.do("POST", "/v1/sandboxes/"+id+"/resume", nil, nil); resp.StatusCode != 409 {
		t.Fatalf("resuming a running sandbox = %d, want 409", resp.StatusCode)
	}

	if got := h.rt.seen(); !strings.Contains(got, "freeze "+id) || !strings.Contains(got, "thaw "+id) {
		t.Fatalf("the daemon saw %q; pause and resume must go through it, not around it", got)
	}
}

// A provider that cannot keep memory must refuse pause, not approximate it with a stop.
func TestPauseIsRefusedWithoutAPauser(t *testing.T) {
	h := newHarness(t, func(h *harness, o *Options) { o.Provider = noPause{h.p} })
	id := h.create(minimalCreate()).ID

	if svc := h.p.service(id); svc.OnIdle != "" {
		t.Fatalf("on_idle %q on a provider that cannot pause; idle must fall back to stop", svc.OnIdle)
	}

	resp := h.do("POST", "/v1/sandboxes/"+id+"/pause", nil, nil)
	if resp.StatusCode != 501 || !strings.Contains(h.errOf(resp).Message, "cannot pause") {
		t.Fatalf("pause without a Pauser = %d", resp.StatusCode)
	}
}

// noPause hides the fake's Pauser, the way the kubernetes provider lacks one.
type noPause struct{ *fakeDocker }

func (noPause) Pause() {}

func TestEndpointResolution(t *testing.T) {
	h := newHarness(t)

	b := minimalCreate()
	b["extensions"] = map[string]string{"sbx.ports": "8080"}
	sb := h.create(b)

	base := "/v1/sandboxes/" + sb.ID + "/endpoints/"

	var execd endpointJSON
	h.do("GET", base+"44772?use_server_proxy=false", nil, &execd)

	svc := h.p.service(sb.ID)
	us, _ := h.p.List(t.Context(), sb.ID)

	if execd.Endpoint != us[0].Client[0].String() || execd.Headers[tokenHeader] != svc.Env[tokenEnv] {
		t.Fatalf("execd endpoint %+v, want the wake port with the minted token", execd)
	}

	var declared endpointJSON
	h.do("GET", base+"8080", nil, &declared)

	if declared.Endpoint != us[0].Client[1].String() || declared.Headers != nil {
		t.Fatalf("declared port endpoint %+v, want its own wake port", declared)
	}

	var other endpointJSON
	h.do("GET", base+"3000", nil, &other)

	if other.Endpoint != us[0].Client[0].String()+"/proxy/3000" || other.Headers[tokenHeader] == "" {
		t.Fatalf("undeclared port endpoint %+v, want execd's /proxy/3000", other)
	}

	for path, status := range map[string]int{
		base + "0":                           400,
		base + "x":                           400,
		base + "44772?use_server_proxy=true": 501,
		base + "44772?expires=99":            501,
	} {
		if resp := h.do("GET", path, nil, nil); resp.StatusCode != status {
			t.Errorf("GET %s = %d, want %d", path, resp.StatusCode, status)
		}
	}
}

func TestDeleteRemovesTheContainerAndTheRecord(t *testing.T) {
	h := newHarness(t)
	id := h.create(minimalCreate()).ID

	if resp := h.do("DELETE", "/v1/sandboxes/"+id, nil, nil); resp.StatusCode != 204 {
		t.Fatalf("delete = %d", resp.StatusCode)
	}

	if resp := h.do("GET", "/v1/sandboxes/"+id, nil, nil); resp.StatusCode != 404 {
		t.Fatalf("GET after delete = %d", resp.StatusCode)
	}

	if !slices.Contains(h.p.removed, id) {
		t.Fatal("the container was not removed")
	}

	if _, err := os.Stat(filepath.Join(h.dir, id+".json")); !os.IsNotExist(err) {
		t.Fatalf("the state file outlived the sandbox: %v", err)
	}

	if resp := h.do("DELETE", "/v1/sandboxes/"+id, nil, nil); resp.StatusCode != 404 {
		t.Fatalf("second delete = %d, want 404", resp.StatusCode)
	}
}

// Expiry has to survive the daemon restarting - otherwise upgrading sbx makes every sandbox
// immortal - and so does a pause, which the daemon forgets when it exits.
func TestStateSurvivesARestart(t *testing.T) {
	h := newHarness(t)

	b := minimalCreate()
	b["metadata"] = map[string]string{"team": "ml"}
	first := h.create(b)

	h.do("POST", "/v1/sandboxes/"+first.ID+"/pause", nil, nil)

	// A second server over the same directory, as after `sbx serve` restarts.
	h.http.Close()
	h.srv.Close()
	h.rt = &fakeRuntime{}
	h.start()

	ctx, cancel := context.WithCancel(t.Context())
	go h.srv.Run(ctx)

	defer cancel()

	var got sandboxJSON
	h.do("GET", "/v1/sandboxes/"+first.ID, nil, &got)

	if got.Status.State != statePaused || !got.ExpiresAt.Equal(*first.ExpiresAt) || got.Metadata["team"] != "ml" {
		t.Fatalf("after restart: %+v", got)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		h.rt.mu.Lock()
		held := h.rt.held[first.ID]
		h.rt.mu.Unlock()

		if held {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("the pause was not re-asserted to the daemon after a restart")
		}

		time.Sleep(10 * time.Millisecond)
	}

	raw, err := os.ReadFile(filepath.Join(h.dir, first.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}

	if info, _ := os.Stat(filepath.Join(h.dir, first.ID+".json")); info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode %v: it holds execd's token, so 0600", info.Mode().Perm())
	}

	var rec record
	if err := json.Unmarshal(raw, &rec); err != nil || rec.Token == "" {
		t.Fatalf("state file: %v %+v", err, rec)
	}
}

func TestReaperRemovesExpiredSandboxesAndSaysWho(t *testing.T) {
	h := newHarness(t)
	sb := h.create(minimalCreate())

	b := minimalCreate()
	delete(b, "timeout")
	forever := h.create(b)

	h.srv.reap(t.Context())

	if resp := h.do("GET", "/v1/sandboxes/"+sb.ID, nil, nil); resp.StatusCode != 200 {
		t.Fatal("reaped before it expired")
	}

	h.advance(601 * time.Second)
	h.srv.reap(t.Context())

	if resp := h.do("GET", "/v1/sandboxes/"+sb.ID, nil, nil); resp.StatusCode != 404 {
		t.Fatal("an expired sandbox was not removed")
	}

	if resp := h.do("GET", "/v1/sandboxes/"+forever.ID, nil, nil); resp.StatusCode != 200 {
		t.Fatal("a sandbox without a timeout was reaped")
	}

	recs, _ := history.Read(history.Filter{Sandbox: sb.ID})

	found := false
	for _, r := range recs {
		if r.Event == "removed" && r.Actor == "expiry" {
			found = true
		}
	}

	if !found {
		t.Fatalf("history does not say expiry removed it: %+v", recs)
	}
}

func TestMetricsEventsAreRecorded(t *testing.T) {
	h := newHarness(t)

	resp := h.do("POST", "/v1/metrics/events",
		`{"eventType":"sandbox.create","sandboxId":"osb-aaaaaaaaaaaa","image":"python:3.11","createDurationMs":1842,"success":true}`,
		nil, "User-Agent", "OpenSandbox-Go-SDK/0.1.0")
	if resp.StatusCode != 204 {
		t.Fatalf("metrics = %d, want 204", resp.StatusCode)
	}

	recs, _ := history.Read(history.Filter{Sandbox: "osb-aaaaaaaaaaaa"})
	if len(recs) != 1 || recs[0].DurationMs != 1842 || !strings.Contains(recs[0].Message, "Go-SDK") {
		t.Fatalf("history: %+v", recs)
	}

	for _, body := range []string{`{"eventType":"other","createDurationMs":1,"success":true}`,
		`{"eventType":"sandbox.create","success":true}`, `{"eventType":"sandbox.create","createDurationMs":1}`} {
		if resp := h.do("POST", "/v1/metrics/events", body, nil); resp.StatusCode != 400 {
			t.Errorf("metrics %s = %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestDiagnostics(t *testing.T) {
	h := newHarness(t)
	id := h.create(minimalCreate()).ID
	base := "/v1/sandboxes/" + id + "/diagnostics/"

	var d diagnosticJSON
	if resp := h.do("GET", base+"logs?scope=container", nil, &d); resp.StatusCode != 200 {
		t.Fatalf("logs = %d", resp.StatusCode)
	}

	if d.Kind != "logs" || d.Delivery != "inline" || !strings.Contains(d.Content, "hello from the container") {
		t.Fatalf("logs descriptor %+v", d)
	}

	h.do("GET", base+"events?scope=lifecycle", nil, &d)
	if d.Kind != "events" || !strings.Contains(d.Content, "created") {
		t.Fatalf("events descriptor %+v", d)
	}

	resp := h.do("GET", base+"logs", nil, nil)
	raw, _ := io.ReadAll(resp.Body)

	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") || !strings.Contains(string(raw), "hello") {
		t.Fatalf("legacy logs: %s %q", resp.Header.Get("Content-Type"), raw)
	}

	if resp := h.do("GET", base+"logs?scope=kernel", nil, nil); resp.StatusCode != 400 {
		t.Fatalf("unknown scope = %d, want 400", resp.StatusCode)
	}
}

// A provider with no way to put the agent into an image (kubernetes, today) refuses at create,
// rather than creating a sandbox that can never become Running.
func TestCreateIsRefusedWithoutAnInjector(t *testing.T) {
	h := newHarness(t, func(h *harness, o *Options) { o.Provider = bare{h.p} })

	resp := h.do("POST", "/v1/sandboxes", minimalCreate(), nil)
	if resp.StatusCode != 501 || !strings.Contains(h.errOf(resp).Message, "fake") {
		t.Fatalf("create without an Injector = %d", resp.StatusCode)
	}
}

// bare is only the core Provider interface.
type bare struct{ *fakeDocker }

func (bare) ImageInfo() {}
