//go:build unix

package execd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (s *Server) downloadFile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	if q.Get("path") == "" {
		writeError(w, http.StatusBadRequest, codeMissingQuery, "missing query parameter 'path': the file to download")
		return
	}

	abs, ok := resolveOrFail(w, q.Get("path"))
	if !ok {
		return
	}

	rawOffset, rawLimit := q.Get("offset"), q.Get("limit")
	lineMode := rawOffset != "" || rawLimit != ""
	rangeHeader := r.Header.Get("Range")

	if lineMode && rangeHeader != "" {
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			"offset/limit (lines) and a Range header (bytes) are mutually exclusive; send one")

		return
	}

	f, err := os.Open(abs)
	if err != nil {
		fileError(w, "download "+q.Get("path"), err)
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		fileError(w, "download "+q.Get("path"), err)
		return
	}

	if fi.IsDir() {
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			fmt.Sprintf("%s is a directory; list it with GET /directories/list", q.Get("path")))

		return
	}

	if lineMode {
		serveLines(w, f, rawOffset, rawLimit)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", contentDisposition(filepath.Base(abs)))
	h.Set("Accept-Ranges", "bytes")

	size := fi.Size()

	if rangeHeader != "" {
		start, length, ok := parseRange(rangeHeader, size)
		if !ok {
			h.Del("Content-Disposition")
			h.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			writeError(w, http.StatusRequestedRangeNotSatisfiable, codeRangeInvalid,
				fmt.Sprintf("range %q is not satisfiable for a %d-byte file", rangeHeader, size))

			return
		}

		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+length-1, size))
		h.Set("Content-Length", strconv.FormatInt(length, 10))
		w.WriteHeader(http.StatusPartialContent)

		if r.Method != http.MethodHead {
			_, _ = io.Copy(w, io.NewSectionReader(f, start, length))
		}

		return
	}

	h.Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// parseRange reads the first range of a "bytes=" header. Upstream serves only the first range
// of a multi-range request, and so does this; a multipart/byteranges body is something no
// OpenSandbox client asks for. Ranges starting past the end are skipped, and a header with no
// satisfiable range is 416.
func parseRange(h string, size int64) (start, length int64, ok bool) {
	spec, found := strings.CutPrefix(h, "bytes=")
	if !found {
		return 0, 0, false
	}

	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		a, b, found := strings.Cut(part, "-")
		if !found {
			return 0, 0, false
		}

		a, b = strings.TrimSpace(a), strings.TrimSpace(b)

		if a == "" {
			// A suffix: the last n bytes.
			n, err := strconv.ParseInt(b, 10, 64)
			if err != nil || n <= 0 {
				return 0, 0, false
			}

			n = min(n, size)
			if n == 0 {
				continue
			}

			return size - n, n, true
		}

		i, err := strconv.ParseInt(a, 10, 64)
		if err != nil || i < 0 {
			return 0, 0, false
		}

		if i >= size {
			continue
		}

		end := size - 1

		if b != "" {
			j, err := strconv.ParseInt(b, 10, 64)
			if err != nil || j < i {
				return 0, 0, false
			}

			end = min(j, size-1)
		}

		return i, end - i + 1, true
	}

	return 0, 0, false
}

// serveLines is the line mode: offset is 1-based, limit caps the count, lines are joined with
// "\n" and the last one gets no terminator - upstream's output byte for byte, which the e2e
// suite compares exactly ("line2\nline3").
func serveLines(w http.ResponseWriter, f *os.File, rawOffset, rawLimit string) {
	offset, limit := int64(1), int64(-1)

	if rawOffset != "" {
		n, err := strconv.ParseInt(rawOffset, 10, 64)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, fmt.Sprintf("invalid query parameter 'offset': %q; lines are numbered from 1", rawOffset))
			return
		}

		offset = n
	}

	if rawLimit != "" {
		n, err := strconv.ParseInt(rawLimit, 10, 64)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, fmt.Sprintf("invalid query parameter 'limit': %q; it must be at least 1", rawLimit))
			return
		}

		limit = n
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	br := bufio.NewReader(f)

	var num, written int64

	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			num++

			if num >= offset {
				if written > 0 {
					_, _ = w.Write([]byte{'\n'})
				}

				_, _ = w.Write(bytes.TrimRight(line, "\r\n"))
				written++

				if limit >= 0 && written >= limit {
					return
				}
			}
		}

		if err != nil {
			return
		}
	}
}

func contentDisposition(name string) string {
	for _, r := range name {
		if r > 127 || r == '"' || r == '\\' {
			enc := url.PathEscape(name)
			return `attachment; filename="` + enc + `"; filename*=UTF-8''` + enc
		}
	}

	return `attachment; filename="` + name + `"`
}

// uploadFiles reads the multipart body as a sequence of (metadata, file) pairs, the order every
// SDK sends them in, and writes each file as its part arrives. Upstream parses the whole form
// first, which buffers anything over 32 MiB to a temp file - on the same disk it is about to
// write the real file to. Streaming needs neither the memory nor the second copy.
func (s *Server) uploadFiles(w http.ResponseWriter, r *http.Request) {
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidFile, "expected a multipart/form-data body of metadata and file parts: "+err.Error())
		return
	}

	var (
		pending *uploadMeta
		pairs   int
	)

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			writeError(w, http.StatusBadRequest, codeInvalidFile, "read multipart body: "+err.Error())
			return
		}

		switch part.FormName() {
		case "metadata":
			if pending != nil {
				_ = part.Close()
				writeError(w, http.StatusBadRequest, codeInvalidContent,
					fmt.Sprintf("metadata for %s has no file part after it; send metadata then file for each upload", pending.Path))

				return
			}

			m, err := readUploadMeta(part)
			_ = part.Close()

			if err != nil {
				writeError(w, http.StatusBadRequest, codeInvalidMetadata, err.Error())
				return
			}

			pending = m

		case "file":
			if pending == nil {
				_ = part.Close()
				writeError(w, http.StatusBadRequest, codeInvalidMetadata,
					"file part without a metadata part before it; send metadata then file for each upload")

				return
			}

			status, code, err := writeUpload(pending, part)
			_ = part.Close()

			if err != nil {
				writeError(w, status, code, err.Error())
				return
			}

			pending = nil
			pairs++

		default:
			_ = part.Close()
		}
	}

	switch {
	case pending != nil:
		writeError(w, http.StatusBadRequest, codeInvalidContent, fmt.Sprintf("file is missing for %s", pending.Path))
	case pairs == 0:
		writeError(w, http.StatusBadRequest, codeInvalidMetadata, "metadata is missing; send a metadata part then a file part")
	default:
		w.WriteHeader(http.StatusOK)
	}
}

type uploadMeta struct {
	Path string `json:"path"`
	permission
}

func readUploadMeta(part *multipart.Part) (*uploadMeta, error) {
	data, err := io.ReadAll(io.LimitReader(part, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read metadata: %w", err)
	}

	var m uploadMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("invalid metadata %q: %w; expected {\"path\":...}", data, err)
	}

	if m.Path == "" {
		return nil, errors.New("metadata path is empty; name the file's destination in path")
	}

	if m.Mode != 0 {
		if _, err := parseMode(m.Mode); err != nil {
			return nil, err
		}
	}

	return &m, nil
}

func writeUpload(m *uploadMeta, src io.Reader) (int, string, error) {
	abs, err := absPath(m.Path)
	if err != nil {
		return http.StatusBadRequest, codeInvalidRequest, fmt.Errorf("invalid path: %w", err)
	}

	if err := mkdirAll(filepath.Dir(abs), permission{Owner: m.Owner, Group: m.Group}); err != nil {
		return http.StatusInternalServerError, codeRuntimeError, fmt.Errorf("create directory for %s: %w", m.Path, err)
	}

	// 0o777 before umask, as upstream: an uploaded script runs without a chmod first. A mode in
	// the metadata replaces it below.
	dst, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o777)
	if err != nil {
		return http.StatusInternalServerError, codeRuntimeError, fmt.Errorf("open %s for writing: %w", m.Path, err)
	}

	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return http.StatusInternalServerError, codeRuntimeError, fmt.Errorf("write %s: %w", m.Path, err)
	}

	if err := dst.Close(); err != nil {
		return http.StatusInternalServerError, codeRuntimeError, fmt.Errorf("write %s: %w", m.Path, err)
	}

	if err := applyPermission(abs, m.permission); err != nil {
		return http.StatusInternalServerError, codeRuntimeError, fmt.Errorf("set permissions on %s: %w", m.Path, err)
	}

	return 0, "", nil
}
