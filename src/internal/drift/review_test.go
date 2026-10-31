// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package drift_test

import (
	"context"
	"crypto/tls"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"bareproxy/internal/drift"

	"golang.org/x/net/dns/dnsmessage"
)

// With hsts off, a server that shows its certificate but doesn't answer
// HTTP (a mail or other TLS port, or a server that hangs up after the
// handshake) is still checked: the certificate is what's watched.
func TestCertificateIsReadWhenHTTPDoesNotAnswerAndHSTSIsOff(t *testing.T) {
	e := newEnv(t)
	e.site("a.test")
	cert := e.ca.Issue(time.Now().AddDate(0, 0, 90), "a.test")
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.(*tls.Conn).Handshake()
			c.Close() // no HTTP here
		}
	}()
	s := e.spec("a.test")
	s.Types, s.HSTS, s.CAA = []string{"A"}, false, false
	s.Port = ln.Addr().(*net.TCPAddr).Port
	rep := e.run(s)
	if len(rep.Unchecked) != 0 || len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0].Text, "certificate a.test") {
		t.Fatalf("the certificate wasn't read:\n%s", rep.Text())
	}
	// And a new certificate there alerts.
	ln.Close()
	ln2, err := tls.Listen("tcp", ln.Addr().String(), &tls.Config{Certificates: []tls.Certificate{e.ca.Issue(time.Now().AddDate(0, 0, 90), "a.test")}})
	if err != nil {
		t.Skip("port taken again:", err)
	}
	defer ln2.Close()
	go func() {
		for {
			c, err := ln2.Accept()
			if err != nil {
				return
			}
			c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	if got := kinds(e.run(s).Alerts); !slices.Equal(got, []string{"a.test cert-changed cert"}) {
		t.Fatalf("a changed certificate gave %v", got)
	}
}

// A change found by a check that was cut off (the server swapping its
// runtime on an apply, or shutting down) isn't taken into the state, so the
// next check still finds it and alerts.
func TestChangeInACheckCutOffStillAlertsNextTime(t *testing.T) {
	e := newEnv(t)
	e.site("a.test")
	e.site("b.test")
	s := e.spec("a.test", "b.test")
	s.Workers = 1
	e.run(s)
	before, err := drift.LoadState(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	e.res.Records("a.test", "A", "127.0.0.1", "127.0.0.2")
	ctx, cancel := context.WithCancel(context.Background())
	checker := &drift.Checker{Dial: func(dctx context.Context, network, addr string) (net.Conn, error) {
		cancel() // cut off once a.test's records are read
		return nil, context.Canceled
	}}
	checker.Run(ctx, s) // a server drops this report: its runtime is gone
	after, err := drift.LoadState(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	if !after.LastRun.Equal(before.LastRun) {
		t.Errorf("a cut-off check moved the last run from %v to %v, so a server waits a whole every before the next", before.LastRun, after.LastRun)
	}
	if got := kinds(e.run(s).Alerts); !slices.Contains(got, "a.test record-changed A") {
		t.Fatalf("the change was swallowed by the cut-off check; next run alerts %v", got)
	}
}

// lameServer answers every question NOERROR with no records and no
// recursion: a referral, as an authoritative-only server gives. That says
// nothing about the records, so it must not read as "no records".
func lameServer(t *testing.T) string {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 4096)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var m dnsmessage.Message
			if m.Unpack(buf[:n]) != nil || len(m.Questions) != 1 {
				continue
			}
			b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: m.Header.ID, Response: true, RecursionDesired: true})
			b.StartQuestions()
			b.Question(m.Questions[0])
			out, _ := b.Finish()
			pc.WriteTo(out, from)
		}
	}()
	return pc.LocalAddr().String()
}

func TestReferralIsUncheckedNotAChange(t *testing.T) {
	e := newEnv(t)
	e.site("a.test")
	s := e.spec("a.test")
	s.TLS, s.HSTS, s.CAA = false, false, false
	e.run(s)
	s.Resolver = lameServer(t)
	rep := e.run(s)
	if len(rep.Alerts) != 0 || len(rep.Unchecked) == 0 {
		t.Fatalf("a non-recursive empty answer read as a change:\n%s", rep.Text())
	}
}
