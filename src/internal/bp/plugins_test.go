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
	"strconv"
	"strings"
	"testing"

	"bareproxy/internal/plugin/plugintest"
)

// testPlugin is the test plugin the running test uses: the Go one or the
// Rust one (see bothPlugins).
var testPlugin []byte

// bothPlugins runs a test with the Go test plugin and with the Rust one, as
// subtests go and rust. The Rust one is skipped when BP_RUST_FIXTURE isn't
// set (see plugintest.RustFixture).
func bothPlugins(t *testing.T, test func(t *testing.T)) {
	goWasm, err := plugintest.Fixture()
	if err != nil {
		t.Fatal(err)
	}
	rustWasm, err := plugintest.RustFixture()
	if err != nil {
		t.Fatal(err)
	}
	for _, fx := range []struct {
		lang string
		wasm []byte
	}{{"go", goWasm}, {"rust", rustWasm}} {
		t.Run(fx.lang, func(t *testing.T) {
			if fx.wasm == nil {
				t.Skip("no Rust test plugin: set BP_RUST_FIXTURE to plugins/dist/testplugin.wasm")
			}
			testPlugin = fx.wasm
			test(t)
		})
	}
}

// pluginFiles writes the test plugin and its config to dir.
func pluginFiles(t *testing.T, dir, modes string) (wasm, conf string) {
	t.Helper()
	b := testPlugin
	if b == nil {
		var err error
		if b, err = plugintest.Fixture(); err != nil {
			t.Fatal(err)
		}
	}
	wasm, conf = filepath.Join(dir, "test.wasm"), filepath.Join(dir, "test.txt")
	if os.WriteFile(wasm, b, 0o644) != nil || os.WriteFile(conf, []byte(modes), 0o644) != nil {
		t.Fatal("can't write the plugin files")
	}
	return wasm, conf
}

// pluginServer starts BareProxy with the test plugin on a site that sends
// / to backend (when set) and everything else to files in dir/public.
func pluginServer(t *testing.T, modes, backend, pluginLines string) *liveServer {
	t.Helper()
	l := &liveServer{dir: t.TempDir(), port: freePort(t)}
	l.conf = filepath.Join(l.dir, "bareproxy.conf")
	os.MkdirAll(filepath.Join(l.dir, "public"), 0o755)
	writeFile(t, filepath.Join(l.dir, "public", "page.txt"), "a static page\n")
	pluginFiles(t, l.dir, modes)
	writeFile(t, l.conf, pluginConfig(l, backend, pluginLines))
	s, err := Start(l.conf)
	if err != nil {
		t.Fatal(err)
	}
	l.s = s
	t.Cleanup(s.Stop)
	return l
}

func pluginConfig(l *liveServer, backend, pluginLines string) string {
	api := "  route /api/* -> respond 200 \"no backend\"\n"
	pool := ""
	if backend != "" {
		api, pool = "  route /api/* -> api\n", "pool api\n  backend "+strings.TrimPrefix(backend, "http://")+"\n"
	}
	return fmt.Sprintf(`global
  admin off
  trace-log off
  state state

plugin test test.wasm
  config test.txt
  timeout 2s
%s
site http://example.com:%d
  use test
%s  route /* -> files public

%s`, pluginLines, l.port, api, pool)
}

// fetch gets a path and returns the response and the request's record.
func fetch(t *testing.T, l *liveServer, path string) (*http.Response, string, *Record) {
	t.Helper()
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d%s", l.port, path), nil)
	req.Host = fmt.Sprintf("example.com:%d", l.port)
	resp, err := directClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	js, _ := l.s.Current().Mem.Find(resp.Header.Get("BareProxy-Id"))
	var rec Record
	if err := json.Unmarshal(js, &rec); err != nil {
		t.Fatalf("no record for %s: %v", path, err)
	}
	return resp, string(b), &rec
}

func TestPluginAnswersBeforeRouting(t *testing.T) { bothPlugins(t, testPluginAnswersBeforeRouting) }

func testPluginAnswersBeforeRouting(t *testing.T) {
	l := pluginServer(t, "deny", "", "")
	resp, body, rec := fetch(t, l, "/page.txt")
	if resp.StatusCode != 403 || body != "denied by plugin\n" || resp.Header.Get("X-Denied") != "1" || resp.Header.Get("BareProxy-Id") == "" {
		t.Errorf("response %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if rec.Outcome != "plugin" || rec.Status != 403 || rec.Rule != "" || len(rec.Plugins) != 1 ||
		rec.Plugins[0].Action != "answered 403" || !strings.HasPrefix(rec.Reason, "plugin test answered 403") {
		t.Errorf("record %+v", rec)
	}
	if why := RenderWhy(rec); !strings.Contains(why, "Plugin test: answered 403") {
		t.Errorf("why:\n%s", why)
	}
}

func TestPluginChangesRequestAndResponse(t *testing.T) {
	bothPlugins(t, testPluginChangesRequestAndResponse)
}

func testPluginChangesRequestAndResponse(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "backend saw x-plugin=%s\n", r.Header.Get("X-Plugin"))
	}))
	defer be.Close()
	l := pluginServer(t, "headers resp log", be.URL, "  body response\n")
	for _, c := range []struct{ path, body string }{
		{"/api/x", "BACKEND SAW X-PLUGIN=HELLO\n"}, // proxied
		{"/page.txt", "A STATIC PAGE\n"},           // a file, read in for the plugin instead of sent with sendfile
	} {
		resp, body, rec := fetch(t, l, c.path)
		if resp.StatusCode != 200 || body != c.body || resp.Header.Get("X-Resp") != "200" || resp.ContentLength != int64(len(c.body)) {
			t.Errorf("%s: %d %q %v", c.path, resp.StatusCode, body, resp.Header)
		}
		p := rec.Plugins
		if len(p) != 1 || p[0].Action != "went on" || !slices.Equal(p[0].Notes, []string{"saw GET " + c.path + " from 127.0.0.1", "logged"}) ||
			rec.BytesOut != int64(len(c.body)) {
			t.Errorf("%s: record %+v", c.path, rec)
		}
	}
	// Explain lists the plugin; why lists its notes.
	out, err := Explain(l.s.Current(), "GET", fmt.Sprintf("http://example.com:%d/", l.port), nil, true)
	if err != nil || !strings.Contains(out, "  test (line 6): "+filepath.Join(l.dir, "test.wasm")) || !strings.Contains(out, "instances working") {
		t.Errorf("explain:\n%s", out)
	}
}

func TestPluginFailureFollowsOnError(t *testing.T) { bothPlugins(t, testPluginFailureFollowsOnError) }

func testPluginFailureFollowsOnError(t *testing.T) {
	l := pluginServer(t, "crash", "", "")
	resp, _, rec := fetch(t, l, "/page.txt")
	if resp.StatusCode != 502 || rec.Outcome != "plugin_error" || !strings.Contains(rec.Plugins[0].Error, "crashed") {
		t.Errorf("on-error closed: %d, record %+v", resp.StatusCode, rec)
	}
	l = pluginServer(t, "crash", "", "  on-error open\n")
	resp, body, rec := fetch(t, l, "/page.txt")
	if resp.StatusCode != 200 || body != "a static page\n" || rec.Outcome != "file" ||
		rec.Plugins[0].Action != "failed, so the request went on without it (on-error open)" {
		t.Errorf("on-error open: %d %q, record %+v", resp.StatusCode, body, rec)
	}
}

// A plugin file that changes under the same config text is a change: plan
// says so, the history keeps the files each version ran, and a rollback runs
// the old file, not whatever is at the path now.
func TestPluginFilesInPlanHistoryAndRollback(t *testing.T) {
	bothPlugins(t, testPluginFilesInPlanHistoryAndRollback)
}

func testPluginFilesInPlanHistoryAndRollback(t *testing.T) {
	l := pluginServer(t, "headers", "", "")
	text, _ := os.ReadFile(l.conf)
	first := l.s.Current().Plugins["test"]
	// A route change keeps the running plugin as it is.
	more := strings.Replace(string(text), "  route /* -> files public", "  route /x -> respond 200 x\n  route /* -> files public", 1)
	mustApply(t, l.s, more)
	if l.s.Current().Plugins["test"] != first {
		t.Error("a route change restarted the plugin")
	}
	mustApply(t, l.s, string(text))
	// The same text with another plugin file and another plugin config.
	wasm, _ := os.ReadFile(filepath.Join(l.dir, "test.wasm"))
	other := append(slices.Clone(wasm), 0, 3, 1, 'x', 0) // a custom section: the same code, another SHA-256
	writeFile(t, filepath.Join(l.dir, "test.wasm"), string(other))
	writeFile(t, filepath.Join(l.dir, "test.txt"), "deny")
	c, probs := Parse(l.conf, string(text))
	if HasErrors(probs) {
		t.Fatal(probs)
	}
	plan := MakePlan(l.s.Current().Cfg, c).Text()
	c.Close()
	if !strings.Contains(plan, "plugin test: "+filepath.Join(l.dir, "test.wasm")+" changed, sha256") ||
		!strings.Contains(plan, "plugin test: config "+filepath.Join(l.dir, "test.txt")+" changed") {
		t.Errorf("plan:\n%s", plan)
	}
	a := mustApply(t, l.s, string(text))
	if a.Unchanged {
		t.Fatal("a changed plugin file counted as no change")
	}
	if resp, _, _ := fetch(t, l, "/page.txt"); resp.StatusCode != 403 {
		t.Errorf("the new plugin config isn't running: %d", resp.StatusCode)
	}
	if _, err := l.s.Rollback(0, "tester"); err != nil {
		t.Fatal(err)
	}
	resp, body, rec := fetch(t, l, "/page.txt")
	if resp.StatusCode != 200 || body != "a static page\n" || len(rec.Plugins) != 1 || len(rec.Plugins[0].Notes) != 1 {
		t.Errorf("after the rollback: %d %q, record %+v", resp.StatusCode, body, rec)
	}
	if st := l.s.Status(); len(st.Plugins) != 1 || !st.Plugins[0].Pinned || st.Plugins[0].SHA256 != sha256Hex(wasm) {
		t.Errorf("status %+v", st.Plugins)
	}
	files, _ := os.ReadDir(filepath.Join(l.dir, "state", "plugins", "files"))
	if len(files) != 4 { // two plugin files, two configs
		t.Errorf("%d files kept in the state folder, want 4", len(files))
	}
}

func TestPluginConfigProblems(t *testing.T) {
	dir := t.TempDir()
	wasm, _ := pluginFiles(t, dir, "")
	writeFile(t, filepath.Join(dir, "bad.wasm"), "not wasm")
	for _, c := range []struct{ conf, want string }{
		{"plugin p " + wasm + "\nsite http://a.test:1\n  use q\n  route /* -> respond 200 x\n", "line 3: error: no plugin named q"},
		{"plugin p " + wasm + "\nsite http://a.test:1\n  route /* -> respond 200 x\n", "line 1: warning: plugin p isn't used by any site"},
		{"plugin p bad.wasm\nsite http://a.test:1\n  use p\n  route /* -> respond 200 x\n", "line 1: error: plugin p (" + filepath.Join(dir, "bad.wasm") + "): not a WebAssembly module"},
		{"plugin p missing.wasm\nsite http://a.test:1\n  use p\n  route /* -> respond 200 x\n", "line 1: error: can't read plugin p"},
		{"plugin p\n", "line 1: error: plugin takes a name"},
		{"plugin p " + wasm + "\n  on-error maybe\n", "line 2: error: on-error takes open"},
		{"plugin p " + wasm + "\n  memory 10KB\n", "line 2: error: memory must be at least 1.0 MB"},
		{"plugin p " + wasm + "\n  allow-http example.com\n", "line 2: error: \"example.com\" isn't host:port"},
		{"plugin p " + wasm + "\n  colour blue\n", "line 2: error: unknown plugin setting \"colour\""},
		{"plugin p " + wasm + "\nsite http://a.test:1\n  use p p\n", "line 3: error: plugin p is already used by this site"},
	} {
		c2, probs := Parse(filepath.Join(dir, "bareproxy.conf"), c.conf)
		c2.Close()
		var got []string
		for _, p := range probs {
			got = append(got, p.String())
		}
		if !slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, c.want) }) {
			t.Errorf("%q: problems %q, want one starting %q", c.conf, got, c.want)
		}
	}
}

// A plugin that fails or answers on the response replaces it: nothing of the
// backend's body gets through.
func TestPluginReplacesTheResponse(t *testing.T) { bothPlugins(t, testPluginReplacesTheResponse) }

func testPluginReplacesTheResponse(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "SECRET BACKEND BODY\n")
	}))
	defer be.Close()
	for _, c := range []struct {
		modes      string
		code       int
		body       string
		outcome    string
		actionPart string
	}{
		{"crashresp", 502, "Bad gateway: a plugin failed\n", "plugin_error", "failed"},
		{"denyresp", 451, "replaced by plugin\n", "ok", "replaced the response with 451"},
	} {
		l := pluginServer(t, c.modes, be.URL, "")
		for _, path := range []string{"/api/x", "/page.txt"} {
			resp, body, rec := fetch(t, l, path)
			if resp.StatusCode != c.code || !strings.HasPrefix(body, c.body) || strings.Contains(body, "SECRET") || strings.Contains(body, "static") {
				t.Errorf("%s %s: %d %q", c.modes, path, resp.StatusCode, body)
			}
			if rec.Status != c.code || !strings.Contains(rec.Plugins[0].Action, c.actionPart) || rec.BytesOut != int64(len(body)) {
				t.Errorf("%s %s: record %+v", c.modes, path, rec)
			}
		}
	}
}

// Two plugins on one site: each gets its own properties, and a plugin that
// fails with on-error open leaves no changes behind.
func TestTwoPluginsOnOneSite(t *testing.T) { bothPlugins(t, testTwoPluginsOnOneSite) }

func testTwoPluginsOnOneSite(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "partial=%q plugin=%q\n", r.Header.Get("X-Partial"), r.Header.Get("X-Plugin"))
	}))
	defer be.Close()
	l := &liveServer{dir: t.TempDir(), port: freePort(t)}
	l.conf = filepath.Join(l.dir, "bareproxy.conf")
	pluginFiles(t, l.dir, "")
	for name, modes := range map[string]string{"a.txt": "setprop", "b.txt": "partial", "c.txt": "headers"} {
		writeFile(t, filepath.Join(l.dir, name), modes)
	}
	writeFile(t, l.conf, fmt.Sprintf(`global
  admin off
  trace-log off
  state state

plugin a test.wasm
  config a.txt
  timeout 2s
plugin b test.wasm
  config b.txt
  timeout 2s
  on-error open
plugin c test.wasm
  config c.txt
  timeout 2s

site http://example.com:%d
  use a b
  use c
  route /* -> api

pool api
  backend %s
`, l.port, strings.TrimPrefix(be.URL, "http://")))
	s, err := Start(l.conf)
	if err != nil {
		t.Fatal(err)
	}
	l.s = s
	t.Cleanup(s.Stop)
	resp, body, rec := fetch(t, l, "/x")
	if resp.StatusCode != 200 || body != "partial=\"\" plugin=\"hello\"\n" {
		t.Errorf("response %d %q", resp.StatusCode, body)
	}
	if len(rec.Plugins) != 3 || !strings.HasPrefix(rec.Plugins[1].Action, "failed") ||
		!slices.Equal(rec.Plugins[2].Notes, []string{"saw GET /x from 127.0.0.1"}) {
		t.Errorf("record %+v", rec.Plugins)
	}
}

// A plugin kept across an apply keeps reading its folder after the old
// config's folders close.
func TestKeptPluginKeepsItsFolders(t *testing.T) { bothPlugins(t, testKeptPluginKeepsItsFolders) }

func testKeptPluginKeepsItsFolders(t *testing.T) {
	l := pluginServer(t, "read", "", "  read public\n")
	writeFile(t, filepath.Join(l.dir, "public", "hello.txt"), "hi\n")
	text, _ := os.ReadFile(l.conf)
	old := l.s.Current()
	mustApply(t, l.s, strings.Replace(string(text), "  route /* -> files public", "  route /x -> respond 200 x\n  route /* -> files public", 1))
	if l.s.Current().Plugins["test"] != old.Plugins["test"] {
		t.Fatal("the plugin wasn't kept")
	}
	old.Cfg.Close() // as retire does, two minutes later
	if _, _, rec := fetch(t, l, "/page.txt"); rec.Status != 200 {
		t.Errorf("record %+v", rec)
	}
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()
	// The read result goes on the request, which the record doesn't show,
	// so look at it through a backend.
	got := ""
	be.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Get("X-Read") })
	mustApply(t, l.s, strings.Replace(string(text), "  route /* -> files public", "  route /* -> api\n\npool api\n  backend "+strings.TrimPrefix(be.URL, "http://"), 1))
	if l.s.Current().Plugins["test"] != old.Plugins["test"] {
		t.Fatal("the plugin wasn't kept")
	}
	fetch(t, l, "/y")
	if got != "hi 0" {
		t.Errorf("x-read %q, want \"hi 0\"", got)
	}
}

// benchPlugin measures a request answered by a respond rule, with the test
// plugin in the given modes on its site, or with no plugin when modes is "".
func benchPlugin(b *testing.B, modes string, rust bool) {
	testPlugin = nil
	if rust {
		w, err := plugintest.RustFixture()
		if err != nil || w == nil {
			b.Skip("no Rust test plugin: set BP_RUST_FIXTURE")
		}
		testPlugin = w
		defer func() { testPlugin = nil }()
	}
	dir := b.TempDir()
	wasm, _ := pluginFiles(&testing.T{}, dir, modes)
	src := "global\n  trace-log off\n"
	use := ""
	if modes != "" {
		src += "plugin p " + wasm + "\n  config test.txt\n  instances 1\n"
		use = "  use p\n"
	}
	src += "site http://bench.local:8080\n" + use + "  route /* -> respond 200 \"x\"\n"
	c, probs := Parse(filepath.Join(dir, "bench.conf"), src)
	if HasErrors(probs) {
		b.Fatal(probs)
	}
	defer c.Close()
	rt, err := NewRuntime(c, nil, 1, nil, true)
	if err != nil {
		b.Fatal(err)
	}
	defer rt.Stop()
	h := NewServer("bench.conf", rt).Handler(8080, false)
	req := httptest.NewRequest(http.MethodGet, "/a/b", nil)
	req.Host, req.RemoteAddr = "bench.local:8080", "127.0.0.1:40000"
	req.Header.Set("User-Agent", "bench")
	w := &nullWriter{h: http.Header{}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		clear(w.h)
		h.ServeHTTP(w, req.Clone(req.Context()))
	}
}

func BenchmarkPluginNone(b *testing.B)        { benchPlugin(b, "", false) }
func BenchmarkPluginGoEmpty(b *testing.B)     { benchPlugin(b, "nothing", false) }
func BenchmarkPluginGoHeaders(b *testing.B)   { benchPlugin(b, "headers", false) }
func BenchmarkPluginRustEmpty(b *testing.B)   { benchPlugin(b, "nothing", true) }
func BenchmarkPluginRustHeaders(b *testing.B) { benchPlugin(b, "headers", true) }

// A plugin's own response goes back through the plugins before it, as a
// backend's would, last first; not through itself or the ones after it.
func TestPluginAnswerPassesEarlierPlugins(t *testing.T) {
	bothPlugins(t, testPluginAnswerPassesEarlierPlugins)
}

func testPluginAnswerPassesEarlierPlugins(t *testing.T) {
	for _, c := range []struct{ answer, action string }{{"deny", "answered 403"}, {"denyresp", "replaced the response with 451"}} {
		l := &liveServer{dir: t.TempDir(), port: freePort(t)}
		l.conf = filepath.Join(l.dir, "bareproxy.conf")
		pluginFiles(t, l.dir, "")
		writeFile(t, filepath.Join(l.dir, "a.txt"), "resp")
		writeFile(t, filepath.Join(l.dir, "b.txt"), c.answer)
		writeFile(t, filepath.Join(l.dir, "c.txt"), "resp")
		writeFile(t, l.conf, fmt.Sprintf(`global
  admin off
  trace-log off
  state state

plugin a test.wasm
  config a.txt
  timeout 2s
plugin b test.wasm
  config b.txt
  timeout 2s
plugin c test.wasm
  config c.txt
  timeout 2s

site http://example.com:%d
  use a b c
  route /* -> respond 200 "from the core"
`, l.port))
		s, err := Start(l.conf)
		if err != nil {
			t.Fatal(err)
		}
		l.s = s
		resp, body, rec := fetch(t, l, "/x")
		want := 403
		if c.answer == "denyresp" {
			want = 451
		}
		// a sees b's answer and adds x-resp; when b answers before routing,
		// c never ran, and when b replaces the response, c had seen the
		// core's 200 first.
		if resp.StatusCode != want || resp.Header.Get("X-Resp") != strconv.Itoa(want) || strings.Contains(body, "core") {
			t.Errorf("%s: response %d %q %v", c.answer, resp.StatusCode, body, resp.Header)
		}
		if rec.Plugins[1].Action != c.action || rec.Status != want {
			t.Errorf("%s: record %+v %+v", c.answer, rec, rec.Plugins)
		}
		s.Stop()
	}
}
