// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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

// pinsPath holds the exact plugin files version n ran, when it ran any.
func (h *history) pinsPath(n int) string {
	return filepath.Join(h.dir, "versions", fmt.Sprintf("%d.plugins.json", n))
}

// pins returns the plugin files version n ran.
func (h *history) pins(n int) map[string]Pin {
	var pins map[string]Pin
	if b, err := os.ReadFile(h.pinsPath(n)); err == nil {
		json.Unmarshal(b, &pins)
	}
	return pins
}

// last returns the newest version number, or 0 when the history is empty or
// isn't kept.
func (h *history) last() int {
	if h == nil || len(h.entries) == 0 {
		return 0
	}
	return h.entries[len(h.entries)-1].Version
}

// text returns the config text of version n.
func (h *history) text(n int) (string, error) {
	if !slices.ContainsFunc(h.entries, func(e Entry) bool { return e.Version == n }) {
		return "", fmt.Errorf("version %d isn't in the history (it keeps the last %d)", n, keepVersions)
	}
	b, err := os.ReadFile(h.path(n))
	return string(b), err
}

// before returns the version that went live before version n, or 0.
func (h *history) before(n int) int {
	i := slices.IndexFunc(h.entries, func(e Entry) bool { return e.Version == n })
	if i < 1 {
		return 0
	}
	return h.entries[i-1].Version
}

// save stores a new version and drops the oldest past the last 100. A
// version that runs plugins also keeps which plugin files it ran; the files
// themselves are kept in the state folder while a kept version names them.
func (h *history) save(e Entry, text string, pins map[string]Pin) error {
	if err := writeFileAtomic(h.path(e.Version), []byte(text), 0o640); err != nil {
		return err
	}
	if len(pins) > 0 {
		b, _ := json.MarshalIndent(pins, "", "  ")
		if err := writeFileAtomic(h.pinsPath(e.Version), b, 0o640); err != nil {
			return err
		}
	}
	h.entries = append(h.entries, e)
	dropped := false
	for len(h.entries) > keepVersions {
		os.Remove(h.path(h.entries[0].Version))
		os.Remove(h.pinsPath(h.entries[0].Version))
		h.entries = h.entries[1:]
		dropped = true
	}
	if dropped {
		h.dropPluginFiles()
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	for _, e := range h.entries {
		enc.Encode(e)
	}
	return writeFileAtomic(filepath.Join(h.dir, "history.jsonl"), b.Bytes(), 0o640)
}

// writeFileAtomic writes a file through a temporary file and a rename, so
// readers see the old content or the new, never a mix.
func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
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
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// dropPluginFiles removes the plugin files no kept version names.
func (h *history) dropPluginFiles() {
	dir := filepath.Join(h.dir, "plugins", "files")
	files, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	keep := map[string]bool{}
	for _, e := range h.entries {
		for _, p := range h.pins(e.Version) {
			keep[p.SHA], keep[p.ConfigSHA] = true, true
		}
	}
	for _, f := range files {
		if !keep[f.Name()] {
			os.Remove(filepath.Join(dir, f.Name()))
		}
	}
}
