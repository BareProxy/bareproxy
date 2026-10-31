// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bareproxy/internal/drift/drifttest"
)

func TestDriftCommandExitCodes(t *testing.T) {
	res, err := drifttest.NewResolver()
	if err != nil {
		t.Fatal(err)
	}
	defer res.Close()
	res.Records("a.test", "A", "192.0.2.1")
	res.Records("a.test", "NS", "ns1.test")
	dir := t.TempDir()
	conf := filepath.Join(dir, "drift.conf")
	writeTestFile(t, conf, fmt.Sprintf("global\n  state %s\n\ndrift web\n  domains a.test\n  records A NS\n  tls off\n  resolver %s\n  timeout 300ms\n",
		filepath.Join(dir, "state"), res.Addr))

	out, errOut, code := runCmd(t, "drift", "-c", conf)
	if code != 0 || !strings.Contains(out, "NOTE       a.test  first look, recorded: A 192.0.2.1; NS ns1.test") || !strings.Contains(out, "Result: 0 alerts, 0 unchecked") {
		t.Fatalf("first run: %d\n%s%s", code, out, errOut)
	}
	res.Records("a.test", "A", "192.0.2.7")
	out, _, code = runCmd(t, "drift", "-c", conf)
	if code != 4 || !strings.Contains(out, "ALERT      a.test  A changed\n           before: 192.0.2.1\n           after:  192.0.2.7") {
		t.Fatalf("a change: %d\n%s", code, out)
	}
	res.Set("a.test", "NS", drifttest.Answer{Fail: "timeout"})
	out, _, code = runCmd(t, "drift", "-c", conf, "--json")
	var reps []struct {
		Unchecked []struct{ Part string }
	}
	if json.Unmarshal([]byte(out), &reps); code != 3 || len(reps) != 1 || len(reps[0].Unchecked) != 1 || reps[0].Unchecked[0].Part != "NS" {
		t.Fatalf("a timeout: %d\n%s", code, out)
	}
	out, _, code = runCmd(t, "drift", "-c", conf, "--history")
	if code != 0 || !strings.Contains(out, "a.test  A changed") || !strings.Contains(out, "after:  192.0.2.7") {
		t.Fatalf("history: %d\n%s", code, out)
	}
	if _, errOut, code = runCmd(t, "drift", "-c", conf, "nope"); code != 1 || !strings.Contains(errOut, "no drift block named nope (it has: web)") {
		t.Fatalf("unknown watch: %d %s", code, errOut)
	}
}

func TestInboxCommandUsage(t *testing.T) {
	for _, args := range [][]string{{"inbox", "bogus"}, {"inbox", "show", "x"}, {"inbox", "replay", "x"}, {"inbox", "replay", "x", "id", "--failed"}} {
		if _, errOut, code := runCmd(t, args...); code != 2 || !strings.Contains(errOut, "inbox takes list") {
			t.Errorf("%v: %d %s", args, code, errOut)
		}
	}
}

func writeTestFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}
