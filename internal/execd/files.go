//go:build unix

package execd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// There is no chroot here, on purpose: upstream has none either, and a sandbox's filesystem is
// the sandbox's. What is checked is that every path resolves and every failure maps to the
// spec's status - never that a path stays inside some root.

// fileInfo is FileInfo. mode is the permission bits written as an octal number read in
// decimal (0o755 is 755), which is how the spec and every SDK carry it.
type fileInfo struct {
	Path       string    `json:"path"`
	Type       string    `json:"type"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
	CreatedAt  time.Time `json:"created_at"`
	Owner      string    `json:"owner"`
	Group      string    `json:"group"`
	Mode       int       `json:"mode"`
}

// permission is Permission / the ownership half of FileMetadata.
type permission struct {
	Owner string `json:"owner"`
	Group string `json:"group"`
	Mode  int    `json:"mode"`
}

// names caches uid and gid lookups for one request: a listing of a thousand files owned by
// root should read /etc/passwd once, not a thousand times.
type names struct {
	users, groups map[uint32]string
}

func newNames() *names {
	return &names{users: map[uint32]string{}, groups: map[uint32]string{}}
}

func (n *names) user(uid uint32) string {
	if s, ok := n.users[uid]; ok {
		return s
	}

	s := strconv.FormatUint(uint64(uid), 10)
	if u, err := user.LookupId(s); err == nil {
		s = u.Username
	}

	n.users[uid] = s

	return s
}

func (n *names) group(gid uint32) string {
	if s, ok := n.groups[gid]; ok {
		return s
	}

	s := strconv.FormatUint(uint64(gid), 10)
	if g, err := user.LookupGroupId(s); err == nil {
		s = g.Name
	}

	n.groups[gid] = s

	return s
}

func (n *names) info(path string, fi os.FileInfo) fileInfo {
	out := fileInfo{
		Path:       path,
		Type:       fileType(fi),
		Size:       fi.Size(),
		ModifiedAt: fi.ModTime(),
		CreatedAt:  createdAt(fi),
		Mode:       octalDigits(fi.Mode().Perm()),
	}

	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		out.Owner, out.Group = n.user(st.Uid), n.group(st.Gid)
	}

	return out
}

func fileType(fi os.FileInfo) string {
	switch m := fi.Mode(); {
	case m&os.ModeSymlink != 0:
		return "symlink"
	case m.IsDir():
		return "directory"
	case m.IsRegular():
		return "file"
	default:
		return "other"
	}
}

// octalDigits turns 0o755 into 755.
func octalDigits(m os.FileMode) int {
	n, _ := strconv.Atoi(strconv.FormatUint(uint64(m), 8))
	return n
}

// parseMode turns 755 into 0o755, refusing digits that are not octal.
func parseMode(n int) (os.FileMode, error) {
	m, err := strconv.ParseUint(strconv.Itoa(n), 8, 32)
	if err != nil || m > 0o7777 {
		return 0, fmt.Errorf("mode %d is not a permission in octal digits; write 0o644 as 644", n)
	}

	return os.FileMode(m), nil
}

// fileError maps a filesystem error to the spec's statuses: missing is 404, anything else 500.
func fileError(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, codeFileNotFound, fmt.Sprintf("%s: file not found: %v", what, err))
		return
	}

	writeError(w, http.StatusInternalServerError, codeRuntimeError, fmt.Sprintf("%s: %v", what, err))
}

// resolveOrFail resolves a request path, answering 400 itself when it cannot.
func resolveOrFail(w http.ResponseWriter, p string) (string, bool) {
	abs, err := absPath(p)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid path: "+err.Error())
		return "", false
	}

	return abs, true
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			"error parsing request, expected a JSON body as specs/execd-api.yaml describes: "+err.Error())

		return false
	}

	return true
}

func (s *Server) filesInfo(w http.ResponseWriter, r *http.Request) {
	paths := r.URL.Query()["path"]
	out := make(map[string]fileInfo, len(paths))
	n := newNames()

	for _, p := range paths {
		abs, ok := resolveOrFail(w, p)
		if !ok {
			return
		}

		// Lstat: a symlink is reported as the link, as upstream does, not as its target.
		fi, err := os.Lstat(abs)
		if err != nil {
			fileError(w, "get info for "+p, err)
			return
		}

		out[p] = n.info(abs, fi)
	}

	writeJSON(w, http.StatusOK, out)
}

// removeFiles deletes files, never directories. A path that is already gone is success, so a
// retried delete does not fail.
func (s *Server) removeFiles(w http.ResponseWriter, r *http.Request) {
	for _, p := range r.URL.Query()["path"] {
		abs, ok := resolveOrFail(w, p)
		if !ok {
			return
		}

		fi, err := os.Lstat(abs)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}

		if err == nil && fi.IsDir() {
			writeError(w, http.StatusBadRequest, codeInvalidRequest,
				fmt.Sprintf("%s is a directory; delete directories with DELETE /directories", p))

			return
		}

		if err == nil {
			err = os.Remove(abs)
		}

		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusInternalServerError, codeRuntimeError, fmt.Sprintf("remove %s: %v", p, err))
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

// applyPermission sets mode (when non-zero) and ownership (when named) on one path.
func applyPermission(abs string, p permission) error {
	if p.Mode != 0 {
		m, err := parseMode(p.Mode)
		if err != nil {
			return err
		}

		if err := os.Chmod(abs, m); err != nil {
			return err
		}
	}

	return chown(abs, p.Owner, p.Group)
}

func chown(abs, owner, group string) error {
	if owner == "" && group == "" {
		return nil
	}

	uid, gid := -1, -1

	if owner != "" {
		u, err := user.Lookup(owner)
		if err != nil {
			return fmt.Errorf("no user %q in this sandbox: %w", owner, err)
		}

		uid, _ = strconv.Atoi(u.Uid)
	}

	if group != "" {
		g, err := user.LookupGroup(group)
		if err != nil {
			return fmt.Errorf("no group %q in this sandbox: %w", group, err)
		}

		gid, _ = strconv.Atoi(g.Gid)
	}

	return os.Chown(abs, uid, gid)
}

func (s *Server) chmodFiles(w http.ResponseWriter, r *http.Request) {
	var req map[string]permission
	if !decodeBody(w, r, &req) {
		return
	}

	for p, perm := range req {
		if _, err := parseMode(perm.Mode); perm.Mode != 0 && err != nil {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, fmt.Sprintf("%s: %v", p, err))
			return
		}
	}

	for p, perm := range req {
		abs, ok := resolveOrFail(w, p)
		if !ok {
			return
		}

		if err := applyPermission(abs, perm); err != nil {
			fileError(w, "change permissions of "+p, err)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

func (s *Server) moveFiles(w http.ResponseWriter, r *http.Request) {
	var req []struct {
		Src  string `json:"src"`
		Dest string `json:"dest"`
	}

	if !decodeBody(w, r, &req) {
		return
	}

	for _, it := range req {
		if it.Src == "" || it.Dest == "" {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "every item needs both src and dest")
			return
		}

		src, ok := resolveOrFail(w, it.Src)
		if !ok {
			return
		}

		dst, ok := resolveOrFail(w, it.Dest)
		if !ok {
			return
		}

		if _, err := os.Lstat(src); err != nil {
			fileError(w, "move "+it.Src, err)
			return
		}

		// Refused rather than overwritten, as upstream: rename(2) would silently replace a
		// file, and a move that destroys what was at the destination is not what anyone asked
		// for.
		if _, err := os.Lstat(dst); err == nil {
			writeError(w, http.StatusBadRequest, codeInvalidRequest,
				fmt.Sprintf("destination %s already exists; delete it first to replace it", it.Dest))

			return
		}

		// Upstream creates the destination's parent, whatever the spec's "must exist" says.
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			fileError(w, "create destination directory for "+it.Dest, err)
			return
		}

		if err := os.Rename(src, dst); err != nil {
			fileError(w, fmt.Sprintf("move %s to %s", it.Src, it.Dest), err)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

// searchFiles walks path for files (not directories) whose name matches pattern. Upstream
// matches the pattern against the base name only, so "*.conf" finds /etc/a/b.conf.
func (s *Server) searchFiles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	if q.Get("path") == "" {
		writeError(w, http.StatusBadRequest, codeMissingQuery, "missing query parameter 'path': the directory to search")
		return
	}

	root, ok := resolveOrFail(w, q.Get("path"))
	if !ok {
		return
	}

	if _, err := os.Stat(root); err != nil {
		fileError(w, "search "+q.Get("path"), err)
		return
	}

	pattern := q.Get("pattern")
	if pattern == "" {
		pattern = "**"
	}

	if err := validGlob(pattern); err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}

	n := newNames()
	out := make([]fileInfo, 0, 16)

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that vanished or cannot be read is skipped, not fatal: upstream fails
			// the whole search, which makes "search /" useless on any real system.
			if d != nil && d.IsDir() && p != root {
				return fs.SkipDir
			}

			if p == root {
				return err
			}

			return nil
		}

		if d.IsDir() || !globMatch(pattern, d.Name()) {
			return nil
		}

		fi, err := d.Info()
		if err != nil {
			return nil
		}

		out = append(out, n.info(p, fi))

		return nil
	})
	if err != nil {
		fileError(w, "search "+q.Get("path"), err)
		return
	}

	writeJSON(w, http.StatusOK, out)
}

func (s *Server) replaceContent(w http.ResponseWriter, r *http.Request) {
	verbose := r.URL.Query().Get("verbose") == "true"

	var req map[string]struct {
		Old string `json:"old"`
		New string `json:"new"`
	}

	if !decodeBody(w, r, &req) {
		return
	}

	// Validated before anything is written, so a bad item cannot leave the batch half done.
	for p, it := range req {
		if it.Old == "" {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, fmt.Sprintf("%s: old must not be empty", p))
			return
		}
	}

	results := map[string]map[string]int{}

	for p, it := range req {
		abs, ok := resolveOrFail(w, p)
		if !ok {
			return
		}

		fi, err := os.Stat(abs)
		if err != nil {
			fileError(w, "replace in "+p, err)
			return
		}

		data, err := os.ReadFile(abs)
		if err != nil {
			fileError(w, "read "+p, err)
			return
		}

		n := strings.Count(string(data), it.Old)

		if n > 0 {
			// WriteFile keeps the existing mode (it only applies perm when creating).
			if err := os.WriteFile(abs, []byte(strings.ReplaceAll(string(data), it.Old, it.New)), fi.Mode().Perm()); err != nil {
				fileError(w, "write "+p, err)
				return
			}
		}

		results[p] = map[string]int{"replacedCount": n}
	}

	if verbose {
		writeJSON(w, http.StatusOK, results)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (s *Server) listDirectory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	if q.Get("path") == "" {
		writeError(w, http.StatusBadRequest, codeMissingQuery, "missing query parameter 'path': the directory to list")
		return
	}

	depth := 1

	if raw := q.Get("depth"); raw != "" {
		d, err := strconv.Atoi(raw)
		if err != nil || d < 0 {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, fmt.Sprintf("invalid query parameter 'depth': %q; use 1 for immediate children", raw))
			return
		}

		depth = d
	}

	root, ok := resolveOrFail(w, q.Get("path"))
	if !ok {
		return
	}

	fi, err := os.Lstat(root)
	if err != nil {
		fileError(w, "list "+q.Get("path"), err)
		return
	}

	if fi.Mode()&os.ModeSymlink != 0 {
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			fmt.Sprintf("%s is a symbolic link, which listings never follow; pass the real directory", q.Get("path")))

		return
	}

	if !fi.IsDir() {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, fmt.Sprintf("%s is not a directory", q.Get("path")))
		return
	}

	n := newNames()
	out := make([]fileInfo, 0, 16)

	var walk func(dir string, level int) error

	walk = func(dir string, level int) error {
		entries, err := os.ReadDir(dir) // sorted by name, which the spec requires
		if err != nil {
			return err
		}

		for _, e := range entries {
			p := filepath.Join(dir, e.Name())

			info, err := e.Info()
			if err != nil {
				continue // removed between ReadDir and now
			}

			out = append(out, n.info(p, info))

			// e.IsDir is false for a symlink to a directory, so links are never descended.
			if e.IsDir() && level+1 < depth {
				if err := walk(p, level+1); err != nil {
					return err
				}
			}
		}

		return nil
	}

	if depth > 0 {
		if err := walk(root, 0); err != nil {
			fileError(w, "list "+q.Get("path"), err)
			return
		}
	}

	writeJSON(w, http.StatusOK, out)
}

// makeDirs is mkdir -p per path. Mode applies to the directory named when it is newly created,
// and ownership to every directory the call created - upstream's MakeDir.
func (s *Server) makeDirs(w http.ResponseWriter, r *http.Request) {
	var req map[string]permission
	if !decodeBody(w, r, &req) {
		return
	}

	for p, perm := range req {
		if _, err := parseMode(perm.Mode); perm.Mode != 0 && err != nil {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, fmt.Sprintf("%s: %v", p, err))
			return
		}
	}

	for p, perm := range req {
		abs, ok := resolveOrFail(w, p)
		if !ok {
			return
		}

		if err := mkdirAll(abs, perm); err != nil {
			fileError(w, "create directory "+p, err)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

func mkdirAll(abs string, perm permission) error {
	_, statErr := os.Stat(abs)
	existed := statErr == nil

	// The directories this call will create, outermost first, for the ownership pass.
	var created []string

	for cur := abs; ; {
		if _, err := os.Stat(cur); err == nil {
			break
		}

		created = append([]string{cur}, created...)

		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}

		cur = parent
	}

	if err := os.MkdirAll(abs, 0o777); err != nil {
		return err
	}

	for _, d := range created {
		if err := chown(d, perm.Owner, perm.Group); err != nil {
			return err
		}
	}

	if !existed && perm.Mode != 0 {
		m, err := parseMode(perm.Mode)
		if err != nil {
			return err
		}

		return os.Chmod(abs, m)
	}

	return nil
}

// removeDirs is rm -rf per path. The root is refused: it would take execd, the shell and the
// sandbox's own binaries with it, and no request means that.
func (s *Server) removeDirs(w http.ResponseWriter, r *http.Request) {
	for _, p := range r.URL.Query()["path"] {
		abs, ok := resolveOrFail(w, p)
		if !ok {
			return
		}

		if abs == "/" {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "refusing to delete /; name the directories to delete")
			return
		}

		if err := os.RemoveAll(abs); err != nil {
			writeError(w, http.StatusInternalServerError, codeRuntimeError, fmt.Sprintf("remove directory %s: %v", p, err))
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}
