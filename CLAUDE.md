# Notes for Claude

Releases are automated. To release, raise the version and push to `main`;
never ask the owner to publish one. [RELEASING.md](RELEASING.md) has the
whole procedure, the checks to run afterwards, and how to copy it to a new
project.

## Done: independent review of inbox and drift, 0.16.1 (31 October 2026)

A separate agent reviewed both features. Five bugs confirmed, each with a
test that failed first (`src/internal/bp/inbox_review_test.go`,
`src/internal/drift/review_test.go`), all fixed and released as v0.16.1
(release files, SHA256SUMS and latest/download links checked; the Linux
binary prints 0.16.1, built with go1.27.3):
- Inbox (high): Go's transport resent a delivery by itself on a broken
  reused connection when it had an `Idempotency-Key` header, so the app got
  it twice with no mark. Fix: `out.GetBody = nil` in `deliverTo`.
- Inbox (low): headers named in `Connection` were passed on to the app.
- Drift (high): a server check cut off by an apply or shutdown saved state
  and history, so a change it found never alerted, and LastRun moved. Fix:
  `Run` returns `ctx.Err()` before saving.
- Drift (medium): with `hsts off`, a TLS server that doesn't answer HTTP
  was always unchecked, so its certificate was never watched.
- Drift (medium): an empty answer with neither RA nor AA (a referral) read
  as "no records" and alerted every record as removed; now unchecked.
Looked at and found sound: fsync and rename order with folder fsync, 202
only after both files, crash recovery and in-doubt marking, replay and
dedupe under one lock, admin lookups by map (no path traversal), signature
checks (raw body, constant-time compare, Stripe tolerance and several v1,
Shopify base64, GitHub prefix), reload refcounting, DNS sorting, TXT
joining, CNAME filtering, TCP fallback, SNI and verification, exit codes.
Open (not fixed, low): the drift lock's stale takeover has a narrow race
(two processes can both take a stale lock); `history.jsonl` grows without
limit; no cap on inbox event count or folder size. The proxy path itself
(not the inbox) is unaffected by the Idempotency-Key resend: incoming
bodies have no GetBody. Results re-recorded (48 in-doubt of 1,548; 500
domains in 1.8 s and 2.7 s); README and both site posts updated.

## Done: inbox and drift case studies (31 October 2026)

Rerunnable scripts and recorded logs in `results/case-studies/`
(`run-all.sh` reruns all; about six minutes; needs python3 with
`cryptography`, curl, jq), all against the released v0.16.0 Linux binary:
`inbox-deploy` (app down 67 s, 18/18 delivered once, longest wait 150 s with
default backoff), `inbox-crash` (SIGKILL with 4 deliveries in the app: 0
lost, 4 resends all marked in-doubt), `inbox-retries-replay` (forged and
stale signatures 401 and not stored; GitHub/Shopify repeats deduped by
key-header; a Stripe retry becomes a second event, since its ID is in the
body; failed events replayed as `operator`), `drift-watch` (DNS move: MX,
NS, CAA, issuer change found; flap noted, timeout unchecked; exit codes
4/4/4/0) and `use-examples` (six recipes). Public DNS answers NXDOMAIN in
the cloud sandbox and HTTPS is intercepted, so the drift runs use
`lib/fakenet.py` (a stand-in DNS + HTTPS server, not the repo's drifttest).
Site: four case study posts (tag "case studies") and a use examples post
on bareproxy.com, linked from the two feature posts and the plugins page.

Found during case studies (not bugs, nothing fixed): `give-up` is checked
when an attempt fails, so an event can be tried once past the give-up time
(up to one max backoff; seen: give-up 30s, last try at 34 s); docs/inbox.md
could say so. `drift --history --json` prints times in local time while
the text output is UTC. `BareProxy-Inbox-Attempt` restarts at 1 on an
operator replay (the code counts per round; the manual doesn't say).

## Done: webhook inbox and DNS/TLS drift watcher (0.16.0, 31 October 2026)

**What was done.** Two features in the core, released as v0.16.0 by the
release workflow (binaries, plugins and SHA256SUMS checked after release,
the downloaded Linux binary prints 0.16.0).

- **Webhook inbox**: `route PATH -> inbox NAME` and an `inbox NAME` block.
  Code: `src/internal/inbox/` (files, delivery loop, retries, replay, crash
  recovery; no BareProxy imports) and `src/internal/bp/inbox.go` (config,
  route action, signatures, delivery through `poolTransport`, admin
  endpoints, explain). Manual: `docs/inbox.md`.
- **Drift watcher**: a `drift NAME` block and `bareproxy drift`. Code:
  `src/internal/drift/` (DNS client on the vendored
  `golang.org/x/net/dns/dnsmessage`, TLS/HSTS probe, state, history, lock,
  report, webhook), `src/internal/drift/drifttest/` (stub resolver, test CA
  and TLS server), `src/internal/bp/drift.go` (config, server-side `every`
  loop). Manual: `docs/drift.md`.
- CLI: `bareproxy inbox list|show|replay`, `bareproxy drift [--history]`.
- Results: `live/inbox-drift-test.sh` writes `results/inbox-test.log`
  (1,500 webhooks, 15 SIGKILLs during delivery, 0 lost, 0 unmarked
  duplicates, 50 marked in-doubt) and `results/drift-test.log` (500 domains
  in 2.6 s, plus the failure tests).
- Site: two posts on bareproxy.com (`bareproxy-webhook-inbox`,
  `bareproxy-drift-watcher`), listed on the plugins page under the kernel,
  the roadmap and the about page.

**Why core, not plugins.** The inbox must flush to disk before answering
the sender, deliver on its own clock across restarts and replay on an
operator's command; the watcher needs raw DNS and TLS and runs from the
command line in CI. A Proxy-Wasm plugin can do none of these (its store
isn't flushed, it runs only inside a request or timer tick, its only way out
is an HTTP call to an `allow-http` address, it has no commands).

**Key design points.** Inbox: 202 only after `ID.req` and `ID.state` are
fsynced; each attempt is marked `delivering` on disk first, so a crash
leads to a resend marked `BareProxy-Inbox-Replay: in-doubt`; operator
replays are marked `operator`; inboxes are kept open across applies by
folder (reference counted). Drift: failed lookups get one retry, then are
unchecked (never unchanged, last good kept); a change must hold on a second
look; exit codes 0, 3 (unchecked), 4 (alerts); a lock file keeps the server
and the command apart.

**Status.** Released. The review was a second pass of my own (no separate
agent could be started in that session); its fixes are in commit 8301197
("Review fixes for the inbox and drift watcher"). CI also needed one fix to an existing, timing-sensitive
test: the crawler test servers now allow plugin calls 1 s
(TestCrawlersLimitAndReport lost counts when an instance was replaced on a
busy runner).

**Next.**
- Plugin 14, uptime checks and status pages (`docs/plugin-program.md`).
  It could reuse the drift watcher's report and webhook shape for alerts.
- Inbox: show inbox counts in `status`; an option to close inboxes at
  shutdown even while a retired runtime still holds them (today they stay
  open until the process exits, which is safe).
- Drift: a per-domain `tls off`, and real-world runs against a public
  resolver.
- The README says release binaries are built with Go 1.27.1; CI's
  `setup-go 1.27.x` built 0.16.0 with go1.27.3. Pin it or update the text.
- Session environment notes: Go 1.27.1 isn't installed in the cloud
  session; it was built from github.com/golang/go at tag go1.27.1 with Go
  1.25.1 as bootstrap. The Rust wasm32 target can't be downloaded there, and
  the Actions job log host is blocked; annotations (check-runs API) work.
