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
	Settings []string   `json:"settings"`    // other changes: listeners, pools, sites, globals
	Warnings []string   `json:"warnings"`    // rules that win nothing, pools no reachable rule uses
	TooMany  bool       `json:"too_many"`    // too many classes: Changes lists changed rules instead

	ports map[int]*portPlan // kept so Classify can place a request in a listed class
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
	res := &PlanResult{ID: PlanID(old, new), Changes: []PlanLine{}, ports: map[int]*portPlan{}}
	old, new = cmp.Or(old, &Config{}), cmp.Or(new, &Config{})
	var ports []*portPlan
	for _, n := range keysOf(old.Ports, new.Ports) {
		pp := newPortPlan(n, old.Ports[n], new.Ports[n])
		ports = append(ports, pp)
		res.ports[n] = pp
		for _, g := range pp.groups {
			res.TooMany = res.TooMany || g.sp.count() > MaxPlanClasses
		}
	}
	for _, pp := range ports {
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
	res.Settings, res.Warnings = settings(old, new, ports), planWarnings(new)
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
	kind := "routing change"
	if p.TooMany {
		kind = "changed rule"
	}
	head := fmt.Sprintf("Plan %s: %s, %s\n", p.ID, plural(len(p.Changes), kind), plural(len(p.Settings), "other change"))
	if p.Empty() {
		head = "No changes.\n"
	}
	var routing []string
	if p.TooMany && len(p.Changes) > 0 {
		routing = append(routing, fmt.Sprintf("(more than %d request classes in a site, so this lists changed rules instead)", MaxPlanClasses))
	}
	for _, l := range p.Changes {
		routing = append(routing, fmt.Sprintf("%s, %s\n      %s  ->  %s", l.Where, l.What, cmp.Or(l.Old, "(none)"), cmp.Or(l.New, "(none)")))
	}
	return head + section("Routing", routing) + section("Other changes", p.Settings) + section("Warnings", p.Warnings)
}

// section writes a titled list, or nothing when the list is empty.
func section(title string, items []string) string {
	if len(items) == 0 {
		return ""
	}
	return title + "\n  " + strings.Join(items, "\n  ") + "\n"
}

// JSON renders the plan for programs.
func (p *PlanResult) JSON() ([]byte, error) { return json.MarshalIndent(p, "", "  ") }

// Classify returns the index in Changes of the line whose class holds a
// request, or -1 when the plan lists no change for it. The host has no
// port, and the path is in normal form (NormalizePath). It always returns
// -1 when TooMany is set.
func (p *PlanResult) Classify(port int, host, method, path string, h http.Header) int {
	pp := p.ports[port]
	if pp == nil || p.TooMany {
		return -1
	}
	if g := pp.byPair[[2]*Site{siteFor(pp.oldP, host), siteFor(pp.newP, host)}]; g != nil {
		if l := g.class[g.sp.index(method, path, h)]; l != nil {
			return l.index
		}
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
	switch a := r.Act; a.Kind {
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
	return r.Act.Kind
}

// portPlan is one port in either config, its hosts split into groups.
type portPlan struct {
	num        int
	oldP, newP *Port
	groups     []*hostGroup
	byPair     map[[2]*Site]*hostGroup
}

// hostGroup is the host classes on a port that go to the same old site and
// the same new site, so they are handled alike.
type hostGroup struct {
	port       *portPlan
	oldS, newS *Site
	hosts      []string // its host classes, described, in display order
	sp         *space
	class      map[int]*lineAcc // class index -> its line
}

// newPortPlan splits the hosts on a port into classes (every exact host in
// either config, every wildcard domain, and all other hosts) and groups
// the classes by the sites they go to.
func newPortPlan(num int, o, n *Port) *portPlan {
	pp := &portPlan{num: num, oldP: o, newP: n, byPair: map[[2]*Site]*hostGroup{}}
	op, np := cmp.Or(o, &Port{}), cmp.Or(n, &Port{}) // a missing port has no sites
	add := func(desc string, os, ns *Site) {
		g := pp.byPair[[2]*Site{os, ns}]
		if g == nil {
			g = &hostGroup{port: pp, oldS: os, newS: ns, sp: newSpace(os, ns)}
			pp.byPair[[2]*Site{os, ns}] = g
			pp.groups = append(pp.groups, g)
		}
		g.hosts = append(g.hosts, desc)
	}
	for _, h := range keysOf(op.Exact, np.Exact) {
		add(h, siteFor(op, h), siteFor(np, h))
	}
	for _, d := range keysOf(op.Wild, np.Wild) {
		add("*."+d, cmp.Or(op.Wild[d], op.Any), cmp.Or(np.Wild[d], np.Any))
	}
	add("any other host", op.Any, np.Any)
	return pp
}

func siteFor(p *Port, host string) *Site {
	if p == nil {
		return nil
	}
	s, _ := p.Find(host)
	return s
}

func (g *hostGroup) where() string {
	return fmt.Sprintf("%s (port %d)", strings.Join(g.hosts, ", "), g.port.num)
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

// effect is how one side handles a class, given that side's rules for the
// path, with the scheme in front when the port switches between http and
// https.
func (g *hostGroup) effect(old bool, rules []*Route, method, path string, h http.Header) string {
	p, other, s := g.port.newP, g.port.oldP, g.newS
	if old {
		p, other, s = g.port.oldP, g.port.newP, g.oldS
	}
	prefix := ""
	if p != nil && other != nil && other.TLS != p.TLS {
		prefix = schemeName(p.TLS) + ": "
	}
	switch {
	case p == nil:
		return noListener(g.port.num)
	case s == nil:
		return prefix + "421, no site"
	}
	return prefix + routeEffect(s, firstMatch(rules, method, path, h))
}

// lineAcc is one routing line before sorting.
type lineAcc struct {
	index int // in Changes, once sorted
	first int // its first path, which it sorts by
	line  PlanLine
}

type effPair struct{ old, new string }

// analyze runs every class of a host group through both configs and turns
// the classes whose effect changes into lines, sorted. Each line is one
// product of paths, methods and header states with one pair of effects.
func (g *hostGroup) analyze() []*lineAcc {
	o, n, sp := g.port.oldP, g.port.newP, g.sp
	// sameElse: same scheme, a site on both sides, and the same effect when
	// no rule matches. Then only the rules can make a difference.
	sameElse := o != nil && n != nil && o.TLS == n.TLS && g.oldS != nil && g.newS != nil &&
		routeEffect(g.oldS, nil) == routeEffect(g.newS, nil)
	if sameElse && sameRules(g.oldS.Routes, g.newS.Routes) {
		return nil // the same rule with the same effect wins every request
	}
	// A path whose rules (those whose pattern takes it) are the same on
	// both sides is handled the same for every method and header.
	ro, rn, same := make([][]*Route, len(sp.paths)), make([][]*Route, len(sp.paths)), make([]bool, len(sp.paths))
	for pi, path := range sp.paths {
		ro[pi], rn[pi] = rulesFor(g.oldS, path), rulesFor(g.newS, path)
		same[pi] = sameElse && sameRules(ro[pi], rn[pi])
	}
	changed := map[effPair][][]int{}
	sp.each(same, func(i, pi int, m string, h http.Header) {
		if e := (effPair{g.effect(true, ro[pi], m, sp.paths[pi], h), g.effect(false, rn[pi], m, sp.paths[pi], h)}); e.old != e.new {
			changed[e] = append(changed[e], sp.digits(i))
		}
	})
	g.class = map[int]*lineAcc{}
	var lines []*lineAcc
	for e, ds := range changed {
		for _, prod := range factor(ds) {
			what := []string{sp.methodPhrase(prod[1])}
			for k := range sp.names {
				if s := sp.headerPhrase(k, prod[k+2]); s != "" {
					what = append(what, s)
				}
			}
			in := map[int]bool{}
			for _, pi := range prod[0] {
				in[pi] = true
			}
			what = append(what, strings.Join(describe(sp.nodes[""], in), "; "))
			l := &lineAcc{first: prod[0][0], line: PlanLine{g.where(), strings.Join(what, ", "), e.old, e.new}}
			lines = append(lines, l)
			for _, i := range sp.indexes(prod) {
				g.class[i] = l
			}
		}
	}
	slices.SortFunc(lines, func(a, b *lineAcc) int {
		return cmp.Or(a.first-b.first, strings.Compare(a.line.What, b.line.What),
			strings.Compare(a.line.Old, b.line.Old), strings.Compare(a.line.New, b.line.New))
	})
	return lines
}

// sameRules reports whether two lists of rules have the same matchers and
// effects in the same order.
func sameRules(a, b []*Route) bool {
	return slices.EqualFunc(a, b, func(x, y *Route) bool {
		return x.Path == y.Path && x.Prefix == y.Prefix && slices.Equal(x.Methods, y.Methods) &&
			slices.Equal(x.Headers, y.Headers) && routeEffect(nil, x) == routeEffect(nil, y)
	})
}

// rulesFor lists a site's rules whose path pattern takes a path (the path
// part of Route.matches), in order. Only they can match the path.
func rulesFor(s *Site, path string) []*Route {
	var out []*Route
	for _, r := range cmp.Or(s, &Site{}).Routes {
		if r.Prefix && (r.Path == "" || path == r.Path || strings.HasPrefix(path, r.Path+"/")) || !r.Prefix && path == r.Path {
			out = append(out, r)
		}
	}
	return out
}

// firstMatch is the rule the matcher picks from a list, or nil.
func firstMatch(rules []*Route, method, path string, h http.Header) *Route {
	for _, r := range rules {
		if ok, _ := r.matches(method, path, h); ok {
			return r
		}
	}
	return nil
}

// factor splits a set of distinct tuples into a few cartesian products,
// each one sorted set of values per place.
func factor(ts [][]int) [][][]int {
	proj, total := make([][]int, len(ts[0])), 1
	for d := range proj {
		for _, t := range ts {
			proj[d] = append(proj[d], t[d])
		}
		slices.Sort(proj[d])
		proj[d] = slices.Compact(proj[d])
		if total <= len(ts) { // past that it can't be a product anyway
			total *= len(proj[d])
		}
	}
	if total == len(ts) {
		return [][][]int{proj}
	}
	// Values in the first place whose other places hold the same tuples go
	// together; the rest of each such part is factored in turn.
	rest := map[int][][]int{}
	for _, t := range ts {
		rest[t[0]] = append(rest[t[0]], t[1:])
	}
	var keys []string
	parts := map[string][]int{}
	for _, v := range proj[0] {
		slices.SortFunc(rest[v], slices.Compare[[]int])
		k := fmt.Sprint(rest[v])
		if parts[k] == nil {
			keys = append(keys, k)
		}
		parts[k] = append(parts[k], v)
	}
	var out [][][]int
	for _, k := range keys {
		for _, sub := range factor(rest[parts[k][0]]) {
			out = append(out, append([][]int{parts[k]}, sub...))
		}
	}
	return out
}

// space is the classes of requests a site pair can tell apart. A class is
// a representative path, a method and a state for each header name that
// conditions use; its index counts them in the mixed radix of radix.
type space struct {
	paths   []string          // representative paths
	nodes   map[string]*pnode // pattern ("" for the root) -> its node
	methods []string          // named methods, HEAD when GET is named, then one other
	names   []string          // header names in conditions, canonical
	vals    [][]string        // named values per header name
	// radix holds the number of paths, of methods, and per header name of
	// states: absent, then present with each subset of its named values.
	radix []int
}

// pnode is a path pattern in the tree of patterns. The root has path "".
type pnode struct {
	path  string
	self  int // representative of the path itself; -1 for the root
	below int // representative of a fresh path below it; -1 when it ends in /
	kids  []*pnode
}

func newSpace(a, b *Site) *space {
	pats, methods, hv := map[string]bool{"": true}, map[string]bool{}, map[string]map[string]bool{}
	for _, r := range slices.Concat(cmp.Or(a, &Site{}).Routes, cmp.Or(b, &Site{}).Routes) {
		pats[r.Path] = true
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
	list := keysOf(pats) // parents come before their children
	seg := "~bp"         // a path segment no pattern holds, for fresh paths
	for n := 2; slices.ContainsFunc(list, func(p string) bool { return strings.Contains(p, seg) }); n++ {
		seg = "~bp" + strconv.Itoa(n)
	}
	sp := &space{nodes: map[string]*pnode{}}
	add := func(path string) int {
		sp.paths = append(sp.paths, path)
		return len(sp.paths) - 1
	}
	for _, p := range list[1:] {
		n := &pnode{path: p, self: add(p), below: -1}
		if !strings.HasSuffix(p, "/") {
			n.below = add(p + "/" + seg)
		}
		sp.nodes[p] = n
	}
	// The root's fresh path comes last, so lines for other paths sort last.
	sp.nodes[""] = &pnode{self: -1, below: add("/" + seg)}
	// A pattern's parent is the longest pattern that ends where one of its
	// slashes starts (the root, "", at worst).
	for _, p := range list[1:] {
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
	sp.radix = []int{len(sp.paths), len(sp.methods)}
	for _, name := range sp.names {
		sp.vals = append(sp.vals, keysOf(hv[name]))
		// Past 30 values the true count is far above any class limit.
		sp.radix = append(sp.radix, 1+1<<min(len(hv[name]), 30))
	}
	return sp
}

// count is the number of classes, or MaxPlanClasses+1 when there are more.
func (sp *space) count() int {
	n := 1
	for _, r := range sp.radix {
		if n *= r; n > MaxPlanClasses {
			return MaxPlanClasses + 1
		}
	}
	return n
}

// digits splits a class index into its path, method and header states.
func (sp *space) digits(i int) []int {
	d := make([]int, len(sp.radix))
	for k := len(d) - 1; k >= 0; k-- {
		d[k], i = i%sp.radix[k], i/sp.radix[k]
	}
	return d
}

// indexes lists the class index of every class in a product (the reverse
// of digits).
func (sp *space) indexes(prod [][]int) []int {
	out := []int{0}
	for k, set := range prod {
		var next []int
		for _, i := range out {
			for _, v := range set {
				next = append(next, i*sp.radix[k]+v)
			}
		}
		out = next
	}
	return out
}

// each calls f for every class whose path skip doesn't mark, with the
// class index, the path's index, the method and the headers.
func (sp *space) each(skip []bool, f func(i, pi int, method string, h http.Header)) {
	nm, nc := len(sp.methods), sp.count()/len(sp.paths)/len(sp.methods)
	hs := make([]http.Header, nc)
	for c := range hs {
		hs[c] = http.Header{}
		for k, st := range sp.digits(c)[2:] {
			// A value no condition names (longer than any of them) stands
			// for every such value.
			if st > 0 {
				hs[c][sp.names[k]] = append(sp.valsIn(k, st-1), strings.Join(sp.vals[k], "")+"~")
			}
		}
	}
	for pi := range sp.paths {
		if skip != nil && skip[pi] {
			continue
		}
		for mi, m := range sp.methods {
			for c, h := range hs {
				f((pi*nm+mi)*nc+c, pi, m, h)
			}
		}
	}
}

// index returns the class of a request whose path is in normal form.
func (sp *space) index(method, path string, h http.Header) int {
	m := slices.Index(sp.methods, method)
	if m < 0 {
		m = len(sp.methods) - 1
	}
	i := sp.pathRep(path)*len(sp.methods) + m
	for k, name := range sp.names {
		st := 0
		if vals := h.Values(name); len(vals) > 0 {
			st = 1
			for j, v := range sp.vals[k] {
				if slices.Contains(vals, v) {
					st += 1 << j
				}
			}
		}
		i = i*sp.radix[k+2] + st
	}
	return i
}

// pathRep returns the representative of a path in normal form: the
// pattern itself, or the fresh path below the nearest pattern above it.
func (sp *space) pathRep(p string) int {
	if n := sp.nodes[p]; n != nil && n.self >= 0 {
		return n.self
	}
	for i := strings.LastIndexByte(p, '/'); i >= 0; i = strings.LastIndexByte(p[:i], '/') {
		if n := sp.nodes[p[:i]]; n != nil && n.below >= 0 {
			return n.below
		}
	}
	return sp.nodes[""].below
}

// valsIn lists the named values of header k whose bits are set in mask.
func (sp *space) valsIn(k, mask int) []string {
	var vs []string
	for j, v := range sp.vals[k] {
		if mask&(1<<j) != 0 {
			vs = append(vs, v)
		}
	}
	return vs
}

func (sp *space) methodPhrase(set []int) string {
	if len(set) == len(sp.methods) {
		return "any method"
	}
	// With the other methods in the set, name the named ones it leaves out.
	other := slices.Contains(set, len(sp.methods)-1)
	var names []string
	for i, m := range sp.methods[:len(sp.methods)-1] {
		if slices.Contains(set, i) != other {
			names = append(names, m)
		}
	}
	if other {
		return "any method except " + joinAnd(names)
	}
	return joinAnd(names)
}

// headerPhrase describes a set of states of header k, or returns "" when
// the set holds every state.
func (sp *space) headerPhrase(k int, set []int) string {
	name, ns := sp.names[k], sp.radix[k+2]
	if len(set) == ns {
		return ""
	}
	// One condition may hold in exactly the set's states, or in none of
	// them: having the header at all (j = -1), or having one named value.
	for j := -1; j < len(sp.vals[k]); j++ {
		agree, cond := 0, name
		if j >= 0 {
			cond += ": " + sp.vals[k][j]
		}
		for st := 0; st < ns; st++ {
			if holds := st > 0 && (j < 0 || (st-1)&(1<<j) != 0); holds == slices.Contains(set, st) {
				agree++
			}
		}
		switch agree {
		case ns:
			return "with " + cond
		case 0:
			return "without " + cond
		}
	}
	var parts []string
	for _, st := range set {
		switch {
		case st == 0:
			parts = append(parts, "without "+name)
		case st == 1:
			parts = append(parts, "with "+name+" other than "+strings.Join(sp.vals[k], ", "))
		default:
			parts = append(parts, "with "+name+": "+strings.Join(sp.valsIn(k, st-1), " and "))
		}
	}
	return strings.Join(parts, " or ")
}

// whole reports whether every representative at and below n (leaving out
// n's own path unless self is set) is in the set (want true) or out of it
// (want false).
func whole(n *pnode, in map[int]bool, want, self bool) bool {
	if self && n.self >= 0 && in[n.self] != want || n.below >= 0 && in[n.below] != want {
		return false
	}
	for _, k := range n.kids {
		if !whole(k, in, want, true) {
			return false
		}
	}
	return true
}

func subtreeText(n *pnode) string {
	switch {
	case n.self < 0:
		return "every path"
	case n.below < 0 && len(n.kids) == 0:
		return n.path
	}
	return n.path + " and below"
}

// describe phrases the paths at and below a node whose representatives
// are in the set.
func describe(n *pnode, in map[int]bool) []string {
	switch {
	case whole(n, in, false, true):
		return nil
	case whole(n, in, true, true):
		return []string{subtreeText(n)}
	}
	selfIn, belowIn := n.self >= 0 && in[n.self], n.below >= 0 && in[n.below]
	if !belowIn {
		var out []string
		if selfIn {
			out = append(out, n.path)
		}
		for _, k := range n.kids {
			out = append(out, describe(k, in)...)
		}
		return out
	}
	// The fresh paths below n are in: name the patterns below it that are
	// not wholly in, and describe what of them is in on its own.
	var not, later []string
	for _, k := range n.kids {
		switch {
		case whole(k, in, true, true):
		case !in[k.self] && whole(k, in, true, false):
			not = append(not, k.path)
		default:
			not = append(not, subtreeText(k))
			later = append(later, describe(k, in)...)
		}
	}
	s := "below " + n.path
	switch {
	case n.self < 0 && len(not) > 0:
		s = "every other path"
	case n.self < 0:
		s = "every path"
	case selfIn:
		s = n.path + " and below"
	}
	if len(not) > 0 {
		s += " (not " + strings.Join(not, ", ") + ")"
	}
	return append([]string{s}, later...)
}

func joinAnd(xs []string) string {
	if len(xs) < 2 {
		return strings.Join(xs, "")
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

// ruleDiff lists the rules that differ between a group's two sites, for
// plans with too many classes to work through. Rules on the longest
// common subsequence of the two lists count as unchanged.
func ruleDiff(g *hostGroup) []PlanLine {
	a, b := cmp.Or(g.oldS, &Site{}).Routes, cmp.Or(g.newS, &Site{}).Routes
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i].Text == b[j].Text {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []PlanLine
	for i, j := 0, 0; i < len(a) || j < len(b); {
		switch {
		case i < len(a) && j < len(b) && a[i].Text == b[j].Text:
			i, j = i+1, j+1
		case j == len(b) || i < len(a) && lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, PlanLine{g.where(), "rule removed", fmt.Sprintf("line %d: %s", a[i].Line, a[i].Text), ""})
			i++
		default:
			out = append(out, PlanLine{g.where(), "rule added", "", fmt.Sprintf("line %d: %s", b[j].Line, b[j].Text)})
			j++
		}
	}
	return out
}

// planWarnings checks a config for rules that win no class and pools that
// no rule able to match uses.
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
		rules := make([][]*Route, len(sp.paths))
		for pi, p := range sp.paths {
			rules[pi] = rulesFor(s, p)
		}
		// The first rule to match a class wins it; the winner takes the
		// class from every later rule that matches it too.
		won, takers := map[*Route]bool{}, map[*Route]map[int]bool{}
		sp.each(nil, func(_, pi int, m string, h http.Header) {
			var w *Route
			for _, r := range rules[pi] {
				switch ok, _ := r.matches(m, sp.paths[pi], h); {
				case ok && w == nil:
					w, won[r] = r, true
				case ok:
					if takers[r] == nil {
						takers[r] = map[int]bool{}
					}
					takers[r][w.Line] = true
				}
			}
		})
		for _, r := range s.Routes {
			if won[r] {
				used[r.Act.Pool] = true
				continue
			}
			var ls []string
			for _, l := range keysOf(takers[r]) {
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
	slices.SortStableFunc(ws, func(a, b warn) int { return a.line - b.line })
	out := []string{}
	for _, w := range ws {
		out = append(out, w.msg)
	}
	return out
}

// settings lists the changes outside routing, one line each: listeners,
// global settings, settings of sites that serve the same hosts, and pools.
func settings(old, new *Config, ports []*portPlan) []string {
	out := []string{}
	for _, pp := range ports {
		o, n := pp.oldP, pp.newP
		switch {
		case o == nil:
			out = append(out, fmt.Sprintf("port %d (%s) added (line %d)", pp.num, schemeName(n.TLS), n.Line))
		case n == nil:
			out = append(out, fmt.Sprintf("port %d (%s) removed (was line %d)", pp.num, schemeName(o.TLS), o.Line))
		case o.TLS != n.TLS:
			out = append(out, fmt.Sprintf("port %d switched from %s to %s (line %d)", pp.num, schemeName(o.TLS), schemeName(n.TLS), n.Line))
		}
	}
	out = diffSettings(out, "global ", blockSettings(old, 0), blockSettings(new, 0))
	seen := map[[2]*Site]bool{}
	for _, pp := range ports {
		for _, g := range pp.groups {
			a, b := g.oldS, g.newS
			if a == nil || b == nil || a.Synthetic || b.Synthetic || seen[[2]*Site{a, b}] {
				continue
			}
			seen[[2]*Site{a, b}] = true
			name := "site " + siteLabel(b)
			if siteLabel(a) != siteLabel(b) {
				name += " (was site " + siteLabel(a) + ")"
			}
			out = diffSettings(out, name+": ", blockSettings(old, a.Line), blockSettings(new, b.Line))
		}
	}
	for _, name := range keysOf(old.Pools, new.Pools) {
		switch a, b := old.Pools[name], new.Pools[name]; {
		case a == nil:
			out = append(out, fmt.Sprintf("pool %s added (line %d), %s", name, b.Line, plural(len(b.Backends), "backend")))
		case b == nil:
			out = append(out, fmt.Sprintf("pool %s removed (was line %d)", name, a.Line))
		default:
			out = diffSettings(out, "pool "+name+": ", blockSettings(old, a.Line), blockSettings(new, b.Line))
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

type cfgLine struct {
	text string
	line int
}

// blockSettings returns the setting lines of the block that starts on line
// start, or of every global block when start is 0. Backends are keyed by
// their whole text, other settings by name; rules are left out.
func blockSettings(c *Config, start int) map[string]cfgLine {
	out := map[string]cfgLine{}
	in := false
	for i, raw := range c.Lines {
		toks, err := splitLine(raw)
		if err != nil || len(toks) == 0 {
			continue
		}
		key, text := toks[0].s, joinTokens(toks)
		switch {
		case raw[0] != ' ' && raw[0] != '\t':
			in = i+1 == start || start == 0 && key == "global"
		case in && key == "backend":
			out[text] = cfgLine{text, i + 1}
		case in && key != "route":
			out[key] = cfgLine{text, i + 1}
		}
	}
	return out
}

// diffSettings adds a line for each setting that is new, gone or changed.
func diffSettings(out []string, label string, o, n map[string]cfgLine) []string {
	for _, k := range keysOf(o, n) {
		a, inOld := o[k]
		b, inNew := n[k]
		switch {
		case !inOld:
			out = append(out, fmt.Sprintf("%s%s added (line %d)", label, b.text, b.line))
		case !inNew:
			out = append(out, fmt.Sprintf("%s%s removed (was line %d)", label, a.text, a.line))
		case a.text != b.text:
			out = append(out, fmt.Sprintf("%s%s  ->  %s (line %d)", label, a.text, strings.TrimPrefix(b.text, k+" "), b.line))
		}
	}
	return out
}
