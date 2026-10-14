// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! The AI crawler control plugin's config, its bots and their address
//! lists, and its decisions, kept apart from the Proxy-Wasm calls so they
//! can be tested on their own.

use crate::bots::{GROUPS, KNOWN, List};
use kit::{Item, Line, Net};
use std::collections::HashMap;
use std::net::IpAddr;
use std::rc::Rc;

/// An hour in milliseconds: the window a bot's requests are counted in for
/// its notes.
pub const HOUR_MS: u32 = 3_600_000;

/// The most path segments the report keeps per bot and site; reads past
/// them count under "other".
pub const MAX_SECTIONS: usize = 100;

/// Where a bot's addresses come from.
#[derive(Debug, Clone, PartialEq)]
pub enum Source {
    /// Nowhere, so its claims can't be checked; the text says why.
    None(String),
    /// Fixed ranges, from the plugin's table or the config, and how notes
    /// name them.
    Fixed(Vec<Net>, String),
    /// An address list: an index into Config::lists.
    List(usize),
}

#[derive(Debug, Clone, PartialEq)]
pub struct Bot {
    pub name: String,
    /// The name in lower case, as looked for in the User-Agent.
    pub word: String,
    /// "" for a bot the config adds.
    pub operator: String,
    pub group: String,
    pub source: Source,
}

/// An address list, fetched from a URL or read from a file in a folder
/// the plugin line names with read.
#[derive(Debug, Clone, PartialEq)]
pub struct ListDef {
    /// The URL or the file.
    pub at: String,
    pub url: bool,
    /// How notes and the report name it, such as "OpenAI's list".
    pub label: String,
    /// The bots that use it: indexes into Config::bots.
    pub bots: Vec<usize>,
}

#[derive(Debug, Clone, Copy, PartialEq)]
pub enum Act {
    Allow,
    Block,
    /// At most count requests in any window_ms, per bot and site.
    Limit {
        count: u64,
        window_ms: u32,
    },
}

#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Rule {
    pub act: Act,
    pub line: usize,
}

/// The rules at the top of the config, or in a path section.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct Rules {
    pub path: String,
    /// By bot (its index), by group, and for every bot.
    pub bots: HashMap<usize, Rule>,
    pub groups: HashMap<String, Rule>,
    pub all: Option<Rule>,
}

impl Rules {
    fn pick(&self, bot: usize, group: &str) -> Option<Rule> {
        self.bots
            .get(&bot)
            .or_else(|| self.groups.get(group))
            .or(self.all.as_ref())
            .copied()
    }
}

#[derive(Debug, Clone, PartialEq)]
pub struct Config {
    pub bots: Vec<Bot>,
    pub lists: Vec<ListDef>,
    pub top: Rules,
    /// Longest path first.
    pub sections: Vec<Rules>,
    /// Whether a request that names a bot from an address outside the
    /// bot's list is turned away (or let through like anyone else).
    pub fake_block: bool,
    /// Whether a claim that can't be checked counts as a fake.
    pub unverified_fake: bool,
    /// The request header the bot's name goes in, lower case; the check's
    /// result goes in the same name with -check after it.
    pub header: Option<String>,
    pub status: u32,
    /// Seconds between fetches of a list.
    pub refresh: u64,
    pub forwarded_from: Vec<Net>,
    pub report: Option<String>,
    pub report_allow: Vec<Net>,
}

/// What a request that names a bot turns out to be.
#[derive(Debug, Clone, PartialEq)]
pub enum Check {
    /// Its address is in the bot's list; the text says so.
    Verified(String),
    /// Its claim can't be checked; the text says why.
    Unverified(String),
    /// Its address isn't in the bot's list; the text says so.
    Fake(String),
}

impl Check {
    pub fn word(&self) -> &'static str {
        match self {
            Check::Verified(_) => "verified",
            Check::Unverified(_) => "unverified",
            Check::Fake(_) => "fake",
        }
    }
}

/// A list's addresses as loaded, and when (seconds since 1970).
#[derive(Debug, Clone, PartialEq, Default)]
pub struct Loaded {
    pub nets: Vec<Net>,
    pub when: u64,
    pub index: Index,
}

impl Loaded {
    pub fn new(nets: Vec<Net>, when: u64) -> Loaded {
        let index = Index::new(&nets);
        Loaded { nets, when, index }
    }
}

/// Ranges sorted and merged, so a lookup takes a binary search however
/// long the list. Amazon's lists name over a thousand single addresses.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct Index {
    v4: Vec<(u128, u128)>,
    v6: Vec<(u128, u128)>,
}

impl Index {
    pub fn new(nets: &[Net]) -> Index {
        let (mut v4, mut v6) = (vec![], vec![]);
        for n in nets {
            let (is4, a, b) = n.bounds();
            if is4 {
                v4.push((a, b))
            } else {
                v6.push((a, b))
            }
        }
        Index {
            v4: merge(v4),
            v6: merge(v6),
        }
    }

    pub fn contains(&self, ip: IpAddr) -> bool {
        let (list, n) = match ip.to_canonical() {
            IpAddr::V4(a) => (&self.v4, u32::from(a) as u128),
            IpAddr::V6(a) => (&self.v6, u128::from(a)),
        };
        let i = list.partition_point(|r| r.0 <= n);
        i > 0 && list[i - 1].1 >= n
    }
}

fn merge(mut v: Vec<(u128, u128)>) -> Vec<(u128, u128)> {
    v.sort_unstable();
    let mut out: Vec<(u128, u128)> = Vec::with_capacity(v.len());
    for (a, b) in v {
        match out.last_mut() {
            Some(last) if a <= last.1.saturating_add(1) => last.1 = last.1.max(b),
            _ => out.push((a, b)),
        }
    }
    out
}

/// A URL, split.
#[derive(Debug, Clone, PartialEq)]
pub struct Url {
    pub scheme: String,
    pub host: String,
    pub port: u16,
    /// The path and query, from the first /.
    pub path: String,
}

impl Url {
    pub fn parse(s: &str) -> Option<Url> {
        let (scheme, rest) = s.split_once("://")?;
        let scheme = scheme.to_ascii_lowercase();
        let def = match scheme.as_str() {
            "http" => 80,
            "https" => 443,
            _ => return None,
        };
        let (auth, path) = match rest.find(['/', '?']) {
            Some(i) if rest.as_bytes()[i] == b'/' => (&rest[..i], rest[i..].to_string()),
            Some(i) => (&rest[..i], format!("/{}", &rest[i..])),
            None => (rest, "/".to_string()),
        };
        if auth.contains('@') || path.contains(char::is_whitespace) {
            return None;
        }
        let (host, port) = if let Some(v6) = auth.strip_prefix('[') {
            let (h, after) = v6.split_once(']')?;
            let port = match after {
                "" => def,
                p => p.strip_prefix(':')?.parse().ok()?,
            };
            (format!("[{h}]"), port)
        } else {
            match auth.rsplit_once(':') {
                Some((h, p)) => (h.to_string(), p.parse().ok()?),
                None => (auth.to_string(), def),
            }
        };
        let ok = |c: char| c.is_ascii_alphanumeric() || "-._[]:".contains(c);
        if host.is_empty() || !host.chars().all(ok) || port == 0 {
            return None;
        }
        Some(Url {
            scheme,
            host: host.to_ascii_lowercase(),
            port,
            path,
        })
    }

    /// The address for an outgoing call, as allow-http names it.
    pub fn upstream(&self) -> String {
        format!("{}:{}", self.host, self.port)
    }

    /// The host as a Host header gives it: without the port when it's the
    /// scheme's own.
    pub fn authority(&self) -> String {
        match (self.scheme.as_str(), self.port) {
            ("http", 80) | ("https", 443) => self.host.clone(),
            _ => self.upstream(),
        }
    }
}

/// Where a redirect leads: a full URL, or a path on the same host.
pub fn resolve(base: &str, location: &str) -> Option<String> {
    if location.contains("://") {
        return Url::parse(location).map(|_| location.to_string());
    }
    if location.starts_with('/') && !location.starts_with("//") {
        let b = Url::parse(base)?;
        return Some(format!("{}://{}{}", b.scheme, b.authority(), location));
    }
    None
}

/// Whether two URLs reach the same address the same way.
pub fn same_host(a: &str, b: &str) -> bool {
    match (Url::parse(a), Url::parse(b)) {
        (Some(x), Some(y)) => x.scheme == y.scheme && x.upstream() == y.upstream(),
        _ => false,
    }
}

/// Where a bot's addresses come from, as the config says it.
enum Spec {
    Table,
    Off(usize),
    Url(String),
    File(String),
    Fixed(Vec<Net>, usize),
}

/// What a rule names.
enum Target {
    Bot(usize),
    Group(String),
    All,
}

fn name_ok(w: &str) -> bool {
    !w.is_empty()
        && w.len() <= 64
        && w.bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"-_.".contains(&b))
}

fn nets(line: &Line, words: &[String]) -> Result<Vec<Net>, String> {
    words
        .iter()
        .map(|w| {
            Net::parse(w).ok_or_else(|| {
                line.err(format!(
                    "{w} isn't an address or a range such as 192.0.2.0/24"
                ))
            })
        })
        .collect()
}

fn limit(line: &Line) -> Result<(&[String], u64, u32), String> {
    let a = line.args();
    let usage =
        || line.err("write limit NAME... COUNT per TIME, such as limit PerplexityBot 30 per 1m");
    let Some(p) = a.iter().position(|w| w == "per") else {
        return Err(usage());
    };
    if p < 2 || p + 2 != a.len() {
        return Err(usage());
    }
    let count = match a[p - 1].parse::<u64>() {
        Ok(n) if n > 0 => n,
        _ => return Err(line.err(format!("{} isn't a count of requests", a[p - 1]))),
    };
    let secs = kit::seconds(line, &a[p + 1])?;
    if !(1..=86400).contains(&secs) {
        return Err(line.err("a limit's time is 1s to 1d"));
    }
    Ok((&a[..p - 1], count, (secs * 1000) as u32))
}

impl Config {
    pub fn parse(text: &str) -> Result<Config, String> {
        let lines: Vec<Line> = kit::parse(text, &[])?
            .into_iter()
            .map(|i| match i {
                Item::Line(l) | Item::Block(l, _) => l,
            })
            .collect();
        let mut bots: Vec<Bot> = KNOWN
            .iter()
            .map(|k| Bot {
                name: k.name.into(),
                word: k.name.to_ascii_lowercase(),
                operator: k.operator.into(),
                group: k.group.into(),
                source: Source::None(String::new()),
            })
            .collect();
        let mut groups: Vec<String> = GROUPS.iter().map(|(g, _)| g.to_string()).collect();
        let find = |bots: &[Bot], w: &str| bots.iter().position(|b| b.name.eq_ignore_ascii_case(w));

        // Bots the config adds come first, so rules anywhere can name them.
        let mut top = true;
        for line in &lines {
            match line.name() {
                "path" => top = false,
                "bot" => {
                    if !top {
                        return Err(line.err("bot goes at the top, before any path section"));
                    }
                    let [name, group] = line.args() else {
                        return Err(line.err(
                            "bot takes a name, the word its User-Agent has, and a group, such as bot Googlebot search-engine",
                        ));
                    };
                    if !name_ok(name) {
                        return Err(line.err(format!(
                            "{name} isn't a bot's name: letters, digits, - _ and . only"
                        )));
                    }
                    if find(&bots, name).is_some() {
                        return Err(line.err(format!(
                            "{name} is a bot already; to give it addresses, write ranges {name} and a list"
                        )));
                    }
                    let g = group.to_ascii_lowercase();
                    if name.eq_ignore_ascii_case("all")
                        || groups.iter().any(|x| x.eq_ignore_ascii_case(name))
                    {
                        return Err(line.err(format!("{name} is a group's name, not a bot's")));
                    }
                    if g == "all"
                        || find(&bots, &g).is_some()
                        || g.eq_ignore_ascii_case(name)
                        || !g.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'-')
                    {
                        return Err(line.err(format!(
                            "{group} can't be a group: write training, search, assistant or a word of your own"
                        )));
                    }
                    if !groups.contains(&g) {
                        groups.push(g.clone());
                    }
                    bots.push(Bot {
                        name: name.clone(),
                        word: name.to_ascii_lowercase(),
                        operator: String::new(),
                        group: g,
                        source: Source::None(String::new()),
                    });
                }
                _ => {}
            }
        }

        let mut c = Config {
            bots: vec![],
            lists: vec![],
            top: Rules {
                path: "/".into(),
                ..Rules::default()
            },
            sections: vec![],
            fake_block: true,
            unverified_fake: false,
            header: Some("x-bot".into()),
            status: 403,
            refresh: 6 * 3600,
            forwarded_from: vec![],
            report: None,
            report_allow: vec![Net::parse("127.0.0.1").unwrap(), Net::parse("::1").unwrap()],
        };
        let mut specs: Vec<Spec> = bots.iter().map(|_| Spec::Table).collect();
        let mut fetch_off: Option<usize> = None;
        let target = |w: &str| -> Option<Target> {
            if w.eq_ignore_ascii_case("all") {
                Some(Target::All)
            } else if let Some(i) = find(&bots, w) {
                Some(Target::Bot(i))
            } else {
                let g = w.to_ascii_lowercase();
                groups.contains(&g).then_some(Target::Group(g))
            }
        };

        for line in &lines {
            let a = line.args();
            let at_top = c.sections.is_empty();
            let only_top = |what: &str| -> Result<(), String> {
                if at_top {
                    Ok(())
                } else {
                    Err(line.err(format!("{what} goes at the top, before any path section")))
                }
            };
            match line.name() {
                "bot" => {}
                "allow" | "block" | "limit" => {
                    let (names, act) = match line.name() {
                        "allow" => (a, Act::Allow),
                        "block" => (a, Act::Block),
                        _ => {
                            let (n, count, window_ms) = limit(line)?;
                            (n, Act::Limit { count, window_ms })
                        }
                    };
                    if names.is_empty() {
                        return Err(line.err(format!(
                            "{} takes bots or groups, such as {} GPTBot training",
                            line.name(),
                            line.name()
                        )));
                    }
                    let rules = c.sections.last_mut().unwrap_or(&mut c.top);
                    let rule = Rule { act, line: line.no };
                    for w in names {
                        let prev = match target(w) {
                            Some(Target::Bot(i)) => rules.bots.insert(i, rule),
                            Some(Target::Group(g)) => rules.groups.insert(g, rule),
                            Some(Target::All) => rules.all.replace(rule),
                            None => {
                                return Err(line.err(format!(
                                    "{w} isn't a bot the plugin knows or a group ({}, all); add a bot of your own with a bot line",
                                    groups.join(", ")
                                )));
                            }
                        };
                        if let Some(p) = prev {
                            return Err(
                                line.err(format!("{w} has a rule on line {} already", p.line))
                            );
                        }
                    }
                }
                "path" => match a {
                    [p] if p.starts_with('/') => {
                        if c.sections.iter().any(|s| s.path == *p) {
                            return Err(line.err(format!("path {p} is set twice")));
                        }
                        c.sections.push(Rules {
                            path: p.clone(),
                            ..Rules::default()
                        });
                    }
                    _ => return Err(line.err("path takes one path prefix, such as /members/")),
                },
                "fake" => {
                    only_top("fake")?;
                    c.fake_block = match a {
                        [w] if w == "block" => true,
                        [w] if w == "allow" => false,
                        _ => return Err(line.err("fake takes block or allow")),
                    };
                }
                "unverified" => {
                    only_top("unverified")?;
                    c.unverified_fake = match a {
                        [w] if w == "bot" => false,
                        [w] if w == "fake" => true,
                        _ => return Err(line.err("unverified takes bot or fake")),
                    };
                }
                "header" => {
                    only_top("header")?;
                    c.header = match a {
                        [w] if w == "off" => None,
                        [w] if w.len() <= 64
                            && w.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'-')
                            && !w.eq_ignore_ascii_case("host") =>
                        {
                            Some(w.to_ascii_lowercase())
                        }
                        _ => {
                            return Err(
                                line.err("header takes a header name, such as X-Bot, or off")
                            );
                        }
                    };
                }
                "status" => {
                    only_top("status")?;
                    c.status = match a {
                        [w] if ["403", "404", "410", "451"].contains(&w.as_str()) => {
                            w.parse().unwrap()
                        }
                        _ => return Err(line.err("status takes 403, 404, 410 or 451")),
                    };
                }
                "refresh" => {
                    only_top("refresh")?;
                    let [w] = a else {
                        return Err(line.err("refresh takes a time, such as 6h"));
                    };
                    c.refresh = kit::seconds(line, w)?;
                    if c.refresh < 600 {
                        return Err(
                            line.err("refresh is 10m at least; the lists change every few days")
                        );
                    }
                }
                "fetch" => {
                    only_top("fetch")?;
                    if !kit::switch(line)? {
                        fetch_off = Some(line.no);
                    }
                }
                "forwarded-from" | "report-allow" => {
                    only_top(line.name())?;
                    if a.is_empty() {
                        return Err(line.err(format!("{} takes addresses or ranges", line.name())));
                    }
                    let n = nets(line, a)?;
                    if line.name() == "forwarded-from" {
                        c.forwarded_from.extend(n);
                    } else {
                        c.report_allow = n;
                    }
                }
                "report" => {
                    only_top("report")?;
                    match a {
                        [p] if p.starts_with('/') => c.report = Some(p.clone()),
                        _ => return Err(line.err("report takes a path, such as /.crawlers")),
                    }
                }
                "ranges" => {
                    only_top("ranges")?;
                    let Some((name, rest)) = a.split_first().filter(|(_, r)| !r.is_empty()) else {
                        return Err(line.err(
                            "ranges takes a bot and where its addresses are: a URL, a file, addresses, or off",
                        ));
                    };
                    let Some(i) = find(&bots, name) else {
                        return Err(line.err(format!(
                            "{name} isn't a bot the plugin knows; add it with a bot line first"
                        )));
                    };
                    specs[i] = match rest {
                        [w] if w == "off" => Spec::Off(line.no),
                        [w] if w.contains("://") => {
                            if Url::parse(w).is_none() {
                                return Err(line.err(format!(
                                    "{w} isn't a URL BareProxy can fetch: http:// or https://, a host, a path"
                                )));
                            }
                            Spec::Url(w.clone())
                        }
                        [w] if Net::parse(w).is_none() => {
                            Spec::File(w.trim_start_matches('/').to_string())
                        }
                        ws => Spec::Fixed(nets(line, ws)?, line.no),
                    };
                }
                other => return Err(line.err(format!("unknown setting {other}"))),
            }
        }

        // Where each bot's addresses come from, with one list for the bots
        // that share it.
        for (i, (mut bot, spec)) in bots.into_iter().zip(specs).enumerate() {
            let known = KNOWN.get(i);
            let fetch_off_why =
                |n: usize| Source::None(format!("fetching address lists is off (line {n})"));
            bot.source = match spec {
                Spec::Table => match known.map(|k| &k.list) {
                    Some(List::Url(u)) => match fetch_off {
                        Some(n) => fetch_off_why(n),
                        None => c.list(u, true, format!("{}'s list", bot.operator), i),
                    },
                    Some(List::Fixed(r)) => Source::Fixed(
                        r.iter().map(|x| Net::parse(x).unwrap()).collect(),
                        format!(
                            "the range{} {} states",
                            if r.len() == 1 { "" } else { "s" },
                            bot.operator
                        ),
                    ),
                    Some(List::None) => {
                        Source::None(format!("{} publishes no address list", bot.operator))
                    }
                    None => Source::None("the config gives it no addresses".into()),
                },
                Spec::Off(n) => Source::None(format!("its address list is off (line {n})")),
                Spec::Url(u) => match fetch_off {
                    Some(n) => fetch_off_why(n),
                    None => {
                        let host = Url::parse(&u).map(|x| x.host).unwrap_or_default();
                        c.list(&u, true, format!("the list from {host}"), i)
                    }
                },
                Spec::File(f) => {
                    let label = format!("the list in {f}");
                    c.list(&f, false, label, i)
                }
                Spec::Fixed(n, line) => Source::Fixed(n, format!("the addresses on line {line}")),
            };
            c.bots.push(bot);
        }
        c.sections.sort_by_key(|s| std::cmp::Reverse(s.path.len()));
        Ok(c)
    }

    fn list(&mut self, at: &str, url: bool, label: String, bot: usize) -> Source {
        if let Some(i) = self.lists.iter().position(|l| l.at == at) {
            self.lists[i].bots.push(bot);
            return Source::List(i);
        }
        self.lists.push(ListDef {
            at: at.to_string(),
            url,
            label,
            bots: vec![bot],
        });
        Source::List(self.lists.len() - 1)
    }

    /// The bot a User-Agent names, if any: the first whose name appears in
    /// it as a word of its own, in any case.
    pub fn find(&self, agent: &str) -> Option<usize> {
        if agent.is_empty() {
            return None;
        }
        let ua = agent.to_ascii_lowercase();
        self.bots.iter().position(|b| has_word(&ua, &b.word))
    }

    /// Whether a claim to be the bot holds for the address.
    pub fn check(&self, bot: usize, addr: Option<IpAddr>, loaded: &[Option<Rc<Loaded>>]) -> Check {
        let Some(ip) = addr else {
            return Check::Unverified("the client's address couldn't be read".into());
        };
        let (found, n, label) = match &self.bots[bot].source {
            Source::None(why) => return Check::Unverified(why.clone()),
            Source::Fixed(nets, label) => (nets.iter().any(|n| n.contains(ip)), nets.len(), label),
            Source::List(i) => {
                let label = &self.lists[*i].label;
                match loaded.get(*i).and_then(|l| l.as_ref()) {
                    Some(l) => (l.index.contains(ip), l.nets.len(), label),
                    None => return Check::Unverified(format!("{label} isn't loaded yet")),
                }
            }
        };
        if found {
            Check::Verified(format!("{ip} is in {label}"))
        } else {
            Check::Fake(format!("{ip} isn't in {label} ({})", plural(n, "range")))
        }
    }

    /// The rule for a bot on a path: the longest path section's, then the
    /// top's; in each, the rule that names the bot, then its group, then
    /// all. None when no rule applies, which lets it in.
    pub fn rule(&self, bot: usize, path: &str) -> Option<Rule> {
        let g = &self.bots[bot].group;
        self.sections
            .iter()
            .find(|s| kit::under(path, &s.path))
            .and_then(|s| s.pick(bot, g))
            .or_else(|| self.top.pick(bot, g))
    }

    /// A bot as notes name it: "GPTBot (OpenAI, training)".
    pub fn who(&self, bot: usize) -> String {
        let b = &self.bots[bot];
        if b.operator.is_empty() {
            format!("{} ({})", b.name, b.group)
        } else {
            format!("{} ({}, {})", b.name, b.operator, b.group)
        }
    }

    /// The report: requests today and yesterday by bot and answer, what the
    /// bots read, and the state of each address list.
    pub fn report(&self, r: &Report) -> String {
        let mut out = format!(
            "BareProxy AI crawler report for {}, {}.\n\nRequests today (since 00:00 UTC), then yesterday:\n",
            r.site,
            kit::utc(r.now)
        );
        let rows: Vec<&Row> = r
            .rows
            .iter()
            .filter(|x| x.today.iter().chain(&x.yesterday).any(|n| *n > 0))
            .collect();
        if rows.is_empty() {
            out.push_str("none from the bots the plugin knows.\n");
        } else {
            out.push_str(
                "bot\tgroup\tlet in\tlimited\tblocked\tfakes\t\tlet in\tlimited\tblocked\tfakes\n",
            );
            for x in rows {
                let b = &self.bots[x.bot];
                let t = x.today.map(|n| n.to_string()).join("\t");
                let y = x.yesterday.map(|n| n.to_string()).join("\t");
                out.push_str(&format!("{}\t{}\t{t}\t\t{y}\n", b.name, b.group));
            }
        }
        out.push_str(&format!(
            "\nWhat they read since the plugin started, by first path segment (brought together every {}s):\n",
            r.every
        ));
        let mut read: Vec<(&String, &HashMap<String, u64>)> = r.reads.iter().collect();
        read.sort_by(|a, b| {
            let sum = |m: &HashMap<String, u64>| m.values().sum::<u64>();
            sum(b.1).cmp(&sum(a.1)).then(a.0.cmp(b.0))
        });
        if read.is_empty() {
            out.push_str("nothing yet.\n");
        }
        for (bot, m) in read {
            let mut v: Vec<(&String, &u64)> = m.iter().collect();
            v.sort_by(|a, b| b.1.cmp(a.1).then(a.0.cmp(b.0)));
            let top: Vec<String> = v.iter().take(10).map(|(p, n)| format!("{n} {p}")).collect();
            let more = if v.len() > 10 {
                format!(" and {} more", v.len() - 10)
            } else {
                String::new()
            };
            out.push_str(&format!("{bot}\t{}{more}\n", top.join(", ")));
        }
        out.push_str("\nAddress lists:\n");
        for (i, l) in self.lists.iter().enumerate() {
            let names: Vec<&str> = l.bots.iter().map(|b| self.bots[*b].name.as_str()).collect();
            let st = r.lists.get(i).cloned().unwrap_or_default();
            let state = if st.ranges > 0 {
                let from = if l.url { "fetched" } else { "read" };
                format!(
                    "{}, {from} {}",
                    plural(st.ranges, "range"),
                    kit::utc(st.when)
                )
            } else {
                "not loaded yet".to_string()
            };
            let err = if st.error.is_empty() {
                String::new()
            } else {
                format!("; last try: {}", st.error)
            };
            out.push_str(&format!(
                "{} ({}): {state}{err}. {}\n",
                upper_first(&l.label),
                names.join(", "),
                l.at
            ));
        }
        for b in &self.bots {
            if let Source::Fixed(n, label) = &b.source {
                out.push_str(&format!(
                    "{} ({}): {}\n",
                    upper_first(label),
                    b.name,
                    plural(n.len(), "range")
                ));
            }
        }
        let none: Vec<&str> = self
            .bots
            .iter()
            .filter(|b| matches!(b.source, Source::None(_)))
            .map(|b| b.name.as_str())
            .collect();
        if !none.is_empty() {
            out.push_str(&format!(
                "No list, so their claims can't be checked: {}.\n",
                none.join(", ")
            ));
        }
        out
    }
}

/// Counts for one bot in the report: let in, limited, blocked and fakes.
#[derive(Debug, Clone, Default)]
pub struct Row {
    pub bot: usize,
    pub today: [u64; 4],
    pub yesterday: [u64; 4],
}

/// A list's state for the report; ranges is 0 when it isn't loaded.
#[derive(Debug, Clone, Default)]
pub struct ListStatus {
    pub ranges: usize,
    pub when: u64,
    pub error: String,
}

pub struct Report<'a> {
    pub site: &'a str,
    pub now: u64,
    pub rows: Vec<Row>,
    /// Reads by bot name, then path segment.
    pub reads: HashMap<String, HashMap<String, u64>>,
    pub lists: Vec<ListStatus>,
    pub every: u64,
}

/// What the four counts of a Row and of a tally count, in order.
pub const IN: usize = 0;
pub const LIMITED: usize = 1;
pub const BLOCKED: usize = 2;
pub const FAKE: usize = 3;

/// One instance's counts of bot requests by answer, keyed by the UTC day
/// (days since 1970), the site and the bot with tabs between.
pub type Tally = HashMap<String, [u64; 4]>;

pub fn tally(t: &mut Tally, day: u64, site: &str, bot: &str, answer: usize) {
    t.entry(format!("{day}\t{site}\t{bot}")).or_default()[answer] += 1;
}

/// Drops the days before yesterday.
pub fn prune(t: &mut Tally, today: u64) {
    t.retain(|k, _| {
        k.split('\t')
            .next()
            .and_then(|d| d.parse::<u64>().ok())
            .is_some_and(|d| d + 1 >= today)
    });
}

/// A tally as an instance shares it: the key and the four counts a line.
pub fn encode_tally(t: &Tally) -> String {
    let mut out = String::new();
    for (k, n) in t {
        out.push_str(&format!("{k}\t{}\t{}\t{}\t{}\n", n[0], n[1], n[2], n[3]));
    }
    out
}

/// Adds a shared tally's counts for one site, today and yesterday, to rows
/// by bot.
pub fn add_tally(c: &Config, rows: &mut Vec<Row>, text: &str, site: &str, today: u64) {
    for line in text.lines() {
        let f: Vec<&str> = line.split('\t').collect();
        let [day, s, bot, n @ ..] = f.as_slice() else {
            continue;
        };
        let (Ok(day), true, 4) = (day.parse::<u64>(), *s == site, n.len()) else {
            continue;
        };
        let Some(b) = c.bots.iter().position(|x| x.name == *bot) else {
            continue;
        };
        let i = match rows.iter().position(|r| r.bot == b) {
            Some(i) => i,
            None => {
                rows.push(Row {
                    bot: b,
                    ..Row::default()
                });
                rows.len() - 1
            }
        };
        let into = if day == today {
            &mut rows[i].today
        } else if day + 1 == today {
            &mut rows[i].yesterday
        } else {
            continue;
        };
        for (j, x) in n.iter().enumerate() {
            into[j] += x.parse::<u64>().unwrap_or(0);
        }
    }
    rows.sort_by_key(|r| r.bot);
}

fn upper_first(s: &str) -> String {
    let mut c = s.chars();
    match c.next() {
        Some(f) => f.to_uppercase().chain(c).collect(),
        None => String::new(),
    }
}

pub fn plural(n: usize, what: &str) -> String {
    if n == 1 {
        format!("1 {what}")
    } else {
        format!("{n} {what}s")
    }
}

/// Whether word is in text with no letter, digit, - or _ just before or
/// after it.
pub fn has_word(text: &str, word: &str) -> bool {
    let b = text.as_bytes();
    let edge =
        |c: Option<&u8>| c.is_none_or(|c| !(c.is_ascii_alphanumeric() || *c == b'-' || *c == b'_'));
    let mut from = 0;
    while let Some(i) = text[from..].find(word) {
        let (s, e) = (from + i, from + i + word.len());
        if edge(s.checked_sub(1).and_then(|j| b.get(j))) && edge(b.get(e)) {
            return true;
        }
        from = s + 1;
    }
    false
}

/// The address ranges in a list: entries named ipv4Prefix and ipv6Prefix,
/// as the operators publish them in JSON (also when the JSON sits in an
/// HTML page), or else plain text with one address or range a line, where
/// # starts a comment. A list with no ranges is an error: adopting one
/// would make every claim a fake. So is JSON or HTML with no entries.
pub fn parse_list(text: &str) -> Result<Vec<Net>, String> {
    let mut out = vec![];
    let json_or_html = text.trim_start().starts_with(['{', '[', '<']);
    if text.contains("ipv4Prefix") || text.contains("ipv6Prefix") || json_or_html {
        for key in ["ipv4Prefix", "ipv6Prefix"] {
            let mut rest = text;
            while let Some(i) = rest.find(key) {
                rest = &rest[i + key.len()..];
                let mut s = rest;
                // Past the quotes and the colon, which an HTML page writes
                // as &quot;.
                for _ in 0..4 {
                    s = s.trim_start_matches(|c: char| {
                        c == '"' || c == '\'' || c == ':' || c.is_whitespace()
                    });
                    match s.strip_prefix("&quot;").or_else(|| s.strip_prefix("&#34;")) {
                        Some(t) => s = t,
                        None => break,
                    }
                }
                let end = s
                    .find(|c: char| !(c.is_ascii_hexdigit() || ".:/\\".contains(c)))
                    .unwrap_or(s.len());
                if let Some(n) = Net::parse(&s[..end].replace('\\', "")) {
                    out.push(n);
                }
            }
        }
    } else {
        for (i, raw) in text.lines().enumerate() {
            match kit::words(raw).as_slice() {
                [] => {}
                [w] => {
                    out.push(Net::parse(w).ok_or_else(|| {
                        format!("line {}: {w} isn't an address or a range", i + 1)
                    })?)
                }
                _ => return Err(format!("line {}: write one address or range a line", i + 1)),
            }
        }
    }
    if out.is_empty() {
        return Err("has no address ranges in it".into());
    }
    Ok(out)
}

/// A list as instances share it, and as the store keeps it: when it was
/// loaded, then a range a line.
pub fn encode_list(l: &Loaded) -> String {
    let mut s = format!("{}\n", l.when);
    for n in &l.nets {
        s.push_str(&n.to_string());
        s.push('\n');
    }
    s
}

pub fn decode_list(text: &str) -> Option<Loaded> {
    let mut lines = text.lines();
    let when = lines.next()?.parse().ok()?;
    let nets: Vec<Net> = lines.filter_map(Net::parse).collect();
    (!nets.is_empty()).then(|| Loaded::new(nets, when))
}

/// The part of a path the report counts reads under: its first segment.
pub fn section(path: &str) -> &str {
    match path.get(1..).and_then(|p| p.find('/')) {
        Some(i) => &path[..i + 2],
        None if path.is_empty() => "/",
        None => path,
    }
}

/// Counts a read of a path segment, keeping at most MAX_SECTIONS per key.
pub fn add_read(reads: &mut HashMap<String, HashMap<String, u64>>, key: &str, sec: &str) {
    let m = reads.entry(key.to_string()).or_default();
    if let Some(n) = m.get_mut(sec) {
        *n += 1;
    } else if m.len() < MAX_SECTIONS {
        m.insert(sec.to_string(), 1);
    } else {
        *m.entry("other".into()).or_default() += 1;
    }
}

/// Reads as one instance shares them: key, segment and count a line, the
/// key being the site and the bot with a tab between; at most max bytes.
pub fn encode_reads(reads: &HashMap<String, HashMap<String, u64>>, max: usize) -> String {
    let mut out = String::new();
    for (k, m) in reads {
        for (sec, n) in m {
            let line = format!("{k}\t{sec}\t{n}\n");
            if out.len() + line.len() > max {
                return out;
            }
            out.push_str(&line);
        }
    }
    out
}

/// Adds shared reads for one site to a map by bot.
pub fn add_reads(into: &mut HashMap<String, HashMap<String, u64>>, text: &str, site: &str) {
    for line in text.lines() {
        let mut f = line.split('\t');
        let (Some(s), Some(bot), Some(sec), Some(n)) = (f.next(), f.next(), f.next(), f.next())
        else {
            continue;
        };
        if s == site
            && let Ok(n) = n.parse::<u64>()
        {
            *into
                .entry(bot.to_string())
                .or_default()
                .entry(sec.to_string())
                .or_default() += n;
        }
    }
}

/// A short time from milliseconds: 1s, 1m, 2h, 1d.
pub fn short(ms: u64) -> String {
    let s = ms.div_ceil(1000);
    match s {
        _ if s >= 86400 && s.is_multiple_of(86400) => format!("{}d", s / 86400),
        _ if s >= 3600 && s.is_multiple_of(3600) => format!("{}h", s / 3600),
        _ if s >= 60 && s.is_multiple_of(60) => format!("{}m", s / 60),
        _ => format!("{s}s"),
    }
}

/// 1st, 2nd, 3rd, 4th, 11th, 21st.
pub fn ordinal(n: u64) -> String {
    let suffix = match (n % 10, n % 100) {
        (_, 11..=13) => "th",
        (1, _) => "st",
        (2, _) => "nd",
        (3, _) => "rd",
        _ => "th",
    };
    format!("{n}{suffix}")
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ip(s: &str) -> Option<IpAddr> {
        Some(s.parse().unwrap())
    }

    fn bot(c: &Config, name: &str) -> usize {
        c.bots.iter().position(|b| b.name == name).unwrap()
    }

    #[test]
    fn finds_bots_in_user_agents() {
        let c = Config::parse("").unwrap();
        let name = |ua: &str| c.find(ua).map(|i| c.bots[i].name.as_str());
        let cases = [
            (
                "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; GPTBot/1.4; +https://openai.com/gptbot",
                Some("GPTBot"),
            ),
            (
                "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; ChatGPT-User/1.0; +https://openai.com/bot",
                Some("ChatGPT-User"),
            ),
            (
                "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36; compatible; OAI-SearchBot/1.4; +https://openai.com/searchbot",
                Some("OAI-SearchBot"),
            ),
            (
                "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; ClaudeBot/1.0; +claudebot@anthropic.com)",
                Some("ClaudeBot"),
            ),
            (
                "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Claude-User/1.0; +Claude-User@anthropic.com)",
                Some("Claude-User"),
            ),
            (
                "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; PerplexityBot/1.0; +https://perplexity.ai/perplexitybot)",
                Some("PerplexityBot"),
            ),
            (
                "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Perplexity-User/1.0; +https://perplexity.ai/perplexity-user)",
                Some("Perplexity-User"),
            ),
            ("CCBot/2.0 (https://commoncrawl.org/faq/)", Some("CCBot")),
            (
                "meta-externalagent/1.1 (+https://developers.facebook.com/docs/sharing/webmasters/crawler)",
                Some("Meta-ExternalAgent"),
            ),
            (
                "Mozilla/5.0 (Linux; Android 5.0) AppleWebKit/537.36 (KHTML, like Gecko) Mobile Safari/537.36 (compatible; Bytespider; spider-feedback@bytedance.com)",
                Some("Bytespider"),
            ),
            (
                "DuckAssistBot/1.2; (+http://duckduckgo.com/duckassistbot.html)",
                Some("DuckAssistBot"),
            ),
            (
                "Mozilla/5.0 (compatible) AI2Bot (+https://www.allenai.org/crawler)",
                Some("AI2Bot"),
            ),
            (
                "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 Safari/605.1.15",
                None,
            ),
            (
                "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
                None,
            ),
            ("GPTBotX/1.0", None),
            ("MyGPTBot/1.0", None),
            ("", None),
        ];
        for (ua, want) in cases {
            assert_eq!(name(ua), want, "{ua}");
        }
    }

    #[test]
    fn words_stand_alone() {
        assert!(has_word("compatible; gptbot/1.4", "gptbot"));
        assert!(has_word("gptbot", "gptbot"));
        assert!(!has_word("gptbot-x/1.0", "gptbot"));
        assert!(!has_word("xgptbot gptbotz", "gptbot"));
        assert!(has_word("xgptbot gptbot", "gptbot"));
    }

    #[test]
    fn builtin_sources() {
        let c = Config::parse("").unwrap();
        let src = |n: &str| &c.bots[bot(&c, n)].source;
        let Source::List(a) = src("ClaudeBot") else {
            panic!()
        };
        assert_eq!(src("Claude-User"), &Source::List(*a));
        assert_eq!(src("Claude-SearchBot"), &Source::List(*a));
        assert_eq!(c.lists[*a].label, "Anthropic's list");
        assert_eq!(c.lists[*a].bots.len(), 3);
        assert!(
            matches!(src("Bytespider"), Source::None(w) if w == "ByteDance publishes no address list")
        );
        assert!(
            matches!(src("YouBot"), Source::Fixed(n, l) if n.len() == 1 && l == "the range You.com states")
        );
        // Thirteen lists from seven hosts, every one https.
        assert_eq!(c.lists.len(), 13);
        let mut hosts: Vec<String> = c
            .lists
            .iter()
            .map(|l| Url::parse(&l.at).unwrap().upstream())
            .collect();
        hosts.sort();
        hosts.dedup();
        assert_eq!(
            hosts,
            [
                "claude.com:443",
                "developer.amazon.com:443",
                "developers.google.com:443",
                "index.commoncrawl.org:443",
                "mistral.ai:443",
                "openai.com:443",
                "www.perplexity.ai:443"
            ]
        );
        // Every name and group is unique and every bot is in a group.
        for (i, b) in c.bots.iter().enumerate() {
            assert!(GROUPS.iter().any(|(g, _)| *g == b.group), "{}", b.name);
            assert_eq!(c.find(&format!("x {}/1.0", b.name)), Some(i), "{}", b.name);
        }
    }

    #[test]
    fn checks_addresses() {
        let c = Config::parse("ranges Bytespider 192.0.2.0/24\n").unwrap();
        let g = bot(&c, "GPTBot");
        let Source::List(li) = c.bots[g].source else {
            panic!()
        };
        let mut loaded = vec![None; c.lists.len()];
        assert_eq!(
            c.check(g, ip("20.171.206.5"), &loaded),
            Check::Unverified("OpenAI's list isn't loaded yet".into())
        );
        loaded[li] = Some(Rc::new(Loaded::new(
            vec![
                Net::parse("20.171.206.0/24").unwrap(),
                Net::parse("2001:db8::/32").unwrap(),
            ],
            1,
        )));
        assert_eq!(
            c.check(g, ip("20.171.206.5"), &loaded),
            Check::Verified("20.171.206.5 is in OpenAI's list".into())
        );
        assert_eq!(c.check(g, ip("2001:db8::7"), &loaded).word(), "verified");
        // The promise: a faked user agent from outside the bot's ranges
        // isn't the bot.
        assert_eq!(
            c.check(g, ip("203.0.113.9"), &loaded),
            Check::Fake("203.0.113.9 isn't in OpenAI's list (2 ranges)".into())
        );
        assert_eq!(c.check(g, None, &loaded).word(), "unverified");
        let b = bot(&c, "Bytespider");
        assert_eq!(
            c.check(b, ip("192.0.2.9"), &loaded),
            Check::Verified("192.0.2.9 is in the addresses on line 1".into())
        );
        assert_eq!(c.check(b, ip("198.51.100.1"), &loaded).word(), "fake");
        let y = bot(&c, "YouBot");
        assert_eq!(c.check(y, ip("68.67.112.40"), &loaded).word(), "verified");
        assert_eq!(c.check(y, ip("68.67.113.40"), &loaded).word(), "fake");
        let m = bot(&c, "Meta-ExternalAgent");
        assert_eq!(
            c.check(m, ip("198.51.100.1"), &loaded),
            Check::Unverified("Meta publishes no address list".into())
        );
    }

    #[test]
    fn rules_by_bot_group_all_and_path() {
        let c = Config::parse(
            "block training\nallow GPTBot\nlimit PerplexityBot ClaudeBot 30 per 1m\nallow all\n\npath /members/\n  block all\n  allow ChatGPT-User\npath /members/free/\n  allow training\n",
        )
        .unwrap();
        let r = |name: &str, path: &str| c.rule(bot(&c, name), path).map(|r| (r.act, r.line));
        assert_eq!(r("GPTBot", "/"), Some((Act::Allow, 2)));
        assert_eq!(r("CCBot", "/blog"), Some((Act::Block, 1)));
        assert_eq!(
            r("ClaudeBot", "/"),
            Some((
                Act::Limit {
                    count: 30,
                    window_ms: 60_000
                },
                3
            ))
        );
        assert_eq!(r("YouBot", "/"), Some((Act::Allow, 4)));
        assert_eq!(r("GPTBot", "/members/x"), Some((Act::Block, 7)));
        assert_eq!(r("ChatGPT-User", "/members/x"), Some((Act::Allow, 8)));
        assert_eq!(r("CCBot", "/members/free/a"), Some((Act::Allow, 10)));
        // The longest section has no rule for a search bot, so the top's apply.
        assert_eq!(
            r("PerplexityBot", "/members/free/a"),
            Some((
                Act::Limit {
                    count: 30,
                    window_ms: 60_000
                },
                3
            ))
        );
        assert_eq!(r("YouBot", "/membersx"), Some((Act::Allow, 4)));
        let none = Config::parse("").unwrap();
        assert_eq!(none.rule(bot(&none, "GPTBot"), "/"), None);
    }

    #[test]
    fn bots_of_your_own() {
        let c = Config::parse(
            "bot Googlebot search-engine\nranges Googlebot https://developers.google.com/static/crawling/ipranges/common-crawlers.json\nbot MyScraper training\nranges MyScraper scrapers.txt\nblock search-engine\nranges ClaudeBot claude.txt\n",
        )
        .unwrap();
        let g = bot(&c, "Googlebot");
        assert_eq!(
            c.find("Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"),
            Some(g)
        );
        assert_eq!(c.who(g), "Googlebot (search-engine)");
        assert_eq!(c.rule(g, "/").map(|r| r.act), Some(Act::Block));
        let Source::List(i) = c.bots[g].source else {
            panic!()
        };
        assert_eq!(c.lists[i].label, "the list from developers.google.com");
        let Source::List(f) = c.bots[bot(&c, "MyScraper")].source else {
            panic!()
        };
        assert!(!c.lists[f].url);
        assert_eq!(c.lists[f].label, "the list in scrapers.txt");
        // ClaudeBot has a file now; the other two keep Anthropic's list.
        let Source::List(cf) = c.bots[bot(&c, "ClaudeBot")].source else {
            panic!()
        };
        assert_eq!(c.lists[cf].at, "claude.txt");
        let Source::List(a) = c.bots[bot(&c, "Claude-User")].source else {
            panic!()
        };
        assert_eq!(c.lists[a].bots.len(), 2);
        assert_eq!(c.who(bot(&c, "GPTBot")), "GPTBot (OpenAI, training)");
    }

    #[test]
    fn settings() {
        let c = Config::parse(
            "fake allow\nunverified fake\nheader X-AI-Bot\nstatus 451\nrefresh 1d\nforwarded-from 10.0.0.0/8\nreport /.crawlers\nreport-allow 203.0.113.0/24\n",
        )
        .unwrap();
        assert!(!c.fake_block && c.unverified_fake);
        assert_eq!(c.header.as_deref(), Some("x-ai-bot"));
        assert_eq!((c.status, c.refresh), (451, 86400));
        assert_eq!(c.report.as_deref(), Some("/.crawlers"));
        assert!(kit::any_contains(&c.report_allow, "203.0.113.5"));
        assert!(!kit::any_contains(&c.report_allow, "127.0.0.1"));
        let d = Config::parse("").unwrap();
        assert!(d.fake_block && !d.unverified_fake);
        assert_eq!(
            (d.header.as_deref(), d.status, d.refresh),
            (Some("x-bot"), 403, 21600)
        );
        assert!(kit::any_contains(&d.report_allow, "::1"));
        let off = Config::parse("fetch off\nheader off\n").unwrap();
        assert!(off.header.is_none());
        assert!(off.lists.is_empty());
        assert!(
            matches!(&off.bots[0].source, Source::None(w) if w == "fetching address lists is off (line 1)")
        );
    }

    #[test]
    fn refused() {
        for (text, want) in [
            (
                "allow GPTbott",
                "line 1: GPTbott isn't a bot the plugin knows or a group (training, search, assistant, all)",
            ),
            (
                "block GPTBot\nallow gptbot",
                "line 2: gptbot has a rule on line 1 already",
            ),
            (
                "block training\nlimit training 5 per 1m",
                "line 2: training has a rule on line 1 already",
            ),
            ("allow", "line 1: allow takes bots or groups"),
            (
                "limit GPTBot 10",
                "line 1: write limit NAME... COUNT per TIME",
            ),
            (
                "limit 10 per 1m",
                "line 1: write limit NAME... COUNT per TIME",
            ),
            (
                "limit GPTBot 0 per 1m",
                "line 1: 0 isn't a count of requests",
            ),
            (
                "limit GPTBot 5 per 2d",
                "line 1: a limit's time is 1s to 1d",
            ),
            ("fake maybe", "line 1: fake takes block or allow"),
            ("unverified yes", "line 1: unverified takes bot or fake"),
            ("status 500", "line 1: status takes 403, 404, 410 or 451"),
            ("refresh 1m", "line 1: refresh is 10m at least"),
            ("header X Y", "line 1: header takes a header name"),
            ("path members", "line 1: path takes one path prefix"),
            ("path /a/\npath /a/", "line 2: path /a/ is set twice"),
            (
                "path /a/\nfake allow",
                "line 2: fake goes at the top, before any path section",
            ),
            ("path /a/\nbot X training", "line 2: bot goes at the top"),
            ("bot GPTBot training", "line 1: GPTBot is a bot already"),
            (
                "bot Search training",
                "line 1: Search is a group's name, not a bot's",
            ),
            ("bot X all", "line 1: all can't be a group"),
            ("bot X/1 training", "line 1: X/1 isn't a bot's name"),
            ("bot X", "line 1: bot takes a name"),
            (
                "ranges Nobody x.txt",
                "line 1: Nobody isn't a bot the plugin knows",
            ),
            ("ranges GPTBot", "line 1: ranges takes a bot and where"),
            (
                "ranges GPTBot ftp://x/y",
                "line 1: ftp://x/y isn't a URL BareProxy can fetch",
            ),
            (
                "ranges GPTBot 192.0.2.0/24 nope",
                "line 1: nope isn't an address or a range",
            ),
            (
                "forwarded-from",
                "line 1: forwarded-from takes addresses or ranges",
            ),
            ("report crawlers", "line 1: report takes a path"),
            ("bogus x", "line 1: unknown setting bogus"),
        ] {
            let err = Config::parse(text).unwrap_err();
            assert!(err.starts_with(want), "{text}: {err}");
        }
    }

    #[test]
    fn lists_as_published() {
        let json = "{\n  \"creationTime\": \"2026-09-22T02:00:07.000000\",\n  \"prefixes\": [\n    {\n      \"ipv4Prefix\": \"132.196.86.0/24\"\n    },\n    {\"ipv6Prefix\": \"2600:1f28:365:8000::/56\"},\n    {\"ipv4Prefix\":\"74.7.175.128\\/25\"}\n  ]\n}\n";
        let n = parse_list(json).unwrap();
        assert_eq!(n.len(), 3);
        assert!(n[0].contains("132.196.86.200".parse().unwrap()));
        // IPv4 entries first, then IPv6.
        assert!(n[1].contains("74.7.175.200".parse().unwrap()));
        assert!(n[2].contains("2600:1f28:365:80ff::1".parse().unwrap()));
        // The same JSON shown in an HTML page, as Amazon does, with bare addresses.
        let html = "<html><pre><code>{&quot;creationTime&quot;: &quot;2026-09-08&quot;, &quot;prefixes&quot;: [{&quot;ipv4Prefix&quot;: &quot;3.81.245.78&quot;}, {&quot;ipv4Prefix&quot;: &quot;3.81.253.213&quot;}]}</code></pre><p>Each entry has an ipv4Prefix and the like.</p></html>";
        let n = parse_list(html).unwrap();
        assert_eq!(n.len(), 2);
        assert!(n[1].contains("3.81.253.213".parse().unwrap()));
        assert!(!n[1].contains("3.81.253.214".parse().unwrap()));
        let text = "# my scrapers\n192.0.2.0/24\n2001:db8::1   # one address\n\n";
        assert_eq!(parse_list(text).unwrap().len(), 2);
        assert_eq!(
            parse_list("192.0.2.0/24\nexample.com\n").unwrap_err(),
            "line 2: example.com isn't an address or a range"
        );
        assert_eq!(
            parse_list("1.2.3.4 5.6.7.8").unwrap_err(),
            "line 1: write one address or range a line"
        );
        assert_eq!(
            parse_list("{\"prefixes\": []}").unwrap_err(),
            "has no address ranges in it"
        );
        assert_eq!(
            parse_list("<html><p>Moved</p></html>").unwrap_err(),
            "has no address ranges in it"
        );
        assert_eq!(
            parse_list("{\"ipv4Prefix\": \"\"}").unwrap_err(),
            "has no address ranges in it"
        );
        assert_eq!(parse_list("").unwrap_err(), "has no address ranges in it");
    }

    #[test]
    fn lists_shared_and_kept() {
        let l = Loaded::new(
            vec![
                Net::parse("192.0.2.0/24").unwrap(),
                Net::parse("2001:db8::/32").unwrap(),
            ],
            1_791_547_500,
        );
        let text = encode_list(&l);
        assert_eq!(text, "1791547500\n192.0.2.0/24\n2001:db8::/32\n");
        assert_eq!(decode_list(&text), Some(l));
        assert_eq!(decode_list("12\n"), None);
        assert_eq!(decode_list("x\n192.0.2.0/24\n"), None);
    }

    #[test]
    fn index_lookups() {
        // Two thousand single addresses, as Amazon lists them: every other
        // one in 3.0.0.0 to 3.0.15.255.
        let mut nets: Vec<Net> = (0..2000u32)
            .map(|i| Net::parse(&format!("3.0.{}.{}", i / 128, i % 128 * 2)).unwrap())
            .collect();
        nets.push(Net::parse("192.0.2.0/25").unwrap());
        nets.push(Net::parse("192.0.2.128/25").unwrap()); // merges with the one before
        nets.push(Net::parse("2600:1f28:365:8000::/56").unwrap());
        let ix = Index::new(&nets);
        let has = |s: &str| ix.contains(s.parse().unwrap());
        for n in &nets {
            let (_, a, b) = n.bounds();
            assert_eq!(Index::new(&[*n]).v4.len() + Index::new(&[*n]).v6.len(), 1);
            assert!(a <= b);
        }
        assert!(has("3.0.0.0") && has("3.0.7.8") && has("3.0.15.158"));
        assert!(!has("3.0.0.1") && !has("3.0.7.9") && !has("3.0.15.160"));
        assert!(has("192.0.2.0") && has("192.0.2.200") && has("192.0.2.255"));
        assert!(!has("192.0.3.0") && !has("192.0.1.255"));
        assert!(has("::ffff:192.0.2.9"));
        assert!(has("2600:1f28:365:80ff:ffff::1") && !has("2600:1f28:365:8100::"));
        assert!(!has("0.0.0.0") && !has("255.255.255.255") && !has("::"));
        assert!(ix.v4.contains(&(0xC000_0200, 0xC000_02FF)));
    }

    #[test]
    fn urls() {
        let u = Url::parse("https://openai.com/gptbot.json").unwrap();
        assert_eq!(
            (u.upstream(), u.authority(), u.path.as_str()),
            ("openai.com:443".into(), "openai.com".into(), "/gptbot.json")
        );
        let u = Url::parse("http://127.0.0.1:8080").unwrap();
        assert_eq!(
            (u.upstream(), u.authority(), u.path.as_str()),
            ("127.0.0.1:8080".into(), "127.0.0.1:8080".into(), "/")
        );
        let u = Url::parse("HTTP://Example.COM?x=1").unwrap();
        assert_eq!((u.host.as_str(), u.path.as_str()), ("example.com", "/?x=1"));
        assert_eq!(
            Url::parse("http://[::1]:9/l").unwrap().upstream(),
            "[::1]:9"
        );
        for bad in [
            "ftp://x/",
            "https://",
            "https://a b/",
            "https://u@h/",
            "https://h:0/",
            "https://h:x/",
            "nope",
        ] {
            assert!(Url::parse(bad).is_none(), "{bad}");
        }
        let base = "https://developer.amazon.com/amazonbot/ip-addresses/";
        assert_eq!(
            resolve(base, "/en-US/amazonbot/ip-addresses.html").as_deref(),
            Some("https://developer.amazon.com/en-US/amazonbot/ip-addresses.html")
        );
        assert_eq!(
            resolve(base, "https://www.amazon.com/x").as_deref(),
            Some("https://www.amazon.com/x")
        );
        assert_eq!(resolve(base, "elsewhere"), None);
        assert_eq!(resolve(base, "//cdn.example/x"), None);
        assert!(same_host(base, "https://developer.amazon.com:443/y"));
        assert!(!same_host(base, "http://developer.amazon.com/y"));
        assert!(!same_host(
            "https://www.perplexity.com/perplexitybot.json",
            "https://www.perplexity.ai/perplexitybot.json"
        ));
    }

    #[test]
    fn reads_and_sections() {
        assert_eq!(section("/"), "/");
        assert_eq!(section("/blog/2026/post"), "/blog/");
        assert_eq!(section("/about"), "/about");
        assert_eq!(section(""), "/");
        let mut r = HashMap::new();
        for i in 0..(MAX_SECTIONS + 5) {
            add_read(&mut r, "example.com\tGPTBot", &format!("/p{i}"));
        }
        add_read(&mut r, "example.com\tGPTBot", "/p0");
        add_read(&mut r, "other.org\tGPTBot", "/x/");
        let m = &r["example.com\tGPTBot"];
        assert_eq!((m.len(), m["/p0"], m["other"]), (MAX_SECTIONS + 1, 2, 5));
        let mut into = HashMap::new();
        let shared = encode_reads(&r, usize::MAX);
        assert!(encode_reads(&r, 100).len() <= 100);
        add_reads(&mut into, &shared, "example.com");
        add_reads(&mut into, &shared, "example.com");
        assert_eq!(into["GPTBot"]["/p0"], 4);
        assert!(!into.contains_key("other.org"));
        assert_eq!(into.len(), 1);
    }

    #[test]
    fn tallies() {
        let c = Config::parse("").unwrap();
        let (g, cl) = (bot(&c, "GPTBot"), bot(&c, "ClaudeBot"));
        let mut a = Tally::new();
        tally(&mut a, 20375, "example.com", "GPTBot", IN);
        tally(&mut a, 20375, "example.com", "GPTBot", IN);
        tally(&mut a, 20375, "example.com", "GPTBot", FAKE);
        tally(&mut a, 20374, "example.com", "GPTBot", LIMITED);
        tally(&mut a, 20373, "example.com", "GPTBot", BLOCKED);
        tally(&mut a, 20375, "other.org", "ClaudeBot", BLOCKED);
        let mut b = Tally::new();
        tally(&mut b, 20375, "example.com", "GPTBot", IN);
        tally(&mut b, 20375, "example.com", "ClaudeBot", BLOCKED);
        tally(&mut b, 20375, "example.com", "Nobody", IN);
        let mut rows = vec![];
        add_tally(&c, &mut rows, &encode_tally(&a), "example.com", 20375);
        add_tally(&c, &mut rows, &encode_tally(&b), "example.com", 20375);
        add_tally(&c, &mut rows, "garbage\nx\ty\n", "example.com", 20375);
        assert_eq!(rows.len(), 2);
        assert_eq!(
            (rows[0].bot, rows[0].today, rows[0].yesterday),
            (g, [3, 0, 0, 1], [0, 1, 0, 0])
        );
        assert_eq!((rows[1].bot, rows[1].today), (cl, [0, 0, 1, 0]));
        prune(&mut a, 20375);
        assert_eq!(a.len(), 3);
        assert!(!a.keys().any(|k| k.starts_with("20373")));
    }

    #[test]
    fn the_report() {
        let c = Config::parse("ranges Bytespider 192.0.2.0/24\n").unwrap();
        let g = bot(&c, "GPTBot");
        let mut reads = HashMap::new();
        reads.insert(
            "GPTBot".to_string(),
            HashMap::from([("/blog/".to_string(), 7), ("/".to_string(), 1)]),
        );
        let mut lists = vec![ListStatus::default(); c.lists.len()];
        let Source::List(li) = c.bots[g].source else {
            panic!()
        };
        lists[li] = ListStatus {
            ranges: 18,
            when: 1_791_547_500,
            error: String::new(),
        };
        let Source::List(ai) = c.bots[bot(&c, "ClaudeBot")].source else {
            panic!()
        };
        lists[ai].error = "claude.com:443 isn't on the plugin's allow-http line".into();
        let r = Report {
            site: "example.com",
            now: 1_791_547_500,
            rows: vec![
                Row {
                    bot: g,
                    today: [8, 0, 0, 2],
                    yesterday: [3, 1, 0, 0],
                },
                Row {
                    bot: 1,
                    ..Row::default()
                },
            ],
            reads,
            lists,
            every: 5,
        };
        let out = c.report(&r);
        for want in [
            "BareProxy AI crawler report for example.com, 2026-10-09 12:05 UTC.",
            "GPTBot\ttraining\t8\t0\t0\t2\t\t3\t1\t0\t0\n",
            "GPTBot\t7 /blog/, 1 /\n",
            "OpenAI's list (GPTBot): 18 ranges, fetched 2026-10-09 12:05 UTC. https://openai.com/gptbot.json\n",
            "Anthropic's list (ClaudeBot, Claude-SearchBot, Claude-User): not loaded yet; last try: claude.com:443 isn't on the plugin's allow-http line. https://claude.com/crawling/bots.json\n",
            "The addresses on line 1 (Bytespider): 1 range\n",
            "The range You.com states (YouBot): 1 range\n",
            "No list, so their claims can't be checked: Google-GeminiNotebook, MistralAI-Training, Meta-ExternalAgent, Meta-WebIndexer, Meta-ExternalFetcher, DuckAssistBot, AI2Bot.\n",
        ] {
            assert!(out.contains(want), "missing {want:?} in:\n{out}");
        }
        assert!(
            !out.contains("OAI-SearchBot\t"),
            "a bot with no requests is listed:\n{out}"
        );
        let empty = Report {
            site: "a.test",
            now: 0,
            rows: vec![],
            reads: HashMap::new(),
            lists: vec![],
            every: 5,
        };
        let out = c.report(&empty);
        assert!(
            out.contains("none from the bots the plugin knows.") && out.contains("nothing yet."),
            "{out}"
        );
    }

    #[test]
    fn words_for_notes() {
        assert_eq!(ordinal(1), "1st");
        assert_eq!(ordinal(2), "2nd");
        assert_eq!(ordinal(3), "3rd");
        assert_eq!(ordinal(11), "11th");
        assert_eq!(ordinal(12), "12th");
        assert_eq!(ordinal(21), "21st");
        assert_eq!(ordinal(41), "41st");
        assert_eq!(ordinal(111), "111th");
        assert_eq!(short(60_000), "1m");
        assert_eq!(short(1000), "1s");
        assert_eq!(short(86_400_000), "1d");
        assert_eq!(short(90_000), "90s");
        assert_eq!(plural(1, "range"), "1 range");
        assert_eq!(plural(0, "range"), "0 ranges");
    }
}
