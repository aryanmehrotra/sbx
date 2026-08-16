package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func specFile(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "sandbox.json")

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

// The packed image must start the base image's OWN process. A wrapper that invented one would
// work for postgres and be quietly wrong for the next thing somebody packed - and "quietly
// wrong" here reads as "the database is broken".
func TestPackStartsTheBaseImagesOwnProcess(t *testing.T) {
	path := specFile(t, `{"version":1,"services":{
		"db":{"image":"postgres:16","ports":[5432],"env":{"POSTGRES_DB":"app"}}}}`)

	out := t.TempDir()

	err := Pack(context.Background(), PackOptions{
		Spec: path, Out: out, Version: "v1.2.3", Out2: &bytes.Buffer{},
		Inspect: func(string) ([]string, []string, error) {
			return []string{"docker-entrypoint.sh"}, []string{"postgres"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	script, err := os.ReadFile(filepath.Join(out, "db", "start.sh"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(script), "docker-entrypoint.sh") || !strings.Contains(string(script), "postgres") {
		t.Errorf("start.sh does not run the image's own entrypoint:\n%s", script)
	}

	if !strings.Contains(string(script), "--front=\"db=5432\"") {
		t.Errorf("start.sh does not front the service's port:\n%s", script)
	}

	if !strings.Contains(string(script), "SBX_CONNECT_TOKEN") {
		t.Error("start.sh does not refuse to run without a token")
	}

	dockerfile, err := os.ReadFile(filepath.Join(out, "db", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(dockerfile), "FROM postgres:16") {
		t.Errorf("Dockerfile does not build on the declared image:\n%s", dockerfile)
	}

	if !strings.Contains(string(dockerfile), `ENV POSTGRES_DB="app"`) {
		t.Errorf("the spec's env did not reach the image:\n%s", dockerfile)
	}
}

// @latest would install an sbx older than the tunnel it depends on, and the container would
// die at startup on an unknown flag - which reads as a broken generator rather than a wrong
// version.
func TestPackNeverInstallsLatest(t *testing.T) {
	for version, want := range map[string]string{
		"v0.3.0": "@v0.3.0",
		"dev":    "@main",
		"":       "@main",
	} {
		if got := packVersion(version); !strings.HasSuffix("@"+got, want) {
			t.Errorf("packVersion(%q) = %q, want %q", version, got, want)
		}
	}
}

// The comment above the install line has to describe the line below it. It explained why main
// was pinned instead of a release - true while there was no release carrying the tunnel, and
// wrong the moment there was, in a file whose whole job is to be read by somebody asking why.
func TestThePinExplainsItself(t *testing.T) {
	released := pinNote("v0.3.0")
	if !strings.Contains(released, "v0.3.0") {
		t.Errorf("a release pin does not name the version it pins:\n%s", released)
	}

	if strings.Contains(released, "--connect-addr") {
		t.Errorf("a release pin still explains itself as a workaround for releases that "+
			"lack the tunnel, which is the thing it now has:\n%s", released)
	}

	if dev := pinNote("main"); !strings.Contains(dev, "--connect-addr") {
		t.Errorf("a development pin no longer says why it cannot take a release:\n%s", dev)
	}
}

// Several ports on one service each need their own name, or --front sees one entry twice.
func TestPackNamesEveryPort(t *testing.T) {
	if got := front("api", []int{8080}); got != "api=8080" {
		t.Errorf("front with one port = %q", got)
	}

	got := front("api", []int{8080, 9090})
	if !strings.Contains(got, "8080") || !strings.Contains(got, "9090") || !strings.Contains(got, ",") {
		t.Errorf("front with two ports = %q, want both named and separated", got)
	}
}

// A service built from source has no image to wrap, and saying so beats generating a
// Dockerfile whose FROM line is empty.
func TestPackRefusesAServiceWithNoImage(t *testing.T) {
	path := specFile(t, `{"version":1,"services":{
		"app":{"build":{"context":"."},"ports":[8080]}}}`)

	err := Pack(context.Background(), PackOptions{
		Spec: path, Out: t.TempDir(), Out2: &bytes.Buffer{},
		Inspect: func(string) ([]string, []string, error) { return nil, nil, nil },
	})

	if err == nil || !strings.Contains(err.Error(), "image") {
		t.Errorf("packing a build-from-source service gave %v, want a refusal naming image", err)
	}
}
