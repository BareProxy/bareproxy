// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bareproxy/internal/drift/drifttest"
)

func TestInboxAndDriftConfigErrors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "secret"), "s3cret\n")
	writeFile(t, filepath.Join(dir, "domains.txt"), "a.test b.test # two\nnot_a domain!\n")
	site := "site http://a.test:8080\n  route /hooks/* -> inbox hooks\npool app\n  backend 127.0.0.1:9\n"
	cases := []struct{ name, text, want string }{
		{"inbox without deliver", site + "inbox hooks\n", "needs deliver POOL"},
		{"inbox to a missing pool", site + "inbox hooks\n  deliver nope\n", "pool nope, which isn't defined"},
		{"route to a missing inbox", strings.Replace(site, "inbox hooks", "inbox other", 1) + "inbox hooks\n  deliver app\n", "no inbox named other"},
		{"inbox defined twice", site + "inbox hooks\n  deliver app\ninbox hooks\n  deliver app\n", "already defined"},
		{"bad signature line", site + "inbox hooks\n  deliver app\n  signature md5 secret\n", "signature takes github"},
		{"missing secret file", site + "inbox hooks\n  deliver app\n  signature github nofile\n", "can't read the signature secret"},
		{"bad retry", site + "inbox hooks\n  deliver app\n  retry 1h 1m\n", "longest wait can't be shorter"},
		{"unknown inbox setting", site + "inbox hooks\n  deliver app\n  colour red\n", "unknown inbox setting"},
		{"drift without domains", "drift d\n  resolver 127.0.0.1:53\n", "needs domains or domains-file"},
		{"drift bad domain", "drift d\n  domains not_a_domain\n", "isn't a domain name"},
		{"drift bad record type", "drift d\n  domains a.test\n  records A SRV\n", "isn't a record type"},
		{"drift every too short", "drift d\n  domains a.test\n  every 10s\n", "at least 1m"},
		{"drift bad domains file", "drift d\n  domains-file domains.txt\n  resolver 127.0.0.1:53\n", "domains.txt line 2"},
		{"drift bad webhook", "drift d\n  domains a.test\n  webhook ftp://x\n", "webhook takes"},
		{"drift missing ca", "drift d\n  domains a.test\n  resolver 127.0.0.1:53\n  ca nofile.pem\n", "can't read the ca file"},
	}
	for _, c := range cases {
		_, probs := Parse(filepath.Join(dir, "x.conf"), c.text)
		if !HasErrors(probs) || !strings.Contains(fmt.Sprint(probs), c.want) {
			t.Errorf("%s: got %v, want an error with %q", c.name, probs, c.want)
		}
	}
}

func TestInboxAndDriftConfigsThatWork(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "secret"), "s3cret\n")
	text := `site http://a.test:8080
  route /hooks/* -> inbox hooks
  route /api/* -> inbox
  route /x/* -> inbox strip

pool inbox
  backend 127.0.0.1:9

inbox hooks
  deliver inbox
  path /webhooks
  signature stripe secret
  key-header Idempotency-Key
  retry 30s 2h
  give-up 7d
  keep 14d
  workers 8
  body-limit 512KB

drift sites
  domains a.test B.test.
  records A NS caa
  resolver 127.0.0.1
  expiry 30d
  every 6h
  hsts off
`
	c, probs := Parse(filepath.Join(dir, "x.conf"), text)
	if HasErrors(probs) {
		t.Fatal(probs)
	}
	ib := c.Inboxes["hooks"]
	if ib.Deliver != "inbox" || ib.Path != "/webhooks" || ib.Sig != "stripe" || string(ib.secret) != "s3cret" || ib.KeyHeader != "Idempotency-Key" ||
		ib.Backoff != 30*time.Second || ib.MaxBackoff != 2*time.Hour || ib.GiveUp != 7*24*time.Hour || ib.Workers != 8 || ib.BodyLimit != 512<<10 {
		t.Errorf("inbox %+v", ib)
	}
	// "inbox" alone, and with strip, still names the pool called inbox.
	routes := c.Sites[0].Routes
	if routes[0].Act.Kind != "inbox" || routes[1].Act.Kind != "pool" || routes[2].Act.Kind != "pool" || !routes[2].Act.Strip {
		t.Errorf("routes %+v %+v %+v", routes[0].Act, routes[1].Act, routes[2].Act)
	}
	d := c.Drifts["sites"]
	if fmt.Sprint(d.Domains) != "[a.test b.test]" || fmt.Sprint(d.Types) != "[A NS CAA]" || d.Resolver != "127.0.0.1:53" ||
		d.Expiry != 30*24*time.Hour || d.Every != 6*time.Hour || d.HSTS || !d.TLS || d.Folder != filepath.Join(DefaultStateDir, "drift", "sites") {
		t.Errorf("drift %+v", d)
	}
	// A config with drift blocks and no sites is fine, for CI.
	_, probs = Parse(filepath.Join(dir, "y.conf"), "drift d\n  domains a.test\n  resolver 127.0.0.1:53\n")
	if HasErrors(probs) || !strings.Contains(fmt.Sprint(probs), "no sites") {
		t.Errorf("drift only: %v", probs)
	}
	// plan lists inbox and drift settings like any block's.
	old, _ := Parse(filepath.Join(dir, "x.conf"), text)
	cur, _ := Parse(filepath.Join(dir, "x.conf"), strings.Replace(text, "every 6h", "every 1h", 1))
	if p := MakePlan(old, cur); !strings.Contains(p.Text(), "drift sites: every 6h  ->  1h") {
		t.Errorf("plan:\n%s", p.Text())
	}
}

func TestServerRunsDriftWatches(t *testing.T) {
	res, err := drifttest.NewResolver()
	if err != nil {
		t.Fatal(err)
	}
	defer res.Close()
	res.Records("a.test", "A", "192.0.2.1")
	old := driftPause
	driftPause = 50 * time.Millisecond
	defer func() { driftPause = old }()
	dir := t.TempDir()
	port := freePort(t)
	conf := filepath.Join(dir, "bareproxy.conf")
	writeFile(t, conf, fmt.Sprintf(`global
  admin off
  trace-log off
  state %s

site http://a.test:%d
  route /* -> respond 200

drift web
  domains a.test
  records A
  tls off
  caa off
  resolver %s
  every 1m
`, filepath.Join(dir, "state"), port, res.Addr))
	s, err := Start(conf)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	waitUntil(t, "the drift event", 10*time.Second, func() bool {
		for _, e := range s.Current().Mem.Events() {
			if e.Kind == "drift" && strings.Contains(e.Text, "drift web: 1 domains, 0 alerts, 0 unchecked") {
				return true
			}
		}
		return false
	})
	// Not again before every has passed, even across a restart.
	s.Stop()
	res.Records("a.test", "A", "192.0.2.9")
	s2, err := Start(conf)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Stop()
	time.Sleep(300 * time.Millisecond)
	for _, e := range s2.Current().Mem.Events() {
		if e.Kind == "drift" {
			t.Fatalf("checked again early: %s", e.Text)
		}
	}
}
