#!/usr/bin/env bash
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# consent-browser-test.sh: a real browser through the cookie consent plugin.
# BareProxy serves a page with a tracking script behind the plugin, and
# headless Chromium loads it: the banner shows and the script doesn't run.
# Accept all brings the browser back to the page with the script running
# and its cookie set; the settings page takes it back, and the cookie goes.
# With JavaScript off, the banner's links still work, and a browser that
# sends Global Privacy Control gets no banner. The server tests check the
# plugin's answers; this checks what a browser makes of them.
#
# Needs Go (GO=path), python3 with Playwright and its Chromium, and the
# plugin built (PLUGIN=path, plugins/dist/consent.wasm by default:
# plugins/build.sh). Run from anywhere: live/consent-browser-test.sh
set -euo pipefail
cd "$(dirname "$0")/.."
GO=${GO:-go}
PLUGIN=${PLUGIN:-plugins/dist/consent.wasm}
work=$(mktemp -d)
pid=
cleanup() { [ -n "$pid" ] && kill "$pid" 2>/dev/null; wait 2>/dev/null || true; rm -rf "$work"; }
trap cleanup EXIT
port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')

(cd src && "$GO" build -o "$work/bareproxy" ./cmd/bareproxy)
cp "$PLUGIN" "$work/consent.wasm"
mkdir -p "$work/public"
cat >"$work/public/index.html" <<'HTML'
<!doctype html><html><head><title>Shop</title><script src="/tracker.js"></script></head>
<body><h1>The shop</h1><p><a href="/.bareproxy-consent">Cookie settings</a></p></body></html>
HTML
printf 'window.tracked = true; document.cookie = "_ga=GA1.test; path=/";\n' >"$work/public/tracker.js"
cat >"$work/consent.conf" <<'CONF'
category analytics Analytics: to count visits
cookies analytics _ga
block analytics /tracker.js
CONF
cat >"$work/bareproxy.conf" <<EOF2
global
  admin off
  state state

plugin consent consent.wasm
  config consent.conf
  body response

site http://localhost:$port
  use consent
  route /* -> files public
EOF2
"$work/bareproxy" check "$work/bareproxy.conf"
"$work/bareproxy" run "$work/bareproxy.conf" >"$work/run.log" 2>&1 &
pid=$!
for _ in $(seq 50); do python3 -c "import socket; socket.create_connection(('127.0.0.1', $port))" 2>/dev/null && break; sleep 0.1; done

python3 - "$port" <<'EOF2'
import sys
from playwright.sync_api import sync_playwright
base = f"http://localhost:{sys.argv[1]}"
def names(ctx):
    return sorted(c["name"] for c in ctx.cookies())
with sync_playwright() as p:
    b = p.chromium.launch()
    print("Chromium", b.version)
    ctx = b.new_context()
    page = ctx.new_page()
    page.goto(base + "/?from=test")
    banner = page.locator("#bp-consent")
    assert banner.is_visible(), "no banner"
    assert page.evaluate("window.tracked") is None and names(ctx) == [], names(ctx)
    print("first visit: the banner shows, the tracker doesn't run, no cookies")
    page.click("text=Accept all")
    page.wait_for_url(base + "/?from=test")
    assert page.locator("#bp-consent").count() == 0
    assert page.evaluate("window.tracked") is True, "the tracker didn't run"
    assert names(ctx) == ["_ga", "bp_consent"], names(ctx)
    print("after Accept all: back on the page, the tracker runs, cookies", names(ctx))
    page.click("text=Cookie settings")
    assert page.locator("input[value=analytics]").is_checked()
    page.uncheck("input[value=analytics]")
    page.click("text=Save my choices")
    page.wait_for_url(base + "/?from=test")  # the page the settings were opened from
    assert page.locator("#bp-consent").count() == 0
    assert page.evaluate("window.tracked") is None, "the tracker ran"
    assert names(ctx) == ["bp_consent"], names(ctx)
    assert ctx.cookies()[0]["value"].endswith(".none"), ctx.cookies()
    print("after taking it back: the tracker doesn't run, the _ga cookie is gone")
    nojs = b.new_context(java_script_enabled=False)
    np = nojs.new_page()
    np.goto(base + "/")
    np.click("text=Reject all")
    np.wait_for_url(base + "/")
    assert np.locator("#bp-consent").count() == 0 and names(nojs) == ["bp_consent"], names(nojs)
    print("without JavaScript: Reject all works, and the banner goes")
    gpc = b.new_context(extra_http_headers={"Sec-GPC": "1"})
    gp = gpc.new_page()
    gp.goto(base + "/")
    assert gp.locator("#bp-consent").count() == 0 and gp.evaluate("window.tracked") is None
    print("with Global Privacy Control: no banner, and the tracker doesn't run")
    b.close()
print("ok")
EOF2
