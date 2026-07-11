# TgNAS S3 CORS Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add opt-in global and per-bucket CORS support to TgNAS’s S3 API without changing WebDAV behavior.

**Architecture:** Compile raw global and bucket origin rules into an immutable `s3api.CORSPolicy` immediately after configuration loading and before any persistent or external startup side effect. Inject that prepared policy into `s3api.Options`, and let `Server.ServeHTTP` apply actual-response decoration and validated preflight handling before SigV4 while leaving health, readiness, and WebDAV behavior unchanged.

**Tech Stack:** Go, `net/http`, Go RE2 `regexp`, YAML configuration, `httptest`, SQLite-backed startup tests.

## Global Constraints

- CORS HTTP behavior is limited to `internal/s3api`; do not modify `internal/dav`.
- Global rules use `server.allowed_origins`; bucket rules use `buckets.<name>.allowed_origins`, with no `cors:` wrapper.
- A non-empty bucket rule list replaces global rules; an absent, empty, or empty-string-only list falls back to global rules.
- CORS is opt-in. A nil or empty effective policy preserves current response and `OPTIONS` behavior.
- Exact, glob (`*` only), and `/regex/` rules are case-insensitive and compile once before startup side effects.
- Recognized preflight means `OPTIONS` plus non-empty `Origin` plus non-empty `Access-Control-Request-Method`; it short-circuits before SigV4.
- Allowed methods are `GET, HEAD, PUT, POST, DELETE, OPTIONS`.
- Allowed request headers are `Authorization, Content-Type, Range, X-Amz-Date, X-Amz-Content-Sha256`.
- Exposed actual-response headers are `ETag, Content-Range, Content-Length, Accept-Ranges, Last-Modified`.
- Never emit `Access-Control-Allow-Credentials`.
- Successful preflight returns `204` with `Access-Control-Max-Age: 3600`; failed preflight returns `403` without negotiation headers.
- Every response under an effective policy varies on `Origin`; recognized preflight also varies on `Access-Control-Request-Method` and `Access-Control-Request-Headers`.
- Preserve all existing `Vary` values and deduplicate tokens case-insensitively across separate and comma-separated values.
- `/healthz` and `/readyz` remain outside CORS handling.
- Do not create commits unless the user explicitly requests them.

---

## File Map

- Modify `config/config.go`: add raw global and bucket origin lists to the strict YAML schema.
- Modify `config/config_test.go`: verify both lists parse and default to empty.
- Create `internal/s3api/cors.go`: compile origin patterns, select effective policies, merge `Vary`, decorate actual responses, and validate preflights.
- Create `internal/s3api/cors_test.go`: unit-test matching, compiler failure context, fallback/replace behavior, and `Vary` merging.
- Modify `internal/s3api/server.go`: accept a prepared policy and invoke CORS after health/readiness but before HTML root and SigV4.
- Modify `internal/s3api/server_test.go`: verify complete HTTP behavior and regressions.
- Modify `cmd/tgnas/main.go`: compile policy before side effects and pass it through one options-construction helper.
- Modify `cmd/tgnas/main_test.go`: verify production mapping, fail-fast startup, and WebDAV isolation.

---

### Task 1: Parse raw CORS configuration

**Files:**
- Modify: `config/config.go:40-46`
- Modify: `config/config.go:85-89`
- Test: `config/config_test.go`

**Interfaces:**
- Produces: `config.ServerConfig.AllowedOrigins []string`
- Produces: `config.BucketConfig.AllowedOrigins []string`
- Constraint: this layer preserves raw strings and does not compile or validate origin expressions.

- [ ] **Step 1: Write failing YAML parsing tests**

Add these tests near the other `LoadFile` tests in `config/config_test.go`:

```go
func TestLoadFileParsesAllowedOrigins(t *testing.T) {
	t.Setenv("TGNAS_SECRET_KEY", "secret")
	path := filepath.Join(t.TempDir(), "config.yaml")
	err := os.WriteFile(path, []byte(`
server:
  allowed_origins:
    - https://global.example.com
    - https://*.example.com
    - /^https://[a-z]+\.internal\.example$/
auth:
  credentials:
    - access_key: AKID
      secret_key_env: TGNAS_SECRET_KEY
telegram:
  bot_token: 123456:valid-token
metadata:
  sqlite_path: metadata.sqlite
buckets:
  photos:
    chat_id: "-100"
    allowed_origins:
      - https://photos.example.com
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile returned error: %v", err)
	}

	wantGlobal := []string{
		"https://global.example.com",
		"https://*.example.com",
		`/^https://[a-z]+\.internal\.example$/`,
	}
	if !reflect.DeepEqual(cfg.Server.AllowedOrigins, wantGlobal) {
		t.Fatalf("server allowed origins = %#v, want %#v", cfg.Server.AllowedOrigins, wantGlobal)
	}
	wantBucket := []string{"https://photos.example.com"}
	if !reflect.DeepEqual(cfg.Buckets["photos"].AllowedOrigins, wantBucket) {
		t.Fatalf("bucket allowed origins = %#v, want %#v", cfg.Buckets["photos"].AllowedOrigins, wantBucket)
	}
}

func TestLoadFileDefaultsAllowedOriginsToEmpty(t *testing.T) {
	cfg := minimalValidConfig()
	if len(cfg.Server.AllowedOrigins) != 0 {
		t.Fatalf("server allowed origins = %#v, want empty", cfg.Server.AllowedOrigins)
	}
	if len(cfg.Buckets["photos"].AllowedOrigins) != 0 {
		t.Fatalf("bucket allowed origins = %#v, want empty", cfg.Buckets["photos"].AllowedOrigins)
	}
}
```

Add `reflect` to the test imports if it is not already imported.

- [ ] **Step 2: Run the tests and verify RED**

Run:

```bash
go test ./config -run 'TestLoadFile(Parses|Defaults)AllowedOrigins' -count=1 -v
```

Expected: compilation fails because `ServerConfig` and `BucketConfig` do not yet have `AllowedOrigins`.

- [ ] **Step 3: Add the minimal schema fields**

Update `config/config.go`:

```go
type ServerConfig struct {
	Listen            string   `yaml:"listen"`
	ListenEnv         string   `yaml:"listen_env"`
	PublicBaseURL     string   `yaml:"public_base_url"`
	TrustedProxies    []string `yaml:"trusted_proxies"`
	TrustedProxyHosts []string `yaml:"trusted_proxy_hosts"`
	AllowedOrigins    []string `yaml:"allowed_origins"`
}
```

```go
type BucketConfig struct {
	ChatID        string   `yaml:"chat_id"`
	BotToken      string   `yaml:"bot_token"`
	PublicRead    bool     `yaml:"public_read"`
	AllowedOrigins []string `yaml:"allowed_origins"`
}
```

Run `gofmt` after editing; alignment is handled by the formatter.

- [ ] **Step 4: Verify GREEN and config regressions**

Run:

```bash
gofmt -w config/config.go config/config_test.go
go test ./config -count=1
```

Expected: all config tests pass, including strict YAML decoding with the new fields.

---

### Task 2: Compile immutable origin policies and merge Vary safely

**Files:**
- Create: `internal/s3api/cors.go`
- Create: `internal/s3api/cors_test.go`

**Interfaces:**
- Produces: `func CompileCORSPolicy(global []string, buckets map[string][]string) (*CORSPolicy, error)`
- Produces: opaque `type CORSPolicy` with unexported immutable fields.
- Produces: `func addVary(header http.Header, names ...string)`
- Produces package-private matcher selection used by Task 3.
- Pattern semantics: empty strings are ignored; exact and glob are anchored; `/regex/` uses native Go RE2 with `(?i)`.

- [ ] **Step 1: Write failing matcher and compiler tests**

Create `internal/s3api/cors_test.go` with table-driven tests that exercise the public compiler and package-private matcher selection:

```go
package s3api

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestCompileCORSPolicyMatchesOriginPatterns(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		origin  string
		want    bool
	}{
		{name: "exact", pattern: "https://app.example.com", origin: "HTTPS://APP.EXAMPLE.COM", want: true},
		{name: "exact anchored", pattern: "https://app.example.com", origin: "https://app.example.com.evil", want: false},
		{name: "glob", pattern: "https://*.staging.example.com", origin: "https://a.staging.example.com", want: true},
		{name: "glob suffix anchored", pattern: "https://*.staging.example.com", origin: "https://a.staging.example.com/path", want: false},
		{name: "glob rejects evil host suffix", pattern: "https://*.staging.example.com", origin: "https://a.staging.example.com.evil", want: false},
		{name: "glob requires literal dot", pattern: "https://*.staging.example.com", origin: "https://staging.example.com", want: false},
		{name: "consecutive stars", pattern: "https://**.example.com", origin: "https://a.example.com", want: true},
		{name: "glob metacharacters literal", pattern: "https://*.example[dev].com", origin: "https://a.example[dev].com", want: true},
		{name: "glob metacharacters not regex", pattern: "https://*.example[dev].com", origin: "https://a.exampled.com", want: false},
		{name: "single star", pattern: "*", origin: "https://anything.example/path", want: true},
		{name: "single star empty", pattern: "*", origin: "", want: true},
		{name: "regex", pattern: `/^https://[a-z]+\.example\.com$/`, origin: "https://app.example.com", want: true},
		{name: "regex case insensitive", pattern: `/^https://[a-z]+\.example\.com$/`, origin: "HTTPS://APP.EXAMPLE.COM", want: true},
		{name: "regex anchored", pattern: `/^https://[a-z]+\.example\.com$/`, origin: "https://app.example.com.evil", want: false},
		{name: "regex multiple slashes", pattern: `/^https://example\.com/a/b$/`, origin: "https://example.com/a/b", want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := CompileCORSPolicy([]string{tc.pattern}, nil)
			if err != nil {
				t.Fatalf("CompileCORSPolicy returned error: %v", err)
			}
			matcher := policy.matcherForBucket("", false)
			if got := matcher.allow(tc.origin); got != tc.want {
				t.Fatalf("allow(%q) = %v, want %v", tc.origin, got, tc.want)
			}
		})
	}
}

func TestCompileCORSPolicyIgnoresEmptyRulesAndMatchesAnyRule(t *testing.T) {
	policy, err := CompileCORSPolicy([]string{"", "https://one.example", "https://two.example"}, nil)
	if err != nil {
		t.Fatalf("CompileCORSPolicy returned error: %v", err)
	}
	matcher := policy.matcherForBucket("", false)
	if !matcher.allow("https://two.example") {
		t.Fatal("second rule did not match")
	}
}

func TestCompileCORSPolicyRejectsInvalidRegexWithoutPartialPolicy(t *testing.T) {
	tests := []struct {
		name       string
		global     []string
		buckets    map[string][]string
		wantText   string
	}{
		{name: "global", global: []string{"/[invalid/"}, wantText: "global CORS policy"},
		{name: "bucket", buckets: map[string][]string{"photos": {"/[invalid/"}}, wantText: `bucket "photos" CORS policy`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := CompileCORSPolicy(tc.global, tc.buckets)
			if err == nil || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("policy = %#v, err = %v, want contextual error containing %q", policy, err, tc.wantText)
			}
			if policy != nil {
				t.Fatalf("policy = %#v, want nil on compile failure", policy)
			}
		})
	}
}

func TestCORSPolicyBucketOverrideAndFallback(t *testing.T) {
	policy, err := CompileCORSPolicy(
		[]string{"https://global.example"},
		map[string][]string{
			"override":        {"https://bucket.example"},
			"fallback-empty":  {},
			"fallback-blanks": {"", ""},
		},
	)
	if err != nil {
		t.Fatalf("CompileCORSPolicy returned error: %v", err)
	}

	if policy.matcherForBucket("override", true).allow("https://global.example") {
		t.Fatal("non-empty bucket override consulted global matcher")
	}
	if !policy.matcherForBucket("override", true).allow("https://bucket.example") {
		t.Fatal("bucket override did not match")
	}
	for _, bucket := range []string{"fallback-empty", "fallback-blanks", "absent"} {
		if !policy.matcherForBucket(bucket, true).allow("https://global.example") {
			t.Fatalf("bucket %q did not fall back to global matcher", bucket)
		}
	}
	if !policy.matcherForBucket("", false).allow("https://global.example") {
		t.Fatal("bucket-less path did not use global matcher")
	}
}
```

- [ ] **Step 2: Write failing Vary tests**

Append to `internal/s3api/cors_test.go`:

```go
func TestAddVaryPreservesAndDeduplicatesAllValues(t *testing.T) {
	header := http.Header{}
	header.Add("Vary", "Accept-Encoding, origin")
	header.Add("Vary", "User-Agent")

	addVary(header, "Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers")
	addVary(header, "ORIGIN", "access-control-request-method")

	var tokens []string
	for _, value := range header.Values("Vary") {
		for _, token := range strings.Split(value, ",") {
			tokens = append(tokens, strings.TrimSpace(token))
		}
	}
	wantCounts := map[string]int{
		"accept-encoding":                 1,
		"origin":                          1,
		"user-agent":                      1,
		"access-control-request-method":   1,
		"access-control-request-headers":  1,
	}
	gotCounts := map[string]int{}
	for _, token := range tokens {
		gotCounts[strings.ToLower(token)]++
	}
	if !reflect.DeepEqual(gotCounts, wantCounts) {
		t.Fatalf("Vary token counts = %#v, want %#v; raw values = %#v", gotCounts, wantCounts, header.Values("Vary"))
	}
}
```

This explicitly covers both comma-separated tokens and multiple independent `Vary` header values.

- [ ] **Step 3: Run the tests and verify RED**

Run:

```bash
go test ./internal/s3api -run 'Test(CompileCORSPolicy|CORSPolicy|AddVary)' -count=1 -v
```

Expected: compilation fails because `CompileCORSPolicy`, `matcherForBucket`, and `addVary` do not exist.

- [ ] **Step 4: Implement the minimal compiler and policy types**

Create `internal/s3api/cors.go` with these types and helpers:

```go
package s3api

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

type ruleKind uint8

const (
	ruleExact ruleKind = iota
	ruleGlob
	ruleRegex
)

type OriginRule struct {
	kind  ruleKind
	regex *regexp.Regexp
}

type OriginMatcher struct {
	rules []OriginRule
}

type CORSPolicy struct {
	global  OriginMatcher
	buckets map[string]OriginMatcher
}

func CompileCORSPolicy(global []string, buckets map[string][]string) (*CORSPolicy, error) {
	policy := &CORSPolicy{buckets: make(map[string]OriginMatcher)}
	if err := policy.global.compile(global); err != nil {
		return nil, fmt.Errorf("compile global CORS policy: %w", err)
	}
	for bucket, rules := range buckets {
		matcher := OriginMatcher{}
		if err := matcher.compile(rules); err != nil {
			return nil, fmt.Errorf("compile bucket %q CORS policy: %w", bucket, err)
		}
		if len(matcher.rules) > 0 {
			policy.buckets[bucket] = matcher
		}
	}
	return policy, nil
}

func (m *OriginMatcher) compile(spec []string) error {
	for _, raw := range spec {
		if raw == "" {
			continue
		}
		kind := ruleExact
		expression := "(?i)^" + regexp.QuoteMeta(raw) + "$"
		switch {
		case len(raw) >= 2 && raw[0] == '/' && raw[len(raw)-1] == '/':
			kind = ruleRegex
			expression = "(?i)" + raw[1:len(raw)-1]
		case strings.Contains(raw, "*"):
			kind = ruleGlob
			parts := strings.Split(raw, "*")
			for i, part := range parts {
				parts[i] = regexp.QuoteMeta(part)
			}
			expression = "(?i)^" + strings.Join(parts, ".*") + "$"
		}
		compiled, err := regexp.Compile(expression)
		if err != nil {
			return fmt.Errorf("compile origin pattern %q: %w", raw, err)
		}
		m.rules = append(m.rules, OriginRule{kind: kind, regex: compiled})
	}
	return nil
}

func (m OriginMatcher) allow(origin string) bool {
	for _, rule := range m.rules {
		if rule.regex.MatchString(origin) {
			return true
		}
	}
	return false
}

func (p *CORSPolicy) matcherForBucket(bucket string, hasBucket bool) OriginMatcher {
	if p == nil {
		return OriginMatcher{}
	}
	if hasBucket {
		if matcher, ok := p.buckets[bucket]; ok {
			return matcher
		}
	}
	return p.global
}

func addVary(header http.Header, names ...string) {
	existing := map[string]bool{}
	for _, value := range header.Values("Vary") {
		for _, token := range strings.Split(value, ",") {
			token = strings.TrimSpace(token)
			if token != "" {
				existing[strings.ToLower(token)] = true
			}
		}
	}
	for _, name := range names {
		key := strings.ToLower(name)
		if existing[key] {
			continue
		}
		header.Add("Vary", name)
		existing[key] = true
	}
}
```

The `kind` field records parser classification even though matching is uniformly regex-backed. Do not export policy internals or add mutation APIs.

- [ ] **Step 5: Verify GREEN**

Run:

```bash
gofmt -w internal/s3api/cors.go internal/s3api/cors_test.go
go test ./internal/s3api -run 'Test(CompileCORSPolicy|CORSPolicy|AddVary)' -count=1 -v
```

Expected: all new compiler and `Vary` tests pass.

---

### Task 3: Apply CORS before SigV4 in the S3 handler

**Files:**
- Modify: `internal/s3api/cors.go`
- Modify: `internal/s3api/server.go:35-51`
- Modify: `internal/s3api/server.go:53-146`
- Test: `internal/s3api/server_test.go`

**Interfaces:**
- Consumes: `*CORSPolicy`, `matcherForBucket`, and `addVary` from Task 2.
- Produces: `s3api.Options.CORS *CORSPolicy`.
- Produces: `(*Server).handleCORS(http.ResponseWriter, *http.Request) bool`; `true` means the response was completed and `ServeHTTP` must return.
- `NewServer` remains `func NewServer(ObjectStore, Options) http.Handler`.

- [ ] **Step 1: Add reusable CORS test helpers**

Add these helpers near the existing `errorPutObjectStore` declaration in `internal/s3api/server_test.go`. Use the existing `errorPutObjectStore` (defined at `internal/s3api/server_test.go:879`) as the in-process store so preflight and unauthenticated actual requests are intercepted before any object store call:

```go
func mustCompileTestCORSPolicy(t *testing.T, global []string, buckets map[string][]string) *CORSPolicy {
	t.Helper()
	policy, err := CompileCORSPolicy(global, buckets)
	if err != nil {
		t.Fatalf("CompileCORSPolicy returned error: %v", err)
	}
	return policy
}

func newCORSTestServer(t *testing.T, policy *CORSPolicy, options func(*Options)) http.Handler {
	t.Helper()
	base := Options{
		Region:      "us-east-1",
		Credentials: map[string]string{"AKID": "SECRET"},
		Ready:       func() bool { return true },
		CORS:        policy,
	}
	if options != nil {
		options(&base)
	}
	return NewServer(errorPutObjectStore{}, base)
}

func newPreflightRequest(path, origin, method, headers string) *http.Request {
	req := httptest.NewRequest(http.MethodOptions, path, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if method != "" {
		req.Header.Set("Access-Control-Request-Method", method)
	}
	if headers != "" {
		req.Header.Set("Access-Control-Request-Headers", headers)
	}
	return req
}

func varyTokenCount(header http.Header, name string) int {
	count := 0
	for _, value := range header.Values("Vary") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), name) {
				count++
			}
		}
	}
	return count
}

func assertNoCORSNegotiationHeaders(t *testing.T, header http.Header) {
	t.Helper()
	for _, name := range []string{
		"Access-Control-Allow-Origin",
		"Access-Control-Allow-Methods",
		"Access-Control-Allow-Headers",
		"Access-Control-Max-Age",
		"Access-Control-Expose-Headers",
		"Access-Control-Allow-Credentials",
	} {
		if got := header.Get(name); got != "" {
			t.Fatalf("%s = %q, want absent", name, got)
		}
	}
}
```

- [ ] **Step 2: Write failing actual-response tests**

Add focused tests to `internal/s3api/server_test.go`:

```go
func TestCORSActualResponses(t *testing.T) {
	policy := mustCompileTestCORSPolicy(t, []string{"https://frontend.example"}, nil)
	server := newCORSTestServer(t, policy, nil)

	tests := []struct {
		name        string
		origin      string
		wantAllow   bool
		wantVary    bool
	}{
		{name: "allowed", origin: "https://frontend.example", wantAllow: true, wantVary: true},
		{name: "denied", origin: "https://evil.example", wantVary: true},
		{name: "missing origin", wantVary: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/photos/object", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, req)

			if tc.wantVary && varyTokenCount(rec.Header(), "Origin") != 1 {
				t.Fatalf("Vary = %#v, want Origin exactly once", rec.Header().Values("Vary"))
			}
			if tc.wantAllow {
				if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tc.origin {
					t.Fatalf("allow origin = %q, want %q", got, tc.origin)
				}
				if got := rec.Header().Get("Access-Control-Expose-Headers"); got != "ETag, Content-Range, Content-Length, Accept-Ranges, Last-Modified" {
					t.Fatalf("expose headers = %q", got)
				}
			} else if rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Access-Control-Expose-Headers") != "" {
				t.Fatalf("unexpected CORS headers: %#v", rec.Header())
			}
			if rec.Header().Get("Access-Control-Allow-Credentials") != "" {
				t.Fatal("Access-Control-Allow-Credentials must not be emitted")
			}
		})
	}
}

func TestCORSDisabledPreservesExistingBehavior(t *testing.T) {
	for _, policy := range []*CORSPolicy{nil, mustCompileTestCORSPolicy(t, nil, nil)} {
		server := newCORSTestServer(t, policy, nil)
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, newPreflightRequest("/photos/object", "https://frontend.example", "GET", ""))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want existing SigV4 rejection %d", rec.Code, http.StatusForbidden)
		}
		assertNoCORSNegotiationHeaders(t, rec.Header())
		if varyTokenCount(rec.Header(), "Origin") != 0 {
			t.Fatalf("Vary = %#v, want no CORS dimension", rec.Header().Values("Vary"))
		}
	}
}
```

The allowed actual request is intentionally unsigned: its existing auth failure proves headers are set before SigV4 and survive error responses.

- [ ] **Step 3: Write failing preflight tests**

Add:

```go
func TestCORSPreflightNegotiation(t *testing.T) {
	policy := mustCompileTestCORSPolicy(t, []string{"https://frontend.example"}, nil)
	server := newCORSTestServer(t, policy, nil)

	tests := []struct {
		name    string
		origin  string
		method  string
		headers string
		want    int
	}{
		{name: "allowed", origin: "https://frontend.example", method: "put", headers: "authorization, Content-Type, range, X-Amz-Date, X-Amz-Content-Sha256", want: http.StatusNoContent},
		{name: "denied origin", origin: "https://evil.example", method: "GET", want: http.StatusForbidden},
		{name: "denied method", origin: "https://frontend.example", method: "PATCH", want: http.StatusForbidden},
		{name: "denied header", origin: "https://frontend.example", method: "GET", headers: "X-Custom", want: http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, newPreflightRequest("/photos/object", tc.origin, tc.method, tc.headers))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			for _, dimension := range []string{"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"} {
				if varyTokenCount(rec.Header(), dimension) != 1 {
					t.Fatalf("Vary = %#v, want %s exactly once", rec.Header().Values("Vary"), dimension)
				}
			}
			if tc.want == http.StatusNoContent {
				if rec.Header().Get("Access-Control-Allow-Origin") != tc.origin ||
					rec.Header().Get("Access-Control-Allow-Methods") != "GET, HEAD, PUT, POST, DELETE, OPTIONS" ||
					rec.Header().Get("Access-Control-Allow-Headers") != "Authorization, Content-Type, Range, X-Amz-Date, X-Amz-Content-Sha256" ||
					rec.Header().Get("Access-Control-Max-Age") != "3600" {
					t.Fatalf("unexpected preflight headers: %#v", rec.Header())
				}
				if rec.Header().Get("Access-Control-Expose-Headers") != "" {
					t.Fatal("preflight must not expose actual-response headers")
				}
				if rec.Header().Get("Access-Control-Allow-Credentials") != "" {
					t.Fatal("preflight must not allow credentials")
				}
			} else {
				assertNoCORSNegotiationHeaders(t, rec.Header())
			}
		})
	}
}

func TestCORSOptionsMissingPreflightFieldsContinuesThroughSigV4(t *testing.T) {
	policy := mustCompileTestCORSPolicy(t, []string{"https://frontend.example"}, nil)
	server := newCORSTestServer(t, policy, nil)

	tests := []struct {
		name            string
		req             *http.Request
		wantStatus      int
		wantOriginVary  int
		wantAllowOrigin string
		wantExpose      bool
	}{
		{
			name:            "missing request method keeps actual decoration and fails SigV4",
			req:             newPreflightRequest("/photos/object", "https://frontend.example", "", ""),
			wantStatus:      http.StatusForbidden,
			wantOriginVary:  1,
			wantAllowOrigin: "https://frontend.example",
			wantExpose:      true,
		},
		{
			name:           "missing origin continues through SigV4 with Origin Vary",
			req:            newPreflightRequest("/photos/object", "", "GET", ""),
			wantStatus:     http.StatusForbidden,
			wantOriginVary: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, tc.req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if varyTokenCount(rec.Header(), "Origin") != tc.wantOriginVary {
				t.Fatalf("Vary Origin count = %d, want %d; values = %#v", varyTokenCount(rec.Header(), "Origin"), tc.wantOriginVary, rec.Header().Values("Vary"))
			}
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tc.wantAllowOrigin {
				t.Fatalf("Access-Control-Allow-Origin = %q, want %q", got, tc.wantAllowOrigin)
			}
			if got := rec.Header().Get("Access-Control-Expose-Headers"); (got != "") != tc.wantExpose {
				t.Fatalf("Access-Control-Expose-Headers = %q, want present %v", got, tc.wantExpose)
			}
			if tc.wantExpose && rec.Header().Get("Access-Control-Expose-Headers") != corsExposeHeaders {
				t.Fatalf("Access-Control-Expose-Headers = %q, want %q", rec.Header().Get("Access-Control-Expose-Headers"), corsExposeHeaders)
			}
			if varyTokenCount(rec.Header(), "Access-Control-Request-Method") != 0 || varyTokenCount(rec.Header(), "Access-Control-Request-Headers") != 0 {
				t.Fatalf("unrecognized preflight got negotiation Vary: %#v", rec.Header().Values("Vary"))
			}
		})
	}
}
```

- [ ] **Step 4: Write failing effective-policy, health/readiness, Vary, and public-read regressions**

Add tests covering the remaining ordering and selection requirements:

```go
func TestCORSBucketOverrideAndFallbackHTTP(t *testing.T) {
	policy := mustCompileTestCORSPolicy(t,
		[]string{"https://global.example"},
		map[string][]string{
			"override":        {"https://bucket.example"},
			"fallback-empty":  {},
			"fallback-blanks": {"", ""},
		},
	)
	server := newCORSTestServer(t, policy, nil)

	tests := []struct {
		path   string
		origin string
		want   int
	}{
		{path: "/override/key", origin: "https://bucket.example", want: http.StatusNoContent},
		{path: "/override/key", origin: "https://global.example", want: http.StatusForbidden},
		{path: "/fallback-empty/key", origin: "https://global.example", want: http.StatusNoContent},
		{path: "/fallback-blanks/key", origin: "https://global.example", want: http.StatusNoContent},
		{path: "/absent/key", origin: "https://global.example", want: http.StatusNoContent},
		{path: "/", origin: "https://global.example", want: http.StatusNoContent},
	}
	for _, tc := range tests {
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, newPreflightRequest(tc.path, tc.origin, "GET", ""))
		if rec.Code != tc.want {
			t.Fatalf("%s from %s: status = %d, want %d", tc.path, tc.origin, rec.Code, tc.want)
		}
	}
}

func TestCORSExcludesHealthAndReadiness(t *testing.T) {
	policy := mustCompileTestCORSPolicy(t, []string{"https://frontend.example"}, nil)
	tests := []struct {
		name      string
		ready     bool
		path      string
		wantCode  int
		wantBody  string
	}{
		{name: "health ready", ready: true, path: "/healthz", wantCode: http.StatusOK, wantBody: "ok"},
		{name: "health not ready", ready: false, path: "/healthz", wantCode: http.StatusOK, wantBody: "ok"},
		{name: "ready", ready: true, path: "/readyz", wantCode: http.StatusOK, wantBody: "ready"},
		{name: "not ready", ready: false, path: "/readyz", wantCode: http.StatusServiceUnavailable, wantBody: "not ready"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newCORSTestServer(t, policy, func(options *Options) {
				options.Ready = func() bool { return tc.ready }
			})

			get := httptest.NewRequest(http.MethodGet, tc.path, nil)
			get.Header.Set("Origin", "https://frontend.example")
			getRec := httptest.NewRecorder()
			server.ServeHTTP(getRec, get)
			if getRec.Code != tc.wantCode || getRec.Body.String() != tc.wantBody {
				t.Fatalf("GET %s status = %d body = %q, want %d %q", tc.path, getRec.Code, getRec.Body.String(), tc.wantCode, tc.wantBody)
			}
			assertNoCORSNegotiationHeaders(t, getRec.Header())
			for _, dimension := range []string{"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"} {
				if varyTokenCount(getRec.Header(), dimension) != 0 {
					t.Fatalf("GET %s Vary = %#v, want no CORS dimensions", tc.path, getRec.Header().Values("Vary"))
				}
			}

			preflightRec := httptest.NewRecorder()
			server.ServeHTTP(preflightRec, newPreflightRequest(tc.path, "https://frontend.example", "GET", ""))
			if preflightRec.Code != http.StatusNotFound {
				t.Fatalf("OPTIONS %s status = %d, want 404", tc.path, preflightRec.Code)
			}
			assertNoCORSNegotiationHeaders(t, preflightRec.Header())
			for _, dimension := range []string{"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"} {
				if varyTokenCount(preflightRec.Header(), dimension) != 0 {
					t.Fatalf("OPTIONS %s Vary = %#v, want no CORS dimensions", tc.path, preflightRec.Header().Values("Vary"))
				}
			}
		})
	}
}

func TestCORSVaryPreservesExistingValues(t *testing.T) {
	policy := mustCompileTestCORSPolicy(t, []string{"https://frontend.example"}, nil)
	inner := newCORSTestServer(t, policy, nil)
	server := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding, origin")
		w.Header().Add("Vary", "User-Agent")
		inner.ServeHTTP(w, r)
	})

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/photos/key", nil),
		newPreflightRequest("/photos/key", "https://frontend.example", "GET", "Authorization"),
	} {
		req.Header.Set("Origin", "https://frontend.example")
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if varyTokenCount(rec.Header(), "Accept-Encoding") != 1 || varyTokenCount(rec.Header(), "User-Agent") != 1 || varyTokenCount(rec.Header(), "Origin") != 1 {
			t.Fatalf("Vary values not preserved/deduplicated: %#v", rec.Header().Values("Vary"))
		}
		if req.Method == http.MethodOptions {
			if varyTokenCount(rec.Header(), "Access-Control-Request-Method") != 1 || varyTokenCount(rec.Header(), "Access-Control-Request-Headers") != 1 {
				t.Fatalf("preflight Vary dimensions = %#v, want each negotiation dimension once", rec.Header().Values("Vary"))
			}
		}
	}
}
```

Add a CORS-capable wrapper around the existing public-read helper, then add exact regressions:

```go
func newPublicReadCORSTestServer(t *testing.T, publicReadBuckets map[string]bool, policy *CORSPolicy) http.Handler {
	t.Helper()
	ctx := context.Background()
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	for name, chatID := range map[string]string{"photos": "-100", "backups": "-200"} {
		if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: name, ChatID: chatID, CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
			t.Fatalf("UpsertBucket(%s) returned error: %v", name, err)
		}
	}
	objectStore, err := store.NewObjectStore(meta, testutil.NewFakeTelegram(), store.Options{Upload: store.DefaultUploadConfig()})
	if err != nil {
		t.Fatalf("NewObjectStore returned error: %v", err)
	}
	return NewServer(objectStore, Options{
		Region:            "us-east-1",
		Credentials:       map[string]string{"AKID": "SECRET"},
		PublicReadBuckets: publicReadBuckets,
		SigV4Clock:        func() time.Time { return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) },
		Ready:             func() bool { return true },
		CORS:              policy,
	})
}

func TestCORSPublicReadRetainsAnonymousBehavior(t *testing.T) {
	policy := mustCompileTestCORSPolicy(t, []string{"https://frontend.example"}, nil)
	server := newPublicReadCORSTestServer(t, map[string]bool{"photos": true}, policy)

	put := signedRecorderRequest(t, http.MethodPut, "/photos/public.txt", "hello", map[string]string{"Content-Type": "text/plain"})
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/photos/public.txt", nil)
	req.Header.Set("Origin", "https://frontend.example")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "hello" {
		t.Fatalf("anonymous get status = %d body = %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://frontend.example" ||
		rec.Header().Get("Access-Control-Expose-Headers") != corsExposeHeaders ||
		varyTokenCount(rec.Header(), "Origin") != 1 {
		t.Fatalf("anonymous CORS headers = %#v", rec.Header())
	}
}

func TestCORSBusinessErrorRetainsActualResponseHeaders(t *testing.T) {
	policy := mustCompileTestCORSPolicy(t, []string{"https://frontend.example"}, nil)
	server := NewServer(errorPutObjectStore{err: errors.New("upload failed")}, Options{
		Region:      "us-east-1",
		Credentials: map[string]string{"AKID": "SECRET"},
		Ready:       func() bool { return true },
		SigV4Clock:  func() time.Time { return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) },
		CORS:        policy,
	})
	put := signedUnsignedPayloadRecorderRequest(t, http.MethodPut, "/photos/fail.txt", "hello", map[string]string{
		"Content-Type": "text/plain",
		"Origin":       "https://frontend.example",
	})
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusInternalServerError {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}
	if put.recorder.Header().Get("Access-Control-Allow-Origin") != "https://frontend.example" ||
		put.recorder.Header().Get("Access-Control-Expose-Headers") != corsExposeHeaders ||
		varyTokenCount(put.recorder.Header(), "Origin") != 1 {
		t.Fatalf("business-error CORS headers = %#v", put.recorder.Header())
	}
}
```

Keep the existing `TestPublicReadAllowsAnonymousObjectGetAndHead` unchanged; these tests isolate CORS-specific regressions without weakening the original assertions.

- [ ] **Step 5: Run all new server tests and verify RED**

Run:

```bash
go test ./internal/s3api -run 'TestCORS' -count=1 -v
```

Expected: compilation fails because `Options.CORS` and handler CORS methods do not exist.

- [ ] **Step 6: Implement HTTP constants and negotiation helpers**

Append to `internal/s3api/cors.go`:

```go
const (
	corsAllowMethods  = "GET, HEAD, PUT, POST, DELETE, OPTIONS"
	corsAllowHeaders  = "Authorization, Content-Type, Range, X-Amz-Date, X-Amz-Content-Sha256"
	corsExposeHeaders = "ETag, Content-Range, Content-Length, Accept-Ranges, Last-Modified"
	corsPreflightMaxAge = "3600"
)

var corsMethods = map[string]bool{
	"GET": true, "HEAD": true, "PUT": true,
	"POST": true, "DELETE": true, "OPTIONS": true,
}

var corsHeaders = map[string]bool{
	"authorization": true,
	"content-type": true,
	"range": true,
	"x-amz-date": true,
	"x-amz-content-sha256": true,
}

func requestedHeadersAllowed(value string) bool {
	for _, name := range strings.Split(value, ",") {
		name = strings.TrimSpace(name)
		if name != "" && !corsHeaders[strings.ToLower(name)] {
			return false
		}
	}
	return true
}

func originAllowed(w http.ResponseWriter, r *http.Request, matcher OriginMatcher) bool {
	origin := r.Header.Get("Origin")
	if !matcher.allow(origin) {
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Expose-Headers", corsExposeHeaders)
	return true
}

func corsPreflight(w http.ResponseWriter, r *http.Request, matcher OriginMatcher) {
	addVary(w.Header(), "Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers")
	origin := r.Header.Get("Origin")
	method := strings.ToUpper(strings.TrimSpace(r.Header.Get("Access-Control-Request-Method")))
	if !matcher.allow(origin) || !corsMethods[method] || !requestedHeadersAllowed(r.Header.Get("Access-Control-Request-Headers")) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Methods", corsAllowMethods)
	w.Header().Set("Access-Control-Allow-Headers", corsAllowHeaders)
	w.Header().Set("Access-Control-Max-Age", corsPreflightMaxAge)
	w.WriteHeader(http.StatusNoContent)
}
```

Do not call `originAllowed` from preflight because it would incorrectly emit `Access-Control-Expose-Headers`.

- [ ] **Step 7: Wire the prepared policy into Server**

Update `internal/s3api/server.go`:

```go
type Options struct {
	Region            string
	Credentials       map[string]string
	PublicReadBuckets map[string]bool
	Ready             func() bool
	SigV4Clock        func() time.Time
	SigV4MaxSkew      time.Duration
	Logger            *log.Logger
	CORS              *CORSPolicy
}
```

```go
type Server struct {
	store             ObjectStore
	ready             func() bool
	verify            *SigV4Verifier
	publicReadBuckets map[string]bool
	logger            *log.Logger
	cors               *CORSPolicy
}
```

Set `cors: options.CORS` in the `Server` literal. Add:

```go
func (s *Server) handleCORS(w http.ResponseWriter, r *http.Request) bool {
	bucket, _, hasBucket := splitPath(r.URL)
	matcher := s.cors.matcherForBucket(bucket, hasBucket)
	if len(matcher.rules) == 0 {
		return false
	}
	addVary(w.Header(), "Origin")
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		corsPreflight(w, r, matcher)
		return true
	}
	originAllowed(w, r, matcher)
	return false
}
```

Call it in `ServeHTTP` immediately after the health/readiness switch and before `shouldServeHTMLRoot`:

```go
	if s.handleCORS(w, r) {
		return
	}
```

Calling a method on a nil `*CORSPolicy` is safe because `matcherForBucket` handles nil.

- [ ] **Step 8: Verify GREEN and full S3 regressions**

Run:

```bash
gofmt -w internal/s3api/cors.go internal/s3api/cors_test.go internal/s3api/server.go internal/s3api/server_test.go
go test ./internal/s3api -run 'TestCORS' -count=1 -v
go test ./internal/s3api -count=1
```

Expected: all CORS tests and the existing AWS SDK/SigV4/public-read tests pass.

---

### Task 4: Compile and inject CORS before startup side effects

**Files:**
- Modify: `cmd/tgnas/main.go:549`
- Modify: `cmd/tgnas/main.go:733-829`
- Test: `cmd/tgnas/main_test.go`

**Interfaces:**
- Produces: `func compileCORSPolicyFromConfig(config.Config) (*s3api.CORSPolicy, error)` with no I/O.
- Produces: `func s3OptionsFromConfig(config.Config, map[string]string, func() bool, *log.Logger, *s3api.CORSPolicy) s3api.Options`.
- Startup ordering: compile immediately after `config.LoadFile`, before caption parsing, secrets, SQLite, metadata, Telegram clients, object store, or listener.

- [ ] **Step 1: Write failing production mapping test**

Add to `cmd/tgnas/main_test.go`:

```go
func TestS3CORSProductionWiring(t *testing.T) {
	cfg := config.Config{
		Server: config.ServerConfig{AllowedOrigins: []string{"https://global.example"}},
		Auth:   config.AuthConfig{Region: "us-east-1"},
		Buckets: map[string]config.BucketConfig{
			"override":        {AllowedOrigins: []string{"https://bucket.example"}},
			"fallback-empty":  {AllowedOrigins: []string{}},
			"fallback-blanks": {AllowedOrigins: []string{"", ""}},
		},
	}
	policy, err := compileCORSPolicyFromConfig(cfg)
	if err != nil {
		t.Fatalf("compileCORSPolicyFromConfig returned error: %v", err)
	}
	options := s3OptionsFromConfig(cfg, map[string]string{"AKID": "SECRET"}, func() bool { return true }, log.New(io.Discard, "", 0), policy)
	server := s3api.NewServer(proxySigV4ObjectStore{}, options)

	tests := []struct {
		path   string
		origin string
		want   int
	}{
		{path: "/override/key", origin: "https://bucket.example", want: http.StatusNoContent},
		{path: "/override/key", origin: "https://global.example", want: http.StatusForbidden},
		{path: "/fallback-empty/key", origin: "https://global.example", want: http.StatusNoContent},
		{path: "/fallback-blanks/key", origin: "https://global.example", want: http.StatusNoContent},
		{path: "/", origin: "https://global.example", want: http.StatusNoContent},
	}
	for _, tc := range tests {
		req := httptest.NewRequest(http.MethodOptions, tc.path, nil)
		req.Header.Set("Origin", tc.origin)
		req.Header.Set("Access-Control-Request-Method", "GET")
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("%s from %s: status = %d, want %d", tc.path, tc.origin, rec.Code, tc.want)
		}
	}
}
```

Use the existing `proxySigV4ObjectStore` defined at `cmd/tgnas/main_test.go:1719`. Recognized preflight must short-circuit before any of its methods are relevant.

- [ ] **Step 2: Write failing startup fail-fast test**

Add a table test using valid temporary YAML and fresh SQLite paths. Always quote the SQLite path with `strconv.Quote`; the existing `writeConfig` helper does not support per-case CORS injection, so the YAML is built inline:

```go
func TestRunServiceRejectsInvalidCORSBeforeSideEffects(t *testing.T) {
	t.Setenv("TGNAS_SECRET_KEY", "secret")
	tests := []struct {
		name        string
		globalCORS  string
		bucketCORS  string
		wantText    string
	}{
		{name: "global", globalCORS: "  allowed_origins:\n    - /[invalid/\n", bucketCORS: "", wantText: "global CORS policy"},
		{name: "bucket", globalCORS: "", bucketCORS: "    allowed_origins:\n      - /[invalid/\n", wantText: `bucket "photos" CORS policy`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			sqlitePath := filepath.Join(dir, "metadata.sqlite")
			configPath := filepath.Join(dir, "config.yaml")
			contents := fmt.Sprintf(`
server:
  listen: :0
%s
auth:
  credentials:
    - access_key: AKID
      secret_key_env: TGNAS_SECRET_KEY
telegram:
  bot_token: 123456:valid-token
metadata:
  sqlite_path: %s
buckets:
  photos:
    chat_id: "-100"
%s`, tc.globalCORS, strconv.Quote(sqlitePath), tc.bucketCORS)
			if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}

			objectStoreCalled := false
			oldNewObjectStore := newObjectStore
			oldListenAndServe := listenAndServe
			newObjectStore = func(metadata.Store, telegram.Client, store.Options) (*store.ObjectStore, error) {
				objectStoreCalled = true
				return nil, errors.New("unexpected object store construction")
			}
			listenAndServe = func(string, http.Handler) error {
				t.Fatal("listenAndServe should not be called")
				return nil
			}
			t.Cleanup(func() {
				newObjectStore = oldNewObjectStore
				listenAndServe = oldListenAndServe
			})

			err := runServiceWithDebug(configPath, serverModeS3, newDebugLogger(false, io.Discard))
			if err == nil || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("err = %v, want text %q", err, tc.wantText)
			}
			if objectStoreCalled {
				t.Fatal("newObjectStore was called")
			}
			if _, statErr := os.Stat(sqlitePath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("sqlite path stat error = %v, want not exist", statErr)
			}
		})
	}
}
```

- [ ] **Step 3: Run the tests and verify RED**

Run:

```bash
go test ./cmd/tgnas -run 'Test(S3CORSProductionWiring|RunServiceRejectsInvalidCORSBeforeSideEffects)' -count=1 -v
```

Expected: compilation fails because `compileCORSPolicyFromConfig` and `s3OptionsFromConfig` do not exist.

- [ ] **Step 4: Implement the config mapping and options helper**

Add near `publicReadBucketsFromConfig` in `cmd/tgnas/main.go`:

```go
func compileCORSPolicyFromConfig(cfg config.Config) (*s3api.CORSPolicy, error) {
	bucketRules := make(map[string][]string, len(cfg.Buckets))
	for name, bucket := range cfg.Buckets {
		bucketRules[name] = bucket.AllowedOrigins
	}
	return s3api.CompileCORSPolicy(cfg.Server.AllowedOrigins, bucketRules)
}

func s3OptionsFromConfig(
	cfg config.Config,
	credentials map[string]string,
	ready func() bool,
	logger *log.Logger,
	corsPolicy *s3api.CORSPolicy,
) s3api.Options {
	return s3api.Options{
		Region:            cfg.Auth.Region,
		Credentials:       credentials,
		PublicReadBuckets: publicReadBucketsFromConfig(cfg),
		Ready:             ready,
		Logger:            logger,
		CORS:              corsPolicy,
	}
}
```

- [ ] **Step 5: Move compilation before every side effect and inject policy**

Immediately after `config.LoadFile` succeeds in `runServiceWithDebug`, add:

```go
	corsPolicy, err := compileCORSPolicyFromConfig(cfg)
	if err != nil {
		return fmt.Errorf("configure S3 CORS: %w", err)
	}
```

Keep this before `telegram.ParseCaptionTemplate`, secret resolution, `ResolveSQLitePath`, and `metadata.OpenSQLite`.

Replace the inline `s3api.Options` literal with:

```go
	s3Handler := s3api.NewServer(objectStore, s3OptionsFromConfig(
		cfg,
		secrets,
		ready.Load,
		dbg.StdLogger(),
		corsPolicy,
	))
```

- [ ] **Step 6: WebDAV isolation regression**

Add a test that drives the real combined handler in `serverModeAll` mode and asserts the S3 CORS pipeline cannot decorate or short-circuit an `OPTIONS` request routed to the WebDAV prefix. This is an isolation regression rather than a behavior-driving RED test: it should pass both before and after Task 4 Step 5, while the startup and production-wiring tests above drive the production change.

```go
func TestCombinedHandlerDoesNotApplyS3CORSToWebDAV(t *testing.T) {
	t.Setenv("TGNAS_SECRET_KEY", "secret")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	sqlitePath := filepath.Join(dir, "metadata.sqlite")
	contents := strings.Join([]string{
		"server:",
		"  listen: :0",
		"  allowed_origins:",
		"    - https://frontend.example",
		"auth:",
		"  credentials:",
		"    - access_key: AKID",
		"      secret_key_env: TGNAS_SECRET_KEY",
		"telegram:",
		"  bot_token: 123456:valid-token",
		"metadata:",
		"  sqlite_path: " + strconv.Quote(sqlitePath),
		"buckets:",
		"  photos:",
		"    chat_id: \"-100\"",
		"webdav:",
		"  prefix: /dav/",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	var handler http.Handler
	oldListenAndServe := listenAndServe
	listenAndServe = func(_ string, h http.Handler) error {
		handler = h
		return nil
	}
	t.Cleanup(func() { listenAndServe = oldListenAndServe })
	if err := runServiceWithDebug(configPath, serverModeAll, newDebugLogger(false, io.Discard)); err != nil {
		t.Fatalf("runServiceWithDebug returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodOptions, "/dav/", nil)
	req.Header.Set("Origin", "https://frontend.example")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("WebDAV OPTIONS status = %d, want existing authentication status %d", rec.Code, http.StatusUnauthorized)
	}
	for _, name := range []string{
		"Access-Control-Allow-Origin",
		"Access-Control-Allow-Methods",
		"Access-Control-Allow-Headers",
		"Access-Control-Max-Age",
		"Access-Control-Expose-Headers",
	} {
		if got := rec.Header().Get(name); got != "" {
			t.Fatalf("WebDAV %s = %q, want absent", name, got)
		}
	}
	for _, dimension := range []string{"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"} {
		if varyTokenCountForMainTest(rec.Header(), dimension) != 0 {
			t.Fatalf("WebDAV Vary = %#v, want no CORS dimension %s", rec.Header().Values("Vary"), dimension)
		}
	}
}
```

Add a package-local helper at the bottom of `cmd/tgnas/main_test.go` because the S3 helper is unexported:

```go
func varyTokenCountForMainTest(header http.Header, name string) int {
	count := 0
	for _, value := range header.Values("Vary") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), name) {
				count++
			}
		}
	}
	return count
}
```

- [ ] **Step 7: Verify GREEN and startup regressions**

Run:

```bash
gofmt -w cmd/tgnas/main.go cmd/tgnas/main_test.go
go test ./cmd/tgnas -run 'Test(S3CORSProductionWiring|RunServiceRejectsInvalidCORSBeforeSideEffects|CombinedHandlerDoesNotApplyS3CORSToWebDAV)' -count=1 -v
go test ./cmd/tgnas -count=1
```

Expected: mapping tests pass, invalid policies leave SQLite absent, WebDAV isolation holds, and all existing mode/startup tests remain green.

---

### Task 5: Run final formatting and full regression verification

**Files:**
- Verify only: every file touched in Tasks 1–4.

**Interfaces:**
- Produces no new code or test interfaces.
- Confirms the diff scope and the absence of production secret/path leakage.

- [ ] **Step 1: Run targeted package tests**

Run:

```bash
go test ./config ./internal/s3api ./cmd/tgnas -count=1
```

Expected: all targeted packages pass with no warnings or races reported by the normal test runner.

- [ ] **Step 2: Format every changed Go file**

Run:

```bash
gofmt -w \
  config/config.go config/config_test.go \
  internal/s3api/cors.go internal/s3api/cors_test.go \
  internal/s3api/server.go internal/s3api/server_test.go \
  cmd/tgnas/main.go cmd/tgnas/main_test.go
```

Expected: command exits successfully and produces no output.

- [ ] **Step 3: Run the full repository regression suite**

Run:

```bash
go test ./... -count=1
```

Expected: every package passes. If any test fails, fix the implementation or test setup without weakening the approved CORS semantics, then rerun the targeted package and full suite.

- [ ] **Step 4: Inspect the final diff for scope and secrets**

Run:

```bash
git diff --check
git status --short
git diff -- config/config.go config/config_test.go internal/s3api/cors.go internal/s3api/cors_test.go internal/s3api/server.go internal/s3api/server_test.go cmd/tgnas/main.go cmd/tgnas/main_test.go
```

Expected:
- `git diff --check` reports no whitespace errors.
- No file under `internal/dav` is modified.
- No `Access-Control-Allow-Credentials` emission exists.
- No CORS pattern is compiled inside request handling or after SQLite initialization.
- No unrelated refactor, generated artifact, SQLite file, token, or credential is present.
