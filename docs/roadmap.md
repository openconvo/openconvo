# Roadmap

OpenConvo's product path is **Capture → Preserve → Find → Curate**. It is
built in vertical slices so each milestone leaves the system genuinely more
useful for a self-hosting administrator. The bar for 1.0 is the
[acceptance scenario](#10-acceptance-scenario) at the bottom.

**Status:** all eight milestones are complete. The acceptance scenario is now
being exercised end to end against a live Discord community; what remains
before 1.0 is completing that testing and fixing what it exposes.

The 1.0 implementation path:

```text
Docker Compose install → connect Discord bot → select channels
→ historical backfill → live sync → attachments → browse → search → curate
→ off-site database backup → portable export → offline verification
```

Deliberate non-goals for v1: Kubernetes, Redis, Elasticsearch/Meilisearch,
standalone vector services, AI-generated summaries or answers, public or
anonymous archive publishing, mobile apps, additional sources (Slack,
Discourse, ...), federation, a plugin marketplace, and analytics. These wait
until the preservation-first knowledge foundation is excellent.

One narrow foundation slice is present ahead of those features: an optional,
local pgvector index generated with the fixed OpenAI
`text-embedding-3-small`/256-dimension preset. Its semantic search stays off
until the operator enables it, and the index remains rebuildable derived data.
Cloudflare Vectorize and a generic provider/model framework remain deferred
until there is evidence for a second implementation.

## Milestones

### 1. Foundation (done)

- [x] Go application: `serve`, `migrate`, `status`, `healthcheck`, `version`
- [x] PostgreSQL schema for the full canonical model + migration runner
- [x] Archive store: idempotent upserts, defensive merges, tombstoning,
      deletion ledger, tested against real PostgreSQL
- [x] PostgreSQL-backed job queue (claim, retry/backoff, dedupe)
- [x] Content-addressed blob storage (filesystem driver)
- [x] Source abstraction + Discord REST client with centralized rate-limit
      handling + payload normalization with fixtures
- [x] HTTP server, `/health`, versioned status API, embedded React shell
- [x] Dockerfile (multi-stage, minimal runtime image), Docker Compose, CI

### 2. Discord archive (done)

- [x] Gateway connection (identify, resume, reconnect) with the
      `MESSAGE_CONTENT` intent documented for users
- [x] Guild + channel discovery; channel selection UI/API
      (nothing archived until explicitly selected)
- [x] Live events: `MESSAGE_CREATE/UPDATE/DELETE/DELETE_BULK`, reactions,
      channel/thread lifecycle
- [x] Historical backfill: resumable newest→oldest pagination with
      checkpoints in `sync_states`, running alongside live sync
- [x] Threads, replies, actors, raw payload capture
- [x] Reconciliation after gaps, resume failures and on a schedule

Known limitations, deliberately deferred: private archived threads are not
enumerated (public and active threads are), `MESSAGE_REACTION_REMOVE_EMOJI`
is corrected by reconciliation rather than handled live, and reconciliation
detects deletions only inside the window it re-reads.

### 3. Attachments (done)

- [x] Download pipeline as jobs: stream → SHA-256 → dedupe → link
- [x] Expired Discord CDN signatures refreshed at download time
- [x] Retry/backoff for failed downloads; per-file size limit
- [x] Orphaned blob reclamation, so deletions free the bytes too
- [x] S3-compatible storage driver (AWS S3, R2, Spaces, MinIO)

Off by default (`OPENCONVO_ATTACHMENTS_ENABLED`): storing a community's
files is an open-ended disk commitment, so it is the operator's decision.
Serving archived files moved to milestone 4, which owns the admin
authentication that reading message content requires.

### 4. Archive UI (done)

- [x] Dashboard with real sync progress per channel
- [x] Channel and thread navigation, message timeline
- [x] Message-in-context view (surrounding conversation, permanent URLs)
- [x] Admin authentication (archives are private by default). This MUST land
      here, before any API that reads message content exists: the milestone-2
      selection endpoints shipped unauthenticated by design, and they expose
      channel metadata only.
- [x] Serving archived attachments from OpenConvo storage, gated by the
      admin authentication above, the same as any other content-reading API
- [x] Read-only release detection with compatibility-aware, copyable host
      upgrade commands; the application never controls the Docker daemon
- [x] Messages with no text of their own still read as something: system
      events (a member joining, a pin) get a plain synthesized line styled
      apart from archived words, and stickers show their name

Known limitation, deliberately deferred: sticker artwork is not downloaded.
The timeline shows the sticker's name, which the source payload preserves;
storing the image would mean a second media pipeline, and Discord's own
sticker packs are Lottie animations that no archive reader can display
without a new frontend dependency.

### 5. Search (done)

- [x] PostgreSQL FTS with filters: text, channel, author, date range,
      has-attachment; highlighted excerpts
- [x] Optional semantic search over disposable pgvector message embeddings,
      with the same filters and an explicit per-query OpenAI privacy notice
- [x] Optional MCP adapter exposing that search as one read-only tool over
      local stdio or authenticated remote Streamable HTTP
- [x] Every result opens in conversational context

### 6. Preservation (done)

- [x] JSONL export per [archive-format](archive-format.md) with manifest
      and checksums
- [x] `openconvo verify` (counts, hashes, references)
- [x] Reconcile stored files against blob rows: reclaim files with no row
      and stale temp files. This closes a known gap: blob reclamation
      enumerates rows, so a download interrupted or failed between
      committing its file and recording it leaves bytes reclamation cannot
      see, including for a message that was later deleted
- [x] Deletion-ledger replay after restores
- [x] Backup documentation and helper script
- [x] Scheduled PostgreSQL custom-format dumps to S3/R2/B2/custom storage,
      configured and downloadable from the authenticated Backups page
- [x] Environment-only backup credentials, bounded retention, run history,
      manual runs, upload size verification, and SHA-256 recording
- [x] Portable exports include every referenced stored attachment from either
      filesystem or S3 storage and verify independently of OpenConvo

The implemented backup scope and its limits are in the
[backup and recovery architecture](backup-architecture.md). Scheduled dumps are convenient
database recovery points; portable exports are the implementation-independent
preservation artifact and include attachment bytes.

Operator-initiated whole-user erasure is deferred until OpenConvo has an
explicit policy for messages that arrive or are posted after the erasure. V1
honors source message deletions durably but does not expose a `delete-user`
command whose result later ingestion could silently undo.

### 7. Curation (done)

- [x] Bookmarks: titles, descriptions, tags, collections
- [x] Discord **Archive** message action

### 8. Extended preservation (done)

- [x] Markdown export

Deferred until there is a demonstrated operational need: static HTML export,
importing an export back into OpenConvo, pgBackRest/WAL/PITR, whole-instance
protection status, and automated restore drills.

## 1.0 acceptance scenario

A release is stable only when this works reliably end to end:

1. A new administrator starts OpenConvo with Docker Compose, configures a
   Discord bot, and OpenConvo connects.
2. They select three channels, one with years of history. Backfill runs
   without blocking live ingestion, and survives a restart mid-import.
3. Messages, replies, threads and attachments appear; new messages appear
   automatically; edits update the archive; deletions remove archived content.
4. Search finds historical messages and opens them in context.
5. Attachments serve from OpenConvo storage, independent of Discord.
6. A complete JSONL export verifies with `openconvo verify`.
7. With the OpenConvo instance and its volumes unavailable, an off-site
   portable export still verifies offline, reports when it was generated, and
   exposes every stored attachment by its verified digest using documented,
   ordinary formats.
