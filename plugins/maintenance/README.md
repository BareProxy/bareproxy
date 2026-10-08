# Maintenance and failover pages

A BareProxy plugin. When the backend answers 502, 503 or 504, or BareProxy does because no backend is up, visitors get a page of the site's own instead of a bare error, with the status kept so search engines know it's temporary. With maintenance on, every visitor gets the maintenance page, except addresses on an allow list.

It's released as `maintenance.wasm` with each BareProxy release, from 0.4.0 on.

## Use it

```
plugin down /etc/bareproxy/plugins/maintenance.wasm
  config /etc/bareproxy/plugins/down.conf

site example.com
  use down
  route /* -> app
```

With an empty config, or none, it sends a plain "Back soon" page for 502, 503 and 504, with `Retry-After: 300`.

## Config

The config file is plain text, in the style of BareProxy's own: one setting a line, `#` starts a comment.

```
# down.conf
maintenance off            # on: everyone not on the allow list gets the maintenance page
allow 203.0.113.0/24 2001:db8::/32 198.51.100.7
failover 502 503 504       # the statuses that bring the failover page, or off
retry-after 5m             # seconds or 30s, 10m, 2h, 1d; off leaves the header out
skip /api/ /healthz        # paths the plugin leaves alone

page
<!doctype html>
<title>Back soon</title>
<h1>Back soon</h1>
<p>We're fixing something. Try again in a few minutes.</p>
end

maintenance-page
<!doctype html>
<title>Maintenance</title>
<h1>Maintenance until 14:00</h1>
end
```

| Setting | Default | What it does |
|---|---|---|
| `maintenance on` or `off` | off | On: every request gets the maintenance page with 503, except from the allow list and skipped paths |
| `allow ADDR...` | none | Addresses and ranges that pass in maintenance mode, IPv4 or IPv6. The line can repeat |
| `failover STATUS...` or `off` | 502 503 504 | Response statuses replaced by the failover page; 5xx only |
| `retry-after TIME` or `off` | 5m | The Retry-After header on both pages |
| `skip PATH...` | none | Path prefixes left alone: `/api` covers `/api` and `/api/x`, not `/apix`. Useful for an API whose clients want its own error bodies, or a health check |
| `page` ... `end` | "Back soon" | The failover page, as HTML, up to a line saying `end` |
| `maintenance-page` ... `end` | the failover page | The maintenance page |

Both pages go out as `text/html; charset=utf-8` with `Cache-Control: no-store`, so no cache keeps them after the site is back. The failover page keeps the status the backend gave; the maintenance page is 503.

Turning maintenance on or off is a change of the config file, and BareProxy treats it like any other: `plan` shows it, `apply` makes it live, `rollback` brings back the file as it was. `check` refuses a config the plugin can't read, naming the line.

## What `why` shows

A replaced response shows in the record as `replaced the response with 502`, with the note "the response was 502: the failover page went out instead". A request answered in maintenance mode shows `answered 503` and "maintenance is on: the maintenance page went out". A request from the allow list notes that it passed.

## How it works

It needs two moments in a request's life. Before routing, it answers with the maintenance page when maintenance is on. On the response headers, it looks at the status and either lets the response go or replaces it. No bodies, no store, no outgoing calls.

## Build and test

`plugins/build.sh` builds it with the other plugins. `cargo test -p maintenance` runs its unit tests; the server tests in `src/internal/bp/shipped_plugins_test.go` run it inside BareProxy against a failing backend, no backend at all, and maintenance mode with and without an allow list.

## License

Apache 2.0, like the rest of BareProxy.
