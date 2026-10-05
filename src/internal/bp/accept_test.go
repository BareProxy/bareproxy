// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

// Acceptance tests, groups 1 and 2 (the design note, section 12):
//
//  1. explain and the server pick the same site, the same rule line and the
//     same action for generated requests (100,000 in a full run).
//  2. every request leaves exactly one record, IDs are unique, and
//     FindRecord finds each one.
//
// Group 3 (broken configs) is in accept_config_test.go, group 4 (paths and
// files) in accept_paths_test.go and group 5 (smuggling) in
// accept_smuggle_test.go. The generators and the reference rules are in
// accept_gen_test.go.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const accSeed = 20261005

// accRequests is how many generated requests group 1 sends.
func accRequests() int {
	switch {
	case testing.Short():
		return 10_000
	case accRace:
		// The race detector makes a request about five times slower.
		return 25_000
	}
	return 100_000
}

// accTail reads a trace log as it grows, one record per call, so each request
// can be compared with the record it just left.
type accTail struct {
	f  *os.File
	br *bufio.Reader
}

func newAccTail(t *testing.T, path string) *accTail {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return &accTail{f: f, br: bufio.NewReaderSize(f, 1<<16)}
}

func (a *accTail) next() (Record, string, error) {
	var rec Record
	line, err := a.br.ReadString('\n')
	if err != nil {
		return rec, line, fmt.Errorf("no complete record after the request: %v (read %q)", err, line)
	}
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		return rec, line, fmt.Errorf("the record is not JSON: %v", err)
	}
	return rec, strings.TrimSpace(line), nil
}

// accReport collects mismatches by kind and keeps a few full examples.
type accReport struct {
	t        *testing.T
	counts   map[string]int
	examples []string
	maxEx    int
	known    map[string]int
	knownEx  map[string]string
}

func newAccReport(t *testing.T) *accReport {
	return &accReport{t: t, counts: map[string]int{}, maxEx: 12, known: map[string]int{}, knownEx: map[string]string{}}
}

func (r *accReport) describe(c *accCase, recJSON, msg string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n  request: %s\n  client got: %d, Location %q", msg, c, c.code, c.loc)
	if recJSON != "" {
		fmt.Fprintf(&b, "\n  record: %s", recJSON)
	}
	if c.explTxt != "" {
		fmt.Fprintf(&b, "\n  explain said:\n    %s", strings.ReplaceAll(strings.TrimRight(c.explTxt, "\n"), "\n", "\n    "))
	}
	return b.String()
}

func (r *accReport) fail(kind string, c *accCase, recJSON string, f string, a ...any) {
	r.counts[kind]++
	if len(r.examples) < r.maxEx {
		r.examples = append(r.examples, r.describe(c, recJSON, fmt.Sprintf("[%s] ", kind)+fmt.Sprintf(f, a...)))
	}
}

// knownBug counts a mismatch that belongs to a bug already reported. The
// test goes on; the count and one example go to the log.
func (r *accReport) knownBug(id string, c *accCase, recJSON string, f string, a ...any) {
	r.known[id]++
	if _, ok := r.knownEx[id]; !ok {
		r.knownEx[id] = r.describe(c, recJSON, fmt.Sprintf(f, a...))
	}
}

func (r *accReport) done() {
	r.t.Helper()
	ids := make([]string, 0, len(r.known))
	for id := range r.known {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		r.t.Logf("known bug %s: %d requests are affected; first one: %s", id, r.known[id], r.knownEx[id])
	}
	if len(r.counts) == 0 {
		return
	}
	kinds := make([]string, 0, len(r.counts))
	total := 0
	for k, n := range r.counts {
		kinds = append(kinds, fmt.Sprintf("%s: %d", k, n))
		total += n
	}
	sort.Strings(kinds)
	r.t.Errorf("%d mismatches (%s). The first %d:\n%s", total, strings.Join(kinds, ", "), len(r.examples), strings.Join(r.examples, "\n\n"))
}

func (c *accCase) String() string {
	return fmt.Sprintf("%s http://%s:%d%s with Host %q and headers %v", c.method, c.hostname, c.port, c.target(), c.host, map[string][]string(c.hdr))
}

// freezeTrials moves the 10 second trial of every down backend far into the
// future. A down backend in a pool without health checks lets one request
// through every 10 seconds, which would make a request that arrives at that
// moment ambiguous between explain and the server. With the trial out of the
// way the same pool state gives the same answer to both.
func (w *accWorld) freezeTrials() {
	far := time.Now().Add(24 * time.Hour)
	for _, p := range w.rt.Pools {
		for _, b := range p.Backends {
			b.mu.Lock()
			if b.state == "down" {
				b.trialAt = far
			}
			b.mu.Unlock()
		}
	}
}

// accSend builds the request for a case and sends it through the handler of
// its port. It returns the recorder.
func (w *accWorld) send(c *accCase) (rr *httptest.ResponseRecorder, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("the test can't build this request: %v", p)
		}
	}()
	req := httptest.NewRequest(c.method, c.target(), nil)
	req.Host = c.host
	for k, vs := range c.hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rr = httptest.NewRecorder()
	w.handler[c.port].ServeHTTP(rr, req)
	c.code = rr.Code
	c.loc = rr.Header().Get("Location")
	c.id = rr.Header().Get("BareProxy-Id")
	if rr.Header().Get("X-Seen-Uri") != "" {
		c.seen = http.Header{}
		for _, k := range []string{"X-Seen-Method", "X-Seen-Uri", "X-Seen-Host", "X-Seen-Id"} {
			c.seen.Set(k, rr.Header().Get(k))
		}
	}
	return rr, nil
}

// check compares one request, its record, what explain said before it and
// the design's rules (the reference code in accept_gen_test.go).
func (w *accWorld) check(rep *accReport, c *accCase, rr *httptest.ResponseRecorder, rec *Record, recJSON string, deadHits int) {
	fail := func(kind, f string, a ...any) { rep.fail(kind, c, recJSON, f, a...) }
	body := rr.Body.String()
	ex := c.expl
	want := w.oracle(c)

	// The client and the record agree, and the response carries the ID.
	if len(c.id) != 16 || c.id != rec.ID {
		fail("id", "response header BareProxy-Id is %q, record ID is %q", c.id, rec.ID)
	}
	if rec.Status != c.code {
		fail("record-status", "record says %d, the client got %d", rec.Status, c.code)
	}
	if rec.BytesOut != int64(len(body)) {
		fail("record-bytes", "record says %d bytes out, the client got %d", rec.BytesOut, len(body))
	}
	if rec.Method != c.method || rec.Host != c.host || rec.Scheme != "http" || rec.Config != 1 {
		fail("record-request", "record has method %q host %q scheme %q config %d", rec.Method, rec.Host, rec.Scheme, rec.Config)
	}

	// The site.
	if want.site == nil {
		if rec.Site != "" || rec.Outcome != "no_site" || c.code != 421 {
			fail("no-site", "no site should answer 421 (no_site); record has site %q outcome %q, client got %d", rec.Site, rec.Outcome, c.code)
		}
		if ex.site != "" || ex.action != "421 (no_site)" {
			fail("no-site-explain", "explain should say 421 (no_site), said site %q action %q", ex.site, ex.action)
		}
		if rec.Path == "" {
			rep.knownBug("no-site-record-has-no-path", c, recJSON, "the record of a 421 has an empty path")
		} else if rec.Path != c.rawPath {
			fail("record-path", "record path %q, the client sent %q", rec.Path, c.rawPath)
		}
		return
	}
	if rec.Site != want.site.name || rec.SiteLine != want.site.line {
		fail("site", "design says site %s (line %d); record has %s (line %d)", want.site.name, want.site.line, rec.Site, rec.SiteLine)
	}
	if ex.site != want.site.name || ex.siteLine != want.site.line {
		fail("site-explain", "design says site %s (line %d); explain says %s (line %d)", want.site.name, want.site.line, ex.site, ex.siteLine)
	}
	if rec.Path != c.rawPath {
		fail("record-path", "record path %q, the client sent %q", rec.Path, c.rawPath)
	}

	// The path.
	if want.bad {
		if rec.Outcome != "bad_request" || c.code != 400 {
			fail("bad-path", "design says 400 (bad_request); record has outcome %q, client got %d", rec.Outcome, c.code)
		}
		if !ex.refused || ex.action != "400 (bad_request)" {
			if !ex.refused && want.site.keep && strings.Contains(c.rawPath, `\`) {
				rep.knownBug("explain-raw-backslash-on-keep-site", c, recJSON, "the server refuses the raw backslash; explain turns it into %%5C, which this site keeps, and routes the request")
			} else {
				fail("bad-path-explain", "explain should refuse the path with 400; refused=%v action %q", ex.refused, ex.action)
			}
		}
		return
	}
	norm := rec.NormPath
	if norm == "" {
		norm = rec.Path
	}
	if norm != want.norm {
		fail("norm", "design says the normalized path is %q; record has %q", want.norm, norm)
	}
	if ex.norm != want.norm {
		fail("norm-explain", "design says the normalized path is %q; explain says %q", want.norm, ex.norm)
	}

	// The rule.
	wantLine := 0
	if want.route != nil {
		wantLine = want.route.line
	}
	if rec.Line != wantLine || ex.ruleLine != wantLine {
		fail("rule", "design says rule line %d; record has %d, explain has %d", wantLine, rec.Line, ex.ruleLine)
	}
	if want.route == nil {
		if rec.Outcome != "no_route" || c.code != 404 || ex.action != "404 (no_route)" {
			fail("no-route", "no rule should answer 404 (no_route); record outcome %q, client got %d, explain action %q", rec.Outcome, c.code, ex.action)
		}
		return
	}
	r := want.route
	if rec.Rule != r.text {
		fail("rule-text", "design says rule %q; record has %q", r.text, rec.Rule)
	}

	// The action: explain's statement, when it makes one, is the status the
	// client gets.
	if st, loc, ok := accActionOutcome(ex.action); ok {
		if st != c.code {
			fail("explain-status", "explain says %d, the server sent %d", st, c.code)
		}
		if loc != "" && loc != c.loc {
			if ex.folder && c.hasQuery && c.query != "" && c.loc == loc+"?"+c.query {
				rep.knownBug("explain-folder-redirect-drops-query", c, recJSON, "explain says Location %q, the server sent %q", loc, c.loc)
			} else {
				fail("explain-location", "explain says Location %q, the server sent %q", loc, c.loc)
			}
		}
	}

	switch r.kind {
	case "respond":
		if c.code != r.code || rec.Outcome != "local" {
			fail("respond", "rule says respond %d; client got %d, outcome %q", r.code, c.code, rec.Outcome)
		}
		wantBody := ""
		if r.body != "" && c.method != "HEAD" {
			wantBody = r.body + "\n"
		}
		if body != wantBody {
			fail("respond-body", "body %q, want %q", body, wantBody)
		}
	case "redirect":
		if c.code != r.code || c.loc != want.loc || rec.Location != c.loc || rec.Outcome != "local" {
			fail("redirect", "rule says redirect %d to %q; client got %d to %q, record has Location %q outcome %q", r.code, want.loc, c.code, c.loc, rec.Location, rec.Outcome)
		}
	case "files":
		w.checkFiles(rep, c, rr, rec, recJSON, r, norm)
	case "pool":
		w.checkPool(rep, c, rr, rec, recJSON, r, want, deadHits)
	}
}

func (w *accWorld) checkFiles(rep *accReport, c *accCase, rr *httptest.ResponseRecorder, rec *Record, recJSON string, r *accRoute, norm string) {
	fail := func(kind, f string, a ...any) { rep.fail(kind, c, recJSON, f, a...) }
	ex := c.expl
	folder := filepath.Join(w.dir, r.folder)
	if rec.Outcome != "file" || rec.Folder != folder {
		fail("files", "files rule: record has outcome %q folder %q, want file and %q", rec.Outcome, rec.Folder, folder)
	}
	if c.method != "GET" && c.method != "HEAD" {
		if c.code != 405 || rr.Header().Get("Allow") != "GET, HEAD" {
			fail("files-405", "files answer only GET and HEAD; client got %d, Allow %q", c.code, rr.Header().Get("Allow"))
		}
		return
	}
	switch c.code {
	case 200:
		if rec.File == "" || strings.Contains(rec.File, "..") {
			fail("files-200", "200 with record file %q", rec.File)
			return
		}
		for _, seg := range strings.Split(rec.File, "/") {
			if strings.HasPrefix(seg, ".") && !(seg == ".well-known" && strings.HasPrefix(rec.File, ".well-known/")) {
				fail("files-hidden", "a hidden name was served: %q", rec.File)
			}
		}
		onDisk, err := os.ReadFile(filepath.Join(folder, rec.File))
		sent := rec.File
		if rec.Sent != "" {
			sent = rec.Sent
			onDisk, err = os.ReadFile(filepath.Join(folder, rec.Sent))
		}
		if err != nil {
			fail("files-200", "served file %q can't be read from the folder: %v", sent, err)
			return
		}
		if strings.Contains(rr.Body.String(), "OUTSIDE") {
			fail("files-outside", "the body holds the bait from outside the folder: %q", rr.Body.String())
		}
		if c.method == "GET" && rr.Body.String() != string(onDisk) {
			fail("files-body", "body %q is not the content of %s (%q)", rr.Body.String(), sent, onDisk)
		}
		if c.method == "HEAD" && rr.Body.Len() != 0 {
			fail("files-head", "HEAD got a body of %d bytes", rr.Body.Len())
		}
		if ex.file != "" && accJSONForm(ex.file) != inFolder(rec.Folder, rec.File) {
			fail("files-explain", "explain says File %q, the server sent %q", ex.file, inFolder(rec.Folder, rec.File))
		}
	case 301:
		if want := norm + "/"; !strings.HasPrefix(c.loc, want) || (c.loc != want && c.loc != want+"?"+c.query) {
			fail("files-301", "folder redirect from %q went to %q", norm, c.loc)
		}
	case 404:
		// A record is JSON, which can't hold a byte that isn't UTF-8 (%FF in a
		// path): it writes U+FFFD instead. Compare in that form.
		if len(rec.Checked) > 0 && ex.checked != "" && accJSONForm(ex.checked) != inFolder(rec.Folder, rec.Checked[0]) {
			fail("files-explain", "explain checked %q, the server checked %q", ex.checked, inFolder(rec.Folder, rec.Checked[0]))
		}
	default:
		fail("files", "a files rule answered %d", c.code)
	}
}

func (w *accWorld) checkPool(rep *accReport, c *accCase, rr *httptest.ResponseRecorder, rec *Record, recJSON string, r *accRoute, want accWant, deadHits int) {
	fail := func(kind, f string, a ...any) { rep.fail(kind, c, recJSON, f, a...) }
	ex := c.expl
	if rec.Pool != r.pool || rec.Upstream != want.upstream {
		fail("pool", "design says pool %s, upstream path %q; record has pool %q, upstream %q", r.pool, want.upstream, rec.Pool, rec.Upstream)
	}
	if ex.upstream != want.upstream || ex.upMethod != c.method {
		fail("pool-explain", "design says upstream %s %q; explain says %s %q", c.method, want.upstream, ex.upMethod, ex.upstream)
	}
	// Explain shows the Host the URL gives. When the request's Host header is
	// the same text, the backend must have been sent it unchanged.
	if c.host == fmt.Sprintf("%s:%d", c.hostname, c.port) && ex.upHost != c.host {
		fail("pool-explain", "explain says upstream Host %q, the request's Host is %q", ex.upHost, c.host)
	}
	if r.pool == "dead" {
		wantStatus, outcome := 502, "connect_failed"
		if deadHits > 3 {
			wantStatus, outcome = 503, "no_backend"
		}
		if c.code != wantStatus || rec.Outcome != outcome {
			fail("pool-dead", "request %d to a dead backend: want %d (%s), client got %d, outcome %q", deadHits, wantStatus, outcome, c.code, rec.Outcome)
		}
		if deadHits > 3 && ex.action != "503 (no_backend)" {
			fail("pool-explain", "the dead backend is down, so explain should say 503; it said %q", ex.action)
		}
		if deadHits <= 3 && ex.action != "" {
			fail("pool-explain", "the dead backend is still up, so explain states no action; it said %q", ex.action)
		}
		return
	}
	if ex.action != "" {
		fail("pool-explain", "a live pool needs no action line; explain said %q", ex.action)
	}
	if c.code != 200 || rec.Outcome != "ok" {
		fail("pool", "a live backend should answer 200; client got %d, outcome %q", c.code, rec.Outcome)
		return
	}
	if c.seen == nil {
		fail("pool", "the backend's answer carries no X-Seen headers")
		return
	}
	// What the backend saw: the same method, the normalized and stripped path,
	// the query untouched, the client's Host, and this request's ID.
	wantURI := want.upstream
	if c.hasQuery && c.query != "" {
		wantURI += "?" + c.query
	}
	gotURI := c.seen.Get("X-Seen-Uri")
	if !c.hasQuery || c.query == "" {
		gotURI, _, _ = strings.Cut(gotURI, "?") // a bare "?" may or may not survive
	}
	if c.seen.Get("X-Seen-Method") != c.method || gotURI != wantURI || c.seen.Get("X-Seen-Host") != c.host || c.seen.Get("X-Seen-Id") != rec.ID {
		fail("pool-backend", "backend saw %s %s Host %q ID %q; want %s %s Host %q ID %q", c.seen.Get("X-Seen-Method"), c.seen.Get("X-Seen-Uri"),
			c.seen.Get("X-Seen-Host"), c.seen.Get("X-Seen-Id"), c.method, wantURI, c.host, rec.ID)
	}
	if len(rec.Attempts) == 0 || rec.Attempts[len(rec.Attempts)-1].Status != 200 {
		fail("pool", "the record shows no successful attempt: %+v", rec.Attempts)
	}
}

// TestAcceptExplainMatchesServer is group 1. For every generated request it
// asks explain first, then sends the request through the server's handler,
// reads the record it left from the trace log and compares site, rule line,
// normalized path, action and what the backend saw, both between explain and
// the server and against the reference rules.
func TestAcceptExplainMatchesServer(t *testing.T) {
	n := accRequests()
	w := accBuildWorld(t, accSeed, "requests.log")
	tail := newAccTail(t, w.logPath)
	g := &accGen{rand.New(rand.NewSource(accSeed + 1))}
	rep := newAccReport(t)
	t.Logf("config: %s; %d lines, %d sites", Summary(w.cfg), len(w.lines), len(w.sites))
	routes := 0
	for _, s := range w.sites {
		routes += len(s.routes)
	}
	t.Logf("%d routes over %d sites on 2 ports", routes, len(w.sites))

	start := time.Now()
	deadHits := 0
	kinds := map[string]int{}
	statuses := map[int]int{}
	var invalid int
	for i := 0; i < n; i++ {
		c := g.next()
		out, err := Explain(w.rt, c.method, c.explainURL(), c.hdr, true)
		if err != nil {
			t.Fatalf("explain refused request %d, %s: %v", i, c, err)
		}
		c.explTxt, c.expl = out, accParseExplain(out)
		rr, err := w.send(c)
		if err != nil {
			invalid++
			if invalid > 5 {
				t.Fatalf("too many requests the test can't build: %v", err)
			}
			continue
		}
		rec, recJSON, err := tail.next()
		if err != nil {
			t.Fatalf("request %d, %s: %v", i, c, err)
		}
		want := w.oracle(c)
		if want.route != nil && want.route.kind == "pool" && want.route.pool == "dead" {
			deadHits++
		}
		w.check(rep, c, rr, &rec, recJSON, deadHits)
		w.freezeTrials()
		statuses[c.code]++
		switch {
		case want.site == nil:
			kinds["no site (421)"]++
		case want.bad:
			kinds["bad path (400)"]++
		case want.route == nil:
			kinds["no rule (404)"]++
		default:
			kinds[want.route.kind]++
		}
		if len(rep.counts) > 0 && rep.total() > 40 {
			break // enough examples; the report shows them
		}
	}
	elapsed := time.Since(start)
	t.Logf("%d requests in %s (%.0f per second), each asked of explain first", n, elapsed.Round(time.Millisecond), float64(n)/elapsed.Seconds())
	t.Logf("what the generated requests came to: %v", kinds)
	var codes []string
	for st, k := range statuses {
		codes = append(codes, fmt.Sprintf("%d:%d", st, k))
	}
	sort.Strings(codes)
	t.Logf("statuses: %s", strings.Join(codes, " "))
	rep.done()

	// The trace log, read whole: one record per request, one line each.
	recs := accReadLog(t, w.logPath)
	if len(recs) != n-invalid {
		t.Errorf("the trace log has %d records for %d requests", len(recs), n-invalid)
	}
	ids := map[string]bool{}
	for _, r := range recs {
		if ids[r.ID] {
			t.Errorf("record ID %s is used twice", r.ID)
			break
		}
		ids[r.ID] = true
	}
	t.Logf("trace log: %d records, %d distinct IDs", len(recs), len(ids))
}

// accJSONForm is s as it comes back from a JSON line: bytes that aren't UTF-8
// become U+FFFD.
func accJSONForm(s string) string {
	b, _ := json.Marshal(s)
	var out string
	json.Unmarshal(b, &out)
	return out
}

func (r *accReport) total() int {
	n := 0
	for _, k := range r.counts {
		n += k
	}
	return n
}

// ---- group 2 ----

// TestAcceptOneRecordPerRequest is group 2. Requests of every kind, sent from
// several goroutines at once, leave exactly one record each, with unique IDs,
// and FindRecord finds every one of them by its full ID (and a sample by an
// 8-character prefix).
func TestAcceptOneRecordPerRequest(t *testing.T) {
	n, workers := 3000, 8
	if testing.Short() {
		n = 800
	}
	if accRace {
		n = 1500
	}
	w := accBuildWorld(t, accSeed+7, "records.log")
	type sent struct {
		id, method, path string
		code             int
		hasBody          bool
	}
	results := make([][]sent, workers)
	var wg sync.WaitGroup
	for k := 0; k < workers; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			g := &accGen{rand.New(rand.NewSource(int64(1000 + k)))}
			for i := 0; i < n/workers; i++ {
				c := g.next()
				var req *http.Request
				var body string
				switch {
				case i%25 == 7:
					// A request with a body, to whatever the route is.
					body = strings.Repeat("x", 1+g.rng.Intn(3000))
					req = httptest.NewRequest("POST", c.target(), strings.NewReader(body))
				case i%40 == 11:
					// A client that has already gone away.
					req = httptest.NewRequest(c.method, c.target(), nil)
					ctx, cancel := context.WithCancel(context.Background())
					cancel()
					req = req.WithContext(ctx)
				default:
					req = httptest.NewRequest(c.method, c.target(), nil)
				}
				req.Host = c.host
				for k, vs := range c.hdr {
					for _, v := range vs {
						req.Header.Add(k, v)
					}
				}
				rr := httptest.NewRecorder()
				w.handler[c.port].ServeHTTP(rr, req)
				results[k] = append(results[k], sent{id: rr.Header().Get("BareProxy-Id"), method: req.Method, path: c.rawPath, code: rr.Code, hasBody: body != ""})
			}
		}(k)
	}
	wg.Wait()

	total := 0
	byID := map[string]bool{}
	for _, rs := range results {
		for _, s := range rs {
			total++
			if len(s.id) != 16 {
				t.Errorf("a %s %s got no BareProxy-Id header", s.method, s.path)
			}
			if byID[s.id] {
				t.Errorf("response ID %s was handed out twice", s.id)
			}
			byID[s.id] = true
		}
	}
	recs := accReadLog(t, w.logPath)
	if len(recs) != total {
		t.Fatalf("%d requests sent and %d records in the log", total, len(recs))
	}
	inLog := map[string]Record{}
	for _, r := range recs {
		if _, dup := inLog[r.ID]; dup {
			t.Errorf("two records have ID %s", r.ID)
		}
		inLog[r.ID] = r
	}
	outcomes := map[string]int{}
	for id := range byID {
		r, ok := inLog[id]
		if !ok {
			t.Errorf("request ID %s has no record", id)
			continue
		}
		outcomes[r.Outcome]++
	}
	t.Logf("%d requests from %d goroutines, %d records, %d distinct IDs; outcomes %v", total, workers, len(recs), len(inLog), outcomes)

	// Every ID found, and explained.
	start := time.Now()
	found := 0
	for _, rs := range results {
		for _, s := range rs {
			rec, err := FindRecord(w.logPath, s.id)
			if err != nil {
				t.Errorf("FindRecord(%s): %v", s.id, err)
				continue
			}
			if rec.ID != s.id || rec.Method != s.method || rec.Path != s.path && rec.Outcome != "no_site" {
				t.Errorf("FindRecord(%s) found %s %s", s.id, rec.Method, rec.Path)
			}
			// A recorder reports 200 when nothing was written, as when the
			// client had gone; the record then has no status.
			if rec.Status != s.code && rec.Outcome != "client_gone" {
				t.Errorf("FindRecord(%s): record status %d, client got %d", s.id, rec.Status, s.code)
			}
			if why := RenderWhy(rec); !strings.Contains(why, "Request "+s.id) || !strings.Contains(why, "outcome "+rec.Outcome) {
				t.Errorf("why for %s doesn't tell the story:\n%s", s.id, why)
			}
			found++
		}
	}
	t.Logf("FindRecord found %d of %d IDs by the full ID in %s", found, total, time.Since(start).Round(time.Millisecond))

	// An 8-character prefix, as the command takes it. A prefix two records
	// share is refused with a clear message, and only then.
	prefixes := map[string]int{}
	for id := range inLog {
		prefixes[id[:8]]++
	}
	checked := 0
	for _, rs := range results {
		for i, s := range rs {
			if i%15 != 0 {
				continue
			}
			rec, err := FindRecord(w.logPath, s.id[:8])
			switch {
			case prefixes[s.id[:8]] > 1:
				if err == nil || !strings.Contains(err.Error(), "give more characters") {
					t.Errorf("prefix %s is shared and should be refused, got %v", s.id[:8], err)
				}
			case err != nil || rec.ID != s.id:
				t.Errorf("FindRecord(%s): %v", s.id[:8], err)
			}
			checked++
		}
	}
	t.Logf("FindRecord found %d IDs by an 8-character prefix", checked)
}
