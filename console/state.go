package main

import "sync"

// state is what the console knows, and it is only ever learned by reading the daemon's log
// stream. Nothing here is authoritative: the daemon owns every sandbox and this is a view of
// what it said it did. Keeping that one-way is what makes observability unable to affect the
// thing being observed.
var state = &store{svc: map[string]*service{}}

type service struct {
	Sandbox  string `json:"sandbox"`
	Service  string `json:"service"`
	Awake    bool   `json:"awake"`
	Wakes    int    `json:"wakes"`
	Sleeps   int    `json:"sleeps"`
	Failures int    `json:"wakeFailures"`
	LastMs   int64  `json:"lastWakeMs"`
}

type store struct {
	mu  sync.RWMutex
	svc map[string]*service
}

func (s *store) Snapshot() []service {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]service, 0, len(s.svc))
	for _, v := range s.svc {
		out = append(out, *v)
	}

	return out
}
