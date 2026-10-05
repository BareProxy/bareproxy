#!/usr/bin/env bash
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# acme-test.sh: BareProxy gets an automatic certificate from Pebble, Let's
# Encrypt's test CA, and serves a page with it. Pebble really validates the
# TLS-ALPN-01 challenge on BareProxy's port; pebble-challtestsrv answers its
# DNS lookups with 127.0.0.1. Then curl fetches the page over HTTPS trusting
# Pebble's root, status and events show the certificate, and a restart shows
# it comes back from the cache in the state folder.
#
# Pebble v2.8.0 from github.com/letsencrypt/pebble/releases (PEBBLE and
# PEBBLE_CHALLTESTSRV may name binaries already on disk). Later Pebble
# releases answer the finalize request without a Location header, which the
# ACME client in golang.org/x/crypto v0.57.0 needs. Needs Go (GO=path),
# openssl, curl and python3. Run from anywhere: live/acme-test.sh
set -euo pipefail
cd "$(dirname "$0")/.."
GO=${GO:-go}
work=$(mktemp -d)
pids=()
cleanup() { for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done; wait 2>/dev/null || true; rm -rf "$work"; }
trap cleanup EXIT
step() { printf '\n## %s\n' "$*"; }
show() { printf '\n$ %s\n' "$*"; "$@"; }
freeport() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }
waitfor() { for _ in $(seq 100); do "$@" >/dev/null 2>&1 && return 0; sleep 0.1; done; echo "gave up waiting for: $*"; exit 1; }

step "Pebble"
if [ -z "${PEBBLE:-}" ] || [ -z "${PEBBLE_CHALLTESTSRV:-}" ]; then
	for n in pebble pebble-challtestsrv; do
		curl -sSL --max-time 120 -o "$work/$n.tar.gz" "https://github.com/letsencrypt/pebble/releases/download/v2.8.0/$n-linux-amd64.tar.gz"
		tar -xzf "$work/$n.tar.gz" -C "$work"
		sha256sum "$work/$n.tar.gz" | sed "s#$work/##"
	done
	PEBBLE=$work/pebble-linux-amd64/linux/amd64/pebble
	PEBBLE_CHALLTESTSRV=$work/pebble-challtestsrv-linux-amd64/linux/amd64/pebble-challtestsrv
	chmod +x "$PEBBLE" "$PEBBLE_CHALLTESTSRV" # the release tarballs don't set the bit
fi
"$PEBBLE" -version
api=$(freeport) mgmt=$(freeport) dns=$(freeport) cts=$(freeport) port=$(freeport) unused=$(freeport)
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 7 -subj /CN=localhost \
	-addext subjectAltName=DNS:localhost -keyout "$work/pebble-key.pem" -out "$work/pebble-cert.pem" 2>/dev/null
cat >"$work/pebble.json" <<EOF
{"pebble": {"listenAddress": "127.0.0.1:$api", "managementListenAddress": "127.0.0.1:$mgmt",
  "certificate": "$work/pebble-cert.pem", "privateKey": "$work/pebble-key.pem", "httpPort": $unused, "tlsPort": $port}}
EOF
dnsflag=-dns01
"$PEBBLE_CHALLTESTSRV" -h 2>&1 | grep -q -- -dnsserver && dnsflag=-dnsserver
"$PEBBLE_CHALLTESTSRV" -defaultIPv4 127.0.0.1 -defaultIPv6 "" $dnsflag 127.0.0.1:$dns -doh "" -http01 "" -https01 "" \
	-tlsalpn01 "" -management 127.0.0.1:$cts >"$work/challtestsrv.log" 2>&1 &
pids+=($!)
PEBBLE_VA_NOSLEEP=1 PEBBLE_WFE_NONCEREJECT=0 NO_PROXY='*' no_proxy='*' \
	"$PEBBLE" -config "$work/pebble.json" -dnsserver 127.0.0.1:$dns >"$work/pebble.log" 2>&1 &
pids+=($!)
CURL=(curl -sS --noproxy '*')
waitfor "${CURL[@]}" --cacert "$work/pebble-cert.pem" "https://localhost:$api/dir"
"${CURL[@]}" --cacert "$work/pebble-cert.pem" -o "$work/pebble-root.pem" "https://localhost:$mgmt/roots/0"
echo "Pebble's directory: https://localhost:$api/dir (always-valid off: it validates for real)"

step "BareProxy"
(cd src && "$GO" build -o "$work/bareproxy" ./cmd/bareproxy)
conf=$work/bareproxy.conf
cat >"$conf" <<EOF
global
  admin $work/admin.sock
  state $work/state
  trace-log $work/trace.log
  acme-ca https://localhost:$api/dir
  acme-email ops@auto.test

site auto.test:$port
  route /* -> respond 200 "hello over ACME"
EOF
sed "s#$work#WORK#g" "$conf"
start() {
	SSL_CERT_FILE=$work/pebble-cert.pem "$work/bareproxy" run "$conf" >>"$work/bareproxy.log" 2>&1 &
	bp=$!
	pids+=($bp)
	waitfor "${CURL[@]}" --unix-socket "$work/admin.sock" http://admin/status
}
start
BPC=("$work/bareproxy")
show "${BPC[@]}" explain --config "$conf" GET "https://auto.test:$port/" | sed "s#$work#WORK#g"

step "The first HTTPS request: BareProxy gets the certificate during the handshake"
HTTPS=("${CURL[@]}" --resolve "auto.test:$port:127.0.0.1" --cacert "$work/pebble-root.pem")
printf '\n$ curl --cacert pebble-root.pem https://auto.test:%s/\n' "$port"
"${HTTPS[@]}" -o "$work/body" -w 'HTTP %{http_code}, %{http_version}, took %{time_total} s\n' "https://auto.test:$port/"
echo "  body: $(cat "$work/body")"
cert() { openssl s_client -connect "127.0.0.1:$port" -servername auto.test </dev/null 2>/dev/null | openssl x509 -noout -ext subjectAltName -issuer -serial -enddate; }
cert | tee "$work/cert1" | sed 's/^/  /'
grep -E "validation|VALID|Issued" "$work/pebble.log" | sed 's/^/  pebble: /' | head -8

step "Status, events and the cache"
printf '\n$ bareproxy status (the certificates part)\n'
"${BPC[@]}" status --config "$conf" | grep -iA2 '^certificates' || true
printf '\n$ bareproxy events (the certificate events)\n'
"${BPC[@]}" events --config "$conf" | grep -i certificate || true
printf '\n$ ls WORK/state/certs\n'
ls "$work/state/certs"

step "Restart: the certificate comes from the cache, so Pebble issues nothing new"
kill "$bp"
wait "$bp" 2>/dev/null || true
issued=$(grep -c "Issued certificate" "$work/pebble.log")
start
"${HTTPS[@]}" -o /dev/null -w 'HTTP %{http_code}\n' "https://auto.test:$port/"
cert >"$work/cert2"
if cmp -s "$work/cert1" "$work/cert2" && [ "$(grep -c "Issued certificate" "$work/pebble.log")" = "$issued" ]; then
	echo "same certificate after the restart (serial $(grep serial "$work/cert2" | cut -d= -f2)), no new issue: PASS"
else
	echo "the certificate changed after the restart: FAIL"
	exit 1
fi
echo
echo "acme-test: PASS"
