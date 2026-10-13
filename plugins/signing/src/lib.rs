// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! BareProxy's request signing plugin. It signs the requests it lets
//! through, so the app behind can tell they came through BareProxy and
//! weren't changed on the way, and it checks signed links that expire,
//! answering 403 to one that was changed or has run out. README.md is the
//! manual.

pub mod config;

use config::{Config, SIGNATURE, SIGNED_HEADERS, SIGNED_PATH, TIMESTAMP};
use proxy_wasm::traits::*;
use proxy_wasm::types::*;
use std::rc::Rc;
use std::time::UNIX_EPOCH;

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
        }))
    }

    fn get_type(&self) -> Option<ContextType> {
        Some(ContextType::HttpContext)
    }
}

struct Http {
    cfg: Option<Rc<Config>>,
}

impl Context for Http {}

impl HttpContext for Http {
    fn on_http_request_headers(&mut self, _: usize, _: bool) -> Action {
        let Some(cfg) = self.cfg.clone() else {
            return Action::Continue;
        };
        let now = self
            .get_current_time()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_secs())
            .unwrap_or(0);
        let path = kit::property(self, &["request", "url_path"]);
        if cfg.needs_link(&path) {
            let query = kit::property(self, &["request", "query"]);
            match cfg.check_link(now, &path, &query) {
                Ok(_) => {}
                Err(why) => {
                    kit::note(&format!("signed link refused: {why}"));
                    self.send_http_response(
                        403,
                        vec![
                            ("content-type", "text/plain; charset=utf-8"),
                            ("cache-control", "no-store"),
                        ],
                        Some(b"This link isn't valid, or has expired.\n"),
                    );
                    return Action::Pause;
                }
            }
        }
        // Whatever the client sent in these goes; only the plugin sets them.
        for h in [TIMESTAMP, SIGNATURE, SIGNED_HEADERS, SIGNED_PATH] {
            self.set_http_request_header(h, None);
        }
        let method = self.get_http_request_header(":method").unwrap_or_default();
        let target = self.get_http_request_header(":path").unwrap_or_default();
        if let Some(headers) =
            cfg.sign_request(now, &method, &target, &|h| self.get_http_request_header(h))
        {
            for (k, v) in &headers {
                self.set_http_request_header(k, Some(v));
            }
        }
        Action::Continue
    }
}
