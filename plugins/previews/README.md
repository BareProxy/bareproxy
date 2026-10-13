# Link previews

A BareProxy plugin. Paste a link into Slack, WhatsApp, LinkedIn, X or iMessage, and a fetcher comes to read the page's Open Graph tags for the preview card: a title, a description and an image. A page without them shows up as a bare URL, or with whatever text the fetcher grabbed first. When a preview fetcher asks for an HTML page that lacks the tags, this plugin adds them to the page's `<head>`, from the page itself. Visitors' pages are never changed.

It's released as `previews.wasm` with each BareProxy release, from 0.11.0 on.

## Use it

```
plugin previews /etc/bareproxy/plugins/previews.wasm
  config /etc/bareproxy/plugins/previews.conf
  body response

site example.com
  use previews
  route /* -> files /var/www/example/public
```

`body response` lets the plugin read and change pages. It asks for the body only when a fetcher is asking; for everyone else the page streams past it as if the plugin weren't there, and files still go out with sendfile.

## Config

An empty config works. Settings:

```
# previews.conf
site-name Example
image /img/card.png             # when a page has no picture of its own

path /docs/
  title Example docs
  description Every setting, explained.
  image /img/docs-card.png

fetchers add mypreviewer
```

| Setting | What it does |
|---|---|
| `site-name NAME` | `og:site_name` on every page |
| `image PATH-OR-URL` | The image for pages with none of their own; in a `path` section, that section's card |
| `path PREFIX` | Starts a section with its own `title`, `description` and `image`; the longest prefix wins |
| `title TEXT`, `description TEXT` | In a section: a fixed card for every page under it |
| `fetchers add WORD...` | More User-Agent words to treat as preview fetchers; `fetchers only WORD...` replaces the list |

The fetchers it knows: Facebook and Instagram, X, Slack, WhatsApp, LinkedIn, Telegram, Discord, Skype and Teams, Reddit, Pinterest, VK, Embedly, Iframely, Mastodon, Bluesky, Google Chat, Apple (iMessage) and Bitly.

## What it adds

Only what the page lacks; a page with all its tags goes out untouched.

| Tag | From |
|---|---|
| `og:title` | The section's title, the page's `<title>`, or its first `<h1>` |
| `og:description` | The section's description, the page's meta description, or its first paragraph of 40 characters or more, cut at 200 |
| `og:image` | The page's first picture (not inline data, not an SVG, not given a width under 200), or the configured image; made a full URL |
| `og:url`, `og:type` | The page's address, and `website` |
| `og:site_name` | `site-name`, when set |
| `twitter:card` | `summary_large_image` with an image, `summary` without |

Only 200 responses that are `text/html` are touched. The tags describe the same page a visitor sees, so a preview never promises something the page doesn't have.

## What `why` shows

A changed page carries a note in its record: "link previews: added og:title, og:description, og:image, og:url, og:type, twitter:card for slackbot". A fetcher's page that had its tags already says so.

## Where it fits

RenderCache, a later plugin, gives a fetcher a whole rendered page when the page is built by JavaScript. This plugin makes sure a plain HTML page tells the fetcher what it is. A JavaScript app may want both; a static site needs only this one.

## How it works

It reads the page with a small scanner for a handful of tags (`<title>`, `<meta>`, `<h1>`, `<p>`, `<img>`), not a full HTML parser, and inserts its tags just before `</head>`. A page in another encoding than UTF-8 goes out as it was. For anyone who isn't a fetcher it sets the request property `bareproxy.response_body` to `skip`, new in 0.11.0, which tells BareProxy not to hand it the body.

## Build and test

`plugins/build.sh` builds it with the other plugins. `cargo test -p previews` runs its unit tests. The server tests in `src/internal/bp/shipped_plugins_test.go` run it inside BareProxy: a bare page filled in for Slack's fetcher with the right Content-Length, the same page unchanged for a visitor, a text file left alone, and a page over the body limit that only a fetcher's request tries to hand over, which shows visitors' pages stream past the plugin.

## License

Apache 2.0, like the rest of BareProxy.
