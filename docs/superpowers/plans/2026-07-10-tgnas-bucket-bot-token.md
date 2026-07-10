# TgNAS Bucket Bot Token Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add optional bucket-level Telegram bot token overrides with global fallback, per-token upload pacing, and bucket-scoped runtime unavailability without persisting tokens into metadata.

**Architecture:** Extend config parsing so each configured bucket resolves to a final token plus a source label. Build a runtime binding map in `cmd/tgnas/main.go`, pass it into `store.ObjectStore`, route every Telegram call by bucket, prebuild upload gates per unique token, and classify Telegram failures through normalized error metadata instead of status-code-only checks.

**Tech Stack:** Go 1.24, stdlib (`context`, `errors`, `fmt`, `sync`, `net/http`, `crypto/sha256`, `encoding/hex`), existing `config`, `store`, `telegram`, SQLite-backed metadata tests, `httptest`

## Global Constraints

- Add global `telegram.bot_token` with `${ENV}` resolution support
- Add `buckets.<name>.bot_token` with `${ENV}` resolution support
- Keep `telegram.bot_token_env` for backward compatibility
- Precedence: `bucket.bot_token` > global `telegram.bot_token` > `telegram.bot_token_env`
- On startup, only check whether the final resolved token is empty; do not pre-validate token authenticity with Telegram API calls
- If the final resolved token for any configured bucket is empty, service startup must fail
- Tokens must not be written to metadata/SQLite; they only exist in config and runtime memory
- Only buckets explicitly declared in config are supported; dynamic or implicit buckets are not supported at runtime
- All Telegram calls related to a configured bucket must use that bucket's final resolved token
- Upload serialization and `429` cooldown must be isolated per token; different tokens must not affect each other
- Only Telegram errors that can be clearly classified as bucket-level token authentication/authorization failures may mark a bucket as runtime-unavailable; HTTP `403` alone must not trip bucket-wide unavailability without considering operation type and error semantics
- Object-level read failures, such as old objects becoming unreadable after a token change or a `file_id` being inaccessible under the current bot, must not mark the whole bucket unavailable
- Logs must include bucket, operation type, token source, and failure reason, but must not print the raw token, derived token key, or any reversible identifier

---

### Task 1: Add config-level token resolution and startup-safe validation

**Files:**
- Modify: `config/config.go:58-64`
- Modify: `config/config.go:84-87`
- Modify: `config/config.go:120-164`
- Modify: `config/config.go:196-205`
- Modify: `config/config.go:219-297`
- Modify: `config/config.go:336-347`
- Modify: `config/config_test.go:11-260`

**Interfaces:**
- Consumes: `func (c Config) ResolveSecret(envName string) string` from `config/config.go`
- Consumes: `func resolveBucketChatID(value string) (string, error)` from `config/config.go`
- Produces: `func resolveConfigValue(value string) (string, error)` in `config/config.go`
- Produces: `func (c Config) ResolveBotToken() (string, string, error)` in `config/config.go`
- Produces: `func (c Config) ResolveBucketToken(name string) (string, string, error)` in `config/config.go`
- Produces: `TelegramConfig.BotToken string` and `BucketConfig.BotToken string`

- [ ] **Step 1: Write the failing config tests**

```go
func TestResolveBotTokenPrefersExplicitValueBeforeEnvName(t *testing.T) {
	cfg := Config{
		Telegram: TelegramConfig{
			BotToken:    "${TGNAS_BOT_TOKEN_VALUE}",
			BotTokenEnv: "TGNAS_BOT_TOKEN_ENV",
		},
	}
	t.Setenv("TGNAS_BOT_TOKEN_VALUE", "123:explicit")
	t.Setenv("TGNAS_BOT_TOKEN_ENV", "456:legacy")

	token, source, err := cfg.ResolveBotToken()
	if err != nil {
		t.Fatalf("ResolveBotToken returned error: %v", err)
	}
	if token != "123:explicit" {
		t.Fatalf("token = %q, want %q", token, "123:explicit")
	}
	if source != "global" {
		t.Fatalf("source = %q, want %q", source, "global")
	}
}

func TestResolveBucketTokenPrefersBucketThenGlobalThenLegacyEnv(t *testing.T) {
	cfg := Config{
		Telegram: TelegramConfig{
			BotToken:    "${TGNAS_GLOBAL_TOKEN}",
			BotTokenEnv: "TGNAS_LEGACY_TOKEN",
		},
		Buckets: map[string]BucketConfig{
			"photos":  {ChatID: "-100", BotToken: "${TGNAS_BUCKET_TOKEN}"},
			"archive": {ChatID: "-200"},
		},
	}
	t.Setenv("TGNAS_BUCKET_TOKEN", "111:bucket")
	t.Setenv("TGNAS_GLOBAL_TOKEN", "222:global")
	t.Setenv("TGNAS_LEGACY_TOKEN", "333:legacy")

	bucketToken, bucketSource, err := cfg.ResolveBucketToken("photos")
	if err != nil {
		t.Fatalf("ResolveBucketToken(photos) returned error: %v", err)
	}
	if bucketToken != "111:bucket" || bucketSource != "bucket" {
		t.Fatalf("photos = (%q, %q), want (%q, %q)", bucketToken, bucketSource, "111:bucket", "bucket")
	}

	globalToken, globalSource, err := cfg.ResolveBucketToken("archive")
	if err != nil {
		t.Fatalf("ResolveBucketToken(archive) returned error: %v", err)
	}
	if globalToken != "222:global" || globalSource != "global" {
		t.Fatalf("archive = (%q, %q), want (%q, %q)", globalToken, globalSource, "222:global", "global")
	}
}

func TestResolveBucketTokenFallsBackToLegacyEnvWhenExplicitGlobalEmpty(t *testing.T) {
	cfg := Config{
		Telegram: TelegramConfig{
			BotToken:    "${TGNAS_EMPTY_TOKEN}",
			BotTokenEnv: "TGNAS_LEGACY_TOKEN",
		},
		Buckets: map[string]BucketConfig{
			"photos": {ChatID: "-100"},
		},
	}
	t.Setenv("TGNAS_EMPTY_TOKEN", "")
	t.Setenv("TGNAS_LEGACY_TOKEN", "333:legacy")

	token, source, err := cfg.ResolveBucketToken("photos")
	if err != nil {
		t.Fatalf("ResolveBucketToken returned error: %v", err)
	}
	if token != "333:legacy" || source != "global_env" {
		t.Fatalf("ResolveBucketToken = (%q, %q), want (%q, %q)", token, source, "333:legacy", "global_env")
	}
}

func TestLoadFileRejectsInvalidPartialEnvReferenceInBucketToken(t *testing.T) {
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
  photos:
    chat_id: "-100123"
    bot_token: "prefix-${TGNAS_BUCKET_TOKEN}"
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, err = LoadFile(path)
	if err == nil || !strings.Contains(err.Error(), "environment reference must use full ${ENV_NAME} form") {
		t.Fatalf("LoadFile error = %v", err)
	}
}
```

- [ ] **Step 2: Run config tests to verify they fail**

Run: `go test ./config -run 'TestResolveBotTokenPrefersExplicitValueBeforeEnvName|TestResolveBucketTokenPrefersBucketThenGlobalThenLegacyEnv|TestResolveBucketTokenFallsBackToLegacyEnvWhenExplicitGlobalEmpty|TestLoadFileRejectsInvalidPartialEnvReferenceInBucketToken' -count=1`
Expected: FAIL because `TelegramConfig.BotToken`, `BucketConfig.BotToken`, `ResolveBucketToken`, and tuple-returning `ResolveBotToken` do not exist yet.

- [ ] **Step 3: Implement shared config value resolution and token precedence**

```go
type TelegramConfig struct {
	BotToken        string        `yaml:"bot_token"`
	BotTokenEnv     string        `yaml:"bot_token_env"`
	APIBaseURL      string        `yaml:"api_base_url"`
	CaptionTemplate string        `yaml:"caption_template"`
	Timeout         time.Duration `yaml:"-"`
	RawTimeout      Duration      `yaml:"timeout"`
}

type BucketConfig struct {
	ChatID     string `yaml:"chat_id"`
	BotToken   string `yaml:"bot_token"`
	PublicRead bool   `yaml:"public_read"`
}
```

```go
func resolveConfigValue(value string) (string, error) {
	hasEnvReference := strings.Contains(value, "${") || strings.Contains(value, "}")
	isFullEnvReference := strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") && strings.Count(value, "${") == 1 && strings.Count(value, "}") == 1
	if hasEnvReference && !isFullEnvReference {
		return "", fmt.Errorf("environment reference must use full ${ENV_NAME} form")
	}
	if isFullEnvReference {
		envName := value[2 : len(value)-1]
		return os.Getenv(envName), nil
	}
	return value, nil
}

func resolveBucketChatID(value string) (string, error) {
	return resolveConfigValue(value)
}
```

```go
func (c Config) ResolveBotToken() (string, string, error) {
	if strings.TrimSpace(c.Telegram.BotToken) != "" {
		value, err := resolveConfigValue(c.Telegram.BotToken)
		if err != nil {
			return "", "", err
		}
		if strings.TrimSpace(value) != "" {
			return value, "global", nil
		}
	}
	if strings.TrimSpace(c.Telegram.BotTokenEnv) != "" {
		return c.ResolveSecret(c.Telegram.BotTokenEnv), "global_env", nil
	}
	return "", "", nil
}

func (c Config) ResolveBucketToken(name string) (string, string, error) {
	bucket, ok := c.Buckets[name]
	if !ok {
		return "", "", fmt.Errorf("bucket %q not configured", name)
	}
	if strings.TrimSpace(bucket.BotToken) != "" {
		value, err := resolveConfigValue(bucket.BotToken)
		if err != nil {
			return "", "", fmt.Errorf("resolve bucket %q bot_token: %w", name, err)
		}
		if strings.TrimSpace(value) != "" {
			return value, "bucket", nil
		}
	}
	return c.ResolveBotToken()
}
```

```go
for name, bucket := range cfg.Buckets {
	chatID, err := resolveBucketChatID(bucket.ChatID)
	if err != nil {
		return Config{}, fmt.Errorf("resolve bucket %q chat_id: %w", name, err)
	}
	bucket.ChatID = chatID
	if strings.TrimSpace(bucket.BotToken) != "" {
		resolvedToken, err := resolveConfigValue(bucket.BotToken)
		if err != nil {
			return Config{}, fmt.Errorf("resolve bucket %q bot_token: %w", name, err)
		}
		bucket.BotToken = resolvedToken
	}
	cfg.Buckets[name] = bucket
}
```

```go
if strings.TrimSpace(c.Telegram.APIBaseURL) == "" {
	return fmt.Errorf("telegram api base url is required")
}
```

- [ ] **Step 4: Add startup-oriented config validation tests**

```go
func TestLoadFileAllowsMissingTelegramBotTokenEnvWhenExplicitBotTokenIsUsed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	err := os.WriteFile(path, []byte(`
auth:
  credentials:
    - access_key: "admin"
      secret_key_env: "SECRET"
telegram:
  bot_token: "${TGNAS_GLOBAL_TOKEN}"
buckets:
  photos:
    chat_id: "-100123"
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TGNAS_GLOBAL_TOKEN", "123:global")

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile returned error: %v", err)
	}
	if token, source, err := cfg.ResolveBucketToken("photos"); err != nil || token != "123:global" || source != "global" {
		t.Fatalf("ResolveBucketToken = (%q, %q, %v)", token, source, err)
	}
}
```

- [ ] **Step 5: Run config package tests and verify they pass**

Run: `go test ./config -count=1`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add config/config.go config/config_test.go
git commit -m "feat(config): resolve bucket bot tokens"
```

### Task 2: Build runtime bucket bindings in main and pass them into store

**Files:**
- Modify: `cmd/tgnas/main.go:726-801`
- Modify: `store/store.go:26-42`
- Modify: `store/store.go:82-141`
- Modify: `store/types.go:139-146`
- Modify: `store/store_test.go:1220-1253`

**Interfaces:**
- Consumes: `func (c Config) ResolveBucketToken(name string) (string, string, error)` from Task 1
- Produces: `type BucketBinding struct { Name string; ChatID string; TokenSource string; TokenKey string; Telegram telegram.Client }` in `store/store.go`
- Produces: `func NewBucketBinding(name, chatID, token, source, apiBaseURL string, client *http.Client) BucketBinding` logic in `cmd/tgnas/main.go`
- Produces: `Options.Buckets map[string]BucketBinding` in `store/types.go`

- [ ] **Step 1: Write failing startup/runtime binding tests**

```go
func TestRunServiceCreatesBucketSpecificTelegramClients(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	err := os.WriteFile(configPath, []byte(`
auth:
  credentials:
    - access_key: "admin"
      secret_key_env: "SECRET"
telegram:
  bot_token: "${TGNAS_GLOBAL_TOKEN}"
metadata:
  sqlite_path: "`+filepath.Join(t.TempDir(), "metadata.sqlite")+`"
buckets:
  photos:
    chat_id: "-100123"
    bot_token: "${TGNAS_PHOTOS_TOKEN}"
  archive:
    chat_id: "-100456"
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SECRET", "secret")
	t.Setenv("TGNAS_GLOBAL_TOKEN", "222:global")
	t.Setenv("TGNAS_PHOTOS_TOKEN", "111:photos")

	captured := store.Options{}
	oldNewObjectStore := newObjectStore
	newObjectStore = func(meta metadata.Store, tg telegram.Client, options store.Options) (*store.ObjectStore, error) {
		captured = options
		return store.NewObjectStore(meta, tg, options)
	}
	defer func() { newObjectStore = oldNewObjectStore }()
	oldListen := listenAndServe
	listenAndServe = func(addr string, handler http.Handler) error { return errors.New("stop after setup") }
	defer func() { listenAndServe = oldListen }()

	err = runServiceWithDebug(configPath, serverModeS3, newDebugLogger(false, io.Discard))
	if err == nil || !strings.Contains(err.Error(), "stop after setup") {
		t.Fatalf("runServiceWithDebug error = %v", err)
	}
	if len(captured.Buckets) != 2 {
		t.Fatalf("len(captured.Buckets) = %d, want 2", len(captured.Buckets))
	}
	if captured.Buckets["photos"].TokenSource != "bucket" {
		t.Fatalf("photos source = %q, want bucket", captured.Buckets["photos"].TokenSource)
	}
	if captured.Buckets["archive"].TokenSource != "global" {
		t.Fatalf("archive source = %q, want global", captured.Buckets["archive"].TokenSource)
	}
}
```

- [ ] **Step 2: Run targeted main/store tests to verify they fail**

Run: `go test ./cmd/tgnas ./store -run 'TestRunServiceCreatesBucketSpecificTelegramClients|TestStoreHeadBucketUsesStartupConfiguredMetadata' -count=1`
Expected: FAIL because `store.Options` does not carry bucket bindings and `runServiceWithDebug` still constructs only one global Telegram client.

- [ ] **Step 3: Add binding types and wire startup construction**

```go
type BucketBinding struct {
	Name        string
	ChatID      string
	TokenSource string
	TokenKey    string
	Telegram    telegram.Client
}

type Options struct {
	Upload           UploadConfig
	Caption          *telegram.CaptionTemplate
	Buckets          map[string]BucketBinding
	MaxUploads       int
	MaxDownloads     int
	MaxTelegramCalls int
	Logger           *log.Logger
}
```

```go
func tokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
```

```go
bucketBindings := map[string]store.BucketBinding{}
for name, bucket := range cfg.Buckets {
	finalToken, source, err := cfg.ResolveBucketToken(name)
	if err != nil {
		return fmt.Errorf("resolve bucket %s bot token: %w", name, err)
	}
	if strings.TrimSpace(finalToken) == "" {
		return fmt.Errorf("resolve bucket %s bot token: empty final token", name)
	}
	bucketBindings[name] = store.BucketBinding{
		Name:        name,
		ChatID:      bucket.ChatID,
		TokenSource: source,
		TokenKey:    tokenKey(finalToken),
		Telegram: telegram.NewHTTPClient(finalToken, cfg.Telegram.APIBaseURL, &http.Client{
			Timeout: cfg.Telegram.Timeout,
		}),
	}
}
```

```go
objectStore, err := newObjectStore(meta, nil, store.Options{
	Upload: store.UploadConfig{
		Strategy:       cfg.Storage.UploadTypeStrategy,
		EnableChunking: *cfg.Storage.EnableChunking,
		MaxFileSize:    cfg.Storage.MaxFileSize,
		ChunkSize:      cfg.Storage.ChunkSize,
		TypeLimits:     cfg.Storage.TypeSizeLimits,
		PutBufferSize:  cfg.Storage.PutBufferSize,
	},
	Caption:          caption,
	Buckets:          bucketBindings,
	MaxUploads:       cfg.Storage.MaxConcurrentUploads,
	MaxDownloads:     cfg.Storage.MaxConcurrentDownloads,
	MaxTelegramCalls: cfg.Storage.MaxConcurrentTelegramRequests,
	Logger:           dbg.StdLogger(),
})
```

- [ ] **Step 4: Add runtime binding tests in store**

```go
func TestNewObjectStoreRequiresBindingsForStartupBuckets(t *testing.T) {
	ctx := context.Background()
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: "photos", ChatID: "-100", CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
		t.Fatalf("UpsertBucket returned error: %v", err)
	}

	_, err = NewObjectStore(meta, nil, Options{Upload: DefaultUploadConfig(), Buckets: map[string]BucketBinding{}})
	if err == nil || !strings.Contains(err.Error(), "missing runtime bucket binding") {
		t.Fatalf("NewObjectStore error = %v", err)
	}
}
```

- [ ] **Step 5: Run targeted startup/binding tests and verify they pass**

Run: `go test ./cmd/tgnas ./store -run 'TestRunServiceCreatesBucketSpecificTelegramClients|TestNewObjectStoreRequiresBindingsForStartupBuckets|TestStoreHeadBucketUsesStartupConfiguredMetadata' -count=1`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add cmd/tgnas/main.go store/store.go store/store_test.go store/types.go
git commit -m "feat(startup): build bucket telegram bindings"
```

### Task 3: Route Telegram calls by bucket and prebuild per-token gates

**Files:**
- Modify: `store/store.go:26-42`
- Modify: `store/store.go:82-141`
- Modify: `store/store.go:168-214`
- Modify: `store/store.go:550-619`
- Modify: `store/store.go:875-1217`
- Modify: `store/store_test.go:565-715`
- Modify: `internal/testutil/faketelegram.go:14-64`

**Interfaces:**
- Consumes: `Options.Buckets map[string]BucketBinding` from Task 2
- Produces: `type uploadGate struct { slot chan struct{}; mu sync.Mutex; until time.Time }` in `store/store.go`
- Produces: `func (s *ObjectStore) bucketBinding(name string) (BucketBinding, error)` in `store/store.go`
- Produces: `func (s *ObjectStore) uploadTelegram(ctx context.Context, binding BucketBinding, request telegram.UploadRequest) (telegram.UploadedFile, error)` in `store/store.go`
- Produces: `func (s *ObjectStore) downloadChunk(ctx context.Context, binding BucketBinding, fileID string) (io.ReadCloser, error)` in `store/store.go`

- [ ] **Step 1: Write failing gate-sharing and bucket-routing tests**

```go
func TestStoreBucketsWithDifferentBindingsUseDifferentTelegramClients(t *testing.T) {
	ctx := context.Background()
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	for name, chatID := range map[string]string{"photos": "-100", "archive": "-200"} {
		if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: name, ChatID: chatID, CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
			t.Fatalf("UpsertBucket(%s) returned error: %v", name, err)
		}
	}
	photoTG := testutil.NewFakeTelegram()
	archiveTG := testutil.NewFakeTelegram()
	caption, _ := telegram.ParseCaptionTemplate("")
	store := mustNewBucketBindingStore(t, meta, map[string]BucketBinding{
		"photos":  {Name: "photos", ChatID: "-100", TokenSource: "bucket", TokenKey: "token-a", Telegram: photoTG},
		"archive": {Name: "archive", ChatID: "-200", TokenSource: "global", TokenKey: "token-b", Telegram: archiveTG},
	}, Options{Upload: DefaultUploadConfig(), Caption: caption})

	_, err = store.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "one.txt", ContentType: "text/plain", Size: 1, Body: strings.NewReader("a")})
	if err != nil {
		t.Fatalf("photos PutObject returned error: %v", err)
	}
	_, err = store.PutObject(ctx, PutObjectInput{Bucket: "archive", Key: "two.txt", ContentType: "text/plain", Size: 1, Body: strings.NewReader("b")})
	if err != nil {
		t.Fatalf("archive PutObject returned error: %v", err)
	}
	if len(photoTG.Uploads) != 1 || photoTG.Uploads[0].ChatID != "-100" {
		t.Fatalf("photoTG.Uploads = %+v", photoTG.Uploads)
	}
	if len(archiveTG.Uploads) != 1 || archiveTG.Uploads[0].ChatID != "-200" {
		t.Fatalf("archiveTG.Uploads = %+v", archiveTG.Uploads)
	}
}

func TestStoreSameTokenBucketsShareCooldownGate(t *testing.T) {
	ctx := context.Background()
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	for name, chatID := range map[string]string{"photos": "-100", "archive": "-200"} {
		if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: name, ChatID: chatID, CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
			t.Fatalf("UpsertBucket(%s) returned error: %v", name, err)
		}
	}
	fake := testutil.NewFakeTelegram()
	started := make(chan string, 2)
	fake.UploadFunc = func(ctx context.Context, request telegram.UploadRequest) (telegram.UploadedFile, error) {
		started <- request.ChatID + ":" + request.Filename
		_, _ = io.ReadAll(request.Reader)
		if request.Filename == "one.txt" {
			return telegram.UploadedFile{}, telegram.NewRateLimitError(errors.New("retry after 1"), time.Second)
		}
		return telegram.UploadedFile{Type: request.Type, FileID: request.Filename, FileUniqueID: request.Filename + "-u", MessageID: 1, FileSize: 1}, nil
	}
	caption, _ := telegram.ParseCaptionTemplate("")
	store := mustNewBucketBindingStore(t, meta, map[string]BucketBinding{
		"photos":  {Name: "photos", ChatID: "-100", TokenSource: "bucket", TokenKey: "shared", Telegram: fake},
		"archive": {Name: "archive", ChatID: "-200", TokenSource: "bucket", TokenKey: "shared", Telegram: fake},
	}, Options{Upload: DefaultUploadConfig(), Caption: caption, MaxTelegramCalls: 2})

	firstDone := make(chan error, 1)
	go func() {
		_, err := store.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "one.txt", ContentType: "text/plain", Size: 1, Body: strings.NewReader("a")})
		firstDone <- err
	}()
	<-started
	if err := <-firstDone; err == nil {
		t.Fatal("first PutObject returned nil error")
	}

	secondCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err = store.PutObject(secondCtx, PutObjectInput{Bucket: "archive", Key: "two.txt", ContentType: "text/plain", Size: 1, Body: strings.NewReader("b")})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second PutObject err = %v, want context.DeadlineExceeded", err)
	}
	select {
	case started := <-started:
		t.Fatalf("shared gate did not hold cooldown, second upload started: %s", started)
	default:
	}
}
```

- [ ] **Step 2: Run targeted store tests to verify they fail**

Run: `go test ./store -run 'TestStoreBucketsWithDifferentBindingsUseDifferentTelegramClients|TestStoreSameTokenBucketsShareCooldownGate|TestStoreUploadGateRecordsCooldownFromFinalRateLimit|TestStoreMaxTelegramCallsSerializationWhenSetToOne' -count=1`
Expected: FAIL because `ObjectStore` still keeps one `tg` field and one global upload gate.

- [ ] **Step 3: Replace single-client routing and global gate state**

```go
type uploadGate struct {
	slot chan struct{}
	mu   sync.Mutex
	until time.Time
}

type ObjectStore struct {
	meta               metadata.Store
	options            Options
	resolver           *UploadStrategyResolver
	locker             *KeyedLocker
	uploads            chan struct{}
	downloads          chan struct{}
	telegramSem        chan struct{}
	logger             *log.Logger
	startupBuckets     map[string]metadata.Bucket
	bucketBindings     map[string]BucketBinding
	uploadGates        map[string]*uploadGate
	multipartMu        sync.Mutex
	multipartUploads   map[string]*multipartUpload
	unavailableMu      sync.RWMutex
	unavailableBuckets map[string]bool
}
```

```go
func newUploadGate() *uploadGate {
	gate := &uploadGate{slot: make(chan struct{}, 1)}
	gate.slot <- struct{}{}
	return gate
}

func (s *ObjectStore) bucketBinding(name string) (BucketBinding, error) {
	binding, ok := s.bucketBindings[name]
	if !ok {
		return BucketBinding{}, ErrNoSuchBucket
	}
	return binding, nil
}
```

```go
func NewObjectStore(meta metadata.Store, tg telegram.Client, options Options) (*ObjectStore, error) {
	_ = tg
	// existing upload defaulting kept intact
	store := &ObjectStore{
		meta:               meta,
		options:            options,
		resolver:           NewUploadStrategyResolver(upload),
		locker:             NewKeyedLocker(),
		logger:             options.Logger,
		startupBuckets:     map[string]metadata.Bucket{},
		bucketBindings:     map[string]BucketBinding{},
		uploadGates:        map[string]*uploadGate{},
		multipartUploads:   map[string]*multipartUpload{},
		unavailableBuckets: map[string]bool{},
	}
	for name, binding := range options.Buckets {
		store.bucketBindings[name] = binding
		if _, ok := store.uploadGates[binding.TokenKey]; !ok {
			store.uploadGates[binding.TokenKey] = newUploadGate()
		}
	}
	for _, bucket := range buckets {
		if !bucket.Enabled {
			continue
		}
		if _, ok := store.bucketBindings[bucket.Name]; !ok {
			return nil, fmt.Errorf("missing runtime bucket binding for %q", bucket.Name)
		}
		store.startupBuckets[bucket.Name] = bucket
	}
	return store, nil
}
```

```go
func (s *ObjectStore) uploadTelegram(ctx context.Context, binding BucketBinding, request telegram.UploadRequest) (telegram.UploadedFile, error) {
	gate := s.uploadGates[binding.TokenKey]
	select {
	case <-gate.slot:
	case <-ctx.Done():
		return telegram.UploadedFile{}, ctx.Err()
	}
	defer func() { gate.slot <- struct{}{} }()

	for {
		gate.mu.Lock()
		wait := time.Until(gate.until)
		gate.mu.Unlock()
		if wait <= 0 {
			break
		}
		if err := sleepWithContext(ctx, wait); err != nil {
			return telegram.UploadedFile{}, err
		}
	}

	uploaded, err := binding.Telegram.Upload(ctx, request)
	if err != nil {
		if retryAfter, ok := telegram.IsRateLimitError(err); ok && retryAfter > 0 {
			gate.mu.Lock()
			gate.until = time.Now().Add(retryAfter)
			gate.mu.Unlock()
		}
		return telegram.UploadedFile{}, err
	}
	return uploaded, nil
}
```

```go
func (s *ObjectStore) downloadChunk(ctx context.Context, binding BucketBinding, fileID string) (io.ReadCloser, error) {
	release := s.acquire(ctx, s.telegramSem)
	if release == nil {
		return nil, ctx.Err()
	}
	defer release()
	return binding.Telegram.Download(ctx, fileID)
}
```

- [ ] **Step 4: Update call sites and helper fixtures**

```go
func (s *ObjectStore) bucketChatID(bucket string) string {
	binding, err := s.bucketBinding(bucket)
	if err != nil {
		return ""
	}
	return binding.ChatID
}
```

```go
func mustNewBucketBindingStore(t *testing.T, meta metadata.Store, bindings map[string]BucketBinding, options Options) *ObjectStore {
	t.Helper()
	options.Buckets = bindings
	store, err := NewObjectStore(meta, nil, options)
	if err != nil {
		t.Fatalf("NewObjectStore returned error: %v", err)
	}
	return store
}
```

- [ ] **Step 5: Run store routing and cooldown tests and verify they pass**

Run: `go test ./store -run 'TestStoreBucketsWithDifferentBindingsUseDifferentTelegramClients|TestStoreSameTokenBucketsShareCooldownGate|TestStoreUploadGateRecordsCooldownFromFinalRateLimit|TestStoreUploadSerializationDoesNotBlockDownloads|TestStoreMaxTelegramCallsSerializationWhenSetToOne' -count=1`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add store/store.go store/store_test.go internal/testutil/faketelegram.go store/types.go
git commit -m "feat(store): route telegram by bucket token"
```

### Task 4: Normalize Telegram auth errors and add bucket-unavailable behavior

**Files:**
- Modify: `telegram/client.go:24-57`
- Modify: `telegram/client.go:102-180`
- Modify: `telegram/client.go:270-454`
- Modify: `telegram/client_test.go:244-320`
- Modify: `store/store.go:536-735`
- Modify: `store/store.go:1056-1217`
- Modify: `store/store_test.go:565-1253`

**Interfaces:**
- Consumes: `BucketBinding` and per-token `uploadGate` from Task 3
- Produces: `type OperationClass string` in `telegram/client.go`
- Produces: `type RequestError struct { Op OperationClass; StatusCode int; Reason string; err error }` in `telegram/client.go`
- Produces: `func ClassifyRequestError(err error) (RequestError, bool)` in `telegram/client.go`
- Produces: `func (s *ObjectStore) markBucketUnavailable(name string) bool` in `store/store.go`
- Produces: `func (s *ObjectStore) isBucketUnavailable(name string) bool` in `store/store.go`
- Produces: `var ErrUnavailable = errors.New("bucket unavailable")` in `store/types.go`

- [ ] **Step 1: Write the failing normalization and unavailable-bucket tests**

```go
func TestTelegramClassifyRequestErrorIncludesOperationStatusAndReason(t *testing.T) {
	err := NewRequestError(OperationUploadSend, http.StatusUnauthorized, "chat_access_denied", errors.New("Unauthorized"))
	classified, ok := ClassifyRequestError(err)
	if !ok {
		t.Fatal("ClassifyRequestError returned ok=false")
	}
	if classified.Op != OperationUploadSend {
		t.Fatalf("classified.Op = %q, want %q", classified.Op, OperationUploadSend)
	}
	if classified.StatusCode != http.StatusUnauthorized {
		t.Fatalf("classified.StatusCode = %d, want %d", classified.StatusCode, http.StatusUnauthorized)
	}
	if classified.Reason != "chat_access_denied" {
		t.Fatalf("classified.Reason = %q, want %q", classified.Reason, "chat_access_denied")
	}
}

func TestStoreOldObjectReadFailureDoesNotMarkBucketUnavailable(t *testing.T) {
	ctx := context.Background()
	objectStore, fake := newReadyTestObjectStore(t, map[string]string{"photos": "-100"})
	_, err := objectStore.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "hello.txt", ContentType: "text/plain", Size: 5, Body: strings.NewReader("hello")})
	if err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}
	fake.DownloadFunc = func(ctx context.Context, fileID string) (io.ReadCloser, error) {
		return nil, telegram.NewRequestError(telegram.OperationDownloadRead, http.StatusForbidden, "file_inaccessible", errors.New("Forbidden"))
	}

	_, _, err = objectStore.GetObject(ctx, GetObjectInput{Bucket: "photos", Key: "hello.txt"})
	if err == nil {
		t.Fatal("GetObject returned nil error")
	}
	if objectStore.isBucketUnavailable("photos") {
		t.Fatal("bucket marked unavailable after object-level read failure")
	}
}

func TestStoreUploadAuthFailureMarksOnlyThatBucketUnavailable(t *testing.T) {
	ctx := context.Background()
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	for name, chatID := range map[string]string{"photos": "-100", "archive": "-200"} {
		if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: name, ChatID: chatID, CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
			t.Fatalf("UpsertBucket(%s) returned error: %v", name, err)
		}
	}
	shared := testutil.NewFakeTelegram()
	shared.UploadFunc = func(ctx context.Context, request telegram.UploadRequest) (telegram.UploadedFile, error) {
		if request.ChatID == "-100" {
			return telegram.UploadedFile{}, telegram.NewRequestError(telegram.OperationUploadSend, http.StatusUnauthorized, "chat_access_denied", errors.New("Unauthorized"))
		}
		_, _ = io.ReadAll(request.Reader)
		return telegram.UploadedFile{Type: request.Type, FileID: request.Filename, FileUniqueID: request.Filename + "-u", MessageID: 1, FileSize: 1}, nil
	}
	caption, _ := telegram.ParseCaptionTemplate("")
	store := mustNewBucketBindingStore(t, meta, map[string]BucketBinding{
		"photos":  {Name: "photos", ChatID: "-100", TokenSource: "bucket", TokenKey: "shared", Telegram: shared},
		"archive": {Name: "archive", ChatID: "-200", TokenSource: "bucket", TokenKey: "shared", Telegram: shared},
	}, Options{Upload: DefaultUploadConfig(), Caption: caption})

	_, err = store.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "one.txt", ContentType: "text/plain", Size: 1, Body: strings.NewReader("a")})
	if err == nil {
		t.Fatal("photos PutObject returned nil error")
	}
	if !store.isBucketUnavailable("photos") {
		t.Fatal("photos bucket not marked unavailable")
	}
	if store.isBucketUnavailable("archive") {
		t.Fatal("archive bucket should remain available")
	}
}
```

- [ ] **Step 2: Run targeted telegram/store tests to verify they fail**

Run: `go test ./telegram ./store -run 'TestTelegramClassifyRequestErrorIncludesOperationStatusAndReason|TestStoreOldObjectReadFailureDoesNotMarkBucketUnavailable|TestStoreUploadAuthFailureMarksOnlyThatBucketUnavailable' -count=1`
Expected: FAIL because structured request classification and bucket unavailable state do not exist.

- [ ] **Step 3: Add normalized Telegram request error metadata**

```go
type OperationClass string

const (
	OperationUploadSend  OperationClass = "upload_send"
	OperationDownloadRead OperationClass = "download_read"
)

type RequestError struct {
	Op         OperationClass
	StatusCode int
	Reason     string
	cause      error
}

func (e *RequestError) Error() string { return e.cause.Error() }
func (e *RequestError) Unwrap() error { return e.cause }

func NewRequestError(op OperationClass, statusCode int, reason string, cause error) error {
	if cause == nil {
		cause = errors.New(reason)
	}
	return &RequestError{Op: op, StatusCode: statusCode, Reason: reason, cause: cause}
}

func ClassifyRequestError(err error) (RequestError, bool) {
	var target *RequestError
	if !errors.As(err, &target) {
		return RequestError{}, false
	}
	return *target, true
}
```

```go
func reasonFromTelegramStatus(statusCode int, data []byte) string {
	text := strings.ToLower(string(data))
	switch {
	case statusCode == http.StatusUnauthorized:
		return "chat_access_denied"
	case strings.Contains(text, "bot was kicked"):
		return "chat_access_denied"
	case strings.Contains(text, "forbidden") && strings.Contains(text, "chat"):
		return "chat_access_denied"
	case strings.Contains(text, "file") && strings.Contains(text, "not found"):
		return "file_not_found"
	case statusCode == http.StatusForbidden:
		return "forbidden"
	default:
		return "telegram_error"
	}
}
```

```go
func statusErrorForOperation(op OperationClass, statusCode int, data []byte) error {
	base := statusError(statusCode, data)
	return NewRequestError(op, statusCode, reasonFromTelegramStatus(statusCode, data), base)
}
```

```go
return nil, retry, delay, statusErrorForOperation(OperationUploadSend, resp.StatusCode, data)
```

```go
if !retry {
	return nil, statusErrorForOperation(OperationDownloadRead, resp.StatusCode, data)
}
lastErr = statusErrorForOperation(OperationDownloadRead, resp.StatusCode, data)
```

- [ ] **Step 4: Add bucket-unavailable handling in store**

```go
var ErrUnavailable = errors.New("bucket unavailable")
```

```go
func (s *ObjectStore) isBucketUnavailable(name string) bool {
	s.unavailableMu.RLock()
	defer s.unavailableMu.RUnlock()
	return s.unavailableBuckets[name]
}

func (s *ObjectStore) markBucketUnavailable(name string) bool {
	s.unavailableMu.Lock()
	defer s.unavailableMu.Unlock()
	if s.unavailableBuckets[name] {
		return false
	}
	s.unavailableBuckets[name] = true
	return true
}

func shouldMarkBucketUnavailable(err error) bool {
	classified, ok := telegram.ClassifyRequestError(err)
	if !ok {
		return false
	}
	if classified.Op != telegram.OperationUploadSend {
		return false
	}
	return classified.StatusCode == http.StatusUnauthorized || classified.Reason == "chat_access_denied"
}
```

```go
if s.isBucketUnavailable(input.Bucket) {
	return PutObjectResult{}, ErrUnavailable
}
```

```go
if err != nil {
	if shouldMarkBucketUnavailable(err) {
		s.markBucketUnavailable(input.Bucket)
	}
	return PutObjectResult{}, err
}
```

```go
if s.isBucketUnavailable(input.Bucket) {
	return nil, ObjectInfo{}, ErrUnavailable
}
```

- [ ] **Step 5: Run targeted error-classification tests and full regression**

Run: `go test ./telegram ./store -run 'TestTelegramClassifyRequestErrorIncludesOperationStatusAndReason|TestStoreOldObjectReadFailureDoesNotMarkBucketUnavailable|TestStoreUploadAuthFailureMarksOnlyThatBucketUnavailable|TestStoreUploadGateRecordsCooldownFromFinalRateLimit|TestStorePutObjectSingleUploadRetriesThroughLocalStaging' -count=1`
Expected: PASS

Run: `go test ./... -count=1`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add telegram/client.go telegram/client_test.go store/store.go store/store_test.go store/types.go
git commit -m "feat(store): classify bucket token failures"
```

## Spec Coverage Check

- Config precedence, `${ENV}` parsing, and backward compatibility are covered in Task 1.
- Runtime binding construction and startup failure for empty final token are covered in Task 2.
- Per-token gate prebuild and token-shared cooldown are covered in Task 3.
- Bucket-unavailable semantics, normalized Telegram error payload, and object-level read failure behavior are covered in Task 4.
- Logging redaction is verified in Task 4 tests and should be implemented alongside existing `sanitizeLogError` / Telegram sanitization paths.

## Verification

- Run `go test ./config -count=1`
- Run `go test ./cmd/tgnas ./store ./telegram -count=1`
- Run `go test ./... -count=1`
- Manual check with one global-token bucket and one bucket-token override bucket:
  - verify uploads route to different Telegram bots
  - verify same-token buckets share cooldown
  - verify different-token buckets do not wait on each other
  - verify old objects unreadable after token change only fail that request
  - verify bucket-level auth failure makes only that bucket fail fast afterward
