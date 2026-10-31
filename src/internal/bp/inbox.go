// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"bareproxy/internal/inbox"
)

// The webhook inbox lives in the core, not in a plugin: it has to write to
// disk before it answers, keep timers across restarts and call the app on its
// own schedule, and a Proxy-Wasm plugin can't hold a request's answer on a
// disk write, has no files of its own and only calls out while a request or
// a timer tick holds it. See internal/inbox for the delivery promises.

// InboxSpec is one inbox block.
type InboxSpec struct {
	Line       int
	Name       string
	Deliver    string // the pool events go to
	Path       string // the path on the app; "" keeps the request's path
	Folder     string
	Sig        string // "", github, shopify, stripe or hmac-sha256
	SigHeader  string
	SigEnc     string // hex or base64, for hmac-sha256
	SigFile    string
	secret     []byte
	KeyHeader  string // the header carrying the sender's own event ID
	BodyLimit  int64
	Timeout    time.Duration
	Backoff    time.Duration
	MaxBackoff time.Duration
	GiveUp     time.Duration
	Keep       time.Duration
	Workers    int
	Tolerance  time.Duration // how old a Stripe signature's time may be
}

func (p *parser) newInbox(ln int, w []string) *InboxSpec {
	ib := &InboxSpec{Line: ln, BodyLimit: 1 << 20, Timeout: 30 * time.Second, Backoff: 10 * time.Second,
		MaxBackoff: time.Hour, GiveUp: 72 * time.Hour, Keep: 30 * 24 * time.Hour, Workers: 4, Tolerance: 5 * time.Minute}
	if len(w) != 2 || !validName(w[1]) {
		p.errf(ln, "inbox takes one name made of letters, digits, - and _")
		return ib
	}
	ib.Name = w[1]
	if old := p.c.Inboxes[ib.Name]; old != nil {
		p.errf(ln, "inbox %s is already defined on line %d", ib.Name, old.Line)
		return ib
	}
	p.c.Inboxes[ib.Name] = ib
	p.c.InboxOrder = append(p.c.InboxOrder, ib.Name)
	return ib
}

// parseLongDuration reads a duration that may also be in days, such as 3d.
func parseLongDuration(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		d, err := strconv.Atoi(n)
		if err != nil || d <= 0 {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		return time.Duration(d) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("bad duration %q: use a number with ms, s, m, h or d", s)
	}
	return d, nil
}

func (p *parser) inboxSetting(ib *InboxSpec, ln int, w []string) error {
	dur := func(dst *time.Duration) error {
		if len(w) != 2 {
			return fmt.Errorf("%s takes one duration", w[0])
		}
		d, err := parseLongDuration(w[1])
		*dst = d
		return err
	}
	switch w[0] {
	case "deliver":
		if len(w) != 2 || !validName(w[1]) {
			return errors.New("deliver takes the name of the pool the events go to")
		}
		ib.Deliver = w[1]
	case "path":
		if len(w) != 2 || !isNormalPath(w[1]) {
			return errors.New("path takes the path events are sent to on the app, such as /hooks/stripe")
		}
		ib.Path = w[1]
	case "folder":
		if len(w) != 2 {
			return errors.New("folder takes one folder")
		}
		ib.Folder = p.path(w[1])
	case "signature":
		switch {
		case len(w) == 3 && (w[1] == "github" || w[1] == "shopify" || w[1] == "stripe"):
			ib.Sig, ib.SigFile = w[1], p.path(w[2])
			ib.SigHeader = map[string]string{"github": "X-Hub-Signature-256", "shopify": "X-Shopify-Hmac-Sha256", "stripe": "Stripe-Signature"}[w[1]]
		case len(w) == 5 && w[1] == "hmac-sha256" && validHeaderName(w[2]) && (w[4] == "hex" || w[4] == "base64"):
			ib.Sig, ib.SigHeader, ib.SigFile, ib.SigEnc = w[1], http.CanonicalHeaderKey(w[2]), p.path(w[3]), w[4]
		default:
			return errors.New("signature takes github, shopify or stripe and a file holding the secret, or hmac-sha256 HEADER FILE hex|base64")
		}
	case "key-header":
		if len(w) != 2 || !validHeaderName(w[1]) {
			return errors.New("key-header takes the header that carries the sender's own event ID, such as X-GitHub-Delivery")
		}
		ib.KeyHeader = http.CanonicalHeaderKey(w[1])
	case "body-limit":
		if len(w) != 2 {
			return errors.New("body-limit takes one size, such as 1MB")
		}
		n, err := parseSize(w[1])
		if err != nil {
			return err
		}
		ib.BodyLimit = n
	case "timeout":
		return dur(&ib.Timeout)
	case "retry":
		if len(w) != 3 {
			return errors.New("retry takes the first wait and the longest wait, such as retry 10s 1h")
		}
		a, err1 := parseLongDuration(w[1])
		b, err2 := parseLongDuration(w[2])
		if err := errors.Join(err1, err2); err != nil {
			return err
		}
		if b < a {
			return errors.New("the longest wait can't be shorter than the first")
		}
		ib.Backoff, ib.MaxBackoff = a, b
	case "give-up":
		return dur(&ib.GiveUp)
	case "keep":
		return dur(&ib.Keep)
	case "workers":
		n, err := strconv.Atoi(w[min(1, len(w)-1)])
		if len(w) != 2 || err != nil || n < 1 || n > 64 {
			return errors.New("workers takes a number from 1 to 64")
		}
		ib.Workers = n
	default:
		return fmt.Errorf("unknown inbox setting %q", w[0])
	}
	return nil
}

// compileInboxes checks each inbox's pool and reads its secret.
func (p *parser) compileInboxes() {
	c := p.c
	used := map[string]bool{}
	for _, s := range c.Sites {
		for _, r := range s.Routes {
			if r.Act.Kind == "inbox" {
				used[r.Act.Inbox] = true
			}
		}
	}
	folders := map[string]string{}
	for _, name := range c.InboxOrder {
		ib := c.Inboxes[name]
		switch {
		case ib.Deliver == "":
			p.errf(ib.Line, "inbox %s needs deliver POOL: the pool its events go to", name)
		case c.Pools[ib.Deliver] == nil:
			p.errf(ib.Line, "inbox %s delivers to pool %s, which isn't defined", name, ib.Deliver)
		}
		if !used[name] {
			p.warnf(ib.Line, "inbox %s isn't used by any route", name)
		}
		if ib.Folder == "" {
			ib.Folder = filepath.Join(stateDir(c), "inbox", name)
		}
		if other, dup := folders[ib.Folder]; dup {
			p.errf(ib.Line, "inbox %s uses the same folder as inbox %s", name, other)
		}
		folders[ib.Folder] = name
		if ib.Sig != "" && !p.opt.NoDisk {
			b, err := os.ReadFile(ib.SigFile)
			if err != nil {
				p.errf(ib.Line, "inbox %s: can't read the signature secret: %v", name, unwrapPathErr(err))
				continue
			}
			if ib.secret = bytes.TrimSpace(b); len(ib.secret) == 0 {
				p.errf(ib.Line, "inbox %s: the signature secret file %s is empty", name, ib.SigFile)
			}
		}
	}
}

// inboxes keeps the open inboxes by folder, so an apply that keeps an inbox
// keeps it open and delivering, and two runtimes never open one folder twice.
var inboxes = struct {
	sync.Mutex
	m map[string]*openInbox
}{m: map[string]*openInbox{}}

type openInbox struct {
	ib   *inbox.Inbox
	refs int
}

func acquireInbox(folder string) (*inbox.Inbox, error) {
	inboxes.Lock()
	defer inboxes.Unlock()
	if o := inboxes.m[folder]; o != nil {
		o.refs++
		return o.ib, nil
	}
	ib, err := inbox.Open(folder)
	if err != nil {
		return nil, err
	}
	inboxes.m[folder] = &openInbox{ib: ib, refs: 1}
	return ib, nil
}

func releaseInbox(folder string) {
	inboxes.Lock()
	o := inboxes.m[folder]
	if o != nil {
		if o.refs--; o.refs <= 0 {
			delete(inboxes.m, folder)
		}
	}
	inboxes.Unlock()
	if o != nil && o.refs <= 0 {
		o.ib.Close()
	}
}

// startInboxes opens the config's inboxes and points them at the runtime's
// pools. It runs last in NewRuntime, so a runtime that fails to build never
// takes the inboxes over.
func (rt *Runtime) startInboxes(logf func(string, ...any)) error {
	rt.Inboxes = map[string]*inbox.Inbox{}
	for _, name := range rt.Cfg.InboxOrder {
		spec := rt.Cfg.Inboxes[name]
		ib, err := acquireInbox(spec.Folder)
		if err != nil {
			rt.stopInboxes()
			return fmt.Errorf("inbox %s: can't open %s: %v", name, spec.Folder, err)
		}
		rt.Inboxes[name] = ib
	}
	for name, ib := range rt.Inboxes {
		spec := rt.Cfg.Inboxes[name]
		o := inbox.Options{Backoff: spec.Backoff, MaxBackoff: spec.MaxBackoff, GiveUp: spec.GiveUp, Keep: spec.Keep, Workers: spec.Workers}
		ib.Set(o, rt.deliverTo(spec), func(s string) {
			if logf != nil {
				logf("inbox %s: %s", name, s)
			}
			rt.Mem.Event("inbox", "inbox "+name+": "+s)
		})
	}
	return nil
}

func (rt *Runtime) stopInboxes() {
	for name := range rt.Inboxes {
		releaseInbox(rt.Cfg.Inboxes[name].Folder)
	}
	rt.Inboxes = nil
}

// hopHeaders aren't passed on with a stored event.
var hopHeaders = []string{"Connection", "Keep-Alive", "Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	"Content-Length", "Expect", "Forwarded", "BareProxy-Id", "BareProxy-Inbox-Id", "BareProxy-Inbox-Attempt", "BareProxy-Inbox-Replay"}

// deliverTo sends an event to the inbox's pool, through the same backend
// choice, health and connection accounting as proxied requests.
func (rt *Runtime) deliverTo(spec *InboxSpec) inbox.Deliver {
	pool := rt.Pools[spec.Deliver]
	return func(ctx context.Context, id string, req *inbox.Request, attempt int, replay string) inbox.Result {
		ctx, cancel := context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
		path := req.Path
		if spec.Path != "" {
			path = spec.Path
		}
		u := "http://inbox" + path
		if req.Query != "" {
			u += "?" + req.Query
		}
		out, err := http.NewRequestWithContext(ctx, req.Method, u, bytes.NewReader(req.Body))
		if err != nil {
			return inbox.Result{Err: err}
		}
		// No GetBody: with it, Go's transport sends a request that carries an
		// Idempotency-Key header again by itself when a reused connection
		// breaks after the request went out, so the app could get the event
		// twice with no in-doubt mark. Without it, that break comes back
		// here as an error and the retry is marked in-doubt.
		out.GetBody = nil
		out.Header = req.Header.Clone()
		// Headers the sender named in Connection are hop-by-hop too.
		for _, v := range out.Header["Connection"] {
			for _, name := range strings.Split(v, ",") {
				if name = strings.TrimSpace(name); name != "" {
					out.Header.Del(name)
				}
			}
		}
		for _, h := range hopHeaders {
			out.Header.Del(h)
		}
		out.ContentLength = int64(len(req.Body))
		out.Host = pool.Spec.HostHeader
		if out.Host == "" {
			out.Host = req.Host
		}
		out.Header.Set("BareProxy-Inbox-Id", id)
		out.Header.Set("BareProxy-Inbox-Attempt", strconv.Itoa(attempt))
		if replay != "" {
			out.Header.Set("BareProxy-Inbox-Replay", replay)
		}
		rec := &Record{}
		resp, err := (&poolTransport{pool: pool, rec: rec}).RoundTrip(out)
		if err != nil {
			var de *dialError
			// No connection was ever opened (or no backend was up), so the
			// app can't have seen it. Anything else may have reached it.
			sure := errors.Is(err, errNoBackend) || errors.As(err, &de)
			return inbox.Result{Err: errors.New(shortErr(err)), InDoubt: !sure}
		}
		// The status is the app's answer; the rest of its body doesn't matter.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return inbox.Result{Status: resp.StatusCode}
	}
}

// checkSignature checks a webhook's signature with the inbox's secret.
func (spec *InboxSpec) checkSignature(h http.Header, body []byte, now time.Time) error {
	got := strings.TrimSpace(h.Get(spec.SigHeader))
	if got == "" {
		return fmt.Errorf("no %s header", spec.SigHeader)
	}
	mac := func(msg []byte) []byte {
		m := hmac.New(sha256.New, spec.secret)
		m.Write(msg)
		return m.Sum(nil)
	}
	equal := func(want []byte, gotText string, enc string) bool {
		var g []byte
		var err error
		if enc == "base64" {
			g, err = base64.StdEncoding.DecodeString(gotText)
		} else {
			g, err = hex.DecodeString(gotText)
		}
		return err == nil && subtle.ConstantTimeCompare(g, want) == 1
	}
	switch spec.Sig {
	case "github":
		if !equal(mac(body), strings.TrimPrefix(got, "sha256="), "hex") {
			return errors.New("the signature doesn't match")
		}
	case "shopify":
		if !equal(mac(body), got, "base64") {
			return errors.New("the signature doesn't match")
		}
	case "hmac-sha256":
		if !equal(mac(body), strings.TrimPrefix(got, "sha256="), spec.SigEnc) {
			return errors.New("the signature doesn't match")
		}
	case "stripe":
		var ts string
		var sigs []string
		for _, part := range strings.Split(got, ",") {
			k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
			switch k {
			case "t":
				ts = v
			case "v1":
				sigs = append(sigs, v)
			}
		}
		sec, err := strconv.ParseInt(ts, 10, 64)
		if err != nil || len(sigs) == 0 {
			return errors.New("the Stripe-Signature header has no t= or no v1=")
		}
		if d := now.Sub(time.Unix(sec, 0)); d > spec.Tolerance || d < -spec.Tolerance {
			return fmt.Errorf("the signature's time is %s off", d.Round(time.Second))
		}
		want := mac([]byte(ts + "." + string(body)))
		for _, s := range sigs {
			if equal(want, s, "hex") {
				return nil
			}
		}
		return errors.New("the signature doesn't match")
	}
	return nil
}

// serveInbox stores a webhook and answers 202 once it is on disk. The app
// gets it from the inbox, not from this request.
func (s *Server) serveInbox(w *respWriter, r *http.Request, rt *Runtime, site *Site, route *Route, norm string, rec *Record) {
	spec := rt.Cfg.Inboxes[route.Act.Inbox]
	ib := rt.Inboxes[route.Act.Inbox]
	rec.Outcome, rec.Inbox = "inbox", spec.Name
	if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch {
		rec.Reason = "an inbox takes only POST, PUT and PATCH"
		w.Header().Set("Allow", "POST, PUT, PATCH")
		s.plain(w, r, rec, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	limit := min(spec.BodyLimit, site.BodyLimit)
	if r.ContentLength > limit {
		rec.Outcome, rec.Reason = "too_large", fmt.Sprintf("the body is over the inbox's limit of %s", fmtBytes(limit))
		s.plain(w, r, rec, http.StatusRequestEntityTooLarge, "The request body is larger than this inbox allows")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			rec.Outcome, rec.Reason = "too_large", fmt.Sprintf("the body is over the inbox's limit of %s", fmtBytes(limit))
			s.plain(w, r, rec, http.StatusRequestEntityTooLarge, "The request body is larger than this inbox allows")
			return
		}
		rec.Outcome, rec.Reason = "bad_request", shortErr(err)
		s.plain(w, r, rec, http.StatusBadRequest, "Bad request: the request body is broken")
		return
	}
	if spec.Sig != "" {
		if err := spec.checkSignature(r.Header, body, time.Now()); err != nil {
			rec.Outcome, rec.Reason = "refused", "signature check failed: "+err.Error()
			s.plain(w, r, rec, http.StatusUnauthorized, "Signature check failed")
			return
		}
	}
	if ib == nil {
		rec.Outcome, rec.Reason = "inbox_error", "the inbox isn't open"
		s.plain(w, r, rec, http.StatusServiceUnavailable, "The inbox isn't open")
		return
	}
	h := r.Header.Clone()
	h.Set("X-Forwarded-For", rec.Client)
	h.Set("X-Forwarded-Proto", rec.Scheme)
	h.Set("X-Forwarded-Host", r.Host)
	key := ""
	if spec.KeyHeader != "" {
		key = strings.TrimSpace(r.Header.Get(spec.KeyHeader))
	}
	id, dup, err := ib.Add(&inbox.Request{Method: r.Method, Host: r.Host, Path: norm, Query: r.URL.RawQuery, Header: h, Body: body}, key)
	if err != nil {
		// Not on disk, so no yes: the sender keeps it and sends it again.
		rec.Outcome, rec.Reason = "inbox_error", "can't store the event: "+err.Error()
		s.plain(w, r, rec, http.StatusServiceUnavailable, "The event couldn't be stored; send it again later")
		return
	}
	rec.Event = id
	if dup {
		rec.Reason = fmt.Sprintf("%s %s is already stored as event %s", spec.KeyHeader, key, id)
	}
	w.Header().Set("BareProxy-Inbox-Id", id)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, "Stored as event %s\n", id)
}

// explainInbox says what an inbox route does with a request.
func explainInbox(b *strings.Builder, rt *Runtime, route *Route, norm, method string) {
	spec := rt.Cfg.Inboxes[route.Act.Inbox]
	if method != http.MethodPost && method != http.MethodPut && method != http.MethodPatch {
		b.WriteString("Action: 405, an inbox takes only POST, PUT and PATCH\n")
		return
	}
	fmt.Fprintf(b, "Inbox %s (line %d): stored in %s, answered 202 once on disk\n", spec.Name, spec.Line, spec.Folder)
	if spec.Sig != "" {
		fmt.Fprintf(b, "  checked first: %s signature in %s (401 when it doesn't match)\n", spec.Sig, spec.SigHeader)
	}
	path := norm
	if spec.Path != "" {
		path = spec.Path
	}
	fmt.Fprintf(b, "  delivered to pool %s as %s %s, retried from %s up to %s apart, for %s\n", spec.Deliver, method, path,
		fmtDur(spec.Backoff), fmtDur(spec.MaxBackoff), fmtDur(spec.GiveUp))
	b.WriteString("Action: 202 (inbox)\n")
}

// fmtDur writes a duration in whole days when it is days.
func fmtDur(d time.Duration) string {
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	return d.String()
}

// The admin endpoints of the inboxes, for bareproxy inbox.
func init() {
	adminHandlers = append(adminHandlers, func(s *Server, mux *http.ServeMux) {
		mux.HandleFunc("GET /inbox", s.adminInboxList)
		mux.HandleFunc("GET /inbox/event", s.adminInboxEvent)
		mux.HandleFunc("POST /inbox/replay", s.adminInboxReplay)
	})
}

// InboxList is what GET /inbox sends for one inbox.
type InboxList struct {
	Name   string        `json:"name"`
	Line   int           `json:"line"`
	Folder string        `json:"folder"`
	Pool   string        `json:"pool"`
	Counts inbox.Counts  `json:"counts"`
	Events []inbox.State `json:"events"`
}

// inboxesNamed returns the running inboxes a request names (all with no
// name), or an error that says which names exist.
func (s *Server) inboxesNamed(name string) (*Runtime, []string, error) {
	rt := s.rt.Load()
	if name == "" {
		if len(rt.Cfg.InboxOrder) == 0 {
			return nil, nil, errors.New("the running config has no inboxes")
		}
		return rt, rt.Cfg.InboxOrder, nil
	}
	if rt.Inboxes[name] == nil {
		return nil, nil, fmt.Errorf("the running config has no inbox named %s (it has: %s)", name, strings.Join(rt.Cfg.InboxOrder, ", "))
	}
	return rt, []string{name}, nil
}

func (s *Server) adminInboxList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rt, names, err := s.inboxesNamed(q.Get("name"))
	if err != nil {
		replyErr(w, r, err)
		return
	}
	status := q.Get("status")
	var out []InboxList
	var b strings.Builder
	for _, n := range names {
		spec, ib := rt.Cfg.Inboxes[n], rt.Inboxes[n]
		l := InboxList{Name: n, Line: spec.Line, Folder: spec.Folder, Pool: spec.Deliver, Counts: ib.Counts(), Events: []inbox.State{}}
		for _, st := range ib.List() {
			if status == "" || st.Status == status {
				l.Events = append(l.Events, st)
			}
		}
		out = append(out, l)
		c := l.Counts
		fmt.Fprintf(&b, "Inbox %s (line %d), to pool %s, in %s\n  %d pending, %d delivering, %d delivered, %d failed\n",
			n, spec.Line, spec.Deliver, spec.Folder, c.Pending, c.Delivering, c.Delivered, c.Failed)
		for _, st := range l.Events {
			fmt.Fprintf(&b, "  %s  %s  %-10s %-8s %s, %s, %d %s%s\n", st.ID, st.Received.Format("2006-01-02 15:04:05"), st.Status,
				st.Replay, st.Path, fmtBytes(int64(st.Size)), len(st.Attempts), plural2(len(st.Attempts), "attempt", "attempts"), lastTry(st))
		}
	}
	reply(w, r, http.StatusOK, out, b.String())
}

func plural2(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// lastTry says how the latest attempt went, and when the next one is.
func lastTry(st inbox.State) string {
	out := ""
	if n := len(st.Attempts); n > 0 {
		a := st.Attempts[n-1]
		switch {
		case a.Error != "":
			out = ", last: " + a.Error
		case a.Status != 0:
			out = fmt.Sprintf(", last: %d", a.Status)
		}
	}
	if st.Status == "pending" && !st.NextAt.IsZero() {
		if d := time.Until(st.NextAt); d > 0 {
			out += ", next in " + d.Round(time.Second).String()
		} else {
			out += ", next now"
		}
	}
	return out
}

func (s *Server) adminInboxEvent(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rt, names, err := s.inboxesNamed(q.Get("name"))
	if err == nil && len(names) != 1 {
		err = errors.New("name the inbox")
	}
	if err != nil {
		replyErr(w, r, err)
		return
	}
	st, req, err := rt.Inboxes[names[0]].Get(q.Get("id"))
	if err != nil {
		replyErr(w, r, fmt.Errorf("inbox %s has no event %s", names[0], q.Get("id")))
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Event %s in inbox %s: %s\n", st.ID, names[0], st.Status)
	fmt.Fprintf(&b, "Received %s: %s %s%s, %s\n", st.Received.Format("2006-01-02 15:04:05.000 UTC"), req.Method, req.Host, withQuery(req.Path, req.Query), fmtBytes(int64(st.Size)))
	if st.Key != "" {
		fmt.Fprintf(&b, "Sender's ID: %s\n", st.Key)
	}
	for _, k := range sortedKeys(req.Header) {
		fmt.Fprintf(&b, "  %s: %s\n", k, strings.Join(req.Header[k], ", "))
	}
	for i, a := range st.Attempts {
		how := fmt.Sprintf("%d", a.Status)
		if a.Error != "" {
			how = a.Error
		}
		mark := ""
		if a.Replay != "" {
			mark = " (marked " + a.Replay + ")"
		}
		if a.InDoubt {
			mark += ", may have reached the app"
		}
		fmt.Fprintf(&b, "Attempt %d at %s: %s%s\n", i+1, a.Start.Format("2006-01-02 15:04:05"), how, mark)
	}
	if !st.Delivered.IsZero() {
		fmt.Fprintf(&b, "Delivered %s\n", st.Delivered.Format("2006-01-02 15:04:05 UTC"))
	}
	if st.Status == "pending" {
		fmt.Fprintf(&b, "Next attempt %s\n", st.NextAt.Format("2006-01-02 15:04:05 UTC"))
	}
	if q.Get("body") == "1" {
		fmt.Fprintf(&b, "\n%s\n", req.Body)
	}
	reply(w, r, http.StatusOK, map[string]any{"state": st, "request": req, "body": string(req.Body)}, b.String())
}

func sortedKeys(h http.Header) []string {
	ks := make([]string, 0, len(h))
	for k := range h {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

func (s *Server) adminInboxReplay(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rt, names, err := s.inboxesNamed(q.Get("name"))
	if err == nil && len(names) != 1 {
		err = errors.New("name the inbox")
	}
	if err != nil {
		replyErr(w, r, err)
		return
	}
	ib := rt.Inboxes[names[0]]
	ids := q["id"]
	if q.Get("failed") == "1" {
		for _, st := range ib.List() {
			if st.Status == "failed" {
				ids = append(ids, st.ID)
			}
		}
		if len(ids) == 0 {
			reply(w, r, http.StatusOK, map[string]any{"replayed": []string{}}, "No failed events to replay.\n")
			return
		}
	}
	var done []string
	var errs []error
	for _, id := range ids {
		if err := ib.Replay(id); err != nil {
			if inbox.IsNotFound(err) {
				err = fmt.Errorf("inbox %s has no event %s", names[0], id)
			}
			errs = append(errs, err)
			continue
		}
		done = append(done, id)
	}
	if len(done) > 0 {
		s.Event("inbox", fmt.Sprintf("inbox %s: %d %s replayed by %s", names[0], len(done), plural2(len(done), "event", "events"), cmp.Or(peerUser(r), "the admin socket")))
	}
	if err := errors.Join(errs...); err != nil {
		replyErr(w, r, err)
		return
	}
	reply(w, r, http.StatusOK, map[string]any{"replayed": done}, fmt.Sprintf("Replayed %d %s; each delivery carries BareProxy-Inbox-Replay: operator.\n", len(done), plural2(len(done), "event", "events")))
}
