// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! The bot challenges plugin's config and its checks, kept apart from the
//! Proxy-Wasm calls so they can be tested on their own: who gets the
//! challenge, the puzzle and its answer, and the pass a solved puzzle earns.
//!
//! The plugin keeps nothing per visitor. A puzzle is an HMAC of when it was
//! made, its work and who it was made for, so the answer can be checked
//! without having stored the puzzle; a pass is an HMAC of when it ends and
//! who it was given to.

use hmac::{Hmac, Mac};
pub use kit::{
    BOT_WORDS, cookie_value, html_escape, is_navigation, looks_like_browser, query_param,
    safe_return,
};
use kit::{Item, Line, Net};
use sha2::{Digest, Sha256};
use std::net::IpAddr;

type HmacSha256 = Hmac<Sha256>;

/// The challenge page; {{DATA}} becomes the puzzle as JSON.
pub const PAGE: &str = include_str!("page.html");

/// Paths never challenged, whatever the config says: a bot may read
/// robots.txt, and browsers and certificate checks fetch the others alone.
pub const ALWAYS_SKIP: &[&str] = &["/robots.txt", "/favicon.ico", "/.well-known/"];

/// How long a browser has to solve a puzzle, in seconds.
pub const SOLVE_WITHIN: u64 = 600;

#[derive(Debug, Clone, Copy, PartialEq)]
pub enum Who {
    /// Clients whose User-Agent says browser and not bot.
    Browsers,
    /// Every client without a pass.
    All,
}

/// What a pass is tied to, besides the User-Agent.
#[derive(Debug, Clone, Copy, PartialEq)]
pub enum Bind {
    /// The address's network: a /24 for IPv4, a /64 for IPv6.
    Prefix,
    Address,
    None,
}

#[derive(Debug, Clone, Copy, PartialEq)]
pub struct After {
    pub count: u64,
    pub window_ms: u32,
    pub line: usize,
}

#[derive(Debug, Clone, PartialEq)]
pub struct Config {
    pub who: Who,
    /// Leading zero bits the answer's hash needs.
    pub work: u32,
    /// Seconds a pass lasts.
    pub valid: u64,
    pub bind: Bind,
    /// Page loads a client may make without a pass before it's challenged.
    pub after: Option<After>,
    /// Networks challenged even under `after`: ranges, and files of them.
    pub always: Vec<Net>,
    pub always_files: Vec<String>,
    pub allow: Vec<Net>,
    pub skip: Vec<String>,
    /// A request header, lower case, and the value that lets a request by:
    /// set by a plugin before this one, such as AI crawler control.
    pub trust_header: Option<(String, String)>,
    pub status: u32,
    pub cookie: String,
    pub key: Option<Vec<u8>>,
    pub forwarded_from: Vec<Net>,
    /// The path the page sends its answer to.
    pub endpoint: String,
}

fn nets(line: &Line, words: &[String]) -> Result<Vec<Net>, String> {
    if words.is_empty() {
        return Err(line.err(format!("{} takes addresses or ranges", line.name())));
    }
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

fn path_ok(p: &str) -> bool {
    p.starts_with('/') && !p.contains(char::is_whitespace)
}

impl Config {
    pub fn parse(text: &str) -> Result<Config, String> {
        let mut c = Config {
            who: Who::Browsers,
            work: 16,
            valid: 7 * 86400,
            bind: Bind::Prefix,
            after: None,
            always: vec![],
            always_files: vec![],
            allow: vec![],
            skip: vec![],
            trust_header: None,
            status: 403,
            cookie: "bp_pass".into(),
            key: None,
            forwarded_from: vec![],
            endpoint: "/.bareproxy-challenge".into(),
        };
        for item in kit::parse(text, &[])? {
            let (Item::Line(line) | Item::Block(line, _)) = item;
            let a = line.args();
            match line.name() {
                "challenge" => {
                    c.who = match a {
                        [w] if w == "browsers" => Who::Browsers,
                        [w] if w == "all" => Who::All,
                        _ => return Err(line.err("challenge takes browsers or all")),
                    };
                }
                "work" => {
                    c.work = match a {
                        [w] => match w.parse::<u32>() {
                            Ok(n) if (8..=24).contains(&n) => n,
                            _ => return Err(line.err("work takes a number of bits from 8 to 24")),
                        },
                        _ => return Err(line.err("work takes a number of bits from 8 to 24")),
                    };
                }
                "valid" => {
                    let [w] = a else {
                        return Err(line.err("valid takes a time, such as 7d"));
                    };
                    c.valid = kit::seconds(&line, w)?;
                    if !(3600..=30 * 86400).contains(&c.valid) {
                        return Err(line.err("valid is 1h to 30d"));
                    }
                }
                "bind" => {
                    c.bind = match a {
                        [w] if w == "prefix" => Bind::Prefix,
                        [w] if w == "address" => Bind::Address,
                        [w] if w == "none" => Bind::None,
                        _ => return Err(line.err("bind takes prefix, address or none")),
                    };
                }
                "after" => {
                    let usage = || line.err("write after COUNT per TIME, such as after 10 per 1m");
                    let [n, per, t] = a else {
                        return Err(usage());
                    };
                    if per != "per" {
                        return Err(usage());
                    }
                    let count = match n.parse::<u64>() {
                        Ok(n) if n > 0 => n,
                        _ => return Err(line.err(format!("{n} isn't a count of page loads"))),
                    };
                    let secs = kit::seconds(&line, t)?;
                    if !(1..=86400).contains(&secs) {
                        return Err(line.err("after's time is 1s to 1d"));
                    }
                    c.after = Some(After {
                        count,
                        window_ms: (secs * 1000) as u32,
                        line: line.no,
                    });
                }
                "always" => {
                    if a.is_empty() {
                        return Err(line.err(
                            "always takes addresses, ranges or files of them, such as always 192.0.2.0/24 datacenters.txt",
                        ));
                    }
                    for w in a {
                        // A word of digits, hex, dots, colons and slashes is
                        // meant as an address; anything else is a file.
                        let addrish = w.bytes().any(|b| b.is_ascii_digit())
                            && w.bytes()
                                .all(|b| b.is_ascii_hexdigit() || b".:/".contains(&b));
                        match Net::parse(w) {
                            Some(n) => c.always.push(n),
                            None if addrish => {
                                return Err(line.err(format!(
                                    "{w} isn't an address or a range such as 192.0.2.0/24"
                                )));
                            }
                            None => c.always_files.push(w.trim_start_matches('/').to_string()),
                        }
                    }
                }
                "allow" => c.allow.extend(nets(&line, a)?),
                "forwarded-from" => c.forwarded_from.extend(nets(&line, a)?),
                "skip" => {
                    if a.is_empty() {
                        return Err(line.err("skip takes path prefixes, such as /api/ /feed.xml"));
                    }
                    for p in a {
                        if !path_ok(p) {
                            return Err(line.err(format!("{p} isn't a path; a path starts with /")));
                        }
                        c.skip.push(p.clone());
                    }
                }
                "trust-header" => {
                    let parsed = match a {
                        [w] => w.split_once('=').filter(|(n, v)| {
                            !n.is_empty()
                                && !v.is_empty()
                                && n.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'-')
                        }),
                        _ => None,
                    };
                    let Some((n, v)) = parsed else {
                        return Err(line.err(
                            "trust-header takes NAME=VALUE, such as trust-header X-Bot-Check=verified",
                        ));
                    };
                    c.trust_header = Some((n.to_ascii_lowercase(), v.to_string()));
                }
                "status" => {
                    c.status = match a {
                        [w] if ["200", "403", "429", "503"].contains(&w.as_str()) => {
                            w.parse().unwrap()
                        }
                        _ => return Err(line.err("status takes 200, 403, 429 or 503")),
                    };
                }
                "cookie" => match a {
                    [w] if !w.is_empty()
                        && w.len() <= 64
                        && w.bytes()
                            .all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-') =>
                    {
                        c.cookie = w.clone()
                    }
                    _ => return Err(line.err("cookie takes a name: letters, digits, _ and -")),
                },
                "key" => match a {
                    [w] if w.len() >= 32 => c.key = Some(w.as_bytes().to_vec()),
                    [_] => {
                        return Err(line.err(
                            "the key is too short: use 32 characters or more, such as the output of openssl rand -hex 32",
                        ));
                    }
                    _ => return Err(line.err("key takes one secret, 32 characters or more")),
                },
                "endpoint" => match a {
                    [p] if path_ok(p) && p.len() > 1 => c.endpoint = p.clone(),
                    _ => {
                        return Err(
                            line.err("endpoint takes a path, such as /.bareproxy-challenge")
                        );
                    }
                },
                other => return Err(line.err(format!("unknown setting {other}"))),
            }
        }
        Ok(c)
    }

    /// Whether a path is never challenged.
    pub fn skipped(&self, path: &str) -> bool {
        ALWAYS_SKIP
            .iter()
            .copied()
            .chain(self.skip.iter().map(|s| s.as_str()))
            .any(|p| kit::under(path, p))
    }

    /// What a pass and a puzzle are tied to: the client's network or
    /// address, as `bind` says, and its User-Agent.
    pub fn bind_text(&self, ip: Option<IpAddr>, ua: &str) -> String {
        let at = match (self.bind, ip) {
            (Bind::None, _) | (_, None) => String::new(),
            (Bind::Address, Some(ip)) => ip.to_canonical().to_string(),
            (Bind::Prefix, Some(ip)) => network(ip, 24, 64),
        };
        format!("{at}|{ua}")
    }

    /// The challenge page for a puzzle, with the request it came from.
    pub fn page(&self, puzzle: &str, t: u64, target: &str) -> String {
        let data = format!(
            "{{\"c\":{},\"t\":{t},\"w\":{},\"r\":{},\"e\":{}}}",
            json_str(puzzle),
            self.work,
            json_str(&safe_return(target)),
            json_str(&self.endpoint)
        );
        PAGE.replace("{{DATA}}", &data)
    }

    /// The Set-Cookie value for a pass.
    pub fn set_cookie(&self, value: &str, https: bool) -> String {
        format!(
            "{}={value}; Path=/; Max-Age={}; HttpOnly; SameSite=Lax{}",
            self.cookie,
            self.valid,
            if https { "; Secure" } else { "" }
        )
    }

    /// Checks an answer sent to the endpoint.
    pub fn verify(&self, key: &[u8], query: &str, bind: &str, now: u64) -> Answer {
        let to = query_param(query, "r")
            .map(|r| safe_return(&r))
            .unwrap_or_else(|| "/".into());
        let fail = |why: &str| Answer::Fail(why.to_string(), to.clone());
        let (Some(t), Some(w), Some(n)) = (
            query_param(query, "t").and_then(|x| x.parse::<u64>().ok()),
            query_param(query, "w").and_then(|x| x.parse::<u32>().ok()),
            query_param(query, "n"),
        ) else {
            return fail("the answer is missing or malformed");
        };
        if n.is_empty() || n.len() > 16 || !n.bytes().all(|b| b.is_ascii_digit()) {
            return fail("the answer is missing or malformed");
        }
        if t + SOLVE_WITHIN < now || t > now + 60 {
            return fail("the puzzle is more than 10 minutes old");
        }
        if w < self.work || w > 32 {
            return fail(&format!(
                "the puzzle asked for {w} bits of work, under {}",
                self.work
            ));
        }
        if zeros(&puzzle(key, t, w, bind), &n) < w {
            return fail("the answer doesn't solve the puzzle");
        }
        let exp = now + self.valid;
        Answer::Pass {
            exp,
            value: pass_value(key, exp, bind),
            to,
        }
    }
}

/// What an answer earns.
#[derive(Debug, Clone, PartialEq)]
pub enum Answer {
    /// A pass until exp, the cookie's value, and where to go back to.
    Pass { exp: u64, value: String, to: String },
    /// Why it failed, and where to go back to.
    Fail(String, String),
}

/// The network an address is in: the first v4 bits of IPv4, v6 of IPv6.
pub fn network(ip: IpAddr, v4: u32, v6: u32) -> String {
    match ip.to_canonical() {
        IpAddr::V4(a) => {
            let n = u32::from(a) & (u32::MAX << (32 - v4));
            format!("{}/{v4}", std::net::Ipv4Addr::from(n))
        }
        IpAddr::V6(a) => {
            let n = u128::from(a) & (u128::MAX << (128 - v6));
            format!("{}/{v6}", std::net::Ipv6Addr::from(n))
        }
    }
}

fn mac(key: &[u8], msg: &str) -> HmacSha256 {
    let mut m = HmacSha256::new_from_slice(key).expect("HMAC takes any key length");
    m.update(msg.as_bytes());
    m
}

fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

fn unhex(s: &str) -> Option<Vec<u8>> {
    if !s.len().is_multiple_of(2) {
        return None;
    }
    (0..s.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(s.get(i..i + 2)?, 16).ok())
        .collect()
}

/// A puzzle: 64 hex characters, made for a time, a work and a client.
pub fn puzzle(key: &[u8], t: u64, work: u32, bind: &str) -> String {
    hex(&mac(key, &format!("puzzle|{t}|{work}|{bind}"))
        .finalize()
        .into_bytes())
}

/// The leading zero bits of SHA-256 of puzzle:answer.
pub fn zeros(puzzle: &str, answer: &str) -> u32 {
    let d = Sha256::new()
        .chain_update(puzzle.as_bytes())
        .chain_update(b":")
        .chain_update(answer.as_bytes())
        .finalize();
    let mut z = 0;
    for b in d {
        z += b.leading_zeros();
        if b != 0 {
            break;
        }
    }
    z
}

/// A pass's cookie value: when it ends, then 16 bytes of HMAC in hex.
pub fn pass_value(key: &[u8], exp: u64, bind: &str) -> String {
    let m = mac(key, &format!("pass|{exp}|{bind}"))
        .finalize()
        .into_bytes();
    format!("{exp}.{}", hex(&m[..16]))
}

/// When a pass ends, if it's one the key made for this client and it
/// hasn't ended. The comparison takes the same time whatever it finds.
pub fn pass_ok(key: &[u8], value: &str, bind: &str, now: u64) -> Option<u64> {
    let (exp, tag) = value.split_once('.')?;
    let exp: u64 = exp.parse().ok()?;
    let tag = unhex(tag).filter(|t| t.len() == 16)?;
    if exp <= now {
        return None;
    }
    mac(key, &format!("pass|{exp}|{bind}"))
        .verify_truncated_left(&tag)
        .ok()
        .map(|_| exp)
}

/// A JSON string that's safe inside a script element: <, > and & are
/// written as escapes, so no text can end the element.
pub fn json_str(s: &str) -> String {
    let mut out = String::from("\"");
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '<' => out.push_str("\\u003c"),
            '>' => out.push_str("\\u003e"),
            '&' => out.push_str("\\u0026"),
            '\u{2028}' => out.push_str("\\u2028"),
            '\u{2029}' => out.push_str("\\u2029"),
            c if (c as u32) < 0x20 => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push(c),
        }
    }
    out.push('"');
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    const KEY: &[u8] = b"0123456789abcdef0123456789abcdef";

    fn ip(s: &str) -> Option<IpAddr> {
        Some(s.parse().unwrap())
    }

    /// Solves a puzzle the way the page does.
    fn solve(c: &str, w: u32) -> String {
        (0u64..)
            .map(|n| n.to_string())
            .find(|n| zeros(c, n) >= w)
            .unwrap()
    }

    #[test]
    fn defaults_and_settings() {
        let d = Config::parse("").unwrap();
        assert_eq!(
            (d.who, d.work, d.valid, d.bind, d.status),
            (Who::Browsers, 16, 604800, Bind::Prefix, 403)
        );
        assert_eq!(
            (d.cookie.as_str(), d.endpoint.as_str()),
            ("bp_pass", "/.bareproxy-challenge")
        );
        assert!(d.after.is_none() && d.key.is_none() && d.trust_header.is_none());
        let c = Config::parse(
            "challenge all\nwork 20\nvalid 1d\nbind address\nafter 10 per 1m\nalways 192.0.2.0/24 2001:db8::/32 datacenters.txt\nallow 203.0.113.7\nskip /api/ /feed.xml\ntrust-header X-Bot-Check=verified\nstatus 429\ncookie pass\nkey 0123456789abcdef0123456789abcdef\nforwarded-from 10.0.0.0/8\nendpoint /.check\n",
        )
        .unwrap();
        assert_eq!(
            (c.who, c.work, c.valid, c.bind, c.status),
            (Who::All, 20, 86400, Bind::Address, 429)
        );
        assert_eq!(
            c.after,
            Some(After {
                count: 10,
                window_ms: 60_000,
                line: 5
            })
        );
        assert_eq!(c.always.len(), 2);
        assert_eq!(c.always_files, ["datacenters.txt"]);
        assert_eq!(
            c.trust_header,
            Some(("x-bot-check".into(), "verified".into()))
        );
        assert_eq!(
            (c.cookie.as_str(), c.endpoint.as_str()),
            ("pass", "/.check")
        );
        assert_eq!(c.key.as_deref(), Some(KEY));
        assert!(kit::any_contains(&c.allow, "203.0.113.7"));
        assert!(
            c.skipped("/api/x")
                && c.skipped("/feed.xml")
                && c.skipped("/robots.txt")
                && c.skipped("/.well-known/security.txt")
        );
        assert!(!c.skipped("/apix") && !c.skipped("/"));
    }

    #[test]
    fn refused() {
        for (text, want) in [
            ("challenge some", "line 1: challenge takes browsers or all"),
            (
                "work 30",
                "line 1: work takes a number of bits from 8 to 24",
            ),
            ("work x", "line 1: work takes a number of bits from 8 to 24"),
            ("valid 10m", "line 1: valid is 1h to 30d"),
            ("valid 60d", "line 1: valid is 1h to 30d"),
            ("bind cookie", "line 1: bind takes prefix, address or none"),
            ("after 10 every 1m", "line 1: write after COUNT per TIME"),
            ("after 0 per 1m", "line 1: 0 isn't a count of page loads"),
            ("after 5 per 2d", "line 1: after's time is 1s to 1d"),
            (
                "always",
                "line 1: always takes addresses, ranges or files of them",
            ),
            (
                "always 192.0.2.0/33",
                "line 1: 192.0.2.0/33 isn't an address or a range",
            ),
            (
                "allow example.com",
                "line 1: example.com isn't an address or a range",
            ),
            ("allow", "line 1: allow takes addresses or ranges"),
            ("skip api", "line 1: api isn't a path"),
            (
                "trust-header X-Bot-Check",
                "line 1: trust-header takes NAME=VALUE",
            ),
            ("trust-header =x", "line 1: trust-header takes NAME=VALUE"),
            ("status 404", "line 1: status takes 200, 403, 429 or 503"),
            ("cookie a;b", "line 1: cookie takes a name"),
            ("key short", "line 1: the key is too short"),
            ("endpoint check", "line 1: endpoint takes a path"),
            ("bogus", "line 1: unknown setting bogus"),
        ] {
            let err = Config::parse(text).unwrap_err();
            assert!(err.starts_with(want), "{text}: {err}");
        }
    }

    #[test]
    fn browsers_and_bots() {
        for (ua, want) in [
            (
                "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36",
                true,
            ),
            (
                "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1",
                true,
            ),
            (
                "Mozilla/5.0 (X11; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0",
                true,
            ),
            (
                "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
                false,
            ),
            (
                "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; GPTBot/1.4; +https://openai.com/gptbot",
                false,
            ),
            (
                "Mozilla/5.0 (Linux; Android 5.0) AppleWebKit/537.36 (KHTML, like Gecko) Mobile Safari/537.36 (compatible; Bytespider; spider-feedback@bytedance.com)",
                false,
            ),
            ("curl/8.5.0", false),
            ("python-requests/2.32.3", false),
            ("", false),
        ] {
            assert_eq!(looks_like_browser(ua), want, "{ua}");
        }
        assert!(is_navigation(Some("navigate"), None));
        assert!(!is_navigation(Some("no-cors"), Some("text/html")));
        assert!(is_navigation(
            None,
            Some("text/html,application/xhtml+xml,*/*;q=0.8")
        ));
        assert!(!is_navigation(None, Some("image/avif,image/webp,*/*")));
        assert!(!is_navigation(None, None));
    }

    #[test]
    fn binding() {
        let ua = "Mozilla/5.0 Firefox/131.0";
        let p = Config::parse("").unwrap();
        assert_eq!(
            p.bind_text(ip("203.0.113.77"), ua),
            format!("203.0.113.0/24|{ua}")
        );
        assert_eq!(
            p.bind_text(ip("::ffff:203.0.113.77"), ua),
            format!("203.0.113.0/24|{ua}")
        );
        assert_eq!(
            p.bind_text(ip("2001:db8:1:2:3:4:5:6"), ua),
            format!("2001:db8:1:2::/64|{ua}")
        );
        let a = Config::parse("bind address").unwrap();
        assert_eq!(
            a.bind_text(ip("203.0.113.77"), ua),
            format!("203.0.113.77|{ua}")
        );
        let n = Config::parse("bind none").unwrap();
        assert_eq!(n.bind_text(ip("203.0.113.77"), ua), format!("|{ua}"));
        assert_eq!(network("10.1.2.3".parse().unwrap(), 32, 128), "10.1.2.3/32");
    }

    #[test]
    fn puzzles_and_answers() {
        let c = Config::parse("work 12").unwrap();
        let bind = "203.0.113.0/24|Mozilla/5.0";
        let now = 1_791_547_500;
        let p = puzzle(KEY, now, 12, bind);
        assert_eq!(p.len(), 64);
        assert!(p.bytes().all(|b| b.is_ascii_hexdigit()));
        assert_ne!(p, puzzle(KEY, now, 12, "198.51.100.0/24|Mozilla/5.0"));
        assert_ne!(
            p,
            puzzle(b"another key, also 32 characters!", now, 12, bind)
        );
        let n = solve(&p, 12);
        let q = format!("t={now}&w=12&n={n}&r=%2Fdocs%2Fa%3Fx%3D1");
        let Answer::Pass { exp, value, to } = c.verify(KEY, &q, bind, now + 5) else {
            panic!("a right answer failed");
        };
        assert_eq!((exp, to.as_str()), (now + 5 + 604800, "/docs/a?x=1"));
        assert_eq!(pass_ok(KEY, &value, bind, now + 6), Some(exp));
        // A pass is for one client, until it ends, from this key.
        assert_eq!(
            pass_ok(KEY, &value, "198.51.100.0/24|Mozilla/5.0", now + 6),
            None
        );
        assert_eq!(pass_ok(KEY, &value, bind, exp), None);
        assert_eq!(
            pass_ok(b"another key, also 32 characters!", &value, bind, now),
            None
        );
        let forged = format!("{}.{}", exp + 86400, value.split_once('.').unwrap().1);
        assert_eq!(pass_ok(KEY, &forged, bind, now), None);
        for bad in ["", "x", "1.2", "99999999999.zz", &format!("{exp}.00")] {
            assert_eq!(pass_ok(KEY, bad, bind, now), None, "{bad}");
        }
        // Answers that don't hold.
        let fails = |q: &str, at: u64| match c.verify(KEY, q, bind, at) {
            Answer::Fail(why, _) => why,
            Answer::Pass { .. } => "passed".into(),
        };
        assert_eq!(
            fails(&format!("t={now}&w=12&n={n}"), now + 601),
            "the puzzle is more than 10 minutes old"
        );
        assert_eq!(
            fails(&format!("t={now}&w=8&n={n}"), now),
            "the puzzle asked for 8 bits of work, under 12"
        );
        assert_eq!(
            fails(&format!("t={now}&w=12&n=x{n}"), now),
            "the answer is missing or malformed"
        );
        assert_eq!(
            fails(&format!("t={now}&w=12"), now),
            "the answer is missing or malformed"
        );
        let wrong = (0u64..)
            .map(|x| x.to_string())
            .find(|x| zeros(&p, x) < 12)
            .unwrap();
        assert_eq!(
            fails(&format!("t={now}&w=12&n={wrong}"), now),
            "the answer doesn't solve the puzzle"
        );
        // Solved for another client.
        assert_eq!(
            c.verify(KEY, &q, "198.51.100.0/24|Mozilla/5.0", now),
            Answer::Fail(
                "the answer doesn't solve the puzzle".into(),
                "/docs/a?x=1".into()
            )
        );
    }

    #[test]
    fn going_back_safely() {
        for (r, want) in [
            ("/docs/a?x=1", "/docs/a?x=1"),
            ("/", "/"),
            ("//evil.example/x", "/"),
            ("/\\evil.example", "/"),
            ("https://evil.example/", "/"),
            ("docs", "/"),
            ("/a\nb", "/"),
        ] {
            assert_eq!(safe_return(r), want, "{r:?}");
        }
        assert_eq!(
            query_param("t=1&r=%2F%2Fevil", "r")
                .map(|r| safe_return(&r))
                .as_deref(),
            Some("/")
        );
        assert_eq!(query_param("a=1&b=x+y%21", "b").as_deref(), Some("x y!"));
        assert_eq!(query_param("a=%zz", "a"), None);
        assert_eq!(query_param("a=1", "b"), None);
    }

    #[test]
    fn the_page_and_cookies() {
        let c = Config::parse("work 12").unwrap();
        let page = c.page("ab12", 77, "/x?q=</script><script>alert(1)</script>&y=\"");
        assert!(page.contains(r#"{"c":"ab12","t":77,"w":12,"r":"/x?q=\u003c/script\u003e\u003cscript\u003ealert(1)\u003c/script\u003e\u0026y=\"","e":"/.bareproxy-challenge"}"#), "{page}");
        assert_eq!(
            page.matches("</script>").count(),
            2,
            "the target ended a script element"
        );
        assert!(!page.contains("{{DATA}}"));
        assert_eq!(
            cookie_value("a=1; bp_pass=123.abc; b=2", "bp_pass"),
            Some("123.abc")
        );
        assert_eq!(cookie_value("xbp_pass=1", "bp_pass"), None);
        assert_eq!(
            c.set_cookie("v", true),
            "bp_pass=v; Path=/; Max-Age=604800; HttpOnly; SameSite=Lax; Secure"
        );
        assert_eq!(
            c.set_cookie("v", false),
            "bp_pass=v; Path=/; Max-Age=604800; HttpOnly; SameSite=Lax"
        );
        assert_eq!(
            html_escape("/a?b=1&c=\"<x>'"),
            "/a?b=1&amp;c=&quot;&lt;x&gt;&#39;"
        );
        assert_eq!(json_str("a\u{1}b\u{2028}"), "\"a\\u0001b\\u2028\"");
    }

    #[test]
    fn zero_bits() {
        // The count is of bits, across byte edges: an answer for 12 bits
        // hashes to a zero byte, then a byte under 16.
        let n = solve("ab", 12);
        let d = Sha256::digest(format!("ab:{n}").as_bytes());
        assert!(d[0] == 0 && d[1] < 16, "{d:x?}");
        assert!(zeros("ab", &n) >= 12);
        let d = Sha256::digest(b"x:1");
        let mut want = 0;
        for b in d {
            want += b.leading_zeros();
            if b != 0 {
                break;
            }
        }
        assert_eq!(zeros("x", "1"), want);
    }
}
