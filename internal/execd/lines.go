package execd

// maxLine bounds how much of an unterminated line is held before it is emitted anyway. Upstream
// holds without limit; a process printing gigabytes with no newline would then take execd's
// memory with it, and execd is the one process in the sandbox that must stay up.
const maxLine = 8 << 20

// lineSplitter turns a byte stream into the per-line events upstream sends, with upstream's
// exact rules (runtime/command_common.go commandOutputTail): a line ends at '\n' or '\r', its
// terminator is not included, a CRLF pair ends one line not two, and an empty line is sent as
// "\n" - because an event with empty text is dropped, and a blank line in the output is
// output.
type lineSplitter struct {
	emit    func(string)
	pending []byte
	lastCR  bool
}

func (l *lineSplitter) write(p []byte) {
	for _, b := range p {
		if b == '\n' || b == '\r' {
			switch {
			case len(l.pending) > 0:
				l.emit(string(l.pending))
				l.pending = l.pending[:0]
			case b == '\n' && l.lastCR:
				// The LF of a CRLF: the CR already ended this line.
			default:
				l.emit("\n")
			}

			l.lastCR = b == '\r'

			continue
		}

		l.lastCR = false
		l.pending = append(l.pending, b)

		if len(l.pending) >= maxLine {
			l.emit(string(l.pending))
			l.pending = l.pending[:0]
		}
	}
}

// flush emits a final line that had no terminator, as upstream does when the process exits.
func (l *lineSplitter) flush() {
	if len(l.pending) > 0 {
		l.emit(string(l.pending))
		l.pending = l.pending[:0]
	}
}
