// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! BareProxy's CORS plugin. It answers preflights at the proxy and adds the
//! Access-Control-* headers to responses, from one allow list of origins,
//! per site and per path prefix. README.md is the manual.

pub mod config;

use config::{Config, Decision};
use proxy_wasm::traits::*;
use proxy_wasm::types::*;
use std::rc::Rc;

proxy_wasm::main! {{
    proxy_wasm::set_log_level(LogLevel::Warn);
    proxy_wasm::set_root_context(|_| -> Box<dyn RootContext> { Box::new(Root { cfg: None }) });
}}

struct Root {
    cfg: Option<Rc<Config>>,
}

impl Context for Root {}

impl RootContext for Root {
    fn on_configure(&mut self, _: usize) -> bool {
        let text = self.get_plugin_configuration().unwrap_or_default();
        match Config::parse(&String::from_utf8_lossy(&text)) {
            Ok(c) => {
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
            out: None,
        }))
    }

    fn get_type(&self) -> Option<ContextType> {
        Some(ContextType::HttpContext)
    }
}

struct Http {
    cfg: Option<Rc<Config>>,
    /// What to do on the response.
    out: Option<(Vec<(String, String)>, bool)>,
}

impl Context for Http {}

impl HttpContext for Http {
    fn on_http_request_headers(&mut self, _: usize, _: bool) -> Action {
        let Some(cfg) = self.cfg.clone() else {
            return Action::Continue;
        };
        let path = kit::property(self, &["request", "url_path"]);
        let origin = self.get_http_request_header("origin");
        let method = self.get_http_request_header(":method").unwrap_or_default();
        let asked_method = self.get_http_request_header("access-control-request-method");
        let asked_headers = self
            .get_http_request_header("access-control-request-headers")
            .unwrap_or_default();
        let preflight = match (&origin, &asked_method) {
            (Some(_), Some(m)) if method == "OPTIONS" => Some((m.as_str(), asked_headers.as_str())),
            _ => None,
        };
        match cfg.decide(&path, origin.as_deref(), preflight) {
            Decision::Answer {
                status,
                headers,
                body,
                note,
            } => {
                let h: Vec<(&str, &str)> = headers
                    .iter()
                    .map(|(k, v)| (k.as_str(), v.as_str()))
                    .collect();
                let body = (!body.is_empty()).then_some(body.as_bytes());
                self.send_http_response(status, h, body);
                if !note.is_empty() {
                    kit::note(&note);
                }
                Action::Pause
            }
            Decision::Response { set, vary, note } => {
                if let Some(n) = note {
                    kit::note(&n);
                }
                self.out = Some((set, vary));
                Action::Continue
            }
        }
    }

    fn on_http_response_headers(&mut self, _: usize, _: bool) -> Action {
        let Some((set, vary)) = self.out.take() else {
            return Action::Continue;
        };
        // The plugin is the one source of CORS headers: the backend's go.
        for (name, _) in self.get_http_response_headers() {
            if name.starts_with("access-control-") {
                self.remove_http_response_header(&name);
            }
        }
        for (k, v) in &set {
            self.set_http_response_header(k, Some(v));
        }
        if vary {
            let old = self.get_http_response_header("vary").unwrap_or_default();
            let has = old.split(',').any(|v| {
                let v = v.trim();
                v == "*" || v.eq_ignore_ascii_case("origin")
            });
            if !has {
                let new = if old.trim().is_empty() {
                    "Origin".to_string()
                } else {
                    format!("{old}, Origin")
                };
                self.set_http_response_header("vary", Some(&new));
            }
        }
        Action::Continue
    }
}
