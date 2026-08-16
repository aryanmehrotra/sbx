package ui

// Making a container's log readable.
//
// What a database prints is written for a log file, not for a person watching a dashboard:
//
//   2026-08-16T12:14:03.671056Z 0 [Warning] [MY-011068] [Server] The syntax '--skip-host-cache'
//
// Of that line, the date is today, the microseconds are noise, the thread id is 0, and the
// error code is a thing you look up only when something is wrong. What a reader wants at a
// glance is the time, whether it is bad, and the sentence. So the fixed parts are dimmed
// rather than deleted - deleting them would be lying about what the service said, and this
// project's whole posture is that it does not do that - and the level is coloured so the eye
// finds the warnings without reading anything.
//
// Every backend writes its own shape, so this recognises rather than parses: postgres,
// mysql, redis and nginx all lead with something time-like and mention a level somewhere.
// Anything it does not recognise is passed through untouched, which is the only safe default
// for output somebody is relying on to debug.

import (
	"strings"
	"time"
)

// timeLayouts are the shapes seen at the front of a log line, most specific first.
var timeLayouts = []string{
	"2006-01-02T15:04:05.000000Z",
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05Z",
	"2006-01-02 15:04:05.000 MST",
	"2006-01-02 15:04:05-07:00",
	"2006-01-02 15:04:05.000",
	"2006-01-02 15:04:05",
	"2006/01/02 15:04:05",
	"02/Jan/2006:15:04:05 -0700",

	// Redis, which writes the day without padding and the month as a word.
	"2 Jan 2006 15:04:05.000",
	"02 Jan 2006 15:04:05.000",
}

// formatLog rewrites one line for reading. It never drops information: the timestamp is
// shortened to a time, and the rest is dimmed or coloured in place.
func formatLog(raw string) string {
	line := strings.TrimRight(raw, "\r")

	// Redis prefixes every line with "<pid>:<role> ", which is the same on every line and so
	// carries nothing at a glance. Held back and dimmed rather than dropped.
	prefix, body := splitPIDPrefix(line)

	rest, when, ok := leadingTime(body)
	if !ok {
		return dim + prefix + reset + colourLevel(body)
	}

	if prefix != "" {
		prefix = dim + prefix + reset
	}

	// Only the clock. The date is on the screen's other timestamps and is nearly always today;
	// when it is not, the events pane says how long ago in plain language.
	return dim + when.Format("15:04:05") + reset + " " + prefix + colourLevel(rest)
}

// splitPIDPrefix separates redis's "1:M " style prefix from the rest of the line.
func splitPIDPrefix(line string) (prefix, rest string) {
	i := strings.IndexByte(line, ':')
	if i <= 0 || i > 8 {
		return "", line
	}

	for _, r := range line[:i] {
		if r < '0' || r > '9' {
			return "", line
		}
	}

	// The role is one letter (C, M, S) or a word like signal-handler.
	j := strings.IndexByte(line[i+1:], ' ')
	if j < 0 || j > 16 {
		return "", line
	}

	return line[:i+j+2], line[i+j+2:]
}

// leadingTime pulls a timestamp off the front of a line, returning what is left.
func leadingTime(line string) (rest string, at time.Time, ok bool) {
	// Bracketed, as nginx and some proxies write it.
	trimmed := strings.TrimPrefix(line, "[")

	for _, layout := range timeLayouts {
		if len(trimmed) < len(layout) {
			continue
		}

		// The layout's length is a good guess at the field's length, and parsing is the check.
		candidate := trimmed[:len(layout)]

		t, err := time.Parse(layout, candidate)
		if err != nil {
			continue
		}

		return strings.TrimLeft(trimmed[len(layout):], "] "), t, true
	}

	return line, time.Time{}, false
}

// levels are the words that decide a line's colour, longest first so that "WARNING" is not
// matched as "WARN" with a stray suffix left behind.
var levels = []struct {
	word   string
	colour string
}{
	{"CRITICAL", red},
	{"EMERGENCY", red},
	{"FATAL", red},
	{"ERROR", red},
	{"ERRO", red},
	{"WARNING", yellow},
	{"WARN", yellow},
	{"NOTICE", cyan},
	{"SYSTEM", dim},
	{"DEBUG", dim},
	{"TRACE", dim},
	{"NOTE", dim},
	{"INFO", dim},
	{"LOG", dim},
}

// colourLevel finds a level word near the front of a line and colours the whole line by it.
//
// The whole line rather than just the word: a warning is worth noticing in peripheral vision,
// and one coloured token in a wall of grey is not. Only near the front, because a line that
// happens to contain the word "error" in its message is not itself an error - `[Note] no
// error was found` would otherwise light up red.
func colourLevel(line string) string {
	// Redis says it with a symbol: # is a warning, * is a notice, . is debug.
	if len(line) > 1 && line[1] == ' ' {
		switch line[0] {
		case '#':
			return yellow + line + reset
		case '*':
			return line
		case '.', '-':
			return dim + line + reset
		}
	}

	head := line
	if len(head) > 48 {
		head = head[:48]
	}

	upper := strings.ToUpper(head)

	// The earliest level word wins, not the first one in the table.
	//
	// Checking in table order made `[1] LOG:  no error was found` an error: ERROR is listed
	// before LOG and the word appears later in the line, so the line was coloured by something
	// it was reporting rather than by what it was. Position is the thing that decides which
	// token is the line's own level.
	best, at := "", len(upper)+1

	for _, l := range levels {
		i := strings.Index(upper, l.word)
		if i < 0 || i > at {
			continue
		}

		// A tie means one word contains the other - WARNING and WARN - and the longer is the
		// real token.
		if i == at && len(l.word) <= len(best) {
			continue
		}

		best, at = l.word, i
	}

	for _, l := range levels {
		if l.word != best {
			continue
		}

		if l.colour == dim {
			return dim + line + reset
		}

		return l.colour + line + reset
	}

	return line
}
