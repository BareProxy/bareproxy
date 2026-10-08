// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tetratelabs/wazero/api"
)

// Proxy-Wasm status codes.
const (
	stOK            = 0
	stNotFound      = 1
	stBadArgument   = 2
	stSerialization = 3
	stMemory        = 6
	stCASMismatch   = 8
	stInternal      = 10
	stUnimplemented = 12
)

type hostFunc struct {
	params, results []api.ValueType
	fn              func(c *hcall, a []uint64) uint32
}

// hcall is one host function call: the instance, the calling module and the
// context, which carries the call's time limit.
type hcall struct {
	in  *instance
	m   api.Module
	ctx context.Context
}

func (h hostFunc) goFunc() api.GoModuleFunc {
	n := len(h.params)
	return func(ctx context.Context, m api.Module, st []uint64) {
		in, _ := ctx.Value(ctxKey{}).(*instance)
		if in == nil {
			st[0] = stInternal
			return
		}
		args := append([]uint64(nil), st[:n]...)
		for i, t := range h.params { // an i32 arrives with whatever is in the upper bits
			if t == api.ValueTypeI32 {
				args[i] = uint64(uint32(args[i]))
			}
		}
		st[0] = uint64(h.fn(&hcall{in, m, ctx}, args))
	}
}

var hostFuncs = map[string]hostFunc{}

func reg(name string, nparams int, fn func(c *hcall, a []uint64) uint32) {
	ps := make([]api.ValueType, nparams)
	for i := range ps {
		ps[i] = api.ValueTypeI32
	}
	hostFuncs[name] = hostFunc{ps, []api.ValueType{api.ValueTypeI32}, fn}
}

func unimplemented(c *hcall, a []uint64) uint32 { return stUnimplemented }

// read copies n bytes of the plugin's memory.
func (c *hcall) read(ptr, n uint64) ([]byte, bool) {
	b, ok := c.m.Memory().Read(uint32(ptr), uint32(n))
	return bytes.Clone(b), ok
}

func (c *hcall) str(ptr, n uint64) (string, bool) {
	b, ok := c.read(ptr, n)
	return string(b), ok
}

func (c *hcall) u32(ptr uint64, v uint32) bool { return c.m.Memory().WriteUint32Le(uint32(ptr), v) }

// give copies data into memory the plugin allocates, and writes where and
// how long to retPtr and retSize.
func (c *hcall) give(data []byte, retPtr, retSize uint64) uint32 {
	var ptr uint64
	if len(data) > 0 {
		r, err := c.in.fn(c.in.p.mod.alloc).Call(c.ctx, uint64(len(data)))
		if err != nil || len(r) == 0 || r[0] == 0 {
			return stInternal
		}
		ptr = r[0]
		if !c.m.Memory().Write(uint32(ptr), data) {
			return stMemory
		}
	}
	if !c.u32(retPtr, uint32(ptr)) || !c.u32(retSize, uint32(len(data))) {
		return stMemory
	}
	return stOK
}

// stream is the request the plugin is acting on, or nil in the root context.
func (c *hcall) stream() *Stream { return c.in.streams[c.in.effective] }

// headerMap returns the header map of a type, or nil when there is none here.
func (c *hcall) headerMap(t uint64) *[][2]string {
	s := c.stream()
	switch {
	case t > 7:
		return nil
	case t == 6 && c.in.callBack:
		return &c.in.callH
	case t == 7 && c.in.callBack:
		return new([][2]string)
	case s == nil:
		return nil
	case t == 0:
		return &s.reqH
	case t == 2:
		return &s.respH
	case t == 1 || t == 3:
		return new([][2]string) // trailers: none are passed on
	}
	return nil
}

// buffer returns a buffer of a type, or nil when there is none here.
func (c *hcall) buffer(t uint64) *[]byte {
	s := c.stream()
	switch {
	case t == 4 && c.in.callBack:
		return &c.in.callB
	case t == 6:
		return new([]byte)
	case t == 7:
		b := c.in.p.S.Config
		return &b
	case s == nil:
		return nil
	case t == 0:
		return &s.reqB
	case t == 1:
		return &s.respB
	}
	return nil
}

// EncodePairs serializes header pairs the Proxy-Wasm way: the number of
// pairs, each key's and value's length, then each key and value with a NUL.
func EncodePairs(h [][2]string) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, uint32(len(h)))
	for _, kv := range h {
		binary.Write(&b, binary.LittleEndian, uint32(len(kv[0])))
		binary.Write(&b, binary.LittleEndian, uint32(len(kv[1])))
	}
	for _, kv := range h {
		b.WriteString(kv[0])
		b.WriteByte(0)
		b.WriteString(kv[1])
		b.WriteByte(0)
	}
	return b.Bytes()
}

// DecodePairs is the reverse of EncodePairs. Keys come out in lower case.
func DecodePairs(b []byte) ([][2]string, bool) {
	if len(b) < 4 { // an empty map may come as nothing or as a single zero byte
		return nil, len(b) == 0 || len(b) == 1 && b[0] == 0
	}
	n := int(binary.LittleEndian.Uint32(b))
	if n > (len(b)-4)/8 {
		return nil, false
	}
	sizes, data := b[4:4+8*n], b[4+8*n:]
	out := make([][2]string, 0, n)
	for i := range n {
		kl, vl := int(binary.LittleEndian.Uint32(sizes[8*i:])), int(binary.LittleEndian.Uint32(sizes[8*i+4:]))
		if kl+vl+2 > len(data) || data[kl] != 0 || data[kl+1+vl] != 0 {
			return nil, false
		}
		out = append(out, [2]string{strings.ToLower(string(data[:kl])), string(data[kl+1 : kl+1+vl])})
		data = data[kl+vl+2:]
	}
	return out, true
}

func init() {
	reg("proxy_log", 3, func(c *hcall, a []uint64) uint32 {
		msg, ok := c.str(a[1], a[2])
		if !ok {
			return stMemory
		}
		if a[0] >= 2 { // info and up
			level := []string{"", "", "", "warning: ", "error: ", "critical: "}[min(a[0], 5)]
			c.in.p.logf("%s%s", level, msg)
		}
		return stOK
	})
	reg("proxy_get_log_level", 1, func(c *hcall, a []uint64) uint32 {
		if !c.u32(a[0], 2) {
			return stMemory
		}
		return stOK
	})
	reg("proxy_get_current_time_nanoseconds", 1, func(c *hcall, a []uint64) uint32 {
		if !c.m.Memory().WriteUint64Le(uint32(a[0]), uint64(time.Now().UnixNano())) {
			return stMemory
		}
		return stOK
	})
	reg("proxy_set_tick_period_milliseconds", 1, func(c *hcall, a []uint64) uint32 {
		c.in.setTick(uint32(a[0]))
		return stOK
	})
	reg("proxy_set_effective_context", 1, func(c *hcall, a []uint64) uint32 {
		id := uint32(a[0])
		if id != rootID && c.in.streams[id] == nil {
			return stBadArgument
		}
		c.in.effective = id
		return stOK
	})
	reg("proxy_done", 0, func(c *hcall, a []uint64) uint32 { return stOK })

	// Buffers and headers.
	reg("proxy_get_buffer_bytes", 5, func(c *hcall, a []uint64) uint32 {
		b := c.buffer(a[0])
		switch {
		case a[0] > 8:
			return stBadArgument
		case b == nil:
			return stNotFound
		}
		start := min(int(a[1]), len(*b))
		end := min(start+int(a[2]), len(*b))
		return c.give((*b)[start:end], a[3], a[4])
	})
	reg("proxy_get_buffer_status", 3, func(c *hcall, a []uint64) uint32 {
		b := c.buffer(a[0])
		if b == nil {
			return stNotFound
		}
		if !c.u32(a[1], uint32(len(*b))) || !c.u32(a[2], 0) {
			return stMemory
		}
		return stOK
	})
	reg("proxy_set_buffer_bytes", 5, func(c *hcall, a []uint64) uint32 {
		b := c.buffer(a[0])
		switch {
		case a[0] > 1: // only the request and response bodies can change
			return stBadArgument
		case b == nil:
			return stNotFound
		}
		val, ok := c.read(a[3], a[4])
		if !ok {
			return stMemory
		}
		start := min(int(a[1]), len(*b))
		end := min(start+int(a[2]), len(*b))
		nb := slices.Concat((*b)[:start], val, (*b)[end:])
		if max := c.in.p.S.BodyMax; max > 0 && int64(len(nb)) > max {
			return stBadArgument
		}
		*b = nb
		return stOK
	})
	reg("proxy_get_header_map_pairs", 3, func(c *hcall, a []uint64) uint32 {
		h := c.headerMap(a[0])
		if h == nil {
			return stNotFound
		}
		return c.give(EncodePairs(*h), a[1], a[2])
	})
	reg("proxy_get_header_map_size", 2, func(c *hcall, a []uint64) uint32 {
		h := c.headerMap(a[0])
		if h == nil {
			return stNotFound
		}
		if !c.u32(a[1], uint32(len(EncodePairs(*h)))) {
			return stMemory
		}
		return stOK
	})
	reg("proxy_set_header_map_pairs", 3, func(c *hcall, a []uint64) uint32 {
		h := c.headerMap(a[0])
		if h == nil || a[0] > 3 {
			return stNotFound
		}
		b, ok := c.read(a[1], a[2])
		if !ok {
			return stMemory
		}
		pairs, ok := DecodePairs(b)
		if !ok {
			return stSerialization
		}
		*h = pairs
		return stOK
	})
	reg("proxy_get_header_map_value", 5, func(c *hcall, a []uint64) uint32 {
		h := c.headerMap(a[0])
		key, ok := c.str(a[1], a[2])
		switch {
		case h == nil:
			return stNotFound
		case !ok:
			return stMemory
		}
		key = strings.ToLower(key)
		for _, kv := range *h {
			if kv[0] == key {
				return c.give([]byte(kv[1]), a[3], a[4])
			}
		}
		return stNotFound
	})
	setValue := func(replace bool) func(c *hcall, a []uint64) uint32 {
		return func(c *hcall, a []uint64) uint32 {
			h := c.headerMap(a[0])
			key, ok1 := c.str(a[1], a[2])
			val, ok2 := c.str(a[3], a[4])
			switch {
			case h == nil || a[0] > 3:
				return stNotFound
			case !ok1 || !ok2:
				return stMemory
			}
			key = strings.ToLower(key)
			if replace {
				i := slices.IndexFunc(*h, func(kv [2]string) bool { return kv[0] == key })
				*h = slices.DeleteFunc(*h, func(kv [2]string) bool { return kv[0] == key })
				if i >= 0 {
					*h = slices.Insert(*h, min(i, len(*h)), [2]string{key, val})
					return stOK
				}
			}
			*h = append(*h, [2]string{key, val})
			return stOK
		}
	}
	reg("proxy_add_header_map_value", 5, setValue(false))
	reg("proxy_replace_header_map_value", 5, setValue(true))
	reg("proxy_remove_header_map_value", 3, func(c *hcall, a []uint64) uint32 {
		h := c.headerMap(a[0])
		key, ok := c.str(a[1], a[2])
		switch {
		case h == nil || a[0] > 3:
			return stNotFound
		case !ok:
			return stMemory
		}
		key = strings.ToLower(key)
		*h = slices.DeleteFunc(*h, func(kv [2]string) bool { return kv[0] == key })
		return stOK
	})

	// The request's flow.
	goOn := func(c *hcall, a []uint64) uint32 {
		s := c.stream()
		if s == nil {
			return stNotFound
		}
		s.goAhd = true
		s.wake()
		return stOK
	}
	reg("proxy_continue_stream", 1, goOn)
	reg("proxy_continue_request", 0, goOn)
	reg("proxy_continue_response", 0, goOn)
	reg("proxy_resume_http_request", 0, goOn)
	reg("proxy_resume_http_response", 0, goOn)
	closeStream := func(c *hcall, a []uint64) uint32 {
		s := c.stream()
		if s == nil {
			return stNotFound
		}
		s.closed = true
		s.wake()
		return stOK
	}
	reg("proxy_close_stream", 1, closeStream)
	reg("proxy_reset_http_request", 0, closeStream)
	reg("proxy_reset_http_response", 0, closeStream)
	reg("proxy_clear_route_cache", 0, func(c *hcall, a []uint64) uint32 { return stOK })
	reg("proxy_send_local_response", 8, func(c *hcall, a []uint64) uint32 {
		s := c.stream()
		if s == nil {
			return stNotFound
		}
		details, ok1 := c.str(a[1], a[2])
		body, ok2 := c.read(a[3], a[4])
		hb, ok3 := c.read(a[5], a[6])
		if !ok1 || !ok2 || !ok3 {
			return stMemory
		}
		h, ok := DecodePairs(hb)
		if !ok {
			return stSerialization
		}
		if a[0] < 200 || a[0] > 599 {
			return stBadArgument
		}
		s.local = &Local{Status: int(a[0]), Details: details, Headers: h, Body: body}
		s.wake()
		return stOK
	})
	reg("proxy_http_call", 10, httpCall)
	reg("proxy_dispatch_http_call", 10, httpCall)

	// Properties.
	reg("proxy_get_property", 4, func(c *hcall, a []uint64) uint32 {
		b, ok := c.read(a[0], a[1])
		if !ok {
			return stMemory
		}
		var parts []string
		for _, p := range strings.Split(string(b), "\x00") {
			if p != "" {
				parts = append(parts, p)
			}
		}
		key := strings.Join(parts, ".")
		switch key {
		case "plugin_name", "plugin_root_id", "plugin_vm_id":
			return c.give([]byte(c.in.p.S.Name), a[2], a[3])
		}
		if s := c.stream(); s != nil {
			if v, ok := s.props[key]; ok {
				return c.give([]byte(v), a[2], a[3])
			}
			if v, ok := s.ints[key]; ok {
				return c.give(binary.LittleEndian.AppendUint64(nil, uint64(v)), a[2], a[3])
			}
		}
		return stNotFound
	})
	reg("proxy_set_property", 4, func(c *hcall, a []uint64) uint32 {
		s := c.stream()
		path, ok1 := c.str(a[0], a[1])
		val, ok2 := c.str(a[2], a[3])
		switch {
		case s == nil:
			return stNotFound
		case !ok1 || !ok2:
			return stMemory
		}
		if s.props == nil {
			s.props = map[string]string{}
		}
		s.props[strings.ReplaceAll(strings.Trim(path, "\x00"), "\x00", ".")] = val
		return stOK
	})

	// Shared data, kept for all of a plugin's instances.
	reg("proxy_get_shared_data", 5, func(c *hcall, a []uint64) uint32 {
		key, ok := c.str(a[0], a[1])
		if !ok {
			return stMemory
		}
		p := c.in.p
		p.mu.Lock()
		v, found := p.data[key]
		p.mu.Unlock()
		if !found {
			return stNotFound
		}
		if st := c.give(v.val, a[2], a[3]); st != stOK {
			return st
		}
		if !c.u32(a[4], v.cas) {
			return stMemory
		}
		return stOK
	})
	reg("proxy_set_shared_data", 5, func(c *hcall, a []uint64) uint32 {
		key, ok1 := c.str(a[0], a[1])
		val, ok2 := c.read(a[2], a[3])
		if !ok1 || !ok2 {
			return stMemory
		}
		p := c.in.p
		p.mu.Lock()
		defer p.mu.Unlock()
		cur, found := p.data[key]
		if cas := uint32(a[4]); cas != 0 && cas != cur.cas {
			return stCASMismatch
		}
		// At most 64 MB in all, and 1 MB a value, kept in BareProxy's memory.
		p.dataSize += len(key) + len(val) - len(cur.val)
		if !found {
			p.dataSize += len(key)
		}
		if len(val) > 1<<20 || p.dataSize > 64<<20 {
			p.dataSize -= len(key) + len(val) - len(cur.val)
			if !found {
				p.dataSize -= len(key)
			}
			return stBadArgument
		}
		p.data[key] = shared{val, cur.cas + 1}
		return stOK
	})

	// Metrics, kept in memory.
	reg("proxy_define_metric", 4, func(c *hcall, a []uint64) uint32 {
		name, ok := c.str(a[1], a[2])
		if !ok {
			return stMemory
		}
		if a[0] > 2 || len(name) > 256 {
			return stBadArgument
		}
		p := c.in.p
		p.mu.Lock()
		i := slices.IndexFunc(p.mets, func(m *metric) bool { return m.name == name })
		if i < 0 && len(p.mets) >= 1000 {
			p.mu.Unlock()
			return stBadArgument
		}
		if i < 0 {
			i = len(p.mets)
			p.mets = append(p.mets, &metric{name: name, kind: uint32(a[0])})
		}
		p.mu.Unlock()
		if !c.u32(a[3], uint32(i+1)) {
			return stMemory
		}
		return stOK
	})
	metricFn := func(f func(m *metric, v int64) bool) hostFunc {
		return hostFunc{[]api.ValueType{api.ValueTypeI32, api.ValueTypeI64}, []api.ValueType{api.ValueTypeI32},
			func(c *hcall, a []uint64) uint32 {
				m := c.in.p.metric(a[0])
				if m == nil {
					return stNotFound
				}
				if !f(m, int64(a[1])) {
					return stBadArgument
				}
				return stOK
			}}
	}
	hostFuncs["proxy_increment_metric"] = metricFn(func(m *metric, v int64) bool {
		if v < 0 && m.kind == 0 { // a counter only goes up
			return false
		}
		m.val.Add(v)
		return true
	})
	hostFuncs["proxy_record_metric"] = metricFn(func(m *metric, v int64) bool {
		if m.kind == 0 {
			return false
		}
		m.val.Store(v)
		return true
	})
	reg("proxy_get_metric", 2, func(c *hcall, a []uint64) uint32 {
		m := c.in.p.metric(a[0])
		switch {
		case m == nil:
			return stNotFound
		case !c.m.Memory().WriteUint64Le(uint32(a[1]), uint64(m.val.Load())):
			return stMemory
		}
		return stOK
	})

	reg("proxy_call_foreign_function", 6, foreign)

	// Not built: shared queues, gRPC and the status of a gRPC call.
	for name, n := range map[string]int{
		"proxy_register_shared_queue": 3, "proxy_resolve_shared_queue": 5, "proxy_enqueue_shared_queue": 3,
		"proxy_dequeue_shared_queue": 3, "proxy_grpc_call": 12, "proxy_grpc_stream": 9, "proxy_grpc_send": 4,
		"proxy_grpc_cancel": 1, "proxy_grpc_close": 1, "proxy_get_status": 3,
	} {
		reg(name, n, unimplemented)
	}
}

func (p *Plugin) metric(id uint64) *metric {
	p.mu.Lock()
	defer p.mu.Unlock()
	if id < 1 || int(id) > len(p.mets) {
		return nil
	}
	return p.mets[id-1]
}

// Metrics returns the plugin's metrics by name.
func (p *Plugin) Metrics() map[string]int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]int64{}
	for _, m := range p.mets {
		out[m.name] = m.val.Load()
	}
	return out
}

// httpCall sends a request to an address the config allows, and hands the
// response to proxy_on_http_call_response when it comes.
func httpCall(c *hcall, a []uint64) uint32 {
	up, ok1 := c.str(a[0], a[1])
	hb, ok2 := c.read(a[2], a[3])
	body, ok3 := c.read(a[4], a[5])
	if !ok1 || !ok2 || !ok3 {
		return stMemory
	}
	p, in := c.in.p, c.in
	if !slices.Contains(p.S.AllowHTTP, up) {
		p.logf("an outgoing call to %s was refused: no allow-http line names it", up)
		if s := c.stream(); s != nil && len(s.notes) < 20 {
			s.notes = append(s.notes, "call to "+up+" refused: not allowed")
		}
		return stBadArgument
	}
	h, ok := DecodePairs(hb)
	if !ok {
		return stSerialization
	}
	get := func(k, def string) string {
		for _, kv := range h {
			if kv[0] == k {
				return kv[1]
			}
		}
		return def
	}
	scheme, path := get(":scheme", "http"), get(":path", "/")
	u, err := url.ParseRequestURI(path)
	if (scheme != "http" && scheme != "https") || err != nil || !strings.HasPrefix(path, "/") || u.Host != "" {
		return stBadArgument
	}
	u.Scheme, u.Host = scheme, up // the address is the allowed one, whatever the path says
	if p.inCall.Add(1) > 64 {
		p.inCall.Add(-1)
		return stBadArgument // too many calls at once
	}
	timeout := min(time.Duration(a[8])*time.Millisecond, time.Minute)
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	req, err := http.NewRequestWithContext(ctx, get(":method", "GET"), u.String(), bytes.NewReader(body))
	if err != nil || req.URL.Host != up {
		cancel()
		p.inCall.Add(-1)
		return stBadArgument
	}
	req.Host = get(":authority", up)
	for _, kv := range h {
		if !strings.HasPrefix(kv[0], ":") {
			req.Header.Add(kv[0], kv[1])
		}
	}
	id := p.calls.Add(1)
	if !c.u32(a[9], id) {
		cancel()
		p.inCall.Add(-1)
		return stMemory
	}
	go func() {
		defer cancel()
		defer p.inCall.Add(-1)
		var rh [][2]string
		var rb []byte
		if resp, err := p.client.Do(req); err == nil {
			rb, _ = io.ReadAll(io.LimitReader(resp.Body, max(p.S.BodyMax, 1<<20)))
			resp.Body.Close()
			rh = append(rh, [2]string{":status", strconv.Itoa(resp.StatusCode)})
			for k, vs := range resp.Header {
				for _, v := range vs {
					rh = append(rh, [2]string{strings.ToLower(k), v})
				}
			}
		}
		in.mu.Lock()
		defer in.mu.Unlock()
		if in.broken != nil || !p.mod.Has("proxy_on_http_call_response") {
			return
		}
		in.callH, in.callB, in.callBack = rh, rb, true
		in.effective = rootID
		in.call("proxy_on_http_call_response", rootID, uint64(id), uint64(len(rh)), uint64(len(rb)), 0)
		in.callH, in.callB, in.callBack = nil, nil, false
	}()
	return stOK
}
