// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Record is the one record every request leaves. Fields that don't apply
// are left out of the JSON.
type Record struct {
	ID          string    `json:"id"`
	Time        string    `json:"time"`
	Config      int       `json:"config"`
	Client      string    `json:"client"`
	TLS         string    `json:"tls,omitempty"`
	SNI         string    `json:"sni,omitempty"`
	Proto       string    `json:"proto"`
	Method      string    `json:"method"`
	Scheme      string    `json:"scheme"`
	Host        string    `json:"host"`
	Path        string    `json:"path"`
	NormPath    string    `json:"norm_path,omitempty"`
	Query       string    `json:"query,omitempty"`
	Site        string    `json:"site,omitempty"`
	SiteLine    int       `json:"site_line,omitempty"`
	Line        int       `json:"line,omitempty"`
	Rule        string    `json:"rule,omitempty"`
	Location    string    `json:"location,omitempty"`
	Upstream    string    `json:"upstream_path,omitempty"`
	Pool        string    `json:"pool,omitempty"`
	PoolLine    int       `json:"pool_line,omitempty"`
	PoolUp      int       `json:"pool_up,omitempty"`
	PoolSize    int       `json:"pool_size,omitempty"`
	Skipped     []Skip    `json:"skipped,omitempty"`
	Attempts    []Attempt `json:"attempts,omitempty"`
	Folder      string    `json:"folder,omitempty"`
	Checked     []string  `json:"checked,omitempty"`
	File        string    `json:"file,omitempty"`
	Sent        string    `json:"sent,omitempty"`
	Encoding    string    `json:"encoding,omitempty"`
	ContentType string    `json:"content_type,omitempty"`
	ErrorPage   string    `json:"error_page,omitempty"`
	Status      int       `json:"status"`
	BytesIn     int64     `json:"bytes_in"`
	BytesOut    int64     `json:"bytes_out"`
	MS          float64   `json:"ms"`
	Outcome     string    `json:"outcome"`
	Reason      string    `json:"reason,omitempty"`
}

// Skip is a backend passed over because it wasn't up.
type Skip struct {
	Backend string `json:"backend"`
	State   string `json:"state"`
	Why     string `json:"why,omitempty"`
}

// Attempt is one try at a backend.
type Attempt struct {
	Backend     string  `json:"backend"`
	Status      int     `json:"status,omitempty"`
	Error       string  `json:"error,omitempty"`
	MS          float64 `json:"ms,omitempty"`
	FirstByteMS float64 `json:"first_byte_ms,omitempty"`
}

// TraceLog writes one JSON line per request.
type TraceLog struct {
	Spec string
	mu   sync.Mutex
	w    io.Writer
	f    *os.File
}

// OpenTraceLog opens the trace log a config names: stdout, off or a file.
func OpenTraceLog(spec string) (*TraceLog, error) {
	t := &TraceLog{Spec: spec}
	switch spec {
	case "off":
	case "stdout":
		t.w = os.Stdout
	default:
		if err := os.MkdirAll(filepath.Dir(spec), 0o755); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(spec, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
		if err != nil {
			return nil, err
		}
		t.f, t.w = f, f
	}
	return t, nil
}

func (t *TraceLog) Write(rec *Record) {
	if t == nil || t.w == nil {
		return
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	b = append(b, '\n')
	t.mu.Lock()
	t.w.Write(b)
	t.mu.Unlock()
}

// Close closes the trace log file, if there is one.
func (t *TraceLog) Close() {
	if t != nil && t.f != nil {
		t.f.Close()
	}
}

// FindRecord finds the record whose ID starts with prefix in a trace log file.
func FindRecord(file, prefix string) (*Record, error) {
	prefix = strings.ToLower(prefix)
	if len(prefix) < 6 {
		return nil, errors.New("give at least 6 characters of the request ID")
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	needle := []byte(`"id":"` + prefix)
	ids := map[string]bool{}
	var found *Record
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, needle) {
			continue
		}
		var rec Record
		if json.Unmarshal(line, &rec) != nil || !strings.HasPrefix(rec.ID, prefix) {
			continue
		}
		ids[rec.ID] = true
		found = &rec
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	switch {
	case found == nil:
		return nil, fmt.Errorf("no request with an ID starting %s in %s", prefix, file)
	case len(ids) > 1:
		return nil, fmt.Errorf("%d requests have IDs starting %s; give more characters", len(ids), prefix)
	}
	return found, nil
}

// RenderWhy tells the story of one request from its record.
func RenderWhy(rec *Record) string {
	var b strings.Builder
	when := rec.Time
	if t, err := time.Parse(time.RFC3339Nano, rec.Time); err == nil {
		when = t.UTC().Format("2 Jan 2006 15:04:05.000 UTC")
	}
	fmt.Fprintf(&b, "Request %s, %s, config version %d\n", rec.ID, when, rec.Config)
	conn := rec.Proto
	if rec.TLS != "" {
		conn = "TLS " + rec.TLS + ", " + rec.Proto
	}
	target := rec.Scheme + "://" + rec.Host + rec.Path
	if rec.Query != "" {
		target += "?" + rec.Query
	}
	fmt.Fprintf(&b, "%s %s from %s, %s\n\n", rec.Method, target, rec.Client, conn)
	if rec.Site != "" {
		fmt.Fprintf(&b, "Site %s (line %d)\n", rec.Site, rec.SiteLine)
	}
	if rec.NormPath != "" {
		fmt.Fprintf(&b, "Normalized path: %s\n", rec.NormPath)
	}
	if rec.Rule != "" {
		fmt.Fprintf(&b, "Rule line %d: %s\n", rec.Line, rec.Rule)
	}
	switch {
	case rec.Pool != "":
		fmt.Fprintf(&b, "  sent upstream as %s\n", rec.Upstream)
		fmt.Fprintf(&b, "Pool %s (line %d): %d of %d up\n", rec.Pool, rec.PoolLine, rec.PoolUp, rec.PoolSize)
		for _, s := range rec.Skipped {
			fmt.Fprintf(&b, "  %-16s skipped: %s\n", s.Backend, s.Why)
		}
		for i, a := range rec.Attempts {
			how := "tried first, fewest in flight"
			if i > 0 {
				how = "tried next"
			}
			if a.Error != "" {
				fmt.Fprintf(&b, "  %-16s %s: %s after %s\n", a.Backend, how, a.Error, fmtMS(a.MS))
			} else {
				fmt.Fprintf(&b, "  %-16s %s: %d %s, first byte after %s\n", a.Backend, how, a.Status, http.StatusText(a.Status), fmtMS(a.FirstByteMS))
			}
		}
	case rec.Folder != "":
		switch {
		case rec.File != "":
			fmt.Fprintf(&b, "File: %s\n", inFolder(rec.Folder, rec.File))
			if rec.Sent != "" {
				fmt.Fprintf(&b, "Representation: %s (Content-Encoding %s)\n", path.Base(rec.Sent), rec.Encoding)
			} else {
				fmt.Fprintf(&b, "Representation: %s\n", path.Base(rec.File))
			}
			fmt.Fprintf(&b, "Content-Type: %s\n", rec.ContentType)
		case len(rec.Checked) > 0:
			fmt.Fprintf(&b, "Checked: %s\n", inFolder(rec.Folder, rec.Checked[0]))
		}
	}
	if rec.Location != "" {
		fmt.Fprintf(&b, "Redirected to %s\n", rec.Location)
	}
	if rec.Reason != "" {
		fmt.Fprintf(&b, "Reason: %s\n", rec.Reason)
	}
	if rec.ErrorPage != "" {
		fmt.Fprintf(&b, "Error page: %s\n", rec.ErrorPage)
	}
	fmt.Fprintf(&b, "Response %d, %s, %s in total (outcome %s)\n", rec.Status, fmtBytes(rec.BytesOut), fmtMS(rec.MS), rec.Outcome)
	return b.String()
}

func fmtMS(ms float64) string {
	if ms < 10 {
		return fmt.Sprintf("%.1f ms", ms)
	}
	return fmt.Sprintf("%.0f ms", ms)
}

func fmtBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}
