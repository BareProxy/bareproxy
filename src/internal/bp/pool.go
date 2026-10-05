// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Backend is one backend's live state. It carries over a reload that keeps
// the backend, so health and counters aren't reset.
type Backend struct {
	Spec     BackendSpec
	Pool     string
	key      string
	health   *HealthSpec
	inflight atomic.Int64

	mu      sync.Mutex
	state   string // unknown, up or down
	since   time.Time
	fails   int
	passes  int
	reason  string
	trialAt time.Time

	stop   context.CancelFunc
	events func(string)
}

// State is a snapshot of a backend.
type State struct {
	Addr     string
	State    string
	Since    time.Time
	Fails    int
	Reason   string
	InFlight int64
	Checked  bool
}

// Snapshot returns the backend's current state.
func (b *Backend) Snapshot() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return State{Addr: b.Spec.Addr, State: b.state, Since: b.since, Fails: b.fails,
		Reason: b.reason, InFlight: b.inflight.Load(), Checked: b.health != nil}
}

func describeState(st State) string {
	switch st.State {
	case "up":
		return fmt.Sprintf("up, %d in flight", st.InFlight)
	case "down":
		what := "failed connections"
		if st.Checked {
			what = "failed checks"
		}
		s := fmt.Sprintf("down since %s, %d %s", st.Since.UTC().Format("15:04:05"), st.Fails, what)
		if st.Reason != "" {
			s += " (" + st.Reason + ")"
		}
		return s
	}
	return "waiting for its first health check"
}

func (b *Backend) event(s string) {
	if b.events != nil {
		b.events(s)
	}
}

// usable reports whether the backend may take a request now. A down backend
// in a pool without health checks gets one trial request every 10 seconds.
func (b *Backend) usable(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state == "up" || (b.state == "down" && b.health == nil && !now.Before(b.trialAt))
}

func (b *Backend) claimTrial(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == "down" {
		b.trialAt = now.Add(10 * time.Second)
	}
}

// connFailed counts a failed connection on live traffic. It only changes
// state in pools without health checks; with checks, the checks decide.
func (b *Backend) connFailed(why string) {
	if b.health != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails++
	b.reason = why
	now := time.Now()
	switch {
	case b.state == "up" && b.fails >= 3:
		b.state, b.since, b.trialAt = "down", now, now.Add(10*time.Second)
		b.event(fmt.Sprintf("%s down after 3 failed connections (%s)", b.Spec.Addr, why))
	case b.state == "down":
		b.trialAt = now.Add(10 * time.Second)
	}
}

func (b *Backend) connOK() {
	if b.health != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails = 0
	if b.state == "down" {
		b.state, b.since, b.reason = "up", time.Now(), ""
		b.event(b.Spec.Addr + " up again after a trial request")
	}
}

// checkResult applies one health check: down after 3 failures in a row,
// up after 2 passes, and a new backend is up after its first pass.
func (b *Backend) checkResult(ok bool, why string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if ok {
		b.passes++
		b.fails = 0
		if b.state == "unknown" || (b.state == "down" && b.passes >= 2) {
			b.state, b.since, b.reason = "up", now, ""
			b.event(b.Spec.Addr + " up (health check passed)")
		}
		return
	}
	b.fails++
	b.passes = 0
	b.reason = why
	if b.state != "down" && b.fails >= 3 {
		b.state, b.since = "down", now
		b.event(fmt.Sprintf("%s down after 3 failed checks (%s)", b.Spec.Addr, why))
	}
}

func (b *Backend) runChecks(ctx context.Context) {
	h := b.health
	client := &http.Client{
		Transport: &http.Transport{
			DialContext:       (&net.Dialer{Timeout: h.Timeout}).DialContext,
			TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
			DisableKeepAlives: true,
		},
		Timeout:       h.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	t := time.NewTicker(h.Every)
	defer t.Stop()
	for {
		b.checkOnce(ctx, client)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (b *Backend) checkOnce(ctx context.Context, client *http.Client) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, schemeName(b.Spec.HTTPS)+"://"+b.Spec.Addr+b.health.Path, nil)
	if err != nil {
		b.checkResult(false, err.Error())
		return
	}
	req.Header.Set("User-Agent", "BareProxy health check")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			b.checkResult(false, shortErr(err))
		}
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode < b.health.Lo || resp.StatusCode > b.health.Hi {
		b.checkResult(false, fmt.Sprintf("status %d", resp.StatusCode))
		return
	}
	b.checkResult(true, "")
}

func shortErr(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connect refused"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	}
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return s
}

// Pool is a pool's live state for one config version.
type Pool struct {
	Spec      *PoolSpec
	Backends  []*Backend
	rr        atomic.Uint64
	transport *http.Transport
}

// pick chooses the usable backend with the fewest requests in flight; ties
// go round robin. With peek set it changes nothing, for explain.
func (p *Pool) pick(skip map[*Backend]bool, peek bool) *Backend {
	n := uint64(len(p.Backends))
	if n == 0 {
		return nil
	}
	start := p.rr.Load()
	if !peek {
		start = p.rr.Add(1) - 1
	}
	now := time.Now()
	var best *Backend
	var bestN int64
	for i := uint64(0); i < n; i++ {
		b := p.Backends[(start+i)%n]
		if skip[b] || !b.usable(now) {
			continue
		}
		if f := b.inflight.Load(); best == nil || f < bestN {
			best, bestN = b, f
		}
	}
	if best != nil && !peek {
		best.claimTrial(now)
	}
	return best
}

// upCount returns how many backends are up.
func (p *Pool) upCount() int {
	n := 0
	for _, b := range p.Backends {
		if b.Snapshot().State == "up" {
			n++
		}
	}
	return n
}

// dialError marks a failure to open a connection, the one case that is
// safe to retry on another backend.
type dialError struct{ err error }

func (e *dialError) Error() string { return e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

func newTransport(ps *PoolSpec) *http.Transport {
	d := &net.Dialer{Timeout: ps.ConnectTimeout, KeepAlive: 30 * time.Second}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, &dialError{err}
			}
			return c, nil
		},
		ResponseHeaderTimeout:  ps.ResponseTimeout,
		MaxResponseHeaderBytes: 64 << 10,
		MaxIdleConnsPerHost:    64,
		IdleConnTimeout:        90 * time.Second,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
	}
}
