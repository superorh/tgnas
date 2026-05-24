# tgnas

`tgnas` is an S3-compatible and WebDAV-capable gateway backed by Telegram storage and local SQLite metadata.

## Service modes

By default, `tgnas` reads `data/config.yaml` and starts one HTTP server with both protocols enabled:

```bash
tgnas
```

The S3 API is served at normal bucket paths. WebDAV is served under the configured WebDAV prefix, defaulting to `/dav/`.

Single-protocol modes are available when you only want one API surface:

```bash
tgnas -c config.yaml s3
tgnas -c config.yaml dav
```

`-c` is a short alias for `-config`. Passing both `-config` and `-c` in the same invocation is a usage error. `-debug` is a global flag, so it must appear before any subcommand.

## Configuration

The default config path is `data/config.yaml`.

Common environment variables use the `TGNAS_*` prefix:

- `TGNAS_LISTEN` overrides `server.listen` when `server.listen_env` is configured.
- `TGNAS_SECRET_KEY` is the example S3/WebDAV credential secret.
- `TGNAS_TELEGRAM_BOT_TOKEN` provides the Telegram bot token.
- `TGNAS_SQLITE_PATH` can override the metadata SQLite path.
- `TGNAS_PRIVATE_CHAT_ID` is an example bucket chat ID reference.

WebDAV configuration:

```yaml
webdav:
  # prefix: "/dav/"
```

The prefix must start with `/`, is normalized to end with `/`, cannot be `/`, and cannot conflict with the first path segment of any configured bucket.

## Authentication

S3 keeps SigV4 authentication.

WebDAV uses HTTP Basic Auth and reuses `auth.credentials`:

- username: `access_key`
- password: resolved value of `secret_key_env`

## Local metadata CLI

`tgnas` also provides read-only local listing commands that inspect the configured SQLite metadata database. These commands do not start the HTTP server and do not contact Telegram.

```text
tgnas [-debug] [-c|-config config.yaml] ls [-n|-limit N] bucket[/prefix]
tgnas [-debug] [-c|-config config.yaml] lsd [bucket[/prefix]]
```

`ls` prints object keys, one per line. It defaults to 1000 results; `-limit N` and `-n N` set the maximum result count, and `0` means no overall result limit while still reading in pages internally.

`lsd` without a path prints enabled bucket names. `lsd bucket/prefix` prints direct pseudo-directories under the prefix using `/` as the delimiter.

## WebDAV behavior

WebDAV exposes object prefixes as directories. `MKCOL /dav/photos/2026/` creates a zero-byte directory marker object with key `2026/`, preserving empty directories.

Supported common operations include `OPTIONS`, `PROPFIND`, `GET`, `HEAD`, `PUT`, `DELETE`, `MKCOL`, `COPY`, and `MOVE`. `LOCK` and `UNLOCK` return not implemented, and `OPTIONS` does not advertise lock support.

`COPY` and `MOVE` are metadata-only within the same bucket, including recursive directory copy/move. They reuse existing Telegram file/chunk metadata rather than downloading and re-uploading content.

Buckets are still created from config only. If a bucket remains in metadata after being removed from config, it is treated as an orphan: normal object access is forbidden, but `DELETE /dav/{bucket}` or S3 `DELETE /{bucket}` can clean up the local metadata record and associated object/chunk metadata.

Bucket `chat_id` values can be literal Telegram chat IDs or full environment-variable references such as `chat_id: "${TGNAS_PRIVATE_CHAT_ID}"`. Partial interpolation is not supported. If the referenced environment variable is unset or empty, the resolved `chat_id` is empty and config validation fails.
