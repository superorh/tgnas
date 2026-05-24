# tgnas Rename + WebDAV Server Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rename the visible CLI from `tgs3` to `tgnas`, add `s3`/`dav` service subcommands, and implement a WebDAV server backed by the existing Telegram/object metadata store.

**Architecture:** The binary moves from `cmd/tgs3` to `cmd/tgnas`. A `serverMode` enum controls which protocols start. WebDAV uses `golang.org/x/net/webdav` with a custom `webdav.FileSystem` backed by the existing metadata store. Metadata-level `CopyObject`/`MoveObject`/`CopyPrefix`/`MovePrefix`/`DeletePrefix`/`DeleteBucket` helpers enable metadata-only recursive operations in SQLite transactions. WebDAV Basic Auth reuses `auth.credentials`. A combined HTTP handler routes by path prefix.

**Tech Stack:** Go, `golang.org/x/net/webdav`, `modernc.org/sqlite`, `gopkg.in/yaml.v3`

---

## File Structure

| File | Role |
|------|------|
| `cmd/tgnas/main.go` | CLI dispatch, server startup, `ls`/`lsd` (moved from `cmd/tgs3`) |
| `cmd/tgnas/main_test.go` | CLI and integration tests (moved from `cmd/tgs3`) |
| `config/config.go` | Add `WebDAVConfig`, update env defaults to `TGNAS_*` |
| `config/config_test.go` | Config validation tests |
| `metadata/types.go` | Add `CopyOptions`, `MoveOptions`, `CopyResult`, `MoveResult` types, extend `Store` interface |
| `metadata/sqlite.go` | Implement `CopyObject`, `MoveObject`, `CopyPrefix`, `MovePrefix`, `DeletePrefix`, `DeleteBucket`; make SQLite read-only `file:` URI paths absolute |
| `metadata/sqlite_test.go` | Metadata helper tests and read-only relative SQLite path regression |
| `store/store.go` | Store zero-byte objects metadata-only without Telegram upload |
| `store/store_test.go` | Zero-byte object regression test |
| `internal/dav/fs.go` | `webdav.FileSystem` implementation backed by metadata store; writable PUT files; seekable reads; object-store interface |
| `internal/dav/fs_test.go` | FileSystem tests |
| `internal/dav/handler.go` | WebDAV HTTP handler: auth, routing, COPY/MOVE, OPTIONS, log filtering, orphan checks |
| `internal/dav/handler_test.go` | Handler integration tests, Finder/class-2 compatibility regressions |
| `internal/dav/lock.go` | Lightweight in-memory `webdav.LockSystem` for Finder class-2 LOCK/UNLOCK compatibility |
| `data/config.yaml` | Update example config with `webdav:` section and `TGNAS_*` env names |
| `README.md` | Update documentation |

---

## Task 1: Rename Binary from cmd/tgs3 to cmd/tgnas

**Files:**
- Create: `cmd/tgnas/main.go` (copy of `cmd/tgs3/main.go`)
- Create: `cmd/tgnas/main_test.go` (copy of `cmd/tgs3/main_test.go`)
- Delete: `cmd/tgs3/main.go`
- Delete: `cmd/tgs3/main_test.go`

This task moves the binary directory, updates user-facing strings, and changes the Go module/import path to `github.com/aahl/tgnas`.

- [ ] **Step 1: Copy cmd/tgs3 to cmd/tgnas**

```bash
mkdir -p cmd/tgnas
cp cmd/tgs3/main.go cmd/tgnas/main.go
cp cmd/tgs3/main_test.go cmd/tgnas/main_test.go
```

- [ ] **Step 2: Update user-facing strings in cmd/tgnas/main.go**

In `cmd/tgnas/main.go`, replace all user-facing `tgs3` references with `tgnas`:

- Line 38-41: `topLevelUsage` — change `tgs3` to `tgnas` in usage lines
- Line 86: `parseGlobalFlags` — change `flag.NewFlagSet("tgs3", ...)` to `flag.NewFlagSet("tgnas", ...)`

Do NOT change:
- Package name (`package main`)

Also update Go import paths from `github.com/aahl/tgs3/...` to `github.com/aahl/tgnas/...`.

- [ ] **Step 3: Update default env names in config/config.go**

In `config/config.go` line 86, change the default `ListenEnv`:

```go
Server: ServerConfig{Listen: ":9000", ListenEnv: "TGNAS_LISTEN"},
```

- [ ] **Step 4: Update default config template**

In `data/config.yaml`, update env var examples from `TGS3_*` to `TGNAS_*`:

- `TGS3_LISTEN` → `TGNAS_LISTEN`
- `TGS3_SECRET_KEY` → `TGNAS_SECRET_KEY`
- `TELEGRAM_BOT_TOKEN` → `TGNAS_TELEGRAM_BOT_TOKEN`
- `TGS3_SQLITE_PATH` → `TGNAS_SQLITE_PATH`
- `TGS3_PRIVATE_CHAT_ID` → `TGNAS_PRIVATE_CHAT_ID`

- [ ] **Step 5: Delete old cmd/tgs3 directory**

```bash
rm -rf cmd/tgs3
```

- [ ] **Step 6: Verify build and tests pass**

```bash
go build ./cmd/tgnas
go test ./...
```

Expected: All tests pass, binary builds as `tgnas`.

- [ ] **Step 7: Commit**

```bash
git add cmd/tgnas/ config/config.go data/config.yaml
git rm -r cmd/tgs3/
git commit -m "feat: rename tgs3 to tgnas"
```

---

## Task 2: Add WebDAV Configuration

**Files:**
- Modify: `config/config.go`
- Test: `config/config_test.go`

- [ ] **Step 1: Add WebDAVConfig struct and validation tests**

In `config/config_test.go`, add tests:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./config/... -run TestWebDAV -v`
Expected: FAIL with undefined types/methods.

- [ ] **Step 3: Add WebDAVConfig struct to config/config.go**

Add after `BucketConfig` (line 82):

```go
type WebDAVConfig struct {
    Prefix string `yaml:"prefix"`
}
```

Add `WebDAV WebDAVConfig` field to `Config` struct:

```go
type Config struct {
    Server   ServerConfig            `yaml:"server"`
    Auth     AuthConfig              `yaml:"auth"`
    Telegram TelegramConfig          `yaml:"telegram"`
    Metadata MetadataConfig          `yaml:"metadata"`
    Storage  StorageConfig           `yaml:"storage"`
    Buckets  map[string]BucketConfig `yaml:"buckets"`
    WebDAV   WebDAVConfig            `yaml:"webdav"`
}
```

- [ ] **Step 4: Add applyWebDAVDefaults method**

```go
func (c *Config) applyWebDAVDefaults() {
    if c.WebDAV.Prefix == "" {
        c.WebDAV.Prefix = "/dav/"
    }
    if !strings.HasSuffix(c.WebDAV.Prefix, "/") {
        c.WebDAV.Prefix += "/"
    }
}
```

- [ ] **Step 5: Add validateWebDAV method**

```go
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
```

- [ ] **Step 6: Integrate into LoadFile**

In `LoadFile`, call `applyWebDAVDefaults()` after `applyStorageDefaults` and call `validateWebDAV()` inside `Validate()`.

- [ ] **Step 7: Run tests**

Run: `go test ./config/... -v`
Expected: All tests pass.

- [ ] **Step 8: Commit**

```bash
git add config/config.go config/config_test.go
git commit -m "feat: add webdav config with prefix validation"
```

---

## Task 3: Add Metadata Helpers for WebDAV Copy/Move/Delete

**Files:**
- Modify: `metadata/types.go`
- Modify: `metadata/sqlite.go`
- Test: `metadata/sqlite_test.go`

- [ ] **Step 1: Add types to metadata/types.go**

Add after `ListQuery` (line 49):

```go
type CopyOptions struct {
    Overwrite bool
}

type MoveOptions struct {
    Overwrite bool
}

type CopyResult struct {
    Created bool
}

type MoveResult struct {
    Created bool
}
```

Add new methods to `Store` interface:

```go
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
    CopyObject(ctx context.Context, bucket, srcKey, dstKey string, options CopyOptions) (CopyResult, error)
    MoveObject(ctx context.Context, bucket, srcKey, dstKey string, options MoveOptions) (MoveResult, error)
    CopyPrefix(ctx context.Context, bucket, srcPrefix, dstPrefix string, options CopyOptions) (CopyResult, error)
    MovePrefix(ctx context.Context, bucket, srcPrefix, dstPrefix string, options MoveOptions) (MoveResult, error)
    DeletePrefix(ctx context.Context, bucket, prefix string) error
    DeleteBucket(ctx context.Context, bucket string) error
    ListAllObjects(ctx context.Context, bucket, prefix string) ([]Object, error)
    CountObjects(ctx context.Context, bucket, prefix string) (int, error)
}
```

- [ ] **Step 2: Write failing tests for CopyObject**

In `metadata/sqlite_test.go`:

```go
func TestCopyObjectCreatesNew(t *testing.T) {
    store := openTestStore(t)
    ctx := context.Background()
    seedBucket(t, store, "b1")
    seedObject(t, store, "b1", "src.txt", 100, "etag1")

    result, err := store.CopyObject(ctx, "b1", "src.txt", "dst.txt", CopyOptions{})
    if err != nil {
        t.Fatalf("CopyObject: %v", err)
    }
    if !result.Created {
        t.Fatal("expected Created=true")
    }

    obj, _, err := store.GetObject(ctx, "b1", "dst.txt")
    if err != nil {
        t.Fatalf("GetObject dst: %v", err)
    }
    if obj.Key != "dst.txt" || obj.Size != 100 || obj.ETag != "etag1" {
        t.Fatalf("unexpected dst object: %+v", obj)
    }
}

func TestCopyObjectOverwrite(t *testing.T) {
    store := openTestStore(t)
    ctx := context.Background()
    seedBucket(t, store, "b1")
    seedObject(t, store, "b1", "src.txt", 100, "etag1")
    seedObject(t, store, "b1", "dst.txt", 50, "etag2")

    result, err := store.CopyObject(ctx, "b1", "src.txt", "dst.txt", CopyOptions{Overwrite: true})
    if err != nil {
        t.Fatalf("CopyObject: %v", err)
    }
    if result.Created {
        t.Fatal("expected Created=false (overwrite)")
    }

    obj, _, err := store.GetObject(ctx, "b1", "dst.txt")
    if err != nil {
        t.Fatalf("GetObject: %v", err)
    }
    if obj.Size != 100 || obj.ETag != "etag1" {
        t.Fatalf("expected overwritten object, got %+v", obj)
    }
}

func TestCopyObjectNoOverwriteFailsIfExists(t *testing.T) {
    store := openTestStore(t)
    ctx := context.Background()
    seedBucket(t, store, "b1")
    seedObject(t, store, "b1", "src.txt", 100, "etag1")
    seedObject(t, store, "b1", "dst.txt", 50, "etag2")

    _, err := store.CopyObject(ctx, "b1", "src.txt", "dst.txt", CopyOptions{Overwrite: false})
    if err == nil {
        t.Fatal("expected error for existing destination with Overwrite=false")
    }
}
```

- [ ] **Step 3: Write failing tests for MoveObject**

```go
func TestMoveObjectCreatesNew(t *testing.T) {
    store := openTestStore(t)
    ctx := context.Background()
    seedBucket(t, store, "b1")
    seedObject(t, store, "b1", "src.txt", 100, "etag1")

    result, err := store.MoveObject(ctx, "b1", "src.txt", "dst.txt", MoveOptions{})
    if err != nil {
        t.Fatalf("MoveObject: %v", err)
    }
    if !result.Created {
        t.Fatal("expected Created=true")
    }

    _, err = store.HeadObject(ctx, "b1", "src.txt")
    if err != metadata.ErrNotFound {
        t.Fatalf("expected source deleted, got %v", err)
    }

    obj, _, err := store.GetObject(ctx, "b1", "dst.txt")
    if err != nil {
        t.Fatalf("GetObject dst: %v", err)
    }
    if obj.Size != 100 || obj.ETag != "etag1" {
        t.Fatalf("unexpected dst object: %+v", obj)
    }
}
```

- [ ] **Step 4: Write failing tests for CopyPrefix/MovePrefix/DeletePrefix/DeleteBucket**

```go
func TestCopyPrefixRecursive(t *testing.T) {
    store := openTestStore(t)
    ctx := context.Background()
    seedBucket(t, store, "b1")
    seedObject(t, store, "b1", "dir/a.txt", 10, "e1")
    seedObject(t, store, "b1", "dir/b.txt", 20, "e2")
    seedObject(t, store, "b1", "dir/sub/c.txt", 30, "e3")

    _, err := store.CopyPrefix(ctx, "b1", "dir/", "copy/", CopyOptions{})
    if err != nil {
        t.Fatalf("CopyPrefix: %v", err)
    }

    objs, err := store.ListAllObjects(ctx, "b1", "copy/")
    if err != nil {
        t.Fatalf("ListAllObjects: %v", err)
    }
    if len(objs) != 3 {
        t.Fatalf("expected 3 objects, got %d", len(objs))
    }
    // originals still exist
    origObjs, _ := store.ListAllObjects(ctx, "b1", "dir/")
    if len(origObjs) != 3 {
        t.Fatalf("expected 3 originals, got %d", len(origObjs))
    }
}

func TestMovePrefixRecursive(t *testing.T) {
    store := openTestStore(t)
    ctx := context.Background()
    seedBucket(t, store, "b1")
    seedObject(t, store, "b1", "dir/a.txt", 10, "e1")
    seedObject(t, store, "b1", "dir/b.txt", 20, "e2")

    _, err := store.MovePrefix(ctx, "b1", "dir/", "moved/", MoveOptions{})
    if err != nil {
        t.Fatalf("MovePrefix: %v", err)
    }

    origObjs, _ := store.ListAllObjects(ctx, "b1", "dir/")
    if len(origObjs) != 0 {
        t.Fatalf("expected source deleted, got %d", len(origObjs))
    }
    movedObjs, _ := store.ListAllObjects(ctx, "b1", "moved/")
    if len(movedObjs) != 2 {
        t.Fatalf("expected 2 moved objects, got %d", len(movedObjs))
    }
}

func TestDeletePrefixRecursive(t *testing.T) {
    store := openTestStore(t)
    ctx := context.Background()
    seedBucket(t, store, "b1")
    seedObject(t, store, "b1", "dir/a.txt", 10, "e1")
    seedObject(t, store, "b1", "dir/b.txt", 20, "e2")
    seedObject(t, store, "b1", "other.txt", 30, "e3")

    err := store.DeletePrefix(ctx, "b1", "dir/")
    if err != nil {
        t.Fatalf("DeletePrefix: %v", err)
    }

    objs, _ := store.ListAllObjects(ctx, "b1", "dir/")
    if len(objs) != 0 {
        t.Fatalf("expected 0 under dir/, got %d", len(objs))
    }
    objs, _ = store.ListAllObjects(ctx, "b1", "other")
    if len(objs) != 1 {
        t.Fatalf("other.txt should still exist")
    }
}

func TestDeleteBucket(t *testing.T) {
    store := openTestStore(t)
    ctx := context.Background()
    seedBucket(t, store, "b1")
    seedObject(t, store, "b1", "a.txt", 10, "e1")

    err := store.DeleteBucket(ctx, "b1")
    if err != nil {
        t.Fatalf("DeleteBucket: %v", err)
    }

    _, err = store.GetBucket(ctx, "b1")
    if err != metadata.ErrNotFound {
        t.Fatalf("expected bucket gone, got %v", err)
    }
}
```

- [ ] **Step 5: Run tests to verify they fail**

Run: `go test ./metadata/... -v`
Expected: FAIL with undefined methods.

- [ ] **Step 6: Implement CopyObject in metadata/sqlite.go**

```go
func (s *SQLiteStore) CopyObject(ctx context.Context, bucket, srcKey, dstKey string, options CopyOptions) (CopyResult, error) {
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return CopyResult{}, err
    }
    defer tx.Rollback()

    src, err := scanObject(tx.QueryRowContext(ctx, `
        SELECT bucket, key, size, content_type, etag, sha256, last_modified, chunk_count, telegram_type, upload_strategy
        FROM objects WHERE bucket = ? AND key = ?
    `, bucket, srcKey))
    if err != nil {
        return CopyResult{}, err
    }

    existing, _ := scanObject(tx.QueryRowContext(ctx, `
        SELECT bucket, key, size, content_type, etag, sha256, last_modified, chunk_count, telegram_type, upload_strategy
        FROM objects WHERE bucket = ? AND key = ?
    `, bucket, dstKey))
    dstExists := existing.Key != ""

    if dstExists && !options.Overwrite {
        return CopyResult{}, fmt.Errorf("destination already exists")
    }

    now := time.Now().UTC()
    _, err = tx.ExecContext(ctx, `
        INSERT INTO objects (bucket, key, size, content_type, etag, sha256, last_modified, chunk_count, telegram_type, upload_strategy)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(bucket, key) DO UPDATE SET
            size = excluded.size, content_type = excluded.content_type, etag = excluded.etag,
            sha256 = excluded.sha256, last_modified = excluded.last_modified,
            chunk_count = excluded.chunk_count, telegram_type = excluded.telegram_type,
            upload_strategy = excluded.upload_strategy
    `, bucket, dstKey, src.Size, src.ContentType, src.ETag, src.SHA256, now.Unix(), src.ChunkCount, src.TelegramType, src.UploadStrategy)
    if err != nil {
        return CopyResult{}, err
    }

    _, err = tx.ExecContext(ctx, `DELETE FROM object_chunks WHERE bucket = ? AND key = ?`, bucket, dstKey)
    if err != nil {
        return CopyResult{}, err
    }

    rows, err := tx.QueryContext(ctx, `
        SELECT bucket, key, part_number, offset, size, telegram_type, telegram_file_id, telegram_message_id, telegram_file_unique_id, sha256
        FROM object_chunks WHERE bucket = ? AND key = ?
        ORDER BY part_number ASC
    `, bucket, srcKey)
    if err != nil {
        return CopyResult{}, err
    }
    defer rows.Close()

    for rows.Next() {
        var c Chunk
        if err := rows.Scan(&c.Bucket, &c.Key, &c.PartNumber, &c.Offset, &c.Size, &c.TelegramType, &c.TelegramFileID, &c.TelegramMessageID, &c.TelegramFileUniqueID, &c.SHA256); err != nil {
            return CopyResult{}, err
        }
        _, err = tx.ExecContext(ctx, `
            INSERT INTO object_chunks (bucket, key, part_number, offset, size, telegram_type, telegram_file_id, telegram_message_id, telegram_file_unique_id, sha256)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        `, bucket, dstKey, c.PartNumber, c.Offset, c.Size, c.TelegramType, c.TelegramFileID, c.TelegramMessageID, c.TelegramFileUniqueID, c.SHA256)
        if err != nil {
            return CopyResult{}, err
        }
    }
    if err := rows.Err(); err != nil {
        return CopyResult{}, err
    }

    return CopyResult{Created: !dstExists}, tx.Commit()
}
```

- [ ] **Step 7: Implement MoveObject**

```go
func (s *SQLiteStore) MoveObject(ctx context.Context, bucket, srcKey, dstKey string, options MoveOptions) (MoveResult, error) {
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return MoveResult{}, err
    }
    defer tx.Rollback()

    src, err := scanObject(tx.QueryRowContext(ctx, `
        SELECT bucket, key, size, content_type, etag, sha256, last_modified, chunk_count, telegram_type, upload_strategy
        FROM objects WHERE bucket = ? AND key = ?
    `, bucket, srcKey))
    if err != nil {
        return MoveResult{}, err
    }

    existing, _ := scanObject(tx.QueryRowContext(ctx, `
        SELECT bucket, key, size, content_type, etag, sha256, last_modified, chunk_count, telegram_type, upload_strategy
        FROM objects WHERE bucket = ? AND key = ?
    `, bucket, dstKey))
    dstExists := existing.Key != ""

    if dstExists && !options.Overwrite {
        return MoveResult{}, fmt.Errorf("destination already exists")
    }

    now := time.Now().UTC()
    _, err = tx.ExecContext(ctx, `
        INSERT INTO objects (bucket, key, size, content_type, etag, sha256, last_modified, chunk_count, telegram_type, upload_strategy)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(bucket, key) DO UPDATE SET
            size = excluded.size, content_type = excluded.content_type, etag = excluded.etag,
            sha256 = excluded.sha256, last_modified = excluded.last_modified,
            chunk_count = excluded.chunk_count, telegram_type = excluded.telegram_type,
            upload_strategy = excluded.upload_strategy
    `, bucket, dstKey, src.Size, src.ContentType, src.ETag, src.SHA256, now.Unix(), src.ChunkCount, src.TelegramType, src.UploadStrategy)
    if err != nil {
        return MoveResult{}, err
    }

    _, err = tx.ExecContext(ctx, `DELETE FROM object_chunks WHERE bucket = ? AND key = ?`, bucket, dstKey)
    if err != nil {
        return MoveResult{}, err
    }

    _, err = tx.ExecContext(ctx, `
        UPDATE object_chunks SET key = ? WHERE bucket = ? AND key = ?
    `, dstKey, bucket, srcKey)
    if err != nil {
        return MoveResult{}, err
    }

    _, err = tx.ExecContext(ctx, `DELETE FROM objects WHERE bucket = ? AND key = ?`, bucket, srcKey)
    if err != nil {
        return MoveResult{}, err
    }

    return MoveResult{Created: !dstExists}, tx.Commit()
}
```

- [ ] **Step 8: Implement CopyPrefix**

```go
func (s *SQLiteStore) CopyPrefix(ctx context.Context, bucket, srcPrefix, dstPrefix string, options CopyOptions) (CopyResult, error) {
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return CopyResult{}, err
    }
    defer tx.Rollback()

    if err := copyPrefixInTx(ctx, tx, bucket, srcPrefix, dstPrefix, options.Overwrite); err != nil {
        return CopyResult{}, err
    }
    return CopyResult{Created: true}, tx.Commit()
}
```

- [ ] **Step 9: Implement MovePrefix**

```go
func (s *SQLiteStore) MovePrefix(ctx context.Context, bucket, srcPrefix, dstPrefix string, options MoveOptions) (MoveResult, error) {
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return MoveResult{}, err
    }
    defer tx.Rollback()

    if err := copyPrefixInTx(ctx, tx, bucket, srcPrefix, dstPrefix, options.Overwrite); err != nil {
        return MoveResult{}, err
    }

    likePattern := escapeSQLiteLikePattern(srcPrefix) + "%"
    _, err = tx.ExecContext(ctx, `DELETE FROM object_chunks WHERE bucket = ? AND key LIKE ? ESCAPE '\'`, bucket, likePattern)
    if err != nil {
        return MoveResult{}, err
    }
    _, err = tx.ExecContext(ctx, `DELETE FROM objects WHERE bucket = ? AND key LIKE ? ESCAPE '\'`, bucket, likePattern)
    if err != nil {
        return MoveResult{}, err
    }

    return MoveResult{Created: true}, tx.Commit()
}
```

- [ ] **Step 10: Implement DeletePrefix**

```go
func (s *SQLiteStore) DeletePrefix(ctx context.Context, bucket, prefix string) error {
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    likePattern := escapeSQLiteLikePattern(prefix) + "%"
    _, err = tx.ExecContext(ctx, `DELETE FROM object_chunks WHERE bucket = ? AND key LIKE ? ESCAPE '\'`, bucket, likePattern)
    if err != nil {
        return err
    }
    _, err = tx.ExecContext(ctx, `DELETE FROM objects WHERE bucket = ? AND key LIKE ? ESCAPE '\'`, bucket, likePattern)
    if err != nil {
        return err
    }

    return tx.Commit()
}
```

- [ ] **Step 11: Implement DeleteBucket**

```go
func (s *SQLiteStore) DeleteBucket(ctx context.Context, bucket string) error {
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    _, err = tx.ExecContext(ctx, `DELETE FROM object_chunks WHERE bucket = ?`, bucket)
    if err != nil {
        return err
    }
    _, err = tx.ExecContext(ctx, `DELETE FROM objects WHERE bucket = ?`, bucket)
    if err != nil {
        return err
    }
    _, err = tx.ExecContext(ctx, `DELETE FROM buckets WHERE name = ?`, bucket)
    if err != nil {
        return err
    }

    return tx.Commit()
}
```

- [ ] **Step 12: Implement ListAllObjects and CountObjects**

```go
func (s *SQLiteStore) ListAllObjects(ctx context.Context, bucket, prefix string) ([]Object, error) {
    var all []Object
    afterKey := ""
    for {
        objs, err := s.ListObjects(ctx, ListQuery{Bucket: bucket, Prefix: prefix, AfterKey: afterKey, Limit: 1000})
        if err != nil {
            return nil, err
        }
        if len(objs) == 0 {
            return all, nil
        }
        all = append(all, objs...)
        afterKey = objs[len(objs)-1].Key
    }
}

func (s *SQLiteStore) CountObjects(ctx context.Context, bucket, prefix string) (int, error) {
    row := s.db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM objects
        WHERE bucket = ? AND key LIKE ? ESCAPE '\'
    `, bucket, escapeSQLiteLikePattern(prefix)+"%")
    var count int
    if err := row.Scan(&count); err != nil {
        return 0, err
    }
    return count, nil
}
```

- [ ] **Step 13: Implement copyPrefixInTx helper**

```go
func copyPrefixInTx(ctx context.Context, tx *sql.Tx, bucket, srcPrefix, dstPrefix string, overwrite bool) error {
    rows, err := tx.QueryContext(ctx, `
        SELECT bucket, key, size, content_type, etag, sha256, last_modified, chunk_count, telegram_type, upload_strategy
        FROM objects WHERE bucket = ? AND key LIKE ? ESCAPE '\'
        ORDER BY key ASC
    `, bucket, escapeSQLiteLikePattern(srcPrefix)+"%")
    if err != nil {
        return err
    }
    defer rows.Close()

    var objects []Object
    for rows.Next() {
        obj, err := scanObject(rows)
        if err != nil {
            return err
        }
        objects = append(objects, obj)
    }
    if err := rows.Err(); err != nil {
        return err
    }

    now := time.Now().UTC()
    for _, src := range objects {
        dstKey := dstPrefix + strings.TrimPrefix(src.Key, srcPrefix)

        if !overwrite {
            var count int
            err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM objects WHERE bucket = ? AND key = ?`, bucket, dstKey).Scan(&count)
            if err != nil {
                return err
            }
            if count > 0 {
                return fmt.Errorf("destination already exists: %s", dstKey)
            }
        }

        _, err = tx.ExecContext(ctx, `
            INSERT INTO objects (bucket, key, size, content_type, etag, sha256, last_modified, chunk_count, telegram_type, upload_strategy)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT(bucket, key) DO UPDATE SET
                size = excluded.size, content_type = excluded.content_type, etag = excluded.etag,
                sha256 = excluded.sha256, last_modified = excluded.last_modified,
                chunk_count = excluded.chunk_count, telegram_type = excluded.telegram_type,
                upload_strategy = excluded.upload_strategy
        `, bucket, dstKey, src.Size, src.ContentType, src.ETag, src.SHA256, now.Unix(), src.ChunkCount, src.TelegramType, src.UploadStrategy)
        if err != nil {
            return err
        }

        _, err = tx.ExecContext(ctx, `DELETE FROM object_chunks WHERE bucket = ? AND key = ?`, bucket, dstKey)
        if err != nil {
            return err
        }

        chunkRows, err := tx.QueryContext(ctx, `
            SELECT bucket, key, part_number, offset, size, telegram_type, telegram_file_id, telegram_message_id, telegram_file_unique_id, sha256
            FROM object_chunks WHERE bucket = ? AND key = ?
            ORDER BY part_number ASC
        `, bucket, src.Key)
        if err != nil {
            return err
        }

        for chunkRows.Next() {
            var c Chunk
            if err := chunkRows.Scan(&c.Bucket, &c.Key, &c.PartNumber, &c.Offset, &c.Size, &c.TelegramType, &c.TelegramFileID, &c.TelegramMessageID, &c.TelegramFileUniqueID, &c.SHA256); err != nil {
                chunkRows.Close()
                return err
            }
            _, err = tx.ExecContext(ctx, `
                INSERT INTO object_chunks (bucket, key, part_number, offset, size, telegram_type, telegram_file_id, telegram_message_id, telegram_file_unique_id, sha256)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            `, bucket, dstKey, c.PartNumber, c.Offset, c.Size, c.TelegramType, c.TelegramFileID, c.TelegramMessageID, c.TelegramFileUniqueID, c.SHA256)
            if err != nil {
                chunkRows.Close()
                return err
            }
        }
        if err := chunkRows.Err(); err != nil {
            chunkRows.Close()
            return err
        }
        chunkRows.Close()
    }
    return nil
}
```

- [ ] **Step 14: Run tests**

Run: `go test ./metadata/... -v`
Expected: All tests pass.

- [ ] **Step 15: Commit**

```bash
git add metadata/types.go metadata/sqlite.go metadata/sqlite_test.go
git commit -m "feat: add metadata helpers for webdav copy/move/delete"
```

---

## Task 4: Add serverMode and Subcommand Dispatch

**Files:**
- Modify: `cmd/tgnas/main.go`
- Test: `cmd/tgnas/main_test.go`

- [ ] **Step 1: Write failing tests for subcommand dispatch**

In `cmd/tgnas/main_test.go`, add tests:

```go
func TestRunMainS3Subcommand(t *testing.T) {
    tmp := t.TempDir()
    configPath := filepath.Join(tmp, "config.yaml")
    writeTestConfig(t, configPath, "server:\n  listen: \":0\"\n")

    called := false
    origRun := runServiceFunc
    runServiceFunc = func(configPath string, mode serverMode, dbg debugLogger) error {
        called = true
        if mode != serverModeS3 {
            t.Fatalf("expected serverModeS3, got %v", mode)
        }
        return nil
    }
    defer func() { runServiceFunc = origRun }()

    err := runMain([]string{"-c", configPath, "s3"}, io.Discard, io.Discard)
    if err != nil {
        t.Fatalf("runMain: %v", err)
    }
    if !called {
        t.Fatal("runServiceFunc not called")
    }
}

func TestRunMainDavSubcommand(t *testing.T) {
    tmp := t.TempDir()
    configPath := filepath.Join(tmp, "config.yaml")
    writeTestConfig(t, configPath, "server:\n  listen: \":0\"\n")

    called := false
    origRun := runServiceFunc
    runServiceFunc = func(configPath string, mode serverMode, dbg debugLogger) error {
        called = true
        if mode != serverModeDAV {
            t.Fatalf("expected serverModeDAV, got %v", mode)
        }
        return nil
    }
    defer func() { runServiceFunc = origRun }()

    err := runMain([]string{"-c", configPath, "dav"}, io.Discard, io.Discard)
    if err != nil {
        t.Fatalf("runMain: %v", err)
    }
    if !called {
        t.Fatal("runServiceFunc not called")
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/tgnas/... -run TestRunMainS3 -v`
Expected: FAIL with undefined types.

- [ ] **Step 3: Add serverMode type and update runMain**

Add at the top of `cmd/tgnas/main.go`:

```go
type serverMode string

const (
    serverModeAll serverMode = "all"
    serverModeS3  serverMode = "s3"
    serverModeDAV serverMode = "dav"
)
```

Update the usage text:

```go
const topLevelUsage = "Usage:\n" +
    "  tgnas [-debug] [-c|-config config.yaml]\n" +
    "  tgnas [-debug] [-c|-config config.yaml] s3\n" +
    "  tgnas [-debug] [-c|-config config.yaml] dav\n" +
    "  tgnas [-debug] [-c|-config config.yaml] ls [-n|-limit N] bucket[/prefix]\n" +
    "  tgnas [-debug] [-c|-config config.yaml] lsd [bucket[/prefix]]\n"
```

Add a `runServiceFunc` variable for testability:

```go
var runServiceFunc = runServiceWithDebug
```

Update `runMain` to handle `s3` and `dav` subcommands:

```go
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
default:
    return fmt.Errorf("unknown subcommand: %s", rest[0])
}
```

Update the no-subcommand case:

```go
if len(rest) == 0 {
    dbg.Printf("mode=all")
    return runServiceFunc(configPath, serverModeAll, dbg)
}
```

- [ ] **Step 4: Rename runWithDebug to runServiceWithDebug**

Rename `runWithDebug` to `runServiceWithDebug` and add a `mode serverMode` parameter. The actual mode-based handler selection will be added in Task 6. For now, accept the parameter but ignore it:

```go
func runServiceWithDebug(configPath string, mode serverMode, dbg debugLogger) error {
    // existing runWithDebug body unchanged for now
    ...
}
```

Update `run` function:

```go
func run(configPath string) error {
    return runServiceWithDebug(configPath, serverModeAll, newDebugLogger(false, io.Discard))
}
```

- [ ] **Step 5: Run tests**

Run: `go test ./cmd/tgnas/... -v`
Expected: All tests pass.

- [ ] **Step 6: Commit**

```bash
git add cmd/tgnas/main.go cmd/tgnas/main_test.go
git commit -m "feat: add s3/dav subcommand dispatch with serverMode"
```

---

## Task 5: Implement WebDAV FileSystem

**Files:**
- Create: `internal/dav/fs.go`
- Create: `internal/dav/fs_test.go`

This implements `webdav.FileSystem` backed by the metadata store. It maps WebDAV paths to bucket/key pairs, handles directory markers and implicit directories, buffers PUT bodies for object-store writes, and buffers GET bodies in a `bytes.Reader` so `http.ServeContent` can seek without Finder `Interrupted system call` / `seeker can't seek` failures.

- [ ] **Step 1: Define the FileSystem struct and constructor**

Create `internal/dav/fs.go`:

```go
package dav

import (
    "bytes"
    "context"
    "errors"
    "fmt"
    "io"
    "net/url"
    "os"
    "path"
    "strings"
    "time"

    "github.com/aahl/tgnas/metadata"
    "github.com/aahl/tgnas/store"
    "golang.org/x/net/webdav"
)

const maxRecursiveObjects = 100000

type ObjectStore interface {
    PutObject(ctx context.Context, input store.PutObjectInput) (store.PutObjectResult, error)
    GetObject(ctx context.Context, input store.GetObjectInput) (io.ReadCloser, store.ObjectInfo, error)
}

type FileSystem struct {
    meta        metadata.Store
    objectStore ObjectStore
}

func NewFileSystem(meta metadata.Store, objectStore ObjectStore) *FileSystem {
    return &FileSystem{meta: meta, objectStore: objectStore}
}
```

- [ ] **Step 2: Implement path parsing**

```go
var ErrBadRequest = errors.New("bad request")

func parsePath(p string) (bucket, key string, isRoot bool, err error) {
    decoded, err := url.PathUnescape(p)
    if err != nil {
        return "", "", false, ErrBadRequest
    }
    cleaned := path.Clean("/" + decoded)
    if cleaned == "/" {
        return "", "", true, nil
    }
    parts := strings.SplitN(strings.TrimPrefix(cleaned, "/"), "/", 2)
    bucket = parts[0]
    if bucket == "" {
        return "", "", true, nil
    }
    if len(parts) > 1 {
        key = parts[1]
        if strings.HasSuffix(decoded, "/") && key != "" && !strings.HasSuffix(key, "/") {
            key += "/"
        }
    }
    return bucket, key, false, nil
}

func isCollection(key string) bool {
    return key == "" || strings.HasSuffix(key, "/")
}
```

- [ ] **Step 3: Implement webdav.FileSystem methods**

```go
func (fs *FileSystem) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
    bucket, key, isRoot, err := parsePath(name)
    if err != nil {
        return err
    }
    if isRoot {
        return webdav.ErrForbidden
    }

    if err := fs.requireEnabledBucket(ctx, bucket); err != nil {
        return err
    }

    if key == "" {
        return webdav.ErrForbidden // MKCOL on bucket root
    }

    if !strings.HasSuffix(key, "/") {
        key += "/"
    }

    parent := parentPrefix(key)
    if parent != "" {
        _, err := fs.meta.HeadObject(ctx, bucket, parent)
        if err == metadata.ErrNotFound {
            return webdav.ErrConflict
        }
        if err != nil {
            return err
        }
    }

    return fs.meta.PutObject(ctx, metadata.Object{
        Bucket:       bucket,
        Key:          key,
        Size:         0,
        ContentType:  "httpd/unix-directory",
        LastModified: time.Now().UTC(),
    }, nil)
}

func (fs *FileSystem) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
    bucket, key, isRoot, err := parsePath(name)
    if err != nil {
        return nil, err
    }

    writing := flag&(os.O_CREATE|os.O_WRONLY|os.O_RDWR|os.O_TRUNC) != 0
    if writing && (isRoot || strings.HasSuffix(name, "/")) {
        return nil, webdav.ErrForbidden
    }

    if isRoot {
        return fs.openRootCollection(ctx)
    }

    if err := fs.requireEnabledBucket(ctx, bucket); err != nil {
        return nil, err
    }

    if writing {
        return &davFile{
            fs:      fs,
            ctx:     ctx,
            bucket:  bucket,
            key:     key,
            writeBuf: new(bytes.Buffer),
        }, nil
    }

    if key == "" || strings.HasSuffix(key, "/") {
        return fs.openCollection(ctx, bucket, key)
    }

    obj, chunks, err := fs.meta.GetObject(ctx, bucket, key)
    if err != nil {
        return nil, err
    }
    return &davFile{
        fs:     fs,
        ctx:    ctx,
        bucket: bucket,
        key:    key,
        object: &obj,
        chunks: chunks,
    }, nil
}

func (fs *FileSystem) RemoveAll(ctx context.Context, name string) error {
    bucket, key, isRoot, err := parsePath(name)
    if err != nil {
        return err
    }
    if isRoot {
        return webdav.ErrForbidden
    }

    if err := fs.requireEnabledBucket(ctx, bucket); err != nil {
        return err
    }

    if key == "" {
        return fs.deleteBucket(ctx, bucket)
    }

    if strings.HasSuffix(key, "/") {
        count, err := fs.meta.CountObjects(ctx, bucket, key)
        if err != nil {
            return err
        }
        if count > maxRecursiveObjects {
            return fmt.Errorf("recursive delete exceeds %d object limit", maxRecursiveObjects)
        }
        return fs.meta.DeletePrefix(ctx, bucket, key)
    }

    return fs.meta.DeleteObject(ctx, bucket, key)
}

func (fs *FileSystem) Rename(ctx context.Context, oldName, newName string) error {
    return webdav.ErrForbidden // handled by handler MOVE
}

func (fs *FileSystem) Stat(ctx context.Context, name string) (os.FileInfo, error) {
    bucket, key, isRoot, err := parsePath(name)
    if err != nil {
        return nil, err
    }
    if isRoot {
        return rootInfo(), nil
    }

    if err := fs.requireEnabledBucket(ctx, bucket); err != nil {
        return nil, err
    }

    if key == "" || strings.HasSuffix(key, "/") {
        return fs.statCollection(ctx, bucket, key)
    }

    obj, err := fs.meta.HeadObject(ctx, bucket, key)
    if err != nil {
        return nil, err
    }
    return &davFileInfo{
        name:    path.Base(key),
        size:    obj.Size,
        modTime: obj.LastModified,
        isDir:   false,
    }, nil
}
```

- [ ] **Step 4: Implement collection helpers**

```go
func (fs *FileSystem) openRootCollection(ctx context.Context) (webdav.File, error) {
    buckets, err := fs.meta.ListBuckets(ctx)
    if err != nil {
        return nil, err
    }
    var children []davFileInfo
    for _, b := range buckets {
        children = append(children, davFileInfo{name: b.Name, isDir: true, modTime: b.CreatedAt})
    }
    return &davFile{fs: fs, ctx: ctx, isRoot: true, children: children}, nil
}

func (fs *FileSystem) openCollection(ctx context.Context, bucket, prefix string) (webdav.File, error) {
    objects, err := fs.meta.ListAllObjects(ctx, bucket, prefix)
    if err != nil {
        return nil, err
    }
    seen := map[string]bool{}
    var children []davFileInfo

    for _, obj := range objects {
        remainder := strings.TrimPrefix(obj.Key, prefix)
        if remainder == "" {
            continue
        }
        if idx := strings.Index(remainder, "/"); idx >= 0 {
            dirName := remainder[:idx+1]
            if !seen[dirName] {
                seen[dirName] = true
                children = append(children, davFileInfo{name: dirName, isDir: true, modTime: obj.LastModified})
            }
        } else {
            children = append(children, davFileInfo{name: remainder, size: obj.Size, modTime: obj.LastModified, isDir: false})
        }
    }
    return &davFile{fs: fs, ctx: ctx, bucket: bucket, key: prefix, isDir: true, children: children}, nil
}

func (fs *FileSystem) statCollection(ctx context.Context, bucket, prefix string) (os.FileInfo, error) {
    if prefix == "" {
        return &davFileInfo{name: bucket, isDir: true}, nil
    }
    _, err := fs.meta.HeadObject(ctx, bucket, prefix)
    if err == nil {
        return &davFileInfo{name: path.Base(strings.TrimSuffix(prefix, "/")), isDir: true}, nil
    }
    objects, err := fs.meta.ListObjects(ctx, metadata.ListQuery{Bucket: bucket, Prefix: prefix, Limit: 1})
    if err != nil {
        return nil, err
    }
    if len(objects) > 0 {
        return &davFileInfo{name: path.Base(strings.TrimSuffix(prefix, "/")), isDir: true, modTime: objects[0].LastModified}, nil
    }
    return nil, webdav.ErrNotFound
}

func (fs *FileSystem) requireEnabledBucket(ctx context.Context, bucket string) error {
    b, err := fs.meta.GetBucket(ctx, bucket)
    if err != nil {
        if err == metadata.ErrNotFound {
            return webdav.ErrNotFound
        }
        return err
    }
    if !b.Enabled {
        return webdav.ErrForbidden // orphan bucket
    }
    return nil
}

func (fs *FileSystem) deleteBucket(ctx context.Context, bucket string) error {
    b, err := fs.meta.GetBucket(ctx, bucket)
    if err != nil {
        if err == metadata.ErrNotFound {
            return webdav.ErrNotFound
        }
        return err
    }
    if b.Enabled {
        return webdav.ErrForbidden
    }
    return fs.meta.DeleteBucket(ctx, bucket)
}

func parentPrefix(key string) string {
    trimmed := strings.TrimSuffix(key, "/")
    idx := strings.LastIndex(trimmed, "/")
    if idx < 0 {
        return ""
    }
    return trimmed[:idx+1]
}
```

- [ ] **Step 5: Implement davFile and davFileInfo**

```go
type davFile struct {
    fs       *FileSystem
    ctx      context.Context
    bucket   string
    key      string
    object   *metadata.Object
    chunks   []metadata.Chunk
    isRoot   bool
    isDir    bool
    children []davFileInfo
    readPos  int
    readBuf  *bytes.Reader
    writeBuf *bytes.Buffer
}

func (f *davFile) Read(p []byte) (int, error) {
    if f.isDir || f.object == nil {
        return 0, fmt.Errorf("is a directory")
    }
    if err := f.ensureReadBuffer(); err != nil {
        return 0, err
    }
    return f.readBuf.Read(p)
}

func (f *davFile) ensureReadBuffer() error {
    if f.readBuf != nil {
        return nil
    }
    if f.object.Size == 0 && len(f.chunks) == 0 {
        f.readBuf = bytes.NewReader(nil)
        return nil
    }
    rc, _, err := f.fs.objectStore.GetObject(f.ctx, store.GetObjectInput{Bucket: f.bucket, Key: f.key})
    if err != nil {
        return err
    }
    defer rc.Close()
    data, err := io.ReadAll(rc)
    if err != nil {
        return err
    }
    f.readBuf = bytes.NewReader(data)
    return nil
}

func (f *davFile) Write(p []byte) (int, error) {
    if f.writeBuf == nil {
        return 0, webdav.ErrForbidden
    }
    return f.writeBuf.Write(p)
}

func (f *davFile) Seek(offset int64, whence int) (int64, error) {
    if f.object == nil {
        return 0, webdav.ErrForbidden
    }
    if err := f.ensureReadBuffer(); err != nil {
        return 0, err
    }
    return f.readBuf.Seek(offset, whence)
}

func (f *davFile) Close() error {
    if f.writeBuf == nil {
        return nil
    }
    _, err := f.fs.objectStore.PutObject(f.ctx, store.PutObjectInput{
        Bucket: f.bucket,
        Key:    f.key,
        Size:   int64(f.writeBuf.Len()),
        Body:   bytes.NewReader(f.writeBuf.Bytes()),
    })
    return err
}

func (f *davFile) Readdir(count int) ([]os.FileInfo, error) {
    if !f.isDir && !f.isRoot {
        return nil, fmt.Errorf("not a directory")
    }
    start := f.readPos
    if start >= len(f.children) {
        if count > 0 {
            return nil, io.EOF
        }
        return nil, nil
    }
    end := len(f.children)
    if count > 0 && start+count < end {
        end = start + count
    }
    f.readPos = end
    result := make([]os.FileInfo, end-start)
    for i := start; i < end; i++ {
        result[i-start] = &f.children[i]
    }
    return result, nil
}

func (f *davFile) Stat() (os.FileInfo, error) {
    if f.isRoot {
        return rootInfo(), nil
    }
    if f.isDir {
        return &davFileInfo{name: path.Base(strings.TrimSuffix(f.key, "/")), isDir: true}, nil
    }
    if f.object != nil {
        return &davFileInfo{name: path.Base(f.key), size: f.object.Size, modTime: f.object.LastModified}, nil
    }
    return nil, webdav.ErrNotFound
}

func (f *davFile) WriteTo(w io.Writer) (int64, error) {
    if f.object == nil {
        return 0, fmt.Errorf("is a directory")
    }
    if err := f.ensureReadBuffer(); err != nil {
        return 0, err
    }
    return f.readBuf.WriteTo(w)
}

type davFileInfo struct {
    name    string
    size    int64
    modTime time.Time
    isDir   bool
}

func (fi *davFileInfo) Name() string      { return fi.name }
func (fi *davFileInfo) Size() int64        { return fi.size }
func (fi *davFileInfo) Mode() os.FileMode {
    if fi.isDir {
        return os.ModeDir | 0755
    }
    return 0644
}
func (fi *davFileInfo) ModTime() time.Time { return fi.modTime }
func (fi *davFileInfo) IsDir() bool        { return fi.isDir }
func (fi *davFileInfo) Sys() interface{}   { return nil }

func rootInfo() *davFileInfo {
    return &davFileInfo{name: "/", isDir: true}
}
```

- [ ] **Step 6: Write FileSystem tests**

Create `internal/dav/fs_test.go` with tests using temporary SQLite:

```go
package dav

import (
    "context"
    "errors"
    "io"
    "os"
    "strings"
    "testing"
    "time"

    "github.com/aahl/tgnas/metadata"
    "github.com/aahl/tgnas/store"
    "golang.org/x/net/webdav"
)

type fakeObjectStore struct {
    objects map[string]string
}

func (s fakeObjectStore) PutObject(ctx context.Context, input store.PutObjectInput) (store.PutObjectResult, error) {
    data, err := io.ReadAll(input.Body)
    if err != nil {
        return store.PutObjectResult{}, err
    }
    if s.objects == nil {
        return store.PutObjectResult{}, errors.New("objects map is nil")
    }
    s.objects[input.Bucket+"/"+input.Key] = string(data)
    return store.PutObjectResult{ETag: "etag"}, nil
}

func (s fakeObjectStore) GetObject(ctx context.Context, input store.GetObjectInput) (io.ReadCloser, store.ObjectInfo, error) {
    value, ok := s.objects[input.Bucket+"/"+input.Key]
    if !ok {
        return nil, store.ObjectInfo{}, store.ErrNoSuchKey
    }
    info := store.ObjectInfo{Bucket: input.Bucket, Key: input.Key, Size: int64(len(value)), LastModified: time.Now().UTC()}
    return io.NopCloser(strings.NewReader(value)), info, nil
}

func openTestFS(t *testing.T) (*FileSystem, metadata.Store, fakeObjectStore) {
    t.Helper()
    tmp := t.TempDir()
    dbPath := tmp + "/test.sqlite"
    meta, err := metadata.OpenSQLite(dbPath)
    if err != nil {
        t.Fatal(err)
    }
    t.Cleanup(func() { meta.Close() })
    objectStore := fakeObjectStore{objects: map[string]string{}}
    return NewFileSystem(meta, objectStore), meta, objectStore
}

func seedBucket(t *testing.T, meta metadata.Store, name string) {
    t.Helper()
    err := meta.UpsertBucket(context.Background(), metadata.Bucket{
        Name: name, ChatID: "123", CreatedAt: time.Now().UTC(), Enabled: true,
    })
    if err != nil {
        t.Fatal(err)
    }
}

func TestMkdirCreatesMarker(t *testing.T) {
    fs, meta, _ := openTestFS(t)
    ctx := context.Background()
    seedBucket(t, meta, "photos")

    err := fs.Mkdir(ctx, "/photos/2026/", 0755)
    if err != nil {
        t.Fatalf("Mkdir: %v", err)
    }

    obj, err := meta.HeadObject(ctx, "photos", "2026/")
    if err != nil {
        t.Fatalf("HeadObject: %v", err)
    }
    if obj.Size != 0 {
        t.Fatalf("expected zero-byte marker, got size %d", obj.Size)
    }
}

func TestMkdirBucketRootForbidden(t *testing.T) {
    fs, meta, _ := openTestFS(t)
    ctx := context.Background()
    seedBucket(t, meta, "photos")

    err := fs.Mkdir(ctx, "/photos", 0755)
    if err != webdav.ErrForbidden {
        t.Fatalf("expected ErrForbidden, got %v", err)
    }
}

func TestStatRoot(t *testing.T) {
    fs, _, _ := openTestFS(t)
    ctx := context.Background()

    info, err := fs.Stat(ctx, "/")
    if err != nil {
        t.Fatalf("Stat /: %v", err)
    }
    if !info.IsDir() {
        t.Fatal("expected root to be a directory")
    }
}

func TestStatBucket(t *testing.T) {
    fs, meta, _ := openTestFS(t)
    ctx := context.Background()
    seedBucket(t, meta, "photos")

    info, err := fs.Stat(ctx, "/photos/")
    if err != nil {
        t.Fatalf("Stat /photos/: %v", err)
    }
    if !info.IsDir() {
        t.Fatal("expected bucket to be a directory")
    }
}

func TestStatUnknownBucket(t *testing.T) {
    fs, _, _ := openTestFS(t)
    ctx := context.Background()

    _, err := fs.Stat(ctx, "/unknown/")
    if err != webdav.ErrNotFound {
        t.Fatalf("expected ErrNotFound, got %v", err)
    }
}

func TestPutToPathEndingInSlashRejected(t *testing.T) {
    fs, meta, _ := openTestFS(t)
    ctx := context.Background()
    seedBucket(t, meta, "photos")

    _, err := fs.OpenFile(ctx, "/photos/dir/", os.O_CREATE|os.O_WRONLY, 0644)
    if err == nil {
        t.Fatal("expected error for PUT to path ending in /")
    }
}

func TestOpenedObjectStatReturnsMetadata(t *testing.T) {
    fs, meta, _ := openTestFS(t)
    ctx := context.Background()
    seedBucket(t, meta, "photos")
    obj := metadata.Object{Bucket: "photos", Key: "test.txt", Size: 5, LastModified: time.Unix(10, 0)}
    if err := meta.PutObject(ctx, obj, nil); err != nil {
        t.Fatalf("PutObject: %v", err)
    }

    f, err := fs.OpenFile(ctx, "/photos/test.txt", os.O_RDONLY, 0644)
    if err != nil {
        t.Fatalf("OpenFile: %v", err)
    }
    info, err := f.Stat()
    if err != nil {
        t.Fatalf("Stat: %v", err)
    }
    if info.Name() != "test.txt" || info.Size() != 5 {
        t.Fatalf("info = %+v", info)
    }
}
```

- [ ] **Step 7: Run tests**

Run: `go test ./internal/dav/... -v`
Expected: All tests pass.

- [ ] **Step 8: Commit**

```bash
git add internal/dav/
git commit -m "feat: add webdav filesystem backed by metadata store"
```

---

## Task 6: Implement WebDAV HTTP Handler

**Files:**
- Create: `internal/dav/handler.go`
- Create: `internal/dav/lock.go`
- Create: `internal/dav/handler_test.go`

This wraps `webdav.Handler` with Basic Auth, Finder-compatible class-2 locking, `OPTIONS` capability advertisement, missing-file `PROPFIND` log filtering, orphan bucket checks, COPY/MOVE logic, and routing.

- [ ] **Step 1: Create lock.go — lightweight in-memory LockSystem**

Create a compatibility lock system for Finder and other class-2 WebDAV clients. It does not persist lock state, but it must let `LOCK` → `PUT` → `UNLOCK` write flows complete through `x/net/webdav`.

```go
package dav

import (
    "fmt"
    "sync/atomic"
    "time"

    "golang.org/x/net/webdav"
)

type noLockSystem struct {
    next atomic.Uint64
}

func (noLockSystem) Confirm(now time.Time, name0, name1 string, conditions ...webdav.Condition) (func(), error) {
    return func() {}, nil
}

func (ls *noLockSystem) Create(now time.Time, details webdav.LockDetails) (string, error) {
    return fmt.Sprintf("opaquelocktoken:tgnas-%d", ls.next.Add(1)), nil
}

func (noLockSystem) Refresh(now time.Time, token string, duration time.Duration) (webdav.LockDetails, error) {
    return webdav.LockDetails{}, webdav.ErrNoSuchLock
}

func (noLockSystem) Unlock(now time.Time, token string) error {
    return nil
}
```

- [ ] **Step 2: Create handler.go**

```go
package dav

import (
    "errors"
    "fmt"
    "io"
    "log"
    "net/http"
    "net/url"
    "os"
    "strings"

    "github.com/aahl/tgnas/metadata"
    "golang.org/x/net/webdav"
)

type HandlerOptions struct {
    Prefix      string
    Credentials map[string]string
    Logger      *log.Logger
}

type Handler struct {
    prefix   string
    creds    map[string]string
    handler  webdav.Handler
    fs       *FileSystem
    meta     metadata.Store
    logger   *log.Logger
}

func NewHandler(meta metadata.Store, fs *FileSystem, opts HandlerOptions) *Handler {
    prefix := opts.Prefix
    if prefix == "" {
        prefix = "/dav/"
    }
    if !strings.HasPrefix(prefix, "/") {
        prefix = "/" + prefix
    }
    if !strings.HasSuffix(prefix, "/") {
        prefix += "/"
    }
    logger := opts.Logger
    if logger == nil {
        logger = log.New(io.Discard, "", 0)
    }
    h := &Handler{prefix: prefix, creds: opts.Credentials, fs: fs, meta: meta, logger: logger}
    h.handler = webdav.Handler{
        Prefix:     prefix,
        FileSystem: fs,
        LockSystem: &noLockSystem{},
        Logger: func(r *http.Request, err error) {
            if err != nil && !errors.Is(err, os.ErrNotExist) {
                logger.Printf("webdav method=%q path=%q error=%q", r.Method, r.URL.Path, err.Error())
            }
        },
    }
    return h
}

func (h *Handler) requestPath(r *http.Request) (string, error) {
    if !strings.HasPrefix(r.URL.Path, h.prefix) {
        return "", ErrNotFound
    }
    return "/" + strings.TrimPrefix(r.URL.Path, h.prefix), nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    if !strings.HasPrefix(r.URL.Path, h.prefix) {
        http.NotFound(w, r)
        return
    }

    if !h.checkBasicAuth(w, r) {
        return
    }

    if r.Method == "OPTIONS" {
        h.handleOptions(w, r)
        return
    }
    if r.Method == "PROPFIND" {
        depth := r.Header.Get("Depth")
        if strings.EqualFold(depth, "infinity") {
            http.Error(w, "Depth infinity is forbidden", http.StatusForbidden)
            return
        }
        if depth == "" {
            if davPath, err := h.requestPath(r); err == nil {
                if info, statErr := h.fs.Stat(r.Context(), davPath); statErr == nil && !info.IsDir() {
                    r.Header.Set("Depth", "0")
                } else {
                    r.Header.Set("Depth", "1")
                }
            }
        }
    }

    davPath := strings.TrimPrefix(r.URL.Path, h.prefix)
    davPath = "/" + davPath

    bucket, key, isRoot, _ := parsePath(davPath)

    if !isRoot {
        b, err := h.meta.GetBucket(r.Context(), bucket)
        if err != nil && err != metadata.ErrNotFound {
            http.Error(w, "internal error", http.StatusInternalServerError)
            return
        }
        if err == metadata.ErrNotFound {
            http.Error(w, "not found", http.StatusNotFound)
            return
        }
        if !b.Enabled {
            if r.Method == http.MethodDelete && (key == "" || key == "/") {
                h.handleOrphanDelete(w, r, bucket)
                return
            }
            http.Error(w, "forbidden", http.StatusForbidden)
            return
        }
    }

    switch r.Method {
    case "COPY":
        h.handleCopy(w, r, davPath)
        return
    case "MOVE":
        h.handleMove(w, r, davPath)
        return
    }

    h.handler.ServeHTTP(w, r)
}

func (h *Handler) checkBasicAuth(w http.ResponseWriter, r *http.Request) bool {
    user, pass, ok := r.BasicAuth()
    if !ok {
        w.Header().Set("WWW-Authenticate", `Basic realm="tgnas"`)
        http.Error(w, "unauthorized", http.StatusUnauthorized)
        return false
    }
    expectedPass, exists := h.creds[user]
    if !exists || expectedPass != pass {
        w.Header().Set("WWW-Authenticate", `Basic realm="tgnas"`)
        http.Error(w, "unauthorized", http.StatusUnauthorized)
        return false
    }
    return true
}

func (h *Handler) handleOptions(w http.ResponseWriter, r *http.Request) {
    davPath := "/" + strings.TrimPrefix(r.URL.Path, h.prefix)
    allow := "OPTIONS, PUT, MKCOL, LOCK, UNLOCK, PROPFIND"
    if info, err := h.fs.Stat(r.Context(), davPath); err == nil {
        if info.IsDir() {
            allow = "OPTIONS, PUT, MKCOL, DELETE, PROPPATCH, COPY, MOVE, LOCK, UNLOCK, PROPFIND"
        } else {
            allow = "OPTIONS, GET, HEAD, POST, DELETE, PROPPATCH, COPY, MOVE, LOCK, UNLOCK, PROPFIND, PUT"
        }
    }
    w.Header().Set("Allow", allow)
    w.Header().Set("DAV", "1, 2")
    w.Header().Set("MS-Author-Via", "DAV")
    w.WriteHeader(http.StatusOK)
}

func (h *Handler) handleOrphanDelete(w http.ResponseWriter, r *http.Request, bucket string) {
    if err := h.meta.DeleteBucket(r.Context(), bucket); err != nil {
        http.Error(w, "internal error", http.StatusInternalServerError)
        return
    }
    w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleCopy(w http.ResponseWriter, r *http.Request, srcPath string) {
    dstURL := r.Header.Get("Destination")
    if dstURL == "" {
        http.Error(w, "missing Destination", http.StatusBadRequest)
        return
    }
    dstPath, err := h.resolveDestination(dstURL, r)
    if err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

    srcBucket, srcKey, srcIsRoot, _ := parsePath(srcPath)
    dstBucket, dstKey, dstIsRoot, _ := parsePath(dstPath)

    if srcIsRoot || dstIsRoot {
        http.Error(w, "forbidden", http.StatusForbidden)
        return
    }
    if srcBucket != dstBucket {
        http.Error(w, "cross-bucket copy forbidden", http.StatusForbidden)
        return
    }

    overwrite, err := parseOverwrite(r.Header.Get("Overwrite"))
    if err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

    if strings.HasSuffix(srcKey, "/") || srcKey == "" {
        if srcKey == "" {
            http.Error(w, "forbidden", http.StatusForbidden)
            return
        }
        if isSubpath(srcKey, dstKey) {
            http.Error(w, "cannot copy into own subtree", http.StatusForbidden)
            return
        }
        count, err := h.meta.CountObjects(r.Context(), srcBucket, srcKey)
        if err != nil || count > maxRecursiveObjects {
            http.Error(w, "too many objects", http.StatusInternalServerError)
            return
        }
        result, err := h.meta.CopyPrefix(r.Context(), srcBucket, srcKey, dstKey, metadata.CopyOptions{Overwrite: overwrite})
        if err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        if result.Created {
            w.WriteHeader(http.StatusCreated)
        } else {
            w.WriteHeader(http.StatusNoContent)
        }
    } else {
        result, err := h.meta.CopyObject(r.Context(), srcBucket, srcKey, dstKey, metadata.CopyOptions{Overwrite: overwrite})
        if err != nil {
            if strings.Contains(err.Error(), "already exists") {
                http.Error(w, err.Error(), http.StatusPreconditionFailed)
                return
            }
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        if result.Created {
            w.WriteHeader(http.StatusCreated)
        } else {
            w.WriteHeader(http.StatusNoContent)
        }
    }
}

func (h *Handler) handleMove(w http.ResponseWriter, r *http.Request, srcPath string) {
    dstURL := r.Header.Get("Destination")
    if dstURL == "" {
        http.Error(w, "missing Destination", http.StatusBadRequest)
        return
    }
    dstPath, err := h.resolveDestination(dstURL, r)
    if err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

    srcBucket, srcKey, srcIsRoot, _ := parsePath(srcPath)
    dstBucket, dstKey, dstIsRoot, _ := parsePath(dstPath)

    if srcIsRoot || dstIsRoot {
        http.Error(w, "forbidden", http.StatusForbidden)
        return
    }
    if srcBucket != dstBucket {
        http.Error(w, "cross-bucket move forbidden", http.StatusForbidden)
        return
    }

    overwrite, err := parseOverwrite(r.Header.Get("Overwrite"))
    if err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

    if strings.HasSuffix(srcKey, "/") || srcKey == "" {
        if srcKey == "" {
            http.Error(w, "forbidden", http.StatusForbidden)
            return
        }
        if isSubpath(srcKey, dstKey) {
            http.Error(w, "cannot move into own subtree", http.StatusForbidden)
            return
        }
        count, err := h.meta.CountObjects(r.Context(), srcBucket, srcKey)
        if err != nil || count > maxRecursiveObjects {
            http.Error(w, "too many objects", http.StatusInternalServerError)
            return
        }
        result, err := h.meta.MovePrefix(r.Context(), srcBucket, srcKey, dstKey, metadata.MoveOptions{Overwrite: overwrite})
        if err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        if result.Created {
            w.WriteHeader(http.StatusCreated)
        } else {
            w.WriteHeader(http.StatusNoContent)
        }
    } else {
        result, err := h.meta.MoveObject(r.Context(), srcBucket, srcKey, dstKey, metadata.MoveOptions{Overwrite: overwrite})
        if err != nil {
            if strings.Contains(err.Error(), "already exists") {
                http.Error(w, err.Error(), http.StatusPreconditionFailed)
                return
            }
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        if result.Created {
            w.WriteHeader(http.StatusCreated)
        } else {
            w.WriteHeader(http.StatusNoContent)
        }
    }
}

func (h *Handler) resolveDestination(dstURL string, r *http.Request) (string, error) {
    u, err := url.Parse(dstURL)
    if err != nil {
        return "", fmt.Errorf("invalid Destination URL")
    }
    if u.Host != "" && u.Host != r.Host {
        return "", fmt.Errorf("cross-host destination forbidden")
    }
    p := u.Path
    if !strings.HasPrefix(p, h.prefix) {
        return "", fmt.Errorf("destination path outside webdav prefix")
    }
    p = strings.TrimPrefix(p, h.prefix)
    return "/" + p, nil
}

func parseOverwrite(value string) (bool, error) {
    switch strings.ToUpper(strings.TrimSpace(value)) {
    case "", "T":
        return true, nil
    case "F":
        return false, nil
    default:
        return false, fmt.Errorf("unsupported Overwrite header %q", value)
    }
}

func isSubpath(parent, child string) bool {
    if !strings.HasSuffix(parent, "/") {
        parent += "/"
    }
    return strings.HasPrefix(child, parent)
}
```

- [ ] **Step 3: Write handler tests**

Create `internal/dav/handler_test.go`:

```go
package dav

import (
    "context"
    "errors"
    "log"
    "net/http"
    "net/http/httptest"
    "os"
    "strings"
    "testing"
    "time"

    "github.com/aahl/tgnas/metadata"
)

func setupHandler(t *testing.T) (*Handler, metadata.Store, fakeObjectStore) {
    t.Helper()
    fs, meta, objectStore := openTestFS(t)
    h := NewHandler(meta, fs, HandlerOptions{
        Prefix:      "/dav/",
        Credentials: map[string]string{"admin": "secret"},
        Logger:      nil,
    })
    return h, meta, objectStore
}

func TestHandlerRequiresBasicAuth(t *testing.T) {
    h, _, _ := setupHandler(t)
    req := httptest.NewRequest("GET", "/dav/", nil)
    w := httptest.NewRecorder()
    h.ServeHTTP(w, req)
    if w.Code != http.StatusUnauthorized {
        t.Fatalf("expected 401, got %d", w.Code)
    }
}

func TestHandlerRejectsWrongAuth(t *testing.T) {
    h, _, _ := setupHandler(t)
    req := httptest.NewRequest("GET", "/dav/", nil)
    req.SetBasicAuth("admin", "wrong")
    w := httptest.NewRecorder()
    h.ServeHTTP(w, req)
    if w.Code != http.StatusUnauthorized {
        t.Fatalf("expected 401, got %d", w.Code)
    }
}

func TestHandlerAcceptsValidAuth(t *testing.T) {
    h, meta, _ := setupHandler(t)
    ctx := context.Background()
    meta.UpsertBucket(ctx, metadata.Bucket{Name: "photos", ChatID: "123", CreatedAt: time.Now().UTC(), Enabled: true})

    req := httptest.NewRequest("PROPFIND", "/dav/", nil)
    req.SetBasicAuth("admin", "secret")
    w := httptest.NewRecorder()
    h.ServeHTTP(w, req)
    if w.Code == http.StatusUnauthorized {
        t.Fatal("expected auth to succeed")
    }
}

func TestHandlerReturns404ForUnknownBucket(t *testing.T) {
    h, _, _ := setupHandler(t)
    req := httptest.NewRequest("GET", "/dav/unknown/file.txt", nil)
    req.SetBasicAuth("admin", "secret")
    w := httptest.NewRecorder()
    h.ServeHTTP(w, req)
    if w.Code != http.StatusNotFound {
        t.Fatalf("expected 404, got %d", w.Code)
    }
}

func TestHandlerOrphanBucketReturns403(t *testing.T) {
    h, meta, _ := setupHandler(t)
    ctx := context.Background()
    meta.UpsertBucket(ctx, metadata.Bucket{Name: "orphan", ChatID: "123", CreatedAt: time.Now().UTC(), Enabled: false})

    req := httptest.NewRequest("GET", "/dav/orphan/file.txt", nil)
    req.SetBasicAuth("admin", "secret")
    w := httptest.NewRecorder()
    h.ServeHTTP(w, req)
    if w.Code != http.StatusForbidden {
        t.Fatalf("expected 403, got %d", w.Code)
    }
}

func TestHandlerOrphanDeleteSucceeds(t *testing.T) {
    h, meta, _ := setupHandler(t)
    ctx := context.Background()
    meta.UpsertBucket(ctx, metadata.Bucket{Name: "orphan", ChatID: "123", CreatedAt: time.Now().UTC(), Enabled: false})

    req := httptest.NewRequest("DELETE", "/dav/orphan", nil)
    req.SetBasicAuth("admin", "secret")
    w := httptest.NewRecorder()
    h.ServeHTTP(w, req)
    if w.Code != http.StatusNoContent {
        t.Fatalf("expected 204, got %d", w.Code)
    }
}

func TestHandlerCopyMissingDestination(t *testing.T) {
    h, meta, _ := setupHandler(t)
    ctx := context.Background()
    meta.UpsertBucket(ctx, metadata.Bucket{Name: "b1", ChatID: "123", CreatedAt: time.Now().UTC(), Enabled: true})

    req := httptest.NewRequest("COPY", "/dav/b1/src.txt", nil)
    req.SetBasicAuth("admin", "secret")
    w := httptest.NewRecorder()
    h.ServeHTTP(w, req)
    if w.Code != http.StatusBadRequest {
        t.Fatalf("expected 400, got %d", w.Code)
    }
}

func TestHandlerOutsidePrefixReturns404(t *testing.T) {
    h, _, _ := setupHandler(t)
    req := httptest.NewRequest("GET", "/other/path", nil)
    req.SetBasicAuth("admin", "secret")
    w := httptest.NewRecorder()
    h.ServeHTTP(w, req)
    if w.Code != http.StatusNotFound {
        t.Fatalf("expected 404, got %d", w.Code)
    }
}

func TestHandlerOptionsAdvertisesLocks(t *testing.T) {
    h, meta, _ := setupHandler(t)
    ctx := context.Background()
    meta.UpsertBucket(ctx, metadata.Bucket{Name: "photos", ChatID: "123", CreatedAt: time.Now().UTC(), Enabled: true})

    req := httptest.NewRequest("OPTIONS", "/dav/photos/", nil)
    req.SetBasicAuth("admin", "secret")
    w := httptest.NewRecorder()
    h.ServeHTTP(w, req)
    if w.Code != http.StatusOK {
        t.Fatalf("expected 200, got %d", w.Code)
    }
    if got := w.Header().Get("DAV"); got != "1, 2" {
        t.Fatalf("DAV = %q", got)
    }
    allow := w.Header().Get("Allow")
    if !strings.Contains(allow, "LOCK") || !strings.Contains(allow, "UNLOCK") || !strings.Contains(allow, "PUT") {
        t.Fatalf("Allow = %q", allow)
    }
}

func TestHandlerPropfindMissingFileDoesNotLogError(t *testing.T) {
    var logs strings.Builder
    h, meta, _ := setupHandler(t)
    h.logger = log.New(&logs, "", 0)
    h.handler.Logger = func(r *http.Request, err error) {
        if err != nil && !errors.Is(err, os.ErrNotExist) {
            h.logger.Printf("webdav method=%q path=%q error=%q", r.Method, r.URL.Path, err.Error())
        }
    }
    ctx := context.Background()
    meta.UpsertBucket(ctx, metadata.Bucket{Name: "photos", ChatID: "123", CreatedAt: time.Now().UTC(), Enabled: true})

    req := httptest.NewRequest("PROPFIND", "/dav/photos/missing.txt", strings.NewReader(`<?xml version="1.0"?><propfind xmlns="DAV:"><allprop/></propfind>`))
    req.SetBasicAuth("admin", "secret")
    w := httptest.NewRecorder()
    h.ServeHTTP(w, req)
    if w.Code != http.StatusNotFound {
        t.Fatalf("expected 404, got %d", w.Code)
    }
    if strings.Contains(logs.String(), "missing.txt") {
        t.Fatalf("unexpected log output: %s", logs.String())
    }
}

func TestHandlerLockPutUnlockWritesObject(t *testing.T) {
    h, meta, objectStore := setupHandler(t)
    ctx := context.Background()
    meta.UpsertBucket(ctx, metadata.Bucket{Name: "photos", ChatID: "123", CreatedAt: time.Now().UTC(), Enabled: true})

    lockBody := strings.NewReader(`<?xml version="1.0"?><lockinfo xmlns="DAV:"><lockscope><exclusive/></lockscope><locktype><write/></locktype><owner>finder</owner></lockinfo>`)
    lockReq := httptest.NewRequest("LOCK", "/dav/photos/new.txt", lockBody)
    lockReq.SetBasicAuth("admin", "secret")
    lockReq.Header.Set("Depth", "0")
    lockW := httptest.NewRecorder()
    h.ServeHTTP(lockW, lockReq)
    if lockW.Code != http.StatusCreated {
        t.Fatalf("LOCK expected 201, got %d body=%s", lockW.Code, lockW.Body.String())
    }
    token := lockW.Header().Get("Lock-Token")
    if token == "" {
        t.Fatal("missing Lock-Token")
    }

    putReq := httptest.NewRequest("PUT", "/dav/photos/new.txt", strings.NewReader("hello"))
    putReq.SetBasicAuth("admin", "secret")
    putReq.Header.Set("If", "(<"+strings.Trim(token, "<>")+">)")
    putW := httptest.NewRecorder()
    h.ServeHTTP(putW, putReq)
    if putW.Code/100 != 2 {
        t.Fatalf("PUT expected 2xx, got %d body=%s", putW.Code, putW.Body.String())
    }

    unlockReq := httptest.NewRequest("UNLOCK", "/dav/photos/new.txt", nil)
    unlockReq.SetBasicAuth("admin", "secret")
    unlockReq.Header.Set("Lock-Token", token)
    unlockW := httptest.NewRecorder()
    h.ServeHTTP(unlockW, unlockReq)
    if unlockW.Code != http.StatusNoContent {
        t.Fatalf("UNLOCK expected 204, got %d body=%s", unlockW.Code, unlockW.Body.String())
    }

    if got := objectStore.objects["photos/new.txt"]; got != "hello" {
        t.Fatalf("stored body = %q, want hello", got)
    }
    obj, err := meta.HeadObject(ctx, "photos", "new.txt")
    if err != nil {
        t.Fatalf("HeadObject: %v", err)
    }
    if obj.Size != 5 {
        t.Fatalf("size = %d", obj.Size)
    }
}
```

The `LOCK` → `PUT` → `UNLOCK` test protects Finder class-2 writes from regressing into `read-only file system` behavior. The missing-file `PROPFIND` test protects routine Finder probes from noisy WebDAV error logs. Add `TestHandlerPropfindSkipsInconsistentChildWithoutDoubleWriteHeader` with a panic-on-double-`WriteHeader` recorder to guard the `PROPFIND allprop` path against `http: superfluous response.WriteHeader` regressions.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/dav/... -v`
Expected: All tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/dav/
git commit -m "feat: add webdav http handler with auth and copy/move"
```

---

## Task 7: Add Zero-Byte Object Store Support

**Files:**
- Modify: `store/store.go`
- Test: `store/store_test.go`

Finder may create placeholder files with `PUT` and `Content-Length: 0`. Telegram rejects empty media with `Bad Request: file must be non-empty`, so the object store must represent zero-byte files as metadata-only objects and skip Telegram upload.

- [ ] **Step 1: Write the failing zero-byte regression test**

Add to `store/store_test.go`:

```go
func TestStorePutZeroByteObjectStoresMetadataWithoutTelegramUpload(t *testing.T) {
    ctx := context.Background()
    meta, err := metadata.OpenSQLite(filepath.Join(t.TempDir(), "metadata.sqlite"))
    if err != nil {
        t.Fatalf("OpenSQLite returned error: %v", err)
    }
    defer meta.Close()
    if err := meta.UpsertBucket(ctx, metadata.Bucket{Name: "photos", ChatID: "-100", CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
        t.Fatalf("UpsertBucket returned error: %v", err)
    }
    fake := testutil.NewFakeTelegram()
    objectStore := mustNewObjectStore(t, meta, fake, Options{Upload: DefaultUploadConfig()})

    result, err := objectStore.PutObject(ctx, PutObjectInput{Bucket: "photos", Key: "empty.txt", ContentType: "text/plain", Size: 0, Body: strings.NewReader("")})
    if err != nil {
        t.Fatalf("PutObject returned error: %v", err)
    }
    if result.ETag != "d41d8cd98f00b204e9800998ecf8427e" {
        t.Fatalf("etag = %q", result.ETag)
    }
    if len(fake.Uploads) != 0 {
        t.Fatalf("uploads = %+v, want none", fake.Uploads)
    }
    object, chunks, err := meta.GetObject(ctx, "photos", "empty.txt")
    if err != nil {
        t.Fatalf("GetObject returned error: %v", err)
    }
    if object.Size != 0 || object.ChunkCount != 0 || object.ContentType != "text/plain" {
        t.Fatalf("object = %+v", object)
    }
    if len(chunks) != 0 {
        t.Fatalf("chunks = %+v, want none", chunks)
    }
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./store -run TestStorePutZeroByteObjectStoresMetadataWithoutTelegramUpload -v`
Expected: FAIL because the store attempts a Telegram document upload for the empty body.

- [ ] **Step 3: Implement metadata-only empty object writes**

In `store/store.go`, after resolving upload strategy and logging the put decision, branch before `putSingle`/`putChunked`:

```go
if input.Size == 0 {
    return s.putEmpty(ctx, input, strategy)
}
```

Add:

```go
func (s *ObjectStore) putEmpty(ctx context.Context, input PutObjectInput, strategy UploadStrategy) (PutObjectResult, error) {
    etag := hex.EncodeToString(md5.New().Sum(nil))
    shaSum := hex.EncodeToString(sha256.New().Sum(nil))
    object := metadata.Object{
        Bucket:         input.Bucket,
        Key:            input.Key,
        Size:           0,
        ContentType:    input.ContentType,
        ETag:           etag,
        SHA256:         shaSum,
        LastModified:   time.Now().UTC(),
        ChunkCount:     0,
        TelegramType:   strategy.TelegramType,
        UploadStrategy: strategy.UploadStrategy,
    }
    if err := s.meta.PutObject(ctx, object, nil); err != nil {
        s.logMetadataPutObject(input.Bucket, input.Key, 0, etag, err)
        return PutObjectResult{}, err
    }
    s.logMetadataPutObject(input.Bucket, input.Key, 0, etag, nil)
    return PutObjectResult{ETag: etag}, nil
}
```

This stores `ChunkCount: 0`, empty-content hashes, and no Telegram chunks. No Telegram upload is attempted, avoiding `file must be non-empty` errors.

- [ ] **Step 4: Run focused and integration tests**

Run:

```bash
go test ./store -run TestStorePutZeroByteObjectStoresMetadataWithoutTelegramUpload -v
go test ./internal/dav -run 'TestHandler(LockPutUnlockWritesObject|PutWritesObject)' -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add store/store.go store/store_test.go
git commit -m "fix: store zero-byte objects without telegram upload"
```

---

## Task 8: Fix SQLite Read-Only Relative Paths for Local CLI

**Files:**
- Modify: `metadata/sqlite.go`
- Test: `metadata/sqlite_test.go`

`tgnas ls` and `tgnas lsd` open SQLite read-only without starting the server or contacting Telegram. When the config uses the default relative path `data/metadata.sqlite`, the read-only `file:` URI must be built from an absolute path or the SQLite driver can fail with `SQL logic error: out of memory (1)`.

- [ ] **Step 1: Write the failing read-only relative path regression test**

Add to `metadata/sqlite_test.go`:

```go
func TestOpenSQLiteReadOnlyOpensExistingRelativeDatabase(t *testing.T) {
    dir := t.TempDir()
    t.Chdir(dir)
    if err := os.Mkdir("data", 0o755); err != nil {
        t.Fatalf("Mkdir returned error: %v", err)
    }
    path := filepath.Join("data", "metadata.sqlite")
    writable, err := OpenSQLite(path)
    if err != nil {
        t.Fatalf("OpenSQLite returned error: %v", err)
    }
    if err := writable.UpsertBucket(t.Context(), Bucket{Name: "photos", ChatID: "-100", CreatedAt: time.Unix(10, 0), Enabled: true}); err != nil {
        t.Fatalf("UpsertBucket returned error: %v", err)
    }
    if err := writable.Close(); err != nil {
        t.Fatalf("Close returned error: %v", err)
    }

    readonly, err := OpenSQLiteReadOnly(path)
    if err != nil {
        t.Fatalf("OpenSQLiteReadOnly returned error: %v", err)
    }
    defer readonly.Close()

    buckets, err := readonly.ListBuckets(t.Context())
    if err != nil {
        t.Fatalf("ListBuckets returned error: %v", err)
    }
    if len(buckets) != 1 || buckets[0].Name != "photos" {
        t.Fatalf("buckets = %+v", buckets)
    }
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./metadata -run TestOpenSQLiteReadOnlyOpensExistingRelativeDatabase -v`
Expected: FAIL with `OpenSQLiteReadOnly returned error: SQL logic error: out of memory (1)` before the fix.

- [ ] **Step 3: Convert read-only SQLite paths to absolute paths**

In `metadata/sqlite.go`, import `path/filepath` and update `OpenSQLiteReadOnly`:

```go
func OpenSQLiteReadOnly(path string) (*SQLiteStore, error) {
    absolutePath, err := filepath.Abs(path)
    if err != nil {
        return nil, err
    }
    return openSQLite(sqliteReadOnlyDSN(absolutePath))
}
```

Keep `sqliteReadOnlyDSN` responsible only for creating the `file:` URI and setting `mode=ro`.

- [ ] **Step 4: Run focused tests and CLI smoke checks**

Run:

```bash
go test ./metadata -run TestOpenSQLiteReadOnlyOpensExistingRelativeDatabase -v
go run ./cmd/tgnas -debug lsd
go run ./cmd/tgnas -debug ls -n 1 <existing-bucket>
```

Expected: tests pass, `lsd`/`ls` print debug `sqlite_path="data/metadata.sqlite"` and do not fail with `out of memory`. Replace `<existing-bucket>` with a bucket already present in the local metadata database used by `data/config.yaml`.

- [ ] **Step 5: Commit**

```bash
git add metadata/sqlite.go metadata/sqlite_test.go
git commit -m "fix: open read-only sqlite relative paths"
```

---

## Task 9: Compose Combined S3 + WebDAV Server

**Files:**
- Modify: `cmd/tgnas/main.go`

- [ ] **Step 1: Update runServiceWithDebug to accept mode**

Replace the existing `runServiceWithDebug` to build handlers based on mode:

```go
func runServiceWithDebug(configPath string, mode serverMode, dbg debugLogger) error {
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
            Name: name, ChatID: bucket.ChatID, CreatedAt: time.Now().UTC(), Enabled: true,
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

    var handler http.Handler

    s3Handler := s3api.NewServer(objectStore, s3api.Options{
        Region: cfg.Auth.Region, Credentials: secrets, Ready: ready.Load, Logger: dbg.StdLogger(),
    })
    fs := dav.NewFileSystem(meta, objectStore)
    davHandler := dav.NewHandler(meta, fs, dav.HandlerOptions{
        Prefix: cfg.WebDAV.Prefix, Credentials: secrets, Logger: dbg.StdLogger(),
    })

    switch mode {
    case serverModeS3:
        handler = s3Handler
    case serverModeDAV:
        handler = newModeHandler(nil, davHandler, cfg.WebDAV.Prefix)
    case serverModeAll:
        handler = newModeHandler(s3Handler, davHandler, cfg.WebDAV.Prefix)
    default:
        return fmt.Errorf("unknown server mode: %s", mode)
    }

    listenAddr := cfg.ResolveListen()
    dbg.Printf("listen_addr=%q mode=%q webdav_prefix=%q", listenAddr, mode, cfg.WebDAV.Prefix)
    log.Printf("listening on %s (mode=%s)", listenAddr, mode)
    return listenAndServe(listenAddr, handler)
}
```

- [ ] **Step 2: Add mode-aware HTTP router**

`modeHandler` is used for both `serverModeAll` and `serverModeDAV` so `/healthz` and `/readyz` remain global operational endpoints outside the WebDAV prefix. In DAV-only mode, non-DAV-prefix requests other than health endpoints return `404` instead of exposing S3.

```go
type modeHandler struct {
    s3     http.Handler
    dav    http.Handler
    prefix string
}

func newModeHandler(s3, dav http.Handler, prefix string) http.Handler {
    return &modeHandler{s3: s3, dav: dav, prefix: prefix}
}

func (h *modeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
        if h.s3 != nil {
            h.s3.ServeHTTP(w, r)
            return
        }
        w.WriteHeader(http.StatusOK)
        return
    }
    if strings.HasPrefix(r.URL.Path, h.prefix) {
        h.dav.ServeHTTP(w, r)
        return
    }
    if h.s3 != nil {
        h.s3.ServeHTTP(w, r)
        return
    }
    http.NotFound(w, r)
}
```

- [ ] **Step 3: Add imports for dav package**

Add `"github.com/aahl/tgnas/internal/dav"` to imports.

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/tgnas/... -v`
Expected: All tests pass.

- [ ] **Step 5: Commit**

```bash
git add cmd/tgnas/main.go
git commit -m "feat: compose s3 and webdav handlers by server mode"
```

---

## Task 10: Update Data Config and Documentation

**Files:**
- Modify: `data/config.yaml`
- Modify: `README.md`

- [ ] **Step 1: Update data/config.yaml**

Add webdav section and update all env var names to `TGNAS_*`:

```yaml
server:
  # listen: ":9000"
  # listen_env: "TGNAS_LISTEN"

auth:
  # region: "us-east-1"
  credentials:
    - access_key: "admin"
      secret_key_env: "TGNAS_SECRET_KEY"

telegram:
  bot_token_env: "TGNAS_TELEGRAM_BOT_TOKEN"
  # api_base_url: "https://api.telegram.org"
  # caption_template: "{bucket}/{key}"
  # timeout: "30s"

metadata:
  # sqlite_path: "data/metadata.sqlite"
  # sqlite_path_env: "TGNAS_SQLITE_PATH"

storage:
  # upload_type_strategy: "document"
  # enable_chunking: true
  # max_file_size: 52428800
  # chunk_size: 20971520

webdav:
  # prefix: "/dav/"

buckets:
  mybucket:
    chat_id: "-1001234567890"
```

- [ ] **Step 2: Update README.md**

Update README to document:
- `tgnas` command name
- Combined S3 + WebDAV root command
- `tgnas s3` and `tgnas dav` single-protocol modes
- `tgnas ls` and `tgnas lsd` local metadata commands
- `TGNAS_*` environment variables
- WebDAV prefix configuration
- WebDAV Basic Auth credential reuse
- Directory marker behavior
- Orphan bucket cleanup

- [ ] **Step 3: Run full test suite**

Run: `go test ./...`
Expected: All tests pass.

- [ ] **Step 4: Build**

Run: `go build ./cmd/tgnas`
Expected: Binary builds successfully.

- [ ] **Step 5: Commit**

```bash
git add data/config.yaml README.md
git commit -m "docs: update config and readme for tgnas and webdav"
```

---

## Verification

Run:

```bash
go test ./...
go build ./cmd/tgnas
```

Manual smoke tests:

```bash
tgnas -debug -c config.yaml
tgnas -debug -c config.yaml s3
tgnas -debug -c config.yaml dav
tgnas -debug ls photos/
tgnas -debug lsd
curl -i -u admin:secret -X OPTIONS http://localhost:9000/dav/photos/
curl -u admin:secret -X PROPFIND http://localhost:9000/dav/
curl -u admin:secret -X MKCOL http://localhost:9000/dav/photos/2026/
curl -u admin:secret -T /tmp/empty-file http://localhost:9000/dav/photos/empty.txt
```

Regression checklist:

- `OPTIONS /dav/<bucket>/` advertises `DAV: 1, 2` and `Allow` includes `PUT`, `MKCOL`, `LOCK`, and `UNLOCK` so Finder does not treat the mount as a `read-only file system`.
- `LOCK` → `PUT` → `UNLOCK` succeeds for a class-2 WebDAV client.
- `GET` uses a seekable `bytes.Reader` path so `http.ServeContent` does not fail with `seeker can't seek` or Finder `Interrupted system call`.
- `webdav.File.Stat()` on an opened object returns object metadata so `PROPFIND allprop` does not produce false missing-file errors or `http: superfluous response.WriteHeader` warnings.
- Routine missing-file `PROPFIND` probes return `404` without logging `os.ErrNotExist` as a WebDAV server error.
- Zero-byte `PUT` stores metadata with `ChunkCount: 0`, empty-content hashes, and no Telegram upload; it must not hit Telegram's `file must be non-empty` rejection.
- `tgnas ls` and `tgnas lsd` open SQLite read-only using an absolute path inside the `file:` URI, so default relative paths do not fail with `SQL logic error: out of memory (1)`.
