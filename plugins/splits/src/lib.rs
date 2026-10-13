// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! BareProxy's A/B and canary splits plugin. It picks each visitor's
//! variant, by the shares in its config, a force rule, or the cookie that
//! keeps a visitor on theirs, and puts it in a request header. BareProxy's
//! own route rules then pick the pool, so `plan` and `explain` see the
//! routing. README.md is the manual.

pub mod config;

use config::{Config, Request};
use proxy_wasm::hostcalls;
use proxy_wasm::traits::*;
use proxy_wasm::types::*;
use std::rc::Rc;
use std::time::UNIX_EPOCH;

proxy_wasm::main! {{
    proxy_wasm::set_log_level(LogLevel::Warn);
    proxy_wasm::set_root_context(|_| -> Box<dyn RootContext> {
        Box::new(Root { cfg: None, metrics: Rc::new(vec![]) })
    });
}}

struct Root {
    cfg: Option<Rc<Config>>,
    /// A counter of requests for each variant, shown by status.
    metrics: Rc<Vec<Option<u32>>>,
}

impl Context for Root {}

impl RootContext for Root {
    fn on_configure(&mut self, _: usize) -> bool {
        let text = self.get_plugin_configuration().unwrap_or_default();
        match Config::parse(&String::from_utf8_lossy(&text)) {
            Ok(c) => {
                self.metrics = Rc::new(
                    c.variants
                        .iter()
                        .map(|v| {
                            hostcalls::define_metric(
                                MetricType::Counter,
                                &format!("variant.{}", v.name),
                            )
                            .ok()
                        })
                        .collect(),
                );
                self.cfg = Some(Rc::new(c));
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
            metrics: self.metrics.clone(),
            cookie: None,
        }))
    }

    fn get_type(&self) -> Option<ContextType> {
        Some(ContextType::HttpContext)
    }
}

struct Http {
    cfg: Option<Rc<Config>>,
    metrics: Rc<Vec<Option<u32>>>,
    /// A Set-Cookie for the response.
    cookie: Option<String>,
}

impl Context for Http {}

impl HttpContext for Http {
    fn on_http_request_headers(&mut self, _: usize, _: bool) -> Action {
        let Some(cfg) = self.cfg.clone() else {
            return Action::Continue;
        };
        let nanos = self
            .get_current_time()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.subsec_nanos() as u64)
            .unwrap_or(0);
        let id = kit::property(self, &["request", "id"]);
        let addr = kit::property(self, &["source", "address"]);
        let query = kit::property(self, &["request", "query"]);
        let header = |h: &str| self.get_http_request_header(h);
        let choice = cfg.choose(&Request {
            addr: &addr,
            header: &header,
            query: &query,
            roll: config::roll(&id, nanos),
        });
        let name = &cfg.variants[choice.variant].name;
        // Set whatever the client sent, so the variant can't be picked by
        // sending the header.
        self.set_http_request_header(&cfg.header, Some(name));
        kit::note(&format!("variant {name}: {}", choice.why));
        if let Some(Some(m)) = self.metrics.get(choice.variant) {
            let _ = hostcalls::increment_metric(*m, 1);
        }
        if choice.set_cookie {
            let https = kit::property(self, &["request", "scheme"]) == "https";
            self.cookie = Some(cfg.set_cookie(choice.variant, https));
        }
        Action::Continue
    }

    fn on_http_response_headers(&mut self, _: usize, _: bool) -> Action {
        if let Some(c) = self.cookie.take() {
            self.add_http_response_header("set-cookie", &c);
        }
        Action::Continue
    }
}
