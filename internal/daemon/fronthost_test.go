package daemon

import "testing"

// --front used to take only a port, which meant it could carry a process on its own loopback and
// nothing else. On a managed platform that is the wrong half: the container is routable to the
// platform's database at a private address, and you are not. host:port is what turns "reachable
// only from inside" into a local port.

func TestFrontAcceptsAHostAndPort(t *testing.T) {
	got, err := parseFront("db=10.0.4.7:3306,cache=6379,7000")
	if err != nil {
		t.Fatal(err)
	}

	if f := got[3306]; f.host != "10.0.4.7" || f.name != "db" || f.port != 3306 {
		t.Fatalf("db = %+v, want host 10.0.4.7 on port 3306", f)
	}

	// A bare port still means loopback, which is every existing deployment.
	if f := got[6379]; f.host != "" || f.name != "cache" {
		t.Fatalf("cache = %+v, want loopback and the name it was given", f)
	}

	if f := got[7000]; f.host != "" || f.name != "port-7000" {
		t.Fatalf("unnamed = %+v, want loopback and a derived name", f)
	}
}

func TestFrontRefusesTwoHostsOnOnePort(t *testing.T) {
	// One local port cannot carry two databases. Letting map order pick would mean connecting
	// to whichever one the runtime felt like that day.
	if _, err := parseFront("a=10.0.0.1:5432,b=10.0.0.2:5432"); err == nil {
		t.Fatal("two hosts on one port were accepted; which one you reach would be arbitrary")
	}

	// The same port twice for the same host is not a conflict - it is a repeat.
	if _, err := parseFront("a=10.0.0.1:5432,b=10.0.0.1:5432"); err != nil {
		t.Fatalf("the same target named twice was refused: %v", err)
	}
}

func TestFrontStillRefusesNonsense(t *testing.T) {
	for _, bad := range []string{"db=notaport", "db=10.0.0.1:0", "db=10.0.0.1:70000", "x=-1"} {
		if _, err := parseFront(bad); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
}

func TestFrontHostDefaultsToLoopbackForDiscoveredPorts(t *testing.T) {
	d := &daemon{fronted: map[int]fronted{3306: {name: "db", port: 3306, host: "10.0.4.7"}}}

	if got := d.frontHost(3306); got != "10.0.4.7" {
		t.Fatalf("frontHost(3306) = %q, want the fronted host", got)
	}

	// A port this daemon discovered is its own wake listener, which is always loopback.
	if got := d.frontHost(20000); got != "" {
		t.Fatalf("frontHost(20000) = %q, want empty for a discovered port", got)
	}
}
