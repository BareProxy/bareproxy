#!/usr/bin/env node
// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0
//
// Headless check of the browser demo.
//
// It serves the built demo folder on localhost (with .wasm as application/wasm),
// opens the page in headless Chromium, and drives it like a visitor: Check,
// every Explain preset, Plan. Each result is compared with what the native
// program prints for the same input, built from the same source tree:
//
//   check    bareproxy check FILE
//   explain  bareproxy explain --offline --config FILE ...
//   plan     planref (this folder), which calls bp.MakePlan directly
//
// It also takes screenshots, checks the layout at 1280, 760 (inside an iframe)
// and 375 px, and records every network request the page makes.
//
//   usage: node check.mjs [--dist DIR] [--shots DIR] [--log FILE] [--allow-plan-stub]
//
// Needs: Go on the PATH (or in /home/claude/tools/go1.27.1), openssl, and
// Playwright with its Chromium (NODE_PATH=/opt/npm-tools/node_modules).
// The exit code is 1 when any check fails.

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import http from 'node:http';
import crypto from 'node:crypto';
import { createRequire } from 'node:module';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const srcDir = path.resolve(here, '..', '..');   // the folder with go.mod
const repoDir = path.resolve(srcDir, '..');

// ---- arguments ---------------------------------------------------------------

const args = process.argv.slice(2);
const opt = (name, dflt) => { const i = args.indexOf(name); return i >= 0 && args[i + 1] ? args[i + 1] : dflt; };
const DIST = path.resolve(opt('--dist', '/home/claude/out/demo-dist'));
const SHOTS = path.resolve(opt('--shots', '/home/claude/out/demo-shots'));
const LOGFILE = path.resolve(opt('--log', path.join(repoDir, 'results', 'demo-check.log')));
const ALLOW_PLAN_STUB = args.includes('--allow-plan-stub');

// ---- log and results ---------------------------------------------------------

const lines = [];
const out = (s = '') => { lines.push(s); console.log(s); };
let passes = 0, fails = 0;
function ok(name, cond, detail = '') {
  if (cond) { passes++; out('PASS  ' + name); return true; }
  fails++;
  out('FAIL  ' + name);
  if (detail) out(detail.split('\n').map((l) => '        ' + l).join('\n'));
  return false;
}
const note = (s) => out('NOTE  ' + s);
const head = (s) => { out(''); out('== ' + s); };
const indent = (text, n = 8) => String(text).replace(/\n+$/, '').split('\n').map((l) => ' '.repeat(n) + l).join('\n');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ---- tools -------------------------------------------------------------------

const goEnv = { ...process.env, GOTOOLCHAIN: 'local', GOPROXY: 'off', GOFLAGS: '-buildvcs=false' };
if (!/go1\.27/.test(goEnv.PATH || '')) goEnv.PATH = '/home/claude/tools/go1.27.1/bin:' + goEnv.PATH;

function run(cmd, argv, o = {}) {
  try {
    const stdout = execFileSync(cmd, argv, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], ...o });
    return { code: 0, stdout, stderr: '' };
  } catch (e) {
    return { code: e.status ?? 1, stdout: String(e.stdout || ''), stderr: String(e.stderr || '') };
  }
}

function loadPlaywright() {
  const dirs = (process.env.NODE_PATH || '/opt/npm-tools/node_modules').split(path.delimiter).filter(Boolean);
  const tries = [import.meta.url, ...dirs.map((d) => path.join(d, '_'))];
  for (const t of tries) {
    try { return createRequire(t)('playwright'); } catch { /* try the next place */ }
  }
  throw new Error('playwright not found: set NODE_PATH to the folder that holds it');
}

// ---- the folder the native program reads --------------------------------------
// The demo has no disk, so the native side gets real folders and a real
// (self-signed) certificate. The folder name is as long as the demo's own
// (/var/www/example/public, 23 characters) so the rule table lines up the same.

const DEMO_PUBLIC = '/var/www/example/public';
const DEMO_RELEASE = '/var/www/example/releases/2026-10-05';
const DEMO_CRT = '/etc/bareproxy/example.com.crt';
const DEMO_KEY = '/etc/bareproxy/example.com.key';

function makeFixture() {
  const tmp = fs.mkdtempSync('/tmp/bpdc-');
  const w = (rel, text) => { const f = path.join(tmp, rel); fs.mkdirSync(path.dirname(f), { recursive: true }); fs.writeFileSync(f, text); };
  w('public/index.html', '<h1>home</h1>\n');
  w('public/about/index.html', '<h1>about</h1>\n');
  w('public/404.html', '<h1>missing</h1>\n');
  w('public/missing.html', '<h1>missing too</h1>\n');
  w('public/style.css', 'body{}\n');
  w('public/static/app.js', 'x\n');
  fs.mkdirSync(path.join(tmp, 'releases/2026-10-05'), { recursive: true });
  w('releases/2026-10-05/index.html', '<h1>new</h1>\n');
  const r = run('openssl', ['req', '-x509', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:prime256v1', '-nodes',
    '-keyout', path.join(tmp, 'key.pem'), '-out', path.join(tmp, 'cert.pem'), '-days', '2', '-subj', '/CN=example.com']);
  if (r.code !== 0) throw new Error('openssl failed: ' + r.stderr);
  return {
    tmp,
    // demo config text -> text for the native program
    toNative: (t) => t.replaceAll(DEMO_CRT, path.join(tmp, 'cert.pem')).replaceAll(DEMO_KEY, path.join(tmp, 'key.pem'))
      .replaceAll(DEMO_RELEASE, path.join(tmp, 'releases/2026-10-05')).replaceAll(DEMO_PUBLIC, path.join(tmp, 'public')),
    // native output -> comparable with the demo's
    fromNative: (t) => t.replaceAll(path.join(tmp, 'bareproxy.conf'), 'bareproxy.conf').replaceAll(path.join(tmp, 'releases/2026-10-05'), DEMO_RELEASE)
      .replaceAll(path.join(tmp, 'public'), DEMO_PUBLIC).replaceAll(path.join(tmp, 'cert.pem'), DEMO_CRT).replaceAll(path.join(tmp, 'key.pem'), DEMO_KEY),
  };
}

// ---- a static server for the built folder --------------------------------------

function serve(dist, extra) {
  const types = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8', '.wasm': 'application/wasm' };
  const seen = [];
  const server = http.createServer((req, res) => {
    const url = new URL(req.url, 'http://localhost');
    let name = decodeURIComponent(url.pathname).replace(/^\/+/, '');
    // The real site serves the demo at /live-demo/, so the same files answer there too.
    if (name.startsWith('live-demo/')) name = name.slice('live-demo/'.length);
    if (name === '') name = 'index.html';
    const rec = { method: req.method, path: url.pathname, status: 0 };
    seen.push(rec);
    if (extra[name]) { rec.status = 200; res.writeHead(200, { 'content-type': 'text/html; charset=utf-8' }); res.end(extra[name]); return; }
    const file = path.join(dist, name);
    if (name.includes('/') || name.includes('..') || !fs.existsSync(file) || !fs.statSync(file).isFile()) {
      rec.status = 404; res.writeHead(404, { 'content-type': 'text/plain' }); res.end('not found'); return;
    }
    rec.status = 200;
    res.writeHead(200, { 'content-type': types[path.extname(file)] || 'application/octet-stream', 'cache-control': 'no-store' });
    fs.createReadStream(file).pipe(res);
  });
  return new Promise((resolve) => server.listen(0, '127.0.0.1', () => resolve({ server, seen, port: server.address().port })));
}

// ---- comparing explain output ---------------------------------------------------

// Everything up to and including the Path line is the routing decision: the
// site, each rule checked, the winner. It must be identical in the demo and
// the native program. What follows is the action. The only part that may
// differ is a files rule, because a browser has no folder to look in.
function splitAtPath(text) {
  const rows = text.replace(/\n+$/, '').split('\n');
  const i = rows.findIndex((r) => /^(Normalized path|Path): /.test(r) || /^No site for|^Nothing listens|^Port \d+ (expects|is plain)/.test(r));
  return i < 0 ? { route: rows, rest: [] } : { route: rows.slice(0, i + 1), rest: rows.slice(i + 1) };
}

function compareExplain(demo, nativeText) {
  const d = splitAtPath(demo), n = splitAtPath(nativeText);
  const problems = [];
  if (d.route.join('\n') !== n.route.join('\n')) problems.push('routing lines differ');
  const dFiles = d.rest.some((r) => r.startsWith('Would check: '));
  if (!dFiles) {
    if (d.rest.join('\n') !== n.rest.join('\n')) problems.push('action lines differ');
    return { problems, kind: 'identical' };
  }
  // files rule
  const dPath = (d.rest.find((r) => r.startsWith('Would check: ')) || '').slice('Would check: '.length);
  const nRow = n.rest.find((r) => /^(Checked|File): /.test(r)) || '';
  const nPath = nRow.replace(/^(Checked|File): /, '');
  if (!dPath || dPath !== nPath) problems.push(`file differs: demo "${dPath}", native "${nPath}"`);
  const dAct = d.rest.find((r) => r.startsWith('Action: ')) || '';
  const nAct = n.rest.find((r) => r.startsWith('Action: ')) || '';
  const want = nAct.replace(/^Action: /, '');
  if (want.startsWith('serve 200')) {
    const ct = (n.rest.find((r) => r.startsWith('Content-Type: ')) || '').replace('Content-Type: ', '');
    if (!dAct.includes('serve 200') || (ct && !dAct.includes('(' + ct + ')'))) problems.push(`demo action "${dAct}" does not allow native "${nAct}" (${ct})`);
  } else if (!dAct.includes(want) && !(want === '404' && dAct.includes('otherwise 404'))) {
    problems.push(`demo action "${dAct}" does not allow native "${nAct}"`);
  }
  const nErr = n.rest.find((r) => r.startsWith('Error page: '));
  const dErr = d.rest.find((r) => r.startsWith('Error page: '));
  if (nErr && (!dErr || !dErr.startsWith(nErr))) problems.push(`error page line differs: demo "${dErr}", native "${nErr}"`);
  return { problems, kind: 'files' };
}

// ---- the check -------------------------------------------------------------------

const BROKEN = [
  'site example.com',
  '  tls /etc/bareproxy/example.com.crt /etc/bareproxy/example.com.key',
  '  route /api/* -> nopool strip',
  '  route /x -> respond 200 "ok" extra',
  '  bogus setting',
  '',
  'site example.com',
  '  route /* -> files /var/www/example/public',
  '',
  'pool api',
  '  backend 10.0.0.11',
  '',
].join('\n');

// More than the presets: methods, header conditions, wildcard, catch-all,
// plain http, another port, odd paths. Used with the demo's functions directly.
const MATRIX_CONF = `# matcher coverage
site example.com
  tls ${DEMO_CRT} ${DEMO_KEY}
  error 404 /missing.html
  route GET,HEAD /static/* -> files ${DEMO_PUBLIC}
  route POST /submit -> api
  route /beta/* header X-Beta=1 -> api-beta strip
  route /beta/* header X-Beta -> respond 202 "beta, other value"
  route /old -> redirect 308 https://example.com/new
  route /a/b -> respond 204
  route /* -> files ${DEMO_PUBLIC}

site *.example.com
  tls ${DEMO_CRT} ${DEMO_KEY}
  route /* -> redirect 302 https://example.com/wild

site http://plain.test
  route /* -> respond 200 "plain"

site other.test:8443
  tls ${DEMO_CRT} ${DEMO_KEY}
  route /* -> api

site http://narrow.test
  route /only -> respond 200 "x"

pool api
  backend 10.0.0.11:8080
  health /healthz

pool api-beta
  backend 10.0.0.21:8080
  backend 10.0.0.22:8080
  host-header beta.internal
`;

const MATRIX = [
  ['GET', 'https://example.com/static/app.js'], ['HEAD', 'https://example.com/static/'],
  ['POST', 'https://example.com/static/x'], ['POST', 'https://example.com/submit'],
  ['GET', 'https://example.com/submit'], ['GET', 'https://example.com/beta/x', ['X-Beta: 1']],
  ['GET', 'https://example.com/beta/x', ['X-Beta: 2']], ['GET', 'https://example.com/beta/x'],
  ['GET', 'https://example.com/beta', ['x-beta: 1']], ['GET', 'https://example.com/old'],
  ['GET', 'https://example.com/a/b'], ['GET', 'https://example.com/a/b/'],
  ['GET', 'https://example.com/a/../b'], ['GET', 'https://example.com/a//b/./c'],
  ['GET', 'https://example.com/a%2Fb'], ['GET', 'https://example.com/%2e%2e/etc/passwd'],
  ['GET', 'https://foo.example.com/x'], ['GET', 'https://a.b.example.com/x'],
  ['GET', 'http://plain.test/'], ['GET', 'https://other.test:8443/x'],
  ['GET', 'https://example.com:8443/'], ['GET', 'http://example.com:8080/'],
  ['GET', 'https://example.com/?q=1'], ['GET', 'http://example.com/old?x=1'],
  ['OPTIONS', 'https://example.com/submit'], ['GET', 'https://EXAMPLE.com/About/'],
  ['GET', 'https://example.com/style.css'], ['GET', 'https://example.com/about'],
  ['GET', 'https://nosuch.test/'], ['GET', 'https://example.com/missing-thing'],
  ['GET', 'http://narrow.test/only'], ['GET', 'http://narrow.test/other'],
];

// What the design note says the config does, checked on the demo's own output.
// [config (A = the prefilled one, M = the matrix one), method, url, headers, text the output must contain]
const EXPECT = [
  ['A', 'GET', 'https://example.com/api/orders', [], 'Sent upstream as GET /orders with Host: example.com'],
  ['A', 'GET', 'https://example.com/api', [], 'Pool api (line 13)'],
  ['A', 'GET', 'https://example.com/api/', [], 'Pool api (line 13)'],
  ['A', 'GET', 'https://example.com/apiv2', [], 'Would check: /var/www/example/public/apiv2'],
  ['A', 'GET', 'https://example.com/healthz', [], 'Action: BareProxy answers 200 itself with "ok"'],
  ['A', 'GET', 'https://www.example.com/x?y=1', [], 'Action: redirect 301 to https://example.com/x?y=1'],
  ['A', 'GET', 'http://www.example.com/x?y=1', [], 'Action: redirect 301 to https://www.example.com/x?y=1'],
  ['A', 'GET', 'https://nosuch.test/', [], 'Action: 421 (no_site)'],
  ['A', 'GET', 'https://example.com/a/../healthz', [], 'Normalized path: /healthz'],
  ['A', 'HEAD', 'https://example.com/about/', [], 'Would check: /var/www/example/public/about/index.html'],
  ['A', 'DELETE', 'https://example.com/about/', [], 'Action: 405, files answer only GET and HEAD'],
  ['A', 'GET', 'https://example.com:8443/', [], 'Nothing listens on port 8443, so the connection is refused'],
  ['A', 'GET', 'http://example.com:443/', [], 'Port 443 expects HTTPS, so a plain HTTP request fails there'],
  ['M', 'GET', 'https://example.com/beta/x', ['X-Beta: 1'], 'Sent upstream as GET /x with Host: beta.internal'],
  ['M', 'GET', 'https://example.com/beta/x', ['x-beta: 1'], 'Sent upstream as GET /x with Host: beta.internal'],
  ['M', 'GET', 'https://example.com/beta/x', ['X-Beta: 2'], 'Action: BareProxy answers 202 itself'],
  ['M', 'GET', 'https://example.com/beta/x', [], 'Would check: /var/www/example/public/beta/x'],
  ['M', 'POST', 'https://example.com/submit', [], 'Sent upstream as POST /submit with Host: example.com'],
  ['M', 'GET', 'https://example.com/submit', [], 'Would check: /var/www/example/public/submit'],
  ['M', 'GET', 'https://foo.example.com/x', [], 'Action: redirect 302 to https://example.com/wild'],
  ['M', 'GET', 'https://a.b.example.com/x', [], 'Action: 421 (no_site)'],
  ['M', 'GET', 'http://narrow.test/other', [], 'No rule matches'],
  ['M', 'GET', 'http://narrow.test/other', [], 'Action: 404 (no_route)'],
];

const BAD_REQUESTS = [['GET', 'example.com/x'], ['GET', 'ftp://example.com/x'], ['GET', 'https://example.com:abc/']];

async function main() {
  const t0 = Date.now();
  out('BareProxy browser demo check');
  out('Run: ' + new Date().toString());
  out('Demo folder: ' + DIST);
  for (const f of ['bareproxy.wasm', 'index.html', 'demo.js', 'demo.css', 'wasm_exec.js']) {
    if (!fs.existsSync(path.join(DIST, f))) { out('FAIL  missing ' + f + ' in ' + DIST); fs.mkdirSync(path.dirname(LOGFILE), { recursive: true }); fs.writeFileSync(LOGFILE, lines.join('\n') + '\n'); process.exit(1); }
  }
  const wasmRaw = fs.statSync(path.join(DIST, 'bareproxy.wasm')).size;
  const gz = run('sh', ['-c', `gzip -9 -c '${path.join(DIST, 'bareproxy.wasm')}' | wc -c`]).stdout.trim();
  out(`bareproxy.wasm: ${wasmRaw} bytes raw, ${gz} bytes gzipped (gzip -9)`);
  fs.mkdirSync(SHOTS, { recursive: true });

  head('What differs between the demo and the native program, and why');
  out(`  1. The first line names bareproxy.conf, the demo's config name. The native program prints the file's path.
  2. A browser has no disk. Folders are not opened and certificate files are not read. So:
     - Check does not report "can't open folder" or "can't load the certificate". Everything else in Check is the same.
     - For a files rule, Explain prints "Would check:" (the same file the server would look at) and "Not looked up:"
       where the native program prints "Checked:", "Exists:" and a plain action. The demo's action line lists what each
       outcome would be, and the native result must be one of them. This check tests that it is.
  3. Everything else, the site, every rule checked, the winner, the path, redirects, respond, pool and strip, must be
     identical, byte for byte, after the config path and folder names are mapped back.
  4. The native side gets real temp folders and a self-signed certificate, with a folder name as long as the demo's so
     the rule table lines up. The native build is from the same source tree as the wasm build.`);

  head('Setup');
  const fx = makeFixture();
  out('fixture folder: ' + fx.tmp);
  const bin = path.join(fx.tmp, 'bareproxy'), planref = path.join(fx.tmp, 'planref');
  let r = run('go', ['build', '-o', bin, './cmd/bareproxy'], { cwd: srcDir, env: goEnv });
  ok('native bareproxy builds', r.code === 0, r.stderr);
  r = run('go', ['build', '-o', planref, './cmd/bareproxy-wasm/planref'], { cwd: srcDir, env: goEnv });
  ok('native planref builds', r.code === 0, r.stderr);
  out('native: ' + run(bin, ['version']).stdout.trim());
  const confFile = path.join(fx.tmp, 'bareproxy.conf');
  const native = {
    check: (text) => { fs.writeFileSync(confFile, fx.toNative(text)); const x = run(bin, ['check', confFile]); return { ...x, stdout: fx.fromNative(x.stdout), stderr: fx.fromNative(x.stderr) }; },
    explain: (text, method, url, hs = []) => {
      fs.writeFileSync(confFile, fx.toNative(text));
      const x = run(bin, ['explain', '--offline', '--config', confFile, ...hs.flatMap((h) => ['-H', h]), method, url]);
      return { ...x, stdout: fx.fromNative(x.stdout), stderr: fx.fromNative(x.stderr) };
    },
    plan: (a, b, nodisk) => {
      const fa = path.join(fx.tmp, 'plan-old.conf'), fb = path.join(fx.tmp, 'plan-new.conf');
      fs.writeFileSync(fa, nodisk ? a : fx.toNative(a)); fs.writeFileSync(fb, nodisk ? b : fx.toNative(b));
      const x = run(planref, [...(nodisk ? ['-nodisk'] : []), fa, fb]);
      return { ...x, stdout: fx.fromNative(x.stdout), stderr: fx.fromNative(x.stderr) };
    },
  };

  const { chromium } = loadPlaywright();
  const extra = {
    '_frame.html': `<!doctype html><meta charset="utf-8"><title>frame</title><body style="margin:0;padding:16px;background:#e9edf1"><iframe id="f" src="/live-demo/" title="BareProxy live demo" style="display:block;width:760px;height:1100px;border:1px solid #b8c2cc;background:#fff"></iframe></body>`,
  };
  const { server, seen, port } = await serve(DIST, extra);
  const base = `http://127.0.0.1:${port}`;
  out('serving ' + DIST + ' at ' + base);
  const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
  out('browser: Chromium ' + browser.version() + ' (headless, Playwright)');
  const consoleProblems = [];
  const hookPage = (page, label) => {
    page.on('console', (m) => { if (['error', 'warning'].includes(m.type())) consoleProblems.push(`${label}: console ${m.type()}: ${m.text()}`); });
    page.on('pageerror', (e) => consoleProblems.push(`${label}: page error: ${e.message}`));
  };
  const outside = [];
  const hookRequests = (ctx) => ctx.on('request', (rq) => { if (!rq.url().startsWith(base + '/') && !rq.url().startsWith('data:') && !rq.url().startsWith('blob:')) outside.push(rq.method() + ' ' + rq.url()); });

  try {
    // ---- loading state and load time -------------------------------------------
    head('Loading');
    {
      const ctx = await browser.newContext({ viewport: { width: 375, height: 812 }, deviceScaleFactor: 2 });
      hookRequests(ctx);
      const page = await ctx.newPage(); hookPage(page, 'loading');
      await page.route('**/bareproxy.wasm', async (route) => { await sleep(1800); await route.continue(); });
      await page.goto(base + '/index.html', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#status[data-state="loading"]', { timeout: 10000 });
      const during = await page.evaluate(() => ({
        text: document.getElementById('status-text').textContent,
        barShown: getComputedStyle(document.querySelector('#status .bar')).display !== 'none',
        disabled: ['check-btn', 'explain-btn', 'plan-btn'].map((id) => document.getElementById(id).disabled),
        explain: document.getElementById('explain-out').textContent,
      }));
      ok('loading indicator shows while the core loads', during.barShown && /Loading/.test(during.text), JSON.stringify(during));
      ok('buttons are disabled while loading', during.disabled.every(Boolean), JSON.stringify(during.disabled));
      await page.screenshot({ path: path.join(SHOTS, 'demo-375-loading.png') });
      await page.waitForSelector('#status[data-state="ready"]', { timeout: 120000 });
      ok('page becomes ready after the delayed load', true);
      await ctx.close();
    }
    {
      // wrong content type: the page falls back from streaming compile
      const ctx = await browser.newContext(); hookRequests(ctx);
      const page = await ctx.newPage(); hookPage(page, 'wrong-type');
      await page.route('**/bareproxy.wasm', async (route) => {
        const resp = await route.fetch();
        await route.fulfill({ response: resp, headers: { ...resp.headers(), 'content-type': 'application/octet-stream' } });
      });
      await page.goto(base + '/index.html');
      let ready = true;
      try { await page.waitForSelector('#status[data-state="ready"]', { timeout: 120000 }); } catch { ready = false; }
      ok('page still starts when the host sends .wasm with the wrong content type', ready);
      const wrongTypeMsgs = consoleProblems.filter((m) => m.startsWith('wrong-type'));
      if (wrongTypeMsgs.length) note('console messages in that case (expected, from the refused streaming compile): ' + wrongTypeMsgs.join(' | '));
      await ctx.close();
    }
    {
      const ctx = await browser.newContext(); hookRequests(ctx);
      const page = await ctx.newPage();
      await page.route('**/bareproxy.wasm', (route) => route.fulfill({ status: 404, body: 'gone' }));
      await page.goto(base + '/index.html');
      let state = '';
      try { await page.waitForSelector('#status[data-state="error"]', { timeout: 20000 }); state = await page.textContent('#status-text'); } catch { state = 'no error state'; }
      ok('a missing wasm file shows an error, not a blank page', /Could not load/.test(state), state);
      const outTxt = await page.textContent('#explain-out');
      ok('and the output panels say the core is not running', /not running/.test(outTxt), outTxt);
      await ctx.close();
    }

    // ---- the real page ---------------------------------------------------------
    const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
    hookRequests(ctx);
    const page = await ctx.newPage(); hookPage(page, 'main');
    const tLoad = Date.now();
    await page.goto(base + '/index.html');
    await page.waitForSelector('#status[data-state="ready"]', { timeout: 120000 });
    const wall = ((Date.now() - tLoad) / 1000).toFixed(1);
    const statusText = await page.textContent('#status-text');
    out(`status line: "${statusText}" (page load to ready, wall clock ${wall} s, local server)`);
    const confA = await page.$eval('#cfg-a', (e) => e.defaultValue);
    const confB = await page.$eval('#cfg-b', (e) => e.defaultValue);
    const version = await page.evaluate(() => String(window.bareproxyVersion));
    out('core version string: ' + version);
    ok('the core version equals the native one', run(bin, ['version']).stdout.includes('bareproxy ' + version), version);

    // ---- check ------------------------------------------------------------------
    head('Check');
    {
      const chip = (await page.textContent('#check-chip')).trim();
      const summary = (await page.textContent('#check-summary')).trim();
      const n = native.check(confA);
      const nSummary = n.stdout.trim().split('\n').pop();
      out('demo:   ' + summary);
      out('native: ' + nSummary);
      ok('prefilled config: demo summary equals native bareproxy check', summary === nSummary && n.code === 0, `demo "${summary}" native "${nSummary}"`);
      ok('prefilled config: result chip says ok, no problems', chip === 'ok, no problems', chip);
      const nB = native.check(confB);
      const bSummary = await page.evaluate((t) => window.bareproxyCheck(t).summary, confB);
      ok('plan editor config: summary equals native', nB.stdout.trim().split('\n').pop() === 'bareproxy.conf: ok, ' + bSummary, bSummary + ' vs ' + nB.stdout);

      // broken config typed into the editor
      await page.fill('#cfg-a', BROKEN);
      await page.click('#check-btn');
      await sleep(350);
      const items = await page.$$eval('#check-list li', (lis) => lis.map((li) => {
        const b = li.querySelector('.lineref');
        return { line: b ? Number(b.dataset.line) : 0, kind: li.querySelector('.kind').textContent, msg: li.querySelector('.msg').textContent };
      }));
      const demoProblems = items.map((p) => (p.line ? `line ${p.line}: ` : '') + `${p.kind}: ${p.msg}`);
      const nb = native.check(BROKEN);
      const nativeProblems = nb.stdout.trim().split('\n').filter((l) => !l.endsWith(': has errors, so it can\'t be used'));
      out('demo problems:\n' + indent(demoProblems.join('\n')));
      ok('broken config: the problem list equals native, line numbers included', JSON.stringify(demoProblems) === JSON.stringify(nativeProblems) && nb.code === 1,
        'demo:\n' + demoProblems.join('\n') + '\nnative:\n' + nativeProblems.join('\n'));
      ok('broken config: summary says it has errors', /has errors/.test(await page.textContent('#check-summary')));
      const marks = await page.$$eval('#gut-a div', (ds) => ds.map((d, i) => (d.classList.contains('err') ? i + 1 : 0)).filter(Boolean));
      const wantMarks = [...new Set(items.filter((p) => p.kind === 'error' && p.line).map((p) => p.line))];
      ok('broken config: the editor gutter marks every error line', JSON.stringify(marks) === JSON.stringify(wantMarks), `marked ${marks} wanted ${wantMarks}`);
      const chipBad = (await page.textContent('#check-chip')).trim();
      ok('broken config: result chip counts the errors', new RegExp('^' + items.filter((p) => p.kind === 'error').length + ' errors?').test(chipBad), chipBad);
      await page.click('#check-list .lineref >> nth=1');
      const sel = await page.$eval('#cfg-a', (e) => e.value.slice(e.selectionStart, e.selectionEnd));
      const second = items.filter((p) => p.line)[1];
      ok('clicking "line N" selects that line in the editor', sel === BROKEN.split('\n')[second.line - 1], `selected "${sel}" line ${second.line}`);
      await page.screenshot({ path: path.join(SHOTS, 'demo-1280-check-errors.png'), clip: { x: 0, y: 0, width: 1280, height: 900 } });
      await page.click('#reset-a');
      await sleep(350);
      ok('Reset example restores the prefilled config and a clean check', (await page.textContent('#check-chip')).trim() === 'ok, no problems' && (await page.$eval('#cfg-a', (e) => e.value)) === confA);

      // what the demo cannot check, because it has no disk
      const diskConf = 'site example.com\n  tls /nonexistent/a.crt /nonexistent/a.key\n  route /* -> files /nonexistent/folder\n';
      const dRes = await page.evaluate((t) => window.bareproxyCheck(t), diskConf);
      const nDisk = native.check(diskConf);
      ok('no disk: the demo reports ok for a missing folder and certificate', dRes.ok === true && dRes.problems.length === 0, JSON.stringify(dRes));
      ok('no disk: the native program reports both (the expected difference)', nDisk.code === 1 && /can't open folder/.test(nDisk.stdout) && /can't load the certificate/.test(nDisk.stdout), nDisk.stdout);
      const autoConf = 'site example.com\n  route /* -> respond 200 "x"\n';
      const aRes = await page.evaluate((t) => window.bareproxyCheck(t), autoConf);
      ok('automatic certificates: the demo gives the same error the server gives', !aRes.ok && /automatic certificates aren't built yet/.test(aRes.problems[0].msg) && native.check(autoConf).stdout.includes(aRes.problems[0].msg));
    }

    // ---- when the core misbehaves ------------------------------------------------------
    head('Check: the page when the core throws or returns something unexpected');
    {
      const c5 = await browser.newContext(); hookRequests(c5);
      const p5 = await c5.newPage(); hookPage(p5, 'misbehave');
      await p5.goto(base + '/index.html');
      await p5.waitForSelector('#status[data-state="ready"]', { timeout: 120000 });
      for (const [what, body] of [['throws', "() => { throw new Error('boom'); }"], ['returns text (as the panic guard does)', "() => 'bareproxy: internal error: boom\\n'"], ['returns nothing', '() => undefined']]) {
        await p5.evaluate((src) => { window.bareproxyCheck = eval(src); }, body);
        await p5.click('#check-btn');
        const chipTxt = (await p5.textContent('#check-chip')).trim();
        const sum = (await p5.textContent('#check-summary')).trim();
        const shownProblems = await p5.$$eval('#check-list li', (ls) => ls.map((l) => l.textContent));
        ok(`core ${what}: the page shows an error and not "ok"`, /error/.test(chipTxt) && /has errors/.test(sum) && shownProblems.length === 1, `chip "${chipTxt}", summary "${sum}", problems ${JSON.stringify(shownProblems)}`);
      }
      await c5.close();
    }

    // ---- explain: the presets -----------------------------------------------------
    head('Explain: the presets');
    const presets = await page.$$eval('#presets .chip', (cs) => cs.map((c) => ({ method: c.dataset.method, url: c.dataset.url, label: c.textContent })));
    ok('there are five presets', presets.length === 5, String(presets.length));
    const wantPresets = ['GET https://example.com/about/', 'GET https://example.com/api/orders', 'GET https://example.com/old-page/', 'GET http://example.com/', 'GET https://www.example.com/x'];
    ok('the presets are the ones listed in the task', JSON.stringify(presets.map((p) => p.method + ' ' + p.url)) === JSON.stringify(wantPresets), JSON.stringify(presets.map((p) => p.label)));
    for (const p of presets) {
      await page.click(`#presets .chip[data-url="${p.url}"]`);
      const cmd = (await page.textContent('#explain-cmd')).trim();
      const shown = await page.$$eval('#explain-out .ln', (ls) => ls.map((l) => l.textContent.trimEnd()));
      const raw = await page.evaluate(([c, m, u]) => window.bareproxyExplain(c, m, u, ''), [confA, p.method, p.url]);
      const n = native.explain(confA, p.method, p.url);
      const cmp = compareExplain(raw, n.stdout);
      out('');
      out(`$ ${p.method} ${p.url}`);
      out(indent(raw));
      ok(`${p.label}: command line shown is "bareproxy explain --offline --config bareproxy.conf ..."`, cmd === `$ bareproxy explain --offline --config bareproxy.conf ${p.method} ${p.url}`, cmd);
      ok(`${p.label}: the page shows exactly what the core returned`, shown.join('\n') === raw.replace(/\n+$/, '').split('\n').map((l) => l.trimEnd()).join('\n'));
      ok(`${p.label}: matches native (${cmp.kind})`, n.code === 0 && cmp.problems.length === 0, cmp.problems.join('\n') + '\nnative:\n' + n.stdout + '\ndemo:\n' + raw);
      const hits = await page.$$eval('#explain-out .hit', (hs) => hs.map((h) => h.textContent));
      ok(`${p.label}: exactly one rule row is highlighted as the match`, hits.length === 1, JSON.stringify(hits));
      ok(`${p.label}: the preset chip is marked as selected`, await page.$eval(`#presets .chip[data-url="${p.url}"]`, (c) => c.classList.contains('on')));
    }

    // ---- explain: typed requests ----------------------------------------------------
    head('Explain: typed requests, headers and methods through the form');
    await page.selectOption('#method', 'POST');
    await page.fill('#url', 'https://example.com/about/');
    ok('the headers box is closed until the visitor opens it', !(await page.$eval('details.headers', (d) => d.open)));
    await page.click('details.headers > summary');
    await page.fill('#headers', 'X-Beta: 1\nAccept-Encoding: gzip');
    await page.click('#explain-btn');
    {
      const cmd = (await page.textContent('#explain-cmd')).trim();
      const raw = await page.evaluate(([c]) => window.bareproxyExplain(c, 'POST', 'https://example.com/about/', 'X-Beta: 1\nAccept-Encoding: gzip'), [confA]);
      const n = native.explain(confA, 'POST', 'https://example.com/about/', ['X-Beta: 1', 'Accept-Encoding: gzip']);
      ok('form: command line includes the -H headers', cmd === '$ bareproxy explain --offline --config bareproxy.conf -H "X-Beta: 1" -H "Accept-Encoding: gzip" POST https://example.com/about/', cmd);
      ok('form: POST to a files rule is a 405, as natively', raw.includes('Action: 405, files answer only GET and HEAD') && compareExplain(raw, n.stdout).problems.length === 0, raw + '\nnative:\n' + n.stdout);
      ok('form: no preset chip is marked while the request is custom', (await page.$$('#presets .chip.on')).length === 0);
    }
    await page.fill('#headers', '');
    await page.selectOption('#method', 'GET');

    // ---- explain: a wider matrix, through the demo's own functions ---------------
    head(`Explain: ${MATRIX.length} more requests on a config with methods, header conditions, wildcard, catch-all, http and other ports`);
    {
      const c = await page.evaluate((t) => window.bareproxyCheck(t), MATRIX_CONF);
      ok('matrix config checks ok in the demo', c.ok && c.problems.length === 0, JSON.stringify(c));
      const nc = native.check(MATRIX_CONF);
      ok('matrix config checks ok natively, same summary', nc.code === 0 && nc.stdout.trim().endsWith(c.summary), nc.stdout);
      let same = 0, filesOk = 0; const bad = [];
      for (const [m, u, hs = []] of MATRIX) {
        const raw = await page.evaluate(([cf, mm, uu, h]) => window.bareproxyExplain(cf, mm, uu, h), [MATRIX_CONF, m, u, hs.join('\n')]);
        const n = native.explain(MATRIX_CONF, m, u, hs);
        const cmp = compareExplain(raw, n.stdout);
        if (n.code === 0 && cmp.problems.length === 0) { cmp.kind === 'files' ? filesOk++ : same++; } else bad.push(`${m} ${u} ${hs.join(' ')}\n  ${cmp.problems.join('; ')}\n  demo:\n${indent(raw, 4)}\n  native:\n${indent(n.stdout + n.stderr, 4)}`);
        const actions = (raw.match(/^Action: .*/m) || raw.match(/^Sent upstream as .*/m) || raw.match(/^Nothing listens.*|^Port \d+ .*/m) || ['(no action line)'])[0];
        out(`  ${m.padEnd(7)} ${u}${hs.length ? '  [' + hs.join(', ') + ']' : ''}\n          ${actions}`);
      }
      ok(`${MATRIX.length} requests: demo and native agree (${same} identical, ${filesOk} files rules with the allowed difference)`, bad.length === 0, (bad.length > 3 ? `${bad.length} requests differ, the first three:\n` : '') + bad.slice(0, 3).join('\n'));
      for (const [m, u] of BAD_REQUESTS) {
        const raw = await page.evaluate(([cf, mm, uu]) => window.bareproxyExplain(cf, mm, uu, ''), [confA, m, u]);
        const n = native.explain(confA, m, u);
        ok(`bad request "${u}": same error as native`, raw.trim() === (n.stderr || n.stdout).trim(), `demo: ${raw}\nnative: ${n.stderr}${n.stdout}`);
      }
      const e = await page.evaluate(() => window.bareproxyExplain('site example.com\n  bogus\n', 'GET', 'https://example.com/', ''));
      ok('explain with a broken config says so and lists the problems', /has errors/.test(e) && /line \d+: error:/.test(e), e);
    }

    head('Explain: behaviour the design note describes');
    for (const [which, m, u, hs, want] of EXPECT) {
      const raw = await page.evaluate(([cf, mm, uu, h]) => window.bareproxyExplain(cf, mm, uu, h), [which === 'A' ? confA : MATRIX_CONF, m, u, hs.join('\n')]);
      ok(`${m} ${u}${hs.length ? ' [' + hs.join(', ') + ']' : ''}: output has "${want}"`, raw.includes(want), raw);
    }

    // ---- plan -------------------------------------------------------------------------
    head('Plan');
    {
      await page.click('#plan-btn');
      const shown = await page.$$eval('#plan-out .ln', (ls) => ls.map((l) => l.textContent.trimEnd()).join('\n'));
      const raw = await page.evaluate(([a, b]) => window.bareproxyPlan(a, b), [confA, confB]);
      out(indent(raw));
      ok('the page shows exactly what the core returned', shown === raw.replace(/\n+$/, '').split('\n').map((l) => l.trimEnd()).join('\n'));
      const nd = native.plan(confA, confB, true);
      ok('plan text is identical to MakePlan run natively on the same pair (same parse options)', nd.code === 0 && raw === nd.stdout, 'demo:\n' + raw + '\nnative:\n' + nd.stdout + nd.stderr);
      // The plan ID is a hash of the two config texts, and the folder variant has real paths in its text,
      // so its ID is different. Everything else must be identical.
      const nr = native.plan(confA, confB, false);
      const maskId = (t) => t.replace(/^Plan [0-9a-f]{12}:/m, 'Plan ID:');
      ok('plan text is identical to MakePlan on configs read from real folders and certificates (plan ID masked: it hashes the text, which has other paths)', nr.code === 0 && maskId(raw) === maskId(nr.stdout), 'demo:\n' + raw + '\nnative (folders):\n' + nr.stdout + nr.stderr);
      const planId = (raw.match(/^Plan ([0-9a-f]{12}):/m) || [])[1];
      if (planId) {
        const wantId = crypto.createHash('sha256').update(confA + '\0' + confB).digest('hex').slice(0, 12);
        ok('plan ID is the first 12 hex digits of sha256(old config, NUL, new config), worked out here with node', planId === wantId, `demo ${planId}, node ${wantId}`);
      }
      const stub = raw.trim() === 'No changes.';
      if (stub) {
        if (ALLOW_PLAN_STUB) note('plan printed "No changes." for a pair that changes things: bp.MakePlan is still the stub on this branch (merge main to get the real one)');
        else ok('plan finds the changes between the two example configs', false, 'plan printed "No changes."; is bp.MakePlan still the stub?');
      } else {
        const wantPlan = [
          'pool api, strip /api  ->  pool api-v2, strip /api/v2',
          'files /var/www/example/public  ->  files /var/www/example/releases/2026-10-05',
          'pool api: backend 10.0.0.14:8080 added',
          'pool api-v2 added',
        ];
        const missing = wantPlan.filter((w) => !raw.includes(w));
        ok('plan lists the four changes between the two example configs (new API rule, new folder, new backend, new pool)', missing.length === 0, 'missing: ' + missing.join(' | ') + '\n' + raw);
      }
      const cls = await page.$$eval('#plan-out .ln', (ls) => ls.map((l) => l.className.replace(/^ln\s*/, '')));
      const count = (k) => cls.filter((c) => c === k).length;
      ok('plan output: the heading, both section names and both change lines are highlighted', count('act') === 1 && count('sec') === 2 && count('chg') === 2, JSON.stringify(cls));

      // a second pair: a rule that can never match, so plan prints a warning
      const shadowed = confB.replace('  route /api/* -> api strip\n', '  route /api/* -> api strip\n  route /api/v1/* -> api strip\n');
      await page.fill('#cfg-b', shadowed);
      await page.click('#plan-btn');
      await sleep(350);
      const rawW = await page.evaluate(([a, b]) => window.bareproxyPlan(a, b), [confA, shadowed]);
      const nW = native.plan(confA, shadowed, true);
      ok('plan with a shadowed rule: same text as MakePlan run natively', nW.code === 0 && rawW === nW.stdout, 'demo:\n' + rawW + '\nnative:\n' + nW.stdout + nW.stderr);
      ok('plan with a shadowed rule: says the rule never matches', /Warnings\n.*never matches/.test(rawW), rawW);
      const warnRows = await page.$$eval('#plan-out .gap', (ls) => ls.map((l) => l.textContent));
      ok('plan with a shadowed rule: the warning row is highlighted in the page', warnRows.length >= 1 && /never matches/.test(warnRows[0]), JSON.stringify(warnRows));
      await page.click('#reset-b');
      await sleep(350);

      const same = await page.evaluate(([a]) => window.bareproxyPlan(a, a), [confA]);
      ok('plan of a config against itself says no changes', same.trim() === 'No changes.', same);
      const bad = await page.evaluate(([a]) => window.bareproxyPlan(a, 'site x\n  nonsense\n'), [confA]);
      ok('plan with a broken new config lists its problems', /new config has errors/.test(bad) && /line 2: error/.test(bad), bad);
    }

    // ---- speed, measured --------------------------------------------------------------
    head('Speed (measured in this browser)');
    {
      const t = await page.evaluate(([a, b]) => {
        const time = (f, n) => { const xs = []; for (let i = 0; i < n; i++) { const s = performance.now(); f(); xs.push(performance.now() - s); } xs.sort((p, q) => p - q); return { median: xs[n >> 1], max: xs[n - 1] }; };
        return {
          check: time(() => window.bareproxyCheck(a), 200),
          explain: time(() => window.bareproxyExplain(a, 'GET', 'https://example.com/api/orders', ''), 200),
          plan: time(() => window.bareproxyPlan(a, b), 50),
        };
      }, [confA, confB]);
      for (const k of Object.keys(t)) out(`  ${k.padEnd(8)} median ${t[k].median.toFixed(2)} ms, slowest ${t[k].max.toFixed(2)} ms`);
      ok('every call is under 100 ms in the median', Object.values(t).every((x) => x.median < 100));
    }

    // ---- layout and screenshots -------------------------------------------------------
    head('Layout and screenshots');
    const overflow = (p) => p.evaluate(() => ({ sw: document.documentElement.scrollWidth, cw: document.documentElement.clientWidth, bw: document.body.scrollWidth }));
    await page.click('#presets .chip[data-url="https://example.com/old-page/"]');
    await page.evaluate(() => window.scrollTo(0, 0));
    let o = await overflow(page);
    ok('1280 px: no horizontal page scroll', o.sw <= o.cw, JSON.stringify(o));
    await page.screenshot({ path: path.join(SHOTS, 'demo-1280.png'), fullPage: true });
    out('screenshot: ' + path.join(SHOTS, 'demo-1280.png'));

    for (const [w, h, name, dsf] of [[375, 812, 'demo-375.png', 2], [320, 640, 'demo-320.png', 2]]) {
      const c2 = await browser.newContext({ viewport: { width: w, height: h }, deviceScaleFactor: dsf, isMobile: true, hasTouch: true });
      hookRequests(c2);
      const p2 = await c2.newPage(); hookPage(p2, 'w' + w);
      await p2.goto(base + '/index.html');
      await p2.waitForSelector('#status[data-state="ready"]', { timeout: 120000 });
      await p2.click('#presets .chip[data-url="https://example.com/api/orders"]');
      o = await overflow(p2);
      ok(`${w} px: no horizontal page scroll`, o.sw <= o.cw, JSON.stringify(o));
      const small = await p2.$$eval('button, .chip, select, input[type=text]', (els) => els.filter((e) => e.getBoundingClientRect().height < 32 && e.offsetParent).map((e) => e.className + ':' + Math.round(e.getBoundingClientRect().height)));
      ok(`${w} px: every button, chip and field is at least 32 px tall`, small.length === 0, small.join(', '));
      const fs16 = await p2.$$eval('input[type=text], select, textarea', (els) => els.filter((e) => parseFloat(getComputedStyle(e).fontSize) < 16 && e.offsetParent).map((e) => e.id + ':' + getComputedStyle(e).fontSize));
      if (fs16.length) note(`${w} px: fields below 16 px font (iOS zooms into those on focus): ${fs16.join(', ')}`);
      await p2.screenshot({ path: path.join(SHOTS, name), fullPage: true });
      out('screenshot: ' + path.join(SHOTS, name));
      await c2.close();
    }

    {
      const c3 = await browser.newContext({ viewport: { width: 800, height: 900 } });
      hookRequests(c3);
      const p3 = await c3.newPage(); hookPage(p3, 'iframe');
      await p3.goto(base + '/_frame.html');
      const frame = p3.frameLocator('#f');
      await frame.locator('#status[data-state="ready"]').waitFor({ timeout: 120000 });
      await frame.locator('#presets .chip').nth(2).click(); // the old-page request, which has the longest output
      const f = p3.frames().find((x) => x.url().endsWith('/live-demo/'));
      ok('the demo loads inside an iframe whose src is /live-demo/ (the way the site embeds it)', !!f);
      o = await f.evaluate(() => ({ sw: document.documentElement.scrollWidth, cw: document.documentElement.clientWidth, h: document.documentElement.scrollHeight }));
      ok('760 px iframe: no horizontal scroll inside the frame', o.sw <= o.cw, JSON.stringify(o));
      await p3.setViewportSize({ width: 800, height: 1160 });
      await p3.screenshot({ path: path.join(SHOTS, 'demo-iframe-760-h1100.png') });
      out('screenshot: ' + path.join(SHOTS, 'demo-iframe-760-h1100.png') + ' (the frame as the site embeds it: 760 x 1100 px, page scrolls inside)');
      out(`  the demo is ${o.h} px tall at 760 px wide, so a 1100 px frame scrolls inside`);
      const ends = await f.evaluate(() => [...document.querySelectorAll('.panel')].map((pn) => Math.round(pn.getBoundingClientRect().bottom + window.scrollY)));
      out(`  at 760 px wide the three panels end ${ends.join(', ')} px from the top, so a frame about ${ends[1] + 16} px tall shows the config and the whole explain panel without scrolling inside`);
      await p3.evaluate((h) => { document.getElementById('f').style.height = h + 'px'; }, o.h);
      await p3.screenshot({ path: path.join(SHOTS, 'demo-iframe-760.png'), fullPage: true });
      out('screenshot: ' + path.join(SHOTS, 'demo-iframe-760.png') + ` (frame content ${o.h} px tall)`);
      await c3.close();
    }

    // ---- served under /live-demo/ -------------------------------------------------------
    head('Served under /live-demo/ (the path the site uses)');
    {
      const c4 = await browser.newContext({ viewport: { width: 1280, height: 900 } });
      hookRequests(c4);
      const p4 = await c4.newPage(); hookPage(p4, 'live-demo');
      await p4.goto(base + '/live-demo/');
      let ready = true;
      try { await p4.waitForSelector('#status[data-state="ready"]', { timeout: 120000 }); } catch { ready = false; }
      ok('https://.../live-demo/ opens the page and the core starts (relative links work from that folder)', ready);
      await p4.click('#presets .chip[data-url="https://example.com/api/orders"]');
      const txt = await p4.textContent('#explain-out');
      ok('and explain answers there', /Sent upstream as GET \/orders/.test(txt), txt);
      const lp = seen.filter((x) => x.path.startsWith('/live-demo/')).map((x) => `${x.path} ${x.status}`);
      out('  requests under /live-demo/: ' + [...new Set(lp)].join(', '));
      ok('every file under /live-demo/ was found', lp.length >= 5 && seen.filter((x) => x.path.startsWith('/live-demo/') && x.status !== 200).length === 0);
      await c4.close();
    }

    // ---- network ------------------------------------------------------------------------
    head('Network: nothing is sent anywhere');
    const methods = [...new Set(seen.map((s) => s.method))];
    out(`  requests the local server saw: ${seen.length}, methods: ${methods.join(', ')}`);
    const byPath = {}; for (const s of seen) byPath[s.path] = (byPath[s.path] || 0) + 1;
    out('  paths: ' + Object.entries(byPath).map(([k, v]) => `${k} x${v}`).join(', '));
    ok('the page only makes GET requests', methods.every((m) => m === 'GET'), methods.join(','));
    ok('every request goes to the local server (none to any other host)', outside.length === 0, outside.join('\n'));
    const pageFiles = new Set(['/index.html', '/demo.css', '/demo.js', '/wasm_exec.js', '/bareproxy.wasm', '/_frame.html', '/favicon.ico']);
    ok('the only files requested are the five that were built (plus the check\'s own frame page)', Object.keys(byPath).every((p) => pageFiles.has(p.replace(/^\/live-demo\//, '/')) || p === '/live-demo/'), Object.keys(byPath).join(', '));
    ok('no request failed except the ones this check forced', seen.filter((s) => s.status >= 400 && s.path !== '/favicon.ico').length === 0, JSON.stringify(seen.filter((s) => s.status >= 400)));
    const real = consoleProblems.filter((m) => !m.startsWith('wrong-type'));
    ok('no console errors or warnings (apart from the forced wrong-content-type case)', real.length === 0 || real.every((m) => /Failed to load resource.*(404|gone)/.test(m)), real.join('\n'));
    if (real.length) note('console messages: ' + real.join(' | '));
  } finally {
    await browser.close();
    server.close();
    try { fs.rmSync(fx.tmp, { recursive: true, force: true }); } catch { /* leave it */ }
  }

  head('Summary');
  out(`${passes} passed, ${fails} failed, in ${((Date.now() - t0) / 1000).toFixed(0)} s`);
  fs.mkdirSync(path.dirname(LOGFILE), { recursive: true });
  fs.writeFileSync(LOGFILE, lines.join('\n') + '\n');
  console.log('log written to ' + LOGFILE);
  process.exit(fails ? 1 : 0);
}

main().catch((e) => {
  out('FAIL  check crashed: ' + (e && e.stack ? e.stack : e));
  try { fs.mkdirSync(path.dirname(LOGFILE), { recursive: true }); fs.writeFileSync(LOGFILE, lines.join('\n') + '\n'); } catch { /* ignore */ }
  process.exit(1);
});
