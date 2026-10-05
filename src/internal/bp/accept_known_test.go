// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

// Bugs the acceptance tests found, each as a small test. All were fixed.

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

// Go's server used to answer OPTIONS * itself, so BareProxy saw nothing and
// no record was made. BareProxy answers it now: 200 with no body, an ID and
// one record, with the site when the Host names one. Other methods with a *
// target still get 400.
func TestAcceptOptionsStar(t *testing.T) {
	e := newAccSmugEnv(t, false)
	for _, c := range []struct{ line, host, site string }{
		{"OPTIONS * HTTP/1.1", e.host, "smug.test"},
		{"OPTIONS * HTTP/1.1", "other.test", ""},
		{"GET * HTTP/1.1", e.host, "smug.test"},
	} {
		resps := e.send(accReq(c.line, []string{"Host: " + c.host, "Connection: close"}, ""), 300*time.Millisecond)
		e.settle()
		recs := e.drainRecords()
		if len(resps) != 1 || len(recs) != 1 {
			t.Fatalf("%s: %d responses and %d records, want 1 and 1", c.line, len(resps), len(recs))
		}
		r, rec := resps[0], recs[0]
		if r.hdr.Get("BareProxy-Id") != rec.ID || rec.Path != "*" || !strings.Contains(rec.Site, c.site) || (c.site == "") != (rec.Site == "") {
			t.Errorf("%s (Host %s): id %q, record %+v", c.line, c.host, r.hdr.Get("BareProxy-Id"), rec)
		}
		if c.line[0] == 'G' {
			if r.status != 400 || rec.Outcome != "bad_request" {
				t.Errorf("GET *: status %d, outcome %q; want 400 and bad_request", r.status, rec.Outcome)
			}
			continue
		}
		if r.status != 200 || len(r.body) != 0 || r.hdr.Get("Content-Length") != "0" || rec.Outcome != "local" || rec.Rule != "OPTIONS *" || rec.Status != 200 {
			t.Errorf("OPTIONS * (Host %s): status %d, body %q, record %+v", c.host, r.status, r.body, rec)
		}
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
