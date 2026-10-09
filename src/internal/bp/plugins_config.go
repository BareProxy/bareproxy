// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"bareproxy/internal/plugin"
)

// PluginSpec is one plugin block: a WebAssembly file and its settings.
type PluginSpec struct {
	Line       int
	Name       string
	File       string
	SHA        string // of the .wasm file; "" when not read (NoDisk)
	ConfigFile string
	ConfigSHA  string
	Config     []byte
	Memory     int64
	Timeout    time.Duration
	Pause      time.Duration
	Instances  int
	OnError    string // closed or open
	AllowHTTP  []string
	Read       []string
	readRoots  []*os.Root
	Store      int64 // 0: no store
	BodyLimit  int64
	// BodyRequest and BodyResponse say which bodies the plugin is handed.
	// Proxy-Wasm SDKs export every callback, so the module can't tell.
	BodyRequest, BodyResponse bool
	Module                    *plugin.Module
	Pinned                    bool   // read from the history's copy, not from File
	wasm                      []byte // the file, for the history's copy
}

// Pin is the exact plugin file and config file a history version ran.
type Pin struct {
	File       string `json:"file"`
	SHA        string `json:"sha256"`
	ConfigFile string `json:"config_file,omitempty"`
	ConfigSHA  string `json:"config_sha256,omitempty"`
}

// key says everything that makes a running plugin what it is: two specs with
// the same key can share one running plugin across an apply.
func (ps *PluginSpec) key() string {
	return fmt.Sprintf("%s|%s|%d|%s|%s|%d|%v|%v|%d|%d|%v|%v", ps.SHA, ps.ConfigSHA, ps.Memory, ps.Timeout, ps.Pause,
		ps.Instances, ps.AllowHTTP, ps.Read, ps.Store, ps.BodyLimit, ps.BodyRequest, ps.BodyResponse)
}

func (p *parser) newPlugin(ln int, w []string) *PluginSpec {
	ps := &PluginSpec{Line: ln, Memory: 64 << 20, Timeout: 5 * time.Millisecond, Pause: 30 * time.Second,
		Instances: min(runtime.GOMAXPROCS(0), 4), OnError: "closed", BodyLimit: 8 << 20}
	if len(w) != 3 || !validName(w[1]) {
		p.errf(ln, "plugin takes a name (letters, digits, - and _) and a .wasm file, such as plugin crawlers /etc/bareproxy/plugins/crawlers.wasm")
		return ps
	}
	ps.Name, ps.File = w[1], p.path(w[2])
	if old := p.c.Plugins[ps.Name]; old != nil {
		p.errf(ln, "plugin %s is already defined on line %d", ps.Name, old.Line)
		return ps
	}
	p.c.Plugins[ps.Name] = ps
	p.c.PluginOrder = append(p.c.PluginOrder, ps.Name)
	return ps
}

func (p *parser) pluginSetting(ps *PluginSpec, ln int, w []string) error {
	one := func() error {
		if len(w) != 2 {
			return fmt.Errorf("%s takes one value", w[0])
		}
		return nil
	}
	dur := func(dst *time.Duration) error {
		if err := one(); err != nil {
			return err
		}
		d, err := time.ParseDuration(w[1])
		if err != nil || d <= 0 {
			return fmt.Errorf("bad duration %q: use a number with ms, s or m", w[1])
		}
		*dst = d
		return nil
	}
	size := func(dst *int64, least int64) error {
		if err := one(); err != nil {
			return err
		}
		n, err := parseSize(w[1])
		if err != nil {
			return err
		}
		if n < least {
			return fmt.Errorf("%s must be at least %s", w[0], fmtBytes(least))
		}
		*dst = n
		return nil
	}
	switch w[0] {
	case "config":
		if err := one(); err != nil {
			return err
		}
		ps.ConfigFile = p.path(w[1])
	case "memory":
		if err := size(&ps.Memory, 1<<20); err != nil {
			return err
		}
		if ps.Memory > 4<<30 {
			return errors.New("memory can be at most 4GB, the most a WebAssembly module can address")
		}
	case "timeout":
		return dur(&ps.Timeout)
	case "pause":
		return dur(&ps.Pause)
	case "instances":
		n, err := strconv.Atoi(w[min(1, len(w)-1)])
		if len(w) != 2 || err != nil || n < 1 || n > 64 {
			return errors.New("instances takes a number from 1 to 64")
		}
		ps.Instances = n
	case "on-error":
		if len(w) != 2 || (w[1] != "open" && w[1] != "closed") {
			return errors.New("on-error takes open (the request goes on without the plugin) or closed (it gets 502)")
		}
		ps.OnError = w[1]
	case "allow-http":
		if len(w) < 2 {
			return errors.New("allow-http takes one or more host:port addresses the plugin may call")
		}
		for _, a := range w[1:] {
			if _, port, err := net.SplitHostPort(a); err != nil || port == "" {
				return fmt.Errorf("%q isn't host:port", a)
			}
			ps.AllowHTTP = append(ps.AllowHTTP, a)
		}
	case "read":
		if err := one(); err != nil {
			return err
		}
		ps.Read = append(ps.Read, p.path(w[1]))
	case "store":
		return size(&ps.Store, 1<<10)
	case "body-limit":
		return size(&ps.BodyLimit, 0)
	case "body":
		if len(w) < 2 {
			return errors.New("body takes request, response or both: the bodies the plugin is handed, whole")
		}
		for _, b := range w[1:] {
			switch b {
			case "request":
				ps.BodyRequest = true
			case "response":
				ps.BodyResponse = true
			default:
				return fmt.Errorf("body takes request and response, not %q", b)
			}
		}
	default:
		return fmt.Errorf("unknown plugin setting %q", w[0])
	}
	return nil
}

// loadPlugins reads, checks and compiles each plugin file, and opens the
// folders plugins may read. A pinned plugin (a history version's) is read
// from the copy in the state folder.
func (p *parser) loadPlugins() {
	c := p.c
	used := map[string]bool{}
	for _, s := range c.Sites {
		for _, u := range s.Uses {
			used[u.Name] = true
		}
	}
	for _, name := range c.PluginOrder {
		ps := c.Plugins[name]
		if !used[name] {
			p.warnf(ps.Line, "plugin %s isn't used by any site", name)
		}
		if p.opt.NoDisk {
			continue
		}
		pin, pinned := p.opt.Pins[name]
		pinned = pinned && pin.File == ps.File && pin.ConfigFile == ps.ConfigFile
		read := func(file, sha string) ([]byte, error) {
			if !pinned || sha == "" {
				return os.ReadFile(file)
			}
			b, err := os.ReadFile(filepath.Join(blobDir(c), sha))
			if err != nil || sha256Hex(b) != sha {
				return nil, fmt.Errorf("the history's copy of %s (sha256 %.12s) is missing or damaged, so the version can't run as it did", file, sha)
			}
			ps.Pinned = true
			return b, nil
		}
		wasm, err := read(ps.File, pin.SHA)
		if err != nil {
			p.errf(ps.Line, "can't read plugin %s: %v", name, unwrapPathErr(err))
			continue
		}
		if ps.ConfigFile != "" {
			if ps.Config, err = read(ps.ConfigFile, pin.ConfigSHA); err != nil {
				p.errf(ps.Line, "can't read the config of plugin %s: %v", name, unwrapPathErr(err))
				continue
			}
			ps.ConfigSHA = sha256Hex(ps.Config)
		}
		for _, dir := range ps.Read {
			root, err := os.OpenRoot(dir)
			if err != nil {
				p.errf(ps.Line, "plugin %s can't read folder %s: %v", name, dir, unwrapPathErr(err))
				continue
			}
			c.roots = append(c.roots, root)
			ps.readRoots = append(ps.readRoots, root)
		}
		m, err := plugin.Compile(wasm, ps.Memory)
		if err != nil {
			p.errf(ps.Line, "plugin %s (%s): %v", name, ps.File, err)
			continue
		}
		ps.Module, ps.SHA, ps.wasm = m, m.SHA, wasm
		// A trial start, so check and plan see a config the plugin refuses,
		// with the plugin's reason. It has the plugin's folders, so a file
		// the plugin loads at the start (a redirect list, say) is checked
		// too, but no store and no outgoing calls: a plugin leaves those
		// for after proxy_on_configure.
		var said []string
		ts := plugin.Settings{Name: name, Config: ps.Config, Timeout: max(ps.Timeout, time.Second),
			Instances: 1, BodyMax: ps.BodyLimit, Logf: func(f string, a ...any) { said = append(said, fmt.Sprintf(f, a...)) }}
		for _, dir := range ps.Read { // its own handles, which Close closes
			if root, err := os.OpenRoot(dir); err == nil {
				ts.Read = append(ts.Read, root)
			}
		}
		tp, err := plugin.Start(m, ts)
		if err != nil {
			prefix := "plugin " + name + ": "
			for i, l := range said {
				said[i] = strings.TrimPrefix(l, prefix)
			}
			if len(said) > 0 && strings.Contains(err.Error(), "refused its config") {
				p.errf(ps.Line, "plugin %s refused its config %s: %s", name, cmp.Or(ps.ConfigFile, "(none)"), strings.Join(said, "; "))
			} else if len(said) > 0 {
				p.errf(ps.Line, "plugin %s didn't start: %v (%s)", name, err, strings.Join(said, "; "))
			} else {
				p.errf(ps.Line, "plugin %s didn't start: %v", name, err)
			}
			continue
		}
		tp.Close()
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// blobDir holds a copy of each plugin file and plugin config file that a
// kept history version ran, named by its SHA-256.
func blobDir(c *Config) string { return filepath.Join(stateDir(c), "plugins", "files") }

// Pins returns the exact files each plugin of a config runs, for the history.
func (c *Config) Pins() map[string]Pin {
	out := map[string]Pin{}
	for name, ps := range c.Plugins {
		if ps.SHA != "" {
			out[name] = Pin{ps.File, ps.SHA, ps.ConfigFile, ps.ConfigSHA}
		}
	}
	return out
}

// pluginPrint sums up the plugin files a config runs, so a config whose text
// is the same but whose plugin files changed counts as changed.
func pluginPrint(c *Config) string {
	if c == nil {
		return ""
	}
	return pinsPrint(c.Pins())
}

func pinsPrint(pins map[string]Pin) string {
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(pins)) {
		fmt.Fprintf(&b, "%s=%s/%s;", name, pins[name].SHA, pins[name].ConfigSHA)
	}
	return b.String()
}

// savePluginFiles copies the plugin files and plugin config files a config
// runs into the state folder, named by their SHA-256, for rollbacks.
func savePluginFiles(c *Config) error {
	dir := blobDir(c)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	for _, ps := range c.Plugins {
		for sha, data := range map[string][]byte{ps.SHA: ps.wasm, ps.ConfigSHA: ps.Config} {
			if sha == "" || data == nil {
				continue
			}
			f := filepath.Join(dir, sha)
			if _, err := os.Stat(f); err == nil {
				continue
			}
			if err := writeFileAtomic(f, data, 0o640); err != nil {
				return err
			}
		}
	}
	return nil
}

// pluginChanges lists, for plan, the plugins whose file or config file
// changed while their config lines stayed the same.
func pluginChanges(old, new *Config) []string {
	var out []string
	short := func(s string) string { return s[:min(12, len(s))] }
	for _, name := range new.PluginOrder {
		o, n := old.Plugins[name], new.Plugins[name]
		if o == nil || o.File != n.File || o.SHA == "" || n.SHA == "" {
			continue
		}
		if o.SHA != n.SHA {
			out = append(out, fmt.Sprintf("plugin %s: %s changed, sha256 %s -> %s (line %d)", name, n.File, short(o.SHA), short(n.SHA), n.Line))
		}
		if o.ConfigFile == n.ConfigFile && o.ConfigSHA != n.ConfigSHA {
			out = append(out, fmt.Sprintf("plugin %s: config %s changed (line %d)", name, n.ConfigFile, n.Line))
		}
	}
	for _, s := range new.Sites {
		for _, os := range old.Sites {
			if len(s.Addrs) > 0 && len(os.Addrs) > 0 && s.Addrs[0].Text == os.Addrs[0].Text && !sameUses(s.Uses, os.Uses) {
				out = append(out, fmt.Sprintf("site %s: plugins run in the order %s (was %s)", s.Addrs[0].Text, useNames(s.Uses), useNames(os.Uses)))
			}
		}
	}
	return out
}

func sameUses(a, b []PluginUse) bool {
	return slices.EqualFunc(a, b, func(x, y PluginUse) bool { return x.Name == y.Name })
}

func useNames(us []PluginUse) string {
	if len(us) == 0 {
		return "none"
	}
	var n []string
	for _, u := range us {
		n = append(n, u.Name)
	}
	return strings.Join(n, ", ")
}
