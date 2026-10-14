// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! BareProxy's bot challenges plugin. A client that says it's a browser
//! and has no pass gets a page with a small proof-of-work puzzle instead of
//! the site. The browser solves it in a moment and sends the answer back;
//! the plugin checks it and gives a pass, a signed cookie, for a time the
//! config sets. Nothing is kept per visitor and nothing leaves BareProxy.
//! README.md is the manual.

pub mod config;

use config::{Answer, Config, Who};
use kit::{Index, Net};
use proxy_wasm::hostcalls;
use proxy_wasm::traits::*;
use proxy_wasm::types::*;
use std::cell::RefCell;
use std::rc::Rc;
use std::time::{Duration, UNIX_EPOCH};

/// How often the files always lines name are checked for changes.
const FILES_EVERY: u64 = 30;

proxy_wasm::main! {{
    proxy_wasm::set_log_level(LogLevel::Warn);
    proxy_wasm::set_root_context(|_| -> Box<dyn RootContext> { Box::new(Root { st: None }) });
}}

/// Random bytes from the host's WASI, for a key of the plugin's own.
#[cfg(target_arch = "wasm32")]
fn random(buf: &mut [u8]) -> bool {
    #[link(wasm_import_module = "wasi_snapshot_preview1")]
    unsafe extern "C" {
        fn random_get(buf: *mut u8, len: usize) -> i32;
    }
    unsafe { random_get(buf.as_mut_ptr(), buf.len()) == 0 }
}

#[cfg(not(target_arch = "wasm32"))]
fn random(_: &mut [u8]) -> bool {
    false
}

#[derive(Clone, Copy, Default)]
struct Metrics {
    challenged: Option<u32>,
    passed: Option<u32>,
    failed: Option<u32>,
}

/// A file always lines name: its bytes as last read, and the last ones
/// refused.
struct File {
    name: String,
    raw: Vec<u8>,
    refused: Option<Vec<u8>>,
    nets: Vec<Net>,
}

struct State {
    cfg: Rc<Config>,
    key: Rc<Vec<u8>>,
    always: Rc<Index>,
    files: Vec<File>,
    metrics: Metrics,
    /// A line for the log on the first tick, which check and plan never
    /// reach: why visitors will solve the puzzle again after a restart.
    warn: Option<String>,
}

impl State {
    fn index(&self) -> Index {
        let mut all = self.cfg.always.clone();
        for f in &self.files {
            all.extend_from_slice(&f.nets);
        }
        Index::new(&all)
    }
}

fn secs(ctx: &dyn Context) -> u64 {
    ctx.get_current_time()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

fn load(name: &str) -> Result<(Vec<u8>, Vec<Net>), String> {
    let raw = kit::read_file(name)?;
    let text = std::str::from_utf8(&raw).map_err(|_| format!("{name} isn't text"))?;
    let nets = kit::parse_list(text).map_err(|e| format!("{name} {e}"))?;
    Ok((raw, nets))
}

struct Root {
    st: Option<Rc<RefCell<State>>>,
}

impl Root {
    /// The key passes and puzzles are made with: the config's, or one the
    /// plugin made, shared by its instances and kept in its store.
    fn key(&self, cfg: &Config) -> Result<(Vec<u8>, Option<String>), String> {
        if let Some(k) = &cfg.key {
            return Ok((k.clone(), None));
        }
        if let Some(k) = self.get_shared_data("key").0 {
            return Ok((k, None));
        }
        if let Some(k) = kit::store_get("key").filter(|k| k.len() == 32) {
            let _ = self.set_shared_data("key", Some(&k), None);
            return Ok((k, None));
        }
        let mut k = vec![0u8; 32];
        if !random(&mut k) {
            return Err(
                "can't make a key: the host gave no random numbers; write one with a key line"
                    .into(),
            );
        }
        let warn = (!kit::store_put("key", &k)).then(|| {
            "no store on the plugin line, so the key the plugin made lasts until BareProxy restarts, and visitors solve the puzzle again after that; add store 1MB, or write a key line".to_string()
        });
        let _ = self.set_shared_data("key", Some(&k), None);
        Ok((k, warn))
    }
}

impl Context for Root {}

impl RootContext for Root {
    fn on_configure(&mut self, _: usize) -> bool {
        let text = self.get_plugin_configuration().unwrap_or_default();
        let made = Config::parse(&String::from_utf8_lossy(&text)).and_then(|cfg| {
            let mut files = vec![];
            for name in &cfg.always_files {
                let (raw, nets) = load(name)?;
                files.push(File {
                    name: name.clone(),
                    raw,
                    refused: None,
                    nets,
                });
            }
            let (key, warn) = self.key(&cfg)?;
            Ok((cfg, files, key, warn))
        });
        let (cfg, files, key, warn) = match made {
            Ok(x) => x,
            Err(e) => {
                kit::note(&e);
                return false;
            }
        };
        let mut st = State {
            cfg: Rc::new(cfg),
            key: Rc::new(key),
            always: Rc::new(Index::default()),
            files,
            metrics: Metrics {
                challenged: hostcalls::define_metric(MetricType::Counter, "challenged").ok(),
                passed: hostcalls::define_metric(MetricType::Counter, "passed").ok(),
                failed: hostcalls::define_metric(MetricType::Counter, "failed").ok(),
            },
            warn,
        };
        st.always = Rc::new(st.index());
        if !st.files.is_empty() || st.warn.is_some() {
            self.set_tick_period(Duration::from_secs(FILES_EVERY));
        }
        self.st = Some(Rc::new(RefCell::new(st)));
        true
    }

    fn on_tick(&mut self) {
        let Some(st) = self.st.clone() else { return };
        let mut s = st.borrow_mut();
        if let Some(w) = s.warn.take() {
            kit::note(&w);
        }
        let mut changed = false;
        for f in &mut s.files {
            match load(&f.name) {
                Ok((raw, _)) if raw == f.raw => {}
                Ok((raw, nets)) => {
                    kit::note(&format!(
                        "reloaded {}: {} address ranges",
                        f.name,
                        nets.len()
                    ));
                    (f.raw, f.nets, f.refused) = (raw, nets, None);
                    changed = true;
                }
                Err(e) => {
                    let mark = kit::read_file(&f.name).unwrap_or_default();
                    if f.refused.as_ref() != Some(&mark) {
                        kit::note(&format!("{e}; the list as it was stays"));
                        f.refused = Some(mark);
                    }
                }
            }
        }
        if changed {
            s.always = Rc::new(s.index());
        }
    }

    fn create_http_context(&self, _: u32) -> Option<Box<dyn HttpContext>> {
        Some(Box::new(Http {
            st: self.st.clone(),
        }))
    }

    fn get_type(&self) -> Option<ContextType> {
        Some(ContextType::HttpContext)
    }
}

struct Http {
    st: Option<Rc<RefCell<State>>>,
}

impl Context for Http {}

fn bump(m: Option<u32>) {
    if let Some(m) = m {
        let _ = hostcalls::increment_metric(m, 1);
    }
}

impl Http {
    /// Checks an answer sent to the endpoint, and gives a pass for a right
    /// one.
    fn answer(&self, cfg: &Config, key: &[u8], metrics: Metrics, bind: &str, now: u64) -> Action {
        let query = kit::property(self, &["request", "query"]);
        match cfg.verify(key, &query, bind, now) {
            Answer::Pass { exp, value, to } => {
                let https = kit::property(self, &["request", "scheme"]) == "https";
                let cookie = cfg.set_cookie(&value, https);
                kit::note(&format!(
                    "challenge passed: work {}, pass until {}",
                    cfg.work,
                    kit::utc(exp)
                ));
                bump(metrics.passed);
                self.send_http_response(
                    302,
                    vec![
                        ("location", to.as_str()),
                        ("set-cookie", cookie.as_str()),
                        ("cache-control", "no-store"),
                    ],
                    None,
                );
            }
            Answer::Fail(why, to) => {
                kit::note(&format!("challenge failed: {why}"));
                bump(metrics.failed);
                let body = format!(
                    "<!doctype html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n<meta name=\"robots\" content=\"noindex, nofollow\">\n<title>Try again</title>\n<style>body{{font:18px/1.5 system-ui,sans-serif;max-width:32em;margin:15vh auto;padding:0 1em}}</style>\n</head>\n<body>\n<h1>Try again</h1>\n<p>The check didn't work this time. <a href=\"{}\">Go back to the page</a> for a new puzzle.</p>\n</body>\n</html>\n",
                    config::html_escape(&to)
                );
                self.send_http_response(
                    403,
                    vec![
                        ("content-type", "text/html; charset=utf-8"),
                        ("cache-control", "no-store"),
                    ],
                    Some(body.as_bytes()),
                );
            }
        }
        Action::Pause
    }
}

impl HttpContext for Http {
    fn on_http_request_headers(&mut self, _: usize, _: bool) -> Action {
        let Some(st) = self.st.clone() else {
            return Action::Continue;
        };
        let (cfg, key, always, metrics) = {
            let s = st.borrow();
            (s.cfg.clone(), s.key.clone(), s.always.clone(), s.metrics)
        };
        let headers = self.get_http_request_headers();
        let get = |n: &str| {
            headers
                .iter()
                .find(|(k, _)| k == n)
                .map(|(_, v)| v.as_str())
        };
        let path = kit::property(self, &["request", "url_path"]);
        let method = get(":method").unwrap_or("");
        let ua = get("user-agent").unwrap_or("");
        let source = kit::property(self, &["source", "address"]);
        let ip = kit::client_addr(&source, get("x-forwarded-for"), &cfg.forwarded_from);
        let now = secs(self);
        if path == cfg.endpoint {
            if method != "GET" {
                self.send_http_response(405, vec![("allow", "GET")], None);
                return Action::Pause;
            }
            return self.answer(&cfg, &key, metrics, &cfg.bind_text(ip, ua), now);
        }
        if method != "GET" && method != "HEAD" || cfg.skipped(&path) {
            return Action::Continue;
        }
        if cfg.who == Who::Browsers && !config::looks_like_browser(ua) {
            return Action::Continue;
        }
        if ip.is_some_and(|ip| cfg.allow.iter().any(|n| n.contains(ip))) {
            return Action::Continue;
        }
        if let Some((name, value)) = &cfg.trust_header
            && get(name) == Some(value.as_str())
        {
            kit::note(&format!("no challenge: {name} is {value} (trust-header)"));
            return Action::Continue;
        }
        let bind = cfg.bind_text(ip, ua);
        if let Some(c) = get("cookie").and_then(|h| config::cookie_value(h, &cfg.cookie))
            && config::pass_ok(&key, c, &bind, now).is_some()
        {
            return Action::Continue;
        }
        let addr = ip.map(|i| i.to_string()).unwrap_or(source);
        let reason = if ip.is_some_and(|ip| always.contains(ip)) {
            format!("{addr} is on the always list")
        } else if let Some(a) = cfg.after {
            let nav = config::is_navigation(get("sec-fetch-mode"), get("accept"));
            let client = ip
                .map(|ip| config::network(ip, 32, 64))
                .unwrap_or(addr.clone());
            let Some((cur, prev, into)) =
                kit::count(&format!("a|{client}"), a.window_ms, nav as u32)
            else {
                return Action::Continue; // BareProxy holds too many counters
            };
            let n = kit::sliding(cur, prev, into, a.window_ms);
            if n <= a.count {
                return Action::Continue;
            }
            format!(
                "{n} page loads in the last {} from {client} without a pass, over {} (line {})",
                kit::short(a.window_ms as u64),
                a.count,
                a.line
            )
        } else if cfg.who == Who::Browsers {
            "a browser with no pass".to_string()
        } else {
            "no pass (challenge all)".to_string()
        };
        let target = get(":path").unwrap_or("/");
        let page = cfg.page(&config::puzzle(&key, now, cfg.work, &bind), now, target);
        kit::note(&format!("challenged: {reason}; work {}", cfg.work));
        bump(metrics.challenged);
        self.send_http_response(
            cfg.status,
            vec![
                ("content-type", "text/html; charset=utf-8"),
                ("cache-control", "no-store"),
                ("x-robots-tag", "noindex"),
            ],
            Some(page.as_bytes()),
        );
        Action::Pause
    }
}
