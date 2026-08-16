package main

// The guided `sbx init`.
//
// `sbx init > sandbox.json` is a fine scripting primitive and a poor first impression: a
// person who has just installed this has to already know that the command prints to stdout,
// that a template exists, which template they want, and what to do with the result. Nobody
// adopts a tool by reading its --help twice before anything happens.
//
// So at a terminal it asks. One question with a short list, a name it can usually guess, and
// it ends by saying exactly what to run next - the shape `docker init` uses, for the same
// reason.
//
// Piped or redirected it does exactly what it always did, byte for byte. `sbx init >
// sandbox.json` is in people's scripts and in this project's own documentation, and an
// interactive prompt appearing in a pipeline is worse than the problem being fixed here.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// isTerminal reports whether f is something a person is looking at.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}

	return info.Mode()&os.ModeCharDevice != 0
}

// wizard carries the streams so this is testable without a terminal.
type wizard struct {
	in   *bufio.Reader
	out  io.Writer
	yes  bool // --yes: take every default, ask nothing
	eof  bool // stdin ended: stop asking, and never act on an answer nobody gave
	name string
}

// ask prints a prompt and reads a line. An empty answer means the default.
func (w *wizard) ask(prompt, def string) string {
	if w.yes {
		return def
	}

	if def != "" {
		fmt.Fprintf(w.out, "  %s [%s]: ", prompt, def)
	} else {
		fmt.Fprintf(w.out, "  %s: ", prompt)
	}

	line, err := w.in.ReadString('\n')
	if err != nil && line == "" {
		// Ctrl-D, or input that ran out. Remembered, so that nothing further is asked and -
		// more importantly - nothing is confirmed on the strength of an answer nobody gave.
		w.eof = true

		fmt.Fprintln(w.out)

		return def
	}

	if answer := strings.TrimSpace(line); answer != "" {
		return answer
	}

	return def
}

func (w *wizard) confirm(prompt string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}

	if w.yes {
		return def
	}

	if w.eof {
		return false
	}

	answer := strings.ToLower(w.ask(prompt+" ("+hint+")", ""))

	// The default applies to somebody pressing enter, not to somebody who is not there. A
	// question whose default is yes would otherwise create a sandbox because a pipe closed,
	// which is how `echo | sbx init` ends up with something running.
	if w.eof {
		return false
	}

	switch answer {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	default:
		return def
	}
}

// guessName is the branch, because that is what a sandbox is usually for. Falling back to the
// directory covers a worktree that is not a git checkout at all.
func guessName() string {
	out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err == nil {
		if b := strings.TrimSpace(string(out)); b != "" && b != "HEAD" {
			return sanitiseName(b)
		}
	}

	if dir, err := os.Getwd(); err == nil {
		return sanitiseName(filepath.Base(dir))
	}

	return "dev"
}

// sanitiseName makes a branch usable as a sandbox name: feature/ABC-123 is a perfectly normal
// branch and not a name a container can carry.
func sanitiseName(s string) string {
	var b strings.Builder

	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}

	return strings.Trim(b.String(), "-")
}

// runInit is the whole command: guided at a terminal, unchanged anywhere else.
func runInit(template string, chosen bool, yes bool, out io.Writer) error {
	// Not a terminal, or a template was named explicitly: the old behaviour exactly.
	if chosen || !isTerminal(os.Stdout) || !isTerminal(os.Stdin) {
		return printTemplate(template, out)
	}

	w := &wizard{in: bufio.NewReader(os.Stdin), out: out, yes: yes}

	fmt.Fprintf(out, "\n  sbx - a sandbox that sleeps when nobody is using it\n\n")

	name, err := w.choose()
	if err != nil {
		return err
	}

	return w.finish(name)
}

// choose asks the one question that matters and returns the template picked.
func (w *wizard) choose() (string, error) {
	names := TemplateNames()
	if len(names) == 0 {
		return "", fmt.Errorf("this build has no templates embedded in it, which should be impossible")
	}

	fmt.Fprintln(w.out, "  What does this branch need?")
	fmt.Fprintln(w.out)

	for i, n := range names {
		fmt.Fprintf(w.out, "    %d) %-12s %s\n", i+1, n, TemplateDescription(n))
	}

	fmt.Fprintln(w.out)

	def := "1"
	for i, n := range names {
		if n == "postgres" {
			def = strconv.Itoa(i + 1)
		}
	}

	for {
		answer := w.ask("Choose", def)

		// A number or the name itself: people type both, and refusing one of them to be
		// strict about it is the kind of thing that makes a tool tiring.
		if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(names) {
			return names[n-1], nil
		}

		for _, n := range names {
			if strings.EqualFold(answer, n) {
				return n, nil
			}
		}

		fmt.Fprintf(w.out, "  %q is not one of them. Pick 1-%d, or the name.\n", answer, len(names))

		if w.yes {
			return names[0], nil // no terminal to correct itself with
		}
	}
}

// finish writes the spec and says what to do with it.
func (w *wizard) finish(template string) error {
	const target = defaultSpec

	fmt.Fprintln(w.out)

	w.name = w.ask("Name this sandbox", guessName())

	if _, err := os.Stat(target); err == nil {
		fmt.Fprintln(w.out)

		if !w.confirm(target+" already exists. Replace it?", false) {
			fmt.Fprintf(w.out, "\n  Left alone. `sbx create %s` uses the spec that is already there.\n\n", w.name)

			return nil
		}
	}

	path, err := MaterializeTemplate(template)
	if err != nil {
		return err
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	if err := os.WriteFile(target, body, 0o644); err != nil {
		return fmt.Errorf("could not write %s: %w", target, err)
	}

	fmt.Fprintf(w.out, "\n  wrote %s\n\n", target)

	// The point of the whole exercise: nobody should have to work out the next command.
	fmt.Fprintf(w.out, "  Next:\n")
	fmt.Fprintf(w.out, "    sbx serve --idle 5m &          once per machine, not per sandbox\n")
	fmt.Fprintf(w.out, "    sbx create %s\n", w.name)
	fmt.Fprintf(w.out, "    eval \"$(sbx env %s)\"\n\n", w.name)

	if w.confirm("Create it now?", true) {
		fmt.Fprintln(w.out)

		return dispatch("create", []string{w.name})
	}

	fmt.Fprintln(w.out)

	return nil
}

// printTemplate is the original behaviour, kept exactly: to stdout, never to a file.
func printTemplate(name string, out io.Writer) error {
	path, err := MaterializeTemplate(name)
	if err != nil {
		return err
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	_, err = out.Write(body)

	return err
}
