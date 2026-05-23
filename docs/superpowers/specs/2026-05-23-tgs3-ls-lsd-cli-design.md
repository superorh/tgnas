# tgs3 Local ls/lsd CLI Design

## Goal

Add simple local metadata listing commands to `tgs3` so operators can inspect buckets, object keys, and pseudo-directories from the configured SQLite database without starting the S3 HTTP service or contacting Telegram.

## Scope

This design adds two subcommands to the existing `tgs3` binary:

- `ls`: list object keys under a bucket/prefix
- `lsd`: list buckets or direct pseudo-directories under a bucket/prefix

The commands read local SQLite metadata only. They do not create or migrate the database, upload, download, delete, mutate buckets, upsert configured buckets, initialize Telegram clients, or perform S3 HTTP requests.

## Default Config File

When no `-config` or `-c` flag is provided, `tgs3` reads `data/config.yaml` from the current working directory. The config file uses YAML format with the following top-level sections:

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

Service startup requires active values for `auth.credentials`, `telegram.bot_token_env`, and at least one bucket with `chat_id`. Local `ls` and `lsd` only parse the metadata section needed to resolve SQLite and therefore do not require auth, Telegram, storage, or bucket service settings to be present.

Bucket `chat_id` may be a literal Telegram chat ID or a full environment-variable reference in the form `${ENV_NAME}`. Partial interpolation and multiple variables in one value are not supported. If the referenced environment variable is unset or empty, the resolved `chat_id` is empty and service config validation fails.

## CLI Shape

Usage:

```text
tgs3 [-debug] [-c|-config config.yaml]
tgs3 [-debug] [-c|-config config.yaml] ls [-n|-limit N] bucket[/prefix]
tgs3 [-debug] [-c|-config config.yaml] lsd [bucket[/prefix]]
```

With no subcommand, `tgs3` starts the HTTP service. `-c` is a short alias for `-config`; passing both is a usage error. `-debug` is a global boolean flag that enables debug logging for key operations.

### `ls`

Behavior:

- Treats the first path segment as the bucket name.
- Treats everything after the first `/` as the key prefix.
- `bucket` without `/` is valid and means an empty prefix.
- Outputs matching object keys, one per line.
- Uses lexicographic key order from SQLite metadata.

Rules:

- Default limit is `1000`.
- `-limit` and `-n` are aliases.
- `0` means no overall result limit.
- `0` still pages internally, so the command does not load all rows at once.
- Negative limits are usage errors.
- Passing both `-limit` and `-n` explicitly is a usage error.

### `lsd`

Behavior:

- With no path, lists enabled buckets, one per line.
- With `bucket/prefix`, lists direct common prefixes under that prefix, one per line.
- The pseudo-directory delimiter is fixed to `/`.
- `lsd` does not support `-limit` or `-n` in this version.

For `lsd bucket/prefix`, direct common-prefix detection is S3-like:

- remove the requested prefix from each matching object key
- if the remainder contains `/`, emit `prefix + firstSegment + "/"`
- if the remainder does not contain `/`, ignore the object because `lsd` lists directories only
- empty first segments are ignored (this handles prefixes that do not end with `/`)
- deduplicate emitted prefixes while preserving metadata listing order

## Debug Logging

`-debug` enables human-readable diagnostic logging for key local and service operations. Debug logs go to stderr so normal command output remains script-friendly on stdout.

Debug logging should include:

- resolved config path
- selected mode (`service`, `ls`, or `lsd`)
- resolved SQLite metadata path
- local listing bucket and prefix
- local listing page requests, including prefix, after-key cursor, and page limit
- number of rows returned per metadata page
- service listen address
- configured bucket upsert names during service startup
- S3 compatibility branches such as configured-bucket `CreateBucket` success and `UNSIGNED-PAYLOAD` acceptance
- S3 `PutObject` request metadata: bucket, key, content length, content type, and payload hash mode (`sha256` or `unsigned`)
- object-store `PutObject` decisions: object size, upload type, whether chunking is enabled, chunk size, and chunk count
- Telegram upload progress per part: bucket, key, part number, Telegram media type, message ID, and whether a file ID was returned
- metadata write completion: bucket, key, chunk count, ETag, and success or error result
- S3 `PutObject` completion: bucket, key, result, and ETag on success

Debug logging must not print secrets or sensitive values, including auth secret values, Telegram bot tokens, raw Authorization headers, object body bytes, or full Telegram file IDs. Bucket names, object keys, prefixes, SQLite paths, listen addresses, object sizes, content types, ETags, Telegram message IDs, and `file_id_present=true|false` are acceptable because they are operational metadata rather than credentials.

## Error Behavior

Usage errors include:

- unknown subcommand
- empty path where a path is required
- empty bucket, such as `/prefix`
- too many operands
- negative `ls` limit
- both `-limit` and `-n` explicitly set
- both `-config` and `-c` explicitly set
- `-debug` placed after the subcommand instead of in the global flag position

Runtime errors include:

- config cannot be loaded or contains unknown YAML fields
- SQLite path cannot be resolved
- SQLite database cannot be opened read-only
- requested bucket does not exist or is disabled
- metadata listing fails

Normal listing output goes to stdout. Errors are returned through the command path and surfaced by `main` consistently with existing `log.Fatal` behavior.

## Architecture

The existing `cmd/tgs3/main.go` service path stays intact. A new dispatch layer parses global flags and chooses between service startup and local CLI subcommands.

Recommended structure:

- `main()` calls a testable `runMain(args []string, stdout io.Writer, stderr io.Writer) error`.
- `runMain` parses global `-config` / `-c` / `-debug` flags.
- `runMain` creates a debug logger that writes to stderr when `-debug` is set and discards logs otherwise.
- If no subcommand is present, `runMain` calls the existing server startup function with debug logging enabled or disabled.
- If subcommand is `ls`, `runMain` calls `runLS` with stdout for listing output and stderr-backed debug logging.
- If subcommand is `lsd`, `runMain` calls `runLSD` with stdout for listing output and stderr-backed debug logging.

Local listing commands share a metadata opener:

```go
openMetadataFromConfig(configPath string) (metadata.Store, string, error)
```

This helper performs strict metadata-only YAML parsing with known-field validation, applies the local CLI defaults for `metadata.driver` and `metadata.sqlite_path`, resolves the SQLite path, and opens the metadata store in read-only mode. It intentionally does not create, migrate, or mutate the SQLite database, and it does not perform server-only setup such as full config validation, bucket upserts, Telegram token validation, Telegram client creation, object-store creation, credential resolution, readiness setup, or HTTP server startup.

## Data Flow

### Service mode

```text
args -> parse global flags -> build debug logger -> no subcommand -> existing run(configPath, debug logger) -> HTTP server
```

### `ls`

```text
args -> parse global flags -> build debug logger -> parse ls flags -> parse bucket/prefix
     -> open SQLite metadata -> validate bucket -> page metadata.ListObjects
     -> print object keys to stdout and optional debug logs to stderr
```

`ls -n 0` uses repeated fixed-size metadata pages until exhausted. Finite limits use the smaller of remaining result count and internal page size.

### `lsd`

```text
args -> parse global flags -> build debug logger -> parse lsd operand
     -> open SQLite metadata
     -> no operand: metadata.ListBuckets -> print bucket names to stdout
     -> operand: validate bucket -> page metadata.ListObjects -> compute direct common prefixes -> print prefixes to stdout
     -> optional debug logs go to stderr
```

## Rclone Compatibility Fixes

During rclone smoke testing, `rclone copy` exposed two server-side S3 compatibility gaps that are adjacent to, but separate from, the local `ls`/`lsd` CLI work:

- `CreateBucket`: rclone may issue `PUT /bucket` before upload. For buckets already present in configured metadata, `tgs3` treats this as an idempotent success and returns `200 OK`. For missing buckets, it returns the existing bucket lookup error, normally `NoSuchBucket`.
- `UNSIGNED-PAYLOAD`: rclone may sign `PutObject` requests with `X-Amz-Content-Sha256: UNSIGNED-PAYLOAD`. SigV4 verification accepts this literal payload hash in the canonical request and leaves the request body available for the object upload path.

These compatibility fixes do not add dynamic bucket creation, bucket mutation, S3 client mode, or relaxed authentication. Configured bucket metadata remains the source of truth.

## Reused Code and Interfaces

The design reuses current metadata primitives:

- `yaml.Decoder.KnownFields(true)` for strict local CLI config parsing
- `Config.ResolveSQLitePath`
- `metadata.OpenSQLiteReadOnly`
- `metadata.Store.ListBuckets`
- `metadata.Store.GetBucket` or enabled bucket lookup via `ListBuckets`
- `metadata.Store.ListObjects`
- `metadata.ListQuery{Bucket, Prefix, AfterKey, Limit}`

It intentionally does not use `store.NewObjectStore` for the CLI listing path because that constructor is tied to Telegram client setup and because `store.ObjectStore.ListObjects` treats `Limit == 0` as an empty result, while the CLI needs `0` to mean unlimited overall.

## Testing Strategy

Add tests under `cmd/tgs3` for:

- global config parsing:
  - `-config`
  - `-c`
  - `-debug`
  - explicit `-config` plus explicit `-c` usage error
  - `-debug` appears in top-level help output
- server dispatch:
  - no subcommand still follows the service startup path
- `ls` validation:
  - missing path
  - too many paths
  - empty bucket path
  - negative `-limit`
  - negative `-n`
  - both `-limit` and `-n`
- `ls` behavior:
  - prefix filtering
  - default limit of 1000
  - finite `-n` limit
  - `-n 0` unlimited output across multiple internal pages
  - missing or disabled bucket error
- `lsd` behavior:
  - no path lists enabled buckets only
  - path lists only direct common prefixes using `/`
  - duplicate child prefixes are emitted once
  - too many args usage error
- debug logging:
  - `-debug` writes diagnostic logs to stderr for service, `ls`, and `lsd`
  - normal listing output remains unchanged on stdout
  - debug logs include config path, SQLite path, selected mode, bucket/prefix, page cursors, page sizes, and row counts
  - debug logs do not contain auth secrets, bot tokens, or Authorization headers

Use temporary SQLite databases in tests. Seed buckets and objects through writable metadata stores from `metadata.OpenSQLite`, then exercise CLI paths through read-only metadata opening. Do not seed through Telegram or HTTP.

Add server compatibility tests under `internal/s3api` for:

- `PUT /bucket` succeeds for a configured bucket and remains rejected for missing buckets.
- `PutObject` succeeds when SigV4 uses `X-Amz-Content-Sha256: UNSIGNED-PAYLOAD`, and the uploaded object can be read back unchanged.

## Verification

Run:

```bash
go test ./...
go build ./cmd/tgs3
```

Manual smoke examples:

```bash
tgs3 -debug ls photos/
tgs3 -debug ls -n 0 photos/
tgs3 -debug lsd
tgs3 -debug lsd photos/
```

## Non-Goals

- No S3 HTTP client mode.
- No dynamic bucket creation outside configured metadata.
- No Telegram API access.
- No object download, upload, delete, or metadata mutation by local listing commands.
- No recursive tree formatting.
- No JSON output.
- No `lsd` limit flag.
- No configurable delimiter; `lsd` uses `/` only.
