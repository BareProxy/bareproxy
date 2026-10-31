// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// rawApp is an app on a plain TCP listener, so a test can drop a
// connection after reading a request and before answering it.
type rawApp struct {
	ln   net.Listener
	port int
	mu   sync.Mutex
	got  []hookCall
	drop func(n int) bool // drop the nth request (from 1) after reading it
}

func newRawApp(t *testing.T, drop func(int) bool) *rawApp {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := &rawApp{ln: ln, port: ln.Addr().(*net.TCPAddr).Port, drop: drop}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go a.serve(c)
		}
	}()
	return a
}

func (a *rawApp) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	for {
		r, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		b, _ := io.ReadAll(r.Body)
		a.mu.Lock()
		a.got = append(a.got, hookCall{ID: r.Header.Get("BareProxy-Inbox-Id"), Replay: r.Header.Get("BareProxy-Inbox-Replay"),
			Attempt: r.Header.Get("BareProxy-Inbox-Attempt"), Body: string(b), Header: r.Header.Clone()})
		n := len(a.got)
		a.mu.Unlock()
		if a.drop(n) {
			return // read, maybe acted on, never answered
		}
		io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
	}
}

func (a *rawApp) calls() []hookCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]hookCall(nil), a.got...)
}

// An event the app may already have (the connection broke after the request
// went out) must not reach the app again unmarked. Go's transport resends a
// request on a reused connection by itself when it carries an
// Idempotency-Key header, which webhooks often do.
func TestInboxNeverResendsUnmarkedOnAReusedConnection(t *testing.T) {
	app := newRawApp(t, func(n int) bool { return n == 2 })
	s, port, _ := startInbox(t, app.port, "  workers 1\n", nil)
	_ = s
	for i, body := range []string{"one", "two"} {
		code, _, _, err := postHook(port, "/hooks/x", body, map[string]string{"Idempotency-Key": "k-" + body})
		if err != nil || code != 202 {
			t.Fatalf("post %d: %d %v", i, code, err)
		}
		want := i + 1
		waitUntil(t, "a delivery", 5*time.Second, func() bool { return len(app.calls()) >= want })
	}
	// Let any retry happen.
	time.Sleep(500 * time.Millisecond)
	seen := map[string]int{}
	for _, c := range app.calls() {
		seen[c.ID]++
		if seen[c.ID] > 1 && c.Replay == "" {
			t.Fatalf("event %s (%s) reached the app again with no BareProxy-Inbox-Replay mark (attempt %s); calls %+v",
				c.ID, c.Body, c.Attempt, app.calls())
		}
	}
}

// Headers the sender named in Connection are hop-by-hop (RFC 9110 7.6.1)
// and aren't passed on, as a proxied request wouldn't pass them on.
func TestInboxDropsHeadersNamedInConnection(t *testing.T) {
	app := newHookApp(t, 0)
	_, port, _ := startInbox(t, app.port, "", nil)
	code, _, _, err := postHook(port, "/hooks/x", "b", map[string]string{"Connection": "keep-alive, X-Hop", "X-Hop": "secret", "X-Keep": "yes"})
	if err != nil || code != 202 {
		t.Fatalf("post: %d %v", code, err)
	}
	waitUntil(t, "a delivery", 5*time.Second, func() bool { return len(app.calls()) == 1 })
	h := app.calls()[0].Header
	if h.Get("X-Hop") != "" || h.Get("X-Keep") != "yes" || strings.Contains(strings.ToLower(h.Get("Connection")), "x-hop") {
		t.Fatalf("headers passed on: %v", h)
	}
}
