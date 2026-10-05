// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
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
	res := &PlanResult{ID: PlanID(old, new), Changes: []PlanLine{}, Settings: []string{}, Warnings: []string{}}
	if old == nil {
		old = &Config{Pools: map[string]*PoolSpec{}, Ports: map[int]*Port{}}
	}
	if new == nil {
		new = &Config{Pools: map[string]*PoolSpec{}, Ports: map[int]*Port{}}
	}
	pl := newPlanner(old, new)
	res.pl = pl
	pl.routing(res)
	res.Settings = append(res.Settings, pl.settings()...)
	res.Warnings = append(res.Warnings, planWarnings(new)...)
	return res
}

// PlanID is a short hash of both configs' text.
func PlanID(old, new *Config) string {
	text := func(c *Config) string {
		if c == nil {
			return ""
		}
		return strings.Join(c.Lines, "\n")
	}
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
		if len(p.Changes) > 0 {
			b.WriteString("Routing\n")
			if p.TooMany {
				fmt.Fprintf(&b, "  (more than %s request classes in a site, so this lists changed rules instead)\n", groupDigits(MaxPlanClasses))
			}
			for _, l := range p.Changes {
				fmt.Fprintf(&b, "  %s, %s\n      %s  ->  %s\n", l.Where, l.What, orNone(l.Old), orNone(l.New))
			}
		}
		if len(p.Settings) > 0 {
			b.WriteString("Other changes\n")
			for _, s := range p.Settings {
				fmt.Fprintf(&b, "  %s\n", s)
			}
		}
	}
	if len(p.Warnings) > 0 {
		b.WriteString("Warnings\n")
		for _, w := range p.Warnings {
			fmt.Fprintf(&b, "  %s\n", w)
		}
	}
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

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

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
	if g == nil || g.class == nil {
		return -1
	}
	sp := g.sp
	t := sp.methodRep(method)*sp.ncombo + sp.comboOf(h)
	id, ok := g.class[[2]int{sp.pathRep(path), t}]
	if !ok {
		return -1
	}
	return p.pl.lineIdx[id]
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
	lineIdx  []int // line id -> index in Changes
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
	hosts      []string // representative hosts, in display order
	where      string
	sp         *space
	class      map[[2]int]int // (path rep, method*combos+combo) -> line id
}

func hostDomain(h string) string {
	if i := strings.IndexByte(h, '.'); i > 0 {
		return h[i+1:]
	}
	return ""
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

func findSite(p *Port, host string) *Site {
	if p == nil {
		return nil
	}
	s, _ := p.Find(host)
	return s
}

func newPlanner(old, new *Config) *planner {
	pl := &planner{old: old, new: new, byNum: map[int]*portPlan{}}
	var nums []int
	for n := range old.Ports {
		nums = append(nums, n)
	}
	for n := range new.Ports {
		if old.Ports[n] == nil {
			nums = append(nums, n)
		}
	}
	sort.Ints(nums)
	for _, n := range nums {
		pp := &portPlan{num: n, oldP: old.Ports[n], newP: new.Ports[n],
			exact: map[string]bool{}, wild: map[string]string{}, byRep: map[string]*hostGroup{}}
		for _, p := range []*Port{pp.oldP, pp.newP} {
			if p == nil {
				continue
			}
			for h := range p.Exact {
				pp.exact[h] = true
			}
			for d := range p.Wild {
				pp.wild[d] = ""
			}
		}
		var domains []string
		for d := range pp.wild {
			domains = append(domains, d)
			for i := 1; ; i++ {
				h := "bp-any." + d
				if i > 1 {
					h = "bp-any" + strconv.Itoa(i) + "." + d
				}
				if !pp.exact[h] {
					pp.wild[d] = h
					break
				}
			}
		}
		for i := 1; ; i++ {
			h := "bp-any.invalid"
			if i > 1 {
				h = "bp-any.bp-" + strconv.Itoa(i) + ".invalid"
			}
			if _, w := pp.wild[hostDomain(h)]; !w && !pp.exact[h] {
				pp.anyRep = h
				break
			}
		}
		var reps []string
		for h := range pp.exact {
			reps = append(reps, h)
		}
		sort.Strings(reps)
		sort.Strings(domains)
		for _, d := range domains {
			reps = append(reps, pp.wild[d])
		}
		reps = append(reps, pp.anyRep)
		byPair := map[[2]*Site]*hostGroup{}
		for _, h := range reps {
			os, ns := findSite(pp.oldP, h), findSite(pp.newP, h)
			k := [2]*Site{os, ns}
			g := byPair[k]
			if g == nil {
				g = &hostGroup{port: pp, oldS: os, newS: ns}
				byPair[k] = g
				pp.groups = append(pp.groups, g)
			}
			g.hosts = append(g.hosts, h)
			pp.byRep[h] = g
		}
		for _, g := range pp.groups {
			var ds []string
			for _, h := range g.hosts {
				ds = append(ds, pp.describeHost(h))
			}
			g.where = fmt.Sprintf("%s (port %d)", strings.Join(ds, ", "), n)
			g.sp = newSpace(g.oldS, g.newS)
		}
		pl.ports = append(pl.ports, pp)
		pl.byNum[n] = pp
	}
	return pl
}

// effect is how one side handles a class representative, with the scheme
// in front when the port switches between http and https.
func (g *hostGroup) effect(old bool, method, path string, h http.Header) string {
	p, other, s := g.port.newP, g.port.oldP, g.newS
	if old {
		p, other, s = g.port.oldP, g.port.newP, g.oldS
	}
	if p == nil {
		return noListener(g.port.num)
	}
	prefix := ""
	if other != nil && other.TLS != p.TLS {
		prefix = "http: "
		if p.TLS {
			prefix = "https: "
		}
	}
	if s == nil {
		return prefix + "421, no site"
	}
	r, _ := s.MatchRoute(method, path, h)
	return prefix + routeEffect(s, r)
}

// lineAcc is one routing line before sorting.
type lineAcc struct {
	id      int
	port    int
	group   int
	sortKey string
	line    PlanLine
}

func (pl *planner) routing(res *PlanResult) {
	for _, pp := range pl.ports {
		for _, g := range pp.groups {
			if g.sp.count() > MaxPlanClasses {
				res.TooMany = true
			}
		}
	}
	var lines []*lineAcc
	for _, pp := range pl.ports {
		for gi, g := range pp.groups {
			if res.TooMany {
				for _, l := range ruleDiff(g) {
					lines = append(lines, &lineAcc{id: len(lines), port: pp.num, group: gi, sortKey: fmt.Sprintf("%08d", len(lines)), line: l})
				}
				continue
			}
			lines = pl.analyze(g, gi, lines)
		}
	}
	sort.SliceStable(lines, func(i, j int) bool {
		a, b := lines[i], lines[j]
		if a.port != b.port {
			return a.port < b.port
		}
		if a.group != b.group {
			return a.group < b.group
		}
		if a.sortKey != b.sortKey {
			return a.sortKey < b.sortKey
		}
		if a.line.What != b.line.What {
			return a.line.What < b.line.What
		}
		if a.line.Old != b.line.Old {
			return a.line.Old < b.line.Old
		}
		return a.line.New < b.line.New
	})
	pl.lineIdx = make([]int, len(lines))
	for i, l := range lines {
		pl.lineIdx[l.id] = i
		res.Changes = append(res.Changes, l.line)
	}
}

type effPair struct{ old, new string }

// analyze runs every class of a host group through both configs and turns
// the classes whose effect changes into lines.
func (pl *planner) analyze(g *hostGroup, gi int, lines []*lineAcc) []*lineAcc {
	if o, n := g.port.oldP, g.port.newP; o != nil && n != nil && o.TLS == n.TLS && sameRules(g.oldS, g.newS) {
		return lines // the matcher picks the same rule with the same effect for every request
	}
	sp := g.sp
	// A path whose rules (those whose path pattern takes it) are the same
	// on both sides is handled the same for every method and header.
	same := make([]bool, len(sp.reps))
	if o, n := g.port.oldP, g.port.newP; o != nil && n != nil && o.TLS == n.TLS && g.oldS != nil && g.newS != nil &&
		routeEffect(g.oldS, nil) == routeEffect(g.newS, nil) {
		for pi, path := range sp.reps {
			same[pi] = sameList(rulesFor(g.oldS, path), rulesFor(g.newS, path))
		}
	}
	changed := map[int]map[effPair][]int{} // path rep -> effects -> tuples
	for c := 0; c < sp.ncombo; c++ {
		h := sp.header(c)
		for pi, path := range sp.reps {
			if same[pi] {
				continue
			}
			for mi, m := range sp.methods {
				eo := g.effect(true, m, path, h)
				en := g.effect(false, m, path, h)
				if eo == en {
					continue
				}
				byEff := changed[pi]
				if byEff == nil {
					byEff = map[effPair][]int{}
					changed[pi] = byEff
				}
				e := effPair{eo, en}
				byEff[e] = append(byEff[e], mi*sp.ncombo+c)
			}
		}
	}
	if len(changed) == 0 {
		return lines
	}
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
	var order []key
	sizes := sp.sizes()
	for pi := range sp.reps {
		for e, ts := range changed[pi] {
			digits := make([][]int, len(ts))
			for i, t := range ts {
				digits[i] = sp.digits(t)
			}
			for _, prod := range factor(digits, sizes) {
				k := key{e, fmt.Sprint(prod)}
				m := groups[k]
				if m == nil {
					m = &merged{e: e, prod: prod, paths: map[int]bool{}}
					groups[k] = m
					order = append(order, k)
				}
				m.paths[pi] = true
			}
		}
	}
	if g.class == nil {
		g.class = map[[2]int]int{}
	}
	for _, k := range order {
		m := groups[k]
		id := len(lines)
		var what []string
		what = append(what, sp.methodPhrase(m.prod[0]))
		for i := range sp.names {
			if s := sp.headerPhrase(i, m.prod[i+1]); s != "" {
				what = append(what, s)
			}
		}
		what = append(what, strings.Join(sp.pathPhrases(m.paths), "; "))
		sortKey := "\xff"
		for pi := range m.paths {
			if s := sp.repSort[pi]; s < sortKey {
				sortKey = s
			}
		}
		lines = append(lines, &lineAcc{id: id, port: g.port.num, group: gi, sortKey: sortKey,
			line: PlanLine{Where: g.where, What: strings.Join(what, ", "), Old: m.e.old, New: m.e.new}})
		for pi := range m.paths {
			for _, t := range sp.tuples(m.prod) {
				g.class[[2]int{pi, t}] = id
			}
		}
	}
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
func factor(ts [][]int, sizes []int) [][][]int {
	nd := len(sizes)
	proj := make([][]int, nd)
	total := 1
	for d := 0; d < nd; d++ {
		seen := map[int]bool{}
		for _, t := range ts {
			if !seen[t[d]] {
				seen[t[d]] = true
				proj[d] = append(proj[d], t[d])
			}
		}
		sort.Ints(proj[d])
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
		sort.Slice(r, func(i, j int) bool { return lessInts(r[i], r[j]) })
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
		for _, sub := range factor(p.rest, sizes[1:]) {
			out = append(out, append([][]int{p.vals}, sub...))
		}
	}
	return out
}

func lessInts(a, b []int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// space is the classes of requests a site pair can tell apart: path
// representatives, method representatives and header combinations.
type space struct {
	seg     string
	reps    []string       // representative paths
	repSort []string       // sort key per representative
	patIdx  map[string]int // pattern path -> its representative
	belowIx map[string]int // pattern path ("" for the root) -> its fresh child
	root    *pnode
	methods []string // named methods, HEAD when GET is named, then one other
	mIdx    map[string]int
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
	var routes []*Route
	for _, s := range []*Site{a, b} {
		if s != nil {
			routes = append(routes, s.Routes...)
		}
	}
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
	sp := &space{patIdx: map[string]int{}, belowIx: map[string]int{}, mIdx: map[string]int{}}
	sp.seg = "~bp"
	for n := 2; ; n++ {
		clash := false
		for p := range pats {
			if strings.Contains(p, sp.seg) {
				clash = true
				break
			}
		}
		if !clash {
			break
		}
		sp.seg = "~bp" + strconv.Itoa(n)
	}
	add := func(path, sortKey string) int {
		sp.reps = append(sp.reps, path)
		sp.repSort = append(sp.repSort, sortKey)
		return len(sp.reps) - 1
	}
	sp.root = &pnode{self: -1}
	sp.root.below = add("/"+sp.seg, "\xfe")
	sp.belowIx[""] = sp.root.below
	nodes := map[string]*pnode{"": sp.root}
	var list []string
	for p := range pats {
		list = append(list, p)
	}
	sort.Strings(list)
	for _, p := range list {
		n := &pnode{path: p, self: add(p, p), below: -1}
		sp.patIdx[p] = n.self
		if !strings.HasSuffix(p, "/") {
			n.below = add(p+"/"+sp.seg, p+"/\xfe")
			sp.belowIx[p] = n.below
		}
		nodes[p] = n
	}
	for _, p := range list {
		for i := len(p) - 1; i >= 0; i-- {
			if p[i] != '/' {
				continue
			}
			if parent := nodes[p[:i]]; parent != nil {
				parent.kids = append(parent.kids, nodes[p])
				break
			}
		}
	}
	for m := range methods {
		sp.methods = append(sp.methods, m)
	}
	if methods["GET"] && !methods["HEAD"] {
		sp.methods = append(sp.methods, "HEAD")
	}
	sort.Strings(sp.methods)
	other := "BPOTHER"
	for methods[other] {
		other += "X"
	}
	sp.methods = append(sp.methods, other)
	for i, m := range sp.methods {
		sp.mIdx[m] = i
	}
	for name := range hv {
		sp.names = append(sp.names, name)
	}
	sort.Strings(sp.names)
	sp.ncombo = 1
	for _, name := range sp.names {
		var vs []string
		for v := range hv[name] {
			vs = append(vs, v)
		}
		sort.Strings(vs)
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
	if sp.big || sp.ncombo > MaxPlanClasses {
		return MaxPlanClasses + 1
	}
	n := len(sp.reps) * len(sp.methods)
	if n > MaxPlanClasses || n*sp.ncombo > MaxPlanClasses {
		return MaxPlanClasses + 1
	}
	return n * sp.ncombo
}

func (sp *space) sizes() []int {
	return append([]int{len(sp.methods)}, sp.nstates...)
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

// tuples lists every tuple in a product.
func (sp *space) tuples(prod [][]int) []int {
	out := []int{0}
	for i, set := range prod {
		radix := sp.ncombo
		if i > 0 {
			radix = 1
			for _, n := range sp.nstates[i:] {
				radix *= n
			}
		}
		var next []int
		for _, t := range out {
			for _, v := range set {
				next = append(next, t+v*radix)
			}
		}
		out = next
	}
	return out
}

// header builds the request headers of one combination.
func (sp *space) header(c int) http.Header {
	h := http.Header{}
	for i := len(sp.names) - 1; i >= 0; i-- {
		st := c % sp.nstates[i]
		c /= sp.nstates[i]
		if st == 0 {
			continue
		}
		mask := st - 1
		if mask == 0 {
			h[sp.names[i]] = []string{sp.other[i]}
			continue
		}
		var vs []string
		for j, v := range sp.vals[i] {
			if mask&(1<<j) != 0 {
				vs = append(vs, v)
			}
		}
		h[sp.names[i]] = vs
	}
	return h
}

func (sp *space) comboOf(h http.Header) int {
	c := 0
	for i, name := range sp.names {
		st := 0
		if vals := h.Values(name); len(vals) > 0 {
			mask := 0
			for j, v := range sp.vals[i] {
				for _, x := range vals {
					if x == v {
						mask |= 1 << j
					}
				}
			}
			st = 1 + mask
		}
		c = c*sp.nstates[i] + st
	}
	return c
}

func (sp *space) methodRep(m string) int {
	if i, ok := sp.mIdx[m]; ok {
		return i
	}
	return len(sp.methods) - 1
}

// pathRep returns the representative of a path in normal form.
func (sp *space) pathRep(p string) int {
	if i, ok := sp.patIdx[p]; ok {
		return i
	}
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			if b, ok := sp.belowIx[p[:i]]; ok {
				return b
			}
		}
	}
	return sp.root.below
}

func (sp *space) methodPhrase(set []int) string {
	if len(set) == len(sp.methods) {
		return "any method"
	}
	in := map[int]bool{}
	for _, i := range set {
		in[i] = true
	}
	otherIx := len(sp.methods) - 1
	var names []string
	if in[otherIx] {
		for i, m := range sp.methods[:otherIx] {
			if !in[i] {
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
		with, without := true, true
		for st := 0; st < ns; st++ {
			has := st > 0 && (st-1)&(1<<j) != 0
			if has != in[st] {
				with = false
			}
			if has == in[st] {
				without = false
			}
		}
		if with {
			return "with " + name + ": " + v
		}
		if without {
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
			var vs []string
			for j, v := range vals {
				if (st-1)&(1<<j) != 0 {
					vs = append(vs, v)
				}
			}
			parts = append(parts, "with "+name+": "+strings.Join(vs, " and "))
		}
	}
	return strings.Join(parts, " or ")
}

func (sp *space) pathPhrases(in map[int]bool) []string {
	var out []string
	sp.describe(sp.root, in, &out)
	return out
}

func (sp *space) full(n *pnode, in map[int]bool) bool {
	if n.self >= 0 && !in[n.self] || n.below >= 0 && !in[n.below] {
		return false
	}
	for _, k := range n.kids {
		if !sp.full(k, in) {
			return false
		}
	}
	return true
}

func (sp *space) none(n *pnode, in map[int]bool) bool {
	if n.self >= 0 && in[n.self] || n.below >= 0 && in[n.below] {
		return false
	}
	for _, k := range n.kids {
		if !sp.none(k, in) {
			return false
		}
	}
	return true
}

// restFull reports whether everything below a node is in the set.
func (sp *space) restFull(n *pnode, in map[int]bool) bool {
	if n.below >= 0 && !in[n.below] {
		return false
	}
	for _, k := range n.kids {
		if !sp.full(k, in) {
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

func (sp *space) describe(n *pnode, in map[int]bool, out *[]string) {
	if sp.none(n, in) {
		return
	}
	if sp.full(n, in) {
		if n.self < 0 {
			*out = append(*out, "every path")
		} else {
			*out = append(*out, subtreeText(n))
		}
		return
	}
	selfIn := n.self >= 0 && in[n.self]
	if n.below < 0 || !in[n.below] {
		if selfIn {
			*out = append(*out, n.path)
		}
		for _, k := range n.kids {
			sp.describe(k, in, out)
		}
		return
	}
	var exc []string
	var later []*pnode
	for _, k := range n.kids {
		switch {
		case sp.full(k, in):
		case k.self >= 0 && !in[k.self] && sp.restFull(k, in):
			exc = append(exc, k.path)
		case sp.none(k, in):
			exc = append(exc, subtreeText(k))
		default:
			exc = append(exc, subtreeText(k))
			later = append(later, k)
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
	*out = append(*out, s)
	for _, k := range later {
		sp.describe(k, in, out)
	}
}

func joinAnd(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

// ruleDiff lists the rule lines that differ between a group's two sites,
// for plans with too many classes to work through.
func ruleDiff(g *hostGroup) []PlanLine {
	text := func(s *Site) []*Route {
		if s == nil {
			return nil
		}
		return s.Routes
	}
	a, b := text(g.oldS), text(g.newS)
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
			out = append(out, PlanLine{Where: g.where, What: "rule removed", Old: fmt.Sprintf("line %d: %s", a[i].Line, a[i].Text)})
			i++
		default:
			out = append(out, PlanLine{Where: g.where, What: "rule added", New: fmt.Sprintf("line %d: %s", b[j].Line, b[j].Text)})
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
		sp.each(func(m, p string, h http.Header) {
			if r, _ := s.MatchRoute(m, p, h); r != nil {
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
			sp.each(func(m, p string, h http.Header) {
				if ok, _ := r.matches(m, p, h); ok {
					if w, _ := s.MatchRoute(m, p, h); w != nil {
						takers[w.Line] = true
					}
				}
			})
			var ls []int
			for l := range takers {
				ls = append(ls, l)
			}
			sort.Ints(ls)
			msg := fmt.Sprintf("line %d: %s never matches", r.Line, r.Text)
			switch len(ls) {
			case 0:
			case 1:
				msg += fmt.Sprintf(": line %d takes every request it would get", ls[0])
			default:
				var parts []string
				for _, l := range ls {
					parts = append(parts, strconv.Itoa(l))
				}
				msg += ": lines " + joinAnd(parts) + " take every request it would get"
			}
			ws = append(ws, warn{r.Line, msg})
		}
	}
	for _, name := range c.PoolOrder {
		if ps := c.Pools[name]; !used[name] {
			ws = append(ws, warn{ps.Line, fmt.Sprintf("line %d: pool %s isn't used by any rule that can match", ps.Line, name)})
		}
	}
	sort.SliceStable(ws, func(i, j int) bool { return ws[i].line < ws[j].line })
	var out []string
	for _, w := range ws {
		out = append(out, w.msg)
	}
	return out
}

// each calls f for every class representative.
func (sp *space) each(f func(method, path string, h http.Header)) {
	for c := 0; c < sp.ncombo; c++ {
		h := sp.header(c)
		for _, p := range sp.reps {
			for _, m := range sp.methods {
				f(m, p, h)
			}
		}
	}
}

// settings lists the changes outside routing, one line each.
func (pl *planner) settings() []string {
	var out []string
	scheme := func(p *Port) string {
		if p.TLS {
			return "https"
		}
		return "http"
	}
	for _, pp := range pl.ports {
		switch {
		case pp.oldP == nil:
			out = append(out, fmt.Sprintf("port %d (%s) added (line %d)", pp.num, scheme(pp.newP), pp.newP.Line))
		case pp.newP == nil:
			out = append(out, fmt.Sprintf("port %d (%s) removed (was line %d)", pp.num, scheme(pp.oldP), pp.oldP.Line))
		case pp.oldP.TLS != pp.newP.TLS:
			out = append(out, fmt.Sprintf("port %d switched from %s to %s (line %d)", pp.num, scheme(pp.oldP), scheme(pp.newP), pp.newP.Line))
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
		if raw[0] != ' ' && raw[0] != '\t' {
			in = toks[0].s == "global"
			continue
		}
		if in {
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
	keys := map[string]bool{}
	for k := range o {
		keys[k] = true
	}
	for k := range n {
		keys[k] = true
	}
	var list []string
	for k := range keys {
		list = append(list, k)
	}
	sort.Strings(list)
	var out []string
	for _, k := range list {
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
			tls := func(s *Site) string {
				switch {
				case s.TLSAuto:
					return "auto"
				case s.CertFile != "":
					return s.CertFile + " " + s.KeyFile
				}
				return "none"
			}
			if tls(a) != tls(b) {
				out = append(out, fmt.Sprintf("%s: tls %s  ->  %s%s", name, tls(a), tls(b), lineRef(b.TLSLine, a.TLSLine)))
			}
			if a.Err404 != b.Err404 {
				out = append(out, fmt.Sprintf("%s: error 404 page %s  ->  %s%s", name, orNone(a.Err404), orNone(b.Err404), lineRef(b.Err404Line, a.Err404Line)))
			}
			if a.BodyLimit != b.BodyLimit {
				out = append(out, fmt.Sprintf("%s: body-limit %s  ->  %s%s", name, sizeText(a.BodyLimit), sizeText(b.BodyLimit),
					lineRef(settingLine(pl.new, b.Line, "body-limit"), settingLine(pl.old, a.Line, "body-limit"))))
			}
			if a.KeepEncodedSlash != b.KeepEncodedSlash {
				es := func(s *Site) string {
					if s.KeepEncodedSlash {
						return "keep"
					}
					return "reject"
				}
				out = append(out, fmt.Sprintf("%s: encoded-slashes %s  ->  %s%s", name, es(a), es(b),
					lineRef(settingLine(pl.new, b.Line, "encoded-slashes"), settingLine(pl.old, a.Line, "encoded-slashes"))))
			}
		}
	}
	return out
}

// siteLabel names a site by its first address as written, so sites with
// the same host on different ports stay apart.
func siteLabel(s *Site) string {
	if len(s.Addrs) > 0 {
		return s.Addrs[0].Text
	}
	return s.Name
}

func sizeText(n int64) string {
	switch {
	case n > 0 && n%(1<<30) == 0:
		return fmt.Sprintf("%dGB", n>>30)
	case n > 0 && n%(1<<20) == 0:
		return fmt.Sprintf("%dMB", n>>20)
	case n > 0 && n%(1<<10) == 0:
		return fmt.Sprintf("%dKB", n>>10)
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
	names := map[string]bool{}
	for n := range old.Pools {
		names[n] = true
	}
	for n := range new.Pools {
		names[n] = true
	}
	var list []string
	for n := range names {
		list = append(list, n)
	}
	sort.Strings(list)
	var out []string
	for _, name := range list {
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
		inA, inB := map[string]bool{}, map[string]bool{}
		for _, x := range a.Backends {
			inA[key(x)] = true
		}
		for _, x := range b.Backends {
			inB[key(x)] = true
		}
		for _, x := range a.Backends {
			if !inB[key(x)] {
				out = append(out, fmt.Sprintf("pool %s: backend %s removed (was line %d)", name, key(x), x.Line))
			}
		}
		for _, x := range b.Backends {
			if !inA[key(x)] {
				out = append(out, fmt.Sprintf("pool %s: backend %s added (line %d)", name, key(x), x.Line))
			}
		}
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
