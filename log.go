package main

// Logging, in the shape a Go service already logs.
//
// A sandbox is a set of processes, and the daemon in front of them is a server. Both should
// read like one — same columns, same levels, one stream — so that `sbx logs` is something
// you can leave running in a pane rather than a wall of raw container output with no idea
// which line came from where.
//
// The format follows GoFr's: a four-character level in colour, the time, then the message.
// Attached to a terminal it aligns into columns; piped anywhere else it emits JSON, because
// that is what a log shipper expects and a human is not reading it anyway.
//
//	INFO [12:34:56] my-branch/postgres  database system is ready to accept connections
//	{"level":"INFO","time":"...","sandbox":"my-branch","service":"postgres","message":"..."}
//
// LOG_LEVEL picks the floor: DEBUG, INFO, NOTICE, WARN, ERROR, FATAL. Default INFO.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

type Level int

const (
	LevelDebug Level = iota + 1
	LevelInfo
	LevelNotice
	LevelWarn
	LevelError
	LevelFatal
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelNotice:
		return "NOTICE"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	case LevelFatal:
		return "FATAL"
	default:
		return "INFO"
	}
}

// color is the 256-colour code for a level, matching what a GoFr service prints so the two
// sit together in one terminal without looking like different tools.
func (l Level) color() int {
	switch l {
	case LevelError, LevelFatal:
		return 160
	case LevelWarn, LevelNotice:
		return 220
	case LevelInfo:
		return 6
	case LevelDebug:
		return 8
	default:
		return 37
	}
}

func parseLevel(s string) Level {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return LevelDebug
	case "NOTICE":
		return LevelNotice
	case "WARN", "WARNING":
		return LevelWarn
	case "ERROR":
		return LevelError
	case "FATAL":
		return LevelFatal
	default:
		return LevelInfo
	}
}

// entry is one line. Sandbox and service are the fields a raw container log never has and
// the reader always wants.
type entry struct {
	Level      string    `json:"level"`
	Time       time.Time `json:"time"`
	Sandbox    string    `json:"sandbox,omitempty"`
	Service    string    `json:"service,omitempty"`
	Message    string    `json:"message"`
	SbxVersion string    `json:"sbxVersion"`
}

// logger is the process-wide logger. A package-level variable rather than something
// threaded through every call because every path here logs, and threading it changed more
// lines than it clarified.
var logger = NewLogger(os.Stdout)

type Logger struct {
	out   io.Writer
	level Level
	tty   bool
	width int // service column, so lines from several services align

	mu sync.Mutex
}

// NewLogger decides its own format. Attached to a terminal it prints columns; anything else
// gets JSON — a pipe means a file, a shipper or a CI log, none of which want escape codes.
func NewLogger(out io.Writer) *Logger {
	return &Logger{
		out:   out,
		level: parseLevel(os.Getenv("LOG_LEVEL")),
		tty:   isTerminal(out),
	}
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}

	info, err := f.Stat()
	if err != nil {
		return false
	}

	return info.Mode()&os.ModeCharDevice != 0
}

// align reserves a column width for service names, so a sandbox with a postgres and a redis
// in it does not jitter between lines.
func (l *Logger) align(width int) {
	l.mu.Lock()
	l.width = width
	l.mu.Unlock()
}

func (l *Logger) log(lvl Level, sandbox, service, msg string) {
	if lvl < l.level {
		return
	}

	// Locked, and whole lines only. Several containers writing at once otherwise interleave
	// mid-sentence, which is exactly the thing that makes aggregated logs unreadable.
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.tty {
		body, err := json.Marshal(entry{
			Level:      lvl.String(),
			Time:       time.Now(),
			Sandbox:    sandbox,
			Service:    service,
			Message:    msg,
			SbxVersion: version,
		})
		if err != nil {
			return
		}

		fmt.Fprintf(l.out, "%s\n", body)

		return
	}

	// Four characters, like GoFr: the levels stay one column wide whatever they are.
	short := lvl.String()
	if len(short) > 4 {
		short = short[:4]
	}

	fmt.Fprintf(l.out, "[38;5;%dm%-4s[0m [%s]", lvl.color(), short, time.Now().Format(time.TimeOnly))

	if where := source(sandbox, service); where != "" {
		fmt.Fprintf(l.out, " [38;5;8m%-*s[0m", l.width, where)
	}

	fmt.Fprintf(l.out, " %s\n", msg)
}

func source(sandbox, service string) string {
	switch {
	case sandbox != "" && service != "":
		return sandbox + "/" + service
	case service != "":
		return service
	default:
		return sandbox
	}
}

func (l *Logger) Debug(sandbox, service, format string, a ...any) {
	l.log(LevelDebug, sandbox, service, fmt.Sprintf(format, a...))
}

func (l *Logger) Info(sandbox, service, format string, a ...any) {
	l.log(LevelInfo, sandbox, service, fmt.Sprintf(format, a...))
}

func (l *Logger) Notice(sandbox, service, format string, a ...any) {
	l.log(LevelNotice, sandbox, service, fmt.Sprintf(format, a...))
}

func (l *Logger) Warn(sandbox, service, format string, a ...any) {
	l.log(LevelWarn, sandbox, service, fmt.Sprintf(format, a...))
}

func (l *Logger) Error(sandbox, service, format string, a ...any) {
	l.log(LevelError, sandbox, service, fmt.Sprintf(format, a...))
}

// lineWriter turns a container's raw output into log lines.
//
// A container writes whatever it likes with no level and no idea which sandbox it is in.
// This attaches both, holds partial lines until they are whole, and passes the text through
// unchanged — reformatting somebody else's log message is how you lose the detail you were
// reading it for.
type lineWriter struct {
	log     *Logger
	sandbox string
	service string
	level   Level

	mu      sync.Mutex
	partial []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.partial = append(w.partial, p...)

	for {
		i := indexByte(w.partial, '\n')
		if i < 0 {
			break
		}

		line := strings.TrimRight(string(w.partial[:i]), "\r")
		w.partial = w.partial[i+1:]

		if strings.TrimSpace(line) != "" {
			w.log.log(w.level, w.sandbox, w.service, line)
		}
	}

	return len(p), nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}

	return -1
}
