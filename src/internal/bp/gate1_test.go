// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Gate 1 review, M2 and M3: an apply says when it rewrote the config file,
// and a symlinked config stays a symlink with the target rewritten.
func TestApplyRewritesSymlinkTargetAndSaysSo(t *testing.T) {
	l := &liveServer{dir: t.TempDir()}
	l.port = freePort(t)
	real := filepath.Join(l.dir, "real.conf")
	writeFile(t, real, l.text("v1"))
	l.conf = filepath.Join(l.dir, "bareproxy.conf")
	if err := os.Symlink(real, l.conf); err != nil {
		t.Fatal(err)
	}
	s, err := Start(l.conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	l.s = s
	v2 := l.text("v2")
	a := mustApply(t, s, v2)
	l.expect(t, 2, "v2")
	if fi, err := os.Lstat(l.conf); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the config is no longer a symlink: %v %v", fi.Mode(), err)
	}
	if b, _ := os.ReadFile(real); string(b) != v2 {
		t.Errorf("the symlink's target holds %q, want the applied text", b)
	}
	if want := l.conf + " now holds version 2."; a.Wrote != l.conf || !strings.Contains(appliedText(a), want) {
		t.Errorf("apply reply: wrote %q, text %q; want %q", a.Wrote, appliedText(a), want)
	}
	if a := mustApply(t, s, v2); a.Wrote != "" || strings.Contains(appliedText(a), "now holds") {
		t.Errorf("an unchanged apply says it wrote the file: %+v", a)
	}
}

// Gate 1 review, M1: status shows that the config file doesn't hold the
// running config.
func TestStatusShowsConfigMismatch(t *testing.T) {
	l := startLive(t, "v1")
	if m := l.s.Status().Mismatch; m != "" {
		t.Errorf("fresh server: mismatch %q", m)
	}
	writeFile(t, l.conf, strings.Replace(l.text("v1"), `respond 200 "v1"`, "nopool", 1))
	l.s.Reload()
	if m := l.s.Status().Mismatch; !strings.Contains(m, "version 1 keeps running") {
		t.Errorf("status after a broken reload: mismatch %q", m)
	}
}
