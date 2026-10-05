// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

// Bugs the acceptance tests found, each as a small test. All were fixed
// except OPTIONS *, which stays as a documented limit: its test skips with the
// reason while the limit is there.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// explain used to say where a folder redirect goes without the query; the
// server adds it.
func TestAcceptExplainFolderRedirectKeepsQuery(t *testing.T) {
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
	if want != "/about/?x=1" {
		t.Fatalf("the server sent Location %q, want /about/?x=1", want)
	}
	if !strings.Contains(out, "redirect 301 to "+want+"\n") {
		t.Errorf("explain should say \"redirect 301 to %s\":\n%s", want, out)
	}

	// The same in the browser demo, where no folder is open.
	c, probs := ParseWith("x.conf", noDiskConf, ParseOptions{NoDisk: true})
	if c == nil || HasErrors(probs) {
		t.Fatalf("problems: %v", probs)
	}
	defer c.Close()
	rt, err := NewRuntime(c, nil, 0, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	out, err = Explain(rt, "GET", "https://example.com/style.css?v=2", http.Header{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "redirect 301 to /style.css/?v=2 if it's a folder") {
		t.Errorf("explain without a disk should keep the query on the folder redirect:\n%s", out)
	}
}

// a 421 used to leave a record with an empty path.
func TestAcceptNoSiteRecordPath(t *testing.T) {
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
	if last.Path != "/some/path" {
		t.Errorf("the record of a 421 has path %q, want /some/path", last.Path)
	}
}

// explain used to turn a raw backslash into %5C; on a site that keeps encoded
// slashes it then routed a request the server refuses. %5C itself is another
// matter: that site keeps it, and explain has to say so.
func TestAcceptExplainRawBackslash(t *testing.T) {
	f := accBuildPaths(t)
	out, err := Explain(f.rt, "GET", "http://keep.test:8080/a\\b", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	rr := f.do("keep.test", "GET", "/a\\b")
	if rr.Code != 400 {
		t.Fatalf("the server answered %d, want 400 for a raw backslash", rr.Code)
	}
	if !strings.Contains(out, "refused") || !strings.Contains(out, "Action: 400 (bad_request)") {
		t.Errorf("explain should refuse a raw backslash as the server does:\n%s", out)
	}

	out, err = Explain(f.rt, "GET", "http://keep.test:8080/a%5Cb", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	rr = f.do("keep.test", "GET", "/a%5Cb")
	if rr.Code == 400 {
		t.Fatalf("the server answered 400 for %%5C on a site that keeps encoded slashes")
	}
	if strings.Contains(out, "refused") {
		t.Errorf("explain refuses %%5C on a site that keeps it:\n%s", out)
	}
}

// Go's server answers OPTIONS * itself, so BareProxy sees nothing and no
// record is made. A documented limit: handing the request to BareProxy's
// handler (DisableGeneralOptionsHandler) would turn it into a 400, since a *
// target is refused for every other method, and some load balancers probe
// with OPTIONS *.
func TestAcceptKnownOptionsStar(t *testing.T) {
	e := newAccSmugEnv(t, false)
	resps := e.send(accReq("OPTIONS * HTTP/1.1", []string{"Host: " + e.host, "Connection: close"}, ""), 300*time.Millisecond)
	e.settle()
	recs := e.drainRecords()
	if len(resps) != 1 {
		t.Fatalf("%d responses", len(resps))
	}
	if resps[0].status < 400 && len(recs) == 0 {
		t.Skipf("known limit: OPTIONS * is answered %d by net/http's own handler before BareProxy's handler runs, so it leaves no record and carries no BareProxy-Id (the design says one record per request read; left as it is because the alternative, a 400, would break probes that use OPTIONS *)", resps[0].status)
	}
}

// A chunked request body that can't be parsed is the client's fault. It used
// to get 502 and a record that blamed the backend's response.
func TestAcceptBadChunkIs400(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"bad chunk size", "ZZ\r\nhello\r\n0\r\n\r\n"},
		{"no CRLF after the chunk data", "5\r\nhelloXX0\r\n\r\n"},
		{"chunk size too large", "FFFFFFFFFFFFFFFFF\r\nhello\r\n0\r\n\r\n"},
	} {
		e := newAccSmugEnv(t, false)
		resps := e.send(accReq("POST /api/x HTTP/1.1", []string{"Host: " + e.host, "Transfer-Encoding: chunked", "Connection: close"}, c.body), 500*time.Millisecond)
		e.settle()
		recs := e.drainRecords()
		seen, _ := e.be.snapshot()
		if len(resps) != 1 || len(recs) != 1 {
			t.Errorf("%s: %d responses and %d records, want 1 and 1", c.name, len(resps), len(recs))
			continue
		}
		if resps[0].status != 400 || recs[0].Outcome != "bad_request" || recs[0].Reason == "" {
			t.Errorf("%s: client got %d, record has outcome %q reason %q; want 400, bad_request and a reason", c.name, resps[0].status, recs[0].Outcome, recs[0].Reason)
		}
		if len(seen) > 1 {
			t.Errorf("%s: the backend parsed %d requests, want at most one", c.name, len(seen))
		}
	}
}
