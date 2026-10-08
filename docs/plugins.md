# BareProxy Plugins: Design Note

Status: accepted on 8 October 2026. Nothing here is built yet. This note fixes the shape of the plugin system before any code, the same way the core started from a short design note. [plugin-program.md](plugin-program.md) lists the plugins and the order they come in.

## The decision

Everything outside the core is a plugin, and a plugin is a WebAssembly module that BareProxy loads at run time.

- **Loaded at run time.** A plugin is a `.wasm` file named in the config. Adding one, upgrading one or taking one out is a config change like any other: `plan` shows it, `apply` makes it live, `rollback` takes it back. No rebuild, no restart.
- **Sandboxed.** A plugin can't open files, sockets or processes. It sees what the host hands it and asks the host for anything else, and the host only grants what the config allows.
- **Any language.** Anything that compiles to WebAssembly works: Rust, Go, C, C++, Zig, AssemblyScript.

This replaces the earlier plan of modules compiled into the binary. The trade is a little speed for flexibility and safety: a plugin call costs more than a Go function call, and a crashing or looping plugin can't take the server down.

## Two choices that follow from it

**The runtime is wazero.** [wazero](https://github.com/tetratelabs/wazero) is a WebAssembly runtime written in pure Go, with no cgo and no dependencies of its own, under Apache 2.0. BareProxy stays one static binary on Linux, macOS and Windows, and the release workflow doesn't change. On amd64 and arm64 it compiles each module to machine code when the plugin loads; elsewhere it interprets. Runtimes such as Wasmtime or WasmEdge would need C libraries and would end the single static binary. wazero becomes the second outside module, next to `golang.org/x/crypto`, and is vendored the same way.

**The plugin interface is Proxy-Wasm.** [Proxy-Wasm](https://github.com/proxy-wasm/spec) is the plugin ABI that Envoy, Istio and other proxies already use, with SDKs for Rust, C++, AssemblyScript and Go. BareProxy implements the host side of ABI 0.2.1, so a plugin author uses an existing SDK instead of learning an API that only BareProxy has, and some plugins written for other Proxy-Wasm hosts run with little or no change. BareProxy adds a few host functions of its own, all optional, under the `bareproxy_` prefix (below). A plugin that uses none of them is a plain Proxy-Wasm plugin.

## Where plugins plug in

Proxy-Wasm defines the callbacks; BareProxy defines when it makes them. A request goes through these points, in this order:

| Point | Proxy-Wasm callback | What a plugin can do there |
|---|---|---|
| Before routing | `proxy_on_request_headers` | Read and change request headers, answer the request itself (`proxy_send_local_response`), or let it go on. Crawler control, rate limits, auth and RenderCache's cache hit all work here. |
| Request body | `proxy_on_request_body` | Read or change the body, when the plugin asks for it. Bodies are passed only to plugins that declare they need them. |
| Response headers | `proxy_on_response_headers` | Read and change response headers. Security headers and cache rules work here. |
| Response body | `proxy_on_response_body` | Read or replace the body. Image conversion, Markdown rendering and RenderCache's fill work here. |
| The record | `proxy_on_log` | Read the finished request, once. Analytics works here. |
| Timer | `proxy_on_tick` | Periodic work: refreshing a list of crawler IP ranges, expiring cache entries. |

Plugins run in the order the site lists them, and a plugin that answers a request stops the ones after it, as in Envoy. Static files served by the core pass through the response points too, so a plugin treats a file and a proxied response the same way.

A plugin can't change how the core matches a request to a rule. It can change the request before routing (a header, the path), and then the core routes what it gets, by the same rules `explain` shows.

## Host functions

BareProxy provides the Proxy-Wasm host functions a filter needs:

- headers and trailers: get, add, replace, remove (`proxy_get_header_map_value` and the rest)
- bodies: `proxy_get_buffer_bytes`, `proxy_set_buffer_bytes`
- local responses: `proxy_send_local_response`
- properties: the client address, the matched site and rule, the pool, the request ID (`proxy_get_property`)
- outgoing HTTP calls: `proxy_dispatch_http_call`, only to hosts the plugin's `allow-http` lines name
- shared data between instances of one plugin: `proxy_get_shared_data`, `proxy_set_shared_data`
- shared queues, metrics, logging and timers

And its own, under `bareproxy_`:

- `bareproxy_note(text)`: a line for `explain` and the request's record. This is how a plugin shows up in BareProxy's answer to "what happened to this request, and why".
- `bareproxy_store_get`, `bareproxy_store_put`, `bareproxy_store_delete`: a key-value store on disk in the state folder, one per plugin, with a size limit from the config. RenderCache keeps its snapshots here and Response cache its responses.
- `bareproxy_read_file(path)`: read-only access to files under a folder the config names with `read`. Markdown serving uses it.

Nothing else. No WASI file system, no sockets, no clock beyond what Proxy-Wasm gives.

## Config

A plugin is declared once, then used by sites:

```
plugin crawlers /etc/bareproxy/plugins/ai-crawler-control.wasm
  config /etc/bareproxy/plugins/crawlers.json
  memory 32MB
  on-error open

plugin rendercache /etc/bareproxy/plugins/rendercache.wasm
  config /etc/bareproxy/plugins/rendercache.json
  store 2GB
  allow-http 127.0.0.1:9222
  on-error open

site example.com
  use crawlers
  use rendercache
  route /api/* -> api strip
  route /* -> app
```

- `config FILE` is passed to the plugin as its configuration (`proxy_on_configure`), as is. Its format is the plugin's own.
- `memory` caps the module's memory (default 16 MB). `store` caps its on-disk store (default off). `allow-http HOST:PORT` lets it call that address; with no line it can call nothing. `read DIR` lets it read files under DIR.
- `on-error open` lets the request go on without the plugin when the plugin fails or runs out of time; `on-error closed` answers 502. Default: closed for plugins that run before routing, open for the rest.
- Every call has a time limit (default 5 ms, `timeout`). A plugin over it is stopped and the request follows `on-error`.

`check` loads each module and refuses one that doesn't export the Proxy-Wasm entry points, or imports a host function BareProxy doesn't have. It names the line, as for every other config error.

## What the core promises, with plugins

The core's rules hold with plugins in place:

- **`explain` shows each plugin.** For a request, `explain` lists the plugins the site uses, what each did (passed, changed a header, answered with 403, served from cache) and the notes it wrote with `bareproxy_note`. `--offline` runs the plugins on the request too, with outgoing calls stubbed.
- **`plan` covers plugins.** Adding, removing or reordering a plugin, or changing its `.wasm` file or config file, is a change `plan` lists, per site. The history keeps each version's plugin files by SHA-256, and a copy of each in the state folder, so `rollback` brings back the exact module that ran, not whatever is at that path now.
- **One record per request.** A plugin's work lands in the request's one record: which plugins ran, how long each took, and their notes. A plugin never makes a second record.
- **A bad plugin never replaces a good one.** A module that fails to load or to configure stops the apply, and the running version stays.
- **The core never depends on a plugin.** Remove every plugin and the core builds, runs and passes all of its tests.

## Heavy work stays outside

Some jobs don't fit in a sandbox: running a headless browser, encoding AVIF at speed. For those, the plugin is the part inside BareProxy (deciding, caching, serving), and the heavy part is a separate worker the plugin calls over HTTP through `proxy_dispatch_http_call`. RenderCache is the first case: the plugin spots crawlers and serves snapshots, and a renderer worker running headless Chromium makes them. The worker ships as its own small Go program and a container image, in this repository.

## Speed

Measured, then published, the same way as the core against nginx. Design budgets, not measurements: under 10 microseconds per plugin per request for a plugin that only reads headers, and no more than 20% fewer requests per second with AI crawler control and RenderCache both on (cache miss path excluded). BareProxy's proxied request costs about 72 microseconds of CPU today, so a header-only plugin should stay small next to it.

Instances are pooled per plugin, so a request never waits for a module to load. Modules compile once, when an apply loads them.

## Size of the work

- **The plugin host:** wazero, the Proxy-Wasm host functions, the hook points in the request path, `bareproxy_` functions, config lines, and plugins in `explain`, `plan`, `history` and the record. Its own line budget: 2,000 lines of Go, outside the core's 5,000.
- **A test plugin set** in Rust and Go, built in CI, covering each callback and each failure: a trap, a loop past the time limit, memory past the cap, a call to a host it isn't allowed, a module that won't load.
- **Each plugin:** its own folder under `plugins/`, its own README and tests, built to `.wasm` by CI and published as release files beside the binaries.
