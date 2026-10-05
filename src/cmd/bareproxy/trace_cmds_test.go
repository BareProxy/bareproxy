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
