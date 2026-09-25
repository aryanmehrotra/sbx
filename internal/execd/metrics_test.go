//go:build unix

package execd

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMetrics(t *testing.T) {
	s := newTestServer(t, Options{})

	code, _, body := s.do("GET", "/metrics", nil)
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}

	// Every field the spec marks required must be present, even at zero.
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}

	for _, k := range []string{"cpu_count", "cpu_used_pct", "mem_total_mib", "mem_used_mib", "timestamp"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing %s in %s", k, body)
		}
	}

	var m metricsBody
	_ = json.Unmarshal(body, &m)

	if m.CPUCount <= 0 {
		t.Errorf("cpu_count %v; upstream's e2e requires it non-zero", m.CPUCount)
	}

	if m.CPUUsedPct < 0 || m.CPUUsedPct > 100 || m.MemUsed < 0 || m.MemTotal < 0 {
		t.Errorf("out of range: %+v", m)
	}

	if d := time.Now().UnixMilli() - m.Timestamp; d < 0 || d > 60_000 {
		t.Errorf("timestamp %d is not now in unix milliseconds", m.Timestamp)
	}
}

// TestWatchMetricsFraming reads two events through the SDK's parser - which only dispatches an
// event at a blank line, so upstream's single-newline framing would deliver nothing here.
func TestWatchMetricsFraming(t *testing.T) {
	s := newTestServer(t, Options{})

	resp, err := http.Get(s.ts.URL + "/metrics/watch")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type %q", ct)
	}

	got := make(chan metricsBody, 4)

	go func() {
		_ = sdkStreamSSE(bufio.NewReader(resp.Body), func(e sdkStreamEvent) error {
			var m metricsBody
			if err := json.Unmarshal([]byte(e.Data), &m); err != nil || strings.Contains(e.Data, "\n") {
				t.Errorf("event %q is not one Metrics object", e.Data)
			}

			got <- m

			return nil
		})
	}()

	for i := range 2 {
		select {
		case m := <-got:
			if m.CPUCount <= 0 || m.Timestamp == 0 {
				t.Errorf("event %d: %+v", i, m)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("event %d did not arrive; the SDK parser saw no complete event", i)
		}
	}
}

func TestCPUPercent(t *testing.T) {
	now := time.Now()
	prev := &cpuReading{busy: time.Second, at: now}

	cases := []struct {
		busy time.Duration
		wall time.Duration
		cpus float64
		want float64
	}{
		{busy: 1500 * time.Millisecond, wall: time.Second, cpus: 1, want: 50},
		{busy: 2 * time.Second, wall: time.Second, cpus: 2, want: 50},
		{busy: 5 * time.Second, wall: time.Second, cpus: 1, want: 100},
		{busy: time.Second, wall: time.Second, cpus: 4, want: 0},
	}

	for _, c := range cases {
		cur := &cpuReading{busy: c.busy, at: now.Add(c.wall)}
		if got := cur.percentSince(prev, c.cpus); got != c.want {
			t.Errorf("%+v: got %v", c, got)
		}
	}
}
