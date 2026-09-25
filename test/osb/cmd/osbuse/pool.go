package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

// casePool is a daemon started with `--osb-pool IMAGE=N --osb-pool-freeze`: N creates are served
// from the warm members, each as the caller's own sandbox, and a create the pool cannot honour
// takes the cold path.
//
// "Served from the pool" is asserted on what only a pool could produce: the sandbox's id is one
// whose container existed, frozen, before the create was sent. The rest is what a claim owes
// the caller - thawed, the request's env, a token minted for this request (the one the member
// was started with, readable from its container's env, is refused), and a history line naming
// the pool.
func casePool(ctx context.Context, t *T, e *env) {
	n, _ := strconv.Atoi(e.arg("n"))
	if n <= 0 {
		n = 3
	}

	// Frozen is the pool's own signal that a member is ready: it is frozen after execd answers,
	// just before it is offered.
	var members []string

	full := waitFor(ctx, 3*time.Minute, time.Second, func() bool {
		members = pausedMembers()
		return len(members) >= n
	})
	t.must(boolErr(full, fmt.Sprintf("%d of %d pool members ready", len(members), n)), "the pool fills")
	t.ok("the pool holds %d frozen members of %s", len(members), e.image)

	oldToken := map[string]string{}
	for _, id := range members {
		oldToken[id] = containerEnv(container(id), "EXECD_ACCESS_TOKEN")
	}

	var (
		made   []*opensandbox.Sandbox
		tokens = map[string]bool{}
	)

	defer func() {
		for _, sb := range made {
			kill(sb)
		}
	}()

	for i := range n {
		want := fmt.Sprintf("claimed-%d-%s", i, randHex(4))
		start := time.Now()

		sb := e.create(ctx, t, opensandbox.SandboxCreateOptions{
			Env: map[string]string{"POOL_PROBE": want}, Metadata: map[string]string{"osbuse": "pool"},
		})
		took := time.Since(start)
		made = append(made, sb)

		id := sb.ID()
		if !t.check(slices.Contains(members, id), "create %d (%v) is %s, a member that existed before it was asked for", i, took.Round(time.Millisecond), id) {
			continue
		}

		p, _ := inspect(container(id), "{{.State.Paused}}")
		t.check(p == "false", "%s was thawed for its caller", id)

		out, _, code, err := run(ctx, sb, "echo $POOL_PROBE")
		t.check(err == nil && code == 0 && strings.TrimSpace(out) == want, "%s has the request's env: %q", id, strings.TrimSpace(out))

		_, base, token, err := execd(ctx, sb)
		if !t.check(err == nil && token != "", "%s has an execd endpoint with a token (%v)", id, err) {
			continue
		}

		t.check(token != oldToken[id] && oldToken[id] != "", "%s's token is not the one its member was started with", id)
		t.check(!tokens[token], "%s's token is its own, not another claim's", id)
		tokens[token] = true

		t.check(execdStatus(ctx, base, token) == http.StatusOK, "execd answers %s's new token", id)

		old := execdStatus(ctx, base, oldToken[id])
		t.check(old == http.StatusUnauthorized || old == http.StatusForbidden, "execd refuses the member's old token (%d)", old)

		msg := createdMessage(e.arg("history"), id)
		t.check(strings.Contains(msg, "warm pool"), "history says %s came from the warm pool: %q", id, msg)
	}

	// A volume is something a member was made without: the create must not be served from the
	// pool, or the caller's storage would simply not be there.
	claim := "osbuse-" + randHex(4)
	yes := true

	defer func() { _ = docker("volume", "rm", "sbx-osb-pvc-"+claim) }()

	before := pausedMembers()
	cold := e.create(ctx, t, opensandbox.SandboxCreateOptions{
		Volumes: []opensandbox.Volume{{Name: "work", PVC: &opensandbox.PVC{ClaimName: claim, CreateIfNotExists: &yes}, MountPath: "/work"}},
	})
	made = append(made, cold)

	t.check(!slices.Contains(members, cold.ID()) && !slices.Contains(before, cold.ID()), "a create with a volume is a new sandbox (%s), not a member", cold.ID())

	msg := createdMessage(e.arg("history"), cold.ID())
	t.check(msg != "" && !strings.Contains(msg, "warm pool"), "and history records it as a cold create: %q", msg)

	out, _, code, err := run(ctx, cold, "echo mounted > /work/probe && cat /work/probe")
	t.check(err == nil && code == 0 && strings.TrimSpace(out) == "mounted", "its volume is mounted: %q", strings.TrimSpace(out))
}

// pausedMembers lists the ids of the frozen API sandboxes on the engine. Before any create of
// the case, those are the pool's ready members.
func pausedMembers() []string {
	out, err := exec.Command("docker", "ps", "--filter", "status=paused", "--filter", "name=^sbx-osb-",
		"--format", "{{.Names}}").Output()
	if err != nil {
		return nil
	}

	var ids []string

	for _, name := range strings.Fields(string(out)) {
		if id, ok := strings.CutSuffix(strings.TrimPrefix(name, "sbx-"), "-sandbox"); ok {
			ids = append(ids, id)
		}
	}

	return ids
}

func containerEnv(name, key string) string {
	out, err := inspect(name, "{{range .Config.Env}}{{println .}}{{end}}")
	if err != nil {
		return ""
	}

	for _, kv := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}

	return ""
}

// execdStatus is the status execd gives a request carrying token.
func execdStatus(ctx context.Context, base, token string) int {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/files/info?path=/tmp", nil)
	req.Header.Set("X-EXECD-ACCESS-TOKEN", token)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0
	}

	resp.Body.Close()

	return resp.StatusCode
}

// createdMessage is the message of the daemon's "created" event for id in the history file.
func createdMessage(path, id string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var msg string

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)

	for sc.Scan() {
		var r struct {
			Sandbox, Event, Message string
		}

		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Sandbox == id && r.Event == "created" {
			msg = r.Message
		}
	}

	return msg
}

func boolErr(ok bool, what string) error {
	if ok {
		return nil
	}

	return fmt.Errorf("%s", what)
}
