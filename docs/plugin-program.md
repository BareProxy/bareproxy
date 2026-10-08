# BareProxy Plugin Program

Status: set on 8 October 2026. No plugin is built yet. Each plugin is a WebAssembly module loaded at run time through BareProxy's Proxy-Wasm host; [plugins.md](plugins.md) is the design.

## The list, roughly by demand

| # | Plugin | What it does | Where it plugs in | Notes |
|---|---|---|---|---|
| 1 | **RenderCache** | Prerendered HTML snapshots of JavaScript-heavy pages, served to search bots, AI crawlers and link-preview fetchers. Everyone else gets the normal app. Snapshots refresh on a schedule or on a purge call. | Before routing (hit), response (fill) | The renderer, headless Chromium, is a separate worker the plugin calls. Snapshots must match what users see. |
| 2 | **AI crawler control** | Allow, block, rate-limit or charge each bot (GPTBot, ClaudeBot, PerplexityBot and the rest), with a log of who is scraping what. | Before routing, the record | Checks a bot's published IP ranges, not just its user agent, which anyone can fake. |
| 3 | **Auto TLS** | Let's Encrypt certificates out of the box. | | Already in the core since 0.1.0-alpha (`tls auto`). Stays in the core: every https site needs it, so it doesn't become a plugin. Listed so the program is complete. |
| 4 | **Rate limiting and basic WAF** | Limits per client address and per path; simple bad-request rules (methods, header sizes, known attack paths). | Before routing | Replaces the earlier Limit module. |
| 5 | **Auth gate** | A password, OAuth or SSO login in front of any app that has none. | Before routing | Replaces the earlier Auth module. OAuth and SSO call the identity provider through allowed outgoing calls. |
| 6 | **Response cache** | Micro-caching for dynamic sites: responses kept for seconds to minutes, purge by path prefix. | Before routing (hit), response (fill) | Replaces the earlier Cache module. Uses the plugin store, like RenderCache. |
| 7 | **Image optimizer** | On-the-fly resize and WebP/AVIF conversion, cached. | Response body | Fast AVIF encoding may need an outside worker, as RenderCache does; decided when it's built. |
| 8 | **Analytics without JavaScript** | Privacy-friendly visit stats from the request records: pages, referrers, countries, bots kept apart from people. No cookies, no script in the page. | The record | Covers part of the earlier Export module. |
| 9 | **Maintenance and failover pages** | A static page when the backend is down or the site is in maintenance. | Response headers | |
| 10 | **Header and rewrite rules** | Security headers, redirects, URL rewrites. | Before routing, response headers | The core already does redirects; this adds headers and rewrites. |
| 11 | **Markdown serving** | `.md` files rendered to HTML straight from the folder, with a template, no build step. | Response body | Reads files through `bareproxy_read_file`, limited to the folders the config names. |
| 12 | **Plan and explain** | Seeing what the proxy does with each request and why. | | Already the core's own (`explain`, `why`, `plan`). The plugin side is that every plugin reports into it, through `bareproxy_note`, so plugins never become a blind spot. |

## The first pair: RenderCache and AI crawler control

The two are built together and tell one story: BareProxy decides what bots see, and on what terms. AI crawler control decides whether a bot gets in, how often and at what price; RenderCache decides what it gets, a fully rendered page instead of an empty JavaScript shell. Most AI crawlers don't run JavaScript at all, so for a single-page app they see nothing without it.

AI crawler control comes first. It is small, needs only headers, the record and a timer, and proves the plugin host end to end. RenderCache follows and adds what heavier plugins need: the plugin store, outgoing calls and an outside worker.

## Order of work

1. **The plugin host.** wazero, the Proxy-Wasm host, the hook points, the `bareproxy_` functions, config lines, plugins in `explain`, `plan`, `history` and the record, and a test plugin set in Rust and Go. Nothing else starts before this passes its tests.
2. **AI crawler control**, then **RenderCache** with its renderer worker.
3. **Rate limiting and basic WAF**, **Auth gate**, **Response cache**: the usual reasons people put a proxy in front of an app.
4. **Header and rewrite rules**, **Maintenance and failover pages**, **Analytics without JavaScript**.
5. **Image optimizer**, **Markdown serving**.

Auto TLS and Plan and explain are in the core already, so they have no step of their own.

## The earlier module plan

Before plugins, the plan was eight modules compiled into the binary. Limit, Auth and Cache carry on as plugins 4 to 6, and part of Export as plugin 8. Compress (run-time compression), Split (weighted and canary splits between pools), Guard (automatic rollback when errors jump after an apply) and Fleet (one plan across many instances) aren't in this program. They may come back later, as plugins or, for Guard and Fleet, which work on config changes rather than requests, in the core.

## What every plugin ships with

- its own folder under `plugins/`, with a README that is its manual
- tests for each thing it does, and failure tests aimed at its main promise (for AI crawler control: a faked user agent from an address outside the bot's ranges is treated as unknown)
- a `.wasm` file built by CI and attached to each release, with its SHA-256 in SHA256SUMS
- a license stated in its folder. The project is Apache 2.0 today; premium add-ons may come later under other licenses, and which plugins, if any, hasn't been decided
