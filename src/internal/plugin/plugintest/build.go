// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

// Package plugintest builds the test plugin in ../testdata/fixture, for the
// tests of the plugin host and of the server.
package plugintest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
)

var (
	once  sync.Once
	built []byte
	err   error
)

// Fixture builds the test plugin with the Go toolchain that runs the tests,
// once per test binary, and returns the .wasm file.
func Fixture() ([]byte, error) {
	once.Do(func() {
		_, here, _, _ := runtime.Caller(0)
		src := filepath.Join(filepath.Dir(here), "..", "testdata", "fixture", "main.go")
		dir, e := os.MkdirTemp("", "bp-fixture")
		if e != nil {
			err = e
			return
		}
		defer os.RemoveAll(dir)
		out := filepath.Join(dir, "fixture.wasm")
		gobin := filepath.Join(runtime.GOROOT(), "bin", "go")
		if _, e := os.Stat(gobin); e != nil {
			gobin = "go"
		}
		cmd := exec.Command(gobin, "build", "-buildmode=c-shared", "-o", out, src)
		cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
		cmd.Dir = filepath.Dir(src)
		if b, e := cmd.CombinedOutput(); e != nil {
			err = fmt.Errorf("building the test plugin: %v\n%s", e, b)
			return
		}
		built, err = os.ReadFile(out)
	})
	return built, err
}

// RustFixture reads the Rust test plugin, plugins/testplugin built by
// plugins/build.sh, from the file BP_RUST_FIXTURE names. It returns nil when
// the variable isn't set, so tests can skip it on a machine without Rust;
// CI sets it.
func RustFixture() ([]byte, error) {
	f := os.Getenv("BP_RUST_FIXTURE")
	if f == "" {
		return nil, nil
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return nil, fmt.Errorf("BP_RUST_FIXTURE: %v", err)
	}
	return b, nil
}
