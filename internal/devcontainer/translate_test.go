package devcontainer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// The three places the spec allows the file to live, because a repository that has one of them
// has already made this choice and should not have to make it again.
func TestItLooksWhereTheSpecSaysTheFileLives(t *testing.T) {
	body := `{"image":"alpine:3","forwardPorts":[8080]}`

	for _, name := range []string{
		".devcontainer/devcontainer.json",
		".devcontainer.json",
		"devcontainer.json",
	} {
		dir := t.TempDir()
		write(t, dir, name, body)

		got, err := Load(dir)
		if err != nil {
			t.Errorf("%s: %v", name, err)

			continue
		}

		if got.Spec.Image != "alpine:3" {
			t.Errorf("%s: image came through as %q", name, got.Spec.Image)
		}
	}
}

func TestATypicalDevcontainerTranslates(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".devcontainer/devcontainer.json", `{
  // a real-shaped file, comments and all
  "name": "My API",
  "image": "mcr.microsoft.com/devcontainers/go:1.22",
  "forwardPorts": [8080, "5432"],
  "containerEnv": { "GOFLAGS": "-mod=mod" },
  "remoteEnv": { "TOKEN": "x" },
  "workspaceFolder": "/workspaces/api",
  "postCreateCommand": "go mod download",
}`)

	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	if got.Service != "my-api" {
		t.Errorf("service name = %q, want my-api", got.Service)
	}

	if len(got.Spec.Ports) != 2 || got.Spec.Ports[0] != 8080 || got.Spec.Ports[1] != 5432 {
		t.Errorf("ports = %v, want [8080 5432] - a string port is legal in the spec", got.Spec.Ports)
	}

	if got.Spec.Env["GOFLAGS"] != "-mod=mod" || got.Spec.Env["TOKEN"] != "x" {
		t.Errorf("env = %v, want both containerEnv and remoteEnv", got.Spec.Env)
	}

	if got.Spec.Mounts["."] != "/workspaces/api" {
		t.Errorf("workspace mounted at %q, want the workspaceFolder", got.Spec.Mounts["."])
	}

	if len(got.Spec.Init) != 1 || got.Spec.Init[0] != "go mod download" {
		t.Errorf("init = %v, want the postCreateCommand", got.Spec.Init)
	}
}

// The hooks that run once map to `init`, in the order the spec fixes them. Getting the order
// wrong means running a build before the thing it builds has been fetched.
func TestTheOnceHooksKeepTheirOrder(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "devcontainer.json", `{
  "image": "alpine:3", "forwardPorts": [1],
  "postCreateCommand": "third",
  "onCreateCommand": "first",
  "updateContentCommand": "second"
}`)

	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"first", "second", "third"}
	if len(got.Spec.Init) != 3 {
		t.Fatalf("init = %v, want %v", got.Spec.Init, want)
	}

	for i := range want {
		if got.Spec.Init[i] != want[i] {
			t.Errorf("init[%d] = %q, want %q (order is onCreate, updateContent, postCreate)",
				i, got.Spec.Init[i], want[i])
		}
	}
}

// A hook may be a string, an argument array, or an object of named commands. All three are legal
// and a repository in the wild uses each.
func TestAHookCanBeWrittenThreeWays(t *testing.T) {
	for _, tc := range []struct {
		name, hook string
		want       int
	}{
		{"string", `"go build ./..."`, 1},
		{"array", `["go","build","./..."]`, 1},
		{"object", `{"a":"one","b":"two"}`, 2},
		{"empty string", `""`, 0},
		{"empty object", `{}`, 0},
	} {
		dir := t.TempDir()
		write(t, dir, "devcontainer.json",
			`{"image":"alpine:3","forwardPorts":[1],"postCreateCommand":`+tc.hook+`}`)

		got, err := Load(dir)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)

			continue
		}

		if len(got.Spec.Init) != tc.want {
			t.Errorf("%s: init = %v, want %d command(s)", tc.name, got.Spec.Init, tc.want)
		}
	}
}

// What it cannot carry across, it names. A partial import that stays quiet is how somebody spends
// an afternoon on a missing tool that a Feature was supposed to install.
func TestItSaysWhatItDropped(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "devcontainer.json", `{
  "image": "alpine:3",
  "forwardPorts": [8080],
  "features": { "ghcr.io/devcontainers/features/node:1": {} },
  "postStartCommand": "echo every start",
  "remoteUser": "vscode",
  "mounts": ["source=/a,target=/b,type=bind"]
}`)

	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(got.Dropped, "\n")
	for _, want := range []string{"Feature", "node", "postStartCommand", "remoteUser", "/b"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the dropped list does not mention %q:\n%s", want, joined)
		}
	}
}

// A devcontainer that forwards nothing is normal - an editor attaches over the docker socket -
// but every sbx service is reached by opening a socket, so this is the one thing the import has
// to invent, and it has to say so.
func TestADevcontainerWithNoPortsGetsOneAndIsToldSo(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "devcontainer.json", `{"image":"alpine:3"}`)

	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(got.Spec.Ports) != 1 || got.Spec.Ports[0] != 22 {
		t.Errorf("ports = %v, want [22]", got.Spec.Ports)
	}

	if !strings.Contains(strings.Join(got.Dropped, " "), "22") {
		t.Errorf("inventing a port was not reported: %v", got.Dropped)
	}
}

// Compose describes several services and how they connect, which is what sandbox.json is for -
// importing one service out of it would mean guessing which one is being developed.
func TestComposeIsRefusedRatherThanGuessedAt(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "devcontainer.json",
		`{"dockerComposeFile":"docker-compose.yml","service":"app"}`)

	_, err := Load(dir)
	if err == nil {
		t.Fatal("a compose-based devcontainer was imported anyway")
	}

	if !strings.Contains(err.Error(), "sandbox.json") {
		t.Errorf("the refusal does not say what to do instead: %v", err)
	}
}

func TestABuildIsCarriedAcross(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "devcontainer.json",
		`{"build":{"dockerfile":"Dockerfile.dev","context":".."},"forwardPorts":[3000]}`)

	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	if got.Spec.Build == nil || got.Spec.Build.Dockerfile != "Dockerfile.dev" ||
		got.Spec.Build.Context != ".." {
		t.Errorf("build = %+v", got.Spec.Build)
	}
}

// Neither an image nor a build means there is nothing to run, which is worth saying plainly
// rather than producing a spec that fails at create.
func TestNothingToRunIsRefused(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "devcontainer.json", `{"name":"empty","forwardPorts":[1]}`)

	if _, err := Load(dir); err == nil {
		t.Fatal("a devcontainer with no image and no build was accepted")
	}
}

// The display name is free text and a service name is not, so it is converted rather than
// rejected - somebody's "My API (v2)" should not be an error.
func TestADisplayNameBecomesAUsableServiceName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"My API", "my-api"},
		{"My API (v2)", "my-api-v2"},
		{"go_service", "go-service"},
		{"  ", "dev"},
		{"", "dev"},
		{"---", "dev"},
		{"9lives", "9lives"},
	} {
		if got := serviceName(tc.in); got != tc.want {
			t.Errorf("serviceName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
