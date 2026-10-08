// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/sys"
)

// Settings is what the config says about a running plugin.
type Settings struct {
	Name      string
	Config    []byte        // handed to proxy_on_configure
	Timeout   time.Duration // the longest one call into the plugin may run
	Pause     time.Duration // the longest a plugin may keep a request waiting
	Instances int           // copies of the module, each running one call at a time
	AllowHTTP []string      // host:port addresses it may call
	Read      []*os.Root    // folders it may read, in order
	StoreDir  string        // its key-value store, or "" for none
	StoreMax  int64
	BodyMax   int64 // the largest request or response body it is handed
	Logf      func(string, ...any)
}

// Plugin is a running plugin: a few instances of one module, with what the
// instances share (shared data, metrics, the store).
type Plugin struct {
	S        Settings
	mod      *Module
	mu       sync.Mutex // guards insts, data, metrics, queued
	insts    []*instance
	data     map[string]shared
	dataSize int
	mets     []*metric
	next     atomic.Uint32
	calls    atomic.Uint32
	store    *store
	client   *http.Client
	closed   atomic.Bool
	done     chan struct{} // closed by Close
	starts   []time.Time   // instances started after a failure, in the last minute
	inCall   atomic.Int32  // outgoing calls in flight
	logs     rateLimit
}

type shared struct {
	val []byte
	cas uint32
}

type metric struct {
	name string
	kind uint32
	val  atomic.Int64
}

// instance is one copy of the module. Calls into it hold mu, so a plugin's
// code never runs twice at once in one instance.
type instance struct {
	p         *Plugin
	mu        sync.Mutex
	mod       api.Module
	nextID    uint32
	streams   map[uint32]*Stream
	effective uint32 // the context host functions act on
	broken    error
	tick      chan struct{} // closed to stop the tick loop
	callH     [][2]string   // the response of an outgoing call, during proxy_on_http_call_response
	callB     []byte
	callBack  bool                    // a call response is being handed over
	limit     time.Duration           // the time limit for calls while starting; 0 means Settings.Timeout
	starting  bool                    // a failure while starting doesn't call for a replacement
	fns       map[string]api.Function // exported functions, made once: making one allocates its stack
	replacing atomic.Bool
}

const rootID = 1

// Start runs a compiled module: it makes the instances, and in each calls
// proxy_on_vm_start and proxy_on_configure. It takes its own reference to m.
func Start(m *Module, s Settings) (*Plugin, error) {
	s.Instances = max(s.Instances, 1)
	if s.Timeout <= 0 {
		s.Timeout = 5 * time.Millisecond
	}
	if s.Pause <= 0 {
		s.Pause = 30 * time.Second
	}
	if s.Logf == nil {
		s.Logf = func(string, ...any) {}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	p := &Plugin{S: s, mod: m.Retain(), data: map[string]shared{}, done: make(chan struct{}),
		client: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if s.StoreDir != "" {
		st, err := openStore(s.StoreDir, s.StoreMax)
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("can't open its store: %v", err)
		}
		p.store = st
	}
	for range s.Instances {
		in, err := p.newInstance()
		if err != nil {
			p.Close()
			return nil, err
		}
		p.insts = append(p.insts, in)
	}
	return p, nil
}

// Module returns the module the plugin runs.
func (p *Plugin) Module() *Module { return p.mod }

// Close stops the plugin. Requests still using it get errors.
func (p *Plugin) Close() {
	if p.closed.Swap(true) {
		return
	}
	close(p.done)
	for _, in := range p.instances() {
		in.mu.Lock()
		in.fail(errors.New("the plugin was stopped"))
		in.mu.Unlock()
	}
	for _, r := range p.S.Read {
		r.Close()
	}
	p.mod.Release()
}

// instances returns a copy of the instance list.
func (p *Plugin) instances() []*instance {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.insts)
}

type ctxKey struct{}

func (p *Plugin) newInstance() (*instance, error) {
	in := &instance{p: p, streams: map[uint32]*Stream{}, nextID: rootID, fns: map[string]api.Function{}}
	logw := &logWriter{p: p}
	// No real sleep: a plugin that sleeps would hold its instance past the
	// time limit, so WASI's sleep returns at once.
	cfg := wazero.NewModuleConfig().WithName("").WithStartFunctions().WithSysWalltime().WithSysNanotime().
		WithRandSource(rand.Reader).WithStdout(logw).WithStderr(logw)
	ctx := context.WithValue(context.Background(), ctxKey{}, in)
	mod, err := p.mod.rt.InstantiateModule(ctx, p.mod.cm, cfg)
	if err != nil {
		return nil, fmt.Errorf("it didn't start: %v", err)
	}
	in.mod = mod
	in.mu.Lock()
	defer in.mu.Unlock()
	// Starting a runtime, such as Go's, takes longer than a request's call.
	in.limit, in.starting = max(p.S.Timeout, 10*time.Second), true
	defer func() { in.limit, in.starting = 0, false }()
	if p.mod.start != "" {
		if _, err := in.call(p.mod.start); err != nil {
			return nil, fmt.Errorf("its %s %v", p.mod.start, err)
		}
	}
	if _, err := in.call("proxy_on_context_create", rootID, 0); err != nil {
		return nil, fmt.Errorf("its root context %v", err)
	}
	if p.mod.Has("proxy_on_vm_start") {
		ok, err := in.call("proxy_on_vm_start", rootID, 0)
		if err == nil && ok == 0 {
			err = errors.New("returned false")
		}
		if err != nil {
			return nil, fmt.Errorf("proxy_on_vm_start %v", err)
		}
	}
	if p.mod.Has("proxy_on_configure") {
		ok, err := in.call("proxy_on_configure", rootID, uint64(len(p.S.Config)))
		if err == nil && ok == 0 {
			err = errors.New("returned false: the plugin refused its config")
		}
		if err != nil {
			return nil, fmt.Errorf("proxy_on_configure %v", err)
		}
	}
	return in, nil
}

// call runs one exported function. The caller holds in.mu. A trap or a call
// that runs past the time limit breaks the instance; another replaces it.
func (in *instance) call(name string, args ...uint64) (uint64, error) {
	if in.broken != nil {
		return 0, in.broken
	}
	f := in.fn(name)
	if f == nil {
		return 0, nil
	}
	limit := cmp.Or(in.limit, in.p.S.Timeout)
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), ctxKey{}, in), limit)
	defer cancel()
	res, err := f.Call(ctx, args...)
	if err != nil {
		var ee *sys.ExitError
		switch {
		case errors.As(err, &ee) && ee.ExitCode() == sys.ExitCodeDeadlineExceeded:
			err = fmt.Errorf("ran past its time limit of %s", limit)
		case errors.As(err, &ee):
			err = fmt.Errorf("exited with code %d", ee.ExitCode())
		default:
			err = fmt.Errorf("crashed: %s", firstLine(err.Error()))
		}
		in.fail(err)
		if !in.starting {
			in.p.replace(in)
		}
		return 0, err
	}
	if len(res) == 0 {
		return 0, nil
	}
	return res[0], nil
}

// fn returns an exported function, or nil.
func (in *instance) fn(name string) api.Function {
	f, ok := in.fns[name]
	if !ok {
		f = in.mod.ExportedFunction(name)
		in.fns[name] = f
	}
	return f
}

// fail marks the instance broken and wakes every request waiting on it.
func (in *instance) fail(err error) {
	if in.broken != nil {
		return
	}
	in.broken = err
	if in.tick != nil {
		close(in.tick)
		in.tick = nil
	}
	for _, s := range in.streams {
		s.wake()
	}
	go in.mod.Close(context.Background())
}

// replace starts a new instance in place of a broken one, in the
// background. After many failures in a minute, and while new instances fail
// to start, it waits longer between tries.
func (p *Plugin) replace(old *instance) {
	if p.closed.Load() || old.replacing.Swap(true) {
		return
	}
	p.S.Logf("plugin %s: an instance stopped (%v); starting another", p.S.Name, old.broken)
	p.mu.Lock()
	now := time.Now()
	p.starts = append(slices.DeleteFunc(p.starts, func(t time.Time) bool { return now.Sub(t) > time.Minute }), now)
	var wait time.Duration
	if len(p.starts) > 10 {
		wait = 10 * time.Second
		p.S.Logf("plugin %s: %d failures in the last minute, so the next instance starts in %s", p.S.Name, len(p.starts), wait)
	}
	p.mu.Unlock()
	go func() {
		for {
			t := time.NewTimer(wait)
			select {
			case <-p.done:
				t.Stop()
				return
			case <-t.C:
			}
			in, err := p.newInstance()
			if err == nil {
				p.mu.Lock()
				defer p.mu.Unlock()
				if i := slices.Index(p.insts, old); i >= 0 && !p.closed.Load() {
					p.insts[i] = in
					return
				}
				in.mu.Lock()
				in.fail(errors.New("not needed"))
				in.mu.Unlock()
				return
			}
			wait = min(max(2*wait, time.Second), time.Minute)
			p.S.Logf("plugin %s: the new instance didn't start (%v); trying again in %s", p.S.Name, err, wait)
		}
	}()
}

// setTick starts, changes or stops the instance's timer. The caller holds in.mu.
func (in *instance) setTick(ms uint32) {
	if in.tick != nil {
		close(in.tick)
		in.tick = nil
	}
	if ms == 0 || !in.p.mod.Has("proxy_on_tick") {
		return
	}
	stop := make(chan struct{})
	in.tick = stop
	go func() {
		t := time.NewTicker(time.Duration(ms) * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				in.mu.Lock()
				in.effective = rootID
				in.call("proxy_on_tick", rootID)
				in.mu.Unlock()
			}
		}
	}()
}

// logWriter sends what a plugin writes to stdout and stderr to the log, a
// line at a time.
type logWriter struct {
	p   *Plugin
	mu  sync.Mutex
	buf []byte
}

func (w *logWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, b...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			if len(w.buf) > 4096 {
				i = len(w.buf)
			} else {
				return len(b), nil
			}
		}
		w.p.logf("%s", strings.TrimRight(string(w.buf[:i]), "\r\n"))
		w.buf = w.buf[min(i+1, len(w.buf)):]
	}
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

// Health returns how many of the plugin's instances are working.
func (p *Plugin) Health() (working, all int) {
	insts := p.instances()
	for _, in := range insts {
		in.mu.Lock()
		if in.broken == nil {
			working++
		}
		in.mu.Unlock()
	}
	return working, len(insts)
}

// rateLimit keeps a plugin from flooding the log: 50 lines a second, and a
// count of what was dropped.
type rateLimit struct {
	mu      sync.Mutex
	since   time.Time
	n, lost int
}

// logf logs a line from the plugin, within the rate limit.
func (p *Plugin) logf(f string, a ...any) {
	r := &p.logs
	r.mu.Lock()
	now := time.Now()
	if now.Sub(r.since) >= time.Second {
		if r.lost > 0 {
			p.S.Logf("plugin %s: %d log lines dropped (more than 50 a second)", p.S.Name, r.lost)
		}
		r.since, r.n, r.lost = now, 0, 0
	}
	r.n++
	ok := r.n <= 50
	if !ok {
		r.lost++
	}
	r.mu.Unlock()
	if ok {
		p.S.Logf("plugin %s: "+f, append([]any{p.S.Name}, a...)...)
	}
}
