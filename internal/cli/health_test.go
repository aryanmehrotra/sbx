package cli

import "testing"

// The message a user sees when their health command is not in the image.
//
// This used to be two minutes of nothing followed by "never became ready within 2m0s". The
// exit code was available on the very first probe - 127 is "not found" and no amount of
// waiting changes it - and was thrown away. Three docs name this as the most common first-run
// failure, which made it the one place the tool said least.
func TestShellReasonKeepsOnlyTheCommandsComplaint(t *testing.T) {
	cases := map[string]string{
		// What the provider actually produces: the command line it built, the exit status,
		// then the output. Only the last part is the user's business.
		`docker exec sbx-b-web sh -c pg_isready -U app: exit status 127: sh: pg_isready: not found`: "sh: pg_isready: not found",
		`docker exec x sh -c ./run: exit status 126: sh: ./run: Permission denied`:                  "sh: ./run: Permission denied",

		// No exit status to anchor on: keep it whole rather than mangling it.
		`something else entirely`: "something else entirely",
		``:                        "no output",
	}

	for in, want := range cases {
		if got := shellReason(in); got != want {
			t.Errorf("shellReason(%q)\n  = %q\n want %q", in, got, want)
		}
	}
}

func TestFirstLineIsReadable(t *testing.T) {
	if got := firstLine("one\ntwo\nthree"); got != "one" {
		t.Errorf("firstLine = %q, want \"one\"", got)
	}

	if got := firstLine("   \n  "); got != "no output" {
		t.Errorf("empty input = %q, want \"no output\"", got)
	}

	long := make([]byte, 400)
	for i := range long {
		long[i] = 'x'
	}

	if got := firstLine(string(long)); len(got) > 210 {
		t.Errorf("a 400-character line was not truncated: %d characters", len(got))
	}
}
