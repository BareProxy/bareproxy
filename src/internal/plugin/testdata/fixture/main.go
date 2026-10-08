// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//go:build wasip1

// The test plugin: a Proxy-Wasm plugin written straight to the ABI, with no
// SDK, so the tests need nothing outside this repository. What it does is set
// by its config, a list of words:
//
//	headers  add x-plugin to the request, and note the request's path
//	deny     answer 403 with "denied by plugin" and an x-denied header
//	resp     add x-resp to the response, and upper-case the response body
//	call     call 127.0.0.1:PORT/ (PORT from the request's x-call-port),
//	         wait, then add x-called with the call's status
//	store    put the path in the store, read it back into x-stored
//	read     put the file hello.txt into x-read
//	count    count requests in shared data, into x-count
//	log      note "logged" in proxy_on_log
//	tick     set a 10 ms timer that counts ticks in shared data
//	loop     loop forever in proxy_on_request_headers
//	crash    panic in proxy_on_request_headers
//	grow     allocate 48 MB in proxy_on_request_headers
//	refuse   refuse the config
//	hold     pause the request and never go on
//	crashresp  panic in proxy_on_response_headers
//	denyresp   answer 451 from proxy_on_response_headers
//	partial  add x-partial to the request, then panic
//	setprop  set the property source.address to 6.6.6.6
//	flaky    refuse to start any instance after the first
//	sleep    sleep for 2 seconds in proxy_on_request_headers
package main

import (
	"encoding/binary"
	"strconv"
	"strings"
	"time"
	"unsafe"
)

func main() {}

var modes = map[string]bool{}

var keep = map[uintptr][]byte{} // memory handed to the host stays alive

//go:wasmexport proxy_abi_version_0_2_1
func abiVersion() {}

//go:wasmexport proxy_on_memory_allocate
func allocate(size uint32) uint32 {
	b := make([]byte, size)
	p := uintptr(unsafe.Pointer(&b[0]))
	keep[p] = b
	return uint32(p)
}

//go:wasmimport env proxy_log
func proxyLog(level uint32, msg unsafe.Pointer, size uint32) uint32

//go:wasmimport env proxy_get_buffer_bytes
func proxyGetBufferBytes(kind, start, max uint32, ret *uint32, retSize *uint32) uint32

//go:wasmimport env proxy_set_buffer_bytes
func proxySetBufferBytes(kind, start, size uint32, data unsafe.Pointer, dataSize uint32) uint32

//go:wasmimport env proxy_get_header_map_value
func proxyGetHeaderMapValue(kind uint32, key unsafe.Pointer, keySize uint32, ret *uint32, retSize *uint32) uint32

//go:wasmimport env proxy_add_header_map_value
func proxyAddHeaderMapValue(kind uint32, key unsafe.Pointer, keySize uint32, val unsafe.Pointer, valSize uint32) uint32

//go:wasmimport env proxy_replace_header_map_value
func proxyReplaceHeaderMapValue(kind uint32, key unsafe.Pointer, keySize uint32, val unsafe.Pointer, valSize uint32) uint32

//go:wasmimport env proxy_send_local_response
func proxySendLocalResponse(status uint32, details unsafe.Pointer, detailsSize uint32, body unsafe.Pointer, bodySize uint32, headers unsafe.Pointer, headersSize uint32, grpc int32) uint32

//go:wasmimport env proxy_get_property
func proxyGetProperty(path unsafe.Pointer, pathSize uint32, ret *uint32, retSize *uint32) uint32

//go:wasmimport env proxy_call_foreign_function
func proxyCallForeignFunction(name unsafe.Pointer, nameSize uint32, args unsafe.Pointer, argsSize uint32, ret *uint32, retSize *uint32) uint32

//go:wasmimport env proxy_http_call
func proxyHTTPCall(up unsafe.Pointer, upSize uint32, h unsafe.Pointer, hSize uint32, body unsafe.Pointer, bodySize uint32, tr unsafe.Pointer, trSize uint32, timeout uint32, ret *uint32) uint32

//go:wasmimport env proxy_set_effective_context
func proxySetEffectiveContext(id uint32) uint32

//go:wasmimport env proxy_continue_stream
func proxyContinueStream(kind uint32) uint32

//go:wasmimport env proxy_get_shared_data
func proxyGetSharedData(key unsafe.Pointer, keySize uint32, ret *uint32, retSize *uint32, cas *uint32) uint32

//go:wasmimport env proxy_set_shared_data
func proxySetSharedData(key unsafe.Pointer, keySize uint32, val unsafe.Pointer, valSize uint32, cas uint32) uint32

//go:wasmimport env proxy_set_property
func proxySetProperty(path unsafe.Pointer, pathSize uint32, val unsafe.Pointer, valSize uint32) uint32

//go:wasmimport env proxy_set_tick_period_milliseconds
func proxySetTickPeriod(ms uint32) uint32

func ptr(s string) (unsafe.Pointer, uint32) {
	return unsafe.Pointer(unsafe.StringData(s)), uint32(len(s))
}

func bptr(b []byte) (unsafe.Pointer, uint32) {
	if len(b) == 0 {
		return nil, 0
	}
	return unsafe.Pointer(&b[0]), uint32(len(b))
}

func take(p, n uint32) []byte {
	if n == 0 {
		return nil
	}
	return append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(uintptr(p))), n)...)
}

func logf(s string) {
	p, n := ptr(s)
	proxyLog(2, p, n)
}

func buffer(kind uint32) []byte {
	var p, n uint32
	if proxyGetBufferBytes(kind, 0, 1<<30, &p, &n) != 0 {
		return nil
	}
	return take(p, n)
}

func header(kind uint32, k string) string {
	var p, n uint32
	kp, kn := ptr(k)
	if proxyGetHeaderMapValue(kind, kp, kn, &p, &n) != 0 {
		return ""
	}
	return string(take(p, n))
}

func addHeader(kind uint32, k, v string) {
	kp, kn := ptr(k)
	vp, vn := ptr(v)
	if st := proxyAddHeaderMapValue(kind, kp, kn, vp, vn); st != 0 {
		logf("add header " + k + ": status " + strconv.Itoa(int(st)))
	}
}

func property(parts ...string) string {
	path := strings.Join(parts, "\x00")
	var p, n uint32
	pp, pn := ptr(path)
	if proxyGetProperty(pp, pn, &p, &n) != 0 {
		return ""
	}
	return string(take(p, n))
}

func foreign(name string, args []byte) ([]byte, uint32) {
	np, nn := ptr(name)
	ap, an := bptr(args)
	var p, n uint32
	st := proxyCallForeignFunction(np, nn, ap, an, &p, &n)
	return take(p, n), st
}

func note(s string) { foreign("bareproxy_note", []byte(s)) }

// count adds one to a shared-data counter and returns the new value.
func count(key string) int {
	kp, kn := ptr(key)
	for {
		var p, n, cas uint32
		v := 0
		if proxyGetSharedData(kp, kn, &p, &n, &cas) == 0 {
			v, _ = strconv.Atoi(string(take(p, n)))
		}
		nv := []byte(strconv.Itoa(v + 1))
		bp, bn := bptr(nv)
		if proxySetSharedData(kp, kn, bp, bn, cas) == 0 {
			return v + 1
		}
	}
}

func encodePairs(h [][2]string) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(len(h)))
	for _, kv := range h {
		b = binary.LittleEndian.AppendUint32(b, uint32(len(kv[0])))
		b = binary.LittleEndian.AppendUint32(b, uint32(len(kv[1])))
	}
	for _, kv := range h {
		b = append(append(append(append(b, kv[0]...), 0), kv[1]...), 0)
	}
	return b
}

//go:wasmexport proxy_on_context_create
func onContextCreate(id, parent uint32) {}

//go:wasmexport proxy_on_vm_start
func onVMStart(root, size uint32) uint32 { return 1 }

//go:wasmexport proxy_on_configure
func onConfigure(root, size uint32) uint32 {
	for _, w := range strings.Fields(string(buffer(7))) {
		modes[w] = true
	}
	if modes["tick"] {
		proxySetTickPeriod(10)
	}
	if modes["refuse"] {
		return 0
	}
	if modes["flaky"] && count("starts") > 1 {
		panic("the test plugin won't start again, as asked")
	}
	return 1
}

//go:wasmexport proxy_on_tick
func onTick(root uint32) { count("ticks") }

var waiting = map[uint32]uint32{} // call ID -> request context

var sink []byte

//go:wasmexport proxy_on_request_headers
func onRequestHeaders(id, n, eos uint32) uint32 {
	switch {
	case modes["loop"]:
		for {
		}
	case modes["crash"]:
		panic("the test plugin crashes, as asked")
	case modes["grow"]:
		sink = make([]byte, 48<<20)
		sink[len(sink)-1] = 1
	case modes["partial"]:
		addHeader(0, "x-partial", "1")
		panic("the test plugin crashes halfway, as asked")
	case modes["sleep"]:
		time.Sleep(2 * time.Second)
	}
	if modes["setprop"] {
		pp, pn := ptr("source\x00address")
		vp, vn := ptr("6.6.6.6")
		proxySetProperty(pp, pn, vp, vn)
	}
	if modes["headers"] {
		addHeader(0, "x-plugin", "hello")
		note("saw " + header(0, ":method") + " " + property("request", "path") + " from " + property("source", "address"))
	}
	if modes["count"] {
		addHeader(0, "x-count", strconv.Itoa(count("requests")))
	}
	if modes["store"] {
		path := property("request", "path")
		key := "last"
		args := binary.LittleEndian.AppendUint32(nil, uint32(len(key)))
		args = append(append(args, key...), path...)
		foreign("bareproxy_store_put", args)
		v, _ := foreign("bareproxy_store_get", []byte(key))
		addHeader(0, "x-stored", string(v))
	}
	if modes["read"] {
		v, st := foreign("bareproxy_read_file", []byte("hello.txt"))
		addHeader(0, "x-read", strings.TrimSpace(string(v))+" "+strconv.Itoa(int(st)))
	}
	if modes["deny"] {
		body := []byte("denied by plugin\n")
		h := encodePairs([][2]string{{"x-denied", "1"}, {"content-type", "text/plain"}})
		dp, dn := ptr("test_deny")
		bp, bn := bptr(body)
		hp, hn := bptr(h)
		proxySendLocalResponse(403, dp, dn, bp, bn, hp, hn, -1)
		return 1
	}
	if modes["call"] {
		up := "127.0.0.1:" + header(0, "x-call-port")
		h := encodePairs([][2]string{{":method", "GET"}, {":path", "/called"}, {":authority", "callee"}})
		upp, upn := ptr(up)
		hp, hn := bptr(h)
		var token uint32
		if st := proxyHTTPCall(upp, upn, hp, hn, nil, 0, nil, 0, 2000, &token); st != 0 {
			addHeader(0, "x-called", "refused "+strconv.Itoa(int(st)))
			return 0
		}
		waiting[token] = id
		return 1
	}
	if modes["hold"] {
		return 1
	}
	return 0
}

//go:wasmexport proxy_on_http_call_response
func onHTTPCallResponse(root, token, nh, size, nt uint32) {
	id := waiting[token]
	delete(waiting, token)
	status := header(6, ":status")
	body := string(buffer(4))
	proxySetEffectiveContext(id)
	addHeader(0, "x-called", status+" "+strings.TrimSpace(body))
	proxyContinueStream(0)
}

//go:wasmexport proxy_on_response_headers
func onResponseHeaders(id, n, eos uint32) uint32 {
	if modes["crashresp"] {
		panic("the test plugin crashes on the response, as asked")
	}
	if modes["denyresp"] {
		body := []byte("replaced by plugin\n")
		dp, dn := ptr("test_denyresp")
		bp, bn := bptr(body)
		proxySendLocalResponse(451, dp, dn, bp, bn, nil, 0, -1)
		return 1
	}
	if modes["resp"] {
		code := []byte(property("response", "code"))
		if len(code) == 8 {
			addHeader(2, "x-resp", strconv.FormatUint(binary.LittleEndian.Uint64(code), 10))
		}
	}
	return 0
}

//go:wasmexport proxy_on_response_body
func onResponseBody(id, size, eos uint32) uint32 {
	if modes["resp"] {
		up := []byte(strings.ToUpper(string(buffer(1))))
		bp, bn := bptr(up)
		proxySetBufferBytes(1, 0, size, bp, bn)
	}
	return 0
}

//go:wasmexport proxy_on_log
func onLog(id uint32) {
	if modes["log"] {
		note("logged")
	}
}

//go:wasmexport proxy_on_done
func onDone(id uint32) uint32 { return 1 }

//go:wasmexport proxy_on_delete
func onDelete(id uint32) {}
