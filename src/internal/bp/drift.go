// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"bareproxy/internal/drift"
)

// The drift watcher lives in the core, not in a plugin: it needs raw DNS
// queries and raw TLS connections, which a plugin can't make (its only way
// out is an HTTP call), and it must run from the command line in CI with no
// server. See internal/drift.

func (p *parser) newDrift(ln int, w []string) *drift.Spec {
	d := &drift.Spec{Line: ln, Types: slices.Clone(drift.Types), Timeout: 5 * time.Second, Expiry: 21 * 24 * time.Hour,
		TLS: true, Port: 443, HSTS: true, CAA: true, Workers: 16}
	if len(w) != 2 || !validName(w[1]) {
		p.errf(ln, "drift takes one name made of letters, digits, - and _")
		return d
	}
	d.Name = w[1]
	if old := p.c.Drifts[d.Name]; old != nil {
		p.errf(ln, "drift %s is already defined on line %d", d.Name, old.Line)
		return d
	}
	p.c.Drifts[d.Name] = d
	p.c.DriftOrder = append(p.c.DriftOrder, d.Name)
	return d
}

func (p *parser) driftSetting(d *drift.Spec, ln int, w []string) error {
	onoff := func(dst *bool) error {
		if len(w) != 2 {
			return fmt.Errorf("%s takes on or off", w[0])
		}
		v, err := onOff(w[1])
		*dst = v
		return err
	}
	dur := func(dst *time.Duration) error {
		if len(w) != 2 {
			return fmt.Errorf("%s takes one duration", w[0])
		}
		v, err := parseLongDuration(w[1])
		*dst = v
		return err
	}
	switch w[0] {
	case "domains":
		if len(w) < 2 {
			return errors.New("domains takes one or more domain names")
		}
		for _, name := range w[1:] {
			name = strings.ToLower(strings.TrimSuffix(name, "."))
			if !drift.ValidDomain(name) {
				return fmt.Errorf("%q isn't a domain name", name)
			}
			d.Domains = append(d.Domains, name)
		}
	case "domains-file":
		if len(w) != 2 {
			return errors.New("domains-file takes one file, with one domain or more a line")
		}
		d.DomainsFile = p.path(w[1])
	case "records":
		if len(w) < 2 {
			return fmt.Errorf("records takes the record types to watch, from %s, or none", strings.Join(drift.Types, " "))
		}
		d.Types = nil
		if len(w) == 2 && w[1] == "none" {
			return nil
		}
		for _, t := range w[1:] {
			t = strings.ToUpper(t)
			if !slices.Contains(drift.Types, t) {
				return fmt.Errorf("%q isn't a record type the watcher reads: use %s", t, strings.Join(drift.Types, " "))
			}
			d.Types = append(d.Types, t)
		}
		slices.SortFunc(d.Types, func(a, b string) int { return slices.Index(drift.Types, a) - slices.Index(drift.Types, b) })
		d.Types = slices.Compact(d.Types)
	case "resolver":
		if len(w) != 2 {
			return errors.New("resolver takes the address of a recursive DNS resolver, such as 1.1.1.1:53")
		}
		a := w[1]
		if _, _, err := net.SplitHostPort(a); err != nil {
			a = net.JoinHostPort(a, "53")
		}
		d.Resolver = a
	case "timeout":
		return dur(&d.Timeout)
	case "expiry":
		return dur(&d.Expiry)
	case "tls":
		return onoff(&d.TLS)
	case "hsts":
		return onoff(&d.HSTS)
	case "caa":
		return onoff(&d.CAA)
	case "port":
		n, err := strconv.Atoi(w[min(1, len(w)-1)])
		if len(w) != 2 || err != nil || n < 1 || n > 65535 {
			return errors.New("port takes the TLS port to connect to, 443 by default")
		}
		d.Port = n
	case "ca":
		if len(w) != 2 {
			return errors.New("ca takes a file of PEM certificates to trust instead of the system's")
		}
		d.CAFile = p.path(w[1])
	case "workers":
		n, err := strconv.Atoi(w[min(1, len(w)-1)])
		if len(w) != 2 || err != nil || n < 1 || n > 256 {
			return errors.New("workers takes a number from 1 to 256")
		}
		d.Workers = n
	case "every":
		if err := dur(&d.Every); err != nil {
			return err
		}
		if d.Every < time.Minute {
			return errors.New("every must be at least 1m")
		}
	case "webhook":
		u, err := url.Parse(w[min(1, len(w)-1)])
		if len(w) != 2 || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("webhook takes the http:// or https:// URL the report is posted to")
		}
		d.Webhook = w[1]
	case "folder":
		if len(w) != 2 {
			return errors.New("folder takes one folder")
		}
		d.Folder = p.path(w[1])
	default:
		return fmt.Errorf("unknown drift setting %q", w[0])
	}
	return nil
}

// compileDrifts fills in each watch's defaults and checks its files.
func (p *parser) compileDrifts() {
	c := p.c
	folders := map[string]string{}
	for _, name := range c.DriftOrder {
		d := c.Drifts[name]
		if d.Folder == "" {
			d.Folder = filepath.Join(stateDir(c), "drift", name)
		}
		if other, dup := folders[d.Folder]; dup {
			p.errf(d.Line, "drift %s uses the same folder as drift %s", name, other)
		}
		folders[d.Folder] = name
		if len(d.Domains) == 0 && d.DomainsFile == "" {
			p.errf(d.Line, "drift %s needs domains or domains-file", name)
		}
		if !d.TLS {
			d.HSTS = false
		}
		if d.Resolver == "" {
			d.Resolver = systemResolver()
			if d.Resolver == "" && len(d.Types) > 0 {
				p.errf(d.Line, "drift %s needs resolver HOST:PORT: no nameserver found in /etc/resolv.conf", name)
			}
		}
		if p.opt.NoDisk {
			continue
		}
		if d.DomainsFile != "" {
			if _, err := d.DomainList(); err != nil {
				p.errf(d.Line, "drift %s: %v", name, unwrapPathErr(err))
			}
		}
		if d.CAFile != "" {
			b, err := os.ReadFile(d.CAFile)
			pool := x509.NewCertPool()
			switch {
			case err != nil:
				p.errf(d.Line, "drift %s: can't read the ca file: %v", name, unwrapPathErr(err))
			case !pool.AppendCertsFromPEM(b):
				p.errf(d.Line, "drift %s: %s holds no PEM certificate", name, d.CAFile)
			default:
				d.Roots = pool
			}
		}
	}
}

// systemResolver is the first nameserver in /etc/resolv.conf, or "".
func systemResolver() string {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" {
			if ip := net.ParseIP(strings.Split(f[1], "%")[0]); ip != nil {
				return net.JoinHostPort(f[1], "53")
			}
		}
	}
	return ""
}

// startDrift runs, in a server, each watch that has every set. A watch is
// checked when every has passed since its last run, as the state file says,
// so a restart or an apply doesn't check it again early. The old runtime's
// loops stop when it is swapped out; the lock file keeps two from
// overlapping.
func (rt *Runtime) startDrift(logf func(string, ...any)) {
	ctx, cancel := context.WithCancel(context.Background())
	rt.driftStop = cancel
	for _, name := range rt.Cfg.DriftOrder {
		spec := rt.Cfg.Drifts[name]
		if spec.Every == 0 {
			continue
		}
		go rt.driftLoop(ctx, spec, logf)
	}
}

func (rt *Runtime) stopDrift() {
	if rt.driftStop != nil {
		rt.driftStop()
	}
}

// driftPause is the least a watch waits after a server starts it.
var driftPause = 5 * time.Second

func (rt *Runtime) driftLoop(ctx context.Context, spec *drift.Spec, logf func(string, ...any)) {
	for {
		wait := time.Duration(0)
		if st, err := drift.LoadState(spec.Folder); err == nil && !st.LastRun.IsZero() {
			wait = time.Until(st.LastRun.Add(spec.Every))
		}
		// A short pause first, so an apply that replaces this runtime at
		// once doesn't start a check that is cut off.
		wait = max(wait, driftPause)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		rep, err := (&drift.Checker{}).Run(ctx, spec)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, drift.ErrBusy):
			// The drift command is checking it now; look again in a minute.
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Minute):
			}
			continue
		case err != nil:
			rt.driftEvent(logf, fmt.Sprintf("drift %s: the check didn't run: %v", spec.Name, err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(spec.Every):
			}
			continue
		}
		summary := fmt.Sprintf("drift %s: %d domains, %d alerts, %d unchecked", spec.Name, rep.Domains, len(rep.Alerts), len(rep.Unchecked))
		if rep.Webhook != "" {
			summary += ", webhook " + rep.Webhook
		}
		rt.driftEvent(logf, summary)
		for i, f := range append(slices.Clone(rep.Alerts), rep.Unchecked...) {
			if i == 20 {
				rt.driftEvent(logf, fmt.Sprintf("drift %s: and %d more; see bareproxy drift --history %s", spec.Name, len(rep.Alerts)+len(rep.Unchecked)-20, spec.Name))
				break
			}
			rt.driftEvent(logf, fmt.Sprintf("drift %s: %s %s", spec.Name, f.Domain, f.Text))
		}
	}
}

func (rt *Runtime) driftEvent(logf func(string, ...any), s string) {
	if logf != nil {
		logf("%s", s)
	}
	rt.Mem.Event("drift", s)
}
