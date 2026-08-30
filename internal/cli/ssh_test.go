package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// listStub is a Provider that returns a fixed fleet.
type listStub struct {
	provider.Provider

	units []provider.Unit
}

func (l *listStub) Name() string { return "stub" }

func (l *listStub) List(context.Context, string) ([]provider.Unit, error) { return l.units, nil }

func specWith(services map[string][]int) *spec.Spec {
	sp := &spec.Spec{Version: 1, Services: map[string]spec.Service{}}
	for name, ports := range services {
		sp.Services[name] = spec.Service{Image: "x", Ports: ports}
	}

	return sp
}

// The SSH service is found by PORT, not by name: the name belongs to whoever wrote the spec and
// the port belongs to the protocol. A service called dev, box or workspace is what people write.
func TestSSHIsFoundByPortWhateverTheServiceIsCalled(t *testing.T) {
	for _, name := range []string{"dev", "box", "workspace", "sshd"} {
		p := &listStub{units: []provider.Unit{{
			Sandbox: "s", Service: name,
			Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20060}},
		}}}

		got, err := FindSSH(context.Background(), p, specWith(map[string][]int{name: {22}}),
			"s", "", "root")
		if err != nil {
			t.Errorf("service %q: %v", name, err)

			continue
		}

		if got.Service != name || got.Port != 20060 {
			t.Errorf("service %q resolved to %s:%d", name, got.Service, got.Port)
		}
	}
}

// A rootless sshd cannot bind 22, so every image that does not run as root picks 2222. Matching
// only 22 would miss the images people actually use.
func TestTheRootlessSSHPortCounts(t *testing.T) {
	p := &listStub{units: []provider.Unit{{
		Sandbox: "s", Service: "dev",
		Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20060}},
	}}}

	if _, err := FindSSH(context.Background(), p, specWith(map[string][]int{"dev": {2222}}),
		"s", "", "root"); err != nil {
		t.Errorf("2222 was not recognised as an SSH port: %v", err)
	}
}

// The Nth declared port is the Nth client address, so a service with several ports has to map
// the right one - picking the first would hand out the database's address for an editor.
func TestTheRightPortIsPickedFromAServiceWithSeveral(t *testing.T) {
	p := &listStub{units: []provider.Unit{{
		Sandbox: "s", Service: "dev",
		Client: []provider.Endpoint{
			{Host: "127.0.0.1", Port: 20060}, // 8080
			{Host: "127.0.0.1", Port: 20061}, // 22
			{Host: "127.0.0.1", Port: 20062}, // 5432
		},
	}}}

	got, err := FindSSH(context.Background(), p, specWith(map[string][]int{"dev": {8080, 22, 5432}}),
		"s", "", "root")
	if err != nil {
		t.Fatal(err)
	}

	if got.Port != 20061 {
		t.Errorf("picked %d, want 20061 - the address of the port that is SSH", got.Port)
	}
}

// A sandbox with no SSH says so, and says what it does have plus how to add one. A bare "not
// found" leaves the reader to guess whether the sandbox, the service or the spec is wrong.
func TestASandboxWithNoSSHSaysWhatItHasInstead(t *testing.T) {
	p := &listStub{units: []provider.Unit{
		{Sandbox: "s", Service: "postgres", Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20060}}},
		{Sandbox: "s", Service: "redis", Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20061}}},
	}}

	_, err := FindSSH(context.Background(), p,
		specWith(map[string][]int{"postgres": {5432}, "redis": {6379}}), "s", "", "root")
	if err == nil {
		t.Fatal("a sandbox with no SSH port resolved to something")
	}

	for _, want := range []string{"postgres", "redis", "22"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// Naming a service that has no SSH is a different mistake from having none at all, and only one
// of them is about the service.
func TestNamingTheWrongServiceSaysSo(t *testing.T) {
	p := &listStub{units: []provider.Unit{
		{Sandbox: "s", Service: "dev", Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20060}}},
		{Sandbox: "s", Service: "db", Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20061}}},
	}}

	_, err := FindSSH(context.Background(), p,
		specWith(map[string][]int{"dev": {22}, "db": {5432}}), "s", "db", "root")
	if err == nil {
		t.Fatal("naming a service with no SSH resolved anyway")
	}

	if !strings.Contains(err.Error(), `"db"`) {
		t.Errorf("the error does not name the service asked for: %v", err)
	}
}

// The URI is what an editor consumes, so its shape is a contract rather than a formatting choice.
func TestTheEditorURIIsTheFormVSCodeTakes(t *testing.T) {
	target := SSHTarget{Sandbox: "s", Service: "dev", Host: "127.0.0.1", Port: 20060, User: "dev"}

	if got, want := target.RemoteURI(), "ssh-remote+dev@127.0.0.1:20060"; got != want {
		t.Errorf("RemoteURI() = %q, want %q", got, want)
	}

	if got, want := target.SSHCommand(), "ssh -p 20060 dev@127.0.0.1"; got != want {
		t.Errorf("SSHCommand() = %q, want %q", got, want)
	}

	cmd := target.CodeCommand("/work")
	if !strings.HasPrefix(cmd, "code --remote ssh-remote+dev@127.0.0.1:20060 ") ||
		!strings.HasSuffix(cmd, " /work") {
		t.Errorf("CodeCommand() = %q", cmd)
	}
}

// The image decides the user - a rootless sshd has its own and root cannot log in at all - so
// what is asked for is what comes back.
func TestTheUserIsWhateverTheImageWants(t *testing.T) {
	p := &listStub{units: []provider.Unit{{
		Sandbox: "s", Service: "dev",
		Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20060}},
	}}}

	got, err := FindSSH(context.Background(), p, specWith(map[string][]int{"dev": {2222}}),
		"s", "", "coder")
	if err != nil {
		t.Fatal(err)
	}

	if got.User != "coder" || !strings.Contains(got.SSHCommand(), "coder@") {
		t.Errorf("user not carried through: %+v", got)
	}
}
