// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! The signing plugin's config, the two signatures it makes and checks,
//! and the base64url they're written in, kept apart from the Proxy-Wasm
//! calls so they can be tested on their own.
//!
//! Both are HMAC-SHA256 over a short text that starts with the scheme's
//! version, so the app or a script can make or check them in a few lines:
//!
//!   request: "v1\n" TIMESTAMP "\n" METHOD "\n" PATH-AND-QUERY ("\n" name ":" value)*
//!   link:    "v1\n" EXPIRES "\n" PATH

use hmac::{Hmac, Mac};
use kit::{Item, Line};
use sha2::Sha256;

type HmacSha256 = Hmac<Sha256>;

/// The headers the plugin adds to a signed request, and removes from what
/// a client sends.
pub const TIMESTAMP: &str = "x-bareproxy-timestamp";
pub const SIGNATURE: &str = "x-bareproxy-signature";
pub const SIGNED_HEADERS: &str = "x-bareproxy-signed-headers";
/// The path and query the signature covers, as the request arrived: the
/// app may get another path, after BareProxy normalizes it or a route
/// strips a prefix, so it checks the signature against this header.
pub const SIGNED_PATH: &str = "x-bareproxy-path";

#[derive(Debug, Clone, PartialEq)]
pub struct Key {
    pub name: String,
    secret: Vec<u8>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct Config {
    pub keys: Vec<Key>,
    /// The key requests to the app are signed with.
    pub sign: Option<usize>,
    /// Request headers that go into the request signature, lower case.
    pub sign_headers: Vec<String>,
    /// Keys a signed link may be made with; more than one while a key is
    /// being changed.
    pub links: Vec<usize>,
    /// Path prefixes that need a signed link.
    pub link_paths: Vec<String>,
    /// The query parameters a link carries.
    pub expires_param: String,
    pub sig_param: String,
}

impl Config {
    pub fn parse(text: &str) -> Result<Config, String> {
        let mut c = Config {
            keys: vec![],
            sign: None,
            sign_headers: vec![],
            links: vec![],
            link_paths: vec![],
            expires_param: "expires".into(),
            sig_param: "sig".into(),
        };
        let mut sign_name: Option<(String, Line)> = None;
        let mut link_names: Vec<(String, Line)> = vec![];
        for item in kit::parse(text, &[])? {
            let Item::Line(line) = item else {
                unreachable!()
            };
            let a = line.args();
            match line.name() {
                "key" => match a {
                    [name, secret] => {
                        if secret.len() < 32 {
                            return Err(line.err(format!(
                                "key {name} is too short: use at least 32 characters, such as the output of openssl rand -hex 32"
                            )));
                        }
                        if c.keys.iter().any(|k| k.name == *name) {
                            return Err(line.err(format!("key {name} is set twice")));
                        }
                        c.keys.push(Key {
                            name: name.clone(),
                            secret: secret.as_bytes().to_vec(),
                        });
                    }
                    _ => return Err(line.err("key takes a name and a secret: key app 7f3a...")),
                },
                "sign" => match a {
                    [k, name] if k == "key" => sign_name = Some((name.clone(), line.clone())),
                    _ => return Err(line.err("sign takes key NAME: sign key app")),
                },
                "sign-headers" => {
                    if a.is_empty() {
                        return Err(
                            line.err("sign-headers takes header names, such as x-country x-user")
                        );
                    }
                    for h in a {
                        if !h
                            .bytes()
                            .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
                        {
                            return Err(line.err(format!("{h} isn't a header name")));
                        }
                        c.sign_headers.push(h.to_ascii_lowercase());
                    }
                }
                "links" => match a {
                    [k, names @ ..] if k == "key" && !names.is_empty() => {
                        for n in names {
                            link_names.push((n.clone(), line.clone()));
                        }
                    }
                    _ => {
                        return Err(line.err(
                            "links takes key NAME..., one or more keys: links key app app-old",
                        ));
                    }
                },
                "link-path" => {
                    if a.is_empty() {
                        return Err(line.err("link-path takes path prefixes, such as /private/"));
                    }
                    for p in a {
                        if !p.starts_with('/') {
                            return Err(line.err(format!("{p} isn't a path; a path starts with /")));
                        }
                        c.link_paths.push(p.clone());
                    }
                }
                "link-params" => match a {
                    [e, s] if e != s => {
                        c.expires_param = e.clone();
                        c.sig_param = s.clone();
                    }
                    _ => {
                        return Err(line
                            .err("link-params takes two names: the expiry's and the signature's"));
                    }
                },
                other => return Err(line.err(format!("unknown setting {other}"))),
            }
        }
        let find = |c: &Config, (name, line): &(String, Line)| -> Result<usize, String> {
            c.keys.iter().position(|k| k.name == *name).ok_or_else(|| {
                line.err(format!(
                    "no key named {name}; add a line such as key {name} SECRET"
                ))
            })
        };
        if let Some(s) = &sign_name {
            c.sign = Some(find(&c, s)?);
        }
        for l in &link_names {
            let i = find(&c, l)?;
            if !c.links.contains(&i) {
                c.links.push(i);
            }
        }
        if !c.link_paths.is_empty() && c.links.is_empty() {
            return Err(
                "link-path needs a links key line saying which keys links are made with".into(),
            );
        }
        if !c.links.is_empty() && c.link_paths.is_empty() {
            return Err(
                "links key needs a link-path line saying which paths need a signed link".into(),
            );
        }
        if c.sign.is_none() && c.links.is_empty() {
            return Err(
                "nothing to do: add sign key NAME, or links key NAME with link-path".into(),
            );
        }
        Ok(c)
    }

    /// The signature headers for a request to the app. `header` gives a
    /// request header's value by its lower-case name.
    pub fn sign_request(
        &self,
        now: u64,
        method: &str,
        path: &str,
        header: &dyn Fn(&str) -> Option<String>,
    ) -> Option<Vec<(String, String)>> {
        let k = &self.keys[self.sign?];
        let mut msg = format!("v1\n{now}\n{method}\n{path}");
        for h in &self.sign_headers {
            msg.push_str(&format!("\n{h}:{}", header(h).unwrap_or_default()));
        }
        let mut out = vec![
            (SIGNED_PATH.to_string(), path.to_string()),
            (TIMESTAMP.to_string(), now.to_string()),
            (
                SIGNATURE.to_string(),
                format!("v1={}", b64url(&mac(&k.secret, &msg))),
            ),
        ];
        if !self.sign_headers.is_empty() {
            out.push((SIGNED_HEADERS.to_string(), self.sign_headers.join(",")));
        }
        Some(out)
    }

    /// Whether a path needs a signed link.
    pub fn needs_link(&self, path: &str) -> bool {
        self.link_paths.iter().any(|p| kit::under(path, p))
    }

    /// Checks a signed link: Ok with the key's name, or why it's refused.
    pub fn check_link(&self, now: u64, path: &str, query: &str) -> Result<&str, String> {
        let (mut expires, mut sig) = (None, None);
        for pair in query.split('&') {
            let (k, v) = pair.split_once('=').unwrap_or((pair, ""));
            if k == self.expires_param {
                expires = Some(v);
            } else if k == self.sig_param {
                sig = Some(v);
            }
        }
        let (Some(expires), Some(sig)) = (expires, sig) else {
            return Err(format!(
                "no signed link: {path} needs ?{}=...&{}=...",
                self.expires_param, self.sig_param
            ));
        };
        let Ok(at) = expires.parse::<u64>() else {
            return Err(format!(
                "the link's {} isn't a time in seconds since 1970",
                self.expires_param
            ));
        };
        let Some(sig) = b64url_decode(sig) else {
            return Err("the link's signature isn't base64url".into());
        };
        let msg = format!("v1\n{at}\n{path}");
        let Some(k) = self
            .links
            .iter()
            .map(|&i| &self.keys[i])
            .find(|k| verify(&k.secret, &msg, &sig))
        else {
            return Err(
                "the link's signature doesn't match: changed, or made with another key".into(),
            );
        };
        if now > at {
            return Err(format!("the link expired {} ago", ago(now - at)));
        }
        Ok(&k.name)
    }

    /// Makes a signed link's query, for tests and the README's examples.
    pub fn make_link(&self, key: usize, expires: u64, path: &str) -> String {
        let s = mac(&self.keys[key].secret, &format!("v1\n{expires}\n{path}"));
        format!(
            "{}={expires}&{}={}",
            self.expires_param,
            self.sig_param,
            b64url(&s)
        )
    }
}

fn mac(secret: &[u8], msg: &str) -> Vec<u8> {
    let mut m = HmacSha256::new_from_slice(secret).expect("HMAC takes any key length");
    m.update(msg.as_bytes());
    m.finalize().into_bytes().to_vec()
}

/// Compares in constant time, so the time taken says nothing about how
/// much of a guessed signature was right.
fn verify(secret: &[u8], msg: &str, sig: &[u8]) -> bool {
    let mut m = HmacSha256::new_from_slice(secret).expect("HMAC takes any key length");
    m.update(msg.as_bytes());
    m.verify_slice(sig).is_ok()
}

fn ago(s: u64) -> String {
    match s {
        0..60 => format!("{s}s"),
        60..3600 => format!("{}m", s / 60),
        3600..86400 => format!("{}h", s / 3600),
        _ => format!("{}d", s / 86400),
    }
}

const B64: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";

/// base64url without padding, as in RFC 4648 section 5.
pub fn b64url(b: &[u8]) -> String {
    let mut out = String::new();
    for c in b.chunks(3) {
        let n = (c[0] as u32) << 16
            | (*c.get(1).unwrap_or(&0) as u32) << 8
            | *c.get(2).unwrap_or(&0) as u32;
        for i in 0..c.len() + 1 {
            out.push(B64[(n >> (18 - 6 * i) & 63) as usize] as char);
        }
    }
    out
}

/// Decodes base64url, with or without padding; standard base64's + and /
/// are taken too, since a hand-made link may use them.
pub fn b64url_decode(s: &str) -> Option<Vec<u8>> {
    let s = s.trim_end_matches('=');
    let mut out = Vec::new();
    let (mut acc, mut bits) = (0u32, 0);
    for ch in s.bytes() {
        let v = match ch {
            b'A'..=b'Z' => ch - b'A',
            b'a'..=b'z' => ch - b'a' + 26,
            b'0'..=b'9' => ch - b'0' + 52,
            b'-' | b'+' => 62,
            b'_' | b'/' => 63,
            _ => return None,
        };
        acc = acc << 6 | v as u32;
        bits += 6;
        if bits >= 8 {
            bits -= 8;
            out.push((acc >> bits) as u8);
            acc &= (1 << bits) - 1;
        }
    }
    Some(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    const KEY: &str = "0123456789abcdef0123456789abcdef";
    const OLD: &str = "fedcba9876543210fedcba9876543210";

    fn hex(b: &[u8]) -> String {
        b.iter().map(|x| format!("{x:02x}")).collect()
    }

    #[test]
    fn hmac_matches_rfc_4231() {
        // RFC 4231, test case 2.
        assert_eq!(
            hex(&mac(b"Jefe", "what do ya want for nothing?")),
            "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843"
        );
    }

    #[test]
    fn base64url() {
        for (raw, enc) in [
            ("", ""),
            ("f", "Zg"),
            ("fo", "Zm8"),
            ("foo", "Zm9v"),
            ("foob", "Zm9vYg"),
            ("\u{fb}\u{ff}", "w7vDvw"),
        ] {
            assert_eq!(b64url(raw.as_bytes()), enc);
            assert_eq!(b64url_decode(enc).unwrap(), raw.as_bytes());
        }
        assert_eq!(
            b64url_decode("Zm9v+/8=").unwrap(),
            b64url_decode("Zm9v-_8").unwrap()
        );
        assert_eq!(b64url_decode("Zm9v!"), None);
    }

    #[test]
    fn signed_requests() {
        let c = Config::parse(&format!(
            "key app {KEY}\nsign key app\nsign-headers X-Country x-user\n"
        ))
        .unwrap();
        let h = |n: &str| (n == "x-country").then(|| "DE".to_string());
        let out = c
            .sign_request(1_760_000_000, "GET", "/api/orders?page=2", &h)
            .unwrap();
        let msg = "v1\n1760000000\nGET\n/api/orders?page=2\nx-country:DE\nx-user:";
        assert_eq!(
            out,
            [
                (SIGNED_PATH.to_string(), "/api/orders?page=2".to_string()),
                (TIMESTAMP.to_string(), "1760000000".to_string()),
                (
                    SIGNATURE.to_string(),
                    format!("v1={}", b64url(&mac(KEY.as_bytes(), msg)))
                ),
                (SIGNED_HEADERS.to_string(), "x-country,x-user".to_string()),
            ]
        );
    }

    #[test]
    fn signed_links() {
        let c = Config::parse(&format!(
            "key new {KEY}\nkey old {OLD}\nlinks key new old\nlink-path /private/ /downloads/\n"
        ))
        .unwrap();
        assert!(
            c.needs_link("/private/a.pdf")
                && c.needs_link("/downloads/x")
                && !c.needs_link("/public/x")
        );
        let now = 1_760_000_000;
        let q = c.make_link(0, now + 600, "/private/a.pdf");
        assert_eq!(c.check_link(now, "/private/a.pdf", &q), Ok("new"));
        assert_eq!(
            c.check_link(now, "/private/a.pdf", &format!("v=2&{q}")),
            Ok("new")
        );
        // A link made with the old key still works while it's listed.
        let q_old = c.make_link(1, now + 600, "/private/a.pdf");
        assert_eq!(c.check_link(now, "/private/a.pdf", &q_old), Ok("old"));
        for (path, query, why) in [
            ("/private/b.pdf", q.clone(), "doesn't match"),
            (
                "/private/a.pdf",
                q.replace(&(now + 600).to_string(), &(now + 6000).to_string()),
                "doesn't match",
            ),
            ("/private/a.pdf", "x=1".to_string(), "no signed link"),
            (
                "/private/a.pdf",
                "expires=soon&sig=abc".to_string(),
                "isn't a time",
            ),
            (
                "/private/a.pdf",
                format!("expires={}&sig=*", now + 1),
                "isn't base64url",
            ),
        ] {
            let err = c.check_link(now, path, &query).unwrap_err();
            assert!(err.contains(why), "{path}?{query}: {err}");
        }
        assert_eq!(
            c.check_link(now + 4000, "/private/a.pdf", &q).unwrap_err(),
            "the link expired 56m ago"
        );
    }

    #[test]
    fn link_matches_the_readme_recipes() {
        // The same link the README's openssl and Python recipes make.
        let c = Config::parse(&format!("key k {KEY}\nlinks key k\nlink-path /private/")).unwrap();
        assert_eq!(
            c.make_link(0, 1760000600, "/private/report.txt"),
            "expires=1760000600&sig=6Z00TMdb06MZhTQgB6k6B2zmQentG-_tTH5kBlx2Irk"
        );
    }

    #[test]
    fn config_errors() {
        for (text, want) in [
            ("key app short", "line 1: key app is too short"),
            (
                &format!("key a {KEY}\nkey a {KEY}"),
                "line 2: key a is set twice",
            ),
            (
                &format!("key a {KEY}\nsign key b"),
                "line 2: no key named b",
            ),
            (
                &format!("key a {KEY}\nlinks key a"),
                "links key needs a link-path line",
            ),
            (
                &format!("key a {KEY}\nlink-path /p/"),
                "link-path needs a links key line",
            ),
            (&format!("key a {KEY}"), "nothing to do"),
            (
                &format!("key a {KEY}\nsign a"),
                "line 2: sign takes key NAME",
            ),
            (
                &format!("key a {KEY}\nsign key a\nsign-headers x:y"),
                "line 3: x:y isn't a header name",
            ),
            (
                &format!("key a {KEY}\nsecret x"),
                "line 2: unknown setting secret",
            ),
        ] {
            let err = Config::parse(text).unwrap_err();
            assert!(err.contains(want), "{text}: {err}");
        }
    }
}
