# tgnas Rename and WebDAV Server Design

## Goal

Rename the user-facing `tgs3` command to `tgnas` and add WebDAV support so one process can expose both the existing S3-compatible API and a WebDAV API backed by the same Telegram/object metadata store.

## Scope

This design covers:

- Renaming the visible command from `tgs3` to `tgnas`.
- Removing old `tgs3` command compatibility; only `tgnas` is supported after the rename.
- Changing user-facing environment variable names from old `TGS3_*` and generic examples to `TGNAS_*` names so examples and defaults match the new command name; backward-compatible aliases are not part of this version.
- Adding `tgnas s3` and `tgnas dav` service subcommands.
- Making the root `tgnas` command start both S3 and WebDAV on the same listen address.
- Keeping `tgnas ls` and `tgnas lsd` as local SQLite metadata inspection commands.
- Adding WebDAV support under a configurable path prefix, defaulting to `/dav/`.
- Supporting common WebDAV read/write operations, including `MKCOL`, same-bucket metadata-only `COPY`/`MOVE`, and recursive directory `COPY`/`MOVE`.
- Allowing S3 and WebDAV bucket deletion only for orphan buckets that remain in metadata after being removed from config.

This design does not add dynamic bucket creation through WebDAV or S3. Bucket creation remains config-driven.

## Command Shape

Usage:

```text
tgnas [-debug] [-c|-config config.yaml]
tgnas [-debug] [-c|-config config.yaml] s3
tgnas [-debug] [-c|-config config.yaml] dav
tgnas [-debug] [-c|-config config.yaml] ls [-n|-limit N] bucket[/prefix]
tgnas [-debug] [-c|-config config.yaml] lsd [bucket[/prefix]]
```

Behavior:

- `tgnas` starts one HTTP server on the resolved listen address with both S3 and WebDAV enabled.
- `tgnas s3` starts only the S3-compatible API.
- `tgnas dav` starts only the WebDAV API.
- `tgnas ls` and `tgnas lsd` keep their current local metadata behavior: they open SQLite read-only, do not initialize Telegram, and do not start an HTTP server.
- `-debug`, `-c`, and `-config` remain global flags and must appear before the subcommand.

Internally service startup can be represented with a mode enum:

```go
type serverMode string

const (
	serverModeAll serverMode = "all"
	serverModeS3  serverMode = "s3"
	serverModeDAV serverMode = "dav"
)
```

Root command maps to `serverModeAll`; `s3` and `dav` map to their single-service modes.

## Routing

WebDAV is mounted under a configurable prefix. The default is `/dav/`.

Root command routing:

- `/healthz` and `/readyz` are global operational endpoints and are served before protocol routing. They are not protected by WebDAV Basic Auth. This applies to all service modes (`all`, `s3`, `dav`).
- Requests whose path matches the configured WebDAV prefix go to the WebDAV handler.
- All other requests go to the existing S3 handler.
- At startup the first normalized path segment of `webdav.prefix` must not match any configured bucket name. If a conflict exists, startup rejects the configuration with an explicit error. This avoids silently capturing bucket routes in combined mode.

`tgnas s3` routing:

- All requests go to S3.

`tgnas dav` routing:

- Requests must still use the configured WebDAV prefix, even in DAV-only mode. This keeps path semantics stable across combined and single-protocol modes.
- Requests outside the prefix return `404`.

## Configuration

The project continues to use a single YAML config file, defaulting to `data/config.yaml`.

Existing service settings remain shared:

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

metadata:
  # sqlite_path: "data/metadata.sqlite"
  # sqlite_path_env: "TGNAS_SQLITE_PATH"
```

Add a WebDAV section:

```yaml
webdav:
  # prefix: "/dav/"
```

Rules:

- `webdav.prefix` defaults to `/dav/`.
- It must start with `/`.
- It is normalized to end with `/`; `/dav` becomes `/dav/`.
- It cannot be `/`, because that would swallow the S3 route namespace.
- Root `tgnas`, `tgnas s3`, and `tgnas dav` all use the same `server.listen` / `server.listen_env` resolution logic.

The local `ls` and `lsd` commands continue to parse only the metadata config needed to resolve SQLite. They do not require auth, Telegram, bucket, storage, or WebDAV service configuration to be complete. They open SQLite read-only; relative SQLite paths such as the default `data/metadata.sqlite` must be converted to absolute paths before building the read-only `file:` URI so the SQLite driver does not reject them with misleading low-level errors.

Environment variable rename rules:

- default listen env becomes `TGNAS_LISTEN`.
- default auth secret examples use `TGNAS_SECRET_KEY` or bucket-specific `TGNAS_<NAME>_SECRET_KEY` names.
- default Telegram token env example becomes `TGNAS_TELEGRAM_BOT_TOKEN`.
- default SQLite path override env example becomes `TGNAS_SQLITE_PATH`.
- old `TGS3_*` names are not checked as aliases.

## Authentication

S3 keeps the existing SigV4 authentication.

WebDAV uses HTTP Basic Auth and reuses `auth.credentials`:

- Basic username = `auth.credentials[].access_key`
- Basic password = the value resolved from `auth.credentials[].secret_key_env`

Startup resolves all configured secrets once. If a secret environment variable is missing or empty, service startup fails. This keeps S3 and WebDAV credentials consistent and avoids a separate WebDAV credential store.

Authentication errors:

- Missing Basic Auth returns `401 Unauthorized` with `WWW-Authenticate`.
- Unknown username or wrong password returns `401 Unauthorized`.
- Debug logs must not print Basic Auth passwords or Authorization headers.

## WebDAV Implementation Approach

Use `golang.org/x/net/webdav` as the WebDAV protocol adapter.

Implementation units:

- Add an internal WebDAV package at `internal/dav`.
- Implement a custom `webdav.FileSystem` backed by the existing object store and metadata store.
- Use a lightweight in-memory lock system so Finder and other class-2 WebDAV clients can complete `LOCK` → `PUT` → `UNLOCK` write flows without persisting lock state.
- Wrap or front the `webdav.Handler` where needed for behavior that `x/net/webdav` does not model cleanly, especially same-bucket metadata-only `COPY` and `MOVE`, `OPTIONS` capability negotiation, and client-compatibility guards.

The adapter should reuse the existing `store.ObjectStore` for ordinary object read/write/delete behavior and use new metadata-level helpers for copy/move operations that must not download and re-upload Telegram content.

## Path Mapping

After stripping `webdav.prefix`, paths map as follows:

```text
/                         WebDAV root collection listing enabled buckets
/{bucket}/                bucket collection
/{bucket}/{key...}        object key or directory path inside bucket
```

Rules:

- `/{bucket}` identifies an existing bucket collection.
- Bucket creation through WebDAV is not supported.
- `MKCOL /{bucket}` returns `403 Forbidden`.
- Enabled buckets are listed in the WebDAV root collection.
- Orphan buckets are not shown in the default root listing. They are reachable only by known path for cleanup.
- All WebDAV methods on an orphan bucket path — including `PROPFIND`, `GET`, `HEAD`, `PUT`, `DELETE` (non-root), `COPY`, `MOVE`, and `MKCOL` — return `403 Forbidden`, except for bucket-root `DELETE /dav/{orphan}` and `DELETE /dav/{orphan}/` which perform metadata cleanup.

## Directory Semantics

WebDAV exposes object-store prefixes as directories.

Path normalization:

- URL path segments are percent-decoded before bucket/key mapping; malformed escapes return `400 Bad Request`.
- Collection paths are canonicalized to trailing-slash key prefixes. `/dav/photos/dir` and `/dav/photos/dir/` both refer to collection prefix `dir/` if `dir/` exists as a marker or has children.
- Recursive collection operations match only the canonical prefix with a trailing `/`; prefix `dir/` must not match sibling keys such as `dir2/file.txt`.
- File paths keep their exact decoded key and do not get a trailing slash added.
- Bucket-root paths `/dav/{bucket}` and `/dav/{bucket}/` are normalized to the same bucket collection.

Directory marker:

- A directory marker is a zero-byte object whose key ends with `/`.
- `MKCOL /dav/photos/2026/` creates object key `2026/` in bucket `photos`.
- Directory markers preserve empty directories.

`PROPFIND` directory listing includes both:

- explicit directory marker objects; and
- implicit common prefixes derived from child object keys.

`PUT` to a path ending in `/` is rejected. Directories are created with `MKCOL`.

`GET` and `HEAD` on directories use collection semantics. The first version delegates directory GET/HEAD behavior to the default `x/net/webdav` response semantics and does not introduce a custom HTML directory listing page.

## WebDAV Operations

Supported common operations:

- `OPTIONS`
- `PROPFIND`
- `GET`
- `HEAD`
- `PUT`
- `DELETE`
- `MKCOL`
- `COPY`
- `MOVE`
- `LOCK`
- `UNLOCK`

`OPTIONS`:

- Advertises `DAV: 1, 2` so Finder and other class-2 clients recognize lock/write support.
- Existing collections advertise writable collection methods including `PUT`, `MKCOL`, `DELETE`, `COPY`, `MOVE`, `LOCK`, `UNLOCK`, and `PROPFIND`.
- Existing files advertise file methods including `GET`, `HEAD`, `PUT`, `DELETE`, `COPY`, `MOVE`, `LOCK`, `UNLOCK`, and `PROPFIND`.
- Non-existing paths advertise creation/probing methods including `PUT`, `MKCOL`, `LOCK`, `UNLOCK`, and `PROPFIND`; object access is still enforced when the actual method runs.

Locking:

- `LOCK` and `UNLOCK` are delegated to `x/net/webdav` with the lightweight lock system.
- The lock system returns opaque lock tokens, confirms lock preconditions, and accepts unlocks without persisting state beyond process memory.
- Locks are compatibility coordination for WebDAV clients, not a durable cross-process locking guarantee.

`PROPFIND`:

- Missing `Depth` defaults to `infinity` per WebDAV, but this implementation treats omitted `Depth` as `1` for collection targets to avoid accidental full-bucket scans by common clients. For non-collection (file) targets, omitted `Depth` behaves as `Depth: 0` because a file has no children.
- `Depth: 0` returns only the requested resource.
- `Depth: 1` returns the requested resource and direct children (collections only; for files, same as `Depth: 0`).
- `Depth: infinity` is rejected with `403 Forbidden` to avoid scanning entire buckets accidentally.
- Routine `PROPFIND` probes for missing files return `404 Not Found` but are not logged as WebDAV errors; Finder uses these probes before creating files.
- Opened file resources must return object metadata from `webdav.File.Stat()` so `allprop` responses do not report false missing-file errors after a successful open.

Common properties:

- `displayname`
- `resourcetype`
- `getcontentlength`
- `getetag`
- `getlastmodified`
- `getcontenttype`

`PUT`:

- Writes file content through the shared object store.
- `PUT` to an existing collection path fails with `409 Conflict`.
- `PUT` to a zero-byte file is valid. The object store records a zero-byte metadata row with `ChunkCount: 0`, empty-content hashes, and no Telegram chunks; it must not attempt to upload an empty file to Telegram because Telegram rejects non-empty media constraints with `Bad Request: file must be non-empty`.

`MKCOL`:

- Creates zero-byte directory marker objects.
- Fails with `409 Conflict` when the parent collection does not exist.
- Fails with `403 Forbidden` when asked to create a bucket.

`DELETE`:

- File delete removes object metadata and returns `204 No Content`.
- Directory delete recursively removes object metadata under that prefix, including marker objects, and returns `204 No Content`.
- Deleting a file or directory that does not exist returns `404 Not Found`. Repeated delete of an already-deleted target also returns `404`.
- Configured bucket root delete is forbidden (`403`).
- Orphan bucket root delete is allowed and cleans local metadata, returning `204 No Content`.
- Bucket-root requests are normalized so that `/dav/{bucket}` and `/dav/{bucket}/` are treated as the same bucket-root collection.
- `DELETE /dav/{unknown}` for a bucket that does not exist in metadata returns `404 Not Found`.
- Orphan cleanup deletes the bucket record and all local object/chunk metadata under it; it does not require deleting child paths first.

`COPY` and `MOVE`:

- First version supports only same-bucket copy/move.
- Cross-bucket copy/move returns `403 Forbidden`.
- Requests require a `Destination` header. Missing or malformed `Destination` returns `400 Bad Request`.
- The `Destination` value is parsed as a URL. If it has no scheme and no host (a bare path), it is treated as a path on the same server. If it is an absolute URL, the host and port must match the request's `Host` header exactly; different hosts return `403 Forbidden`. After resolving the host, the path must start with the configured `webdav.prefix`; otherwise it returns `400 Bad Request`.
- `Destination` must target the same bucket as the source path. Different bucket destinations are rejected explicitly.
- `Overwrite` defaults to `T` when omitted.
- `Overwrite: F` with an existing destination returns `412 Precondition Failed` and leaves source and destination unchanged.
- Unsupported `Overwrite` values return `400 Bad Request`.
- With `Overwrite: T`, an existing destination metadata entry is replaced.
- Successful copy/move to a new destination returns `201 Created`.
- Successful copy/move replacing an existing destination returns `204 No Content`.
- File copy/move is metadata-only and reuses the source object's Telegram chunk/file ID metadata.
- Directory copy/move recursively rewrites metadata for every object under the source prefix.
- `MOVE` is copy followed by deletion of source metadata within one metadata transaction.
- Telegram content is not downloaded or re-uploaded for copy/move.
- Copy/move of a directory into its own subtree is forbidden with `403 Forbidden`.
- Recursive metadata rewrites must be all-or-nothing inside one SQLite metadata transaction. A failure rolls back the entire recursive operation and returns a single error response, not `207 Multi-Status`.

## Metadata and Store Changes

The existing object store supports normal object operations. WebDAV needs additional metadata-level operations for server-side copy/move and orphan bucket cleanup.

Required metadata helpers:

```go
CopyObject(ctx context.Context, bucket, srcKey, dstKey string, options CopyOptions) (CopyResult, error)
MoveObject(ctx context.Context, bucket, srcKey, dstKey string, options MoveOptions) (MoveResult, error)
CopyPrefix(ctx context.Context, bucket, srcPrefix, dstPrefix string, options CopyOptions) (CopyResult, error)
MovePrefix(ctx context.Context, bucket, srcPrefix, dstPrefix string, options MoveOptions) (MoveResult, error)
DeletePrefix(ctx context.Context, bucket, prefix string) error
DeleteBucket(ctx context.Context, bucket string) error
```

These helpers expand the metadata/store interfaces, not just the WebDAV adapter. S3 bucket-delete behavior and WebDAV copy/move/delete should use the same metadata primitives so bucket cleanup and recursive operations have one consistency model.

`CopyObject` and `CopyPrefix` must copy:

- object metadata rows;
- chunk metadata rows;
- ETag, SHA256, size, content type, Telegram type, upload strategy, and Telegram chunk references.

Copied objects get a fresh `LastModified` timestamp at the destination. Their ETag and SHA256 remain the same because the bytes are unchanged. `MOVE` preserves ETag/SHA256 and sets destination `LastModified` to the move time.

All copy/move/delete-prefix helpers run in a SQLite transaction. Recursive directory operations are all-or-nothing and do not return `207 Multi-Status` for partial failure. They must not contact Telegram. To prevent unbounded lock holding, recursive operations count affected objects in a preliminary read pass and reject operations exceeding 100,000 objects with `500 Internal Server Error`. This cap covers the practical range for metadata-only operations in a single SQLite transaction.

`DeleteBucket` deletes local metadata rows for:

- chunks in the bucket;
- objects in the bucket;
- the bucket record.

It does not delete Telegram messages or files.

Zero-byte objects are represented as object metadata without chunks:

- `Size` is `0`.
- `ChunkCount` is `0`.
- ETag is the MD5 of empty content.
- SHA256 is the SHA256 of empty content.
- No Telegram upload is attempted.
- `GET` returns an empty readable body from metadata/chunk state.

## Bucket Lifecycle and Orphan Cleanup

Bucket creation remains config-driven.

Configured bucket:

- Exists in current `config.yaml` `buckets`.
- Service startup upserts/enables it.
- S3 and WebDAV can read/write objects in it.
- S3 `DeleteBucket` is forbidden.
- WebDAV `DELETE /dav/{bucket}` is forbidden.

Orphan bucket:

- Exists in metadata but not in current `config.yaml` `buckets`.
- Normal S3/WebDAV object read/write access is rejected.
- WebDAV object-level access under an orphan bucket, including `GET`, `HEAD`, `PUT`, `DELETE` object, `COPY`, `MOVE`, `MKCOL`, and `PROPFIND`, returns `403 Forbidden`.
- `OPTIONS` remains available at the protocol layer and does not imply object access.
- S3 `DeleteBucket` is allowed for cleanup.
- WebDAV `DELETE /dav/{bucket}` is allowed for cleanup.
- Cleanup removes local metadata only and does not contact Telegram.

S3 `CreateBucket` remains idempotent only for configured buckets. It does not create new buckets.

## Server Composition

Service startup should initialize shared dependencies once:

1. Load full config.
2. Validate `webdav.prefix`, including rejecting configured bucket name conflicts.
3. Open/migrate SQLite metadata.
4. Upsert configured buckets.
5. Resolve credentials.
6. Create Telegram client.
7. Create `store.ObjectStore`.
8. Build selected HTTP handlers based on mode.
9. Start one `http.ListenAndServe`.

`/healthz` and `/readyz` remain global operational endpoints in all service modes (`all`, `s3`, and `dav`). They are served before protocol routing, so DAV-only mode still exposes health checks outside `webdav.prefix`.

Combined handler:

```text
if mode includes DAV and request path matches webdav.prefix:
    serve WebDAV
else if mode includes S3:
    serve S3
else:
    404
```

This keeps root `tgnas` as one process, one port, two protocols.

## Debug Logging

`-debug` adds WebDAV logs while keeping normal command output on stdout and logs on stderr.

Log fields should include:

- service mode: `all`, `s3`, or `dav`
- resolved WebDAV prefix
- WebDAV request method and path
- bucket and key
- `PROPFIND` depth
- `MKCOL` marker key
- `COPY`/`MOVE` source and destination
- metadata-only copy/move result
- orphan bucket delete result

Safety rules:

- Use `%q` for path, bucket, key, header-derived values, and errors.
- Do not log Basic Auth passwords.
- Do not log raw Authorization headers.
- Do not log full Telegram file IDs.
- Suppress routine `os.ErrNotExist` logs from client probes such as Finder `PROPFIND` checks for files that are about to be created.

## Error Mapping

Recommended WebDAV status mapping:

- Missing or invalid Basic Auth: `401 Unauthorized`
- Unknown bucket: `404 Not Found`
- Orphan bucket non-delete access (including `PROPFIND`): `403 Forbidden`
- Create bucket through WebDAV: `403 Forbidden`
- Delete configured bucket: `403 Forbidden`
- Delete orphan bucket: `204 No Content` if metadata cleanup succeeds
- Successful file or directory `DELETE`: `204 No Content`
- `DELETE` of non-existent file, directory, or already-deleted target: `404 Not Found`
- Cross-bucket `COPY`/`MOVE`: `403 Forbidden`
- Missing or malformed `Destination` header: `400 Bad Request`
- `Destination` host mismatch: `403 Forbidden`
- `Destination` path outside `webdav.prefix`: `400 Bad Request`
- Unsupported `Overwrite` header value: `400 Bad Request`
- `Overwrite: F` and existing destination: `412 Precondition Failed`
- Successful `COPY`/`MOVE` creating a new destination: `201 Created`
- Successful `COPY`/`MOVE` replacing an existing destination: `204 No Content`
- Recursive operation exceeds object limit: `500 Internal Server Error`
- Recursive operation failure after transaction rollback: `500 Internal Server Error`
- Parent collection missing for `MKCOL`: `409 Conflict`
- `COPY`/`MOVE` into own subtree: `403 Forbidden`
- `LOCK`: `201 Created` for a new compatibility lock when `x/net/webdav` accepts the request
- `UNLOCK`: `204 No Content` when `x/net/webdav` accepts the request
- `Depth: infinity`: `403 Forbidden`
- Malformed percent-encoding in path: `400 Bad Request`
- Internal metadata/store failures: `500 Internal Server Error`

Startup validation mapping:

- `webdav.prefix` normalized first segment matches a configured bucket name: startup fails with a configuration error.

## Testing Strategy

### CLI and rename

- `tgnas` root command starts a combined handler.
- `tgnas s3` starts only S3.
- `tgnas dav` starts only WebDAV.
- `tgnas ls` and `tgnas lsd` do not start a server.
- `tgnas ls` and `tgnas lsd` open the default relative SQLite path read-only without `SQL logic error: out of memory` by converting it to an absolute path before creating the `file:` URI.
- Usage/help text uses `tgnas`.
- No `tgs3` command compatibility remains.
- The binary moves from `cmd/tgs3` to `cmd/tgnas`; the Go module/import path is now `github.com/aahl/tgnas`; user-facing docs and default config use `tgnas`.

### Config

- `webdav.prefix` defaults to `/dav/`.
- `/dav` normalizes to `/dav/`.
- `/` is rejected.
- Prefix without leading `/` is rejected.
- If `webdav.prefix` first segment matches a configured bucket name, startup rejects the config.
- Default env names are `TGNAS_LISTEN`, `TGNAS_SECRET_KEY`, `TGNAS_TELEGRAM_BOT_TOKEN`, and `TGNAS_SQLITE_PATH`; old `TGS3_*` aliases are not read.
- Local `ls`/`lsd` do not require WebDAV config.

### Auth

- Missing Basic Auth returns `401`.
- Wrong Basic Auth returns `401`.
- Valid Basic Auth reaches WebDAV handler.
- Empty secret env causes service startup failure.

### WebDAV operations

- `PROPFIND` prefix root lists enabled buckets.
- `PROPFIND` bucket root lists files, implicit directories, and marker directories.
- Omitted `Depth` behaves as `Depth: 1` for collection targets; omitted `Depth` on file targets behaves as `Depth: 0`.
- `Depth: 0` and `Depth: 1` work for both file and collection targets.
- `Depth: infinity` is rejected.
- URL-escaped bucket/key paths decode correctly; malformed escapes return `400`.
- Collection operations on `/dir` and `/dir/` target the same canonical `dir/` prefix and do not affect sibling `dir2/` keys.
- `MKCOL` writes a zero-byte directory marker.
- `PUT`, `GET`, `HEAD`, and `DELETE` work for files.
- `GET` uses a seekable read path so `http.ServeContent` can serve WebDAV file reads without `seeker can't seek` / Finder `Interrupted system call` failures.
- Zero-byte `PUT` writes metadata without Telegram upload and can be read back as an empty file.
- Missing-file `PROPFIND` probes return `404` without error log noise.
- File `PROPFIND allprop` after opening an object does not trigger a superfluous `WriteHeader` path from a false missing-file `Stat`.
- Successful file and directory `DELETE` returns `204 No Content`.
- `DELETE` of a non-existent target returns `404 Not Found`.
- `DELETE` directory recursively removes metadata under the prefix.
- Same-bucket file `COPY` is metadata-only.
- Same-bucket file `MOVE` is metadata-only plus source delete.
- Same-bucket directory `COPY` recursively copies metadata.
- Same-bucket directory `MOVE` recursively moves metadata.
- Missing or malformed `Destination` returns `400`.
- `Destination` with a different host returns `403`; same-host absolute destinations and bare paths work.
- `Destination` path outside `webdav.prefix` returns `400`.
- `Overwrite: F` against an existing destination returns `412`.
- Unsupported `Overwrite` values return `400`.
- Existing destination metadata is overwritten when `Overwrite` is omitted or `T`.
- New destinations return `201`; replaced destinations return `204`.
- Directory copy/move into its own subtree returns `403`.
- Recursive copy/move is all-or-nothing in a metadata transaction; injected metadata failures leave source and destination unchanged.
- Recursive copy/move exceeding 100,000 objects is rejected before the transaction starts.
- Cross-bucket `COPY`/`MOVE` returns `403`.
- `OPTIONS` advertises `DAV: 1, 2` and writable collection methods for Finder compatibility, avoiding Finder treating the mount as a `read-only file system`.
- `LOCK`, `PUT`, and `UNLOCK` work as a class-2 client write sequence.

### Orphan bucket cleanup

- Configured bucket S3 `DeleteBucket` is forbidden.
- Configured bucket WebDAV `DELETE /dav/{bucket}` is forbidden.
- Removing a bucket from config makes its metadata bucket an orphan.
- Orphan buckets are not listed by `PROPFIND /dav/`.
- Orphan bucket `PROPFIND /dav/{bucket}/` returns `403`.
- Orphan bucket S3 `DeleteBucket` deletes bucket/object/chunk metadata.
- Orphan bucket WebDAV `DELETE /dav/{bucket}` and `DELETE /dav/{bucket}/` delete bucket/object/chunk metadata.
- Unknown bucket WebDAV `DELETE /dav/{bucket}` returns `404`.
- Object-level orphan bucket DAV methods return `403`; bucket-root `DELETE` remains the only cleanup action.
- Orphan cleanup does not call Telegram.

### Health endpoints

- `/healthz` and `/readyz` return operational status in root, S3-only, and DAV-only modes.
- Health endpoints are served before S3/WebDAV routing and are not protected by WebDAV Basic Auth.

## Documentation Updates

Update README and default config template to show:

- `tgnas` command name and `TGNAS_*` environment variable examples;
- root command combined S3 + WebDAV behavior;
- `tgnas s3` and `tgnas dav` single-protocol modes;
- configurable `webdav.prefix`;
- WebDAV Basic Auth credential reuse;
- directory marker behavior;
- same-bucket metadata-only `COPY`/`MOVE` limitation;
- orphan bucket cleanup rules.
