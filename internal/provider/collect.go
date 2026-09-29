package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Orphans lists sbx volumes and snapshot images whose sandbox is gone.
//
// "Gone" means no container carries the sandbox label - not "stopped". A sleeping sandbox
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
		a, orphan := classifyVolume(name, live)
		if !orphan {
			continue
		}

		a.Age = d.ageOfVolume(ctx, name)
		out = append(out, a)
	}

	imgs, err := d.docker("images", "--format", "{{.Repository}}:{{.Tag}}\t{{.CreatedAt}}",
		"--filter", "reference=sbx-snap-*")
	// On a failure volumes are still worth reporting, but not a snapshot volume: whether its
	// image is one a fork runs from is exactly what cannot be told without the images.
	noImages := err != nil

	for _, line := range lines(imgs) {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}

		out = append(out, Artifact{
			Kind: "image", Name: parts[0], Snapshot: true, Age: sinceDockerTime(parts[1]),
		})
	}

	// What a fork still runs from is not an orphan, whichever sandbox the snapshot came from.
	// Failing closed: a listing that cannot tell would offer a live fork's snapshot for --force.
	u, err := d.usage()
	if err != nil {
		return nil, fmt.Errorf("could not tell which images and volumes a sandbox still uses, so "+
			"nothing is offered: %w", err)
	}

	var names []string

	for _, a := range out {
		if a.Kind == "image" {
			names = append(names, a.Name)
		}
	}

	markInUse(out, d.imageMetas(names), u)

	if noImages {
		for i := range out {
			if out[i].Snapshot {
				out[i].InUse = true
			}
		}
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

// PVCVolumePrefix names the volumes the OpenSandbox API creates for a `pvc` volume. They are
// a caller's persistent storage, created on request and kept until that caller deletes them -
// no sandbox owns them, so "no live sandbox" says nothing about whether they are garbage.
const PVCVolumePrefix = "sbx-osb-pvc-"

// classifyVolume decides whether a volume is an orphan, and of what.
func classifyVolume(name string, live map[string]bool) (Artifact, bool) {
	a := Artifact{Kind: "volume", Name: name}

	switch {
	case strings.HasPrefix(name, PVCVolumePrefix):
		// Checked before the -data suffix: a claim named "x-data" would otherwise be read as
		// the data volume of a sandbox that does not exist, and swept.
		return a, false
	case strings.HasPrefix(name, "sbx-snapvol-"):
		a.Snapshot = true
	case strings.HasSuffix(name, "-data"):
		// sbx-<sandbox>-<service>-data. The service name may contain dashes and the
		// sandbox may too, so this is a prefix question rather than a split: a volume
		// belongs to a live sandbox if any live sandbox's prefix matches it.
		a.Sandbox = ownerOf(name, live)
		if a.Sandbox != "" {
			return a, false // its sandbox still exists
		}
	default:
		return a, false // not ours to judge
	}

	return a, true
}

// Labels a snapshot writes on its images; internal/cli writes them, this reads them to keep a
// snapshot whole. One home, so the two cannot drift apart.
const (
	SnapshotNameLabel   = "sbx.snapshot.name"
	SnapshotVolumeLabel = "sbx.snapshot.volume"
)

// unitUsage is what every container, running or stopped, uses: image IDs and volume names,
// each mapped to the sandboxes using it ("container <name>" for one sbx did not make).
type unitUsage struct {
	images  map[string][]string
	volumes map[string][]string
}

// usage asks docker what every container uses. Stopped ones count: a sleeping fork has no
// running container and still needs its image to wake.
//
// By image ID, not by the name a container was created with: `docker ps` prints the name only
// while the tag still points at the same image, and "sbx-snap-x-pg" and "sbx-snap-x-pg:latest"
// are one image under two spellings.
func (d *dockerProvider) usage() (unitUsage, error) {
	ids, err := d.docker("ps", "-aq", "--no-trunc")
	if err != nil {
		return unitUsage{}, err
	}

	if len(lines(ids)) == 0 {
		return unitUsage{}, nil
	}

	// The labels as JSON, not `index .Config.Labels`: docker's templates run over decoded
	// JSON, where an object with no labels has no Labels key and indexing it fails the whole
	// inspect - found against alpine:3, whose config carries none.
	format := "{{.Name}}\t{{.Image}}\t{{range .Mounts}}{{.Name}} {{end}}\t{{json .Config}}"

	args := append([]string{"inspect", "--type", "container", "--format", format}, lines(ids)...)

	out, err := d.docker(args...)
	if err != nil {
		// A container removed between the list and the inspect fails the whole inspect. Once
		// more, from a fresh list; a second failure is a real one.
		return d.usageOnce(format)
	}

	return parseUsage(out), nil
}

func (d *dockerProvider) usageOnce(format string) (unitUsage, error) {
	ids, err := d.docker("ps", "-aq", "--no-trunc")
	if err != nil {
		return unitUsage{}, err
	}

	if len(lines(ids)) == 0 {
		return unitUsage{}, nil
	}

	out, err := d.docker(append([]string{"inspect", "--type", "container", "--format", format}, lines(ids)...)...)
	if err != nil {
		return unitUsage{}, err
	}

	return parseUsage(out), nil
}

// parseUsage reads "name<TAB>imageID<TAB>vol vol <TAB>config JSON", one container per line.
func parseUsage(out string) unitUsage {
	u := unitUsage{images: map[string][]string{}, volumes: map[string][]string{}}

	add := func(m map[string][]string, key, user string) {
		if key != "" && !slices.Contains(m[key], user) {
			m[key] = append(m[key], user)
		}
	}

	for _, l := range strings.Split(out, "\n") {
		f := strings.Split(l, "\t")
		if len(f) != 4 {
			continue
		}

		user := configLabels(f[3])[labelSandbox]
		if user == "" {
			user = "container " + strings.TrimPrefix(strings.TrimSpace(f[0]), "/")
		}

		add(u.images, strings.TrimSpace(f[1]), user)

		for _, v := range strings.Fields(f[2]) {
			add(u.volumes, v, user)
		}
	}

	return u
}

// imageMeta is what gc needs to know about a snapshot image to keep its snapshot whole.
type imageMeta struct{ ID, Snapshot, Volume string }

// imageMetas inspects images, one call for all of them. An image that has gone since it was
// listed fails that call, so it falls back to one call each and leaves the missing ones out.
func (d *dockerProvider) imageMetas(names []string) map[string]imageMeta {
	out := map[string]imageMeta{}
	if len(names) == 0 {
		return out
	}

	// JSON for the same reason as in usage: an image with no labels has no Labels key, and an
	// image made before snapshots were labelled is exactly the one this must still read.
	format := "{{.Id}}\t{{json .Config}}"

	parse := func(name, line string) {
		id, config, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			return
		}

		labels := configLabels(config)
		out[name] = imageMeta{ID: id, Snapshot: labels[SnapshotNameLabel], Volume: labels[SnapshotVolumeLabel]}
	}

	all, err := d.docker(append([]string{"image", "inspect", "--format", format}, names...)...)
	if ls := strings.Split(all, "\n"); err == nil && len(ls) == len(names) {
		for i, n := range names {
			parse(n, ls[i])
		}

		return out
	}

	for _, n := range names {
		if one, err := d.docker("image", "inspect", "--format", format, n); err == nil {
			parse(n, one)
		}
	}

	return out
}

// markInUse sets InUse on every artifact a container still uses, and keeps each such snapshot
// whole: its other images and the volumes that go with them.
//
// Whole, not just the image a fork runs from. A sweep that took the unused half left a
// snapshot with some of its services, and a later fork of it started those from fresh
// volumes - the one outcome a fork must never have. An image nobody could inspect is kept too:
// not knowing is not the same as free, and the cost of the mistake is one run of gc.
func markInUse(arts []Artifact, meta map[string]imageMeta, u unitUsage) {
	used := map[string]bool{} // snapshot names with an image in use

	for i := range arts {
		a := &arts[i]
		if a.Kind != "image" {
			continue
		}

		m, ok := meta[a.Name]
		if !ok || len(u.images[m.ID]) > 0 {
			a.InUse = true

			if m.Snapshot != "" {
				used[m.Snapshot] = true
			}
		}
	}

	keep := map[string]bool{} // volumes that go with a kept image

	for i := range arts {
		a := &arts[i]
		if a.Kind != "image" {
			continue
		}

		m := meta[a.Name]
		if m.Snapshot != "" && used[m.Snapshot] {
			a.InUse = true
		}

		if !a.InUse {
			continue
		}

		// The conventional name as well as the label: an image made before labels names its
		// volume only by convention, sbx-snap-<x>:latest beside sbx-snapvol-<x>.
		keep["sbx-snapvol-"+strings.TrimSuffix(strings.TrimPrefix(a.Name, "sbx-snap-"), ":latest")] = true

		if m.Volume != "" && m.Volume != "none" {
			keep[m.Volume] = true
		}
	}

	for i := range arts {
		a := &arts[i]
		if a.Kind == "volume" && (len(u.volumes[a.Name]) > 0 || keep[a.Name]) {
			a.InUse = true
		}
	}
}

// InUse implements UsageFinder.
func (d *dockerProvider) InUse(_ context.Context, images, volumes []string) (map[string][]string, error) {
	u, err := d.usage()
	if err != nil {
		return nil, err
	}

	out := map[string][]string{}
	meta := d.imageMetas(images)

	for _, img := range images {
		m, ok := meta[img]
		if !ok {
			continue // not there, so nobody uses it
		}

		if users := u.images[m.ID]; len(users) > 0 {
			out[img] = users
		}
	}

	for _, v := range volumes {
		if users := u.volumes[v]; len(users) > 0 {
			out[v] = users
		}
	}

	return out, nil
}

var _ UsageFinder = (*dockerProvider)(nil)

// configLabels reads Labels out of an image's or container's config as `{{json .Config}}`
// prints it; nothing when there are none or it does not parse.
func configLabels(config string) map[string]string {
	var c struct {
		Labels map[string]string `json:"Labels"`
	}

	if json.Unmarshal([]byte(strings.TrimSpace(config)), &c) != nil || c.Labels == nil {
		return map[string]string{}
	}

	return c.Labels
}
