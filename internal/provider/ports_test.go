package provider

import "testing"

func TestParsePorts(t *testing.T) {
	cases := []struct {
		name  string
		label string
		want  []PortPair
		fails bool
	}{
		{name: "single", label: "3306:13306", want: []PortPair{{Public: 3306, Backing: 13306}}},
		{name: "several", label: "3306:13306,6379:16379", want: []PortPair{{Public: 3306, Backing: 13306}, {Public: 6379, Backing: 16379}}},
		{name: "spaced", label: " 3306:13306 , 6379:16379 ", want: []PortPair{{Public: 3306, Backing: 13306}, {Public: 6379, Backing: 16379}}},
		// An empty label must fail rather than yield a unit with no listeners: a sandbox
		// that silently fronts nothing is the failure mode that looks like success.
		{name: "empty", label: "", fails: true},
		{name: "blank", label: "   ", fails: true},
		{name: "no colon", label: "3306", fails: true},
		{name: "bad public", label: "http:13306", fails: true},
		{name: "bad backing", label: "3306:mysql", fails: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParsePorts(c.label)

			if c.fails {
				if err == nil {
					t.Fatalf("ParsePorts(%q) = %v, want error", c.label, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("ParsePorts(%q): %v", c.label, err)
			}

			if len(got) != len(c.want) {
				t.Fatalf("ParsePorts(%q) = %v, want %v", c.label, got, c.want)
			}

			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("pair %d = %v, want %v", i, got[i], c.want[i])
				}
			}
		})
	}
}
