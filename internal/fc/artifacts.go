package fc

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// The pinned VMM and guest kernel. Pinned by sha256, not by URL: a URL is where to look, the
// hash is what must be found there, and the two can drift apart without anybody noticing.
//
// The firecracker hashes are the release's own `.sha256.txt`; the aarch64 kernel hash was
// measured by the spike and re-measured when this was written, the x86_64 one measured when
// this was written. The kernel comes from Firecracker's CI bucket, keyed by a dated prefix and
// NOT by version - `firecracker-ci/v1.17/` is empty, and a pipeline that computes the prefix from
// the release tag finds nothing (spike, "Setup").
//
// Why this kernel and not a distro's: its config has CONFIG_VSOCKETS=y and
// CONFIG_VIRTIO_VSOCKETS=y built in, so the ROADMAP's vermagic landmine - vsock modules that do
// not match the kernel, failing silently - cannot fire. It also has CONFIG_IP_PNP=y, which is
// what lets the guest's address arrive on the kernel command line with no network tooling in the
// image, and CONFIG_VMGENID=y for entropy after a restore.
const (
	FirecrackerVersion = "v1.17.0"
	KernelVersion      = "6.18.48"

	releaseBase = "https://github.com/firecracker-microvm/firecracker/releases/download/" + FirecrackerVersion
	kernelBase  = "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/20260923-6f82ac4cf331-0"

	builtSentinel = ".built"
)

// Pin is one architecture's artifacts.
type Pin struct {
	FirecrackerURL    string
	FirecrackerSHA256 string // of the .tgz
	FirecrackerMember string // the binary's path inside it
	KernelURL         string
	KernelSHA256      string
}

// Pins maps GOARCH to what to fetch. An architecture absent here has no pinned artifacts and
// needs SBX_FC_BINARY and SBX_FC_KERNEL.
var Pins = map[string]Pin{
	"arm64": {
		FirecrackerURL:    releaseBase + "/firecracker-" + FirecrackerVersion + "-aarch64.tgz",
		FirecrackerSHA256: "e351ebe4f7a16b5873bbd51005d2e6767103cff4d5ebc829df2d3f95a93e2256",
		FirecrackerMember: "release-" + FirecrackerVersion + "-aarch64/firecracker-" + FirecrackerVersion + "-aarch64",
		KernelURL:         kernelBase + "/aarch64/vmlinux-" + KernelVersion,
		KernelSHA256:      "a80108af80d9549b357ea7e00bd5c12f80686869541d135a8a67f6fe1ec3451e",
	},
	"amd64": {
		FirecrackerURL:    releaseBase + "/firecracker-" + FirecrackerVersion + "-x86_64.tgz",
		FirecrackerSHA256: "06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558",
		FirecrackerMember: "release-" + FirecrackerVersion + "-x86_64/firecracker-" + FirecrackerVersion + "-x86_64",
		KernelURL:         kernelBase + "/x86_64/vmlinux-" + KernelVersion,
		KernelSHA256:      "9204218e8bcca6ac23848d74f45df2eb19d7f31e8277840a7d145a0df8b078d2",
	},
}

// Artifacts are the two files a VM needs from outside its own directory.
type Artifacts struct {
	Firecracker string
	Kernel      string
}

// ArtifactCache fetches and keeps the pinned artifacts under Dir.
type ArtifactCache struct {
	Dir    string
	HTTP   *http.Client
	Getenv func(string) string // os.Getenv; a field so a test can set overrides without the process env
	Pins   map[string]Pin      // Pins; a field so a test can point at a local server
}

// NewArtifactCache caches under dir with the real pins and environment.
func NewArtifactCache(dir string) *ArtifactCache {
	return &ArtifactCache{Dir: dir, HTTP: http.DefaultClient, Getenv: os.Getenv, Pins: Pins}
}

// Resolve returns the firecracker binary and kernel for arch (GOARCH spelling), fetching
// whichever is not already cached. SBX_FC_BINARY and SBX_FC_KERNEL replace either one with a
// file the operator supplies; those are used as given and never hashed, since the point of an
// override is to run something that is not the pin.
func (c *ArtifactCache) Resolve(ctx context.Context, arch string) (Artifacts, error) {
	var a Artifacts

	pin, pinned := c.Pins[arch]

	if p := c.Getenv("SBX_FC_BINARY"); p != "" {
		if err := isFile(p, "SBX_FC_BINARY"); err != nil {
			return a, err
		}

		a.Firecracker = p
	}

	if p := c.Getenv("SBX_FC_KERNEL"); p != "" {
		if err := isFile(p, "SBX_FC_KERNEL"); err != nil {
			return a, err
		}

		a.Kernel = p
	}

	if (a.Firecracker == "" || a.Kernel == "") && !pinned {
		return a, fmt.Errorf("no pinned firecracker or guest kernel for %s: sbx pins x86_64 and "+
			"aarch64 only. Set SBX_FC_BINARY and SBX_FC_KERNEL to files you trust", arch)
	}

	if a.Firecracker == "" {
		dir, err := c.ensure(ctx, "firecracker-"+FirecrackerVersion+"-"+arch, pin.FirecrackerSHA256,
			func(tmp string) error {
				return c.fetchTgzMember(ctx, pin.FirecrackerURL, pin.FirecrackerSHA256, pin.FirecrackerMember,
					filepath.Join(tmp, "firecracker"))
			})
		if err != nil {
			return a, err
		}

		a.Firecracker = filepath.Join(dir, "firecracker")
	}

	if a.Kernel == "" {
		dir, err := c.ensure(ctx, "vmlinux-"+KernelVersion+"-"+arch, pin.KernelSHA256,
			func(tmp string) error {
				return c.fetchFile(ctx, pin.KernelURL, pin.KernelSHA256, filepath.Join(tmp, "vmlinux"))
			})
		if err != nil {
			return a, err
		}

		a.Kernel = filepath.Join(dir, "vmlinux")
	}

	return a, nil
}

func isFile(p, env string) error {
	st, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("%s=%s: %w", env, p, err)
	}

	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s=%s is not a regular file", env, p)
	}

	return nil
}

// ensure returns Dir/<name>-<sha[:12]>, filling it with fill on first use.
//
// The directory is built under a temporary name and renamed into place only once it is
// complete and carries its .built sentinel, so a crash or a Ctrl-C mid-download leaves either
// nothing or a stray temp dir - never a final path holding half a kernel that the next run
// would trust. The hash is in the name so changing a pin can never be served an old download.
func (c *ArtifactCache) ensure(ctx context.Context, name, sha string, fill func(tmp string) error) (string, error) {
	dir, err := ensureBuilt(c.Dir, name+"-"+shortSHA(sha), sha, fill)
	if err != nil {
		return "", err
	}

	return dir, ctx.Err()
}

// ensureBuilt returns root/name, filling it with fill on first use. The rootfs cache shares it.
func ensureBuilt(root, name, stamp string, fill func(tmp string) error) (string, error) {
	final := filepath.Join(root, name)

	if _, err := os.Stat(filepath.Join(final, builtSentinel)); err == nil {
		return final, nil
	}

	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}

	// A final path without a sentinel is a previous run's crash between rename and
	// sentinel - impossible with the order below, but cheap to heal rather than to trust.
	if _, err := os.Stat(final); err == nil {
		if err := os.RemoveAll(final); err != nil {
			return "", err
		}
	}

	tmp, err := os.MkdirTemp(root, ".tmp-"+name+"-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp) // a no-op after a successful rename

	if err := fill(tmp); err != nil {
		return "", err
	}

	if err := os.WriteFile(filepath.Join(tmp, builtSentinel), []byte(stamp+"\n"), 0o644); err != nil {
		return "", err
	}

	if err := os.Rename(tmp, final); err != nil {
		// Somebody else finished the same download first; theirs is as good as ours.
		if _, serr := os.Stat(filepath.Join(final, builtSentinel)); serr == nil {
			return final, nil
		}

		return "", err
	}

	return final, nil
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}

	return s
}

// ErrChecksum is a download whose bytes are not the pinned ones.
var ErrChecksum = errors.New("checksum mismatch")

func (c *ArtifactCache) get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w - set SBX_FC_BINARY/SBX_FC_KERNEL to local "+
			"copies on a machine without network", url, err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("downloading %s: %s", url, resp.Status)
	}

	return resp.Body, nil
}

// fetchFile downloads url to dst and refuses it unless its sha256 is want.
func (c *ArtifactCache) fetchFile(ctx context.Context, url, want, dst string) error {
	body, err := c.get(ctx, url)
	if err != nil {
		return err
	}
	defer body.Close()

	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), body); err != nil {
		return fmt.Errorf("downloading %s: %w", url, err)
	}

	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%w: %s is %s, pinned %s - the upstream file changed or the download "+
			"was tampered with; sbx will not run it", ErrChecksum, url, got, want)
	}

	return f.Close()
}

// fetchTgzMember downloads a release tarball, checks the TARBALL's hash (that is what the
// release publishes), and extracts one member as an executable.
func (c *ArtifactCache) fetchTgzMember(ctx context.Context, url, want, member, dst string) error {
	tgz := dst + ".tgz"
	if err := c.fetchFile(ctx, url, want, tgz); err != nil {
		return err
	}
	defer os.Remove(tgz)

	f, err := os.Open(tgz)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", url, err)
	}

	tr := tar.NewReader(gz)

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s has no %s in it - the release layout changed; pin a new "+
				"FirecrackerMember", url, member)
		}

		if err != nil {
			return fmt.Errorf("%s: %w", url, err)
		}

		if strings.TrimPrefix(h.Name, "./") != member {
			continue
		}

		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}

		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}

		return out.Close()
	}
}
