package osb

// Writing records without holding the server's lock.
//
// Every record write used to happen under s.mu: marshal, create a temp file, write, chmod,
// rename. That is a millisecond or so on a laptop, and s.mu is the lock every GET, list and
// create takes - so a burst of a hundred creates queued each other's file writes, and every GET
// in flight queued behind them. Now the record is copied under s.mu and written outside it.
//
// What still has to hold is order: two writes of one record must land in the order their
// changes were made, and a write must never land after the record was deleted (which would
// resurrect it on the next start). A lock per record gives both - striped, so there is no map
// of locks to grow and clean up. A writer takes its stripe, then copies the record as it is at
// that moment: whoever writes last copied last. remove takes the same stripe around deleting
// the file.

import (
	"hash/fnv"
	"sync"

	"github.com/aryanmehrotra/sbx/internal/history"
)

const persistStripes = 64

func (s *Server) persistLock(id string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))

	return &s.saveMu[h.Sum32()%persistStripes]
}

// persist writes a record's current state. A record already removed is not written.
func (s *Server) persist(id string) error {
	l := s.persistLock(id)
	l.Lock()
	defer l.Unlock()

	c, ok := s.snapshot(id)
	if !ok {
		return nil
	}

	return s.store.save(&c)
}

// history appends an event off the request path. The log is one file behind one mutex, opened
// and closed per line, and a create's answer should not wait in that queue for a record of
// itself; Close waits for it.
func (s *Server) history(r history.Record) {
	s.wg.Add(1)

	go func() {
		defer s.wg.Done()

		history.Append(r)
	}()
}
