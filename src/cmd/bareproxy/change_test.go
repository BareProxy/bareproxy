// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// adminConfig writes a config that names sock as its admin socket and
// returns its path.
func adminConfig(t *testing.T, sock string) string {
	t.Helper()
	conf := filepath.Join(t.TempDir(), "bareproxy.conf")
	src := "global\n  admin " + sock + "\nsite http://example.com:8080\n  route /* -> respond 200 \"x\"\n"
	if err := os.WriteFile(conf, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return conf
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

func TestApplyShowsThePlanThenApplies(t *testing.T) {
	noTerminal(t)
	var applied string
	sock := fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/plan":
			io.WriteString(w, `{"plan_id":"p1","running":3,"unchanged":false,"warnings":["line 4: a warning"],"text":"+ site x\n"}`)
		case "/apply":
			applied = r.Method + " " + r.URL.RawQuery
			io.WriteString(w, "Applied: version 4\n")
		}
	})
	conf := adminConfig(t, sock)
	out, code := captureStdout(t, func() int { return applyCmd([]string{conf, "--yes"}) })
	want := "Compared with running version 3:\n+ site x\nline 4: a warning\nApplied: version 4\n"
	if code != 0 || out != want || applied != "POST plan=p1" {
		t.Errorf("apply --yes: exit %d, output %q, request %q; want output %q and the request POST plan=p1", code, out, applied, want)
	}
	applied = ""
	if _, code = captureStdout(t, func() int { return applyCmd([]string{conf, "--yes", "--plan", "other"}) }); code != 1 || applied != "" {
		t.Errorf("a plan ID that doesn't match: exit %d, request %q; want exit 1 and nothing applied", code, applied)
	}
	if _, code = captureStdout(t, func() int { return applyCmd([]string{conf}) }); code != 1 || applied != "" {
		t.Errorf("no --yes and no terminal: exit %d, request %q; want exit 1 and nothing applied", code, applied)
	}
}

func TestApplyWithNoChanges(t *testing.T) {
	applied := false
	sock := fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) {
		applied = applied || r.URL.Path == "/apply"
		io.WriteString(w, `{"plan_id":"p1","running":3,"unchanged":true,"text":""}`)
	})
	conf := adminConfig(t, sock)
	out, code := captureStdout(t, func() int { return applyCmd([]string{conf, "--yes"}) })
	if want := "Compared with running version 3:\nNo changes: version 3 keeps running.\n"; code != 0 || out != want || applied {
		t.Errorf("unchanged: exit %d, output %q, applied %v; want output %q and nothing applied", code, out, applied, want)
	}
	out, code = captureStdout(t, func() int { return applyCmd([]string{conf, "--yes", "--json"}) })
	if want := "{\"version\": 3, \"unchanged\": true}\n"; code != 0 || out != want || applied {
		t.Errorf("unchanged, --json: exit %d, output %q, applied %v; want output %q and nothing applied", code, out, applied, want)
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
		run  func([]string) int
		args []string
		want int
	}{
		{rollbackCmd, []string{"--config", conf}, 0},
		{rollbackCmd, []string{"--config", conf, "2", "--json"}, 0},
		{rollbackCmd, []string{"-c", conf, "9"}, 1},
		{historyCmd, []string{"--config", conf, "--json"}, 0},
		{historyCmd, []string{"--config", conf, "extra"}, 2},
		{rollbackCmd, []string{"--config", conf, "--bogus"}, 2},
	} {
		if _, code := captureStdout(t, func() int { return c.run(c.args) }); code != c.want {
			t.Errorf("%q: exit %d, want %d", c.args, code, c.want)
		}
	}
	want := []string{"POST /rollback?", "POST /rollback?json=1&version=2", "POST /rollback?version=9", "GET /history?json=1"}
	if len(got) != len(want) {
		t.Fatalf("requests %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d is %q, want %q", i, got[i], want[i])
		}
	}
}
