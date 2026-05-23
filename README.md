# tgs3

`tgs3` is an S3-compatible gateway backed by Telegram storage and local SQLite metadata.

## Service mode

When no subcommand is provided, `tgs3` starts the HTTP service. By default it reads `data/config.yaml` from the current working directory:

```bash
tgs3
```

## CLI usage

```text
tgs3 [-debug] [-c|-config config.yaml]
tgs3 [-debug] [-c|-config config.yaml] ls [-n|-limit N] bucket[/prefix]
tgs3 [-debug] [-c|-config config.yaml] lsd [bucket[/prefix]]
```

`-c` is a short alias for `-config`. Passing both `-config` and `-c` in the same invocation is a usage error. `-debug` is a global flag, so it must appear before any subcommand. Debug logs are written to stderr and normal command output stays on stdout.

## Local metadata CLI

The `tgs3` binary also provides read-only local listing commands that inspect the configured SQLite metadata database. These commands do not start the HTTP server and do not contact Telegram.

`ls` prints object keys, one per line. It defaults to 1000 results; `-limit N` and `-n N` set the maximum result count, and `0` means no overall result limit while still reading in pages internally.

`lsd` without a path prints enabled bucket names. `lsd bucket/prefix` prints direct pseudo-directories under the prefix using `/` as the delimiter.

Bucket `chat_id` values can be literal Telegram chat IDs or full environment-variable references such as `chat_id: "${TGS3_PRIVATE_CHAT_ID}"`. Partial interpolation is not supported. If the referenced environment variable is unset or empty, the resolved `chat_id` is empty and config validation fails.
