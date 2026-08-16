// Package update answers "is there a newer sbx" without ever making anybody wait for it.
//
// Three rules, because a version check is the kind of feature that quietly becomes a problem:
//
//   - It never blocks. The answer is read from a cache file; the network call that refreshes
//     that cache happens in the background and its result is used by the *next* run. A
//     dashboard that stutters for 300ms on a slow network because it wanted to tell you about
//     a patch release has made itself worse than the release it is advertising.
//   - It is off by default in anything automated. CI runners and scripts have no use for it
//     and would make the request on every invocation.
//   - It can be turned off entirely and says so. SBX_NO_UPDATE_CHECK=1.
//
// The request is a GET to the GitHub releases API with no identifiers attached beyond what
// any HTTP client sends.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Interval is how often the cache is refreshed. A day: new releases are not so frequent that
// a shorter one tells anybody anything, and a longer one makes the feature pointless.
const Interval = 24 * time.Hour

// cache is what is written to disk between runs.
type cache struct {
	Latest    string    `json:"latest"`
	CheckedAt time.Time `json:"checkedAt"`
}

func path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, ".sbx", "update.json"), nil
}

// Disabled reports whether the check has been turned off, or is in an environment where it
// should not run at all.
func Disabled() bool {
	if os.Getenv("SBX_NO_UPDATE_CHECK") != "" {
		return true
	}

	// Somebody's build pipeline is not somebody. Checking here means a CI job never makes the
	// request, rather than making it and discarding the answer.
	for _, k := range []string{"CI", "GITHUB_ACTIONS", "BUILDKITE", "JENKINS_URL", "GITLAB_CI"} {
		if os.Getenv(k) != "" {
			return true
		}
	}

	return false
}

// Available returns the newer version, or "" if there is nothing to say.
//
// Reads the cache only. Never makes a request, never blocks, and is safe to call while
// drawing a frame.
func Available(current string) string {
	if Disabled() {
		return ""
	}

	p, err := path()
	if err != nil {
		return ""
	}

	raw, err := os.ReadFile(p)
	if err != nil {
		return ""
	}

	var c cache
	if err := json.Unmarshal(raw, &c); err != nil {
		return ""
	}

	if Newer(current, c.Latest) {
		return c.Latest
	}

	return ""
}

// Refresh updates the cache in the background if it is stale. It returns immediately.
//
// The caller gets nothing back on purpose: the result is for the next run. Handing back a
// channel would invite somebody to wait on it, which is the behaviour this package exists to
// avoid.
func Refresh(repo string) {
	if Disabled() || !stale() {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		latest, err := fetch(ctx, repo)
		if err != nil {
			// Offline is the normal case, not an error worth showing anybody. The timestamp
			// is still written so a machine with no network does not retry on every run.
			latest = ""
		}

		write(cache{Latest: latest, CheckedAt: time.Now()})
	}()
}

func stale() bool {
	p, err := path()
	if err != nil {
		return false
	}

	raw, err := os.ReadFile(p)
	if err != nil {
		return true // never checked
	}

	var c cache
	if err := json.Unmarshal(raw, &c); err != nil {
		return true
	}

	return time.Since(c.CheckedAt) > Interval
}

func write(c cache) {
	p, err := path()
	if err != nil {
		return
	}

	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}

	raw, err := json.Marshal(c)
	if err != nil {
		return
	}

	// Written to a temporary file and renamed, so a run that is killed mid-write leaves the
	// previous answer rather than a truncated file that every later run fails to parse.
	tmp := p + ".tmp"

	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}

	_ = os.Rename(tmp, p)
}

func fetch(ctx context.Context, repo string) (string, error) {
	url := "https://api.github.com/repos/" + repo + "/releases/latest"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github answered %d", resp.StatusCode)
	}

	var body struct {
		TagName string `json:"tag_name"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}

	return body.TagName, nil
}

// Newer reports whether latest is a later release than current.
//
// Semver by hand, which is enough for vMAJOR.MINOR.PATCH and refuses everything else. A
// development build reports "dev" and must never be told it is out of date: somebody running
// a binary they just built from source does not need to be advised to download an older one.
func Newer(current, latest string) bool {
	if latest == "" || current == "" || current == "dev" {
		return false
	}

	c, ok := parse(current)
	if !ok {
		return false
	}

	l, ok := parse(latest)
	if !ok {
		return false
	}

	for i := range c {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}

	return false
}

// parse turns v1.2.3 into its three numbers. A pre-release suffix is ignored rather than
// ordered: telling somebody on v1.0.0 that v1.0.1-rc1 is available is not helpful.
func parse(v string) ([3]int, bool) {
	var out [3]int

	v = strings.TrimPrefix(strings.TrimSpace(v), "v")

	if i := strings.IndexAny(v, "-+"); i >= 0 {
		return out, false
	}

	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}

	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}

		out[i] = n
	}

	return out, true
}
