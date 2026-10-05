# BareProxy 0.1.0-alpha

BareProxy is a small web server and reverse proxy that explains every routing decision. It terminates TLS, routes each request by host and path, and either serves it from a folder or proxies it to a pool of backends. For any request, `explain` says what would happen before it arrives, and `why` says what did happen after. `plan` says what a config change would do before it goes live.

Most sites and applications use a small part of nginx. The question behind this project is how little machinery it takes to provide the part of nginx that most applications use. BareProxy is one Go binary, built with Go's standard library only. The core does TLS, routing, static files, backend health, safe config changes and request tracing. Add-on modules come later.

This is version 0.1.0-alpha, released on 5 October 2026. It follows the 0.1.0-dev first cut of 2 October. It runs on Linux only for now (macOS and Windows come later) and is built with Go 1.27.1. It hasn't had an outside security review yet, so don't put it in front of anything that matters.

The whole project is open source under the Apache License 2.0. Copyright 2026 BareProxy.com.

## Install

The Linux binaries are in this repository, in [releases/v0.1.0-alpha](releases/v0.1.0-alpha/): `bareproxy-0.1.0-alpha-linux-amd64.tar.gz` for x86-64 and `bareproxy-0.1.0-alpha-linux-arm64.tar.gz` for 64-bit ARM, with `SHA256SUMS` and the [release notes](releases/v0.1.0-alpha/RELEASE-NOTES.md). The binary inside is static, so it needs no libraries on the machine.

```
V=0.1.0-alpha
curl -LO https://github.com/BareProxy/bareproxy/raw/main/releases/v$V/bareproxy-$V-linux-amd64.tar.gz
curl -LO https://github.com/BareProxy/bareproxy/raw/main/releases/v$V/SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS
tar xzf bareproxy-$V-linux-amd64.tar.gz
cd bareproxy-$V-linux-amd64
./bareproxy version
sudo install -m 0755 bareproxy /usr/local/bin/bareproxy
```

The folder also holds this README, LICENSE and NOTICE.

## What works

- **Config.** The grammar from the design note plus two additions: the `files` action and `error 404`. Every error names its line. Settings that are in the grammar but not built yet give a warning.
- **Static files.** `/about/` serves `about/index.html`, and `/about` redirects to `/about/` when it's a folder. Folders are never listed. Hidden names such as `.git` and `.env` are never served, except `/.well-known/`.
  - Files can't be reached outside the folder, through `..` or through symlinks. Relative symlinks that stay inside work; absolute symlinks are refused.
  - HEAD, conditional requests, byte ranges, ETag and Last-Modified all work.
  - Precompressed `.br` and `.gz` copies are sent to clients that accept them. Compression doesn't happen at run time.
  - The site's 404 page is found through the site's own rules. It's used only for 404s BareProxy makes itself.
- **Proxying.** Requests go to the backend with the fewest in flight, with ties going round robin. There are active health checks, and pools without checks count failed connections instead. One retry happens only when a connection can't be opened. The prefix can be stripped. BareProxy sets `X-Forwarded-For`, `-Host` and `-Proto` and passes the request ID on. WebSocket connections pass through.
- **HTTPS** from certificate files, TLS 1.2 or newer, with HTTP/2.
- **Tracing.** Every request that Go's HTTP server hands to BareProxy gets a `BareProxy-Id` header (`id-header off` turns it off) and leaves exactly one JSON record. The record leaves out the query string unless `trace-query on` is set. `why` turns a record back into a story.
  - **In-memory record store.** The latest records are also kept in memory: 32 MB of record JSON by default, set with `trace-memory SIZE`, or `trace-memory off`. `why`, `tail` and `status` read it, so they work even with `trace-log off`. It starts empty after a restart.
  - **Log rotation.** `trace-log FILE SIZE COUNT` (for example `trace-log /var/log/bareproxy/requests.log 10MB 5`) moves the file to `FILE.1` before it would pass SIZE. Older files shift up to `FILE.COUNT`, and the oldest is dropped. `why` looks through the rotated files too.
  - **traceparent.** A valid W3C `traceparent` header from the client goes on to the backend with BareProxy's request ID as the new parent ID, and the record keeps the trace ID. BareProxy never starts a trace of its own.
- **explain.** It asks the running server over its admin socket, so it shows live backend states. With no server running, or with `--offline`, it reads the config file.
- **why.** It looks in the running server's memory first, then in the trace log file and its rotated files. `--json` prints the record itself.
- **plan.** It says what a config would change, in classes of requests: for each class, how it was handled and how it would be handled. It also lists other changes (listeners, pools, certificates, globals) and warnings (rules that win nothing, pools no rule uses). `bareproxy plan FILE` compares the file with the running config, and `plan FILE --from OLD` compares two files with no server. For a very large change (over a million classes for one site), plan lists the changed rules instead.
- **apply with plan IDs.** `apply` checks the file, shows the plan, asks, and makes the file live. Every plan has an ID. `apply --plan ID` refuses if the running config or the file has changed since that plan was made, so you apply what you read. `--yes` skips the question, and it's needed when there's no terminal.
- **Rollback and history.** Every version that goes live is saved in the state folder (`/var/lib/bareproxy`, or the folder named by the global setting `state`). The last 100 versions are kept. `history` lists them with the time, how each went live (startup, apply, rollback or reload), the Unix user and the plan ID. `rollback` makes an earlier version live, by default the one that went live before the running version. Apply and rollback also write the config file, so it holds the running config.
- **Listener changes without a restart.** An apply opens new ports before it swaps the config, so a port that can't be opened stops the apply and nothing changes. Ports the new config doesn't have close once their requests in flight finish (30 seconds at most). A port that switches between http and https is closed and reopened. The admin socket path and the state folder can't change while BareProxy runs. An apply that changes them is refused.
- **Draining removed backends.** A backend that a new config drops gets no new requests. Requests that started on the old version can still reach it. Its connections close after the pool's `drain` time, 30 seconds by default.
- **Startup on the last good version.** If the config file has errors when BareProxy starts, it runs the newest version in the history instead and records the mismatch in `events`. With no history it refuses to start.
- **tail, status and events.** `tail` shows records as they happen, with filters that are all ANDed: `status>=500`, `status=404`, `pool=`, `site=`, `host=`, `outcome=`, `method=` and `path=/prefix`. `status` shows listeners, sites, backends, certificates and recent error rates. `events` lists recent changes: backends going up and down, certificates, reloads, applies and rollbacks. All three take `--json`.
- **Admin socket.** A Unix socket (`global admin PATH`, `/run/bareproxy/admin.sock` by default, mode 0660). `explain`, `why`, `plan`, `apply`, `rollback`, `history`, `tail`, `status` and `events` all talk to it, and `admin off` turns it off. Whoever can open the socket can change the config, so keep it to root or a trusted group. The history records the Unix user who made each change, read from the socket's peer credentials on Linux.
- **Reload on SIGHUP.** A config with errors never replaces the running one. Requests in flight finish on the version they started with. A reload that changes the config goes into the history like any other change.
- **Browser demo.** `src/cmd/bareproxy-wasm` compiles the same parser, matcher, explain and plan to WebAssembly. A visitor edits a config, checks it, asks how a request would be handled and sees what a change would do, all in the browser, with nothing sent anywhere. See [its README](src/cmd/bareproxy-wasm/README.md).

## Build and test

Only Go's standard library is used, so nothing is downloaded. The release binaries are built with Go 1.27.1.

```
cd src
export GOTOOLCHAIN=local GOPROXY=off
go build -o ../bin/bareproxy ./cmd/bareproxy
go build -o ../bin/testapi ./tools/testapi
go test ./...
go test -race ./...
```

The acceptance tests are in `src/internal/bp`, and their names start with `TestAccept`. To run just them with their output: `go test -count=1 -run TestAccept -v ./internal/bp/`.

`live/release.sh [DIR]` builds the release tarballs and `SHA256SUMS` into DIR (`dist/` by default): static binaries for linux/amd64 and linux/arm64, each with `-trimpath` and `-ldflags="-s -w"`. `sh src/cmd/bareproxy-wasm/build.sh DIR` builds the browser demo.

**Go version.** Build with Go 1.27.1. Go 1.24.7's `os.Root` follows a symlink out of the folder when a path ends in a slash (CVE-2026-39822), and `TestOSRootTrailingSlash` shows that Go 1.27.1 refuses it: `Open("link/")` fails. BareProxy never opens a path ending in a slash anyway, because it asks for `index.html` instead, so its lookups stay inside on either version.

## Run

```
bareproxy check site.conf
bareproxy run site.conf
bareproxy explain --config site.conf GET https://example.com/about/
bareproxy why --config site.conf 7f3a9c0d
```

While it runs:

```
bareproxy plan site.conf
bareproxy apply site.conf
bareproxy history --config site.conf
bareproxy rollback --config site.conf
bareproxy tail --config site.conf 'status>=500'
bareproxy status --config site.conf
bareproxy events --config site.conf
```

`bareproxy help` lists every command and option. FILE defaults to `$BAREPROXY_CONFIG`, then `/etc/bareproxy/bareproxy.conf`.

A complete config for a Hugo site with an API behind it:

```
global
  admin /run/bareproxy/admin.sock
  state /var/lib/bareproxy
  trace-log /var/log/bareproxy/requests.log 10MB 5

site example.com
  tls /etc/bareproxy/example.com.crt /etc/bareproxy/example.com.key
  error 404 /404.html
  route /api/* -> api strip
  route /* -> files /var/www/example/public

pool api
  backend 127.0.0.1:8080
  health /healthz
```

### Change a running config

Edit the file, then look at the plan before anything goes live. This is a real run, with a second pool added for `/api/v2`:

```
$ bareproxy plan site.conf
Compared with running version 1:
Plan 1caa15bccdf8: 1 routing change, 1 other change
Routing
  example.com (port 8088), any method, /api/v2 and below
      pool api, strip /api  ->  pool api2, strip /api/v2
Other changes
  pool api2 added (line 14), 1 backend

$ bareproxy apply site.conf
Compared with running version 1:
Plan 1caa15bccdf8: 1 routing change, 1 other change
Routing
  example.com (port 8088), any method, /api/v2 and below
      pool api, strip /api  ->  pool api2, strip /api/v2
Other changes
  pool api2 added (line 14), 1 backend
Apply? [y/N] y
Version 2 is running (it was 1).

$ bareproxy history --config site.conf
Version  Time (UTC)           How       User        Plan
      1  2026-10-05 07:55:25  startup   root
      2  2026-10-05 07:55:26  apply     root        1caa15bccdf8 (running)

$ bareproxy rollback --config site.conf
Plan 19b44ba3e7c3: 1 routing change, 1 other change
Routing
  example.com (port 8088), any method, /api/v2 and below
      pool api2, strip /api/v2  ->  pool api, strip /api
Other changes
  pool api2 removed (was line 14)
Version 3 is running (it was 2).
```

The rollback is a new version (3) that holds the text of version 1, and the config file holds that text again.

## Live test

`live/live-test.sh` builds the bareproxy.com Hugo site and adds bait files. It then starts BareProxy over HTTPS with two test API backends and one dead backend, and checks the following, printing everything it sees:

- static files, precompressed copies, 304s, byte ranges and HEAD
- refused paths and the API
- `explain` and `why`
- a backend failing, a broken reload and a good reload

```
HUGO=/path/to/hugo SITE=/path/to/bareproxy.com-main ./live/live-test.sh
```

## Measured on 5 October 2026 (Go 1.27.1, linux/amd64)

- **Tests.** 96 test functions, in 6,621 lines of test code. Blank lines and comments aren't counted.
- **Acceptance tests** (`results/accept-test.log`, 30 seconds on a shared 2-CPU machine):
  - 100,000 generated requests were each asked of `explain` first and then sent to a running server. `explain` agreed with the server on every one. The requests ran over 114 routes on 6 sites and 2 ports. The trace log held 100,000 records with 100,000 distinct IDs.
  - 67 broken configs. Every one is refused by the parser with an error (65 name a line, 2 are about the whole file), refused by a reload on a running server, and stops a start.
  - 30,000 generated paths, plus 127 written path cases on 2 sites (each with GET and HEAD) and 30 method cases. Nothing outside the folder was read, and all 4,763 requests for hidden names got 404.
  - 91 request smuggling payloads: Content-Length and Transfer-Encoding tricks, odd chunked bodies, folded headers, absolute-form targets, bad Host headers, Upgrade requests and pipelined requests. 34 were refused before the backend, 11 were answered by a rule and 46 were forwarded as one request. There were 0 problems: no request for a path that a rule refuses reached the backend, the backend never got more requests than the payload held, and every response has one record.
- **Plan exactness.** 1,000 generated config pairs with 300 random requests each (300,000 requests). Every request whose handling changes falls in a listed class with the right old and new effect, and no request whose handling stays the same does.
- **Applies under load.** 20 applies while requests ran over HTTP/1.1 and HTTP/2, with 0 failed requests.
- **Browser demo.** 113 checks pass in headless Chromium (`results/demo-check.log`). They compare the demo's check, explain and plan results with the native commands.
- **Code size.** The core package and the command are 6,422 lines of Go (5,556 in the core, 866 in the command), against the 5,000-line budget. That is 1,422 lines over. Blank lines and comments aren't counted.
- **Binary size.** 8.4 MB for linux/amd64 and 7.7 MB for linux/arm64, static and stripped (8,351,904 and 7,733,408 bytes).
- **Against nginx.** Measured on 5 October 2026 on a shared 2-CPU cloud machine (Intel Xeon at 2.1 GHz under KVM): each server on one core and the wrk load generator on the other, plain HTTP with keep-alive and 50 connections, 10-second runs, 3 per case, best run shown. All 51 attempts ran with the machine otherwise quiet. Logging was off for both servers; BareProxy kept its default 32 MB in-memory record store, which is most of its memory under load. The full tables, with the setup and caveats, are in `results/bench-summary.md`.

  | | BareProxy | nginx 1.24.0 |
  | --- | --- | --- |
  | Home page, requests per second | 20,872 | 61,593 |
  | Home page, p99 latency | 11.1 ms | 1.6 ms |
  | 72 KB image, requests per second | 17,084 | 53,809 (a floor: the load generator's core was full) |
  | Proxied API, requests per second | 10,430 | 35,414 |
  | Proxied API, p99 latency | 12.5 ms | 3.1 ms |
  | Memory when idle | 7.6 MiB | 11.3 MiB (6.0 MiB PSS) |
  | Memory under load, peak | 111 MiB, as the record store fills | 12.3 MiB |
- **Live test.** The live test of 2 October, on the 0.1.0-dev build, made 24 requests and got 24 records: 23 over HTTP/2 with TLS 1.3, and 1 over HTTP/1.1 (`results/live-test.log`).

## Not in this release

These settings are in the grammar, but they aren't built yet. The config check warns that BareProxy ignores them:

- `acme-email` and `acme-ca`
- `trust`
- `client-header-timeout`, `client-body-timeout`, `client-idle-timeout` and `shutdown-timeout`
- `set-response-header` and `remove-response-header`

`unix:` backends aren't built either, and a config that uses one is an error.

## Known limits

- **Automatic certificates aren't built.** TLS uses certificate files only. A site that serves HTTPS needs `tls CERT KEY`, and the config check says so when the line is missing.
- **`OPTIONS *` leaves no record.** Go's HTTP server answers it with 200 before BareProxy's handler runs, so it has no `BareProxy-Id` and no trace record. Go's server also answers requests it can't parse (a bad request line or header, an oversized header, an unknown HTTP version) with 400, 431, 501 or 505, and those leave no record either. A test for `OPTIONS *` is skipped on purpose and will fail once the behavior changes.
- **WebSocket tunnels aren't inspected.** After an upgrade the connection is a plain tunnel, so routing rules don't see what travels inside it.
- **macOS and Windows are untested.** Only Linux has been built and run.
- **Modules aren't built.** The core is the whole product in this release.
- **The core is over its line budget.** It is 6,422 lines (core and command) against 5,000.

## License

The entire project runs under the Apache License 2.0 for now. Premium add-ons may be offered later under different licenses. Copyright 2026 BareProxy.com. See [LICENSE](LICENSE) and [NOTICE](NOTICE). Everything in this repository is under Apache 2.0 unless a file says otherwise.

Questions and security reports: info@bareproxy.com. See [SECURITY.md](SECURITY.md).
