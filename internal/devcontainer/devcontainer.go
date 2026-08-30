package devcontainer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/spec"
)

// File is the subset of devcontainer.json sbx can act on.
//
// A subset on purpose. The spec is large and most of it describes how an EDITOR should behave -
// which extensions to install, which settings to apply, what to do on attach - and sbx is not an
// editor. What it can use is the part that describes the container: what to run, what to publish,
// what to mount, and what to do once it is up.
type File struct {
	Name string `json:"name"`

	Image string `json:"image"`

	Build *struct {
		Dockerfile string `json:"dockerfile"`
		Context    string `json:"context"`
	} `json:"build"`

	// ForwardPorts is the current field. appPort is the legacy one and is read too, because a
	// repository that has not been touched in three years is exactly the one being imported.
	ForwardPorts []json.RawMessage `json:"forwardPorts"`
	AppPort      []json.RawMessage `json:"appPort"`

	ContainerEnv map[string]string `json:"containerEnv"`
	RemoteEnv    map[string]string `json:"remoteEnv"`

	Mounts []json.RawMessage `json:"mounts"`

	WorkspaceFolder string `json:"workspaceFolder"`

	// The lifecycle hooks sbx can honour. Each may be a string, an array, or an object of
	// named commands, which is why they are raw until asked for.
	OnCreateCommand   json.RawMessage `json:"onCreateCommand"`
	PostCreateCommand json.RawMessage `json:"postCreateCommand"`
	UpdateContentCmd  json.RawMessage `json:"updateContentCommand"`
	PostStartCommand  json.RawMessage `json:"postStartCommand"`
	PostAttachCommand json.RawMessage `json:"postAttachCommand"`
	InitializeCommand json.RawMessage `json:"initializeCommand"`

	// What sbx cannot honour, read only so the import can say it was dropped.
	Features         map[string]json.RawMessage `json:"features"`
	DockerComposeFil json.RawMessage            `json:"dockerComposeFile"`
	RemoteUser       string                     `json:"remoteUser"`
}

// Result is a translated devcontainer, plus what could not be carried across.
type Result struct {
	Service string
	Spec    spec.Service

	// Dropped is every part of the file sbx ignored, in the reader's words rather than the
	// field's. An import that stays quiet about what it dropped is how somebody ends up
	// debugging a missing tool that a Feature was supposed to install.
	Dropped []string
}

// Load reads a devcontainer.json and translates it.
//
// path may be the file, or a directory containing `.devcontainer/devcontainer.json` or
// `.devcontainer.json` - the three places the spec says it can live.
func Load(path string) (*Result, error) {
	file, err := find(path)
	if err != nil {
		return nil, err
	}

	body, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}

	var dc File
	if err := json.Unmarshal([]byte(stripJSONC(string(body))), &dc); err != nil {
		return nil, fmt.Errorf("reading %s: %w", file, err)
	}

	return dc.translate(filepath.Dir(file))
}

// find locates the devcontainer.json in the three places the spec allows.
func find(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}

	if !info.IsDir() {
		return path, nil
	}

	for _, candidate := range []string{
		filepath.Join(path, ".devcontainer", "devcontainer.json"),
		filepath.Join(path, ".devcontainer.json"),
		filepath.Join(path, "devcontainer.json"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("no devcontainer.json under %s - looked in .devcontainer/, "+
		".devcontainer.json and devcontainer.json", path)
}

func (dc File) translate(dir string) (*Result, error) {
	out := &Result{Service: serviceName(dc.Name)}

	switch {
	case dc.Image != "":
		out.Spec.Image = dc.Image

	case dc.Build != nil:
		context := dc.Build.Context
		if context == "" {
			context = "."
		}

		out.Spec.Build = &spec.Build{Context: context, Dockerfile: dc.Build.Dockerfile}

	case len(dc.DockerComposeFil) > 0:
		// Compose describes several services and their wiring, which is sandbox.json's own job
		// - translating it would mean guessing which service is the one being developed.
		return nil, fmt.Errorf("this devcontainer uses dockerComposeFile, which describes " +
			"several services and how they connect - that is what sandbox.json does, so write " +
			"the services out rather than importing one of them")

	default:
		return nil, fmt.Errorf("the devcontainer names neither an image nor a build, so there " +
			"is nothing to run")
	}

	out.Spec.Ports = ports(dc.ForwardPorts, dc.AppPort)

	// Every service needs a port, because opening a socket is how everything in sbx is reached.
	// A devcontainer that forwards nothing is normal - an editor attaches over the docker
	// socket - so this is the one place the import has to invent something, and it says so.
	if len(out.Spec.Ports) == 0 {
		out.Spec.Ports = []int{22}
		out.Dropped = append(out.Dropped,
			"it forwards no ports, so 22 was added - the port `sbx ssh` looks for")
	}

	out.Spec.Env = mergeEnv(dc.ContainerEnv, dc.RemoteEnv)

	// The workspace, mounted from wherever the spec is written.
	folder := dc.WorkspaceFolder
	if folder == "" {
		folder = "/workspaces/" + out.Service
	}

	out.Spec.Mounts = map[string]string{".": folder}

	for _, m := range dc.Mounts {
		if s := mountString(m); s != "" {
			out.Dropped = append(out.Dropped, "a mount sbx did not translate: "+s)
		}
	}

	// The hooks that run once, in the order the spec fixes them.
	for _, h := range []struct {
		name string
		raw  json.RawMessage
	}{
		{"onCreateCommand", dc.OnCreateCommand},
		{"updateContentCommand", dc.UpdateContentCmd},
		{"postCreateCommand", dc.PostCreateCommand},
	} {
		out.Spec.Init = append(out.Spec.Init, commands(h.raw)...)
	}

	for _, h := range []struct {
		name string
		raw  json.RawMessage
	}{
		{"postStartCommand", dc.PostStartCommand},
		{"postAttachCommand", dc.PostAttachCommand},
		{"initializeCommand", dc.InitializeCommand},
	} {
		if len(commands(h.raw)) > 0 {
			out.Dropped = append(out.Dropped, h.name+" runs on every start or attach, which sbx "+
				"has no equivalent for - `init` runs once")
		}
	}

	if len(dc.Features) > 0 {
		names := make([]string, 0, len(dc.Features))
		for f := range dc.Features {
			names = append(names, f)
		}

		sort.Strings(names)

		out.Dropped = append(out.Dropped, fmt.Sprintf(
			"%d Feature(s) sbx does not install (%s) - bake them into the image, or add them to init",
			len(names), strings.Join(names, ", ")))
	}

	if dc.RemoteUser != "" {
		out.Dropped = append(out.Dropped, "remoteUser "+dc.RemoteUser+
			" - pass it to `sbx ssh --user` rather than the spec")
	}

	return out, nil
}

// serviceName turns a devcontainer's display name into something sandbox.json accepts.
func serviceName(name string) string {
	var b strings.Builder

	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '_' || r == '-' || r == '.':
			b.WriteByte('-')
		}
	}

	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}

	// A name has to start with a letter or a digit, and "dev" is what the thing is.
	if out == "" || !isAlnum(rune(out[0])) {
		return "dev"
	}

	return out
}

func isAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}

// ports reads forwardPorts and the legacy appPort, both of which may hold numbers or strings
// like "8080" and "127.0.0.1:8080".
func ports(lists ...[]json.RawMessage) []int {
	seen := map[int]bool{}

	var out []int

	for _, list := range lists {
		for _, raw := range list {
			p, ok := port(raw)
			if !ok || seen[p] {
				continue
			}

			seen[p] = true
			out = append(out, p)
		}
	}

	return out
}

func port(raw json.RawMessage) (int, bool) {
	var n int
	if json.Unmarshal(raw, &n) == nil {
		return n, n > 0 && n < 65536
	}

	var s string
	if json.Unmarshal(raw, &s) != nil {
		return 0, false
	}

	// "host:port" is legal here, and the port is the part sbx wants.
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[i+1:]
	}

	n, err := strconv.Atoi(strings.TrimSpace(s))

	return n, err == nil && n > 0 && n < 65536
}

// mountString renders a mount for the dropped list, whether it was written as a string or an
// object.
func mountString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}

	var o struct {
		Source string `json:"source"`
		Target string `json:"target"`
	}

	if json.Unmarshal(raw, &o) == nil && o.Target != "" {
		return o.Source + " -> " + o.Target
	}

	return ""
}

// commands reads a lifecycle hook, which the spec allows to be a string, an array of arguments,
// or an object of named commands run in parallel.
func commands(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}

	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return nil
		}

		return []string{s}
	}

	var list []string
	if json.Unmarshal(raw, &list) == nil {
		if len(list) == 0 {
			return nil
		}

		// An array is one command and its arguments, not several commands.
		return []string{strings.Join(list, " ")}
	}

	var named map[string]json.RawMessage
	if json.Unmarshal(raw, &named) == nil {
		keys := make([]string, 0, len(named))
		for k := range named {
			keys = append(keys, k)
		}

		// Sorted, because the spec says these run in parallel and a map does not have an
		// order - so any order is arbitrary and only a stable one is reproducible.
		sort.Strings(keys)

		var out []string
		for _, k := range keys {
			out = append(out, commands(named[k])...)
		}

		return out
	}

	return nil
}

func mergeEnv(maps ...map[string]string) map[string]string {
	out := map[string]string{}

	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}
