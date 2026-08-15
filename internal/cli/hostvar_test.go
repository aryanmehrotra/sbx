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

		// No recognisable port suffix: append rather than mangle. Nothing reads this, but
		// silently producing "_HOST" from a bare "PORT" would be worse than an odd name.
		"PORT":    "PORT_HOST",
		"GATEWAY": "GATEWAY_HOST",
	}

	for in, want := range cases {
		if got := hostVar(in); got != want {
			t.Errorf("hostVar(%q) = %q, want %q", in, got, want)
		}
	}
}
