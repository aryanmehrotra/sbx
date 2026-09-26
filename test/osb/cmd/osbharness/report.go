package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"text/tabwriter"
)

// event is one line of `go test -json` (cmd/test2json). Build failures arrive as
// build-output/build-fail with ImportPath set rather than Package, since Go 1.24.
type event struct {
	Action     string  `json:"Action"`
	Package    string  `json:"Package"`
	ImportPath string  `json:"ImportPath"`
	Test       string  `json:"Test"`
	Output     string  `json:"Output"`
	Elapsed    float64 `json:"Elapsed"`
}

// Row is one expected upstream test and what became of it.
type Row struct {
	Test    string
	File    string
	Status  string // PASS | FAIL | SKIP | NORUN
	OK      bool   // counts towards a green run
	Elapsed float64
	Note    string
	Output  []string
}

// Outcome is the whole run.
type Outcome struct {
	Rows []Row
	// PackageFailed: the package failed outside any one test - a build error, a panic that
	// took the binary down, the -timeout firing. Any of those can leave every test that DID
	// report looking green, so it fails the run by itself.
	PackageFailed bool
	PackageOutput []string
	// Unexpected are top-level tests that reported a result without being asked for.
	Unexpected []string
	// Stale are allowances for tests that passed: the skip they excuse no longer happens.
	Stale []string
}

// Green is the only thing the exit status is allowed to depend on.
func (o Outcome) Green() bool {
	if o.PackageFailed || len(o.Rows) == 0 {
		return false
	}

	for _, r := range o.Rows {
		if !r.OK {
			return false
		}
	}

	return true
}

var logLine = regexp.MustCompile(`^\s*\S+\.go:\d+: (.*)$`)

// continuation is a testify line with an empty label column.
var continuation = regexp.MustCompile(`^\s*\t\s+\t`)

// evaluate folds a go test -json stream into one row per expected test.
//
// Only top-level tests are rows. A subtest's failure fails its parent in go test's own
// accounting, so the parent's status already carries it; the subtest's lines are kept in
// the parent's output for the failure detail.
func evaluate(in io.Reader, expected []SuiteTest, exp *Expectations, live io.Writer, save io.Writer) (Outcome, error) {
	type state struct {
		status  string
		elapsed float64
		out     []string
	}

	seen := map[string]*state{}
	var o Outcome

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()

		if save != nil {
			_, _ = save.Write(append(append([]byte{}, line...), '\n'))
		}

		var ev event
		if err := json.Unmarshal(line, &ev); err != nil || ev.Action == "" {
			// Not test2json: go itself printing something (a download, a toolchain message).
			o.PackageOutput = append(o.PackageOutput, string(line))
			continue
		}

		top, _, _ := strings.Cut(ev.Test, "/")

		switch {
		case ev.Action == "build-output":
			o.PackageOutput = append(o.PackageOutput, strings.TrimRight(ev.Output, "\n"))
		case ev.Action == "build-fail":
			o.PackageFailed = true
		case ev.Test == "":
			if ev.Action == "output" {
				o.PackageOutput = append(o.PackageOutput, strings.TrimRight(ev.Output, "\n"))
			}

			if ev.Action == "fail" {
				o.PackageFailed = true
			}
		default:
			s := seen[top]
			if s == nil {
				s = &state{}
				seen[top] = s
			}

			if ev.Action == "output" {
				s.out = append(s.out, strings.TrimRight(ev.Output, "\n"))
			}

			if ev.Test == top && (ev.Action == "pass" || ev.Action == "fail" || ev.Action == "skip") {
				s.status = strings.ToUpper(ev.Action)
				s.elapsed = ev.Elapsed

				if live != nil {
					fmt.Fprintf(live, "  --- %-4s %s (%.1fs)\n", s.status, top, ev.Elapsed)
				}
			}
		}
	}

	if err := sc.Err(); err != nil {
		return o, fmt.Errorf("reading go test output: %w", err)
	}

	want := map[string]bool{}

	for _, t := range expected {
		want[t.Name] = true
		r := Row{Test: t.Name, File: t.File}
		s := seen[t.Name]

		switch {
		case s == nil || s.status == "":
			r.Status = "NORUN"
			r.Note = "never reported a result - the package failed, timed out, or -run missed it"
		default:
			r.Status, r.Elapsed, r.Output = s.status, s.elapsed, s.out
		}

		switch r.Status {
		case "PASS":
			r.OK = true

			if _, ok := exp.Skips[r.Test]; ok {
				o.Stale = append(o.Stale, r.Test)
			}
		case "FAIL":
			r.Note = failureLine(r.Output)
		case "SKIP":
			reason := skipReason(r.Output)
			allow, listed := exp.Skips[r.Test]

			switch {
			case listed && strings.Contains(reason, allow.Message):
				r.OK = true
				r.Note = "allowed (" + allow.Why + "): " + reason
			case listed:
				r.Note = "NOT allowed - skipped for a different reason than the allowance names (" +
					allow.Message + "): " + reason
			default:
				r.Note = "NOT allowed - a skip is not a pass: " + reason
			}
		}

		o.Rows = append(o.Rows, r)
	}

	for name, s := range seen {
		if !want[name] && s.status != "" {
			o.Unexpected = append(o.Unexpected, name)
		}
	}

	return o, nil
}

// skipReason is the t.Skip message: the log line go test prints for it, file:line prefix
// dropped. With several, the last one is the skip (earlier ones are t.Log).
func skipReason(out []string) string {
	reason := ""

	for _, l := range out {
		if m := logLine.FindStringSubmatch(l); m != nil {
			reason = strings.TrimSpace(m[1])
		}
	}

	if reason == "" {
		return "(no message)"
	}

	return reason
}

// failureLine picks the one line worth putting in a table: testify's "Error:" text (with its
// continuation when the first line only introduces it), else the last log line.
func failureLine(out []string) string {
	for i, l := range out {
		_, after, ok := strings.Cut(l, "Error:")
		if !ok || strings.Contains(l, "Error Trace:") {
			continue
		}

		msg := strings.TrimSpace(after)

		// testify continues a message on lines whose label column is blank ("\t    \t...");
		// "Not equal:" alone says nothing, the expected/actual lines under it say everything.
		for _, next := range out[i+1:] {
			if !continuation.MatchString(next) {
				break
			}

			msg += " " + strings.TrimSpace(next)
		}

		return clip(msg)
	}

	last := ""

	for _, l := range out {
		if m := logLine.FindStringSubmatch(l); m != nil {
			last = m[1]
		}
	}

	if last != "" {
		return clip(last)
	}

	return "(no message; see the output below)"
}

func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 140 {
		return s[:140] + "..."
	}

	return s
}

func printOutcome(w io.Writer, o Outcome) {
	counts := map[string]int{}

	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  RESULT\tFILE\tTEST\tTIME\tNOTE")

	for _, r := range o.Rows {
		status := r.Status
		if !r.OK {
			status += " ✗"
		}

		counts[r.Status]++
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%.1fs\t%s\n", status, r.File, r.Test, r.Elapsed, r.Note)
	}

	_ = tw.Flush()

	for _, r := range o.Rows {
		if r.Status != "FAIL" {
			continue
		}

		fmt.Fprintf(w, "\n── %s ───────────────────────────────\n", r.Test)

		lines := r.Output
		if len(lines) > 80 {
			fmt.Fprintf(w, "  (%d lines, last 80 shown)\n", len(lines))
			lines = lines[len(lines)-80:]
		}

		for _, l := range lines {
			fmt.Fprintln(w, l)
		}
	}

	// go test fails the package whenever a test fails; that is only news when no test did.
	if o.PackageFailed && counts["FAIL"] == 0 {
		fmt.Fprintln(w, "\n── the package itself failed (build error, panic or timeout) ──")

		lines := o.PackageOutput
		if len(lines) > 40 {
			lines = lines[len(lines)-40:]
		}

		for _, l := range lines {
			fmt.Fprintln(w, "  "+l)
		}
	}

	if len(o.Unexpected) > 0 {
		fmt.Fprintf(w, "\n  note: ran but not asked for: %s\n", strings.Join(o.Unexpected, ", "))
	}

	if len(o.Stale) > 0 {
		fmt.Fprintf(w, "\n  note: allowed to skip but PASSED - remove from test/osb/expectations: %s\n",
			strings.Join(o.Stale, ", "))
	}

	fmt.Fprintf(w, "\n  %d passed, %d failed, %d skipped, %d did not run - %s\n",
		counts["PASS"], counts["FAIL"], counts["SKIP"], counts["NORUN"], verdict(o))
}

func verdict(o Outcome) string {
	if o.Green() {
		return "CONFORMANT for the selected tests"
	}

	return "NOT conformant"
}

func readExpected(path string) ([]SuiteTest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var tests []SuiteTest

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, file, ok := strings.Cut(sc.Text(), "\t")
		if !ok || name == "" {
			continue
		}

		tests = append(tests, SuiteTest{Name: name, File: file})
	}

	if len(tests) == 0 {
		return nil, errors.New("the expected-test list is empty; refusing to call an empty run green")
	}

	return tests, sc.Err()
}

func runReport(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	expPath := fs.String("expectations", "", "path to test/osb/expectations")
	expectedPath := fs.String("expected", "", "file of <Test>\\t<file> lines, from `osbharness tests`")
	savePath := fs.String("save", "", "also write the raw go test -json stream here")
	prov := fs.String("provider", "docker", "the provider the server ran on: selects skip@<provider> allowances")

	if err := fs.Parse(args); err != nil {
		return err
	}

	exp, err := loadExpectationsFor(*expPath, *prov)
	if err != nil {
		return err
	}

	expected, err := readExpected(*expectedPath)
	if err != nil {
		return err
	}

	var save io.Writer

	if *savePath != "" {
		f, err := os.Create(*savePath)
		if err != nil {
			return err
		}
		defer f.Close()

		save = f
	}

	o, err := evaluate(in, expected, exp, out, save)
	if err != nil {
		return err
	}

	printOutcome(out, o)

	if !o.Green() {
		return errors.New("not conformant")
	}

	return nil
}
