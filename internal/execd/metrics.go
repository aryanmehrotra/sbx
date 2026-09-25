//go:build unix

package execd

import (
	"encoding/json"
	"net/http"
	"runtime"
	"time"
)

// metricsBody is Metrics.
type metricsBody struct {
	CPUCount   float64 `json:"cpu_count"`
	CPUUsedPct float64 `json:"cpu_used_pct"`
	MemTotal   float64 `json:"mem_total_mib"`
	MemUsed    float64 `json:"mem_used_mib"`
	Timestamp  int64   `json:"timestamp"`
}

// cpuSample is how long /metrics watches the CPU to compute a percentage. Upstream samples for
// a full second (gopsutil's cpu.Percent), which makes every /metrics call take a second; a
// tenth is enough to tell a busy sandbox from an idle one.
var cpuSample = 100 * time.Millisecond

// readMetrics reports the sandbox's own limits where the kernel exposes them (the container's
// cgroup) and the machine's otherwise. cpu_count is GOMAXPROCS, as upstream: since Go 1.25 it
// already honours the cgroup's CPU quota, so a container limited to 2 CPUs reports 2, not the
// host's 64.
func readMetrics(prev *cpuReading) (metricsBody, *cpuReading) {
	cpus := float64(runtime.GOMAXPROCS(-1))

	m := metricsBody{CPUCount: cpus}

	now := readCPU()
	if prev != nil && now != nil {
		m.CPUUsedPct = now.percentSince(prev, cpus)
	}

	m.MemTotal, m.MemUsed = readMemory()
	m.Timestamp = time.Now().UnixMilli()

	return m, now
}

// cpuReading is cumulative CPU time consumed, at a wall-clock instant.
type cpuReading struct {
	busy time.Duration // CPU time used
	at   time.Time
}

// percentSince is usage between two readings as a share of all the CPUs the sandbox may use,
// 0-100 - the scale gopsutil's cpu.Percent(_, false) reports and upstream returns.
func (c *cpuReading) percentSince(prev *cpuReading, cpus float64) float64 {
	wall := c.at.Sub(prev.at)
	if wall <= 0 || cpus <= 0 {
		return 0
	}

	pct := float64(c.busy-prev.busy) / float64(wall) / cpus * 100

	return min(max(pct, 0), 100)
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	first := readCPU()

	select {
	case <-time.After(cpuSample):
	case <-r.Context().Done():
		return
	}

	m, _ := readMetrics(first)
	writeJSON(w, http.StatusOK, m)
}

// watchMetrics streams a Metrics object every second until the client leaves. Each is framed
// like an execd event - JSON and a blank line. Upstream ends each with a single newline, which
// every SSE parser, the OpenSandbox SDKs' included, reads as one ever-growing event that is
// only dispatched when the stream closes.
func (s *Server) watchMetrics(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	_ = rc.Flush()

	prev := readCPU()

	t := time.NewTicker(time.Second)
	defer t.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
		}

		var m metricsBody
		m, prev = readMetrics(prev)

		b, _ := json.Marshal(m)
		if _, err := w.Write(append(b, '\n', '\n')); err != nil {
			return
		}

		_ = rc.Flush()
	}
}
