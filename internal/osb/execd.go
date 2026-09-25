package osb

// Getting `sbx execd` into a sandbox whose image has never heard of sbx.
//
// The agent is this same binary, built for linux and the container's architecture. It goes into
// a named volume once and is mounted read-only at /opt/sbx in every API sandbox, so the image
// is used as it is - no rebuild, no layer, nothing the caller has to prepare.
//
// Where the binary comes from, in order:
//
//  1. $SBX_EXECD_BINARY - an explicit path, for anyone whose setup the rest guesses wrong.
//  2. This process's own executable, when the host IS linux on the same architecture.
//  3. A cross-compile of this module, when `go` is on PATH and the source is findable - the
//     development path, where the version is "dev" and no published image matches it.
//  4. The published activator image for this version, which carries the binary at
//     /usr/local/bin/sbx for every release architecture.
//
// A volume filled from a FILE is named for the file's content, not just the version: a dev
// build is "dev" every time it is rebuilt, and a volume keyed only on that would keep serving
// the first build's execd for ever. That is the same rule DECISIONS.md already applies to built
// images - keyed by content, never by age.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

const (
	execdPort   = 44772
	execdMount  = "/opt/sbx"
	execdBinary = "sbx"
	modulePath  = "github.com/aryanmehrotra/sbx"
	activator   = "ghcr.io/aryanmehrotra/sbx-activator"

	// tokenEnv is the variable upstream execd reads its access token from
	// (components/execd/pkg/flag/parser.go at release-1.1.0); sbx execd reads the same one.
	tokenEnv    = "EXECD_ACCESS_TOKEN"
	tokenHeader = "X-EXECD-ACCESS-TOKEN"
)

// execdSource is where a volume's binary comes from: a file on this machine, or an image.
type execdSource struct {
	Volume string
	File   string // seed from this host file...
	Image  string // ...or from this image's /usr/local/bin
}

// execdResolver finds the agent binary for an architecture, building it at most once per
// architecture per process.
type execdResolver struct {
	version string

	mu    sync.Mutex
	built map[string]execdSource
}

func newExecdResolver(version string) *execdResolver {
	return &execdResolver{version: version, built: map[string]execdSource{}}
}

func (e *execdResolver) resolve(ctx context.Context, arch string) (execdSource, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if src, ok := e.built[arch]; ok {
		return src, nil
	}

	src, err := e.find(ctx, arch)
	if err != nil {
		return execdSource{}, err
	}

	e.built[arch] = src

	return src, nil
}

func (e *execdResolver) find(ctx context.Context, arch string) (execdSource, error) {
	if p := os.Getenv("SBX_EXECD_BINARY"); p != "" {
		return e.fromFile(p)
	}

	if runtime.GOOS == "linux" && runtime.GOARCH == arch {
		if self, err := os.Executable(); err == nil {
			return e.fromFile(self)
		}
	}

	var buildErr error

	if gobin, err := exec.LookPath("go"); err == nil {
		if src, ok := findSource(); ok {
			out, err := e.crossCompile(ctx, gobin, src, arch)
			if err == nil {
				return e.fromFile(out)
			}

			buildErr = err
		}
	}

	if e.version != "" && e.version != "dev" {
		return execdSource{
			Volume: volumeName(e.version),
			Image:  activator + ":" + e.version,
		}, nil
	}

	msg := fmt.Sprintf("no linux/%s sbx binary to run as the sandbox agent: this is a dev build "+
		"(no published image matches it), go is not on PATH or the source was not found, and "+
		"SBX_EXECD_BINARY is not set. Build one - `CGO_ENABLED=0 GOOS=linux GOARCH=%s go build -o "+
		"sbx-linux-%s .` in the sbx checkout - and set SBX_EXECD_BINARY to it", arch, arch, arch)

	if buildErr != nil {
		msg += fmt.Sprintf(" (the cross-compile was tried and failed: %v)", buildErr)
	}

	return execdSource{}, errors.New(msg)
}

func (e *execdResolver) fromFile(path string) (execdSource, error) {
	f, err := os.Open(path)
	if err != nil {
		return execdSource{}, fmt.Errorf("the sandbox agent binary %s: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return execdSource{}, err
	}

	sum := hex.EncodeToString(h.Sum(nil))[:12]

	return execdSource{Volume: volumeName(e.version + "-" + sum), File: path}, nil
}

func volumeName(key string) string {
	key = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, key)

	return "sbx-execd-" + key
}

// crossCompile builds this module for linux into ~/.sbx/execd, statically, so it runs in any
// image - including one with no libc.
func (e *execdResolver) crossCompile(ctx context.Context, gobin, src, arch string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	out := filepath.Join(home, ".sbx", "execd", "linux-"+arch, "sbx")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}

	cmd := exec.CommandContext(ctx, gobin, "build", "-trimpath",
		"-ldflags", "-s -w -X main.version="+e.version, "-o", out, ".")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)

	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build in %s: %w: %s", src, err, strings.TrimSpace(string(b)))
	}

	return out, nil
}

// findSource looks for this module's checkout: $SBX_SOURCE_DIR, then upward from the
// executable (a `go build -o sbx .` binary sits in the checkout), then upward from the working
// directory (which is where `go test` runs).
func findSource() (string, bool) {
	var starts []string

	if d := os.Getenv("SBX_SOURCE_DIR"); d != "" {
		starts = append(starts, d)
	}

	if self, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(self))
	}

	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}

	for _, s := range starts {
		for dir := s; ; dir = filepath.Dir(dir) {
			if isModuleRoot(dir) {
				return dir, true
			}

			if filepath.Dir(dir) == dir {
				break
			}
		}
	}

	return "", false
}

func isModuleRoot(dir string) bool {
	f, err := os.Open(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return false
	}

	if strings.TrimSpace(sc.Text()) != "module "+modulePath {
		return false
	}

	_, err = os.Stat(filepath.Join(dir, "main.go"))

	return err == nil
}

// ErrPublishedOnly is AgentFile's answer when the only linux binary for this version is the one
// inside the published activator image: a release build on a machine with no linux sbx and no
// go toolchain. The caller that cannot pull an image fetches the release asset instead.
var ErrPublishedOnly = errors.New("no local linux sbx binary; only the published release has one")

// AgentFile is the file half of the agent resolution above, for a caller that needs the linux
// sbx binary itself rather than a volume holding it - the Firecracker helper VM, which runs the
// same version of sbx as the host. Same order, same env overrides, same cross-compile.
func AgentFile(ctx context.Context, version, arch string) (string, error) {
	src, err := newExecdResolver(version).find(ctx, arch)
	if err != nil {
		return "", err
	}

	if src.File == "" {
		return "", ErrPublishedOnly
	}

	return src.File, nil
}
