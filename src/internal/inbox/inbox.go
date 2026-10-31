// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

// Package inbox is BareProxy's webhook inbox: incoming webhooks written to
// disk before the sender hears yes, then delivered to the app behind with
// retries and backoff, and kept so an operator can list and replay them.
//
// Each event is two files in the inbox's folder: ID.req, the request as it
// came (written once), and ID.state, its delivery state (rewritten after
// every change). Both are written to a temporary file, flushed to disk and
// renamed, and the folder is flushed after, so a crash leaves either the
// old file or the new one.
//
// The promise about duplicates: an event the app answered 2xx is sent again
// only when an operator replays it, and that delivery carries
// BareProxy-Inbox-Replay: operator. When BareProxy can't know whether an
// attempt reached the app (it stopped during the attempt, or the answer
// never came), every later attempt carries BareProxy-Inbox-Replay: in-doubt.
package inbox

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Request is a webhook as it came in.
type Request struct {
	Method   string      `json:"method"`
	Host     string      `json:"host"`
	Path     string      `json:"path"`
	Query    string      `json:"query,omitempty"`
	Header   http.Header `json:"header"`
	Received time.Time   `json:"received"`
	Body     []byte      `json:"-"` // after the JSON line in the file
}

// State is an event's delivery state.
type State struct {
	ID        string    `json:"id"`
	Received  time.Time `json:"received"`
	Path      string    `json:"path"`
	Size      int       `json:"size"`
	Key       string    `json:"key,omitempty"`    // the sender's own ID for the event, for dedupe
	Status    string    `json:"status"`           // pending, delivering, delivered or failed
	Since     time.Time `json:"since"`            // when this round of delivery began (received, or replayed)
	NextAt    time.Time `json:"next_at,omitzero"` // when a pending event is tried next
	Replay    string    `json:"replay,omitempty"` // what the next attempts are marked: operator or in-doubt
	Delivered time.Time `json:"delivered,omitzero"`
	Attempts  []Attempt `json:"attempts,omitempty"`
}

// Attempt is one try at delivering an event.
type Attempt struct {
	Start   time.Time `json:"start"`
	End     time.Time `json:"end,omitzero"`
	Status  int       `json:"status,omitempty"`
	Error   string    `json:"error,omitempty"`
	Replay  string    `json:"replay,omitempty"`   // what this attempt was marked
	InDoubt bool      `json:"in_doubt,omitempty"` // it may have reached the app with no answer
}

// Result is what one delivery attempt came to. InDoubt says the request
// may have reached the app although no answer came back.
type Result struct {
	Status  int
	Err     error
	InDoubt bool
}

// Deliver sends one attempt of an event to the app. attempt counts from 1
// within the current round; replay is "", "operator" or "in-doubt".
type Deliver func(ctx context.Context, id string, req *Request, attempt int, replay string) Result

// Options are an inbox's delivery settings.
type Options struct {
	Backoff    time.Duration // the wait after the first failed attempt; it doubles after each one
	MaxBackoff time.Duration // the longest wait between attempts
	GiveUp     time.Duration // an event still undelivered this long after its round began fails
	Keep       time.Duration // delivered and failed events are deleted this long after
	Workers    int           // attempts in flight at once
}

// Delay is the wait after the nth failed attempt of a round (n from 1).
func (o Options) Delay(n int) time.Duration {
	d := o.Backoff
	for i := 1; i < n && d < o.MaxBackoff; i++ {
		d *= 2
	}
	return min(d, o.MaxBackoff)
}

// Counts sums an inbox's events by status.
type Counts struct {
	Pending, Delivering, Delivered, Failed int
	OldestPending                          time.Time
}

// Inbox is one inbox and its folder.
type Inbox struct {
	dir string

	mu       sync.Mutex
	events   map[string]*State
	keys     map[string]string // dedupe key -> event ID
	inFlight map[string]bool
	opts     Options
	deliver  Deliver
	notify   func(string)
	pending  []string // notes made before Set gave a notify
	paused   bool

	wake   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	closed bool
}

// ErrNotFound is returned for an event ID the inbox doesn't hold.
var ErrNotFound = errors.New("no such event")

// Open reads an inbox's folder, making it if need be. An event that was
// being delivered when BareProxy stopped is pending again, and its next
// attempts are marked in-doubt. A request with no state file was never
// answered, so the sender will send it again, and it is deleted. Delivery
// starts once Set has given the inbox a Deliver.
func Open(dir string) (*Inbox, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	ib := &Inbox{dir: dir, events: map[string]*State{}, keys: map[string]string{}, inFlight: map[string]bool{},
		wake: make(chan struct{}, 1), paused: true}
	ib.ctx, ib.cancel = context.WithCancel(context.Background())
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	reqs := map[string]bool{}
	var recovered int
	for _, e := range ents {
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".tmp"):
			os.Remove(filepath.Join(dir, name))
		case strings.HasSuffix(name, ".req"):
			reqs[strings.TrimSuffix(name, ".req")] = true
		case strings.HasSuffix(name, ".state"):
			b, err := os.ReadFile(filepath.Join(dir, name))
			var st State
			if err != nil || json.Unmarshal(b, &st) != nil || st.ID+".state" != name {
				return nil, fmt.Errorf("the inbox file %s is broken", filepath.Join(dir, name))
			}
			if st.Status == "delivering" {
				n := len(st.Attempts) - 1
				st.Attempts[n].End = time.Now().UTC()
				st.Attempts[n].Error = "BareProxy stopped during the attempt"
				st.Attempts[n].InDoubt = true
				st.Status, st.NextAt = "pending", time.Now().UTC()
				st.Replay = cmp.Or(st.Replay, "in-doubt")
				if err := ib.saveState(&st); err != nil {
					return nil, err
				}
				recovered++
			}
			ib.events[st.ID] = &st
			if st.Key != "" {
				ib.keys[st.Key] = st.ID
			}
		}
	}
	for id := range reqs {
		if ib.events[id] == nil {
			os.Remove(filepath.Join(dir, id+".req"))
		}
	}
	for id := range ib.events {
		if !reqs[id] {
			return nil, fmt.Errorf("the inbox event %s has a state file but no request file in %s", id, dir)
		}
	}
	if recovered > 0 {
		ib.pending = append(ib.pending, fmt.Sprintf("%d %s in delivery when BareProxy stopped, so %s sent again, marked in-doubt",
			recovered, plural(recovered, "event was", "events were"), plural(recovered, "it is", "they are")))
	}
	ib.wg.Add(1)
	go ib.loop()
	return ib, nil
}

// Set gives the inbox its settings, its way to the app and where its notes
// go, and starts or resumes delivery.
func (ib *Inbox) Set(o Options, d Deliver, notify func(string)) {
	ib.mu.Lock()
	ib.opts, ib.deliver, ib.notify, ib.paused = o, d, notify, false
	notes := ib.pending
	ib.pending = nil
	ib.mu.Unlock()
	for _, n := range notes {
		ib.say(n)
	}
	ib.poke()
}

// Pause stops new attempts; attempts in flight finish. Set resumes.
func (ib *Inbox) Pause() {
	ib.mu.Lock()
	ib.paused = true
	ib.mu.Unlock()
}

// Close stops delivery and waits for attempts in flight. An attempt cut
// short is in doubt, and the next start sends it again, marked so.
func (ib *Inbox) Close() {
	ib.mu.Lock()
	if ib.closed {
		ib.mu.Unlock()
		return
	}
	ib.closed = true
	ib.mu.Unlock()
	ib.cancel()
	ib.wg.Wait()
}

func (ib *Inbox) poke() {
	select {
	case ib.wake <- struct{}{}:
	default:
	}
}

func (ib *Inbox) say(s string) {
	ib.mu.Lock()
	n := ib.notify
	if n == nil {
		ib.pending = append(ib.pending, s)
	}
	ib.mu.Unlock()
	if n != nil {
		n(s)
	}
}

// Add stores a request and returns its event ID once it is on disk. A key
// (the sender's own ID for the event) that the inbox already holds returns
// the earlier event's ID and stores nothing, with dup set.
func (ib *Inbox) Add(r *Request, key string) (id string, dup bool, err error) {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	if key != "" {
		if old, ok := ib.keys[key]; ok {
			return old, true, nil
		}
	}
	now := time.Now().UTC()
	r.Received = now
	id = newID(now)
	head, err := json.Marshal(r)
	if err != nil {
		return "", false, err
	}
	data := append(append(head, '\n'), r.Body...)
	if err := writeSync(filepath.Join(ib.dir, id+".req"), data); err != nil {
		return "", false, err
	}
	st := &State{ID: id, Received: now, Path: r.Path, Size: len(r.Body), Key: key, Status: "pending", Since: now, NextAt: now}
	if err := ib.saveState(st); err != nil {
		os.Remove(filepath.Join(ib.dir, id+".req"))
		return "", false, err
	}
	ib.events[id] = st
	if key != "" {
		ib.keys[key] = id
	}
	ib.poke()
	return id, false, nil
}

// List returns every event's state, newest first.
func (ib *Inbox) List() []State {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	out := make([]State, 0, len(ib.events))
	for _, st := range ib.events {
		out = append(out, cloneState(st))
	}
	slices.SortFunc(out, func(a, b State) int { return strings.Compare(b.ID, a.ID) })
	return out
}

// Get returns one event's state and request.
func (ib *Inbox) Get(id string) (State, *Request, error) {
	ib.mu.Lock()
	st, ok := ib.events[id]
	var cp State
	if ok {
		cp = cloneState(st)
	}
	ib.mu.Unlock()
	if !ok {
		return State{}, nil, ErrNotFound
	}
	r, err := ib.readReq(id)
	return cp, r, err
}

// Replay sends an event again, marked as a replay. An event in delivery
// right now can't be replayed.
func (ib *Inbox) Replay(id string) error {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	st, ok := ib.events[id]
	switch {
	case !ok:
		return ErrNotFound
	case st.Status == "delivering" || st.Status == "pending" && st.Replay == "operator":
		return fmt.Errorf("event %s is already on its way", id)
	}
	now := time.Now().UTC()
	old := cloneState(st)
	st.Status, st.Replay, st.Since, st.NextAt, st.Delivered = "pending", "operator", now, now, time.Time{}
	if err := ib.saveState(st); err != nil {
		*st = old
		return err
	}
	ib.poke()
	return nil
}

// Counts sums the events by status.
func (ib *Inbox) Counts() Counts {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	var c Counts
	for _, st := range ib.events {
		switch st.Status {
		case "pending":
			c.Pending++
			if c.OldestPending.IsZero() || st.Received.Before(c.OldestPending) {
				c.OldestPending = st.Received
			}
		case "delivering":
			c.Delivering++
		case "delivered":
			c.Delivered++
		case "failed":
			c.Failed++
		}
	}
	return c
}

// loop starts attempts as events fall due, and sweeps old events.
func (ib *Inbox) loop() {
	defer ib.wg.Done()
	timer := time.NewTimer(0)
	defer timer.Stop()
	nextSweep := time.Time{}
	for {
		now := time.Now()
		if !now.Before(nextSweep) {
			ib.sweep(now)
			nextSweep = now.Add(time.Hour)
		}
		wait := ib.startDue(now)
		timer.Reset(min(wait, time.Until(nextSweep)))
		select {
		case <-ib.ctx.Done():
			return
		case <-ib.wake:
		case <-timer.C:
		}
	}
}

// startDue starts attempts for the events that are due, oldest first, up to
// the number of workers, and returns how long until the next one is due.
func (ib *Inbox) startDue(now time.Time) time.Duration {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	wait := time.Hour
	if ib.paused || ib.deliver == nil || ib.ctx.Err() != nil {
		return wait
	}
	var due []*State
	for _, st := range ib.events {
		if st.Status != "pending" || ib.inFlight[st.ID] {
			continue
		}
		if d := st.NextAt.Sub(now); d > 0 {
			wait = min(wait, d)
			continue
		}
		due = append(due, st)
	}
	slices.SortFunc(due, func(a, b *State) int { return strings.Compare(a.ID, b.ID) })
	for _, st := range due {
		if len(ib.inFlight) >= max(ib.opts.Workers, 1) {
			break
		}
		at := Attempt{Start: time.Now().UTC(), Replay: st.Replay}
		st.Status = "delivering"
		st.Attempts = append(st.Attempts, at)
		if err := ib.saveState(st); err != nil {
			// The attempt isn't on disk, so it isn't made; the folder may
			// be full. Try again a little later.
			st.Status, st.Attempts = "pending", st.Attempts[:len(st.Attempts)-1]
			st.NextAt = now.Add(5 * time.Second)
			wait = min(wait, 5*time.Second)
			continue
		}
		ib.inFlight[st.ID] = true
		n := ib.roundAttempts(st)
		ib.wg.Add(1)
		go ib.attempt(st.ID, n, st.Replay, ib.deliver)
	}
	return wait
}

// roundAttempts counts the attempts since the round began.
func (ib *Inbox) roundAttempts(st *State) int {
	n := 0
	for _, a := range st.Attempts {
		if !a.Start.Before(st.Since) {
			n++
		}
	}
	return n
}

func (ib *Inbox) attempt(id string, n int, replay string, deliver Deliver) {
	defer ib.wg.Done()
	req, err := ib.readReq(id)
	var res Result
	if err != nil {
		res = Result{Err: err}
	} else {
		res = deliver(ib.ctx, id, req, n, replay)
	}
	stopped := ib.ctx.Err() != nil
	ib.mu.Lock()
	delete(ib.inFlight, id)
	st := ib.events[id]
	now := time.Now().UTC()
	a := &st.Attempts[len(st.Attempts)-1]
	a.End, a.Status = now, res.Status
	if res.Err != nil {
		a.Error = res.Err.Error()
	}
	var note string
	switch {
	case res.Err == nil && res.Status >= 200 && res.Status < 300:
		st.Status, st.Delivered, st.Replay, st.NextAt = "delivered", now, "", time.Time{}
	default:
		if res.InDoubt || stopped {
			a.InDoubt = true
			st.Replay = cmp.Or(st.Replay, "in-doubt")
		}
		if stopped && a.Error == "" && res.Status == 0 {
			a.Error = "BareProxy stopped during the attempt"
		}
		st.Status = "pending"
		st.NextAt = now.Add(ib.opts.Delay(n))
		if !stopped && now.Sub(st.Since) >= ib.opts.GiveUp {
			st.Status, st.NextAt = "failed", time.Time{}
			note = fmt.Sprintf("event %s failed for good after %d %s since %s; replay it once the app takes it", id, n,
				plural(n, "attempt", "attempts"), st.Since.Format("2006-01-02 15:04:05 UTC"))
		}
	}
	err = ib.saveState(st)
	ib.mu.Unlock()
	if err != nil {
		// The outcome isn't on disk; the next start finds the event in
		// delivery and sends it again, marked in-doubt, which is safe.
		ib.say(fmt.Sprintf("event %s: can't write its state: %v", id, err))
	}
	if note != "" {
		ib.say(note)
	}
	ib.poke()
}

// sweep deletes delivered and failed events older than Keep.
func (ib *Inbox) sweep(now time.Time) {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	if ib.opts.Keep <= 0 {
		return
	}
	for id, st := range ib.events {
		end := st.Delivered
		if st.Status == "failed" && len(st.Attempts) > 0 {
			end = st.Attempts[len(st.Attempts)-1].End
		}
		if (st.Status == "delivered" || st.Status == "failed") && now.Sub(end) > ib.opts.Keep {
			if os.Remove(filepath.Join(ib.dir, id+".state")) == nil {
				os.Remove(filepath.Join(ib.dir, id+".req"))
				delete(ib.events, id)
				if st.Key != "" {
					delete(ib.keys, st.Key)
				}
			}
		}
	}
}

func (ib *Inbox) readReq(id string) (*Request, error) {
	b, err := os.ReadFile(filepath.Join(ib.dir, id+".req"))
	if err != nil {
		return nil, err
	}
	head, body, ok := bytes.Cut(b, []byte("\n"))
	r := &Request{}
	if !ok || json.Unmarshal(head, r) != nil {
		return nil, fmt.Errorf("the request file of event %s is broken", id)
	}
	r.Body = body
	return r, nil
}

func (ib *Inbox) saveState(st *State) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return writeSync(filepath.Join(ib.dir, st.ID+".state"), b)
}

// writeSync writes a file so that a crash leaves the old one or the new one:
// a temporary file, flushed, renamed over it, and the folder flushed.
func writeSync(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		d.Sync() // not possible on every system (Windows), and not needed there
		d.Close()
	}
	return nil
}

// newID makes an event ID that sorts by the time it was received.
func newID(t time.Time) string {
	var b [3]byte
	rand.Read(b[:])
	return fmt.Sprintf("%x%s", t.UnixNano(), hex.EncodeToString(b[:]))
}

func cloneState(st *State) State {
	cp := *st
	cp.Attempts = slices.Clone(st.Attempts)
	return cp
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// IsNotFound reports whether err says there's no such event.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) || errors.Is(err, fs.ErrNotExist) }
