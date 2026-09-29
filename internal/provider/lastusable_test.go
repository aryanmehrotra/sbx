package provider

import "testing"

func TestLastUsableAddress(t *testing.T) {
	for in, want := range map[string]string{
		"172.30.0.0/16":   "172.30.255.254",
		"192.168.16.0/20": "192.168.31.254",
		"10.0.0.0/24":     "10.0.0.254",
	} {
		got, err := lastUsable(in)
		if err != nil || got != want {
			t.Errorf("lastUsable(%s) = %q, %v, want %s", in, got, err, want)
		}
	}

	for _, bad := range []string{"10.0.0.0/31", "fd00::/64", "nonsense"} {
		if _, err := lastUsable(bad); err == nil {
			t.Errorf("lastUsable(%s) gave an address", bad)
		}
	}
}
