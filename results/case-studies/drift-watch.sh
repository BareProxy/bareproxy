#!/usr/bin/env bash
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# Case study 4: a DNS move goes wrong, and the drift watcher sees it.
#
# Six names under .test, served by lib/fakenet.py: a DNS resolver and an
# HTTPS server on 127.0.0.1 with certificates from two test CAs. The
# sandbox the case study was recorded in can't reach public DNS (every name
# answers NXDOMAIN) and its HTTPS is intercepted, so real hosts were out;
# no real domain is checked.
#
# Four checks with `bareproxy drift`, the watch changing in between:
#   1. the baseline, with the standing problems it already has;
#   2. after a nameserver move that changed the MX and CAA records too, a
#      certificate renewed by another CA, one flapping answer and one
#      lookup that times out;
#   3. the next check with nothing changed;
#   4. after HSTS and CAA are fixed and the resolver answers again.
# Then the history.
#
# usage: results/case-studies/drift-watch.sh  > results/case-studies/drift-watch.log

source "$(dirname "$0")/lib/common.sh"
getbp
W=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null; sleep 0.3; rm -rf "$W"' EXIT
exec > >(sed -u "s#$W#/work#g; s#$BP#bareproxy#g") 2>&1
DNS=$(freeport); TLS=$(freeport)
mkdir -p "$W/net"
c() { python3 "$LIB/certs.py" "$@"; }
c ca "$W/net" old "Acme Old Test CA"
c ca "$W/net" new "Second Test CA"
cat "$W/net/old-ca.pem" "$W/net/new-ca.pem" >"$W/ca-bundle.pem"
c leaf "$W/net" old acme 60 acme.test
c leaf "$W/net" old www 80 www.acme.test
c leaf "$W/net" old shop 9 shop.acme.test
c leaf "$W/net" old blog 70 blog.acme.test
c leaf "$W/net" old api 50 api.acme.test
c leaf "$W/net" old status 40 status.otherco.test

# zone STAGE: write zone.json for that stage.
zone() {
	python3 - "$W/net" "$1" <<'PY'
import json, sys
d, stage = sys.argv[1], int(sys.argv[2])
H = "max-age=31536000; includeSubDomains"
dns = {
  "acme.test": {"A": ["127.0.0.1"], "NS": ["ns1.dnshost-a.test", "ns2.dnshost-a.test"],
                "MX": ["10 mx1.mailhost.test", "20 mx2.mailhost.test"],
                "TXT": ["v=spf1 include:mailhost.test -all"], "CAA": ['0 issue "old-ca.test"']},
  "www.acme.test": {"A": ["127.0.0.1"], "MX": []},
  "shop.acme.test": {"A": ["127.0.0.1"]},
  "blog.acme.test": {"A": ["127.0.0.1"]},
  "api.acme.test": {"A": ["127.0.0.1"]},
  "status.otherco.test": {"A": ["127.0.0.1"]},
  "otherco.test": {"NS": ["ns.otherco.test"]},
}
tls = {n: {"cert": c + ".pem", "key": c + ".key", "hsts": H} for n, c in
       [("acme.test", "acme"), ("www.acme.test", "www"), ("shop.acme.test", "shop"),
        ("blog.acme.test", "blog"), ("api.acme.test", "api"), ("status.otherco.test", "status")]}
tls["blog.acme.test"]["hsts"] = ""
z = {"dns": dns, "tls": tls, "drop": [], "flap": {}}
if stage >= 2:
    # The move to a new DNS host: new nameservers, and the new zone was
    # typed in by hand with the MX and CAA records wrong.
    dns["acme.test"]["NS"] = ["ns1.cheapdns.test", "ns2.cheapdns.test"]
    dns["acme.test"]["MX"] = ["10 mx1.mailhost.test"]
    dns["acme.test"]["CAA"] = ['0 issue "new-ca.test"']
    tls["shop.acme.test"].update(cert="shop2.pem", key="shop2.key")
if stage == 2:
    z["flap"] = {"api.acme.test/A": ["127.0.0.9"]}
    z["drop"] = ["www.acme.test/MX"]
if stage >= 4:
    tls["blog.acme.test"]["hsts"] = H
    dns["otherco.test"]["CAA"] = ['0 issue "new-ca.test"']
json.dump(z, open(d + "/zone.json", "w"), indent=1)
PY
}

cat >"$W/drift.conf" <<EOF
global
  state $W/state

drift acme
  domains acme.test www.acme.test shop.acme.test blog.acme.test
  domains api.acme.test status.otherco.test
  resolver 127.0.0.1:$DNS
  port $TLS
  ca $W/ca-bundle.pem
  timeout 2s
EOF

echo "# Case study: a DNS move that went wrong"
echo "# $("$BP" version), $(date -u '+%Y-%m-%d %H:%M:%S UTC'), $(uname -sm)"
echo "# DNS and HTTPS from lib/fakenet.py on 127.0.0.1 (resolver port $DNS, TLS port $TLS)"
say "Config"
cat "$W/drift.conf"

zone 1
python3 "$LIB/fakenet.py" "$W/net" "$DNS" "$TLS" >"$W/fakenet.out" 2>&1 &
waitport "$TLS"

say "Check 1: the baseline"
show "$BP" drift -c "$W/drift.conf"

zone 2
c leaf "$W/net" new shop2 90 shop.acme.test
say "Check 2: after the move to the new DNS host"
echo "Changed since check 1: NS, MX and CAA on acme.test; shop.acme.test renewed by the second CA;"
echo "api.acme.test answers 127.0.0.9 once; the resolver drops the MX question for www.acme.test."
show "$BP" drift -c "$W/drift.conf"

zone 3
say "Check 3: nothing changed since check 2"
show "$BP" drift -c "$W/drift.conf"

zone 4
say "Check 4: HSTS on blog.acme.test, a CAA record on otherco.test"
show "$BP" drift -c "$W/drift.conf"

say "The history"
show "$BP" drift -c "$W/drift.conf" --history

say "Check 4 again as JSON, summed up with jq"
"$BP" drift -c "$W/drift.conf" --json | jq -c '.[] | {watch, domains, clean, alerts: (.alerts | length), unchecked: (.unchecked | length)}'
echo "(exit code ${PIPESTATUS[0]})"

say "Questions the resolver got over the four checks and the JSON run"
awk '{print $2}' "$W/net/fakenet.log" | wc -l | sed 's/^/total questions: /'
grep -c dropped "$W/net/fakenet.log" | sed 's/^/dropped (no reply): /'
