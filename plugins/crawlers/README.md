# AI crawler control

A BareProxy plugin that decides, bot by bot, which AI crawlers get into a site and how often. It reads the bot's name from the User-Agent. Then it checks the request's address against the list of addresses the bot's operator publishes, because anyone can send any User-Agent. A request that says it's GPTBot but comes from somewhere else isn't GPTBot. It's a fake, and by default it's turned away. Every bot request is noted in its record and counted, and a report page shows what each bot read.

It's released as `crawlers.wasm` with each BareProxy release, from 0.12.0 on.

## Use it

```
plugin crawlers /etc/bareproxy/plugins/crawlers.wasm
  config /etc/bareproxy/plugins/crawlers.conf
  allow-http openai.com:443 claude.com:443 www.perplexity.ai:443 developers.google.com:443 index.commoncrawl.org:443 mistral.ai:443 developer.amazon.com:443
  store 1MB
  on-error open

site example.com
  use crawlers
  route /* -> files /var/www/example/public
```

`allow-http` names the hosts the operators publish their lists on. The plugin may call nothing else. `store` keeps the lists across restarts, so claims are checked from the first request after one. With `on-error open`, the site keeps serving people if the plugin ever fails; only the bot rules pause.

## Config

```
# crawlers.conf
block training                  # every bot that crawls to train AI models
allow GPTBot                    # this one may all the same
limit PerplexityBot 30 per 1m
fake block                      # the default

report /.crawlers
report-allow 127.0.0.1 ::1 203.0.113.0/24

path /members/
  block all
```

| Setting | Default | What it does |
|---|---|---|
| `allow NAME...` | | Lets these bots in |
| `block NAME...` | | Turns them away, with `status` |
| `limit NAME... COUNT per TIME` | | At most COUNT requests in any TIME (1s to 1d), for each bot on its own, per site; past it, 429 with `Retry-After` |
| `fake block` or `allow` | block | A request that names a bot from an address outside the bot's list: turned away, or let through like any visitor |
| `unverified bot` or `fake` | bot | A claim that can't be checked, because the bot has no list or its list hasn't loaded: taken at its word, or treated as a fake |
| `status CODE` | 403 | The status for bots turned away: 403, 404, 410 or 451 |
| `header NAME` or `off` | X-Bot | The request header the bot's name goes in, with `verified` or `unverified` in NAME-Check |
| `refresh TIME` | 6h | How often each published list is fetched again; 10m at least |
| `fetch on` or `off` | on | Whether the plugin fetches the operators' lists at all |
| `ranges NAME WHERE` | | Where a bot's addresses come from: a URL, a file in a `read` folder, addresses written out, or `off` |
| `bot NAME GROUP` | | A bot of your own: the word its User-Agent has, and a group |
| `forwarded-from ADDR...` | none | Proxies in front, such as a CDN, whose `X-Forwarded-For` is believed |
| `report PATH` | none | A plain-text report at this path |
| `report-allow ADDR...` | 127.0.0.1 ::1 | Who may see the report; to anyone else the path is an ordinary one |
| `path PREFIX` | | Starts a section with rules of its own |

NAME in `allow`, `block` and `limit` is a bot's name in any case, a group (`training`, `search`, `assistant`, or one a `bot` line adds), or `all`. A name can have one rule at the top and one in each section. Times are 30s, 1m, 1h, 1d.

## The bots

| Bot | Operator | Group | Checked against |
|---|---|---|---|
| GPTBot | OpenAI | training | https://openai.com/gptbot.json |
| OAI-SearchBot | OpenAI | search | https://openai.com/searchbot.json |
| ChatGPT-User | OpenAI | assistant | https://openai.com/chatgpt-user.json |
| ClaudeBot | Anthropic | training | https://claude.com/crawling/bots.json |
| Claude-SearchBot | Anthropic | search | the same list |
| Claude-User | Anthropic | assistant | the same list |
| PerplexityBot | Perplexity | search | https://www.perplexity.ai/perplexitybot.json |
| Perplexity-User | Perplexity | assistant | https://www.perplexity.ai/perplexity-user.json |
| Google-Agent | Google | assistant | https://developers.google.com/static/crawling/ipranges/user-triggered-agents.json |
| Google-GeminiNotebook | Google | assistant | no list |
| CCBot | Common Crawl | training | https://index.commoncrawl.org/ccbot.json |
| MistralAI-Training | Mistral AI | training | no list |
| MistralAI-Index | Mistral AI | search | https://mistral.ai/mistralai-index-ips.json |
| MistralAI-User | Mistral AI | assistant | https://mistral.ai/mistralai-user-ips.json |
| Amazonbot | Amazon | training | https://developer.amazon.com/amazonbot/ip-addresses/ |
| Amzn-SearchBot | Amazon | search | https://developer.amazon.com/amazonbot/searchbot-ip-addresses/ |
| Amzn-User | Amazon | assistant | https://developer.amazon.com/amazonbot/live-ip-addresses/ |
| Meta-ExternalAgent | Meta | training | no list |
| Meta-WebIndexer | Meta | search | no list |
| Meta-ExternalFetcher | Meta | assistant | no list |
| Bytespider | ByteDance | training | no list |
| DuckAssistBot | DuckDuckGo | assistant | no list |
| YouBot | You.com | search | 68.67.112.0/24, the range You.com states |
| AI2Bot | Ai2 | training | no list |

The groups go by what each bot is for, from its operator's own page where there is one:

- **training** bots collect pages to train AI models, or to build data sets for them.
- **search** bots index pages for an AI search or answer engine, which links back to them.
- **assistant** bots fetch a page when a person asks an AI assistant or agent to.

A bot's name is also the word the plugin looks for in the User-Agent, in any case, standing on its own: `GPTBot/1.4` matches and `GPTBotX` doesn't. The table was checked against each operator's own page in October 2026.

Google-Extended and Applebot-Extended aren't in it. They're words for robots.txt with no User-Agent of their own, and the crawling they govern is done by Googlebot and Applebot. Keep using robots.txt for those.

## Which rule applies

For a bot's request, the plugin looks at the longest `path` section the path falls under, then at the top of the config. In each, a rule that names the bot comes first, then a rule for its group, then `all`. A section takes from the top what it doesn't set. A bot no rule names is let in, and counted.

`/robots.txt` is always let through, so a bot that's turned away can still read the rules that say so.

## How a claim is checked

OpenAI, Anthropic, Perplexity, Google, Common Crawl, Mistral AI and Amazon publish the addresses their bots crawl from. The plugin fetches each list a second after it starts, through `proxy_http_call`, and again every `refresh`. One instance fetches and the others take its copy. With a `store`, each list is saved; after a restart the saved copy is used at once, and the list is fetched again when its refresh time comes.

The lists are JSON with `ipv4Prefix` and `ipv6Prefix` entries. Amazon shows the same JSON inside a web page, and the plugin reads both. A list is looked up in sorted order, so Amazon's thousand-odd single addresses cost a binary search.

A list that can't be fetched, or comes back with no addresses in it, is refused. The copy before it stays, BareProxy's log says why once, and the report shows the last try. A failed fetch is tried again after 10 minutes, then 20, doubling up to the refresh time. A list on a host the `allow-http` line doesn't name waits for the refresh time, since trying sooner can't help. A redirect is followed on the list's own host.

Until a bot's list has loaded once, its claims can't be checked. Neither can the claims of bots whose operators publish nothing to fetch: Meta names its network but gives no list, and ByteDance says nothing at all. Those requests are unverified. With `unverified bot`, the default, they follow the bot's rules; with `unverified fake`, only bots whose address checked out get their rules.

## Lists of your own

`ranges` gives a bot a list of your own, or turns its check off:

```
ranges Bytespider bytespider.txt                      # a file in a read folder
ranges Meta-ExternalAgent 157.240.0.0/16 69.63.176.0/20
ranges CCBot off
```

A file has one address or range a line, and `#` starts a comment; the JSON form works too. It's read at the start, so a missing or broken file is an error in `check` and `plan`, and it's checked for changes every 30 seconds. A file that breaks later is refused, with a line in BareProxy's log, and the list as it was stays.

A `bot` line adds a bot the plugin doesn't know. Scrapers often say they're Googlebot to get past limits, and two lines turn the fakes away while the real Googlebot gets in, as no rule names it:

```
bot Googlebot search-engine
ranges Googlebot https://developers.google.com/static/crawling/ipranges/common-crawlers.json
```

## The headers

A bot that's let in carries `X-Bot` with its name and `X-Bot-Check` with `verified` or `unverified`, for the app and for BareProxy's route rules. A fake let through with `fake allow` gets `X-Bot-Check: fake` and no `X-Bot`. The plugin drops both headers when a client sends them, so they can't be faked.

```
site example.com
  use crawlers
  route /* header X-Bot-Check=verified -> bots
  route /* -> app
```

## What `why` and `status` show

Every bot request's record has a note:

- "GPTBot (OpenAI, training): 20.171.206.5 is in OpenAI's list; let in by line 2, its 41st request let in this hour"
- "fake GPTBot: 203.0.113.9 isn't in OpenAI's list (18 ranges); turned away (fake block)"
- "Bytespider (ByteDance, training): 111.225.149.3 not verified, ByteDance publishes no address list; turned away by line 1"
- "PerplexityBot (Perplexity, search): 18.97.1.229 is in Perplexity's list; 31 requests in the last 1m, over 30 (line 3); the window resets in 23s"

`status` shows four counters: `bots`, `fakes`, `blocked` and `limited`.

## The report

`report /.crawlers` adds a plain-text page for the site it's asked on. It has each bot's requests today and yesterday (UTC days), as let in, limited, blocked and fakes. It has what each bot read since the plugin started, by first path segment, such as `/blog/` or `/docs/`. And it has each list's state: how many ranges, when it was fetched, and why the last try failed if it did.

```
BareProxy AI crawler report for example.com, 2026-10-14 12:05 UTC.

Requests today (since 00:00 UTC), then yesterday:
bot	group	let in	limited	blocked	fakes		let in	limited	blocked	fakes
GPTBot	training	1204	0	0	14		980	0	0	3
ClaudeBot	training	0	0	55	0		0	0	61	0

What they read since the plugin started, by first path segment (brought together every 5s):
GPTBot	1100 /blog/, 90 /docs/, 14 /
```

Each instance keeps its own counts and shares them with the others every 5 seconds, so the report can be that far behind. The counts start again when BareProxy restarts or the plugin's config changes; the trace log keeps every bot request for good, with its note.

## Behind a CDN

The address BareProxy sees is the connection's. Behind a CDN that's the CDN's address, so every real GPTBot would look like a fake. List the CDN's ranges with `forwarded-from` before turning this plugin on. From those addresses, the plugin takes the client's address from `X-Forwarded-For`, the last entry that isn't one of them. From anyone else `X-Forwarded-For` is ignored, so it can't be used to pass as a bot. A report full of fakes with nothing verified is the sign that the line is missing.

## Limits worth knowing

- A list can lag a few hours behind a new range. Until it catches up, real requests from the new range count as fakes.
- Some operators now also sign their requests, with HTTP Message Signatures (Web Bot Auth). The plugin checks addresses and doesn't check signatures yet.
- Turning a bot away at the proxy doesn't tell it to stop crawling. robots.txt does, for the bots that honour it, so use both.
- New AI bots show up all the time. Add one with a `bot` line, and tell us at the repository so it can join the table.

## Build and test

`plugins/build.sh` builds it with the other plugins. `cargo test -p crawlers` runs its unit tests: bots found in real User-Agent strings, addresses checked against a list (a faked User-Agent from outside the bot's ranges is a fake), rules by bot, group and path, the lists in the forms the operators publish, the sorted lookup, and the report. The server tests in `src/internal/bp/shipped_plugins_test.go` run it inside BareProxy with two instances and a store. They cover lists fetched once between the instances and kept across a restart, verified bots and fakes, groups, a path section, a rate limit counted across instances, the headers and a route chosen by them, the report, lists that fail or move, `unverified fake`, and configs refused.

## License

Apache 2.0, like the rest of BareProxy.
