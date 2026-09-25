//go:build unix

package execd

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func q(kv ...string) string {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Add(kv[i], kv[i+1])
	}

	return "?" + v.Encode()
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFilesInfo(t *testing.T) {
	s := newTestServer(t, Options{})
	dir := mustEval(t, t.TempDir())
	f := filepath.Join(dir, "a.txt")
	writeFile(t, f, "hello")

	if err := os.Chmod(f, 0o640); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(f, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	code, _, body := s.do("GET", "/files/info"+q("path", f, "path", filepath.Join(dir, "link"), "path", dir), nil)
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}

	var got map[string]fileInfo
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}

	fi := got[f]
	if fi.Path != f || fi.Size != 5 || fi.Type != "file" || fi.Mode != 640 || fi.Owner == "" || fi.Group == "" ||
		fi.ModifiedAt.IsZero() || fi.CreatedAt.IsZero() {
		t.Errorf("file info %+v", fi)
	}

	if got[filepath.Join(dir, "link")].Type != "symlink" {
		t.Errorf("a symlink is reported as itself: %+v", got[filepath.Join(dir, "link")])
	}

	if got[dir].Type != "directory" {
		t.Errorf("directory: %+v", got[dir])
	}

	// The key is the path as asked; path inside is absolute. A relative path must not panic.
	code, _, body = s.do("GET", "/files/info"+q("path", filepath.Join(dir, "sub", "..", "a.txt")), nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"path":"`+f+`"`) {
		t.Errorf("a path with .. resolves: %d %s", code, body)
	}

	code, _, body = s.do("GET", "/files/info"+q("path", filepath.Join(dir, "missing")), nil)
	wantError(t, code, body, http.StatusNotFound, codeFileNotFound)

	code, _, body = s.do("GET", "/files/info"+q("path", "$SBX_EXECD_UNSET/x"), nil)
	wantError(t, code, body, http.StatusBadRequest, codeInvalidRequest)

	code, _, body = s.do("GET", "/files/info", nil)
	if code != http.StatusOK || string(body) != "{}" {
		t.Errorf("no paths: %d %s", code, body)
	}
}

func TestRemoveFiles(t *testing.T) {
	s := newTestServer(t, Options{})
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	writeFile(t, a, "x")
	writeFile(t, b, "x")

	code, _, body := s.do("DELETE", "/files"+q("path", a, "path", b, "path", filepath.Join(dir, "never-existed")), nil)
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}

	for _, p := range []string{a, b} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists", p)
		}
	}

	code, _, body = s.do("DELETE", "/files"+q("path", dir), nil)
	wantError(t, code, body, http.StatusBadRequest, codeInvalidRequest)

	if _, err := os.Stat(dir); err != nil {
		t.Fatal("DELETE /files removed a directory")
	}
}

func TestChmodFiles(t *testing.T) {
	s := newTestServer(t, Options{})
	f := filepath.Join(t.TempDir(), "perm")
	writeFile(t, f, "x")

	code, _, body := s.do("POST", "/files/permissions", map[string]any{f: map[string]any{"mode": 604}})
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}

	if fi, _ := os.Stat(f); fi.Mode().Perm() != 0o604 {
		t.Fatalf("mode %o, want 604 - the number is octal digits", fi.Mode().Perm())
	}

	code, _, body = s.do("POST", "/files/permissions", map[string]any{f: map[string]any{"mode": 999}})
	wantError(t, code, body, http.StatusBadRequest, codeInvalidRequest)

	code, _, body = s.do("POST", "/files/permissions", map[string]any{f + ".missing": map[string]any{"mode": 644}})
	wantError(t, code, body, http.StatusNotFound, codeFileNotFound)

	code, _, body = s.do("POST", "/files/permissions", map[string]any{f: map[string]any{"owner": "sbx-execd-no-such-user"}})
	wantError(t, code, body, http.StatusInternalServerError, codeRuntimeError)

	code, _, body = s.do("POST", "/files/permissions", `[1,2]`)
	wantError(t, code, body, http.StatusBadRequest, codeInvalidRequest)
}

func TestMoveFiles(t *testing.T) {
	s := newTestServer(t, Options{})
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	writeFile(t, src, "move-me")

	dst := filepath.Join(dir, "new", "dir", "dst.txt")

	code, _, body := s.do("POST", "/files/mv", []map[string]string{{"src": src, "dest": dst}})
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}

	if b, err := os.ReadFile(dst); err != nil || string(b) != "move-me" {
		t.Fatalf("destination: %q %v", b, err)
	}

	code, _, body = s.do("POST", "/files/mv", []map[string]string{{"src": src, "dest": dst + "2"}})
	wantError(t, code, body, http.StatusNotFound, codeFileNotFound)

	other := filepath.Join(dir, "other")
	writeFile(t, other, "keep")

	code, _, body = s.do("POST", "/files/mv", []map[string]string{{"src": dst, "dest": other}})
	wantError(t, code, body, http.StatusBadRequest, codeInvalidRequest)

	if b, _ := os.ReadFile(other); string(b) != "keep" {
		t.Fatal("a move overwrote an existing destination")
	}

	code, _, body = s.do("POST", "/files/mv", []map[string]string{{"src": dst}})
	wantError(t, code, body, http.StatusBadRequest, codeInvalidRequest)
}

func TestSearchFiles(t *testing.T) {
	s := newTestServer(t, Options{})
	dir := mustEval(t, t.TempDir())

	for _, p := range []string{"a.conf", "b.txt", "sub/c.conf", "sub/deeper/d.conf"} {
		writeFile(t, filepath.Join(dir, p), "x")
	}

	search := func(pattern string) []string {
		t.Helper()

		code, _, body := s.do("GET", "/files/search"+q("path", dir, "pattern", pattern), nil)
		if code != http.StatusOK {
			t.Fatalf("%s: %d %s", pattern, code, body)
		}

		var got []fileInfo
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}

		var names []string
		for _, f := range got {
			rel, _ := filepath.Rel(dir, f.Path)
			names = append(names, rel)
		}

		sort.Strings(names)

		return names
	}

	cases := []struct {
		pattern string
		want    string
	}{
		{"*.conf", "a.conf,sub/c.conf,sub/deeper/d.conf"},
		{"**", "a.conf,b.txt,sub/c.conf,sub/deeper/d.conf"},
		{"", "a.conf,b.txt,sub/c.conf,sub/deeper/d.conf"},
		{"**/*.txt", "b.txt"},
		{"?.txt", "b.txt"},
		{"nothing*", ""},
	}

	for _, c := range cases {
		if got := strings.Join(search(c.pattern), ","); got != c.want {
			t.Errorf("pattern %q: got %s, want %s", c.pattern, got, c.want)
		}
	}

	code, _, body := s.do("GET", "/files/search"+q("path", dir, "pattern", "[unclosed"), nil)
	wantError(t, code, body, http.StatusBadRequest, codeInvalidRequest)

	code, _, body = s.do("GET", "/files/search"+q("path", filepath.Join(dir, "missing")), nil)
	wantError(t, code, body, http.StatusNotFound, codeFileNotFound)

	code, _, body = s.do("GET", "/files/search", nil)
	wantError(t, code, body, http.StatusBadRequest, codeMissingQuery)
}

func TestReplaceContent(t *testing.T) {
	s := newTestServer(t, Options{})
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	writeFile(t, a, "hello localhost localhost\n")
	writeFile(t, b, "nothing here\n")

	if err := os.Chmod(a, 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, body := s.do("POST", "/files/replace?verbose=true", map[string]any{
		a: map[string]string{"old": "localhost", "new": "example.com"},
		b: map[string]string{"old": "absent", "new": "x"},
	})
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}

	var counts map[string]struct {
		ReplacedCount *int `json:"replacedCount"`
	}

	if err := json.Unmarshal(body, &counts); err != nil {
		t.Fatal(err)
	}

	if counts[a].ReplacedCount == nil || *counts[a].ReplacedCount != 2 || counts[b].ReplacedCount == nil || *counts[b].ReplacedCount != 0 {
		t.Fatalf("counts %s", body)
	}

	if got, _ := os.ReadFile(a); string(got) != "hello example.com example.com\n" {
		t.Fatalf("content %q", got)
	}

	if fi, _ := os.Stat(a); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode changed to %o", fi.Mode().Perm())
	}

	code, _, body = s.do("POST", "/files/replace", map[string]any{a: map[string]string{"old": "example.com", "new": "done"}})
	if code != http.StatusOK || len(body) != 0 {
		t.Fatalf("without verbose the body is empty: %d %q", code, body)
	}

	code, _, body = s.do("POST", "/files/replace", map[string]any{a: map[string]string{"old": "", "new": "x"}})
	wantError(t, code, body, http.StatusBadRequest, codeInvalidRequest)

	code, _, body = s.do("POST", "/files/replace", map[string]any{a + "missing": map[string]string{"old": "a", "new": "x"}})
	wantError(t, code, body, http.StatusNotFound, codeFileNotFound)
}

func TestDirectories(t *testing.T) {
	s := newTestServer(t, Options{})
	root := mustEval(t, t.TempDir())
	made := filepath.Join(root, "p", "q")

	code, _, body := s.do("POST", "/directories", map[string]any{made: map[string]any{"mode": 750}})
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}

	if fi, err := os.Stat(made); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o750 {
		t.Fatalf("made %v %v", fi, err)
	}

	// An existing directory keeps its mode: the mode is for creation.
	code, _, _ = s.do("POST", "/directories", map[string]any{made: map[string]any{"mode": 700}})
	if fi, _ := os.Stat(made); code != http.StatusOK || fi.Mode().Perm() != 0o750 {
		t.Fatalf("re-creating changed the mode: %d %o", code, fi.Mode().Perm())
	}

	code, _, body = s.do("POST", "/directories", map[string]any{made: map[string]any{"mode": 8}})
	wantError(t, code, body, http.StatusBadRequest, codeInvalidRequest)

	writeFile(t, filepath.Join(root, "p", "b.txt"), "x")
	writeFile(t, filepath.Join(root, "p", "a.txt"), "x")
	writeFile(t, filepath.Join(made, "deep.txt"), "x")

	if err := os.Symlink(filepath.Join(root, "p"), filepath.Join(root, "p", "loop")); err != nil {
		t.Fatal(err)
	}

	list := func(depth string) []string {
		t.Helper()

		path := "/directories/list" + q("path", filepath.Join(root, "p"))
		if depth != "" {
			path += "&depth=" + depth
		}

		code, _, body := s.do("GET", path, nil)
		if code != http.StatusOK {
			t.Fatalf("list depth %s: %d %s", depth, code, body)
		}

		var got []fileInfo
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}

		var out []string
		for _, f := range got {
			rel, _ := filepath.Rel(filepath.Join(root, "p"), f.Path)
			out = append(out, rel+":"+f.Type)
		}

		return out
	}

	if got := strings.Join(list(""), ","); got != "a.txt:file,b.txt:file,loop:symlink,q:directory" {
		t.Errorf("depth 1 (default): %s", got)
	}

	if got := strings.Join(list("2"), ","); got != "a.txt:file,b.txt:file,loop:symlink,q:directory,q/deep.txt:file" {
		t.Errorf("depth 2, lexical, children after their parent, links not followed: %s", got)
	}

	if got := list("0"); len(got) != 0 {
		t.Errorf("depth 0: %v", got)
	}

	for _, c := range []struct {
		path, depth string
		status      int
		code        string
	}{
		{filepath.Join(root, "p", "loop"), "", http.StatusBadRequest, codeInvalidRequest},
		{filepath.Join(root, "p", "a.txt"), "", http.StatusBadRequest, codeInvalidRequest},
		{filepath.Join(root, "nope"), "", http.StatusNotFound, codeFileNotFound},
		{root, "-1", http.StatusBadRequest, codeInvalidRequest},
		{root, "x", http.StatusBadRequest, codeInvalidRequest},
		{"", "", http.StatusBadRequest, codeMissingQuery},
	} {
		path := "/directories/list" + q("path", c.path, "depth", c.depth)
		code, _, body := s.do("GET", path, nil)
		wantError(t, code, body, c.status, c.code)
	}

	code, _, body = s.do("DELETE", "/directories"+q("path", filepath.Join(root, "p"), "path", filepath.Join(root, "absent")), nil)
	if code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}

	if _, err := os.Stat(filepath.Join(root, "p")); !os.IsNotExist(err) {
		t.Fatal("directory still there")
	}

	code, _, body = s.do("DELETE", "/directories"+q("path", "/"), nil)
	wantError(t, code, body, http.StatusBadRequest, codeInvalidRequest)

	code, _, body = s.do("DELETE", "/directories"+q("path", "/tmp/../"), nil)
	wantError(t, code, body, http.StatusBadRequest, codeInvalidRequest)
}
