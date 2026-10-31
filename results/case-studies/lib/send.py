#!/usr/bin/env python3
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# send.py: sends one webhook to BareProxy in the format a real sender uses,
# signed with a test secret. It never contacts GitHub, Stripe or Shopify.
#
#   github   X-GitHub-Event, X-GitHub-Delivery (a GUID that stays the same
#            on GitHub's redeliveries), X-Hub-Signature-256: sha256=<hex>
#   stripe   Stripe-Signature: t=<unix time>,v1=<hex HMAC of "t.body">;
#            the event ID is in the body ("id": "evt_..."), not a header
#   shopify  X-Shopify-Topic, X-Shopify-Webhook-Id (the same on retries),
#            X-Shopify-Hmac-Sha256: <base64>
#
# usage: send.py URL KIND SECRETFILE N [--bad-signature] [--stale] [--id ID]
# Prints one line: kind, the sender's event ID, the status, the inbox ID.

import base64, hashlib, hmac, json, os, sys, time, urllib.request, uuid

url, kind, secret_file, n = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
flags = sys.argv[5:]
secret = open(secret_file).read().strip().encode()
if "--bad-signature" in flags:
    secret = b"not-the-secret"
eid = flags[flags.index("--id") + 1] if "--id" in flags else None
START = float(os.environ.get("CASE_START", time.time()))
h = {"Content-Type": "application/json", "User-Agent": "case-study-sender"}
if os.environ.get("HOOK_HOST"):
    h["Host"] = os.environ["HOOK_HOST"]  # the site name; the URL names 127.0.0.1

if kind == "github":
    eid = eid or str(uuid.uuid4())
    body = json.dumps({"ref": "refs/heads/main", "after": hashlib.sha1(str(n).encode()).hexdigest(),
                       "repository": {"full_name": "acme/shop"}, "pusher": {"name": "dev"}, "seq": n}).encode()
    h["X-GitHub-Event"] = "push"
    h["X-GitHub-Delivery"] = eid
    h["X-Hub-Signature-256"] = "sha256=" + hmac.new(secret, body, hashlib.sha256).hexdigest()
elif kind == "stripe":
    eid = eid or "evt_" + uuid.uuid4().hex[:24]
    body = json.dumps({"id": eid, "object": "event", "type": "invoice.paid",
                       "data": {"object": {"id": "in_%06d" % n, "amount_paid": 1900 + n, "currency": "usd"}}}).encode()
    t = int(time.time()) - (600 if "--stale" in flags else 0)
    sig = hmac.new(secret, str(t).encode() + b"." + body, hashlib.sha256).hexdigest()
    h["Stripe-Signature"] = f"t={t},v1={sig}"
elif kind == "shopify":
    eid = eid or str(uuid.uuid4())
    body = json.dumps({"id": 820982911946154508 + n, "email": "buyer%d@example.test" % n,
                       "total_price": "%d.00" % (40 + n), "financial_status": "paid"}).encode()
    h["X-Shopify-Topic"] = "orders/create"
    h["X-Shopify-Webhook-Id"] = eid
    h["X-Shopify-Hmac-Sha256"] = base64.b64encode(hmac.new(secret, body, hashlib.sha256).digest()).decode()
else:
    sys.exit("unknown kind " + kind)

t0 = time.time()
req = urllib.request.Request(url, data=body, headers=h, method="POST")
try:
    with urllib.request.urlopen(req, timeout=10) as r:
        code, iid = r.status, r.headers.get("BareProxy-Inbox-Id", "")
except urllib.error.HTTPError as e:
    code, iid = e.code, e.headers.get("BareProxy-Inbox-Id", "")
except Exception as e:
    code, iid = "no answer (%s)" % type(e).__name__, ""
print(f"t={t0 - START:.1f}s {kind:8} {eid} -> {code} {iid} ({(time.time() - t0) * 1000:.0f} ms)", flush=True)
