# Request signing

A BareProxy plugin for two jobs with one kind of signature, HMAC-SHA256 with a key from its config:

- **Signed requests.** It signs each request it lets through, so the app behind can tell the request came through BareProxy and its headers weren't changed on the way. An app that trusts headers the proxy adds (the country from geo rules, a user from a login) needs this when it can be reached any other way.
- **Links that expire.** A private file, a download for a paying customer, a preview for a client: a signed link carries an expiry time and a signature, and works only until then. The plugin checks it before the request goes anywhere and answers 403 to a link that was changed or has run out.

It's released as `signing.wasm` with each BareProxy release, from 0.8.0 on.

## Use it

```
plugin sign /etc/bareproxy/plugins/signing.wasm
  config /etc/bareproxy/plugins/sign.conf

site example.com
  use geo sign
  route /private/* -> files /var/www/private
  route /* -> app
```

List it last on the site, after plugins that add headers, so it signs the request as the app will get it.

## Config

```
# sign.conf
key app 3f9c2a7e5b1d4c8f9a0b6e2d7c4f1a8b3e5d9c0f2a7b6e1d4c8f3a9b2e7d5c1f
key old 8a1b3c5d7e9f0a2b4c6d8e0f1a3b5c7d9e1f3a5b7c9d1e3f5a7b9c1d3e5f7a9b

sign key app
sign-headers x-country x-user

links key app old
link-path /private/ /downloads/
```

| Setting | What it does |
|---|---|
| `key NAME SECRET` | A key. The secret is at least 32 characters; `openssl rand -hex 32` makes one |
| `sign key NAME` | Sign the requests that go on, with this key |
| `sign-headers NAME...` | Request headers the signature covers, besides the method and path |
| `links key NAME...` | The keys a signed link may be made with. Two while you change keys: links made with the old one keep working until you take it off the line |
| `link-path PREFIX...` | The paths that need a signed link |
| `link-params EXP SIG` | The query parameters a link uses; `expires` and `sig` by default |

Changing a key is a change of the config file: `plan` lists it and `rollback` brings the old file back. BareProxy keeps a copy of each plugin config file in its state folder for that, so keep the state folder as private as the config.

## Signed requests

Each request that goes on gets four headers; the same headers from a client are removed first, so they can't be faked:

| Header | Value |
|---|---|
| `X-BareProxy-Timestamp` | Seconds since 1970 when it was signed |
| `X-BareProxy-Path` | The path and query as the request arrived. The app may see another path, after BareProxy cleans it up or a route strips a prefix, so the signature covers this one |
| `X-BareProxy-Signed-Headers` | The `sign-headers`, comma-separated (only when there are some) |
| `X-BareProxy-Signature` | `v1=` and the signature, base64url without padding |

The signature is HMAC-SHA256, keyed with the secret, over these lines joined by `\n`: `v1`, the timestamp, the method, the path, then one `name:value` line for each signed header (an empty value when the request has none).

Checking it in the app, in Python:

```python
import hmac, hashlib, base64, time

def came_through_bareproxy(headers, method, secret, signed=("x-country", "x-user")):
    ts = headers.get("X-BareProxy-Timestamp", "")
    if not ts.isdigit() or abs(time.time() - int(ts)) > 300:
        return False
    lines = ["v1", ts, method, headers.get("X-BareProxy-Path", "")]
    lines += [f"{h}:{headers.get(h, '')}" for h in signed]
    mac = hmac.new(secret.encode(), "\n".join(lines).encode(), hashlib.sha256).digest()
    want = "v1=" + base64.urlsafe_b64encode(mac).rstrip(b"=").decode()
    return hmac.compare_digest(want, headers.get("X-BareProxy-Signature", ""))
```

In Go:

```go
func cameThroughBareProxy(r *http.Request, secret string, signed []string) bool {
	ts := r.Header.Get("X-BareProxy-Timestamp")
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.Unix(n, 0)).Abs() > 5*time.Minute {
		return false
	}
	lines := []string{"v1", ts, r.Method, r.Header.Get("X-BareProxy-Path")}
	for _, h := range signed {
		lines = append(lines, h+":"+r.Header.Get(h))
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(strings.Join(lines, "\n")))
	want := "v1=" + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	return hmac.Equal([]byte(want), []byte(r.Header.Get("X-BareProxy-Signature")))
}
```

The five-minute window keeps an old signed request from being replayed later; the request body isn't signed.

## Signed links

A link is the path with two query parameters: `expires`, seconds since 1970, and `sig`, the HMAC-SHA256 of `v1`, the expiry and the path, joined by `\n`, in base64url without padding. Other query parameters may come along; they aren't signed.

Making one in a shell:

```sh
KEY=3f9c2a7e...   # the secret from the config
path=/private/report.pdf
exp=$(( $(date +%s) + 86400 ))   # a day from now
sig=$(printf 'v1\n%s\n%s' "$exp" "$path" | openssl dgst -sha256 -hmac "$KEY" -binary | base64 | tr '+/' '-_' | tr -d '=')
echo "https://example.com$path?expires=$exp&sig=$sig"
```

In Python:

```python
import hmac, hashlib, base64, time

def signed_link(secret, path, seconds=86400):
    exp = int(time.time()) + seconds
    mac = hmac.new(secret.encode(), f"v1\n{exp}\n{path}".encode(), hashlib.sha256).digest()
    return f"{path}?expires={exp}&sig={base64.urlsafe_b64encode(mac).rstrip(b'=').decode()}"
```

A link that's missing, changed, made with a key not on the `links` line, or past its time gets 403 and a short message. The request's record says which: "signed link refused: the link expired 2h ago".

## How it works

Signatures are compared in constant time, so the time an answer takes says nothing about how much of a guessed signature was right. HMAC and SHA-256 come from the RustCrypto `hmac` and `sha2` crates, vendored in `plugins/vendor/` like the other crates. The plugin makes no outside calls and keeps nothing.

## Build and test

`plugins/build.sh` builds it with the other plugins. `cargo test -p signing` runs its unit tests, among them RFC 4231's HMAC test vector and the exact link the recipes above make. The server tests in `src/internal/bp/shipped_plugins_test.go` run it inside BareProxy and check its signatures with Go's own `crypto/hmac`, as an app would: a signed request through a route that strips a prefix, a forged signature header replaced, good links, links made with the old key, and changed, foreign and expired links refused.

## License

Apache 2.0, like the rest of BareProxy.
