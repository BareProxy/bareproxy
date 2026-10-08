#!/bin/bash
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# Build every plugin in this workspace to WebAssembly, into DIR (dist/ by
# default) as NAME.wasm, with a SHA256SUMS file.
#
#   usage: plugins/build.sh [DIR]
#
# Needs Rust (cargo) with the wasm32-unknown-unknown target:
#   rustup target add wasm32-unknown-unknown
# The crates are vendored in vendor/, so nothing is downloaded. To update
# them: change Cargo.toml, then cargo update && cargo vendor --versioned-dirs.
#
# The target is wasm32-unknown-unknown rather than wasm32-wasip1: a plugin
# needs nothing from WASI, the Proxy-Wasm host gives it everything it uses,
# so its module imports only the host's functions and stays small.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
out=${1:-$here/dist}
mkdir -p "$out"
out=$(cd "$out" && pwd)
target=wasm32-unknown-unknown

cd "$here"
cargo build --release --target "$target" --locked
names=()
for f in target/$target/release/*.wasm; do
  name=$(basename "$f")
  cp "$f" "$out/$name"
  names+=("$name")
  echo "$name: $(wc -c <"$f" | tr -d ' ') bytes"
done
(cd "$out" && sha256sum "${names[@]}" >SHA256SUMS)
