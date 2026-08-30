package devcontainer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The stripper rewrites a file somebody else wrote, so the one thing it must never do is turn
// valid JSON into different valid JSON - a silent change to a configuration is worse than a
// refusal, because nothing reports it.
func FuzzStripJSONC(f *testing.F) {
	for _, s := range []string{
		`{"a":1}`, `{"a":1,}`, `{"a":[1,2,]}`, "{} // c", "{/* c */}",
		`{"a":"//"}`, `{"a":"/*"}`, `{"a":"\""}`, `{"a":"x,"}`, `{"a":"\\"}`,
		`{"a":"https://x//y"}`, "", "{", "}", `"`, `{"a":`, "[[[[", "\x00",
		`{"a":"","}`, "{\n// c\n}", "/*", "//", `{"":""}`,
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, src string) {
		out := stripJSONC(src)

		// Never longer: it only ever removes.
		if len(out) > len(src) {
			t.Errorf("stripJSONC grew the input: %d -> %d\n  in  %q\n  out %q",
				len(src), len(out), src, out)
		}

		// The property that matters. If the input was already valid JSON with no comments and
		// no trailing commas, the output must parse to exactly the same value - stripping must
		// be a no-op on the files it has nothing to do to.
		var before any
		if json.Unmarshal([]byte(src), &before) != nil {
			return // not valid to begin with; the stripper owes it nothing
		}

		var after any
		if err := json.Unmarshal([]byte(out), &after); err != nil {
			t.Fatalf("valid JSON stopped parsing after stripping: %v\n  in  %q\n  out %q",
				err, src, out)
		}

		b1, _ := json.Marshal(before)
		b2, _ := json.Marshal(after)

		if string(b1) != string(b2) {
			t.Errorf("stripping changed the VALUE of valid JSON\n  in   %q\n  out  %q\n"+
				"  was  %s\n  now  %s", src, out, b1, b2)
		}
	})
}

// Load reads a file from disk that a repository author wrote, so it takes whatever is there. It
// must return a usable service or an error, never a panic and never a spec that cannot be run.
func FuzzLoadDevcontainer(f *testing.F) {
	for _, s := range []string{
		`{"image":"alpine:3","forwardPorts":[8080]}`,
		`{"build":{"dockerfile":"D"},"forwardPorts":[1]}`,
		`{"image":"a","forwardPorts":["8080","127.0.0.1:9090",0,-1,99999]}`,
		`{"image":"a","postCreateCommand":{"x":["a","b"]}}`,
		`{"image":"a","mounts":[{"source":"/a","target":"/b"}]}`,
		`{"dockerComposeFile":"c.yml"}`, `{}`, `null`, `[]`, ``, `{"name":"‹›"}`,
	} {
		f.Add(s)
	}

	dir := f.TempDir()

	f.Fuzz(func(t *testing.T, body string) {
		path := filepath.Join(dir, "devcontainer.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}

		got, err := Load(path)
		if err != nil {
			return // refusing is always a valid answer
		}

		// Whatever it accepted, `sbx create` is about to be handed it.
		if got.Service == "" {
			t.Errorf("accepted a devcontainer with no service name: %q", body)
		}

		if strings.ContainsAny(got.Service, " /\\:\t\n") {
			t.Errorf("service name %q is not usable as a container name: %q", got.Service, body)
		}

		if got.Spec.Image == "" && got.Spec.Build == nil {
			t.Errorf("accepted a service with nothing to run: %q", body)
		}

		if len(got.Spec.Ports) == 0 {
			t.Errorf("accepted a service with no ports - nothing could reach it: %q", body)
		}

		for _, p := range got.Spec.Ports {
			if p < 1 || p > 65535 {
				t.Errorf("accepted port %d, which cannot be published: %q", p, body)
			}
		}
	})
}
