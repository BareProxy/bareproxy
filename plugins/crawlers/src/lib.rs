// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! BareProxy's AI crawler control plugin. It finds the AI bot a request
//! names in its User-Agent, checks the claim against the address list the
//! bot's operator publishes (fetched and refreshed on a timer, shared by the
//! plugin's instances, kept in the store across restarts), and lets the bot
//! in, turns it away or holds it to a rate, by bot, group and path. A
//! request that names a bot from an address outside its list is a fake.
//! Every bot request is noted in its record and counted, and a report page
//! shows who came and what they read. README.md is the manual.

pub mod bots;
pub mod config;

use config::{Act, Check, Config, Loaded, Url};
use proxy_wasm::hostcalls;
use proxy_wasm::traits::*;
use proxy_wasm::types::*;
use std::cell::RefCell;
use std::collections::HashMap;
use std::rc::Rc;
use std::time::{Duration, UNIX_EPOCH};

/// Ticks come every second. Every SHARE_EVERY ticks an instance shares
/// the reads it counted, so the report can be that far behind.
const SHARE_EVERY: u64 = 5;
/// How often, in ticks, the files ranges lines name are checked.
const FILES_EVERY: u64 = 30;
/// How long one instance holds a list's fetch before another may try.
const LEASE: u64 = 90;
/// The wait after a failed fetch; it doubles with each failure, up to the
/// refresh time.
const RETRY: u64 = 600;
/// Redirects followed for a list, on its own host.
const HOPS: u8 = 3;
/// The most an instance shares of its reads, under BareProxy's 1 MB for a
/// shared value.
const MAX_SHARED: usize = 1 << 20;

proxy_wasm::main! {{
    proxy_wasm::set_log_level(LogLevel::Warn);
    proxy_wasm::set_root_context(|_| -> Box<dyn RootContext> { Box::new(Root { st: None }) });
}}

/// A list's state in one instance, besides its addresses.
#[derive(Default)]
struct ListState {
    /// A file's bytes as last read, and the last ones refused.
    raw: Vec<u8>,
    refused: Option<Vec<u8>>,
    /// Whether a fetch this instance started hasn't been answered.
    busy: bool,
    /// Why the last fetch failed, as the instances share it, for the report.
    error: String,
}

/// An answer to a fetch. It's read on the next tick, which gets at least
/// a second, rather than in the callback, which gets the request timeout.
struct Answer {
    list: usize,
    url: String,
    hops: u8,
    /// 0 when the call failed or timed out.
    status: u32,
    location: String,
    body: Vec<u8>,
}

#[derive(Clone, Copy, Default)]
struct Metrics {
    bots: Option<u32>,
    fakes: Option<u32>,
    blocked: Option<u32>,
    limited: Option<u32>,
}

struct State {
    cfg: Rc<Config>,
    /// Each list's addresses, as Config::check takes them.
    loaded: Vec<Option<Rc<Loaded>>>,
    lists: Vec<ListState>,
    /// Fetches in flight, by token: the list, the URL and the redirects
    /// followed so far.
    calls: HashMap<u32, (usize, String, u8)>,
    answers: Vec<Answer>,
    /// Reads let in, by site and bot, then path segment.
    reads: HashMap<String, HashMap<String, u64>>,
    /// Bot requests by day, site, bot and answer.
    tally: config::Tally,
    dirty: bool,
    ticks: u64,
    id: u64,
    metrics: Metrics,
}

/// A fetched list's state, shared by the instances as "at fails when
/// error": when it's next due, how many fetches in a row failed, when the
/// shared copy was fetched (so an instance reads the copy only when it
/// changed), and the last failure.
#[derive(Default)]
struct Due {
    at: u64,
    fails: u32,
    when: u64,
    error: String,
}

impl Due {
    fn read(ctx: &dyn Context, key: &str) -> (Due, Option<u32>) {
        let (val, cas) = ctx.get_shared_data(key);
        let text = val
            .map(|v| String::from_utf8_lossy(&v).into_owned())
            .unwrap_or_default();
        let mut f = text.splitn(4, ' ');
        let mut num = || f.next().and_then(|x| x.parse::<u64>().ok()).unwrap_or(0);
        let (at, fails, when) = (num(), num() as u32, num());
        let d = Due {
            at,
            fails,
            when,
            error: f.next().unwrap_or("").to_string(),
        };
        (d, cas)
    }

    fn write(&self, ctx: &dyn Context, key: &str, cas: Option<u32>) -> bool {
        let text = format!("{} {} {} {}", self.at, self.fails, self.when, self.error);
        ctx.set_shared_data(key, Some(text.as_bytes()), cas).is_ok()
    }
}

fn secs(ctx: &dyn Context) -> u64 {
    ctx.get_current_time()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

/// Adds one to a shared-data counter and returns the new value.
fn bump(ctx: &dyn Context, key: &str) -> u64 {
    loop {
        let (val, cas) = ctx.get_shared_data(key);
        let n = val
            .and_then(|v| String::from_utf8(v).ok())
            .and_then(|s| s.parse::<u64>().ok())
            .unwrap_or(0)
            + 1;
        if ctx
            .set_shared_data(key, Some(n.to_string().as_bytes()), cas)
            .is_ok()
        {
            return n;
        }
    }
}

fn parse_bytes(raw: &[u8]) -> Result<Vec<kit::Net>, String> {
    std::str::from_utf8(raw)
        .map_err(|_| "isn't text".to_string())
        .and_then(config::parse_list)
}

fn list_key(at: &str) -> String {
    format!("list|{at}")
}

fn due_key(at: &str) -> String {
    format!("due|{at}")
}

/// Takes the shared copy of a fetched list when it's newer than the
/// instance's.
fn adopt(ctx: &dyn Context, st: &mut State, i: usize, when: u64) {
    if when == 0 || st.loaded[i].as_ref().is_some_and(|l| l.when >= when) {
        return;
    }
    if let Some(v) = ctx.get_shared_data(&list_key(&st.cfg.lists[i].at)).0
        && let Some(l) = config::decode_list(&String::from_utf8_lossy(&v))
    {
        st.loaded[i] = Some(Rc::new(l));
    }
}

struct Root {
    st: Option<Rc<RefCell<State>>>,
}

impl Root {
    /// Takes a fetched list's newer copy from the other instances, and
    /// starts a fetch when it's due and no other instance holds it.
    fn tend(&self, s: &mut State, i: usize, now: u64) {
        let key = due_key(&s.cfg.lists[i].at);
        let (d, cas) = Due::read(self, &key);
        adopt(self, s, i, d.when);
        s.lists[i].error.clone_from(&d.error);
        if s.lists[i].busy || now < d.at || cas.is_none() {
            return;
        }
        let lease = Due {
            at: now + LEASE,
            ..d
        };
        if !lease.write(self, &key, cas) {
            return; // another instance took it first
        }
        let at = s.cfg.lists[i].at.clone();
        self.fetch(s, i, &at, 0, now);
    }

    fn fetch(&self, s: &mut State, i: usize, url: &str, hops: u8, now: u64) {
        let Some(u) = Url::parse(url) else {
            return self.failed(
                s,
                i,
                now,
                format!("{url} isn't a URL BareProxy can fetch"),
                true,
            );
        };
        let ua = format!(
            "BareProxy-crawlers/{} (+https://bareproxy.com)",
            env!("CARGO_PKG_VERSION")
        );
        let auth = u.authority();
        let headers = vec![
            (":method", "GET"),
            (":path", u.path.as_str()),
            (":authority", auth.as_str()),
            (":scheme", u.scheme.as_str()),
            ("user-agent", ua.as_str()),
            ("accept", "application/json, text/plain, */*"),
        ];
        match self.dispatch_http_call(
            &u.upstream(),
            headers,
            None,
            vec![],
            Duration::from_secs(30),
        ) {
            Ok(token) => {
                s.calls.insert(token, (i, url.to_string(), hops));
                s.lists[i].busy = true;
            }
            Err(_) => self.failed(
                s,
                i,
                now,
                format!("{} isn't on the plugin's allow-http line", u.upstream()),
                true,
            ),
        }
    }

    /// Records a failed fetch for every instance, and when it can be tried
    /// again: after the refresh time when trying sooner can't help, sooner
    /// otherwise. A reason is logged once, not on every try.
    fn failed(&self, s: &mut State, i: usize, now: u64, why: String, long: bool) {
        s.lists[i].busy = false;
        let def = &s.cfg.lists[i];
        let key = due_key(&def.at);
        let (d, _) = Due::read(self, &key);
        let fails = d.fails.saturating_add(1);
        let wait = if long {
            s.cfg.refresh
        } else {
            (RETRY << (fails - 1).min(8)).min(s.cfg.refresh)
        };
        if d.error != why {
            let names: Vec<&str> = def
                .bots
                .iter()
                .map(|b| s.cfg.bots[*b].name.as_str())
                .collect();
            let keep = match &s.loaded[i] {
                Some(l) => format!("the copy from {} stays", kit::utc(l.when)),
                None => format!(
                    "claims to be {} aren't checked until it loads",
                    names.join(" or ")
                ),
            };
            kit::note(&format!(
                "can't load {} from {}: {why}; {keep}",
                def.label, def.at
            ));
        }
        s.lists[i].error.clone_from(&why);
        Due {
            at: now + wait,
            fails,
            when: d.when,
            error: why,
        }
        .write(self, &key, None);
    }

    /// Reads the answer to a fetch.
    fn answer(&self, s: &mut State, a: Answer, now: u64) {
        let i = a.list;
        match a.status {
            200 => match parse_bytes(&a.body) {
                Ok(nets) => {
                    let def = s.cfg.lists[i].clone();
                    let changed = s.loaded[i].as_ref().is_none_or(|l| l.nets != nets);
                    let n = nets.len();
                    let l = Loaded::new(nets, now);
                    let text = config::encode_list(&l);
                    let key = list_key(&def.at);
                    let _ = self.set_shared_data(&key, Some(text.as_bytes()), None);
                    kit::store_put(&key, text.as_bytes());
                    Due {
                        at: now + s.cfg.refresh,
                        when: now,
                        ..Due::default()
                    }
                    .write(self, &due_key(&def.at), None);
                    s.lists[i].busy = false;
                    s.lists[i].error.clear();
                    s.loaded[i] = Some(Rc::new(l));
                    if changed {
                        let names: Vec<&str> =
                            def.bots.iter().map(|b| s.cfg.bots[*b].name.as_str()).collect();
                        kit::note(&format!(
                            "loaded {} ({}): {}",
                            def.label,
                            names.join(", "),
                            config::plural(n, "address range")
                        ));
                    }
                }
                Err(e) if e.starts_with("line ") => {
                    self.failed(s, i, now, format!("the list's {e}"), false)
                }
                Err(e) => self.failed(s, i, now, format!("the list {e}"), false),
            },
            301 | 302 | 303 | 307 | 308 => match config::resolve(&a.url, &a.location) {
                Some(next) if a.hops < HOPS && config::same_host(&a.url, &next) => {
                    self.fetch(s, i, &next, a.hops + 1, now)
                }
                Some(next) => self.failed(
                    s,
                    i,
                    now,
                    format!("it moved to {next}; give its bots that URL with a ranges line, and allow its host"),
                    true,
                ),
                None => self.failed(s, i, now, format!("answered {} with no Location", a.status), false),
            },
            0 => self.failed(s, i, now, "no answer: the connection failed or timed out".into(), false),
            st => self.failed(s, i, now, format!("answered {st}"), false),
        }
    }

    /// Reads a file a ranges line names again when it changed. A broken
    /// file is refused, once in the log, and the list as it was stays.
    fn reload_file(&self, s: &mut State, i: usize, now: u64) {
        let at = s.cfg.lists[i].at.clone();
        let l = &mut s.lists[i];
        match kit::read_file(&at) {
            Ok(raw) if raw == l.raw => {}
            Ok(raw) => match parse_bytes(&raw) {
                Ok(nets) => {
                    kit::note(&format!(
                        "reloaded {at}: {}",
                        config::plural(nets.len(), "address range")
                    ));
                    s.loaded[i] = Some(Rc::new(Loaded::new(nets, now)));
                    l.raw = raw;
                    l.refused = None;
                }
                Err(e) => {
                    if l.refused.as_ref() != Some(&raw) {
                        kit::note(&format!(
                            "{at} wasn't reloaded, the list as it was stays: {e}"
                        ));
                        l.refused = Some(raw);
                    }
                }
            },
            Err(e) => {
                if l.refused.as_deref() != Some(b"") {
                    kit::note(&format!("{e}; the list as it was stays"));
                    l.refused = Some(Vec::new());
                }
            }
        }
    }
}

impl Context for Root {
    fn on_http_call_response(&mut self, token: u32, _: usize, body_size: usize, _: usize) {
        let Some(st) = self.st.clone() else { return };
        let mut s = st.borrow_mut();
        let Some((list, url, hops)) = s.calls.remove(&token) else {
            return;
        };
        let status = self
            .get_http_call_response_header(":status")
            .and_then(|v| v.parse().ok())
            .unwrap_or(0);
        let location = self
            .get_http_call_response_header("location")
            .unwrap_or_default();
        let body = if status == 200 {
            self.get_http_call_response_body(0, body_size)
                .unwrap_or_default()
        } else {
            Vec::new()
        };
        s.answers.push(Answer {
            list,
            url,
            hops,
            status,
            location,
            body,
        });
    }
}

impl RootContext for Root {
    fn on_configure(&mut self, _: usize) -> bool {
        let text = self.get_plugin_configuration().unwrap_or_default();
        let cfg = match Config::parse(&String::from_utf8_lossy(&text)) {
            Ok(c) => c,
            Err(e) => {
                kit::note(&e);
                return false;
            }
        };
        let now = secs(self);
        let n = cfg.lists.len();
        let mut st = State {
            cfg: Rc::new(cfg),
            loaded: vec![None; n],
            lists: (0..n).map(|_| ListState::default()).collect(),
            calls: HashMap::new(),
            answers: Vec::new(),
            reads: HashMap::new(),
            tally: config::Tally::new(),
            dirty: false,
            ticks: 0,
            id: 0,
            metrics: Metrics::default(),
        };
        for i in 0..n {
            let def = st.cfg.lists[i].clone();
            if !def.url {
                // A file is read now, so a missing or broken one stops
                // check and plan with the reason.
                let raw = match kit::read_file(&def.at) {
                    Ok(b) => b,
                    Err(e) => {
                        kit::note(&e);
                        return false;
                    }
                };
                match parse_bytes(&raw) {
                    Ok(nets) => st.loaded[i] = Some(Rc::new(Loaded::new(nets, now))),
                    Err(e) => {
                        kit::note(&format!("{} {e}", def.at));
                        return false;
                    }
                }
                st.lists[i].raw = raw;
                continue;
            }
            // A list fetched before: the instances' shared copy, or after a
            // restart the store's, which isn't due again until its refresh
            // time. Fetches wait for the first tick: check and plan start
            // the plugin with no outgoing calls.
            let (lk, dk) = (list_key(&def.at), due_key(&def.at));
            if self.get_shared_data(&dk).0.is_none() {
                let mut d = Due::default();
                if let Some(b) = kit::store_get(&lk)
                    && let Some(l) = config::decode_list(&String::from_utf8_lossy(&b))
                {
                    let _ = self.set_shared_data(&lk, Some(&b), None);
                    (d.at, d.when) = (l.when + st.cfg.refresh, l.when);
                }
                d.write(self, &dk, None);
            }
            let (d, _) = Due::read(self, &dk);
            adopt(self, &mut st, i, d.when);
        }
        st.id = bump(self, "instances");
        st.metrics = Metrics {
            bots: hostcalls::define_metric(MetricType::Counter, "bots").ok(),
            fakes: hostcalls::define_metric(MetricType::Counter, "fakes").ok(),
            blocked: hostcalls::define_metric(MetricType::Counter, "blocked").ok(),
            limited: hostcalls::define_metric(MetricType::Counter, "limited").ok(),
        };
        self.set_tick_period(Duration::from_secs(1));
        self.st = Some(Rc::new(RefCell::new(st)));
        true
    }

    fn on_tick(&mut self) {
        let Some(st) = self.st.clone() else { return };
        let mut s = st.borrow_mut();
        s.ticks += 1;
        let now = secs(self);
        for a in std::mem::take(&mut s.answers) {
            self.answer(&mut s, a, now);
        }
        for i in 0..s.cfg.lists.len() {
            if s.cfg.lists[i].url {
                self.tend(&mut s, i, now);
            } else if s.ticks % FILES_EVERY == 0 {
                self.reload_file(&mut s, i, now);
            }
        }
        if s.dirty && s.ticks % SHARE_EVERY == 0 {
            let text = config::encode_reads(&s.reads, MAX_SHARED);
            let _ = self.set_shared_data(&format!("reads/{}", s.id), Some(text.as_bytes()), None);
            config::prune(&mut s.tally, now / 86400);
            let text = config::encode_tally(&s.tally);
            let _ = self.set_shared_data(&format!("days/{}", s.id), Some(text.as_bytes()), None);
            s.dirty = false;
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

impl Http {
    fn refuse(
        &self,
        status: u32,
        extra: &[(&str, &str)],
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
        h.extend_from_slice(extra);
        self.send_http_response(status, h, Some(body.as_bytes()));
        Action::Pause
    }

    /// The report page, for the site it's asked on.
    fn report(&self, st: &RefCell<State>, cfg: &Config, site: &str) -> Action {
        let s = st.borrow();
        let now = secs(self);
        let today = now / 86400;
        let mut rows = vec![];
        let mut reads = HashMap::new();
        config::add_tally(cfg, &mut rows, &config::encode_tally(&s.tally), site, today);
        config::add_reads(
            &mut reads,
            &config::encode_reads(&s.reads, usize::MAX),
            site,
        );
        let n = self
            .get_shared_data("instances")
            .0
            .and_then(|v| String::from_utf8(v).ok())
            .and_then(|v| v.parse::<u64>().ok())
            .unwrap_or(0);
        for id in (1..=n).filter(|id| *id != s.id) {
            if let Some(v) = self.get_shared_data(&format!("days/{id}")).0 {
                config::add_tally(cfg, &mut rows, &String::from_utf8_lossy(&v), site, today);
            }
            if let Some(v) = self.get_shared_data(&format!("reads/{id}")).0 {
                config::add_reads(&mut reads, &String::from_utf8_lossy(&v), site);
            }
        }
        let lists = cfg
            .lists
            .iter()
            .enumerate()
            .map(|(i, l)| {
                let mut ls = config::ListStatus::default();
                if let Some(x) = &s.loaded[i] {
                    ls.ranges = x.nets.len();
                    ls.when = x.when;
                }
                if l.url {
                    ls.error.clone_from(&s.lists[i].error);
                }
                ls
            })
            .collect();
        let body = cfg.report(&config::Report {
            site,
            now,
            rows,
            reads,
            lists,
            every: SHARE_EVERY,
        });
        kit::note("the crawler report went out");
        self.send_http_response(
            200,
            vec![
                ("content-type", "text/plain; charset=utf-8"),
                ("cache-control", "no-store"),
            ],
            Some(body.as_bytes()),
        );
        Action::Pause
    }
}

/// Counts a request for the report, by day, site, bot and answer.
fn tally(st: &RefCell<State>, now: u64, site: &str, bot: &str, answer: usize) {
    let mut s = st.borrow_mut();
    config::tally(&mut s.tally, now / 86400, site, bot, answer);
    s.dirty = true;
}

impl HttpContext for Http {
    fn on_http_request_headers(&mut self, _: usize, _: bool) -> Action {
        let Some(st) = self.st.clone() else {
            return Action::Continue;
        };
        let cfg = st.borrow().cfg.clone();
        let headers = self.get_http_request_headers();
        let get = |n: &str| {
            headers
                .iter()
                .find(|(k, _)| k == n)
                .map(|(_, v)| v.as_str())
        };
        let check_header = cfg.header.as_ref().map(|h| format!("{h}-check"));
        if let (Some(h), Some(hc)) = (&cfg.header, &check_header) {
            // Only the plugin sets these; a client's own are dropped.
            if get(h).is_some() {
                self.set_http_request_header(h, None);
            }
            if get(hc).is_some() {
                self.set_http_request_header(hc, None);
            }
        }
        let bot = cfg.find(get("user-agent").unwrap_or(""));
        if bot.is_none() && cfg.report.is_none() {
            return Action::Continue;
        }
        let path = kit::property(self, &["request", "url_path"]);
        let is_report = cfg.report.as_deref() == Some(path.as_str());
        if bot.is_none() && !is_report {
            return Action::Continue;
        }
        let source = kit::property(self, &["source", "address"]);
        let ip = kit::client_addr(&source, get("x-forwarded-for"), &cfg.forwarded_from);
        let site = kit::property(self, &["bareproxy", "site"]);
        if is_report && ip.is_some_and(|ip| cfg.report_allow.iter().any(|n| n.contains(ip))) {
            return self.report(&st, &cfg, &site);
        }
        let Some(b) = bot else {
            return Action::Continue;
        };
        let name = cfg.bots[b].name.as_str();
        let who = cfg.who(b);
        if path == "/robots.txt" {
            kit::note(&format!(
                "{who}: /robots.txt is open to every bot, so it can read the site's rules"
            ));
            return Action::Continue;
        }
        let metrics = st.borrow().metrics;
        let now = secs(self);
        if let Some(m) = metrics.bots {
            let _ = hostcalls::increment_metric(m, 1);
        }
        let addr = ip.map(|i| i.to_string()).unwrap_or(source);
        let mut check = cfg.check(b, ip, &st.borrow().loaded);
        if cfg.unverified_fake
            && let Check::Unverified(why) = &check
        {
            check = Check::Fake(format!(
                "{addr} not verified, {why}, and claims that can't be checked count as fakes"
            ));
        }
        let how = match &check {
            Check::Fake(why) => {
                if let Some(m) = metrics.fakes {
                    let _ = hostcalls::increment_metric(m, 1);
                }
                tally(&st, now, &site, name, config::FAKE);
                if cfg.fake_block {
                    return self.refuse(
                        cfg.status,
                        &[],
                        "Forbidden.\n",
                        &format!("fake {name}: {why}; turned away (fake block)"),
                        None,
                    );
                }
                if let Some(hc) = &check_header {
                    self.set_http_request_header(hc, Some("fake"));
                }
                kit::note(&format!(
                    "fake {name}: {why}; let through like any visitor (fake allow)"
                ));
                return Action::Continue;
            }
            Check::Verified(t) => t.clone(),
            Check::Unverified(why) => format!("{addr} not verified, {why}"),
        };
        let rule = cfg.rule(b, &path);
        let mut extra = String::new();
        match rule.map(|r| r.act) {
            Some(Act::Block) => {
                tally(&st, now, &site, name, config::BLOCKED);
                return self.refuse(
                    cfg.status,
                    &[],
                    &format!("This site doesn't allow {name}.\n"),
                    &format!("{who}: {how}; turned away by line {}", rule.unwrap().line),
                    metrics.blocked,
                );
            }
            Some(Act::Limit { count, window_ms }) => {
                let line = rule.unwrap().line;
                match kit::count(&format!("l|{site}|{name}|{line}"), window_ms, 1) {
                    Some((cur, prev, into)) => {
                        let n = kit::sliding(cur, prev, into, window_ms);
                        let per = config::short(window_ms as u64);
                        if n > count {
                            tally(&st, now, &site, name, config::LIMITED);
                            let wait = window_ms as u64 - into.min(window_ms as u64);
                            let retry = wait.div_ceil(1000).max(1).to_string();
                            return self.refuse(
                                429,
                                &[("retry-after", retry.as_str())],
                                &format!("Too many requests from {name}. Please slow down.\n"),
                                &format!(
                                    "{who}: {how}; {n} requests in the last {per}, over {count} (line {line}); the window resets in {}",
                                    config::short(wait)
                                ),
                                metrics.limited,
                            );
                        }
                        extra = format!(" ({n} of {count} in the last {per})");
                    }
                    None => extra = " (not counted: BareProxy holds too many counters)".into(),
                }
            }
            Some(Act::Allow) | None => {}
        }
        if let (Some(h), Some(hc)) = (&cfg.header, &check_header) {
            self.set_http_request_header(h, Some(name));
            self.set_http_request_header(hc, Some(check.word()));
        }
        {
            let mut s = st.borrow_mut();
            config::add_read(
                &mut s.reads,
                &format!("{site}\t{name}"),
                config::section(&path),
            );
            s.dirty = true;
        }
        let by = match rule {
            Some(r) => format!("let in by line {}{extra}", r.line),
            None => "let in, no rule names it".to_string(),
        };
        tally(&st, now, &site, name, config::IN);
        let nth = kit::count(&format!("h|{site}|{name}"), config::HOUR_MS, 1)
            .map(|c| format!(", its {} request let in this hour", config::ordinal(c.0)))
            .unwrap_or_default();
        kit::note(&format!("{who}: {how}; {by}{nth}"));
        Action::Continue
    }
}
