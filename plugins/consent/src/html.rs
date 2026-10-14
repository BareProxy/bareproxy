// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! Changes to an HTML page: scripts and iframes from categories without
//! consent made inert, and the banner put before </body>. A short scan,
//! not a parser: it finds each <script and <iframe outside comments, reads
//! the tag's attributes, and skips a script's text as a browser does, to
//! the next </script. Everything else is copied as it is.

use crate::config::Category;
use memchr::memchr;

/// Something kept out of the page.
#[derive(Debug, Clone, PartialEq)]
pub struct Hit {
    /// "script" or "iframe".
    pub tag: &'static str,
    pub category: String,
    /// The block pattern that matched.
    pub by: String,
}

/// The page with its changes, and what was kept out; None when nothing
/// changed.
pub fn rewrite(
    html: &str,
    blocked: &[&Category],
    banner: Option<&str>,
) -> Option<(String, Vec<Hit>)> {
    let b = html.as_bytes();
    let mut out = String::new();
    let mut copied = 0;
    let mut hits = vec![];
    let mut i = 0;
    let any_block = blocked.iter().any(|c| !c.block.is_empty());
    while any_block && let Some(off) = memchr(b'<', &b[i..]) {
        let at = i + off;
        let rest = &b[at..];
        // One byte says most tags (<div, <a, </p) aren't wanted.
        let tag = match rest.get(1).map(|c| c | 0x20) {
            Some(b'!') if rest.starts_with(b"<!--") => {
                i = match memchr::memmem::find(&b[at + 4..], b"-->") {
                    Some(e) => at + 4 + e + 3,
                    None => b.len(),
                };
                continue;
            }
            Some(b's') if name_at(rest, b"script") => "script",
            Some(b'i') if name_at(rest, b"iframe") => "iframe",
            _ => {
                i = at + 1;
                continue;
            }
        };
        let Some((attrs, end)) = attributes(html, at + 7) else {
            break; // a tag that never ends: leave the rest
        };
        // A script's text runs to the next </script, whatever it says.
        let (text_end, after) = if tag == "script" {
            match find_ci(&b[end..], b"</script") {
                Some(e) => (end + e, end + e + 8),
                None => (b.len(), b.len()),
            }
        } else {
            (end, end)
        };
        let src = attrs.iter().find(|a| a.name.eq_ignore_ascii_case("src"));
        let hay = match (tag, src) {
            (_, Some(a)) => a.value.unwrap_or(""),
            ("script", None) => &html[end..text_end],
            _ => "",
        };
        if let Some((cat, by)) = matching(blocked, hay) {
            if out.is_empty() {
                out.reserve(html.len() + banner.map_or(0, str::len) + 256);
            }
            out.push_str(&html[copied..at]);
            out.push('<');
            out.push_str(tag);
            for a in &attrs {
                let n = a.name;
                if tag == "script" && n.eq_ignore_ascii_case("type") {
                    continue; // made text/plain below
                }
                out.push(' ');
                if tag == "iframe" && n.eq_ignore_ascii_case("src") {
                    out.push_str("data-consent-src");
                    out.push_str(&a.raw[n.len()..]);
                } else {
                    out.push_str(a.raw);
                }
            }
            if tag == "script" {
                out.push_str(" type=\"text/plain\"");
            }
            out.push_str(" data-consent=\"");
            out.push_str(&cat.name);
            out.push_str("\">");
            copied = end;
            hits.push(Hit {
                tag,
                category: cat.name.clone(),
                by: by.to_string(),
            });
        }
        i = after;
    }
    let Some(banner) = banner else {
        if hits.is_empty() {
            return None;
        }
        out.push_str(&html[copied..]);
        return Some((out, hits));
    };
    // The banner goes before the last </body>, or at the end when there's
    // none, and never inside a part already copied.
    let at = rfind_ci(&b[copied..], b"</body").map_or(b.len(), |e| copied + e);
    if out.is_empty() {
        out.reserve(html.len() + banner.len());
    }
    out.push_str(&html[copied..at]);
    out.push_str(banner);
    out.push_str(&html[at..]);
    Some((out, hits))
}

/// The first category whose block patterns the text holds, and the pattern.
fn matching<'a>(blocked: &[&'a Category], text: &str) -> Option<(&'a Category, &'a str)> {
    if text.is_empty() {
        return None;
    }
    let low = text.to_ascii_lowercase();
    blocked.iter().find_map(|c| {
        c.block
            .iter()
            .find(|p| low.contains(p.as_str()))
            .map(|p| (*c, p.as_str()))
    })
}

/// Whether the bytes start with < and the tag name, then something that
/// ends a name.
fn name_at(rest: &[u8], name: &[u8]) -> bool {
    rest.len() > name.len()
        && rest[1..=name.len()].eq_ignore_ascii_case(name)
        && rest
            .get(name.len() + 1)
            .is_none_or(|&c| c.is_ascii_whitespace() || c == b'>' || c == b'/')
}

/// An attribute of a tag, as spans of the page.
#[derive(Debug, PartialEq)]
struct Attr<'a> {
    name: &'a str,
    value: Option<&'a str>,
    /// All of it as written: name="value".
    raw: &'a str,
}

/// A tag's attributes, from just after its name, and where the tag ends
/// (after the >). None when it never ends.
fn attributes(html: &str, from: usize) -> Option<(Vec<Attr<'_>>, usize)> {
    let b = html.as_bytes();
    let mut attrs = vec![];
    let mut i = from;
    loop {
        while i < b.len() && (b[i].is_ascii_whitespace() || b[i] == b'/') {
            i += 1;
        }
        if *b.get(i)? == b'>' {
            return Some((attrs, i + 1));
        }
        let start = i;
        while i < b.len() && !b[i].is_ascii_whitespace() && !matches!(b[i], b'=' | b'>' | b'/') {
            i += 1;
        }
        let name = &html[start..i];
        let mut j = i;
        while j < b.len() && b[j].is_ascii_whitespace() {
            j += 1;
        }
        let mut value = None;
        if b.get(j) == Some(&b'=') {
            j += 1;
            while j < b.len() && b[j].is_ascii_whitespace() {
                j += 1;
            }
            match b.get(j)? {
                &q @ (b'"' | b'\'') => {
                    let close = j + 1 + memchr(q, &b[j + 1..])?;
                    value = Some(&html[j + 1..close]);
                    i = close + 1;
                }
                _ => {
                    let v0 = j;
                    while j < b.len() && !b[j].is_ascii_whitespace() && b[j] != b'>' {
                        j += 1;
                    }
                    value = Some(&html[v0..j]);
                    i = j;
                }
            }
        }
        if name.is_empty() {
            if value.is_none() {
                i += 1; // a stray character: step past it
            }
            continue;
        }
        attrs.push(Attr {
            name,
            value,
            raw: &html[start..i],
        });
    }
}

/// The first place the needle is, ignoring ASCII case.
fn find_ci(hay: &[u8], needle: &[u8]) -> Option<usize> {
    let first = needle[0];
    let mut i = 0;
    while let Some(off) = memchr::memchr2(
        first.to_ascii_lowercase(),
        first.to_ascii_uppercase(),
        &hay[i..],
    ) {
        let at = i + off;
        if hay.len() - at >= needle.len() && hay[at..at + needle.len()].eq_ignore_ascii_case(needle)
        {
            return Some(at);
        }
        i = at + 1;
    }
    None
}

/// The last place the needle is, ignoring ASCII case.
fn rfind_ci(hay: &[u8], needle: &[u8]) -> Option<usize> {
    let first = needle[0];
    let mut end = hay.len();
    while let Some(at) = memchr::memrchr2(
        first.to_ascii_lowercase(),
        first.to_ascii_uppercase(),
        &hay[..end],
    ) {
        if hay.len() - at >= needle.len() && hay[at..at + needle.len()].eq_ignore_ascii_case(needle)
        {
            return Some(at);
        }
        end = at;
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::Config;

    fn cats() -> Vec<Category> {
        Config::parse(
            "category analytics A\nblock analytics googletagmanager.com gtag(\ncategory media M\nblock media youtube.com\ncategory plain P\n",
        )
        .unwrap()
        .categories
    }

    #[test]
    fn makes_blocked_scripts_and_iframes_inert() {
        let c = cats();
        let blocked: Vec<&Category> = c.iter().collect();
        let page = r#"<!doctype html><html><head>
<script async src="https://www.googletagmanager.com/gtag/js?id=G-1"></script>
<SCRIPT type="module" SRC='/app.js'></SCRIPT>
<script>window.dataLayer=[];function gtag(){dataLayer.push(arguments)}</script>
<script type="application/ld+json">{"a":"</p>"}</script>
<!-- <script src="https://www.googletagmanager.com/old.js"></script> -->
</head><body>
<iframe width=560 src="https://www.youtube.com/embed/x" allowfullscreen></iframe>
<iframe src="/map.html"></iframe>
<p>A <scripts> tag isn't one, nor is <script-x>.</p>
<script>var s = "<iframe src='https://www.youtube.com/embed/y'>";</script>
</body></html>"#;
        let (out, hits) = rewrite(page, &blocked, None).unwrap();
        assert!(out.contains(r#"<script async src="https://www.googletagmanager.com/gtag/js?id=G-1" type="text/plain" data-consent="analytics"></script>"#), "{out}");
        assert!(
            out.contains(r#"<SCRIPT type="module" SRC='/app.js'></SCRIPT>"#),
            "not blocked, so untouched"
        );
        assert!(out.contains(r#"<script type="text/plain" data-consent="analytics">window.dataLayer=[];function gtag()"#), "{out}");
        assert!(out.contains(r#"<script type="application/ld+json">{"a":"</p>"}</script>"#));
        assert!(
            out.contains(
                r#"<!-- <script src="https://www.googletagmanager.com/old.js"></script> -->"#
            ),
            "comments are left alone"
        );
        assert!(out.contains(r#"<iframe width=560 data-consent-src="https://www.youtube.com/embed/x" allowfullscreen data-consent="media"></iframe>"#), "{out}");
        assert!(out.contains(r#"<iframe src="/map.html"></iframe>"#));
        // A script's text is skipped: the iframe in it is a string, so the
        // script is blocked as a whole by what it says, not the iframe.
        assert!(out.contains(r#"<script type="text/plain" data-consent="media">var s = "<iframe src='https://www.youtube.com/embed/y'>";</script>"#), "{out}");
        let what: Vec<(&str, &str, &str)> = hits
            .iter()
            .map(|h| (h.tag, h.category.as_str(), h.by.as_str()))
            .collect();
        assert_eq!(
            what,
            [
                ("script", "analytics", "googletagmanager.com"),
                ("script", "analytics", "gtag("),
                ("iframe", "media", "youtube.com"),
                ("script", "media", "youtube.com"),
            ]
        );
        // With only media refused, analytics scripts stay.
        let media: Vec<&Category> = c.iter().filter(|x| x.name == "media").collect();
        let (out, hits) = rewrite(page, &media, None).unwrap();
        assert_eq!(hits.len(), 2);
        assert!(out.contains(
            r#"<script async src="https://www.googletagmanager.com/gtag/js?id=G-1"></script>"#
        ));
        // Nothing to block and no banner: no change.
        assert_eq!(rewrite(page, &[], None), None);
        assert_eq!(rewrite("<p>hi</p>", &blocked, None), None);
    }

    #[test]
    fn puts_the_banner_before_the_last_body_end() {
        let banner = "<div id=\"bp-consent\"></div>";
        let (out, hits) = rewrite(
            "<html><body><p>x</p><p>&lt;/body&gt;</p></BODY ></html>",
            &[],
            Some(banner),
        )
        .unwrap();
        assert_eq!(
            out,
            "<html><body><p>x</p><p>&lt;/body&gt;</p><div id=\"bp-consent\"></div></BODY ></html>"
        );
        assert!(hits.is_empty());
        let (out, _) = rewrite("<p>no body tags</p>", &[], Some(banner)).unwrap();
        assert_eq!(out, "<p>no body tags</p><div id=\"bp-consent\"></div>");
        let c = cats();
        let blocked: Vec<&Category> = c.iter().collect();
        let (out, hits) = rewrite(
            "<body><script src=//youtube.com/x.js></script></body>",
            &blocked,
            Some(banner),
        )
        .unwrap();
        assert_eq!(
            out,
            "<body><script src=//youtube.com/x.js type=\"text/plain\" data-consent=\"media\"></script><div id=\"bp-consent\"></div></body>"
        );
        assert_eq!(hits.len(), 1);
    }

    #[test]
    fn odd_markup_does_no_harm() {
        let c = cats();
        let blocked: Vec<&Category> = c.iter().collect();
        for page in [
            "<script src=\"https://youtube.com/a.js\"",
            "<script src='https://youtube.com/a.js>",
            "<script",
            "<",
            "<!-- never closed <script src=https://youtube.com/a.js></script>",
            "<script>never closed youtube.com",
            "<iframe src = \"https://youtube.com/embed\" >",
            "<script data-x=\"a>b\" src=\"https://youtube.com/a.js\"></script>",
        ] {
            let got = rewrite(page, &blocked, None);
            if let Some((out, _)) = &got {
                assert!(out.contains("data-consent"), "{page} -> {out}");
            }
        }
        let (out, _) = rewrite(
            "<iframe src = \"https://youtube.com/embed\" >",
            &blocked,
            None,
        )
        .unwrap();
        assert_eq!(
            out,
            "<iframe data-consent-src = \"https://youtube.com/embed\" data-consent=\"media\">"
        );
        let (out, _) = rewrite(
            "<script data-x=\"a>b\" src=\"https://youtube.com/a.js\"></script>",
            &blocked,
            None,
        )
        .unwrap();
        assert_eq!(
            out,
            "<script data-x=\"a>b\" src=\"https://youtube.com/a.js\" type=\"text/plain\" data-consent=\"media\"></script>"
        );
        let (out, _) = rewrite("<script>never closed youtube.com", &blocked, None).unwrap();
        assert_eq!(
            out,
            "<script type=\"text/plain\" data-consent=\"media\">never closed youtube.com"
        );
        assert_eq!(find_ci(b"abc</ScRiPt>", b"</script"), Some(3));
        assert_eq!(rfind_ci(b"</body>x</BODY>", b"</body"), Some(8));
        assert_eq!(rfind_ci(b"none", b"</body"), None);
    }
}
