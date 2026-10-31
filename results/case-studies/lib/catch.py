#!/usr/bin/env python3
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# catch.py: an alert receiver for the drift webhook. Writes each POST body
# it gets to OUT, one JSON document a line.
#
# usage: catch.py PORT OUT

import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

PORT, OUT = int(sys.argv[1]), sys.argv[2]


class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        with open(OUT, "a") as f:
            f.write(json.dumps(json.loads(body)) + "\n")
        self.send_response(204)
        self.end_headers()

    def log_message(self, *a):
        pass


HTTPServer(("127.0.0.1", PORT), H).serve_forever()
