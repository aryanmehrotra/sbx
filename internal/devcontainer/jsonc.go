// Package devcontainer reads a devcontainer.json and turns it into a service sbx can run.
//
// The point is adoption without a second file. A repository that already has `.devcontainer/`
// has already answered most of what sandbox.json asks - the image, the ports, the mounts, the
// setup command - and asking somebody to write it again in a different shape is the reason they
// do not try the tool.
//
// It is a translation and not an implementation. sbx does not run Features, does not honour
// every lifecycle hook, and says so rather than appearing to: a partial import that stays quiet
// about what it dropped is worse than one that prints the list.
package devcontainer

import "strings"

// stripJSONC removes comments and trailing commas so encoding/json can read the file.
//
// devcontainer.json is JSON with comments - the spec says so, VS Code's own templates ship with
// them, and encoding/json refuses all of it. Stripping is the whole reason this function exists.
//
// It is a scanner rather than a regexp because the hard cases are all about context: a `//`
// inside a string is not a comment, an escaped quote does not end a string, and a `}` after a
// comma is only a trailing comma if the comma was not itself inside a string. A regexp gets each
// of those wrong in a way that silently changes somebody's configuration.
func stripJSONC(src string) string {
	var (
		out       strings.Builder
		inString  bool
		escaped   bool
		inLine    bool
		inBlock   bool
		lastComma = -1 // index in out of a comma that might turn out to be trailing
	)

	out.Grow(len(src))

	flushComma := func(keep bool) {
		if lastComma < 0 {
			return
		}

		if !keep {
			// Rewrite without the comma. Cheap because it happens once per trailing comma,
			// which is rare, rather than once per byte.
			s := out.String()
			s = s[:lastComma] + s[lastComma+1:]

			out.Reset()
			out.WriteString(s)
		}

		lastComma = -1
	}

	for i := 0; i < len(src); i++ {
		c := src[i]

		switch {
		case inLine:
			if c == '\n' {
				inLine = false

				out.WriteByte(c)
			}

			continue

		case inBlock:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlock = false
				i++
			}

			continue

		case inString:
			out.WriteByte(c)

			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}

			continue
		}

		// Outside a string: a slash pair starts a comment, and everything else is structure.
		if c == '/' && i+1 < len(src) {
			if src[i+1] == '/' {
				inLine = true
				i++

				continue
			}

			if src[i+1] == '*' {
				inBlock = true
				i++

				continue
			}
		}

		switch {
		case c == '"':
			flushComma(true)

			inString = true

			out.WriteByte(c)

		case c == ',':
			flushComma(true)

			lastComma = out.Len()

			out.WriteByte(c)

		case c == '}' || c == ']':
			// The comma before this closer was trailing after all.
			flushComma(false)
			out.WriteByte(c)

		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			out.WriteByte(c) // whitespace does not settle whether a comma was trailing

		default:
			flushComma(true)
			out.WriteByte(c)
		}
	}

	return out.String()
}
