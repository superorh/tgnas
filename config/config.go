package config

import (
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var raw string
	if err := value.Decode(&raw); err != nil {
		return err
	}

	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", raw, err)
	}

	*d = Duration(parsed)
	return nil
}

type Config struct {
	Server   ServerConfig            `yaml:"server"`
	Auth     AuthConfig              `yaml:"auth"`
	Telegram TelegramConfig          `yaml:"telegram"`
	Metadata MetadataConfig          `yaml:"metadata"`
	Storage  StorageConfig           `yaml:"storage"`
	Buckets  map[string]BucketConfig `yaml:"buckets"`
	WebDAV   WebDAVConfig            `yaml:"webdav"`
}

type ServerConfig struct {
	Listen            string   `yaml:"listen"`
	ListenEnv         string   `yaml:"listen_env"`
	PublicBaseURL     string   `yaml:"public_base_url"`
	TrustedProxies    []string `yaml:"trusted_proxies"`
	TrustedProxyHosts []string `yaml:"trusted_proxy_hosts"`
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
	CaptionTemplate string        `yaml:"caption_template"`
	Timeout         time.Duration `yaml:"-"`
	RawTimeout      Duration      `yaml:"timeout"`
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
	ChatID     string `yaml:"chat_id"`
	PublicRead bool   `yaml:"public_read"`
}

type WebDAVConfig struct {
	Prefix string `yaml:"prefix"`
}

func (c *Config) applyWebDAVDefaults() {
	if c.WebDAV.Prefix == "" {
		c.WebDAV.Prefix = "/dav/"
	}
	if !strings.HasSuffix(c.WebDAV.Prefix, "/") {
		c.WebDAV.Prefix += "/"
	}
}

func (c Config) validateWebDAV() error {
	prefix := c.WebDAV.Prefix
	if prefix == "/" {
		return fmt.Errorf("webdav prefix cannot be /")
	}
	if !strings.HasPrefix(prefix, "/") {
		return fmt.Errorf("webdav prefix must start with /")
	}
	firstSegment := strings.TrimPrefix(prefix, "/")
	if idx := strings.Index(firstSegment, "/"); idx >= 0 {
		firstSegment = firstSegment[:idx]
	}
	if _, exists := c.Buckets[firstSegment]; exists {
		return fmt.Errorf("webdav prefix first segment %q conflicts with bucket name", firstSegment)
	}
	return nil
}

func LoadFile(path string) (Config, error) {
	cfg := Config{
		Server: ServerConfig{Listen: ":9000", ListenEnv: "TGNAS_LISTEN"},
		Auth:   AuthConfig{Region: "us-east-1"},
		Telegram: TelegramConfig{
			APIBaseURL: "https://api.telegram.org",
			Timeout:    30 * time.Second,
		},
		Metadata: MetadataConfig{Driver: "sqlite", SQLitePath: "data/metadata.sqlite"},
		Storage:  DefaultStorageConfig(),
		Buckets:  map[string]BucketConfig{},
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}

	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, err
	}

	if cfg.Telegram.RawTimeout != 0 {
		cfg.Telegram.Timeout = time.Duration(cfg.Telegram.RawTimeout)
	}

	applyStorageDefaults(&cfg.Storage)
	cfg.applyWebDAVDefaults()

	for name, bucket := range cfg.Buckets {
		chatID, err := resolveBucketChatID(bucket.ChatID)
		if err != nil {
			return Config{}, fmt.Errorf("resolve bucket %q chat_id: %w", name, err)
		}
		bucket.ChatID = chatID
		cfg.Buckets[name] = bucket
	}

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
		PutBufferSize:                 1 * 1024 * 1024,
	}
}

func (c Config) ResolveListen() string {
	if c.Server.ListenEnv != "" {
		if value := os.Getenv(c.Server.ListenEnv); value != "" {
			return value
		}
	}
	return c.Server.Listen
}

func (c Config) ResolveSecret(envName string) string {
	if envName == "" {
		return ""
	}
	return os.Getenv(envName)
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
	return "", fmt.Errorf("metadata sqlite path is required")
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.ResolveListen()) == "" {
		return fmt.Errorf("server listen is required")
	}
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

	if strings.TrimSpace(c.Auth.Region) == "" {
		return fmt.Errorf("auth region is required")
	}
	if len(c.Auth.Credentials) == 0 {
		return fmt.Errorf("at least one auth credential is required")
	}
	for i, cred := range c.Auth.Credentials {
		if strings.TrimSpace(cred.AccessKey) == "" {
			return fmt.Errorf("auth credential %d access key is required", i)
		}
		if strings.TrimSpace(cred.SecretKeyEnv) == "" {
			return fmt.Errorf("auth credential %d secret key env is required", i)
		}
	}

	if strings.TrimSpace(c.Telegram.BotTokenEnv) == "" {
		return fmt.Errorf("telegram bot token env is required")
	}
	if strings.TrimSpace(c.Telegram.APIBaseURL) == "" {
		return fmt.Errorf("telegram api base url is required")
	}

	if strings.TrimSpace(c.Metadata.Driver) != "sqlite" {
		return fmt.Errorf("metadata driver must be sqlite")
	}
	path, err := c.ResolveSQLitePath()
	if err != nil {
		return err
	}
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("metadata sqlite path is required")
	}

	if c.Storage.UploadTypeStrategy != "document" && c.Storage.UploadTypeStrategy != "auto" {
		return fmt.Errorf("storage upload type strategy must be document or auto")
	}
	if c.Storage.MaxFileSize <= 0 {
		return fmt.Errorf("storage max file size must be positive")
	}
	if c.Storage.ChunkSize <= 0 {
		return fmt.Errorf("storage chunk size must be positive")
	}
	if c.Storage.TypeSizeLimits["document"] <= 0 {
		return fmt.Errorf("storage document type size limit must be positive")
	}

	if len(c.Buckets) == 0 {
		return fmt.Errorf("at least one bucket is required")
	}
	for name, bucket := range c.Buckets {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("bucket name is required")
		}
		if strings.TrimSpace(bucket.ChatID) == "" {
			return fmt.Errorf("bucket %q chat id is required", name)
		}
	}

	if err := c.validateWebDAV(); err != nil {
		return err
	}

	return nil
}

func applyStorageDefaults(storage *StorageConfig) {
	defaults := DefaultStorageConfig()

	if storage.UploadTypeStrategy == "" {
		storage.UploadTypeStrategy = defaults.UploadTypeStrategy
	}
	if storage.EnableChunking == nil {
		storage.EnableChunking = boolPtr(*defaults.EnableChunking)
	}
	if storage.MaxFileSize == 0 {
		storage.MaxFileSize = defaults.MaxFileSize
	}
	if storage.ChunkSize == 0 {
		storage.ChunkSize = defaults.ChunkSize
	}
	if storage.TypeSizeLimits == nil {
		storage.TypeSizeLimits = map[string]int64{}
	}
	for name, limit := range defaults.TypeSizeLimits {
		if storage.TypeSizeLimits[name] == 0 {
			storage.TypeSizeLimits[name] = limit
		}
	}
	if storage.MaxConcurrentUploads == 0 {
		storage.MaxConcurrentUploads = defaults.MaxConcurrentUploads
	}
	if storage.MaxConcurrentDownloads == 0 {
		storage.MaxConcurrentDownloads = defaults.MaxConcurrentDownloads
	}
	if storage.MaxConcurrentTelegramRequests == 0 {
		storage.MaxConcurrentTelegramRequests = defaults.MaxConcurrentTelegramRequests
	}
	if storage.PutBufferSize == 0 {
		storage.PutBufferSize = defaults.PutBufferSize
	}
}

func resolveBucketChatID(value string) (string, error) {
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

func boolPtr(v bool) *bool {
	return &v
}
