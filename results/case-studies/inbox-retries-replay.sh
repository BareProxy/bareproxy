#!/usr/bin/env bash
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# Case study 3: forged and stale signatures, senders that send the same
# event twice, and a bad deploy whose failed events are replayed.
#
# Three inboxes in the formats of GitHub, Shopify and Stripe, signed with
# test secrets made for the run. Retries are short here (2 s, up to 10 s,
# give up after 30 s) so a failed event shows up within a minute; the
# defaults are 10 s, 1 h and 3 days. Local only.
#
# usage: results/case-studies/inbox-retries-replay.sh  > results/case-studies/inbox-retries-replay.log

source "$(dirname "$0")/lib/common.sh"
getbp
W=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null; sleep 0.3; rm -rf "$W"' EXIT
exec > >(sed -u "s#$W#/work#g; s#$BP#bareproxy#g") 2>&1
PORT=$(freeport); APP=$(freeport)
for k in github stripe shopify; do head -c 24 /dev/urandom | base64 >"$W/$k-secret"; done
cat >"$W/bareproxy.conf" <<EOC
global
  admin $W/admin.sock
  state $W/state
  trace-log $W/requests.log 10MB 2

site http://hooks.test:$PORT
  route /hooks/github -> inbox github
  route /hooks/shopify -> inbox shopify
  route /hooks/stripe -> inbox stripe

inbox github
  deliver app
  signature github $W/github-secret
  key-header X-GitHub-Delivery
  retry 2s 10s
  give-up 30s

inbox shopify
  deliver app
  signature shopify $W/shopify-secret
  key-header X-Shopify-Webhook-Id
  retry 2s 10s
  give-up 30s

inbox stripe
  deliver app
  signature stripe $W/stripe-secret
  retry 2s 10s
  give-up 30s

pool app
  backend 127.0.0.1:$APP
EOC
export CASE_START=$(date +%s.%N) HOOK_HOST=hooks.test:$PORT
el() { python3 -c "import time;print(f't={time.time()-$CASE_START:.1f}s')"; }
send() { local k=$1; shift; python3 "$LIB/send.py" "http://127.0.0.1:$PORT/hooks/$k" "$k" "$W/$k-secret" "$@"; }
bpc() { local rc=0; "$BP" "$@" -c "$W/bareproxy.conf" 2>&1 | sed "s#$W#/work#g"; }
counts() { "$BP" inbox -c "$W/bareproxy.conf" --json list | jq -r '.. | objects | select(has("name") and has("pending")) | "\(.name): \(.pending) pending, \(.delivering) delivering, \(.delivered) delivered, \(.failed) failed"' 2>/dev/null || "$BP" inbox -c "$W/bareproxy.conf" list | grep -E '^Inbox|pending,'; }
idle() { for _ in $(seq 120); do n=$("$BP" inbox -c "$W/bareproxy.conf" --json list | jq '[.. | objects | select(has("status")) | select(.status=="pending" or .status=="delivering")] | length'); [ "$n" = 0 ] && return; sleep 0.5; done; }

echo "# Case study: forged signatures, repeated events and a bad deploy"
echo "# $("$BP" version), $(date -u '+%Y-%m-%d %H:%M:%S UTC'), $(uname -sm)"
say "Config"
sed "s#$W#/work#g" "$W/bareproxy.conf"
python3 "$LIB/app.py" "$APP" "$W/app.log" "$W/seen" "$W/mode" &
waitport "$APP"
"$BP" run "$W/bareproxy.conf" >"$W/server.log" 2>&1 &
waitport "$PORT"

say "What an inbox route does"
show "$BP" explain -c "$W/bareproxy.conf" POST "http://hooks.test:$PORT/hooks/github" 2>&1 | sed "s#$W#/work#g"

say "Part 1: signatures"
send github 1
send github 2 --bad-signature
send shopify 3 --bad-signature
send stripe 4 --bad-signature
send stripe 5 --stale
send stripe 6
idle
echo "Stored events:"
"$BP" inbox -c "$W/bareproxy.conf" list | grep -E '^Inbox|pending,' | sed "s#$W#/work#g"

say "Part 2: the sender sends the same event again"
echo "GitHub redelivers with the same X-GitHub-Delivery:"
G=$(python3 -c 'import uuid;print(uuid.uuid4())')
for i in 1 2 3; do send github 10 --id "$G"; done
echo "Shopify retries with the same X-Shopify-Webhook-Id:"
S=$(python3 -c 'import uuid;print(uuid.uuid4())')
for i in 1 2; do send shopify 11 --id "$S"; done
echo "Stripe retries the same event (same evt_ ID in the body, new Stripe-Signature):"
for i in 1 2; do send stripe 12 --id evt_retried_by_stripe; sleep 1.1; done
idle
echo
echo "What the app received for these:"
grep -E "\"$G\"|\"$S\"|evt_retried_by_stripe" "$W/app.log"

say "Part 3: a bad deploy answers 500 for a while"
echo fail >"$W/mode"
echo "$(el) the app now answers 500"
send github 20; send github 21; send shopify 22; send stripe 23
echo "Waiting for the inbox to give up (give-up 30s)..."
for _ in $(seq 90); do
	f=$("$BP" inbox -c "$W/bareproxy.conf" --json list --status failed | jq '[.. | objects | select(has("status")) | select(.status=="failed")] | length')
	[ "$f" = 4 ] && break
	sleep 1
done
echo "$(el) failed events:"
show "$BP" inbox -c "$W/bareproxy.conf" list --status failed 2>&1 | sed "s#$W#/work#g"
echo
echo ok >"$W/mode"
echo "$(el) the fix is deployed; replaying"
for k in github shopify stripe; do show "$BP" inbox -c "$W/bareproxy.conf" replay $k --failed; done
idle
echo "$(el) done"

say "Every delivery the app received in this run"
cat "$W/app.log"

say "Inbox at the end"
"$BP" inbox -c "$W/bareproxy.conf" list | sed "s#$W#/work#g"

say "Summary"
python3 - "$W/app.log" "$G" "$S" <<'PY'
import json, sys, collections
recs = [json.loads(l) for l in open(sys.argv[1])]
def n(f): return sum(1 for r in recs if f(r))
print(f"GitHub delivery sent 3 times, deliveries to the app: {n(lambda r: r['sender_event'] == sys.argv[2])}")
print(f"Shopify webhook sent 2 times, deliveries to the app: {n(lambda r: r['sender_event'] == sys.argv[3])}")
print(f"Stripe event sent 2 times, deliveries to the app: {n(lambda r: r['sender_event'] == 'evt_retried_by_stripe')}, processed: {n(lambda r: r['sender_event'] == 'evt_retried_by_stripe' and r['result'] == '200 processed')}")
print(f"answers 500 during the bad deploy: {n(lambda r: r['result'].startswith('500'))}")
print(f"replayed deliveries marked operator: {n(lambda r: r['replay'] == 'operator')}, processed: {n(lambda r: r['replay'] == 'operator' and r['result'] == '200 processed')}")
PY
