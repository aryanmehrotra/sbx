package daemon

// The client half of the control endpoints - see control.go for the server half.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// control POSTs one control action to a deployment and returns its refusal, if any.
//
// The body the server sends on a non-2xx is the message a person needs - "this endpoint only
// fronts", "no such ref", whatever the provider said - so it is read back and returned rather
// than flattened to the status code. Bounded like every other reply this client reads: a
// control endpoint answers in a sentence, and an unbounded read is a wrong or hostile URL
// spending this process's memory.
func control(ctx context.Context, src *source, verb string, body map[string]any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		src.base.String()+"/v1/control/"+verb, bytes.NewReader(buf))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+src.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := fleetClient.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach %s: %w", src.base.Redacted(), err)
	}

	defer resp.Body.Close()

	if resp.StatusCode/100 == 2 {
		return nil
	}

	msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxFleetBody))

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%s rejected the token", src.base.Redacted())
	}

	if text := trimmed(msg); text != "" {
		return fmt.Errorf("%s", text)
	}

	return fmt.Errorf("%s answered %s", src.base.Redacted(), resp.Status)
}

// controlLogs streams a service's log tail from a deployment into w.
func controlLogs(ctx context.Context, src *source, ref string, lines int, w io.Writer) error {
	q := url.Values{"ref": {ref}}
	if lines > 0 {
		q.Set("tail", strconv.Itoa(lines))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		src.base.String()+"/v1/control/logs?"+q.Encode(), nil)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+src.token)

	resp, err := fleetClient.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach %s: %w", src.base.Redacted(), err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxFleetBody))
		if text := trimmed(msg); text != "" {
			return fmt.Errorf("%s", text)
		}

		return fmt.Errorf("%s answered %s", src.base.Redacted(), resp.Status)
	}

	// Bounded: a runaway or hostile endpoint streaming logs without end is the same memory
	// exposure the fleet cap guards against, just slower. A tail is small; this is generous.
	_, err = io.Copy(w, io.LimitReader(resp.Body, 1<<20))

	return err
}

func trimmed(b []byte) string {
	return string(bytes.TrimSpace(b))
}
