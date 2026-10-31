#!/usr/bin/env bash
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# Case study 2: BareProxy is killed in the middle of deliveries.
#
# The app takes 3 seconds per webhook. Six Stripe-format webhooks come in,
# four go out at once (workers 4), and BareProxy is killed with SIGKILL
# while those four are inside the app. The app finishes them anyway. A
# webhook sent while BareProxy is down gets no answer, and its sender
# tries again. Then BareProxy starts again and finishes the work. The app
# keeps the inbox IDs it has handled, so it can spot a repeat.
# Local only, with a test secret made for the run.
#
# usage: results/case-studies/inbox-crash.sh  > results/case-studies/inbox-crash.log

source "$(dirname "$0")/lib/common.sh"
getbp
W=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null; sleep 0.3; rm -rf "$W"' EXIT
exec > >(sed -u "s#$W#/work#g; s#$BP#bareproxy#g") 2>&1
PORT=$(freeport); APP=$(freeport)
head -c 24 /dev/urandom | base64 >"$W/stripe-secret"
cat >"$W/bareproxy.conf" <<EOC
global
  admin $W/admin.sock
  state $W/state
  trace-log $W/requests.log 10MB 2

site http://hooks.test:$PORT
  route /hooks/stripe -> inbox stripe

inbox stripe
  deliver app
  signature stripe $W/stripe-secret
  workers 4

pool app
  backend 127.0.0.1:$APP
EOC
export CASE_START=$(date +%s.%N) HOOK_HOST=hooks.test:$PORT
el() { python3 -c "import time;print(f't={time.time()-$CASE_START:.1f}s')"; }
send() { python3 "$LIB/send.py" "http://127.0.0.1:$PORT/hooks/stripe" stripe "$W/stripe-secret" "$@" | tee -a "$W/sent"; }
startbp() { "$BP" run "$W/bareproxy.conf" >>"$W/server.log" 2>&1 & BPPID=$!; waitport "$PORT"; }
bpc() { "$BP" inbox -c "$W/bareproxy.conf" "$@" 2>&1 | sed "s#$W#/work#g"; }

echo "# Case study: a crash in the middle of deliveries"
echo "# $("$BP" version), $(date -u '+%Y-%m-%d %H:%M:%S UTC'), $(uname -sm)"
say "Config"
sed "s#$W#/work#g" "$W/bareproxy.conf"
echo "slow 3" >"$W/mode"
python3 "$LIB/app.py" "$APP" "$W/app.log" "$W/seen" "$W/mode" &
waitport "$APP"
startbp

say "Six webhooks; the app takes 3 s for each"
for i in 1 2 3 4 5 6; do send "$i"; done
sleep 1.2
say "SIGKILL while four deliveries are inside the app"
echo "$(el) kill -9 bareproxy"
kill -9 $BPPID; wait $BPPID 2>/dev/null
echo "The event files on disk, as BareProxy left them:"
for f in "$W"/state/inbox/stripe/*.state; do printf '%s: ' "$(basename "$f")"; tr '\n' ' ' <"$f"; echo; done
say "A webhook arrives while BareProxy is down"
send 7 --id evt_sent_during_outage
sleep 3
echo "$(el) the app has finished what it had:"
cat "$W/app.log"
echo "ok" >"$W/mode"

say "BareProxy starts again; the sender of event 7 tries again"
echo "$(el) bareproxy run"
startbp
send 7 --id evt_sent_during_outage
for _ in $(seq 60); do
	n=$("$BP" inbox -c "$W/bareproxy.conf" --json list | jq '[.. | objects | select(has("status")) | select(.status=="pending" or .status=="delivering")] | length')
	[ "$n" = 0 ] && break
	sleep 0.5
done
echo "$(el) nothing pending"

say "What the app received (one line per delivery)"
cat "$W/app.log"

say "Inbox"
bpc list stripe
IND=$(jq -r 'select(.replay=="in-doubt") | .inbox_id' "$W/app.log" | head -1)
say "One of the in-doubt events"
bpc show stripe "$IND"

say "Summary"
python3 - "$W/app.log" "$W/sent" <<'PY'
import json, sys, collections
recs = [json.loads(l) for l in open(sys.argv[1])]
sent = [l.split() for l in open(sys.argv[2])]
ok = [x for x in sent if x[4] == "202"]
print(f"webhooks sent: {len(sent)}, answered 202: {len(ok)}, no answer: {len(sent) - len(ok)}")
by = collections.defaultdict(list)
for r in recs: by[r["inbox_id"]].append(r)
print(f"deliveries: {len(recs)}, distinct events: {len(by)}")
print(f"events the senders got a 202 for that never reached the app: {sum(1 for x in ok if x[5] not in by)}")
print(f"events that reached the app twice: {sum(1 for v in by.values() if len(v) > 1)}")
print(f"  of those, second delivery marked in-doubt: {sum(1 for v in by.values() if len(v) > 1 and v[1]['replay'] == 'in-doubt')}")
print(f"deliveries marked in-doubt: {sum(1 for r in recs if r['replay'] == 'in-doubt')}")
print(f"processed by the app: {sum(1 for r in recs if r['result'] == '200 processed')}, skipped as repeats: {sum(1 for r in recs if 'duplicate' in r['result'])}")
PY
