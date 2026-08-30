package cli

// An editor in a sandbox, through the wake path that already exists.
//
// Decided by mechanism rather than preference. VS Code has three remote modes and only one of
// them is an inbound TCP dial:
//
//   - Remote-SSH connects a socket. sbx already wakes on a socket, so this needs no new code in
//     the daemon and no editor-specific path to keep correct.
//   - Attach to Container goes through the docker socket and never touches the container's
//     network, so nothing sbx fronts is ever dialled and nothing wakes. An earlier draft of this
//     work proposed it; the mechanism ruled it out.
//   - Remote-Tunnels needs a `code tunnel` process already running inside, holding an outbound
//     connection. There is nothing to intercept.
//
// So this command finds the SSH port and prints how to reach it. JetBrains Gateway, `scp` and
// `rsync` come along for free, because to sbx they are all just a connection.

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// sshPorts are the container ports that mean "there is an sshd here".
//
// 22 is the protocol.s port. 2222 is on the list because a container that does not run as root
// cannot bind 22, so every rootless sshd image - linuxserver/openssh-server among them - picks
// 2222 by convention. Matching only 22 would miss the images people actually use.
var sshPorts = []int{22, 2222}

func isSSHPort(p int) bool {
	for _, s := range sshPorts {
		if p == s {
			return true
		}
	}

	return false
}

// SSHTarget is where an editor should connect, and what to type.
type SSHTarget struct {
	Sandbox string
	Service string
	Host    string
	Port    int
	User    string
}

// Addr is host:port.
func (t SSHTarget) Addr() string { return t.Host + ":" + strconv.Itoa(t.Port) }

// SSHCommand is the plain ssh invocation, which is also what an editor runs underneath.
func (t SSHTarget) SSHCommand() string {
	return fmt.Sprintf("ssh -p %d %s@%s", t.Port, t.User, t.Host)
}

// RemoteURI is what `code --remote` takes.
//
// The host part is the ssh destination as VS Code's own config would name it, so this matches
// what somebody would get by adding a Host block by hand. The folder is separate because
// `--remote` takes it as an ordinary argument.
func (t SSHTarget) RemoteURI() string {
	return fmt.Sprintf("ssh-remote+%s@%s:%d", t.User, t.Host, t.Port)
}

// CodeCommand is the whole line to open an editor on it.
func (t SSHTarget) CodeCommand(folder string) string {
	return fmt.Sprintf("code --remote %s %s", t.RemoteURI(), folder)
}

// FindSSH locates the service in a sandbox that publishes SSH.
//
// By port rather than by name, because the name is the author.s and the port is the protocol: a
// service called "dev" or "box" or "workspace" that publishes 22 is the thing somebody means.
//
// The container port lives in the spec rather than on the unit - a Unit carries the addresses a
// client uses, not the ports inside - so the spec is read the same way every other command reads
// it, and the Nth declared port is the Nth client address.
func FindSSH(ctx context.Context, p provider.Provider, sp *spec.Spec, sandbox, service, user string) (SSHTarget, error) {
	units, err := p.List(ctx, sandbox)
	if err != nil {
		return SSHTarget{}, err
	}

	if len(units) == 0 {
		return SSHTarget{}, UnknownSandbox(ctx, p, sandbox)
	}

	var candidates []string

	for _, u := range units {
		if service != "" && u.Service != service {
			continue
		}

		candidates = append(candidates, u.Service)

		svc, ok := sp.Services[u.Service]
		if !ok {
			continue // in the sandbox but not in this spec - `sbx add` puts one there
		}

		for i, cp := range svc.Ports {
			if !isSSHPort(cp) || i >= len(u.Client) {
				continue
			}

			return SSHTarget{
				Sandbox: sandbox,
				Service: u.Service,
				Host:    u.Client[i].Host,
				Port:    u.Client[i].Port,
				User:    user,
			}, nil
		}
	}

	// Naming a service that has no SSH and having a sandbox with none at all are different
	// mistakes, and only one of them is about the service.
	if service != "" {
		return SSHTarget{}, fmt.Errorf(
			"service %q in sandbox %q publishes no SSH port (%v), so there is nothing to connect to",
			service, sandbox, sshPorts)
	}

	return SSHTarget{}, fmt.Errorf(
		"no service in sandbox %q publishes an SSH port %v (it has: %s).\n"+
			"     Add one to the spec - any image running sshd will do:\n"+
			"       \"dev\": { \"image\": \"...\", \"ports\": [22], \"mounts\": {\".\": \"/work\"} }",
		sandbox, sshPorts, strings.Join(candidates, ", "))
}

// SSH prints where to connect and what to type.
//
// It prints rather than launches. An editor is a personal choice - VS Code, Cursor, JetBrains
// Gateway, plain ssh - and a command that shells out to one of them is a command that is wrong
// for everybody else. The address is the useful part; the lines below it are a convenience.
func SSH(ctx context.Context, p provider.Provider, sp *spec.Spec, sandbox, service, user, folder string,
	w io.Writer,
) error {
	t, err := FindSSH(ctx, p, sp, sandbox, service, user)
	if err != nil {
		return err
	}

	fmt.Fprintf(w, "sbx ssh · %s/%s · %s\n\n", t.Sandbox, t.Service, t.Addr())
	fmt.Fprintf(w, "  %s\n", t.SSHCommand())
	fmt.Fprintf(w, "  %s\n\n", t.CodeCommand(folder))

	// The wake is the point, and it is worth saying that this address behaves like every other
	// one sbx hands out rather than being a special case.
	fmt.Fprintln(w, "  Connecting wakes the sandbox, like any other port. An attached editor")
	fmt.Fprintln(w, "  keeps it awake - nothing sleeps underneath a live editor, here or anywhere.")

	return nil
}
