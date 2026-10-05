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
	Settings []string   `json:"settings"`    // other changes: listeners, globals, sites, pools
	Warnings []string   `json:"warnings"`    // rules that win nothing, pools no reachable rule uses
	TooMany  bool       `json:"too_many"`    // too many classes: Changes lists changed rules instead

	// Kept so Classify can place a request in a listed class.
	old, new *Config
	groups   map[int][]*hostGroup // by port
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
	res := &PlanResult{ID: PlanID(old, new), Changes: []PlanLine{}, old: cmp.Or(old, &Config{}), new: cmp.Or(new, &Config{}),
		groups: map[int][]*hostGroup{}}
	var all []*hostGroup
	for _, n := range keysOf(res.old.Ports, res.new.Ports) {
		res.groups[n] = hostGroups(n, res.old.Ports[n], res.new.Ports[n])
		all = append(all, res.groups[n]...)
	}
	res.TooMany = slices.ContainsFunc(all, func(g *hostGroup) bool { return g.sp.size > MaxPlanClasses })
	for _, g := range all {
		if res.TooMany {
			res.Changes = append(res.Changes, ruleDiff(g)...)
			continue
		}
		for _, l := range g.analyze() {
			l.index = len(res.Changes)
			res.Changes = append(res.Changes, l.line)
		}
	}
	res.Settings, res.Warnings = settings(res.old, res.new), planWarnings(res.new)
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
	kind := map[bool]string{false: "routing change", true: "changed rule"}[p.TooMany]
	// The heading counts what the plan holds and leaves out what it has none of.
	var counts []string
	for _, c := range []string{plural(len(p.Changes), kind), plural(len(p.Settings), "other change"), plural(len(p.Warnings), "warning")} {
		if !strings.HasPrefix(c, "0 ") {
			counts = append(counts, c)
		}
	}
	head := "Plan " + p.ID + ": " + strings.Join(counts, ", ") + "\n"
	if p.Empty() {
		head = "No changes.\n"
	}
	var routing []string
	if p.TooMany && len(p.Changes) > 0 {
		routing = append(routing, "(a site has too many request classes to list, so this lists changed rules instead)")
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
	if p.old == nil || p.TooMany {
		return -1
	}
	os, ns := siteFor(p.old.Ports[port], host), siteFor(p.new.Ports[port], host)
	for _, g := range p.groups[port] {
		if g.oldS != os || g.newS != ns {
			continue
		}
		d := g.sp.classOf(method, path, h)
		for _, l := range g.lines {
			k := 0
			for k < len(d) && slices.Contains(l.prod[k], d[k]) {
				k++
			}
			if k == len(d) {
				return l.index
			}
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
	s := siteFor(c.Ports[port], host)
	switch {
	case c.Ports[port] == nil:
		return noListener(port)
	case s == nil:
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
		// A URL with no path keeps the request's path and query.
		if redirectTarget(a.URL, "/", "") != a.URL {
			return fmt.Sprintf("redirect %d to %s, keeping path and query", a.Code, a.URL)
		}
		return fmt.Sprintf("redirect %d to %s", a.Code, a.URL)
	case "respond":
		// An empty body is left out.
		return strings.TrimSuffix(fmt.Sprintf("respond %d %q", a.Status, a.Body), ` ""`)
	case "https":
		return "plain HTTP redirected to https://"
	}
	return r.Act.Kind
}

// effect is how one side handles a request: p is its port, s its site and
// rules its rules for the path. The scheme comes first when the port
// switches between http and https (other is the other side's port).
func effect(num int, p, other *Port, s *Site, rules []*Route, method, path string, h http.Header) string {
	prefix := ""
	if p != nil && other != nil && other.TLS != p.TLS {
		prefix = schemeName(p.TLS) + ": "
	}
	switch {
	case p == nil:
		return noListener(num)
	case s == nil:
		return prefix + "421, no site"
	}
	r, _ := (&Site{Routes: rules}).MatchRoute(method, path, h)
	return prefix + routeEffect(s, r)
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

// sameRules reports whether two lists of rules have the same matchers and
// effects in the same order.
func sameRules(a, b []*Route) bool {
	return slices.EqualFunc(a, b, func(x, y *Route) bool {
		return x.Path == y.Path && x.Prefix == y.Prefix && slices.Equal(x.Methods, y.Methods) &&
			slices.Equal(x.Headers, y.Headers) && routeEffect(nil, x) == routeEffect(nil, y)
	})
}

// hostGroup is the host classes on a port that go to the same old site and
// the same new site, so they are handled alike, and its routing lines.
type hostGroup struct {
	port       int
	oldP, newP *Port
	oldS, newS *Site
	hosts      []string // its host classes, described, in display order
	sp         *space
	lines      []*lineAcc
}

// lineAcc is one routing line and the product of classes it holds.
type lineAcc struct {
	index int     // in Changes
	prod  [][]int // per place of a class (see classOf), the values it holds
	line  PlanLine
}

// hostGroups splits the hosts on a port into classes (every exact host in
// either config, every wildcard domain, all other hosts) and groups the
// classes by the sites they go to.
func hostGroups(num int, o, n *Port) []*hostGroup {
	var gs []*hostGroup
	op, np := cmp.Or(o, &Port{}), cmp.Or(n, &Port{}) // a missing port has no sites
	add := func(desc string, os, ns *Site) {
		i := slices.IndexFunc(gs, func(g *hostGroup) bool { return g.oldS == os && g.newS == ns })
		if i < 0 {
			i, gs = len(gs), append(gs, &hostGroup{port: num, oldP: o, newP: n, oldS: os, newS: ns, sp: newSpace(os, ns)})
		}
		gs[i].hosts = append(gs[i].hosts, desc)
	}
	for _, h := range keysOf(op.Exact, np.Exact) {
		add(h, siteFor(op, h), siteFor(np, h))
	}
	for _, d := range keysOf(op.Wild, np.Wild) {
		add("*."+d, cmp.Or(op.Wild[d], op.Any), cmp.Or(np.Wild[d], np.Any))
	}
	add("any other host", op.Any, np.Any)
	return gs
}

func siteFor(p *Port, host string) *Site {
	if p == nil {
		return nil
	}
	s, _ := p.Find(host)
	return s
}

func (g *hostGroup) where() string {
	return fmt.Sprintf("%s (port %d)", strings.Join(g.hosts, ", "), g.port)
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

type effPair struct{ old, new string }

// analyze runs every class of a host group through both configs and turns
// the classes whose effect changes into lines, sorted. Each line is one
// product of paths, methods and header states with one pair of effects.
func (g *hostGroup) analyze() []*lineAcc {
	sp := g.sp
	// sameElse: same scheme, a site on both sides, and the same effect when
	// no rule matches. Then only the rules can make a difference.
	sameElse := g.oldP != nil && g.newP != nil && g.oldP.TLS == g.newP.TLS && g.oldS != nil && g.newS != nil &&
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
	sp.each(same, func(pi int, m string, h http.Header) {
		path := sp.paths[pi]
		e := effPair{effect(g.port, g.oldP, g.newP, g.oldS, ro[pi], m, path, h), effect(g.port, g.newP, g.oldP, g.newS, rn[pi], m, path, h)}
		if e.old != e.new {
			changed[e] = append(changed[e], sp.classOf(m, path, h))
		}
	})
	for e, classes := range changed {
		for _, prod := range factor(classes) {
			what := []string{sp.methodPhrase(prod[1])}
			for k := range sp.names {
				what = append(what, sp.headerPhrase(k, prod[k+2]))
			}
			in := func(pi int) bool { _, ok := slices.BinarySearch(prod[0], pi); return ok }
			what = append(what, strings.Join(describe(sp.nodes[""], in), "; "))
			what = slices.DeleteFunc(what, func(s string) bool { return s == "" }) // headers in every state
			g.lines = append(g.lines, &lineAcc{prod: prod, line: PlanLine{g.where(), strings.Join(what, ", "), e.old, e.new}})
		}
	}
	// Lines go in the order of their products, which never overlap: by
	// their first paths (patterns sorted, then all other paths), and so on.
	slices.SortFunc(g.lines, func(a, b *lineAcc) int { return slices.CompareFunc(a.prod, b.prod, slices.Compare) })
	return g.lines
}

// factor splits a set of distinct tuples into a few cartesian products,
// each one sorted set of values per place: the values in the first place
// whose other places hold the same tuples go together, and those tuples
// are factored in turn.
func factor(ts [][]int) [][][]int {
	rest := map[int][][]int{}
	for _, t := range ts {
		rest[t[0]] = append(rest[t[0]], t[1:])
	}
	parts := map[string][]int{} // the order of the products doesn't matter
	for _, v := range keysOf(rest) {
		slices.SortFunc(rest[v], slices.Compare)
		k := fmt.Sprint(rest[v])
		parts[k] = append(parts[k], v)
	}
	var out [][][]int
	for _, vals := range parts {
		if len(ts[0]) == 1 {
			out = append(out, [][]int{vals})
			continue
		}
		for _, sub := range factor(rest[vals[0]]) {
			out = append(out, append([][]int{vals}, sub...))
		}
	}
	return out
}

// space is the classes of requests a site pair can tell apart. A class is
// a representative path, a method and a state for each header name that
// conditions use: absent, or present with some subset of the values they
// name.
type space struct {
	paths   []string          // representative paths
	nodes   map[string]*pnode // pattern ("" for the root) -> its node
	methods []string          // named methods, HEAD when GET is named, then "" for all others
	names   []string          // header names in conditions, canonical
	vals    [][]string        // named values per header name
	size    int               // the number of classes, or more than MaxPlanClasses
}

// pnode is a path pattern in the tree of patterns. The root has path "".
type pnode struct {
	path  string
	self  int // representative of the path itself; -1 for the root
	below int // representative of fresh paths below it; -1 when it ends in /
	kids  []*pnode
}

func newSpace(a, b *Site) *space {
	pats, methods, hv := map[string]bool{"": true}, map[string]bool{}, map[string][]string{}
	for _, r := range slices.Concat(cmp.Or(a, &Site{}).Routes, cmp.Or(b, &Site{}).Routes) {
		pats[r.Path] = true
		for _, m := range r.Methods {
			methods[m] = true
		}
		for _, c := range r.Headers {
			vs := hv[c.Name]
			if c.HasValue {
				vs = append(vs, c.Value)
			}
			hv[c.Name] = vs
		}
	}
	// A fresh path below a pattern ends in a segment with a space, which no
	// pattern holds (they are in normal form).
	sp := &space{nodes: map[string]*pnode{}}
	add := func(path string) int {
		sp.paths = append(sp.paths, path)
		return len(sp.paths) - 1
	}
	for _, p := range keysOf(pats) { // the root first, and parents before their children
		n := &pnode{path: p, self: -1, below: -1}
		if p != "" {
			n.self = add(p)
			// Its parent is the longest pattern that ends where one of its
			// slashes starts.
			for q := p[:strings.LastIndexByte(p, '/')]; ; q = q[:strings.LastIndexByte(q, '/')] {
				if parent := sp.nodes[q]; parent != nil {
					parent.kids = append(parent.kids, n)
					break
				}
			}
			if !strings.HasSuffix(p, "/") {
				n.below = add(p + "/ bp")
			}
		}
		sp.nodes[p] = n
	}
	sp.nodes[""].below = add("/ bp") // last, so lines for other paths sort last
	if methods["GET"] {
		methods["HEAD"] = true
	}
	sp.methods = append(keysOf(methods), "")
	sp.names = keysOf(hv)
	sp.size = len(sp.paths) * len(sp.methods)
	for _, name := range sp.names {
		vs := slices.Compact(slices.Sorted(slices.Values(hv[name])))
		sp.vals = append(sp.vals, vs)
		if sp.size <= MaxPlanClasses {
			sp.size *= 1 + 1<<min(len(vs), 30) // past 30 values it is far too big anyway
		}
	}
	return sp
}

// each calls f with a request of every class, leaving out the paths skip
// marks. A value no condition names (longer than all of them) stands for
// every such value.
func (sp *space) each(skip []bool, f func(pi int, method string, h http.Header)) {
	hs := []http.Header{{}}
	for k, name := range sp.names {
		var next []http.Header
		for _, h := range hs {
			next = append(next, h)
			for mask := 0; mask < 1<<len(sp.vals[k]); mask++ {
				h2 := maps.Clone(h)
				h2[name] = append(sp.valsIn(k, mask), strings.Join(sp.vals[k], "")+"~")
				next = append(next, h2)
			}
		}
		hs = next
	}
	for pi := range sp.paths {
		for _, m := range sp.methods {
			for _, h := range hs {
				if skip == nil || !skip[pi] {
					f(pi, m, h)
				}
			}
		}
	}
}

// classOf returns the class of a request: its path's representative (the
// path in normal form), its method's place in methods, and the state of
// each header name (0 when absent, else 1 plus the mask of named values).
func (sp *space) classOf(method, path string, h http.Header) []int {
	m := slices.Index(sp.methods, method)
	if m < 0 {
		m = len(sp.methods) - 1
	}
	d := []int{sp.pathRep(path), m}
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
		d = append(d, st)
	}
	return d
}

// pathRep returns the representative of a path: the pattern itself, or
// the fresh path below the nearest pattern above it.
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
	name, ns := "header "+sp.names[k], 1+1<<len(sp.vals[k])
	if len(set) == ns {
		return ""
	}
	// One condition may hold in exactly the set's states, or in none of
	// them: having the header at all (j = -1), or having one named value.
	for j := -1; j < len(sp.vals[k]); j++ {
		agree, cond, anyValue := 0, name, " (any value)"
		if j >= 0 {
			cond, anyValue = name+": "+sp.vals[k][j], ""
		}
		for st := 0; st < ns; st++ {
			if holds := st > 0 && (j < 0 || (st-1)&(1<<j) != 0); holds == slices.Contains(set, st) {
				agree++
			}
		}
		switch agree {
		case ns:
			return "with " + cond + anyValue
		case 0:
			return "without " + cond
		}
	}
	var parts []string
	for _, st := range set {
		if st == 0 {
			parts = append(parts, "without "+name)
		} else {
			parts = append(parts, "with "+name+": "+cmp.Or(strings.Join(sp.valsIn(k, st-1), " and "), "none of "+strings.Join(sp.vals[k], ", ")))
		}
	}
	return strings.Join(parts, " or ")
}

// whole reports whether every representative at and below n (leaving out
// n's own path unless self is set) is in the set (want true) or out of it
// (want false).
func whole(n *pnode, in func(int) bool, want, self bool) bool {
	if self && n.self >= 0 && in(n.self) != want || n.below >= 0 && in(n.below) != want {
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
func describe(n *pnode, in func(int) bool) []string {
	switch {
	case whole(n, in, false, true):
		return nil
	case whole(n, in, true, true):
		return []string{subtreeText(n)}
	}
	selfIn, belowIn := n.self >= 0 && in(n.self), n.below >= 0 && in(n.below)
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
		case !in(k.self) && whole(k, in, true, false):
			not = append(not, k.path)
		default:
			not = append(not, subtreeText(k))
			later = append(later, describe(k, in)...)
		}
	}
	s := "below " + n.path
	switch {
	case n.self < 0:
		s = "every path"
	case selfIn:
		s = n.path + " and below"
	}
	if len(not) > 0 { // single paths first, so "and below" ends the list
		slices.SortStableFunc(not, func(a, b string) int { return strings.Count(a, " and below") - strings.Count(b, " and below") })
		s += " except " + joinAnd(not)
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
// common subsequence of the two lists count as unchanged; a rule removed
// where another is added counts as one changed rule.
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
		case i < len(a) && j < len(b) && lcs[i][j] == lcs[i+1][j+1]: // both off the subsequence: one rule changed
			out = append(out, PlanLine{g.where(), "rule changed", fmt.Sprintf("line %d: %s", a[i].Line, a[i].Text), fmt.Sprintf("line %d: %s", b[j].Line, b[j].Text)})
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
	ws := map[int]string{} // by line: one rule, site or pool per line
	used := map[string]bool{}
	for _, s := range c.Sites {
		if s.Synthetic {
			continue
		}
		sp := newSpace(s, nil)
		if sp.size > MaxPlanClasses {
			ws[s.Line] = fmt.Sprintf("line %d: site %s has too many request classes to check for rules that never match", s.Line, s.Name)
			for _, r := range s.Routes {
				used[r.Act.Pool] = true
			}
			continue
		}
		rules := make([][]*Route, len(sp.paths))
		for pi, p := range sp.paths {
			rules[pi] = rulesFor(s, p)
		}
		// For each rule, the lines of the rules that win the classes it
		// matches: its own line when it wins one.
		wins := map[*Route]map[int]bool{}
		sp.each(nil, func(pi int, m string, h http.Header) {
			var w *Route
			for _, r := range rules[pi] {
				if ok, _ := r.matches(m, sp.paths[pi], h); ok {
					w = cmp.Or(w, r)
					if wins[r] == nil {
						wins[r] = map[int]bool{}
					}
					wins[r][w.Line] = true
				}
			}
		})
		for _, r := range s.Routes {
			if wins[r][r.Line] {
				used[r.Act.Pool] = true
				continue
			}
			var ls []string
			for _, l := range keysOf(wins[r]) {
				ls = append(ls, strconv.Itoa(l))
			}
			ws[r.Line] = fmt.Sprintf("line %d: %s never matches: line %s takes every request it would get", r.Line, r.Text, joinAnd(ls))
			if len(ls) > 1 {
				ws[r.Line] = fmt.Sprintf("line %d: %s never matches: lines %s take every request it would get", r.Line, r.Text, joinAnd(ls))
			}
		}
	}
	for _, name := range c.PoolOrder {
		if ps := c.Pools[name]; !used[name] {
			ws[ps.Line] = fmt.Sprintf("line %d: pool %s isn't used by any rule that can match", ps.Line, name)
		}
	}
	out := []string{}
	for _, l := range keysOf(ws) {
		out = append(out, ws[l])
	}
	return out
}

// settings lists the changes outside routing, one line each: listeners,
// then the setting lines of each block (see blocks) that were added,
// removed or changed.
func settings(old, new *Config) []string {
	out := []string{}
	for _, n := range keysOf(old.Ports, new.Ports) {
		switch o, p := old.Ports[n], new.Ports[n]; {
		case o == nil:
			out = append(out, fmt.Sprintf("port %d (%s) added (line %d)", n, schemeName(p.TLS), p.Line))
		case p == nil:
			out = append(out, fmt.Sprintf("port %d (%s) removed (was line %d)", n, schemeName(o.TLS), o.Line))
		case o.TLS != p.TLS:
			out = append(out, fmt.Sprintf("port %d switched from %s to %s (line %d)", n, schemeName(o.TLS), schemeName(p.TLS), p.Line))
		}
	}
	ob, nb := blocks(old), blocks(new)
	for _, name := range keysOf(ob, nb) {
		o, n := ob[name], nb[name]
		switch {
		case o == nil: // an added block comes with its settings
			var lines []string
			for _, k := range keysOf(n)[1:] { // [0] is "", the block's first line
				lines = append(lines, n[k].text)
			}
			out = append(out, strings.TrimSuffix(fmt.Sprintf("%s added (line %d): %s", name, n[""].line, strings.Join(lines, ", ")), ": "))
		case n == nil:
			out = append(out, fmt.Sprintf("%s removed (was line %d)", name, o[""].line))
		}
		for _, k := range keysOf(o, n) {
			a, inOld := o[k]
			b, inNew := n[k]
			switch {
			case k == "" || o == nil || n == nil:
			case !inOld:
				out = append(out, fmt.Sprintf("%s: %s added (line %d)", name, b.text, b.line))
			case !inNew:
				out = append(out, fmt.Sprintf("%s: %s removed (was line %d)", name, a.text, a.line))
			case a.text != b.text:
				out = append(out, fmt.Sprintf("%s: %s  ->  %s (line %d)", name, a.text, strings.TrimPrefix(b.text, k+" "), b.line))
			}
		}
	}
	return out
}

type cfgLine struct {
	text string
	line int
}

// blocks returns the setting lines of each block in a config, by block:
// "global" (all global blocks), "site" and its first address as written,
// or "pool" and its name. Under "" is the block's first line. Backends are
// keyed by their whole text, other settings by name; rules are left out.
func blocks(c *Config) map[string]map[string]cfgLine {
	out := map[string]map[string]cfgLine{}
	cur := map[string]cfgLine{} // (lines before any block, which a valid config hasn't)
	for i, raw := range c.Lines {
		toks, err := splitLine(raw)
		if err != nil || len(toks) == 0 {
			continue
		}
		key, text := toks[0].s, joinTokens(toks)
		switch {
		case raw[0] != ' ' && raw[0] != '\t':
			name := strings.Join(strings.Fields(text)[:min(2, len(toks))], " ")
			if out[name] == nil {
				out[name] = map[string]cfgLine{"": {text, i + 1}}
			}
			cur = out[name]
		case key == "backend":
			cur[text] = cfgLine{text, i + 1}
		case key != "route":
			cur[key] = cfgLine{text, i + 1}
		}
	}
	return out
}
