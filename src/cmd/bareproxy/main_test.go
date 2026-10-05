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
	"reflect"
	"strings"
	"sync"
	"testing"

	"bareproxy/internal/bp"
)

// capture runs f with os.Stdout and os.Stderr redirected, and returns what it
// wrote to each and the exit code.
func capture(t *testing.T, f func() int) (stdout, stderr string, code int) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
	var got [2]string
	var writers [2]*os.File
	var wg sync.WaitGroup
	for i := range writers {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		writers[i] = w
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, _ := io.ReadAll(r)
			got[i] = string(b)
		}()
	}
	os.Stdout, os.Stderr = writers[0], writers[1]
	code = f()
	writers[0].Close()
	writers[1].Close()
	wg.Wait()
	return got[0], got[1], code
}

// runCmd runs one command line and returns what it wrote and its exit code.
func runCmd(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	return capture(t, func() int { return execute(args) })
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

// writeConfig writes a config with the global settings in global (one per
// line) and returns its path.
func writeConfig(t *testing.T, global ...string) string {
	t.Helper()
	conf := filepath.Join(t.TempDir(), "bareproxy.conf")
	src := "global\n  " + strings.Join(global, "\n  ") + "\nsite http://example.com:8080\n  route /* -> respond 200 \"x\"\n"
	if err := os.WriteFile(conf, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return conf
}

// adminConfig writes a config that names sock as its admin socket.
func adminConfig(t *testing.T, sock string) string {
	t.Helper()
	return writeConfig(t, "admin "+sock)
}

// noTerminal makes os.Stdin an empty file for the test, so apply can't ask
// a question even when the tests run from a terminal.
func noTerminal(t *testing.T) {
	t.Helper()
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = null
	t.Cleanup(func() {
		os.Stdin = old
		null.Close()
	})
}

func TestParse(t *testing.T) {
	const names = "--json -y -c --config -H"
	cases := []struct {
		what string
		args []string
		want options
		ok   bool
	}{
		{"options in any position", []string{"GET", "-H", "A: 1", "http://x/", "--json", "-H", "B: 2"},
			options{json: true, headers: []string{"A: 1", "B: 2"}, pos: []string{"GET", "http://x/"}}, true},
		{"short and long spellings, the last one wins", []string{"-c", "a.conf", "--config", "b.conf", "-y"},
			options{config: "b.conf", yes: true}, true},
		{"a value may start with a dash", []string{"-c", "-odd.conf"}, options{config: "-odd.conf"}, true},
		{"an unknown option is refused", []string{"x", "--bogus"}, options{}, false},
		{"an option of another command is refused", []string{"--offline"}, options{}, false},
		{"an option without its value is refused", []string{"x", "--config"}, options{}, false},
	}
	for _, c := range cases {
		got, err := parse(c.args, names)
		if (err == nil) != c.ok || (c.ok && !reflect.DeepEqual(got, c.want)) {
			t.Errorf("%s: parse(%q) = %+v, %v; want %+v, ok %v", c.what, c.args, got, err, c.want, c.ok)
		}
	}
}

func TestBadUsageHasOneWording(t *testing.T) {
	for _, c := range []struct {
		args []string
		msg  string
	}{
		{nil, "no command given"},
		{[]string{"frobnicate"}, `unknown command "frobnicate"`},
		{[]string{"status", "--bogus"}, "unknown option --bogus"},
		{[]string{"explain", "--offline", "-H"}, "-H needs a value"},
		{[]string{"why", "--config"}, "--config needs a value"},
		{[]string{"explain", "GET"}, "wrong number of arguments"},
		{[]string{"history", "extra"}, "wrong number of arguments"},
		{[]string{"check", "a.conf", "b.conf"}, "wrong number of arguments"},
		{[]string{"tail", "status"}, `filter "status" needs a key`},
	} {
		out, errOut, code := runCmd(t, c.args...)
		if code != 2 || out != "" || !strings.HasPrefix(errOut, "bareproxy: "+c.msg) || !strings.Contains(errOut, "\nusage:\n") {
			t.Errorf("%q: exit %d, stdout %q, stderr %q; want exit 2, nothing on stdout, and %q then the usage", c.args, code, out, errOut, "bareproxy: "+c.msg)
		}
	}
	for _, help := range []string{"help", "-h", "--help"} {
		if out, errOut, code := runCmd(t, help); code != 0 || out != "" || !strings.HasPrefix(errOut, "usage:\n") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want the usage and exit 0", help, code, out, errOut)
		}
	}
	if out, _, code := runCmd(t, "version"); code != 0 || !strings.HasPrefix(out, "bareproxy ") {
		t.Errorf("version: exit %d, stdout %q", code, out)
	}
}

func TestNoServerHasOneWording(t *testing.T) {
	noTerminal(t)
	sock := filepath.Join(t.TempDir(), "none.sock")
	conf := adminConfig(t, sock)
	want := "bareproxy: no BareProxy is running on " + sock + "\n"
	for _, args := range [][]string{
		{"status", "-c", conf}, {"events", "-c", conf}, {"history", "-c", conf}, {"rollback", "-c", conf},
		{"tail", "-c", conf}, {"plan", conf}, {"apply", "--yes", conf},
	} {
		if out, errOut, code := runCmd(t, args...); code != 1 || out != "" || errOut != want {
			t.Errorf("%q: exit %d, stdout %q, stderr %q; want exit 1 and only %q", args, code, out, errOut, want)
		}
	}
}

func TestConfigThatCantFindTheServer(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone.conf")
	for _, cmd := range []string{"status", "rollback"} {
		out, errOut, code := runCmd(t, cmd, "-c", gone)
		if code != 1 || out != "" || !strings.HasPrefix(errOut, "bareproxy: can't read the config: ") || strings.Contains(errOut, "usage:") {
			t.Errorf("%s with no config file: exit %d, stdout %q, stderr %q", cmd, code, out, errOut)
		}
	}
	_, errOut, code := runCmd(t, "status", "-c", writeConfig(t, "admin off"))
	if want := "bareproxy: the config turns the admin socket off, so there is no server to ask\n"; code != 1 || errOut != want {
		t.Errorf("admin off: exit %d, stderr %q, want %q", code, errOut, want)
	}
}

func TestAdminErrorsHaveOneWording(t *testing.T) {
	sock := fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("json") == "1" {
			http.Error(w, `{"error": "no version 9"}`, http.StatusNotFound)
		} else {
			http.Error(w, "no version 9", http.StatusNotFound)
		}
	})
	conf := adminConfig(t, sock)
	out, errOut, code := runCmd(t, "rollback", "-c", conf, "9")
	if code != 1 || out != "" || errOut != "bareproxy: no version 9\n" {
		t.Errorf("text: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	out, errOut, code = runCmd(t, "rollback", "-c", conf, "9", "--json")
	if code != 1 || out != "{\"error\": \"no version 9\"}\n" || errOut != "" {
		t.Errorf("--json: exit %d, stdout %q, stderr %q; want the server's JSON on stdout", code, out, errOut)
	}
}

func TestApplyShowsThePlanThenApplies(t *testing.T) {
	noTerminal(t)
	var applied []string
	sock := fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/plan":
			io.WriteString(w, `{"plan_id":"p1","running":3,"unchanged":false,"warnings":["line 4: a warning"],"text":"+ site x\n"}`)
		case "/apply":
			applied = append(applied, r.Method+" "+r.URL.RawQuery)
			io.WriteString(w, "Version 4 is running (it was 3).\n")
		}
	})
	conf := adminConfig(t, sock)
	out, _, code := runCmd(t, "apply", conf, "--yes")
	want := "Compared with running version 3:\n+ site x\nline 4: a warning\nVersion 4 is running (it was 3).\n"
	if code != 0 || out != want || !reflect.DeepEqual(applied, []string{"POST plan=p1"}) {
		t.Errorf("apply --yes: exit %d, output %q, requests %q; want output %q and one request, POST plan=p1", code, out, applied, want)
	}
	applied = nil
	out, errOut, code := runCmd(t, "apply", conf, "--yes", "--json")
	if code != 0 || out != "Version 4 is running (it was 3).\n" || errOut != "" || !reflect.DeepEqual(applied, []string{"POST json=1&plan=p1"}) {
		t.Errorf("apply --yes --json: exit %d, stdout %q, stderr %q, requests %q; want the reply alone on stdout", code, out, errOut, applied)
	}
	applied = nil
	_, errOut, code = runCmd(t, "apply", conf, "--yes", "--plan", "other")
	if want := "bareproxy: plan other doesn't match what " + conf + " would do now (that is plan p1), so nothing was applied; see the new plan with: bareproxy plan " + conf + "\n"; code != 1 || errOut != want || applied != nil {
		t.Errorf("a plan ID that doesn't match: exit %d, stderr %q, requests %q; want exit 1, %q and nothing applied", code, errOut, applied, want)
	}
	out, _, code = runCmd(t, "apply", conf, "--yes", "--plan", "other", "--json")
	if code != 1 || !strings.HasPrefix(out, "{\n  \"error\": \"plan other doesn't match") || applied != nil {
		t.Errorf("a plan ID that doesn't match, --json: exit %d, stdout %q, requests %q", code, out, applied)
	}
	_, errOut, code = runCmd(t, "apply", conf)
	if !strings.Contains(errOut, "there is no terminal to ask in; add --yes") || code != 1 || applied != nil {
		t.Errorf("no --yes and no terminal: exit %d, stderr %q, requests %q; want exit 1 and nothing applied", code, errOut, applied)
	}
}

func TestApplyOfAnUnchangedTextStillReachesTheServer(t *testing.T) {
	noTerminal(t)
	var applied []string
	sock := fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/plan":
			io.WriteString(w, `{"plan_id":"p1","running":3,"unchanged":true,"text":"No changes.\n"}`)
		case "/apply":
			applied = append(applied, r.URL.RawQuery)
			io.WriteString(w, "No changes: version 3 keeps running.\n")
		}
	})
	conf := adminConfig(t, sock)
	// Nothing to ask about, so no --yes and no terminal is fine. The server
	// answers, because only it knows what an unchanged text does (it reloads
	// the certificate files, as SIGHUP does).
	out, _, code := runCmd(t, "apply", conf)
	want := "No changes: version 3 keeps running.\n"
	if code != 0 || out != want || !reflect.DeepEqual(applied, []string{"plan=p1"}) {
		t.Errorf("unchanged: exit %d, output %q, requests %q; want output %q and one request, plan=p1", code, out, applied, want)
	}
}

func TestRollbackAndHistoryAskTheServerOfTheConfig(t *testing.T) {
	var got []string
	sock := fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		if r.URL.Query().Get("version") == "9" {
			http.Error(w, "no version 9", http.StatusNotFound)
			return
		}
		io.WriteString(w, "reply\n")
	})
	conf := adminConfig(t, sock)
	for _, c := range []struct {
		args []string
		want int
	}{
		{[]string{"rollback", "--config", conf}, 0},
		{[]string{"rollback", "--config", conf, "2", "--json"}, 0},
		{[]string{"rollback", "-c", conf, "9"}, 1},
		{[]string{"history", "--config", conf, "--json"}, 0},
		{[]string{"history", "--config", conf, "extra"}, 2},
		{[]string{"rollback", "--config", conf, "--bogus"}, 2},
	} {
		if _, _, code := runCmd(t, c.args...); code != c.want {
			t.Errorf("%q: exit %d, want %d", c.args, code, c.want)
		}
	}
	want := []string{"POST /rollback?", "POST /rollback?json=1&version=2", "POST /rollback?version=9", "GET /history?json=1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("requests %q, want %q", got, want)
	}
}

func TestTailLine(t *testing.T) {
	cases := []struct {
		rec  bp.Record
		want string
	}{
		{bp.Record{ID: "7f3a9c0d12e4b5a6", Time: "2026-10-01T14:03:22.418Z", Method: "GET", Host: "example.com", Path: "/api/orders",
			Status: 200, Outcome: "ok", MS: 38.2, Line: 8, Rule: "route /api/* -> api", Pool: "api"},
			"14:03:22.418  7f3a9c0d12e4b5a6  GET  example.com/api/orders  200  ok  38.2 ms  line 8: route /api/* -> api"},
		{bp.Record{ID: "1b2c3d4e5f607182", Time: "2026-10-01T14:03:22.502Z", Method: "POST", Host: "example.com", Path: "/",
			Status: 405, Outcome: "file", MS: 0.24, Line: 11, Rule: "route /* -> files /var/www/public"},
			"14:03:22.502  1b2c3d4e5f607182  POST example.com/  405  file  0.2 ms  line 11: route /* -> files /var/www/public"},
		{bp.Record{ID: "0123456789abcdef", Time: "2026-10-01T14:03:23.000Z", Method: "GET", Host: "example.com", Path: "/x",
			Outcome: "client_gone", MS: 1500, Line: 9, Rule: "route /x -> respond 200 \"x\""},
			"14:03:23.000  0123456789abcdef  GET  example.com/x  -  client_gone  1500.0 ms  line 9: route /x -> respond 200 \"x\""},
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

func TestTailPrintsWhatTheServerStreams(t *testing.T) {
	rec := bp.Record{ID: "7f3a9c0d12e4b5a6", Time: "2026-10-01T14:03:22.418Z", Method: "GET", Host: "example.com", Path: "/", Status: 200, Outcome: "ok", MS: 1}
	js, _ := json.Marshal(rec)
	var query string
	sock := fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.Write(append(js, "\n{\"dropped\":3}\n"...))
	})
	conf := adminConfig(t, sock)
	out, errOut, code := runCmd(t, "tail", "-c", conf, "status>=500")
	want := tailLine(&rec) + "\n-- 3 records skipped, the reader was too slow --\n"
	if code != 1 || out != want || errOut != "bareproxy: the server closed the connection\n" || query != "f=status%3E%3D500" {
		t.Errorf("tail: exit %d, stdout %q, stderr %q, query %q; want stdout %q", code, out, errOut, query, want)
	}
	out, _, _ = runCmd(t, "tail", "-c", conf, "--json")
	if want := string(js) + "\n{\"dropped\":3}\n"; out != want {
		t.Errorf("tail --json: stdout %q, want %q", out, want)
	}
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
	conf := writeConfig(t, "admin "+sock, "trace-log "+logFile)
	out, _, code := runCmd(t, "why", "--config", conf, "abcdef")
	if code != 0 || !strings.Contains(out, "memory.example/from-memory") || strings.Contains(out, "file.example") {
		t.Errorf("a request in both: exit %d, output %q, want the one from memory", code, out)
	}
	out, _, code = runCmd(t, "why", "--config", conf, "123456")
	if code != 0 || !strings.Contains(out, "file.example/only-file") {
		t.Errorf("a request only in the file: exit %d, output %q", code, out)
	}
	out, _, code = runCmd(t, "why", "--config", conf, "--json", "123456")
	var got bp.Record
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || got.ID != onlyFile.ID {
		t.Errorf("--json: exit %d, output %q", code, out)
	}
	if _, _, code = runCmd(t, "why", "--config", conf, "999999"); code != 1 {
		t.Errorf("an unknown request: exit %d, want 1", code)
	}
	// With no server and a trace log that isn't a file, there is nowhere to look.
	_, errOut, code := runCmd(t, "why", "--config", writeConfig(t, "admin "+filepath.Join(dir, "none.sock"), "trace-log off"), "abcdef")
	if code != 1 || !strings.Contains(errOut, "no BareProxy is running on") || !strings.Contains(errOut, "so there is no file to look in") {
		t.Errorf("trace-log off, no server: exit %d, stderr %q", code, errOut)
	}
}

func TestPrintEvents(t *testing.T) {
	out, _, _ := capture(t, func() int { printEvents(nil); return 0 })
	if out != "No events yet.\n" {
		t.Errorf("no events: %q", out)
	}
	out, _, _ = capture(t, func() int {
		printEvents([]bp.Event{{Time: "2026-10-01T07:02:10Z", Kind: "backend", Text: "pool api: 10.0.0.13:8080 is down"}})
		return 0
	})
	if want := "2026-10-01T07:02:10Z  backend      pool api: 10.0.0.13:8080 is down\n"; out != want {
		t.Errorf("one event: %q, want %q", out, want)
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
	out, _, _ := capture(t, func() int { printStatus(st); return 0 })
	for _, want := range []string{
		"BareProxy 0.1.0-dev (first cut), up 1h2m5s (since 2026-10-01T06:00:00Z)",
		"Config /etc/bareproxy/bareproxy.conf, version 12",
		"Listeners (2)", "  :80 http  example.com", "  :443 https  example.com",
		"  example.com (line 5), rules: 1, https://example.com",
		"  api (line 14): 1 of 2 up. Checks: GET /healthz every 5s",
		"    10.0.0.11:8080         up since 2026-10-01T06:00:05Z, 2 in flight, 0 failures in a row",
		"    10.0.0.13:8080         down (connect refused) since 2026-10-01T07:02:10Z, 0 in flight, 3 failures in a row",
		"  example.com  CN=example.com  ends 2026-12-01, 57 days left",
		"  old.example  CN=old.example  ends 2026-09-30, -5 days left",
		"Requests (the 6100 most recent, held in memory, back to 2026-10-01T07:03:00.000Z)",
		"  last minute     requests: 1204, status 5xx: 3, proxy errors: 1\n",
		"  last 5 minutes  requests: 6100 or more (the ring has wrapped), status 5xx: 9, proxy errors: 4",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status text lacks %q:\n%s", want, out)
		}
	}
	st.Requests.Ring.Limit = 0
	out, _, _ = capture(t, func() int { printStatus(st); return 0 })
	if !strings.Contains(out, "No requests counted: trace-memory is off") || strings.Contains(out, "last minute") {
		t.Errorf("with trace-memory off:\n%s", out)
	}
}
