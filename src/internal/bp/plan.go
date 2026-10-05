// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// MaxPlanClasses is the most request classes plan works through for one
// site pair. Above it, plan lists changed rules instead (design note,
// section 9).
var MaxPlanClasses = 1000000

// PlanResult says what replacing one config with another would change.
type PlanResult struct {
	ID       string     `json:"id"`          // short hash of both configs; apply --plan checks it
	OldVer   int        `json:"old_version"` // running version the plan was made against (0 = a file)
	Changes  []PlanLine `json:"changes"`     // classes of requests handled differently
	Settings []string   `json:"settings"`    // other changes: listeners, pools, certificates, globals
	Warnings []string   `json:"warnings"`    // rules that win nothing, pools no reachable rule uses
	TooMany  bool       `json:"too_many"`    // too many classes: Changes lists changed rules instead

	pl *planner // kept so Classify can place a request in a listed class
}

// PlanLine is one class of requests whose handling changes.
type PlanLine struct {
	Where string `json:"where"` // e.g. "example.com (port 443)"
	What  string `json:"what"`  // e.g. "any method, /api/v2 and below"
	Old   string `json:"old"`   // e.g. "pool api, strip /api"
	New   string `json:"new"`   // e.g. "pool api-v2, strip /api/v2"
}

// MakePlan compares two compiled configs. Neither is changed. A nil config
// counts as one with no listeners, sites or pools.
func MakePlan(old, new *Config) *PlanResult {
	pl := newPlanner(cmp.Or(old, &Config{}), cmp.Or(new, &Config{}))
	res := &PlanResult{ID: PlanID(old, new), Changes: []PlanLine{}, Settings: []string{}, Warnings: []string{}, pl: pl}
	pl.routing(res)
	res.Settings = append(res.Settings, pl.settings()...)
	res.Warnings = append(res.Warnings, planWarnings(pl.new)...)
	return res
}

// PlanID is a short hash of both configs' text.
func PlanID(old, new *Config) string {
	text := func(c *Config) string { return strings.Join(cmp.Or(c, &Config{}).Lines, "\n") }
	sum := sha256.Sum256([]byte(text(old) + "\x00" + text(new)))
	return hex.EncodeToString(sum[:])[:12]
}

// Empty reports whether the plan changes nothing at all.
func (p *PlanResult) Empty() bool { return len(p.Changes) == 0 && len(p.Settings) == 0 }

// Text renders the plan for a terminal.
func (p *PlanResult) Text() string {
	var b strings.Builder
	if p.Empty() {
		b.WriteString("No changes.\n")
	} else {
		kind := "routing change"
		if p.TooMany {
			kind = "changed rule"
		}
		fmt.Fprintf(&b, "Plan %s: %s, %s\n", p.ID, plural(len(p.Changes), kind), plural(len(p.Settings), "other change"))
	}
	if len(p.Changes) > 0 {
		b.WriteString("Routing\n")
		if p.TooMany {
			fmt.Fprintf(&b, "  (more than %s request classes in a site, so this lists changed rules instead)\n", groupDigits(MaxPlanClasses))
		}
		for _, l := range p.Changes {
			fmt.Fprintf(&b, "  %s, %s\n      %s  ->  %s\n", l.Where, l.What, orNone(l.Old), orNone(l.New))
		}
	}
	list := func(title string, items []string) {
		if len(items) > 0 {
			b.WriteString(title + "\n")
			for _, s := range items {
				fmt.Fprintf(&b, "  %s\n", s)
			}
		}
	}
	list("Other changes", p.Settings)
	list("Warnings", p.Warnings)
	return b.String()
}

// JSON renders the plan for programs.
func (p *PlanResult) JSON() ([]byte, error) { return json.MarshalIndent(p, "", "  ") }

// groupDigits writes a count with commas, such as 1,000,000.
func groupDigits(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func orNone(s string) string { return cmp.Or(s, "(none)") }

// Classify returns the index in Changes of the line whose class holds a
// request, or -1 when the plan lists no change for it. The host has no
// port, and the path is in normal form (NormalizePath). It always returns
// -1 when TooMany is set.
func (p *PlanResult) Classify(port int, host, method, path string, h http.Header) int {
	if p.pl == nil || p.TooMany {
		return -1
	}
	pp := p.pl.byNum[port]
	if pp == nil {
		return -1
	}
	g := pp.byRep[pp.repFor(host)]
	if g == nil {
		return -1
	}
	sp := g.sp
	t := sp.methodRep(method)*sp.ncombo + sp.comboOf(h)
	if l := g.class[[2]int{sp.pathRep(path), t}]; l != nil {
		return l.index
	}
	return -1
}

// Covers reports whether a request falls in a class the plan lists.
func (p *PlanResult) Covers(port int, host, method, path string, h http.Header) bool {
	return p.Classify(port, host, method, path, h) >= 0
}

// RequestEffect says how a config handles a request, in the words plan
// uses: the port, the host's site and the first matching rule decide it.
// The path must be in normal form.
func RequestEffect(c *Config, port int, host, method, path string, h http.Header) string {
	p := c.Ports[port]
	if p == nil {
		return noListener(port)
	}
	s, _ := p.Find(host)
	if s == nil {
		return "421, no site"
	}
	r, _ := s.MatchRoute(method, path, h)
	return routeEffect(s, r)
}

func noListener(port int) string { return fmt.Sprintf("nothing listens on port %d", port) }

func routeEffect(s *Site, r *Route) string {
	if r == nil {
		// The 404 page comes from the files rule that GET for its path
		// reaches (Site.ErrorPage); without one a plain 404 is sent.
		if s.Err404 != "" {
			if er, _ := s.MatchRoute(http.MethodGet, s.Err404, nil); er != nil && er.Act.Kind == "files" {
				return "404, no rule, error page " + s.Err404 + " from " + er.Act.Dir
			}
		}
		return "404, no rule"
	}
	a := r.Act
	switch a.Kind {
	case "pool":
		if a.Strip && r.Path != "" {
			return "pool " + a.Pool + ", strip " + r.Path
		}
		return "pool " + a.Pool
	case "files":
		return "files " + a.Dir
	case "redirect":
		return fmt.Sprintf("redirect %d %s", a.Code, a.URL)
	case "respond":
		if a.Body != "" {
			return fmt.Sprintf("respond %d %s", a.Status, strconv.Quote(a.Body))
		}
		return fmt.Sprintf("respond %d", a.Status)
	case "https":
		return "plain HTTP redirected to https://"
	}
	return a.Kind
}

// planner holds the work behind one plan.
type planner struct {
	old, new *Config
	ports    []*portPlan
	byNum    map[int]*portPlan
}

// portPlan splits the hosts on one port into classes: every exact host in
// either config, one fresh name under each wildcard domain, and one fresh
// name for every other host.
type portPlan struct {
	num        int
	oldP, newP *Port
	exact      map[string]bool
	wild       map[string]string // domain -> representative host
	anyRep     string
	groups     []*hostGroup
	byRep      map[string]*hostGroup
}

// hostGroup is the host classes on a port that go to the same old site and
// the same new site.
type hostGroup struct {
	port       *portPlan
	oldS, newS *Site
	hosts      []string // its host classes, described, in display order
	sp         *space
	class      map[[2]int]*lineAcc // (path rep, method*combos+combo) -> its line
}

func (g *hostGroup) where() string {
	return fmt.Sprintf("%s (port %d)", strings.Join(g.hosts, ", "), g.port.num)
}

func hostDomain(h string) string {
	if i := strings.IndexByte(h, '.'); i > 0 {
		return h[i+1:]
	}
	return ""
}

// keysOf lists the keys of some maps, sorted, each once.
func keysOf[K cmp.Ordered, V any](ms ...map[K]V) []K {
	all := map[K]bool{}
	for _, m := range ms {
		for k := range m {
			all[k] = true
		}
	}
	return slices.Sorted(maps.Keys(all))
}

func (pp *portPlan) repFor(host string) string {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if pp.exact[host] {
		return host
	}
	if r, ok := pp.wild[hostDomain(host)]; ok && hostDomain(host) != "" {
		return r
	}
	return pp.anyRep
}

func (pp *portPlan) describeHost(h string) string {
	switch {
	case h == pp.anyRep:
		return "any other host"
	case pp.exact[h]:
		return h
	}
	return "*." + hostDomain(h)
}

func newPlanner(old, new *Config) *planner {
	pl := &planner{old: old, new: new, byNum: map[int]*portPlan{}}
	for _, n := range keysOf(old.Ports, new.Ports) {
		pp := &portPlan{num: n, oldP: old.Ports[n], newP: new.Ports[n],
			exact: map[string]bool{}, wild: map[string]string{}, byRep: map[string]*hostGroup{}}
		op, np := cmp.Or(pp.oldP, &Port{}), cmp.Or(pp.newP, &Port{}) // a missing port has no sites
		reps := keysOf(op.Exact, np.Exact)
		for _, h := range reps {
			pp.exact[h] = true
		}
		// Representatives are fresh names: bp-any.DOMAIN, or bp-any2.DOMAIN
		// and so on when a config names that host.
		for _, d := range keysOf(op.Wild, np.Wild) {
			h := "bp-any." + d
			for i := 2; pp.exact[h]; i++ {
				h = "bp-any" + strconv.Itoa(i) + "." + d
			}
			pp.wild[d] = h
			reps = append(reps, h)
		}
		taken := func(h string) bool {
			_, w := pp.wild[hostDomain(h)]
			return w || pp.exact[h]
		}
		pp.anyRep = "bp-any.invalid"
		for i := 2; taken(pp.anyRep); i++ {
			pp.anyRep = "bp-any.bp-" + strconv.Itoa(i) + ".invalid"
		}
		reps = append(reps, pp.anyRep)
		byPair := map[[2]*Site]*hostGroup{}
		for _, h := range reps {
			os, _ := op.Find(h)
			ns, _ := np.Find(h)
			k := [2]*Site{os, ns}
			g := byPair[k]
			if g == nil {
				g = &hostGroup{port: pp, oldS: os, newS: ns, sp: newSpace(os, ns)}
				byPair[k] = g
				pp.groups = append(pp.groups, g)
			}
			g.hosts = append(g.hosts, pp.describeHost(h))
			pp.byRep[h] = g
		}
		pl.ports = append(pl.ports, pp)
		pl.byNum[n] = pp
	}
	return pl
}

// effect is how one side handles a class representative, with the scheme
// in front when the port switches between http and https.
// rules holds that side's rules for the path (rulesFor): only they can
// match it, so the matcher runs on them alone.
func (g *hostGroup) effect(old bool, rules *Site, method, path string, h http.Header) string {
	p, other, s := g.port.newP, g.port.oldP, g.newS
	if old {
		p, other, s = g.port.oldP, g.port.newP, g.oldS
	}
	if p == nil {
		return noListener(g.port.num)
	}
	prefix := ""
	if other != nil && other.TLS != p.TLS {
		prefix = schemeName[p.TLS] + ": "
	}
	if s == nil {
		return prefix + "421, no site"
	}
	r, _ := rules.MatchRoute(method, path, h)
	return prefix + routeEffect(s, r)
}

// lineAcc is one routing line before sorting.
type lineAcc struct {
	index   int // in Changes, once sorted
	sortKey string
	line    PlanLine
}

// routing lists the routing changes, port by port and host group by host
// group, each group's lines in order.
func (pl *planner) routing(res *PlanResult) {
	for _, pp := range pl.ports {
		for _, g := range pp.groups {
			res.TooMany = res.TooMany || g.sp.count() > MaxPlanClasses
		}
	}
	for _, pp := range pl.ports {
		for _, g := range pp.groups {
			if res.TooMany {
				res.Changes = append(res.Changes, ruleDiff(g)...)
				continue
			}
			for _, l := range g.analyze() {
				l.index = len(res.Changes)
				res.Changes = append(res.Changes, l.line)
			}
		}
	}
}

type effPair struct{ old, new string }

// analyze runs every class of a host group through both configs and turns
// the classes whose effect changes into lines, sorted.
func (g *hostGroup) analyze() []*lineAcc {
	o, n := g.port.oldP, g.port.newP
	samePort := o != nil && n != nil && o.TLS == n.TLS
	if samePort && sameRules(g.oldS, g.newS) {
		return nil // the matcher picks the same rule with the same effect for every request
	}
	sp := g.sp
	// A path whose rules (those whose path pattern takes it) are the same
	// on both sides is handled the same for every method and header.
	sameElse := samePort && g.oldS != nil && g.newS != nil && routeEffect(g.oldS, nil) == routeEffect(g.newS, nil)
	fo, fn, same := make([]*Site, len(sp.reps)), make([]*Site, len(sp.reps)), make([]bool, len(sp.reps))
	for pi, path := range sp.reps {
		fo[pi], fn[pi] = pathSite(g.oldS, path), pathSite(g.newS, path)
		same[pi] = sameElse && sameList(fo[pi].Routes, fn[pi].Routes)
	}
	type pathEff struct {
		pi int
		e  effPair
	}
	changed := map[pathEff][]int{} // tuples whose effect changes, by path and effects
	for c := 0; c < sp.ncombo; c++ {
		h := sp.header(c)
		for pi, path := range sp.reps {
			if same[pi] {
				continue
			}
			for mi, m := range sp.methods {
				if eo, en := g.effect(true, fo[pi], m, path, h), g.effect(false, fn[pi], m, path, h); eo != en {
					k := pathEff{pi, effPair{eo, en}}
					changed[k] = append(changed[k], mi*sp.ncombo+c)
				}
			}
		}
	}
	// Each path's tuples split into products of method and header states;
	// one line holds a product with its effects on every path that has it.
	type key struct {
		e    effPair
		prod string
	}
	type merged struct {
		e     effPair
		prod  [][]int
		paths map[int]bool
	}
	groups := map[key]*merged{}
	for pe, ts := range changed {
		digits := make([][]int, len(ts))
		for i, t := range ts {
			digits[i] = sp.digits(t)
		}
		for _, prod := range factor(digits) {
			k := key{pe.e, fmt.Sprint(prod)}
			m := groups[k]
			if m == nil {
				m = &merged{e: pe.e, prod: prod, paths: map[int]bool{}}
				groups[k] = m
			}
			m.paths[pe.pi] = true
		}
	}
	// Lines come out of the map in no set order; they are sorted below.
	g.class = map[[2]int]*lineAcc{}
	var lines []*lineAcc
	for _, m := range groups {
		what := []string{sp.methodPhrase(m.prod[0])}
		for i := range sp.names {
			if s := sp.headerPhrase(i, m.prod[i+1]); s != "" {
				what = append(what, s)
			}
		}
		what = append(what, strings.Join(describe(sp.root, m.paths), "; "))
		sortKey := "\xff"
		for pi := range m.paths {
			sortKey = min(sortKey, sp.repSort[pi])
		}
		l := &lineAcc{sortKey: sortKey, line: PlanLine{Where: g.where(), What: strings.Join(what, ", "), Old: m.e.old, New: m.e.new}}
		lines = append(lines, l)
		for pi := range m.paths {
			for _, t := range sp.tuples(m.prod) {
				g.class[[2]int{pi, t}] = l
			}
		}
	}
	slices.SortStableFunc(lines, func(a, b *lineAcc) int {
		return cmp.Or(strings.Compare(a.sortKey, b.sortKey), strings.Compare(a.line.What, b.line.What),
			strings.Compare(a.line.Old, b.line.Old), strings.Compare(a.line.New, b.line.New))
	})
	return lines
}

// sameRules reports whether two sites have the same rules in the same
// order, each with the same matchers and effect, and the same no-rule
// effect. Then every request is handled the same by both.
func sameRules(a, b *Site) bool {
	if a == nil || b == nil {
		return a == b
	}
	return routeEffect(a, nil) == routeEffect(b, nil) && sameList(a.Routes, b.Routes)
}

// sameList reports whether two lists of rules have the same matchers and
// effects in the same order.
func sameList(a, b []*Route) bool {
	if len(a) != len(b) {
		return false
	}
	for i, x := range a {
		y := b[i]
		if x.Path != y.Path || x.Prefix != y.Prefix || strings.Join(x.Methods, ",") != strings.Join(y.Methods, ",") ||
			fmt.Sprint(x.Headers) != fmt.Sprint(y.Headers) || routeEffect(nil, x) != routeEffect(nil, y) {
			return false
		}
	}
	return true
}

// rulesFor lists a site's rules whose path pattern takes a path (the path
// part of Route.matches), in order.
func rulesFor(s *Site, path string) []*Route {
	var out []*Route
	for _, r := range s.Routes {
		if r.Prefix && (r.Path == "" || path == r.Path || strings.HasPrefix(path, r.Path+"/")) || !r.Prefix && path == r.Path {
			out = append(out, r)
		}
	}
	return out
}

// factor splits a set of distinct tuples into a few cartesian products.
// Each result holds one sorted set of values per dimension.
func factor(ts [][]int) [][][]int {
	proj := make([][]int, len(ts[0]))
	total := 1
	for d := range proj {
		for _, t := range ts {
			proj[d] = append(proj[d], t[d])
		}
		slices.Sort(proj[d])
		proj[d] = slices.Compact(proj[d])
		if total <= len(ts) {
			total *= len(proj[d])
		}
	}
	if total == len(ts) {
		return [][][]int{proj}
	}
	rest := map[int][][]int{}
	for _, t := range ts {
		rest[t[0]] = append(rest[t[0]], t[1:])
	}
	type part struct {
		vals []int
		rest [][]int
	}
	var parts []*part
	byKey := map[string]*part{}
	for _, v := range proj[0] {
		r := rest[v]
		slices.SortFunc(r, slices.Compare[[]int])
		k := fmt.Sprint(r)
		p := byKey[k]
		if p == nil {
			p = &part{rest: r}
			byKey[k] = p
			parts = append(parts, p)
		}
		p.vals = append(p.vals, v)
	}
	var out [][][]int
	for _, p := range parts {
		for _, sub := range factor(p.rest) {
			out = append(out, append([][]int{p.vals}, sub...))
		}
	}
	return out
}

// space is the classes of requests a site pair can tell apart: path
// representatives, method representatives and header combinations.
type space struct {
	seg     string
	reps    []string          // representative paths
	repSort []string          // sort key per representative
	nodes   map[string]*pnode // pattern path ("" for the root) -> its node
	root    *pnode
	methods []string   // named methods, HEAD when GET is named, then one other
	names   []string   // header names in conditions, canonical
	vals    [][]string // named values per header name
	other   []string   // a value per header name that no condition names
	nstates []int      // absent, then present with each subset of the named values
	ncombo  int
	big     bool
}

// pnode is a path pattern in the tree of patterns. The root has path "".
type pnode struct {
	path  string
	self  int // representative of the path itself; -1 for the root
	below int // representative of a fresh child; -1 when the path ends in /
	kids  []*pnode
}

func newSpace(a, b *Site) *space {
	routes := slices.Concat(routesOf(a), routesOf(b))
	pats := map[string]bool{}
	methods := map[string]bool{}
	hv := map[string]map[string]bool{}
	for _, r := range routes {
		if r.Path != "" {
			pats[r.Path] = true
		}
		for _, m := range r.Methods {
			methods[m] = true
		}
		for _, c := range r.Headers {
			if hv[c.Name] == nil {
				hv[c.Name] = map[string]bool{}
			}
			if c.HasValue {
				hv[c.Name][c.Value] = true
			}
		}
	}
	list := keysOf(pats)
	// seg is a path segment no pattern holds, for fresh paths.
	sp := &space{seg: "~bp"}
	for n := 2; slices.ContainsFunc(list, func(p string) bool { return strings.Contains(p, sp.seg) }); n++ {
		sp.seg = "~bp" + strconv.Itoa(n)
	}
	add := func(path, sortKey string) int {
		sp.reps = append(sp.reps, path)
		sp.repSort = append(sp.repSort, sortKey)
		return len(sp.reps) - 1
	}
	sp.root = &pnode{self: -1}
	sp.root.below = add("/"+sp.seg, "\xfe")
	sp.nodes = map[string]*pnode{"": sp.root}
	for _, p := range list {
		n := &pnode{path: p, self: add(p, p), below: -1}
		if !strings.HasSuffix(p, "/") {
			n.below = add(p+"/"+sp.seg, p+"/\xfe")
		}
		sp.nodes[p] = n
	}
	// A pattern's parent is the longest pattern that ends where one of its
	// slashes starts (the root, "", at worst).
	for _, p := range list {
		for q := p; ; {
			q = q[:strings.LastIndexByte(q, '/')]
			if parent := sp.nodes[q]; parent != nil {
				parent.kids = append(parent.kids, sp.nodes[p])
				break
			}
		}
	}
	if methods["GET"] {
		methods["HEAD"] = true
	}
	other := "BPOTHER"
	for methods[other] {
		other += "X"
	}
	sp.methods = append(keysOf(methods), other)
	sp.names = keysOf(hv)
	sp.ncombo = 1
	for _, name := range sp.names {
		vs := keysOf(hv[name])
		o := "~bp-other"
		for n := 2; hv[name][o]; n++ {
			o = "~bp-other" + strconv.Itoa(n)
		}
		ns := 1 << 21
		if len(vs) <= 20 {
			ns = 1 + 1<<len(vs)
		} else {
			sp.big = true
		}
		sp.vals = append(sp.vals, vs)
		sp.other = append(sp.other, o)
		sp.nstates = append(sp.nstates, ns)
		if sp.ncombo <= MaxPlanClasses {
			sp.ncombo *= ns
		} else {
			sp.big = true
		}
	}
	return sp
}

// count is the number of classes, or more than MaxPlanClasses when there
// are too many to count.
func (sp *space) count() int {
	n := len(sp.reps) * len(sp.methods)
	if sp.big || sp.ncombo > MaxPlanClasses || n > MaxPlanClasses || n*sp.ncombo > MaxPlanClasses {
		return MaxPlanClasses + 1
	}
	return n * sp.ncombo
}

// digits splits a tuple (method*combos+combo) into method and header states.
func (sp *space) digits(t int) []int {
	d := make([]int, 1+len(sp.names))
	d[0] = t / sp.ncombo
	c := t % sp.ncombo
	for i := len(sp.names) - 1; i >= 0; i-- {
		d[i+1] = c % sp.nstates[i]
		c /= sp.nstates[i]
	}
	return d
}

// tuples lists every tuple in a product, digit by digit (the reverse of
// digits).
func (sp *space) tuples(prod [][]int) []int {
	out := []int{0}
	for i, set := range prod {
		radix := 1 // the method digit comes first, onto 0
		if i > 0 {
			radix = sp.nstates[i-1]
		}
		var next []int
		for _, t := range out {
			for _, v := range set {
				next = append(next, t*radix+v)
			}
		}
		out = next
	}
	return out
}

// header builds the request headers of one combination.
func (sp *space) header(c int) http.Header {
	h := http.Header{}
	for i, st := range sp.digits(c)[1:] {
		switch {
		case st == 1:
			h[sp.names[i]] = []string{sp.other[i]}
		case st > 1:
			h[sp.names[i]] = sp.valsIn(i, st-1)
		}
	}
	return h
}

// valsIn lists the named values of header i whose bits are set in mask.
func (sp *space) valsIn(i, mask int) []string {
	var vs []string
	for j, v := range sp.vals[i] {
		if mask&(1<<j) != 0 {
			vs = append(vs, v)
		}
	}
	return vs
}

func (sp *space) comboOf(h http.Header) int {
	c := 0
	for i, name := range sp.names {
		st := 0
		if vals := h.Values(name); len(vals) > 0 {
			mask := 0
			for j, v := range sp.vals[i] {
				if slices.Contains(vals, v) {
					mask |= 1 << j
				}
			}
			st = 1 + mask
		}
		c = c*sp.nstates[i] + st
	}
	return c
}

func (sp *space) methodRep(m string) int {
	if i := slices.Index(sp.methods, m); i >= 0 {
		return i
	}
	return len(sp.methods) - 1
}

// pathRep returns the representative of a path in normal form: the
// pattern itself, or the fresh child of the nearest pattern above it.
func (sp *space) pathRep(p string) int {
	if n := sp.nodes[p]; n != nil && n.self >= 0 {
		return n.self
	}
	for i := strings.LastIndexByte(p, '/'); i >= 0; i = strings.LastIndexByte(p[:i], '/') {
		if n := sp.nodes[p[:i]]; n != nil && n.below >= 0 {
			return n.below
		}
	}
	return sp.root.below
}

func (sp *space) methodPhrase(set []int) string {
	if len(set) == len(sp.methods) {
		return "any method"
	}
	otherIx := len(sp.methods) - 1
	var names []string
	if slices.Contains(set, otherIx) {
		for i, m := range sp.methods[:otherIx] {
			if !slices.Contains(set, i) {
				names = append(names, m)
			}
		}
		return "any method except " + joinAnd(names)
	}
	for _, i := range set {
		names = append(names, sp.methods[i])
	}
	return joinAnd(names)
}

// headerPhrase describes a set of states of one header name, or returns ""
// when the set holds every state.
func (sp *space) headerPhrase(i int, set []int) string {
	name, vals, ns := sp.names[i], sp.vals[i], sp.nstates[i]
	if len(set) == ns {
		return ""
	}
	in := map[int]bool{}
	for _, s := range set {
		in[s] = true
	}
	if len(set) == ns-1 && !in[0] {
		return "with " + name
	}
	if len(set) == 1 && in[0] {
		return "without " + name
	}
	// with NAME: v, or without NAME: v, when the set is exactly the states
	// that hold v, or exactly those that don't.
	for j, v := range vals {
		agree := 0 // states where holding v and being in the set agree
		for st := 0; st < ns; st++ {
			if has := st > 0 && (st-1)&(1<<j) != 0; has == in[st] {
				agree++
			}
		}
		switch agree {
		case ns:
			return "with " + name + ": " + v
		case 0:
			return "without " + name + ": " + v
		}
	}
	var parts []string
	for _, st := range set {
		switch {
		case st == 0:
			parts = append(parts, "without "+name)
		case st == 1:
			parts = append(parts, "with "+name+" other than "+strings.Join(vals, ", "))
		default:
			parts = append(parts, "with "+name+": "+strings.Join(sp.valsIn(i, st-1), " and "))
		}
	}
	return strings.Join(parts, " or ")
}

// all reports whether every representative at and below a node is in the
// set (want true) or out of it (want false). With self false it skips the
// node's own path.
func all(n *pnode, in map[int]bool, want, self bool) bool {
	if self && n.self >= 0 && in[n.self] != want || n.below >= 0 && in[n.below] != want {
		return false
	}
	for _, k := range n.kids {
		if !all(k, in, want, true) {
			return false
		}
	}
	return true
}

func subtreeText(n *pnode) string {
	if n.below < 0 && len(n.kids) == 0 {
		return n.path
	}
	return n.path + " and below"
}

// describe phrases the paths at and below a node whose representatives
// are in the set.
func describe(n *pnode, in map[int]bool) []string {
	if all(n, in, false, true) {
		return nil
	}
	if all(n, in, true, true) {
		if n.self < 0 {
			return []string{"every path"}
		}
		return []string{subtreeText(n)}
	}
	selfIn := n.self >= 0 && in[n.self]
	var out []string
	if n.below < 0 || !in[n.below] {
		if selfIn {
			out = append(out, n.path)
		}
		for _, k := range n.kids {
			out = append(out, describe(k, in)...)
		}
		return out
	}
	var exc []string
	var later []*pnode
	for _, k := range n.kids {
		switch {
		case all(k, in, true, true):
		case k.self >= 0 && !in[k.self] && all(k, in, true, false):
			exc = append(exc, k.path)
		default:
			exc = append(exc, subtreeText(k))
			if !all(k, in, false, true) {
				later = append(later, k)
			}
		}
	}
	var s string
	switch {
	case n.self < 0 && len(exc) > 0:
		s = "every other path"
	case n.self < 0:
		s = "every path"
	case selfIn:
		s = n.path + " and below"
	default:
		s = "below " + n.path
	}
	if len(exc) > 0 {
		s += " (not " + strings.Join(exc, ", ") + ")"
	}
	out = append(out, s)
	for _, k := range later {
		out = append(out, describe(k, in)...)
	}
	return out
}

func joinAnd(xs []string) string {
	if len(xs) < 2 {
		return strings.Join(xs, "")
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

// ruleDiff lists the rule lines that differ between a group's two sites,
// for plans with too many classes to work through.
func ruleDiff(g *hostGroup) []PlanLine {
	a, b := routesOf(g.oldS), routesOf(g.newS)
	// longest common subsequence of rule texts
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i].Text == b[j].Text {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []PlanLine
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i].Text == b[j].Text:
			i++
			j++
		case j >= m || (i < n && lcs[i+1][j] >= lcs[i][j+1]):
			out = append(out, PlanLine{Where: g.where(), What: "rule removed", Old: fmt.Sprintf("line %d: %s", a[i].Line, a[i].Text)})
			i++
		default:
			out = append(out, PlanLine{Where: g.where(), What: "rule added", New: fmt.Sprintf("line %d: %s", b[j].Line, b[j].Text)})
			j++
		}
	}
	return out
}

// planWarnings checks the new config for rules that win no class and pools
// that no rule able to match uses.
func planWarnings(c *Config) []string {
	type warn struct {
		line int
		msg  string
	}
	var ws []warn
	used := map[string]bool{}
	for _, s := range c.Sites {
		if s.Synthetic {
			continue
		}
		sp := newSpace(s, nil)
		if sp.count() > MaxPlanClasses {
			ws = append(ws, warn{s.Line, fmt.Sprintf("line %d: site %s has too many request classes to check for rules that never match", s.Line, s.Name)})
			for _, r := range s.Routes {
				used[r.Act.Pool] = true
			}
			continue
		}
		won := map[*Route]bool{}
		sp.each(s, func(fs *Site, m, p string, h http.Header) {
			if r, _ := fs.MatchRoute(m, p, h); r != nil {
				won[r] = true
			}
		})
		for _, r := range s.Routes {
			if won[r] {
				if r.Act.Kind == "pool" {
					used[r.Act.Pool] = true
				}
				continue
			}
			takers := map[int]bool{}
			sp.each(s, func(fs *Site, m, p string, h http.Header) {
				if ok, _ := r.matches(m, p, h); ok {
					if w, _ := fs.MatchRoute(m, p, h); w != nil {
						takers[w.Line] = true
					}
				}
			})
			var ls []string
			for _, l := range keysOf(takers) {
				ls = append(ls, strconv.Itoa(l))
			}
			msg := fmt.Sprintf("line %d: %s never matches", r.Line, r.Text)
			if len(ls) == 1 {
				msg += ": line " + ls[0] + " takes every request it would get"
			} else if len(ls) > 1 {
				msg += ": lines " + joinAnd(ls) + " take every request it would get"
			}
			ws = append(ws, warn{r.Line, msg})
		}
	}
	for _, name := range c.PoolOrder {
		if ps := c.Pools[name]; !used[name] {
			ws = append(ws, warn{ps.Line, fmt.Sprintf("line %d: pool %s isn't used by any rule that can match", ps.Line, name)})
		}
	}
	slices.SortStableFunc(ws, func(a, b warn) int { return cmp.Compare(a.line, b.line) })
	var out []string
	for _, w := range ws {
		out = append(out, w.msg)
	}
	return out
}

// each calls f for every class representative of a site, with the
// site's rules for the representative's path (pathSite).
func (sp *space) each(s *Site, f func(rules *Site, method, path string, h http.Header)) {
	fs := make([]*Site, len(sp.reps))
	for pi, p := range sp.reps {
		fs[pi] = pathSite(s, p)
	}
	for c := 0; c < sp.ncombo; c++ {
		h := sp.header(c)
		for pi, p := range sp.reps {
			for _, m := range sp.methods {
				f(fs[pi], m, p, h)
			}
		}
	}
}

func routesOf(s *Site) []*Route {
	if s == nil {
		return nil
	}
	return s.Routes
}

// pathSite is a site cut down to its rules for one path, or nil.
func pathSite(s *Site, path string) *Site {
	if s == nil {
		return nil
	}
	return &Site{Line: s.Line, Name: s.Name, Routes: rulesFor(s, path)}
}

// schemeName names a port's scheme by whether it serves TLS.
var schemeName = map[bool]string{false: "http", true: "https"}

// settings lists the changes outside routing, one line each.
func (pl *planner) settings() []string {
	var out []string
	for _, pp := range pl.ports {
		switch {
		case pp.oldP == nil:
			out = append(out, fmt.Sprintf("port %d (%s) added (line %d)", pp.num, schemeName[pp.newP.TLS], pp.newP.Line))
		case pp.newP == nil:
			out = append(out, fmt.Sprintf("port %d (%s) removed (was line %d)", pp.num, schemeName[pp.oldP.TLS], pp.oldP.Line))
		case pp.oldP.TLS != pp.newP.TLS:
			out = append(out, fmt.Sprintf("port %d switched from %s to %s (line %d)", pp.num, schemeName[pp.oldP.TLS], schemeName[pp.newP.TLS], pp.newP.Line))
		}
	}
	out = append(out, globalChanges(pl.old, pl.new)...)
	out = append(out, pl.siteChanges()...)
	out = append(out, poolChanges(pl.old, pl.new)...)
	return out
}

type cfgLine struct {
	text string
	line int
}

// globalLines returns the settings in a config's global blocks by name.
func globalLines(c *Config) map[string]cfgLine {
	out := map[string]cfgLine{}
	in := false
	for i, raw := range c.Lines {
		toks, err := splitLine(raw)
		if err != nil || len(toks) == 0 {
			continue
		}
		switch {
		case raw[0] != ' ' && raw[0] != '\t':
			in = toks[0].s == "global"
		case in:
			out[toks[0].s] = cfgLine{joinTokens(toks), i + 1}
		}
	}
	return out
}

// settingLine finds the last line in the block starting at line start that
// sets key, or 0.
func settingLine(c *Config, start int, key string) int {
	found := 0
	for i := start; i < len(c.Lines); i++ {
		raw := c.Lines[i]
		toks, err := splitLine(raw)
		if err != nil || len(toks) == 0 {
			continue
		}
		if raw[0] != ' ' && raw[0] != '\t' {
			break
		}
		if toks[0].s == key {
			found = i + 1
		}
	}
	return found
}

func lineRef(newLine, oldLine int) string {
	switch {
	case newLine > 0:
		return fmt.Sprintf(" (line %d)", newLine)
	case oldLine > 0:
		return fmt.Sprintf(" (was line %d)", oldLine)
	}
	return ""
}

func globalChanges(old, new *Config) []string {
	o, n := globalLines(old), globalLines(new)
	var out []string
	for _, k := range keysOf(o, n) {
		a, ina := o[k]
		b, inb := n[k]
		switch {
		case !ina:
			out = append(out, fmt.Sprintf("global %s added (line %d)", b.text, b.line))
		case !inb:
			out = append(out, fmt.Sprintf("global %s removed (was line %d)", a.text, a.line))
		case a.text != b.text:
			out = append(out, fmt.Sprintf("global %s  ->  %s (line %d)", a.text, b.text, b.line))
		}
	}
	return out
}

func (pl *planner) siteChanges() []string {
	var out []string
	seen := map[[2]*Site]bool{}
	for _, pp := range pl.ports {
		for _, g := range pp.groups {
			a, b := g.oldS, g.newS
			k := [2]*Site{a, b}
			if a == nil || b == nil || a.Synthetic || b.Synthetic || seen[k] {
				continue
			}
			seen[k] = true
			name := "site " + siteLabel(b)
			if siteLabel(a) != siteLabel(b) {
				name += " (was site " + siteLabel(a) + ")"
			}
			// set adds a line when a setting differs. The line numbers
			// are those that set it in each site's block, unless given.
			set := func(key, from, to string, lines ...int) {
				if from == to {
					return
				}
				if lines == nil {
					lines = []int{settingLine(pl.new, b.Line, key), settingLine(pl.old, a.Line, key)}
				}
				out = append(out, fmt.Sprintf("%s: %s %s  ->  %s%s", name, key, from, to, lineRef(lines[0], lines[1])))
			}
			set("tls", tlsText(a), tlsText(b), b.TLSLine, a.TLSLine)
			set("error 404 page", orNone(a.Err404), orNone(b.Err404), b.Err404Line, a.Err404Line)
			set("body-limit", sizeText(a.BodyLimit), sizeText(b.BodyLimit))
			slashes := map[bool]string{false: "reject", true: "keep"}
			set("encoded-slashes", slashes[a.KeepEncodedSlash], slashes[b.KeepEncodedSlash])
		}
	}
	return out
}

func tlsText(s *Site) string {
	switch {
	case s.TLSAuto:
		return "auto"
	case s.CertFile != "":
		return s.CertFile + " " + s.KeyFile
	}
	return "none"
}

// siteLabel names a site by its first address as written, so sites with
// the same host on different ports stay apart.
func siteLabel(s *Site) string {
	if len(s.Addrs) > 0 {
		return s.Addrs[0].Text
	}
	return s.Name
}

// sizeText writes a size in the largest unit that divides it.
func sizeText(n int64) string {
	for i, unit := range []string{"GB", "MB", "KB"} {
		if shift := 30 - 10*i; n > 0 && n%(1<<shift) == 0 {
			return fmt.Sprintf("%d%s", n>>shift, unit)
		}
	}
	return fmt.Sprintf("%dB", n)
}

func healthText(h *HealthSpec) string {
	if h == nil {
		return "none"
	}
	return fmt.Sprintf("%s every %s timeout %s expect %d-%d", h.Path, h.Every, h.Timeout, h.Lo, h.Hi)
}

func poolChanges(old, new *Config) []string {
	var out []string
	for _, name := range keysOf(old.Pools, new.Pools) {
		a, b := old.Pools[name], new.Pools[name]
		switch {
		case a == nil:
			out = append(out, fmt.Sprintf("pool %s added (line %d), %s", name, b.Line, plural(len(b.Backends), "backend")))
			continue
		case b == nil:
			out = append(out, fmt.Sprintf("pool %s removed (was line %d)", name, a.Line))
			continue
		}
		key := func(x BackendSpec) string {
			if x.HTTPS {
				return "https://" + x.Addr
			}
			return x.Addr
		}
		// missing lists the backends of xs that ys lacks.
		missing := func(xs, ys []BackendSpec, format string) {
			for _, x := range xs {
				if !slices.ContainsFunc(ys, func(y BackendSpec) bool { return key(y) == key(x) }) {
					out = append(out, fmt.Sprintf(format, name, key(x), x.Line))
				}
			}
		}
		missing(a.Backends, b.Backends, "pool %s: backend %s removed (was line %d)")
		missing(b.Backends, a.Backends, "pool %s: backend %s added (line %d)")
		set := func(what, from, to string) {
			if from != to {
				out = append(out, fmt.Sprintf("pool %s: %s %s  ->  %s%s", name, what, from, to,
					lineRef(settingLine(new, b.Line, what), settingLine(old, a.Line, what))))
			}
		}
		set("health", healthText(a.Health), healthText(b.Health))
		set("host-header", orNone(a.HostHeader), orNone(b.HostHeader))
		set("connect-timeout", a.ConnectTimeout.String(), b.ConnectTimeout.String())
		set("response-timeout", a.ResponseTimeout.String(), b.ResponseTimeout.String())
		set("retries", strconv.Itoa(a.Retries), strconv.Itoa(b.Retries))
	}
	return out
}
