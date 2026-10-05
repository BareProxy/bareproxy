// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DefaultStateDir holds the config history unless the global setting state
// names another folder.
var DefaultStateDir = "/var/lib/bareproxy"

// keepVersions is how many config versions the history keeps.
const keepVersions = 100

// Entry is one config version in the history.
type Entry struct {
	Version int    `json:"version"`
	Time    string `json:"time"`
	How     string `json:"how"` // startup, apply, rollback or reload
	User    string `json:"user,omitempty"`
	Plan    string `json:"plan,omitempty"`
	From    int    `json:"from,omitempty"` // the version a rollback restored
}

// history keeps config versions in the state dir: versions/N.conf holds
// version N, and history.jsonl has one entry per line, oldest first.
type history struct {
	dir     string
	entries []Entry
}

func stateDir(c *Config) string {
	if c != nil && c.State != "" {
		return c.State
	}
	return DefaultStateDir
}

func openHistory(dir string) (*history, error) {
	if err := os.MkdirAll(filepath.Join(dir, "versions"), 0o750); err != nil {
		return nil, err
	}
	h := &history{dir: dir}
	data, err := os.ReadFile(filepath.Join(dir, "history.jsonl"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		var e Entry
		if json.Unmarshal([]byte(line), &e) == nil && e.Version > 0 {
			h.entries = append(h.entries, e)
		}
	}
	return h, nil
}

func (h *history) path(n int) string {
	return filepath.Join(h.dir, "versions", fmt.Sprintf("%d.conf", n))
}

// last returns the newest version number, or 0 when the history is empty.
func (h *history) last() int {
	if len(h.entries) == 0 {
		return 0
	}
	return h.entries[len(h.entries)-1].Version
}

// text returns the config text of version n.
func (h *history) text(n int) (string, error) {
	for _, e := range h.entries {
		if e.Version == n {
			b, err := os.ReadFile(h.path(n))
			return string(b), err
		}
	}
	return "", fmt.Errorf("version %d isn't in the history (it keeps the last %d)", n, keepVersions)
}

// before returns the version that went live before version n, or 0.
func (h *history) before(n int) int {
	for i, e := range h.entries {
		if e.Version == n && i > 0 {
			return h.entries[i-1].Version
		}
	}
	return 0
}

// save stores a new version and drops the oldest past the last 100.
func (h *history) save(e Entry, text string) error {
	if err := writeFileAtomic(h.path(e.Version), []byte(text), 0o640); err != nil {
		return err
	}
	h.entries = append(h.entries, e)
	for len(h.entries) > keepVersions {
		os.Remove(h.path(h.entries[0].Version))
		h.entries = h.entries[1:]
	}
	var b strings.Builder
	for _, e := range h.entries {
		line, _ := json.Marshal(e)
		b.Write(line)
		b.WriteByte('\n')
	}
	return writeFileAtomic(filepath.Join(h.dir, "history.jsonl"), []byte(b.String()), 0o640)
}

// writeFileAtomic writes a file through a temporary file and a rename, so
// readers see the old content or the new, never a mix.
func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = f.Chmod(perm)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
