// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// nullWriter is a ResponseWriter that throws the response away, so the
// benchmarks below measure the server's own work and not a recorder's.
type nullWriter struct{ h http.Header }

func (w *nullWriter) Header() http.Header         { return w.h }
func (w *nullWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *nullWriter) WriteHeader(int)             {}

// benchHandler serves the four cases of live/bench (the home page, a 72 KB
// file, a 404 with the site's error page and a proxied API call) from the
// same config, with the trace log off and trace memory at its default.
func benchHandler(b *testing.B, path string, want int) {
	dir := b.TempDir()
	for name, size := range map[string]int{"index.html": 11596, "images/plan-demo.png": 72732, "404.html": 4686} {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Repeat("x", size)), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"orders":[{"id":1,"item":"widget","qty":2},{"id":2,"item":"gadget","qty":1}],"path":"`+r.URL.Path+`","note":"testapi"}`)
	}))
	defer api.Close()
	src := "global\n  trace-log off\nsite http://bench.local:8080\n  error 404 /404.html\n  route /api/* -> api strip\n  route /* -> files " +
		dir + "\n\npool api\n  backend " + strings.TrimPrefix(api.URL, "http://") + "\n"
	c, probs := Parse(filepath.Join(dir, "bench.conf"), src)
	if HasErrors(probs) {
		b.Fatal(probs)
	}
	defer c.Close()
	rt, err := NewRuntime(c, nil, 1, nil, false)
	if err != nil {
		b.Fatal(err)
	}
	h := NewServer("bench.conf", rt).Handler(8080, false)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host, req.RemoteAddr = "bench.local:8080", "127.0.0.1:40000"
	w := &nullWriter{h: http.Header{}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		clear(w.h)
		h.ServeHTTP(w, req.Clone(req.Context()))
	}
	b.StopTimer()
	if js, n := rt.Mem.Find(""); n == 0 || !strings.Contains(string(js), `"status":`+strconv.Itoa(want)) {
		b.Fatalf("last of %d records: %s", n, js)
	}
}

func BenchmarkHandlerHome(b *testing.B) { benchHandler(b, "/", 200) }
func BenchmarkHandlerFile(b *testing.B) { benchHandler(b, "/images/plan-demo.png", 200) }
func BenchmarkHandler404(b *testing.B)  { benchHandler(b, "/no-such-page/", 404) }
func BenchmarkHandlerAPI(b *testing.B)  { benchHandler(b, "/api/orders", 200) }
