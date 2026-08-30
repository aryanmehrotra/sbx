package features

import "testing"

// A gate is consulted where a command starts, never per connection - and the spec said so, which
// makes it a claim to check rather than assume.
//
// Enabled() reads the environment every call so that a test which sets the variable sees it on
// the next one. That is the right trade at command scale and the wrong one on a hot path, so if
// a gate ever migrates onto the connection path this is the number that says what it cost. The
// connection benchmark in internal/daemon is the other half: +0 allocations there means no gate
// arrived on it.
func BenchmarkGateCheck(b *testing.B) {
	withBench(b)

	b.Setenv("SBX_FEATURES", "ssh,devcontainer,waitpage")
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		if !Enabled("ssh") {
			b.Fatal("expected ssh on")
		}
	}
}

// The same check when nothing is set, which is what every ungated command pays.
func BenchmarkGateCheckUnset(b *testing.B) {
	withBench(b)

	b.Setenv("SBX_FEATURES", "")
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		_ = Enabled("ssh")
	}
}

func withBench(b *testing.B) {
	b.Helper()

	saved := registry
	registry = nil

	b.Cleanup(func() { registry = saved })

	Register(Feature{Name: "ssh", Stability: Preview, Summary: "s"})
}
