// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

// Command testapi is a small backend for trying BareProxy. It answers
// /healthz, echoes back what it received, and echoes WebSocket messages
// at /ws.
package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: testapi NAME ADDR")
		os.Exit(2)
	}
	name, addr := os.Args[1], os.Args[2]
	fmt.Fprintln(os.Stderr, http.ListenAndServe(addr, newMux(name)))
	os.Exit(1)
}

func newMux(name string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/ws", wsEcho)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"backend":%q,"path":%q,"host":%q,"x_forwarded_for":%q,"x_forwarded_proto":%q,"bareproxy_id":%q}`+"\n",
			name, r.URL.RequestURI(), r.Host, r.Header.Get("X-Forwarded-For"), r.Header.Get("X-Forwarded-Proto"), r.Header.Get("BareProxy-Id"))
	})
	return mux
}

// wsEcho answers a WebSocket handshake and sends every text or binary message
// back until the client closes. It takes only single-frame messages with no
// extensions, which is all the load test sends.
func wsEcho(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || key == "" {
		http.Error(w, "this is a WebSocket endpoint", http.StatusBadRequest)
		return
	}
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer conn.Close()
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
		base64.StdEncoding.EncodeToString(sum[:]))
	if rw.Flush() != nil {
		return
	}
	for {
		op, msg, err := readFrame(rw.Reader)
		if err != nil {
			return
		}
		switch op {
		case 0x1, 0x2: // text, binary: send it back
			err = writeFrame(rw, op, msg)
		case 0x9: // ping: pong with the same bytes
			err = writeFrame(rw, 0xA, msg)
		case 0x8: // close: say goodbye and stop
			writeFrame(rw, 0x8, msg)
			return
		case 0xA: // pong: nothing to do
		default: // fragments and anything else are not supported
			return
		}
		if err != nil {
			return
		}
	}
}

// readFrame reads one frame from a client: the opcode and the unmasked payload.
func readFrame(r *bufio.Reader) (op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(r, h[:]); err != nil {
		return
	}
	op = h[0] & 0x0f
	if h[0]&0x80 == 0 || h[1]&0x80 == 0 {
		return 0, nil, fmt.Errorf("fragmented or unmasked frame")
	}
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(r, b[:]); err == nil {
			n = uint64(binary.BigEndian.Uint16(b[:]))
		}
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(r, b[:]); err == nil {
			n = binary.BigEndian.Uint64(b[:])
		}
	}
	if err != nil || n > 1<<20 {
		return 0, nil, fmt.Errorf("bad frame length")
	}
	var mask [4]byte
	if _, err = io.ReadFull(r, mask[:]); err != nil {
		return
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(r, payload); err != nil {
		return
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return
}

// writeFrame sends one unmasked frame, as a server must.
func writeFrame(w *bufio.ReadWriter, op byte, payload []byte) error {
	h := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		h = append(h, byte(n))
	case n <= 0xffff:
		h = append(h, 126, byte(n>>8), byte(n))
	default:
		h = binary.BigEndian.AppendUint64(append(h, 127), uint64(n))
	}
	w.Write(h)
	w.Write(payload)
	return w.Flush()
}
