// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! What visitors see: the banner, the settings page and their styles. None
//! of it needs JavaScript: the buttons are links and a form, so a choice
//! works in any browser, and the styles come from the endpoint as a file,
//! so a Content-Security-Policy that forbids inline styles still allows
//! them.

use crate::config::{Config, fnv};
use kit::html_escape as esc;

/// The banner's and the settings page's styles.
pub const CSS: &str = include_str!("consent.css");

/// The styles' version, in their URL, so a browser can keep them a year.
pub fn css_version() -> String {
    format!("{:08x}", fnv(CSS.as_bytes()) as u32)
}

impl Config {
    fn css_link(&self) -> String {
        format!(
            "<link rel=\"stylesheet\" href=\"{}/consent.css?v={}\">",
            esc(&self.endpoint),
            css_version()
        )
    }

    fn privacy_link(&self) -> String {
        match &self.texts.privacy {
            Some((url, words)) => format!(" <a href=\"{}\">{}</a>", esc(url), esc(words)),
            None => String::new(),
        }
    }

    /// The banner for a page: `back` is its path and query, where a choice
    /// returns to.
    pub fn banner(&self, back: &str) -> String {
        let e = esc(&self.endpoint);
        let r = esc(&kit::percent_encode(&kit::safe_return(back)));
        let t = &self.texts;
        format!(
            "<div id=\"bp-consent\" role=\"region\" aria-label=\"{title}\" lang=\"{lang}\">{css}<p>{banner}{privacy}</p><p class=\"bp-consent-actions\"><a class=\"bp-consent-button\" href=\"{e}?accept=all&amp;r={r}\" rel=\"nofollow\">{accept}</a> <a class=\"bp-consent-button\" href=\"{e}?reject=all&amp;r={r}\" rel=\"nofollow\">{reject}</a> <a class=\"bp-consent-button\" href=\"{e}?r={r}\" rel=\"nofollow\">{choose}</a></p></div>\n",
            title = esc(&t.title),
            lang = esc(&self.lang),
            css = self.css_link(),
            banner = esc(&t.banner),
            privacy = self.privacy_link(),
            accept = esc(&t.accept),
            reject = esc(&t.reject),
            choose = esc(&t.choose),
        )
    }

    /// The settings page: each category with a box to tick, ticked when
    /// it's accepted now. With gpc, a line says why they start unticked.
    pub fn settings(&self, accepted: &[String], gpc: bool, back: &str) -> String {
        let t = &self.texts;
        let back = kit::safe_return(back);
        let (nlabel, nabout) = t
            .necessary
            .split_once(": ")
            .unwrap_or(("Necessary", t.necessary.as_str()));
        let mut items = format!(
            "<li><label><input type=\"checkbox\" checked disabled> {}</label><span>{}</span></li>\n",
            esc(nlabel),
            esc(nabout)
        );
        for c in &self.categories {
            items.push_str(&format!(
                "<li><label><input type=\"checkbox\" name=\"set\" value=\"{}\"{}> {}</label><span>{}</span></li>\n",
                esc(&c.name),
                if accepted.contains(&c.name) { " checked" } else { "" },
                esc(&c.label),
                esc(&c.about)
            ));
        }
        let note = if gpc {
            format!("<p class=\"bp-consent-note\">{}</p>\n", esc(&t.gpc_note))
        } else {
            String::new()
        };
        format!(
            "<!doctype html>
<html lang=\"{lang}\">
<head>
<meta charset=\"utf-8\">
<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">
<meta name=\"robots\" content=\"noindex\">
<title>{title}</title>
{css}
</head>
<body class=\"bp-consent-page\">
<main>
<h1>{title}</h1>
<p>{banner}</p>
{note}<form method=\"get\" action=\"{e}\">
<input type=\"hidden\" name=\"r\" value=\"{back}\">
<ul class=\"bp-consent-list\">
{items}</ul>
<p class=\"bp-consent-actions\"><button class=\"bp-consent-button\" name=\"save\" value=\"1\">{save}</button> <button class=\"bp-consent-button\" name=\"accept\" value=\"all\">{accept}</button> <button class=\"bp-consent-button\" name=\"reject\" value=\"all\">{reject}</button></p>
</form>
<p><a href=\"{back}\">{back_text}</a>{privacy}</p>
</main>
</body>
</html>
",
            lang = esc(&self.lang),
            title = esc(&t.title),
            css = self.css_link(),
            banner = esc(&t.banner),
            e = esc(&self.endpoint),
            back = esc(&back),
            save = esc(&t.save),
            accept = esc(&t.accept),
            reject = esc(&t.reject),
            back_text = esc(&t.back),
            privacy = self.privacy_link().replacen(' ', " · ", 1),
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_banner() {
        let c =
            Config::parse("banner Cookies <b>here</b>?\nprivacy /privacy/\naccept Ja\n").unwrap();
        let b = c.banner("/a b?x=1&y=\"2\"");
        assert!(
            b.starts_with(
                "<div id=\"bp-consent\" role=\"region\" aria-label=\"Cookie settings\" lang=\"en\">"
            ),
            "{b}"
        );
        assert!(b.contains(&format!(
            "<link rel=\"stylesheet\" href=\"/.bareproxy-consent/consent.css?v={}\">",
            css_version()
        )));
        assert!(
            b.contains(
                "<p>Cookies &lt;b&gt;here&lt;/b&gt;? <a href=\"/privacy/\">Privacy policy</a></p>"
            ),
            "{b}"
        );
        assert!(b.contains("href=\"/.bareproxy-consent?accept=all&amp;r=/a%20b%3Fx%3D1%26y%3D%222%22\" rel=\"nofollow\">Ja</a>"), "{b}");
        assert!(b.contains("href=\"/.bareproxy-consent?reject=all&amp;r=/a%20b%3Fx%3D1%26y%3D%222%22\" rel=\"nofollow\">Reject all</a>"));
        assert!(b.contains("href=\"/.bareproxy-consent?r=/a%20b%3Fx%3D1%26y%3D%222%22\" rel=\"nofollow\">Choose</a>"));
        assert!(!b.contains("<script"), "no JavaScript");
        assert!(
            c.banner("https://evil.example/").contains("r=/\""),
            "only paths on the site"
        );
    }

    #[test]
    fn the_settings_page() {
        let c = Config::default();
        let p = c.settings(&["marketing".into()], false, "/docs/?q=<x>");
        assert!(p.contains("<input type=\"checkbox\" checked disabled> Necessary</label><span>needed for the site to work, so they&#39;re always on.</span>"), "{p}");
        assert!(p.contains(
            "<input type=\"checkbox\" name=\"set\" value=\"analytics\"> Analytics</label>"
        ));
        assert!(p.contains("<input type=\"checkbox\" name=\"set\" value=\"marketing\" checked> Marketing</label><span>to show ads and measure them</span>"));
        assert!(p.contains("<input type=\"hidden\" name=\"r\" value=\"/docs/?q=&lt;x&gt;\">"));
        assert!(p.contains(
            "<button class=\"bp-consent-button\" name=\"save\" value=\"1\">Save my choices</button>"
        ));
        assert!(p.contains("<a href=\"/docs/?q=&lt;x&gt;\">Back to the site</a>"));
        assert!(!p.contains("bp-consent-note"));
        assert!(!p.contains("<script"));
        let g = c.settings(&[], true, "/");
        assert!(
            g.contains("<p class=\"bp-consent-note\">Your browser asks sites not to track you")
        );
        assert!(!g.contains(" checked>"));
        assert_eq!(css_version().len(), 8);
        assert!(CSS.contains("#bp-consent{color-scheme:light;position:fixed"));
    }
}
