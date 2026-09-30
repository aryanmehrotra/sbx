package spec

import "testing"

// The cycle message used to be spliced into its own path - "a → b → services depend on each
// other in a cycle, at "a" - ..." - and, alone among load errors, carried no file name.
func TestACycleErrorNamesTheFileAndTheWholeLoop(t *testing.T) {
	raw := []byte(`{"version":1,"services":{` +
		`"a":{"image":"x","ports":[1],"depends_on":["b"]},` +
		`"b":{"image":"x","ports":[2],"depends_on":["a"]}}}`)

	_, err := ParseSpec(raw, "spec.json")
	if err == nil {
		t.Fatal("a dependency cycle was accepted at load")
	}

	want := "spec.json: services depend on each other in a cycle: a → b → a - " +
		"nothing can be created first; remove one depends_on edge"
	if err.Error() != want {
		t.Errorf("error = %q\nwant    %q", err, want)
	}
}

// Only the loop is the cycle. A service that depends on the loop without being part of it is
// not what the reader has to break, so it stays out of the path.
func TestACycleErrorShowsOnlyTheLoop(t *testing.T) {
	s := specOf(map[string][]string{"a": {"b"}, "b": {"c"}, "c": {"b"}})

	_, err := s.CreationOrder()
	if err == nil {
		t.Fatal("a dependency cycle was accepted")
	}

	want := "services depend on each other in a cycle: b → c → b - nothing can be created " +
		"first; remove one depends_on edge"
	if err.Error() != want {
		t.Errorf("error = %q\nwant    %q", err, want)
	}
}
