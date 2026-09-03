# Database backup and recovery

This document explains the backup design, guarantees and limits. For setup and
restore commands, see [Backups](self-hosting.md#backups) in the self-hosting
guide.

## Implemented scope

OpenConvo creates logical PostgreSQL backups with `pg_dump` and stores them in
an administrator-selected S3-compatible bucket. The authenticated Backups page
owns the ordinary workflow:

- select Amazon S3, Cloudflare R2, Backblaze B2, or a custom S3 endpoint;
- set bucket, object prefix, cadence, and retention;
- enable or disable the schedule and request an immediate backup;
- see pending, running, successful, and failed runs;
- download successful dumps through the authenticated OpenConvo API.

This deliberately solves the common single-VPS failure first. It does not claim
PITR, zero RPO, automatic restore, or whole-archive protection.

```text
Backups page ──► backup settings (non-secret) ──► PostgreSQL settings table
    │
    ├── Back up now ─┐
    │                ▼
schedule ───────► PostgreSQL job queue ──► pg_dump ──► S3 / R2 / B2 / MinIO
                                                    │
recent backups ◄──── database_backups metadata ◄───┘
    │
    └── authenticated download ────────────────────► remote object
```

## Configuration and secrets

Destination policy is small application state and may be saved from the
dashboard. Environment variables provide the initial defaults until dashboard
settings exist.

Access keys and session tokens are different: they are secrets and remain
environment-only. They are never returned to the browser or stored in the
`settings` or `database_backups` tables. The API exposes only
`credentials_configured: true|false`.

R2 uses its S3 API endpoint (`https://<account-id>.r2.cloudflarestorage.com`)
and signing region `auto`. AWS uses the SDK's normal endpoint for the selected
region. Backblaze and custom providers accept their HTTPS endpoint and signing
region. Path-style addressing is exposed only for custom providers such as
MinIO.

Saving an enabled configuration performs `HeadBucket` with a short deadline.
A bad endpoint, region, bucket, or credential therefore fails before the
schedule is enabled.

## Backup lifecycle

One database backup may be pending or running at a time. This is enforced by a
partial unique database index, not merely process memory, so simultaneous
scheduler and dashboard requests cannot start two dumps.

Each run snapshots its non-secret destination settings, then:

1. runs `pg_dump --format=custom` into a private temporary file, excluding the
   disposable `derived.message_embeddings` table data while keeping its schema;
2. records the file size and computes SHA-256;
3. uploads under a timestamped, random-suffixed object key with an explicit
   content length;
4. checks the visible remote object size;
5. marks the database row successful;
6. deletes successful objects beyond retention for that exact destination.

The temporary file is removed after every attempt. Failed work retries through
the existing PostgreSQL job queue with bounded backoff. A crash after upload but
before completion is safe: retrying overwrites the same object key before
publishing success.

Changing destination settings does not rewrite old history. Each run retains
its endpoint, region, bucket, prefix, retention count, and object key so it can
still be located.
Because credentials are not retained, downloading or expiring objects from an
old destination requires the current environment credentials to still grant
access there.

## Download security

Database dumps contain the private archive, raw source payloads, deletion
ledger, and operational metadata. Downloads therefore use the same admin
authentication as message and attachment reads. OpenConvo proxies the remote
object with `Cache-Control: private, no-store`, `Content-Disposition:
attachment`, and `X-Content-Type-Options: nosniff`; it does not expose bucket
credentials or permanent public URLs.

Embedding vectors are the exception: scheduled dumps omit their table data.
Canonical message content and embedding-generation provenance remain in the
dump, so an enabled embedding worker can rebuild the index after restore.

## Scope limits

Docker volumes remain persistence, not backups. A successful database dump
protects canonical PostgreSQL rows, including attachment metadata, but it does
not copy filesystem attachment bytes. An installation using local attachment
storage uses portable exports when it needs one self-contained preservation
artifact with both records and attachment bytes.

Logical dumps are immutable point-in-time recovery material, not continuous
archives. Changes after the newest successful dump are outside the observed RPO,
including later edits and deletions. OpenConvo does not rewrite retained dumps
when the live archive changes. Consequently, an older dump can contain content
deleted after it was created; independently tracking those later deletions is
deferred for now.

Physical backups, WAL/PITR, automated restore drills, and whole-instance
protection status are outside the current preservation scope. They can be
reconsidered if operators demonstrate a need for instance-level disaster
recovery.

## References

- [PostgreSQL backup and restore](https://www.postgresql.org/docs/current/backup.html)
- [Cloudflare R2 S3 API](https://developers.cloudflare.com/r2/get-started/s3/)
