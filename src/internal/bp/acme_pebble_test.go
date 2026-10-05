// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestACMEPebble gets a certificate from Pebble, Let's Encrypt's test CA, and
// serves a page with it over HTTPS. It runs only when PEBBLE names the pebble
// binary (github.com/letsencrypt/pebble/releases). With PEBBLE_CHALLTESTSRV
// naming pebble-challtestsrv too, Pebble looks the name up there and really
// validates the TLS-ALPN-01 challenge on BareProxy's port; without it, Pebble
// passes every challenge (PEBBLE_VA_ALWAYS_VALID=1).
func TestACMEPebble(t *testing.T) {
	bin := os.Getenv("PEBBLE")
	if bin == "" {
		t.Skip("set PEBBLE to the pebble binary to run the ACME flow against Pebble")
	}
	dir := t.TempDir()
	_, keyPair := selfSigned(t, "localhost", 30) // Pebble's own HTTPS certificate and key, in one file
	writeFile(t, filepath.Join(dir, "pebble.pem"), string(keyPair))
	api, mgmt, port, dns := freePort(t), freePort(t), freePort(t), freePort(t)
	writeFile(t, filepath.Join(dir, "pebble.json"), fmt.Sprintf(`{"pebble": {"listenAddress": "127.0.0.1:%d",
  "managementListenAddress": "127.0.0.1:%d", "certificate": %q, "privateKey": %q, "httpPort": %d, "tlsPort": %d}}`,
		api, mgmt, filepath.Join(dir, "pebble.pem"), filepath.Join(dir, "pebble.pem"), freePort(t), port))
	args := []string{"-config", filepath.Join(dir, "pebble.json")}
	env := []string{"PEBBLE_VA_NOSLEEP=1", "PEBBLE_WFE_NONCEREJECT=0", "NO_PROXY=*", "no_proxy=*"}
	mode := "every challenge passes (PEBBLE_VA_ALWAYS_VALID=1)"
	if cts := os.Getenv("PEBBLE_CHALLTESTSRV"); cts != "" {
		help, _ := exec.Command(cts, "-h").CombinedOutput()
		dnsFlag := "-dns01" // the name up to v2.8.0; later releases call it -dnsserver
		if strings.Contains(string(help), "-dnsserver") {
			dnsFlag = "-dnsserver"
		}
		runFor(t, cts, nil, "-defaultIPv4", "127.0.0.1", "-defaultIPv6", "", dnsFlag, fmt.Sprintf("127.0.0.1:%d", dns),
			"-doh", "", "-http01", "", "-https01", "", "-tlsalpn01", "", "-management", fmt.Sprintf("127.0.0.1:%d", freePort(t)))
		args = append(args, "-dnsserver", fmt.Sprintf("127.0.0.1:%d", dns))
		mode = "Pebble validates the challenge on BareProxy's port, names resolved by pebble-challtestsrv"
	} else {
		env = append(env, "PEBBLE_VA_ALWAYS_VALID=1")
	}
	t.Log("mode:", mode)
	runFor(t, bin, env, args...)
	trust := x509.NewCertPool()
	trust.AppendCertsFromPEM(keyPair)
	acmeHTTPClient = &http.Client{Timeout: time.Minute, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: trust}}}
	defer func() { acmeHTTPClient = nil }()
	waitGet(t, fmt.Sprintf("https://localhost:%d/dir", api))
	root := waitGet(t, fmt.Sprintf("https://localhost:%d/roots/0", mgmt)) // the root of the certificates Pebble issues

	conf := filepath.Join(dir, "bareproxy.conf")
	writeFile(t, conf, fmt.Sprintf("global\n  admin off\n  state %s\n  trace-log off\n  acme-ca https://localhost:%d/dir\n  acme-email ops@auto.test\n\n"+
		"site auto.test:%d\n  route /* -> respond 200 \"hello over ACME\"\n", filepath.Join(dir, "state"), api, port))
	s, err := Start(conf)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(root)
	c := &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, fmt.Sprintf("127.0.0.1:%d", port))
		}}}
	start := time.Now()
	resp, err := c.Get(fmt.Sprintf("https://auto.test:%d/", port))
	if err != nil {
		t.Fatalf("%v; events: %+v", err, s.Current().Mem.Events())
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello over ACME\n" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	leaf := resp.TLS.PeerCertificates[0]
	t.Logf("certificate for %v from %s, valid to %s, first request took %s", leaf.DNSNames, leaf.Issuer, leaf.NotAfter.UTC().Format(time.RFC3339), time.Since(start).Round(time.Millisecond))
	st := s.Status()
	if !slices.ContainsFunc(st.Certificates, func(cs CertStatus) bool { return cs.Auto && cs.Site == "auto.test" }) {
		t.Errorf("status doesn't list the automatic certificate: %+v", st.Certificates)
	}
	if !slices.ContainsFunc(s.Current().Mem.Events(), func(e Event) bool { return strings.Contains(e.Text, "certificate for auto.test obtained") }) {
		t.Errorf("no event for the certificate obtained: %+v", s.Current().Mem.Events())
	}
}

// runFor runs a program until the test ends, and shows its output if the
// test fails or PEBBLE_LOG is set.
func runFor(t *testing.T, bin string, env []string, args ...string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), filepath.Base(bin)+".log")
	out, err := os.Create(log)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Env, cmd.Stdout, cmd.Stderr = append(os.Environ(), env...), out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		out.Close()
		if t.Failed() || os.Getenv("PEBBLE_LOG") != "" {
			data, _ := os.ReadFile(log)
			t.Logf("%s output:\n%s", filepath.Base(bin), data)
		}
	})
}

// waitGet fetches a URL with acmeHTTPClient, waiting up to 10 s for the
// server to come up.
func waitGet(t *testing.T, url string) []byte {
	t.Helper()
	for i := 0; ; i++ {
		resp, err := acmeHTTPClient.Get(url)
		if err == nil {
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return b
		}
		if i == 100 {
			t.Fatalf("%s: %v", url, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
