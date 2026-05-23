# tgs3 Local ls/lsd CLI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add local SQLite-backed `ls` and `lsd` subcommands to `tgs3` while preserving the existing default server startup behavior.

**Architecture:** Keep `cmd/tgs3/main.go` as the single command entry point, but add a testable `runMain(args, stdout, stderr)` dispatcher. By default, service and local CLI commands read `data/config.yaml`, and the config falls back to `data/metadata.sqlite` for SQLite metadata plus `us-east-1` for SigV4 region. Local listing commands strictly parse only the metadata config needed to resolve SQLite, open SQLite metadata read-only, and page through `metadata.Store` directly, avoiding full service validation, Telegram, HTTP server startup, database creation, migrations, and metadata mutation.

**Tech Stack:** Go standard `flag`, `io`, `os`, `strings`, `context`, `net/url`; existing `config` and `metadata` packages; writable SQLite metadata backend through `metadata.OpenSQLite` for service/test seeding and read-only CLI metadata backend through `metadata.OpenSQLiteReadOnly`.

---

## File Structure

- Modify: `cmd/tgs3/main.go`
  - Add CLI dispatch, global `-config`/`-c` parsing, `ls`/`lsd` parsing, local read-only metadata opener, paged listing helpers, and bucket validation helper.
  - Add global `-debug` flag parsing and stderr-backed debug logging.
  - Keep `run(configPath string) error` as the server startup path.
  - Use `data/config.yaml` in the current working directory as the default config path.
- Modify: `cmd/tgs3/main_test.go`
  - Add unit/integration-style tests for dispatch, parsing, local SQLite listing, limits, common prefixes, and bucket errors.
  - Reuse existing `writeConfig` helper, extending it only if needed.
- Modify: `config/config.go`
  - Add config defaults for `auth.region = "us-east-1"` and `metadata.sqlite_path = "data/metadata.sqlite"`.
- Modify: `config/config_test.go`
  - Add coverage that a minimal config can omit `auth.region` and `metadata.sqlite_path`.
- Modify: `metadata/sqlite.go`
  - Add `OpenSQLiteReadOnly` and shared SQLite opening helpers so CLI listing can inspect existing metadata without creating or migrating the database.
- Modify: `metadata/sqlite_test.go`
  - Add coverage that read-only SQLite opening does not create a missing database file.
- Create: `data/config.yaml`
  - Create the `data/` directory if it does not exist, then add the default config template from the design: required active auth/Telegram/bucket values, with optional settings commented and defaults visible.
- Modify: `README.md`
  - Add a short CLI section documenting service mode, concise usage, default `data/config.yaml`, `-c`, `ls`, `-n`, `-n 0`, and `lsd`.

---

### Task 1: Add config defaults for local data paths

**Files:**
- Modify: `config/config.go`
- Test: `config/config_test.go`

- [ ] **Step 1: Write failing test for minimal config defaults**

Add this test to `config/config_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify failure**

Run:

```bash
go test ./config
```

Expected: FAIL because `metadata.sqlite_path` is still required or missing a default; this also guards the `auth.region` default.

- [ ] **Step 3: Implement config defaults**

In `config/config.go`, initialize defaults in `LoadFile`:

```go
cfg := Config{
	Server: ServerConfig{Listen: ":9000", ListenEnv: "TGS3_LISTEN"},
	Auth:   AuthConfig{Region: "us-east-1"},
	Telegram: TelegramConfig{
		APIBaseURL: "https://api.telegram.org",
		Timeout:    30 * time.Second,
	},
	Metadata: MetadataConfig{Driver: "sqlite", SQLitePath: "data/metadata.sqlite"},
	Storage:  DefaultStorageConfig(),
	Buckets:  map[string]BucketConfig{},
}
```

- [ ] **Step 4: Run tests to verify pass**

Run:

```bash
go test ./config
```

Expected: PASS.

- [ ] **Step 4b: Add failing test for `${ENV_NAME}` bucket chat_id resolution**

Add these tests to `config/config_test.go`:

```go
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
    chat_id: "${TGS3_PHOTOS_CHAT_ID}"
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TGS3_PHOTOS_CHAT_ID", "-100999")

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
    chat_id: "prefix-${TGS3_PHOTOS_CHAT_ID}"
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TGS3_PHOTOS_CHAT_ID", "123")

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
    chat_id: "${TGS3_MISSING_CHAT_ID}"
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TGS3_MISSING_CHAT_ID", "")

	_, err = LoadFile(path)
	if err == nil {
		t.Fatal("expected empty chat id validation error")
	}
}
```

Run:

```bash
go test ./config
```

Expected: FAIL because `LoadFile` still returns the literal `${TGS3_PHOTOS_CHAT_ID}` and does not reject partial interpolation.

- [ ] **Step 4c: Implement `${ENV_NAME}` chat_id resolution and validation**

In `config/config.go`, resolve bucket chat_id references before `Validate()`:

```go
for name, bucket := range cfg.Buckets {
	chatID, err := resolveBucketChatID(bucket.ChatID)
	if err != nil {
		return Config{}, fmt.Errorf("resolve bucket %q chat_id: %w", name, err)
	}
	bucket.ChatID = chatID
	cfg.Buckets[name] = bucket
}
```

Add this helper below `applyStorageDefaults`:

```go
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
```

Run:

```bash
go test ./config
```

Expected: PASS.

- [ ] **Step 5: Record changed files without committing**

```bash
git diff -- config/config.go config/config_test.go
```

Expected: diff only contains config default changes and tests.

---

### Task 2: Add top-level CLI dispatch and `-c` alias

**Files:**
- Modify: `cmd/tgs3/main.go:3-41`
- Test: `cmd/tgs3/main_test.go`

- [ ] **Step 1: Write failing tests for global parsing, default config path, and service dispatch**

Add these tests to `cmd/tgs3/main_test.go` after `TestRunReturnsObjectStoreCreationFailure`:

```go
func TestParseGlobalFlagsDefaultsToDataConfig(t *testing.T) {
	configPath, debug, rest, err := parseGlobalFlags(nil, io.Discard)
	if err != nil {
		t.Fatalf("parseGlobalFlags returned error: %v", err)
	}
	if configPath != "data/config.yaml" {
		t.Fatalf("configPath = %q", configPath)
	}
	if debug {
		t.Fatal("debug = true, want false")
	}
	if len(rest) != 0 {
		t.Fatalf("rest = %v", rest)
	}
}

func TestParseGlobalFlagsAcceptsConfigAliases(t *testing.T) {
	configPath, debug, rest, err := parseGlobalFlags([]string{"-config", "config-a.yaml", "ls", "photos"}, io.Discard)
	if err != nil {
		t.Fatalf("parseGlobalFlags returned error: %v", err)
	}
	if configPath != "config-a.yaml" {
		t.Fatalf("configPath = %q", configPath)
	}
	if debug {
		t.Fatal("debug = true, want false")
	}
	if strings.Join(rest, " ") != "ls photos" {
		t.Fatalf("rest = %v", rest)
	}

	configPath, debug, rest, err = parseGlobalFlags([]string{"-c", "config-b.yaml", "lsd"}, io.Discard)
	if err != nil {
		t.Fatalf("parseGlobalFlags returned error: %v", err)
	}
	if configPath != "config-b.yaml" {
		t.Fatalf("configPath = %q", configPath)
	}
	if debug {
		t.Fatal("debug = true, want false")
	}
	if strings.Join(rest, " ") != "lsd" {
		t.Fatalf("rest = %v", rest)
	}
}

func TestRunMainStartsServerWithoutSubcommand(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "123456:valid-token")
	t.Setenv("TGS3_SECRET_KEY", "secret")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, configPath, filepath.Join(t.TempDir(), "metadata.sqlite"))

	oldListenAndServe := listenAndServe
	listenAndServe = func(addr string, handler http.Handler) error {
		if addr != "127.0.0.1:0" {
			t.Fatalf("addr = %q, want 127.0.0.1:0", addr)
		}
		return errors.New("server stopped")
	}
	t.Cleanup(func() { listenAndServe = oldListenAndServe })

	err := runMain([]string{"-c", configPath}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "server stopped") {
		t.Fatalf("err = %v, want server stopped", err)
	}
}

func TestRunMainRejectsBothConfigAliases(t *testing.T) {
	err := runMain([]string{"-config", "a.yaml", "-c", "b.yaml"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "-config and -c cannot both be set") {
		t.Fatalf("err = %v", err)
	}
}
```

Also add `io` to the test imports:

```go
import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aahl/tgs3/metadata"
	"github.com/aahl/tgs3/store"
	"github.com/aahl/tgs3/telegram"
)
```

- [ ] **Step 2: Run tests to verify failure**

Run:

```bash
go test ./cmd/tgs3
```

Expected: FAIL because `runMain` is undefined.

- [ ] **Step 3: Implement top-level dispatcher**

In `cmd/tgs3/main.go`, update imports from:

```go
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
```

to:

```go
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
```

Replace `main()` with:

```go
func main() {
	if err := runMain(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, errHelpRequested) {
			return
		}
		log.Fatal(err)
	}
}
```

Add these helpers below `main()` and above `run(configPath string) error`:

```go
const defaultConfigPath = "data/config.yaml"

const topLevelUsage = "Usage:\n" +
	"  tgs3 [-debug] [-c|-config config.yaml]\n" +
	"  tgs3 [-debug] [-c|-config config.yaml] ls [-n|-limit N] bucket[/prefix]\n" +
	"  tgs3 [-debug] [-c|-config config.yaml] lsd [bucket[/prefix]]\n"

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
		dbg.Printf("config_path=%s", configPath)
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
```

Add temporary stubs below `parseGlobalFlags` so Task 2 compiles before later tasks implement the commands:

```go
func runLS(configPath string, args []string, stdout, stderr io.Writer, dbg debugLogger) error {
	return fmt.Errorf("ls not implemented")
}

func runLSD(configPath string, args []string, stdout, stderr io.Writer, dbg debugLogger) error {
	return fmt.Errorf("lsd not implemented")
}
```

- [ ] **Step 4: Run tests to verify pass**

Run:

```bash
go test ./cmd/tgs3
```

Expected: PASS.

- [ ] **Step 5: Record changed files without committing**

```bash
git diff -- cmd/tgs3/main.go cmd/tgs3/main_test.go
```

Expected: diff only contains the dispatch and alias changes for this task.

---

### Task 3: Add shared read-only metadata opener and bucket/path parsing

**Files:**
- Modify: `cmd/tgs3/main.go`
- Modify: `metadata/sqlite.go`
- Test: `cmd/tgs3/main_test.go`
- Test: `metadata/sqlite_test.go`

- [ ] **Step 1: Write failing tests for path parsing, local metadata opening, and missing read-only databases**

Add these tests to `cmd/tgs3/main_test.go`:

```go
func TestParseBucketPrefix(t *testing.T) {
	testCases := []struct {
		name       string
		input      string
		wantBucket string
		wantPrefix string
		wantErr    bool
	}{
		{name: "bucket only", input: "photos", wantBucket: "photos", wantPrefix: ""},
		{name: "bucket prefix", input: "photos/2026/", wantBucket: "photos", wantPrefix: "2026/"},
		{name: "empty", input: "", wantErr: true},
		{name: "empty bucket", input: "/2026/", wantErr: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			bucket, prefix, err := parseBucketPrefix(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBucketPrefix returned error: %v", err)
			}
			if bucket != tc.wantBucket || prefix != tc.wantPrefix {
				t.Fatalf("bucket=%q prefix=%q, want bucket=%q prefix=%q", bucket, prefix, tc.wantBucket, tc.wantPrefix)
			}
		})
	}
}

func TestOpenMetadataFromConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	writeConfig(t, configPath, sqlitePath)

	writableMeta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	if err := writableMeta.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	meta, sqlitePath, err := openMetadataFromConfig(configPath)
	if err != nil {
		t.Fatalf("openMetadataFromConfig returned error: %v", err)
	}
	defer meta.Close()
	if sqlitePath == "" {
		t.Fatal("sqlitePath = empty")
	}
}
```

Add this test to `metadata/sqlite_test.go`:

```go
func TestOpenSQLiteReadOnlyDoesNotCreateMissingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.sqlite")
	store, err := OpenSQLiteReadOnly(path)
	if err == nil {
		_ = store.Close()
		t.Fatal("expected OpenSQLiteReadOnly error")
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("sqlite path was created or stat failed unexpectedly: %v", statErr)
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

Run:

```bash
go test ./cmd/tgs3 ./metadata
```

Expected: FAIL because `parseBucketPrefix`, `openMetadataFromConfig`, and `OpenSQLiteReadOnly` are undefined.

- [ ] **Step 3: Implement read-only SQLite opener**

In `metadata/sqlite.go`, add `net/url` to imports and refactor the opener functions to share `openSQLite`:

```go
func OpenSQLite(path string) (*SQLiteStore, error) {
	store, err := openSQLite(path)
	if err != nil {
		return nil, err
	}
	if err := store.migrate(); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func OpenSQLiteReadOnly(path string) (*SQLiteStore, error) {
	return openSQLite(sqliteReadOnlyDSN(path))
}

func openSQLite(dataSourceName string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", dataSourceName)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &SQLiteStore{db: db}, nil
}

func sqliteReadOnlyDSN(path string) string {
	u := url.URL{Scheme: "file", Path: path}
	query := u.Query()
	query.Set("mode", "ro")
	u.RawQuery = query.Encode()
	return u.String()
}
```

- [ ] **Step 4: Implement command helpers**

In `cmd/tgs3/main.go`, add below the temporary `runLSD` stub:

```go
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
```

- [ ] **Step 5: Run tests to verify pass**

Run:

```bash
go test ./cmd/tgs3 ./metadata
```

Expected: PASS.

- [ ] **Step 6: Record changed files without committing**

```bash
git diff -- cmd/tgs3/main.go cmd/tgs3/main_test.go metadata/sqlite.go metadata/sqlite_test.go
```

Expected: diff only contains metadata opener, path parsing, bucket validation, and read-only SQLite support for this task.

---

### Task 4: Implement `ls` argument parsing and object listing

**Files:**
- Modify: `cmd/tgs3/main.go`
- Test: `cmd/tgs3/main_test.go`

- [ ] **Step 1: Add test helpers for metadata fixtures**

Add these helpers to `cmd/tgs3/main_test.go` below `writeConfig`:

```go
func writeConfigWithPath(t *testing.T, sqlitePath string) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, configPath, sqlitePath)
	return configPath
}

func seedBucket(t *testing.T, meta metadata.Store, name string, enabled bool) {
	t.Helper()
	if err := meta.UpsertBucket(context.Background(), metadata.Bucket{Name: name, ChatID: "-100", CreatedAt: time.Now().UTC(), Enabled: enabled}); err != nil {
		t.Fatalf("UpsertBucket returned error: %v", err)
	}
}

func seedObject(t *testing.T, meta metadata.Store, bucket, key string) {
	t.Helper()
	object := metadata.Object{
		Bucket:         bucket,
		Key:            key,
		Size:           int64(len(key)),
		ContentType:    "text/plain",
		ETag:           key + "-etag",
		SHA256:         key + "-sha",
		LastModified:   time.Now().UTC(),
		ChunkCount:     1,
		TelegramType:   "document",
		UploadStrategy: "document",
	}
	chunk := metadata.Chunk{Bucket: bucket, Key: key, PartNumber: 1, Offset: 0, Size: object.Size, TelegramType: "document", TelegramFileID: key + "-file", TelegramMessageID: 1, TelegramFileUniqueID: key + "-unique", SHA256: object.SHA256}
	if err := meta.PutObject(context.Background(), object, []metadata.Chunk{chunk}); err != nil {
		t.Fatalf("PutObject(%s) returned error: %v", key, err)
	}
}
```

Also add `context`, `time`, and `metadata` to test imports:

```go
import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aahl/tgs3/metadata"
```

- [ ] **Step 2: Write failing tests for `ls` validation and behavior**

Add these tests to `cmd/tgs3/main_test.go`:

```go
func TestRunLSRejectsInvalidArguments(t *testing.T) {
	configPath := writeConfigWithPath(t, filepath.Join(t.TempDir(), "metadata.sqlite"))
	testCases := []struct {
		name string
		args []string
	}{
		{name: "missing path", args: nil},
		{name: "too many paths", args: []string{"photos", "extra"}},
		{name: "empty bucket", args: []string{"/prefix"}},
		{name: "negative limit", args: []string{"-limit", "-1", "photos"}},
		{name: "negative short limit", args: []string{"-n", "-1", "photos"}},
		{name: "both limits", args: []string{"-limit", "1", "-n", "2", "photos"}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := runLS(configPath, tc.args, io.Discard, io.Discard, newDebugLogger(false, io.Discard))
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestRunLSListsObjectKeysWithPrefixAndLimit(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	configPath := writeConfigWithPath(t, sqlitePath)
	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	seedBucket(t, meta, "photos", true)
	seedObject(t, meta, "photos", "2026/a.txt")
	seedObject(t, meta, "photos", "2026/b.txt")
	seedObject(t, meta, "photos", "2027/c.txt")

	var out strings.Builder
	if err := runLS(configPath, []string{"-n", "2", "photos/2026/"}, &out, io.Discard, newDebugLogger(false, io.Discard)); err != nil {
		t.Fatalf("runLS returned error: %v", err)
	}
	if out.String() != "2026/a.txt\n2026/b.txt\n" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestRunLSDefaultLimitIsOneThousand(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	configPath := writeConfigWithPath(t, sqlitePath)
	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	seedBucket(t, meta, "photos", true)
	for i := 0; i < 1005; i++ {
		seedObject(t, meta, "photos", fmt.Sprintf("k%04d", i))
	}

	var out strings.Builder
	if err := runLS(configPath, []string{"photos"}, &out, io.Discard, newDebugLogger(false, io.Discard)); err != nil {
		t.Fatalf("runLS returned error: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 1000 {
		t.Fatalf("line count = %d, want 1000", len(lines))
	}
}

func TestRunLSZeroLimitListsAllAcrossPages(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	configPath := writeConfigWithPath(t, sqlitePath)
	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	seedBucket(t, meta, "photos", true)
	for i := 0; i < 1005; i++ {
		seedObject(t, meta, "photos", fmt.Sprintf("k%04d", i))
	}

	var out strings.Builder
	if err := runLS(configPath, []string{"-n", "0", "photos"}, &out, io.Discard, newDebugLogger(false, io.Discard)); err != nil {
		t.Fatalf("runLS returned error: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 1005 {
		t.Fatalf("line count = %d, want 1005", len(lines))
	}
}

func TestRunLSRejectsMissingOrDisabledBucket(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	configPath := writeConfigWithPath(t, sqlitePath)
	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	seedBucket(t, meta, "disabled", false)

	err = runLS(configPath, []string{"missing"}, io.Discard, io.Discard, newDebugLogger(false, io.Discard))
	if err == nil || !strings.Contains(err.Error(), "bucket not found: missing") {
		t.Fatalf("err = %v", err)
	}
	err = runLS(configPath, []string{"disabled"}, io.Discard, io.Discard, newDebugLogger(false, io.Discard))
	if err == nil || !strings.Contains(err.Error(), "bucket not found: disabled") {
		t.Fatalf("err = %v", err)
	}
}
```

Add `fmt` to test imports:

```go
import (
	"context"
	"errors"
	"fmt"
	"io"
```

- [ ] **Step 3: Run tests to verify failure**

Run:

```bash
go test ./cmd/tgs3
```

Expected: FAIL because `runLS` still returns `ls not implemented` and helper imports/functions are missing before implementation.

- [ ] **Step 4: Implement `ls` parser and paged listing**

In `cmd/tgs3/main.go`, replace the temporary `runLS` stub with:

```go
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
	limit := fs.Int("limit", 1000, "maximum number of object keys to list; 0 means unlimited")
	shortLimit := fs.Int("n", 1000, "maximum number of object keys to list; 0 means unlimited")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0, "", errHelpRequested
		}
		return 0, "", err
	}
	seenLimit := false
	seenShortLimit := false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "limit":
			seenLimit = true
		case "n":
			seenShortLimit = true
		}
	})
	if seenLimit && seenShortLimit {
		return 0, "", fmt.Errorf("-limit and -n cannot both be set")
	}
	selectedLimit := *limit
	if seenShortLimit {
		selectedLimit = *shortLimit
	}
	if selectedLimit < 0 {
		return 0, "", fmt.Errorf("limit must be non-negative")
	}
	operands := fs.Args()
	if len(operands) != 1 {
		return 0, "", fmt.Errorf("ls requires exactly one bucket/prefix path")
	}
	return selectedLimit, operands[0], nil
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
		dbg.Printf("page_after=%s page_limit=%d", afterKey, pageLimit)
		objects, err := meta.ListObjects(ctx, metadata.ListQuery{Bucket: bucket, Prefix: prefix, AfterKey: afterKey, Limit: pageLimit})
		if err != nil {
			return err
		}
		dbg.Printf("page_after=%s rows=%d", afterKey, len(objects))
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
```

- [ ] **Step 5: Run tests to verify pass**

Run:

```bash
go test ./cmd/tgs3
```

Expected: PASS.

- [ ] **Step 6: Record changed files without committing**

```bash
git diff -- cmd/tgs3/main.go cmd/tgs3/main_test.go
```

Expected: diff only contains `ls` parsing, listing, and tests for this task.

---

### Task 5: Implement `lsd` bucket and common-prefix listing

**Files:**
- Modify: `cmd/tgs3/main.go`
- Test: `cmd/tgs3/main_test.go`

- [ ] **Step 1: Write failing tests for `lsd`**

Add these tests to `cmd/tgs3/main_test.go`:

```go
func TestRunLSDListsEnabledBuckets(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	configPath := writeConfigWithPath(t, sqlitePath)
	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	seedBucket(t, meta, "backups", true)
	seedBucket(t, meta, "disabled", false)
	seedBucket(t, meta, "photos", true)

	var out strings.Builder
	if err := runLSD(configPath, nil, &out, io.Discard, newDebugLogger(false, io.Discard)); err != nil {
		t.Fatalf("runLSD returned error: %v", err)
	}
	if out.String() != "backups\nphotos\n" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestRunLSDListsDirectCommonPrefixes(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	configPath := writeConfigWithPath(t, sqlitePath)
	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	seedBucket(t, meta, "photos", true)
	seedObject(t, meta, "photos", "2026/a.txt")
	seedObject(t, meta, "photos", "2026/jan/a.txt")
	seedObject(t, meta, "photos", "2026/jan/b.txt")
	seedObject(t, meta, "photos", "2026/feb/c.txt")
	seedObject(t, meta, "photos", "2027/root.txt")

	var out strings.Builder
	if err := runLSD(configPath, []string{"photos/2026/"}, &out, io.Discard, newDebugLogger(false, io.Discard)); err != nil {
		t.Fatalf("runLSD returned error: %v", err)
	}
	if out.String() != "2026/feb/\n2026/jan/\n" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestRunLSDRejectsInvalidArgumentsAndMissingBucket(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "metadata.sqlite")
	configPath := writeConfigWithPath(t, sqlitePath)
	meta, err := metadata.OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer meta.Close()
	seedBucket(t, meta, "disabled", false)

	if err := runLSD(configPath, []string{"photos", "extra"}, io.Discard, io.Discard, newDebugLogger(false, io.Discard)); err == nil {
		t.Fatal("expected too many args error")
	}
	if err := runLSD(configPath, []string{"-n", "1", "photos"}, io.Discard, io.Discard, newDebugLogger(false, io.Discard)); err == nil {
		t.Fatal("expected unsupported -n error")
	}
	if err := runLSD(configPath, []string{"-limit", "1", "photos"}, io.Discard, io.Discard, newDebugLogger(false, io.Discard)); err == nil {
		t.Fatal("expected unsupported -limit error")
	}
	if err := runLSD(configPath, []string{"/prefix"}, io.Discard, io.Discard, newDebugLogger(false, io.Discard)); err == nil {
		t.Fatal("expected empty bucket error")
	}
	err = runLSD(configPath, []string{"missing"}, io.Discard, io.Discard, newDebugLogger(false, io.Discard))
	if err == nil || !strings.Contains(err.Error(), "bucket not found: missing") {
		t.Fatalf("err = %v", err)
	}
	err = runLSD(configPath, []string{"disabled"}, io.Discard, io.Discard, newDebugLogger(false, io.Discard))
	if err == nil || !strings.Contains(err.Error(), "bucket not found: disabled") {
		t.Fatalf("err = %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

Run:

```bash
go test ./cmd/tgs3
```

Expected: FAIL because `runLSD` still returns `lsd not implemented`.

- [ ] **Step 3: Implement `lsd`**

In `cmd/tgs3/main.go`, replace the temporary `runLSD` stub with:

```go
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
		dbg.Printf("page_after=%s page_limit=%d", afterKey, cliListPageSize)
		objects, err := meta.ListObjects(ctx, metadata.ListQuery{Bucket: bucket, Prefix: prefix, AfterKey: afterKey, Limit: cliListPageSize})
		if err != nil {
			return err
		}
		dbg.Printf("page_after=%s rows=%d", afterKey, len(objects))
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
```

- [ ] **Step 4: Run tests to verify pass**

Run:

```bash
go test ./cmd/tgs3
```

Expected: PASS.

- [ ] **Step 5: Record changed files without committing**

```bash
git diff -- cmd/tgs3/main.go cmd/tgs3/main_test.go
```

Expected: diff only contains `lsd` bucket/common-prefix listing and tests for this task.

---

### Task 6: Wire README documentation and full verification

**Files:**
- Create: `data/config.yaml`
- Modify: `README.md:80-100`
- Test: full repository

- [ ] **Step 1: Create default config template**

Create the `data/` directory if needed, then create `data/config.yaml` with:

```yaml
server:
  # listen: ":9000"
  # If set and the environment variable is non-empty, it overrides listen.
  # listen_env: "TGS3_LISTEN"
  # Public URL used when the service needs to describe its external endpoint.
  # public_base_url: "https://s3.example.com"

auth:
  # region: "us-east-1"
  credentials:
    - access_key: "admin"
      secret_key_env: "TGS3_SECRET_KEY"

telegram:
  bot_token_env: "TELEGRAM_BOT_TOKEN"
  # api_base_url: "https://api.telegram.org"
  # caption_template: "{bucket}/{key}"
  # timeout: "30s"

metadata:
  # sqlite_path: "data/metadata.sqlite"
  # If set and the environment variable is non-empty, it overrides sqlite_path.
  # sqlite_path_env: "TGS3_SQLITE_PATH"

storage:
  # upload_type_strategy: "document"
  # enable_chunking: true
  # max_file_size: 52428800
  # chunk_size: 20971520
  # type_size_limits:
  #   photo: 10485760
  #   video: 20971520
  #   audio: 20971520
  #   animation: 20971520
  #   document: 20971520
  # max_concurrent_uploads: 4
  # max_concurrent_downloads: 16
  # max_concurrent_telegram_requests: 8
  # put_buffer_size: 1048576

buckets:
  mybucket:
    chat_id: "-1001234567890"
  # private:
  #   chat_id: "${TGS3_PRIVATE_CHAT_ID}"
```

- [ ] **Step 2: Update README CLI documentation**

In `README.md`, add or replace the local CLI section with:

````markdown
## Service mode

When no subcommand is provided, `tgs3` starts the HTTP service. By default it reads `data/config.yaml` from the current working directory:

```bash
tgs3
```

## CLI usage

```text
tgs3 [-c|-config config.yaml]
tgs3 [-c|-config config.yaml] ls [-n|-limit N] bucket[/prefix]
tgs3 [-c|-config config.yaml] lsd [bucket[/prefix]]
```

`-c` is a short alias for `-config`. Passing both `-config` and `-c` in the same invocation is a usage error.

## Local metadata CLI

The `tgs3` binary also provides read-only local listing commands that inspect the configured SQLite metadata database. These commands do not start the HTTP server and do not contact Telegram.

`ls` prints object keys, one per line. It defaults to 1000 results; `-limit N` and `-n N` set the maximum result count, and `0` means no overall result limit while still reading in pages internally.

`lsd` without a path prints enabled bucket names. `lsd bucket/prefix` prints direct pseudo-directories under the prefix using `/` as the delimiter.
````

- [ ] **Step 3: Run formatting**

Run:

```bash
gofmt -w cmd/tgs3/main.go cmd/tgs3/main_test.go
```

Expected: no output.

- [ ] **Step 4: Run full tests**

Run:

```bash
go test ./...
```

Expected: PASS for all packages.

- [ ] **Step 5: Run build**

Run:

```bash
go build ./cmd/tgs3
```

Expected: command exits 0.

- [ ] **Step 6: Record changed files without committing**

```bash
git diff -- README.md data/config.yaml cmd/tgs3/main.go cmd/tgs3/main_test.go config/config.go config/config_test.go metadata/sqlite.go metadata/sqlite_test.go
```

Expected: diff contains the default config template, full local CLI implementation, config defaults, read-only SQLite support, tests, and README updates, with no commits created.

---

### Task 8: Add global `-debug` logging option

**Files:**
- Modify: `cmd/tgs3/main.go`
- Modify: `cmd/tgs3/main_test.go`
- Modify: `README.md`
- Modify: `docs/superpowers/specs/2026-05-23-tgs3-ls-lsd-cli-design.md`

- [ ] **Step 1: Write failing test for global `-debug` flag parsing**

Add a test that verifies `-debug` is accepted as a global flag and is not allowed after a subcommand:

```go
func TestParseGlobalFlagsAcceptsDebugFlag(t *testing.T) {
	configPath, debug, rest, err := parseGlobalFlags([]string{"-debug", "ls", "photos"}, io.Discard)
	if err != nil {
		t.Fatalf("parseGlobalFlags returned error: %v", err)
	}
	if configPath != "data/config.yaml" {
		t.Fatalf("configPath = %q", configPath)
	}
	if !debug {
		t.Fatal("debug = false, want true")
	}
	if strings.Join(rest, " ") != "ls photos" {
		t.Fatalf("rest = %v", rest)
	}
}

func TestRunMainRejectsDebugFlagAfterSubcommand(t *testing.T) {
	err := runMain([]string{"ls", "-debug", "photos"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("err = %v", err)
	}
}
```

- [ ] **Step 2: Run new debug-flag tests to verify they fail**

Run:

```bash
go test ./cmd/tgs3 -run "TestParseGlobalFlagsAcceptsDebugFlag|TestRunMainRejectsDebugFlagAfterSubcommand" -count=1 -v
```

Expected: FAIL because `-debug` is not accepted yet.

- [ ] **Step 3: Implement global `-debug` flag parsing**

In `parseGlobalFlags`, add:

```go
debug := fs.Bool("debug", false, "enable debug logging")
```

Return the parsed debug value alongside `configPath` and `rest`, and update `runMain` and callers accordingly.

- [ ] **Step 4: Write failing test for debug logger creation**

Add a test that verifies `-debug` creates a stderr-backed logger:

```go
func TestRunMainDebugFlagCreatesStderrLogger(t *testing.T) {
	var errOut strings.Builder
	err := runMain([]string{"-debug"}, io.Discard, &errOut)
	if err != nil {
		t.Fatalf("runMain returned error: %v", err)
	}
	if !strings.Contains(errOut.String(), "debug mode=service") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}
```

- [ ] **Step 5: Run debug logger test to verify it fails**

Run:

```bash
go test ./cmd/tgs3 -run TestRunMainDebugFlagCreatesStderrLogger -count=1 -v
```

Expected: FAIL because `runMain` does not emit debug logs yet.

- [ ] **Step 6: Implement minimal stderr-backed debug logger**

Create a debug logger that writes to stderr when `-debug` is set and discards output otherwise. Pass the logger into `run`, `runLS`, and `runLSD` so they can emit key operations.

- [ ] **Step 7: Write failing tests for key debug log points**

Add focused tests verifying that debug output contains:

- `mode=service` when no subcommand is used
- `mode=ls` when `ls` is used
- `mode=lsd` when `lsd` is used
- `sqlite_path=...` for local commands
- `bucket=... prefix=... limit=...` for `ls`
- `bucket=... prefix=...` for `lsd bucket/prefix`

- [ ] **Step 8: Implement debug log points for local commands and key service operations**

Add stderr debug logging for:

- resolved config path
- selected mode (`service`, `ls`, or `lsd`)
- resolved SQLite metadata path
- local listing bucket and prefix
- page cursor, page limit, and returned row counts
- service listen address
- configured bucket upsert names
- S3 compatibility branches: configured-bucket `CreateBucket` and `UNSIGNED-PAYLOAD`
- S3 `PutObject` request metadata: bucket, key, content length, content type, and payload hash mode
- object-store upload decisions: size, upload type, chunking, chunk size, chunk count
- Telegram upload progress per part: part number, media type, message ID, and whether a file ID was returned
- metadata write completion: bucket, key, chunk count, ETag, result
- S3 `PutObject` completion: bucket, key, result, ETag

Do not print secrets or sensitive values, including auth secret values, Telegram bot tokens, raw Authorization headers, object body bytes, or full Telegram file IDs.

- [ ] **Step 9: Write failing test for README coverage of `-debug`**

Verify README mentions the `-debug` flag and its stderr logging behavior.

- [ ] **Step 10: Update README**

Add a short note to the CLI section describing `-debug` and what logs to expect.

- [ ] **Step 11: Run targeted tests, full tests, and build**

Run:

```bash
go test ./cmd/tgs3 -run "TestParseGlobalFlagsAcceptsDebugFlag|TestRunMainRejectsDebugFlagAfterSubcommand|TestRunMainDebugFlagCreatesStderrLogger" -count=1 -v
go test ./...
go build ./cmd/tgs3
```

Expected: all commands pass.

---

### Task 7: Add rclone S3 upload compatibility fixes

**Files:**
- Modify: `internal/s3api/server.go`
- Modify: `internal/s3api/sigv4.go`
- Test: `internal/s3api/server_test.go`

- [ ] **Step 1: Write failing CreateBucket compatibility test**

Replace the previous `CreateBucket` disabled assertion with configured-bucket success and missing-bucket rejection tests:

```go
func TestCreateBucketForConfiguredBucketSucceeds(t *testing.T) {
	server := newSignedTestServer(t)
	put := signedRecorderRequest(t, http.MethodPut, "/photos", "", nil)
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK || put.recorder.Body.Len() != 0 {
		t.Fatalf("status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}
}

func TestCreateBucketForMissingBucketReturnsNotFound(t *testing.T) {
	server := newSignedTestServer(t)
	put := signedRecorderRequest(t, http.MethodPut, "/missing", "", nil)
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusNotFound || !strings.Contains(put.recorder.Body.String(), "<Code>NoSuchBucket</Code>") {
		t.Fatalf("status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}
}
```

- [ ] **Step 2: Run CreateBucket test to verify it fails**

Run:

```bash
go test ./internal/s3api -run TestCreateBucketForConfiguredBucketSucceeds -count=1
```

Expected before implementation: FAIL because `PUT /photos` returns `501 NotImplemented`.

- [ ] **Step 3: Implement minimal CreateBucket compatibility**

In `handleBucket`, change `PUT /bucket` to check the configured bucket through `HeadBucket`. Return `200 OK` when it exists, otherwise return the mapped lookup error:

```go
case http.MethodPut:
	if err := s.store.HeadBucket(r.Context(), bucket); err != nil {
		WriteErrorResponse(w, r, MapError(err), r.URL.Path, "")
		return
	}
	w.WriteHeader(http.StatusOK)
```

This intentionally does not create new buckets or mutate metadata.

- [ ] **Step 4: Write failing UNSIGNED-PAYLOAD PutObject test**

Add a server-level test that signs `PutObject` with `X-Amz-Content-Sha256: UNSIGNED-PAYLOAD`, uploads an object, and reads it back:

```go
func TestPutObjectAcceptsUnsignedPayload(t *testing.T) {
	server := newSignedTestServer(t)
	put := signedUnsignedPayloadRecorderRequest(t, http.MethodPut, "/photos/unsigned.txt", "hello", map[string]string{"Content-Type": "text/plain"})
	server.ServeHTTP(put.recorder, put.request)
	if put.recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", put.recorder.Code, put.recorder.Body.String())
	}

	get := signedRecorderRequest(t, http.MethodGet, "/photos/unsigned.txt", "", nil)
	server.ServeHTTP(get.recorder, get.request)
	if get.recorder.Code != http.StatusOK || get.recorder.Body.String() != "hello" {
		t.Fatalf("get status = %d body = %q", get.recorder.Code, get.recorder.Body.String())
	}
}

func signedUnsignedPayloadRecorderRequest(t *testing.T, method, path, body string, headers map[string]string) signedHTTPTest {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	request.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	signRequest(t, request, "AKID", "SECRET")
	return signedHTTPTest{recorder: httptest.NewRecorder(), request: request}
}
```

- [ ] **Step 5: Run UNSIGNED-PAYLOAD test to verify it fails**

Run:

```bash
go test ./internal/s3api -run TestPutObjectAcceptsUnsignedPayload -count=1
```

Expected before implementation: FAIL with `403 SignatureDoesNotMatch`.

- [ ] **Step 6: Implement UNSIGNED-PAYLOAD SigV4 compatibility**

In `payloadHash`, accept `UNSIGNED-PAYLOAD` as the canonical payload hash without reading or replacing the request body:

```go
if hash == "UNSIGNED-PAYLOAD" {
	return hash, nil
}
```

Keep the existing full body SHA256 validation for normal hashed payloads.

- [ ] **Step 7: Run targeted and full verification**

Run:

```bash
go test ./internal/s3api -run "TestCreateBucketForConfiguredBucketSucceeds|TestCreateBucketForMissingBucketReturnsNotFound|TestPutObjectAcceptsUnsignedPayload" -count=1
go test ./internal/s3api -count=1
go test ./...
go build ./cmd/tgs3
```

Expected: all commands pass.

---

## Self-Review Checklist

- Spec coverage:
  - default `data/metadata.sqlite` SQLite path and `us-east-1` auth region: Task 1.
  - `-c` alias: Task 2.
  - rejecting both config aliases: Task 2.
  - default server behavior unchanged: Task 2.
  - default `data/config.yaml` from the current working directory and default config template: Tasks 2 and 6.
  - concise CLI usage for service mode, `ls`, and `lsd`: Task 6.
  - `ls bucket/prefix`: Task 4.
  - `ls -limit` and `ls -n`: Task 4.
  - `ls -n 0` unlimited with internal paging: Task 4.
  - missing and disabled bucket errors: Tasks 4 and 5.
  - `lsd` buckets: Task 5.
  - `lsd bucket/prefix` common prefixes with `/`: Task 5.
  - `lsd` rejects unsupported flags such as `-n` and `-limit`: Task 5.
  - SQLite-only, no Telegram/server startup, and no database creation/migration for listing: Tasks 3-5.
  - README docs: Task 6.
  - global `-debug` flag, stderr debug logging, and key debug log points for service, `ls`, `lsd`, and `PutObject`: Task 8.
  - rclone upload compatibility for idempotent configured-bucket `CreateBucket` and `UNSIGNED-PAYLOAD` `PutObject`: Task 7.
- Placeholder scan: no TBD/TODO placeholders.
- Type consistency:
  - Uses existing `metadata.Store`, `metadata.ListQuery`, `metadata.Object`, and `metadata.Bucket` names.
  - Keeps existing `run(configPath string) error` server function.
  - Adds `runMain(args []string, stdout io.Writer, stderr io.Writer) error` as dispatch function.
