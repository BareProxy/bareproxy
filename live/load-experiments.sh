#!/usr/bin/env bash
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
# Five cases that the 20 applies of load-test.sh stay clear of, run under load
# so the effect can be counted. None of them is part of that test's pass rule.
#   drain    a backend is removed from the pool that carries WebSocket
#            connections: they are closed when the pool's drain time ends
#   newpool  an apply adds a pool and points a route at it at once
#   twostep  the same change in two applies: add the pool, then point the route
#   health   an apply changes a pool's health line
#   swap     an apply replaces a pool's only backend
# Usage: live/load-experiments.sh [MODE]   (no MODE runs all five, one after another)
# Needs Go (to build), curl and python3. Each run takes about 30 seconds.
set -u
SELF=$(readlink -f "$0")
cd "$(dirname "$SELF")"
MODE=${1:-all}
if [ "$MODE" = all ]; then
  for m in drain newpool twostep health swap; do "$SELF" "$m"; done
  exit 0
fi
(cd ../src &&
  go build -o ../bin/bareproxy ./cmd/bareproxy &&
  go build -o ../bin/testapi ./tools/testapi &&
  go build -o ../bin/loadtest ./tools/loadtest) || { echo "build failed"; exit 1; }
BIN=$(cd ../bin && pwd)
RUN=run/exp-$MODE
rm -rf $RUN && mkdir -p $RUN && cd $RUN
pids=()
trap 'for p in "${pids[@]:-}"; do kill "$p" 2>/dev/null; done; wait 2>/dev/null' EXIT

read -r P A1 A2 <<< "$(python3 - <<'PY'
import socket
s = [socket.socket() for _ in range(3)]
for x in s:
    x.bind(("127.0.0.1", 0))
print(*[x.getsockname()[1] for x in s])
PY
)"
"$BIN/testapi" api-1 127.0.0.1:$A1 2>/dev/null & pids+=($!)
"$BIN/testapi" api-2 127.0.0.1:$A2 2>/dev/null & pids+=($!)

hdr() { printf 'global\n  admin admin.sock\n  state state\n  trace-log off\n\nsite http://plain.test:%s\n' "$P"; }
pool() { # NAME BACKEND_PORT...
  local name=$1; shift
  printf '\npool %s\n' "$name"
  for b in "$@"; do echo "  backend 127.0.0.1:$b"; done
  echo "  health /healthz every ${EVERY:-1s}"
  if [ -n "${DRAIN:-}" ]; then echo "  drain $DRAIN"; fi
}
# mk builds the config of one step; the arguments depend on the mode.
HTTP=(-http1 "http://plain.test:$P" -paths /moved/x=backend -split 100,0,0 -rate 1000 -duration 24s)
case $MODE in
  drain)
    TITLE="remove a backend from the pool that carries 20 WebSocket connections (drain 3s)"
    mk() { hdr; echo "  route /ws -> ws"; if [ "$1" = both ]; then DRAIN=3s pool ws $A1 $A2; else DRAIN=3s pool ws $A2; fi; }
    mk both > c.conf
    LOAD=(-ws "ws://plain.test:$P/ws" -split 0,0,100 -ws-conns 20 -rate 400 -duration 20s)
    STEPS=1 ;;
  newpool)
    TITLE="10 applies, each adds a new pool and points /moved/* at it in the same apply"
    mk() { hdr; echo "  route /moved/* -> $2 strip"; for p in $1; do if [ "$p" = p0 ]; then pool p0 $A1; else pool "$p" $A2; fi; done; }
    mk p0 p0 > c.conf; LOAD=("${HTTP[@]}"); STEPS=10 ;;
  twostep)
    TITLE="10 times: one apply adds a new pool, the next (0.6 s later) points /moved/* at it"
    mk() { hdr; echo "  route /moved/* -> $2 strip"; for p in $1; do if [ "$p" = p0 ]; then pool p0 $A1; else pool "$p" $A2; fi; done; }
    mk p0 p0 > c.conf; LOAD=("${HTTP[@]}"); STEPS=10 ;;
  health)
    TITLE="10 applies that change a pool's health line (every 1s and every 2s in turn)"
    mk() { hdr; echo "  route /moved/* -> api strip"; EVERY=$1 pool api $A1 $A2; }
    mk 1s > c.conf; LOAD=("${HTTP[@]}"); STEPS=10 ;;
  swap)
    TITLE="10 applies that replace the only backend of a pool with the other one, and back"
    mk() { hdr; echo "  route /moved/* -> api strip"; pool api "$1"; }
    mk $A1 > c.conf; LOAD=("${HTTP[@]}"); STEPS=10 ;;
  *) echo "usage: $0 [drain|newpool|twostep|health|swap]"; exit 2 ;;
esac

printf '\n## %s: %s\n' "$MODE" "$TITLE"
echo "starting config:"; cat -n c.conf
"$BIN/bareproxy" run c.conf > bp.log 2>&1 & pids+=($!)
for _ in $(seq 40); do
  [ -S admin.sock ] && "$BIN/bareproxy" status -c c.conf > /dev/null 2>&1 &&
    [ "$(curl -s --noproxy '*' -m 2 -o /dev/null -w '%{http_code}' --resolve plain.test:$P:127.0.0.1 http://plain.test:$P/moved/x)" != 000 ] && break
  sleep 0.25
done
T0=$(date +%s.%N)
"$BIN/loadtest" "${LOAD[@]}" -connect 127.0.0.1 > lt.out 2>&1 & LT=$!; pids+=($LT)
sleep 4
for k in $(seq 1 $STEPS); do
  case $MODE in
    drain)   mk one > c.conf ;;
    newpool) mk "p0 p$k" p$k > c.conf ;;
    twostep) mk "p$((k-1)) p$k" p$((k-1)) > c.conf
             "$BIN/bareproxy" apply c.conf --yes > ap.out 2>&1 || { echo "apply failed:"; cat ap.out; }
             sleep 0.6; mk "p$k" p$k > c.conf ;;
    health)  if [ $((k % 2)) = 1 ]; then mk 2s > c.conf; else mk 1s > c.conf; fi ;;
    swap)    if [ $((k % 2)) = 1 ]; then mk $A2 > c.conf; else mk $A1 > c.conf; fi ;;
  esac
  a=$(date +%s.%N)
  "$BIN/bareproxy" apply c.conf --yes > ap.out 2>&1 || { echo "apply $k failed:"; cat ap.out; }
  printf 'apply %d at %.1f s: %s\n' "$k" "$(awk -v a="$a" -v t="$T0" 'BEGIN { print a - t }')" "$(tail -1 ap.out)"
  [ "$MODE" = drain ] && break
  sleep 1.4
done
wait $LT
echo "the load tool:"
grep -vE '^\{|^t=|first failures|^Request ID|^load starts' lt.out | sed 's/^/  /'
echo "BareProxy's log, drain lines:"
grep -E "removed, draining|drained" bp.log | sed 's/^bareproxy: //; s/^/  /' | head -6
