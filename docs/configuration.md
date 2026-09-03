# Configuration reference

Process configuration and secrets are supplied through environment variables.
The Compose deployment reads them from the gitignored `.env` file, which the
guided installer writes for you; [.env.example](../.env.example) is the
annotated template for manual installs. This page lists every supported
variable, while the [self-hosting guide](self-hosting.md) explains the features
they belong to.

Some non-secret feature settings are **initial values**. They seed the stored
setting until an administrator saves that feature's dashboard form; the stored
value is authoritative afterwards. Credentials always remain environment-only.
Rows below identify initial values explicitly.

## Required

Both fail closed: Compose refuses to start while either is empty, so a
copied-but-unedited `.env` fails before the containers exist.

| Variable | Default | Purpose |
| --- | --- | --- |
| `POSTGRES_PASSWORD` | (none; required by Compose) | Password for the bundled PostgreSQL container; Compose fails to start while it is empty |
| `OPENCONVO_ADMIN_PASSWORD` | (none; required for `serve`) | Built-in administrator login password; at least 12 characters |

## Discord

| Variable | Default | Purpose |
| --- | --- | --- |
| `DISCORD_TOKEN` | (none) | Discord bot token; archival is idle without it |
| `DISCORD_APPLICATION_ID` | (none) | Discord application ID |

## Server

See [public-internet deployment](self-hosting.md#putting-openconvo-on-the-public-internet)
for running behind a reverse proxy.

| Variable | Default | Purpose |
| --- | --- | --- |
| `OPENCONVO_HOST` | all interfaces | HTTP bind interface for a bare process; leave empty under Compose |
| `OPENCONVO_PORT` | `8080` | Host port published (Compose) / listen port (bare process) |
| `OPENCONVO_PUBLISH_ADDRESS` | `127.0.0.1` | Interface that port is published on (Compose only); set `0.0.0.0` only with an explicit network/TLS boundary |

## Database

| Variable | Default | Purpose |
| --- | --- | --- |
| `DATABASE_URL` | (none; required) | PostgreSQL connection string (set automatically by Compose) |
| `OPENCONVO_AUTO_MIGRATE` | `true` | Apply schema migrations on startup |

## Attachment storage

See [archiving attachments](self-hosting.md#archiving-attachments) and
[S3-compatible object storage](self-hosting.md#s3-compatible-object-storage).

| Variable | Default | Purpose |
| --- | --- | --- |
| `OPENCONVO_ATTACHMENTS_ENABLED` | `false` | Download attachment files into the configured storage backend |
| `OPENCONVO_ATTACHMENT_MAX_BYTES` | `104857600` | Per-file download size cap, in bytes |
| `STORAGE_DRIVER` | `filesystem` | Attachment storage driver |
| `STORAGE_PATH` | `/data/attachments` | Filesystem storage root |
| `S3_ENDPOINT` | (none) | S3-compatible API endpoint; empty for AWS S3 |
| `S3_REGION` | (none) | S3 signing region (`auto` for Cloudflare R2) |
| `S3_BUCKET` | (none) | Existing S3-compatible bucket |
| `S3_ACCESS_KEY` | (none) | S3 access key ID |
| `S3_SECRET_KEY` | (none) | S3 secret access key |
| `S3_SESSION_TOKEN` | (none) | Optional temporary-credential token |
| `S3_FORCE_PATH_STYLE` | `false` | Use path-style bucket addressing for MinIO-like services |

## Database backups

See [scheduled database backups](self-hosting.md#scheduled-database-backups).

| Variable | Default | Purpose |
| --- | --- | --- |
| `BACKUP_ENABLED` | `false` | Initial scheduled-backup state |
| `BACKUP_PROVIDER` | `r2` | Initial provider: `s3`, `r2`, `backblaze`, or `custom` |
| `BACKUP_S3_ENDPOINT` | (none) | Initial S3-compatible API endpoint; empty for AWS |
| `BACKUP_S3_REGION` | `auto` | Initial signing region (`auto` for R2) |
| `BACKUP_S3_BUCKET` | (none) | Initial existing backup bucket |
| `BACKUP_S3_PREFIX` | `openconvo/database-backups` | Initial object-key prefix for dumps |
| `BACKUP_S3_ACCESS_KEY` | (none) | Backup bucket access key (environment-only) |
| `BACKUP_S3_SECRET_KEY` | (none) | Backup bucket secret key (environment-only) |
| `BACKUP_S3_SESSION_TOKEN` | (none) | Optional temporary credential (environment-only) |
| `BACKUP_S3_FORCE_PATH_STYLE` | `false` | Initial path-style setting for MinIO-like storage |
| `BACKUP_INTERVAL_HOURS` | `24` | Initial backup cadence |
| `BACKUP_RETENTION_COUNT` | `30` | Initial successful dumps retained per destination |
| `BACKUP_PG_DUMP_PATH` | `pg_dump` | `pg_dump` executable for bare-process installs |

## Optional message embeddings

See [optional message embeddings](self-hosting.md#optional-message-embeddings).

| Variable | Default | Purpose |
| --- | --- | --- |
| `OPENCONVO_EMBEDDINGS_ENABLED` | `false` | Initial opt-in for sending archived message text to OpenAI for embedding |
| `OPENAI_API_KEY` | (none) | OpenAI credential for optional embeddings; environment-only |

## Remote MCP search

See [MCP search](mcp.md).

| Variable | Default | Purpose |
| --- | --- | --- |
| `OPENCONVO_MCP_HTTP_ENABLED` | `false` | Expose the read-only MCP search endpoint at `/mcp` |
| `OPENCONVO_MCP_TOKEN` | (none; required when remote MCP is enabled) | Dedicated bearer token for `/mcp`; at least 32 characters |

## Logging

| Variable | Default | Purpose |
| --- | --- | --- |
| `LOG_LEVEL` | `info` | `debug` `info` `warn` `error` |
| `LOG_FORMAT` | `text` | `text` or `json` |
