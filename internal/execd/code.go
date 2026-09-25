//go:build unix

package execd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/jupyter"
)

// The /code routes: execd-api.yaml's CodeInterpreting tag, run through internal/jupyter against
// the Jupyter Server the image provides. An image without one gets 501 with the engine's reason,
// which names the variable to set - never a hang and never a 404.
//
// Languages execd runs itself are not Jupyter's business: a POST /code with no language (or
// "command") is a foreground command, exactly as upstream treats it, and needs no Jupyter.

type codeContextRequest struct {
	ID       string `json:"id,omitempty"`
	Language string `json:"language,omitempty"`
	Cwd      string `json:"cwd,omitempty"`
}

// codeContextResponse is CodeContext. cwd is echoed because upstream echoes the request it was
// given.
type codeContextResponse struct {
	ID       string `json:"id"`
	Language string `json:"language"`
	Cwd      string `json:"cwd,omitempty"`
}

type runCodeRequest struct {
	Context codeContextRequest `json:"context"`
	Code    string             `json:"code"`
}

// codeReady answers 501 when there is no Jupyter to talk to. It reports whether the caller may
// go on.
func (s *Server) codeReady(w http.ResponseWriter, r *http.Request) bool {
	if err := s.code.Available(r.Context()); err != nil {
		writeError(w, http.StatusNotImplemented, codeNotSupported, err.Error())
		return false
	}

	return true
}

// codeError maps an engine error to a status and error code. Anything unrecognised is a 500,
// upstream's answer for every code-interpreter failure.
func codeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, jupyter.ErrUnsupportedLanguage):
		writeError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
	case errors.Is(err, jupyter.ErrContextNotFound):
		writeError(w, http.StatusNotFound, codeContextNotFound,
			err.Error()+"; ids come from POST /code/context or GET /code/contexts")
	case errors.Is(err, jupyter.ErrNotConfigured):
		writeError(w, http.StatusNotImplemented, codeNotSupported, err.Error())
	default:
		// ErrContextBusy included: upstream answers a second execution on a busy context with
		// a 500, and clients written against it retry on that.
		writeError(w, http.StatusInternalServerError, codeRuntimeError, err.Error())
	}
}

func decodeJSON(r io.Reader, v any) error {
	data, err := io.ReadAll(io.LimitReader(r, 16<<20))
	if err != nil {
		return err
	}

	return json.Unmarshal(data, v)
}

func (s *Server) createCodeContext(w http.ResponseWriter, r *http.Request) {
	if !s.codeReady(w, r) {
		return
	}

	var req codeContextRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			"invalid code context request: "+err.Error()+`; send {"language":"python","cwd":"/optional/dir"}`)

		return
	}

	c, err := s.code.CreateContext(r.Context(), req.Language, req.Cwd)
	if err != nil {
		codeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, codeContextResponse{ID: c.ID, Language: c.Language, Cwd: req.Cwd})
}

func (s *Server) listCodeContexts(w http.ResponseWriter, r *http.Request) {
	if !s.codeReady(w, r) {
		return
	}

	// The spec marks language as required; upstream lists every context without it, and so
	// does this.
	writeJSON(w, http.StatusOK, s.code.ListContexts(r.URL.Query().Get("language")))
}

func (s *Server) deleteCodeContexts(w http.ResponseWriter, r *http.Request) {
	if !s.codeReady(w, r) {
		return
	}

	lang := r.URL.Query().Get("language")
	if strings.TrimSpace(lang) == "" {
		writeError(w, http.StatusBadRequest, codeMissingQuery,
			"missing query parameter 'language'; DELETE /code/contexts/{id} deletes a single context")

		return
	}

	if err := s.code.DeleteContextsByLanguage(r.Context(), lang); err != nil {
		writeError(w, http.StatusInternalServerError, codeRuntimeError,
			fmt.Sprintf("delete %s contexts: %v", lang, err))

		return
	}

	w.WriteHeader(http.StatusOK)
}

func (s *Server) getCodeContext(w http.ResponseWriter, r *http.Request) {
	if !s.codeReady(w, r) {
		return
	}

	c, err := s.code.GetContext(r.PathValue("contextId"))
	if err != nil {
		codeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, codeContextResponse{ID: c.ID, Language: c.Language})
}

func (s *Server) deleteCodeContext(w http.ResponseWriter, r *http.Request) {
	if !s.codeReady(w, r) {
		return
	}

	if err := s.code.DeleteContext(r.Context(), r.PathValue("contextId")); err != nil {
		codeError(w, err)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (s *Server) runCode(w http.ResponseWriter, r *http.Request) {
	var req runCodeRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			"invalid run code request: "+err.Error()+`; send {"context":{"language":"python"},"code":"..."}`)

		return
	}

	if req.Code == "" {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid run code request: code is required")
		return
	}

	lang := strings.ToLower(strings.TrimSpace(req.Context.Language))

	// A context id without a language is still a kernel run when the id is a kernel's: the
	// alternative - running the code as a shell command, as upstream would - is never what a
	// caller holding a context id meant.
	kernel := jupyter.IsKernelLanguage(lang)
	if !kernel && lang == "" && req.Context.ID != "" {
		if _, err := s.code.GetContext(req.Context.ID); err == nil {
			kernel = true
		}
	}

	if !kernel {
		s.runCodeAsCommand(w, r, lang, req)
		return
	}

	if !s.codeReady(w, r) {
		return
	}

	ew := jupyter.NewEventWriter(w)

	err := s.code.Run(r.Context(), jupyter.RunRequest{ContextID: req.Context.ID, Language: lang, Code: req.Code}, ew.Emit)
	if err != nil && !ew.Started() {
		codeError(w, err)
	}
	// Once the stream has started the status line is gone; the stream itself carries the
	// error, and all that is left is to end the response.
}

// runCodeAsCommand is the language-less path: upstream runs the code as a shell command, in the
// foreground, and so does this.
func (s *Server) runCodeAsCommand(w http.ResponseWriter, r *http.Request, lang string, req runCodeRequest) {
	switch lang {
	case "", "command":
	case "background-command":
		cmdReq := &runCommandRequest{Command: req.Code, Background: true}

		cmd, err := buildCmd(cmdReq)
		if err != nil {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
			return
		}

		s.runBackground(w, cmdReq, cmd)

		return
	default:
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			fmt.Sprintf("language %q is not supported; use one of %s, or omit it to run the code as a shell command",
				req.Context.Language, strings.Join(jupyter.Languages, ", ")))

		return
	}

	cmdReq := &runCommandRequest{Command: req.Code}

	cmd, err := buildCmd(cmdReq)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}

	s.runForeground(w, r, cmdReq, cmd)
}

// interruptCode is DELETE /code?id=. A kernel context is interrupted through Jupyter; any other
// id is a command or session, handled as DELETE /command handles it - upstream's Interrupt looks
// in the same three places.
func (s *Server) interruptCode(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, codeMissingQuery, "missing query parameter 'id': a context id or a command id")
		return
	}

	if _, err := s.code.GetContext(id); err != nil {
		s.interrupt(w, r)
		return
	}

	if err := s.code.Interrupt(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, codeRuntimeError, err.Error())
		return
	}

	w.WriteHeader(http.StatusOK)
}
