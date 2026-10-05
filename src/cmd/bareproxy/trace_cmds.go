// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"bareproxy/internal/bp"
)

func init() {
	extraCommands["tail"] = tailCmd
	extraCommands["status"] = statusCmd
	extraCommands["events"] = eventsCmd
}

// traceClient makes an HTTP client that talks to the admin socket. A zero
// timeout means none, for streams.
func traceClient(sock string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}},
	}
}

// traceGet asks the admin socket for a path and returns the answer.
func traceGet(sock, path string) ([]byte, int, error) {
	if sock == "" || sock == "off" {
		return nil, 0, fmt.Errorf("the config has no admin socket")
	}
	resp, err := traceClient(sock, 5*time.Second).Get("http://bareproxy" + path)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

// traceWhy asks a running BareProxy for a request it still has in memory.
// It returns no record and no error when the server isn't there or doesn't
// have the request, so the caller can try the trace log file; asked says
// whether a server answered at all.
func traceWhy(sock, prefix string) (rec *bp.Record, asked bool, err error) {
	b, code, err := traceGet(sock, "/why?id="+url.QueryEscape(prefix))
	switch {
	case err != nil:
		return nil, false, nil
	case code == http.StatusNotFound:
		return nil, true, nil
	case code != http.StatusOK:
		return nil, true, fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	rec = &bp.Record{}
	if err := json.Unmarshal(b, rec); err != nil {
		return nil, true, err
	}
	return rec, true, nil
}

// traceJSON prints a value as one JSON line, the way the trace log has it.
func traceJSON(v any) {
	b, _ := bp.RecordJSON(v)
	fmt.Println(string(b))
}

// traceOpen reads the flags every command here takes (--config FILE and
// --json) and finds the admin socket. rest holds the other arguments.
func traceOpen(args []string) (sock string, asJSON bool, rest []string, ok bool) {
	var file string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config", "-c":
			if i++; i < len(args) {
				file = args[i]
			}
		case "--json":
			asJSON = true
		default:
			rest = append(rest, args[i])
		}
	}
	c, probs := bp.Load(configFile(file))
	if c == nil {
		for _, p := range probs {
			fmt.Fprintln(os.Stderr, p)
		}
		return "", false, nil, false
	}
	c.Close()
	return c.Admin, asJSON, rest, true
}

// traceFetch asks the running server for a path and returns the body.
func traceFetch(sock, path string) ([]byte, bool) {
	b, code, err := traceGet(sock, path)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "bareproxy: no running server answered (%v)\n", err)
	case code != http.StatusOK:
		fmt.Fprintf(os.Stderr, "bareproxy: %s\n", strings.TrimSpace(string(b)))
	default:
		return b, true
	}
	return nil, false
}

func tailCmd(args []string) int {
	sock, asJSON, filters, ok := traceOpen(args)
	if !ok {
		return 1
	}
	for _, f := range filters {
		if _, err := bp.ParseTailFilter(f); err != nil {
			fmt.Fprintln(os.Stderr, "bareproxy:", err)
			return 2
		}
	}
	if sock == "" || sock == "off" {
		fmt.Fprintln(os.Stderr, "bareproxy: the config has no admin socket")
		return 1
	}
	resp, err := traceClient(sock, 0).Get("http://bareproxy/tail?" + url.Values{"f": filters}.Encode())
	if err != nil {
		fmt.Fprintf(os.Stderr, "bareproxy: no running server answered (%v)\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "bareproxy: %s\n", strings.TrimSpace(string(b)))
		return 1
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if asJSON {
			fmt.Printf("%s\n", sc.Bytes())
			continue
		}
		var rec bp.Record
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		if rec.ID == "" { // {"dropped":N}
			var d struct{ Dropped int }
			json.Unmarshal(sc.Bytes(), &d)
			fmt.Printf("-- %d records skipped, the reader was too slow --\n", d.Dropped)
			continue
		}
		fmt.Println(tailLine(&rec))
	}
	fmt.Fprintln(os.Stderr, "bareproxy: the server closed the connection")
	return 1
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

func eventsCmd(args []string) int {
	sock, asJSON, rest, ok := traceOpen(args)
	if !ok || len(rest) > 0 {
		usage()
		return 2
	}
	b, ok := traceFetch(sock, "/events")
	if !ok {
		return 1
	}
	if asJSON {
		os.Stdout.Write(b)
		return 0
	}
	var evs []bp.Event
	if err := json.Unmarshal(b, &evs); err != nil {
		fmt.Fprintln(os.Stderr, "bareproxy:", err)
		return 1
	}
	if len(evs) == 0 {
		fmt.Println("No events yet.")
	}
	for _, e := range evs {
		fmt.Printf("%s  %-11s  %s\n", strings.Replace(strings.TrimSuffix(e.Time, "Z"), "T", " ", 1), e.Kind, e.Text)
	}
	return 0
}

func statusCmd(args []string) int {
	sock, asJSON, rest, ok := traceOpen(args)
	if !ok || len(rest) > 0 {
		usage()
		return 2
	}
	b, ok := traceFetch(sock, "/status")
	if !ok {
		return 1
	}
	if asJSON {
		os.Stdout.Write(b)
		return 0
	}
	var st bp.Status
	if err := json.Unmarshal(b, &st); err != nil {
		fmt.Fprintln(os.Stderr, "bareproxy:", err)
		return 1
	}
	printStatus(&st)
	return 0
}

func count(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func clock(stamp string) string {
	if len(stamp) >= 19 {
		return stamp[11:19] + " UTC"
	}
	return stamp
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
	fmt.Printf("BareProxy %s, up %s (since %s)\n", st.Version, age(st.UptimeSeconds), strings.Replace(strings.TrimSuffix(st.Started, "Z"), "T", " ", 1)+" UTC")
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
	for _, w := range []struct {
		name string
		r    bp.Rate
	}{{"last minute", r.Last1m}, {"last 5 minutes", r.Last5m}} {
		more := ""
		if !w.r.Complete {
			more = " or more (the ring has wrapped)"
		}
		fmt.Printf("  %-15s %s%s, %d with status 5xx, %s\n", w.name, count(w.r.Requests, "request"), more, w.r.Status5xx, count(w.r.ProxyErrors, "proxy error"))
	}
}
