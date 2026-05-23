package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aahl/tgs3/config"
	"github.com/aahl/tgs3/internal/s3api"
	"github.com/aahl/tgs3/metadata"
	"github.com/aahl/tgs3/store"
	"github.com/aahl/tgs3/telegram"
	"gopkg.in/yaml.v3"
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

const defaultConfigPath = "data/config.yaml"

const topLevelUsage = "Usage:\n" +
	"  tgs3 [-debug] [-c|-config config.yaml]\n" +
	"  tgs3 [-debug] [-c|-config config.yaml] ls [-n|-limit N] bucket[/prefix]\n" +
	"  tgs3 [-debug] [-c|-config config.yaml] lsd [bucket[/prefix]]\n"

var newObjectStore = store.NewObjectStore
var listenAndServe = http.ListenAndServe

var errHelpRequested = errors.New("help requested")

type debugLogger struct {
	enabled bool
	logger  *log.Logger
}

func newDebugLogger(enabled bool, stderr io.Writer) debugLogger {
	if stderr == nil {
		stderr = io.Discard
	}
	return debugLogger{enabled: enabled, logger: log.New(stderr, "", 0)}
}

func (d debugLogger) Printf(format string, args ...any) {
	if !d.enabled {
		return
	}
	d.logger.Printf("debug "+format, args...)
}

func (d debugLogger) StdLogger() *log.Logger {
	if d.enabled {
		return d.logger
	}
	return log.New(io.Discard, "", 0)
}

func main() {
	if err := runMain(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, errHelpRequested) {
			return
		}
		log.Fatal(err)
	}
}

func runMain(args []string, stdout, stderr io.Writer) error {
	configPath, debug, rest, err := parseGlobalFlags(args, stderr)
	if err != nil {
		if errors.Is(err, errHelpRequested) {
			return nil
		}
		return err
	}
	dbg := newDebugLogger(debug, stderr)
	if debug {
		dbg.Printf("config_path=%q", configPath)
	}
	if len(rest) == 0 {
		dbg.Printf("mode=service")
		return runWithDebug(configPath, dbg)
	}

	switch rest[0] {
	case "ls":
		dbg.Printf("mode=ls")
		return runLS(configPath, rest[1:], stdout, stderr, dbg)
	case "lsd":
		dbg.Printf("mode=lsd")
		return runLSD(configPath, rest[1:], stdout, stderr, dbg)
	default:
		return fmt.Errorf("unknown subcommand: %s", rest[0])
	}
}

func parseGlobalFlags(args []string, stderr io.Writer) (string, bool, []string, error) {
	fs := flag.NewFlagSet("tgs3", flag.ContinueOnError)
	if stderr == nil {
		stderr = io.Discard
	}
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, topLevelUsage)
	}

	configPath := fs.String("config", defaultConfigPath, "path to YAML config")
	shortConfigPath := fs.String("c", defaultConfigPath, "path to YAML config")
	debug := fs.Bool("debug", false, "enable debug logging to stderr")

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return "", false, nil, errHelpRequested
		}
		return "", false, nil, err
	}

	var configSet bool
	var shortConfigSet bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "config":
			configSet = true
		case "c":
			shortConfigSet = true
		}
	})

	if configSet && shortConfigSet {
		return "", false, nil, fmt.Errorf("-config and -c cannot both be set")
	}
	if shortConfigSet {
		return *shortConfigPath, *debug, fs.Args(), nil
	}
	return *configPath, *debug, fs.Args(), nil
}

const cliListPageSize = 1000

func runLS(configPath string, args []string, stdout, stderr io.Writer, dbg debugLogger) error {
	limit, path, err := parseLSFlags(args, stderr)
	if err != nil {
		if errors.Is(err, errHelpRequested) {
			return nil
		}
		return err
	}
	bucket, prefix, err := parseBucketPrefix(path)
	if err != nil {
		return err
	}
	meta, sqlitePath, err := openMetadataFromConfig(configPath)
	if err != nil {
		return err
	}
	dbg.Printf("sqlite_path=%q", sqlitePath)
	dbg.Printf("bucket=%q prefix=%q limit=%d", bucket, prefix, limit)
	defer meta.Close()
	ctx := context.Background()
	if err := requireEnabledBucket(ctx, meta, bucket); err != nil {
		return err
	}
	return writeObjectKeys(ctx, meta, bucket, prefix, limit, stdout, dbg)
}

func parseLSFlags(args []string, output io.Writer) (int, string, error) {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	if output == nil {
		output = io.Discard
	}
	fs.SetOutput(output)

	limitFlag := fs.Int("limit", 1000, "maximum number of objects to list")
	shortLimitFlag := fs.Int("n", 1000, "maximum number of objects to list")

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0, "", errHelpRequested
		}
		return 0, "", err
	}

	var limitSet bool
	var shortLimitSet bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "limit":
			limitSet = true
		case "n":
			shortLimitSet = true
		}
	})

	if limitSet && shortLimitSet {
		return 0, "", fmt.Errorf("-limit and -n cannot both be set")
	}

	selectedLimit := *limitFlag
	if shortLimitSet {
		selectedLimit = *shortLimitFlag
	}
	if selectedLimit < 0 {
		return 0, "", fmt.Errorf("limit must be non-negative")
	}
	if len(fs.Args()) != 1 {
		return 0, "", fmt.Errorf("ls requires exactly one bucket/prefix path")
	}

	return selectedLimit, fs.Arg(0), nil
}

func writeObjectKeys(ctx context.Context, meta metadata.Store, bucket, prefix string, limit int, stdout io.Writer, dbg debugLogger) error {
	afterKey := ""
	written := 0
	for {
		pageLimit := cliListPageSize
		if limit > 0 {
			remaining := limit - written
			if remaining <= 0 {
				return nil
			}
			if remaining < pageLimit {
				pageLimit = remaining
			}
		}
		dbg.Printf("page_after=%q page_limit=%d", afterKey, pageLimit)
		objects, err := meta.ListObjects(ctx, metadata.ListQuery{Bucket: bucket, Prefix: prefix, AfterKey: afterKey, Limit: pageLimit})
		if err != nil {
			return err
		}
		dbg.Printf("page_after=%q rows=%d", afterKey, len(objects))
		if len(objects) == 0 {
			return nil
		}
		for _, object := range objects {
			if _, err := fmt.Fprintln(stdout, object.Key); err != nil {
				return err
			}
			written++
			afterKey = object.Key
			if limit > 0 && written >= limit {
				return nil
			}
		}
		if len(objects) < pageLimit {
			return nil
		}
	}
}

func runLSD(configPath string, args []string, stdout, stderr io.Writer, dbg debugLogger) error {
	// lsd does not use a FlagSet because it accepts no flags at all.
	// Handle -h/-help explicitly so users get a usage hint instead of
	// the generic "lsd does not support flags" error.
	if len(args) == 1 && (args[0] == "-h" || args[0] == "-help") {
		_, err := fmt.Fprintln(stderr, "Usage of lsd:\n  lsd [bucket[/prefix]]")
		return err
	}
	if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("lsd does not support flags")
	}
	if len(args) > 1 {
		return fmt.Errorf("lsd accepts at most one bucket/prefix path")
	}
	var bucket, prefix string
	if len(args) == 1 {
		var err error
		bucket, prefix, err = parseBucketPrefix(args[0])
		if err != nil {
			return err
		}
	}
	meta, sqlitePath, err := openMetadataFromConfig(configPath)
	if err != nil {
		return err
	}
	defer meta.Close()
	dbg.Printf("sqlite_path=%q", sqlitePath)
	ctx := context.Background()
	if len(args) == 0 {
		return writeBucketNames(ctx, meta, stdout)
	}
	dbg.Printf("bucket=%q prefix=%q", bucket, prefix)
	if err := requireEnabledBucket(ctx, meta, bucket); err != nil {
		return err
	}
	return writeCommonPrefixes(ctx, meta, bucket, prefix, stdout, dbg)
}

func writeBucketNames(ctx context.Context, meta metadata.Store, stdout io.Writer) error {
	buckets, err := meta.ListBuckets(ctx)
	if err != nil {
		return err
	}
	for _, bucket := range buckets {
		if _, err := fmt.Fprintln(stdout, bucket.Name); err != nil {
			return err
		}
	}
	return nil
}

func writeCommonPrefixes(ctx context.Context, meta metadata.Store, bucket, prefix string, stdout io.Writer, dbg debugLogger) error {
	afterKey := ""
	seen := map[string]struct{}{}
	for {
		dbg.Printf("page_after=%q page_limit=%d", afterKey, cliListPageSize)
		objects, err := meta.ListObjects(ctx, metadata.ListQuery{Bucket: bucket, Prefix: prefix, AfterKey: afterKey, Limit: cliListPageSize})
		if err != nil {
			return err
		}
		dbg.Printf("page_after=%q rows=%d", afterKey, len(objects))
		if len(objects) == 0 {
			return nil
		}
		for _, object := range objects {
			afterKey = object.Key
			remainder := strings.TrimPrefix(object.Key, prefix)
			segment, _, found := strings.Cut(remainder, "/")
			if !found || segment == "" {
				continue
			}
			commonPrefix := prefix + segment + "/"
			if _, ok := seen[commonPrefix]; ok {
				continue
			}
			seen[commonPrefix] = struct{}{}
			if _, err := fmt.Fprintln(stdout, commonPrefix); err != nil {
				return err
			}
		}
		if len(objects) < cliListPageSize {
			return nil
		}
	}
}

func openMetadataFromConfig(configPath string) (metadata.Store, string, error) {
	sqlitePath, err := loadSQLitePathForLocalCLI(configPath)
	if err != nil {
		return nil, "", err
	}
	meta, err := metadata.OpenSQLiteReadOnly(sqlitePath)
	if err != nil {
		return nil, "", fmt.Errorf("open sqlite metadata: %w", err)
	}
	return meta, sqlitePath, nil
}

func loadSQLitePathForLocalCLI(configPath string) (string, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", fmt.Errorf("load config: %w", err)
	}
	var cfg config.Config
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return "", fmt.Errorf("parse config: %w", err)
	}
	if cfg.Metadata.Driver == "" {
		cfg.Metadata.Driver = "sqlite"
	}
	if cfg.Metadata.SQLitePath == "" {
		cfg.Metadata.SQLitePath = "data/metadata.sqlite"
	}
	if strings.TrimSpace(cfg.Metadata.Driver) != "sqlite" {
		return "", fmt.Errorf("metadata driver must be sqlite")
	}
	sqlitePath, err := cfg.ResolveSQLitePath()
	if err != nil {
		return "", fmt.Errorf("resolve sqlite path: %w", err)
	}
	if strings.TrimSpace(sqlitePath) == "" {
		return "", fmt.Errorf("metadata sqlite path is required")
	}
	return sqlitePath, nil
}

func parseBucketPrefix(value string) (string, string, error) {
	if value == "" {
		return "", "", fmt.Errorf("path is required")
	}
	bucket, prefix, found := strings.Cut(value, "/")
	if bucket == "" {
		return "", "", fmt.Errorf("bucket is required")
	}
	if !found {
		return bucket, "", nil
	}
	return bucket, prefix, nil
}

func requireEnabledBucket(ctx context.Context, meta metadata.Store, bucket string) error {
	found, err := meta.GetBucket(ctx, bucket)
	if err != nil {
		if err == metadata.ErrNotFound {
			return fmt.Errorf("bucket not found: %s", bucket)
		}
		return err
	}
	if !found.Enabled {
		return fmt.Errorf("bucket not found: %s", bucket)
	}
	return nil
}

func run(configPath string) error {
	return runWithDebug(configPath, newDebugLogger(false, io.Discard))
}

func runWithDebug(configPath string, dbg debugLogger) error {
	var ready atomic.Bool

	cfg, err := config.LoadFile(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	sqlitePath, err := cfg.ResolveSQLitePath()
	if err != nil {
		return fmt.Errorf("resolve sqlite path: %w", err)
	}
	dbg.Printf("sqlite_path=%q", sqlitePath)

	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		return fmt.Errorf("open sqlite metadata: %w", err)
	}
	defer meta.Close()

	ctx := context.Background()
	for name, bucket := range cfg.Buckets {
		dbg.Printf("bucket=%q upsert=configured", name)
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
		Logger:           dbg.StdLogger(),
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
		Logger:      dbg.StdLogger(),
	})

	listenAddr := cfg.ResolveListen()
	dbg.Printf("listen_addr=%q", listenAddr)
	log.Printf("listening on %s", listenAddr)
	if err := listenAndServe(listenAddr, server); err != nil {
		return err
	}
	return nil
}
