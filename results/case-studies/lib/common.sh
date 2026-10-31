# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# common.sh: shared by the case study scripts. Sourced, not run.
#
# BP is the bareproxy binary under test: $BP if set, else the v0.16.0 Linux
# amd64 release, downloaded once into the work folder and checked against
# SHA256SUMS from the same release.

set -u
CS=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
LIB=$CS/lib
VERSION=${VERSION:-v0.16.0}
export NO_PROXY=localhost,127.0.0.1,127.0.0.2 no_proxy=localhost,127.0.0.1,127.0.0.2

getbp() {
	if [ -n "${BP:-}" ]; then return; fi
	local d=$CS/.bin/$VERSION
	if [ ! -x "$d/bareproxy" ]; then
		mkdir -p "$d"
		local base=https://github.com/BareProxy/bareproxy/releases/download/$VERSION
		curl -fsSL -o "$d/bp.tgz" "$base/bareproxy_linux_amd64.tar.gz"
		curl -fsSL -o "$d/SHA256SUMS" "$base/SHA256SUMS"
		(cd "$d" && grep bareproxy_linux_amd64.tar.gz SHA256SUMS | sed 's/bareproxy_linux_amd64.tar.gz/bp.tgz/' | sha256sum -c -)
		tar -xzf "$d/bp.tgz" -C "$d" --strip-components=1 bareproxy_linux_amd64/bareproxy
	fi
	BP=$d/bareproxy
}

freeport() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }

waitport() { # PORT: wait until something listens
	for _ in $(seq 100); do
		(exec 3<>/dev/tcp/127.0.0.1/"$1") 2>/dev/null && return 0
		sleep 0.1
	done
	echo "nothing listens on $1" >&2
	return 1
}

say() { printf '\n## %s\n\n' "$*"; }
show() { printf '$ %s\n' "$*"; "$@"; local rc=$?; [ $rc -ne 0 ] && printf '(exit code %d)\n' $rc; return 0; }
