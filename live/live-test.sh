#!/usr/bin/env bash
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
# BareProxy live test: serves the bareproxy.com Hugo site over HTTPS and
# HTTP/2, with a test API behind /api/, and prints everything it sees.
# Needs the bareproxy and testapi binaries in ../bin, Hugo (HUGO=path),
# the site source (SITE=path), openssl, curl and gzip.
set -u
cd "$(dirname "$0")"
HUGO=${HUGO:-hugo}
SITE=${SITE:-../../site/bareproxy.com-main}
BP=../bin/bareproxy
API=../bin/testapi
CURL=(curl -sS --noproxy '*' --resolve bareproxy.com:8443:127.0.0.1 --resolve bareproxy.com:8080:127.0.0.1 --cacert certs/cert.pem)
B=https://bareproxy.com:8443
step() { printf '\n## %s\n' "$*"; }
show() { printf '\n$ %s\n' "$*"; "$@"; }
hit() {
  printf '\n$ curl %s\n' "$*"
  "${CURL[@]}" -o run/body -D run/headers "$@" || true
  tr -d '\r' < run/headers | grep -E '^HTTP/' | tail -1
  tr -d '\r' < run/headers | grep -iE '^(location|content-type|content-encoding|content-range|etag|vary|bareproxy-id):' | sed 's/^/  /'
  if grep -q '<title>' run/body 2>/dev/null && grep -qi '^content-type: text/html' run/headers && ! grep -qi '^content-encoding' run/headers; then
    printf '  page title: %s\n' "$(grep -o '<title>[^<]*' run/body | head -1 | sed 's/<title>//')"
  elif grep -qiE '^content-type: (text/plain|application/json)' run/headers; then
    printf '  body: %s\n' "$(head -c 400 run/body | tr '\n' ' ')"
  fi
  LAST_ID=$(tr -d '\r' < run/headers | grep -i '^bareproxy-id:' | tail -1 | cut -d' ' -f2)
}

rm -rf public run certs live.conf live.conf.good
mkdir -p run certs

step "Setup: build the bareproxy.com site with Hugo, precompress, add bait"
"$HUGO" --quiet --source "$SITE" --destination "$PWD/public"
find public \( -name '*.html' -o -name '*.css' \) -exec gzip -k -9 {} \;
mkdir -p public/.git && echo "[core] secret" > public/.git/config
ln -s /etc public/etc-link
ln -s ../../../../etc public/etc-rel
echo "$(find public -type f -not -name '*.gz' | wc -l) files, $(find public -name '*.gz' | wc -l) gzip copies, plus .git/config, etc-link -> /etc, etc-rel -> ../../../../etc"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout certs/key.pem -out certs/cert.pem \
  -days 30 -subj "/CN=bareproxy.com" -addext "subjectAltName=DNS:bareproxy.com" 2>/dev/null
cat > live.conf <<EOF
# BareProxy live test: the bareproxy.com Hugo site plus a test API
global
  admin run/admin.sock
  state run/state
  trace-log run/requests.log

site bareproxy.com:8443 http://bareproxy.com:8080
  tls certs/cert.pem certs/key.pem
  error 404 /404.html
  route /api/* -> api strip
  route /* -> files public

pool api
  backend 127.0.0.1:9081
  backend 127.0.0.1:9082
  backend 127.0.0.1:9083
  health /healthz every 1s
EOF
step "The config (line numbers added for reading)"
cat -n live.conf

"$API" api-1 127.0.0.1:9081 2>run/api-1.log & A1=$!
"$API" api-2 127.0.0.1:9082 2>run/api-2.log & A2=$!
show $BP check live.conf
$BP run live.conf 2>run/bareproxy.log & BPID=$!
trap 'kill $BPID $A1 $A2 2>/dev/null; wait 2>/dev/null' EXIT
sleep 4

step "Static files from the Hugo build, over HTTPS"
hit $B/
hit $B/platform/
hit $B/platform
hit "$B/platform?ref=x"
hit $B/no-such-page/; ID_404=$LAST_ID
hit http://bareproxy.com:8080/modules/

step "Precompressed copies, conditional requests, byte ranges, HEAD"
hit -H 'Accept-Encoding: gzip' $B/platform/; ID_GZ=$LAST_ID
ETAG=$("${CURL[@]}" -o /dev/null -D - $B/platform/ | tr -d '\r' | grep -i '^etag:' | cut -d' ' -f2)
hit -H "If-None-Match: $ETAG" $B/platform/
hit -r 0-99 $B/images/plan-demo.png
hit -I $B/about/

step "Refused: hidden files, symlinks out of the folder, tricky paths, other methods"
hit $B/.git/config
hit $B/etc-link/passwd; ID_ESC=$LAST_ID
hit $B/etc-link/
hit $B/etc-rel/passwd
hit --path-as-is $B/../etc/passwd
hit $B/files/a%2Fb
hit --path-as-is $B/api/%2e%2e/platform/
hit -X POST $B/platform/

step "The API behind /api/: prefix stripped, forwarded headers, request ID passed on"
hit "$B/api/orders?x=1"; ID_API=$LAST_ID
hit "$B/api/orders?x=2"

step "explain, answered by the running server over its admin socket"
show $BP explain --config live.conf GET $B/platform/
show $BP explain --config live.conf -H 'Accept-Encoding: br, gzip' GET $B/platform/
show $BP explain --config live.conf GET $B/no-such-page/
show $BP explain --config live.conf GET $B/etc-link/passwd
show $BP explain --config live.conf GET "$B/api/orders?x=1"

step "why, read back from the trace log"
show $BP why --config live.conf "${ID_GZ:0:8}"
show $BP why --config live.conf "${ID_404:0:8}"
show $BP why --config live.conf "${ID_ESC:0:8}"
show $BP why --config live.conf "${ID_API:0:8}"

step "Stop one API backend: health checks take it out, requests keep working"
kill $A2; sleep 4
show $BP explain --config live.conf GET $B/api/orders
hit $B/api/orders

step "Reload with a broken config: the running version keeps serving"
cp live.conf live.conf.good
sed -i 's|route /api/\* -> api strip|route /api/* -> apix strip|' live.conf
kill -HUP $BPID; sleep 1
tail -3 run/bareproxy.log
hit $B/platform/

step "Reload with a good change: a new rule, version 2"
cp live.conf.good live.conf
sed -i 's|  route /\* -> files public|  route /hello -> respond 200 "hello from version 2"\n  route /* -> files public|' live.conf
kill -HUP $BPID; sleep 1
tail -1 run/bareproxy.log
hit $B/hello

step "Trace log: one JSON record per request"
echo "$(wc -l < run/requests.log) records"
python3 - <<'PY'
import json, collections
recs = [json.loads(l) for l in open("run/requests.log")]
print("outcomes:", dict(collections.Counter(r["outcome"] for r in recs)))
print("protocols:", dict(collections.Counter(r["proto"] for r in recs)))
print("last record:", json.dumps(recs[-1]))
PY

step "BareProxy's own log"
cat run/bareproxy.log
