// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! The rate limiting and WAF plugin's config and its rules, kept apart from
//! the Proxy-Wasm calls so they can be tested on their own. The counting
//! itself is BareProxy's (bareproxy_count), shared by all instances.

use kit::{Item, Line, Net};

/// Paths no real visitor asks for, on almost any site, for `common-rules`.
pub const COMMON_PATHS: &[&str] = &[
    "/.env",
    "/.git/",
    "/.svn/",
    "/.hg/",
    "/.aws/",
    "/.ssh/",
    "/.DS_Store",
    "/.htpasswd",
    "/.htaccess",
    "/phpinfo.php",
    "/server-status",
    "/config.json",
    "/web.config",
    "/backup.sql",
    "/dump.sql",
    "/id_rsa",
];

/// Scanners that say who they are, for `common-rules`.
pub const COMMON_AGENTS: &[&str] = &[
    "sqlmap",
    "nikto",
    "masscan",
    "zgrab",
    "nuclei",
    "acunetix",
    "nessus",
    "wpscan",
    "dirbuster",
    "gobuster",
    "ffuf",
    "openvas",
    "netsparker",
    "w3af",
    "jaeles",
];

#[derive(Debug, Clone, Copy, PartialEq)]
pub enum By {
    Address,
    Site,
}

#[derive(Debug, Clone, PartialEq)]
pub struct Limit {
    pub name: String,
    pub count: u64,
    pub window_ms: u32,
    pub by: By,
    /// Path prefixes it counts; empty for every path.
    pub paths: Vec<String>,
    pub line: usize,
}

#[derive(Debug, Clone, PartialEq)]
pub struct Config {
    pub limits: Vec<Limit>,
    /// Methods allowed; empty for any.
    pub methods: Vec<String>,
    pub max_url: Option<usize>,
    pub max_header: Option<usize>,
    /// Blocked paths, lower case, with the line that blocked each (0 for
    /// the common rules).
    pub block_paths: Vec<(String, usize)>,
    pub block_agents: Vec<(String, usize)>,
    /// Addresses no rule or limit applies to.
    pub allow: Vec<Net>,
    pub forwarded_from: Vec<Net>,
}

/// What the plugin does with a request.
#[derive(Debug, Clone, PartialEq)]
pub enum Verdict {
    Pass,
    /// Turn it away: status, extra headers, body, note.
    Refuse(u32, Vec<(String, String)>, String, String),
}

/// What the plugin needs to know about a request.
pub struct Request<'a> {
    pub addr: &'a str,
    pub method: &'a str,
    /// The path and query as sent.
    pub target: &'a str,
    pub path: &'a str,
    pub headers: &'a [(String, String)],
}

fn size(line: &Line, w: &str) -> Result<usize, String> {
    let up = w.to_ascii_uppercase();
    let (num, mul) = if let Some(n) = up.strip_suffix("KB") {
        (n, 1024)
    } else if let Some(n) = up.strip_suffix('B') {
        (n, 1)
    } else {
        (up.as_str(), 1)
    };
    match num.parse::<usize>() {
        Ok(n) if n > 0 => Ok(n * mul),
        _ => Err(line.err(format!("{w} isn't a size; write 2048 or 8KB"))),
    }
}

impl Config {
    pub fn parse(text: &str) -> Result<Config, String> {
        let mut c = Config {
            limits: vec![],
            methods: vec![],
            max_url: None,
            max_header: None,
            block_paths: vec![],
            block_agents: vec![],
            allow: vec![],
            forwarded_from: vec![],
        };
        for item in kit::parse(text, &[])? {
            let Item::Line(line) = item else {
                unreachable!()
            };
            let a = line.args();
            match line.name() {
                "limit" => c.limits.push(limit(&line)?),
                "methods" => {
                    if a.is_empty() {
                        return Err(line.err(
                            "methods takes the methods the site uses, such as GET HEAD POST",
                        ));
                    }
                    for m in a {
                        if !m.bytes().all(|b| b.is_ascii_alphabetic()) {
                            return Err(line.err(format!("{m} isn't a method")));
                        }
                        c.methods.push(m.to_ascii_uppercase());
                    }
                }
                "max-url" => match a {
                    [w] => c.max_url = Some(size(&line, w)?),
                    _ => return Err(line.err("max-url takes a size, such as 4KB")),
                },
                "max-header" => match a {
                    [w] => c.max_header = Some(size(&line, w)?),
                    _ => return Err(line.err("max-header takes a size, such as 8KB")),
                },
                "block-path" => {
                    if a.is_empty() {
                        return Err(
                            line.err("block-path takes paths, such as /wp-login.php /xmlrpc.php")
                        );
                    }
                    for p in a {
                        if !p.starts_with('/') {
                            return Err(line.err(format!("{p} isn't a path; a path starts with /")));
                        }
                        c.block_paths.push((p.to_ascii_lowercase(), line.no));
                    }
                }
                "block-agent" => {
                    if a.is_empty() {
                        return Err(line.err(
                            "block-agent takes words to find in the User-Agent, such as sqlmap",
                        ));
                    }
                    for w in a {
                        c.block_agents.push((w.to_ascii_lowercase(), line.no));
                    }
                }
                "common-rules" => {
                    if kit::switch(&line)? {
                        c.block_paths
                            .extend(COMMON_PATHS.iter().map(|p| (p.to_ascii_lowercase(), 0)));
                        c.block_agents
                            .extend(COMMON_AGENTS.iter().map(|w| (w.to_string(), 0)));
                    }
                }
                "allow" | "forwarded-from" => {
                    if a.is_empty() {
                        return Err(line.err(format!("{} takes addresses or ranges", line.name())));
                    }
                    for w in a {
                        let n = Net::parse(w).ok_or_else(|| {
                            line.err(format!(
                                "{w} isn't an address or a range such as 192.0.2.0/24"
                            ))
                        })?;
                        if line.name() == "allow" {
                            c.allow.push(n)
                        } else {
                            c.forwarded_from.push(n)
                        }
                    }
                }
                other => return Err(line.err(format!("unknown setting {other}"))),
            }
        }
        if c.limits.is_empty()
            && c.methods.is_empty()
            && c.max_url.is_none()
            && c.max_header.is_none()
            && c.block_paths.is_empty()
            && c.block_agents.is_empty()
        {
            return Err("nothing to do: add a limit line, or rules such as common-rules on".into());
        }
        Ok(c)
    }

    /// The WAF rules, before any counting.
    pub fn rules(&self, r: &Request) -> Verdict {
        let refuse = |status: u32, extra: Vec<(String, String)>, body: &str, note: String| {
            Verdict::Refuse(status, extra, format!("{body}\n"), note)
        };
        if !self.methods.is_empty() && !self.methods.iter().any(|m| m == r.method) {
            return refuse(
                405,
                vec![("allow".into(), self.methods.join(", "))],
                "Method not allowed.",
                format!("WAF: method {} isn't one the site uses", r.method),
            );
        }
        if let Some(max) = self.max_url
            && r.target.len() > max
        {
            return refuse(
                414,
                vec![],
                "URL too long.",
                format!("WAF: URL of {} bytes, over {max}", r.target.len()),
            );
        }
        if let Some(max) = self.max_header
            && let Some((n, v)) = r
                .headers
                .iter()
                .find(|(n, v)| !n.starts_with(':') && n.len() + v.len() > max)
        {
            return refuse(
                431,
                vec![],
                "Request header too large.",
                format!("WAF: header {n} of {} bytes, over {max}", n.len() + v.len()),
            );
        }
        let lower = r.path.to_ascii_lowercase();
        if let Some((p, line)) = self.block_paths.iter().find(|(p, _)| kit::under(&lower, p)) {
            return refuse(
                403,
                vec![],
                "Forbidden.",
                format!("WAF: path {p} is blocked ({})", from(*line)),
            );
        }
        let agent = r
            .headers
            .iter()
            .find(|(n, _)| n == "user-agent")
            .map(|(_, v)| v.to_ascii_lowercase())
            .unwrap_or_default();
        if let Some((w, line)) = self
            .block_agents
            .iter()
            .find(|(w, _)| agent.contains(w.as_str()))
        {
            return refuse(
                403,
                vec![],
                "Forbidden.",
                format!("WAF: user agent has {w} ({})", from(*line)),
            );
        }
        Verdict::Pass
    }

    /// The limits a request counts against, with the key to count it under.
    pub fn keys<'a>(&'a self, addr: &str, path: &str) -> Vec<(&'a Limit, String)> {
        self.limits
            .iter()
            .filter(|l| l.paths.is_empty() || l.paths.iter().any(|p| kit::under(path, p)))
            .map(|l| {
                let who = match l.by {
                    By::Address => addr,
                    By::Site => "site",
                };
                (l, format!("{}|{who}", l.name))
            })
            .collect()
    }
}

fn from(line: usize) -> String {
    if line == 0 {
        "common-rules".into()
    } else {
        format!("line {line}")
    }
}

fn limit(line: &Line) -> Result<Limit, String> {
    let usage = || {
        line.err("write limit NAME COUNT per TIME [by address|site] [path PREFIX...], such as limit login 5 per 1m path /login")
    };
    let a = line.args();
    if a.len() < 4 || a[2] != "per" {
        return Err(usage());
    }
    let count = a[1]
        .parse::<u64>()
        .ok()
        .filter(|n| *n > 0)
        .ok_or_else(usage)?;
    let secs = kit::seconds(line, &a[3])?;
    if secs == 0 || secs > 86400 {
        return Err(line.err("a limit's window is 1s to 1d"));
    }
    let mut by = By::Address;
    let mut paths = vec![];
    let mut rest = &a[4..];
    while let Some(w) = rest.first() {
        match (w.as_str(), rest.get(1)) {
            ("by", Some(x)) if x == "address" => by = By::Address,
            ("by", Some(x)) if x == "site" => by = By::Site,
            ("path", Some(_)) => {
                for p in &rest[1..] {
                    if !p.starts_with('/') {
                        return Err(line.err(format!("{p} isn't a path; a path starts with /")));
                    }
                    paths.push(p.clone());
                }
                break;
            }
            _ => return Err(usage()),
        }
        rest = &rest[2..];
    }
    Ok(Limit {
        name: a[0].clone(),
        count,
        window_ms: (secs * 1000) as u32,
        by,
        paths,
        line: line.no,
    })
}

/// Seconds as a short text: 45s, 3m.
pub fn short(ms: u64) -> String {
    let s = ms.div_ceil(1000);
    if s < 120 {
        format!("{s}s")
    } else if s < 7200 {
        format!("{}m", s.div_ceil(60))
    } else {
        format!("{}h", s.div_ceil(3600))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn req<'a>(method: &'a str, target: &'a str, headers: &'a [(String, String)]) -> Request<'a> {
        let path = target.split('?').next().unwrap();
        Request {
            addr: "203.0.113.9",
            method,
            target,
            path,
            headers,
        }
    }

    fn h(pairs: &[(&str, &str)]) -> Vec<(String, String)> {
        pairs
            .iter()
            .map(|(a, b)| (a.to_string(), b.to_string()))
            .collect()
    }

    const CONF: &str = "methods GET HEAD POST
max-url 64
max-header 100
block-path /wp-login.php /xmlrpc.php /admin/
block-agent EvilBot
common-rules on
limit site 600 per 1m
limit login 5 per 1m path /login /account/
limit api 10000 per 1h by site path /api/
";

    fn status(v: Verdict) -> u32 {
        match v {
            Verdict::Pass => 200,
            Verdict::Refuse(s, ..) => s,
        }
    }

    #[test]
    fn rules() {
        let c = Config::parse(CONF).unwrap();
        let ok = h(&[("user-agent", "Mozilla/5.0")]);
        assert_eq!(status(c.rules(&req("GET", "/", &ok))), 200);
        match c.rules(&req("PUT", "/", &ok)) {
            Verdict::Refuse(405, extra, _, note) => {
                assert_eq!(extra, h(&[("allow", "GET, HEAD, POST")]));
                assert_eq!(note, "WAF: method PUT isn't one the site uses");
            }
            v => panic!("{v:?}"),
        }
        assert_eq!(
            status(c.rules(&req("GET", &format!("/?q={}", "x".repeat(70)), &ok))),
            414
        );
        assert_eq!(
            status(c.rules(&req("GET", "/", &h(&[("cookie", &"x".repeat(200))])))),
            431
        );
        for p in [
            "/wp-login.php",
            "/WP-LOGIN.PHP",
            "/admin/users",
            "/.env",
            "/.git/config",
        ] {
            assert_eq!(status(c.rules(&req("GET", p, &ok))), 403, "{p}");
        }
        for p in ["/administrator", "/wp-login.phpx", "/.environment"] {
            assert_eq!(status(c.rules(&req("GET", p, &ok))), 200, "{p}");
        }
        match c.rules(&req("GET", "/.env", &ok)) {
            Verdict::Refuse(403, _, _, note) => {
                assert_eq!(note, "WAF: path /.env is blocked (common-rules)")
            }
            v => panic!("{v:?}"),
        }
        match c.rules(&req("GET", "/", &h(&[("user-agent", "sqlmap/1.7")]))) {
            Verdict::Refuse(403, _, _, note) => {
                assert_eq!(note, "WAF: user agent has sqlmap (common-rules)")
            }
            v => panic!("{v:?}"),
        }
        match c.rules(&req(
            "GET",
            "/",
            &h(&[("user-agent", "the evilbot crawler")]),
        )) {
            Verdict::Refuse(403, _, _, note) => {
                assert_eq!(note, "WAF: user agent has evilbot (line 5)")
            }
            v => panic!("{v:?}"),
        }
    }

    #[test]
    fn limit_keys() {
        let c = Config::parse(CONF).unwrap();
        let names = |path: &str| -> Vec<String> {
            c.keys("203.0.113.9", path)
                .into_iter()
                .map(|(_, k)| k)
                .collect()
        };
        assert_eq!(names("/"), ["site|203.0.113.9"]);
        assert_eq!(names("/login"), ["site|203.0.113.9", "login|203.0.113.9"]);
        assert_eq!(names("/api/x"), ["site|203.0.113.9", "api|site"]);
        let l = &c.limits[1];
        assert_eq!(
            (l.count, l.window_ms, l.by, l.line),
            (5, 60_000, By::Address, 8)
        );
    }

    #[test]
    fn config_errors() {
        for (text, want) in [
            ("", "nothing to do"),
            (
                "limit login 5 every 1m",
                "line 1: write limit NAME COUNT per TIME",
            ),
            ("limit login five per 1m", "line 1: write limit NAME"),
            (
                "limit login 5 per 2d",
                "line 1: a limit's window is 1s to 1d",
            ),
            (
                "limit login 5 per 1m path login",
                "line 1: login isn't a path",
            ),
            ("limit login 5 per 1m by user", "line 1: write limit NAME"),
            ("methods GET P0ST", "line 1: P0ST isn't a method"),
            ("max-url big", "line 1: big isn't a size"),
            (
                "block-path wp-login.php",
                "line 1: wp-login.php isn't a path",
            ),
            ("allow office", "line 1: office isn't an address"),
            ("common-rules yes", "line 1: common-rules takes on or off"),
            ("ban 1.2.3.4", "line 1: unknown setting ban"),
        ] {
            let err = Config::parse(text).unwrap_err();
            assert!(err.contains(want), "{text}: {err}");
        }
    }

    #[test]
    fn short_times() {
        assert_eq!(short(1), "1s");
        assert_eq!(short(45_000), "45s");
        assert_eq!(short(600_000), "10m");
        assert_eq!(short(3 * 3_600_000), "3h");
    }
}
