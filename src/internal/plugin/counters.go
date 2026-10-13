// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"encoding/binary"
	"sync"
	"time"
)

// counters are a plugin's windowed counters, behind bareproxy_count: one
// per key, shared by all of the plugin's instances, counting in windows
// aligned to the clock. Each keeps the current window and the one before,
// so a plugin can weigh the two into a sliding count. Counters for windows
// long gone are swept, so a key per client address doesn't pile up.
type counters struct {
	mu    sync.Mutex
	m     map[string]*counter
	calls int
	now   func() time.Time
}

type counter struct {
	win       int64 // the window's length, in milliseconds
	idx       int64 // which window, counted from 1970
	cur, prev uint64
}

// maxCounters caps a plugin's counters; past it, new keys are refused and
// the plugin decides what that means (a rate limiter lets the request on).
const maxCounters = 500_000

// count adds n to key's counter for windows of win, and returns the
// current window's count, the last window's, and how far into the current
// window it is.
func (cs *counters) count(key string, win time.Duration, n uint64) (cur, prev uint64, into time.Duration, ok bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.m == nil {
		cs.m = map[string]*counter{}
	}
	nowMS := cs.now().UnixMilli()
	ms := max(win.Milliseconds(), 1)
	idx := nowMS / ms
	cs.calls++
	if cs.calls%4096 == 0 || len(cs.m) >= maxCounters {
		for k, c := range cs.m {
			if nowMS/c.win > c.idx+1 {
				delete(cs.m, k)
			}
		}
	}
	c := cs.m[key]
	if c == nil || c.win != ms {
		if c == nil && len(cs.m) >= maxCounters {
			return 0, 0, 0, false
		}
		c = &counter{win: ms, idx: idx}
		cs.m[key] = c
	}
	switch {
	case idx == c.idx:
	case idx == c.idx+1:
		c.prev, c.cur = c.cur, 0
	default:
		c.prev, c.cur = 0, 0
	}
	c.idx = idx
	c.cur += n
	return c.cur, c.prev, time.Duration(nowMS-idx*ms) * time.Millisecond, true
}

// countCall is bareproxy_count: args are a 4-byte little-endian window in
// milliseconds, a 4-byte little-endian amount to add, then the key. It
// returns 24 bytes, little-endian: the current window's count, the last
// window's, and the milliseconds into the current window.
func countCall(p *Plugin, args []byte) ([]byte, uint32) {
	if len(args) < 9 || len(args) > 8+1024 {
		return nil, stBadArgument
	}
	win := time.Duration(binary.LittleEndian.Uint32(args)) * time.Millisecond
	n := uint64(binary.LittleEndian.Uint32(args[4:]))
	if win <= 0 {
		return nil, stBadArgument
	}
	cur, prev, into, ok := p.counts.count(string(args[8:]), win, n)
	if !ok {
		return nil, stBadArgument
	}
	out := make([]byte, 24)
	binary.LittleEndian.PutUint64(out, cur)
	binary.LittleEndian.PutUint64(out[8:], prev)
	binary.LittleEndian.PutUint64(out[16:], uint64(into.Milliseconds()))
	return out, stOK
}
