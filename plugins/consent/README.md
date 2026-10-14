# Cookie consent

A BareProxy plugin that asks visitors before a site sets cookies it doesn't need, and holds those cookies back until they say yes. A visitor who hasn't chosen gets a small banner at the foot of each HTML page, with Accept all, Reject all and Choose. Until a category of cookies is accepted, its cookies never reach the browser, the scripts that would set them stay out of the page, and the app doesn't see them either. A browser that sends Global Privacy Control counts as having said no. None of it needs JavaScript.

It's released as `consent.wasm` with each BareProxy release, from 0.15.0 on. This isn't legal advice, and the plugin isn't either: what a site needs consent for is for its owner to settle.

## Use it

```
plugin consent /etc/bareproxy/plugins/consent.wasm
  config /etc/bareproxy/plugins/consent.conf
  body response

site shop.example.com
  use consent
  route /* -> app
```

`body response` lets the plugin change pages: the banner, and the scripts it keeps out. The config file can be left out: the plugin then asks about analytics and marketing, with the cookies and scripts of the tools most sites use (see below).

Add a link to the settings page somewhere every page has one, such as the footer, so a visitor can change their mind:

```html
<a href="/.bareproxy-consent">Cookie settings</a>
```

## Config

```
# consent.conf
category analytics Analytics: to count visits and see which pages people read
cookies analytics _ga _ga_* _gid
block analytics googletagmanager.com google-analytics.com
category videos Videos: YouTube films in the pages
block videos youtube.com youtube-nocookie.com

banner We'd like to count visits and show videos from YouTube. Choose what you allow.
privacy /privacy/ How we use data
```

| Setting | Default | What it does |
|---|---|---|
| `category NAME LABEL[: WHAT FOR]` | analytics and marketing | A kind of cookie visitors are asked about: its name, what they see, and after a colon what it's for |
| `cookies NAME COOKIE...` | see below | Cookies of a category, by name; `_ga_*` takes every name that starts with `_ga_` |
| `block NAME TEXT...` | see below | Scripts and iframes kept out of pages until the category is accepted: those whose address holds the text, and inline scripts that do |
| `banner TEXT` | "This site would like to use cookies beyond the ones it needs to work. You can accept them, refuse them, or choose." | The banner's words |
| `accept`, `reject`, `choose` `TEXT` | Accept all, Reject all, Choose | The banner's buttons |
| `title`, `save`, `back` `TEXT` | Cookie settings, Save my choices, Back to the site | The settings page's heading, button and way back |
| `necessary LABEL: TEXT` | "Necessary: needed for the site to work, so they're always on." | The settings page's line for the cookies that need no consent |
| `gpc-note TEXT` | a line about Global Privacy Control | What the settings page tells a browser that sends it |
| `privacy URL [TEXT]` | none | A link to the privacy policy, in the banner and on the settings page |
| `lang TAG` | en | The banner's and the settings page's language |
| `remember TIME` | 180d | How long a choice lasts, 1d to 400d |
| `gpc on` or `off` | on | Global Privacy Control counts as no |
| `header NAME` or `off` | X-Consent | The request header that tells the app what was accepted |
| `cookie NAME` | bp_consent | The cookie that keeps the choice |
| `endpoint PATH` | /.bareproxy-consent | The settings page, and where the buttons send a choice |
| `skip PATH...` | none | Path prefixes whose pages get no banner and aren't changed; their cookies still wait |

Every word visitors read comes from the config, so the banner can be in any language. Cookies in no category are taken to be needed (a session, a basket, the language) and go out as before. Writing a `category` line turns off the built-in ones; `cookies` and `block` lines without one add to them.

## The built-in categories

| Category | Cookies | Scripts kept out |
|---|---|---|
| analytics | `_ga`, `_ga_*`, `_gid`, `_gat`, `_gat_*` (Google Analytics), `_hjid`, `_hjSession*`, `_hjFirstSeen` (Hotjar), `_clck`, `_clsk` (Microsoft Clarity), `_pk_id.*`, `_pk_ses.*` (Matomo), `ajs_anonymous_id`, `ajs_user_id` (Segment), `mp_*` (Mixpanel) | googletagmanager.com, google-analytics.com, static.hotjar.com, clarity.ms, cdn.segment.com, cdn.mxpnl.com, matomo.js, piwik.js |
| marketing | `_fbp`, `_fbc` (Meta), `_gcl_au`, `_gcl_aw`, `_gcl_dc` (Google Ads), `_uetsid`, `_uetvid` (Microsoft Ads), `_ttp`, `_tt_enable_cookie` (TikTok), `li_fat_id` (LinkedIn), `_pin_unauth` (Pinterest), `_rdt_uuid` (Reddit) | connect.facebook.net, googleadservices.com, googlesyndication.com, doubleclick.net, analytics.tiktok.com, snap.licdn.com, bat.bing.com, s.pinimg.com, redditstatic.com/ads |

Most of these tools set their cookies from JavaScript in the browser, where a proxy never sees them. Keeping their scripts out of the page is what stops those cookies; holding back `Set-Cookie` headers stops the ones the app sets.

## What a visitor sees

On a page, before choosing: the banner, fixed at the foot of the window, with the three buttons. Accept all and Reject all look the same and sit side by side. Choose opens the settings page: each category with a box to tick, and buttons to save, accept all or reject all. A choice brings the visitor back to the page they were on, and the banner is gone.

The buttons are links and the settings page is a form, so a choice works with JavaScript off. The banner's styles come from the endpoint as a file (`/.bareproxy-consent/consent.css`, kept by browsers for a year), so a Content-Security-Policy that forbids inline styles still allows them, and a site's own CSS can change them through `#bp-consent`. The banner stays light in dark mode, as most sites do; the settings page follows the visitor's.

The banner goes into HTML pages that a browser loads as pages: GET requests with `Sec-Fetch-Dest: document` (or, from a browser too old to send it, that ask for `text/html`), from a client whose User-Agent says browser and not bot, answered 200 as `text/html` in UTF-8. Images, styles, API calls, pages in iframes and search engines' bots get none. It goes before the page's last `</body>`.

## What waits for consent

Until a category is accepted, which is also the case before any choice and with Global Privacy Control:

- **Cookies to the browser.** A `Set-Cookie` header for one of its cookies is taken out of every response, of any kind.
- **Scripts and iframes in pages.** A `<script>` whose `src` holds one of its `block` texts, or an inline script whose text does, gets `type="text/plain"`, so the browser doesn't run it. An `<iframe>`'s `src` becomes `data-consent-src`, so it doesn't load. Each gets `data-consent="CATEGORY"`. Once the category is accepted, pages go out as the app made them.
- **Cookies to the app.** The category's cookies are taken out of the `Cookie` header, so the app doesn't read one the browser kept from before.

The app learns the choice from `X-Consent`: the categories accepted (`analytics,marketing`), `none`, or `unknown` before a choice. The plugin sets it on every request and drops one a client sends.

Taking a category back on the settings page deletes its cookies in the browser: the response sets each one the browser sent to expire, for the host and for each domain above it, as tracking cookies are often set for the whole site.

## The choice

The choice is the cookie `bp_consent`, such as `bp_consent=3fa1c2.analytics` or `bp_consent=3fa1c2.none`, for 180 days. The first part changes when the categories do, so visitors are asked again after a category is added. The cookie isn't HttpOnly, so the site's own scripts can read it. It's SameSite=Lax, and Secure on https.

The buttons send a choice to the endpoint: `?accept=all`, `?reject=all`, or `?save=1&set=analytics&set=videos`, with `r` for the page to go back to (only a path on the same site). An accept counts only from the site's own pages, as `Sec-Fetch-Site` (or, from an older browser, the `Referer`) says, so another site can't accept on a visitor's behalf with a link; it gets the settings page instead. A refusal counts from anywhere.

With `gpc on`, a browser that sends `Sec-GPC: 1` and hasn't chosen counts as having refused everything, and gets no banner. A choice made on the settings page counts over the signal.

## Pages and caches

A page that may change (the banner, or scripts to keep out) is asked of the app whole: without `Accept-Encoding`, so it comes uncompressed, and without `If-None-Match` and `If-Modified-Since`, as the copy a browser kept may be from before the visitor chose. A changed page goes out with `Cache-Control: private, no-cache` and no `ETag` or `Last-Modified`. Every HTML page gets `Vary: Cookie`, so a cache in between keeps a copy for each choice. The pages of a visitor who accepted everything aren't held at all: they stream past the plugin, compressed as the app sent them, and files still go out with sendfile.

Pages are changed in a timer call, a millisecond after the page arrives, as a request's own call may run for 5 milliseconds and a timer call for a second. Going through a page takes about 45 ms a megabyte on a 2.1 GHz server, so a 100 KB page costs about 5 ms.

## What `why` and `status` show

- "showed the banner: no choice yet; kept 2 scripts out until consent: googletagmanager.com (analytics), connect.facebook.net (marketing)"
- "held back _ga (analytics) and _fbp (marketing) until consent"
- "kept _fbp from the app: no consent"
- "the browser sends Global Privacy Control, so nothing is accepted"
- "consent: accepted analytics and marketing"
- "consent: accepted analytics; withdrew marketing"
- "consent: declined everything"
- "an accept from another site isn't counted, so the settings page asks instead"

`status` counts `banners` shown, choices that `accepted` something, choices that `declined` everything, choices that took a category back (`withdrawn`), cookies `held` back and scripts and iframes `blocked`. Those counts are the site's own record of consent given and withdrawn; nothing is sent anywhere.

## Limits worth knowing

- A proxy sees the cookies that cross it. One a script sets in the browser, or one a third party sets from its own domain, never does; the `block` rules keep those scripts out instead, by the text in their address. A site with many third-party tags still wants a review by someone who knows them.
- An inline script is kept out when its text holds a `block` text. A small snippet that only sets things up, such as the lines that define `gtag()`, loads nothing itself and is left alone.
- A page another plugin answers itself, such as a Markdown page, gets no banner, as those answers skip the body phase; its cookies still wait.
- A page over the plugin line's `body-limit` (8 MB by default), compressed by the app anyway, or not UTF-8, goes out unchanged, with a note.
- A changed page goes out once it's whole, so a page the app streams in parts arrives all at once.
- Visitors who choose are asked again after `remember`, and after a change of categories.

## Build and test

`plugins/build.sh` builds it with the other plugins. `cargo test -p consent` runs its unit tests: the config and the built-in categories, choices from the cookie and Global Privacy Control, cookies held back, taken out and deleted, what the endpoint does with each query and where it comes from, the page scan (scripts, iframes, inline scripts, comments, odd markup) and where the banner goes, and the banner and the settings page. The server tests in `src/internal/bp/shipped_plugins_test.go` run it inside BareProxy in front of an app that sets cookies and compresses its pages: the banner and held cookies, accepting, taking a category back and refusing, accepts from other sites, Global Privacy Control, the settings page and its styles, skipped paths, and configs refused. `live/consent-browser-test.sh` runs it in headless Chromium: the banner, a tracking script that runs only after Accept all, taking it back on the settings page, Reject all with JavaScript off, and Global Privacy Control (`results/consent-browser-test.log`).

## License

Apache 2.0, like the rest of BareProxy.
