// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxEvents is how many events the event ring keeps.
const maxEvents = 1000

// TraceMem holds what BareProxy remembers about itself: a ring of recent
// request records (kept as their JSON lines, so trace-memory counts real
// bytes), a ring of events, and the people tailing the records. Every
// runtime inherits it from the one before, so a reload keeps it.
type TraceMem struct {
	mu      sync.Mutex
	recs    []memRec // recs[start:] are the records held, oldest first
	start   int
	bytes   int64
	limit   int64
	evicted int64 // records pushed out so far
	events  []Event
	subs    map[*Tail]bool
	tailers atomic.Int32 // len(subs), readable without the lock
	started time.Time
}

type memRec struct {
	at     int64 // when it was stored, in Unix nanoseconds
	status int
	perr   bool // the outcome was a proxy error, not the application's
	id     [16]byte
	js     []byte
}

// Event is a change in BareProxy's own state.
type Event struct {
	Time string `json:"time"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// Tail is one live subscription to the records as they happen.
type Tail struct {
	C       chan tailItem
	Dropped atomic.Int64
}

type tailItem struct {
	rec *Record
	js  []byte
}

// record writes a finished record to the trace log and to memory. With the
// log off, no ring and nobody tailing, nothing reads the record, so it isn't
// even turned into JSON.
func (rt *Runtime) record(rec *Record) {
	if rt.Cfg.TraceMem == 0 && (rt.Trace == nil || rt.Trace.Spec == "off") && rt.Mem.tailers.Load() == 0 {
		return
	}
	if js, err := RecordJSON(rec); err == nil {
		rt.Trace.WriteLine(js)
		rt.Mem.Add(rec, js, rt.Cfg.TraceMem)
	}
}

func newTraceMem() *TraceMem { return &TraceMem{started: time.Now()} }

// isProxyError reports outcomes where BareProxy, not the application,
// failed to deliver.
func isProxyError(outcome string) bool {
	return slices.Contains([]string{"no_backend", "connect_failed", "bad_response", "timeout"}, outcome)
}

// Add stores a finished record, given with its JSON. The oldest records go
// out first while the JSON held is over limit bytes. The record is shared
// with tailers, so it must not change afterwards.
func (m *TraceMem) Add(rec *Record, js []byte, limit int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for t := range m.subs {
		select {
		case t.C <- tailItem{rec, js}:
		default:
			t.Dropped.Add(1)
		}
	}
	m.limit = limit
	e := memRec{at: time.Now().UnixNano(), status: rec.Status, perr: isProxyError(rec.Outcome), js: js}
	copy(e.id[:], rec.ID)
	m.recs = append(m.recs, e)
	m.bytes += int64(len(js))
	for m.bytes > limit && m.start < len(m.recs) {
		m.bytes -= int64(len(m.recs[m.start].js))
		m.recs[m.start] = memRec{}
		m.start++
		m.evicted++
	}
	if m.start >= 4096 && m.start*2 >= len(m.recs) {
		n := copy(m.recs, m.recs[m.start:])
		clear(m.recs[n:])
		m.recs, m.start = m.recs[:n], 0
	}
}

// cleanPrefix checks a request ID prefix: 6 to 16 hex digits.
func cleanPrefix(p string) (string, error) {
	p = strings.ToLower(strings.TrimSpace(p))
	switch {
	case len(p) < 6:
		return "", errors.New("give at least 6 characters of the request ID")
	case len(p) > 16 || !lowerHex(p):
		return "", errors.New("a request ID is up to 16 hex digits")
	}
	return p, nil
}

// Find returns the JSON of the one record whose ID starts with prefix, and
// how many records match (more than one means the prefix isn't unique).
func (m *TraceMem) Find(prefix string) ([]byte, int) {
	if m == nil || len(prefix) > len(memRec{}.id) {
		return nil, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var js []byte
	n := 0
	for i := m.start; i < len(m.recs); i++ {
		if e := &m.recs[i]; string(e.id[:len(prefix)]) == prefix {
			js = e.js
			n++
		}
	}
	return js, n
}

// Stats counts what the ring holds.
type Stats struct {
	Records, Bytes, Limit int64
	Oldest                time.Time
}

// Stats reports the ring's size and the age of its oldest record.
func (m *TraceMem) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Stats{Records: int64(len(m.recs) - m.start), Bytes: m.bytes, Limit: m.limit}
	if m.start < len(m.recs) {
		st.Oldest = time.Unix(0, m.recs[m.start].at)
	}
	return st
}

// Window counts the records stored in the last d.
type Window struct {
	Requests    int `json:"requests"`
	Status5xx   int `json:"status_5xx"`
	ProxyErrors int `json:"proxy_errors"`
}

// Recent counts the requests, 5xx responses and proxy errors in the ring
// from the last d. complete is false when the ring has already pushed out
// records from inside that time, so the counts are a lower bound.
func (m *TraceMem) Recent(d time.Duration) (w Window, complete bool) {
	cut := time.Now().Add(-d).UnixNano()
	m.mu.Lock()
	defer m.mu.Unlock()
	i := len(m.recs) - 1
	for ; i >= m.start && m.recs[i].at >= cut; i-- {
		e := &m.recs[i]
		w.Requests++
		if e.status >= 500 {
			w.Status5xx++
		}
		if e.perr {
			w.ProxyErrors++
		}
	}
	return w, m.evicted == 0 || i >= m.start
}

// Subscribe starts a live feed of records. Records that arrive while the
// reader is behind are dropped and counted in Dropped, never waited for.
func (m *TraceMem) Subscribe() *Tail {
	t := &Tail{C: make(chan tailItem, 256)}
	m.mu.Lock()
	if m.subs == nil {
		m.subs = map[*Tail]bool{}
	}
	m.subs[t] = true
	m.tailers.Add(1)
	m.mu.Unlock()
	return t
}

// Unsubscribe ends a feed.
func (m *TraceMem) Unsubscribe(t *Tail) {
	m.mu.Lock()
	if m.subs[t] {
		delete(m.subs, t)
		m.tailers.Add(-1)
	}
	m.mu.Unlock()
}

// Event adds an event to the event ring.
func (m *TraceMem) Event(kind, text string) {
	if m == nil {
		return
	}
	text = strings.ReplaceAll(strings.TrimSpace(text), "\n", "; ") // one line each
	ev := Event{Time: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), Kind: kind, Text: text}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, ev)
	if len(m.events) > maxEvents {
		m.events = append(m.events[:0], m.events[len(m.events)-maxEvents:]...)
	}
}

// Events returns the events held, oldest first.
func (m *TraceMem) Events() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Event{}, m.events...)
}

// Started is when this process began keeping the ring, which is when it started.
func (m *TraceMem) Started() time.Time { return m.started }

// Event records a change in BareProxy's own state, such as an apply, a
// reload or a backend going down, for the events command.
func (s *Server) Event(kind, text string) { s.Current().Mem.Event(kind, text) }

// logEvent logs a line and records it as an event too.
func (s *Server) logEvent(kind, f string, a ...any) {
	s.logf(f, a...)
	s.Event(kind, fmt.Sprintf(f, a...))
}
