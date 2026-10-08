# BareProxy

BareProxy is a small web server and reverse proxy that explains every routing decision. It terminates TLS, routes each request by host and path, and either serves it from a folder or proxies it to a pool of backends. For any request, `explain` says what would happen before it arrives, and `why` says what did happen after. `plan` says what a config change would do before it goes live.

Most sites and applications use a small part of nginx. The question behind this project is how little machinery it takes to provide the part of nginx that most applications use. BareProxy is one Go binary, built with Go's standard library, the Go team's own `golang.org/x/crypto` for automatic certificates, and wazero for plugins. The core does TLS, routing, static files, backend health, safe config changes and request tracing. Everything else comes as plugins: WebAssembly modules loaded at run time, sandboxed, written in any language, using the Proxy-Wasm interface that Envoy and Istio use. This release has the plugin host; the plugins themselves come next. The [plugin design](docs/plugins.md) and the [plugin program](docs/plugin-program.md) say how and in what order.

This is version 0.2.0, released on 8 October 2026. It adds the plugin host to 0.1.0 of the same day, which followed the 0.1.0-alpha of 5 October and the 0.1.0-dev first cut of 2 October. Binaries are built for Linux, macOS and Windows with Go 1.27. Linux is where it has been run in earnest and measured; see Known limits for the others. It hasn't had an outside security review yet, so don't put it in front of anything that matters.

The whole project is open source under the Apache License 2.0. Copyright 2026 BareProxy.com.

## Install

Ready-built binaries are on the [releases page](https://github.com/BareProxy/bareproxy/releases/latest). Each archive holds the `bareproxy` command with this README, LICENSE and NOTICE, in a folder of the same name as the archive. The links below always get the newest release.

| System | Archive |
|---|---|
| Linux, x86-64 (static) | [bareproxy_linux_amd64.tar.gz](https://github.com/BareProxy/bareproxy/releases/latest/download/bareproxy_linux_amd64.tar.gz) |
| Linux, 64-bit ARM (static) | [bareproxy_linux_arm64.tar.gz](https://github.com/BareProxy/bareproxy/releases/latest/download/bareproxy_linux_arm64.tar.gz) |
| macOS, Apple silicon | [bareproxy_darwin_arm64.tar.gz](https://github.com/BareProxy/bareproxy/releases/latest/download/bareproxy_darwin_arm64.tar.gz) |
| macOS, Intel | [bareproxy_darwin_amd64.tar.gz](https://github.com/BareProxy/bareproxy/releases/latest/download/bareproxy_darwin_amd64.tar.gz) |
| Windows, x86-64 | [bareproxy_windows_amd64.zip](https://github.com/BareProxy/bareproxy/releases/latest/download/bareproxy_windows_amd64.zip) |

On Linux:

```
U=https://github.com/BareProxy/bareproxy/releases/latest/download
curl -LO $U/bareproxy_linux_amd64.tar.gz
curl -LO $U/SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS
tar xzf bareproxy_linux_amd64.tar.gz
cd bareproxy_linux_amd64
./bareproxy version
sudo install -m 0755 bareproxy /usr/local/bin/bareproxy
```

The Linux binaries are static, so they need no libraries on the machine. The macOS binaries aren't signed by Apple, so macOS may ask you to allow them the first time, or run `xattr -d com.apple.quarantine bareproxy`. Earlier versions and their notes are on the [releases page](https://github.com/BareProxy/bareproxy/releases) and in the [CHANGELOG](CHANGELOG.md).

## What works

- **Config.** The grammar from the design note plus two additions: the `files` action and `error 404`. Every error names its line. Settings that are in the grammar but not built yet give a warning.
- **Static files.** `/about/` serves `about/index.html`, and `/about` redirects to `/about/` when it's a folder. Folders are never listed. Hidden names such as `.git` and `.env` are never served, except `/.well-known/`.
  - Files can't be reached outside the folder, through `..` or through symlinks. Relative symlinks that stay inside work; absolute symlinks are refused.
  - HEAD, conditional requests, byte ranges, ETag and Last-Modified all work.
  - Precompressed `.br` and `.gz` copies are sent to clients that accept them. Compression doesn't happen at run time.
  - The site's 404 page is found through the site's own rules. It's used only for 404s BareProxy makes itself.
- **Proxying.** Requests go to the backend with the fewest in flight, with ties going round robin. There are active health checks, and pools without checks count failed connections instead. A backend still waiting for its first health check takes requests only when no backend in its pool is up, so a pool that an apply adds, or whose backends it replaces, serves at once. One retry happens only when a connection can't be opened. The prefix can be stripped. BareProxy sets `X-Forwarded-For`, `-Host` and `-Proto` and passes the request ID on. WebSocket connections pass through.
- **HTTPS** with automatic certificates or certificate files, TLS 1.2 or newer, with HTTP/2.
  - An https site with `tls auto`, or with no `tls` line, gets its certificate from an ACME CA on first use: `acme-ca` names the CA's directory URL (Let's Encrypt by default) and `acme-email` the account's contact. BareProxy accepts the CA's terms of service for you. Certificates are kept in `<state>/certs` and renewed 30 days before they expire.
  - The CA can check the name in two ways: TLS-ALPN-01 on the site's own port, or HTTP-01 through the port-80 redirect, which answers `/.well-known/acme-challenge/` itself (outcome `local`, one record each).
  - Only exact host names get automatic certificates: a `*.example.com` or `*` site needs `tls CERT KEY`, and the config check names the line. BareProxy asks only for the https host names of the running config, and the list follows every apply.
  - `explain` says where a site's certificate comes from, `status` lists automatic certificates from the cache with their end dates, and `events` notes each certificate obtained and the first failure for a name.
- **Tracing.** Every request that Go's HTTP server hands to BareProxy gets a `BareProxy-Id` header (`id-header off` turns it off) and leaves exactly one JSON record. The record leaves out the query string unless `trace-query on` is set. `why` turns a record back into a story.
  - **In-memory record store.** The latest records are also kept in memory: 8 MB of record JSON by default (about 17,000 typical records), set with `trace-memory SIZE`, or `trace-memory off`. The store is most of BareProxy's memory under load: 46 MiB at the peak of the load test with 8 MB, 25 MiB with the store off (`results/memory-test.log`). `why`, `tail` and `status` read it, so they work even with `trace-log off`. It starts empty after a restart.
  - **Log rotation.** `trace-log FILE SIZE COUNT` (for example `trace-log /var/log/bareproxy/requests.log 10MB 5`) moves the file to `FILE.1` before it would pass SIZE. Older files shift up to `FILE.COUNT`, and the oldest is dropped. `why` looks through the rotated files too.
  - **traceparent.** A valid W3C `traceparent` header from the client goes on to the backend with BareProxy's request ID as the new parent ID, and the record keeps the trace ID. BareProxy never starts a trace of its own.
- **explain.** It asks the running server over its admin socket, so it shows live backend states. With no server running, or with `--offline`, it reads the config file.
- **why.** It looks in the running server's memory first, then in the trace log file and its rotated files. `--json` prints the record itself.
- **plan.** It says what a config would change, in classes of requests: for each class, how it was handled and how it would be handled. It also lists other changes (listeners, pools, certificates, globals) and warnings (rules that win nothing, pools no rule uses). `check` gives the same warnings, and so does a startup. `bareproxy plan FILE` compares the file with the running config, and `plan FILE --from OLD` compares two files with no server. For a very large change (over a million classes for one site), plan lists the changed rules instead.
- **apply with plan IDs.** `apply` checks the file, shows the plan, asks, and makes the file live. Every plan has an ID. `apply --plan ID` refuses if the running config or the file has changed since that plan was made, so you apply what you read. `--yes` skips the question, and it's needed when there's no terminal.
- **Rollback and history.** Every version that goes live is saved in the state folder (`/var/lib/bareproxy`, or the folder named by the global setting `state`). The last 100 versions are kept. `history` lists them with the time, how each went live (startup, apply, rollback or reload), the Unix user and the plan ID. `rollback` makes an earlier version live, by default the one that went live before the running version. Apply and rollback also write the config file, so it holds the running config.
- **Listener changes without a restart.** An apply opens new ports before it swaps the config, so a port that can't be opened stops the apply and nothing changes. Ports the new config doesn't have close once their requests in flight finish (30 seconds at most). A port that switches between http and https is closed and reopened. The admin socket path and the state folder can't change while BareProxy runs. An apply that changes them is refused.
- **Draining removed backends.** A backend that a new config drops gets no new requests. Requests that started on the old version can still reach it. Its connections close after the pool's `drain` time, 30 seconds by default, and that includes WebSocket connections through it, in use or not. `status` lists it as draining until then, and `events` notes the drain and its end.
- **Startup on the last good version.** If the config file has errors when BareProxy starts, it runs the newest version in the history instead and records the mismatch in `events`. With no history it refuses to start.
- **tail, status and events.** `tail` shows records as they happen, with filters that are all ANDed: `status>=500`, `status=404`, `pool=`, `site=`, `host=`, `outcome=`, `method=` and `path=/prefix`. `status` shows listeners, sites, backends (draining ones too), certificates and recent error rates, and warns when the config file doesn't hold the running config. `events` lists recent changes: backends going up and down, certificates, drains, reloads, applies and rollbacks, each with the plan it applied ("version 4 running (apply by root): plan c78b089a8272, 1 routing change"). All three take `--json`.
- **Admin socket.** A Unix socket (`global admin PATH`, `/run/bareproxy/admin.sock` by default, mode 0660). `explain`, `why`, `plan`, `apply`, `rollback`, `history`, `tail`, `status` and `events` all talk to it, and `admin off` turns it off. Whoever can open the socket can change the config, so keep it to root or a trusted group. The history records the Unix user who made each change, read from the socket's peer credentials on Linux.
- **Reload on SIGHUP.** A config with errors never replaces the running one. Requests in flight finish on the version they started with. A reload that changes the config goes into the history like any other change.
- **Browser demo.** `src/cmd/bareproxy-wasm` compiles the same parser, matcher, explain and plan to WebAssembly. A visitor edits a config, checks it, asks how a request would be handled and sees what a change would do, all in the browser, with nothing sent anywhere. See [its README](src/cmd/bareproxy-wasm/README.md).

## Build and test

The outside modules are vendored (see Dependencies below), so nothing is downloaded. The release binaries are built with Go 1.27.1. The plugin tests build a test plugin (`src/internal/plugin/testdata/fixture`) with the same Go toolchain for `GOOS=wasip1 GOARCH=wasm`, which every Go install can do.

```
cd src
export GOTOOLCHAIN=local GOPROXY=off
go build -o ../bin/bareproxy ./cmd/bareproxy
go build -o ../bin/testapi ./tools/testapi
go test ./...
go test -race ./...
```

The acceptance tests are in `src/internal/bp`, and their names start with `TestAccept`. To run just them with their output: `go test -count=1 -run TestAccept -v ./internal/bp/`.

`live/release.sh [DIR]` builds the release archives and `SHA256SUMS` into DIR (`dist/` by default): binaries for linux/amd64, linux/arm64, darwin/arm64, darwin/amd64 and windows/amd64, each with `CGO_ENABLED=0`, `-trimpath` and `-ldflags="-s -w"`. `live/smoke.sh BINARY` runs a binary the way a new user would: version, check, explain, serve a page and a 404, status. `sh src/cmd/bareproxy-wasm/build.sh DIR` builds the browser demo.

**Go version.** Build with Go 1.27.1. Go 1.24.7's `os.Root` follows a symlink out of the folder when a path ends in a slash (CVE-2026-39822), and `TestOSRootTrailingSlash` shows that Go 1.27.1 refuses it: `Open("link/")` fails. BareProxy never opens a path ending in a slash anyway, because it asks for `index.html` instead, so its lookups stay inside on either version.

### Releases

Releases go out on their own. The test workflow runs `go vet`, `go test` (with `-race` on Linux) on Linux and macOS for every push to main, checks the vendored modules against Go's checksum database, and builds every release target. When it passes and the `Version` in `src/internal/bp/server.go` has no tag yet, the release workflow builds the archives from that commit, smoke-tests them on Linux x86-64, Linux ARM, macOS and Windows, and only then tags the commit `vX.Y.Z` and publishes the release with the archives, `SHA256SUMS` and that version's section of the CHANGELOG. So a release is: raise the version, add its CHANGELOG section, push.

### Dependencies

BareProxy uses two modules from outside Go's standard library: `golang.org/x/crypto` v0.57.0, for `acme/autocert`, and `github.com/tetratelabs/wazero` v1.12.0, the WebAssembly runtime for plugins (pure Go, Apache 2.0). They bring in `golang.org/x/net` v0.58.0 (for `idna`), `golang.org/x/text` v0.42.0 and `golang.org/x/sys` v0.48.0. All of them are vendored in `src/vendor/`, so a fresh clone builds and tests with no network. CI checks `go.sum` against Go's checksum database on every push.

The Go module proxy (proxy.golang.org) wasn't reachable when they were added. `live/vendor-deps.sh` took them from GitHub's mirror of the Go repositories at those release tags, packed them as standard module zips and ran `go get` with `GONOSUMDB=golang.org/x`, so `go.sum` was computed from those copies. Once the proxy is reachable, check them against the official copies:

```
cd src
export GOFLAGS=-mod=mod GOMODCACHE=$(mktemp -d)
go mod download && go mod verify
```

`live/acme-test.sh` runs the whole ACME flow against Pebble, Let's Encrypt's test CA, with a real TLS-ALPN-01 check (`results/acme-test.log`). It uses Pebble v2.8.0: later releases answer the finalize request without the `Location` header that this version of `golang.org/x/crypto/acme` follows. `go test` runs the same flow when `PEBBLE` names the pebble binary (and `PEBBLE_CHALLTESTSRV` names pebble-challtestsrv for the real check).

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

Edit the file, then look at the plan before anything goes live. This is a real run, with a second pool added for `/api/v2` (the folder it ran in is shown as /srv/example):

```
$ bareproxy plan site.conf
Compared with running version 1:
Plan c78b089a8272: 1 routing change, 1 other change
Routing
  example.com (port 8088), any method, /api/v2 and below
      pool api, strip /api  ->  pool api2, strip /api/v2
Other changes
  pool api2 added (line 15): backend 127.0.0.1:19082

$ bareproxy apply site.conf
Compared with running version 1:
Plan c78b089a8272: 1 routing change, 1 other change
Routing
  example.com (port 8088), any method, /api/v2 and below
      pool api, strip /api  ->  pool api2, strip /api/v2
Other changes
  pool api2 added (line 15): backend 127.0.0.1:19082
Apply? [y/N] y
Version 2 is running (it was 1).

$ bareproxy history -c site.conf
Version  Time (UTC)           How       User        Plan
      1  2026-10-05 10:52:52  startup   root
      2  2026-10-05 10:52:53  apply     root        c78b089a8272 (running)

$ bareproxy rollback -c site.conf
Plan a241a09c11fa: 1 routing change, 1 other change
Routing
  example.com (port 8088), any method, /api/v2 and below
      pool api2, strip /api/v2  ->  pool api, strip /api
Other changes
  pool api2 removed (was line 15)
Version 3 is running (it was 2).
/srv/example/site.conf now holds version 3.
```

The rollback is a new version (3) that holds the text of version 1, and the config file holds that text again; the reply says so whenever an apply or a rollback rewrites the file.

## Plugins

A plugin is a WebAssembly module written to the Proxy-Wasm ABI (0.2.1, or 0.2.0). It's declared once in a `plugin` block and run by the sites that `use` it, in the order they name it:

```
plugin crawlers /etc/bareproxy/plugins/crawlers.wasm
  config /etc/bareproxy/plugins/crawlers.json
  timeout 5ms
  on-error open

site example.com
  use crawlers
  route /* -> files /var/www/example/public
```

| Setting | Default | What it does |
|---|---|---|
| `config FILE` | none | Handed to the plugin's `proxy_on_configure` as it is |
| `memory SIZE` | 64MB | The most memory the module may use (1MB to 4GB) |
| `timeout DURATION` | 5ms | The longest one call into the plugin may run; a call past it stops the instance |
| `pause DURATION` | 30s | The longest a plugin may keep a request waiting (on an outgoing call, say) |
| `instances N` | up to 4 | Copies of the module; each runs one call at a time |
| `on-error open\|closed` | closed | When the plugin fails: go on without it, or answer 502 |
| `allow-http HOST:PORT ...` | none | Addresses the plugin may call with `proxy_http_call` |
| `read DIR` | none | A folder it may read with `bareproxy_read_file` |
| `store SIZE` | off | A key-value store of its own on disk, in the state folder |
| `body-limit SIZE` | 8MB | The largest request or response body it is handed |

**What runs when.** A site's plugins see a request after the site is found and before the path is normalized and routed, so a plugin can change the method, the path and the headers, and the core routes what it gets. A plugin can answer the request itself, and the plugins after it don't run. On the way back the plugins see the response headers, then, if a plugin asks for it, the whole body, last plugin first. The body isn't handed over when it is over the body limit, partial (206), compressed, a stream of events, or flushed while it is sent; those go out as they are, with a note in the record. `proxy_on_log` runs once the response is sent.

**The sandbox.** A plugin gets WASI with no files, no environment and no arguments (clocks, random numbers, and its output going to BareProxy's log, at most 50 lines a second), the Proxy-Wasm host functions, and BareProxy's own functions through `proxy_call_foreign_function`: `bareproxy_note` (a line in the request's record and in `why`), `bareproxy_store_get`, `_put` and `_delete`, and `bareproxy_read_file`. Shared queues and gRPC calls aren't built. An outgoing call goes only to an address an `allow-http` line names. A plugin that traps or runs past its time limit loses its instance; the request follows `on-error`, and a new instance starts in the background, more slowly after repeated failures.

**The core's promises hold.** `check` compiles each module and refuses one that isn't a Proxy-Wasm plugin or imports something BareProxy doesn't provide. `plan` lists a plugin file or plugin config file that changed under the same config text, and a changed plugin order. The history keeps the plugin files and plugin config files each version ran, so `rollback` runs the exact module that ran before, not whatever is at the path now (`status` says "run from the history's copy"). Every request leaves one record, and `why` shows what each plugin did, how long it took and its notes. `explain` lists a site's plugins but doesn't run them. `status` shows each plugin's working instances and the metrics it defined.

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

- **Tests.** 129 test functions, in 8,056 lines of test code. Three run only when asked: a golden check and a timing check for `plan` (`BP_PLAN_GOLDEN`) and the test against the Pebble CA (`PEBBLE`). `go test ./...` and `go test -race ./...` pass (`results/go-test.log`, `results/go-test-race.log`). Blank lines and comments aren't counted.
- **Acceptance tests** (`results/accept-test.log`, 29 seconds on a shared 2-CPU machine):
  - 100,000 generated requests were each asked of `explain` first and then sent to a running server. `explain` agreed with the server on every one. The requests ran over 114 routes on 6 sites and 2 ports. The trace log held 100,000 records with 100,000 distinct IDs.
  - 71 broken configs. Every one is refused by the parser with an error (69 name a line, 2 are about the whole file), refused by a reload on a running server, and stops a start.
  - 30,000 generated paths, plus 127 written path cases on 2 sites (each with GET and HEAD) and 30 method cases. Nothing outside the folder was read, and all 4,763 requests for hidden names got 404.
  - 91 request smuggling payloads: Content-Length and Transfer-Encoding tricks, odd chunked bodies, folded headers, absolute-form targets, bad Host headers, Upgrade requests and pipelined requests. 34 were refused before the backend, 11 were answered by a rule and 46 were forwarded as one request. There were 0 problems: no request for a path that a rule refuses reached the backend, the backend never got more requests than the payload held, and every response has one record.
- **Plan exactness.** 1,000 generated config pairs with 300 random requests each (300,000 requests). Every request whose handling changes falls in a listed class with the right old and new effect, and no request whose handling stays the same does.
- **Load test at the design's full size** (`results/load-test.log`). `live/load-test.sh` sends 2,000 requests per second for 60 seconds: 800 over HTTP/1.1, 800 over HTTP/2 with TLS, and 400 WebSocket messages on 20 connections (half of them over TLS). During the run, 20 applies add and remove backends, move a route between pools and change a response. Result: 120,020 requests and messages, 0 failed, 0 connections reset, 0 WebSocket messages lost, and after each apply the next requests already followed the new config. On a quiet machine the p99 latency was 2.5 ms over HTTP/1.1, 2.7 ms over HTTP/2 and 2.3 ms for a WebSocket echo. All 14 full runs on 5 October passed, some of them while other builds and tests kept the machine busy. `live/load-experiments.sh` (`results/load-experiments.log`) runs five cases the applies stay clear of: a route moved to a new pool, a changed health line and a replaced only backend lose no requests (each lost 1 to 2.5 requests per apply before the fix in this build), and a WebSocket connection through a removed backend closes when the drain time ends.
- **Memory.** The peak under the load test is 46 MiB with the default 8 MB record store, 25 MiB with the store off and 111 MiB with the old 32 MB default (`results/memory-test.log`).
- **Browser demo.** 114 checks pass in headless Chromium (`results/demo-check.log`). They compare the demo's check, explain and plan results with the native commands.
- **Code size.** Blank lines and comments aren't counted. Without plugins, the core package and the command are 5,225 lines of Go (4,767 in the core, 458 in the command), and with file serving's own budget (`files.go`, 235 lines) taken out, 4,990 lines count against the core's 5,000-line budget. The alpha first shipped at 6,422 lines; trims on 5 October took it down, and automatic certificates added about 150. The plugin host in 0.2.0 is 2,416 lines: 1,464 in `internal/plugin`, 740 in `plugins_config.go` and `plugins_run.go`, and 212 of hook-ups in the core's files. That is over the 2,000 lines the design set for it, and trimming it is on the list.
- **Binary size.** 11.6 MB for linux/amd64 with the plugin host (11,632,800 bytes), static and stripped; 8.9 MB before it (8,917,152 bytes), and 8.4 MB for the first build of the alpha, before automatic certificates.
- **Speed work.** For a proxied request, the server's CPU time fell from about 80 to 72 microseconds in the speed runs, and the memory it allocates from 43.7 KB to 10.3 KB: the copy buffers come from a pool, each attempt copies the request without cloning its headers, and the server matches routes without writing explain's notes.
- **Against nginx.** Measured on 5 October 2026 on this build, on a shared 2-CPU cloud machine (Intel Xeon at 2.1 GHz under KVM) with nothing else running: each server on one core and the wrk load generator on the other, plain HTTP with keep-alive and 50 connections, 10-second runs, 3 per case, best run shown. Logging was off for both servers; BareProxy kept its default 8 MB in-memory record store. The full tables, with logging to a file, the setup and the caveats, are in `results/bench-summary.md`. Per core, nginx handled 1.9 to 3.2 times as many requests (computed from the best runs). On the proxied API, BareProxy went from 10,430 to 12,055 requests per second since the morning's measurement of the first alpha build, while nginx stayed at about 35,400.

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
- **Live test.** Re-run on 5 October on this build: 24 requests and 24 records, 23 over HTTP/2 with TLS 1.3 and 1 over HTTP/1.1 (`results/live-test.log`).
- **Gate 1 review.** A hands-on review inside the project (not an outside review) ran `plan`, apply and rollback on a real config (the bareproxy.com site from two release folders, an API pool with health checks, a redirect, a second site, an HTTPS site). Verdict: pass with conditions, no blockers. Removing a backend under load: 45,518 requests in 8 s, 0 failures. Its four conditions and its five minor items are fixed in this build: `status` shows a config file that doesn't hold the running config, apply and rollback say when they rewrite the file, a symlinked config stays a symlink, an apply with no changes still reaches the server, apply shows each warning once, `check` warns about rules that can never match, events say what an apply changed, draining backends stay in `status`, and the message about the admin socket says what happened.

## Not in this release

These settings are in the grammar, but they aren't built yet. The config check warns that BareProxy ignores them:

- `trust`
- `client-header-timeout`, `client-body-timeout`, `client-idle-timeout` and `shutdown-timeout`
- `set-response-header` and `remove-response-header`

`unix:` backends aren't built either, and a config that uses one is an error.

## Known limits

- **Automatic certificates are tested against Pebble only.** Let's Encrypt itself hasn't issued a certificate to BareProxy yet; that needs a public name and ports 80 or 443.
- **Requests Go's server can't parse leave no record.** It answers them itself (a bad request line or header, an oversized header, an unknown HTTP version) with 400, 431, 501 or 505. `OPTIONS *` does reach BareProxy: it gets 200 with no body, a `BareProxy-Id` and one record (outcome `local`, rule `OPTIONS *`).
- **WebSocket tunnels aren't inspected.** After an upgrade the connection is a plain tunnel, so routing rules don't see what travels inside it. When an apply removes a backend, the tunnels through it close at the end of the pool's `drain` time, in use or not. WebSocket was tested over HTTP/1.1, plain and with TLS, not over HTTP/2.
- **macOS and Windows are lightly tested.** The test suite runs on Linux and macOS on every push, and each release's binaries pass a smoke test on macOS and Windows, but only Linux has run under load or in front of a real site. Outside Linux, `history` doesn't record the user who made a change. Windows has no SIGHUP, so reload with `apply`, and its admin socket is a Unix socket file, which needs Windows 10 version 1803 or later.
- **No plugin is built yet.** This release has the plugin host, tested with a test plugin written in Go straight to the ABI. The plugins come next ([docs/plugin-program.md](docs/plugin-program.md)).
- **A plugin costs more than its budget.** With the test plugin (Go, built for `wasip1`), a request costs about 42 microseconds more with a plugin that does nothing and about 85 more with one that adds a header and a note, on a 2.1 GHz Xeon (`results/plugin-bench.log`); the request alone costs about 5. The design budget is 10. Most of the time is inside the plugin: Go's runtime starts each call. Plugins in Rust or C should cost much less; that is measured when the first one is built.

## License

The entire project runs under the Apache License 2.0 for now. Premium add-ons may be offered later under different licenses. Copyright 2026 BareProxy.com. See [LICENSE](LICENSE) and [NOTICE](NOTICE). Everything in this repository is under Apache 2.0 unless a file says otherwise.

Questions and security reports: info@bareproxy.com. See [SECURITY.md](SECURITY.md).
