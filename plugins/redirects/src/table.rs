// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! The plugin's config, the redirect file read into a table, the lookup and
//! the hit report, kept apart from the Proxy-Wasm calls so they can be
//! tested on their own.

use hashbrown::HashTable;
use kit::{Item, Net};
use std::collections::HashMap;

/// The plugin's own config.
#[derive(Debug, Clone, PartialEq)]
pub struct Config {
    /// The redirect file, a path inside a folder the plugin line names with read.
    pub file: String,
    /// The status for lines that don't name one.
    pub status: u32,
    /// Seconds between checks of the file for changes; 0 for never.
    pub reload: u64,
    /// Whether the query goes along by default.
    pub keep_query: bool,
    /// The path of the hit report, if any.
    pub report: Option<String>,
    /// Who may read the report.
    pub report_allow: Vec<Net>,
}

impl Config {
    pub fn parse(text: &str) -> Result<Config, String> {
        let mut c = Config {
            file: String::new(),
            status: 301,
            reload: 30,
            keep_query: true,
            report: None,
            report_allow: vec![Net::parse("127.0.0.1").unwrap(), Net::parse("::1").unwrap()],
        };
        let mut allow_set = false;
        for item in kit::parse(text, &[])? {
            let Item::Line(line) = item else {
                unreachable!()
            };
            let a = line.args();
            match line.name() {
                "file" => {
                    match a {
                        [f] => c.file = f.trim_start_matches('/').to_string(),
                        _ => return Err(line.err(
                            "file takes one path, inside a folder the plugin line names with read",
                        )),
                    }
                }
                "status" => match a {
                    [w] => c.status = status(w).ok_or_else(|| line.err(bad_status(w)))?,
                    _ => return Err(line.err("status takes 301, 302, 307 or 308")),
                },
                "reload" => match a {
                    [w] if w == "off" => c.reload = 0,
                    [w] => c.reload = kit::seconds(&line, w)?.max(1),
                    _ => return Err(line.err("reload takes a time, such as 30s, or off")),
                },
                "query" => match a {
                    [w] if w == "keep" => c.keep_query = true,
                    [w] if w == "drop" => c.keep_query = false,
                    _ => return Err(line.err("query takes keep or drop")),
                },
                "report" => match a {
                    [p] if p.starts_with('/') => c.report = Some(p.clone()),
                    _ => return Err(line.err("report takes a path, such as /.redirects")),
                },
                "report-allow" => {
                    if a.is_empty() {
                        return Err(line.err("report-allow takes one or more addresses or ranges"));
                    }
                    if !allow_set {
                        c.report_allow.clear();
                        allow_set = true;
                    }
                    for w in a {
                        c.report_allow.push(Net::parse(w).ok_or_else(|| {
                            line.err(format!(
                                "{w} isn't an address or a range such as 192.0.2.0/24"
                            ))
                        })?);
                    }
                }
                other => return Err(line.err(format!("unknown setting {other}"))),
            }
        }
        if c.file.is_empty() {
            return Err("no file: add a line such as file redirects.txt".into());
        }
        Ok(c)
    }
}

fn status(w: &str) -> Option<u32> {
    match w.parse::<u32>() {
        Ok(n @ (301 | 302 | 307 | 308)) => Some(n),
        _ => None,
    }
}

fn bad_status(w: &str) -> String {
    format!("{w} isn't a redirect status; use 301, 302, 307 or 308")
}

/// A line of the redirect file. Its paths are places in the file's text,
/// so loading a hundred thousand lines makes no string of each.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Entry {
    pub line: usize,
    from: (u32, u32),
    to: (u32, u32),
    pub status: u32,
    pub keep_query: bool,
}

/// The redirect file, read.
#[derive(Default)]
pub struct Table {
    text: String,
    pub entries: Vec<Entry>,
    /// Indexes into entries, by the exact path.
    exact: HashTable<u32>,
    /// By prefix without the *: /old/* is under /old/.
    prefix: HashTable<u32>,
}

impl std::fmt::Debug for Table {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "Table({} entries)", self.entries.len())
    }
}

/// Where a request goes.
#[derive(Debug, Clone, PartialEq)]
pub struct Hit {
    pub entry: Entry,
    pub location: String,
}

/// FxHash, as in rustc: the keys are paths from the site owner's own file,
/// so a hash built to resist chosen keys buys nothing here.
fn fx(s: &str) -> u64 {
    const K: u64 = 0x51_7c_c1_b7_27_22_0a_95;
    let mut h = 0u64;
    let (chunks, rest) = s.as_bytes().as_chunks::<8>();
    for c in chunks {
        h = (h.rotate_left(5) ^ u64::from_le_bytes(*c)).wrapping_mul(K);
    }
    for &b in rest {
        h = (h.rotate_left(5) ^ b as u64).wrapping_mul(K);
    }
    h
}

/// The key an old path is looked up by: the path, or a prefix without *.
fn key(from: &str) -> &str {
    from.strip_suffix('*').unwrap_or(from)
}

fn slice(text: &str, s: (u32, u32)) -> &str {
    &text[s.0 as usize..(s.0 + s.1) as usize]
}

impl Table {
    pub fn from(&self, e: &Entry) -> &str {
        slice(&self.text, e.from)
    }

    pub fn to(&self, e: &Entry) -> &str {
        slice(&self.text, e.to)
    }

    fn find(&self, prefix: bool, k: &str) -> Option<&Entry> {
        let tab = if prefix { &self.prefix } else { &self.exact };
        tab.find(fx(k), |&i| key(self.from(&self.entries[i as usize])) == k)
            .map(|&i| &self.entries[i as usize])
    }

    /// Reads a redirect file. Defaults come from the plugin's config.
    pub fn parse(text: String, cfg: &Config) -> Result<Table, String> {
        let base = text.as_ptr() as usize;
        let span = |w: &str| ((w.as_ptr() as usize - base) as u32, w.len() as u32);
        let mut entries = Vec::new();
        for (i, raw) in text.lines().enumerate() {
            let no = i + 1;
            // Words up to a comment, without an allocation each.
            let mut w: [&str; 6] = [""; 6];
            let mut n = 0;
            for x in raw.split_ascii_whitespace() {
                if x.starts_with('#') {
                    break;
                }
                if n == w.len() {
                    return Err(format!("line {no}: too many words"));
                }
                w[n] = x;
                n += 1;
            }
            if n == 0 {
                continue;
            }
            let err = |m: String| format!("line {no}: {m}");
            if n < 2 {
                return Err(err(format!(
                    "{} has nowhere to go; write OLD-PATH NEW-PATH-OR-URL [301|302|307|308] [drop-query]",
                    w[0]
                )));
            }
            let (from, to) = (w[0], w[1]);
            let (mut status, mut keep_query) = (cfg.status, cfg.keep_query);
            for x in &w[2..n] {
                match *x {
                    "drop-query" => keep_query = false,
                    "keep-query" => keep_query = true,
                    _ => {
                        status = self::status(x).ok_or_else(|| {
                            err(format!("{x} isn't a redirect status or drop-query"))
                        })?
                    }
                }
            }
            if !from.starts_with('/') || from.contains(['?', '#']) {
                return Err(err(format!(
                    "{from} isn't a path; an old path starts with / and has no query"
                )));
            }
            let pre = from.strip_suffix('*');
            let bad_prefix = match pre {
                Some(p) => !p.ends_with('/') || p.contains('*'),
                None => from.contains('*'),
            };
            if bad_prefix {
                return Err(err(format!(
                    "{from}: a prefix ends in /*, and * goes nowhere else"
                )));
            }
            if !(to.starts_with('/') || to.starts_with("https://") || to.starts_with("http://")) {
                return Err(err(format!(
                    "{to} isn't a path or a full http:// or https:// URL"
                )));
            }
            if to[..to.len() - 1].contains('*') || to.ends_with('*') && pre.is_none() {
                return Err(err(format!("{to} can end in * only when {from} does")));
            }
            if pre.is_none() && to == from {
                return Err(err(format!("{from} redirects to itself")));
            }
            entries.push(Entry {
                line: no,
                from: span(from),
                to: span(to),
                status,
                keep_query,
            });
        }
        let mut exact: HashTable<u32> = HashTable::with_capacity(entries.len());
        let mut prefix: HashTable<u32> = HashTable::new();
        for (i, e) in entries.iter().enumerate() {
            let from = slice(&text, e.from);
            let k = key(from);
            let tab = if from.ends_with('*') {
                &mut prefix
            } else {
                &mut exact
            };
            let same = |&j: &u32| key(slice(&text, entries[j as usize].from)) == k;
            if let Some(&j) = tab.find(fx(k), same) {
                return Err(format!(
                    "line {}: {from} is on line {} already",
                    e.line, entries[j as usize].line
                ));
            }
            tab.insert_unique(fx(k), i as u32, |&j| {
                fx(key(slice(&text, entries[j as usize].from)))
            });
        }
        Ok(Table {
            text,
            entries,
            exact,
            prefix,
        })
    }

    /// Looks a path up: exactly, then with or without its trailing slash,
    /// then under the longest prefix. A few hash lookups, however long the
    /// file.
    pub fn lookup(&self, path: &str, query: &str) -> Option<Hit> {
        let alt = match path.strip_suffix('/') {
            Some(p) => p.to_string(),
            None => format!("{path}/"),
        };
        let found = self.find(false, path).or_else(|| {
            if alt.is_empty() {
                None
            } else {
                self.find(false, &alt)
            }
        });
        let (e, to) = match found {
            Some(e) => (*e, self.to(e).to_string()),
            None => {
                let mut end = path.len();
                loop {
                    let cut = path[..end].rfind('/')?;
                    if let Some(e) = self.find(true, &path[..=cut]) {
                        let rest = &path[cut + 1..];
                        let to = self.to(e);
                        break (
                            *e,
                            match to.strip_suffix('*') {
                                Some(t) => format!("{t}{rest}"),
                                None => to.to_string(),
                            },
                        );
                    }
                    if cut == 0 {
                        return None;
                    }
                    end = cut;
                }
            }
        };
        let location = if e.keep_query && !query.is_empty() {
            let sep = if to.contains('?') { '&' } else { '?' };
            format!("{to}{sep}{query}")
        } else {
            to
        };
        Some(Hit { entry: e, location })
    }
}

/// Hit counts as text, one "count<TAB>from" a line, largest first, cut to
/// max bytes.
pub fn encode_hits(hits: &HashMap<String, u64>, max: usize) -> String {
    let mut v: Vec<(&String, &u64)> = hits.iter().filter(|(_, n)| **n > 0).collect();
    v.sort_by(|a, b| b.1.cmp(a.1).then(a.0.cmp(b.0)));
    let mut out = String::new();
    for (from, n) in v {
        let line = format!("{n}\t{from}\n");
        if out.len() + line.len() > max {
            break;
        }
        out.push_str(&line);
    }
    out
}

/// Adds counts in the text form to a map.
pub fn add_hits(into: &mut HashMap<String, u64>, text: &str) {
    for line in text.lines() {
        if let Some((n, from)) = line.split_once('\t')
            && let Ok(n) = n.parse::<u64>()
        {
            *into.entry(from.to_string()).or_default() += n;
        }
    }
}

/// The hit report: lines that were used, most hits first, then how many
/// were never used, or with `unused`, the list of them.
pub fn report(
    t: &Table,
    file: &str,
    loaded: &str,
    hits: &HashMap<String, u64>,
    unused: bool,
    every: u64,
) -> String {
    let mut out = format!(
        "BareProxy redirects from {file}: {} lines, loaded {loaded}.\nHits since the plugin started{}.\n\n",
        t.entries.len(),
        if every > 0 {
            format!(", brought together every {every}s")
        } else {
            String::new()
        }
    );
    let mut used: Vec<(&Entry, u64)> = t
        .entries
        .iter()
        .filter_map(|e| hits.get(t.from(e)).filter(|n| **n > 0).map(|n| (e, *n)))
        .collect();
    used.sort_by(|a, b| b.1.cmp(&a.1).then(a.0.line.cmp(&b.0.line)));
    let never = t.entries.len() - used.len();
    if unused {
        out.push_str(&format!("Never used: {never} lines.\n\nline\tfrom\tto\n"));
        for e in t
            .entries
            .iter()
            .filter(|e| hits.get(t.from(e)).is_none_or(|n| *n == 0))
        {
            out.push_str(&format!("{}\t{}\t{}\n", e.line, t.from(e), t.to(e)));
        }
        return out;
    }
    out.push_str("hits\tline\tfrom\tto\n");
    for (e, n) in &used {
        out.push_str(&format!("{n}\t{}\t{}\t{}\n", e.line, t.from(e), t.to(e)));
    }
    out.push_str(&format!(
        "\nNever used: {never} lines. Add ?unused to the address to list them.\n"
    ));
    out
}

/// A time in seconds since 1970 as "2026-10-09 14:05 UTC".
pub fn utc(secs: u64) -> String {
    let days = (secs / 86400) as i64;
    let (h, m) = ((secs % 86400) / 3600, (secs % 3600) / 60);
    // Howard Hinnant's days-to-civil.
    let z = days + 719468;
    let era = z.div_euclid(146097);
    let doe = z - era * 146097;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let mo = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = yoe + era * 400 + (mo <= 2) as i64;
    format!("{y}-{mo:02}-{d:02} {h:02}:{m:02} UTC")
}

#[cfg(test)]
mod tests {
    use super::*;

    fn cfg() -> Config {
        Config::parse("file redirects.txt").unwrap()
    }

    const FILE: &str = "# old            new
/p/123             /posts/hello
/about-us          /about/        308
/search            /find          drop-query
/shop/old-cat/*    /shop/new-cat/*
/docs/*            https://docs.example.com/
/blog/*            https://blog.example.com/*   302
/blog/2019/*       /archive/2019/*
/with-query        /new?from=old
";

    #[test]
    fn lookups() {
        let t = Table::parse(FILE.to_string(), &cfg()).unwrap();
        assert_eq!(t.entries.len(), 8);
        let at = |p: &str, q: &str| {
            t.lookup(p, q)
                .map(|h| (h.entry.status, h.location, h.entry.line))
        };
        assert_eq!(at("/p/123", ""), Some((301, "/posts/hello".into(), 2)));
        assert_eq!(
            at("/p/123/", "a=1"),
            Some((301, "/posts/hello?a=1".into(), 2))
        );
        assert_eq!(at("/about-us", ""), Some((308, "/about/".into(), 3)));
        assert_eq!(at("/search", "q=x"), Some((301, "/find".into(), 4)));
        assert_eq!(
            at("/shop/old-cat/shoes/red", ""),
            Some((301, "/shop/new-cat/shoes/red".into(), 5))
        );
        assert_eq!(
            at("/shop/old-cat/", ""),
            Some((301, "/shop/new-cat/".into(), 5))
        );
        assert_eq!(
            at("/docs/a/b", ""),
            Some((301, "https://docs.example.com/".into(), 6))
        );
        // The longest prefix wins.
        assert_eq!(
            at("/blog/2019/post", ""),
            Some((301, "/archive/2019/post".into(), 8))
        );
        assert_eq!(
            at("/blog/2020/post", ""),
            Some((302, "https://blog.example.com/2020/post".into(), 7))
        );
        assert_eq!(
            at("/with-query", "a=1"),
            Some((301, "/new?from=old&a=1".into(), 9))
        );
        assert_eq!(at("/p/1234", ""), None);
        assert_eq!(at("/shop/old-catalog", ""), None);
        assert_eq!(at("/", ""), None);
    }

    #[test]
    fn config_defaults_apply() {
        let c =
            Config::parse("file /r.txt\nstatus 302\nquery drop\nreload off\nreport /.redirects")
                .unwrap();
        assert_eq!(c.file, "r.txt");
        assert_eq!((c.reload, c.report.as_deref()), (0, Some("/.redirects")));
        let t = Table::parse("/a /b\n/c /d keep-query 301".to_string(), &c).unwrap();
        assert_eq!(
            t.lookup("/a", "x=1").map(|h| (h.entry.status, h.location)),
            Some((302, "/b".into()))
        );
        assert_eq!(
            t.lookup("/c", "x=1").map(|h| (h.entry.status, h.location)),
            Some((301, "/d?x=1".into()))
        );
    }

    #[test]
    fn file_errors_name_the_line() {
        for (text, want) in [
            ("/a /b\n/c", "line 2: /c has nowhere to go"),
            (
                "/a /b 303",
                "line 1: 303 isn't a redirect status or drop-query",
            ),
            ("a /b", "line 1: a isn't a path"),
            ("/a?x=1 /b", "line 1: /a?x=1 isn't a path"),
            ("/old* /new", "line 1: /old*: a prefix ends in /*"),
            ("/a/*/b /c", "a prefix ends in /*"),
            (
                "/a example.com",
                "line 1: example.com isn't a path or a full",
            ),
            ("/a /b/*", "line 1: /b/* can end in * only when /a does"),
            ("/a /a", "line 1: /a redirects to itself"),
            ("/a /b\n\n/a /c", "line 3: /a is on line 1 already"),
            ("/x/* /y/\n/x/* /z/", "line 2: /x/* is on line 1 already"),
        ] {
            let err = Table::parse(text.to_string(), &cfg()).unwrap_err();
            assert!(err.contains(want), "{text}: {err}");
        }
        for (text, want) in [
            ("status 301", "no file"),
            ("file a b", "line 1: file takes one path"),
            ("file a\nstatus 200", "line 2: 200 isn't a redirect status"),
            ("file a\nreload soon", "line 2: soon isn't a time"),
            (
                "file a\nreport-allow everyone",
                "line 2: everyone isn't an address",
            ),
            ("file a\nquery maybe", "line 2: query takes keep or drop"),
        ] {
            let err = Config::parse(text).unwrap_err();
            assert!(err.contains(want), "{text}: {err}");
        }
    }

    #[test]
    fn hits_and_report() {
        let t = Table::parse(FILE.to_string(), &cfg()).unwrap();
        let mut a = HashMap::new();
        a.insert("/p/123".to_string(), 5u64);
        a.insert("/blog/*".to_string(), 2u64);
        let mut all = HashMap::new();
        add_hits(&mut all, &encode_hits(&a, 1 << 20));
        add_hits(&mut all, "3\t/p/123\n");
        assert_eq!(all.get("/p/123"), Some(&8));
        assert_eq!(encode_hits(&a, 10), "5\t/p/123\n");
        let r = report(&t, "redirects.txt", "2026-10-09 14:05 UTC", &all, false, 30);
        assert!(r.contains("8 lines, loaded 2026-10-09 14:05 UTC"));
        assert!(r.contains("hits\tline\tfrom\tto\n8\t2\t/p/123\t/posts/hello\n2\t7\t/blog/*\thttps://blog.example.com/*\n"), "{r}");
        assert!(r.contains("Never used: 6 lines."));
        let u = report(&t, "redirects.txt", "x", &all, true, 30);
        assert!(
            u.contains("3\t/about-us\t/about/\n") && !u.contains("/p/123"),
            "{u}"
        );
    }

    #[test]
    fn dates() {
        assert_eq!(utc(0), "1970-01-01 00:00 UTC");
        assert_eq!(utc(1_791_547_500), "2026-10-09 12:05 UTC");
        assert_eq!(utc(951_782_400), "2000-02-29 00:00 UTC");
    }
}

#[cfg(test)]
mod speed {
    #[test]
    #[ignore]
    fn parse_100k() {
        let mut s = String::new();
        for i in 0..100_000 {
            s.push_str(&format!("/old/page-{i} /new/page-{i}\n"));
        }
        let cfg = super::Config::parse("file x").unwrap();
        let t0 = std::time::Instant::now();
        let t = super::Table::parse(s, &cfg).unwrap();
        eprintln!("parsed {} in {:?}", t.entries.len(), t0.elapsed());
    }
}
