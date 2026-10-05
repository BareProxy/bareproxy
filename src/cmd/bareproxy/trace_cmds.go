// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"bareproxy/internal/bp"
)

// traceOptions are the options that why, tail, status and events take.
var traceOptions = []string{"--config", "-c", "--json"}

// traceWhy asks a running BareProxy for a request it still has in memory.
// It returns no record and no error when the server isn't there or doesn't
// have the request, so the caller can try the trace log file; asked says
// whether a server answered at all.
func traceWhy(sock, prefix string) (rec *bp.Record, asked bool, err error) {
	code, body, err := get(sock, "/why?id="+url.QueryEscape(prefix))
	switch {
	case err != nil:
		return nil, false, nil
	case code == http.StatusNotFound:
		return nil, true, nil
	case code != http.StatusOK:
		return nil, true, errors.New(strings.TrimSpace(body))
	}
	rec = &bp.Record{}
	if err := json.Unmarshal([]byte(body), rec); err != nil {
		return nil, true, err
	}
	return rec, true, nil
}

func tailCmd(args []string) int {
	o, _ := parse(args, false, traceOptions...)
	c := loadConfig(o.config)
	if c == nil {
		return 1
	}
	for _, f := range o.pos {
		if _, err := bp.ParseTailFilter(f); err != nil {
			fmt.Fprintln(os.Stderr, "bareproxy:", err)
			return 2
		}
	}
	if c.Admin == "" || c.Admin == "off" {
		return fail("the config has no admin socket")
	}
	resp, err := adminClient(c.Admin, 0).Get("http://bareproxy/tail?" + url.Values{"f": o.pos}.Encode())
	if err != nil {
		return fail("no running server answered (%v)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fail("%s", strings.TrimSpace(string(b)))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if o.json {
			fmt.Printf("%s\n", sc.Bytes())
			continue
		}
		var line struct {
			bp.Record
			Dropped int // a line that says {"dropped":N} instead of holding a record
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		if line.ID == "" {
			fmt.Printf("-- %d records skipped, the reader was too slow --\n", line.Dropped)
			continue
		}
		fmt.Println(tailLine(&line.Record))
	}
	return fail("the server closed the connection")
}

// tailLine is one record on one line: time (UTC), ID, method, host and
// path, status, outcome, time taken, and the rule line with its pool or folder.
func tailLine(r *bp.Record) string {
	t := r.Time
	if len(t) >= 23 {
		t = t[11:23]
	}
	status := "-"
	if r.Status != 0 {
		status = fmt.Sprint(r.Status)
	}
	rule := "-"
	switch {
	case r.Pool != "":
		rule = fmt.Sprintf("line %d pool %s", r.Line, r.Pool)
	case r.Folder != "":
		rule = fmt.Sprintf("line %d files %s", r.Line, r.Folder)
	case r.Line != 0:
		rule = fmt.Sprintf("line %d", r.Line)
	}
	ms := fmt.Sprintf("%.0f ms", r.MS)
	if r.MS < 10 {
		ms = fmt.Sprintf("%.1f ms", r.MS)
	}
	return fmt.Sprintf("%s  %s  %-4s %s%s  %s  %s  %s  %s", t, r.ID, r.Method, r.Host, r.Path, status, r.Outcome, ms, rule)
}

// fetch runs the commands that show one JSON document from a running
// server. With --json it prints the document as it came; otherwise it reads
// the document into v and calls text to print it.
func fetch(args []string, path string, v any, text func()) int {
	o, _ := parse(args, false, traceOptions...)
	c := loadConfig(o.config)
	if c == nil || len(o.pos) > 0 {
		return badUsage()
	}
	code, body, err := get(c.Admin, path)
	switch {
	case err != nil:
		return fail("no running server answered (%v)", err)
	case code != http.StatusOK:
		return fail("%s", strings.TrimSpace(body))
	case o.json:
		fmt.Print(body)
		return 0
	}
	if err := json.Unmarshal([]byte(body), v); err != nil {
		return fail("%v", err)
	}
	text()
	return 0
}

func eventsCmd(args []string) int {
	var evs []bp.Event
	return fetch(args, "/events", &evs, func() {
		if len(evs) == 0 {
			fmt.Println("No events yet.")
		}
		for _, e := range evs {
			fmt.Printf("%s  %-11s  %s\n", stamp(e.Time), e.Kind, e.Text)
		}
	})
}

func statusCmd(args []string) int {
	var st bp.Status
	return fetch(args, "/status", &st, func() { printStatus(&st) })
}

func count(n int, word string) string {
	if n != 1 {
		word += "s"
	}
	return fmt.Sprintf("%d %s", n, word)
}

// stamp turns 2026-10-01T06:00:00Z into 2026-10-01 06:00:00.
func stamp(t string) string {
	return strings.Replace(strings.TrimSuffix(t, "Z"), "T", " ", 1)
}

func clock(t string) string {
	if len(t) >= 19 {
		return t[11:19] + " UTC"
	}
	return t
}

func age(secs float64) string {
	d := time.Duration(secs) * time.Second
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd %dh", d/(24*time.Hour), d%(24*time.Hour)/time.Hour)
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", d/time.Hour, d%time.Hour/time.Minute)
	case d >= time.Minute:
		return fmt.Sprintf("%dm %ds", d/time.Minute, d%time.Minute/time.Second)
	}
	return fmt.Sprintf("%ds", d/time.Second)
}

func printStatus(st *bp.Status) {
	fmt.Printf("BareProxy %s, up %s (since %s UTC)\n", st.Version, age(st.UptimeSeconds), stamp(st.Started))
	fmt.Printf("Config %s, version %d\n\nListeners\n", st.ConfigFile, st.ConfigVersion)
	for _, l := range st.Listeners {
		kind := "http"
		if l.TLS {
			kind = "https"
		}
		fmt.Printf("  :%d %s  %s\n", l.Port, kind, strings.Join(l.Sites, ", "))
	}
	fmt.Println("\nSites")
	for _, s := range st.Sites {
		fmt.Printf("  %s (line %d), %s: %s\n", s.Name, s.Line, count(s.Rules, "rule"), strings.Join(s.Addresses, " "))
	}
	if len(st.Pools) > 0 {
		fmt.Println("\nPools")
	}
	for _, p := range st.Pools {
		fmt.Printf("  %s (line %d): %d of %d up. Checks: %s\n", p.Name, p.Line, p.Up, p.Size, p.Checks)
		for _, b := range p.Backends {
			line := fmt.Sprintf("    %-22s %s since %s", b.Addr, b.State, clock(b.Since))
			switch {
			case b.State == "up":
				line += fmt.Sprintf(", %d in flight", b.InFlight)
			case b.Failures > 0:
				line += fmt.Sprintf(", %d failures in a row (%s)", b.Failures, b.Reason)
			}
			fmt.Println(line)
		}
	}
	if len(st.Certificates) > 0 {
		fmt.Println("\nCertificates")
	}
	for _, c := range st.Certificates {
		left := fmt.Sprintf("%d days left", c.DaysLeft)
		if c.DaysLeft < 0 {
			left = fmt.Sprintf("expired %d days ago", -c.DaysLeft)
		}
		fmt.Printf("  %s  %s  ends %s, %s\n", c.Site, c.Subject, c.NotAfter[:10], left)
	}
	r := st.Requests
	if r.Ring.Limit == 0 {
		fmt.Println("\nRequests are not counted, because trace-memory is off")
		return
	}
	fmt.Printf("\nRequests (the %d most recent, held in memory", r.Ring.Records)
	if r.Ring.Oldest != "" {
		fmt.Printf(", back to %s", clock(r.Ring.Oldest))
	}
	fmt.Println(")")
	rateLine("last minute", r.Last1m)
	rateLine("last 5 minutes", r.Last5m)
}

func rateLine(name string, r bp.Rate) {
	more := ""
	if !r.Complete {
		more = " or more (the ring has wrapped)"
	}
	fmt.Printf("  %-15s %s%s, %d with status 5xx, %s\n", name, count(r.Requests, "request"), more, r.Status5xx, count(r.ProxyErrors, "proxy error"))
}
