//go:build unix

package execd

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type uploadPart struct {
	meta    any // nil: no metadata part
	content *string
}

// uploadBody builds the multipart body the way the Go SDK's newUploadFilesRequest does: per file
// a "metadata" part carrying a filename and application/json, then a "file" part.
func uploadBody(t *testing.T, parts []uploadPart) (string, *bytes.Buffer) {
	t.Helper()

	var buf bytes.Buffer

	mw := multipart.NewWriter(&buf)

	for _, p := range parts {
		if p.meta != nil {
			h := make(textproto.MIMEHeader)
			h.Set("Content-Disposition", `form-data; name="metadata"; filename="metadata"`)
			h.Set("Content-Type", "application/json")

			w, err := mw.CreatePart(h)
			if err != nil {
				t.Fatal(err)
			}

			var data []byte
			if s, ok := p.meta.(string); ok {
				data = []byte(s)
			} else {
				data, _ = json.Marshal(p.meta)
			}

			_, _ = w.Write(data)
		}

		if p.content != nil {
			w, err := mw.CreateFormFile("file", "file")
			if err != nil {
				t.Fatal(err)
			}

			_, _ = w.Write([]byte(*p.content))
		}
	}

	_ = mw.Close()

	return mw.FormDataContentType(), &buf
}

func (s *testServer) upload(ct string, body io.Reader) (int, []byte) {
	s.t.Helper()

	resp, err := http.Post(s.ts.URL+"/files/upload", ct, body)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)

	return resp.StatusCode, data
}

func ptr(s string) *string { return &s }

func TestUpload(t *testing.T) {
	s := newTestServer(t, Options{})
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "new", "sub", "b.sh")

	ct, body := uploadBody(t, []uploadPart{
		{meta: map[string]any{"path": a}, content: ptr("go-e2e-upload-download")},
		{meta: map[string]any{"path": b, "mode": 700}, content: ptr("#!/bin/sh\necho hi\n")},
	})

	if code, data := s.upload(ct, body); code != http.StatusOK {
		t.Fatalf("%d %s", code, data)
	}

	if got, _ := os.ReadFile(a); string(got) != "go-e2e-upload-download" {
		t.Fatalf("a = %q", got)
	}

	if fi, err := os.Stat(b); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("b: %v %v", fi, err)
	}

	// Overwrite truncates.
	ct, body = uploadBody(t, []uploadPart{{meta: map[string]any{"path": a}, content: ptr("short")}})
	if code, data := s.upload(ct, body); code != http.StatusOK {
		t.Fatalf("%d %s", code, data)
	}

	if got, _ := os.ReadFile(a); string(got) != "short" {
		t.Fatalf("overwrite left %q", got)
	}
}

func TestUploadErrors(t *testing.T) {
	s := newTestServer(t, Options{})
	dir := t.TempDir()

	cases := []struct {
		name  string
		parts []uploadPart
		code  string
	}{
		{"no parts", nil, codeInvalidMetadata},
		{"file without metadata", []uploadPart{{content: ptr("x")}}, codeInvalidMetadata},
		{"metadata without file", []uploadPart{{meta: map[string]any{"path": filepath.Join(dir, "x")}}}, codeInvalidContent},
		{"metadata not json", []uploadPart{{meta: "{nope", content: ptr("x")}}, codeInvalidMetadata},
		{"metadata without path", []uploadPart{{meta: map[string]any{"mode": 644}, content: ptr("x")}}, codeInvalidMetadata},
		{"bad mode", []uploadPart{{meta: map[string]any{"path": filepath.Join(dir, "y"), "mode": 9}, content: ptr("x")}}, codeInvalidMetadata},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ct, body := uploadBody(t, c.parts)
			code, data := s.upload(ct, body)
			wantError(t, code, data, http.StatusBadRequest, c.code)
		})
	}

	code, data := s.upload("application/json", strings.NewReader("{}"))
	wantError(t, code, data, http.StatusBadRequest, codeInvalidFile)
}

func TestDownload(t *testing.T) {
	s := newTestServer(t, Options{})
	dir := t.TempDir()
	f := filepath.Join(dir, "data.bin")
	writeFile(t, f, "0123456789")

	code, h, body := s.do("GET", "/files/download"+q("path", f), nil)
	if code != http.StatusOK || string(body) != "0123456789" || h.Get("Content-Length") != "10" ||
		h.Get("Content-Type") != "application/octet-stream" || h.Get("Content-Disposition") != `attachment; filename="data.bin"` {
		t.Fatalf("full: %d %v %q", code, h, body)
	}

	ranges := []struct {
		header, body, contentRange string
	}{
		{"bytes=0-3", "0123", "bytes 0-3/10"},
		{"bytes=7-", "789", "bytes 7-9/10"},
		{"bytes=-2", "89", "bytes 8-9/10"},
		{"bytes=5-100", "56789", "bytes 5-9/10"},
		{"bytes=2-3,5-6", "23", "bytes 2-3/10"},
	}

	for _, r := range ranges {
		code, h, body := s.do("GET", "/files/download"+q("path", f), nil, "Range", r.header)
		if code != http.StatusPartialContent || string(body) != r.body || h.Get("Content-Range") != r.contentRange {
			t.Errorf("%s: %d %q %q", r.header, code, body, h.Get("Content-Range"))
		}
	}

	for _, bad := range []string{"bytes=20-30", "bytes=5-2", "lines=1-2", "bytes=x-"} {
		code, _, body := s.do("GET", "/files/download"+q("path", f), nil, "Range", bad)
		wantError(t, code, body, http.StatusRequestedRangeNotSatisfiable, codeRangeInvalid)
	}

	code, _, body = s.do("GET", "/files/download"+q("path", f, "offset", "1"), nil, "Range", "bytes=0-1")
	wantError(t, code, body, http.StatusBadRequest, codeInvalidRequest)

	code, _, body = s.do("GET", "/files/download"+q("path", filepath.Join(dir, "missing")), nil)
	wantError(t, code, body, http.StatusNotFound, codeFileNotFound)

	code, _, body = s.do("GET", "/files/download"+q("path", dir), nil)
	wantError(t, code, body, http.StatusBadRequest, codeInvalidRequest)

	code, _, body = s.do("GET", "/files/download", nil)
	wantError(t, code, body, http.StatusBadRequest, codeMissingQuery)
}

// TestDownloadLines uses the exact expectations of upstream's filesystem e2e test.
func TestDownloadLines(t *testing.T) {
	s := newTestServer(t, Options{})
	f := filepath.Join(t.TempDir(), "lines.txt")
	writeFile(t, f, "line1\nline2\nline3\nline4\nline5")

	crlf := filepath.Join(t.TempDir(), "crlf.txt")
	writeFile(t, crlf, "a\r\nb\r\nc\r\n")

	cases := []struct {
		file, offset, limit, want string
	}{
		{f, "2", "2", "line2\nline3"},
		{f, "4", "", "line4\nline5"},
		{f, "", "2", "line1\nline2"},
		{f, "9", "", ""},
		{crlf, "1", "", "a\nb\nc"},
	}

	for _, c := range cases {
		path := "/files/download" + q("path", c.file, "offset", c.offset, "limit", c.limit)
		code, h, body := s.do("GET", path, nil)

		if code != http.StatusOK || string(body) != c.want || !strings.HasPrefix(h.Get("Content-Type"), "text/plain") {
			t.Errorf("offset=%s limit=%s: %d %q (%s), want %q", c.offset, c.limit, code, body, h.Get("Content-Type"), c.want)
		}
	}

	for _, bad := range [][2]string{{"0", ""}, {"", "0"}, {"x", ""}, {"", "-1"}} {
		code, _, body := s.do("GET", "/files/download"+q("path", f, "offset", bad[0], "limit", bad[1]), nil)
		if bad[0] == "" && bad[1] == "" {
			continue
		}

		wantError(t, code, body, http.StatusBadRequest, codeInvalidRequest)
	}
}
