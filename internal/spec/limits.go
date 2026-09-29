package spec

import (
	"encoding/csv"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// The shape checks for cpu, memory and gpus.
//
// These were passed to the runtime verbatim and checked by nobody until create, which runs
// after the image pull: `"memory": "lots"` passed `sbx validate` and then failed in docker's
// words. The checks refuse only what no provider accepts. One spec file serves three
// providers with different spellings - docker's "512m", Kubernetes' "512Mi" and "500m" cores,
// Firecracker's own parser - so anything one of them takes still loads, and each provider
// still has the last word on its own dialect.
//
// Run from ParseSpec, not Service.validate: the OpenSandbox API builds these fields from its own
// already-parsed Kubernetes quantities (internal/osb/validate.go), and its gpus value is the
// caller's, which this change does not set out to start refusing with a 400.

// A size: a number, then an optional unit with an optional "i" and "b" ("512m", "1.5g", "512Mi",
// "512mb"), or a Kubernetes decimal exponent ("1e9"). A plain number is bytes. No sign, so zero -
// docker's "no limit" - is the smallest value that loads.
var memorySize = regexp.MustCompile(`^([0-9]+(\.[0-9]*)?|\.[0-9]+)([eE][+-]?[0-9]+| ?[kKmMgGtTpPeE]?[iI]?[bB]?)$`)

// The keys docker's --gpus takes.
var gpuKeys = map[string]bool{"count": true, "driver": true, "device": true, "capabilities": true, "options": true}

func checkLimits(name string, svc Service) error {
	if svc.CPU != "" {
		// Kubernetes millicores ("500m") as well as docker's cores ("0.5"). Zero is docker's
		// "no limit", so it loads.
		n, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(svc.CPU), "m"), 64)
		if err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
			return fmt.Errorf("service %q: cpu %q is not a number of cores - try \"0.5\" or \"2\"",
				name, svc.CPU)
		}
	}

	if svc.Memory != "" && !memorySize.MatchString(strings.TrimSpace(svc.Memory)) {
		return fmt.Errorf("service %q: memory %q is not a size - try \"512m\" or \"2g\"",
			name, svc.Memory)
	}

	if svc.GPUs != "" && !validGPUs(strings.TrimSpace(svc.GPUs)) {
		return fmt.Errorf("service %q: gpus %q is not \"all\", a count like \"1\", or docker's "+
			"key=value form like \"device=0\"", name, svc.GPUs)
	}

	return nil
}

// validGPUs follows docker's --gpus parser (probed against docker 29.5.2): CSV fields, each
// either key=value with a key docker knows or a bare "all" or integer, which docker reads as
// the count. That is why "device=0,1" loads: docker takes it as device 0 and a count of 1.
func validGPUs(g string) bool {
	fields, err := csv.NewReader(strings.NewReader(g)).Read()
	if err != nil {
		return false
	}

	for _, f := range fields {
		key, val, ok := strings.Cut(f, "=")
		if !ok {
			key, val = "count", f
		}

		if !gpuKeys[key] {
			return false
		}

		if key == "count" {
			if _, err := strconv.Atoi(val); err != nil && val != "all" {
				return false
			}
		}
	}

	return true
}
