#!/usr/bin/env bash
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# Builds what bench.sh needs: BareProxy (plain and stripped), the test API
# backend, and the bareproxy.com Hugo site. The Go source is read only here:
# nothing is edited, committed or written in the source tree.
#
#   ./build.sh            build everything
#   SRC=/other/src ./build.sh   build from another copy of the source
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC="${SRC:-$(cd "$(dirname "$0")/../../src" && pwd)}"
SITE_SRC="${SITE_SRC:?set SITE_SRC to the bareproxy.com site source}"
HUGO="${HUGO:-hugo}"

export GOTOOLCHAIN=local GOFLAGS=-buildvcs=false

mkdir -p "$HERE/bin" "$HERE/site"
cd "$SRC"
echo "go: $(go version)"
nice -n 10 go build -o "$HERE/bin/bareproxy" ./cmd/bareproxy
nice -n 10 go build -trimpath -ldflags="-s -w" -o "$HERE/bin/bareproxy-stripped" ./cmd/bareproxy
nice -n 10 go build -trimpath -ldflags="-s -w" -o "$HERE/bin/testapi" ./tools/testapi
ls -l "$HERE/bin"
{
  echo "git $(git -C "$SRC/.." log -1 --format='%h %s' 2>/dev/null || echo unknown); uncommitted files: $(git -C "$SRC/.." status --short 2>/dev/null | wc -l); built $(date '+%Y-%m-%d %H:%M:%S')"
} > "$HERE/bin/build-info.txt"
cat "$HERE/bin/build-info.txt"

"$HUGO" --source "$SITE_SRC" --destination "$HERE/site/public" --cleanDestinationDir --quiet
echo "site: $(find "$HERE/site/public" -type f | wc -l) files, $(du -sk "$HERE/site/public" | cut -f1) KB"
