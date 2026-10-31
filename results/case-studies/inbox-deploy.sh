#!/usr/bin/env bash
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# Case study 1: webhooks keep arriving while the app is down for a deploy.
#
# An app takes GitHub, Stripe and Shopify webhooks through three inboxes
# with the default retry settings (10 s, doubling, up to 1 h). The app is
# stopped for 60 seconds, as a deploy would, while a webhook comes in every
# 4 seconds. Then the app comes back and the script waits until every event
# is delivered. Everything runs on 127.0.0.1 with test secrets made for the
# run; nothing contacts GitHub, Stripe or Shopify.
#
# usage: results/case-studies/inbox-deploy.sh  > results/case-studies/inbox-deploy.log

source "$(dirname "$0")/lib/common.sh"
getbp
W=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null; sleep 0.3; rm -rf "$W"' EXIT
exec > >(sed -u "s#$W#/work#g; s#$BP#bareproxy#g") 2>&1
PORT=$(freeport); APP=$(freeport)
for k in github stripe shopify; do head -c 24 /dev/urandom | base64 >"$W/$k-secret"; done
cat >"$W/bareproxy.conf" <<EOF
global
  admin $W/admin.sock
  state $W/state
  trace-log $W/requests.log 10MB 2

site http://hooks.test:$PORT
  route /hooks/github -> inbox github
  route /hooks/stripe -> inbox stripe
  route /hooks/shopify -> inbox shopify

inbox github
  deliver app
  signature github $W/github-secret
  key-header X-GitHub-Delivery

inbox stripe
  deliver app
  signature stripe $W/stripe-secret

inbox shopify
  deliver app
  signature shopify $W/shopify-secret
  key-header X-Shopify-Webhook-Id

pool app
  backend 127.0.0.1:$APP
EOF

export CASE_START=$(date +%s.%N) HOOK_HOST=hooks.test:$PORT
startapp() { python3 "$LIB/app.py" "$APP" "$W/app.log" "$W/seen" "$W/mode" & APPPID=$!; waitport "$APP"; }
send() { python3 "$LIB/send.py" "http://127.0.0.1:$PORT/hooks/$1" "$1" "$W/$1-secret" "$2" | tee -a "$W/sent"; }

echo "# Case study: webhooks during an app deploy"
echo "# $("$BP" version), $(date -u '+%Y-%m-%d %H:%M:%S UTC'), $(uname -sm)"
say "Config (secrets are random per run; the paths are a temp folder)"
sed "s#$W#/work#g" "$W/bareproxy.conf"

"$BP" run "$W/bareproxy.conf" >"$W/server.log" 2>&1 &
waitport "$PORT"
startapp

say "Webhooks while the app is up"
send github 1; send stripe 2; send shopify 3
sleep 2

say "Deploy: the app stops for 60 s; a webhook every 4 s meanwhile"
kill $APPPID; wait $APPPID 2>/dev/null
echo "t=$(python3 -c "import time;print(f'{time.time()-$CASE_START:6.1f}')")s  app stopped"
kinds=(github stripe shopify)
for i in $(seq 4 18); do
	send "${kinds[$((i % 3))]}" "$i"
	sleep 4
done
startapp
echo "t=$(python3 -c "import time;print(f'{time.time()-$CASE_START:6.1f}')")s  app back"

say "Inbox while waiting for the retries"
show "$BP" inbox -c "$W/bareproxy.conf" list github
# Wait until nothing is pending (at most 3 minutes).
for _ in $(seq 180); do
	n=$("$BP" inbox -c "$W/bareproxy.conf" --json list | jq '[.. | objects | select(has("status")) | select(.status=="pending" or .status=="delivering")] | length')
	[ "$n" = 0 ] && break
	sleep 1
done
echo "t=$(python3 -c "import time;print(f'{time.time()-$CASE_START:6.1f}')")s  nothing pending"

say "Inbox after the app came back"
show "$BP" inbox -c "$W/bareproxy.conf" list

say "What the app received (one line per delivery)"
cat "$W/app.log"

say "Summary"
python3 - "$W/app.log" "$W/sent" <<'PY'
import json, sys, collections
recs = [json.loads(l) for l in open(sys.argv[1])]
sent = [l.split() for l in open(sys.argv[2])]
print(f"webhooks sent: {len(sent)}, answered 202: {sum(1 for x in sent if x[4] == '202')}")
sent_at = {x[2]: float(x[0][2:-1]) for x in sent}
ms = sorted(int(x[-2].strip('(')) for x in sent)
print(f"time to the 202: median {ms[len(ms)//2]} ms, longest {ms[-1]} ms")
by = collections.defaultdict(list)
for r in recs: by[r["inbox_id"]].append(r)
processed = sum(1 for r in recs if r["result"] == "200 processed")
print(f"deliveries: {len(recs)}, distinct events: {len(by)}, processed: {processed}")
print(f"events delivered more than once: {sum(1 for v in by.values() if len(v) > 1)}")
print(f"deliveries marked as replays: {sum(1 for r in recs if r['replay'])}")
first = {}
for r in recs: first.setdefault(r["sender_event"], r["t"])
waits = sorted(first[e] - sent_at[e] for e in sent_at if e in first)
print(f"events never delivered: {sum(1 for e in sent_at if e not in first)}")
print(f"wait from 202 to first delivery: shortest {waits[0]:.1f} s, longest {waits[-1]:.1f} s")
print("attempts per event:", dict(sorted(collections.Counter(max(x['attempt'] for x in v) for v in by.values()).items())))
PY
