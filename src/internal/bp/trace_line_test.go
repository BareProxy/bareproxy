// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

// Write writes a record as one JSON line, as the server does; only tests use
// it outside Runtime.record.
func (t *TraceLog) Write(rec *Record) {
	if t == nil {
		return
	}
	if b, err := RecordJSON(rec); err == nil {
		t.WriteLine(b)
	}
}
