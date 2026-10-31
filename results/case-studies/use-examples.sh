#!/usr/bin/env bash
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# use-examples.sh: runs each recipe of the "use examples" post once and
# records what it printed. Local only: the webhooks are signed with test
# secrets made for the run, and the drift recipes read DNS and HTTPS from
# lib/fakenet.py on 127.0.0.1 (see drift-watch.sh for why).
#
# usage: results/case-studies/use-examples.sh  > results/case-studies/use-examples.log

source "$(dirname "$0")/lib/common.sh"
getbp
W=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null; sleep 0.3; rm -rf "$W"' EXIT
exec > >(sed -u "s#$W#/work#g; s#$BP#bareproxy#g") 2>&1
PORT=$(freeport); APP=$(freeport); DNS=$(freeport); TLS=$(freeport); ALERT=$(freeport)
mkdir -p "$W/etc"
head -c 24 /dev/urandom | base64 >"$W/etc/stripe-secret"
head -c 24 /dev/urandom | base64 >"$W/etc/github-secret"
export CASE_START=$(date +%s.%N) HOOK_HOST=hooks.test:$PORT
el() { python3 -c "import time;print(f't={time.time()-$CASE_START:.1f}s')"; }
startapp() { python3 "$LIB/app.py" "$APP" "$W/app.log" "$W/seen" "$W/mode" & APPPID=$!; waitport "$APP"; }
send() { local k=$1; shift; python3 "$LIB/send.py" "http://127.0.0.1:$PORT/hooks/$k" "$k" "$W/etc/$k-secret" "$@"; }
idle() { for _ in $(seq 240); do n=$("$BP" inbox -c "$W/hooks.conf" --json list | jq '[.. | objects | select(has("status")) | select(.status=="pending" or .status=="delivering")] | length'); [ "$n" = 0 ] && return; sleep 0.5; done; }

cat >"$W/hooks.conf" <<EOF
global
  admin $W/admin.sock
  state $W/state
  trace-log $W/requests.log 10MB 2

site http://hooks.test:$PORT
  route /hooks/stripe -> inbox stripe
  route /hooks/github -> inbox github

inbox stripe
  deliver app
  signature stripe $W/etc/stripe-secret
  retry 5s 2m

inbox github
  deliver app
  signature github $W/etc/github-secret
  key-header X-GitHub-Delivery
  retry 2s 5s
  give-up 15s

pool app
  backend 127.0.0.1:$APP
  health /healthz
EOF

echo "# Use examples: each recipe run once"
echo "# $("$BP" version), $(date -u '+%Y-%m-%d %H:%M:%S UTC'), $(uname -sm)"
say "Recipes 1 to 3 share this config"
cat "$W/hooks.conf"
show "$BP" check "$W/hooks.conf"
startapp
"$BP" run "$W/hooks.conf" >"$W/server.log" 2>&1 &
waitport "$PORT"
sleep 1

say "Recipe 1: an inbox in front of an app that restarts on every deploy"
kill $APPPID; wait $APPPID 2>/dev/null
echo "$(el) app stopped"
send stripe 1
sleep 15
startapp
echo "$(el) app back"
idle
echo "$(el) delivered:"
cat "$W/app.log"
: >"$W/app.log"

say "Recipe 2: Stripe signatures"
send stripe 2
send stripe 3 --bad-signature
send stripe 4 --stale
idle

say "Recipe 3: replay what failed during an outage"
echo fail >"$W/mode"
echo "$(el) the app answers 500"
send github 5; send github 6
for _ in $(seq 60); do
	f=$("$BP" inbox -c "$W/hooks.conf" --json list github --status failed | jq '[.. | objects | select(has("status")) | select(.status=="failed")] | length')
	[ "$f" = 2 ] && break
	sleep 1
done
echo ok >"$W/mode"
show "$BP" inbox -c "$W/hooks.conf" list github --status failed
show "$BP" inbox -c "$W/hooks.conf" replay github --failed
idle
echo "Deliveries after the replay:"
jq -c 'select(.replay=="operator")' "$W/app.log"
ID=$(jq -r 'select(.replay=="operator") | .inbox_id' "$W/app.log" | head -1)
show "$BP" inbox -c "$W/hooks.conf" show github "$ID"

# --- Drift recipes ---------------------------------------------------------
mkdir -p "$W/net"
python3 "$LIB/certs.py" ca "$W/net" t "Recipe Test CA"
python3 "$LIB/certs.py" leaf "$W/net" t www 60 www.example.test
python3 "$LIB/certs.py" leaf "$W/net" t shop 12 shop.example.test
python3 - "$W/net" <<'PY'
import json, sys
H = "max-age=31536000"
z = {"dns": {"example.test": {"NS": ["ns1.dns.test"], "CAA": ['0 issue "ca.test"']},
             "www.example.test": {"A": ["127.0.0.1"]}, "shop.example.test": {"A": ["127.0.0.1"]}},
     "tls": {"www.example.test": {"cert": "www.pem", "key": "www.key", "hsts": H},
             "shop.example.test": {"cert": "shop.pem", "key": "shop.key", "hsts": H}}}
json.dump(z, open(sys.argv[1] + "/zone.json", "w"))
PY
python3 "$LIB/fakenet.py" "$W/net" "$DNS" "$TLS" >"$W/fakenet.out" 2>&1 &
waitport "$TLS"

cat >"$W/drift-ci.conf" <<EOF
global
  state $W/ci-state

drift sites
  domains www.example.test shop.example.test
  resolver 127.0.0.1:$DNS
  port $TLS
  ca $W/net/t-ca.pem
EOF
cat >"$W/drift-check.sh" <<'EOF'
#!/bin/sh
bareproxy drift -c drift-ci.conf
case $? in
  0) echo "drift: all clear" ;;
  3) echo "drift: some parts couldn't be checked, see above" ;;
  4) echo "drift: alerts, failing the job"; exit 1 ;;
  *) echo "drift: the check couldn't run"; exit 1 ;;
esac
EOF

say "Recipe 4: drift in CI, failing the job on alerts"
cat "$W/drift-ci.conf"
echo
echo "drift-check.sh:"
cat "$W/drift-check.sh"
echo
echo '$ ./drift-check.sh'
(cd "$W" && PATH="$(dirname "$BP"):$PATH" sh drift-check.sh; echo "(job exit code $?)")
echo
echo "With expiry 7d, the certificate with 11 days left is outside the window:"
sed -i 's/^  ca /  expiry 7d\n  ca /' "$W/drift-ci.conf"
(cd "$W" && PATH="$(dirname "$BP"):$PATH" sh drift-check.sh; echo "(job exit code $?)")

say "Recipe 5: drift every hour inside the server, with a webhook alert"
python3 "$LIB/catch.py" "$ALERT" "$W/alerts.jsonl" &
waitport "$ALERT"
cat >"$W/server-drift.conf" <<EOF
global
  admin $W/admin2.sock
  state $W/state2

site http://status.test:$(freeport)
  route /* -> respond 200 "ok"

drift sites
  domains www.example.test shop.example.test
  resolver 127.0.0.1:$DNS
  port $TLS
  ca $W/net/t-ca.pem
  every 1h
  webhook http://127.0.0.1:$ALERT/bareproxy
EOF
cat "$W/server-drift.conf"
"$BP" run "$W/server-drift.conf" >"$W/server2.log" 2>&1 &
for _ in $(seq 40); do [ -s "$W/alerts.jsonl" ] && break; sleep 0.5; done
echo
echo "The alert receiver got:"
jq -c '{watch, domains, alerts: [.alerts[] | {domain, text}], unchecked: (.unchecked | length)}' "$W/alerts.jsonl"
echo
show "$BP" events -c "$W/server-drift.conf"

say "Recipe 6: reading the history"
python3 - "$W/net" <<'PY'
import json, sys
p = sys.argv[1] + "/zone.json"
z = json.load(open(p))
z["dns"]["www.example.test"]["A"] = ["127.0.0.2"]
json.dump(z, open(p, "w"))
PY
echo "(www.example.test moved to 127.0.0.2)"
show "$BP" drift -c "$W/drift-ci.conf"
show "$BP" drift -c "$W/drift-ci.conf" --history
echo
echo '$ bareproxy drift -c drift-ci.conf --history --json | jq ...'
"$BP" drift -c "$W/drift-ci.conf" --history --json | tee "$W/hist.json" | jq -c '.. | objects | select(has("before") and has("after"))' | head -5
echo
echo "(the raw JSON, first 40 lines)"
jq . "$W/hist.json" | head -40
