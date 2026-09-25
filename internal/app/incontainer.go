package app

// Commands sbx runs INSIDE a sandbox rather than on the host.
//
// The OpenSandbox API mounts this same binary read-only at /opt/sbx in every sandbox it creates,
// so anything the sandbox needs that its image may not carry can be a subcommand here instead of
// a dependency on the image. They skip everything Main does for a host command - the history
// journal above all: a health check that ran every five seconds would otherwise append to a
// journal inside the container's filesystem for its whole life.

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

var inContainer = map[string]func(args []string) int{
	"httpcheck": httpcheck,
}

// httpcheck exits 0 when a URL answers 2xx. It is the health check for API sandboxes, because
// python:3.11-slim - the OpenSandbox SDK's default image - has neither curl nor wget.
func httpcheck(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: sbx httpcheck URL")
		return 2
	}

	c := http.Client{Timeout: 2 * time.Second}

	resp, err := c.Get(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	_ = resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		fmt.Fprintf(os.Stderr, "%s answered %s\n", args[0], resp.Status)
		return 1
	}

	return 0
}
