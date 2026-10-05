// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// traceTestID makes distinct 16-digit IDs whose first 6 digits differ too.
func traceTestID(i int) string { return fmt.Sprintf("%06x%010x", i*4099+0x100000, i) }

// traceTestRec makes a record and its JSON. pad lengthens the JSON.
func traceTestRec(id string, pad int) (*Record, []byte) {
	rec := &Record{ID: id, Time: "2026-10-05T09:00:00.000Z", Method: "GET", Host: "example.com",
		Path: "/x", Status: 200, Outcome: "ok", Reason: strings.Repeat("x", pad)}
	js, _ := json.Marshal(rec)
	return rec, js
}

// traceDo is like fixture.do but takes headers that can repeat.
func (f *fixture) traceDo(target string, h http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", target, nil)
	req.Host = "example.com:8080"
	for k, vs := range h {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rr := httptest.NewRecorder()
	f.h.ServeHTTP(rr, req)
	return rr
}

func TestTraceRingEvictsOldestFirstAndFindsByPrefix(t *testing.T) {
	m := newTraceMem()
	_, one := traceTestRec("aaaaaa0000000001", 0)
	limit := int64(3*len(one) + len(one)/2) // room for three records, not four
	add := func(id string) {
		rec, js := traceTestRec(id, 0)
		m.Add(rec, js, limit)
	}
	for _, id := range []string{"eeeeee0000000000", "aaaaaa0000000001", "aaaaaa0000000002", "bbbbbb0000000003"} {
		add(id)
	}
	if st := m.Stats(); st.Records != 3 || st.Bytes != int64(3*len(one)) || st.Limit != limit {
		t.Fatalf("after 4 adds the ring holds %+v, want 3 records of %d bytes", st, len(one))
	}
	if _, n := m.Find("eeeeee"); n != 0 {
		t.Errorf("the oldest record should be out, but Find saw %d", n)
	}
	if _, n := m.Find("aaaaaa"); n != 2 {
		t.Errorf("a shared prefix should match 2 records, got %d", n)
	}
	js, n := m.Find("aaaaaa0000000002")
	var got Record
	if n != 1 || json.Unmarshal(js, &got) != nil || got.ID != "aaaaaa0000000002" {
		t.Errorf("full ID: n=%d, record %q", n, js)
	}
	if _, n := m.Find("bbbbbb"); n != 1 {
		t.Errorf("bbbbbb: %d matches, want 1", n)
	}
	add("cccccc0000000004") // pushes out aaaaaa...01, so aaaaaa is unique now
	if js, n := m.Find("aaaaaa"); n != 1 || !strings.Contains(string(js), "aaaaaa0000000002") {
		t.Errorf("after eviction the prefix should find the one left, got n=%d %s", n, js)
	}
	if _, n := m.Find("ffffff"); n != 0 {
		t.Errorf("unknown prefix matched %d records", n)
	}
	var nilMem *TraceMem
	if _, n := nilMem.Find("aaaaaa"); n != 0 {
		t.Errorf("a nil ring found something")
	}
}

func TestTraceRingKeepsItsInvariantsUnderChurn(t *testing.T) {
	m := newTraceMem()
	const limit, total = 40000, 12000
	for i := 1; i <= total; i++ {
		rec, js := traceTestRec(fmt.Sprintf("%016x", i), (i*37)%300)
		m.Add(rec, js, limit)
		if m.bytes > limit {
			t.Fatalf("after %d adds the ring holds %d bytes, over the limit %d", i, m.bytes, limit)
		}
	}
	held := m.recs[m.start:]
	var sum int64
	for k, e := range held {
		if want := fmt.Sprintf("%016x", total-len(held)+k+1); string(e.id[:]) != want {
			t.Fatalf("held record %d is %s, want %s: the ring must hold the newest records in order", k, e.id[:], want)
		}
		sum += int64(len(e.js))
	}
	if sum != m.bytes {
		t.Errorf("the bytes counter says %d, the records hold %d", m.bytes, sum)
	}
	if len(held) < 100 {
		t.Errorf("only %d records held in %d bytes", len(held), limit)
	}
	if len(m.recs) > 4096+2*len(held) {
		t.Errorf("the slice grew to %d for %d records held: compaction isn't working", len(m.recs), len(held))
	}
	// A record bigger than the whole ring doesn't stay.
	rec, js := traceTestRec("ffffff0000000000", 2*limit)
	m.Add(rec, js, limit)
	if _, n := m.Find("ffffff"); n != 0 || m.bytes > limit {
		t.Errorf("an oversize record stayed in the ring (n=%d, bytes %d)", n, m.bytes)
	}
	// trace-memory off keeps nothing.
	m2 := newTraceMem()
	rec, js = traceTestRec("aaaaaa0000000001", 0)
	m2.Add(rec, js, 0)
	if st := m2.Stats(); st.Records != 0 || st.Bytes != 0 {
		t.Errorf("with a zero limit the ring holds %+v", st)
	}
}

func TestTraceCleanPrefix(t *testing.T) {
	for _, in := range []string{"", "abc", "abcde", "abcdeg", "0123456789abcdef0", "7f3a 9c"} {
		if p, err := cleanPrefix(in); err == nil {
			t.Errorf("cleanPrefix(%q) = %q, want an error", in, p)
		}
	}
	for in, want := range map[string]string{"7f3a9c": "7f3a9c", " 7F3A9C\n": "7f3a9c", "7f3a9c0d12e4b5a6": "7f3a9c0d12e4b5a6"} {
		if p, err := cleanPrefix(in); err != nil || p != want {
			t.Errorf("cleanPrefix(%q) = %q, %v; want %q", in, p, err, want)
		}
	}
}

func TestTraceRecordsGoToMemoryAndTheLogFile(t *testing.T) {
	f := newFixture(t, "", false)
	var ids []string
	for _, p := range []string{"/", "/about/", "/nope/", "/healthz"} {
		ids = append(ids, f.do("GET", p).Header().Get("BareProxy-Id"))
	}
	data, err := os.ReadFile(f.log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != len(ids) {
		t.Fatalf("%d log lines, want %d", len(lines), len(ids))
	}
	for i, id := range ids {
		js, n := f.rt.Mem.Find(id[:6])
		if n != 1 {
			t.Fatalf("request %d (%s): %d records in memory", i, id, n)
		}
		if string(js) != lines[i] {
			t.Errorf("request %d: memory and log differ:\n%s\n%s", i, js, lines[i])
		}
	}
	// A rule is written as it reads in the config: -> and quotes, not \u003e.
	if want := `"rule":"route /healthz -> respond 200 \"ok\""`; !strings.Contains(lines[3], want) {
		t.Errorf("the record lacks %s:\n%s", want, lines[3])
	}
	if strings.Contains(string(data), `\u003e`) {
		t.Errorf("the log escapes > as \\u003e")
	}
}

func TestTraceMemoryLimitFromTheConfigHoldsOnlyTheNewest(t *testing.T) {
	f := newFixture(t, "", false)
	first := f.do("GET", "/").Header().Get("BareProxy-Id")
	js, _ := f.rt.Mem.Find(first[:6])
	f.c.TraceMem = int64(10*len(js) + len(js)/2)
	var last string
	for i := 0; i < 60; i++ {
		last = f.do("GET", "/").Header().Get("BareProxy-Id")
	}
	st := f.rt.Mem.Stats()
	if st.Records < 8 || st.Records > 12 || st.Bytes > f.c.TraceMem {
		t.Errorf("ring holds %d records, %d bytes, for a limit of %d", st.Records, st.Bytes, f.c.TraceMem)
	}
	if _, n := f.rt.Mem.Find(first[:6]); n != 0 {
		t.Errorf("the first request is still in memory")
	}
	if _, n := f.rt.Mem.Find(last[:6]); n != 1 {
		t.Errorf("the last request isn't in memory")
	}
	// The log file keeps all of them, whatever the ring does.
	if recs := f.records(t); len(recs) != 61 {
		t.Errorf("%d records in the log file, want 61", len(recs))
	}
}

func TestTraceReloadKeepsTheRingAndTakesTheNewLimit(t *testing.T) {
	f := newFixture(t, "", false)
	s := NewServer(f.conf, f.rt)
	s.logger.SetOutput(io.Discard)
	id := f.do("GET", "/").Header().Get("BareProxy-Id")
	writeFile(t, f.conf, "global\n  admin off\n  trace-log requests.log\n  trace-memory 64KB\nsite http://example.com:8080\n  route /* -> respond 200 \"v2\"\n")
	s.Reload()
	rt := s.Current()
	defer rt.Stop()
	if rt.Version != 2 || rt.Mem != f.rt.Mem {
		t.Fatalf("version %d; the ring was %s", rt.Version, map[bool]string{true: "kept", false: "replaced"}[rt.Mem == f.rt.Mem])
	}
	if rt.Cfg.TraceMem != 64<<10 {
		t.Errorf("trace-memory is %d, want 65536", rt.Cfg.TraceMem)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "example.com:8080"
	s.Handler(8080, false).ServeHTTP(rr, req)
	if _, n := rt.Mem.Find(id[:6]); n != 1 {
		t.Errorf("the request from before the reload is gone")
	}
	if _, n := rt.Mem.Find(rr.Header().Get("BareProxy-Id")[:6]); n != 1 {
		t.Errorf("the request after the reload isn't in the ring")
	}
	if st := rt.Mem.Stats(); st.Limit != 64<<10 {
		t.Errorf("the ring's limit is %d after the reload", st.Limit)
	}
}

func TestTraceConfigSettings(t *testing.T) {
	const site = "site http://example.com:8080\n  route /* -> respond 200 \"x\"\n"
	c, probs := Parse("/etc/x/bp.conf", "global\n  admin off\n  trace-log trace.log 10MB 5\n  trace-memory 8MB\n"+site)
	if len(probs) != 0 {
		t.Fatalf("problems: %v", probs)
	}
	if c.TraceLog != "/etc/x/trace.log" || c.TraceSize != 10<<20 || c.TraceCount != 5 || c.TraceMem != 8<<20 {
		t.Errorf("got log %q size %d count %d memory %d", c.TraceLog, c.TraceSize, c.TraceCount, c.TraceMem)
	}
	c, _ = Parse("/etc/x/bp.conf", "global\n  admin off\n"+site)
	if c.TraceMem != 32<<20 || c.TraceSize != 0 || c.TraceLog != "stdout" {
		t.Errorf("defaults are log %q, size %d, memory %d; want stdout, 0, 32MB", c.TraceLog, c.TraceSize, c.TraceMem)
	}
	c, _ = Parse("/etc/x/bp.conf", "global\n  trace-memory off\n"+site)
	if c.TraceMem != 0 {
		t.Errorf("trace-memory off gave %d", c.TraceMem)
	}
	bad := map[string]string{
		"trace-log stdout 10MB 3": "only a trace log file can rotate",
		"trace-log x.log 0 3":     "more than zero",
		"trace-log x.log 10MB 0":  "1 or more",
		"trace-log x.log 10MB":    "trace-log takes",
		"trace-memory lots":       "bad size",
		"trace-memory":            "takes one value",
	}
	for line, frag := range bad {
		_, probs := Parse("/etc/x/bp.conf", "global\n  "+line+"\n"+site)
		found := false
		for _, p := range probs {
			if p.Line == 2 && !p.Warn && strings.Contains(p.Msg, frag) {
				found = true
			}
		}
		if !found {
			t.Errorf("%q: no error on line 2 mentioning %q; got %v", line, frag, probs)
		}
	}
}

func traceReadLines(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func TestTraceLogRotatesAndKeepsTheCount(t *testing.T) {
	file := filepath.Join(t.TempDir(), "trace.log")
	tl, err := OpenTraceLog(file)
	if err != nil {
		t.Fatal(err)
	}
	_, js := traceTestRec(traceTestID(1), 40)
	line := int64(len(js) + 1) // every record is the same length
	tl.SetRotation(10*line, 3) // ten records to a file, three old files kept
	for i := 1; i <= 95; i++ {
		rec, _ := traceTestRec(traceTestID(i), 40)
		tl.Write(rec)
	}
	tl.Close()
	// File.1 is the newest old file: 91-95 are in the live file, 81-90 in .1, 71-80 in .2, 61-70 in .3.
	for name, first := range map[string]int{file: 91, file + ".1": 81, file + ".2": 71, file + ".3": 61} {
		lines := traceReadLines(t, name)
		want := 10
		if name == file {
			want = 5
		}
		if len(lines) != want {
			t.Errorf("%s has %d records, want %d", filepath.Base(name), len(lines), want)
			continue
		}
		for k, l := range lines {
			if !strings.Contains(l, `"id":"`+traceTestID(first+k)+`"`) {
				t.Errorf("%s record %d is %.40s..., want ID %s", filepath.Base(name), k, l, traceTestID(first+k))
			}
		}
		if st, _ := os.Stat(name); st.Size() > 10*line {
			t.Errorf("%s is %d bytes, over the limit %d", filepath.Base(name), st.Size(), 10*line)
		}
	}
	if _, err := os.Stat(file + ".4"); err == nil {
		t.Errorf("a fourth old file was kept")
	}
	// why finds records in the old files, and gives up on the ones rotated out.
	if rec, err := FindRecord(file, traceTestID(65)[:6]); err != nil || rec.ID != traceTestID(65) {
		t.Errorf("record 65 (in .3): %v", err)
	}
	if rec, err := FindRecord(file, traceTestID(95)); err != nil || rec.ID != traceTestID(95) {
		t.Errorf("record 95 (live file): %v", err)
	}
	if _, err := FindRecord(file, traceTestID(5)[:6]); err == nil || !strings.Contains(err.Error(), "no request") {
		t.Errorf("record 5 was rotated out, but FindRecord said %v", err)
	}
}

func TestTraceLogReopenedFileCountsItsSizeAndOffLogsNothing(t *testing.T) {
	file := filepath.Join(t.TempDir(), "trace.log")
	_, js := traceTestRec(traceTestID(1), 40)
	line := int64(len(js) + 1)
	writeFile(t, file, strings.Repeat(string(js)+"\n", 9)) // nine records already there
	tl, err := OpenTraceLog(file)
	if err != nil {
		t.Fatal(err)
	}
	tl.SetRotation(10*line, 1)
	for i := 1; i <= 2; i++ {
		rec, _ := traceTestRec(traceTestID(i), 40)
		tl.Write(rec)
	}
	tl.Close()
	if n := len(traceReadLines(t, file+".1")); n != 10 {
		t.Errorf("the first rotation came after %d records, want 10 (9 from before the restart and 1)", n)
	}
	off, _ := OpenTraceLog("off")
	rec, _ := traceTestRec(traceTestID(1), 0)
	off.Write(rec)
	off.WriteLine([]byte("{}"))
	off.Close()
	var none *TraceLog
	none.Write(rec)
	none.Close()
}

func TestTraceLogRotationUnderConcurrentWrites(t *testing.T) {
	file := filepath.Join(t.TempDir(), "trace.log")
	tl, err := OpenTraceLog(file)
	if err != nil {
		t.Fatal(err)
	}
	const limit = 4096
	tl.SetRotation(limit, 2)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				rec, js := traceTestRec(traceTestID(g*1000+i), 30)
				tl.Write(rec)
				tl.WriteLine(js)
			}
		}()
	}
	wg.Wait()
	tl.Close()
	tl.Write(&Record{ID: traceTestID(1)}) // after Close: dropped, no panic
	for _, name := range []string{file, file + ".1", file + ".2"} {
		fh, err := os.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		if st, _ := fh.Stat(); st.Size() > limit {
			t.Errorf("%s is %d bytes, over %d", filepath.Base(name), st.Size(), limit)
		}
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			var r Record
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				t.Fatalf("%s has a torn line %q: %v", filepath.Base(name), sc.Text(), err)
			}
		}
		fh.Close()
	}
	if _, err := os.Stat(file + ".3"); err == nil {
		t.Errorf("a third old file was kept")
	}
}

func TestTraceparentParsing(t *testing.T) {
	const tid, pid = "0af7651916cd43dd8448eb211c80319c", "b7ad6b7169203331"
	if id, fl, ok := parseTraceparent("00-" + tid + "-" + pid + "-01"); !ok || id != tid || fl != "01" {
		t.Errorf("a valid header: %q %q %v", id, fl, ok)
	}
	if id, fl, ok := parseTraceparent("00-" + tid + "-" + pid + "-00"); !ok || id != tid || fl != "00" {
		t.Errorf("flags 00: %q %q %v", id, fl, ok)
	}
	for _, v := range []string{
		"",
		"garbage",
		"00-00000000000000000000000000000000-" + pid + "-01", // trace ID all zeros
		"00-" + tid + "-0000000000000000-01",                 // parent ID all zeros
		"00-0AF7651916CD43DD8448EB211C80319C-" + pid + "-01", // upper case
		"00-" + tid + "-" + pid + "-0A",                      // upper case flags
		"00-" + tid + "-" + pid,                              // no flags
		"00-" + tid + "-" + pid + "-1",                       // short flags
		"00-" + tid + "-" + pid + "-01-extra",                // too long
		"ff-" + tid + "-" + pid + "-01",                      // version ff
		"01-" + tid + "-" + pid + "-01",                      // a version we don't know
		"00-" + tid[:31] + "-" + pid + "-01",                 // short trace ID
		"00-" + tid + "x-" + pid[:15] + "-01",                // dash in the wrong place
		"00-" + tid[:31] + "g-" + pid + "-01",                // not hex
		" 00-" + tid + "-" + pid + "-01",                     // leading space
		"00_" + tid + "_" + pid + "_01",                      // wrong separators
		"00-" + tid + "-" + pid + "-01\r\n",                  // trailing junk
	} {
		if _, _, ok := parseTraceparent(v); ok {
			t.Errorf("parseTraceparent(%q) accepted it", v)
		}
	}
}

// traceBackend is a backend that sends every request's headers down a channel.
func traceBackend(t *testing.T) (addr string, seen chan http.Header) {
	t.Helper()
	seen = make(chan http.Header, 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), seen
}

func traceNext(t *testing.T, seen chan http.Header) http.Header {
	t.Helper()
	select {
	case h := <-seen:
		return h
	case <-time.After(3 * time.Second):
		t.Fatal("the backend never saw the request")
		return nil
	}
}

func TestTraceparentRecordedAndForwardedWithTheRequestID(t *testing.T) {
	addr, seen := traceBackend(t)
	f := newFixture(t, "  backend "+addr+"\n", false)
	const tid = "0af7651916cd43dd8448eb211c80319c"
	for _, flags := range []string{"01", "00"} {
		in := "00-" + tid + "-b7ad6b7169203331-" + flags
		rr := f.traceDo("/api/x", http.Header{"Traceparent": {in}, "Tracestate": {"vendor=opaque"}})
		id := rr.Header().Get("BareProxy-Id")
		h := traceNext(t, seen)
		if want := "00-" + tid + "-" + id + "-" + flags; h.Get("Traceparent") != want {
			t.Errorf("flags %s: the backend got %q, want %q", flags, h.Get("Traceparent"), want)
		}
		if h.Get("Tracestate") != "vendor=opaque" {
			t.Errorf("tracestate changed to %q", h.Get("Tracestate"))
		}
		if h.Get("BareProxy-Id") != id {
			t.Errorf("BareProxy-Id %q, want %q", h.Get("BareProxy-Id"), id)
		}
		js, n := f.rt.Mem.Find(id[:6])
		var rec Record
		if n != 1 || json.Unmarshal(js, &rec) != nil || rec.TraceID != tid {
			t.Errorf("flags %s: record %s (n=%d), want trace_id %s", flags, js, n, tid)
		}
		if !strings.Contains(string(js), `"trace_id":"`+tid+`"`) {
			t.Errorf("the JSON has no trace_id field: %s", js)
		}
		why := RenderWhy(&rec)
		for _, want := range []string{"W3C trace ID " + tid, "traceparent sent upstream with parent ID " + id} {
			if !strings.Contains(why, want) {
				t.Errorf("why lacks %q:\n%s", want, why)
			}
		}
	}
	// A request nobody sends a traceparent with stays without one: BareProxy starts no traces.
	rr := f.do("GET", "/api/x")
	h := traceNext(t, seen)
	if v := h.Values("Traceparent"); len(v) != 0 {
		t.Errorf("BareProxy made up a traceparent: %v", v)
	}
	if js, _ := f.rt.Mem.Find(rr.Header().Get("BareProxy-Id")[:6]); strings.Contains(string(js), "trace_id") {
		t.Errorf("a record without a traceparent has a trace_id: %s", js)
	}
}

func TestTraceparentInvalidOnesPassUnchangedAndAreNotRecorded(t *testing.T) {
	addr, seen := traceBackend(t)
	f := newFixture(t, "  backend "+addr+"\n", false)
	const tid, pid = "0af7651916cd43dd8448eb211c80319c", "b7ad6b7169203331"
	cases := map[string][]string{
		"zero trace ID": {"00-00000000000000000000000000000000-" + pid + "-01"},
		"zero parent":   {"00-" + tid + "-0000000000000000-01"},
		"upper case":    {"00-" + strings.ToUpper(tid) + "-" + pid + "-01"},
		"no flags":      {"00-" + tid + "-" + pid},
		"version ff":    {"ff-" + tid + "-" + pid + "-01"},
		"garbage":       {"not a traceparent"},
		"empty":         {""},
		"two headers":   {"00-" + tid + "-" + pid + "-01", "00-" + tid + "-" + pid + "-01"},
	}
	for name, vs := range cases {
		rr := f.traceDo("/api/x", http.Header{"Traceparent": vs})
		id := rr.Header().Get("BareProxy-Id")
		h := traceNext(t, seen)
		got := h.Values("Traceparent")
		if strings.Join(got, "|") != strings.Join(vs, "|") {
			t.Errorf("%s: the backend got %q, want it unchanged as %q", name, got, vs)
		}
		if js, n := f.rt.Mem.Find(id[:6]); n != 1 || strings.Contains(string(js), "trace_id") {
			t.Errorf("%s: record %s (n=%d) should have no trace_id", name, js, n)
		}
	}
}

func TestTraceparentOnARuleThatDoesNotProxyIsStillRecorded(t *testing.T) {
	f := newFixture(t, "", false)
	const tid = "0af7651916cd43dd8448eb211c80319c"
	id := f.do("GET", "/about/", "Traceparent", "00-"+tid+"-b7ad6b7169203331-01").Header().Get("BareProxy-Id")
	js, _ := f.rt.Mem.Find(id[:6])
	if !strings.Contains(string(js), `"trace_id":"`+tid+`"`) || !strings.Contains(string(js), `"outcome":"file"`) {
		t.Errorf("a files request lost its trace ID: %s", js)
	}
}

func TestTraceAdminWhyAnswersFromMemory(t *testing.T) {
	f := newFixture(t, "", false)
	s := NewServer(f.conf, f.rt)
	mux := http.NewServeMux()
	for _, add := range adminHandlers {
		add(s, mux)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	get := func(id string) (int, string) {
		resp, err := http.Get(srv.URL + "/why?id=" + id)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	id := f.do("GET", "/about/").Header().Get("BareProxy-Id")
	if code, body := get(id[:6]); code != 200 || !strings.Contains(body, `"id":"`+id+`"`) {
		t.Errorf("a request in memory: %d %s", code, body)
	}
	if code, body := get(strings.ToUpper(id)); code != 200 || !strings.Contains(body, id) {
		t.Errorf("upper case full ID: %d %s", code, body)
	}
	if code, _ := get("abc"); code != 400 {
		t.Errorf("a 3 digit prefix gave %d, want 400", code)
	}
	if code, _ := get("zzzzzz"); code != 400 {
		t.Errorf("a non-hex prefix gave %d, want 400", code)
	}
	if code, _ := get("fedcba"); code != 404 {
		t.Errorf("an unknown ID gave %d, want 404", code)
	}
	for _, i := range []string{"abcdef0000000001", "abcdef0000000002"} {
		rec, js := traceTestRec(i, 0)
		f.rt.Mem.Add(rec, js, 1<<20)
	}
	if code, body := get("abcdef"); code != 409 || !strings.Contains(body, "2 requests") {
		t.Errorf("an ambiguous prefix: %d %q, want 409 naming 2 requests", code, body)
	}
	if code, _ := get("abcdef0000000002"); code != 200 {
		t.Errorf("a longer prefix should settle it, got %d", code)
	}
}

func TestTraceEventWhenABackendGoesDownWithoutHealthChecks(t *testing.T) {
	dead := deadAddr(t)
	f := newFixture(t, "  backend "+dead+"\n", false)
	for i := 0; i < 3; i++ {
		f.do("GET", "/api/x")
	}
	evs := f.rt.Mem.Events()
	if len(evs) != 1 || evs[0].Kind != "backend" || evs[0].Time == "" ||
		!strings.Contains(evs[0].Text, "pool api: "+dead+" down after 3 failed connections (connect refused)") {
		t.Fatalf("events: %+v", evs)
	}
}

func TestTraceEventsFromHealthChecks(t *testing.T) {
	live := echoBackend(t, "live")
	dead := deadAddr(t)
	f := newFixture(t, fmt.Sprintf("  backend %s\n  backend %s\n  health /healthz every 30ms timeout 200ms\n", strings.TrimPrefix(live.URL, "http://"), dead), true)
	has := func(frag string) bool {
		for _, e := range f.rt.Mem.Events() {
			if e.Kind == "backend" && strings.Contains(e.Text, frag) {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !(has("down after 3 failed checks") && has("up (health check passed)")) {
		time.Sleep(20 * time.Millisecond)
	}
	if !has("pool api: "+dead+" down after 3 failed checks (connect refused)") || !has("pool api: "+strings.TrimPrefix(live.URL, "http://")+" up (health check passed)") {
		t.Errorf("events: %+v", f.rt.Mem.Events())
	}
}

func TestTraceEventRingKeepsTheLast1000(t *testing.T) {
	m := newTraceMem()
	for i := 1; i <= 1005; i++ {
		m.Event("test", fmt.Sprint(i))
	}
	evs := m.Events()
	if len(evs) != 1000 || evs[0].Text != "6" || evs[999].Text != "1005" || evs[0].Kind != "test" {
		t.Errorf("%d events, first %q, last %q", len(evs), evs[0].Text, evs[len(evs)-1].Text)
	}
	f := newFixture(t, "", false)
	s := NewServer(f.conf, f.rt)
	s.Event("apply", "version 2 applied")
	if evs := f.rt.Mem.Events(); len(evs) != 1 || evs[0].Kind != "apply" || evs[0].Text != "version 2 applied" {
		t.Errorf("Server.Event: %+v", evs)
	}
}

// TestTraceRingCapacityAtTheDefaultSize measures what the design note
// estimates: how many typical records the default 32MB ring holds, and what
// that costs in heap. It logs the numbers (run with -v) and checks they are
// in a sane range.
func TestTraceRingCapacityAtTheDefaultSize(t *testing.T) {
	addr, _ := traceBackend(t)
	f := newFixture(t, "  backend "+addr+"\n", false)
	// A mix of what the proxy sees: backend requests (with a traceparent on
	// some), files, a miss, a local answer.
	f.do("GET", "/api/orders?page=2", "Traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	f.do("GET", "/api/orders/42")
	f.do("GET", "/about/", "Accept-Encoding", "gzip")
	f.do("GET", "/app.js", "Accept-Encoding", "gzip, br")
	f.do("GET", "/nope/")
	f.do("GET", "/healthz")
	var samples [][]byte
	var avg float64
	for _, l := range traceReadLines(t, f.log) {
		samples = append(samples, []byte(l))
		avg += float64(len(l))
	}
	avg /= float64(len(samples))

	const limit = 32 << 20
	m := newTraceMem()
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	for i := 0; m.Stats().Bytes < limit-1024 || i < 1000; i++ {
		js := append([]byte(nil), samples[i%len(samples)]...)
		rec := &Record{Status: 200, Outcome: "ok"}
		rec.ID = fmt.Sprintf("%016x", i)
		m.Add(rec, js, limit)
	}
	runtime.GC()
	runtime.ReadMemStats(&m1)
	st := m.Stats()
	heap := int64(m1.HeapAlloc) - int64(m0.HeapAlloc)
	t.Logf("average record: %.0f bytes of JSON (%d samples)", avg, len(samples))
	t.Logf("a %d MB ring holds %d records (%d bytes of JSON)", limit>>20, st.Records, st.Bytes)
	t.Logf("heap used by the full ring: %.1f MB, %.0f bytes per record (%.2fx the JSON)",
		float64(heap)/(1<<20), float64(heap)/float64(st.Records), float64(heap)/float64(st.Bytes))
	if st.Records < 30000 || st.Records > 200000 {
		t.Errorf("%d records in 32MB is outside the range this test expects", st.Records)
	}
	runtime.KeepAlive(m)
}

func TestTraceFilterParsing(t *testing.T) {
	good := map[string]TailFilter{
		"status>=500": {Key: "status", Op: ">=", Val: "500", n: 500},
		"status<=399": {Key: "status", Op: "<=", Val: "399", n: 399},
		"status>499":  {Key: "status", Op: ">", Val: "499", n: 499},
		"status<300":  {Key: "status", Op: "<", Val: "300", n: 300},
		"status=404":  {Key: "status", Op: "=", Val: "404", n: 404},
		"status!=200": {Key: "status", Op: "!=", Val: "200", n: 200},
		"pool=api":    {Key: "pool", Op: "=", Val: "api"},
		"path!=/ping": {Key: "path", Op: "!=", Val: "/ping"},
		"path=/a=b":   {Key: "path", Op: "=", Val: "/a=b"},
	}
	for in, want := range good {
		if got, err := ParseTailFilter(in); err != nil || got != want {
			t.Errorf("ParseTailFilter(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	bad := map[string]string{
		"":             "needs a key",
		"status":       "needs a key",
		"=500":         "needs a key",
		"status>=":     "status must be a number",
		"status>=abc":  "status must be a number",
		"status=99":    "status must be a number",
		"status=600":   "status must be a number",
		"pool>api":     "takes = or !=",
		"method>=GET":  "takes = or !=",
		"colour=red":   "unknown key",
		"pool=":        "needs an operator and a value",
		"pool!":        "takes = or !=",
		"site~example": "needs a key",
	}
	for in, frag := range bad {
		if _, err := ParseTailFilter(in); err == nil || !strings.Contains(err.Error(), frag) {
			t.Errorf("ParseTailFilter(%q): error %v, want one mentioning %q", in, err, frag)
		}
	}
}

func TestTraceFilterMatching(t *testing.T) {
	rec := &Record{Status: 502, Pool: "api", Site: "example.com", Host: "Example.com:8443", Outcome: "connect_failed",
		Method: "GET", Path: "/api/%2e%2e/admin/users", NormPath: "/admin/users"}
	cases := map[string]bool{
		"status>=500": true, "status>=503": false, "status>501": true, "status>502": false,
		"status<=502": true, "status<502": false, "status<599": true, "status=502": true, "status=404": false,
		"status!=404": true, "status!=502": false,
		"pool=api": true, "pool=web": false, "pool!=web": true, "pool!=api": false,
		"site=example.com": true, "site=EXAMPLE.COM": true, "site=other.org": false,
		"host=example.com": true, "host=example.com:8443": true, "host=EXAMPLE.com": true, "host=other.org": false,
		"outcome=connect_failed": true, "outcome=ok": false, "outcome!=ok": true,
		"method=GET": true, "method=get": true, "method=POST": false,
		"path=/api": true, "path=/admin": true, "path=/api/%2e": true, "path=/other": false, "path!=/admin": false, "path!=/other": true,
	}
	for in, want := range cases {
		f, err := ParseTailFilter(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := f.Match(rec); got != want {
			t.Errorf("%s on the 502 record: %v, want %v", in, got, want)
		}
	}
	// Filters are ANDed.
	f1, _ := ParseTailFilter("status>=500")
	f2, _ := ParseTailFilter("pool=web")
	f3, _ := ParseTailFilter("pool=api")
	if MatchAll([]TailFilter{f1, f2}, rec) || !MatchAll([]TailFilter{f1, f3}, rec) || !MatchAll(nil, rec) {
		t.Errorf("MatchAll doesn't AND the filters")
	}
}

func traceAdmin(t *testing.T, f *fixture) (*Server, *httptest.Server) {
	t.Helper()
	s := NewServer(f.conf, f.rt)
	mux := http.NewServeMux()
	for _, add := range adminHandlers {
		add(s, mux)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, srv
}

func TestTraceTailStreamsOnlyWhatMatches(t *testing.T) {
	f := newFixture(t, "  backend "+deadAddr(t)+"\n", false)
	_, srv := traceAdmin(t, f)
	resp, err := http.Get(srv.URL + "/tail?f=status%3E%3D500&f=path%3D/api")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	f.mustServe(t, "/")          // 200 from files: no
	a := f.do("GET", "/api/one") // 502 from the dead backend: yes
	f.do("GET", "/healthz")      // 200 local: no
	b := f.do("GET", "/api/two") // 502: yes
	f.do("GET", "/nope/")        // 404: no
	var got []string
	for len(got) < 2 {
		select {
		case l := <-lines:
			got = append(got, l)
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d lines arrived: %q", len(got), got)
		}
	}
	for i, want := range []string{a.Header().Get("BareProxy-Id"), b.Header().Get("BareProxy-Id")} {
		var rec Record
		if err := json.Unmarshal([]byte(got[i]), &rec); err != nil || rec.ID != want || rec.Status != 502 {
			t.Errorf("line %d: %q, want the 502 with ID %s", i, got[i], want)
		}
	}
	select {
	case l := <-lines:
		t.Errorf("an extra line came through: %q", l)
	case <-time.After(150 * time.Millisecond):
	}
	// A reader that goes away is dropped from the feed.
	resp.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.rt.Mem.mu.Lock()
		n := len(f.rt.Mem.subs)
		f.rt.Mem.mu.Unlock()
		if n == 0 {
			return
		}
		f.do("GET", "/api/three") // a write is what shows the server the reader is gone
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("the tail is still subscribed after the reader left")
}

func (f *fixture) mustServe(t *testing.T, target string) {
	t.Helper()
	if rr := f.do("GET", target); rr.Code != 200 {
		t.Fatalf("GET %s: %d", target, rr.Code)
	}
}

func TestTailRefusesABadFilter(t *testing.T) {
	f := newFixture(t, "", false)
	_, srv := traceAdmin(t, f)
	resp, err := http.Get(srv.URL + "/tail?f=colour%3Dred")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 400 || !strings.Contains(string(b), "unknown key") {
		t.Errorf("got %d %q, want 400 naming the key", resp.StatusCode, b)
	}
}

// slowWriter is a response writer whose first write waits at a gate, to be a slow reader.
type slowWriter struct {
	mu   sync.Mutex
	out  strings.Builder
	gate chan struct{}
	once sync.Once
}

func (w *slowWriter) Header() http.Header { return http.Header{} }
func (w *slowWriter) WriteHeader(int)     {}
func (w *slowWriter) Flush()              {}
func (w *slowWriter) Write(b []byte) (int, error) {
	w.once.Do(func() { <-w.gate })
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.Write(b)
}
func (w *slowWriter) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.String()
}

func TestTraceTailSaysHowManyRecordsItSkipped(t *testing.T) {
	f := newFixture(t, "", false)
	s := NewServer(f.conf, f.rt)
	ctx, cancel := context.WithCancel(context.Background())
	w := &slowWriter{gate: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		s.adminTail(w, httptest.NewRequest("GET", "/tail", nil).WithContext(ctx))
		close(done)
	}()
	for { // wait for the subscription
		f.rt.Mem.mu.Lock()
		n := len(f.rt.Mem.subs)
		f.rt.Mem.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	const total = 700 // the first is stuck in the slow write; the feed holds 256; the rest are skipped
	for i := 1; i <= total; i++ {
		rec, js := traceTestRec(traceTestID(i), 0)
		f.rt.Mem.Add(rec, js, 1<<20)
	}
	close(w.gate)
	var delivered, skipped int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		delivered, skipped = 0, 0
		for _, l := range strings.Split(strings.TrimSpace(w.text()), "\n") {
			var d struct{ Dropped int }
			switch {
			case strings.HasPrefix(l, `{"dropped":`):
				json.Unmarshal([]byte(l), &d)
				skipped += d.Dropped
			case strings.HasPrefix(l, `{"id":`):
				delivered++
			}
		}
		if delivered+skipped == total {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if delivered+skipped != total || skipped < total-300 || delivered < 200 {
		t.Errorf("%d delivered and %d skipped of %d; every record must be one or the other", delivered, skipped, total)
	}
}

// traceTestCert writes a self-signed certificate and its key into dir.
func traceTestCert(t *testing.T, dir, cn string, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter, DNSNames: []string{cn}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	writeFile(t, dir+"/cert.pem", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	writeFile(t, dir+"/key.pem", string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
}

func TestTraceStatusShape(t *testing.T) {
	dir := t.TempDir()
	traceTestCert(t, dir, "example.com", time.Now().Add(90*24*time.Hour+time.Hour))
	conf := dir + "/bareproxy.conf"
	writeFile(t, conf, `global
  admin off
site https://example.com:8443
  tls cert.pem key.pem
  route /* -> respond 200 "secure"
site http://example.com:8080
  route /api/* -> api
  route /* -> respond 200 "plain"
pool api
  backend 127.0.0.1:1
  backend 127.0.0.1:2
`)
	c, probs := Load(conf)
	if HasErrors(probs) {
		t.Fatal(probs)
	}
	rt, err := NewRuntime(c, nil, 7, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(conf, rt)
	for i := 0; i < 3; i++ {
		rt.Pools["api"].Backends[1].connFailed("connect refused")
	}
	// Requests: a 200, a proxy error, and an application 500 (not a proxy error), one of them 2 minutes old.
	for _, r := range []struct {
		status  int
		outcome string
	}{{200, "ok"}, {502, "connect_failed"}, {500, "ok"}} {
		rec, js := traceTestRec(traceTestID(r.status), 0)
		rec.Status, rec.Outcome = r.status, r.outcome
		rt.Mem.Add(rec, js, 1<<20)
	}
	rt.Mem.recs[rt.Mem.start].at = time.Now().Add(-2 * time.Minute).UnixNano()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.Status()) }))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	for _, k := range []string{"version", "config_file", "config_version", "started", "uptime_seconds", "listeners", "sites", "pools", "certificates", "requests"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("the status JSON has no %q: %s", k, raw)
		}
	}
	reqs := doc["requests"].(map[string]any)
	for _, k := range []string{"last_1m", "last_5m", "ring"} {
		if _, ok := reqs[k]; !ok {
			t.Errorf("requests has no %q", k)
		}
	}
	var st Status
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.ConfigFile != conf || st.ConfigVersion != 7 || st.Version != Version || st.UptimeSeconds < 0 || st.Started == "" {
		t.Errorf("header: %+v", st)
	}
	if len(st.Listeners) != 2 || st.Listeners[0].Port != 8080 || st.Listeners[0].TLS || st.Listeners[1].Port != 8443 || !st.Listeners[1].TLS ||
		strings.Join(st.Listeners[1].Sites, ",") != "example.com" {
		t.Errorf("listeners: %+v", st.Listeners)
	}
	if len(st.Sites) != 2 || st.Sites[1].Name != "example.com" || st.Sites[1].Rules != 2 || st.Sites[1].Line != 6 {
		t.Errorf("sites: %+v", st.Sites)
	}
	if len(st.Certificates) != 1 || st.Certificates[0].Site != "example.com" || st.Certificates[0].Subject != "CN=example.com" ||
		st.Certificates[0].DaysLeft != 90 || st.Certificates[0].NotAfter == "" {
		t.Errorf("certificates: %+v", st.Certificates)
	}
	if len(st.Pools) != 1 || st.Pools[0].Name != "api" || st.Pools[0].Up != 1 || st.Pools[0].Size != 2 || len(st.Pools[0].Backends) != 2 {
		t.Fatalf("pools: %+v", st.Pools)
	}
	if b := st.Pools[0].Backends; b[0].State != "up" || b[1].State != "down" || b[1].Failures != 3 || b[1].Reason != "connect refused" || b[1].Since == "" {
		t.Errorf("backends: %+v", b)
	}
	if r := st.Requests; r.Last1m.Requests != 2 || r.Last1m.Status5xx != 2 || r.Last1m.ProxyErrors != 1 || !r.Last1m.Complete ||
		r.Last5m.Requests != 3 || r.Last5m.Status5xx != 2 || r.Last5m.ProxyErrors != 1 || r.Ring.Records != 3 || r.Ring.Limit != 1<<20 || r.Ring.Oldest == "" {
		t.Errorf("requests: %+v", r)
	}
}

func TestTraceRateSaysWhenTheRingHasWrapped(t *testing.T) {
	m := newTraceMem()
	_, one := traceTestRec(traceTestID(1), 0)
	for i := 1; i <= 100; i++ {
		rec, js := traceTestRec(traceTestID(i), 0)
		m.Add(rec, js, int64(10*len(one)))
	}
	if w, complete := m.Recent(time.Minute); w.Requests != 10 || complete {
		t.Errorf("a wrapped ring: %+v complete=%v; want 10 requests and incomplete", w, complete)
	}
	m.recs[m.start].at = time.Now().Add(-10 * time.Minute).UnixNano() // the ring does reach back past the window
	if w, complete := m.Recent(time.Minute); w.Requests != 9 || !complete {
		t.Errorf("a ring that reaches back: %+v complete=%v; want 9 requests and complete", w, complete)
	}
	if w, complete := newTraceMem().Recent(time.Minute); w.Requests != 0 || !complete {
		t.Errorf("an empty ring that never wrapped: %+v complete=%v", w, complete)
	}
}

func TestTraceCertificateEvents(t *testing.T) {
	dir := t.TempDir()
	traceTestCert(t, dir, "example.com", time.Now().Add(30*24*time.Hour+time.Hour))
	conf := dir + "/bareproxy.conf"
	writeFile(t, conf, "global\n  admin off\nsite https://example.com:8443\n  tls cert.pem key.pem\n  route /* -> respond 200 \"x\"\n")
	c, probs := Load(conf)
	if HasErrors(probs) {
		t.Fatal(probs)
	}
	rt, err := NewRuntime(c, nil, 1, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	rt.certEvents(nil)
	evs := rt.Mem.Events()
	if len(evs) != 1 || evs[0].Kind != "certificate" || !strings.Contains(evs[0].Text, "example.com: certificate for CN=example.com loaded, ends ") ||
		!strings.Contains(evs[0].Text, "(30 days left)") {
		t.Fatalf("events: %+v", evs)
	}
	rt.certEvents(rt) // the same certificate again is nothing new
	if n := len(rt.Mem.Events()); n != 1 {
		t.Errorf("an unchanged certificate made another event (%d)", n)
	}
	traceTestCert(t, dir, "example.com", time.Now().Add(60*24*time.Hour+time.Hour)) // a renewed one on disk
	c2, _ := Load(conf)
	rt2, _ := NewRuntime(c2, rt, 2, nil, false)
	rt2.certEvents(rt)
	if evs := rt2.Mem.Events(); len(evs) != 2 || !strings.Contains(evs[1].Text, "(60 days left)") {
		t.Errorf("a renewed certificate should give a new event: %+v", evs)
	}
}

func TestTraceReloadEvents(t *testing.T) {
	f := newFixture(t, "", false)
	s := NewServer(f.conf, f.rt)
	s.logger.SetOutput(io.Discard)
	writeFile(t, f.conf, "site http://example.com:8080\n  route /* -> nopool\n")
	s.Reload()
	writeFile(t, f.conf, "global\n  admin off\n  trace-log requests.log\nsite http://example.com:8080\n  route /* -> respond 200 \"v2\"\n")
	s.Reload()
	defer s.Current().Stop()
	evs := s.Current().Mem.Events()
	if len(evs) != 2 || evs[0].Kind != "reload" || evs[1].Kind != "reload" ||
		!strings.Contains(evs[0].Text, "reload failed (line 2: error: no pool named nopool), so version 1 keeps running") ||
		!strings.HasPrefix(evs[1].Text, "version 2 running: 1 site") {
		t.Errorf("events: %+v", evs)
	}
}

func TestTraceEventsEndpoint(t *testing.T) {
	f := newFixture(t, "", false)
	_, srv := traceAdmin(t, f)
	get := func() []Event {
		resp, err := http.Get(srv.URL + "/events")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var evs []Event
		b, _ := io.ReadAll(resp.Body)
		if err := json.Unmarshal(b, &evs); err != nil || evs == nil {
			t.Fatalf("%q: %v (an empty list must be [], not null)", b, err)
		}
		return evs
	}
	if evs := get(); len(evs) != 0 {
		t.Errorf("a new server has events: %+v", evs)
	}
	f.rt.Mem.Event("apply", "version 2 applied by uid 1000")
	if evs := get(); len(evs) != 1 || evs[0].Kind != "apply" || evs[0].Text != "version 2 applied by uid 1000" {
		t.Errorf("events: %+v", evs)
	}
}

func TestTraceReloadAppliesNewRotationToTheOpenLog(t *testing.T) {
	f := newFixture(t, "", false)
	s := NewServer(f.conf, f.rt)
	s.logger.SetOutput(io.Discard)
	writeFile(t, f.conf, "global\n  admin off\n  trace-log requests.log 1KB 2\nsite http://example.com:8080\n  route /* -> respond 200 \"v2\"\n")
	s.Reload()
	rt := s.Current()
	defer rt.Stop()
	if rt.Version != 2 || rt.Trace != f.rt.Trace {
		t.Fatalf("version %d; the log file was reopened instead of kept", rt.Version)
	}
	h := s.Handler(8080, false)
	for i := 0; i < 40; i++ {
		req := httptest.NewRequest("GET", fmt.Sprintf("/page%d", i), nil)
		req.Host = "example.com:8080"
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	for _, name := range []string{"requests.log", "requests.log.1", "requests.log.2"} {
		st, err := os.Stat(filepath.Join(f.dir, name))
		if err != nil || st.Size() > 1024 || st.Size() == 0 {
			t.Errorf("%s: %v (size %v), want a file of 1 to 1024 bytes", name, err, st)
		}
	}
	if _, err := os.Stat(filepath.Join(f.dir, "requests.log.3")); err == nil {
		t.Errorf("a third old file was kept")
	}
	// The newest request is in the live file, and the ring still has all 40.
	last := traceReadLines(t, filepath.Join(f.dir, "requests.log"))
	if len(last) == 0 || !strings.Contains(last[len(last)-1], `"path":"/page39"`) {
		t.Errorf("the live file doesn't end with the newest request: %q", last)
	}
	if st := rt.Mem.Stats(); st.Records != 40 {
		t.Errorf("the ring holds %d records, want 40", st.Records)
	}
}

func TestTraceRecordCostsNothingWhenNothingReadsIt(t *testing.T) {
	rt := &Runtime{Cfg: &Config{TraceMem: 0}, Mem: newTraceMem()}
	rt.Trace, _ = OpenTraceLog("off")
	rec := traceBenchRecord()
	if n := testing.AllocsPerRun(100, func() { rt.record(rec) }); n != 0 {
		t.Errorf("with the log off and no ring, record allocates %v times a request; it should do nothing", n)
	}
	tl := rt.Mem.Subscribe() // someone tailing makes it worth encoding
	rt.record(rec)
	select {
	case it := <-tl.C:
		if it.rec != rec || len(it.js) == 0 {
			t.Errorf("the tail got %+v", it)
		}
	default:
		t.Errorf("a tail got nothing while the log and ring were off")
	}
	rt.Mem.Unsubscribe(tl)
	rt.Mem.Unsubscribe(tl) // twice is harmless
	if n := rt.Mem.tailers.Load(); n != 0 {
		t.Errorf("%d tailers counted after both left", n)
	}
	if n := testing.AllocsPerRun(100, func() { rt.record(rec) }); n != 0 {
		t.Errorf("after the tail left, record allocates %v times again", n)
	}
	rt.Cfg.TraceMem = 1 << 20 // the ring on
	rt.record(rec)
	if st := rt.Mem.Stats(); st.Records != 1 {
		t.Errorf("with the ring on, %d records held", st.Records)
	}
}
