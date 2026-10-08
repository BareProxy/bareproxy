#!/bin/bash
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# Build the BareProxy release files: a static binary for each system below,
# each packed with README.md, LICENSE and NOTICE in a folder of the same name
# as its archive, and a SHA256SUMS file that lists the archives.
#
#   usage: live/release.sh [DIR]        DIR defaults to dist/ at the repo root
#
#   bareproxy_linux_amd64.tar.gz     Linux, x86-64
#   bareproxy_linux_arm64.tar.gz     Linux, 64-bit ARM
#   bareproxy_darwin_arm64.tar.gz    macOS, Apple silicon
#   bareproxy_darwin_amd64.tar.gz    macOS, Intel
#   bareproxy_windows_amd64.zip      Windows, x86-64
#
# The archive names carry no version, so links to a release's latest download
# stay the same from one release to the next. The version is the Version const
# in src/internal/bp/server.go, and `bareproxy version` prints it.
#
# The binaries are built with CGO_ENABLED=0, -trimpath and -ldflags="-s -w".
# Needs Go, GNU tar and zip on the PATH. The outside modules are vendored, so
# nothing is downloaded. If git can't read the repo (a folder owned by another
# user, say), set GOFLAGS=-buildvcs=false.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/.." && pwd)
src=$root/src
out=${1:-$root/dist}

version=$(sed -n 's/^const Version = "\(.*\)"$/\1/p' "$src/internal/bp/server.go")
if [ -z "$version" ]; then
  echo "release.sh: can't find the Version const in src/internal/bp/server.go" >&2
  exit 1
fi

mkdir -p "$out"
out=$(cd "$out" && pwd)

# File times inside the archives: SOURCE_DATE_EPOCH, else the last commit, else now.
epoch=${SOURCE_DATE_EPOCH:-$(git -C "$root" log -1 --format=%ct 2>/dev/null || date +%s)}

echo "BareProxy $version, $(go version)"
echo "output: $out"

archives=()
for target in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 windows/amd64; do
  os=${target%/*}
  arch=${target#*/}
  name=bareproxy_${os}_${arch}
  exe=bareproxy
  [ "$os" = windows ] && exe=bareproxy.exe
  stage=$out/$name
  rm -rf "$stage" "$out/$name.tar.gz" "$out/$name.zip"
  mkdir -p "$stage"

  (cd "$src" && CGO_ENABLED=0 GOOS=$os GOARCH=$arch \
    go build -trimpath -ldflags="-s -w" -o "$stage/$exe" ./cmd/bareproxy)
  cp "$root/README.md" "$root/LICENSE" "$root/NOTICE" "$stage/"
  chmod 0755 "$stage" "$stage/$exe"
  chmod 0644 "$stage/README.md" "$stage/LICENSE" "$stage/NOTICE"

  if [ "$os" = windows ]; then
    archive=$name.zip
    find "$stage" -exec touch -h -d "@$epoch" {} +
    (cd "$out" && find "$name" | LC_ALL=C sort | zip -q -X -9 "$archive" -@)
  else
    archive=$name.tar.gz
    tar -C "$out" --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$epoch" -cf - "$name" |
      gzip -9 -n >"$out/$archive"
  fi
  archives+=("$archive")

  bin_bytes=$(wc -c <"$stage/$exe" | tr -d ' ')
  arc_bytes=$(wc -c <"$out/$archive" | tr -d ' ')
  echo "$os/$arch: $exe $bin_bytes bytes, $archive $arc_bytes bytes"
  rm -rf "$stage"
done

(cd "$out" && sha256sum "${archives[@]}" >SHA256SUMS)
echo
echo "$out/SHA256SUMS"
cat "$out/SHA256SUMS"
