package provider

// Exec, ExecTTY, Copy and the daemon's vsock leg: everything the firecracker provider does
// INSIDE a VM goes to execd over the guest channel (fc.Guest - on linux, Firecracker's hybrid
// vsock device). There is no `docker exec` underneath a VM to fall back on, so without a real
// channel each of these is refused, never approximated over another path.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/fc"
	"github.com/aryanmehrotra/sbx/internal/osbclient"
	"github.com/aryanmehrotra/sbx/internal/tui"
	"github.com/aryanmehrotra/sbx/internal/wsclient"
)

// ExitError is a command inside a sandbox that ran and exited non-zero - as opposed to one that
// could not be run at all. ExitCode matches *exec.ExitError's, so a caller need not care which
// provider ran it.
type ExitError struct {
	Code   int
	Output string
}

func (e *ExitError) Error() string {
	if e.Output == "" {
		return fmt.Sprintf("exit status %d", e.Code)
	}

	return fmt.Sprintf("exit status %d: %s", e.Code, e.Output)
}

// ExitCode is the command's exit status.
func (e *ExitError) ExitCode() int { return e.Code }

// execdBase is the URL every call is made against. The host is never resolved: the transport
// dials the guest channel whatever the URL says.
const execdBase = "http://execd"

// execdFor returns an HTTP client whose every connection is a new stream to the VM's execd, and
// the VM record that says which token it expects. No keep-alive: a pooled connection is one a
// snapshot would capture half-open, and the device's handshake is a few milliseconds.
func (p *fcProvider) execdFor(ref, op string) (*http.Client, *fcVM, error) {
	if !p.guest.Available() {
		return nil, nil, errNoGuest(op)
	}

	vm, err := p.load(ref)
	if err != nil {
		return nil, nil, err
	}

	gvm := p.guestVM(vm)

	hc := &http.Client{Transport: &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return p.guest.Dial(ctx, gvm, fc.ExecdVsockPort)
		},
		DisableKeepAlives: true,
	}}

	return hc, vm, nil
}

func (p *fcProvider) execd(ref, op string) (*osbclient.Execd, error) {
	hc, vm, err := p.execdFor(ref, op)
	if err != nil {
		return nil, err
	}

	return osbclient.NewExecd(execdBase, vm.AccessToken, hc), nil
}

// Exec runs argv (not a shell line) in the VM and returns its output, stdout and stderr in the
// order they arrived, trimmed as docker exec's is. A non-zero exit is an *ExitError carrying it.
func (p *fcProvider) Exec(ctx context.Context, ref string, argv []string) (string, error) {
	if len(argv) == 0 {
		return "", errors.New("nothing to run: argv is empty")
	}

	e, err := p.execd(ref, "run a command inside a VM")
	if err != nil {
		return "", err
	}

	var (
		out  strings.Builder
		x    = osbclient.NewExecution()
		code *int
	)

	err = e.StreamCommand(ctx, osbclient.CommandRequest{Argv: argv}, func(ev osbclient.Event) error {
		x.Apply(ev)

		if ev.Type == "stdout" || ev.Type == "stderr" {
			out.WriteString(ev.Text)
		}

		return nil
	})
	if err != nil {
		return "", fmt.Errorf("%s: exec %s: %w", ref, argv[0], err)
	}

	x.InferExitCode()
	code = x.ExitCode

	text := strings.TrimSpace(out.String())

	switch {
	case code == nil:
		return text, fmt.Errorf("%s: exec %s: the command's stream ended without an exit status", ref, argv[0])
	case *code != 0:
		return "", &ExitError{Code: *code, Output: text}
	}

	return text, nil
}

// Copy moves one file in or out, as docker cp does for a file: into an existing directory it
// keeps its name. A directory is refused rather than half-copied - execd moves files, and a
// tree would be a walk of them, which `sbx exec ... tar` already does honestly.
func (p *fcProvider) Copy(ctx context.Context, ref, src, dst string) error {
	e, err := p.execd(ref, "copy files in or out of a VM")
	if err != nil {
		return err
	}

	switch in, out := strings.HasPrefix(dst, ":"), strings.HasPrefix(src, ":"); {
	case in && !out:
		return p.copyIn(ctx, e, src, dst[1:])
	case out && !in:
		return p.copyOut(ctx, e, src[1:], dst)
	default:
		return errors.New(`exactly one of src and dst must be inside the sandbox, written as ":path"`)
	}
}

func (p *fcProvider) copyIn(ctx context.Context, e *osbclient.Execd, src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}

	if fi.IsDir() {
		return fmt.Errorf("%s is a directory; the firecracker provider copies one file at a time", src)
	}

	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}

	if strings.HasSuffix(dst, "/") {
		dst += filepath.Base(src)
	} else if st, err := e.Stat(ctx, dst); err == nil && st[dst].Type == "directory" {
		dst = path.Join(dst, filepath.Base(src))
	}

	// The contract writes mode as octal digits read in decimal: 0640 travels as 640.
	mode, _ := strconv.Atoi(strconv.FormatUint(uint64(fi.Mode().Perm()), 8))

	if err := e.MakeDirs(ctx, map[string]osbclient.Permission{path.Dir(dst): {Mode: 755}}); err != nil {
		return fmt.Errorf("creating %s in the VM: %w", path.Dir(dst), err)
	}

	return e.WriteFile(ctx, dst, data, osbclient.Permission{Mode: mode})
}

func (p *fcProvider) copyOut(ctx context.Context, e *osbclient.Execd, src, dst string) error {
	st, err := e.Stat(ctx, src)
	if err != nil {
		return fmt.Errorf("%s in the VM: %w", src, err)
	}

	info, ok := st[src]
	if !ok {
		return fmt.Errorf("%s: no such file in the VM", src)
	}

	if info.Type == "directory" {
		return fmt.Errorf("%s is a directory in the VM; the firecracker provider copies one file at a time", src)
	}

	data, err := e.ReadFile(ctx, src, "")
	if err != nil {
		return err
	}

	if fi, err := os.Stat(dst); err == nil && fi.IsDir() {
		dst = filepath.Join(dst, path.Base(src))
	}

	perm := os.FileMode(0o644)
	if m, err := strconv.ParseUint(strconv.Itoa(info.Mode), 8, 32); err == nil && m != 0 {
		perm = os.FileMode(m).Perm()
	}

	return os.WriteFile(dst, data, perm)
}

// ExecTTY attaches this terminal to a command in the VM through execd's PTY sessions - the same
// protocol an OpenSandbox terminal client speaks.
func (p *fcProvider) ExecTTY(ctx context.Context, ref string, argv []string) error {
	return p.execTTY(ctx, ref, argv, os.Stdin, os.Stdout, os.Stderr)
}

// PTY frame tags, execd's (internal/execd/pty.go).
const (
	ptyStdin  byte = 0x00
	ptyStdout byte = 0x01
	ptyStderr byte = 0x02
	ptyReplay byte = 0x03
)

func (p *fcProvider) execTTY(ctx context.Context, ref string, argv []string, in io.Reader, out, errw io.Writer) error {
	if len(argv) == 0 {
		return errors.New("nothing to run: argv is empty")
	}

	hc, vm, err := p.execdFor(ref, "open a terminal inside a VM")
	if err != nil {
		return err
	}

	inFile, _ := in.(*os.File)
	outFile, _ := out.(*os.File)
	tty := inFile != nil && outFile != nil && tui.IsTerminal(inFile) && tui.IsTerminal(outFile)

	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'"'"'`) + "'"
	}

	body := map[string]any{"command": "exec " + strings.Join(quoted, " ")}
	if tty {
		body["rows"], body["cols"] = tui.Size(outFile)
	}

	id, err := createPTY(ctx, hc, vm.AccessToken, body)
	if err != nil {
		return fmt.Errorf("%s: %w", ref, err)
	}

	defer func() {
		req, _ := http.NewRequestWithContext(context.WithoutCancel(ctx), http.MethodDelete,
			execdBase+"/pty/"+url.PathEscape(id), nil)
		req.Header.Set(osbclient.ExecdTokenHeader, vm.AccessToken)

		if resp, err := hc.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()

	mode := "0"
	if tty {
		mode = "1"
	}

	gvm := p.guestVM(vm)

	ws, err := wsclient.Dial(ctx, "ws://execd/pty/"+url.PathEscape(id)+"/ws?pty="+mode, wsclient.Options{
		Header: http.Header{osbclient.ExecdTokenHeader: {vm.AccessToken}},
		NetDial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return p.guest.Dial(ctx, gvm, fc.ExecdVsockPort)
		},
	})
	if err != nil {
		return fmt.Errorf("%s: attaching to the terminal: %w", ref, err)
	}
	defer ws.Close()

	if tty {
		restore, err := tui.RawTerminal(inFile)
		if err == nil {
			defer restore()
		}

		stop := watchResize(outFile, func(rows, cols int) {
			b, _ := json.Marshal(map[string]any{"type": "resize", "rows": rows, "cols": cols})
			_ = ws.WriteText(b)
		})
		defer stop()
	}

	go func() {
		buf := make([]byte, 32<<10)
		buf[0] = ptyStdin

		for {
			n, err := in.Read(buf[1:])
			if n > 0 {
				if ws.WriteMessage(wsclient.BinaryMessage, buf[:1+n]) != nil {
					return
				}
			}

			if err != nil {
				return
			}
		}
	}()

	return readPTY(ws, out, errw)
}

func createPTY(ctx context.Context, hc *http.Client, token string, body map[string]any) (string, error) {
	b, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, execdBase+"/pty", bytes.NewReader(b))
	if err != nil {
		return "", err
	}

	req.Header.Set(osbclient.ExecdTokenHeader, token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("creating a terminal session: %w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("creating a terminal session: execd answered %d: %s",
			resp.StatusCode, bytes.TrimSpace(data))
	}

	var r struct {
		ID string `json:"session_id"`
	}

	if err := json.Unmarshal(data, &r); err != nil || r.ID == "" {
		return "", fmt.Errorf("creating a terminal session: execd answered %q, want {\"session_id\":...}", data)
	}

	return r.ID, nil
}

// readPTY copies the session's output until it exits, and returns its status.
func readPTY(ws *wsclient.Conn, out, errw io.Writer) error {
	for {
		typ, data, err := ws.ReadMessage()
		if err != nil {
			return fmt.Errorf("the terminal connection ended before the command exited: %w", err)
		}

		if typ == wsclient.BinaryMessage {
			if len(data) == 0 {
				continue
			}

			switch data[0] {
			case ptyStdout:
				_, _ = out.Write(data[1:])
			case ptyStderr:
				_, _ = errw.Write(data[1:])
			case ptyReplay:
				// 0x03 + an 8-byte offset: scrollback from before this attach.
				if len(data) >= 9 {
					_, _ = out.Write(data[9:])
				}
			}

			continue
		}

		var f struct {
			Type     string `json:"type"`
			ExitCode *int   `json:"exit_code"`
			Error    string `json:"error"`
			Code     string `json:"code"`
		}

		if json.Unmarshal(data, &f) != nil {
			continue
		}

		switch f.Type {
		case "exit":
			if f.ExitCode != nil && *f.ExitCode != 0 {
				return &ExitError{Code: *f.ExitCode}
			}

			return nil
		case "error":
			return fmt.Errorf("execd: %s %s", f.Code, f.Error)
		}
	}
}

// DialGuestPort opens a stream to a port inside a VM over the guest channel: a guest AF_VSOCK
// listener, which in practice is execd.
func (p *fcProvider) DialGuestPort(ctx context.Context, sandbox, service string, port int) (net.Conn, error) {
	vm, err := p.load(containerName(sandbox, service))
	if err != nil {
		return nil, err
	}

	return p.guest.Dial(ctx, p.guestVM(vm), port)
}

// GuestDialer is the daemon's seam (provider.GuestDialer), and it covers ONE leg: execd's port,
// the only thing in a VM that listens on vsock. A workload's own ports - redis's 6379, a web
// server's 8080 - listen on TCP in the guest, and a vsock CONNECT to them is hung up on by
// Firecracker; execd's /proxy/{port} would carry HTTP only, never a database protocol. So those
// stay on the tap (ok=false): the daemon runs beside the VM (on a Mac, inside the helper VM), and
// the guest's address on its bridge is one hop away. The TCP leg is the explicit fallback, not
// an accident: removing it would cut every non-HTTP workload off.
func (p *fcProvider) GuestDialer(sandbox, service string, guestPort int) (DialFunc, bool) {
	if guestPort != fc.ExecdVsockPort || !p.guest.Available() {
		return nil, false
	}

	return func(ctx context.Context) (net.Conn, error) {
		return p.DialGuestPort(ctx, sandbox, service, guestPort)
	}, true
}
