// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

// Package bp is BareProxy's first cut: config, routing, static files,
// proxying, request records and explain.
package bp

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Problem is an error or warning found in a config, tied to its line.
type Problem struct {
	Line int
	Msg  string
	Warn bool
}

func (p Problem) String() string {
	kind := "error"
	if p.Warn {
		kind = "warning"
	}
	if p.Line == 0 {
		return kind + ": " + p.Msg
	}
	return fmt.Sprintf("line %d: %s: %s", p.Line, kind, p.Msg)
}

// HasErrors reports whether any problem is an error and not just a warning.
func HasErrors(ps []Problem) bool {
	for _, p := range ps {
		if !p.Warn {
			return true
		}
	}
	return false
}

// Config is a compiled config file.
type Config struct {
	File       string
	Lines      []string
	Admin      string
	TraceLog   string
	TraceSize  int64 // rotate the trace log file past this many bytes; 0 means never
	TraceCount int   // old trace log files kept
	TraceMem   int64 // bytes of record JSON kept in memory
	TraceQuery bool
	IDHeader   bool
	Sites      []*Site
	Pools      map[string]*PoolSpec
	PoolOrder  []string
	Ports      map[int]*Port
	roots      []*os.Root
}

// Port is one listener and the sites it serves.
type Port struct {
	Num   int
	TLS   bool
	Line  int
	Exact map[string]*Site
	Wild  map[string]*Site
	Any   *Site
}

// Site is one site block.
type Site struct {
	Line             int
	Name             string
	Addrs            []Addr
	Routes           []*Route
	CertFile         string
	KeyFile          string
	TLSLine          int
	TLSAuto          bool
	Cert             *tls.Certificate
	Err404           string
	Err404Line       int
	KeepEncodedSlash bool
	BodyLimit        int64
	Synthetic        bool
}

// Addr is one site address.
type Addr struct {
	Scheme string
	Host   string
	Port   int
	Text   string
}

// Route is one route line.
type Route struct {
	Line    int
	Text    string
	Methods []string
	Path    string // the prefix for "/api/*" is "/api"; "/*" has ""
	Prefix  bool
	Headers []HeaderCond
	Act     Action
}

// HeaderCond is a header condition on a route.
type HeaderCond struct {
	Name     string
	Value    string
	HasValue bool
}

// Action is what a route does.
type Action struct {
	Kind   string // pool, files, redirect, respond, https
	Pool   string
	Strip  bool
	Dir    string
	Root   *os.Root
	Code   int
	URL    string
	Status int
	Body   string
}

// PoolSpec is one pool block.
type PoolSpec struct {
	Line            int
	Name            string
	Backends        []BackendSpec
	Health          *HealthSpec
	HostHeader      string
	ConnectTimeout  time.Duration
	ResponseTimeout time.Duration
	Retries         int
	used            bool
}

// BackendSpec is one backend line.
type BackendSpec struct {
	Line  int
	Addr  string
	HTTPS bool
}

// HealthSpec is a pool's active health check.
type HealthSpec struct {
	Line    int
	Path    string
	Every   time.Duration
	Timeout time.Duration
	Lo, Hi  int
}

func (h *HealthSpec) key() string {
	if h == nil {
		return "-"
	}
	return fmt.Sprintf("%s|%s|%s|%d-%d", h.Path, h.Every, h.Timeout, h.Lo, h.Hi)
}

// Close releases the folders a config holds open.
func (c *Config) Close() {
	for _, r := range c.roots {
		r.Close()
	}
	c.roots = nil
}

// Load reads and compiles a config file.
func Load(file string) (*Config, []Problem) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, []Problem{{Msg: err.Error()}}
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, []Problem{{Msg: err.Error()}}
	}
	return Parse(abs, string(data))
}

type token struct {
	s      string
	quoted bool
}

func splitLine(s string) ([]token, error) {
	var out []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			i++
			continue
		case c == '#':
			return out, nil
		case c == '"':
			var b strings.Builder
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				b.WriteByte(s[j])
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("a quoted text has no closing quote")
			}
			out = append(out, token{b.String(), true})
			i = j + 1
			continue
		}
		j := i
		for j < len(s) && s[j] != ' ' && s[j] != '\t' && s[j] != '\r' {
			j++
		}
		out = append(out, token{s[i:j], false})
		i = j
	}
	return out, nil
}

func joinTokens(toks []token) string {
	parts := make([]string, len(toks))
	for i, t := range toks {
		if t.quoted {
			parts[i] = strconv.Quote(t.s)
		} else {
			parts[i] = t.s
		}
	}
	return strings.Join(parts, " ")
}

type parser struct {
	c     *Config
	dir   string
	probs []Problem
}

func (p *parser) errf(line int, f string, a ...any) {
	p.probs = append(p.probs, Problem{Line: line, Msg: fmt.Sprintf(f, a...)})
}

func (p *parser) warnf(line int, f string, a ...any) {
	p.probs = append(p.probs, Problem{Line: line, Msg: fmt.Sprintf(f, a...), Warn: true})
}

// Parse compiles config source. It returns the config even when there are
// problems; callers check HasErrors before using it.
func Parse(file, src string) (*Config, []Problem) {
	c := &Config{
		File:     file,
		Admin:    "/run/bareproxy/admin.sock",
		TraceLog: "stdout",
		TraceMem: 32 << 20,
		IDHeader: true,
		Pools:    map[string]*PoolSpec{},
		Ports:    map[int]*Port{},
	}
	p := &parser{c: c, dir: filepath.Dir(file)}
	src = strings.ReplaceAll(src, "\r\n", "\n")
	c.Lines = strings.Split(src, "\n")
	block := ""
	var site *Site
	var pool *PoolSpec
	for i, raw := range c.Lines {
		ln := i + 1
		toks, err := splitLine(raw)
		if err != nil {
			p.errf(ln, "%v", err)
			continue
		}
		if len(toks) == 0 {
			continue
		}
		w := make([]string, len(toks))
		for k, t := range toks {
			w[k] = t.s
		}
		if raw[0] != ' ' && raw[0] != '\t' {
			site, pool, block = nil, nil, ""
			switch w[0] {
			case "global":
				block = "global"
				if len(w) > 1 {
					p.errf(ln, "global takes nothing after it")
				}
			case "site":
				block = "site"
				site = &Site{Line: ln, BodyLimit: 10 << 20}
				if len(w) < 2 {
					p.errf(ln, "site needs at least one address")
				}
				for _, a := range w[1:] {
					ad, err := parseAddr(a)
					if err != nil {
						p.errf(ln, "%v", err)
						continue
					}
					site.Addrs = append(site.Addrs, ad)
				}
				if len(site.Addrs) > 0 {
					site.Name = site.Addrs[0].Host
				}
				c.Sites = append(c.Sites, site)
			case "pool":
				block = "pool"
				pool = &PoolSpec{Line: ln, ConnectTimeout: 3 * time.Second, ResponseTimeout: 60 * time.Second, Retries: 1}
				if len(w) != 2 || !validName(w[1]) {
					p.errf(ln, "pool needs one name made of letters, digits, - and _ (and not files, redirect, respond or strip)")
					continue
				}
				pool.Name = w[1]
				if old, dup := c.Pools[pool.Name]; dup {
					p.errf(ln, "pool %s is already defined on line %d", pool.Name, old.Line)
					continue
				}
				c.Pools[pool.Name] = pool
				c.PoolOrder = append(c.PoolOrder, pool.Name)
			default:
				p.errf(ln, "%q can't start a block: a line in the first column starts global, site or pool", w[0])
			}
			continue
		}
		switch block {
		case "global":
			p.global(ln, w)
		case "site":
			p.siteSetting(site, ln, toks, w)
		case "pool":
			p.poolSetting(pool, ln, w)
		default:
			p.errf(ln, "indented line outside a global, site or pool block")
		}
	}
	p.compile()
	sort.SliceStable(p.probs, func(i, j int) bool { return p.probs[i].Line < p.probs[j].Line })
	return c, p.probs
}

func (p *parser) path(s string) string {
	if filepath.IsAbs(s) {
		return filepath.Clean(s)
	}
	return filepath.Join(p.dir, s)
}

func (p *parser) onOff(ln int, s string) bool {
	switch s {
	case "on":
		return true
	case "off":
		return false
	}
	p.errf(ln, "expected on or off, got %q", s)
	return false
}

func (p *parser) global(ln int, w []string) {
	c := p.c
	one := func() bool {
		if len(w) != 2 {
			p.errf(ln, "%s takes one value", w[0])
			return false
		}
		return true
	}
	switch w[0] {
	case "admin":
		if one() {
			if w[1] == "off" {
				c.Admin = "off"
			} else {
				c.Admin = p.path(w[1])
			}
		}
	case "trace-log":
		if len(w) != 2 && len(w) != 4 {
			p.errf(ln, "trace-log takes stdout, off, or a file with an optional size and count")
			return
		}
		switch w[1] {
		case "stdout", "off":
			c.TraceLog = w[1]
		default:
			c.TraceLog = p.path(w[1])
		}
		c.TraceSize, c.TraceCount = 0, 0
		if len(w) == 4 {
			size, err := parseSize(w[2])
			count, err2 := strconv.Atoi(w[3])
			switch {
			case c.TraceLog == "stdout" || c.TraceLog == "off":
				p.errf(ln, "only a trace log file can rotate, not %s", w[1])
			case err != nil || size == 0:
				p.errf(ln, "the size to rotate at must be more than zero, such as 10MB")
			case err2 != nil || count < 1:
				p.errf(ln, "the number of old files to keep must be 1 or more")
			default:
				c.TraceSize, c.TraceCount = size, count
			}
		}
	case "trace-memory":
		if !one() {
			return
		}
		if w[1] == "off" {
			c.TraceMem = 0
		} else if n, err := parseSize(w[1]); err != nil {
			p.errf(ln, "%v", err)
		} else {
			c.TraceMem = n
		}
	case "trace-query":
		if one() {
			c.TraceQuery = p.onOff(ln, w[1])
		}
	case "id-header":
		if one() {
			c.IDHeader = p.onOff(ln, w[1])
		}
	case "state", "acme-email", "acme-ca", "trust",
		"client-header-timeout", "client-body-timeout", "client-idle-timeout", "shutdown-timeout":
		p.warnf(ln, "%s isn't built yet in this version, so it's ignored", w[0])
	default:
		p.errf(ln, "unknown global setting %q", w[0])
	}
}

func (p *parser) siteSetting(s *Site, ln int, toks []token, w []string) {
	switch w[0] {
	case "route":
		if r := p.route(ln, toks); r != nil {
			s.Routes = append(s.Routes, r)
		}
	case "tls":
		switch {
		case len(w) == 2 && w[1] == "auto":
			s.TLSAuto, s.TLSLine = true, ln
		case len(w) == 3:
			s.CertFile, s.KeyFile, s.TLSLine = p.path(w[1]), p.path(w[2]), ln
		default:
			p.errf(ln, "tls takes auto, or a certificate file and a key file")
		}
	case "error":
		if len(w) != 3 {
			p.errf(ln, "error takes a status and a path, such as error 404 /404.html")
			return
		}
		if w[1] != "404" {
			p.errf(ln, "only error 404 is supported")
			return
		}
		if !isNormalPath(w[2]) || strings.HasSuffix(w[2], "/") {
			p.errf(ln, "the error page must be a plain file path such as /404.html")
			return
		}
		s.Err404, s.Err404Line = w[2], ln
	case "encoded-slashes":
		if len(w) == 2 && (w[1] == "reject" || w[1] == "keep") {
			s.KeepEncodedSlash = w[1] == "keep"
		} else {
			p.errf(ln, "encoded-slashes takes reject or keep")
		}
	case "body-limit":
		if len(w) != 2 {
			p.errf(ln, "body-limit takes one size, such as 10MB")
			return
		}
		n, err := parseSize(w[1])
		if err != nil {
			p.errf(ln, "%v", err)
			return
		}
		s.BodyLimit = n
	case "set-response-header", "remove-response-header":
		p.warnf(ln, "%s isn't built yet in this version, so it's ignored", w[0])
	default:
		p.errf(ln, "unknown site setting %q", w[0])
	}
}

func (p *parser) route(ln int, toks []token) *Route {
	r := &Route{Line: ln, Text: joinTokens(toks)}
	arrow := -1
	for i, t := range toks {
		if t.s == "->" && !t.quoted {
			arrow = i
			break
		}
	}
	if arrow < 0 {
		p.errf(ln, "route needs -> before its action")
		return nil
	}
	left, right := toks[1:arrow], toks[arrow+1:]
	if len(left) == 0 {
		p.errf(ln, "route needs a path, such as /api/*")
		return nil
	}
	i := 0
	if !strings.HasPrefix(left[0].s, "/") {
		for _, m := range strings.Split(left[0].s, ",") {
			if !validMethod(m) {
				p.errf(ln, "%q isn't a method; write methods in capitals, such as GET,HEAD", m)
				return nil
			}
			r.Methods = append(r.Methods, m)
		}
		i = 1
	}
	if i >= len(left) {
		p.errf(ln, "route needs a path after the methods")
		return nil
	}
	ps := left[i].s
	i++
	switch {
	case ps == "/*":
		r.Prefix = true
	case strings.HasSuffix(ps, "/*"):
		r.Prefix, r.Path = true, strings.TrimSuffix(ps, "/*")
	default:
		r.Path = ps
	}
	check := r.Path
	if check == "" {
		check = "/"
	}
	if strings.Contains(check, "*") || !isNormalPath(check) || (r.Prefix && strings.HasSuffix(r.Path, "/")) {
		p.errf(ln, "path %q isn't in normal form: no * except a final /*, no //, no . or .. parts, and escapes only where needed", ps)
		return nil
	}
	for i < len(left) {
		if left[i].s != "header" || i+1 >= len(left) {
			p.errf(ln, "unexpected %q: after the path a route takes only header NAME or header NAME=VALUE", left[i].s)
			return nil
		}
		name, val, has := strings.Cut(left[i+1].s, "=")
		if !validHeaderName(name) {
			p.errf(ln, "%q isn't a header name", name)
			return nil
		}
		r.Headers = append(r.Headers, HeaderCond{Name: http.CanonicalHeaderKey(name), Value: val, HasValue: has})
		i += 2
	}
	if len(right) == 0 {
		p.errf(ln, "route needs an action after ->")
		return nil
	}
	a := &r.Act
	switch right[0].s {
	case "files":
		if len(right) != 2 {
			p.errf(ln, "files takes one folder")
			return nil
		}
		a.Kind, a.Dir = "files", p.path(right[1].s)
	case "redirect":
		if len(right) != 3 {
			p.errf(ln, "redirect takes a code and a URL")
			return nil
		}
		code, _ := strconv.Atoi(right[1].s)
		if code != 301 && code != 302 && code != 307 && code != 308 {
			p.errf(ln, "the redirect code must be 301, 302, 307 or 308")
			return nil
		}
		u, err := url.Parse(right[2].s)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			p.errf(ln, "redirect needs a full http:// or https:// URL")
			return nil
		}
		a.Kind, a.Code, a.URL = "redirect", code, right[2].s
	case "respond":
		if len(right) < 2 || len(right) > 3 {
			p.errf(ln, "respond takes a status and an optional text")
			return nil
		}
		st, err := strconv.Atoi(right[1].s)
		if err != nil || st < 200 || st > 599 {
			p.errf(ln, "respond needs a status from 200 to 599")
			return nil
		}
		a.Kind, a.Status = "respond", st
		if len(right) == 3 {
			a.Body = right[2].s
		}
	default:
		if !validName(right[0].s) {
			p.errf(ln, "%q isn't an action: use a pool name, files, redirect or respond", right[0].s)
			return nil
		}
		a.Kind, a.Pool = "pool", right[0].s
		switch {
		case len(right) == 2 && right[1].s == "strip":
			a.Strip = true
			if r.Path == "" {
				p.warnf(ln, "strip does nothing on /*")
			}
		case len(right) != 1:
			p.errf(ln, "after a pool name a route takes only strip")
			return nil
		}
	}
	return r
}

func (p *parser) poolSetting(ps *PoolSpec, ln int, w []string) {
	dur := func() (time.Duration, bool) {
		if len(w) != 2 {
			p.errf(ln, "%s takes one duration, such as 5s", w[0])
			return 0, false
		}
		d, err := time.ParseDuration(w[1])
		if err != nil || d <= 0 {
			p.errf(ln, "bad duration %q: use a number with ms, s or m", w[1])
			return 0, false
		}
		return d, true
	}
	switch w[0] {
	case "backend":
		if len(w) != 2 {
			p.errf(ln, "backend takes one address, such as 10.0.0.11:8080")
			return
		}
		a, https := w[1], false
		switch {
		case strings.HasPrefix(a, "unix:"):
			p.errf(ln, "unix: backends aren't built yet in this version")
			return
		case strings.HasPrefix(a, "https://"):
			a, https = a[len("https://"):], true
		case strings.HasPrefix(a, "http://"):
			a = a[len("http://"):]
		}
		host, port, err := net.SplitHostPort(a)
		if err != nil || host == "" {
			p.errf(ln, "a backend address is host:port, such as 10.0.0.11:8080")
			return
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			p.errf(ln, "bad backend port %q", port)
			return
		}
		for _, b := range ps.Backends {
			if b.Addr == a && b.HTTPS == https {
				p.errf(ln, "backend %s is already listed on line %d", a, b.Line)
				return
			}
		}
		ps.Backends = append(ps.Backends, BackendSpec{Line: ln, Addr: a, HTTPS: https})
	case "health":
		if len(w) < 2 || !strings.HasPrefix(w[1], "/") {
			p.errf(ln, "health takes a path, such as /healthz")
			return
		}
		h := &HealthSpec{Line: ln, Path: w[1], Every: 5 * time.Second, Timeout: 2 * time.Second, Lo: 200, Hi: 399}
		for i := 2; i < len(w); i += 2 {
			if i+1 >= len(w) {
				p.errf(ln, "%s needs a value", w[i])
				return
			}
			switch w[i] {
			case "every", "timeout":
				d, err := time.ParseDuration(w[i+1])
				if err != nil || d <= 0 {
					p.errf(ln, "bad duration %q", w[i+1])
					return
				}
				if w[i] == "every" {
					h.Every = d
				} else {
					h.Timeout = d
				}
			case "expect":
				lo, hi, ok := parseRange(w[i+1])
				if !ok {
					p.errf(ln, "expect takes a status range such as 200-399")
					return
				}
				h.Lo, h.Hi = lo, hi
			default:
				p.errf(ln, "unknown health option %q: use every, timeout or expect", w[i])
				return
			}
		}
		ps.Health = h
	case "host-header":
		if len(w) != 2 {
			p.errf(ln, "host-header takes one value")
			return
		}
		ps.HostHeader = w[1]
	case "connect-timeout":
		if d, ok := dur(); ok {
			ps.ConnectTimeout = d
		}
	case "response-timeout":
		if d, ok := dur(); ok {
			ps.ResponseTimeout = d
		}
	case "retries":
		if len(w) != 2 || (w[1] != "0" && w[1] != "1" && w[1] != "2") {
			p.errf(ln, "retries takes 0, 1 or 2")
			return
		}
		ps.Retries = int(w[1][0] - '0')
	case "drain":
		p.warnf(ln, "drain isn't built yet in this version, so it's ignored")
	default:
		p.errf(ln, "unknown pool setting %q", w[0])
	}
}

// compile resolves pools, opens folders and certificates and builds the
// port tables. It runs after the whole file is read.
func (p *parser) compile() {
	c := p.c
	roots := map[string]*os.Root{}
	for _, s := range c.Sites {
		if len(s.Routes) == 0 {
			p.warnf(s.Line, "site %s has no routes, so every request gets 404", s.Name)
		}
		for _, r := range s.Routes {
			switch r.Act.Kind {
			case "pool":
				ps := c.Pools[r.Act.Pool]
				if ps == nil {
					p.errf(r.Line, "no pool named %s", r.Act.Pool)
					continue
				}
				ps.used = true
			case "files":
				root := roots[r.Act.Dir]
				if root == nil {
					var err error
					root, err = os.OpenRoot(r.Act.Dir)
					if err != nil {
						p.errf(r.Line, "can't open folder %s: %v", r.Act.Dir, unwrapPathErr(err))
						continue
					}
					roots[r.Act.Dir] = root
					c.roots = append(c.roots, root)
				}
				r.Act.Root = root
			}
		}
		https := false
		for _, a := range s.Addrs {
			if a.Scheme == "https" {
				https = true
			}
		}
		switch {
		case https && (s.TLSAuto || s.CertFile == ""):
			p.errf(s.Line, "automatic certificates aren't built yet in this version: add tls CERT KEY, or use http:// addresses")
		case https:
			cert, err := tls.LoadX509KeyPair(s.CertFile, s.KeyFile)
			if err != nil {
				p.errf(s.TLSLine, "can't load the certificate: %v", err)
			} else {
				s.Cert = &cert
			}
		case s.TLSLine != 0:
			p.warnf(s.TLSLine, "tls is unused: the site has no https address")
		}
		for _, a := range s.Addrs {
			port := c.ensurePort(a.Port, a.Scheme == "https", s.Line)
			if port.TLS != (a.Scheme == "https") {
				p.errf(s.Line, "port %d can't be both http and https (see line %d)", a.Port, port.Line)
				continue
			}
			if other := port.claim(a.Host, s); other != nil {
				p.errf(s.Line, "%s on port %d already belongs to the site on line %d", a.Host, a.Port, other.Line)
			}
		}
	}
	// HTTPS sites on port 443 get plain HTTP on port 80 redirected to them,
	// unless an http:// site claims the same host there.
	for _, s := range c.Sites {
		var hosts []string
		for _, a := range s.Addrs {
			if a.Scheme == "https" && a.Port == 443 {
				hosts = append(hosts, a.Host)
			}
		}
		if len(hosts) == 0 {
			continue
		}
		port := c.ensurePort(80, false, s.Line)
		if port.TLS {
			continue
		}
		red := &Site{Line: s.Line, Name: s.Name, Synthetic: true, BodyLimit: s.BodyLimit,
			Routes: []*Route{{Line: s.Line, Text: "plain HTTP goes to https://", Prefix: true, Act: Action{Kind: "https", Code: 301}}}}
		for _, h := range hosts {
			port.claim(h, red)
		}
	}
	for _, name := range c.PoolOrder {
		ps := c.Pools[name]
		if len(ps.Backends) == 0 {
			p.errf(ps.Line, "pool %s has no backends", name)
		}
		if !ps.used {
			p.warnf(ps.Line, "pool %s isn't used by any route", name)
		}
	}
	if len(c.Sites) == 0 {
		p.errf(0, "the config has no sites")
	}
	if HasErrors(p.probs) {
		c.Close()
	}
}

func (c *Config) ensurePort(n int, isTLS bool, line int) *Port {
	if p := c.Ports[n]; p != nil {
		return p
	}
	p := &Port{Num: n, TLS: isTLS, Line: line, Exact: map[string]*Site{}, Wild: map[string]*Site{}}
	c.Ports[n] = p
	return p
}

// claim gives a host on this port to a site. It returns the site that
// already holds the host, if another one does.
func (p *Port) claim(host string, s *Site) *Site {
	switch {
	case host == "*":
		if p.Any != nil && p.Any != s {
			return p.Any
		}
		p.Any = s
	case strings.HasPrefix(host, "*."):
		k := host[2:]
		if o := p.Wild[k]; o != nil && o != s {
			return o
		}
		p.Wild[k] = s
	default:
		if o := p.Exact[host]; o != nil && o != s {
			return o
		}
		p.Exact[host] = s
	}
	return nil
}

// Find returns the site for a host on this port, and how it matched.
func (p *Port) Find(host string) (*Site, string) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if s := p.Exact[host]; s != nil {
		return s, "exact host match"
	}
	if i := strings.IndexByte(host, '.'); i > 0 {
		if s := p.Wild[host[i+1:]]; s != nil {
			return s, "matches *." + host[i+1:]
		}
	}
	if p.Any != nil {
		return p.Any, "the catch-all site *"
	}
	return nil, ""
}

func parseAddr(s string) (Addr, error) {
	a := Addr{Text: s, Scheme: "https"}
	rest := s
	switch {
	case strings.HasPrefix(rest, "http://"):
		a.Scheme, rest = "http", rest[len("http://"):]
	case strings.HasPrefix(rest, "https://"):
		rest = rest[len("https://"):]
	}
	host, port := rest, ""
	if i := strings.LastIndexByte(rest, ':'); i >= 0 {
		host, port = rest[:i], rest[i+1:]
	}
	if port == "" {
		a.Port = 443
		if a.Scheme == "http" {
			a.Port = 80
		}
	} else {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return a, fmt.Errorf("bad port in address %q", s)
		}
		a.Port = n
	}
	host = strings.ToLower(host)
	if host != "*" && !validHost(strings.TrimPrefix(host, "*.")) {
		return a, fmt.Errorf("bad host name in address %q", s)
	}
	a.Host = host
	return a, nil
}

func validHost(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

var reservedNames = map[string]bool{"files": true, "redirect": true, "respond": true, "strip": true}

func validName(s string) bool {
	if s == "" || reservedNames[s] {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func validMethod(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}

func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func parseSize(s string) (int64, error) {
	u := strings.ToUpper(s)
	mult := int64(1)
	for _, x := range []struct {
		suf string
		m   int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(u, x.suf) {
			u, mult = strings.TrimSuffix(u, x.suf), x.m
			break
		}
	}
	n, err := strconv.ParseInt(u, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad size %q: use a number with B, KB, MB or GB", s)
	}
	return n * mult, nil
}

func parseRange(s string) (int, int, bool) {
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, false
	}
	lo, e1 := strconv.Atoi(a)
	hi, e2 := strconv.Atoi(b)
	if e1 != nil || e2 != nil || lo < 100 || hi > 599 || lo > hi {
		return 0, 0, false
	}
	return lo, hi, true
}

// Summary describes a config in one line.
func Summary(c *Config) string {
	rules := 0
	for _, s := range c.Sites {
		rules += len(s.Routes)
	}
	var ls []string
	for _, p := range sortedPorts(c) {
		kind := "http"
		if p.TLS {
			kind = "https"
		}
		ls = append(ls, fmt.Sprintf(":%d (%s)", p.Num, kind))
	}
	return fmt.Sprintf("%s, %s, %s; listening on %s",
		plural(len(c.Sites), "site"), plural(len(c.Pools), "pool"), plural(rules, "rule"), strings.Join(ls, ", "))
}

func plural(n int, w string) string {
	if n == 1 {
		return "1 " + w
	}
	return fmt.Sprintf("%d %ss", n, w)
}

func sortedPorts(c *Config) []*Port {
	var ps []*Port
	for _, p := range c.Ports {
		ps = append(ps, p)
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Num < ps[j].Num })
	return ps
}
