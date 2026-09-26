package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Expectations is test/osb/expectations, parsed.
type Expectations struct {
	// Tiers in file order, so "cumulative" means "this one and every one above it".
	Tiers []Tier
	// Skips by test name.
	Skips map[string]AllowedSkip
}

type Tier struct {
	Name  string
	Files []string
}

type AllowedSkip struct {
	Test    string
	Message string // the skip reason must contain this
	Why     string
}

func loadExpectations(path string) (*Expectations, error) { return loadExpectationsFor(path, "docker") }

// loadExpectationsFor reads the file as it applies to one provider: `skip@<provider>` lines
// count only for that provider, plain `skip` lines for every one.
func loadExpectationsFor(path, provider string) (*Expectations, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return parseExpectationsFor(f, provider)
}

func parseExpectations(r io.Reader) (*Expectations, error) { return parseExpectationsFor(r, "docker") }

func parseExpectationsFor(r io.Reader, provider string) (*Expectations, error) {
	e := &Expectations{Skips: map[string]AllowedSkip{}}
	sc := bufio.NewScanner(r)
	n := 0

	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		kind, rest, _ := strings.Cut(line, " ")

		// skip@<provider>: an allowance that holds on one provider only - a capability that
		// backend refuses by name (a microVM mounting a host directory), which another has.
		scoped := ""
		if k, p, ok := strings.Cut(kind, "@"); ok && k == "skip" {
			if p == "" {
				return nil, fmt.Errorf("line %d: skip@ needs a provider, as skip@firecracker", n)
			}

			kind, scoped = "skip", p
		}

		switch kind {
		case "tier":
			fields := strings.Fields(rest)
			if len(fields) < 2 {
				return nil, fmt.Errorf("line %d: tier needs a name and at least one file", n)
			}

			e.Tiers = append(e.Tiers, Tier{Name: fields[0], Files: fields[1:]})
		case "skip":
			parts := strings.Split(rest, "|")
			if len(parts) != 3 {
				return nil, fmt.Errorf("line %d: skip is `skip <Test> | <message> | <why>`", n)
			}

			s := AllowedSkip{
				Test:    strings.TrimSpace(parts[0]),
				Message: strings.TrimSpace(parts[1]),
				Why:     strings.TrimSpace(parts[2]),
			}

			// An empty message would match every skip reason, which is exactly the blanket
			// allowance this file exists to prevent.
			if s.Test == "" || s.Message == "" || s.Why == "" {
				return nil, fmt.Errorf("line %d: skip needs a test, a message and a reason, all non-empty", n)
			}

			if scoped != "" && scoped != provider {
				continue
			}

			e.Skips[s.Test] = s
		default:
			return nil, fmt.Errorf("line %d: unknown directive %q", n, kind)
		}
	}

	return e, sc.Err()
}

// FilesFor returns every file up to and including the named tier.
func (e *Expectations) FilesFor(tier string) ([]string, error) {
	var files []string

	for _, t := range e.Tiers {
		files = append(files, t.Files...)

		if t.Name == tier {
			return files, nil
		}
	}

	names := make([]string, 0, len(e.Tiers))
	for _, t := range e.Tiers {
		names = append(names, t.Name)
	}

	return nil, fmt.Errorf("no tier %q; known: %s", tier, strings.Join(names, ", "))
}

func runTier(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tier", flag.ContinueOnError)
	exp := fs.String("expectations", "", "path to test/osb/expectations")
	tier := fs.String("tier", "", "release tier, e.g. v0.9.0")

	if err := fs.Parse(args); err != nil {
		return err
	}

	e, err := loadExpectations(*exp)
	if err != nil {
		return err
	}

	files, err := e.FilesFor(*tier)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintln(out, strings.Join(files, ","))

	return err
}

// SuiteTest is one top-level upstream test and the file it came from.
type SuiteTest struct {
	Name string
	File string
}

var testFunc = regexp.MustCompile(`(?m)^func (Test\w+)\(`)

// upstreamFile maps a short name to its file. Every file is <name>_e2e_test.go except one,
// e2e_test.go, which the short name "e2e" means.
func upstreamFile(dir, name string) string {
	if name == "e2e" {
		return filepath.Join(dir, "e2e_test.go")
	}

	return filepath.Join(dir, name+"_e2e_test.go")
}

// listTests reads the named files and returns their top-level tests, in file order. An
// unknown file name is an error rather than zero tests: a typo in --files must not produce
// a run that checks nothing and passes.
func listTests(dir string, files []string, match *regexp.Regexp) ([]SuiteTest, error) {
	var tests []SuiteTest

	for _, name := range files {
		path := upstreamFile(dir, name)

		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("no upstream test file for %q (%s): %w", name, path, err)
		}

		found := testFunc.FindAllSubmatch(body, -1)
		if len(found) == 0 {
			return nil, fmt.Errorf("%s has no top-level tests", path)
		}

		for _, m := range found {
			t := SuiteTest{Name: string(m[1]), File: name}
			if match == nil || match.MatchString(t.Name) {
				tests = append(tests, t)
			}
		}
	}

	if len(tests) == 0 {
		return nil, fmt.Errorf("no tests selected from %s", strings.Join(files, ","))
	}

	return tests, nil
}

func runTests(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tests", flag.ContinueOnError)
	dir := fs.String("dir", "", "upstream tests/go directory")
	files := fs.String("files", "", "comma-separated short file names")
	match := fs.String("match", "", "keep only tests whose name matches this regexp")

	if err := fs.Parse(args); err != nil {
		return err
	}

	var re *regexp.Regexp

	if *match != "" {
		var err error
		if re, err = regexp.Compile(*match); err != nil {
			return fmt.Errorf("-match: %w", err)
		}
	}

	var names []string

	for _, f := range strings.Split(*files, ",") {
		if f = strings.TrimSpace(f); f != "" {
			names = append(names, f)
		}
	}

	tests, err := listTests(*dir, names, re)
	if err != nil {
		return err
	}

	for _, t := range tests {
		if _, err := fmt.Fprintf(out, "%s\t%s\n", t.Name, t.File); err != nil {
			return err
		}
	}

	return nil
}

// runFreePort asks the kernel for a port nothing holds. There is a window between this
// closing and sbx binding it, which is acceptable for a test daemon and is why the caller
// checks that the daemon it started is the one that answers.
func runFreePort(out io.Writer) error {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}

	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	_, err = fmt.Fprintln(out, port)

	return err
}
