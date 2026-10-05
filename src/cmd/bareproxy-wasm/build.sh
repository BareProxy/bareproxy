#!/bin/sh
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# Build the BareProxy browser demo: bareproxy.wasm and the page that runs it.
#
#   usage: build.sh [DIST]        DIST defaults to /home/claude/out/demo-dist
#
# Needs Go on the PATH. Only the standard library is used, so nothing is
# downloaded. The result is a folder of static files; serve it with any web
# server that sends .wasm as application/wasm.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
src=$(cd "$here/../.." && pwd) # the folder with go.mod
dist=${1:-/home/claude/out/demo-dist}

mkdir -p "$dist"
rm -f "$dist/bareproxy.wasm" "$dist/wasm_exec.js"

cd "$src"
GOOS=js GOARCH=wasm go build -trimpath -ldflags="-s -w" -o "$dist/bareproxy.wasm" ./cmd/bareproxy-wasm
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" "$dist/wasm_exec.js"
cp "$here/web/index.html" "$here/web/demo.css" "$here/web/demo.js" "$dist/"

raw=$(wc -c <"$dist/bareproxy.wasm")
gz=$(gzip -9 -c "$dist/bareproxy.wasm" | wc -c)
echo "built $dist with $(go version | cut -d' ' -f3)"
echo "bareproxy.wasm: $raw bytes raw, $gz bytes gzipped (gzip -9)"
