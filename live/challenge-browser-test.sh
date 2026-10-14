#!/usr/bin/env bash
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# challenge-browser-test.sh: a real browser passes the bot challenges
# plugin. BareProxy serves a page behind the plugin, and headless Chromium
# loads it: it gets the puzzle, solves it with the page's own script,
# sends the answer, gets the pass cookie and lands on the page, with its
# image. A reload goes straight through. With JavaScript off, the page says
# the puzzle needs it. The server tests solve puzzles in Go; this is the
# check that the page's JavaScript works in a browser.
#
# Needs Go (GO=path), python3 with Playwright and its Chromium, and the
# plugin built (PLUGIN=path, plugins/dist/challenges.wasm by default:
# plugins/build.sh). Run from anywhere: live/challenge-browser-test.sh
set -euo pipefail
cd "$(dirname "$0")/.."
GO=${GO:-go}
PLUGIN=${PLUGIN:-plugins/dist/challenges.wasm}
work=$(mktemp -d)
pid=
cleanup() { [ -n "$pid" ] && kill "$pid" 2>/dev/null; wait 2>/dev/null || true; rm -rf "$work"; }
trap cleanup EXIT
port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')

(cd src && "$GO" build -o "$work/bareproxy" ./cmd/bareproxy)
cp "$PLUGIN" "$work/challenges.wasm"
mkdir -p "$work/public"
printf '<!doctype html><title>Real page</title><h1>The real page</h1><img src="/pic.svg" alt="">\n' >"$work/public/index.html"
printf '<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"/>\n' >"$work/public/pic.svg"
printf 'work 16\n' >"$work/challenges.conf"
cat >"$work/bareproxy.conf" <<EOF
global
  admin off
  state state

plugin challenges challenges.wasm
  config challenges.conf
  store 1MB

site http://localhost:$port
  use challenges
  route /* -> files public
EOF
"$work/bareproxy" check "$work/bareproxy.conf"
"$work/bareproxy" run "$work/bareproxy.conf" >"$work/run.log" 2>&1 &
pid=$!
for _ in $(seq 50); do python3 -c "import socket; socket.create_connection(('127.0.0.1', $port))" 2>/dev/null && break; sleep 0.1; done

python3 - "$port" <<'EOF'
import sys, time
from playwright.sync_api import sync_playwright
base = f"http://localhost:{sys.argv[1]}"
with sync_playwright() as p:
    b = p.chromium.launch()
    ctx = b.new_context()
    page = ctx.new_page()
    seen = []
    page.on("response", lambda r: seen.append(f"{r.status} {r.url.removeprefix(base)}"))
    t0 = time.time()
    page.goto(base + "/?from=test")
    page.wait_for_selector("text=The real page", timeout=30000)
    print(f"Chromium {b.version}: on the real page {time.time() - t0:.2f}s after asking for it")
    print("  " + "\n  ".join(seen))
    cookies = [(c["name"], c["httpOnly"], c["sameSite"]) for c in ctx.cookies()]
    print("cookies:", cookies)
    assert [s.split()[0] for s in seen] == ["403", "302", "200", "200"], seen
    assert cookies == [("bp_pass", True, "Lax")], cookies
    seen.clear()
    page.reload()
    page.wait_for_selector("text=The real page")
    print("after a reload:", ", ".join(seen))
    assert not any(s.startswith("403") for s in seen), seen
    nojs = b.new_context(java_script_enabled=False).new_page()
    nojs.goto(base + "/")
    text = nojs.inner_text("noscript")
    print("without JavaScript:", nojs.title(), "|", text)
    assert "needs JavaScript" in text
    b.close()
print("ok")
EOF
