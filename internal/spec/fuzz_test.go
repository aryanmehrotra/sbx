package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sandbox.json is the one file a person writes by hand, so it is the one most likely to be
// wrong in a way nobody predicted - and it is also what an agent generates, which makes the
// input space wider than anything a table test covers.
//
// The contract is total: for ANY bytes, LoadSpec returns a usable Spec or an error. It must
// not panic, and it must not return a Spec that later code will trust and then trip over -
// so anything it accepts is checked here for the invariants the rest of sbx assumes.
func FuzzLoadSpec(f *testing.F) {
	f.Add(`{"version":1,"services":{"db":{"image":"postgres:16","ports":[5432]}}}`)
	f.Add(`{"version":1,"services":{"a":{"image":"x","ports":[1],"depends_on":["b"]},` +
		`"b":{"image":"y","ports":[2]}}}`)
	f.Add(`{"version":1,"services":{"db":{"build":{"context":"."},"ports":[5432]}}}`)
	f.Add(`{"version":1,"services":{"db":{"image":"x","ports":[5432],"idle":"30m",` +
		`"egress":"deny","cpu":"0.5","memory":"512m"}}}`)
	// The shapes that should be refused rather than crash.
	f.Add(`{"version":1,"services":{}}`)
	f.Add(`{"version":99,"services":{"a":{"image":"x","ports":[1]}}}`)
	f.Add(`{"version":1,"services":{"a":{"image":"x","build":{"context":"."},"ports":[1]}}}`)
	f.Add(`{"version":1,"services":{"a":{"image":"x","ports":[0]}}}`)
	f.Add(`{"version":1,"services":{"a":{"image":"x","ports":[99999999]}}}`)
	f.Add(`{"version":1,"services":{"a":{"image":"x","ports":[1],"depends_on":["a"]}}}`)
	f.Add(`{"version":1,"services":{"a":{"image":"x","ports":[1],"depends_on":["nope"]}}}`)
	f.Add(`{"version":1,"services":{"-bad":{"image":"x","ports":[1]}}}`)
	f.Add(`{}`)
	f.Add(``)
	f.Add(`null`)
	f.Add(`[]`)
	f.Add(`{"version":1,"services":{"a":{"image":"x","ports":[1],"idle":"not-a-duration"}}}`)

	dir := f.TempDir()

	f.Fuzz(func(t *testing.T, body string) {
		path := filepath.Join(dir, "sandbox.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}

		sp, err := LoadSpec(path)
		if err != nil {
			return // refusing is always a valid answer
		}

		if sp == nil {
			t.Fatal("LoadSpec returned no spec and no error")
		}

		// Whatever it accepted, the rest of sbx will now assume all of this.
		if len(sp.Services) == 0 {
			t.Errorf("accepted a spec with no services: %q", body)
		}

		for name, svc := range sp.Services {
			if name == "" {
				t.Errorf("accepted a service with an empty name: %q", body)
			}

			if len(svc.Ports) == 0 {
				t.Errorf("service %q accepted with no ports - every service is reached by "+
					"opening a socket: %q", name, body)
			}

			for _, p := range svc.Ports {
				if p < 1 || p > 65535 {
					t.Errorf("service %q accepted port %d, which cannot be published: %q",
						name, p, body)
				}
			}

			if strings.TrimSpace(svc.Image) == "" && svc.Build == nil {
				t.Errorf("service %q has neither image nor build, so there is nothing to "+
					"run: %q", name, body)
			}

			if strings.TrimSpace(svc.Image) != "" && svc.Build != nil {
				t.Errorf("service %q has both image and build, which the spec calls an "+
					"error rather than a precedence rule: %q", name, body)
			}

			// A dependency that resolves to nothing would be woken as nothing and the
			// dependent would come up to `no such host`.
			for _, dep := range svc.DependsOn {
				if _, ok := sp.Services[dep]; !ok {
					t.Errorf("service %q depends on %q, which the spec does not declare: %q",
						name, dep, body)
				}

				if dep == name {
					t.Errorf("service %q depends on itself: %q", name, body)
				}
			}
		}

		// Ordering must be derivable from anything that loaded, or create cannot start.
		if _, err := sp.CreationOrder(); err != nil {
			t.Errorf("a spec that loaded has no creation order (%v): %q", err, body)
		}
	})
}

// Environment expansion reads values from outside the file, so its input is not the author's.
// A ${VAR} that is unset must be refused rather than expanded to nothing - a database URL
// silently missing its password is the failure this exists to prevent.
func FuzzExpandEnv(f *testing.F) {
	for _, s := range []string{
		`{"version":1,"services":{"a":{"image":"x","ports":[1],"env":{"K":"${FOO}"}}}}`,
		`{"version":1,"services":{"a":{"image":"x","ports":[1],"env":{"K":"$FOO"}}}}`,
		`{"version":1,"services":{"a":{"image":"x","ports":[1],"env":{"K":"${}"}}}}`,
		`{"version":1,"services":{"a":{"image":"x","ports":[1],"env":{"K":"${FOO"}}}}`,
		`{"version":1,"services":{"a":{"image":"x","ports":[1],"env":{"K":"${FOO}${BAR}"}}}}`,
		`{"version":1,"services":{"a":{"image":"x","ports":[1],"env":{"K":"$${FOO}"}}}}`,
		`{"version":1,"services":{"a":{"image":"x","ports":[1],"env":{"K":"a${FOO}b"}}}}`,
	} {
		f.Add(s, "SET")
	}

	dir := f.TempDir()

	f.Fuzz(func(t *testing.T, body, value string) {
		path := filepath.Join(dir, "sandbox.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}

		sp, err := LoadSpec(path)
		if err != nil {
			return
		}

		// Every declared variable resolves, or the whole load fails. A partially expanded
		// value reaching a container is the outcome with no acceptable version.
		err = sp.expandEnv(func(string) (string, bool) { return value, true })
		if err != nil {
			return
		}

		for name, svc := range sp.Services {
			for k, v := range svc.Env {
				// Only a WELL-FORMED reference. The regex deliberately matches ${NAME} and
				// nothing else, so `${}` and an unterminated `${FOO` are literals by design -
				// braces are what make the boundary unambiguous, and a password containing a
				// literal $ must not silently become a substitution.
				if envRef.MatchString(v) {
					t.Errorf("service %q env %q still holds an unexpanded reference %q "+
						"after a successful expand: %q", name, k, v, body)
				}
			}
		}
	})
}
