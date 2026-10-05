BareProxy is a small web server and reverse proxy that explains every routing decision. Most sites and applications use a small part of nginx. The question behind the project is how little machinery it takes to provide the part of nginx that most applications use.

0.1.0-alpha is the second cut. It runs on Linux only for now (macOS and Windows come later) and is built with Go 1.27.1, standard library only. It hasn't had an outside security review yet, so don't put it in front of anything that matters.

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

- 96 test functions. Acceptance tests: `explain` agreed with the live server on 100,000 generated requests, 67 broken configs were all refused, 30,000 generated paths read nothing outside the folder, and 91 request smuggling payloads got 0 problems.
- Plan exactness holds on 1,000 generated config pairs (300,000 requests). 20 applies under load had 0 failed requests.
- The core and the command are 6,422 lines of Go, against a 5,000-line budget. Binary size: 8.4 MB for linux/amd64 and 7.7 MB for linux/arm64, static and stripped (8,351,904 and 7,733,408 bytes).
- Against nginx 1.24.0 on the same routes: Measured on 5 October 2026 on a shared 2-CPU cloud machine (Intel Xeon at 2.1 GHz under KVM): each server on one core and the wrk load generator on the other, plain HTTP with keep-alive and 50 connections, 10-second runs, 3 per case, best run shown. All 51 attempts ran with the machine otherwise quiet. Logging was off for both servers; BareProxy kept its default 32 MB in-memory record store, which is most of its memory under load. The full tables are in [results/bench-summary.md](https://github.com/BareProxy/bareproxy/blob/main/results/bench-summary.md).

  | | BareProxy | nginx 1.24.0 |
  | --- | --- | --- |
  | Home page, requests per second | 20,872 | 61,593 |
  | Home page, p99 latency | 11.1 ms | 1.6 ms |
  | 72 KB image, requests per second | 17,084 | 53,809 (a floor: the load generator's core was full) |
  | Proxied API, requests per second | 10,430 | 35,414 |
  | Proxied API, p99 latency | 12.5 ms | 3.1 ms |
  | Memory when idle | 7.6 MiB | 11.3 MiB (6.0 MiB PSS) |
  | Memory under load, peak | 111 MiB, as the record store fills | 12.3 MiB |

## Known limits

- Automatic certificates aren't built. TLS uses certificate files only.
- `OPTIONS *` is answered by Go's HTTP server and leaves no record. Requests that Go's server can't parse get its own error and no record either.
- After a WebSocket upgrade the connection is a plain tunnel, so routing rules don't see what travels inside it.
- macOS and Windows are untested.
- Modules aren't built.
- The core is over its line budget.

See the [README](https://github.com/BareProxy/bareproxy#readme) for the full list and the [CHANGELOG](https://github.com/BareProxy/bareproxy/blob/main/CHANGELOG.md) for details.

## License

The entire project runs under the Apache License 2.0 for now. Premium add-ons may be offered later under different licenses. Copyright 2026 BareProxy.com. Questions and security reports: info@bareproxy.com.
