# CORS

A BareProxy plugin. It answers CORS preflights at the proxy and adds the `Access-Control-*` headers to responses, from one allow list of origins, for the whole site and per path prefix. The app behind it doesn't need to know about CORS at all.

It's released as `cors.wasm` with each BareProxy release, from 0.4.0 on.

## Use it

```
plugin cors /etc/bareproxy/plugins/cors.wasm
  config /etc/bareproxy/plugins/cors.conf

site api.example.com
  use cors
  route /* -> api
```

## Config

Plain text, one setting a line, `#` starts a comment. Settings at the top cover the whole site. A `path` line starts a section for a path prefix; a section takes what it doesn't set from the top, and the longest prefix that matches a request wins.

```
# cors.conf
origins https://app.example.com https://admin.example.com
methods GET POST PUT DELETE
headers content-type authorization
expose x-request-id
credentials on
max-age 1h

path /public/
  origins *
  credentials off
```

| Setting | Default | What it does |
|---|---|---|
| `origins ORIGIN...`, `*` or `none` | none | Who may call. Exact origins, `scheme://host` or `scheme://host:port`. `*` is any origin; `none` closes a path |
| `methods METHOD...` or `*` | GET HEAD POST | The methods a preflight may ask for |
| `headers NAME...` or `*` | none | The request headers a preflight may ask for |
| `expose NAME...` | none | Response headers the page's script may read |
| `credentials on` or `off` | off | Whether calls may carry cookies and logins |
| `max-age TIME` | 10m | How long a browser keeps a preflight's answer |
| `path PREFIX` | | Starts a section for a path prefix |

The plugin refuses some configs, and `check` says which line and why:

- `*` together with `credentials on`. Browsers reject that pair, and allowing it by echoing back every origin would let any site call the API with a visitor's cookies.
- `null` as an origin. Sandboxed frames and local files all send it.
- Wildcards inside an origin, such as `https://*.example.com`. List the origins.
- No origins anywhere.

## What it does with a request

- **A preflight** (`OPTIONS` with `Origin` and `Access-Control-Request-Method`) is answered at the proxy and never reaches the app. An allowed one gets 204 with the allowed methods and headers and the max-age. One from an origin not on the list, or asking for a method or header not allowed, gets 403, and the record says which.
- **Any other request** goes on. On the response, the plugin drops any `Access-Control-*` headers the app set, so the list here is the only one, then adds its own when the origin is allowed. It adds `Origin` to `Vary` whenever the answer depends on the origin, so a cache never hands one origin's headers to another. Static files get the same headers.
- **A request with no `Origin`** isn't a CORS request: it goes on with only `Vary: Origin` added.

A call from an origin not on the list still gets its response; the browser is what keeps the page's script from reading it. The record notes it: "CORS: origin https://evil.example isn't on the list for /".

## Build and test

`plugins/build.sh` builds it with the other plugins. `cargo test -p cors` runs its unit tests; the server tests in `src/internal/bp/shipped_plugins_test.go` run it inside BareProxy with preflights, real calls, a backend that sets CORS headers of its own, and static files.

## License

Apache 2.0, like the rest of BareProxy.
