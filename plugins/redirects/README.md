# Redirects from a file

A BareProxy plugin for the redirects a site migration leaves behind: thousands of them, in one plain file, the kind a migration script or a spreadsheet export produces. The file is read into a table, so a request costs a few lookups however long the list is. A changed file is picked up on its own, and a broken one is refused while the old list keeps working. Every redirect is counted, so after a while it's clear which old URLs still matter.

It's released as `redirects.wasm` with each BareProxy release, from 0.6.0 on.

## Use it

```
plugin moved /etc/bareproxy/plugins/redirects.wasm
  config /etc/bareproxy/plugins/moved.conf
  read /etc/bareproxy/redirects

site example.com
  use moved
  route /* -> files /var/www/example/public
```

The `read` line names the folder the list is in. The plugin can read nothing else.

## The plugin's config

```
# moved.conf
file redirects.txt        # the list, inside the read folder
status 301                # for lines that don't name one
query keep                # keep or drop the query by default
reload 30s                # how often the file is checked for changes; off
report /.redirects        # a page with the hit counts
report-allow 127.0.0.1 ::1 203.0.113.0/24
```

| Setting | Default | What it does |
|---|---|---|
| `file NAME` | (needed) | The list, a path inside a folder the plugin line names with `read` |
| `status CODE` | 301 | The status for lines that don't name one: 301, 302, 307 or 308 |
| `query keep` or `drop` | keep | Whether the query string goes along to the new address |
| `reload TIME` or `off` | 30s | How often the file is checked; it's reloaded only when it changed |
| `report PATH` | none | A plain-text page of hit counts at this path |
| `report-allow ADDR...` | 127.0.0.1 ::1 | Who may see the report; to anyone else the path is an ordinary one |

## The list

One redirect a line: the old path, the new path or URL, then optionally a status and `drop-query` or `keep-query`. `#` starts a comment.

```
# old path              new address
/p/123                  /posts/hello
/about-us               /about/                       308
/search                 /find                         drop-query
/shop/old-cat/*         /shop/new-cat/*
/blog/*                 https://blog.example.com/*    302
/blog/2019/*            /archive/2019/*
```

- An old path matches exactly, and also with or without a trailing slash: `/p/123/` goes where `/p/123` goes.
- An old path ending in `/*` is a prefix. The rest of the path goes where the new address has its `*`, or nowhere if it has none. The longest prefix wins, and an exact line beats any prefix.
- The new address is a path on the site or a full `http://` or `https://` URL. The query goes along, after a `&` if the new address has a query of its own.
- Paths compare as they arrive, percent-encoding and all.

A line that can't be read stops the whole file, with the line number: an old path listed twice, a status that isn't one, a `*` anywhere but at the end, a path that redirects to itself. At the start (and in `check` and `plan`) that's an error naming the file and the line. On a reload, BareProxy's log says why and the list as it was keeps working until the file is fixed.

## What `why` and `status` show

Each redirected request is answered by the plugin, and its record carries the line that matched: "redirects.txt line 12: /p/123 to /posts/hello (301)". `status` shows the counter `redirects`, the total since the plugin started.

## The report

`report /.redirects` adds a plain-text page with each line's hits since the plugin started, most first, and how many lines were never used. `/.redirects?unused` lists those lines, the ones that can probably go. Each instance shares its counts with the others at every reload tick, so the page can be up to one tick behind. The counts start again when BareProxy restarts or the plugin's config changes; the trace log keeps every redirect for good, with its line.

## Size

The file can be as large as the plugin's `body-limit` (8 MB by default). A list of 100,000 redirects loads in under half a second on a small server, when each instance starts and when the file changes; requests then cost a few hash lookups. A reload runs in a timer tick, which gets at least a second. For a list that takes longer than that, raise `timeout` on the plugin line or set `reload off` and apply after each change.

## Build and test

`plugins/build.sh` builds it with the other plugins. `cargo test -p redirects` runs its unit tests; the server tests in `src/internal/bp/shipped_plugins_test.go` run it inside BareProxy with exact and prefix redirects, hits counted across two instances, a reload, a broken file refused, `check` naming the broken line, and a list of 100,000 lines.

## License

Apache 2.0, like the rest of BareProxy.
