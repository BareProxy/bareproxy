// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"fmt"
	"testing"
	"time"
)

func TestCounters(t *testing.T) {
	now := time.UnixMilli(60_000 * 1000) // the start of a minute
	cs := counters{now: func() time.Time { return now }}
	step := func(d time.Duration) { now = now.Add(d) }
	check := func(key string, wantCur, wantPrev uint64, wantInto time.Duration) {
		t.Helper()
		cur, prev, into, ok := cs.count(key, time.Minute, 1)
		if !ok || cur != wantCur || prev != wantPrev || into != wantInto {
			t.Errorf("%s: got %d %d %v %v, want %d %d %v", key, cur, prev, into, ok, wantCur, wantPrev, wantInto)
		}
	}
	check("a", 1, 0, 0)
	step(10 * time.Second)
	check("a", 2, 0, 10*time.Second)
	check("b", 1, 0, 10*time.Second)
	step(55 * time.Second) // into the next minute
	check("a", 1, 2, 5*time.Second)
	step(2 * time.Minute) // a gap: both windows gone
	check("a", 1, 0, 5*time.Second)

	// Old counters are swept.
	for i := range 5000 {
		cs.count(fmt.Sprint("k", i), time.Second, 1)
	}
	step(time.Hour)
	for i := range 5000 {
		cs.count("fresh", time.Second, 1)
		_ = i
	}
	if len(cs.m) > 10 {
		t.Errorf("%d counters left after the sweep", len(cs.m))
	}
}
