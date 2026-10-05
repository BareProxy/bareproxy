// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"encoding/json"
	"testing"
)

func traceBenchRecord() *Record {
	return &Record{ID: "7f3a9c0d12e4b5a6", Time: "2026-10-01T14:03:22.418Z", Config: 12, Client: "203.0.113.7", TLS: "1.3", SNI: "example.com",
		Proto: "HTTP/2.0", TraceID: "0af7651916cd43dd8448eb211c80319c", Method: "GET", Scheme: "https", Host: "example.com", Path: "/api/orders",
		Site: "example.com", SiteLine: 5, Line: 8, Rule: "route /api/* -> api strip", Upstream: "/orders", Pool: "api", PoolLine: 14, PoolUp: 2, PoolSize: 3,
		Skipped:  []Skip{{Backend: "10.0.0.13:8080", State: "down", Why: "down since 14:02:10, 3 failed checks (connect refused)"}},
		Attempts: []Attempt{{Backend: "10.0.0.11:8080", Error: "connect refused", MS: 0.4}, {Backend: "10.0.0.12:8080", Status: 200, FirstByteMS: 36.9}},
		Status:   200, BytesOut: 5120, MS: 38.2, Outcome: "ok"}
}

func BenchmarkTraceJSONMarshal(b *testing.B) {
	rec := traceBenchRecord()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		json.Marshal(rec)
	}
}

func BenchmarkTraceRecordJSON(b *testing.B) {
	rec := traceBenchRecord()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		RecordJSON(rec)
	}
}

// BenchmarkTraceRecordToRingAndLog is the whole cost of one finished request:
// the JSON, the ring (32MB, so it is always evicting) and a log that is off.
func BenchmarkTraceRecordToRingAndLog(b *testing.B) {
	rt := &Runtime{Cfg: &Config{TraceMem: 32 << 20}, Mem: newTraceMem()}
	rt.Trace, _ = OpenTraceLog("off")
	rec := traceBenchRecord()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rt.record(rec)
	}
}
