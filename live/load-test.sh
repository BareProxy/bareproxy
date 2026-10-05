#!/usr/bin/env bash
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
# BareProxy load test: 2,000 requests per second for about 60 seconds over
# HTTP/1.1, HTTP/2 and WebSocket, with 20 applies (`bareproxy apply --yes`)
# that change routing and pools while the load runs. It builds bareproxy,
# testapi and loadtest from the tree, starts three test backends and
# BareProxy, and prints what the load tool counted and what BareProxy logged.
#
# Pass means: 0 failed requests, 0 reset connections, 0 lost WebSocket
# messages and 20 applies done. Exit status 0 for a pass, 1 for a fail.
#
# Needs Go (to build), openssl, curl and python3. Settings from the
# environment: DURATION (seconds, default 60), RATE (requests per second,
# default 2000), APPLIES (default 20), TRACE_MEMORY (the config's trace-memory,
# such as off or 8MB; empty leaves the default). KILL_BACKEND_AT=SECONDS kills backend
# api-1 that long into the load (with APPLIES=0 this is the control run: the
# test must then fail, which shows that the load tool sees failures).
set -u
cd "$(dirname "$0")"
DURATION=${DURATION:-60}
RATE=${RATE:-2000}
APPLIES=${APPLIES:-20}
TM=""; if [ -n "${TRACE_MEMORY:-}" ]; then TM=$'\n'"  trace-memory $TRACE_MEMORY"; fi
BIN=../bin
RUN=run/load
BP=$BIN/bareproxy
step() { printf '\n## %s\n' "$*"; }
now() { date +%s.%N; }

# The machine: 2 CPUs shared with other people's builds, tests and servers.
# This prints what each process used during the run, so a slow result can be
# read against what else was running.
cpusnap() { python3 - "$@" <<'PY'
import os, sys, json
mode, path = sys.argv[1], sys.argv[2]
def snap():
    out = {}
    for pid in filter(str.isdigit, os.listdir("/proc")):
        try:
            raw = open(f"/proc/{pid}/stat").read()
            comm = raw[raw.index("(") + 1:raw.rindex(")")]
            f = raw[raw.rindex(")") + 2:].split()
            out[pid] = [comm, int(f[11]) + int(f[12]), int(f[19])]
        except Exception:
            pass
    cpu = [int(x) for x in open("/proc/stat").readline().split()[1:]]
    return {"procs": out, "cpu": cpu}
if mode == "save":
    json.dump(snap(), open(path, "w"))
else:
    a, b = json.load(open(path)), snap()
    tick = os.sysconf("SC_CLK_TCK")
    d = [y - x for x, y in zip(a["cpu"], b["cpu"])]
    total = sum(d) or 1
    idle = d[3] + d[4]
    steal = d[7] if len(d) > 7 else 0
    print("machine, over the run: %.0f%% of both CPUs busy (idle %.0f%%, taken by the host %.0f%%)" % (
        100 * (total - idle - steal) / total, 100 * idle / total, 100 * steal / total))
    used = []
    for pid, (comm, t, st) in b["procs"].items():
        t0 = a["procs"].get(pid, [comm, 0, 0])
        if a["procs"].get(pid, [None, 0, st])[2] != st:
            t0 = [comm, 0, 0]  # a new process that reuses the number
        if t - t0[1] > 0:
            used.append((t - t0[1], pid, comm))
    used.sort(reverse=True)
    print("CPU seconds used during the run, busiest processes:")
    for t, pid, comm in used[:8]:
        print("  %6.1f s  %-16s pid %s" % (t / tick, comm, pid))
PY
}

pids=()
cleanup() {
  for p in "${pids[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null; done
  wait 2>/dev/null
}
trap cleanup EXIT

step "The test and the machine"
echo "BareProxy load test, $(date '+%Y-%m-%d %H:%M:%S %Z')"
echo "target: $RATE requests per second for $DURATION seconds over HTTP/1.1, HTTP/2 and WebSocket, $APPLIES applies during the run"
echo "machine: $(nproc) CPUs ($(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2 | sed 's/^ //')), $(awk '/MemTotal/{printf "%.1f GB", $2/1048576}' /proc/meminfo) memory, $(uname -sr)"
echo "The CPUs are shared with other workers' builds, tests and servers; the results depend on what they were doing."
echo "load average before the test (1, 5, 15 minutes): $(cut -d' ' -f1-3 /proc/loadavg)"
echo "tree: $(git rev-parse --short HEAD 2>/dev/null || echo unknown)$(git diff --quiet HEAD -- .. 2>/dev/null || echo ' plus uncommitted changes')"

step "Build from the tree"
(cd ../src &&
  go build -o ../bin/bareproxy ./cmd/bareproxy &&
  go build -o ../bin/testapi ./tools/testapi &&
  go build -o ../bin/loadtest ./tools/loadtest) || { echo "build failed"; exit 1; }
$BP version
go version

step "Setup: certificate, ports, config"
rm -rf $RUN && mkdir -p $RUN
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout $RUN/key.pem -out $RUN/cert.pem \
  -days 2 -subj "/CN=secure.test" -addext "subjectAltName=DNS:secure.test" 2>/dev/null
# Free ports, so a second test or another worker's server can't collide with this one.
read -r P_HTTP P_HTTPS P_A1 P_A2 P_A3 <<< "$(python3 - <<'PY'
import socket
socks = [socket.socket() for _ in range(5)]
for s in socks:
    s.bind(("127.0.0.1", 0))
print(*[s.getsockname()[1] for s in socks])
PY
)"
PORT=(0 "$P_A1" "$P_A2" "$P_A3")
echo "ports: http $P_HTTP, https $P_HTTPS, backends api-1 $P_A1, api-2 $P_A2, api-3 $P_A3"

# The config is written from these four settings. Each apply changes one or two.
API="1 2"; API2="3"; HELLO=v1; MOVED=api
routes() {
  cat <<EOT
  route /hello -> respond 200 "hello $HELLO"
  route /api/* -> api strip
  route /moved/* -> $MOVED strip
  route /ws -> ws
  route /spare/* -> api2 strip
EOT
}
pool() {
  local name=$1; shift
  printf '\npool %s\n' "$name"
  for n in "$@"; do echo "  backend 127.0.0.1:${PORT[$n]}"; done
  echo "  health /healthz every 1s"
}
conf() {
  cat <<EOT
# BareProxy load test
global
  admin admin.sock
  state state
  trace-log requests.log$TM

site http://plain.test:$P_HTTP
$(routes)

site https://secure.test:$P_HTTPS
  tls cert.pem key.pem
$(routes)
$(pool api $API)
$(pool api2 $API2)
$(pool ws 1 2 3)
EOT
}
# change N sets the settings for apply N and says what it does.
change() {
  case $1 in
     1) API="1 2 3";   WHAT="add backend api-3 to pool api" ;;
     2) HELLO=v2;      WHAT="change the /hello text to v2" ;;
     3) MOVED=api2;    WHAT="move /moved/* to pool api2" ;;
     4) API="2 3";     WHAT="remove backend api-1 from pool api" ;;
     5) HELLO=v1;      WHAT="change the /hello text back to v1" ;;
     6) MOVED=api;     WHAT="move /moved/* back to pool api" ;;
     7) API="2 3 1";   WHAT="add backend api-1 to pool api again" ;;
     8) API="3 1";     WHAT="remove backend api-2 from pool api" ;;
     9) HELLO=v3;      WHAT="change the /hello text to v3" ;;
    10) MOVED=api2;    WHAT="move /moved/* to pool api2" ;;
    11) API="3 1 2";   WHAT="add backend api-2 to pool api again" ;;
    12) API2="3 2";    WHAT="add backend api-2 to pool api2" ;;
    13) HELLO=v1;      WHAT="change the /hello text back to v1" ;;
    14) MOVED=api;     WHAT="move /moved/* back to pool api" ;;
    15) API="1 2";     WHAT="remove backend api-3 from pool api" ;;
    16) API2="3";      WHAT="remove backend api-2 from pool api2" ;;
    17) HELLO=v4;      WHAT="change the /hello text to v4" ;;
    18) API="1 2 3";   WHAT="add backend api-3 to pool api" ;;
    19) MOVED=api2;    WHAT="move /moved/* to pool api2" ;;
    20) HELLO=v1; MOVED=api; WHAT="change the /hello text back to v1 and move /moved/* back to pool api" ;;
    *)  WHAT="no change"; return 1 ;;
  esac
}
# probe asks both sites for /hello, /api/x and /moved/x right after an apply.
# New requests must already follow the new config: the new /hello text, and an
# answer from a backend that is in the pool the route points at now.
PROBES=0
probe() {
  local site pool set want_hello=$1 line n bad=0
  for site in "http://plain.test:$P_HTTP" "https://secure.test:$P_HTTPS"; do
    local out
    # One line per answer: the body, then | and the status.
    out=$("${CURL[@]}" -w '##%{http_code}' "$site/hello" "$site/api/x" "$site/moved/x" "$site/api/x" "$site/moved/x" "$site/api/x" "$site/moved/x" 2>&1 |
      tr -d '\n' | sed 's/##\([0-9][0-9][0-9]\)/|\1\n/g')
    PROBES=$((PROBES + 7))
    n=0
    while IFS= read -r line; do
      case $n in
        0) [ "$line" = "$want_hello|200" ] || { echo "  probe: $site/hello answered '$line', wanted '$want_hello|200'"; bad=1; } ;;
        1|3|5) set=$API ;;
        *) if [ "$MOVED" = api2 ]; then set=$API2; else set=$API; fi ;;
      esac
      if [ $n -gt 0 ]; then
        local b=${line#*\"backend\":\"api-}; b=${b%%\"*}
        case " $set " in *" $b "*) ;; *) echo "  probe: $site request $n was answered by api-$b, which isn't in the pool it should use ($set): $line"; bad=1 ;; esac
        case $line in *'|200') ;; *) echo "  probe: $site request $n: $line"; bad=1 ;; esac
      fi
      n=$((n + 1))
    done <<< "$out"
  done
  return $bad
}
conf > $RUN/load.conf
step "The starting config (line numbers added for reading)"
cat -n $RUN/load.conf
$BP check $RUN/load.conf || exit 1

step "Start three backends and BareProxy"
for n in 1 2 3; do
  $BIN/testapi api-$n 127.0.0.1:${PORT[$n]} 2>$RUN/api-$n.log & pids+=($!)
done
$BP run $RUN/load.conf >$RUN/bareproxy.log 2>&1 & BPID=$!; pids+=($BPID)
CURL=(curl -sS --noproxy '*' -m 3 --resolve plain.test:$P_HTTP:127.0.0.1 --resolve secure.test:$P_HTTPS:127.0.0.1 --cacert $RUN/cert.pem)
ready=0
for _ in $(seq 1 60); do
  c1=$("${CURL[@]}" -o /dev/null -w '%{http_code}' http://plain.test:$P_HTTP/api/x 2>/dev/null)
  c2=$("${CURL[@]}" -o /dev/null -w '%{http_code}' https://secure.test:$P_HTTPS/moved/x 2>/dev/null)
  c3=$("${CURL[@]}" -o /dev/null -w '%{http_code}' http://plain.test:$P_HTTP/ws 2>/dev/null)  # the backend answers 400 to a plain GET
  if [ "$c1" = 200 ] && [ "$c2" = 200 ] && [ "$c3" = 400 ]; then ready=1; break; fi
  sleep 0.5
done
if [ $ready != 1 ]; then echo "BareProxy didn't come up; its log:"; cat $RUN/bareproxy.log; exit 1; fi
for u in http://plain.test:$P_HTTP/hello http://plain.test:$P_HTTP/api/x https://secure.test:$P_HTTPS/hello https://secure.test:$P_HTTPS/moved/x; do
  printf '%s -> HTTP/%s %s\n' "$u" "$("${CURL[@]}" -o $RUN/body -w '%{http_version}' "$u")" "$(head -c 70 $RUN/body | tr '\n' ' ')"
done
$BP status -c $RUN/load.conf | head -30
RECORDS_BEFORE=$(wc -l < $RUN/requests.log)

step "Load: $RATE per second for $DURATION seconds, 20 applies spread across it"
LOADAVG_BEFORE=$(cut -d' ' -f1-3 /proc/loadavg)
cpusnap save $RUN/snap.json
BP_TICKS0=$(awk '{print $14+$15}' /proc/$BPID/stat)
T0=$(now)
$BIN/loadtest -http1 http://plain.test:$P_HTTP -http2 https://secure.test:$P_HTTPS \
  -ws "ws://plain.test:$P_HTTP/ws,wss://secure.test:$P_HTTPS/ws" -connect 127.0.0.1 -cacert $RUN/cert.pem \
  -rate "$RATE" -duration "${DURATION}s" > $RUN/loadtest.out 2>&1 & LT=$!; pids+=($LT)

APPLIES_OK=0
GAP=$(awk -v d="$DURATION" -v n="$APPLIES" 'BEGIN { printf "%.3f", (n > 0 ? (d - 9) / n : 0) }')
if [ -n "${KILL_BACKEND_AT:-}" ]; then
  ( sleep "$KILL_BACKEND_AT"; kill "${pids[0]}"; echo "control: backend api-1 killed $KILL_BACKEND_AT s into the load" > $RUN/control.txt ) &
fi
for i in $(seq 1 "$APPLIES"); do
  at=$(awk -v t0="$T0" -v i="$i" -v g="$GAP" 'BEGIN { printf "%.3f", t0 + 4 + (i - 1) * g }')
  sleep "$(awk -v n="$(now)" -v t="$at" 'BEGIN { d = t - n; printf "%.3f", (d < 0 ? 0 : d) }')"
  if ! change "$i"; then break; fi
  conf > $RUN/load.conf.new && mv $RUN/load.conf.new $RUN/load.conf
  a=$(now)
  out=$($BP apply $RUN/load.conf --yes 2>&1); rc=$?
  b=$(now)
  printf '\napply %d at %.1f s, took %.0f ms: %s\n' "$i" "$(awk -v a="$a" -v t0="$T0" 'BEGIN { print a - t0 }')" "$(awk -v a="$a" -v b="$b" 'BEGIN { print (b - a) * 1000 }')" "$WHAT"
  echo "$out" | sed 's/^/  /'
  if [ $rc = 0 ] && echo "$out" | grep -q 'is running'; then
    if probe "hello $HELLO"; then APPLIES_OK=$((APPLIES_OK + 1)); else echo "  apply $i was accepted but the next requests didn't follow the new config"; fi
  else
    echo "  this apply failed (exit status $rc)"
  fi
done
wait $LT
LT_RC=$?
BP_TICKS1=$(awk '{print $14+$15}' /proc/$BPID/stat)
BP_RSS=$(awk '/VmHWM/{printf "%.0f", $2/1024}' /proc/$BPID/status)
step "What the load tool counted"
cat $RUN/loadtest.out

step "The machine during the run"
echo "load average before (1, 5, 15 minutes): $LOADAVG_BEFORE; after: $(cut -d' ' -f1-3 /proc/loadavg)"
cpusnap diff $RUN/snap.json
echo "BareProxy: $(awk -v a="$BP_TICKS0" -v b="$BP_TICKS1" -v h="$(getconf CLK_TCK)" 'BEGIN { printf "%.1f CPU seconds", (b - a) / h }') in $DURATION seconds, peak memory $BP_RSS MiB"

step "BareProxy's events (bareproxy events)"
$BP events -c $RUN/load.conf
step "Versions (bareproxy history, the first line and the last 4)"
HISTORY=$($BP history -c $RUN/load.conf)
if [ "$(echo "$HISTORY" | wc -l)" -gt 6 ]; then echo "$HISTORY" | head -1; echo "$HISTORY" | tail -4; else echo "$HISTORY"; fi

step "Trace records"
sleep 2
RECORDS=$(( $(wc -l < $RUN/requests.log) - RECORDS_BEFORE ))
python3 - "$RUN/loadtest.out" "$RECORDS" "$PROBES" <<'PY'
import json, sys
s = json.loads([l for l in open(sys.argv[1]) if l.startswith("{")][-1])
k = s["kinds"]
probes = int(sys.argv[3])
want = k["http1"]["sent"] + k["http2"]["sent"] + k["ws"]["conns_opened"] + probes
got = int(sys.argv[2])
print("requests BareProxy was sent: %d (%d HTTP/1.1, %d HTTP/2, %d WebSocket handshakes, %d probes after the applies); records written: %d (%s)" % (
    want, k["http1"]["sent"], k["http2"]["sent"], k["ws"]["conns_opened"], probes, got, "equal" if want == got else "NOT equal, %+d" % (got - want)))
PY

step "Result"
python3 - "$RUN/loadtest.out" "$APPLIES_OK" "$APPLIES" <<'PY' | tee $RUN/verdict
import json, sys
try:
    s = json.loads([l for l in open(sys.argv[1]) if l.startswith("{")][-1])
except Exception:
    print("FAIL: the load tool printed no summary")
    sys.exit(0)
ok, want = int(sys.argv[2]), int(sys.argv[3])
bad = []
if s["failed"]: bad.append("%d failed requests" % s["failed"])
if s["resets"]: bad.append("%d connections reset" % s["resets"])
if s["ws_messages_lost"]: bad.append("%d WebSocket messages lost" % s["ws_messages_lost"])
if not s["rate_ok"]: bad.append("the load tool didn't hold the rate")
if ok != want: bad.append("%d of %d applies done and followed by the new config" % (ok, want))
print("%s: %d requests and WebSocket messages sent, %d answered, %d failed, %d connections reset, %d WebSocket messages lost, %d of %d applies done%s" % (
    "FAIL" if bad else "PASS", s["sent"], s["answered"], s["failed"], s["resets"], s["ws_messages_lost"], ok, want,
    ("; " + "; ".join(bad)) if bad else ""))
PY
grep -q '^PASS' $RUN/verdict
