#!/usr/bin/env bash
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# Measures BareProxy against nginx with the same routes, on this machine, and
# writes a raw log plus a summary. Nothing here touches the system nginx or
# the BareProxy source tree.
#
#   ./bench.sh                               measure bin/bareproxy-stripped
#   BAREPROXY=/path/to/bareproxy ./bench.sh  measure another binary
#   BUILD=1 ./bench.sh                       rebuild BareProxy, testapi and the site first
#   BENCH_MODES=off,file,tls ./bench.sh      add the HTTPS with HTTP/2 case (default: off,file)
#   BENCH_DURATION=2 BENCH_WARMUP=1 BENCH_RUNS=1 ./bench.sh    quick check, about 2 minutes
#
# Time: on a quiet machine a run of the default modes (2 s warm-up, 10 s measured, 3 rounds, 4 routes,
# 2 servers, 2 logging modes, plus the backend alone) takes about 12 minutes, and 18 minutes with
# tls. This machine is shared, and a run that other processes disturb (more than 10% of a core spent
# by something that is not ours) is repeated, up to BENCH_TRIES attempts (default 3), so expect two
# to three times as long when the other workers are busy. Every attempt stays in the log.
#
# Output: results/bench-YYYY-MM-DD.log (raw; a new name with -HHMM is used when that file exists)
# and results/summary.md, made from the new log plus any logs named in EXTRA_LOGS (space separated),
# whose attempts are pooled. To rebuild the summary from any logs:
#   python3 summarize.py results/bench-A.log results/bench-B.log > results/summary.md
#
# Other settings (environment): BENCH_CONNS (50), BENCH_ROUTES (home,file,404,api),
# BENCH_SERVERS (bareproxy,nginx), BENCH_DIRECT (1: also measure the backend alone), BENCH_SERVER_CPU (0),
# BENCH_LOAD_CPU (1), BENCH_TRIES (3), BENCH_NOISE_PCT (10), BENCH_QUIET_WAIT (15 s),
# BENCH_MAX_MINUTES (40, no repeats after that), BP_PORT (18080), NGX_PORT (18081), API_PORT (19001).
# A port that is taken is replaced by the next free one, and the log says so.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export GOTOOLCHAIN=local GOPROXY=off GOFLAGS=-buildvcs=false

need() {
  command -v "$1" >/dev/null 2>&1 || { echo "bench.sh: $1 is not installed ($2)" >&2; exit 1; }
}
need python3 "needed to run the harness"
need wrk "apt-get install -y wrk"
case ",${BENCH_MODES:-off,file}," in
  *,tls,*)
    need h2load "apt-get install -y nghttp2-client (the tls mode uses it)"
    need openssl "needed to make the test certificate for the tls mode"
    ;;
esac
need taskset "part of util-linux"
need nginx "apt-get install -y nginx; the system service is never started, only a private copy"

if [ "${BUILD:-0}" = 1 ] || [ ! -x "$HERE/bin/testapi" ] || [ ! -x "$HERE/bin/bareproxy-stripped" ] \
   || [ ! -x "$HERE/bin/bareproxy" ] || [ ! -f "$HERE/site/public/index.html" ]; then
  "$HERE/build.sh"
fi

export BAREPROXY="${BAREPROXY:-$HERE/bin/bareproxy-stripped}"
mkdir -p "$HERE/results"
if [ -z "${BENCH_LOG:-}" ]; then
  BENCH_LOG="$HERE/results/bench-$(date +%F).log"
  [ -e "$BENCH_LOG" ] && BENCH_LOG="$HERE/results/bench-$(date +%F-%H%M).log"
fi
export BENCH_LOG

status=0
python3 "$HERE/bench.py" || status=$?

if [ -s "$BENCH_LOG" ] && [ -f "$HERE/summarize.py" ]; then
  # shellcheck disable=SC2086
  python3 "$HERE/summarize.py" ${EXTRA_LOGS:-} "$BENCH_LOG" > "$HERE/results/summary.md.new" \
    && mv "$HERE/results/summary.md.new" "$HERE/results/summary.md" \
    && echo "summary: $HERE/results/summary.md"
fi
echo "log: $BENCH_LOG"
exit "$status"
