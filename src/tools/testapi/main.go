// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

// Command testapi is a small backend for trying BareProxy. It answers
// /healthz and echoes back what it received.
package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: testapi NAME ADDR")
		os.Exit(2)
	}
	name, addr := os.Args[1], os.Args[2]
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"backend":%q,"path":%q,"host":%q,"x_forwarded_for":%q,"x_forwarded_proto":%q,"bareproxy_id":%q}`+"\n",
			name, r.URL.RequestURI(), r.Host, r.Header.Get("X-Forwarded-For"), r.Header.Get("X-Forwarded-Proto"), r.Header.Get("BareProxy-Id"))
	})
	fmt.Fprintln(os.Stderr, http.ListenAndServe(addr, mux))
	os.Exit(1)
}
