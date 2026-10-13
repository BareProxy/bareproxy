// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! The link previews plugin's config, which fetchers it serves, and how it
//! fills in a page's Open Graph tags, kept apart from the Proxy-Wasm calls
//! so it can be tested on its own. The page reading is deliberately small:
//! it looks for a handful of tags in the HTML, it doesn't build a document.

use kit::Item;

/// Words in the User-Agent of the fetchers that make link previews.
pub const FETCHERS: &[&str] = &[
    "facebookexternalhit",
    "facebot",
    "twitterbot",
    "slackbot",
    "slack-imgproxy",
    "whatsapp",
    "linkedinbot",
    "telegrambot",
    "discordbot",
    "skypeuripreview",
    "redditbot",
    "pinterest",
    "vkshare",
    "embedly",
    "iframely",
    "mastodon",
    "cardyb",
    "google-pagerenderer",
    "applebot",
    "bitlybot",
];

#[derive(Debug, Clone, PartialEq, Default)]
pub struct Section {
    pub path: String,
    pub title: Option<String>,
    pub description: Option<String>,
    pub image: Option<String>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct Config {
    pub fetchers: Vec<String>,
    pub site_name: Option<String>,
    /// Longest path first; the last covers the whole site.
    pub sections: Vec<Section>,
}

impl Config {
    pub fn parse(text: &str) -> Result<Config, String> {
        let mut c = Config {
            fetchers: FETCHERS.iter().map(|s| s.to_string()).collect(),
            site_name: None,
            sections: vec![],
        };
        let mut secs = vec![Section {
            path: "/".into(),
            ..Section::default()
        }];
        for item in kit::parse(text, &[])? {
            let Item::Line(line) = item else {
                unreachable!()
            };
            let rest = line.args().join(" ");
            let need = |what: &str| -> Result<String, String> {
                if rest.is_empty() {
                    Err(line.err(format!("{} takes {what}", line.name())))
                } else {
                    Ok(rest.clone())
                }
            };
            let top = secs.len() == 1;
            match line.name() {
                "fetchers" => match line.args() {
                    [w, words @ ..] if w == "add" && !words.is_empty() => c
                        .fetchers
                        .extend(words.iter().map(|x| x.to_ascii_lowercase())),
                    [w, words @ ..] if w == "only" && !words.is_empty() => {
                        c.fetchers = words.iter().map(|x| x.to_ascii_lowercase()).collect()
                    }
                    _ => return Err(line.err("fetchers takes add WORD... or only WORD...")),
                },
                "site-name" => {
                    if !top {
                        return Err(line.err("site-name goes at the top, before any path section"));
                    }
                    c.site_name = Some(need("the site's name")?);
                }
                "path" => match line.args() {
                    [p] if p.starts_with('/') => {
                        if secs.iter().any(|s| s.path == *p) {
                            return Err(line.err(format!("path {p} is set twice")));
                        }
                        secs.push(Section {
                            path: p.clone(),
                            ..Section::default()
                        });
                    }
                    _ => return Err(line.err("path takes one path prefix, such as /docs/")),
                },
                "title" => secs.last_mut().unwrap().title = Some(need("a title")?),
                "description" => {
                    secs.last_mut().unwrap().description = Some(need("a description")?)
                }
                "image" => {
                    let v = need("an image's path or URL")?;
                    if !(v.starts_with('/')
                        || v.starts_with("https://")
                        || v.starts_with("http://"))
                    {
                        return Err(line.err(format!(
                            "{v} isn't a path or a full http:// or https:// URL"
                        )));
                    }
                    secs.last_mut().unwrap().image = Some(v);
                }
                other => return Err(line.err(format!("unknown setting {other}"))),
            }
        }
        let top = secs[0].clone();
        for s in &mut secs[1..] {
            if s.image.is_none() {
                s.image = top.image.clone();
            }
        }
        secs.sort_by_key(|s| std::cmp::Reverse(s.path.len()));
        c.sections = secs;
        Ok(c)
    }

    /// The fetcher's word in a User-Agent, when it's one.
    pub fn fetcher(&self, agent: &str) -> Option<&str> {
        let a = agent.to_ascii_lowercase();
        self.fetchers
            .iter()
            .find(|w| a.contains(w.as_str()))
            .map(|s| s.as_str())
    }

    pub fn section(&self, path: &str) -> &Section {
        self.sections
            .iter()
            .find(|s| kit::under(path, &s.path))
            .unwrap_or(self.sections.last().unwrap())
    }

    /// The page with the Open Graph tags it lacks added to its head, and
    /// the names of the tags added; None when it has all it needs.
    /// `base` is the page's origin, such as https://example.com.
    pub fn fill(&self, html: &str, base: &str, path: &str) -> Option<(String, Vec<String>)> {
        let lower = html.to_ascii_lowercase();
        let head_end = lower.find("</head>").unwrap_or(lower.len());
        let head = &lower[..head_end];
        let has = |prop: &str| {
            head.contains(&format!("property=\"{prop}\""))
                || head.contains(&format!("property='{prop}'"))
                || head.contains(&format!("name=\"{prop}\""))
        };
        let sec = self.section(path);
        let mut tags: Vec<(String, String)> = vec![];
        if !has("og:title")
            && let Some(t) = sec
                .title
                .clone()
                .or_else(|| text_of(html, &lower, "title"))
                .or_else(|| text_of(html, &lower, "h1"))
        {
            tags.push(("og:title".into(), t));
        }
        if !has("og:description")
            && let Some(d) = sec
                .description
                .clone()
                .or_else(|| meta_content(html, &lower, "description"))
                .or_else(|| first_paragraph(html, &lower))
        {
            tags.push(("og:description".into(), cut(&d, 200)));
        }
        let mut image = false;
        if !has("og:image") {
            let found = sec
                .title
                .as_ref()
                .and(sec.image.clone())
                .or_else(|| first_image(html, &lower))
                .or(sec.image.clone());
            if let Some(src) = found {
                tags.push(("og:image".into(), absolute(&src, base, path)));
                image = true;
            }
        } else {
            image = true;
        }
        if tags.is_empty() {
            return None;
        }
        if !has("og:url") {
            tags.push(("og:url".into(), format!("{base}{path}")));
        }
        if !has("og:type") {
            tags.push(("og:type".into(), "website".into()));
        }
        if !has("og:site_name")
            && let Some(n) = &self.site_name
        {
            tags.push(("og:site_name".into(), n.clone()));
        }
        if !has("twitter:card") {
            tags.push((
                "twitter:card".into(),
                if image {
                    "summary_large_image"
                } else {
                    "summary"
                }
                .into(),
            ));
        }
        let mut block = String::from("\n");
        for (k, v) in &tags {
            let attr = if k.starts_with("twitter:") {
                "name"
            } else {
                "property"
            };
            block.push_str(&format!(
                "<meta {attr}=\"{k}\" content=\"{}\">\n",
                escape(v)
            ));
        }
        let at = if head_end < lower.len() {
            head_end
        } else if let Some(i) = lower.find("<head") {
            lower[i..].find('>').map(|j| i + j + 1).unwrap_or(0)
        } else {
            0
        };
        let mut out = String::with_capacity(html.len() + block.len());
        out.push_str(&html[..at]);
        out.push_str(&block);
        out.push_str(&html[at..]);
        Some((out, tags.into_iter().map(|(k, _)| k).collect()))
    }
}

/// The text inside the first <tag>...</tag>, tags inside stripped.
fn text_of(html: &str, lower: &str, tag: &str) -> Option<String> {
    let open = find_tag(lower, tag, 0)?;
    let start = lower[open..].find('>')? + open + 1;
    let end = lower[start..].find(&format!("</{tag}"))? + start;
    let t = clean(&html[start..end]);
    (!t.is_empty()).then_some(t)
}

/// Where the first <tag ...> or <tag> starts, from `from`.
fn find_tag(lower: &str, tag: &str, from: usize) -> Option<usize> {
    let pat = format!("<{tag}");
    let mut i = from;
    while let Some(j) = lower[i..].find(&pat) {
        let at = i + j;
        let next = lower
            .as_bytes()
            .get(at + pat.len())
            .copied()
            .unwrap_or(b'>');
        if next == b'>' || next.is_ascii_whitespace() || next == b'/' {
            return Some(at);
        }
        i = at + pat.len();
    }
    None
}

/// A tag's attribute value, from the tag's text (lower-cased name).
fn attr(tag_html: &str, tag_lower: &str, name: &str) -> Option<String> {
    for q in ['"', '\''] {
        let pat = format!(" {name}={q}");
        if let Some(i) = tag_lower.find(&pat) {
            let s = i + pat.len();
            let e = tag_lower[s..].find(q)? + s;
            return Some(decode(&tag_html[s..e]));
        }
    }
    None
}

fn meta_content(html: &str, lower: &str, name: &str) -> Option<String> {
    let mut i = 0;
    while let Some(at) = find_tag(lower, "meta", i) {
        let end = lower[at..].find('>')? + at;
        let (t, tl) = (&html[at..end], &lower[at..end]);
        if attr(t, tl, "name").is_some_and(|n| n.eq_ignore_ascii_case(name)) {
            return attr(t, tl, "content")
                .map(|c| clean(&c))
                .filter(|c| !c.is_empty());
        }
        i = end;
    }
    None
}

/// The first paragraph with some substance: 40 characters or more.
fn first_paragraph(html: &str, lower: &str) -> Option<String> {
    let mut i = lower.find("<body").unwrap_or(0);
    while let Some(at) = find_tag(lower, "p", i) {
        let start = lower[at..].find('>')? + at + 1;
        let end = lower[start..]
            .find("</p")
            .map(|e| e + start)
            .unwrap_or(lower.len().min(start + 2000));
        let t = clean(&html[start..end]);
        if t.chars().count() >= 40 {
            return Some(t);
        }
        i = end;
    }
    None
}

/// The first image that looks like a picture: not inline data, not an SVG
/// icon, and not given a small width.
fn first_image(html: &str, lower: &str) -> Option<String> {
    let mut i = lower.find("<body").unwrap_or(0);
    while let Some(at) = find_tag(lower, "img", i) {
        let end = lower[at..].find('>')? + at;
        let (t, tl) = (&html[at..end], &lower[at..end]);
        i = end;
        let Some(src) = attr(t, tl, "src") else {
            continue;
        };
        let small = attr(t, tl, "width")
            .and_then(|w| w.trim_end_matches("px").parse::<u32>().ok())
            .is_some_and(|w| w < 200);
        if src.starts_with("data:") || src.to_ascii_lowercase().ends_with(".svg") || small {
            continue;
        }
        return Some(src);
    }
    None
}

fn absolute(src: &str, base: &str, path: &str) -> String {
    if src.starts_with("https://") || src.starts_with("http://") {
        return src.to_string();
    }
    if let Some(rest) = src.strip_prefix("//") {
        let scheme = base.split("://").next().unwrap_or("https");
        return format!("{scheme}://{rest}");
    }
    if src.starts_with('/') {
        return format!("{base}{src}");
    }
    let dir = &path[..path.rfind('/').map(|i| i + 1).unwrap_or(0)];
    format!("{base}{dir}{src}")
}

/// Text from HTML: tags out, entities decoded, white space collapsed.
fn clean(s: &str) -> String {
    let mut out = String::new();
    let mut in_tag = false;
    for ch in s.chars() {
        match ch {
            '<' => in_tag = true,
            '>' => {
                in_tag = false;
                out.push(' ');
            }
            _ if !in_tag => out.push(ch),
            _ => {}
        }
    }
    decode(&out)
        .split_whitespace()
        .collect::<Vec<_>>()
        .join(" ")
}

fn decode(s: &str) -> String {
    s.replace("&nbsp;", " ")
        .replace("&lt;", "<")
        .replace("&gt;", ">")
        .replace("&quot;", "\"")
        .replace("&#39;", "'")
        .replace("&#x27;", "'")
        .replace("&amp;", "&")
}

fn escape(s: &str) -> String {
    s.replace('&', "&amp;")
        .replace('"', "&quot;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
}

/// Cut to at most n characters, at a word, with an ellipsis.
fn cut(s: &str, n: usize) -> String {
    if s.chars().count() <= n {
        return s.to_string();
    }
    let mut out: String = s.chars().take(n).collect();
    if let Some(i) = out.rfind(' ') {
        out.truncate(i);
    }
    out.push('…');
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    const PAGE: &str = r#"<!doctype html>
<html><head>
<meta charset="utf-8">
<title>Tea &amp; Biscuits | Example</title>
<meta name="description" content="How to make a good pot of tea, with biscuits.">
</head>
<body>
<img src="/logo.svg" alt="logo">
<img src="/icon.png" width="32">
<h1>Tea</h1>
<p>Short.</p>
<p>Warm the pot first. Then put in one spoon of tea for each cup, and one for the pot.</p>
<img src="photos/pot.jpg" alt="a teapot">
</body></html>"#;

    fn tag(html: &str, k: &str) -> Option<String> {
        let lower = html.to_ascii_lowercase();
        let pat = format!("=\"{k}\" content=\"");
        let i = lower.find(&pat)? + pat.len();
        Some(html[i..i + html[i..].find('"')?].to_string())
    }

    #[test]
    fn fills_a_bare_page() {
        let c = Config::parse("site-name Example").unwrap();
        let (out, added) = c
            .fill(PAGE, "https://example.com", "/guides/tea.html")
            .unwrap();
        assert_eq!(
            added,
            [
                "og:title",
                "og:description",
                "og:image",
                "og:url",
                "og:type",
                "og:site_name",
                "twitter:card"
            ]
        );
        assert_eq!(
            tag(&out, "og:title").as_deref(),
            Some("Tea &amp; Biscuits | Example")
        );
        assert_eq!(
            tag(&out, "og:description").as_deref(),
            Some("How to make a good pot of tea, with biscuits.")
        );
        assert_eq!(
            tag(&out, "og:image").as_deref(),
            Some("https://example.com/guides/photos/pot.jpg")
        );
        assert_eq!(
            tag(&out, "og:url").as_deref(),
            Some("https://example.com/guides/tea.html")
        );
        assert_eq!(
            tag(&out, "twitter:card").as_deref(),
            Some("summary_large_image")
        );
        // In the head, and the page otherwise as it was.
        let at = out.find("og:title").unwrap();
        assert!(at < out.find("</head>").unwrap() && at > out.find("<head>").unwrap());
        assert_eq!(
            out.replace(
                &out[out.find("\n<meta property=\"og:title\"").unwrap()
                    ..out.find("</head>").unwrap()],
                ""
            ),
            PAGE
        );
    }

    #[test]
    fn falls_back() {
        let c = Config::parse("image /img/card.png").unwrap();
        let page = "<html><head></head><body><h1>Only &lt;a&gt; heading</h1><p>Too short.</p></body></html>";
        let (out, added) = c.fill(page, "http://x.test", "/").unwrap();
        assert_eq!(
            tag(&out, "og:title").as_deref(),
            Some("Only &lt;a&gt; heading")
        );
        assert_eq!(
            tag(&out, "og:image").as_deref(),
            Some("http://x.test/img/card.png")
        );
        assert!(!added.contains(&"og:description".to_string()));
        // No head at all.
        let (out, _) = c
            .fill(
                "<p>Just a paragraph that is long enough to be a description.</p>",
                "http://x.test",
                "/a",
            )
            .unwrap();
        assert!(out.starts_with("\n<meta property=\"og:description\""));
    }

    #[test]
    fn leaves_good_pages_alone() {
        let c = Config::parse("").unwrap();
        let page = r#"<html><head><meta property="og:title" content="T"><meta property="og:description" content="D"><meta property='og:image' content="https://x/i.png"></head><body></body></html>"#;
        assert_eq!(c.fill(page, "https://x", "/"), None);
        // Only what's missing is added.
        let page = r#"<html><head><title>T</title><meta property="og:image" content="https://x/i.png"></head></html>"#;
        let (_, added) = c.fill(page, "https://x", "/").unwrap();
        assert_eq!(added, ["og:title", "og:url", "og:type", "twitter:card"]);
    }

    #[test]
    fn sections() {
        let c = Config::parse("image /card.png\npath /docs/\n  title BareProxy docs\n  description Every setting, explained.\n").unwrap();
        let (out, _) = c.fill(PAGE, "https://e.com", "/docs/plugins").unwrap();
        assert_eq!(tag(&out, "og:title").as_deref(), Some("BareProxy docs"));
        assert_eq!(
            tag(&out, "og:description").as_deref(),
            Some("Every setting, explained.")
        );
        assert_eq!(
            tag(&out, "og:image").as_deref(),
            Some("https://e.com/card.png"),
            "a section with a title takes its card image"
        );
    }

    #[test]
    fn fetchers() {
        let c = Config::parse("fetchers add MyPreviewer").unwrap();
        assert_eq!(
            c.fetcher("Slackbot-LinkExpanding 1.0 (+https://api.slack.com/robots)"),
            Some("slackbot")
        );
        assert_eq!(
            c.fetcher("facebookexternalhit/1.1"),
            Some("facebookexternalhit")
        );
        assert_eq!(c.fetcher("WhatsApp/2.23.20.0"), Some("whatsapp"));
        assert_eq!(c.fetcher("mypreviewer/2"), Some("mypreviewer"));
        assert_eq!(c.fetcher("Mozilla/5.0 (Macintosh) Safari/605.1"), None);
    }

    #[test]
    fn long_descriptions_are_cut() {
        let d = cut(&"word ".repeat(100), 200);
        assert!(d.chars().count() <= 201 && d.ends_with("word…"));
    }

    #[test]
    fn config_errors() {
        for (text, want) in [
            ("image card.png", "line 1: card.png isn't a path or a full"),
            ("title", "line 1: title takes a title"),
            ("fetchers slack", "line 1: fetchers takes add WORD"),
            ("path /a\nsite-name X", "line 2: site-name goes at the top"),
            ("path a", "line 1: path takes one path prefix"),
            ("colour red", "line 1: unknown setting colour"),
        ] {
            let err = Config::parse(text).unwrap_err();
            assert!(err.contains(want), "{text}: {err}");
        }
    }
}
