// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! BareProxy's header and rewrite rules plugin: security headers on every
//! response, header changes by path, redirects (some by request header) and
//! rewrites of the path the core routes on. Exact paths and prefixes only,
//! never regular expressions. README.md is the manual.

pub mod config;

use config::{Change, Config};
use proxy_wasm::traits::*;
use proxy_wasm::types::*;
use std::rc::Rc;

proxy_wasm::main! {{
    proxy_wasm::set_log_level(LogLevel::Warn);
    proxy_wasm::set_root_context(|_| -> Box<dyn RootContext> {
        Box::new(Root { cfg: Rc::new(Config::parse("").unwrap()) })
    });
}}

struct Root {
    cfg: Rc<Config>,
}

impl Context for Root {}

impl RootContext for Root {
    fn on_configure(&mut self, _: usize) -> bool {
        let text = self.get_plugin_configuration().unwrap_or_default();
        match Config::parse(&String::from_utf8_lossy(&text)) {
            Ok(c) => {
                self.cfg = Rc::new(c);
                true
            }
            Err(e) => {
                kit::note(&e);
                false
            }
        }
    }

    fn create_http_context(&self, _: u32) -> Option<Box<dyn HttpContext>> {
        Some(Box::new(Http {
            cfg: self.cfg.clone(),
            path: String::new(),
            https: false,
        }))
    }

    fn get_type(&self) -> Option<ContextType> {
        Some(ContextType::HttpContext)
    }
}

struct Http {
    cfg: Rc<Config>,
    /// The path as it arrived, before any rewrite.
    path: String,
    https: bool,
}

impl Context for Http {}

impl HttpContext for Http {
    fn on_http_request_headers(&mut self, _: usize, _: bool) -> Action {
        self.path = kit::property(self, &["request", "url_path"]);
        self.https = kit::property(self, &["request", "scheme"]) == "https";
        let query = kit::property(self, &["request", "query"]);
        let plan = self
            .cfg
            .on_request(&self.path, &query, &|h| self.get_http_request_header(h));
        for n in &plan.notes {
            kit::note(n);
        }
        if let Some((code, location, vary)) = plan.redirect {
            let mut h = vec![("location", location.as_str())];
            if let Some(v) = &vary {
                h.push(("vary", v.as_str()));
            }
            self.send_http_response(code, h, None);
            return Action::Pause;
        }
        if let Some(p) = plan.rewrite {
            self.set_http_request_header(":path", Some(&p));
        }
        for ch in &plan.changes {
            match ch {
                Change::Set(n, v) => self.set_http_request_header(n, Some(v)),
                Change::Add(n, v) => self.add_http_request_header(n, v),
                Change::Remove(n) => self.set_http_request_header(n, None),
            }
        }
        Action::Continue
    }

    fn on_http_response_headers(&mut self, _: usize, _: bool) -> Action {
        let (changes, note) = self.cfg.on_response(&self.path, self.https, &|n| {
            self.get_http_response_header(n).is_some()
        });
        for ch in &changes {
            match ch {
                Change::Set(n, v) => self.set_http_response_header(n, Some(v)),
                Change::Add(n, v) => self.add_http_response_header(n, v),
                Change::Remove(n) => self.set_http_response_header(n, None),
            }
        }
        if let Some(n) = note {
            kit::note(&n);
        }
        Action::Continue
    }
}
