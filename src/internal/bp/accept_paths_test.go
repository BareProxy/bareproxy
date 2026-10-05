// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

// Acceptance tests, group 4: paths, traversal and files (design note,
// sections 4 and 12). A files folder has bait next to it and symlinks that
// stay inside, point outside and are absolute. No generated or hand-written
// case may read anything outside the folder, hidden names are refused except
// under /.well-known/, and conditional and range requests follow HTTP.

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	accBaitSecret  = "BAIT-SECRET outside the folder"
	accBaitIndex   = "BAIT-INDEX outside the folder"
	accBaitSibling = "BAIT-SIBLING next to the folder"
)

type accPathsFx struct {
	dir, site, bait, logPath string
	h                        http.Handler
	content                  []byte    // a.txt, 1000 bytes
	mtime                    time.Time // its modification time
}

// accBuildPaths makes the folder, the bait and the symlinks, and a server with
// two sites on the folder: files.test (the default rules) and keep.test
// (encoded-slashes keep).
func accBuildPaths(t *testing.T) *accPathsFx {
	t.Helper()
	dir := t.TempDir()
	f := &accPathsFx{dir: dir, site: filepath.Join(dir, "site"), bait: filepath.Join(dir, "bait"), logPath: filepath.Join(dir, "paths.log")}
	s := f.site
	f.content = make([]byte, 1000)
	for i := range f.content {
		f.content[i] = byte('a' + i%26)
	}
	f.mtime = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	files := map[string]string{
		"index.html":                "<h1>site home</h1>",
		"404.html":                  "<h1>site not found</h1>",
		"a.txt":                     string(f.content),
		"docs/readme.txt":           "readme",
		"sub/index.html":            "<h1>sub</h1>",
		"sub/b.txt":                 "b text",
		"sub/.hidden":               "hidden in sub",
		".git/config":               "secret git",
		".hidden":                   "secret hidden",
		".well-known/security.txt":  "Contact: info@bareproxy.com",
		".well-known/.secret":       "secret inside well-known",
		"app.js":                    "console.log('plain')",
		"app.js.gz":                 "GZGZGZGZGZGZGZGZGZGZ",
		"app.js.br":                 "BRBRBRBRBRBR",
		"emptydir/.keep":            "",
		"café.txt":                  "accented",
		"a b.txt":                   "with a space",
		"x...y":                     "dots inside a name",
		"...":                       "three dots, a hidden name",
		"name%with%percent.txt":     "percent in the name",
		"bait":                      "a file inside the folder that is named bait",
		"site":                      "a file inside the folder that is named site",
		"../bait/secret.txt":        accBaitSecret,
		"../bait/index.html":        accBaitIndex,
		"../bait/passwd":            "BAIT-PASSWD root:x:0:0",
		"../site-evil/secret.txt":   accBaitSibling,
		"../site-evil/index.html":   accBaitSibling,
		"../bait/sub/deeper/in.txt": "BAIT-DEEP",
	}
	for name, body := range files {
		accWrite(t, filepath.Join(s, name), body)
	}
	if err := os.Chtimes(filepath.Join(s, "a.txt"), f.mtime, f.mtime); err != nil {
		t.Fatal(err)
	}
	etcRel, err := filepath.Rel(s, "/etc")
	if err != nil {
		t.Fatal(err)
	}
	links := [][2]string{
		{"a.txt", "inside-file"},
		{"sub", "inside-dir"},
		{"sub/b.txt", "inside-deep"},
		{"../site/a.txt", "inside-via-up"}, // up and back down, never above the folder
		{filepath.Join(s, "a.txt"), "abs-link"},
		{filepath.Join(s, "sub"), "abs-dir"},
		{"../bait/secret.txt", "out-file"},
		{"../bait", "out-dir"},
		{"../bait/sub", "out-deep"},
		{f.bait, "out-abs"},
		{"../../bait", "sub/up"},
		{etcRel, "out-etc"},
		{"/etc", "out-etc-abs"},
		{"chain2", "chain"},
		{"../bait", "chain2"},
		{"loop", "loop"},
		{"nowhere", "dangling"},
		{"../site-evil", "out-sibling"},
		{"..", "out-parent"},
		{"../..", "out-grandparent"},
		{"/", "out-root"},
	}
	for _, l := range links {
		if err := os.Symlink(l[0], filepath.Join(s, l[1])); err != nil {
			t.Fatal(err)
		}
	}
	conf := filepath.Join(dir, "bareproxy.conf")
	accWrite(t, conf, "global\n  admin off\n  trace-log "+f.logPath+"\n\n"+
		"site http://files.test:8080\n  error 404 /404.html\n  route /* -> files site\n\n"+
		"site http://keep.test:8080\n  encoded-slashes keep\n  route /* -> files site\n")
	c, probs := Load(conf)
	if HasErrors(probs) {
		t.Fatalf("the paths config: %v", probs)
	}
	rt, err := NewRuntime(c, nil, 1, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Stop(); rt.Trace.Close(); c.Close() })
	srv := NewServer(conf, rt)
	srv.logger.SetOutput(nopWriter{})
	f.h = srv.Handler(8080, false)
	return f
}

// do sends one request to the site named by host. hdr is name, value pairs.
// A target the test's own request parser rejects returns nil.
func (f *accPathsFx) do(host, method, target string, hdr ...string) (rr *httptest.ResponseRecorder) {
	defer func() {
		if p := recover(); p != nil {
			rr = nil
		}
	}()
	req := httptest.NewRequest(method, target, nil)
	req.Host = host + ":8080"
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Add(hdr[i], hdr[i+1])
	}
	rr = httptest.NewRecorder()
	f.h.ServeHTTP(rr, req)
	return rr
}

// leaks reports bait in a response: the content of anything outside the
// folder, or of /etc/passwd.
func accLeaks(body string) string {
	for _, m := range []string{"BAIT", "root:x:0:0", "daemon:", "/bin/bash", "nologin"} {
		if strings.Contains(body, m) {
			return m
		}
	}
	return ""
}

// TestAcceptPathCases is group 4's hand-written part: each case names what
// the path must do on the default site, and on a site that keeps encoded
// slashes.
func TestAcceptPathCases(t *testing.T) {
	f := accBuildPaths(t)
	type tc struct {
		path     string
		def, kep int    // wanted status on files.test and keep.test; 0 means the same as def
		body     string // a fragment of the body when 200
		loc      string // the Location of a 301
	}
	cases := []tc{
		// Plain files and folders.
		{"/", 200, 0, "site home", ""},
		{"/index.html", 200, 0, "site home", ""},
		{"/a.txt", 200, 0, "abcdef", ""},
		{"/sub", 301, 0, "", "/sub/"},
		{"/sub/", 200, 0, "<h1>sub</h1>", ""},
		{"/sub/b.txt", 200, 0, "b text", ""},
		{"/docs/readme.txt", 200, 0, "readme", ""},
		{"/emptydir", 301, 0, "", "/emptydir/"},
		{"/emptydir/", 404, 0, "", ""},
		{"/docs/", 404, 0, "", ""},
		{"/a.txt/", 404, 0, "", ""},
		{"/nothing", 404, 0, "", ""},
		{"/caf%C3%A9.txt", 200, 0, "accented", ""},
		{"/caf%c3%a9.txt", 200, 0, "accented", ""},
		{"/a%20b.txt", 200, 0, "with a space", ""},
		{"/name%25with%25percent.txt", 200, 0, "percent in the name", ""},
		{"/x...y", 200, 0, "dots inside", ""},
		{"/bait", 200, 0, "named bait", ""},
		{"/site", 200, 0, "named site", ""},

		// Dot segments and encodings that stay inside.
		{"/sub/../a.txt", 200, 0, "abcdef", ""},
		{"/sub/%2e%2e/a.txt", 200, 0, "abcdef", ""},
		{"/sub/%2E%2e/a.txt", 200, 0, "abcdef", ""},
		{"/./a.txt", 200, 0, "abcdef", ""},
		{"//a.txt", 200, 0, "abcdef", ""},
		{"/sub//b.txt", 200, 0, "b text", ""},
		{"/sub/./b.txt", 200, 0, "b text", ""},
		{"/%61.txt", 200, 0, "abcdef", ""},
		{"/sub/../sub/../a.txt", 200, 0, "abcdef", ""},
		{"/a.txt/../a.txt", 200, 0, "abcdef", ""},
		{"/a.txt/.", 404, 0, "", ""},

		// Climbing out is refused before any lookup.
		{"/..", 400, 0, "", ""},
		{"/../bait/secret.txt", 400, 0, "", ""},
		{"/%2e%2e/bait/secret.txt", 400, 0, "", ""},
		{"/%2E%2E/bait/secret.txt", 400, 0, "", ""},
		{"/.%2e/bait/secret.txt", 400, 0, "", ""},
		{"/%2e./bait/secret.txt", 400, 0, "", ""},
		{"/sub/../../bait/secret.txt", 400, 0, "", ""},
		{"/sub/%2e%2e/%2e%2e/bait/secret.txt", 400, 0, "", ""},
		{"/a.txt/../../bait/secret.txt", 400, 0, "", ""},
		{"/../site-evil/secret.txt", 400, 0, "", ""},
		{"/../../../../../../../../etc/passwd", 400, 0, "", ""},
		{"/sub/../../site-evil/index.html", 400, 0, "", ""},
		{"/..%2fbait/secret.txt", 400, 404, "", ""},
		{"/%2e%2e%2fbait%2fsecret.txt", 400, 404, "", ""},
		{"/sub/..%5cbait%5csecret.txt", 400, 404, "", ""},

		// Encoded slashes and backslashes, controls, double encoding.
		{"/a%2Fb", 400, 404, "", ""},
		{"/a%2fb", 400, 404, "", ""},
		{"/sub%2Fb.txt", 400, 404, "", ""},
		{"/sub%2F", 400, 404, "", ""},
		{"/a%5Cb", 400, 404, "", ""},
		{"/a%5cb", 400, 404, "", ""},
		{"/sub%5Cb.txt", 400, 404, "", ""},
		{"/a\\b", 400, 0, "", ""},
		{"/sub\\..\\a.txt", 400, 0, "", ""},
		{"/a.txt%00", 400, 0, "", ""},
		{"/a.txt%00.png", 400, 0, "", ""},
		{"/a%0Ab", 400, 0, "", ""},
		{"/a%0d%0ab", 400, 0, "", ""},
		{"/a%7f", 400, 0, "", ""},
		{"/%252e%252e/bait/secret.txt", 404, 0, "", ""},
		{"/%252e%252e%252fbait/secret.txt", 404, 0, "", ""},
		{"/%252fbait/secret.txt", 404, 0, "", ""},
		{"/%c0%ae%c0%ae/bait/secret.txt", 404, 0, "", ""},
		{"/%e0%80%ae%e0%80%ae/bait/secret.txt", 404, 0, "", ""},
		{"/..;/bait/secret.txt", 404, 0, "", ""},
		{"/sub/..;/a.txt", 404, 0, "", ""},

		// Symlinks: inside works, outside and absolute are refused.
		{"/inside-file", 200, 0, "abcdef", ""},
		{"/inside-dir", 301, 0, "", "/inside-dir/"},
		{"/inside-dir/", 200, 0, "<h1>sub</h1>", ""},
		{"/inside-dir/b.txt", 200, 0, "b text", ""},
		{"/inside-deep", 200, 0, "b text", ""},
		{"/inside-via-up", 404, 0, "", ""}, // the folder's own ".." leaves the root while resolving
		{"/abs-link", 404, 0, "", ""},
		{"/abs-dir", 404, 0, "", ""},
		{"/abs-dir/", 404, 0, "", ""},
		{"/abs-dir/b.txt", 404, 0, "", ""},
		{"/out-file", 404, 0, "", ""},
		{"/out-dir", 404, 0, "", ""},
		{"/out-dir/", 404, 0, "", ""},
		{"/out-dir/index.html", 404, 0, "", ""},
		{"/out-dir/secret.txt", 404, 0, "", ""},
		{"/out-dir/sub/deeper/in.txt", 404, 0, "", ""},
		{"/out-deep/deeper/in.txt", 404, 0, "", ""},
		{"/out-abs", 404, 0, "", ""},
		{"/out-abs/", 404, 0, "", ""},
		{"/out-abs/secret.txt", 404, 0, "", ""},
		{"/sub/up", 404, 0, "", ""},
		{"/sub/up/", 404, 0, "", ""},
		{"/sub/up/secret.txt", 404, 0, "", ""},
		{"/chain", 404, 0, "", ""},
		{"/chain/", 404, 0, "", ""},
		{"/chain/secret.txt", 404, 0, "", ""},
		{"/chain2/secret.txt", 404, 0, "", ""},
		{"/out-sibling/secret.txt", 404, 0, "", ""},
		{"/out-sibling/", 404, 0, "", ""},
		{"/out-parent/bait/secret.txt", 404, 0, "", ""},
		{"/out-parent/", 404, 0, "", ""},
		{"/out-grandparent/", 404, 0, "", ""},
		{"/out-root/etc/passwd", 404, 0, "", ""},
		{"/out-etc/passwd", 404, 0, "", ""},
		{"/out-etc-abs/passwd", 404, 0, "", ""},
		{"/out-etc", 404, 0, "", ""},
		{"/loop", 404, 0, "", ""},
		{"/loop/", 404, 0, "", ""},
		{"/dangling", 404, 0, "", ""},
		{"/dangling/", 404, 0, "", ""},
		{"/inside-dir/../out-dir/secret.txt", 404, 0, "", ""},
		{"/sub/../out-dir/secret.txt", 404, 0, "", ""},

		// Hidden names: refused, even encoded, except under /.well-known/.
		{"/.git/config", 404, 0, "", ""},
		{"/.git", 404, 0, "", ""},
		{"/.git/", 404, 0, "", ""},
		{"/%2egit/config", 404, 0, "", ""},
		{"/%2Egit/config", 404, 0, "", ""},
		{"/.hidden", 404, 0, "", ""},
		{"/sub/.hidden", 404, 0, "", ""},
		{"/sub/%2ehidden", 404, 0, "", ""},
		{"/...", 404, 0, "", ""},
		{"/.well-known/security.txt", 200, 0, "Contact", ""},
		{"/%2ewell-known/security.txt", 200, 0, "Contact", ""},
		{"/.well-known/.secret", 404, 0, "", ""},
		{"/.well-known/%2esecret", 404, 0, "", ""},
		{"/.well-known/../.git/config", 404, 0, "", ""},
		{"/sub/.well-known/security.txt", 404, 0, "", ""},
		{"/.well-known", 301, 0, "", "/.well-known/"},
		{"/.well-known/", 404, 0, "", ""},
		{"/x/../.git/config", 404, 0, "", ""},
		{"/sub/../.hidden", 404, 0, "", ""},
	}
	for _, c := range cases {
		for i, host := range []string{"files.test", "keep.test"} {
			want := c.def
			if i == 1 && c.kep != 0 {
				want = c.kep
			}
			rr := f.do(host, "GET", c.path)
			if rr == nil {
				t.Errorf("%s: the test can't build this request", c.path)
				continue
			}
			if rr.Code != want {
				t.Errorf("GET %s on %s: status %d, want %d (body %q)", c.path, host, rr.Code, want, rr.Body.String())
				continue
			}
			if m := accLeaks(rr.Body.String()); m != "" {
				t.Errorf("GET %s on %s: the response holds %q from outside the folder: %q", c.path, host, m, rr.Body.String())
			}
			if want == 200 && !strings.Contains(rr.Body.String(), c.body) {
				t.Errorf("GET %s on %s: body %q lacks %q", c.path, host, rr.Body.String(), c.body)
			}
			if want == 301 && rr.Header().Get("Location") != c.loc {
				t.Errorf("GET %s on %s: Location %q, want %q", c.path, host, rr.Header().Get("Location"), c.loc)
			}
			// HEAD says the same as GET, with no body.
			hr := f.do(host, "HEAD", c.path)
			if hr.Code != want || hr.Body.Len() != 0 {
				t.Errorf("HEAD %s on %s: status %d with %d body bytes, want %d with none", c.path, host, hr.Code, hr.Body.Len(), want)
			}
		}
	}
	// Methods other than GET and HEAD get 405, whatever the path.
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH", "OPTIONS", "PROPFIND"} {
		for _, p := range []string{"/", "/a.txt", "/out-file", "/../bait/secret.txt", "/.git/config"} {
			rr := f.do("files.test", m, p)
			want := 405
			if strings.Contains(p, "..") {
				want = 400 // the path check comes first
			}
			if rr.Code != want {
				t.Errorf("%s %s: status %d, want %d", m, p, rr.Code, want)
			}
		}
	}
	t.Logf("%d path cases on 2 sites, each with GET and HEAD, plus 30 method cases; nothing outside the folder was read", len(cases))
}

// ---- generated traversal sweep ----

var accPathTokens = []string{
	"..", "..", "..", "%2e%2e", "%2E%2E", ".%2e", "%2e.", ".", "%2e", "...", "....", "%2F", "%2f", "%5C", "%5c", "\\",
	"%00", "%252e%252e", "%2e%2e%2f", "..%2f", "..%5c", "..;", "..%00", "%c0%ae%c0%ae", "%e0%80%ae", "%u002e%u002e", "%2", "%zz",
	"", "", "", "bait", "secret.txt", "passwd", "site", "site-evil", "out-file", "out-dir", "out-abs", "out-etc", "out-etc-abs",
	"out-deep", "out-sibling", "out-parent", "out-grandparent", "out-root", "chain", "chain2", "loop", "dangling",
	"inside-file", "inside-dir", "inside-deep", "inside-via-up", "abs-link", "abs-dir", "up", "sub", "docs", "a.txt", "b.txt",
	"readme.txt", "index.html", "404.html", ".git", ".hidden", ".well-known", "security.txt", ".secret", "config", "etc",
	"app.js", "app.js.gz", "emptydir", "deeper", "in.txt", "caf%C3%A9.txt", "a%20b.txt", "x...y",
}

func accLong(n int) string { return strings.Repeat("a", n) }

func (g *accGen) pathsPath() string {
	n := 1 + g.rng.Intn(8)
	var b strings.Builder
	for i := 0; i < n; i++ {
		if g.chance(0.12) {
			b.WriteString("//")
		} else {
			b.WriteString("/")
		}
		switch r := g.rng.Float64(); {
		case r < 0.01:
			b.WriteString(accLong(256 + g.rng.Intn(300)))
		case r < 0.012:
			b.WriteString(accLong(5000))
		case r < 0.2:
			b.WriteString(g.noisy(g.pick(accPathTokens)))
		default:
			b.WriteString(g.pick(accPathTokens))
		}
	}
	switch r := g.rng.Float64(); {
	case r < 0.25:
		b.WriteString("/")
	case r < 0.3:
		b.WriteString("/.")
	case r < 0.33:
		b.WriteString("/..")
	}
	return b.String()
}

// hiddenName reports whether the decoded segments of a normal-form path
// include a name that starts with a dot, other than /.well-known/ as the
// first one.
func accHidden(norm string) bool {
	for i, seg := range strings.Split(strings.Trim(norm, "/"), "/") {
		d, err := url.PathUnescape(seg)
		if err != nil {
			return false
		}
		if strings.HasPrefix(d, ".") && !(i == 0 && d == ".well-known") {
			return true
		}
	}
	return false
}

// accInside checks a served file the way the design says it must be: every
// step of the path is inside the folder, and any symlink on it is relative.
func accInside(site, rel string) error {
	root, err := filepath.EvalSymlinks(site)
	if err != nil {
		return err
	}
	cur := site
	for _, seg := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(cur)
			if err != nil {
				return err
			}
			if filepath.IsAbs(target) {
				return fmt.Errorf("%s is an absolute symlink to %s", seg, target)
			}
		}
	}
	real, err := filepath.EvalSymlinks(filepath.Join(site, rel))
	if err != nil {
		return err
	}
	if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
		return fmt.Errorf("resolves to %s, outside %s", real, root)
	}
	return nil
}

// TestAcceptPathsGenerated is group 4's generated part: paths made of
// traversal tokens, encodings, symlink names, bait names and hidden names, on
// both sites, with GET and HEAD. Nothing outside the folder may be read,
// hidden names get no 200, bad paths are 400 exactly when the reference rules
// say so, and every request leaves one record.
func TestAcceptPathsGenerated(t *testing.T) {
	n := 30_000
	if testing.Short() || accRace {
		n = 8_000
	}
	f := accBuildPaths(t)
	tail := newAccTail(t, f.logPath)
	g := &accGen{rand.New(rand.NewSource(accSeed + 4))}
	rep := newAccReport(t)
	statuses := map[int]int{}
	var unparsable, served, hiddenRefused, refused400 int
	for i := 0; i < n; i++ {
		host, keep := "files.test", false
		if g.chance(0.3) {
			host, keep = "keep.test", true
		}
		method := "GET"
		if g.chance(0.2) {
			method = "HEAD"
		}
		path := g.pathsPath()
		query := ""
		if g.chance(0.2) {
			query = "?" + g.pick(accQueries)
		}
		c := &accCase{port: 8080, method: method, hostname: host, host: host + ":8080", rawPath: path, hdr: http.Header{}}
		if query != "" {
			c.hasQuery, c.query = true, query[1:]
		}
		rr := f.do(host, method, path+query)
		if rr == nil {
			unparsable++ // an escape the HTTP layer would refuse before BareProxy sees it
			continue
		}
		c.code, c.loc = rr.Code, rr.Header().Get("Location")
		rec, recJSON, err := tail.next()
		if err != nil {
			t.Fatalf("request %d %s: %v", i, c, err)
		}
		statuses[rr.Code]++
		fail := func(kind, format string, a ...any) { rep.fail(kind, c, recJSON, format, a...) }
		body := rr.Body.String()
		if m := accLeaks(body); m != "" {
			fail("leak", "the response holds %q from outside the folder: %.200q", m, body)
		}
		if rr.Code >= 500 || rr.Code != 200 && rr.Code != 301 && rr.Code != 400 && rr.Code != 404 {
			fail("status", "unexpected status %d", rr.Code)
		}
		if rec.ID != rr.Header().Get("BareProxy-Id") || rec.Status != rr.Code {
			fail("record", "record ID %s status %d, client got ID %s status %d", rec.ID, rec.Status, rr.Header().Get("BareProxy-Id"), rr.Code)
		}
		norm, ok := accRefNormalize(path, keep)
		switch {
		case !ok:
			refused400++
			if rr.Code != 400 || rec.Outcome != "bad_request" {
				fail("bad-path", "the reference rules refuse this path; the server answered %d (outcome %s)", rr.Code, rec.Outcome)
			}
			continue
		case rr.Code == 400:
			fail("bad-path", "the reference rules accept this path (%q); the server refused it: %s", norm, rec.Reason)
			continue
		}
		if rec.Outcome != "file" {
			fail("outcome", "outcome %q, want file", rec.Outcome)
		}
		if accHidden(norm) {
			if rr.Code != 404 {
				fail("hidden", "a hidden name was answered %d for %q", rr.Code, norm)
			}
			hiddenRefused++
		}
		switch rr.Code {
		case 200:
			served++
			if err := accInside(f.site, rec.File); err != nil {
				fail("escape", "served file %q: %v", rec.File, err)
				continue
			}
			disk, err := os.ReadFile(filepath.Join(f.site, rec.File))
			if err != nil {
				fail("served", "served file %q can't be read: %v", rec.File, err)
			} else if method == "GET" && body != string(disk) {
				fail("served", "body is not the content of %s", rec.File)
			}
		case 301:
			loc := rr.Header().Get("Location")
			if !strings.HasPrefix(loc, norm+"/") || strings.HasPrefix(loc, "//") || !strings.HasPrefix(loc, "/") {
				fail("redirect", "folder redirect from %q went to %q", norm, loc)
			}
		}
	}
	var codes []string
	for st, k := range statuses {
		codes = append(codes, fmt.Sprintf("%d:%d", st, k))
	}
	sort.Strings(codes)
	t.Logf("%d generated paths (%d the test's request parser refused, as Go's server would): statuses %s; %d served files all inside the folder, %d refused with 400, %d hidden-name requests all 404",
		n, unparsable, strings.Join(codes, " "), served, refused400, hiddenRefused)
	if served < n/50 || refused400 < n/50 || statuses[404] < n/10 || hiddenRefused < n/50 {
		t.Errorf("the generated paths are too one-sided to mean much: %d served, %d refused, %d not found, %d hidden", served, refused400, statuses[404], hiddenRefused)
	}
	rep.done()
	recs := accReadLog(t, f.logPath)
	if len(recs) != n-unparsable {
		t.Errorf("%d records for %d requests", len(recs), n-unparsable)
	}
}

// ---- conditional and range requests ----

type accHTTPCase struct {
	name   string
	method string
	path   string
	hdr    []string
	status []int // accepted statuses (HTTP lets a server choose in a few cases)
	body   func(f *accPathsFx, rr *httptest.ResponseRecorder) string
	check  func(f *accPathsFx, rr *httptest.ResponseRecorder, c accHTTPCase) string
}

func accHTTPDate(t time.Time) string { return t.UTC().Format(http.TimeFormat) }

func TestAcceptConditionalAndRange(t *testing.T) {
	f := accBuildPaths(t)
	lm := accHTTPDate(f.mtime)
	day := 24 * time.Hour
	first := f.do("files.test", "GET", "/a.txt")
	etag := first.Header().Get("ETag")
	if first.Code != 200 || etag == "" || first.Header().Get("Last-Modified") != lm || first.Header().Get("Accept-Ranges") != "bytes" ||
		first.Header().Get("Content-Length") != "1000" || first.Body.String() != string(f.content) {
		t.Fatalf("a plain GET of a.txt: %d, headers %v", first.Code, first.Header())
	}
	if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) || strings.HasPrefix(etag, "W/") {
		t.Fatalf("ETag %q should be a strong validator", etag)
	}
	if hd := f.do("files.test", "HEAD", "/a.txt"); hd.Header().Get("ETag") != etag || hd.Header().Get("Content-Length") != "1000" || hd.Body.Len() != 0 {
		t.Errorf("HEAD: ETag %q Content-Length %q body %d", hd.Header().Get("ETag"), hd.Header().Get("Content-Length"), hd.Body.Len())
	}
	if again := f.do("files.test", "GET", "/a.txt").Header().Get("ETag"); again != etag {
		t.Errorf("the ETag changes between requests: %q then %q", etag, again)
	}

	slice := func(a, b int) func(*accPathsFx, *httptest.ResponseRecorder) string {
		return func(f *accPathsFx, _ *httptest.ResponseRecorder) string { return string(f.content[a : b+1]) }
	}
	full := func(f *accPathsFx, _ *httptest.ResponseRecorder) string { return string(f.content) }
	none := func(*accPathsFx, *httptest.ResponseRecorder) string { return "" }
	contentRange := func(want string) func(*accPathsFx, *httptest.ResponseRecorder, accHTTPCase) string {
		return func(_ *accPathsFx, rr *httptest.ResponseRecorder, _ accHTTPCase) string {
			if got := rr.Header().Get("Content-Range"); got != want {
				return fmt.Sprintf("Content-Range %q, want %q", got, want)
			}
			return ""
		}
	}
	ok := []int{200}
	cases := []accHTTPCase{
		// If-None-Match: the weak comparison, for GET and HEAD.
		{name: "If-None-Match with the ETag", method: "GET", hdr: []string{"If-None-Match", etag}, status: []int{304}, body: none},
		{name: "If-None-Match with another ETag", method: "GET", hdr: []string{"If-None-Match", `"other"`}, status: ok, body: full},
		{name: "If-None-Match *", method: "GET", hdr: []string{"If-None-Match", "*"}, status: []int{304}, body: none},
		{name: "If-None-Match with a list", method: "GET", hdr: []string{"If-None-Match", `"x", "y", ` + etag}, status: []int{304}, body: none},
		{name: "If-None-Match with the weak form", method: "GET", hdr: []string{"If-None-Match", "W/" + etag}, status: []int{304}, body: none},
		{name: "If-None-Match on HEAD", method: "HEAD", hdr: []string{"If-None-Match", etag}, status: []int{304}, body: none},
		// If-Match: the strong comparison.
		{name: "If-Match with the ETag", method: "GET", hdr: []string{"If-Match", etag}, status: ok, body: full},
		{name: "If-Match with another ETag", method: "GET", hdr: []string{"If-Match", `"other"`}, status: []int{412}, body: none},
		{name: "If-Match *", method: "GET", hdr: []string{"If-Match", "*"}, status: ok, body: full},
		{name: "If-Match with the weak form", method: "GET", hdr: []string{"If-Match", "W/" + etag}, status: []int{412}, body: none},
		// If-Modified-Since.
		{name: "If-Modified-Since equal to Last-Modified", method: "GET", hdr: []string{"If-Modified-Since", lm}, status: []int{304}, body: none},
		{name: "If-Modified-Since a day later", method: "GET", hdr: []string{"If-Modified-Since", accHTTPDate(f.mtime.Add(day))}, status: []int{304}, body: none},
		{name: "If-Modified-Since a day earlier", method: "GET", hdr: []string{"If-Modified-Since", accHTTPDate(f.mtime.Add(-day))}, status: ok, body: full},
		{name: "If-Modified-Since that isn't a date", method: "GET", hdr: []string{"If-Modified-Since", "yesterday"}, status: ok, body: full},
		{name: "If-Modified-Since on HEAD", method: "HEAD", hdr: []string{"If-Modified-Since", lm}, status: []int{304}, body: none},
		{name: "If-None-Match that fails beats If-Modified-Since", method: "GET", hdr: []string{"If-None-Match", `"other"`, "If-Modified-Since", lm}, status: ok, body: full},
		// If-Unmodified-Since.
		{name: "If-Unmodified-Since equal to Last-Modified", method: "GET", hdr: []string{"If-Unmodified-Since", lm}, status: ok, body: full},
		{name: "If-Unmodified-Since a day earlier", method: "GET", hdr: []string{"If-Unmodified-Since", accHTTPDate(f.mtime.Add(-day))}, status: []int{412}, body: none},
		{name: "If-Unmodified-Since a day later", method: "GET", hdr: []string{"If-Unmodified-Since", accHTTPDate(f.mtime.Add(day))}, status: ok, body: full},
		{name: "If-Match that passes beats If-Unmodified-Since", method: "GET", hdr: []string{"If-Match", etag, "If-Unmodified-Since", accHTTPDate(f.mtime.Add(-day))}, status: ok, body: full},
		// One range.
		{name: "Range first ten bytes", method: "GET", hdr: []string{"Range", "bytes=0-9"}, status: []int{206}, body: slice(0, 9), check: contentRange("bytes 0-9/1000")},
		{name: "Range from byte 990", method: "GET", hdr: []string{"Range", "bytes=990-"}, status: []int{206}, body: slice(990, 999), check: contentRange("bytes 990-999/1000")},
		{name: "Range last ten bytes", method: "GET", hdr: []string{"Range", "bytes=-10"}, status: []int{206}, body: slice(990, 999), check: contentRange("bytes 990-999/1000")},
		{name: "Range one byte", method: "GET", hdr: []string{"Range", "bytes=0-0"}, status: []int{206}, body: slice(0, 0), check: contentRange("bytes 0-0/1000")},
		{name: "Range last byte", method: "GET", hdr: []string{"Range", "bytes=999-999"}, status: []int{206}, body: slice(999, 999), check: contentRange("bytes 999-999/1000")},
		{name: "Range end past the file", method: "GET", hdr: []string{"Range", "bytes=500-99999"}, status: []int{206}, body: slice(500, 999), check: contentRange("bytes 500-999/1000")},
		{name: "Range whole file", method: "GET", hdr: []string{"Range", "bytes=0-999"}, status: []int{200, 206}, body: full},
		{name: "Range suffix longer than the file", method: "GET", hdr: []string{"Range", "bytes=-5000"}, status: []int{200, 206}, body: full},
		{name: "Range spaces around the numbers", method: "GET", hdr: []string{"Range", "bytes= 0-4 "}, status: []int{206, 416}, body: nil},
		// Ranges that can't be met, and ranges that aren't valid.
		{name: "Range start at the end of the file", method: "GET", hdr: []string{"Range", "bytes=1000-"}, status: []int{416}, body: nil, check: contentRange("bytes */1000")},
		{name: "Range start past the end", method: "GET", hdr: []string{"Range", "bytes=2000-3000"}, status: []int{416}, body: nil, check: contentRange("bytes */1000")},
		{name: "Range backwards", method: "GET", hdr: []string{"Range", "bytes=5-2"}, status: []int{416}, body: nil},
		{name: "Range with letters", method: "GET", hdr: []string{"Range", "bytes=abc"}, status: []int{416}, body: nil},
		{name: "Range with a negative start and an end", method: "GET", hdr: []string{"Range", "bytes=--5"}, status: []int{416}, body: nil},
		{name: "Range with no dash", method: "GET", hdr: []string{"Range", "bytes=5"}, status: []int{416}, body: nil},
		{name: "Range in another unit", method: "GET", hdr: []string{"Range", "items=0-5"}, status: []int{200, 416}, body: nil},
		// Several ranges.
		{name: "Range two parts", method: "GET", hdr: []string{"Range", "bytes=0-4,10-14"}, status: []int{206}, body: nil, check: accMultipart([][3]int{{0, 4, 1000}, {10, 14, 1000}})},
		{name: "Range three parts, one a suffix", method: "GET", hdr: []string{"Range", "bytes=0-4,100-104,-5"}, status: []int{206}, body: nil, check: accMultipart([][3]int{{0, 4, 1000}, {100, 104, 1000}, {995, 999, 1000}})},
		{name: "Range one part met and one not", method: "GET", hdr: []string{"Range", "bytes=0-4,2000-3000"}, status: []int{206}, body: slice(0, 4), check: contentRange("bytes 0-4/1000")},
		{name: "Range that asks for the file twice", method: "GET", hdr: []string{"Range", "bytes=0-999,0-999"}, status: []int{200, 206}, body: nil},
		// If-Range.
		{name: "If-Range with the ETag", method: "GET", hdr: []string{"Range", "bytes=0-9", "If-Range", etag}, status: []int{206}, body: slice(0, 9)},
		{name: "If-Range with another ETag", method: "GET", hdr: []string{"Range", "bytes=0-9", "If-Range", `"other"`}, status: ok, body: full},
		{name: "If-Range with the weak ETag", method: "GET", hdr: []string{"Range", "bytes=0-9", "If-Range", "W/" + etag}, status: ok, body: full},
		{name: "If-Range with Last-Modified", method: "GET", hdr: []string{"Range", "bytes=0-9", "If-Range", lm}, status: []int{206}, body: slice(0, 9)},
		{name: "If-Range with an earlier date", method: "GET", hdr: []string{"Range", "bytes=0-9", "If-Range", accHTTPDate(f.mtime.Add(-day))}, status: ok, body: full},
		// Conditions are checked before ranges.
		{name: "Range with a matching If-None-Match", method: "GET", hdr: []string{"Range", "bytes=0-9", "If-None-Match", etag}, status: []int{304}, body: none},
		{name: "Range with a matching If-Modified-Since", method: "GET", hdr: []string{"Range", "bytes=0-9", "If-Modified-Since", lm}, status: []int{304}, body: none},
		{name: "Range with a failing If-Match", method: "GET", hdr: []string{"Range", "bytes=0-9", "If-Match", `"other"`}, status: []int{412}, body: none},
		{name: "Range on HEAD", method: "HEAD", hdr: []string{"Range", "bytes=0-9"}, status: []int{200, 206}, body: none},
	}
	for _, c := range cases {
		path := c.path
		if path == "" {
			path = "/a.txt"
		}
		rr := f.do("files.test", c.method, path, c.hdr...)
		accCheckHTTP(t, f, c, rr)
		// Everything the same on the second site (keep.test serves the same folder).
		accCheckHTTP(t, f, c, f.do("keep.test", c.method, path, c.hdr...))
	}

	// Representations: a precompressed copy has its own ETag, so a validator
	// for one never matches another.
	id := f.do("files.test", "GET", "/app.js")
	gz := f.do("files.test", "GET", "/app.js", "Accept-Encoding", "gzip")
	br := f.do("files.test", "GET", "/app.js", "Accept-Encoding", "br, gzip")
	tags := map[string]string{"identity": id.Header().Get("ETag"), "gzip": gz.Header().Get("ETag"), "br": br.Header().Get("ETag")}
	if id.Header().Get("Content-Encoding") != "" || gz.Header().Get("Content-Encoding") != "gzip" || br.Header().Get("Content-Encoding") != "br" {
		t.Errorf("Content-Encoding is %q, %q, %q for identity, gzip, br", id.Header().Get("Content-Encoding"), gz.Header().Get("Content-Encoding"), br.Header().Get("Content-Encoding"))
	}
	if tags["identity"] == tags["gzip"] || tags["gzip"] == tags["br"] || tags["identity"] == tags["br"] || tags["identity"] == "" {
		t.Errorf("the three representations of app.js must have three ETags, got %v", tags)
	}
	for _, rr := range []*httptest.ResponseRecorder{id, gz, br} {
		if !strings.Contains(rr.Header().Get("Vary"), "Accept-Encoding") {
			t.Errorf("a file with precompressed copies must say Vary: Accept-Encoding, got %q", rr.Header().Get("Vary"))
		}
	}
	for _, c := range []struct {
		name string
		hdr  []string
		want int
	}{
		{"identity ETag with gzip accepted", []string{"If-None-Match", tags["identity"], "Accept-Encoding", "gzip"}, 200},
		{"gzip ETag with gzip accepted", []string{"If-None-Match", tags["gzip"], "Accept-Encoding", "gzip"}, 304},
		{"gzip ETag with nothing accepted", []string{"If-None-Match", tags["gzip"]}, 200},
		{"br ETag with br accepted", []string{"If-None-Match", tags["br"], "Accept-Encoding", "br"}, 304},
		{"gzip ETag with br preferred", []string{"If-None-Match", tags["gzip"], "Accept-Encoding", "br, gzip"}, 200},
		{"a range of the gzip copy", []string{"Range", "bytes=0-3", "Accept-Encoding", "gzip"}, 206},
	} {
		rr := f.do("files.test", "GET", "/app.js", c.hdr...)
		if rr.Code != c.want {
			t.Errorf("app.js, %s: status %d, want %d", c.name, rr.Code, c.want)
		}
		if c.want == 206 && (rr.Body.String() != "GZGZ" || rr.Header().Get("Content-Encoding") != "gzip" || rr.Header().Get("Content-Range") != "bytes 0-3/20") {
			t.Errorf("app.js, %s: body %q, Content-Encoding %q, Content-Range %q", c.name, rr.Body.String(), rr.Header().Get("Content-Encoding"), rr.Header().Get("Content-Range"))
		}
	}
	// Conditions on what isn't there or isn't a file.
	for _, c := range []struct {
		path string
		hdr  []string
		want int
	}{
		{"/nothing", []string{"If-None-Match", "*"}, 404},
		{"/nothing", []string{"Range", "bytes=0-9"}, 404},
		{"/sub", []string{"If-None-Match", "*"}, 301},
		{"/.git/config", []string{"If-None-Match", "*"}, 404},
		{"/.git/config", []string{"Range", "bytes=0-3"}, 404},
		{"/out-file", []string{"Range", "bytes=0-3"}, 404},
	} {
		if rr := f.do("files.test", "GET", c.path, c.hdr...); rr.Code != c.want {
			t.Errorf("GET %s with %v: status %d, want %d", c.path, c.hdr, rr.Code, c.want)
		}
	}
	t.Logf("%d conditional and range cases on 2 sites (RFC 9110 sections 13 and 14), plus 12 on precompressed copies and 6 on missing, hidden and outside files", len(cases))
}

func accCheckHTTP(t *testing.T, f *accPathsFx, c accHTTPCase, rr *httptest.ResponseRecorder) {
	t.Helper()
	okStatus := false
	for _, s := range c.status {
		okStatus = okStatus || rr.Code == s
	}
	if !okStatus {
		t.Errorf("%s: status %d, want one of %v (headers %v)", c.name, rr.Code, c.status, rr.Header())
		return
	}
	if rr.Code == 304 {
		if rr.Body.Len() != 0 || rr.Header().Get("ETag") == "" {
			t.Errorf("%s: a 304 has no body and keeps the ETag; got %d body bytes, ETag %q", c.name, rr.Body.Len(), rr.Header().Get("ETag"))
		}
	}
	if rr.Code == 416 && rr.Header().Get("Content-Range") != "bytes */1000" && !strings.Contains(c.name, "backwards") &&
		!strings.Contains(c.name, "letters") && !strings.Contains(c.name, "negative") && !strings.Contains(c.name, "no dash") && !strings.Contains(c.name, "another unit") {
		t.Errorf("%s: a 416 says Content-Range: bytes */1000, got %q", c.name, rr.Header().Get("Content-Range"))
	}
	if c.body != nil && rr.Code != 416 {
		if want := c.body(f, rr); rr.Body.String() != want && c.method != "HEAD" {
			t.Errorf("%s: body is %d bytes, want %d", c.name, rr.Body.Len(), len(want))
		}
	}
	if c.method == "HEAD" && rr.Body.Len() != 0 {
		t.Errorf("%s: HEAD got %d body bytes", c.name, rr.Body.Len())
	}
	if rr.Code == 206 && c.check == nil && !strings.HasPrefix(rr.Header().Get("Content-Type"), "multipart/byteranges") && rr.Header().Get("Content-Range") == "" {
		t.Errorf("%s: a 206 needs a Content-Range", c.name)
	}
	if rr.Code == 206 && c.method == "GET" && !strings.HasPrefix(rr.Header().Get("Content-Type"), "multipart/") {
		if cl := rr.Header().Get("Content-Length"); cl != strconv.Itoa(rr.Body.Len()) {
			t.Errorf("%s: Content-Length %q for a body of %d bytes", c.name, cl, rr.Body.Len())
		}
	}
	if c.check != nil && rr.Code != 416 || c.check != nil && strings.Contains(c.name, "start at the end") || c.check != nil && strings.Contains(c.name, "start past the end") {
		if msg := c.check(f, rr, c); msg != "" {
			t.Errorf("%s: %s", c.name, msg)
		}
	}
}

// accMultipart checks a multipart/byteranges answer: one part per range, each
// with its Content-Range and the right bytes.
func accMultipart(parts [][3]int) func(*accPathsFx, *httptest.ResponseRecorder, accHTTPCase) string {
	return func(f *accPathsFx, rr *httptest.ResponseRecorder, _ accHTTPCase) string {
		mt, params, err := mime.ParseMediaType(rr.Header().Get("Content-Type"))
		if err != nil || mt != "multipart/byteranges" || params["boundary"] == "" {
			return fmt.Sprintf("Content-Type %q is not multipart/byteranges", rr.Header().Get("Content-Type"))
		}
		mr := multipart.NewReader(bytes.NewReader(rr.Body.Bytes()), params["boundary"])
		for i, want := range parts {
			p, err := mr.NextPart()
			if err != nil {
				return fmt.Sprintf("part %d: %v", i, err)
			}
			data, _ := io.ReadAll(p)
			wantRange := fmt.Sprintf("bytes %d-%d/%d", want[0], want[1], want[2])
			if got := p.Header.Get("Content-Range"); got != wantRange {
				return fmt.Sprintf("part %d: Content-Range %q, want %q", i, got, wantRange)
			}
			if string(data) != string(f.content[want[0]:want[1]+1]) {
				return fmt.Sprintf("part %d: wrong bytes", i)
			}
		}
		if _, err := mr.NextPart(); err != io.EOF {
			return "more parts than ranges"
		}
		return ""
	}
}
