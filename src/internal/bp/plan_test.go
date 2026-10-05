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
		"files "+filepath.Join(dir, "release-41"), "404, no rule, error page /404.html")
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
