# Go Telegram S3 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a Go library and S3-compatible service that stores object bytes in Telegram via Bot API, stores metadata in configurable SQLite, and exposes the approved v1 S3 Object API surface.

**Architecture:** Implement a reusable `store` package that owns bucket/object semantics, metadata coordination, Telegram upload/download, chunking, hashing, Range reads, and object-level locks. Keep `internal/s3api` as the protocol boundary for path-style S3 routing, SigV4 header authentication, S3 XML responses, request parsing, response headers, and integration with common S3 clients. Use YAML configuration, SQLite behind a metadata interface, Telegram `sendDocument` by default, optional typed uploads in `auto` mode, and explicit container entrypoints.

**Tech Stack:** Go 1.22+, `net/http`, `database/sql`, `crypto/hmac`, `crypto/sha256`, `crypto/md5`, `encoding/xml`, `gopkg.in/yaml.v3`, `modernc.org/sqlite`, AWS SDK for Go v2 integration tests, Docker.

---

## Revision Notes

This plan replaces the earlier staged plan. The previous plan postponed full S3 object routes, AWS SDK integration tests, and HTTP Range streaming. Those are v1 requirements, so this revised plan includes them before the final build gate.

## File Structure

Create these files unless they already exist from an earlier task.

- `go.mod` — module definition and dependencies.
- `config/config.go` — YAML config structs, defaults, env resolution, validation.
- `config/config_test.go` — config loading, env precedence, validation tests.
- `metadata/types.go` — metadata records and `Store` interface.
- `metadata/sqlite.go` — SQLite migrations and CRUD implementation.
- `metadata/sqlite_test.go` — bucket/object/chunk/list/delete transaction tests.
- `telegram/types.go` — Telegram constants, request/response types, client interface.
- `telegram/caption.go` — caption template validation and rendering.
- `telegram/client.go` — streaming Telegram Bot API HTTP client.
- `telegram/caption_test.go` — caption variable tests.
- `telegram/client_test.go` — upload method mapping, caption, download tests.
- `store/types.go` — public object-store input/output types and errors.
- `store/upload_strategy.go` — `document` and `auto` strategy resolver.
- `store/range.go` — Range parsing and chunk selection.
- `store/locks.go` — keyed bucket/object mutex.
- `store/store.go` — core HeadBucket/Put/Get/Head/Delete/List/ListBuckets orchestration for startup-configured buckets.
- `store/upload_strategy_test.go` — typed/document/fallback/chunk strategy tests.
- `store/range_test.go` — Range parser and chunk overlap tests.
- `store/locks_test.go` — same-key serialization test.
- `store/store_test.go` — fake Telegram + real SQLite store behavior tests, including Range reads.
- `internal/testutil/faketelegram.go` — fake Telegram implementation for store and S3 tests.
- `internal/testutil/reader.go` — bounded readers and error readers for tests.
- `internal/s3api/errors.go` — S3 XML error writer and error mapping.
- `internal/s3api/xml.go` — S3 XML success response structs.
- `internal/s3api/list_token.go` — ListObjectsV2 opaque continuation token.
- `internal/s3api/sigv4.go` — SigV4 header verifier.
- `internal/s3api/server.go` — complete v1 S3 HTTP routes and health checks.
- `internal/s3api/errors_test.go` — S3 XML error shape tests.
- `internal/s3api/list_token_test.go` — continuation token tests.
- `internal/s3api/sigv4_test.go` — SigV4 known-vector and rejection tests.
- `internal/s3api/server_test.go` — route, XML, header, Range, and auth tests.
- `internal/s3api/integration_test.go` — AWS SDK path-style Put/Get/Head/Delete/ListObjectsV2 tests.
- `cmd/tgs3/main.go` — service entrypoint.
- `Dockerfile` — container image.
- `.dockerignore` — container build exclusions.
- `README.md` — minimal quick start and configuration reference.

## Implementation Tasks

### Task 1: Initialize Go Module and Config Loader

**Files:**
- Create: `go.mod`
- Create: `config/config.go`
- Create: `config/config_test.go`

- [ ] **Step 1: Write failing config tests**

Create `config/config_test.go` with these tests:

```go
package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfigDefaultsAndYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	err := os.WriteFile(path, []byte(`
server:
  listen: ":9100"
auth:
  region: "us-east-1"
  credentials:
    - access_key: "admin"
      secret_key_env: "TGS3_TEST_SECRET"
telegram:
  bot_token_env: "TGS3_TEST_BOT_TOKEN"
  api_base_url: "https://api.telegram.org"
  timeout: "45s"
metadata:
  driver: "sqlite"
  sqlite_path: "/tmp/tgs3.sqlite"
storage:
  upload_type_strategy: "document"
buckets:
  photos:
    chat_id: "-100123"
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TGS3_TEST_SECRET", "secret")
	t.Setenv("TGS3_TEST_BOT_TOKEN", "bot-token")

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile returned error: %v", err)
	}
	if cfg.Server.Listen != ":9100" {
		t.Fatalf("listen = %q", cfg.Server.Listen)
	}
	if cfg.Telegram.Timeout != 45*time.Second {
		t.Fatalf("timeout = %s", cfg.Telegram.Timeout)
	}
	if cfg.Storage.UploadTypeStrategy != "document" {
		t.Fatalf("strategy = %q", cfg.Storage.UploadTypeStrategy)
	}
	if cfg.Storage.ChunkSize != 20*1024*1024 {
		t.Fatalf("chunk size = %d", cfg.Storage.ChunkSize)
	}
	if cfg.Storage.EnableChunking == nil || !*cfg.Storage.EnableChunking {
		t.Fatalf("enable_chunking = %v, want true", cfg.Storage.EnableChunking)
	}
	if got := cfg.ResolveSecret(cfg.Auth.Credentials[0].SecretKeyEnv); got != "secret" {
		t.Fatalf("secret = %q", got)
	}
	if got := cfg.ResolveBotToken(); got != "bot-token" {
		t.Fatalf("bot token = %q", got)
	}
}

func TestLoadConfigAllowsDisablingChunking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	err := os.WriteFile(path, []byte(`
server:
  listen: ":9000"
auth:
  region: "us-east-1"
  credentials:
    - access_key: "admin"
      secret_key_env: "SECRET"
telegram:
  bot_token_env: "BOT_TOKEN"
  api_base_url: "https://api.telegram.org"
metadata:
  driver: "sqlite"
  sqlite_path: "/tmp/tgs3.sqlite"
storage:
  upload_type_strategy: "document"
  enable_chunking: false
buckets:
  photos:
    chat_id: "-100123"
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile returned error: %v", err)
	}
	if cfg.Storage.EnableChunking == nil || *cfg.Storage.EnableChunking {
		t.Fatalf("enable_chunking = %v, want false", cfg.Storage.EnableChunking)
	}
}

func TestSQLitePathEnvPrecedence(t *testing.T) {
	t.Setenv("TGS3_SQLITE_PATH", "/env/tgs3.sqlite")
	cfg := Config{Metadata: MetadataConfig{Driver: "sqlite", SQLitePath: "/file/tgs3.sqlite", SQLitePathEnv: "TGS3_SQLITE_PATH"}}
	got, err := cfg.ResolveSQLitePath()
	if err != nil {
		t.Fatalf("ResolveSQLitePath returned error: %v", err)
	}
	if got != "/env/tgs3.sqlite" {
		t.Fatalf("path = %q", got)
	}
}

func TestValidateRejectsUnknownUploadStrategy(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Storage.UploadTypeStrategy = "typed"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidateRejectsMissingBucketChatID(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Buckets["photos"] = BucketConfig{}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func minimalValidConfig() Config {
	return Config{
		Server:   ServerConfig{Listen: ":9000"},
		Auth:     AuthConfig{Region: "us-east-1", Credentials: []CredentialConfig{{AccessKey: "admin", SecretKeyEnv: "SECRET"}}},
		Telegram: TelegramConfig{BotTokenEnv: "BOT_TOKEN", APIBaseURL: "https://api.telegram.org", Timeout: 30 * time.Second},
		Metadata: MetadataConfig{Driver: "sqlite", SQLitePath: "/tmp/tgs3.sqlite"},
		Storage:  DefaultStorageConfig(),
		Buckets:  map[string]BucketConfig{"photos": {ChatID: "-100123"}},
	}
}
```

- [ ] **Step 2: Run config tests and verify red state**

Run:

```bash
go test ./config
```

Expected: failure because module/config files are not implemented yet.

- [ ] **Step 3: Create `go.mod`**

Create `go.mod`:

```go
module github.com/aahl/tgs3

go 1.22

require (
	github.com/aws/aws-sdk-go-v2 v1.32.2
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.6.6
	github.com/aws/aws-sdk-go-v2/config v1.28.2
	github.com/aws/aws-sdk-go-v2/credentials v1.17.45
	github.com/aws/aws-sdk-go-v2/service/s3 v1.65.3
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.33.1
)
```

- [ ] **Step 4: Implement config loader**

Create `config/config.go`:

```go
package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var raw string
	if err := value.Decode(&raw); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", raw, err)
	}
	d.Duration = parsed
	return nil
}

type Config struct {
	Server   ServerConfig            `yaml:"server"`
	Auth     AuthConfig              `yaml:"auth"`
	Telegram TelegramConfig          `yaml:"telegram"`
	Metadata MetadataConfig          `yaml:"metadata"`
	Storage  StorageConfig           `yaml:"storage"`
	Buckets  map[string]BucketConfig `yaml:"buckets"`
}

type ServerConfig struct {
	Listen        string `yaml:"listen"`
	ListenEnv     string `yaml:"listen_env"`
	PublicBaseURL string `yaml:"public_base_url"`
}

type AuthConfig struct {
	Region      string             `yaml:"region"`
	Credentials []CredentialConfig `yaml:"credentials"`
}

type CredentialConfig struct {
	AccessKey    string `yaml:"access_key"`
	SecretKeyEnv string `yaml:"secret_key_env"`
}

type TelegramConfig struct {
	BotTokenEnv     string        `yaml:"bot_token_env"`
	APIBaseURL      string        `yaml:"api_base_url"`
	Timeout         time.Duration `yaml:"-"`
	RawTimeout      Duration      `yaml:"timeout"`
	CaptionTemplate string        `yaml:"caption_template"`
}

type MetadataConfig struct {
	Driver        string `yaml:"driver"`
	SQLitePath    string `yaml:"sqlite_path"`
	SQLitePathEnv string `yaml:"sqlite_path_env"`
}

type StorageConfig struct {
	UploadTypeStrategy            string           `yaml:"upload_type_strategy"`
	EnableChunking                *bool            `yaml:"enable_chunking"`
	MaxFileSize                   int64            `yaml:"max_file_size"`
	ChunkSize                     int64            `yaml:"chunk_size"`
	TypeSizeLimits                map[string]int64 `yaml:"type_size_limits"`
	MaxConcurrentUploads          int              `yaml:"max_concurrent_uploads"`
	MaxConcurrentDownloads        int              `yaml:"max_concurrent_downloads"`
	MaxConcurrentTelegramRequests int              `yaml:"max_concurrent_telegram_requests"`
	PutBufferSize                 int              `yaml:"put_buffer_size"`
}

type BucketConfig struct {
	ChatID string `yaml:"chat_id"`
}

func LoadFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Server:   ServerConfig{Listen: ":9000"},
		Telegram: TelegramConfig{APIBaseURL: "https://api.telegram.org", Timeout: 30 * time.Second},
		Metadata: MetadataConfig{Driver: "sqlite"},
		Storage:  DefaultStorageConfig(),
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.Telegram.RawTimeout.Duration != 0 {
		cfg.Telegram.Timeout = cfg.Telegram.RawTimeout.Duration
	}
	applyStorageDefaults(&cfg.Storage)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func DefaultStorageConfig() StorageConfig {
	return StorageConfig{
		UploadTypeStrategy: "document",
		EnableChunking:     boolPtr(true),
		MaxFileSize:        50 * 1024 * 1024,
		ChunkSize:          20 * 1024 * 1024,
		TypeSizeLimits: map[string]int64{
			"photo":     10 * 1024 * 1024,
			"video":     20 * 1024 * 1024,
			"audio":     20 * 1024 * 1024,
			"animation": 20 * 1024 * 1024,
			"document":  20 * 1024 * 1024,
		},
		MaxConcurrentUploads:          4,
		MaxConcurrentDownloads:        16,
		MaxConcurrentTelegramRequests: 8,
		PutBufferSize:                 1024 * 1024,
	}
}

func boolPtr(value bool) *bool { return &value }

func applyStorageDefaults(s *StorageConfig) {
	defaults := DefaultStorageConfig()
	if s.UploadTypeStrategy == "" {
		s.UploadTypeStrategy = defaults.UploadTypeStrategy
	}
	if s.EnableChunking == nil {
		s.EnableChunking = defaults.EnableChunking
	}
	if s.MaxFileSize == 0 {
		s.MaxFileSize = defaults.MaxFileSize
	}
	if s.ChunkSize == 0 {
		s.ChunkSize = defaults.ChunkSize
	}
	if s.TypeSizeLimits == nil {
		s.TypeSizeLimits = map[string]int64{}
	}
	for k, v := range defaults.TypeSizeLimits {
		if s.TypeSizeLimits[k] == 0 {
			s.TypeSizeLimits[k] = v
		}
	}
	if s.MaxConcurrentUploads == 0 {
		s.MaxConcurrentUploads = defaults.MaxConcurrentUploads
	}
	if s.MaxConcurrentDownloads == 0 {
		s.MaxConcurrentDownloads = defaults.MaxConcurrentDownloads
	}
	if s.MaxConcurrentTelegramRequests == 0 {
		s.MaxConcurrentTelegramRequests = defaults.MaxConcurrentTelegramRequests
	}
	if s.PutBufferSize == 0 {
		s.PutBufferSize = defaults.PutBufferSize
	}
}

func (c Config) ResolveListen() string {
	if c.Server.ListenEnv != "" {
		if value := os.Getenv(c.Server.ListenEnv); value != "" {
			return value
		}
	}
	if c.Server.Listen == "" {
		return ":9000"
	}
	return c.Server.Listen
}

func (c Config) ResolveSecret(env string) string {
	if env == "" {
		return ""
	}
	return os.Getenv(env)
}

func (c Config) ResolveBotToken() string {
	return c.ResolveSecret(c.Telegram.BotTokenEnv)
}

func (c Config) ResolveSQLitePath() (string, error) {
	if c.Metadata.SQLitePathEnv != "" {
		if value := os.Getenv(c.Metadata.SQLitePathEnv); value != "" {
			return value, nil
		}
	}
	if c.Metadata.SQLitePath != "" {
		return c.Metadata.SQLitePath, nil
	}
	return "", errors.New("metadata sqlite path is required")
}

func (c Config) Validate() error {
	if c.Server.Listen == "" && c.Server.ListenEnv == "" {
		return errors.New("server.listen or server.listen_env is required")
	}
	if c.Auth.Region == "" {
		return errors.New("auth.region is required")
	}
	if len(c.Auth.Credentials) == 0 {
		return errors.New("auth.credentials is required")
	}
	for _, credential := range c.Auth.Credentials {
		if credential.AccessKey == "" || credential.SecretKeyEnv == "" {
			return errors.New("auth credentials require access_key and secret_key_env")
		}
	}
	if c.Telegram.BotTokenEnv == "" {
		return errors.New("telegram.bot_token_env is required")
	}
	if c.Telegram.APIBaseURL == "" {
		return errors.New("telegram.api_base_url is required")
	}
	if c.Metadata.Driver != "sqlite" {
		return fmt.Errorf("unsupported metadata driver %q", c.Metadata.Driver)
	}
	if _, err := c.ResolveSQLitePath(); err != nil {
		return err
	}
	if c.Storage.UploadTypeStrategy != "document" && c.Storage.UploadTypeStrategy != "auto" {
		return fmt.Errorf("unsupported upload_type_strategy %q", c.Storage.UploadTypeStrategy)
	}
	if c.Storage.ChunkSize <= 0 || c.Storage.MaxFileSize <= 0 {
		return errors.New("storage max_file_size and chunk_size must be positive")
	}
	if c.Storage.TypeSizeLimits["document"] <= 0 {
		return errors.New("storage.type_size_limits.document must be positive")
	}
	if len(c.Buckets) == 0 {
		return errors.New("at least one bucket is required")
	}
	for name, bucket := range c.Buckets {
		if name == "" || bucket.ChatID == "" {
			return errors.New("bucket names and chat_id values are required")
		}
	}
	return nil
}
```

- [ ] **Step 5: Run config tests and verify green state**

Run:

```bash
go test ./config
```

Expected: PASS.

- [ ] **Step 6: Checkpoint**

If this directory is a git repository, commit:

```bash
git add go.mod config/config.go config/config_test.go
git commit -m "feat: add YAML configuration loader"
```

If this directory is not a git repository, write the changed file list in the session response for this checkpoint and continue.

### Task 2: Implement Metadata Interface and SQLite Store

**Files:**
- Create: `metadata/types.go`
- Create: `metadata/sqlite.go`
- Create: `metadata/sqlite_test.go`

- [ ] **Step 1: Write failing metadata tests**

Create `metadata/sqlite_test.go` with tests for: bucket upsert/get/list; object replacement with chunk replacement in one transaction; ordered list by key with `Prefix`, `AfterKey`, and `Limit`; delete metadata only; missing object returns `ErrNotFound`; disabled buckets are still persisted but not returned by `ListBuckets`.

Use this concrete first test as the minimum red test:

```go
package metadata

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteBucketsObjectsAndChunks(t *testing.T) {
	ctx := context.Background()
	store, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer store.Close()

	bucket := Bucket{Name: "photos", ChatID: "-100123", CreatedAt: time.Unix(10, 0), Enabled: true}
	if err := store.UpsertBucket(ctx, bucket); err != nil {
		t.Fatalf("UpsertBucket returned error: %v", err)
	}
	gotBucket, err := store.GetBucket(ctx, "photos")
	if err != nil {
		t.Fatalf("GetBucket returned error: %v", err)
	}
	if gotBucket.ChatID != "-100123" || !gotBucket.Enabled {
		t.Fatalf("bucket = %+v", gotBucket)
	}

	object := Object{
		Bucket: "photos", Key: "b/cat.jpg", Size: 11, ContentType: "image/jpeg",
		ETag: "5eb63bbbe01eeed093cb22bb8f5acdc3", SHA256: "sha", LastModified: time.Unix(20, 0),
		ChunkCount: 2, TelegramType: "document", UploadStrategy: "chunked_document",
	}
	chunks := []Chunk{
		{Bucket: "photos", Key: "b/cat.jpg", PartNumber: 1, Offset: 0, Size: 5, TelegramType: "document", TelegramFileID: "file-1", TelegramMessageID: 101, TelegramFileUniqueID: "u1", SHA256: "c1"},
		{Bucket: "photos", Key: "b/cat.jpg", PartNumber: 2, Offset: 5, Size: 6, TelegramType: "document", TelegramFileID: "file-2", TelegramMessageID: 102, TelegramFileUniqueID: "u2", SHA256: "c2"},
	}
	if err := store.PutObject(ctx, object, chunks); err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}
	gotObject, gotChunks, err := store.GetObject(ctx, "photos", "b/cat.jpg")
	if err != nil {
		t.Fatalf("GetObject returned error: %v", err)
	}
	if gotObject.Key != object.Key || len(gotChunks) != 2 || gotChunks[1].Offset != 5 {
		t.Fatalf("object = %+v chunks = %+v", gotObject, gotChunks)
	}
}
```

Add companion tests named `TestSQLiteListObjectsOrderedAndPaginated`, `TestSQLitePutObjectReplacesChunks`, `TestSQLiteDeleteObjectRemovesMetadata`, and `TestSQLiteListBucketsOnlyEnabled` before implementation.

- [ ] **Step 2: Run metadata tests and verify red state**

Run:

```bash
go test ./metadata
```

Expected: failure because metadata package is not implemented.

- [ ] **Step 3: Implement metadata records and interface**

Create `metadata/types.go`:

```go
package metadata

import (
	"context"
	"errors"
	"time"
)

var ErrNotFound = errors.New("metadata not found")

type Bucket struct {
	Name      string
	ChatID    string
	CreatedAt time.Time
	Enabled   bool
}

type Object struct {
	Bucket         string
	Key            string
	Size           int64
	ContentType    string
	ETag           string
	SHA256         string
	LastModified   time.Time
	ChunkCount     int
	TelegramType   string
	UploadStrategy string
}

type Chunk struct {
	Bucket               string
	Key                  string
	PartNumber           int
	Offset               int64
	Size                 int64
	TelegramType         string
	TelegramFileID       string
	TelegramMessageID    int64
	TelegramFileUniqueID string
	SHA256               string
}

type ListQuery struct {
	Bucket   string
	Prefix   string
	AfterKey string
	Limit    int
}

type Store interface {
	Close() error
	UpsertBucket(ctx context.Context, bucket Bucket) error
	GetBucket(ctx context.Context, name string) (Bucket, error)
	ListBuckets(ctx context.Context) ([]Bucket, error)
	PutObject(ctx context.Context, object Object, chunks []Chunk) error
	GetObject(ctx context.Context, bucket, key string) (Object, []Chunk, error)
	HeadObject(ctx context.Context, bucket, key string) (Object, error)
	ListObjects(ctx context.Context, query ListQuery) ([]Object, error)
	DeleteObject(ctx context.Context, bucket, key string) error
}
```

- [ ] **Step 4: Implement SQLite migrations and methods**

Create `metadata/sqlite.go` implementing the `Store` interface with these requirements:

```go
package metadata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

type SQLiteStore struct{ db *sql.DB }

func OpenSQLite(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	store := &SQLiteStore{db: db}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *SQLiteStore) Close() error { return s.db.Close() }
```

Implement `migrate` with tables exactly matching the design: `buckets`, `objects`, `object_chunks`. Use integer Unix seconds for timestamps, `INTEGER` booleans, `PRIMARY KEY(bucket, key)` for objects, and `PRIMARY KEY(bucket, key, part_number)` for chunks. Implement `PutObject` with a transaction that deletes old chunks, upserts the object row, inserts new chunks, and commits only after all inserts succeed. Implement `ListObjects` as `WHERE bucket = ? AND key LIKE ? AND key > ? ORDER BY key ASC LIMIT ?` with default limit `1000` when the caller passes `Limit <= 0`.

- [ ] **Step 5: Run metadata tests and verify green state**

Run:

```bash
go test ./metadata
```

Expected: PASS.

- [ ] **Step 6: Checkpoint**

If this directory is a git repository, commit:

```bash
git add metadata/types.go metadata/sqlite.go metadata/sqlite_test.go go.mod go.sum
git commit -m "feat: add SQLite metadata store"
```

If this directory is not a git repository, write the changed file list in the session response for this checkpoint and continue.

### Task 3: Implement Telegram Types, Captions, and Streaming HTTP Client

**Files:**
- Create: `telegram/types.go`
- Create: `telegram/caption.go`
- Create: `telegram/client.go`
- Create: `telegram/caption_test.go`
- Create: `telegram/client_test.go`

- [ ] **Step 1: Write failing caption tests**

Create `telegram/caption_test.go` with tests for `{bucket}`, `{key}`, `{name}`, `{size}`, `{bytes}`, `{part}`, `{parts}`, and `{chunk}`. Include this required non-chunk behavior test:

```go
func TestCaptionTemplateNonChunkedChunkIsEmpty(t *testing.T) {
	tpl, err := ParseCaptionTemplate("part={part} parts={parts} chunk={chunk}")
	if err != nil {
		t.Fatalf("ParseCaptionTemplate returned error: %v", err)
	}
	caption := tpl.Render(CaptionData{Part: 1, Parts: 1})
	if caption != "part=1 parts=1 chunk=" {
		t.Fatalf("caption = %q", caption)
	}
}
```

Also add `TestCaptionTemplateRejectsUnknownVariable`, `TestCaptionTemplateTruncatesToLimit`, and this empty-template test so the default config sends no caption:

```go
func TestCaptionTemplateEmptyTemplateRendersEmptyCaption(t *testing.T) {
	tpl, err := ParseCaptionTemplate("")
	if err != nil {
		t.Fatalf("ParseCaptionTemplate returned error: %v", err)
	}
	caption := tpl.Render(CaptionData{Bucket: "photos", Key: "hello.txt", Name: "hello.txt", Bytes: 5, Part: 1, Parts: 1})
	if caption != "" {
		t.Fatalf("caption = %q, want empty", caption)
	}
}
```

- [ ] **Step 2: Write failing Telegram client tests**

Create `telegram/client_test.go` with these tests:

```go
func TestClientUploadDocumentSendsCaptionAndParsesFile(t *testing.T) {
	var path string
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		data, _ := io.ReadAll(r.Body)
		body = string(data)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":77,"document":{"file_id":"file-1","file_unique_id":"unique-1","file_size":5,"mime_type":"text/plain","file_name":"hello.txt"}}}`))
	}))
	defer server.Close()

	client := NewHTTPClient("token", server.URL, http.DefaultClient)
	uploaded, err := client.Upload(context.Background(), UploadRequest{Type: TypeDocument, ChatID: "-100", Reader: strings.NewReader("hello"), Filename: "hello.txt", MIMEType: "text/plain", Caption: "caption text"})
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}
	if path != "/bottoken/sendDocument" {
		t.Fatalf("path = %q", path)
	}
	if !strings.Contains(body, `name="caption"`) || !strings.Contains(body, "caption text") || !strings.Contains(body, `name="document"`) {
		t.Fatalf("multipart body missing fields: %s", body)
	}
	if uploaded.FileID != "file-1" || uploaded.MessageID != 77 || uploaded.FileSize != 5 {
		t.Fatalf("uploaded = %+v", uploaded)
	}
}

func TestClientUploadPhotoUsesPhotoEndpointAndLargestPhoto(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":78,"photo":[{"file_id":"small","file_unique_id":"u-small","file_size":1},{"file_id":"large","file_unique_id":"u-large","file_size":5}]}}`))
	}))
	defer server.Close()

	client := NewHTTPClient("token", server.URL, http.DefaultClient)
	uploaded, err := client.Upload(context.Background(), UploadRequest{Type: TypePhoto, ChatID: "-100", Reader: strings.NewReader("hello"), Filename: "hello.jpg", MIMEType: "image/jpeg"})
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}
	if path != "/bottoken/sendPhoto" {
		t.Fatalf("path = %q", path)
	}
	if uploaded.FileID != "large" || uploaded.FileUniqueID != "u-large" {
		t.Fatalf("uploaded = %+v", uploaded)
	}
}

func TestClientDownloadStreamUsesGetFilePath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bottoken/getFile":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"file-1","file_path":"documents/file_1.txt","file_size":5}}`))
		case "/file/bottoken/documents/file_1.txt":
			_, _ = w.Write([]byte("hello"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewHTTPClient("token", server.URL, http.DefaultClient)
	stream, err := client.Download(context.Background(), "file-1")
	if err != nil {
		t.Fatalf("Download returned error: %v", err)
	}
	defer stream.Close()
	data, _ := io.ReadAll(stream)
	if string(data) != "hello" {
		t.Fatalf("data = %q", string(data))
	}
}

func TestClientUploadStreamsMultipartBody(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		_, _ = io.Copy(io.Discard, r.Body)
		<-finish
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":77,"document":{"file_id":"file-1","file_unique_id":"unique-1","file_size":5}}}`))
	}))
	defer server.Close()

	reader := io.MultiReader(strings.NewReader("hello"), blockingReader{done: finish})
	client := NewHTTPClient("token", server.URL, http.DefaultClient)
	errCh := make(chan error, 1)
	go func() {
		_, err := client.Upload(context.Background(), UploadRequest{Type: TypeDocument, ChatID: "-100", Reader: reader, Filename: "hello.txt"})
		errCh <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("server did not receive request before upload reader completed")
	}
	close(finish)
	if err := <-errCh; err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}
}

type blockingReader struct{ done <-chan struct{} }

func (r blockingReader) Read(p []byte) (int, error) {
	<-r.done
	return 0, io.EOF
}
```

The implementation should use `io.Pipe` with multipart writer so large objects are not copied into a full request buffer before the HTTP request starts.

- [ ] **Step 3: Run Telegram tests and verify red state**

Run:

```bash
go test ./telegram
```

Expected: failure because Telegram package is incomplete.

- [ ] **Step 4: Implement Telegram public types**

Create `telegram/types.go`:

```go
package telegram

import (
	"context"
	"io"
)

const (
	TypePhoto     = "photo"
	TypeVideo     = "video"
	TypeAudio     = "audio"
	TypeAnimation = "animation"
	TypeDocument  = "document"
)

type UploadRequest struct {
	Type     string
	ChatID   string
	Reader   io.Reader
	Filename string
	MIMEType string
	Caption  string
}

type UploadedFile struct {
	Type         string
	FileID       string
	FileUniqueID string
	MessageID    int64
	FileSize     int64
	MIMEType     string
}

type Client interface {
	Upload(ctx context.Context, request UploadRequest) (UploadedFile, error)
	Download(ctx context.Context, fileID string) (io.ReadCloser, error)
}
```

- [ ] **Step 5: Implement caption template**

Create `telegram/caption.go` with concrete type `CaptionTemplate`, `ParseCaptionTemplate(raw string) (*CaptionTemplate, error)`, `func (t *CaptionTemplate) Render(data CaptionData) string`, `func (t *CaptionTemplate) RenderWithLimit(data CaptionData, limit int) string`, `CaptionData`, `CaptionLimit = 1024`, known-variable validation, rune-safe truncation, and chunk marker semantics:

```go
package telegram

import "strconv"

type CaptionTemplate struct {
	raw string
}

type CaptionData struct {
	Bucket string
	Key    string
	Name   string
	Size   string
	Bytes  int64
	Part   int
	Parts  int
}

func ParseCaptionTemplate(raw string) (*CaptionTemplate, error) {
	return &CaptionTemplate{raw: raw}, nil
}

func normalizePart(value int) int {
	if value < 1 {
		return 1
	}
	return value
}

func chunkMarker(part, parts int) string {
	part = normalizePart(part)
	parts = normalizePart(parts)
	if part == 1 && parts == 1 {
		return ""
	}
	return strconv.Itoa(part) + "/" + strconv.Itoa(parts)
}
```

- [ ] **Step 6: Implement streaming Telegram HTTP client**

Create `telegram/client.go` implementing:

- Concrete type `HTTPClient` with constructor `NewHTTPClient(botToken, apiBaseURL string, httpClient *http.Client) *HTTPClient`; this type must satisfy the `telegram.Client` interface.
- Method mapping: `photo -> sendPhoto/photo`, `video -> sendVideo/video`, `audio -> sendAudio/audio`, `animation -> sendAnimation/animation`, `document -> sendDocument/document`.
- Multipart upload using `io.Pipe` and `multipart.NewWriter` so request body streams from `UploadRequest.Reader`.
- Optional `caption` field only when non-empty.
- `getFile` POST with `file_id`, then file download from `/file/bot{token}/{file_path}`.
- JSON parsing for `document`, `video`, `audio`, `animation`, and largest `photo` size.
- Non-2xx and `ok:false` responses return errors without logging bot tokens.
- Retry Telegram uploads, `getFile`, and downloads for HTTP 429 and 5xx with bounded exponential backoff inside `HTTPClient`; if Telegram returns `retry_after`, use that delay up to the configured/request timeout instead of the generic backoff. Telegram-call concurrency is not owned by `HTTPClient` in v1; the store layer serializes calls with `Options.MaxTelegramCalls`.

- [ ] **Step 7: Run Telegram tests and verify green state**

Run:

```bash
go test ./telegram
```

Expected: PASS.

- [ ] **Step 8: Checkpoint**

If git is available:

```bash
git add telegram/types.go telegram/caption.go telegram/client.go telegram/caption_test.go telegram/client_test.go
git commit -m "feat: add Telegram Bot API client"
```

If this directory is not a git repository, write the changed file list in the session response for this checkpoint and continue.

### Task 4: Implement Upload Strategy, Range Helpers, and Object Locks

**Files:**
- Create: `store/types.go`
- Create: `store/upload_strategy.go`
- Create: `store/range.go`
- Create: `store/locks.go`
- Create: `store/upload_strategy_test.go`
- Create: `store/range_test.go`
- Create: `store/locks_test.go`

- [ ] **Step 1: Write failing upload strategy tests**

Create tests covering:

```go
func TestDocumentStrategyAlwaysUsesDocumentWithinLimit(t *testing.T) {
	resolver := NewUploadStrategyResolver(DefaultUploadConfig())
	strategy, err := resolver.Resolve("photo.jpg", "image/jpeg", 1024)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if strategy.TelegramType != "document" || strategy.UploadStrategy != "document" || strategy.Chunked {
		t.Fatalf("strategy = %+v", strategy)
	}
}

func TestAutoStrategyInfersPhoto(t *testing.T) {
	config := DefaultUploadConfig()
	config.Strategy = "auto"
	resolver := NewUploadStrategyResolver(config)
	strategy, err := resolver.Resolve("photo.jpg", "image/jpeg", 1024)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if strategy.TelegramType != "photo" || strategy.UploadStrategy != "typed" || strategy.Chunked {
		t.Fatalf("strategy = %+v", strategy)
	}
}

func TestAutoStrategyFallsBackToDocument(t *testing.T) {
	config := DefaultUploadConfig()
	config.Strategy = "auto"
	resolver := NewUploadStrategyResolver(config)
	strategy, err := resolver.Resolve("photo.jpg", "image/jpeg", 15*1024*1024)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if strategy.TelegramType != "document" || strategy.UploadStrategy != "document" || strategy.Chunked {
		t.Fatalf("strategy = %+v", strategy)
	}
}

func TestResolverChunksLargeDocument(t *testing.T) {
	resolver := NewUploadStrategyResolver(DefaultUploadConfig())
	strategy, err := resolver.Resolve("archive.zip", "application/zip", 30*1024*1024)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if strategy.TelegramType != "document" || strategy.UploadStrategy != "chunked_document" || !strategy.Chunked || strategy.ChunkSize != 20*1024*1024 {
		t.Fatalf("strategy = %+v", strategy)
	}
}

func TestResolverRejectsLargeFileWhenChunkingDisabled(t *testing.T) {
	config := DefaultUploadConfig()
	config.EnableChunking = false
	resolver := NewUploadStrategyResolver(config)
	_, err := resolver.Resolve("archive.zip", "application/zip", 30*1024*1024)
	if err != ErrEntityTooLarge {
		t.Fatalf("err = %v, want ErrEntityTooLarge", err)
	}
}
```

- [ ] **Step 2: Write failing Range tests**

Create tests covering exact, open-ended, suffix, invalid, zero-size invalid, and chunk overlap selection:

```go
func TestSelectChunksForRange(t *testing.T) {
	chunks := []ChunkRef{{Part: 1, FileID: "f1", Offset: 0, Size: 5}, {Part: 2, FileID: "f2", Offset: 5, Size: 5}, {Part: 3, FileID: "f3", Offset: 10, Size: 5}}
	selected := SelectChunksForRange(chunks, ByteRange{Start: 3, End: 11})
	if len(selected) != 3 || selected[0].Skip != 3 || selected[2].Take != 2 {
		t.Fatalf("selected = %+v", selected)
	}
}
```

- [ ] **Step 3: Write failing lock tests**

Create `TestKeyedLockerSerializesSameKey` and `TestKeyedLockerAllowsDifferentKeys`.

- [ ] **Step 4: Run store helper tests and verify red state**

Run:

```bash
go test ./store
```

Expected: failure because store helper files are incomplete.

- [ ] **Step 5: Implement store errors and public types**

Create `store/types.go` with these errors and API records:

```go
package store

import (
	"errors"
	"io"
	"log"
	"time"

	"github.com/aahl/tgs3/telegram"
)

var (
	ErrNotImplemented       = errors.New("not implemented")
	ErrEntityTooLarge       = errors.New("entity too large")
	ErrNoSuchBucket         = errors.New("no such bucket")
	ErrNoSuchKey            = errors.New("no such key")
	ErrMissingContentLength = errors.New("missing content length")
	ErrInvalidRange         = errors.New("invalid range")
)

type PutObjectInput struct {
	Bucket      string
	Key         string
	ContentType string
	Size        int64
	Body        io.Reader
}

type PutObjectResult struct{ ETag string }

type ObjectInfo struct {
	Bucket       string
	Key          string
	Size         int64
	ContentType  string
	ETag         string
	SHA256       string
	LastModified time.Time
}

type GetObjectInput struct {
	Bucket string
	Key    string
	Range  *ByteRange
}

type ListObjectsInput struct {
	Bucket    string
	Prefix    string
	Delimiter string
	AfterKey  string
	Limit     int
}

type ListObjectsResult struct {
	Objects               []ObjectInfo
	CommonPrefixes        []string
	NextContinuationAfter string
	IsTruncated           bool
}

type UploadConfig struct {
	Strategy       string
	EnableChunking bool
	MaxFileSize    int64
	ChunkSize      int64
	TypeLimits     map[string]int64
	PutBufferSize  int
}

func DefaultUploadConfig() UploadConfig {
	return UploadConfig{
		Strategy:       "document",
		EnableChunking: true,
		MaxFileSize:    50 * 1024 * 1024,
		ChunkSize:      20 * 1024 * 1024,
		TypeLimits: map[string]int64{
			"photo":     10 * 1024 * 1024,
			"video":     20 * 1024 * 1024,
			"audio":     20 * 1024 * 1024,
			"animation": 20 * 1024 * 1024,
			"document":  20 * 1024 * 1024,
		},
		PutBufferSize: 1024 * 1024,
	}
}

type Options struct {
	Upload           UploadConfig
	Caption          *telegram.CaptionTemplate
	MaxUploads       int
	MaxDownloads     int
	MaxTelegramCalls int
	Logger           *log.Logger
}
```

`store/types.go` therefore imports `log` and `github.com/aahl/tgs3/telegram` in addition to `errors`, `io`, and `time`.

- [ ] **Step 6: Implement upload strategy**

Create `store/upload_strategy.go` with:

- Default `Strategy: "document"`.
- Optional `Strategy: "auto"`.
- MIME inference: `image/gif -> animation`, `image/* -> photo`, `video/* -> video`, `audio/* -> audio`, else `document`.
- Extension inference only when content type is empty.
- Fallback from typed media to document when typed limit is exceeded and document limit fits.
- Chunked document when document limit is exceeded and chunking is enabled.
- `ErrEntityTooLarge` when chunking is disabled.

- [ ] **Step 7: Implement Range helpers**

Create `store/range.go` with `ParseRange`, `ByteRange.Length`, `ChunkRef`, `SelectedChunk`, and `SelectChunksForRange`. `ParseRange` must support `bytes=N-M`, `bytes=N-`, and `bytes=-N`; reject multi-range headers for v1; clamp end to `size-1`; reject empty objects with a Range header.

- [ ] **Step 8: Implement keyed locker**

Create `store/locks.go` with a keyed mutex by `bucket + "\x00" + key`. Release functions must be idempotent-safe in tests by using them once; no lock cleanup is required in v1.

- [ ] **Step 9: Run store helper tests and verify green state**

Run:

```bash
go test ./store
```

Expected: PASS.

- [ ] **Step 10: Checkpoint**

If git is available:

```bash
git add store/types.go store/upload_strategy.go store/range.go store/locks.go store/upload_strategy_test.go store/range_test.go store/locks_test.go
git commit -m "feat: add store strategy and range helpers"
```

If this directory is not a git repository, write the changed file list in the session response for this checkpoint and continue.

### Task 5: Implement Core Object Store with Chunked and Range Reads

**Files:**
- Create: `internal/testutil/faketelegram.go`
- Create: `internal/testutil/reader.go`
- Create: `store/store.go`
- Create: `store/store_test.go`

- [ ] **Step 1: Write fake Telegram test helper**

Create `internal/testutil/reader.go`:

```go
package testutil

import (
	"errors"
	"io"
	"strings"
)

type ErrorReader struct{ Err error }

func (r ErrorReader) Read(p []byte) (int, error) {
	if r.Err != nil {
		return 0, r.Err
	}
	return 0, errors.New("test read error")
}

func LimitedStringReader(value string, limit int64) io.Reader {
	return io.LimitReader(strings.NewReader(value), limit)
}
```

Use `ErrorReader` in store tests that need upload/read failures; keep `internal/testutil/reader.go` because Task 5 owns shared test readers for store and S3 tests.

Create `internal/testutil/faketelegram.go`:

```go
package testutil

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/aahl/tgs3/telegram"
)

type FakeTelegram struct {
	mu        sync.Mutex
	Uploads   []telegram.UploadRequest
	Downloads []string
	Files     map[string]string
}

func NewFakeTelegram() *FakeTelegram {
	return &FakeTelegram{Files: map[string]string{}}
}

func (f *FakeTelegram) Upload(ctx context.Context, request telegram.UploadRequest) (telegram.UploadedFile, error) {
	data, err := io.ReadAll(request.Reader)
	if err != nil {
		return telegram.UploadedFile{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	request.Reader = strings.NewReader(string(data))
	f.Uploads = append(f.Uploads, request)
	fileID := fmt.Sprintf("file-%d", len(f.Uploads))
	f.Files[fileID] = string(data)
	return telegram.UploadedFile{Type: request.Type, FileID: fileID, FileUniqueID: fileID + "-unique", MessageID: int64(len(f.Uploads)), FileSize: int64(len(data)), MIMEType: request.MIMEType}, nil
}

func (f *FakeTelegram) Download(ctx context.Context, fileID string) (io.ReadCloser, error) {
	f.mu.Lock()
	f.Downloads = append(f.Downloads, fileID)
	data, ok := f.Files[fileID]
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("fake telegram file %q not found", fileID)
	}
	return io.NopCloser(strings.NewReader(data)), nil
}
```

- [ ] **Step 2: Write failing object store tests**

Create `store/store_test.go` covering:

```go
func TestStoreHeadBucketUsesStartupConfiguredMetadata(t *testing.T) {
	objectStore, _ := newReadyTestObjectStore(t, map[string]string{"photos": "-100"})
	if err := objectStore.HeadBucket(context.Background(), "photos"); err != nil {
		t.Fatalf("HeadBucket returned error: %v", err)
	}
	if err := objectStore.HeadBucket(context.Background(), "unknown"); err != ErrNoSuchBucket {
		t.Fatalf("err = %v, want ErrNoSuchBucket", err)
	}
}

func TestStorePutHeadDeleteAndList(t *testing.T) {
	ctx := context.Background()
	objectStore, fake := newReadyTestObjectStore(t, map[string]string{"photos": "-100"})
	result, err := objectStore.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "hello.txt", ContentType: "text/plain", Size: 5, Body: strings.NewReader("hello")})
	if err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}
	if result.ETag != "5d41402abc4b2a76b9719d911017c592" {
		t.Fatalf("etag = %q", result.ETag)
	}
	if len(fake.Uploads) != 1 || fake.Uploads[0].ChatID != "-100" || fake.Uploads[0].Type != telegram.TypeDocument {
		t.Fatalf("uploads = %+v", fake.Uploads)
	}
	head, err := objectStore.HeadObject(ctx, "photos", "hello.txt")
	if err != nil || head.SHA256 == "" || head.Size != 5 {
		t.Fatalf("head = %+v err = %v", head, err)
	}
	listed, err := objectStore.ListObjects(ctx, ListObjectsInput{Bucket: "photos", Limit: 10})
	if err != nil || len(listed.Objects) != 1 || listed.Objects[0].Key != "hello.txt" {
		t.Fatalf("listed = %+v err = %v", listed, err)
	}
	if err := objectStore.DeleteObject(ctx, "photos", "hello.txt"); err != nil {
		t.Fatalf("DeleteObject returned error: %v", err)
	}
	_, err = objectStore.HeadObject(ctx, "photos", "hello.txt")
	if err != ErrNoSuchKey {
		t.Fatalf("err = %v, want ErrNoSuchKey", err)
	}
}

func TestStoreChunkedPutAndFullGet(t *testing.T) {
	ctx := context.Background()
	objectStore, fake := newReadyTestObjectStoreWithUploadConfig(t, map[string]string{"backups": "-200"}, UploadConfig{Strategy: "document", EnableChunking: true, MaxFileSize: 50, ChunkSize: 3, TypeLimits: map[string]int64{"document": 3}})
	_, err := objectStore.PutObject(ctx, PutObjectInput{Bucket: "backups", Key: "big.bin", ContentType: "application/octet-stream", Size: 8, Body: strings.NewReader("abcdefgh")})
	if err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}
	if len(fake.Uploads) != 3 {
		t.Fatalf("uploads = %d", len(fake.Uploads))
	}
	reader, head, err := objectStore.GetObject(ctx, GetObjectInput{Bucket: "backups", Key: "big.bin"})
	if err != nil {
		t.Fatalf("GetObject returned error: %v", err)
	}
	defer reader.Close()
	data, _ := io.ReadAll(reader)
	if string(data) != "abcdefgh" || head.Size != 8 {
		t.Fatalf("data = %q head = %+v", string(data), head)
	}
}

func TestStoreRangeGetSingleFile(t *testing.T) {
	ctx := context.Background()
	objectStore, _ := newReadyTestObjectStore(t, map[string]string{"photos": "-100"})
	_, _ = objectStore.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "letters.txt", ContentType: "text/plain", Size: 8, Body: strings.NewReader("abcdefgh")})
	byteRange := ByteRange{Start: 2, End: 4}
	reader, _, err := objectStore.GetObject(ctx, GetObjectInput{Bucket: "photos", Key: "letters.txt", Range: &byteRange})
	if err != nil {
		t.Fatalf("GetObject returned error: %v", err)
	}
	defer reader.Close()
	data, _ := io.ReadAll(reader)
	if string(data) != "cde" {
		t.Fatalf("data = %q", string(data))
	}
}

func TestStoreMissingContentLength(t *testing.T) {
	objectStore, _ := newReadyTestObjectStore(t, map[string]string{"photos": "-100"})
	_, err := objectStore.PutObject(context.Background(), PutObjectInput{Bucket: "photos", Key: "x", Size: -1, Body: strings.NewReader("x")})
	if err != ErrMissingContentLength {
		t.Fatalf("err = %v, want ErrMissingContentLength", err)
	}
}
```

Also add `TestStoreRangeGetChunkedDownloadsOnlyOverlappingChunks` by extending `FakeTelegram` with a downloaded file ID log and asserting a Range inside the middle chunk does not download the first or last chunk. Add `TestStoreMissingBucketAndKeyErrors` to assert `ErrNoSuchBucket` and `ErrNoSuchKey`. Add `TestStoreLogsOrphanUploadWhenMetadataCommitFails` using a metadata test double that fails after Telegram upload, and assert the configured logger output contains `orphan_upload` plus bucket/key but not bot tokens or secret keys. Add semaphore tests or fake blocking Telegram tests to show `MaxUploads` serializes concurrent puts and `MaxDownloads` serializes concurrent gets when set to `1`.

Add these test helpers in `store/store_test.go`; they must initialize bucket metadata directly, mirroring startup configuration, and must not call any CreateBucket method:

```go
func newReadyTestObjectStore(t *testing.T, buckets map[string]string) (*ObjectStore, *testutil.FakeTelegram) {
	t.Helper()
	return newReadyTestObjectStoreWithUploadConfig(t, buckets, DefaultUploadConfig())
}

func newReadyTestObjectStoreWithUploadConfig(t *testing.T, buckets map[string]string, upload UploadConfig) (*ObjectStore, *testutil.FakeTelegram) {
	t.Helper()
	ctx := context.Background()
	meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	for name, chatID := range buckets {
		if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: name, ChatID: chatID, CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
			t.Fatalf("UpsertBucket(%s) returned error: %v", name, err)
		}
	}
	fake := testutil.NewFakeTelegram()
	caption, err := telegram.ParseCaptionTemplate("{bucket}:{key}:{part}:{parts}:{chunk}")
	if err != nil {
		t.Fatalf("ParseCaptionTemplate returned error: %v", err)
	}
	return NewObjectStore(meta, fake, Options{Upload: upload, Caption: caption}), fake
}
```

These helpers are the only way store tests create buckets; `ObjectStore` has no CreateBucket API in v1.

- [ ] **Step 3: Run object store tests and verify red state**

Run:

```bash
go test ./store ./internal/testutil
```

Expected: failure because object store is incomplete.

- [ ] **Step 4: Implement core store**

Create `store/store.go` implementing:

- `NewObjectStore(meta metadata.Store, tg telegram.Client, options Options) *ObjectStore`.
- `ListBuckets(ctx) ([]metadata.Bucket, error)`.
- `HeadBucket(ctx, name string) error`: returns nil only for enabled buckets initialized from startup configuration.
- `PutObject(ctx, PutObjectInput) (PutObjectResult, error)`:
  - Reject `Size < 0` with `ErrMissingContentLength`.
  - Verify bucket exists and enabled.
  - Lock on bucket/key.
  - Choose strategy before upload.
  - For non-chunked upload, first reject any object whose declared `Size` exceeds the resolved Telegram type limit. Then stream the request body to Telegram through an `io.Pipe` while `io.TeeReader` and hash writers compute MD5/SHA256 as bytes pass through. Use `PutBufferSize` as the copy buffer size, verify the copied byte count equals `Size`, and wait for the Telegram upload goroutine to return before committing metadata. Do not create temp files in v1.
  - For chunked upload, read each chunk into memory up to `ChunkSize`, compute whole-object MD5/SHA256 and per-chunk SHA256 as bytes are read, upload each chunk as `document`, then commit metadata after all Telegram uploads succeed.
  - Limit concurrent `PutObject` calls with a semaphore sized by `Options.MaxUploads` and concurrent full/range download streams with a semaphore sized by `Options.MaxDownloads`.
  - Wrap every `tg.Upload` and `tg.Download` call in the store with a semaphore sized by `Options.MaxTelegramCalls`; `telegram.HTTPClient` does not own this semaphore in v1.
  - If Telegram upload succeeds but metadata commit fails, log a structured `orphan_upload` event with bucket, key, uploaded file IDs/message IDs, and the metadata error; never include bot tokens or secrets.
  - Store lowercase hex MD5 ETag without quotes and SHA256 separately.
  - Render captions with `{part}`, `{parts}`, and `{chunk}` values.
- `HeadObject(ctx, bucket, key) (ObjectInfo, error)`.
- `GetObject(ctx, GetObjectInput) (io.ReadCloser, ObjectInfo, error)`:
  - For full reads, stream chunks sequentially through `io.Pipe` without buffering the full object.
  - For Range reads, select only overlapping chunks using metadata offsets.
  - Skip prefix bytes in the first selected chunk and limit bytes using `io.LimitReader`.
- `ListObjects(ctx, ListObjectsInput) (ListObjectsResult, error)`:
  - Ask metadata for ordered objects with `bucket`, `prefix`, `afterKey`, and enough rows to fill the requested page after delimiter collapsing.
  - If `Delimiter == ""`, return objects directly.
  - If `Delimiter != ""`, for each object key after `Prefix`, find the first delimiter occurrence. If none exists, append the object to `Objects`; if one exists, append `Prefix + segment + Delimiter` to `CommonPrefixes` only once for that page.
  - Count both emitted objects and newly emitted common prefixes toward `Limit`/`max-keys`.
  - Track the last scanned object key as `ListObjectsResult.NextContinuationAfter`, including keys that only contributed to an already-seen common prefix.
  - Set `ListObjectsResult.IsTruncated` when another metadata row exists after `NextContinuationAfter`.
  - Continue scanning metadata rows until the page is full or no rows remain; never use common prefix strings themselves as continuation tokens.
- `DeleteObject(ctx, bucket, key) error`: remove metadata only and return nil for missing object to match common DeleteObject behavior.
- `HumanSize(size int64) string` for captions.

- [ ] **Step 5: Run object store tests and verify green state**

Run:

```bash
go test ./store ./internal/testutil
```

Expected: PASS.

- [ ] **Step 6: Checkpoint**

If git is available:

```bash
git add internal/testutil/faketelegram.go internal/testutil/reader.go store/store.go store/store_test.go
git commit -m "feat: add Telegram-backed object store"
```

If this directory is not a git repository, write the changed file list in the session response for this checkpoint and continue.

### Task 6: Implement S3 XML Models, Errors, and List Tokens

**Files:**
- Create: `internal/s3api/errors.go`
- Create: `internal/s3api/xml.go`
- Create: `internal/s3api/list_token.go`
- Create: `internal/s3api/errors_test.go`
- Create: `internal/s3api/list_token_test.go`

- [ ] **Step 1: Write failing S3 utility tests**

Create tests for:

```go
func TestWriteErrorXML(t *testing.T) {
	recorder := httptest.NewRecorder()
	WriteError(recorder, S3Error{Code: "NoSuchKey", Message: "object not found", Status: http.StatusNotFound}, "/bucket/key", "req-1")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d", recorder.Code)
	}
	body := recorder.Body.String()
	for _, want := range []string{"<Code>NoSuchKey</Code>", "<Message>object not found</Message>", "<Resource>/bucket/key</Resource>", "<RequestId>req-1</RequestId>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %s: %s", want, body)
		}
	}
}

func TestMapStoreError(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{store.ErrNotImplemented, "NotImplemented"},
		{store.ErrNoSuchBucket, "NoSuchBucket"},
		{store.ErrNoSuchKey, "NoSuchKey"},
		{store.ErrInvalidRange, "InvalidRange"},
		{store.ErrMissingContentLength, "MissingContentLength"},
		{store.ErrEntityTooLarge, "EntityTooLarge"},
	}
	for _, tc := range cases {
		mapped := MapError(tc.err)
		if mapped.Code != tc.code {
			t.Fatalf("MapError(%v) = %s, want %s", tc.err, mapped.Code, tc.code)
		}
	}
}

func TestContinuationTokenRoundTrip(t *testing.T) {
	token, err := EncodeContinuationToken("photos/cat.jpg")
	if err != nil {
		t.Fatalf("EncodeContinuationToken returned error: %v", err)
	}
	lastKey, err := DecodeContinuationToken(token)
	if err != nil {
		t.Fatalf("DecodeContinuationToken returned error: %v", err)
	}
	if lastKey != "photos/cat.jpg" {
		t.Fatalf("lastKey = %q", lastKey)
	}
}

func TestDecodeContinuationTokenRejectsInvalid(t *testing.T) {
	_, err := DecodeContinuationToken("not-base64")
	if err == nil {
		t.Fatal("expected error")
	}
}
```

- [ ] **Step 2: Run S3 utility tests and verify red state**

Run:

```bash
go test ./internal/s3api
```

Expected: failure because utilities are incomplete.

- [ ] **Step 3: Implement S3 errors and XML models**

Create `internal/s3api/errors.go` with `ErrorResponse`, `WriteError`, and `S3Error` mapping for `NotImplemented`, `EntityTooLarge`, `InvalidAccessKeyId`, `SignatureDoesNotMatch`, `NoSuchBucket`, `NoSuchKey`, `InvalidRange`, `MissingContentLength`, `InvalidArgument`, `ServiceUnavailable`, and `InternalError`.

Create `internal/s3api/xml.go` with response structs for:

- `ListAllMyBucketsResult`
- `ListBucketResult` for ListObjectsV2
- `Contents`
- `CommonPrefixes`
- `Owner` only if needed by AWS SDK compatibility

- [ ] **Step 4: Implement continuation tokens**

Create `internal/s3api/list_token.go` using `base64.RawURLEncoding` over JSON:

```go
type continuationTokenPayload struct {
	LastKey string `json:"last_key"`
}
```

Decode errors must be reported by handlers as S3 `InvalidArgument`.

- [ ] **Step 5: Run S3 utility tests and verify green state**

Run:

```bash
go test ./internal/s3api
```

Expected: PASS.

- [ ] **Step 6: Checkpoint**

If git is available:

```bash
git add internal/s3api/errors.go internal/s3api/xml.go internal/s3api/list_token.go internal/s3api/errors_test.go internal/s3api/list_token_test.go
git commit -m "feat: add S3 XML utilities"
```

If this directory is not a git repository, write the changed file list in the session response for this checkpoint and continue.

### Task 7: Implement SigV4 Header Authentication

**Files:**
- Create: `internal/s3api/sigv4.go`
- Create: `internal/s3api/sigv4_test.go`

- [ ] **Step 1: Write failing SigV4 tests**

Create tests that include:

```go
func TestVerifySigV4AWSKnownExample(t *testing.T) {
	request, _ := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	request.Host = "examplebucket.s3.amazonaws.com"
	request.Header.Set("Range", "bytes=0-9")
	request.Header.Set("X-Amz-Date", "20130524T000000Z")
	request.Header.Set("X-Amz-Content-Sha256", EmptyPayloadSHA256)
	request.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c9645db77db45aa41455ba34f2e1a650f857d3dfd1c6d65b243db05")
	verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKIDEXAMPLE": "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"})
	identity, err := verifier.Verify(request)
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	if identity.AccessKey != "AKIDEXAMPLE" {
		t.Fatalf("identity = %+v", identity)
	}
}

func TestVerifySigV4RejectsUnknownAccessKey(t *testing.T) {
	request := signedTestRequest(t, "UNKNOWN", "SECRET", "us-east-1", "s3")
	verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"})
	_, err := verifier.Verify(request)
	if err != ErrInvalidAccessKeyID {
		t.Fatalf("err = %v, want ErrInvalidAccessKeyID", err)
	}
}

func TestVerifySigV4RejectsSignatureMismatch(t *testing.T) {
	request := signedTestRequest(t, "AKID", "WRONG", "us-east-1", "s3")
	verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"})
	_, err := verifier.Verify(request)
	if err != ErrSignatureDoesNotMatch {
		t.Fatalf("err = %v, want ErrSignatureDoesNotMatch", err)
	}
}

func TestVerifySigV4RejectsWrongRegionOrService(t *testing.T) {
	for _, request := range []*http.Request{signedTestRequest(t, "AKID", "SECRET", "eu-west-1", "s3"), signedTestRequest(t, "AKID", "SECRET", "us-east-1", "ec2")} {
		verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"})
		_, err := verifier.Verify(request)
		if err != ErrSignatureDoesNotMatch {
			t.Fatalf("err = %v, want ErrSignatureDoesNotMatch", err)
		}
	}
}

func TestVerifySigV4SignedByAWSSDK(t *testing.T) {
	request := signedTestRequest(t, "AKID", "SECRET", "us-east-1", "s3")
	verifier := NewSigV4Verifier("us-east-1", map[string]string{"AKID": "SECRET"})
	identity, err := verifier.Verify(request)
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	if identity.AccessKey != "AKID" {
		t.Fatalf("identity = %+v", identity)
	}
}

func signedTestRequest(t *testing.T, accessKey, secret, region, service string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "https://example.com/photos/hello.txt", nil)
	request.Header.Set("X-Amz-Content-Sha256", EmptyPayloadSHA256)
	credentials := aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secret}
	err := v4.NewSigner().SignHTTP(context.Background(), credentials, request, EmptyPayloadSHA256, service, region, time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatalf("SignHTTP returned error: %v", err)
	}
	return request
}
```

These tests need imports for `context`, `net/http`, `net/http/httptest`, `testing`, `time`, `github.com/aws/aws-sdk-go-v2/aws`, and `github.com/aws/aws-sdk-go-v2/aws/signer/v4`.

Use the AWS SDK signer only for negative and SDK-compatibility tests; do not use the verifier's canonicalization helper as the only source of truth for the known-vector assertion.

- [ ] **Step 2: Run SigV4 tests and verify red state**

Run:

```bash
go test ./internal/s3api
```

Expected: failure because verifier is incomplete.

- [ ] **Step 3: Implement SigV4 verifier**

Create `internal/s3api/sigv4.go` implementing:

- Header auth only: `Authorization: AWS4-HMAC-SHA256 ...`.
- Credential scope parsing: `access/date/region/service/aws4_request`.
- Region must equal configured region.
- Service must be `s3`.
- Unknown access key returns `ErrInvalidAccessKeyID`.
- Canonical URI, canonical query, canonical headers, signed header list, and payload hash.
- Use `X-Amz-Content-Sha256`; default only to empty hash when header is absent and request has no body.
- Constant-time signature comparison.
- Do not log or return canonical signature material in errors.

- [ ] **Step 4: Run SigV4 tests and verify green state**

Run:

```bash
go test ./internal/s3api
```

Expected: PASS.

- [ ] **Step 5: Checkpoint**

If git is available:

```bash
git add internal/s3api/sigv4.go internal/s3api/sigv4_test.go
git commit -m "feat: add SigV4 header authentication"
```

If this directory is not a git repository, write the changed file list in the session response for this checkpoint and continue.

### Task 8: Implement Complete S3 HTTP Server Routes

**Files:**
- Create: `internal/s3api/server.go`
- Create: `internal/s3api/server_test.go`

- [ ] **Step 1: Write failing server route tests**

Create `internal/s3api/server_test.go` with tests for:

```go
func TestRootNegotiationDefaultsToS3ListBuckets(t *testing.T) {
	server := newSignedTestServer(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	signRequest(t, request, "AKID", "SECRET")
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "ListAllMyBucketsResult") {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestRootNegotiationAllowsFutureHTML(t *testing.T) {
	server := newSignedTestServer(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept", "text/html")
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestRootAcceptApplicationXMLUsesS3ListBuckets(t *testing.T) {
	server := newSignedTestServer(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept", "application/xml")
	signRequest(t, request, "AKID", "SECRET")
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "ListAllMyBucketsResult") {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestPutHeadGetDeleteObject(t *testing.T) {
	server := newSignedTestServer(t)
	put := signedRecorderRequest(t, http.MethodPut, "/photos/hello.txt", "hello", map[string]string{"Content-Type": "text/plain"})
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}
	head := signedRecorderRequest(t, http.MethodHead, "/photos/hello.txt", "", nil)
	server.ServeHTTP(head.recorder, head.request)
	if head.recorder.Code != http.StatusOK || head.recorder.Header().Get("ETag") != `"5d41402abc4b2a76b9719d911017c592"` {
		t.Fatalf("head status = %d headers = %v", head.recorder.Code, head.recorder.Header())
	}
	get := signedRecorderRequest(t, http.MethodGet, "/photos/hello.txt", "", nil)
	server.ServeHTTP(get.recorder, get.request)
	if get.recorder.Code != http.StatusOK || get.recorder.Body.String() != "hello" {
		t.Fatalf("get status = %d body = %q", get.recorder.Code, get.recorder.Body.String())
	}
	deleteReq := signedRecorderRequest(t, http.MethodDelete, "/photos/hello.txt", "", nil)
	server.ServeHTTP(deleteReq.recorder, deleteReq.request)
	if deleteReq.recorder.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", deleteReq.recorder.Code)
	}
}

func TestGetObjectRange(t *testing.T) {
	server := newSignedTestServer(t)
	put := signedRecorderRequest(t, http.MethodPut, "/photos/letters.txt", "abcdefgh", nil)
	server.ServeHTTP(put.recorder, put.request)
	get := signedRecorderRequest(t, http.MethodGet, "/photos/letters.txt", "", nil)
	get.request.Header.Set("Range", "bytes=2-5")
	signRequest(t, get.request, "AKID", "SECRET")
	server.ServeHTTP(get.recorder, get.request)
	if get.recorder.Code != http.StatusPartialContent || get.recorder.Body.String() != "cdef" || get.recorder.Header().Get("Content-Range") != "bytes 2-5/8" {
		t.Fatalf("status = %d headers = %v body = %q", get.recorder.Code, get.recorder.Header(), get.recorder.Body.String())
	}
}

type signedHTTPTest struct {
	recorder *httptest.ResponseRecorder
	request  *http.Request
}

func newSignedTestServer(t *testing.T) http.Handler {
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
	objectStore := store.NewObjectStore(meta, fake, store.Options{Upload: store.DefaultUploadConfig()})
	return NewServer(objectStore, Options{Region: "us-east-1", Credentials: map[string]string{"AKID": "SECRET"}, Ready: func() bool { return true }})
}

func signedRecorderRequest(t *testing.T, method, path, body string, headers map[string]string) signedHTTPTest {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	sum := sha256.Sum256([]byte(body))
	request.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(sum[:]))
	signRequest(t, request, "AKID", "SECRET")
	return signedHTTPTest{recorder: httptest.NewRecorder(), request: request}
}

func signRequest(t *testing.T, request *http.Request, accessKey, secret string) {
	t.Helper()
	payloadHash := request.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		payloadHash = EmptyPayloadSHA256
		request.Header.Set("X-Amz-Content-Sha256", payloadHash)
	}
	request.Header.Del("Authorization")
	request.Header.Del("X-Amz-Date")
	credentials := aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secret}
	err := v4.NewSigner().SignHTTP(context.Background(), credentials, request, payloadHash, "s3", "us-east-1", time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatalf("SignHTTP returned error: %v", err)
	}
}
```

These tests need imports for `context`, `crypto/sha256`, `encoding/hex`, `net/http`, `net/http/httptest`, `path/filepath`, `strings`, `testing`, `time`, `github.com/aws/aws-sdk-go-v2/aws`, `github.com/aws/aws-sdk-go-v2/aws/signer/v4`, plus project packages `metadata`, `store`, and `internal/testutil`.

Also add explicit tests named `TestCreateBucketDisabled`, `TestHeadBucket`, `TestListObjectsV2WithContinuationToken`, `TestInvalidContinuationToken`, `TestAuthErrorsAreS3XML`, and `TestReadyzReturnsUnavailableWhenNotReady` with the assertions listed in their names. `TestCreateBucketDisabled` must assert `PUT /photos` returns S3 XML `NotImplemented` even when `photos` exists in startup configuration. `TestReadyzReturnsUnavailableWhenNotReady` must build `NewServer(objectStore, Options{Region: "us-east-1", Credentials: map[string]string{"AKID": "SECRET"}, Ready: func() bool { return false }})` and assert `GET /readyz` returns `503` without requiring SigV4.

- [ ] **Step 2: Run server tests and verify red state**

Run:

```bash
go test ./internal/s3api
```

Expected: failure because server routes are incomplete.

- [ ] **Step 3: Define explicit S3 object store interface**

In `internal/s3api/server.go`, define the dependency as:

```go
type ObjectStore interface {
	ListBuckets(ctx context.Context) ([]metadata.Bucket, error)
	HeadBucket(ctx context.Context, name string) error
	PutObject(ctx context.Context, input store.PutObjectInput) (store.PutObjectResult, error)
	GetObject(ctx context.Context, input store.GetObjectInput) (io.ReadCloser, store.ObjectInfo, error)
	HeadObject(ctx context.Context, bucket, key string) (store.ObjectInfo, error)
	ListObjects(ctx context.Context, input store.ListObjectsInput) (store.ListObjectsResult, error)
	DeleteObject(ctx context.Context, bucket, key string) error
}

type Options struct {
	Region      string
	Credentials map[string]string
	Ready       func() bool
}
```

Adapt `store.ObjectStore` to satisfy this interface by adding `HeadBucket` if not already present. Do not add a store-level CreateBucket method in v1; startup configuration writes bucket metadata directly through `metadata.Store`.

- [ ] **Step 4: Implement routing and request parsing**

Implement `ServeHTTP` with these routes:

- `GET /healthz` -> `200 ok`.
- `GET /readyz` -> `200 ready` only when `Options.Ready()` returns true; otherwise return `503 not ready`. If `Options.Ready` is nil, treat it as ready for tests that do not exercise readiness. `cmd/tgs3` uses an `atomic.Bool` and passes `Ready: ready.Load`, setting it true only after config is loaded, SQLite is initialized, startup buckets are upserted, and Telegram bot token basic validation has passed.
- `GET /` -> root negotiation and ListBuckets.
- `PUT /{bucket}` -> S3 XML `NotImplemented`; bucket metadata is created only at startup from config.
- `HEAD /{bucket}` -> HeadBucket.
- `GET /{bucket}` with `list-type=2` or no object key -> ListObjectsV2.
- `PUT /{bucket}/{key}` -> PutObject; require `Content-Length >= 0`.
- `GET /{bucket}/{key}` -> GetObject; parse optional `Range`.
- `HEAD /{bucket}/{key}` -> HeadObject.
- `DELETE /{bucket}/{key}` -> DeleteObject.

Use path-style addressing only. Preserve keys after the first slash without cleaning away valid S3 key characters.

- [ ] **Step 5: Implement auth handling**

Apply SigV4 to S3 routes. Requests with S3 characteristics and invalid credentials return XML auth errors. Health endpoints do not require SigV4. Future HTML root path does not require SigV4 when it has no S3 characteristic and explicitly prefers `text/html`.

- [ ] **Step 6: Implement response headers and XML bodies**

For object responses:

- `ETag` must be quoted, for example `"5d41402abc4b2a76b9719d911017c592"`.
- `Content-Length` must match full or partial body length.
- `Accept-Ranges: bytes` on `GET` and `HEAD` object responses.
- `Content-Range: bytes start-end/size` on `206` responses.
- `Content-Type`, `Last-Modified`, and status codes must match S3 client expectations.

For ListObjectsV2:

- Order by key ascending.
- Respect `prefix`, `delimiter`, `continuation-token`, and `max-keys`.
- When `delimiter` is set, return objects before the next delimiter as `Contents` and grouped child prefixes as `CommonPrefixes` with the delimiter included, matching S3 ListObjectsV2 semantics.
- Encode next continuation token from `store.ListObjectsResult.NextContinuationAfter` only when `store.ListObjectsResult.IsTruncated` is true.
- Invalid token returns `InvalidArgument`.

- [ ] **Step 7: Run server tests and verify green state**

Run:

```bash
go test ./internal/s3api
```

Expected: PASS.

- [ ] **Step 8: Checkpoint**

If git is available:

```bash
git add internal/s3api/server.go internal/s3api/server_test.go store/store.go store/store_test.go
git commit -m "feat: add S3 object API routes"
```

If this directory is not a git repository, write the changed file list in the session response for this checkpoint and continue.

### Task 9: Add AWS SDK Integration Tests

**Files:**
- Create: `internal/s3api/integration_test.go`

- [ ] **Step 1: Write AWS SDK integration tests**

Create tests that start an in-memory `httptest.Server` using fake Telegram and real SQLite. The helper must insert `photos -> -100` and `backups -> -200` into metadata before creating the S3 server, matching startup-configured bucket behavior. Then configure AWS SDK for Go v2 with:

- static credentials matching server config;
- region `us-east-1`;
- path-style S3 addressing;
- `s3.Options.BaseEndpoint` pointing to the test server URL;
- AWS SDK config imported as `awsconfig "github.com/aws/aws-sdk-go-v2/config"` to avoid colliding with this project's `config` package.

Create this helper before the tests:

```go
func newAWSSDKTestClient(t *testing.T) (*s3.Client, *testutil.FakeTelegram) {
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
	objectStore := store.NewObjectStore(meta, fake, store.Options{Upload: store.DefaultUploadConfig()})
	s3Server := NewServer(objectStore, Options{Region: "us-east-1", Credentials: map[string]string{"AKID": "SECRET"}, Ready: func() bool { return true }})
	httpServer := httptest.NewServer(s3Server)
	t.Cleanup(httpServer.Close)
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("AKID", "SECRET", "")),
	)
	if err != nil {
		t.Fatalf("LoadDefaultConfig returned error: %v", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(httpServer.URL)
		o.UsePathStyle = true
	})
	return client, fake
}
```

Tests required:

```go
func TestAWSSDKObjectLifecycleAgainstConfiguredBucket(t *testing.T) {
	client, _ := newAWSSDKTestClient(t)
	ctx := context.Background()
	_, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("photos"), Key: aws.String("hello.txt"), Body: strings.NewReader("hello"), ContentType: aws.String("text/plain")})
	if err != nil {
		t.Fatalf("PutObject returned error: %v", err)
	}
	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("photos"), Key: aws.String("hello.txt")})
	if err != nil || head.ContentLength == nil || *head.ContentLength != 5 {
		t.Fatalf("HeadObject = %+v err = %v", head, err)
	}
	get, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("photos"), Key: aws.String("hello.txt")})
	if err != nil {
		t.Fatalf("GetObject returned error: %v", err)
	}
	defer get.Body.Close()
	body, _ := io.ReadAll(get.Body)
	if string(body) != "hello" {
		t.Fatalf("body = %q", string(body))
	}
	_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("photos"), Key: aws.String("hello.txt")})
	if err != nil {
		t.Fatalf("DeleteObject returned error: %v", err)
	}
}

func TestAWSSDKListObjectsV2Pagination(t *testing.T) {
	client, _ := newAWSSDKTestClient(t)
	ctx := context.Background()
	for _, key := range []string{"a/1.txt", "a/2.txt", "a/3.txt"} {
		_, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("photos"), Key: aws.String(key), Body: strings.NewReader(key)})
		if err != nil {
			t.Fatalf("PutObject(%s): %v", key, err)
		}
	}
	first, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("photos"), Prefix: aws.String("a/"), MaxKeys: aws.Int32(1)})
	if err != nil || len(first.Contents) != 1 || first.NextContinuationToken == nil {
		t.Fatalf("first page = %+v err = %v", first, err)
	}
	second, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("photos"), Prefix: aws.String("a/"), ContinuationToken: first.NextContinuationToken, MaxKeys: aws.Int32(2)})
	if err != nil || len(second.Contents) != 2 {
		t.Fatalf("second page = %+v err = %v", second, err)
	}
}

func TestAWSSDKRangeGetObject(t *testing.T) {
	client, _ := newAWSSDKTestClient(t)
	ctx := context.Background()
	_, _ = client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("photos"), Key: aws.String("letters.txt"), Body: strings.NewReader("abcdefgh")})
	out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("photos"), Key: aws.String("letters.txt"), Range: aws.String("bytes=2-5")})
	if err != nil {
		t.Fatalf("GetObject returned error: %v", err)
	}
	defer out.Body.Close()
	body, _ := io.ReadAll(out.Body)
	if string(body) != "cdef" || out.ContentRange == nil || *out.ContentRange != "bytes 2-5/8" {
		t.Fatalf("body = %q contentRange = %v", string(body), out.ContentRange)
	}
}

func TestAWSSDKTwoBucketsUseDifferentChats(t *testing.T) {
	client, fake := newAWSSDKTestClient(t)
	ctx := context.Background()
	_, _ = client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("photos"), Key: aws.String("p.txt"), Body: strings.NewReader("p")})
	_, _ = client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("backups"), Key: aws.String("b.txt"), Body: strings.NewReader("b")})
	if len(fake.Uploads) != 2 || fake.Uploads[0].ChatID == fake.Uploads[1].ChatID {
		t.Fatalf("uploads = %+v", fake.Uploads)
	}
}
```

- [ ] **Step 2: Run integration tests and verify red or compile state**

Run:

```bash
go test ./internal/s3api -run 'TestAWSSDK'
```

Expected before final fixes: failure only for concrete compatibility mismatches, not missing packages.

- [ ] **Step 3: Fix compatibility mismatches**

Fix only issues surfaced by the AWS SDK tests: endpoint signing host, XML names, status codes, headers, ListObjectsV2 query parsing, and Range response metadata. Do not add v2 features such as presigned URLs, ACLs, policies, versioning, tags, lifecycle, multipart upload API, virtual-hosted-style routing, or conditional requests.

- [ ] **Step 4: Run integration tests and verify green state**

Run:

```bash
go test ./internal/s3api -run 'TestAWSSDK'
```

Expected: PASS.

- [ ] **Step 5: Add MinIO client or rclone smoke check**

If `mc` is available locally, run a path-style smoke test against a local `cmd/tgs3` process using a fake/local Telegram setup only if the current implementation exposes one for development. Create `/tmp/tgs3-smoke.yaml` with the same auth and configured bucket shape used by integration tests:

```yaml
server:
  listen: ":9000"
auth:
  region: "us-east-1"
  credentials:
    - access_key: "AKID"
      secret_key_env: "TGS3_SMOKE_SECRET"
telegram:
  bot_token_env: "TGS3_SMOKE_BOT_TOKEN"
  api_base_url: "http://127.0.0.1:19090"
  timeout: "30s"
metadata:
  driver: "sqlite"
  sqlite_path: "/tmp/tgs3-smoke.sqlite"
storage:
  upload_type_strategy: "document"
  enable_chunking: true
buckets:
  photos:
    chat_id: "-100"
```

Run the service and smoke commands:

```bash
export TGS3_SMOKE_SECRET=SECRET
export TGS3_SMOKE_BOT_TOKEN=123456:smoke
rm -f /tmp/tgs3-smoke.sqlite
./tgs3 -config /tmp/tgs3-smoke.yaml
mc alias set tgs3 http://127.0.0.1:9000 AKID SECRET --api S3v4
printf 'hello' | mc pipe tgs3/photos/tgs3-smoke.txt
mc cat tgs3/photos/tgs3-smoke.txt
mc ls tgs3/photos
mc rm tgs3/photos/tgs3-smoke.txt
```

Expected: `mc cat` prints `hello`, `mc ls` shows the object before deletion, and `mc rm` succeeds. Stop the service after the smoke test. If no fake/local Telegram endpoint exists, or if neither `mc` nor `rclone` is installed, state the skip reason in the session response and keep the AWS SDK tests as the automated compatibility gate.

- [ ] **Step 6: Checkpoint**

If git is available:

```bash
git add internal/s3api/integration_test.go internal/s3api/*.go store/*.go
git commit -m "test: add AWS SDK S3 compatibility coverage"
```

If this directory is not a git repository, write the changed file list in the session response for this checkpoint and continue.

### Task 10: Wire Service Entrypoint and Container Image

**Files:**
- Create: `cmd/tgs3/main.go`
- Create: `Dockerfile`
- Create: `.dockerignore`
- Create: `README.md`

- [ ] **Step 1: Implement service entrypoint**

Create `cmd/tgs3/main.go` with final imports and no known compile errors. The startup path must mark readiness false until config loading, SQLite initialization, startup bucket upserts, and Telegram bot token basic validation all succeed. Basic token validation can be format validation that rejects empty tokens and tokens without the `number:secret` Bot API shape; do not call Telegram or send network requests during readiness setup.

Then create the main file:

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aahl/tgs3/config"
	"github.com/aahl/tgs3/internal/s3api"
	"github.com/aahl/tgs3/metadata"
	"github.com/aahl/tgs3/store"
	"github.com/aahl/tgs3/telegram"
)

func validateBotToken(token string) error {
	parts := strings.SplitN(token, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("invalid Telegram bot token format")
	}
	if _, err := strconv.ParseInt(parts[0], 10, 64); err != nil {
		return fmt.Errorf("invalid Telegram bot token prefix")
	}
	return nil
}

func main() {
	configPath := flag.String("config", "/etc/tgs3/config.yaml", "path to YAML config")
	flag.Parse()
	var ready atomic.Bool

	cfg, err := config.LoadFile(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	sqlitePath, err := cfg.ResolveSQLitePath()
	if err != nil {
		log.Fatalf("resolve sqlite path: %v", err)
	}
	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		log.Fatalf("open sqlite metadata: %v", err)
	}
	defer meta.Close()

	ctx := context.Background()
	for name, bucket := range cfg.Buckets {
		if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: name, ChatID: bucket.ChatID, CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
			log.Fatalf("upsert bucket %s: %v", name, err)
		}
	}

	botToken := cfg.ResolveBotToken()
	if err := validateBotToken(botToken); err != nil {
		log.Fatalf("validate bot token: %v", err)
	}
	caption, err := telegram.ParseCaptionTemplate(cfg.Telegram.CaptionTemplate)
	if err != nil {
		log.Fatalf("parse caption template: %v", err)
	}
	tg := telegram.NewHTTPClient(botToken, cfg.Telegram.APIBaseURL, &http.Client{Timeout: cfg.Telegram.Timeout})
	objectStore := store.NewObjectStore(meta, tg, store.Options{
		Upload: store.UploadConfig{
			Strategy:      cfg.Storage.UploadTypeStrategy,
			EnableChunking: *cfg.Storage.EnableChunking,
			MaxFileSize:   cfg.Storage.MaxFileSize,
			ChunkSize:     cfg.Storage.ChunkSize,
			TypeLimits:    cfg.Storage.TypeSizeLimits,
			PutBufferSize: cfg.Storage.PutBufferSize,
		},
		Caption:          caption,
		MaxUploads:       cfg.Storage.MaxConcurrentUploads,
		MaxDownloads:     cfg.Storage.MaxConcurrentDownloads,
		MaxTelegramCalls: cfg.Storage.MaxConcurrentTelegramRequests,
	})
	secrets := map[string]string{}
	for _, credential := range cfg.Auth.Credentials {
		secrets[credential.AccessKey] = cfg.ResolveSecret(credential.SecretKeyEnv)
	}
	ready.Store(true)
	server := s3api.NewServer(objectStore, s3api.Options{Region: cfg.Auth.Region, Credentials: secrets, Ready: ready.Load})
	log.Printf("listening on %s", cfg.ResolveListen())
	if err := http.ListenAndServe(cfg.ResolveListen(), server); err != nil {
		log.Fatal(err)
	}
}
```

- [ ] **Step 2: Add container files**

Create `Dockerfile`:

```dockerfile
FROM golang:1.22 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/tgs3 ./cmd/tgs3

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tgs3 /tgs3
USER nonroot:nonroot
EXPOSE 9000
ENTRYPOINT ["/tgs3"]
CMD ["-config", "/etc/tgs3/config.yaml"]
```

Create `.dockerignore`:

```text
.git
.claude
.agents
vendor
*.sqlite
*.db
docs/superpowers/plans
```

- [ ] **Step 3: Add minimal README**

Create `README.md` with the v1 scope, config example, env vars, path-style endpoint, health endpoints, and explicit v2 exclusions for presigned URLs and multipart upload API. Include the sample YAML from the design with `upload_type_strategy: "document"`, `metadata.sqlite_path`, and `metadata.sqlite_path_env`.

- [ ] **Step 4: Run full tests and build**

Run:

```bash
go test ./...
go build ./cmd/tgs3
```

Expected: both commands exit 0.

- [ ] **Step 5: Checkpoint**

If git is available:

```bash
git add cmd/tgs3/main.go Dockerfile .dockerignore README.md go.mod go.sum
git commit -m "feat: add tgs3 service entrypoint"
```

If this directory is not a git repository, write the changed file list in the session response for this checkpoint and continue.

### Task 11: Final Verification Against Approved Design

**Files:**
- Verify: all created Go files
- Verify: `docs/superpowers/specs/2026-05-22-go-telegram-s3-design.md`

- [ ] **Step 1: Run unit and integration tests**

Run:

```bash
go test ./...
```

Expected: PASS.

- [ ] **Step 2: Run service build**

Run:

```bash
go build ./cmd/tgs3
```

Expected: PASS.

- [ ] **Step 3: Verify v1 requirement checklist**

Check each item manually against tests and implementation:

- Go library package exists in `store`.
- Service binary exists in `cmd/tgs3`.
- Multiple buckets are metadata namespaces.
- Bucket-to-chat mapping comes from config.
- SigV4 header authentication is enforced on S3 routes.
- `GET /` ListBuckets works by default.
- Future HTML root is reserved only for explicit non-S3 `Accept: text/html` requests.
- `PUT /{bucket}` returns S3 XML `NotImplemented` because CreateBucket is disabled in v1.
- `HEAD /{bucket}`, `GET /{bucket}`, `PUT /{bucket}/{key}`, `GET /{bucket}/{key}`, `HEAD /{bucket}/{key}`, `DELETE /{bucket}/{key}` are implemented for startup-configured buckets.
- `ListObjectsV2` supports `prefix`, `delimiter`, `continuation-token`, and `max-keys`.
- Default Telegram upload strategy is `document`.
- Optional `auto` strategy supports typed upload, document fallback, and chunked document.
- Caption template supports `{bucket}`, `{key}`, `{name}`, `{size}`, `{bytes}`, `{part}`, `{parts}`, `{chunk}`.
- Missing `Content-Length` returns `MissingContentLength`.
- Full reads and Range reads return original bytes.
- Chunked Range reads download only overlapping chunks in store tests.
- SQLite path resolution is env first, config second, error otherwise.
- Health endpoints exist.
- ETag is stored as lowercase MD5 hex and returned quoted in HTTP.
- SHA256 is stored separately.
- DeleteObject removes metadata only.
- No presigned URL, presigned POST, ACL, policy, versioning, tags, lifecycle, SSE, virtual-hosted-style, or multipart upload API was added.

- [ ] **Step 4: Record final status**

If all checks pass, report exact commands run and their passing output summaries. If any check fails, keep the relevant task open and fix before reporting completion.

## Self-Review Checklist

- Spec coverage:
  - Go reusable library and service binary: Tasks 5 and 10.
  - YAML config and env resolution: Task 1.
  - SQLite metadata behind interface: Task 2.
  - Telegram typed/document upload, caption template, and download: Task 3.
  - Upload strategy and chunking: Task 4.
  - Full and Range reads: Tasks 4, 5, 8, 9.
  - Object-level locking: Task 4.
  - S3 XML errors, continuation tokens, SigV4, full object routes: Tasks 6, 7, 8.
  - AWS SDK compatibility: Task 9.
  - Docker/Kubernetes-friendly entrypoint and port 9000: Task 10.
- Placeholder scan:
  - No task is deferred outside this plan.
  - No v1 S3 route is left for later.
  - No compile-error-first entrypoint is included.
- Type consistency:
  - `metadata.Store` is consumed by `store.ObjectStore`.
  - `telegram.Client` is consumed by `store.ObjectStore` and fake Telegram.
  - `internal/s3api.ObjectStore` explicitly matches the methods used by HTTP handlers.
  - `store.ByteRange` is shared by store and S3 layers.

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-05-22-go-telegram-s3.md`. Two execution options:

1. **Subagent-Driven (recommended)** - Dispatch a fresh subagent per task, review between tasks, fast iteration.
2. **Inline Execution** - Execute tasks in this session using executing-plans, batch execution with checkpoints.

Which approach?
