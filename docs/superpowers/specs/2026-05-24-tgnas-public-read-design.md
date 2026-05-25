# TgNAS Public Read, Trusted Proxy, and Presigned URL Design

This design defines three related S3 compatibility improvements for TgNAS:

1. Bucket-level public read for anonymous S3 object downloads.
2. Trusted reverse proxy handling so SigV4 verification can use the external host/proto seen by clients behind cloudflared or similar proxies.
3. SigV4 query-string authentication for object `GET` and `HEAD` presigned URLs.

Public read and trusted proxy handling are opt-in. Presigned URL support preserves existing credential-based authentication and does not add a separate configuration switch.

## Goals

- Allow configured buckets to expose anonymous S3 object `GET` and `HEAD` requests.
- Keep bucket listing, object listing, writes, deletes, and WebDAV authenticated.
- Allow SigV4 requests signed for a public proxy endpoint to verify after the proxy forwards them to TgNAS with a different origin `Host`.
- Allow S3 clients to use presigned URLs for object downloads and metadata checks.
- Make all trust boundaries explicit in YAML config.

## Non-Goals

- No public ListBuckets or ListObjects support.
- No public writes, deletes, copy, or bucket operations.
- No WebDAV anonymous access.
- No presigned URL support for writes, deletes, bucket operations, listing, or WebDAV.
- No virtual-hosted-style bucket routing.
- No multipart upload support.
- No automatic trust of arbitrary proxy headers without configuration.

## Public Read Design

### Configuration

Public read is configured per bucket:

```yaml
buckets:
  public-files:
    chat_id: "${TGNAS_TELEGRAM_CHAT_ID}"
    public_read: true
```

`public_read` defaults to `false` when omitted.

### Authentication boundary

Anonymous access is allowed only when all of these conditions are true:

- The request has no SigV4 authentication headers.
- The request has no SigV4 query authentication parameters.
- The method is `GET` or `HEAD`.
- The request path addresses a specific object key, not the root or bucket path.
- The bucket is configured with `public_read: true`.

All other requests continue through normal SigV4 verification, including presigned URL verification when SigV4 query authentication parameters are present.

### Allowed anonymous operations

- `GET /{bucket}/{key}` for public buckets.
- `HEAD /{bucket}/{key}` for public buckets.

Object metadata headers and range behavior reuse the existing authenticated object paths.

### Disallowed anonymous operations

These still require SigV4:

- `GET /` ListBuckets.
- `GET /{bucket}` ListObjects/ListObjectsV2.
- `PUT /{bucket}` bucket create compatibility path.
- `HEAD /{bucket}` bucket existence check.
- `DELETE /{bucket}` bucket cleanup path.
- `PUT /{bucket}/{key}` object writes.
- `DELETE /{bucket}/{key}` object deletes.
- Any WebDAV request.

### Implementation shape

- Add `PublicRead bool` to `config.BucketConfig` with YAML key `public_read`.
- Build a `map[string]bool` of public buckets in `cmd/tgnas`.
- Pass the map to `s3api.Options.PublicReadBuckets`.
- Copy the map into `s3api.Server` at construction time.
- Add a small predicate in `internal/s3api.Server` to detect anonymous public object reads.
- In `ServeHTTP`, bypass SigV4 only when that predicate returns true.

### Security properties

- Public read is opt-in per bucket.
- Anonymous clients must already know object keys.
- Public read does not expose bucket names or object listings.
- Existing authenticated behavior remains unchanged for private buckets.

## Trusted Proxy Design

### Problem statement

SigV4 includes the `host` header in the canonical request. If a client signs a request for `https://s3.example.com/` and cloudflared forwards it to TgNAS as `http://127.0.0.1:9000/`, TgNAS sees a different `Host`. The canonical request changes and SigV4 verification fails with `SignatureDoesNotMatch`.

TgNAS needs an opt-in trust mechanism that restores the external request host, and optionally scheme, before S3/WebDAV routing.

### Configuration

```yaml
server:
  trusted_proxies:
    - "127.0.0.1/32"
    - "::1/128"
    - "172.16.0.0/12"
  trusted_proxy_hosts:
    - "s3.example.com"
```

- `trusted_proxies` is a list of IPv4/IPv6 CIDR prefixes.
- `trusted_proxy_hosts` is a list of exact forwarded host values.
- Both lists are optional.
- If both lists are empty, trusted proxy handling is disabled.

### Trust model

Forwarded headers are trusted when **either** condition matches:

1. `RemoteAddr` is inside one of `server.trusted_proxies`.
2. The forwarded host is in `server.trusted_proxy_hosts`.

This is an OR relationship.

### Important consequence

If `trusted_proxies` matches, TgNAS accepts **any** forwarded host from that remote IP. This is intentional for proxy/tunnel deployments where the source IP identifies the trusted proxy. The deployment must prevent untrusted clients from directly reaching TgNAS from those source ranges.

If `trusted_proxy_hosts` matches without a proxy CIDR match, TgNAS trusts only that matching forwarded host. This is useful when source IPs are not stable, but it is weaker if TgNAS is directly reachable by untrusted clients because forwarded headers are client-controlled unless a proxy strips and replaces them.

### Header reading

External host is read in this order:

1. `X-Forwarded-Host`
2. `Forwarded: host=...`

External proto is read in this order:

1. `X-Forwarded-Proto`
2. `Forwarded: proto=...`

Rules:

- Only the first comma-separated value is used.
- Leading and trailing quotes are stripped.
- Host comparisons are case-insensitive.
- Proto values are lowercased before use.

### Request rewriting

When trust matches and either a forwarded host or forwarded proto exists, TgNAS clones the request and rewrites the fields represented by the trusted headers:

- `r.Host`, if forwarded host exists.
- `r.URL.Host`, if forwarded host exists.
- `r.URL.Scheme`, if forwarded proto exists.

The cloned request then enters the selected handler:

- S3-only mode.
- WebDAV-only mode.
- Combined S3/WebDAV mode.

The original request is not mutated.

### SigV4 interaction

`internal/s3api` SigV4 verification remains unchanged. It continues to read `r.Host` through the existing canonical header path. The middleware ensures that trusted proxied requests arrive at the S3 handler with the external host already restored.

### Validation

- Each `trusted_proxies` entry must parse as a CIDR prefix.
- Each `trusted_proxy_hosts` entry must be non-empty after trimming whitespace.
- No wildcard or suffix host matching is supported.

### Security boundaries

Recommended safe deployments:

- Use `trusted_proxies` when TgNAS is reachable only from stable proxy/tunnel/container CIDRs.
- Use `trusted_proxy_hosts` when source IPs are unstable but allowed external hostnames are narrow.
- Avoid exposing TgNAS directly to untrusted networks when relying on forwarded headers.

Spoofing risks:

- `trusted_proxy_hosts` can be spoofed by direct clients unless a trusted proxy strips incoming forwarded headers.
- Over-broad `trusted_proxies` accepts arbitrary forwarded hosts from those sources.

### WebDAV interaction

WebDAV authentication remains Basic Auth and unchanged. Because the middleware runs before routing, WebDAV absolute `Destination` host checks see the trusted external host when trust matches. That keeps S3 and WebDAV host identity consistent behind the proxy.

## Presigned URL Design

### Scope

TgNAS supports SigV4 query-string authentication for object reads only:

- `GET /{bucket}/{key}?X-Amz-*`
- `HEAD /{bucket}/{key}?X-Amz-*`

Presigned URLs do not authorize:

- `GET /` ListBuckets.
- `GET /{bucket}` ListObjects/ListObjectsV2.
- `PUT`, `DELETE`, or any other write/delete operation.
- WebDAV requests.

### Required query parameters

A presigned URL must include these query parameters:

- `X-Amz-Algorithm=AWS4-HMAC-SHA256`
- `X-Amz-Credential=<access-key>/<date>/<region>/s3/aws4_request`
- `X-Amz-Date=<YYYYMMDDTHHMMSSZ>`
- `X-Amz-Expires=<seconds>`
- `X-Amz-SignedHeaders=<semicolon-separated-list>`
- `X-Amz-Signature=<hex-signature>`

The region must match `auth.region`, the service must be `s3`, and the access key must exist in the configured credentials.

### Expiration rules

`X-Amz-Expires` must be a positive integer number of seconds and cannot exceed `604800` seconds (7 days).

A presigned request is valid only when the verifier clock is in this interval:

```text
X-Amz-Date <= now <= X-Amz-Date + X-Amz-Expires
```

TgNAS may continue using the existing `SigV4MaxSkew` setting for small future clock skew tolerance, but expired URLs must fail once `now` is after `X-Amz-Date + X-Amz-Expires`.

### Canonical request rules

Presigned URL verification reuses the existing SigV4 canonical request implementation with one query-specific change: canonical query string construction must exclude `X-Amz-Signature` itself. All other query parameters remain signed, including S3 response override parameters such as `response-content-disposition`.

For presigned `GET` and `HEAD`, the payload hash is `UNSIGNED-PAYLOAD`. The request does not need an `X-Amz-Content-Sha256` header.

If a client modifies any signed query parameter, such as `response-content-disposition`, verification fails with `SignatureDoesNotMatch`.

### Verifier structure

`internal/s3api.SigV4Verifier.Verify` should support both authentication forms:

1. Header SigV4, using the existing `Authorization` header parser.
2. Query SigV4, using the `X-Amz-*` query parameters when no `Authorization` header is present.

Both paths should share credential lookup, region/service validation, canonical request construction, signing key derivation, and constant-time signature comparison.

### Server authorization boundary

The S3 server continues to call the verifier before normal routing. When query-string authentication is used, the authenticated request is allowed to continue only if it targets an object with method `GET` or `HEAD`.

Requests with presigned query parameters do not use the public-read anonymous bypass. Public read remains limited to requests with neither SigV4 headers nor SigV4 query parameters.

Unsupported presigned operations fail as authorization errors instead of falling back to anonymous public-read behavior.

### Trusted proxy interaction

Trusted proxy rewriting still runs before S3 verification. Presigned URLs signed for an external proxy host verify successfully when the trusted proxy middleware restores `r.Host` before `SigV4Verifier.Verify` builds the canonical request.

## Combined Behavior

Trusted proxy rewriting happens before S3/WebDAV routing and before public-read or SigV4 authorization decisions. Public-read logic still only bypasses SigV4 for anonymous object `GET`/`HEAD` requests in public buckets.

For authenticated S3 requests behind a proxy:

1. The trusted proxy middleware may restore external host/proto.
2. S3 SigV4 verification sees the restored host.
3. Normal authenticated routing continues.

For presigned S3 object read requests behind a proxy:

1. The trusted proxy middleware may restore external host/proto.
2. S3 SigV4 query verification sees the restored host.
3. Only object `GET`/`HEAD` requests continue after query authentication succeeds.

For anonymous public-read requests behind a proxy:

1. The trusted proxy middleware may restore external host/proto.
2. Public-read authorization checks method, path, absence of SigV4 headers and query auth parameters, and bucket config.
3. Object `GET`/`HEAD` proceeds without SigV4 only if the bucket is public.

## Testing Strategy

### Public read tests

- Config parser accepts `public_read: true` and defaults omitted values to false.
- Anonymous `GET` and `HEAD` work for public bucket objects.
- Anonymous object reads fail for private buckets.
- Anonymous root listing and bucket listing fail.
- Anonymous writes and deletes fail.
- Config-to-server wiring maps only public buckets.

### Trusted proxy tests

- Config parser accepts valid `trusted_proxies` and `trusted_proxy_hosts`.
- Invalid CIDR entries fail validation.
- Empty trusted proxy host entries fail validation.
- Middleware trusts `X-Forwarded-Host` when remote IP matches.
- Middleware accepts any forwarded host when remote IP matches.
- Middleware trusts a forwarded host that matches `trusted_proxy_hosts` when remote IP does not match.
- Middleware leaves the request unchanged when neither condition matches.
- Middleware reads `Forwarded: host=...;proto=...` fallback.
- Middleware uses the first comma-separated forwarded host value.
- A SigV4 request signed for the external host verifies after middleware rewriting.
- Existing S3/DAV service routing tests still pass.

### Presigned URL tests

- AWS SDK generated presigned `GET` for an object verifies and returns the object.
- AWS SDK generated presigned `HEAD` for an object verifies and returns object metadata.
- S3 response override query parameters such as `response-content-disposition` participate in the signature and succeed when unmodified.
- Modifying any signed query parameter fails with `SignatureDoesNotMatch`.
- Missing required `X-Amz-*` query parameters fail.
- Expired presigned URLs fail.
- `X-Amz-Expires` values greater than `604800` fail.
- Presigned `PUT`, `DELETE`, root listing, and bucket listing fail.
- Presigned URLs signed for an external host verify after trusted proxy rewriting.

## Documentation Requirements

README and sample config should document:

- `public_read: true` per bucket.
- Exact anonymous access scope for public read.
- Presigned URL support for object `GET` and `HEAD` only.
- `X-Amz-Expires` maximum of 7 days.
- `trusted_proxies` and `trusted_proxy_hosts` examples.
- The OR trust condition.
- The rule that proxy CIDR matches accept any forwarded host.
- The spoofing risks of forwarded headers.

## Rollout

Public read and trusted proxy handling are disabled by default:

- `public_read` defaults to false per bucket.
- trusted proxy lists default to empty.

Presigned URL support is available to existing configured credentials and does not expose new anonymous access. Existing deployments keep their current behavior unless operators explicitly opt in to public buckets or trusted proxy headers.
