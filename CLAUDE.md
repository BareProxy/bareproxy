# Notes for Claude

Releases are automated. To release, raise the version and push to `main`;
never ask the owner to publish one. [RELEASING.md](RELEASING.md) has the
whole procedure, the checks to run afterwards, and how to copy it to a new
project.

## In progress: webhook inbox and DNS/TLS drift watcher

Started 31 October 2026. Both ship together in one minor release, 0.16.0.
This section is updated and pushed (with `[skip ci]`) after every step, so
a fresh session or ChatGPT can carry on from the repo alone.

### What and why

- **Webhook inbox.** Takes incoming webhooks (Stripe, GitHub, Shopify and
  the like), writes each one to disk before the sender hears yes (202),
  delivers it to the app behind with retries and backoff, and lets the
  operator list and replay any stored event. Optional HMAC signature check
  per inbox (github, shopify, stripe, or a generic hmac-sha256 header).
  Senders retry for a while and then drop events; an app that is down or
  being deployed loses them. The inbox holds them for it.
- **DNS and TLS drift watcher.** Watches a list of domains (hundreds is
  fine) and alerts on certificates ending within a window, changed
  certificates and issuers, changed DNS records (A, AAAA, CNAME, MX, NS,
  TXT, CAA) against the last known good state, nameserver moves, and
  missing HSTS and CAA. Keeps a diff history. Reports as text or JSON, with
  an exit code for CI and an optional webhook. It sits next to the planned
  auto TLS and uptime check work.

### Where each one lives, and why: core, not plugins

Both are core Go packages, not Proxy-Wasm plugins. A plugin can't do what
they need:

- The inbox must write the event to disk and flush it *before* answering
  the sender. A plugin's store write isn't flushed to disk, and a plugin
  call may run for 5 ms. Delivery needs timers that survive restarts and
  calls to the app on the inbox's own schedule; a plugin only calls out
  while a request or a timer tick holds it, and only to `allow-http`
  addresses. Replay needs an operator command, which plugins don't have.
- The drift watcher needs raw DNS queries (UDP and TCP to a resolver) and
  raw TLS connections to read certificates. A plugin has neither: its only
  network access is HTTP calls through `proxy_http_call`. It also has to run
  from the command line in CI, with no server running.

Layout:

- `src/internal/inbox/`: the inbox itself (files, delivery loop, retries,
  replay, crash recovery). No BareProxy imports.
- `src/internal/bp/inbox.go`: the `inbox NAME` config block, the
  `route PATH -> inbox NAME` action, the signature check, delivery through
  the pool's backends (`poolTransport`, so health and accounting are shared),
  `explain`, records.
- `src/internal/drift/`: the watcher (DNS client on
  `golang.org/x/net/dns/dnsmessage`, newly vendored from x/net v0.58.0, the
  same version already vendored; TLS and HSTS probe; state, history, lock,
  report, webhook). No BareProxy imports.
- `src/internal/bp/drift.go`: the `drift NAME` config block, and a
  running server checking each watch with `every` set, alerts to `events`
  and the webhook.
- `src/cmd/bareproxy/main.go`: `bareproxy inbox` (list, show,
  replay, over the admin socket) and `bareproxy drift [NAME] [--history]`
  (runs without a server).

### Design decisions so far

- **Inbox files.** Per event, `ID.req` (request JSON line, then the body;
  written once) and `ID.state` (delivery state; rewritten). Every write is
  temp file, fsync, rename, fsync of the folder. Default folder
  `<state>/inbox/NAME`.
- **Delivery promise.** No event lost: 202 only after both files are on
  disk; a failed write answers 503 so the sender retries. No event
  delivered twice unless marked: an event is marked `delivering` on disk
  before each attempt. On start, an event still `delivering` was cut off,
  so it is sent again with `BareProxy-Inbox-Replay: in-doubt`. An attempt
  whose answer never came (timeout after the request was sent) is also
  in-doubt. Operator replays carry `BareProxy-Inbox-Replay: operator`.
  Attempts where no connection opened are not in doubt. Every delivery
  carries `BareProxy-Inbox-Id` and `BareProxy-Inbox-Attempt`, so the app can
  dedupe on the ID.
- **Retries.** Backoff doubles from `retry FIRST MAX` (default 10s, 1h);
  `give-up` (default 3d) marks it failed (an event note); failed events
  can be replayed. Delivered and failed events are deleted after `keep`
  (default 30d). Oldest first, `workers` (default 4) in flight.
  `key-header` (such as `X-GitHub-Delivery`) dedupes a sender's own retries.
- **Inboxes are kept open across applies** by folder (reference counted),
  so an apply doesn't stop delivery.
- **Drift watch rules.** A failed or timed-out lookup is tried once more,
  then reported as unchecked, and the last known good value is kept. A
  lookup showing a change is tried once more: if the second look shows the
  known value, it is a "flapped" note, not an alert. Expiry alerts every
  run (from the last seen certificate when the connection fails). Changes
  alert once; the new value becomes the known one, and the change goes into
  `history.jsonl`. Missing HSTS and CAA (CAA inherited from parent names)
  alert every run unless turned off. Exit codes: 0 clean, 3 something
  unchecked, 4 alerts (1 for errors, 2 for usage). A lock file keeps the
  server and the command from checking one watch at once.
- No scheduled (cron) GitHub workflows: the owner's Actions minutes are
  budgeted.

### Steps

1. [done, pushed; tests come in step 5] Inbox package and its config, route action,
   signature check, delivery through pools, explain and records.
2. [done, pushed] Drift package: DNS client, TLS/HSTS probe, state,
   history, lock, report, webhook.
3. [done, pushed] Drift config block in bp, server-side `every` loop with events and
   webhook; allow a config with drift blocks and no sites (for CI).
4. [done, pushed] CLI: `bareproxy inbox list|show|replay` (admin socket endpoints) and
   `bareproxy drift`.
5. [done, pushed] Tests. Inbox: every operation; upstream down then back; a process
   killed with SIGKILL mid-delivery and restarted; no event lost, none
   delivered twice unless marked; backoff honoured. Drift: expiring and
   changed certificates always alert; changed records alert with before and
   after; lookup failure or timeout is unchecked, never unchanged; one
   flapping lookup doesn't alert. All on local test servers and a stub
   resolver, no internet.
6. [done, pushed; re-record after the review] Record results in `results/` (script in `live/`, like the others).
7. [done, pushed] Review of the diff (a second pass of my own; no
   separate agent could be started in that session), fixes, re-record.
   Found and fixed: a data race in the TLS probe (a late handshake wrote to
   a reused probe); an inbox that failed to open during an apply called
   rt.Stop(), which would have stopped health checks of backends shared
   with the running config, after the shared ACME state had already taken
   the new config (now opened before ACME, and only what the new runtime
   started is stopped); CAA gap judged from a stale timestamp; Expect and
   Forwarded headers passed on to the app; a broken state file could panic;
   plan missed changes in all but the last `domains` line; the browser demo
   would refuse drift blocks with no resolver.
8. [in progress: docs, CHANGELOG and version 0.16.0 pushed; watching CI and the release] README, docs, CHANGELOG 0.16.0, version bump in
   `src/internal/bp/server.go`; push; watch CI and the release; check the
   files, SHA256SUMS, the latest/download links, run a downloaded binary.
9. [ ] Site (photozeitgeist/bareproxy.com): two posts with numbers from
   `results/`, listed where the site lists features; check the deploy once.
10. [ ] Turn this section into the normal done/status/next entry.

### If this session stops

Start at the first step not marked done. Code is pushed after each step. All tests
pass, with -race too (`internal/inbox`, `internal/drift`, `bp/inbox_test.go`,
`bp/drift_test.go`, `cmd/bareproxy/drift_test.go`). The SIGKILL test reruns
the bp test binary as a real BareProxy (TestMain, `BP_TEST_RUN_CONFIG`). Build with Go 1.27.1 (`GOTOOLCHAIN=local`, `-mod=vendor`).
