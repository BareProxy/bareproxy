// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! The splits plugin's config and how it picks a visitor's variant, kept
//! apart from the Proxy-Wasm calls so they can be tested on their own.

use kit::{Item, Line, Net};

#[derive(Debug, Clone, PartialEq)]
pub struct Variant {
    pub name: String,
    /// Its share, out of the sum of all shares.
    pub share: u32,
}

#[derive(Debug, Clone, PartialEq)]
pub enum Match {
    Address(Vec<Net>),
    /// A request header equals a value (case-insensitive).
    Header(String, String),
    Cookie(String, String),
    Query(String, String),
}

/// A rule that sends matching requests to a variant on purpose.
#[derive(Debug, Clone, PartialEq)]
pub struct Force {
    pub line: usize,
    pub variant: usize,
    pub when: Match,
}

#[derive(Debug, Clone, PartialEq)]
pub struct Config {
    pub variants: Vec<Variant>,
    pub forces: Vec<Force>,
    /// The request header the variant goes in, lower case.
    pub header: String,
    pub cookie: String,
    /// Seconds the cookie lasts.
    pub cookie_age: u64,
}

/// What the plugin decided for a request.
#[derive(Debug, Clone, PartialEq)]
pub struct Choice {
    pub variant: usize,
    /// Why, for the record.
    pub why: String,
    /// Whether to set the cookie on the response.
    pub set_cookie: bool,
}

/// What the plugin needs to know about a request.
pub struct Request<'a> {
    pub addr: &'a str,
    pub header: &'a dyn Fn(&str) -> Option<String>,
    pub query: &'a str,
    /// A number to pick with, the same for one request.
    pub roll: u64,
}

fn token(w: &str) -> bool {
    !w.is_empty()
        && w.bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_' || b == b'.')
}

impl Config {
    pub fn parse(text: &str) -> Result<Config, String> {
        let mut c = Config {
            variants: vec![],
            forces: vec![],
            header: "x-variant".into(),
            cookie: "bp_variant".into(),
            cookie_age: 30 * 86400,
        };
        let mut forces: Vec<(Line, String, Match)> = vec![];
        for item in kit::parse(text, &[])? {
            let Item::Line(line) = item else {
                unreachable!()
            };
            let a = line.args();
            match line.name() {
                "variant" => match a {
                    [name, share] => {
                        if !token(name) {
                            return Err(line.err(format!("{name} isn't a name: use letters, digits, - _ and .")));
                        }
                        if c.variants.iter().any(|v| v.name == *name) {
                            return Err(line.err(format!("variant {name} is set twice")));
                        }
                        let share = share.trim_end_matches('%').parse::<u32>().map_err(|_| {
                            line.err(format!("{share} isn't a share; write a whole number, such as 95 or 5"))
                        })?;
                        c.variants.push(Variant { name: name.clone(), share });
                    }
                    _ => return Err(line.err("variant takes a name and a share: variant canary 5")),
                },
                "force" => {
                    let usage = || {
                        line.err(
                            "write force VARIANT address RANGE..., force VARIANT header NAME VALUE, \
                             force VARIANT cookie NAME=VALUE or force VARIANT query NAME=VALUE",
                        )
                    };
                    let pair = |w: &str| -> Result<(String, String), String> {
                        match w.split_once('=') {
                            Some((k, v)) if token(k) && !v.is_empty() => Ok((k.to_string(), v.to_string())),
                            _ => Err(line.err(format!("{w} isn't NAME=VALUE"))),
                        }
                    };
                    let when = match a {
                        [_, kind, rest @ ..] if kind == "address" && !rest.is_empty() => {
                            let mut nets = vec![];
                            for w in rest {
                                nets.push(Net::parse(w).ok_or_else(|| {
                                    line.err(format!("{w} isn't an address or a range such as 192.0.2.0/24"))
                                })?);
                            }
                            Match::Address(nets)
                        }
                        [_, kind, name, value] if kind == "header" && token(name) => {
                            Match::Header(name.to_ascii_lowercase(), value.clone())
                        }
                        [_, kind, w] if kind == "cookie" => {
                            let (k, v) = pair(w)?;
                            Match::Cookie(k, v)
                        }
                        [_, kind, w] if kind == "query" => {
                            let (k, v) = pair(w)?;
                            Match::Query(k, v)
                        }
                        _ => return Err(usage()),
                    };
                    forces.push((line.clone(), a[0].clone(), when));
                }
                "header" => match a {
                    [h] if token(h) && !h.eq_ignore_ascii_case("host") => c.header = h.to_ascii_lowercase(),
                    _ => return Err(line.err("header takes a header name, such as X-Variant")),
                },
                "cookie" => match a {
                    [name] | [name, _] if !token(name) => {
                        return Err(line.err(format!("{name} isn't a cookie name")));
                    }
                    [name] => c.cookie = name.clone(),
                    [name, age] => {
                        c.cookie = name.clone();
                        c.cookie_age = kit::seconds(&line, age)?;
                    }
                    _ => return Err(line.err("cookie takes a name and, if you like, how long it lasts: cookie bp_variant 30d")),
                },
                other => return Err(line.err(format!("unknown setting {other}"))),
            }
        }
        if c.variants.len() < 2 {
            return Err(
                "a split needs at least two variants: variant stable 95 and variant canary 5"
                    .into(),
            );
        }
        if c.variants.iter().all(|v| v.share == 0) {
            return Err("every variant's share is 0: give at least one a share".into());
        }
        for (line, name, when) in forces {
            let variant = c
                .variants
                .iter()
                .position(|v| v.name == name)
                .ok_or_else(|| line.err(format!("no variant named {name}")))?;
            c.forces.push(Force {
                line: line.no,
                variant,
                when,
            });
        }
        Ok(c)
    }

    /// Picks a request's variant: a force rule first, then the visitor's
    /// cookie if it names a variant with a share, then by the shares.
    pub fn choose(&self, r: &Request) -> Choice {
        for f in &self.forces {
            let (hit, what) = match &f.when {
                Match::Address(nets) => (
                    kit::any_contains(nets, r.addr),
                    format!("address {}", r.addr),
                ),
                Match::Header(h, v) => (
                    (r.header)(h).is_some_and(|x| x.trim().eq_ignore_ascii_case(v)),
                    format!("header {h}"),
                ),
                Match::Cookie(k, v) => (cookie(r, k).as_deref() == Some(v), format!("cookie {k}")),
                Match::Query(k, v) => (param(r.query, k) == Some(v), format!("query {k}={v}")),
            };
            if hit {
                // A query force is a preview link: it sticks, like a share.
                let set_cookie = matches!(f.when, Match::Query(..));
                return Choice {
                    variant: f.variant,
                    why: format!("forced by {what} (line {})", f.line),
                    set_cookie,
                };
            }
        }
        if let Some(name) = cookie(r, &self.cookie)
            && let Some(i) = self
                .variants
                .iter()
                .position(|v| v.name == name && v.share > 0)
        {
            return Choice {
                variant: i,
                why: format!("kept by the {} cookie", self.cookie),
                set_cookie: false,
            };
        }
        let total: u64 = self.variants.iter().map(|v| v.share as u64).sum();
        let mut x = r.roll % total;
        let mut pick = 0;
        for (i, v) in self.variants.iter().enumerate() {
            if x < v.share as u64 {
                pick = i;
                break;
            }
            x -= v.share as u64;
        }
        let v = &self.variants[pick];
        Choice {
            variant: pick,
            why: format!("assigned by share ({} of {total})", v.share),
            set_cookie: true,
        }
    }

    /// The Set-Cookie value that keeps a visitor on a variant.
    pub fn set_cookie(&self, variant: usize, https: bool) -> String {
        format!(
            "{}={}; Path=/; Max-Age={}; SameSite=Lax; HttpOnly{}",
            self.cookie,
            self.variants[variant].name,
            self.cookie_age,
            if https { "; Secure" } else { "" }
        )
    }
}

/// A cookie's value from the request's Cookie headers.
fn cookie(r: &Request, name: &str) -> Option<String> {
    let all = (r.header)("cookie")?;
    all.split(';').find_map(|c| {
        let (k, v) = c.trim().split_once('=')?;
        (k == name).then(|| v.trim_matches('"').to_string())
    })
}

fn param<'a>(query: &'a str, name: &str) -> Option<&'a str> {
    query.split('&').find_map(|p| {
        let (k, v) = p.split_once('=')?;
        (k == name).then_some(v)
    })
}

/// A number from the request's ID, for picking: the ID is random, and the
/// hash spreads it evenly.
pub fn roll(id: &str, nanos: u64) -> u64 {
    let mut h: u64 = 0xcbf2_9ce4_8422_2325 ^ nanos;
    for b in id.bytes() {
        h ^= b as u64;
        h = h.wrapping_mul(0x0100_0000_01b3);
    }
    // A final mix, so nearby inputs land far apart.
    h ^= h >> 33;
    h = h.wrapping_mul(0xff51_afd7_ed55_8ccd);
    h ^ h >> 33
}

#[cfg(test)]
mod tests {
    use super::*;

    const CONF: &str = "variant stable 95
variant canary 5%
force canary address 10.0.0.0/8
force canary header x-beta yes
force canary cookie beta=1
force stable query variant=stable
force canary query variant=canary
";

    fn req<'a>(
        addr: &'a str,
        hdr: &'a dyn Fn(&str) -> Option<String>,
        query: &'a str,
        roll: u64,
    ) -> Request<'a> {
        Request {
            addr,
            header: hdr,
            query,
            roll,
        }
    }

    #[test]
    fn shares() {
        let c = Config::parse(CONF).unwrap();
        let none = |_: &str| None;
        let mut counts = [0u32; 2];
        for i in 0..100_000u64 {
            let ch = c.choose(&req(
                "203.0.113.1",
                &none,
                "",
                roll(&format!("{i:016x}"), 0),
            ));
            assert!(ch.set_cookie);
            counts[ch.variant] += 1;
        }
        // About 5 percent, within a few tenths of a point.
        let canary = counts[1] as f64 / 1000.0;
        assert!((4.5..5.5).contains(&canary), "canary got {canary}%");
        let ch = c.choose(&req("203.0.113.1", &none, "", 99));
        assert_eq!(ch.why, "assigned by share (5 of 100)");
    }

    #[test]
    fn cookies_keep_visitors() {
        let c = Config::parse(CONF).unwrap();
        let canary = |h: &str| (h == "cookie").then(|| "theme=dark; bp_variant=canary".to_string());
        // Whatever the roll, the cookie wins.
        for roll in [0, 50, 99] {
            let ch = c.choose(&req("203.0.113.1", &canary, "", roll));
            assert_eq!((ch.variant, ch.set_cookie), (1, false));
            assert_eq!(ch.why, "kept by the bp_variant cookie");
        }
        // A cookie for a variant that's gone or down to 0 is assigned again.
        let gone = |h: &str| (h == "cookie").then(|| "bp_variant=old".to_string());
        assert!(c.choose(&req("203.0.113.1", &gone, "", 0)).set_cookie);
        let drained = Config::parse("variant stable 100\nvariant canary 0").unwrap();
        assert_eq!(
            drained.choose(&req("203.0.113.1", &canary, "", 1)).variant,
            0
        );
    }

    #[test]
    fn forces() {
        let c = Config::parse(CONF).unwrap();
        let none = |_: &str| None;
        let ch = c.choose(&req("10.1.2.3", &none, "", 0));
        assert_eq!(
            (ch.variant, ch.why.as_str(), ch.set_cookie),
            (1, "forced by address 10.1.2.3 (line 3)", false)
        );
        let beta = |h: &str| (h == "x-beta").then(|| "YES".to_string());
        assert_eq!(
            c.choose(&req("203.0.113.1", &beta, "", 0)).why,
            "forced by header x-beta (line 4)"
        );
        let cookie = |h: &str| (h == "cookie").then(|| "bp_variant=stable; beta=1".to_string());
        assert_eq!(
            c.choose(&req("203.0.113.1", &cookie, "", 0)).variant,
            1,
            "a force beats the split cookie"
        );
        let ch = c.choose(&req("203.0.113.1", &none, "a=1&variant=canary", 0));
        assert_eq!((ch.variant, ch.set_cookie), (1, true));
        assert_eq!(
            c.choose(&req("10.0.0.1", &none, "variant=stable", 0))
                .variant,
            1,
            "in file order"
        );
    }

    #[test]
    fn cookie_value() {
        let c = Config::parse("variant a 50\nvariant b 50\ncookie ab 7d").unwrap();
        assert_eq!(
            c.set_cookie(1, true),
            "ab=b; Path=/; Max-Age=604800; SameSite=Lax; HttpOnly; Secure"
        );
        assert_eq!(
            c.set_cookie(0, false),
            "ab=a; Path=/; Max-Age=604800; SameSite=Lax; HttpOnly"
        );
    }

    #[test]
    fn config_errors() {
        for (text, want) in [
            ("variant stable 100", "at least two variants"),
            ("variant a 0\nvariant b 0", "every variant's share is 0"),
            ("variant a x\nvariant b 1", "line 1: x isn't a share"),
            ("variant a 1\nvariant a 1", "line 2: variant a is set twice"),
            ("variant a/b 1\nvariant c 1", "line 1: a/b isn't a name"),
            (
                "variant a 1\nvariant b 1\nforce c address 10.0.0.0/8",
                "line 3: no variant named c",
            ),
            (
                "variant a 1\nvariant b 1\nforce a address office",
                "line 3: office isn't an address",
            ),
            (
                "variant a 1\nvariant b 1\nforce a cookie beta",
                "line 3: beta isn't NAME=VALUE",
            ),
            (
                "variant a 1\nvariant b 1\nforce a planet mars",
                "line 3: write force VARIANT",
            ),
            (
                "variant a 1\nvariant b 1\ncookie a;b",
                "line 3: a;b isn't a cookie name",
            ),
            (
                "variant a 1\nvariant b 1\nweights 1 2",
                "line 3: unknown setting weights",
            ),
        ] {
            let err = Config::parse(text).unwrap_err();
            assert!(err.contains(want), "{text}: {err}");
        }
    }
}
