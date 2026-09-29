// Package agentbin finds a linux sbx binary to run inside a sandbox as its agent.
//
// The agent is this same program built for linux and the sandbox's architecture: `sbx execd` in
// a container, and PID 1 (`sbx fc-init`, then execd) in a Firecracker VM. Both providers need the
// same answer to "where is that binary", so the search lives here once - it used to live in
// internal/osb alone, and a second copy in the microVM provider would have been the first to
// disagree about a dev build.
//
// Where it comes from, in order:
//
//  1. $SBX_EXECD_BINARY - an explicit path, for anyone whose setup the rest guesses wrong.
//  2. This process's own executable, when the host IS linux on the same architecture.
//  3. A cross-compile of this module, when `go` is on PATH and the source is findable - the
//     development path, where the version is "dev" and no published image matches it.
//  4. The published activator image for this version, which carries the binary at
//     /usr/local/bin/sbx for every release architecture.
package agentbin

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/aryanmehrotra/sbx/internal/logs"
)

const (
	// ModulePath identifies a checkout of this repository.
	ModulePath = "github.com/aryanmehrotra/sbx"

	// Activator is the published image carrying the binary at ImagePath.
	Activator = "ghcr.io/aryanmehrotra/sbx-activator"
	ImagePath = "/usr/local/bin/sbx"
)

// Source is where the binary is: a file on this machine, or an image to copy it out of.
type Source struct {
	File  string
	Image string // with the binary at ImagePath
}

// Locate finds a linux/arch sbx binary for a build whose version string is version.
func Locate(ctx context.Context, arch, version string) (Source, error) {
	if p := os.Getenv("SBX_EXECD_BINARY"); p != "" {
		return Source{File: p}, nil
	}

	if runtime.GOOS == "linux" && runtime.GOARCH == arch {
		if self, err := os.Executable(); err == nil {
			return Source{File: self}, nil
		}
	}

	var buildErr error

	// A release build never compiles whatever checkout it happens to find - , next
	// to the executable, or above the working directory - into the binary it runs as root in a
	// sandbox or the helper VM: that is some other source tree at some other commit, possibly one
	// the person merely cloned. It uses the published artifact for its own version, whose
	// checksum the release carries. Dev builds, which have no published artifact, still compile.
	if gobin, err := exec.LookPath("go"); err == nil && !Release(version) {
		if src, ok := FindSource(); ok {
			out, err := compileOnce(ctx, gobin, src, arch, version)
			if err == nil {
				return Source{File: out}, nil
			}

			buildErr = err
		}
	}

	// Only a release has a published image. The test was `version != "dev"`, so a build stamped
	// by git describe - v0.15.1-dev+ffd872d - asked docker for an image that was never pushed and
	// whose tag docker cannot even parse ('+'), and the user was told to raise a timeout.
	if Release(version) {
		return Source{Image: Activator + ":" + version}, nil
	}

	msg := fmt.Sprintf("no linux/%s sbx binary to run as the sandbox agent: this is a dev build "+
		"(%q; no published image matches it), go is not on PATH or the source was not found, and "+
		"SBX_EXECD_BINARY is not set. Build one - `CGO_ENABLED=0 GOOS=linux GOARCH=%s go build -o "+
		"sbx-linux-%s .` in the sbx checkout - and set SBX_EXECD_BINARY to it", arch, version, arch, arch)

	if buildErr != nil {
		msg += fmt.Sprintf(" (the cross-compile was tried and failed: %v)", buildErr)
	}

	return Source{}, errors.New(msg)
}

// crossCompile is CrossCompile; a variable so a test can see whether Locate reached for it.
var crossCompile = CrossCompile

// Release reports whether version is a published release (vX.Y.Z), as release.sh stamps it.
func Release(version string) bool { return releaseRE.MatchString(version) }

var releaseRE = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)

// CrossCompile builds this module for linux into ~/.sbx/execd, statically, so it runs in any
// image - including one with no libc.
func CrossCompile(ctx context.Context, gobin, src, arch, version string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	out := filepath.Join(home, ".sbx", "execd", "linux-"+arch, "sbx")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}

	cmd := exec.CommandContext(ctx, gobin, "build", "-trimpath",
		"-ldflags", "-s -w -X main.version="+version, "-o", out, ".")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)

	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build in %s: %w: %s", src, err, strings.TrimSpace(string(b)))
	}

	return out, nil
}

// FindSource looks for this module's checkout: $SBX_SOURCE_DIR, then upward from the
// executable (a `go build -o sbx .` binary sits in the checkout), then upward from the working
// directory (which is where `go test` runs).
func FindSource() (string, bool) {
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

	if strings.TrimSpace(sc.Text()) != "module "+ModulePath {
		return false
	}

	_, err = os.Stat(filepath.Join(dir, "main.go"))

	return err == nil
}

// build is one cross-compile of the agent, shared by every caller that asks for the same one.
type build struct {
	done chan struct{}
	out  string
	err  error
}

var builds struct {
	mu sync.Mutex
	m  map[string]*build
}

// compileOnce is crossCompile at most once per process for a given checkout, architecture and
// version - concurrent callers wait for the one in flight, later ones get its result.
//
// A source build's first OpenSandbox sandbox used to pay for this compile inside its create: 50s
// on a cold go cache, measured on the report that found it, against a client ready timeout of
// 30s (sbx mcp's default), so the first create failed and said nothing about why. The daemon now
// starts the build when it starts serving the API, and the create that follows joins it here
// rather than compiling a second time. A failure is not kept, so the next caller retries.
//
// Logged at INFO at both ends: it is the one step of an API create that can take a minute, and
// the only one that happens once.
func compileOnce(ctx context.Context, gobin, src, arch, version string) (string, error) {
	key := src + "\x00" + arch + "\x00" + version

	builds.mu.Lock()
	if builds.m == nil {
		builds.m = map[string]*build{}
	}

	if b, ok := builds.m[key]; ok {
		builds.mu.Unlock()

		select {
		case <-b.done:
			return b.out, b.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	b := &build{done: make(chan struct{})}
	builds.m[key] = b
	builds.mu.Unlock()

	logs.Default.Info("", "", "building the sandbox agent (sbx for linux/%s) from %s, once for this "+
		"daemon; the first API sandbox waits for it", arch, src)

	began := time.Now()

	// Not the caller's cancellation: the build serves every caller waiting on it.
	b.out, b.err = crossCompile(context.WithoutCancel(ctx), gobin, src, arch, version)

	if b.err != nil {
		builds.mu.Lock()
		delete(builds.m, key)
		builds.mu.Unlock()

		logs.Default.Warn("", "", "building the sandbox agent for linux/%s failed after %s: %v",
			arch, time.Since(began).Round(time.Millisecond), b.err)
	} else {
		logs.Default.Info("", "", "built the sandbox agent for linux/%s in %s: %s",
			arch, time.Since(began).Round(time.Millisecond), b.out)
	}

	close(b.done)

	return b.out, b.err
}

// forgetBuilds drops every remembered build, for a test that swaps crossCompile.
func forgetBuilds() {
	builds.mu.Lock()
	builds.m = nil
	builds.mu.Unlock()
}
