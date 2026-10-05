// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"cmp"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

var errNoBackend = errors.New("no backend is up")

// replayBody lets a request body be sent again after a failed connection,
// as long as nothing was read from it.
type replayBody struct {
	r    io.ReadCloser
	used bool
}

func (b *replayBody) Read(p []byte) (int, error) {
	b.used = true
	return b.r.Read(p)
}

func (b *replayBody) Close() error { return nil }

type doneBody struct {
	io.ReadCloser
	once sync.Once
	done func()
}

func (d *doneBody) Close() error {
	err := d.ReadCloser.Close()
	d.once.Do(d.done)
	return err
}

// poolTransport picks a backend for each attempt and retries on another
// backend only when no connection could be opened.
type poolTransport struct {
	pool *Pool
	rec  *Record
}

func (t *poolTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var rb *replayBody
	if req.Body != nil && req.Body != http.NoBody {
		rb = &replayBody{r: req.Body}
		req.Body = rb
	}
	tried := map[*Backend]bool{}
	var lastErr error
	for attempt := 0; ; attempt++ {
		b := t.pool.pick(tried, false)
		if b == nil {
			if lastErr == nil {
				lastErr = errNoBackend
			}
			return nil, lastErr
		}
		tried[b] = true
		out := req.Clone(req.Context())
		out.URL.Scheme, out.URL.Host = b.scheme(), b.Spec.Addr
		b.inflight.Add(1)
		start := time.Now()
		resp, err := t.pool.transport.RoundTrip(out)
		ms := msSince(start)
		if err != nil {
			b.inflight.Add(-1)
			var de *dialError
			dial := errors.As(err, &de)
			if dial {
				b.connFailed(shortErr(err))
			}
			t.rec.Attempts = append(t.rec.Attempts, Attempt{Backend: b.Spec.Addr, Error: shortErr(err), MS: ms})
			lastErr = err
			if dial && attempt < t.pool.Spec.Retries && (rb == nil || !rb.used) && req.Context().Err() == nil {
				continue
			}
			return nil, err
		}
		b.connOK()
		t.rec.Attempts = append(t.rec.Attempts, Attempt{Backend: b.Spec.Addr, Status: resp.StatusCode, FirstByteMS: ms})
		if resp.StatusCode == http.StatusSwitchingProtocols {
			b.inflight.Add(-1) // the connection now belongs to the WebSocket
			return resp, nil
		}
		resp.Body = &doneBody{ReadCloser: resp.Body, done: func() { b.inflight.Add(-1) }}
		return resp, nil
	}
}

// bodyReader remembers why reading the client's request body failed, so a
// broken body is answered as the client's mistake and not as a bad backend.
type bodyReader struct {
	io.ReadCloser
	err error
}

func (b *bodyReader) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF && b.err == nil {
		b.err = err
	}
	return n, err
}

// isWebSocketUpgrade says whether a request asks to switch to WebSocket and
// to nothing else.
func isWebSocketUpgrade(h http.Header) bool {
	if !hasToken(h.Values("Connection"), "upgrade") {
		return false
	}
	up := h.Values("Upgrade")
	return len(up) == 1 && strings.EqualFold(strings.TrimSpace(up[0]), "websocket")
}

func hasToken(values []string, token string) bool {
	for _, v := range values {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// websocketOnly lets WebSocket upgrades through and strips every other
// protocol switch (h2c for one) from the request, so the backend gets a plain
// request. A tunnel to any other protocol would carry requests that no rule
// of ours ever sees.
func websocketOnly(h http.Header) {
	if isWebSocketUpgrade(h) {
		return
	}
	h.Del("Upgrade")
	h.Del("HTTP2-Settings")
	var keep []string
	for _, v := range h.Values("Connection") {
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); t != "" && !strings.EqualFold(t, "upgrade") && !strings.EqualFold(t, "http2-settings") {
				keep = append(keep, t)
			}
		}
	}
	if len(keep) == 0 {
		h.Del("Connection")
	} else {
		h.Set("Connection", strings.Join(keep, ", "))
	}
}

func (s *Server) proxy(w *respWriter, r *http.Request, rt *Runtime, site *Site, route *Route, norm string, rec *Record) {
	pool := rt.Pools[route.Act.Pool]
	upstream := norm
	if route.Act.Strip {
		upstream = stripPrefix(norm, route.Path)
	}
	rec.Upstream = upstream
	rec.Pool, rec.PoolLine, rec.PoolSize = pool.Spec.Name, pool.Spec.Line, len(pool.Backends)
	for _, b := range pool.Backends {
		st := b.Snapshot()
		if st.State == "up" {
			rec.PoolUp++
			continue
		}
		rec.Skipped = append(rec.Skipped, Skip{Backend: st.Addr, State: st.State, Why: describeState(st)})
	}
	if r.ContentLength > site.BodyLimit {
		rec.Outcome = "too_large"
		s.plain(w, r, rec, http.StatusRequestEntityTooLarge, "The request body is larger than this site allows")
		return
	}
	body := &bodyReader{}
	if r.Body != nil && r.Body != http.NoBody {
		body.ReadCloser = http.MaxBytesReader(w, r.Body, site.BodyLimit)
		r.Body = body
	}
	websocketOnly(r.Header)
	decoded, err := url.PathUnescape(upstream)
	if err != nil {
		decoded = upstream
	}
	host := cmp.Or(pool.Spec.HostHeader, r.Host)
	rec.Outcome = "ok"
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Path, pr.Out.URL.RawPath = decoded, upstream
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = host
			pr.SetXForwarded()
			pr.Out.Header.Del("Forwarded")
			pr.Out.Header.Set("BareProxy-Id", rec.ID)
			setTraceparent(pr.Out.Header, pr.In.Header, rec)
		},
		Transport: &poolTransport{pool: pool, rec: rec},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("BareProxy-Id")
			return nil
		},
		ErrorHandler: func(_ http.ResponseWriter, _ *http.Request, err error) {
			var mb *http.MaxBytesError
			if body.err != nil && !errors.As(body.err, &mb) && r.Context().Err() == nil {
				// The client's body couldn't be read (a bad chunk size, say): its fault, not the backend's.
				rec.Outcome, rec.Reason = "bad_request", shortErr(body.err)
				s.plain(w, r, rec, http.StatusBadRequest, "Bad request: the request body is broken")
				return
			}
			s.proxyError(w, r, rec, err)
		},
		ErrorLog: log.New(io.Discard, "", 0),
	}
	rp.ServeHTTP(w, r)
}

func (s *Server) proxyError(w *respWriter, r *http.Request, rec *Record, err error) {
	var de *dialError
	var mb *http.MaxBytesError
	rec.Reason = shortErr(err)
	switch {
	case errors.Is(err, errNoBackend):
		rec.Outcome, rec.Reason = "no_backend", "no backend in the pool is up"
		s.plain(w, r, rec, http.StatusServiceUnavailable, "No backend is up")
	case errors.As(err, &mb):
		rec.Outcome = "too_large"
		s.plain(w, r, rec, http.StatusRequestEntityTooLarge, "The request body is larger than this site allows")
	case r.Context().Err() != nil:
		rec.Outcome = "client_gone"
	case errors.As(err, &de):
		rec.Outcome = "connect_failed"
		s.plain(w, r, rec, http.StatusBadGateway, "Bad gateway: no backend connection could be opened")
	case isTimeout(err):
		rec.Outcome = "timeout"
		s.plain(w, r, rec, http.StatusGatewayTimeout, "Gateway timeout: the backend didn't answer in time")
	default:
		rec.Outcome = "bad_response"
		s.plain(w, r, rec, http.StatusBadGateway, "Bad gateway: the backend's response was broken")
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout() || strings.Contains(err.Error(), "timeout awaiting response headers")
}
