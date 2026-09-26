package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

// counter is a background loop that proves a process survived: it keeps a number in a shell
// variable (memory, not disk) and writes it out each second with its own pid.
const counter = `sh -c 'echo $$ > /tmp/counter.pid; i=0; while :; do i=$((i+1)); echo $i > /tmp/counter; sleep 1; done'`

func readCounter(ctx context.Context, sb *opensandbox.Sandbox) (n int, pid string, err error) {
	out, _, _, err := run(ctx, sb, "cat /tmp/counter; cat /tmp/counter.pid; kill -0 $(cat /tmp/counter.pid) && echo alive")
	if err != nil {
		return 0, "", err
	}

	f := strings.Fields(out)
	if len(f) < 2 {
		return 0, "", fmt.Errorf("unexpected counter output %q", out)
	}

	n, _ = strconv.Atoi(f[0])
	if len(f) < 3 || f[2] != "alive" {
		return n, f[1], fmt.Errorf("counter process %s is not alive", f[1])
	}

	return n, f[1], nil
}

func startCounter(ctx context.Context, t *T, sb *opensandbox.Sandbox) {
	_, err := sb.RunCommandWithOpts(ctx, opensandbox.RunCommandRequest{Command: counter, Background: true}, nil)
	t.must(err, "start the background counter")

	ok := waitFor(ctx, 10*time.Second, 300*time.Millisecond, func() bool {
		n, _, err := readCounter(ctx, sb)
		return err == nil && n >= 2
	})
	t.check(ok, "background counter is running")
}

// caseIdle needs a daemon started with a short --idle (the -idle flag says how short). It must
// not call the API while waiting: any request is activity and restarts the idle clock.
func caseIdle(ctx context.Context, t *T, e *env) {
	idle, err := time.ParseDuration(e.arg("idle"))
	t.must(err, "-idle")

	// freeze: the default for an API sandbox.
	sb := e.create(ctx, t, opensandbox.SandboxCreateOptions{})
	defer kill(sb)

	startCounter(ctx, t, sb)
	_, pid0, _ := readCounter(ctx, sb)
	lastTouch := time.Now()

	name := container(sb.ID())
	var frozenAt time.Time

	frozen := waitFor(ctx, idle+90*time.Second, time.Second, func() bool {
		v, err := inspect(name, "{{.State.Paused}}")
		if v == "true" {
			frozenAt = time.Now()
		}

		return err == nil && v == "true"
	})
	if !t.check(frozen, "the idle daemon froze it (container paused) %v after the last request", time.Since(lastTouch).Round(time.Second)) {
		return
	}

	st, _ := inspect(name, "{{.State.Running}}")
	t.check(st == "true", "frozen, not stopped: the container is still running")

	time.Sleep(12 * time.Second) // frozen time the counter must NOT see

	n1, pid1, err := readCounter(ctx, sb) // this request is the wake
	t.check(err == nil && pid1 == pid0, "an API call woke it and the same counter process is alive (pid %s -> %s, %v)", pid0, pid1, err)

	elapsed := time.Since(lastTouch).Seconds()
	frozenFor := time.Since(frozenAt).Seconds()
	t.check(float64(n1) < elapsed-frozenFor+6, "the counter did not advance while frozen (count %d, %.0fs since start, %.0fs of it frozen)", n1, elapsed, frozenFor)

	time.Sleep(3 * time.Second)

	n2, _, err := readCounter(ctx, sb)
	t.check(err == nil && n2 > n1, "and it resumed counting from where it stopped (%d -> %d)", n1, n2)

	p, _ := inspect(name, "{{.State.Paused}}")
	t.check(p == "false", "the container is thawed")

	// sleep: opted into with extensions, stopped to 0 B, disk kept, processes not.
	sl := e.create(ctx, t, opensandbox.SandboxCreateOptions{Extensions: map[string]string{"sbx.idle": "sleep"}})
	defer kill(sl)

	startCounter(ctx, t, sl)
	lastTouch = time.Now()
	sname := container(sl.ID())

	stopped := waitFor(ctx, idle+90*time.Second, time.Second, func() bool {
		v, err := inspect(sname, "{{.State.Running}}")
		return err == nil && v == "false"
	})
	if !t.check(stopped, "sbx.idle=sleep stops the container (%v after the last request)", time.Since(lastTouch).Round(time.Second)) {
		return
	}

	out, _, _, err := run(ctx, sl, "cat /tmp/counter")
	t.check(err == nil && strings.TrimSpace(out) != "", "an API call wakes the slept sandbox and its disk is intact (counter file %q)", strings.TrimSpace(out))

	// Survival is judged by what the counter DOES, not by its PID. A container that was stopped
	// and started again numbers its processes from 1, so `kill -0` on the old PID can find a new,
	// unrelated process - the check's own shell included - and read "alive"; and an exec that came
	// back without an exit status read as 0. A counter that survived would still be counting.
	c1, _, _, err1 := run(ctx, sl, "cat /tmp/counter")
	time.Sleep(2500 * time.Millisecond)
	c2, _, _, err2 := run(ctx, sl, "cat /tmp/counter")
	t.check(err1 == nil && err2 == nil && strings.TrimSpace(c1) != "" && strings.TrimSpace(c1) == strings.TrimSpace(c2),
		"but the background process did not survive a sleep, as documented (counter %q, then %q 2.5s later)",
		strings.TrimSpace(c1), strings.TrimSpace(c2))

	v, _ := inspect(sname, "{{.State.Running}}")
	t.check(v == "true", "it is running again")
}

// casePause is POST /pause and /resume with a process running: the process is kept, the
// sandbox refuses traffic while paused, and a resumed process carries on.
func casePause(ctx context.Context, t *T, e *env) {
	sb := e.create(ctx, t, opensandbox.SandboxCreateOptions{})
	defer kill(sb)

	startCounter(ctx, t, sb)

	_, base, token, err := execd(ctx, sb)
	t.must(err, "execd endpoint")

	n0, pid0, _ := readCounter(ctx, sb)

	t.must(sb.Pause(ctx), "pause")
	pausedAt := time.Now()

	info, err := sb.GetInfo(ctx)
	t.check(err == nil && info.Status.State == opensandbox.StatePaused, "GetInfo says Paused (%v)", stateOf(info))

	p, _ := inspect(container(sb.ID()), "{{.State.Paused}}")
	t.check(p == "true", "the container is paused")

	// Straight at execd's address, as a client holding the endpoint would. A held connection
	// that never answers counts as refused too, so the client has its own short timeout.
	hc := &http.Client{Timeout: 5 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/files/info?path=/tmp", nil)
	req.Header.Set("X-EXECD-ACCESS-TOKEN", token)

	resp, err := hc.Do(req)
	if err == nil {
		resp.Body.Close()
	}

	t.check(err != nil || resp.StatusCode >= 400, "traffic to a paused sandbox is refused, not served (err=%v)", err)

	p, _ = inspect(container(sb.ID()), "{{.State.Paused}}")
	t.check(p == "true", "and that traffic did not wake it")

	time.Sleep(8 * time.Second)

	resumed, err := sb.Resume(ctx)
	t.must(err, "resume")

	sb = resumed

	info, err = sb.GetInfo(ctx)
	t.check(err == nil && info.Status.State == opensandbox.StateRunning, "GetInfo says Running after resume (%v)", stateOf(info))

	n1, pid1, err := readCounter(ctx, sb)
	pausedFor := time.Since(pausedAt).Seconds()
	t.check(err == nil && pid1 == pid0, "the same process is alive after resume (pid %s -> %s, %v)", pid0, pid1, err)
	t.check(float64(n1-n0) < pausedFor-2, "it did not count while paused (%d -> %d over %.0fs)", n0, n1, pausedFor)

	time.Sleep(2 * time.Second)

	n2, _, _ := readCounter(ctx, sb)
	t.check(n2 > n1, "and it carries on (%d -> %d)", n1, n2)
}

func stateOf(info *opensandbox.SandboxInfo) string {
	if info == nil {
		return "no info"
	}

	return string(info.Status.State)
}

// caseExpiryMake creates a 60 s sandbox and renews it. It writes "<id> <expiresAt RFC3339>" to
// -out for expiry-gone, which runs after the shell has restarted the daemon.
func caseExpiryMake(ctx context.Context, t *T, e *env) {
	timeout := 60
	sb := e.create(ctx, t, opensandbox.SandboxCreateOptions{TimeoutSeconds: &timeout})

	info, err := sb.GetInfo(ctx)
	t.must(err, "get info")

	if !t.check(info.ExpiresAt != nil && time.Until(*info.ExpiresAt) > 50*time.Second && time.Until(*info.ExpiresAt) <= 61*time.Second,
		"timeout=60 became an absolute expiresAt about a minute out (%v)", info.ExpiresAt) {
		kill(sb)
		return
	}

	_, err = sb.Renew(ctx, 10*time.Second)
	t.check(err != nil, "a renew that would move expiry EARLIER is refused (%v)", err)

	r, err := sb.Renew(ctx, 90*time.Second)
	t.check(err == nil && r.ExpiresAt.After(*info.ExpiresAt), "renew by 90s moves it later (%v -> %v)", info.ExpiresAt.Format(time.TimeOnly), r.ExpiresAt.Format(time.TimeOnly))

	info2, _ := sb.GetInfo(ctx)
	t.check(info2 != nil && info2.ExpiresAt != nil && info2.ExpiresAt.Equal(r.ExpiresAt), "GetInfo reports the renewed expiry")

	if err := writeOut(e.arg("out"), sb.ID()+" "+r.ExpiresAt.Format(time.RFC3339Nano)); err != nil {
		t.fail("%v", err)
		kill(sb)
	}
}

// caseExpiryGone runs against the restarted daemon: the sandbox is still there with the same
// expiry, and the reaper removes it once that passes - not before.
func caseExpiryGone(ctx context.Context, t *T, e *env) {
	id := e.arg("id")
	within, _ := time.ParseDuration(e.arg("within"))
	fields := strings.Fields(id)
	t.must(func() error {
		if len(fields) != 2 {
			return fmt.Errorf("-id wants \"<id> <expiresAt>\", got %q", id)
		}

		return nil
	}(), "-id")

	id = fields[0]
	want, err := time.Parse(time.RFC3339Nano, fields[1])
	t.must(err, "expiresAt")

	lc := opensandbox.NewLifecycleClient(e.url+"/v1", e.key)

	info, err := lc.GetSandbox(ctx, id)
	t.check(err == nil && info.ExpiresAt != nil && info.ExpiresAt.Equal(want),
		"after a daemon restart the sandbox is still there with the same expiry (%v)", err)

	var goneAt time.Time
	gone := waitFor(ctx, time.Until(want)+within, time.Second, func() bool {
		_, err := lc.GetSandbox(ctx, id)
		if statusOf(err) == http.StatusNotFound {
			goneAt = time.Now()
			return true
		}

		return false
	})

	if !t.check(gone, "the reaper removed it within %v of expiring", within) {
		mgr := opensandbox.NewSandboxManager(e.cfg)
		_ = mgr.KillSandbox(context.Background(), id)

		return
	}

	t.check(!goneAt.Before(want), "and not before it expired (%v after)", goneAt.Sub(want).Round(time.Second))

	_, err = inspect(container(id), "{{.Name}}")
	t.check(err != nil, "its container is gone too")
}

// caseCreate makes a sandbox for the shell to reason about, and writes its id to -out.
// -extensions is k=v,k=v.
func caseCreate(ctx context.Context, t *T, e *env) {
	ext := map[string]string{}
	for _, kv := range strings.Split(e.arg("extensions"), ",") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			ext[k] = v
		}
	}

	sb := e.create(ctx, t, opensandbox.SandboxCreateOptions{Extensions: ext})

	out, _, _, err := run(ctx, sb, "echo made-$HOSTNAME")
	t.check(err == nil && strings.Contains(out, "made-"), "it runs a command")

	if err := writeOut(e.arg("out"), sb.ID()); err != nil {
		t.fail("%v", err)
		kill(sb)
	}
}

// casePoke runs one command in an existing sandbox - a wake, if it was asleep.
func casePoke(ctx context.Context, t *T, e *env) {
	sb, err := opensandbox.ConnectSandbox(ctx, e.cfg, e.arg("id"))
	t.must(err, "connect "+e.arg("id"))

	out, _, code, err := run(ctx, sb, "echo poked")
	t.check(err == nil && code == 0 && strings.Contains(out, "poked"), "a command in %s answers", e.arg("id"))
}

func caseKill(ctx context.Context, t *T, e *env) {
	mgr := opensandbox.NewSandboxManager(e.cfg)
	t.check(mgr.KillSandbox(ctx, e.arg("id")) == nil, "kill %s", e.arg("id"))
}

// caseEgress is a sandbox that may reach one host, then two, changed while it runs. The
// sandbox's id goes to -out, and it is left running for the shell to read with `sbx egress`;
// the shell kills it.
func caseEgress(ctx context.Context, t *T, e *env) {
	sb := e.create(ctx, t, opensandbox.SandboxCreateOptions{
		NetworkPolicy: &opensandbox.NetworkPolicy{
			DefaultAction: "deny",
			Egress:        []opensandbox.NetworkRule{{Action: "allow", Target: "example.com"}},
		},
	})

	keep := false
	defer func() {
		if !keep {
			kill(sb)
		}
	}()

	// Reached means the far end answered with ANY status - a site may well refuse urllib's user
	// agent with its own 403. Denied is the proxy refusing the tunnel, which never gets that far.
	fetch := func(url string) (bool, string) {
		code := fmt.Sprintf("import urllib.request as u, urllib.error as ue\ntry:\n    print('STATUS', u.urlopen(%q, timeout=15).status)\nexcept ue.HTTPError as e:\n    print('STATUS', e.code)\nexcept Exception as e:\n    print('ERR', type(e).__name__, e)", url)
		out, _, _, err := run(ctx, sb, "python -c "+shellQuote(code))
		if err != nil {
			return false, err.Error()
		}

		return strings.Contains(out, "STATUS "), strings.TrimSpace(out)
	}

	ok, out := fetch("https://example.com/")
	t.check(ok, "the allowed host is reachable: %s", clip(out))

	ok, out = fetch("https://www.example.org/")
	t.check(!ok, "a host not on the list is denied: %s", clip(out))

	raw, _, _, _ := run(ctx, sb, `python -c "import socket
try:
    socket.create_connection(('1.1.1.1', 443), 5); print('CONNECTED')
except Exception as e:
    print('BLOCKED', e)"`)
	t.check(!strings.Contains(raw, "CONNECTED"), "a direct connection that ignores the proxy goes nowhere: %s", clip(raw))

	started, err := inspect(container(sb.ID()), "{{.State.StartedAt}}")
	t.must(err, "container start time")

	lc := opensandbox.NewLifecycleClient(e.url+"/v1", e.key)
	_, err = lc.PatchNetworkPolicy(ctx, sb.ID(), []opensandbox.NetworkRule{{Action: "allow", Target: "*.example.org"}})
	t.check(err == nil, "PATCH networkpolicy adds *.example.org (%v)", err)

	ok, out = fetch("https://www.example.org/")
	t.check(ok, "the newly allowed host now works, with no restart: %s", clip(out))

	ok, out = fetch("https://example.com/")
	t.check(ok, "and the original rule still holds: %s", clip(out))

	after, _ := inspect(container(sb.ID()), "{{.State.StartedAt}}")
	t.check(after == started, "the container was not restarted (StartedAt %s)", started)

	pol, err := lc.GetNetworkPolicy(ctx, sb.ID())
	if t.check(err == nil && pol.Policy != nil, "GET networkpolicy answers") {
		var targets []string
		for _, r := range pol.Policy.Egress {
			targets = append(targets, r.Action+":"+r.Target)
		}

		joined := strings.Join(targets, " ")
		t.check(strings.Contains(joined, "allow:example.com") && strings.Contains(joined, "allow:*.example.org") && pol.Policy.DefaultAction == "deny",
			"and it holds both rules and default deny: %s default=%s", joined, pol.Policy.DefaultAction)
	}

	// The other door: the SDK's sidecar calls (endpoint 18080), which sbx points back at the
	// same filter. What one door wrote, the other must read.
	side, err := sb.GetEgressPolicy(ctx)
	t.check(err == nil && side.Policy != nil && pol != nil && pol.Policy != nil && len(side.Policy.Egress) == len(pol.Policy.Egress),
		"the SDK's sidecar door (GetEgressPolicy) reads the same policy (%v)", err)

	if err := writeOut(e.arg("out"), sb.ID()); err == nil && t.failed == 0 {
		keep = true
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// caseFork is snapshot -> create from snapshotId: the fork has the data, its own token, and the
// source's token does not open it.
func caseFork(ctx context.Context, t *T, e *env) {
	src := e.create(ctx, t, opensandbox.SandboxCreateOptions{})
	defer kill(src)

	marker := "fork-" + randHex(6)
	_, _, code, err := run(ctx, src, "mkdir -p /srv/state && echo "+marker+" > /srv/state/marker")
	t.must(err, "write the marker")
	t.check(code == 0, "wrote %s into the source", marker)

	snap, err := src.CreateSnapshot(ctx, opensandbox.CreateSnapshotRequest{Name: "osbuse-" + marker})
	t.must(err, "create snapshot")

	mgr := opensandbox.NewSandboxManager(e.cfg)
	defer func() { _ = mgr.DeleteSnapshot(context.Background(), snap.ID) }()

	var state opensandbox.SnapshotState
	ready := waitFor(ctx, 2*time.Minute, time.Second, func() bool {
		s, err := mgr.GetSnapshot(ctx, snap.ID)
		if err == nil {
			state = s.Status.State
		}

		return state == opensandbox.SnapshotStateReady || state == opensandbox.SnapshotStateFailed
	})
	if !t.check(ready && state == opensandbox.SnapshotStateReady, "snapshot %s is Ready (%s)", snap.ID, state) {
		return
	}

	fork := e.create(ctx, t, opensandbox.SandboxCreateOptions{SnapshotID: snap.ID})
	defer kill(fork)

	t.check(fork.ID() != src.ID(), "the fork is a new sandbox (%s)", fork.ID())

	out, _, _, err := run(ctx, fork, "cat /srv/state/marker")
	t.check(err == nil && strings.TrimSpace(out) == marker, "the fork has the source's file: %q", strings.TrimSpace(out))

	_, forkBase, forkTok, err := execd(ctx, fork)
	t.must(err, "fork execd endpoint")
	_, _, srcTok, err := execd(ctx, src)
	t.must(err, "source execd endpoint")

	t.check(forkTok != "" && forkTok != srcTok, "the fork has its own execd token")

	stale := opensandbox.NewExecdClient(forkBase, srcTok)
	_, err = stale.ListDirectory(ctx, "/srv/state")
	t.check(statusOf(err) == http.StatusUnauthorized || statusOf(err) == http.StatusForbidden,
		"the SOURCE's token is rejected by the fork (%d %v)", statusOf(err), err)

	good := opensandbox.NewExecdClient(forkBase, forkTok)
	_, err = good.ListDirectory(ctx, "/srv/state")
	t.check(err == nil, "and the fork's own token is accepted (%v)", err)

	_, _, _, _ = run(ctx, fork, "echo fork-only > /srv/state/marker")
	out, _, _, _ = run(ctx, src, "cat /srv/state/marker")
	t.check(strings.TrimSpace(out) == marker, "a write in the fork is invisible to the source (%q)", strings.TrimSpace(out))
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)

	return hex.EncodeToString(b)
}

// caseVolumes: a named volume that outlives one sandbox and is picked up by the next, a host
// directory under --osb-host-paths, and a host directory outside it refused.
func caseVolumes(ctx context.Context, t *T, e *env) {
	claim := "osbuse-" + randHex(4)
	yes := true
	pvc := []opensandbox.Volume{{Name: "work", PVC: &opensandbox.PVC{ClaimName: claim, CreateIfNotExists: &yes}, MountPath: "/work"}}

	defer func() {
		_ = docker("volume", "rm", "sbx-osb-pvc-"+claim)
	}()

	a := e.create(ctx, t, opensandbox.SandboxCreateOptions{Volumes: pvc})
	_, _, code, err := run(ctx, a, "echo first-writer > /work/shared.txt")
	t.check(err == nil && code == 0, "sandbox A writes to pvc %s", claim)
	kill(a)

	b := e.create(ctx, t, opensandbox.SandboxCreateOptions{Volumes: pvc})
	defer kill(b)

	out, _, _, err := run(ctx, b, "cat /work/shared.txt")
	t.check(err == nil && strings.TrimSpace(out) == "first-writer", "sandbox B, created after A was removed, reads A's data: %q", strings.TrimSpace(out))

	hostdir := e.arg("hostdir")
	if hostdir == "" {
		t.fail("-hostdir is required")
		return
	}

	sub := filepath.Join(hostdir, "osbuse-"+randHex(4))
	t.must(os.MkdirAll(sub, 0o755), "mkdir "+sub)
	defer os.RemoveAll(sub)
	t.must(os.WriteFile(filepath.Join(sub, "from-host.txt"), []byte("hello from the host\n"), 0o644), "host file")

	h := e.create(ctx, t, opensandbox.SandboxCreateOptions{
		Volumes: []opensandbox.Volume{{Name: "hostdir", Host: &opensandbox.Host{Path: sub}, MountPath: "/mnt/host"}},
	})
	defer kill(h)

	out, _, _, err = run(ctx, h, "cat /mnt/host/from-host.txt && echo from-sandbox > /mnt/host/from-sandbox.txt")
	t.check(err == nil && strings.Contains(out, "hello from the host"), "the sandbox reads a file the host put there: %q", clip(out))

	back, rerr := os.ReadFile(filepath.Join(sub, "from-sandbox.txt"))
	t.check(rerr == nil && strings.TrimSpace(string(back)) == "from-sandbox", "and the host sees what the sandbox wrote (%v)", rerr)

	outside := e.arg("outside")
	if outside == "" {
		outside = "/etc"
	}

	_, err = opensandbox.CreateSandbox(ctx, e.cfg, opensandbox.SandboxCreateOptions{
		Image:   e.image,
		Volumes: []opensandbox.Volume{{Name: "nope", Host: &opensandbox.Host{Path: outside}, MountPath: "/mnt/nope"}},
	})
	t.check(err != nil, "a host path outside --osb-host-paths is refused at create (%s): %v", outside, err)

	escape := filepath.Join(sub, "..", "..")
	_, err = opensandbox.CreateSandbox(ctx, e.cfg, opensandbox.SandboxCreateOptions{
		Image:   e.image,
		Volumes: []opensandbox.Volume{{Name: "dots", Host: &opensandbox.Host{Path: escape}, MountPath: "/mnt/nope"}},
	})
	t.check(err != nil, "and so is a ../ escape from an allowed root: %v", err)
}

// caseConcurrent is n agents at once, each create -> run -> kill.
func caseConcurrent(ctx context.Context, t *T, e *env) {
	n, _ := strconv.Atoi(e.arg("n"))
	if n <= 0 {
		n = 10
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	start := time.Now()

	for i := range n {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			sb, err := opensandbox.CreateSandbox(ctx, e.cfg, opensandbox.SandboxCreateOptions{
				Image: e.image, ReadyTimeout: 3 * time.Minute, Metadata: map[string]string{"osbuse": "concurrent"},
			})
			if err != nil {
				errs[i] = fmt.Errorf("create: %w", err)
				return
			}

			defer func() {
				if kerr := sb.Kill(context.Background()); kerr != nil && errs[i] == nil {
					errs[i] = fmt.Errorf("kill: %w", kerr)
				}
			}()

			want := fmt.Sprintf("agent-%d-ok", i)
			out, _, code, err := run(ctx, sb, "echo "+want)
			if err != nil || code != 0 || !strings.Contains(out, want) {
				errs[i] = fmt.Errorf("run: out=%q code=%d err=%v", out, code, err)
			}
		}(i)
	}

	wg.Wait()

	failed := 0
	for i, err := range errs {
		if err != nil {
			failed++
			t.fail("agent %d: %v", i, err)
		}
	}

	t.check(failed == 0, "%d of %d parallel create->run->kill succeeded in %v", n-failed, n, time.Since(start).Round(time.Millisecond))
}
