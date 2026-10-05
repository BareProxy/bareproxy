<!--
Copyright 2026 BareProxy.com
SPDX-License-Identifier: Apache-2.0
-->

# BareProxy browser demo

The real BareProxy code, compiled to WebAssembly and running in a web page. A visitor edits a
config, checks it, asks how a request would be handled, and sees what a config change would do.
It all runs in the browser. The page loads its own five files and sends nothing anywhere.

Design rule 6 says the server, the commands, the tests and the browser demo run the same parser,
matcher, file lookup, explain and plan. The demo calls the same functions the commands call:
`bp.ParseWith` (the same parser as `bp.Parse`), `bp.Explain` and `bp.MakePlan`. Nothing is copied
or rewritten for the browser.

## What the page has

1. **Config.** An editor with line numbers, filled with the example from section 6 of the Design
   Note. Check lists problems with their line numbers (click one to jump to the line) and prints
   the same summary line as `bareproxy check`.
2. **Explain a request.** A method, a URL and optional headers, with five ready-made requests.
   The output is what `bareproxy explain --offline` prints: the site, every rule checked, the
   winner, the action. The winning rule is highlighted.
3. **Plan a change.** A second editor, filled with a changed config (a new `/api/v2/*` rule and
   pool, a new release folder, one more backend), and a Plan button. It is the text `plan` prints
   for the two configs.

Results follow as you type. The page is plain HTML, CSS and JavaScript (no libraries, nothing from
a CDN), uses system fonts and a light theme, and works from 320 px wide up. It looks the same
standalone and inside an iframe of about 760 px.

## What a browser can't do

A browser has no disk. The demo parses configs with `bp.ParseOptions{NoDisk: true}`, which skips
the two things that need one:

- Folders are not opened, so Check can't say "can't open folder". Certificate files are not
  read, so it can't say "can't load the certificate". Every other check is the same.
- For a files rule, Explain can't look at the folder. It names the file the server would look at
  (`Would check:`), says it did not look (`Not looked up:`), and lists what each outcome would be
  on the action line. On a server, explain looks and says what is there.

Backend states are unknown, as they are for `explain --offline` on any machine.

The example config has `tls CERT KEY` lines that the Design Note's example doesn't. The Design
Note uses automatic certificates, and this version doesn't build those yet. Check says so, exactly
as the server does, so the demo's example uses certificate files. The paths are not read.

## Files

| File | What it is |
| --- | --- |
| `main.go` | Registers `bareproxyCheck(config)`, `bareproxyExplain(config, method, url, headers)`, `bareproxyPlan(oldConfig, newConfig)` and `bareproxyVersion` for the page. Build tag `js && wasm`. |
| `web/index.html`, `web/demo.css`, `web/demo.js` | The page. |
| `build.sh` | Builds the folder to upload. |
| `check.mjs` | Headless check with Playwright. |
| `planref/` | A small native program that prints `bp.MakePlan(old, new).Text()`, for the check to compare with. |

`wasm_exec.js` is Go's own glue file. The build copies it from `$(go env GOROOT)/lib/wasm/`, so it
always matches the compiler that built the wasm. It is not kept in the source tree.

## Build

```
export PATH=/home/claude/tools/go1.27.1/bin:$PATH GOTOOLCHAIN=local GOPROXY=off GOFLAGS=-buildvcs=false
sh src/cmd/bareproxy-wasm/build.sh [DIST]
```

`DIST` defaults to `/home/claude/out/demo-dist`. The build uses only the Go standard library, so
nothing is downloaded. It runs
`GOOS=js GOARCH=wasm go build -trimpath -ldflags="-s -w"`, copies the page files and
`wasm_exec.js` next to the result, and prints the wasm size raw and gzipped. The folder holds
five files: `index.html`, `demo.css`, `demo.js`, `wasm_exec.js`, `bareproxy.wasm`.

## Serve it

Put the five files in one folder of any static web server. The links are relative, so the folder
can sit at any path (the plan is `https://bareproxy.com/live-demo/`). Two things help:

- Send `.wasm` as `application/wasm`. If a host doesn't, the page falls back to a slower way of
  starting and still works. The check tests this.
- Compress `.wasm` (gzip or brotli). It is the one big file.

To try it on your own machine: `cd DIST && python3 -m http.server 8000`, then open
http://localhost:8000/. Opening `index.html` straight from disk does not work in Chromium (tested),
and the page then says to use a web server.

## Check it

```
NODE_PATH=/opt/npm-tools/node_modules node src/cmd/bareproxy-wasm/check.mjs [--dist DIR] [--shots DIR] [--log FILE]
```

It builds the native `bareproxy` and `planref` from the same source tree, serves the built folder
on localhost, and drives the page in headless Chromium like a visitor. Then it compares:

- **Loading.** The indicator shows and the buttons are off while the wasm loads. A wrong content
  type still works. A missing wasm file shows an error, not a blank page. Opened from disk, the
  page says to use a web server.
- **Check.** The prefilled config gives the same summary as `bareproxy check`. A broken config
  gives the same problems with the same line numbers, the gutter marks the lines, and the "line N"
  links select them. A missing folder and certificate pass in the demo and fail natively, which is
  the expected difference.
- **Explain.** All five presets, a typed request with headers, 32 more requests on a second config
  (methods, header conditions, a wildcard site, a catch-all, plain http, another port, odd paths,
  bad URLs), and 23 behaviours the Design Note describes. Each result is compared with
  `bareproxy explain --offline` on the same config with real folders and a self-signed
  certificate. Everything up to the action line must be identical, character for character. For
  files rules the native result must be one of the outcomes the demo lists.
- **Plan.** The page's output is compared with `planref` on the same two configs, once read the
  way the demo reads them and once read from real folders and certificates. The plan ID hashes the
  config text, which has other paths in the second case, so it is masked there and the ID rule
  (first 12 hex digits of sha256 of old, a NUL byte, new) is checked on its own. A second pair with
  a rule that can never match checks the Warnings section.
- **Speed.** Median time of each call in the browser, measured.
- **Layout.** No sideways page scroll at 1280, 760 (in an iframe), 375 and 320 px, and every
  button and field at least 32 px tall. Screenshots go to `--shots` (default
  `/home/claude/out/demo-shots`).
- **Network.** Every request the page makes is a GET to the local server for one of the five
  files. None goes anywhere else.

The log is written to `results/demo-check.log` (change it with `--log`). The exit code is 1 when
anything fails. Until `bp.MakePlan` is the real one, pass `--allow-plan-stub`, otherwise a plan
that says "No changes." for the two example configs counts as a failure.

## Sizes

All measured on the build in this tree (Go 1.27.1, `-trimpath -ldflags="-s -w"`), with the real
`plan` merged in:

| File | Bytes |
| --- | --- |
| `bareproxy.wasm` | 11,502,780 raw, 3,027,740 gzipped (`gzip -9`) |
| `index.html` | 7,495 |
| `demo.css` | 10,297 |
| `demo.js` | 13,066 |
| `wasm_exec.js` | 16,992 |

The four small files add up to 47,850 bytes (computed). For scale, an empty Go program that only
imports `syscall/js`, built the same way, is 1,966,166 bytes raw and 575,344 gzipped. The rest of
the wasm is BareProxy and the standard library it uses. A smaller build is possible work for later
and hasn't been tried. These numbers change with the code, so `build.sh` prints the current ones.

## Licence

BareProxy and this demo are under the Apache License 2.0, Copyright 2026 BareProxy.com.
`wasm_exec.js`, and the Go runtime and standard library inside `bareproxy.wasm`, come from the Go
distribution and stay under the Go project's own BSD-style license (https://go.dev/LICENSE). The
header in `wasm_exec.js` stays as it is.
