// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! A small reader for MaxMind DB files (.mmdb), the format of GeoLite2,
//! DB-IP Lite, IPinfo Lite and others: a binary search tree over address
//! bits, then a data section of maps and strings. The file is queried where
//! it lies, so loading one costs nothing beyond reading it. Only what a
//! country lookup needs is decoded, and every read is bounds-checked: a
//! broken file gives an error, never a panic.
//!
//! The format: https://maxmind.github.io/MaxMind-DB/

use std::net::IpAddr;

const MARKER: &[u8] = b"\xAB\xCD\xEFMaxMind.com";

pub struct Db {
    buf: Vec<u8>,
    node_count: u32,
    record_size: u32,
    ip_version: u32,
    /// Where the data section starts.
    data: usize,
    /// The node IPv4 addresses start from in an IPv6 tree, after 96 zero
    /// bits; None when the tree has no IPv4 part.
    v4_start: Option<u32>,
    pub db_type: String,
    pub build_epoch: u64,
}

type R<T> = Result<T, String>;

fn broken(what: &str) -> String {
    format!("the database is broken: {what}")
}

impl Db {
    pub fn open(buf: Vec<u8>) -> R<Db> {
        let tail = buf.len().saturating_sub(128 * 1024);
        let at = buf[tail..]
            .windows(MARKER.len())
            .rposition(|w| w == MARKER)
            .ok_or("this isn't a MaxMind DB file (.mmdb): it has no metadata marker")?;
        let meta = tail + at + MARKER.len();
        let mut db = Db {
            buf,
            node_count: 0,
            record_size: 0,
            ip_version: 0,
            data: meta,
            v4_start: None,
            db_type: String::new(),
            build_epoch: 0,
        };
        // The metadata is decoded like the data section, from its own start.
        let get = |db: &Db, k: &str| -> R<u64> {
            let o = db
                .field(meta, k)?
                .ok_or_else(|| broken(&format!("no {k} in its metadata")))?;
            db.uint(o)
        };
        db.node_count = u32::try_from(get(&db, "node_count")?).map_err(|_| broken("node_count"))?;
        db.record_size = get(&db, "record_size")? as u32;
        db.ip_version = get(&db, "ip_version")? as u32;
        db.build_epoch = get(&db, "build_epoch").unwrap_or(0);
        if let Some(o) = db.field(meta, "database_type")? {
            db.db_type = db.string(o)?.0.to_string();
        }
        if !matches!(db.record_size, 24 | 28 | 32) {
            return Err(broken(&format!("record size {}", db.record_size)));
        }
        if !matches!(db.ip_version, 4 | 6) {
            return Err(broken(&format!("IP version {}", db.ip_version)));
        }
        let tree = (db.record_size as usize * 2 / 8)
            .checked_mul(db.node_count as usize)
            .ok_or_else(|| broken("its tree size"))?;
        if tree + 16 > tail + at {
            return Err(broken("its tree runs past its end"));
        }
        db.data = tree + 16;
        if db.ip_version == 6 {
            let mut node = 0u32;
            for _ in 0..96 {
                if node >= db.node_count {
                    break;
                }
                node = db.record(node, 0)?;
            }
            db.v4_start = (node < db.node_count).then_some(node);
        } else {
            db.v4_start = Some(0);
        }
        Ok(db)
    }

    /// The size of the file in bytes; for comparing with a new copy.
    pub fn size(&self) -> usize {
        self.buf.len()
    }

    fn byte(&self, o: usize) -> R<u8> {
        self.buf
            .get(o)
            .copied()
            .ok_or_else(|| broken("a read past its end"))
    }

    fn be(&self, o: usize, n: usize) -> R<u64> {
        let b = self
            .buf
            .get(o..o + n)
            .ok_or_else(|| broken("a read past its end"))?;
        Ok(b.iter().fold(0u64, |v, &x| v << 8 | x as u64))
    }

    fn record(&self, node: u32, bit: u8) -> R<u32> {
        let n = node as usize;
        Ok(match self.record_size {
            24 => self.be(n * 6 + bit as usize * 3, 3)? as u32,
            28 => {
                let mid = self.byte(n * 7 + 3)? as u32;
                if bit == 0 {
                    (mid & 0xF0) << 20 | self.be(n * 7, 3)? as u32
                } else {
                    (mid & 0x0F) << 24 | self.be(n * 7 + 4, 3)? as u32
                }
            }
            _ => self.be(n * 8 + bit as usize * 4, 4)? as u32,
        })
    }

    /// The data for an address: an offset into the data section, or None
    /// when the database has no record for it.
    pub fn lookup(&self, ip: IpAddr) -> R<Option<usize>> {
        let ip = match ip {
            IpAddr::V6(v6) => v6
                .to_ipv4_mapped()
                .map(IpAddr::V4)
                .unwrap_or(IpAddr::V6(v6)),
            v4 => v4,
        };
        let (bits, mut node): (u128, u32) = match ip {
            IpAddr::V4(v4) => match self.v4_start {
                Some(n) => ((u32::from(v4) as u128) << 96, n),
                None => return Ok(None),
            },
            IpAddr::V6(_) if self.ip_version == 4 => return Ok(None),
            IpAddr::V6(v6) => (u128::from(v6), 0),
        };
        let depth = if ip.is_ipv4() { 32 } else { 128 };
        for i in 0..depth {
            if node >= self.node_count {
                break;
            }
            let bit = (bits >> (127 - i) & 1) as u8;
            node = self.record(node, bit)?;
        }
        if node == self.node_count {
            return Ok(None);
        }
        if node < self.node_count {
            return Err(broken("its tree is deeper than an address"));
        }
        let off = (node - self.node_count) as usize;
        if off < 16 {
            return Err(broken("a record points into the separator"));
        }
        Ok(Some(self.data + off - 16))
    }

    /// The country code for an address: country.iso_code (MaxMind, DB-IP),
    /// country_code (IPinfo), or registered_country.iso_code.
    pub fn country(&self, ip: IpAddr) -> R<Option<String>> {
        let Some(o) = self.lookup(ip)? else {
            return Ok(None);
        };
        for path in [
            &["country", "iso_code"][..],
            &["country_code"],
            &["registered_country", "iso_code"],
        ] {
            if let Some(s) = self.path_str(o, path)? {
                return Ok(Some(s));
            }
        }
        Ok(None)
    }

    /// The string at a path of map keys from a value, if there is one.
    pub fn path_str(&self, mut o: usize, path: &[&str]) -> R<Option<String>> {
        for k in path {
            match self.field(o, k)? {
                Some(v) => o = v,
                None => return Ok(None),
            }
        }
        let (t, _, _) = self.head(self.resolve(o)?)?;
        Ok(if t == 2 {
            Some(self.string(o)?.0.to_string())
        } else {
            None
        })
    }

    /// Reads a control byte: (type, size or pointer, where the payload starts).
    fn head(&self, mut o: usize) -> R<(u8, usize, usize)> {
        let c = self.byte(o)?;
        o += 1;
        let mut t = c >> 5;
        if t == 1 {
            let v = (c & 7) as usize;
            let (p, n) = match (c >> 3) & 3 {
                0 => ((v << 8) | self.be(o, 1)? as usize, 1),
                1 => (((v << 16) | self.be(o, 2)? as usize) + 2048, 2),
                2 => (((v << 24) | self.be(o, 3)? as usize) + 526336, 3),
                _ => (self.be(o, 4)? as usize, 4),
            };
            return Ok((1, p, o + n));
        }
        if t == 0 {
            t = 7u8
                .checked_add(self.byte(o)?)
                .ok_or_else(|| broken("an unknown type"))?;
            o += 1;
        }
        let mut size = (c & 0x1f) as usize;
        if size >= 29 {
            let n = size - 28;
            let v = self.be(o, n)? as usize;
            o += n;
            size = [29, 285, 65821][n - 1] + v;
        }
        Ok((t, size, o))
    }

    /// Follows a pointer to the value it points at.
    fn resolve(&self, o: usize) -> R<usize> {
        let (t, p, _) = self.head(o)?;
        if t != 1 {
            return Ok(o);
        }
        let to = self
            .data
            .checked_add(p)
            .ok_or_else(|| broken("a pointer"))?;
        if to >= self.buf.len() || self.head(to)?.0 == 1 {
            return Err(broken("a pointer goes nowhere"));
        }
        Ok(to)
    }

    /// Where the value after this one starts.
    fn skip(&self, o: usize, depth: u32) -> R<usize> {
        if depth > 64 {
            return Err(broken("values nested too deep"));
        }
        let (t, size, mut p) = self.head(o)?;
        Ok(match t {
            1 | 14 => p,
            3 => p + 8,
            15 => p + 4,
            2 | 4 | 5 | 6 | 8 | 9 | 10 => p + size,
            7 | 11 => {
                let items = if t == 7 { size * 2 } else { size };
                for _ in 0..items {
                    p = self.skip(p, depth + 1)?;
                }
                p
            }
            _ => return Err(broken(&format!("an unknown type {t}"))),
        })
    }

    fn string(&self, o: usize) -> R<(&str, usize)> {
        let next = self.skip(o, 0)?;
        let (t, size, p) = self.head(self.resolve(o)?)?;
        if t != 2 {
            return Err(broken("a string that isn't one"));
        }
        let b = self
            .buf
            .get(p..p + size)
            .ok_or_else(|| broken("a string past its end"))?;
        Ok((
            std::str::from_utf8(b).map_err(|_| broken("a string that isn't UTF-8"))?,
            next,
        ))
    }

    fn uint(&self, o: usize) -> R<u64> {
        let (t, size, p) = self.head(self.resolve(o)?)?;
        match t {
            5 | 6 | 9 | 10 if size <= 8 => self.be(p, size),
            _ => Err(broken("a number that isn't one")),
        }
    }

    /// The value of a key in the map at o, if the map has it.
    fn field(&self, o: usize, key: &str) -> R<Option<usize>> {
        let (t, n, mut p) = self.head(self.resolve(o)?)?;
        if t != 7 {
            return Ok(None);
        }
        for _ in 0..n {
            let (k, v) = self.string(p)?;
            if k == key {
                return Ok(Some(v));
            }
            p = self.skip(v, 0)?;
        }
        Ok(None)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn open(name: &str) -> Db {
        let b = std::fs::read(format!("{}/testdata/{name}", env!("CARGO_MANIFEST_DIR"))).unwrap();
        Db::open(b).unwrap()
    }

    fn ip(s: &str) -> IpAddr {
        s.parse().unwrap()
    }

    #[test]
    fn record_sizes() {
        for size in [24, 28, 32] {
            let db = open(&format!("MaxMind-DB-test-ipv4-{size}.mmdb"));
            assert_eq!(db.db_type, "Test");
            for (addr, net) in [
                ("1.1.1.1", "1.1.1.1"),
                ("1.1.1.3", "1.1.1.2"),
                ("1.1.1.7", "1.1.1.4"),
                ("1.1.1.15", "1.1.1.8"),
                ("1.1.1.31", "1.1.1.16"),
                ("1.1.1.32", "1.1.1.32"),
            ] {
                let o = db
                    .lookup(ip(addr))
                    .unwrap()
                    .unwrap_or_else(|| panic!("{size}: {addr}"));
                assert_eq!(
                    db.path_str(o, &["ip"]).unwrap().as_deref(),
                    Some(net),
                    "{size}: {addr}"
                );
            }
            assert_eq!(db.lookup(ip("1.1.1.33")).unwrap(), None);
            assert_eq!(db.lookup(ip("2.2.2.2")).unwrap(), None);
            assert_eq!(
                db.lookup(ip("2001:db8::1")).unwrap(),
                None,
                "IPv6 in an IPv4 database"
            );
        }
    }

    #[test]
    fn ipv6() {
        let db = open("MaxMind-DB-test-ipv6-24.mmdb");
        for (addr, net) in [
            ("::1:ffff:ffff", "::1:ffff:ffff"),
            ("::2:0:41", "::2:0:40"),
            ("::2:0:59", "::2:0:58"),
        ] {
            let o = db.lookup(ip(addr)).unwrap().unwrap();
            assert_eq!(
                db.path_str(o, &["ip"]).unwrap().as_deref(),
                Some(net),
                "{addr}"
            );
        }
        assert_eq!(db.lookup(ip("::abcd")).unwrap(), None);
        // This one has no IPv4 part.
        assert_eq!(db.lookup(ip("1.1.1.1")).unwrap(), None);
    }

    #[test]
    fn countries() {
        let db = open("GeoIP2-Country-Test.mmdb");
        assert_eq!(db.db_type, "GeoIP2-Country");
        assert!(db.build_epoch > 0);
        for (addr, cc) in [
            ("2.125.160.216", Some("GB")),
            ("89.160.20.115", Some("SE")),
            ("::ffff:89.160.20.115", Some("SE")),
            ("175.16.199.5", Some("CN")),
            ("67.43.156.1", Some("BT")),
            ("2001:218::1", Some("JP")),
            ("2001:220::5", Some("KR")),
            ("214.78.120.5", Some("US")),
            ("10.0.0.1", None),
            ("127.0.0.1", None),
        ] {
            assert_eq!(db.country(ip(addr)).unwrap().as_deref(), cc, "{addr}");
        }
    }

    #[test]
    fn decoder_types() {
        // A record with every type; its map is walked past them all.
        let db = open("MaxMind-DB-test-decoder.mmdb");
        let o = db.lookup(ip("1.1.1.1")).unwrap().unwrap();
        assert_eq!(
            db.path_str(o, &["utf8_string"]).unwrap().as_deref(),
            Some("unicode! ☯ - ♫")
        );
        assert_eq!(
            db.path_str(o, &["map", "mapX", "utf8_stringX"])
                .unwrap()
                .as_deref(),
            Some("hello")
        );
        assert_eq!(db.path_str(o, &["no such key"]).unwrap(), None);
    }

    #[test]
    fn broken_files_give_errors() {
        for f in [
            "MaxMind-DB-test-broken-pointers-24.mmdb",
            "MaxMind-DB-test-broken-search-tree-24.mmdb",
        ] {
            let b = std::fs::read(format!("{}/testdata/{f}", env!("CARGO_MANIFEST_DIR"))).unwrap();
            if let Ok(db) = Db::open(b) {
                for a in ["1.1.1.1", "1.1.1.16", "1.1.1.32", "::1"] {
                    let _ = db.country(ip(a)); // an error or an answer, never a panic
                }
            }
        }
        assert!(
            Db::open(b"not a database".to_vec())
                .err()
                .unwrap()
                .contains("isn't a MaxMind DB file")
        );
        // A truncated file.
        let mut b = std::fs::read(format!(
            "{}/testdata/GeoIP2-Country-Test.mmdb",
            env!("CARGO_MANIFEST_DIR")
        ))
        .unwrap();
        let tail = b.split_off(b.len() - 600);
        b.truncate(2000);
        b.extend(tail);
        assert!(Db::open(b).is_err());
    }
}
