// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package drift_test

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"bareproxy/internal/drift"
	"bareproxy/internal/drift/drifttest"
)

// env is a stub resolver, a TLS server and a CA, with domains set up as a
// healthy site: A 127.0.0.1, two nameservers, MX, SPF, CAA, a certificate
// good for 90 days and HSTS.
type env struct {
	t   *testing.T
	res *drifttest.Resolver
	tls *drifttest.TLSServer
	ca  *drifttest.CA
	dir string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	res, err := drifttest.NewResolver()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(res.Close)
	ts := drifttest.NewTLSServer()
	t.Cleanup(ts.Close)
	return &env{t: t, res: res, tls: ts, ca: drifttest.NewCA("Test CA One", "Test One"), dir: t.TempDir()}
}

func (e *env) site(d string) {
	e.res.Records(d, "A", "127.0.0.1")
	e.res.Records(d, "AAAA")
	e.res.Records(d, "CNAME")
	e.res.Records(d, "NS", "ns1.dns.test", "ns2.dns.test")
	e.res.Records(d, "MX", "10 mx."+d)
	e.res.Records(d, "TXT", `"v=spf1 -all"`)
	e.res.Records(d, "CAA", `0 issue "ca.test"`)
	e.tls.Set(d, e.ca.Issue(time.Now().Add(90*24*time.Hour), d), "max-age=31536000")
}

func (e *env) spec(domains ...string) *drift.Spec {
	pool := x509.NewCertPool()
	pool.AddCert(e.ca.Cert)
	return &drift.Spec{Name: "test", Domains: domains, Types: slices.Clone(drift.Types), Resolver: e.res.Addr,
		Timeout: 400 * time.Millisecond, Expiry: 21 * 24 * time.Hour, TLS: true, Port: e.tls.Port, HSTS: true, CAA: true,
		Roots: pool, Workers: 16, Folder: e.dir}
}

func (e *env) run(s *drift.Spec) *drift.Report {
	e.t.Helper()
	rep, err := (&drift.Checker{}).Run(context.Background(), s)
	if err != nil {
		e.t.Fatal(err)
	}
	return rep
}

func kinds(fs []drift.Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Domain+" "+f.Kind+" "+f.Part)
	}
	slices.Sort(out)
	return out
}

func TestFirstRunRecordsABaselineAndAlertsOnNothing(t *testing.T) {
	e := newEnv(t)
	e.site("a.test")
	rep := e.run(e.spec("a.test"))
	if len(rep.Alerts) != 0 || len(rep.Unchecked) != 0 || rep.ExitCode() != drift.ExitClean {
		t.Fatalf("first run: alerts %v, unchecked %v\n%s", kinds(rep.Alerts), kinds(rep.Unchecked), rep.Text())
	}
	if len(rep.Notes) != 1 || rep.Notes[0].Kind != drift.KindBaseline || !strings.Contains(rep.Notes[0].Text, "A 127.0.0.1") {
		t.Fatalf("notes %+v", rep.Notes)
	}
	rep = e.run(e.spec("a.test"))
	if len(rep.Alerts)+len(rep.Unchecked)+len(rep.Notes) != 0 || rep.Clean != 1 {
		t.Fatalf("second run should say nothing:\n%s", rep.Text())
	}
}

func TestChangedRecordAlwaysAlertsWithBeforeAndAfter(t *testing.T) {
	e := newEnv(t)
	e.site("a.test")
	s := e.spec("a.test")
	e.run(s)
	for _, c := range []struct{ typ, before, after string }{
		{"A", "127.0.0.1", "127.0.0.2"},
		{"MX", "10 mx.a.test", "20 mx.elsewhere.test"},
		{"TXT", `"v=spf1 -all"`, `"v=spf1 include:mail.test -all"`},
		{"CAA", `0 issue "ca.test"`, `0 issue "other-ca.test"`},
		{"CNAME", "", "edge.cdn.test"},
		{"AAAA", "", "::1"},
	} {
		if c.after == "" {
			e.res.Records("a.test", c.typ)
		} else {
			e.res.Records("a.test", c.typ, c.after)
		}
		if c.typ == "A" {
			e.res.Records("a.test", "A", "127.0.0.1", "127.0.0.2") // keep the TLS server reachable
			c.after = "127.0.0.1, 127.0.0.2"
		}
		rep := e.run(s)
		var f *drift.Finding
		for i := range rep.Alerts {
			if rep.Alerts[i].Part == c.typ && rep.Alerts[i].Kind == drift.KindRecord {
				f = &rep.Alerts[i]
			}
		}
		if f == nil || strings.Join(f.Before, ", ") != c.before || strings.Join(f.After, ", ") != c.after || rep.ExitCode() != drift.ExitAlerts {
			t.Fatalf("%s change: got %+v\n%s", c.typ, f, rep.Text())
		}
		if !strings.Contains(rep.Text(), "before: "+cmpOr(c.before, "(none)")) {
			t.Errorf("%s: the report doesn't show before:\n%s", c.typ, rep.Text())
		}
		// Reported once: the new value is now the known one.
		if rep := e.run(s); len(rep.Alerts) != 0 {
			t.Fatalf("%s: alerted again on the next run:\n%s", c.typ, rep.Text())
		}
	}
	h, err := drift.History(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(h); got != 7 { // the baseline and six changes
		t.Errorf("history has %d entries, want 7:\n%s", got, drift.HistoryText(h))
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func TestNameserverMove(t *testing.T) {
	e := newEnv(t)
	e.site("a.test")
	s := e.spec("a.test")
	e.run(s)
	e.res.Records("a.test", "NS", "ns-1.newhost.test", "ns-2.newhost.test")
	rep := e.run(s)
	if got := kinds(rep.Alerts); !slices.Equal(got, []string{"a.test nameservers-moved NS"}) {
		t.Fatalf("alerts %v\n%s", got, rep.Text())
	}
	if !strings.Contains(rep.Alerts[0].Text, "ns1.dns.test, ns2.dns.test -> ns-1.newhost.test, ns-2.newhost.test") {
		t.Errorf("text %q", rep.Alerts[0].Text)
	}
}

func TestLookupFailureIsUncheckedNeverUnchanged(t *testing.T) {
	for _, fail := range []string{"timeout", "servfail", "refused"} {
		t.Run(fail, func(t *testing.T) {
			e := newEnv(t)
			e.site("a.test")
			s := e.spec("a.test")
			e.run(s)
			e.res.Set("a.test", "MX", drifttest.Answer{Fail: fail})
			rep := e.run(s)
			if got := kinds(rep.Unchecked); !slices.Equal(got, []string{"a.test unchecked MX"}) || rep.Clean != 0 {
				t.Fatalf("unchecked %v, clean %d\n%s", got, rep.Clean, rep.Text())
			}
			if rep.ExitCode() != drift.ExitUnchecked {
				t.Errorf("exit code %d, want %d", rep.ExitCode(), drift.ExitUnchecked)
			}
			if !strings.Contains(rep.Unchecked[0].Text, "last known 10 mx.a.test kept") {
				t.Errorf("text %q", rep.Unchecked[0].Text)
			}
			if n := e.res.Asked("a.test", "MX"); n != 2+1 { // once on the first run, twice on this one
				t.Errorf("MX asked %d times, want 3 (one retry before unchecked)", n)
			}
			st, _ := drift.LoadState(e.dir)
			if got := st.Domains["a.test"].Records["MX"]; !slices.Equal(got, []string{"10 mx.a.test"}) {
				t.Errorf("the known MX became %v", got)
			}
			// Back with a different value: a change against the kept value.
			e.res.Records("a.test", "MX", "5 mx2.a.test")
			if got := kinds(e.run(s).Alerts); !slices.Equal(got, []string{"a.test record-changed MX"}) {
				t.Errorf("after the failure, the change gave %v", got)
			}
		})
	}
}

func TestOneFailedLookupThenAnswerIsFine(t *testing.T) {
	e := newEnv(t)
	e.site("a.test")
	s := e.spec("a.test")
	e.run(s)
	e.res.Set("a.test", "TXT", drifttest.Answer{Fail: "timeout"}, drifttest.Answer{Records: []string{`"v=spf1 -all"`}})
	if rep := e.run(s); len(rep.Alerts)+len(rep.Unchecked) != 0 {
		t.Fatalf("one timeout, then the answer:\n%s", rep.Text())
	}
}

func TestFlappingLookupDoesNotAlert(t *testing.T) {
	e := newEnv(t)
	e.site("a.test")
	s := e.spec("a.test")
	e.run(s)
	// Each run: one odd answer, then the known one. Ten runs, no alerts.
	for i := 0; i < 10; i++ {
		e.res.Set("a.test", "TXT", drifttest.Answer{Records: []string{fmt.Sprintf(`"odd %d"`, i)}}, drifttest.Answer{Records: []string{`"v=spf1 -all"`}})
		rep := e.run(s)
		if len(rep.Alerts) != 0 || rep.ExitCode() != drift.ExitClean {
			t.Fatalf("run %d alerted on a flap:\n%s", i, rep.Text())
		}
		if got := kinds(rep.Notes); !slices.Equal(got, []string{"a.test flapped TXT"}) {
			t.Fatalf("run %d notes %v", i, got)
		}
	}
	// A change that holds on the second look alerts, once.
	e.res.Set("a.test", "TXT", drifttest.Answer{Records: []string{`"new"`}})
	if got := kinds(e.run(s).Alerts); !slices.Equal(got, []string{"a.test record-changed TXT"}) {
		t.Fatalf("a real change gave %v", got)
	}
}

func TestChangeThenFailedSecondLookIsUnchecked(t *testing.T) {
	e := newEnv(t)
	e.site("a.test")
	s := e.spec("a.test")
	e.run(s)
	e.res.Set("a.test", "A", drifttest.Answer{Records: []string{"127.0.0.9"}}, drifttest.Answer{Fail: "servfail"})
	rep := e.run(s)
	if len(rep.Alerts) != 0 || !slices.Contains(kinds(rep.Unchecked), "a.test unchecked A") {
		t.Fatalf("got\n%s", rep.Text())
	}
}

func TestExpiringCertificateAlwaysAlerts(t *testing.T) {
	e := newEnv(t)
	e.site("a.test")
	e.tls.Set("a.test", e.ca.Issue(time.Now().Add(10*24*time.Hour), "a.test"), "max-age=31536000")
	s := e.spec("a.test")
	for i := 0; i < 3; i++ {
		rep := e.run(s)
		if got := kinds(rep.Alerts); !slices.Equal(got, []string{"a.test expiring cert"}) {
			t.Fatalf("run %d: alerts %v\n%s", i, got, rep.Text())
		}
		if !strings.Contains(rep.Alerts[0].Text, "inside the 21-day window") {
			t.Errorf("text %q", rep.Alerts[0].Text)
		}
	}
	// The server stops answering: the last certificate seen still ends.
	e.tls.Close()
	rep := e.run(s)
	if got := kinds(rep.Alerts); !slices.Equal(got, []string{"a.test expiring cert"}) || !strings.Contains(rep.Alerts[0].Text, "last seen") {
		t.Fatalf("with the server down: alerts %v\n%s", got, rep.Text())
	}
	if got := kinds(rep.Unchecked); !slices.Equal(got, []string{"a.test unchecked cert"}) {
		t.Fatalf("unchecked %v", got)
	}
}

func TestExpiredCertificate(t *testing.T) {
	e := newEnv(t)
	e.site("a.test")
	e.tls.Set("a.test", e.ca.Issue(time.Now().Add(-time.Hour), "a.test"), "max-age=31536000")
	got := kinds(e.run(e.spec("a.test")).Alerts)
	if !slices.Equal(got, []string{"a.test cert-invalid cert", "a.test expired cert"}) {
		t.Fatalf("alerts %v", got)
	}
}

func TestChangedCertificateAndIssuerAlert(t *testing.T) {
	e := newEnv(t)
	e.site("a.test")
	s := e.spec("a.test")
	e.run(s)
	e.tls.Set("a.test", e.ca.Issue(time.Now().Add(80*24*time.Hour), "a.test"), "max-age=31536000")
	rep := e.run(s)
	if got := kinds(rep.Alerts); !slices.Equal(got, []string{"a.test cert-changed cert"}) {
		t.Fatalf("same issuer: %v\n%s", got, rep.Text())
	}
	if f := rep.Alerts[0]; len(f.Before) != 1 || len(f.After) != 1 || f.Before[0] == f.After[0] {
		t.Errorf("before and after: %+v", f)
	}
	if rep := e.run(s); len(rep.Alerts) != 0 {
		t.Fatalf("alerted again:\n%s", rep.Text())
	}
	two := drifttest.NewCA("Test CA Two", "Test Two")
	s.Roots.AddCert(two.Cert)
	e.tls.Set("a.test", two.Issue(time.Now().Add(80*24*time.Hour), "a.test"), "max-age=31536000")
	rep = e.run(s)
	if got := kinds(rep.Alerts); !slices.Equal(got, []string{"a.test issuer-changed cert"}) {
		t.Fatalf("new issuer: %v\n%s", got, rep.Text())
	}
	if !strings.Contains(rep.Alerts[0].Text, "Test CA One, Test One -> Test CA Two, Test Two") {
		t.Errorf("text %q", rep.Alerts[0].Text)
	}
}

func TestCertificateFlapDoesNotAlert(t *testing.T) {
	e := newEnv(t)
	e.site("a.test")
	known := e.ca.Issue(time.Now().Add(90*24*time.Hour), "a.test")
	e.tls.Set("a.test", known, "max-age=31536000")
	s := e.spec("a.test")
	e.run(s)
	// A load balancer with one stray node: another certificate on the next
	// connection only, then the known one again.
	e.tls.Set("a.test", e.ca.Issue(time.Now().Add(90*24*time.Hour), "a.test"), "max-age=31536000")
	calls := 0
	checker := &drift.Checker{Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if calls++; calls == 2 {
			e.tls.Set("a.test", known, "max-age=31536000")
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	rep, err := checker.Run(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Alerts) != 0 || !slices.Contains(kinds(rep.Notes), "a.test flapped cert") {
		t.Fatalf("a one-off certificate alerted:\n%s", rep.Text())
	}
}

func TestInvalidCertificateAlerts(t *testing.T) {
	e := newEnv(t)
	e.site("a.test")
	e.tls.Set("a.test", e.ca.Issue(time.Now().Add(90*24*time.Hour), "other.test"), "max-age=31536000")
	rep := e.run(e.spec("a.test"))
	if got := kinds(rep.Alerts); !slices.Equal(got, []string{"a.test cert-invalid cert"}) {
		t.Fatalf("alerts %v\n%s", got, rep.Text())
	}
}

func TestHSTSAndCAAGaps(t *testing.T) {
	e := newEnv(t)
	for _, d := range []string{"nohsts.test", "off.test", "nocaa.test", "www.parent.test"} {
		e.site(d)
	}
	e.tls.Set("nohsts.test", e.ca.Issue(time.Now().Add(90*24*time.Hour), "nohsts.test"), "")
	e.tls.Set("off.test", e.ca.Issue(time.Now().Add(90*24*time.Hour), "off.test"), "max-age=0")
	e.res.Records("nocaa.test", "CAA")
	e.res.Records("www.parent.test", "CAA")                  // none of its own,
	e.res.Records("parent.test", "CAA", `0 issue "ca.test"`) // but its parent has one
	s := e.spec("nohsts.test", "off.test", "nocaa.test", "www.parent.test")
	want := []string{"nocaa.test caa-missing CAA", "nohsts.test hsts-missing hsts", "off.test hsts-missing hsts"}
	for i := 0; i < 2; i++ { // standing gaps alert every run
		if got := kinds(e.run(s).Alerts); !slices.Equal(got, want) {
			t.Fatalf("run %d: %v, want %v", i, got, want)
		}
	}
	s.HSTS, s.CAA = false, false
	if rep := e.run(s); len(rep.Alerts) != 0 {
		t.Fatalf("with hsts and caa off:\n%s", rep.Text())
	}
}

func TestTruncatedAnswerFallsBackToTCP(t *testing.T) {
	e := newEnv(t)
	e.site("a.test")
	long := `"` + strings.Repeat("k", 900) + `"`
	e.res.Set("a.test", "TXT", drifttest.Answer{Records: []string{long}, TCPOnly: true})
	s := e.spec("a.test")
	e.run(s)
	st, _ := drift.LoadState(e.dir)
	if got := st.Domains["a.test"].Records["TXT"]; len(got) != 1 || len(got[0]) != 902 {
		t.Fatalf("TXT over TCP: %v", got)
	}
}

func TestWebhookGetsTheReport(t *testing.T) {
	e := newEnv(t)
	e.site("a.test")
	var got drift.Report
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
	}))
	defer hook.Close()
	s := e.spec("a.test")
	s.Webhook = hook.URL
	e.run(s)
	if got.Watch != "" {
		t.Fatal("the webhook was called with nothing to report")
	}
	e.res.Records("a.test", "MX", "1 changed.test")
	rep := e.run(s)
	if rep.Webhook != "sent, answered 200" || len(got.Alerts) != 1 || got.Alerts[0].Part != "MX" {
		t.Fatalf("webhook %q, got %+v", rep.Webhook, got)
	}
}

func TestBusyLock(t *testing.T) {
	e := newEnv(t)
	e.site("a.test")
	os.MkdirAll(e.dir, 0o750)
	os.WriteFile(filepath.Join(e.dir, "lock"), []byte("1 x\n"), 0o640)
	if _, err := (&drift.Checker{}).Run(context.Background(), e.spec("a.test")); err != drift.ErrBusy {
		t.Fatalf("err %v, want ErrBusy", err)
	}
	old := time.Now().Add(-10 * time.Minute)
	os.Chtimes(filepath.Join(e.dir, "lock"), old, old)
	if _, err := (&drift.Checker{}).Run(context.Background(), e.spec("a.test")); err != nil {
		t.Fatalf("a stale lock: %v", err)
	}
}

// TestManyDomains checks hundreds of domains in one run. The time it took is
// logged for results/drift-test.log.
func TestManyDomains(t *testing.T) {
	e := newEnv(t)
	var ds []string
	for i := 0; i < 500; i++ {
		d := fmt.Sprintf("site%03d.test", i)
		e.site(d)
		ds = append(ds, d)
	}
	s := e.spec(ds...)
	start := time.Now()
	rep := e.run(s)
	first := time.Since(start)
	if len(rep.Alerts)+len(rep.Unchecked) != 0 || len(rep.Notes) != 500 {
		t.Fatalf("first run: %d alerts, %d unchecked, %d notes", len(rep.Alerts), len(rep.Unchecked), len(rep.Notes))
	}
	// Change one record on 5 of them and make one lookup fail on 3.
	for i := 0; i < 5; i++ {
		e.res.Records(ds[i*100], "A", "127.0.0.1", "127.0.0.3")
	}
	for i := 0; i < 3; i++ {
		e.res.Set(ds[i*100+50], "NS", drifttest.Answer{Fail: "timeout"})
	}
	start = time.Now()
	rep = e.run(s)
	second := time.Since(start)
	if len(rep.Alerts) != 5 || len(rep.Unchecked) != 3 || rep.Clean != 492 {
		t.Fatalf("second run: %d alerts, %d unchecked, %d clean\n%s", len(rep.Alerts), len(rep.Unchecked), rep.Clean, rep.Text())
	}
	t.Logf("500 domains, 7 record types, TLS and HSTS each, 16 workers: first run %.2f s, second run %.2f s (5 changes found, 3 timeouts unchecked)",
		first.Seconds(), second.Seconds())
}
