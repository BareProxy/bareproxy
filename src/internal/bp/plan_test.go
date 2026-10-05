// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	mrand "math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// planDir makes a folder for test configs, with the given subfolders for
// files actions.
func planDir(t *testing.T, folders ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range folders {
		if err := os.MkdirAll(filepath.Join(dir, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// writeTestCert puts a self-signed certificate for example.com and
// www.example.com in dir as cert.pem and key.pem.
func writeTestCert(t *testing.T, dir string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "example.com"},
		DNSNames: []string{"example.com", "www.example.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "cert.pem"), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	writeFile(t, filepath.Join(dir, "key.pem"), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})))
}

func mustParse(t *testing.T, dir, src string) *Config {
	t.Helper()
	c, probs := Parse(filepath.Join(dir, "bareproxy.conf"), src)
	if HasErrors(probs) {
		t.Fatalf("config has errors: %v\n%s", probs, src)
	}
	t.Cleanup(c.Close)
	return c
}

// planOf parses two configs in one folder and plans the change.
func planOf(t *testing.T, dir, old, new string) *PlanResult {
	t.Helper()
	return MakePlan(mustParse(t, dir, old), mustParse(t, dir, new))
}

func hasLine(p *PlanResult, where, what, old, new string) bool {
	for _, l := range p.Changes {
		if l.Where == where && l.What == what && l.Old == old && l.New == new {
			return true
		}
	}
	return false
}

func wantLine(t *testing.T, p *PlanResult, where, what, old, new string) {
	t.Helper()
	if !hasLine(p, where, what, old, new) {
		t.Errorf("no change line %q | %q | %q -> %q in:\n%s", where, what, old, new, p.Text())
	}
}

func wantText(t *testing.T, list []string, frag string) {
	t.Helper()
	for _, s := range list {
		if strings.Contains(s, frag) {
			return
		}
	}
	t.Errorf("no line containing %q in %q", frag, list)
}

func TestPlanSameConfigAndMovedRule(t *testing.T) {
	dir := planDir(t)
	old := `site http://example.com
  route /a -> respond 200 "a"
  route /b/* -> respond 200 "b"
  route /* -> respond 404 "none"
`
	if p := planOf(t, dir, old, old); !p.Empty() || p.Text() != "No changes.\n" || len(p.Warnings) != 0 {
		t.Errorf("same config: got %+v\n%s", p, p.Text())
	}
	// The two rules swap places and move down a line: they never overlap,
	// so no request is handled differently.
	moved := `# moved
site http://example.com
  route /b/* -> respond 200 "b"
  route /a -> respond 200 "a"
  route /* -> respond 404 "none"
`
	p := planOf(t, dir, old, moved)
	if !p.Empty() {
		t.Errorf("moved rules: want no changes, got:\n%s", p.Text())
	}
	if p.ID == planOf(t, dir, old, old).ID || len(p.ID) != 12 {
		t.Errorf("plan IDs should differ and have 12 digits: %q", p.ID)
	}
}

func TestPlanPrefixChange(t *testing.T) {
	dir := planDir(t)
	old := `site http://example.com
  route /api/* -> api strip
  route /* -> respond 200 "home"

pool api
  backend 10.0.0.11:8080
`
	new := `site http://example.com
  route /api/v2/* -> api-v2 strip
  route /api/* -> api strip
  route /* -> respond 200 "home"

pool api
  backend 10.0.0.11:8080

pool api-v2
  backend 10.0.0.21:8080
  backend 10.0.0.22:8080
`
	p := planOf(t, dir, old, new)
	wantLine(t, p, "example.com (port 80)", "any method, /api/v2 and below", "pool api, strip /api", "pool api-v2, strip /api/v2")
	if len(p.Changes) != 1 {
		t.Errorf("want 1 change, got:\n%s", p.Text())
	}
	wantText(t, p.Settings, "pool api-v2 added (line 9), 2 backends")
	if !p.Covers(80, "example.com", "GET", "/api/v2/users", nil) || p.Covers(80, "example.com", "GET", "/api/v1", nil) ||
		!p.Covers(80, "EXAMPLE.com.", "DELETE", "/api/v2", nil) || p.Covers(80, "example.com", "GET", "/api/v2x", nil) {
		t.Errorf("Covers places requests wrongly")
	}
}

func TestPlanMethodSplit(t *testing.T) {
	dir := planDir(t)
	old := `site http://example.com
  route /form -> respond 200 "form"
`
	new := `site http://example.com
  route POST /form -> respond 201 "sent"
  route /form -> respond 200 "form"
`
	p := planOf(t, dir, old, new)
	wantLine(t, p, "example.com (port 80)", "POST, /form", `respond 200 "form"`, `respond 201 "sent"`)
	if len(p.Changes) != 1 {
		t.Errorf("want 1 change, got:\n%s", p.Text())
	}
	// GET also takes HEAD.
	get := `site http://example.com
  route GET /form -> respond 200 "form"
`
	p = planOf(t, dir, old, get)
	wantLine(t, p, "example.com (port 80)", "any method except GET and HEAD, /form", `respond 200 "form"`, "404, no rule")
}

func TestPlanHeaderCondition(t *testing.T) {
	dir := planDir(t)
	old := `site http://example.com
  route /app/* -> respond 200 "app"
`
	new := `site http://example.com
  route /app/* header X-Beta=1 -> respond 200 "beta"
  route /app/* -> respond 200 "app"
`
	p := planOf(t, dir, old, new)
	wantLine(t, p, "example.com (port 80)", "any method, with X-Beta: 1, /app and below", `respond 200 "app"`, `respond 200 "beta"`)
	if len(p.Changes) != 1 {
		t.Errorf("want 1 change, got:\n%s", p.Text())
	}
	h := http.Header{}
	h.Set("X-Beta", "1")
	if !p.Covers(80, "example.com", "GET", "/app/x", h) {
		t.Errorf("X-Beta: 1 should be covered")
	}
	h.Set("X-Beta", "2")
	if p.Covers(80, "example.com", "GET", "/app/x", h) || p.Covers(80, "example.com", "GET", "/app/x", nil) {
		t.Errorf("other X-Beta values and no X-Beta should not be covered")
	}
	// A presence condition.
	pres := `site http://example.com
  route /app/* header X-Beta -> respond 200 "beta"
  route /app/* -> respond 200 "app"
`
	p = planOf(t, dir, old, pres)
	wantLine(t, p, "example.com (port 80)", "any method, with X-Beta, /app and below", `respond 200 "app"`, `respond 200 "beta"`)
}

func TestPlanFilesFolderChange(t *testing.T) {
	dir := planDir(t, "release-41", "release-42")
	old := `site http://example.com
  route /* -> files release-41
`
	new := `site http://example.com
  route /* -> files release-42
`
	p := planOf(t, dir, old, new)
	wantLine(t, p, "example.com (port 80)", "any method, every path", "files "+filepath.Join(dir, "release-41"), "files "+filepath.Join(dir, "release-42"))
	if len(p.Changes) != 1 {
		t.Errorf("want 1 change, got:\n%s", p.Text())
	}
}

func TestPlanHostMovesToAnotherSite(t *testing.T) {
	dir := planDir(t)
	old := `site http://example.com http://www.example.com
  route /* -> respond 200 "main"
`
	new := `site http://example.com
  route /* -> respond 200 "main"

site http://www.example.com
  route /* -> redirect 301 http://example.com
`
	p := planOf(t, dir, old, new)
	wantLine(t, p, "www.example.com (port 80)", "any method, every path", `respond 200 "main"`, "redirect 301 http://example.com")
	if len(p.Changes) != 1 {
		t.Errorf("want 1 change, got:\n%s", p.Text())
	}
}

func TestPlanSiteAddedAndRemoved(t *testing.T) {
	dir := planDir(t)
	old := `site http://a.example
  route /* -> respond 200 "a"
`
	new := `site http://b.example
  route /* -> respond 200 "b"
`
	p := planOf(t, dir, old, new)
	wantLine(t, p, "a.example (port 80)", "any method, every path", `respond 200 "a"`, "421, no site")
	wantLine(t, p, "b.example (port 80)", "any method, every path", "421, no site", `respond 200 "b"`)
	if len(p.Changes) != 2 {
		t.Errorf("want 2 changes, got:\n%s", p.Text())
	}
	// A wildcard and a catch-all site.
	wild := `site http://a.example
  route /* -> respond 200 "a"

site http://*.a.example
  route /* -> respond 200 "sub"

site http://*
  route /* -> respond 200 "any"
`
	p = planOf(t, dir, old, wild)
	wantLine(t, p, "*.a.example (port 80)", "any method, every path", "421, no site", `respond 200 "sub"`)
	wantLine(t, p, "any other host (port 80)", "any method, every path", "421, no site", `respond 200 "any"`)
	if !p.Covers(80, "x.a.example", "GET", "/", nil) || !p.Covers(80, "10.0.0.1", "GET", "/", nil) || p.Covers(80, "a.example", "GET", "/", nil) {
		t.Errorf("Covers places hosts wrongly")
	}
}

func TestPlanListenerAdded(t *testing.T) {
	dir := planDir(t)
	old := `site http://example.com
  route /* -> respond 200 "a"
`
	new := `site http://example.com
  route /* -> respond 200 "a"

site http://example.com:8080
  route /* -> respond 200 "admin"
`
	p := planOf(t, dir, old, new)
	wantText(t, p.Settings, "port 8080 (http) added (line 4)")
	wantLine(t, p, "example.com (port 8080)", "any method, every path", "nothing listens on port 8080", `respond 200 "admin"`)
	p = planOf(t, dir, new, old)
	wantText(t, p.Settings, "port 8080 (http) removed (was line 4)")
}

func TestPlanBackendAndPoolSettings(t *testing.T) {
	dir := planDir(t)
	old := `site http://example.com
  route /* -> api

pool api
  backend 10.0.0.11:8080
  backend 10.0.0.12:8080
`
	new := `site http://example.com
  route /* -> api

pool api
  backend 10.0.0.11:8080
  backend 10.0.0.13:8080
  health /healthz
  retries 2
`
	p := planOf(t, dir, old, new)
	if len(p.Changes) != 0 {
		t.Errorf("backends don't change routing, got:\n%s", p.Text())
	}
	wantText(t, p.Settings, "pool api: backend 10.0.0.12:8080 removed (was line 6)")
	wantText(t, p.Settings, "pool api: backend 10.0.0.13:8080 added (line 6)")
	wantText(t, p.Settings, "pool api: health none  ->  /healthz every 5s timeout 2s expect 200-399 (line 7)")
	wantText(t, p.Settings, "pool api: retries 1  ->  2 (line 8)")
	if p.Empty() {
		t.Errorf("plan with setting changes isn't empty")
	}
}

func TestPlanWarnings(t *testing.T) {
	dir := planDir(t)
	old := `site http://example.com
  route /* -> respond 200 "a"
`
	new := `site http://example.com
  route /* -> respond 200 "a"
  route /old/* -> api

pool api
  backend 10.0.0.11:8080

pool spare
  backend 10.0.0.12:8080
`
	p := planOf(t, dir, old, new)
	want := []string{
		"line 3: route /old/* -> api never matches: line 2 takes every request it would get",
		"line 5: pool api isn't used by any rule that can match",
		"line 8: pool spare isn't used by any rule that can match",
	}
	if strings.Join(p.Warnings, "\n") != strings.Join(want, "\n") {
		t.Errorf("warnings:\n%s\nwant:\n%s", strings.Join(p.Warnings, "\n"), strings.Join(want, "\n"))
	}
	if len(p.Changes) != 0 {
		t.Errorf("a shadowed rule changes nothing, got:\n%s", p.Text())
	}
	// Two rules that together take every request of a third.
	split := `site http://example.com
  route GET /x -> respond 200 "get"
  route /x header X-T -> respond 200 "t"
  route GET /x header X-T -> respond 200 "never"
  route /* -> respond 404
`
	p = planOf(t, dir, old, split)
	wantText(t, p.Warnings, `line 4: route GET /x header X-T -> respond 200 "never" never matches: line 2 takes every request it would get`)
}

func TestPlanTooMany(t *testing.T) {
	dir := planDir(t)
	old := `site http://example.com
  route /a -> respond 200 "a"
  route /b -> respond 200 "b"
`
	new := `site http://example.com
  route /a -> respond 200 "a"
  route GET /c -> respond 200 "c"
`
	saved := MaxPlanClasses
	MaxPlanClasses = 5
	defer func() { MaxPlanClasses = saved }()
	p := planOf(t, dir, old, new)
	if !p.TooMany {
		t.Fatalf("want TooMany, got:\n%s", p.Text())
	}
	wantLine(t, p, "example.com (port 80)", "rule removed", `line 3: route /b -> respond 200 "b"`, "")
	wantLine(t, p, "example.com (port 80)", "rule added", "", `line 3: route GET /c -> respond 200 "c"`)
	if len(p.Changes) != 2 || !strings.Contains(p.Text(), "lists changed rules instead") {
		t.Errorf("TooMany text:\n%s", p.Text())
	}
}

func TestPlanHTTPSAndJSON(t *testing.T) {
	dir := planDir(t, "release-41", "release-42")
	writeTestCert(t, dir)
	old := `site example.com www.example.com
  tls cert.pem key.pem
  error 404 /404.html
  route /api/* -> api strip
  route /* -> files release-41

pool api
  backend 10.0.0.11:8080
`
	new := `site example.com www.example.com
  tls cert.pem key.pem
  error 404 /404.html
  route /api/v2/* -> api-v2 strip
  route /api/* -> api strip
  route GET /* -> files release-42

pool api
  backend 10.0.0.11:8080

pool api-v2
  backend 10.0.0.21:8080
  backend 10.0.0.22:8080
`
	p := planOf(t, dir, old, new)
	where := "example.com, www.example.com (port 443)"
	wantLine(t, p, where, "any method, /api/v2 and below", "pool api, strip /api", "pool api-v2, strip /api/v2")
	wantLine(t, p, where, "GET and HEAD, every other path (not /api and below)",
		"files "+filepath.Join(dir, "release-41"), "files "+filepath.Join(dir, "release-42"))
	wantLine(t, p, where, "any method except GET and HEAD, every other path (not /api and below)",
		"files "+filepath.Join(dir, "release-41"), "404, no rule, error page /404.html from "+filepath.Join(dir, "release-42"))
	if len(p.Changes) != 3 {
		t.Errorf("want 3 changes, got:\n%s", p.Text())
	}
	// Port 80 only redirects to https://, before and after.
	for _, l := range p.Changes {
		if strings.Contains(l.Where, "port 80)") {
			t.Errorf("port 80 shouldn't change: %+v", l)
		}
	}
	txt := p.Text()
	if !strings.HasPrefix(txt, "Plan "+p.ID+": 3 routing changes, 1 other change\nRouting\n  example.com, www.example.com (port 443), any method, /api/v2 and below\n      pool api, strip /api  ->  pool api-v2, strip /api/v2\n") {
		t.Errorf("text:\n%s", txt)
	}
	if strings.ContainsRune(txt, '—') || strings.ContainsRune(txt, '→') {
		t.Errorf("text should be plain ASCII arrows, no em dashes")
	}
	t.Logf("\n%s", strings.ReplaceAll(txt, dir, "/var/www"))
	js, err := p.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var back PlanResult
	if err := json.Unmarshal(js, &back); err != nil || back.ID != p.ID || len(back.Changes) != 3 || len(back.Settings) != 1 {
		t.Errorf("JSON round trip: %v %+v\n%s", err, back, js)
	}
	// Moving a site from https to plain http on the same hosts.
	plain := `site http://example.com:443
  route /* -> respond 200 "plain"
`
	p = planOf(t, dir, old, plain)
	wantText(t, p.Settings, "port 443 switched from https to http")
}

// genRoute and genSite describe a generated config before it is written out.
type genRoute struct{ methods, path, header, action string }

type genSite struct {
	hosts  []string
	err404 bool
	routes []genRoute
}

var (
	genHosts   = []string{"a.test", "b.test", "www.a.test", "*.a.test", "*"}
	genPaths   = []string{"/*", "/", "/a", "/a/*", "/a/b", "/a/b/*", "/api", "/api/*", "/api/v2", "/api/v2/*"}
	genMethods = []string{"", "", "", "GET", "POST", "GET,POST"}
	genHeaders = []string{"", "", "", "header X-T", "header X-T=1", "header X-T=2"}
	genActions = []string{`respond 200 "x"`, "respond 201", "redirect 301 http://r.test", "redirect 302 http://r.test/fixed",
		"p1", "p1 strip", "p2", "files f1", "files f2"}
)

func pick[T any](r *mrand.Rand, xs []T) T { return xs[r.IntN(len(xs))] }

func genRandomRoute(r *mrand.Rand) genRoute {
	return genRoute{pick(r, genMethods), pick(r, genPaths), pick(r, genHeaders), pick(r, genActions)}
}

func genConfig(r *mrand.Rand) []genSite {
	hosts := append([]string(nil), genHosts...)
	r.Shuffle(len(hosts), func(i, j int) { hosts[i], hosts[j] = hosts[j], hosts[i] })
	n := 1 + r.IntN(3)
	sites := make([]genSite, n)
	for i := range sites {
		sites[i].hosts = []string{hosts[0]}
		hosts = hosts[1:]
		if r.IntN(3) == 0 && len(hosts) > n-1-i {
			sites[i].hosts = append(sites[i].hosts, hosts[0])
			hosts = hosts[1:]
		}
		sites[i].err404 = r.IntN(4) == 0
		for k := 1 + r.IntN(8); k > 0; k-- {
			sites[i].routes = append(sites[i].routes, genRandomRoute(r))
		}
	}
	return sites
}

func cloneSites(in []genSite) []genSite {
	out := make([]genSite, len(in))
	for i, s := range in {
		out[i] = genSite{hosts: append([]string(nil), s.hosts...), err404: s.err404, routes: append([]genRoute(nil), s.routes...)}
	}
	return out
}

// mutate makes one to three random edits to a generated config.
func mutate(r *mrand.Rand, in []genSite) []genSite {
	sites := cloneSites(in)
	for k := 1 + r.IntN(3); k > 0; k-- {
		s := &sites[r.IntN(len(sites))]
		var rt *genRoute
		if len(s.routes) > 0 {
			rt = &s.routes[r.IntN(len(s.routes))]
		}
		switch op := r.IntN(10); {
		case op == 0 && rt != nil:
			rt.action = pick(r, genActions)
		case op == 1 && rt != nil:
			rt.path = pick(r, genPaths)
		case op == 2 && rt != nil:
			rt.methods = pick(r, genMethods)
		case op == 3 && rt != nil:
			rt.header = pick(r, genHeaders)
		case op == 4 && len(s.routes) > 1:
			i := r.IntN(len(s.routes) - 1)
			s.routes[i], s.routes[i+1] = s.routes[i+1], s.routes[i]
		case op == 5 && len(s.routes) > 1:
			i := r.IntN(len(s.routes))
			s.routes = append(s.routes[:i], s.routes[i+1:]...)
		case op == 6 && len(s.routes) < 8:
			i := r.IntN(len(s.routes) + 1)
			s.routes = append(s.routes[:i], append([]genRoute{genRandomRoute(r)}, s.routes[i:]...)...)
		case op == 7:
			s.err404 = !s.err404
		case op == 8 && len(sites) > 1:
			// Move a host to another site (when its site keeps one).
			from, to := r.IntN(len(sites)), r.IntN(len(sites))
			if from != to && len(sites[from].hosts) > 1 {
				h := sites[from].hosts[len(sites[from].hosts)-1]
				sites[from].hosts = sites[from].hosts[:len(sites[from].hosts)-1]
				sites[to].hosts = append(sites[to].hosts, h)
			}
		case op == 9:
			// Add a site on an unused host, or remove one.
			used := map[string]bool{}
			for _, x := range sites {
				for _, h := range x.hosts {
					used[h] = true
				}
			}
			var free []string
			for _, h := range genHosts {
				if !used[h] {
					free = append(free, h)
				}
			}
			if len(free) > 0 && r.IntN(2) == 0 {
				sites = append(sites, genSite{hosts: []string{pick(r, free)}, routes: []genRoute{genRandomRoute(r)}})
			} else if len(sites) > 1 {
				i := r.IntN(len(sites))
				sites = append(sites[:i], sites[i+1:]...)
			}
		}
	}
	return sites
}

func renderSites(sites []genSite) string {
	var b strings.Builder
	for _, s := range sites {
		b.WriteString("site")
		for _, h := range s.hosts {
			b.WriteString(" http://" + h)
		}
		b.WriteString("\n")
		if s.err404 {
			b.WriteString("  error 404 /404.html\n")
		}
		for _, rt := range s.routes {
			b.WriteString("  route ")
			if rt.methods != "" {
				b.WriteString(rt.methods + " ")
			}
			b.WriteString(rt.path + " ")
			if rt.header != "" {
				b.WriteString(rt.header + " ")
			}
			b.WriteString("-> " + rt.action + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("pool p1\n  backend 10.0.0.1:80\n\npool p2\n  backend 10.0.0.2:80\n")
	return b.String()
}

type genReq struct {
	host, method, path string
	h                  http.Header
}

func genHeader(r *mrand.Rand) http.Header {
	h := http.Header{}
	switch r.IntN(7) {
	case 1:
		h.Add("X-T", "1")
	case 2:
		h.Add("x-t", "2")
	case 3:
		h.Add("X-T", "3")
	case 4:
		h.Add("X-T", "1")
		h.Add("X-T", "2")
	case 5:
		h.Add("X-T", "")
	case 6:
		h.Add("X-T", "2")
		h.Add("X-T", "9")
	}
	return h
}

var (
	reqHosts   = []string{"a.test", "b.test", "www.a.test", "x.a.test", "y.b.test", "c.test", "x.y.a.test", "A.Test.", "10.0.0.1"}
	reqMethods = []string{"GET", "HEAD", "POST", "PUT", "DELETE"}
	reqPaths   = []string{"/", "/a", "/a/", "/a/b", "/a/b/", "/a/b/c", "/a/bc", "/ab", "/api", "/api/", "/api/v2", "/api/v2/",
		"/api/v2/x", "/api/v3", "/apix", "/z", "/z/y", "/api/v2x", "/a/b/c/d"}
)

// candidates holds at least one request of every class the generated
// configs can tell apart.
func candidates() []genReq {
	var out []genReq
	hdrs := []http.Header{{}, {"X-T": {"1"}}, {"X-T": {"2"}}, {"X-T": {"3"}}, {"X-T": {"1", "2"}}}
	for _, host := range []string{"a.test", "b.test", "www.a.test", "x.a.test", "c.test"} {
		for _, m := range []string{"GET", "HEAD", "POST", "PUT"} {
			for _, p := range []string{"/", "/a", "/a/b", "/api", "/api/v2", "/zz", "/a/zz", "/a/b/zz", "/api/zz", "/api/v2/zz"} {
				for _, h := range hdrs {
					out = append(out, genReq{host, m, p, h})
				}
			}
		}
	}
	return out
}

// TestPlanExactOnGeneratedPairs is the check from design note section 12:
// plan is exact on 1,000 generated config pairs. Every request whose
// handling changes falls in a listed class with the right old and new
// effect, no request whose handling stays the same falls in one, and
// every listed class holds a request that changes.
func TestPlanExactOnGeneratedPairs(t *testing.T) {
	dir := planDir(t, "f1", "f2")
	pairs, perPair := 1000, 300
	if testing.Short() {
		pairs = 200
	}
	r := mrand.New(mrand.NewPCG(2026, 10))
	cands := candidates()
	lines, changed := 0, 0
	for i := 0; i < pairs; i++ {
		oldSites := genConfig(r)
		newSites := mutate(r, oldSites)
		oldSrc, newSrc := renderSites(oldSites), renderSites(newSites)
		oc, op := Parse(filepath.Join(dir, "old.conf"), oldSrc)
		nc, np := Parse(filepath.Join(dir, "new.conf"), newSrc)
		if HasErrors(op) || HasErrors(np) {
			t.Fatalf("pair %d: generated config has errors: %v %v\n%s\n----\n%s", i, op, np, oldSrc, newSrc)
		}
		p := MakePlan(oc, nc)
		if p.TooMany {
			t.Fatalf("pair %d: TooMany on a small config", i)
		}
		fail := func(what string, q genReq, eo, en string) {
			t.Fatalf("pair %d: %s: %s %s%s %v: old %q, new %q\nplan:\n%s\nold:\n%s\nnew:\n%s", i, what, q.method, q.host, q.path, q.h, eo, en, p.Text(), oldSrc, newSrc)
		}
		check := func(q genReq) bool {
			eo := RequestEffect(oc, 80, q.host, q.method, q.path, q.h)
			en := RequestEffect(nc, 80, q.host, q.method, q.path, q.h)
			k := p.Classify(80, q.host, q.method, q.path, q.h)
			switch {
			case eo != en && k < 0:
				fail("changed request in no listed class", q, eo, en)
			case eo == en && k >= 0:
				fail("unchanged request in listed class "+p.Changes[k].Where+", "+p.Changes[k].What, q, eo, en)
			case k >= 0 && (p.Changes[k].Old != eo || p.Changes[k].New != en):
				fail("request in a class with other effects "+p.Changes[k].Old+" -> "+p.Changes[k].New, q, eo, en)
			}
			return eo != en
		}
		for j := 0; j < perPair; j++ {
			q := genReq{pick(r, reqHosts), pick(r, reqMethods), pick(r, reqPaths), genHeader(r)}
			if check(q) {
				changed++
			}
		}
		held := make([]bool, len(p.Changes))
		for _, q := range cands {
			if k := p.Classify(80, q.host, q.method, q.path, q.h); k >= 0 && !held[k] {
				held[k] = check(q)
			}
		}
		for k, ok := range held {
			if !ok {
				t.Fatalf("pair %d: listed class holds no changed request: %+v\nplan:\n%s\nold:\n%s\nnew:\n%s", i, p.Changes[k], p.Text(), oldSrc, newSrc)
			}
		}
		lines += len(p.Changes)
		oc.Close()
		nc.Close()
	}
	t.Logf("%d config pairs, %d random requests (%d changed), %d listed classes all checked", pairs, pairs*perPair, changed, lines)
}
