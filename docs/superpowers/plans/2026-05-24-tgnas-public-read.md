# TgNAS S3 Public Read Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add bucket-level `public_read` support for the S3 API so anonymous clients can read objects from explicitly public buckets.

**Architecture:** Public-read remains opt-in per configured bucket. The config loader parses `buckets.<name>.public_read`, `cmd/tgnas` passes the resulting bucket set into the S3 server, and `internal/s3api.Server` bypasses SigV4 only for anonymous `GET`/`HEAD` object requests whose bucket is public. ListBuckets, ListObjects, all bucket-level operations, all writes/deletes, and WebDAV remain authenticated.

**Tech Stack:** Go, `net/http`, existing TgNAS config YAML loader, existing S3 API server tests, existing fake Telegram object-store test helpers.

---

## File Structure

- Modify `config/config.go`
  - Add `PublicRead bool` with YAML tag `yaml:"public_read"` to `BucketConfig`.
  - No new validation is needed because omitted YAML defaults to `false`.
- Modify `config/config_test.go`
  - Add a config loader test proving `public_read: true` is accepted and attached only to that bucket.
- Modify `internal/s3api/server.go`
  - Add `PublicReadBuckets map[string]bool` to `Options`.
  - Store that map in `Server`.
  - Add a tiny helper that recognizes public anonymous object reads.
  - Call the helper before SigV4 verification in `ServeHTTP`.
- Modify `internal/s3api/server_test.go`
  - Add focused tests for anonymous object `GET` and `HEAD` on public buckets.
  - Add negative tests proving private bucket object reads, list operations, and writes still require SigV4.
- Modify `cmd/tgnas/main.go`
  - Add helper `publicReadBucketsFromConfig(cfg config.Config) map[string]bool`.
  - Pass that map into `s3api.Options`.
- Modify `cmd/tgnas/main_test.go`
  - Add a unit test for `publicReadBucketsFromConfig`.
- Modify `README.md`
  - Document the bucket-level `public_read` option and exact security boundary.
- Modify `data/config.yaml`
  - Add a commented example line for `public_read` under the sample bucket.

No commits should be created while executing this plan.

---

### Task 1: Parse bucket-level `public_read` in config

**Files:**
- Modify: `config/config.go:81-83`
- Modify: `config/config_test.go:11-66`

- [ ] **Step 1: Write the failing config loader test**

Append this test to `config/config_test.go`:

```go
func TestLoadConfigReadsBucketPublicRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	err := os.WriteFile(path, []byte(`
auth:
  credentials:
    - access_key: "admin"
      secret_key_env: "SECRET"
telegram:
  bot_token_env: "BOT_TOKEN"
buckets:
  public:
    chat_id: "-100123"
    public_read: true
  private:
    chat_id: "-100456"
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile returned error: %v", err)
	}
	if !cfg.Buckets["public"].PublicRead {
		t.Fatalf("public bucket PublicRead = false, want true")
	}
	if cfg.Buckets["private"].PublicRead {
		t.Fatalf("private bucket PublicRead = true, want false")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run:

```bash
go test ./config -run TestLoadConfigReadsBucketPublicRead -v
```

Expected: FAIL because `BucketConfig` has no `PublicRead` field and YAML `KnownFields(true)` rejects `public_read` with an error like `field public_read not found in type config.BucketConfig`.

- [ ] **Step 3: Add the config field**

Change `config/config.go`:

```go
type BucketConfig struct {
	ChatID     string `yaml:"chat_id"`
	PublicRead bool   `yaml:"public_read"`
}
```

Do not add validation. The zero value `false` is the desired safe default.

- [ ] **Step 4: Run the config test to verify it passes**

Run:

```bash
go test ./config -run TestLoadConfigReadsBucketPublicRead -v
```

Expected: PASS.

---

### Task 2: Add S3 public-read authorization boundary

**Files:**
- Modify: `internal/s3api/server.go:31-45`
- Modify: `internal/s3api/server.go:71-124`
- Modify: `internal/s3api/server_test.go:84-122`
- Modify: `internal/s3api/server_test.go:502-521`

- [ ] **Step 1: Write the failing public object read tests**

Add these tests to `internal/s3api/server_test.go` after `TestPutHeadGetDeleteObject`:

```go
func TestPublicReadAllowsAnonymousObjectGetAndHead(t *testing.T) {
	server := newPublicReadTestServer(t, map[string]bool{"photos": true})

	put := signedRecorderRequest(t, http.MethodPut, "/photos/public.txt", "hello", map[string]string{"Content-Type": "text/plain"})
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}

	getRecorder := httptest.NewRecorder()
	getRequest := httptest.NewRequest(http.MethodGet, "/photos/public.txt", nil)
	server.ServeHTTP(getRecorder, getRequest)
	if getRecorder.Code != http.StatusOK || getRecorder.Body.String() != "hello" {
		t.Fatalf("anonymous get status = %d body = %q", getRecorder.Code, getRecorder.Body.String())
	}
	if getRecorder.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("anonymous get headers = %v", getRecorder.Header())
	}

	headRecorder := httptest.NewRecorder()
	headRequest := httptest.NewRequest(http.MethodHead, "/photos/public.txt", nil)
	server.ServeHTTP(headRecorder, headRequest)
	if headRecorder.Code != http.StatusOK || headRecorder.Body.Len() != 0 {
		t.Fatalf("anonymous head status = %d body = %q", headRecorder.Code, headRecorder.Body.String())
	}
	if headRecorder.Header().Get("Content-Length") != "5" || headRecorder.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("anonymous head headers = %v", headRecorder.Header())
	}
}

func TestPublicReadKeepsPrivateObjectsAuthenticated(t *testing.T) {
	server := newPublicReadTestServer(t, map[string]bool{"photos": true})

	put := signedRecorderRequest(t, http.MethodPut, "/backups/private.txt", "secret", nil)
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/backups/private.txt", nil)
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "<Code>SignatureDoesNotMatch</Code>") {
		t.Fatalf("anonymous private get status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestPublicReadDoesNotExposeBucketListing(t *testing.T) {
	server := newPublicReadTestServer(t, map[string]bool{"photos": true})

	put := signedRecorderRequest(t, http.MethodPut, "/photos/public.txt", "hello", nil)
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}

	for _, path := range []string{"/", "/photos", "/photos?list-type=2"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		server.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "<Code>SignatureDoesNotMatch</Code>") {
			t.Fatalf("anonymous list %s status = %d body = %s", path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestPublicReadDoesNotAllowAnonymousWrites(t *testing.T) {
	server := newPublicReadTestServer(t, map[string]bool{"photos": true})

	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodPut, path: "/photos/public.txt", body: "replace"},
		{method: http.MethodDelete, path: "/photos/public.txt"},
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		server.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "<Code>SignatureDoesNotMatch</Code>") {
			t.Fatalf("anonymous %s status = %d body = %s", tc.method, recorder.Code, recorder.Body.String())
		}
	}
}
```

- [ ] **Step 2: Add the public-read test server helper**

Add this helper near `newSignedTestServer` in `internal/s3api/server_test.go`:

```go
func newPublicReadTestServer(t *testing.T, publicReadBuckets map[string]bool) http.Handler {
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
	fake := testutil.NewFakeTelegram()
	objectStore, err := store.NewObjectStore(meta, fake, store.Options{Upload: store.DefaultUploadConfig()})
	if err != nil {
		t.Fatalf("NewObjectStore returned error: %v", err)
	}
	return NewServer(objectStore, Options{
		Region:            "us-east-1",
		Credentials:       map[string]string{"AKID": "SECRET"},
		PublicReadBuckets: publicReadBuckets,
		SigV4Clock:        func() time.Time { return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) },
		Ready:             func() bool { return true },
	})
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run:

```bash
go test ./internal/s3api -run 'TestPublicRead' -v
```

Expected: FAIL at compile time because `Options` does not have `PublicReadBuckets` yet, or fail at runtime with anonymous requests returning `SignatureDoesNotMatch`.

- [ ] **Step 4: Add public-read fields and helper**

Change `internal/s3api/server.go` option and server structs:

```go
type Options struct {
	Region            string
	Credentials       map[string]string
	PublicReadBuckets map[string]bool
	Ready             func() bool
	SigV4Clock        func() time.Time
	SigV4MaxSkew      time.Duration
	Logger            *log.Logger
}

type Server struct {
	store             ObjectStore
	ready             func() bool
	verify            *SigV4Verifier
	publicReadBuckets map[string]bool
	logger            *log.Logger
}
```

Change `NewServer` to copy the map into the server:

```go
	publicReadBuckets := make(map[string]bool, len(options.PublicReadBuckets))
	for bucket, publicRead := range options.PublicReadBuckets {
		if publicRead {
			publicReadBuckets[bucket] = true
		}
	}
	return &Server{
		store:             objectStore,
		ready:             ready,
		verify:            NewSigV4Verifier(options.Region, options.Credentials, verifierOptions...),
		publicReadBuckets: publicReadBuckets,
		logger:            logger,
	}
```

Add this helper near `ServeHTTP`:

```go
func (s *Server) allowsAnonymousPublicRead(r *http.Request) bool {
	if r.Header.Get("Authorization") != "" || r.Header.Get("X-Amz-Date") != "" || r.Header.Get("X-Amz-Content-Sha256") != "" {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	bucket, key, hasBucket := splitPath(r.URL)
	if !hasBucket || key == "" {
		return false
	}
	return s.publicReadBuckets[bucket]
}
```

- [ ] **Step 5: Bypass SigV4 only for anonymous public object reads**

Change the SigV4 block in `internal/s3api/server.go` from:

```go
	if _, err := s.verify.Verify(r); err != nil {
		WriteErrorResponse(w, r, MapError(err), r.URL.Path, "")
		return
	}
```

to:

```go
	if !s.allowsAnonymousPublicRead(r) {
		if _, err := s.verify.Verify(r); err != nil {
			WriteErrorResponse(w, r, MapError(err), r.URL.Path, "")
			return
		}
	}
```

Keep the route dispatch below unchanged. Public requests still reach the existing `getObject` and `headObject` code paths, so missing objects and range requests keep existing behavior.

- [ ] **Step 6: Run public-read tests to verify they pass**

Run:

```bash
go test ./internal/s3api -run 'TestPublicRead' -v
```

Expected: PASS.

- [ ] **Step 7: Run focused existing auth tests to catch regressions**

Run:

```bash
go test ./internal/s3api -run 'TestAuthErrorsAreS3XML|TestRootNegotiationDefaultsToS3ListBuckets|TestPutHeadGetDeleteObject|TestGetObjectRange' -v
```

Expected: PASS.

---

### Task 3: Wire configured public buckets into the S3 server

**Files:**
- Modify: `cmd/tgnas/main.go:559-565`
- Modify: `cmd/tgnas/main_test.go:181-247`

- [ ] **Step 1: Write the failing wiring helper test**

Add this test to `cmd/tgnas/main_test.go` near the service-mode tests:

```go
func TestPublicReadBucketsFromConfig(t *testing.T) {
	cfg := config.Config{Buckets: map[string]config.BucketConfig{
		"photos":  {ChatID: "-100", PublicRead: true},
		"backups": {ChatID: "-200"},
		"media":   {ChatID: "-300", PublicRead: true},
	}}

	publicReadBuckets := publicReadBucketsFromConfig(cfg)
	if !publicReadBuckets["photos"] || !publicReadBuckets["media"] {
		t.Fatalf("publicReadBuckets = %#v, want photos and media", publicReadBuckets)
	}
	if publicReadBuckets["backups"] {
		t.Fatalf("publicReadBuckets = %#v, backups should not be public", publicReadBuckets)
	}
}
```

Also add the missing import to `cmd/tgnas/main_test.go` if it is not already present:

```go
	"github.com/aahl/tgnas/config"
```

- [ ] **Step 2: Run the helper test to verify it fails**

Run:

```bash
go test ./cmd/tgnas -run TestPublicReadBucketsFromConfig -v
```

Expected: FAIL because `publicReadBucketsFromConfig` does not exist.

- [ ] **Step 3: Add the helper**

Add this function in `cmd/tgnas/main.go`, near other small service-wiring helpers before `runServiceWithDebug`:

```go
func publicReadBucketsFromConfig(cfg config.Config) map[string]bool {
	publicReadBuckets := map[string]bool{}
	for name, bucket := range cfg.Buckets {
		if bucket.PublicRead {
			publicReadBuckets[name] = true
		}
	}
	return publicReadBuckets
}
```

- [ ] **Step 4: Pass the public bucket set to the S3 server**

Change the S3 server construction in `cmd/tgnas/main.go` from:

```go
	s3Handler := s3api.NewServer(objectStore, s3api.Options{
		Region:      cfg.Auth.Region,
		Credentials: secrets,
		Ready:       ready.Load,
		Logger:      dbg.StdLogger(),
	})
```

to:

```go
	s3Handler := s3api.NewServer(objectStore, s3api.Options{
		Region:            cfg.Auth.Region,
		Credentials:       secrets,
		PublicReadBuckets: publicReadBucketsFromConfig(cfg),
		Ready:             ready.Load,
		Logger:            dbg.StdLogger(),
	})
```

- [ ] **Step 5: Run the helper test to verify it passes**

Run:

```bash
go test ./cmd/tgnas -run TestPublicReadBucketsFromConfig -v
```

Expected: PASS.

- [ ] **Step 6: Run service routing tests to verify WebDAV and S3 routing are unchanged**

Run:

```bash
go test ./cmd/tgnas -run 'TestRunServiceWithDebugRoutesByMode|TestRunMainS3Subcommand|TestRunMainDAVSubcommand' -v
```

Expected: PASS.

---

### Task 4: Document bucket-level public read

**Files:**
- Modify: `README.md:24-44`
- Modify: `README.md:84-92`
- Modify: `data/config.yaml`

- [ ] **Step 1: Update the sample config comment**

In `data/config.yaml`, under the sample `tgnas` bucket, add this commented line:

```yaml
    # public_read: false
```

The bucket block should look like:

```yaml
buckets:
  tgnas:
    chat_id: "${TGNAS_TELEGRAM_CHAT_ID}"
    # public_read: false
```

- [ ] **Step 2: Update README configuration docs**

In `README.md`, after the environment variable list and before `WebDAV configuration:`, add:

````markdown
Bucket-level public read can be enabled for anonymous S3 object downloads:

```yaml
buckets:
  public-files:
    chat_id: "${TGNAS_TELEGRAM_CHAT_ID}"
    public_read: true
```

`public_read` defaults to `false`. When enabled, anonymous S3 clients may only `GET` and `HEAD` objects in that bucket when they already know the object key. Bucket listing, root bucket listing, writes, deletes, and WebDAV still require authentication.
````

- [ ] **Step 3: Update README authentication docs**

Change the S3 authentication sentence in `README.md` from:

```markdown
S3 keeps SigV4 authentication.
```

to:

```markdown
S3 keeps SigV4 authentication except for anonymous `GET` and `HEAD` object requests against buckets configured with `public_read: true`.
```

- [ ] **Step 4: Run a docs-adjacent config parse check**

Run:

```bash
go test ./config -run 'TestLoadConfigReadsBucketPublicRead|TestLoadConfigDefaultsAndYAML' -v
```

Expected: PASS.

---

### Task 5: Full verification

**Files:**
- Read-only verification across the repository.

- [ ] **Step 1: Run all Go tests**

Run:

```bash
go test ./...
```

Expected: PASS.

- [ ] **Step 2: Build the CLI**

Run:

```bash
go build ./cmd/tgnas
```

Expected: exit code 0.

- [ ] **Step 3: Inspect the final diff**

Run:

```bash
git diff -- config/config.go config/config_test.go internal/s3api/server.go internal/s3api/server_test.go cmd/tgnas/main.go cmd/tgnas/main_test.go README.md data/config.yaml
```

Expected: diff contains only bucket-level public-read config parsing, S3 anonymous `GET`/`HEAD` object-read bypass for public buckets, wiring, tests, and docs.

- [ ] **Step 4: Confirm no commit was created**

Run:

```bash
git status --short
```

Expected: files are modified but no new commit has been created by this plan execution.

---

## Trusted Proxy Addendum

This addendum extends the same no-commit plan with trusted reverse-proxy support for SigV4 deployments behind cloudflared or similar proxies.

**Additional Goal:** Allow TgNAS to verify S3 SigV4 requests behind a trusted reverse proxy by using forwarded host/proto values only when an explicit trust rule matches.

**Additional Architecture:** Add `server.trusted_proxies` and `server.trusted_proxy_hosts` to config. Wrap the selected HTTP handler in `cmd/tgnas` with a small middleware that clones the request and rewrites `Host`, `URL.Host`, and optionally `URL.Scheme` before S3/WebDAV routing. Trust is granted when either the remote IP matches `trusted_proxies` or the forwarded host matches `trusted_proxy_hosts`; if the remote IP matches, any forwarded host is accepted. Without a match, the request is passed through unchanged so SigV4 fails normally.

**Additional File Structure:**

- Modify `config/config.go`
  - Add `TrustedProxies []string` and `TrustedProxyHosts []string` to `ServerConfig`.
  - Validate `trusted_proxies` entries as CIDR prefixes.
  - Validate `trusted_proxy_hosts` entries as non-empty host values.
- Modify `config/config_test.go`
  - Add tests proving trusted proxy fields parse.
  - Add tests proving invalid CIDR and empty host values fail config validation.
- Modify `cmd/tgnas/main.go`
  - Add a trusted proxy middleware and helper functions.
  - Wrap the final service handler before `listenAndServe`.
  - Keep `s3api` SigV4 verifier unchanged; it continues to use `r.Host`.
- Modify `cmd/tgnas/main_test.go`
  - Add request-level middleware tests for IP trust, forwarded-host trust, proto rewrite, and no-trust pass-through.
- Modify `README.md`
  - Document the trusted proxy options, cloudflared use case, and spoofing risk.
- Modify `data/config.yaml`
  - Add commented trusted proxy examples under `server`.

No commits should be created while executing this addendum.

---

### Task 6: Parse and validate trusted proxy config

**Files:**
- Modify: `config/config.go:3-12`
- Modify: `config/config.go:39-43`
- Modify: `config/config.go:216-280`
- Modify: `config/config_test.go`

- [ ] **Step 1: Write the failing config parse test**

Append this test to `config/config_test.go`:

```go
func TestLoadConfigReadsTrustedProxySettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	err := os.WriteFile(path, []byte(`
server:
  trusted_proxies:
    - "127.0.0.1/32"
    - "172.16.0.0/12"
  trusted_proxy_hosts:
    - "s3.example.com"
auth:
  credentials:
    - access_key: "admin"
      secret_key_env: "SECRET"
telegram:
  bot_token_env: "BOT_TOKEN"
buckets:
  tgnas:
    chat_id: "-100123"
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile returned error: %v", err)
	}
	if strings.Join(cfg.Server.TrustedProxies, ",") != "127.0.0.1/32,172.16.0.0/12" {
		t.Fatalf("trusted proxies = %#v", cfg.Server.TrustedProxies)
	}
	if strings.Join(cfg.Server.TrustedProxyHosts, ",") != "s3.example.com" {
		t.Fatalf("trusted proxy hosts = %#v", cfg.Server.TrustedProxyHosts)
	}
}
```

- [ ] **Step 2: Write failing validation tests**

Append these tests to `config/config_test.go`:

```go
func TestLoadConfigRejectsInvalidTrustedProxyCIDR(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	err := os.WriteFile(path, []byte(`
server:
  trusted_proxies:
    - "not-a-cidr"
auth:
  credentials:
    - access_key: "admin"
      secret_key_env: "SECRET"
telegram:
  bot_token_env: "BOT_TOKEN"
buckets:
  tgnas:
    chat_id: "-100123"
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, err = LoadFile(path)
	if err == nil || !strings.Contains(err.Error(), "server trusted proxy 0 must be a CIDR prefix") {
		t.Fatalf("LoadFile error = %v", err)
	}
}

func TestLoadConfigRejectsEmptyTrustedProxyHost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	err := os.WriteFile(path, []byte(`
server:
  trusted_proxy_hosts:
    - " "
auth:
  credentials:
    - access_key: "admin"
      secret_key_env: "SECRET"
telegram:
  bot_token_env: "BOT_TOKEN"
buckets:
  tgnas:
    chat_id: "-100123"
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, err = LoadFile(path)
	if err == nil || !strings.Contains(err.Error(), "server trusted proxy host 0 is required") {
		t.Fatalf("LoadFile error = %v", err)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run:

```bash
go test ./config -run 'TestLoadConfigReadsTrustedProxySettings|TestLoadConfigRejectsInvalidTrustedProxyCIDR|TestLoadConfigRejectsEmptyTrustedProxyHost' -v
```

Expected: FAIL because `ServerConfig` does not have `TrustedProxies` or `TrustedProxyHosts`, and YAML `KnownFields(true)` rejects those fields.

- [ ] **Step 4: Add config fields**

In `config/config.go`, add `net/netip` to imports.

Change `ServerConfig` to:

```go
type ServerConfig struct {
	Listen            string   `yaml:"listen"`
	ListenEnv         string   `yaml:"listen_env"`
	PublicBaseURL     string   `yaml:"public_base_url"`
	TrustedProxies    []string `yaml:"trusted_proxies"`
	TrustedProxyHosts []string `yaml:"trusted_proxy_hosts"`
}
```

- [ ] **Step 5: Add validation**

In `Config.Validate`, after the `server listen is required` check, add:

```go
	for i, value := range c.Server.TrustedProxies {
		if _, err := netip.ParsePrefix(strings.TrimSpace(value)); err != nil {
			return fmt.Errorf("server trusted proxy %d must be a CIDR prefix: %w", i, err)
		}
	}
	for i, value := range c.Server.TrustedProxyHosts {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("server trusted proxy host %d is required", i)
		}
	}
```

Do not validate DNS syntax beyond non-empty strings. Exact host comparison and normalization are handled by the HTTP middleware.

- [ ] **Step 6: Run the config tests to verify they pass**

Run:

```bash
go test ./config -run 'TestLoadConfigReadsTrustedProxySettings|TestLoadConfigRejectsInvalidTrustedProxyCIDR|TestLoadConfigRejectsEmptyTrustedProxyHost' -v
```

Expected: PASS.

---

### Task 7: Add trusted forwarded host/proto middleware

**Files:**
- Modify: `cmd/tgnas/main.go:3-23`
- Modify: `cmd/tgnas/main.go:455-488`
- Modify: `cmd/tgnas/main.go:596-599`
- Modify: `cmd/tgnas/main_test.go`

- [ ] **Step 1: Write failing tests for IP-based trust**

Append these tests to `cmd/tgnas/main_test.go`:

```go
func TestTrustedProxyMiddlewareTrustsForwardedHostWhenRemoteIPMatches(t *testing.T) {
	handler, err := newTrustedProxyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "external.example.com" {
			t.Fatalf("Host = %q", r.Host)
		}
		if r.URL.Host != "external.example.com" {
			t.Fatalf("URL.Host = %q", r.URL.Host)
		}
		if r.URL.Scheme != "https" {
			t.Fatalf("URL.Scheme = %q", r.URL.Scheme)
		}
		w.WriteHeader(http.StatusNoContent)
	}), config.ServerConfig{TrustedProxies: []string{"127.0.0.1/32"}})
	if err != nil {
		t.Fatalf("newTrustedProxyMiddleware returned error: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9000/", nil)
	request.RemoteAddr = "127.0.0.1:54321"
	request.Header.Set("X-Forwarded-Host", "external.example.com")
	request.Header.Set("X-Forwarded-Proto", "https")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestTrustedProxyMiddlewareAcceptsAnyForwardedHostWhenRemoteIPMatches(t *testing.T) {
	handler, err := newTrustedProxyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "tenant.example.net" {
			t.Fatalf("Host = %q", r.Host)
		}
		w.WriteHeader(http.StatusNoContent)
	}), config.ServerConfig{TrustedProxies: []string{"10.0.0.0/8"}, TrustedProxyHosts: []string{"s3.example.com"}})
	if err != nil {
		t.Fatalf("newTrustedProxyMiddleware returned error: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://10.1.2.3:9000/", nil)
	request.RemoteAddr = "10.1.2.3:54321"
	request.Header.Set("X-Forwarded-Host", "tenant.example.net")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}
}
```

- [ ] **Step 2: Write failing tests for host-based trust and no-trust pass-through**

Append these tests to `cmd/tgnas/main_test.go`:

```go
func TestTrustedProxyMiddlewareTrustsForwardedHostWhenHostMatches(t *testing.T) {
	handler, err := newTrustedProxyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "s3.example.com" {
			t.Fatalf("Host = %q", r.Host)
		}
		w.WriteHeader(http.StatusNoContent)
	}), config.ServerConfig{TrustedProxyHosts: []string{"s3.example.com"}})
	if err != nil {
		t.Fatalf("newTrustedProxyMiddleware returned error: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9000/", nil)
	request.RemoteAddr = "203.0.113.10:54321"
	request.Header.Set("X-Forwarded-Host", "S3.EXAMPLE.COM")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestTrustedProxyMiddlewareLeavesRequestUnchangedWithoutTrustMatch(t *testing.T) {
	handler, err := newTrustedProxyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "127.0.0.1:9000" {
			t.Fatalf("Host = %q", r.Host)
		}
		if r.URL.Host != "127.0.0.1:9000" {
			t.Fatalf("URL.Host = %q", r.URL.Host)
		}
		if r.URL.Scheme != "http" {
			t.Fatalf("URL.Scheme = %q", r.URL.Scheme)
		}
		w.WriteHeader(http.StatusNoContent)
	}), config.ServerConfig{TrustedProxies: []string{"10.0.0.0/8"}, TrustedProxyHosts: []string{"s3.example.com"}})
	if err != nil {
		t.Fatalf("newTrustedProxyMiddleware returned error: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9000/", nil)
	request.RemoteAddr = "203.0.113.10:54321"
	request.Header.Set("X-Forwarded-Host", "evil.example.com")
	request.Header.Set("X-Forwarded-Proto", "https")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}
}
```

- [ ] **Step 3: Write failing tests for `Forwarded` fallback and multi-value handling**

Append these tests to `cmd/tgnas/main_test.go`:

```go
func TestTrustedProxyMiddlewareReadsForwardedHeaderFallback(t *testing.T) {
	handler, err := newTrustedProxyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "s3.example.com" {
			t.Fatalf("Host = %q", r.Host)
		}
		if r.URL.Scheme != "https" {
			t.Fatalf("URL.Scheme = %q", r.URL.Scheme)
		}
		w.WriteHeader(http.StatusNoContent)
	}), config.ServerConfig{TrustedProxyHosts: []string{"s3.example.com"}})
	if err != nil {
		t.Fatalf("newTrustedProxyMiddleware returned error: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9000/", nil)
	request.RemoteAddr = "203.0.113.10:54321"
	request.Header.Set("Forwarded", `for=203.0.113.9;proto=https;host="s3.example.com"`)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestTrustedProxyMiddlewareUsesFirstForwardedHostValue(t *testing.T) {
	handler, err := newTrustedProxyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "first.example.com" {
			t.Fatalf("Host = %q", r.Host)
		}
		w.WriteHeader(http.StatusNoContent)
	}), config.ServerConfig{TrustedProxies: []string{"127.0.0.1/32"}})
	if err != nil {
		t.Fatalf("newTrustedProxyMiddleware returned error: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9000/", nil)
	request.RemoteAddr = "127.0.0.1:54321"
	request.Header.Set("X-Forwarded-Host", "first.example.com, second.example.com")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}
}
```

- [ ] **Step 4: Write the failing SigV4 proxy integration test**

Append this test to `cmd/tgnas/main_test.go` before implementing middleware:

```go
func TestTrustedProxyMiddlewareAllowsSigV4SignedForForwardedHost(t *testing.T) {
	s3Handler := s3api.NewServer(proxySigV4ObjectStore{}, s3api.Options{
		Region:      "us-east-1",
		Credentials: map[string]string{"AKID": "SECRET"},
		SigV4Clock:  func() time.Time { return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) },
		Ready:       func() bool { return true },
	})
	handler, err := newTrustedProxyMiddleware(s3Handler, config.ServerConfig{TrustedProxies: []string{"127.0.0.1/32"}})
	if err != nil {
		t.Fatalf("newTrustedProxyMiddleware returned error: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "https://s3.example.com/", nil)
	signRequestForProxyTest(t, request)
	request.URL.Scheme = "http"
	request.URL.Host = "127.0.0.1:9000"
	request.Host = "127.0.0.1:9000"
	request.RemoteAddr = "127.0.0.1:54321"
	request.Header.Set("X-Forwarded-Host", "s3.example.com")
	request.Header.Set("X-Forwarded-Proto", "https")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}
```

Add these imports to `cmd/tgnas/main_test.go` if they are not already present:

```go
	"crypto/sha256"
	"encoding/hex"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/aahl/tgnas/internal/s3api"
```

- [ ] **Step 5: Add SigV4 proxy test helpers**

Append these helpers to `cmd/tgnas/main_test.go`:

```go
type proxySigV4ObjectStore struct{}

func (proxySigV4ObjectStore) ListBuckets(context.Context) ([]metadata.Bucket, error) {
	return []metadata.Bucket{{Name: "tgnas", CreatedAt: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), Enabled: true}}, nil
}

func (proxySigV4ObjectStore) HeadBucket(context.Context, string) error {
	return nil
}

func (proxySigV4ObjectStore) DeleteBucket(context.Context, string) error {
	return nil
}

func (proxySigV4ObjectStore) PutObject(context.Context, store.PutObjectInput) (store.PutObjectResult, error) {
	return store.PutObjectResult{}, nil
}

func (proxySigV4ObjectStore) GetObject(context.Context, store.GetObjectInput) (io.ReadCloser, store.ObjectInfo, error) {
	return nil, store.ObjectInfo{}, store.ErrNoSuchKey
}

func (proxySigV4ObjectStore) HeadObject(context.Context, string, string) (store.ObjectInfo, error) {
	return store.ObjectInfo{}, store.ErrNoSuchKey
}

func (proxySigV4ObjectStore) ListObjects(context.Context, store.ListObjectsInput) (store.ListObjectsResult, error) {
	return store.ListObjectsResult{}, nil
}

func (proxySigV4ObjectStore) DeleteObject(context.Context, string, string) error {
	return nil
}

func signRequestForProxyTest(t *testing.T, request *http.Request) {
	t.Helper()
	payloadHash := request.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		sum := sha256.Sum256(nil)
		payloadHash = hex.EncodeToString(sum[:])
		request.Header.Set("X-Amz-Content-Sha256", payloadHash)
	}
	credentials := aws.Credentials{AccessKeyID: "AKID", SecretAccessKey: "SECRET"}
	if err := v4.NewSigner().SignHTTP(context.Background(), credentials, request, payloadHash, "s3", "us-east-1", time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)); err != nil {
		t.Fatalf("SignHTTP returned error: %v", err)
	}
}
```

- [ ] **Step 6: Run middleware and SigV4 proxy tests to verify they fail**

Run:

```bash
go test ./cmd/tgnas -run 'TestTrustedProxyMiddleware' -v
```

Expected: FAIL because `newTrustedProxyMiddleware` does not exist.

- [ ] **Step 7: Add imports**

In `cmd/tgnas/main.go`, add these imports if they are not already present:

```go
	"net"
	"net/netip"
```

- [ ] **Step 8: Add middleware types and constructor**

Add this code near the `combinedHandler` type in `cmd/tgnas/main.go`:

```go
type trustedProxyMiddleware struct {
	next  http.Handler
	trust trustedProxyTrust
}

type trustedProxyTrust struct {
	proxies []netip.Prefix
	hosts   map[string]struct{}
}

func newTrustedProxyMiddleware(next http.Handler, server config.ServerConfig) (http.Handler, error) {
	trust, err := newTrustedProxyTrust(server)
	if err != nil {
		return nil, err
	}
	if len(trust.proxies) == 0 && len(trust.hosts) == 0 {
		return next, nil
	}
	return trustedProxyMiddleware{next: next, trust: trust}, nil
}

func newTrustedProxyTrust(server config.ServerConfig) (trustedProxyTrust, error) {
	trust := trustedProxyTrust{hosts: map[string]struct{}{}}
	for i, value := range server.TrustedProxies {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
		if err != nil {
			return trustedProxyTrust{}, fmt.Errorf("parse trusted proxy %d: %w", i, err)
		}
		trust.proxies = append(trust.proxies, prefix)
	}
	for _, host := range server.TrustedProxyHosts {
		normalized := normalizeForwardedHost(host)
		if normalized != "" {
			trust.hosts[normalized] = struct{}{}
		}
	}
	return trust, nil
}
```

- [ ] **Step 9: Add request handling and trust checks**

Add this code below the constructor in `cmd/tgnas/main.go`:

```go
func (m trustedProxyMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	forwardedHost := forwardedHost(r)
	forwardedProto := forwardedProto(r)
	if forwardedHost == "" && forwardedProto == "" {
		m.next.ServeHTTP(w, r)
		return
	}
	if !m.trust.trusts(r, forwardedHost) {
		m.next.ServeHTTP(w, r)
		return
	}

	clone := r.Clone(r.Context())
	if forwardedHost != "" {
		normalized := normalizeForwardedHost(forwardedHost)
		clone.Host = normalized
		clone.URL.Host = normalized
	}
	if forwardedProto != "" {
		clone.URL.Scheme = strings.ToLower(forwardedProto)
	}
	m.next.ServeHTTP(w, clone)
}

func (t trustedProxyTrust) trusts(r *http.Request, forwardedHost string) bool {
	if t.trustsRemoteAddr(r.RemoteAddr) {
		return true
	}
	if forwardedHost == "" {
		return false
	}
	_, ok := t.hosts[normalizeForwardedHost(forwardedHost)]
	return ok
}

func (t trustedProxyTrust) trustsRemoteAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return false
	}
	for _, prefix := range t.proxies {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 10: Add forwarded header parsing helpers**

Add this code below the trust checks in `cmd/tgnas/main.go`:

```go
func forwardedHost(r *http.Request) string {
	if value := firstForwardedValue(r.Header.Get("X-Forwarded-Host")); value != "" {
		return value
	}
	return forwardedHeaderParam(r.Header.Get("Forwarded"), "host")
}

func forwardedProto(r *http.Request) string {
	if value := firstForwardedValue(r.Header.Get("X-Forwarded-Proto")); value != "" {
		return value
	}
	return forwardedHeaderParam(r.Header.Get("Forwarded"), "proto")
}

func firstForwardedValue(value string) string {
	first, _, _ := strings.Cut(value, ",")
	return strings.Trim(strings.TrimSpace(first), `"`)
}

func forwardedHeaderParam(header, name string) string {
	firstElement, _, _ := strings.Cut(header, ",")
	for _, part := range strings.Split(firstElement, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), name) {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"`)
	}
	return ""
}

func normalizeForwardedHost(host string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(host), `"`))
}
```

- [ ] **Step 11: Wrap the final handler at startup**

In `runServiceWithDebug`, after `handler` has been selected and before resolving `listenAddr`, add:

```go
	handler, err = newTrustedProxyMiddleware(handler, cfg.Server)
	if err != nil {
		return fmt.Errorf("configure trusted proxy middleware: %w", err)
	}
```

Keep health checks, combined S3/WebDAV routing, and DAV-only routing unchanged.

- [ ] **Step 12: Run middleware and SigV4 proxy tests to verify they pass**

Run:

```bash
go test ./cmd/tgnas -run 'TestTrustedProxyMiddleware' -v
```

Expected: PASS, including `TestTrustedProxyMiddlewareAllowsSigV4SignedForForwardedHost`, proving a request signed for the external forwarded host still verifies after the middleware restores that host before S3 handling.

---

### Task 8: Verify trusted proxy behavior with focused test groups

**Files:**
- Read-only verification for trusted proxy tests.

- [ ] **Step 1: Run config trusted proxy tests**

Run:

```bash
go test ./config -run 'TestLoadConfigReadsTrustedProxySettings|TestLoadConfigRejectsInvalidTrustedProxyCIDR|TestLoadConfigRejectsEmptyTrustedProxyHost' -v
```

Expected: PASS.

- [ ] **Step 2: Run command trusted proxy tests**

Run:

```bash
go test ./cmd/tgnas -run 'TestTrustedProxyMiddleware' -v
```

Expected: PASS.

- [ ] **Step 3: Run existing service routing tests**

Run:

```bash
go test ./cmd/tgnas -run 'TestRunServiceWithDebugRoutesByMode|TestRunMainS3Subcommand|TestRunMainDAVSubcommand' -v
```

Expected: PASS.

---

### Task 9: Document trusted proxy configuration

**Files:**
- Modify: `data/config.yaml:1-8`
- Modify: `README.md:24-56`

- [ ] **Step 1: Update sample config comments**

In `data/config.yaml`, extend the `server` block comments to:

```yaml
server:
  # listen: ":9000"
  # If set and the environment variable is non-empty, it overrides listen.
  # listen_env: "TGNAS_LISTEN"
  # Public URL used when the service needs to describe its external endpoint.
  # public_base_url: "https://s3.example.com"
  # Trust forwarded host/proto from matching proxy CIDRs. If the remote IP
  # matches, any forwarded host is accepted.
  # trusted_proxies:
  #   - "127.0.0.1/32"
  #   - "172.16.0.0/12"
  # Trust forwarded host/proto when the forwarded host itself matches.
  # trusted_proxy_hosts:
  #   - "s3.example.com"
```

- [ ] **Step 2: Update README configuration docs**

In `README.md`, after the `public_read` paragraph and before `WebDAV configuration:`, add:

````markdown
Trusted reverse proxy support can be enabled when TgNAS runs behind cloudflared or another proxy that changes the origin `Host` header. This lets SigV4 verification use the external host that the client signed:

```yaml
server:
  trusted_proxies:
    - "127.0.0.1/32"
    - "172.16.0.0/12"
  trusted_proxy_hosts:
    - "s3.example.com"
```

When `trusted_proxies` contains the request's remote IP, TgNAS trusts `X-Forwarded-Host` or `Forwarded: host=...` and accepts any forwarded host from that proxy. When `trusted_proxy_hosts` contains the forwarded host, TgNAS also trusts it even if the remote IP is not listed. These two trust checks are OR conditions. When trusted, TgNAS also applies `X-Forwarded-Proto` or `Forwarded: proto=...` to the cloned request before routing.

Only enable this when direct access to TgNAS is restricted to trusted networks or when `trusted_proxy_hosts` is a safe allow-list for your deployment. Forwarded headers are client-controlled unless a trusted proxy strips and replaces them.
````

- [ ] **Step 3: Run config and command tests**

Run:

```bash
go test ./config ./cmd/tgnas -run 'TestLoadConfigReadsTrustedProxySettings|TestLoadConfigRejectsInvalidTrustedProxyCIDR|TestLoadConfigRejectsEmptyTrustedProxyHost|TestTrustedProxyMiddleware' -v
```

Expected: PASS.

---

### Task 10: Full trusted proxy verification

**Files:**
- Read-only verification across the repository.

- [ ] **Step 1: Run all Go tests**

Run:

```bash
go test ./...
```

Expected: PASS.

- [ ] **Step 2: Build the CLI**

Run:

```bash
go build ./cmd/tgnas
```

Expected: exit code 0.

- [ ] **Step 3: Inspect the trusted proxy diff**

Run:

```bash
git diff -- config/config.go config/config_test.go cmd/tgnas/main.go cmd/tgnas/main_test.go README.md data/config.yaml
```

Expected: diff contains public-read changes plus trusted proxy config parsing, middleware, tests, and docs. There should be no SigV4 verifier rewrite in `internal/s3api/sigv4.go`.

- [ ] **Step 4: Confirm no commit was created**

Run:

```bash
git status --short
```

Expected: files are modified but no new commit has been created by this addendum execution.

---

## Self-Review Notes

- Spec coverage: bucket-level config is covered in Task 1; anonymous object GET/HEAD is covered in Task 2; ListBuckets/ListObjects and writes remaining authenticated are covered in Task 2; config-to-server wiring is covered in Task 3; README and sample config are covered in Task 4; verification is covered in Task 5. Trusted proxy config parsing and validation are covered in Task 6; request rewriting and trust semantics are covered in Task 7; SigV4-through-proxy behavior is covered in Task 8; trusted proxy docs are covered in Task 9; full verification is covered in Task 10.
- Placeholder scan: no TBD/TODO/fill-in-later steps remain. Every code-changing step includes concrete code.
- Type consistency: the same `PublicRead bool` field maps to YAML `public_read`, `config.BucketConfig.PublicRead`, `publicReadBucketsFromConfig`, `s3api.Options.PublicReadBuckets`, and `Server.publicReadBuckets`. Trusted proxy config uses `config.ServerConfig.TrustedProxies` and `config.ServerConfig.TrustedProxyHosts`, which feed `newTrustedProxyMiddleware`, `newTrustedProxyTrust`, and `trustedProxyTrust`.
- Trust semantics: forwarded headers are trusted when the remote IP matches `trusted_proxies` OR the forwarded host matches `trusted_proxy_hosts`. If the remote IP matches, any forwarded host is accepted. If only forwarded-host trust matches, the forwarded host must be in `trusted_proxy_hosts`. Empty trust lists preserve current behavior.
- User constraint: no commit steps are included because the user requested not to commit.

---

## Presigned URL Addendum

This addendum extends the same no-commit plan with S3 SigV4 query-string authentication for presigned object download URLs.

**Additional Goal:** Support S3 presigned URLs for object `GET` and `HEAD` only, using existing configured credentials and the existing SigV4 verification machinery.

**Additional Architecture:** Extend `internal/s3api.SigV4Verifier` so `Verify` accepts either header SigV4 authentication or query-string SigV4 authentication. Header authentication keeps the existing behavior. Query authentication parses `X-Amz-*` parameters, validates credential scope and expiration, builds the canonical request with `X-Amz-Signature` excluded from the canonical query string, and uses `UNSIGNED-PAYLOAD` for `GET`/`HEAD`. The verifier returns whether the accepted auth came from query parameters so `internal/s3api.Server` can allow presigned auth only for object `GET`/`HEAD` routes and reject presigned root, bucket, write, and delete requests.

**Additional File Structure:**

- Modify `internal/s3api/sigv4.go`
  - Add an auth-source field to `Identity` or an equivalent small result type returned by `Verify`.
  - Add query auth parsing for the required `X-Amz-*` fields.
  - Add expiration validation with a maximum of `604800` seconds.
  - Add canonical request construction that can exclude `X-Amz-Signature` from the canonical query string.
  - Keep header-auth behavior compatible with the existing tests.
- Modify `internal/s3api/sigv4_test.go`
  - Add AWS SDK presign-based verifier tests for `GET` and `HEAD`.
  - Add negative verifier tests for tampered query parameters, missing required parameters, expiration, and excessive `X-Amz-Expires`.
  - Keep existing header SigV4 tests unchanged.
- Modify `internal/s3api/server.go`
  - Reject anonymous public-read bypass when SigV4 query parameters are present.
  - After successful query auth, allow only object `GET` and `HEAD` routes.
  - Return SigV4-style authorization errors for presigned unsupported operations.
- Modify `internal/s3api/server_test.go`
  - Add server-level presigned `GET` and `HEAD` object tests.
  - Add tests proving presigned `PUT`, `DELETE`, root listing, and bucket listing fail.
  - Add public-read interaction tests proving invalid/tampered presigned query parameters do not fall through to anonymous public read.
- Modify `cmd/tgnas/main_test.go`
  - Add a trusted-proxy integration test proving a presigned URL signed for the external forwarded host verifies after middleware rewrites the request.
- Modify `README.md`
  - Document presigned URL support scope, 7-day maximum expiration, and unsupported operations.

No commits should be created while executing this addendum.

---

### Task 11: Add verifier-level presigned URL tests

**Files:**
- Modify: `internal/s3api/sigv4_test.go`

- [ ] **Step 1: Add the presign helper import**

In `internal/s3api/sigv4_test.go`, add `strconv` to the standard-library imports:

```go
	"strconv"
```

- [ ] **Step 2: Write the failing verifier tests for presigned GET and HEAD**

Append these tests before `type signedRequestOptions` in `internal/s3api/sigv4_test.go`:

```go
func TestVerifySigV4PresignedGetAndHead(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			request := presignedTestRequest(t, presignedRequestOptions{method: method})
			verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time {
				return time.Date(2024, 1, 2, 3, 5, 0, 0, time.UTC)
			}))

			identity, err := verifier.Verify(request)
			if err != nil {
				t.Fatalf("Verify returned error: %v", err)
			}
			if identity.AccessKey != "AKID" {
				t.Fatalf("identity = %+v", identity)
			}
			if !identity.Presigned {
				t.Fatalf("identity.Presigned = false, want true")
			}
		})
	}
}

func TestVerifySigV4PresignedResponseOverrideIsSigned(t *testing.T) {
	request := presignedTestRequest(t, presignedRequestOptions{
		method: http.MethodGet,
		responseContentDisposition: `attachment; filename*=UTF-8''hello.txt`,
	})
	verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time {
		return time.Date(2024, 1, 2, 3, 5, 0, 0, time.UTC)
	}))

	if _, err := verifier.Verify(request); err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}

	query := request.URL.Query()
	query.Set("response-content-disposition", `attachment; filename*=UTF-8''tampered.txt`)
	request.URL.RawQuery = query.Encode()
	_, err := verifier.Verify(request)
	if err != ErrSignatureDoesNotMatch {
		t.Fatalf("err = %v, want ErrSignatureDoesNotMatch", err)
	}
}
```

- [ ] **Step 3: Add the presigned request helper**

Append this helper near `signedTestRequest` in `internal/s3api/sigv4_test.go`:

```go
type presignedRequestOptions struct {
	method                     string
	accessKey                  string
	secret                     string
	region                     string
	service                    string
	expires                    time.Duration
	responseContentDisposition string
}

func presignedTestRequest(t *testing.T, opts presignedRequestOptions) *http.Request {
	t.Helper()

	method := opts.method
	if method == "" {
		method = http.MethodGet
	}
	accessKey := opts.accessKey
	if accessKey == "" {
		accessKey = "AKID"
	}
	secret := opts.secret
	if secret == "" {
		secret = "SECRET"
	}
	region := opts.region
	if region == "" {
		region = "us-east-1"
	}
	service := opts.service
	if service == "" {
		service = "s3"
	}
	expires := opts.expires
	if expires == 0 {
		expires = 15 * time.Minute
	}

	request := httptest.NewRequest(method, "https://example.com/photos/hello.txt", nil)
	request.Host = request.URL.Host
	query := request.URL.Query()
	query.Set("X-Amz-Expires", strconv.FormatInt(int64(expires/time.Second), 10))
	if opts.responseContentDisposition != "" {
		query.Set("response-content-disposition", opts.responseContentDisposition)
	}
	request.URL.RawQuery = query.Encode()

	credentials := aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secret}
	signedURL, _, err := v4.NewSigner().PresignHTTP(context.Background(), credentials, request, "UNSIGNED-PAYLOAD", service, region, time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), func(options *v4.SignerOptions) {
		options.DisableURIPathEscaping = true
	})
	if err != nil {
		t.Fatalf("PresignHTTP returned error: %v", err)
	}
	signedRequest := httptest.NewRequest(method, signedURL, nil)
	signedRequest.Host = signedRequest.URL.Host
	return signedRequest
}
```

The helper intentionally ignores the `signedHeaders` returned by `PresignHTTP` because these tests sign only the `host` header plus query parameters; no additional signed headers are returned for these requests.

- [ ] **Step 4: Write failing negative verifier tests**

Append these tests before `type signedRequestOptions` in `internal/s3api/sigv4_test.go`, next to the positive presigned tests:

```go
func TestVerifySigV4PresignedRejectsMissingRequiredQueryParameter(t *testing.T) {
	for _, key := range []string{"X-Amz-Algorithm", "X-Amz-Credential", "X-Amz-Date", "X-Amz-Expires", "X-Amz-SignedHeaders", "X-Amz-Signature"} {
		t.Run(key, func(t *testing.T) {
			request := presignedTestRequest(t, presignedRequestOptions{method: http.MethodGet})
			query := request.URL.Query()
			query.Del(key)
			request.URL.RawQuery = query.Encode()
			verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time {
				return time.Date(2024, 1, 2, 3, 5, 0, 0, time.UTC)
			}))

			_, err := verifier.Verify(request)
			if err != ErrSignatureDoesNotMatch {
				t.Fatalf("err = %v, want ErrSignatureDoesNotMatch", err)
			}
		})
	}
}

func TestVerifySigV4PresignedRejectsExpiredURL(t *testing.T) {
	request := presignedTestRequest(t, presignedRequestOptions{method: http.MethodGet, expires: time.Minute})
	verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time {
		return time.Date(2024, 1, 2, 3, 5, 6, 0, time.UTC)
	}))

	_, err := verifier.Verify(request)
	if err != ErrSignatureDoesNotMatch {
		t.Fatalf("err = %v, want ErrSignatureDoesNotMatch", err)
	}
}

func TestVerifySigV4PresignedRejectsExpiresAboveSevenDays(t *testing.T) {
	request := presignedTestRequest(t, presignedRequestOptions{method: http.MethodGet, expires: 7*24*time.Hour + time.Second})
	verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time {
		return time.Date(2024, 1, 2, 3, 5, 0, 0, time.UTC)
	}))

	_, err := verifier.Verify(request)
	if err != ErrSignatureDoesNotMatch {
		t.Fatalf("err = %v, want ErrSignatureDoesNotMatch", err)
	}
}

func TestVerifySigV4PresignedRejectsWrongRegionServiceAndAccessKey(t *testing.T) {
	t.Run("wrong region", func(t *testing.T) {
		request := presignedTestRequest(t, presignedRequestOptions{method: http.MethodGet, region: "eu-west-1"})
		verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time {
			return time.Date(2024, 1, 2, 3, 5, 0, 0, time.UTC)
		}))
		_, err := verifier.Verify(request)
		if err != ErrSignatureDoesNotMatch {
			t.Fatalf("err = %v, want ErrSignatureDoesNotMatch", err)
		}
	})

	t.Run("wrong service", func(t *testing.T) {
		request := presignedTestRequest(t, presignedRequestOptions{method: http.MethodGet, service: "ec2"})
		verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time {
			return time.Date(2024, 1, 2, 3, 5, 0, 0, time.UTC)
		}))
		_, err := verifier.Verify(request)
		if err != ErrSignatureDoesNotMatch {
			t.Fatalf("err = %v, want ErrSignatureDoesNotMatch", err)
		}
	})

	t.Run("unknown access key", func(t *testing.T) {
		request := presignedTestRequest(t, presignedRequestOptions{method: http.MethodGet, accessKey: "UNKNOWN"})
		verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"}, WithSigV4Clock(func() time.Time {
			return time.Date(2024, 1, 2, 3, 5, 0, 0, time.UTC)
		}))
		_, err := verifier.Verify(request)
		if err != ErrInvalidAccessKeyID {
			t.Fatalf("err = %v, want ErrInvalidAccessKeyID", err)
		}
	})
}
```

- [ ] **Step 5: Run the tests to verify they fail**

Run:

```bash
go test ./internal/s3api -run 'TestVerifySigV4Presigned' -v
```

Expected: FAIL because `Identity.Presigned` and SigV4 query verification are not implemented yet.

---

### Task 12: Implement query SigV4 verification

**Files:**
- Modify: `internal/s3api/sigv4.go`

- [ ] **Step 1: Add auth source to verifier result**

Change `Identity` in `internal/s3api/sigv4.go` to:

```go
type Identity struct {
	AccessKey string
	Presigned bool
}
```

Existing tests that only check `AccessKey` should continue to compile.

- [ ] **Step 2: Extend the internal authorization model**

Change `sigV4Authorization` to include the request timestamp, expiration, and query-auth source:

```go
type sigV4Authorization struct {
	accessKey     string
	date          string
	region        string
	service       string
	signedHeaders []string
	signature     string
	xAmzDate      string
	expires       time.Duration
	presigned     bool
}
```

- [ ] **Step 3: Update header auth parsing to populate `xAmzDate`**

In `Verify`, replace the current direct header parse with a helper call:

```go
auth, err := parseSigV4RequestAuth(r)
if err != nil {
	return Identity{}, ErrSignatureDoesNotMatch
}
```

Add this helper below `validateRequestTime`:

```go
func parseSigV4RequestAuth(r *http.Request) (sigV4Authorization, error) {
	if r.Header.Get("Authorization") != "" {
		auth, err := parseSigV4Authorization(r.Header.Get("Authorization"))
		if err != nil {
			return sigV4Authorization{}, err
		}
		auth.xAmzDate = r.Header.Get("X-Amz-Date")
		return auth, nil
	}
	return parseSigV4QueryAuthorization(r.URL.Query())
}
```

- [ ] **Step 4: Add query auth parsing**

Add this function below `parseSigV4Authorization`:

```go
func parseSigV4QueryAuthorization(query url.Values) (sigV4Authorization, error) {
	if query.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" {
		return sigV4Authorization{}, fmt.Errorf("invalid query authorization algorithm")
	}
	credential := query.Get("X-Amz-Credential")
	xAmzDate := query.Get("X-Amz-Date")
	expiresRaw := query.Get("X-Amz-Expires")
	signedHeadersRaw := query.Get("X-Amz-SignedHeaders")
	signature := query.Get("X-Amz-Signature")
	if credential == "" || xAmzDate == "" || expiresRaw == "" || signedHeadersRaw == "" || signature == "" {
		return sigV4Authorization{}, fmt.Errorf("missing query authorization fields")
	}

	credentialParts := strings.Split(credential, "/")
	if len(credentialParts) != 5 || credentialParts[4] != "aws4_request" {
		return sigV4Authorization{}, fmt.Errorf("invalid credential scope")
	}

	expiresSeconds, err := strconv.Atoi(expiresRaw)
	if err != nil || expiresSeconds <= 0 || expiresSeconds > 604800 {
		return sigV4Authorization{}, fmt.Errorf("invalid query authorization expiration")
	}

	signedHeaders := strings.Split(signedHeadersRaw, ";")
	if len(signedHeaders) == 0 || signedHeaders[0] == "" {
		return sigV4Authorization{}, fmt.Errorf("missing signed headers")
	}

	return sigV4Authorization{
		accessKey:     credentialParts[0],
		date:          credentialParts[1],
		region:        credentialParts[2],
		service:       credentialParts[3],
		signedHeaders: signedHeaders,
		signature:     strings.ToLower(signature),
		xAmzDate:      xAmzDate,
		expires:       time.Duration(expiresSeconds) * time.Second,
		presigned:     true,
	}, nil
}
```

`sigv4.go` already imports `net/url`, `strconv`, `strings`, and `time`, so no new import is needed for this function.

- [ ] **Step 5: Add presigned expiration validation**

Add this method below `validateRequestTime`:

```go
func (v *SigV4Verifier) validatePresignedRequestTime(xAmzDate, scopeDate string, expires time.Duration) error {
	signedAt, err := time.Parse("20060102T150405Z", xAmzDate)
	if err != nil {
		return err
	}
	if scopeDate != signedAt.UTC().Format("20060102") {
		return fmt.Errorf("credential scope date does not match request timestamp")
	}
	now := v.clock().UTC()
	if v.maxSkew > 0 && signedAt.After(now.Add(v.maxSkew)) {
		return fmt.Errorf("request timestamp outside allowed skew")
	}
	if now.After(signedAt.Add(expires)) {
		return fmt.Errorf("presigned request expired")
	}
	return nil
}
```

This preserves future clock-skew tolerance while enforcing expiration after `X-Amz-Date + X-Amz-Expires`.

- [ ] **Step 6: Update `Verify` to share the signing path**

Change the middle of `Verify` so it uses `auth.xAmzDate`, query expiration validation, and `UNSIGNED-PAYLOAD` for presigned requests:

```go
if auth.region != v.region || auth.service != "s3" {
	return Identity{}, ErrSignatureDoesNotMatch
}

secret, ok := v.keys[auth.accessKey]
if !ok {
	return Identity{}, ErrInvalidAccessKeyID
}

if auth.xAmzDate == "" {
	return Identity{}, ErrSignatureDoesNotMatch
}
if auth.presigned {
	if err := v.validatePresignedRequestTime(auth.xAmzDate, auth.date, auth.expires); err != nil {
		return Identity{}, ErrSignatureDoesNotMatch
	}
} else if err := v.validateRequestTime(auth.xAmzDate, auth.date); err != nil {
	return Identity{}, ErrSignatureDoesNotMatch
}

canonicalPayloadHash := "UNSIGNED-PAYLOAD"
if !auth.presigned {
	var err error
	canonicalPayloadHash, err = payloadHash(r)
	if err != nil {
		return Identity{}, ErrSignatureDoesNotMatch
	}
}

canonicalRequest, err := buildCanonicalRequest(r, auth.signedHeaders, canonicalPayloadHash, auth.presigned)
if err != nil {
	return Identity{}, ErrSignatureDoesNotMatch
}

credentialScope := strings.Join([]string{auth.date, auth.region, auth.service, "aws4_request"}, "/")
stringToSign := strings.Join([]string{
	"AWS4-HMAC-SHA256",
	auth.xAmzDate,
	credentialScope,
	hexSHA256(canonicalRequest),
}, "\n")

signingKey := deriveSigningKey(secret, auth.date, auth.region, auth.service)
expectedSignature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
if subtle.ConstantTimeCompare([]byte(expectedSignature), []byte(auth.signature)) != 1 {
	return Identity{}, ErrSignatureDoesNotMatch
}

return Identity{AccessKey: auth.accessKey, Presigned: auth.presigned}, nil
```

- [ ] **Step 7: Make canonical query construction exclude `X-Amz-Signature` only for presigned auth**

Change `buildCanonicalRequest` signature and body to:

```go
func buildCanonicalRequest(r *http.Request, signedHeaders []string, payloadHash string, excludeSignatureQuery bool) (string, error) {
	canonicalHeaders, signedHeaderList, err := canonicalHeaders(r, signedHeaders)
	if err != nil {
		return "", err
	}

	return strings.Join([]string{
		r.Method,
		canonicalURI(r.URL),
		canonicalQueryString(r.URL, excludeSignatureQuery),
		canonicalHeaders,
		signedHeaderList,
		payloadHash,
	}, "\n"), nil
}
```

Change `canonicalQueryString` signature and token loop to:

```go
func canonicalQueryString(u *url.URL, excludeSignature bool) string {
	if u.RawQuery == "" {
		return ""
	}

	type pair struct {
		key   string
		value string
	}

	tokens := strings.Split(u.RawQuery, "&")
	pairs := make([]pair, 0, len(tokens))
	for _, token := range tokens {
		key, value, hasValue := strings.Cut(token, "=")
		if excludeSignature {
			decodedKey, err := url.QueryUnescape(key)
			if err == nil && decodedKey == "X-Amz-Signature" {
				continue
			}
		}
		if !hasValue {
			value = ""
		}
		pairs = append(pairs, pair{
			key:   canonicalQueryComponent(key),
			value: canonicalQueryComponent(value),
		})
	}
```

Keep the existing sort and encode logic after the loop unchanged.

Update the existing `buildCanonicalRequest` caller to pass `auth.presigned`. If any tests call `canonicalQueryString` directly, pass `false` for the existing header-auth behavior.

- [ ] **Step 8: Run the verifier tests to verify they pass**

Run:

```bash
go test ./internal/s3api -run 'TestVerifySigV4PresignedGetAndHead|TestVerifySigV4PresignedResponseOverrideIsSigned|TestVerifySigV4SignedByAWSSDK|TestCanonicalQueryStringMatchesAWSSigner' -v
```

Expected: PASS.

---

### Task 13: Verify presigned verifier coverage

**Files:**
- Read-only verification for `internal/s3api/sigv4_test.go` and `internal/s3api/sigv4.go`.

- [ ] **Step 1: Run the presigned negative tests**

Run:

```bash
go test ./internal/s3api -run 'TestVerifySigV4PresignedRejects' -v
```

Expected: PASS. If a test fails, make only the minimal verifier change needed for that specific validation and rerun the same command.

- [ ] **Step 2: Run all SigV4 verifier tests**

Run:

```bash
go test ./internal/s3api -run 'TestVerifySigV4|TestCanonicalQueryString' -v
```

Expected: PASS.

---

### Task 14: Restrict presigned auth to object GET and HEAD at the server boundary

**Files:**
- Modify: `internal/s3api/server.go`
- Modify: `internal/s3api/server_test.go`

- [ ] **Step 1: Add server-level presigned helpers**

Add this helper near `signRequest` in `internal/s3api/server_test.go`:

```go
func presignServerRequest(t *testing.T, method, target string, expires time.Duration) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, "https://example.com"+target, nil)
	request.Host = request.URL.Host
	expiresSeconds := int64(expires / time.Second)
	if expiresSeconds == 0 {
		expiresSeconds = int64((15 * time.Minute) / time.Second)
	}
	query := request.URL.Query()
	query.Set("X-Amz-Expires", strconv.FormatInt(expiresSeconds, 10))
	request.URL.RawQuery = query.Encode()

	credentials := aws.Credentials{AccessKeyID: "AKID", SecretAccessKey: "SECRET"}
	signedURL, _, err := v4.NewSigner().PresignHTTP(context.Background(), credentials, request, "UNSIGNED-PAYLOAD", "s3", "us-east-1", time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), func(options *v4.SignerOptions) {
		options.DisableURIPathEscaping = true
	})
	if err != nil {
		t.Fatalf("PresignHTTP returned error: %v", err)
	}
	presignedRequest := httptest.NewRequest(method, signedURL, nil)
	presignedRequest.Host = presignedRequest.URL.Host
	return presignedRequest
}
```

Add `strconv` to the standard-library imports in `internal/s3api/server_test.go` for this helper.

- [ ] **Step 2: Write failing server tests for presigned object GET and HEAD**

Append these tests after `TestPutHeadGetDeleteObject` in `internal/s3api/server_test.go`:

```go
func TestPresignedObjectGetAndHead(t *testing.T) {
	server := newSignedTestServer(t)

	put := signedRecorderRequest(t, http.MethodPut, "/photos/presigned.txt", "hello", map[string]string{"Content-Type": "text/plain"})
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}

	getRecorder := httptest.NewRecorder()
	getRequest := presignServerRequest(t, http.MethodGet, "/photos/presigned.txt", 15*time.Minute)
	server.ServeHTTP(getRecorder, getRequest)
	if getRecorder.Code != http.StatusOK || getRecorder.Body.String() != "hello" {
		t.Fatalf("presigned get status = %d body = %q", getRecorder.Code, getRecorder.Body.String())
	}
	if getRecorder.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("presigned get headers = %v", getRecorder.Header())
	}

	headRecorder := httptest.NewRecorder()
	headRequest := presignServerRequest(t, http.MethodHead, "/photos/presigned.txt", 15*time.Minute)
	server.ServeHTTP(headRecorder, headRequest)
	if headRecorder.Code != http.StatusOK || headRecorder.Body.Len() != 0 {
		t.Fatalf("presigned head status = %d body = %q", headRecorder.Code, headRecorder.Body.String())
	}
	if headRecorder.Header().Get("Content-Length") != "5" {
		t.Fatalf("presigned head headers = %v", headRecorder.Header())
	}
}
```

- [ ] **Step 3: Write failing server tests for unsupported presigned operations**

Append this test to `internal/s3api/server_test.go`:

```go
func TestPresignedUnsupportedOperationsFail(t *testing.T) {
	server := newSignedTestServer(t)

	for _, tc := range []struct {
		name   string
		method string
		target string
	}{
		{name: "root listing", method: http.MethodGet, target: "/"},
		{name: "bucket listing", method: http.MethodGet, target: "/photos"},
		{name: "bucket head", method: http.MethodHead, target: "/photos"},
		{name: "put object", method: http.MethodPut, target: "/photos/presigned.txt"},
		{name: "delete object", method: http.MethodDelete, target: "/photos/presigned.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := presignServerRequest(t, tc.method, tc.target, 15*time.Minute)
			server.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "<Code>SignatureDoesNotMatch</Code>") {
				t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}
```

- [ ] **Step 4: Run server presigned tests to verify they fail**

Run:

```bash
go test ./internal/s3api -run 'TestPresignedObjectGetAndHead|TestPresignedUnsupportedOperationsFail' -v
```

Expected: `TestPresignedObjectGetAndHead` should pass after Tasks 12-13 are complete, but `TestPresignedUnsupportedOperationsFail` should fail before the server-level query-auth restriction is added because verified presigned root or bucket operations still route normally.

- [ ] **Step 5: Add server boundary helper for presigned object reads**

In `internal/s3api/server.go`, add this helper near `allowsAnonymousPublicRead`:

```go
func allowsPresignedRequest(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	_, key, hasBucket := splitPath(r.URL)
	return hasBucket && key != ""
}
```

- [ ] **Step 6: Enforce the server boundary after verification**

Change the authentication block in `ServeHTTP` from:

```go
if !s.allowsAnonymousPublicRead(r) {
	if _, err := s.verify.Verify(r); err != nil {
		WriteErrorResponse(w, r, MapError(err), r.URL.Path, "")
		return
	}
}
```

to:

```go
if !s.allowsAnonymousPublicRead(r) {
	identity, err := s.verify.Verify(r)
	if err != nil {
		WriteErrorResponse(w, r, MapError(err), r.URL.Path, "")
		return
	}
	if identity.Presigned && !allowsPresignedRequest(r) {
		WriteErrorResponse(w, r, ErrSignatureMismatch, r.URL.Path, "")
		return
	}
}
```

Do not add presigned support for bucket operations or writes. This plan intentionally reuses `SignatureDoesNotMatch` for unsupported presigned operations so unsupported query-auth requests fail in the same S3 XML authorization-error family as invalid signatures, and never fall back to anonymous public-read behavior.

- [ ] **Step 7: Run server presigned tests to verify they pass**

Run:

```bash
go test ./internal/s3api -run 'TestPresignedObjectGetAndHead|TestPresignedUnsupportedOperationsFail' -v
```

Expected: PASS.

---

### Task 15: Prevent public-read fallback for SigV4 query auth

**Files:**
- Modify: `internal/s3api/server.go`
- Modify: `internal/s3api/server_test.go`

- [ ] **Step 1: Write the failing public-read interaction tests**

Append these tests near the existing public-read tests in `internal/s3api/server_test.go`:

```go
func TestPublicReadDoesNotBypassPresignedQueryAuth(t *testing.T) {
	server := newPublicReadTestServer(t, map[string]bool{"photos": true})

	put := signedRecorderRequest(t, http.MethodPut, "/photos/public.txt", "hello", nil)
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}

	request := presignServerRequest(t, http.MethodGet, "/photos/public.txt", 15*time.Minute)
	query := request.URL.Query()
	query.Set("response-content-disposition", "tampered")
	request.URL.RawQuery = query.Encode()

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "<Code>SignatureDoesNotMatch</Code>") {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestPublicReadDoesNotBypassIncompletePresignedQueryAuth(t *testing.T) {
	server := newPublicReadTestServer(t, map[string]bool{"photos": true})

	put := signedRecorderRequest(t, http.MethodPut, "/photos/public.txt", "hello", nil)
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/photos/public.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256", nil)
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "<Code>SignatureDoesNotMatch</Code>") {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run:

```bash
go test ./internal/s3api -run 'TestPublicReadDoesNotBypass.*PresignedQueryAuth' -v
```

Expected: FAIL if `allowsAnonymousPublicRead` ignores `X-Amz-*` query parameters and allows public read despite invalid presigned auth.

- [ ] **Step 3: Add a SigV4 query detector**

In `internal/s3api/server.go`, add this helper near `allowsAnonymousPublicRead`:

```go
func hasSigV4QueryAuth(r *http.Request) bool {
	for key := range r.URL.Query() {
		if strings.HasPrefix(strings.ToLower(key), "x-amz-") {
			return true
		}
	}
	return false
}
```

`server.go` already imports `strings`, so no import change is needed.

- [ ] **Step 4: Use the detector in public-read bypass**

Change the first guard in `allowsAnonymousPublicRead` to:

```go
if r.Header.Get("Authorization") != "" || r.Header.Get("X-Amz-Date") != "" || r.Header.Get("X-Amz-Content-Sha256") != "" || hasSigV4QueryAuth(r) {
	return false
}
```

- [ ] **Step 5: Run the public-read interaction tests**

Run:

```bash
go test ./internal/s3api -run 'TestPublicReadDoesNotBypass.*PresignedQueryAuth|TestPublicReadAllowsAnonymousObjectGetAndHead' -v
```

Expected: PASS.

---

### Task 16: Verify presigned URLs behind trusted proxy rewriting

**Files:**
- Modify: `cmd/tgnas/main_test.go`

- [ ] **Step 1: Add the failing trusted-proxy presigned test**

Append this test near `TestTrustedProxyMiddlewareAllowsSigV4SignedForForwardedHost` in `cmd/tgnas/main_test.go`. Add `strconv` to the standard-library imports in that file if it is not already present:

```go
func TestTrustedProxyMiddlewareAllowsPresignedURLSignedForForwardedHost(t *testing.T) {
	s3Handler := s3api.NewServer(proxySigV4ObjectStore{}, s3api.Options{
		Region:      "us-east-1",
		Credentials: map[string]string{"AKID": "SECRET"},
		SigV4Clock:  func() time.Time { return time.Date(2024, 1, 2, 3, 5, 0, 0, time.UTC) },
		Ready:       func() bool { return true },
	})
	handler, err := newTrustedProxyMiddleware(s3Handler, config.ServerConfig{TrustedProxies: []string{"127.0.0.1/32"}})
	if err != nil {
		t.Fatalf("newTrustedProxyMiddleware returned error: %v", err)
	}

	request := httptest.NewRequest(http.MethodHead, "https://s3.example.com/tgnas/test.txt", nil)
	request.Host = request.URL.Host
	query := request.URL.Query()
	query.Set("X-Amz-Expires", strconv.FormatInt(int64((15*time.Minute)/time.Second), 10))
	request.URL.RawQuery = query.Encode()
	credentials := aws.Credentials{AccessKeyID: "AKID", SecretAccessKey: "SECRET"}
	signedURL, _, err := v4.NewSigner().PresignHTTP(context.Background(), credentials, request, "UNSIGNED-PAYLOAD", "s3", "us-east-1", time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), func(options *v4.SignerOptions) {
		options.DisableURIPathEscaping = true
	})
	if err != nil {
		t.Fatalf("PresignHTTP returned error: %v", err)
	}
	request = httptest.NewRequest(http.MethodHead, signedURL, nil)
	request.URL.Scheme = "http"
	request.URL.Host = "127.0.0.1:9000"
	request.Host = "127.0.0.1:9000"
	request.RemoteAddr = "127.0.0.1:54321"
	request.Header.Set("X-Forwarded-Host", "s3.example.com")
	request.Header.Set("X-Forwarded-Proto", "https")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}
```

This uses `HEAD` so the request reaches the object path after auth; `proxySigV4ObjectStore.HeadObject` returns `store.ErrNoSuchKey`, so the expected post-auth result is `NoSuchKey` / HTTP 404 rather than a signature error.

- [ ] **Step 2: Run the test to verify it fails before presigned proxy support is complete**

Run:

```bash
go test ./cmd/tgnas -run 'TestTrustedProxyMiddlewareAllowsPresignedURLSignedForForwardedHost' -v
```

Expected before Tasks 12-14 are complete: FAIL with `SignatureDoesNotMatch`. Expected after Tasks 12-14 are complete: PASS with HTTP 404 from the fake object store.

- [ ] **Step 3: Run all trusted proxy middleware tests**

Run:

```bash
go test ./cmd/tgnas -run 'TestTrustedProxyMiddleware' -v
```

Expected: PASS.

---

### Task 17: Document presigned URL support

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Update README authentication docs**

In `README.md`, find the S3 authentication documentation that currently mentions SigV4 and public read. Extend it with this paragraph:

```markdown
S3 object `GET` and `HEAD` requests may also use SigV4 query-string authentication, commonly called presigned URLs. Presigned URLs use the existing configured credentials and support `X-Amz-Expires` values up to `604800` seconds (7 days). Presigned URLs do not authorize bucket listing, root listing, writes, deletes, copy operations, or WebDAV requests.
```

- [ ] **Step 2: Add a short presigned URL example**

In the same S3 authentication section, add this example after the paragraph:

````markdown
A presigned object URL has the same path-style shape as normal S3 object access:

```text
https://s3.example.com/tgnas/test.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=...&X-Amz-Date=...&X-Amz-Expires=900&X-Amz-SignedHeaders=host&X-Amz-Signature=...
```

If TgNAS is behind a reverse proxy that changes the origin host, configure `trusted_proxies` or `trusted_proxy_hosts` so the verifier sees the external host that was signed.
````

Do not document a new config switch and do not change `data/config.yaml`; presigned URL support is automatic for configured S3 credentials.

- [ ] **Step 3: Run docs-adjacent tests**

Run:

```bash
go test ./internal/s3api -run 'TestVerifySigV4Presigned|TestPresigned|TestPublicReadDoesNotBypass' -v
```

Expected: PASS.

---

### Task 18: Full presigned URL verification and final review loop

**Files:**
- Read-only verification across the repository.

- [ ] **Step 1: Run focused S3 presigned tests**

Run:

```bash
go test ./internal/s3api -run 'TestVerifySigV4Presigned|TestPresigned|TestPublicReadDoesNotBypass|TestRootAcceptHTMLWithS3QueryDoesNotUseHTMLShortcut' -v
```

Expected: PASS.

- [ ] **Step 2: Run focused trusted-proxy tests**

Run:

```bash
go test ./cmd/tgnas -run 'TestTrustedProxyMiddleware' -v
```

Expected: PASS, including both header SigV4 and presigned URL proxy tests.

- [ ] **Step 3: Run all Go tests**

Run:

```bash
go test ./...
```

Expected: PASS.

- [ ] **Step 4: Build the CLI**

Run:

```bash
go build ./cmd/tgnas
```

Expected: exit code 0.

- [ ] **Step 5: Inspect the full diff**

Run:

```bash
git diff -- README.md data/config.yaml config/config.go config/config_test.go internal/s3api/sigv4.go internal/s3api/sigv4_test.go internal/s3api/server.go internal/s3api/server_test.go cmd/tgnas/main.go cmd/tgnas/main_test.go
```

Expected: diff contains public-read, trusted-proxy, and presigned object `GET`/`HEAD` support only. There should be no presigned writes, deletes, listing, WebDAV anonymous access, or broad proxy-header trust beyond the configured trust rules.

- [ ] **Step 6: Request final code review**

Dispatch a fresh code-review subagent with this context:

```text
Review the uncommitted TgNAS changes implementing three S3 compatibility features: bucket-level public_read anonymous object GET/HEAD, trusted reverse-proxy forwarded host/proto handling, and SigV4 query-string presigned URL verification for object GET/HEAD only. Verify against docs/superpowers/specs/2026-05-24-tgnas-public-read-design.md. Pay particular attention to auth boundary issues: public read must not expose listing or writes; trusted proxy trust is remote-IP OR forwarded-host; remote-IP trust accepts any forwarded host; presigned URLs must exclude X-Amz-Signature from canonical query, enforce max 7-day expiry, use UNSIGNED-PAYLOAD, reject unsupported operations, and must not fall back to anonymous public-read behavior when query auth is invalid.

Report Critical, Important, and Minor issues. Do not modify files.
```

If the reviewer reports Critical or Important issues, verify each finding against the codebase, fix valid issues with TDD, rerun focused tests and `go test ./...`, then dispatch another review subagent. Repeat until no Critical or Important issues remain.

- [ ] **Step 7: Confirm no commit was created**

Run:

```bash
git status --short
```

Expected: files are modified but no new commit has been created by this addendum execution.

---

## Presigned URL Self-Review Notes

- Spec coverage: verifier-level query auth tests are covered in Task 11; shared query/header verification implementation and canonical query exclusion for `X-Amz-Signature` are covered in Task 12; verifier regression checks are covered in Task 13; response override tampering, expiration, and 7-day maximum are covered in Tasks 11-13; server-level object `GET`/`HEAD` restriction is covered in Task 14; public-read interaction is covered in Task 15; trusted-proxy interaction is covered in Task 16; docs are covered in Task 17; final verification and repeated review are covered in Task 18.
- Placeholder scan: no TBD/TODO/fill-in-later steps remain. Every code-changing step includes concrete code.
- Type consistency: the plan uses `Identity.Presigned` as the single verifier-to-server signal for query-string auth. Header-auth identities leave `Presigned` false, preserving existing authenticated behavior.
- Security boundaries: presigned auth never broadens public-read, never authorizes listing or writes, and fails unsupported operations with `SignatureDoesNotMatch` instead of anonymous fallback.
- Trust semantics: presigned URLs still interact with the trusted proxy rules already described in the main plan: `trusted_proxies` matches by remote IP and accepts any forwarded host, while `trusted_proxy_hosts` matches by forwarded host; either match is sufficient.
- User constraint: no commit steps are included because the user requested not to commit.
