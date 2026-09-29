package cli

import "testing"

// The companion host variable for a declared port export.
//
// `PGHOST`/`PGPORT` is what libpq reads, so getting this right is the difference between the
// README's `psql` example connecting and quietly going to a local socket instead. The
// underscore form is the one most application config already uses.
func TestHostVar(t *testing.T) {
	cases := map[string]string{
		"DATABASE_PORT": "DATABASE_HOST",
		"REDIS_PORT":    "REDIS_HOST",
		"PGPORT":        "PGHOST",
		"MYSQL_PORT":    "MYSQL_HOST",
		"CDP_PORT":      "CDP_HOST",

		// No recognisable port suffix: append rather than mangle.
		"GATEWAY": "GATEWAY_HOST",
	}

	for in, want := range cases {
		if got, ok := hostVar(in); !ok || got != want {
			t.Errorf("hostVar(%q) = %q, %v, want %q", in, got, ok, want)
		}
	}
}

// SPEC.md: "A bare `PORT` gets none." It used to get PORT_HOST, a name nothing reads, set in
// the environment of every command `sbx env` wraps.
func TestABarePortExportHasNoHostCompanion(t *testing.T) {
	if got, ok := hostVar("PORT"); ok {
		t.Errorf("hostVar(\"PORT\") = %q, want no companion", got)
	}
}
