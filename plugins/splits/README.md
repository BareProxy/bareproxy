# A/B and canary splits

A BareProxy plugin that splits visitors between versions of an app: a canary release that meets real traffic a little at a time, or an A/B test where each visitor keeps seeing the same design. It picks each visitor's variant and puts it in a request header. BareProxy's own route rules then pick the pool, so every routing decision stays in the config, where `plan` and `explain` can see it.

It's released as `splits.wasm` with each BareProxy release, from 0.9.0 on.

## Use it

```
plugin split /etc/bareproxy/plugins/splits.wasm
  config /etc/bareproxy/plugins/split.conf

site example.com
  use split
  route /* header X-Variant=canary -> app-next
  route /* -> app
```

## Config

```
# split.conf
variant stable 95
variant canary 5

force canary address 10.0.0.0/8          # the office
force canary header x-beta yes
force canary cookie beta=1
force canary query preview=canary        # a preview link; it sticks

header X-Variant
cookie bp_variant 30d
```

| Setting | Default | What it does |
|---|---|---|
| `variant NAME SHARE` | (two or more) | A variant and its share; shares are relative, so 95 and 5, or 1 and 1, or 50 50 |
| `force VARIANT address RANGE...` | | Requests from these addresses go to the variant |
| `force VARIANT header NAME VALUE` | | Requests whose header equals the value (case doesn't matter) |
| `force VARIANT cookie NAME=VALUE` | | Requests with this cookie |
| `force VARIANT query NAME=VALUE` | | Requests with this query parameter; the variant then sticks in the cookie, so a preview link works for the rest of the visit |
| `header NAME` | X-Variant | The request header the variant goes in |
| `cookie NAME [TIME]` | bp_variant 30d | The cookie that keeps a visitor on a variant |

## How a variant is picked

1. The first `force` line that matches, in file order.
2. The visitor's cookie, if it names a variant whose share isn't 0.
3. By the shares, for a visitor with no cookie. The plugin then sets the cookie (`Path=/`, `HttpOnly`, `SameSite=Lax`, and `Secure` on https).

The variant header is always set by the plugin; one a visitor sends is replaced, so nobody picks a variant by sending it.

## Ramping a canary

Going from 5 to 25 percent is a change to `split.conf`: `plan` shows it, `apply` makes it live, `rollback` undoes it. Visitors keep the variant their cookie names, so a new share applies to new visitors. To take everyone off a bad canary at once, set its share to 0: cookies for a variant with no share are ignored and those visitors are assigned again. To start a test over from scratch, change the cookie's name.

Pages that differ by variant shouldn't be cached by a shared cache as one page; send `Cache-Control: private` from the app, or keep the canary's pages out of the cache.

## What `why` and `status` show

Every request's record says its variant and why: "variant canary: assigned by share (5 of 100)", "variant canary: kept by the bp_variant cookie", "variant canary: forced by address 10.1.2.3 (line 4)". The route the core then matched is in the record as usual. `status` shows a counter for each variant, `variant.stable` and `variant.canary`, so the actual split is plain.

## A next step

A guard that watches the canary's errors against the stable version's, and rolls the split back by itself when they jump, is the natural plugin to follow. It isn't built yet.

## Build and test

`plugins/build.sh` builds it with the other plugins. `cargo test -p splits` runs its unit tests, among them a 100,000-visitor run that checks a 5 percent share lands within half a point. The server tests in `src/internal/bp/shipped_plugins_test.go` run it inside BareProxy: a 50/50 split over 400 visitors routed by the core, the cookie keeping a visitor on their variant, a header from outside ignored, a variant drained to 0, forces by header and by a preview link, and the counters.

## License

Apache 2.0, like the rest of BareProxy.
