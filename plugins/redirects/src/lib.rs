// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! BareProxy's redirects-from-a-file plugin: thousands of redirects from one
//! plain file in a folder the plugin may read, looked up in a table, reloaded
//! when the file changes (a broken file is refused and the old list stays),
//! and counted. README.md is the manual.

pub mod table;

use proxy_wasm::hostcalls;
use proxy_wasm::traits::*;
use proxy_wasm::types::*;
use std::cell::RefCell;
use std::collections::HashMap;
use std::rc::Rc;
use std::time::{Duration, UNIX_EPOCH};
use table::{Config, Table};

/// The largest hit list an instance shares with the others.
const MAX_HITS: usize = 1 << 20;

/// How often an instance shares its hits, and so how far the report can
/// lag behind. The file is checked every `reload` seconds, on these ticks.
const SHARE_EVERY: u64 = 5;

proxy_wasm::main! {{
    proxy_wasm::set_log_level(LogLevel::Warn);
    proxy_wasm::set_root_context(|_| -> Box<dyn RootContext> { Box::new(Root { st: None }) });
}}

/// What an instance's root and its requests share.
struct State {
    cfg: Config,
    table: Rc<Table>,
    /// The file as last loaded, and the last one refused.
    raw: Vec<u8>,
    refused: Option<Vec<u8>>,
    loaded: String,
    /// This instance's hits, by the line's old path, and whether they
    /// changed since they were last shared.
    hits: HashMap<String, u64>,
    dirty: bool,
    /// When the file was last checked, in seconds since 1970.
    checked: u64,
    id: u64,
    metric: Option<u32>,
}

struct Root {
    st: Option<Rc<RefCell<State>>>,
}

/// Reads the file through bareproxy_read_file.
fn read(file: &str) -> Result<Vec<u8>, String> {
    match hostcalls::call_foreign_function("bareproxy_read_file", Some(file.as_bytes())) {
        Ok(b) => Ok(b.unwrap_or_default()),
        Err(Status::NotFound) => Err(format!(
            "can't read {file}: it isn't in a folder the plugin line names with read"
        )),
        Err(Status::BadArgument) => Err(format!(
            "can't read {file}: it's larger than the plugin's body-limit"
        )),
        Err(st) => Err(format!("can't read {file}: {st:?}")),
    }
}

/// The file as text. A plain from_utf8 check, which is fast for the ASCII
/// paths a redirect list is mostly made of.
fn utf8(file: &str, raw: &[u8]) -> Result<String, String> {
    String::from_utf8(raw.to_vec()).map_err(|e| {
        format!(
            "{file} isn't UTF-8 text (at byte {})",
            e.utf8_error().valid_up_to()
        )
    })
}

fn secs(ctx: &dyn Context) -> u64 {
    ctx.get_current_time()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

fn now(ctx: &dyn Context) -> String {
    table::utc(secs(ctx))
}

/// Adds one to a shared-data counter and returns the new value.
fn count(ctx: &dyn Context, key: &str) -> u64 {
    loop {
        let (val, cas) = ctx.get_shared_data(key);
        let n = val
            .and_then(|v| String::from_utf8(v).ok())
            .and_then(|s| s.parse::<u64>().ok())
            .unwrap_or(0)
            + 1;
        if ctx
            .set_shared_data(key, Some(n.to_string().as_bytes()), cas)
            .is_ok()
        {
            return n;
        }
    }
}

impl Context for Root {}

impl RootContext for Root {
    fn on_configure(&mut self, _: usize) -> bool {
        let text = self.get_plugin_configuration().unwrap_or_default();
        let loaded = Config::parse(&String::from_utf8_lossy(&text)).and_then(|cfg| {
            let raw = read(&cfg.file)?;
            let t = Table::parse(utf8(&cfg.file, &raw)?, &cfg)
                .map_err(|e| format!("{} {e}", cfg.file))?;
            Ok((cfg, t, raw))
        });
        let (cfg, table, raw) = match loaded {
            Ok(x) => x,
            Err(e) => {
                kit::note(&e);
                return false;
            }
        };
        self.set_tick_period(Duration::from_secs(SHARE_EVERY.min(cfg.reload.max(1))));
        let id = count(self, "instances");
        let metric = hostcalls::define_metric(MetricType::Counter, "redirects").ok();
        self.st = Some(Rc::new(RefCell::new(State {
            cfg,
            table: Rc::new(table),
            raw,
            refused: None,
            loaded: now(self),
            checked: secs(self),
            hits: HashMap::new(),
            dirty: false,
            id,
            metric,
        })));
        true
    }

    fn on_tick(&mut self) {
        let Some(st) = self.st.clone() else { return };
        let mut s = st.borrow_mut();
        let t = secs(self);
        if s.cfg.reload > 0 && t >= s.checked + s.cfg.reload {
            s.checked = t;
            let file = s.cfg.file.clone();
            match read(&file) {
                Ok(raw) if raw == s.raw => {}
                Ok(raw) => match utf8(&file, &raw).and_then(|text| Table::parse(text, &s.cfg)) {
                    Ok(t) => {
                        kit::note(&format!("reloaded {file}: {} lines", t.entries.len()));
                        s.table = Rc::new(t);
                        s.raw = raw;
                        s.refused = None;
                        s.loaded = now(self);
                    }
                    Err(e) => {
                        if s.refused.as_ref() != Some(&raw) {
                            kit::note(&format!("{file} wasn't reloaded, the old list stays: {e}"));
                            s.refused = Some(raw);
                        }
                    }
                },
                Err(e) => {
                    if s.refused.as_deref() != Some(b"") {
                        kit::note(&format!("{e}; the old list stays"));
                        s.refused = Some(Vec::new());
                    }
                }
            }
        }
        if s.dirty {
            let key = format!("hits/{}", s.id);
            let _ = self.set_shared_data(
                &key,
                Some(table::encode_hits(&s.hits, MAX_HITS).as_bytes()),
                None,
            );
            s.dirty = false;
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

impl Http {
    /// Every instance's hits, this one's as they are now.
    fn all_hits(&self, s: &State) -> HashMap<String, u64> {
        let n = self
            .get_shared_data("instances")
            .0
            .and_then(|v| String::from_utf8(v).ok())
            .and_then(|v| v.parse::<u64>().ok())
            .unwrap_or(0);
        let mut all = s.hits.clone();
        for id in (1..=n).filter(|id| *id != s.id) {
            if let Some(v) = self.get_shared_data(&format!("hits/{id}")).0 {
                table::add_hits(&mut all, &String::from_utf8_lossy(&v));
            }
        }
        all
    }
}

impl HttpContext for Http {
    fn on_http_request_headers(&mut self, _: usize, _: bool) -> Action {
        let Some(st) = self.st.clone() else {
            return Action::Continue;
        };
        let path = kit::property(self, &["request", "url_path"]);
        let query = kit::property(self, &["request", "query"]);
        let mut s = st.borrow_mut();

        if s.cfg.report.as_deref() == Some(path.as_str())
            && kit::any_contains(
                &s.cfg.report_allow,
                &kit::property(self, &["source", "address"]),
            )
        {
            let hits = self.all_hits(&s);
            let unused = query.split('&').any(|p| p == "unused");
            let every = SHARE_EVERY.min(s.cfg.reload.max(1));
            let body = table::report(&s.table, &s.cfg.file, &s.loaded, &hits, unused, every);
            self.send_http_response(
                200,
                vec![
                    ("content-type", "text/plain; charset=utf-8"),
                    ("cache-control", "no-store"),
                ],
                Some(body.as_bytes()),
            );
            kit::note("the redirect report went out");
            return Action::Pause;
        }

        let t = s.table.clone();
        let Some(hit) = t.lookup(&path, &query) else {
            return Action::Continue;
        };
        let e = hit.entry;
        kit::note(&format!(
            "{} line {}: {} to {} ({})",
            s.cfg.file, e.line, path, hit.location, e.status
        ));
        *s.hits.entry(t.from(&e).to_string()).or_default() += 1;
        s.dirty = true;
        if let Some(m) = s.metric {
            let _ = hostcalls::increment_metric(m, 1);
        }
        self.send_http_response(e.status, vec![("location", hit.location.as_str())], None);
        Action::Pause
    }
}
