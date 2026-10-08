// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

// Package plugin is BareProxy's plugin host. It runs WebAssembly plugins
// written to the Proxy-Wasm ABI (versions 0.2.0 and 0.2.1) on wazero, a
// WebAssembly runtime in pure Go, so BareProxy stays one static binary.
//
// A plugin is sandboxed: it gets WASI with no files, no environment and no
// arguments (clocks, random numbers and a log for its output), the Proxy-Wasm
// host functions, and a few of BareProxy's own through
// proxy_call_foreign_function. Anything else it asks for (an outgoing HTTP
// call, a folder to read, a store on disk) the config has to allow.
package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// cache keeps compiled machine code across configs, so applying a config
// that names the same .wasm file again doesn't compile it again.
var cache = wazero.NewCompilationCache()

const pageSize = 64 << 10 // a WebAssembly memory page

// Module is a compiled plugin file. Each module has its own wazero runtime,
// because the memory cap is a runtime setting.
type Module struct {
	SHA     string // hex SHA-256 of the file
	ABI     string // the Proxy-Wasm ABI version it was built for
	refs    atomic.Int32
	rt      wazero.Runtime
	cm      wazero.CompiledModule
	exports map[string]bool
	alloc   string // proxy_on_memory_allocate or malloc
	start   string // _initialize, _start or ""
}

// Compile checks and compiles a plugin file. memory is the most memory the
// plugin may use, in bytes. The module starts with one reference, which
// Release drops.
func Compile(wasm []byte, memory int64) (*Module, error) {
	ctx := context.Background()
	pages := uint32(min(max((memory+pageSize-1)/pageSize, 1), 65536))
	cfg := wazero.NewRuntimeConfig().WithCompilationCache(cache).WithCloseOnContextDone(true).WithMemoryLimitPages(pages)
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	sum := sha256.Sum256(wasm)
	m := &Module{SHA: hex.EncodeToString(sum[:]), rt: rt, exports: map[string]bool{}}
	m.refs.Store(1)
	fail := func(err error) (*Module, error) {
		rt.Close(ctx)
		return nil, err
	}
	cm, err := rt.CompileModule(ctx, wasm)
	if err != nil {
		return fail(fmt.Errorf("not a WebAssembly module BareProxy can run: %v", err))
	}
	m.cm = cm
	for name := range cm.ExportedFunctions() {
		m.exports[name] = true
	}
	switch {
	case m.exports["proxy_abi_version_0_2_1"]:
		m.ABI = "0.2.1"
	case m.exports["proxy_abi_version_0_2_0"]:
		m.ABI = "0.2.0"
	case m.exports["proxy_abi_version_0_1_0"]:
		return fail(errors.New("it's built for Proxy-Wasm ABI 0.1.0; BareProxy runs 0.2.0 and 0.2.1"))
	default:
		return fail(errors.New("it isn't a Proxy-Wasm plugin: it exports no proxy_abi_version_0_2_1 (or 0_2_0)"))
	}
	switch {
	case m.exports["proxy_on_memory_allocate"]:
		m.alloc = "proxy_on_memory_allocate"
	case m.exports["malloc"]:
		m.alloc = "malloc"
	default:
		return fail(errors.New("it exports neither proxy_on_memory_allocate nor malloc, so BareProxy can't hand it data"))
	}
	if !m.exports["proxy_on_context_create"] {
		return fail(errors.New("it doesn't export proxy_on_context_create"))
	}
	for _, s := range []string{"_initialize", "_start"} {
		if m.exports[s] {
			m.start = s
			break
		}
	}
	var missing []string
	for _, f := range cm.ImportedFunctions() {
		mod, name, _ := f.Import()
		switch {
		case mod == "wasi_snapshot_preview1":
		case mod == "env" && hostFuncs[name].fn != nil:
		default:
			missing = append(missing, mod+"."+name)
		}
	}
	if len(missing) > 0 {
		return fail(fmt.Errorf("it imports %s, which BareProxy doesn't provide", strings.Join(missing, ", ")))
	}
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		return fail(err)
	}
	b := rt.NewHostModuleBuilder("env")
	for name, h := range hostFuncs {
		b.NewFunctionBuilder().WithGoModuleFunction(h.goFunc(), h.params, h.results).Export(name)
	}
	if _, err := b.Instantiate(ctx); err != nil {
		return fail(err)
	}
	return m, nil
}

// Has reports whether the module exports a function, such as
// proxy_on_response_body.
func (m *Module) Has(name string) bool { return m.exports[name] }

// Retain adds a reference.
func (m *Module) Retain() *Module {
	m.refs.Add(1)
	return m
}

// Release drops a reference, and closes the module after the last one.
func (m *Module) Release() {
	if m != nil && m.refs.Add(-1) == 0 {
		m.rt.Close(context.Background())
	}
}
