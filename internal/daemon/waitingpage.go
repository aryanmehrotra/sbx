package daemon

// The page a browser gets while its sandbox is still waking.
//
// sbx's answer to a cold connection is to HOLD it, not refuse it: the client waits, the service
// comes up, and the request is served. That is right for everything with a client library on the
// other end - psql, a driver, a test runner - and it is what makes "connecting is the wake signal"
// work at all. Nothing has to know sbx exists.
//
// It is wrong for exactly one caller: a person looking at a browser. A held connection renders as
// a white screen with a spinner, for as long as the stack takes - about a second for one service,
// noticeably longer for a stack that has to bring up its dependencies first. There is nothing on
// the screen that says the machine is doing anything, so the honest reading of it is "the link is
// broken", and the reflex is to reload, which opens a second connection to a service that is
// already waking. Sablier ships a waiting page for this reason; sbx offered a spinner.
//
// So: only when the wait is long enough to be worth explaining, only when the first bytes are an
// HTTP request, and only when asked for. Everything else is untouched - the hold is still the
// default and still the thing the wake numbers are measured on.

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/aryanmehrotra/sbx/internal/features"
	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// waitingPageAfter is how long a wake may take before a browser is told what is happening.
//
// Below this, saying anything is worse than saying nothing: the page would appear and be replaced
// before it could be read, and the reload it asks for would arrive after the service was already
// up. A local redis wakes in about 0.2s and a postgres in about a second, so a second of quiet
// covers the ordinary case and the page is for the stack that genuinely takes longer.
const waitingPageAfter = time.Second

// peekWindow is how long to wait for a client's first bytes before deciding it is not HTTP.
//
// An HTTP client sends its request line immediately. A server-first protocol - MySQL and SMTP
// both greet before the client says anything - sends nothing at all, and must not be held here
// on the chance that it might. Short, because this is only reached on a wake that is already
// slow, and everything it costs is added to that.
const peekWindow = 250 * time.Millisecond

// httpMethods are the request-line openings worth recognising. Deliberately not a parser: the
// question is "would a browser render what I send back", and the method token answers it.
var httpMethods = [][]byte{
	[]byte("GET "), []byte("POST "), []byte("HEAD "), []byte("PUT "),
	[]byte("DELETE "), []byte("PATCH "), []byte("OPTIONS "), []byte("CONNECT "),
	[]byte("TRACE "),
}

// looksHTTP reports whether these first bytes begin an HTTP request.
func looksHTTP(b []byte) bool {
	for _, m := range httpMethods {
		if bytes.HasPrefix(b, m) {
			return true
		}
	}

	return false
}

// peek reads whatever the client has already sent, up to n bytes, within peekWindow.
//
// The bytes are RETURNED, not consumed: whatever this reads has to be handed to the upstream when
// the connection turns out to be an ordinary one, or the first thing the service receives is a
// request with its verb missing. A proxy that eats part of a request is worse than one that never
// looked.
func peek(c net.Conn, n int) []byte {
	if err := c.SetReadDeadline(time.Now().Add(peekWindow)); err != nil {
		return nil
	}

	defer func() { _ = c.SetReadDeadline(time.Time{}) }()

	buf := make([]byte, n)

	k, _ := c.Read(buf)
	if k <= 0 {
		return nil
	}

	return buf[:k]
}

// waitingPage is the response, built once per request because it carries the service's name.
//
// 503 with Retry-After, not 200: this is not the service answering, and a cache or a monitor that
// treated it as one would remember a holding page as the site. Connection: close so the browser
// does not hold this socket for the reload. The refresh is in a meta tag rather than script so it
// works with scripting off, and the interval is short enough to feel immediate but long enough
// that a slow stack is not reloaded twenty times on the way up.
func waitingPage(sandbox, service string) []byte {
	title := service
	if sandbox != "" {
		title = sandbox + " / " + service
	}

	body := fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="refresh" content="2">
<title>Starting %s</title>
<style>
:root { color-scheme: light dark; }
body { margin:0; min-height:100vh; display:flex; align-items:center; justify-content:center;
       font:15px/1.5 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;
       background:#fafafa; color:#1a1a1a; }
@media (prefers-color-scheme: dark) { body { background:#141414; color:#ededed; } }
.card { max-width:26rem; padding:2rem; text-align:center; }
.dot { display:inline-block; width:.5rem; height:.5rem; border-radius:50%%;
       background:currentColor; opacity:.25; animation:p 1.2s infinite; margin:0 .15rem; }
.dot:nth-child(2) { animation-delay:.2s } .dot:nth-child(3) { animation-delay:.4s }
@keyframes p { 0%%,100%% { opacity:.25 } 50%% { opacity:1 } }
h1 { font-size:1rem; font-weight:600; margin:0 0 .5rem }
p { margin:.5rem 0 0; opacity:.7; font-size:.875rem }
code { font-family:ui-monospace,SFMono-Regular,Menlo,monospace; font-size:.8125rem; opacity:.85 }
</style>
</head>
<body>
<div class="card">
<div><span class="dot"></span><span class="dot"></span><span class="dot"></span></div>
<h1>Starting <code>%s</code></h1>
<p>It was asleep, and your request woke it. This page reloads by itself.</p>
</div>
</body>
</html>
`, title, title)

	return []byte("HTTP/1.1 503 Service Unavailable\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"Retry-After: 2\r\n" +
		"Cache-Control: no-store\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(body)) +
		"Connection: close\r\n" +
		"\r\n" + body)
}

// prefixConn is a net.Conn whose first reads come from bytes already taken off the wire.
//
// This is what makes peeking safe. The peeked request is replayed to the upstream before anything
// else, so a connection that was inspected and then carried normally is byte-for-byte the
// connection that would have been carried without inspecting it.
type prefixConn struct {
	net.Conn

	pre []byte
}

func (p *prefixConn) Read(b []byte) (int, error) {
	if len(p.pre) > 0 {
		n := copy(b, p.pre)
		p.pre = p.pre[n:]

		return n, nil
	}

	return p.Conn.Read(b)
}

// wakeSummary is what the log line and the page call this unit.
func wakeSummary(sandbox, service string) string {
	if sandbox == "" {
		return service
	}

	return strings.TrimSpace(sandbox + "/" + service)
}

// maybeWaitingPage decides whether this connection should be answered with a holding page
// instead of being held.
//
// It returns any bytes it consumed and whether it answered. The order matters and is the whole
// safety argument:
//
//  1. If the gate is off, or the unit is already awake, do nothing at all. The common path never
//     reaches a single extra syscall.
//  2. Start the wake. This is not delayed by anything below - the page is a thing said WHILE the
//     sandbox comes up, never instead of bringing it up.
//  3. Wait waitingPageAfter for that wake. If it finishes, return with nothing consumed: the
//     connection proceeds exactly as it always did.
//  4. Only on a wake that is genuinely slow, look at the client's first bytes. If they are an
//     HTTP request, answer with the page. If they are anything else, hand them back to be
//     replayed and carry on holding.
//
// The wake goroutine outlives a served page on purpose. The point is that the reload two seconds
// later finds a service that is up, which means the wake this request started has to keep going
// after the request is answered.
func (u *unit) maybeWaitingPage(ctx context.Context, p provider.Provider, client net.Conn,
	readyTimeout time.Duration,
) (pre []byte, served bool) {
	if !features.Enabled("waiting-page") || u.isAwake() {
		return nil, false
	}

	woke := make(chan error, 1)
	go func() { woke <- u.wake(ctx, p, readyTimeout) }()

	select {
	case err := <-woke:
		if err != nil {
			logs.Default.Event(logs.LevelError, u.sandbox, u.service, "wakeFailed", 0,
				"could not wake: %v", err)
		}
		// Awake, or failed. Either way handle() does what it always did - including
		// reporting the failure by hanging up.
		return nil, false

	case <-time.After(waitingPageAfter):
	}

	first := peek(client, 512)
	if !looksHTTP(first) {
		return first, false
	}

	if _, err := client.Write(waitingPage(u.sandbox, u.service)); err != nil {
		return first, false
	}

	logs.Default.Info(u.sandbox, u.service, "served the waiting page while %s wakes",
		wakeSummary(u.sandbox, u.service))

	return nil, true
}
