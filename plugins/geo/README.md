# Geo rules

A BareProxy plugin. It looks up the country of each request's address in a local database file, passes it on in a request header that the app and BareProxy's own route rules can use, and lets in or turns away visitors by country, for the whole site or a path. No lookup leaves the machine.

It's released as `geo.wasm` with each BareProxy release, from 0.7.0 on.

## The database

The plugin reads MaxMind DB files (`.mmdb`), the format the free country databases come in:

- [GeoLite2 Country](https://dev.maxmind.com/geoip/geolite2-free-geolocation-data) from MaxMind (free with an account)
- [DB-IP IP to Country Lite](https://db-ip.com/db/download/ip-to-country-lite) (free, CC BY 4.0, no account)
- [IPinfo Lite](https://ipinfo.io/lite) (free with an account)

It takes the country from `country.iso_code` (MaxMind, DB-IP), `country_code` (IPinfo) or `registered_country.iso_code`, whichever the file has. BareProxy doesn't ship a database: each has its own license and is updated monthly, so you download one and keep it fresh with the provider's updater or a cron job. The plugin picks up a new file on its own.

## Use it

```
plugin geo /etc/bareproxy/plugins/geo.wasm
  config /etc/bareproxy/plugins/geo.conf
  read /var/lib/geoip
  body-limit 32MB

site example.com
  use geo
  route /* header X-Country=FR -> eu-app
  route /* -> app
```

`read` names the folder the database is in. `body-limit` is also the most the plugin may read from a file; a country database is 5 to 10 MB, more than the 8 MB default for some.

## Config

```
# geo.conf
database dbip-country-lite.mmdb
header X-Country            # or off
block RU KP
status 403                  # or 451
reload 1h                   # how often to check the file; off
forwarded-from 173.245.48.0/20 103.21.244.0/22

path /checkout/
  allow DE AT CH
  unknown block

page
<!doctype html>
<title>Not available</title>
<p>Sorry, this shop doesn't deliver to your country.</p>
end
```

| Setting | Default | What it does |
|---|---|---|
| `database FILE` | (needed) | The `.mmdb` file, inside a folder the plugin line names with `read` |
| `header NAME` or `off` | X-Country | The request header the country goes in, as an ISO code such as `DE`, or `XX` when the address has none. A value the client sent is always replaced |
| `allow CC...` | none | Only these countries get in |
| `block CC...` | none | These countries are turned away |
| `unknown allow` or `block` | allow | Addresses with no country: local and private ones, or ones the database lacks |
| `status 403` or `451` | 403 | The status for those turned away, with the page |
| `page` ... `end` | "Not available here" | The page they get, as HTML |
| `reload TIME` or `off` | 1h | How often the database file is checked for a new version |
| `forwarded-from ADDR...` | none | Proxies in front, such as a CDN, whose `X-Forwarded-For` is believed |
| `path PREFIX` | | Starts a section with its own `allow`, `block` and `unknown`; it takes from the top what it doesn't set, and the longest prefix wins |

Country codes are two letters, ISO 3166. With no `allow` or `block` anywhere, the plugin only sets the header.

## Behind a CDN or load balancer

The address BareProxy sees is the connection's. Behind a CDN that's the CDN's own address, so every visitor would get the CDN's country. List the CDN's address ranges with `forwarded-from`: when a request comes from one of them, the plugin takes the client's address from `X-Forwarded-For`, the last entry that isn't one of those proxies. From anyone else, `X-Forwarded-For` is ignored, so it can't be used to fake a country.

## What `why` shows

A visitor turned away is answered by the plugin, and the record says why: "turned away: CN is on the block list for / (line 4) (address 175.16.199.5)". A request let in carries the country header, and a route chosen by it shows in the record like any other rule. A new database version is a line in BareProxy's log; a broken one is refused there too, and the database as it was keeps answering.

## Limits worth knowing

Address-to-country data is good, not perfect: VPNs, mobile carriers and cloud providers move addresses around. Geo rules are a sensible filter, never proof of where someone is.

## How it works

The plugin's own reader for the MaxMind DB format (no outside crate) queries the file where it lies: loading it costs only reading it, and a lookup walks at most 128 steps of the search tree. Every read is checked against the file's size, so a broken or hostile file gives an error, never a crash. `check` and `plan` load the database, so a missing or broken file is an error there, before anything goes live.

## Build and test

`plugins/build.sh` builds it with the other plugins. `cargo test -p geo` runs its unit tests against MaxMind's own test databases (in `testdata/`, MIT licensed): all three record sizes, IPv4 and IPv6, every data type, and broken files. The server tests in `src/internal/bp/shipped_plugins_test.go` run it inside BareProxy with the country header, a route chosen by it, allow and block lists, `X-Forwarded-For` from a trusted proxy, a reload and a broken database.

## License

Apache 2.0, like the rest of BareProxy. The test databases are MaxMind's, under the MIT License.
