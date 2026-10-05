// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"net/http"
)

func init() {
	adminHandlers = append(adminHandlers, func(s *Server, mux *http.ServeMux) {
		mux.HandleFunc("GET /why", s.adminWhy)
	})
}

// adminWhy sends back the JSON record of the request whose ID starts with
// the id parameter, if the ring in memory still has it.
func (s *Server) adminWhy(w http.ResponseWriter, r *http.Request) {
	prefix, err := cleanPrefix(r.URL.Query().Get("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	js, n := s.Current().Mem.Find(prefix)
	switch {
	case n == 0:
		http.Error(w, "no request with an ID starting "+prefix+" in memory", http.StatusNotFound)
	case n > 1:
		http.Error(w, plural(n, "request")+" have IDs starting "+prefix+"; give more characters", http.StatusConflict)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.Write(js)
	}
}
