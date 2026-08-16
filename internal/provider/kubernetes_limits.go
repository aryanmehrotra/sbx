package provider

// Ceilings, in a cluster.
//
// The same capability as the docker provider's, and the same words on screen, but almost
// nothing is shared underneath - which is the point of Limiter being an interface rather than
// four methods on Provider.
//
// Two differences are worth knowing before reading the rest:
//
//   - A cluster CAN remove a limit. Docker's update endpoint reads a zero as "leave this
//     alone", so a container that has a ceiling keeps it until it is recreated; a deployment's
//     resources are just a field, and a field can be deleted.
//   - Changing them here rolls the pod. Docker adjusts a running container's cgroup in place,
//     whereas a Deployment's pod template is immutable once a pod exists from it, so the
//     kubelet replaces the pod. The service is briefly unavailable and its memory is lost.
//     Callers that care are told, because a restart nobody mentioned is a bug report.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// kubeContainer is the name every sbx deployment gives its container - see deployment(). A
// strategic merge patch matches list entries by name, so patching resources needs it.
const kubeContainer = "app"

// Limits reads what one deployment allows its container. An absent field is no ceiling, which
// is the ordinary case and not an error.
func (k *kubeProvider) Limits(_ context.Context, ref string) (Limits, error) {
	const path = "{.spec.template.spec.containers[0].resources.limits"

	out, err := k.kc("", "get", "deployment", ref, "-o",
		"jsonpath="+path+".cpu}|"+path+".memory}")
	if err != nil {
		return Limits{}, fmt.Errorf("reading limits of %s: %w", ref, err)
	}

	cpu, mem, _ := strings.Cut(strings.TrimSpace(out), "|")

	var l Limits

	if cpu = strings.TrimSpace(cpu); cpu != "" {
		cores, err := parseKubeCPU(cpu)
		if err != nil {
			return Limits{}, err
		}

		l.NanoCPUs = int64(cores * 1e9)
	}

	if mem = strings.TrimSpace(mem); mem != "" {
		b, err := parseKubeMemory(mem)
		if err != nil {
			return Limits{}, err
		}

		l.MemBytes = b
	}

	return l, nil
}

// SetLimits patches the deployment's container resources.
//
// Both keys are always sent, null for a ceiling that is being removed. A strategic merge patch
// merges maps, so omitting a key leaves whatever was there - which would make "clear the cpu
// limit" silently mean "keep it", the exact failure docker has and this backend does not.
func (k *kubeProvider) SetLimits(_ context.Context, ref string, l Limits) error {
	limits := map[string]any{"cpu": nil, "memory": nil}

	if l.NanoCPUs > 0 {
		limits["cpu"] = kubeCPU(l.NanoCPUs)
	}

	if l.MemBytes > 0 {
		limits["memory"] = kubeMemory(l.MemBytes)
	}

	patch := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{map[string]any{
						"name":      kubeContainer,
						"resources": map[string]any{"limits": limits},
					}},
				},
			},
		},
	}

	raw, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	if _, err := k.kc("", "patch", "deployment", ref, "--type=strategic", "-p", string(raw)); err != nil {
		return fmt.Errorf("setting limits on %s: %w", ref, err)
	}

	return nil
}

// kubeCPU writes a core count the way a cluster wants it: millicores, which is the unit that
// survives a round trip without a fraction turning into something else.
func kubeCPU(nanoCPUs int64) string {
	return strconv.FormatInt(nanoCPUs/1_000_000, 10) + "m"
}

// kubeMemory writes a byte count as a binary quantity where it divides exactly, and as plain
// bytes where it does not. "536870912" is uglier than "512Mi" and never wrong.
func kubeMemory(b uint64) string {
	switch {
	case b%(1<<30) == 0:
		return strconv.FormatUint(b/(1<<30), 10) + "Gi"
	case b%(1<<20) == 0:
		return strconv.FormatUint(b/(1<<20), 10) + "Mi"
	case b%(1<<10) == 0:
		return strconv.FormatUint(b/(1<<10), 10) + "Ki"
	default:
		return strconv.FormatUint(b, 10)
	}
}

// parseKubeCPU reads "500m", "2" or "0.5" as a number of cores.
func parseKubeCPU(s string) (float64, error) {
	if m, ok := strings.CutSuffix(s, "m"); ok {
		milli, err := strconv.ParseFloat(m, 64)
		if err != nil {
			return 0, fmt.Errorf("cpu limit %q is not a quantity kubernetes would have written", s)
		}

		return milli / 1000, nil
	}

	cores, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("cpu limit %q is not a quantity kubernetes would have written", s)
	}

	return cores, nil
}

// kubeSuffixes are the quantity suffixes kubernetes accepts, binary and decimal. Both exist
// and they are not the same: 1G is a thousand million bytes and 1Gi is 1073741824, and a
// dashboard that showed one as the other would be wrong by seven percent.
var kubeSuffixes = []struct {
	suffix string
	mult   float64
}{
	{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
	{"k", 1e3}, {"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12},
}

// parseKubeMemory reads "512Mi", "2Gi", "512M" or a plain byte count.
func parseKubeMemory(s string) (uint64, error) {
	for _, u := range kubeSuffixes {
		digits, ok := strings.CutSuffix(s, u.suffix)
		if !ok {
			continue
		}

		n, err := strconv.ParseFloat(digits, 64)
		if err != nil {
			break
		}

		return uint64(n * u.mult), nil
	}

	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("memory limit %q is not a quantity kubernetes would have written", s)
	}

	return n, nil
}
