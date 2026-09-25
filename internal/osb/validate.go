package osb

// Input rules that OpenSandbox states and sbx has to honour identically, because a client that
// works against one server and is refused by the other has not been given a compatible API.

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Metadata follows Kubernetes label rules upstream (it becomes labels there), so it does here
// too: a key or value accepted by sbx and refused by OpenSandbox would make code written against
// sbx fail on the day it moved.
var (
	dnsSubdomain = regexp.MustCompile(`^(?:[a-z0-9]([-a-z0-9]*[a-z0-9])?\.)*[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	labelName    = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
	labelValue   = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?$`)
)

const reservedPrefix = "opensandbox.io/"

func checkMetadataKey(key string) error {
	if strings.HasPrefix(key, reservedPrefix) {
		return fmt.Errorf("metadata key %q uses the reserved prefix %q, which is managed by the "+
			"server and cannot be set", key, reservedPrefix)
	}

	name := key
	if prefix, rest, ok := strings.Cut(key, "/"); ok {
		if prefix == "" || rest == "" || len(prefix) > 253 || !dnsSubdomain.MatchString(prefix) {
			return fmt.Errorf("metadata key %q has an invalid prefix: it must be a DNS subdomain "+
				"(lower-case, dots and dashes) up to 253 characters", key)
		}

		name = rest
	}

	if len(name) > 63 || !labelName.MatchString(name) {
		return fmt.Errorf("metadata key %q is invalid: the name part must be at most 63 "+
			"characters of letters, digits, '-', '_' or '.', starting and ending alphanumeric", key)
	}

	return nil
}

func checkMetadataValue(key, value string) error {
	if len(value) > 63 || !labelValue.MatchString(value) {
		return fmt.Errorf("metadata value %q for %q is invalid: at most 63 characters of letters, "+
			"digits, '-', '_' or '.', starting and ending alphanumeric", value, key)
	}

	return nil
}

func checkMetadata(m map[string]string) error {
	for k, v := range m {
		if err := checkMetadataKey(k); err != nil {
			return err
		}

		if err := checkMetadataValue(k, v); err != nil {
			return err
		}
	}

	return nil
}

// parseCPU reads a Kubernetes CPU quantity ("500m", "1", "0.5") into cores.
func parseCPU(s string) (float64, error) {
	t := strings.TrimSpace(s)

	scale := 1.0
	if strings.HasSuffix(t, "m") {
		scale, t = 0.001, strings.TrimSuffix(t, "m")
	}

	v, err := strconv.ParseFloat(t, 64)
	if err != nil || v <= 0 || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, fmt.Errorf("resourceLimits.cpu %q is not a CPU quantity - try \"500m\" or \"1\"", s)
	}

	return v * scale, nil
}

// parseMemory reads a Kubernetes memory quantity ("512Mi", "1Gi", "1G", "1048576") into bytes.
//
// Kubernetes spelling, not docker's. "512m" in a quantity is 0.512 BYTES, and the SDKs send
// Kubernetes quantities; reading it docker's way instead would turn that into half a gigabyte,
// which is a guess about what was meant. It is refused, naming the spelling they probably meant.
func parseMemory(s string) (uint64, error) {
	t := strings.TrimSpace(s)

	units := []struct {
		suffix string
		mult   float64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"Pi", 1 << 50},
		{"k", 1e3}, {"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"P", 1e15},
	}

	mult := 1.0

	for _, u := range units {
		if strings.HasSuffix(t, u.suffix) {
			mult, t = u.mult, strings.TrimSuffix(t, u.suffix)
			break
		}
	}

	if strings.HasSuffix(t, "m") {
		return 0, fmt.Errorf("resourceLimits.memory %q is millibytes in Kubernetes notation - "+
			"did you mean %sMi?", s, strings.TrimSuffix(t, "m"))
	}

	v, err := strconv.ParseFloat(t, 64)
	if err != nil || v <= 0 || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, fmt.Errorf("resourceLimits.memory %q is not a memory quantity - try \"512Mi\" or \"2Gi\"", s)
	}

	b := v * mult

	// Docker's floor, refused here with a message that names the field rather than by docker
	// with one about "minimum memory limit allowed" that reads like a bug in sbx.
	if b < 6<<20 {
		return 0, fmt.Errorf("resourceLimits.memory %q is below the 6Mi docker will accept", s)
	}

	return uint64(b), nil
}

// parsePorts reads extensions["sbx.ports"]: container ports to front directly rather than
// through execd's /proxy. A sandbox's slot has 20 ports and execd holds one.
func parsePorts(s string) ([]int, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}

	seen := map[int]bool{execdPort: true}

	var out []int

	for _, part := range strings.Split(s, ",") {
		p, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || p < 1 || p > 65535 {
			return nil, fmt.Errorf("extensions[\"sbx.ports\"] %q: %q is not a port", s, part)
		}

		if seen[p] {
			return nil, fmt.Errorf("extensions[\"sbx.ports\"] %q: port %d is listed twice or is "+
				"execd's own", s, p)
		}

		seen[p] = true
		out = append(out, p)
	}

	if len(out) > 19 {
		return nil, fmt.Errorf("extensions[\"sbx.ports\"] lists %d ports; a sandbox has room for 19 "+
			"besides execd - reach the rest through the /proxy/{port} endpoint", len(out))
	}

	return out, nil
}
