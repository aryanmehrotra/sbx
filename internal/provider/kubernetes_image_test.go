package provider

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/spec"
)

// fakeKubectl puts a `kubectl` on PATH that reports an existing deployment running image, and
// logs every call. Through the real exec path, so what is asserted is the argv Create sends.
func fakeKubectl(t *testing.T, image string) (log string) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the fake kubectl is a shell script")
	}

	dir := t.TempDir()
	log = filepath.Join(dir, "calls")

	script := "#!/bin/sh\necho \"$*\" >> " + log + "\n" +
		"case \"$*\" in\n" +
		"  *jsonpath=*containers*image*) printf '%s' '" + image + "' ;;\n" +
		"esac\nexit 0\n"

	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return log
}

func calls(t *testing.T, log string) string {
	t.Helper()

	b, _ := os.ReadFile(log)

	return string(b)
}

// The docker S1 bug on the cluster side: an existing deployment stopped at "already exists"
// and kept the old image while create reported success. The image is patched in place, which
// keeps the deployment's PersistentVolumeClaim.
func TestKubernetesCreatePatchesAStaleImage(t *testing.T) {
	log := fakeKubectl(t, "sbx-build-old:latest")
	k := newKube("sbx")

	var err error

	out := stdoutOf(t, func() {
		err = k.Create(context.Background(), "b", 0, 0, "app", spec.Service{Image: "sbx-build-new:latest", Ports: []int{80}},
			nil, "", IsolationContainer)
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got := calls(t, log)
	if !strings.Contains(got, "set image deployment/sbx-b-app app=sbx-build-new:latest") {
		t.Fatalf("the deployment's image was not patched; kubectl saw:\n%s\nstdout: %s", got, out)
	}

	if strings.Contains(got, "delete") || strings.Contains(got, "apply") {
		t.Errorf("the deployment was replaced rather than patched, which risks its claim:\n%s", got)
	}

	if !strings.Contains(out, "recreated (image changed)") {
		t.Errorf("the change was silent: %q", out)
	}
}

func TestKubernetesCreateLeavesTheSameImageAlone(t *testing.T) {
	log := fakeKubectl(t, "redis:7-alpine")
	k := newKube("sbx")

	out := stdoutOf(t, func() {
		if err := k.Create(context.Background(), "b", 0, 0, "cache", spec.Service{Image: "redis:7-alpine", Ports: []int{6379}},
			nil, "", IsolationContainer); err != nil {
			t.Fatalf("Create: %v", err)
		}
	})

	if got := calls(t, log); strings.Contains(got, "set image") {
		t.Fatalf("an unchanged image was patched:\n%s", got)
	}

	if !strings.Contains(out, "already exists") {
		t.Errorf("an unchanged deployment did not say it already exists: %q", out)
	}
}
