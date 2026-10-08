// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"crypto/x509"
	"fmt"
	"maps"
	"slices"
	"time"
)

// Status is what GET /status sends: the running server at a glance.
type Status struct {
	Version       string           `json:"version"`
	ConfigFile    string           `json:"config_file"`
	ConfigVersion int              `json:"config_version"`
	Started       string           `json:"started"`
	UptimeSeconds float64          `json:"uptime_seconds"`
	Listeners     []ListenerStatus `json:"listeners"`
	Sites         []SiteStatus     `json:"sites"`
	Pools         []PoolStatus     `json:"pools"`
	Certificates  []CertStatus     `json:"certificates"`
	Plugins       []PluginStatus   `json:"plugins"`
	Requests      RequestStatus    `json:"requests"`
	Mismatch      string           `json:"mismatch,omitempty"` // why the config file doesn't hold the running config
}

type ListenerStatus struct {
	Port  int      `json:"port"`
	TLS   bool     `json:"tls"`
	Sites []string `json:"sites"`
}

type SiteStatus struct {
	Name      string   `json:"name"`
	Line      int      `json:"line"`
	Addresses []string `json:"addresses"`
	Rules     int      `json:"rules"`
}

// PoolStatus is one pool. A pool that an apply removed has line 0 and is
// listed while its backends drain.
type PoolStatus struct {
	Name     string          `json:"name"`
	Line     int             `json:"line"`
	Checks   string          `json:"checks"`
	Up       int             `json:"up"`
	Size     int             `json:"size"`
	Backends []BackendStatus `json:"backends"`
}

// BackendStatus is one backend. Failures counts the failed checks (or, in a
// pool without checks, failed connections) in a row right now; Reason is the
// latest failure. A backend that an apply removed is "draining" from Since
// to Until, when its connections close.
type BackendStatus struct {
	Addr     string `json:"addr"`
	State    string `json:"state"`
	Since    string `json:"since"`
	InFlight int64  `json:"in_flight"`
	Failures int    `json:"failures"`
	Reason   string `json:"reason,omitempty"`
	Until    string `json:"until,omitempty"`
}

// PluginStatus is one plugin: its file, how many instances work, and the
// metrics it defined.
type PluginStatus struct {
	Name      string           `json:"name"`
	Line      int              `json:"line"`
	File      string           `json:"file"`
	SHA256    string           `json:"sha256"`
	Pinned    bool             `json:"pinned,omitempty"` // run from the history's copy
	Working   int              `json:"working"`
	Instances int              `json:"instances"`
	Metrics   map[string]int64 `json:"metrics,omitempty"`
}

type CertStatus struct {
	Site     string `json:"site"`
	Subject  string `json:"subject"`
	NotAfter string `json:"not_after"`
	DaysLeft int    `json:"days_left"`
	Auto     bool   `json:"auto,omitempty"` // an automatic certificate, read from the cache
}

// Rate counts requests over a time. Complete is false when the ring of
// records has already pushed out some from that time, so the counts are a
// lower bound.
type Rate struct {
	Window
	Complete bool `json:"complete"`
}

type RequestStatus struct {
	Last1m Rate `json:"last_1m"`
	Last5m Rate `json:"last_5m"`
	Ring   struct {
		Records int64  `json:"records"`
		Bytes   int64  `json:"bytes"`
		Limit   int64  `json:"limit"`
		Oldest  string `json:"oldest,omitempty"`
	} `json:"ring"`
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// certOf reads a site's certificate: who it names and when it ends.
func certOf(site *Site) (subject string, notAfter time.Time, ok bool) {
	if site.Cert == nil || len(site.Cert.Certificate) == 0 {
		return "", time.Time{}, false
	}
	leaf := site.Cert.Leaf
	if leaf == nil {
		var err error
		if leaf, err = x509.ParseCertificate(site.Cert.Certificate[0]); err != nil {
			return "", time.Time{}, false
		}
	}
	return leaf.Subject.String(), leaf.NotAfter, true
}

func daysLeft(t time.Time) int { return int(time.Until(t) / (24 * time.Hour)) }

func portSites(p *Port) []string {
	seen := map[string]bool{}
	for _, s := range p.Exact {
		seen[s.Name] = true
	}
	for _, s := range p.Wild {
		seen[s.Name] = true
	}
	if p.Any != nil {
		seen[p.Any.Name] = true
	}
	return slices.Sorted(maps.Keys(seen))
}

func poolChecksText(h *HealthSpec) string {
	if h == nil {
		return "none: a backend is down after 3 failed connections in a row"
	}
	return fmt.Sprintf("GET %s every %s, timeout %s, pass on %d to %d", h.Path, h.Every, h.Timeout, h.Lo, h.Hi)
}

// Status describes the running server.
func (s *Server) Status() *Status {
	rt := s.Current()
	c := rt.Cfg
	st := &Status{Version: Version, ConfigFile: c.File, ConfigVersion: rt.Version, Mismatch: s.ConfigMismatch(),
		Listeners: []ListenerStatus{}, Sites: []SiteStatus{}, Pools: []PoolStatus{}, Certificates: []CertStatus{}, Plugins: []PluginStatus{}}
	started := rt.Mem.started
	st.Started, st.UptimeSeconds = stamp(started), time.Since(started).Seconds()
	for _, p := range sortedPorts(c) {
		st.Listeners = append(st.Listeners, ListenerStatus{Port: p.Num, TLS: p.TLS, Sites: portSites(p)})
	}
	for _, site := range c.Sites {
		ss := SiteStatus{Name: site.Name, Line: site.Line, Rules: len(site.Routes), Addresses: []string{}}
		for _, a := range site.Addrs {
			ss.Addresses = append(ss.Addresses, a.Text)
		}
		st.Sites = append(st.Sites, ss)
		if subject, end, ok := certOf(site); ok {
			st.Certificates = append(st.Certificates, CertStatus{Site: site.Name, Subject: subject, NotAfter: stamp(end), DaysLeft: daysLeft(end)})
		}
		st.Certificates = append(st.Certificates, cachedCerts(c, site)...)
	}
	for _, name := range c.PoolOrder {
		pool := rt.Pools[name]
		ps := PoolStatus{Name: name, Line: pool.Spec.Line, Checks: poolChecksText(pool.Spec.Health), Size: len(pool.Backends), Backends: []BackendStatus{}}
		for _, b := range pool.Backends {
			b := b.Snapshot()
			if b.State == "up" {
				ps.Up++
			}
			ps.Backends = append(ps.Backends, BackendStatus{Addr: b.Addr, State: b.State, Since: stamp(b.Since),
				InFlight: b.InFlight, Failures: b.Fails, Reason: b.Reason})
		}
		st.Pools = append(st.Pools, ps)
	}
	for _, name := range c.PluginOrder {
		ps, p := c.Plugins[name], rt.Plugins[name]
		pst := PluginStatus{Name: name, Line: ps.Line, File: ps.File, SHA256: ps.SHA, Pinned: ps.Pinned}
		if p != nil {
			pst.Working, pst.Instances = p.Health()
			if m := p.Metrics(); len(m) > 0 {
				pst.Metrics = m
			}
		}
		st.Plugins = append(st.Plugins, pst)
	}
	s.mu.Lock()
	drains := slices.DeleteFunc(slices.Clone(s.cs.drains), func(d drainEntry) bool { return time.Now().After(d.until) })
	s.mu.Unlock()
	for _, d := range drains {
		i := slices.IndexFunc(st.Pools, func(p PoolStatus) bool { return p.Name == d.b.Pool })
		if i < 0 { // the pool was removed too (line 0), and stays listed while its backends drain
			i, st.Pools = len(st.Pools), append(st.Pools, PoolStatus{Name: d.b.Pool, Checks: "none", Backends: []BackendStatus{}})
		}
		st.Pools[i].Backends = append(st.Pools[i].Backends, BackendStatus{Addr: d.b.Spec.Addr, State: "draining",
			Since: stamp(d.since), Until: stamp(d.until), InFlight: d.b.inflight.Load()})
	}
	r := &st.Requests
	r.Last1m.Window, r.Last1m.Complete = rt.Mem.Recent(time.Minute)
	r.Last5m.Window, r.Last5m.Complete = rt.Mem.Recent(5 * time.Minute)
	ring := rt.Mem.Stats()
	r.Ring.Records, r.Ring.Bytes, r.Ring.Limit = ring.Records, ring.Bytes, ring.Limit
	if ring.Records > 0 {
		r.Ring.Oldest = ring.Oldest.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return st
}

// certEvents records an event for every certificate this runtime has that
// old (the runtime before it, or nil) didn't have in the same form.
func (rt *Runtime) certEvents(old *Runtime) {
	had := map[string]string{}
	if old != nil {
		for _, site := range old.Cfg.Sites {
			if site.Cert != nil && len(site.Cert.Certificate) > 0 {
				had[site.Name] = string(site.Cert.Certificate[0])
			}
		}
	}
	for _, site := range rt.Cfg.Sites {
		subject, end, ok := certOf(site)
		if ok && had[site.Name] != string(site.Cert.Certificate[0]) {
			rt.Mem.Event("certificate", fmt.Sprintf("%s: certificate for %s loaded, ends %s (%d days left)",
				site.Name, subject, end.UTC().Format("2006-01-02"), daysLeft(end)))
		}
	}
}
