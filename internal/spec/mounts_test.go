package spec

import (
	"strings"
	"testing"
)

// A container path has to be absolute. Docker reads a relative one as a *name* it invents,
// so the mount silently lands somewhere nobody asked for rather than failing.
func TestAMountNeedsAnAbsoluteContainerPath(t *testing.T) {
	svc := Service{Image: "postgres:16", Ports: []int{5432}, Mounts: map[string]string{"./data": "data"}}

	err := svc.validate("db")
	if err == nil {
		t.Fatal("a relative container path was accepted")
	}

	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error was %q, want it to say the path must be absolute", err)
	}
}

func TestAMountNeedsAHostPath(t *testing.T) {
	svc := Service{Image: "postgres:16", Ports: []int{5432}, Mounts: map[string]string{"  ": "/data"}}

	if err := svc.validate("db"); err == nil {
		t.Fatal("a mount with no host path was accepted")
	}
}

func TestAnOrdinaryMountIsAccepted(t *testing.T) {
	svc := Service{Image: "postgres:16", Ports: []int{5432}, Mounts: map[string]string{
		"./data": "/var/lib/postgresql/data",
		"/tmp/x": "/x",
	}}

	if err := svc.validate("db"); err != nil {
		t.Errorf("an ordinary mount was refused: %v", err)
	}
}
