# Notes for Claude

Releases are automated. To release, raise the version and push to `main`;
never ask the owner to publish one. [RELEASING.md](RELEASING.md) has the
whole procedure, the checks to run afterwards, and how to copy it to a new
project.

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
agent could be started in that session); its fixes are in the CHANGELOG
history of commits. CI also needed one fix to an existing, timing-sensitive
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
