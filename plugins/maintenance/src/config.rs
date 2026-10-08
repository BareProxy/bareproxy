// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! The maintenance plugin's config, and its two decisions, kept apart from
//! the Proxy-Wasm calls so they can be tested on their own.

use kit::{Item, Net};

/// The page that goes out when the config has none of its own.
pub const DEFAULT_PAGE: &str = "<!doctype html>
<html lang=\"en\">
<head>
<meta charset=\"utf-8\">
<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">
<title>Back soon</title>
<style>body{font:18px/1.5 system-ui,sans-serif;max-width:32em;margin:15vh auto;padding:0 1em;color:#222}</style>
</head>
<body>
<h1>Back soon</h1>
<p>This site is taking a short break. Please try again in a few minutes.</p>
</body>
</html>
";

#[derive(Debug, Clone, PartialEq)]
pub struct Config {
    /// Maintenance mode: every visitor not on the allow list gets the page.
    pub maintenance: bool,
    /// Addresses that pass in maintenance mode.
    pub allow: Vec<Net>,
    /// Backend statuses that bring the failover page.
    pub failover: Vec<u32>,
    /// Seconds for Retry-After; None leaves the header out.
    pub retry_after: Option<u64>,
    /// Path prefixes the plugin leaves alone.
    pub skip: Vec<String>,
    /// The failover page.
    pub page: String,
    /// The maintenance page; the failover page when not set.
    pub maintenance_page: Option<String>,
}

impl Default for Config {
    fn default() -> Config {
        Config {
            maintenance: false,
            allow: Vec::new(),
            failover: vec![502, 503, 504],
            retry_after: Some(300),
            skip: Vec::new(),
            page: DEFAULT_PAGE.to_string(),
            maintenance_page: None,
        }
    }
}

impl Config {
    pub fn parse(text: &str) -> Result<Config, String> {
        let mut c = Config::default();
        for item in kit::parse(text, &["page", "maintenance-page"])? {
            match item {
                Item::Block(line, body) => {
                    if !line.args().is_empty() {
                        return Err(line.err(format!(
                            "{} takes no words; the page starts on the next line",
                            line.name()
                        )));
                    }
                    if line.name() == "page" {
                        c.page = body;
                    } else {
                        c.maintenance_page = Some(body);
                    }
                }
                Item::Line(line) => match line.name() {
                    "maintenance" => c.maintenance = kit::switch(&line)?,
                    "allow" => {
                        if line.args().is_empty() {
                            return Err(line.err("allow takes one or more addresses or ranges"));
                        }
                        for w in line.args() {
                            match Net::parse(w) {
                                Some(n) => c.allow.push(n),
                                None => {
                                    return Err(line.err(format!(
                                        "{w} isn't an address or a range such as 192.0.2.0/24"
                                    )));
                                }
                            }
                        }
                    }
                    "failover" => {
                        c.failover.clear();
                        if line.args() == ["off"] {
                            continue;
                        }
                        if line.args().is_empty() {
                            return Err(
                                line.err("failover takes statuses, such as 502 503 504, or off")
                            );
                        }
                        for w in line.args() {
                            match w.parse::<u32>() {
                                Ok(n) if (500..=599).contains(&n) => c.failover.push(n),
                                _ => {
                                    return Err(line.err(format!(
                                        "{w} isn't a 5xx status; failover pages are for errors"
                                    )));
                                }
                            }
                        }
                    }
                    "retry-after" => {
                        c.retry_after = match line.args() {
                            [w] if w == "off" => None,
                            [w] => Some(kit::seconds(&line, w)?),
                            _ => {
                                return Err(
                                    line.err("retry-after takes a time, such as 10m, or off")
                                );
                            }
                        }
                    }
                    "skip" => {
                        if line.args().is_empty() {
                            return Err(
                                line.err("skip takes one or more path prefixes, such as /api/")
                            );
                        }
                        for w in line.args() {
                            if !w.starts_with('/') {
                                return Err(
                                    line.err(format!("{w} isn't a path; a path starts with /"))
                                );
                            }
                            c.skip.push(w.clone());
                        }
                    }
                    other => return Err(line.err(format!("unknown setting {other}"))),
                },
            }
        }
        Ok(c)
    }

    fn skipped(&self, path: &str) -> bool {
        self.skip.iter().any(|p| kit::under(path, p))
    }

    /// Before routing: the maintenance page, when maintenance is on and the
    /// visitor isn't on the allow list.
    pub fn on_request(&self, path: &str, addr: &str) -> Option<Answer> {
        if !self.maintenance || self.skipped(path) {
            return None;
        }
        if kit::any_contains(&self.allow, addr) {
            return Some(Answer::Pass(format!(
                "maintenance is on; {addr} is on the allow list"
            )));
        }
        let page = self.maintenance_page.as_ref().unwrap_or(&self.page);
        Some(self.page(
            503,
            page,
            "maintenance is on: the maintenance page went out".into(),
        ))
    }

    /// On the response: the failover page, when the backend (or BareProxy,
    /// with no backend up) answered with one of the failover statuses.
    pub fn on_response(&self, path: &str, status: u32) -> Option<Answer> {
        if !self.failover.contains(&status) || self.skipped(path) {
            return None;
        }
        Some(self.page(
            status,
            &self.page,
            format!("the response was {status}: the failover page went out instead"),
        ))
    }

    fn page(&self, status: u32, body: &str, note: String) -> Answer {
        let mut headers = vec![
            (
                "content-type".to_string(),
                "text/html; charset=utf-8".to_string(),
            ),
            ("cache-control".to_string(), "no-store".to_string()),
        ];
        if let Some(s) = self.retry_after {
            headers.push(("retry-after".to_string(), s.to_string()));
        }
        Answer::Page {
            status,
            headers,
            body: body.to_string(),
            note,
        }
    }
}

/// What the plugin does with a request or a response.
#[derive(Debug, Clone, PartialEq)]
pub enum Answer {
    /// Send this page instead.
    Page {
        status: u32,
        headers: Vec<(String, String)>,
        body: String,
        note: String,
    },
    /// Let it go on, with a note for the record.
    Pass(String),
}

#[cfg(test)]
mod tests {
    use super::*;

    fn status(a: Option<Answer>) -> Option<u32> {
        match a {
            Some(Answer::Page { status, .. }) => Some(status),
            _ => None,
        }
    }

    #[test]
    fn defaults_cover_backend_errors() {
        let c = Config::parse("").unwrap();
        assert!(!c.maintenance);
        assert_eq!(status(c.on_request("/", "192.0.2.1")), None);
        for s in [502, 503, 504] {
            assert_eq!(status(c.on_response("/", s)), Some(s), "status {s}");
        }
        for s in [200, 404, 500] {
            assert_eq!(status(c.on_response("/", s)), None, "status {s}");
        }
        match c.on_response("/x", 502) {
            Some(Answer::Page { headers, body, .. }) => {
                assert!(headers.contains(&("retry-after".into(), "300".into())));
                assert!(headers.contains(&("cache-control".into(), "no-store".into())));
                assert_eq!(body, DEFAULT_PAGE);
            }
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn maintenance_with_an_allow_list() {
        let text = "maintenance on\nallow 203.0.113.0/24 2001:db8::/32\nretry-after 1h\n\
                    skip /healthz\nmaintenance-page\n<h1>Maintenance until 14:00</h1>\nend\n";
        let c = Config::parse(text).unwrap();
        match c.on_request("/shop", "192.0.2.1") {
            Some(Answer::Page {
                status,
                headers,
                body,
                ..
            }) => {
                assert_eq!(status, 503);
                assert_eq!(body, "<h1>Maintenance until 14:00</h1>\n");
                assert!(headers.contains(&("retry-after".into(), "3600".into())));
            }
            other => panic!("{other:?}"),
        }
        assert!(matches!(
            c.on_request("/shop", "203.0.113.9"),
            Some(Answer::Pass(_))
        ));
        assert!(matches!(
            c.on_request("/shop", "2001:db8::5"),
            Some(Answer::Pass(_))
        ));
        assert_eq!(c.on_request("/healthz", "192.0.2.1"), None);
        // The failover page is still the default one.
        match c.on_response("/", 504) {
            Some(Answer::Page { body, .. }) => assert_eq!(body, DEFAULT_PAGE),
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn own_failover_statuses_and_page() {
        let c =
            Config::parse("failover 503\nretry-after off\nskip /api/\npage\n<p>down</p>\nend\n")
                .unwrap();
        assert_eq!(status(c.on_response("/", 502)), None);
        assert_eq!(status(c.on_response("/api/x", 503)), None);
        match c.on_response("/", 503) {
            Some(Answer::Page { headers, body, .. }) => {
                assert_eq!(body, "<p>down</p>\n");
                assert!(!headers.iter().any(|(k, _)| k == "retry-after"));
            }
            other => panic!("{other:?}"),
        }
        let off = Config::parse("failover off").unwrap();
        assert_eq!(status(off.on_response("/", 502)), None);
    }

    #[test]
    fn config_errors_name_the_line() {
        for (text, want) in [
            ("maintenance yes", "line 1: maintenance takes on or off"),
            (
                "\nallow example.com",
                "line 2: example.com isn't an address or a range such as 192.0.2.0/24",
            ),
            (
                "failover 404",
                "line 1: 404 isn't a 5xx status; failover pages are for errors",
            ),
            (
                "retry-after soon",
                "line 1: soon isn't a time; write 600, 30s, 10m, 2h or 1d",
            ),
            ("skip api", "line 1: api isn't a path; a path starts with /"),
            (
                "page /down.html\nend",
                "line 1: page takes no words; the page starts on the next line",
            ),
            ("colour red", "line 1: unknown setting colour"),
            ("page\n<p>", "line 1: page has no line saying end"),
        ] {
            assert_eq!(Config::parse(text).unwrap_err(), want, "{text}");
        }
    }
}
