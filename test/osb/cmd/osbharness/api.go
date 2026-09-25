package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The lifecycle API's key header, from specs/sandbox-lifecycle.yml at the pinned tag.
const keyHeader = "OPEN-SANDBOX-API-KEY"

type apiFlags struct {
	url string
	key string
}

func (a *apiFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&a.url, "url", "", "lifecycle API base, without /v1 (http://127.0.0.1:8080)")
	fs.StringVar(&a.key, "key", "", "value for the "+keyHeader+" header; empty sends none")
}

func (a *apiFlags) get(ctx context.Context, path string) (*http.Response, error) {
	return a.do(ctx, http.MethodGet, path)
}

func (a *apiFlags) do(ctx context.Context, method, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.url, "/")+"/v1"+path, nil)
	if err != nil {
		return nil, err
	}

	if a.key != "" {
		req.Header.Set(keyHeader, a.key)
	}

	return http.DefaultClient.Do(req)
}

// runReady waits until GET /v1/sandboxes answers 200, and when it gives up says WHICH of the
// three different problems it saw: nothing listening, something listening that refuses the
// key, or something listening that is not an OpenSandbox lifecycle API at all. Those need
// three different fixes and "not ready" names none of them.
func runReady(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("ready", flag.ContinueOnError)
	var a apiFlags
	a.register(fs)
	timeout := fs.Duration("timeout", 60*time.Second, "give up after this long")

	if err := fs.Parse(args); err != nil {
		return err
	}

	deadline := time.Now().Add(*timeout)
	last := errors.New("never asked")

	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		resp, err := a.get(ctx, "/sandboxes?page=1&pageSize=1")

		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			_ = resp.Body.Close()

			switch {
			case resp.StatusCode == http.StatusOK:
				cancel()
				_, err = fmt.Fprintf(out, "lifecycle API answering at %s/v1\n", a.url)

				return err
			case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
				last = fmt.Errorf("%s answers %d to the key it was given: the server wants a different "+
					"%s (set OPENSANDBOX_TEST_API_KEY)", a.url, resp.StatusCode, keyHeader)
			case resp.StatusCode == http.StatusNotFound:
				last = fmt.Errorf("%s is listening but has no /v1/sandboxes (404) - it is not an "+
					"OpenSandbox lifecycle API; body: %s", a.url, oneLine(body))
			default:
				last = fmt.Errorf("%s answered %d: %s", a.url, resp.StatusCode, oneLine(body))
			}
		} else {
			last = fmt.Errorf("nothing answering at %s: %v", a.url, err)
		}

		cancel()

		if time.Now().After(deadline) {
			return fmt.Errorf("gave up after %s: %w", *timeout, last)
		}

		time.Sleep(250 * time.Millisecond)
	}
}

type sandboxList struct {
	Items []struct {
		ID string `json:"id"`
	} `json:"items"`
}

// runSweep deletes, through the API, every sandbox the API lists whose id carries the
// prefix. It is only ever pointed at the throwaway daemon the conformance script started,
// on a docker endpoint it checked was empty of sbx sandboxes - and the prefix is a second
// guard on top of that, because "delete everything" is the one bug this must not have.
func runSweep(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("sweep", flag.ContinueOnError)
	var a apiFlags
	a.register(fs)
	prefix := fs.String("prefix", "osb-", "only delete ids starting with this; never empty")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *prefix == "" {
		return errors.New("-prefix must not be empty")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Collect first, then delete: deleting while paging shifts the pages under the cursor.
	var ids []string

	for page := 1; page <= 100; page++ {
		resp, err := a.get(ctx, "/sandboxes?"+url.Values{
			"page": {fmt.Sprint(page)}, "pageSize": {"100"},
		}.Encode())
		if err != nil {
			return err
		}

		var l sandboxList
		err = json.NewDecoder(resp.Body).Decode(&l)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("listing sandboxes: HTTP %d", resp.StatusCode)
		}

		if err != nil {
			return fmt.Errorf("listing sandboxes: %w", err)
		}

		for _, it := range l.Items {
			if strings.HasPrefix(it.ID, *prefix) {
				ids = append(ids, it.ID)
			}
		}

		if len(l.Items) < 100 {
			break
		}
	}

	var failed []string

	for _, id := range ids {
		resp, err := a.do(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(id))
		if err != nil {
			failed = append(failed, id+": "+err.Error())
			continue
		}

		_ = resp.Body.Close()

		// 404 is fine: the test's own cleanup got there first.
		if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
			failed = append(failed, fmt.Sprintf("%s: HTTP %d", id, resp.StatusCode))
			continue
		}

		fmt.Fprintf(out, "deleted leftover %s\n", id)
	}

	if len(failed) > 0 {
		return fmt.Errorf("could not delete: %s", strings.Join(failed, "; "))
	}

	return nil
}

func oneLine(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 160 {
		s = s[:160] + "..."
	}

	return s
}
