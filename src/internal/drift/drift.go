// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

// Package drift is BareProxy's DNS and TLS drift watcher. For each domain
// in a list it looks up DNS records, connects over TLS and reads the
// certificate and the HSTS header, and compares what it finds with the last
// known good state kept on disk. It reports certificates near their end,
// changed certificates and issuers, changed records with before and after,
// nameserver moves, and missing HSTS and CAA. Every change goes into a
// history file, so the operator can see what changed and when.
//
// Two rules keep the report honest. A lookup that fails or times out is
// tried once more, and if it fails again that part is reported as
// unchecked, never as unchanged, and its last known good state is kept. A
// lookup that shows a change is also tried once more, and the change is
// reported only when the second look agrees that it changed, so one
// flapping answer doesn't raise an alert.
package drift

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Spec is one drift block: what to watch and how.
type Spec struct {
	Name        string
	Line        int
	Domains     []string // from the config
	DomainsFile string   // more domains, one a line, read at each run
	Types       []string // record types, in the order of Types
	Resolver    string
	Timeout     time.Duration
	Expiry      time.Duration // alert when a certificate ends within this
	TLS         bool          // connect and read the certificate and HSTS
	Port        int
	HSTS, CAA   bool // report missing HSTS and CAA
	CAFile      string
	Roots       *x509.CertPool // nil: the system's
	Workers     int
	Every       time.Duration // a running server checks this often; 0: only on command
	Webhook     string
	Folder      string
}

// CertInfo is what the report keeps of a certificate.
type CertInfo struct {
	Subject  string    `json:"subject"`
	Issuer   string    `json:"issuer"`
	NotAfter time.Time `json:"not_after"`
	SHA256   string    `json:"sha256"`
	Names    []string  `json:"names,omitempty"`
}

func (c *CertInfo) line() string {
	return fmt.Sprintf("%s, issued by %s, ends %s, sha256 %.16s", c.Subject, c.Issuer, c.NotAfter.UTC().Format("2006-01-02"), c.SHA256)
}

// Known is the last known good state of one domain.
type Known struct {
	Records map[string][]string `json:"records"`
	Cert    *CertInfo           `json:"cert,omitempty"`
	Seen    map[string]string   `json:"seen"` // part -> when it was last checked, RFC 3339
}

// State is a watch's state file.
type State struct {
	LastRun time.Time         `json:"last_run"`
	Domains map[string]*Known `json:"domains"`
}

// Finding is one line of a report.
type Finding struct {
	Domain string   `json:"domain"`
	Kind   string   `json:"kind"` // see the kinds below
	Part   string   `json:"part"` // a record type, cert or hsts
	Text   string   `json:"text"`
	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`
}

// The kinds of findings. Alerts: expiring, expired, cert-invalid,
// cert-changed, issuer-changed, record-changed, nameservers-moved,
// hsts-missing, caa-missing. Unchecked: unchecked. Notes: baseline, flapped.
const (
	KindExpiring    = "expiring"
	KindExpired     = "expired"
	KindInvalid     = "cert-invalid"
	KindCert        = "cert-changed"
	KindIssuer      = "issuer-changed"
	KindRecord      = "record-changed"
	KindNameservers = "nameservers-moved"
	KindHSTS        = "hsts-missing"
	KindCAA         = "caa-missing"
	KindUnchecked   = "unchecked"
	KindBaseline    = "baseline"
	KindFlapped     = "flapped"
)

// Report is what one run found.
type Report struct {
	Watch     string    `json:"watch"`
	Time      time.Time `json:"time"`
	Domains   int       `json:"domains"`
	Seconds   float64   `json:"seconds"`
	Alerts    []Finding `json:"alerts"`
	Unchecked []Finding `json:"unchecked"`
	Notes     []Finding `json:"notes"`
	Clean     int       `json:"clean"` // domains with nothing to report
	Webhook   string    `json:"webhook,omitempty"`
}

// Exit codes for the drift command.
const (
	ExitClean     = 0
	ExitUnchecked = 3 // nothing to alert on, but something couldn't be checked
	ExitAlerts    = 4
)

// ExitCode is the drift command's exit code for this report.
func (r *Report) ExitCode() int {
	switch {
	case len(r.Alerts) > 0:
		return ExitAlerts
	case len(r.Unchecked) > 0:
		return ExitUnchecked
	}
	return ExitClean
}

// Text is the report as people read it.
func (r *Report) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Drift watch %s, %d %s, %s, took %.1f s\n", r.Watch, r.Domains, plural(r.Domains, "domain", "domains"),
		r.Time.UTC().Format("2006-01-02 15:04:05 UTC"), r.Seconds)
	section := func(label string, fs []Finding) {
		for _, f := range fs {
			fmt.Fprintf(&b, "%-9s  %s  %s\n", label, f.Domain, f.Text)
			if len(f.Before) > 0 || len(f.After) > 0 {
				fmt.Fprintf(&b, "           before: %s\n", cmp.Or(strings.Join(f.Before, ", "), "(none)"))
				fmt.Fprintf(&b, "           after:  %s\n", cmp.Or(strings.Join(f.After, ", "), "(none)"))
			}
		}
	}
	section("ALERT", r.Alerts)
	section("UNCHECKED", r.Unchecked)
	section("NOTE", r.Notes)
	if r.Clean > 0 {
		fmt.Fprintf(&b, "OK         %d %s with nothing to report\n", r.Clean, plural(r.Clean, "domain", "domains"))
	}
	fmt.Fprintf(&b, "Result: %d %s, %d unchecked\n", len(r.Alerts), plural(len(r.Alerts), "alert", "alerts"), len(r.Unchecked))
	if r.Webhook != "" {
		fmt.Fprintf(&b, "Webhook: %s\n", r.Webhook)
	}
	return b.String()
}

// HistoryEntry is one line of a watch's history: a change, or the first
// state recorded for a domain.
type HistoryEntry struct {
	Time   time.Time `json:"time"`
	Domain string    `json:"domain"`
	Kind   string    `json:"kind"`
	Part   string    `json:"part"`
	Text   string    `json:"text"`
	Before []string  `json:"before,omitempty"`
	After  []string  `json:"after,omitempty"`
}

// Checker runs checks. Now and Dial can be replaced in tests.
type Checker struct {
	Now  func() time.Time
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// ErrBusy says another check of the same watch is running.
var ErrBusy = errors.New("another check of this watch is running")

// Domains reads the watch's list: the config's domains, then the file's.
func (s *Spec) DomainList() ([]string, error) {
	out := slices.Clone(s.Domains)
	if s.DomainsFile != "" {
		b, err := os.ReadFile(s.DomainsFile)
		if err != nil {
			return nil, err
		}
		for i, line := range strings.Split(string(b), "\n") {
			line, _, _ = strings.Cut(line, "#")
			for _, d := range strings.Fields(line) {
				d = strings.ToLower(strings.TrimSuffix(d, "."))
				if !ValidDomain(d) {
					return nil, fmt.Errorf("%s line %d: %q isn't a domain name", s.DomainsFile, i+1, d)
				}
				out = append(out, d)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// ValidDomain reports whether d is a plain domain name in lower case.
func ValidDomain(d string) bool {
	if len(d) == 0 || len(d) > 253 || !strings.Contains(d, ".") {
		return false
	}
	for _, l := range strings.Split(d, ".") {
		if len(l) == 0 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, c := range l {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// Run checks every domain once, updates the state and the history, sends
// the webhook when there is something to say, and returns the report.
func (c *Checker) Run(ctx context.Context, s *Spec) (*Report, error) {
	now := c.now()
	unlock, err := lock(s.Folder, now)
	if err != nil {
		return nil, err
	}
	defer unlock()
	domains, err := s.DomainList()
	if err != nil {
		return nil, err
	}
	st, err := LoadState(s.Folder)
	if err != nil {
		return nil, err
	}
	rep := &Report{Watch: s.Name, Time: now, Domains: len(domains), Alerts: []Finding{}, Unchecked: []Finding{}, Notes: []Finding{}}
	type result struct {
		known    *Known
		findings []Finding
	}
	results := make([]result, len(domains))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range max(1, min(s.Workers, len(domains))) {
		wg.Go(func() {
			for i := range jobs {
				k, f := c.checkDomain(ctx, s, domains[i], st.Domains[domains[i]])
				results[i] = result{k, f}
			}
		})
	}
	for i := range domains {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	next := map[string]*Known{}
	var hist []HistoryEntry
	for i, d := range domains {
		next[d] = results[i].known
		if len(results[i].findings) == 0 {
			rep.Clean++
		}
		for _, f := range results[i].findings {
			switch f.Kind {
			case KindUnchecked:
				rep.Unchecked = append(rep.Unchecked, f)
			case KindBaseline, KindFlapped:
				rep.Notes = append(rep.Notes, f)
			default:
				rep.Alerts = append(rep.Alerts, f)
			}
			switch f.Kind {
			case KindCert, KindIssuer, KindRecord, KindNameservers, KindBaseline:
				hist = append(hist, HistoryEntry{Time: now, Domain: f.Domain, Kind: f.Kind, Part: f.Part, Text: f.Text, Before: f.Before, After: f.After})
			}
		}
	}
	rep.Seconds = c.now().Sub(now).Seconds()
	if err := appendHistory(s.Folder, hist); err != nil {
		return nil, err
	}
	if err := saveState(s.Folder, &State{LastRun: now, Domains: next}); err != nil {
		return nil, err
	}
	if s.Webhook != "" && (len(rep.Alerts) > 0 || len(rep.Unchecked) > 0) {
		rep.Webhook = sendWebhook(ctx, s.Webhook, rep)
	}
	return rep, nil
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// checkDomain looks at one domain and returns its new known state and what
// to report. Parts that couldn't be checked keep their old state.
func (c *Checker) checkDomain(ctx context.Context, s *Spec, d string, old *Known) (*Known, []Finding) {
	now := c.now()
	first := old == nil
	k := &Known{Records: map[string][]string{}, Seen: map[string]string{}}
	if old != nil {
		maps(k.Records, old.Records)
		for p, t := range old.Seen {
			k.Seen[p] = t
		}
		k.Cert = old.Cert
	} else {
		old = &Known{Records: map[string][]string{}}
	}
	res := Resolver{Addr: s.Resolver, Timeout: s.Timeout}
	var out []Finding
	var baseline []string
	for _, typ := range s.Types {
		got, err := res.Lookup(ctx, d, typ)
		if err != nil {
			got, err = res.Lookup(ctx, d, typ) // one more try before calling it unchecked
		}
		prev, had := old.Records[typ]
		if err == nil && !first && had && !slices.Equal(got, prev) {
			// A change: look once more, and believe it only if it holds.
			again, err2 := res.Lookup(ctx, d, typ)
			switch {
			case err2 != nil:
				out = append(out, Finding{Domain: d, Kind: KindUnchecked, Part: typ,
					Text: fmt.Sprintf("%s: one lookup showed a change, the next failed (%s); last known %s kept", typ, err2, list(prev))})
				continue
			case slices.Equal(again, prev):
				out = append(out, Finding{Domain: d, Kind: KindFlapped, Part: typ,
					Text: fmt.Sprintf("%s: one lookup showed %s, the next showed the known %s again; not reported", typ, list(got), list(prev))})
				k.Seen[typ] = now.UTC().Format(time.RFC3339)
				continue
			}
			got = again
		}
		if err != nil {
			text := fmt.Sprintf("%s: lookup failed twice (%s)", typ, err)
			if had {
				text += "; last known " + list(prev) + " kept"
			}
			out = append(out, Finding{Domain: d, Kind: KindUnchecked, Part: typ, Text: text})
			continue
		}
		k.Records[typ] = got
		k.Seen[typ] = now.UTC().Format(time.RFC3339)
		switch {
		case first || !had:
			baseline = append(baseline, fmt.Sprintf("%s %s", typ, list(got)))
		case !slices.Equal(got, prev) && typ == "NS":
			out = append(out, Finding{Domain: d, Kind: KindNameservers, Part: typ,
				Text: fmt.Sprintf("nameservers moved: %s -> %s", list(prev), list(got)), Before: prev, After: got})
		case !slices.Equal(got, prev):
			out = append(out, Finding{Domain: d, Kind: KindRecord, Part: typ,
				Text: fmt.Sprintf("%s changed", typ), Before: prev, After: got})
		}
	}
	if s.CAA && slices.Contains(s.Types, "CAA") {
		if caa, ok := k.Records["CAA"]; ok && k.Seen["CAA"] == now.UTC().Format(time.RFC3339) && len(caa) == 0 {
			if f := c.caaGap(ctx, res, d); f != nil {
				out = append(out, *f)
			}
		}
	}
	if s.TLS {
		out = append(out, c.checkTLS(ctx, s, d, k, old.Cert, first, &baseline)...)
	}
	if len(baseline) > 0 {
		out = append([]Finding{{Domain: d, Kind: KindBaseline, Part: "all", Text: "first look, recorded: " + strings.Join(baseline, "; ")}}, out...)
	}
	return k, out
}

// caaGap reports a domain with no CAA records of its own or above it (CAA
// is inherited from the parent names, RFC 8659), so any CA may issue. A
// lookup of a parent that fails says nothing, rather than a false gap.
func (c *Checker) caaGap(ctx context.Context, res Resolver, d string) *Finding {
	labels := strings.Split(d, ".")
	for i := 1; i < len(labels)-1; i++ {
		parent := strings.Join(labels[i:], ".")
		got, err := res.Lookup(ctx, parent, "CAA")
		if err != nil {
			got, err = res.Lookup(ctx, parent, "CAA")
		}
		if err != nil || len(got) > 0 {
			return nil
		}
	}
	return &Finding{Domain: d, Kind: KindCAA, Part: "CAA", Text: "no CAA records here or on its parent names, so any CA may issue a certificate for it"}
}

// probe is what one TLS connection found.
type probe struct {
	cert      *CertInfo
	verifyErr error
	hsts      string
	hstsSeen  bool
}

// checkTLS connects to the domain, reads its certificate and HSTS header,
// and compares the certificate with the last known one.
func (c *Checker) checkTLS(ctx context.Context, s *Spec, d string, k *Known, prev *CertInfo, first bool, baseline *[]string) []Finding {
	var out []Finding
	now := c.now()
	addrs := append(slices.Clone(k.Records["A"]), k.Records["AAAA"]...)
	if !slices.Contains(s.Types, "A") && !slices.Contains(s.Types, "AAAA") {
		addrs = []string{d} // no address records watched: the system resolves the name
	}
	p, err := c.probe(ctx, s, d, addrs)
	if err != nil {
		p, err = c.probe(ctx, s, d, addrs)
	}
	if err == nil && prev != nil && !first && p.cert.SHA256 != prev.SHA256 {
		p2, err2 := c.probe(ctx, s, d, addrs)
		switch {
		case err2 != nil:
			err = fmt.Errorf("one connection showed a new certificate, the next failed (%s)", err2)
		case p2.cert.SHA256 == prev.SHA256:
			out = append(out, Finding{Domain: d, Kind: KindFlapped, Part: "cert",
				Text: fmt.Sprintf("certificate: one connection showed %.16s, the next the known %.16s again; not reported", p.cert.SHA256, prev.SHA256)})
			p = p2
		default:
			p = p2
		}
	}
	if err != nil {
		text := fmt.Sprintf("TLS: %s", err)
		if prev != nil {
			text += "; last known certificate kept"
		}
		out = append(out, Finding{Domain: d, Kind: KindUnchecked, Part: "cert", Text: text})
		// The certificate seen last still ends when it ends.
		if prev != nil {
			out = append(out, expiry(d, prev, now, s.Expiry, " (last seen; this run couldn't connect)")...)
		}
		return out
	}
	k.Cert = p.cert
	k.Seen["cert"] = now.UTC().Format(time.RFC3339)
	switch {
	case first || prev == nil:
		*baseline = append(*baseline, "certificate "+p.cert.line())
	case p.cert.SHA256 != prev.SHA256 && p.cert.Issuer != prev.Issuer:
		out = append(out, Finding{Domain: d, Kind: KindIssuer, Part: "cert",
			Text:   fmt.Sprintf("certificate changed and its issuer too: %s -> %s", prev.Issuer, p.cert.Issuer),
			Before: []string{prev.line()}, After: []string{p.cert.line()}})
	case p.cert.SHA256 != prev.SHA256:
		out = append(out, Finding{Domain: d, Kind: KindCert, Part: "cert",
			Text:   fmt.Sprintf("certificate changed, same issuer (%s)", p.cert.Issuer),
			Before: []string{prev.line()}, After: []string{p.cert.line()}})
	}
	if p.verifyErr != nil {
		out = append(out, Finding{Domain: d, Kind: KindInvalid, Part: "cert", Text: "certificate doesn't verify: " + p.verifyErr.Error()})
	}
	out = append(out, expiry(d, p.cert, now, s.Expiry, "")...)
	if s.HSTS {
		k.Seen["hsts"] = now.UTC().Format(time.RFC3339)
		switch age, ok := hstsMaxAge(p.hsts); {
		case !p.hstsSeen:
			out = append(out, Finding{Domain: d, Kind: KindHSTS, Part: "hsts", Text: "no Strict-Transport-Security header on https://" + d + "/"})
		case !ok || age == 0:
			out = append(out, Finding{Domain: d, Kind: KindHSTS, Part: "hsts", Text: fmt.Sprintf("Strict-Transport-Security %q turns HSTS off or can't be read", p.hsts)})
		}
	}
	return out
}

func expiry(d string, cert *CertInfo, now time.Time, window time.Duration, suffix string) []Finding {
	left := cert.NotAfter.Sub(now)
	end := cert.NotAfter.UTC().Format("2006-01-02 15:04 UTC")
	switch {
	case left <= 0:
		return []Finding{{Domain: d, Kind: KindExpired, Part: "cert", Text: "certificate expired on " + end + suffix}}
	case left < window:
		days := int(left.Hours() / 24)
		return []Finding{{Domain: d, Kind: KindExpiring, Part: "cert",
			Text: fmt.Sprintf("certificate ends in %d %s (%s), inside the %d-day window%s", days, plural(days, "day", "days"), end, int(window.Hours()/24), suffix)}}
	}
	return nil
}

// hstsMaxAge reads max-age from a Strict-Transport-Security value.
func hstsMaxAge(v string) (int64, bool) {
	for _, part := range strings.Split(v, ";") {
		k, val, _ := strings.Cut(strings.TrimSpace(part), "=")
		if strings.EqualFold(strings.TrimSpace(k), "max-age") {
			n, err := strconv.ParseInt(strings.Trim(strings.TrimSpace(val), `"`), 10, 64)
			return n, err == nil && n >= 0
		}
	}
	return 0, false
}

// probe connects to the first address that answers, reads the certificate
// (verified separately, so a bad one is still seen) and asks for / to read
// the HSTS header. Redirects aren't followed.
func (c *Checker) probe(ctx context.Context, s *Spec, d string, addrs []string) (*probe, error) {
	if len(addrs) == 0 {
		return nil, errors.New("no A or AAAA records to connect to")
	}
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	dial := c.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	var lastErr error
	for _, a := range addrs {
		// Each address gets its own probe: a handshake cut off by the
		// timeout may still finish on its own goroutine and write to it.
		p := &probe{}
		var mu sync.Mutex
		target := net.JoinHostPort(a, strconv.Itoa(s.Port))
		tr := &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) { return dial(ctx, network, target) },
			TLSClientConfig: &tls.Config{ServerName: d, MinVersion: tls.VersionTLS12, InsecureSkipVerify: true,
				VerifyConnection: func(cs tls.ConnectionState) error {
					if len(cs.PeerCertificates) == 0 {
						return errors.New("the server sent no certificate")
					}
					leaf := cs.PeerCertificates[0]
					sum := sha256.Sum256(leaf.Raw)
					mu.Lock()
					defer mu.Unlock()
					p.cert = &CertInfo{Subject: name(leaf.Subject.CommonName, leaf.DNSNames), Issuer: issuer(leaf),
						NotAfter: leaf.NotAfter.UTC(), SHA256: hex.EncodeToString(sum[:]), Names: leaf.DNSNames}
					inter := x509.NewCertPool()
					for _, ic := range cs.PeerCertificates[1:] {
						inter.AddCert(ic)
					}
					_, p.verifyErr = leaf.Verify(x509.VerifyOptions{DNSName: d, Roots: s.Roots, Intermediates: inter, CurrentTime: c.now()})
					return nil
				}},
			DisableKeepAlives: true,
			ForceAttemptHTTP2: false,
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+d+"/", nil)
		req.Header.Set("User-Agent", "BareProxy drift watcher")
		resp, err := (&http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		tr.CloseIdleConnections()
		if err != nil {
			mu.Lock()
			gotCert := p.cert != nil
			mu.Unlock()
			if gotCert && s.HSTS {
				// The certificate came, the HTTP answer didn't: the HSTS
				// header is unknown, so the whole look is unchecked.
				lastErr = fmt.Errorf("%s: the certificate came but no HTTP answer (%s)", a, short(err))
			} else {
				lastErr = fmt.Errorf("%s: %s", a, short(err))
			}
			continue
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		mu.Lock()
		defer mu.Unlock()
		p.hsts = resp.Header.Get("Strict-Transport-Security")
		p.hstsSeen = p.hsts != ""
		return p, nil
	}
	return nil, lastErr
}

func name(cn string, sans []string) string {
	if cn != "" {
		return cn
	}
	if len(sans) > 0 {
		return sans[0]
	}
	return "(no name)"
}

func issuer(c *x509.Certificate) string {
	parts := []string{}
	if c.Issuer.CommonName != "" {
		parts = append(parts, c.Issuer.CommonName)
	}
	if len(c.Issuer.Organization) > 0 {
		parts = append(parts, c.Issuer.Organization[0])
	}
	if len(parts) == 0 {
		return c.Issuer.String()
	}
	return strings.Join(parts, ", ")
}

func list(v []string) string {
	if len(v) == 0 {
		return "(none)"
	}
	return strings.Join(v, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func maps(dst, src map[string][]string) {
	for k, v := range src {
		dst[k] = v
	}
}

// LoadState reads a watch's state file; a missing one is an empty state.
func LoadState(folder string) (*State, error) {
	st := &State{Domains: map[string]*Known{}}
	b, err := os.ReadFile(filepath.Join(folder, "state.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("the drift state file %s is broken: %v", filepath.Join(folder, "state.json"), err)
	}
	if st.Domains == nil {
		st.Domains = map[string]*Known{}
	}
	return st, nil
}

func saveState(folder string, st *State) error {
	b, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(folder, "state.json.tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(folder, "state.json"))
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

func appendHistory(folder string, es []HistoryEntry) error {
	if len(es) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for _, e := range es {
		b, _ := json.Marshal(e)
		buf.Write(append(b, '\n'))
	}
	f, err := os.OpenFile(filepath.Join(folder, "history.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	_, err = f.Write(buf.Bytes())
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// History reads a watch's history, oldest first.
func History(folder string) ([]HistoryEntry, error) {
	b, err := os.ReadFile(filepath.Join(folder, "history.jsonl"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []HistoryEntry
	for _, line := range bytes.Split(b, []byte("\n")) {
		var e HistoryEntry
		if json.Unmarshal(line, &e) == nil && e.Domain != "" {
			out = append(out, e)
		}
	}
	return out, nil
}

// HistoryText writes history entries as people read them.
func HistoryText(es []HistoryEntry) string {
	if len(es) == 0 {
		return "No changes recorded yet.\n"
	}
	var b strings.Builder
	for _, e := range es {
		fmt.Fprintf(&b, "%s  %s  %s\n", e.Time.UTC().Format("2006-01-02 15:04:05"), e.Domain, e.Text)
		if len(e.Before) > 0 || len(e.After) > 0 {
			fmt.Fprintf(&b, "                     before: %s\n", list(e.Before))
			fmt.Fprintf(&b, "                     after:  %s\n", list(e.After))
		}
	}
	return b.String()
}

// lock takes the watch's lock file, so a running server and the drift
// command never check the same watch at once. A lock left by a check that
// died is taken over once it hasn't been touched for 5 minutes; a running
// check touches it every minute.
func lock(folder string, now time.Time) (func(), error) {
	if err := os.MkdirAll(folder, 0o750); err != nil {
		return nil, err
	}
	path := filepath.Join(folder, "lock")
	for try := 0; ; try++ {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
		if err == nil {
			fmt.Fprintf(f, "%d %s\n", os.Getpid(), now.UTC().Format(time.RFC3339))
			f.Close()
			stop := make(chan struct{})
			go func() {
				t := time.NewTicker(time.Minute)
				defer t.Stop()
				for {
					select {
					case <-stop:
						return
					case tick := <-t.C:
						os.Chtimes(path, tick, tick)
					}
				}
			}()
			return func() { close(stop); os.Remove(path) }, nil
		}
		fi, serr := os.Stat(path)
		if !errors.Is(err, fs.ErrExist) || serr != nil || try > 0 || time.Since(fi.ModTime()) < 5*time.Minute {
			if errors.Is(err, fs.ErrExist) {
				return nil, ErrBusy
			}
			return nil, err
		}
		os.Remove(path) // stale
	}
}

// sendWebhook posts the report as JSON and says how it went.
func sendWebhook(ctx context.Context, url string, rep *Report) string {
	b, _ := json.Marshal(rep)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return "not sent: " + err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "BareProxy drift watcher")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "not sent: " + short(err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Sprintf("sent, answered %d", resp.StatusCode)
	}
	return fmt.Sprintf("sent, answered %d", resp.StatusCode)
}
