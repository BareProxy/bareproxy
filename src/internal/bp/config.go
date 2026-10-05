// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

// Package bp is BareProxy's first cut: config, routing, static files,
// proxying, request records and explain.
package bp

import (
	"cmp"
	"crypto/tls"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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
	return slices.ContainsFunc(ps, func(p Problem) bool { return !p.Warn })
}

// Config is a compiled config file.
type Config struct {
	File       string
	Lines      []string
	Admin      string
	State      string // state dir; empty means /var/lib/bareproxy
	TraceLog   string
	TraceSize  int64 // rotate the trace log file past this many bytes; 0 means never
	TraceCount int   // old trace log files kept
	TraceMem   int64 // bytes of record JSON kept in memory
	TraceQuery bool
	IDHeader   bool
	ACMEEmail  string
	ACMECA     string // the ACME directory URL; empty means Let's Encrypt
	Sites      []*Site
	Pools      map[string]*PoolSpec
	PoolOrder  []string
	Ports      map[int]*Port
	roots      []*os.Root
	warns      []Problem // what Parse found; Warnings adds plan's
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
	Drain           time.Duration // how long removed backends drain; 0 means 30 s
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
	for {
		s = strings.TrimLeft(s, " \t\r")
		switch {
		case s == "" || s[0] == '#':
			return out, nil
		case s[0] == '"':
			var b strings.Builder
			j := 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				b.WriteByte(s[j])
				j++
			}
			if j >= len(s) {
				return nil, errors.New("a quoted text has no closing quote")
			}
			out = append(out, token{b.String(), true})
			s = s[j+1:]
		default:
			j := strings.IndexAny(s, " \t\r")
			if j < 0 {
				j = len(s)
			}
			out = append(out, token{s[:j], false})
			s = s[j:]
		}
	}
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
	opt   ParseOptions
	probs []Problem
}

func (p *parser) errf(line int, f string, a ...any) {
	p.probs = append(p.probs, Problem{Line: line, Msg: fmt.Sprintf(f, a...)})
}

func (p *parser) warnf(line int, f string, a ...any) {
	p.probs = append(p.probs, Problem{Line: line, Msg: fmt.Sprintf(f, a...), Warn: true})
}

// ParseOptions changes how a config is compiled.
type ParseOptions struct {
	// NoDisk skips everything that needs a disk: folders are not opened and
	// certificates are not loaded, so a config can be checked where there is
	// no disk, such as the browser demo. Files rules get no Root, and explain
	// says so instead of looking.
	NoDisk bool
}

// Parse compiles config source. It returns the config even when there are
// problems; callers check HasErrors before using it.
func Parse(file, src string) (*Config, []Problem) {
	return ParseWith(file, src, ParseOptions{})
}

// ParseWith is Parse with options.
func ParseWith(file, src string, o ParseOptions) (*Config, []Problem) {
	c := &Config{
		File:     file,
		Admin:    "/run/bareproxy/admin.sock",
		TraceLog: "stdout",
		TraceMem: 32 << 20,
		IDHeader: true,
		Pools:    map[string]*PoolSpec{},
		Ports:    map[int]*Port{},
	}
	p := &parser{c: c, dir: filepath.Dir(file), opt: o}
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
			err = p.global(ln, w)
		case "site":
			err = p.siteSetting(site, ln, toks, w)
		case "pool":
			err = p.poolSetting(pool, ln, w)
		default:
			err = errors.New("indented line outside a global, site or pool block")
		}
		if err != nil {
			p.errf(ln, "%v", err)
		}
	}
	p.compile()
	slices.SortStableFunc(p.probs, func(a, b Problem) int { return a.Line - b.Line })
	c.warns = p.probs
	return c, p.probs
}

func (p *parser) path(s string) string {
	if filepath.IsAbs(s) {
		return filepath.Clean(s)
	}
	return filepath.Join(p.dir, s)
}

func onOff(s string) (bool, error) {
	if s != "on" && s != "off" {
		return false, fmt.Errorf("expected on or off, got %q", s)
	}
	return s == "on", nil
}

func (p *parser) global(ln int, w []string) error {
	c := p.c
	if len(w) != 2 && slices.Contains([]string{"admin", "state", "trace-memory", "trace-query", "id-header", "acme-email", "acme-ca"}, w[0]) {
		return fmt.Errorf("%s takes one value", w[0])
	}
	var err error
	switch w[0] {
	case "admin":
		c.Admin = "off"
		if w[1] != "off" {
			c.Admin = p.path(w[1])
		}
	case "state":
		c.State = p.path(w[1])
	case "trace-query":
		c.TraceQuery, err = onOff(w[1])
	case "id-header":
		c.IDHeader, err = onOff(w[1])
	case "trace-memory":
		if w[1] == "off" {
			c.TraceMem = 0
		} else if n, e := parseSize(w[1]); e != nil {
			err = e
		} else {
			c.TraceMem = n
		}
	case "trace-log":
		if len(w) != 2 && len(w) != 4 {
			return errors.New("trace-log takes stdout, off, or a file with an optional size and count")
		}
		file := w[1] != "stdout" && w[1] != "off"
		c.TraceLog = w[1]
		if file {
			c.TraceLog = p.path(w[1])
		}
		c.TraceSize, c.TraceCount = 0, 0
		if len(w) == 4 {
			size, err1 := parseSize(w[2])
			count, err2 := strconv.Atoi(w[3])
			switch {
			case !file:
				return fmt.Errorf("only a trace log file can rotate, not %s", w[1])
			case err1 != nil || size == 0:
				return errors.New("the size to rotate at must be more than zero, such as 10MB")
			case err2 != nil || count < 1:
				return errors.New("the number of old files to keep must be 1 or more")
			}
			c.TraceSize, c.TraceCount = size, count
		}
	case "acme-email":
		if !strings.Contains(w[1], "@") {
			return errors.New("acme-email takes an email address")
		}
		c.ACMEEmail = w[1]
	case "acme-ca":
		if u, e := url.Parse(w[1]); e != nil || u.Scheme != "https" || u.Host == "" {
			return errors.New("acme-ca takes the https:// URL of an ACME directory")
		}
		c.ACMECA = w[1]
	case "trust",
		"client-header-timeout", "client-body-timeout", "client-idle-timeout", "shutdown-timeout":
		p.warnf(ln, "%s isn't built yet in this version, so it's ignored", w[0])
	default:
		return fmt.Errorf("unknown global setting %q", w[0])
	}
	return err
}

func (p *parser) siteSetting(s *Site, ln int, toks []token, w []string) error {
	switch w[0] {
	case "route":
		r, err := p.route(ln, toks)
		if err != nil {
			return err
		}
		s.Routes = append(s.Routes, r)
	case "tls":
		switch {
		case len(w) == 2 && w[1] == "auto":
			s.TLSAuto, s.TLSLine = true, ln
		case len(w) == 3:
			s.CertFile, s.KeyFile, s.TLSLine = p.path(w[1]), p.path(w[2]), ln
		default:
			return errors.New("tls takes auto, or a certificate file and a key file")
		}
	case "error":
		switch {
		case len(w) != 3:
			return errors.New("error takes a status and a path, such as error 404 /404.html")
		case w[1] != "404":
			return errors.New("only error 404 is supported")
		case !isNormalPath(w[2]) || strings.HasSuffix(w[2], "/"):
			return errors.New("the error page must be a plain file path such as /404.html")
		}
		s.Err404, s.Err404Line = w[2], ln
	case "encoded-slashes":
		if len(w) != 2 || (w[1] != "reject" && w[1] != "keep") {
			return errors.New("encoded-slashes takes reject or keep")
		}
		s.KeepEncodedSlash = w[1] == "keep"
	case "body-limit":
		if len(w) != 2 {
			return errors.New("body-limit takes one size, such as 10MB")
		}
		n, err := parseSize(w[1])
		if err != nil {
			return err
		}
		s.BodyLimit = n
	case "set-response-header", "remove-response-header":
		p.warnf(ln, "%s isn't built yet in this version, so it's ignored", w[0])
	default:
		return fmt.Errorf("unknown site setting %q", w[0])
	}
	return nil
}

func (p *parser) route(ln int, toks []token) (*Route, error) {
	r := &Route{Line: ln, Text: joinTokens(toks)}
	arrow := slices.IndexFunc(toks, func(t token) bool { return t.s == "->" && !t.quoted })
	if arrow < 0 {
		return nil, errors.New("route needs -> before its action")
	}
	left, right := toks[1:arrow], toks[arrow+1:]
	if len(left) == 0 {
		return nil, errors.New("route needs a path, such as /api/*")
	}
	if !strings.HasPrefix(left[0].s, "/") {
		r.Methods = strings.Split(left[0].s, ",")
		if j := slices.IndexFunc(r.Methods, func(m string) bool { return !validMethod(m) }); j >= 0 {
			return nil, fmt.Errorf("%q isn't a method; write methods in capitals, such as GET,HEAD", r.Methods[j])
		}
		left = left[1:]
	}
	if len(left) == 0 {
		return nil, errors.New("route needs a path after the methods")
	}
	ps := left[0].s
	r.Path, r.Prefix = strings.CutSuffix(ps, "/*")
	check := cmp.Or(r.Path, "/")
	if strings.Contains(check, "*") || !isNormalPath(check) || (r.Prefix && strings.HasSuffix(r.Path, "/")) {
		return nil, fmt.Errorf("path %q isn't in normal form: no * except a final /*, no //, no . or .. parts, and escapes only where needed", ps)
	}
	for left = left[1:]; len(left) > 0; left = left[2:] {
		if left[0].s != "header" || len(left) < 2 {
			return nil, fmt.Errorf("unexpected %q: after the path a route takes only header NAME or header NAME=VALUE", left[0].s)
		}
		name, val, has := strings.Cut(left[1].s, "=")
		if !validHeaderName(name) {
			return nil, fmt.Errorf("%q isn't a header name", name)
		}
		r.Headers = append(r.Headers, HeaderCond{Name: http.CanonicalHeaderKey(name), Value: val, HasValue: has})
	}
	if len(right) == 0 {
		return nil, errors.New("route needs an action after ->")
	}
	a := &r.Act
	switch right[0].s {
	case "files":
		if len(right) != 2 {
			return nil, errors.New("files takes one folder")
		}
		a.Kind, a.Dir = "files", p.path(right[1].s)
	case "redirect":
		if len(right) != 3 {
			return nil, errors.New("redirect takes a code and a URL")
		}
		code, _ := strconv.Atoi(right[1].s)
		if !slices.Contains([]int{301, 302, 307, 308}, code) {
			return nil, errors.New("the redirect code must be 301, 302, 307 or 308")
		}
		u, err := url.Parse(right[2].s)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, errors.New("redirect needs a full http:// or https:// URL")
		}
		a.Kind, a.Code, a.URL = "redirect", code, right[2].s
	case "respond":
		if len(right) < 2 || len(right) > 3 {
			return nil, errors.New("respond takes a status and an optional text")
		}
		st, err := strconv.Atoi(right[1].s)
		if err != nil || st < 200 || st > 599 {
			return nil, errors.New("respond needs a status from 200 to 599")
		}
		a.Kind, a.Status = "respond", st
		if len(right) == 3 {
			a.Body = right[2].s
		}
	default:
		if !validName(right[0].s) {
			return nil, fmt.Errorf("%q isn't an action: use a pool name, files, redirect or respond", right[0].s)
		}
		a.Kind, a.Pool = "pool", right[0].s
		switch {
		case len(right) == 2 && right[1].s == "strip":
			a.Strip = true
			if r.Path == "" {
				p.warnf(ln, "strip does nothing on /*")
			}
		case len(right) != 1:
			return nil, errors.New("after a pool name a route takes only strip")
		}
	}
	return r, nil
}

func (p *parser) poolSetting(ps *PoolSpec, ln int, w []string) error {
	setDuration := func(dst *time.Duration) error {
		if len(w) != 2 {
			return fmt.Errorf("%s takes one duration, such as 5s", w[0])
		}
		d, err := time.ParseDuration(w[1])
		if err != nil || d <= 0 {
			return fmt.Errorf("bad duration %q: use a number with ms, s or m", w[1])
		}
		*dst = d
		return nil
	}
	switch w[0] {
	case "backend":
		if len(w) != 2 {
			return errors.New("backend takes one address, such as 10.0.0.11:8080")
		}
		if strings.HasPrefix(w[1], "unix:") {
			return errors.New("unix: backends aren't built yet in this version")
		}
		a, https := strings.CutPrefix(w[1], "https://")
		if !https {
			a = strings.TrimPrefix(a, "http://")
		}
		host, port, err := net.SplitHostPort(a)
		if err != nil || host == "" {
			return errors.New("a backend address is host:port, such as 10.0.0.11:8080")
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("bad backend port %q", port)
		}
		if i := slices.IndexFunc(ps.Backends, func(b BackendSpec) bool { return b.Addr == a && b.HTTPS == https }); i >= 0 {
			return fmt.Errorf("backend %s is already listed on line %d", a, ps.Backends[i].Line)
		}
		ps.Backends = append(ps.Backends, BackendSpec{Line: ln, Addr: a, HTTPS: https})
	case "health":
		if len(w) < 2 || !strings.HasPrefix(w[1], "/") {
			return errors.New("health takes a path, such as /healthz")
		}
		h := &HealthSpec{Line: ln, Path: w[1], Every: 5 * time.Second, Timeout: 2 * time.Second, Lo: 200, Hi: 399}
		for i := 2; i < len(w); i += 2 {
			if i+1 >= len(w) {
				return fmt.Errorf("%s needs a value", w[i])
			}
			switch w[i] {
			case "every", "timeout":
				d, err := time.ParseDuration(w[i+1])
				if err != nil || d <= 0 {
					return fmt.Errorf("bad duration %q", w[i+1])
				}
				if w[i] == "every" {
					h.Every = d
				} else {
					h.Timeout = d
				}
			case "expect":
				lo, hi, ok := parseRange(w[i+1])
				if !ok {
					return errors.New("expect takes a status range such as 200-399")
				}
				h.Lo, h.Hi = lo, hi
			default:
				return fmt.Errorf("unknown health option %q: use every, timeout or expect", w[i])
			}
		}
		ps.Health = h
	case "host-header":
		if len(w) != 2 {
			return errors.New("host-header takes one value")
		}
		ps.HostHeader = w[1]
	case "connect-timeout":
		return setDuration(&ps.ConnectTimeout)
	case "response-timeout":
		return setDuration(&ps.ResponseTimeout)
	case "drain":
		return setDuration(&ps.Drain)
	case "retries":
		if len(w) != 2 || (w[1] != "0" && w[1] != "1" && w[1] != "2") {
			return errors.New("retries takes 0, 1 or 2")
		}
		ps.Retries = int(w[1][0] - '0')
	default:
		return fmt.Errorf("unknown pool setting %q", w[0])
	}
	return nil
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
				if c.Pools[r.Act.Pool] == nil {
					p.errf(r.Line, "no pool named %s", r.Act.Pool)
				}
			case "files":
				if p.opt.NoDisk {
					continue
				}
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
		https := slices.ContainsFunc(s.Addrs, func(a Addr) bool { return a.Scheme == "https" })
		switch {
		case https && s.CertFile == "": // tls auto, or no tls line
			s.TLSAuto = true
			for _, a := range s.Addrs {
				if a.Scheme == "https" && strings.HasPrefix(a.Host, "*") {
					p.errf(s.Line, "%s needs certificate files (tls CERT KEY): automatic certificates are for exact host names only", a.Text)
				}
			}
		case https && p.opt.NoDisk:
			// the certificate files are not read, so they are not checked
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
		red := &Site{Line: s.Line, Name: s.Name, Synthetic: true, BodyLimit: s.BodyLimit, TLSAuto: s.TLSAuto,
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
	table, key := p.Exact, host
	switch {
	case host == "*":
		if p.Any != nil && p.Any != s {
			return p.Any
		}
		p.Any = s
		return nil
	case strings.HasPrefix(host, "*."):
		table, key = p.Wild, host[2:]
	}
	if o := table[key]; o != nil && o != s {
		return o
	}
	table[key] = s
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
	a := Addr{Text: s, Scheme: "https", Port: 443}
	rest := strings.TrimPrefix(s, "https://")
	if r, ok := strings.CutPrefix(s, "http://"); ok {
		a.Scheme, a.Port, rest = "http", 80, r
	}
	host := rest
	if i := strings.LastIndexByte(rest, ':'); i >= 0 {
		host = rest[:i]
		if port := rest[i+1:]; port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 {
				return a, fmt.Errorf("bad port in address %q", s)
			}
			a.Port = n
		}
	}
	host = strings.ToLower(host)
	if host != "*" && !validHost(strings.TrimPrefix(host, "*.")) {
		return a, fmt.Errorf("bad host name in address %q", s)
	}
	a.Host = host
	return a, nil
}

// isAll reports whether s is not empty and every character of it passes ok.
func isAll(s string, ok func(rune) bool) bool {
	return s != "" && strings.IndexFunc(s, func(c rune) bool { return !ok(c) }) < 0
}

func isLower(c rune) bool { return 'a' <= c && c <= 'z' }
func isUpper(c rune) bool { return 'A' <= c && c <= 'Z' }
func isDigit(c rune) bool { return '0' <= c && c <= '9' }

func validHost(h string) bool {
	return len(h) <= 253 && !slices.ContainsFunc(strings.Split(h, "."), func(label string) bool {
		return len(label) > 63 || !isAll(label, func(c rune) bool { return isLower(c) || isDigit(c) || c == '-' })
	})
}

var reservedNames = map[string]bool{"files": true, "redirect": true, "respond": true, "strip": true}

func validName(s string) bool {
	return !reservedNames[s] && isAll(s, func(c rune) bool { return isLower(c) || isUpper(c) || isDigit(c) || c == '-' || c == '_' })
}

func validMethod(s string) bool { return len(s) <= 20 && isAll(s, isUpper) }

func validHeaderName(s string) bool {
	return isAll(s, func(c rune) bool { return isLower(c) || isUpper(c) || isDigit(c) || c == '-' })
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

// schemeName is the URL scheme of a TLS or a plain connection.
func schemeName(isTLS bool) string {
	if isTLS {
		return "https"
	}
	return "http"
}

// Summary describes a config in one line.
func Summary(c *Config) string {
	rules := 0
	for _, s := range c.Sites {
		rules += len(s.Routes)
	}
	var ls []string
	for _, p := range sortedPorts(c) {
		ls = append(ls, fmt.Sprintf(":%d (%s)", p.Num, schemeName(p.TLS)))
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
	return slices.SortedFunc(maps.Values(c.Ports), func(a, b *Port) int { return a.Num - b.Num })
}
