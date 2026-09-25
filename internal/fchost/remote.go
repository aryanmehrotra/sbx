package fchost

// provider.For("firecracker") on a host that cannot run Firecracker itself.
//
// Most of the CLI never gets here: Redirect runs the whole command line inside the helper VM.
// What is left is code that holds a provider.Provider in THIS process - anything that asks
// provider.For directly rather than going through a redirected command. hostcap answers
// "helper-vm" for this machine, the provider package calls HelperVMProvider, and this file is
// that hook: a Provider whose every method is the same method on the in-VM sbx's firecracker
// provider, run through one `sbx fc call <method>` over the helper VM's shell.
//
// Nothing is reimplemented: the call on the far side is the real provider, so a VM created this
// way is one the in-VM daemon lists, wakes and sleeps like any other. The request travels as
// one base64 argument (so a terminal can still own stdin), the answer as one JSON line on stdout,
// and the two methods whose point is a stream - Logs and ExecTTY - stream on stdout instead.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aryanmehrotra/sbx/internal/fc/hostcap"
	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

func init() {
	provider.HelperVMProvider = helperVMProvider
	provider.DecideHost = func() hostcap.Decision { return HostBackend().Decision() }
}

// helperVMProvider is provider.HelperVMProvider: the helper VM for this machine, as a Provider.
func helperVMProvider(hostcap.Decision) (provider.Provider, error) {
	b := HostBackend()
	if b.Kind != HelperVM {
		// hostcap said helper-vm and the full detection (which also needs a VM tool) did not.
		return nil, Refusal(b)
	}

	m, err := NewManager(b.Helper, os.Stderr)
	if err != nil {
		return nil, err
	}

	return &Remote{M: m, Version: provider.Version}, nil
}

// Remote is a provider.Provider whose methods run in the helper VM.
type Remote struct {
	M       *Manager
	Version string

	once    sync.Once
	ensured error
}

var _ provider.Provider = (*Remote)(nil)

// request is every method's arguments, by name; each method reads the ones it takes.
type request struct {
	Sandbox    string              `json:"sandbox,omitempty"`
	Service    string              `json:"service,omitempty"`
	Ref        string              `json:"ref,omitempty"`
	Slot       int                 `json:"slot,omitempty"`
	Ordinal    int                 `json:"ordinal,omitempty"`
	StartIndex int                 `json:"startIndex,omitempty"`
	Spec       *spec.Service       `json:"spec,omitempty"`
	Endpoints  []provider.Endpoint `json:"endpoints,omitempty"`
	Ports      []int               `json:"ports,omitempty"`
	Isolation  provider.Isolation  `json:"isolation,omitempty"`
	Argv       []string            `json:"argv,omitempty"`
	Lines      int                 `json:"lines,omitempty"`
	Follow     bool                `json:"follow,omitempty"`
	Path       string              `json:"path,omitempty"` // the in-VM side of a copy
	Name       string              `json:"name,omitempty"` // the file's base name, copying in
	Mode       os.FileMode         `json:"mode,omitempty"`
}

// response is one JSON line. Error is the far side's error text; ExitCode is set when it was a
// command in the sandbox that exited non-zero, so that survives the hop as *provider.ExitError.
type response struct {
	Result   json.RawMessage `json:"result,omitempty"`
	Error    string          `json:"error,omitempty"`
	ExitCode int             `json:"exitCode,omitempty"`
}

func (r *Remote) ensure(ctx context.Context) error {
	r.once.Do(func() {
		st, err := r.M.Status(ctx)
		if err != nil {
			r.ensured = err
			return
		}

		if st != Running {
			r.ensured = r.M.Ensure(ctx, EnsureOptions{Version: r.Version})
		}
	})

	return r.ensured
}

// argv is the shell command that runs `sbx fc call method <req>` in the VM.
func (r *Remote) argv(ctx context.Context, method string, req request, tty bool) ([]string, error) {
	if err := r.ensure(ctx); err != nil {
		return nil, err
	}

	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	return r.M.Shell(ctx, "/", tty, GuestArgv("fc", []string{"call", method, base64.RawURLEncoding.EncodeToString(b)}))
}

// call runs a JSON method and decodes its result into out (which may be nil).
func (r *Remote) call(ctx context.Context, method string, req request, stdin io.Reader, out any) error {
	sh, err := r.argv(ctx, method, req, false)
	if err != nil {
		return err
	}

	var stdout, stderr bytes.Buffer

	if err := r.M.Run.Run(ctx, Cmd{Argv: sh, Stdin: stdin, Stdout: &stdout, Stderr: &stderr}); err != nil {
		return fmt.Errorf("sbx fc call %s in the helper VM: %w: %s", method, err, strings.TrimSpace(stderr.String()))
	}

	var resp response
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return fmt.Errorf("sbx fc call %s: the helper VM answered %q, not JSON: %w",
			method, strings.TrimSpace(stdout.String()), err)
	}

	switch {
	case resp.ExitCode != 0:
		return &provider.ExitError{Code: resp.ExitCode, Output: resp.Error}
	case resp.Error != "":
		return errors.New(resp.Error)
	case out != nil && len(resp.Result) > 0:
		return json.Unmarshal(resp.Result, out)
	}

	return nil
}

// stream runs a streaming method with the given streams; its exit status is the method's.
func (r *Remote) stream(ctx context.Context, method string, req request, tty bool, c Cmd) error {
	sh, err := r.argv(ctx, method, req, tty)
	if err != nil {
		return err
	}

	c.Argv = sh

	err = r.M.Run.Run(ctx, c)

	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return &provider.ExitError{Code: ee.ExitCode()}
	}

	return err
}

func (r *Remote) Name() string { return Firecracker }

func (r *Remote) Create(ctx context.Context, sandbox string, slot, ordinal int, service string,
	svc spec.Service, eps []provider.Endpoint, _ string, iso provider.Isolation,
) error {
	// specDir is a path on THIS machine; the firecracker provider builds from an image and
	// never reads it, so it is not sent.
	return r.call(ctx, "create", request{Sandbox: sandbox, Slot: slot, Ordinal: ordinal, Service: service,
		Spec: &svc, Endpoints: eps, Isolation: iso}, nil, nil)
}

func (r *Remote) Start(ctx context.Context, ref string) error {
	return r.call(ctx, "start", request{Ref: ref}, nil, nil)
}

func (r *Remote) Stop(ctx context.Context, ref string) error {
	return r.call(ctx, "stop", request{Ref: ref}, nil, nil)
}

type health struct{ Serving, Declared bool }

func (r *Remote) Healthy(ctx context.Context, ref string) (bool, bool) {
	var h health
	if r.call(ctx, "healthy", request{Ref: ref}, nil, &h) != nil {
		return false, false
	}

	return h.Serving, h.Declared
}

func (r *Remote) Probe(ctx context.Context, ref string) (bool, bool) {
	var h health
	if r.call(ctx, "probe", request{Ref: ref}, nil, &h) != nil {
		return false, false
	}

	return h.Serving, h.Declared
}

func (r *Remote) List(ctx context.Context, sandbox string) ([]provider.Unit, error) {
	var units []provider.Unit

	return units, r.call(ctx, "list", request{Sandbox: sandbox}, nil, &units)
}

func (r *Remote) Remove(ctx context.Context, sandbox string) error {
	return r.call(ctx, "remove", request{Sandbox: sandbox}, nil, nil)
}

func (r *Remote) Exec(ctx context.Context, ref string, argv []string) (string, error) {
	var out string

	return out, r.call(ctx, "exec", request{Ref: ref, Argv: argv}, nil, &out)
}

func (r *Remote) ExecTTY(ctx context.Context, ref string, argv []string) error {
	return r.stream(ctx, "exectty", request{Ref: ref, Argv: argv}, isTerminal(os.Stdin) && isTerminal(os.Stdout),
		Cmd{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
}

func (r *Remote) Logs(ctx context.Context, ref string, lines int, follow bool, w io.Writer) error {
	return r.stream(ctx, "logs", request{Ref: ref, Lines: lines, Follow: follow}, false,
		Cmd{Stdout: w, Stderr: os.Stderr})
}

// Copy moves one file across two boundaries: this machine to the helper VM travels on the call's
// stdin or stdout, and the helper VM to the microVM is the in-VM provider's own Copy.
func (r *Remote) Copy(ctx context.Context, ref, src, dst string) error {
	switch {
	case strings.HasPrefix(dst, ":") && !strings.HasPrefix(src, ":"):
		fi, err := os.Stat(src)
		if err != nil {
			return err
		}

		if fi.IsDir() {
			return fmt.Errorf("%s is a directory; the firecracker provider copies one file at a time", src)
		}

		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()

		return r.call(ctx, "copyin", request{Ref: ref, Path: dst, Name: filepath.Base(src), Mode: fi.Mode().Perm()}, f, nil)
	case strings.HasPrefix(src, ":") && !strings.HasPrefix(dst, ":"):
		if fi, err := os.Stat(dst); err == nil && fi.IsDir() {
			dst = filepath.Join(dst, filepath.Base(src[1:]))
		}

		var buf, stderr bytes.Buffer

		if err := r.stream(ctx, "copyout", request{Ref: ref, Path: src}, false, Cmd{Stdout: &buf, Stderr: &stderr}); err != nil {
			return fmt.Errorf("copying %s out of %s: %w: %s", src, ref, err, strings.TrimSpace(stderr.String()))
		}

		return os.WriteFile(dst, buf.Bytes(), 0o644)
	default:
		return errors.New(`exactly one of src and dst must be inside the sandbox, written as ":path"`)
	}
}

func (r *Remote) Endpoints(sandbox, service string, slot, startIndex int, containerPorts []int) []provider.Endpoint {
	var eps []provider.Endpoint

	_ = r.call(context.Background(), "endpoints", request{Sandbox: sandbox, Service: service, Slot: slot,
		StartIndex: startIndex, Ports: containerPorts}, nil, &eps)

	return eps
}

func (r *Remote) AllocSlot(ctx context.Context, sandbox string) (int, error) {
	var slot int

	return slot, r.call(ctx, "allocslot", request{Sandbox: sandbox}, nil, &slot)
}

// Call is `sbx fc call <method> <request>`, the far side of Remote, run by the in-VM sbx
// against its own provider. It is not meant to be typed: `sbx fc` does not list it.
func Call(ctx context.Context, p provider.Provider, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) != 2 {
		return errors.New("usage: sbx fc call <method> <base64 request>")
	}

	raw, err := base64.RawURLEncoding.DecodeString(args[1])
	if err != nil {
		return fmt.Errorf("sbx fc call: the request is not base64: %w", err)
	}

	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return fmt.Errorf("sbx fc call: the request is not JSON: %w", err)
	}

	reply := func(result any, err error) error {
		var resp response

		var ee *provider.ExitError

		switch {
		case errors.As(err, &ee):
			resp.ExitCode, resp.Error = ee.Code, ee.Output
		case err != nil:
			resp.Error = err.Error()
		case result != nil:
			resp.Result, err = json.Marshal(result)
			if err != nil {
				return err
			}
		}

		return json.NewEncoder(stdout).Encode(resp)
	}

	switch args[0] {
	case "create":
		if req.Spec == nil {
			return reply(nil, errors.New("create needs a service spec"))
		}

		return reply(nil, p.Create(ctx, req.Sandbox, req.Slot, req.Ordinal, req.Service, *req.Spec, req.Endpoints, "", req.Isolation))
	case "start":
		return reply(nil, p.Start(ctx, req.Ref))
	case "stop":
		return reply(nil, p.Stop(ctx, req.Ref))
	case "healthy":
		s, d := p.Healthy(ctx, req.Ref)
		return reply(health{s, d}, nil)
	case "probe":
		s, d := p.Probe(ctx, req.Ref)
		return reply(health{s, d}, nil)
	case "list":
		units, err := p.List(ctx, req.Sandbox)
		return reply(units, err)
	case "remove":
		return reply(nil, p.Remove(ctx, req.Sandbox))
	case "exec":
		out, err := p.Exec(ctx, req.Ref, req.Argv)
		return reply(out, err)
	case "endpoints":
		return reply(p.Endpoints(req.Sandbox, req.Service, req.Slot, req.StartIndex, req.Ports), nil)
	case "allocslot":
		slot, err := p.AllocSlot(ctx, req.Sandbox)
		return reply(slot, err)
	case "copyin":
		return reply(nil, copyIn(ctx, p, req, stdin))
	case "copyout":
		return copyOut(ctx, p, req, stdout)
	case "logs":
		return p.Logs(ctx, req.Ref, req.Lines, req.Follow, stdout)
	case "exectty":
		return p.ExecTTY(ctx, req.Ref, req.Argv)
	default:
		return fmt.Errorf("sbx fc call: unknown method %q", args[0])
	}
}

func copyIn(ctx context.Context, p provider.Provider, req request, stdin io.Reader) error {
	dir, err := os.MkdirTemp("", "sbx-fc-copy")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	name := filepath.Base(req.Name)
	if name == "." || name == "/" || name == "" {
		name = "file"
	}

	tmp := filepath.Join(dir, name)

	mode := req.Mode.Perm()
	if mode == 0 {
		mode = 0o644
	}

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode)
	if err != nil {
		return err
	}

	if _, err := io.Copy(f, stdin); err != nil {
		f.Close()
		return err
	}

	if err := f.Close(); err != nil {
		return err
	}

	return p.Copy(ctx, req.Ref, tmp, req.Path)
}

func copyOut(ctx context.Context, p provider.Provider, req request, stdout io.Writer) error {
	dir, err := os.MkdirTemp("", "sbx-fc-copy")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	tmp := filepath.Join(dir, "file")

	if err := p.Copy(ctx, req.Ref, req.Path, tmp); err != nil {
		return err
	}

	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(stdout, f)

	return err
}
