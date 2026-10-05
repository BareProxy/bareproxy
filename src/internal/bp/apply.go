// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
)

// changeState is what a Server keeps for config changes. It is guarded by
// Server.mu.
type changeState struct {
	lns      map[int]*listener   // open listeners by port; nil when the server doesn't own them
	hist     *history            // nil when no history is kept
	text     string              // the running config text
	mismatch string              // why the file doesn't hold the running config, if it doesn't
	conns    map[string]*connSet // the running version's backend connections, by pool
	admin    *http.Server        // the admin socket's server, if it runs
}

// Stop stops serving: listeners close once their requests in flight finish
// (30 s at most), and the admin socket goes away.
func (s *Server) Stop() { s.shutdown() }

func (s *Server) setAdmin(srv *http.Server) {
	s.mu.Lock()
	s.cs.admin = srv
	s.mu.Unlock()
}

type listener struct {
	srv *http.Server
	ln  net.Listener
	tls bool
}

// Change asks for a config text to go live.
type Change struct {
	Text   string
	How    string // startup, apply, rollback or reload
	User   string // the Unix user who asked
	PlanID string // when set, refuse if the plan for this change has another ID
	From   int    // for a rollback, the version restored

	keepFile bool // run the text but leave the file alone (startup fallback)
	version  int  // run as this history version (startup fallback)
}

// Applied says what an apply did.
type Applied struct {
	Version   int         `json:"version"`
	Previous  int         `json:"previous,omitempty"`
	Unchanged bool        `json:"unchanged,omitempty"`
	PlanID    string      `json:"plan_id,omitempty"`
	Wrote     string      `json:"wrote,omitempty"` // the config file, when the apply rewrote it
	Warnings  []string    `json:"warnings,omitempty"`
	Plan      *PlanResult `json:"-"`
}

// ConfigError is a config that can't go live, with its problems.
type ConfigError struct{ Problems []Problem }

func (e *ConfigError) Error() string {
	msg := "the config has errors, so nothing changed"
	for _, p := range e.Problems {
		msg += "\n  " + p.String()
	}
	return msg
}

// PlanChangedError refuses an apply whose plan is out of date.
type PlanChangedError struct{ ID string }

func (e *PlanChangedError) Error() string {
	return fmt.Sprintf("the running config or the file changed since plan %s was made, so nothing changed; make a new plan", e.ID)
}

// logOutput is where a started server logs.
var logOutput io.Writer = os.Stderr

// Start starts BareProxy with a config file and serves until Stop. When the
// file has errors it runs the last version in the history, if there is one,
// and flags the mismatch.
func Start(file string) (*Server, error) {
	s := &Server{file: file, logger: log.New(logOutput, "bareproxy: ", log.LstdFlags)}
	s.cs.lns = map[int]*listener{}
	abs, _ := filepath.Abs(file)
	data, err := os.ReadFile(abs)
	var c *Config
	probs := []Problem{{Msg: fmt.Sprint(err)}}
	if err == nil {
		c, probs = Parse(abs, string(data))
	}
	for _, p := range probs {
		s.logf("%s", p)
	}
	if s.cs.hist, err = openHistory(stateDir(c)); err != nil {
		s.logf("no config history: %v (set state in global to a folder BareProxy can write)", err)
	}
	if c != nil {
		c.Close()
	}
	user := userName(os.Getuid())
	if c != nil && !HasErrors(probs) {
		if _, err := s.Apply(Change{Text: string(data), How: "startup", User: user}); err != nil {
			return nil, err
		}
		return s, nil
	}
	n := s.cs.hist.last()
	if n == 0 {
		return nil, errors.New("the config has errors, so BareProxy didn't start")
	}
	text, err := s.cs.hist.text(n)
	if err == nil {
		_, err = s.Apply(Change{Text: text, How: "startup", User: user, keepFile: true, version: n})
	}
	if err != nil {
		return nil, fmt.Errorf("the config has errors, and version %d from the history didn't start either: %v", n, err)
	}
	s.mu.Lock()
	s.cs.mismatch = fmt.Sprintf("%s has errors, so version %d from the history is running; fix the file and apply it", abs, n)
	s.logEvent("start", "%s", s.cs.mismatch)
	s.mu.Unlock()
	return s, nil
}

// ConfigMismatch says why the config file doesn't hold the running config,
// or returns "" when it does.
func (s *Server) ConfigMismatch() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cs.mismatch
}

// Reload applies the config file without asking, for SIGHUP. A file with
// errors never replaces the running config.
func (s *Server) Reload() {
	data, err := os.ReadFile(s.file)
	if err == nil {
		_, err = s.Apply(Change{Text: string(data), How: "reload", User: userName(os.Getuid())})
	}
	if err != nil {
		s.mu.Lock()
		s.cs.mismatch = fmt.Sprintf("%s can't go live (%s), so version %d keeps running", s.file, strings.ReplaceAll(err.Error(), "\n  ", "; "), s.Current().Version)
		s.logEvent("reload", "reload: %s", s.cs.mismatch)
		s.mu.Unlock()
	}
}

// Rollback makes a version from the history live again. Version 0 means
// the one that went live before the running version.
func (s *Server) Rollback(version int, user string) (*Applied, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.cs.hist
	if h == nil {
		return nil, errors.New("no config history is kept, so there is nothing to roll back to")
	}
	if version == 0 {
		cur := s.rt.Load().Version
		if version = h.before(cur); version == 0 {
			return nil, fmt.Errorf("the history has no version before the running version %d", cur)
		}
	}
	text, err := h.text(version)
	if err != nil {
		return nil, err
	}
	return s.apply(Change{Text: text, How: "rollback", User: user, From: version})
}

// History returns the config versions kept, oldest first.
func (s *Server) History() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cs.hist == nil {
		return nil
	}
	return append([]Entry(nil), s.cs.hist.entries...)
}

// Apply makes a config text live: it checks it in full, opens any new
// listeners, swaps the runtime with one atomic pointer write, closes removed
// listeners, drains removed backends, saves the version to the history and
// writes the config file, so the file always holds the running config.
// Startup, apply, rollback and SIGHUP all come through here.
func (s *Server) Apply(ch Change) (*Applied, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apply(ch)
}

func (s *Server) apply(ch Change) (*Applied, error) {
	old := s.rt.Load()
	file, _ := filepath.Abs(s.file)
	c, probs := Parse(file, ch.Text)
	if HasErrors(probs) {
		c.Close()
		return nil, &ConfigError{probs}
	}
	res := &Applied{}
	for _, p := range probs {
		res.Warnings = append(res.Warnings, p.String())
	}
	if old != nil && (c.Admin != old.Cfg.Admin || c.State != old.Cfg.State) {
		c.Close()
		return nil, errors.New("admin and state can't change while BareProxy runs, so nothing changed: change them in the file and restart")
	}
	if old != nil {
		res.Previous = old.Version
		res.Plan = MakePlan(old.Cfg, c)
		res.PlanID = res.Plan.ID
		if ch.PlanID != "" && ch.PlanID != res.PlanID {
			c.Close()
			return nil, &PlanChangedError{ch.PlanID}
		}
	}
	opened, err := s.openListeners(c)
	if err != nil {
		c.Close()
		return nil, err
	}
	h := s.cs.hist
	unchanged := old != nil && ch.Text == s.cs.text
	save := false
	switch {
	case unchanged:
		res.Version, res.Unchanged = old.Version, true
	case ch.version > 0:
		res.Version = ch.version
	case old == nil && h.last() > 0 && h.lastText() == ch.Text:
		res.Version = h.last()
	default:
		save = true
		res.Version = max(res.Previous, h.last()) + 1
	}
	rt, err := NewRuntime(c, old, res.Version, s.logf, true)
	if err != nil {
		for _, ln := range opened {
			ln.Close()
		}
		c.Close()
		return nil, err
	}
	conns := trackConns(rt)
	s.rt.Store(rt)
	rt.certEvents(old)
	s.switchListeners(c, opened)
	if old != nil {
		retire(old, rt)
		s.drain(old, rt, s.cs.conns)
	}
	s.cs.conns, s.cs.text = conns, ch.Text
	if save && h != nil {
		e := Entry{Version: res.Version, Time: time.Now().UTC().Format(time.RFC3339), How: ch.How,
			User: ch.User, Plan: res.PlanID, From: ch.From}
		if err := h.save(e, ch.Text); err != nil {
			res.Warnings = append(res.Warnings, "the version wasn't saved to the history: "+err.Error())
		}
	}
	if !ch.keepFile {
		s.cs.mismatch = ""
		if wrote, err := writeIfChanged(file, ch.Text); err != nil {
			s.cs.mismatch = fmt.Sprintf("version %d is running, but %s couldn't be written: %v", res.Version, file, err)
			res.Warnings = append(res.Warnings, s.cs.mismatch)
		} else if wrote {
			res.Wrote = file
		}
	}
	how := ch.How
	if ch.From > 0 {
		how = fmt.Sprintf("rollback to version %d", ch.From)
	}
	if ch.User != "" {
		how += " by " + ch.User
	}
	kind := strings.Replace(ch.How, "startup", "start", 1) // the event kind
	if unchanged {
		s.logEvent(kind, "version %d reloaded, config unchanged (%s)", res.Version, how)
	} else {
		s.logEvent(kind, "version %d running (%s): %s", res.Version, how, Summary(c))
	}
	return res, nil
}

func (h *history) lastText() string {
	t, _ := h.text(h.last())
	return t
}

// writeIfChanged writes text to the config file, through a temporary file
// and a rename, unless the file already holds exactly that text.
func writeIfChanged(file, text string) (bool, error) {
	if real, err := filepath.EvalSymlinks(file); err == nil {
		file = real // keep a symlinked config a symlink: write where it points
	}
	cur, err := os.ReadFile(file)
	if err == nil && string(cur) == text {
		return false, nil
	}
	perm := os.FileMode(0o644)
	if fi, err := os.Stat(file); err == nil {
		perm = fi.Mode().Perm()
	}
	return true, writeFileAtomic(file, []byte(text), perm)
}

// openListeners opens the ports c needs that aren't open yet, before the
// swap, so a port that can't be opened stops the apply. A port that
// switches between http and https is reopened after the swap instead.
func (s *Server) openListeners(c *Config) (map[int]net.Listener, error) {
	if s.cs.lns == nil {
		return nil, nil
	}
	opened := map[int]net.Listener{}
	for _, p := range sortedPorts(c) {
		if _, ok := s.cs.lns[p.Num]; ok {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p.Num))
		if err != nil {
			for _, l := range opened {
				l.Close()
			}
			return nil, fmt.Errorf("can't open port %d, so nothing changed: %v", p.Num, err)
		}
		opened[p.Num] = ln
	}
	return opened, nil
}

// switchListeners runs after the swap: it closes the ports c doesn't have,
// letting their requests in flight finish, and starts serving new ones.
func (s *Server) switchListeners(c *Config, opened map[int]net.Listener) {
	if s.cs.lns == nil {
		return
	}
	for n, l := range s.cs.lns {
		p := c.Ports[n]
		if p != nil && p.TLS == l.tls {
			continue
		}
		delete(s.cs.lns, n)
		l.ln.Close()
		go shutdownServer(l.srv, 30*time.Second)
		s.logf("port %d closed", n)
		if p != nil {
			ln, err := net.Listen("tcp", fmt.Sprintf(":%d", n))
			if err != nil {
				s.logf("port %d couldn't be reopened for its new protocol: %v", n, err)
				continue
			}
			opened[n] = ln
		}
	}
	for n, ln := range opened {
		s.serveOn(n, c.Ports[n].TLS, ln)
	}
}

// clientServer is the http.Server of a client-facing port. OPTIONS * comes
// to BareProxy's handler too, so it leaves a record like any other request.
func clientServer(h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second,
		MaxHeaderBytes: 32 << 10, ErrorLog: log.New(io.Discard, "", 0), DisableGeneralOptionsHandler: true}
}

func (s *Server) serveOn(port int, isTLS bool, ln net.Listener) {
	srv := clientServer(s.Handler(port, isTLS))
	if isTLS {
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: s.certFor(port), NextProtos: []string{"h2", "http/1.1", acme.ALPNProto}}
		go srv.ServeTLS(ln, "", "")
	} else {
		go srv.Serve(ln)
	}
	s.cs.lns[port] = &listener{srv: srv, ln: ln, tls: isTLS}
}

func shutdownServer(srv *http.Server, grace time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if srv.Shutdown(ctx) != nil {
		srv.Close()
	}
}

// closeListeners stops every listener, letting requests in flight finish.
func (s *Server) closeListeners() {
	s.mu.Lock()
	lns, admin := s.cs.lns, s.cs.admin
	s.cs.lns, s.cs.admin = map[int]*listener{}, nil
	s.mu.Unlock()
	if admin != nil {
		admin.Close()
	}
	var wg sync.WaitGroup
	for _, l := range lns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			shutdownServer(l.srv, 30*time.Second)
		}()
	}
	wg.Wait()
}

// drain takes backends that the new version dropped out of service. New
// requests can't reach them, since the new version doesn't have them.
// Requests that started on the old version still may, even for their first
// try, and their connections close after the pool's drain time (30 s by
// default).
func (s *Server) drain(old, rt *Runtime, conns map[string]*connSet) {
	for key, b := range old.backends {
		if _, kept := rt.backends[key]; kept {
			continue
		}
		d := 30 * time.Second
		ps := rt.Cfg.Pools[b.Pool] // the new setting, if the pool is still there
		if ps == nil {
			ps = old.Cfg.Pools[b.Pool]
		}
		if ps.Drain > 0 {
			d = ps.Drain
		}
		set, pool, addr := conns[b.Pool], b.Pool, b.Spec.Addr
		s.logf("pool %s: %s removed, draining for %s", pool, addr, d)
		time.AfterFunc(d, func() {
			if n := set.close(addr); n > 0 {
				s.logf("pool %s: %s drained, %d connections closed", pool, addr, n)
			}
		})
	}
}

// connSet tracks one pool transport's open connections by backend address,
// so a drained backend's connections can be closed.
type connSet struct {
	mu    sync.Mutex
	conns map[*trackedConn]string
}

type trackedConn struct {
	net.Conn
	set *connSet
}

func (c *trackedConn) Close() error {
	c.set.mu.Lock()
	delete(c.set.conns, c)
	c.set.mu.Unlock()
	return c.Conn.Close()
}

func (cs *connSet) close(addr string) int {
	if cs == nil {
		return 0
	}
	var list []*trackedConn
	cs.mu.Lock()
	for c, a := range cs.conns {
		if a == addr {
			list = append(list, c)
		}
	}
	cs.mu.Unlock()
	for _, c := range list {
		c.Close()
	}
	return len(list)
}

// trackConns makes each pool of a new runtime record its connections. It
// runs before the runtime goes live.
func trackConns(rt *Runtime) map[string]*connSet {
	out := map[string]*connSet{}
	for name, p := range rt.Pools {
		set := &connSet{conns: map[*trackedConn]string{}}
		dial := p.transport.DialContext
		p.transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			tc := &trackedConn{Conn: c, set: set}
			set.mu.Lock()
			set.conns[tc] = addr
			set.mu.Unlock()
			return tc, nil
		}
		out[name] = set
	}
	return out
}
