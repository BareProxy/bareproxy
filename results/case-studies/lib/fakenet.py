#!/usr/bin/env python3
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# fakenet.py: a stand-in for the internet, for the drift case studies.
#
# The sandbox the case studies were recorded in has no working public DNS
# (every name answers NXDOMAIN) and its HTTPS goes through an intercepting
# proxy, so the watcher can't see real records or certificates there. This
# script gives it a small world of its own instead, written for the case
# studies and separate from the repo's test stubs:
#
#   - a DNS resolver on 127.0.0.1 (UDP and TCP) that answers from zone.json,
#     read again on every question, so a scenario changes a record by
#     rewriting the file;
#   - an HTTPS server on 127.0.0.1 and 127.0.0.2 that shows each name the
#     certificate and Strict-Transport-Security header zone.json gives it.
#
# zone.json:
#   {"dns":  {"name": {"A": ["127.0.0.1"], "MX": ["10 mx.example.test"],
#                      "NS": [...], "TXT": ["v=spf1 -all"],
#                      "CAA": ["0 issue \"ca.test\""], "AAAA": [...]}},
#    "drop": ["name/TYPE"],          no reply at all (the watcher times out)
#    "servfail": ["name/TYPE"],      SERVFAIL
#    "flap": {"name/TYPE": [...]},   this answer once, then the normal one
#    "tls":  {"name": {"cert": "file.pem", "key": "file.key",
#                      "hsts": "max-age=31536000" or ""}}}
#
# A name with no entry in "dns" is NXDOMAIN. Every question is logged.
#
# usage: fakenet.py DIR DNSPORT TLSPORT   (DIR holds zone.json)

import json, os, socket, socketserver, ssl, struct, sys, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

DIR, DNSPORT, TLSPORT = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
TYPES = {1: "A", 2: "NS", 5: "CNAME", 15: "MX", 16: "TXT", 28: "AAAA", 257: "CAA"}
flapped = set()
lock = threading.Lock()
log = open(os.path.join(DIR, "fakenet.log"), "a", buffering=1)


def zone():
    with open(os.path.join(DIR, "zone.json")) as f:
        return json.load(f)


def enc_name(n):
    out = b""
    for label in n.rstrip(".").split("."):
        if label:
            out += bytes([len(label)]) + label.encode()
    return out + b"\0"


def rdata(typ, rec):
    if typ == "A":
        return socket.inet_aton(rec)
    if typ == "AAAA":
        return socket.inet_pton(socket.AF_INET6, rec)
    if typ in ("NS", "CNAME"):
        return enc_name(rec)
    if typ == "MX":
        pref, host = rec.split(None, 1)
        return struct.pack(">H", int(pref)) + enc_name(host)
    if typ == "TXT":
        b = rec.encode()
        return b"".join(bytes([len(b[i:i + 255])]) + b[i:i + 255] for i in range(0, max(len(b), 1), 255))
    if typ == "CAA":
        flags, tag, value = rec.split(None, 2)
        value = value.strip('"').encode()
        return bytes([int(flags), len(tag)]) + tag.encode() + value
    raise ValueError(typ)


def answer(msg):
    qid, flags, qd = struct.unpack(">HHH", msg[:6])
    i, labels = 12, []
    while msg[i]:
        labels.append(msg[i + 1:i + 1 + msg[i]].decode().lower())
        i += 1 + msg[i]
    qname = ".".join(labels)
    qtype, qclass = struct.unpack(">HH", msg[i + 1:i + 5])
    question = msg[12:i + 5]
    typ = TYPES.get(qtype, str(qtype))
    key = qname + "/" + typ
    z = zone()
    rcode, recs = 0, []
    with lock:
        if key in z.get("drop", []):
            log.write(f"{time.time():.3f} {key} dropped\n")
            return None
        if key in z.get("servfail", []):
            rcode = 2
        elif key in z.get("flap", {}) and key not in flapped:
            flapped.add(key)
            recs = z["flap"][key]
        elif qname not in z["dns"]:
            rcode = 3
        else:
            recs = z["dns"][qname].get(typ, [])
        log.write(f"{time.time():.3f} {key} rcode={rcode} {recs}\n")
    out = struct.pack(">HHHHHH", qid, 0x8180 | rcode | (flags & 0x0100), 1, len(recs), 0, 0) + question
    for r in recs:
        rd = rdata(typ, r)
        out += enc_name(qname) + struct.pack(">HHIH", qtype, 1, 300, len(rd)) + rd
    return out


class UDP(socketserver.BaseRequestHandler):
    def handle(self):
        data, sock = self.request
        reply = answer(data)
        if reply is not None:
            sock.sendto(reply, self.client_address)


class TCP(socketserver.BaseRequestHandler):
    def handle(self):
        try:
            n = struct.unpack(">H", self.request.recv(2))[0]
            data = b""
            while len(data) < n:
                data += self.request.recv(n - len(data))
            reply = answer(data)
            if reply is not None:
                self.request.sendall(struct.pack(">H", len(reply)) + reply)
        except Exception:
            pass


contexts = {}


def context_for(name):
    t = zone().get("tls", {}).get(name)
    if not t:
        return None
    cert, key = os.path.join(DIR, t["cert"]), os.path.join(DIR, t["key"])
    k = (cert, os.path.getmtime(cert))
    if k not in contexts:
        c = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        c.load_cert_chain(cert, key)
        contexts[k] = c
    return contexts[k]


def sni(sock, name, ctx):
    c = context_for(name or "")
    if c is None:
        return ssl.ALERT_DESCRIPTION_UNRECOGNIZED_NAME
    sock.context = c


class Page(BaseHTTPRequestHandler):
    def do_GET(self):
        host = (self.headers.get("Host") or "").split(":")[0]
        hsts = zone().get("tls", {}).get(host, {}).get("hsts", "")
        self.send_response(200)
        if hsts:
            self.send_header("Strict-Transport-Security", hsts)
        self.send_header("Content-Length", "2")
        self.end_headers()
        self.wfile.write(b"ok")

    def log_message(self, *a):
        pass


def tls_server(addr):
    base = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    base.sni_callback = sni
    first = next(iter(zone()["tls"].values()))
    base.load_cert_chain(os.path.join(DIR, first["cert"]), os.path.join(DIR, first["key"]))
    srv = ThreadingHTTPServer((addr, TLSPORT), Page)
    srv.socket = base.wrap_socket(srv.socket, server_side=True, do_handshake_on_connect=False)
    srv.handle_error = lambda *a: None
    threading.Thread(target=srv.serve_forever, daemon=True).start()


socketserver.ThreadingUDPServer.allow_reuse_address = True
socketserver.ThreadingTCPServer.allow_reuse_address = True
udp = socketserver.ThreadingUDPServer(("127.0.0.1", DNSPORT), UDP)
tcp = socketserver.ThreadingTCPServer(("127.0.0.1", DNSPORT), TCP)
threading.Thread(target=udp.serve_forever, daemon=True).start()
threading.Thread(target=tcp.serve_forever, daemon=True).start()
tls_server("127.0.0.1")
tls_server("127.0.0.2")
print("fakenet ready", flush=True)
while True:
    time.sleep(3600)
