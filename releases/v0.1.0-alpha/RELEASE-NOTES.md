BareProxy is a small web server and reverse proxy that explains every routing decision. Most sites and applications use a small part of nginx. The question behind the project is how little machinery it takes to provide the part of nginx that most applications use.

0.1.0-alpha is the second cut. It runs on Linux only for now (macOS and Windows come later) and is built with Go 1.27.1, with Go's standard library and, for automatic certificates, the Go team's `golang.org/x/crypto`. It hasn't had an outside security review yet, so don't put it in front of anything that matters.

## Finished on 5 October 2026, evening

The binaries in this folder were rebuilt again, from the same version line, with the alpha finished:

- The gate 1 review's minor items are fixed: apply shows each warning once, `check` warns about rules that can never match, events say what an apply changed, removed backends stay in `status` while they drain, and the admin socket message says what happened.
- The load test at the design's full size passes: 2,000 requests per second over HTTP/1.1, HTTP/2 and WebSocket for 60 seconds, with 20 applies, and 0 failed requests, resets or lost messages. A pool that an apply adds, or whose only backend it replaces, now serves at once instead of answering 503 until its first health check.
- The in-memory record store defaults to 8 MB (was 32 MB): peak memory under the load test is 46 MiB (was 111 MiB).
- The proxy path is faster: about 72 instead of 80 microseconds of server CPU per proxied request, and 10.3 KB allocated instead of 43.7 KB.
- New measurements against nginx on this build (below).

## Rebuilt on 5 October 2026, afternoon

The binaries in this folder were rebuilt from the same version line with these changes:

- Automatic certificates: an https site without certificate files gets its certificate from Let's Encrypt or any ACME CA (`acme-ca`, `acme-email`), with renewal 30 days before expiry. Tested against Let's Encrypt's Pebble test CA.
- `OPTIONS *` is answered by BareProxy and leaves a record.
- From the gate 1 review: `status` says when the config file doesn't hold the running config, apply and rollback say when they rewrote the file, a symlinked config stays a symlink, and an apply with no changes still goes to the server (it reloads certificate files).
- `plan` reads more plainly ("every path except ...", settings as config lines) and the command's errors use one wording each. The core and the command are 5,217 lines of Go, under the 5,000-line budget once file serving (235 lines, its own budget) is set apart.
- Built with Go 1.27.1; golang.org/x/crypto, x/net and x/text are vendored.

## What's new since 0.1.0-dev

- `plan` says what a config change would do, in classes of requests, before it goes live.
- `apply` makes a config live. `apply --plan ID` refuses if anything changed since that plan was made, so you apply what you read.
- `rollback` and `history`. The last 100 config versions are kept in a state folder, with the time, how each went live, the Unix user and the plan ID.
- Listeners change without a restart, and removed backends drain. If the config file has errors at startup, BareProxy runs the last good version.
- The admin socket carries `explain`, `why`, `plan`, `apply`, `rollback`, `history`, `tail`, `status` and `events`.
- `tail`, `status` and `events` show requests, state and changes as they happen.
- The latest records are kept in memory, and `why` looks there first. A trace log file can rotate. A valid `traceparent` header goes on to the backend.
- A browser demo runs the same parser, explain and plan, compiled to WebAssembly.
- Fixes found by the acceptance tests: only WebSocket upgrades are forwarded (an h2c upgrade could carry a request past a rule), a bad chunk in a request body gets 400, `explain` keeps the query string on a folder redirect and treats a raw backslash the way the server does, static files go out with sendfile where the platform allows, and a backend's response headers are limited to 64 KB.
- Release binaries for linux/amd64 and linux/arm64. They are static and have no dependencies.

## Install

The files are in this folder. Pick the tarball for your CPU: `bareproxy-0.1.0-alpha-linux-amd64.tar.gz` for x86-64, or `bareproxy-0.1.0-alpha-linux-arm64.tar.gz` for 64-bit ARM. Check it against `SHA256SUMS` before you unpack it.

```
VERSION=0.1.0-alpha
curl -LO https://github.com/BareProxy/bareproxy/raw/main/releases/v$VERSION/bareproxy-$VERSION-linux-amd64.tar.gz
curl -LO https://github.com/BareProxy/bareproxy/raw/main/releases/v$VERSION/SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS
tar xzf bareproxy-$VERSION-linux-amd64.tar.gz
cd bareproxy-$VERSION-linux-amd64
./bareproxy version
```

The check should print `bareproxy-0.1.0-alpha-linux-amd64.tar.gz: OK`. The folder holds the `bareproxy` binary, README.md, LICENSE and NOTICE. Then write a config, run `bareproxy check` on it, and start it with `bareproxy run`. The [README](https://github.com/BareProxy/bareproxy#readme) has a complete example config.

## Measured

- 129 test functions; `go test` and `go test -race` pass. Acceptance tests: `explain` agreed with the live server on 100,000 generated requests, 71 broken configs were all refused, 30,000 generated paths read nothing outside the folder, and 91 request smuggling payloads got 0 problems.
- Plan exactness holds on 1,000 generated config pairs (300,000 requests).
- The load test at the design's full size: 120,020 requests and WebSocket messages in 60 seconds with 20 applies, 0 failed, in all 14 runs.
- The core and the command are 5,225 lines of Go; 4,990 of them count against the 5,000-line budget once file serving (235 lines, its own budget) is set apart. Binary size: 8.9 MB for linux/amd64 and 8.3 MB for linux/arm64, static and stripped (8,917,152 and 8,257,696 bytes).
- Against nginx 1.24.0 on the same routes, measured on 5 October 2026 on this build, on a shared 2-CPU cloud machine (Intel Xeon at 2.1 GHz under KVM) with nothing else running: each server on one core and the wrk load generator on the other, plain HTTP with keep-alive and 50 connections, 10-second runs, 3 per case, best run shown. Logging was off for both servers; BareProxy kept its default 8 MB in-memory record store. The full tables are in [results/bench-summary.md](https://github.com/BareProxy/bareproxy/blob/main/results/bench-summary.md).

  | | BareProxy | nginx 1.24.0 |
  | --- | --- | --- |
  | Home page, requests per second | 21,819 | 66,136 |
  | Home page, p99 latency | 28.3 ms (8.1 to 28.3 over the 3 runs) | 1.5 ms |
  | 72 KB image, requests per second | 17,769 | 57,095 (a floor: the load generator's core was full) |
  | Missing page (404), requests per second | 24,278 | 46,762 |
  | Proxied API, requests per second | 12,055 | 35,432 |
  | Proxied API, p99 latency | 9.1 ms | 3.6 ms |
  | Memory when idle | 8.0 MiB | 11.4 MiB (6.0 MiB PSS) |
  | Memory under load, peak | 38.8 MiB | 12.4 MiB |

## Known limits

- Automatic certificates are tested against Pebble only, not yet against Let's Encrypt itself.
- Requests that Go's server can't parse get its own error and no record.
- After a WebSocket upgrade the connection is a plain tunnel, so routing rules don't see what travels inside it. When an apply removes a backend, the tunnels through it close at the end of the drain time.
- macOS and Windows are untested.
- Modules aren't built.

See the [README](https://github.com/BareProxy/bareproxy#readme) for the full list and the [CHANGELOG](https://github.com/BareProxy/bareproxy/blob/main/CHANGELOG.md) for details.

## License

The entire project runs under the Apache License 2.0 for now. Premium add-ons may be offered later under different licenses. Copyright 2026 BareProxy.com. Questions and security reports: info@bareproxy.com.
