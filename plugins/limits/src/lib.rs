// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! BareProxy's rate limiting and basic WAF plugin. Limits count requests
//! per client address, path or site in sliding windows, shared by all of
//! the plugin's instances through BareProxy's own counters, and answer 429
//! past them. Simple rules turn away methods, sizes, paths and scanners a
//! site never wants, with exact values and prefixes only. README.md is the
//! manual.

pub mod config;

use config::{Config, Request, Verdict};
use proxy_wasm::hostcalls;
use proxy_wasm::traits::*;
use proxy_wasm::types::*;
use std::rc::Rc;

proxy_wasm::main! {{
    proxy_wasm::set_log_level(LogLevel::Warn);
    proxy_wasm::set_root_context(|_| -> Box<dyn RootContext> {
        Box::new(Root { cfg: None, metrics: (None, None) })
    });
}}

struct Root {
    cfg: Option<Rc<Config>>,
    /// Counters of requests limited and blocked, shown by status.
    metrics: (Option<u32>, Option<u32>),
}

impl Context for Root {}

impl RootContext for Root {
    fn on_configure(&mut self, _: usize) -> bool {
        let text = self.get_plugin_configuration().unwrap_or_default();
        match Config::parse(&String::from_utf8_lossy(&text)) {
            Ok(c) => {
                self.metrics = (
                    hostcalls::define_metric(MetricType::Counter, "limited").ok(),
                    hostcalls::define_metric(MetricType::Counter, "blocked").ok(),
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
            metrics: self.metrics,
        }))
    }

    fn get_type(&self) -> Option<ContextType> {
        Some(ContextType::HttpContext)
    }
}

struct Http {
    cfg: Option<Rc<Config>>,
    metrics: (Option<u32>, Option<u32>),
}

impl Context for Http {}

impl Http {
    fn refuse(
        &self,
        status: u32,
        extra: &[(String, String)],
        body: &str,
        note: &str,
        metric: Option<u32>,
    ) -> Action {
        kit::note(note);
        if let Some(m) = metric {
            let _ = hostcalls::increment_metric(m, 1);
        }
        let mut h = vec![
            ("content-type", "text/plain; charset=utf-8"),
            ("cache-control", "no-store"),
        ];
        h.extend(extra.iter().map(|(k, v)| (k.as_str(), v.as_str())));
        self.send_http_response(status, h, Some(body.as_bytes()));
        Action::Pause
    }
}

impl HttpContext for Http {
    fn on_http_request_headers(&mut self, _: usize, _: bool) -> Action {
        let Some(cfg) = self.cfg.clone() else {
            return Action::Continue;
        };
        let source = kit::property(self, &["source", "address"]);
        let forwarded = self.get_http_request_header("x-forwarded-for");
        let addr = kit::client_addr(&source, forwarded.as_deref(), &cfg.forwarded_from)
            .map(|ip| ip.to_string())
            .unwrap_or(source);
        if kit::any_contains(&cfg.allow, &addr) {
            return Action::Continue;
        }
        let headers = self.get_http_request_headers();
        let get = |n: &str| {
            headers
                .iter()
                .find(|(k, _)| k == n)
                .map(|(_, v)| v.as_str())
                .unwrap_or("")
        };
        let path = kit::property(self, &["request", "url_path"]);
        let r = Request {
            addr: &addr,
            method: get(":method"),
            target: get(":path"),
            path: &path,
            headers: &headers,
        };
        if let Verdict::Refuse(status, extra, body, note) = cfg.rules(&r) {
            return self.refuse(status, &extra, &body, &note, self.metrics.1);
        }
        for (l, key) in cfg.keys(&addr, &path) {
            let Some((cur, prev, into)) = kit::count(&key, l.window_ms, 1) else {
                // BareProxy keeps too many counters already; let it through.
                kit::note(&format!(
                    "limit {}: not counted, BareProxy holds too many counters",
                    l.name
                ));
                continue;
            };
            let n = kit::sliding(cur, prev, into, l.window_ms);
            if n > l.count {
                let wait = l.window_ms as u64 - into.min(l.window_ms as u64);
                let who = if l.by == config::By::Site {
                    "the site".to_string()
                } else {
                    addr.clone()
                };
                let note = format!(
                    "limit {} (line {}): {n} requests in the last {} from {who}, over {}; the window resets in {}",
                    l.name,
                    l.line,
                    config::short(l.window_ms as u64),
                    l.count,
                    config::short(wait)
                );
                let retry = [("retry-after".to_string(), wait.div_ceil(1000).to_string())];
                return self.refuse(
                    429,
                    &retry,
                    "Too many requests. Please slow down.\n",
                    &note,
                    self.metrics.0,
                );
            }
        }
        Action::Continue
    }
}
