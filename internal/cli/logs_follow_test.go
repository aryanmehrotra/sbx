package cli

// `sbx logs -f` ends when the followed container stops, because that is when `docker logs
// --follow` ends. On a sandbox that sleeps, that is routine - and it used to exit 0 with no
// word, which reads as "the log ended" or as sbx crashing, not as "your service went to sleep".

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// followStub serves one service that is awake when the command starts and asleep once its log
// stream ends - the idle timer firing mid-follow.
type followStub struct {
	provider.Provider
	lists int

	// stillUp is how many lists after the first still report it running: docker ends the log
	// stream as the process exits, a moment before it reports the container stopped.
	stillUp int
}

func (f *followStub) List(_ context.Context, sandbox string) ([]provider.Unit, error) {
	f.lists++

	return []provider.Unit{{Sandbox: sandbox, Service: "redis", Ref: "sbx-x-redis", Running: f.lists <= 1+f.stillUp}}, nil
}

func (f *followStub) Logs(_ context.Context, _ string, _ int, _ bool, w io.Writer) error {
	_, err := io.WriteString(w, "Ready to accept connections\n")

	return err
}

func TestLogsFollowSaysWhenTheServiceWentToSleep(t *testing.T) {
	for _, c := range []struct {
		service string
		stillUp int
	}{
		{"redis", 0}, {"", 0}, // one service, and all of them
		{"redis", 3}, // docker still says running when the stream ends (seen live)
	} {
		t.Run(fmt.Sprintf("service=%s,stillUp=%d", c.service, c.stillUp), func(t *testing.T) {
			out := captureStderr(t)

			if err := Logs(context.Background(), &followStub{stillUp: c.stillUp}, "x", c.service, 10, true); err != nil {
				t.Fatal(err)
			}

			for _, want := range []string{"redis", "went to sleep", "sbx logs -f x redis"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("the note does not say %q:\n%s", want, out.String())
				}
			}
		})
	}
}

// Without -f the command ends because it was asked to, and a note would be noise.
func TestLogsWithoutFollowAddsNoNote(t *testing.T) {
	out := captureStderr(t)

	if err := Logs(context.Background(), &followStub{}, "x", "redis", 10, false); err != nil {
		t.Fatal(err)
	}

	if out.Len() != 0 {
		t.Errorf("a plain `sbx logs` printed a note:\n%s", out.String())
	}
}
