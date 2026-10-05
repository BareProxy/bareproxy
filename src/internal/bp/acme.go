// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// acmeHTTPClient, when set, talks to the ACME CA (tests trust a test CA with it).
var acmeHTTPClient *http.Client

// acmeState gets and renews the automatic certificates of the running config.
// Runtimes with the same CA, email and state folder share one, so a reload
// keeps the account, the cache and any challenge in flight.
type acmeState struct {
	*autocert.Manager
	http01 http.Handler // answers HTTP-01 challenges; making it turns HTTP-01 on
	key    string
	cfg    atomic.Pointer[Config] // the running config: its https names are the host policy
	mem    *TraceMem
	fails  sync.Map // host name -> last error text, so each failure is noted once
}

func acmeCA(c *Config) string { return cmp.Or(c.ACMECA, acme.LetsEncryptURL) }

func certDir(c *Config) string { return filepath.Join(stateDir(c), "certs") }

// autoNames lists a site's https host names when its certificate is automatic.
func (s *Site) autoNames() (names []string) {
	for _, a := range s.Addrs {
		if s.TLSAuto && a.Scheme == "https" {
			names = append(names, a.Host)
		}
	}
	return names
}

// newACME returns the certificate manager for c: old's when the CA, the
// email and the state folder are the same, nil when no site needs one.
func newACME(c *Config, old *acmeState, mem *TraceMem) *acmeState {
	if !slices.ContainsFunc(c.Sites, func(s *Site) bool { return len(s.autoNames()) > 0 }) {
		return nil
	}
	a, key := old, acmeCA(c)+" "+c.ACMEEmail+" "+certDir(c)
	if a == nil || a.key != key {
		a = &acmeState{key: key, mem: mem}
		a.Manager = &autocert.Manager{Prompt: autocert.AcceptTOS, Email: c.ACMEEmail, RenewBefore: 30 * 24 * time.Hour,
			Cache:  acmeCache{autocert.DirCache(certDir(c)), mem},
			Client: &acme.Client{DirectoryURL: acmeCA(c), HTTPClient: acmeHTTPClient},
			HostPolicy: func(_ context.Context, host string) error {
				if !slices.ContainsFunc(a.cfg.Load().Sites, func(s *Site) bool { return slices.Contains(s.autoNames(), hostOnly(host)) }) {
					return fmt.Errorf("%s has no https site with an automatic certificate", host)
				}
				return nil
			}}
		a.http01 = a.HTTPHandler(nil)
	}
	a.cfg.Store(c)
	return a
}

// get returns the certificate for a handshake, and notes in the events the
// first failure for a name.
func (a *acmeState) get(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert, err := a.GetCertificate(hello)
	if err == nil {
		a.fails.Delete(hello.ServerName)
	} else if prev, _ := a.fails.Swap(hello.ServerName, err.Error()); prev != err.Error() {
		a.mem.Event("certificate", fmt.Sprintf("no certificate for %s: %v", hello.ServerName, err))
	}
	return cert, err
}

// acmeCache keeps autocert's files in <state>/certs and notes in the events
// each certificate it stores, that is each one obtained or renewed.
type acmeCache struct {
	autocert.DirCache
	mem *TraceMem
}

func (c acmeCache) Put(ctx context.Context, key string, data []byte) error {
	err := c.DirCache.Put(ctx, key, data)
	if leaf := pemLeaf(data); err == nil && leaf != nil && !strings.HasSuffix(key, "+token") {
		c.mem.Event("certificate", fmt.Sprintf("certificate for %s obtained, ends %s", key, leaf.NotAfter.UTC().Format("2006-01-02")))
	}
	return err
}

// pemLeaf returns the first certificate in PEM data, or nil.
func pemLeaf(data []byte) *x509.Certificate {
	for b, rest := pem.Decode(data); b != nil; b, rest = pem.Decode(rest) {
		if b.Type == "CERTIFICATE" {
			leaf, _ := x509.ParseCertificate(b.Bytes)
			return leaf
		}
	}
	return nil
}

// certSource says where an https site's certificate comes from.
func certSource(c *Config, s *Site) string {
	if !s.TLSAuto {
		return fmt.Sprintf("from the file %s (line %d)", s.CertFile, s.TLSLine)
	}
	return fmt.Sprintf("automatic, from %s, kept in %s", acmeCA(c), certDir(c))
}

// cachedCerts lists the automatic certificates the cache holds for a site.
func cachedCerts(c *Config, s *Site) (out []CertStatus) {
	for _, name := range s.autoNames() {
		data, _ := os.ReadFile(filepath.Join(certDir(c), name))
		if leaf := pemLeaf(data); leaf != nil {
			out = append(out, CertStatus{Site: s.Name, Subject: leaf.Subject.String(), NotAfter: stamp(leaf.NotAfter), DaysLeft: daysLeft(leaf.NotAfter), Auto: true})
		}
	}
	return out
}
