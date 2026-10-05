// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// acmeRuntime builds a runtime as an apply does, with the state folder and
// the trace log in dir. global adds lines to the global block.
func acmeRuntime(t *testing.T, dir, global, sites string, old *Runtime) *Runtime {
	t.Helper()
	text := fmt.Sprintf("global\n  admin off\n  state %s\n  trace-log %s\n  acme-email ops@auto.test\n%s\n%s", dir, filepath.Join(dir, "trace.log"), global, sites)
	c, probs := Parse(filepath.Join(dir, "bareproxy.conf"), text)
	if HasErrors(probs) {
		t.Fatalf("config: %v", probs)
	}
	rt, err := NewRuntime(c, old, 1, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		rt.Stop()
		if old == nil || old.Trace != rt.Trace {
			rt.Trace.Close()
		}
	})
	return rt
}

// The certificate manager takes exactly the https names of the running
// config, and an apply that keeps the CA, the email and the state folder
// keeps the manager (its account and its challenges in flight).
func TestACMEHostPolicyFollowsApply(t *testing.T) {
	dir, ctx := t.TempDir(), context.Background()
	rt1 := acmeRuntime(t, dir, "", "site a.test www.a.test\n  route /* -> respond 200\nsite http://plain.test\n  route /* -> respond 200\n", nil)
	if rt1.acme == nil {
		t.Fatal("an https site with no tls line has no certificate manager")
	}
	allowed := func(rt *Runtime, host string) bool { return rt.acme.HostPolicy(ctx, host) == nil }
	if !allowed(rt1, "a.test") || !allowed(rt1, "www.a.test") || allowed(rt1, "b.test") || allowed(rt1, "plain.test") {
		t.Error("version 1: the policy should take a.test and www.a.test only")
	}
	rt2 := acmeRuntime(t, dir, "", "site b.test\n  tls auto\n  route /* -> respond 200\n", rt1)
	if rt2.acme != rt1.acme {
		t.Error("an apply with the same CA, email and state made a new manager")
	}
	if !allowed(rt2, "b.test") || allowed(rt2, "a.test") {
		t.Error("version 2: the policy should take b.test only")
	}
	rt3 := acmeRuntime(t, dir, "  acme-ca https://ca.test/dir\n", "site b.test\n  route /* -> respond 200\n", rt2)
	if rt3.acme == rt2.acme || rt3.acme.Client.DirectoryURL != "https://ca.test/dir" {
		t.Error("a new acme-ca should make a new manager for that CA")
	}
	if rt4 := acmeRuntime(t, dir, "", "site http://plain.test\n  route /* -> respond 200\n", rt3); rt4.acme != nil {
		t.Error("a config without https sites has a certificate manager")
	}
}

// The port-80 redirect of a site with an automatic certificate answers
// HTTP-01 challenges through the manager, with one record each.
func TestACMEHTTP01OnPort80(t *testing.T) {
	dir := t.TempDir()
	rt := acmeRuntime(t, dir, "", "site auto.test\n  route /* -> respond 200\n", nil)
	writeFile(t, filepath.Join(dir, "certs", "tok123+http-01"), "tok123.thumbprint")
	h := NewServer(filepath.Join(dir, "bareproxy.conf"), rt).Handler(80, false)
	cases := []struct {
		path, rule string
		status     int
		body       string
	}{
		{"/.well-known/acme-challenge/tok123", "ACME challenge (HTTP-01)", 200, "tok123.thumbprint"},
		{"/.well-known/acme-challenge/unknown", "ACME challenge (HTTP-01)", 404, ""},
		{"/page", "plain HTTP goes to https://", 301, ""},
	}
	for _, c := range cases {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", "http://auto.test"+c.path, nil))
		if rr.Code != c.status || !strings.HasPrefix(rr.Body.String(), c.body) {
			t.Errorf("%s: %d %q, want %d %q", c.path, rr.Code, rr.Body.String(), c.status, c.body)
		}
	}
	recs := (&fixture{log: filepath.Join(dir, "trace.log")}).records(t)
	if len(recs) != len(cases) {
		t.Fatalf("%d records for %d requests", len(recs), len(cases))
	}
	for i, c := range cases {
		if recs[i].Outcome != "local" || recs[i].Rule != c.rule || recs[i].Status != c.status {
			t.Errorf("%s: record %+v", c.path, recs[i])
		}
	}
	out, err := Explain(rt, "GET", "http://auto.test/.well-known/acme-challenge/tok123", nil, true)
	if err != nil || !strings.Contains(out, "answers the ACME HTTP-01 challenge itself") {
		t.Errorf("explain should say BareProxy answers the challenge: %v\n%s", err, out)
	}
}

// selfSigned makes a certificate for host and returns it with the PEM that
// autocert's cache holds: the key, then the certificate.
func selfSigned(t *testing.T, host string, days int) (der, cached []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Duration(days) * 24 * time.Hour)}
	if der, err = x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key); err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	cached = append(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	return der, cached
}

// A site without certificate files gets its certificate from the manager,
// here from the cache; status lists it and explain says where it comes from.
func TestACMECertForFallsBackToManager(t *testing.T) {
	dir := t.TempDir()
	rt := acmeRuntime(t, dir, "", "site auto.test:8443\n  route /* -> respond 200\n", nil)
	der, cached := selfSigned(t, "auto.test", 90)
	writeFile(t, filepath.Join(dir, "certs", "auto.test"), string(cached))
	s := NewServer(filepath.Join(dir, "bareproxy.conf"), rt)
	cc, sc := net.Pipe()
	go func() {
		tls.Server(sc, &tls.Config{GetCertificate: s.certFor(8443)}).Handshake()
		sc.Close()
	}()
	tc := tls.Client(cc, &tls.Config{ServerName: "auto.test", InsecureSkipVerify: true})
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	if got := tc.ConnectionState().PeerCertificates[0].Raw; !bytes.Equal(got, der) {
		t.Error("the handshake didn't use the certificate from the cache")
	}
	tc.Close()
	st := s.Status()
	if len(st.Certificates) != 1 || !st.Certificates[0].Auto || st.Certificates[0].DaysLeft < 88 || st.Certificates[0].Site != "auto.test" {
		t.Errorf("status certificates: %+v", st.Certificates)
	}
	out, _ := Explain(rt, "GET", "https://auto.test:8443/", nil, true)
	if !strings.Contains(out, "Certificate: automatic, from "+acmeCA(rt.Cfg)+", kept in "+filepath.Join(dir, "certs")) {
		t.Errorf("explain should name the automatic certificate:\n%s", out)
	}
}
