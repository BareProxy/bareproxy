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
	"strings"
	"testing"
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

site http://example.com:%d
  use p
  route /api/* -> api
  route /* -> files public

pool api
  backend %s
`, l.port, strings.TrimPrefix(backend, "http://")))
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

// send makes a request with the given headers and returns the response, its
// body and the request's record.
func send(t *testing.T, l *liveServer, method, path string, hdr ...string) (*http.Response, string, *Record) {
	t.Helper()
	req, _ := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", l.port, path), nil)
	req.Host = fmt.Sprintf("example.com:%d", l.port)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := directClient.Do(req)
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
