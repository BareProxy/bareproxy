// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! BareProxy's cookie consent plugin. A visitor who hasn't chosen yet gets
//! a banner at the foot of each HTML page, with links to accept, refuse or
//! choose; the choice goes in one cookie. Until a category is accepted, its
//! cookies don't reach the browser (their Set-Cookie headers are dropped)
//! or the app (they're taken out of the Cookie header), and the scripts and
//! iframes the config names for it are made inert in the page. A browser
//! that sends Global Privacy Control counts as having said no. README.md is
//! the manual.
//!
//! Pages are changed in timer calls. A request's own call may run for 5
//! milliseconds, and going through a large page in the sandbox takes
//! longer (about 45 ms a megabyte on a 2.1 GHz server), where a timer
//! call may run for a second.

pub mod config;
pub mod html;
pub mod pages;

use config::{Choice, Config, Decision};
use proxy_wasm::hostcalls;
use proxy_wasm::traits::*;
use proxy_wasm::types::*;
use std::cell::RefCell;
use std::collections::VecDeque;
use std::rc::Rc;
use std::time::{Duration, UNIX_EPOCH};

/// The most a timer call spends on pages before it leaves the rest to the
/// next one, a millisecond later: well inside its second.
const TICK_BUDGET_MS: f64 = 250.0;

proxy_wasm::main! {{
    proxy_wasm::set_log_level(LogLevel::Warn);
    proxy_wasm::set_root_context(|_| -> Box<dyn RootContext> {
        Box::new(Root { cfg: None, m: Metrics::default(), work: Rc::default() })
    });
}}

#[derive(Clone, Copy, Default)]
struct Metrics {
    banners: Option<u32>,
    accepted: Option<u32>,
    declined: Option<u32>,
    withdrawn: Option<u32>,
    held: Option<u32>,
    blocked: Option<u32>,
}

fn bump(m: Option<u32>, n: usize) {
    if let Some(id) = m
        && n > 0
    {
        let _ = hostcalls::increment_metric(id, n as i64);
    }
}

struct Root {
    cfg: Option<Rc<Config>>,
    m: Metrics,
    work: Rc<RefCell<Work>>,
}

/// Pages waiting for a timer call.
#[derive(Default)]
struct Work {
    jobs: VecDeque<Job>,
    ticking: bool,
}

/// A page to change: its request, its size, the banner (if it gets one)
/// and the categories whose scripts stay out.
struct Job {
    ctx: u32,
    size: usize,
    banner: Option<String>,
    refused: Vec<String>,
}

fn millis(ctx: &dyn Context) -> f64 {
    ctx.get_current_time()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs_f64() * 1000.0)
        .unwrap_or(0.0)
}

/// Changes a page that waited for a timer call, in its request's context.
fn change_page(cfg: &Config, m: &Metrics, job: Job) {
    let body = hostcalls::get_buffer(BufferType::HttpResponseBody, 0, job.size)
        .ok()
        .flatten()
        .unwrap_or_default();
    let Ok(text) = std::str::from_utf8(&body) else {
        kit::note("the page isn't UTF-8, so it went out unchanged, without the banner");
        return;
    };
    let refused: Vec<&config::Category> = cfg
        .categories
        .iter()
        .filter(|c| job.refused.contains(&c.name))
        .collect();
    let Some((page, hits)) = html::rewrite(text, &refused, job.banner.as_deref()) else {
        return;
    };
    let _ = hostcalls::set_buffer(BufferType::HttpResponseBody, 0, job.size, page.as_bytes());
    let mut notes = vec![];
    if job.banner.is_some() {
        bump(m.banners, 1);
        notes.push("showed the banner: no choice yet".to_string());
    }
    if !hits.is_empty() {
        bump(m.blocked, hits.len());
        let mut what: Vec<String> = hits
            .iter()
            .map(|h| format!("{} ({})", h.by, h.category))
            .collect();
        what.dedup();
        let plural = |n: usize, w: &str| format!("{n} {w}{}", if n == 1 { "" } else { "s" });
        let scripts = hits.iter().filter(|h| h.tag == "script").count();
        let count = match (scripts, hits.len() - scripts) {
            (s, 0) => plural(s, "script"),
            (0, i) => plural(i, "iframe"),
            (s, i) => format!("{} and {}", plural(s, "script"), plural(i, "iframe")),
        };
        notes.push(format!(
            "kept {count} out until consent: {}",
            what.join(", ")
        ));
    }
    kit::note(&notes.join("; "));
}

impl Context for Root {}

impl RootContext for Root {
    fn on_configure(&mut self, _: usize) -> bool {
        let text = self.get_plugin_configuration().unwrap_or_default();
        match Config::parse(&String::from_utf8_lossy(&text)) {
            Ok(c) => {
                self.cfg = Some(Rc::new(c));
                let metric = |n: &str| hostcalls::define_metric(MetricType::Counter, n).ok();
                self.m = Metrics {
                    banners: metric("banners"),
                    accepted: metric("accepted"),
                    declined: metric("declined"),
                    withdrawn: metric("withdrawn"),
                    held: metric("held"),
                    blocked: metric("blocked"),
                };
                true
            }
            Err(e) => {
                kit::note(&e);
                false
            }
        }
    }

    /// Changes the pages waiting, for up to TICK_BUDGET_MS, and stops the
    /// timer when none are left.
    fn on_tick(&mut self) {
        let Some(cfg) = self.cfg.clone() else { return };
        let start = millis(self);
        while millis(self) - start < TICK_BUDGET_MS {
            let Some(job) = self.work.borrow_mut().jobs.pop_front() else {
                break;
            };
            // A request that's gone (the client left) has no context now.
            if hostcalls::set_effective_context(job.ctx).is_err() {
                continue;
            }
            change_page(&cfg, &self.m, job);
            let _ = hostcalls::resume_http_response();
        }
        let mut w = self.work.borrow_mut();
        if w.jobs.is_empty() {
            w.ticking = false;
            drop(w);
            self.set_tick_period(Duration::ZERO);
        }
    }

    fn create_http_context(&self, id: u32) -> Option<Box<dyn HttpContext>> {
        Some(Box::new(Http {
            id,
            work: self.work.clone(),
            cfg: self.cfg.clone(),
            m: self.m,
            choice: Choice::Undecided,
            want: false,
            banner: false,
            back: String::new(),
            html: false,
        }))
    }

    fn get_type(&self) -> Option<ContextType> {
        Some(ContextType::HttpContext)
    }
}

struct Http {
    id: u32,
    work: Rc<RefCell<Work>>,
    cfg: Option<Rc<Config>>,
    m: Metrics,
    choice: Choice,
    /// The page may change: the banner, or scripts to keep out.
    want: bool,
    banner: bool,
    /// The page's path and query, where a choice returns to.
    back: String,
    /// The response is an HTML page the plugin changes.
    html: bool,
}

impl Context for Http {}

/// The values of a header, from all the request's headers.
fn header<'a>(h: &'a [(String, String)], name: &str) -> Option<&'a str> {
    h.iter().find(|(k, _)| k == name).map(|(_, v)| v.as_str())
}

/// Every Cookie header, as one.
fn cookies(h: &[(String, String)]) -> String {
    h.iter()
        .filter(|(k, _)| k == "cookie")
        .map(|(_, v)| v.as_str())
        .collect::<Vec<_>>()
        .join("; ")
}

/// The host part of an authority, without its port.
fn host_only(authority: &str) -> &str {
    if authority.starts_with('[') {
        return authority.split(']').next().map_or(authority, |h| &h[1..]);
    }
    authority.rsplit_once(':').map_or(authority, |(h, _)| h)
}

impl Http {
    /// The endpoint: the settings page, the styles, and choices to keep.
    fn endpoint(&mut self, cfg: &Config, path: &str, h: &[(String, String)]) -> Action {
        let method = header(h, ":method").unwrap_or("");
        if method != "GET" && method != "HEAD" {
            self.send_http_response(
                405,
                vec![("allow", "GET, HEAD")],
                Some(b"Method not allowed\n"),
            );
            return Action::Pause;
        }
        let rest = &path[cfg.endpoint.len()..];
        if rest == "/consent.css" {
            self.send_http_response(
                200,
                vec![
                    ("content-type", "text/css; charset=utf-8"),
                    ("cache-control", "public, max-age=31536000, immutable"),
                ],
                Some(pages::CSS.as_bytes()),
            );
            return Action::Pause;
        }
        if !rest.is_empty() {
            self.send_http_response(
                404,
                vec![("content-type", "text/plain; charset=utf-8")],
                Some(b"Not found\n"),
            );
            return Action::Pause;
        }
        let query = kit::property(self, &["request", "query"]);
        let jar = cookies(h);
        let gpc = header(h, "sec-gpc").is_some_and(|v| v.trim() == "1");
        let old = cfg.choice(&jar, gpc);
        let authority = header(h, ":authority").unwrap_or("");
        let referer = header(h, "referer");
        let same = config::same_site(header(h, "sec-fetch-site"), referer, authority);
        let back = config::return_to(&query, referer, authority);
        match cfg.decide(&query, same) {
            Decision::Show(elsewhere) => {
                if elsewhere {
                    kit::note(
                        "an accept from another site isn't counted, so the settings page asks instead",
                    );
                }
                let page = cfg.settings(old.accepted(), old == Choice::Gpc, &back);
                self.send_http_response(
                    200,
                    vec![
                        ("content-type", "text/html; charset=utf-8"),
                        ("cache-control", "no-store"),
                    ],
                    Some(page.as_bytes()),
                );
            }
            Decision::Keep(set) => {
                let https = kit::property(self, &["request", "scheme"]) == "https";
                let mut sets = vec![cfg.set_cookie(&set, https)];
                sets.extend(cfg.expiries(&jar, &set, host_only(authority)));
                let gone: Vec<String> = old
                    .accepted()
                    .iter()
                    .filter(|c| !set.contains(c))
                    .cloned()
                    .collect();
                let mut note = if set.is_empty() {
                    bump(self.m.declined, 1);
                    "consent: declined everything".to_string()
                } else {
                    bump(self.m.accepted, 1);
                    format!("consent: accepted {}", Config::names(&set))
                };
                if !gone.is_empty() {
                    bump(self.m.withdrawn, 1);
                    note.push_str(&format!("; withdrew {}", Config::names(&gone)));
                }
                let mut hs = vec![("location", back.as_str()), ("cache-control", "no-store")];
                for s in &sets {
                    hs.push(("set-cookie", s.as_str()));
                }
                self.send_http_response(303, hs, None);
                kit::note(&note);
            }
        }
        Action::Pause
    }
}

impl HttpContext for Http {
    fn on_http_request_headers(&mut self, _: usize, _: bool) -> Action {
        let Some(cfg) = self.cfg.clone() else {
            return Action::Continue;
        };
        let h = self.get_http_request_headers();
        let path = kit::property(self, &["request", "url_path"]);
        if path == cfg.endpoint || path.starts_with(&format!("{}/", cfg.endpoint)) {
            return self.endpoint(&cfg, &path, &h);
        }
        let jar = cookies(&h);
        let gpc = header(&h, "sec-gpc").is_some_and(|v| v.trim() == "1");
        self.choice = cfg.choice(&jar, gpc);
        // The app hears what was accepted from the plugin, never from the client.
        if let Some(name) = &cfg.header {
            self.set_http_request_header(name, Some(&cfg.header_value(&self.choice)));
        }
        // Cookies of categories without consent don't reach the app.
        if !jar.is_empty() {
            let (kept, gone) = cfg.strip_cookies(&jar, &self.choice);
            if !gone.is_empty() {
                self.set_http_request_header("cookie", (!kept.is_empty()).then_some(kept.as_str()));
                kit::note(&format!(
                    "kept {} from the app: no consent",
                    Config::names(&gone)
                ));
            }
        }
        if self.choice == Choice::Gpc {
            kit::note("the browser sends Global Privacy Control, so nothing is accepted");
        }
        let method = header(&h, ":method").unwrap_or("");
        let page = config::is_page(
            method,
            header(&h, "sec-fetch-dest"),
            header(&h, "sec-fetch-mode"),
            header(&h, "accept"),
            header(&h, "user-agent").unwrap_or(""),
        ) && !cfg.skipped(&path);
        let blocking = cfg
            .refused(&self.choice)
            .iter()
            .any(|c| !c.block.is_empty());
        self.banner = page && self.choice == Choice::Undecided;
        self.want = page && (self.banner || blocking);
        if self.want {
            // The page comes whole and uncompressed, so the plugin can
            // change it: no 304 for a copy the browser kept, which may be
            // from before the visitor chose.
            self.set_http_request_header("accept-encoding", None);
            self.set_http_request_header("if-none-match", None);
            self.set_http_request_header("if-modified-since", None);
            let q = kit::property(self, &["request", "query"]);
            self.back = if q.is_empty() {
                path
            } else {
                format!("{path}?{q}")
            };
        } else {
            // The body streams past the plugin, and a file still goes out
            // with sendfile.
            self.set_property(vec!["bareproxy", "response_body"], Some(b"skip"));
        }
        Action::Continue
    }

    fn on_http_response_headers(&mut self, _: usize, _: bool) -> Action {
        let Some(cfg) = self.cfg.clone() else {
            return Action::Continue;
        };
        // A cookie without consent never reaches the browser.
        let headers = self.get_http_response_headers();
        let mut held = vec![];
        let mut kept: Vec<(&str, &str)> = Vec::with_capacity(headers.len());
        for (k, v) in &headers {
            if k == "set-cookie"
                && let Some((name, cat)) = cfg.held(v, &self.choice)
            {
                held.push(format!("{name} ({cat})"));
                continue;
            }
            kept.push((k.as_str(), v.as_str()));
        }
        if !held.is_empty() {
            self.set_http_response_headers(kept);
            bump(self.m.held, held.len());
            kit::note(&format!("held back {} until consent", Config::names(&held)));
        }
        let get = |n: &str| {
            headers
                .iter()
                .find(|(k, _)| k == n)
                .map(|(_, v)| v.as_str())
        };
        let html =
            get("content-type").is_some_and(|t| t.to_ascii_lowercase().starts_with("text/html"));
        if html {
            // The page depends on the visitor's choice, so a cache in
            // between keeps a copy for each.
            let vary = get("vary").unwrap_or("");
            if !vary.split(',').any(|v| {
                let v = v.trim();
                v == "*" || v.eq_ignore_ascii_case("cookie")
            }) {
                let v = if vary.trim().is_empty() {
                    "Cookie".to_string()
                } else {
                    format!("{vary}, Cookie")
                };
                self.set_http_response_header("vary", Some(&v));
            }
        }
        self.html = self.want && html && get(":status") == Some("200");
        if self.html {
            // A changed page is for this visitor's choice only, and has
            // nothing a browser could check a kept copy against.
            self.set_http_response_header("cache-control", Some("private, no-cache"));
            self.set_http_response_header("etag", None);
            self.set_http_response_header("last-modified", None);
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
        // The page is changed in the next timer call, and waits for it.
        let mut w = self.work.borrow_mut();
        w.jobs.push_back(Job {
            ctx: self.id,
            size,
            banner: self.banner.then(|| cfg.banner(&self.back)),
            refused: cfg
                .refused(&self.choice)
                .iter()
                .map(|c| c.name.clone())
                .collect(),
        });
        if !w.ticking {
            w.ticking = true;
            let _ = hostcalls::set_tick_period(Duration::from_millis(1));
        }
        Action::Pause
    }
}
