// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func init() { logOutput = io.Discard }

// liveServer is a BareProxy started from a file in a temporary folder,
// serving on a free port, with its history in that folder.
type liveServer struct {
	dir, conf string
	port      int
	s         *Server
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// text is a config with one site that answers body on every path.
func (l *liveServer) text(body string) string { return l.textWith("", body, "") }

// textWith adds routes before the one that answers body, and blocks after
// the site.
func (l *liveServer) textWith(routes, body, blocks string) string {
	return fmt.Sprintf(`global
  admin %s
  trace-log off
  state state

site http://example.com:%d
%s  route /* -> respond 200 %q
%s`, filepath.Join(l.dir, "admin.sock"), l.port, routes, body, blocks)
}

func startLive(t *testing.T, body string) *liveServer {
	t.Helper()
	l := &liveServer{dir: t.TempDir()}
	l.port = freePort(t)
	l.conf = filepath.Join(l.dir, "bareproxy.conf")
	writeFile(t, l.conf, l.text(body))
	s, err := Start(l.conf)
	if err != nil {
		t.Fatal(err)
	}
	l.s = s
	t.Cleanup(s.Stop)
	return l
}

var directClient = &http.Client{Transport: &http.Transport{}}

// get fetches a path from example.com on a port of this machine.
func get(port int, path string) (int, string, error) {
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	req.Host = fmt.Sprintf("example.com:%d", port)
	resp, err := directClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

// expect checks what the running config answers and what the file holds.
func (l *liveServer) expect(t *testing.T, version int, body string) {
	t.Helper()
	if v := l.s.Current().Version; v != version {
		t.Errorf("running version %d, want %d", v, version)
	}
	if code, got, err := get(l.port, "/"); err != nil || code != 200 || got != body+"\n" {
		t.Errorf("GET / = %d %q, %v; want 200 %q", code, got, err, body)
	}
}

func (l *liveServer) expectFile(t *testing.T, want string) {
	t.Helper()
	if b, err := os.ReadFile(l.conf); err != nil || string(b) != want {
		t.Errorf("the config file holds %q (%v), want %q", b, err, want)
	}
}

func mustApply(t *testing.T, s *Server, text string) *Applied {
	t.Helper()
	a, err := s.Apply(Change{Text: text, How: "apply", User: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestApplySwapsAndRecordsHistory(t *testing.T) {
	l := startLive(t, "v1")
	l.expect(t, 1, "v1")
	v1, v2 := l.text("v1"), l.text("v2")
	a := mustApply(t, l.s, v2)
	if a.Version != 2 || a.Previous != 1 || a.Unchanged || a.PlanID == "" {
		t.Fatalf("applied %+v, want version 2 after 1 with a plan ID", a)
	}
	l.expect(t, 2, "v2")
	l.expectFile(t, v2)
	h := l.s.History()
	if len(h) != 2 || h[0].How != "startup" || h[0].User != userName(os.Getuid()) ||
		h[1].How != "apply" || h[1].User != "tester" || h[1].Plan != a.PlanID || h[1].Time == "" {
		t.Fatalf("history %+v", h)
	}
	for n, want := range map[int]string{1: v1, 2: v2} {
		b, err := os.ReadFile(filepath.Join(l.dir, "state", "versions", fmt.Sprintf("%d.conf", n)))
		if err != nil || string(b) != want {
			t.Errorf("versions/%d.conf = %q, %v", n, b, err)
		}
	}
	// The same text again keeps the version and adds nothing to the history.
	if a := mustApply(t, l.s, v2); !a.Unchanged || a.Version != 2 || len(l.s.History()) != 2 {
		t.Fatalf("applying the running text again: %+v, %d versions", a, len(l.s.History()))
	}
	// A restart reads the history back and runs the same version.
	l.s.Stop()
	s, err := Start(l.conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	l.s = s
	l.expect(t, 2, "v2")
	if h := s.History(); len(h) != 2 || h[1].User != "tester" {
		t.Fatalf("history after a restart: %+v", h)
	}
}

func TestRollbackRestoresAndRewritesFile(t *testing.T) {
	l := startLive(t, "v1")
	v1, v2 := l.text("v1"), l.text("v2")
	mustApply(t, l.s, v2)
	a, err := l.s.Rollback(0, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if a.Version != 3 || a.Previous != 2 {
		t.Fatalf("rollback: %+v, want version 3 after 2", a)
	}
	l.expect(t, 3, "v1")
	l.expectFile(t, v1)
	h := l.s.History()
	if e := h[len(h)-1]; e.How != "rollback" || e.From != 1 || e.User != "tester" {
		t.Fatalf("history entry for the rollback: %+v", e)
	}
	// The default is the version before the running one, so a second
	// rollback undoes the first.
	if _, err := l.s.Rollback(0, "tester"); err != nil {
		t.Fatal(err)
	}
	l.expect(t, 4, "v2")
	l.expectFile(t, v2)
	if _, err := l.s.Rollback(1, "tester"); err != nil {
		t.Fatal(err)
	}
	l.expect(t, 5, "v1")
	// A version the history doesn't have changes nothing.
	if _, err := l.s.Rollback(99, "tester"); err == nil {
		t.Fatal("rollback to version 99 worked")
	}
	l.expect(t, 5, "v1")
	l.expectFile(t, v1)
}

func TestPlanIDMismatchRefused(t *testing.T) {
	l := startLive(t, "v1")
	v1, v2, v3 := l.text("v1"), l.text("v2"), l.text("v3")
	planFor := func(text string) string {
		c, probs := Parse(l.conf, text)
		if HasErrors(probs) {
			t.Fatal(probs)
		}
		defer c.Close()
		return MakePlan(l.s.Current().Cfg, c).ID
	}
	id := planFor(v2)
	var pe *PlanChangedError
	if _, err := l.s.Apply(Change{Text: v2, How: "apply", PlanID: "x" + id}); !errors.As(err, &pe) {
		t.Fatalf("apply with another plan's ID: %v, want a refusal", err)
	}
	l.expect(t, 1, "v1")
	l.expectFile(t, v1)
	if n := len(l.s.History()); n != 1 {
		t.Fatalf("%d versions after a refused apply, want 1", n)
	}
	// Once another config goes live, a plan made before it is out of date.
	mustApply(t, l.s, v3)
	if _, err := l.s.Apply(Change{Text: v2, How: "apply", PlanID: id}); !errors.As(err, &pe) {
		t.Fatalf("apply with a stale plan: %v, want a refusal", err)
	}
	l.expect(t, 2, "v3")
	if _, err := l.s.Apply(Change{Text: v2, How: "apply", PlanID: planFor(v2)}); err != nil {
		t.Fatalf("apply with a fresh plan: %v", err)
	}
	l.expect(t, 3, "v2")
}

func TestBrokenConfigNeverGoesLive(t *testing.T) {
	l := startLive(t, "v1")
	v1 := l.text("v1")
	broken := strings.Replace(v1, `respond 200 "v1"`, "nopool", 1)
	var ce *ConfigError
	if _, err := l.s.Apply(Change{Text: broken, How: "apply"}); !errors.As(err, &ce) || len(ce.Problems) == 0 {
		t.Fatalf("apply of a broken config: %v, want a config error", err)
	}
	l.expect(t, 1, "v1")
	l.expectFile(t, v1)
	// SIGHUP with a broken file keeps the running version and flags that
	// the file doesn't hold it.
	writeFile(t, l.conf, broken)
	l.s.Reload()
	l.expect(t, 1, "v1")
	if m := l.s.ConfigMismatch(); !strings.Contains(m, "version 1 keeps running") {
		t.Errorf("mismatch after a broken reload: %q", m)
	}
	// SIGHUP with a good file applies it and clears the flag.
	v2 := l.text("v2")
	writeFile(t, l.conf, v2)
	l.s.Reload()
	l.expect(t, 2, "v2")
	h := l.s.History()
	if m := l.s.ConfigMismatch(); m != "" || len(h) != 2 || h[1].How != "reload" {
		t.Errorf("after a good reload: mismatch %q, history %+v", m, h)
	}
}

func TestStartupFallsBackToLastGoodVersion(t *testing.T) {
	l := startLive(t, "v1")
	mustApply(t, l.s, l.text("v2"))
	l.s.Stop()
	broken := strings.Replace(l.text("v3"), `respond 200 "v3"`, "nopool", 1)
	writeFile(t, l.conf, broken)
	s, err := Start(l.conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	l.s = s
	l.expect(t, 2, "v2")
	if m := s.ConfigMismatch(); !strings.Contains(m, "version 2 from the history is running") {
		t.Errorf("mismatch: %q", m)
	}
	l.expectFile(t, broken)
	if n := len(s.History()); n != 2 {
		t.Errorf("%d versions after a fallback start, want 2", n)
	}
	// Without a history, a broken file still stops the start.
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "bareproxy.conf"), broken)
	if s, err := Start(filepath.Join(dir, "bareproxy.conf")); err == nil {
		s.Stop()
		t.Fatal("a broken file with no history started")
	}
}

// adminCall sends one request to an admin socket.
func adminCall(t *testing.T, sock, method, target, body string) (int, string) {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	req, _ := http.NewRequest(method, "http://bareproxy"+target, strings.NewReader(body))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestAdminEndpoints(t *testing.T) {
	l := startLive(t, "v1")
	sock := filepath.Join(l.dir, "admin.sock")
	if err := l.s.startAdmin(sock); err != nil {
		t.Fatal(err)
	}
	v2 := l.text("v2")
	code, out := adminCall(t, sock, "POST", "/plan?json=1", v2)
	var pr planReply
	if err := json.Unmarshal([]byte(out), &pr); err != nil || code != 200 || pr.Running != 1 || pr.Unchanged || pr.PlanID == "" {
		t.Fatalf("POST /plan: %d %s", code, out)
	}
	if code, out := adminCall(t, sock, "POST", "/plan", l.text("v1")); code != 200 || !strings.Contains(out, "No changes") {
		t.Errorf("plan of the running text: %d %q", code, out)
	}
	other := filepath.Join(l.dir, "other.conf")
	writeFile(t, other, v2)
	if code, out := adminCall(t, sock, "GET", "/plan?file="+url.QueryEscape(other), ""); code != 200 || !strings.Contains(out, "Compared with running version 1:") {
		t.Errorf("GET /plan?file=: %d %q", code, out)
	}
	if code, out := adminCall(t, sock, "POST", "/apply?plan=x"+pr.PlanID, v2); code != http.StatusConflict {
		t.Errorf("apply with another plan's ID: %d %q", code, out)
	}
	if code, out := adminCall(t, sock, "POST", "/apply", "site http://example.com:1\n  route /* -> nopool\n"); code != http.StatusUnprocessableEntity || !strings.Contains(out, "no pool named nopool") {
		t.Errorf("apply of a broken config: %d %q", code, out)
	}
	l.expect(t, 1, "v1")
	if code, out := adminCall(t, sock, "POST", "/apply?plan="+pr.PlanID, v2); code != 200 || !strings.Contains(out, "Version 2 is running (it was 1).") {
		t.Fatalf("apply: %d %q", code, out)
	}
	l.expect(t, 2, "v2")
	l.expectFile(t, v2)
	code, out = adminCall(t, sock, "GET", "/history?json=1", "")
	var hr struct {
		Running  int
		Versions []Entry
	}
	if err := json.Unmarshal([]byte(out), &hr); err != nil || code != 200 || hr.Running != 2 || len(hr.Versions) != 2 ||
		hr.Versions[1].User != userName(os.Getuid()) || hr.Versions[1].Plan != pr.PlanID {
		t.Fatalf("GET /history: %d %s", code, out)
	}
	if code, out := adminCall(t, sock, "GET", "/history", ""); code != 200 || !strings.Contains(out, "(running)") {
		t.Errorf("GET /history as text: %d %q", code, out)
	}
	if code, out := adminCall(t, sock, "POST", "/rollback", ""); code != 200 || !strings.Contains(out, "Version 3 is running (it was 2).") {
		t.Fatalf("rollback: %d %q", code, out)
	}
	l.expect(t, 3, "v1")
	if code, out := adminCall(t, sock, "POST", "/rollback?version=x", ""); code != http.StatusBadRequest {
		t.Errorf("rollback to version x: %d %q", code, out)
	}
}

func TestRemovedBackendDrains(t *testing.T) {
	arrived, release := make(chan struct{}, 4), make(chan struct{})
	var aHits atomic.Int32
	aClosed := make(chan struct{}, 8)
	a := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aHits.Add(1)
		arrived <- struct{}{}
		<-release
		io.WriteString(w, "A")
	}))
	a.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateClosed {
			aClosed <- struct{}{}
		}
	}
	a.Start()
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "B") }))
	defer b.Close()
	l := startLive(t, "v1")
	withPool := func(addr string) string {
		return l.textWith("  route /api/* -> api\n", "v1", "\npool api\n  drain 2s\n  backend "+addr+"\n")
	}
	mustApply(t, l.s, withPool(a.Listener.Addr().String()))
	got := make(chan string, 1)
	go func() {
		code, body, err := get(l.port, "/api/slow")
		got <- fmt.Sprintf("%d %s %v", code, body, err)
	}()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached backend A")
	}
	mustApply(t, l.s, withPool(b.Listener.Addr().String()))
	// New requests go to B at once, and A gets none.
	for i := 0; i < 5; i++ {
		if code, body, err := get(l.port, "/api/x"); err != nil || code != 200 || body != "B" {
			t.Fatalf("GET /api/x after the apply: %d %q, %v; want B", code, body, err)
		}
	}
	if n := aHits.Load(); n != 1 {
		t.Fatalf("backend A got %d requests, want only the one in flight", n)
	}
	// The request in flight on A finishes on the version it started with.
	close(release)
	if r := <-got; r != "200 A <nil>" {
		t.Fatalf("the request in flight got %s, want 200 A", r)
	}
	// After the pool's drain time, A's connections are closed.
	select {
	case <-aClosed:
	case <-time.After(6 * time.Second):
		t.Fatal("backend A's connection wasn't closed after the drain time")
	}
}

func TestApplyOpensAndClosesListeners(t *testing.T) {
	l := startLive(t, "v1")
	p1, p2 := l.port, freePort(t)
	movePort := func(text string, from, to int) string {
		return strings.Replace(text, fmt.Sprintf(":%d\n", from), fmt.Sprintf(":%d\n", to), 1)
	}
	v2 := movePort(l.text("v2"), p1, p2)
	mustApply(t, l.s, v2)
	if code, body, err := get(p2, "/"); err != nil || code != 200 || body != "v2\n" {
		t.Fatalf("new port %d: %d %q, %v", p2, code, body, err)
	}
	if _, _, err := get(p1, "/"); err == nil {
		t.Fatalf("removed port %d still answers", p1)
	}
	// A port that can't be opened stops the apply before anything changes.
	busy, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	p3 := busy.Addr().(*net.TCPAddr).Port
	if _, err := l.s.Apply(Change{Text: movePort(v2, p2, p3), How: "apply"}); err == nil || !strings.Contains(err.Error(), "can't open port") {
		t.Fatalf("apply onto a busy port: %v", err)
	}
	if code, body, err := get(p2, "/"); err != nil || code != 200 || body != "v2\n" || l.s.Current().Version != 2 {
		t.Fatalf("after the refused apply: %d %q, %v, version %d", code, body, err, l.s.Current().Version)
	}
	l.expectFile(t, v2)
}
