// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

// BareProxy live demo. The Go side (bareproxy.wasm) registers three functions:
//   bareproxyCheck(config)                       -> {ok, problems: [{line, msg, warn}], summary}
//   bareproxyExplain(config, method, url, hdrs)  -> text
//   bareproxyPlan(oldConfig, newConfig)          -> text
// Everything here is plain JavaScript with no libraries. Text from the core and
// from the visitor is only ever put on the page with textContent.

(() => {
  'use strict';

  const WASM_URL = 'bareproxy.wasm';
  const CONF = 'bareproxy.conf'; // the file name the core prints, as in the Go side
  const LINE_PX = 20;            // editor line height, in step with --line-px in demo.css
  const MAX_LINES = 28;          // editors grow to this many lines, then scroll
  const DELAY_MS = 200;          // wait after typing before the results are redrawn

  const $ = (id) => document.getElementById(id);
  const el = {
    status: $('status'), statusText: $('status-text'), version: $('version'),
    cfgA: $('cfg-a'), gutA: $('gut-a'), resetA: $('reset-a'), checkBtn: $('check-btn'),
    checkChip: $('check-chip'), checkSummary: $('check-summary'), checkList: $('check-list'),
    method: $('method'), url: $('url'), headers: $('headers'), request: $('request'),
    explainBtn: $('explain-btn'), explainCmd: $('explain-cmd'), explainOut: $('explain-out'),
    presets: $('presets'),
    cfgB: $('cfg-b'), gutB: $('gut-b'), resetB: $('reset-b'), planBtn: $('plan-btn'),
    planChip: $('plan-chip'), planOut: $('plan-out'),
  };

  let ready = false;

  // ---- editors -----------------------------------------------------------

  // An editor is a textarea with a column of line numbers beside it. The
  // textarea grows with its text up to MAX_LINES; past that both scroll together.
  function makeEditor(ta, gut) {
    const ed = { ta, gut, lines: 0, marks: new Map(), picked: 0 };
    ed.draw = () => {
      const n = ta.value.split('\n').length;
      ta.style.height = (Math.min(n, MAX_LINES) * LINE_PX + 36) + 'px';
      const frag = document.createDocumentFragment();
      for (let i = 1; i <= n; i++) {
        const d = document.createElement('div');
        const kind = ed.marks.get(i);
        if (kind) d.className = kind;
        if (i === ed.picked) d.classList.add('pick');
        d.textContent = String(i);
        frag.appendChild(d);
      }
      gut.replaceChildren(frag);
      gut.scrollTop = ta.scrollTop;
      ed.lines = n;
    };
    ed.mark = (map) => { ed.marks = map; ed.picked = 0; ed.draw(); };
    ed.jump = (line) => {
      const rows = ta.value.split('\n');
      if (line < 1 || line > rows.length) return;
      let start = 0;
      for (let i = 0; i < line - 1; i++) start += rows[i].length + 1;
      ta.focus({ preventScroll: true });
      ta.setSelectionRange(start, start + rows[line - 1].length);
      const first = (line - 1) * LINE_PX;
      if (first < ta.scrollTop || first > ta.scrollTop + ta.clientHeight - 2 * LINE_PX) {
        ta.scrollTop = Math.max(0, first - 3 * LINE_PX);
      }
      ed.picked = line;
      ed.draw();
      const row = gut.children[line - 1];
      if (row && row.scrollIntoView) row.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
    };
    ta.addEventListener('scroll', () => { gut.scrollTop = ta.scrollTop; });
    ed.draw();
    return ed;
  }

  const edA = makeEditor(el.cfgA, el.gutA);
  const edB = makeEditor(el.cfgB, el.gutB);

  // ---- output ------------------------------------------------------------

  // Lines of core output get a class, for display only: the winning rule, the
  // rules that did not match, the action, and the demo's "not looked up" note.
  function classify(line) {
    if (/^ {2}line \d+ .* {3}match$/.test(line)) return 'hit';
    if (/^ {2}line \d+ .* {3}no: /.test(line)) return 'miss';
    if (/^Action:/.test(line)) return 'act';
    if (/^Not looked up:/.test(line)) return 'gap';
    if (/^Config /.test(line)) return 'miss';
    if (/^bareproxy: |: error: |^The .*config.* has errors/.test(line)) return 'bad';
    return '';
  }

  // The same for plan output: the heading, the section names, each change (old
  // and new on one line), and the warnings. A fresh one is made for each print
  // because it remembers which section it is in.
  function planClassifier() {
    let section = '';
    return (line) => {
      if (/^Plan [0-9a-f]{12}:/.test(line)) return 'act';
      if (/^(Routing|Other changes|Warnings)$/.test(line)) { section = line; return 'sec'; }
      if (section === 'Routing' && /^ {4,}\S.* {2}-> {2}/.test(line)) return 'chg';
      if (section === 'Warnings' && /^ {2}\S/.test(line)) return 'gap';
      if (/^The .*config.* has errors|^bareproxy: |: error: /.test(line)) return 'bad';
      return '';
    };
  }

  function showText(pre, text, classifier = classify) {
    const frag = document.createDocumentFragment();
    for (const line of String(text).replace(/\n+$/, '').split('\n')) {
      const row = document.createElement('span');
      row.className = 'ln ' + classifier(line);
      row.textContent = line === '' ? ' ' : line;
      frag.appendChild(row);
    }
    pre.replaceChildren(frag);
  }

  function plural(n, word) { return n + ' ' + word + (n === 1 ? '' : 's'); }

  function setChip(chip, kind, text) {
    chip.dataset.kind = kind;
    chip.textContent = text;
  }

  // ---- calls into the core -----------------------------------------------

  function safely(label, fn, fallback) {
    try { return fn(); } catch (e) {
      return fallback(label + ' failed: ' + (e && e.message ? e.message : e));
    }
  }

  function check(text) {
    return safely('Check', () => window.bareproxyCheck(text), (msg) => (
      { ok: false, problems: [{ line: 0, msg, warn: false }], summary: 'has errors, so it can\'t be used' }));
  }

  function markMap(problems) {
    const map = new Map();
    for (const p of problems) {
      if (!p.line) continue;
      if (p.warn && map.get(p.line) === 'err') continue;
      map.set(p.line, p.warn ? 'warn' : 'err');
    }
    return map;
  }

  function runCheck() {
    if (!ready) return;
    const res = check(el.cfgA.value);
    const probs = res.problems || [];
    const errs = probs.filter((p) => !p.warn).length;
    const warns = probs.length - errs;
    edA.mark(markMap(probs));

    el.checkSummary.textContent = CONF + ': ' + (res.ok ? 'ok, ' : '') + res.summary;
    el.checkSummary.dataset.kind = res.ok ? 'ok' : 'err';
    const items = document.createDocumentFragment();
    for (const p of probs) {
      const li = document.createElement('li');
      li.className = p.warn ? 'warn' : 'err';
      if (p.line) {
        const b = document.createElement('button');
        b.type = 'button';
        b.className = 'lineref';
        b.dataset.line = String(p.line);
        b.textContent = 'line ' + p.line;
        li.appendChild(b);
      }
      const kind = document.createElement('span');
      kind.className = 'kind';
      kind.textContent = p.warn ? 'warning' : 'error';
      const msg = document.createElement('span');
      msg.className = 'msg';
      msg.textContent = p.msg;
      li.append(kind, msg);
      items.appendChild(li);
    }
    el.checkList.replaceChildren(items);

    if (errs) setChip(el.checkChip, 'err', plural(errs, 'error') + (warns ? ', ' + plural(warns, 'warning') : ''));
    else if (warns) setChip(el.checkChip, 'warn', 'ok, ' + plural(warns, 'warning'));
    else setChip(el.checkChip, 'ok', 'ok, no problems');
  }

  function headerLines() {
    return el.headers.value.split('\n').map((s) => s.trim()).filter(Boolean);
  }

  function runExplain() {
    if (!ready) return;
    const method = el.method.value;
    const url = el.url.value.trim();
    const hs = headerLines();
    let cmd = '$ bareproxy explain --offline --config ' + CONF;
    for (const h of hs) cmd += ' -H "' + h.replace(/"/g, '\\"') + '"';
    cmd += ' ' + method + ' ' + (url || '""');
    el.explainCmd.textContent = cmd;
    const out = safely('Explain', () => window.bareproxyExplain(el.cfgA.value, method, url, hs.join('\n')),
      (msg) => 'bareproxy: ' + msg);
    showText(el.explainOut, out);
    markPreset();
  }

  function runPlan() {
    if (!ready) return;
    const res = check(el.cfgB.value);
    edB.mark(markMap(res.problems || []));
    const out = safely('Plan', () => window.bareproxyPlan(el.cfgA.value, el.cfgB.value),
      (msg) => 'bareproxy: ' + msg);
    showText(el.planOut, out, planClassifier());
    if (!res.ok) setChip(el.planChip, 'err', 'new config has errors');
    else if ((res.problems || []).length) setChip(el.planChip, 'warn', 'new config: ' + plural(res.problems.length, 'warning'));
    else setChip(el.planChip, 'none', '');
  }

  // ---- wiring ------------------------------------------------------------

  function debounce(fn, ms) {
    let t = 0;
    return () => { clearTimeout(t); t = setTimeout(fn, ms); };
  }

  const afterConfigA = debounce(() => { runCheck(); runExplain(); runPlan(); }, DELAY_MS);
  const afterConfigB = debounce(runPlan, DELAY_MS);
  const afterRequest = debounce(runExplain, DELAY_MS);

  el.cfgA.addEventListener('input', () => { edA.picked = 0; edA.draw(); afterConfigA(); });
  el.cfgB.addEventListener('input', () => { edB.picked = 0; edB.draw(); afterConfigB(); });
  el.url.addEventListener('input', afterRequest);
  el.method.addEventListener('change', runExplain);
  el.headers.addEventListener('input', afterRequest);

  el.checkBtn.addEventListener('click', runCheck);
  el.planBtn.addEventListener('click', runPlan);
  el.request.addEventListener('submit', (e) => { e.preventDefault(); runExplain(); });

  el.resetA.addEventListener('click', () => {
    el.cfgA.value = el.cfgA.defaultValue; edA.picked = 0; edA.draw(); runCheck(); runExplain(); runPlan();
  });
  el.resetB.addEventListener('click', () => {
    el.cfgB.value = el.cfgB.defaultValue; edB.picked = 0; edB.draw(); runPlan();
  });

  el.checkList.addEventListener('click', (e) => {
    const b = e.target.closest('.lineref');
    if (b) edA.jump(Number(b.dataset.line));
  });

  el.presets.addEventListener('click', (e) => {
    const b = e.target.closest('.chip');
    if (!b) return;
    el.method.value = b.dataset.method;
    el.url.value = b.dataset.url;
    runExplain();
  });

  function markPreset() {
    const url = el.url.value.trim();
    for (const b of el.presets.querySelectorAll('.chip')) {
      b.classList.toggle('on', b.dataset.method === el.method.value && b.dataset.url === url);
    }
  }

  // ---- loading the core --------------------------------------------------

  function setStatus(state, text) {
    el.status.dataset.state = state;
    el.statusText.textContent = text;
  }

  function fail(text) {
    setStatus('error', text);
    const msg = 'The core is not running, so there is no output.';
    showText(el.explainOut, msg);
    showText(el.planOut, msg);
  }

  async function instantiate(go) {
    if (WebAssembly.instantiateStreaming) {
      try {
        return await WebAssembly.instantiateStreaming(fetch(WASM_URL), go.importObject);
      } catch (e) {
        // A host that serves .wasm with the wrong content type refuses streaming.
        // Read the bytes and compile them instead.
      }
    }
    const resp = await fetch(WASM_URL);
    if (!resp.ok) throw new Error('could not load ' + WASM_URL + ' (HTTP ' + resp.status + ')');
    return WebAssembly.instantiate(await resp.arrayBuffer(), go.importObject);
  }

  async function boot() {
    if (typeof WebAssembly !== 'object') {
      fail('This browser has no WebAssembly, so the demo cannot run.');
      return;
    }
    if (typeof Go !== 'function') {
      fail('Could not load wasm_exec.js, so the demo cannot start.');
      return;
    }
    const t0 = performance.now();
    const go = new Go();
    let result;
    try {
      result = await instantiate(go);
    } catch (e) {
      fail('Could not load the core: ' + (e && e.message ? e.message : e));
      return;
    }
    go.run(result.instance).then(() => {
      ready = false;
      el.checkBtn.disabled = el.explainBtn.disabled = el.planBtn.disabled = true;
      fail('The core has stopped. Reload the page to start it again.');
    });
    for (let i = 0; i < 100 && typeof window.bareproxyCheck !== 'function'; i++) {
      await new Promise((r) => setTimeout(r, 50));
    }
    if (typeof window.bareproxyCheck !== 'function') {
      fail('The core started but did not register its functions.');
      return;
    }
    ready = true;
    const secs = ((performance.now() - t0) / 1000).toFixed(1);
    const ver = window.bareproxyVersion ? String(window.bareproxyVersion) : '';
    setStatus('ready', 'Ready' + (ver ? '. BareProxy ' + ver : '') + ', loaded in ' + secs + ' s.');
    if (ver) el.version.textContent = 'BareProxy ' + ver;
    el.checkBtn.disabled = el.explainBtn.disabled = el.planBtn.disabled = false;
    runCheck();
    runExplain();
    runPlan();
  }

  boot();
})();
