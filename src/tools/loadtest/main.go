// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

// Command loadtest sends a fixed rate of requests at a server through three
// kinds of client at once: HTTP/1.1 with keep-alive, HTTP/2 over TLS, and
// WebSocket connections that send small messages and read them back for the
// whole run. It counts what was sent, answered and failed (by kind and
// reason), the connections that were reset, the WebSocket messages echoed and
// lost, and the latency of each kind, then prints one JSON summary line.
//
// The load is open loop. Requests are scheduled at fixed times and a
// request's latency counts from its scheduled time, so a slow server shows up
// as latency and not as a lower rate. Exit status: 0 when nothing failed, no
// connection was reset and no WebSocket message was lost; 1 otherwise.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// config is what the command line sets.
type config struct {
	http1, http2 string        // base URLs, empty to skip a kind
	ws           []string      // ws:// and wss:// URLs; connections take them in turn
	connect      string        // address to dial instead of the URL's host
	cacert       string        // PEM file that signs the https and wss certificates
	rate         float64       // requests per second, all kinds together
	split        [3]float64    // share of the rate for http1, http2 and ws
	duration     time.Duration // how long requests are sent
	grace        time.Duration // how long to wait for the last answers
	timeout      time.Duration // per request
	targets      []target
	h1Conns      int // HTTP/1.1 keep-alive connections
	h2Conns      int // HTTP/2 connections
	h2Streams    int // requests in flight at most on each HTTP/2 connection
	wsConns      int
}

// target is a path to request and a string its answer must contain.
type target struct{ path, want string }

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(2)
	}
	sum := run(cfg, os.Stdout)
	line, _ := json.Marshal(sum)
	fmt.Println(string(line))
	if !sum.Pass {
		os.Exit(1)
	}
}

func parseFlags(args []string) (c config, err error) {
	fs := flag.NewFlagSet("loadtest", flag.ContinueOnError)
	var ws, paths, split string
	fs.StringVar(&c.http1, "http1", "", "base URL for the HTTP/1.1 client, such as http://plain.test:8080")
	fs.StringVar(&c.http2, "http2", "", "base URL for the HTTP/2 client, such as https://secure.test:8443")
	fs.StringVar(&ws, "ws", "", "comma-separated ws:// or wss:// URLs for the WebSocket clients")
	fs.StringVar(&c.connect, "connect", "", "dial this address for every connection, whatever the URL's host")
	fs.StringVar(&c.cacert, "cacert", "", "PEM file of the CA (or self-signed certificate) for https and wss")
	fs.Float64Var(&c.rate, "rate", 2000, "requests (and WebSocket messages) per second, all kinds together")
	fs.StringVar(&split, "split", "40,40,20", "percent of the rate for HTTP/1.1, HTTP/2 and WebSocket")
	fs.DurationVar(&c.duration, "duration", 60*time.Second, "how long to send")
	fs.DurationVar(&c.grace, "grace", 3*time.Second, "how long to wait for the last answers")
	fs.DurationVar(&c.timeout, "timeout", 10*time.Second, "limit for one request")
	fs.StringVar(&paths, "paths", "/hello=hello,/api/x=backend,/moved/x=backend", "PATH=TEXT pairs: the paths HTTP clients ask for in turn, and a text each answer must contain")
	fs.IntVar(&c.h1Conns, "h1-conns", 16, "keep-alive connections of the HTTP/1.1 client")
	fs.IntVar(&c.h2Conns, "h2-conns", 4, "HTTP/2 connections")
	fs.IntVar(&c.h2Streams, "h2-streams", 8, "requests in flight at most on each HTTP/2 connection")
	fs.IntVar(&c.wsConns, "ws-conns", 20, "WebSocket connections")
	if err = fs.Parse(args); err != nil {
		return
	}
	if c.http1 == "" && c.http2 == "" && ws == "" {
		return c, errors.New("give at least one of -http1, -http2 and -ws")
	}
	if ws != "" {
		c.ws = strings.Split(ws, ",")
	}
	parts := strings.Split(split, ",")
	if len(parts) != 3 {
		return c, errors.New("-split takes three numbers, such as 40,40,20")
	}
	total := 0.0
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || v < 0 {
			return c, fmt.Errorf("bad -split value %q", p)
		}
		c.split[i], total = v, total+v
	}
	if total == 0 || c.rate <= 0 || c.duration <= 0 {
		return c, errors.New("-rate, -duration and -split must be above zero")
	}
	for i := range c.split {
		c.split[i] = c.rate * c.split[i] / total
	}
	if c.targets, err = parseTargets(paths); err != nil {
		return c, err
	}
	return
}

func parseTargets(s string) ([]target, error) {
	var out []target
	for _, p := range strings.Split(s, ",") {
		path, want, ok := strings.Cut(p, "=")
		if !ok || !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("bad -paths entry %q: want /path=text", p)
		}
		out = append(out, target{path, want})
	}
	return out, nil
}

// stats is what one kind of client counted.
type stats struct {
	name  string
	start time.Time
	rate  float64
	sent  atomic.Int64
	// Connections: opened, reset (ECONNRESET or EPIPE on an open connection)
	// and closed by the server (EOF while we hadn't closed it).
	opened, reset, closedBySrv atomic.Int64
	lastOpened                 atomic.Int64 // nanoseconds after the start of the load

	mu       sync.Mutex
	answered int64
	lost     int64 // WebSocket messages sent and never echoed
	fails    map[string]int64
	samples  []sample
	lat      []time.Duration
	resets   int64 // failures that mean a reset: counted with the connection resets
}

// sample is one failure, kept so a report can say when and how it happened.
type sample struct {
	T      float64 `json:"t_s"`
	Kind   string  `json:"kind"`
	Reason string  `json:"reason"`
	Detail string  `json:"detail"`
}

func (s *stats) ok(d time.Duration) {
	s.mu.Lock()
	s.answered++
	s.lat = append(s.lat, d)
	s.mu.Unlock()
}

// failN records n failures for one reason; detail is kept for the first few.
func (s *stats) failN(reason, detail string, n int64, reset bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails[reason] += n
	if reset {
		s.resets += n
	}
	if len(s.samples) < 5 {
		s.samples = append(s.samples, sample{time.Since(s.start).Seconds(), s.name, reason, detail})
	}
}

// failErr records one failed request, named by what went wrong.
func (s *stats) failErr(prefix string, err error) {
	reason, reset := classify(err)
	if prefix != "" {
		reason = prefix + ": " + reason
	}
	s.failN(reason, err.Error(), 1, reset)
}

// classify names an error and says whether it counts as a reset beyond the
// ones the connection wrapper sees: a dropped connection (EOF) or an HTTP/2
// stream or connection error.
func classify(err error) (reason string, reset bool) {
	msg := err.Error()
	var ne net.Error
	switch {
	case errors.Is(err, syscall.ECONNRESET): // the connection wrapper counts these
		return "connection reset", false
	case errors.Is(err, syscall.EPIPE):
		return "broken pipe", false
	case errors.Is(err, errClosedByServer):
		return "closed by the server", true
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused", false
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout():
		return "timeout", false
	case strings.Contains(msg, "stream error") || strings.Contains(msg, "GOAWAY") || strings.Contains(msg, "RST_STREAM"):
		return "http2 stream or connection error", true
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(msg, "EOF"):
		return "connection closed with no answer (EOF)", true
	}
	if len(msg) > 80 {
		msg = msg[:80]
	}
	return "other: " + msg, false
}

// countedConn counts how a connection ends, whatever the transport above it
// does about it (it may quietly retry, which would hide a reset).
type countedConn struct {
	net.Conn
	s      *stats
	ours   atomic.Bool // we closed it
	counts atomic.Bool // already counted
}

func (c *countedConn) note(err error) {
	if err == nil || c.ours.Load() {
		return
	}
	switch {
	case errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE):
		if c.counts.CompareAndSwap(false, true) {
			c.s.reset.Add(1)
		}
	case errors.Is(err, io.EOF):
		if c.counts.CompareAndSwap(false, true) {
			c.s.closedBySrv.Add(1)
		}
	}
}

func (c *countedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.note(err)
	return n, err
}

func (c *countedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.note(err)
	return n, err
}

func (c *countedConn) Close() error {
	c.ours.Store(true)
	return c.Conn.Close()
}

// loader holds what every client shares.
type loader struct {
	config
	roots      *x509.CertPool
	start, end time.Time
}

func (l *loader) dial(s *stats, addr string) (*countedConn, error) {
	if l.connect != "" {
		_, port, _ := net.SplitHostPort(addr)
		addr = net.JoinHostPort(l.connect, port)
	}
	c, err := (&net.Dialer{Timeout: 5 * time.Second}).Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	s.opened.Add(1)
	s.lastOpened.Store(int64(max(0, time.Since(l.start)))) // the WebSocket clients dial a little before the start
	return &countedConn{Conn: c, s: s}, nil
}

// pace calls fire at fixed times from start until end. A call that comes late
// is followed at once by the ones it missed, so the rate holds.
func pace(rate float64, start, end time.Time, fire func(i int, sched time.Time)) {
	gap := float64(time.Second) / rate
	for i := 0; ; i++ {
		sched := start.Add(time.Duration(float64(i) * gap))
		if !sched.Before(end) {
			return
		}
		if d := time.Until(sched); d > 0 {
			time.Sleep(d)
		}
		fire(i, sched)
	}
}

type job struct {
	i     int
	sched time.Time
}

// runHTTP sends GETs at the given rate over conns connections of its own,
// each with its own transport, so every one stays open through the whole run
// and gets its share of the requests in turn. On HTTP/2 each connection runs
// several requests at once.
func (l *loader) runHTTP(s *stats, base string, h2 bool, conns, perConn int) {
	u, err := url.Parse(base)
	if err != nil {
		s.failErr("bad URL", err)
		return
	}
	var wg sync.WaitGroup
	chans := make([]chan job, conns)
	for k := range chans {
		tr := &http.Transport{
			DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
				c, err := l.dial(s, addr)
				if err != nil {
					return nil, err
				}
				return c, nil
			},
			TLSClientConfig:     &tls.Config{RootCAs: l.roots, ServerName: u.Hostname()},
			ForceAttemptHTTP2:   h2,
			MaxIdleConnsPerHost: perConn,
			DisableCompression:  true,
			IdleConnTimeout:     90 * time.Second,
		}
		if !h2 {
			tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{} // no h2 on this client
		}
		client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		chans[k] = make(chan job, 1024)
		for range perConn {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range chans[k] {
					l.get(client, s, base, j, h2)
				}
			}()
		}
		defer tr.CloseIdleConnections()
	}
	pace(s.rate, l.start, l.end, func(i int, sched time.Time) {
		s.sent.Add(1)
		select {
		case chans[i%conns] <- job{i, sched}:
		default:
			s.failN("client queue full (the load tool fell behind)", "", 1, false)
		}
	})
	for _, ch := range chans {
		close(ch)
	}
	wg.Wait()
}

// oneLine shortens a response body to something that fits on a line of a log.
func oneLine(b []byte) string {
	s := strings.Join(strings.Fields(string(b[:min(len(b), 100)])), " ")
	return s
}

func (l *loader) get(c *http.Client, s *stats, base string, j job, h2 bool) {
	t := l.targets[j.i%len(l.targets)]
	ctx, cancel := context.WithTimeout(context.Background(), l.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+t.path, nil)
	if err != nil {
		s.failErr("bad request", err)
		return
	}
	resp, err := c.Do(req)
	if err != nil {
		s.failErr("", err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	switch {
	case err != nil:
		s.failErr("reading the answer", err)
	case resp.StatusCode != http.StatusOK:
		s.failN(fmt.Sprintf("status %d", resp.StatusCode), t.path+": "+oneLine(body), 1, false)
	case h2 != (resp.ProtoMajor == 2):
		s.failN("wrong protocol "+resp.Proto, t.path, 1, false)
	case !bytes.Contains(body, []byte(t.want)):
		s.failN("wrong body", t.path+": "+oneLine(body), 1, false)
	default:
		s.ok(time.Since(j.sched))
	}
}

// ---- WebSocket ----

// wsConn is a client connection after the handshake.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	cc   *countedConn
}

// wsDial opens a connection and does the opening handshake.
func (l *loader) wsDial(s *stats, rawurl string) (*wsConn, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, err
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"ws": "80", "wss": "443"}[u.Scheme]
	}
	cc, err := l.dial(s, net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return nil, err
	}
	var conn net.Conn = cc
	if u.Scheme == "wss" {
		conn = tls.Client(cc, &tls.Config{RootCAs: l.roots, ServerName: u.Hostname(), NextProtos: []string{"http/1.1"}})
	}
	fail := func(err error) (*wsConn, error) { cc.Close(); return nil, err }
	conn.SetDeadline(time.Now().Add(l.timeout))
	key := newKey()
	_, err = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		u.RequestURI(), u.Host, key)
	if err != nil {
		return fail(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		return fail(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(key) {
		return fail(fmt.Errorf("handshake answered %s", resp.Status))
	}
	conn.SetDeadline(time.Time{})
	return &wsConn{conn, br, cc}, nil
}

func payload(id, seq int) []byte {
	return fmt.Appendf(nil, "%d:%d:%s", id, seq, strings.Repeat("x", 40))
}

// runWS runs conns connections, each sending its share of the rate.
func (l *loader) runWS(s *stats) {
	gap := time.Duration(float64(time.Second) * float64(l.wsConns) / s.rate)
	var wg sync.WaitGroup
	for i := range l.wsConns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.wsClient(s, l.ws[i%len(l.ws)], i, gap)
		}()
	}
	wg.Wait()
}

// wsClient keeps one connection busy until the end of the run: it sends a
// message at each scheduled time and checks the echo against what it sent. A
// connection that breaks is counted, its unanswered messages are lost, and a
// new one takes over.
func (l *loader) wsClient(s *stats, rawurl string, id int, gap time.Duration) {
	next := l.start.Add(time.Duration(id) * gap / time.Duration(l.wsConns)) // spread the connections out
	seq := 0
	for time.Now().Before(l.end) {
		c, err := l.wsDial(s, rawurl)
		if err != nil {
			s.failErr("websocket handshake", err)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		var mu sync.Mutex
		pending := map[int]time.Time{}
		dead := make(chan error, 1)
		go func() { // the reader
			for {
				op, msg, err := readFrame(c.br)
				if err != nil {
					dead <- err
					return
				}
				switch op {
				case 0x1:
					var gid, gseq int
					n, _ := fmt.Sscanf(string(msg), "%d:%d:", &gid, &gseq)
					mu.Lock()
					t, ok := pending[gseq]
					delete(pending, gseq)
					mu.Unlock()
					if n != 2 || gid != id || !ok || !bytes.Equal(msg, payload(id, gseq)) {
						s.failN("websocket echo wrong or repeated", string(msg), 1, false)
					} else {
						s.ok(time.Since(t))
					}
				}
			}
		}()
		var failure error
	send:
		for time.Now().Before(l.end) {
			if d := time.Until(next); d > 0 {
				select {
				case <-time.After(d):
				case failure = <-dead:
					break send
				}
			}
			select {
			case failure = <-dead:
				break send
			default:
			}
			seq++
			mu.Lock()
			pending[seq] = next
			mu.Unlock()
			s.sent.Add(1)
			if failure = writeFrame(c.conn, 0x1, payload(id, seq)); failure != nil {
				break send
			}
			next = next.Add(gap)
		}
		if failure == nil { // the end: give the last echoes time to come back
			deadline := time.Now().Add(l.grace)
		wait:
			for time.Now().Before(deadline) {
				mu.Lock()
				n := len(pending)
				mu.Unlock()
				if n == 0 {
					break
				}
				select {
				case failure = <-dead:
					break wait
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		if failure == nil {
			writeFrame(c.conn, 0x8, nil) // a polite close
		}
		c.cc.Close()
		mu.Lock()
		lost := len(pending)
		mu.Unlock()
		if failure != nil {
			reason, reset := classify(failure)
			s.failN("websocket connection lost: "+reason, failure.Error(), 1, reset)
		}
		if lost > 0 {
			s.mu.Lock()
			s.lost += int64(lost)
			s.mu.Unlock()
			s.failN("websocket message sent, never echoed", fmt.Sprintf("connection %d, %d messages", id, lost), int64(lost), false)
		}
		if failure != nil {
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// ---- running and reporting ----

// kindSummary is one kind's part of the JSON line.
type kindSummary struct {
	TargetRate     float64          `json:"target_rate"`
	Sent           int64            `json:"sent"`
	Answered       int64            `json:"answered"`
	Failed         int64            `json:"failed"`
	Failures       map[string]int64 `json:"failures,omitempty"`
	ConnsOpened    int64            `json:"conns_opened"`
	LastOpenedS    float64          `json:"last_conn_opened_s"`
	ConnsReset     int64            `json:"conns_reset"`
	ConnsClosedSrv int64            `json:"conns_closed_by_server"`
	Lost           int64            `json:"ws_messages_lost,omitempty"`
	P50ms          float64          `json:"p50_ms"`
	P99ms          float64          `json:"p99_ms"`
	MaxMs          float64          `json:"max_ms"`
}

// summary is the JSON line at the end.
type summary struct {
	DurationS float64                `json:"duration_s"`
	Rate      float64                `json:"target_rate"`
	Kinds     map[string]kindSummary `json:"kinds"`
	Sent      int64                  `json:"sent"`
	Answered  int64                  `json:"answered"`
	Failed    int64                  `json:"failed"`
	Resets    int64                  `json:"resets"`
	WSLost    int64                  `json:"ws_messages_lost"`
	RateOK    bool                   `json:"rate_ok"`
	CPUs      float64                `json:"loadtest_cpu_s"`
	Samples   []sample               `json:"failure_samples,omitempty"`
	Pass      bool                   `json:"pass"`
}

// percentile is the nearest-rank percentile of a sorted slice.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[max(0, int(math.Ceil(p*float64(len(sorted))))-1)]
}

func ms(d time.Duration) float64 { return math.Round(float64(d)/float64(time.Millisecond)*100) / 100 }

func run(cfg config, out io.Writer) summary {
	l := &loader{config: cfg, roots: x509.NewCertPool()}
	if cfg.cacert != "" {
		pem, err := os.ReadFile(cfg.cacert)
		if err != nil || !l.roots.AppendCertsFromPEM(pem) {
			fmt.Fprintln(os.Stderr, "loadtest: can't use -cacert", cfg.cacert)
			os.Exit(2)
		}
	}
	l.start = time.Now().Add(300 * time.Millisecond)
	l.end = l.start.Add(cfg.duration)
	mk := func(name string, rate float64) *stats {
		return &stats{name: name, start: l.start, rate: rate, fails: map[string]int64{}}
	}
	var all []*stats
	var wg sync.WaitGroup
	launch := func(s *stats, f func()) {
		all = append(all, s)
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	if cfg.http1 != "" {
		s := mk("http1", cfg.split[0])
		launch(s, func() { l.runHTTP(s, cfg.http1, false, cfg.h1Conns, 1) })
	}
	if cfg.http2 != "" {
		s := mk("http2", cfg.split[1])
		launch(s, func() { l.runHTTP(s, cfg.http2, true, cfg.h2Conns, cfg.h2Streams) })
	}
	if len(cfg.ws) > 0 {
		s := mk("ws", cfg.split[2])
		launch(s, func() { l.runWS(s) })
	}
	fmt.Fprintf(out, "load starts at %s: %.0f requests per second for %s (%s)\n",
		l.start.Format("15:04:05.000"), cfg.rate, cfg.duration, describe(all))
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
loop:
	for {
		select {
		case <-done:
			break loop
		case <-tick.C:
			fmt.Fprintf(out, "t=%3.0fs", time.Since(l.start).Seconds())
			for _, s := range all {
				s.mu.Lock()
				fmt.Fprintf(out, "  %s sent %d answered %d failed %d", s.name, s.sent.Load(), s.answered, sumValues(s.fails))
				s.mu.Unlock()
			}
			fmt.Fprintln(out)
		}
	}
	elapsed := time.Since(l.start)
	sum := summary{DurationS: math.Round(elapsed.Seconds()*10) / 10, Rate: cfg.rate, Kinds: map[string]kindSummary{}, RateOK: true}
	for _, s := range all {
		s.mu.Lock()
		slices.Sort(s.lat)
		k := kindSummary{TargetRate: math.Round(s.rate*10) / 10, Sent: s.sent.Load(), Answered: s.answered, Failed: sumValues(s.fails),
			Failures: s.fails, ConnsOpened: s.opened.Load(), LastOpenedS: math.Round(float64(s.lastOpened.Load())/1e7) / 100, ConnsReset: s.reset.Load(), ConnsClosedSrv: s.closedBySrv.Load(), Lost: s.lost,
			P50ms: ms(percentile(s.lat, 0.50)), P99ms: ms(percentile(s.lat, 0.99))}
		if len(s.lat) > 0 {
			k.MaxMs = ms(s.lat[len(s.lat)-1])
		}
		sum.Kinds[s.name] = k
		sum.Sent += k.Sent
		sum.Answered += k.Answered
		sum.Failed += k.Failed
		sum.Resets += k.ConnsReset + s.resets
		sum.WSLost += s.lost
		sum.Samples = append(sum.Samples, s.samples...)
		if float64(k.Sent) < 0.98*s.rate*cfg.duration.Seconds() {
			sum.RateOK = false
		}
		s.mu.Unlock()
	}
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil {
		sum.CPUs = math.Round((float64(ru.Utime.Sec+ru.Stime.Sec)+float64(ru.Utime.Usec+ru.Stime.Usec)/1e6)*10) / 10
	}
	sum.Pass = sum.Failed == 0 && sum.Resets == 0 && sum.WSLost == 0 && sum.Sent > 0 && sum.RateOK
	report(out, all, sum)
	return sum
}

func describe(all []*stats) string {
	var parts []string
	for _, s := range all {
		parts = append(parts, fmt.Sprintf("%s %.0f/s", s.name, s.rate))
	}
	return strings.Join(parts, ", ")
}

func sumValues(m map[string]int64) (n int64) {
	for _, v := range m {
		n += v
	}
	return
}

func report(out io.Writer, all []*stats, sum summary) {
	fmt.Fprintf(out, "\n%-6s %9s %9s %7s %6s %9s %6s %7s %8s %8s %8s\n", "kind", "sent", "answered", "failed", "conns", "last open", "reset", "closed", "p50 ms", "p99 ms", "max ms")
	for _, s := range all {
		k := sum.Kinds[s.name]
		fmt.Fprintf(out, "%-6s %9d %9d %7d %6d %8.1fs %6d %7d %8.2f %8.2f %8.2f\n", s.name, k.Sent, k.Answered, k.Failed, k.ConnsOpened, k.LastOpenedS, k.ConnsReset, k.ConnsClosedSrv, k.P50ms, k.P99ms, k.MaxMs)
	}
	if ws, ok := sum.Kinds["ws"]; ok {
		fmt.Fprintf(out, "WebSocket messages: %d sent, %d echoed, %d lost\n", ws.Sent, ws.Answered, ws.Lost)
	}
	fmt.Fprintf(out, "total: %d sent, %d answered, %d failed, %d connections reset, %.1f s, rate held: %v, loadtest CPU %.1f s\n", sum.Sent, sum.Answered, sum.Failed, sum.Resets, sum.DurationS, sum.RateOK, sum.CPUs)
	for _, s := range all {
		for _, reason := range slices.Sorted(maps.Keys(s.fails)) {
			fmt.Fprintf(out, "  failure, %s: %s: %d\n", s.name, reason, s.fails[reason])
		}
	}
	for _, f := range sum.Samples {
		fmt.Fprintf(out, "  first failures: at %.2f s, %s, %s: %s\n", f.T, f.Kind, f.Reason, f.Detail)
	}
	if sum.Pass {
		fmt.Fprintln(out, "loadtest: PASS")
	} else {
		fmt.Fprintln(out, "loadtest: FAIL")
	}
}
