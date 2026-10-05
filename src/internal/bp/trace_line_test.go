// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"bytes"
	"math"
	"math/rand/v2"
	"reflect"
	"testing"
)

// checkLine fails unless recordLine gives exactly what RecordJSON gives.
func checkLine(t *testing.T, rec *Record) {
	t.Helper()
	got, err1 := recordLine(rec)
	want, err2 := RecordJSON(rec)
	if (err1 == nil) != (err2 == nil) || !bytes.Equal(got, want) {
		t.Fatalf("recordLine differs from encoding/json:\n got %s (%v)\nwant %s (%v)", got, err1, want, err2)
	}
}

func TestRecordLineMatchesEncodingJSON(t *testing.T) {
	odd := []string{"", "plain", `quote " and \ backslash`, "tab\tnew\nline\r", "<b>&amp;</b>", "café \U0001F600",
		"bad \xff\xfe utf-8", "line sep ", "del \x7f nul \x00", "/api/orders?x=1&y=<2>", "route /api/* -> api strip"}
	floats := []float64{0, 0.001, 0.4, 36.9, 12345.678, 1e20, 1e21, 1e-7, math.Copysign(0, -1), math.NaN(), math.Inf(1)}
	checkLine(t, &Record{})
	checkLine(t, traceBenchRecord())
	checkLine(t, &Record{Skipped: []Skip{}, Attempts: []Attempt{}, Checked: []string{}})
	for _, f := range floats {
		checkLine(t, &Record{MS: f, Attempts: []Attempt{{MS: f, FirstByteMS: f}}})
	}
	// Random records: every field empty or set, from the values above, so
	// a field added to Record without a json tag the encoder knows shows up.
	rnd := rand.New(rand.NewPCG(1, 2))
	var fill func(v reflect.Value)
	fill = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Struct:
			for i := range v.NumField() {
				if rnd.IntN(3) > 0 {
					fill(v.Field(i))
				}
			}
		case reflect.Slice:
			n := rnd.IntN(3)
			v.Set(reflect.MakeSlice(v.Type(), n, n))
			for i := range n {
				fill(v.Index(i))
			}
		case reflect.String:
			v.SetString(odd[rnd.IntN(len(odd))])
		case reflect.Int, reflect.Int64:
			v.SetInt(rnd.Int64N(100000) - 10)
		case reflect.Float64:
			v.SetFloat(floats[rnd.IntN(len(floats)-2)]) // no NaN or Inf: those are checked above
		default:
			t.Fatalf("Record has a %s field: teach recordLine's test (and maybe recordLine) about it", v.Kind())
		}
	}
	for range 3000 {
		var rec Record
		fill(reflect.ValueOf(&rec).Elem())
		checkLine(t, &rec)
	}
}

func BenchmarkTraceRecordLine(b *testing.B) {
	rec := traceBenchRecord()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		recordLine(rec)
	}
}
