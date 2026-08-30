package provider

import (
	"strings"
	"testing"
)

func TestFilterImageTagChangesWithTheSource(t *testing.T) {
	a := filterImageTag(map[string]string{"filter.go": "package main // one"})
	b := filterImageTag(map[string]string{"filter.go": "package main // two"})

	if a == b {
		t.Fatal("two different filters hash to the same image tag: a changed allow-list " +
			"implementation would silently keep running the old binary from the cache")
	}

	if a != filterImageTag(map[string]string{"filter.go": "package main // one"}) {
		t.Fatal("the same source hashed twice gave two tags; every create would rebuild")
	}

	if !strings.HasPrefix(a, "sbx-egress-filter:") {
		t.Fatalf("tag %q is not in sbx's own namespace", a)
	}
}

func TestFilterImageTagCoversEveryFileAndItsName(t *testing.T) {
	// A hash over contents alone would collide when two files swap, and the Dockerfile is
	// where the image pins live - a changed pin has to produce a changed tag.
	x := filterImageTag(map[string]string{"a": "1", "b": "2"})
	y := filterImageTag(map[string]string{"a": "2", "b": "1"})

	if x == y {
		t.Fatal("swapping two files' contents did not change the tag")
	}

	if filterImageTag(map[string]string{"Dockerfile": "FROM golang:1.26-alpine"}) ==
		filterImageTag(map[string]string{"Dockerfile": "FROM golang:1.27-alpine"}) {
		t.Fatal("a changed base-image pin did not change the tag; the rebuild would be skipped")
	}
}

func TestFilterContainerIsNamedForItsSandbox(t *testing.T) {
	if got := filterContainer("proj-feature"); got != "sbx-egressfilter-proj-feature" {
		t.Fatalf("filterContainer = %q; the name has to identify the sandbox it belongs to, "+
			"or `sbx gc` cannot tell whose it is", got)
	}
}
