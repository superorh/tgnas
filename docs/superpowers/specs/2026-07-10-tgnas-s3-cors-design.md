# TgNAS S3 CORS Design

## Goal

Allow a frontend website hosted on a different origin to read images stored in TgNAS through the S3-compatible API (e.g. `<img>` tag, `fetch()`, or presigned URLs) without the browser rejecting the response due to CORS. CORS request handling applies only to `internal/s3api`; configuration and startup wiring may change, but WebDAV behavior must remain unaffected.

## Constraints

- CORS HTTP behavior is implemented only in `internal/s3api`. Supporting configuration and startup wiring may change in `config` and `cmd/tgnas`; `internal/dav` and WebDAV behavior remain unchanged.
- Configuration must live on the existing `server:` section for global policy and directly on `buckets.<name>:` (no extra wrapping section, e.g. no `cors:`).
- A `buckets.<name>.allowed_origins` list that is absent, empty, or contains only empty strings is treated as "no bucket-level override", and matching falls back to the global `server.allowed_origins`. A non-empty bucket-level list fully replaces the global list for that bucket: if the request origin does not match the bucket list, the global list is not consulted.
- CORS support is opt-in: when no allowed origin patterns are configured, the server must behave exactly as today (no `Access-Control-*` headers, no `OPTIONS` short-circuit).
- An origin pattern can be:
  - exact (default), case-insensitive
  - glob, supporting `*` as a wildcard, compiled once at startup, case-insensitive
  - regex, wrapped in leading/trailing `/`, Go RE2 syntax, forced case-insensitive at compile time
- Startup fails fast on CORS configuration: every allowed-origin pattern is compiled before any database, Telegram binding, or object-store work begins. Invalid patterns surface as a clean startup error before `metadata.OpenSQLite`, bucket upserts, the object store, and the listener are constructed, so no persistent state is written when CORS is invalid.
- A request is a CORS preflight only when it uses `OPTIONS` and includes both a non-empty `Origin` and a non-empty `Access-Control-Request-Method`. Recognized preflights must short-circuit before the SigV4 verifier; `OPTIONS` requests missing either header continue through the existing request path.
- `Access-Control-Allow-Credentials` is never emitted. Anonymous public reads and presigned URLs can succeed without it, and avoiding credentials keeps the implementation compatible with explicit-Origin matching.
- Each response processed by an enabled CORS policy (allowed or denied) includes `Vary: Origin`. Every recognized preflight response additionally varies on `Access-Control-Request-Method` and `Access-Control-Request-Headers` so caches do not reuse negotiation results across methods or header sets. CORS dimensions are appended without replacing existing `Vary` values and are deduplicated case-insensitively across all comma-separated header values.
- Methods exposed through `Access-Control-Allow-Methods`, and accepted from `Access-Control-Request-Method` using a case-insensitive comparison, are `GET, HEAD, PUT, POST, DELETE, OPTIONS`.
- Headers exposed through `Access-Control-Allow-Headers`, and accepted from comma-separated `Access-Control-Request-Headers` using trimmed, case-insensitive comparisons, are `Authorization, Content-Type, Range, X-Amz-Date, X-Amz-Content-Sha256`. Every requested header must be allowed.
- Successful preflight responses emit a fixed `Access-Control-Max-Age: 3600`; max-age is intentionally not configurable.
- Headers exposed via `Access-Control-Expose-Headers` must cover what frontend JS reads: `ETag, Content-Range, Content-Length, Accept-Ranges, Last-Modified`.

## Problem Summary

The current `internal/s3api` does not emit any `Access-Control-*` response header and treats `OPTIONS` like any other request, which means SigV4 verification rejects preflight with `403`. A frontend hosted on a different origin either fails the preflight or, for simple `GET`s, can read the response body only as opaque without being able to consume it in JS. The user-facing symptom is the well-known "blocked by CORS policy" error.

We need an opt-in middleware on the S3 API entrypoint that:
- For requests with an `Origin` header, decides whether that origin is allowed against configured patterns.
- For recognized preflight requests, validates the origin, requested method, and every requested header; valid negotiations return `204` and bypass SigV4, while any failed check returns `403` without negotiation headers.
- For actual requests from an allowed origin, decorates the response with `Access-Control-Allow-Origin`, `Access-Control-Expose-Headers`, and `Vary: Origin`.

## Options Considered

### 1. Inline CORS handling inside `Server.ServeHTTP`

Keep everything in the existing `server.go`: a helper that decides whether `r.Header.Get("Origin")` matches a configured pattern, sets the response headers, and short-circuits `OPTIONS` preflights.

Pros:
- Smallest diff, no new package surface
- Easy to reason about ordering vs SigV4
- Reuses existing path parsing

Cons:
- Pattern parsing and matching logic mixes with HTTP routing
- Hard to test the matcher in isolation

### 2. Dedicated `internal/s3api/cors.go` package surface

A new file `internal/s3api/cors.go` (same package) holds `Origins`, `Pattern`, `Matcher`, and a `Handle(handler http.Handler)`-style middleware function. `server.go` wires it in.

Pros:
- `Pattern`/`Matcher` are independently unit-testable
- Future configuration concerns (e.g. per-bucket headers, allowed credentials) have a clear home
- No change to public package layout — same package, just a new file

Cons:
- Slightly more files to navigate

### 3. Use a third-party CORS middleware (rs/cors or gorilla/handlers)

Wire a battle-tested CORS middleware.

Pros:
- Less code to own

Cons:
- Tighter pattern configuration than the three-mode matcher we want (exact/glob/regex)
- `*`-support combined with case insensitivity must still be reproduced
- The compiler error semantics for bad regexes need a custom layer anyway

## Recommended Approach

Use option 2. Keep the change local and add a focused file `internal/s3api/cors.go` that owns:
- the pattern parser (exact / glob / regex)
- the matcher against a request origin
- the middleware that decorates responses and short-circuits preflights

`internal/s3api/server.go` only learns about CORS by holding a small struct field and inserting one helper call at the right place in `ServeHTTP`.

`config/config.go` adds `AllowedOrigins` to `ServerConfig` and `BucketConfig` (raw strings, no semicolon interpretation at this layer). Compiling them into matchers happens in `s3api`.

## Architecture

```
              ┌─────────────────────────────────┐
              │  Server/Bucket AllowedOrigins   │  raw strings
              └──────────────┬──────────────────┘
                             │ LoadFile
                             ▼
              ┌─────────────────────────────────┐
              │  config.Config (with strings)   │
              └──────────────┬──────────────────┘
                             │ collect raw global/bucket rules
                             ▼
              ┌─────────────────────────────────┐
              │  s3api.CompileCORSPolicy        │  before side effects
              └──────────────┬──────────────────┘
                             │ success
                             ▼
              ┌─────────────────────────────────┐
              │  *s3api.CORSPolicy              │  immutable, precompiled
              └──────────────┬──────────────────┘
                             │ only now initialize DB/Telegram/store
                             ▼
              ┌─────────────────────────────────┐
              │  s3api.Options.CORS             │
              └──────────────┬──────────────────┘
                             │ NewServer stores prepared policy
                             ▼
              ┌─────────────────────────────────┐
              │  Server CORS policy             │
              └──────────────┬──────────────────┘
                             │ middleware call from ServeHTTP
                             ▼
        ┌────────────────────────────────────────────┐
        │  decideOrigin(req) → allowed bool             │
        │  validatePreflight(req) → allowed bool        │
        │  writePreflight(origin, method, headers)      │
        │    → 204/403                                  │
        │  decorateActual(origin) → headers             │
        └────────────────────────────────────────────┘
```

The decision tree inside `ServeHTTP` becomes:

1. `/healthz`, `/readyz` first (no CORS).
2. CORS middleware: select the effective matcher for the current bucket (or the global matcher for non-bucket paths). If that matcher has no rules, skip CORS entirely and continue unchanged. Otherwise, add `Vary: Origin` before inspecting the request origin; if `Origin` is absent, continue through the existing SigV4 / anonymous public read / routing path without any `Access-Control-*` header.
3. If the request uses `OPTIONS` and has both a non-empty `Origin` and a non-empty `Access-Control-Request-Method`, treat it as preflight: append all three `Vary` dimensions, validate origin, requested method, and requested headers, then return `204` on success or `403` on failure. It never reaches SigV4.
4. Otherwise, treat the request as an actual CORS request: decorate it according to the origin decision and continue through SigV4 (or anonymous public read).
5. Continue normal routing.

## Components

### Config layer

- `config/config.go`:
  - `ServerConfig.AllowedOrigins []string` (yaml: `allowed_origins`)
  - `BucketConfig.AllowedOrigins []string` (yaml: `allowed_origins`)
  - No validation in this layer beyond "non-empty string preserved".

### CORS module

- New file `internal/s3api/cors.go` in package `s3api` containing:
  - `type OriginRule struct { kind ruleKind; regex *regexp.Regexp }`
    - `ruleKind` is one of `ruleExact`, `ruleGlob`, `ruleRegex`.
  - `type OriginMatcher struct { rules []OriginRule }`
  - `type CORSPolicy struct { global OriginMatcher; buckets map[string]OriginMatcher }` — exported as an opaque, immutable prepared policy; its fields remain unexported and are never mutated after construction.
  - `CompileCORSPolicy(global []string, buckets map[string][]string) (*CORSPolicy, error)` — compiles the global matcher and every non-empty bucket override eagerly. Empty entries are discarded before deciding whether a bucket has an override. Global failures include global-policy context; bucket failures include the bucket name. Any failure returns `(nil, error)`, never a partially prepared policy. An empty configuration returns a valid policy with no effective rules.
  - `(m *OriginMatcher) compile(spec []string) error` — converts raw strings into prebuilt regexes and returns an error on invalid regex syntax. For every entry it first executes the equivalent of `if s == "" { continue }`; no indexing or delimiter inspection occurs before this empty-entry check.
  - `(m *OriginMatcher) allow(origin string) bool` — case-insensitive, tests against any rule.
  - Pattern recognition (evaluated only after the empty-entry check above):
    - `len(s) >= 2 && s[0] == '/' && s[len(s)-1] == '/'` → ruleRegex, body is `s[1:len(s)-1]`, compiled as `(?i)<body>`. Only the first and last `/` are treated as delimiters; any `/` inside the body is passed to the Go RE2 compiler as an ordinary character and does not need escaping.
    - else if `strings.Contains(s, "*")` → ruleGlob: split the raw pattern on every `*`, escape each resulting literal segment with `regexp.QuoteMeta`, join the escaped segments with `.*`, and compile `(?i)^<joined>$`. Glob syntax has no escape mechanism: every `*`, including consecutive `*` characters, is an independent wildcard. A pattern consisting of the single character `*` is valid and behaves as "match any origin".
    - else → ruleExact: a compiled regex of `(?i)^<escaped literal>$`.
  - `addVary(header http.Header, names ...string)` — scans every existing comma-separated `Vary` token case-insensitively, preserves all existing values, and appends each requested dimension only when it is absent. Every response under an effective CORS policy uses this helper for `Origin`, even when the request omits `Origin`; recognized preflight responses also use it for the two request negotiation dimensions, so each CORS dimension appears at most once.
  - `originAllowed(w, r, matcher)` — is called only after `handleCORS` has added `Vary: Origin` and confirmed a non-empty `Origin`; when the origin is allowed, it sets `Access-Control-Allow-Origin` and `Access-Control-Expose-Headers` before the request reaches SigV4 or routing so successful and error responses retain both headers.
  - `const corsPreflightMaxAge = "3600"` — fixed one-hour browser cache lifetime; this is intentionally not configurable.
  - `corsPreflight(w, r, matcher)` — handles only recognized preflight requests. Before calling it, the caller must verify `r.Method == http.MethodOptions`, a non-empty `Origin`, and a non-empty `Access-Control-Request-Method`. It adds `Origin`, `Access-Control-Request-Method`, and `Access-Control-Request-Headers` through `addVary`; compares the requested method against the allowed method set; splits `Access-Control-Request-Headers` on commas, trims each name, and compares every non-empty name case-insensitively against the allowed header set. It returns `204` with `Access-Control-Allow-Origin`, `Access-Control-Allow-Methods`, `Access-Control-Allow-Headers`, and `Access-Control-Max-Age: 3600` only when the origin, method, and all headers are allowed; otherwise it returns `403` without those negotiation headers.

### Wire-in

- `cmd/tgnas/main.go`:
  - Add `compileCORSPolicyFromConfig(cfg config.Config) (*s3api.CORSPolicy, error)` as the single raw-config mapping path. It builds a raw bucket-rule map containing every configured bucket and calls `s3api.CompileCORSPolicy(cfg.Server.AllowedOrigins, bucketRules)`; it performs no I/O.
  - Before any database, Telegram binding, or object-store work, `runServiceWithDebug` calls `compileCORSPolicyFromConfig` and propagates any compilation error as a startup failure. This ordering ensures invalid CORS never opens SQLite, never writes bucket metadata, and never constructs the Telegram-backed object store.
  - Add `s3OptionsFromConfig(cfg config.Config, credentials map[string]string, ready func() bool, logger *log.Logger, corsPolicy *s3api.CORSPolicy) s3api.Options` as the single production path for constructing S3 options. It no longer handles allowed-origin mapping; it just copies Region, Credentials, PublicRead, the readiness probe, the logger, and the already-compiled `corsPolicy` into `Options`.
  - `runServiceWithDebug` passes the helper result to `s3api.NewServer`. `NewServer` does not return an error: every CORS-related failure is caught by `CompileCORSPolicy` upstream.
- `internal/s3api/server.go`:
  - `Options` replaces `CORSAllowedOrigins` and `BucketCORSAllowedOrigins` with a single `CORS *CORSPolicy`. A `nil` policy keeps today's behavior; an empty policy compiled from empty rules also behaves as if CORS were disabled. Preflight max-age is not part of `Options`; successful preflights always use the fixed value `3600`.
  - `NewServer` keeps its original signature `func NewServer(objectStore ObjectStore, options Options) http.Handler`. It stores the prepared `*CORSPolicy` on `*Server` (no goroutines, no shared state) and never recompiles patterns.
  - `ServeHTTP` gains a `s.handleCORS(w, r)` call after the `/readyz` branch and before the `SigV4` branch. The helper:
    - pulls the current bucket name via the existing `splitPath` helper (cheap; no metadata call)
    - chooses the global matcher for bucket-less paths (`/`, including `OPTIONS /`)
    - for bucket paths, uses a non-empty bucket matcher exclusively; when the bucket list is absent, empty, or contains only empty strings, uses the global matcher instead
    - if the effective matcher has no rules, returns without changing headers; otherwise adds `Vary: Origin` through `addVary` before checking `Origin`
    - if `Origin` is empty after `Vary` is added, lets the request continue without any `Access-Control-*` header
    - delegates to `corsPreflight` only when `r.Method == http.MethodOptions`, `Origin` is non-empty, and `Access-Control-Request-Method` is non-empty
    - otherwise calls `originAllowed` to decorate the actual response and lets the request continue, including `OPTIONS` requests that are not recognized preflights

### Order of operations

```
ServeHTTP
├── /healthz / /readyz (no CORS)
├── handleCORS:
│     ├── effective matcher empty → continue unchanged
│     ├── add Vary: Origin
│     ├── Origin absent → continue without Access-Control-* headers
│     ├── OPTIONS + non-empty Origin + non-empty Access-Control-Request-Method → validate preflight, return 204/403
│     └── actual request → decorate origin headers and continue
├── shouldServeHTMLRoot short-circuit
├── SigV4 verification (or anonymous public read)
└── normal routing
```

This guarantees every recognized preflight is resolved before SigV4.

### Response header matrix

| Case | Status | `Access-Control-Allow-Origin` | `Access-Control-Expose-Headers` | `Vary` |
|------|--------|-------------------------------|---------------------------------|--------|
| No effective CORS rules | as-is | not set | not set | unchanged |
| Effective CORS rules, no `Origin` | as-is | not set | not set | `Origin` |
| Actual request, origin not allowed | as-is | not set | not set | `Origin` |
| Actual request, origin allowed | as-is | set to request `Origin` | set | `Origin` |
| Recognized preflight, any origin/method/header check fails | `403` | not set | not set | `Origin`, `Access-Control-Request-Method`, `Access-Control-Request-Headers` |
| Recognized preflight, all checks pass | `204` | set to request `Origin` | not set | `Origin`, `Access-Control-Request-Method`, `Access-Control-Request-Headers` |

Headers emitted on allowed real responses:
- `Access-Control-Expose-Headers: ETag, Content-Range, Content-Length, Accept-Ranges, Last-Modified`

Preflight-only headers emitted on successful `204` responses:
- `Access-Control-Allow-Methods: GET, HEAD, PUT, POST, DELETE, OPTIONS`
- `Access-Control-Allow-Headers: Authorization, Content-Type, Range, X-Amz-Date, X-Amz-Content-Sha256`
- `Access-Control-Max-Age: 3600`

`Vary` values are added through `addVary`: existing tokens are preserved, comparisons are case-insensitive, and each CORS dimension appears at most once across all comma-separated header values. `Access-Control-Allow-Credentials` is never emitted, in any case.

## Glob and regex semantics in detail

- Glob is shell-glob-like but restricted to one wildcard character: every `*` matches any sequence of characters including the empty string. Compilation splits the pattern on `*`, applies `regexp.QuoteMeta` to each literal segment, joins the segments with `.*`, and anchors the result at both ends. There is no glob escape syntax, so every `*` is a wildcard; consecutive stars have no distinct `**` meaning and behave as adjacent independent wildcards. All other regex metacharacters are literals (`.`, `(`, `?`, `+`, `[`, `\`...).
- Regex syntax is Go RE2 (`regexp` standard library). All compiled patterns get a `(?i)` prefix so matching is case-insensitive, because `Origin` schemes and hosts are case-insensitive per RFC 6454. Internal `/` characters are ordinary regex characters and must be written literally in the body; only the leading and trailing `/` are interpreted as delimiters.
- Single-char `*` (the whole pattern is just `*`) is a valid glob and matches any origin. This is opt-in: users who write `*` are declaring "allow any origin". It is accepted without warning and behaves identically to `(?i)^.*$` anchored at both ends.
- Empty strings in config are ignored at compile time (silently dropped) so users can leave trailing comma entries in YAML.

## Failure modes

- Bad regex body at startup → `CompileCORSPolicy` returns `(nil, error)` with global or bucket context. `runServiceWithDebug` calls it before opening SQLite or constructing any Telegram/object-store dependency and propagates the error immediately; no policy, persistent state change, handler, or listener is produced.
- Empty entries in the raw global or bucket rule lists are skipped silently without touching `s[0]` or `s[len(s)-1]`; only non-empty entries reach delimiter inspection.
- Bad regex body at request time (impossible because the server receives only a prepared policy).
- Bad origin header on a request → just no match; `403` for a recognized preflight, normal SigV4 response for an actual request.

## Testing Strategy

Unit (`internal/s3api/cors_test.go`):
- Exact lowercased match.
- Glob `https://*.staging.example.com` matches `https://a.staging.example.com` but not `https://a.staging.example.com.evil`, not `https://a.staging.example.com/path`, and not `https://staging.example.com` (anchored at both ends).
- Consecutive stars in `https://**.example.com` are accepted as adjacent independent wildcards and have no special `**` semantics.
- Non-wildcard regex metacharacters in a glob, such as `https://*.example[dev].com`, are matched literally rather than interpreted as a character class.
- Regex `/^https://[a-z]+\.example\.com$/` matches `https://app.example.com`; uppercase `https://APP.example.com` is also treated as match (case-insensitive), but `https://app.example.com.evil` is not (anchored).
- Single-char `*` glob matches any non-empty origin and any empty origin header (when an empty `Origin` ever reaches the matcher; in practice the middleware skips empty `Origin` values).
- Multiple rules: any-match semantics.
- Empty entries ignored.
- `CompileCORSPolicy` returns `(nil, error)` for an invalid global regex with global-policy context, and for an invalid bucket regex with the bucket name in the error; neither case panics or returns a partial policy.

Integration (`internal/s3api/server_test.go`):
- Empty config → no `Access-Control-*` headers and no CORS `Vary` dimensions; OPTIONS still goes through SigV4 rejection (existing behavior).
- With a global rule matching `https://frontend.example.com`, `GET /healthz` remains `200` with body `ok`; `GET /readyz` remains `200` with body `ready` when ready and `503` with body `not ready` when not ready. None of these responses emit any `Access-Control-*` header or any of the three CORS `Vary` dimensions.
- With the same matching rule, recognized preflight requests to `/healthz` and `/readyz` remain the existing `404` responses rather than becoming `204` or `403`; they emit no `Access-Control-*` header and no CORS `Vary` dimension. Assertions inspect every comma-separated `Vary` token rather than comparing a single raw header value.
- If an outer handler has already set `Vary: Accept-Encoding, origin`, an allowed actual request preserves `Accept-Encoding` and contains exactly one case-insensitive `Origin` token. A recognized preflight preserves the same existing tokens, adds `Access-Control-Request-Method` and `Access-Control-Request-Headers`, and contains each of the three CORS dimensions exactly once across all `Vary` header values.
- Global config matches → a real GET produces `Access-Control-Allow-Origin`, `Access-Control-Expose-Headers`, and `Vary: Origin`; a recognized preflight with an allowed method and headers returns `204`, emits the negotiation headers including `Access-Control-Max-Age: 3600` and all three `Vary` dimensions, and omits `Access-Control-Expose-Headers`.
- Global config does not match → real responses omit `Access-Control-Allow-Origin` and `Access-Control-Expose-Headers`; recognized preflight returns `403` without negotiation headers and includes all three `Vary` dimensions.
- A recognized preflight requesting unsupported method `PATCH` returns `403` without `Access-Control-Allow-Origin`, `Access-Control-Allow-Methods`, `Access-Control-Allow-Headers`, or `Access-Control-Max-Age`.
- A recognized preflight requesting unsupported header `X-Custom` returns the same `403` response.
- Requested methods and headers are matched case-insensitively; comma-separated mixed-case headers such as `authorization, Content-Type, range, X-Amz-Date` succeed after trimming.
- An `OPTIONS` request with `Origin` but without `Access-Control-Request-Method` is not recognized as preflight: it receives actual-request origin decoration when allowed and continues through SigV4 instead of short-circuiting.
- An allowed-origin real request that fails SigV4 still returns the original authentication status and retains `Access-Control-Allow-Origin`, `Access-Control-Expose-Headers`, and `Vary: Origin`.
- An allowed-origin real request that reaches routing but fails with an S3 business error retains the same three CORS headers.
- A non-empty bucket override replaces the global matcher: an origin allowed only by the global list is denied for that bucket.
- A bucket override list that is absent, empty, or contains only empty strings falls back to the global matcher.
- No `Origin` header on a path with effective CORS rules → no `Access-Control-*` headers, but existing `Vary` values are preserved and `Origin` is added exactly once; OPTIONS without `Origin` does not short-circuit and continues through SigV4.
- Existing SigV4 anonymous public read path is unaffected.
- A handler built with `NewServer(store, Options{CORS: nil})` behaves exactly as today; a handler built with `NewServer(store, Options{CORS: policy})` where `policy` was produced by `CompileCORSPolicy` behaves according to the policy. The compile failure tests live in `cors_test.go` against `CompileCORSPolicy` rather than against `NewServer`.

Config (`config/config_test.go`):
- `ServerConfig.AllowedOrigins` parses YAML.
- `BucketConfig.AllowedOrigins` parses YAML.
- Default empty value works.

Production wire-in (`cmd/tgnas/main_test.go`):
- Build a `config.Config` with global origin `https://global.example.com`, bucket `override` allowing only `https://bucket.example.com`, bucket `fallback-empty` with an empty list, and bucket `fallback-blanks` with `[]string{"", ""}`. Produce the policy through the production `compileCORSPolicyFromConfig` helper, not by rebuilding the bucket map in the test.
- Construct the real handler with `s3api.NewServer(objectStore, s3OptionsFromConfig(..., corsPolicy))`; use a fake object store that records any unexpected call. Recognized preflights must short-circuit before the store.
- `OPTIONS /override/key` with `https://bucket.example.com` returns `204`, while the same path with `https://global.example.com` returns `403`, proving the prepared bucket matcher replaces the global matcher.
- `OPTIONS /fallback-empty/key` and `/fallback-blanks/key` with `https://global.example.com` return `204`, proving empty and empty-only lists are omitted from the prepared bucket map and fall back to global.
- `OPTIONS /` with `https://global.example.com` returns `204`, proving bucket-less paths use the prepared global matcher.
- Every request includes a non-empty `Origin` and `Access-Control-Request-Method: GET`; policy compilation failures fail the test immediately.
- Separately exercise the actual `runServiceWithDebug` path with otherwise valid temporary YAML in two table cases: an invalid global regex in `server.allowed_origins`, and an invalid bucket regex in `buckets.<name>.allowed_origins`. Stub `listenAndServe` with a sentinel and configure a fresh SQLite path that does not exist before the call.
- In both invalid-regex cases, `runServiceWithDebug` returns contextual CORS policy compilation error before `listenAndServe` is called, and the SQLite file remains absent, proving compilation happened before `metadata.OpenSQLite` or any metadata write. If the existing object-store factory test hook is available, also assert it was not called.
- The global case proves the early production compiler receives `server.allowed_origins`; the bucket case proves it receives every bucket's raw `allowed_origins`.

## Non-Goals

- No `Access-Control-Allow-Credentials` support.
- No WebDAV changes.
- No dynamic per-route CORS policy.
- No glob operators beyond `*`: `?` and `[abc]` are literals, and `**` has no special recursive meaning beyond two adjacent `*` wildcards.
- No origin allow/deny telemetry beyond what's already in the S3 logger.

## Migration Notes

- `NewServer` keeps its existing `func NewServer(objectStore ObjectStore, options Options) http.Handler` signature; no repository-wide return-value migration is needed.
- `s3api.Options` gains `CORS *CORSPolicy`. Existing callers that do not configure CORS remain source-compatible because the zero value is `nil` and disables CORS.
- `cmd/tgnas/main.go` maps raw global and bucket rules through `compileCORSPolicyFromConfig` before opening SQLite or constructing Telegram/object-store dependencies, then passes the resulting policy through `s3OptionsFromConfig` into `NewServer`.
- CORS-enabled tests compile a policy explicitly before calling `NewServer`; compile-failure tests target `CompileCORSPolicy` directly and assert `(nil, error)`.
- The two existing allowed-origin-related fields in YAML (`auth.credentials`, etc.) are unrelated; this design does not touch them.
- Existing buckets with no CORS configuration continue to behave exactly as they do today. No silent change.
