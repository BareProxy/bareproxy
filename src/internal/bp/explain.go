// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// Explain says how a request would be handled, using the same matching and
// file lookup code as the server. With live pools it also says which
// backend would get the request right now.
func Explain(rt *Runtime, method, rawURL string, h http.Header, live bool) (string, error) {
	method = strings.ToUpper(cmp.Or(method, http.MethodGet))
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("bad URL: %v", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("give a full URL starting http:// or https://")
	}
	port := 443
	if u.Scheme == "http" {
		port = 80
	}
	if ps := u.Port(); ps != "" {
		if port, err = strconv.Atoi(ps); err != nil {
			return "", fmt.Errorf("bad port %q", ps)
		}
	}
	c := rt.Cfg
	var b strings.Builder
	if live {
		fmt.Fprintf(&b, "Running config %s, version %d\n", c.File, rt.Version)
	} else {
		fmt.Fprintf(&b, "Config %s, read from the file (no running server, so backend states are unknown)\n", c.File)
	}
	p := c.Ports[port]
	switch {
	case p == nil:
		fmt.Fprintf(&b, "Nothing listens on port %d, so the connection is refused\n", port)
		return b.String(), nil
	case p.TLS && u.Scheme == "http":
		fmt.Fprintf(&b, "Port %d expects HTTPS, so a plain HTTP request fails there\n", port)
		return b.String(), nil
	case !p.TLS && u.Scheme == "https":
		fmt.Fprintf(&b, "Port %d is plain HTTP, so an HTTPS request fails there\n", port)
		return b.String(), nil
	}
	host := strings.ToLower(u.Hostname())
	site, how := p.Find(host)
	if site == nil {
		fmt.Fprintf(&b, "No site for %s on port %d\nAction: 421 (no_site)\n", host, port)
		return b.String(), nil
	}
	if site.Synthetic {
		fmt.Fprintf(&b, "Site %s (line %d) is HTTPS, so plain HTTP on port 80 is redirected\n", site.Name, site.Line)
	} else {
		fmt.Fprintf(&b, "Site %s (line %d): %s\n", site.Name, site.Line, how)
	}
	raw := u.EscapedPath()
	if strings.Contains(u.RawPath, `\`) {
		raw = u.RawPath // EscapedPath would turn a raw backslash into %5C; the server sees it as sent and refuses it
	}
	raw = cmp.Or(raw, "/")
	norm, err := NormalizePath(raw, site.KeepEncodedSlash)
	if err != nil {
		fmt.Fprintf(&b, "Path %s refused: %v\nAction: 400 (bad_request)\n", raw, err)
		return b.String(), nil
	}
	route, checks := site.MatchRoute(method, norm, h)
	width := 0
	for _, ch := range checks {
		width = max(width, len(ch.Route.Text))
	}
	for _, ch := range checks {
		why := ch.Why
		if !ch.OK {
			why = "no: " + why
		}
		fmt.Fprintf(&b, "  line %-4d %-*s   %s\n", ch.Route.Line, width, ch.Route.Text, why)
	}
	if norm != raw {
		fmt.Fprintf(&b, "Normalized path: %s\n", norm)
	} else {
		fmt.Fprintf(&b, "Path: %s\n", norm)
	}
	if route == nil {
		b.WriteString("No rule matches\n")
		explainErrorPage(&b, site)
		b.WriteString("Action: 404 (no_route)\n")
		return b.String(), nil
	}
	a := route.Act
	switch a.Kind {
	case "respond":
		fmt.Fprintf(&b, "Action: BareProxy answers %d itself", a.Status)
		if a.Body != "" {
			fmt.Fprintf(&b, " with %q", a.Body)
		}
		b.WriteString("\n")
	case "redirect":
		fmt.Fprintf(&b, "Action: redirect %d to %s\n", a.Code, redirectTarget(a.URL, norm, u.RawQuery))
	case "https":
		fmt.Fprintf(&b, "Action: redirect 301 to %s\n", withQuery("https://"+host+norm, u.RawQuery))
	case "files":
		explainFiles(&b, site, route, norm, u.RawQuery, method, h)
	case "pool":
		explainPool(&b, rt, route, norm, method, u.Host, live)
	}
	return b.String(), nil
}

func explainFiles(b *strings.Builder, site *Site, route *Route, norm, query, method string, h http.Header) {
	a := route.Act
	if method != http.MethodGet && method != http.MethodHead {
		fmt.Fprintf(b, "Action: 405, files answer only GET and HEAD\n")
		return
	}
	fr := LookupFile(a.Root, norm)
	if a.Root == nil && fr.Status == 0 {
		explainFilesNoDisk(b, site, a, fr, norm, query)
		return
	}
	switch fr.Status {
	case 301:
		fmt.Fprintf(b, "Checked: %s\nIt's a folder\nAction: redirect 301 to %s\n", inFolder(a.Dir, fr.Checked[0]), withQuery(fr.Location, query))
	case 404:
		if len(fr.Checked) > 0 {
			fmt.Fprintf(b, "Checked: %s\n", inFolder(a.Dir, fr.Checked[0]))
		}
		fmt.Fprintf(b, "Exists: no (%s)\n", fr.Reason)
		explainErrorPage(b, site)
		b.WriteString("Action: 404\n")
	default:
		rep, variants := ChooseRep(a.Root, fr.Rel, h.Get("Accept-Encoding"))
		fmt.Fprintf(b, "File: %s\nExists: yes\nContent-Type: %s\n", inFolder(a.Dir, fr.Rel), ContentType(fr.Rel))
		if rep.Encoding != "" {
			fmt.Fprintf(b, "Representation: %s (Content-Encoding %s)\n", path.Base(rep.Name), rep.Encoding)
		} else if len(variants) > 0 {
			var names []string
			for _, v := range variants {
				names = append(names, path.Base(v))
			}
			fmt.Fprintf(b, "Also on disk: %s, sent to clients that accept them\n", strings.Join(names, ", "))
		}
		b.WriteString("Action: serve 200\n")
	}
}

// explainFilesNoDisk is the files branch where no folder is open, as in the
// browser demo. The file is worked out the same way as on a server, but
// nothing is looked up, so it says what each outcome would be.
func explainFilesNoDisk(b *strings.Builder, site *Site, a Action, fr FileResult, norm, query string) {
	name := fr.Checked[0]
	fmt.Fprintf(b, "Would check: %s\n", inFolder(a.Dir, name))
	b.WriteString("Not looked up: the browser demo doesn't read the disk, so it can't tell if that exists\n")
	explainErrorPage(b, site)
	if strings.HasSuffix(norm, "/") {
		fmt.Fprintf(b, "Action: serve 200 (%s) if the file exists, otherwise 404\n", ContentType(name))
		return
	}
	fmt.Fprintf(b, "Action: serve 200 (%s) if it's a file, redirect 301 to %s if it's a folder, otherwise 404\n", ContentType(name), withQuery(norm+"/", query))
}

// withQuery puts the request's query back on a redirect target, as the server does.
func withQuery(loc, query string) string {
	if query == "" {
		return loc
	}
	return loc + "?" + query
}

func explainErrorPage(b *strings.Builder, site *Site) {
	if site.Err404 == "" {
		return
	}
	rt, fr, ok := site.ErrorPage()
	switch {
	case ok:
		fmt.Fprintf(b, "Error page: %s (line %d), from %s\n", site.Err404, site.Err404Line, inFolder(rt.Act.Dir, fr.Rel))
	case rt == nil:
		fmt.Fprintf(b, "Error page: %s (line %d) matches no rule, so a plain 404 is sent\n", site.Err404, site.Err404Line)
	case rt.Act.Kind != "files":
		fmt.Fprintf(b, "Error page: %s (line %d) goes to rule line %d, which doesn't serve files, so a plain 404 is sent\n", site.Err404, site.Err404Line, rt.Line)
	case rt.Act.Root == nil && len(fr.Checked) > 0:
		fmt.Fprintf(b, "Error page: %s (line %d), from %s if that file exists\n", site.Err404, site.Err404Line, inFolder(rt.Act.Dir, fr.Checked[0]))
	default:
		fmt.Fprintf(b, "Error page: %s (line %d) isn't in %s, so a plain 404 is sent\n", site.Err404, site.Err404Line, rt.Act.Dir)
	}
}

func explainPool(b *strings.Builder, rt *Runtime, route *Route, norm, method, hostHeader string, live bool) {
	pool := rt.Pools[route.Act.Pool]
	up := norm
	if route.Act.Strip {
		up = stripPrefix(norm, route.Path)
	}
	hostHeader = cmp.Or(pool.Spec.HostHeader, hostHeader)
	fmt.Fprintf(b, "Sent upstream as %s %s with Host: %s\n", method, up, hostHeader)
	if !live {
		fmt.Fprintf(b, "Pool %s (line %d): %d backends, states unknown without a running server\n", pool.Spec.Name, pool.Spec.Line, len(pool.Backends))
		for _, bk := range pool.Backends {
			fmt.Fprintf(b, "  %s\n", bk.Spec.Addr)
		}
		return
	}
	next := pool.pick(nil, true)
	fmt.Fprintf(b, "Pool %s (line %d): %d of %d up, fewest in flight wins\n", pool.Spec.Name, pool.Spec.Line, pool.upCount(), len(pool.Backends))
	for _, bk := range pool.Backends {
		mark := ""
		if bk == next {
			mark = "next pick"
		}
		fmt.Fprintf(b, "  %-16s %-56s %s\n", bk.Spec.Addr, describeState(bk.Snapshot()), mark)
	}
	if next == nil {
		b.WriteString("Action: 503 (no_backend)\n")
	}
}
