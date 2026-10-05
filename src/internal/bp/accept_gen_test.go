// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

// Helpers for the acceptance tests in accept_test.go: a generated config with
// several sites and many routes, a generator of requests, and a small
// reference implementation of the rules in the design note (section 5 and
// section 10), written apart from the code under test so the tests have
// something independent to compare with.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var accWords = []string{"api", "v1", "v2", "users", "admin", "static", "docs", "blog", "old", "new",
	"shop", "cart", "login", "beta", "health", "assets", "img", "about", "contact", "app"}

var accFileNames = []string{"index.html", "a.txt", "app.js", "404.html", ".git", ".well-known", ".hidden", "x.txt",
	"inner", "inner-abs", "escape", "escape-file", "photos", "sub", "app.js.gz", "app.js.br"}

var accOddSegments = []string{"caf%C3%A9", "a%20b", "a+b", "a:b", "x@y", "%7Eme", "%41bc", "~", "a%2Bb", "a%25b",
	"%E2%9C%93", "q%3Fx", "h%23x", "a;b", "a=b", "a,b", "a(b)", "a$b", "a%24b", "%FF", "UPPER", "MiXed"}

var accEvilSegments = []string{"..", "%2e%2e", "%2E%2e", ".%2e", "%2e.", ".", "%2e", "%2F", "%2f", "%5C", "%5c",
	"\\", "%00", "%0a", "%7f", "...", "..%2F", "%2e%2e%2f", "a%5Cb"}

var accQueries = []string{"a=1", "a=1&b=2", "q=%2e%2e", "x=/../y", "", "t=a%2Fb", "empty=", "z", "r=//x"}

var accMethodSets = [][]string{{"GET"}, {"GET", "HEAD"}, {"POST"}, {"PUT", "DELETE"}, {"PATCH"}, {"PURGE"},
	{"OPTIONS"}, {"GET", "POST"}, {"HEAD"}, {"DELETE"}}

// Methods of generated requests. Unusual ones are valid tokens that a Go
// server accepts and hands to the handler as sent.
var accRequestMethods = []string{"GET", "GET", "GET", "GET", "GET", "GET", "GET", "GET", "GET", "HEAD", "HEAD",
	"POST", "POST", "PUT", "DELETE", "PATCH", "OPTIONS", "PURGE", "PROPFIND", "TRACE", "M-SEARCH", "LINK"}

type accCond struct {
	name     string // canonical form
	text     string // as written in the config
	value    string
	hasValue bool
}

var accCondChoices = []accCond{
	{"X-Beta", "X-Beta", "", false},
	{"X-Beta", "X-Beta=1", "1", true},
	{"X-Beta", "x-beta=0", "0", true},
	{"X-Env", "X-Env=prod", "prod", true},
	{"X-Env", "X-ENV=staging", "staging", true},
	{"X-Tenant", "X-Tenant", "", false},
	{"Accept-Language", "Accept-Language=en", "en", true},
	{"X-Empty", "X-Empty=", "", true},
}

// Request headers: name, then the values a generated request may carry.
var accHeaderValues = []struct {
	name   string
	values []string
}{
	{"X-Beta", []string{"1", "0", "", "1,2", "true"}},
	{"X-Env", []string{"prod", "staging", "PROD", "prod ", "dev"}},
	{"X-Tenant", []string{"acme", "x"}},
	{"Accept-Language", []string{"en", "de", "en-US"}},
	{"X-Empty", []string{"", "x"}},
	{"X-Other", []string{"1", "2"}},
}

type accRoute struct {
	line    int
	text    string
	methods []string
	path    string // "" with prefix set is /*
	prefix  bool
	conds   []accCond
	kind    string // files, pool, redirect, respond
	folder  string
	pool    string
	strip   bool
	code    int // redirect code or respond status
	url     string
	bare    bool // the redirect URL is a bare origin: path and query are kept
	body    string
}

type accSite struct {
	line   int
	name   string
	port   int
	hosts  []string // exact hosts
	wild   string   // "wild.test" for *.wild.test
	catch  bool
	keep   bool
	err404 bool
	routes []*accRoute
}

// accWorld is a generated config, compiled, with a handler per port.
type accWorld struct {
	dir, conf, logPath, src string
	lines                   []string
	sites                   []*accSite
	cfg                     *Config
	rt                      *Runtime
	srv                     *Server
	handler                 map[int]http.Handler
	liveAddrs               []string
}

// accBackend is a test backend. It answers 200 and says in headers what it saw.
func accBackend(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Seen-Method", r.Method)
		h.Set("X-Seen-Uri", r.RequestURI)
		h.Set("X-Seen-Host", r.Host)
		h.Set("X-Seen-Id", r.Header.Get("BareProxy-Id"))
		w.WriteHeader(200)
		if r.Method != "HEAD" {
			w.Write([]byte("backend ok"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func accDeadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := ln.Addr().String()
	ln.Close()
	return a
}

func accWrite(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// accFolders makes three site folders with files named after the vocabulary,
// hidden names, precompressed copies, an empty folder and symlinks that stay
// inside, point outside and are absolute, plus bait outside the folders.
func accFolders(t *testing.T, dir string) {
	t.Helper()
	for k, name := range []string{"public1", "public2", "public3"} {
		pub := filepath.Join(dir, name)
		accWrite(t, pub+"/index.html", "<h1>home "+name+"</h1>")
		accWrite(t, pub+"/404.html", "<h1>not here "+name+"</h1>")
		accWrite(t, pub+"/app.js", "console.log('"+name+"')")
		accWrite(t, pub+"/app.js.gz", "GZ")
		accWrite(t, pub+"/app.js.br", "BR")
		accWrite(t, pub+"/a.txt", "text "+name)
		accWrite(t, pub+"/x.txt", "x "+name)
		accWrite(t, pub+"/.git/config", "secret git")
		accWrite(t, pub+"/.hidden", "secret hidden")
		accWrite(t, pub+"/.well-known/x.txt", "well known")
		for i, w := range accWords {
			if k == 1 && i%2 == 1 || k == 2 && i%4 != 0 {
				continue
			}
			accWrite(t, pub+"/"+w+"/index.html", "<h1>"+w+"</h1>")
			accWrite(t, pub+"/"+w+"/a.txt", "a "+w)
			accWrite(t, pub+"/"+w+"/sub/index.html", "<h1>sub "+w+"</h1>")
		}
		if err := os.MkdirAll(pub+"/photos", 0o755); err != nil {
			t.Fatal(err)
		}
		abs, _ := filepath.Abs(pub + "/docs")
		for _, l := range [][2]string{{"about", pub + "/inner"}, {abs, pub + "/inner-abs"}, {filepath.Join(dir, "outside"), pub + "/escape"},
			{"../outside/secret.txt", pub + "/escape-file"}} {
			if err := os.Symlink(l[0], l[1]); err != nil {
				t.Fatal(err)
			}
		}
	}
	accWrite(t, dir+"/outside/secret.txt", "OUTSIDE SECRET")
	accWrite(t, dir+"/outside/index.html", "OUTSIDE INDEX")
}

type accGen struct {
	rng *rand.Rand
}

func (g *accGen) pick(s []string) string { return s[g.rng.Intn(len(s))] }
func (g *accGen) chance(p float64) bool  { return g.rng.Float64() < p }

func (g *accGen) routePath() (path string, prefix bool) {
	d := 1
	switch r := g.rng.Float64(); {
	case r < 0.35:
		d = 2
	case r < 0.5:
		d = 3
	}
	segs := make([]string, d)
	for i := range segs {
		segs[i] = g.pick(accWords)
	}
	return "/" + strings.Join(segs, "/"), g.chance(0.55)
}

func (g *accGen) route(site *accSite, last bool) *accRoute {
	r := &accRoute{}
	if last {
		r.prefix = true
	} else if g.chance(0.04) {
		r.path = "/"
	} else {
		r.path, r.prefix = g.routePath()
	}
	if !last {
		if g.chance(0.3) {
			r.methods = accMethodSets[g.rng.Intn(len(accMethodSets))]
		}
		switch x := g.rng.Float64(); {
		case x < 0.2:
			r.conds = []accCond{accCondChoices[g.rng.Intn(len(accCondChoices))]}
		case x < 0.3:
			a, b := accCondChoices[g.rng.Intn(len(accCondChoices))], accCondChoices[g.rng.Intn(len(accCondChoices))]
			if a.name != b.name {
				r.conds = []accCond{a, b}
			}
		}
	}
	switch x := g.rng.Float64(); {
	case x < 0.34 || last && x < 0.7:
		r.kind, r.folder = "files", g.pick([]string{"public1", "public2", "public3"})
	case x < 0.58 || last:
		r.kind, r.pool = "pool", g.pick([]string{"live", "two", "dead", "mixed", "live", "two"})
		r.strip = g.chance(0.5)
	case x < 0.8:
		r.kind = "respond"
		r.code = []int{200, 200, 201, 202, 204, 301, 400, 403, 404, 410, 418, 429, 500, 503}[g.rng.Intn(14)]
		if g.chance(0.6) && r.code != 204 {
			r.body = g.pick([]string{"ok", "hello there", "maintenance", "no"})
		}
	default:
		r.kind = "redirect"
		r.code = []int{301, 302, 307, 308}[g.rng.Intn(4)]
		r.url = g.pick([]string{"https://example.org", "http://other.example:8080", "https://example.org/landing",
			"https://example.org/", "https://example.org/a/b?x=1", "https://moved.example"})
		u := strings.TrimPrefix(strings.TrimPrefix(r.url, "https://"), "http://")
		r.bare = !strings.Contains(u, "/")
	}
	return r
}

func (r *accRoute) render() string {
	var b strings.Builder
	b.WriteString("route")
	if len(r.methods) > 0 {
		b.WriteString(" " + strings.Join(r.methods, ","))
	}
	switch {
	case r.prefix && r.path == "":
		b.WriteString(" /*")
	case r.prefix:
		b.WriteString(" " + r.path + "/*")
	default:
		b.WriteString(" " + r.path)
	}
	for _, c := range r.conds {
		b.WriteString(" header " + c.text)
	}
	b.WriteString(" -> ")
	switch r.kind {
	case "files":
		b.WriteString("files " + r.folder)
	case "pool":
		b.WriteString(r.pool)
		if r.strip {
			b.WriteString(" strip")
		}
	case "respond":
		fmt.Fprintf(&b, "respond %d", r.code)
		if r.body != "" {
			fmt.Fprintf(&b, " %q", r.body)
		}
	case "redirect":
		fmt.Fprintf(&b, "redirect %d %s", r.code, r.url)
	}
	return b.String()
}

// accBuildWorld writes a config with several sites and many routes on two
// ports, compiles it and returns it ready to serve. Everything is http://, so
// no certificates are needed.
func accBuildWorld(t *testing.T, seed int64, logName string) *accWorld {
	t.Helper()
	g := &accGen{rand.New(rand.NewSource(seed))}
	dir := t.TempDir()
	accFolders(t, dir)
	w := &accWorld{dir: dir, conf: filepath.Join(dir, "bareproxy.conf"), logPath: filepath.Join(dir, logName)}
	var live []*httptest.Server
	for i := 0; i < 2; i++ {
		s := accBackend(t)
		live = append(live, s)
		w.liveAddrs = append(w.liveAddrs, strings.TrimPrefix(s.URL, "http://"))
	}
	dead1, dead2 := accDeadAddr(t), accDeadAddr(t)

	w.sites = []*accSite{
		{name: "alpha.test", port: 8080, hosts: []string{"alpha.test"}, err404: true},
		{name: "beta.test", port: 8080, hosts: []string{"beta.test", "www.beta.test"}},
		{name: "*.wild.test", port: 8080, wild: "wild.test"},
		{name: "keep.test", port: 8080, hosts: []string{"keep.test"}, keep: true},
		{name: "other.test", port: 8081, hosts: []string{"other.test"}, err404: true},
		{name: "*", port: 8081, catch: true},
	}
	var b strings.Builder
	line := 0
	emit := func(f string, a ...any) int {
		line++
		fmt.Fprintf(&b, f+"\n", a...)
		return line
	}
	emit("global")
	emit("  admin off")
	emit("  trace-log %s", w.logPath)
	emit("")
	for _, s := range w.sites {
		var addrs []string
		switch {
		case s.catch:
			addrs = []string{fmt.Sprintf("http://*:%d", s.port)}
		case s.wild != "":
			addrs = []string{fmt.Sprintf("http://*.%s:%d", s.wild, s.port)}
		default:
			for _, h := range s.hosts {
				addrs = append(addrs, fmt.Sprintf("http://%s:%d", h, s.port))
			}
		}
		s.line = emit("site %s", strings.Join(addrs, " "))
		if s.err404 {
			emit("  error 404 /404.html")
		}
		if s.keep {
			emit("  encoded-slashes keep")
		}
		n := 12 + g.rng.Intn(14)
		for i := 0; i < n; i++ {
			r := g.route(s, false)
			r.text = r.render()
			r.line = emit("  %s", r.text)
			s.routes = append(s.routes, r)
		}
		if s.name != "*.wild.test" && g.chance(0.75) {
			r := g.route(s, true)
			r.text = r.render()
			r.line = emit("  %s", r.text)
			s.routes = append(s.routes, r)
		}
		emit("")
	}
	emit("pool live")
	emit("  backend %s", w.liveAddrs[0])
	emit("pool two")
	emit("  backend %s", w.liveAddrs[0])
	emit("  backend %s", w.liveAddrs[1])
	emit("pool dead")
	emit("  backend %s", dead1)
	emit("pool mixed")
	emit("  backend %s", dead2)
	emit("  backend %s", w.liveAddrs[1])
	w.src = b.String()
	accWrite(t, w.conf, w.src)
	c, probs := Load(w.conf)
	if c == nil || HasErrors(probs) {
		t.Fatalf("the generated config has errors: %v\n%s", probs, w.src)
	}
	w.cfg = c
	w.lines = c.Lines
	rt, err := NewRuntime(c, nil, 1, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	w.rt = rt
	t.Cleanup(func() { rt.Stop(); rt.Trace.Close(); c.Close() })
	w.srv = NewServer(w.conf, rt)
	w.srv.logger.SetOutput(nopWriter{})
	w.handler = map[int]http.Handler{8080: w.srv.Handler(8080, false), 8081: w.srv.Handler(8081, false)}
	return w
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// accCase is one generated request and what came of it.
type accCase struct {
	port     int
	method   string
	hostname string // as a client wrote it (case, trailing dot)
	host     string // the Host header: with or without the port
	rawPath  string
	query    string
	hasQuery bool
	hdr      http.Header

	// results
	code    int
	loc     string
	id      string
	seen    http.Header // the X-Seen headers a backend added, if one answered
	expl    accExpl
	explTxt string
}

func (c *accCase) target() string {
	if c.hasQuery {
		return c.rawPath + "?" + c.query
	}
	return c.rawPath
}

func (c *accCase) explainURL() string {
	return fmt.Sprintf("http://%s:%d%s", c.hostname, c.port, c.target())
}

func (g *accGen) segment() string {
	r := g.rng.Float64()
	switch {
	case r < 0.62:
		return g.pick(accWords)
	case r < 0.78:
		return fmt.Sprintf("zz%d", g.rng.Intn(50))
	case r < 0.93:
		return g.pick(accFileNames)
	}
	return g.pick(accOddSegments)
}

// noisy percent-encodes some characters of a segment that has no escape yet.
func (g *accGen) noisy(s string) string {
	if strings.Contains(s, "%") || !g.chance(0.14) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if g.chance(0.35) {
			if g.chance(0.5) {
				fmt.Fprintf(&b, "%%%02x", s[i])
			} else {
				fmt.Fprintf(&b, "%%%02X", s[i])
			}
		} else {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func (g *accGen) path() string {
	n := g.rng.Intn(5)
	var b strings.Builder
	for i := 0; i < n; i++ {
		switch r := g.rng.Float64(); {
		case r < 0.84:
			b.WriteString("/")
		case r < 0.91:
			b.WriteString("//")
		case r < 0.93:
			b.WriteString("///")
		case r < 0.96:
			b.WriteString("/./")
		case r < 0.98:
			b.WriteString("/x/../")
		default:
			b.WriteString("/%2e/")
		}
		if g.chance(0.04) {
			b.WriteString(g.pick(accEvilSegments))
		} else {
			b.WriteString(g.noisy(g.segment()))
		}
	}
	switch r := g.rng.Float64(); {
	case r < 0.30:
		b.WriteString("/")
	case r < 0.33:
		b.WriteString("//")
	case r < 0.36:
		b.WriteString("/.")
	case r < 0.38:
		b.WriteString("/..")
	case r < 0.40:
		b.WriteString("/%2e")
	case r < 0.41:
		b.WriteString("/x/..")
	}
	if b.Len() == 0 {
		return "/"
	}
	return b.String()
}

// next makes a request. Hosts, ports, methods and headers vary; the path is
// built from the route vocabulary plus children, dot segments, escapes and
// extra slashes.
func (g *accGen) next() *accCase {
	c := &accCase{}
	if g.chance(0.72) {
		c.port = 8080
		c.hostname = g.pick([]string{"alpha.test", "alpha.test", "alpha.test", "beta.test", "beta.test", "www.beta.test",
			"a.wild.test", "b.wild.test", "x.y.wild.test", "wild.test", "keep.test", "keep.test", "unknown.test",
			"ALPHA.TEST", "Beta.Test.", "alpha.test."})
	} else {
		c.port = 8081
		c.hostname = g.pick([]string{"other.test", "other.test", "OTHER.test", "any.test", "random.example", "alpha.test", "other.test."})
	}
	c.host = c.hostname
	if g.chance(0.7) {
		c.host = fmt.Sprintf("%s:%d", c.hostname, c.port)
	}
	c.method = g.pick(accRequestMethods)
	c.rawPath = g.path()
	if g.chance(0.35) {
		c.hasQuery, c.query = true, g.pick(accQueries)
	}
	c.hdr = http.Header{}
	for _, h := range accHeaderValues {
		if g.chance(0.35) {
			c.hdr.Add(h.name, g.pick(h.values))
			if g.chance(0.15) {
				c.hdr.Add(h.name, g.pick(h.values))
			}
		}
	}
	return c
}

// ---- reference implementation of the design's rules ----

// accRefNormalize is the path rule of section 10 written apart from
// NormalizePath: escapes of unreserved characters are decoded, other escapes
// are kept in capitals, dot segments are resolved, runs of slashes are
// merged, and %2F, %5C, raw backslashes, control characters and a climb above
// the root are refused (%2F and %5C are kept when the site says keep).
func accRefNormalize(raw string, keep bool) (string, bool) {
	if !strings.HasPrefix(raw, "/") {
		return "", false
	}
	unreserved := func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~", c) >= 0
	}
	pchar := func(c byte) bool { return unreserved(c) || strings.IndexByte("!$&'()*+,;=:@", c) >= 0 }
	var out []string
	parts := strings.Split(raw[1:], "/")
	trailing := false
	for i, p := range parts {
		var seg strings.Builder
		for j := 0; j < len(p); j++ {
			c := p[j]
			switch {
			case c == '%':
				if j+2 >= len(p)+0 && j+2 > len(p)-1 {
					return "", false
				}
				v, err := strconv.ParseUint(p[j+1:j+3], 16, 8)
				if err != nil || strings.ContainsAny(p[j+1:j+3], "+-") {
					return "", false
				}
				b := byte(v)
				j += 2
				switch {
				case unreserved(b):
					seg.WriteByte(b)
				case b == '/' || b == '\\':
					if !keep {
						return "", false
					}
					fmt.Fprintf(&seg, "%%%02X", b)
				case b < 0x20 || b == 0x7f:
					return "", false
				default:
					fmt.Fprintf(&seg, "%%%02X", b)
				}
			case c == '\\' || c < 0x20 || c == 0x7f:
				return "", false
			case pchar(c):
				seg.WriteByte(c)
			default:
				fmt.Fprintf(&seg, "%%%02X", c)
			}
		}
		last := i == len(parts)-1
		switch s := seg.String(); s {
		case "", ".":
			trailing = trailing || last
		case "..":
			if len(out) == 0 {
				return "", false
			}
			out = out[:len(out)-1]
			trailing = trailing || last
		default:
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return "/", true
	}
	n := "/" + strings.Join(out, "/")
	if trailing {
		n += "/"
	}
	return n, true
}

func (w *accWorld) siteFor(port int, host string) *accSite {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	var wild, catch *accSite
	for _, s := range w.sites {
		if s.port != port {
			continue
		}
		for _, h := range s.hosts {
			if h == host {
				return s
			}
		}
		if s.wild != "" {
			if i := strings.IndexByte(host, '.'); i > 0 && host[i+1:] == s.wild {
				wild = s
			}
		}
		if s.catch {
			catch = s
		}
	}
	if wild != nil {
		return wild
	}
	return catch
}

func (r *accRoute) matches(method, path string, h http.Header) bool {
	switch {
	case r.prefix && r.path == "":
	case r.prefix:
		if path != r.path && !strings.HasPrefix(path, r.path+"/") {
			return false
		}
	case path != r.path:
		return false
	}
	if len(r.methods) > 0 {
		ok := false
		for _, m := range r.methods {
			if m == method || m == "GET" && method == "HEAD" {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	for _, c := range r.conds {
		vals := h.Values(c.name)
		if len(vals) == 0 {
			return false
		}
		if c.hasValue {
			found := false
			for _, v := range vals {
				found = found || v == c.value
			}
			if !found {
				return false
			}
		}
	}
	return true
}

// accWant is what the design says should happen to a request.
type accWant struct {
	site     *accSite
	norm     string
	bad      bool // 400
	route    *accRoute
	status   int // 0: not known from the rules alone
	loc      string
	upstream string
}

func (w *accWorld) oracle(c *accCase) accWant {
	var o accWant
	o.site = w.siteFor(c.port, c.hostname)
	if o.site == nil {
		o.status = 421
		return o
	}
	norm, ok := accRefNormalize(c.rawPath, o.site.keep)
	if !ok {
		o.bad, o.status = true, 400
		return o
	}
	o.norm = norm
	for _, r := range o.site.routes {
		if r.matches(c.method, norm, c.hdr) {
			o.route = r
			break
		}
	}
	if o.route == nil {
		o.status = 404
		return o
	}
	switch r := o.route; r.kind {
	case "respond":
		o.status = r.code
	case "redirect":
		o.status, o.loc = r.code, r.url
		if r.bare {
			o.loc = r.url + norm
			if c.query != "" {
				o.loc += "?" + c.query
			}
		}
	case "files":
		if c.method != "GET" && c.method != "HEAD" {
			o.status = 405
		}
	case "pool":
		o.upstream = norm
		if r.strip && r.path != "" {
			o.upstream = strings.TrimPrefix(norm, r.path)
			if o.upstream == "" {
				o.upstream = "/"
			}
		}
		if r.pool == "live" || r.pool == "two" || r.pool == "mixed" {
			o.status = 200
		}
	}
	return o
}

// ---- reading what explain says ----

type accExpl struct {
	site     string
	siteLine int
	ruleLine int // 0 when no rule matches
	action   string
	norm     string // the normalized path explain shows
	upstream string
	upMethod string
	upHost   string
	folder   bool // a folder redirect
	refused  bool
	file     string // the File: line, when explain finds one
	checked  string // the Checked: line, when explain looked for one
}

var (
	accSiteRe     = regexp.MustCompile(`^Site (\S+) \(line (\d+)\)`)
	accRuleRe     = regexp.MustCompile(`^  line (\d+)\s`)
	accUpstreamRe = regexp.MustCompile(`^Sent upstream as (\S+) (\S+) with Host: (.*)$`)
)

func accParseExplain(out string) accExpl {
	var e accExpl
	for _, ln := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(ln, "No site for "):
			e.site = ""
		case accSiteRe.MatchString(ln):
			m := accSiteRe.FindStringSubmatch(ln)
			e.site = m[1]
			e.siteLine, _ = strconv.Atoi(m[2])
		case accRuleRe.MatchString(ln):
			m := accRuleRe.FindStringSubmatch(ln)
			if strings.HasSuffix(ln, "   match") {
				e.ruleLine, _ = strconv.Atoi(m[1])
			}
		case strings.HasPrefix(ln, "Action: "):
			e.action = strings.TrimPrefix(ln, "Action: ")
		case strings.HasPrefix(ln, "Normalized path: "):
			e.norm = strings.TrimPrefix(ln, "Normalized path: ")
		case strings.HasPrefix(ln, "Path: "):
			e.norm = strings.TrimPrefix(ln, "Path: ")
		case strings.HasPrefix(ln, "Path ") && strings.Contains(ln, " refused: "):
			e.refused = true
		case strings.HasPrefix(ln, "It's a folder"):
			e.folder = true
		case strings.HasPrefix(ln, "File: "):
			e.file = strings.TrimPrefix(ln, "File: ")
		case strings.HasPrefix(ln, "Checked: "):
			e.checked = strings.TrimPrefix(ln, "Checked: ")
		}
		if m := accUpstreamRe.FindStringSubmatch(ln); m != nil {
			e.upMethod, e.upstream, e.upHost = m[1], m[2], m[3]
		}
	}
	return e
}

// accActionOutcome turns the Action line of explain into the status (and
// Location) the server should send. ok is false when explain states no action.
func accActionOutcome(a string) (status int, loc string, ok bool) {
	switch {
	case a == "":
		return 0, "", false
	case strings.HasPrefix(a, "421 "):
		return 421, "", true
	case strings.HasPrefix(a, "400 "):
		return 400, "", true
	case strings.HasPrefix(a, "404"):
		return 404, "", true
	case strings.HasPrefix(a, "503 "):
		return 503, "", true
	case strings.HasPrefix(a, "405,"):
		return 405, "", true
	case a == "serve 200":
		return 200, "", true
	case strings.HasPrefix(a, "BareProxy answers "):
		var n int
		fmt.Sscanf(a, "BareProxy answers %d itself", &n)
		return n, "", true
	case strings.HasPrefix(a, "redirect "):
		var n int
		fmt.Sscanf(a, "redirect %d to", &n)
		_, loc, _ = strings.Cut(a, " to ")
		return n, loc, true
	}
	return 0, "", false
}

func accReadLog(t *testing.T, path string) []Record {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("a trace log line is not a record: %q: %v", sc.Text(), err)
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
