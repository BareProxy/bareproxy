// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package main

// The WebSocket part of the client, as little as the test needs: the key and
// accept values of the opening handshake, masked frames going out, and frames
// coming in. No extensions, and no fragmented messages.

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

var errClosedByServer = errors.New("closed by the server")

func newKey() string {
	var b [16]byte
	rand.Read(b[:])
	return base64.StdEncoding.EncodeToString(b[:])
}

// acceptKey is the Sec-WebSocket-Accept value that answers a key.
func acceptKey(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// encodeFrame builds one final frame from a client. Clients must mask.
func encodeFrame(op byte, payload []byte, mask [4]byte) []byte {
	b := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		b = append(b, 0x80|byte(n))
	case n <= 0xffff:
		b = append(b, 0x80|126, byte(n>>8), byte(n))
	default:
		b = binary.BigEndian.AppendUint64(append(b, 0x80|127), uint64(n))
	}
	b = append(b, mask[:]...)
	for i, c := range payload {
		b = append(b, c^mask[i%4])
	}
	return b
}

func writeFrame(w io.Writer, op byte, payload []byte) error {
	var mask [4]byte
	rand.Read(mask[:])
	_, err := w.Write(encodeFrame(op, payload, mask))
	return err
}

// readFrame reads one frame from a server: its opcode and payload. Servers
// don't mask, and a message in several frames isn't supported.
func readFrame(r *bufio.Reader) (op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(r, h[:]); err != nil {
		return
	}
	op = h[0] & 0x0f
	if h[0]&0x80 == 0 || h[1]&0x80 != 0 {
		return 0, nil, fmt.Errorf("a fragmented or masked frame from the server")
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
		return 0, nil, fmt.Errorf("a bad frame length")
	}
	payload = make([]byte, n)
	_, err = io.ReadFull(r, payload)
	if err == nil && op == 0x8 {
		err = errClosedByServer
	}
	return
}
