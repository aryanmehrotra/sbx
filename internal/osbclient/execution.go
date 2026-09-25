package osbclient

import "strconv"

// Output is one line (or chunk) a command wrote.
type Output struct {
	Text      string `json:"text"`
	Timestamp int64  `json:"timestamp"`
	IsError   bool   `json:"is_error"`
}

// Result is a rich result event - what a code interpreter returns for an expression.
type Result struct {
	Text            string            `json:"text"`
	Timestamp       int64             `json:"timestamp"`
	ExtraProperties map[string]string `json:"extra_properties"`
}

// ExecError is how a command failed. For a shell command, Value is the exit code as text.
type ExecError struct {
	Name      string   `json:"name"`
	Value     string   `json:"value"`
	Timestamp int64    `json:"timestamp"`
	Traceback []string `json:"traceback"`
}

// Complete is the end of an execution.
type Complete struct {
	Timestamp             int64 `json:"timestamp"`
	ExecutionTimeInMillis int64 `json:"execution_time_in_millis"`
}

// Logs is a command's output, split by stream.
type Logs struct {
	Stdout []Output `json:"stdout"`
	Stderr []Output `json:"stderr"`
}

// Execution is a command stream folded into one value. Its JSON is the shape the upstream
// SDKs' Execution model serializes to, field for field, so a tool that returns it matches
// upstream's MCP server.
type Execution struct {
	ID             *string    `json:"id"`
	ExecutionCount *int       `json:"execution_count"`
	Result         []Result   `json:"result"`
	Error          *ExecError `json:"error"`
	Complete       *Complete  `json:"complete"`
	ExitCode       *int       `json:"exit_code"`
	Logs           Logs       `json:"logs"`
}

// NewExecution returns an empty Execution with non-nil slices, so it serializes as [] rather
// than null before any output has arrived.
func NewExecution() *Execution {
	return &Execution{Result: []Result{}, Logs: Logs{Stdout: []Output{}, Stderr: []Output{}}}
}

// Apply folds one event in.
func (x *Execution) Apply(ev Event) {
	switch ev.Type {
	case "init":
		id := ev.Text
		x.ID = &id
	case "stdout":
		x.Logs.Stdout = append(x.Logs.Stdout, Output{Text: ev.Text, Timestamp: ev.Timestamp})
	case "stderr":
		x.Logs.Stderr = append(x.Logs.Stderr, Output{Text: ev.Text, Timestamp: ev.Timestamp, IsError: true})
	case "result":
		text, _ := ev.Results["text/plain"].(string)
		if t, ok := ev.Results["text"].(string); ok && text == "" {
			text = t
		}

		x.Result = append(x.Result, Result{Text: text, Timestamp: ev.Timestamp, ExtraProperties: map[string]string{}})
	case "error":
		if ev.Error == nil {
			return
		}

		tb := ev.Error.Traceback
		if tb == nil {
			tb = []string{}
		}

		x.Error = &ExecError{Name: ev.Error.Name, Value: ev.Error.Value, Timestamp: ev.Timestamp, Traceback: tb}
	case "execution_complete":
		x.Complete = &Complete{Timestamp: ev.Timestamp, ExecutionTimeInMillis: ev.ExecutionTime}
	case "execution_count":
		if ev.ExecutionCount != nil {
			n := *ev.ExecutionCount
			x.ExecutionCount = &n
		}
	}
}

// InferExitCode sets ExitCode the way the upstream SDKs do for a foreground command: an error
// event's value is the exit code; a clean completion is 0; anything else is unknown (nil) -
// a stream that broke off did not exit 0.
func (x *Execution) InferExitCode() {
	switch {
	case x.Error != nil:
		if n, err := strconv.Atoi(x.Error.Value); err == nil {
			x.ExitCode = &n
		}
	case x.Complete != nil:
		n := 0
		x.ExitCode = &n
	}
}
