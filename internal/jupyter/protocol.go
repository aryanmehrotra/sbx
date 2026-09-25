package jupyter

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

// The Jupyter messaging protocol, version 5.3, as Jupyter Server relays it over a kernel's
// channels websocket: one JSON object per text frame, with a "channel" field naming the ZMQ
// socket it came from or is bound for. No subprotocol is requested, so the server uses this
// legacy JSON framing rather than the v1 binary one.
const protocolVersion = "5.3"

type msgHeader struct {
	MsgID    string `json:"msg_id"`
	MsgType  string `json:"msg_type"`
	Username string `json:"username,omitempty"`
	Session  string `json:"session,omitempty"`
	Date     string `json:"date,omitempty"`
	Version  string `json:"version,omitempty"`
}

// inbound is a message from the kernel. Only the fields the engine reads are decoded.
type inbound struct {
	Header       msgHeader       `json:"header"`
	ParentHeader msgHeader       `json:"parent_header"`
	Content      json.RawMessage `json:"content"`
	Channel      string          `json:"channel"`
}

// outbound is a message to the kernel. parent_header and metadata are always objects, never
// null: some kernels index into them without checking.
type outbound struct {
	Header       msgHeader      `json:"header"`
	ParentHeader map[string]any `json:"parent_header"`
	Metadata     map[string]any `json:"metadata"`
	Content      any            `json:"content"`
	Buffers      []any          `json:"buffers"`
	Channel      string         `json:"channel"`
}

type executeRequest struct {
	Code            string            `json:"code"`
	Silent          bool              `json:"silent"`
	StoreHistory    bool              `json:"store_history"`
	UserExpressions map[string]string `json:"user_expressions"`
	AllowStdin      bool              `json:"allow_stdin"`
	StopOnError     bool              `json:"stop_on_error"`
}

// newExecuteRequest builds the shell message. The options are upstream's: history on, stdin off
// (there is nobody to answer input()), and stop_on_error so a failing cell aborts anything the
// kernel had queued behind it.
func newExecuteRequest(session, code string) outbound {
	return outbound{
		Header: msgHeader{
			MsgID:    newID(),
			MsgType:  "execute_request",
			Username: "sbx",
			Session:  session,
			Date:     time.Now().UTC().Format(time.RFC3339Nano),
			Version:  protocolVersion,
		},
		ParentHeader: map[string]any{},
		Metadata:     map[string]any{},
		Content: executeRequest{
			Code:            code,
			StoreHistory:    true,
			UserExpressions: map[string]string{},
			StopOnError:     true,
		},
		Buffers: []any{},
		Channel: "shell",
	}
}

type streamContent struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

type executeResultContent struct {
	ExecutionCount int            `json:"execution_count"`
	Data           map[string]any `json:"data"`
}

type displayDataContent struct {
	Data map[string]any `json:"data"`
}

type statusContent struct {
	ExecutionState string `json:"execution_state"`
}

type executeReplyContent struct {
	Status         string   `json:"status"`
	ExecutionCount int      `json:"execution_count"`
	EName          string   `json:"ename"`
	EValue         string   `json:"evalue"`
	Traceback      []string `json:"traceback"`
}

// newID is a random hex id. Not a UUID library: 16 random bytes are all a msg_id has to be.
func newID() string {
	var b [16]byte

	_, _ = rand.Read(b[:])

	return hex.EncodeToString(b[:])
}

// resultsFromData converts a MIME bundle to an event's results map.
//
// "text/plain" becomes "text" because upstream renames it on the way out and its SDKs read the
// renamed key; every other MIME type passes through untouched.
func resultsFromData(data map[string]any) map[string]any {
	if len(data) == 0 {
		return nil
	}

	out := make(map[string]any, len(data))

	for k, v := range data {
		if k == "text/plain" {
			k = "text"
		}

		out[k] = v
	}

	return out
}
