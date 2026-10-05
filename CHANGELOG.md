# Changelog

## 0.1.0-alpha (5 October 2026)

Second cut, and the first with release binaries (Linux, amd64 and arm64).
Config changes are now planned before they go live and can be undone, and the
running server can say what it is doing.

New: `plan` says what a config change would do, in classes of requests, before
it goes live. `apply` makes a config live, and `apply --plan ID` refuses if
anything changed since that plan was made. `rollback` and `history` go back to
an earlier version and list the last 100, kept in a state folder. Listeners
change without a restart, removed backends drain, and a broken config file at
startup falls back to the last good version. These commands run over the admin
socket, and the history records the Unix user. `tail`, `status` and `events`
show records, state and changes as they happen. The latest records are kept in
memory (`why` looks there first), a trace log file can rotate, and a valid
`traceparent` header goes on to the backend. A browser demo runs the same
parser, explain and plan, compiled to WebAssembly.

Tested: acceptance tests (`explain` against the live server on 100,000
generated requests, 67 broken configs, 30,000 generated paths and 91 request
smuggling payloads), plan exactness on 1,000 generated config pairs, and 20
applies under load with no failed requests. Measurements against nginx 1.24.0
are in `results/bench-summary.md`.

Fixed after the acceptance tests: BareProxy now forwards only WebSocket
upgrades, and removes any other Upgrade header before the request reaches the
backend (an h2c upgrade could carry a request past a rule). A request body with
a bad chunk gets 400. `explain` keeps the query string on a folder redirect and
treats a raw backslash the way the server does. Static files go out with
sendfile where the platform allows. Response headers from a backend are limited
to 64 KB.

Known limits: automatic certificates are not built (certificate files only),
`OPTIONS *` is answered by Go's server and leaves no record, macOS and Windows
are untested, modules are not built, and the core is over its 5,000-line
budget.

## 0.1.0-dev (October 2026)

First public cut: a small web server and reverse proxy that explains every
routing decision. Static files, proxying to backend pools with health checks,
HTTPS with HTTP/2, a trace record per request, `explain` and `why`, and reload
on SIGHUP. License: Apache 2.0.
