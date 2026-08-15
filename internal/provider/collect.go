package provider

import (
	"context"
	"strings"
	"time"
)

// Orphans lists sbx volumes and snapshot images whose sandbox is gone.
//
// "Gone" means no container carries the sandbox label — not "stopped". A sleeping sandbox
// has no running container and every byte of its data still matters, which is the whole
// design; treating stopped as garbage would delete the branch somebody returns to.
func (d *dockerProvider) Orphans(ctx context.Context) ([]Artifact, error) {
	live, err := d.liveSandboxes()
	if err != nil {
		return nil, err
	}

	var out []Artifact

	vols, err := d.docker("volume", "ls", "--format", "{{.Name}}", "--filter", "name=sbx-")
	if err != nil {
		return nil, err
	}

	for _, name := range lines(vols) {
		a := Artifact{Kind: "volume", Name: name}

		switch {
		case strings.HasPrefix(name, "sbx-snapvol-"):
			a.Snapshot = true
		case strings.HasSuffix(name, "-data"):
			// sbx-<sandbox>-<service>-data. The service name may contain dashes and the
			// sandbox may too, so this is a prefix question rather than a split: a volume
			// belongs to a live sandbox if any live sandbox's prefix matches it.
			a.Sandbox = ownerOf(name, live)
			if a.Sandbox != "" {
				continue // its sandbox still exists
			}
		default:
			continue // not ours to judge
		}

		a.Age = d.ageOfVolume(ctx, name)
		out = append(out, a)
	}

	imgs, err := d.docker("images", "--format", "{{.Repository}}:{{.Tag}}\t{{.CreatedAt}}",
		"--filter", "reference=sbx-snap-*")
	if err != nil {
		return out, nil // volumes are still worth reporting
	}

	for _, line := range lines(imgs) {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}

		out = append(out, Artifact{
			Kind: "image", Name: parts[0], Snapshot: true, Age: sinceDockerTime(parts[1]),
		})
	}

	return out, nil
}

func (d *dockerProvider) Reclaim(_ context.Context, a Artifact) error {
	var err error

	switch a.Kind {
	case "volume":
		_, err = d.docker("volume", "rm", a.Name)
	case "image":
		_, err = d.docker("rmi", a.Name)
	}

	return err
}

// liveSandboxes is every sandbox with a container, running or not.
func (d *dockerProvider) liveSandboxes() (map[string]bool, error) {
	format := "{{.Label \"" + labelSandbox + "\"}}"

	out, err := d.docker("ps", "-a", "--format", format, "--filter", "label="+labelSandbox)
	if err != nil {
		return nil, err
	}

	live := map[string]bool{}
	for _, s := range lines(out) {
		if s != "" {
			live[s] = true
		}
	}

	return live, nil
}

func ownerOf(volume string, live map[string]bool) string {
	for sandbox := range live {
		if strings.HasPrefix(volume, "sbx-"+sandbox+"-") {
			return sandbox
		}
	}

	return ""
}

func (d *dockerProvider) ageOfVolume(_ context.Context, name string) time.Duration {
	out, err := d.docker("volume", "inspect", "-f", "{{.CreatedAt}}", name)
	if err != nil {
		return 0
	}

	return sinceDockerTime(strings.TrimSpace(out))
}

// sinceDockerTime parses the several shapes docker prints times in. An unparseable time
// reports as brand new, so an artifact is never swept on the strength of a date nobody
// could read.
func sinceDockerTime(s string) time.Duration {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05 -0700 MST", "2006-01-02T15:04:05Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return time.Since(t)
		}
	}

	return 0
}

func lines(s string) []string {
	var out []string

	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}

	return out
}
