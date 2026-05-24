# TgNAS

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

Common environment variables:

- `TGNAS_LISTEN` overrides `server.listen` when `server.listen_env` is configured. Default is `:9000`.
- `TGNAS_SECRET_KEY` is the example S3/WebDAV credential secret.
- `TGNAS_TELEGRAM_BOT_TOKEN` provides the Telegram bot token in the default Docker-oriented config.
- `TGNAS_TELEGRAM_CHAT_ID` is the default bucket chat ID reference.
- `TGNAS_SQLITE_PATH` can override the metadata SQLite path.

WebDAV configuration:

```yaml
webdav:
  # prefix: "/dav/"
```

The prefix must start with `/`, is normalized to end with `/`, cannot be `/`, and cannot conflict with the first path segment of any configured bucket.

## Docker

Run it with the default Docker-oriented config in `data/config.yaml`:

```bash
mkdir -p data

docker run --rm -u root -v "$PWD/data:/app/data" ghcr.io/aahl/tgoss chown -R app:app /app/data

docker run -d \
  --name tgnas \
  -p 9000:9000 \
  -v "$PWD/data:/app/data" \
  -e TGNAS_SECRET_KEY="your-s3-and-webdav-password" \
  -e TGNAS_TELEGRAM_CHAT_ID="-1001234567890" \
  -e TGNAS_TELEGRAM_BOT_TOKEN="123456:telegram-bot-token" \
  ghcr.io/aahl/tgoss
```

The container runs as a non-root `app` user and uses `/app` as its working directory. The mounted `data` directory must be writable by that container user because SQLite metadata is stored under `/app/data` by default. If SQLite fails to open or create `metadata.sqlite`, fix the host directory ownership or permissions before restarting the container.

## Docker Compose

The included `docker-compose.yml` uses the published GHCR image and mounts `./data` to `/app/data`:

```bash
cat << EOF > .env
TGNAS_PORT_EXPOSED=9000
TGNAS_SECRET_KEY="your-s3-and-webdav-password"
TGNAS_TELEGRAM_CHAT_ID="-1001234567890"
TGNAS_TELEGRAM_BOT_TOKEN="123456:telegram-bot-token"
EOF

docker compose run --rm -u root tgnas chown -R app:app /app/data
docker compose up -d
```

If the host `data` directory is owned by root or another user, grant write access to the UID used by the container's `app` user, or use a permissions policy such as a writable group on `./data`. Do not make the config or SQLite directory read-only.

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
