// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"compress/gzip"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	js := findRecord(l, resp.Header.Get("BareProxy-Id"))
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

// With the default 30-second reload, hits are still shared every 5 seconds.
func TestRedirectsReportWithin5s(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "redirects.txt"), redirectFile)
	l := mustShippedServer2(t, "redirects", "file redirects.txt\nreport /.redirects\n", "  read "+dir+"\n  instances 2\n")
	for range 4 {
		send(t, l, "GET", "/about-us")
	}
	time.Sleep(5500 * time.Millisecond)
	for range 2 { // both instances answer the report the same
		if _, body, _ := send(t, l, "GET", "/.redirects"); !strings.Contains(body, "4\t3\t/about-us\t/about/\n") ||
			!strings.Contains(body, "brought together every 5s") {
			t.Errorf("report:\n%s", body)
		}
	}
}

// geoServer starts BareProxy with the geo plugin reading db (a file in
// plugins/geo/testdata) and believing X-Forwarded-For from 127.0.0.1, with
// a route that matches on the country header.
func geoServer(t *testing.T, db, conf string) (*liveServer, *http.Header) {
	t.Helper()
	l := &liveServer{dir: t.TempDir(), port: freePort(t)}
	l.conf = filepath.Join(l.dir, "bareproxy.conf")
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "plugins", "geo", "testdata", db))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(l.dir, "geoip", "country.mmdb"), string(src))
	writeFile(t, filepath.Join(l.dir, "geo.wasm"), string(shipped(t, "geo")))
	writeFile(t, filepath.Join(l.dir, "geo.conf"), "database country.mmdb\nforwarded-from 127.0.0.1\nreload 1s\n"+conf)
	seen := &http.Header{}
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.Header.Clone()
		fmt.Fprintln(w, "from the app")
	}))
	t.Cleanup(b.Close)
	writeFile(t, l.conf, fmt.Sprintf(`global
  admin off
  trace-log off
  state state

plugin geo geo.wasm
  config geo.conf
  read geoip

site http://example.com:%d
  use geo
  route /* header X-Country=SE -> respond 200 "hej from the Swedish route"
  route /* -> app

pool app
  backend %s
`, l.port, strings.TrimPrefix(b.URL, "http://")))
	s, err := Start(l.conf)
	if err != nil {
		t.Fatal(err)
	}
	l.s = s
	t.Cleanup(s.Stop)
	return l, seen
}

func TestGeoCountryHeaderAndRoutes(t *testing.T) {
	l, seen := geoServer(t, "GeoIP2-Country-Test.mmdb", "")
	// The app gets the country, and a country the client sends itself is replaced.
	resp, body, _ := send(t, l, "GET", "/x", "X-Forwarded-For", "2.125.160.216", "X-Country", "SE")
	if resp.StatusCode != 200 || body != "from the app\n" || seen.Get("X-Country") != "GB" {
		t.Errorf("GB: %d %q %v", resp.StatusCode, body, *seen)
	}
	// A route rule matches on the header the plugin set.
	if _, body, rec := send(t, l, "GET", "/x", "X-Forwarded-For", "89.160.20.115"); body != "hej from the Swedish route\n" {
		t.Errorf("SE: %q %+v", body, rec)
	}
	// IPv6, and an address the database doesn't know.
	send(t, l, "GET", "/x", "X-Forwarded-For", "2001:218::1")
	if seen.Get("X-Country") != "JP" {
		t.Errorf("JP: %v", *seen)
	}
	send(t, l, "GET", "/x")
	if seen.Get("X-Country") != "XX" {
		t.Errorf("unknown: %v", *seen)
	}
}

func TestGeoAllowAndBlock(t *testing.T) {
	l, _ := geoServer(t, "GeoIP2-Country-Test.mmdb", "block CN\n\npath /checkout/\n  allow SE GB\n  unknown block\npage\n<p>Not here</p>\nend\n")
	resp, body, rec := send(t, l, "GET", "/x", "X-Forwarded-For", "175.16.199.5")
	if resp.StatusCode != 403 || body != "<p>Not here</p>\n" ||
		!strings.Contains(notes(rec), "turned away: CN is on the block list for / (line 4) (address 175.16.199.5)") {
		t.Errorf("CN: %d %q %+v", resp.StatusCode, body, rec.Plugins)
	}
	if resp, _, _ := send(t, l, "GET", "/checkout/pay", "X-Forwarded-For", "2.125.160.216"); resp.StatusCode != 200 {
		t.Errorf("GB at checkout: %d", resp.StatusCode)
	}
	resp, _, rec = send(t, l, "GET", "/checkout/pay", "X-Forwarded-For", "2001:218::1")
	if resp.StatusCode != 403 || !strings.Contains(notes(rec), "JP isn't on the allow list for /checkout/") {
		t.Errorf("JP at checkout: %d %+v", resp.StatusCode, rec.Plugins)
	}
	if resp, _, _ := send(t, l, "GET", "/checkout/pay"); resp.StatusCode != 403 {
		t.Errorf("unknown at checkout: %d", resp.StatusCode)
	}
	if resp, _, _ := send(t, l, "GET", "/x"); resp.StatusCode != 200 {
		t.Errorf("unknown elsewhere: %d", resp.StatusCode)
	}
}

func TestGeoReload(t *testing.T) {
	l, seen := geoServer(t, "MaxMind-DB-test-ipv4-24.mmdb", "")
	send(t, l, "GET", "/x", "X-Forwarded-For", "2.125.160.216")
	if seen.Get("X-Country") != "XX" {
		t.Fatalf("before: %v", *seen)
	}
	src, _ := os.ReadFile(filepath.Join("..", "..", "..", "plugins", "geo", "testdata", "GeoIP2-Country-Test.mmdb"))
	writeFile(t, filepath.Join(l.dir, "geoip", "country.mmdb"), string(src))
	waitFor(t, func() bool {
		send(t, l, "GET", "/x", "X-Forwarded-For", "2.125.160.216")
		return seen.Get("X-Country") == "GB"
	})
	// A broken file is refused; the database as it was keeps answering.
	writeFile(t, filepath.Join(l.dir, "geoip", "country.mmdb"), "not a database")
	time.Sleep(2500 * time.Millisecond)
	send(t, l, "GET", "/x", "X-Forwarded-For", "2.125.160.216")
	if seen.Get("X-Country") != "GB" {
		t.Errorf("after a broken file: %v", *seen)
	}
}

func TestGeoDatabaseChecked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "p.wasm"), string(shipped(t, "geo")))
	writeFile(t, filepath.Join(dir, "p.conf"), "database country.mmdb\n")
	writeFile(t, filepath.Join(dir, "geoip", "country.mmdb"), "not a database")
	conf := "plugin p p.wasm\n  config p.conf\n  read geoip\nsite http://a.test:1\n  use p\n  route /* -> respond 200 x\n"
	cfg, probs := Parse(filepath.Join(dir, "bareproxy.conf"), conf)
	cfg.Close()
	full := "line 1: error: plugin p refused its config " + filepath.Join(dir, "p.conf") +
		": country.mmdb: this isn't a MaxMind DB file (.mmdb): it has no metadata marker"
	if !slices.ContainsFunc(probs, func(p Problem) bool { return p.String() == full }) {
		t.Errorf("problems %q, want %q", probs, full)
	}
}

const signKey = "0123456789abcdef0123456789abcdef"

// hmacB64 is the signing plugin's signature, made here independently, as
// an app or a script would: HMAC-SHA256, base64url without padding.
func hmacB64(key, msg string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func signServer(t *testing.T, conf string) (*liveServer, *http.Header, *string) {
	t.Helper()
	l := &liveServer{dir: t.TempDir(), port: freePort(t)}
	l.conf = filepath.Join(l.dir, "bareproxy.conf")
	writeFile(t, filepath.Join(l.dir, "private", "report.txt"), "the report\n")
	writeFile(t, filepath.Join(l.dir, "sign.wasm"), string(shipped(t, "signing")))
	writeFile(t, filepath.Join(l.dir, "sign.conf"), conf)
	seen, path := &http.Header{}, new(string)
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen, *path = r.Header.Clone(), r.URL.RequestURI()
		fmt.Fprintln(w, "from the app")
	}))
	t.Cleanup(b.Close)
	writeFile(t, l.conf, fmt.Sprintf(`global
  admin off
  trace-log off
  state state

plugin sign sign.wasm
  config sign.conf

site http://example.com:%d
  use sign
  route /api/* -> app strip
  route /private/* -> files .

pool app
  backend %s
`, l.port, strings.TrimPrefix(b.URL, "http://")))
	s, err := Start(l.conf)
	if err != nil {
		t.Fatal(err)
	}
	l.s = s
	t.Cleanup(s.Stop)
	return l, seen, path
}

func TestSignedRequests(t *testing.T) {
	l, seen, path := signServer(t, "key app "+signKey+"\nsign key app\nsign-headers x-user\n")
	resp, body, _ := send(t, l, "GET", "/api/orders?page=2", "X-User", "ann", "X-BareProxy-Signature", "v1=forged")
	if resp.StatusCode != 200 || body != "from the app\n" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	// The app got the stripped path; the signature covers the path as it arrived.
	if *path != "/orders?page=2" || seen.Get("X-BareProxy-Path") != "/api/orders?page=2" {
		t.Errorf("path %q, signed path %q", *path, seen.Get("X-BareProxy-Path"))
	}
	ts := seen.Get("X-BareProxy-Timestamp")
	if n, err := strconv.ParseInt(ts, 10, 64); err != nil || time.Since(time.Unix(n, 0)).Abs() > time.Minute {
		t.Errorf("timestamp %q", ts)
	}
	msg := "v1\n" + ts + "\nGET\n" + seen.Get("X-BareProxy-Path") + "\nx-user:ann"
	if got, want := seen.Get("X-BareProxy-Signature"), "v1="+hmacB64(signKey, msg); got != want {
		t.Errorf("signature %q, want %q", got, want)
	}
	if seen.Get("X-BareProxy-Signed-Headers") != "x-user" {
		t.Errorf("signed headers %q", seen.Get("X-BareProxy-Signed-Headers"))
	}
}

func TestSignedLinks(t *testing.T) {
	l, _, _ := signServer(t, "key new "+signKey+"\nkey old fedcba9876543210fedcba9876543210\nlinks key new old\nlink-path /private/\n")
	link := func(key, path string, exp int64) string {
		return fmt.Sprintf("%s?expires=%d&sig=%s", path, exp, hmacB64(key, fmt.Sprintf("v1\n%d\n%s", exp, path)))
	}
	soon := time.Now().Add(time.Hour).Unix()
	if resp, body, _ := send(t, l, "GET", link(signKey, "/private/report.txt", soon)); resp.StatusCode != 200 || body != "the report\n" {
		t.Errorf("good link: %d %q", resp.StatusCode, body)
	}
	if resp, _, _ := send(t, l, "GET", link("fedcba9876543210fedcba9876543210", "/private/report.txt", soon)); resp.StatusCode != 200 {
		t.Errorf("old key: %d", resp.StatusCode)
	}
	for _, c := range []struct{ target, why string }{
		{"/private/report.txt", "no signed link"},
		{link(signKey, "/private/other.txt", soon)[len("/private/other.txt"):], "doesn't match"},
		{link("not-the-key-not-the-key-not-the-key", "/private/report.txt", soon), "doesn't match"},
		{link(signKey, "/private/report.txt", time.Now().Add(-2*time.Hour).Unix()), "the link expired 2h ago"},
	} {
		target := c.target
		if strings.HasPrefix(target, "?") {
			target = "/private/report.txt" + target
		}
		resp, body, rec := send(t, l, "GET", target)
		if resp.StatusCode != 403 || body != "This link isn't valid, or has expired.\n" || !strings.Contains(notes(rec), c.why) {
			t.Errorf("%s: %d %q %+v", target, resp.StatusCode, body, rec.Plugins)
		}
	}
	// Paths not under link-path need nothing.
	if resp, _, _ := send(t, l, "GET", "/api/x"); resp.StatusCode != 200 {
		t.Errorf("unsigned path: %d", resp.StatusCode)
	}
}

func TestSigningConfigRefused(t *testing.T) {
	configRefused(t, "signing", "key app short\nsign key app\n",
		"line 1: key app is too short: use at least 32 characters, such as the output of openssl rand -hex 32")
}

func splitServer(t *testing.T, conf string) *liveServer {
	t.Helper()
	l := &liveServer{dir: t.TempDir(), port: freePort(t)}
	l.conf = filepath.Join(l.dir, "bareproxy.conf")
	writeFile(t, filepath.Join(l.dir, "split.wasm"), string(shipped(t, "splits")))
	writeFile(t, filepath.Join(l.dir, "split.conf"), conf)
	writeFile(t, l.conf, fmt.Sprintf(`global
  admin off
  trace-log off
  state state

plugin split split.wasm
  config split.conf

site http://example.com:%d
  use split
  route /* header X-Variant=canary -> respond 200 "canary"
  route /* -> respond 200 "stable"
`, l.port))
	s, err := Start(l.conf)
	if err != nil {
		t.Fatal(err)
	}
	l.s = s
	t.Cleanup(s.Stop)
	return l
}

func TestSplitsShareAndCookie(t *testing.T) {
	l := splitServer(t, "variant stable 50\nvariant canary 50\n")
	counts := map[string]int{}
	var canaryCookie string
	for range 400 {
		resp, body, rec := send(t, l, "GET", "/")
		counts[body]++
		sc := resp.Header.Get("Set-Cookie")
		if !strings.HasPrefix(sc, "bp_variant="+strings.TrimSpace(body)+"; Path=/; Max-Age=2592000; SameSite=Lax; HttpOnly") ||
			!strings.Contains(notes(rec), "assigned by share (50 of 100)") {
			t.Fatalf("%q, Set-Cookie %q, %+v", body, sc, rec.Plugins)
		}
		if body == "canary\n" {
			canaryCookie = strings.Split(sc, ";")[0]
		}
	}
	if counts["canary\n"] < 150 || counts["stable\n"] < 150 {
		t.Errorf("a 50/50 split gave %v", counts)
	}
	// The cookie keeps a visitor on their variant, and isn't set again.
	for range 20 {
		resp, body, rec := send(t, l, "GET", "/", "Cookie", canaryCookie)
		if body != "canary\n" || resp.Header.Get("Set-Cookie") != "" || !strings.Contains(notes(rec), "kept by the bp_variant cookie") {
			t.Fatalf("with the cookie: %q %v %+v", body, resp.Header, rec.Plugins)
		}
	}
	if m := l.s.Current().Plugins["split"].Metrics(); m["variant.canary"]+m["variant.stable"] != 420 || m["variant.canary"] < 170 {
		t.Errorf("metrics %v", m)
	}
}

func TestSplitsForce(t *testing.T) {
	l := splitServer(t, "variant stable 100\nvariant canary 0\nforce canary header x-beta yes\nforce canary query preview=canary\n")
	// Nobody gets a variant with no share, and the header can't be sent from outside.
	for range 20 {
		if _, body, _ := send(t, l, "GET", "/", "X-Variant", "canary"); body != "stable\n" {
			t.Fatalf("got %q", body)
		}
	}
	resp, body, rec := send(t, l, "GET", "/", "X-Beta", "yes")
	if body != "canary\n" || resp.Header.Get("Set-Cookie") != "" || !strings.Contains(notes(rec), "variant canary: forced by header x-beta (line 3)") {
		t.Errorf("forced by header: %q %v %+v", body, resp.Header, rec.Plugins)
	}
	// A preview link sticks.
	resp, body, _ = send(t, l, "GET", "/?preview=canary")
	if body != "canary\n" || !strings.HasPrefix(resp.Header.Get("Set-Cookie"), "bp_variant=canary;") {
		t.Errorf("preview link: %q %v", body, resp.Header)
	}
}

func TestSplitsConfigRefused(t *testing.T) {
	configRefused(t, "splits", "variant stable 95\nvariant canary 5\nforce beta address 10.0.0.0/8\n", "line 3: no variant named beta")
}

const limitsConf = `forwarded-from 127.0.0.1
allow 192.0.2.1
methods GET HEAD POST
block-path /wp-login.php
common-rules on
limit login 3 per 1m path /login
`

func TestLimitsCountAcrossInstances(t *testing.T) {
	l := mustShippedServer2(t, "limits", limitsConf, "  instances 3\n")
	for i := 1; i <= 3; i++ {
		if resp, _, _ := send(t, l, "GET", "/login", "X-Forwarded-For", "203.0.113.9"); resp.StatusCode == 429 {
			t.Fatalf("request %d limited", i)
		}
	}
	// The fourth, on whichever instance, is over: the count is BareProxy's, not an instance's.
	resp, body, rec := send(t, l, "GET", "/login", "X-Forwarded-For", "203.0.113.9")
	if resp.StatusCode != 429 || body != "Too many requests. Please slow down.\n" {
		t.Fatalf("4th: %d %q", resp.StatusCode, body)
	}
	if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err != nil || n < 1 || n > 60 {
		t.Errorf("Retry-After %q", resp.Header.Get("Retry-After"))
	}
	if !strings.Contains(notes(rec), "limit login (line 6): 4 requests in the last 60s from 203.0.113.9, over 3; the window resets in") {
		t.Errorf("record %+v", rec.Plugins)
	}
	// Another address has its own count, other paths aren't limited, and the allow list is never limited.
	if resp, _, _ := send(t, l, "GET", "/login", "X-Forwarded-For", "198.51.100.7"); resp.StatusCode == 429 {
		t.Error("another address was limited")
	}
	if resp, _, _ := send(t, l, "GET", "/page.txt", "X-Forwarded-For", "203.0.113.9"); resp.StatusCode != 200 {
		t.Errorf("another path: %d", resp.StatusCode)
	}
	for range 5 {
		if resp, _, _ := send(t, l, "GET", "/login", "X-Forwarded-For", "192.0.2.1"); resp.StatusCode == 429 {
			t.Fatal("an allowed address was limited")
		}
	}
	if m := l.s.Current().Plugins["p"].Metrics(); m["limited"] != 1 {
		t.Errorf("metrics %v", m)
	}
}

func TestLimitsWAF(t *testing.T) {
	l := mustShippedServer2(t, "limits", limitsConf, "")
	for _, c := range []struct {
		method, path string
		hdr          []string
		code         int
		note         string
	}{
		{"DELETE", "/page.txt", nil, 405, "WAF: method DELETE isn't one the site uses"},
		{"GET", "/wp-login.php", nil, 403, "WAF: path /wp-login.php is blocked (line 4)"},
		{"GET", "/.env", nil, 403, "WAF: path /.env is blocked (common-rules)"},
		{"GET", "/.git/config", nil, 403, "WAF: path /.git/ is blocked (common-rules)"},
		{"GET", "/page.txt", []string{"User-Agent", "sqlmap/1.8"}, 403, "WAF: user agent has sqlmap (common-rules)"},
		{"GET", "/page.txt", []string{"User-Agent", "Mozilla/5.0"}, 200, ""},
	} {
		resp, _, rec := send(t, l, c.method, c.path, c.hdr...)
		if resp.StatusCode != c.code || !strings.Contains(notes(rec), c.note) {
			t.Errorf("%s %s: %d %v %+v", c.method, c.path, resp.StatusCode, resp.Header, rec.Plugins)
		}
		if c.code == 405 && resp.Header.Get("Allow") != "GET, HEAD, POST" {
			t.Errorf("Allow %q", resp.Header.Get("Allow"))
		}
	}
}

func TestLimitsConfigRefused(t *testing.T) {
	configRefused(t, "limits", "limit login 5 every 1m\n",
		"line 1: write limit NAME COUNT per TIME [by address|site] [path PREFIX...], such as limit login 5 per 1m path /login")
}

const previewPage = `<!doctype html>
<html><head>
<title>Tea and Biscuits</title>
</head>
<body>
<h1>Tea</h1>
<p>Warm the pot first. Then put in one spoon of tea for each cup, and one for the pot.</p>
<img src="/img/pot.jpg" alt="a teapot">
</body></html>
`

func TestLinkPreviews(t *testing.T) {
	l := mustShippedServer2(t, "previews", "site-name Example\n", "  body response\n")
	writeFile(t, filepath.Join(l.dir, "public", "tea.html"), previewPage)

	resp, body, rec := send(t, l, "GET", "/tea.html", "User-Agent", "Slackbot-LinkExpanding 1.0 (+https://api.slack.com/robots)")
	host := fmt.Sprintf("http://example.com:%d", l.port)
	for _, want := range []string{
		`<meta property="og:title" content="Tea and Biscuits">`,
		`<meta property="og:description" content="Warm the pot first. Then put in one spoon of tea for each cup, and one for the pot.">`,
		`<meta property="og:image" content="` + host + `/img/pot.jpg">`,
		`<meta property="og:url" content="` + host + `/tea.html">`,
		`<meta property="og:site_name" content="Example">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in\n%s", want, body)
		}
	}
	if resp.Header.Get("Content-Length") != strconv.Itoa(len(body)) || strings.Index(body, "og:title") > strings.Index(body, "</head>") {
		t.Errorf("length %q for %d bytes", resp.Header.Get("Content-Length"), len(body))
	}
	if !strings.Contains(notes(rec), "link previews: added og:title, og:description, og:image, og:url, og:type, og:site_name, twitter:card for slackbot") {
		t.Errorf("record %+v", rec.Plugins)
	}

	// A visitor gets the page as it is, and its body never goes to the plugin.
	resp, body, rec = send(t, l, "GET", "/tea.html", "User-Agent", "Mozilla/5.0 Safari/605")
	if body != previewPage || notes(rec) != "" {
		t.Errorf("visitor got %q, %+v", body, rec.Plugins)
	}

	// Not HTML, or not 200: as it was.
	if _, body, _ := send(t, l, "GET", "/page.txt", "User-Agent", "facebookexternalhit/1.1"); body != "a static page\n" {
		t.Errorf("text file: %q", body)
	}
}

// The body is held for the plugin only for fetchers: with a body limit
// smaller than the page, a fetcher's request notes that the body was too big
// to hand over, and a visitor's has nothing to note, since it streamed.
func TestLinkPreviewsVisitorsStream(t *testing.T) {
	l := mustShippedServer2(t, "previews", "", "  body response\n  body-limit 1KB\n")
	writeFile(t, filepath.Join(l.dir, "public", "big.html"), "<html><head></head><body>"+strings.Repeat("<p>tea</p>", 500)+"</body></html>")
	_, _, rec := send(t, l, "GET", "/big.html", "User-Agent", "WhatsApp/2.23")
	if !strings.Contains(notes(rec), "over the body limit") {
		t.Errorf("fetcher: %+v", rec.Plugins)
	}
	_, _, rec = send(t, l, "GET", "/big.html", "User-Agent", "Mozilla/5.0")
	if notes(rec) != "" {
		t.Errorf("visitor: %+v", rec.Plugins)
	}
}

func TestLinkPreviewsConfigRefused(t *testing.T) {
	configRefused(t, "previews", "image card.png\n", "line 1: card.png isn't a path or a full http:// or https:// URL")
}

// listServer serves the address lists the crawler tests fetch, and counts
// the requests for each path. A body "status N" answers N, and "redirect
// PATH" redirects there.
type listServer struct {
	mu    sync.Mutex
	files map[string]string
	hits  map[string]int
	srv   *httptest.Server
}

func newListServer(t *testing.T, files map[string]string) *listServer {
	t.Helper()
	ls := &listServer{files: files, hits: map[string]int{}}
	ls.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ls.mu.Lock()
		defer ls.mu.Unlock()
		ls.hits[r.URL.Path]++
		body, ok := ls.files[r.URL.Path]
		switch {
		case !ok:
			http.NotFound(w, r)
		case strings.HasPrefix(body, "redirect "):
			http.Redirect(w, r, strings.TrimPrefix(body, "redirect "), http.StatusFound)
		case strings.HasPrefix(body, "status "):
			code, _ := strconv.Atoi(strings.TrimPrefix(body, "status "))
			w.WriteHeader(code)
		default:
			io.WriteString(w, body)
		}
	}))
	t.Cleanup(ls.srv.Close)
	return ls
}

func (ls *listServer) count(path string) int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.hits[path]
}

const (
	uaGPTBot     = "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; GPTBot/1.4; +https://openai.com/gptbot"
	uaClaudeBot  = "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; ClaudeBot/1.0; +claudebot@anthropic.com)"
	uaPerplexity = "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; PerplexityBot/1.0; +https://perplexity.ai/perplexitybot)"
	uaBytespider = "Mozilla/5.0 (Linux; Android 5.0) AppleWebKit/537.36 (KHTML, like Gecko) Mobile Safari/537.36 (compatible; Bytespider; spider-feedback@bytedance.com)"
	uaMeta       = "meta-externalagent/1.1 (+https://developers.facebook.com/docs/sharing/webmasters/crawler)"
)

var crawlerLists = map[string]string{
	"/gptbot.json": `{"creationTime": "2026-09-22T02:00:07.000000", "prefixes": [{"ipv4Prefix": "20.171.206.0/24"}]}`,
	"/claude.json": `{"creationTime": "2026-08-18T23:56:36Z", "prefixes": [{"ipv4Prefix": "160.79.104.0/23"}]}`,
	"/pplx.json":   `{"prefixes": [{"ipv4Prefix": "18.97.1.228/30"}, {"ipv6Prefix": "2600:1f28:365:8000::/56"}]}`,
}

// crawlerConf's lines are named in the tests' notes: block training is
// line 7, allow GPTBot 8, the limit 9 and the section's block 12.
const crawlerConf = `ranges GPTBot LISTS/gptbot.json
ranges ClaudeBot LISTS/claude.json
ranges PerplexityBot LISTS/pplx.json
ranges Bytespider bytespider.txt
forwarded-from 127.0.0.1
report /.crawlers
block training
allow GPTBot
limit PerplexityBot 3 per 1m

path /members/
  block all
`

// crawlerServer starts BareProxy with the crawlers plugin in two
// instances, with a store, fetching its lists from ls (LISTS in conf), a
// folder of files for ranges lines, a route chosen by the bot header, and
// an app that records the headers it gets.
func crawlerServer(t *testing.T, conf string, ls *listServer, files map[string]string) (*liveServer, *http.Header) {
	t.Helper()
	l := &liveServer{dir: t.TempDir(), port: freePort(t)}
	l.conf = filepath.Join(l.dir, "bareproxy.conf")
	writeFile(t, filepath.Join(l.dir, "lists", "README"), "files for ranges lines\n")
	for name, body := range files {
		writeFile(t, filepath.Join(l.dir, "lists", name), body)
	}
	writeFile(t, filepath.Join(l.dir, "crawlers.wasm"), string(shipped(t, "crawlers")))
	writeFile(t, filepath.Join(l.dir, "crawlers.conf"), strings.ReplaceAll(conf, "LISTS", ls.srv.URL))
	seen := &http.Header{}
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.Header.Clone()
		fmt.Fprintln(w, "from the app")
	}))
	t.Cleanup(b.Close)
	writeFile(t, l.conf, fmt.Sprintf(`global
  admin off
  trace-log off
  state state

plugin p crawlers.wasm
  config crawlers.conf
  read lists
  allow-http %s
  instances 2
  store 1MB

site http://example.com:%d
  use p
  route /special header X-Bot=GPTBot -> respond 200 "hello GPTBot"
  route /* -> app

pool app
  backend %s
`, strings.TrimPrefix(ls.srv.URL, "http://"), l.port, strings.TrimPrefix(b.URL, "http://")))
	s, err := Start(l.conf)
	if err != nil {
		t.Fatal(err)
	}
	l.s = s
	t.Cleanup(s.Stop)
	return l, seen
}

// crawlersReady waits until both instances' reports show every want.
func crawlersReady(t *testing.T, l *liveServer, want ...string) {
	t.Helper()
	var body string
	ready := func() bool {
		for range 2 {
			_, body, _ = send(t, l, "GET", "/.crawlers")
			for _, w := range want {
				if !strings.Contains(body, w) {
					return false
				}
			}
		}
		return true
	}
	for deadline := time.Now().Add(8 * time.Second); !ready(); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the lists didn't load; the report says:\n%s", body)
		}
	}
}

func TestCrawlersVerifiedFakeAndRules(t *testing.T) {
	ls := newListServer(t, crawlerLists)
	l, seen := crawlerServer(t, crawlerConf, ls, map[string]string{"bytespider.txt": "# from our logs\n111.225.148.0/22\n"})
	crawlersReady(t, l, "(GPTBot): 1 range, fetched", "(ClaudeBot): 1 range, fetched", "(PerplexityBot): 2 ranges, fetched")

	// GPTBot from its own addresses: let in, named for the app and the routes.
	resp, body, rec := send(t, l, "GET", "/blog/post", "User-Agent", uaGPTBot, "X-Forwarded-For", "20.171.206.5")
	if resp.StatusCode != 200 || body != "from the app\n" || seen.Get("X-Bot") != "GPTBot" || seen.Get("X-Bot-Check") != "verified" ||
		!strings.Contains(notes(rec), "GPTBot (OpenAI, training): 20.171.206.5 is in the list from 127.0.0.1; let in by line 8, its 1st request let in this hour") {
		t.Errorf("GPTBot: %d %q %v %+v", resp.StatusCode, body, *seen, rec.Plugins)
	}
	if _, body, _ := send(t, l, "GET", "/special", "User-Agent", uaGPTBot, "X-Forwarded-For", "20.171.206.9"); body != "hello GPTBot\n" {
		t.Errorf("route by the bot header: %q", body)
	}

	// The promise: the same user agent from elsewhere isn't GPTBot.
	resp, body, rec = send(t, l, "GET", "/blog/post", "User-Agent", uaGPTBot, "X-Forwarded-For", "203.0.113.9")
	if resp.StatusCode != 403 || body != "Forbidden.\n" ||
		!strings.Contains(notes(rec), "fake GPTBot: 203.0.113.9 isn't in the list from 127.0.0.1 (1 range); turned away (fake block)") {
		t.Errorf("fake: %d %q %+v", resp.StatusCode, body, rec.Plugins)
	}

	for _, c := range []struct {
		ua, addr, path string
		code           int
		body, note     string
	}{
		// A training bot, blocked by its group, verified or not.
		{uaClaudeBot, "160.79.105.20", "/", 403, "This site doesn't allow ClaudeBot.\n",
			"ClaudeBot (Anthropic, training): 160.79.105.20 is in the list from 127.0.0.1; turned away by line 7"},
		{uaMeta, "157.240.1.1", "/", 403, "This site doesn't allow Meta-ExternalAgent.\n",
			"Meta-ExternalAgent (Meta, training): 157.240.1.1 not verified, Meta publishes no address list; turned away by line 7"},
		// Addresses from a file in a read folder.
		{uaBytespider, "111.225.149.3", "/", 403, "This site doesn't allow Bytespider.\n", "111.225.149.3 is in the list in bytespider.txt"},
		{uaBytespider, "198.51.100.7", "/", 403, "Forbidden.\n", "fake Bytespider: 198.51.100.7 isn't in the list in bytespider.txt (1 range)"},
		// The path section turns away every bot, GPTBot too.
		{uaGPTBot, "20.171.206.5", "/members/page", 403, "This site doesn't allow GPTBot.\n", "turned away by line 12"},
		// robots.txt is open to all, so a turned-away bot can read why.
		{uaClaudeBot, "160.79.105.20", "/robots.txt", 200, "from the app\n", "/robots.txt is open to every bot"},
	} {
		resp, body, rec := send(t, l, "GET", c.path, "User-Agent", c.ua, "X-Forwarded-For", c.addr)
		if resp.StatusCode != c.code || body != c.body || !strings.Contains(notes(rec), c.note) {
			t.Errorf("%.30s from %s: %d %q %+v", c.ua, c.addr, resp.StatusCode, body, rec.Plugins)
		}
	}

	// A visitor can't send the bot headers to look like GPTBot, to the
	// app or to the routes.
	send(t, l, "GET", "/x", "X-Bot", "GPTBot", "X-Bot-Check", "verified")
	if seen.Get("X-Bot") != "" || seen.Get("X-Bot-Check") != "" {
		t.Errorf("a visitor's own bot headers reached the app: %v", *seen)
	}
	if _, body, rec := send(t, l, "GET", "/special", "X-Bot", "GPTBot"); body != "from the app\n" || notes(rec) != "" {
		t.Errorf("a visitor's X-Bot chose a route: %q %+v", body, rec.Plugins)
	}

	m := l.s.Current().Plugins["p"].Metrics()
	if m["bots"] != 8 || m["fakes"] != 2 || m["blocked"] != 4 {
		t.Errorf("metrics %v", m)
	}
}

func TestCrawlersLimitAndReport(t *testing.T) {
	ls := newListServer(t, crawlerLists)
	l, _ := crawlerServer(t, crawlerConf, ls, map[string]string{"bytespider.txt": "111.225.148.0/22\n"})
	crawlersReady(t, l, "(PerplexityBot): 2 ranges, fetched")
	for i := 1; i <= 3; i++ {
		if resp, _, _ := send(t, l, "GET", fmt.Sprintf("/docs/%d", i), "User-Agent", uaPerplexity, "X-Forwarded-For", "2600:1f28:365:8000::5"); resp.StatusCode != 200 {
			t.Fatalf("request %d: %d", i, resp.StatusCode)
		}
	}
	// The fourth, on either instance, is over: the count is BareProxy's.
	resp, body, rec := send(t, l, "GET", "/docs/4", "User-Agent", uaPerplexity, "X-Forwarded-For", "18.97.1.229")
	if resp.StatusCode != 429 || body != "Too many requests from PerplexityBot. Please slow down.\n" ||
		!strings.Contains(notes(rec), "PerplexityBot (Perplexity, search): 18.97.1.229 is in the list from 127.0.0.1; 4 requests in the last 1m, over 3 (line 9); the window resets in") {
		t.Fatalf("4th: %d %q %+v", resp.StatusCode, body, rec.Plugins)
	}
	if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err != nil || n < 1 || n > 60 {
		t.Errorf("Retry-After %q", resp.Header.Get("Retry-After"))
	}
	for range 2 {
		send(t, l, "GET", "/blog/a", "User-Agent", uaGPTBot, "X-Forwarded-For", "20.171.206.5")
	}
	send(t, l, "GET", "/", "User-Agent", uaGPTBot, "X-Forwarded-For", "20.171.206.5")
	send(t, l, "GET", "/", "User-Agent", uaGPTBot, "X-Forwarded-For", "192.0.2.1")
	send(t, l, "GET", "/", "User-Agent", uaClaudeBot, "X-Forwarded-For", "160.79.104.1")

	// Both instances' reads come together in the report within 5 seconds.
	time.Sleep(5500 * time.Millisecond)
	for range 2 {
		_, body, rec := send(t, l, "GET", "/.crawlers")
		for _, want := range []string{
			"BareProxy AI crawler report for example.com, ",
			"GPTBot\ttraining\t3\t0\t0\t1\t\t0\t0\t0\t0\n",
			"ClaudeBot\ttraining\t0\t0\t1\t0\t\t0\t0\t0\t0\n",
			"PerplexityBot\tsearch\t3\t1\t0\t0\t\t0\t0\t0\t0\n",
			"GPTBot\t2 /blog/, 1 /\n",
			"PerplexityBot\t3 /docs/\n",
			"No list, so their claims can't be checked: Google-GeminiNotebook, MistralAI-Training, Meta-ExternalAgent,",
			"OpenAI's list (OAI-SearchBot): not loaded yet; last try: openai.com:443 isn't on the plugin's allow-http line.",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("missing %q in the report:\n%s", want, body)
			}
		}
		if !strings.Contains(notes(rec), "the crawler report went out") {
			t.Errorf("report record %+v", rec.Plugins)
		}
	}
	// To an address report-allow doesn't name, the path is an ordinary one.
	if _, body, _ := send(t, l, "GET", "/.crawlers", "X-Forwarded-For", "203.0.113.9"); body != "from the app\n" {
		t.Errorf("the report went to an outside address: %q", body)
	}
}

// Two instances fetch each list once between them; a restart takes the
// lists from the store, so claims are checked from the first request.
func TestCrawlersListsSharedAndKept(t *testing.T) {
	ls := newListServer(t, crawlerLists)
	l, _ := crawlerServer(t, crawlerConf, ls, map[string]string{"bytespider.txt": "111.225.148.0/22\n"})
	crawlersReady(t, l, "(GPTBot): 1 range, fetched", "(ClaudeBot): 1 range, fetched", "(PerplexityBot): 2 ranges, fetched")
	time.Sleep(2 * time.Second)
	for _, p := range []string{"/gptbot.json", "/claude.json", "/pplx.json"} {
		if n := ls.count(p); n != 1 {
			t.Errorf("%s fetched %d times", p, n)
		}
	}
	l.s.Stop()
	s, err := Start(l.conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	l.s = s
	resp, _, rec := send(t, l, "GET", "/", "User-Agent", uaGPTBot, "X-Forwarded-For", "20.171.206.5")
	if resp.StatusCode != 200 || !strings.Contains(notes(rec), "is in the list from 127.0.0.1") {
		t.Errorf("after a restart: %d %+v", resp.StatusCode, rec.Plugins)
	}
	if resp, _, _ := send(t, l, "GET", "/", "User-Agent", uaGPTBot, "X-Forwarded-For", "203.0.113.9"); resp.StatusCode != 403 {
		t.Errorf("a fake after a restart: %d", resp.StatusCode)
	}
	time.Sleep(1500 * time.Millisecond)
	if n := ls.count("/gptbot.json"); n != 1 {
		t.Errorf("fetched again after a restart: %d times", n)
	}
}

// A list that fails to load leaves its bots unverified, and the report
// says why; a redirect on the list's own host is followed; with
// unverified fake, claims that can't be checked are turned away.
func TestCrawlersListFailures(t *testing.T) {
	ls := newListServer(t, map[string]string{
		"/gptbot.json": "status 503",
		"/claude.json": `{"prefixes": []}`,
		"/old.json":    "redirect /pplx.json",
		"/pplx.json":   crawlerLists["/pplx.json"],
	})
	conf := strings.Replace(crawlerConf, "LISTS/pplx.json", "LISTS/old.json", 1)
	l, _ := crawlerServer(t, conf, ls, map[string]string{"bytespider.txt": "111.225.148.0/22\n"})
	crawlersReady(t, l, "(PerplexityBot): 2 ranges, fetched",
		"(GPTBot): not loaded yet; last try: answered 503.",
		"(ClaudeBot): not loaded yet; last try: the list has no address ranges in it.")
	resp, _, rec := send(t, l, "GET", "/", "User-Agent", uaGPTBot, "X-Forwarded-For", "203.0.113.9")
	if resp.StatusCode != 200 || !strings.Contains(notes(rec), "203.0.113.9 not verified, the list from 127.0.0.1 isn't loaded yet; let in by line 8") {
		t.Errorf("unverified GPTBot: %d %+v", resp.StatusCode, rec.Plugins)
	}

	strict := strings.Replace(crawlerConf, "report /.crawlers\n", "report /.crawlers\nunverified fake\n", 1)
	ls2 := newListServer(t, map[string]string{"/gptbot.json": "status 503"})
	l2, _ := crawlerServer(t, strict, ls2, map[string]string{"bytespider.txt": "111.225.148.0/22\n"})
	resp, _, rec = send(t, l2, "GET", "/", "User-Agent", uaGPTBot, "X-Forwarded-For", "20.171.206.5")
	if resp.StatusCode != 403 || !strings.Contains(notes(rec), "fake GPTBot: 20.171.206.5 not verified, the list from 127.0.0.1 isn't loaded yet, and claims that can't be checked count as fakes; turned away") {
		t.Errorf("unverified fake: %d %+v", resp.StatusCode, rec.Plugins)
	}
}

func TestCrawlersConfigRefused(t *testing.T) {
	configRefused(t, "crawlers", "allow GPTBot\nblock gptbot\n", "line 2: gptbot has a rule on line 1 already")
	configRefused(t, "crawlers", "limit GPTBot 30 every 1m\n", "line 1: write limit NAME... COUNT per TIME, such as limit PerplexityBot 30 per 1m")
	configRefused(t, "crawlers", "ranges Bytespider bytespider.txt\n",
		"can't read bytespider.txt: it isn't in a folder the plugin line names with read")
}

const uaBrowser = "Mozilla/5.0 (X11; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0"

// challengeServer starts BareProxy with the challenges plugin, believing
// X-Forwarded-For from 127.0.0.1, with more lines for the plugin block.
func challengeServer(t *testing.T, conf, lines string, files map[string]string) *liveServer {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "README"), "files for always lines\n")
	for name, body := range files {
		writeFile(t, filepath.Join(dir, name), body)
	}
	return mustShippedServer2(t, "challenges", "forwarded-from 127.0.0.1\n"+conf, "  read "+dir+"\n"+lines)
}

var challengeData = regexp.MustCompile(`<script type="application/json" id="bp-challenge">([^<]*)</script>`)

// solveChallenge reads the puzzle from a challenge page and answers it as
// the page's script does: the first n whose SHA-256 of c:n starts with w
// zero bits. It returns the query for the endpoint.
func solveChallenge(t *testing.T, page string) (endpoint, query string) {
	t.Helper()
	m := challengeData.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no puzzle in the page:\n%s", page)
	}
	var d struct {
		C string `json:"c"`
		T int64  `json:"t"`
		W int    `json:"w"`
		R string `json:"r"`
		E string `json:"e"`
	}
	if err := json.Unmarshal([]byte(m[1]), &d); err != nil {
		t.Fatal(err)
	}
	for n := 0; ; n++ {
		h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", d.C, n)))
		z := 0
		for _, b := range h {
			if b == 0 {
				z += 8
				continue
			}
			for b&0x80 == 0 {
				z++
				b <<= 1
			}
			break
		}
		if z >= d.W {
			return d.E, fmt.Sprintf("t=%d&w=%d&n=%d&r=%s", d.T, d.W, n, url.QueryEscape(d.R))
		}
	}
}

func TestChallengesBrowserSolvesAndPasses(t *testing.T) {
	l := challengeServer(t, "work 12\n", "  store 1MB\n", nil)
	resp, body, rec := send(t, l, "GET", "/page.txt?x=1", "User-Agent", uaBrowser, "X-Forwarded-For", "203.0.113.5")
	if resp.StatusCode != 403 || !strings.Contains(body, "One moment") || resp.Header.Get("Cache-Control") != "no-store" ||
		resp.Header.Get("X-Robots-Tag") != "noindex" || !strings.Contains(notes(rec), "challenged: a browser with no pass; work 12") {
		t.Fatalf("no challenge: %d %v %+v", resp.StatusCode, resp.Header, rec.Plugins)
	}
	ep, q := solveChallenge(t, body)
	if ep != "/.bareproxy-challenge" {
		t.Errorf("endpoint %q", ep)
	}
	resp, _, rec = send(t, l, "GET", ep+"?"+q, "User-Agent", uaBrowser, "X-Forwarded-For", "203.0.113.5")
	cookie := resp.Header.Get("Set-Cookie")
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "/page.txt?x=1" || !strings.HasPrefix(cookie, "bp_pass=") ||
		!strings.Contains(cookie, "; Path=/; Max-Age=604800; HttpOnly; SameSite=Lax") || strings.Contains(cookie, "Secure") ||
		!strings.Contains(notes(rec), "challenge passed: work 12, pass until ") {
		t.Fatalf("answer: %d %v %+v", resp.StatusCode, resp.Header, rec.Plugins)
	}
	pass := strings.SplitN(cookie, ";", 2)[0]
	resp, body, rec = send(t, l, "GET", "/page.txt", "User-Agent", uaBrowser, "X-Forwarded-For", "203.0.113.77", "Cookie", "a=1; "+pass)
	if resp.StatusCode != 200 || body != "a static page\n" || notes(rec) != "" {
		t.Errorf("with the pass, from the same network: %d %q %+v", resp.StatusCode, body, rec.Plugins)
	}
	// A pass is for its network and its browser.
	for _, hdr := range [][]string{
		{"User-Agent", uaBrowser, "X-Forwarded-For", "198.51.100.5", "Cookie", pass},
		{"User-Agent", uaBrowser + " Edge/1", "X-Forwarded-For", "203.0.113.5", "Cookie", pass},
		{"User-Agent", uaBrowser, "X-Forwarded-For", "203.0.113.5", "Cookie", pass + "0"},
	} {
		if resp, _, _ := send(t, l, "GET", "/page.txt", hdr...); resp.StatusCode != 403 {
			t.Errorf("the pass worked with %v: %d", hdr, resp.StatusCode)
		}
	}
	// A wrong answer, and an answer for another network.
	wrong := strings.Replace(q, "&n=", "&n=9", 1)
	resp, body, rec = send(t, l, "GET", ep+"?"+wrong, "User-Agent", uaBrowser, "X-Forwarded-For", "203.0.113.5")
	if resp.StatusCode != 403 || !strings.Contains(body, `<a href="/page.txt?x=1">Go back to the page</a>`) ||
		!strings.Contains(notes(rec), "challenge failed: the answer doesn't solve the puzzle") {
		t.Errorf("wrong answer: %d %q %+v", resp.StatusCode, body, rec.Plugins)
	}
	if resp, _, _ := send(t, l, "GET", ep+"?"+q, "User-Agent", uaBrowser, "X-Forwarded-For", "198.51.100.5"); resp.StatusCode != 403 {
		t.Errorf("an answer from another network passed: %d", resp.StatusCode)
	}
	// The endpoint never sends anyone off the site.
	evil := strings.Replace(q, "&r=", "&r=%2F%2Fevil.example%2F&x=", 1)
	if resp, _, _ := send(t, l, "GET", ep+"?"+evil, "User-Agent", uaBrowser, "X-Forwarded-For", "203.0.113.5"); resp.Header.Get("Location") != "/" {
		t.Errorf("the endpoint sent a browser to %q", resp.Header.Get("Location"))
	}
	if m := l.s.Current().Plugins["p"].Metrics(); m["challenged"] != 4 || m["passed"] != 2 || m["failed"] != 2 {
		t.Errorf("metrics %v", m)
	}
}

func TestChallengesWhoGetsOne(t *testing.T) {
	l := challengeServer(t, "allow 192.0.2.0/24\nskip /feed.xml\ntrust-header X-Bot-Check=verified\n", "", nil)
	writeFile(t, filepath.Join(l.dir, "public", "feed.xml"), "<rss/>\n")
	for _, c := range []struct {
		method, path string
		hdr          []string
		code         int
		note         string
	}{
		{"GET", "/page.txt", []string{"User-Agent", "curl/8.5.0"}, 200, ""},
		{"GET", "/page.txt", []string{"User-Agent", "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"}, 200, ""},
		{"GET", "/page.txt", nil, 200, ""},
		{"GET", "/robots.txt", []string{"User-Agent", uaBrowser}, 404, ""},
		{"GET", "/feed.xml", []string{"User-Agent", uaBrowser}, 200, ""},
		{"POST", "/page.txt", []string{"User-Agent", uaBrowser}, 405, ""},
		{"GET", "/page.txt", []string{"User-Agent", uaBrowser, "X-Forwarded-For", "192.0.2.9"}, 200, ""},
		{"GET", "/page.txt", []string{"User-Agent", uaBrowser, "X-Bot-Check", "verified"}, 200, "no challenge: x-bot-check is verified (trust-header)"},
		{"GET", "/page.txt", []string{"User-Agent", uaBrowser, "X-Bot-Check", "fake"}, 403, "challenged: a browser with no pass"},
		{"HEAD", "/page.txt", []string{"User-Agent", uaBrowser}, 403, "challenged"},
	} {
		resp, _, rec := send(t, l, c.method, c.path, c.hdr...)
		if resp.StatusCode != c.code || (c.note == "" && notes(rec) != "") || !strings.Contains(notes(rec), c.note) {
			t.Errorf("%s %s %v: %d %+v", c.method, c.path, c.hdr, resp.StatusCode, rec.Plugins)
		}
	}
	// challenge all: every client without a pass, bots and tools too.
	all := challengeServer(t, "challenge all\nstatus 429\n", "", nil)
	if resp, _, rec := send(t, all, "GET", "/page.txt", "User-Agent", "curl/8.5.0"); resp.StatusCode != 429 ||
		!strings.Contains(notes(rec), "challenged: no pass (challenge all)") {
		t.Errorf("challenge all: %d %+v", resp.StatusCode, rec.Plugins)
	}
}

// With after, a client loads a few pages before the challenge; images
// and scripts don't count. Addresses on the always list get it at once.
func TestChallengesAfterAndAlways(t *testing.T) {
	l := challengeServer(t, "after 3 per 1m\nalways 198.51.100.0/24 hosting.txt\n", "  instances 2\n",
		map[string]string{"hosting.txt": "# a hosting provider\n100.24.0.0/16\n"})
	page := []string{"User-Agent", uaBrowser, "X-Forwarded-For", "203.0.113.5", "Accept", "text/html,application/xhtml+xml,*/*;q=0.8"}
	image := []string{"User-Agent", uaBrowser, "X-Forwarded-For", "203.0.113.5", "Accept", "image/avif,image/webp,*/*", "Sec-Fetch-Mode", "no-cors"}
	for i := 1; i <= 3; i++ {
		if resp, _, _ := send(t, l, "GET", "/page.txt", page...); resp.StatusCode != 200 {
			t.Fatalf("page load %d: %d", i, resp.StatusCode)
		}
		if resp, _, _ := send(t, l, "GET", "/page.txt", image...); resp.StatusCode != 200 {
			t.Fatalf("image %d: %d", i, resp.StatusCode)
		}
	}
	resp, _, rec := send(t, l, "GET", "/page.txt", page...)
	if resp.StatusCode != 403 || !strings.Contains(notes(rec), "challenged: 4 page loads in the last 1m from 203.0.113.5/32 without a pass, over 3 (line 2); work 16") {
		t.Errorf("the 4th page load: %d %+v", resp.StatusCode, rec.Plugins)
	}
	// Over the count, an image is challenged too, until the pass.
	if resp, _, _ := send(t, l, "GET", "/page.txt", image...); resp.StatusCode != 403 {
		t.Errorf("an image over the count: %d", resp.StatusCode)
	}
	if resp, _, _ := send(t, l, "GET", "/page.txt", "User-Agent", uaBrowser, "X-Forwarded-For", "203.0.113.6"); resp.StatusCode != 200 {
		t.Errorf("another address shared the count: %d", resp.StatusCode)
	}
	for _, addr := range []string{"198.51.100.20", "100.24.7.9"} {
		resp, _, rec := send(t, l, "GET", "/page.txt", "User-Agent", uaBrowser, "X-Forwarded-For", addr)
		if resp.StatusCode != 403 || !strings.Contains(notes(rec), "challenged: "+addr+" is on the always list") {
			t.Errorf("always %s: %d %+v", addr, resp.StatusCode, rec.Plugins)
		}
	}
}

// The key the plugin makes is shared by its instances and kept in its
// store, so a pass works on every instance and after a restart.
func TestChallengesKeyKept(t *testing.T) {
	l := challengeServer(t, "work 10\n", "  instances 2\n  store 1MB\n", nil)
	hdr := []string{"User-Agent", uaBrowser, "X-Forwarded-For", "203.0.113.5"}
	_, body, _ := send(t, l, "GET", "/page.txt", hdr...)
	ep, q := solveChallenge(t, body)
	resp, _, _ := send(t, l, "GET", ep+"?"+q, hdr...)
	pass := strings.SplitN(resp.Header.Get("Set-Cookie"), ";", 2)[0]
	withPass := append(slices.Clone(hdr), "Cookie", pass)
	for i := range 4 { // both instances
		if resp, _, _ := send(t, l, "GET", "/page.txt", withPass...); resp.StatusCode != 200 {
			t.Fatalf("request %d with the pass: %d", i, resp.StatusCode)
		}
	}
	l.s.Stop()
	s, err := Start(l.conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	l.s = s
	if resp, _, _ := send(t, l, "GET", "/page.txt", withPass...); resp.StatusCode != 200 {
		t.Errorf("the pass after a restart: %d", resp.StatusCode)
	}
}

func TestChallengesConfigRefused(t *testing.T) {
	configRefused(t, "challenges", "work 30\n", "line 1: work takes a number of bits from 8 to 24")
	configRefused(t, "challenges", "always hosting.txt\n", "can't read hosting.txt: it isn't in a folder the plugin line names with read")
}

// markdownServer starts BareProxy with the Markdown plugin reading the
// site's own folder, public/, which files fill, with more lines for the
// plugin block. /api/* answers "api" itself.
func markdownServer(t *testing.T, conf, lines string, files map[string]string) *liveServer {
	t.Helper()
	l := &liveServer{dir: t.TempDir(), port: freePort(t)}
	l.conf = filepath.Join(l.dir, "bareproxy.conf")
	for name, body := range files {
		writeFile(t, filepath.Join(l.dir, "public", name), body)
	}
	writeFile(t, filepath.Join(l.dir, "p.wasm"), string(shipped(t, "markdown")))
	writeFile(t, filepath.Join(l.dir, "p.conf"), conf)
	writeFile(t, l.conf, fmt.Sprintf(`global
  admin off
  trace-log off
  state state

plugin p p.wasm
  config p.conf
  read public
%s
site http://example.com:%d
  use p
  route /api/* -> respond 200 api
  route /* -> files public
`, lines, l.port))
	s, err := Start(l.conf)
	if err != nil {
		t.Fatal(err)
	}
	l.s = s
	t.Cleanup(s.Stop)
	return l
}

// rewrite changes a file the plugin reads so its size and time of change
// are new, as an editor saving it would.
func rewrite(t *testing.T, l *liveServer, name, body string) {
	t.Helper()
	p := filepath.Join(l.dir, "public", name)
	writeFile(t, p, body)
	later := time.Now().Add(2 * time.Second)
	os.Chtimes(p, later, later)
}

var markdownSite = map[string]string{
	"index.md":        "# Docs\n\nWelcome to the docs.\n\n- [Setup](guide/setup.md)\n- [Notes](notes/README.md)\n",
	"guide/index.md":  "# Guide\n\nStart with [setup](setup.md#install).\n",
	"guide/setup.md":  "---\ntitle: Setting up\nauthor: Ann\n---\n# Setup\n\nInstall it.\n\n## Install\n\nRun it.\n\n## Check\n\n[Back](index.md)\n",
	"notes/README.md": "Notes, with no heading.\n",
	"style.css":       "body{}\n",
	"café menu.md":    "# Café\n",
}

func TestMarkdownPagesRenderAndKeep(t *testing.T) {
	l := markdownServer(t, "", "  instances 1\n", markdownSite)
	resp, body, rec := send(t, l, "GET", "/")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/html; charset=utf-8" ||
		!strings.Contains(body, "<title>Docs</title>") || !strings.Contains(body, `<h1 id="docs">Docs</h1>`) ||
		!strings.Contains(body, `<a href="guide/setup">Setup</a>`) || !strings.Contains(body, `<a href="notes/">Notes</a>`) ||
		!strings.Contains(body, `<meta name="description" content="Welcome to the docs.">`) ||
		resp.Header.Get("Vary") != "Accept" || resp.Header.Get("Link") != `</index.md>; rel="alternate"; type="text/markdown"` ||
		rec.Plugins[0].Action != "answered 200" || !strings.Contains(notes(rec), "rendered index.md (") {
		t.Fatalf("front page: %d %v %+v\n%s", resp.StatusCode, resp.Header, rec.Plugins, body)
	}
	etag := resp.Header.Get("ETag")
	resp, again, rec := send(t, l, "GET", "/")
	if again != body || resp.Header.Get("ETag") != etag || notes(rec) != "index.md as rendered before" {
		t.Errorf("again: %q, %q", resp.Header.Get("ETag"), notes(rec))
	}

	resp, body, _ = send(t, l, "GET", "/guide/setup")
	if !strings.Contains(body, "<title>Setting up</title>") || !strings.Contains(body, `<h2 id="install">Install</h2>`) ||
		!strings.Contains(body, `<a href="./">Back</a>`) || strings.Contains(body, "author: Ann") ||
		resp.Header.Get("Link") != `</guide/setup.md>; rel="alternate"; type="text/markdown"` {
		t.Errorf("a page with front matter:\n%s", body)
	}
	if _, body, _ = send(t, l, "GET", "/guide/"); !strings.Contains(body, `<a href="setup#install">setup</a>`) {
		t.Errorf("a folder's index.md:\n%s", body)
	}
	if _, body, _ = send(t, l, "GET", "/notes/"); !strings.Contains(body, "<title>Notes</title>") ||
		!strings.Contains(body, "<p>Notes, with no heading.</p>") {
		t.Errorf("a folder's README.md, titled by its folder:\n%s", body)
	}
	if resp, body, _ := send(t, l, "GET", "/guide/?utm=x"); resp.StatusCode != 200 || !strings.Contains(body, "<title>Guide</title>") {
		t.Errorf("a query: %d", resp.StatusCode)
	}
	if resp, body, _ := send(t, l, "GET", "/caf%C3%A9%20menu"); !strings.Contains(body, "<title>Café</title>") ||
		resp.Header.Get("Link") != `</caf%C3%A9%20menu.md>; rel="alternate"; type="text/markdown"` {
		t.Errorf("an encoded name: %d %v", resp.StatusCode, resp.Header)
	}
	// A folder without its slash: the files route sends it to the slash.
	if resp, _, rec := send(t, l, "GET", "/guide"); resp.StatusCode != 301 || resp.Header.Get("Location") != "/guide/" ||
		notes(rec) != "no guide.md in the folders the plugin line names with read, so the request went on" {
		t.Errorf("/guide: %d %v %q", resp.StatusCode, resp.Header, notes(rec))
	}
	// The Markdown itself, other files and other methods are the route's.
	if resp, body, rec := send(t, l, "GET", "/guide/setup.md"); resp.StatusCode != 200 ||
		resp.Header.Get("Content-Type") != "text/markdown; charset=utf-8" || body != markdownSite["guide/setup.md"] || notes(rec) != "" {
		t.Errorf("the .md file: %d %v %q", resp.StatusCode, resp.Header, notes(rec))
	}
	if resp, _, rec := send(t, l, "GET", "/style.css"); resp.StatusCode != 200 || notes(rec) != "" {
		t.Errorf("style.css: %d %q", resp.StatusCode, notes(rec))
	}
	if resp, _, rec := send(t, l, "POST", "/guide/setup"); resp.StatusCode != 405 || rec.Plugins[0].Action != "went on" {
		t.Errorf("POST: %d %+v", resp.StatusCode, rec.Plugins)
	}
	if resp, body, _ := send(t, l, "GET", "/api/x"); body != "api\n" {
		t.Errorf("another route: %d %q", resp.StatusCode, body)
	}
	if resp, _, rec := send(t, l, "GET", "/missing"); resp.StatusCode != 404 || !strings.Contains(notes(rec), "no missing.md") {
		t.Errorf("no file: %d %q", resp.StatusCode, notes(rec))
	}
	if resp, _, _ := send(t, l, "GET", "/.hidden"); resp.StatusCode != 404 {
		t.Errorf("hidden name: %d", resp.StatusCode)
	}
	// HEAD gets the headers alone, with the page's length.
	get, getBody, _ := send(t, l, "GET", "/guide/setup")
	head, headBody, _ := send(t, l, "HEAD", "/guide/setup")
	if head.StatusCode != 200 || headBody != "" || head.Header.Get("Content-Length") != strconv.Itoa(len(getBody)) ||
		head.Header.Get("ETag") != get.Header.Get("ETag") {
		t.Errorf("HEAD: %d %v (GET was %d bytes)", head.StatusCode, head.Header, len(getBody))
	}

	// A changed file is rendered again on the next request.
	rewrite(t, l, "guide/setup.md", "# Setup, again\n\nNew words.\n")
	resp, body, rec = send(t, l, "GET", "/guide/setup")
	if !strings.Contains(body, "<title>Setup, again</title>") || !strings.Contains(notes(rec), "rendered guide/setup.md") ||
		resp.Header.Get("ETag") == get.Header.Get("ETag") {
		t.Errorf("after a change: %q\n%s", notes(rec), body)
	}
	m := l.s.Current().Plugins["p"].Metrics()
	if m["rendered"] != 6 || m["pages"] != 10 || m["markdown"] != 0 {
		t.Errorf("metrics %v", m)
	}
}

func TestMarkdownConditionsAndNegotiation(t *testing.T) {
	l := markdownServer(t, "", "", markdownSite)
	resp, _, _ := send(t, l, "GET", "/guide/setup")
	etag := resp.Header.Get("ETag")
	// The ETag comes from the file's size and time of change, so every
	// instance gives the same one, and none has to read the file for a 304.
	for range 4 {
		resp, body, rec := send(t, l, "GET", "/guide/setup", "If-None-Match", etag)
		if resp.StatusCode != 304 || body != "" || resp.Header.Get("ETag") != etag ||
			notes(rec) != "guide/setup.md hasn't changed since the client's copy (ETag)" {
			t.Fatalf("If-None-Match: %d %v %q", resp.StatusCode, resp.Header, notes(rec))
		}
	}
	// A client that asks for Markdown gets it as written, at the page's URL.
	resp, body, rec := send(t, l, "GET", "/guide/setup", "Accept", "text/markdown, text/html;q=0.9")
	mdTag := resp.Header.Get("ETag")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/markdown; charset=utf-8" ||
		body != markdownSite["guide/setup.md"] || resp.Header.Get("Vary") != "Accept" || mdTag == etag ||
		!strings.HasSuffix(notes(rec), "sent the Markdown, as the client asked for text/markdown") {
		t.Errorf("Markdown: %d %v %q\n%s", resp.StatusCode, resp.Header, notes(rec), body)
	}
	if resp, _, _ := send(t, l, "GET", "/guide/setup", "Accept", "text/markdown", "If-None-Match", mdTag); resp.StatusCode != 304 {
		t.Errorf("Markdown with its ETag: %d", resp.StatusCode)
	}
	browser := "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
	if resp, _, _ := send(t, l, "GET", "/guide/setup", "Accept", browser, "If-None-Match", mdTag); resp.StatusCode != 200 ||
		resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Errorf("a browser with the Markdown's ETag: %d %v", resp.StatusCode, resp.Header)
	}
	// A change makes the old ETag stale.
	rewrite(t, l, "guide/setup.md", "# Changed\n")
	if resp, _, _ := send(t, l, "GET", "/guide/setup", "If-None-Match", etag); resp.StatusCode != 200 {
		t.Errorf("a stale ETag: %d", resp.StatusCode)
	}
	if got := l.s.Current().Plugins["p"].Metrics()["markdown"]; got != 1 {
		t.Errorf("metric markdown = %d", got)
	}

	// With accept off, everyone gets HTML, and nothing varies.
	off := markdownServer(t, "accept off\n", "", markdownSite)
	resp, _, _ = send(t, off, "GET", "/guide/setup", "Accept", "text/markdown")
	if resp.Header.Get("Content-Type") != "text/html; charset=utf-8" || resp.Header.Get("Vary") != "" {
		t.Errorf("accept off: %v", resp.Header)
	}
}

func TestMarkdownTemplate(t *testing.T) {
	files := maps.Clone(markdownSite)
	files[".page.html"] = "<html lang=\"{{lang}}\"><title>{{title}} | Docs</title><nav>{{toc}}</nav><main>{{content}}</main><p>{{ author }}</p></html>\n"
	l := markdownServer(t, "template .page.html\nlang fr\n", "  instances 1\n", files)
	resp, body, _ := send(t, l, "GET", "/guide/setup")
	if !strings.HasPrefix(body, `<html lang="fr"><title>Setting up | Docs</title><nav><ul>`) ||
		!strings.Contains(body, `<li><a href="#install">Install</a></li>`) || !strings.HasSuffix(body, "<p>Ann</p></html>\n") {
		t.Errorf("the template:\n%s", body)
	}
	etag := resp.Header.Get("ETag")
	// The template is a hidden file, which the files route never serves.
	if resp, _, _ := send(t, l, "GET", "/.page.html"); resp.StatusCode != 404 {
		t.Errorf("the template file: %d", resp.StatusCode)
	}
	// A changed template counts at once, and changes the ETag.
	rewrite(t, l, ".page.html", "<main class=\"v2\">{{content}}</main>\n")
	resp, body, rec := send(t, l, "GET", "/guide/setup")
	if !strings.HasPrefix(body, `<main class="v2"><h1 id="setup">`) || resp.Header.Get("ETag") == etag ||
		!strings.Contains(notes(rec), "read the template .page.html again, as it changed") {
		t.Errorf("a changed template: %q\n%s", notes(rec), body)
	}
	// A broken one is refused, and the one before stays.
	rewrite(t, l, ".page.html", "<main>no place for the page</main>\n")
	_, body, rec = send(t, l, "GET", "/guide/setup")
	if !strings.HasPrefix(body, `<main class="v2">`) ||
		!strings.Contains(notes(rec), "the template .page.html has no {{content}}, the place the page goes, so the one before stays") {
		t.Errorf("a broken template: %q\n%s", notes(rec), body)
	}
	if _, _, rec = send(t, l, "GET", "/guide/setup"); strings.Contains(notes(rec), "template") {
		t.Errorf("the broken template was noted twice: %q", notes(rec))
	}
}

func TestMarkdownBigAndOddPages(t *testing.T) {
	var b strings.Builder
	b.WriteString("# A long page\n\n")
	for i := 0; b.Len() < 200<<10; i++ {
		fmt.Fprintf(&b, "## Part %d\n\nSome *words*, [a link](part.md), `code` and more words to fill a line.\n\n- one\n- two\n\n", i)
	}
	b.WriteString("## The end\n")
	l := markdownServer(t, "largest 512KB\n", "  instances 1\n", map[string]string{
		"long.md": b.String(),
		"huge.md": strings.Repeat("A line of text.\n", 40<<10),
		// Small, and slow: thousands of quotes inside quotes take longer to
		// render than a request's call may run, so pages render in timer calls.
		"nested.md": strings.Repeat(">", 5000) + " deep\n",
	})
	resp, body, rec := send(t, l, "GET", "/long")
	if resp.StatusCode != 200 || !strings.Contains(body, `<h2 id="the-end">The end</h2>`) || !strings.HasSuffix(body, "</html>\n") ||
		!strings.HasPrefix(notes(rec), "rendered long.md (201 KB) in ") {
		t.Fatalf("a long page: %d %q (%d bytes)", resp.StatusCode, notes(rec), len(body))
	}
	if _, again, rec := send(t, l, "GET", "/long"); again != body || notes(rec) != "long.md as rendered before" {
		t.Errorf("a long page again: %q", notes(rec))
	}
	if resp, body, rec := send(t, l, "GET", "/nested"); resp.StatusCode != 200 || strings.Count(body, "<blockquote>") != 5000 {
		t.Errorf("deep nesting: %d %q", resp.StatusCode, notes(rec))
	}
	// Past largest, a page isn't rendered at all.
	resp, body, rec = send(t, l, "GET", "/huge")
	if resp.StatusCode != 500 || body != "This page is too large to show.\n" ||
		notes(rec) != "huge.md is 640 KB, over largest 512 KB, so it wasn't rendered: raise largest (and memory on the plugin line)" {
		t.Errorf("a huge page: %d %q %q", resp.StatusCode, body, notes(rec))
	}
	if working, all := l.s.Current().Plugins["p"].Health(); working != all {
		t.Errorf("%d of %d instances working", working, all)
	}
}

// A file whose rendering takes its instance down (here, out of memory) is
// tried again only once it changes, so it can't keep the plugin down.
func TestMarkdownStoppedRenderNotRetried(t *testing.T) {
	l := markdownServer(t, "", "  instances 1\n  memory 8MB\n", map[string]string{
		"deep.md": strings.Repeat(">", 300<<10) + " deep\n",
		"ok.md":   "# Fine\n",
	})
	if resp, _, rec := send(t, l, "GET", "/deep"); resp.StatusCode != 502 || !strings.Contains(rec.Plugins[0].Error, "crashed") {
		t.Fatalf("the first try: %d %+v", resp.StatusCode, rec.Plugins)
	}
	time.Sleep(3200 * time.Millisecond)
	resp, body, rec := send(t, l, "GET", "/deep")
	if resp.StatusCode != 500 || body != "This page can't be shown.\n" ||
		notes(rec) != "rendering deep.md stopped part way before, out of time or memory, so it isn't tried again until the file changes" {
		t.Errorf("after it stopped: %d %q %q %+v", resp.StatusCode, body, notes(rec), rec.Plugins)
	}
	if resp, _, _ := send(t, l, "GET", "/ok"); resp.StatusCode != 200 {
		t.Errorf("another page: %d", resp.StatusCode)
	}
	rewrite(t, l, "deep.md", "> not so deep\n")
	if resp, body, _ := send(t, l, "GET", "/deep"); resp.StatusCode != 200 || !strings.Contains(body, "not so deep") {
		t.Errorf("once it changed: %d", resp.StatusCode)
	}
}

func TestMarkdownConfigRefused(t *testing.T) {
	configRefused(t, "markdown", "html maybe\n", "line 1: html takes on or off")
	configRefused(t, "markdown", "lang en\ntemplate page.html\n", "line 2: can't read page.html: it isn't in a folder the plugin line names with read")
}

const consentPage = `<!doctype html><html><head><title>Shop</title>
<script async src="https://www.googletagmanager.com/gtag/js?id=G-1"></script>
<script>window.dataLayer=[];function gtag(){dataLayer.push(arguments)}gtag('config','G-1')</script>
<script src="https://connect.facebook.net/en_US/fbevents.js"></script>
<script src="/app.js"></script>
</head><body><h1>Shop</h1><p>Welcome.</p></body></html>
`

// consentServer starts BareProxy with the consent plugin in front of an
// app that sets a session cookie, an analytics cookie and a marketing
// cookie on each page, and says in headers what it was sent: X-Saw-Cookie,
// X-Saw-Consent and X-Saw-Encoding. It gzips a page when it may.
func consentServer(t *testing.T, conf string) *liveServer {
	t.Helper()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Saw-Cookie", r.Header.Get("Cookie"))
		h.Set("X-Saw-Consent", r.Header.Get("X-Consent"))
		h.Set("X-Saw-Encoding", r.Header.Get("Accept-Encoding"))
		h.Set("X-Saw-Conditional", r.Header.Get("If-None-Match")+r.Header.Get("If-Modified-Since"))
		h.Add("Set-Cookie", "session=abc; Path=/; HttpOnly")
		h.Add("Set-Cookie", "_ga=GA1.1.42; Path=/; Max-Age=63072000")
		h.Add("Set-Cookie", "_fbp=fb.1.42; Path=/; Domain=example.com")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true}`)
			return
		}
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "public, max-age=60")
		h.Set("ETag", `"v1"`)
		h.Set("Last-Modified", "Wed, 14 Oct 2026 10:00:00 GMT")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			h.Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			gz.Write([]byte(consentPage))
			gz.Close()
			return
		}
		fmt.Fprint(w, consentPage)
	}))
	t.Cleanup(app.Close)
	l := &liveServer{dir: t.TempDir(), port: freePort(t)}
	l.conf = filepath.Join(l.dir, "bareproxy.conf")
	writeFile(t, filepath.Join(l.dir, "p.wasm"), string(shipped(t, "consent")))
	writeFile(t, filepath.Join(l.dir, "p.conf"), conf)
	writeFile(t, l.conf, fmt.Sprintf(`global
  admin off
  trace-log off
  state state

plugin p p.wasm
  config p.conf
  body response

site http://example.com:%d
  use p
  route /* -> app

pool app
  backend %s
`, l.port, strings.TrimPrefix(app.URL, "http://")))
	s, err := Start(l.conf)
	if err != nil {
		t.Fatal(err)
	}
	l.s = s
	t.Cleanup(s.Stop)
	return l
}

const uaChrome = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"

// browsing is what a browser sends for a page.
func browsing(more ...string) []string {
	return append([]string{"User-Agent", uaChrome, "Accept", "text/html,application/xhtml+xml,*/*;q=0.8",
		"Sec-Fetch-Dest", "document", "Sec-Fetch-Mode", "navigate", "Accept-Encoding", "gzip, br"}, more...)
}

func TestConsentBannerAndHeldCookies(t *testing.T) {
	l := consentServer(t, "privacy /privacy/\n")
	// The browser's kept copy may be from before a choice, so a page that
	// may change comes whole, and goes out with nothing to check it by.
	resp, body, rec := send(t, l, "GET", "/shoes?color=red", browsing("If-None-Match", `"v1"`, "If-Modified-Since", "Wed, 14 Oct 2026 10:00:00 GMT")...)
	cookies := resp.Header.Values("Set-Cookie")
	if resp.StatusCode != 200 || !strings.Contains(body, `<div id="bp-consent" role="region"`) ||
		!strings.Contains(body, `href="/.bareproxy-consent?accept=all&amp;r=/shoes%3Fcolor%3Dred"`) ||
		!strings.HasSuffix(body, "</div>\n</body></html>\n") {
		t.Fatalf("no banner: %d %v\n%s", resp.StatusCode, resp.Header, body)
	}
	if !strings.Contains(body, `<script async src="https://www.googletagmanager.com/gtag/js?id=G-1" type="text/plain" data-consent="analytics"></script>`) ||
		!strings.Contains(body, `<script>window.dataLayer=[];function gtag()`) || // it loads nothing itself
		!strings.Contains(body, `<script src="https://connect.facebook.net/en_US/fbevents.js" type="text/plain" data-consent="marketing"></script>`) ||
		!strings.Contains(body, `<script src="/app.js"></script>`) {
		t.Errorf("scripts:\n%s", body)
	}
	if !slices.Equal(cookies, []string{"session=abc; Path=/; HttpOnly"}) {
		t.Errorf("cookies that went out: %q", cookies)
	}
	if resp.Header.Get("Vary") != "Cookie" || resp.Header.Get("Cache-Control") != "private, no-cache" ||
		resp.Header.Get("Content-Encoding") != "" || resp.Header.Get("X-Saw-Encoding") == "gzip, br" ||
		resp.Header.Get("X-Saw-Consent") != "unknown" || resp.Header.Get("X-Saw-Conditional") != "" ||
		resp.Header.Get("ETag") != "" || resp.Header.Get("Last-Modified") != "" {
		t.Errorf("headers: %v", resp.Header)
	}
	if n := notes(rec); !strings.Contains(n, "held back _ga (analytics) and _fbp (marketing) until consent") ||
		!strings.Contains(n, "showed the banner: no choice yet; kept 2 scripts out until consent: googletagmanager.com (analytics), connect.facebook.net (marketing)") {
		t.Errorf("notes: %q", n)
	}
	// Not a page for a browser: no banner and no change, but the cookies
	// still wait, and the body streams as it came (compressed, here).
	for _, c := range []struct {
		name string
		hdr  []string
	}{
		{"an API call", []string{"User-Agent", uaChrome, "Accept", "application/json", "Sec-Fetch-Dest", "empty", "Sec-Fetch-Mode", "cors", "Accept-Encoding", "gzip"}},
		{"Googlebot", []string{"User-Agent", "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", "Accept", "text/html", "Accept-Encoding", "gzip"}},
		{"a page in an iframe", browsing("Sec-Fetch-Dest", "iframe")},
	} {
		path := "/page"
		if c.name == "an API call" {
			path = "/api/data"
		}
		resp, body, _ := send(t, l, "GET", path, c.hdr...)
		if strings.Contains(body, "bp-consent") || !slices.Equal(resp.Header.Values("Set-Cookie"), []string{"session=abc; Path=/; HttpOnly"}) ||
			resp.Header.Get("X-Saw-Encoding") != "gzip" && resp.Header.Get("X-Saw-Encoding") != "gzip, br" {
			t.Errorf("%s: %v", c.name, resp.Header)
		}
	}
	m := l.s.Current().Plugins["p"].Metrics()
	if m["banners"] != 1 || m["held"] != 8 || m["blocked"] != 2 {
		t.Errorf("metrics %v", m)
	}
}

// consentCookie is the consent cookie a choice of categories makes.
func consentCookie(t *testing.T, l *liveServer, query string) string {
	t.Helper()
	resp, _, _ := send(t, l, "GET", "/.bareproxy-consent?"+query, "Sec-Fetch-Site", "same-origin")
	for _, c := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(c, "bp_consent=") {
			return strings.SplitN(c, ";", 2)[0]
		}
	}
	t.Fatalf("no consent cookie for %s: %d %v", query, resp.StatusCode, resp.Header)
	return ""
}

func TestConsentAcceptWithdrawAndRefuse(t *testing.T) {
	l := consentServer(t, "")
	resp, _, rec := send(t, l, "GET", "/.bareproxy-consent?accept=all&r=%2Fshoes%3Fcolor%3Dred", "Sec-Fetch-Site", "same-origin")
	sets := resp.Header.Values("Set-Cookie")
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/shoes?color=red" || len(sets) != 1 ||
		!regexp.MustCompile(`^bp_consent=[0-9a-f]{6}\.analytics\+marketing; Path=/; Max-Age=15552000; SameSite=Lax$`).MatchString(sets[0]) ||
		notes(rec) != "consent: accepted analytics and marketing" {
		t.Fatalf("accept: %d %v %q", resp.StatusCode, resp.Header, notes(rec))
	}
	all := strings.SplitN(sets[0], ";", 2)[0]
	// With consent, the page and its cookies go out as the app made them.
	resp, body, rec := send(t, l, "GET", "/shoes", browsing("Cookie", all+"; _ga=GA1.1.42; _fbp=fb.1", "If-None-Match", `"v1"`)...)
	if strings.Contains(body, "bp-consent") || len(resp.Header.Values("Set-Cookie")) != 3 ||
		resp.Header.Get("X-Saw-Conditional") != `"v1"` || resp.Header.Get("ETag") != `"v1"` ||
		resp.Header.Get("X-Saw-Consent") != "analytics,marketing" || resp.Header.Get("X-Saw-Encoding") != "gzip, br" ||
		resp.Header.Get("X-Saw-Cookie") != all+"; _ga=GA1.1.42; _fbp=fb.1" || resp.Header.Get("Cache-Control") != "public, max-age=60" ||
		resp.Header.Get("Vary") != "Cookie" || notes(rec) != "" {
		t.Errorf("with consent: %v %q", resp.Header, notes(rec))
	}
	// Taking marketing back deletes its cookies in the browser.
	resp, _, rec = send(t, l, "GET", "/.bareproxy-consent?save=1&set=analytics&r=%2F", "Sec-Fetch-Site", "same-origin",
		"Cookie", all+"; _ga=GA1.1.42; _fbp=fb.1")
	sets = resp.Header.Values("Set-Cookie")
	if resp.StatusCode != 303 || len(sets) != 3 || !strings.HasSuffix(sets[0], ".analytics; Path=/; Max-Age=15552000; SameSite=Lax") ||
		sets[1] != "_fbp=; Path=/; Max-Age=0" || sets[2] != "_fbp=; Path=/; Domain=example.com; Max-Age=0" ||
		notes(rec) != "consent: accepted analytics; withdrew marketing" {
		t.Errorf("withdraw: %d %q %q", resp.StatusCode, sets, notes(rec))
	}
	some := strings.SplitN(sets[0], ";", 2)[0]
	resp, body, rec = send(t, l, "GET", "/shoes", browsing("Cookie", some+"; _ga=GA1.1.42; _fbp=fb.1")...)
	if strings.Contains(body, "bp-consent") || !strings.Contains(body, `<script async src="https://www.googletagmanager.com/gtag/js?id=G-1"></script>`) ||
		!strings.Contains(body, `fbevents.js" type="text/plain" data-consent="marketing">`) ||
		resp.Header.Get("X-Saw-Cookie") != some+"; _ga=GA1.1.42" || resp.Header.Get("X-Saw-Consent") != "analytics" ||
		!slices.Equal(resp.Header.Values("Set-Cookie"), []string{"session=abc; Path=/; HttpOnly", "_ga=GA1.1.42; Path=/; Max-Age=63072000"}) {
		t.Errorf("analytics only: %v %q\n%s", resp.Header, notes(rec), body)
	}
	if n := notes(rec); !strings.Contains(n, "kept _fbp from the app: no consent") || !strings.Contains(n, "kept 1 script out until consent: connect.facebook.net (marketing)") {
		t.Errorf("notes: %q", n)
	}
	// Refusing counts from anywhere; accepting only from the site's pages.
	resp, _, _ = send(t, l, "GET", "/.bareproxy-consent?reject=all", "Sec-Fetch-Site", "cross-site")
	if resp.StatusCode != 303 || !strings.Contains(resp.Header.Get("Set-Cookie"), ".none; ") {
		t.Errorf("reject from another site: %d %v", resp.StatusCode, resp.Header)
	}
	resp, body, rec = send(t, l, "GET", "/.bareproxy-consent?accept=all", "Sec-Fetch-Site", "cross-site")
	if resp.StatusCode != 200 || resp.Header.Get("Set-Cookie") != "" || !strings.Contains(body, "<h1>Cookie settings</h1>") ||
		notes(rec) != "an accept from another site isn't counted, so the settings page asks instead" {
		t.Errorf("accept from another site: %d %v %q", resp.StatusCode, resp.Header, notes(rec))
	}
	if resp, _, _ := send(t, l, "GET", "/.bareproxy-consent?accept=all", "Referer", "https://evil.example/"); resp.StatusCode != 200 {
		t.Errorf("accept with another site's Referer: %d", resp.StatusCode)
	}
	if resp, _, _ := send(t, l, "GET", "/.bareproxy-consent?accept=all", "Referer", fmt.Sprintf("http://example.com:%d/page", l.port)); resp.StatusCode != 303 {
		t.Errorf("accept with the site's Referer: %d", resp.StatusCode)
	}
	m := l.s.Current().Plugins["p"].Metrics()
	if m["accepted"] != 3 || m["declined"] != 1 || m["withdrawn"] != 1 {
		t.Errorf("metrics %v", m)
	}
}

func TestConsentGlobalPrivacyControl(t *testing.T) {
	l := consentServer(t, "")
	resp, body, rec := send(t, l, "GET", "/page", browsing("Sec-GPC", "1")...)
	if strings.Contains(body, "bp-consent") || !strings.Contains(body, `type="text/plain" data-consent="analytics"`) ||
		resp.Header.Get("X-Saw-Consent") != "none" || len(resp.Header.Values("Set-Cookie")) != 1 ||
		!strings.Contains(notes(rec), "the browser sends Global Privacy Control, so nothing is accepted") {
		t.Errorf("GPC: %v %q\n%s", resp.Header, notes(rec), body)
	}
	_, body, _ = send(t, l, "GET", "/.bareproxy-consent", "Sec-GPC", "1")
	if !strings.Contains(body, `<p class="bp-consent-note">Your browser asks sites not to track you`) {
		t.Errorf("settings page with GPC:\n%s", body)
	}
	// A choice made on the site counts over the signal.
	c := consentCookie(t, l, "save=1&set=analytics")
	if resp, _, _ := send(t, l, "GET", "/page", browsing("Sec-GPC", "1", "Cookie", c)...); resp.Header.Get("X-Saw-Consent") != "analytics" {
		t.Errorf("GPC after a choice: %v", resp.Header)
	}
	off := consentServer(t, "gpc off\n")
	if _, body, _ := send(t, off, "GET", "/page", browsing("Sec-GPC", "1")...); !strings.Contains(body, "bp-consent") {
		t.Error("gpc off: no banner")
	}
}

func TestConsentSettingsPage(t *testing.T) {
	l := consentServer(t, "category stats Statistics: to count visits\ncookies stats _ga\nblock stats googletagmanager.com\ntitle Cookies\nlang de\nskip /checkout/\n")
	c := consentCookie(t, l, "save=1&set=stats")
	resp, body, _ := send(t, l, "GET", "/.bareproxy-consent", "Cookie", c, "Referer", fmt.Sprintf("http://example.com:%d/docs/x?y=1", l.port))
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" || !strings.Contains(body, `<html lang="de">`) ||
		!strings.Contains(body, `<input type="checkbox" name="set" value="stats" checked> Statistics</label><span>to count visits</span>`) ||
		!strings.Contains(body, `<input type="hidden" name="r" value="/docs/x?y=1">`) || !strings.Contains(body, "<title>Cookies</title>") {
		t.Errorf("settings: %d %v\n%s", resp.StatusCode, resp.Header, body)
	}
	resp, css, _ := send(t, l, "GET", "/.bareproxy-consent/consent.css")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/css; charset=utf-8" ||
		resp.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" || !strings.HasPrefix(css, "#bp-consent{") {
		t.Errorf("styles: %d %v", resp.StatusCode, resp.Header)
	}
	if resp, _, _ := send(t, l, "POST", "/.bareproxy-consent"); resp.StatusCode != 405 || resp.Header.Get("Allow") != "GET, HEAD" {
		t.Errorf("POST: %d", resp.StatusCode)
	}
	if resp, _, _ := send(t, l, "GET", "/.bareproxy-consent/other"); resp.StatusCode != 404 {
		t.Errorf("another path under the endpoint: %d", resp.StatusCode)
	}
	// The config's own categories: _fbp isn't in one, so it's needed.
	resp, body, _ = send(t, l, "GET", "/page", browsing()...)
	if !slices.Equal(resp.Header.Values("Set-Cookie"), []string{"session=abc; Path=/; HttpOnly", "_fbp=fb.1.42; Path=/; Domain=example.com"}) ||
		strings.Contains(body, `fbevents.js" type="text/plain"`) || !strings.Contains(body, `data-consent="stats"`) {
		t.Errorf("own categories: %v\n%s", resp.Header, body)
	}
	// A skipped path gets no banner, and its cookies still wait.
	resp, body, _ = send(t, l, "GET", "/checkout/pay", browsing()...)
	if strings.Contains(body, "bp-consent") || strings.Contains(body, "data-consent") || len(resp.Header.Values("Set-Cookie")) != 2 {
		t.Errorf("a skipped path: %v\n%s", resp.Header, body)
	}
}

func TestConsentConfigRefused(t *testing.T) {
	configRefused(t, "consent", "gpc maybe\n", "line 1: gpc takes on or off")
	configRefused(t, "consent", "cookies ads _x\n", "line 1: cookies names category ads, which no category line before it defines")
}
