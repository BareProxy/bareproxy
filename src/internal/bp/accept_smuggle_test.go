// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

// Acceptance tests, group 5: request smuggling (design note, section 12).
// A real plain-HTTP listener on localhost runs BareProxy's handler with the
// server settings Run uses. A raw TCP client sends payloads that try to get a
// second request past a path rule (CL plus TE, two Content-Lengths, bad chunk
// sizes, obs-fold, absolute-form request lines, odd request lines, hop-by-hop
// headers, Upgrade). The backend is a raw TCP server that parses everything it
// receives and counts the requests. Each payload must end as a refusal or as
// at most one well-formed request at the backend, and nothing under /admin or
// /secret (both answered 403 by rules) may ever reach the backend.

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// accSeen is one request the backend parsed.
type accSeen struct {
	conn    int
	method  string
	uri     string
	host    string
	hdr     http.Header
	body    []byte
	bodyErr error
	cl      int64
	te      []string
}

// accRawBackend is a TCP server that speaks just enough HTTP/1.1 to answer
// 200, parses every request it receives and keeps them. With upgrade set it
// answers 101 to a request that asks for an upgrade and goes on reading
// HTTP/1.1 from the same connection.
type accRawBackend struct {
	ln      net.Listener
	mu      sync.Mutex
	seen    []accSeen
	bad     []string // what it could not parse
	conns   []net.Conn
	upgrade bool
	early   bool // answer as soon as the headers are in, without reading the body
}

func newAccRawBackend(t *testing.T, upgrade bool) *accRawBackend {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &accRawBackend{ln: ln, upgrade: upgrade}
	t.Cleanup(func() {
		ln.Close()
		b.mu.Lock()
		for _, c := range b.conns {
			c.Close()
		}
		b.mu.Unlock()
	})
	go func() {
		for id := 1; ; id++ {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			b.mu.Lock()
			b.conns = append(b.conns, c)
			b.mu.Unlock()
			go b.serve(c, id)
		}
	}()
	return b
}

func (b *accRawBackend) addr() string { return b.ln.Addr().String() }

func (b *accRawBackend) serve(c net.Conn, id int) {
	defer c.Close()
	br := bufio.NewReader(c)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			if err != io.EOF && !strings.Contains(err.Error(), "closed") && !strings.Contains(err.Error(), "reset") {
				b.mu.Lock()
				b.bad = append(b.bad, fmt.Sprintf("connection %d: %v", id, err))
				b.mu.Unlock()
			}
			return
		}
		b.mu.Lock()
		early := b.early
		b.mu.Unlock()
		if early {
			b.mu.Lock()
			b.seen = append(b.seen, accSeen{conn: id, method: req.Method, uri: req.RequestURI, host: req.Host, hdr: req.Header, cl: req.ContentLength, te: req.TransferEncoding})
			b.mu.Unlock()
			fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: 5\r\nConnection: close\r\n\r\nearly")
			return
		}
		body, berr := io.ReadAll(req.Body)
		s := accSeen{conn: id, method: req.Method, uri: req.RequestURI, host: req.Host, hdr: req.Header, body: body, bodyErr: berr, cl: req.ContentLength, te: req.TransferEncoding}
		b.mu.Lock()
		b.seen = append(b.seen, s)
		b.mu.Unlock()
		if berr != nil {
			return // the body was cut short: nothing after it can be a request
		}
		if b.upgrade && req.Header.Get("Upgrade") != "" &&
			(!strings.EqualFold(req.Header.Get("Upgrade"), "h2c") || len(req.Header["Http2-Settings"]) == 1) {
			fmt.Fprintf(c, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", req.Header.Get("Upgrade"))
			continue
		}
		msg := "ok " + req.URL.Path
		if req.Method == "HEAD" {
			msg = ""
		}
		conn := ""
		if req.Close {
			conn = "Connection: close\r\n"
		}
		fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n%s\r\n%s", len(msg), conn, msg)
		if req.Close {
			return
		}
	}
}

func (b *accRawBackend) snapshot() (seen []accSeen, bad []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]accSeen(nil), b.seen...), append([]string(nil), b.bad...)
}

// accSmugEnv is a running BareProxy on localhost in front of a raw backend.
type accSmugEnv struct {
	t       *testing.T
	be      *accRawBackend
	addr    string // host:port of the proxy's listener
	host    string // the Host the site answers to
	logPath string
	tail    *accTail
}

func newAccSmugEnv(t *testing.T, beUpgrade bool) *accSmugEnv {
	t.Helper()
	be := newAccRawBackend(t, beUpgrade)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	dir := t.TempDir()
	e := &accSmugEnv{t: t, be: be, addr: ln.Addr().String(), host: fmt.Sprintf("smug.test:%d", port), logPath: filepath.Join(dir, "smug.log")}
	conf := filepath.Join(dir, "bareproxy.conf")
	accWrite(t, conf, fmt.Sprintf("global\n  admin off\n  trace-log %s\n  trace-query on\n\n"+
		"site http://smug.test:%d\n"+
		"  route /admin/* -> respond 403 \"blocked\"\n"+
		"  route /secret/* -> respond 403 \"blocked\"\n"+
		"  route /* -> backend\n\n"+
		"pool backend\n  backend %s\n", e.logPath, port, be.addr()))
	c, probs := Load(conf)
	if HasErrors(probs) {
		t.Fatalf("the smuggling config: %v", probs)
	}
	rt, err := NewRuntime(c, nil, 1, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(conf, rt)
	srv.logger.SetOutput(nopWriter{})
	// The settings Run gives every listener.
	hs := &http.Server{
		Handler:           srv.Handler(port, false),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	go hs.Serve(ln)
	t.Cleanup(func() { hs.Close(); rt.Stop(); rt.Trace.Close(); c.Close() })
	e.tail = newAccTail(t, e.logPath)
	return e
}

type accResp struct {
	status int
	hdr    http.Header
	body   []byte
}

// send writes the payload on a fresh connection and reads every response
// that comes back until the connection closes or goes quiet.
func (e *accSmugEnv) send(payload string, quiet time.Duration) []accResp {
	conn, err := net.Dial("tcp", e.addr)
	if err != nil {
		e.t.Fatal(err)
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	conn.Write([]byte(payload)) // the server may close before it has read all of a huge payload
	br := bufio.NewReader(conn)
	var out []accResp
	for {
		conn.SetReadDeadline(time.Now().Add(quiet))
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			return out
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 100 {
			continue
		}
		out = append(out, accResp{status: resp.StatusCode, hdr: resp.Header, body: body})
		if resp.Close || resp.StatusCode == 101 {
			return out
		}
	}
}

func (e *accSmugEnv) counters() [3]int {
	seen, bad := e.be.snapshot()
	var size int
	if fi, err := os.Stat(e.logPath); err == nil {
		size = int(fi.Size())
	}
	return [3]int{len(seen), len(bad), size}
}

// settle waits until the backend and the trace log have been quiet for a moment.
func (e *accSmugEnv) settle() {
	last, stable := e.counters(), 0
	for i := 0; i < 60 && stable < 3; i++ {
		time.Sleep(25 * time.Millisecond)
		if c := e.counters(); c == last {
			stable++
		} else {
			last, stable = c, 0
		}
	}
}

func (e *accSmugEnv) drainRecords() (recs []Record) {
	for {
		rec, _, err := e.tail.next()
		if err != nil {
			return recs
		}
		recs = append(recs, rec)
	}
}

type accSmug struct {
	name    string
	payload string
	mode    string // "one", "two", "blocked", "pipeblocked", "safe"
	path    string // the path the one request must have, for one and pipeblocked
	body    string // the body it must have, when not empty
	refuse  bool   // the standards say this must be refused: no request reaches the backend and the client gets an error
	noReply bool   // a refusal may arrive as a closed connection with no response
	extra   func(r *accSmugResult) string
}

type accSmugResult struct {
	env   *accSmugEnv
	resps []accResp
	seen  []accSeen
	bad   []string
	recs  []Record
}

func (r *accSmugResult) statuses() []int {
	var s []int
	for _, x := range r.resps {
		s = append(s, x.status)
	}
	return s
}

// outcome says in a few words what happened to a payload.
func (r *accSmugResult) outcome() string {
	var parts []string
	parts = append(parts, fmt.Sprintf("client %v", r.statuses()))
	var bs []string
	for _, s := range r.seen {
		x := s.method + " " + s.uri
		if s.bodyErr != nil {
			x += " (body cut short)"
		} else if len(s.body) > 0 {
			x += fmt.Sprintf(" (%d body bytes)", len(s.body))
		}
		bs = append(bs, x)
	}
	if len(bs) == 0 {
		bs = []string{"nothing"}
	}
	parts = append(parts, "backend: "+strings.Join(bs, "; "))
	parts = append(parts, fmt.Sprintf("%d records", len(r.recs)))
	return strings.Join(parts, ", ")
}

// accUnder reports whether a request path, as sent, is dir or below it once
// normalized. The path is origin-form: it is never read as a URL reference.
func accUnder(p, dir string) bool {
	p, _, _ = strings.Cut(p, "?")
	n, err := NormalizePath(p, true)
	if err != nil {
		n = p
	}
	n = strings.ToLower(n)
	return n == dir || strings.HasPrefix(n, dir+"/")
}

func (e *accSmugEnv) run(c accSmug, quiet time.Duration) *accSmugResult {
	e.t.Helper()
	before, badBefore := e.be.snapshot()
	e.drainRecords() // anything left over from the last case
	resps := e.send(c.payload, quiet)
	e.settle()
	all, bad := e.be.snapshot()
	return &accSmugResult{env: e, resps: resps, seen: all[len(before):], bad: bad[len(badBefore):], recs: e.drainRecords()}
}

// check applies the rules every payload must meet, and those of its mode.
func (r *accSmugResult) check(c accSmug) (problems []string) {
	add := func(f string, a ...any) { problems = append(problems, fmt.Sprintf(f, a...)) }
	maxReq := 1
	if c.mode == "two" {
		maxReq = 2
	}
	if len(r.seen) > maxReq {
		add("the backend got %d requests, at most %d are allowed", len(r.seen), maxReq)
	}
	for _, s := range r.seen {
		if accUnder(s.uri, "/admin") || accUnder(s.uri, "/secret") {
			add("a request for %q reached the backend; its rule says 403", s.uri)
		}
		if len(s.te) > 0 && s.hdr.Get("Content-Length") != "" {
			add("the backend got both Transfer-Encoding and Content-Length")
		}
		if len(s.hdr["Content-Length"]) > 1 {
			add("the backend got %d Content-Length headers", len(s.hdr["Content-Length"]))
		}
		if s.hdr.Get("X-Forwarded-For") == "" && c.mode != "skipxff" {
			add("the backend got no X-Forwarded-For")
		}
	}
	// Every response BareProxy made has one record, and every record one response.
	own := 0
	ids := map[string]bool{}
	for _, x := range r.resps {
		if id := x.hdr.Get("BareProxy-Id"); id != "" && x.status != 101 {
			own++
			ids[id] = true
		}
	}
	if own != len(r.recs) {
		add("the client got %d responses from BareProxy and the log has %d records", own, len(r.recs))
	}
	for _, rec := range r.recs {
		if !ids[rec.ID] && rec.Outcome != "client_gone" && rec.Outcome != "upgraded" {
			add("record %s has no response with its ID", rec.ID)
		}
		if accUnder(rec.Path, "/admin") || accUnder(rec.Path, "/secret") {
			if rec.Status != 403 || rec.Pool != "" {
				add("record %s: %s was not refused by its rule (status %d, pool %q)", rec.ID, rec.Path, rec.Status, rec.Pool)
			}
		}
	}
	if len(r.bad) > 0 {
		add("the backend received bytes it could not parse as a request: %v", r.bad)
	}
	first := 0
	if len(r.resps) > 0 {
		first = r.resps[0].status
	}
	switch c.mode {
	case "one":
		if len(r.seen) != 1 || r.seen[0].bodyErr != nil || accPathOf(r.seen[0].uri) != c.path {
			add("want exactly one complete request for %s at the backend", c.path)
		}
		if len(r.seen) == 1 && c.body != "" && string(r.seen[0].body) != c.body {
			add("the backend's body is %q, want %q", r.seen[0].body, c.body)
		}
		if first != 200 {
			add("the client got %v, want 200 first", r.statuses())
		}
	case "two":
		if len(r.seen) != 2 || len(r.resps) != 2 || len(r.recs) != 2 {
			add("want two requests, two responses and two records; got %d, %d and %d", len(r.seen), len(r.resps), len(r.recs))
		}
	case "blocked":
		if len(r.seen) != 0 || first != 403 || len(r.resps) != 1 {
			add("want the rule's 403 and nothing at the backend")
		}
	case "pipeblocked":
		if len(r.seen) != 1 || accPathOf(r.seen[0].uri) != c.path || len(r.resps) != 2 || r.resps[0].status != 200 || r.resps[1].status != 403 {
			add("want one request for %s at the backend, then the rule's 403 for the second", c.path)
		}
	}
	if c.refuse {
		complete := 0
		for _, s := range r.seen {
			if s.bodyErr == nil {
				complete++
			}
		}
		switch {
		case len(r.seen) != 0:
			add("this must be refused, but %d request(s) reached the backend", len(r.seen))
		case len(r.resps) == 0 && !c.noReply:
			add("this must be refused with an error response, but the client got none")
		case len(r.resps) > 0 && first < 400:
			add("this must be refused, but the client got %d", first)
		}
	}
	if c.extra != nil {
		if msg := c.extra(r); msg != "" {
			add("%s", msg)
		}
	}
	return problems
}

func accPathOf(uri string) string {
	p, _, _ := strings.Cut(uri, "?")
	return p
}

func accReq(line string, hdrs []string, body string) string {
	var b strings.Builder
	b.WriteString(line + "\r\n")
	for _, h := range hdrs {
		b.WriteString(h + "\r\n")
	}
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.String()
}

func accSmuggleCases(H string) []accSmug {
	host := "Host: " + H
	evil := "GET /admin HTTP/1.1\r\nHost: " + H + "\r\nConnection: close\r\n\r\n"
	chunk := func(s string) string { return fmt.Sprintf("%x\r\n%s\r\n", len(s), s) }
	const last = "0\r\n\r\n"
	post := func(extra []string, body string) string {
		return accReq("POST /api/x HTTP/1.1", append([]string{host}, extra...), body)
	}
	get := func(path string, extra ...string) string {
		return accReq("GET "+path+" HTTP/1.1", append([]string{host}, extra...), "")
	}
	chunked := []string{"Transfer-Encoding: chunked"}
	noBackendHeader := func(name string) func(*accSmugResult) string {
		return func(r *accSmugResult) string {
			for _, s := range r.seen {
				if s.hdr.Get(name) != "" || len(s.te) > 0 && name == "Transfer-Encoding" {
					return fmt.Sprintf("the backend got a %s header", name)
				}
			}
			return ""
		}
	}
	forwarded := func(r *accSmugResult) string {
		if len(r.seen) != 1 || len(r.recs) != 1 {
			return "want one request and one record"
		}
		s, rec := r.seen[0], r.recs[0]
		switch {
		case s.hdr.Get("Bareproxy-Id") != rec.ID:
			return fmt.Sprintf("the backend got BareProxy-Id %q, the record has %q", s.hdr.Get("Bareproxy-Id"), rec.ID)
		case s.hdr.Get("X-Forwarded-For") != "127.0.0.1":
			return fmt.Sprintf("the backend got X-Forwarded-For %q, want 127.0.0.1", s.hdr.Get("X-Forwarded-For"))
		case s.hdr.Get("Forwarded") != "":
			return fmt.Sprintf("the backend got Forwarded %q", s.hdr.Get("Forwarded"))
		case s.hdr.Get("X-Forwarded-Host") != H:
			return fmt.Sprintf("the backend got X-Forwarded-Host %q, want %q", s.hdr.Get("X-Forwarded-Host"), H)
		case s.hdr.Get("X-Forwarded-Proto") != "http":
			return fmt.Sprintf("the backend got X-Forwarded-Proto %q", s.hdr.Get("X-Forwarded-Proto"))
		}
		return ""
	}
	hostIs := func(want string) func(*accSmugResult) string {
		return func(r *accSmugResult) string {
			for _, s := range r.seen {
				if s.host != want {
					return fmt.Sprintf("the backend got Host %q, want %q", s.host, want)
				}
			}
			return ""
		}
	}
	status := func(want int) func(*accSmugResult) string {
		return func(r *accSmugResult) string {
			if len(r.resps) == 0 || r.resps[0].status != want {
				return fmt.Sprintf("the client got %v, want %d", r.statuses(), want)
			}
			return ""
		}
	}
	cl := func(v ...string) []string {
		var h []string
		for _, x := range v {
			h = append(h, "Content-Length: "+x)
		}
		return h
	}
	return []accSmug{
		// Controls: the harness sees one request as one, two as two, and the rules answer /admin.
		{name: "control: one GET", payload: get("/api/x", "Connection: close"), mode: "one", path: "/api/x"},
		{name: "control: POST with a Content-Length body", payload: post(append(cl("5"), "Connection: close"), "hello"), mode: "one", path: "/api/x", body: "hello"},
		{name: "control: POST with a chunked body", payload: post(append(chunked, "Connection: close"), chunk("hello")+last), mode: "one", path: "/api/x", body: "hello"},
		{name: "control: POST whose body looks like a request", payload: post(append(cl(strconv.Itoa(len(evil))), "Connection: close"), evil), mode: "one", path: "/api/x", body: evil},
		{name: "control: Expect 100-continue", payload: post(append(cl("5"), "Expect: 100-continue", "Connection: close"), "hello"), mode: "one", path: "/api/x", body: "hello"},
		{name: "control: two pipelined GETs", payload: get("/api/a") + get("/api/b", "Connection: close"), mode: "two"},
		{name: "control: GET /admin", payload: get("/admin", "Connection: close"), mode: "blocked"},
		{name: "control: GET /%61dmin", payload: get("/%61dmin", "Connection: close"), mode: "blocked"},
		{name: "control: GET //admin/", payload: get("//admin/", "Connection: close"), mode: "blocked"},
		{name: "control: GET /api/../secret/x", payload: get("/api/../secret/x", "Connection: close"), mode: "blocked"},
		{name: "control: a GET and a pipelined /admin", payload: get("/api/a") + evil, mode: "pipeblocked", path: "/api/a"},

		// Content-Length with Transfer-Encoding.
		{name: "CL.TE: Content-Length ends inside the chunked body", payload: post(append(cl("6"), chunked...), last+evil), mode: "safe"},
		{name: "TE.CL: the chunk holds the second request", payload: post(append(cl("4"), chunked...), chunk(evil)+last), mode: "safe"},
		{name: "CL.0: Content-Length 0 and a request as the body", payload: post(cl("0"), evil), mode: "pipeblocked", path: "/api/x"},
		{name: "TE.TE: Transfer-Encoding xchunked", payload: post(append(cl("4"), "Transfer-Encoding: xchunked"), chunk(evil)+last), mode: "safe", refuse: true},
		{name: "TE.TE: Transfer-Encoding twice, chunked then identity", payload: post(append(chunked, "Transfer-Encoding: identity"), chunk("hello")+last+evil), mode: "safe", refuse: true},
		{name: "TE: identity, chunked", payload: post([]string{"Transfer-Encoding: identity, chunked"}, chunk("hello")+last+evil), mode: "safe", refuse: true},
		{name: "TE: chunked, identity (chunked not last)", payload: post([]string{"Transfer-Encoding: chunked, identity"}, chunk("hello")+last+evil), mode: "safe", refuse: true},
		{name: "TE: tab before the value", payload: post(append(cl("4"), "Transfer-Encoding:\tchunked"), chunk("hello")+last+evil), mode: "safe"},
		{name: "TE: space before the colon", payload: post(append(cl("4"), "Transfer-Encoding : chunked"), chunk(evil)+last), mode: "safe", refuse: true},
		{name: "TE: leading space folds it into Content-Length", payload: accReq("POST /api/x HTTP/1.1", []string{host, "Content-Length: 4", " Transfer-Encoding: chunked"}, chunk(evil)+last), mode: "safe", refuse: true},
		{name: "TE: CHUNKED in capitals", payload: post(append(cl("4"), "Transfer-Encoding: CHUNKED"), chunk("hello")+last+evil), mode: "safe"},
		{name: "TE: chunked with a parameter", payload: post(append(cl("4"), "Transfer-Encoding: chunked;q=1"), chunk(evil)+last), mode: "safe"},
		{name: "TE: chunked in quotes", payload: post(append(cl("4"), `Transfer-Encoding: "chunked"`), chunk(evil)+last), mode: "safe", refuse: true},

		// Two or odd Content-Lengths.
		{name: "CL: two different values", payload: post(cl("5", "6"), "hello"+evil), mode: "safe", refuse: true},
		{name: "CL: a list (5, 6)", payload: post(cl("5, 6"), "hello"+evil), mode: "safe", refuse: true},
		{name: "CL: the same value twice", payload: post(cl("5", "5"), "hello"+evil), mode: "pipeblocked", path: "/api/x"},
		{name: "CL: plus sign", payload: post(cl("+5"), "hello"+evil), mode: "safe", refuse: true},
		{name: "CL: hexadecimal", payload: post(cl("0x5"), "hello"+evil), mode: "safe", refuse: true},
		{name: "CL: negative", payload: post(cl("-1"), "hello"+evil), mode: "safe", refuse: true},
		{name: "CL: a space inside", payload: post(cl("5 5"), "hello"+evil), mode: "safe", refuse: true},
		{name: "CL: leading zeros", payload: post(cl("005"), "hello"+evil), mode: "pipeblocked", path: "/api/x"},
		{name: "CL: empty value", payload: post(cl(""), "hello"+evil), mode: "safe"},
		{name: "CL: name with an underscore as a decoy", payload: post([]string{"Content-Length: 5", "Content_Length: 100"}, "hello"+evil), mode: "pipeblocked", path: "/api/x"},
		{name: "CL: on a GET, the next request in its body", payload: accReq("GET /api/x HTTP/1.1", []string{host, "Content-Length: 30"}, evil), mode: "safe"},

		// Chunk sizes and chunk framing, with a request after the body.
		{name: "chunk size not hexadecimal", payload: post(chunked, "ZZ\r\nhello\r\n0\r\n\r\n"+evil), mode: "safe"},
		{name: "chunk size negative", payload: post(chunked, "-5\r\nhello\r\n0\r\n\r\n"+evil), mode: "safe"},
		{name: "chunk size with a plus", payload: post(chunked, "+5\r\nhello\r\n0\r\n\r\n"+evil), mode: "safe"},
		{name: "chunk size 0x5", payload: post(chunked, "0x5\r\nhello\r\n0\r\n\r\n"+evil), mode: "safe"},
		{name: "chunk size with a leading space", payload: post(chunked, " 5\r\nhello\r\n0\r\n\r\n"+evil), mode: "safe"},
		{name: "chunk size with a trailing space", payload: post(chunked, "5 \r\nhello\r\n0\r\n\r\n"+evil), mode: "safe"},
		{name: "chunk extension", payload: post(chunked, "5;a=b\r\nhello\r\n0\r\n\r\n"+evil), mode: "safe"},
		{name: "chunk size of 17 hex digits", payload: post(chunked, "FFFFFFFFFFFFFFFFF\r\nhello\r\n0\r\n\r\n"+evil), mode: "safe"},
		{name: "empty chunk size line", payload: post(chunked, "\r\nhello\r\n0\r\n\r\n"+evil), mode: "safe"},
		{name: "bare LF in the chunked body", payload: post(chunked, "5\nhello\n0\n\n"+evil), mode: "safe"},
		{name: "no CRLF after the chunk data", payload: post(chunked, "5\r\nhelloXX0\r\n\r\n"+evil), mode: "safe"},
		{name: "chunk data longer than its size", payload: post(chunked, "3\r\nhello\r\n0\r\n\r\n"+evil), mode: "safe"},
		{name: "chunk data shorter than its size", payload: post(chunked, "9\r\nhello\r\n0\r\n\r\n"+evil), mode: "safe"},
		{name: "a request in a chunk trailer", payload: post(chunked, "5\r\nhello\r\n0\r\nX-Trailer: GET /admin HTTP/1.1\r\n\r\n"+evil), mode: "safe"},
		{name: "a request line as a trailer line", payload: post(chunked, "5\r\nhello\r\n0\r\nGET /admin HTTP/1.1\r\nHost: "+H+"\r\n\r\n"), mode: "safe"},
		{name: "a request right after the last chunk", payload: post(chunked, "5\r\nhello\r\n0\r\n\r\n"+evil), mode: "pipeblocked", path: "/api/x"},

		// Folded header lines.
		{name: "obs-fold: a folded line that looks like Transfer-Encoding", payload: accReq("GET /api/x HTTP/1.1", []string{host, "X-Foo: bar", " Transfer-Encoding: chunked", "Connection: close"}, ""), mode: "safe", extra: noBackendHeader("Transfer-Encoding")},
		{name: "obs-fold: a folded line after Host", payload: accReq("GET /api/x HTTP/1.1", []string{host, " , evil.test", "Connection: close"}, ""), mode: "safe", refuse: true},
		{name: "obs-fold: a folded line after Content-Length", payload: accReq("POST /api/x HTTP/1.1", []string{host, "Content-Length: 5", " 0", "Connection: close"}, "hello"), mode: "safe", refuse: true},
		{name: "obs-fold: a tab continuation", payload: accReq("GET /api/x HTTP/1.1", []string{host, "X-Foo: bar", "\tContent-Length: 40", "Connection: close"}, ""), mode: "safe", extra: noBackendHeader("Content-Length")},

		// Absolute-form request lines and other request targets.
		{name: "absolute-form: our host", payload: accReq("GET http://"+H+"/api/x HTTP/1.1", []string{host, "Connection: close"}, ""), mode: "one", path: "/api/x"},
		{name: "absolute-form: /admin", payload: accReq("GET http://"+H+"/admin HTTP/1.1", []string{host, "Connection: close"}, ""), mode: "blocked"},
		{name: "absolute-form: /%61dmin", payload: accReq("GET http://"+H+"/%61dmin HTTP/1.1", []string{host, "Connection: close"}, ""), mode: "blocked"},
		{name: "absolute-form: //admin", payload: accReq("GET http://"+H+"//admin HTTP/1.1", []string{host, "Connection: close"}, ""), mode: "blocked"},
		{name: "absolute-form: /api/../admin", payload: accReq("GET http://"+H+"/api/../admin HTTP/1.1", []string{host, "Connection: close"}, ""), mode: "blocked"},
		{name: "absolute-form: userinfo and /admin", payload: accReq("GET http://user:pw@"+H+"/admin HTTP/1.1", []string{host, "Connection: close"}, ""), mode: "blocked"},
		{name: "absolute-form: HTTP in capitals", payload: accReq("GET HTTP://"+H+"/admin HTTP/1.1", []string{host, "Connection: close"}, ""), mode: "blocked"},
		{name: "absolute-form: another scheme", payload: accReq("GET ftp://"+H+"/admin HTTP/1.1", []string{host, "Connection: close"}, ""), mode: "safe"},
		{name: "absolute-form: another host, our Host header", payload: accReq("GET http://evil.test/api/x HTTP/1.1", []string{host, "Connection: close"}, ""), mode: "safe", refuse: true, extra: status(421)},
		{name: "absolute-form: our host, another Host header", payload: accReq("GET http://"+H+"/api/x HTTP/1.1", []string{"Host: evil.test", "Connection: close"}, ""), mode: "one", path: "/api/x", extra: hostIs(H)},
		{name: "target //host/admin (network-path reference)", payload: accReq("GET //"+H+"/admin HTTP/1.1", []string{host, "Connection: close"}, ""), mode: "safe"},
		{name: "target * with GET", payload: accReq("GET * HTTP/1.1", []string{host, "Connection: close"}, ""), mode: "safe", refuse: true},
		{name: "OPTIONS * (Go answers it itself)", payload: accReq("OPTIONS * HTTP/1.1", []string{host, "Connection: close"}, ""), mode: "safe"},
		{name: "CONNECT", payload: accReq("CONNECT "+H+" HTTP/1.1", []string{host, "Connection: close"}, ""), mode: "safe"},
		{name: "relative target without a slash", payload: accReq("GET admin HTTP/1.1", []string{host, "Connection: close"}, ""), mode: "safe", refuse: true},
		{name: "target with a space: two paths", payload: accReq("GET /api/x /admin HTTP/1.1", []string{host, "Connection: close"}, ""), mode: "safe", refuse: true},
		{name: "tabs in the request line", payload: accReq("GET\t/api/x\tHTTP/1.1", []string{host, "Connection: close"}, ""), mode: "safe", refuse: true},
		{name: "NUL in the target", payload: accReq("GET /api/x\x00/admin HTTP/1.1", []string{host, "Connection: close"}, ""), mode: "safe", refuse: true},
		{name: "lower-case method", payload: accReq("get /api/x HTTP/1.1", []string{host, "Connection: close"}, ""), mode: "safe"},
		{name: "HTTP version 9.9", payload: accReq("GET /api/x HTTP/9.9", []string{host, "Connection: close"}, ""), mode: "safe"},
		{name: "bare LF line endings", payload: "GET /api/x HTTP/1.1\nHost: " + H + "\nConnection: close\n\n", mode: "safe"},

		// Host and header oddities.
		{name: "no Host header in HTTP/1.1", payload: accReq("GET /api/x HTTP/1.1", []string{"Connection: close"}, ""), mode: "safe", refuse: true},
		{name: "no Host header in HTTP/1.0", payload: accReq("GET /api/x HTTP/1.0", nil, ""), mode: "safe", refuse: true},
		{name: "two Host headers", payload: accReq("GET /api/x HTTP/1.1", []string{host, "Host: evil.test", "Connection: close"}, ""), mode: "safe", refuse: true},
		{name: "space before the colon in Host", payload: accReq("GET /api/x HTTP/1.1", []string{"Host : " + H, "Connection: close"}, ""), mode: "safe", refuse: true},
		{name: "bare CR in a header value", payload: accReq("GET /api/x HTTP/1.1", []string{host, "X-A: b\rGET /admin HTTP/1.1", "Connection: close"}, ""), mode: "safe"},
		{name: "NUL in a header value", payload: accReq("GET /api/x HTTP/1.1", []string{host, "X-A: b\x00c", "Connection: close"}, ""), mode: "safe"},
		{name: "header name with a space", payload: accReq("GET /api/x HTTP/1.1", []string{host, "X A: b", "Connection: close"}, ""), mode: "safe", refuse: true},
		{name: "oversized request line (60 KB)", payload: get("/api/"+strings.Repeat("a", 60000), "Connection: close"), mode: "safe", refuse: true, noReply: true},
		{name: "headers past 32 KB", payload: accReq("GET /api/x HTTP/1.1", append([]string{host, "Connection: close"}, func() []string {
			var hs []string
			for i := 0; i < 400; i++ {
				hs = append(hs, fmt.Sprintf("X-Pad-%d: %s", i, strings.Repeat("p", 90)))
			}
			return hs
		}()...), ""), mode: "safe", refuse: true, noReply: true},

		// Hop-by-hop headers and the headers BareProxy sets.
		{name: "hop-by-hop: Connection names Content-Length", payload: post(append(cl("5"), "Connection: Content-Length, close"), "hello"), mode: "one", path: "/api/x", body: "hello"},
		{name: "hop-by-hop: Connection names Transfer-Encoding", payload: post(append(chunked, "Connection: Transfer-Encoding, close"), chunk("hello")+last), mode: "one", path: "/api/x", body: "hello"},
		{name: "hop-by-hop: Connection names the headers BareProxy sets", payload: get("/api/x", "Connection: X-Forwarded-For, BareProxy-Id, X-Forwarded-Host, X-Forwarded-Proto, close"), mode: "one", path: "/api/x", extra: forwarded},
		{name: "client-sent forwarding headers are replaced", payload: get("/api/x", "BareProxy-Id: 0000000000000000", "X-Forwarded-For: 6.6.6.6", "Forwarded: for=6.6.6.6", "X-Forwarded-Host: evil.test", "X-Forwarded-Proto: https", "Connection: close"), mode: "one", path: "/api/x", extra: forwarded},
		{name: "Upgrade: websocket to a backend that answers 200, then /admin", payload: get("/api/x", "Connection: Upgrade", "Upgrade: websocket", "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==", "Sec-WebSocket-Version: 13") + evil, mode: "pipeblocked", path: "/api/x"},
		{name: "Upgrade: h2c to a backend that answers 200, then /admin", payload: get("/api/x", "Connection: Upgrade, HTTP2-Settings", "Upgrade: h2c", "HTTP2-Settings: AAMAAABkAAQAAP__") + evil, mode: "pipeblocked", path: "/api/x"},
	}
}

// TestAcceptSmuggling is group 5: every payload ends as a refusal or as at
// most one well-formed request at the backend, and no path rule is bypassed.
func TestAcceptSmuggling(t *testing.T) {
	e := newAccSmugEnv(t, false)
	cases := accSmuggleCases(e.host)
	var table []string
	refused, forwarded, blocked, failed := 0, 0, 0, 0
	for _, c := range cases {
		r := e.run(c, 200*time.Millisecond)
		probs := r.check(c)
		kind := "?"
		switch {
		case len(r.seen) == 0 && len(r.resps) > 0 && r.resps[0].status == 403:
			kind, blocked = "answered by a rule", blocked+1
		case len(r.seen) == 0:
			kind, refused = "refused", refused+1
		default:
			kind, forwarded = "forwarded", forwarded+1
		}
		table = append(table, fmt.Sprintf("  %-70s %-18s %s", c.name, kind, r.outcome()))
		if len(probs) > 0 {
			failed++
			t.Errorf("%s:\n    %s\n    payload: %.300q\n    outcome: %s", c.name, strings.Join(probs, "\n    "), c.payload, r.outcome())
		}
	}
	t.Logf("%d payloads: %d refused before the backend, %d answered by a rule, %d forwarded as one request; %d problems\n%s",
		len(cases), refused, blocked, forwarded, failed, strings.Join(table, "\n"))
	seen, bad := e.be.snapshot()
	t.Logf("the backend parsed %d requests in all and could not parse %d times", len(seen), len(bad))
}

// TestAcceptSmugglingQuery sends queries a parser might rewrite or drop
// (semicolons, broken escapes, empty parts, a second question mark) through
// the real listener. The backend must get them exactly as sent.
func TestAcceptSmugglingQuery(t *testing.T) {
	e := newAccSmugEnv(t, false)
	queries := []string{"a=1;b=2", "a=%zz", "a=%", "%", "&&&", "a=1&a=2", "a=b?c", "x=/../y", "=", "a=%2F%2e%2e", ";", "a=1&;b=2", "a=%u0041", "?", "a=b#c", "a=%0d%0aGET%20/admin%20HTTP/1.1", "a=%0D%0A%0D%0A", "q=\\", "a[]=1&a[]=2", "a=%E2%9C%93", "a=%c0%ae", "x=1&&y=2", "a==b", "%00"}
	for _, q := range queries {
		c := accSmug{name: "query " + q, payload: accReq("GET /api/x?"+q+" HTTP/1.1", []string{"Host: " + e.host, "Connection: close"}, ""), mode: "one", path: "/api/x"}
		r := e.run(c, 200*time.Millisecond)
		for _, p := range r.check(c) {
			t.Errorf("?%s: %s (%s)", q, p, r.outcome())
		}
		if len(r.seen) == 1 {
			if _, got, _ := strings.Cut(r.seen[0].uri, "?"); got != strings.SplitN(q, "#", 2)[0] && !strings.Contains(q, "#") {
				t.Errorf("query %q reached the backend as %q", q, got)
			}
		}
		if len(r.recs) == 1 && !strings.Contains(q, "#") && r.recs[0].Query != q {
			t.Errorf("query %q was recorded as %q", q, r.recs[0].Query)
		}
	}
}

// TestAcceptSmugglingUpgrade covers a backend that agrees to an upgrade (a
// 101), which turns the connection into a tunnel the rules can't see into.
// Only WebSocket may do that. Any other protocol, h2c above all, carries
// requests of its own that the path rules would never see, so BareProxy takes
// the Upgrade headers off and the backend gets a plain request.
func TestAcceptSmugglingUpgrade(t *testing.T) {
	const h2s = "HTTP2-Settings: AAMAAABkAAQAAP__"
	ws := []string{"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==", "Sec-WebSocket-Version: 13"}
	for _, c := range []struct {
		name   string
		hdr    []string
		tunnel bool
	}{
		{"websocket", append([]string{"Connection: Upgrade", "Upgrade: websocket"}, ws...), true},
		{"WebSocket in capitals, Connection with another token", append([]string{"Connection: keep-alive, Upgrade", "Upgrade: WebSocket"}, ws...), true},
		{"h2c, HTTP2-Settings not named in Connection", []string{"Connection: Upgrade", "Upgrade: h2c", h2s}, false},
		{"h2c, HTTP2-Settings named in Connection", []string{"Connection: Upgrade, HTTP2-Settings", "Upgrade: h2c", h2s}, false},
		{"an unknown protocol", []string{"Connection: Upgrade", "Upgrade: foo/1"}, false},
		{"websocket and h2c in one Upgrade header", []string{"Connection: Upgrade", "Upgrade: websocket, h2c", h2s}, false},
		{"websocket and h2c in two Upgrade headers", []string{"Connection: Upgrade", "Upgrade: websocket", "Upgrade: h2c", h2s}, false},
		{"Upgrade: h2c without Connection: Upgrade", []string{"Upgrade: h2c", h2s}, false},
	} {
		e := newAccSmugEnv(t, true)
		hdr := append([]string{"Host: " + e.host}, c.hdr...)
		conn, err := net.Dial("tcp", e.addr)
		if err != nil {
			t.Fatal(err)
		}
		conn.Write([]byte(accReq("GET /api/x HTTP/1.1", hdr, "")))
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		status, second := 0, 0
		if err == nil {
			status = resp.StatusCode
		}
		evil := "GET /admin HTTP/1.1\r\nHost: " + e.host + "\r\n\r\n"
		if status == 101 {
			// Whatever the client sends now goes through the tunnel.
			conn.Write([]byte(evil))
			time.Sleep(200 * time.Millisecond)
		} else if status == 200 {
			// No tunnel: the next request on the connection is an ordinary one.
			io.Copy(io.Discard, resp.Body)
			conn.Write([]byte(evil))
			if r2, err := http.ReadResponse(br, nil); err == nil {
				second = r2.StatusCode
			}
		}
		conn.Close()
		e.settle()
		seen, _ := e.be.snapshot()
		admin := 0
		for _, s := range seen {
			if accUnder(s.uri, "/admin") {
				admin++
			}
		}
		t.Logf("Upgrade, %s: client got %d (then %d), backend parsed %d request(s), %d for /admin", c.name, status, second, len(seen), admin)
		if c.tunnel {
			if status != 101 {
				t.Errorf("%s: a WebSocket upgrade should open a tunnel when the backend answers 101, got %d", c.name, status)
			}
			continue
		}
		if status != 200 || second != 403 || admin != 0 || len(seen) != 1 {
			t.Errorf("%s: want a plain 200, then 403 for /admin from BareProxy, one request at the backend and none for /admin; got %d then %d, %d requests, %d for /admin", c.name, status, second, len(seen), admin)
		}
		if len(seen) > 0 {
			h := seen[0].hdr
			if h.Get("Upgrade") != "" || len(h["Http2-Settings"]) > 0 || hasToken(h.Values("Connection"), "upgrade") || hasToken(h.Values("Connection"), "http2-settings") {
				t.Errorf("%s: the backend should get a plain request, got Connection %q, Upgrade %q, HTTP2-Settings %q", c.name, h.Values("Connection"), h.Values("Upgrade"), h.Values("Http2-Settings"))
			}
		}
	}
}

// TestAcceptSmugglingEarlyResponse: a backend that answers before it has read
// the request body must not leave body bytes that BareProxy then reads as the
// next request.
func TestAcceptSmugglingEarlyResponse(t *testing.T) {
	e := newAccSmugEnv(t, false)
	e.be.mu.Lock()
	e.be.early = true
	e.be.mu.Unlock()
	evil := "GET /admin HTTP/1.1\r\nHost: " + e.host + "\r\n\r\n"
	chunk := func(s string) string { return fmt.Sprintf("%x\r\n%s\r\n", len(s), s) }
	for _, c := range []accSmug{
		{name: "early reply, Content-Length body that is a request", payload: accReq("POST /api/x HTTP/1.1", []string{"Host: " + e.host, "Content-Length: " + strconv.Itoa(len(evil))}, evil), mode: "safe"},
		{name: "early reply, chunked body that is a request", payload: accReq("POST /api/x HTTP/1.1", []string{"Host: " + e.host, "Transfer-Encoding: chunked"}, chunk(evil)+"0\r\n\r\n"), mode: "safe"},
		{name: "early reply, big body with a request at its end", payload: accReq("POST /api/x HTTP/1.1", []string{"Host: " + e.host, "Content-Length: " + strconv.Itoa(100000+len(evil))}, strings.Repeat("x", 100000)+evil), mode: "safe"},
	} {
		r := e.run(c, 300*time.Millisecond)
		for _, p := range r.check(c) {
			t.Errorf("%s: %s (%s)", c.name, p, r.outcome())
		}
		t.Logf("%s: %s", c.name, r.outcome())
	}
}

// TestAcceptSmugglingPipeline sends 60 requests in one write, alternating a
// path that goes to the backend and one that a rule answers. Responses come
// back in order, each request has one record, and only the first kind reaches
// the backend.
func TestAcceptSmugglingPipeline(t *testing.T) {
	e := newAccSmugEnv(t, false)
	var payload strings.Builder
	n := 60
	for i := 0; i < n; i++ {
		path := fmt.Sprintf("/api/%d", i)
		if i%2 == 1 {
			path = fmt.Sprintf("/admin/%d", i)
		}
		extra := ""
		if i == n-1 {
			extra = "Connection: close\r\n"
		}
		fmt.Fprintf(&payload, "GET %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", path, e.host, extra)
	}
	resps := e.send(payload.String(), 500*time.Millisecond)
	e.settle()
	seen, bad := e.be.snapshot()
	recs := e.drainRecords()
	if len(resps) != n || len(recs) != n || len(seen) != n/2 || len(bad) != 0 {
		t.Fatalf("%d pipelined requests: %d responses, %d records, %d requests at the backend (want %d, %d, %d), %d unparsable", n, len(resps), len(recs), len(seen), n, n, n/2, len(bad))
	}
	for i, r := range resps {
		if i%2 == 0 && (r.status != 200 || string(r.body) != fmt.Sprintf("ok /api/%d", i)) {
			t.Errorf("response %d is %d %q, want 200 \"ok /api/%d\"", i, r.status, r.body, i)
		}
		if i%2 == 1 && r.status != 403 {
			t.Errorf("response %d is %d, want the rule's 403", i, r.status)
		}
		if r.hdr.Get("BareProxy-Id") != recs[i].ID {
			t.Errorf("response %d carries ID %s, record %d has %s", i, r.hdr.Get("BareProxy-Id"), i, recs[i].ID)
		}
	}
	t.Logf("%d pipelined requests in one write: %d responses in order, %d records, %d requests at the backend, none for /admin", n, len(resps), len(recs), len(seen))
}
