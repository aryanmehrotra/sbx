package provider

// Pause and injection for the docker provider: the two capabilities a sandbox created through
// the OpenSandbox API needs and a sandbox from a sandbox.json does not.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Pause freezes the container with the cgroup freezer. Idempotent: a container that is already
// paused is the state the caller asked for, and the daemon and the API can both arrive here.
func (d *dockerProvider) Pause(ctx context.Context, ref string) error {
	err := d.api.do(ctx, http.MethodPost, "/containers/"+ref+"/pause", nil)
	if err != nil && strings.Contains(err.Error(), "already paused") {
		return nil
	}

	return err
}

// Unpause thaws it. Idempotent in the same direction - see Pauser.
func (d *dockerProvider) Unpause(ctx context.Context, ref string) error {
	err := d.api.do(ctx, http.MethodPost, "/containers/"+ref+"/unpause", nil)
	if err != nil && strings.Contains(err.Error(), "not paused") {
		return nil
	}

	return err
}

// dockerCtx is d.docker with a deadline. Injection runs containers, and a docker that has
// stopped answering must not hold a create request open forever.
func (d *dockerProvider) dockerCtx(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+d.endpoint.String())

	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}

	return strings.TrimSpace(string(out)), nil
}

func (d *dockerProvider) ImageInfo(ctx context.Context, image string) (ImageInfo, error) {
	out, err := d.dockerCtx(ctx, "image", "inspect", image)
	if err != nil {
		return ImageInfo{}, err
	}

	var got []struct {
		Os           string `json:"Os"`
		Architecture string `json:"Architecture"`
		Config       struct {
			Entrypoint []string `json:"Entrypoint"`
			Cmd        []string `json:"Cmd"`
		} `json:"Config"`
	}

	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got) == 0 {
		return ImageInfo{}, fmt.Errorf("reading image %s: unexpected inspect output: %v", image, err)
	}

	return ImageInfo{
		Entrypoint: got[0].Config.Entrypoint,
		Cmd:        got[0].Config.Cmd,
		OS:         got[0].Os,
		Arch:       got[0].Architecture,
	}, nil
}

// VolumeRuns executes the file rather than looking for it. A binary for the wrong architecture
// is present, the right size and executable, and fails only when run - which is the one way
// this goes wrong on a machine that has run both an amd64 and an arm64 runtime.
func (d *dockerProvider) VolumeRuns(ctx context.Context, volume, name, image string) bool {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	_, err := d.dockerCtx(ctx, "run", "--rm", "--network", "none",
		"-v", volume+":/opt/sbx-probe:ro",
		"--entrypoint", "/opt/sbx-probe/"+name,
		image, "version")

	return err == nil
}

// SeedFile copies through a container that is created and never started: `docker cp` writes
// into a stopped container's volumes, so the image needs no shell, no cp and no tar - which is
// what makes this work for a distroless image as well as for python:3.11-slim.
func (d *dockerProvider) SeedFile(ctx context.Context, volume, name, hostPath, image string) error {
	if _, err := d.dockerCtx(ctx, "volume", "create", volume); err != nil {
		return err
	}

	helper := "sbx-seed-" + randHex(6)

	if _, err := d.dockerCtx(ctx, "create", "--name", helper,
		"-v", volume+":/seed", "--entrypoint", "/seed/"+name, image); err != nil {
		return err
	}

	defer func() { _, _ = d.dockerCtx(context.WithoutCancel(ctx), "rm", "-f", helper) }()

	if _, err := d.dockerCtx(ctx, "cp", hostPath, helper+":/seed/"+name); err != nil {
		return err
	}

	return nil
}

// SeedFromImage relies on docker's copy-up: an empty named volume mounted over a directory an
// image already has is filled with that directory's contents when the container is created.
// So creating (not starting) one container is the whole copy, and it works from an image that
// has nothing in it but the binary.
func (d *dockerProvider) SeedFromImage(ctx context.Context, volume, image, dir string) error {
	if _, err := d.dockerCtx(ctx, "volume", "create", volume); err != nil {
		return err
	}

	if _, err := d.dockerCtx(ctx, "pull", image); err != nil {
		return err
	}

	helper := "sbx-seed-" + randHex(6)

	if _, err := d.dockerCtx(ctx, "create", "--name", helper, "-v", volume+":"+dir, image); err != nil {
		return err
	}

	_, _ = d.dockerCtx(context.WithoutCancel(ctx), "rm", "-f", helper)

	return nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)

	return hex.EncodeToString(b)
}

// Compile-time proof the docker provider offers both, since nothing else would notice if a
// signature drifted: the capability check is a type assertion that would quietly say "no".
var (
	_ Pauser   = (*dockerProvider)(nil)
	_ Injector = (*dockerProvider)(nil)
)
