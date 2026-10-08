# BareProxy Plugin Program

Status: set on 8 October 2026, with eleven candidates added and all 21 put in one build order, easiest first, the same day. The plugin host is built (0.2.0, Rust plugins in 0.3.0). Plugins 1 and 2 are built and ship in 0.4.0. Each plugin is a WebAssembly module loaded at run time through BareProxy's Proxy-Wasm host; [plugins.md](plugins.md) is the design.

## Build order

All 21 plugins in one order, easiest and quickest to ship first, so a new plugin comes out every few days at the start and the long jobs come last. The time is coding time for one person, with tests and a README, once the plugin host is trimmed. Each has a page on bareproxy.com that shows the same number.

| # | Plugin | What it does | Where it plugs in | Coding time |
|---|---|---|---|---|
| 1 | **Maintenance and failover pages** (built, 0.4.0) | A static page when the backend is down or the site is in maintenance, with an allow list and Retry-After. | Response headers | An hour or two: one callback, nothing else. The first plugin, so it also proves the path from `plugins/` to a `.wasm` file in a release. |
| 2 | **CORS** (built, 0.4.0) | Allowed origins per site or path, preflight answers at the proxy, `Vary: Origin`. | Before routing, response headers | Two or three hours: headers only. |
| 3 | **Header and rewrite rules** | Security headers and URL rewrites. The core already does redirects. | Before routing, response headers | About half a day: exact and prefix rules, no bodies. |
| 4 | **Redirects from a file** | Thousands of redirects from one plain file the plugin reads, looked up in a table, reloaded on change, counted. | Before routing | About half a day: one file read into a table, reloaded on a timer. |
| 5 | **Geo rules** | Allow, block or route by country from a local database file; the country passed on in a header ordinary route rules can match. | Before routing | About a day: a lookup, then a header. |
| 6 | **Request signing** | Signed requests to backends, so an app can tell a request came through BareProxy, and signed URLs that expire. | Before routing | About a day: signatures with a key from the config, no outside calls. |
| 7 | **A/B and canary splits** | A share of traffic sent to a new version, visitors kept on theirs by a cookie. The plugin sets a header and the core's routes pick the pool, so `plan` sees the routing. | Before routing, response headers | About a day. A guard that rolls back a bad canary (the earlier Guard module) would follow later. |
| 8 | **Rate limiting and basic WAF** | Limits per client address and per path; simple bad-request rules (methods, header sizes, known attack paths). | Before routing | About a day: counters in shared data and simple rules. Replaces the earlier Limit module. |
| 9 | **Link previews** | Open Graph tags added for preview fetchers to pages that lack them. | Response body | About a day: tags added to the head of HTML pages. |
| 10 | **AI crawler control** | Allow, block or rate-limit each bot (GPTBot, ClaudeBot, PerplexityBot and the rest), with a log of who is scraping what. | Before routing, the record | A day or two. Checks a bot's published address ranges, fetched and refreshed on a timer, not just its user agent, which anyone can fake. |
| 11 | **Bot challenges** | A proof-of-work check for clients that look like scrapers, with a signed cookie once passed and no outside service. | Before routing | A day or two: a challenge page, a check and a signed cookie. |
| 12 | **Markdown serving** | `.md` files rendered to HTML straight from the folder, with a template, no build step. | Response body | About two days. Reads files through `bareproxy_read_file`, limited to the folders the config names. |
| 13 | **Cookie consent** | A consent banner on HTML pages, non-essential cookies held back until consent, Global Privacy Control respected. | Response headers and body | About two days. |
| 14 | **Uptime checks and status pages** | Checks on a timer, 90 days of history in the store, a public status page served by the plugin. | Timer, before routing | Two or three days. |
| 15 | **OpenTelemetry export** | Request records sent as spans or logs over OTLP/HTTP, in batches, sampled. | The record, timer | Two or three days. Covers the earlier Export module. |
| 16 | **Analytics without JavaScript** | Visit stats from the request records: pages, referrers, countries, bots kept apart from people. No cookies, no script in the page. | The record | About three days: counting, daily hashing and a report page. |
| 17 | **Auth gate** | A password, OAuth or SSO login in front of any app that has none. | Before routing | Three or four days. A password login is quick; OAuth and SSO call the identity provider through allowed outgoing calls and take the time. Replaces the earlier Auth module. |
| 18 | **Response cache** | Micro-caching for dynamic sites: responses kept for seconds to minutes, purge by path prefix. | Before routing (hit), response (fill) | Three or four days: a cache has to be exactly right about what it may keep and share. Replaces the earlier Cache module. |
| 19 | **AI crawler payment gate** | 402 Payment Required with prices per bot and path; access once a crawler pays. Works with AI crawler control, which tells real crawlers from impostors. | Before routing | Four or five days, on payment schemes still settling, which is one more reason it comes late. |
| 20 | **Image optimizer** | On-the-fly resize and WebP/AVIF conversion, cached. | Response body | About a week: the plugin plus an image encoder running beside BareProxy. |
| 21 | **RenderCache** | Prerendered HTML snapshots of JavaScript-heavy pages, served to search bots, AI crawlers and link-preview fetchers. Everyone else gets the normal app. Snapshots refresh on a schedule or on a purge call. | Before routing (hit), response (fill) | One to two weeks: the plugin plus a headless Chromium renderer beside it, and snapshots that must match what users see. |

Adding the times up gives roughly eight to ten weeks of coding for all 21. The first nine take about a week and a half together.

## Before plugin 1

**The plugin host.** Done in 0.2.0: wazero, the Proxy-Wasm host, the hook points, the `bareproxy_` functions, config lines, plugins in `explain`, `plan`, `history`, `status` and the record, and a test plugin in Go with tests for each callback and failure. Plugins in Rust are set up in 0.3.0 (`plugins/`, with a Rust test plugin the host's tests run with); the plugins are written in Rust. Still to do here: trim the host to its 2,000-line budget. Each plugin's `.wasm` file is attached to every release since 0.4.0.

## Kept in the core

Two items on the first list stay in the core and have no place in the build order:

- **Auto TLS.** Let's Encrypt certificates, in the core since 0.1.0-alpha (`tls auto`). Every https site needs it.
- **Plan and explain.** `explain`, `why` and `plan` are the core's own. The plugin side is that every plugin reports into them through `bareproxy_note`, so plugins never become a blind spot.

## Bots and AI, together

AI crawler control (10), the payment gate (19) and RenderCache (21) tell one story: BareProxy decides what bots see, and on what terms. Crawler control decides whether a bot gets in and how often; the payment gate, at what price; RenderCache, what it gets, a fully rendered page instead of an empty JavaScript shell. They were first in the earlier order. They are spread out now because the order goes by how quickly each can ship, and crawler control, the one the other two lean on, still comes well before them.

## The earlier module plan

Before plugins, the plan was eight modules compiled into the binary. Limit, Auth and Cache carry on as plugins 8, 17 and 18, Export as plugins 15 and 16, and Split as plugin 7. Compress (run-time compression), Guard (automatic rollback when errors jump after an apply) and Fleet (one plan across many instances) aren't in this program. They may come back later, as plugins or, for Guard and Fleet, which work on config changes rather than requests, in the core.

## What every plugin ships with

- its own folder under `plugins/`, with a README that is its manual
- tests for each thing it does, and failure tests aimed at its main promise (for AI crawler control: a faked user agent from an address outside the bot's ranges is treated as unknown)
- its code in Rust, in a folder of `plugins/`, built by `plugins/build.sh`
- a `.wasm` file built by CI and attached to each release, with its SHA-256 in SHA256SUMS
- a license stated in its folder. The project is Apache 2.0 today; premium add-ons may come later under other licenses, and which plugins, if any, hasn't been decided
