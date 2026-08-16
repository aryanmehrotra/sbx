package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// line is the subset of the daemon's JSON log this cares about. Everything else in the
// stream is for humans and is passed over without complaint - a console that fell over on
// an unfamiliar field would be a worse thing to run than no console.
type line struct {
	Sandbox    string `json:"sandbox"`
	Service    string `json:"service"`
	Event      string `json:"event"`
	DurationMs int64  `json:"durationMs"`
}

// observer receives one decoded event. The metrics backend implements it; the tests
// implement it too, which is why ingest does not import gofr.
type observer interface {
	Wake(sandbox, service string, ms int64)
	Sleep(sandbox, service string)
	WakeFailed(sandbox, service string)
}

// Ingest reads the daemon's log stream to exhaustion and reports what it saw.
//
// Every failure here is a dropped line, never a stopped console: a truncated write, a line
// of terminal output that is not JSON, an event from a newer daemon than this build knows.
// This is a monitoring surface, so a missing sample is a gap in a graph, and a crash is an
// outage of the thing that was supposed to tell you about outages.
func Ingest(r io.Reader, o observer) error {
	// A Reader, not a Scanner. Scanner gives up permanently on a line past its buffer -
	// one oversized line would end all metrics for the life of the process, which is a far
	// larger blast radius than the dropped sample this is willing to accept.
	br := bufio.NewReaderSize(r, 64*1024)

	for {
		raw, err := br.ReadString('\n')
		if raw == "" && err != nil {
			if errors.Is(err, io.EOF) {
				return nil // the daemon exited; that is not a failure of this program
			}

			return err
		}

		text := strings.TrimSpace(raw)
		if !strings.HasPrefix(text, "{") {
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}

			continue // a terminal-formatted line, or somebody's stray echo
		}

		var l line
		if err := json.Unmarshal([]byte(text), &l); err != nil {
			continue // truncated or malformed: drop the line, keep the console
		}

		switch l.Event {
		case "woke":
			o.Wake(l.Sandbox, l.Service, l.DurationMs)
		case "slept":
			o.Sleep(l.Sandbox, l.Service)
		case "wakeFailed":
			o.WakeFailed(l.Sandbox, l.Service)
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return err
		}
	}
}

// key identifies one service of one sandbox. Two sandboxes may each have a postgres.
func key(sandbox, service string) string { return sandbox + "/" + service }

// Wake records a wake and how long the caller waited for it.
func (s *store) Wake(sandbox, service string, ms int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	v := s.get(sandbox, service)
	v.Awake = true
	v.Wakes++
	v.LastMs = ms
}

func (s *store) Sleep(sandbox, service string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	v := s.get(sandbox, service)
	v.Awake = false
	v.Sleeps++
}

// WakeFailed counts a wake that did not happen. It deliberately does not mark the service
// awake: a failed wake leaves it exactly as asleep as it was.
func (s *store) WakeFailed(sandbox, service string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.get(sandbox, service).Failures++
}

func (s *store) get(sandbox, name string) *service {
	k := key(sandbox, name)
	if v, ok := s.svc[k]; ok {
		return v
	}

	v := &service{Sandbox: sandbox, Service: name}
	s.svc[k] = v

	return v
}
