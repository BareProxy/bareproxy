#!/usr/bin/env python3
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
"""Measures BareProxy against nginx with the same routes, on this machine.

Started by bench.sh. Everything here uses the Python standard library.

Layout of one run:
  - the server under test is pinned to one CPU, the load generator (wrk) and
    this script to the other, and the test API backend to the server's CPU
  - for each logging mode (off, file) both servers are started, idle memory is
    read before any request, the routes are checked, then each case is run
    several times (2 s warm-up, 10 s measured) with wrk and the servers taking
    turns, so slow changes on the machine hit both of them alike
  - every measured run is one RESULT line (JSON) in the log, after the raw
    wrk output; summarize.py turns the log into summary.md
"""
import atexit
import ctypes
import datetime
import http.client
import json
import os
import re
import resource
import shutil
import signal
import socket
import ssl
import statistics
import subprocess
import sys
import threading
import time

# The repository root: this script lives in live/bench/.
REPO = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..'))

HERE = os.path.dirname(os.path.abspath(__file__))


def env(name, default):
    return os.environ.get(name, default)


BAREPROXY = env("BAREPROXY", os.path.join(HERE, "bin", "bareproxy-stripped"))
NGINX = env("NGINX", "/usr/sbin/nginx")
WRK = env("WRK", "wrk")
TESTAPI = env("TESTAPI", os.path.join(HERE, "bin", "testapi"))
SITE = env("BENCH_SITE", os.path.join(HERE, "site", "public"))
DURATION = int(env("BENCH_DURATION", "10"))
WARMUP = int(env("BENCH_WARMUP", "2"))
RUNS = int(env("BENCH_RUNS", "3"))
CONNS = int(env("BENCH_CONNS", "50"))
SERVER_CPU = int(env("BENCH_SERVER_CPU", "0"))
LOAD_CPU = int(env("BENCH_LOAD_CPU", "1"))
BP_PORT = int(env("BP_PORT", "18080"))
NGX_PORT = int(env("NGX_PORT", "18081"))
API_PORT = int(env("API_PORT", "19001"))
MODES = env("BENCH_MODES", "off,file").split(",")   # off, file, tls (HTTPS with HTTP/2, logging off)
H2LOAD = env("H2LOAD", "h2load")
TRIES = int(env("BENCH_TRIES", "3"))                 # attempts per run when other processes disturb it
NOISE_PCT = float(env("BENCH_NOISE_PCT", "10"))      # other processes using more of a core than this: disturbed
MAX_SECS = float(env("BENCH_MAX_MINUTES", "40")) * 60  # no retries after this long
STARTED_AT = time.time()
ROUTES = env("BENCH_ROUTES", "home,file,404,api").split(",")
SERVERS = env("BENCH_SERVERS", "bareproxy,nginx").split(",")
DIRECT = env("BENCH_DIRECT", "1") == "1"
HOSTNAME = "bench.local"

RUN_DIR = os.path.join(HERE, "run")
NGX_DIR = os.path.join(HERE, "nginx")
RESULTS = os.path.join(HERE, "results")

# route name -> (path, expected status)
ROUTE_DEF = {
    "home": ("/", 200),
    "file": ("/images/plan-demo.png", 200),
    "404": ("/no-such-page/", 404),
    "api": ("/api/orders", 200),
}

CLK = os.sysconf("SC_CLK_TCK")
LIBC = ctypes.CDLL("libc.so.6", use_errno=True)


class Log:
    def __init__(self, path):
        self.path = path
        self.f = open(path, "w", buffering=1)

    def w(self, text=""):
        self.f.write(text + "\n")
        print(text, flush=True)

    def raw(self, text):
        self.f.write(text if text.endswith("\n") else text + "\n")


LOG = None
STARTED = []  # every process this script started


def die_with_parent():
    LIBC.prctl(1, signal.SIGKILL)  # PR_SET_PDEATHSIG


def spawn(cmd, out, extra_env=None):
    e = dict(os.environ)
    if extra_env:
        e.update(extra_env)
    p = subprocess.Popen(cmd, env=e, stdout=open(out, "w"), stderr=subprocess.STDOUT,
                         stdin=subprocess.DEVNULL, preexec_fn=die_with_parent)
    STARTED.append(p)
    return p


def children_of(pid):
    out = []
    for d in os.listdir("/proc"):
        if d.isdigit():
            try:
                with open(f"/proc/{d}/stat") as f:
                    s = f.read()
                if int(s[s.rindex(")") + 2:].split()[1]) == pid:
                    out.append(int(d))
            except (OSError, ValueError):
                pass
    return out


def stop(p, sig=signal.SIGTERM, wait=8):
    if p.poll() is not None:
        return
    kids = children_of(p.pid)
    try:
        p.send_signal(sig)
        p.wait(timeout=wait)
    except subprocess.TimeoutExpired:
        p.kill()
        p.wait()
    except ProcessLookupError:
        pass
    for k in kids:
        try:
            with open(f"/proc/{k}/comm") as f:
                if f.read().strip() == "nginx":
                    os.kill(k, signal.SIGKILL)
        except (OSError, ProcessLookupError):
            pass


def stop_all():
    for p in STARTED:
        stop(p)


atexit.register(stop_all)


def on_signal(signum, frame):
    raise SystemExit(128 + signum)


# ---------- /proc readers ----------

def rss_kb(pid, key="VmRSS"):
    try:
        with open(f"/proc/{pid}/status") as f:
            for line in f:
                if line.startswith(key + ":"):
                    return int(line.split()[1])
    except OSError:
        pass
    return 0


def proc_ticks(pid):
    try:
        with open(f"/proc/{pid}/stat") as f:
            s = f.read()
        r = s[s.rindex(")") + 2:].split()
        return int(r[11]) + int(r[12])
    except (OSError, ValueError):
        return 0


def pss_kb(pid):
    """Proportional set size: shared pages are split between the processes that share them."""
    try:
        with open(f"/proc/{pid}/smaps_rollup") as f:
            for line in f:
                if line.startswith("Pss:"):
                    return int(line.split()[1])
    except OSError:
        pass
    return 0


def tcp_passive_opens():
    """Connections accepted on this machine so far (all processes), from /proc/net/snmp."""
    try:
        with open("/proc/net/snmp") as f:
            rows = [l.split() for l in f if l.startswith("Tcp:")]
        return int(dict(zip(rows[0], rows[1]))["PassiveOpens"])
    except (OSError, KeyError, IndexError, ValueError):
        return 0


def cpu_stat():
    d = {}
    with open("/proc/stat") as f:
        for line in f:
            if line.startswith("cpu") and line[3:4].isdigit():
                p = line.split()
                v = list(map(int, p[1:9]))
                user, nice, system, idle, iowait, irq, softirq, steal = v
                busy = user + nice + system + irq + softirq + steal
                d[p[0]] = {"busy": busy, "total": busy + idle + iowait, "steal": steal, "softirq": softirq}
    return d


def loadavg():
    with open("/proc/loadavg") as f:
        return " ".join(f.read().split()[:3])


def listening(port):
    want = f":{port:04X}"
    for name in ("/proc/net/tcp", "/proc/net/tcp6"):
        try:
            with open(name) as f:
                next(f)
                for line in f:
                    p = line.split()
                    if p[3] == "0A" and p[1].endswith(want):
                        return True
        except OSError:
            pass
    return False


def wait_listening(port, p, secs=10):
    end = time.time() + secs
    while time.time() < end:
        if p.poll() is not None:
            raise RuntimeError(f"process {p.args[:4]} exited early with {p.returncode}")
        if listening(port):
            return
        time.sleep(0.02)
    raise RuntimeError(f"nothing listening on port {port} after {secs} s")


def port_free(port):
    return not listening(port)


# ---------- servers ----------

class Server:
    """A running server: its processes, port, and how to read its memory."""

    def __init__(self, name, port, proc, log_file=None):
        self.name, self.port, self.proc, self.log_file = name, port, proc, log_file

    def pids(self):
        if self.name == "nginx":
            return [self.proc.pid] + children_of(self.proc.pid)
        return [self.proc.pid]

    def rss(self, key="VmRSS"):
        return sum(rss_kb(p, key) for p in self.pids())

    def pss(self):
        return sum(pss_kb(p) for p in self.pids())

    def ticks(self):
        return sum(proc_ticks(p) for p in self.pids())


NGINX_CONF = """\
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
# Written by bench.py. Same routes as bareproxy-{mode}.conf.
worker_processes 1;
user root;
pid {ngx}/logs/nginx.pid;
error_log {ngx}/logs/error.log warn;

events {{
    worker_connections 4096;
}}

http {{
    include {ngx}/mime.types;
    default_type application/octet-stream;
    access_log {access};
    sendfile on;
    tcp_nopush on;
    keepalive_timeout 65;
    keepalive_requests 1000000;
    client_body_temp_path {ngx}/tmp/body;
    proxy_temp_path {ngx}/tmp/proxy;
    fastcgi_temp_path {ngx}/tmp/fastcgi;
    uwsgi_temp_path {ngx}/tmp/uwsgi;
    scgi_temp_path {ngx}/tmp/scgi;

    upstream api {{
        server 127.0.0.1:{api};
        keepalive 64;
        keepalive_requests 1000000;
    }}

    server {{
        listen {listen};
        server_name {host};
{ssl}        root {site};
        index index.html;
        error_page 404 /404.html;

        location = /404.html {{
            internal;
        }}

        location /api/ {{
            proxy_pass http://api/;
            proxy_http_version 1.1;
            proxy_set_header Connection "";
            proxy_set_header Host $http_host;
            proxy_set_header X-Forwarded-For $remote_addr;
            proxy_set_header X-Forwarded-Proto $scheme;
            proxy_set_header X-Forwarded-Host $http_host;
        }}

        location / {{
        }}
    }}
}}
"""

BAREPROXY_CONF = """\
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
# Written by bench.py. Same routes as nginx-{mode}.conf.
global
  admin {admin}
  trace-log {trace}
{extra}
site {addr}
{tls}  error 404 /404.html
  route /api/* -> api strip
  route /* -> files {site}

pool api
  backend 127.0.0.1:{api}
"""


def ensure_cert():
    """A self-signed ECDSA P-256 certificate for bench.local, made once and shared by both servers."""
    d = os.path.join(RUN_DIR, "tls")
    os.makedirs(d, exist_ok=True)
    crt, key = os.path.join(d, "bench.crt"), os.path.join(d, "bench.key")
    if not (os.path.exists(crt) and os.path.exists(key)):
        subprocess.run(["openssl", "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1",
                        "-nodes", "-keyout", key, "-out", crt, "-days", "30", "-subj", f"/CN={HOSTNAME}",
                        "-addext", f"subjectAltName=DNS:{HOSTNAME}"], check=True, capture_output=True)
    return crt, key


def write_configs(mode):
    os.makedirs(RUN_DIR, exist_ok=True)
    for d in ("logs", "tmp"):
        os.makedirs(os.path.join(NGX_DIR, d), exist_ok=True)
    shutil.copy("/etc/nginx/mime.types", os.path.join(NGX_DIR, "mime.types"))
    quiet = mode in ("off", "tls")
    trace = "off" if quiet else os.path.join(RUN_DIR, "bareproxy-trace.log")
    access = "off" if quiet else os.path.join(NGX_DIR, "logs", "access.log")
    crt = key = None
    if mode == "tls":
        crt, key = ensure_cert()
    extra = env("BP_EXTRA_GLOBAL", "")
    extra = "".join(f"  {l}\n" for l in extra.split(";") if l.strip())
    bp = BAREPROXY_CONF.format(mode=mode, admin=os.path.join(RUN_DIR, "admin.sock"), trace=trace,
                               extra=extra, host=HOSTNAME, addr=(f"{HOSTNAME}:{BP_PORT}" if crt else f"http://{HOSTNAME}:{BP_PORT}"),
                               tls=(f"  tls {crt} {key}\n" if crt else ""), site=SITE, api=API_PORT)
    ng = NGINX_CONF.format(mode=mode, ngx=NGX_DIR, access=access, api=API_PORT,
                           listen=(f"{NGX_PORT} ssl http2" if crt else f"{NGX_PORT}"),
                           ssl=(f"        ssl_certificate {crt};\n        ssl_certificate_key {key};\n"
                                f"        ssl_protocols TLSv1.2 TLSv1.3;\n" if crt else ""),
                           host=HOSTNAME, site=SITE)
    bpf = os.path.join(RUN_DIR, f"bareproxy-{mode}.conf")
    ngf = os.path.join(NGX_DIR, f"nginx-{mode}.conf")
    open(bpf, "w").write(bp)
    open(ngf, "w").write(ng)
    return bpf, ngf, (None if quiet else trace), (None if quiet else access)


def start_backend():
    p = spawn(["taskset", "-c", str(SERVER_CPU), TESTAPI, "api", f"127.0.0.1:{API_PORT}"],
              os.path.join(RUN_DIR, "testapi.out"))
    wait_listening(API_PORT, p)
    return p


def start_bareproxy(mode):
    bpf, _, trace, _ = write_configs(mode)
    LOG.w(f"$ GOMAXPROCS=1 taskset -c {SERVER_CPU} {BAREPROXY} run {bpf}")
    p = spawn(["taskset", "-c", str(SERVER_CPU), BAREPROXY, "run", bpf],
              os.path.join(RUN_DIR, f"bareproxy-{mode}.out"), {"GOMAXPROCS": "1"})
    wait_listening(BP_PORT, p)
    return Server("bareproxy", BP_PORT, p, trace)


def start_nginx(mode):
    _, ngf, _, access = write_configs(mode)
    cmd = ["taskset", "-c", str(SERVER_CPU), NGINX, "-p", NGX_DIR + "/", "-c", ngf,
           "-e", os.path.join(NGX_DIR, "logs", "startup-error.log"), "-g", "daemon off;"]
    LOG.w("$ " + " ".join(cmd))
    p = spawn(cmd, os.path.join(RUN_DIR, f"nginx-{mode}.out"))
    wait_listening(NGX_PORT, p)
    time.sleep(0.3)  # the worker is forked right after the listener opens
    return Server("nginx", NGX_PORT, p, access)


# ---------- load ----------

def parse_ms(s):
    m = re.match(r"([\d.]+)(us|ms|s|m)$", s)
    v, u = float(m.group(1)), m.group(2)
    return v * {"us": 0.001, "ms": 1.0, "s": 1000.0, "m": 60000.0}[u]


def parse_wrk(out):
    r = {}
    m = re.search(r"(\d+) requests in ([\d.]+)s", out)
    r["requests"], r["secs"] = int(m.group(1)), float(m.group(2))
    r["rps"] = float(re.search(r"Requests/sec:\s+([\d.]+)", out).group(1))
    for pct in ("50", "75", "90", "99"):
        m = re.search(rf"^[ \t]+{pct}%[ \t]+(\S+)", out, re.M)
        r[f"p{pct}_ms"] = parse_ms(m.group(1)) if m else None
    m = re.search(r"^[ \t]+Latency[ \t]+(\S+)[ \t]+(\S+)[ \t]+(\S+)", out, re.M)
    r["lat_avg_ms"], r["lat_max_ms"] = parse_ms(m.group(1)), parse_ms(m.group(3))
    m = re.search(r"Non-2xx or 3xx responses: (\d+)", out)
    r["non2xx"] = int(m.group(1)) if m else 0
    m = re.search(r"Socket errors: (.*)", out)
    r["socket_errors"] = m.group(1).strip() if m else ""
    m = re.search(r"([\d.]+)(KB|MB|GB|B) read", out)
    r["read_mb"] = float(m.group(1)) * {"B": 1e-6, "KB": 1e-3, "MB": 1.0, "GB": 1e3}[m.group(2)] if m else 0.0
    return r


def wrk(port, path, secs):
    url = f"http://127.0.0.1:{port}{path}"
    cmd = ["taskset", "-c", str(LOAD_CPU), WRK, "-t1", f"-c{CONNS}", f"-d{secs}s", "--latency",
           "-H", f"Host: {HOSTNAME}:{port}", url]
    p = subprocess.run(cmd, capture_output=True, text=True, timeout=secs + 60)
    return " ".join(cmd), p.stdout + p.stderr


H2_LOG = os.path.join(RUN_DIR, "h2load-requests.tsv")


def h2load(port, path, secs):
    """HTTPS with HTTP/2: CONNS clients with one stream each, so 50 requests in flight like the wrk runs."""
    try:
        os.remove(H2_LOG)  # h2load appends to its log, so every run starts from no file
    except FileNotFoundError:
        pass
    cmd = ["taskset", "-c", str(LOAD_CPU), H2LOAD, f"-D{secs}s", f"-c{CONNS}", "-m1", "-t1",
           f"--connect-to=127.0.0.1:{port}", f"--log-file={H2_LOG}", f"https://{HOSTNAME}:{port}{path}"]
    p = subprocess.run(cmd, capture_output=True, text=True, timeout=secs + 60)
    return " ".join(cmd), p.stdout + p.stderr


def parse_h2load(out):
    """Same fields as parse_wrk. Percentiles come from h2load's per-request log (exact)."""
    if "Application protocol: h2" not in out:
        raise RuntimeError("h2load did not negotiate HTTP/2:\n" + out[-600:])
    r = {}
    m = re.search(r"finished in ([\d.]+)(us|ms|s|m|h), ([\d.]+) req/s", out)
    r["secs"] = float(m.group(1)) * {"us": 1e-6, "ms": 1e-3, "s": 1.0, "m": 60.0, "h": 3600.0}[m.group(2)]
    r["rps"] = float(m.group(3))
    m = re.search(r"requests: (\d+) total, (\d+) started, (\d+) done, (\d+) succeeded, (\d+) failed, (\d+) errored, (\d+) timeout", out)
    r["requests"] = int(m.group(3))
    errored, timeout = int(m.group(6)), int(m.group(7))
    r["socket_errors"] = f"{errored} errored, {timeout} timeout" if errored or timeout else ""
    m = re.search(r"status codes: (\d+) 2xx, (\d+) 3xx, (\d+) 4xx, (\d+) 5xx", out)
    r["non2xx"] = int(m.group(3)) + int(m.group(4)) if m else 0
    m = re.search(r"traffic: ([\d.]+)(B|KB|MB|GB) ", out)
    r["read_mb"] = float(m.group(1)) * {"B": 1e-6, "KB": 1e-3, "MB": 1.0, "GB": 1e3}[m.group(2)] if m else 0.0
    m = re.search(r"time for request:\s+(\S+)\s+(\S+)\s+(\S+)\s+(\S+)\s+[\d.]+%", out)
    r["lat_avg_ms"], r["lat_max_ms"] = parse_ms(m.group(3)), parse_ms(m.group(2))
    durs = []
    with open(H2_LOG) as f:
        for line in f:
            parts = line.split("\t")
            if len(parts) >= 3:
                durs.append(int(parts[2]))
    try:
        os.remove(H2_LOG)
    except FileNotFoundError:
        pass
    durs.sort()
    for pct in (50, 75, 90, 99):
        r[f"p{pct}_ms"] = durs[min(len(durs) - 1, int(round(pct / 100.0 * (len(durs) - 1))))] / 1000.0 if durs else None
    return r


def load(mode, port, path, secs):
    return h2load(port, path, secs) if mode == "tls" else wrk(port, path, secs)


def parse_load(mode, out):
    return parse_h2load(out) if mode == "tls" else parse_wrk(out)


class Sampler(threading.Thread):
    """Reads the memory of a server every 200 ms: VmRSS summed over its processes, and PSS."""

    def __init__(self, server, interval=0.2):
        super().__init__(daemon=True)
        self.server, self.interval, self.stopped = server, interval, threading.Event()
        self.samples = []
        self.pss_samples = []

    def take(self):
        self.samples.append(self.server.rss())
        self.pss_samples.append(self.server.pss())

    def run(self):
        while True:
            self.take()
            if self.stopped.wait(self.interval):
                break
        self.take()

    def finish(self):
        self.stopped.set()
        self.join()
        return self.samples


def count_lines(path):
    n = 0
    with open(path, "rb") as f:
        while True:
            b = f.read(1 << 22)
            if not b:
                return n
            n += b.count(b"\n")


def truncate(path):
    if path and os.path.exists(path):
        os.truncate(path, 0)


def self_ticks():
    with open("/proc/self/stat") as f:
        s = f.read()
    r = s[s.rindex(")") + 2:].split()
    return int(r[11]) + int(r[12])


def measured_run(label, port, path, server, backend, extra):
    """One warm-up and one measured wrk run. Returns the RESULT dict."""
    if server is not None:
        truncate(server.log_file)
    mode = extra.get("mode")
    cmd, out = load(mode, port, path, WARMUP)
    LOG.raw(f"--- warm-up {label}")
    LOG.raw("$ " + cmd)
    LOG.raw(out)
    warm = parse_load(mode, out)
    time.sleep(0.3)
    la = loadavg()
    c0 = cpu_stat()
    srv0 = server.ticks() if server else 0
    be0 = proc_ticks(backend.pid)
    me0 = self_ticks()
    ch0 = resource.getrusage(resource.RUSAGE_CHILDREN)
    sampler = Sampler(server) if server else None
    if sampler:
        sampler.start()
    po0 = tcp_passive_opens()
    t0 = time.monotonic()
    cmd, out = load(mode, port, path, DURATION)
    wall = time.monotonic() - t0
    po1 = tcp_passive_opens()
    samples = sampler.finish() if sampler else []
    c1 = cpu_stat()
    srv1 = server.ticks() if server else 0
    be1 = proc_ticks(backend.pid)
    me1 = self_ticks()
    ch1 = resource.getrusage(resource.RUSAGE_CHILDREN)
    LOG.raw(f"--- measured {label}")
    LOG.raw("$ " + cmd)
    LOG.raw(out)
    r = parse_load(mode, out)
    r.update(extra)
    r["loadavg_before"] = la
    r["conns_accepted"] = po1 - po0
    r["warm_requests"] = warm["requests"]
    r["wall_s"] = round(wall, 2)
    sc = SERVER_CPU_KEY
    lc = LOAD_CPU_KEY
    d = lambda k, cpu: c1[cpu][k] - c0[cpu][k]
    tot_s, tot_l = max(1, d("total", sc)), max(1, d("total", lc))
    ours_s = (srv1 - srv0) + (be1 - be0)
    wrk_ticks = ((ch1.ru_utime + ch1.ru_stime) - (ch0.ru_utime + ch0.ru_stime)) * CLK
    ours_l = wrk_ticks + (me1 - me0)
    r["server_cpu_ticks"] = srv1 - srv0
    r["backend_cpu_ticks"] = be1 - be0
    r["server_cpu_us_per_req"] = round((srv1 - srv0) * 1e6 / CLK / max(1, r["requests"]), 2) if server else None
    r["cpu_server_core_busy_pct"] = round(100.0 * d("busy", sc) / tot_s, 1)
    r["cpu_load_core_busy_pct"] = round(100.0 * d("busy", lc) / tot_l, 1)
    r["cpu_server_core_ours_pct"] = round(100.0 * ours_s / tot_s, 1)
    r["cpu_load_core_ours_pct"] = round(100.0 * ours_l / tot_l, 1)
    r["cpu_server_core_other_pct"] = round(100.0 * (d("busy", sc) - ours_s) / tot_s, 1)
    r["cpu_load_core_other_pct"] = round(100.0 * (d("busy", lc) - ours_l) / tot_l, 1)
    r["steal_ticks"] = d("steal", sc) + d("steal", lc)
    if samples:
        r["rss_peak_kb"], r["rss_min_kb"], r["rss_samples"] = max(samples), min(samples), len(samples)
        r["pss_peak_kb"] = max(sampler.pss_samples)
    if server and server.log_file:
        time.sleep(0.5)
        r["log_lines"] = count_lines(server.log_file)
        r["log_bytes"] = os.path.getsize(server.log_file)
        r["log_expected"] = warm["requests"] + r["requests"]
    return r


SERVER_CPU_KEY = f"cpu{SERVER_CPU}"
LOAD_CPU_KEY = f"cpu{LOAD_CPU}"


def fetch(port, path, tls=False):
    if tls:
        ctx = ssl.create_default_context()
        ctx.check_hostname, ctx.verify_mode = False, ssl.CERT_NONE
        c = http.client.HTTPSConnection("127.0.0.1", port, timeout=10, context=ctx)
        c.sock = ctx.wrap_socket(socket.create_connection(("127.0.0.1", port), timeout=10), server_hostname=HOSTNAME)
    else:
        c = http.client.HTTPConnection("127.0.0.1", port, timeout=10)
    c.request("GET", path, headers={"Host": f"{HOSTNAME}:{port}"})
    r = c.getresponse()
    body = r.read()
    heads = r.getheaders()
    c.close()
    return r.status, heads, body


def sanity(servers, label, tls=False):
    """Fetch every route from each server and log what came back."""
    LOG.w(f"== routes check ({label}) ==")
    bad = []
    for route in ROUTES:
        path, want = ROUTE_DEF[route]
        for s in servers:
            status, heads, body = fetch(s.port, path, tls)
            hs = "; ".join(f"{k}: {v}" for k, v in heads if k.lower() in
                           ("content-type", "content-length", "etag", "cache-control", "server", "bareproxy-id", "connection"))
            LOG.w(f"{route:5} {s.name:9} GET {path} -> {status}, {len(body)} bytes; {hs}")
            if route == "api":
                LOG.w(f"        body: {body.decode(errors='replace').strip()}")
                if b'"path":"/orders"' not in body:
                    bad.append(f"{s.name} {route}: the prefix was not stripped")
            if status != want:
                bad.append(f"{s.name} {route}: status {status}, wanted {want}")
        # compare sizes between servers
    if bad:
        raise RuntimeError("route check failed: " + "; ".join(bad))


def explain_log(bpf, scheme="http"):
    """Ask the running BareProxy to explain each route, for the log."""
    LOG.w("== bareproxy explain, per route ==")
    for route in ROUTES:
        path, _ = ROUTE_DEF[route]
        cmd = [BAREPROXY, "explain", "--config", bpf, "GET", f"{scheme}://{HOSTNAME}:{BP_PORT}{path}"]
        try:
            p = subprocess.run(cmd, capture_output=True, text=True, timeout=20)
            LOG.w("$ " + " ".join(cmd))
            LOG.w((p.stdout + p.stderr).rstrip())
        except Exception as e:  # keep going: this only fills the log
            LOG.w(f"explain failed: {e}")


def sh(cmd):
    try:
        return subprocess.run(cmd, shell=True, capture_output=True, text=True, timeout=30).stdout.strip()
    except Exception as e:
        return f"({e})"


def environment():
    LOG.w("== environment ==")
    now = datetime.datetime.now().astimezone()
    LOG.w(f"date: {now.strftime('%Y-%m-%d %H:%M:%S %Z (UTC%z)')}")
    LOG.w(f"date utc: {datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%d %H:%M:%S')}")
    cpu = sh("lscpu | grep -E 'Model name|^CPU\\(s\\)|Thread|Core|Hypervisor|Virtualization type|L2 cache|L3 cache'")
    LOG.w("lscpu:\n" + cpu)
    LOG.w("cpu MHz: " + sh("grep -m1 MHz /proc/cpuinfo | cut -d: -f2"))
    LOG.w("kernel: " + sh("uname -srvm"))
    LOG.w("memory: " + sh("free -m | head -2"))
    LOG.w("go (build tool): " + sh("GOTOOLCHAIN=local go version"))
    LOG.w(f"bareproxy binary: {BAREPROXY}")
    LOG.w("bareproxy version: " + sh(f"'{BAREPROXY}' version"))
    LOG.w("bareproxy binary sha256: " + sh(f"sha256sum '{BAREPROXY}' | cut -c1-16") + " (first 16 hex digits)")
    LOG.w("bareproxy binary built: " + sh(f"date -r '{BAREPROXY}' '+%Y-%m-%d %H:%M:%S'"))
    info = os.path.join(HERE, "bin", "build-info.txt")
    if os.path.exists(info):
        LOG.w("bareproxy build info (bin/build-info.txt): " + open(info).read().strip())
    LOG.w("bareproxy source tree at run time: git " + sh(f"git -C '{REPO}' log -1 --format='%h %s' 2>&1")
          + "; uncommitted files: " + sh(f"git -C '{REPO}' status --short 2>&1 | wc -l"))
    LOG.w("go version of the binary: " + sh(f"GOTOOLCHAIN=local go version '{BAREPROXY}' 2>&1 | head -1"))
    LOG.w("nginx: " + sh(f"{NGINX} -v 2>&1"))
    LOG.w("nginx build: " + sh(f"dpkg -l nginx-light nginx 2>/dev/null | grep '^ii' | awk '{{print $2, $3}}' | tr '\\n' ' '"))
    LOG.w("nginx package: " + sh(f"dpkg -S {NGINX} 2>&1 | head -1"))
    LOG.w("nginx configure flags: " + sh(f"{NGINX} -V 2>&1 | grep -o 'with-[a-z_0-9-]*' | tr '\\n' ' ' | cut -c1-600"))
    LOG.w("wrk: " + sh(f"{WRK} --version 2>&1 | head -1"))
    LOG.w("h2load: " + sh(f"{H2LOAD} --version 2>&1 | head -1"))
    LOG.w("openssl: " + sh("openssl version 2>&1 | head -1"))
    LOG.w("python: " + sys.version.split()[0])
    LOG.w(f"load average at start: {loadavg()}")
    LOG.w("busiest processes at start (other agents share this machine):")
    LOG.w(sh("ps -eo pid,pcpu,pmem,etime,args --sort=-pcpu | head -6 | cut -c1-150"))
    LOG.w(f"settings: server cpu {SERVER_CPU}, load cpu {LOAD_CPU}, connections {CONNS}, warm-up {WARMUP} s, "
          f"measured {DURATION} s, runs {RUNS}, modes {','.join(MODES)}, routes {','.join(ROUTES)}; "
          f"a run that other processes disturbed (over {NOISE_PCT:.0f}% of a core) is repeated, up to {TRIES} attempts")
    LOG.w("")


def sizes():
    LOG.w("== binary sizes ==")
    out = {}
    for label, path in (("bareproxy under test", BAREPROXY),
                        ("bareproxy built plain", os.path.join(HERE, "bin", "bareproxy")),
                        ("bareproxy built stripped", os.path.join(HERE, "bin", "bareproxy-stripped")),
                        ("testapi (stripped)", TESTAPI)):
        if os.path.exists(path):
            out[label] = os.path.getsize(path)
            LOG.w(f"size {label}: {out[label]} bytes ({path})")
    ng = os.path.getsize(NGINX)
    LOG.w(f"size nginx binary: {ng} bytes ({NGINX})")
    total = ng
    for line in sh(f"ldd {NGINX}").splitlines():
        m = re.search(r"=> (/\S+)", line)
        if m:
            real = os.path.realpath(m.group(1))
            sz = os.path.getsize(real)
            total += sz
            LOG.w(f"size nginx lib {os.path.basename(m.group(1))}: {sz} bytes ({real})")
    LOG.w(f"size nginx binary plus shared libraries: {total} bytes")
    LOG.w("RESULT_SIZES " + json.dumps({"sizes": out, "nginx_binary": ng, "nginx_with_libs": total}))
    LOG.w("")


def median(v):
    return statistics.median(v)


def other_pct(r):
    return max(r["cpu_server_core_other_pct"], r["cpu_load_core_other_pct"])


def quiet_wait(maxwait=float(env("BENCH_QUIET_WAIT", "15"))):
    """Wait for a lull: other processes using under 5% of both cores. Nothing of ours is running now."""
    end = time.time() + maxwait
    while True:
        a = cpu_stat()
        time.sleep(0.5)
        b = cpu_stat()
        worst = max(100.0 * (b[k]["busy"] - a[k]["busy"]) / max(1, b[k]["total"] - a[k]["total"])
                    for k in (SERVER_CPU_KEY, LOAD_CPU_KEY))
        if worst < 5.0 or time.time() >= end:
            return worst
        time.sleep(0.5)


def run_case(label, port, path, server, backend, extra):
    """One round of one case. This machine is shared, so a run that other processes disturbed (more than
    NOISE_PCT of a core not spent by us) is repeated, up to TRIES attempts. Every attempt is logged as a
    RESULT line; summarize.py uses the clean ones."""
    out = []
    for attempt in range(1, TRIES + 1):
        quiet = quiet_wait()
        LOG.w(f"### {label} attempt={attempt} (other CPU use just before: {quiet:.0f}%)")
        r = measured_run(f"{label} attempt={attempt}", port, path, server, backend,
                         dict(extra, attempt=attempt, quiet_before_pct=round(quiet, 1)))
        r["other_pct"] = round(other_pct(r), 1)
        r["noisy"] = r["other_pct"] > NOISE_PCT
        LOG.w("RESULT " + json.dumps(r))
        out.append(r)
        time.sleep(1)
        if not r["noisy"]:
            break
        more = attempt < TRIES and time.time() - STARTED_AT < MAX_SECS
        LOG.w(f"note: other processes used {r['other_pct']:.0f}% of a core during that run; "
              + ("trying again" if more else "no more attempts, the run stays flagged"))
        if not more:
            break
    return out


def pick_ports():
    """Use the wanted ports, or the next free one when another process (this
    machine is shared) already holds it. Every change is written to the log."""
    global BP_PORT, NGX_PORT, API_PORT
    taken = set()
    chosen = {}
    for name, want in (("bareproxy", BP_PORT), ("nginx", NGX_PORT), ("backend", API_PORT)):
        port = want
        while port in taken or not port_free(port):
            port += 1
            if port > want + 50:
                raise SystemExit(f"no free port near {want} for {name}")
        if port != want:
            LOG.w(f"note: port {want} is in use by another process, so {name} uses port {port}")
        taken.add(port)
        chosen[name] = port
    BP_PORT, NGX_PORT, API_PORT = chosen["bareproxy"], chosen["nginx"], chosen["backend"]
    LOG.w(f"ports: bareproxy {BP_PORT}, nginx {NGX_PORT}, backend {API_PORT}")


def main():
    global LOG
    signal.signal(signal.SIGTERM, on_signal)
    signal.signal(signal.SIGINT, on_signal)
    os.sched_setaffinity(0, {LOAD_CPU})
    os.makedirs(RESULTS, exist_ok=True)
    os.makedirs(RUN_DIR, exist_ok=True)
    stamp = datetime.date.today().isoformat()
    path = env("BENCH_LOG", os.path.join(RESULTS, f"bench-{stamp}.log"))
    if "BENCH_LOG" not in os.environ and os.path.exists(path):
        path = os.path.join(RESULTS, f"bench-{stamp}-{datetime.datetime.now().strftime('%H%M')}.log")
    LOG = Log(path)
    LOG.w(f"# BareProxy against nginx, same routes. Log: {path}")
    started = time.time()
    pick_ports()
    for prog in (BAREPROXY, NGINX, TESTAPI):
        if not os.access(prog, os.X_OK):
            raise SystemExit(f"{prog} is not an executable file")
    if not os.path.exists(os.path.join(SITE, "index.html")):
        raise SystemExit(f"no built site at {SITE}: run ./build.sh first")
    if shutil.which(WRK) is None:
        raise SystemExit("wrk is not installed (apt-get install -y wrk)")
    environment()
    sizes()
    backend = start_backend()
    LOG.w(f"backend: testapi, pid {backend.pid}, port {API_PORT}, cpu {SERVER_CPU}")
    results = []
    if DIRECT:
        LOG.w("== backend alone (wrk straight to testapi, same core as the servers use) ==")
        for rnd in range(1, RUNS + 1):
            label = f"server=direct mode=- route=api round={rnd}"
            results.extend(run_case(label, API_PORT, "/orders", None, backend,
                                    {"server": "direct", "mode": "-", "route": "api", "round": rnd}))
    for mode in MODES:
        LOG.w(f"== logging mode: {mode} ==")
        servers = {}
        for name in SERVERS:
            servers[name] = start_bareproxy(mode) if name == "bareproxy" else start_nginx(mode)
        time.sleep(2)  # let startup work finish before the idle reading
        for name, s in servers.items():
            idle = {"server": name, "mode": mode, "rss_idle_kb": s.rss(), "hwm_idle_kb": s.rss("VmHWM"), "pss_idle_kb": s.pss(),
                    "pids": s.pids(), "loadavg": loadavg()}
            LOG.w("RESULT_IDLE " + json.dumps(idle))
            results.append(idle)
        sanity(list(servers.values()), f"mode {mode}", mode == "tls")
        if "bareproxy" in servers:
            explain_log(os.path.join(RUN_DIR, f"bareproxy-{mode}.conf"), "https" if mode == "tls" else "http")
        for rnd in range(1, RUNS + 1):
            for route in ROUTES:
                path_, _ = ROUTE_DEF[route]
                for name in SERVERS:
                    s = servers[name]
                    label = f"server={name} mode={mode} route={route} round={rnd}"
                    results.extend(run_case(label, s.port, path_, s, backend,
                                            {"server": name, "mode": mode, "route": route, "round": rnd}))
        time.sleep(5)
        for name, s in servers.items():
            after = {"server": name, "mode": mode, "rss_after_kb": s.rss(), "hwm_end_kb": s.rss("VmHWM")}
            LOG.w("RESULT_AFTER " + json.dumps(after))
            results.append(after)
        for s in servers.values():
            stop(s.proc)
        time.sleep(1)
    stop(backend)
    LOG.w(f"== done in {time.time() - started:.0f} s; processes stopped ==")
    return path


if __name__ == "__main__":
    try:
        log_path = main()
    finally:
        stop_all()
    left = [p for p in STARTED if p.poll() is None]
    print("left running:", len(left))
    print(log_path)
