package cli

// What happened, and who did it.
//
// The daemon's own output goes wherever its stdout went, which for a launchd or systemd unit
// is not where the person asking is looking, and a terminal's scrollback is not an audit
// trail. `sbx history` reads the journal instead, so "why did this wake at 3am" and "who
// removed my sandbox" have answers after the fact.

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aryanmehrotra/sbx/internal/history"
)

// History prints the journal, newest last.
func History(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	limit := fs.Int("limit", 50, "how many records to show; 0 for all")
	asJSON := fs.Bool("json", false, "one JSON object per line, as stored")
	commands := fs.Bool("commands", false, "only what somebody ran")
	events := fs.Bool("events", false, "only what the daemon did")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *commands && *events {
		return fmt.Errorf("--commands and --events ask for opposite halves of the same file; " +
			"leave both off to see everything")
	}

	f := history.Filter{Limit: *limit}

	switch {
	case *commands:
		f.Kind = "command"
	case *events:
		f.Kind = "event"
	}

	// A bare name is the sandbox to filter by, which is the common case: `sbx history my-branch`.
	if rest := fs.Args(); len(rest) > 0 {
		f.Sandbox = rest[0]
	}

	records, err := history.Read(f)
	if err != nil {
		return fmt.Errorf("could not read the history: %w", err)
	}

	if len(records) == 0 {
		path, _ := history.Path()

		if f.Sandbox != "" {
			fmt.Fprintf(out, "nothing recorded for %q yet.\n", f.Sandbox)
		} else {
			fmt.Fprintf(out, "nothing recorded yet - %s is written as you use sbx.\n", path)
		}

		return nil
	}

	if *asJSON {
		enc := json.NewEncoder(out)

		for _, r := range records {
			if err := enc.Encode(r); err != nil {
				return err
			}
		}

		return nil
	}

	return render(records, out)
}

func render(records []history.Record, out io.Writer) error {
	// The widest label, so the two columns line up rather than starting wherever the previous
	// name happened to end - the same reason the daemon reserves one.
	width := 0
	for _, r := range records {
		if w := len(label(r)); w > width {
			width = w
		}
	}

	var day string

	for _, r := range records {
		// A date header rather than a date on every line: the question is usually "what
		// happened today", and repeating the date 50 times answers it worse.
		if d := r.Time.Format("Mon 2 Jan 2006"); d != day {
			if day != "" {
				fmt.Fprintln(out)
			}

			fmt.Fprintf(out, "%s\n", d)
			day = d
		}

		fmt.Fprintf(out, "  %s  %-*s  %s\n",
			r.Time.Format(time.TimeOnly), width, label(r), detail(r))
	}

	return nil
}

func label(r history.Record) string {
	switch {
	case r.Sandbox != "" && r.Service != "":
		return r.Sandbox + "/" + r.Service
	case r.Sandbox != "":
		return r.Sandbox
	default:
		return "-"
	}
}

func detail(r history.Record) string {
	var b strings.Builder

	if r.Kind == "command" {
		b.WriteString("$ ")
		b.WriteString(strings.Join(r.Command, " "))
	} else {
		b.WriteString(r.Event)

		// The same field means different things per event, and printing one preposition for
		// both says something false. A wake's duration is how long the caller waited; a
		// sleep's is how long the sandbox sat idle first, which is not how long sleeping took.
		if r.DurationMs > 0 {
			switch r.Event {
			case "slept":
				fmt.Fprintf(&b, " after %s idle",
					(time.Duration(r.DurationMs) * time.Millisecond).Round(time.Second))
			default:
				fmt.Fprintf(&b, " in %dms", r.DurationMs)
			}
		}
	}

	// Who did it, when it was not the daemon. Printed only for a person, because saying
	// "by daemon" on every one of thousands of automatic lines would bury the handful that
	// somebody actually did.
	if r.Actor != "" && r.Actor != "daemon" {
		fmt.Fprintf(&b, "   by %s", r.Actor)
	}

	if r.Failed {
		b.WriteString("   failed")

		if r.Error != "" {
			b.WriteString(": ")
			b.WriteString(firstLine(r.Error))
		}
	}

	return b.String()
}
