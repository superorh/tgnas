# TgNAS Bucket Rename CLI Design

This design adds a local metadata CLI command that renames a bucket in the SQLite metadata database without touching Telegram files.

The command is intended for configuration-driven bucket renames: when an operator changes a bucket name in `config.yaml`, this command migrates the existing metadata from the old name to the new name so that objects, chunks, and bucket records all refer to the updated name.

## Goals

- Support atomic, metadata-only bucket renaming through the CLI.
- Preserve all existing object metadata, chunk references, and Telegram file IDs.
- Provide a dry-run mode that reports what would change without modifying data.
- Require the target bucket name to already exist in the current config file.
- Require the target bucket config to resolve to the same `chat_id` as the existing source bucket metadata.
- Warn (but do not block) when the source bucket still appears in the current config.
- Follow existing CLI conventions for flag parsing, config loading, and metadata access.

## Non-Goals

- No Telegram-side file migration or re-upload.
- No bucket creation or deletion as part of this command.
- No `DisableBucketsExcept` or `UpsertBucket` calls; normal startup reconciles config-defined bucket properties.
- No cross-database migration (only the single configured SQLite metadata database).
- No remote or multi-instance coordination.
- No batch rename of multiple buckets in one invocation.

## CLI Surface

```text
tgnas [-debug] [-c|-config config.yaml] bucket rename [--dry-run] old-bucket new-bucket
```

`-debug` is a global flag and must appear before `bucket`. `--dry-run` is a `bucket rename` flag and must appear after `rename` and before the positional arguments.

### Dry-run mode

```text
tgnas -c config.yaml bucket rename --dry-run old-bucket new-bucket
```

Behavior:

- Loads the config and validates preconditions (target bucket exists in config, source bucket exists in metadata, target bucket does not exist in metadata, target `chat_id` matches source metadata `chat_id`).
- Queries row counts from `buckets`, `objects`, and `object_chunks` for the source bucket name.
- Prints to stdout: `would rename bucket old-bucket to new-bucket: buckets=1 objects=42 chunks=128`
- Does not modify any database rows.

### Real migration

```text
tgnas -c config.yaml bucket rename old-bucket new-bucket
```

Behavior:

- Loads the config and validates preconditions.
- Opens the metadata database in writable mode.
- Runs a single transaction that updates all three tables.
- Prints to stdout: `renamed bucket old-bucket to new-bucket: buckets=1 objects=42 chunks=128`

### Warning: source still in config

If the old bucket name still appears as a key in `config.Buckets`, the command prints to stderr:

```text
warning: source bucket still exists in config: old-bucket
```

This is a warning only; the migration proceeds.

### Error: target not configured

If the new bucket name does not appear as a key in `config.Buckets`, the command exits with:

```text
target bucket is not configured: new-bucket
```

### Error: source not found

If the old bucket name does not exist in the `buckets` table, the command exits with:

```text
source bucket not found: old-bucket
```

### Error: target chat ID differs

If the target bucket's resolved config `chat_id` differs from the source bucket's existing metadata `chat_id`, the command exits with:

```text
target bucket chat_id differs from source bucket metadata: new-bucket
```

### Error: target already exists

If the new bucket name already exists in the `buckets` table, the command exits with:

```text
destination bucket already exists: new-bucket
```

### Error: same names

If old and new names are identical, the command exits with:

```text
source and destination bucket are the same
```

## Metadata API

### Types

Add to `metadata/types.go`:

```go
type BucketRename struct {
	Buckets int
	Objects int
	Chunks  int
}
```

### Interface methods

Add to the `metadata.Store` interface:

```go
CountBucketRenameRows(ctx context.Context, oldName string) (BucketRename, error)
RenameBucket(ctx context.Context, oldName, newName string) (BucketRename, error)
```

`CountBucketRenameRows` is read-only and used by `--dry-run` and by the summary message.

`RenameBucket` validates database preconditions, counts affected rows, updates all tables in one transaction, and returns the counts. The CLI performs the config-vs-metadata `chat_id` check before calling `RenameBucket`, because the metadata package should not depend on config parsing.

### SQLite implementation

Both methods are implemented in `metadata/sqlite.go`.

`CountBucketRenameRows`:

- Counts `buckets WHERE name = oldName` (0 or 1).
- Counts `objects WHERE bucket = oldName`.
- Counts `object_chunks WHERE bucket = oldName`.
- Returns the counts without modifying any rows.

`RenameBucket`:

1. Rejects identical old/new names: `source and destination bucket are the same`.
2. Reads the source bucket row from `buckets`; returns `metadata.ErrNotFound` if absent.
3. Checks for the destination name in `buckets`; returns an error if present: `destination bucket already exists: <name>`.
4. Counts objects and chunks that will change.
5. Updates rows in this order:
   - `UPDATE buckets SET name = ? WHERE name = ?` — only `name` changes; `chat_id`, `created_at`, and `enabled` are preserved from the source row
   - `UPDATE objects SET bucket = ? WHERE bucket = ?`
   - `UPDATE object_chunks SET bucket = ? WHERE bucket = ?`
6. Commits the transaction.
7. Returns the counts.

There are no foreign key constraints between these tables, so update order is not required for referential integrity. The single transaction is the safety boundary: if any update fails, the entire migration is rolled back.

## Config loading

The command reuses existing config loading patterns:

- `config.LoadFile(configPath)` parses the YAML with `KnownFields(true)` and resolves environment variable references in `BucketConfig.ChatID`.
- The command compares `cfg.Buckets[newName].ChatID` with the existing source bucket row's `chat_id` from metadata before migrating. They must match exactly.
- `cfg.ResolveSQLitePath()` resolves the SQLite database path relative to the config file directory.
- The metadata database is opened with `metadata.OpenSQLite(sqlitePath)` (writable, with automatic schema migration) rather than `OpenSQLiteReadOnly`, because the command needs to write.

## CLI internals

### Dispatch

`runMain` in `cmd/tgnas/main.go` adds `bucket` to the existing subcommand switch, then dispatches `bucket rename`:

```go
case "bucket":
    return runBucketCommand(configPath, rest[1:], stdout, stderr, dbg)
```

`runBucketCommand` dispatches the nested action:

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
```

The top-level usage string adds:

```text
  bucket rename [--dry-run] old-bucket new-bucket   rename a bucket in metadata
```

### Helper functions

```go
func runRenameBucket(configPath string, args []string, stdout, stderr io.Writer, dbg debugLogger) error
```

Orchestrates the full command: parse flags, load config, validate preconditions, open metadata, run dry-run or real migration, print results.

```go
func openWritableMetadataFromConfig(configPath string) (*metadata.SQLiteStore, string, error)
```

Loads the config, resolves the SQLite path, opens the database in writable mode with `metadata.OpenSQLite`, and returns the store plus the resolved path for debug logging.

## TDD approach

### Metadata tests

In `metadata/sqlite_test.go`:

- `TestSQLiteRenameBucketRenamesBucketObjectsAndChunks`: Seeds a source bucket with objects and chunks, migrates to a new name, asserts the source bucket/object/chunks are gone and the destination bucket/object/chunks exist with the same metadata.
- `TestSQLiteRenameBucketRejectsExistingTarget`: Seeds both source and destination buckets; asserts migration returns an error and source rows remain unchanged.
- `TestSQLiteRenameBucketPreservesChatID`: Seeds old bucket with `chat_id="-100999"`; migrates to new; asserts new bucket has the same `chat_id="-100999"`.
- `TestSQLiteCountBucketRenameRowsDoesNotModifyData`: Seeds rows, calls count, asserts correct counts and no metadata changes.

### CLI tests

In `cmd/tgnas/main_test.go`:

- `TestRunMainRenameBucketDryRunDoesNotModifyMetadata`: Config contains target bucket, metadata contains source bucket with objects and chunks; runs `bucket rename --dry-run old new`; asserts stdout says `would rename` and metadata remains under the old name.
- `TestRunMainRenameBucketRenamesMetadata`: Runs `bucket rename old new`; asserts stdout says `renamed`, metadata moved to new name, old bucket absent.
- `TestRunMainRenameBucketRequiresConfiguredTarget`: Config omits target bucket; asserts clear error and no metadata changes.
- `TestRunMainRenameBucketRejectsDifferentTargetChatID`: Config target bucket resolves to a different `chat_id` than the source bucket metadata; asserts clear error and no metadata changes.
- `TestRunMainRenameBucketRejectsExistingTarget`: Metadata already contains target; asserts clear error.
- `TestRunMainRenameBucketWarnsWhenSourceStillConfigured`: Config contains both old and new names; asserts stderr warning and successful migration.

## Verification

Run, in order:

1. `go test ./metadata -run 'RenameBucket|CountBucketRenameRows'`
2. `go test ./cmd/tgnas -run 'Bucket|RenameBucket'`
3. `go test ./...`
4. `go vet ./...`

Manual smoke check:

1. Create a temp config with `metadata.sqlite_path` pointing to a temp database and `buckets` containing only the new name.
2. Seed metadata with the old bucket name.
3. Run `tgnas -c <config> bucket rename --dry-run old new` and confirm row counts without changes.
4. Run `tgnas -c <config> bucket rename old new` and confirm `tgnas -c <config> lsd` lists the new bucket.
