// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// RFC 6455: the accept value of section 1.3 and the masked "Hello" of 5.7.
func TestRFC6455Samples(t *testing.T) {
	if got := acceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Errorf("accept value %q", got)
	}
	got := encodeFrame(0x1, []byte("Hello"), [4]byte{0x37, 0xfa, 0x21, 0x3d})
	want := []byte{0x81, 0x85, 0x37, 0xfa, 0x21, 0x3d, 0x7f, 0x9f, 0x4d, 0x51, 0x58}
	if string(got) != string(want) {
		t.Errorf("masked Hello: % x, want % x", got, want)
	}
}

func TestFrameLengths(t *testing.T) {
	for _, c := range []struct{ n, header int }{{0, 2}, {125, 2}, {126, 4}, {65535, 4}, {65536, 10}} {
		f := encodeFrame(0x2, make([]byte, c.n), [4]byte{})
		if len(f) != c.header+4+c.n { // header, mask, payload
			t.Errorf("%d bytes: frame of %d, want %d", c.n, len(f), c.header+4+c.n)
		}
	}
}

func TestPercentile(t *testing.T) {
	var d []time.Duration
	for i := 1; i <= 100; i++ {
		d = append(d, time.Duration(i)*time.Millisecond)
	}
	if p := percentile(d, 0.50); p != 50*time.Millisecond {
		t.Errorf("p50 %v", p)
	}
	if p := percentile(d, 0.99); p != 99*time.Millisecond {
		t.Errorf("p99 %v", p)
	}
	if p := percentile(nil, 0.5); p != 0 {
		t.Errorf("empty: %v", p)
	}
}

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		err    error
		reason string
		reset  bool
	}{
		{fmt.Errorf("read: %w", syscall.ECONNRESET), "connection reset", false}, // the connection wrapper counts it
		{io.ErrUnexpectedEOF, "connection closed with no answer (EOF)", true},
		{errClosedByServer, "closed by the server", true},
		{context.DeadlineExceeded, "timeout", false},
		{errors.New("http2: server sent GOAWAY and closed the connection"), "http2 stream or connection error", true},
	} {
		if reason, reset := classify(c.err); reason != c.reason || reset != c.reset {
			t.Errorf("%v: %q, %v; want %q, %v", c.err, reason, reset, c.reason, c.reset)
		}
	}
}

func TestParseFlags(t *testing.T) {
	c, err := parseFlags([]string{"-http1", "http://x", "-rate", "2000"})
	if err != nil || c.split != [3]float64{800, 800, 400} {
		t.Errorf("default split: %v, %v", c.split, err)
	}
	for _, bad := range [][]string{{}, {"-http1", "http://x", "-split", "1,2"}, {"-http1", "http://x", "-paths", "nope"}, {"-http1", "http://x", "-rate", "0"}} {
		if _, err := parseFlags(bad); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
}

// backend answers /hello with the given status and echoes WebSocket messages
// at /ws, dropping the connection after dropAfter messages when that is set.
func backend(status, dropAfter int) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, "hello")
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
			acceptKey(r.Header.Get("Sec-WebSocket-Key")))
		rw.Flush()
		for n := 0; ; n++ {
			var h [2]byte
			var mask [4]byte
			if _, err := io.ReadFull(rw, h[:]); err != nil {
				return
			}
			msg := make([]byte, h[1]&0x7f) // the test's messages are short
			if _, err := io.ReadFull(rw, mask[:]); err != nil {
				return
			}
			if _, err := io.ReadFull(rw, msg); err != nil {
				return
			}
			for i := range msg {
				msg[i] ^= mask[i%4]
			}
			if h[0]&0x0f == 8 || dropAfter > 0 && n >= dropAfter {
				return
			}
			rw.Write(append([]byte{0x80 | h[0]&0x0f, byte(len(msg))}, msg...))
			rw.Flush()
		}
	})
	return mux
}

func TestAllThreeKindsPass(t *testing.T) {
	plain := httptest.NewServer(backend(200, 0))
	defer plain.Close()
	secure := httptest.NewUnstartedServer(backend(200, 0))
	secure.EnableHTTP2 = true
	secure.StartTLS()
	defer secure.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: secure.Certificate().Raw}), 0o644)
	cfg, err := parseFlags([]string{"-http1", plain.URL, "-http2", secure.URL, "-cacert", ca, "-rate", "300", "-duration", "1s", "-grace", "2s",
		"-paths", "/hello=hello", "-ws-conns", "4",
		"-ws", "ws://" + plain.Listener.Addr().String() + "/ws,wss://" + secure.Listener.Addr().String() + "/ws"})
	if err != nil {
		t.Fatal(err)
	}
	sum := run(cfg, io.Discard)
	if !sum.Pass {
		t.Fatalf("not a pass: %+v", sum)
	}
	for kind, want := range map[string]int64{"http1": 120, "http2": 120, "ws": 60} {
		k := sum.Kinds[kind]
		if k.Sent < want*95/100 || k.Answered != k.Sent || k.Failed != 0 {
			t.Errorf("%s: %+v, want about %d sent and all answered", kind, k, want)
		}
	}
}

func TestFailuresAreCounted(t *testing.T) {
	srv := httptest.NewServer(backend(500, 3)) // answers 500, and drops each WebSocket after 3 messages
	defer srv.Close()
	cfg, err := parseFlags([]string{"-http1", srv.URL, "-ws", "ws://" + srv.Listener.Addr().String() + "/ws", "-split", "50,0,50",
		"-rate", "200", "-duration", "1s", "-grace", "1s", "-paths", "/hello=hello", "-ws-conns", "2"})
	if err != nil {
		t.Fatal(err)
	}
	sum := run(cfg, io.Discard)
	if sum.Pass {
		t.Fatalf("a run with 500s and dropped WebSockets passed: %+v", sum)
	}
	if n := sum.Kinds["http1"].Failures["status 500"]; n != sum.Kinds["http1"].Sent {
		t.Errorf("status 500 failures: %d of %d", n, sum.Kinds["http1"].Sent)
	}
	ws := sum.Kinds["ws"]
	if ws.Failed == 0 || ws.ConnsOpened <= 2 || sum.Resets == 0 {
		t.Errorf("dropped WebSockets not counted: %+v, resets %d", ws, sum.Resets)
	}
}
