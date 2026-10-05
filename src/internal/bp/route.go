// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// PathError says why a request path was refused.
type PathError struct{ Reason string }

func (e *PathError) Error() string { return e.Reason }

func isUnreserved(b byte) bool {
	return 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9' ||
		b == '-' || b == '.' || b == '_' || b == '~'
}

func isPathChar(b byte) bool {
	return isUnreserved(b) || strings.IndexByte("!$&'()*+,;=:@", b) >= 0
}

// NormalizePath turns a raw request path into the one form BareProxy routes
// on and forwards: escapes of plain characters decoded, other escapes in
// capitals, dot segments resolved and runs of slashes merged. It refuses
// broken escapes, control characters, backslashes, paths that climb above
// the root and, unless keepSlash is set, encoded slashes and backslashes.
func NormalizePath(raw string, keepSlash bool) (string, error) {
	if raw == "" || raw[0] != '/' {
		return "", &PathError{"the path doesn't start with /"}
	}
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c == '%':
			if i+2 >= len(raw) {
				return "", &PathError{"broken % escape"}
			}
			n, err := strconv.ParseUint(raw[i+1:i+3], 16, 8)
			if err != nil {
				return "", &PathError{"broken % escape"}
			}
			v := byte(n)
			i += 2
			switch {
			case isUnreserved(v):
				b.WriteByte(v)
			case v == '/' || v == '\\':
				if !keepSlash {
					return "", &PathError{"encoded slash or backslash (%2F or %5C)"}
				}
				fmt.Fprintf(&b, "%%%02X", v)
			case v < 0x20 || v == 0x7f:
				return "", &PathError{"encoded control character"}
			default:
				fmt.Fprintf(&b, "%%%02X", v)
			}
		case c == '\\':
			return "", &PathError{"backslash in the path"}
		case c < 0x20 || c == 0x7f:
			return "", &PathError{"control character in the path"}
		case c == '/' || isPathChar(c):
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	segs := strings.Split(b.String()[1:], "/")
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		switch s {
		case "", ".":
		case "..":
			if len(out) == 0 {
				return "", &PathError{"the path climbs above /"}
			}
			out = out[:len(out)-1]
		default:
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return "/", nil
	}
	p := "/" + strings.Join(out, "/")
	if last := segs[len(segs)-1]; last == "" || last == "." || last == ".." {
		p += "/"
	}
	return p, nil
}

func isNormalPath(s string) bool {
	n, err := NormalizePath(s, false)
	return err == nil && n == s
}

// RuleCheck records whether one rule matched a request, and why not.
type RuleCheck struct {
	Route *Route
	OK    bool
	Why   string
}

// MatchRoute tries a site's rules from the top and returns the first match,
// with a note for each rule tried, as explain shows them.
func (s *Site) MatchRoute(method, path string, h http.Header) (*Route, []RuleCheck) {
	var checks []RuleCheck
	r := s.match(method, path, h, &checks)
	return r, checks
}

// match is MatchRoute's loop. The server passes nil notes: the same code,
// without building any text.
func (s *Site) match(method, path string, h http.Header, notes *[]RuleCheck) *Route {
	for _, r := range s.Routes {
		ok, why := r.test(method, path, h, notes != nil)
		if notes != nil {
			*notes = append(*notes, RuleCheck{r, ok, why})
		}
		if ok {
			return r
		}
	}
	return nil
}

func (r *Route) matches(method, path string, h http.Header) (bool, string) {
	return r.test(method, path, h, true)
}

// test says whether a rule takes a request and, with notes on, why not.
func (r *Route) test(method, path string, h http.Header, notes bool) (bool, string) {
	no := func(why ...string) (bool, string) {
		if !notes {
			return false, ""
		}
		return false, strings.Join(why, "")
	}
	switch {
	case r.Prefix && r.Path == "":
	case r.Prefix:
		if path != r.Path && !strings.HasPrefix(path, r.Path+"/") {
			return no("path is not ", r.Path, " or below")
		}
	case path != r.Path:
		return no("path is not ", r.Path)
	}
	if len(r.Methods) > 0 && !methodAllowed(r.Methods, method) {
		return no("method is not ", strings.Join(r.Methods, ","))
	}
	for _, c := range r.Headers {
		vals := h.Values(c.Name)
		if len(vals) == 0 {
			return no("no ", c.Name, " header")
		}
		if c.HasValue && !slices.Contains(vals, c.Value) {
			return no(c.Name, " is not ", c.Value)
		}
	}
	return true, "match"
}

func methodAllowed(ms []string, m string) bool {
	return slices.Contains(ms, m) || (m == http.MethodHead && slices.Contains(ms, http.MethodGet))
}

// stripPrefix removes a route's prefix from a path, keeping a leading slash.
func stripPrefix(path, prefix string) string {
	if rest := strings.TrimPrefix(path, prefix); rest != "" {
		return rest
	}
	return "/"
}

// redirectTarget builds a redirect's Location. A bare origin keeps the
// request's path and query; a URL with a path is used exactly.
func redirectTarget(base, norm, query string) string {
	if u, err := url.Parse(base); err != nil || u.Path != "" {
		return base
	}
	return withQuery(base+norm, query)
}
