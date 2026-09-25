package fc

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dockerHostForTests is the engine the rootfs pipeline's docker-backed tests may use: set
// SBX_FC_TEST_DOCKER_HOST, or it looks for the colima `osb` profile. Never the default engine,
// and skipped under -short.
func dockerHostForTests(t *testing.T) string {
	t.Helper()

	if testing.Short() {
		t.Skip("-short: needs a docker engine")
	}

	if h := os.Getenv("SBX_FC_TEST_DOCKER_HOST"); h != "" {
		return h
	}

	home, _ := os.UserHomeDir()

	sock := filepath.Join(home, ".colima", "osb", "docker.sock")
	if _, err := os.Stat(sock); err != nil {
		t.Skip("no SBX_FC_TEST_DOCKER_HOST and no colima osb profile")
	}

	return "unix://" + sock
}

func TestDockerCLIExportsAFlattenedFilesystem(t *testing.T) {
	d := DockerCLI{Env: []string{"DOCKER_HOST=" + dockerHostForTests(t)}}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const image = "alpine:3"

	cfg, err := d.Inspect(ctx, image)
	if errors.Is(err, ErrNoImage) {
		if err := d.Pull(ctx, image); err != nil {
			t.Skip("alpine:3 not present and not pullable:", err)
		}

		cfg, err = d.Inspect(ctx, image)
	}

	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(cfg.ID, "sha256:") || cfg.OS != "linux" || len(cfg.Cmd) == 0 {
		t.Fatalf("config = %+v", cfg)
	}

	var buf bytes.Buffer
	if err := d.Export(ctx, image, &buf); err != nil {
		t.Fatal(err)
	}

	found := false
	whiteouts := 0

	tr := tar.NewReader(&buf)

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatal(err)
		}

		if h.Name == "etc/alpine-release" {
			found = true
		}

		if strings.HasPrefix(filepath.Base(h.Name), ".wh.") {
			whiteouts++
		}
	}

	if !found {
		t.Fatal("etc/alpine-release not in the export")
	}

	// The claim the pipeline rests on: export is already flattened, so there is nothing to
	// honour. If an engine ever hands back whiteouts, this is the test that says so.
	if whiteouts != 0 {
		t.Fatalf("export carried %d whiteout entries", whiteouts)
	}

	if _, err := d.Inspect(ctx, "sbx-fc-test-absent-image:never"); !errors.Is(err, ErrNoImage) {
		t.Fatalf("absent image = %v, want ErrNoImage", err)
	}

	// The throwaway container is gone.
	out, _ := d.run(ctx, "ps", "-a", "--filter", "label=sbx.fc.export=1", "--format", "{{.ID}}")
	if out != "" {
		t.Fatalf("export left containers behind: %s", out)
	}
}
