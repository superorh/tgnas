# `bucket rename` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `tgnas bucket rename [--dry-run] old new` CLI command that atomically renames a bucket in the SQLite metadata database.

**Architecture:** Two-layer CLI dispatch (`bucket` → `rename`) with a metadata-layer `RenameBucket` transaction and a CLI-layer `chat_id` guard. Tests first for metadata and CLI layers.

**Tech Stack:** Go, SQLite (modernc.org/sqlite), existing `metadata.Store` and CLI dispatch in `cmd/tgnas/main.go`.

---

**Spec:** `docs/superpowers/specs/2026-05-25-tgnas-bucket-rename-design.md`

## File structure

| File | Responsibility |
|------|----------------|
| `metadata/types.go` | Add `BucketRename` struct and `CountBucketRenameRows`/`RenameBucket` to `Store` interface |
| `metadata/sqlite.go` | Implement `CountBucketRenameRows` and `RenameBucket` |
| `metadata/sqlite_test.go` | Add 4 rename-related tests |
| `cmd/tgnas/main.go` | Add `bucket` dispatch, `runBucketCommand`, `runRenameBucket`, `parseRenameBucketFlags`; update `topLevelUsage` |
| `cmd/tgnas/main_test.go` | Add 6 CLI rename tests |
| `README.md` | Add `bucket rename` to local metadata CLI section |

---

### Task 1: Add `BucketRename` types and `Store` interface methods

**Files:**
- Modify: `metadata/types.go`
- Test: `metadata/sqlite_test.go`

- [ ] **Step 1: Write the failing test — `metadata/sqlite_test.go`**

```go
func TestSQLiteCountBucketRenameRowsDoesNotModifyData(t *testing.T) {
	store, _ := testSQLiteStore(t)
	defer store.Close()
	ctx := context.Background()

	createdAt := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	if err := store.UpsertBucket(ctx, metadata.Bucket{Name: "old", ChatID: "-100111", CreatedAt: createdAt, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutObject(ctx, metadata.Object{Bucket: "old", Key: "a.txt", Size: 3, ContentType: "text/plain", ETag: "etag1", SHA256: "sha1", LastModified: createdAt, ChunkCount: 1, TelegramType: "document", UploadStrategy: "single"}, []metadata.Chunk{{Bucket: "old", Key: "a.txt", PartNumber: 1, Offset: 0, Size: 3, TelegramType: "document", TelegramFileID: "f1", SHA256: "csha1"}}); err != nil {
		t.Fatal(err)
	}

	rename, err := store.CountBucketRenameRows(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	if rename.Buckets != 1 || rename.Objects != 1 || rename.Chunks != 1 {
		t.Fatalf("unexpected counts: %+v", rename)
	}

	found, err := store.GetBucket(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	if found.ChatID != "-100111" {
		t.Fatalf("bucket modified by count: %+v", found)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./metadata -run TestSQLiteCountBucketRenameRowsDoesNotModifyData -v`
Expected: compilation error — `CountBucketRenameRows` not defined on `metadata.Store`

- [ ] **Step 3: Add `BucketRename` struct and interface methods to `metadata/types.go`**

Append after the `MoveResult` struct:

```go
type BucketRename struct {
	Buckets int
	Objects int
	Chunks  int
}
```

Add to the `Store` interface, after `DisableBucketsExcept`:

```go
CountBucketRenameRows(ctx context.Context, oldName string) (BucketRename, error)
RenameBucket(ctx context.Context, oldName, newName string) (BucketRename, error)
```

- [ ] **Step 4: Run test to verify it still fails**

Run: `go test ./metadata -run TestSQLiteCountBucketRenameRowsDoesNotModifyData -v`
Expected: compilation error — `*SQLiteStore` does not implement `metadata.Store`

- [ ] **Step 5: Commit**

```bash
git add metadata/types.go
git commit -m "feat(metadata): add BucketRename type and Store interface methods"
```

---

### Task 2: Implement `CountBucketRenameRows` and `RenameBucket` in SQLite

**Files:**
- Modify: `metadata/sqlite.go`
- Test: `metadata/sqlite_test.go`

- [ ] **Step 1: Write the remaining failing tests — `metadata/sqlite_test.go`**

```go
func TestSQLiteRenameBucketRenamesBucketObjectsAndChunks(t *testing.T) {
	store, _ := testSQLiteStore(t)
	defer store.Close()
	ctx := context.Background()

	createdAt := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	if err := store.UpsertBucket(ctx, metadata.Bucket{Name: "old", ChatID: "-100222", CreatedAt: createdAt, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutObject(ctx, metadata.Object{Bucket: "old", Key: "doc.txt", Size: 5, ContentType: "text/plain", ETag: "etag2", SHA256: "sha2", LastModified: createdAt, ChunkCount: 1, TelegramType: "document", UploadStrategy: "single"}, []metadata.Chunk{{Bucket: "old", Key: "doc.txt", PartNumber: 1, Offset: 0, Size: 5, TelegramType: "document", TelegramFileID: "f2", SHA256: "csha2"}}); err != nil {
		t.Fatal(err)
	}

	rename, err := store.RenameBucket(ctx, "old", "new")
	if err != nil {
		t.Fatal(err)
	}
	if rename.Buckets != 1 || rename.Objects != 1 || rename.Chunks != 1 {
		t.Fatalf("unexpected counts: %+v", rename)
	}

	_, err = store.GetBucket(ctx, "old")
	if err != metadata.ErrNotFound {
		t.Fatalf("expected old bucket gone, got err=%v", err)
	}

	bucket, err := store.GetBucket(ctx, "new")
	if err != nil {
		t.Fatal(err)
	}
	if bucket.ChatID != "-100222" || bucket.CreatedAt != createdAt {
		t.Fatalf("bucket metadata not preserved: %+v", bucket)
	}

	obj, chunks, err := store.GetObject(ctx, "new", "doc.txt")
	if err != nil {
		t.Fatal(err)
	}
	if obj.Bucket != "new" || obj.ETag != "etag2" {
		t.Fatalf("object not renamed: %+v", obj)
	}
	if len(chunks) != 1 || chunks[0].Bucket != "new" || chunks[0].TelegramFileID != "f2" {
		t.Fatalf("chunks not renamed: %+v", chunks)
	}
}

func TestSQLiteRenameBucketRejectsExistingTarget(t *testing.T) {
	store, _ := testSQLiteStore(t)
	defer store.Close()
	ctx := context.Background()

	createdAt := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	if err := store.UpsertBucket(ctx, metadata.Bucket{Name: "old", ChatID: "-100333", CreatedAt: createdAt, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBucket(ctx, metadata.Bucket{Name: "new", ChatID: "-100444", CreatedAt: createdAt, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	_, err := store.RenameBucket(ctx, "old", "new")
	if err == nil {
		t.Fatal("expected error renaming to existing bucket")
	}

	bucket, err := store.GetBucket(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	if bucket.Name != "old" {
		t.Fatalf("old bucket should be unchanged: %+v", bucket)
	}
}

func TestSQLiteRenameBucketPreservesChatID(t *testing.T) {
	store, _ := testSQLiteStore(t)
	defer store.Close()
	ctx := context.Background()

	createdAt := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	if err := store.UpsertBucket(ctx, metadata.Bucket{Name: "old", ChatID: "-100999", CreatedAt: createdAt, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	_, err := store.RenameBucket(ctx, "old", "new")
	if err != nil {
		t.Fatal(err)
	}

	bucket, err := store.GetBucket(ctx, "new")
	if err != nil {
		t.Fatal(err)
	}
	if bucket.ChatID != "-100999" {
		t.Fatalf("chat_id not preserved: %s", bucket.ChatID)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./metadata -run 'TestSQLiteRenameBucket|TestSQLiteCountBucketRenameRows' -v`
Expected: compilation error — `CountBucketRenameRows` and `RenameBucket` methods missing from `*SQLiteStore`

- [ ] **Step 3: Implement `CountBucketRenameRows` and `RenameBucket` in `metadata/sqlite.go`**

Append after the last existing method:

```go
func (s *SQLiteStore) CountBucketRenameRows(ctx context.Context, oldName string) (BucketRename, error) {
	var counts BucketRename
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM buckets WHERE name = ?`, oldName).Scan(&counts.Buckets); err != nil {
		return BucketRename{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM objects WHERE bucket = ?`, oldName).Scan(&counts.Objects); err != nil {
		return BucketRename{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM object_chunks WHERE bucket = ?`, oldName).Scan(&counts.Chunks); err != nil {
		return BucketRename{}, err
	}
	return counts, nil
}

func (s *SQLiteStore) RenameBucket(ctx context.Context, oldName, newName string) (BucketRename, error) {
	if oldName == newName {
		return BucketRename{}, errors.New("source and destination bucket are the same")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BucketRename{}, err
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM buckets WHERE name = ?`, oldName).Scan(&exists); err != nil {
		return BucketRename{}, err
	}
	if exists == 0 {
		return BucketRename{}, ErrNotFound
	}

	var targetExists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM buckets WHERE name = ?`, newName).Scan(&targetExists); err != nil {
		return BucketRename{}, err
	}
	if targetExists > 0 {
		return BucketRename{}, fmt.Errorf("destination bucket already exists: %s", newName)
	}

	var counts BucketRename
	counts.Buckets = 1
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM objects WHERE bucket = ?`, oldName).Scan(&counts.Objects); err != nil {
		return BucketRename{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM object_chunks WHERE bucket = ?`, oldName).Scan(&counts.Chunks); err != nil {
		return BucketRename{}, err
	}

	if _, err := tx.ExecContext(ctx, `UPDATE buckets SET name = ? WHERE name = ?`, newName, oldName); err != nil {
		return BucketRename{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE objects SET bucket = ? WHERE bucket = ?`, newName, oldName); err != nil {
		return BucketRename{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE object_chunks SET bucket = ? WHERE bucket = ?`, newName, oldName); err != nil {
		return BucketRename{}, err
	}

	return counts, tx.Commit()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./metadata -run 'TestSQLiteRenameBucket|TestSQLiteCountBucketRenameRows' -v`
Expected: all 4 tests PASS

- [ ] **Step 5: Run full metadata suite**

Run: `go test ./metadata -v`
Expected: all PASS

- [ ] **Step 6: Commit**

```bash
git add metadata/sqlite.go metadata/sqlite_test.go
git commit -m "feat(metadata): add CountBucketRenameRows and RenameBucket"
```

---

### Task 3: Add CLI dispatch, helpers, and tests

**Files:**
- Modify: `cmd/tgnas/main.go`
- Test: `cmd/tgnas/main_test.go`
- Modify: `README.md`

- [ ] **Step 1: Write the failing CLI tests — `cmd/tgnas/main_test.go`**

```go
func TestRunMainBucketRenameDryRunDoesNotModifyMetadata(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "metadata.sqlite")

	cfg := fmt.Sprintf("metadata:\n  sqlite_path: %s\nbuckets:\n  new:\n    chat_id: \"-100555\"\n", dbPath)
	if err := os.WriteFile(configPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	meta, err := metadata.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	if err := meta.UpsertBucket(context.Background(), metadata.Bucket{Name: "old", ChatID: "-100555", CreatedAt: createdAt, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := meta.PutObject(context.Background(), metadata.Object{Bucket: "old", Key: "file.txt", Size: 4, ContentType: "text/plain", ETag: "etag3", SHA256: "sha3", LastModified: createdAt, ChunkCount: 1, TelegramType: "document", UploadStrategy: "single"}, []metadata.Chunk{{Bucket: "old", Key: "file.txt", PartNumber: 1, Offset: 0, Size: 4, TelegramType: "document", TelegramFileID: "f3", SHA256: "csha3"}}); err != nil {
		t.Fatal(err)
	}
	meta.Close()

	var stdout, stderr bytes.Buffer
	if err := runMain([]string{"-c", configPath, "bucket", "rename", "--dry-run", "old", "new"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}

	out := stdout.String()
	if !strings.Contains(out, "would rename bucket old to new: buckets=1") {
		t.Fatalf("unexpected stdout: %s", out)
	}

	meta, err = metadata.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	_, err = meta.GetBucket(context.Background(), "old")
	if err != nil {
		t.Fatalf("dry-run should not have removed old bucket: %v", err)
	}
}

func TestRunMainBucketRenameRenamesMetadata(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "metadata.sqlite")

	cfg := fmt.Sprintf("metadata:\n  sqlite_path: %s\nbuckets:\n  new:\n    chat_id: \"-100666\"\n", dbPath)
	if err := os.WriteFile(configPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	meta, err := metadata.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	if err := meta.UpsertBucket(context.Background(), metadata.Bucket{Name: "old", ChatID: "-100666", CreatedAt: createdAt, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	meta.Close()

	var stdout, stderr bytes.Buffer
	if err := runMain([]string{"-c", configPath, "bucket", "rename", "old", "new"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}

	out := stdout.String()
	if !strings.Contains(out, "renamed bucket old to new: buckets=1") {
		t.Fatalf("unexpected stdout: %s", out)
	}

	meta, err = metadata.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	_, err = meta.GetBucket(context.Background(), "old")
	if err != metadata.ErrNotFound {
		t.Fatalf("expected old bucket gone after rename: %v", err)
	}
	bucket, err := meta.GetBucket(context.Background(), "new")
	if err != nil {
		t.Fatal(err)
	}
	if bucket.ChatID != "-100666" {
		t.Fatalf("chat_id not preserved: %s", bucket.ChatID)
	}
}

func TestRunMainBucketRenameRequiresConfiguredTarget(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "metadata.sqlite")

	cfg := fmt.Sprintf("metadata:\n  sqlite_path: %s\nbuckets:\n  old:\n    chat_id: \"-100777\"\n", dbPath)
	if err := os.WriteFile(configPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	meta, err := metadata.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	if err := meta.UpsertBucket(context.Background(), metadata.Bucket{Name: "old", ChatID: "-100777", CreatedAt: createdAt, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	meta.Close()

	var stdout, stderr bytes.Buffer
	err = runMain([]string{"-c", configPath, "bucket", "rename", "old", "new"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error when target bucket not configured")
	}
	if !strings.Contains(err.Error(), "target bucket is not configured") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunMainBucketRenameRejectsDifferentTargetChatID(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "metadata.sqlite")

	cfg := fmt.Sprintf("metadata:\n  sqlite_path: %s\nbuckets:\n  new:\n    chat_id: \"-100999\"\n", dbPath)
	if err := os.WriteFile(configPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	meta, err := metadata.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	if err := meta.UpsertBucket(context.Background(), metadata.Bucket{Name: "old", ChatID: "-100888", CreatedAt: createdAt, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	meta.Close()

	var stdout, stderr bytes.Buffer
	err = runMain([]string{"-c", configPath, "bucket", "rename", "old", "new"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error when target chat_id differs from source metadata")
	}
	if !strings.Contains(err.Error(), "target bucket chat_id differs") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunMainBucketRenameRejectsExistingTarget(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "metadata.sqlite")

	cfg := fmt.Sprintf("metadata:\n  sqlite_path: %s\nbuckets:\n  new:\n    chat_id: \"-100111\"\n", dbPath)
	if err := os.WriteFile(configPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	meta, err := metadata.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	_ = meta.UpsertBucket(context.Background(), metadata.Bucket{Name: "old", ChatID: "-100111", CreatedAt: createdAt, Enabled: true})
	_ = meta.UpsertBucket(context.Background(), metadata.Bucket{Name: "new", ChatID: "-100222", CreatedAt: createdAt, Enabled: true})
	meta.Close()

	var stdout, stderr bytes.Buffer
	err = runMain([]string{"-c", configPath, "bucket", "rename", "old", "new"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error when target bucket already exists in metadata")
	}
	if !strings.Contains(err.Error(), "destination bucket already exists") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunMainBucketRenameWarnsWhenSourceStillConfigured(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "metadata.sqlite")

	cfg := fmt.Sprintf("metadata:\n  sqlite_path: %s\nbuckets:\n  old:\n    chat_id: \"-100333\"\n  new:\n    chat_id: \"-100333\"\n", dbPath)
	if err := os.WriteFile(configPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	meta, err := metadata.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	if err := meta.UpsertBucket(context.Background(), metadata.Bucket{Name: "old", ChatID: "-100333", CreatedAt: createdAt, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	meta.Close()

	var stdout, stderr bytes.Buffer
	if err := runMain([]string{"-c", configPath, "bucket", "rename", "old", "new"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(stderr.String(), "warning: source bucket still exists in config") {
		t.Fatalf("expected stderr warning, got: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "renamed bucket old to new") {
		t.Fatalf("expected success output, got: %s", stdout.String())
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/tgnas -run 'TestRunMainBucketRename' -v`
Expected: compilation error — `bucket` not a recognized subcommand

- [ ] **Step 3: Implement `topLevelUsage`, `runMain`, and helpers in `cmd/tgnas/main.go`**

Update `topLevelUsage` to add the bucket command line:

```go
"  tgnas [-debug] [-c|-config config.yaml] bucket rename [--dry-run] old-bucket new-bucket\n"
```

Add `case "bucket"` to the `runMain` switch, before `default`:

```go
case "bucket":
	dbg.Printf("mode=bucket")
	return runBucketCommand(configPath, rest[1:], stdout, stderr, dbg)
```

Add the following functions after `runLSD`:

```go
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
	if output != nil {
		fs.SetOutput(output)
	}
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/tgnas -run 'TestRunMainBucketRename' -v`
Expected: all 6 tests PASS

- [ ] **Step 5: Run full CLI suite**

Run: `go test ./cmd/tgnas -v`
Expected: all PASS

- [ ] **Step 6: Update README.md**

Add to the `ls`/`lsd` command block:

```text
tgnas [-debug] [-c|-config config.yaml] bucket rename [--dry-run] old-bucket new-bucket
```

And a description:

> `bucket rename` renames a bucket in the SQLite metadata database. The target bucket name must exist in the current config file with the same `chat_id` as the source bucket metadata. `--dry-run` prints what would change without modifying data. A warning is printed to stderr if the source bucket still appears in the config file.

- [ ] **Step 7: Commit**

```bash
git add cmd/tgnas/main.go cmd/tgnas/main_test.go README.md
git commit -m "feat: add tgnas bucket rename CLI command"
```

---

### Task 4: Final verification

- [ ] **Step 1: Run focused metadata tests**

Run: `go test ./metadata -run 'TestSQLiteRenameBucket|TestSQLiteCountBucketRenameRows' -v`
Expected: 4 PASS

- [ ] **Step 2: Run focused CLI tests**

Run: `go test ./cmd/tgnas -run 'TestRunMainBucketRename' -v`
Expected: 6 PASS

- [ ] **Step 3: Run full test suite**

Run: `go test ./...`
Expected: all PASS

- [ ] **Step 4: Run go vet**

Run: `go vet ./...`
Expected: clean output, exit 0
