package app

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/aryanmehrotra/sbx/internal/mcp"
	"github.com/aryanmehrotra/sbx/internal/osb"
	"github.com/aryanmehrotra/sbx/internal/osbclient"
)

// defaultOSBURL is where `sbx serve --osb-addr` listens unless told otherwise.
const defaultOSBURL = "http://127.0.0.1:8080"

// osbTarget picks the server and key `sbx mcp` talks to: a flag, then sbx's own variable, then
// the one upstream's MCP server and SDKs read - so an agent config written for OpenSandbox
// keeps working when `sbx mcp` is dropped in for `opensandbox-mcp`.
func osbTarget(flagURL, flagKey string, getenv func(string) string) (url, key string) {
	url = firstNonEmpty(flagURL, getenv("SBX_OSB_URL"), getenv("OPEN_SANDBOX_DOMAIN"), defaultOSBURL)
	key = firstNonEmpty(flagKey, getenv("SBX_OSB_KEY"), getenv("OPEN_SANDBOX_API_KEY"))

	return url, key
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}

	return ""
}

func runMCP(args []string) error {
	fs := newFlagSet("mcp")
	flagURL := fs.String("url", "", "OpenSandbox server (default $SBX_OSB_URL, $OPEN_SANDBOX_DOMAIN, or "+defaultOSBURL+")")
	flagKey := fs.String("key", "", "API key (default $SBX_OSB_KEY or $OPEN_SANDBOX_API_KEY)")
	_ = fs.Parse(args)

	if fs.NArg() > 0 {
		return fmt.Errorf("sbx mcp takes no arguments, got %q; it speaks MCP on stdin and stdout", fs.Args())
	}

	url, key := osbTarget(*flagURL, *flagKey, os.Getenv)
	key = withLocalKey(url, key, func() string {
		dir, err := osb.DefaultStateDir()
		if err != nil {
			return ""
		}

		return osb.ReadKey(dir)
	})

	client, err := osbclient.New(url, key, osbclient.WithUserAgent("sbx-mcp/"+version))
	if err != nil {
		return err
	}

	s := mcp.NewServer("sbx", version)
	s.Title = "sbx sandboxes"
	s.Instructions = mcp.Instructions
	// stdout is the protocol. Everything else goes to stderr, which MCP clients keep as the
	// server's log.
	s.Log = os.Stderr
	s.Register(mcp.NewSandboxes(client).Tools()...)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(os.Stderr, "sbx mcp: serving MCP on stdio for %s\n", client.BaseURL())

	// A signal is how an MCP client ends a server that did not exit on end of input; that is a
	// normal shutdown, not a failure to report.
	if err := s.Serve(ctx, os.Stdin, os.Stdout); err != nil && ctx.Err() == nil {
		return err
	}

	return nil
}

// withLocalKey falls back to the key `sbx serve` generated (~/.sbx/osb/key) when no key was
// given - and only when the server is on this machine's loopback. That file is the credential
// for this machine's API; sending it to whatever --url names would hand it to somebody else's
// server.
func withLocalKey(target, key string, read func() string) string {
	if key != "" {
		return key
	}

	raw := target
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}

	host := u.Hostname()
	ip := net.ParseIP(host)

	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return ""
	}

	return read()
}
