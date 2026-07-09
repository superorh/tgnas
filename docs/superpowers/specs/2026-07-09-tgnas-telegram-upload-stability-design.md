# TgNAS Telegram Upload Stability Design

## Goal

Make batch uploads complete reliably when Telegram Bot API returns `429 Too Many Requests`, even if this reduces throughput. Stability takes priority over upload speed.

## Constraints

- Prefer completion over throughput.
- It is acceptable to make Telegram uploads effectively serial for a single bot.
- Avoid introducing a durable background queue or a new subsystem for this iteration.
- Keep changes focused on the existing `store` and `telegram` packages.
- Preserve current orphan logging behavior when Telegram upload succeeds but metadata commit fails.

## Problem Summary

Current single-object uploads in `store.putSingle` stream request bytes through an `io.Pipe` directly into the Telegram client. When Telegram returns a retryable error such as `429 Too Many Requests`, the client can only safely retry if the upload body implements `io.ReadSeeker`.

That requirement is already encoded in the Telegram client retry path, but the current `io.Pipe` upload body is not replayable. As a result, batch uploads may fail with:

```text
telegram upload cannot safely retry non-seekable reader after failure: Too Many Requests: retry after N
```

Chunked upload paths do not have the same core problem because they already materialize chunk bytes before each Telegram call, although they still do unnecessary byte copying.

## Options Considered

### 1. Serialize uploads, make bodies replayable, and honor a shared 429 cooldown

This option keeps the existing synchronous upload flow but changes the store upload boundary so Telegram uploads are serialized and each upload body can be replayed across retries.

Pros:

- Directly fixes the current `non-seekable reader` failure mode.
- Matches the requirement to favor completion over throughput.
- Keeps the implementation inside existing `store` and `telegram` boundaries.
- Reuses the existing retry logic already present in the Telegram client.

Cons:

- Reduces upload throughput, especially during batch operations.
- Adds local buffering overhead for single uploads.

### 2. Only serialize uploads and wait longer on retry

This option reduces pressure on Telegram by forcing low concurrency but leaves the single-upload request body structure unchanged.

Pros:

- Smallest code change.
- May reduce the frequency of 429 responses.

Cons:

- Does not solve the core failure mode when a retryable error still occurs.
- Leaves `putSingle` unable to replay an upload body after failure.

### 3. Stage all uploads locally and send them through a dedicated worker queue

This option writes all uploads to local staging first, then sends them through a single sequential worker.

Pros:

- Strong control over upload pacing.
- Clear separation between ingest and Telegram delivery.

Cons:

- Introduces a larger queueing subsystem than the current problem requires.
- Raises new design questions around persistence, recovery, cleanup, and observability.

## Recommended Approach

Use option 1: serialize uploads, make single-upload bodies replayable, and apply a shared Telegram cooldown after `429` responses.

This design fits the requirement to prioritize successful completion, addresses the actual root cause of the observed failure, and keeps the implementation small enough for the current codebase.

## Architecture

The existing `telegram.Client` remains the component that decides whether an individual HTTP response is retryable and how long a specific retry should wait. The `store` package remains the single S3/WebDAV-facing caller of Telegram upload operations.

The main architectural change is that `store.uploadTelegram` becomes the stable coordination point for upload pacing rather than just a thin semaphore wrapper. It will still be the only place in `store` that invokes `tg.Upload`, but it will also enforce serialized upload entry and shared cooldown waiting between uploads.

This upload gate applies only to upload operations. Download operations keep their existing path and continue to use the current Telegram call concurrency controls without going through the new upload gate.

Single-object uploads will stop streaming directly from `input.Body` into Telegram. Instead, they will first produce a local replayable upload source, then hand that source to the existing Telegram client retry machinery.

Chunked and multipart-part uploads will keep their current chunk-by-chunk flow, but each chunk upload will use a replayable byte reader without converting chunk bytes through a Go string.

## Components

### Telegram upload gate in `store`

Add a lightweight upload gate owned by `store.ObjectStore`.

Responsibilities:

- Allow at most one Telegram upload request to be actively sent at a time.
- Hold that upload slot for the full duration of a single `tg.Upload` call, including the Telegram client's internal retries and any `retry_after` sleeps within that call.
- Track a shared cooldown deadline only when a `tg.Upload` call ultimately returns a final rate-limit (`429`) failure.
- Block later uploads until the cooldown expires or the request context is canceled.

Non-responsibilities:

- It does not decide retry attempt counts.
- It does not parse HTTP responses directly.
- It does not manage durable queue state.
- It does not govern download operations.

This keeps retry policy in the Telegram client and pacing policy in the store layer.

### Replayable single-upload source

Replace the current `io.Pipe`-based `putSingle` upload path with a two-phase flow:

1. Read the incoming object body once into a local replayable medium while computing object hashes.
2. Upload from that replayable medium using the existing Telegram client.

The replayable medium should use a size-based strategy derived from the existing upload buffer configuration rather than a new config knob:

- If `input.Size <= PutBufferSize`, use in-memory buffering and `bytes.Reader`.
- If `input.Size > PutBufferSize`, write to a temporary file and upload from `*os.File`.
- If `PutBufferSize <= 0`, fall back to `DefaultUploadConfig().PutBufferSize` before applying the rule.

This design intentionally reuses the existing `PutBufferSize` setting instead of adding a separate replay-buffer threshold.

Both choices satisfy the Telegram client requirement for `Seek(0)` on retry.

Temporary-file lifecycle rules:

- The temporary file must be closed before `putSingle` returns on every path.
- The temporary file must be deleted after upload success, after upload failure, after context cancellation, and after metadata commit failure following a successful upload.
- Cleanup is the responsibility of the single-upload staging path, not the Telegram client.
- If file removal itself fails, the upload should return the original operation result and only surface the cleanup problem through logging rather than replacing the main upload error.

### Replayable chunk readers

Chunked object uploads and multipart-part uploads already materialize each chunk as `[]byte`. These paths should switch from `strings.NewReader(string(partData))` to `bytes.NewReader(partData)`.

This keeps the current behavior while avoiding an unnecessary byte-to-string copy and preserving replayability.

### Retry metadata surface from `telegram`

The store upload gate needs to know when a Telegram request ultimately failed with `429` and how long the cooldown should last. That information should be exposed through a recognizable error shape from the `telegram` package rather than extracted from formatted error text.

A small typed error or wrapper is sufficient as long as it preserves:

- whether the final returned failure was caused by rate limiting
- the `retry_after` duration when present on that final returned failure
- the original underlying error for logging and wrapping

The Telegram client should continue to perform per-request retries internally before returning the final error. If an upload encounters one or more retryable `429` responses internally and later succeeds, that transient rate-limit history does not need to be surfaced to the store upload gate.

## Data Flow

### Single object upload

1. `PutObject` resolves the upload strategy.
2. If the object uses the single-upload path, `putSingle` reads the incoming body into a replayable source while computing MD5 and SHA256.
3. `putSingle` calls `uploadTelegram` with the replayable reader.
4. The upload gate waits for any active cooldown and then admits the upload.
5. The Telegram client performs the HTTP upload.
6. If Telegram responds with a retryable error, the client rewinds the reader and retries according to its retry policy.
7. If the upload ultimately fails with a returned `429`, the gate records the cooldown deadline before returning the error.
8. If the upload eventually succeeds after internal retries, the gate does not record an additional shared cooldown from that transient rate-limit history.
9. If upload succeeds, metadata is written exactly as today.

### Chunked and multipart-part upload

1. The store reads one configured chunk into `[]byte`.
2. The store creates `bytes.NewReader(partData)` for Telegram upload.
3. The upload passes through the same serialized upload gate.
4. Telegram retries remain local to the client.
5. On success, the existing metadata/chunk bookkeeping continues unchanged.

## Error Handling

`429 Too Many Requests` is treated as normal control flow for stability purposes, not as an immediate terminal failure.

The upload should fail only when one of the following is true:

- request context expires or is canceled
- local replayable staging fails
- Telegram returns a non-retryable error
- Telegram exhausts retry attempts and still fails
- metadata commit fails after a successful Telegram upload

The design does not add remote cleanup of already uploaded Telegram messages after partial failures. Existing orphan logging remains the recovery mechanism because it matches the current codebase and avoids introducing a second failure path.

If the Telegram client returns a final rate-limit error after exhausting retries, the upload gate should still record the cooldown so later uploads do not immediately hit the same limit. If the Telegram client absorbs transient `429` responses internally and ultimately succeeds, that transient history does not update the shared cooldown.

## Concurrency and Pacing

This design intentionally optimizes for stability by reducing concurrency.

Behavioral target:

- At most one active Telegram upload request per process at a time.
- After a `429`, later uploads wait until the known cooldown expires before sending the next request.
- Existing `MaxTelegramCalls` remains compatible for download behavior.
- For upload behavior, `MaxTelegramCalls` no longer increases effective upload concurrency; uploads remain serial even if the configured Telegram call limit is higher than one.
- Uploads and downloads do not share the new upload gate.

This means batch uploads may become significantly slower, but they should stop failing due to unreplayable request bodies under normal rate-limit conditions.

## Testing

Use focused tests in `store` and `telegram`.

### Telegram tests

- Upload retries succeed with a replayable reader after a `429` response that includes `retry_after`.
- Returned error shape preserves rate-limit metadata for callers.

### Store tests

- `putSingle` succeeds when the source reader itself is not seekable but Telegram returns one retryable `429` before success.
- The single-upload path uses replayable local staging rather than direct pipe streaming.
- A second upload waits for a prior cooldown recorded by the upload gate.
- Upload serialization does not force download serialization; download behavior remains governed by the existing Telegram concurrency controls.
- Temporary files are removed on upload success, upload failure, context cancellation, and metadata commit failure after upload success.
- Chunked upload behavior remains correct when switching to `bytes.NewReader`.
- Multipart-part upload behavior remains correct when switching to `bytes.NewReader`.
- Existing orphan logging behavior remains unchanged when metadata commit fails after upload success.

### Regression target

Add at least one test that reproduces the current observed class of failure and proves it no longer fails with `telegram upload cannot safely retry non-seekable reader after failure`.

## Non-Goals

This design does not include:

- a durable on-disk upload queue
- cross-process coordination between multiple TgNAS instances
- dynamic rate-limit tuning based on historical Telegram behavior
- changes to download concurrency behavior
- remote deletion of Telegram messages as compensating cleanup

## Implementation Notes

Keep the code changes concentrated in the existing files where the behavior already lives:

- `store/store.go` for upload coordination and replayable staging
- `telegram/client.go` for exposing recognizable rate-limit error metadata
- related tests in `store/store_test.go` and `telegram/client_test.go`

The design should not introduce a broad abstraction layer or a generic queue framework. The goal is a narrow reliability fix aligned with the current code structure.
