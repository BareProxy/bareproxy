// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! BareProxy's Markdown serving plugin. A request for /guide/setup gets
//! guide/setup.md, from a folder the plugin line names with read, rendered
//! to HTML inside a template; /guide/ gets guide/index.md or README.md. A
//! page is rendered once and kept until its file or the template changes,
//! which the plugin tells from the file's size and time of change, without
//! reading it. A client that asks for text/markdown gets the Markdown as
//! written. README.md is the manual.
//!
//! Pages render in timer calls, never in a request's own call: a request's
//! call may run for 5 milliseconds, and a Markdown file's size says little
//! about how long it takes (5 KB of nested quotes takes longer than 100 KB
//! of prose), where a timer call may run for a second. A request whose page
//! isn't rendered yet waits for the next timer call, a millisecond away.

pub mod config;
pub mod render;

use config::{At, Config, Lookup, Template};
use proxy_wasm::hostcalls;
use proxy_wasm::traits::*;
use proxy_wasm::types::*;
use std::cell::RefCell;
use std::collections::{HashMap, VecDeque};
use std::rc::Rc;
use std::time::{Duration, UNIX_EPOCH};

/// The most a timer call spends rendering before it leaves the pages still
/// waiting to the next one, a millisecond later: well inside its second.
const TICK_BUDGET_MS: f64 = 250.0;

/// A render that began this long ago and never finished lost its instance:
/// it ran past the time limit, or out of memory.
const STOPPED_AFTER_MS: f64 = 3000.0;

proxy_wasm::main! {{
    proxy_wasm::set_log_level(LogLevel::Warn);
    proxy_wasm::set_root_context(|_| -> Box<dyn RootContext> { Box::new(Root { st: None }) });
}}

#[derive(Clone, Copy, Default)]
struct Metrics {
    pages: Option<u32>,
    rendered: Option<u32>,
    markdown: Option<u32>,
}

fn bump(m: Option<u32>) {
    if let Some(id) = m {
        let _ = hostcalls::increment_metric(id, 1);
    }
}

/// A page as last rendered, and the Markdown it came from.
struct Entry {
    file: String,
    size: u64,
    mtime: u64,
    /// The template it was made with (State::tpl_gen).
    tpl: u64,
    html: Rc<Vec<u8>>,
    source: Rc<Vec<u8>>,
    used: u64,
}

impl Entry {
    fn cost(&self, key: &str) -> u64 {
        (self.html.len() + self.source.len() + key.len() + self.file.len() + 128) as u64
    }
}

/// The file a page comes from, as stat found it.
struct Found {
    key: String,
    file: String,
    size: u64,
    mtime: u64,
}

/// What a request asks, as its answer needs it.
struct Ask {
    none_match: String,
    markdown: bool,
    site: String,
}

/// A request waiting for its page to render.
struct Job {
    ctx: u32,
    found: Found,
    ask: Ask,
}

/// An answer, sent by the request's own call or by a timer call.
struct Reply {
    status: u32,
    headers: Vec<(&'static str, String)>,
    body: Option<Rc<Vec<u8>>>,
    note: String,
    metric: Option<u32>,
}

enum Step {
    Send(Reply),
    /// The request goes on to the route, with a note.
    GoOn(String),
    /// The page renders in the next timer call.
    Later,
}

enum Read {
    Ok(Vec<u8>),
    Missing,
    TooBig,
}

fn read(file: &str) -> Read {
    match hostcalls::call_foreign_function("bareproxy_read_file", Some(file.as_bytes())) {
        Ok(b) => Read::Ok(b.unwrap_or_default()),
        Err(Status::BadArgument) => Read::TooBig,
        Err(_) => Read::Missing,
    }
}

fn send(r: &Reply) {
    let h: Vec<(&str, &str)> = r.headers.iter().map(|(k, v)| (*k, v.as_str())).collect();
    let _ = hostcalls::send_http_response(r.status, h, r.body.as_ref().map(|b| b.as_slice()));
    bump(r.metric);
    kit::note(&r.note);
}

/// A page that can't be shown, with a note that says why.
fn refusal(body: &str, note: String) -> Reply {
    Reply {
        status: 500,
        headers: vec![
            ("content-type", "text/plain; charset=utf-8".into()),
            ("cache-control", "no-store".into()),
        ],
        body: Some(Rc::new(format!("{body}\n").into_bytes())),
        note,
        metric: None,
    }
}

const TOO_LARGE: &str = "This page is too large to show.";

/// Whether rendering this version of the file stopped part way before: a
/// render began over STOPPED_AFTER_MS ago and never finished, so it took
/// its instance with it. The mark is in shared data, which outlives an
/// instance, so the new instance sees it.
fn stopped_before(f: &Found, now: f64) -> bool {
    let Ok((Some(v), _)) = hostcalls::get_shared_data(&format!("try|{}", f.file)) else {
        return false;
    };
    let v = String::from_utf8_lossy(&v);
    let mut it = v.split(' ');
    let (Some(size), Some(mtime), Some(at)) = (it.next(), it.next(), it.next()) else {
        return false;
    };
    size == f.size.to_string()
        && mtime == f.mtime.to_string()
        && at
            .parse::<f64>()
            .is_ok_and(|at| now - at > STOPPED_AFTER_MS)
}

/// Marks a render begun (Some(now)) or finished (None).
fn mark(f: &Found, began: Option<f64>) {
    let v = began
        .map(|t| format!("{} {} {t:.0}", f.size, f.mtime))
        .unwrap_or_default();
    let _ = hostcalls::set_shared_data(&format!("try|{}", f.file), Some(v.as_bytes()), None);
}

fn millis(ctx: &dyn Context) -> f64 {
    ctx.get_current_time()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs_f64() * 1000.0)
        .unwrap_or(0.0)
}

struct State {
    cfg: Rc<Config>,
    tpl: Template,
    /// The template file's size and time of change when it was read.
    tpl_stat: Option<(u64, u64)>,
    /// Those of a version that was refused, so it isn't read again.
    tpl_refused: Option<Option<(u64, u64)>>,
    /// Goes up each time the template changes.
    tpl_gen: u64,
    cache: HashMap<String, Entry>,
    bytes: u64,
    clock: u64,
    jobs: VecDeque<Job>,
    ticking: bool,
    metrics: Metrics,
}

impl State {
    /// Reads the template again when its file changed. One that's gone or
    /// broken leaves the one before in place, with a note once.
    fn refresh_template(&mut self) {
        let Some((file, _)) = self.cfg.template.clone() else {
            return;
        };
        let stat = kit::stat_file(&file);
        if stat == self.tpl_stat || self.tpl_refused == Some(stat) {
            return;
        }
        let made = match stat {
            None => Err(format!("the template {file} is gone")),
            Some(_) => load_template(&file),
        };
        match made {
            Ok(t) => {
                (self.tpl, self.tpl_stat, self.tpl_refused) = (t, stat, None);
                self.tpl_gen += 1;
                kit::note(&format!("read the template {file} again, as it changed"));
            }
            Err(e) => {
                self.tpl_refused = Some(stat);
                kit::note(&format!("{e}, so the one before stays"));
            }
        }
    }

    /// The answer to a request whose file was found. Without render (in
    /// a request's own call), a page that isn't rendered yet is
    /// Step::Later.
    fn answer(&mut self, f: &Found, ask: &Ask, now: &dyn Fn() -> f64, render: bool) -> Step {
        let cfg = self.cfg.clone();
        if f.size > cfg.largest {
            return Step::Send(refusal(
                TOO_LARGE,
                format!(
                    "{} is {}, over largest {}, so it wasn't rendered: raise largest (and memory on the plugin line)",
                    f.file,
                    kit::bytes(f.size),
                    kit::bytes(cfg.largest)
                ),
            ));
        }
        let tag = cfg.etag(&f.file, f.size, f.mtime, self.tpl_stat, ask.markdown);
        let mut headers = vec![("etag", tag.clone())];
        if cfg.accept {
            headers.push(("vary", "Accept".into()));
        }
        if !ask.none_match.is_empty() && config::none_match(&ask.none_match, &tag) {
            return Step::Send(Reply {
                status: 304,
                headers,
                body: None,
                note: format!("{} hasn't changed since the client's copy (ETag)", f.file),
                metric: None,
            });
        }
        let fresh = self.cache.get(&f.key).is_some_and(|e| {
            e.file == f.file && e.size == f.size && e.mtime == f.mtime && e.tpl == self.tpl_gen
        });
        let (html, source, mut note) = if fresh {
            self.clock += 1;
            let e = self.cache.get_mut(&f.key).expect("fresh");
            e.used = self.clock;
            (
                e.html.clone(),
                e.source.clone(),
                format!("{} as rendered before", f.file),
            )
        } else {
            if !render {
                return Step::Later;
            }
            if stopped_before(f, now()) {
                return Step::Send(refusal(
                    "This page can't be shown.",
                    format!(
                        "rendering {} stopped part way before, out of time or memory, so it isn't tried again until the file changes",
                        f.file
                    ),
                ));
            }
            let src = match read(&f.file) {
                Read::Ok(b) => b,
                Read::Missing => {
                    return Step::GoOn(format!(
                        "{} went away before it was read, so the request went on",
                        f.file
                    ));
                }
                Read::TooBig => {
                    return Step::Send(refusal(
                        TOO_LARGE,
                        format!(
                            "can't read {}: it's larger than the plugin line's body-limit; raise body-limit",
                            f.file
                        ),
                    ));
                }
            };
            let t0 = now();
            mark(f, Some(t0));
            let page = render::render(
                &String::from_utf8_lossy(&src),
                &render::Opts {
                    html: cfg.html,
                    links: cfg.links,
                    index: &cfg.index,
                },
            );
            let at = At {
                file: &f.file,
                path: &f.key,
                site: &ask.site,
            };
            let html = Rc::new(cfg.page(&self.tpl, &page, &at).into_bytes());
            let ms = now() - t0;
            mark(f, None);
            bump(self.metrics.rendered);
            let source = Rc::new(src);
            self.store(f, html.clone(), source.clone());
            let note = format!("rendered {} ({}) in {ms:.1} ms", f.file, kit::bytes(f.size));
            (html, source, note)
        };
        if ask.markdown {
            headers.insert(0, ("content-type", "text/markdown; charset=utf-8".into()));
            note.push_str("; sent the Markdown, as the client asked for text/markdown");
            return Step::Send(Reply {
                status: 200,
                headers,
                body: Some(source),
                note,
                metric: self.metrics.markdown,
            });
        }
        let at = At {
            file: &f.file,
            path: &f.key,
            site: &ask.site,
        };
        headers.insert(0, ("content-type", "text/html; charset=utf-8".into()));
        headers.push((
            "link",
            format!(
                "<{}>; rel=\"alternate\"; type=\"text/markdown\"",
                config::source_url(&at)
            ),
        ));
        Step::Send(Reply {
            status: 200,
            headers,
            body: Some(html),
            note,
            metric: self.metrics.pages,
        })
    }

    /// Keeps a page, letting the least recently used go when the pages
    /// would take more than the cache setting.
    fn store(&mut self, f: &Found, html: Rc<Vec<u8>>, source: Rc<Vec<u8>>) {
        if let Some(old) = self.cache.remove(&f.key) {
            self.bytes -= old.cost(&f.key);
        }
        self.clock += 1;
        let e = Entry {
            file: f.file.clone(),
            size: f.size,
            mtime: f.mtime,
            tpl: self.tpl_gen,
            html,
            source,
            used: self.clock,
        };
        let cost = e.cost(&f.key);
        if cost > self.cfg.cache {
            return;
        }
        while self.bytes + cost > self.cfg.cache {
            let Some(oldest) = self
                .cache
                .iter()
                .min_by_key(|(_, e)| e.used)
                .map(|(k, _)| k.clone())
            else {
                break;
            };
            if let Some(gone) = self.cache.remove(&oldest) {
                self.bytes -= gone.cost(&oldest);
            }
        }
        self.bytes += cost;
        self.cache.insert(f.key.clone(), e);
    }
}

fn load_template(file: &str) -> Result<Template, String> {
    let raw = kit::read_file(file)?;
    let text =
        String::from_utf8(raw).map_err(|_| format!("the template {file} isn't UTF-8 text"))?;
    Template::parse(&text).map_err(|e| format!("the template {file} {e}"))
}

/// The first of the files that exists, as stat finds it.
fn find(key: &str, files: &[String]) -> Option<Found> {
    files.iter().find_map(|f| {
        kit::stat_file(f).map(|(size, mtime)| Found {
            key: key.to_string(),
            file: f.clone(),
            size,
            mtime,
        })
    })
}

/// Whether a miss deserves a note: a folder's URL, or a page's name with no
/// dot in it (so not /style.css or /logo.png).
fn worth_a_note(key: &str) -> bool {
    key.ends_with('/') || !key.rsplit('/').next().unwrap_or("").contains('.')
}

/// "a", "a or b", "a, b or c".
fn or_list(items: &[String]) -> String {
    match items {
        [] => String::new(),
        [one] => one.clone(),
        [rest @ .., last] => format!("{} or {last}", rest.join(", ")),
    }
}

struct Root {
    st: Option<Rc<RefCell<State>>>,
}

impl Context for Root {}

impl RootContext for Root {
    fn on_configure(&mut self, _: usize) -> bool {
        let text = self.get_plugin_configuration().unwrap_or_default();
        let made = Config::parse(&String::from_utf8_lossy(&text)).and_then(|cfg| {
            let (tpl, stat) = match &cfg.template {
                None => (Template::parse(config::DEFAULT_TEMPLATE)?, None),
                Some((file, line)) => (
                    load_template(file).map_err(|e| format!("line {line}: {e}"))?,
                    kit::stat_file(file),
                ),
            };
            Ok((cfg, tpl, stat))
        });
        let (cfg, tpl, tpl_stat) = match made {
            Ok(x) => x,
            Err(e) => {
                kit::note(&e);
                return false;
            }
        };
        self.st = Some(Rc::new(RefCell::new(State {
            cfg: Rc::new(cfg),
            tpl,
            tpl_stat,
            tpl_refused: None,
            tpl_gen: 0,
            cache: HashMap::new(),
            bytes: 0,
            clock: 0,
            jobs: VecDeque::new(),
            ticking: false,
            metrics: Metrics {
                pages: hostcalls::define_metric(MetricType::Counter, "pages").ok(),
                rendered: hostcalls::define_metric(MetricType::Counter, "rendered").ok(),
                markdown: hostcalls::define_metric(MetricType::Counter, "markdown").ok(),
            },
        })));
        true
    }

    /// Renders the pages requests are waiting for, for up to TICK_BUDGET_MS,
    /// and stops the timer when none are left.
    fn on_tick(&mut self) {
        let Some(st) = self.st.clone() else { return };
        let start = millis(self);
        while millis(self) - start < TICK_BUDGET_MS {
            let Some(job) = st.borrow_mut().jobs.pop_front() else {
                break;
            };
            // A request that's gone (the client left) has no context now.
            if hostcalls::set_effective_context(job.ctx).is_err() {
                continue;
            }
            let step = st
                .borrow_mut()
                .answer(&job.found, &job.ask, &|| millis(self), true);
            match step {
                Step::Send(r) => send(&r),
                Step::GoOn(note) => {
                    kit::note(&note);
                    let _ = hostcalls::resume_http_request();
                }
                Step::Later => {
                    let _ = hostcalls::resume_http_request();
                }
            }
        }
        let mut s = st.borrow_mut();
        if s.jobs.is_empty() {
            s.ticking = false;
            drop(s);
            self.set_tick_period(Duration::ZERO);
        }
    }

    fn create_http_context(&self, id: u32) -> Option<Box<dyn HttpContext>> {
        Some(Box::new(Http {
            id,
            st: self.st.clone(),
        }))
    }

    fn get_type(&self) -> Option<ContextType> {
        Some(ContextType::HttpContext)
    }
}

struct Http {
    id: u32,
    st: Option<Rc<RefCell<State>>>,
}

impl Context for Http {}

impl HttpContext for Http {
    fn on_http_request_headers(&mut self, _: usize, _: bool) -> Action {
        let Some(st) = self.st.clone() else {
            return Action::Continue;
        };
        let method = self.get_http_request_header(":method").unwrap_or_default();
        let raw = kit::property(self, &["request", "url_path"]);
        let mut s = st.borrow_mut();
        let Lookup::Files { key, files } = s.cfg.lookup(&method, &raw) else {
            return Action::Continue;
        };
        s.refresh_template();
        let Some(found) = find(&key, &files) else {
            if worth_a_note(&key) {
                kit::note(&format!(
                    "no {} in the folders the plugin line names with read, so the request went on",
                    or_list(&files)
                ));
            }
            return Action::Continue;
        };
        let accept = self.get_http_request_header("accept").unwrap_or_default();
        let ask = Ask {
            none_match: self
                .get_http_request_header("if-none-match")
                .unwrap_or_default(),
            markdown: s.cfg.accept && config::wants_markdown(&accept),
            site: kit::property(self, &["bareproxy", "site"]),
        };
        match s.answer(&found, &ask, &|| millis(self), false) {
            Step::Send(r) => {
                drop(s);
                send(&r);
                Action::Pause
            }
            Step::GoOn(note) => {
                kit::note(&note);
                Action::Continue
            }
            Step::Later => {
                s.jobs.push_back(Job {
                    ctx: self.id,
                    found,
                    ask,
                });
                if !s.ticking {
                    s.ticking = true;
                    let _ = hostcalls::set_tick_period(Duration::from_millis(1));
                }
                Action::Pause
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn notes_read_well() {
        assert_eq!(or_list(&["a.md".into()]), "a.md");
        assert_eq!(
            or_list(&["g/index.md".into(), "g/README.md".into()]),
            "g/index.md or g/README.md"
        );
        assert_eq!(or_list(&["a".into(), "b".into(), "c".into()]), "a, b or c");
        assert!(worth_a_note("/guide/") && worth_a_note("/guide/setup") && worth_a_note("/"));
        assert!(!worth_a_note("/style.css") && !worth_a_note("/img/logo.png"));
    }
}
