// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! What BareProxy's plugins share.
//!
//! A plugin's config is a plain text file in the same style as BareProxy's
//! own: one setting a line, words split by spaces, `#` starts a comment.
//! A block of text (a page, say) starts with a line naming it and runs to a
//! line that says `end`, taken as it is, comments and all.

use std::net::IpAddr;

/// A config line: its number (from 1) and its words, comments dropped.
#[derive(Debug, Clone, PartialEq)]
pub struct Line {
    pub no: usize,
    pub words: Vec<String>,
}

impl Line {
    /// The setting's name, the first word.
    pub fn name(&self) -> &str {
        &self.words[0]
    }

    /// The words after the name.
    pub fn args(&self) -> &[String] {
        &self.words[1..]
    }

    /// An error that names this line.
    pub fn err(&self, msg: impl std::fmt::Display) -> String {
        format!("line {}: {}", self.no, msg)
    }
}

/// An item of a config: a line of settings, or a block of text.
#[derive(Debug, Clone, PartialEq)]
pub enum Item {
    Line(Line),
    /// A block: the line that opened it, and its text up to `end`.
    Block(Line, String),
}

/// Parses a config. Lines whose first word is in `blocks` open a block of
/// text that runs to a line saying `end`.
pub fn parse(text: &str, blocks: &[&str]) -> Result<Vec<Item>, String> {
    let mut items = Vec::new();
    let mut lines = text.lines().enumerate();
    while let Some((i, raw)) = lines.next() {
        let words = words(raw);
        if words.is_empty() {
            continue;
        }
        let line = Line { no: i + 1, words };
        if !blocks.contains(&line.name()) {
            items.push(Item::Line(line));
            continue;
        }
        let mut body = String::new();
        let mut closed = false;
        for (_, raw) in lines.by_ref() {
            if raw.trim() == "end" {
                closed = true;
                break;
            }
            body.push_str(raw);
            body.push('\n');
        }
        if !closed {
            return Err(line.err(format!("{} has no line saying end", line.name())));
        }
        items.push(Item::Block(line, body));
    }
    Ok(items)
}

/// The words of a line, up to a `#` that starts the line or follows a space.
pub fn words(raw: &str) -> Vec<String> {
    let mut out = Vec::new();
    for w in raw.split_whitespace() {
        if w.starts_with('#') {
            break;
        }
        out.push(w.to_string());
    }
    out
}

/// "on" or "off".
pub fn switch(line: &Line) -> Result<bool, String> {
    match line.args() {
        [w] if w == "on" => Ok(true),
        [w] if w == "off" => Ok(false),
        _ => Err(line.err(format!("{} takes on or off", line.name()))),
    }
}

/// A number of seconds, such as 600, 30s, 10m, 2h or 1d.
pub fn seconds(line: &Line, w: &str) -> Result<u64, String> {
    let (num, unit) = match w.find(|c: char| !c.is_ascii_digit()) {
        Some(i) => w.split_at(i),
        None => (w, "s"),
    };
    let mul = match unit {
        "s" => 1,
        "m" => 60,
        "h" => 3600,
        "d" => 86400,
        _ => 0,
    };
    match num.parse::<u64>() {
        Ok(n) if mul > 0 && !num.is_empty() => Ok(n * mul),
        _ => Err(line.err(format!("{w} isn't a time; write 600, 30s, 10m, 2h or 1d"))),
    }
}

/// An address or a range of addresses: 192.0.2.7, 192.0.2.0/24, 2001:db8::/32.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Net {
    addr: IpAddr,
    bits: u8,
}

impl Net {
    pub fn parse(s: &str) -> Option<Net> {
        let (a, bits) = match s.split_once('/') {
            Some((a, b)) => (a, Some(b.parse::<u8>().ok()?)),
            None => (s, None),
        };
        let addr = canon(a.parse::<IpAddr>().ok()?);
        let max = if addr.is_ipv4() { 32 } else { 128 };
        let bits = bits.unwrap_or(max);
        if bits > max {
            return None;
        }
        Some(Net { addr, bits })
    }

    /// The range's first and last address as numbers, and whether it's
    /// IPv4.
    pub fn bounds(&self) -> (bool, u128, u128) {
        let (v4, n, width) = match self.addr {
            IpAddr::V4(a) => (true, u32::from(a) as u128, 32),
            IpAddr::V6(a) => (false, u128::from(a), 128),
        };
        let host = width - self.bits as u32;
        let mask = if host == 128 {
            u128::MAX
        } else {
            (1u128 << host) - 1
        };
        (v4, n & !mask, (n & !mask) | mask)
    }

    /// Whether the range holds the address.
    pub fn contains(&self, ip: IpAddr) -> bool {
        let ip = canon(ip);
        let keep = |n: u128, width: u32| {
            if self.bits == 0 {
                0
            } else {
                n >> (width - self.bits as u32)
            }
        };
        match (self.addr, ip) {
            (IpAddr::V4(a), IpAddr::V4(b)) => {
                keep(u32::from(a) as u128, 32) == keep(u32::from(b) as u128, 32)
            }
            (IpAddr::V6(a), IpAddr::V6(b)) => keep(u128::from(a), 128) == keep(u128::from(b), 128),
            _ => false,
        }
    }
}

/// A range as text, 192.0.2.0/24; a single address has its full length,
/// 192.0.2.7/32.
impl std::fmt::Display for Net {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}/{}", self.addr, self.bits)
    }
}

/// An IPv4 address written as IPv6 (::ffff:192.0.2.7) is taken as IPv4.
fn canon(ip: IpAddr) -> IpAddr {
    match ip {
        IpAddr::V6(v6) => v6
            .to_ipv4_mapped()
            .map(IpAddr::V4)
            .unwrap_or(IpAddr::V6(v6)),
        v4 => v4,
    }
}

/// Ranges sorted and merged, so a lookup takes a binary search however
/// long the list. Amazon's lists of its crawlers' addresses name over a thousand single
/// addresses.
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

/// The address ranges in a list: entries named ipv4Prefix and ipv6Prefix,
/// as the AI crawler operators and Google Cloud publish them in JSON (also
/// when the JSON sits in an HTML page), or ip_prefix and ipv6_prefix, as AWS
/// does, or else plain text with one address or range a line, where #
/// starts a comment. A list with no ranges is an error: adopting one
/// would make every claim a fake. So is JSON or HTML with no entries.
pub fn parse_list(text: &str) -> Result<Vec<Net>, String> {
    const KEYS: [&str; 4] = ["ipv4Prefix", "ipv6Prefix", "ip_prefix", "ipv6_prefix"];
    let mut out = vec![];
    let json_or_html = text.trim_start().starts_with(['{', '[', '<']);
    if KEYS.iter().any(|k| text.contains(k)) || json_or_html {
        for key in KEYS {
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
            match words(raw).as_slice() {
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

/// Whether any range in the list holds the address (written as text, as the
/// property source.address gives it).
pub fn any_contains(nets: &[Net], addr: &str) -> bool {
    match addr.parse::<IpAddr>() {
        Ok(ip) => nets.iter().any(|n| n.contains(ip)),
        Err(_) => false,
    }
}

/// Whether a path falls under a prefix: /api matches /api and /api/x but not
/// /apix; a prefix ending in / or * matches what starts with it.
pub fn under(path: &str, prefix: &str) -> bool {
    let p = prefix.trim_end_matches('*');
    if p.is_empty() || p.ends_with('/') || p.len() < prefix.len() {
        return path.starts_with(p);
    }
    path == p || path.starts_with(p) && path[p.len()..].starts_with('/')
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

/// Writes a line into the request's record, shown by `why` and `explain`.
/// Outside a request, the line goes to BareProxy's log.
#[cfg(target_arch = "wasm32")]
pub fn note(text: &str) {
    let _ = proxy_wasm::hostcalls::call_foreign_function("bareproxy_note", Some(text.as_bytes()));
}

/// Writes a line into the request's record (a no-op off WebAssembly, for
/// tests).
#[cfg(not(target_arch = "wasm32"))]
pub fn note(_: &str) {}

/// Adds n to a counter BareProxy keeps for the key, shared by all of the
/// plugin's instances, in windows of `window_ms` aligned to the clock
/// (bareproxy_count). Returns the current window's count, the last
/// window's, and the milliseconds into the current window; None when
/// BareProxy refuses (too many counters).
pub fn count(key: &str, window_ms: u32, n: u32) -> Option<(u64, u64, u64)> {
    let mut args = window_ms.to_le_bytes().to_vec();
    args.extend_from_slice(&n.to_le_bytes());
    args.extend_from_slice(key.as_bytes());
    let b = proxy_wasm::hostcalls::call_foreign_function("bareproxy_count", Some(&args)).ok()??;
    let at = |i: usize| {
        b.get(i..i + 8)
            .map(|x| u64::from_le_bytes(x.try_into().unwrap()))
    };
    Some((at(0)?, at(8)?, at(16)?))
}

/// A sliding count from a window's counts: the last window's, weighed by
/// how much of it still falls in the last `window_ms`, plus the current.
pub fn sliding(cur: u64, prev: u64, into_ms: u64, window_ms: u32) -> u64 {
    let w = window_ms.max(1) as u64;
    let left = w.saturating_sub(into_ms.min(w));
    cur + (prev * left).div_ceil(w)
}

/// The client's address: the connection's, or when it comes from one of
/// the `trusted` proxies, the last address in X-Forwarded-For that isn't
/// one of them. From anyone else, X-Forwarded-For is ignored.
pub fn client_addr(source: &str, forwarded: Option<&str>, trusted: &[Net]) -> Option<IpAddr> {
    let src: IpAddr = source.parse().ok()?;
    if trusted.is_empty() || !trusted.iter().any(|n| n.contains(src)) {
        return Some(src);
    }
    let Some(f) = forwarded else { return Some(src) };
    let mut last = src;
    for part in f.rsplit(',') {
        let Ok(ip) = part.trim().parse::<IpAddr>() else {
            return Some(last);
        };
        if !trusted.iter().any(|n| n.contains(ip)) {
            return Some(ip);
        }
        last = ip;
    }
    Some(last)
}

/// Reads a file inside a folder the plugin line names with read, through
/// bareproxy_read_file.
pub fn read_file(file: &str) -> Result<Vec<u8>, String> {
    use proxy_wasm::types::Status;
    match proxy_wasm::hostcalls::call_foreign_function("bareproxy_read_file", Some(file.as_bytes()))
    {
        Ok(b) => Ok(b.unwrap_or_default()),
        Err(Status::NotFound) => Err(format!(
            "can't read {file}: it isn't in a folder the plugin line names with read"
        )),
        Err(Status::BadArgument) => Err(format!(
            "can't read {file}: it's larger than the plugin's body-limit; raise body-limit on the plugin line"
        )),
        Err(st) => Err(format!("can't read {file}: {st:?}")),
    }
}

/// A value from the plugin's store (bareproxy_store_get), or None when the
/// key isn't there or the plugin line has no store.
pub fn store_get(key: &str) -> Option<Vec<u8>> {
    proxy_wasm::hostcalls::call_foreign_function("bareproxy_store_get", Some(key.as_bytes()))
        .ok()
        .flatten()
}

/// Puts a value in the plugin's store (bareproxy_store_put); false when it
/// has no store or the store is full.
pub fn store_put(key: &str, val: &[u8]) -> bool {
    let mut args = (key.len() as u32).to_le_bytes().to_vec();
    args.extend_from_slice(key.as_bytes());
    args.extend_from_slice(val);
    proxy_wasm::hostcalls::call_foreign_function("bareproxy_store_put", Some(&args)).is_ok()
}

/// The value of a property as text, or "" when there is none.
pub fn property(ctx: &dyn proxy_wasm::traits::Context, path: &[&str]) -> String {
    ctx.get_property(path.to_vec())
        .map(|b| String::from_utf8_lossy(&b).into_owned())
        .unwrap_or_default()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_lines_and_blocks() {
        let text = "# a comment\nmaintenance on  # with a note\n\npage\n<h1># not a comment</h1>\n  end  \nallow 192.0.2.1\n";
        let items = parse(text, &["page"]).unwrap();
        assert_eq!(items.len(), 3);
        assert_eq!(
            items[0],
            Item::Line(Line {
                no: 2,
                words: vec!["maintenance".into(), "on".into()]
            })
        );
        match &items[1] {
            Item::Block(l, body) => {
                assert_eq!(l.no, 4);
                assert_eq!(body, "<h1># not a comment</h1>\n");
            }
            _ => panic!("no block"),
        }
        assert!(matches!(&items[2], Item::Line(l) if l.no == 7 && l.args() == ["192.0.2.1"]));
        let err = parse("page\n<p>\n", &["page"]).unwrap_err();
        assert_eq!(err, "line 1: page has no line saying end");
    }

    #[test]
    fn ranges() {
        let n = Net::parse("192.0.2.0/24").unwrap();
        assert!(n.contains("192.0.2.200".parse().unwrap()));
        assert!(!n.contains("192.0.3.1".parse().unwrap()));
        assert!(n.contains("::ffff:192.0.2.9".parse().unwrap()));
        assert!(!n.contains("2001:db8::1".parse().unwrap()));
        let one = Net::parse("198.51.100.7").unwrap();
        assert!(one.contains("198.51.100.7".parse().unwrap()));
        assert!(!one.contains("198.51.100.8".parse().unwrap()));
        let v6 = Net::parse("2001:db8::/32").unwrap();
        assert!(v6.contains("2001:db8:ffff::1".parse().unwrap()));
        assert!(!v6.contains("2001:db9::1".parse().unwrap()));
        assert!(
            Net::parse("0.0.0.0/0")
                .unwrap()
                .contains("8.8.8.8".parse().unwrap())
        );
        assert!(Net::parse("192.0.2.0/33").is_none());
        assert!(Net::parse("example.com").is_none());
        assert!(any_contains(&[n, one], "198.51.100.7"));
        assert_eq!(n.bounds(), (true, 0xC000_0200, 0xC000_02FF));
        assert_eq!(one.bounds(), (true, 0xC633_6407, 0xC633_6407));
        assert_eq!(Net::parse("::/0").unwrap().bounds(), (false, 0, u128::MAX));
        assert_eq!(n.to_string(), "192.0.2.0/24");
        assert_eq!(
            Net::parse("::ffff:192.0.2.7").unwrap().to_string(),
            "192.0.2.7/32"
        );
        assert!(!any_contains(&[n], "not an address"));
    }

    #[test]
    fn prefixes() {
        assert!(under("/api", "/api"));
        assert!(under("/api/x", "/api"));
        assert!(!under("/apix", "/api"));
        assert!(under("/api/x", "/api/"));
        assert!(under("/apix", "/api*"));
        assert!(under("/anything", "/"));
        assert!(under("/anything", "/*"));
    }

    #[test]
    fn sliding_counts() {
        assert_eq!(sliding(3, 0, 0, 60_000), 3);
        assert_eq!(sliding(3, 10, 0, 60_000), 13);
        assert_eq!(sliding(3, 10, 30_000, 60_000), 8);
        assert_eq!(sliding(3, 10, 59_999, 60_000), 4);
        assert_eq!(sliding(3, 10, 70_000, 60_000), 3);
    }

    #[test]
    fn client_addresses() {
        let cdn = [Net::parse("10.0.0.0/8").unwrap()];
        let ip = |s: &str| s.parse::<IpAddr>().unwrap();
        assert_eq!(
            client_addr("203.0.113.9", Some("1.2.3.4"), &cdn),
            Some(ip("203.0.113.9"))
        );
        assert_eq!(
            client_addr("10.0.0.5", Some("6.6.6.6, 1.2.3.4, 10.0.0.9"), &cdn),
            Some(ip("1.2.3.4"))
        );
        assert_eq!(
            client_addr("10.0.0.5", Some("garbage, 10.0.0.9"), &cdn),
            Some(ip("10.0.0.9"))
        );
        assert_eq!(client_addr("10.0.0.5", None, &cdn), Some(ip("10.0.0.5")));
        assert_eq!(
            client_addr("10.0.0.5", Some("1.2.3.4"), &[]),
            Some(ip("10.0.0.5"))
        );
    }

    #[test]
    fn times() {
        let l = Line {
            no: 1,
            words: vec!["retry-after".into()],
        };
        assert_eq!(seconds(&l, "600"), Ok(600));
        assert_eq!(seconds(&l, "10m"), Ok(600));
        assert_eq!(seconds(&l, "2h"), Ok(7200));
        assert!(seconds(&l, "10x").is_err());
        assert!(seconds(&l, "m").is_err());
    }
    #[test]
    fn lists_as_published() {
        let aws = "{\"prefixes\": [{\"ip_prefix\": \"3.5.140.0/22\", \"region\": \"x\"}], \"ipv6_prefixes\": [{\"ipv6_prefix\": \"2600:1f14::/35\"}]}";
        assert_eq!(parse_list(aws).unwrap().len(), 2);
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
}
