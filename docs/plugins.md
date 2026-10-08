# BareProxy Plugins: Design Note

Status: accepted on 8 October 2026, and the plugin host is built in 0.2.0. No plugin is built yet; [plugin-program.md](plugin-program.md) lists them and their order. This note now describes what was built. Where the build differs from the first version of the note, it says so.

## The decision

Everything outside the core is a plugin, and a plugin is a WebAssembly module that BareProxy loads at run time.

- **Loaded at run time.** A plugin is a `.wasm` file named in the config. Adding one, upgrading one or taking one out is a config change like any other: `plan` shows it, `apply` makes it live, `rollback` takes it back. No rebuild, no restart.
- **Sandboxed.** A plugin can't open files, sockets or processes. It sees what the host hands it and asks the host for anything else, and the host only grants what the config allows.
- **Any language.** Anything that compiles to WebAssembly works: Rust, Go, C, C++, Zig, AssemblyScript.

This replaces the earlier plan of modules compiled into the binary. The trade is speed for flexibility and safety: a plugin call costs far more than a Go function call (see Speed), and a crashing or looping plugin can't take the server down.

## Two choices that follow from it

**The runtime is wazero.** [wazero](https://github.com/tetratelabs/wazero) is a WebAssembly runtime written in pure Go, with no cgo, under Apache 2.0. BareProxy stays one static binary on Linux, macOS and Windows, and the release workflow didn't change. On amd64 and arm64 it compiles each module to machine code when the config is checked; elsewhere it interprets. Each module gets its own wazero runtime, because the memory cap is a runtime setting; compiled code is cached across applies. wazero v1.12.0 and `golang.org/x/sys` are vendored like `golang.org/x/crypto`.

**The plugin interface is Proxy-Wasm.** [Proxy-Wasm](https://github.com/proxy-wasm/spec) is the plugin ABI of Envoy, Istio and other proxies, with SDKs for Rust, C++, AssemblyScript and Go. BareProxy implements the host side of ABI 0.2.1, and the 0.2.0 names of the calls that changed. A plugin author uses an existing SDK.

## Where plugins plug in

| Point | Proxy-Wasm callback | What a plugin can do there |
|---|---|---|
| Before routing | `proxy_on_request_headers` | Read and change the request's headers, method, path and host (`:method`, `:path`, `:authority`), answer the request itself (`proxy_send_local_response`), pause it, or let it go on |
| Request body | `proxy_on_request_body` | Read or replace the whole body, up to the body limit, when the plugin exports this callback |
| Response headers | `proxy_on_response_headers` | Read and change the status and the headers, or replace the response |
| Response body | `proxy_on_response_body` | Read or replace the whole body, when the plugin exports this callback |
| After the response | `proxy_on_done`, `proxy_on_log`, `proxy_on_delete` | Read the finished request; write notes for the record |
| Timer | `proxy_on_tick` | Periodic work, once a plugin sets a period |
| Outgoing call | `proxy_on_http_call_response` | The response to a `proxy_http_call` |

A site's plugins run in the order its `use` lines name them, after the site is found by host and before the path is normalized and routed. A plugin that answers stops the ones after it. The response goes through the plugins last one first, as in Envoy. Static files go through the response points like proxied responses, so a plugin treats both the same way. A WebSocket upgrade (101) passes through untouched.

A plugin can't change which site a request belongs to, or how the core matches rules. It can change the request before routing, and the core routes what it gets, by the same rules `explain` shows. A changed path is noted in the record.

Whole bodies only: a plugin gets the body in one piece, up to its body limit (8 MB by default). A body over the limit, a partial (206) body, a compressed body, a stream of events and a response that flushes while it's sent go out as they are, with a note in the record. When a plugin changes a response body, Content-Length is set again and ETag dropped.

## Host functions

The Proxy-Wasm host functions a filter needs: headers (get, add, replace, remove, get and set all pairs), bodies (get, set, status), local responses, properties, outgoing HTTP calls, shared data with compare-and-swap, metrics, logging, the log level, the time, timers, the effective context, and continuing or closing a stream.

Properties: `request.path`, `request.url_path`, `request.method`, `request.host`, `request.scheme`, `request.id` (BareProxy's request ID), `request.protocol`, `request.query`, `source.address`, `source.port` (an 8-byte integer), `connection.requested_server_name`, `connection.tls_version`, `response.code` (an 8-byte integer, in the response phase), `plugin_name`, `bareproxy.site` and `bareproxy.config_version`. A plugin can set properties of its own on its request with `proxy_set_property`; they aren't shared with other plugins.

Not built: shared queues and gRPC calls (they answer UNIMPLEMENTED).

BareProxy's own functions come through `proxy_call_foreign_function`, which every SDK can call, rather than as extra imports (the first version of this note had them as imports):

- `bareproxy_note`: a line for the request's record and `why` (20 notes a request at most)
- `bareproxy_store_get`, `bareproxy_store_put` (4-byte little-endian key length, key, value), `bareproxy_store_delete`: a key-value store on disk, one per plugin, under its size cap (each key counts 4 KB besides its bytes)
- `bareproxy_read_file`: a regular file under a folder the config names with `read`

WASI is there because SDKs need it: clocks, random numbers, and stdout and stderr going to BareProxy's log. No files, no environment, no arguments, and no real sleep (a sleep returns at once, so a plugin can't hold its instance by sleeping). The first version of this note said "no clock beyond what Proxy-Wasm gives"; SDK runtimes need WASI's clock.

## Config

```
plugin crawlers /etc/bareproxy/plugins/crawlers.wasm
  config /etc/bareproxy/plugins/crawlers.json
  memory 32MB
  timeout 5ms
  on-error open

plugin rendercache /etc/bareproxy/plugins/rendercache.wasm
  config /etc/bareproxy/plugins/rendercache.json
  store 2GB
  allow-http 127.0.0.1:9222

site example.com
  use crawlers rendercache
  route /api/* -> api strip
  route /* -> app
```

The README's Plugins section lists every setting and its default. Two defaults differ from the first version of this note: memory is 64 MB (Go's runtime in a plugin wants more than 16), and `on-error` is `closed` in every phase, so a broken auth or crawler plugin fails safe.

`check` loads each module and refuses one that isn't a Proxy-Wasm plugin (no `proxy_abi_version_0_2_1` or `_0_2_0` export, no allocator, no `proxy_on_context_create`), is built for ABI 0.1.0, or imports a function BareProxy doesn't provide. It names the line, as for every other config error.

## Instances, limits and failures

Each plugin runs as a few instances of its module (`instances`, up to 4 by default), each running one call at a time. A request keeps to one instance from its first callback to its last.

- **Time.** Every call has a time limit (`timeout`, 5 ms by default; starting an instance gets 10 s). A call past it is stopped, and so is the instance.
- **Memory.** Each module's memory is capped (`memory`).
- **Pauses.** A plugin that pauses a request (to wait for an outgoing call) has `pause` (30 s by default) to let it go on; then the request follows `on-error`.
- **Failures.** A trap, an exit or a call past its limit stops the instance. The request follows `on-error` (502 with `closed`, without the plugin with `open`; with `open`, what the failed plugin changed is dropped), and a new instance starts in the background. A new instance that fails to start is tried again after 1 s, then 2, 4 and so on up to a minute, and after more than 10 failures in a minute the next start waits 10 s.
- **Host limits.** Shared data: 1 MB a value, 64 MB a plugin. Metrics: 1,000 a plugin. Outgoing calls: 64 at once, a minute at most, and only to `allow-http` addresses, whatever the call's path says. Log output: 50 lines a second, with a count of what was dropped.
- **Folders.** A running plugin holds its own handles to its `read` folders, so it keeps them when an apply keeps the plugin running.

An apply that leaves a plugin's file, config file and settings as they were keeps the running plugin, with its instances and shared data. Anything else starts a new one.

## What the core promises, with plugins

- **`why` shows each plugin.** A request's one record lists the plugins that ran, what each did (went on, answered 403, replaced the response, failed), how long it took, its error and its notes.
- **`explain` lists the plugins** a site runs, with their files, SHA-256 and `on-error`, and live, their working instances. It doesn't run them; the first version of this note promised an `--offline` run, which isn't built.
- **`plan` covers plugins.** A plugin block's lines are compared like a pool's. A changed `.wasm` file or plugin config file under the same text is listed with its SHA-256, and so is a changed plugin order on a site. The plan ID changes with them.
- **History and rollback.** The history keeps, for each version, the SHA-256 of each plugin file and plugin config file it ran, and a copy of each in `<state>/plugins/files`. A rollback runs those copies, not whatever is at the path now; `status` says so. A missing or damaged copy stops the rollback. Copies no kept version names are deleted.
- **A bad plugin never replaces a good one.** A module that fails to compile, start or configure stops the apply, and the running version stays.
- **The core never depends on a plugin.** With no `plugin` lines, BareProxy runs and tests as before.

## Heavy work stays outside

Some jobs don't fit a sandbox: running a headless browser, encoding AVIF at speed. For those the plugin is the part inside BareProxy (deciding, caching, serving), and the heavy part is a separate worker the plugin calls through `proxy_http_call`. RenderCache is the first case.

## Speed

Measured on a 2.1 GHz Xeon with the test plugin, which is written in Go and built for `wasip1` (`results/plugin-bench.log`): a request answered by a `respond` rule costs about 4.7 microseconds with no plugin, about 42 with a plugin that does nothing, and about 89 with one that adds a request header and writes a note. The design budget was under 10 microseconds for a header-only plugin, so this is well over it.

Most of the time is spent inside the plugin: each callback into Go's WebAssembly runtime costs a few microseconds, and a request makes seven of them. Plugins written in Rust, C or AssemblyScript should cost much less; that is measured when the first one is built. On the host side, each module's exported functions are made once per instance, since making one allocates its stack.

## Size of the work

- **The plugin host**, as built: 1,464 lines in `src/internal/plugin`, 740 in `src/internal/bp/plugins_config.go` and `plugins_run.go`, and 212 lines of hook-ups in the core's files, 2,416 in all (blank lines and comments not counted). The design set 2,000; trimming it is on the list.
- **The test plugin** (`src/internal/plugin/testdata/fixture`), written in Go straight to the ABI with no SDK, so the tests need nothing outside the repository. Its config picks what it does: change headers, answer, call out, use the store and a folder, tick, loop, crash, grow its memory, refuse its config, hold a request, sleep. Tests cover each callback and each failure (`results/plugin-test.log`).
- **Each plugin:** its own folder under `plugins/`, its own README and tests, built to `.wasm` by CI and published as release files beside the binaries.
