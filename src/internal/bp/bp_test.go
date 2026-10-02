// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNormalizePath(t *testing.T) {
	cases := []struct {
		in, want string
		bad      bool
	}{
		{"/api/%6Frders", "/api/orders", false},
		{"/api/v1/../orders", "/api/orders", false},
		{"//api///orders", "/api/orders", false},
		{"/api/%2e%2e/admin", "/admin", false},
		{"/caf%c3%a9", "/caf%C3%A9", false},
		{"/a/b/", "/a/b/", false},
		{"/a/.", "/a/", false},
		{"/a/..", "/", false},
		{"/", "/", false},
		{"/a b", "/a%20b", false},
		{"/files/a%2Fb", "", true},
		{"/files/a%5Cb", "", true},
		{"/a\\b", "", true},
		{"/../etc/passwd", "", true},
		{"/%zz", "", true},
		{"/%2", "", true},
		{"/a%00b", "", true},
		{"relative", "", true},
	}
	for _, c := range cases {
		got, err := NormalizePath(c.in, false)
		if c.bad {
			if err == nil {
				t.Errorf("NormalizePath(%q) = %q, want an error", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("NormalizePath(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	if got, err := NormalizePath("/files/a%2fb", true); err != nil || got != "/files/a%2Fb" {
		t.Errorf("with encoded-slashes keep: got %q, %v", got, err)
	}
}

func TestConfigErrorsNameTheirLines(t *testing.T) {
	src := `site http://example.com:8080
  route /api/* -> nopool
  route /x -> files /does/not/exist
  route /bad//path -> respond 200
  route /y -> respond 99
  wibble
site example.org
  route /* -> respond 200
pool files
  backend 10.0.0.1:80
`
	_, probs := Parse("/tmp/test.conf", src)
	want := map[int]string{2: "no pool named nopool", 3: "can't open folder", 4: "normal form", 5: "respond needs a status", 6: "unknown site setting", 7: "automatic certificates", 9: "pool needs one name"}
	for line, frag := range want {
		found := false
		for _, p := range probs {
			if p.Line == line && !p.Warn && strings.Contains(p.Msg, frag) {
				found = true
			}
		}
		if !found {
			t.Errorf("no error on line %d mentioning %q; got %v", line, frag, probs)
		}
	}
}

type fixture struct {
	dir, public, conf, log string
	c                      *Config
	rt                     *Runtime
	h                      http.Handler
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The fixture config. Line numbers matter to the tests:
// 5 site, 6 error, 7 healthz, 8 old, 9 beta, 10 api, 11 files, 13 pool.
const fixtureConf = `global
  admin off
  trace-log requests.log

site http://example.com:8080
  error 404 /404.html
  route /healthz -> respond 200 "ok"
  route /old/* -> redirect 301 https://example.com
  route GET /beta/* header X-Beta=1 -> respond 200 "beta"
  route /api/* -> api strip
  route /* -> files public

pool api
%s`

func newFixture(t *testing.T, poolLines string, checks bool) *fixture {
	t.Helper()
	dir := t.TempDir()
	pub := filepath.Join(dir, "public")
	writeFile(t, pub+"/index.html", "<h1>home</h1>")
	writeFile(t, pub+"/about/index.html", "<h1>about</h1>")
	writeFile(t, pub+"/404.html", "<h1>not here</h1>")
	writeFile(t, pub+"/app.js", "console.log('plain')")
	writeFile(t, pub+"/app.js.gz", "GZ")
	writeFile(t, pub+"/app.js.br", "BR")
	writeFile(t, pub+"/.git/config", "secret")
	writeFile(t, pub+"/.well-known/security.txt", "Contact: x")
	if err := os.MkdirAll(pub+"/photos", 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir+"/outside/secret.txt", "outside secret")
	writeFile(t, dir+"/outside/index.html", "outside index")
	for _, l := range [][2]string{{"about", pub + "/inner"}, {pub + "/about", pub + "/inner-abs"}, {dir + "/outside", pub + "/escape"}, {"../outside/secret.txt", pub + "/escape-file"}} {
		if err := os.Symlink(l[0], l[1]); err != nil {
			t.Fatal(err)
		}
	}
	if poolLines == "" {
		poolLines = "  backend 127.0.0.1:1\n"
	}
	conf := filepath.Join(dir, "bareproxy.conf")
	writeFile(t, conf, fmt.Sprintf(fixtureConf, poolLines))
	c, probs := Load(conf)
	if HasErrors(probs) {
		t.Fatalf("fixture config: %v", probs)
	}
	rt, err := NewRuntime(c, nil, 1, func(string, ...any) {}, checks)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Trace == nil {
		rt.Trace, _ = OpenTraceLog(c.TraceLog)
	}
	t.Cleanup(func() { rt.Stop(); rt.Trace.Close(); c.Close() })
	s := NewServer(conf, rt)
	return &fixture{dir: dir, public: pub, conf: conf, log: c.TraceLog, c: c, rt: rt, h: s.Handler(8080, false)}
}

func (f *fixture) do(method, target string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.Host = "example.com:8080"
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rr := httptest.NewRecorder()
	f.h.ServeHTTP(rr, req)
	return rr
}

func (f *fixture) records(t *testing.T) []Record {
	t.Helper()
	fh, err := os.Open(f.log)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	var out []Record
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("bad record %q: %v", sc.Text(), err)
		}
		out = append(out, r)
	}
	return out
}

func TestFilesAndRoutes(t *testing.T) {
	f := newFixture(t, "", false)
	cases := []struct {
		method, target string
		status         int
		body, location string
	}{
		{"GET", "/", 200, "home", ""},
		{"GET", "/about/", 200, "about", ""},
		{"GET", "/about/index.html", 200, "about", ""},
		{"GET", "/about", 301, "", "/about/"},
		{"GET", "/about?x=1", 301, "", "/about/?x=1"},
		{"GET", "/nope/", 404, "not here", ""},
		{"GET", "/nope", 404, "not here", ""},
		{"GET", "/.git/config", 404, "not here", ""},
		{"GET", "/.well-known/security.txt", 200, "Contact", ""},
		{"GET", "/photos/", 404, "not here", ""},
		{"GET", "/photos", 301, "", "/photos/"},
		{"GET", "/inner/", 200, "about", ""},
		{"GET", "/inner-abs/", 404, "not here", ""},
		{"GET", "/escape/secret.txt", 404, "not here", ""},
		{"GET", "/escape/", 404, "not here", ""},
		{"GET", "/escape", 404, "not here", ""},
		{"GET", "/escape-file", 404, "not here", ""},
		{"GET", "/api/%2e%2e/about/", 200, "about", ""},
		{"GET", "/files/a%2Fb", 400, "encoded slash", ""},
		{"GET", "/../etc/passwd", 400, "climbs above", ""},
		{"GET", "/healthz", 200, "ok", ""},
		{"GET", "/old/x?q=1", 301, "", "https://example.com/old/x?q=1"},
		{"GET", "/beta/x", 404, "not here", ""},
		{"POST", "/about/", 405, "Method not allowed", ""},
	}
	for _, c := range cases {
		rr := f.do(c.method, c.target)
		if rr.Code != c.status {
			t.Errorf("%s %s: status %d, want %d (body %q)", c.method, c.target, rr.Code, c.status, rr.Body.String())
			continue
		}
		if c.body != "" && !strings.Contains(rr.Body.String(), c.body) {
			t.Errorf("%s %s: body %q, want it to contain %q", c.method, c.target, rr.Body.String(), c.body)
		}
		if c.location != "" && rr.Header().Get("Location") != c.location {
			t.Errorf("%s %s: Location %q, want %q", c.method, c.target, rr.Header().Get("Location"), c.location)
		}
		if len(rr.Header().Get("BareProxy-Id")) != 16 {
			t.Errorf("%s %s: no BareProxy-Id header", c.method, c.target)
		}
	}
	rr := f.do("GET", "/beta/x", "X-Beta", "1")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "beta") {
		t.Errorf("header rule: %d %q", rr.Code, rr.Body.String())
	}
	rr = f.do("GET", "/about/")
	if ct := rr.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type %q", ct)
	}
	if rr.Header().Get("ETag") == "" || rr.Header().Get("Last-Modified") == "" || rr.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("missing ETag, Last-Modified or Cache-Control: %v", rr.Header())
	}
	// Every request left exactly one record.
	recs := f.records(t)
	if want := len(cases) + 2; len(recs) != want {
		t.Errorf("%d records, want %d", len(recs), want)
	}
}

func TestConditionalRangeHead(t *testing.T) {
	f := newFixture(t, "", false)
	tag := f.do("GET", "/about/").Header().Get("ETag")
	if rr := f.do("GET", "/about/", "If-None-Match", tag); rr.Code != 304 {
		t.Errorf("If-None-Match: status %d, want 304", rr.Code)
	}
	if rr := f.do("GET", "/app.js", "Range", "bytes=0-6"); rr.Code != 206 || rr.Body.String() != "console" {
		t.Errorf("Range: %d %q", rr.Code, rr.Body.String())
	}
	rr := f.do("HEAD", "/about/")
	if rr.Code != 200 || rr.Body.Len() != 0 || rr.Header().Get("Content-Length") != "14" {
		t.Errorf("HEAD: %d, %d bytes, Content-Length %q", rr.Code, rr.Body.Len(), rr.Header().Get("Content-Length"))
	}
}

func TestPrecompressed(t *testing.T) {
	f := newFixture(t, "", false)
	cases := []struct{ accept, body, enc string }{
		{"gzip, br", "BR", "br"},
		{"gzip", "GZ", "gzip"},
		{"br;q=0, gzip", "GZ", "gzip"},
		{"", "console.log('plain')", ""},
	}
	for _, c := range cases {
		rr := f.do("GET", "/app.js", "Accept-Encoding", c.accept)
		if rr.Body.String() != c.body || rr.Header().Get("Content-Encoding") != c.enc {
			t.Errorf("Accept-Encoding %q: body %q encoding %q", c.accept, rr.Body.String(), rr.Header().Get("Content-Encoding"))
		}
		if rr.Header().Get("Vary") != "Accept-Encoding" || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/javascript") {
			t.Errorf("Accept-Encoding %q: headers %v", c.accept, rr.Header())
		}
	}
}

func TestOSRootTrailingSlash(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"root", "outside"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, dir+"/outside/index.html", "outside")
	if err := os.Symlink(dir+"/outside", dir+"/root/link"); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir + "/root")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if fh, err := root.Open("link/"); err == nil {
		fh.Close()
		t.Logf("%s HAS the os.Root trailing-slash escape (CVE-2026-39822): Open(\"link/\") reached outside the root", runtime.Version())
	} else {
		t.Logf("%s refuses Open(\"link/\"): %v", runtime.Version(), err)
	}
	// BareProxy never hands os.Root a path ending in a slash, so its lookups
	// stay inside on any Go version.
	for _, p := range []string{"/link/", "/link", "/link/index.html"} {
		if fr := LookupFile(root, p); fr.Status != 404 {
			t.Errorf("LookupFile(%q) = %d, want 404", p, fr.Status)
		}
	}
}

func echoBackend(t *testing.T, name string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			io.WriteString(w, "ok")
			return
		}
		fmt.Fprintf(w, "%s path=%s xff=%s xfh=%s xfp=%s id=%s host=%s",
			name, r.URL.RequestURI(), r.Header.Get("X-Forwarded-For"), r.Header.Get("X-Forwarded-Host"),
			r.Header.Get("X-Forwarded-Proto"), r.Header.Get("BareProxy-Id"), r.Host)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestProxyStripRetryAndHeaders(t *testing.T) {
	live := echoBackend(t, "live")
	dead := deadAddr(t)
	f := newFixture(t, fmt.Sprintf("  backend %s\n  backend %s\n", dead, strings.TrimPrefix(live.URL, "http://")), false)
	for i := 0; i < 4; i++ {
		rr := f.do("GET", "/api/orders?x=1")
		body := rr.Body.String()
		id := rr.Header().Get("BareProxy-Id")
		if rr.Code != 200 || !strings.Contains(body, "path=/orders?x=1") {
			t.Fatalf("request %d: %d %q", i, rr.Code, body)
		}
		if !strings.Contains(body, "xff=192.0.2.1") || !strings.Contains(body, "xfh=example.com:8080") ||
			!strings.Contains(body, "xfp=http") || !strings.Contains(body, "host=example.com:8080") || !strings.Contains(body, "id="+id) {
			t.Errorf("request %d: forwarded headers wrong: %q (id %s)", i, body, id)
		}
	}
	retried := false
	for _, r := range f.records(t) {
		if len(r.Attempts) == 2 && r.Attempts[0].Error == "connect refused" && r.Attempts[1].Status == 200 && r.Outcome == "ok" {
			retried = true
			if !strings.Contains(RenderWhy(&r), "tried next: 200 OK") {
				t.Errorf("why doesn't show the retry:\n%s", RenderWhy(&r))
			}
		}
	}
	if !retried {
		t.Errorf("no request was retried on the live backend after a refused connection")
	}
}

func TestNoBackendUp(t *testing.T) {
	f := newFixture(t, fmt.Sprintf("  backend %s\n", deadAddr(t)), false)
	for i := 0; i < 3; i++ {
		if rr := f.do("GET", "/api/x"); rr.Code != 502 {
			t.Fatalf("request %d: %d, want 502", i, rr.Code)
		}
	}
	rr := f.do("GET", "/api/x")
	if rr.Code != 503 || !strings.Contains(rr.Body.String(), "Request ID: "+rr.Header().Get("BareProxy-Id")) {
		t.Errorf("after 3 failed connections: %d %q, want 503 with the request ID", rr.Code, rr.Body.String())
	}
	recs := f.records(t)
	if recs[0].Outcome != "connect_failed" || recs[3].Outcome != "no_backend" {
		t.Errorf("outcomes %s and %s", recs[0].Outcome, recs[3].Outcome)
	}
}

func TestHealthChecksAndLiveExplain(t *testing.T) {
	live := echoBackend(t, "live")
	dead := deadAddr(t)
	f := newFixture(t, fmt.Sprintf("  backend %s\n  backend %s\n  health /healthz every 30ms timeout 200ms\n", strings.TrimPrefix(live.URL, "http://"), dead), true)
	pool := f.rt.Pools["api"]
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if pool.Backends[0].Snapshot().State == "up" && pool.Backends[1].Snapshot().State == "down" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s0, s1 := pool.Backends[0].Snapshot().State, pool.Backends[1].Snapshot().State; s0 != "up" || s1 != "down" {
		t.Fatalf("states %s, %s; want up, down", s0, s1)
	}
	out, err := Explain(f.rt, "GET", "http://example.com:8080/api/orders", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Sent upstream as GET /orders", "1 of 2 up", "next pick", "down since", "failed checks (connect refused)"} {
		if !strings.Contains(out, want) {
			t.Errorf("explain lacks %q:\n%s", want, out)
		}
	}
	rr := f.do("GET", "/api/orders")
	if rr.Code != 200 {
		t.Errorf("with one backend down: %d", rr.Code)
	}
	recs := f.records(t)
	last := recs[len(recs)-1]
	if len(last.Skipped) != 1 || len(last.Attempts) != 1 {
		t.Errorf("record should skip the down backend and try once: %+v", last)
	}
}

func TestExplainFilesOffline(t *testing.T) {
	f := newFixture(t, "", false)
	rt, err := NewRuntime(f.c, nil, 0, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		url, accept string
		want        []string
	}{
		{"http://example.com:8080/about/", "", []string{"line 11", "File: " + f.public + "/about/index.html", "Exists: yes", "Content-Type: text/html", "Action: serve 200"}},
		{"http://example.com:8080/foo/", "", []string{"Checked: " + f.public + "/foo/index.html", "Exists: no (no such file)", "Error page: /404.html (line 6)", "Action: 404"}},
		{"http://example.com:8080/about", "", []string{"It's a folder", "redirect 301 to /about/"}},
		{"http://example.com:8080/escape/secret.txt", "", []string{"Exists: no (it leads outside the site folder)"}},
		{"http://example.com:8080/.git/config", "", []string{"hidden name .git"}},
		{"http://example.com:8080/app.js", "br", []string{"Representation: app.js.br (Content-Encoding br)"}},
		{"http://example.com:8080/app.js", "", []string{"Also on disk: app.js.br, app.js.gz"}},
		{"http://example.com:8080/api/orders", "", []string{"line 7", "no: path is not /healthz", "match", "Sent upstream as GET /orders"}},
		{"http://example.com:8080/api/%2e%2e/about/", "", []string{"Normalized path: /about/", "Action: serve 200"}},
		{"http://other.org:8080/", "", []string{"No site for other.org", "421"}},
		{"https://example.com/", "", []string{"Nothing listens on port 443"}},
	}
	for _, c := range cases {
		h := http.Header{}
		if c.accept != "" {
			h.Set("Accept-Encoding", c.accept)
		}
		out, err := Explain(rt, "GET", c.url, h, false)
		if err != nil {
			t.Fatalf("%s: %v", c.url, err)
		}
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("explain %s lacks %q:\n%s", c.url, w, out)
			}
		}
	}
}

func TestWhy(t *testing.T) {
	f := newFixture(t, "", false)
	id := f.do("GET", "/app.js", "Accept-Encoding", "gzip").Header().Get("BareProxy-Id")
	rec, err := FindRecord(f.log, id[:8])
	if err != nil {
		t.Fatal(err)
	}
	out := RenderWhy(rec)
	for _, want := range []string{"Request " + id, "GET http://example.com:8080/app.js", "Site example.com (line 5)",
		"Rule line 11: route /* -> files public", "File: " + f.public + "/app.js", "Representation: app.js.gz (Content-Encoding gzip)",
		"Content-Type: text/javascript", "Response 200", "outcome file"} {
		if !strings.Contains(out, want) {
			t.Errorf("why lacks %q:\n%s", want, out)
		}
	}
	id = f.do("GET", "/nope/").Header().Get("BareProxy-Id")
	rec, _ = FindRecord(f.log, id)
	out = RenderWhy(rec)
	for _, want := range []string{"Checked: " + f.public + "/nope/index.html", "Reason: no such file", "Error page: " + f.public + "/404.html", "Response 404"} {
		if !strings.Contains(out, want) {
			t.Errorf("why for a miss lacks %q:\n%s", want, out)
		}
	}
	if _, err := FindRecord(f.log, "abc"); err == nil {
		t.Errorf("a 3-character ID should be refused")
	}
}

func TestReloadKeepsGoodConfig(t *testing.T) {
	f := newFixture(t, "", false)
	s := NewServer(f.conf, f.rt)
	s.logger.SetOutput(io.Discard)
	writeFile(t, f.conf, "site http://example.com:8080\n  route /* -> nopool\n")
	s.Reload()
	if v := s.Current().Version; v != 1 {
		t.Fatalf("a broken config replaced version 1 (now %d)", v)
	}
	writeFile(t, f.conf, "global\n  admin off\n  trace-log requests.log\nsite http://example.com:8080\n  route /* -> respond 200 \"v2\"\n")
	s.Reload()
	if v := s.Current().Version; v != 2 {
		t.Fatalf("a good config didn't load (version %d)", v)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "example.com:8080"
	s.Handler(8080, false).ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), "v2") {
		t.Errorf("version 2 isn't serving: %q", rr.Body.String())
	}
	s.Current().Stop()
}
