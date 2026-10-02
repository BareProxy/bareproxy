# BareProxy 0.1.0-dev, first cut

BareProxy is a small web server and reverse proxy that explains every routing decision. It terminates TLS, routes each request by host and path, and either serves it from a folder or proxies it to a pool of backends. For any request, `explain` says what would happen before it arrives, and `why` says what did happen after.

This is the first cut of version 0.1, built in about an hour on 2 October 2026, then rebuilt and retested with Go 1.27.1 the same day. It follows BareProxy's design, with static files in the core.

Open source under the Apache License 2.0. Copyright 2026 BareProxy.com.

## What works

- **Config.** The grammar from the design note plus two additions: the `files` action and `error 404`. Every error names its line. Settings that are in the grammar but not built yet give a warning.
- **Static files.** `/about/` serves `about/index.html`, and `/about` redirects to `/about/` when it's a folder. Folders are never listed. Hidden names such as `.git` and `.env` are never served, except `/.well-known/`.
  - Files can't be reached outside the folder, through `..` or through symlinks. Relative symlinks that stay inside work; absolute symlinks are refused.
  - HEAD, conditional requests, byte ranges, ETag and Last-Modified all work.
  - Precompressed `.br` and `.gz` copies are sent to clients that accept them. Compression doesn't happen at run time.
  - The site's 404 page is found through the site's own rules. It's used only for 404s BareProxy makes itself.
- **Proxying.** Requests go to the backend with the fewest in flight, with ties going round robin. There are active health checks, and pools without checks count failed connections instead. One retry happens only when a connection can't be opened. The prefix can be stripped. BareProxy sets `X-Forwarded-For`, `-Host` and `-Proto` and passes the request ID on.
- **HTTPS** from certificate files, TLS 1.2 or newer, with HTTP/2.
- **Tracing.** Every request gets a `BareProxy-Id` header and leaves exactly one JSON record in the trace log. `why` turns a record back into a story.
- **explain.** It asks the running server over its admin socket, so it shows live backend states. With no server running, it reads the config file.
- **Reload on SIGHUP.** A config with errors never replaces the running one. Requests in flight finish on the version they started with.

## Build and test

Only Go's standard library is used, so nothing is downloaded. Build with a current Go; this build used Go 1.27.1.

```
cd src
export GOTOOLCHAIN=local GOPROXY=off
go build -o ../bin/bareproxy ./cmd/bareproxy
go build -o ../bin/testapi ./tools/testapi
go test ./...
go test -race ./...
```

## Run

```
bareproxy check site.conf
bareproxy run site.conf
bareproxy explain --config site.conf GET https://example.com/about/
bareproxy why --config site.conf 7f3a9c0d
```

A complete config for a Hugo site with an API behind it:

```
global
  admin /run/bareproxy/admin.sock
  trace-log /var/log/bareproxy/requests.log

site example.com
  tls /etc/bareproxy/example.com.crt /etc/bareproxy/example.com.key
  error 404 /404.html
  route /api/* -> api strip
  route /* -> files /var/www/example/public

pool api
  backend 127.0.0.1:8080
  health /healthz
```

## Live test

`live/live-test.sh` builds the bareproxy.com Hugo site and adds bait files. It then starts BareProxy over HTTPS with two test API backends and one dead backend, and checks the following, printing everything it sees:

- static files, precompressed copies, 304s, byte ranges and HEAD
- refused paths and the API
- `explain` and `why`
- a backend failing, a broken reload and a good reload

```
HUGO=/path/to/hugo SITE=/path/to/bareproxy.com-main ./live/live-test.sh
```

## Measured on 2 October 2026 (Go 1.27.1, linux/amd64)

- **Code size.** The core package and the command are 2,926 lines of Go, against the 5,000-line budget. Tests are 494 lines. Blank lines and comments aren't counted.
- **Tests.** All 12 pass, and they also pass under the race detector (`results/go-test.log`, `results/go-test-race.log`).
- **Live test.** It made 24 requests and got 24 records: 23 over HTTP/2 with TLS 1.3, and 1 over HTTP/1.1 (`results/live-test.log`).
- **Binary size.** The binary is 11.8 MB as built and 8.1 MB stripped (`-ldflags="-s -w"`). The Go 1.24.7 build was 10.5 MB and 7.1 MB.

## Not in this cut

- `plan`
- apply with plan IDs, rollback and history
- admin commands beyond `explain`, and draining removed backends
- `tail`, `status` and `events`
- the in-memory record store, trace log rotation and `traceparent`
- automatic certificates
- `unix:` backends, response header settings and the `trust` list
- the planned acceptance tests, and measurements against nginx

Adding or removing a listener needs a restart in this cut.

**Go version.** The first build used Go 1.24.7, which is past its support window. Its `os.Root` follows a symlink out of the folder when a path ends in a slash (CVE-2026-39822). This build uses Go 1.27.1, and `TestOSRootTrailingSlash` shows the escape is fixed there: `Open("link/")` is refused. BareProxy never opens a path ending in a slash anyway, because it asks for `index.html` instead, so its lookups stay inside on either version.

## License

Apache License 2.0. Copyright 2026 BareProxy.com. See [LICENSE](LICENSE) and [NOTICE](NOTICE). Everything in this repository is under Apache 2.0 unless a file says otherwise.
