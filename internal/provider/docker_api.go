package provider

// A hand-rolled Docker Engine API client over the unix socket.
//
// The official SDK pulls in most of Moby to call a handful of endpoints, and this binary's
// whole argument is that it stays small enough to run unconditionally. A handful it is.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type dockerClient struct {
	http *http.Client
}

func newDockerClient(ep dockerEndpoint) *dockerClient {
	return &dockerClient{http: &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return ep.dial(ctx)
			},
		},
		// Deliberately not zero: a hung docker socket must not wedge a wake forever.
		Timeout: 60 * time.Second,
	}}
}

func (d *dockerClient) do(ctx context.Context, method, path string, out any) error {
	return d.send(ctx, method, path, nil, out)
}

// send is do with a JSON body, which the exec endpoints need.
func (d *dockerClient) send(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader

	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}

		rdr = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, rdr)
	if err != nil {
		return err
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("docker %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(body)))
	}

	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

// container is the slice of the API's container object this proxy actually reads.
type container struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

func (c container) name() string {
	if len(c.Names) == 0 {
		return c.ID
	}

	return strings.TrimPrefix(c.Names[0], "/")
}

// list returns every container carrying the sandbox label, running or not. `all=1` is the
// load-bearing part: a stopped sandbox is precisely the one this proxy exists to wake.
func (d *dockerClient) list(ctx context.Context, label string) ([]container, error) {
	filters := url.QueryEscape(fmt.Sprintf(`{"label":["%s"]}`, label))

	var cs []container
	if err := d.do(ctx, http.MethodGet, "/containers/json?all=1&filters="+filters, &cs); err != nil {
		return nil, err
	}

	return cs, nil
}

// health is the container's own opinion of whether it is serving.
//
// This exists because the obvious check - dial the published port - is a lie. Docker binds
// the host side of `-p` the instant the container starts, so a TCP connect succeeds against
// a MySQL that is still opening its data directory, and the client that gets spliced to it
// dies reading the initial handshake. Measured: the port answered in 139 ms; mysqld needed
// about a second more. Asking the container is the only honest question.
type health struct {
	State struct {
		Status string `json:"Status"`
		Health *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`

	Config struct {
		Healthcheck *struct {
			Test []string `json:"Test"`
		} `json:"Healthcheck"`
	} `json:"Config"`
}

// healthy reports whether the container is serving, and whether it could tell us at all.
// A container with no HEALTHCHECK returns ok=false, and the caller falls back to dialling.
func (d *dockerClient) healthy(ctx context.Context, id string) (serving, ok bool) {
	var h health
	if err := d.do(ctx, http.MethodGet, "/containers/"+id+"/json", &h); err != nil {
		return false, false
	}

	if h.State.Health == nil {
		return false, false
	}

	return h.State.Health.Status == "healthy", true
}

func (d *dockerClient) start(ctx context.Context, id string) error {
	err := d.do(ctx, http.MethodPost, "/containers/"+id+"/start", nil)
	// 304 is "already started", which is a race this proxy runs into by design when two
	// connections arrive at once. It is success.
	if err != nil && strings.Contains(err.Error(), "304") {
		return nil
	}

	return err
}

func (d *dockerClient) stop(ctx context.Context, id string, timeout time.Duration) error {
	secs := strconv.Itoa(int(timeout.Seconds()))

	err := d.do(ctx, http.MethodPost, "/containers/"+id+"/stop?t="+secs, nil)
	if err != nil && strings.Contains(err.Error(), "304") {
		return nil
	}

	return err
}

// exec runs a command inside a container over the API and returns its exit code.
//
// This is on the wake path. Probe used to shell out to `docker exec`, which is a process
// spawn plus the docker CLI's own startup - measured at about 150 ms - and the wake loop
// calls it repeatedly until the workload answers. So the published 191 ms wake was mostly
// this: not the container starting, but a CLI being started to ask whether it had.
//
// Three calls, because that is what the API requires: create the exec, start it, then read
// the exit code. The output is read and discarded - the exit code is the whole answer, and
// a health command's stdout is not something sbx has any business interpreting.
func (d *dockerClient) exec(ctx context.Context, id string, argv []string) (int, error) {
	var created struct {
		ID string `json:"Id"`
	}

	body := map[string]any{
		"AttachStdout": true,
		"AttachStderr": true,
		"Cmd":          argv,
	}

	if err := d.send(ctx, http.MethodPost, "/containers/"+id+"/exec", body, &created); err != nil {
		return 0, err
	}

	if created.ID == "" {
		return 0, fmt.Errorf("docker exec create returned no id")
	}

	// Detach=false, so this returns once the command has finished and its output is drained.
	// Detaching would mean polling for completion, which is the cost this is removing.
	if err := d.send(ctx, http.MethodPost, "/exec/"+created.ID+"/start",
		map[string]any{"Detach": false, "Tty": false}, nil); err != nil {
		return 0, err
	}

	var status struct {
		ExitCode int  `json:"ExitCode"`
		Running  bool `json:"Running"`
	}

	if err := d.do(ctx, http.MethodGet, "/exec/"+created.ID+"/json", &status); err != nil {
		return 0, err
	}

	// Running should be false by the time the start call returned. If docker says otherwise,
	// treat it as "not finished, so not yet a success" rather than reading a zero exit code
	// that has not been decided.
	if status.Running {
		return -1, nil
	}

	return status.ExitCode, nil
}

// healthCommand returns the check a container declares, as a shell command.
//
// From the same endpoint healthy() already uses, so asking costs one request on a unix
// socket rather than a `docker inspect` fork. That is why it is not cached: the previous
// version cached by container NAME, and names are reused - `sbx rm x && sbx create x` with
// an edited health command left the old one in the map, on the wake path, for the life of a
// daemon designed to run for weeks. A cache whose invalidation was reasoned about against a
// key that does not carry the property.
func (d *dockerClient) healthCommand(ctx context.Context, id string) (string, bool) {
	var h health
	if err := d.do(ctx, http.MethodGet, "/containers/"+id+"/json", &h); err != nil {
		return "", false
	}

	if h.Config.Healthcheck == nil || len(h.Config.Healthcheck.Test) < 2 {
		return "", false
	}

	test := h.Config.Healthcheck.Test

	// ["CMD-SHELL", "redis-cli ping"] or ["CMD", "redis-cli", "ping"].
	switch test[0] {
	case "CMD-SHELL":
		return test[1], true
	case "CMD":
		return strings.Join(test[1:], " "), true
	}

	return "", false
}
