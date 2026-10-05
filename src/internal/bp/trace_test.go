// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
