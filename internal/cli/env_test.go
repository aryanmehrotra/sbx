package cli

import (
	"strings"
	"testing"
)

func TestShellFormat(t *testing.T) {
	cases := []struct{ shell, want string }{
		{"posix", `export DB_PORT='20002'`},
		{"bash", `export DB_PORT='20002'`},
		{"zsh", `export DB_PORT='20002'`},
		{"", `export DB_PORT='20002'`},
		{"fish", `set -gx DB_PORT '20002'`},
		{"powershell", `$env:DB_PORT = "20002"`},
		{"pwsh", `$env:DB_PORT = "20002"`},
		{"cmd", `set DB_PORT=20002`},
	}

	for _, c := range cases {
		got, err := shellFormat(c.shell, "DB_PORT", "20002")
		if err != nil {
			t.Fatalf("shellFormat(%q): %v", c.shell, err)
		}

		if got != c.want {
			t.Errorf("shellFormat(%q) = %q, want %q", c.shell, got, c.want)
		}
	}

	if _, err := shellFormat("nonsense", "A", "b"); err == nil {
		t.Error("an unknown shell must fail rather than emit something that looks right")
	}
}

// A host is a value from a spec, and a spec is a file someone edits. Quoting is what stops a
// stray character in it becoming a command.
func TestShellQuotingIsSafe(t *testing.T) {
	got, err := shellFormat("posix", "H", "a'; rm -rf /; echo '")
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(got, "; rm -rf /;") && !strings.Contains(got, `'\''`) {
		t.Errorf("unescaped shell metacharacters survived quoting: %s", got)
	}

	ps, err := shellFormat("powershell", "H", `a" ; rm x`)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(ps, `""`) {
		t.Errorf("powershell quote not doubled: %s", ps)
	}
}
