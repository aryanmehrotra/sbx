package osb

import (
	"strings"
	"testing"
	"time"
)

// poolMisses are the "pool miss" log lines written since mark.
func poolMisses(mark int) []string {
	sink.mu.Lock()
	defer sink.mu.Unlock()

	var out []string

	for _, e := range sink.lines[mark:] {
		if strings.Contains(e.Message, "pool miss") {
			out = append(out, e.Message)
		}
	}

	return out
}

func sinkMark() int {
	sink.mu.Lock()
	defer sink.mu.Unlock()

	return len(sink.lines)
}

// A create that omits resourceLimits while the pool was built with the SDKs' defaults used to
// take the cold path with nothing said. It now says which fields differed - once per distinct
// miss per window, not once per create.
func TestAPoolMissSaysWhichFieldDiffersOncePerWindow(t *testing.T) {
	cl := &claimLog{}
	h := poolHarness(t, 1, cl)
	h.poolReady(1)

	mark := sinkMark()

	bare := sdkCreate()
	delete(bare, "resourceLimits")

	for range 3 {
		if got := h.create(bare); got.Status.State != stateRunning {
			t.Fatalf("cold create: %s", got.Status.State)
		}
	}

	misses := poolMisses(mark)
	if len(misses) != 1 {
		t.Fatalf("%d pool-miss lines for three identical misses, want exactly 1: %q", len(misses), misses)
	}

	for _, want := range []string{"python:3.11-slim", `resourceLimits.cpu is unset, the pool's is "1"`,
		`resourceLimits.memory is unset, the pool's is "2147483648"`, "cold path", "cpu 1, memory 2Gi"} {
		if !strings.Contains(misses[0], want) {
			t.Errorf("pool-miss line %q does not say %q", misses[0], want)
		}
	}

	if strings.Contains(misses[0], "entrypoint is") {
		t.Errorf("pool-miss line names entrypoint, which matched: %q", misses[0])
	}

	// A different miss is its own line.
	other := sdkCreate()
	other["entrypoint"] = []string{"sleep", "infinity"}
	h.create(other)

	if misses = poolMisses(mark); len(misses) != 2 || !strings.Contains(misses[1], "entrypoint differs from the pool's") {
		t.Fatalf("a second, different miss: %q", misses)
	}

	// And the first comes back once its window has passed.
	h.advance(poolMissEvery + time.Second)
	h.create(bare)

	if misses = poolMisses(mark); len(misses) != 3 {
		t.Fatalf("the same miss after the window: %d lines, want 3: %q", len(misses), misses)
	}

	if len(cl.all()) != 0 {
		t.Fatal("the pool served a create it should not have")
	}
}

// A request the pool can never carry says which field, and an explicit sbx.pool=off says nothing.
func TestAnUnpoolableCreateSaysWhichFieldAndOptOutIsSilent(t *testing.T) {
	cl := &claimLog{}
	h := poolHarness(t, 1, cl)
	h.poolReady(1)

	mark := sinkMark()

	off := sdkCreate()
	off["extensions"] = map[string]string{"sbx.pool": "off"}
	h.create(off)

	if m := poolMisses(mark); len(m) != 0 {
		t.Fatalf("sbx.pool=off was logged as a miss: %q", m)
	}

	ports := sdkCreate()
	ports["extensions"] = map[string]string{"sbx.ports": "8000"}
	h.create(ports)

	if m := poolMisses(mark); len(m) != 1 || !strings.Contains(m[0], `extensions["sbx.ports"]`) {
		t.Fatalf("unpoolable create: %q", m)
	}
}

// No pools, no lines: a daemon without --osb-pool never misses anything.
func TestNoPoolMeansNoPoolMissLines(t *testing.T) {
	h := newHarness(t)
	mark := sinkMark()

	h.create(minimalCreate())

	if m := poolMisses(mark); len(m) != 0 {
		t.Fatalf("pool-miss lines with no pool configured: %q", m)
	}
}

// A pool-miss line is written from request values. The caller's entrypoint is their argv - it
// can hold a token, and a newline that forges the next log line - so the line names the field
// that differed, never its value, and every value it does print is quoted.
func TestAPoolMissNeverLogsTheCallersArgvOrARawNewline(t *testing.T) {
	h := poolHarness(t, 1, &claimLog{})
	h.poolReady(1)

	mark := sinkMark()

	c := sdkCreate()
	c["entrypoint"] = []string{"sh", "-c", "curl -H 'Authorization: s3cr3t'\n2026-09-26 INFO forged line"}
	h.create(c)

	misses := poolMisses(mark)
	if len(misses) != 1 {
		t.Fatalf("pool-miss lines: %q", misses)
	}

	if strings.Contains(misses[0], "s3cr3t") || strings.Contains(misses[0], "\n") || strings.Contains(misses[0], "forged") {
		t.Fatalf("the pool-miss line carries the caller's argv: %q", misses[0])
	}

	if !strings.Contains(misses[0], "entrypoint differs") || !strings.Contains(misses[0], `"python:3.11-slim"`) {
		t.Fatalf("the pool-miss line no longer says what differed, or does not quote the image: %q", misses[0])
	}
}
