package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The gate is the product here: every way a run can look green without being green gets a
// case, and each case asserts the verdict the stream should produce.

const exps = `
tier v0.9.0 sandbox command
tier v0.10.0 volume
skip TestSandbox_PauseAndResume | skip pause/resume e2e test | upstream skips it
`

func mustExp(t *testing.T) *Expectations {
	t.Helper()

	e, err := parseExpectations(strings.NewReader(exps))
	if err != nil {
		t.Fatal(err)
	}

	return e
}

// ev builds one test2json line.
func ev(action, test, output string) string {
	s := `{"Action":"` + action + `","Package":"github.com/alibaba/OpenSandbox/tests/go"`
	if test != "" {
		s += `,"Test":"` + test + `"`
	}

	if output != "" {
		s += `,"Output":` + quote(output)
	}

	return s + "}\n"
}

func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

var two = []SuiteTest{{"TestA", "sandbox"}, {"TestSandbox_PauseAndResume", "sandbox"}}

func run(t *testing.T, stream string, expected []SuiteTest) Outcome {
	t.Helper()

	o, err := evaluate(strings.NewReader(stream), expected, mustExp(t), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	return o
}

func TestAllPassAndAnAllowedSkipIsGreen(t *testing.T) {
	o := run(t,
		ev("run", "TestA", "")+ev("pass", "TestA", "")+
			ev("output", "TestSandbox_PauseAndResume", "    sandbox_e2e_test.go:207: skip pause/resume e2e test\n")+
			ev("skip", "TestSandbox_PauseAndResume", "")+ev("pass", "", ""),
		two)

	if !o.Green() {
		t.Fatalf("expected green, got %+v", o.Rows)
	}
}

func TestAFailIsRed(t *testing.T) {
	o := run(t,
		ev("output", "TestA", "        Error:      \tReceived unexpected error:\n")+
			ev("output", "TestA", "        \t            \tconnection refused\n")+
			ev("fail", "TestA", "")+ev("fail", "", ""),
		two[:1])

	if o.Green() {
		t.Fatal("a failed test produced a green run")
	}

	if got := o.Rows[0].Note; !strings.Contains(got, "connection refused") {
		t.Fatalf("failure note should carry testify's message, got %q", got)
	}
}

func TestAnUnlistedSkipIsRed(t *testing.T) {
	o := run(t,
		ev("output", "TestA", "    x_test.go:9: PauseSandbox not supported in this environment\n")+
			ev("skip", "TestA", "")+ev("pass", "", ""),
		two[:1])

	if o.Green() {
		t.Fatal("a skip that no expectation allows produced a green run")
	}
}

// The allowance names a message, so the same test skipping for another reason is not covered.
func TestAListedTestSkippingForAnotherReasonIsRed(t *testing.T) {
	o := run(t,
		ev("output", "TestSandbox_PauseAndResume", "    sandbox_e2e_test.go:222: Pause not supported\n")+
			ev("skip", "TestSandbox_PauseAndResume", "")+ev("pass", "", ""),
		two[1:])

	if o.Green() {
		t.Fatal("a listed test skipping for a different reason produced a green run")
	}
}

func TestATestThatNeverReportedIsRed(t *testing.T) {
	o := run(t, ev("run", "TestA", "")+ev("pass", "TestA", "")+ev("pass", "", ""), two)

	if o.Green() {
		t.Fatal("an expected test with no result produced a green run")
	}

	if o.Rows[1].Status != "NORUN" {
		t.Fatalf("want NORUN, got %s", o.Rows[1].Status)
	}
}

// A panic or -timeout fails the package; tests that already passed must not hide it.
func TestAPackageFailureIsRedEvenIfEveryTestPassed(t *testing.T) {
	o := run(t, ev("pass", "TestA", "")+ev("output", "", "panic: test timed out after 30m0s\n")+ev("fail", "", ""), two[:1])

	if o.Green() {
		t.Fatal("a package-level failure produced a green run")
	}
}

func TestABuildFailureIsRed(t *testing.T) {
	stream := `{"ImportPath":"x","Action":"build-output","Output":"undefined: foo\n"}` + "\n" +
		`{"ImportPath":"x","Action":"build-fail"}` + "\n"

	if run(t, stream, two[:1]).Green() {
		t.Fatal("a build failure produced a green run")
	}
}

func TestAnEmptyStreamIsRed(t *testing.T) {
	if run(t, "", two[:1]).Green() {
		t.Fatal("no output at all produced a green run")
	}
}

func TestNothingExpectedIsRed(t *testing.T) {
	if run(t, ev("pass", "", ""), nil).Green() {
		t.Fatal("an empty selection produced a green run")
	}
}

// A subtest failing fails its parent in go test's accounting; the row must follow the parent.
func TestSubtestFailureFailsTheParentRow(t *testing.T) {
	o := run(t, ev("fail", "TestA/sub", "")+ev("fail", "TestA", "")+ev("fail", "", ""), two[:1])

	if o.Rows[0].Status != "FAIL" {
		t.Fatalf("want FAIL, got %s", o.Rows[0].Status)
	}
}

func TestAnAllowanceThatPassedIsReportedStale(t *testing.T) {
	o := run(t, ev("pass", "TestSandbox_PauseAndResume", "")+ev("pass", "", ""), two[1:])

	if !o.Green() || len(o.Stale) != 1 {
		t.Fatalf("want green with one stale allowance, got green=%v stale=%v", o.Green(), o.Stale)
	}
}

func TestExpectationsRejectABlanketSkip(t *testing.T) {
	_, err := parseExpectations(strings.NewReader("skip TestA |  | because\n"))
	if err == nil {
		t.Fatal("an empty message would allow every skip reason; it must be refused")
	}
}

func TestTiersAreCumulative(t *testing.T) {
	files, err := mustExp(t).FilesFor("v0.10.0")
	if err != nil {
		t.Fatal(err)
	}

	if strings.Join(files, ",") != "sandbox,command,volume" {
		t.Fatalf("got %v", files)
	}

	if _, err := mustExp(t).FilesFor("v9"); err == nil {
		t.Fatal("an unknown tier must be an error, not an empty selection")
	}
}

func TestListTestsRefusesAnUnknownFile(t *testing.T) {
	dir := t.TempDir()
	body := "package e2e\n\nfunc TestOne(t *testing.T) {}\nfunc helper() {}\nfunc TestTwo(t *testing.T) {}\n"

	if err := os.WriteFile(filepath.Join(dir, "sandbox_e2e_test.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := listTests(dir, []string{"sandbox"}, nil)
	if err != nil || len(got) != 2 || got[0].Name != "TestOne" || got[1].Name != "TestTwo" {
		t.Fatalf("got %v, %v", got, err)
	}

	if _, err := listTests(dir, []string{"sandbx"}, nil); err == nil {
		t.Fatal("a typo in --files must be an error, not a run of nothing")
	}

	if _, err := listTests(dir, []string{"sandbox"}, regexp.MustCompile("Nope")); err == nil {
		t.Fatal("a -run that selects nothing must be an error")
	}
}

// The committed expectations file must parse and name every tier the design promises.
func TestCommittedExpectationsParse(t *testing.T) {
	e, err := loadExpectations(filepath.Join("..", "..", "expectations"))
	if err != nil {
		t.Fatal(err)
	}

	files, err := e.FilesFor("v0.9.0")
	if err != nil {
		t.Fatal(err)
	}

	want := "sandbox,command,filesystem,manager,lifecycle_metrics,error_handling,concurrent,streaming_timeout"
	if strings.Join(files, ",") != want {
		t.Fatalf("v0.9.0 is %v, the design says %s", files, want)
	}
}

// "Not equal:" alone is useless in a table; the expected and actual lines are the finding.
func TestFailureNoteCarriesTestifyContinuationLines(t *testing.T) {
	out := []string{
		"    error_handling_e2e_test.go:40: ",
		"        \tError Trace:\t/x/error_handling_e2e_test.go:40",
		"        \tError:      \tNot equal: ",
		"        \t            \texpected: 404",
		"        \t            \tactual  : 501",
		"        \tTest:       \tTestError_XRequestIDPassthrough",
	}

	if got, want := failureLine(out), "Not equal: expected: 404 actual : 501"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
