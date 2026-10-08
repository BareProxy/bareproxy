// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! The header and rewrite rules plugin's config, and its decisions, kept
//! apart from the Proxy-Wasm calls so they can be tested on their own.

use kit::{Item, Line};

/// The security headers added to every response that lacks them, unless
/// `security off`. Strict-Transport-Security goes on https responses only.
pub const SECURITY: [(&str, &str); 4] = [
    ("x-content-type-options", "nosniff"),
    ("referrer-policy", "strict-origin-when-cross-origin"),
    ("x-frame-options", "SAMEORIGIN"),
    ("strict-transport-security", "max-age=31536000"),
];

/// Headers the plugin won't touch: they belong to the connection, the
/// routing or BareProxy's own tracing.
const HANDS_OFF: [&str; 8] = [
    "host",
    "content-length",
    "transfer-encoding",
    "connection",
    "upgrade",
    "keep-alive",
    "te",
    "bareproxy-id",
];

#[derive(Debug, Clone, PartialEq)]
pub enum Change {
    Set(String, String),
    Add(String, String),
    Remove(String),
}

impl Change {
    fn describe(&self) -> String {
        match self {
            Change::Set(n, _) => format!("set {n}"),
            Change::Add(n, _) => format!("added {n}"),
            Change::Remove(n) => format!("removed {n}"),
        }
    }
}

#[derive(Debug, Clone, PartialEq)]
struct Section {
    path: String,
    request: Vec<Change>,
    response: Vec<Change>,
}

#[derive(Debug, Clone, PartialEq)]
enum Kind {
    Redirect(u32),
    Rewrite,
}

/// A redirect or a rewrite.
#[derive(Debug, Clone, PartialEq)]
struct Move {
    line: usize,
    kind: Kind,
    /// An exact path, or a prefix when it ends in *.
    from: String,
    to: String,
    /// A condition: the header holds the word.
    when: Option<(String, String)>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct Config {
    security: bool,
    /// The first covers the whole site.
    sections: Vec<Section>,
    /// In file order; the first that matches a request is the one.
    moves: Vec<Move>,
}

/// What to do with a request.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct RequestPlan {
    /// Answer with a redirect: status, Location, and a header to name in
    /// Vary when the redirect depended on one.
    pub redirect: Option<(u32, String, Option<String>)>,
    /// Route the request on this path (with its query) instead.
    pub rewrite: Option<String>,
    pub changes: Vec<Change>,
    pub notes: Vec<String>,
}

impl Config {
    pub fn parse(text: &str) -> Result<Config, String> {
        let mut c = Config {
            security: true,
            sections: vec![Section {
                path: "/".into(),
                request: vec![],
                response: vec![],
            }],
            moves: vec![],
        };
        for item in kit::parse(text, &[])? {
            let Item::Line(line) = item else {
                unreachable!()
            };
            let in_section = c.sections.len() > 1;
            match line.name() {
                "security" => {
                    if in_section {
                        return Err(line.err("security goes at the top, before any path section"));
                    }
                    c.security = kit::switch(&line)?
                }
                "path" => match line.args() {
                    [p] if p.starts_with('/') => {
                        if c.sections.iter().any(|s| s.path == *p) {
                            return Err(line.err(format!("path {p} is set twice")));
                        }
                        c.sections.push(Section {
                            path: p.clone(),
                            request: vec![],
                            response: vec![],
                        });
                    }
                    _ => return Err(line.err("path takes one path prefix, such as /assets/")),
                },
                "request" | "response" => {
                    let ch = change(&line)?;
                    let s = c.sections.last_mut().unwrap();
                    if line.name() == "request" {
                        s.request.push(ch)
                    } else {
                        s.response.push(ch)
                    }
                }
                "redirect" | "rewrite" => {
                    if in_section {
                        return Err(line.err(format!(
                            "{} goes at the top, before any path section: it names its own path",
                            line.name()
                        )));
                    }
                    c.moves.push(moves(&line)?);
                }
                other => return Err(line.err(format!("unknown setting {other}"))),
            }
        }
        Ok(c)
    }

    /// Decides what happens to a request before routing. `header` gives a
    /// request header's value by its lower-case name.
    pub fn on_request(
        &self,
        path: &str,
        query: &str,
        header: &dyn Fn(&str) -> Option<String>,
    ) -> RequestPlan {
        let mut plan = RequestPlan::default();
        for m in &self.moves {
            let Some(to) = target(&m.from, &m.to, path) else {
                continue;
            };
            if let Some((h, word)) = &m.when
                && !has_word(&header(h).unwrap_or_default(), word)
            {
                continue;
            }
            let with_query = if query.is_empty() || to.contains('?') {
                to.clone()
            } else {
                format!("{to}?{query}")
            };
            match m.kind {
                Kind::Redirect(code) => {
                    plan.notes
                        .push(format!("redirected {path} to {to} (line {})", m.line));
                    plan.redirect =
                        Some((code, with_query, m.when.as_ref().map(|(h, _)| h.clone())));
                    return plan;
                }
                Kind::Rewrite => {
                    plan.notes
                        .push(format!("rewrote {path} to {to} (line {})", m.line));
                    plan.rewrite = Some(with_query);
                    break;
                }
            }
        }
        for s in self.sections_for(path) {
            plan.changes.extend(s.request.iter().cloned());
        }
        if !plan.changes.is_empty() {
            plan.notes.push(describe("request headers", &plan.changes));
        }
        plan
    }

    /// The changes to a response. `has` says whether the response has a
    /// header already; `https` whether the request came over TLS.
    pub fn on_response(
        &self,
        path: &str,
        https: bool,
        has: &dyn Fn(&str) -> bool,
    ) -> (Vec<Change>, Option<String>) {
        let mut out = Vec::new();
        let mut added = 0;
        if self.security {
            for (n, v) in SECURITY {
                if (https || n != "strict-transport-security") && !has(n) {
                    out.push(Change::Set(n.into(), v.into()));
                    added += 1;
                }
            }
        }
        let mut own = Vec::new();
        for s in self.sections_for(path) {
            own.extend(s.response.iter().cloned());
        }
        let mut parts = Vec::new();
        if added > 0 {
            parts.push(format!(
                "added {added} security header{}",
                if added == 1 { "" } else { "s" }
            ));
        }
        if !own.is_empty() {
            parts.push(describe("", &own).trim_start_matches(": ").to_string());
        }
        out.extend(own);
        let note = (!parts.is_empty()).then(|| format!("response headers: {}", parts.join("; ")));
        (out, note)
    }

    fn sections_for<'a>(&'a self, path: &'a str) -> impl Iterator<Item = &'a Section> + 'a {
        self.sections
            .iter()
            .filter(move |s| kit::under(path, &s.path))
    }
}

fn describe(what: &str, changes: &[Change]) -> String {
    let list: Vec<String> = changes.iter().map(Change::describe).collect();
    format!("{what}: {}", list.join(", "))
}

fn change(line: &Line) -> Result<Change, String> {
    let a = line.args();
    let usage = || {
        line.err(format!(
            "write {0} set NAME VALUE, {0} add NAME VALUE or {0} remove NAME",
            line.name()
        ))
    };
    if a.len() < 2 {
        return Err(usage());
    }
    let name = a[1].to_ascii_lowercase();
    if !name
        .bytes()
        .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
    {
        return Err(line.err(format!("{} isn't a header name", a[1])));
    }
    if HANDS_OFF.contains(&name.as_str()) {
        return Err(line.err(format!(
            "{name} belongs to BareProxy, so the plugin leaves it alone"
        )));
    }
    let value = a[2..].join(" ");
    match a[0].as_str() {
        "set" | "add" if value.is_empty() => {
            Err(line.err(format!("{} {} needs a value", a[0], name)))
        }
        "set" => Ok(Change::Set(name, value)),
        "add" => Ok(Change::Add(name, value)),
        "remove" if a.len() == 2 => Ok(Change::Remove(name)),
        _ => Err(usage()),
    }
}

fn moves(line: &Line) -> Result<Move, String> {
    let redirect = line.name() == "redirect";
    let usage = || {
        if redirect {
            line.err("write redirect FROM TO [301|302|307|308] [when HEADER has WORD]")
        } else {
            line.err("write rewrite FROM TO [when HEADER has WORD]")
        }
    };
    let a = line.args();
    if a.len() < 2 {
        return Err(usage());
    }
    let (from, to) = (a[0].clone(), a[1].clone());
    let mut rest = &a[2..];
    let mut code = None;
    if redirect
        && let Some(w) = rest
            .first()
            .filter(|w| w.bytes().all(|b| b.is_ascii_digit()))
    {
        match w.parse::<u32>() {
            Ok(n @ (301 | 302 | 307 | 308)) => code = Some(n),
            _ => {
                return Err(line.err(format!(
                    "{w} isn't a redirect status; use 301, 302, 307 or 308"
                )));
            }
        }
        rest = &rest[1..];
    }
    let when = match rest {
        [] => None,
        [w, h, has, word] if w == "when" && has == "has" => {
            Some((h.to_ascii_lowercase(), word.to_ascii_lowercase()))
        }
        _ => return Err(usage()),
    };
    if !from.starts_with('/') || from[..from.len() - 1].contains('*') {
        return Err(line.err(format!(
            "{from} isn't a path or a prefix; write /old or /old/*, with * only at the end"
        )));
    }
    let absolute = to.starts_with("https://") || to.starts_with("http://");
    if !(to.starts_with('/') || redirect && absolute) {
        return Err(line.err(if redirect {
            format!("{to} isn't a path or a full http:// or https:// URL")
        } else {
            format!("{to} isn't a path; a rewrite stays on this site")
        }));
    }
    if to[..to.len() - 1].contains('*') || to.ends_with('*') && !from.ends_with('*') {
        return Err(line.err(format!(
            "{to} can end in * only when {from} does, and * goes only at the end"
        )));
    }
    if !redirect && from == to {
        return Err(line.err("a rewrite to the same path does nothing"));
    }
    // A redirect that depends on a header shouldn't be kept for good.
    let code = code.unwrap_or(if when.is_some() { 302 } else { 301 });
    Ok(Move {
        line: line.no,
        kind: if redirect {
            Kind::Redirect(code)
        } else {
            Kind::Rewrite
        },
        from,
        to,
        when,
    })
}

/// Where a path goes under a move, or None when the move doesn't match.
fn target(from: &str, to: &str, path: &str) -> Option<String> {
    match from.strip_suffix('*') {
        None => (path == from).then(|| to.to_string()),
        Some(prefix) => {
            let rest = path.strip_prefix(prefix)?;
            Some(match to.strip_suffix('*') {
                Some(t) => format!("{t}{rest}"),
                None => to.to_string(),
            })
        }
    }
}

/// Whether a header's value holds a word: one of its comma, semicolon or
/// space separated parts is the word, or starts with it and a dash (so de
/// matches de-DE in Accept-Language).
fn has_word(value: &str, word: &str) -> bool {
    value
        .split([',', ';', ' '])
        .map(|p| p.trim().to_ascii_lowercase())
        .any(|p| p == word || p.starts_with(word) && p[word.len()..].starts_with('-'))
}

#[cfg(test)]
mod tests {
    use super::*;

    const SITE: &str = "
redirect /old-page /new-page
redirect /blog/* https://blog.example.com/* 308
redirect / /de/ when accept-language has de
rewrite /docs/* /manual/*

request set x-from-proxy 1
response set cache-control no-cache
response remove server

path /assets/
  response set cache-control public, max-age=31536000, immutable
";

    fn none(_: &str) -> Option<String> {
        None
    }

    #[test]
    fn redirects() {
        let c = Config::parse(SITE).unwrap();
        let p = c.on_request("/old-page", "a=1", &none);
        assert_eq!(p.redirect, Some((301, "/new-page?a=1".into(), None)));
        assert_eq!(p.notes, ["redirected /old-page to /new-page (line 2)"]);
        let p = c.on_request("/blog/2024/post", "", &none);
        assert_eq!(
            p.redirect,
            Some((308, "https://blog.example.com/2024/post".into(), None))
        );
        assert_eq!(c.on_request("/blogger", "", &none).redirect, None);
        assert_eq!(c.on_request("/old-page/x", "", &none).redirect, None);

        let german =
            |h: &str| (h == "accept-language").then(|| "de-DE,de;q=0.9,en;q=0.8".to_string());
        let p = c.on_request("/", "", &german);
        assert_eq!(
            p.redirect,
            Some((302, "/de/".into(), Some("accept-language".into())))
        );
        let danish = |h: &str| (h == "accept-language").then(|| "da,denmark".to_string());
        assert_eq!(c.on_request("/", "", &danish).redirect, None);
        assert_eq!(c.on_request("/", "", &none).redirect, None);
    }

    #[test]
    fn rewrites_and_request_headers() {
        let c = Config::parse(SITE).unwrap();
        let p = c.on_request("/docs/intro", "v=2", &none);
        assert_eq!(p.redirect, None);
        assert_eq!(p.rewrite, Some("/manual/intro?v=2".into()));
        assert_eq!(p.changes, [Change::Set("x-from-proxy".into(), "1".into())]);
        assert_eq!(
            p.notes,
            [
                "rewrote /docs/intro to /manual/intro (line 5)",
                "request headers: set x-from-proxy"
            ]
        );
    }

    #[test]
    fn response_headers() {
        let c = Config::parse(SITE).unwrap();
        let (ch, note) = c.on_response("/assets/app.js", true, &|n| n == "x-frame-options");
        assert_eq!(
            ch,
            [
                Change::Set("x-content-type-options".into(), "nosniff".into()),
                Change::Set(
                    "referrer-policy".into(),
                    "strict-origin-when-cross-origin".into()
                ),
                Change::Set(
                    "strict-transport-security".into(),
                    "max-age=31536000".into()
                ),
                Change::Set("cache-control".into(), "no-cache".into()),
                Change::Remove("server".into()),
                Change::Set(
                    "cache-control".into(),
                    "public, max-age=31536000, immutable".into()
                ),
            ]
        );
        assert_eq!(
            note.unwrap(),
            "response headers: added 3 security headers; set cache-control, removed server, set cache-control"
        );
        // No HSTS over plain http, and none of it with security off.
        let (ch, _) = c.on_response("/", false, &|_| false);
        assert!(
            !ch.iter()
                .any(|c| matches!(c, Change::Set(n, _) if n == "strict-transport-security"))
        );
        let off = Config::parse("security off").unwrap();
        assert_eq!(off.on_response("/", true, &|_| false), (vec![], None));
    }

    #[test]
    fn config_errors() {
        for (text, want) in [
            (
                "response set cache-control",
                "line 1: set cache-control needs a value",
            ),
            (
                "response delete server",
                "line 1: write response set NAME VALUE",
            ),
            (
                "request set host evil.example",
                "line 1: host belongs to BareProxy",
            ),
            ("response set x:y 1", "line 1: x:y isn't a header name"),
            ("redirect /a /b 303", "line 1: 303 isn't a redirect status"),
            ("redirect a /b", "line 1: a isn't a path or a prefix"),
            ("redirect /a/*/b /c", "isn't a path or a prefix"),
            (
                "redirect /a example.com",
                "line 1: example.com isn't a path or a full http:// or https:// URL",
            ),
            (
                "rewrite /a https://b.example/",
                "line 1: https://b.example/ isn't a path; a rewrite stays on this site",
            ),
            (
                "rewrite /a /b/*",
                "line 1: /b/* can end in * only when /a does",
            ),
            (
                "rewrite /a /a",
                "line 1: a rewrite to the same path does nothing",
            ),
            (
                "redirect /a /b when cookie is x",
                "line 1: write redirect FROM TO",
            ),
            (
                "path /x\nredirect /a /b",
                "line 2: redirect goes at the top",
            ),
            ("path /x\nsecurity off", "line 2: security goes at the top"),
            ("path x", "line 1: path takes one path prefix"),
            ("headers on", "line 1: unknown setting headers"),
        ] {
            let err = Config::parse(text).unwrap_err();
            assert!(err.contains(want), "{text}: {err}");
        }
    }
}
