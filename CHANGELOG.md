# Changelog

## 0.4.0 (8 October 2026)

The first two plugins, numbers 1 and 2 in the [build order](docs/plugin-program.md).

- **Maintenance and failover pages** (`plugins/maintenance`, released as `maintenance.wasm`): when the backend answers 502, 503 or 504, or BareProxy does because no backend is up, visitors get a page of the site's own, with the status kept and `Retry-After`. With `maintenance on`, everyone gets the maintenance page (503) except addresses on an allow list; paths can be left alone with `skip`. See its README.
- **CORS** (`plugins/cors`, released as `cors.wasm`): preflights answered at the proxy, never reaching the app; `Access-Control-*` headers added to responses from one list of exact origins, with sections per path prefix; the app's own CORS headers dropped so the list is the only one; `Vary: Origin` whenever the answer depends on the origin. It refuses `*` with credentials, `null` and wildcard origins. A refused call is noted in the record with its origin. See its README.
- `plugins/kit`, what plugins share: config files in BareProxy's style, address ranges, path prefixes, notes for the record.
- `check` and `plan` start each plugin once, without its store, folders or outgoing calls, so a plugin config the plugin refuses is an error there, naming the plugin's config file and its reason (`plugin cors refused its config cors.conf: line 2: ...`). Before, it showed only when the config went live.
- Tests: unit tests in each plugin (`cargo test` in `plugins/`), and server tests that run the built plugins inside BareProxy against failing backends, no backend, maintenance mode, preflights and real calls (`BP_PLUGINS_DIST`). CI runs both.

## 0.3.0 (8 October 2026)

Plugins in Rust. No plugin is built yet; the first ones will be written this way.

- `plugins/`: a Cargo workspace for plugins written with the Proxy-Wasm Rust SDK (0.2.5), built to `wasm32-unknown-unknown` by `plugins/build.sh`, with the crates vendored so plugins build with no network. See "Writing a plugin in Rust" in the README.
- `plugins/testplugin`: the Rust test plugin. It does what the Go test plugin does, and the plugin host's tests run with both (in CI; locally when `BP_RUST_FIXTURE` names its `.wasm`). The real SDK works with the host unchanged.
- New plugin setting `body request response`: the bodies a plugin is handed. SDKs export every callback, so the module can't say which bodies a plugin reads; before, any plugin with the callback got whole bodies, which with an SDK would have meant every plugin held up every body. A plugin that pauses on its headers waits only when no body follows.
- Cheaper calls into a plugin: the time limit is now kept by one timer per instance instead of a context deadline per call. A do-nothing Rust plugin adds about 15 microseconds to a request, and the Go test plugin about 25 (was 42) (`results/plugin-bench.log`).
- CI builds the plugins, checks their formatting and lints, and runs the host's tests with the Rust test plugin; releases attach every plugin but the test plugin as `NAME.wasm`, with its checksum in SHA256SUMS.

## 0.2.0 (8 October 2026)

The plugin host. No plugin is built yet; the [plugin program](docs/plugin-program.md) lists them.

- WebAssembly plugins written to the Proxy-Wasm ABI (0.2.1 and 0.2.0), run on wazero, a WebAssembly runtime in pure Go, so BareProxy is still one static binary. New config lines: a `plugin NAME FILE` block (`config`, `memory`, `timeout`, `pause`, `instances`, `on-error`, `allow-http`, `read`, `store`, `body-limit`) and `use NAME ...` in a site. See Plugins in the README.
- Plugins run before routing and can change the request or answer it, see the response headers and, when they ask, the whole response body, and get `proxy_on_log` after the response. BareProxy's own functions (`bareproxy_note`, a store on disk, reading a folder) come through `proxy_call_foreign_function`.
- The sandbox: no files, environment or network unless the config allows them; a memory cap per module; a time limit per call; a broken instance replaced in the background, with growing waits after repeated failures; plugin output rate-limited in the log; limits on shared data, metrics and outgoing calls.
- The core's promises with plugins: `check` refuses a module that isn't a Proxy-Wasm plugin; `plan` lists changed plugin files and plugin configs; the history keeps the files each version ran, and `rollback` runs them; every request's one record says what each plugin did (`why` shows it); `explain` and `status` list plugins.
- An independent review of the first build found a restart storm when new instances failed to start, sleeps that held an instance past its time limit, the backend's body leaking after a plugin replaced the response, a failed plugin's partial changes being kept, kept plugins losing their folders two minutes after an apply, plugins sharing request properties, and an outgoing call that could be pointed at another address through its path. All are fixed and tested (`results/plugin-test.log`).
- `golang.org/x/sys` and `github.com/tetratelabs/wazero` v1.12.0 vendored; `live/vendor-deps.sh` fetches them.
- Measured: a do-nothing plugin written in Go adds about 42 microseconds a request, over the design's 10 (`results/plugin-bench.log`, and Known limits in the README). The binary is 11.6 MB (was 8.9 MB).

## 0.1.0 (8 October 2026)

The first release published on GitHub's releases page, and the first built by the project's own release workflow. The code is the 0.1.0-alpha of 5 October, evening, with no change in behavior.

- Binaries for macOS (Apple silicon and Intel) and Windows (x86-64), beside Linux x86-64 and ARM. On Linux they are tested in full; on macOS the test suite runs on every push; each release's macOS and Windows binaries pass a smoke test (see Known limits in the README).
- Releases go out on their own: a push to main that passes the tests and carries a version with no tag yet is built, smoke-tested on every system, tagged and published, with SHA256SUMS. Archive names carry no version (`bareproxy_linux_amd64.tar.gz` and so on), so `releases/latest/download/...` links always get the newest release.
- Continuous integration on every push and pull request: `go vet` and `go test` on Linux and macOS, the race detector on Linux, the vendored modules checked against Go's checksum database, and every release target and the browser demo built.
- The binaries are no longer kept in the repository; the `releases/` folder is gone, and the README's install section points at the releases page.

## 0.1.0-alpha, finished (5 October 2026, evening)

The binaries in `releases/v0.1.0-alpha` were rebuilt with these changes. The version name stays 0.1.0-alpha.

- The review's minor items: apply shows each warning once; `check` and a startup give the warnings `plan` gives (rules that never match, pools no rule uses); apply, rollback and reload events name the plan and count its changes; `status` lists removed backends as draining until their drain ends, and `events` notes each drain; the "no BareProxy is running" message says that a running BareProxy keeps its old admin socket until a restart.
- A backend still waiting for its first health check takes requests when no backend in its pool is up. Before, a route moved to a new pool, a changed health line or a replaced only backend answered 503 for the millisecond or two before the first check (1 to 2.5 failed requests per apply at 1,000 requests per second; now none, `results/load-experiments.log`). A backend that gets such a trial request is no longer listed as skipped in its record.
- The in-memory record store defaults to 8 MB (was 32 MB). Peak memory under the load test is 46 MiB (was 111 MiB); `results/memory-test.log` has runs with the store off, 8 MB, 16 MB and 32 MB.
- Faster proxying: the 32 KB copy buffers come from a pool, each attempt copies the request without cloning its headers, and the server matches routes without writing explain's notes. CPU per proxied request fell from about 80 to 72 microseconds in the speed runs, and memory allocated per proxied request from 43.7 KB to 10.3 KB.
- The load test at the design's full size: `live/load-test.sh` runs 2,000 requests per second for 60 seconds over HTTP/1.1, HTTP/2 and WebSocket with 20 applies, with its own load tool (`src/tools/loadtest`) and a WebSocket echo in `testapi` (`results/load-test.log`). `live/load-experiments.sh` runs five cases the applies stay clear of.
- Fixed a data race: the bytes-in count of a record was written by the transport's goroutine while the handler read it.
- The config parser lost 30 lines with no change in behavior, which keeps the core within its 5,000-line budget (4,990).
- New measurements against nginx on this build (`results/bench-summary.md`).

## 0.1.0-alpha, rebuilt (5 October 2026, afternoon)

The binaries in `releases/v0.1.0-alpha` were rebuilt with these changes. The version name stays 0.1.0-alpha.

- Automatic certificates from Let's Encrypt or any ACME CA (`tls auto`, or an https site with no `tls` line), with `acme-email` and `acme-ca`, kept in `<state>/certs` and renewed 30 days before they expire. TLS-ALPN-01 and HTTP-01 (through the port-80 redirect). Wildcard and catch-all https sites still need certificate files. `explain`, `status` and `events` show automatic certificates. Tested against Pebble with a real TLS-ALPN-01 check (`results/acme-test.log`).
- The first module from outside the standard library: `golang.org/x/crypto` v0.57.0 (with `golang.org/x/net` v0.58.0 and `golang.org/x/text` v0.42.0), vendored. See Dependencies in the README. go.mod now says Go 1.27.
- `OPTIONS *` is answered by BareProxy: 200 with no body, a `BareProxy-Id` and one record. Other methods with a `*` target still get 400.
- From the gate 1 review on a real config: `status` shows when the config file doesn't hold the running config; apply and rollback say when they rewrote the file; a symlinked config stays a symlink and its target is rewritten; an apply with no changes still goes to the server, so certificate files are reloaded; a refused reload logs its reason on one line.
- Trimmed the core and the command from 6,422 to 5,217 lines of Go. The first trim changed no output. The second changed wording: `plan` says "every path except ...", lists an added block's settings, shows settings as config lines and redirects as "redirect 301 to URL, keeping path and query"; the command uses one wording for each kind of error (no server, admin error, bad usage), parses options strictly, and `tail` shows the rule as written.
- The live test was re-run on this build (`results/live-test.log`).

## 0.1.0-alpha (5 October 2026)

Second cut, and the first with release binaries (Linux, amd64 and arm64).
Config changes are now planned before they go live and can be undone, and the
running server can say what it is doing.

New: `plan` says what a config change would do, in classes of requests, before
it goes live. `apply` makes a config live, and `apply --plan ID` refuses if
anything changed since that plan was made. `rollback` and `history` go back to
an earlier version and list the last 100, kept in a state folder. Listeners
change without a restart, removed backends drain, and a broken config file at
startup falls back to the last good version. These commands run over the admin
socket, and the history records the Unix user. `tail`, `status` and `events`
show records, state and changes as they happen. The latest records are kept in
memory (`why` looks there first), a trace log file can rotate, and a valid
`traceparent` header goes on to the backend. A browser demo runs the same
parser, explain and plan, compiled to WebAssembly.

Tested: acceptance tests (`explain` against the live server on 100,000
generated requests, 67 broken configs, 30,000 generated paths and 91 request
smuggling payloads), plan exactness on 1,000 generated config pairs, and 20
applies under load with no failed requests. Measurements against nginx 1.24.0
are in `results/bench-summary.md`.

Fixed after the acceptance tests: BareProxy now forwards only WebSocket
upgrades, and removes any other Upgrade header before the request reaches the
backend (an h2c upgrade could carry a request past a rule). A request body with
a bad chunk gets 400. `explain` keeps the query string on a folder redirect and
treats a raw backslash the way the server does. Static files go out with
sendfile where the platform allows. Response headers from a backend are limited
to 64 KB.

Known limits: automatic certificates are not built (certificate files only),
`OPTIONS *` is answered by Go's server and leaves no record, macOS and Windows
are untested, modules are not built, and the core is over its 5,000-line
budget.

## 0.1.0-dev (October 2026)

First public cut: a small web server and reverse proxy that explains every
routing decision. Static files, proxying to backend pools with health checks,
HTTPS with HTTP/2, a trace record per request, `explain` and `why`, and reload
on SIGHUP. License: Apache 2.0.
