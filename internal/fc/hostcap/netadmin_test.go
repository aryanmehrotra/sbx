package hostcap

import "testing"

func TestCapEffHasReadsTheEffectiveSet(t *testing.T) {
	for status, want := range map[string]bool{
		"Name:\tsbx\nCapInh:\t0000000000000000\nCapPrm:\t0000000000001000\nCapEff:\t0000000000001000\n": true,
		"CapPrm:\t0000000000001000\nCapEff:\t0000000000000000\n":                                        false, // permitted, not effective
		"CapEff:\t000001ffffffffff\n": true,
		"CapEff:\tzz\n":               false,
		"Name:\tsbx\n":                false,
	} {
		if got := capEffHas(status, capNetAdmin); got != want {
			t.Errorf("%q: %v, want %v", status, got, want)
		}
	}
}
