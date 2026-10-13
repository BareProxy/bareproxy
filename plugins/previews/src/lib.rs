// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! BareProxy's link previews plugin. When a link-preview fetcher (Slack,
//! WhatsApp, LinkedIn, X, iMessage and the rest) asks for an HTML page
//! without Open Graph tags, it adds them: the page's title, a description,
//! an image. Visitors' pages are never changed, and their bodies aren't even
//! handed to the plugin. README.md is the manual.

pub mod config;

use config::Config;
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
            fetcher: None,
            html: false,
            base: String::new(),
            path: String::new(),
        }))
    }

    fn get_type(&self) -> Option<ContextType> {
        Some(ContextType::HttpContext)
    }
}

struct Http {
    cfg: Option<Rc<Config>>,
    /// The fetcher asking, if one is.
    fetcher: Option<String>,
    html: bool,
    base: String,
    path: String,
}

impl Context for Http {}

impl HttpContext for Http {
    fn on_http_request_headers(&mut self, _: usize, _: bool) -> Action {
        let Some(cfg) = self.cfg.clone() else {
            return Action::Continue;
        };
        let agent = self
            .get_http_request_header("user-agent")
            .unwrap_or_default();
        match cfg.fetcher(&agent) {
            Some(f) => {
                self.fetcher = Some(f.to_string());
                let scheme = kit::property(self, &["request", "scheme"]);
                let host = self
                    .get_http_request_header(":authority")
                    .unwrap_or_default();
                self.base = format!(
                    "{}://{host}",
                    if scheme.is_empty() { "https" } else { &scheme }
                );
                self.path = kit::property(self, &["request", "url_path"]);
            }
            // Not a fetcher: the response body streams past the plugin, and a
            // file still goes out with sendfile.
            None => self.set_property(vec!["bareproxy", "response_body"], Some(b"skip")),
        }
        Action::Continue
    }

    fn on_http_response_headers(&mut self, _: usize, _: bool) -> Action {
        if self.fetcher.is_some() {
            let status = self.get_http_response_header(":status").unwrap_or_default();
            let ctype = self
                .get_http_response_header("content-type")
                .unwrap_or_default()
                .to_ascii_lowercase();
            self.html = status == "200" && ctype.starts_with("text/html");
        }
        Action::Continue
    }

    fn on_http_response_body(&mut self, size: usize, end_of_stream: bool) -> Action {
        if !self.html {
            return Action::Continue;
        }
        if !end_of_stream {
            return Action::Pause; // the whole page, please
        }
        let Some(cfg) = self.cfg.clone() else {
            return Action::Continue;
        };
        let body = self.get_http_response_body(0, size).unwrap_or_default();
        let Ok(html) = std::str::from_utf8(&body) else {
            kit::note("link previews: the page isn't UTF-8, so it went out as it was");
            return Action::Continue;
        };
        match cfg.fill(html, &self.base, &self.path) {
            Some((page, added)) => {
                self.set_http_response_body(0, size, page.as_bytes());
                kit::note(&format!(
                    "link previews: added {} for {}",
                    added.join(", "),
                    self.fetcher.as_deref().unwrap_or("a fetcher")
                ));
            }
            None => kit::note("link previews: the page has its tags already"),
        }
        Action::Continue
    }
}
