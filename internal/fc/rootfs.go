package fc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// OCI image → ext4 root filesystem.
//
// Through the docker engine the host already has, not a registry client: `docker create` then
// `docker export` hands back the image's filesystem already flattened, so there are no layers
// to apply and no `.wh.` whiteouts to honour - the engine applied them when it assembled the
// container's rootfs. The ROADMAP budgeted four weeks for "apply layers in userspace honouring
// whiteouts"; export makes that work unnecessary for any image the engine can pull, which is
// every image a docker sandbox can already use, with the engine's credentials and mirrors. A
// registry client in sbx would be a second copy of all of that with zero dependencies to build
// it from.
//
// The ext4 is built by e2fsprogs' `mkfs.ext4 -d`, shelled out - the same rule as tunnels: sbx
// drives the tool that exists rather than reimplementing a filesystem writer. Its absence is a
// refusal with the package name, never a fallback.
//
// The image rootfs holds the image and nothing else. The agent and the per-VM init config go on
// a second, tiny drive (BuildAgentDrive), and PID 1 bind-mounts the agent into /opt/sbx at boot.
// So the rootfs is keyed by the image's ID alone and a rebuilt sbx binary does not invalidate
// every cached rootfs on the machine - DECISIONS.md: a built artifact is keyed by its content.

// ImageConfig is what an image says it runs.
type ImageConfig struct {
	ID         string   `json:"id"` // sha256:<hex> of the image config - content-addressed
	Entrypoint []string `json:"entrypoint"`
	Cmd        []string `json:"cmd"`
	Env        []string `json:"env"`
	WorkingDir string   `json:"working_dir"`
	User       string   `json:"user"`
	OS         string   `json:"os"`
	Arch       string   `json:"arch"`
}

// ErrNoImage is an image the engine does not have locally.
var ErrNoImage = errors.New("image not present")

// Engine is the part of a container engine the pipeline uses.
type Engine interface {
	Inspect(ctx context.Context, image string) (ImageConfig, error) // ErrNoImage when absent
	Pull(ctx context.Context, image string) error
	Export(ctx context.Context, image string, w io.Writer) error // the flattened filesystem, as tar
	CopyOut(ctx context.Context, image, path, dst string) error  // one file out of an image
}

// Ext4Builder makes an ext4 image from a directory, or from a tar where it can.
type Ext4Builder interface {
	// TakesTar reports whether Build accepts a tar file as src (e2fsprogs >= 1.47.1), which
	// keeps owners and modes without extracting as root.
	TakesTar(ctx context.Context) bool

	// Build writes an ext4 filesystem holding src into img, which already exists at its size.
	Build(ctx context.Context, src, img, label string) error
}

// RootfsBuilder turns images into cached, read-only-by-convention ext4 files.
type RootfsBuilder struct {
	Dir    string // cache root: one subdirectory per image ID
	Engine Engine
	Ext4   Ext4Builder

	// Root is whether this process can chown. Extracting a tar as anyone else collapses every
	// file's owner to that user - a postgres data directory owned by postgres becomes one owned
	// by uid 1000, and postgres refuses to start - so without root, and without an mkfs that
	// takes the tar directly, the build is refused rather than produced wrong.
	Root bool

	// Headroom is free space added on top of the image's content, for the workload to write
	// into. The file is sparse, so this costs nothing on disk until it is used.
	Headroom int64
}

// DefaultHeadroom is what a sandbox can write before its root filesystem is full.
const DefaultHeadroom = 2 << 30

// Rootfs is a built image root filesystem.
type Rootfs struct {
	Path   string // the cached ext4; clone it, never boot it directly
	Config ImageConfig
}

// Build returns the cached rootfs for image, pulling and building it on first use.
func (b *RootfsBuilder) Build(ctx context.Context, image string) (Rootfs, error) {
	cfg, err := b.Engine.Inspect(ctx, image)
	if errors.Is(err, ErrNoImage) {
		if err := b.Engine.Pull(ctx, image); err != nil {
			return Rootfs{}, err
		}

		cfg, err = b.Engine.Inspect(ctx, image)
	}

	if err != nil {
		return Rootfs{}, err
	}

	key := strings.TrimPrefix(cfg.ID, "sha256:")
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(key) {
		return Rootfs{}, fmt.Errorf("image %s has ID %q, which is not a sha256 - sbx keys the "+
			"rootfs cache by it and will not guess", image, cfg.ID)
	}

	dir, err := ensureBuilt(b.Dir, key, cfg.ID, func(tmp string) error {
		return b.fill(ctx, image, cfg, tmp)
	})
	if err != nil {
		return Rootfs{}, fmt.Errorf("building a root filesystem from %s: %w", image, err)
	}

	return Rootfs{Path: filepath.Join(dir, RootfsName), Config: cfg}, nil
}

func (b *RootfsBuilder) fill(ctx context.Context, image string, cfg ImageConfig, tmp string) error {
	tarPath := filepath.Join(tmp, "rootfs.tar")

	f, err := os.Create(tarPath)
	if err != nil {
		return err
	}

	if err := b.Engine.Export(ctx, image, f); err != nil {
		f.Close()
		return err
	}

	if err := f.Close(); err != nil {
		return err
	}

	content, err := tarContentSize(tarPath)
	if err != nil {
		return fmt.Errorf("reading the exported filesystem: %w", err)
	}

	src := tarPath

	if !b.Ext4.TakesTar(ctx) {
		if !b.Root {
			return errors.New("building an ext4 from a directory needs root, or every file's " +
				"owner becomes yours and services that check ownership (postgres, sshd) refuse to " +
				"start. Run sbx as root, or install e2fsprogs 1.47.1 or later, whose mkfs.ext4 -d " +
				"reads the tar directly and keeps the owners")
		}

		tree := filepath.Join(tmp, "tree")
		if err := extractTar(tarPath, tree, true); err != nil {
			return fmt.Errorf("unpacking the exported filesystem: %w", err)
		}

		defer os.RemoveAll(tree)

		src = tree
	}

	headroom := b.Headroom
	if headroom <= 0 {
		headroom = DefaultHeadroom
	}

	img := filepath.Join(tmp, RootfsName)
	if err := sparseFile(img, roundMiB(content+content/5+headroom)); err != nil {
		return err
	}

	if err := b.Ext4.Build(ctx, src, img, "sbxroot"); err != nil {
		return err
	}

	_ = os.Remove(tarPath)

	meta, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(tmp, "image.json"), meta, 0o644)
}

func roundMiB(n int64) int64 { return (n + (1<<20 - 1)) &^ (1<<20 - 1) }

func sparseFile(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	if err := f.Truncate(size); err != nil {
		f.Close()
		return err
	}

	return f.Close()
}

// AgentDrive is what goes on a VM's first drive.
type AgentDrive struct {
	Agent  string     // host path of the linux sbx binary
	Config InitConfig // becomes /init.json
}

// BuildAgentDrive writes the per-VM boot drive to dst: /sbx (the agent, which the kernel runs
// as init), /init.json, and the empty directories PID 1 mounts onto before it switches root.
// It is the guest's root device, attached read-only, and the image rootfs is the second drive.
//
// dst is created 0600: init.json carries the service's environment, which is where secrets go.
func BuildAgentDrive(ctx context.Context, ext4 Ext4Builder, d AgentDrive, dst string) error {
	stage, err := os.MkdirTemp(filepath.Dir(dst), ".agent-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	for _, dir := range []string{"dev", "proc", "sys", "newroot"} {
		if err := os.Mkdir(filepath.Join(stage, dir), 0o755); err != nil {
			return err
		}
	}

	if err := copyFile(d.Agent, filepath.Join(stage, "sbx"), 0o755); err != nil {
		return fmt.Errorf("the agent binary: %w", err)
	}

	cfg, err := json.Marshal(d.Config)
	if err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(stage, "init.json"), cfg, 0o600); err != nil {
		return err
	}

	st, err := os.Stat(d.Agent)
	if err != nil {
		return err
	}

	if err := sparseFile(dst, roundMiB(st.Size()+int64(len(cfg))+16<<20)); err != nil {
		return err
	}

	return ext4.Build(ctx, stage, dst, "sbxagent")
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}

	return out.Close()
}

// DockerCLI is Engine over the docker command, pinned to one endpoint through Env (the
// provider passes the DOCKER_HOST it resolved, so the pipeline reads the same engine the rest
// of sbx does - never whatever `docker context` happens to be active).
type DockerCLI struct {
	Env []string // appended to the process environment
}

func (d DockerCLI) cmd(ctx context.Context, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, "docker", args...)
	c.Env = append(os.Environ(), d.Env...)

	return c
}

func (d DockerCLI) run(ctx context.Context, args ...string) (string, error) {
	var stderr bytes.Buffer

	c := d.cmd(ctx, args...)
	c.Stderr = &stderr

	out, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}

	return strings.TrimSpace(string(out)), nil
}

func (d DockerCLI) Inspect(ctx context.Context, image string) (ImageConfig, error) {
	// The whole object, decoded here: a --format template naming .Config.Entrypoint fails with
	// "map has no entry for key" on an image that has none (alpine:3, on the engine this was
	// tested against), so the absent-field rule has to be Go's, not the template's.
	out, err := d.run(ctx, "image", "inspect", "--format", "{{json .}}", image)
	if err != nil {
		if strings.Contains(err.Error(), "No such image") || strings.Contains(err.Error(), "No such object") {
			return ImageConfig{}, fmt.Errorf("%w: %s", ErrNoImage, image)
		}

		return ImageConfig{}, err
	}

	var raw struct {
		ID     string `json:"Id"`
		Config struct {
			Entrypoint []string
			Cmd        []string
			Env        []string
			WorkingDir string
			User       string
		}
		Os           string
		Architecture string
	}

	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return ImageConfig{}, fmt.Errorf("docker image inspect %s: %w", image, err)
	}

	return ImageConfig{
		ID: raw.ID, Entrypoint: raw.Config.Entrypoint, Cmd: raw.Config.Cmd, Env: raw.Config.Env,
		WorkingDir: raw.Config.WorkingDir, User: raw.Config.User, OS: raw.Os, Arch: raw.Architecture,
	}, nil
}

func (d DockerCLI) Pull(ctx context.Context, image string) error {
	_, err := d.run(ctx, "pull", "--quiet", image)
	return err
}

// Export creates a container that is never started, streams its filesystem, and removes it.
// The entrypoint is overridden only so that `create` accepts images with no CMD at all.
func (d DockerCLI) Export(ctx context.Context, image string, w io.Writer) error {
	id, err := d.run(ctx, "create", "--label", "sbx.fc.export=1", "--entrypoint", "/sbx-export", image)
	if err != nil {
		return err
	}

	defer func() { _, _ = d.run(context.WithoutCancel(ctx), "rm", "-f", id) }()

	var stderr bytes.Buffer

	c := d.cmd(ctx, "export", id)
	c.Stdout = w
	c.Stderr = &stderr

	if err := c.Run(); err != nil {
		return fmt.Errorf("docker export %s: %w: %s", image, err, strings.TrimSpace(stderr.String()))
	}

	return nil
}

func (d DockerCLI) CopyOut(ctx context.Context, image, path, dst string) error {
	id, err := d.run(ctx, "create", "--entrypoint", "/sbx-export", image)
	if err != nil {
		return err
	}

	defer func() { _, _ = d.run(context.WithoutCancel(ctx), "rm", "-f", id) }()

	_, err = d.run(ctx, "cp", id+":"+path, dst)

	return err
}

// Mkfs is Ext4Builder over e2fsprogs' mkfs.ext4.
type Mkfs struct {
	Path string // from hostcap.Report.Mkfs; empty means absent
}

// ErrNoMkfs is the refusal when e2fsprogs is missing.
var ErrNoMkfs = errors.New("mkfs.ext4 not found: the firecracker provider builds root " +
	"filesystems with it. Install e2fsprogs (apt install e2fsprogs, dnf install e2fsprogs, " +
	"apk add e2fsprogs) on the machine that runs firecracker")

var mkfsVersion = regexp.MustCompile(`mke2fs (\d+)\.(\d+)(?:\.(\d+))?`)

func (m Mkfs) TakesTar(ctx context.Context) bool {
	if m.Path == "" {
		return false
	}

	out, _ := exec.CommandContext(ctx, m.Path, "-V").CombinedOutput()

	return takesTar(string(out))
}

// takesTar parses `mke2fs -V`. Tar input to -d arrived in e2fsprogs 1.47.1.
func takesTar(version string) bool {
	v := mkfsVersion.FindStringSubmatch(version)
	if v == nil {
		return false
	}

	n := func(s string) int { i, _ := strconv.Atoi(s); return i }
	major, minor, patch := n(v[1]), n(v[2]), n(v[3])

	switch {
	case major != 1:
		return major > 1
	case minor != 47:
		return minor > 47
	default:
		return patch >= 1
	}
}

func (m Mkfs) Build(ctx context.Context, src, img, label string) error {
	if m.Path == "" {
		return ErrNoMkfs
	}

	// -F: img is a regular file, not a block device, and mkfs asks before writing to one.
	// No size argument: mkfs takes the file's (sparse) size.
	out, err := exec.CommandContext(ctx, m.Path, "-q", "-F", "-t", "ext4", "-L", label,
		"-d", src, img).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s -d %s %s: %w: %s", m.Path, src, img, err, strings.TrimSpace(string(out)))
	}

	return nil
}
