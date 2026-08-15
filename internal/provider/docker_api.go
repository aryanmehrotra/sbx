package provider

// A hand-rolled Docker Engine API client over the unix socket.
//
// The official SDK pulls in most of Moby to call four endpoints, and this binary's whole
// argument is that it stays small enough to run unconditionally. Four endpoints it is.

import (
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
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, nil)
	if err != nil {
		return err
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
// This exists because the obvious check — dial the published port — is a lie. Docker binds
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
