# Go Telegram S3 Storage Design

## Goal

Build a Go project that provides a reusable Telegram-backed object storage library and a container-friendly S3-compatible HTTP service. The service stores object bytes in Telegram chats through the Telegram Bot API, stores metadata locally through an abstract metadata interface with a SQLite implementation, and exposes a focused S3 Object API surface.

The reference implementation is `/opt/dev/misc/flysystem-telegram`, a PHP Flysystem adapter that uses Telegram messages as object storage, SQLite for metadata, typed Telegram uploads, document fallback, and chunked reads for large files.

## Scope

First version includes:

- Go library for bucket/object storage.
- S3-compatible service binary.
- Multiple buckets as metadata namespaces.
- Bucket-to-Telegram-chat mapping from configuration.
- AWS Signature V4 header authentication.
- Object API operations for bucket/object access.
- Telegram `sendDocument` uploads by default, with optional typed uploads, fallback to `sendDocument`, and configurable message captions.
- Chunked large-object upload and streamed full/Range reads.
- SQLite metadata backend behind an interface.
- Container/Kubernetes-friendly configuration and health checks.
- SHA256 and ETag integrity metadata.

First version excludes:

- S3 Multipart Upload API.
- Presigned GET/PUT URLs.
- Presigned POST form. This is a second-version candidate.
- ACL, policy, IAM, versioning, object tagging, lifecycle, and server-side encryption.
- Production multi-writer horizontal scaling with SQLite.

## Architecture

The module is split into a reusable core and service entrypoint:

- `cmd/tgs3`: loads config, initializes dependencies, and starts the HTTP service.
- `internal/s3api`: minimal S3 Object API protocol layer. It handles routing, Signature V4, S3 XML responses/errors, Range parsing, and request/response translation. It depends only on the object store interface.
- `store`: reusable object storage library. It exposes bucket/object operations and coordinates metadata, Telegram uploads/downloads, chunking, hashes, and object-level locking.
- `telegram`: Telegram Bot API client abstraction and HTTP implementation. It supports typed uploads and downloads by `file_id`.
- `metadata`: metadata interfaces and SQLite implementation.
- `config`: config loading from file plus environment-variable references/overrides.

The S3 layer does not know Telegram details. The Telegram layer does not know S3 details. `store` is the orchestration boundary. This lets the first version implement the minimal protocol in-house while keeping the S3 protocol layer replaceable later.

## S3 API Surface

Supported in v1:

- `PUT /{bucket}`: CreateBucket is disabled in v1 and returns `NotImplemented`. Buckets are initialized from server configuration at startup.
- `HEAD /{bucket}`: HeadBucket.
- `GET /{bucket}`: ListObjectsV2 with `prefix`, `delimiter`, `continuation-token`, and `max-keys`. Results are ordered by object key ascending. `continuation-token` is an opaque base64url-without-padding JSON value containing `last_key`; the next page starts after `last_key`. Invalid tokens return `InvalidArgument`.
- `PUT /{bucket}/{key}`: PutObject.
- `GET /{bucket}/{key}`: GetObject with HTTP `Range` support.
- `HEAD /{bucket}/{key}`: HeadObject.
- `DELETE /{bucket}/{key}`: DeleteObject.
- `GET /`: ListBuckets by default, with room for a future frontend page.

Root request handling uses S3-first content negotiation:

1. If the request has any S3 characteristic, handle it as S3 `ListBuckets` or an S3 XML auth error. S3 characteristics are: `Authorization` beginning with `AWS4-HMAC-SHA256`, `X-Amz-Date`, `X-Amz-Content-Sha256`, any `x-amz-*` header, or `Accept` explicitly preferring `application/xml`.
2. If the request has no S3 characteristics and `Accept` explicitly prefers `text/html`, reserve it for a future frontend page.
3. Otherwise, default to S3 `ListBuckets` for client compatibility.

Path-style addressing is the primary target: `http://host:port/bucket/key`. Virtual-hosted style is out of scope for v1.

## Authentication

V1 supports AWS Signature Version 4 header authentication using static credentials from config/env. Presigned URLs and presigned POST forms are deferred to v2.

Credential config contains one or more access keys and secret-key sources. Secrets may be read from environment variables so container deployments do not need plaintext secrets in config files.

The implementation returns S3 XML auth errors such as `InvalidAccessKeyId` and `SignatureDoesNotMatch`.

## Bucket Semantics

Buckets are metadata namespaces. They do not create directories or resources in Telegram.

Bucket mappings are predefined in configuration:

```yaml
buckets:
  photos:
    chat_id: "-100123"
  backups:
    chat_id: "-100456"
```

Configured buckets are initialized into metadata at startup. `CreateBucket` is disabled in v1 because S3 has no standard place to supply the Telegram `chat_id`, and accepting it through non-standard request data would expose deployment-specific storage topology. `PUT /{bucket}` returns `NotImplemented`. Object operations against missing or disabled buckets return `NoSuchBucket`.

`DeleteBucket` is not required in v1. If added later, it should only allow empty buckets and should affect metadata only.

## Metadata Model

The metadata interface must support replacement by another backend later. SQLite is the default v1 implementation.

Core tables:

### `buckets`

- `name`
- `chat_id`
- `created_at`
- `enabled`

### `objects`

- `bucket`
- `key`
- `size`
- `content_type`
- `etag`
- `sha256`
- `last_modified`
- `chunk_count`
- `telegram_type`
- `upload_strategy`: `typed`, `document`, or `chunked_document`

### `object_chunks`

- `bucket`
- `key`
- `part_number`
- `offset`
- `size`
- `telegram_type`
- `telegram_file_id`
- `telegram_message_id`
- `telegram_file_unique_id`
- `sha256`

`DeleteObject` removes metadata only. It does not delete Telegram messages or files. Replacing an object uploads new Telegram data and swaps metadata; old Telegram files become orphaned.

## Telegram Upload Strategy

The Go implementation defaults to byte-preserving `sendDocument` uploads because S3 object storage requires `GetObject` bytes to match `PutObject` bytes. The optional `auto` strategy mirrors the reference project's typed upload behavior for users who prefer Telegram media previews and accept that Telegram typed media may not preserve original bytes for every file type.

Supported Telegram types:

- `photo` via `sendPhoto`
- `video` via `sendVideo`
- `audio` via `sendAudio`
- `animation` via `sendAnimation`
- `document` via `sendDocument`

Config:

- `upload_type_strategy`: `document` or `auto`; default is `document`.
- `enable_chunking`: default is `true`.
- `max_file_size`
- `chunk_size`
- `type_size_limits`

Default size limits:

- `photo`: 10 MiB
- `video`: 20 MiB
- `audio`: 20 MiB
- `animation`: 20 MiB
- `document`: 20 MiB

The `document` strategy always uses `sendDocument` for non-chunked uploads. Type inference applies only in `auto` mode:

- `image/gif` -> `animation`
- `image/*` -> `photo`
- `video/*` -> `video`
- `audio/*` -> `audio`
- everything else -> `document`

The MIME type comes from S3 `Content-Type` when present, then extension inference, then falls back to `document`.

Resolution rules:

1. If size is within the inferred type limit, upload with the inferred Telegram method.
2. If inferred type is not `document`, size exceeds that type limit, and size fits the document limit, fall back to `sendDocument`.
3. If size exceeds the document limit and chunking is enabled, split into chunks and upload every chunk with `sendDocument`.
4. If chunking is disabled and the object exceeds the limit, return S3 `EntityTooLarge`.

The Telegram client exposes one upload method with a request containing type, chat ID, stream, filename, MIME type, and caption. The HTTP implementation maps type to Bot API method and multipart field.

## Telegram Caption Template

V1 supports an optional `telegram.caption_template`. If empty, uploads send no caption. If configured, the template is rendered for every Telegram upload.

Supported variables:

- `{bucket}`: bucket name.
- `{key}`: full object key.
- `{name}`: base name of the object key.
- `{size}`: human-readable object size.
- `{bytes}`: object size in bytes.
- `{part}`: current chunk number for chunked uploads; `1` for non-chunked uploads.
- `{parts}`: total chunk count for chunked uploads; `1` for non-chunked uploads.
- `{chunk}`: chunk marker formatted as `part/parts` for chunked uploads; empty for non-chunked uploads.

Unknown variables make configuration validation fail at startup. Rendered captions are truncated to Telegram Bot API caption limits before upload. Chunked uploads render the same template for each chunk with `{part}`, `{parts}`, and `{chunk}` populated so users can distinguish parts.

## PutObject Flow

1. S3 layer validates Signature V4 and parses bucket, key, content type, content length, and headers.
2. Store checks that the bucket exists/enabled and loads its Telegram `chat_id`.
3. Store streams the request body while computing MD5 for ETag and SHA256 for integrity metadata.
4. Upload strategy chooses typed upload, document fallback, or chunked document upload.
5. Single-file upload writes one object metadata row.
6. Chunked upload writes chunk metadata with offsets and sizes, then commits object metadata.
7. Metadata is committed after Telegram upload succeeds. If Telegram upload succeeds but metadata commit fails, the file is orphaned in Telegram and an `orphan_upload` log event is emitted.

V1 requires `Content-Length` for PutObject so the upload strategy can be chosen before upload. Unknown-length PutObject requests return `MissingContentLength`. Support for unknown-length streaming can be added later by buffering to temp files or chunking opportunistically.

## GetObject and Range Flow

For full reads:

- Single-file objects download one Telegram file stream.
- Chunked objects download chunks in `part_number` order and stream them sequentially.

For Range reads:

- The store uses chunk `offset` and `size` metadata to find chunks overlapping the requested range.
- It downloads only the required chunks.
- It skips prefix bytes in the first chunk and truncates output after the requested length.
- Single-file Range reads download the Telegram file and crop the stream.

Responses include:

- `Content-Length`
- `Content-Range` for partial responses
- `Accept-Ranges: bytes`
- `ETag`
- `Content-Type`
- `Last-Modified`

ETag is the object content MD5 for v1, including chunked objects. Metadata stores the lowercase 32-character hex MD5 without quotes; HTTP/S3 responses return it as a quoted ETag, for example `"d41d8cd98f00b204e9800998ecf8427e"`. SHA256 is stored separately for integrity verification. The ETag does not use S3 multipart ETag syntax because S3 Multipart Upload is not implemented in v1. V1 does not implement conditional request semantics such as `If-Match` or `If-None-Match`.

## Configuration and Deployment

The service is container/Kubernetes-friendly. V1 supports YAML configuration files only, with environment-variable references for secrets, paths, and small deployment overrides. The SQLite database location is explicitly configurable and must not depend on the process working directory.

Example:

```yaml
server:
  listen: ":9000"
  public_base_url: "https://s3.example.com"

auth:
  region: "us-east-1"
  credentials:
    - access_key: "admin"
      secret_key_env: "TGS3_SECRET_KEY"

telegram:
  bot_token_env: "TELEGRAM_BOT_TOKEN"
  api_base_url: "https://api.telegram.org"
  timeout: "30s"
  caption_template: |
    {key}
    name: {name}
    size: {size}

metadata:
  driver: "sqlite"
  sqlite_path: "/data/tgs3.sqlite"
  sqlite_path_env: "TGS3_SQLITE_PATH"

storage:
  upload_type_strategy: "document"
  enable_chunking: true
  max_file_size: 52428800
  chunk_size: 20971520
  type_size_limits:
    photo: 10485760
    video: 20971520
    audio: 20971520
    animation: 20971520
    document: 20971520
  max_concurrent_uploads: 4
  max_concurrent_downloads: 16
  max_concurrent_telegram_requests: 8
  put_buffer_size: 1048576

buckets:
  photos:
    chat_id: "-100123"
  backups:
    chat_id: "-100456"
```

SQLite path resolution order:

1. If `metadata.sqlite_path_env` is set and the named environment variable is non-empty, use that value.
2. Otherwise, use `metadata.sqlite_path`.
3. If neither resolves to a non-empty path, startup fails.

Container requirements:

- Telegram bot token.
- YAML config file and/or env vars.
- Configured SQLite path through `metadata.sqlite_path` or `metadata.sqlite_path_env`.
- Persistent volume mounted at the configured SQLite path or its parent directory.
- HTTP port exposure.

Health endpoints:

- `GET /healthz`: process liveness.
- `GET /readyz`: config loaded, SQLite initialized, and Telegram bot token basic validation has passed.

## Performance and Concurrency

V1 prioritizes correctness and bounded resource usage over high throughput.

Concurrency controls:

- `max_concurrent_uploads`
- `max_concurrent_downloads`
- `max_concurrent_telegram_requests`
- `put_buffer_size`

The Telegram client uses a semaphore to limit Bot API requests and reduce rate-limit risk.

PutObject:

- Small objects stream to Telegram while hashes are computed.
- Large objects are chunked and uploaded sequentially in v1.
- `chunk_upload_concurrency` can be added later, but the initial default is sequential upload to reduce memory, retry, and rate-limit complexity.

GetObject:

- Full chunked reads stream chunks sequentially.
- Range reads download only required chunks.
- No local download cache in v1.
- Response buffering is bounded and configurable.

Consistency:

- Same `bucket + key` Put/Delete operations use an object-level keyed mutex in the process.
- Metadata writes use SQLite transactions.
- SQLite mode is recommended for single-writer/single-replica deployment. Kubernetes is supported as a packaging target, but horizontal write scaling requires a future PostgreSQL metadata backend plus distributed locking or optimistic versioning.

Telegram failures:

- 429 and 5xx responses get bounded retries with exponential backoff.
- Telegram `retry_after`, when available, controls backoff.
- Failed chunk upload fails the whole PutObject. Already uploaded chunks may become orphans and are logged.
- Download failures return S3 `ServiceUnavailable` or interrupt the response after configured retries.

## Error Handling

S3 layer emits XML errors with `Code`, `Message`, `Resource`, and `RequestId`.

Mappings:

- Unknown access key -> `InvalidAccessKeyId`
- Signature mismatch -> `SignatureDoesNotMatch`
- `CreateBucket` / `PUT /{bucket}` -> `NotImplemented`
- Missing/disabled bucket during object operations -> `NoSuchBucket`
- Missing object -> `NoSuchKey`
- Invalid Range -> `InvalidRange`
- Missing `Content-Length` on PutObject -> `MissingContentLength`
- Object too large without chunking -> `EntityTooLarge`
- Telegram rate limit or temporary failure -> `ServiceUnavailable`
- Unexpected Telegram/API/metadata errors -> `InternalError`

Logs are structured and include request ID, bucket, key, error code, and Telegram status where safe. Logs must not include bot tokens, secret keys, or complete canonical signature material.

## Testing Strategy

Unit tests:

- Signature V4 canonical request and signature validation.
- S3 routing and XML error generation.
- Upload strategy: typed upload, document fallback, chunked document.
- Range-to-chunk selection and stream cropping.
- SQLite metadata CRUD and transactions.
- Object-level locking behavior.

Integration tests:

- Local HTTP service with fake Telegram client.
- Real SQLite temp database.
- AWS SDK for Go v2 object operations.
- MinIO client and/or rclone smoke tests against path-style endpoint.

Optional real Telegram tests:

- Require explicit env vars: `TELEGRAM_BOT_TOKEN`, `TELEGRAM_TEST_CHAT_ID`.
- Skipped by default in CI to avoid sending messages accidentally.

Acceptance criteria:

- Two configured buckets write to different Telegram chat IDs.
- With default `document` strategy, small image uploads use `sendDocument` to preserve bytes.
- With optional `auto` strategy, small image uploads use `sendPhoto` and oversized images within the document limit fall back to `sendDocument`.
- Large object uploads as document chunks.
- Full and Range reads return bytes matching the original object.
- HeadBucket and Put/Get/Head/Delete/ListObjects work with common S3 clients using SigV4 header auth against buckets configured at startup.
- S3 errors are XML and use expected S3 codes.

## V2 Candidates

- Presigned GET/PUT URLs.
- Presigned POST form uploads.
- Frontend page at root for browser requests.
- Virtual-hosted style routing.
- S3 Multipart Upload API.
- PostgreSQL metadata backend and multi-replica writes.
- Local read-through cache.
- Concurrent chunk uploads.
- Optional client-side encryption before Telegram upload.
