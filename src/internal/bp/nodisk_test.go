// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"net/http"
	"strings"
	"testing"
)

// A config that names a folder and certificate files that don't exist, as the
// browser demo's configs do.
const noDiskConf = `site example.com
  tls /nonexistent/example.crt /nonexistent/example.key
  error 404 /404.html
  route /api/* -> api strip
  route /* -> files /nonexistent/public

pool api
  backend 10.0.0.11:8080
`

func TestParseNoDisk(t *testing.T) {
	// Parse is unchanged: it opens the folder and loads the certificate.
	_, probs := Parse("x.conf", noDiskConf)
	if !HasErrors(probs) {
		t.Fatal("Parse accepted a missing folder and certificate")
	}
	var folder, cert bool
	for _, p := range probs {
		folder = folder || strings.Contains(p.Msg, "can't open folder")
		cert = cert || strings.Contains(p.Msg, "can't load the certificate")
	}
	if !folder || !cert {
		t.Errorf("Parse problems %v: want the folder and the certificate named", probs)
	}

	// ParseWith with NoDisk reads neither.
	c, probs := ParseWith("x.conf", noDiskConf, ParseOptions{NoDisk: true})
	if c == nil || len(probs) != 0 {
		t.Fatalf("NoDisk: config %v, problems %v; want none", c, probs)
	}
	defer c.Close()
	for _, r := range c.Sites[0].Routes {
		if r.Act.Kind == "files" && r.Act.Root != nil {
			t.Error("NoDisk opened a folder")
		}
	}

	// Everything else is still checked.
	_, probs = ParseWith("x.conf", "site *.example.com\n  route /* -> nopool\n", ParseOptions{NoDisk: true})
	if len(probs) == 0 || !strings.Contains(probs[0].Msg, "needs certificate files") {
		t.Errorf("NoDisk dropped the certificate check: %v", probs)
	}
	_, probs = ParseWith("x.conf", "site http://a.test\n  route /* -> nopool\n", ParseOptions{NoDisk: true})
	if len(probs) != 1 || !strings.Contains(probs[0].Msg, "no pool named nopool") {
		t.Errorf("NoDisk dropped the pool check: %v", probs)
	}
}

func TestLookupFileNoFolder(t *testing.T) {
	for _, c := range []struct{ norm, checked string }{
		{"/", "index.html"},
		{"/about/", "about/index.html"},
		{"/style.css", "style.css"},
		{"/a/b", "a/b"},
	} {
		fr := LookupFile(nil, c.norm)
		if fr.Status != 0 || len(fr.Checked) != 1 || fr.Checked[0] != c.checked {
			t.Errorf("LookupFile(nil, %q) = status %d, checked %v; want status 0, checked [%s]", c.norm, fr.Status, fr.Checked, c.checked)
		}
	}
}

func TestExplainNoDisk(t *testing.T) {
	c, probs := ParseWith("x.conf", noDiskConf, ParseOptions{NoDisk: true})
	if c == nil || HasErrors(probs) {
		t.Fatalf("problems: %v", probs)
	}
	defer c.Close()
	rt, err := NewRuntime(c, nil, 0, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	explain := func(method, url string) string {
		out, err := Explain(rt, method, url, http.Header{}, false)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, c := range []struct{ method, url, want string }{
		{"GET", "https://example.com/about/", "Would check: /nonexistent/public/about/index.html"},
		{"GET", "https://example.com/about/", "Not looked up: the browser demo doesn't read the disk"},
		{"GET", "https://example.com/about/", "Error page: /404.html (line 3), from /nonexistent/public/404.html if that file exists"},
		{"GET", "https://example.com/about/", "Action: serve 200 (text/html; charset=utf-8) if the file exists, otherwise 404"},
		{"GET", "https://example.com/style.css", "if it's a file, redirect 301 to /style.css/ if it's a folder, otherwise 404"},
		{"POST", "https://example.com/about/", "Action: 405, files answer only GET and HEAD"},
		{"GET", "https://example.com/api/orders", "Sent upstream as GET /orders"},
	} {
		if got := explain(c.method, c.url); !strings.Contains(got, c.want) {
			t.Errorf("%s %s: output lacks %q:\n%s", c.method, c.url, c.want, got)
		}
	}
}
