package devcontainer

import (
	"encoding/json"
	"testing"
)

// devcontainer.json is JSON with comments — the spec says so and VS Code's own templates ship
// with them, while encoding/json refuses all of it. The hard cases are all about context, which
// is why this is a scanner and not a regexp: a `//` inside a string is not a comment.
func TestJSONCKeepsWhatIsInsideStrings(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"line comment", "{\"a\":1} // trailing", `{"a":1} `},
		{"block comment", "{/* why */ \"a\":1}", `{ "a":1}`},
		{"trailing comma in object", `{"a":1,}`, `{"a":1}`},
		{"trailing comma in array", `{"a":[1,2,]}`, `{"a":[1,2]}`},
		{"comma then comment then close", "{\"a\":1, // x\n}", "{\"a\":1 \n}"},

		// The ones a regexp gets wrong.
		{"slashes inside a string", `{"url":"https://x.dev//y"}`, `{"url":"https://x.dev//y"}`},
		{"comment marker in a string", `{"a":"// not a comment"}`, `{"a":"// not a comment"}`},
		{"block marker in a string", `{"a":"/* nor this */"}`, `{"a":"/* nor this */"}`},
		{"comma in a string before a brace", `{"a":"x,"}`, `{"a":"x,"}`},
		{"escaped quote then slashes", `{"a":"say \" // no"}`, `{"a":"say \" // no"}`},
	} {
		if got := stripJSONC(tc.in); got != tc.want {
			t.Errorf("%s:\n  in   %q\n  got  %q\n  want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// Whatever it produces has to be readable by encoding/json, which is the only reason it exists.
func TestStrippedJSONCParses(t *testing.T) {
	src := `{
  // the image this dev container uses
  "name": "My Project",   /* display name */
  "image": "mcr.microsoft.com/devcontainers/go:1.22",
  "forwardPorts": [8080, "5432",],
  "containerEnv": { "URL": "https://x.dev//path" },
}`

	var got map[string]any
	if err := json.Unmarshal([]byte(stripJSONC(src)), &got); err != nil {
		t.Fatalf("stripped output does not parse: %v\n%s", err, stripJSONC(src))
	}

	if got["name"] != "My Project" {
		t.Errorf("name came through as %v", got["name"])
	}

	if env, ok := got["containerEnv"].(map[string]any); !ok || env["URL"] != "https://x.dev//path" {
		t.Errorf("a URL inside a string was mangled: %v", got["containerEnv"])
	}
}
