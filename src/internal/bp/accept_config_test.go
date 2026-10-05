// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

// Acceptance tests, group 3: none of the broken configs goes live, and each
// error names its line (the design note, section 12).

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type accBroken struct {
	name string
	src  string
	line int    // the line the error must name; 0 for an error about the whole config
	frag string // part of the message
}

const accOKSite = "site http://a.test:8080\n  route /* -> respond 200 \"ok\"\n" // lines 1 and 2

func accBrokenConfigs() []accBroken {
	ok := accOKSite
	return []accBroken{
		// Lexing and block structure.
		{"unterminated quote", "site http://a.test:8080\n  route /* -> respond 200 \"oops\n", 2, "no closing quote"},
		{"global with an argument", "global on\n" + ok, 1, "global takes nothing"},
		{"unknown block keyword", "server a.test\n  route /* -> respond 200\n", 1, "can't start a block"},
		{"indented line before any block", "  route /* -> respond 200\n" + ok, 1, "indented line outside"},

		// Global settings.
		{"unknown global setting", "global\n  speed fast\n" + ok, 2, "unknown global setting"},
		{"admin without a value", "global\n  admin\n" + ok, 2, "admin takes one value"},
		{"trace-log with three words", "global\n  trace-log a.log 10MB\n" + ok, 2, "trace-log takes"},
		{"trace-query with yes", "global\n  trace-query yes\n" + ok, 2, "expected on or off"},
		{"id-header with maybe", "global\n  id-header maybe\n" + ok, 2, "expected on or off"},

		// Site lines.
		{"site without an address", "site\n  route /* -> respond 200\n", 1, "site needs at least one address"},
		{"port out of range", "site a.test:70000\n  route /* -> respond 200\n", 1, "bad port in address"},
		{"bad host name", "site http://bad_host.test:8080\n  route /* -> respond 200\n", 1, "bad host name"},
		{"path in the address", "site http://a.test:8080/app\n  route /* -> respond 200\n", 1, "bad port in address"},
		{"unknown site setting", ok + "  wibble on\n", 3, "unknown site setting"},
		{"tls without arguments", ok + "  tls\n", 3, "tls takes"},
		{"error with another status", ok + "  error 500 /500.html\n", 3, "only error 404"},
		{"error page without a leading slash", ok + "  error 404 404.html\n", 3, "plain file path"},
		{"error page that is a folder", ok + "  error 404 /pages/\n", 3, "plain file path"},
		{"encoded-slashes with maybe", ok + "  encoded-slashes maybe\n", 3, "encoded-slashes takes"},
		{"body-limit with a word", ok + "  body-limit lots\n", 3, "bad size"},

		// Route lines.
		{"route without an arrow", ok + "  route /x respond 200\n", 3, "route needs ->"},
		{"route without a path", ok + "  route -> respond 200\n", 3, "route needs a path"},
		{"method in lowercase", ok + "  route get /x -> respond 200\n", 3, "isn't a method"},
		{"methods without a path", ok + "  route GET -> respond 200\n", 3, "path after the methods"},
		{"double slash in a path", ok + "  route /a//b -> respond 200\n", 3, "normal form"},
		{"dot segment in a path", ok + "  route /a/../b -> respond 200\n", 3, "normal form"},
		{"star in the middle of a path", ok + "  route /a/*/b -> respond 200\n", 3, "normal form"},
		{"prefix with a trailing slash", ok + "  route /a//* -> respond 200\n", 3, "normal form"},
		{"word after the path", ok + "  route /x foo -> respond 200\n", 3, "unexpected"},
		{"header without a name", ok + "  route /x header -> respond 200\n", 3, "unexpected"},
		{"bad header name", ok + "  route /x header X_Bad=1 -> respond 200\n", 3, "isn't a header name"},
		{"no action", ok + "  route /x ->\n", 3, "route needs an action"},
		{"files without a folder", ok + "  route /x -> files\n", 3, "files takes one folder"},
		{"files with two folders", ok + "  route /x -> files public other\n", 3, "files takes one folder"},
		{"redirect with code 303", ok + "  route /x -> redirect 303 https://example.org\n", 3, "redirect code must be"},
		{"redirect without a scheme", ok + "  route /x -> redirect 301 example.org\n", 3, "full http"},
		{"respond with status 99", ok + "  route /x -> respond 99\n", 3, "respond needs a status"},
		{"respond with too many words", ok + "  route /x -> respond 200 \"a\" \"b\"\n", 3, "respond takes"},
		{"junk after a pool name", "site http://a.test:8080\n  route /* -> respond 200\n  route /x -> api weird\npool api\n  backend 127.0.0.1:9\n", 3, "after a pool name"},
		{"action that is no name", ok + "  route /x -> bad!name\n", 3, "isn't an action"},
		{"route to a pool that doesn't exist", ok + "  route /x -> nopool\n", 3, "no pool named nopool"},
		{"files folder that doesn't exist", ok + "  route /x -> files nowhere\n", 3, "can't open folder"},
		{"files folder that is a file", ok + "  route /x -> files afile\n", 3, "can't open folder"},

		// Pool blocks.
		{"pool without a name", ok + "pool\n  backend 127.0.0.1:9\n", 3, "pool needs one name"},
		{"pool named files", ok + "pool files\n  backend 127.0.0.1:9\n", 3, "pool needs one name"},
		{"pool defined twice", ok + "pool api\n  backend 127.0.0.1:9\npool api\n  backend 127.0.0.1:10\n", 5, "already defined on line 3"},
		{"pool without backends", "site http://a.test:8080\n  route /* -> api\npool api\n", 3, "has no backends"},
		{"backend without a port", ok + "pool api\n  backend 10.0.0.1\n", 4, "host:port"},
		{"backend port out of range", ok + "pool api\n  backend 10.0.0.1:99999\n", 4, "bad backend port"},
		{"backend listed twice", ok + "pool api\n  backend 10.0.0.1:80\n  backend 10.0.0.1:80\n", 5, "already listed on line 4"},
		{"unix backend", ok + "pool api\n  backend unix:/tmp/x.sock\n", 4, "unix: backends"},
		{"health without a path", ok + "pool api\n  backend 10.0.0.1:80\n  health\n", 5, "health takes a path"},
		{"unknown health option", ok + "pool api\n  backend 10.0.0.1:80\n  health /h speed 5s\n", 5, "unknown health option"},
		{"health option without a value", ok + "pool api\n  backend 10.0.0.1:80\n  health /h every\n", 5, "needs a value"},
		{"health status range out of bounds", ok + "pool api\n  backend 10.0.0.1:80\n  health /h expect 700-800\n", 5, "status range"},
		{"retries 5", ok + "pool api\n  backend 10.0.0.1:80\n  retries 5\n", 5, "retries takes"},
		{"timeout that isn't a duration", ok + "pool api\n  backend 10.0.0.1:80\n  connect-timeout soon\n", 5, "bad duration"},
		{"unknown pool setting", ok + "pool api\n  backend 10.0.0.1:80\n  weight 3\n", 5, "unknown pool setting"},
		{"host-header without a value", ok + "pool api\n  backend 10.0.0.1:80\n  host-header\n", 5, "host-header takes"},

		// Things only the whole file shows.
		{"https site without a certificate", "site b.test:8443\n  route /* -> respond 200\n", 1, "automatic certificates"},
		{"certificate files that don't exist", "site b.test:8443\n  tls nothere.pem nothere.key\n  route /* -> respond 200\n", 2, "can't load the certificate"},
		{"same host and port in two sites", ok + "site http://a.test:8080\n  route /* -> respond 200\n", 3, "already belongs to the site on line 1"},
		{"same wildcard in two sites", "site http://*.a.test:8080\n  route /* -> respond 200\nsite http://*.a.test:8080\n  route /* -> respond 200\n", 3, "already belongs"},
		{"two catch-all sites on a port", "site http://*:8080\n  route /* -> respond 200\nsite http://*:8080\n  route /* -> respond 200\n", 3, "already belongs"},
		{"http and https on one port", ok + "site b.test:8080\n  route /* -> respond 200\n", 3, "can't be both http and https"},
		{"no sites at all", "# nothing here\n", 0, "no sites"},
		{"empty file", "", 0, "no sites"},
	}
}

// accNoStderr runs f with the process's standard error pointed at /dev/null,
// for the calls that log their problems there.
func accNoStderr(t *testing.T, f func()) {
	t.Helper()
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = null
	defer func() { os.Stderr = old; null.Close() }()
	f()
}

// TestAcceptBrokenConfigs is group 3. Each broken config must fail to
// compile with an error that names the line of the mistake; a running server
// must refuse it on reload and keep its version; and the server must refuse
// to start with it.
func TestAcceptBrokenConfigs(t *testing.T) {
	dir := t.TempDir()
	accWrite(t, dir+"/public/index.html", "x")
	accWrite(t, dir+"/afile", "not a folder")
	conf := filepath.Join(dir, "bareproxy.conf")

	// Controls: a good config parses clean, so the table fails for its
	// mistakes and for nothing else.
	for _, src := range []string{accOKSite, accOKSite + "  route /files/* -> files public\n  route /api/* -> api strip\npool api\n  backend 127.0.0.1:9\n  health /h every 1s\n"} {
		c, probs := Parse(conf, src)
		if HasErrors(probs) {
			t.Errorf("a good config fails to compile: %v\n%s", probs, src)
		}
		c.Close()
	}

	// A running server on a good config.
	accWrite(t, conf, "global\n  admin off\n  trace-log "+filepath.Join(dir, "good.log")+"\n"+accOKSite)
	c, probs := Load(conf)
	if HasErrors(probs) {
		t.Fatalf("the running config: %v", probs)
	}
	rt, err := NewRuntime(c, nil, 1, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(conf, rt)
	srv.logger.SetOutput(io.Discard)
	t.Cleanup(func() { rt.Stop(); rt.Trace.Close(); c.Close() })

	cases := accBrokenConfigs()
	if len(cases) < 50 {
		t.Fatalf("the table has %d broken configs, want at least 50", len(cases))
	}
	seen := map[string]bool{}
	named, wholeFile := 0, 0
	for _, bc := range cases {
		if seen[bc.name] {
			t.Errorf("two cases are called %q", bc.name)
		}
		seen[bc.name] = true

		c, probs := Parse(conf, bc.src)
		if c != nil {
			c.Close()
		}
		if !HasErrors(probs) {
			t.Errorf("%s: compiles without errors (problems: %v)\n%s", bc.name, probs, bc.src)
			continue
		}
		var hit *Problem
		var lines []int
		for i := range probs {
			p := probs[i]
			if p.Warn {
				continue
			}
			lines = append(lines, p.Line)
			if p.Line == bc.line && strings.Contains(p.Msg, bc.frag) {
				hit = &probs[i]
			}
		}
		switch {
		case hit == nil:
			t.Errorf("%s: no error on line %d mentioning %q; errors are %v\n%s", bc.name, bc.line, bc.frag, probs, bc.src)
		case bc.line > 0 && !strings.HasPrefix(hit.String(), fmt.Sprintf("line %d: error: ", bc.line)):
			t.Errorf("%s: the message doesn't start with its line: %q", bc.name, hit.String())
		}
		if bc.line > 0 {
			named++
		} else {
			wholeFile++
		}

		// It never goes live: a running server keeps its version.
		accWrite(t, conf, bc.src)
		srv.Reload()
		if srv.Current() != rt || srv.Current().Version != 1 {
			t.Errorf("%s: a reload replaced the running config (now version %d)", bc.name, srv.Current().Version)
		}

		// And it doesn't start.
		done := make(chan error, 1)
		accNoStderr(t, func() {
			go func() { done <- Run(conf) }()
			select {
			case err := <-done:
				if err == nil {
					t.Errorf("%s: Run returned without an error", bc.name)
				}
			case <-time.After(3 * time.Second):
				t.Errorf("%s: Run didn't refuse the config; it is probably serving", bc.name)
			}
		})
	}
	t.Logf("%d broken configs: %d name a line, %d are about the whole file; every one is an error from Parse, is refused by Reload on a running server, and stops Run from starting", len(cases), named, wholeFile)
}
