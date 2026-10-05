// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The pool transport reads at most 64 KB of response headers (design note,
// section 10). A backend that sends more gets a 502 and a record that says its
// response was broken; one just under the limit goes through.
func TestAcceptResponseHeaderLimit(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := 0
		if r.URL.Path == "/small" {
			n = 40 << 10
		} else if r.URL.Path == "/big" {
			n = 70 << 10
		}
		w.Header().Set("X-Filler", strings.Repeat("a", n))
		w.Write([]byte("ok"))
	}))
	defer be.Close()
	f := newFixture(t, "  backend "+strings.TrimPrefix(be.URL, "http://")+"\n", false)
	for _, c := range []struct {
		path    string
		status  int
		outcome string
	}{
		{"/api/small", 200, "ok"},
		{"/api/big", 502, "bad_response"},
	} {
		rr := f.do("GET", c.path)
		recs := f.records(t)
		last := recs[len(recs)-1]
		if rr.Code != c.status || last.Outcome != c.outcome {
			t.Errorf("GET %s: client got %d, outcome %q; want %d and %q (reason %q)", c.path, rr.Code, last.Outcome, c.status, c.outcome, last.Reason)
		}
	}
}
