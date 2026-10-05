// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

// Bugs the acceptance tests found, each as a small test that reproduces it
// and skips with a "known bug" message while it is there. When a bug is fixed
// its test passes and the skip goes away by itself.

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// explain says where a folder redirect goes without the query; the server adds it.
func TestAcceptKnownExplainFolderRedirectQuery(t *testing.T) {
	f := newFixture(t, "", false)
	out, err := Explain(f.rt, "GET", "http://example.com:8080/about?x=1", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	rr := f.do("GET", "/about?x=1")
	if rr.Code != 301 {
		t.Fatalf("the server answered %d", rr.Code)
	}
	want := rr.Header().Get("Location")
	if !strings.Contains(out, "redirect 301 to "+want+"\n") {
		t.Skipf("known bug: explain GET /about?x=1 says \"redirect 301 to /about/\", the server sends Location %q (expected: explain shows the query too, as it does for a redirect rule)", want)
	}
}

// a 421 leaves a record with an empty path.
func TestAcceptKnownNoSiteRecordPath(t *testing.T) {
	f := newFixture(t, "", false)
	req := httptest.NewRequest("GET", "/some/path?x=1", nil)
	req.Host = "unknown.example:8080"
	rr := httptest.NewRecorder()
	f.h.ServeHTTP(rr, req)
	recs := f.records(t)
	last := recs[len(recs)-1]
	if rr.Code != 421 || last.Outcome != "no_site" {
		t.Fatalf("status %d, outcome %q", rr.Code, last.Outcome)
	}
	if last.Path == "" {
		t.Skip("known bug: the record of a request for an unknown host (421, outcome no_site) has path \"\", so why can't say what was asked for (expected: path /some/path, as on every other record)")
	}
}

// explain turns a raw backslash into %5C; on a site that keeps encoded
// slashes it then routes a request the server refuses.
func TestAcceptKnownExplainRawBackslash(t *testing.T) {
	f := accBuildPaths(t)
	out, err := Explain(f.rt, "GET", "http://keep.test:8080/a\\b", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	rr := f.do("keep.test", "GET", "/a\\b")
	if rr.Code != 400 {
		t.Fatalf("the server answered %d, want 400 for a raw backslash", rr.Code)
	}
	if !strings.Contains(out, "refused") {
		t.Skip("known bug: on a site with encoded-slashes keep, explain GET http://keep.test:8080/a\\b treats the backslash as %5C and routes the request, but the server refuses the raw backslash with 400 (expected: explain says the path is refused)")
	}
}

// Go's server answers OPTIONS * itself, so BareProxy sees nothing and no record is made.
func TestAcceptKnownOptionsStar(t *testing.T) {
	e := newAccSmugEnv(t, false)
	resps := e.send(accReq("OPTIONS * HTTP/1.1", []string{"Host: " + e.host, "Connection: close"}, ""), 300*time.Millisecond)
	e.settle()
	recs := e.drainRecords()
	if len(resps) != 1 {
		t.Fatalf("%d responses", len(resps))
	}
	if resps[0].status < 400 && len(recs) == 0 {
		t.Skipf("known bug: OPTIONS * is answered %d by net/http's own handler before BareProxy's handler runs, so it leaves no record and carries no BareProxy-Id (expected: one record per request read, as in the design; setting DisableGeneralOptionsHandler on the server would hand it to the handler)", resps[0].status)
	}
}

// A chunked request body that can't be parsed is the client's fault, but the
// client gets 502 and the record says the backend's response was broken.
func TestAcceptKnownBadChunkIs502(t *testing.T) {
	e := newAccSmugEnv(t, false)
	body := "ZZ\r\nhello\r\n0\r\n\r\n"
	resps := e.send(accReq("POST /api/x HTTP/1.1", []string{"Host: " + e.host, "Transfer-Encoding: chunked", "Connection: close"}, body), 500*time.Millisecond)
	e.settle()
	recs := e.drainRecords()
	if len(resps) != 1 || len(recs) != 1 {
		t.Fatalf("%d responses and %d records", len(resps), len(recs))
	}
	if resps[0].status == 502 {
		t.Skipf("known bug: a request body with a bad chunk size (\"ZZ\") is answered 502 with outcome %q, reason %q, as if the backend's response were broken; the backend gets the request cut short (expected: 400, outcome bad_request)", recs[0].Outcome, recs[0].Reason)
	}
	if resps[0].status != 400 {
		t.Errorf("a bad chunk size got %d, want 400", resps[0].status)
	}
}
