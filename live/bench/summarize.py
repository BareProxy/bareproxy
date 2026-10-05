#!/usr/bin/env python3
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
"""Turns a bench.py log into summary.md (printed on stdout).

    python3 summarize.py results/bench-2026-10-05.log > results/summary.md
    python3 summarize.py LOG1 LOG2 ... > results/summary.md      (attempts from all logs are pooled)

A run that other processes disturbed (they used more than 10% of one of the two cores) is left out of the
medians whenever the case has at least one clean run. Every attempt stays in the logs.

Every number in the tables is read from the log. Medians, ratios and
percentages are computed here and labelled as computed.
"""
import hashlib
import json
import os
import re
import statistics
import subprocess
import sys
from collections import defaultdict

WRK_BUSY_LIMIT = 90.0   # a generator core busier than this percent: the server number is a floor
NOISE_LIMIT = 10.0      # other processes using more than this percent of a core during a run

SERVER_LABEL = {"bareproxy": "BareProxy", "nginx": "nginx", "direct": "testapi alone"}
MODE_LABEL = {"off": "logging off", "file": "logging to a file", "tls": "HTTPS with HTTP/2, logging off"}


def other_of(r):
    return max(r.get("cpu_server_core_other_pct", 0.0), r.get("cpu_load_core_other_pct", 0.0))


def med(xs):
    xs = [x for x in xs if x is not None]
    return statistics.median(xs) if xs else None


def n0(x):
    return "n/a" if x is None else f"{x:,.0f}"


def n1(x):
    return "n/a" if x is None else f"{x:,.1f}"


def ms(x):
    if x is None:
        return "n/a"
    return f"{x:.2f}" if x < 100 else f"{x:.0f}"


def mib(kb):
    return "n/a" if kb is None else f"{kb / 1024:.1f}"


def parse(path):
    d = {"results": [], "idle": [], "after": [], "sizes": None, "env": [], "notes": [], "routes": {},
         "done": None, "header": "", "libs": [], "log_path": path}
    in_env = False
    with open(path, errors="replace") as f:
        for raw in f:
            ln = raw.rstrip("\n")
            if ln.startswith("RESULT_IDLE "):
                d["idle"].append(json.loads(ln[len("RESULT_IDLE "):]))
            elif ln.startswith("RESULT_AFTER "):
                d["after"].append(json.loads(ln[len("RESULT_AFTER "):]))
            elif ln.startswith("RESULT_SIZES "):
                d["sizes"] = json.loads(ln[len("RESULT_SIZES "):])
            elif ln.startswith("RESULT "):
                d["results"].append(json.loads(ln[len("RESULT "):]))
            elif ln.startswith("# BareProxy against nginx"):
                d["header"] = ln
            elif ln.startswith("note: port") or ln.startswith("ports:"):
                d["notes"].append(ln)
            elif ln.startswith("== done in"):
                d["done"] = ln
            elif ln.startswith("== environment =="):
                in_env = True
            elif in_env:
                if ln.strip() == "":
                    in_env = False
                else:
                    d["env"].append(ln)
            m = re.match(r"^(home|file|404|api)\s+(bareproxy|nginx)\s+GET (\S+) -> (\d+), (\d+) bytes", ln)
            if m and m.group(2) == "bareproxy" and m.group(1) not in d["routes"]:
                d["routes"][m.group(1)] = (m.group(3), int(m.group(4)), int(m.group(5)))
            m = re.match(r"^size nginx lib (\S+): (\d+) bytes", ln)
            if m:
                d["libs"].append((m.group(1), int(m.group(2))))
    return d


def env_value(env, key):
    for ln in env:
        if ln.startswith(key):
            return ln[len(key):].strip()
    return ""


def linkage(bin_path, sha_prefix):
    """How the measured BareProxy binary is linked, by running ldd on it now. Only trusted when the file
    still has the hash the run logged."""
    try:
        with open(bin_path, "rb") as f:
            now = hashlib.sha256(f.read()).hexdigest()
        if sha_prefix and not now.startswith(sha_prefix.split()[0]):
            return "not checked: the binary has changed since the run"
        out = subprocess.run(["ldd", bin_path], capture_output=True, text=True, timeout=20)
        text = out.stdout + out.stderr
        if "not a dynamic executable" in text:
            return "static: no shared libraries"
        libs = re.findall(r"^\s*(\S+)\s+=>", text, re.M)
        return "dynamic: " + (", ".join(libs) if libs else "see ldd") + " (cgo is on in this build)"
    except (OSError, subprocess.SubprocessError):
        return "not checked"


def md_table(header, rows, align=None):
    out = ["| " + " | ".join(header) + " |"]
    a = align or ["l"] * len(header)
    out.append("|" + "|".join(" ---: " if x == "r" else " --- " for x in a) + "|")
    for r in rows:
        out.append("| " + " | ".join(str(c) for c in r) + " |")
    return "\n".join(out)


def merge(paths):
    ds = [parse(p) for p in paths]
    d = ds[0]
    starts = [env_value(x["env"], "date:") for x in ds if x["env"]]
    d["env"] = next((x["env"] for x in reversed(ds) if x["env"]), [])
    d["results"] = [r for x in ds for r in x["results"]]
    d["idle"] = [r for x in ds for r in x["idle"]]
    d["after"] = [r for x in ds for r in x["after"]]
    for x in ds[1:]:
        for k, v in x["routes"].items():
            d["routes"].setdefault(k, v)
        d["notes"] += [n for n in x["notes"] if n not in d["notes"] and n.startswith("note:")]
        if d["sizes"] is None:
            d["sizes"] = x["sizes"]
    d["dones"] = [x["done"] for x in ds if x["done"]]
    d["starts"] = starts
    d["log_path"] = ", ".join(paths)
    return d


def main(paths):
    d = merge(paths)
    cases = defaultdict(list)
    for r in d["results"]:
        cases[(r["server"], r["mode"], r["route"])].append(r)
    chosen, counts = {}, {}
    for k, rs in cases.items():
        clean = [r for r in rs if other_of(r) <= NOISE_LIMIT]
        chosen[k] = clean if clean else rs
        counts[k] = (len(clean), len(rs))
    routes = [r for r in ("home", "file", "404", "api") if r in d["routes"]] or ["home", "file", "404", "api"]
    modes = [m for m in ("off", "file", "tls") if any(k[1] == m for k in cases)]

    def route_label(route):
        if route in d["routes"]:
            p, st, size = d["routes"][route]
            return f"`GET {p}` ({st}, {size:,} bytes)"
        return route

    best = {k: max(rs, key=lambda r: r["rps"]) for k, rs in cases.items()}

    def stat(server, mode, route, key):
        return med([r.get(key) for r in cases.get((server, mode, route), [])])

    def bestrps(server, mode, route):
        b = best.get((server, mode, route))
        return b["rps"] if b else None

    def flags(k):
        f = ""
        b = best.get(k)
        if b and b["cpu_load_core_busy_pct"] >= WRK_BUSY_LIMIT:
            f += " †"
        if b and other_of(b) > NOISE_LIMIT:
            f += " ‡"
        return f

    env = d["env"]
    lines = []
    w = lines.append

    bp_ver = env_value(env, "bareproxy version:")
    bin_path = env_value(env, "bareproxy binary:")
    ngx_ver = env_value(env, "nginx:")
    date_s = env_value(env, "date:")
    model = ""
    for ln in env:
        if "Model name" in ln:
            model = ln.split(":", 1)[1].strip()
    kernel = env_value(env, "kernel:")
    total_time = ""
    secs = 0
    for line in d.get("dones", []):
        m = re.search(r"done in (\d+) s", line)
        if m:
            secs += int(m.group(1))
    if secs:
        total_time = f"{secs // 60} min {secs % 60} s" + (" (all logs added up)" if len(d.get("dones", [])) > 1 else "")

    w("# BareProxy against nginx: measurements")
    w("")
    w(f"Measured without targets. BareProxy ({bp_ver}) and nginx ({ngx_ver.replace('nginx version: ', '')}) "
      f"served the same routes over plain HTTP on localhost, each on one CPU core, on a shared cloud VM. "
      f"{'Logs started at ' + '; '.join(d['starts']) if len(d.get('starts', [])) > 1 else 'Run on ' + date_s}. "
      f"Raw log{'s' if len(d.get('starts', [])) > 1 else ''}: `{d['log_path']}`."
      + (f" A run that was allowed to finish took {total_time}." if total_time else ""))
    w("")
    w("Read the caveats at the end before quoting any number. The machine was shared with other workers, "
      "and latency in this test follows throughput.")
    w("")

    # headline, computed
    heads = []
    for mode in modes:
        rat = []
        for route in routes:
            b_, n_ = bestrps("bareproxy", mode, route), bestrps("nginx", mode, route)
            if b_ and n_:
                rat.append(n_ / b_)
        if rat:
            heads.append(f"with {MODE_LABEL[mode]}, nginx handled {min(rat):.1f} to {max(rat):.1f} times as many "
                         f"requests per second as BareProxy")
    cpu = []
    for mode in modes:
        for route in routes:
            b_, n_ = stat("bareproxy", mode, route, "server_cpu_us_per_req"), stat("nginx", mode, route, "server_cpu_us_per_req")
            if b_ and n_:
                cpu.append((b_, n_))
    if heads:
        w("**In one line (computed from the best runs below):** " + "; ".join(heads) + ".")
        if cpu:
            w(f"The server process itself used {min(b_ for b_, n_ in cpu):.0f} to {max(b_ for b_, n_ in cpu):.0f} microseconds "
              f"of CPU per request in BareProxy and {min(n_ for b_, n_ in cpu):.0f} to {max(n_ for b_, n_ in cpu):.0f} in nginx "
              f"(medians; this figure stayed steady even when other processes disturbed the machine).")
        w("")
    spreads, weakest = [], []
    for k, rs in cases.items():
        cl = sorted(r["rps"] for r in rs if other_of(r) <= NOISE_LIMIT)
        if len(cl) >= 2:
            spreads.append((cl[-1] - cl[0]) / cl[-1] * 100.0)
        if len(rs) >= 2:
            weakest.append(min(r["rps"] for r in rs) / max(r["rps"] for r in rs) * 100.0)
    n_dist = sum(1 for r in d["results"] if other_of(r) > NOISE_LIMIT)
    txt = ("**Why best runs.** The other workers on this machine were building and testing all through the measurements. "
           f"Of {len(d['results'])} attempts, {n_dist} lost more than {NOISE_LIMIT:.0f}% of a core to other processes. "
           "That can only slow a run down. ")
    if spreads:
        txt += (f"Among the {len(spreads)} cases that have two or more clean attempts, the req/s of the clean attempts differed by at most "
                f"{max(spreads):.0f}%. ")
    if weakest:
        txt += f"Across the cases with repeats, the weakest attempt reached as little as {min(weakest):.0f}% of the best run of its case. "
    txt += ("So the main figure is the best run of each case (highest req/s over all attempts, with the p50 and p99 of that same run), "
            "and the median of all attempts is shown beside it for comparison. The median of disturbed runs measures the neighbours "
            "as much as the servers.")
    w(txt)
    w("")

    # per-mode tables
    for mode in modes:
        w(f"## Throughput and latency, {MODE_LABEL[mode]}")
        w("")
        w("Keep-alive, 50 connections, 2 s warm-up, 10 s per run, every attempt logged. "
          "An attempt is clean when other processes used 10% or less of either core.")
        w("")
        rows = []
        for route in routes:
            for server in ("bareproxy", "nginx"):
                k = (server, mode, route)
                rs = cases.get(k, [])
                if not rs:
                    continue
                rps = [r["rps"] for r in rs]
                bst = best[k]
                rows.append([
                    route_label(route) if server == "bareproxy" else "",
                    SERVER_LABEL[server],
                    n0(bst["rps"]) + flags(k),
                    f"{n0(med(rps))} ({n0(min(rps))} to {n0(max(rps))})",
                    "%d of %d" % counts[k],
                    ms(bst["p50_ms"]),
                    ms(bst["p99_ms"]),
                    n1(med([r.get("server_cpu_us_per_req") for r in rs])),
                    n0(bst["cpu_load_core_busy_pct"]) + "%",
                ])
            if route == "api":
                k = ("direct", "-", "api")
                rs = cases.get(k, [])
                if rs:
                    rps = [r["rps"] for r in rs]
                    bst = best[k]
                    rows.append(["", "testapi alone (no proxy)", n0(bst["rps"]) + flags(k),
                                 f"{n0(med(rps))} ({n0(min(rps))} to {n0(max(rps))})", "%d of %d" % counts[k],
                                 ms(bst["p50_ms"]), ms(bst["p99_ms"]), "n/a", n0(bst["cpu_load_core_busy_pct"]) + "%"])
        w(md_table(["Case", "Server", "req/s, best run", "req/s, median of all attempts (lowest to highest)", "Attempts, clean of all",
                    "p50 (ms), best run", "p99 (ms), best run", "Server CPU per request (us, median)", "Generator core busy (all processes), best run"],
                   rows, ["l", "l", "r", "r", "r", "r", "r", "r", "r"]))
        w("")
        w("† The load generator's core was more than 90% busy in the best run, so the server could have gone faster: treat the number as a floor. "
          "‡ Even the best run was disturbed by other processes (more than 10% of a core): the number is a floor too.")
        w("")
        w("Ratios (computed):")
        w("")
        rows = []
        for route in routes:
            kb, kn = ("bareproxy", mode, route), ("nginx", mode, route)
            if kb not in best or kn not in best:
                continue
            bb, nn = stat(*kb[:3], "rps"), stat(*kn[:3], "rps")
            bc, nc = stat(*kb[:3], "server_cpu_us_per_req"), stat(*kn[:3], "server_cpu_us_per_req")
            b5, n5, b9, n9 = best[kb]["p50_ms"], best[kn]["p50_ms"], best[kb]["p99_ms"], best[kn]["p99_ms"]
            rows.append([route_label(route),
                         f"{best[kn]['rps'] / best[kb]['rps']:.1f}",
                         f"{nn / bb:.1f}" if bb and nn else "n/a",
                         f"{bc / nc:.1f}" if bc and nc else "n/a",
                         f"{b5 / n5:.1f}" if b5 and n5 else "n/a"])
        w(md_table(["Case", "req/s, nginx over BareProxy (best runs)", "req/s, nginx over BareProxy (medians of all attempts)",
                    "CPU per request, BareProxy over nginx", "p50, BareProxy over nginx (best runs)"],
                   rows, ["l", "r", "r", "r", "r"]))
        w("")

    # cost of logging
    if "off" in modes and "file" in modes:
        w("## What logging to a file costs")
        w("")
        w("Change in best-run req/s when each server writes one log record per request to a file, against logging off (computed). "
          "BareProxy writes a JSON trace record; nginx writes its default access log line. "
          "Every request left a record: see the check below the table.")
        w("")
        rows = []
        for route in routes:
            row = [route_label(route)]
            for server in ("bareproxy", "nginx"):
                a, b = bestrps(server, "off", route), bestrps(server, "file", route)
                row.append(f"{(b / a - 1) * 100:+.1f}%" if a and b else "n/a")
            rows.append(row)
        w(md_table(["Case", "BareProxy", "nginx"], rows, ["l", "r", "r"]))
        w("")
        ok = []
        for server in ("bareproxy", "nginx"):
            rs = [r for k, v in chosen.items() if k[0] == server and k[1] == "file" for r in v if "log_lines" in r]
            if rs:
                short = [r for r in rs if r["log_lines"] < r["log_expected"]]
                lo = min(r["log_lines"] / r["log_expected"] for r in rs)
                ok.append(f"{SERVER_LABEL[server]}: {len(rs) - len(short)} of {len(rs)} runs had at least as many log lines as requests "
                          f"counted by wrk (lowest ratio {lo:.4f}; a few more lines than requests is normal, because requests still in "
                          f"flight when wrk stops are logged but not counted)")
        if ok:
            w("Log check: " + "; ".join(ok) + ".")
            w("")
        per = []
        for server in ("bareproxy", "nginx"):
            rs = [r for k, v in chosen.items() if k[0] == server and k[1] == "file" for r in v if r.get("log_bytes") and r.get("log_lines")]
            if rs:
                per.append(f"{SERVER_LABEL[server]} {med([r['log_bytes'] / r['log_lines'] for r in rs]):.0f} bytes")
        if per:
            w("Average size of one log line, computed from file size over lines: " + ", ".join(per) + ".")
            w("")

    # connections
    w("## Keep-alive check")
    w("")
    w("Connections accepted on the machine during a measured run, from `/proc/net/snmp`; the lowest of the attempts of each case. "
      "The generator holds 50 connections, so a figure near 50 means every connection was reused for the whole run. "
      "The count is machine-wide, so tests that other workers run at the same time add to it: that is why the lowest attempt is shown. "
      "In the API case the count also includes the proxy's connections to the backend.")
    w("")
    rows = []
    for mode in modes:
        for route in routes:
            row = [MODE_LABEL[mode], route_label(route)]
            for server in ("bareproxy", "nginx"):
                cs = [r["conns_accepted"] for r in cases.get((server, mode, route), []) if "conns_accepted" in r]
                row.append(n0(min(cs)) if cs else "n/a")
            rows.append(row)
    w(md_table(["Logging", "Case", "BareProxy", "nginx"], rows, ["l", "l", "r", "r"]))
    w("")

    # memory
    w("## Memory")
    w("")
    w("VmRSS from `/proc/PID/status`, summed over the processes of a server (nginx is its master plus its worker), "
      "read right after start and every 200 ms during each measured run. "
      "PSS (also summed) splits pages shared between processes, which matters for nginx because its master and worker share memory. "
      "Values are in MiB (computed from kB). The high-water mark is the kernel's VmHWM for the whole life of the server.")
    w("")
    rows = []
    for mode in modes:
        for server in ("bareproxy", "nginx"):
            idle = [i for i in d["idle"] if i["server"] == server and i["mode"] == mode]
            after = [a for a in d["after"] if a["server"] == server and a["mode"] == mode]
            runs = [r for k, v in cases.items() if k[0] == server and k[1] == mode for r in v if "rss_peak_kb" in r]
            by_route = " / ".join(mib(med([r["rss_peak_kb"] for r in runs if r["route"] == rt])) for rt in routes)
            rows.append([MODE_LABEL[mode], SERVER_LABEL[server],
                         mib(med([i["rss_idle_kb"] for i in idle])) if idle else "n/a",
                         mib(med([i.get("pss_idle_kb") for i in idle if i.get("pss_idle_kb")])) if any(i.get("pss_idle_kb") for i in idle) else "n/a",
                         mib(max(r["rss_peak_kb"] for r in runs)) if runs else "n/a",
                         mib(max(r["pss_peak_kb"] for r in runs if "pss_peak_kb" in r)) if any("pss_peak_kb" in r for r in runs) else "n/a",
                         by_route,
                         mib(max(a["hwm_end_kb"] for a in after)) if after else "n/a"])
    w(md_table(["Logging", "Server", "Idle RSS", "Idle PSS", "Peak RSS under load (highest of all runs)",
                "Peak PSS under load", "Peak RSS by case, median of runs (" + " / ".join(routes) + ")", "High-water mark at the end"],
               rows, ["l", "l", "r", "r", "r", "r", "r", "r"]))
    w("")

    # sizes
    w("## Binary size")
    w("")
    sz = (d["sizes"] or {}).get("sizes", {})
    ngx = (d["sizes"] or {}).get("nginx_binary")
    ngx_all = (d["sizes"] or {}).get("nginx_with_libs")
    rows = []
    for k in ("bareproxy built plain", "bareproxy built stripped", "testapi (stripped)"):
        if k in sz:
            note = {"bareproxy built plain": "`go build`, default flags",
                    "bareproxy built stripped": "`-trimpath -ldflags=\"-s -w\"`; this is the binary that was measured"
                    if "stripped" in bin_path else "`-trimpath -ldflags=\"-s -w\"`",
                    "testapi (stripped)": "the test backend, for reference"}[k]
            rows.append([k.replace("bareproxy built plain", "BareProxy as built").replace("bareproxy built stripped", "BareProxy stripped"),
                         f"{sz[k]:,}", f"{sz[k] / 1048576:.1f}", note])
    if ngx:
        rows.append(["nginx binary alone", f"{ngx:,}", f"{ngx / 1048576:.1f}", "`/usr/sbin/nginx`, Ubuntu package"])
        rows.append(["nginx binary plus shared libraries", f"{ngx_all:,}", f"{ngx_all / 1048576:.1f}",
                     "from `ldd`: " + ", ".join(f"{n} {s / 1048576:.1f}" for n, s in d["libs"]) + " (MiB each); the libraries are shared with the rest of the system"])
    w(md_table(["What", "Bytes", "MiB (computed)", "Notes"], rows, ["l", "r", "r", "l"]))
    w("")
    w("Linkage of the BareProxy binary that was measured (`ldd`, run when this summary was made, only if the file still matches "
      f"the hash in the log): {linkage(bin_path, env_value(env, 'bareproxy binary sha256:'))}. "
      "The nginx figure with libraries is for context only, since libc and the TLS library would be on the machine anyway.")
    w("")

    # noise
    noisy, lows = [], []
    for k, rs in sorted(cases.items()):
        for r in rs:
            o = other_of(r)
            if o > NOISE_LIMIT:
                noisy.append((k, r["round"], o))
    steal = sum(r.get("steal_ticks", 0) for r in d["results"])
    la = [float(r["loadavg_before"].split()[0]) for r in d["results"] if "loadavg_before" in r]

    w("## Environment")
    w("")
    w("```")
    for ln in env:
        w(re.sub(r"^cpu MHz: cpu MHz\s*:\s*", "cpu MHz: ", ln)[:150])
    for ln in d["notes"]:
        w(ln)
    w("```")
    w("")
    w(f"Load average (1 minute) before each measured run: lowest {min(la):.2f}, highest {max(la):.2f}. " if la else "")
    total = len(d["results"])
    none_clean = sorted(k for k, (c, n) in counts.items() if c == 0)
    w(f"Attempts disturbed by other processes (they used more than {NOISE_LIMIT:.0f}% of the server or generator core): "
      f"{len(noisy)} of {total}. Cases with no clean attempt at all: {len(none_clean)} of {len(counts)}"
      + (" (" + "; ".join(f"{SERVER_LABEL[k[0]]} {MODE_LABEL.get(k[1], k[1])} {k[2]}" for k in none_clean[:10]) + (", and more" if len(none_clean) > 10 else "") + ")" if none_clean else "")
      + f". CPU steal ticks seen during measured runs: {steal}.")
    w("")

    w("## Setup")
    w("")
    w("- Routes, the same on both servers: `/` is served from the public folder of the built bareproxy.com site "
      "(`index.html` for folders); `/api/*` goes to the test backend with the prefix stripped "
      "(BareProxy `route /api/* -> api strip`; nginx `location /api/` with `proxy_pass http://api/;`, "
      "an upstream with `keepalive 64`, HTTP/1.1 and an empty `Connection` header); both use the site's `404.html` for missing pages. "
      "Both add `X-Forwarded-For`, `-Proto` and `-Host` on the proxied request.")
    w("- BareProxy runs with `GOMAXPROCS=1` under `taskset -c 0`. nginx runs `worker_processes 1` under `taskset -c 0`, from its own "
      "config and prefix (`nginx -p` pointing at the bench's own nginx/ folder); the system nginx is never touched. "
      "The generator is `wrk -t1 -c50` under `taskset -c 1`, and this script runs there too.")
    w("- The test backend (`tools/testapi`) is pinned to the server's core, so in the API case the proxy and the backend share one core. "
      "The row `testapi alone` shows what the backend manages with the same generator and no proxy in between.")
    w("- Servers and cases take turns within each round (BareProxy, then nginx, case by case), so slow changes on the machine hit both alike.")
    w("- Logging off: BareProxy `trace-log off`, nginx `access_log off`. Logging to a file: BareProxy `trace-log FILE`, nginx `access_log FILE` "
      "with the default format and no buffering. The log file is emptied before each run.")
    if "tls" in modes:
        w("- HTTPS case (`HTTPS with HTTP/2`): both servers use the same self-signed ECDSA P-256 certificate for `bench.local`, "
          "TLS 1.2 and 1.3 allowed, logging off, HTTP/2 negotiated (the harness stops if it is not). BareProxy listens with "
          "`site bench.local:PORT` and `tls CERT KEY`; nginx with `listen PORT ssl http2`. The generator is `h2load -c50 -m1 -t1` "
          "(50 clients with one stream each, so 50 requests in flight as in the plain runs; many streams on few connections is not covered). "
          "p50 and p99 come from h2load's per-request log. The negotiated TLS version and cipher are in the raw log.")
    w("- To repeat: `BAREPROXY=/path/to/binary ./bench.sh` in `live/bench/`, or `python3 summarize.py LOG` to rebuild this file from a log.")
    w("")

    w("## Caveats")
    w("")
    w("- **A shared two-CPU cloud VM.** This is a KVM guest with two vCPUs, shared with other workers who were building and "
      "testing at the same time, and the host under it is shared too. Run-to-run differences of several percent are normal here, "
      "and a run can be hit by a neighbour's burst. The tables show the range of runs. The flag ‡ comes from the busy time of a core "
      "that was not spent by the server, the backend, wrk or the harness (read from `/proc/stat`, in 10 ms ticks, so a few percent "
      "either way is rounding). Treat a difference under about 10% as noise.")
    w("- **One core each, by design.** The server gets one core and the generator the other. This compares the two servers per core. "
      "It does not show what either does with more cores (nginx with several workers, BareProxy with GOMAXPROCS above 1).")
    w("- **Latency follows throughput in this test.** Fifty connections each send the next request as soon as the last one is answered, "
      "so the average latency is about 50 divided by req/s. A server that is faster shows a lower p50 for that reason alone. "
      "The p99 shows the tail. Latency at an equal, fixed request rate is not measured here.")
    w("- **The generator can be the limit.** wrk runs on one thread. Where its core was above 90% busy (marked †), "
      "the server's number is a floor.")
    w("- **Localhost only.** No network between the machines, GET requests only, one route per run, small responses"
      + (", and one HTTPS case with HTTP/2 beside the plain HTTP ones" if "tls" in modes else ", plain HTTP only (no TLS, no HTTP/2 in this run)")
      + ". It is a measurement of the proxy and file paths, not a model of real traffic.")
    w("- **API case shares a core with the backend.** The proxy's own cost is the column `Server CPU per request`, "
      "taken from the CPU time of the server processes alone (computed from `/proc` ticks of 10 ms, over a 10 s run).")
    w("- **The two logs differ.** BareProxy's trace record is a JSON line with the routing decision; nginx's default line is shorter. "
      "Both servers write one line per request, and the log check above confirms it.")
    w("- **Keep-alive is set up alike on both sides.** nginx has `keepalive_requests` raised to 1,000,000 and an upstream pool of 64 idle "
      "connections; BareProxy's backend transport also keeps up to 64 idle connections per backend (checked in `pool.go` when this was written). "
      "The keep-alive table above shows how many connections each server really opened.")
    w("- **One build, one day.** BareProxy here is the 0.1.0-dev first cut as built at the start of the day; "
      "nginx is Ubuntu's 1.24.0 package. The numbers are for these two builds on this machine and not a general claim.")
    w("")
    return "\n".join(lines)


if __name__ == "__main__":
    if len(sys.argv) < 2:
        sys.exit("usage: summarize.py LOGFILE [LOGFILE...]")
    print(main(sys.argv[1:]))
