# Webhook inbox

Added in 0.16.0. Part of the core, not a plugin (see "Why it isn't a plugin" below).

Stripe, GitHub, Shopify and most other services send webhooks once, retry for a while, and then give up. An app that is down, being deployed or slow loses the events sent in that window. An inbox stands in front of the app: it takes each webhook, writes it to disk, answers the sender `202 Accepted`, and then delivers it to the app, retrying with backoff until the app takes it. Every event stays on disk for a while, so it can be listed and sent again.

## Config

```
site hooks.example.com
  route /hooks/* -> inbox payments

inbox payments
  deliver app
  signature stripe /etc/bareproxy/stripe-secret
  key-header Idempotency-Key

pool app
  backend 127.0.0.1:8080
```

`route PATH -> inbox NAME` sends matching requests to the inbox. An inbox takes POST, PUT and PATCH; anything else gets 405. (A pool named `inbox` still works: `-> inbox` alone, or `-> inbox strip`, names that pool.)

| Setting | Default | What it does |
|---|---|---|
| `deliver POOL` | required | The pool events are delivered to. Backend choice, health and connection counts are the pool's own, shared with proxied requests. |
| `path PATH` | the request's path | The path events are delivered to on the app. The query string is kept. |
| `folder DIR` | `<state>/inbox/NAME` | Where events are kept. |
| `signature github FILE` | none | Checks `X-Hub-Signature-256` (HMAC-SHA256 of the body, hex) against the secret in FILE. |
| `signature shopify FILE` | | Checks `X-Shopify-Hmac-Sha256` (HMAC-SHA256 of the body, base64). |
| `signature stripe FILE` | | Checks `Stripe-Signature` (`t=` and one or more `v1=`, HMAC-SHA256 of `t.body`), and that `t` is within 5 minutes. |
| `signature hmac-sha256 HEADER FILE hex\|base64` | | Any other sender that signs the body with HMAC-SHA256. A leading `sha256=` is allowed. |
| `key-header HEADER` | none | The header that carries the sender's own ID for the event, such as `X-GitHub-Delivery` or `Idempotency-Key`. A second webhook with an ID the inbox already holds gets the first one's event ID and is not stored again, so the sender's own retries don't make duplicates. |
| `body-limit SIZE` | 1MB | The largest body taken (413 past it). The site's `body-limit` applies too. |
| `timeout DURATION` | 30s | How long one delivery attempt may take. |
| `retry FIRST MAX` | 10s 1h | The wait after the first failed attempt, doubling after each one, up to MAX. |
| `give-up DURATION` | 3d | An event still not delivered this long after it came in is marked failed. It can still be replayed. |
| `keep DURATION` | 30d | Delivered and failed events are deleted this long after their last attempt. |
| `workers N` | 4 | Deliveries in flight at once. Oldest events go first. |

Durations take `ms`, `s`, `m`, `h`, and `d` for days. The secret file holds the secret alone; spaces and line ends around it are ignored. A webhook whose signature doesn't match gets `401` and is not stored.

## What the sender and the app see

The sender gets `202` with `BareProxy-Inbox-Id: ID` and the text `Stored as event ID`, and only once the event is on disk. When the event can't be written (a full disk, say), it gets `503` and keeps the event, as it would for any failed delivery.

The app gets the request as it came: method, path (or `path`), query, headers and body, with `X-Forwarded-For`, `-Proto` and `-Host` set, and three more headers:

- `BareProxy-Inbox-Id`: the event ID, the same on every attempt. Use it to tell events apart.
- `BareProxy-Inbox-Attempt`: 1 for the first attempt, counting up on each retry.
- `BareProxy-Inbox-Replay`: only on deliveries that may repeat an earlier one: `operator` when someone replayed the event, `in-doubt` when BareProxy can't know whether an earlier attempt reached the app.

Any 2xx answer means delivered. Anything else, a refused connection or a timeout means try again later.

## The promises, and how they are kept

**No event lost.** Each event is two files: `ID.req` (the request) and `ID.state` (its delivery state). Every write goes to a temporary file, is flushed to disk, renamed over the old one, and the folder is flushed after. The sender hears `202` only after both are on disk.

**No event delivered twice unless marked.** Before each attempt, the event is marked `delivering` on disk. When BareProxy starts and finds an event still `delivering`, it stopped in the middle of that attempt (a crash, a kill, a power cut), so it can't know whether the app got it. The event is sent again with `BareProxy-Inbox-Replay: in-doubt`. The same mark goes on retries after an attempt whose request was sent but whose answer never came (a timeout). An attempt that never opened a connection isn't in doubt, and its retries aren't marked. An event the app answered with 2xx is sent again only when an operator replays it, marked `operator`.

So an app that handles `in-doubt` and `operator` deliveries idempotently (by checking `BareProxy-Inbox-Id`, or the sender's own event ID) sees each event exactly once.

**Retries follow the backoff.** 10s, 20s, 40s and so on up to an hour by default. The tests check the waits between attempts against the setting.

An inbox stays open across `apply` and `rollback` as long as its folder is the same, so a config change doesn't stop deliveries. A removed inbox keeps its files; adding it back delivers what is left.

## Commands

These ask the running server over the admin socket.

```
bareproxy inbox list [NAME] [--status pending|delivering|delivered|failed]
bareproxy inbox show NAME ID [--body]
bareproxy inbox replay NAME ID...
bareproxy inbox replay NAME --failed
```

`list` shows each inbox's counts and its events, newest first, with the last attempt's result and when the next one is due. `show` prints one event's request (headers, and the body with `--body`) and every attempt. `replay` sends events again, marked `operator`; `--failed` replays every failed event. All take `--json`. Replays go into `events`.

Each webhook leaves one request record like any other (`outcome inbox`, the inbox and the event ID), and `why` shows them. `explain` shows what an inbox route would do, signature check included. `plan` lists changes to inbox settings like any other block's.

## Why it isn't a plugin

BareProxy's plugins run in a WebAssembly sandbox through the Proxy-Wasm interface, and the inbox needs three things that sandbox can't give:

- It must write the event to disk, flushed, before it answers the sender. A plugin's store writes aren't flushed, and a plugin call may run for 5 ms.
- It delivers on its own schedule, with waits that last across restarts. A plugin calls out only while a request or a timer tick holds it, and only to addresses an `allow-http` line names.
- Replay is an operator command, and plugins have no commands.

So the inbox is Go code in the core (`src/internal/inbox`, with the config and routing in `src/internal/bp/inbox.go`), and delivery goes through the same pools, health checks and connection accounting as proxied requests.

## Known limits

- One BareProxy per folder. Two servers sharing an inbox folder would deliver events twice.
- Headers are stored as they came, `Authorization` and cookies included. The folder is created with mode 0750 and the files 0640; keep the state folder private.
- A pool without health checks gives a backend that went down one trial request every 10 seconds, so deliveries resume within about that after the app comes back. With `health` set, they resume after the checks pass.
- Adding an event writes and flushes two files, one event at a time per inbox. That is fast enough for webhook traffic, not for a firehose.
