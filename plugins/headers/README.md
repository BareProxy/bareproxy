# Header and rewrite rules

A BareProxy plugin. It adds the security headers scanners look for to every response, changes request and response headers by path, redirects by path or by a request header, and rewrites the path a request is routed on. Rules are exact paths and prefixes, never regular expressions, like BareProxy's own routing.

It's released as `headers.wasm` with each BareProxy release, from 0.5.0 on.

## Use it

```
plugin headers /etc/bareproxy/plugins/headers.wasm
  config /etc/bareproxy/plugins/headers.conf

site example.com
  use headers
  route /* -> files /var/www/example/public
```

With an empty config, or none, it adds the security headers and does nothing else.

## Config

Plain text, one setting a line, `#` starts a comment (also after a space inside a value). Header lines at the top cover the whole site; a `path` line starts a section for a path prefix, and every section whose prefix matches a request applies, after the top, in file order.

```
# headers.conf
security on

redirect /old-page /new-page
redirect /blog/* https://blog.example.com/* 308
redirect / /de/ when accept-language has de
rewrite /docs/* /manual/*

request set x-forwarded-site example.com
response set content-security-policy default-src 'self'
response remove x-powered-by

path /assets/
  response set cache-control public, max-age=31536000, immutable
```

| Setting | What it does |
|---|---|
| `security on` or `off` | On (the default): every response that lacks them gets `X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin`, `X-Frame-Options: SAMEORIGIN`, and on https `Strict-Transport-Security: max-age=31536000`. Headers the app sets itself are kept |
| `request set NAME VALUE` | Sets a request header before the app sees it. `add` adds one more value; `remove NAME` takes it out |
| `response set NAME VALUE` | The same for the response. A `response` line runs after the security headers, so it can change or remove one |
| `redirect FROM TO [CODE]` | Answers with a redirect. `CODE` is 301 (the default), 302, 307 or 308. The query goes along unless `TO` has one |
| `rewrite FROM TO` | Routes the request on another path of the same site. The core routes, and `explain` and `why` show, the new path |
| `... when HEADER has WORD` | On a redirect or rewrite: only when the request header holds the word, as one of its comma, semicolon or space separated parts, or that part's start before a dash (`de` matches `de-DE`). Such a redirect defaults to 302 and names the header in `Vary` |
| `path PREFIX` | Starts a section of header lines for a path prefix |

`FROM` is an exact path (`/old-page`) or a prefix ending in `*` (`/blog/*`). `TO` ends in `*` when `FROM` does, and the rest of the path goes there. A redirect's `TO` can be a full `https://` URL; a rewrite's stays on the site. Redirects and rewrites go at the top, and the first one in the file that matches a request is the one that runs: one per request.

Headers BareProxy owns are refused: `host`, `content-length`, `transfer-encoding`, `connection`, `upgrade`, `keep-alive`, `te` and `bareproxy-id`. `check` refuses a config the plugin can't read, naming the line.

## What `why` shows

Each request's record says what the plugin did: "redirected /old-page to /new-page (line 3)", "rewrote /docs/intro to /manual/intro (line 6)" followed by the core's own "the plugins changed the path to /manual/intro", "request headers: set x-forwarded-site", "response headers: added 3 security headers; removed x-powered-by". The rule the core matched after a rewrite is in the record as usual.

## Order with other plugins

List it first on a site (`use headers cors ...`) and its response headers go on everything, including a page another plugin answers with, such as the maintenance page, since a plugin's answer goes back through the plugins before it. Section paths match the path as it arrived, before a rewrite.

## Build and test

`plugins/build.sh` builds it with the other plugins. `cargo test -p headers` runs its unit tests; the server tests in `src/internal/bp/shipped_plugins_test.go` run it inside BareProxy with redirects, a redirect by language, a rewrite served from files, request headers reaching an app, and the app's own headers kept or removed.

## License

Apache 2.0, like the rest of BareProxy.
