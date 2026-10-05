// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"bufio"
	"fmt"
	"go/ast"
	goparser "go/parser"
	gotoken "go/token"
	mrand "math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestPlanGolden writes plan's output for many config pairs to the file
// named by BP_PLAN_GOLDEN, so a change to plan.go can be checked to leave
// every byte the same: run it before and after, and compare the files.
// It covers every pair of configs in plan_test.go's TestPlan functions,
// the generated pairs and requests of TestPlanExactOnGeneratedPairs, and
// the generated configs of TestPlanWarningsOnGeneratedConfigs.
func TestPlanGolden(t *testing.T) {
	out := os.Getenv("BP_PLAN_GOLDEN")
	if out == "" {
		t.Skip("set BP_PLAN_GOLDEN to a file name to write plan output for comparison")
	}
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	dir := planDir(t, "release-41", "release-42", "f1", "f2", "pub")
	writeTestCert(t, dir)
	clean := func(s string) string { return strings.ReplaceAll(s, dir, "DIR") }
	saved := MaxPlanClasses
	defer func() { MaxPlanClasses = saved }()
	plan := func(name string, oc, nc *Config) *PlanResult {
		p := MakePlan(oc, nc)
		js, err := p.JSON()
		fmt.Fprintf(w, "== %s (max %d) empty=%v\n%s%s\nerr=%v\n", name, MaxPlanClasses, p.Empty(), clean(p.Text()), clean(string(js)), err)
		return p
	}
	ask := func(p *PlanResult, oc, nc *Config, port int, q genReq) {
		k := p.Classify(port, q.host, q.method, q.path, q.h)
		fmt.Fprintf(w, "%d %s %s %s %v: %d %v", port, q.host, q.method, q.path, q.h, k, p.Covers(port, q.host, q.method, q.path, q.h))
		for _, c := range []*Config{oc, nc} {
			if c != nil {
				fmt.Fprintf(w, " | %s", clean(RequestEffect(c, port, q.host, q.method, q.path, q.h)))
			}
		}
		w.WriteString("\n")
	}

	// (a) and (b): every ordered pair of the configs written out in the
	// TestPlan functions, nil on either side too, at the normal class limit
	// and at small ones (so the changed-rule listing runs as well).
	fs := gotoken.NewFileSet()
	file, err := goparser.ParseFile(fs, "plan_test.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || !strings.HasPrefix(fn.Name.Name, "TestPlan") {
			continue
		}
		var cfgs []*Config
		ast.Inspect(fn, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != gotoken.STRING || !strings.HasPrefix(lit.Value, "`") {
				return true
			}
			src, _ := strconv.Unquote(lit.Value)
			if c, probs := Parse(filepath.Join(dir, "bareproxy.conf"), src); !HasErrors(probs) {
				t.Cleanup(c.Close)
				cfgs = append(cfgs, c)
			}
			return true
		})
		if len(cfgs) == 0 {
			continue
		}
		reqs, ports := goldenRequests(cfgs)
		for qi, q := range reqs {
			fmt.Fprintf(w, "request %d: %s %s %s %v\n", qi, q.host, q.method, q.path, q.h)
		}
		for ci, c := range cfgs {
			for _, port := range ports {
				for qi, q := range reqs {
					fmt.Fprintf(w, "effect %d %d %d: %s\n", ci, port, qi, clean(RequestEffect(c, port, q.host, q.method, q.path, q.h)))
				}
			}
		}
		cfgs = append(cfgs, nil)
		for _, max := range []int{saved, 5, 40} {
			MaxPlanClasses = max
			for i, oc := range cfgs {
				for j, nc := range cfgs {
					p := plan(fmt.Sprintf("%s %d->%d", fn.Name.Name, i, j), oc, nc)
					for _, port := range ports {
						var ks []string
						for _, q := range reqs {
							k := p.Classify(port, q.host, q.method, q.path, q.h)
							if (k >= 0) != p.Covers(port, q.host, q.method, q.path, q.h) {
								t.Fatalf("Covers disagrees with Classify")
							}
							ks = append(ks, strconv.Itoa(k))
						}
						fmt.Fprintf(w, "classify %d: %s\n", port, strings.Join(ks, " "))
					}
				}
			}
		}
	}
	MaxPlanClasses = saved

	// (c): the generated pairs and requests of the exactness test, drawn in
	// the same order from the same seed.
	r := mrand.New(mrand.NewPCG(2026, 10))
	cands := candidates()
	for i := 0; i < 1000; i++ {
		oldSites := genConfig(r)
		newSites := mutate(r, oldSites)
		oc, op := Parse(filepath.Join(dir, "old.conf"), renderSites(oldSites))
		nc, np := Parse(filepath.Join(dir, "new.conf"), renderSites(newSites))
		if HasErrors(op) || HasErrors(np) {
			t.Fatalf("pair %d: generated config has errors", i)
		}
		p := plan(fmt.Sprintf("generated %d", i), oc, nc)
		for j := 0; j < 300; j++ {
			ask(p, oc, nc, 80, genReq{pick(r, reqHosts), pick(r, reqMethods), pick(r, reqPaths), genHeader(r)})
		}
		var ks []string
		for _, q := range cands {
			ks = append(ks, strconv.Itoa(p.Classify(80, q.host, q.method, q.path, q.h)))
		}
		fmt.Fprintf(w, "candidates %s\n", strings.Join(ks, " "))
		plan(fmt.Sprintf("generated %d reversed", i), nc, oc)
		if i%10 == 0 {
			MaxPlanClasses = 60
			plan(fmt.Sprintf("generated %d few classes", i), oc, nc)
			MaxPlanClasses = saved
		}
		oc.Close()
		nc.Close()
	}

	// The warnings test's generated configs.
	r = mrand.New(mrand.NewPCG(11, 3))
	for i := 0; i < 200; i++ {
		c, probs := Parse(filepath.Join(dir, "bareproxy.conf"), renderSites(genConfig(r)))
		if HasErrors(probs) {
			t.Fatalf("config %d has errors", i)
		}
		plan(fmt.Sprintf("warnings %d", i), nil, c)
		c.Close()
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// goldenRequests builds requests that reach every rule of some configs in
// several ways, and the ports to send them to.
func goldenRequests(cfgs []*Config) ([]genReq, []int) {
	hosts := map[string]bool{"other.test": true, "EXAMPLE.COM.": true, "x.example.com": true}
	paths := map[string]bool{"/": true, "/zz": true, "/404.html": true}
	headers := []http.Header{{}}
	portSet := map[int]bool{80: true, 9: true}
	for _, c := range cfgs {
		for n, p := range c.Ports {
			portSet[n] = true
			for h := range p.Exact {
				hosts[h] = true
				hosts["x."+h] = true
			}
			for d := range p.Wild {
				hosts["w."+d] = true
			}
		}
		for _, s := range c.Sites {
			for _, rt := range s.Routes {
				for _, p := range []string{rt.Path, rt.Path + "/zz", rt.Path + "x", rt.Path + "/"} {
					if strings.HasPrefix(p, "/") {
						paths[p] = true
					}
				}
				for _, hc := range rt.Headers {
					headers = append(headers, http.Header{hc.Name: {hc.Value}}, http.Header{hc.Name: {"~other"}})
				}
			}
		}
	}
	sorted := func(m map[string]bool) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	var reqs []genReq
	for _, host := range sorted(hosts) {
		for _, m := range []string{"GET", "HEAD", "POST", "DELETE", "PUT"} {
			for _, p := range sorted(paths) {
				for _, h := range headers {
					reqs = append(reqs, genReq{host, m, p, h})
				}
			}
		}
	}
	var ports []int
	for n := range portSet {
		ports = append(ports, n)
	}
	sort.Ints(ports)
	return reqs, ports
}

// TestPlanTiming times a plan for one changed rule on a site of 300 rules.
// It only runs when BP_PLAN_GOLDEN is set.
func TestPlanTiming(t *testing.T) {
	if os.Getenv("BP_PLAN_GOLDEN") == "" {
		t.Skip("set BP_PLAN_GOLDEN to time plan")
	}
	dir := planDir(t)
	src := func(changed int) string {
		var b strings.Builder
		b.WriteString("site http://example.com\n")
		for i := 0; i < 300; i++ {
			fmt.Fprintf(&b, "  route GET /r%d/* header X-V=%d -> respond 200 \"%d\"\n", i, i%7, i)
			if i == changed {
				b.WriteString("  route /new/* -> respond 201\n")
			}
		}
		return b.String()
	}
	oc, nc := mustParse(t, dir, src(-1)), mustParse(t, dir, src(150))
	best := time.Duration(1 << 62)
	for i := 0; i < 5; i++ {
		start := time.Now()
		MakePlan(oc, nc)
		best = min(best, time.Since(start))
	}
	t.Logf("plan for one changed rule of 300: best of 5 runs %v", best)
}
