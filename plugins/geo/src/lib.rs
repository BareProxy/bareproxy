// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! BareProxy's geo rules plugin: the country of each request's address,
//! from a local MaxMind DB file (GeoLite2, DB-IP Lite, IPinfo Lite), passed
//! on in a request header that route rules can match, with allow and block
//! lists by country for the whole site or a path. README.md is the manual.

pub mod config;
pub mod mmdb;

use config::Config;
use mmdb::Db;
use proxy_wasm::traits::*;
use proxy_wasm::types::*;
use std::cell::RefCell;
use std::rc::Rc;
use std::time::Duration;

proxy_wasm::main! {{
    proxy_wasm::set_log_level(LogLevel::Warn);
    proxy_wasm::set_root_context(|_| -> Box<dyn RootContext> { Box::new(Root { st: None }) });
}}

struct State {
    cfg: Rc<Config>,
    db: Rc<Db>,
    /// A file that couldn't be loaded, so its error is logged once: its
    /// length, or 0 when it couldn't be read.
    refused: Option<usize>,
}

struct Root {
    st: Option<Rc<RefCell<State>>>,
}

fn load(file: &str) -> Result<Db, String> {
    Db::open(kit::read_file(file)?).map_err(|e| format!("{file}: {e}"))
}

impl Context for Root {}

impl RootContext for Root {
    fn on_configure(&mut self, _: usize) -> bool {
        let text = self.get_plugin_configuration().unwrap_or_default();
        let loaded = Config::parse(&String::from_utf8_lossy(&text)).and_then(|cfg| {
            let db = load(&cfg.database)?;
            Ok((cfg, db))
        });
        let (cfg, db) = match loaded {
            Ok(x) => x,
            Err(e) => {
                kit::note(&e);
                return false;
            }
        };
        if cfg.reload > 0 {
            self.set_tick_period(Duration::from_secs(cfg.reload));
        }
        self.st = Some(Rc::new(RefCell::new(State {
            cfg: Rc::new(cfg),
            db: Rc::new(db),
            refused: None,
        })));
        true
    }

    fn on_tick(&mut self) {
        let Some(st) = self.st.clone() else { return };
        let mut s = st.borrow_mut();
        let file = s.cfg.database.clone();
        match load(&file) {
            Ok(db) => {
                if db.size() != s.db.size() || db.build_epoch != s.db.build_epoch {
                    kit::note(&format!(
                        "reloaded {file} ({}, built {})",
                        db.db_type, db.build_epoch
                    ));
                    s.db = Rc::new(db);
                }
                s.refused = None;
            }
            Err(e) => {
                let mark = kit::read_file(&file).map(|b| b.len()).unwrap_or(0);
                if s.refused != Some(mark) {
                    kit::note(&format!("{e}; the database as it was stays"));
                    s.refused = Some(mark);
                }
            }
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

impl HttpContext for Http {
    fn on_http_request_headers(&mut self, _: usize, _: bool) -> Action {
        let Some(st) = self.st.clone() else {
            return Action::Continue;
        };
        let (cfg, db) = {
            let s = st.borrow();
            (s.cfg.clone(), s.db.clone())
        };
        let source = kit::property(self, &["source", "address"]);
        let forwarded = self.get_http_request_header("x-forwarded-for");
        let ip = cfg.client(&source, forwarded.as_deref());
        let country = match ip.map(|ip| db.country(ip)) {
            Some(Ok(c)) => c,
            Some(Err(e)) => {
                kit::note(&format!("geo lookup failed: {e}"));
                None
            }
            None => None,
        };
        // Set it whatever the client sent, so it can't be faked.
        if let Some(h) = &cfg.header {
            self.set_http_request_header(h, Some(country.as_deref().unwrap_or("XX")));
        }
        let path = kit::property(self, &["request", "url_path"]);
        if let Err(why) = cfg.decide(&path, country.as_deref()) {
            let addr = ip.map(|i| i.to_string()).unwrap_or(source);
            kit::note(&format!("turned away: {why} (address {addr})"));
            self.send_http_response(
                cfg.status,
                vec![
                    ("content-type", "text/html; charset=utf-8"),
                    ("cache-control", "no-store"),
                ],
                Some(cfg.page.as_bytes()),
            );
            return Action::Pause;
        }
        Action::Continue
    }
}
