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
	parts := strings.SplitN(strings.TrimSpace(token), ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("invalid Telegram bot token format")
	}
	if _, err := strconv.ParseInt(parts[0], 10, 64); err != nil {
		return fmt.Errorf("invalid Telegram bot token prefix")
	}
	return nil
}

var newObjectStore = store.NewObjectStore
var listenAndServe = http.ListenAndServe

func main() {
	configPath := flag.String("config", "/etc/tgs3/config.yaml", "path to YAML config")
	flag.Parse()
	if err := run(*configPath); err != nil {
		log.Fatal(err)
	}
}

func run(configPath string) error {
	var ready atomic.Bool

	cfg, err := config.LoadFile(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	sqlitePath, err := cfg.ResolveSQLitePath()
	if err != nil {
		return fmt.Errorf("resolve sqlite path: %w", err)
	}

	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		return fmt.Errorf("open sqlite metadata: %w", err)
	}
	defer meta.Close()

	ctx := context.Background()
	for name, bucket := range cfg.Buckets {
		if err := meta.UpsertBucket(ctx, metadata.Bucket{
			Name:      name,
			ChatID:    bucket.ChatID,
			CreatedAt: time.Now().UTC(),
			Enabled:   true,
		}); err != nil {
			return fmt.Errorf("upsert bucket %s: %w", name, err)
		}
	}

	botToken := cfg.ResolveBotToken()
	if err := validateBotToken(botToken); err != nil {
		return fmt.Errorf("validate bot token: %w", err)
	}

	caption, err := telegram.ParseCaptionTemplate(cfg.Telegram.CaptionTemplate)
	if err != nil {
		return fmt.Errorf("parse caption template: %w", err)
	}

	tg := telegram.NewHTTPClient(botToken, cfg.Telegram.APIBaseURL, &http.Client{Timeout: cfg.Telegram.Timeout})
	objectStore, err := newObjectStore(meta, tg, store.Options{
		Upload: store.UploadConfig{
			Strategy:       cfg.Storage.UploadTypeStrategy,
			EnableChunking: *cfg.Storage.EnableChunking,
			MaxFileSize:    cfg.Storage.MaxFileSize,
			ChunkSize:      cfg.Storage.ChunkSize,
			TypeLimits:     cfg.Storage.TypeSizeLimits,
			PutBufferSize:  cfg.Storage.PutBufferSize,
		},
		Caption:          caption,
		MaxUploads:       cfg.Storage.MaxConcurrentUploads,
		MaxDownloads:     cfg.Storage.MaxConcurrentDownloads,
		MaxTelegramCalls: cfg.Storage.MaxConcurrentTelegramRequests,
	})
	if err != nil {
		return fmt.Errorf("create object store: %w", err)
	}

	secrets := map[string]string{}
	for _, credential := range cfg.Auth.Credentials {
		secret := cfg.ResolveSecret(credential.SecretKeyEnv)
		if strings.TrimSpace(secret) == "" {
			return fmt.Errorf("resolve secret for access key %s: environment variable %s is empty", credential.AccessKey, credential.SecretKeyEnv)
		}
		secrets[credential.AccessKey] = secret
	}

	ready.Store(true)

	server := s3api.NewServer(objectStore, s3api.Options{
		Region:      cfg.Auth.Region,
		Credentials: secrets,
		Ready:       ready.Load,
	})

	listenAddr := cfg.ResolveListen()
	log.Printf("listening on %s", listenAddr)
	if err := listenAndServe(listenAddr, server); err != nil {
		return err
	}
	return nil
}
