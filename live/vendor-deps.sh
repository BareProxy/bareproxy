#!/bin/bash
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# vendor-deps.sh fetches the outside dependencies, golang.org/x/crypto (for
# acme/autocert) and github.com/tetratelabs/wazero (the WebAssembly runtime
# for plugins), and the modules they need, then writes go.mod, go.sum and
# src/vendor/ so the repo builds with no network.
#
# Why it looks like this: proxy.golang.org isn't reachable from the build
# machine, so the modules come from GitHub: the Go team's mirror of the Go
# repositories (github.com/golang/<name>) and wazero's own repository, at
# their release tags. The script packs each one
# as a standard module zip (files under <module>@<version>/, no .git, no
# nested modules), serves them from a local file proxy and runs go get with
# GONOSUMDB=golang.org/x, so go.sum is computed from those zips. Once the
# module proxy is reachable, check them against the checksum database:
#   cd src && GOFLAGS=-mod=mod go mod verify
#
# Run it from anywhere: live/vendor-deps.sh
set -euo pipefail

src=$(cd "$(dirname "$0")/../src" && pwd)
work=$(mktemp -d)
trap 'chmod -R u+w "$work"; rm -rf "$work"' EXIT
proxy=$work/proxy

# module, GitHub repository, tag, and what the build needs: the whole module,
# or its go.mod only (term is in the module graph, but no package of it is used).
deps='golang.org/x/crypto golang/crypto v0.57.0 full
golang.org/x/net golang/net v0.58.0 full
golang.org/x/text golang/text v0.42.0 full
golang.org/x/sys golang/sys v0.48.0 full
golang.org/x/term golang/term v0.46.0 mod
github.com/tetratelabs/wazero tetratelabs/wazero v1.12.0 full'

while read -r mod gh ver kind; do
	name=${gh#*/}
	out=$proxy/$mod/@v
	mkdir -p "$out"
	repo=$work/git/$name
	if [ "$kind" = full ]; then
		git -c advice.detachedHead=false clone -q --depth 1 --branch "$ver" "https://github.com/$gh" "$repo"
		cp "$repo/go.mod" "$out/$ver.mod"
		python3 - "$repo" "$mod@$ver" "$out/$ver.zip" <<'PY'
import os, sys, zipfile
src, prefix, out = sys.argv[1:]
with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as z:
    for root, dirs, files in os.walk(src):
        # Version control folders and nested modules (a folder with its own
        # go.mod) are not part of the module, as in golang.org/x/mod/zip.
        dirs[:] = sorted(d for d in dirs if d not in (".git", ".hg", ".svn", ".bzr")
                         and not os.path.exists(os.path.join(root, d, "go.mod")))
        for f in sorted(files):
            p = os.path.join(root, f)
            if os.path.islink(p) or not os.path.isfile(p):
                continue
            if os.path.basename(root) == "vendor":
                sys.exit("unexpected vendor folder in " + root)
            z.write(p, prefix + "/" + os.path.relpath(p, src))
PY
	else
		git clone -q --depth 1 --branch "$ver" --filter=blob:none --no-checkout "https://github.com/$gh" "$repo"
		git -C "$repo" show HEAD:go.mod >"$out/$ver.mod"
	fi
	when=$(TZ=UTC git -C "$repo" log -1 --date=format-local:%Y-%m-%dT%H:%M:%SZ --format=%cd)
	printf '{"Version":"%s","Time":"%s"}\n' "$ver" "$when" >"$out/$ver.info"
	echo "$ver" >"$out/list"
	echo "$mod $ver ($kind) from github.com/$gh commit $(git -C "$repo" rev-parse HEAD)"
done <<<"$deps"

cd "$src"
export GOPROXY=file://$proxy GONOSUMDB=golang.org/x,github.com/tetratelabs/wazero GOFLAGS="-mod=mod -buildvcs=false" GOTOOLCHAIN=local GOMODCACHE=$work/modcache
go get golang.org/x/crypto/acme/autocert@v0.57.0 github.com/tetratelabs/wazero@v1.12.0
go mod tidy
rm -rf vendor
go mod vendor
echo "go.mod, go.sum and vendor/ written:"
grep -v '^$' go.mod
cat go.sum
