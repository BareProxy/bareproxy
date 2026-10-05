# BareProxy against nginx: measurements

Measured without targets. BareProxy (bareproxy 0.1.0-alpha, built with go1.27.1) and nginx (nginx/1.24.0 (Ubuntu)) served the same routes over plain HTTP on localhost, each on one CPU core, on a shared cloud VM. Run on 2026-10-05 11:04:31 IDT (UTC+0300). Raw log: `results/bench-2026-10-05.log`. Harness: `live/bench/`. A run that was allowed to finish took 12 min 21 s.

Read the caveats at the end before quoting any number. The machine was shared with other workers, and latency in this test follows throughput.

**In one line (computed from the best runs below):** with logging off, nginx handled 2.1 to 3.4 times as many requests per second as BareProxy; with logging to a file, nginx handled 1.9 to 3.4 times as many requests per second as BareProxy.
The server process itself used 45 to 86 microseconds of CPU per request in BareProxy and 14 to 24 in nginx (medians; this figure stayed steady even when other processes disturbed the machine).

**Why best runs.** Every attempt is in the raw log. This run had the machine to itself: none of the 51 attempts lost more than 10% of a core to other processes, so the best run and the median of all attempts sit close together in the tables.

## Throughput and latency, logging off

Keep-alive, 50 connections, 2 s warm-up, 10 s per run, every attempt logged. An attempt is clean when other processes used 10% or less of either core.

| Case | Server | req/s, best run | req/s, median of all attempts (lowest to highest) | Attempts, clean of all | p50 (ms), best run | p99 (ms), best run | Server CPU per request (us, median) | Generator core busy (all processes), best run |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `GET /` (200, 11,596 bytes) | BareProxy | 20,872 | 20,692 (20,211 to 20,872) | 3 of 3 | 2.47 | 11.07 | 48.0 | 38% |
|  | nginx | 61,593 | 60,340 (59,468 to 61,593) | 3 of 3 | 0.81 | 1.59 | 16.0 | 71% |
| `GET /images/plan-demo.png` (200, 72,732 bytes) | BareProxy | 17,084 | 16,620 (16,439 to 17,084) | 3 of 3 | 2.94 | 7.71 | 59.4 | 46% |
|  | nginx | 53,809 † | 53,018 (52,033 to 53,809) | 3 of 3 | 0.38 | 41.54 | 15.4 | 99% |
| `GET /no-such-page/` (404, 4,686 bytes) | BareProxy | 22,397 | 22,121 (22,041 to 22,397) | 3 of 3 | 2.57 | 23.10 | 44.5 | 31% |
|  | nginx | 46,273 | 46,031 (45,541 to 46,273) | 3 of 3 | 1.12 | 2.08 | 21.4 | 55% |
| `GET /api/orders` (200, 153 bytes) | BareProxy | 10,430 | 10,028 (9,904 to 10,430) | 3 of 3 | 4.48 | 12.50 | 80.1 | 16% |
|  | nginx | 35,414 | 34,326 (34,231 to 35,414) | 3 of 3 | 1.31 | 3.10 | 13.9 | 30% |
|  | testapi alone (no proxy) | 47,832 | 47,593 (47,193 to 47,832) | 3 of 3 | 1.05 | 2.68 | n/a | 51% |

† The load generator's core was more than 90% busy in the best run, so the server could have gone faster: treat the number as a floor. ‡ Even the best run was disturbed by other processes (more than 10% of a core): the number is a floor too.

Ratios (computed):

| Case | req/s, nginx over BareProxy (best runs) | req/s, nginx over BareProxy (medians of all attempts) | CPU per request, BareProxy over nginx | p50, BareProxy over nginx (best runs) |
| --- | ---: | ---: | ---: | ---: |
| `GET /` (200, 11,596 bytes) | 3.0 | 2.9 | 3.0 | 3.1 |
| `GET /images/plan-demo.png` (200, 72,732 bytes) | 3.1 | 3.2 | 3.8 | 7.7 |
| `GET /no-such-page/` (404, 4,686 bytes) | 2.1 | 2.1 | 2.1 | 2.3 |
| `GET /api/orders` (200, 153 bytes) | 3.4 | 3.4 | 5.8 | 3.4 |

## Throughput and latency, logging to a file

Keep-alive, 50 connections, 2 s warm-up, 10 s per run, every attempt logged. An attempt is clean when other processes used 10% or less of either core.

| Case | Server | req/s, best run | req/s, median of all attempts (lowest to highest) | Attempts, clean of all | p50 (ms), best run | p99 (ms), best run | Server CPU per request (us, median) | Generator core busy (all processes), best run |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `GET /` (200, 11,596 bytes) | BareProxy | 20,617 | 20,385 (19,357 to 20,617) | 3 of 3 | 2.64 | 18.80 | 48.5 | 35% |
|  | nginx | 51,474 | 49,767 (36,507 to 51,474) | 3 of 3 | 0.99 | 1.68 | 19.6 | 64% |
| `GET /images/plan-demo.png` (200, 72,732 bytes) | BareProxy | 17,029 | 15,979 (14,114 to 17,029) | 3 of 3 | 2.97 | 7.95 | 62.5 | 44% |
|  | nginx | 55,333 † | 54,809 (53,949 to 55,333) | 3 of 3 | 0.38 | 40.39 | 16.3 | 98% |
| `GET /no-such-page/` (404, 4,686 bytes) | BareProxy | 21,652 | 21,632 (21,595 to 21,652) | 3 of 3 | 2.64 | 29.45 | 46.0 | 34% |
|  | nginx | 41,489 | 40,819 (40,598 to 41,489) | 3 of 3 | 1.20 | 2.49 | 24.1 | 49% |
| `GET /api/orders` (200, 153 bytes) | BareProxy | 9,827 | 9,359 (9,357 to 9,827) | 3 of 3 | 4.76 | 13.19 | 86.0 | 15% |
|  | nginx | 33,193 | 33,006 (31,480 to 33,193) | 3 of 3 | 1.42 | 3.24 | 15.4 | 33% |
|  | testapi alone (no proxy) | 47,832 | 47,593 (47,193 to 47,832) | 3 of 3 | 1.05 | 2.68 | n/a | 51% |

† The load generator's core was more than 90% busy in the best run, so the server could have gone faster: treat the number as a floor. ‡ Even the best run was disturbed by other processes (more than 10% of a core): the number is a floor too.

Ratios (computed):

| Case | req/s, nginx over BareProxy (best runs) | req/s, nginx over BareProxy (medians of all attempts) | CPU per request, BareProxy over nginx | p50, BareProxy over nginx (best runs) |
| --- | ---: | ---: | ---: | ---: |
| `GET /` (200, 11,596 bytes) | 2.5 | 2.4 | 2.5 | 2.7 |
| `GET /images/plan-demo.png` (200, 72,732 bytes) | 3.2 | 3.4 | 3.8 | 7.9 |
| `GET /no-such-page/` (404, 4,686 bytes) | 1.9 | 1.9 | 1.9 | 2.2 |
| `GET /api/orders` (200, 153 bytes) | 3.4 | 3.5 | 5.6 | 3.4 |

## What logging to a file costs

Change in best-run req/s when each server writes one log record per request to a file, against logging off (computed). BareProxy writes a JSON trace record; nginx writes its default access log line. Every request left a record: see the check below the table.

| Case | BareProxy | nginx |
| --- | ---: | ---: |
| `GET /` (200, 11,596 bytes) | -1.2% | -16.4% |
| `GET /images/plan-demo.png` (200, 72,732 bytes) | -0.3% | +2.8% |
| `GET /no-such-page/` (404, 4,686 bytes) | -3.3% | -10.3% |
| `GET /api/orders` (200, 153 bytes) | -5.8% | -6.3% |

Log check: BareProxy: 12 of 12 runs had at least as many log lines as requests counted by wrk (lowest ratio 1.0004; a few more lines than requests is normal, because requests still in flight when wrk stops are logged but not counted); nginx: 12 of 12 runs had at least as many log lines as requests counted by wrk (lowest ratio 1.0001; a few more lines than requests is normal, because requests still in flight when wrk stops are logged but not counted).

Average size of one log line, computed from file size over lines: BareProxy 502 bytes, nginx 88 bytes.

## Keep-alive check

Connections accepted on the machine during a measured run, from `/proc/net/snmp`; the lowest of the attempts of each case. The generator holds 50 connections, so a figure near 50 means every connection was reused for the whole run. The count is machine-wide, so tests that other workers run at the same time add to it: that is why the lowest attempt is shown. In the API case the count also includes the proxy's connections to the backend.

| Logging | Case | BareProxy | nginx |
| --- | --- | ---: | ---: |
| logging off | `GET /` (200, 11,596 bytes) | 51 | 51 |
| logging off | `GET /images/plan-demo.png` (200, 72,732 bytes) | 51 | 51 |
| logging off | `GET /no-such-page/` (404, 4,686 bytes) | 51 | 51 |
| logging off | `GET /api/orders` (200, 153 bytes) | 51 | 51 |
| logging to a file | `GET /` (200, 11,596 bytes) | 51 | 51 |
| logging to a file | `GET /images/plan-demo.png` (200, 72,732 bytes) | 51 | 51 |
| logging to a file | `GET /no-such-page/` (404, 4,686 bytes) | 51 | 51 |
| logging to a file | `GET /api/orders` (200, 153 bytes) | 51 | 51 |

## Memory

VmRSS from `/proc/PID/status`, summed over the processes of a server (nginx is its master plus its worker), read right after start and every 200 ms during each measured run. PSS (also summed) splits pages shared between processes, which matters for nginx because its master and worker share memory. Values are in MiB (computed from kB). The high-water mark is the kernel's VmHWM for the whole life of the server.

| Logging | Server | Idle RSS | Idle PSS | Peak RSS under load (highest of all runs) | Peak PSS under load | Peak RSS by case, median of runs (home / file / 404 / api) | High-water mark at the end |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| logging off | BareProxy | 7.6 | 7.6 | 111.1 | 111.1 | 106.4 / 106.4 / 106.5 / 110.9 | 118.2 |
| logging off | nginx | 11.3 | 6.0 | 12.3 | 6.5 | 12.3 / 12.3 / 12.3 / 12.3 | 12.3 |
| logging to a file | BareProxy | 7.6 | 7.6 | 111.8 | 111.8 | 107.1 / 107.1 / 106.1 / 111.2 | 119.9 |
| logging to a file | nginx | 11.4 | 6.1 | 12.4 | 6.6 | 12.4 / 12.4 / 12.4 / 12.4 | 12.4 |

BareProxy ran with its default `trace-memory 32MB` in every run, including "logging off", so its in-memory record store filled up under load. That store is most of the peak: when full it holds about 68,000 records in about 38 MB of heap (measured in the trace tests), and Go's garbage collector lets the heap grow to about twice what is live. `trace-memory off` would lower the peak; it was not measured in this run.

## Binary size

| What | Bytes | MiB (computed) | Notes |
| --- | ---: | ---: | --- |
| BareProxy as built | 11,852,861 | 11.3 | `go build`, default flags |
| BareProxy stripped | 8,069,383 | 7.7 | `-trimpath -ldflags="-s -w"` |
| testapi (stripped) | 5,878,023 | 5.6 | the test backend, for reference |
| nginx binary alone | 1,314,136 | 1.3 | `/usr/sbin/nginx`, Ubuntu package |
| nginx binary plus shared libraries | 10,386,480 | 9.9 | from `ldd`: libcrypt.so.1 0.2, libpcre2-8.so.0 0.6, libssl.so.3 0.7, libcrypto.so.3 5.1, libz.so.1 0.1, libc.so.6 2.0 (MiB each); the libraries are shared with the rest of the system |

Linkage of the BareProxy binary that was measured (`ldd`, run when this summary was made, only if the file still matches the hash in the log): static: no shared libraries. The nginx figure with libraries is for context only, since libc and the TLS library would be on the machine anyway.

## Environment

```
date: 2026-10-05 11:04:31 IDT (UTC+0300)
date utc: 2026-10-05 08:04:31
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
Mem:            8031         770        6744          13         811        7260
go (build tool): go version go1.27.1 linux/amd64
bareproxy binary: /home/claude/final/bareproxy
bareproxy version: bareproxy 0.1.0-alpha, built with go1.27.1
bareproxy binary sha256: a8587f81f998a1d0 (first 16 hex digits)
bareproxy binary built: 2026-10-05 11:04:25
bareproxy source tree at run time: git 0017b3f Merge branch 'release'; uncommitted files: 0
go version of the binary: /home/claude/final/bareproxy: go1.27.1
nginx: nginx version: nginx/1.24.0 (Ubuntu)
nginx build: nginx 1.24.0-2ubuntu7.18 nginx-light 1.24.0-2ubuntu7.18
nginx package: nginx: /usr/sbin/nginx
nginx configure flags: with-cc-opt with-ld-opt with-compat with-debug with-pcre-jit with-http_ssl_module with-http_stub_status_module with-http_realip
wrk: wrk debian/4.1.0-4build2 [epoll] Copyright (C) 2012 Will Glozer
h2load: h2load nghttp2/1.59.0
openssl: OpenSSL 3.0.13 30 Jan 2024 (Library: OpenSSL 3.0.13 30 Jan 2024)
python: 3.13.16
load average at start: 0.73 0.30 0.23
busiest processes at start (other agents share this machine):
PID %CPU %MEM     ELAPSED COMMAND
27585 25.9  0.2       00:00 python3 /home/claude/bench/bench.py
   69  3.4  5.3    02:08:02 /opt/node22/bin/claude --preload /home/claude/.claude/remote/spare.sock
    1  0.0  0.0    02:08:04 /process_api --firecracker-init --addr 0.0.0.0:2024 --max-ws-buffer-size 32768 --block-local-connections --listen-vsock-po
   97  0.0  0.5    02:07:40 /usr/local/bin/environment-manager task-run --stdin --session cse_01XhCycMqABUGhDzHctCRQVC --session-mode new --upgrade-cl
   43  0.0  0.0    02:08:04 [kworker/1:1H-kblockd]
settings: server cpu 0, load cpu 1, connections 50, warm-up 2 s, measured 10 s, runs 3, modes off,file, routes home,file,404,api; a run that other pro
ports: bareproxy 18080, nginx 18081, backend 19001
```

Load average (1 minute) before each measured run: lowest 0.83, highest 1.84. 
Attempts disturbed by other processes (they used more than 10% of the server or generator core): 0 of 51. Cases with no clean attempt at all: 0 of 17. CPU steal ticks seen during measured runs: 18.

## Setup

- Routes, the same on both servers: `/` is served from the public folder of the built bareproxy.com site (`index.html` for folders); `/api/*` goes to the test backend with the prefix stripped (BareProxy `route /api/* -> api strip`; nginx `location /api/` with `proxy_pass http://api/;`, an upstream with `keepalive 64`, HTTP/1.1 and an empty `Connection` header); both use the site's `404.html` for missing pages. Both add `X-Forwarded-For`, `-Proto` and `-Host` on the proxied request.
- BareProxy runs with `GOMAXPROCS=1` under `taskset -c 0`. nginx runs `worker_processes 1` under `taskset -c 0`, from its own config and prefix (`nginx -p` pointing at the bench's own nginx/ folder); the system nginx is never touched. The generator is `wrk -t1 -c50` under `taskset -c 1`, and this script runs there too.
- The test backend (`tools/testapi`) is pinned to the server's core, so in the API case the proxy and the backend share one core. The row `testapi alone` shows what the backend manages with the same generator and no proxy in between.
- Servers and cases take turns within each round (BareProxy, then nginx, case by case), so slow changes on the machine hit both alike.
- Logging off: BareProxy `trace-log off`, nginx `access_log off`. Logging to a file: BareProxy `trace-log FILE`, nginx `access_log FILE` with the default format and no buffering. The log file is emptied before each run.
- To repeat: `BAREPROXY=/path/to/binary ./bench.sh` in `live/bench/`, or `python3 summarize.py LOG` to rebuild this file from a log.

## Caveats

- **A shared two-CPU cloud VM.** This is a KVM guest with two vCPUs, shared with other workers who were building and testing at the same time, and the host under it is shared too. Run-to-run differences of several percent are normal here, and a run can be hit by a neighbour's burst. The tables show the range of runs. The flag ‡ comes from the busy time of a core that was not spent by the server, the backend, wrk or the harness (read from `/proc/stat`, in 10 ms ticks, so a few percent either way is rounding). Treat a difference under about 10% as noise.
- **One core each, by design.** The server gets one core and the generator the other. This compares the two servers per core. It does not show what either does with more cores (nginx with several workers, BareProxy with GOMAXPROCS above 1).
- **Latency follows throughput in this test.** Fifty connections each send the next request as soon as the last one is answered, so the average latency is about 50 divided by req/s. A server that is faster shows a lower p50 for that reason alone. The p99 shows the tail. Latency at an equal, fixed request rate is not measured here.
- **The generator can be the limit.** wrk runs on one thread. Where its core was above 90% busy (marked †), the server's number is a floor.
- **Localhost only.** No network between the machines, GET requests only, one route per run, small responses, plain HTTP only (no TLS, no HTTP/2 in this run). It is a measurement of the proxy and file paths, not a model of real traffic.
- **API case shares a core with the backend.** The proxy's own cost is the column `Server CPU per request`, taken from the CPU time of the server processes alone (computed from `/proc` ticks of 10 ms, over a 10 s run).
- **The two logs differ.** BareProxy's trace record is a JSON line with the routing decision; nginx's default line is shorter. Both servers write one line per request, and the log check above confirms it.
- **Keep-alive is set up alike on both sides.** nginx has `keepalive_requests` raised to 1,000,000 and an upstream pool of 64 idle connections; BareProxy's backend transport also keeps up to 64 idle connections per backend (checked in `pool.go` when this was written). The keep-alive table above shows how many connections each server really opened.
- **One build, one day.** BareProxy here is the 0.1.0-dev first cut as built at the start of the day; nginx is Ubuntu's 1.24.0 package. The numbers are for these two builds on this machine and not a general claim.

