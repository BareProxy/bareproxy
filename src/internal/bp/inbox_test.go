// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain lets the inbox's crash test run this test binary as a real
// BareProxy, which it then kills with SIGKILL.
func TestMain(m *testing.M) {
	if conf := os.Getenv("BP_TEST_RUN_CONFIG"); conf != "" {
		if err := Run(conf); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// hookApp is the app behind an inbox: it records every delivery.
type hookApp struct {
	srv      *httptest.Server
	port     int
	mu       sync.Mutex
	got      []hookCall
	inFlight atomic.Int32
	delay    time.Duration
	status   int
}

type hookCall struct {
	ID, Replay, Attempt, Path, Body string
	Header                          http.Header
}

func newHookApp(t *testing.T, port int) *hookApp {
	a := &hookApp{status: 200}
	a.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.inFlight.Add(1)
		defer a.inFlight.Add(-1)
		b, _ := io.ReadAll(r.Body)
		a.mu.Lock()
		a.got = append(a.got, hookCall{ID: r.Header.Get("BareProxy-Inbox-Id"), Replay: r.Header.Get("BareProxy-Inbox-Replay"),
			Attempt: r.Header.Get("BareProxy-Inbox-Attempt"), Path: r.URL.RequestURI(), Body: string(b), Header: r.Header.Clone()})
		d, st := a.delay, a.status
		a.mu.Unlock()
		time.Sleep(d)
		w.WriteHeader(st)
	}))
	if port != 0 {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Fatal(err)
		}
		a.srv.Listener = ln
	}
	a.srv.Start()
	a.port = a.srv.Listener.Addr().(*net.TCPAddr).Port
	t.Cleanup(a.srv.Close)
	return a
}

func (a *hookApp) calls() []hookCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]hookCall(nil), a.got...)
}

func inboxConfig(dir string, port, appPort int, inboxLines string) string {
	return fmt.Sprintf(`global
  admin off
  trace-log off
  state %s

site http://hooks.test:%d
  route /hooks/* -> inbox app
  route /* -> respond 200 "site"

inbox app
  deliver app
  retry 50ms 200ms
%s
pool app
  backend 127.0.0.1:%d
`, filepath.Join(dir, "state"), port, inboxLines, appPort)
}

func startInbox(t *testing.T, appPort int, inboxLines string, files map[string]string) (*Server, int, string) {
	t.Helper()
	dir := t.TempDir()
	for name, text := range files {
		writeFile(t, filepath.Join(dir, name), text)
	}
	port := freePort(t)
	conf := filepath.Join(dir, "bareproxy.conf")
	writeFile(t, conf, inboxConfig(dir, port, appPort, inboxLines))
	s, err := Start(conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	return s, port, dir
}

func postHook(port int, path, body string, h map[string]string) (int, string, http.Header, error) {
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d%s", port, path), strings.NewReader(body))
	req.Host = fmt.Sprintf("hooks.test:%d", port)
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := directClient.Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header, nil
}

func waitUntil(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestInboxStoresAnswersAndDelivers(t *testing.T) {
	app := newHookApp(t, 0)
	s, port, dir := startInbox(t, app.port, "", nil)
	code, body, h, err := postHook(port, "/hooks/stripe?x=1", `{"id":"evt_1"}`, map[string]string{"Content-Type": "application/json", "Stripe-Signature": "t=1"})
	if err != nil || code != 202 || !strings.HasPrefix(body, "Stored as event ") || h.Get("BareProxy-Inbox-Id") == "" {
		t.Fatalf("POST: %d %q %v", code, body, err)
	}
	id := h.Get("BareProxy-Inbox-Id")
	waitUntil(t, "delivery", 5*time.Second, func() bool { return len(app.calls()) == 1 })
	c := app.calls()[0]
	if c.ID != id || c.Replay != "" || c.Attempt != "1" || c.Path != "/hooks/stripe?x=1" || c.Body != `{"id":"evt_1"}` ||
		c.Header.Get("Stripe-Signature") != "t=1" || c.Header.Get("Content-Type") != "application/json" || c.Header.Get("X-Forwarded-For") != "127.0.0.1" {
		t.Fatalf("the app got %+v", c)
	}
	if _, err := os.Stat(filepath.Join(dir, "state", "inbox", "app", id+".req")); err != nil {
		t.Fatalf("the event isn't in the default folder: %v", err)
	}
	// Only POST, PUT and PATCH; other routes work as before.
	if code, _, _, _ := postHookMethod(port, "GET", "/hooks/stripe"); code != 405 {
		t.Errorf("GET on an inbox: %d", code)
	}
	if code, _, _, _ := postHookMethod(port, "GET", "/other"); code != 200 {
		t.Errorf("other route: %d", code)
	}
	// The record and why.
	rec := recordOf(t, s, h.Get("BareProxy-Id"))
	if rec.Outcome != "inbox" || rec.Inbox != "app" || rec.Event != id || rec.Status != 202 {
		t.Fatalf("record %+v", rec)
	}
	if w := RenderWhy(rec); !strings.Contains(w, "Inbox app") || !strings.Contains(w, "stored as event "+id) {
		t.Errorf("why:\n%s", w)
	}
	// explain.
	out, err := Explain(s.Current(), "POST", fmt.Sprintf("http://hooks.test:%d/hooks/x", port), nil, true)
	if err != nil || !strings.Contains(out, "Inbox app (line 10): stored in") || !strings.Contains(out, "Action: 202 (inbox)") {
		t.Fatalf("explain: %v\n%s", err, out)
	}
	// The admin endpoints: list, show, replay.
	l := inboxAdmin(t, s.adminInboxList, "GET", "/inbox?json=1")
	var lists []InboxList
	json.Unmarshal([]byte(l), &lists)
	if len(lists) != 1 || lists[0].Counts.Delivered != 1 || lists[0].Events[0].ID != id {
		t.Fatalf("list: %s", l)
	}
	if txt := inboxAdmin(t, s.adminInboxList, "GET", "/inbox"); !strings.Contains(txt, "0 pending, 0 delivering, 1 delivered, 0 failed") {
		t.Errorf("list text:\n%s", txt)
	}
	if txt := inboxAdmin(t, s.adminInboxEvent, "GET", "/inbox/event?name=app&id="+id+"&body=1"); !strings.Contains(txt, "Attempt 1 at") || !strings.Contains(txt, `{"id":"evt_1"}`) {
		t.Errorf("show:\n%s", txt)
	}
	if txt := inboxAdmin(t, s.adminInboxReplay, "POST", "/inbox/replay?name=app&id="+id); !strings.Contains(txt, "Replayed 1 event") {
		t.Fatalf("replay: %s", txt)
	}
	waitUntil(t, "the replay", 5*time.Second, func() bool { return len(app.calls()) == 2 })
	if c := app.calls()[1]; c.ID != id || c.Replay != "operator" {
		t.Fatalf("replayed delivery %+v", c)
	}
}

func postHookMethod(port int, method, path string) (int, string, http.Header, error) {
	req, _ := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	req.Host = fmt.Sprintf("hooks.test:%d", port)
	resp, err := directClient.Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	resp.Body.Close()
	return resp.StatusCode, "", resp.Header, nil
}

// recordOf finds a request's record by the BareProxy-Id it was answered with.
func recordOf(t *testing.T, s *Server, id string) *Record {
	t.Helper()
	var js []byte
	waitUntil(t, "the record", 2*time.Second, func() bool { js, _ = s.Current().Mem.Find(id); return js != nil })
	rec := &Record{}
	if err := json.Unmarshal(js, rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func inboxAdmin(t *testing.T, h http.HandlerFunc, method, target string) string {
	t.Helper()
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(method, target, nil))
	if w.Code != 200 {
		t.Fatalf("%s %s: %d %s", method, target, w.Code, w.Body)
	}
	return w.Body.String()
}

func TestInboxUpstreamDownThenBack(t *testing.T) {
	appPort := freePort(t) // nothing listens there yet
	s, port, _ := startInbox(t, appPort, "", nil)
	var ids []string
	for i := 0; i < 20; i++ {
		code, _, h, err := postHook(port, "/hooks/gh", fmt.Sprintf("n=%d", i), nil)
		if err != nil || code != 202 {
			t.Fatalf("POST %d while the app is down: %d %v", i, code, err)
		}
		ids = append(ids, h.Get("BareProxy-Inbox-Id"))
	}
	ib := s.Current().Inboxes["app"]
	waitUntil(t, "failed attempts", 5*time.Second, func() bool {
		st, _, _ := ib.Get(ids[19])
		return len(st.Attempts) >= 2
	})
	st, _, _ := ib.Get(ids[0])
	if st.Status == "delivered" || st.Attempts[0].Error != "connect refused" || st.Attempts[0].InDoubt {
		t.Fatalf("while down: %+v", st)
	}
	app := newHookApp(t, appPort)
	// A pool without health checks gives a down backend one trial request
	// every 10 seconds, so the inbox gets through within about that.
	waitUntil(t, "every delivery", 30*time.Second, func() bool { return ib.Counts().Delivered == 20 })
	time.Sleep(300 * time.Millisecond)
	per := map[string]int{}
	for _, c := range app.calls() {
		per[c.ID]++
		if c.Replay != "" {
			t.Errorf("a delivery marked %q after only refused connections", c.Replay)
		}
	}
	for _, id := range ids {
		if per[id] != 1 {
			t.Errorf("event %s delivered %d times", id, per[id])
		}
	}
}

func TestInboxSignatures(t *testing.T) {
	app := newHookApp(t, 0)
	secret := "whsec_test"
	mac := func(msg string) []byte {
		m := hmac.New(sha256.New, []byte(secret))
		m.Write([]byte(msg))
		return m.Sum(nil)
	}
	body := `{"event":"push"}`
	now := strconv.FormatInt(time.Now().Unix(), 10)
	old := strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
	cases := []struct {
		line   string
		header map[string]string
		want   int
	}{
		{"signature github secret.txt", map[string]string{"X-Hub-Signature-256": "sha256=" + hex.EncodeToString(mac(body))}, 202},
		{"signature github secret.txt", map[string]string{"X-Hub-Signature-256": "sha256=" + hex.EncodeToString(mac(body+" "))}, 401},
		{"signature github secret.txt", nil, 401},
		{"signature shopify secret.txt", map[string]string{"X-Shopify-Hmac-Sha256": base64.StdEncoding.EncodeToString(mac(body))}, 202},
		{"signature shopify secret.txt", map[string]string{"X-Shopify-Hmac-Sha256": base64.StdEncoding.EncodeToString(mac("x"))}, 401},
		{"signature stripe secret.txt", map[string]string{"Stripe-Signature": "t=" + now + ",v1=" + hex.EncodeToString(mac(now+"."+body))}, 202},
		{"signature stripe secret.txt", map[string]string{"Stripe-Signature": "t=" + now + ",v1=00,v1=" + hex.EncodeToString(mac(now+"."+body))}, 202},
		{"signature stripe secret.txt", map[string]string{"Stripe-Signature": "t=" + old + ",v1=" + hex.EncodeToString(mac(old+"."+body))}, 401},
		{"signature stripe secret.txt", map[string]string{"Stripe-Signature": "t=" + now + ",v1=" + hex.EncodeToString(mac(body))}, 401},
		{"signature hmac-sha256 X-Signature secret.txt hex", map[string]string{"X-Signature": hex.EncodeToString(mac(body))}, 202},
		{"signature hmac-sha256 X-Signature secret.txt base64", map[string]string{"X-Signature": base64.StdEncoding.EncodeToString(mac(body))}, 202},
		{"signature hmac-sha256 X-Signature secret.txt base64", map[string]string{"X-Signature": hex.EncodeToString(mac(body))}, 401},
	}
	for i, c := range cases {
		s, port, _ := startInbox(t, app.port, "  "+c.line+"\n", map[string]string{"secret.txt": secret + "\n"})
		code, _, rh, err := postHook(port, "/hooks/x", body, c.header)
		if err != nil || code != c.want {
			t.Errorf("case %d (%s): %d %v, want %d", i, c.line, code, err, c.want)
		}
		if c.want == 401 {
			if rec := recordOf(t, s, rh.Get("BareProxy-Id")); rec.Outcome != "refused" || !strings.HasPrefix(rec.Reason, "signature check failed") {
				t.Errorf("case %d: record %s %q", i, rec.Outcome, rec.Reason)
			}
			if n := s.Current().Inboxes["app"].Counts(); n.Pending+n.Delivered+n.Delivering != 0 {
				t.Errorf("case %d: a refused webhook was stored", i)
			}
		}
		s.Stop()
	}
}

func TestInboxBodyLimitAndKeyHeader(t *testing.T) {
	app := newHookApp(t, 0)
	s, port, _ := startInbox(t, app.port, "  body-limit 1KB\n  key-header X-GitHub-Delivery\n", nil)
	if code, _, _, _ := postHook(port, "/hooks/x", strings.Repeat("a", 2000), nil); code != 413 {
		t.Errorf("over the limit: %d", code)
	}
	_, _, h1, _ := postHook(port, "/hooks/x", "one", map[string]string{"X-GitHub-Delivery": "d-1"})
	code, _, h2, _ := postHook(port, "/hooks/x", "one", map[string]string{"X-GitHub-Delivery": "d-1"})
	if code != 202 || h1.Get("BareProxy-Inbox-Id") != h2.Get("BareProxy-Inbox-Id") {
		t.Fatalf("a resent delivery got another event: %d %s %s", code, h1.Get("BareProxy-Inbox-Id"), h2.Get("BareProxy-Inbox-Id"))
	}
	waitUntil(t, "delivery", 5*time.Second, func() bool { return s.Current().Inboxes["app"].Counts().Delivered == 1 })
	time.Sleep(200 * time.Millisecond)
	if n := len(app.calls()); n != 1 {
		t.Fatalf("%d deliveries", n)
	}
}

func TestInboxKeepsDeliveringAcrossAnApply(t *testing.T) {
	app := newHookApp(t, 0)
	app.status = 500
	s, port, dir := startInbox(t, app.port, "", nil)
	_, _, h, _ := postHook(port, "/hooks/x", "a", nil)
	id := h.Get("BareProxy-Inbox-Id")
	ib := s.Current().Inboxes["app"]
	waitUntil(t, "an attempt", 5*time.Second, func() bool { st, _, _ := ib.Get(id); return len(st.Attempts) >= 1 })
	text := inboxConfig(dir, port, app.port, "") + "\n# changed\n"
	if _, err := s.Apply(Change{Text: text, How: "apply"}); err != nil {
		t.Fatal(err)
	}
	if s.Current().Inboxes["app"] != ib {
		t.Fatal("the apply opened the inbox again instead of keeping it")
	}
	app.mu.Lock()
	app.status = 200
	app.mu.Unlock()
	waitUntil(t, "delivery after the apply", 5*time.Second, func() bool { st, _, _ := ib.Get(id); return st.Status == "delivered" })
}

// TestInboxSurvivesSIGKILL runs BareProxy as a child process, sends it
// webhooks, and kills it with SIGKILL while deliveries are in flight, three
// times, then starts it once more. No event the sender got a 202 for is
// lost, and no event reaches the app twice unless the later delivery is
// marked in-doubt.
func TestInboxSurvivesSIGKILL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGKILL is a Unix signal")
	}
	app := newHookApp(t, 0)
	app.delay = 30 * time.Millisecond
	dir := t.TempDir()
	port := freePort(t)
	conf := filepath.Join(dir, "bareproxy.conf")
	writeFile(t, conf, inboxConfig(dir, port, app.port, "  workers 4\n  key-header X-Event\n"))
	var child *exec.Cmd
	logf, _ := os.Create(filepath.Join(dir, "child.log"))
	defer logf.Close()
	start := func() {
		child = exec.Command(os.Args[0], "-test.run=^$")
		child.Env = append(os.Environ(), "BP_TEST_RUN_CONFIG="+conf)
		child.Stdout, child.Stderr = logf, logf
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		waitUntil(t, "the child to listen", 20*time.Second, func() bool {
			c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if c != nil {
				c.Close()
			}
			return err == nil
		})
	}
	defer func() {
		if child != nil && child.Process != nil {
			child.Process.Kill()
			child.Wait()
		}
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(dir, "child.log"))
			t.Logf("child log:\n%s", b)
		}
	}()
	const total = 300
	accepted := map[int]string{} // event number -> inbox ID
	var mu sync.Mutex
	nextToSend := 0
	send := func(n int) bool {
		code, _, h, err := postHook(port, "/hooks/x", fmt.Sprintf("n=%d", n), map[string]string{"X-Event": strconv.Itoa(n)})
		if err != nil || code != 202 {
			return false
		}
		mu.Lock()
		if old, ok := accepted[n]; ok && old != h.Get("BareProxy-Inbox-Id") {
			t.Errorf("event %d stored twice: %s and %s", n, old, h.Get("BareProxy-Inbox-Id"))
		}
		accepted[n] = h.Get("BareProxy-Inbox-Id")
		mu.Unlock()
		return true
	}
	kills := 0
	for round := 0; round < 3; round++ {
		start()
		// Send until deliveries are in flight and some events are stored,
		// then kill mid-stream. Events the sender got no 202 for are sent
		// again later, as a real sender does.
		for i := 0; i < 60 && nextToSend < total; i++ {
			if send(nextToSend) {
				nextToSend++
			}
		}
		waitUntil(t, "deliveries in flight", 10*time.Second, func() bool { return app.inFlight.Load() > 0 })
		child.Process.Kill() // SIGKILL
		child.Wait()
		kills++
	}
	start()
	for nextToSend < total {
		if send(nextToSend) {
			nextToSend++
		}
	}
	// Every event the sender got a 202 for (and every one resent after a
	// failure) reaches the app with a 2xx.
	waitUntil(t, "every event delivered", 60*time.Second, func() bool {
		seen := map[string]bool{}
		for _, c := range app.calls() {
			seen[c.ID] = true
		}
		mu.Lock()
		defer mu.Unlock()
		for _, id := range accepted {
			if !seen[id] {
				return false
			}
		}
		return true
	})
	time.Sleep(500 * time.Millisecond)
	per := map[string][]hookCall{}
	for _, c := range app.calls() {
		per[c.ID] = append(per[c.ID], c)
	}
	inDoubt, unmarkedDup := 0, 0
	for _, cs := range per {
		plain := 0
		for _, c := range cs {
			switch c.Replay {
			case "":
				plain++
			case "in-doubt":
				inDoubt++
			default:
				t.Errorf("event %s marked %q", c.ID, c.Replay)
			}
		}
		if plain > 1 {
			unmarkedDup++
		}
	}
	lost := 0
	for n, id := range accepted {
		if len(per[id]) == 0 {
			lost++
			t.Errorf("event %d (%s) lost", n, id)
		}
	}
	if unmarkedDup > 0 {
		t.Errorf("%d events delivered twice without a mark", unmarkedDup)
	}
	if len(accepted) != total {
		t.Errorf("%d events accepted, want %d", len(accepted), total)
	}
	if inDoubt == 0 {
		t.Errorf("no delivery was marked in-doubt, so no kill landed mid-delivery")
	}
	t.Logf("%d events, %d SIGKILLs during delivery, %d deliveries: %d lost, %d delivered twice unmarked, %d sent again marked in-doubt",
		len(accepted), kills, len(app.calls()), lost, unmarkedDup, inDoubt)
}
