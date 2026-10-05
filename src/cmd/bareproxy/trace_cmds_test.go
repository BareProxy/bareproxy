// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bareproxy/internal/bp"
)

func TestTailLine(t *testing.T) {
	cases := []struct {
		rec  bp.Record
		want string
	}{
		{bp.Record{ID: "7f3a9c0d12e4b5a6", Time: "2026-10-01T14:03:22.418Z", Method: "GET", Host: "example.com", Path: "/api/orders",
			Status: 200, Outcome: "ok", MS: 38.2, Line: 8, Pool: "api"},
			"14:03:22.418  7f3a9c0d12e4b5a6  GET  example.com/api/orders  200  ok  38 ms  line 8 pool api"},
		{bp.Record{ID: "1b2c3d4e5f607182", Time: "2026-10-01T14:03:22.502Z", Method: "POST", Host: "example.com", Path: "/",
			Status: 405, Outcome: "file", MS: 0.24, Line: 11, Folder: "/var/www/public"},
			"14:03:22.502  1b2c3d4e5f607182  POST example.com/  405  file  0.2 ms  line 11 files /var/www/public"},
		{bp.Record{ID: "0123456789abcdef", Time: "2026-10-01T14:03:23.000Z", Method: "GET", Host: "example.com", Path: "/x",
			Outcome: "client_gone", MS: 1500, Line: 9},
			"14:03:23.000  0123456789abcdef  GET  example.com/x  -  client_gone  1500 ms  line 9"},
		{bp.Record{ID: "fedcba9876543210", Time: "2026-10-01T14:03:24.000Z", Method: "GET", Host: "other.org", Path: "/",
			Status: 421, Outcome: "no_site", MS: 0.05},
			"14:03:24.000  fedcba9876543210  GET  other.org/  421  no_site  0.1 ms  -"},
	}
	for _, c := range cases {
		if got := tailLine(&c.rec); got != c.want {
			t.Errorf("tailLine:\n got %q\nwant %q", got, c.want)
		}
	}
}

func TestAgeAndCount(t *testing.T) {
	for secs, want := range map[float64]string{5.9: "5s", 75: "1m 15s", 3700: "1h 1m", 90000: "1d 1h"} {
		if got := age(secs); got != want {
			t.Errorf("age(%v) = %q, want %q", secs, got, want)
		}
	}
	if count(1, "rule") != "1 rule" || count(0, "rule") != "0 rules" || count(3, "proxy error") != "3 proxy errors" {
		t.Errorf("count is wrong")
	}
}

// fakeAdmin serves h on a Unix socket in a temporary folder and returns its path.
func fakeAdmin(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func TestTraceWhyStates(t *testing.T) {
	rec := bp.Record{ID: "abcdef0123456789", Method: "GET", Host: "h", Path: "/p", Status: 200, Outcome: "ok"}
	js, _ := json.Marshal(rec)
	sock := fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("id") {
		case "abcdef":
			w.Write(js)
		case "aaaaaa":
			http.Error(w, "2 requests have IDs starting aaaaaa; give more characters", http.StatusConflict)
		case "abc":
			http.Error(w, "give at least 6 characters of the request ID", http.StatusBadRequest)
		default:
			http.Error(w, "no request with an ID starting "+r.URL.Query().Get("id")+" in memory", http.StatusNotFound)
		}
	})
	if got, asked, err := traceWhy(sock, "abcdef"); err != nil || !asked || got == nil || got.ID != rec.ID {
		t.Errorf("a request the server has: %+v, asked=%v, err=%v", got, asked, err)
	}
	if got, asked, err := traceWhy(sock, "ffffff"); err != nil || !asked || got != nil {
		t.Errorf("a request it hasn't: %+v, asked=%v, err=%v (want nothing, asked, no error, so the file is tried)", got, asked, err)
	}
	if _, asked, err := traceWhy(sock, "aaaaaa"); err == nil || !asked || !strings.Contains(err.Error(), "give more characters") {
		t.Errorf("an ambiguous prefix: asked=%v, err=%v", asked, err)
	}
	if _, asked, err := traceWhy(sock, "abc"); err == nil || !asked {
		t.Errorf("a short prefix: asked=%v, err=%v", asked, err)
	}
	for _, none := range []string{filepath.Join(t.TempDir(), "nothing.sock"), "off", ""} {
		if got, asked, err := traceWhy(none, "abcdef"); got != nil || asked || err != nil {
			t.Errorf("no server at %q: %+v, asked=%v, err=%v (want nothing, not asked, no error)", none, got, asked, err)
		}
	}
}

func captureStdout(t *testing.T, f func() int) (string, int) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	code := f()
	w.Close()
	os.Stdout = old
	return <-done, code
}

func TestWhyAsksMemoryBeforeTheLogFile(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "requests.log")
	inMemory := bp.Record{ID: "abcdef0123456789", Time: "2026-10-01T14:03:22.418Z", Method: "GET", Scheme: "http", Host: "memory.example", Path: "/from-memory", Status: 200, Outcome: "ok"}
	inFile := bp.Record{ID: "abcdef0123456789", Time: "2026-10-01T14:03:22.418Z", Method: "GET", Scheme: "http", Host: "file.example", Path: "/from-file", Status: 200, Outcome: "ok"}
	onlyFile := bp.Record{ID: "123456fffffffff0", Time: "2026-10-01T14:03:22.418Z", Method: "GET", Scheme: "http", Host: "file.example", Path: "/only-file", Status: 200, Outcome: "ok"}
	var lines []string
	for _, r := range []bp.Record{inFile, onlyFile} {
		b, _ := json.Marshal(r)
		lines = append(lines, string(b))
	}
	if err := os.WriteFile(logFile, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	memJS, _ := json.Marshal(inMemory)
	sock := fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(inMemory.ID, r.URL.Query().Get("id")) {
			w.Write(memJS)
			return
		}
		http.Error(w, "not in memory", http.StatusNotFound)
	})
	conf := filepath.Join(dir, "bareproxy.conf")
	src := "global\n  admin " + sock + "\n  trace-log " + logFile + "\nsite http://example.com:8080\n  route /* -> respond 200 \"x\"\n"
	if err := os.WriteFile(conf, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := captureStdout(t, func() int { return why([]string{"--config", conf, "abcdef"}) })
	if code != 0 || !strings.Contains(out, "memory.example/from-memory") || strings.Contains(out, "file.example") {
		t.Errorf("a request in both: exit %d, output %q, want the one from memory", code, out)
	}
	out, code = captureStdout(t, func() int { return why([]string{"--config", conf, "123456"}) })
	if code != 0 || !strings.Contains(out, "file.example/only-file") {
		t.Errorf("a request only in the file: exit %d, output %q", code, out)
	}
	out, code = captureStdout(t, func() int { return why([]string{"--config", conf, "--json", "123456"}) })
	var got bp.Record
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || got.ID != onlyFile.ID {
		t.Errorf("--json: exit %d, output %q", code, out)
	}
	if _, code = captureStdout(t, func() int { return why([]string{"--config", conf, "999999"}) }); code != 1 {
		t.Errorf("an unknown request: exit %d, want 1", code)
	}
}

func TestPrintStatus(t *testing.T) {
	st := bp.Status{Version: "0.1.0-dev (first cut)", ConfigFile: "/etc/bareproxy/bareproxy.conf", ConfigVersion: 12,
		Started: "2026-10-01T06:00:00Z", UptimeSeconds: 3725,
		Listeners:    []bp.ListenerStatus{{Port: 80, Sites: []string{"example.com"}}, {Port: 443, TLS: true, Sites: []string{"example.com"}}},
		Sites:        []bp.SiteStatus{{Name: "example.com", Line: 5, Rules: 1, Addresses: []string{"https://example.com"}}},
		Certificates: []bp.CertStatus{{Site: "example.com", Subject: "CN=example.com", NotAfter: "2026-12-01T00:00:00Z", DaysLeft: 57}, {Site: "old.example", Subject: "CN=old.example", NotAfter: "2026-09-30T00:00:00Z", DaysLeft: -5}},
		Pools: []bp.PoolStatus{{Name: "api", Line: 14, Checks: "GET /healthz every 5s, timeout 2s, pass on 200 to 399", Up: 1, Size: 2, Backends: []bp.BackendStatus{
			{Addr: "10.0.0.11:8080", State: "up", Since: "2026-10-01T06:00:05Z", InFlight: 2},
			{Addr: "10.0.0.13:8080", State: "down", Since: "2026-10-01T07:02:10Z", Failures: 3, Reason: "connect refused"}}}}}
	st.Requests.Last1m = bp.Rate{Window: bp.Window{Requests: 1204, Status5xx: 3, ProxyErrors: 1}, Complete: true}
	st.Requests.Last5m = bp.Rate{Window: bp.Window{Requests: 6100, Status5xx: 9, ProxyErrors: 4}}
	st.Requests.Ring.Records, st.Requests.Ring.Limit, st.Requests.Ring.Oldest = 6100, 32<<20, "2026-10-01T07:03:00.000Z"
	out, _ := captureStdout(t, func() int { printStatus(&st); return 0 })
	for _, want := range []string{
		"BareProxy 0.1.0-dev (first cut), up 1h 2m (since 2026-10-01 06:00:00 UTC)",
		"Config /etc/bareproxy/bareproxy.conf, version 12",
		"  :80 http  example.com", "  :443 https  example.com",
		"  example.com (line 5), 1 rule: https://example.com",
		"  api (line 14): 1 of 2 up. Checks: GET /healthz every 5s",
		"    10.0.0.11:8080         up since 06:00:05 UTC, 2 in flight",
		"    10.0.0.13:8080         down since 07:02:10 UTC, 3 failures in a row (connect refused)",
		"  example.com  CN=example.com  ends 2026-12-01, 57 days left",
		"  old.example  CN=old.example  ends 2026-09-30, expired 5 days ago",
		"Requests (the 6100 most recent, held in memory, back to 07:03:00 UTC)",
		"  last minute     1204 requests, 3 with status 5xx, 1 proxy error\n",
		"  last 5 minutes  6100 requests or more (the ring has wrapped), 9 with status 5xx, 4 proxy errors",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status text lacks %q:\n%s", want, out)
		}
	}
	st.Requests.Ring.Limit = 0
	out, _ = captureStdout(t, func() int { printStatus(&st); return 0 })
	if !strings.Contains(out, "Requests are not counted, because trace-memory is off") || strings.Contains(out, "last minute") {
		t.Errorf("with trace-memory off:\n%s", out)
	}
}
