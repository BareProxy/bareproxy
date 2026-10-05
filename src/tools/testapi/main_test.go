// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAnswers(t *testing.T) {
	srv := httptest.NewServer(newMux("t1"))
	defer srv.Close()
	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/healthz"); code != 200 || body != "ok\n" {
		t.Errorf("/healthz: %d %q", code, body)
	}
	if code, body := get("/a/b?x=1"); code != 200 || !strings.Contains(body, `"backend":"t1"`) || !strings.Contains(body, `"path":"/a/b?x=1"`) {
		t.Errorf("/a/b?x=1: %d %q", code, body)
	}
	if code, _ := get("/ws"); code != http.StatusBadRequest {
		t.Errorf("a plain GET of /ws: status %d, want 400", code)
	}
}

// TestWebSocketEcho uses the sample bytes from RFC 6455: the key and accept
// value of section 1.3, and the masked "Hello" of section 5.7.
func TestWebSocketEcho(t *testing.T) {
	srv := httptest.NewServer(newMux("t1"))
	defer srv.Close()
	dial := func() (net.Conn, *bufio.Reader) {
		c, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprint(c, "GET /ws HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
		br := bufio.NewReader(c)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
			t.Fatalf("handshake: %s, accept %q", resp.Status, resp.Header.Get("Sec-WebSocket-Accept"))
		}
		return c, br
	}
	expect := func(br *bufio.Reader, want []byte) {
		t.Helper()
		got := make([]byte, len(want))
		if _, err := io.ReadFull(br, got); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("got % x (%v), want % x", got, err, want)
		}
	}
	c, br := dial()
	defer c.Close()
	c.Write([]byte{0x81, 0x85, 0x37, 0xfa, 0x21, 0x3d, 0x7f, 0x9f, 0x4d, 0x51, 0x58}) // masked "Hello"
	expect(br, []byte{0x81, 0x05, 'H', 'e', 'l', 'l', 'o'})
	c.Write([]byte{0x89, 0x82, 0, 0, 0, 0, 'h', 'i'}) // ping
	expect(br, []byte{0x8a, 0x02, 'h', 'i'})          // pong
	c.Write([]byte{0x88, 0x80, 1, 2, 3, 4})           // close
	expect(br, []byte{0x88, 0x00})
	if _, err := br.ReadByte(); err != io.EOF {
		t.Errorf("after the close frame: %v, want EOF", err)
	}

	c2, br2 := dial() // a frame that isn't masked ends the connection
	defer c2.Close()
	c2.Write([]byte{0x81, 0x01, 'x'})
	if _, err := br2.ReadByte(); err != io.EOF {
		t.Errorf("after an unmasked frame: %v, want EOF", err)
	}
}
