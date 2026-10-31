// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package inbox

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// app is a Deliver that records every delivery and answers what answer says.
type app struct {
	mu     sync.Mutex
	got    []call
	answer func(n int, c call) Result
	seen   chan call
}

type call struct {
	ID, Replay string
	Attempt    int
	At         time.Time
	Body       string
}

func newApp(answer func(n int, c call) Result) *app {
	return &app{answer: answer, seen: make(chan call, 1000)}
}

func (a *app) deliver(ctx context.Context, id string, req *Request, attempt int, replay string) Result {
	c := call{ID: id, Replay: replay, Attempt: attempt, At: time.Now(), Body: string(req.Body)}
	a.mu.Lock()
	a.got = append(a.got, c)
	n := len(a.got)
	a.mu.Unlock()
	a.seen <- c
	if a.answer == nil {
		return Result{Status: 200}
	}
	return a.answer(n, c)
}

func (a *app) calls() []call {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]call(nil), a.got...)
}

var fast = Options{Backoff: 40 * time.Millisecond, MaxBackoff: 160 * time.Millisecond, GiveUp: time.Hour, Keep: time.Hour, Workers: 4}

func open(t *testing.T, dir string, o Options, a *app) *Inbox {
	t.Helper()
	ib, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ib.Close)
	ib.Set(o, a.deliver, func(s string) { t.Log("note: " + s) })
	return ib
}

func add(t *testing.T, ib *Inbox, body, key string) string {
	t.Helper()
	id, _, err := ib.Add(&Request{Method: "POST", Host: "hooks.test", Path: "/hook", Header: http.Header{"X-Test": {"1"}}, Body: []byte(body)}, key)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func status(ib *Inbox, id string) string {
	st, _, _ := ib.Get(id)
	return st.Status
}

func TestAddDeliverListGetReplay(t *testing.T) {
	a := newApp(nil)
	ib := open(t, t.TempDir(), fast, a)
	id := add(t, ib, `{"type":"invoice.paid"}`, "")
	waitFor(t, "delivery", func() bool { return status(ib, id) == "delivered" })
	st, req, err := ib.Get(id)
	if err != nil || string(req.Body) != `{"type":"invoice.paid"}` || req.Header.Get("X-Test") != "1" || len(st.Attempts) != 1 || st.Attempts[0].Status != 200 {
		t.Fatalf("Get: %+v %+v %v", st, req, err)
	}
	if l := ib.List(); len(l) != 1 || l[0].ID != id {
		t.Fatalf("List: %+v", l)
	}
	if c := a.calls(); len(c) != 1 || c[0].Replay != "" || c[0].Attempt != 1 {
		t.Fatalf("calls %+v", c)
	}
	if err := ib.Replay(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the replay", func() bool { return len(a.calls()) == 2 && status(ib, id) == "delivered" })
	if c := a.calls()[1]; c.Replay != "operator" || c.Attempt != 1 {
		t.Fatalf("replay call %+v", c)
	}
	if err := ib.Replay("nope"); !IsNotFound(err) {
		t.Fatalf("replay of a missing event: %v", err)
	}
	if c := ib.Counts(); c.Delivered != 1 || c.Pending != 0 {
		t.Fatalf("counts %+v", c)
	}
}

func TestSendersKeyStoresOnce(t *testing.T) {
	a := newApp(nil)
	ib := open(t, t.TempDir(), fast, a)
	id1 := add(t, ib, "x", "evt_1")
	id2, dup, err := ib.Add(&Request{Method: "POST", Path: "/hook", Header: http.Header{}, Body: []byte("x")}, "evt_1")
	if err != nil || !dup || id2 != id1 {
		t.Fatalf("second add with the same key: %s %v %v", id2, dup, err)
	}
	waitFor(t, "delivery", func() bool { return status(ib, id1) == "delivered" })
	time.Sleep(100 * time.Millisecond)
	if n := len(a.calls()); n != 1 {
		t.Fatalf("%d deliveries, want 1", n)
	}
}

// TestUpstreamDownThenBack: the app fails four times, then takes the event.
// It is delivered once, and the waits between attempts follow the backoff.
func TestUpstreamDownThenBack(t *testing.T) {
	a := newApp(func(n int, c call) Result {
		if n <= 4 {
			return Result{Err: errors.New("connect refused")}
		}
		return Result{Status: 204}
	})
	ib := open(t, t.TempDir(), fast, a)
	id := add(t, ib, "x", "")
	waitFor(t, "delivery", func() bool { return status(ib, id) == "delivered" })
	c := a.calls()
	if len(c) != 5 {
		t.Fatalf("%d attempts, want 5", len(c))
	}
	want := []time.Duration{40, 80, 160, 160}
	for i, w := range want {
		gap := c[i+1].At.Sub(c[i].At)
		if gap < w*time.Millisecond-5*time.Millisecond {
			t.Errorf("wait before attempt %d was %s, want at least %dms", i+2, gap, w)
		}
		if c[i+1].Attempt != i+2 || c[i+1].Replay != "" {
			t.Errorf("attempt %d: %+v", i+2, c[i+1])
		}
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(a.calls()); n != 5 {
		t.Fatalf("delivered again after a 2xx: %d calls", n)
	}
}

func TestNon2xxIsRetriedAndInDoubtIsMarked(t *testing.T) {
	a := newApp(func(n int, c call) Result {
		switch n {
		case 1:
			return Result{Status: 500}
		case 2:
			return Result{Err: errors.New("timeout"), InDoubt: true}
		}
		return Result{Status: 200}
	})
	ib := open(t, t.TempDir(), fast, a)
	id := add(t, ib, "x", "")
	waitFor(t, "delivery", func() bool { return status(ib, id) == "delivered" })
	c := a.calls()
	if len(c) != 3 || c[0].Replay != "" || c[1].Replay != "" || c[2].Replay != "in-doubt" {
		t.Fatalf("calls %+v", c)
	}
	st, _, _ := ib.Get(id)
	if !st.Attempts[1].InDoubt || st.Attempts[0].InDoubt || st.Replay != "" {
		t.Fatalf("state %+v", st)
	}
}

func TestGiveUpThenReplay(t *testing.T) {
	down := true
	var mu sync.Mutex
	a := newApp(func(n int, c call) Result {
		mu.Lock()
		defer mu.Unlock()
		if down {
			return Result{Status: 503}
		}
		return Result{Status: 200}
	})
	o := fast
	o.GiveUp = 150 * time.Millisecond
	ib := open(t, t.TempDir(), o, a)
	id := add(t, ib, "x", "")
	waitFor(t, "giving up", func() bool { return status(ib, id) == "failed" })
	n := len(a.calls())
	time.Sleep(300 * time.Millisecond)
	if len(a.calls()) != n {
		t.Fatal("a failed event was tried again on its own")
	}
	mu.Lock()
	down = false
	mu.Unlock()
	if err := ib.Replay(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the replay", func() bool { return status(ib, id) == "delivered" })
	if c := a.calls(); c[len(c)-1].Replay != "operator" || c[len(c)-1].Attempt != 1 {
		t.Fatalf("last call %+v", c[len(c)-1])
	}
}

// TestCrashDuringDeliveryIsSentAgainMarked copies the inbox's folder while
// an attempt is in flight, which is what a SIGKILL leaves on disk, and
// opens the copy: the event goes again, marked in-doubt.
func TestCrashDuringDeliveryIsSentAgainMarked(t *testing.T) {
	dir, snap := t.TempDir(), t.TempDir()
	hold := make(chan struct{})
	a := newApp(func(n int, c call) Result {
		copyDir(t, dir, snap)
		<-hold
		return Result{Status: 200}
	})
	ib := open(t, dir, fast, a)
	id := add(t, ib, "x", "")
	<-a.seen
	close(hold)
	waitFor(t, "delivery", func() bool { return status(ib, id) == "delivered" })

	b := newApp(nil)
	ib2 := open(t, snap, fast, b)
	waitFor(t, "the resend", func() bool { return status(ib2, id) == "delivered" })
	c := b.calls()
	if len(c) != 1 || c[0].Replay != "in-doubt" || c[0].Attempt != 2 {
		t.Fatalf("after the crash: %+v", c)
	}
	st, _, _ := ib2.Get(id)
	if len(st.Attempts) != 2 || !st.Attempts[0].InDoubt || !strings.Contains(st.Attempts[0].Error, "stopped") {
		t.Fatalf("state %+v", st)
	}
}

func copyDir(t *testing.T, from, to string) {
	ents, _ := os.ReadDir(from)
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(from, e.Name()))
		if err == nil {
			os.WriteFile(filepath.Join(to, e.Name()), b, 0o640)
		}
	}
}

func TestCloseDuringDeliveryIsSentAgainMarked(t *testing.T) {
	dir := t.TempDir()
	block := make(chan struct{})
	ib, _ := Open(dir)
	ib.Set(fast, func(ctx context.Context, id string, req *Request, attempt int, replay string) Result {
		close(block)
		<-ctx.Done()
		return Result{Err: ctx.Err(), InDoubt: true}
	}, nil)
	id := add(t, ib, "x", "")
	<-block
	ib.Close()
	b := newApp(nil)
	ib2 := open(t, dir, fast, b)
	waitFor(t, "the resend", func() bool { return status(ib2, id) == "delivered" })
	if c := b.calls(); len(c) != 1 || c[0].Replay != "in-doubt" {
		t.Fatalf("after Close: %+v", c)
	}
}

func TestDeliveredEventsStayDeliveredAfterRestart(t *testing.T) {
	dir := t.TempDir()
	a := newApp(nil)
	ib := open(t, dir, fast, a)
	var ids []string
	for i := 0; i < 20; i++ {
		ids = append(ids, add(t, ib, "x", ""))
	}
	waitFor(t, "deliveries", func() bool { return ib.Counts().Delivered == 20 })
	ib.Close()
	b := newApp(nil)
	ib2 := open(t, dir, fast, b)
	time.Sleep(200 * time.Millisecond)
	if n := len(b.calls()); n != 0 || ib2.Counts().Delivered != 20 {
		t.Fatalf("after a clean restart: %d deliveries, counts %+v", n, ib2.Counts())
	}
}

func TestOrphanRequestFileIsDropped(t *testing.T) {
	dir := t.TempDir()
	// A request written, then a crash before its state: the sender never
	// got a yes, so it sends again; the orphan goes.
	os.WriteFile(filepath.Join(dir, "1.req"), []byte("{}\n"), 0o640)
	os.WriteFile(filepath.Join(dir, "2.state.tmp"), []byte("{"), 0o640)
	ib, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ib.Close()
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("left %v", ents)
	}
}

func TestBrokenStateFileRefusesToOpen(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "1.state"), []byte("{"), 0o640)
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("err %v", err)
	}
}

func TestSweepDeletesOldEvents(t *testing.T) {
	dir := t.TempDir()
	a := newApp(nil)
	o := fast
	ib := open(t, dir, o, a)
	id := add(t, ib, "x", "k1")
	waitFor(t, "delivery", func() bool { return status(ib, id) == "delivered" })
	ib.mu.Lock()
	ib.opts.Keep = time.Millisecond
	ib.mu.Unlock()
	ib.sweep(time.Now().Add(time.Second))
	if _, _, err := ib.Get(id); !IsNotFound(err) {
		t.Fatalf("still there: %v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("files left %v", ents)
	}
	if _, dup, _ := ib.Add(&Request{Method: "POST", Path: "/", Header: http.Header{}}, "k1"); dup {
		t.Fatal("the key of a swept event still dedupes")
	}
}

func TestWorkersLimitInFlight(t *testing.T) {
	var mu sync.Mutex
	in, peak := 0, 0
	a := newApp(func(n int, c call) Result {
		mu.Lock()
		in++
		peak = max(peak, in)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		in--
		mu.Unlock()
		return Result{Status: 200}
	})
	o := fast
	o.Workers = 3
	ib := open(t, t.TempDir(), o, a)
	for i := 0; i < 30; i++ {
		add(t, ib, "x", "")
	}
	waitFor(t, "deliveries", func() bool { return ib.Counts().Delivered == 30 })
	if peak > 3 || peak < 2 {
		t.Fatalf("peak in flight %d, want 3", peak)
	}
}

func TestDelay(t *testing.T) {
	o := Options{Backoff: 10 * time.Second, MaxBackoff: time.Hour}
	for n, want := range map[int]time.Duration{1: 10 * time.Second, 2: 20 * time.Second, 3: 40 * time.Second, 9: 2560 * time.Second, 10: time.Hour, 50: time.Hour} {
		if got := o.Delay(n); got != want {
			t.Errorf("Delay(%d) = %s, want %s", n, got, want)
		}
	}
}
