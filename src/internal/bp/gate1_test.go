// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Gate 1 review, M2 and M3: an apply says when it rewrote the config file,
// and a symlinked config stays a symlink with the target rewritten.
func TestApplyRewritesSymlinkTargetAndSaysSo(t *testing.T) {
	l := &liveServer{dir: t.TempDir()}
	l.port = freePort(t)
	real := filepath.Join(l.dir, "real.conf")
	writeFile(t, real, l.text("v1"))
	l.conf = filepath.Join(l.dir, "bareproxy.conf")
	if err := os.Symlink(real, l.conf); err != nil {
		t.Fatal(err)
	}
	s, err := Start(l.conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	l.s = s
	v2 := l.text("v2")
	a := mustApply(t, s, v2)
	l.expect(t, 2, "v2")
	if fi, err := os.Lstat(l.conf); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the config is no longer a symlink: %v %v", fi.Mode(), err)
	}
	if b, _ := os.ReadFile(real); string(b) != v2 {
		t.Errorf("the symlink's target holds %q, want the applied text", b)
	}
	if want := l.conf + " now holds version 2."; a.Wrote != l.conf || !strings.Contains(appliedText(a), want) {
		t.Errorf("apply reply: wrote %q, text %q; want %q", a.Wrote, appliedText(a), want)
	}
	if a := mustApply(t, s, v2); a.Wrote != "" || strings.Contains(appliedText(a), "now holds") {
		t.Errorf("an unchanged apply says it wrote the file: %+v", a)
	}
}

// Gate 1 review, M1: status shows that the config file doesn't hold the
// running config.
func TestStatusShowsConfigMismatch(t *testing.T) {
	l := startLive(t, "v1")
	if m := l.s.Status().Mismatch; m != "" {
		t.Errorf("fresh server: mismatch %q", m)
	}
	writeFile(t, l.conf, strings.Replace(l.text("v1"), `respond 200 "v1"`, "nopool", 1))
	l.s.Reload()
	if m := l.s.Status().Mismatch; !strings.Contains(m, "version 1 keeps running") {
		t.Errorf("status after a broken reload: mismatch %q", m)
	}
}

// Gate 1 review, m3: plan shows each of a config's warnings once, Parse's and
// plan's own together in plan's wording, and the apply and rollback replies
// don't repeat them.
func TestEachWarningShowsOnce(t *testing.T) {
	l := startLive(t, "v1")
	sock := filepath.Join(l.dir, "admin.sock")
	if err := l.s.startAdmin(sock); err != nil {
		t.Fatal(err)
	}
	warned := l.textWith("  set-response-header X-A 1\n  route /api/* -> api\n  route /api/x -> api\n", "v2",
		"\npool api\n  backend 127.0.0.1:1\npool spare\n  backend 127.0.0.1:2\n")
	warnings := []string{
		"line 7: set-response-header isn't built yet in this version, so it's ignored",
		"line 9: route /api/x -> api never matches: line 8 takes every request it would get",
		"line 14: pool spare isn't used by any rule that can match",
	}
	count := func(what string, code int, out string, n int) {
		t.Helper()
		if code != 200 {
			t.Fatalf("%s: %d %s", what, code, out)
		}
		for _, w := range warnings {
			if got := strings.Count(out, w); got != n {
				t.Errorf("%s: %q shows %d times, want %d:\n%s", what, w, got, n, out)
			}
		}
		if strings.Contains(out, "warning: ") || strings.Contains(out, "isn't used by any route") {
			t.Errorf("%s: a warning in another wording:\n%s", what, out)
		}
	}
	code, out := adminCall(t, sock, "POST", "/plan", warned)
	count("plan", code, out, 1)
	code, out = adminCall(t, sock, "POST", "/apply", warned)
	count("apply", code, out, 0)
	mustApply(t, l.s, l.text("v3"))
	code, out = adminCall(t, sock, "POST", "/rollback?version=2", "")
	count("rollback", code, out, 1)
}

// Gate 1 review, m2: Warnings, which check prints, holds Parse's warnings and
// plan's in line order. Parse leaves the unused pool to plan's wording.
func TestWarningsHoldParsesAndPlans(t *testing.T) {
	src := "site http://example.com:8080\n  set-response-header X-A 1\n  route /api/* -> api\n  route /api/x -> api\n" +
		"  route /* -> respond 200 \"x\"\npool api\n  backend 127.0.0.1:1\npool spare\n  backend 127.0.0.1:2\n"
	c, probs := Parse("x.conf", src)
	defer c.Close()
	if len(probs) != 1 || probs[0].Line != 2 {
		t.Errorf("Parse's problems: %v, want only the line 2 warning", probs)
	}
	var got []string
	for _, w := range Warnings(c) {
		got = append(got, w.String())
	}
	want := []string{
		"line 2: warning: set-response-header isn't built yet in this version, so it's ignored",
		"line 4: warning: route /api/x -> api never matches: line 3 takes every request it would get",
		"line 8: warning: pool spare isn't used by any rule that can match",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Warnings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Gate 1 review, m5: apply, rollback and reload events name the plan and
// count what it changes, on one line. A startup says what runs.
func TestEventsNameThePlan(t *testing.T) {
	l := startLive(t, "v1")
	mustApply(t, l.s, l.text("v2"))
	if _, err := l.s.Rollback(1, "tester"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, l.conf, l.textWith("", "v4", "\npool spare\n  backend 127.0.0.1:1\n"))
	l.s.Reload()
	h, me := l.s.History(), userName(os.Getuid())
	if len(h) != 4 {
		t.Fatalf("history: %+v", h)
	}
	want := []string{
		fmt.Sprintf("start: version 1 running (startup by %s): 1 site, 0 pools, 1 rule; listening on :%d (http)", me, l.port),
		"apply: version 2 running (apply by tester): plan " + h[1].Plan + ", 1 routing change",
		"rollback: version 3 running (rollback to version 1 by tester): plan " + h[2].Plan + ", 1 routing change",
		fmt.Sprintf("reload: version 4 running (reload by %s): plan %s, 1 routing change, 1 other change, 1 warning", me, h[3].Plan),
	}
	var got []string
	for _, e := range l.s.Current().Mem.Events() {
		got = append(got, e.Kind+": "+e.Text)
	}
	if !slices.Equal(got, want) {
		t.Errorf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Gate 1 review, m5: status lists a removed backend under its pool while it
// drains, with its requests in flight and when the drain ends. A removed
// pool stays listed while its backends drain. Both go once the drain ends.
func TestStatusListsDrainingBackends(t *testing.T) {
	arrived, release := make(chan struct{}, 4), make(chan struct{})
	var once sync.Once
	slowHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
		io.WriteString(w, "slow")
	})
	a, b := httptest.NewServer(slowHandler), httptest.NewServer(slowHandler)
	defer a.Close()
	defer b.Close()
	defer once.Do(func() { close(release) })
	l := startLive(t, "v1")
	withPool := func(backends ...string) string {
		return l.textWith("  route /api/* -> api\n", "v1", "\npool api\n  drain 2s\n  backend "+strings.Join(backends, "\n  backend ")+"\n")
	}
	addrA, addrB := a.Listener.Addr().String(), b.Listener.Addr().String()
	mustApply(t, l.s, withPool(addrA, addrB))
	for range 2 { // one request in flight on each backend
		go get(l.port, "/api/x")
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("a request never reached its backend")
		}
	}
	pool := func() *PoolStatus {
		for _, p := range l.s.Status().Pools {
			if p.Name == "api" {
				return &p
			}
		}
		return nil
	}
	draining := func(p *PoolStatus, addr string) bool {
		for _, bs := range p.Backends {
			if end, err := time.Parse(time.RFC3339, bs.Until); bs.Addr == addr && bs.State == "draining" && bs.InFlight == 1 &&
				err == nil && time.Until(end) > 0 && time.Until(end) <= 2*time.Second {
				return true
			}
		}
		return false
	}
	mustApply(t, l.s, withPool(addrB)) // A is removed
	if p := pool(); p == nil || p.Size != 1 || len(p.Backends) != 2 || p.Backends[0].Addr != addrB || !draining(p, addrA) {
		t.Errorf("pool api after A was removed: %+v", p)
	}
	mustApply(t, l.s, l.text("v1")) // the pool is removed, and B with it
	if p := pool(); p == nil || p.Checks != "none, the pool was removed" || !draining(p, addrB) {
		t.Errorf("pool api after it was removed: %+v", p)
	}
	once.Do(func() { close(release) })
	for deadline := time.Now().Add(5 * time.Second); pool() != nil; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("pool api is still listed after its drain time: %+v", pool())
		}
	}
}
