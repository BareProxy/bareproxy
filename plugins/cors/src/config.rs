// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! The CORS plugin's config, and its decisions, kept apart from the
//! Proxy-Wasm calls so they can be tested on their own.

use kit::{Item, Line};

/// A rule: what one part of the site allows.
#[derive(Debug, Clone, PartialEq)]
pub struct Rule {
    /// The path prefix it covers; "/" for the whole site.
    pub path: String,
    /// Exact origins, or Any for "*". Empty: no origin may call.
    pub origins: Origins,
    /// Methods a preflight may ask for; "*" allows any.
    pub methods: Vec<String>,
    /// Request headers a preflight may ask for, lower case; "*" allows any.
    pub headers: Vec<String>,
    /// Response headers the page's script may read.
    pub expose: Vec<String>,
    pub credentials: bool,
    /// Seconds a browser may keep a preflight's answer.
    pub max_age: u64,
}

#[derive(Debug, Clone, PartialEq)]
pub enum Origins {
    Any,
    List(Vec<String>),
}

/// The settings of a section as written, before the defaults fill the gaps.
#[derive(Default)]
struct Raw {
    line: usize,
    path: String,
    origins: Option<Origins>,
    methods: Option<Vec<String>>,
    headers: Option<Vec<String>>,
    expose: Option<Vec<String>>,
    credentials: Option<bool>,
    max_age: Option<u64>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct Config {
    /// Longest path first, so the first rule that covers a path is its rule.
    pub rules: Vec<Rule>,
}

impl Config {
    pub fn parse(text: &str) -> Result<Config, String> {
        let mut raws = vec![Raw {
            path: "/".into(),
            ..Raw::default()
        }];
        for item in kit::parse(text, &[])? {
            let Item::Line(line) = item else {
                unreachable!()
            };
            let args = line.args();
            if line.name() == "path" {
                match args {
                    [p] if p.starts_with('/') => {
                        if raws.iter().any(|r| r.path == *p) {
                            return Err(line.err(format!("path {p} is set twice")));
                        }
                        raws.push(Raw {
                            line: line.no,
                            path: p.clone(),
                            ..Raw::default()
                        });
                    }
                    _ => return Err(line.err("path takes one path prefix, such as /api/")),
                }
                continue;
            }
            let raw = raws.last_mut().unwrap();
            let need = |what: &str| -> Result<(), String> {
                if args.is_empty() {
                    Err(line.err(format!("{} takes {what}", line.name())))
                } else {
                    Ok(())
                }
            };
            match line.name() {
                "origins" => {
                    need("one or more origins, such as https://app.example.com, or * or none")?;
                    raw.origins = Some(origins(&line)?);
                }
                "methods" => {
                    need("one or more methods, such as GET POST, or *")?;
                    for m in args {
                        if !m.bytes().all(|b| b.is_ascii_alphabetic()) && m != "*" {
                            return Err(line.err(format!("{m} isn't a method")));
                        }
                    }
                    raw.methods = Some(args.iter().map(|m| m.to_ascii_uppercase()).collect());
                }
                "headers" | "expose" => {
                    need("one or more header names, such as content-type")?;
                    for h in args {
                        if !h
                            .bytes()
                            .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
                            && h != "*"
                        {
                            return Err(line.err(format!("{h} isn't a header name")));
                        }
                    }
                    let list = Some(args.iter().map(|h| h.to_ascii_lowercase()).collect());
                    if line.name() == "headers" {
                        raw.headers = list
                    } else {
                        raw.expose = list
                    }
                }
                "credentials" => raw.credentials = Some(kit::switch(&line)?),
                "max-age" => match args {
                    [w] => raw.max_age = Some(kit::seconds(&line, w)?),
                    _ => return Err(line.err("max-age takes a time, such as 10m")),
                },
                other => return Err(line.err(format!("unknown setting {other}"))),
            }
        }

        let top = &raws[0];
        let mut rules = Vec::new();
        for raw in &raws {
            let r = Rule {
                path: raw.path.clone(),
                origins: raw
                    .origins
                    .clone()
                    .or(top.origins.clone())
                    .unwrap_or(Origins::List(vec![])),
                methods: raw
                    .methods
                    .clone()
                    .or(top.methods.clone())
                    .unwrap_or_else(|| ["GET", "HEAD", "POST"].map(String::from).to_vec()),
                headers: raw
                    .headers
                    .clone()
                    .or(top.headers.clone())
                    .unwrap_or_default(),
                expose: raw
                    .expose
                    .clone()
                    .or(top.expose.clone())
                    .unwrap_or_default(),
                credentials: raw.credentials.or(top.credentials).unwrap_or(false),
                max_age: raw.max_age.or(top.max_age).unwrap_or(600),
            };
            if r.credentials && r.origins == Origins::Any {
                let at = if raw.line == 0 {
                    String::new()
                } else {
                    format!("line {}: ", raw.line)
                };
                return Err(format!(
                    "{at}credentials on needs a list of origins, not *: browsers refuse that pair, \
                     and it would let any site call with a visitor's cookies"
                ));
            }
            rules.push(r);
        }
        if rules.iter().all(|r| r.origins == Origins::List(vec![])) {
            return Err("no origins: add a line such as origins https://app.example.com".into());
        }
        rules.sort_by_key(|r| std::cmp::Reverse(r.path.len()));
        Ok(Config { rules })
    }

    /// The rule for a path.
    pub fn rule(&self, path: &str) -> &Rule {
        self.rules
            .iter()
            .find(|r| kit::under(path, &r.path))
            .unwrap_or(self.rules.last().unwrap())
    }

    /// Decides what to do with a request. `preflight` holds the method and
    /// headers a preflight asks for (Access-Control-Request-Method and
    /// -Headers), when the request is an OPTIONS preflight.
    pub fn decide(
        &self,
        path: &str,
        origin: Option<&str>,
        preflight: Option<(&str, &str)>,
    ) -> Decision {
        let rule = self.rule(path);
        let vary = rule.origins != Origins::Any || rule.credentials;
        let Some(origin) = origin else {
            return Decision::Response {
                set: vec![],
                vary,
                note: None,
            };
        };
        let allowed = match &rule.origins {
            Origins::Any => true,
            Origins::List(l) => l.iter().any(|o| o.eq_ignore_ascii_case(origin)),
        };
        let allow_origin = if rule.origins == Origins::Any && !rule.credentials {
            "*"
        } else {
            origin
        };
        let mut set = Vec::new();
        if allowed {
            set.push((
                "access-control-allow-origin".into(),
                allow_origin.to_string(),
            ));
            if rule.credentials {
                set.push(("access-control-allow-credentials".into(), "true".into()));
            }
        }

        let Some((method, asked)) = preflight else {
            if !allowed {
                return Decision::Response {
                    set: vec![],
                    vary,
                    note: Some(format!(
                        "CORS: origin {origin} isn't on the list for {}",
                        rule.path
                    )),
                };
            }
            if !rule.expose.is_empty() {
                set.push((
                    "access-control-expose-headers".into(),
                    rule.expose.join(", "),
                ));
            }
            return Decision::Response {
                set,
                vary,
                note: None,
            };
        };

        let mut vary_list = vec![
            "Access-Control-Request-Method",
            "Access-Control-Request-Headers",
        ];
        if vary {
            vary_list.insert(0, "Origin");
        }
        let refuse = |why: String| Decision::Answer {
            status: 403,
            headers: vec![
                ("vary".into(), vary_list.join(", ")),
                ("content-type".into(), "text/plain; charset=utf-8".into()),
            ],
            body: format!("{why}\n"),
            note: why,
        };
        if !allowed {
            return refuse(format!(
                "CORS: origin {origin} isn't on the list for {}",
                rule.path
            ));
        }
        let method = method.trim();
        let any_method = rule.methods.iter().any(|m| m == "*");
        if !any_method && !rule.methods.iter().any(|m| m == method) {
            return refuse(format!(
                "CORS: method {method} isn't allowed for {}",
                rule.path
            ));
        }
        let asked: Vec<String> = asked
            .split(',')
            .map(|h| h.trim().to_ascii_lowercase())
            .filter(|h| !h.is_empty())
            .collect();
        let any_header = rule.headers.iter().any(|h| h == "*");
        if let Some(h) = asked
            .iter()
            .find(|h| !any_header && !rule.headers.contains(h))
        {
            return refuse(format!("CORS: header {h} isn't allowed for {}", rule.path));
        }
        let methods = if any_method {
            method.to_string()
        } else {
            rule.methods.join(", ")
        };
        set.push(("access-control-allow-methods".into(), methods));
        let headers = if any_header {
            asked.join(", ")
        } else {
            rule.headers.join(", ")
        };
        if !headers.is_empty() {
            set.push(("access-control-allow-headers".into(), headers));
        }
        set.push(("access-control-max-age".into(), rule.max_age.to_string()));
        set.push(("vary".into(), vary_list.join(", ")));
        Decision::Answer {
            status: 204,
            headers: set,
            body: String::new(),
            note: String::new(),
        }
    }
}

fn origins(line: &Line) -> Result<Origins, String> {
    match line.args() {
        [w] if w == "*" => return Ok(Origins::Any),
        [w] if w == "none" => return Ok(Origins::List(vec![])),
        _ => {}
    }
    let mut list = Vec::new();
    for w in line.args() {
        if w == "*" || w == "none" {
            return Err(line.err(format!("{w} goes on a line of its own")));
        }
        if w == "null" {
            return Err(
                line.err("null isn't allowed: sandboxed frames and local files all send it")
            );
        }
        let ok = match w.split_once("://") {
            Some((scheme, host)) => {
                (scheme == "https" || scheme == "http")
                    && !host.is_empty()
                    && !host.contains(['/', '?', '#', '*', '@'])
            }
            None => false,
        };
        if !ok {
            return Err(line.err(format!(
                "{w} isn't an origin; write scheme://host or scheme://host:port, such as https://app.example.com"
            )));
        }
        list.push(w.to_ascii_lowercase());
    }
    Ok(Origins::List(list))
}

/// What the plugin does with a request.
#[derive(Debug, Clone, PartialEq)]
pub enum Decision {
    /// Answer a preflight at the proxy.
    Answer {
        status: u32,
        headers: Vec<(String, String)>,
        body: String,
        note: String,
    },
    /// Let the request go on, and on the response set these headers (any
    /// Access-Control-* headers from the backend are dropped first), add
    /// Origin to Vary when `vary`, and note `note`.
    Response {
        set: Vec<(String, String)>,
        vary: bool,
        note: Option<String>,
    },
}

#[cfg(test)]
mod tests {
    use super::*;

    const API: &str = "origins https://app.example.com https://admin.example.com
methods GET POST PUT DELETE
headers content-type authorization
expose x-request-id
credentials on
max-age 1h

path /public/
  origins *
  credentials off
  headers *
";

    fn header<'a>(h: &'a [(String, String)], k: &str) -> Option<&'a str> {
        h.iter().find(|(n, _)| n == k).map(|(_, v)| v.as_str())
    }

    #[test]
    fn preflight_allowed() {
        let c = Config::parse(API).unwrap();
        let d = c.decide(
            "/orders",
            Some("https://app.example.com"),
            Some(("PUT", "Content-Type, Authorization")),
        );
        let Decision::Answer {
            status, headers, ..
        } = d
        else {
            panic!("{d:?}")
        };
        assert_eq!(status, 204);
        assert_eq!(
            header(&headers, "access-control-allow-origin"),
            Some("https://app.example.com")
        );
        assert_eq!(
            header(&headers, "access-control-allow-credentials"),
            Some("true")
        );
        assert_eq!(
            header(&headers, "access-control-allow-methods"),
            Some("GET, POST, PUT, DELETE")
        );
        assert_eq!(
            header(&headers, "access-control-allow-headers"),
            Some("content-type, authorization")
        );
        assert_eq!(header(&headers, "access-control-max-age"), Some("3600"));
        assert_eq!(
            header(&headers, "vary"),
            Some("Origin, Access-Control-Request-Method, Access-Control-Request-Headers")
        );
    }

    #[test]
    fn preflight_refused() {
        let c = Config::parse(API).unwrap();
        for (origin, method, hdrs, why) in [
            (
                "https://evil.example",
                "GET",
                "",
                "origin https://evil.example isn't on the list for /",
            ),
            (
                "https://app.example.com.evil.example",
                "GET",
                "",
                "isn't on the list",
            ),
            (
                "https://app.example.com",
                "PATCH",
                "",
                "method PATCH isn't allowed for /",
            ),
            (
                "https://app.example.com",
                "GET",
                "x-secret",
                "header x-secret isn't allowed for /",
            ),
        ] {
            let d = c.decide("/orders", Some(origin), Some((method, hdrs)));
            let Decision::Answer {
                status,
                headers,
                note,
                ..
            } = d
            else {
                panic!("{d:?}")
            };
            assert_eq!(status, 403);
            assert!(note.contains(why), "{note}");
            assert_eq!(header(&headers, "access-control-allow-origin"), None);
        }
    }

    #[test]
    fn real_requests() {
        let c = Config::parse(API).unwrap();
        let d = c.decide("/orders", Some("https://admin.example.com"), None);
        let Decision::Response { set, vary, note } = d else {
            panic!("{d:?}")
        };
        assert!(vary && note.is_none());
        assert_eq!(
            header(&set, "access-control-allow-origin"),
            Some("https://admin.example.com")
        );
        assert_eq!(
            header(&set, "access-control-expose-headers"),
            Some("x-request-id")
        );

        let d = c.decide("/orders", Some("https://evil.example"), None);
        let Decision::Response { set, vary, note } = d else {
            panic!("{d:?}")
        };
        assert!(set.is_empty() && vary);
        assert_eq!(
            note.unwrap(),
            "CORS: origin https://evil.example isn't on the list for /"
        );

        // No Origin header: nothing to add, but caches still need Vary.
        let d = c.decide("/orders", None, None);
        assert_eq!(
            d,
            Decision::Response {
                set: vec![],
                vary: true,
                note: None
            }
        );
    }

    #[test]
    fn a_path_of_its_own() {
        let c = Config::parse(API).unwrap();
        let d = c.decide("/public/feed", Some("https://anyone.example"), None);
        let Decision::Response { set, vary, .. } = d else {
            panic!("{d:?}")
        };
        assert!(!vary);
        assert_eq!(header(&set, "access-control-allow-origin"), Some("*"));
        assert_eq!(header(&set, "access-control-allow-credentials"), None);
        // It keeps what it doesn't set itself from the top.
        assert_eq!(
            header(&set, "access-control-expose-headers"),
            Some("x-request-id")
        );
        let d = c.decide(
            "/public/feed",
            Some("https://anyone.example"),
            Some(("DELETE", "x-anything")),
        );
        let Decision::Answer {
            status, headers, ..
        } = d
        else {
            panic!("{d:?}")
        };
        assert_eq!(status, 204);
        assert_eq!(
            header(&headers, "access-control-allow-headers"),
            Some("x-anything")
        );
        assert_eq!(header(&headers, "access-control-max-age"), Some("3600"));
    }

    #[test]
    fn config_errors() {
        for (text, want) in [
            (
                "origins *\ncredentials on",
                "credentials on needs a list of origins, not *",
            ),
            (
                "origins https://a.example\npath /x\n  origins *\n  credentials on",
                "line 2: credentials on needs",
            ),
            (
                "methods GET",
                "no origins: add a line such as origins https://app.example.com",
            ),
            (
                "origins https://a.example/",
                "line 1: https://a.example/ isn't an origin",
            ),
            (
                "origins app.example.com",
                "line 1: app.example.com isn't an origin",
            ),
            ("origins https://*.example.com", "isn't an origin"),
            ("origins null", "line 1: null isn't allowed"),
            (
                "origins https://a.example *",
                "line 1: * goes on a line of its own",
            ),
            (
                "origins https://a.example\npath api",
                "line 2: path takes one path prefix",
            ),
            (
                "origins https://a.example\npath /a\npath /a",
                "line 3: path /a is set twice",
            ),
            (
                "origins https://a.example\nheaders x:y",
                "line 2: x:y isn't a header name",
            ),
            (
                "origins https://a.example\nallow-all on",
                "line 2: unknown setting allow-all",
            ),
        ] {
            let err = Config::parse(text).unwrap_err();
            assert!(err.contains(want), "{text}: {err}");
        }
    }
}
