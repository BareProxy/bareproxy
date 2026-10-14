// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! The Markdown plugin's config, and the rules around it: which file a URL
//! names, what a client asks for, ETags, and the page template.

use crate::render::{Page, escape_into};
use kit::Item;
use std::borrow::Cow;

/// The page around the content when the config names no template.
pub const DEFAULT_TEMPLATE: &str = include_str!("page.html");

/// The settings, and what they're called in a note.
const SETTINGS: &str = "template, index, skip, html, links, accept, lang, largest and cache";

#[derive(Debug, Clone, PartialEq)]
pub struct Config {
    /// The template file, and the line that names it.
    pub template: Option<(String, usize)>,
    /// The files that serve a folder's URL, in order.
    pub index: Vec<String>,
    /// Path prefixes the plugin leaves alone.
    pub skip: Vec<String>,
    /// Raw HTML in the Markdown passes as written.
    pub html: bool,
    /// Links to .md files point to their pages.
    pub links: bool,
    /// A client that asks for text/markdown gets the Markdown at the page's URL.
    pub accept: bool,
    /// {{lang}} when the front matter doesn't say.
    pub lang: String,
    /// The largest file rendered.
    pub largest: u64,
    /// The most memory rendered pages take in each instance; 0 keeps none.
    pub cache: u64,
    /// A hash of the config's text, part of every ETag.
    pub hash: u64,
}

impl Default for Config {
    fn default() -> Config {
        Config {
            template: None,
            index: vec!["index.md".into(), "README.md".into()],
            skip: vec![],
            html: true,
            links: true,
            accept: true,
            lang: "en".into(),
            largest: 512 << 10,
            cache: 16 << 20,
            hash: fnv(b"", FNV_START),
        }
    }
}

impl Config {
    pub fn parse(text: &str) -> Result<Config, String> {
        let mut c = Config {
            hash: fnv(text.as_bytes(), FNV_START),
            ..Config::default()
        };
        let mut seen: Vec<(String, usize)> = vec![];
        for item in kit::parse(text, &[])? {
            let Item::Line(l) = item else { continue };
            let name = l.name().to_string();
            if name != "skip" {
                if let Some((_, at)) = seen.iter().find(|(n, _)| *n == name) {
                    return Err(l.err(format!("{name} is set already, on line {at}")));
                }
                seen.push((name.clone(), l.no));
            }
            let one = || match l.args() {
                [v] => Ok(v.clone()),
                _ => Err(l.err(format!("{name} takes one value"))),
            };
            match name.as_str() {
                "template" => {
                    let [f] = l.args() else {
                        return Err(l.err("template takes one file, such as template page.html"));
                    };
                    c.template = Some((f.clone(), l.no));
                }
                "index" => {
                    let bad = |n: &String| {
                        n.len() < 4 || !n.ends_with(".md") || n.contains('/') || n.starts_with('.')
                    };
                    if l.args().is_empty() || l.args().iter().any(bad) {
                        return Err(l.err(
                            "index takes file names ending in .md, such as index index.md README.md",
                        ));
                    }
                    c.index = l.args().to_vec();
                }
                "skip" => {
                    if l.args().is_empty() || l.args().iter().any(|p| !p.starts_with('/')) {
                        return Err(l.err("skip takes paths that start with /, such as skip /api/"));
                    }
                    c.skip.extend_from_slice(l.args());
                }
                "html" => c.html = kit::switch(&l)?,
                "links" => c.links = kit::switch(&l)?,
                "accept" => c.accept = kit::switch(&l)?,
                "lang" => {
                    let v = one()?;
                    let ok = v.len() <= 35
                        && !v.starts_with('-')
                        && v.chars().all(|ch| ch.is_ascii_alphanumeric() || ch == '-');
                    if !ok {
                        return Err(l.err(format!("{v} isn't a language tag, such as en or pt-BR")));
                    }
                    c.lang = v;
                }
                "largest" => {
                    let n = kit::size(&l, &one()?)?;
                    if !(1 << 10..=1 << 30).contains(&n) {
                        return Err(l.err("largest is from 1KB to 1GB"));
                    }
                    c.largest = n;
                }
                "cache" => {
                    let n = kit::size(&l, &one()?)?;
                    if n > 1 << 30 {
                        return Err(l.err("cache is at most 1GB; 0 keeps no pages"));
                    }
                    c.cache = n;
                }
                _ => {
                    return Err(l.err(format!(
                        "unknown setting {name}; the settings are {SETTINGS}"
                    )));
                }
            }
        }
        Ok(c)
    }

    /// Which files a request's page would come from.
    pub fn lookup(&self, method: &str, raw_path: &str) -> Lookup {
        if method != "GET" && method != "HEAD" {
            return Lookup::Pass;
        }
        let Some(path) = normalize(raw_path) else {
            return Lookup::Pass; // BareProxy answers 400
        };
        if self.skip.iter().any(|p| kit::under(&path, p)) {
            return Lookup::Pass;
        }
        // Hidden names (.git, .env) are never served, like the files route.
        if path.split('/').any(|s| s.starts_with('.')) {
            return Lookup::Pass;
        }
        let rel = &path[1..];
        let files = if rel.is_empty() || rel.ends_with('/') {
            self.index.iter().map(|n| format!("{rel}{n}")).collect()
        } else if rel.ends_with(".md") {
            return Lookup::Pass; // the Markdown itself, from the route
        } else {
            vec![format!("{rel}.md")]
        };
        Lookup::Files { key: path, files }
    }

    /// A page's ETag, from what makes the page: the file's size and time of
    /// change, the template's, the config and the plugin's version. Every
    /// instance gives the same one, and none has to read the file for it.
    /// The Markdown's own ETag ends in -md.
    pub fn etag(
        &self,
        file: &str,
        size: u64,
        mtime: u64,
        tpl: Option<(u64, u64)>,
        md: bool,
    ) -> String {
        let mut h = fnv(env!("CARGO_PKG_VERSION").as_bytes(), FNV_START);
        h = fnv(&self.hash.to_le_bytes(), h);
        h = fnv(file.as_bytes(), h);
        for n in [size, mtime] {
            h = fnv(&n.to_le_bytes(), h);
        }
        if let Some((ts, tm)) = tpl {
            for n in [ts, tm] {
                h = fnv(&n.to_le_bytes(), h);
            }
        }
        if md {
            format!("\"{h:016x}-md\"")
        } else {
            format!("\"{h:016x}\"")
        }
    }
}

/// What the plugin does with a request.
#[derive(Debug, PartialEq)]
pub enum Lookup {
    /// Leaves it alone: not GET or HEAD, skipped, hidden, or a .md file.
    Pass,
    /// Looks for these files, in order. key is the path decoded and in its
    /// normal form.
    Files { key: String, files: Vec<String> },
}

/// A path decoded and in its normal form, as BareProxy routes it: %XX
/// decoded, empty and . segments dropped, .. taking the one before it, a
/// trailing slash kept. None for what BareProxy refuses: an encoded slash
/// or backslash, control characters, a climb above /, or bytes that aren't
/// UTF-8.
pub fn normalize(raw: &str) -> Option<String> {
    let b = raw.as_bytes();
    if b.first() != Some(&b'/') {
        return None;
    }
    let mut out = Vec::with_capacity(b.len());
    let mut i = 0;
    while i < b.len() {
        let c = b[i];
        if c == b'%' {
            let hex = std::str::from_utf8(b.get(i + 1..i + 3)?).ok()?;
            let v = u8::from_str_radix(hex, 16).ok()?;
            if v == b'/' || v == b'\\' || v < 0x20 || v == 0x7f {
                return None;
            }
            out.push(v);
            i += 3;
            continue;
        }
        if c == b'\\' || c < 0x20 || c == 0x7f {
            return None;
        }
        out.push(c);
        i += 1;
    }
    let s = String::from_utf8(out).ok()?;
    let segs: Vec<&str> = s[1..].split('/').collect();
    let mut kept: Vec<&str> = Vec::with_capacity(segs.len());
    for seg in &segs {
        match *seg {
            "" | "." => {}
            ".." => {
                kept.pop()?;
            }
            seg => kept.push(seg),
        }
    }
    if kept.is_empty() {
        return Some("/".into());
    }
    let mut p = format!("/{}", kept.join("/"));
    if matches!(segs.last(), Some(&"" | &"." | &"..")) {
        p.push('/');
    }
    Some(p)
}

/// A decoded path made fit for a URL again.
pub fn encode_path(p: &str) -> String {
    let mut s = String::with_capacity(p.len());
    for &b in p.as_bytes() {
        if b.is_ascii_alphanumeric() || b"-._~/!$&'()*+,;=:@".contains(&b) {
            s.push(b as char);
        } else {
            s.push_str(&format!("%{b:02X}"));
        }
    }
    s
}

/// Whether a client asks for Markdown before HTML: its Accept header names
/// text/markdown with a q at least as high as HTML's, which a browser's
/// never does. Only a type named outright counts for Markdown; text/* and
/// */* count for HTML.
pub fn wants_markdown(accept: &str) -> bool {
    let (mut md, mut html, mut text, mut any) = (0f32, None, None, None);
    for part in accept.split(',') {
        let mut it = part.split(';');
        let ty = it.next().unwrap_or("").trim().to_ascii_lowercase();
        let mut q = 1f32;
        for p in it {
            if let Some((k, v)) = p.split_once('=')
                && k.trim().eq_ignore_ascii_case("q")
            {
                q = v.trim().parse().unwrap_or(0.0);
            }
        }
        match ty.as_str() {
            "text/markdown" | "text/x-markdown" => md = md.max(q),
            "text/html" => html = Some(q),
            "text/*" => text = Some(q),
            "*/*" => any = Some(q),
            _ => {}
        }
    }
    let html = html.or(text).or(any).unwrap_or(0.0);
    md > 0.0 && md >= html
}

/// Whether an If-None-Match header names the ETag (or says *).
pub fn none_match(header: &str, etag: &str) -> bool {
    header
        .split(',')
        .map(str::trim)
        .any(|t| t == "*" || t.strip_prefix("W/").unwrap_or(t) == etag)
}

/// A template: text with {{name}} in it, read once.
#[derive(Debug, Clone, PartialEq)]
pub struct Template {
    segs: Vec<Seg>,
}

#[derive(Debug, Clone, PartialEq)]
enum Seg {
    Text(String),
    Var(String),
}

impl Template {
    /// Reads a template. It needs a {{content}}; a {{ that doesn't start a
    /// name stays as it is.
    pub fn parse(text: &str) -> Result<Template, String> {
        let mut segs = Vec::new();
        let mut lit = String::new();
        let mut rest = text;
        while let Some(i) = rest.find("{{") {
            let after = &rest[i + 2..];
            let name = after.find("}}").map(|j| (j, after[..j].trim()));
            match name {
                Some((j, n))
                    if !n.is_empty()
                        && n.len() <= 64
                        && n.chars()
                            .all(|c| c.is_ascii_alphanumeric() || c == '_' || c == '-') =>
                {
                    lit.push_str(&rest[..i]);
                    if !lit.is_empty() {
                        segs.push(Seg::Text(std::mem::take(&mut lit)));
                    }
                    segs.push(Seg::Var(n.to_ascii_lowercase()));
                    rest = &after[j + 2..];
                }
                _ => {
                    lit.push_str(&rest[..i + 2]);
                    rest = after;
                }
            }
        }
        lit.push_str(rest);
        if !lit.is_empty() {
            segs.push(Seg::Text(lit));
        }
        if !segs.contains(&Seg::Var("content".into())) {
            return Err("has no {{content}}, the place the page goes".into());
        }
        Ok(Template { segs })
    }

    /// The template with each {{name}} filled in. content and toc go in as
    /// HTML; every other value is text, escaped.
    pub fn fill<'v>(&self, value: impl Fn(&str) -> Cow<'v, str>) -> String {
        let mut out = String::new();
        for s in &self.segs {
            match s {
                Seg::Text(t) => out.push_str(t),
                Seg::Var(n) if n == "content" || n == "toc" => out.push_str(&value(n)),
                Seg::Var(n) => escape_into(&mut out, &value(n)),
            }
        }
        out
    }
}

/// Where a page is: what the template's other names are filled with.
pub struct At<'a> {
    /// The file, inside the folder.
    pub file: &'a str,
    /// The page's path, decoded, in its normal form.
    pub path: &'a str,
    /// The site, for the title of a site's front page that has none.
    pub site: &'a str,
}

impl Config {
    /// A whole page: the template filled in with a rendered file.
    pub fn page(&self, tpl: &Template, p: &Page, at: &At) -> String {
        let title = if p.title.is_empty() {
            self.fallback_title(at)
        } else {
            p.title.clone()
        };
        tpl.fill(|name| match name {
            "content" => Cow::Borrowed(p.content.as_str()),
            "toc" => Cow::Borrowed(p.toc.as_str()),
            "title" => Cow::Owned(title.clone()),
            "description" => Cow::Borrowed(p.description.as_str()),
            "lang" => Cow::Owned(p.meta("lang").unwrap_or(&self.lang).to_string()),
            "path" => Cow::Owned(encode_path(at.path)),
            "source" => Cow::Owned(source_url(at)),
            "file" => Cow::Owned(at.file.to_string()),
            other => Cow::Owned(p.meta(other).unwrap_or("").to_string()),
        })
    }

    /// The title of a page that gives none: its file's name (a folder's
    /// for an index file, the site's for the front page), dashes and
    /// underscores made spaces, the first letter made a capital.
    pub fn fallback_title(&self, at: &At) -> String {
        let (dir, name) = at.file.rsplit_once('/').unwrap_or(("", at.file));
        let raw = if self.index.iter().any(|n| n == name) {
            match dir.rsplit_once('/').map_or(dir, |(_, d)| d) {
                "" => return at.site.to_string(),
                d => d,
            }
        } else {
            name.strip_suffix(".md").unwrap_or(name)
        };
        let words = raw.replace(['-', '_'], " ");
        let mut c = words.chars();
        match c.next() {
            Some(f) => f.to_uppercase().chain(c).collect(),
            None => at.site.to_string(),
        }
    }
}

/// The URL of a page's Markdown: the folder's URL and the index file's
/// name for a folder, the page's URL with .md for the rest.
pub fn source_url(at: &At) -> String {
    if at.path.ends_with('/') {
        let name = at.file.rsplit_once('/').map_or(at.file, |(_, n)| n);
        encode_path(&format!("{}{name}", at.path))
    } else {
        encode_path(&format!("{}.md", at.path))
    }
}

const FNV_START: u64 = 0xcbf29ce484222325;

/// FNV-1a, 64 bits: for ETags and the config's hash, never for secrets.
fn fnv(bytes: &[u8], mut h: u64) -> u64 {
    for &b in bytes {
        h ^= b as u64;
        h = h.wrapping_mul(0x100000001b3);
    }
    h
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::render::{Opts, render};

    #[test]
    fn parses_a_config() {
        let c = Config::parse(
            "# docs\ntemplate theme/page.html\nindex index.md\nskip /api/ /feed\nskip /raw/\nhtml off\nlinks off\naccept off\nlang pt-BR\nlargest 4MB\ncache 0\n",
        )
        .unwrap();
        assert_eq!(c.template, Some(("theme/page.html".into(), 2)));
        assert_eq!(c.index, vec!["index.md"]);
        assert_eq!(c.skip, vec!["/api/", "/feed", "/raw/"]);
        assert!(!c.html && !c.links && !c.accept);
        assert_eq!((c.lang.as_str(), c.largest, c.cache), ("pt-BR", 4 << 20, 0));
        let d = Config::parse("").unwrap();
        assert_eq!(
            d,
            Config {
                hash: d.hash,
                ..Config::default()
            }
        );
        assert!(d.html && d.links && d.accept && d.template.is_none());
        assert_ne!(d.hash, c.hash);
    }

    #[test]
    fn refuses_bad_lines() {
        for (text, want) in [
            ("template", "line 1: template takes one file"),
            ("template a b", "line 1: template takes one file"),
            (
                "index README.txt",
                "line 1: index takes file names ending in .md",
            ),
            (
                "index docs/index.md",
                "line 1: index takes file names ending in .md",
            ),
            ("index .md", "line 1: index takes file names ending in .md"),
            ("skip api/", "line 1: skip takes paths that start with /"),
            ("html maybe", "line 1: html takes on or off"),
            ("lang en_US", "line 1: en_US isn't a language tag"),
            ("largest 10B", "line 1: largest is from 1KB to 1GB"),
            ("largest lots", "line 1: lots isn't a size"),
            ("cache 2GB", "line 1: cache is at most 1GB"),
            (
                "lang en\n\nlang fr",
                "line 3: lang is set already, on line 1",
            ),
            (
                "render fast",
                "line 1: unknown setting render; the settings are template,",
            ),
        ] {
            let e = Config::parse(text).unwrap_err();
            assert!(e.starts_with(want), "{text}: {e}");
        }
    }

    #[test]
    fn maps_paths_to_files() {
        let c = Config::parse("skip /api/").unwrap();
        let files = |m: &str, p: &str| match c.lookup(m, p) {
            Lookup::Files { key, files } => format!("{key} {}", files.join(" ")),
            Lookup::Pass => "pass".into(),
        };
        assert_eq!(files("GET", "/"), "/ index.md README.md");
        assert_eq!(
            files("HEAD", "/guide/"),
            "/guide/ guide/index.md guide/README.md"
        );
        assert_eq!(files("GET", "/guide/setup"), "/guide/setup guide/setup.md");
        assert_eq!(files("GET", "/notes/v1.2"), "/notes/v1.2 notes/v1.2.md");
        assert_eq!(
            files("GET", "/caf%C3%A9/men%20u"),
            "/café/men u café/men u.md"
        );
        assert_eq!(files("GET", "/a//b/./c/../d"), "/a/b/d a/b/d.md");
        assert_eq!(files("GET", "/a/b/.."), "/a/ a/index.md a/README.md");
        assert_eq!(files("GET", "/style.css"), "/style.css style.css.md");
        for (m, p) in [
            ("POST", "/guide/"),
            ("GET", "/guide/setup.md"),
            ("GET", "/api/users"),
            ("GET", "/.git/config"),
            ("GET", "/docs/.hidden/"),
            ("GET", "/.well-known/x"),
            ("GET", "/a%2Fb"),
            ("GET", "/a%5Cb"),
            ("GET", "/a%00b"),
            ("GET", "/a\\b"),
            ("GET", "/%"),
            ("GET", "/%zz"),
            ("GET", "/%FF"),
            ("GET", "/../etc/passwd"),
            ("GET", "*"),
        ] {
            assert_eq!(files(m, p), "pass", "{m} {p}");
        }
        assert_eq!(encode_path("/café/men u/a&b"), "/caf%C3%A9/men%20u/a&b");
    }

    #[test]
    fn negotiates_markdown() {
        for (accept, want) in [
            ("text/markdown", true),
            ("text/markdown, text/html;q=0.9", true),
            ("text/x-markdown", true),
            ("text/html, text/markdown", true),
            ("text/markdown;q=0.5, text/html", false),
            ("text/markdown;q=0.5, */*;q=0.1", true),
            ("text/markdown;q=0, */*", false),
            (
                "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
                false,
            ),
            ("*/*", false),
            ("", false),
            ("text/plain, text/markdown;q=0.8, text/*;q=0.7", true),
        ] {
            assert_eq!(wants_markdown(accept), want, "{accept}");
        }
    }

    #[test]
    fn etags_and_conditions() {
        let c = Config::default();
        let a = c.etag("a.md", 10, 99, None, false);
        assert_eq!(
            a,
            c.etag("a.md", 10, 99, None, false),
            "the same in every instance"
        );
        assert!(a.starts_with('"') && a.ends_with('"') && a.len() == 18);
        assert_ne!(a, c.etag("a.md", 11, 99, None, false));
        assert_ne!(a, c.etag("a.md", 10, 98, None, false));
        assert_ne!(a, c.etag("b.md", 10, 99, None, false));
        assert_ne!(a, c.etag("a.md", 10, 99, Some((1, 2)), false));
        let other = Config::parse("html off").unwrap();
        assert_ne!(
            a,
            other.etag("a.md", 10, 99, None, false),
            "the config counts"
        );
        let md = c.etag("a.md", 10, 99, None, true);
        assert_eq!(md, format!("{}-md\"", &a[..a.len() - 1]));
        assert!(none_match(&a, &a));
        assert!(none_match(&format!("\"x\", W/{a}"), &a));
        assert!(none_match("*", &a));
        assert!(!none_match("\"x\"", &a));
        assert!(!none_match("", &a));
    }

    #[test]
    fn templates() {
        let t = Template::parse(
            "<title>{{ title }}</title>{{content}} {{ x {{y}} {{Toc}} {{}} {{a b}}",
        )
        .unwrap();
        let out = t.fill(|n| match n {
            "title" => Cow::Borrowed("A <b> & \"c\""),
            "content" => Cow::Borrowed("<p>hi</p>"),
            "toc" => Cow::Borrowed("<ul></ul>"),
            "y" => Cow::Borrowed("why"),
            _ => Cow::Borrowed(""),
        });
        assert_eq!(
            out,
            "<title>A &lt;b&gt; &amp; &quot;c&quot;</title><p>hi</p> {{ x why <ul></ul> {{}} {{a b}}"
        );
        assert_eq!(
            Template::parse("<html>{{title}}</html>").unwrap_err(),
            "has no {{content}}, the place the page goes"
        );
        assert!(Template::parse(DEFAULT_TEMPLATE).is_ok());
    }

    #[test]
    fn fills_a_page() {
        let c = Config::parse("lang fr").unwrap();
        let tpl = Template::parse(
            "{{lang}}|{{title}}|{{description}}|{{path}}|{{source}}|{{file}}|{{author}}|{{nope}}|{{content}}",
        )
        .unwrap();
        let p = render(
            "---\nauthor: Ann <a@b>\n---\n# Héllo\n\nFirst words.\n",
            &Opts {
                html: true,
                links: true,
                index: &c.index,
            },
        );
        let at = At {
            file: "guide/setup.md",
            path: "/guide/setup",
            site: "docs.example.com",
        };
        assert_eq!(
            c.page(&tpl, &p, &at),
            "fr|Héllo|First words.|/guide/setup|/guide/setup.md|guide/setup.md|Ann &lt;a@b&gt;||<h1 id=\"héllo\">Héllo</h1>\n<p>First words.</p>\n"
        );
        let de = render(
            "---\nlang: de\n---\nText.\n",
            &Opts {
                html: true,
                links: true,
                index: &c.index,
            },
        );
        assert!(c.page(&tpl, &de, &at).starts_with("de|Setup|Text.|"));
    }

    #[test]
    fn titles_and_sources_for_pages_without_one() {
        let c = Config::default();
        let t = |file: &str, path: &str| {
            let at = At {
                file,
                path,
                site: "docs.example.com",
            };
            format!("{} {}", c.fallback_title(&at), source_url(&at))
        };
        assert_eq!(
            t("guide/getting-started.md", "/guide/getting-started"),
            "Getting started /guide/getting-started.md"
        );
        assert_eq!(t("guide/index.md", "/guide/"), "Guide /guide/index.md");
        assert_eq!(
            t("a/my_docs/README.md", "/a/my_docs/"),
            "My docs /a/my_docs/README.md"
        );
        assert_eq!(t("index.md", "/"), "docs.example.com /index.md");
        assert_eq!(t("café.md", "/café"), "Café /caf%C3%A9.md");
    }
}
