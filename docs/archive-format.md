# OpenConvo archive format (v1)

> **Status: stable.** This is a public interface. Every future OpenConvo
> verifier must continue to accept version 1, and other software can implement
> it from this document alone, without running OpenConvo.

A portable export is the implementation-independent preservation artifact. It
is not currently importable into a live OpenConvo database. Use a PostgreSQL
dump for operational recovery; see
[restoring a database backup](self-hosting.md#restoring-a-database-backup).
Importing portable exports remains a deferred roadmap item.

## Goals

- Survive without OpenConvo: plain JSONL + plain files, processable with
  `jq`, `grep`, or any programming language.
- Streamable and compressible; works for archives with millions of messages.
- Independently verifiable via SHA-256 checksums.
- Versioned; future changes are additive. Breaking changes require a new
  major format version, and old versions stay readable.

## Layout

```text
openconvo-export/
├── manifest.json          # what this export is
├── communities.jsonl      # one JSON object per line
├── channels.jsonl
├── actors.jsonl
├── messages.jsonl
├── attachments.jsonl
├── bookmarks.jsonl
├── deletion_ledger.jsonl
├── blobs/
│   └── sha256/
│       └── <aa>/<full-sha256-hex>     # attachment files, content-addressed
├── markdown/                # optional human-readable rendering
│   ├── README.md
│   └── channels/
│       └── <channel-uuid>.md
└── checksums.sha256       # `sha256sum -c` compatible, covers every file
```

## manifest.json

```json
{
  "format": "openconvo-archive",
  "format_version": 1,
  "generated_at": "2026-08-18T12:00:00Z",
  "openconvo_version": "0.1.0",
  "sources": ["discord"],
  "renderings": ["markdown"],
  "communities": [
    { "id": "…", "source": "discord", "external_id": "…", "name": "…" }
  ],
  "counts": {
    "communities": 1,
    "channels": 37,
    "actors": 412,
    "messages": 482191,
    "attachments": 18424,
    "bookmarks": 86,
    "deletion_ledger": 24,
    "blobs": 17902,
    "markdown_channels": 34
  }
}
```

`renderings` is optional. It lists derived views included alongside the
canonical JSONL records; omitting it means the export contains only the
canonical representation.

## JSONL records

Every line is one JSON object. Shared conventions:

- `id`: UUID, stable across exports of the same installation
- `source` + `external_id`: identity on the source platform
- timestamps: RFC 3339, always UTC, whatever time zone the exporting server
  runs in. OpenConvo writes the `+00:00` spelling in JSONL records and the
  equivalent `Z` spelling in `manifest.json`; readers must accept both
- `raw_payload`: the source platform's original object, verbatim, when the
  installation retained it (scrubbed for deleted content)
- unknown fields must be ignored by readers (forward compatibility)

Manifest counts are the corresponding JSONL line counts. `blobs` is the
number of unique hashes referenced by stored attachments and copied into the
export; untracked files and unreferenced database blob rows are maintenance
debris, not archive records. `markdown_channels` is the number of files under
`markdown/channels/`; it is present only when the export carries the Markdown
rendering, and exists so that a rendering which lost files cannot pass
verification.

### messages.jsonl

```json
{
  "id": "0198c0de-…",
  "channel_id": "0198c0aa-…",
  "actor_id": "0198c0bb-…",
  "external_id": "1140384234567890123",
  "kind": "reply",
  "content": "What glue are you using?",
  "reply_to_message_id": "0198c0cc-…",
  "reply_to_external_id": "1140380000000000042",
  "source_created_at": "2026-03-14T09:26:53.589+00:00",
  "source_updated_at": null,
  "deleted_at": null,
  "reactions": [
    { "emoji_key": "👍", "emoji_name": "👍", "count": 3 }
  ],
  "raw_payload": { "…": "original Discord message object" }
}
```

`reactions` is the aggregated reaction list, embedded in the message it
belongs to. Each element requires `emoji_key`, the source platform's stable
identifier for the emoji (the literal character for Unicode emoji,
`custom:<id>:<name>` for custom ones), plus `emoji_name` and `count`.
OpenConvo's own exports additionally carry the reaction row's `id`,
`message_id`, `raw_payload` and timestamps; producers may omit them and
readers must ignore them like any other unknown field. A reaction that does
carry `message_id` must repeat the enclosing message's `id`.

Deleted messages appear as tombstones: `content` is `null`, `deleted_at` is
set, `raw_payload` is `{}` and no attachments reference them.

### attachments.jsonl

```json
{
  "id": "0198c0ff-…",
  "message_id": "0198c0de-…",
  "external_id": "1140384235000000001",
  "filename": "delamination.jpg",
  "description": "Photo of the delaminated deck",
  "content_type": "image/jpeg",
  "size": 431872,
  "sha256": "4ad390ab…",
  "download_status": "stored",
  "source_url": "https://cdn.discordapp.com/…"
}
```

`sha256` locates the file at `blobs/sha256/<first two hex chars>/<sha256>`.
Multiple attachments may reference one blob (deduplication). An attachment
whose download never succeeded has `download_status: "failed"` and no
`sha256`; the metadata is still preserved.

### communities / channels / actors / bookmarks / deletion_ledger

Follow the corresponding archive tables 1:1 (see
`migrations/0001_initial.sql`): channels carry `parent_channel_id` for
threads and `kind` (`text`, `forum`, `thread`, …); actors carry `username`,
`display_name`, `avatar_url`, `is_bot` and never emails or IPs;
`deletion_ledger.jsonl` lets an importer honor deletions even when importing
into an installation restored from an older backup.

## checksums.sha256

```text
<sha256>  communities.jsonl
<sha256>  messages.jsonl
<sha256>  blobs/sha256/4a/4ad390ab…
…
```

`cd openconvo-export && sha256sum -c checksums.sha256` must pass; blob
digests double as their own content checksums. The checksum file covers every
other regular file in the export (it cannot recursively checksum itself).

`openconvo verify openconvo-export` additionally checks manifest counts,
JSONL validity, cross-record references, that every stored attachment has its
content-addressed blob, and that each blob's checksum line matches the digest
in its own path, so regenerating `checksums.sha256` cannot launder content
that no longer matches its address. Verification is offline and needs neither
a database nor storage credentials.

## Markdown rendering

`openconvo export --format markdown` creates the complete canonical export
above and adds a derived Markdown view under `markdown/`. The canonical JSONL
files remain authoritative and make the export lossless; Markdown is for
reading with ordinary text editors and renderers.

`markdown/README.md` indexes communities, channels, and threads. Each channel
or thread uses its stable archive UUID as the filename, so duplicate or renamed
channels cannot collide or break links. Channel files contain live messages in
chronological order with stable message anchors, literal message text, reply
links, reactions, bookmark annotations, and attachment links. Tombstoned
messages are absent. Unstored attachments retain their filename, size, and
status but have no link to expiring source URLs.

Stored attachment links point to the same content-addressed objects under
`blobs/sha256/` as the canonical attachment records. Markdown files are listed
in `checksums.sha256`, and `openconvo verify` verifies them with the rest of
the archive: `markdown/README.md` must be present, and the number of channel
files must match `counts.markdown_channels`.

## Versioning policy

- `format_version` increments only for breaking changes; OpenConvo's verifier
  must accept every format version ≤ its own.
- Additive changes (new fields, new files) do not bump the version.
- This document is the source of truth, maintained independently of the Go
  implementation.
