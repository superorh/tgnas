package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

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
      secret_key_env: "TGNAS_TEST_SECRET"
telegram:
  bot_token_env: "TGNAS_TEST_BOT_TOKEN"
  api_base_url: "https://api.telegram.org"
  timeout: "45s"
metadata:
  driver: "sqlite"
  sqlite_path: "/tmp/tgnas.sqlite"
storage:
  upload_type_strategy: "document"
buckets:
  photos:
    chat_id: "-100123"
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TGNAS_TEST_SECRET", "secret")
	t.Setenv("TGNAS_TEST_BOT_TOKEN", "bot-token")

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
	if token, _, err := cfg.ResolveBotToken(); err != nil {
		t.Fatalf("ResolveBotToken returned error: %v", err)
	} else if token != "bot-token" {
		t.Fatalf("bot token = %q", token)
	}
}

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
  sqlite_path: "/tmp/tgnas.sqlite"
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

func TestLoadConfigDefaultsRegionAndSQLitePath(t *testing.T) {
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
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile returned error: %v", err)
	}
	if cfg.Auth.Region != "us-east-1" {
		t.Fatalf("region = %q", cfg.Auth.Region)
	}
	got, err := cfg.ResolveSQLitePath()
	if err != nil {
		t.Fatalf("ResolveSQLitePath returned error: %v", err)
	}
	if got != "data/metadata.sqlite" {
		t.Fatalf("path = %q", got)
	}
}

func TestDefaultListenEnvOverridesListen(t *testing.T) {
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
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TGNAS_LISTEN", "127.0.0.1:12345")

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile returned error: %v", err)
	}
	if got := cfg.ResolveListen(); got != "127.0.0.1:12345" {
		t.Fatalf("listen = %q", got)
	}
}

func TestSQLitePathEnvPrecedence(t *testing.T) {
	t.Setenv("TGNAS_SQLITE_PATH", "/env/tgnas.sqlite")
	cfg := Config{Metadata: MetadataConfig{Driver: "sqlite", SQLitePath: "/file/tgnas.sqlite", SQLitePathEnv: "TGNAS_SQLITE_PATH"}}
	got, err := cfg.ResolveSQLitePath()
	if err != nil {
		t.Fatalf("ResolveSQLitePath returned error: %v", err)
	}
	if got != "/env/tgnas.sqlite" {
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
  sqlite_path: "/tmp/tgnas.sqlite"
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
  sqlite_path: "/tmp/tgnas.sqlite"
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

func TestBucketChatIDResolvesFullEnvReference(t *testing.T) {
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
    chat_id: "${TGNAS_PHOTOS_CHAT_ID}"
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TGNAS_PHOTOS_CHAT_ID", "-100999")

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile returned error: %v", err)
	}
	if got := cfg.Buckets["photos"].ChatID; got != "-100999" {
		t.Fatalf("chat_id = %q", got)
	}
}

func TestBucketChatIDRejectsPartialEnvInterpolation(t *testing.T) {
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
    chat_id: "prefix-${TGNAS_PHOTOS_CHAT_ID}"
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TGNAS_PHOTOS_CHAT_ID", "123")

	_, err = LoadFile(path)
	if err == nil {
		t.Fatal("expected partial interpolation error")
	}
}

func TestBucketChatIDFailsWhenReferencedEnvMissing(t *testing.T) {
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
    chat_id: "${TGNAS_MISSING_CHAT_ID}"
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TGNAS_MISSING_CHAT_ID", "")

	_, err = LoadFile(path)
	if err == nil {
		t.Fatal("expected empty chat id validation error")
	}
}

func minimalValidConfig() Config {
	return Config{
		Server:   ServerConfig{Listen: ":9000"},
		Auth:     AuthConfig{Region: "us-east-1", Credentials: []CredentialConfig{{AccessKey: "admin", SecretKeyEnv: "SECRET"}}},
		Telegram: TelegramConfig{BotTokenEnv: "BOT_TOKEN", APIBaseURL: "https://api.telegram.org", Timeout: 30 * time.Second},
		Metadata: MetadataConfig{Driver: "sqlite", SQLitePath: "/tmp/tgnas.sqlite"},
		Storage:  DefaultStorageConfig(),
		Buckets:  map[string]BucketConfig{"photos": {ChatID: "-100123"}},
	}
}

func TestWebDAVPrefixDefault(t *testing.T) {
	cfg := Config{WebDAV: WebDAVConfig{}}
	cfg.applyWebDAVDefaults()
	if cfg.WebDAV.Prefix != "/dav/" {
		t.Fatalf("expected default prefix /dav/, got %q", cfg.WebDAV.Prefix)
	}
}

func TestWebDAVPrefixNormalizesTrailingSlash(t *testing.T) {
	cfg := Config{WebDAV: WebDAVConfig{Prefix: "/dav"}}
	cfg.applyWebDAVDefaults()
	if cfg.WebDAV.Prefix != "/dav/" {
		t.Fatalf("expected /dav/, got %q", cfg.WebDAV.Prefix)
	}
}

func TestWebDAVPrefixRejectsRoot(t *testing.T) {
	cfg := Config{WebDAV: WebDAVConfig{Prefix: "/"}}
	err := cfg.validateWebDAV()
	if err == nil || !strings.Contains(err.Error(), "cannot be /") {
		t.Fatalf("expected root prefix rejected, got %v", err)
	}
}

func TestWebDAVPrefixRejectsMissingLeadingSlash(t *testing.T) {
	cfg := Config{WebDAV: WebDAVConfig{Prefix: "dav/"}}
	err := cfg.validateWebDAV()
	if err == nil || !strings.Contains(err.Error(), "must start with /") {
		t.Fatalf("expected missing leading slash rejected, got %v", err)
	}
}

func TestWebDAVPrefixRejectsBucketNameConflict(t *testing.T) {
	cfg := Config{
		Buckets: map[string]BucketConfig{"photos": {ChatID: "123"}},
		WebDAV:  WebDAVConfig{Prefix: "/photos/"},
	}
	err := cfg.validateWebDAV()
	if err == nil || !strings.Contains(err.Error(), "conflicts with bucket") {
		t.Fatalf("expected bucket conflict rejected, got %v", err)
	}
}

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
