# BareProxy against nginx: measurements

Measured without targets. BareProxy (bareproxy 0.1.0-alpha, built with go1.27.1) and nginx (nginx/1.24.0 (Ubuntu)) served the same routes over plain HTTP on localhost, each on one CPU core, on a shared cloud VM. Run on 2026-10-05 16:35:40 IDT (UTC+0300). Raw log: `results/bench-2026-10-05.log`. Harness: `live/bench/`. A run that was allowed to finish took 12 min 22 s.

Read the caveats at the end before quoting any number. The machine was shared with other workers, and latency in this test follows throughput.

**In one line (computed from the best runs below):** with logging off, nginx handled 1.9 to 3.2 times as many requests per second as BareProxy; with logging to a file, nginx handled 2.0 to 3.4 times as many requests per second as BareProxy.
The server process itself used 41 to 72 microseconds of CPU per request in BareProxy and 14 to 23 in nginx (medians; this figure stayed steady even when other processes disturbed the machine).

**Why best runs.** This machine is shared, and other processes can take CPU time during a run. Of 51 attempts, 0 lost more than 10% of a core to other processes. That can only slow a run down. Among the 17 cases that have two or more clean attempts, the req/s of the clean attempts differed by at most 8%. Across the cases with repeats, the weakest attempt reached as little as 92% of the best run of its case. So the main figure is the best run of each case (highest req/s over all attempts, with the p50 and p99 of that same run), and the median of all attempts is shown beside it for comparison. The median of disturbed runs measures the neighbours as much as the servers.

## Throughput and latency, logging off

Keep-alive, 50 connections, 2 s warm-up, 10 s per run, every attempt logged. An attempt is clean when other processes used 10% or less of either core.

| Case | Server | req/s, best run | req/s, median of all attempts (lowest to highest) | Attempts, clean of all | p50 (ms), best run | p99 (ms), best run | Server CPU per request (us, median) | Generator core busy (all processes), best run |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `GET /` (200, 11,596 bytes) | BareProxy | 21,819 | 21,642 (21,621 to 21,819) | 3 of 3 | 2.35 | 28.30 | 45.8 | 34% |
|  | nginx | 66,136 | 64,784 (61,276 to 66,136) | 3 of 3 | 0.76 | 1.52 | 15.2 | 74% |
| `GET /images/plan-demo.png` (200, 72,732 bytes) | BareProxy | 17,769 | 17,512 (17,145 to 17,769) | 3 of 3 | 2.84 | 7.58 | 57.0 | 44% |
|  | nginx | 57,095 † | 56,595 (54,999 to 57,095) | 3 of 3 | 0.36 | 40.63 | 15.4 | 98% |
| `GET /no-such-page/` (404, 4,686 bytes) | BareProxy | 24,278 | 24,260 (23,076 to 24,278) | 3 of 3 | 2.36 | 18.78 | 40.8 | 34% |
|  | nginx | 46,762 | 46,691 (46,185 to 46,762) | 3 of 3 | 1.06 | 2.31 | 21.0 | 53% |
| `GET /api/orders` (200, 153 bytes) | BareProxy | 12,055 | 11,946 (11,176 to 12,055) | 3 of 3 | 3.95 | 9.09 | 65.6 | 19% |
|  | nginx | 35,432 | 34,516 (34,454 to 35,432) | 3 of 3 | 1.28 | 3.56 | 13.7 | 35% |
|  | testapi alone (no proxy) | 51,022 | 50,887 (50,367 to 51,022) | 3 of 3 | 0.99 | 2.40 | n/a | 51% |

† The load generator's core was more than 90% busy in the best run, so the server could have gone faster: treat the number as a floor. ‡ Even the best run was disturbed by other processes (more than 10% of a core): the number is a floor too.

Ratios (computed):

| Case | req/s, nginx over BareProxy (best runs) | req/s, nginx over BareProxy (medians of all attempts) | CPU per request, BareProxy over nginx | p50, BareProxy over nginx (best runs) |
| --- | ---: | ---: | ---: | ---: |
| `GET /` (200, 11,596 bytes) | 3.0 | 3.0 | 3.0 | 3.1 |
| `GET /images/plan-demo.png` (200, 72,732 bytes) | 3.2 | 3.2 | 3.7 | 7.9 |
| `GET /no-such-page/` (404, 4,686 bytes) | 1.9 | 1.9 | 1.9 | 2.2 |
| `GET /api/orders` (200, 153 bytes) | 2.9 | 2.9 | 4.8 | 3.1 |

## Throughput and latency, logging to a file

Keep-alive, 50 connections, 2 s warm-up, 10 s per run, every attempt logged. An attempt is clean when other processes used 10% or less of either core.

| Case | Server | req/s, best run | req/s, median of all attempts (lowest to highest) | Attempts, clean of all | p50 (ms), best run | p99 (ms), best run | Server CPU per request (us, median) | Generator core busy (all processes), best run |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `GET /` (200, 11,596 bytes) | BareProxy | 21,964 | 21,720 (20,327 to 21,964) | 3 of 3 | 2.42 | 7.22 | 45.5 | 34% |
|  | nginx | 52,819 | 52,035 (49,869 to 52,819) | 3 of 3 | 0.97 | 1.65 | 19.0 | 66% |
| `GET /images/plan-demo.png` (200, 72,732 bytes) | BareProxy | 16,642 | 16,516 (16,105 to 16,642) | 3 of 3 | 3.06 | 8.27 | 60.3 | 44% |
|  | nginx | 55,940 † | 55,921 (55,602 to 55,940) | 3 of 3 | 0.55 | 40.33 | 16.8 | 98% |
| `GET /no-such-page/` (404, 4,686 bytes) | BareProxy | 22,261 | 21,648 (21,382 to 22,261) | 3 of 3 | 2.58 | 34.49 | 46.1 | 31% |
|  | nginx | 43,549 | 43,337 (41,162 to 43,549) | 3 of 3 | 1.17 | 1.95 | 22.8 | 52% |
| `GET /api/orders` (200, 153 bytes) | BareProxy | 11,592 | 11,011 (10,658 to 11,592) | 3 of 3 | 4.16 | 9.01 | 71.5 | 17% |
|  | nginx | 34,234 | 34,211 (33,547 to 34,234) | 3 of 3 | 1.37 | 3.26 | 14.6 | 31% |
|  | testapi alone (no proxy) | 51,022 | 50,887 (50,367 to 51,022) | 3 of 3 | 0.99 | 2.40 | n/a | 51% |

† The load generator's core was more than 90% busy in the best run, so the server could have gone faster: treat the number as a floor. ‡ Even the best run was disturbed by other processes (more than 10% of a core): the number is a floor too.

Ratios (computed):

| Case | req/s, nginx over BareProxy (best runs) | req/s, nginx over BareProxy (medians of all attempts) | CPU per request, BareProxy over nginx | p50, BareProxy over nginx (best runs) |
| --- | ---: | ---: | ---: | ---: |
| `GET /` (200, 11,596 bytes) | 2.4 | 2.4 | 2.4 | 2.5 |
| `GET /images/plan-demo.png` (200, 72,732 bytes) | 3.4 | 3.4 | 3.6 | 5.6 |
| `GET /no-such-page/` (404, 4,686 bytes) | 2.0 | 2.0 | 2.0 | 2.2 |
| `GET /api/orders` (200, 153 bytes) | 3.0 | 3.1 | 4.9 | 3.0 |

## What logging to a file costs

Change in best-run req/s when each server writes one log record per request to a file, against logging off (computed). BareProxy writes a JSON trace record; nginx writes its default access log line. Every request left a record: see the check below the table.

| Case | BareProxy | nginx |
| --- | ---: | ---: |
| `GET /` (200, 11,596 bytes) | +0.7% | -20.1% |
| `GET /images/plan-demo.png` (200, 72,732 bytes) | -6.3% | -2.0% |
| `GET /no-such-page/` (404, 4,686 bytes) | -8.3% | -6.9% |
| `GET /api/orders` (200, 153 bytes) | -3.8% | -3.4% |

Log check: BareProxy: 12 of 12 runs had at least as many log lines as requests counted by wrk (lowest ratio 1.0004; a few more lines than requests is normal, because requests still in flight when wrk stops are logged but not counted); nginx: 12 of 12 runs had at least as many log lines as requests counted by wrk (lowest ratio 1.0001; a few more lines than requests is normal, because requests still in flight when wrk stops are logged but not counted).

Average size of one log line, computed from file size over lines: BareProxy 502 bytes, nginx 88 bytes.

## Keep-alive check

Connections accepted on the machine during a measured run, from `/proc/net/snmp`; the lowest of the attempts of each case. The generator holds 50 connections, so a figure near 50 means every connection was reused for the whole run. The count is machine-wide, so tests that other workers run at the same time add to it: that is why the lowest attempt is shown. In the API case the count also includes the proxy's connections to the backend.

| Logging | Case | BareProxy | nginx |
| --- | --- | ---: | ---: |
| logging off | `GET /` (200, 11,596 bytes) | 51 | 51 |
| logging off | `GET /images/plan-demo.png` (200, 72,732 bytes) | 51 | 51 |
| logging off | `GET /no-such-page/` (404, 4,686 bytes) | 51 | 51 |
| logging off | `GET /api/orders` (200, 153 bytes) | 71 | 51 |
| logging to a file | `GET /` (200, 11,596 bytes) | 51 | 51 |
| logging to a file | `GET /images/plan-demo.png` (200, 72,732 bytes) | 51 | 51 |
| logging to a file | `GET /no-such-page/` (404, 4,686 bytes) | 51 | 51 |
| logging to a file | `GET /api/orders` (200, 153 bytes) | 51 | 51 |

## Memory

VmRSS from `/proc/PID/status`, summed over the processes of a server (nginx is its master plus its worker), read right after start and every 200 ms during each measured run. PSS (also summed) splits pages shared between processes, which matters for nginx because its master and worker share memory. Values are in MiB (computed from kB). The high-water mark is the kernel's VmHWM for the whole life of the server.

| Logging | Server | Idle RSS | Idle PSS | Peak RSS under load (highest of all runs) | Peak PSS under load | Peak RSS by case, median of runs (home / file / 404 / api) | High-water mark at the end |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| logging off | BareProxy | 8.0 | 8.0 | 38.8 | 38.8 | 38.2 / 38.2 / 38.3 / 38.3 | 38.8 |
| logging off | nginx | 11.4 | 6.0 | 12.4 | 6.5 | 12.4 / 12.4 / 12.4 / 12.4 | 12.4 |
| logging to a file | BareProxy | 8.0 | 8.0 | 38.7 | 38.7 | 38.1 / 38.1 / 38.7 / 38.7 | 38.7 |
| logging to a file | nginx | 11.4 | 6.0 | 12.4 | 6.5 | 12.4 / 12.4 / 12.2 / 12.4 | 12.4 |

## Binary size

| What | Bytes | MiB (computed) | Notes |
| --- | ---: | ---: | --- |
| BareProxy as measured | 8,917,152 | 8.5 | the binary under test, named in the environment below |
| testapi (stripped) | 5,894,407 | 5.6 | the test backend, for reference |
| nginx binary alone | 1,314,136 | 1.3 | `/usr/sbin/nginx`, Ubuntu package |
| nginx binary plus shared libraries | 10,386,480 | 9.9 | from `ldd`: libcrypt.so.1 0.2, libpcre2-8.so.0 0.6, libssl.so.3 0.7, libcrypto.so.3 5.1, libz.so.1 0.1, libc.so.6 2.0 (MiB each); the libraries are shared with the rest of the system |

Linkage of the BareProxy binary that was measured (`ldd`, run when this summary was made, only if the file still matches the hash in the log): static: no shared libraries. The nginx figure with libraries is for context only, since libc and the TLS library would be on the machine anyway.

## Environment

```
date: 2026-10-05 16:35:40 IDT (UTC+0300)
date utc: 2026-10-05 13:35:40
lscpu:
CPU(s):                                  2
Model name:                              Intel(R) Xeon(R) Processor @ 2.10GHz
Thread(s) per core:                      1
Core(s) per socket:                      2
Hypervisor vendor:                       KVM
Virtualization type:                     full
L2 cache:                                4 MiB (2 instances)
L3 cache:                                260 MiB (1 instance)
cpu MHz: 2100.000
kernel: Linux 6.18.44-fc-v70 #1 SMP PREEMPT_DYNAMIC @0 x86_64
memory: total        used        free      shared  buff/cache   available
Mem:            8031         669        5255          13        2400        7361
go (build tool): go version go1.27.1 linux/amd64
bareproxy binary: /home/claude/bench/bin/bareproxy-final
bareproxy version: bareproxy 0.1.0-alpha, built with go1.27.1
bareproxy binary sha256: be4f37d5efd08d9d (first 16 hex digits)
bareproxy binary built: 2026-10-05 16:35:34
bareproxy source tree at run time: git fff3e3b The in-memory record store defaults to 8 MB (was 32 MB): peak memory under the full load test 46 MiB (w
go version of the binary: /home/claude/bench/bin/bareproxy-final: go1.27.1
nginx: nginx version: nginx/1.24.0 (Ubuntu)
nginx build: nginx 1.24.0-2ubuntu7.18 nginx-light 1.24.0-2ubuntu7.18
nginx package: nginx: /usr/sbin/nginx
nginx configure flags: with-cc-opt with-ld-opt with-compat with-debug with-pcre-jit with-http_ssl_module with-http_stub_status_module with-http_realip
wrk: wrk debian/4.1.0-4build2 [epoll] Copyright (C) 2012 Will Glozer
h2load: h2load nghttp2/1.59.0
openssl: OpenSSL 3.0.13 30 Jan 2024 (Library: OpenSSL 3.0.13 30 Jan 2024)
python: 3.13.16
load average at start: 0.75 1.04 1.24
busiest processes at start (other agents share this machine):
PID %CPU %MEM     ELAPSED COMMAND
32206  8.8  0.2       00:00 python3 /home/claude/bench/bench.py
   88  2.4  4.6    01:08:28 /opt/claude-code/bin/claude --preload /home/claude/.claude/remote/spare.sock
    1  0.0  0.0    01:08:31 /process_api --firecracker-init --addr 0.0.0.0:2024 --max-ws-buffer-size 32768 --block-local-connections --listen-vsock-po
   81  0.0  0.5    01:08:28 /usr/local/bin/environment-manager task-run --stdin --session cse_01XhCycMqABUGhDzHctCRQVC --session-mode resume --upgrade
   64  0.0  0.3    01:08:29 /opt/rclone/rclone-filestore multimount --config /dev/shm/rclone-boot/config.json
settings: server cpu 0, load cpu 1, connections 50, warm-up 2 s, measured 10 s, runs 3, modes off,file, routes home,file,404,api; a run that other pro
ports: bareproxy 18080, nginx 18081, backend 19001
```

Load average (1 minute) before each measured run: lowest 0.85, highest 2.31. 
Attempts disturbed by other processes (they used more than 10% of the server or generator core): 0 of 51. Cases with no clean attempt at all: 0 of 17. CPU steal ticks seen during measured runs: 24.

## Setup

- Routes, the same on both servers: `/` is served from the public folder of the built bareproxy.com site (`index.html` for folders); `/api/*` goes to the test backend with the prefix stripped (BareProxy `route /api/* -> api strip`; nginx `location /api/` with `proxy_pass http://api/;`, an upstream with `keepalive 64`, HTTP/1.1 and an empty `Connection` header); both use the site's `404.html` for missing pages. Both add `X-Forwarded-For`, `-Proto` and `-Host` on the proxied request.
- BareProxy runs with `GOMAXPROCS=1` under `taskset -c 0`. nginx runs `worker_processes 1` under `taskset -c 0`, from its own config and prefix (`nginx -p` pointing at the bench's own nginx/ folder); the system nginx is never touched. The generator is `wrk -t1 -c50` under `taskset -c 1`, and this script runs there too.
- The test backend (`tools/testapi`) is pinned to the server's core, so in the API case the proxy and the backend share one core. The row `testapi alone` shows what the backend manages with the same generator and no proxy in between.
- Servers and cases take turns within each round (BareProxy, then nginx, case by case), so slow changes on the machine hit both alike.
- Logging off: BareProxy `trace-log off`, nginx `access_log off`. Logging to a file: BareProxy `trace-log FILE`, nginx `access_log FILE` with the default format and no buffering. The log file is emptied before each run.
- To repeat: `BAREPROXY=/path/to/binary ./bench.sh` in `live/bench/`, or `python3 summarize.py LOG` to rebuild this file from a log.

## Caveats

- **A shared two-CPU cloud VM.** This is a KVM guest with two vCPUs; other work can run on it during a measurement (the count of disturbed attempts above says how much did), and the host under it is shared too. Run-to-run differences of several percent are normal here, and a run can be hit by a neighbour's burst. The tables show the range of runs. The flag ‡ comes from the busy time of a core that was not spent by the server, the backend, wrk or the harness (read from `/proc/stat`, in 10 ms ticks, so a few percent either way is rounding). Treat a difference under about 10% as noise.
- **One core each, by design.** The server gets one core and the generator the other. This compares the two servers per core. It does not show what either does with more cores (nginx with several workers, BareProxy with GOMAXPROCS above 1).
- **Latency follows throughput in this test.** Fifty connections each send the next request as soon as the last one is answered, so the average latency is about 50 divided by req/s. A server that is faster shows a lower p50 for that reason alone. The p99 shows the tail. Latency at an equal, fixed request rate is not measured here.
- **The generator can be the limit.** wrk runs on one thread. Where its core was above 90% busy (marked †), the server's number is a floor.
- **Localhost only.** No network between the machines, GET requests only, one route per run, small responses, plain HTTP only (no TLS, no HTTP/2 in this run). It is a measurement of the proxy and file paths, not a model of real traffic.
- **API case shares a core with the backend.** The proxy's own cost is the column `Server CPU per request`, taken from the CPU time of the server processes alone (computed from `/proc` ticks of 10 ms, over a 10 s run).
- **The two logs differ.** BareProxy's trace record is a JSON line with the routing decision; nginx's default line is shorter. Both servers write one line per request, and the log check above confirms it.
- **Keep-alive is set up alike on both sides.** nginx has `keepalive_requests` raised to 1,000,000 and an upstream pool of 64 idle connections; BareProxy's backend transport also keeps up to 64 idle connections per backend (checked in `pool.go` when this was written). The keep-alive table above shows how many connections each server really opened.
- **One build, one day.** BareProxy here is the binary named in the environment section (source tree at run time: fff3e3b); nginx is Ubuntu's 1.24.0 package. The numbers are for these two builds on this machine and not a general claim.

