// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"bareproxy/internal/plugin/plugintest"
)

// The test plugins: the Go one (testdata/fixture), built once for the
// package, and the Rust one (plugins/testplugin), when BP_RUST_FIXTURE names
// its .wasm file. fixture and lang are the one the running test uses.
var (
	goFixture, rustFixture []byte
	fixture                []byte
	lang                   string
)

func TestMain(m *testing.M) {
	var err error
	if goFixture, err = plugintest.Fixture(); err == nil {
		rustFixture, err = plugintest.RustFixture()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// both runs a test with each test plugin, as subtests go and rust.
func both(t *testing.T, test func(t *testing.T)) {
	for _, fx := range []struct {
		lang string
		wasm []byte
	}{{"go", goFixture}, {"rust", rustFixture}} {
		t.Run(fx.lang, func(t *testing.T) {
			if fx.wasm == nil {
				t.Skip("no Rust test plugin: set BP_RUST_FIXTURE to plugins/dist/testplugin.wasm")
			}
			fixture, lang = fx.wasm, fx.lang
			test(t)
		})
	}
}

func start(t *testing.T, modes string, s Settings, memory ...int64) *Plugin {
	t.Helper()
	m, err := Compile(fixture, append(memory, 64<<20)[0])
	if err != nil {
		t.Fatal(err)
	}
	defer m.Release()
	s.Name, s.Config = "test", []byte(modes)
	if s.Timeout == 0 {
		s.Timeout = time.Second // the race detector is slow
	}
	var mu sync.Mutex
	var logs []string
	s.Logf = func(f string, a ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(f, a...))
		mu.Unlock()
	}
	p, err := Start(m, s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// request runs a request's header phase and returns the headers after it.
func request(t *testing.T, p *Plugin, extra ...[2]string) (*Stream, map[string]string, error) {
	t.Helper()
	s, err := p.NewStream(map[string]string{"request.path": "/a/b", "source.address": "192.0.2.7"}, nil)
	if err != nil {
		return nil, nil, err
	}
	h := append([][2]string{{":method", "GET"}, {":path", "/a/b"}, {":authority", "example.com"}}, extra...)
	h, err = s.Headers(Request, h, true, true)
	out := map[string]string{}
	for _, kv := range h {
		out[kv[0]] = kv[1]
	}
	return s, out, err
}

func TestCompileRefusesWhatIsNotAPlugin(t *testing.T) {
	for _, c := range []struct {
		wasm []byte
		want string
	}{
		{[]byte("not wasm"), "not a WebAssembly module"},
		{[]byte("\x00asm\x01\x00\x00\x00"), "isn't a Proxy-Wasm plugin"},
	} {
		if _, err := Compile(c.wasm, 16<<20); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Compile: %v, want %q", err, c.want)
		}
	}
	m, err := Compile(goFixture, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Release()
	if m.ABI != "0.2.1" || len(m.SHA) != 64 || !m.Has("proxy_on_response_body") || m.Has("proxy_on_request_body") {
		t.Errorf("module: ABI %s, SHA %q, exports %v", m.ABI, m.SHA, m.exports)
	}
	if rustFixture != nil { // the Rust SDK exports every callback
		m, err := Compile(rustFixture, 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		defer m.Release()
		if m.ABI != "0.2.1" || !m.Has("proxy_on_request_body") {
			t.Errorf("Rust module: ABI %s, exports %v", m.ABI, m.exports)
		}
	}
}

func TestRequestHeadersAndNotes(t *testing.T) { both(t, testRequestHeadersAndNotes) }

func testRequestHeadersAndNotes(t *testing.T) {
	p := start(t, "headers count", Settings{Instances: 2})
	for i := 1; i <= 4; i++ {
		s, h, err := request(t, p)
		if err != nil {
			t.Fatal(err)
		}
		if h["x-plugin"] != "hello" || h["x-count"] != fmt.Sprint(i) {
			t.Errorf("request %d: headers %v", i, h)
		}
		if n := s.Notes(); !slices.Equal(n, []string{"saw GET /a/b from 192.0.2.7"}) {
			t.Errorf("notes %q", n)
		}
		s.Done(true)
	}
}

func TestLocalResponse(t *testing.T) { both(t, testLocalResponse) }

func testLocalResponse(t *testing.T) {
	p := start(t, "deny", Settings{})
	s, _, err := request(t, p)
	if err != nil {
		t.Fatal(err)
	}
	l := s.Local()
	details := map[string]string{"go": "test_deny", "rust": ""}[lang] // the Rust SDK sends no details
	if l == nil || l.Status != 403 || string(l.Body) != "denied by plugin\n" || l.Details != details ||
		!slices.Contains(l.Headers, [2]string{"x-denied", "1"}) {
		t.Errorf("local response %+v", l)
	}
}

func TestResponsePhase(t *testing.T) { both(t, testResponsePhase) }

func testResponsePhase(t *testing.T) {
	p := start(t, "resp log", Settings{})
	s, _, err := request(t, p)
	if err != nil {
		t.Fatal(err)
	}
	s.SetInt("response.code", 200)
	h, err := s.Headers(Response, [][2]string{{":status", "200"}}, false, false)
	if err != nil || !slices.Contains(h, [2]string{"x-resp", "200"}) {
		t.Errorf("response headers %v, %v", h, err)
	}
	b, err := s.Body(Response, []byte("hello"))
	if err != nil || string(b) != "HELLO" {
		t.Errorf("response body %q, %v", b, err)
	}
	if err := s.Done(true); err != nil || !slices.Contains(s.Notes(), "logged") {
		t.Errorf("log: %v, notes %q", err, s.Notes())
	}
}

func TestOutgoingCall(t *testing.T) { both(t, testOutgoingCall) }

func testOutgoingCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s\n", r.Host, r.URL.Path)
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	p := start(t, "call", Settings{AllowHTTP: []string{"127.0.0.1:" + port}})
	_, h, err := request(t, p, [2]string{"x-call-port", port})
	if err != nil || h["x-called"] != "200 callee /called" {
		t.Errorf("allowed call: %v, %v", h, err)
	}
	// An address the config doesn't name is refused, and the request goes on.
	s, h, err := request(t, p, [2]string{"x-call-port", "1"})
	if err != nil || h["x-called"] != "refused 2" || !slices.Contains(s.Notes(), "call to 127.0.0.1:1 refused: not allowed") {
		t.Errorf("refused call: %v, %v, notes %q", h, err, s.Notes())
	}
}

func TestStoreAndRead(t *testing.T) { both(t, testStoreAndRead) }

func testStoreAndRead(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi there\n"), 0o644)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	p := start(t, "store read", Settings{StoreDir: filepath.Join(t.TempDir(), "store"), StoreMax: 1 << 20, Read: []*os.Root{root}})
	_, h, err := request(t, p)
	if err != nil || h["x-stored"] != "/a/b" || h["x-read"] != "hi there 0" {
		t.Errorf("headers %v, %v", h, err)
	}
	// With no store and no folder, both say not found.
	p = start(t, "store read", Settings{})
	if _, h, _ = request(t, p); h["x-stored"] != "" || h["x-read"] != " 1" {
		t.Errorf("without store and folder: %v", h)
	}
}

func TestTicks(t *testing.T) { both(t, testTicks) }

func testTicks(t *testing.T) {
	p := start(t, "tick", Settings{})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		v := string(p.data["ticks"].val)
		p.mu.Unlock()
		if v != "" && v != "1" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("the timer didn't tick twice")
}

// A plugin that fails breaks only its instance: the request gets an error,
// and a new instance takes its place.
func TestFailuresBreakTheInstanceOnly(t *testing.T) { both(t, testFailuresBreakTheInstanceOnly) }

func testFailuresBreakTheInstanceOnly(t *testing.T) {
	for _, c := range []struct {
		modes  string
		s      Settings
		memory int64
		want   string
	}{
		{"loop", Settings{Timeout: 50 * time.Millisecond}, 0, "ran past its time limit of 50ms"},
		{"crash", Settings{}, 0, "crashed: wasm error: unreachable"},
		{"grow", Settings{}, 32 << 20, "crashed: wasm error: unreachable"},
		{"hold", Settings{Pause: 100 * time.Millisecond}, 0, "kept the request waiting past its limit of 100ms"},
	} {
		t.Run(c.modes, func(t *testing.T) {
			p := start(t, c.modes, c.s, append([]int64{c.memory}[:min(1, int(c.memory))], 64<<20)...)
			_, _, err := request(t, p)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %v, want %q", err, c.want)
			}
			if c.modes == "hold" {
				return
			}
			old := p.insts[0]
			deadline := time.Now().Add(20 * time.Second)
			for {
				p.mu.Lock()
				replaced := p.insts[0] != old && p.insts[0].broken == nil
				p.mu.Unlock()
				if replaced {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("no new instance")
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

// The memory cap is the configured one: 48 MB fits in 128 MB, not in 32 MB
// (TestFailuresBreakTheInstanceOnly).
func TestMemoryCap(t *testing.T) { both(t, testMemoryCap) }

func testMemoryCap(t *testing.T) {
	p := start(t, "grow", Settings{}, 128<<20)
	if _, _, err := request(t, p); err != nil {
		t.Errorf("48 MB in a 128 MB cap: %v", err)
	}
}

func TestRefusedConfigStopsStart(t *testing.T) { both(t, testRefusedConfigStopsStart) }

func testRefusedConfigStopsStart(t *testing.T) {
	m, err := Compile(fixture, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Release()
	_, err = Start(m, Settings{Name: "test", Config: []byte("refuse")})
	if err == nil || !strings.Contains(err.Error(), "refused its config") {
		t.Errorf("Start: %v", err)
	}
}

func TestPairs(t *testing.T) {
	h := [][2]string{{":path", "/"}, {"x-a", ""}, {"x-b", "v"}}
	got, ok := DecodePairs(EncodePairs(h))
	if !ok || !slices.Equal(got, h) {
		t.Errorf("round trip %v %v", got, ok)
	}
	for _, bad := range [][]byte{{1, 0, 0, 0}, {1, 0, 0, 0, 5, 0, 0, 0, 0, 0, 0, 0, 'a', 0}} {
		if _, ok := DecodePairs(bad); ok {
			t.Errorf("DecodePairs(%v) accepted", bad)
		}
	}
}

// A plugin that crashes, and whose new instances then fail to start, is
// retried with growing waits, not in a storm.
func TestNoRestartStorm(t *testing.T) { both(t, testNoRestartStorm) }

func testNoRestartStorm(t *testing.T) {
	p := start(t, "crash flaky", Settings{})
	if _, _, err := request(t, p); err == nil {
		t.Fatal("no crash")
	}
	time.Sleep(3 * time.Second)
	p.mu.Lock()
	starts := string(p.data["starts"].val)
	p.mu.Unlock()
	if n, _ := strconv.Atoi(starts); n < 2 || n > 5 {
		t.Errorf("%s instances started in 3 seconds", starts)
	}
}

// A plugin that sleeps doesn't hold its instance: WASI's sleep returns at
// once, or the call runs into its time limit.
func TestSleepDoesNotHold(t *testing.T) {
	fixture = goFixture // a Rust plugin has no sleep
	p := start(t, "sleep", Settings{Timeout: 100 * time.Millisecond})
	begin := time.Now()
	request(t, p)
	if d := time.Since(begin); d > time.Second {
		t.Errorf("the request took %s", d)
	}
}
