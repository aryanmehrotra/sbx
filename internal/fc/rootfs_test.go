package fc

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

const testID = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type fakeEngine struct {
	present bool
	id      string
	tar     []byte
	pulls   int
	exports int
}

func (e *fakeEngine) Inspect(_ context.Context, image string) (ImageConfig, error) {
	if !e.present {
		return ImageConfig{}, ErrNoImage
	}

	return ImageConfig{ID: e.id, Cmd: []string{"redis-server"}, Env: []string{"PATH=/usr/bin"}, OS: "linux", Arch: "arm64"}, nil
}

func (e *fakeEngine) Pull(context.Context, string) error { e.pulls++; e.present = true; return nil }

func (e *fakeEngine) Export(_ context.Context, _ string, w io.Writer) error {
	e.exports++
	_, err := w.Write(e.tar)

	return err
}

func (e *fakeEngine) CopyOut(context.Context, string, string, string) error { return nil }

// fakeExt4 records what it was asked to build and writes a manifest into the image, so a test
// can see what the filesystem would have held.
type fakeExt4 struct {
	tar   bool
	built []string // "label:src"
}

func (f *fakeExt4) TakesTar(context.Context) bool { return f.tar }

func (f *fakeExt4) Build(_ context.Context, src, img, label string) error {
	st, err := os.Stat(img)
	if err != nil {
		return errors.New("Build called before the image file existed")
	}

	if st.Size() == 0 {
		return errors.New("image file has no size")
	}

	f.built = append(f.built, label+":"+src)

	var names []string

	if fi, _ := os.Stat(src); fi != nil && fi.IsDir() {
		_ = filepath.Walk(src, func(p string, _ os.FileInfo, _ error) error {
			rel, _ := filepath.Rel(src, p)
			names = append(names, filepath.ToSlash(rel))

			return nil
		})
	} else {
		names = append(names, "tar:"+filepath.Base(src))
	}

	b, _ := json.Marshal(names)

	fh, err := os.OpenFile(img, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer fh.Close()

	_, err = fh.WriteAt(b, 0)

	return err
}

func manifest(t *testing.T, img string) []string {
	t.Helper()

	b, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	_ = json.NewDecoder(bytes.NewReader(bytes.TrimRight(b, "\x00"))).Decode(&names)

	return names
}

type entry struct {
	name, link string
	typ        byte
	mode       int64
	body       string
}

func mkTar(t *testing.T, es ...entry) []byte {
	t.Helper()

	var buf bytes.Buffer

	tw := tar.NewWriter(&buf)

	for _, e := range es {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link,
			Size: int64(len(e.body)), Uid: os.Getuid(), Gid: os.Getgid()}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}

		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}

		if e.typ == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}

	tw.Close()

	return buf.Bytes()
}

func imageTar(t *testing.T) []byte {
	return mkTar(t,
		entry{name: "etc/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "etc/os-release", typ: tar.TypeReg, mode: 0o644, body: "ID=test\n"},
		entry{name: "usr/bin/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "usr/bin/tool", typ: tar.TypeReg, mode: 0o4755, body: "#!"},
		entry{name: "bin", typ: tar.TypeSymlink, link: "usr/bin"},
		entry{name: "usr/bin/tool2", typ: tar.TypeLink, link: "usr/bin/tool"},
		entry{name: "dev/null", typ: tar.TypeChar},
	)
}

func TestRootfsFromTarIsCachedByImageID(t *testing.T) {
	eng := &fakeEngine{id: testID, tar: imageTar(t)} // absent: must be pulled first
	ext := &fakeExt4{tar: true}
	b := &RootfsBuilder{Dir: t.TempDir(), Engine: eng, Ext4: ext, Headroom: 64 << 20}

	r, err := b.Build(context.Background(), "redis:7")
	if err != nil {
		t.Fatal(err)
	}

	if eng.pulls != 1 {
		t.Fatalf("pulls = %d, want 1 for an absent image", eng.pulls)
	}

	if want := filepath.Join(b.Dir, strings.TrimPrefix(testID, "sha256:"), RootfsName); r.Path != want {
		t.Fatalf("path = %s, want %s", r.Path, want)
	}

	if !slices.Equal(manifest(t, r.Path), []string{"tar:rootfs.tar"}) {
		t.Fatalf("mkfs was not handed the tar: %v", manifest(t, r.Path))
	}

	if st, _ := os.Stat(r.Path); st.Size() < 64<<20 || st.Size()%(1<<20) != 0 {
		t.Fatalf("size %d is not content + headroom rounded to MiB", st.Size())
	}

	// The tar is gone, the image config is kept beside it.
	if _, err := os.Stat(filepath.Join(filepath.Dir(r.Path), "rootfs.tar")); err == nil {
		t.Fatal("the exported tar was left in the cache")
	}

	if r.Config.Cmd[0] != "redis-server" {
		t.Fatalf("config = %+v", r.Config)
	}

	// Same image ID: no second export.
	if _, err := b.Build(context.Background(), "redis:7"); err != nil {
		t.Fatal(err)
	}

	if eng.exports != 1 {
		t.Fatalf("exports = %d; an unchanged image was rebuilt", eng.exports)
	}

	// A new image ID under the same tag is a new rootfs - the tag is not the key.
	eng.id = "sha256:" + strings.Repeat("f", 64)
	if _, err := b.Build(context.Background(), "redis:7"); err != nil {
		t.Fatal(err)
	}

	if eng.exports != 2 {
		t.Fatal("a changed image under the same tag reused the old rootfs")
	}
}

func TestRootfsWithoutTarSupportNeedsRoot(t *testing.T) {
	eng := &fakeEngine{present: true, id: testID, tar: imageTar(t)}
	b := &RootfsBuilder{Dir: t.TempDir(), Engine: eng, Ext4: &fakeExt4{}, Root: false}

	_, err := b.Build(context.Background(), "postgres:16")
	if err == nil || !strings.Contains(err.Error(), "1.47.1") || !strings.Contains(err.Error(), "root") {
		t.Fatalf("err = %v", err)
	}

	if ents, _ := os.ReadDir(b.Dir); len(ents) != 0 {
		t.Fatalf("a refused build left %v", ents)
	}
}

func TestRootfsFromTreeWhenRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks and hard links in the fixture")
	}

	eng := &fakeEngine{present: true, id: testID, tar: imageTar(t)}
	ext := &fakeExt4{}
	// Root=true with a fixture owned by the running user, so the chown is a no-op anywhere.
	b := &RootfsBuilder{Dir: t.TempDir(), Engine: eng, Ext4: ext, Root: true, Headroom: 1 << 20}

	r, err := b.Build(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}

	got := manifest(t, r.Path)
	for _, want := range []string{"etc/os-release", "usr/bin/tool", "usr/bin/tool2", "bin"} {
		if !slices.Contains(got, want) {
			t.Fatalf("%s missing from %v", want, got)
		}
	}

	if slices.Contains(got, "dev/null") {
		t.Fatal("a device node was created; PID 1 mounts devtmpfs instead")
	}

	// The staging tree does not outlive the build.
	if _, err := os.Stat(filepath.Join(filepath.Dir(r.Path), "tree")); err == nil {
		t.Fatal("tree left in the cache")
	}
}

func TestRootfsRefusesAnUnkeyableID(t *testing.T) {
	b := &RootfsBuilder{Dir: t.TempDir(), Engine: &fakeEngine{present: true, id: "latest"}, Ext4: &fakeExt4{tar: true}}

	if _, err := b.Build(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "not a sha256") {
		t.Fatalf("err = %v", err)
	}
}

func TestExtractKeepsModesAndStaysInside(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix modes")
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "in.tar")

	if err := os.WriteFile(src, imageTar(t), 0o600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "out")
	if err := extractTar(src, out, false); err != nil {
		t.Fatal(err)
	}

	st, err := os.Stat(filepath.Join(out, "usr/bin/tool"))
	if err != nil {
		t.Fatal(err)
	}

	if st.Mode()&os.ModeSetuid == 0 || st.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want setuid 0755", st.Mode())
	}

	if l, _ := os.Readlink(filepath.Join(out, "bin")); l != "usr/bin" {
		t.Fatalf("symlink = %q", l)
	}

	for name, bad := range map[string][]entry{
		"dotdot": {{name: "../escape", typ: tar.TypeReg, mode: 0o644, body: "x"}},
		"through a symlink": {
			{name: "etc", typ: tar.TypeSymlink, link: dir},
			{name: "etc/passwd", typ: tar.TypeReg, mode: 0o644, body: "x"},
		},
		"hard link out": {{name: "l", typ: tar.TypeLink, link: "../../x"}},
	} {
		p := filepath.Join(dir, "bad.tar")
		if err := os.WriteFile(p, mkTar(t, bad...), 0o600); err != nil {
			t.Fatal(err)
		}

		dst := filepath.Join(dir, "bad-"+strings.ReplaceAll(name, " ", "-"))

		err := extractTar(p, dst, false)
		if name == "dotdot" || name == "hard link out" {
			// Cleaned to stay inside root: "../escape" lands at <root>/escape, never above it, and
			// a hard link to "../../x" is a link to <root>/x (absent here, so an error) - either
			// way nothing outside the root is touched.
			_ = err

			if _, err := os.Stat(filepath.Join(dir, "escape")); err == nil {
				t.Fatalf("%s wrote outside the root", name)
			}

			continue
		}

		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("%s: err = %v", name, err)
		}

		if _, err := os.Stat(filepath.Join(dir, "passwd")); err == nil {
			t.Fatalf("%s wrote through the symlink", name)
		}
	}
}

func TestTakesTar(t *testing.T) {
	for v, want := range map[string]bool{
		"mke2fs 1.47.0 (5-Feb-2023)":  false, // Ubuntu 24.04
		"mke2fs 1.47.1 (20-May-2024)": true,
		"mke2fs 1.47.2 (1-Jan-2025)":  true,
		"mke2fs 1.46.5 (30-Dec-2021)": false,
		"mke2fs 1.48 (1-Jan-2026)":    true,
		"mke2fs 2.0.0":                true,
		"command not found":           false,
	} {
		if got := takesTar(v); got != want {
			t.Errorf("takesTar(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestMkfsAbsentIsARefusalNamingThePackage(t *testing.T) {
	err := Mkfs{}.Build(context.Background(), "/src", "/img", "x")
	if !errors.Is(err, ErrNoMkfs) || !strings.Contains(err.Error(), "e2fsprogs") {
		t.Fatalf("err = %v", err)
	}
}

func TestCloneFileIsSparseAndExact(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.ext4")

	// 256 MiB apparent, 3 MiB of data at the start, middle and end.
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}

	data := bytes.Repeat([]byte("sbx!"), 1<<18) // 1 MiB
	for _, off := range []int64{0, 100 << 20, 255 << 20} {
		if _, err := f.WriteAt(data, off); err != nil {
			t.Fatal(err)
		}
	}

	f.Close()

	dst := filepath.Join(dir, "dst.ext4")

	start := time.Now()

	how, err := CloneFile(src, dst)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("clone of a 256 MiB file holding 3 MiB: %s in %s", how, time.Since(start))

	a, _ := os.ReadFile(src)
	b, _ := os.ReadFile(dst)

	if !bytes.Equal(a, b) {
		t.Fatal("clone differs from source")
	}

	if runtime.GOOS != "linux" && how == CloneReflink {
		t.Fatalf("how = %s off linux", how)
	}

	if how != CloneReflink {
		if used, ok := allocated(dst); ok && used > 16<<20 {
			t.Fatalf("copy allocated %d bytes for 3 MiB of data; holes were written out", used)
		}
	}
}

func TestAgentDrive(t *testing.T) {
	dir := t.TempDir()
	agent := filepath.Join(dir, "sbx-linux")

	if err := os.WriteFile(agent, []byte("ELF"), 0o755); err != nil {
		t.Fatal(err)
	}

	ext := &fakeExt4{}
	dst := filepath.Join(dir, "agent.ext4")

	cfg := InitConfig{Argv: []string{"redis-server"}, Env: []string{"SECRET=1"}, RootDevice: GuestRootfsDevice}
	if err := BuildAgentDrive(context.Background(), ext, AgentDrive{Agent: agent, Config: cfg}, dst); err != nil {
		t.Fatal(err)
	}

	got := manifest(t, dst)
	for _, want := range []string{"sbx", "init.json", "dev", "proc", "sys", "newroot"} {
		if !slices.Contains(got, want) {
			t.Fatalf("%s missing from the agent drive: %v", want, got)
		}
	}

	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(dst); st.Mode().Perm() != 0o600 {
			t.Fatalf("agent drive mode %v; it holds the service's environment", st.Mode().Perm())
		}
	}
}

func TestComposeIsDockersRule(t *testing.T) {
	img, cmd := []string{"docker-entrypoint.sh"}, []string{"postgres"}

	for _, tc := range []struct {
		name        string
		entry, args []string
		want        []string
	}{
		{"image as is", nil, nil, []string{"docker-entrypoint.sh", "postgres"}},
		{"args replace cmd", nil, []string{"postgres", "-c", "fsync=off"}, []string{"docker-entrypoint.sh", "postgres", "-c", "fsync=off"}},
		{"entrypoint drops cmd", []string{"sh"}, nil, []string{"sh"}},
		{"entrypoint and args", []string{"sh", "-c"}, []string{"echo hi"}, []string{"sh", "-c", "echo hi"}},
	} {
		if got := Compose(img, cmd, tc.entry, tc.args); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}

	env := MergeEnv([]string{"PATH=/bin", "LANG=C"}, map[string]string{"LANG": "en", "X": "1"}, []string{"LANG", "X"})
	if !slices.Equal(env, []string{"PATH=/bin", "LANG=en", "X=1"}) {
		t.Errorf("env = %v", env)
	}
}
