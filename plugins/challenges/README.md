# Bot challenges

A BareProxy plugin that asks clients who say they're browsers to prove it before the site does any work. They get a small page with a puzzle that the browser's JavaScript solves. Scrapers that pose as browsers mostly don't run JavaScript, and the ones that do pay for each solve in CPU time. A browser solves it in a moment, once, and gets a pass: a signed cookie that lasts a week by default. Nothing is kept per visitor, and nothing leaves BareProxy.

It's released as `challenges.wasm` with each BareProxy release, from 0.13.0 on.

## Use it

```
plugin challenges /etc/bareproxy/plugins/challenges.wasm
  config /etc/bareproxy/plugins/challenges.conf
  store 1MB

site git.example.com
  use challenges
  route /* -> forge
```

`store` keeps the key the plugin makes for itself, so passes still work after a restart. Without a store, write a `key` line instead.

## Config

```
# challenges.conf
work 16                  # the puzzle's size, in bits
valid 7d                 # how long a pass lasts
after 10 per 1m          # let an address load 10 pages a minute first
always hosting.txt       # challenge these networks at once
allow 203.0.113.0/24     # never challenge these
skip /api/ /feed.xml     # or these paths
```

An empty config works too: every browser without a pass gets the puzzle.

| Setting | Default | What it does |
|---|---|---|
| `challenge browsers` or `all` | browsers | Who gets the puzzle: clients whose User-Agent says browser, or every client without a pass |
| `work BITS` | 16 | Leading zero bits the answer needs, 8 to 24; each bit doubles the work |
| `valid TIME` | 7d | How long a pass lasts, 1h to 30d |
| `bind prefix`, `address` or `none` | prefix | What a pass is tied to besides the User-Agent: the client's network (a /24 for IPv4, a /64 for IPv6), its exact address, or nothing |
| `after COUNT per TIME` | none | Page loads an address may make without a pass before it's challenged |
| `always ADDR-OR-FILE...` | none | Addresses, ranges or files of them that get the puzzle at once, even under `after` |
| `allow ADDR...` | none | Addresses never challenged |
| `skip PATH...` | none | Path prefixes never challenged |
| `trust-header NAME=VALUE` | none | Requests with this header go on: one a plugin before this one sets |
| `status CODE` | 403 | The status of the puzzle page: 200, 403, 429 or 503 |
| `cookie NAME` | bp_pass | The pass cookie's name |
| `key SECRET` | none | The key passes and puzzles are signed with, 32 characters or more, such as the output of `openssl rand -hex 32` |
| `endpoint PATH` | /.bareproxy-challenge | Where the page sends its answer |
| `forwarded-from ADDR...` | none | Proxies in front, such as a CDN, whose `X-Forwarded-For` is believed |

## Who gets the puzzle

Only GET and HEAD requests are challenged. A form post or an API call goes on as it is.

With `challenge browsers`, the default, a client gets the puzzle when its User-Agent starts with `Mozilla/`, as every browser's does, and doesn't say it's a bot: no `bot`, `crawl`, `spider` or `slurp`, and no `+http` contact link. Bots that say who they are go on, Googlebot among them. robots.txt, [AI crawler control](../crawlers/README.md) and rate limits are the tools for those. So are tools like curl, which don't pretend to be browsers.

With `challenge all`, every client without a pass gets it, bots and tools too. A feed reader or an API client can't solve it, so `skip` their paths.

These are never challenged: `/robots.txt`, `/favicon.ico` and `/.well-known/`, the `skip` paths, the `allow` addresses, requests with the `trust-header`, and anyone with a valid pass.

With `after`, an address may load COUNT pages in any TIME without a pass first. Only page loads count: a browser marks them with `Sec-Fetch-Mode: navigate`, or asks for `text/html`. Images, styles and scripts don't count, so one page with thirty pictures is one page load. Past the count, every request from that address gets the puzzle until it has a pass. Addresses are counted one by one (IPv6 by /64), and all of the plugin's instances share the count. A scraper that spreads one request a minute over thousands of addresses never reaches the count, which is what `always` is for.

`always` takes addresses and ranges, and files of them in a folder the plugin line names with `read`: hosting providers' ranges, say, where people rarely browse from and scrapers often run. A file has one address or range a line, `#` starting a comment, or the JSON that Google Cloud (`ipv4Prefix`) and AWS (`ip_prefix`) publish their ranges in. A file is read at the start, so a missing or broken one is an error in `check` and `plan`, and it's checked for changes every 30 seconds.

## The puzzle

The page carries a puzzle: 64 hexadecimal characters, an HMAC-SHA256 of when it was made, its work and the client it was made for. The browser looks for a number n so that SHA-256 of the puzzle, a colon and n starts with `work` zero bits. At 16 bits that takes 65,536 tries on average. The page's SHA-256 is plain JavaScript, since the browser's own crypto is missing on plain-HTTP pages. In tests, Chrome's engine made about a million tries a second on a desktop computer, so the default takes a fraction of a second there, and longer on an old phone.

The answer goes to `/.bareproxy-challenge`. The plugin makes the puzzle again from the time and the client, checks the answer with one SHA-256, and checks the time: an answer has 10 minutes. The puzzle was never stored. A right answer gets a pass and a redirect back to the page that was asked for, always a path on the same site, so the endpoint can't send anyone elsewhere. A wrong one gets a short page with a link to try again.

## The pass

The pass is the cookie `bp_pass`: when it ends, then an HMAC of that and the client. The plugin checks it without having stored it, and it can't be used by another client: it's tied to the User-Agent and, by default, to the network the address is in. A phone that moves between networks may see the puzzle again; `bind none` ties a pass to the User-Agent alone. The cookie is HttpOnly and SameSite=Lax, and Secure on https.

The key comes from the `key` line, or the plugin makes one from the host's random numbers the first time it starts, keeps it in its store and shares it with its instances. A new key ends every pass, and every visitor solves the puzzle again.

## With AI crawler control

List AI crawler control first and trust what it checked:

```
site example.com
  use crawlers challenges
  route /* -> app
```

```
# challenges.conf
trust-header X-Bot-Check=verified
```

A bot whose address crawler control verified then goes on without a puzzle, even with a browser-like User-Agent. Crawler control drops `X-Bot-Check` headers that clients send, so the header can't be faked. Without crawler control before it, the line would let in anyone who sends the header.

## What `why` and `status` show

- "challenged: a browser with no pass; work 16"
- "challenged: 11 page loads in the last 1m from 203.0.113.5/32 without a pass, over 10 (line 3); work 16"
- "challenged: 100.24.7.9 is on the always list; work 16"
- "challenge passed: work 16, pass until 2026-10-21 12:00 UTC"
- "challenge failed: the answer doesn't solve the puzzle"
- "no challenge: x-bot-check is verified (trust-header)"

A request with a valid pass has no note. `status` shows three counters: `challenged`, `passed` and `failed`.

## Limits worth knowing

- Visitors without JavaScript can't pass: text browsers, and some privacy setups. `after` and `allow` let some of them in.
- A scraper that drives a real browser solves the puzzle like anyone. The work makes each solve cost CPU time; raise `work` and lower `valid` to raise the price, at the cost of visitors' time.
- A monitoring check or an app with a browser's User-Agent gets the puzzle page. Allow its address or skip its paths.
- The page is in English, and it can't be changed yet.

## Build and test

`plugins/build.sh` builds it with the other plugins. `cargo test -p challenges` runs its unit tests: the config, who counts as a browser, puzzles and answers (right, wrong, too old, solved for another client), passes (forged, ended, from another network), redirects that stay on the site, and the page with a hostile path in it. The server tests in `src/internal/bp/shipped_plugins_test.go` run it inside BareProxy and solve its puzzles in Go: a pass and where it works, who gets the puzzle, `after` and `always`, a key shared by two instances and kept across a restart, and configs refused. `live/challenge-browser-test.sh` runs a page through it in headless Chromium, with the page's own script doing the solving (`results/challenge-browser-test.log`).

## License

Apache 2.0, like the rest of BareProxy.
