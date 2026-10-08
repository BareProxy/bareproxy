// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"bytes"
	"cmp"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"bareproxy/internal/plugin"
)

// PluginRun is what one plugin did to a request, in its record.
type PluginRun struct {
	Name   string   `json:"name"`
	Action string   `json:"action"`
	Error  string   `json:"error,omitempty"`
	MS     float64  `json:"ms"`
	Notes  []string `json:"notes,omitempty"`
}

// startPlugins starts the config's plugins. A plugin whose file, config and
// settings are the same as in the old runtime keeps running as it is, with
// its instances and shared data.
func (rt *Runtime) startPlugins(old *Runtime, logf func(string, ...any)) error {
	rt.Plugins = map[string]*plugin.Plugin{}
	c := rt.Cfg
	for _, name := range c.PluginOrder {
		ps := c.Plugins[name]
		if old != nil {
			if ops := old.Cfg.Plugins[name]; ops != nil && ops.key() == ps.key() && old.Plugins[name] != nil {
				rt.Plugins[name] = old.Plugins[name]
				continue
			}
		}
		s := plugin.Settings{Name: name, Config: ps.Config, Timeout: ps.Timeout, Pause: ps.Pause, Instances: ps.Instances,
			AllowHTTP: ps.AllowHTTP, BodyMax: ps.BodyLimit, Logf: logf}
		for _, dir := range ps.Read { // the plugin's own handles, which it keeps across applies
			root, err := os.OpenRoot(dir)
			if err != nil {
				for _, r := range s.Read {
					r.Close()
				}
				rt.stopPlugins(old)
				return fmt.Errorf("plugin %s (line %d) can't read folder %s: %v", name, ps.Line, dir, unwrapPathErr(err))
			}
			s.Read = append(s.Read, root)
		}
		if ps.Store > 0 {
			s.StoreDir, s.StoreMax = pluginStoreDir(c, name), ps.Store
		}
		p, err := plugin.Start(ps.Module, s)
		if err != nil {
			rt.stopPlugins(old)
			return fmt.Errorf("plugin %s (line %d) didn't start: %v", name, ps.Line, err)
		}
		rt.Plugins[name] = p
	}
	return nil
}

func pluginStoreDir(c *Config, name string) string {
	return stateDir(c) + "/plugins/store/" + name
}

// stopPlugins stops the plugins rt has and keep (another runtime) doesn't.
func (rt *Runtime) stopPlugins(keep *Runtime) {
	for name, p := range rt.Plugins {
		if keep == nil || keep.Plugins[name] != p {
			p.Close()
		}
	}
}

// pluginUse is one plugin running on one request.
type pluginUse struct {
	name string
	spec *PluginSpec
	p    *plugin.Plugin
	st   *plugin.Stream
	i    int  // its PluginRun in the record
	off  bool // it failed with on-error open, so later phases skip it
}

// wantsBody reports whether the plugin is handed a body: its config says so
// (body request, body response) and it has the callback.
func (u *pluginUse) wantsBody(phase int) bool {
	if phase == plugin.Request {
		return u.spec.BodyRequest && u.p.Wants("proxy_on_request_body")
	}
	return u.spec.BodyResponse && u.p.Wants("proxy_on_response_body")
}

// pluginRun is a request's plugins, and the response as they see it.
type pluginRun struct {
	s         *Server
	r         *http.Request
	rec       *Record
	uses      []*pluginUse
	respHdr   bool // a plugin has proxy_on_response_headers
	respBody  bool // a plugin has proxy_on_response_body
	bodyMax   int64
	seen      bool // the response's status went through the plugins
	buffering bool // the body is held for proxy_on_response_body
	replaced  bool // a plugin's response (or a 502) went out instead; the rest is dropped
	code      int
	buf       bytes.Buffer
}

// pluginRequest runs the site's plugins on a request before routing. It
// returns true when a plugin answered the request (or failed with on-error
// closed), so the request goes no further.
func (s *Server) pluginRequest(w *respWriter, r *http.Request, rt *Runtime, site *Site, rec *Record) bool {
	pr := &pluginRun{s: s, r: r, rec: rec}
	w.pl = pr
	host, port, _ := net.SplitHostPort(r.RemoteAddr)
	props := map[string]string{"request.path": r.RequestURI, "request.url_path": rec.Path, "request.method": r.Method,
		"request.host": r.Host, "request.scheme": rec.Scheme, "request.id": rec.ID, "request.protocol": r.Proto,
		"request.query": r.URL.RawQuery, "source.address": cmp.Or(host, r.RemoteAddr), "bareproxy.site": site.Name,
		"bareproxy.config_version": strconv.Itoa(rt.Version)}
	if r.TLS != nil {
		props["connection.requested_server_name"] = r.TLS.ServerName
		props["connection.tls_version"] = rec.TLS
	}
	ints := map[string]int64{}
	if n, err := strconv.Atoi(port); err == nil {
		ints["source.port"] = int64(n)
	}
	h := requestPairs(r, rec.Scheme)
	eos := r.Body == nil || r.Body == http.NoBody
	for _, u := range site.Uses {
		use := &pluginUse{name: u.Name, spec: rt.Cfg.Plugins[u.Name], p: rt.Plugins[u.Name], i: len(rec.Plugins)}
		rec.Plugins = append(rec.Plugins, PluginRun{Name: u.Name, Action: "went on"})
		pr.uses = append(pr.uses, use)
		start := time.Now()
		st, err := use.p.NewStream(maps.Clone(props), maps.Clone(ints))
		var nh [][2]string
		if err == nil {
			use.st = st
			// A plugin that pauses for the request body gets it next;
			// otherwise a pause waits for it to go on.
			nh, err = st.Headers(plugin.Request, h, eos, eos || !use.wantsBody(plugin.Request))
		}
		rec.Plugins[use.i].MS += msSince(start)
		if err != nil { // what a failed plugin changed is dropped
			if pr.fail(w, use, err) {
				return true
			}
			continue
		}
		h = nh
		if l := st.Local(); l != nil {
			rec.Plugins[use.i].Action = fmt.Sprintf("answered %d", l.Status)
			rec.Outcome, rec.Reason = "plugin", fmt.Sprintf("plugin %s answered %d", u.Name, l.Status)
			if l.Details != "" {
				rec.Reason += " (" + l.Details + ")"
			}
			pr.writeLocal(w, l)
			return true
		}
		if st.Closed() {
			rec.Plugins[use.i].Action = "closed the request"
			rec.Outcome, rec.Reason = "plugin_closed", "plugin "+u.Name+" closed the request"
			panic(http.ErrAbortHandler)
		}
	}
	if !eos && pr.requestBody(w, site) {
		return true
	}
	pr.applyRequest(h)
	for _, use := range pr.uses {
		if use.off {
			continue
		}
		pr.respHdr = pr.respHdr || use.p.Wants("proxy_on_response_headers")
		if use.wantsBody(plugin.Response) {
			pr.respBody, pr.bodyMax = true, max(pr.bodyMax, use.spec.BodyLimit)
		}
	}
	return false
}

// requestPairs gives a request's headers the Proxy-Wasm way: the pseudo
// headers first, then the rest in lower case, sorted.
func requestPairs(r *http.Request, scheme string) [][2]string {
	h := [][2]string{{":method", r.Method}, {":path", r.RequestURI}, {":authority", r.Host}, {":scheme", scheme}}
	for _, k := range slices.Sorted(maps.Keys(r.Header)) {
		for _, v := range r.Header[k] {
			h = append(h, [2]string{strings.ToLower(k), v})
		}
	}
	return h
}

// applyRequest puts the headers the plugins left back on the request. A
// changed :path changes the path the core routes.
func (pr *pluginRun) applyRequest(h [][2]string) {
	r := pr.r
	nh := http.Header{}
	for _, kv := range h {
		switch kv[0] {
		case ":method":
			r.Method = kv[1]
		case ":authority":
			r.Host = kv[1]
		case ":scheme":
		case ":path":
			if kv[1] == r.RequestURI {
				continue
			}
			u, err := url.ParseRequestURI(kv[1])
			if err != nil || !strings.HasPrefix(kv[1], "/") {
				pr.note("path %q from the plugins isn't a path, so it was left as it was", kv[1])
				continue
			}
			r.RequestURI, r.URL.Path, r.URL.RawPath, r.URL.RawQuery = kv[1], u.Path, u.RawPath, u.RawQuery
			pr.rec.Path = requestPath(r)
			pr.note("the plugins changed the path to %s", pr.rec.Path)
		default:
			nh.Add(kv[0], kv[1])
		}
	}
	r.Header = nh
}

// note adds a line to the last plugin's record.
func (pr *pluginRun) note(f string, a ...any) {
	if n := len(pr.rec.Plugins); n > 0 {
		pr.rec.Plugins[n-1].Notes = append(pr.rec.Plugins[n-1].Notes, fmt.Sprintf(f, a...))
	}
}

// requestBody hands the whole request body to the plugins that want it,
// when it fits in their body limit.
func (pr *pluginRun) requestBody(w *respWriter, site *Site) bool {
	var want []*pluginUse
	var limit int64
	for _, use := range pr.uses {
		if !use.off && use.wantsBody(plugin.Request) {
			want, limit = append(want, use), max(limit, use.spec.BodyLimit)
		}
	}
	if len(want) == 0 {
		return false
	}
	r := pr.r
	limit = min(limit, site.BodyLimit)
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		pr.rec.Outcome, pr.rec.Reason = "bad_request", "the request body couldn't be read: "+shortErr(err)
		pr.seen = true
		pr.s.plain(w, r, pr.rec, http.StatusBadRequest, "Bad request: the request body is broken")
		return true
	}
	if int64(len(body)) > limit {
		for _, use := range want {
			pr.rec.Plugins[use.i].Notes = append(pr.rec.Plugins[use.i].Notes, "the request body is over the body limit, so it wasn't handed over")
		}
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(body), r.Body), r.Body}
		return false
	}
	for _, use := range want {
		if use.off {
			continue
		}
		start := time.Now()
		b, err := use.st.Body(plugin.Request, body)
		pr.rec.Plugins[use.i].MS += msSince(start)
		if err != nil {
			if pr.fail(w, use, err) {
				return true
			}
			continue
		}
		body = b
		if l := use.st.Local(); l != nil {
			pr.rec.Plugins[use.i].Action = fmt.Sprintf("answered %d", l.Status)
			pr.rec.Outcome, pr.rec.Reason = "plugin", fmt.Sprintf("plugin %s answered %d", use.name, l.Status)
			pr.writeLocal(w, l)
			return true
		}
	}
	r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	r.TransferEncoding = nil
	return false
}

// fail handles a plugin that failed. With on-error open the request goes on
// without it; with on-error closed it gets 502, if the response hasn't
// started. It returns true when the request is answered.
func (pr *pluginRun) fail(w *respWriter, use *pluginUse, err error) bool {
	run := &pr.rec.Plugins[use.i]
	run.Error = err.Error()
	if use.spec.OnError == "open" {
		use.off = true
		run.Action = "failed, so the request went on without it (on-error open)"
		return false
	}
	run.Action = "failed (on-error closed)"
	pr.rec.Outcome, pr.rec.Reason = "plugin_error", "plugin "+use.name+" failed: "+err.Error()
	if w.status != 0 {
		return true
	}
	pr.seen, pr.buffering = true, false
	pr.s.plain(w, pr.r, pr.rec, http.StatusBadGateway, "Bad gateway: a plugin failed")
	pr.replaced = true
	return true
}

// writeLocal sends a plugin's own response. The response phase is skipped.
func (pr *pluginRun) writeLocal(w *respWriter, l *plugin.Local) {
	pr.seen, pr.buffering = true, false
	h := w.Header()
	id := h.Get("BareProxy-Id")
	clear(h)
	if id != "" {
		h.Set("BareProxy-Id", id)
	}
	for _, kv := range l.Headers {
		if !strings.HasPrefix(kv[0], ":") {
			h.Add(kv[0], kv[1])
		}
	}
	if h.Get("Content-Type") == "" && len(l.Body) > 0 {
		h.Set("Content-Type", "text/plain; charset=utf-8")
	}
	h.Set("Content-Length", strconv.Itoa(len(l.Body)))
	w.writeHeader(l.Status)
	if pr.r.Method != http.MethodHead {
		w.Write(l.Body)
	}
	pr.replaced = true
}

// header runs the response headers through the plugins, last plugin first.
// It returns the status to send and whether the response was handled here
// (answered by a plugin, or held for the body phase).
func (pr *pluginRun) header(w *respWriter, code int) (int, bool) {
	if pr.seen || code < 200 && code != http.StatusSwitchingProtocols {
		return code, false
	}
	pr.seen = true
	if code == http.StatusSwitchingProtocols { // a WebSocket tunnel passes as it is
		return code, false
	}
	// Whether the body goes to the plugins is settled first: a plugin that
	// pauses on the headers to wait for the body gets it in the body phase.
	why := pr.bodySkip(w, code)
	body := pr.respBody && why == ""
	if pr.respHdr {
		h := [][2]string{{":status", strconv.Itoa(code)}}
		for _, k := range slices.Sorted(maps.Keys(w.Header())) {
			for _, v := range w.Header()[k] {
				h = append(h, [2]string{strings.ToLower(k), v})
			}
		}
		for i := len(pr.uses) - 1; i >= 0; i-- {
			use := pr.uses[i]
			if use.off || !use.p.Wants("proxy_on_response_headers") {
				continue
			}
			start := time.Now()
			use.st.SetInt("response.code", int64(code))
			nh, err := use.st.Headers(plugin.Response, h, false, !(body && use.wantsBody(plugin.Response)))
			pr.rec.Plugins[use.i].MS += msSince(start)
			if err != nil {
				if pr.fail(w, use, err) {
					return 0, true
				}
				continue
			}
			h = nh
			if l := use.st.Local(); l != nil {
				pr.rec.Plugins[use.i].Action = fmt.Sprintf("replaced the response with %d", l.Status)
				pr.writeLocal(w, l)
				return 0, true
			}
		}
		out := w.Header()
		id := out.Get("BareProxy-Id")
		clear(out)
		for _, kv := range h {
			if kv[0] == ":status" {
				if n, err := strconv.Atoi(kv[1]); err == nil && n >= 200 && n <= 999 {
					code = n
				}
				continue
			}
			if !strings.HasPrefix(kv[0], ":") {
				out.Add(kv[0], kv[1])
			}
		}
		if id != "" && out.Get("BareProxy-Id") == "" {
			out.Set("BareProxy-Id", id)
		}
	}
	pr.code = code
	if why != "" && why != "none" {
		pr.bodyNote("the response body is %s, so it wasn't handed over", why)
	}
	if !body {
		return code, false
	}
	pr.buffering = true
	return code, true
}

// bodySkip says why the response body won't go to the plugins: "none" when
// no plugin takes it or there's no body, or a reason for the record. Bodies
// a plugin can't make sense of whole go out as they are: a part of a file
// (206), a compressed body, a stream of events.
func (pr *pluginRun) bodySkip(w *respWriter, code int) string {
	if !pr.respBody || pr.r.Method == http.MethodHead || code == http.StatusNoContent || code == http.StatusNotModified {
		return "none"
	}
	h := w.Header()
	switch enc := h.Get("Content-Encoding"); {
	case code == http.StatusPartialContent:
		return "partial (206)"
	case enc != "" && enc != "identity":
		return "compressed (" + enc + ")"
	case strings.HasPrefix(h.Get("Content-Type"), "text/event-stream"):
		return "a stream of events"
	}
	return ""
}

// bodyNote adds a note to each plugin that wanted the response body.
func (pr *pluginRun) bodyNote(f string, a ...any) {
	for _, use := range pr.uses {
		if !use.off && use.wantsBody(plugin.Response) {
			pr.rec.Plugins[use.i].Notes = append(pr.rec.Plugins[use.i].Notes, fmt.Sprintf(f, a...))
		}
	}
}

// release sends the held status and body and stops holding: the rest of
// the body streams, unseen by the plugins.
func (pr *pluginRun) release(w *respWriter) error {
	pr.buffering = false
	w.writeHeader(pr.code)
	_, err := w.Write(pr.buf.Bytes())
	pr.buf.Reset()
	return err
}

// write takes response body bytes while the body is held. Past the body
// limit the held part goes out and the rest streams, unseen by the plugins.
func (pr *pluginRun) write(w *respWriter, b []byte) (int, error) {
	if int64(pr.buf.Len()+len(b)) <= pr.bodyMax {
		return pr.buf.Write(b)
	}
	pr.bodyNote("the response body is over the body limit, so it wasn't handed over")
	if err := pr.release(w); err != nil {
		return 0, err
	}
	return w.Write(b)
}

// finishBody hands a held response body to the plugins, last plugin first,
// and sends it.
func (pr *pluginRun) finishBody(w *respWriter) {
	if !pr.buffering {
		return
	}
	pr.buffering = false
	body := pr.buf.Bytes()
	for i := len(pr.uses) - 1; i >= 0; i-- {
		use := pr.uses[i]
		if use.off || !use.wantsBody(plugin.Response) {
			continue
		}
		start := time.Now()
		b, err := use.st.Body(plugin.Response, body)
		pr.rec.Plugins[use.i].MS += msSince(start)
		if err != nil {
			if pr.fail(w, use, err) {
				return
			}
			continue
		}
		body = b
		if l := use.st.Local(); l != nil {
			pr.rec.Plugins[use.i].Action = fmt.Sprintf("replaced the response with %d", l.Status)
			pr.writeLocal(w, l)
			return
		}
	}
	if !bytes.Equal(body, pr.buf.Bytes()) { // validators of the old body don't fit the new one
		w.Header().Del("ETag")
		w.Header().Del("Content-MD5")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.writeHeader(pr.code)
	w.Write(body)
}

// finish runs proxy_on_log and ends each plugin's stream, and puts their
// notes in the record. It runs once the response is sent.
func (pr *pluginRun) finish() {
	for _, use := range pr.uses {
		if use.st == nil {
			continue
		}
		start := time.Now()
		run := &pr.rec.Plugins[use.i]
		if err := use.st.Done(!use.off); err != nil && run.Error == "" {
			run.Error = "in proxy_on_log: " + err.Error()
		}
		run.Notes = append(use.st.Notes(), run.Notes...)
		run.MS += msSince(start)
	}
}
