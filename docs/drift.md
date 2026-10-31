# DNS and TLS drift watcher

Added in 0.16.0. Part of the core, not a plugin (see "Why it isn't a plugin" below).

The watcher keeps an eye on a list of domains, from a handful to hundreds. For each one it looks up DNS records, connects over TLS, reads the certificate and the HSTS header, and compares what it finds with the last known good state it keeps on disk. It reports:

- a certificate that ends within the window (21 days by default), or has ended;
- a certificate that changed, and whether its issuer changed with it;
- a certificate that doesn't verify (wrong name, unknown issuer, expired);
- changed A, AAAA, CNAME, MX, NS, TXT and CAA records, with before and after;
- nameservers that moved (an NS change, reported as such);
- no HSTS header, or one that turns HSTS off;
- no CAA records on the name or any parent name, so any CA may issue for it.

Every change goes into a history file, so you can see what changed and when.

## Config

A `drift` block goes in the same config file as everything else. A config may hold only drift blocks, for a CI job:

```
global
  state /var/lib/bareproxy

drift sites
  domains example.com www.example.com shop.example.com
  domains-file /etc/bareproxy/more-domains.txt
  resolver 1.1.1.1:53
  expiry 21d
  every 6h
  webhook https://alerts.example.com/bareproxy
```

| Setting | Default | What it does |
|---|---|---|
| `domains NAME...` | | Domains to watch. The line can repeat. |
| `domains-file FILE` | | More domains, any number a line, `#` starts a comment. Read at each check, so adding a domain needs no apply. |
| `records TYPE...` | all seven | Which of A AAAA CNAME MX NS TXT CAA to watch, or `none`. |
| `resolver HOST[:PORT]` | the first nameserver in /etc/resolv.conf | The recursive resolver asked. Port 53 when left out. |
| `timeout DURATION` | 5s | How long one lookup, or one TLS connection with its request, may take. |
| `expiry DURATION` | 21d | Alert when a certificate ends within this. |
| `tls on\|off` | on | Connect, read the certificate and the HSTS header. |
| `port N` | 443 | The TLS port. |
| `hsts on\|off` | on | Report a missing HSTS header. |
| `caa on\|off` | on | Report missing CAA records. |
| `ca FILE` | the system's | PEM certificates to trust instead of the system's, for internal CAs. |
| `workers N` | 16 | Domains checked at once. |
| `every DURATION` | none | A running server checks this often (at least 1m). Without it, only the `drift` command checks. |
| `webhook URL` | none | The report is posted here as JSON when it has alerts or unchecked parts. |
| `folder DIR` | `<state>/drift/NAME` | Where the state, the history and the lock file go. |

The TLS connection goes to the domain's A and AAAA addresses as just looked up (the first one that answers), with the domain as the server name. The certificate is read even when it doesn't verify, so a bad one is reported as bad rather than as a failed connection. HSTS is read from the answer to `GET /`; redirects aren't followed.

## Running a check

```
bareproxy drift [-c FILE] [--json] [NAME]...
bareproxy drift [-c FILE] [--json] --history [NAME]...
```

`bareproxy drift` checks every drift block (or the ones named) now. It reads the config itself and needs no running server. The exit code says what it found:

| Code | Meaning |
|---|---|
| 0 | Nothing to report |
| 3 | Nothing to alert on, but something couldn't be checked |
| 4 | At least one alert |
| 1 | The check couldn't run (a broken config, a folder it can't write) |

A report looks like this:

```
Drift watch sites, 3 domains, 2026-10-31 09:00:00 UTC, took 0.4 s
ALERT      example.com  A changed
           before: 192.0.2.1
           after:  192.0.2.7
ALERT      shop.example.com  certificate ends in 9 days (2026-11-09 12:00 UTC), inside the 21-day window
UNCHECKED  www.example.com  MX: lookup failed twice (timeout); last known 10 mx.example.com kept
Result: 2 alerts, 1 unchecked
```

`--history` prints every change recorded, oldest first, with before and after. `--json` gives the report or the history as JSON, the same JSON the webhook gets.

With `every` set, a running server checks the watch on its own: the time since the last check is taken from the state file, so a restart or an apply doesn't check early. Each check's summary and its alerts (the first 20) go into `events`, and the webhook gets the report. The server and the command never check one watch at the same time: a lock file in the folder keeps them apart, and a lock left by a check that died is taken over after 5 minutes.

## How it avoids false alarms and false calm

- **A failed lookup is unchecked, never unchanged.** A lookup that times out or gets SERVFAIL or REFUSED from the resolver is tried once more. If it fails again, that part is reported as unchecked, the exit code is at least 3, and its last known good value is kept for next time. The same goes for a TLS connection that fails twice. A name that doesn't exist (NXDOMAIN) or has no records of a type is an answer, an empty one.
- **One flapping answer doesn't alert.** A lookup that shows a change is asked again. If the second answer is the known value, the change isn't reported; a note says it flapped. If the second answer differs too, the change is reported with the second answer as "after". Certificates get the same second look, for load balancers with one stray node.
- **A change alerts once.** The new value becomes the known one and goes into the history. Standing problems alert on every check until they are fixed: a certificate inside its window or expired, one that doesn't verify, missing HSTS or CAA. When the connection fails, the last certificate seen still alerts if it is near its end.
- **The first check records, it doesn't alert on changes.** It notes what it found as the baseline. Standing problems still alert.

Answers cut short over UDP are asked again over TCP, so long TXT records are read whole.

## Why it isn't a plugin

BareProxy's plugins run in a WebAssembly sandbox through the Proxy-Wasm interface. The watcher needs raw DNS queries to a resolver and raw TLS connections to read certificates, and a plugin has neither: its only way out is an HTTP call through `proxy_http_call`. It also has to run from the command line in CI, where no server runs. So it is Go code in the core (`src/internal/drift`, with the config in `src/internal/bp/drift.go`). It sits next to the core's automatic certificates and the uptime check plugin planned as number 14.

## Known limits

- The resolver's cache stands between the watcher and the authoritative servers, so a change shows up once the old record's TTL runs out there.
- CAA is followed up the parent names to the last two labels, so a name under a public suffix such as `co.uk` is checked up to `example.co.uk`, not to `co.uk`.
- A domain with no address records and `tls on` is reported as unchecked on every check; give it a watch of its own with `tls off`.
- A renewed certificate is a changed certificate and alerts once, with the old and new end dates, so a renewal shows up as one.
