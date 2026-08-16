// Package history is the answer to "what happened to this sandbox, and who did it".
//
// Everything sbx does is otherwise invisible after the fact. A sandbox woke at 3am because
// something connected to it, a colleague removed one, an agent created forty and left them:
// none of that survived the terminal scrollback it was printed to, and the daemon's own logs
// go wherever its stdout went, which for a launchd or systemd unit is not where the person
// asking the question is looking.
//
// So it is written down. One append-only file, one JSON object per line, in the same ~/.sbx
// the daemon already uses. JSON because the point of a record is that a machine can read it
// too - `sbx history --json | jq` should work without parsing a sentence written for a human.
//
// Best-effort throughout, like the presence file: a read-only home directory degrades this to
// nothing rather than failing the command somebody actually asked for. Nobody's `sbx create`
// should fail because their audit log could not be written.
package history

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Record is one thing that happened.
type Record struct {
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"` // command | event
	Sandbox string    `json:"sandbox,omitempty"`
	Service string    `json:"service,omitempty"`

	// Command is the argv as typed, with secrets removed. Kind == "command".
	Command []string `json:"command,omitempty"`
	Dir     string   `json:"dir,omitempty"`

	// Event is the daemon's own vocabulary: woke, slept, wakeFailed. Kind == "event".
	Event      string `json:"event,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`

	Message string `json:"message,omitempty"`
	Failed  bool   `json:"failed,omitempty"`
	Error   string `json:"error,omitempty"`
}

// maxBytes caps the file. At roughly 150 bytes a record this is some 70,000 events, which is
// months of ordinary use; past it the file is rotated once rather than grown without limit,
// because an audit log that fills a laptop is a bug and not a feature.
const maxBytes = 10 << 20

var mu sync.Mutex

// Path is where the journal lives. Beside the daemon's presence file, for the same reason:
// it is per-user, because the daemon runs as you rather than as root.
func Path() (string, error) {
	if p := os.Getenv("SBX_HISTORY"); p != "" {
		return p, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, ".sbx", "history.jsonl"), nil
}

// Append writes one record. It never returns an error: see the package comment.
func Append(r Record) {
	if r.Time.IsZero() {
		r.Time = time.Now()
	}

	path, err := Path()
	if err != nil {
		return
	}

	mu.Lock()
	defer mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}

	rotate(path)

	line, err := json.Marshal(r)
	if err != nil {
		return
	}

	// 0600: this records what was run and where, which is nobody else's business on a shared
	// machine.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()

	_, _ = f.Write(append(line, '\n'))
}

// rotate keeps one previous file, so the cap is a bound rather than a data loss surprise.
func rotate(path string) {
	info, err := os.Stat(path)
	if err != nil || info.Size() < maxBytes {
		return
	}

	_ = os.Rename(path, path+".1")
}

// Filter narrows a read.
type Filter struct {
	Sandbox string
	Kind    string
	Limit   int
}

// Read returns the most recent records last, which is the order they are printed in: a log
// people scroll to the bottom of should end at "now".
func Read(f Filter) ([]Record, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}

	var out []Record

	// The rotated file first, so a limit that spans a rotation still returns the older half
	// in the right order.
	for _, p := range []string{path + ".1", path} {
		recs, err := readFile(p, f)
		if err != nil {
			return nil, err
		}

		out = append(out, recs...)
	}

	if f.Limit > 0 && len(out) > f.Limit {
		out = out[len(out)-f.Limit:]
	}

	return out, nil
}

func readFile(path string, f Filter) ([]Record, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // nothing has happened yet, which is not an error
		}

		return nil, err
	}
	defer file.Close()

	var out []Record

	sc := bufio.NewScanner(file)

	// A long record must not end the read. bufio.Scanner gives up permanently past its
	// buffer, so one oversized line would otherwise hide every record after it.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for sc.Scan() {
		var r Record

		// A truncated last line is normal after a kill, and is skipped rather than treated as
		// a corrupt file.
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue
		}

		if f.Sandbox != "" && r.Sandbox != f.Sandbox {
			continue
		}

		if f.Kind != "" && r.Kind != f.Kind {
			continue
		}

		out = append(out, r)
	}

	return out, sc.Err()
}

// secretish matches the flags and assignments whose values must never be written down.
//
// The audit log is the one place where recording faithfully is the wrong default: `sbx add x
// db --env POSTGRES_PASSWORD=hunter2` is an ordinary command, and a file that remembers it
// forever has turned a convenience into a credential leak that outlives the sandbox.
var secretish = regexp.MustCompile(`(?i)(pass|secret|token|key|cred|auth)`)

// Redact returns argv with secret values replaced. It keeps the names, because knowing that
// POSTGRES_PASSWORD was set is the useful half and the value is the dangerous half.
func Redact(argv []string) []string {
	out := make([]string, 0, len(argv))

	envFlag := false

	for _, a := range argv {
		switch {
		case envFlag:
			// The value of a preceding --env, whatever it looks like.
			out = append(out, redactAssignments(a))
			envFlag = false

		case a == "--env" || a == "-e":
			out = append(out, a)
			envFlag = true

		case strings.HasPrefix(a, "--env="):
			out = append(out, "--env="+redactAssignments(strings.TrimPrefix(a, "--env=")))

		case strings.Contains(a, "="):
			out = append(out, redactAssignments(a))

		default:
			out = append(out, a)
		}
	}

	return out
}

// redactAssignments rewrites K=V pairs, keeping K. Several pairs may be comma-separated,
// which is how `sbx add --env` takes them.
func redactAssignments(s string) string {
	parts := strings.Split(s, ",")

	for i, p := range parts {
		k, v, ok := strings.Cut(p, "=")
		if !ok || v == "" {
			continue
		}

		if secretish.MatchString(k) {
			parts[i] = k + "=***"
		}
	}

	return strings.Join(parts, ",")
}
