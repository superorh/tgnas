package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aahl/tgnas/config"
	"github.com/aahl/tgnas/internal/dav"
	"github.com/aahl/tgnas/internal/s3api"
	"github.com/aahl/tgnas/metadata"
	"github.com/aahl/tgnas/store"
	"github.com/aahl/tgnas/telegram"
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

type serverMode string

const (
	serverModeAll serverMode = "all"
	serverModeS3  serverMode = "s3"
	serverModeDAV serverMode = "dav"
)

const defaultConfigPath = "data/config.yaml"

const topLevelUsage = "Usage:\n" +
	"  tgnas [-debug] [-c|-config config.yaml]\n" +
	"  tgnas [-debug] [-c|-config config.yaml] s3\n" +
	"  tgnas [-debug] [-c|-config config.yaml] dav\n" +
	"  tgnas [-debug] [-c|-config config.yaml] ls [-n|-limit N] bucket[/prefix]\n" +
	"  tgnas [-debug] [-c|-config config.yaml] lsd [bucket[/prefix]]\n" +
	"  tgnas [-debug] [-c|-config config.yaml] bucket rename [--dry-run] old-bucket new-bucket\n"

var newObjectStore = store.NewObjectStore
var listenAndServe = http.ListenAndServe
var runServiceFunc = runServiceWithDebug

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
		dbg.Printf("mode=all")
		return runServiceFunc(configPath, serverModeAll, dbg)
	}

	switch rest[0] {
	case "s3":
		dbg.Printf("mode=s3")
		return runServiceFunc(configPath, serverModeS3, dbg)
	case "dav":
		dbg.Printf("mode=dav")
		return runServiceFunc(configPath, serverModeDAV, dbg)
	case "ls":
		dbg.Printf("mode=ls")
		return runLS(configPath, rest[1:], stdout, stderr, dbg)
	case "lsd":
		dbg.Printf("mode=lsd")
		return runLSD(configPath, rest[1:], stdout, stderr, dbg)
	case "bucket":
		dbg.Printf("mode=bucket")
		return runBucketCommand(configPath, rest[1:], stdout, stderr, dbg)
	default:
		return fmt.Errorf("unknown subcommand: %s", rest[0])
	}
}

func parseGlobalFlags(args []string, stderr io.Writer) (string, bool, []string, error) {
	fs := flag.NewFlagSet("tgnas", flag.ContinueOnError)
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

func runBucketCommand(configPath string, args []string, stdout, stderr io.Writer, dbg debugLogger) error {
	if len(args) == 0 {
		return fmt.Errorf("bucket subcommand is required")
	}
	switch args[0] {
	case "rename":
		return runRenameBucket(configPath, args[1:], stdout, stderr, dbg)
	default:
		return fmt.Errorf("unknown bucket subcommand: %s", args[0])
	}
}

func parseRenameBucketFlags(args []string, output io.Writer) (bool, string, string, error) {
	fs := flag.NewFlagSet("bucket rename", flag.ContinueOnError)
	if output == nil {
		output = io.Discard
	}
	fs.SetOutput(output)
	dryRun := fs.Bool("dry-run", false, "print what would change without modifying data")
	if err := fs.Parse(args); err != nil {
		return false, "", "", err
	}
	rest := fs.Args()
	if len(rest) != 2 {
		return false, "", "", fmt.Errorf("usage: bucket rename [--dry-run] old-bucket new-bucket")
	}
	return *dryRun, rest[0], rest[1], nil
}

func runRenameBucket(configPath string, args []string, stdout, stderr io.Writer, dbg debugLogger) error {
	dryRun, oldName, newName, err := parseRenameBucketFlags(args, stderr)
	if err != nil {
		return err
	}

	cfg, err := config.LoadFile(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	targetBucket, ok := cfg.Buckets[newName]
	if !ok {
		return fmt.Errorf("target bucket is not configured: %s", newName)
	}

	if _, oldStillConfigured := cfg.Buckets[oldName]; oldStillConfigured {
		fmt.Fprintf(stderr, "warning: source bucket still exists in config: %s\n", oldName)
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
	sourceBucket, err := meta.GetBucket(ctx, oldName)
	if err != nil {
		if err == metadata.ErrNotFound {
			return fmt.Errorf("source bucket not found: %s", oldName)
		}
		return err
	}

	if targetBucket.ChatID != sourceBucket.ChatID {
		return fmt.Errorf("target bucket chat_id differs from source bucket metadata: %s", newName)
	}

	if dryRun {
		counts, err := meta.CountBucketRenameRows(ctx, oldName)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "would rename bucket %s to %s: buckets=%d objects=%d chunks=%d\n", oldName, newName, counts.Buckets, counts.Objects, counts.Chunks)
		return nil
	}

	counts, err := meta.RenameBucket(ctx, oldName, newName)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "renamed bucket %s to %s: buckets=%d objects=%d chunks=%d\n", oldName, newName, counts.Buckets, counts.Objects, counts.Chunks)
	return nil
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
	return runServiceWithDebug(configPath, serverModeAll, newDebugLogger(false, io.Discard))
}

func publicReadBucketsFromConfig(cfg config.Config) map[string]bool {
	publicReadBuckets := map[string]bool{}
	for name, bucket := range cfg.Buckets {
		if bucket.PublicRead {
			publicReadBuckets[name] = true
		}
	}
	return publicReadBuckets
}

type trustedProxyMiddleware struct {
	next   http.Handler
	trust  trustedProxyTrust
	logger *log.Logger
}

type trustedProxyTrust struct {
	proxies []netip.Prefix
	hosts   map[string]struct{}
}

func newTrustedProxyMiddleware(next http.Handler, server config.ServerConfig) (http.Handler, error) {
	return newTrustedProxyMiddlewareWithLogger(next, server, nil)
}

func newTrustedProxyMiddlewareWithLogger(next http.Handler, server config.ServerConfig, logger *log.Logger) (http.Handler, error) {
	trust, err := newTrustedProxyTrust(server)
	if err != nil {
		return nil, err
	}
	if len(trust.proxies) == 0 && len(trust.hosts) == 0 {
		return next, nil
	}
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return trustedProxyMiddleware{next: next, trust: trust, logger: logger}, nil
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

func (m trustedProxyMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	forwardedHost := forwardedHost(r)
	forwardedProto := forwardedProto(r)
	if forwardedHost == "" && forwardedProto == "" {
		m.next.ServeHTTP(w, r)
		return
	}
	trusted := m.trust.trusts(r, forwardedHost)
	originalHost := r.Host
	originalScheme := r.URL.Scheme
	clone := r.Clone(r.Context())
	if !trusted {
		m.logger.Printf("debug event=trusted_proxy remote_addr=%q original_host=%q original_scheme=%q forwarded_host=%q forwarded_proto=%q trusted=false", r.RemoteAddr, originalHost, originalScheme, forwardedHost, forwardedProto)
		m.next.ServeHTTP(w, clone)
		return
	}

	if forwardedHost != "" {
		normalized := normalizeForwardedHost(forwardedHost)
		clone.Host = normalized
		clone.URL.Host = normalized
	}
	if forwardedProto != "" {
		clone.URL.Scheme = strings.ToLower(forwardedProto)
	}
	m.logger.Printf("debug event=trusted_proxy remote_addr=%q original_host=%q original_scheme=%q forwarded_host=%q forwarded_proto=%q trusted=true rewritten_host=%q rewritten_scheme=%q", r.RemoteAddr, originalHost, originalScheme, forwardedHost, forwardedProto, clone.Host, clone.URL.Scheme)
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

type combinedHandler struct {
	s3      http.Handler
	dav     http.Handler
	prefix  string
	davOnly bool
}

func newCombinedHandler(s3Handler, davHandler http.Handler, prefix string) http.Handler {
	return &combinedHandler{s3: s3Handler, dav: davHandler, prefix: prefix}
}

func newDAVOnlyHandler(s3Handler, davHandler http.Handler, prefix string) http.Handler {
	return &combinedHandler{s3: s3Handler, dav: davHandler, prefix: prefix, davOnly: true}
}

func (h *combinedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
		h.s3.ServeHTTP(w, r)
		return
	}
	if r.URL.Path == strings.TrimSuffix(h.prefix, "/") {
		http.Redirect(w, r, h.prefix, http.StatusPermanentRedirect)
		return
	}
	if strings.HasPrefix(r.URL.Path, h.prefix) {
		h.dav.ServeHTTP(w, r)
		return
	}
	if h.davOnly {
		http.NotFound(w, r)
		return
	}
	h.s3.ServeHTTP(w, r)
}

func runServiceWithDebug(configPath string, mode serverMode, dbg debugLogger) error {
	var ready atomic.Bool

	cfg, err := config.LoadFile(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	botToken := cfg.ResolveBotToken()
	if err := validateBotToken(botToken); err != nil {
		return fmt.Errorf("validate bot token: %w", err)
	}

	caption, err := telegram.ParseCaptionTemplate(cfg.Telegram.CaptionTemplate)
	if err != nil {
		return fmt.Errorf("parse caption template: %w", err)
	}

	secrets := map[string]string{}
	for _, credential := range cfg.Auth.Credentials {
		secret := cfg.ResolveSecret(credential.SecretKeyEnv)
		if strings.TrimSpace(secret) == "" {
			return fmt.Errorf("resolve secret for access key %s: environment variable %s is empty", credential.AccessKey, credential.SecretKeyEnv)
		}
		secrets[credential.AccessKey] = secret
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
	var configuredNames []string
	for name, bucket := range cfg.Buckets {
		dbg.Printf("bucket=%q upsert=configured", name)
		configuredNames = append(configuredNames, name)
		if err := meta.UpsertBucket(ctx, metadata.Bucket{
			Name:      name,
			ChatID:    bucket.ChatID,
			CreatedAt: time.Now().UTC(),
			Enabled:   true,
		}); err != nil {
			return fmt.Errorf("upsert bucket %s: %w", name, err)
		}
	}
	if err := meta.DisableBucketsExcept(ctx, configuredNames); err != nil {
		return fmt.Errorf("disable removed buckets: %w", err)
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

	ready.Store(true)

	var handler http.Handler
	s3Handler := s3api.NewServer(objectStore, s3api.Options{
		Region:            cfg.Auth.Region,
		Credentials:       secrets,
		PublicReadBuckets: publicReadBucketsFromConfig(cfg),
		Ready:             ready.Load,
		Logger:            dbg.StdLogger(),
	})
	if mode == serverModeS3 {
		handler = s3Handler
	} else {
		fs := dav.NewFileSystem(meta, objectStore)
		davHandler := dav.NewHandler(meta, fs, dav.HandlerOptions{
			Prefix:      cfg.WebDAV.Prefix,
			Credentials: secrets,
			Logger:      dbg.StdLogger(),
		})
		switch mode {
		case serverModeAll:
			handler = newCombinedHandler(s3Handler, davHandler, cfg.WebDAV.Prefix)
		case serverModeDAV:
			handler = newDAVOnlyHandler(s3Handler, davHandler, cfg.WebDAV.Prefix)
		default:
			return fmt.Errorf("unknown server mode: %s", mode)
		}
	}

	handler, err = newTrustedProxyMiddlewareWithLogger(handler, cfg.Server, dbg.StdLogger())
	if err != nil {
		return fmt.Errorf("configure trusted proxy middleware: %w", err)
	}

	listenAddr := cfg.ResolveListen()
	dbg.Printf("listen_addr=%q mode=%q webdav_prefix=%q", listenAddr, mode, cfg.WebDAV.Prefix)
	log.Printf("listening on %s (mode=%s)", listenAddr, mode)
	if err := listenAndServe(listenAddr, handler); err != nil {
		return err
	}
	return nil
}
