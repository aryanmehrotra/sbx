package osbclient

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// errStop ends a stream early without it being an error to the caller.
var errStop = errors.New("stop")

// readEvents parses a command stream and calls fn per event.
//
// Two framings arrive on the same endpoint. Standard SSE - `data: {...}` lines ended by a
// blank line - and the one execd has always actually written: one bare JSON object, then two
// newlines. Upstream's Python SDK sniffs the first byte to pick one; this accepts both line by
// line, since a line starting with `{` can only be a bare frame (an SSE field line starts with
// its field name) and JSON from an encoder never contains a raw newline.
func readEvents(r io.Reader, fn func(Event) error) error {
	br := bufio.NewReaderSize(r, 64*1024)

	var data [][]byte

	dispatch := func(payload []byte) error {
		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 {
			return nil
		}

		var ev Event
		if err := json.Unmarshal(payload, &ev); err != nil {
			return fmt.Errorf("opensandbox: command stream sent an event that is not JSON (%s): %w", clip(payload), err)
		}

		return fn(ev)
	}

	flush := func() error {
		if len(data) == 0 {
			return nil
		}

		payload := bytes.Join(data, []byte("\n"))
		data = data[:0]

		return dispatch(payload)
	}

	for {
		// ReadBytes rather than a Scanner: a single stdout line of a command is a single
		// event of unbounded size, and a Scanner's token limit would cut it off.
		line, rerr := br.ReadBytes('\n')
		line = bytes.TrimRight(line, "\r\n")

		var err error

		switch {
		case len(line) == 0:
			err = flush()
		case line[0] == '{':
			if err = flush(); err == nil {
				err = dispatch(line)
			}
		case line[0] == ':':
			// An SSE comment - a keepalive.
		default:
			field, value, _ := bytes.Cut(line, []byte(":"))
			value = bytes.TrimPrefix(value, []byte(" "))

			if string(field) == "data" {
				data = append(data, append([]byte(nil), value...))
			}
			// event:, id: and retry: carry nothing the payload does not already say.
		}

		if err != nil {
			if errors.Is(err, errStop) {
				return nil
			}

			return err
		}

		if rerr != nil {
			if rerr != io.EOF {
				return fmt.Errorf("opensandbox: command stream broke off: %w", rerr)
			}

			if err := flush(); err != nil && !errors.Is(err, errStop) {
				return err
			}

			return nil
		}
	}
}

func clip(b []byte) string {
	if len(b) > 120 {
		return strconv.Quote(string(b[:120])) + "..."
	}

	return strconv.Quote(string(b))
}
