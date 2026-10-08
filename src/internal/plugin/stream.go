// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"errors"
	"fmt"
	"time"
)

// Local is a response a plugin sends itself, in place of the request going on.
type Local struct {
	Status  int
	Details string
	Headers [][2]string
	Body    []byte
}

// Stream is one request's context in a plugin. A request keeps to one
// instance from its first callback to its last.
type Stream struct {
	in     *instance
	id     uint32
	props  map[string]string
	ints   map[string]int64
	reqH   [][2]string
	respH  [][2]string
	reqB   []byte
	respB  []byte
	local  *Local
	closed bool
	goAhd  bool // the plugin said to go on (proxy_continue_stream)
	notes  []string
	resume chan struct{}
}

// Phases a stream goes through, for Headers and Body.
const (
	Request  = 0
	Response = 1
)

// ErrNoInstance is returned when every instance of a plugin is broken.
var ErrNoInstance = errors.New("no instance of the plugin is working")

// NewStream makes a request's context. props are its string properties
// (request.path, source.address and so on) and ints its integer ones.
func (p *Plugin) NewStream(props map[string]string, ints map[string]int64) (*Stream, error) {
	insts := p.instances()
	start := p.next.Add(1)
	for i := range insts {
		in := insts[(int(start)+i)%len(insts)]
		in.mu.Lock()
		if in.broken != nil {
			in.mu.Unlock()
			continue
		}
		in.nextID++
		s := &Stream{in: in, id: in.nextID, props: props, ints: ints, resume: make(chan struct{}, 1)}
		in.streams[s.id] = s
		in.effective = s.id
		_, err := in.call("proxy_on_context_create", uint64(s.id), rootID)
		in.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return s, nil
	}
	return nil, ErrNoInstance
}

// Wants reports whether the plugin has a callback, such as
// proxy_on_response_body, so the server knows what to hand it.
func (p *Plugin) Wants(callback string) bool { return p.mod.Has(callback) }

// Headers runs proxy_on_request_headers or proxy_on_response_headers and
// returns the headers as the plugin left them. If the plugin pauses the
// request, Headers waits for it to go on, up to the pause limit. After it
// returns, Local and Closed say whether the plugin answered or closed the
// request instead.
func (s *Stream) Headers(phase int, h [][2]string, endOfStream bool) ([][2]string, error) {
	name := "proxy_on_request_headers"
	if phase == Response {
		name = "proxy_on_response_headers"
	}
	in := s.in
	in.mu.Lock()
	if phase == Request {
		s.reqH = h
	} else {
		s.respH = h
	}
	err := s.run(name, uint64(len(h)), boolArg(endOfStream))
	if phase == Request {
		h = s.reqH
	} else {
		h = s.respH
	}
	in.mu.Unlock()
	return h, err
}

// Body runs proxy_on_request_body or proxy_on_response_body with the whole
// body, and returns the body as the plugin left it.
func (s *Stream) Body(phase int, b []byte) ([]byte, error) {
	name := "proxy_on_request_body"
	if phase == Response {
		name = "proxy_on_response_body"
	}
	in := s.in
	in.mu.Lock()
	if phase == Request {
		s.reqB = b
	} else {
		s.respB = b
	}
	err := s.run(name, uint64(len(b)), 1)
	if phase == Request {
		b = s.reqB
	} else {
		b = s.respB
	}
	in.mu.Unlock()
	return b, err
}

// run makes one callback, and waits while the plugin keeps the request
// paused. The caller holds in.mu; it is let go while waiting.
func (s *Stream) run(name string, a, b uint64) error {
	in := s.in
	if !in.p.mod.Has(name) {
		return nil
	}
	select { // a stale go-ahead from an earlier phase
	case <-s.resume:
	default:
	}
	s.goAhd = false
	in.effective = s.id
	action, err := in.call(name, uint64(s.id), a, b)
	if err != nil || action == 0 || s.local != nil || s.closed {
		return err
	}
	in.mu.Unlock()
	t := time.NewTimer(in.p.S.Pause)
	select {
	case <-s.resume:
	case <-t.C:
	}
	t.Stop()
	in.mu.Lock()
	switch {
	case in.broken != nil:
		return in.broken
	case s.local == nil && !s.closed && !s.goAhd:
		return fmt.Errorf("kept the request waiting past its limit of %s", in.p.S.Pause)
	}
	return nil
}

// wake tells a waiting request to look again.
func (s *Stream) wake() {
	select {
	case s.resume <- struct{}{}:
	default:
	}
}

// Local returns the response the plugin sent itself, or nil.
func (s *Stream) Local() *Local {
	s.in.mu.Lock()
	defer s.in.mu.Unlock()
	return s.local
}

// Closed reports whether the plugin asked to close the request.
func (s *Stream) Closed() bool {
	s.in.mu.Lock()
	defer s.in.mu.Unlock()
	return s.closed
}

// Notes returns what the plugin wrote with bareproxy_note.
func (s *Stream) Notes() []string {
	s.in.mu.Lock()
	defer s.in.mu.Unlock()
	return append([]string(nil), s.notes...)
}

// SetInt sets an integer property, such as response.code, before a callback.
func (s *Stream) SetInt(name string, v int64) {
	s.in.mu.Lock()
	defer s.in.mu.Unlock()
	if s.ints == nil {
		s.ints = map[string]int64{}
	}
	s.ints[name] = v
}

// Done ends the stream once the response is sent: proxy_on_done, then
// proxy_on_log when log is set (a plugin that failed with on-error open
// gets no log call), then proxy_on_delete. It returns the error of the log
// call, if any.
func (s *Stream) Done(log bool) error {
	in := s.in
	in.mu.Lock()
	defer in.mu.Unlock()
	defer delete(in.streams, s.id)
	if in.broken != nil {
		return nil
	}
	in.effective = s.id
	if _, err := in.call("proxy_on_done", uint64(s.id)); err != nil {
		return nil
	}
	var err error
	if log && in.p.mod.Has("proxy_on_log") {
		in.effective = s.id
		if _, err = in.call("proxy_on_log", uint64(s.id)); err != nil {
			return err
		}
	}
	in.effective = s.id
	in.call("proxy_on_delete", uint64(s.id))
	return err
}

func boolArg(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}
