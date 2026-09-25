//go:build unix

package execd

// The SSE parsing below is copied from the OpenSandbox Go SDK at release-1.1.0
// (sdks/sandbox/go/streaming.go streamSSE and execution.go processStreamEvent), trimmed to what
// a command stream uses. Copyright 2025 The OpenSandbox Authors, Apache License 2.0
// (http://www.apache.org/licenses/LICENSE-2.0). It is copied rather than reimplemented so
// the tests parse execd's output exactly the way the client that has to work against it does:
// a framing mistake that a hand-written parser would forgive fails here.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

type sdkStreamEvent struct {
	Event string
	Data  string
}

func sdkStreamSSE(body io.Reader, handler func(sdkStreamEvent) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), math.MaxInt)

	var current sdkStreamEvent

	var dataLines []string

	eventCount := 0

	for {
		if !scanner.Scan() {
			if len(dataLines) > 0 {
				current.Data = strings.Join(dataLines, "\n")
				if err := handler(current); err != nil {
					return err
				}
				eventCount++
			}

			if err := scanner.Err(); err != nil {
				return fmt.Errorf("opensandbox: sse read: %w", err)
			}

			if eventCount == 0 {
				return fmt.Errorf("opensandbox: empty sse stream")
			}

			return nil
		}

		line := scanner.Text()

		if line == "" {
			if len(dataLines) > 0 {
				current.Data = strings.Join(dataLines, "\n")
				if err := handler(current); err != nil {
					return err
				}
				eventCount++
			}

			current = sdkStreamEvent{}
			dataLines = nil

			continue
		}

		if strings.HasPrefix(line, ":") {
			continue
		}

		if strings.HasPrefix(line, "{") {
			dataLines = append(dataLines, line)

			var probe struct{ Type string }
			if json.Unmarshal([]byte(line), &probe) == nil && probe.Type != "" {
				current.Event = probe.Type
			}

			continue
		}

		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")

		switch field {
		case "data":
			dataLines = append(dataLines, value)
		case "event":
			current.Event = value
		}
	}
}

// sdkExecution is the SDK's Execution, reduced to what the tests assert on.
type sdkExecution struct {
	ID       string
	Stdout   []string
	Stderr   []string
	ErrName  string
	ErrValue string
	Complete bool
	ExitCode *int
	Pings    int
	Types    []string
}

func (e *sdkExecution) Text() string { return strings.Join(e.Stdout, "\n") }

func sdkProcess(exec *sdkExecution, event sdkStreamEvent) error {
	if event.Data == "" {
		return nil
	}

	var ev struct {
		Type          string `json:"type"`
		Text          string `json:"text"`
		Timestamp     int64  `json:"timestamp"`
		ExecutionTime int64  `json:"execution_time,omitempty"`
		Error         *struct {
			EName     string   `json:"ename,omitempty"`
			EValue    string   `json:"evalue,omitempty"`
			Traceback []string `json:"traceback,omitempty"`
		} `json:"error,omitempty"`
	}

	if err := json.Unmarshal([]byte(event.Data), &ev); err != nil {
		exec.Stdout = append(exec.Stdout, event.Data)
		return nil
	}

	exec.Types = append(exec.Types, ev.Type)

	switch ev.Type {
	case "init":
		exec.ID = ev.Text
	case "stdout":
		exec.Stdout = append(exec.Stdout, ev.Text)
	case "stderr":
		exec.Stderr = append(exec.Stderr, ev.Text)
	case "error":
		if ev.Error != nil {
			exec.ErrName, exec.ErrValue = ev.Error.EName, ev.Error.EValue
		}

		if code, err := strconv.Atoi(exec.ErrValue); err == nil {
			exec.ExitCode = &code
		}
	case "execution_complete":
		exec.Complete = true

		if exec.ExitCode == nil && exec.ErrName == "" {
			zero := 0
			exec.ExitCode = &zero
		}
	case "ping":
		exec.Pings++
	default:
		if ev.Text != "" {
			exec.Stdout = append(exec.Stdout, ev.Text)
		}
	}

	return nil
}

// parseExecution runs a whole response body through the SDK's parser.
func parseExecution(body io.Reader) (*sdkExecution, error) {
	exec := &sdkExecution{}
	err := sdkStreamSSE(body, func(e sdkStreamEvent) error { return sdkProcess(exec, e) })

	return exec, err
}
