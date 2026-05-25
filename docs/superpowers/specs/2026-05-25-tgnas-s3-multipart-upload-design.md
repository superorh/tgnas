# TgNAS S3 Multipart Upload Design

This design adds the minimal S3 multipart upload lifecycle needed by common S3 clients:

1. `CreateMultipartUpload`
2. `UploadPart`
3. `CompleteMultipartUpload`
4. `AbortMultipartUpload`

The implementation uploads each received S3 part directly to Telegram in internal chunks, keeps unfinished upload metadata in memory, and writes final object metadata only when the client completes the multipart upload.

## Goals

- Support practical S3 multipart uploads from SDKs and compatible clients.
- Avoid local full-object staging and avoid re-uploading data at complete time.
- Keep unfinished multipart state out of final object listings and reads.
- Reuse existing Telegram upload, object metadata, range-read, auth, and error response patterns where possible.
- Allow final objects to contain chunks with different sizes.
- Return S3 multipart-style final ETags.

## Non-Goals

- No durable resume of unfinished multipart uploads after process restart.
- No `ListParts` support.
- No `ListMultipartUploads` support.
- No multipart copy support.
- No checksum algorithm support beyond ETag/MD5 compatibility.
- No server-side encryption behavior.
- No remote cleanup of already-uploaded Telegram messages unless deletion support already exists in the Telegram client.

## S3 API Surface

### CreateMultipartUpload

Request:

```http
POST /{bucket}/{key}?uploads
```

Behavior:

- Requires normal SigV4 authentication.
- Verifies that the bucket exists.
- Creates an in-memory upload session.
- Records bucket, key, content type, creation time, and upload ID.
- Enforces a maximum of 1000 unfinished multipart upload sessions. If creating a new session would exceed the limit, TgNAS aborts and removes the oldest unfinished session before returning the new upload ID.
- Returns `InitiateMultipartUploadResult` XML containing bucket, key, and upload ID.

### UploadPart

Request:

```http
PUT /{bucket}/{key}?partNumber={n}&uploadId={id}
```

Behavior:

- Requires normal SigV4 authentication.
- Requires a valid upload ID for the same bucket and key.
- Requires `partNumber >= 1`.
- Requires a non-negative `Content-Length`.
- Streams the request body into Telegram uploads using existing configured Telegram chunk size and type limits.
- A single S3 part may produce multiple internal Telegram chunks.
- Computes the S3 part ETag as the MD5 of the original S3 part bytes.
- Records, in memory, the part number, part ETag, total part size, and internal Telegram chunk metadata.
- Re-uploading the same part number replaces the in-memory metadata for that S3 part. Previously uploaded Telegram chunks become orphaned and are logged.
- Returns the part ETag in the `ETag` response header.

### CompleteMultipartUpload

Request:

```http
POST /{bucket}/{key}?uploadId={id}
```

Body contains S3 `CompleteMultipartUpload` XML with ordered part numbers and ETags.

Behavior:

- Requires normal SigV4 authentication.
- Requires a valid upload ID for the same bucket and key.
- Parses the XML part list.
- Requires part numbers to be strictly increasing.
- Requires every referenced part to exist in the in-memory session.
- Requires every submitted ETag to match the ETag returned by `UploadPart`, ignoring surrounding quotes.
- Expands the referenced S3 parts into a final ordered `[]metadata.Chunk`.
- Assigns final object chunk numbers globally from `1..N`, independent of the original S3 part numbers.
- Computes each final chunk offset from the preceding chunk sizes, so chunks may have different sizes.
- Computes final object size as the sum of referenced S3 part sizes.
- Records per-chunk SHA256 values computed while streaming each internal Telegram chunk.
- Does not claim a true whole-object SHA256 for multipart objects, because S3 parts may upload out of final order and SHA256 digests cannot be combined after the fact without rereading object bytes.
- Computes the final object ETag using S3 multipart ETag semantics: `md5(concat(part_md5_bytes))-part_count`.
- Writes the final `metadata.Object` and `object_chunks` in one metadata operation, using an empty object-level SHA256 value for multipart objects.
- Removes the in-memory upload session only after the final metadata write succeeds.
- Returns `CompleteMultipartUploadResult` XML with bucket, key, and final ETag.

### AbortMultipartUpload

Request:

```http
DELETE /{bucket}/{key}?uploadId={id}
```

Behavior:

- Requires normal SigV4 authentication.
- Removes the in-memory upload session.
- Logs any already-uploaded Telegram chunks as orphaned if remote deletion is unavailable.
- Returns `204 No Content`.

## Routing

Extend object routing in `internal/s3api/server.go`:

- `POST` with query key `uploads` calls CreateMultipartUpload.
- `PUT` with `partNumber` and `uploadId` calls UploadPart.
- `POST` with `uploadId` calls CompleteMultipartUpload.
- `DELETE` with `uploadId` calls AbortMultipartUpload.
- Existing single-object `PUT`, `GET`, `HEAD`, and `DELETE` behavior remains unchanged.

## Store Design

Add multipart methods to the S3-facing store interface and implement them on `store.ObjectStore`:

- `CreateMultipartUpload(ctx, input)`
- `UploadPart(ctx, input)`
- `CompleteMultipartUpload(ctx, input)`
- `AbortMultipartUpload(ctx, input)`

The store keeps a mutex-protected in-memory map of upload ID to session.

The store allows at most 1000 unfinished upload sessions. When a new session pushes the map over that limit, the oldest session by creation time is evicted. Eviction uses the same cleanup path as abort: remove the in-memory session and log already-uploaded Telegram chunks as orphaned when remote deletion is unavailable.

Each session contains:

- bucket
- key
- content type
- creation time
- map of S3 part number to part metadata

Each S3 part contains:

- part number
- S3 part ETag
- S3 part MD5 bytes
- total S3 part size
- one or more staged internal Telegram chunks

Each internal Telegram chunk contains the data needed to become a final `metadata.Chunk`:

- Telegram type
- Telegram file ID
- Telegram message ID
- Telegram file unique ID
- size
- SHA256 of that internal chunk

Unfinished multipart uploads are not written to `objects` or `object_chunks`.

## Metadata and Chunk Semantics

The final object uses the existing `objects` and `object_chunks` tables.

A final object may have chunks with different sizes. This is already compatible with the current read model because each chunk records its own size and offset, and range reads select chunks by those recorded ranges.

Final chunk numbering is independent of S3 part numbering. For example:

- S3 part 1 uploads as internal chunks 1, 2, 3.
- S3 part 2 uploads as internal chunks 4, 5.
- The completed object stores final `object_chunks.part_number` values `1..5`.

## Error Mapping

Add store and S3 API errors for multipart validation:

- Missing upload session: `NoSuchUpload`, HTTP 404.
- Missing referenced part or ETag mismatch: `InvalidPart`, HTTP 400.
- Non-increasing complete part list: `InvalidPartOrder`, HTTP 400.
- Invalid upload ID, part number, malformed XML, or invalid query: `InvalidArgument`, HTTP 400.
- Oversized individual Telegram chunk according to configured upload limits: existing entity-too-large behavior.

## Security and Privacy

- All multipart operations require the existing SigV4 authentication path.
- Public-read buckets do not allow anonymous multipart operations.
- Presigned URL support remains limited to object `GET` and `HEAD`; presigned multipart writes are not added in this design.
- Logs must not include secrets, private domains, raw file IDs, or request bodies.

## Failure Behavior

- If the process restarts, unfinished upload sessions are lost. Subsequent `UploadPart`, `CompleteMultipartUpload`, or `AbortMultipartUpload` calls return `NoSuchUpload`.
- If more than 1000 unfinished upload sessions are created, the oldest unfinished session is evicted. Later operations for that upload ID return `NoSuchUpload`.
- If `UploadPart` uploads some Telegram chunks and then fails, already-uploaded chunks are logged as orphans and the part is not recorded as complete.
- If `CompleteMultipartUpload` fails before metadata write, the upload session remains available for retry.
- If metadata write succeeds but the response fails to reach the client, retrying complete with the same upload ID may return `NoSuchUpload`; the object itself has already been committed.

## Tests

Use TDD for implementation.

Server-level tests in `internal/s3api/server_test.go`:

- Create returns `InitiateMultipartUploadResult` with bucket, key, and upload ID.
- UploadPart returns ETag and does not make the object visible before Complete.
- Complete with two S3 parts returns multipart ETag and makes `GET` return concatenated content.
- A S3 part larger than one internal Telegram chunk completes into multiple final object chunks.
- Abort removes the upload session and later part/complete calls return `NoSuchUpload`.
- Complete rejects missing parts with `InvalidPart`.
- Complete rejects non-increasing part order with `InvalidPartOrder`.
- Existing single `PutObject` behavior remains unchanged.

Store-level tests in `store`:

- UploadPart records multiple internal chunks when the configured chunk size requires splitting.
- Complete expands S3 parts into final chunks with continuous offsets and variable sizes.
- Complete computes S3 multipart ETag as `md5(concat(partMD5s))-partCount`.
- Abort removes the in-memory session.
- Creating the 1001st unfinished upload evicts the oldest session, and later operations for the evicted upload ID return `NoSuchUpload`.

Verification commands:

```bash
go test ./internal/s3api -run 'Multipart|CreateMultipartUpload|UploadPart|CompleteMultipartUpload|AbortMultipartUpload'
go test ./store -run Multipart
go test ./...
```
