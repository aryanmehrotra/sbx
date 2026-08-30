package daemon

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestLooksHTTPRecognisesARequestAndNothingElse(t *testing.T) {
	yes := []string{"GET / HTTP/1.1\r\n", "POST /x HTTP/1.1", "HEAD / HTTP/1.0", "OPTIONS * HTTP/1.1"}
	for _, s := range yes {
		if !looksHTTP([]byte(s)) {
			t.Fatalf("%q was not recognised as HTTP; a browser would get a spinner", s)
		}
	}

	// The ones that must never be answered with HTML. A postgres startup packet begins with a
	// length; redis with '*'; TLS with 0x16. Answering any of them with a web page corrupts
	// the stream in a way the client cannot report usefully.
	no := []string{
		"\x00\x00\x00\x08\x04\xd2\x16\x2f", // postgres SSLRequest
		"*1\r\n$4\r\nPING\r\n",             // redis
		"\x16\x03\x01\x02\x00",             // TLS ClientHello
		"GETS ",                            // close, but not a method
		"GET",                              // no space yet: not a request line
		"",
	}
	for _, s := range no {
		if looksHTTP([]byte(s)) {
			t.Fatalf("%q was treated as HTTP; it would have been answered with a web page", s)
		}
	}
}

func TestTheWaitingPageIsA503ABrowserWillReload(t *testing.T) {
	raw := waitingPage("my-sandbox", "web")

	resp, err := http.ReadResponse(newReader(raw), nil)
	if err != nil {
		t.Fatalf("the page is not a parseable HTTP response: %v", err)
	}

	defer resp.Body.Close()

	// 503, not 200: this is not the service answering, and a cache or an uptime monitor that
	// took it for one would remember a holding page as the site.
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}

	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Fatal("no Retry-After: a client with no scripting has nothing telling it to come back")
	}

	if !resp.Close {
		t.Fatal("the response does not close the connection; the browser would hold this " +
			"socket open against a service that is about to start serving on it")
	}

	body, _ := io.ReadAll(resp.Body)
	if int64(len(body)) != resp.ContentLength {
		t.Fatalf("Content-Length says %d, body is %d - a browser would hang waiting for the rest",
			resp.ContentLength, len(body))
	}

	// It has to say which thing is starting, or it is indistinguishable from a generic error.
	if !strings.Contains(string(body), "my-sandbox") || !strings.Contains(string(body), "web") {
		t.Fatal("the page does not name the sandbox and service it is waiting for")
	}

	if !strings.Contains(string(body), `http-equiv="refresh"`) {
		t.Fatal("nothing reloads the page, so it is a dead end rather than a wait")
	}
}

// The safety property the whole design rests on: looking at a connection must not change it.
func TestPeekedBytesAreReplayedExactly(t *testing.T) {
	const req = "GET /index.html HTTP/1.1\r\nHost: x\r\n\r\n"

	client, server := net.Pipe()

	go func() {
		_, _ = client.Write([]byte(req))
		_ = client.Close()
	}()

	pre := peek(server, 512)
	if len(pre) == 0 {
		t.Fatal("peek read nothing from a client that had already written")
	}

	// What the upstream would receive: the replayed prefix, then the rest.
	got, err := io.ReadAll(&prefixConn{Conn: server, pre: pre})
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}

	if string(got) != req {
		t.Fatalf("the upstream would have received %q, want %q - inspecting the connection "+
			"changed it", got, req)
	}
}

func TestPeekReturnsNothingForAClientThatSpeaksSecond(t *testing.T) {
	// MySQL and SMTP both greet before the client says anything. Such a connection must not be
	// held here on the chance it might turn out to be HTTP.
	_, server := net.Pipe()

	start := time.Now()
	pre := peek(server, 512)

	if len(pre) != 0 {
		t.Fatalf("peek invented %d bytes from a silent client", len(pre))
	}

	if d := time.Since(start); d > 2*peekWindow {
		t.Fatalf("peek waited %s on a silent client; the window is %s", d, peekWindow)
	}
}

func TestPrefixConnPassesThroughOnceDrained(t *testing.T) {
	client, server := net.Pipe()

	go func() {
		_, _ = client.Write([]byte("tail"))
		_ = client.Close()
	}()

	p := &prefixConn{Conn: server, pre: []byte("head")}

	got, _ := io.ReadAll(p)
	if string(got) != "headtail" {
		t.Fatalf("got %q, want the prefix then the live bytes", got)
	}
}

func newReader(b []byte) *bufio.Reader { return bufio.NewReader(bytes.NewReader(b)) }

// FuzzPeekReplayNeverChangesTheStream is the invariant that makes inspecting a connection safe:
// whatever peek consumes, prefixConn must hand back, byte for byte, for ANY input - including the
// ones that look almost like an HTTP request and the ones that are pure binary.
func FuzzPeekReplayNeverChangesTheStream(f *testing.F) {
	f.Add([]byte("GET / HTTP/1.1\r\n\r\n"))
	f.Add([]byte("\x00\x00\x00\x08\x04\xd2\x16\x2f"))
	f.Add([]byte("*1\r\n$4\r\nPING\r\n"))
	f.Add([]byte(""))
	f.Add([]byte("GET"))

	f.Fuzz(func(t *testing.T, in []byte) {
		if len(in) > 4096 {
			in = in[:4096]
		}

		client, server := net.Pipe()

		go func() {
			_, _ = client.Write(in)
			_ = client.Close()
		}()

		pre := peek(server, 512)

		got, err := io.ReadAll(&prefixConn{Conn: server, pre: pre})
		if err != nil && err != io.EOF {
			return // a closed pipe is not what this is testing
		}

		if !bytes.Equal(got, in) {
			t.Fatalf("the stream changed: sent %q, upstream would see %q", in, got)
		}
	})
}
