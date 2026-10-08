#!/bin/bash
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# Smoke test for a release binary: run it the way a new user would.
#
#   usage: live/smoke.sh BINARY [PORT]
#
# It prints the version, checks a small config, asks explain about it offline,
# starts the server on 127.0.0.1:PORT (18080 by default), fetches a page and a
# missing page, asks status over the admin socket, and stops the server. Any
# step that fails ends the test with a non-zero exit. Needs curl.
set -euo pipefail

bin=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
port=${2:-18080}
work=$(mktemp -d)
trap 'kill "$pid" 2>/dev/null || true; wait 2>/dev/null || true; rm -rf "$work"' EXIT
pid=
cd "$work"

fail() { echo "smoke: $*" >&2; [ -f run/bareproxy.log ] && cat run/bareproxy.log >&2; exit 1; }

"$bin" version | grep -q '^bareproxy ' || fail "version printed nothing"
"$bin" version

mkdir -p public run
echo '<h1>smoke ok</h1>' >public/index.html
cat >smoke.conf <<EOF
global
  admin run/admin.sock
  state run/state
  trace-log run/requests.log

site http://localhost:$port
  route /* -> files public
EOF

"$bin" check smoke.conf || fail "check refused the config"
"$bin" explain --offline -c smoke.conf GET "http://localhost:$port/" | tee run/explain.txt
grep -q 'public' run/explain.txt || fail "explain didn't name the files folder"

"$bin" run smoke.conf 2>run/bareproxy.log &
pid=$!
for _ in $(seq 50); do
  curl -fsS "http://127.0.0.1:$port/" -H "Host: localhost:$port" -o run/page.html 2>/dev/null && break
  kill -0 "$pid" 2>/dev/null || fail "the server stopped"
  sleep 0.2
done
grep -q 'smoke ok' run/page.html || fail "the page didn't come back"

code=$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/missing" -H "Host: localhost:$port")
[ "$code" = 404 ] || fail "a missing page gave $code, not 404"

"$bin" status -c smoke.conf | tee run/status.txt
grep -q 'localhost' run/status.txt || fail "status didn't list the site"

kill "$pid"
wait "$pid" 2>/dev/null || true
pid=
echo "smoke test passed"
