// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The plugins BareProxy ships (plugins/ in the repository), run in a real
// server. They're built by plugins/build.sh; the tests run when
// BP_PLUGINS_DIST names the folder it built them into, as in CI.

// shipped returns a shipped plugin's module, or skips the test.
func shipped(t *testing.T, name string) []byte {
	t.Helper()
	dir := os.Getenv("BP_PLUGINS_DIST")
	if dir == "" {
		t.Skip("no shipped plugins: set BP_PLUGINS_DIST to plugins/dist")
	}
	b, err := os.ReadFile(filepath.Join(dir, name+".wasm"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// shippedServer starts BareProxy with one shipped plugin, used by the site
// as p, with config conf. The site sends /api/* to backend (a dead address
// when backend is "") and the rest to files in public/.
func shippedServer(t *testing.T, name, conf, backend string) (*liveServer, error) {
	t.Helper()
	return shippedServerWith(t, name, conf, backend, "")
}

// shippedServerWith is shippedServer with more lines for the plugin block.
func shippedServerWith(t *testing.T, name, conf, backend, pluginLines string) (*liveServer, error) {
	t.Helper()
	l := &liveServer{dir: t.TempDir(), port: freePort(t)}
	l.conf = filepath.Join(l.dir, "bareproxy.conf")
	writeFile(t, filepath.Join(l.dir, "public", "page.txt"), "a static page\n")
	writeFile(t, filepath.Join(l.dir, "p.wasm"), string(shipped(t, name)))
	writeFile(t, filepath.Join(l.dir, "p.conf"), conf)
	if backend == "" {
		backend = fmt.Sprintf("127.0.0.1:%d", freePort(t)) // nothing listens there
	}
	writeFile(t, l.conf, fmt.Sprintf(`global
  admin off
  trace-log off
  state state

plugin p p.wasm
  config p.conf
%s
site http://example.com:%d
  use p
  route /api/* -> api
  route /* -> files public

pool api
  backend %s
`, pluginLines, l.port, strings.TrimPrefix(backend, "http://")))
	s, err := Start(l.conf)
	if err != nil {
		return nil, err
	}
	l.s = s
	t.Cleanup(s.Stop)
	return l, nil
}

func mustShippedServer(t *testing.T, name, conf, backend string) *liveServer {
	t.Helper()
	l, err := shippedServer(t, name, conf, backend)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// noRedirects is a client that hands back a redirect instead of following it.
var noRedirects = &http.Client{Transport: directClient.Transport,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// send makes a request with the given headers and returns the response, its
// body and the request's record.
func send(t *testing.T, l *liveServer, method, path string, hdr ...string) (*http.Response, string, *Record) {
	t.Helper()
	req, _ := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", l.port, path), nil)
	req.Host = fmt.Sprintf("example.com:%d", l.port)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := noRedirects.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	js, _ := l.s.Current().Mem.Find(resp.Header.Get("BareProxy-Id"))
	var rec Record
	if err := json.Unmarshal(js, &rec); err != nil {
		t.Fatalf("no record for %s %s: %v", method, path, err)
	}
	return resp, string(b), &rec
}

func notes(rec *Record) string {
	var all []string
	for _, p := range rec.Plugins {
		all = append(all, p.Notes...)
	}
	return strings.Join(all, " | ")
}

// backendSays is a backend that answers every request with a status, a
// header and a body.
func backendSays(t *testing.T, code int, hdr ...string) string {
	t.Helper()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i+1 < len(hdr); i += 2 {
			w.Header().Add(hdr[i], hdr[i+1])
		}
		w.WriteHeader(code)
		fmt.Fprintf(w, "backend body %d\n", code)
	}))
	t.Cleanup(b.Close)
	return b.URL
}

func TestMaintenanceFailoverPage(t *testing.T) {
	page := "page\n<h1>We'll be right back</h1>\nend\n"
	for _, code := range []int{502, 503, 504} {
		l := mustShippedServer(t, "maintenance", page, backendSays(t, code))
		resp, body, rec := send(t, l, "GET", "/api/x")
		if resp.StatusCode != code || body != "<h1>We'll be right back</h1>\n" ||
			resp.Header.Get("Retry-After") != "300" || resp.Header.Get("Content-Type") != "text/html; charset=utf-8" ||
			resp.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%d: response %d %q %v", code, resp.StatusCode, body, resp.Header)
		}
		if rec.Plugins[0].Action != fmt.Sprintf("replaced the response with %d", code) ||
			!strings.Contains(notes(rec), "failover page went out") {
			t.Errorf("%d: record %+v", code, rec.Plugins)
		}
	}

	// Other statuses, and static files, go out as they are.
	l := mustShippedServer(t, "maintenance", page, backendSays(t, 500))
	if resp, body, _ := send(t, l, "GET", "/api/x"); resp.StatusCode != 500 || body != "backend body 500\n" {
		t.Errorf("500: %d %q", resp.StatusCode, body)
	}
	if resp, body, rec := send(t, l, "GET", "/page.txt"); resp.StatusCode != 200 || body != "a static page\n" || rec.Plugins[0].Action != "went on" {
		t.Errorf("static: %d %q %+v", resp.StatusCode, body, rec.Plugins)
	}
}

func TestMaintenanceNoBackendUp(t *testing.T) {
	l := mustShippedServer(t, "maintenance", "", "")
	resp, body, rec := send(t, l, "GET", "/api/x")
	if resp.StatusCode < 502 || resp.StatusCode > 504 || !strings.Contains(body, "<h1>Back soon</h1>") {
		t.Errorf("response %d %q", resp.StatusCode, body)
	}
	if !strings.Contains(notes(rec), "failover page went out") {
		t.Errorf("record %+v", rec.Plugins)
	}
}

func TestMaintenanceMode(t *testing.T) {
	conf := "maintenance on\nallow 192.0.2.0/24\nretry-after 1h\nskip /api/health\n" +
		"maintenance-page\n<p>Maintenance until 14:00</p>\nend\n"
	l := mustShippedServer(t, "maintenance", conf, backendSays(t, 200))
	resp, body, rec := send(t, l, "GET", "/page.txt")
	if resp.StatusCode != 503 || body != "<p>Maintenance until 14:00</p>\n" || resp.Header.Get("Retry-After") != "3600" {
		t.Errorf("response %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if rec.Outcome != "plugin" || rec.Plugins[0].Action != "answered 503" || !strings.Contains(notes(rec), "maintenance is on") {
		t.Errorf("record %+v %+v", rec, rec.Plugins)
	}
	if resp, body, _ := send(t, l, "GET", "/api/health"); resp.StatusCode != 200 || body != "backend body 200\n" {
		t.Errorf("skipped path: %d %q", resp.StatusCode, body)
	}

	// The tests' requests come from 127.0.0.1: on the allow list, they pass.
	l = mustShippedServer(t, "maintenance", "maintenance on\nallow 127.0.0.1 ::1\n", "")
	resp, body, rec = send(t, l, "GET", "/page.txt")
	if resp.StatusCode != 200 || body != "a static page\n" || !strings.Contains(notes(rec), "on the allow list") {
		t.Errorf("allowed: %d %q %+v", resp.StatusCode, body, rec.Plugins)
	}
}

// configRefused checks that check names the line of the plugin's own config
// that it refused, and why.
func configRefused(t *testing.T, name, conf, want string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "p.wasm"), string(shipped(t, name)))
	writeFile(t, filepath.Join(dir, "p.conf"), conf)
	c, probs := Parse(filepath.Join(dir, "bareproxy.conf"),
		"plugin p p.wasm\n  config p.conf\nsite http://a.test:1\n  use p\n  route /* -> respond 200 x\n")
	c.Close()
	full := "line 1: error: plugin p refused its config " + filepath.Join(dir, "p.conf") + ": " + want
	for _, p := range probs {
		if p.String() == full {
			return
		}
	}
	t.Errorf("problems %q, want %q", probs, full)
}

func TestMaintenanceConfigRefused(t *testing.T) {
	configRefused(t, "maintenance", "retry-after 1h\nmaintenance soon\n", "line 2: maintenance takes on or off")
}

const corsConf = `origins https://app.example.com
methods GET POST PUT
headers content-type authorization
expose x-request-id
credentials on
max-age 1h

path /api/public/
  origins *
  credentials off
`

func TestCORSPreflight(t *testing.T) {
	var hits int
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	t.Cleanup(b.Close)
	l := mustShippedServer(t, "cors", corsConf, b.URL)

	resp, body, rec := send(t, l, "OPTIONS", "/api/orders", "Origin", "https://app.example.com",
		"Access-Control-Request-Method", "PUT", "Access-Control-Request-Headers", "Content-Type")
	h := resp.Header
	if resp.StatusCode != 204 || body != "" || h.Get("Access-Control-Allow-Origin") != "https://app.example.com" ||
		h.Get("Access-Control-Allow-Credentials") != "true" || h.Get("Access-Control-Allow-Methods") != "GET, POST, PUT" ||
		h.Get("Access-Control-Allow-Headers") != "content-type, authorization" || h.Get("Access-Control-Max-Age") != "3600" ||
		!strings.HasPrefix(h.Get("Vary"), "Origin") {
		t.Errorf("preflight %d %q %v", resp.StatusCode, body, h)
	}
	if rec.Outcome != "plugin" || rec.Plugins[0].Action != "answered 204" {
		t.Errorf("record %+v", rec.Plugins)
	}

	resp, body, rec = send(t, l, "OPTIONS", "/api/orders", "Origin", "https://evil.example",
		"Access-Control-Request-Method", "PUT")
	if resp.StatusCode != 403 || resp.Header.Get("Access-Control-Allow-Origin") != "" ||
		!strings.Contains(notes(rec), "origin https://evil.example isn't on the list for /") {
		t.Errorf("refused preflight %d %q %v %+v", resp.StatusCode, body, resp.Header, rec.Plugins)
	}
	resp, _, rec = send(t, l, "OPTIONS", "/api/orders", "Origin", "https://app.example.com",
		"Access-Control-Request-Method", "DELETE")
	if resp.StatusCode != 403 || !strings.Contains(notes(rec), "method DELETE isn't allowed") {
		t.Errorf("refused method %d %+v", resp.StatusCode, rec.Plugins)
	}
	if hits != 0 {
		t.Errorf("preflights reached the backend %d times", hits)
	}
}

func TestCORSResponses(t *testing.T) {
	// The backend sets CORS headers of its own; the plugin's list wins.
	back := backendSays(t, 200, "Access-Control-Allow-Origin", "*", "Access-Control-Allow-Methods", "*", "Vary", "Accept-Encoding")
	l := mustShippedServer(t, "cors", corsConf, back)

	resp, body, _ := send(t, l, "GET", "/api/orders", "Origin", "https://app.example.com")
	h := resp.Header
	if resp.StatusCode != 200 || body != "backend body 200\n" || h.Get("Access-Control-Allow-Origin") != "https://app.example.com" ||
		h.Get("Access-Control-Allow-Credentials") != "true" || h.Get("Access-Control-Expose-Headers") != "x-request-id" ||
		h.Get("Access-Control-Allow-Methods") != "" || h.Get("Vary") != "Accept-Encoding, Origin" {
		t.Errorf("allowed origin %d %q %v", resp.StatusCode, body, h)
	}

	resp, _, rec := send(t, l, "GET", "/api/orders", "Origin", "https://evil.example")
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "" ||
		resp.Header.Get("Vary") != "Accept-Encoding, Origin" || !strings.Contains(notes(rec), "isn't on the list") {
		t.Errorf("other origin %d %v %+v", resp.StatusCode, resp.Header, rec.Plugins)
	}

	// The public path takes any origin, without credentials.
	resp, _, _ = send(t, l, "GET", "/api/public/feed", "Origin", "https://anyone.example")
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" || resp.Header.Get("Access-Control-Allow-Credentials") != "" {
		t.Errorf("public path %v", resp.Header)
	}

	// Static files get the headers too; a request with no Origin gets only Vary.
	resp, _, _ = send(t, l, "GET", "/page.txt", "Origin", "https://app.example.com")
	if resp.Header.Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Errorf("static file %v", resp.Header)
	}
	resp, _, _ = send(t, l, "GET", "/page.txt")
	if resp.Header.Get("Access-Control-Allow-Origin") != "" || resp.Header.Get("Vary") != "Origin" {
		t.Errorf("no origin %v", resp.Header)
	}
}

func TestCORSConfigRefused(t *testing.T) {
	configRefused(t, "cors", "origins *\ncredentials on\n", "credentials on needs a list of origins, not *: "+
		"browsers refuse that pair, and it would let any site call with a visitor's cookies")
}

// With CORS before the maintenance plugin, the maintenance and failover
// pages carry the CORS headers, so a page on another site can read the 503.
func TestCORSOnMaintenancePages(t *testing.T) {
	for _, c := range []struct{ down, path string }{{"maintenance on\n", "/page.txt"}, {"", "/api/x"}} {
		l := &liveServer{dir: t.TempDir(), port: freePort(t)}
		l.conf = filepath.Join(l.dir, "bareproxy.conf")
		writeFile(t, filepath.Join(l.dir, "public", "page.txt"), "a static page\n")
		writeFile(t, filepath.Join(l.dir, "cors.wasm"), string(shipped(t, "cors")))
		writeFile(t, filepath.Join(l.dir, "maintenance.wasm"), string(shipped(t, "maintenance")))
		writeFile(t, filepath.Join(l.dir, "cors.conf"), "origins https://app.example.com\n")
		writeFile(t, filepath.Join(l.dir, "down.conf"), c.down)
		writeFile(t, l.conf, fmt.Sprintf(`global
  admin off
  trace-log off
  state state

plugin cors cors.wasm
  config cors.conf
plugin down maintenance.wasm
  config down.conf

site http://example.com:%d
  use cors down
  route /api/* -> api
  route /* -> files public

pool api
  backend 127.0.0.1:%d
`, l.port, freePort(t)))
		s, err := Start(l.conf)
		if err != nil {
			t.Fatal(err)
		}
		l.s = s
		resp, body, _ := send(t, l, "GET", c.path, "Origin", "https://app.example.com")
		if resp.StatusCode < 502 || resp.StatusCode > 504 || !strings.Contains(body, "Back soon") ||
			resp.Header.Get("Access-Control-Allow-Origin") != "https://app.example.com" || resp.Header.Get("Vary") != "Origin" {
			t.Errorf("%s: %d %q %v", c.path, resp.StatusCode, body, resp.Header)
		}
		s.Stop()
	}
}

const headersConf = `redirect /old-page /page.txt
redirect /home /de/ when accept-language has de
rewrite /docs/* /manual/*

request set x-from-proxy yes
request remove x-secret
response remove x-powered-by

path /manual/
  response set cache-control public, max-age=3600
path /docs/
  response add x-docs 1
`

func TestHeaderRules(t *testing.T) {
	var seen http.Header
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("X-Powered-By", "PHP/5.4")
		w.Header().Set("X-Frame-Options", "DENY")
		fmt.Fprintln(w, "from the app")
	}))
	t.Cleanup(b.Close)
	l := mustShippedServer(t, "headers", headersConf, b.URL)
	writeFile(t, filepath.Join(l.dir, "public", "manual", "intro.txt"), "the manual\n")

	// Redirects, with the query kept; one that depends on a header says so in Vary.
	resp, _, rec := send(t, l, "GET", "/old-page?a=1")
	if resp.StatusCode != 301 || resp.Header.Get("Location") != "/page.txt?a=1" ||
		!strings.Contains(notes(rec), "redirected /old-page to /page.txt (line 1)") {
		t.Errorf("redirect %d %v %+v", resp.StatusCode, resp.Header, rec.Plugins)
	}
	resp, _, _ = send(t, l, "GET", "/home", "Accept-Language", "de-DE,de;q=0.9")
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "/de/" || resp.Header.Get("Vary") != "accept-language" {
		t.Errorf("redirect by language %d %v", resp.StatusCode, resp.Header)
	}
	if resp, _, _ := send(t, l, "GET", "/home", "Accept-Language", "en"); resp.StatusCode != 404 {
		t.Errorf("no redirect for en: %d", resp.StatusCode)
	}

	// A rewrite: routed and served as /manual/intro.txt. Path sections
	// match the path as it arrived, so /docs/ applies and /manual/ doesn't.
	resp, body, rec := send(t, l, "GET", "/docs/intro.txt")
	if resp.StatusCode != 200 || body != "the manual\n" || resp.Header.Get("X-Docs") != "1" || resp.Header.Get("Cache-Control") != "" {
		t.Errorf("rewrite %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if !strings.Contains(notes(rec), "rewrote /docs/intro.txt to /manual/intro.txt (line 3)") ||
		!strings.Contains(notes(rec), "the plugins changed the path to /manual/intro.txt") {
		t.Errorf("rewrite record %+v", rec.Plugins)
	}
	if why := RenderWhy(rec); !strings.Contains(why, "rewrote /docs/intro.txt") {
		t.Errorf("why doesn't show the rewrite:\n%s", why)
	}

	// Security headers on a static file; none over plain http for HSTS.
	resp, _, _ = send(t, l, "GET", "/manual/intro.txt")
	h := resp.Header
	if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "strict-origin-when-cross-origin" ||
		h.Get("X-Frame-Options") != "SAMEORIGIN" || h.Get("Strict-Transport-Security") != "" || h.Get("Cache-Control") != "public, max-age=3600" {
		t.Errorf("static headers %v", h)
	}

	// Request headers reach the app changed; the app's own X-Frame-Options stays.
	resp, body, _ = send(t, l, "GET", "/api/x", "X-Secret", "s3cret")
	if body != "from the app\n" || seen.Get("X-From-Proxy") != "yes" || seen.Get("X-Secret") != "" {
		t.Errorf("request headers %q %v", body, seen)
	}
	if resp.Header.Get("X-Powered-By") != "" || resp.Header.Get("X-Frame-Options") != "DENY" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("app response headers %v", resp.Header)
	}
}

func TestHeaderRulesConfigRefused(t *testing.T) {
	configRefused(t, "headers", "response set cache-control\n", "line 1: set cache-control needs a value")
}

const redirectFile = `# old path          new address
/p/123              /posts/hello
/about-us           /about/          308
/shop/old-cat/*     /shop/new-cat/*
/blog/*             https://blog.example.com/*   302
`

// redirectServer starts BareProxy with the redirects plugin reading
// lists/redirects.txt, with two instances and a 1-second reload.
func redirectServer(t *testing.T, file string) *liveServer {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "redirects.txt"), file)
	l := mustShippedServer2(t, "redirects", "file redirects.txt\nreload 1s\nreport /.redirects\n",
		"  read "+dir+"\n  instances 2\n")
	l.dir = dir // where the list is
	return l
}

func mustShippedServer2(t *testing.T, name, conf, lines string) *liveServer {
	t.Helper()
	l, err := shippedServerWith(t, name, conf, "", lines)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestRedirectsFromFile(t *testing.T) {
	l := redirectServer(t, redirectFile)
	for _, c := range []struct {
		path, loc string
		code      int
		line      string
	}{
		{"/p/123?utm=x", "/posts/hello?utm=x", 301, "line 2"},
		{"/p/123/", "/posts/hello", 301, "line 2"},
		{"/about-us", "/about/", 308, "line 3"},
		{"/shop/old-cat/shoes/red", "/shop/new-cat/shoes/red", 301, "line 4"},
		{"/blog/2020/post", "https://blog.example.com/2020/post", 302, "line 5"},
	} {
		resp, _, rec := send(t, l, "GET", c.path)
		if resp.StatusCode != c.code || resp.Header.Get("Location") != c.loc ||
			!strings.Contains(notes(rec), "redirects.txt "+c.line+": ") || rec.Plugins[0].Action != fmt.Sprintf("answered %d", c.code) {
			t.Errorf("%s: %d %v %+v", c.path, resp.StatusCode, resp.Header, rec.Plugins)
		}
	}
	if resp, body, _ := send(t, l, "GET", "/page.txt"); resp.StatusCode != 200 || body != "a static page\n" {
		t.Errorf("a path not on the list: %d %q", resp.StatusCode, body)
	}

	// Hits from both instances come together in the report within a tick.
	for range 6 {
		send(t, l, "GET", "/p/123")
	}
	time.Sleep(1500 * time.Millisecond)
	_, body, _ := send(t, l, "GET", "/.redirects")
	if !strings.Contains(body, "4 lines") || !strings.Contains(body, "8\t2\t/p/123\t/posts/hello\n") ||
		!strings.Contains(body, "Never used: 0 lines") {
		t.Errorf("report:\n%s", body)
	}
	if got := l.s.Current().Plugins["p"].Metrics()["redirects"]; got != 11 {
		t.Errorf("metric redirects = %d, want 11", got)
	}
}

func TestRedirectsReload(t *testing.T) {
	l := redirectServer(t, redirectFile)
	// A changed file is picked up on its own.
	writeFile(t, filepath.Join(l.dir, "redirects.txt"), redirectFile+"/new-line /somewhere\n")
	waitFor(t, func() bool { r, _, _ := send(t, l, "GET", "/new-line"); return r.StatusCode == 301 })
	// A broken one is refused, and the list as it was keeps working.
	writeFile(t, filepath.Join(l.dir, "redirects.txt"), "/new-line /elsewhere\n/broken\n")
	time.Sleep(2500 * time.Millisecond)
	for range 4 { // both instances
		if r, _, _ := send(t, l, "GET", "/new-line"); r.Header.Get("Location") != "/somewhere" {
			t.Fatalf("the broken file replaced the list: %v", r.Header)
		}
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
	}
}

func TestRedirectsFileChecked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "p.wasm"), string(shipped(t, "redirects")))
	writeFile(t, filepath.Join(dir, "p.conf"), "file redirects.txt\n")
	writeFile(t, filepath.Join(dir, "lists", "redirects.txt"), "/a /b\n/a /c\n")
	conf := "plugin p p.wasm\n  config p.conf\n  read lists\nsite http://a.test:1\n  use p\n  route /* -> respond 200 x\n"
	for _, c := range []struct{ conf, want string }{
		{conf, "redirects.txt line 2: /a is on line 1 already"},
		{strings.Replace(conf, "  read lists\n", "", 1), "can't read redirects.txt: it isn't in a folder the plugin line names with read"},
	} {
		cfg, probs := Parse(filepath.Join(dir, "bareproxy.conf"), c.conf)
		cfg.Close()
		full := "line 1: error: plugin p refused its config " + filepath.Join(dir, "p.conf") + ": " + c.want
		if !slices.ContainsFunc(probs, func(p Problem) bool { return p.String() == full }) {
			t.Errorf("problems %q, want %q", probs, full)
		}
	}
}

// A hundred thousand redirects load at the start, reload on a tick and
// cost a lookup each.
func TestRedirectsLargeFile(t *testing.T) {
	var b strings.Builder
	for i := range 100_000 {
		fmt.Fprintf(&b, "/old/page-%d /new/page-%d\n", i, i)
	}
	start := time.Now()
	l := redirectServer(t, b.String())
	t.Logf("started with 100,000 redirects in %v", time.Since(start))
	if r, _, _ := send(t, l, "GET", "/old/page-99999"); r.Header.Get("Location") != "/new/page-99999" {
		t.Fatalf("lookup: %v", r.Header)
	}
	writeFile(t, filepath.Join(l.dir, "redirects.txt"), b.String()+"/added /here\n")
	waitFor(t, func() bool { r, _, _ := send(t, l, "GET", "/added"); return r.StatusCode == 301 })
}
