# Rate limiting and basic WAF

A BareProxy plugin for two of the oldest reasons to put a proxy in front of an app: stop one client from taking all of it, and turn away requests that are plainly up to no good before the app has to look at them.

- **Limits** count requests per client address, or for the whole site, over a sliding window, for every path or a few, and answer 429 Too Many Requests past them, with `Retry-After`. The counts are BareProxy's own, shared by all of the plugin's instances, so a limit holds however the load is spread.
- **WAF rules** turn away methods a site never uses, oversized URLs and headers, paths no real visitor asks for, and scanners that say who they are. Exact values, prefixes and words only, never regular expressions.

It's released as `limits.wasm` with each BareProxy release, from 0.10.0 on. It replaces the Limit module of BareProxy's earlier plan.

## Use it

```
plugin limits /etc/bareproxy/plugins/limits.wasm
  config /etc/bareproxy/plugins/limits.conf

site example.com
  use limits
  route /* -> app
```

## Config

```
# limits.conf
limit site 600 per 1m
limit login 5 per 1m path /login /account/password
limit api 10000 per 1h by site path /api/

methods GET HEAD POST
max-url 4KB
max-header 8KB
block-path /wp-login.php /xmlrpc.php
block-agent badbot
common-rules on

allow 203.0.113.0/24                 # the office, monitoring
forwarded-from 173.245.48.0/20       # a CDN in front
```

| Setting | What it does |
|---|---|
| `limit NAME COUNT per TIME [by address\|site] [path PREFIX...]` | At most COUNT requests in any TIME (1s to 1d), per client address (the default) or for the whole site, on these path prefixes or every path. A request counts against every limit it falls under |
| `methods METHOD...` | Only these methods; others get 405 with an `Allow` header |
| `max-url SIZE` | A path and query longer than this gets 414 |
| `max-header SIZE` | A header (name and value) longer than this gets 431 |
| `block-path PATH...` | These paths get 403: exact, or a prefix when it ends in `/`. Case doesn't matter |
| `block-agent WORD...` | A User-Agent that contains one of these words gets 403. Case doesn't matter |
| `common-rules on` | Adds a short built-in list: dotfiles and folders such as `/.env`, `/.git/` and `/.aws/`, `/phpinfo.php`, `/server-status`, SQL dumps and keys, and scanners such as sqlmap, nikto, nuclei, masscan and wpscan |
| `allow ADDR...` | Addresses no rule or limit applies to |
| `forwarded-from ADDR...` | Proxies in front, such as a CDN, whose `X-Forwarded-For` is believed; from anyone else it's ignored |

Sizes are bytes, or KB. Times are 30s, 1m, 1h, 1d. `common-rules` leaves WordPress paths alone, since some sites are WordPress; add them with `block-path` on a site that isn't.

## How the limits count

Each limit keeps a counter per key (the client address, or the site) in windows of its length, aligned to the clock. The count for a request is the current window's count plus the share of the last window that still falls within the last TIME: a sliding window, so a client can't send COUNT just before a window turns and COUNT again just after. Requests past the limit count too, so a client that keeps hammering stays limited.

The counters are BareProxy's (`bareproxy_count`), not the plugin's: every instance of the plugin adds to the same one. Old windows are swept away, so one counter per address doesn't pile up. If BareProxy ever holds 500,000 counters for the plugin, new keys aren't counted and the request goes on, with a note.

## What `why` and `status` show

Every request the plugin turns away is answered by it, and the record says why:

- "limit login (line 2): 6 requests in the last 60s from 203.0.113.9, over 5; the window resets in 34s"
- "WAF: path /.env is blocked (common-rules)"
- "WAF: method PUT isn't one the site uses"

`status` shows two counters, `limited` and `blocked`. A rule that turns away a real visitor shows up in the records the same day.

## Limits worth knowing

This covers the part of a WAF a small site needs. A full WAF with thousands of signatures is a different product. Behind a CDN, list its ranges with `forwarded-from`, or every visitor shares the CDN's address and one limit.

## Build and test

`plugins/build.sh` builds it with the other plugins. `cargo test -p limits` runs its unit tests. The server tests in `src/internal/bp/shipped_plugins_test.go` run it inside BareProxy with three instances, to show a limit holds across them, with per-address counts behind a trusted proxy, the allow list, and each WAF rule. `go test ./internal/plugin -run TestCounters` tests BareProxy's counters: windows, the sliding count's inputs and the sweep.

## License

Apache 2.0, like the rest of BareProxy.
