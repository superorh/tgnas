# TgNAS Bucket Bot Token Design

## Goal

Allow different buckets in the same TgNAS process to use different Telegram Bot Tokens so data can be written through different Telegram bots. A bucket-level token is optional and falls back to the global token when unset. At the same time, change upload serialization and `429` cooldown handling from a single global gate to token-scoped gates.

## Constraints

- Add global `telegram.bot_token` with `${ENV}` resolution support
- Add `buckets.<name>.bot_token` with `${ENV}` resolution support
- Keep `telegram.bot_token_env` for backward compatibility
- Precedence: `bucket.bot_token` > global `telegram.bot_token` > `telegram.bot_token_env`
- On startup, only check whether the final resolved token is empty; do not pre-validate token authenticity with Telegram API calls
- If the final resolved token for any configured bucket is empty, service startup must fail
- Tokens must not be written to metadata/SQLite; they only exist in config and runtime memory
- Only buckets explicitly declared in config are supported; dynamic or implicit buckets are not supported at runtime
- All Telegram calls related to a configured bucket must use that bucket's final resolved token
- Upload serialization and `429` cooldown must be isolated per token; different tokens must not affect each other
- Only Telegram errors that can be clearly classified as bucket-level token authentication/authorization failures may mark a bucket as runtime-unavailable; HTTP `403` alone must not trip bucket-wide unavailability without considering operation type and error semantics
- Object-level read failures, such as old objects becoming unreadable after a token change or a `file_id` being inaccessible under the current bot, must not mark the whole bucket unavailable
- Logs must include bucket, operation type, token source, and failure reason, but must not print the raw token, derived token key, or any reversible identifier

## Problem Summary

The current `config` only supports global `telegram.bot_token_env`, and `store` only keeps a single global `telegram.Client`, so all buckets share one bot. At startup, each bucket is only written with `chat_id` and `enabled` state, with no token concept attached to the bucket. The goal is to isolate different buckets onto different bots while preserving the default fallback behavior.

## Options Considered

### 1. Per-bucket Telegram client + per-token upload gate

Support bucket-level tokens in config and startup wiring, then select the corresponding Telegram client at runtime by bucket. Replace the single global upload gate with a token-keyed gate map.

Pros:
- Directly satisfies the requirement
- Clear boundaries: config resolves precedence, main builds runtime bindings, store selects by bucket
- Upload pacing and cooldown naturally isolate by token with no extra workaround
- Failure handling rules are straightforward to embed

Cons:
- Requires changes to the store constructor and runtime bucket resolution path
- Upload gate changes from a single instance to a map, so lifecycle and concurrency must be handled
- Client count grows from 1 to N

### 2. Single client, pass token per request

Keep one `telegram.Client`, but pass the token into every request.

Pros:
- Client count stays unchanged
- Smaller surface area of changes

Cons:
- `telegram.Client` interface must change and becomes messy
- Token-scoped pacing/cooldown is awkward to isolate inside one client
- Conflicts with the requirement that all bucket-related Telegram calls must follow the bucket token

### 3. Global client with bucket token override on a few paths

Keep a global client and only override token on a few selected paths.

Pros:
- Smallest code change

Cons:
- Conflicts with the requirement that all bucket-related Telegram calls must follow the bucket token
- Easy to miss paths as the code evolves
- Token-scoped pacing/cooldown still needs hacks

## Recommended Approach

Use option 1: support bucket-level tokens and global precedence in config, build Telegram clients per bucket at runtime, prebuild upload gates per unique token, and isolate pacing/cooldown by token.

## Architecture

The config layer resolves token source and precedence into a runtime view that only covers configured buckets. The startup layer creates a Telegram client for each bucket from that view and prebuilds upload gates using deduplicated token keys. The store layer no longer keeps a single global client and instead selects the correct handle by bucket from the runtime view. The metadata layer remains token-free and only stores basic bucket information.

## Components

### Config token resolution

Extend `config.Config` with:
- A new `TelegramConfig.BotToken string` field with `${ENV}` resolution support
- A new `BucketConfig.BotToken string` field with `${ENV}` resolution support
- Resolution logic: `ResolveBucketToken(name string)` returns the final token and a source label
  - Source labels: `bucket`, `global`, `global_env`
  - Precedence: bucket `bot_token` > global `telegram.bot_token` > `telegram.bot_token_env`
- Empty propagation: if bucket token is unset or resolves to empty, fall back to global; if global also resolves to empty, return empty
- At startup, iterate over all configured buckets and fail if any bucket resolves to an empty final token
- Do not probe Telegram; only check whether the resolved token is empty

### Runtime bucket binding

Build the runtime view in `cmd/tgnas/main.go`:
- One record per configured bucket containing:
  - bucket name
  - `chat_id`
  - final token (non-empty, only checked for emptiness, no Telegram probe)
  - token source label
  - corresponding `telegram.Client`
  - a stable token key derived from the final token, used only in memory for pacing gate partitioning and never logged
- Pass this runtime view to `store.ObjectStore`
- Prebuild the gate map in `store` from the unique token keys in the runtime view; do not lazily create gates on first request
- Record token source per bucket in startup logs in redacted form

### Store per-bucket client selection

Change `store.ObjectStore` to:
- Remove the single global `tg telegram.Client` field
- Add a runtime bucket binding view, such as `map[string]telegramClientBinding`
- Resolve the bucket binding before every Telegram call site, including `putSingle`, `putChunked`, `uploadMultipartPartChunks`, and `downloadChunk`
- If a bucket has been marked runtime-unavailable, return an error immediately and log it instead of calling Telegram

### Per-token upload gate

Change `store.ObjectStore` to:
- Remove the single global `uploadGate` / `uploadMu` / `uploadUntil`
- Add `map[string]*uploadGate` (`token key -> gate`)
- Prebuild all gates once in `ObjectStore` construction from the unique token keys in the runtime binding view; at runtime the gate map is read-only and never lazily populated
- Let each gate independently maintain serialization and `429` cooldown
- Share one gate across multiple buckets that resolve to the same token
- Keep buckets with different tokens fully independent for serialization and cooldown
- Keep gate map construction on the single-threaded startup path so first-hit concurrent gate creation races do not exist
- Let gate lifecycle follow `ObjectStore`; no explicit shutdown handling is required

### Runtime unavailable bucket

Change `store.ObjectStore` to:
- Add `map[string]bool unavailableBuckets` (`bucket name -> unavailable state`)
- Only mark a bucket unavailable when an error can be stably classified as a bucket-level token authentication/authorization failure
- Introduce a normalized Telegram error classification with a minimum decision payload of: operation class (`upload_send`, `download_read`, or equivalent), HTTP status code if present, and a normalized reason enum/string that distinguishes bucket-level authorization failures from object-level read failures
- Base bucket-unavailable decisions on that normalized classification instead of raw HTTP status codes or ad hoc string matching; for example, only an upload/send-path `401 Unauthorized` or a normalized reason that clearly means the current bot is no longer allowed to send/access the bucket's chat may trip bucket-wide unavailability
- Treat object-level failures during reads, such as old `file_id` values becoming unreadable after a token switch, missing files, or request-scoped read permission issues, as request failures only and do not mark the bucket unavailable
- After a bucket is marked unavailable, make all later object access paths for that bucket (`HeadObject`, `GetObject`, `PutObject`, `ListObjects`, multipart) fail fast with an `ErrUnavailable`-style error instead of attempting Telegram calls
- Keep the state transition thread-safe with `sync.RWMutex` or `sync.Mutex`, and ensure repeated concurrent hits on the same bucket only produce one state change
- Log bucket name, operation type, token source, and failure reason without printing the raw token, token key, or reversible identifiers

## Data Flow

### Startup

1. Load config
2. Resolve global `telegram.bot_token` with `${ENV}` support, falling back to `bot_token_env`
3. Iterate through each configured bucket, resolve `bucket.bot_token` with `${ENV}` support, and fall back to the global token
4. Fail startup if any configured bucket ends with an empty final token
5. Create a `telegram.Client` for each configured bucket using token, `api_base_url`, and timeout
6. Build a runtime bucket binding view that only covers configured buckets and pass it to `store.ObjectStore`
7. Prebuild the upload gate map in `store` from deduplicated token keys
8. Start the service

### Object upload

1. `PutObject` resolves upload strategy
2. Look up the Telegram client from the runtime binding by bucket name
3. If the bucket is in the unavailable set, return an error immediately and log it
4. Hit the prebuilt upload gate for that token key and wait out any cooldown
5. Send the upload request through the bucket's Telegram client
6. If the error can be stably classified as a bucket-level token authentication/authorization failure, mark the bucket unavailable so later requests fail fast
7. If the failure is only request-scoped or object-scoped, return the current request error and log it without changing bucket availability
8. If upload succeeds, metadata writes remain unchanged

### Object download

1. `GetObject` loads object metadata and chunks
2. Look up the Telegram client from the runtime binding by bucket name
3. If the bucket is in the unavailable set, return an error immediately and log it
4. Each `downloadChunk` call fetches data through that bucket's client
5. By default, treat Telegram download failures as request-scoped errors; only mark the bucket unavailable when the error semantics clearly prove that the current bot can no longer access the bucket's chat. Old objects that are unreadable under the current bot only fail the current request and are logged

## Error Handling

### Configuration-time errors

- If any configured bucket resolves to an empty final token at startup, fail the service immediately
- Error messages must explicitly identify which bucket could not resolve a token
- Logs must not print any raw token values or partial token text

### Runtime errors

- Ordinary Telegram call failures, such as network timeouts, non-`429` server errors, or object-level read failures, return an error for the current request and emit structured logs
- Only errors that can be clearly attributed to bucket-level token invalidity may trip bucket-wide unavailability. The implementation must normalize Telegram responses first and then judge by operation semantics; the minimum normalized decision payload is operation class, HTTP status code when available, and a normalized reason value
- Examples that may trip unavailability: an upload/send-path `401 Unauthorized`, or an authorization error whose normalized reason clearly means the current bot cannot access the bucket's chat
- Examples that must not trip unavailability: an old `file_id` becoming unreadable under the current bot, file-not-found, transient network failures, ordinary download failures, or a `403` whose normalized reason does not provide enough evidence that the bucket token is the root cause
- Once unavailability is triggered:
  - return the current request error
  - add the bucket to the unavailable set
  - emit structured logs
- All later object access for that bucket must return a unified error immediately without attempting Telegram calls
- Runtime unavailable state only lives for the current process lifetime; restart re-evaluates from config

## Concurrency and Pacing

- Upload gating changes from one global gate to token-scoped gates
- Within each token gate, requests remain serialized and observe `429` cooldown before the next request is sent
- Different token gates run independently and can proceed in parallel
- Downloads continue using `telegramSem` when configured and are unaffected by upload gates
- Token-scoped pacing means buckets on different bots do not wait on each other, while multiple buckets sharing the same bot still share that bot's pacing and cooldown

## Testing

### Config tests

- `telegram.bot_token` supports `${ENV}` resolution
- `bucket.bot_token` supports `${ENV}` resolution
- Precedence is bucket > global `bot_token` > global `bot_token_env`
- Empty `bucket.bot_token` falls back to the global token instead of failing immediately
- Empty global `telegram.bot_token` continues to fall back to `telegram.bot_token_env`
- Startup fails when any configured bucket resolves to an empty final token
- An empty bucket list does not fail because of bucket token logic
- Config-level logging does not leak raw tokens

### Runtime binding tests

- Different buckets receive different token-backed clients when configured that way
- Buckets without bucket-level token override fall back to the global token
- Multiple buckets resolving to the same final token produce the same token key and reuse the same prebuilt gate
- The runtime binding view only covers configured buckets and does not introduce dynamic-bucket resolution paths
- The runtime binding view is not persisted into metadata
- Startup logs correctly include bucket name and token source without leaking raw token, token key, or reversible identifiers

### Store tests

- Upload gates are isolated by token, so buckets with different tokens can upload in parallel
- Multiple buckets sharing the same token also share the same `429` cooldown
- `ObjectStore` prebuilds gates by unique token key at startup and does not rely on lazy first-request gate creation
- Multiple buckets sharing the same token reuse the same gate and do not create duplicate gates or split cooldown state under concurrent first requests
- A bucket is marked unavailable after a clearly bucket-level Telegram authentication failure
- Error classification tests assert the normalized decision payload shape: operation class, HTTP status code when present, and normalized reason
- After that, `HeadObject`, `GetObject`, `PutObject`, and `ListObjects` for that bucket all fail fast
- Old objects unreadable under the current bot only fail the current request and do not mark the bucket unavailable
- A `403` without enough evidence of a bucket-level token problem does not mark the bucket unavailable
- One unavailable bucket under a shared token does not cascade unavailability to other buckets using the same token
- Concurrent hits that trip unavailability on the same bucket only cause one state transition
- Logs include bucket name, operation type, token source, and failure reason without printing raw token, token key, or reversible identifiers
- Non-token Telegram failures, such as ordinary network timeouts, do not mark buckets unavailable

### Regression target

- The single global token scenario preserves current behavior with no token-bucket regression
- When all buckets use the global token, upload pacing and download behavior remain the same as before the change
- An empty bucket list is still handled by the current startup rules and must not fail because of the new bucket token fields

## Non-Goals

- Do not hot-reload bucket tokens at runtime; config changes still require restart
- Do not persist bucket-level tokens into the SQLite bucket table
- Do not add a dynamic bucket management API in CLI or HTTP
- Do not add Telegram token probing or pre-validation beyond empty-value checks
- Do not design cross-bucket object migration; old objects becoming unreadable after a token switch remains a known limitation
- Do not support reading old objects after switching a bucket from token A to token B

## Implementation Notes

Changes are concentrated in existing files:
- `config/config.go`: config parsing and token precedence
- `config/config_test.go`: config resolution tests
- `cmd/tgnas/main.go`: runtime binding construction and startup failure handling
- `store/store.go`: bucket-based client selection, token-scoped upload gates, and runtime unavailable bucket tracking
- `store/store_test.go`: token-bucket pacing and unavailable bucket tests
- `metadata`: no new fields; only reuse existing bucket information
