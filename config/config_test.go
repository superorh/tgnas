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

func TestLoadConfigReadsTask1FieldNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	err := os.WriteFile(path, []byte(`
server:
  listen: ":9000"
  public_base_url: "https://files.example.com"
auth:
  region: "us-east-1"
  credentials:
    - access_key: "admin"
      secret_key_env: "SECRET"
telegram:
  bot_token_env: "BOT_TOKEN"
  api_base_url: "https://api.telegram.org"
  caption_template: "{{.Name}}"
metadata:
  driver: "sqlite"
  sqlite_path: "/tmp/tgs3.sqlite"
storage:
  upload_type_strategy: "document"
  type_size_limits:
    document: 1234
  max_concurrent_telegram_requests: 7
  put_buffer_size: 4096
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
	if cfg.Server.PublicBaseURL != "https://files.example.com" {
		t.Fatalf("public base url = %q", cfg.Server.PublicBaseURL)
	}
	if cfg.Telegram.CaptionTemplate != "{{.Name}}" {
		t.Fatalf("caption template = %q", cfg.Telegram.CaptionTemplate)
	}
	if cfg.Storage.TypeSizeLimits["document"] != 1234 {
		t.Fatalf("document type size limit = %d", cfg.Storage.TypeSizeLimits["document"])
	}
	if cfg.Storage.MaxConcurrentTelegramRequests != 7 {
		t.Fatalf("max concurrent telegram requests = %d", cfg.Storage.MaxConcurrentTelegramRequests)
	}
	if cfg.Storage.PutBufferSize != 4096 {
		t.Fatalf("put buffer size = %d", cfg.Storage.PutBufferSize)
	}
}

func TestLoadConfigRejectsUnknownYAMLField(t *testing.T) {
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
  upload_strategy: "document"
buckets:
  photos:
    chat_id: "-100123"
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, err = LoadFile(path)
	if err == nil {
		t.Fatal("expected unknown field error")
	}
}

func TestValidateRejectsUnsupportedMetadataDriver(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Metadata.Driver = "postgres"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidateRejectsMissingDocumentTypeSizeLimit(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Storage.TypeSizeLimits["document"] = 0
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
