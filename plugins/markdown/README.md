# Markdown serving

A BareProxy plugin that serves Markdown files as web pages, straight from a folder. Write `guide/setup.md`, and `/guide/setup` shows it as HTML inside a template. There's no build step and no output folder to keep in sync: change the file, reload the page, and the change is there. Tools and AI agents that ask for Markdown get the file as written, at the same URL.

It's released as `markdown.wasm` with each BareProxy release, from 0.14.0 on. It needs BareProxy 0.14.0 or later.

## Use it

```
plugin md /etc/bareproxy/plugins/markdown.wasm
  read /var/www/docs

site docs.example.com
  use md
  route /* -> files /var/www/docs
```

The plugin line's `read` names the same folder the files route serves. The plugin answers for the pages it finds there; everything else (pictures, styles, HTML files, the `.md` files themselves) goes on to the route as before. No config file is needed. With one:

```
plugin md /etc/bareproxy/plugins/markdown.wasm
  config /etc/bareproxy/plugins/md.conf
  read /var/www/docs
```

## Config

```
# md.conf
template .page.html      # the page around the content
index index.md README.md # the files that serve a folder's URL
skip /api/               # paths the plugin leaves alone
```

| Setting | Default | What it does |
|---|---|---|
| `template FILE` | a plain built-in page | The page around the content: a file in a folder the plugin line reads, with `{{content}}` in it |
| `index NAME...` | index.md README.md | The files that serve a folder's URL, in order |
| `skip PATH...` | none | Path prefixes the plugin leaves alone |
| `html on` or `off` | on | Raw HTML in the Markdown passes as written, or shows as text |
| `links on` or `off` | on | Links to `.md` files point to their pages |
| `accept on` or `off` | on | A client that asks for `text/markdown` gets the Markdown at the page's URL |
| `lang TAG` | en | `{{lang}}`, when the front matter doesn't give one |
| `largest SIZE` | 512KB | The largest file it renders |
| `cache SIZE` | 16MB | The memory rendered pages take in each instance; 0 keeps none |

## Which file a URL gets

- `/guide/setup` gets `guide/setup.md`, and `/notes/v1.2` gets `notes/v1.2.md`.
- `/guide/` gets `guide/index.md`, or `guide/README.md` when there's no index.md. `/` gets the folder's own.
- `/guide`, when `guide` is a folder, is the files route's: it sends the visitor to `/guide/`, so links in the page work from there.
- `/guide/setup.md` is the files route's too: the Markdown as it is, as `text/markdown`.
- When there's no such file, the request goes on to the route, which serves a picture or an HTML file as usual, or answers 404.

Only GET and HEAD requests get pages. Hidden names (`.git`, `.page.html`) are never looked for, as the files route never serves them, and neither are the `skip` paths. A path is decoded and put in its normal form first, as BareProxy routes it, so `/caf%C3%A9` finds `café.md`.

## The Markdown

CommonMark, with what GitHub adds: tables, task lists, strikethrough, footnotes and alerts (`> [!NOTE]`). Headings get ids the way GitHub makes them, so `## Getting started` can be linked as `#getting-started`; write `## Install {#setup}` to choose one. Definition lists work too. A code block's language becomes its class (`language-rust`), for a highlighter in the template; the plugin doesn't color code itself.

Front matter at the top of a file, `key: value` lines between `---` lines or `key = value` lines between `+++` lines, gives the page its values. `title`, `description` (or `summary`) and `lang` mean what they say, and every plain value can go into the template by name. Lists and nested keys are skipped.

A page's title is the front matter's title, or its first `#` heading, or else its file's name (the folder's name for an index file, the site's for the front page). Its description is the front matter's, or the first paragraph's text, cut to about 200 characters.

With `links on`, a link to `setup.md` points to `setup`, and a link to `docs/index.md` or `docs/README.md` points to `docs/`, keeping any `#part`. So a folder of Markdown that links file to file, as on GitHub, works as a site. Links with a scheme (`https:`) or to another host, and pictures, are left alone.

With `html on`, raw HTML in a file passes as written, as the folder's owner wrote it. Turn it off for a folder others can write to: raw HTML then shows as text, and links and pictures may only be relative, http, https, mailto or tel (and `data:image/` for a picture), so a file can't run a script.

## The template

Without a template line, pages go out in a plain built-in page that reads well on a phone and in dark mode. A template of your own is an HTML file with names in double braces:

```html
<!doctype html>
<html lang="{{lang}}">
<title>{{title}} | Docs</title>
<meta name="description" content="{{description}}">
<link rel="alternate" type="text/markdown" href="{{source}}">
<nav>{{toc}}</nav>
<main>{{content}}</main>
<footer>{{author}}</footer>
```

| Name | What it holds |
|---|---|
| `{{content}}` | The page, as HTML. A template needs it |
| `{{toc}}` | Links to the page's level-two and level-three headings, as a list; empty with fewer than two |
| `{{title}}`, `{{description}}`, `{{lang}}` | As above |
| `{{path}}` | The page's path, such as /guide/setup |
| `{{source}}` | The URL of its Markdown, such as /guide/setup.md |
| `{{file}}` | Its file inside the folder, such as guide/setup.md |
| any other | The front matter value of that name, or nothing |

Every value is escaped as text except `content` and `toc`. Keep the template in the site's folder under a hidden name, such as `.page.html`: the plugin reads it, and the files route never serves it. It's read at the start, so a missing or broken one is an error in `check` and `plan`. After that a changed template counts at once. One that breaks is refused with a note, and the one before stays.

## Markdown for tools and agents

A client whose Accept header asks for `text/markdown` before `text/html` gets the Markdown as written, at the page's own URL. Browsers never ask that way, so they get HTML. Both answers carry `Vary: Accept`, so a cache in between keeps them apart. Every page also points to its Markdown with a `Link: </guide/setup.md>; rel="alternate"; type="text/markdown"` header, and the built-in page with the same `<link>` tag. `accept off` turns this off, and everyone gets HTML.

## Rendered once

Each page is rendered once and kept until its file or the template changes. On every request the plugin checks the file's size and time of change, which costs a stat and no read (`bareproxy_stat_file`, new in BareProxy 0.14.0), so an edit shows on the next request. Each instance keeps its own pages, up to `cache`; the least recently used go first.

A page's ETag comes from its file's size and time of change, the template's, the config and the plugin's version. Every instance gives the same one, so a browser that asks `If-None-Match` gets a 304 from any instance, without the file even being read. The plugin sends no `Last-Modified` and no `Cache-Control`, so browsers check each time and get a quick 304 when nothing changed. Add a `Cache-Control` header with the header rules plugin to let them keep pages longer.

## Big pages

Pages render in a timer call, a millisecond after the request comes in, and the request waits for it. A request's own call may run for 5 milliseconds (the plugin line's `timeout`), and a file's size says little about how long it takes to render: a few kilobytes of quotes inside quotes take longer than a hundred of plain text. A timer call may run for a second. Inside BareProxy's sandbox the renderer gets through about 2 to 5 MB of Markdown a second: on a 2.1 GHz server, a 10 KB page took 2 to 5 ms, and a 500 KB one 90 to 280 ms, depending on how many headings and lists it had. A timer call renders for up to 250 ms, then leaves the pages still waiting to the next one.

A file over `largest` gets a 500 page and a note. Raise `largest` with `memory` on the plugin line; rendering takes several times a file's size in memory. A file the plugin can't read because it's over the plugin line's `body-limit` (8 MB by default) gets the same.

A file whose rendering takes its instance down, by running out of memory or past the time limit, is marked. BareProxy starts a new instance, and after 3 seconds requests for that file get a 500 page saying it can't be shown, with a note, until the file changes. One bad file can't keep the plugin down.

## What `why` and `status` show

- "rendered guide/setup.md (12 KB) in 3.1 ms"
- "guide/setup.md as rendered before"
- "guide/setup.md hasn't changed since the client's copy (ETag)"
- "guide/setup.md as rendered before; sent the Markdown, as the client asked for text/markdown"
- "no guide/setup.md in the folders the plugin line names with read, so the request went on"
- "read the template .page.html again, as it changed"
- "the template .page.html has no {{content}}, the place the page goes, so the one before stays"
- "huge.md is 640 KB, over largest 512 KB, so it wasn't rendered: raise largest (and memory on the plugin line)"
- "rendering deep.md stopped part way before, out of time or memory, so it isn't tried again until the file changes"

A miss for a name with a dot in it, such as `/style.css`, has no note. `status` shows three counters: `pages` (HTML pages sent), `rendered` and `markdown` (Markdown sent to clients that asked for it).

## Limits worth knowing

- One plugin line serves the folders it reads. Two sites with folders of their own need a plugin line each (the same `.wasm` file twice, under two names); with one, a page missing from one folder could come from the other.
- Every `.md` file in the folder is public, as a page and as Markdown. There are no drafts.
- No folder listings: a folder without an index file goes on to the route.
- No code coloring, math or diagrams built in. A script in the template can add them.
- Pages go out uncompressed.
- The first view of a changed page waits for it to render, once in each instance.

## Build and test

`plugins/build.sh` builds it with the other plugins. `cargo test -p markdown` runs its unit tests: the config, which file a URL gets, Accept headers, ETags, the template, and rendering (GitHub's additions, heading ids, the table of contents, front matter, links to `.md` files, and raw HTML with `html off`). The server tests in `src/internal/bp/shipped_plugins_test.go` run it inside BareProxy: pages and folders, the cache and a changed file, HEAD, 304s, Markdown for agents, the template and its changes, big and odd pages, a file that takes its instance down, and configs refused.

## License

Apache 2.0, like the rest of BareProxy.
