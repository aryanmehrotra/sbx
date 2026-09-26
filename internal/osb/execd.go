package osb

// Getting `sbx execd` into a sandbox whose image has never heard of sbx.
//
// The agent is this same binary, built for linux and the container's architecture. It goes into
// a named volume once and is mounted read-only at /opt/sbx in every API sandbox, so the image
// is used as it is - no rebuild, no layer, nothing the caller has to prepare.
//
// Where the binary comes from is internal/agentbin's answer - the same search the microVM
// provider uses - and this file only decides which volume it goes into.
//
// A volume filled from a FILE is named for the file's content, not just the version: a dev
// build is "dev" every time it is rebuilt, and a volume keyed only on that would keep serving
// the first build's execd for ever. That is the same rule DECISIONS.md already applies to built
// images - keyed by content, never by age.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/aryanmehrotra/sbx/internal/agentbin"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

const (
	execdPort   = 44772
	execdMount  = "/opt/sbx"
	execdBinary = "sbx"

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
	src, err := agentbin.Locate(ctx, arch, e.version)
	if err != nil {
		return execdSource{}, err
	}

	if src.File != "" {
		return e.fromFile(src.File)
	}

	return execdSource{Volume: volumeName(e.version), Image: src.Image}, nil
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

// ErrPublishedOnly is AgentFile's answer when the only linux binary for this version is the one
// inside the published activator image: a release build on a machine with no linux sbx and no
// go toolchain. The caller that cannot pull an image fetches the release asset instead.
var ErrPublishedOnly = errors.New("no local linux sbx binary; only the published release has one")

// AgentFile is the file half of agentbin.Locate, for a caller that needs the linux sbx binary
// itself rather than a volume holding it - the Firecracker helper VM, which runs the same version
// of sbx as the host. One lookup order for both, so the two cannot disagree about which binary
// "this version" is.
func AgentFile(ctx context.Context, version, arch string) (string, error) {
	src, err := agentbin.Locate(ctx, arch, version)
	if err != nil {
		return "", err
	}

	if src.File == "" {
		return "", ErrPublishedOnly
	}

	return src.File, nil
}

// runsAgent reports whether the provider's sandboxes already run execd as their agent
// (provider.RunsAgent: a microVM). Then there is nothing to place: no volume, no /opt/sbx
// mount, no entrypoint wrapper, and the provider boots execd with the token in the env.
func (s *Server) runsAgent() bool {
	_, ok := s.p.(provider.RunsAgent)
	return ok
}

// inspector is the provider's image inspection, from whichever capability carries it: RunsAgent
// for a provider that runs the agent itself, Injector for one sbx puts it into. Neither is the
// API's refusal, naming the backend.
func (s *Server) inspector() (provider.ImageInspector, error) {
	if ra, ok := s.p.(provider.RunsAgent); ok {
		return ra, nil
	}

	return provider.InjectorFor(s.p)
}
