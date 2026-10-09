// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"bareproxy/internal/plugin"
)

// Version is this build's version.
const Version = "0.6.1"

// Runtime is one running config version with its live pools.
type Runtime struct {
	Cfg      *Config
	Version  int
	Pools    map[string]*Pool
	backends map[string]*Backend
	Trace    *TraceLog
	Mem      *TraceMem // recent records and events, inherited across reloads
	Plugins  map[string]*plugin.Plugin
	acme     *acmeState
}

// NewRuntime builds live pools for a config. Backends that old already has
// are reused, so their health and counters carry over a reload. With checks
// off (for explain without a server) it starts nothing and opens no log.
func NewRuntime(c *Config, old *Runtime, version int, logf func(string, ...any), checks bool) (*Runtime, error) {
	rt := &Runtime{Cfg: c, Version: version, Pools: map[string]*Pool{}, backends: map[string]*Backend{}, Mem: newTraceMem()}
	if old != nil && old.Mem != nil {
		rt.Mem = old.Mem
	}
	if old != nil && old.Trace != nil && old.Trace.Spec == c.TraceLog {
		rt.Trace = old.Trace
	} else if checks {
		t, err := OpenTraceLog(c.TraceLog)
		if err != nil {
			return nil, fmt.Errorf("can't open the trace log: %v", err)
		}
		rt.Trace = t
	}
	if rt.Trace != nil {
		rt.Trace.SetRotation(c.TraceSize, c.TraceCount)
	}
	for _, name := range c.PoolOrder {
		ps := c.Pools[name]
		pool := &Pool{Spec: ps, transport: newTransport(ps)}
		for _, bs := range ps.Backends {
			key := fmt.Sprintf("%s|%s|%v|%s", name, bs.Addr, bs.HTTPS, ps.Health.key())
			var b *Backend
			if old != nil {
				b = old.backends[key]
			}
			if b == nil {
				b = &Backend{Spec: bs, Pool: name, key: key, health: ps.Health, state: "up", since: time.Now()}
				if ps.Health != nil {
					b.state = "unknown"
				}
				b.events = func(s string) {
					if logf != nil {
						logf("pool %s: %s", name, s)
					}
					rt.Mem.Event("backend", "pool "+name+": "+s)
				}
				if checks && ps.Health != nil {
					ctx, cancel := context.WithCancel(context.Background())
					b.stop = cancel
					go b.runChecks(ctx)
				}
			}
			pool.Backends = append(pool.Backends, b)
			rt.backends[key] = b
		}
		rt.Pools[name] = pool
	}
	if checks {
		if err := rt.startPlugins(old, logf); err != nil {
			return nil, err
		}
		var prev *acmeState
		if old != nil {
			prev = old.acme
		}
		rt.acme = newACME(c, prev, rt.Mem)
	}
	return rt, nil
}

// Stop ends the runtime's health checks.
func (rt *Runtime) Stop() {
	for _, b := range rt.backends {
		if b.stop != nil {
			b.stop()
		}
	}
	rt.stopPlugins(nil)
}

// retire stops what the old runtime has and the new one doesn't. Folders and
// idle connections close a little later, after requests in flight finish.
func retire(old, rt *Runtime) {
	for k, b := range old.backends {
		if _, ok := rt.backends[k]; !ok && b.stop != nil {
			b.stop()
		}
	}
	if old.Trace != nil && old.Trace != rt.Trace {
		time.AfterFunc(time.Minute, old.Trace.Close)
	}
	time.AfterFunc(2*time.Minute, func() {
		old.stopPlugins(rt)
		old.Cfg.Close()
		for _, p := range old.Pools {
			p.transport.CloseIdleConnections()
		}
	})
}

// Server runs BareProxy.
type Server struct {
	file      string
	rt        atomic.Pointer[Runtime]
	mu        sync.Mutex
	cs        changeState // listeners, history and the running text (apply.go)
	adminPath string
	logger    *log.Logger
}

// NewServer makes a server around a runtime, for tests and embedding.
func NewServer(file string, rt *Runtime) *Server {
	s := &Server{file: file, logger: log.New(os.Stderr, "bareproxy: ", log.LstdFlags)}
	s.rt.Store(rt)
	return s
}

func (s *Server) logf(f string, a ...any) { s.logger.Printf(f, a...) }

// Current returns the running runtime.
func (s *Server) Current() *Runtime { return s.rt.Load() }

// Run starts BareProxy with a config file and serves until stopped.
func Run(file string) error {
	s, err := Start(file)
	if err != nil {
		return err
	}
	if err := s.startAdmin(s.Current().Cfg.Admin); err != nil {
		s.logf("no admin socket (%v), so explain reads the file instead", err)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, append([]os.Signal{os.Interrupt, syscall.SIGTERM}, reloadSignals...)...)
	for x := range sig {
		if isReloadSignal(x) {
			s.Reload()
			continue
		}
		s.logf("stopping")
		s.shutdown()
		return nil
	}
	return nil
}

func (s *Server) shutdown() {
	s.closeListeners()
	if s.adminPath != "" {
		os.Remove(s.adminPath)
	}
	if rt := s.rt.Load(); rt != nil {
		rt.Stop()
	}
}

// certFor picks the certificate of the site named in the TLS handshake.
func (s *Server) certFor(port int) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		rt := s.rt.Load()
		c := rt.Cfg
		p := c.Ports[port]
		if p == nil {
			return nil, errors.New("no sites on this port")
		}
		if site, _ := p.Find(hello.ServerName); hello.ServerName != "" && site != nil {
			if site.Cert != nil {
				return site.Cert, nil
			} else if site.TLSAuto && rt.acme != nil {
				return rt.acme.get(hello)
			}
		}
		for _, site := range c.Sites {
			if site.Cert != nil && slices.ContainsFunc(site.Addrs, func(a Addr) bool { return a.Port == port }) {
				return site.Cert, nil
			}
		}
		return nil, errors.New("no certificate")
	}
}

// adminHandlers add endpoints to the admin socket. Each file registers its
// own in an init function, so files don't collide.
var adminHandlers []func(s *Server, mux *http.ServeMux)

// startAdmin serves explain for the live config on a Unix socket.
func (s *Server) startAdmin(path string) error {
	if path == "" || path == "off" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	os.Chmod(path, 0o660)
	s.adminPath = path
	mux := http.NewServeMux()
	mux.HandleFunc("GET /explain", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		h := http.Header{}
		for _, kv := range q["h"] {
			k, v, _ := strings.Cut(kv, ":")
			h.Add(strings.TrimSpace(k), strings.TrimSpace(v))
		}
		out, err := Explain(s.rt.Load(), q.Get("method"), q.Get("url"), h, true)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		io.WriteString(w, out)
	})
	for _, add := range adminHandlers {
		add(s, mux)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ConnContext: peerContext}
	s.setAdmin(srv)
	go srv.Serve(ln)
	return nil
}

type respWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	pl     *pluginRun // the request's plugins, when its site uses any
}

func (w *respWriter) WriteHeader(code int) {
	if w.pl != nil && w.pl.replaced {
		return
	}
	if w.pl != nil {
		var held bool
		if code, held = w.pl.header(w, code); held {
			return
		}
	}
	w.writeHeader(code)
}

// writeHeader sends the status, past the plugins.
func (w *respWriter) writeHeader(code int) {
	if code >= 200 && w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *respWriter) Write(b []byte) (int, error) {
	if w.pl != nil && w.pl.replaced {
		return len(b), nil
	}
	if w.pl != nil && !w.pl.seen {
		w.WriteHeader(http.StatusOK)
	}
	if w.pl != nil && w.pl.buffering {
		return w.pl.write(w, b)
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// ReadFrom hands a file to the underlying writer's ReaderFrom, which on plain
// HTTP/1.1 sends it with sendfile. The status and the byte count are kept as
// Write keeps them. A body held for the plugins is read in instead.
func (w *respWriter) ReadFrom(r io.Reader) (int64, error) {
	if w.pl != nil && w.pl.replaced {
		return io.Copy(io.Discard, r)
	}
	if w.pl != nil && !w.pl.seen {
		w.WriteHeader(http.StatusOK)
	}
	if w.pl != nil && w.pl.buffering {
		return io.Copy(struct{ io.Writer }{w}, r)
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	var n int64
	var err error
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err = rf.ReadFrom(r)
	} else {
		n, err = io.Copy(struct{ io.Writer }{w.ResponseWriter}, r)
	}
	w.bytes += n
	return n, err
}

func (w *respWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *respWriter) Flush() {
	if w.pl != nil {
		switch {
		case w.pl.replaced:
			return
		case !w.pl.seen:
			w.WriteHeader(http.StatusOK)
		}
		if w.pl.buffering { // a response that flushes is streaming: it goes out as it comes
			w.pl.bodyNote("the response streams (it flushed), so its body wasn't handed over")
			if w.pl.release(w) != nil {
				return
			}
		}
	}
	http.NewResponseController(w.ResponseWriter).Flush()
}

type countReader struct {
	io.ReadCloser
	n atomic.Int64 // the transport reads the body on its own goroutine
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// Handler serves the requests arriving on one port. Every request leaves
// exactly one record.
func (s *Server) Handler(port int, isTLS bool) http.Handler {
	return http.HandlerFunc(func(w0 http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rt := s.rt.Load()
		w := &respWriter{ResponseWriter: w0}
		rec := &Record{ID: newID(), Time: start.UTC().Format("2006-01-02T15:04:05.000Z"), Config: rt.Version,
			Client: clientIP(r.RemoteAddr), Proto: r.Proto, Method: r.Method, Scheme: schemeName(isTLS), Host: r.Host}
		rec.TraceID, _ = requestTrace(r.Header)
		if r.TLS != nil {
			rec.TLS = strings.TrimPrefix(tls.VersionName(r.TLS.Version), "TLS ")
			rec.SNI = r.TLS.ServerName
		}
		if rt.Cfg.TraceQuery {
			rec.Query = r.URL.RawQuery
		}
		if rt.Cfg.IDHeader {
			w.Header().Set("BareProxy-Id", rec.ID)
		}
		var body *countReader
		if r.Body != nil && r.Body != http.NoBody {
			body = &countReader{ReadCloser: r.Body}
			r.Body = body
		}
		finish := func() {
			rec.Status = w.status
			if n := len(rec.Attempts); rec.Status == 0 && n > 0 && rec.Attempts[n-1].Status == http.StatusSwitchingProtocols {
				rec.Status, rec.Outcome = http.StatusSwitchingProtocols, "upgraded"
			}
			rec.BytesOut = w.bytes
			if body != nil {
				rec.BytesIn = body.n.Load()
			}
			if w.pl != nil {
				w.pl.finish()
			}
			rec.MS = msSince(start)
			rt.record(rec)
		}
		defer func() {
			if p := recover(); p != nil {
				if r.Context().Err() != nil {
					rec.Outcome = "client_gone"
				} else if rec.Outcome == "ok" {
					rec.Outcome = "bad_response"
				}
				finish()
				panic(p)
			}
			finish()
		}()
		s.serve(w, r, rt, port, rec)
		if w.pl != nil {
			w.pl.finishBody(w)
		}
	})
}

func (s *Server) serve(w *respWriter, r *http.Request, rt *Runtime, port int, rec *Record) {
	p := rt.Cfg.Ports[port]
	rec.Path = requestPath(r)
	var site *Site
	if p != nil {
		site, _ = p.Find(hostOnly(r.Host))
	}
	if r.Method == http.MethodOptions && rec.Path == "*" { // asks about the server itself
		if site != nil {
			rec.Site, rec.SiteLine = site.Name, site.Line
		}
		rec.Outcome, rec.Rule = "local", "OPTIONS *"
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
		return
	}
	if site == nil {
		rec.Outcome = "no_site"
		s.plain(w, r, rec, http.StatusMisdirectedRequest, "No site here for "+hostOnly(r.Host))
		return
	}
	rec.Site, rec.SiteLine = site.Name, site.Line
	if r.TLS != nil && r.TLS.ServerName != "" {
		if s2, _ := p.Find(r.TLS.ServerName); s2 != site {
			rec.Outcome, rec.Reason = "no_site", "Host and SNI belong to different sites"
			s.plain(w, r, rec, http.StatusMisdirectedRequest, "Host and SNI belong to different sites")
			return
		}
	}
	if len(site.Uses) > 0 && rt.Plugins != nil && s.pluginRequest(w, r, rt, site, rec) {
		return
	}
	norm, err := NormalizePath(rec.Path, site.KeepEncodedSlash)
	if err != nil {
		rec.Outcome, rec.Reason = "bad_request", err.Error()
		s.plain(w, r, rec, http.StatusBadRequest, "Bad request: "+err.Error())
		return
	}
	if norm != rec.Path {
		rec.NormPath = norm
	}
	route, _ := site.match(r.Method, norm, r.Header, false)
	if route == nil {
		rec.Outcome, rec.Reason = "no_route", "no rule matches"
		s.notFound(w, r, rec, site)
		return
	}
	rec.Line, rec.Rule = route.Line, route.Text
	a := route.Act
	if a.Kind == "https" && site.TLSAuto && rt.acme != nil && strings.HasPrefix(norm, "/.well-known/acme-challenge/") {
		rec.Outcome, rec.Rule = "local", "ACME challenge (HTTP-01)"
		rt.acme.http01.ServeHTTP(w, r)
		return
	}
	switch a.Kind {
	case "respond":
		rec.Outcome = "local"
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(a.Status)
		if a.Body != "" && r.Method != http.MethodHead {
			io.WriteString(w, a.Body+"\n")
		}
	case "redirect", "https":
		loc := redirectTarget(a.URL, norm, r.URL.RawQuery)
		if a.Kind == "https" {
			loc = withQuery("https://"+hostOnly(r.Host)+norm, r.URL.RawQuery)
		}
		rec.Outcome, rec.Location = "local", loc
		w.Header().Set("Location", loc)
		w.WriteHeader(a.Code)
	case "files":
		s.files(w, r, rec, site, route, norm)
	case "pool":
		s.proxy(w, r, rt, site, route, norm, rec)
	}
}

func (s *Server) files(w *respWriter, r *http.Request, rec *Record, site *Site, route *Route, norm string) {
	a := route.Act
	rec.Outcome, rec.Folder = "file", a.Dir
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		rec.Reason = "files answer only GET and HEAD"
		w.Header().Set("Allow", "GET, HEAD")
		s.plain(w, r, rec, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	fr := LookupFile(a.Root, norm)
	rec.Checked = fr.Checked
	switch fr.Status {
	case http.StatusMovedPermanently:
		loc := withQuery(fr.Location, r.URL.RawQuery)
		rec.Location = loc
		w.Header().Set("Location", loc)
		w.WriteHeader(http.StatusMovedPermanently)
		return
	case http.StatusNotFound:
		rec.Reason = fr.Reason
		s.notFound(w, r, rec, site)
		return
	}
	rep, variants := ChooseRep(a.Root, fr.Rel, r.Header.Get("Accept-Encoding"))
	rec.File = fr.Rel
	if rep.Encoding != "" {
		rec.Sent, rec.Encoding = rep.Name, rep.Encoding
	}
	rec.ContentType = ContentType(fr.Rel)
	if err := serveFile(w, r, a.Root, rep, rec.ContentType, len(variants) > 0); err != nil {
		rec.Reason = statReason(nil, err)
		s.notFound(w, r, rec, site)
	}
}

// notFound sends the site's 404 page if it has one, or a plain 404. It is
// only used for 404s BareProxy makes itself, never for a backend's.
func (s *Server) notFound(w *respWriter, r *http.Request, rec *Record, site *Site) {
	if route, fr, ok := site.ErrorPage(); ok {
		if serveErrorPage(w, r, route.Act.Root, fr.Rel, http.StatusNotFound) == nil {
			rec.ErrorPage = inFolder(route.Act.Dir, fr.Rel)
			return
		}
	}
	s.plain(w, r, rec, http.StatusNotFound, "Not found")
}

// plain sends a short text response that carries the request ID.
func (s *Server) plain(w *respWriter, r *http.Request, rec *Record, status int, msg string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Del("Content-Length")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		fmt.Fprintf(w, "%s\nRequest ID: %s\n", msg, rec.ID)
	}
}

// requestPath returns the path exactly as the client sent it.
func requestPath(r *http.Request) string {
	uri := r.RequestURI
	if uri == "*" {
		return uri
	}
	if strings.HasPrefix(uri, "/") {
		path, _, _ := strings.Cut(uri, "?")
		return path
	}
	if p := r.URL.EscapedPath(); p != "" {
		return p
	}
	return "/"
}

func newID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func clientIP(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000
}
