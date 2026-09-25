package jupyter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// collect runs code and returns its events, failing the test on a pre-stream error.
func collect(t *testing.T, e *Engine, req RunRequest) []Event {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var evs []Event

	if err := e.Run(ctx, req, func(ev Event) { evs = append(evs, ev) }); err != nil {
		t.Fatalf("Run(%q): %v", req.Code, err)
	}

	return evs
}

func types(evs []Event) []EventType {
	out := make([]EventType, 0, len(evs))
	for _, ev := range evs {
		if ev.Type != EventPing {
			out = append(out, ev.Type)
		}
	}

	return out
}

func mustCreate(t *testing.T, e *Engine, lang string) Context {
	t.Helper()

	c, err := e.CreateContext(context.Background(), lang, "")
	if err != nil {
		t.Fatalf("CreateContext(%s): %v", lang, err)
	}

	return c
}

// The event sequence is the contract with every client, so each case pins it exactly.
func TestRunEventSequences(t *testing.T) {
	f := newFake(t)
	e := f.engine()
	c := mustCreate(t, e, "python")

	cases := []struct {
		name  string
		code  string
		want  []EventType
		check func(t *testing.T, evs []Event)
	}{
		{
			name: "stdout only",
			code: "print:hello",
			want: []EventType{EventInit, EventStdout, EventComplete},
			check: func(t *testing.T, evs []Event) {
				if evs[0].Text != c.ID {
					t.Errorf("init text = %q, want the context id %q", evs[0].Text, c.ID)
				}

				if evs[1].Text != "hello\n" {
					t.Errorf("stdout = %q", evs[1].Text)
				}
			},
		},
		{
			name: "stderr",
			code: "eprint:warn",
			want: []EventType{EventInit, EventStderr, EventComplete},
			check: func(t *testing.T, evs []Event) {
				if evs[1].Text != "warn\n" {
					t.Errorf("stderr = %q", evs[1].Text)
				}
			},
		},
		{
			name: "result renames text/plain to text",
			code: "print:x\nresult:4",
			want: []EventType{EventInit, EventStdout, EventExecutionCount, EventResult, EventComplete},
			check: func(t *testing.T, evs []Event) {
				if evs[2].ExecutionCount < 1 {
					t.Errorf("execution_count = %d", evs[2].ExecutionCount)
				}

				if !reflect.DeepEqual(evs[3].Results, map[string]any{"text": "4"}) {
					t.Errorf("results = %v", evs[3].Results)
				}
			},
		},
		{
			name: "display_data becomes a result",
			code: "display:image/png=iVBORw0K",
			want: []EventType{EventInit, EventResult, EventComplete},
			check: func(t *testing.T, evs []Event) {
				if evs[1].Results["image/png"] != "iVBORw0K" {
					t.Errorf("results = %v", evs[1].Results)
				}
			},
		},
		{
			name: "error carries name, value and traceback, then completes",
			code: "print:before\nraise:ZeroDivisionError:division by zero",
			want: []EventType{EventInit, EventStdout, EventStatus, EventError, EventComplete},
			check: func(t *testing.T, evs []Event) {
				if evs[2].Text != "error" {
					t.Errorf("status text = %q", evs[2].Text)
				}

				want := &ErrorOutput{EName: "ZeroDivisionError", EValue: "division by zero",
					Traceback: []string{"Traceback (most recent call last):", "ZeroDivisionError: division by zero"}}
				if !reflect.DeepEqual(evs[3].Error, want) {
					t.Errorf("error = %+v", evs[3].Error)
				}
			},
		},
		{
			name: "foreign parent is filtered",
			code: "noise\nprint:mine",
			want: []EventType{EventInit, EventStdout, EventComplete},
			check: func(t *testing.T, evs []Event) {
				for _, ev := range evs {
					if strings.Contains(ev.Text, "NOT MINE") {
						t.Errorf("a message for another request leaked: %+v", ev)
					}
				}
			},
		},
		{
			name: "reply before idle",
			code: "replyfirst\nresult:1",
			want: []EventType{EventInit, EventExecutionCount, EventResult, EventComplete},
		},
		{
			name: "missing reply ends after the grace",
			code: "noreply\nprint:x",
			want: []EventType{EventInit, EventStdout, EventComplete},
		},
		{
			name: "error only in the reply is surfaced",
			code: "replyerror",
			want: []EventType{EventInit, EventStatus, EventError, EventComplete},
			check: func(t *testing.T, evs []Event) {
				if evs[2].Error.EName != "Aborted" {
					t.Errorf("error = %+v", evs[2].Error)
				}
			},
		},
	}

	e.cfg.ReplyGrace = 200 * time.Millisecond

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evs := collect(t, e, RunRequest{ContextID: c.ID, Code: tc.code})

			if got := types(evs); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("events = %v, want %v", got, tc.want)
			}

			for _, ev := range evs {
				if ev.Timestamp == 0 {
					t.Errorf("%s event has no timestamp", ev.Type)
				}
			}

			if tc.check != nil {
				tc.check(t, evs)
			}
		})
	}
}

func TestExecutionCountAdvancesAndStateSurvives(t *testing.T) {
	f := newFake(t)
	e := f.engine()
	c := mustCreate(t, e, "python")

	collect(t, e, RunRequest{ContextID: c.ID, Code: "set:x=42"})

	evs := collect(t, e, RunRequest{ContextID: c.ID, Code: "get:x\nresult:ok"})
	if evs[1].Text != "42" {
		t.Fatalf("state lost between runs: %+v", evs[1])
	}

	if evs[2].ExecutionCount != 2 {
		t.Fatalf("execution_count = %d, want 2", evs[2].ExecutionCount)
	}
}

func TestInterruptEndsARunningCell(t *testing.T) {
	f := newFake(t)
	e := f.engine()
	c := mustCreate(t, e, "python")

	var (
		evs []Event
		err error
		wg  sync.WaitGroup
	)

	wg.Add(1)

	go func() {
		defer wg.Done()

		err = e.Run(context.Background(), RunRequest{ContextID: c.ID, Code: "sleep"}, func(ev Event) { evs = append(evs, ev) })
	}()

	<-f.running

	if err := e.Interrupt(context.Background(), c.ID); err != nil {
		t.Fatal(err)
	}

	wg.Wait()

	if err != nil {
		t.Fatal(err)
	}

	if got := types(evs); !reflect.DeepEqual(got, []EventType{EventInit, EventStatus, EventError, EventComplete}) {
		t.Fatalf("events = %v", got)
	}

	if evs[len(evs)-2].Error.EName != "KeyboardInterrupt" {
		t.Fatalf("error = %+v", evs[len(evs)-2].Error)
	}

	if err := e.Interrupt(context.Background(), "nope"); !errors.Is(err, ErrContextNotFound) {
		t.Fatalf("Interrupt(unknown) = %v", err)
	}
}

func TestBusyContextIsRefusedAndOthersRun(t *testing.T) {
	f := newFake(t)
	e := f.engine()
	busy := mustCreate(t, e, "python")
	other := mustCreate(t, e, "python")

	done := make(chan error, 1)

	go func() {
		done <- e.Run(context.Background(), RunRequest{ContextID: busy.ID, Code: "sleep"}, func(Event) {})
	}()

	<-f.running

	called := false

	err := e.Run(context.Background(), RunRequest{ContextID: busy.ID, Code: "print:x"}, func(Event) { called = true })
	if !errors.Is(err, ErrContextBusy) {
		t.Fatalf("second run on a busy context = %v, want ErrContextBusy", err)
	}

	if called {
		t.Fatal("a refused run emitted events; execd could no longer answer with an error status")
	}

	// A different context is not blocked by the busy one.
	if got := types(collect(t, e, RunRequest{ContextID: other.ID, Code: "print:free"})); len(got) != 3 {
		t.Fatalf("other context events = %v", got)
	}

	_ = e.Interrupt(context.Background(), busy.ID)

	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// And the busy one is usable again once its cell ends.
	collect(t, e, RunRequest{ContextID: busy.ID, Code: "print:again"})
}

func TestCancelledCallerInterruptsTheKernel(t *testing.T) {
	f := newFake(t)
	e := f.engine()
	c := mustCreate(t, e, "python")

	ctx, cancel := context.WithCancel(context.Background())

	var evs []Event

	done := make(chan error, 1)

	go func() {
		done <- e.Run(ctx, RunRequest{ContextID: c.ID, Code: "sleep"}, func(ev Event) { evs = append(evs, ev) })
	}()

	<-f.running
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}

	if f.interrupts.Load() != 1 {
		t.Fatalf("interrupts = %d, want 1: an abandoned cell must not keep the context busy", f.interrupts.Load())
	}

	last := evs[len(evs)-1]
	if last.Type != EventError || last.Error.EName != "ContextCancelled" {
		t.Fatalf("last event = %+v", last)
	}
}

func TestConcurrentContextsAreIsolated(t *testing.T) {
	f := newFake(t)
	e := f.engine()

	const n = 8

	ids := make([]string, n)
	for i := range ids {
		ids[i] = mustCreate(t, e, "python").ID
	}

	var wg sync.WaitGroup

	errs := make(chan error, n)

	for i, id := range ids {
		wg.Add(1)

		go func() {
			defer wg.Done()

			ctx := context.Background()
			if err := e.Run(ctx, RunRequest{ContextID: id, Code: fmt.Sprintf("set:v=%d", i)}, func(Event) {}); err != nil {
				errs <- err

				return
			}

			var out string

			err := e.Run(ctx, RunRequest{ContextID: id, Code: "get:v"}, func(ev Event) {
				if ev.Type == EventStdout {
					out += ev.Text
				}
			})
			if err != nil {
				errs <- err

				return
			}

			if out != fmt.Sprint(i) {
				errs <- fmt.Errorf("context %d read %q", i, out)
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
}

func TestKernelDeath(t *testing.T) {
	for _, tc := range []struct{ code, ename string }{
		{"die", "KernelDied"},
		{"drop", "KernelConnectionLost"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			f := newFake(t)
			e := f.engine()
			c := mustCreate(t, e, "python")

			go func() {
				for range f.running {
				}
			}()

			evs := collect(t, e, RunRequest{ContextID: c.ID, Code: "print:partial\n" + tc.code})

			if got := types(evs); !reflect.DeepEqual(got, []EventType{EventInit, EventStdout, EventError}) {
				t.Fatalf("events = %v", got)
			}

			last := evs[len(evs)-1].Error
			if last.EName != tc.ename || !strings.Contains(last.EValue, c.ID) {
				t.Fatalf("error = %+v, want %s naming the context", last, tc.ename)
			}

			// The context is not left locked: the next run is accepted.
			collect(t, e, RunRequest{ContextID: c.ID, Code: "print:after"})
		})
	}

	// A drop after idle is not a death: the cell had finished, so the stream ends normally.
	t.Run("drop after idle", func(t *testing.T) {
		f := newFake(t)
		e := f.engine()
		c := mustCreate(t, e, "python")

		evs := collect(t, e, RunRequest{ContextID: c.ID, Code: "print:done\ndropafteridle"})
		if got := types(evs); !reflect.DeepEqual(got, []EventType{EventInit, EventStdout, EventComplete}) {
			t.Fatalf("events = %v", got)
		}
	})
}

func TestImplicitContextPerLanguage(t *testing.T) {
	f := newFake(t)
	e := f.engine()

	// Concurrent first requests must share one implicit context rather than race to start two.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			_ = e.Run(context.Background(), RunRequest{Language: "Python", Code: "set:shared=1"}, func(Event) {})
		}()
	}

	wg.Wait()

	if n := f.sessionCreates.Load(); n != 1 {
		t.Fatalf("sessions created = %d, want 1 implicit python context", n)
	}

	evs := collect(t, e, RunRequest{Language: "python", Code: "get:shared"})
	if evs[1].Text != "1" {
		t.Fatalf("implicit context did not keep state: %+v", evs[1])
	}

	collect(t, e, RunRequest{Language: "bash", Code: "print:x"})

	if got := len(e.ListContexts("python")); got != 1 {
		t.Fatalf("python contexts = %d", got)
	}

	if got := len(e.ListContexts("")); got != 2 {
		t.Fatalf("all contexts = %d", got)
	}

	// Deleting by language takes the implicit one too, and the next bare run makes a new one.
	if err := e.DeleteContextsByLanguage(context.Background(), "python"); err != nil {
		t.Fatal(err)
	}

	if got := e.ListContexts("python"); len(got) != 0 {
		t.Fatalf("after delete: %v", got)
	}

	evs = collect(t, e, RunRequest{Language: "python", Code: "get:shared"})
	if evs[1].Text != "undefined" {
		t.Fatalf("new implicit context inherited state: %+v", evs[1])
	}

	if n := f.sessionCreates.Load(); n != 3 {
		t.Fatalf("sessions created = %d, want 3", n)
	}
}

func TestContextLifecycle(t *testing.T) {
	f := newFake(t)
	e := f.engine()
	ctx := context.Background()

	py := mustCreate(t, e, "python")
	sh := mustCreate(t, e, "bash")

	if py.Language != "python" || !strings.HasPrefix(py.ID, "session-") {
		t.Fatalf("context = %+v: its id must be the Jupyter session id", py)
	}

	got, err := e.GetContext(py.ID)
	if err != nil || got != py {
		t.Fatalf("GetContext = %+v, %v", got, err)
	}

	if l := e.ListContexts("python"); len(l) != 1 || l[0] != py {
		t.Fatalf("ListContexts(python) = %v", l)
	}

	if l := e.ListContexts(""); len(l) != 2 || l[0] != py || l[1] != sh {
		t.Fatalf("ListContexts() = %v, want oldest first", l)
	}

	if err := e.DeleteContext(ctx, py.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := e.GetContext(py.ID); !errors.Is(err, ErrContextNotFound) {
		t.Fatalf("GetContext after delete = %v", err)
	}

	if err := e.DeleteContext(ctx, py.ID); !errors.Is(err, ErrContextNotFound) {
		t.Fatalf("second delete = %v", err)
	}

	if err := e.Run(ctx, RunRequest{ContextID: py.ID, Code: "print:x"}, func(Event) {}); !errors.Is(err, ErrContextNotFound) {
		t.Fatalf("Run on deleted = %v", err)
	}
}

func TestLanguages(t *testing.T) {
	f := newFake(t)
	e := f.engine()
	ctx := context.Background()

	if _, err := e.CreateContext(ctx, "cobol", ""); !errors.Is(err, ErrUnsupportedLanguage) {
		t.Fatalf("cobol = %v", err)
	}

	if err := e.Run(ctx, RunRequest{Language: "sql", Code: "select 1"}, func(Event) {}); !errors.Is(err, ErrUnsupportedLanguage) {
		t.Fatalf("sql = %v", err)
	}

	// A kernel language with no kernel installed says which ones are.
	_, err := e.CreateContext(ctx, "java", "")
	if !errors.Is(err, ErrUnsupportedLanguage) || !strings.Contains(err.Error(), "bash, python") {
		t.Fatalf("java = %v", err)
	}

	for lang, want := range map[string]bool{"python": true, " Go ": true, "command": false, "": false, "sql": false} {
		if IsKernelLanguage(lang) != want {
			t.Errorf("IsKernelLanguage(%q) = %v", lang, !want)
		}
	}
}

func TestKernelChoicePrefersTheImagesPythonOverPython3(t *testing.T) {
	f := newFake(t)
	f.specs = map[string]string{"python3": "python", "python3.14": "python", "a-bash": "bash", "z-bash": "bash"}
	e := f.engine()

	mustCreate(t, e, "python")

	if got := f.lastKernelName.Load(); got != "python3.14" {
		t.Fatalf("kernel = %v, want python3.14 (upstream skips python3)", got)
	}

	mustCreate(t, e, "bash")

	if got := f.lastKernelName.Load(); got != "a-bash" {
		t.Fatalf("kernel = %v, want the first by name, every time", got)
	}

	f.specs = map[string]string{"python3": "python"}
	mustCreate(t, e, "python")

	if got := f.lastKernelName.Load(); got != "python3" {
		t.Fatalf("kernel = %v: python3 alone must still serve python", got)
	}
}

func TestWorkingDirectory(t *testing.T) {
	f := newFake(t)
	e := f.engine()

	mustCreate(t, e, "python")

	if p := f.lastPath.Load().(string); strings.Contains(p, "/") || !strings.HasSuffix(p, ".ipynb") {
		t.Fatalf("path without cwd = %q, want a bare notebook name", p)
	}

	dir := t.TempDir() + "/nested/work"

	if _, err := e.CreateContext(context.Background(), "python", dir); err != nil {
		t.Fatal(err)
	}

	if p := f.lastPath.Load().(string); !strings.HasPrefix(p, dir+"/") {
		t.Fatalf("path = %q, want it under %s", p, dir)
	}
}

func TestTokenIsRequiredAndNamed(t *testing.T) {
	f := newFake(t)
	e := New(Config{BaseURL: f.srv.URL, Token: "wrong"})

	start := time.Now()

	_, err := e.CreateContext(context.Background(), "python", "")

	// A refused token is an answer, not a server still starting: retrying it for StartupWait
	// would turn a config mistake into a 30 second hang.
	if time.Since(start) > 2*time.Second || errors.Is(err, ErrUnavailable) {
		t.Fatalf("a 403 was retried: %v after %s", err, time.Since(start))
	}

	if err == nil || !strings.Contains(err.Error(), EnvToken) {
		t.Fatalf("err = %v, want it to name %s", err, EnvToken)
	}

	if f.sessionCreates.Load() != 0 {
		t.Fatal("a refused token was retried as if the server were starting")
	}
}

func TestCreateRetriesWhileJupyterStarts(t *testing.T) {
	f := newFake(t)
	f.failSessions.Store(2)
	e := f.engine()

	mustCreate(t, e, "python")

	if f.failSessions.Load() != 0 {
		t.Fatal("the 503s were not consumed")
	}

	// And gives up, naming the server, when it never comes up.
	dead := New(Config{BaseURL: "http://127.0.0.1:1", StartupWait: 300 * time.Millisecond})

	_, err := dead.CreateContext(context.Background(), "python", "")
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("err = %v", err)
	}
}

func TestAvailable(t *testing.T) {
	ctx := context.Background()

	if err := New(Config{}).Available(ctx); !errors.Is(err, ErrNotConfigured) || !strings.Contains(err.Error(), EnvHost) {
		t.Fatalf("unconfigured = %v", err)
	}

	if err := New(Config{BaseURL: "ftp://x"}).Available(ctx); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("bad scheme = %v", err)
	}

	if err := New(Config{BaseURL: "http://127.0.0.1:1"}).Available(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unreachable = %v", err)
	}

	f := newFake(t)
	if err := f.engine().Available(ctx); err != nil {
		t.Fatalf("fake = %v", err)
	}

	f.specs = map[string]string{}
	if err := f.engine().Available(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no kernels = %v", err)
	}

	// Every entry point refuses the same way, before touching the network.
	e := New(Config{})
	if _, err := e.CreateContext(ctx, "python", ""); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("CreateContext = %v", err)
	}

	if err := e.Run(ctx, RunRequest{Language: "python", Code: "1"}, func(Event) {}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Run = %v", err)
	}
}

func TestPingsDuringAQuietCell(t *testing.T) {
	f := newFake(t)
	e := f.engine(func(c *Config) { c.PingInterval = 20 * time.Millisecond })
	c := mustCreate(t, e, "python")

	var (
		mu    sync.Mutex
		pings int
	)

	done := make(chan error, 1)

	go func() {
		done <- e.Run(context.Background(), RunRequest{ContextID: c.ID, Code: "sleep"}, func(ev Event) {
			if ev.Type == EventPing {
				mu.Lock()
				pings++
				mu.Unlock()

				if ev.Text != "pong" {
					t.Errorf("ping text = %q", ev.Text)
				}
			}
		})
	}()

	<-f.running
	time.Sleep(150 * time.Millisecond)
	_ = e.Interrupt(context.Background(), c.ID)
	<-done

	mu.Lock()
	defer mu.Unlock()

	if pings < 2 {
		t.Fatalf("pings = %d over ~150ms at 20ms", pings)
	}
}

func TestBasePathIsKept(t *testing.T) {
	f := newFake(t)

	// Mount the fake under /jupyter, as a server with base_url=/jupyter/ would be.
	mux := http.NewServeMux()
	mux.Handle("/jupyter/", http.StripPrefix("/jupyter", http.HandlerFunc(f.serve)))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := New(Config{BaseURL: srv.URL + "/jupyter/", Token: f.token})
	c := mustCreate(t, e, "python")

	if got := types(collect(t, e, RunRequest{ContextID: c.ID, Code: "print:x"})); len(got) != 3 {
		t.Fatalf("events = %v", got)
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv(EnvHost, "")
	t.Setenv(EnvPort, "")
	t.Setenv(EnvToken, "")

	if c := ConfigFromEnv(); c.BaseURL != "" {
		t.Fatalf("empty env = %+v", c)
	}

	t.Setenv(EnvPort, "44771")
	t.Setenv(EnvToken, "tok")

	if c := ConfigFromEnv(); c.BaseURL != "http://127.0.0.1:44771" || c.Token != "tok" {
		t.Fatalf("port only = %+v", c)
	}

	t.Setenv(EnvHost, "http://jupyter:8888")

	if c := ConfigFromEnv(); c.BaseURL != "http://jupyter:8888" {
		t.Fatalf("host wins = %+v", c)
	}
}
