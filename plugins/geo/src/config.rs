// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! The geo plugin's config and its decisions, kept apart from the
//! Proxy-Wasm calls so they can be tested on their own.

use kit::{Item, Line, Net};
use std::net::IpAddr;

/// The page those turned away get when the config has none of its own.
pub const DEFAULT_PAGE: &str = "<!doctype html>
<html lang=\"en\">
<head>
<meta charset=\"utf-8\">
<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">
<title>Not available</title>
<style>body{font:18px/1.5 system-ui,sans-serif;max-width:32em;margin:15vh auto;padding:0 1em;color:#222}</style>
</head>
<body>
<h1>Not available here</h1>
<p>Sorry, this site isn't available in your region.</p>
</body>
</html>
";

#[derive(Debug, Clone, PartialEq)]
pub struct Rule {
    pub path: String,
    /// Countries let in; when not empty, only these.
    pub allow: Vec<String>,
    pub block: Vec<String>,
    /// Whether addresses with no country (local, private, not in the
    /// database) get in.
    pub unknown_allowed: bool,
    /// The config line that set the list, for the record.
    pub line: usize,
}

#[derive(Debug, Clone, PartialEq)]
pub struct Config {
    pub database: String,
    /// The request header the country goes in, lower case; None for none.
    pub header: Option<String>,
    pub status: u32,
    /// Seconds between checks of the database file; 0 for never.
    pub reload: u64,
    /// Proxies in front whose X-Forwarded-For is believed.
    pub forwarded_from: Vec<Net>,
    pub page: String,
    /// Longest path first.
    pub rules: Vec<Rule>,
}

#[derive(Default)]
struct Raw {
    path: String,
    allow: Option<(Vec<String>, usize)>,
    block: Option<(Vec<String>, usize)>,
    unknown: Option<bool>,
}

fn countries(line: &Line) -> Result<Vec<String>, String> {
    if line.args().is_empty() {
        return Err(line.err(format!(
            "{} takes country codes, such as DE FR",
            line.name()
        )));
    }
    line.args()
        .iter()
        .map(|w| {
            if w.len() == 2 && w.bytes().all(|b| b.is_ascii_alphabetic()) {
                Ok(w.to_ascii_uppercase())
            } else {
                Err(line.err(format!(
                    "{w} isn't a two-letter country code (ISO 3166), such as DE"
                )))
            }
        })
        .collect()
}

impl Config {
    pub fn parse(text: &str) -> Result<Config, String> {
        let mut c = Config {
            database: String::new(),
            header: Some("x-country".into()),
            status: 403,
            reload: 3600,
            forwarded_from: vec![],
            page: DEFAULT_PAGE.into(),
            rules: vec![],
        };
        let mut raws = vec![Raw {
            path: "/".into(),
            ..Raw::default()
        }];
        for item in kit::parse(text, &["page"])? {
            let line = match item {
                Item::Block(line, body) => {
                    if !line.args().is_empty() {
                        return Err(
                            line.err("page takes no words; the page starts on the next line")
                        );
                    }
                    c.page = body;
                    continue;
                }
                Item::Line(line) => line,
            };
            let a = line.args();
            let top = raws.len() == 1;
            let only_top = |what: &str| -> Result<(), String> {
                if top {
                    Ok(())
                } else {
                    Err(line.err(format!("{what} goes at the top, before any path section")))
                }
            };
            match line.name() {
                "database" => {
                    only_top("database")?;
                    match a {
                        [f] => c.database = f.trim_start_matches('/').to_string(),
                        _ => return Err(line.err("database takes one file, inside a folder the plugin line names with read")),
                    }
                }
                "header" => {
                    only_top("header")?;
                    c.header = match a {
                        [w] if w == "off" => None,
                        [w] if w.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'-')
                            && !w.eq_ignore_ascii_case("host") =>
                        {
                            Some(w.to_ascii_lowercase())
                        }
                        _ => {
                            return Err(
                                line.err("header takes a header name, such as X-Country, or off")
                            );
                        }
                    };
                }
                "status" => {
                    only_top("status")?;
                    c.status = match a {
                        [w] if w == "403" => 403,
                        [w] if w == "451" => 451,
                        _ => return Err(line.err("status takes 403 or 451")),
                    };
                }
                "reload" => {
                    only_top("reload")?;
                    c.reload = match a {
                        [w] if w == "off" => 0,
                        [w] => kit::seconds(&line, w)?.max(1),
                        _ => return Err(line.err("reload takes a time, such as 1h, or off")),
                    };
                }
                "forwarded-from" => {
                    only_top("forwarded-from")?;
                    if a.is_empty() {
                        return Err(
                            line.err("forwarded-from takes the addresses of the proxies in front")
                        );
                    }
                    for w in a {
                        c.forwarded_from.push(Net::parse(w).ok_or_else(|| {
                            line.err(format!(
                                "{w} isn't an address or a range such as 192.0.2.0/24"
                            ))
                        })?);
                    }
                }
                "path" => match a {
                    [p] if p.starts_with('/') => {
                        if raws.iter().any(|r| r.path == *p) {
                            return Err(line.err(format!("path {p} is set twice")));
                        }
                        raws.push(Raw {
                            path: p.clone(),
                            ..Raw::default()
                        });
                    }
                    _ => return Err(line.err("path takes one path prefix, such as /checkout/")),
                },
                "allow" | "block" => {
                    let list = countries(&line)?;
                    let r = raws.last_mut().unwrap();
                    let slot = if line.name() == "allow" {
                        &mut r.allow
                    } else {
                        &mut r.block
                    };
                    match slot {
                        Some((v, _)) => v.extend(list),
                        None => *slot = Some((list, line.no)),
                    }
                }
                "unknown" => {
                    raws.last_mut().unwrap().unknown = Some(match a {
                        [w] if w == "allow" => true,
                        [w] if w == "block" => false,
                        _ => return Err(line.err("unknown takes allow or block")),
                    })
                }
                other => return Err(line.err(format!("unknown setting {other}"))),
            }
        }
        if c.database.is_empty() {
            return Err("no database: add a line such as database dbip-country-lite.mmdb".into());
        }
        let top = &raws[0];
        for r in &raws {
            let allow = r.allow.clone().or(top.allow.clone());
            let block = r.block.clone().or(top.block.clone());
            let line = allow
                .as_ref()
                .map(|x| x.1)
                .or(block.as_ref().map(|x| x.1))
                .unwrap_or(0);
            c.rules.push(Rule {
                path: r.path.clone(),
                allow: allow.map(|x| x.0).unwrap_or_default(),
                block: block.map(|x| x.0).unwrap_or_default(),
                unknown_allowed: r.unknown.or(top.unknown).unwrap_or(true),
                line,
            });
        }
        c.rules.sort_by_key(|r| std::cmp::Reverse(r.path.len()));
        Ok(c)
    }

    pub fn rule(&self, path: &str) -> &Rule {
        self.rules
            .iter()
            .find(|r| kit::under(path, &r.path))
            .unwrap_or(self.rules.last().unwrap())
    }

    /// The client's address: the connection's, or with forwarded-from, the
    /// last address in X-Forwarded-For that isn't one of the proxies.
    pub fn client(&self, source: &str, forwarded: Option<&str>) -> Option<IpAddr> {
        kit::client_addr(source, forwarded, &self.forwarded_from)
    }

    /// Whether a request from a country gets in, and if not, why.
    pub fn decide(&self, path: &str, country: Option<&str>) -> Result<(), String> {
        let r = self.rule(path);
        if r.allow.is_empty() && r.block.is_empty() {
            return Ok(());
        }
        let Some(cc) = country else {
            return if r.unknown_allowed {
                Ok(())
            } else {
                Err(format!(
                    "no country for the address, and unknown is block for {}",
                    r.path
                ))
            };
        };
        if !r.allow.is_empty() && !r.allow.iter().any(|a| a == cc) {
            return Err(format!(
                "{cc} isn't on the allow list for {} (line {})",
                r.path, r.line
            ));
        }
        if r.block.iter().any(|b| b == cc) {
            return Err(format!(
                "{cc} is on the block list for {} (line {})",
                r.path, r.line
            ));
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const CONF: &str = "database GeoLite2-Country.mmdb
block RU KP
forwarded-from 10.0.0.0/8

path /checkout/
  allow DE AT CH
  unknown block
";

    #[test]
    fn rules() {
        let c = Config::parse(CONF).unwrap();
        assert_eq!(c.header.as_deref(), Some("x-country"));
        assert!(c.decide("/", Some("DE")).is_ok());
        assert_eq!(
            c.decide("/news", Some("RU")).unwrap_err(),
            "RU is on the block list for / (line 2)"
        );
        assert!(c.decide("/news", None).is_ok());
        assert!(c.decide("/checkout/pay", Some("AT")).is_ok());
        assert_eq!(
            c.decide("/checkout/pay", Some("FR")).unwrap_err(),
            "FR isn't on the allow list for /checkout/ (line 6)"
        );
        // The section keeps the top's block list too.
        assert!(
            c.decide("/checkout/", None)
                .unwrap_err()
                .contains("unknown is block")
        );
        let open = Config::parse("database x.mmdb").unwrap();
        assert!(open.decide("/", Some("KP")).is_ok());
    }

    #[test]
    fn client_address() {
        let c = Config::parse(CONF).unwrap();
        let ip = |s: &str| s.parse::<IpAddr>().unwrap();
        assert_eq!(
            c.client("203.0.113.9", Some("1.2.3.4")),
            Some(ip("203.0.113.9")),
            "not from a proxy: XFF ignored"
        );
        assert_eq!(
            c.client("10.0.0.5", Some("6.6.6.6, 1.2.3.4")),
            Some(ip("1.2.3.4"))
        );
        assert_eq!(
            c.client("10.0.0.5", Some("6.6.6.6, 1.2.3.4, 10.0.0.9")),
            Some(ip("1.2.3.4"))
        );
        assert_eq!(
            c.client("10.0.0.5", Some("garbage, 10.0.0.9")),
            Some(ip("10.0.0.9"))
        );
        assert_eq!(c.client("10.0.0.5", None), Some(ip("10.0.0.5")));
        let plain = Config::parse("database x.mmdb").unwrap();
        assert_eq!(
            plain.client("10.0.0.5", Some("1.2.3.4")),
            Some(ip("10.0.0.5"))
        );
    }

    #[test]
    fn config_errors() {
        for (text, want) in [
            ("block RU", "no database"),
            ("database a b", "line 1: database takes one file"),
            (
                "database x\nblock Russia",
                "line 2: Russia isn't a two-letter country code",
            ),
            ("database x\nallow", "line 2: allow takes country codes"),
            ("database x\nstatus 404", "line 2: status takes 403 or 451"),
            (
                "database x\nunknown maybe",
                "line 2: unknown takes allow or block",
            ),
            (
                "database x\npath /a/\n  header off",
                "line 3: header goes at the top",
            ),
            (
                "database x\nheader host",
                "line 2: header takes a header name",
            ),
            (
                "database x\nforwarded-from cdn",
                "line 2: cdn isn't an address",
            ),
            (
                "database x\npath /a\npath /a",
                "line 3: path /a is set twice",
            ),
            ("database x\ncountry DE", "line 2: unknown setting country"),
        ] {
            let err = Config::parse(text).unwrap_err();
            assert!(err.contains(want), "{text}: {err}");
        }
    }
}
