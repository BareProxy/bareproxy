// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestAppliesUnderLoad makes 20 config changes while clients send requests
// over HTTP/1.1 and HTTP/2, half of them proxied to a backend that every
// change swaps. No request may fail.
func TestAppliesUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("load test")
	}
	backend := func(name string) string {
		b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, name) }))
		t.Cleanup(b.Close)
		return b.Listener.Addr().String()
	}
	b1, b2 := backend("b1"), backend("b2")
	dir := t.TempDir()
	writeTestCert(t, dir)
	port, tlsPort := freePort(t), freePort(t)
	conf := func(i int) string {
		addr := b1
		if i%2 == 1 {
			addr = b2
		}
		return fmt.Sprintf(`global
  admin off
  trace-log off
  state state

site http://example.com:%d https://example.com:%d
  tls cert.pem key.pem
  route /api/* -> api
  route /* -> respond 200 "v%d"

pool api
  drain 1s
  backend %s
`, port, tlsPort, i, addr)
	}
	file := filepath.Join(dir, "bareproxy.conf")
	writeFile(t, file, conf(0))
	s, err := Start(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)

	certPEM, err := os.ReadFile(filepath.Join(dir, "cert.pem"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	dialTo := func(port int) func(context.Context, string, string) (net.Conn, error) {
		return func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, fmt.Sprintf("127.0.0.1:%d", port))
		}
	}
	clients := []struct {
		proto, base string
		c           *http.Client
	}{
		{"HTTP/1.1", fmt.Sprintf("http://example.com:%d", port), &http.Client{Transport: &http.Transport{DialContext: dialTo(port)}}},
		{"HTTP/2.0", fmt.Sprintf("https://example.com:%d", tlsPort), &http.Client{Transport: &http.Transport{
			DialContext: dialTo(tlsPort), TLSClientConfig: &tls.Config{RootCAs: roots}, ForceAttemptHTTP2: true}}},
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	var mu sync.Mutex
	counts := map[string]int{}
	var failures []string
	for _, cl := range clients {
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; !stop.Load(); i++ {
					path := "/"
					if i%2 == 1 {
						path = "/api/x"
					}
					fail := ""
					resp, err := cl.c.Get(cl.base + path)
					if err != nil {
						fail = err.Error()
					} else {
						b, _ := io.ReadAll(resp.Body)
						resp.Body.Close()
						body := string(b)
						if resp.StatusCode != 200 || resp.Proto != cl.proto || !(strings.HasPrefix(body, "v") || body == "b1" || body == "b2") {
							fail = fmt.Sprintf("%s %d %q", resp.Proto, resp.StatusCode, body)
						}
					}
					mu.Lock()
					counts[cl.proto]++
					if fail != "" {
						failures = append(failures, cl.proto+" "+path+": "+fail)
					}
					mu.Unlock()
				}
			}()
		}
	}
	time.Sleep(100 * time.Millisecond)
	for i := 1; i <= 20; i++ {
		if _, err := s.Apply(Change{Text: conf(i), How: "apply"}); err != nil {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("apply %d: %v", i, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
	t.Logf("requests while 20 applies ran: %v; failed: %d", counts, len(failures))
	if len(failures) > 0 {
		t.Fatalf("%d requests failed; the first: %s", len(failures), failures[0])
	}
	if counts["HTTP/1.1"] == 0 || counts["HTTP/2.0"] == 0 {
		t.Fatalf("a protocol sent no requests: %v", counts)
	}
	if v := s.Current().Version; v != 21 {
		t.Fatalf("running version %d after 20 applies, want 21", v)
	}
}
