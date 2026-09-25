// Command osbharness is the Go half of scripts/osb-conformance.sh and scripts/osb-bench.sh.
//
// The shell owns processes - building sbx, starting and killing the throwaway daemon, the
// trap that tears it down. Everything that parses something lives here instead: go test's
// JSON stream, upstream's test files, the expectations file and the lifecycle API's
// responses. Parsing JSON with grep is how a gate ends up passing on a stream it did not
// understand.
//
//	osbharness freeport
//	osbharness tier     -expectations F -tier v0.9.0
//	osbharness tests    -dir tests/go -files sandbox,command [-match REGEX]
//	osbharness ready    -url http://127.0.0.1:P -key K [-timeout 60s]
//	osbharness sweep    -url http://127.0.0.1:P -key K
//	osbharness report   -expectations F -expected FILE [-save FILE]  < go-test-json
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: osbharness freeport|tier|tests|ready|sweep|report [flags]")
		os.Exit(2)
	}

	var err error

	switch os.Args[1] {
	case "freeport":
		err = runFreePort(os.Stdout)
	case "tier":
		err = runTier(os.Args[2:], os.Stdout)
	case "tests":
		err = runTests(os.Args[2:], os.Stdout)
	case "ready":
		err = runReady(os.Args[2:], os.Stdout)
	case "sweep":
		err = runSweep(os.Args[2:], os.Stdout)
	case "report":
		err = runReport(os.Args[2:], os.Stdin, os.Stdout)
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "osbharness %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}
