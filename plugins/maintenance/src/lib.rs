// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! BareProxy's maintenance and failover pages plugin. When the backend
//! answers 502, 503 or 504, or BareProxy does because no backend is up, it
//! sends a page of the site's own instead, with the status kept. With
//! maintenance on, every visitor gets the maintenance page, except addresses
//! on an allow list. README.md is the manual.

pub mod config;

use config::{Answer, Config};
use proxy_wasm::traits::*;
use proxy_wasm::types::*;
use std::rc::Rc;

proxy_wasm::main! {{
    proxy_wasm::set_log_level(LogLevel::Warn);
    proxy_wasm::set_root_context(|_| -> Box<dyn RootContext> {
        Box::new(Root { cfg: Rc::new(Config::default()) })
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
            answered: false,
        }))
    }

    fn get_type(&self) -> Option<ContextType> {
        Some(ContextType::HttpContext)
    }
}

struct Http {
    cfg: Rc<Config>,
    path: String,
    answered: bool,
}

impl Context for Http {}

impl Http {
    /// Sends the answer, or notes why the request went on. Returns whether
    /// a page went out.
    fn act(&mut self, a: Option<Answer>) -> bool {
        match a {
            Some(Answer::Page {
                status,
                headers,
                body,
                note,
            }) => {
                let h: Vec<(&str, &str)> = headers
                    .iter()
                    .map(|(k, v)| (k.as_str(), v.as_str()))
                    .collect();
                self.send_http_response(status, h, Some(body.as_bytes()));
                kit::note(&note);
                self.answered = true;
                true
            }
            Some(Answer::Pass(note)) => {
                kit::note(&note);
                false
            }
            None => false,
        }
    }
}

impl HttpContext for Http {
    fn on_http_request_headers(&mut self, _: usize, _: bool) -> Action {
        self.path = kit::property(self, &["request", "url_path"]);
        let addr = kit::property(self, &["source", "address"]);
        let a = self.cfg.on_request(&self.path, &addr);
        if self.act(a) {
            Action::Pause
        } else {
            Action::Continue
        }
    }

    fn on_http_response_headers(&mut self, _: usize, _: bool) -> Action {
        if self.answered {
            return Action::Continue;
        }
        let status = self
            .get_http_response_header(":status")
            .and_then(|s| s.parse::<u32>().ok())
            .unwrap_or(0);
        let a = self.cfg.on_response(&self.path, status);
        if self.act(a) {
            Action::Pause
        } else {
            Action::Continue
        }
    }
}
