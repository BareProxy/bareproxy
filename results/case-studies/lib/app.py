#!/usr/bin/env python3
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# app.py: the app behind the inbox in the case studies. It takes webhooks,
# writes one JSON line per delivery to LOG, and handles each event once:
# it keeps the BareProxy-Inbox-Id values it has processed in SEEN (a file,
# so a restart or a deploy doesn't forget them) and answers a repeat with
# 200 without processing it again. That is the idempotent handling the
# inbox manual asks of apps.
#
# MODE is a file read on every request:
#   ok         answer 200 (the default when the file is missing)
#   fail       answer 500, as a bad deploy would
#   slow N     wait N seconds, then answer 200
#
# usage: app.py PORT LOG SEEN MODE

import json, os, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT, LOG, SEEN, MODE = int(sys.argv[1]), sys.argv[2], sys.argv[3], sys.argv[4]
START = float(os.environ.get("CASE_START", time.time()))


def mode():
    try:
        return open(MODE).read().split()
    except FileNotFoundError:
        return ["ok"]


def seen():
    try:
        return set(open(SEEN).read().split())
    except FileNotFoundError:
        return set()


class App(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        m = mode()
        if m[0] == "slow":
            time.sleep(float(m[1]))
        h = self.headers
        iid = h.get("BareProxy-Inbox-Id", "")
        try:
            sender_id = json.loads(body).get("id", "")
        except Exception:
            sender_id = ""
        rec = {
            "t": round(time.time() - START, 1),
            "path": self.path,
            "inbox_id": iid,
            "attempt": int(h.get("BareProxy-Inbox-Attempt") or 0),
            "replay": h.get("BareProxy-Inbox-Replay", ""),
            "sender_event": h.get("X-GitHub-Delivery") or h.get("X-Shopify-Webhook-Id") or sender_id,
        }
        if m[0] == "fail":
            rec["result"] = "500 (bad deploy)"
            code = 500
        elif iid in seen():
            rec["result"] = "200 duplicate, not processed again"
            code = 200
        else:
            with open(SEEN, "a") as f:
                f.write(iid + "\n")
            rec["result"] = "200 processed"
            code = 200
        with open(LOG, "a") as f:
            f.write(json.dumps(rec) + "\n")
        self.send_response(code)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_GET(self):  # health checks
        self.send_response(200)
        self.send_header("Content-Length", "2")
        self.end_headers()
        self.wfile.write(b"ok")

    def log_message(self, *a):
        pass


ThreadingHTTPServer(("127.0.0.1", PORT), App).serve_forever()
