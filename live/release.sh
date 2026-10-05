#!/bin/bash
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# Build the BareProxy release files: a static binary for linux/amd64 and for
# linux/arm64, each packed as bareproxy-VERSION-linux-ARCH.tar.gz, and a
# SHA256SUMS file that lists the two tarballs.
#
#   usage: live/release.sh [DIR]        DIR defaults to dist/ at the repo root
#
# VERSION is the Version const in src/internal/bp/server.go. Each tarball holds
# one folder with the same name as the tarball, and in it bareproxy, README.md,
# LICENSE and NOTICE. The binaries are built with CGO_ENABLED=0, -trimpath and
# -ldflags="-s -w". Needs Go and GNU tar on the PATH. Only Go's standard library
# is used, so nothing is downloaded. If git can't read the repo (a folder owned
# by another user, say), set GOFLAGS=-buildvcs=false.
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

# File times inside the tarballs: SOURCE_DATE_EPOCH, else the last commit, else now.
epoch=${SOURCE_DATE_EPOCH:-$(git -C "$root" log -1 --format=%ct 2>/dev/null || date +%s)}

echo "BareProxy $version, $(go version)"
echo "output: $out"

for arch in amd64 arm64; do
  name=bareproxy-$version-linux-$arch
  stage=$out/$name
  rm -rf "$stage" "$out/$name.tar.gz"
  mkdir -p "$stage"

  (cd "$src" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch \
    go build -trimpath -ldflags="-s -w" -o "$stage/bareproxy" ./cmd/bareproxy)
  cp "$root/README.md" "$root/LICENSE" "$root/NOTICE" "$stage/"
  chmod 0755 "$stage" "$stage/bareproxy"
  chmod 0644 "$stage/README.md" "$stage/LICENSE" "$stage/NOTICE"

  tar -C "$out" --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$epoch" -cf - "$name" |
    gzip -9 -n >"$out/$name.tar.gz"

  bin_bytes=$(wc -c <"$stage/bareproxy" | tr -d ' ')
  tgz_bytes=$(wc -c <"$out/$name.tar.gz" | tr -d ' ')
  bin_mb=$(awk -v n="$bin_bytes" 'BEGIN { printf "%.1f", n / 1000000 }')
  tgz_mb=$(awk -v n="$tgz_bytes" 'BEGIN { printf "%.1f", n / 1000000 }')
  echo "linux/$arch: bareproxy $bin_bytes bytes ($bin_mb MB), $name.tar.gz $tgz_bytes bytes ($tgz_mb MB)"
  rm -rf "$stage"
done

(cd "$out" && sha256sum "bareproxy-$version-linux-amd64.tar.gz" "bareproxy-$version-linux-arm64.tar.gz" >SHA256SUMS)
echo
echo "$out/SHA256SUMS"
cat "$out/SHA256SUMS"
