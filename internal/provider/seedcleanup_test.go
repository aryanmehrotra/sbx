package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedDocker is a `docker` on PATH that records every call, reports volumes absent, and fails
// the one verb named by fail - the shape of a placement that dies half-way.
func seedDocker(t *testing.T, fail string) (log string) {
	t.Helper()

	dir := t.TempDir()
	log = filepath.Join(dir, "calls")

	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + log + "\n" +
		"case \"$1 $2\" in\n" +
		"  'volume inspect') exit 1 ;;\n" +
		"esac\n" +
		"[ \"$1\" = '" + fail + "' ] && { echo 'invalid reference format' >&2; exit 1; }\n" +
		"exit 0\n"

	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return log
}

func calls(t *testing.T, log string) string {
	t.Helper()

	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}

// A seed that fails must not leave the volume it created behind. A dev build asked docker for
// ghcr.io/...:v0.15.1-dev+ffd872d, the pull failed on the '+', and an empty sbx-execd-v0.15.1-dev-
// ffd872d stayed on the machine: nothing reclaims it, and the next attempt found a volume that
// looked placed.
func TestAFailedSeedLeavesNoVolumeItCreated(t *testing.T) {
	d := newDocker(dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"})

	for _, c := range []struct {
		name, fail string
		seed       func() error
	}{
		{"SeedFromImage, pull fails", "pull", func() error {
			return d.SeedFromImage(context.Background(), "sbx-execd-x", "img:bad+tag", "/usr/local/bin")
		}},
		{"SeedFromImage, create fails", "create", func() error {
			return d.SeedFromImage(context.Background(), "sbx-execd-x", "img:1", "/usr/local/bin")
		}},
		{"SeedFile, create fails", "create", func() error {
			return d.SeedFile(context.Background(), "sbx-execd-x", "sbx", "/bin/sh", "img:1")
		}},
		{"SeedFile, cp fails", "cp", func() error {
			return d.SeedFile(context.Background(), "sbx-execd-x", "sbx", "/bin/sh", "img:1")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			log := seedDocker(t, c.fail)

			if err := c.seed(); err == nil {
				t.Fatal("the seed succeeded though docker failed")
			}

			got := calls(t, log)
			if strings.Contains(got, "volume create sbx-execd-x") && !strings.Contains(got, "volume rm sbx-execd-x") {
				t.Fatalf("the volume this seed created was left behind:\n%s", got)
			}
		})
	}
}

// A volume that was there before is somebody's - another version's agent, or one a running
// sandbox mounts. A failed seed into it must not remove it.
func TestAFailedSeedKeepsAVolumeThatAlreadyExisted(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")

	script := "#!/bin/sh\necho \"$*\" >> " + log + "\n[ \"$1\" = pull ] && exit 1\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	d := newDocker(dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"})
	if err := d.SeedFromImage(context.Background(), "sbx-execd-x", "img:1", "/usr/local/bin"); err == nil {
		t.Fatal("the seed succeeded though the pull failed")
	}

	if got := calls(t, log); strings.Contains(got, "volume rm") {
		t.Fatalf("a volume that existed before the seed was removed:\n%s", got)
	}
}
