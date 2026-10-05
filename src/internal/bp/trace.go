// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"bufio"
	"bytes"
	"encoding/json"
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
	TraceID     string    `json:"trace_id,omitempty"`
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

// TraceLog writes one JSON line per request. A file log can rotate by size.
type TraceLog struct {
	Spec string
	mu   sync.Mutex // guards everything below
	w    io.Writer  // nil when the log is off or closed
	f    *os.File
	size int64  // bytes in the file now
	max  int64  // a write that would take the file past this rotates it first; 0 means never
	keep int    // old files kept: Spec.1 is the newest
	buf  []byte // the line being written, reused
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
		if st, err := f.Stat(); err == nil {
			t.size = st.Size()
		}
	}
	return t, nil
}

// SetRotation makes a file log rotate once it would pass size bytes, keeping
// keep old files. A size of 0 turns rotation off.
func (t *TraceLog) SetRotation(size int64, keep int) {
	t.mu.Lock()
	t.max, t.keep = size, max(keep, 1)
	t.mu.Unlock()
}

// Write writes a record as one JSON line.
func (t *TraceLog) Write(rec *Record) {
	if t == nil {
		return
	}
	if b, err := RecordJSON(rec); err == nil {
		t.WriteLine(b)
	}
}

// RecordJSON is a record as one line of JSON, with < > and & left as they
// are, so a rule reads route /api/* -> api in the log, not \u003e.
func RecordJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// WriteLine writes one record's JSON as a line. If the line would take the
// file past its size limit, the file rotates first, so no file passes the
// limit unless one record does.
func (t *TraceLog) WriteLine(js []byte) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.w == nil {
		return
	}
	t.buf = append(append(t.buf[:0], js...), '\n')
	if t.f != nil && t.max > 0 && t.size > 0 && t.size+int64(len(t.buf)) > t.max {
		t.rotate()
	}
	n, _ := t.w.Write(t.buf)
	t.size += int64(n)
}

// rotate moves Spec to Spec.1, Spec.1 to Spec.2 and so on, dropping the
// file past keep, then starts a new file. If the new file can't be opened,
// records go to stderr rather than being lost.
func (t *TraceLog) rotate() {
	t.f.Close()
	for i := t.keep; i >= 1; i-- {
		os.Rename(rotated(t.Spec, i-1), rotated(t.Spec, i))
	}
	f, err := os.OpenFile(t.Spec, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		t.f, t.w = nil, os.Stderr
		return
	}
	t.f, t.w, t.size = f, f, 0
}

// Close closes the trace log file, if there is one. Writes after that are
// dropped.
func (t *TraceLog) Close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f != nil {
		t.f.Close()
		t.f, t.w = nil, nil
	}
}

// rotated names a trace log file: i is 0 for the log itself and 1 for the
// newest rotated file (file.1).
func rotated(file string, i int) string {
	if i == 0 {
		return file
	}
	return fmt.Sprintf("%s.%d", file, i)
}

// FindRecord finds the record whose ID starts with prefix in a trace log
// file and the rotated files beside it (file.1, file.2 and so on).
func FindRecord(file, prefix string) (*Record, error) {
	prefix, err := cleanPrefix(prefix)
	if err != nil {
		return nil, err
	}
	needle := []byte(`"id":"` + prefix)
	ids := map[string]bool{}
	var found *Record
	var firstErr error
	opened := false
	for i := 0; ; i++ {
		f, err := os.Open(rotated(file, i))
		if err != nil {
			if i == 0 {
				firstErr = err
				continue
			}
			break
		}
		opened = true
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
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
		err = sc.Err()
		f.Close()
		if err != nil {
			return nil, err
		}
	}
	switch {
	case !opened:
		return nil, firstErr
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
	note := func(format, value string) {
		if value != "" {
			fmt.Fprintf(&b, format, value)
		}
	}
	when := rec.Time
	if t, err := time.Parse(time.RFC3339Nano, rec.Time); err == nil {
		when = t.UTC().Format("2 Jan 2006 15:04:05.000 UTC")
	}
	fmt.Fprintf(&b, "Request %s, %s, config version %d\n", rec.ID, when, rec.Config)
	conn := rec.Proto
	if rec.TLS != "" {
		conn = "TLS " + rec.TLS + ", " + rec.Proto
	}
	target := withQuery(rec.Scheme+"://"+rec.Host+rec.Path, rec.Query)
	fmt.Fprintf(&b, "%s %s from %s, %s\n", rec.Method, target, rec.Client, conn)
	note("W3C trace ID %s\n", rec.TraceID)
	b.WriteString("\n")
	if rec.Site != "" {
		fmt.Fprintf(&b, "Site %s (line %d)\n", rec.Site, rec.SiteLine)
	}
	note("Normalized path: %s\n", rec.NormPath)
	if rec.Rule != "" {
		fmt.Fprintf(&b, "Rule line %d: %s\n", rec.Line, rec.Rule)
	}
	switch {
	case rec.Pool != "":
		fmt.Fprintf(&b, "  sent upstream as %s\n", rec.Upstream)
		if rec.TraceID != "" {
			fmt.Fprintf(&b, "  traceparent sent upstream with parent ID %s\n", rec.ID)
		}
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
	note("Redirected to %s\n", rec.Location)
	note("Reason: %s\n", rec.Reason)
	note("Error page: %s\n", rec.ErrorPage)
	fmt.Fprintf(&b, "Response %d, %s, %s in total (outcome %s)\n", rec.Status, fmtBytes(rec.BytesOut), fmtMS(rec.MS), rec.Outcome)
	return b.String()
}

// parseTraceparent checks a W3C traceparent value of version 00: 32 hex
// digits of trace ID and 16 of parent ID, neither all zeros, then 2 of
// flags, all in lower case.
func parseTraceparent(v string) (traceID, flags string, ok bool) {
	if len(v) != 55 || v[:3] != "00-" || v[35] != '-' || v[52] != '-' {
		return "", "", false
	}
	traceID, parent, flags := v[3:35], v[36:52], v[53:]
	if !lowerHex(traceID) || !lowerHex(parent) || !lowerHex(flags) ||
		strings.Trim(traceID, "0") == "" || strings.Trim(parent, "0") == "" {
		return "", "", false
	}
	return traceID, flags, true
}

func lowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// requestTrace returns the trace ID and flags of a request's traceparent
// header, or empty strings unless there is exactly one and it is valid.
func requestTrace(h http.Header) (traceID, flags string) {
	if vs := h.Values("Traceparent"); len(vs) == 1 {
		traceID, flags, _ = parseTraceparent(vs[0])
	}
	return traceID, flags
}

// setTraceparent puts the request ID in as the new parent ID of the
// outgoing request's traceparent when the client's one (in) is valid, which
// is exactly when the record has a trace ID. Anything else passes unchanged,
// and BareProxy never starts a trace of its own.
func setTraceparent(out, in http.Header, rec *Record) {
	if id, flags := requestTrace(in); id != "" {
		out.Set("Traceparent", "00-"+id+"-"+rec.ID+"-"+flags)
	}
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
