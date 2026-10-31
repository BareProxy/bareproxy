// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

// Package drifttest has a stub DNS resolver and a TLS test server for the
// drift watcher's tests, so they never depend on the internet.
package drifttest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Answer is what the stub says for one name and type: records as the
// watcher writes them ("192.0.2.1", "10 mx.example.com", `"v=spf1"`,
// `0 issue "ca.test"`), or a failure.
type Answer struct {
	Records []string
	Fail    string // "", "timeout" (no answer at all), "servfail", "refused"
	TCPOnly bool   // over UDP, answer cut short (TC set), so the client must use TCP
}

// Resolver is a stub DNS server on 127.0.0.1, over UDP and TCP.
type Resolver struct {
	Addr string

	mu      sync.Mutex
	answers map[string][]Answer // name|TYPE -> answers in turn; the last one repeats
	asked   map[string]int
	udp     net.PacketConn
	tcp     net.Listener
}

// NewResolver starts a stub resolver. Stop it with Close.
func NewResolver() (*Resolver, error) {
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := udp.LocalAddr().(*net.UDPAddr).Port
	tcp, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
	if err != nil {
		udp.Close()
		return nil, err
	}
	r := &Resolver{Addr: udp.LocalAddr().String(), answers: map[string][]Answer{}, asked: map[string]int{}, udp: udp, tcp: tcp}
	go r.serveUDP()
	go r.serveTCP()
	return r, nil
}

func itoa(n int) string { return big.NewInt(int64(n)).String() }

// Close stops the resolver.
func (r *Resolver) Close() {
	r.udp.Close()
	r.tcp.Close()
}

// Set gives a name and type its answers, used in turn; the last one stays.
func (r *Resolver) Set(name, typ string, as ...Answer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answers[strings.ToLower(name)+"|"+typ] = as
}

// Records sets a plain answer.
func (r *Resolver) Records(name, typ string, recs ...string) { r.Set(name, typ, Answer{Records: recs}) }

// Asked says how many questions came for a name and type.
func (r *Resolver) Asked(name, typ string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.asked[strings.ToLower(name)+"|"+typ]
}

var typeNames = map[dnsmessage.Type]string{dnsmessage.TypeA: "A", dnsmessage.TypeAAAA: "AAAA", dnsmessage.TypeCNAME: "CNAME",
	dnsmessage.TypeMX: "MX", dnsmessage.TypeNS: "NS", dnsmessage.TypeTXT: "TXT", 257: "CAA"}

// next takes the answer for a question.
func (r *Resolver) next(name, typ string) Answer {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := strings.ToLower(strings.TrimSuffix(name, ".")) + "|" + typ
	r.asked[k]++
	as := r.answers[k]
	if len(as) == 0 {
		return Answer{}
	}
	a := as[0]
	if len(as) > 1 {
		r.answers[k] = as[1:]
	}
	return a
}

func (r *Resolver) serveUDP() {
	buf := make([]byte, 65535)
	for {
		n, from, err := r.udp.ReadFrom(buf)
		if err != nil {
			return
		}
		if out := r.answer(buf[:n], true); out != nil {
			r.udp.WriteTo(out, from)
		}
	}
}

func (r *Resolver) serveTCP() {
	for {
		c, err := r.tcp.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			c.SetDeadline(time.Now().Add(10 * time.Second))
			var l [2]byte
			if _, err := io.ReadFull(c, l[:]); err != nil {
				return
			}
			q := make([]byte, binary.BigEndian.Uint16(l[:]))
			if _, err := io.ReadFull(c, q); err != nil {
				return
			}
			if out := r.answer(q, false); out != nil {
				c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(out))), out...))
			}
		}()
	}
}

// answer builds the reply to one query, or nil for no reply at all.
func (r *Resolver) answer(msg []byte, udp bool) []byte {
	var m dnsmessage.Message
	if m.Unpack(msg) != nil || len(m.Questions) != 1 {
		return nil
	}
	q := m.Questions[0]
	typ := typeNames[q.Type]
	a := r.next(q.Name.String(), typ)
	h := dnsmessage.Header{ID: m.Header.ID, Response: true, RecursionDesired: true, RecursionAvailable: true}
	switch a.Fail {
	case "timeout":
		return nil
	case "servfail":
		h.RCode = dnsmessage.RCodeServerFailure
	case "refused":
		h.RCode = dnsmessage.RCodeRefused
	}
	if udp && a.TCPOnly {
		h.Truncated = true
	}
	b := dnsmessage.NewBuilder(nil, h)
	b.StartQuestions()
	b.Question(q)
	b.StartAnswers()
	if a.Fail == "" && !(udp && a.TCPOnly) {
		rh := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60}
		for _, rec := range a.Records {
			addRecord(&b, rh, typ, rec)
		}
	}
	out, _ := b.Finish()
	return out
}

func name(s string) dnsmessage.Name {
	return dnsmessage.MustNewName(strings.TrimSuffix(s, ".") + ".")
}

func addRecord(b *dnsmessage.Builder, h dnsmessage.ResourceHeader, typ, rec string) {
	switch typ {
	case "A":
		b.AResource(h, dnsmessage.AResource{A: netip.MustParseAddr(rec).As4()})
	case "AAAA":
		b.AAAAResource(h, dnsmessage.AAAAResource{AAAA: netip.MustParseAddr(rec).As16()})
	case "CNAME":
		b.CNAMEResource(h, dnsmessage.CNAMEResource{CNAME: name(rec)})
	case "NS":
		b.NSResource(h, dnsmessage.NSResource{NS: name(rec)})
	case "MX":
		pref, host, _ := strings.Cut(rec, " ")
		n, _ := new(big.Int).SetString(pref, 10)
		b.MXResource(h, dnsmessage.MXResource{Pref: uint16(n.Int64()), MX: name(host)})
	case "TXT":
		v := strings.Trim(rec, `"`)
		var parts []string
		for len(v) > 255 {
			parts, v = append(parts, v[:255]), v[255:]
		}
		b.TXTResource(h, dnsmessage.TXTResource{TXT: append(parts, v)})
	case "CAA":
		f := strings.SplitN(rec, " ", 3)
		flags, _ := new(big.Int).SetString(f[0], 10)
		d := append(append([]byte{byte(flags.Int64()), byte(len(f[1]))}, f[1]...), strings.Trim(f[2], `"`)...)
		b.UnknownResource(h, dnsmessage.UnknownResource{Type: 257, Data: d})
	}
}

// CA is a test certificate authority.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
	PEM  []byte
}

// NewCA makes a CA with a common name and an organization.
func NewCA(cn, org string) *CA {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: cn, Organization: []string{org}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	cert, _ := x509.ParseCertificate(der)
	return &CA{Cert: cert, Key: key, PEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	return n
}

// Issue makes a certificate for names that ends at notAfter.
func (ca *CA) Issue(notAfter time.Time, names ...string) tls.Certificate {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// TLSServer is an HTTPS server on 127.0.0.1 that shows each domain the
// certificate and HSTS header set for it.
type TLSServer struct {
	Port int
	srv  *httptest.Server

	mu    sync.Mutex
	certs map[string]tls.Certificate
	hsts  map[string]string
}

// NewTLSServer starts one. Stop it with Close.
func NewTLSServer() *TLSServer {
	s := &TLSServer{certs: map[string]tls.Certificate{}, hsts: map[string]string{}}
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		h := s.hsts[strings.ToLower(r.TLS.ServerName)]
		s.mu.Unlock()
		if h != "" {
			w.Header().Set("Strict-Transport-Security", h)
		}
		io.WriteString(w, "ok\n")
	}))
	s.srv.TLS = &tls.Config{GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		c, ok := s.certs[strings.ToLower(hello.ServerName)]
		if !ok {
			return nil, io.EOF
		}
		return &c, nil
	}}
	s.srv.StartTLS()
	s.Port = s.srv.Listener.Addr().(*net.TCPAddr).Port
	return s
}

// Set gives a domain its certificate and HSTS header ("" for none).
func (s *TLSServer) Set(domain string, cert tls.Certificate, hsts string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.certs[domain], s.hsts[domain] = cert, hsts
}

// Close stops the server.
func (s *TLSServer) Close() { s.srv.Close() }
