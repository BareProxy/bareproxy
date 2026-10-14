// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! The cookie consent plugin's config, and the rules that use it: what a
//! visitor chose, which cookies wait for consent, what the endpoint does
//! with a choice.

use kit::{Item, Line};

/// A category of cookies that need consent, such as analytics.
#[derive(Debug, Clone, PartialEq)]
pub struct Category {
    /// Its name in the config, the consent cookie and X-Consent.
    pub name: String,
    /// What visitors see: "Analytics".
    pub label: String,
    /// What it's for, on the settings page.
    pub about: String,
    /// Cookie names, exact or ending in * for a prefix.
    pub cookies: Vec<String>,
    /// Text in a script's or an iframe's src, or in an inline script, that
    /// keeps it out of the page until consent. Lower case.
    pub block: Vec<String>,
}

impl Category {
    /// Whether a cookie belongs to the category.
    pub fn has_cookie(&self, name: &str) -> bool {
        self.cookies.iter().any(|p| match p.strip_suffix('*') {
            Some(prefix) => name.starts_with(prefix),
            None => name == p,
        })
    }
}

/// What visitors read: the banner's and the settings page's words.
#[derive(Debug, Clone, PartialEq)]
pub struct Texts {
    pub banner: String,
    pub accept: String,
    pub reject: String,
    pub choose: String,
    pub save: String,
    pub back: String,
    pub title: String,
    pub necessary: String,
    pub gpc_note: String,
    /// A link to the privacy policy, and its words.
    pub privacy: Option<(String, String)>,
}

impl Default for Texts {
    fn default() -> Texts {
        Texts {
            banner: "This site would like to use cookies beyond the ones it needs to work. You can accept them, refuse them, or choose.".into(),
            accept: "Accept all".into(),
            reject: "Reject all".into(),
            choose: "Choose".into(),
            save: "Save my choices".into(),
            back: "Back to the site".into(),
            title: "Cookie settings".into(),
            necessary: "Necessary: needed for the site to work, so they're always on.".into(),
            gpc_note: "Your browser asks sites not to track you (Global Privacy Control), so these stay off unless you turn them on here.".into(),
            privacy: None,
        }
    }
}

#[derive(Debug, Clone, PartialEq)]
pub struct Config {
    pub categories: Vec<Category>,
    pub texts: Texts,
    pub lang: String,
    /// How long a choice lasts, in seconds.
    pub remember: u64,
    /// A browser's Global Privacy Control counts as no.
    pub gpc: bool,
    /// The request header that tells the app what was accepted (lower
    /// case), or None.
    pub header: Option<String>,
    pub cookie: String,
    pub endpoint: String,
    /// Path prefixes that get no banner and no changes to their pages.
    pub skip: Vec<String>,
    /// Changes when the categories do, so visitors are asked again.
    pub rev: String,
}

/// The settings, for a note about an unknown one.
const SETTINGS: &str = "category, cookies, block, banner, accept, reject, choose, save, back, title, necessary, gpc-note, privacy, lang, remember, gpc, header, cookie, endpoint and skip";

/// The categories used when the config names none: the analytics and
/// marketing tools most sites use, by their cookies and their scripts.
pub fn default_categories() -> Vec<Category> {
    let cat = |name: &str, label: &str, about: &str, cookies: &[&str], block: &[&str]| Category {
        name: name.into(),
        label: label.into(),
        about: about.into(),
        cookies: cookies.iter().map(|s| s.to_string()).collect(),
        block: block.iter().map(|s| s.to_string()).collect(),
    };
    vec![
        cat(
            "analytics",
            "Analytics",
            "to count visits and see which pages people read",
            &[
                "_ga",
                "_ga_*",
                "_gid",
                "_gat",
                "_gat_*",
                "_hjid",
                "_hjSession*",
                "_hjFirstSeen",
                "_clck",
                "_clsk",
                "_pk_id.*",
                "_pk_ses.*",
                "ajs_anonymous_id",
                "ajs_user_id",
                "mp_*",
            ],
            &[
                "googletagmanager.com",
                "google-analytics.com",
                "static.hotjar.com",
                "clarity.ms",
                "cdn.segment.com",
                "cdn.mxpnl.com",
                "matomo.js",
                "piwik.js",
            ],
        ),
        cat(
            "marketing",
            "Marketing",
            "to show ads and measure them",
            &[
                "_fbp",
                "_fbc",
                "_gcl_au",
                "_gcl_aw",
                "_gcl_dc",
                "_uetsid",
                "_uetvid",
                "_ttp",
                "_tt_enable_cookie",
                "li_fat_id",
                "_pin_unauth",
                "_rdt_uuid",
            ],
            &[
                "connect.facebook.net",
                "googleadservices.com",
                "googlesyndication.com",
                "doubleclick.net",
                "analytics.tiktok.com",
                "snap.licdn.com",
                "bat.bing.com",
                "s.pinimg.com",
                "redditstatic.com/ads",
            ],
        ),
    ]
}

impl Default for Config {
    fn default() -> Config {
        let categories = default_categories();
        Config {
            rev: rev(&categories),
            categories,
            texts: Texts::default(),
            lang: "en".into(),
            remember: 180 * 86400,
            gpc: true,
            header: Some("x-consent".into()),
            cookie: "bp_consent".into(),
            endpoint: "/.bareproxy-consent".into(),
            skip: vec![],
        }
    }
}

/// A cookie name: the characters RFC 6265 allows, and not empty.
fn cookie_name_ok(s: &str) -> bool {
    !s.is_empty()
        && s.bytes()
            .all(|b| b.is_ascii_graphic() && !b"()<>@,;:\\\"/[]?={}".contains(&b))
}

/// Text from the rest of a line, the words joined by single spaces.
fn text(l: &Line) -> Result<String, String> {
    if l.args().is_empty() {
        return Err(l.err(format!("{} takes some words", l.name())));
    }
    Ok(l.args().join(" "))
}

impl Config {
    pub fn parse(text_in: &str) -> Result<Config, String> {
        let items = kit::parse(text_in, &[])?;
        let lines: Vec<Line> = items
            .into_iter()
            .filter_map(|i| match i {
                Item::Line(l) => Some(l),
                Item::Block(..) => None,
            })
            .collect();
        let mut c = Config::default();
        // The built-in categories stay only when the config names none of
        // its own; cookies and block lines may add to them.
        if lines.iter().any(|l| l.name() == "category") {
            c.categories.clear();
        }
        let mut seen: Vec<(String, usize)> = vec![];
        for l in &lines {
            let name = l.name().to_string();
            let repeats = ["category", "cookies", "block", "skip"].contains(&name.as_str());
            if !repeats {
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
                "category" => {
                    let Some((cname, label)) = l.args().split_first() else {
                        return Err(l.err(
                            "category takes a name and what visitors see, such as category analytics Analytics: to count visits",
                        ));
                    };
                    let ok = !cname.is_empty()
                        && cname.len() <= 32
                        && cname.bytes().all(|b| {
                            b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'_' || b == b'-'
                        });
                    if !ok {
                        return Err(l.err(format!(
                            "{cname} isn't a category name: use lower-case letters, digits, - and _"
                        )));
                    }
                    if label.is_empty() {
                        return Err(l.err(format!(
                            "category {cname} needs what visitors see after its name"
                        )));
                    }
                    if c.categories.iter().any(|x| x.name == *cname) {
                        return Err(l.err(format!("category {cname} is there already")));
                    }
                    let words = label.join(" ");
                    let (label, about) = match words.split_once(": ") {
                        Some((a, b)) => (a.to_string(), b.to_string()),
                        None => (words, String::new()),
                    };
                    c.categories.push(Category {
                        name: cname.clone(),
                        label,
                        about,
                        cookies: vec![],
                        block: vec![],
                    });
                }
                "cookies" | "block" => {
                    let Some((cname, rest)) = l.args().split_first() else {
                        return Err(l.err(format!("{name} takes a category and one or more names")));
                    };
                    let Some(cat) = c.categories.iter_mut().find(|x| x.name == *cname) else {
                        return Err(l.err(format!(
                            "{name} names category {cname}, which no category line before it defines"
                        )));
                    };
                    if rest.is_empty() {
                        return Err(l.err(format!(
                            "{name} {cname} takes one or more names after the category"
                        )));
                    }
                    for w in rest {
                        if name == "cookies" {
                            let base = w.strip_suffix('*').unwrap_or(w);
                            if !cookie_name_ok(base) || base.contains('*') {
                                return Err(l.err(format!(
                                    "{w} isn't a cookie name, or a name ending in * for all that start with it"
                                )));
                            }
                            cat.cookies.push(w.clone());
                        } else {
                            if w.len() < 4 {
                                return Err(l.err(format!(
                                    "{w} is too short to block by: write at least 4 characters, such as a host name"
                                )));
                            }
                            cat.block.push(w.to_ascii_lowercase());
                        }
                    }
                }
                "banner" => c.texts.banner = text(l)?,
                "accept" => c.texts.accept = text(l)?,
                "reject" => c.texts.reject = text(l)?,
                "choose" => c.texts.choose = text(l)?,
                "save" => c.texts.save = text(l)?,
                "back" => c.texts.back = text(l)?,
                "title" => c.texts.title = text(l)?,
                "necessary" => c.texts.necessary = text(l)?,
                "gpc-note" => c.texts.gpc_note = text(l)?,
                "privacy" => {
                    let Some((url, words)) = l.args().split_first() else {
                        return Err(l.err(
                            "privacy takes a link to the privacy policy, such as privacy /privacy/",
                        ));
                    };
                    let ok = url.starts_with('/') && !url.starts_with("//")
                        || url.starts_with("https://")
                        || url.starts_with("http://");
                    if !ok {
                        return Err(l.err(format!("{url} isn't a path or an http(s) link")));
                    }
                    let words = if words.is_empty() {
                        "Privacy policy".to_string()
                    } else {
                        words.join(" ")
                    };
                    c.texts.privacy = Some((url.clone(), words));
                }
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
                "remember" => {
                    let s = kit::seconds(l, &one()?)?;
                    if !(86400..=400 * 86400).contains(&s) {
                        return Err(l.err("remember is from 1d to 400d"));
                    }
                    c.remember = s;
                }
                "gpc" => c.gpc = kit::switch(l)?,
                "header" => {
                    let v = one()?;
                    if v == "off" {
                        c.header = None;
                    } else if v.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'-')
                        && !v.is_empty()
                    {
                        c.header = Some(v.to_ascii_lowercase());
                    } else {
                        return Err(l.err(format!(
                            "{v} isn't a header name; write one such as X-Consent, or off"
                        )));
                    }
                }
                "cookie" => {
                    let v = one()?;
                    if !cookie_name_ok(&v) {
                        return Err(l.err(format!("{v} isn't a cookie name")));
                    }
                    c.cookie = v;
                }
                "endpoint" => {
                    let v = one()?;
                    if !v.starts_with('/')
                        || v.len() < 2
                        || v.ends_with('/')
                        || v.contains(['?', '#'])
                    {
                        return Err(l.err("endpoint takes a path such as /.bareproxy-consent"));
                    }
                    c.endpoint = v;
                }
                "skip" => {
                    if l.args().is_empty() || l.args().iter().any(|p| !p.starts_with('/')) {
                        return Err(l.err("skip takes paths that start with /, such as skip /api/"));
                    }
                    c.skip.extend_from_slice(l.args());
                }
                _ => {
                    return Err(l.err(format!(
                        "unknown setting {name}; the settings are {SETTINGS}"
                    )));
                }
            }
        }
        if c.categories.is_empty() {
            return Err("there are no categories of cookies to ask about".into());
        }
        if let Some(cat) = c.categories.iter().find(|x| x.has_cookie(&c.cookie)) {
            return Err(format!(
                "category {} takes in {}, the consent cookie itself; leave it out",
                cat.name, c.cookie
            ));
        }
        c.rev = rev(&c.categories);
        Ok(c)
    }

    /// What a visitor chose, from the consent cookie, or Global Privacy
    /// Control when there's no choice yet.
    pub fn choice(&self, cookie_header: &str, gpc: bool) -> Choice {
        if let Some(v) = kit::cookie_value(cookie_header, &self.cookie)
            && let Some(set) = self.read_cookie(v)
        {
            return Choice::Chosen(set);
        }
        if self.gpc && gpc {
            return Choice::Gpc;
        }
        Choice::Undecided
    }

    /// The categories in a consent cookie's value, or None when it's from
    /// another set of categories (or broken).
    pub fn read_cookie(&self, v: &str) -> Option<Vec<String>> {
        let (r, cats) = v.split_once('.')?;
        if r != self.rev {
            return None;
        }
        if cats == "none" {
            return Some(vec![]);
        }
        Some(
            self.categories
                .iter()
                .filter(|c| cats.split('+').any(|x| x == c.name))
                .map(|c| c.name.clone())
                .collect(),
        )
    }

    /// The consent cookie's value for the categories accepted.
    pub fn cookie_value(&self, accepted: &[String]) -> String {
        let names: Vec<&str> = self
            .categories
            .iter()
            .filter(|c| accepted.contains(&c.name))
            .map(|c| c.name.as_str())
            .collect();
        if names.is_empty() {
            format!("{}.none", self.rev)
        } else {
            format!("{}.{}", self.rev, names.join("+"))
        }
    }

    /// The Set-Cookie value for a choice. It isn't HttpOnly, so the site's
    /// own scripts can read it.
    pub fn set_cookie(&self, accepted: &[String], https: bool) -> String {
        format!(
            "{}={}; Path=/; Max-Age={}; SameSite=Lax{}",
            self.cookie,
            self.cookie_value(accepted),
            self.remember,
            if https { "; Secure" } else { "" }
        )
    }

    /// The category a cookie belongs to, if any.
    pub fn category_of(&self, cookie: &str) -> Option<&Category> {
        if cookie == self.cookie {
            return None;
        }
        self.categories.iter().find(|c| c.has_cookie(cookie))
    }

    /// The categories a choice doesn't accept.
    pub fn refused<'a>(&'a self, choice: &Choice) -> Vec<&'a Category> {
        let accepted = choice.accepted();
        self.categories
            .iter()
            .filter(|c| !accepted.contains(&c.name))
            .collect()
    }

    /// Whether a Set-Cookie header sets a cookie the choice doesn't allow
    /// yet: the cookie's category, or None when it may go out.
    pub fn held(&self, set_cookie: &str, choice: &Choice) -> Option<(String, String)> {
        let name = set_cookie.split(';').next()?.split_once('=')?.0.trim();
        let cat = self.category_of(name)?;
        (!choice.accepted().contains(&cat.name)).then(|| (name.to_string(), cat.name.clone()))
    }

    /// A Cookie header without the cookies of categories the choice
    /// doesn't accept, and the names it left out.
    pub fn strip_cookies(&self, header: &str, choice: &Choice) -> (String, Vec<String>) {
        let accepted = choice.accepted();
        let mut kept = vec![];
        let mut gone = vec![];
        for kv in header.split(';') {
            let kv = kv.trim();
            if kv.is_empty() {
                continue;
            }
            let name = kv.split_once('=').map_or(kv, |(k, _)| k.trim());
            match self.category_of(name) {
                Some(cat) if !accepted.contains(&cat.name) => gone.push(name.to_string()),
                _ => kept.push(kv),
            }
        }
        (kept.join("; "), gone)
    }

    /// Set-Cookie values that delete, in the browser, the cookies a Cookie
    /// header shows from categories that aren't accepted: for the host,
    /// and for each domain above it, as tracking cookies are often set
    /// for the whole site (Domain=.example.com).
    pub fn expiries(&self, header: &str, accepted: &[String], host: &str) -> Vec<String> {
        let mut out = vec![];
        let mut done: Vec<&str> = vec![];
        let labels: Vec<&str> = host.split('.').collect();
        for kv in header.split(';') {
            let name = kv
                .trim()
                .split_once('=')
                .map_or(kv.trim(), |(k, _)| k.trim());
            let Some(cat) = self.category_of(name) else {
                continue;
            };
            if accepted.contains(&cat.name) || done.contains(&name) {
                continue;
            }
            done.push(name);
            out.push(format!("{name}=; Path=/; Max-Age=0"));
            for i in 0..labels.len().saturating_sub(1) {
                out.push(format!(
                    "{name}=; Path=/; Domain={}; Max-Age=0",
                    labels[i..].join(".")
                ));
            }
        }
        out
    }

    /// What the endpoint does with a query: a choice to keep, or the
    /// settings page. Accepting counts only from the site's own pages
    /// (same_site), so another site can't accept on a visitor's behalf;
    /// refusing counts from anywhere.
    pub fn decide(&self, query: &str, same_site: bool) -> Decision {
        let has = |k: &str| {
            query
                .split('&')
                .any(|kv| kv == k || kv.starts_with(&format!("{k}=")))
        };
        if has("reject") {
            return Decision::Keep(vec![]);
        }
        if has("accept") {
            return if same_site {
                Decision::Keep(self.categories.iter().map(|c| c.name.clone()).collect())
            } else {
                Decision::Show(true)
            };
        }
        if has("save") {
            if !same_site {
                return Decision::Show(true);
            }
            let set = kit::query_params(query, "set");
            return Decision::Keep(
                self.categories
                    .iter()
                    .filter(|c| set.contains(&c.name))
                    .map(|c| c.name.clone())
                    .collect(),
            );
        }
        Decision::Show(false)
    }

    /// The value of the header that tells the app what was accepted.
    pub fn header_value(&self, choice: &Choice) -> String {
        match choice {
            Choice::Undecided => "unknown".into(),
            Choice::Gpc => "none".into(),
            Choice::Chosen(set) if set.is_empty() => "none".into(),
            Choice::Chosen(set) => set.join(","),
        }
    }

    pub fn skipped(&self, path: &str) -> bool {
        self.skip.iter().any(|p| kit::under(path, p))
    }

    /// Labels for a note: "analytics and marketing".
    pub fn names(list: &[String]) -> String {
        match list {
            [] => "nothing".into(),
            [one] => one.clone(),
            [rest @ .., last] => format!("{} and {last}", rest.join(", ")),
        }
    }
}

/// Whether a request loads a page in a browser, the kind that gets the
/// banner: a GET whose Sec-Fetch-Dest says document (not an iframe), or,
/// from a browser that doesn't send it, that asks for HTML; and from a
/// client that says it's a browser, not a bot.
pub fn is_page(
    method: &str,
    dest: Option<&str>,
    mode: Option<&str>,
    accept: Option<&str>,
    ua: &str,
) -> bool {
    let doc = match dest {
        Some(d) => d.eq_ignore_ascii_case("document"),
        None => kit::is_navigation(mode, accept),
    };
    method == "GET" && doc && kit::looks_like_browser(ua)
}

/// Whether a request to the endpoint comes from the site's own pages (or
/// from the visitor's own typing, a bookmark): Sec-Fetch-Site says so, or
/// for a browser without it, the Referer is on the same host. With
/// neither, it counts as the site's.
pub fn same_site(sec_fetch_site: Option<&str>, referer: Option<&str>, authority: &str) -> bool {
    match (sec_fetch_site, referer) {
        (Some(s), _) => s.eq_ignore_ascii_case("same-origin") || s.eq_ignore_ascii_case("none"),
        (None, Some(r)) => referer_split(r).is_some_and(|(a, _)| a.eq_ignore_ascii_case(authority)),
        (None, None) => true,
    }
}

/// A Referer's host (with its port) and path.
fn referer_split(r: &str) -> Option<(&str, &str)> {
    let rest = r
        .strip_prefix("https://")
        .or_else(|| r.strip_prefix("http://"))?;
    let i = rest.find(['/', '?', '#']).unwrap_or(rest.len());
    let path = &rest[i..];
    let path = path.split('#').next().unwrap_or("");
    Some((&rest[..i], if path.is_empty() { "/" } else { path }))
}

/// Where a choice returns to: the r parameter, or the page it came from on
/// this host, or the front page.
pub fn return_to(query: &str, referer: Option<&str>, authority: &str) -> String {
    if let Some(r) = kit::query_param(query, "r") {
        return kit::safe_return(&r);
    }
    match referer.and_then(referer_split) {
        Some((a, p)) if a.eq_ignore_ascii_case(authority) => kit::safe_return(p),
        _ => "/".into(),
    }
}

/// What a visitor chose.
#[derive(Debug, Clone, PartialEq)]
pub enum Choice {
    /// No choice yet: the banner shows, and nothing is accepted.
    Undecided,
    /// No choice, and the browser sends Global Privacy Control: no banner,
    /// and nothing is accepted.
    Gpc,
    /// The categories accepted (perhaps none).
    Chosen(Vec<String>),
}

impl Choice {
    pub fn accepted(&self) -> &[String] {
        match self {
            Choice::Chosen(set) => set,
            _ => &[],
        }
    }
}

/// What the endpoint does.
#[derive(Debug, PartialEq)]
pub enum Decision {
    /// Keeps a choice: these categories accepted.
    Keep(Vec<String>),
    /// Shows the settings page; true when it shows instead of an accept
    /// that came from another site.
    Show(bool),
}

/// A short hash of the category names: a choice made for other categories
/// asks again.
pub fn rev(cats: &[Category]) -> String {
    let names: Vec<&str> = cats.iter().map(|c| c.name.as_str()).collect();
    format!("{:06x}", fnv(names.join(",").as_bytes()) & 0xff_ffff)
}

/// FNV-1a, 64 bits, for short names of things, never for secrets.
pub fn fnv(bytes: &[u8]) -> u64 {
    let mut h: u64 = 0xcbf29ce484222325;
    for &b in bytes {
        h ^= b as u64;
        h = h.wrapping_mul(0x100000001b3);
    }
    h
}

#[cfg(test)]
mod tests {
    use super::*;

    const CONF: &str = "# consent
category analytics Analytics: to count visits
cookies analytics _ga _ga_* _gid
block analytics googletagmanager.com
category marketing Marketing
cookies marketing _fbp
block marketing Connect.Facebook.net
category media Videos: YouTube and Vimeo
block media youtube.com player.vimeo.com
banner We use cookies for statistics and ads.
privacy /privacy/ How we use data
accept Yes
reject No
lang de
remember 30d
gpc off
header X-Cookie-Choice
cookie choice
endpoint /cookies
skip /api/
skip /feed
";

    #[test]
    fn parses_a_config() {
        let c = Config::parse(CONF).unwrap();
        let names: Vec<&str> = c.categories.iter().map(|x| x.name.as_str()).collect();
        assert_eq!(names, ["analytics", "marketing", "media"]);
        let a = &c.categories[0];
        assert_eq!(
            (a.label.as_str(), a.about.as_str()),
            ("Analytics", "to count visits")
        );
        assert_eq!(a.cookies, ["_ga", "_ga_*", "_gid"]);
        assert_eq!(c.categories[1].block, ["connect.facebook.net"]);
        assert_eq!(c.categories[2].label, "Videos");
        assert_eq!(c.categories[2].about, "YouTube and Vimeo");
        assert_eq!(c.texts.banner, "We use cookies for statistics and ads.");
        assert_eq!(
            c.texts.privacy,
            Some(("/privacy/".into(), "How we use data".into()))
        );
        assert_eq!(
            (c.texts.accept.as_str(), c.texts.reject.as_str()),
            ("Yes", "No")
        );
        assert_eq!(c.texts.choose, "Choose");
        assert_eq!(
            (c.lang.as_str(), c.remember, c.gpc),
            ("de", 30 * 86400, false)
        );
        assert_eq!(c.header.as_deref(), Some("x-cookie-choice"));
        assert_eq!(
            (c.cookie.as_str(), c.endpoint.as_str()),
            ("choice", "/cookies")
        );
        assert_eq!(c.skip, ["/api/", "/feed"]);
        assert_eq!(c.rev.len(), 6);
        assert!(c.skipped("/api/x") && c.skipped("/feed") && !c.skipped("/feeds"));
    }

    #[test]
    fn built_in_categories_unless_the_config_names_its_own() {
        let d = Config::parse("cookies analytics _mine_*\nheader off\n").unwrap();
        assert_eq!(d.categories.len(), 2);
        assert!(d.categories[0].has_cookie("_ga_ABC123") && d.categories[0].has_cookie("_mine_x"));
        assert!(d.categories[1].has_cookie("_fbp") && !d.categories[1].has_cookie("_fbpx"));
        assert_eq!(d.header, None);
        assert_eq!(
            d.rev,
            Config::default().rev,
            "the same categories, the same rev"
        );
        let own = Config::parse("category stats Stats\n").unwrap();
        assert_eq!(own.categories.len(), 1);
        assert_ne!(own.rev, d.rev);
    }

    #[test]
    fn refuses_bad_lines() {
        for (text, want) in [
            ("category", "line 1: category takes a name"),
            (
                "category Analytics x",
                "line 1: Analytics isn't a category name",
            ),
            (
                "category stats",
                "line 1: category stats needs what visitors see",
            ),
            (
                "category a A\ncategory a B",
                "line 2: category a is there already",
            ),
            (
                "cookies ads _x",
                "line 1: cookies names category ads, which no category line before it defines",
            ),
            (
                "cookies analytics",
                "line 1: cookies analytics takes one or more names",
            ),
            ("cookies analytics a*b", "line 1: a*b isn't a cookie name"),
            ("cookies analytics a;b", "line 1: a;b isn't a cookie name"),
            (
                "block analytics .io",
                "line 1: .io is too short to block by",
            ),
            ("banner", "line 1: banner takes some words"),
            (
                "privacy javascript:x",
                "line 1: javascript:x isn't a path or an http(s) link",
            ),
            ("remember 2h", "line 1: remember is from 1d to 400d"),
            ("gpc maybe", "line 1: gpc takes on or off"),
            ("header X Y", "line 1: header takes one value"),
            ("header X_Y", "line 1: X_Y isn't a header name"),
            ("endpoint /x/", "line 1: endpoint takes a path"),
            ("skip api", "line 1: skip takes paths that start with /"),
            ("lang en\nlang fr", "line 2: lang is set already, on line 1"),
            (
                "color red",
                "line 1: unknown setting color; the settings are category,",
            ),
            (
                "cookies analytics bp_consent",
                "category analytics takes in bp_consent, the consent cookie itself",
            ),
        ] {
            let e = Config::parse(text).unwrap_err();
            assert!(e.starts_with(want), "{text}: {e}");
        }
    }

    #[test]
    fn choices_from_the_cookie_and_gpc() {
        let c = Config::default();
        let v = c.cookie_value(&["marketing".into(), "analytics".into()]);
        assert_eq!(
            v,
            format!("{}.analytics+marketing", c.rev),
            "in the config's order"
        );
        let header = format!("a=1; bp_consent={v}; b=2");
        assert_eq!(
            c.choice(&header, true),
            Choice::Chosen(vec!["analytics".into(), "marketing".into()])
        );
        let none = format!("bp_consent={}", c.cookie_value(&[]));
        assert_eq!(c.choice(&none, false), Choice::Chosen(vec![]));
        assert_eq!(
            c.choice("bp_consent=0000aa.analytics", false),
            Choice::Undecided,
            "another rev"
        );
        assert_eq!(c.choice("bp_consent=garbage", true), Choice::Gpc);
        assert_eq!(c.choice("", true), Choice::Gpc);
        assert_eq!(c.choice("", false), Choice::Undecided);
        let mut off = c.clone();
        off.gpc = false;
        assert_eq!(off.choice("", true), Choice::Undecided);
        assert_eq!(
            c.choice(&format!("bp_consent={}.analytics+nonsense", c.rev), false),
            Choice::Chosen(vec!["analytics".into()])
        );
        assert_eq!(
            c.set_cookie(&["analytics".into()], true),
            format!(
                "bp_consent={}.analytics; Path=/; Max-Age=15552000; SameSite=Lax; Secure",
                c.rev
            )
        );
        assert_eq!(c.header_value(&Choice::Undecided), "unknown");
        assert_eq!(c.header_value(&Choice::Gpc), "none");
        assert_eq!(
            c.header_value(&Choice::Chosen(vec![
                "analytics".into(),
                "marketing".into()
            ])),
            "analytics,marketing"
        );
    }

    #[test]
    fn cookies_held_back_stripped_and_deleted() {
        let c = Config::default();
        let undecided = Choice::Undecided;
        let analytics = Choice::Chosen(vec!["analytics".into()]);
        assert_eq!(
            c.held("_ga=GA1.1.2; Path=/; Max-Age=63072000", &undecided),
            Some(("_ga".into(), "analytics".into()))
        );
        assert_eq!(c.held("_ga=GA1; Path=/", &analytics), None);
        assert_eq!(
            c.held("_fbp=fb.1; Domain=.example.com", &analytics),
            Some(("_fbp".into(), "marketing".into()))
        );
        assert_eq!(
            c.held("session=abc; HttpOnly", &undecided),
            None,
            "a cookie in no category is needed"
        );
        assert_eq!(c.held("bp_consent=x", &undecided), None);
        assert_eq!(c.held("garbage", &undecided), None);
        let (kept, gone) = c.strip_cookies(
            "session=1; _ga=GA1; _fbp=fb; theme=dark; _gid=2",
            &analytics,
        );
        assert_eq!(kept, "session=1; _ga=GA1; theme=dark; _gid=2");
        assert_eq!(gone, ["_fbp"]);
        let (kept, gone) = c.strip_cookies("_ga=1;_fbp=2", &Choice::Gpc);
        assert_eq!((kept.as_str(), gone.len()), ("", 2));
        assert_eq!(
            c.expiries(
                "session=1; _ga=1; _fbp=2; _ga=3",
                &["marketing".into()],
                "www.example.com"
            ),
            [
                "_ga=; Path=/; Max-Age=0",
                "_ga=; Path=/; Domain=www.example.com; Max-Age=0",
                "_ga=; Path=/; Domain=example.com; Max-Age=0",
            ]
        );
        assert_eq!(
            c.expiries("_ga=1", &[], "localhost"),
            ["_ga=; Path=/; Max-Age=0"]
        );
    }

    #[test]
    fn decisions() {
        let c = Config::default();
        let all = vec!["analytics".to_string(), "marketing".to_string()];
        assert_eq!(
            c.decide("accept=all&r=%2F", true),
            Decision::Keep(all.clone())
        );
        assert_eq!(
            c.decide("accept=all", false),
            Decision::Show(true),
            "accepting from another site"
        );
        assert_eq!(
            c.decide("reject=all", false),
            Decision::Keep(vec![]),
            "refusing from anywhere"
        );
        assert_eq!(
            c.decide("save=1&set=marketing&set=bogus&r=%2Fx", true),
            Decision::Keep(vec!["marketing".into()])
        );
        assert_eq!(c.decide("save=1", true), Decision::Keep(vec![]));
        assert_eq!(
            c.decide("save=1&set=analytics", false),
            Decision::Show(true)
        );
        assert_eq!(c.decide("r=%2F", true), Decision::Show(false));
        assert_eq!(c.decide("", true), Decision::Show(false));
        assert_eq!(
            c.decide("accepted=1", true),
            Decision::Show(false),
            "only the exact names"
        );
        assert_eq!(Config::names(&all), "analytics and marketing");
        assert_eq!(Config::names(&[]), "nothing");
    }

    #[test]
    fn pages_and_sites() {
        let ff = "Mozilla/5.0 (X11; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0";
        let html = Some("text/html,application/xhtml+xml,*/*;q=0.8");
        assert!(is_page("GET", Some("document"), Some("navigate"), html, ff));
        assert!(
            !is_page("GET", Some("iframe"), Some("navigate"), html, ff),
            "a page in an iframe"
        );
        assert!(!is_page(
            "GET",
            Some("image"),
            Some("no-cors"),
            Some("image/*"),
            ff
        ));
        assert!(
            is_page("GET", None, None, html, ff),
            "an older browser that asks for HTML"
        );
        assert!(!is_page("GET", None, None, Some("*/*"), ff));
        assert!(!is_page(
            "POST",
            Some("document"),
            Some("navigate"),
            html,
            ff
        ));
        assert!(!is_page(
            "GET",
            Some("document"),
            Some("navigate"),
            html,
            "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"
        ));
        assert!(!is_page("GET", None, None, html, "curl/8.5.0"));

        assert!(same_site(Some("same-origin"), None, "a.example"));
        assert!(
            same_site(Some("none"), None, "a.example"),
            "typed or bookmarked"
        );
        assert!(
            !same_site(Some("cross-site"), Some("https://a.example/"), "a.example"),
            "Sec-Fetch-Site decides"
        );
        assert!(
            !same_site(Some("same-site"), None, "a.example"),
            "a sister subdomain isn't the site"
        );
        assert!(same_site(
            None,
            Some("https://a.example:8443/x?y"),
            "a.example:8443"
        ));
        assert!(!same_site(
            None,
            Some("https://evil.example/a.example"),
            "a.example"
        ));
        assert!(!same_site(None, Some("not a url"), "a.example"));
        assert!(same_site(None, None, "a.example"));

        assert_eq!(
            return_to("r=%2Fdocs%2F%3Fq%3D1", None, "a.example"),
            "/docs/?q=1"
        );
        assert_eq!(
            return_to("r=https%3A%2F%2Fevil.example%2F", None, "a.example"),
            "/"
        );
        assert_eq!(
            return_to("", Some("https://a.example/blog/post?x=1#top"), "a.example"),
            "/blog/post?x=1"
        );
        assert_eq!(return_to("", Some("https://a.example"), "a.example"), "/");
        assert_eq!(
            return_to("", Some("https://evil.example/x"), "a.example"),
            "/"
        );
        assert_eq!(return_to("", None, "a.example"), "/");
    }
}
