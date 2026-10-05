// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// With no history kept (a server made by NewServer keeps none), the history
// endpoint says so in text, and gives an empty list in JSON.
func TestAdminHistoryWithoutHistory(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "bareproxy.conf")
	c, probs := Parse(conf, "global\n  admin off\n  trace-log off\n\nsite http://example.com:8080\n  route /* -> respond 200 \"ok\"\n")
	if HasErrors(probs) {
		t.Fatal(probs)
	}
	rt, err := NewRuntime(c, nil, 3, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Stop(); c.Close() })
	s := NewServer(conf, rt)
	for _, tc := range []struct{ query, ctype, body string }{
		{"", "text/plain; charset=utf-8", "No config history is kept: the state folder couldn't be used (see the log).\n"},
		{"?json=1", "application/json", "{\n  \"running\": 3,\n  \"versions\": []\n}\n"},
	} {
		w := httptest.NewRecorder()
		s.adminHistory(w, httptest.NewRequest("GET", "/history"+tc.query, nil))
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != tc.ctype || w.Body.String() != tc.body {
			t.Errorf("history%s: %d %q %q, want 200 %q %q", tc.query, w.Code, w.Header().Get("Content-Type"), w.Body.String(), tc.ctype, tc.body)
		}
	}
}
