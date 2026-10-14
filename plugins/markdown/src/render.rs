// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! Markdown to HTML: pulldown-cmark's parser and HTML writer, with the
//! plugin's changes made on the way through. Headings get ids, links to .md
//! files point to their pages, raw HTML shows as text when the config says
//! so, and the page's title, description, table of contents and front
//! matter are picked up as the events go by. Nothing holds the whole
//! document's events at once, so a big file costs little more memory than
//! the parser's own.

use pulldown_cmark::{
    CodeBlockKind, CowStr, Event, HeadingLevel, MetadataBlockKind, Options, Parser, Tag, TagEnd,
    html,
};
use std::collections::{HashSet, VecDeque};

/// What changes how a page renders.
pub struct Opts<'o> {
    /// Raw HTML passes as written. Off, it shows as text, and links and
    /// images may only be relative or http, https, mailto and tel, so a
    /// file someone else wrote can't run a script.
    pub html: bool,
    /// Links to .md files point to their pages instead.
    pub links: bool,
    /// The files that serve a folder's URL, such as index.md and README.md;
    /// a link to one points to its folder.
    pub index: &'o [String],
}

/// A rendered page.
#[derive(Debug, Default, PartialEq)]
pub struct Page {
    /// The HTML.
    pub content: String,
    /// The front matter's title, or the first level-one heading's text, or
    /// "" when there's neither.
    pub title: String,
    /// The front matter's description (or summary), or the first
    /// paragraph's text, cut to about 200 characters.
    pub description: String,
    /// Links to the level-two and level-three headings as a list, or ""
    /// when there are fewer than two.
    pub toc: String,
    /// The front matter's plain values, keys in lower case.
    pub meta: Vec<(String, String)>,
}

impl Page {
    /// A front matter value.
    pub fn meta(&self, key: &str) -> Option<&str> {
        self.meta
            .iter()
            .find(|(k, _)| k == key)
            .map(|(_, v)| v.as_str())
    }
}

/// The Markdown the plugin reads: CommonMark with GitHub's tables, task
/// lists, strikethrough, footnotes and alerts, heading ids written as
/// {#id}, definition lists, and front matter between --- or +++ lines.
pub fn options() -> Options {
    Options::ENABLE_TABLES
        | Options::ENABLE_FOOTNOTES
        | Options::ENABLE_STRIKETHROUGH
        | Options::ENABLE_TASKLISTS
        | Options::ENABLE_HEADING_ATTRIBUTES
        | Options::ENABLE_YAML_STYLE_METADATA_BLOCKS
        | Options::ENABLE_PLUSES_DELIMITED_METADATA_BLOCKS
        | Options::ENABLE_GFM
        | Options::ENABLE_DEFINITION_LIST
}

/// Renders a Markdown file.
pub fn render(src: &str, o: &Opts) -> Page {
    let mut pass = Pass {
        inner: Parser::new_ext(src, options()),
        o,
        queue: VecDeque::new(),
        ids: HashSet::new(),
        heads: Vec::new(),
        title: None,
        desc: None,
        para: None,
        image: 0,
        meta: String::new(),
        meta_kind: None,
        in_meta: false,
    };
    let mut content = String::with_capacity(src.len() + src.len() / 2);
    html::push_html(&mut content, &mut pass);
    let meta = front_matter(&pass.meta, pass.meta_kind);
    let get = |k: &str| {
        meta.iter()
            .find(|(m, _)| m == k)
            .map(|(_, v)| squash(v))
            .filter(|v| !v.is_empty())
    };
    let title = get("title").or(pass.title).unwrap_or_default();
    let description = get("description")
        .or_else(|| get("summary"))
        .or(pass.desc)
        .map(|d| cut(&d, 200))
        .unwrap_or_default();
    Page {
        content,
        title,
        description,
        toc: toc(&pass.heads),
        meta,
    }
}

/// The events between the parser and the HTML writer, changed as they go.
struct Pass<'a, 'o, I> {
    inner: I,
    o: &'o Opts<'o>,
    /// Events held back: a heading's, until its id is known.
    queue: VecDeque<Event<'a>>,
    ids: HashSet<String>,
    /// Each heading: its level, id and text.
    heads: Vec<(HeadingLevel, String, String)>,
    title: Option<String>,
    desc: Option<String>,
    /// The text of the first paragraph with any, while it's read.
    para: Option<String>,
    /// Inside a picture, whose alt text isn't the paragraph's.
    image: u32,
    meta: String,
    meta_kind: Option<MetadataBlockKind>,
    in_meta: bool,
}

impl<'a, I: Iterator<Item = Event<'a>>> Iterator for Pass<'a, '_, I> {
    type Item = Event<'a>;

    fn next(&mut self) -> Option<Event<'a>> {
        if let Some(e) = self.queue.pop_front() {
            return Some(e);
        }
        let e = self.inner.next()?;
        let Event::Start(Tag::Heading {
            level,
            id,
            classes,
            attrs,
        }) = e
        else {
            return Some(self.fix(e));
        };
        // A heading: its text makes its id, so its events wait for the end.
        let mut text = String::new();
        while let Some(e) = self.inner.next() {
            match &e {
                Event::Text(t) | Event::Code(t) => text.push_str(t),
                Event::SoftBreak | Event::HardBreak => text.push(' '),
                _ => {}
            }
            let end = matches!(e, Event::End(TagEnd::Heading(_)));
            let e = self.fix(e);
            self.queue.push_back(e);
            if end {
                break;
            }
        }
        let text = squash(&text);
        let id = match id {
            Some(id) => {
                self.ids.insert(id.to_string());
                id.to_string()
            }
            None => self.unique(slug(&text)),
        };
        if level == HeadingLevel::H1 && self.title.is_none() && !text.is_empty() {
            self.title = Some(text.clone());
        }
        self.heads.push((level, id.clone(), text));
        Some(Event::Start(Tag::Heading {
            level,
            id: Some(CowStr::from(id)),
            classes,
            attrs,
        }))
    }
}

impl<'a, I> Pass<'a, '_, I> {
    /// An id no heading has yet: the slug, or the slug with -1, -2 and on.
    fn unique(&mut self, base: String) -> String {
        let base = if base.is_empty() {
            "section".to_string()
        } else {
            base
        };
        let mut id = base.clone();
        let mut n = 0;
        while self.ids.contains(&id) {
            n += 1;
            id = format!("{base}-{n}");
        }
        self.ids.insert(id.clone());
        id
    }

    /// One event, as the page gets it.
    fn fix(&mut self, e: Event<'a>) -> Event<'a> {
        match &e {
            Event::Start(Tag::Image { .. }) => self.image += 1,
            Event::End(TagEnd::Image) => self.image = self.image.saturating_sub(1),
            _ => {}
        }
        if let Some(p) = self.para.as_mut().filter(|_| self.image == 0) {
            match &e {
                Event::Text(t) | Event::Code(t) if p.len() < 600 => p.push_str(t),
                Event::SoftBreak | Event::HardBreak => p.push(' '),
                _ => {}
            }
        }
        if self.in_meta
            && let Event::Text(t) = &e
        {
            self.meta.push_str(t);
        }
        match e {
            Event::Start(Tag::MetadataBlock(kind)) => {
                self.in_meta = true;
                self.meta_kind = Some(kind);
                Event::Start(Tag::MetadataBlock(kind))
            }
            Event::End(TagEnd::MetadataBlock(kind)) => {
                self.in_meta = false;
                Event::End(TagEnd::MetadataBlock(kind))
            }
            Event::Start(Tag::Paragraph) if self.desc.is_none() && self.para.is_none() => {
                self.para = Some(String::new());
                Event::Start(Tag::Paragraph)
            }
            Event::End(TagEnd::Paragraph) if self.para.is_some() => {
                // A paragraph with no text (a picture, say) doesn't count.
                self.desc = self
                    .para
                    .take()
                    .map(|p| squash(&p))
                    .filter(|p| !p.is_empty());
                Event::End(TagEnd::Paragraph)
            }
            Event::Start(Tag::Link {
                link_type,
                dest_url,
                title,
                id,
            }) => {
                let mut url = dest_url;
                if self.o.links
                    && let Some(u) = page_link(&url, self.o.index)
                {
                    url = CowStr::from(u);
                }
                if !self.o.html && !safe_url(&url, false) {
                    url = CowStr::from("#");
                }
                Event::Start(Tag::Link {
                    link_type,
                    dest_url: url,
                    title,
                    id,
                })
            }
            Event::Start(Tag::Image {
                link_type,
                dest_url,
                title,
                id,
            }) if !self.o.html && !safe_url(&dest_url, true) => Event::Start(Tag::Image {
                link_type,
                dest_url: CowStr::from("#"),
                title,
                id,
            }),
            // With html off, raw HTML shows as text: a block of it as code.
            Event::Start(Tag::HtmlBlock) if !self.o.html => {
                Event::Start(Tag::CodeBlock(CodeBlockKind::Indented))
            }
            Event::End(TagEnd::HtmlBlock) if !self.o.html => Event::End(TagEnd::CodeBlock),
            Event::Html(h) | Event::InlineHtml(h) if !self.o.html => Event::Text(h),
            e => e,
        }
    }
}

/// Where a link to a Markdown file should point instead: guide.md becomes
/// guide, and a folder's index file (docs/index.md) the folder (docs/).
/// The part after ? or # stays. Links with a scheme, links to another host
/// (//host/x) and links within the page are left alone, and so is
/// anything that doesn't end in .md: None.
pub fn page_link(url: &str, index: &[String]) -> Option<String> {
    if url.starts_with("//") || url.starts_with('#') || scheme(url).is_some() {
        return None;
    }
    let end = url.find(['?', '#']).unwrap_or(url.len());
    let (path, rest) = url.split_at(end);
    let stem = path.strip_suffix(".md")?;
    let (dir, name) = match path.rfind('/') {
        Some(i) => (&path[..=i], &path[i + 1..]),
        None => ("", path),
    };
    if name.len() <= 3 {
        return None; // ".md" alone names no page
    }
    let page = if index.iter().any(|n| n == name) {
        if dir.is_empty() { "./" } else { dir }
    } else {
        stem
    };
    Some(format!("{page}{rest}"))
}

/// The scheme of a URL (http, mailto, javascript), lower case, ignoring
/// the spaces and control characters a browser ignores; None for a
/// relative URL.
fn scheme(url: &str) -> Option<String> {
    let clean: String = url
        .chars()
        .filter(|c| !c.is_ascii_whitespace() && !c.is_ascii_control())
        .collect();
    let i = clean.find(':')?;
    let s = &clean[..i];
    let ok = s.starts_with(|c: char| c.is_ascii_alphabetic())
        && s.chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '+' || c == '-' || c == '.');
    ok.then(|| s.to_ascii_lowercase())
}

/// Whether a link (or, with image set, a picture) is safe in a file
/// someone else wrote: relative, or http, https, mailto or tel, and for a
/// picture a data:image/ URL too.
fn safe_url(url: &str, image: bool) -> bool {
    match scheme(url).as_deref() {
        None | Some("http" | "https" | "mailto" | "tel") => true,
        Some("data") => {
            image
                && url
                    .trim_start()
                    .get(5..11)
                    .is_some_and(|t| t.eq_ignore_ascii_case("image/"))
        }
        _ => false,
    }
}

/// A heading's id, the way GitHub makes it: lower case, letters, digits,
/// - and _ kept, spaces made -, everything else dropped.
pub fn slug(text: &str) -> String {
    let mut s = String::new();
    for c in text.trim().chars().flat_map(char::to_lowercase) {
        if c.is_alphanumeric() || c == '_' || c == '-' {
            s.push(c);
        } else if c.is_whitespace() {
            s.push('-');
        }
    }
    s
}

/// The table of contents: the level-two headings, each with its level-three
/// ones under it.
fn toc(heads: &[(HeadingLevel, String, String)]) -> String {
    let items: Vec<_> = heads
        .iter()
        .filter(|h| matches!(h.0, HeadingLevel::H2 | HeadingLevel::H3))
        .collect();
    if items.len() < 2 {
        return String::new();
    }
    let mut s = String::from("<ul>\n");
    let (mut item, mut sub) = (false, false);
    for (level, id, text) in items {
        let link = format!("<a href=\"#{}\">{}</a>", escape(id), escape(text));
        if *level == HeadingLevel::H2 {
            if sub {
                s.push_str("</ul>\n");
                sub = false;
            }
            if item {
                s.push_str("</li>\n");
            }
            s.push_str("<li>");
            s.push_str(&link);
            item = true;
        } else {
            if !item {
                s.push_str("<li>");
                item = true;
            }
            if !sub {
                s.push_str("\n<ul>\n");
                sub = true;
            }
            s.push_str("<li>");
            s.push_str(&link);
            s.push_str("</li>\n");
        }
    }
    if sub {
        s.push_str("</ul>\n");
    }
    if item {
        s.push_str("</li>\n");
    }
    s.push_str("</ul>\n");
    s
}

/// The plain values of front matter: `key: value` lines between --- lines,
/// or `key = value` between +++ lines. Lists, nested keys and blocks of
/// text are skipped; quotes around a value go.
pub fn front_matter(text: &str, kind: Option<MetadataBlockKind>) -> Vec<(String, String)> {
    let sep = match kind {
        Some(MetadataBlockKind::PlusesStyle) => '=',
        _ => ':',
    };
    let mut out: Vec<(String, String)> = Vec::new();
    for line in text.lines() {
        if line.starts_with(|c: char| c.is_whitespace() || c == '-' || c == '#') {
            continue;
        }
        let Some((k, v)) = line.split_once(sep) else {
            continue;
        };
        let k = k.trim().to_ascii_lowercase();
        let v = v.trim();
        let plain_key = !k.is_empty()
            && k.chars()
                .all(|c| c.is_ascii_alphanumeric() || c == '_' || c == '-');
        if !plain_key || v.is_empty() || v.starts_with(['[', '{', '|', '>']) {
            continue;
        }
        if out.iter().any(|(m, _)| *m == k) {
            continue;
        }
        out.push((k, unquote(v)));
    }
    out
}

/// A value without its quotes, or without a comment when it has none.
fn unquote(v: &str) -> String {
    for q in ['"', '\''] {
        if v.len() >= 2
            && v.starts_with(q)
            && let Some(end) = v[1..].rfind(q)
        {
            let inner = &v[1..1 + end];
            return if q == '"' {
                inner.replace("\\\"", "\"").replace("\\\\", "\\")
            } else {
                inner.replace("''", "'")
            };
        }
    }
    match v.find(" #") {
        Some(i) => v[..i].trim_end().to_string(),
        None => v.to_string(),
    }
}

/// Runs of spaces, tabs and line ends made one space.
fn squash(s: &str) -> String {
    s.split_whitespace().collect::<Vec<_>>().join(" ")
}

/// Text cut to at most `max` characters, at a space when there's one.
fn cut(s: &str, max: usize) -> String {
    if s.chars().count() <= max {
        return s.to_string();
    }
    let end = s.char_indices().nth(max).map_or(s.len(), |(i, _)| i);
    let head = &s[..end];
    match head.rfind(' ') {
        Some(i) if i > max / 2 => head[..i].trim_end_matches([',', ';', ':', '-']).to_string(),
        _ => head.to_string(),
    }
}

/// Text made safe in HTML, in an element or an attribute.
pub fn escape(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    escape_into(&mut out, s);
    out
}

/// escape, appending to out.
pub fn escape_into(out: &mut String, s: &str) {
    for c in s.chars() {
        match c {
            '&' => out.push_str("&amp;"),
            '<' => out.push_str("&lt;"),
            '>' => out.push_str("&gt;"),
            '"' => out.push_str("&quot;"),
            '\'' => out.push_str("&#39;"),
            c => out.push(c),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn idx() -> Vec<String> {
        vec!["index.md".into(), "README.md".into()]
    }

    fn page(src: &str) -> Page {
        render(
            src,
            &Opts {
                html: true,
                links: true,
                index: &idx(),
            },
        )
    }

    #[test]
    fn renders_commonmark_and_github_extras() {
        let p = page(
            "# Guide\n\nSome *words* and `code`.\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n- [x] done\n- [ ] not\n\n~~gone~~ and a note[^n].\n\n> [!NOTE]\n> Look.\n\n```rust\nfn x() {}\n```\n\n[^n]: The note.\n",
        );
        let c = &p.content;
        assert!(c.contains("<h1 id=\"guide\">Guide</h1>"), "{c}");
        assert!(c.contains("<em>words</em>") && c.contains("<code>code</code>"));
        assert!(c.contains("<table>") && c.contains("<td>2</td>"));
        assert!(c.contains("type=\"checkbox\" checked=\"\""));
        assert!(c.contains("<del>gone</del>"));
        assert!(c.contains("footnote-reference") && c.contains("footnote-definition"));
        assert!(c.contains("class=\"markdown-alert-note\""));
        assert!(c.contains("<pre><code class=\"language-rust\">fn x() {}"));
        assert_eq!(p.title, "Guide");
        assert_eq!(p.description, "Some words and code.");
    }

    #[test]
    fn headings_get_unique_ids_and_a_table_of_contents() {
        let p = page(
            "# Title\n\n## Getting started\n\n### Install it\n\n## Getting started\n\n## Ünïcode & *more*!\n\n## Own {#mine}\n\n## \n\n#### Deep\n",
        );
        let c = &p.content;
        assert!(c.contains("<h2 id=\"getting-started\">"), "{c}");
        assert!(c.contains("<h3 id=\"install-it\">"));
        assert!(c.contains("<h2 id=\"getting-started-1\">"));
        assert!(c.contains("<h2 id=\"ünïcode--more\">"), "{c}");
        assert!(c.contains("<h2 id=\"mine\">Own</h2>"));
        assert!(c.contains("<h2 id=\"section\"></h2>"), "{c}");
        assert!(c.contains("<h4 id=\"deep\">"));
        assert_eq!(
            p.toc,
            "<ul>\n<li><a href=\"#getting-started\">Getting started</a>\n<ul>\n<li><a href=\"#install-it\">Install it</a></li>\n</ul>\n</li>\n<li><a href=\"#getting-started-1\">Getting started</a></li>\n<li><a href=\"#ünïcode--more\">Ünïcode &amp; more!</a></li>\n<li><a href=\"#mine\">Own</a></li>\n<li><a href=\"#section\"></a></li>\n</ul>\n"
        );
        assert_eq!(page("# Only\n\n## One\n").toc, "");
    }

    #[test]
    fn front_matter_gives_title_and_values() {
        let p = page(
            "---\ntitle: \"Setting up: the long way\"\ndescription: How it's done # really\ndate: 2026-10-14\ntags: [a, b]\nnested:\n  key: x\nauthor: 'Y''s'\n---\n\n# Heading\n\nText.\n",
        );
        assert_eq!(p.title, "Setting up: the long way");
        assert_eq!(p.description, "How it's done");
        assert_eq!(p.meta("date"), Some("2026-10-14"));
        assert_eq!(p.meta("author"), Some("Y's"));
        assert_eq!(p.meta("tags"), None);
        assert_eq!(p.meta("nested"), None);
        assert!(!p.content.contains("2026-10-14"), "{}", p.content);
        assert!(p.content.starts_with("<h1"), "{}", p.content);
        let t = page("+++\ntitle = \"Toml\"\nweight = 3\n+++\n\nBody.\n");
        assert_eq!(t.title, "Toml");
        assert_eq!(t.meta("weight"), Some("3"));
        assert_eq!(t.description, "Body.");
    }

    #[test]
    fn description_comes_from_the_first_paragraph_with_text() {
        let long = "word ".repeat(80);
        let p = page(&format!("![a picture](p.png)\n\n{long}\n\nSecond.\n"));
        assert!(p.description.len() <= 200 && p.description.ends_with("word"));
        assert!(!p.description.contains("picture"));
        assert_eq!(page("# T\n\n- a list\n").description, "");
    }

    #[test]
    fn links_to_markdown_files_point_to_their_pages() {
        let i = idx();
        let cases = [
            ("setup.md", Some("setup")),
            ("../guide/setup.md#install", Some("../guide/setup#install")),
            ("/docs/a.md?x=1", Some("/docs/a?x=1")),
            ("index.md", Some("./")),
            ("docs/README.md", Some("docs/")),
            ("docs/index.md#top", Some("docs/#top")),
            ("notes.md.txt", None),
            ("pic.png", None),
            (".md", None),
            ("https://example.com/a.md", None),
            ("//example.com/a.md", None),
            ("#x.md", None),
            ("mailto:a.md", None),
        ];
        for (url, want) in cases {
            assert_eq!(page_link(url, &i).as_deref(), want, "{url}");
        }
        let p = page("[s](setup.md) [r](README.md) ![p](pic.md)\n");
        assert!(p.content.contains("href=\"setup\""), "{}", p.content);
        assert!(p.content.contains("href=\"./\""));
        assert!(p.content.contains("src=\"pic.md\""), "pictures stay");
        let kept = render(
            "[s](setup.md)\n",
            &Opts {
                html: true,
                links: false,
                index: &i,
            },
        );
        assert!(kept.content.contains("href=\"setup.md\""));
    }

    #[test]
    fn html_off_shows_raw_html_as_text_and_drops_unsafe_links() {
        let src = "<script>alert(1)</script>\n\nHi <b onclick=\"x()\">there</b>.\n\n[a](javascript:alert(1)) [b](<JaVa\tScRiPt:x>) [c](https://ok.example/) [d](mailto:a@b.example) [e](rel/page) [f](data:text/html,x)\n\n![p](data:image/png;base64,AA==) ![q](data:text/html,x)\n";
        let i = idx();
        let off = render(
            src,
            &Opts {
                html: false,
                links: true,
                index: &i,
            },
        );
        let c = &off.content;
        assert!(
            !c.contains("<script>") && c.contains("&lt;script&gt;"),
            "{c}"
        );
        assert!(c.contains("<pre><code>&lt;script&gt;"), "{c}");
        assert!(!c.contains("<b ") && c.contains("&lt;b onclick="), "{c}");
        assert!(!c.to_lowercase().contains("javascript:"), "{c}");
        assert!(!c.contains("href=\"data:"), "{c}");
        assert!(
            c.contains("href=\"https://ok.example/\"") && c.contains("href=\"mailto:a@b.example\"")
        );
        assert!(c.contains("href=\"rel/page\""));
        assert!(c.contains("src=\"data:image/png;base64,AA==\""));
        assert!(!c.contains("src=\"data:text/html"), "{c}");
        let on = page(src);
        assert!(on.content.contains("<script>alert(1)</script>"));
        assert!(on.content.contains("<b onclick=\"x()\">"));
    }

    #[test]
    fn helpers() {
        assert_eq!(slug(" Hello,  World! "), "hello--world");
        assert_eq!(slug("a_b-c"), "a_b-c");
        assert_eq!(cut("one two three", 9), "one two");
        assert_eq!(cut("short", 9), "short");
        assert_eq!(cut("abcdefghijklmnop", 5), "abcde");
        assert_eq!(
            escape("<a href='x'>&\"</a>"),
            "&lt;a href=&#39;x&#39;&gt;&amp;&quot;&lt;/a&gt;"
        );
        assert_eq!(unquote("\"a \\\"b\\\"\""), "a \"b\"");
        assert_eq!(unquote("plain # comment"), "plain");
        assert_eq!(unquote("a#b"), "a#b");
    }
}
