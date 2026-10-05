// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// The trace endpoints of the admin socket (why, tail, status, events),
// registered in admin.go. They answer in JSON.

func writeJSON(w http.ResponseWriter, v any) {
	b, err := RecordJSON(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(append(b, '\n'))
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

// TailFilter is one condition on the records a tail shows, such as
// status>=500 or pool=api. A tail shows the records that meet all of them.
type TailFilter struct {
	Key, Op, Val string
	n            int // the status as a number
}

// ParseTailFilter reads a filter. The keys are status (with =, !=, <, <=, >
// and >=), and pool, site, host, outcome, method and path (with = or !=; a
// path matches by prefix, as sent or after cleaning).
func ParseTailFilter(s string) (TailFilter, error) {
	i := strings.IndexAny(s, "<>=!")
	if i <= 0 {
		return TailFilter{}, fmt.Errorf("filter %q needs a key, an operator and a value, such as status>=500 or pool=api", s)
	}
	f := TailFilter{Key: s[:i]}
	for _, op := range []string{">=", "<=", "!=", ">", "<", "="} {
		if val, ok := strings.CutPrefix(s[i:], op); ok {
			f.Op, f.Val = op, val
			break
		}
	}
	switch f.Key {
	case "status":
		n, err := strconv.Atoi(f.Val)
		if err != nil || n < 100 || n > 599 {
			return f, fmt.Errorf("filter %q: the status must be a number from 100 to 599", s)
		}
		f.n = n
	case "pool", "site", "host", "outcome", "method", "path":
		if f.Op != "=" && f.Op != "!=" {
			return f, fmt.Errorf("filter %q: %s takes = or !=", s, f.Key)
		}
	default:
		return f, fmt.Errorf("filter %q: unknown key %q; use status, pool, site, host, outcome, method or path", s, f.Key)
	}
	if f.Op == "" || f.Val == "" {
		return f, fmt.Errorf("filter %q needs an operator and a value", s)
	}
	return f, nil
}

// Match reports whether a record meets the filter.
func (f TailFilter) Match(rec *Record) bool {
	var ok bool
	switch f.Key {
	case "status":
		switch f.Op {
		case ">=":
			return rec.Status >= f.n
		case "<=":
			return rec.Status <= f.n
		case ">":
			return rec.Status > f.n
		case "<":
			return rec.Status < f.n
		}
		ok = rec.Status == f.n
	case "pool":
		ok = rec.Pool == f.Val
	case "site":
		ok = strings.EqualFold(rec.Site, f.Val)
	case "host":
		ok = strings.EqualFold(rec.Host, f.Val) || hostOnly(rec.Host) == strings.ToLower(f.Val)
	case "outcome":
		ok = rec.Outcome == f.Val
	case "method":
		ok = strings.EqualFold(rec.Method, f.Val)
	case "path":
		ok = strings.HasPrefix(rec.Path, f.Val) || rec.NormPath != "" && strings.HasPrefix(rec.NormPath, f.Val)
	}
	return ok == (f.Op == "=")
}

// MatchAll reports whether a record meets every filter.
func MatchAll(fs []TailFilter, rec *Record) bool {
	for _, f := range fs {
		if !f.Match(rec) {
			return false
		}
	}
	return true
}

// adminTail streams the records that meet the filters (the f parameters, one
// per filter), one JSON line each, as requests finish. If the reader falls
// behind, records are skipped, and a line {"dropped":N} says how many.
func (s *Server) adminTail(w http.ResponseWriter, r *http.Request) {
	var fs []TailFilter
	for _, raw := range r.URL.Query()["f"] {
		f, err := ParseTailFilter(raw)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fs = append(fs, f)
	}
	mem := s.Current().Mem
	t := mem.Subscribe()
	defer mem.Unsubscribe(t)
	w.Header().Set("Content-Type", "application/x-ndjson")
	rc := http.NewResponseController(w)
	w.WriteHeader(http.StatusOK)
	rc.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case it := <-t.C:
			if MatchAll(fs, it.rec) {
				fmt.Fprintf(w, "%s\n", it.js)
			}
		}
		if n := t.Dropped.Swap(0); n > 0 {
			fmt.Fprintf(w, "{\"dropped\":%d}\n", n)
		}
		if len(t.C) == 0 && rc.Flush() != nil {
			return
		}
	}
}
