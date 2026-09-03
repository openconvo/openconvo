# OpenConvo architecture

OpenConvo keeps a community's own copy of its knowledge: a faithful, private,
self-hosted archive of the chat it lives in. This document describes the
preservation-first architecture underneath that promise. It reflects the code
as it exists; roadmap items are marked as such.

## The foundation rule

> If every derived system disappeared, the canonical data must still be enough
> to faithfully reconstruct the conversations.

```text
                ┌──► archive browser
                │
                ├──► full-text search
Discord ──► CANONICAL ARCHIVE ──► JSONL / Markdown exports
                │
                ├──► MCP search
                │
                ├──► embeddings           (optional derived schema)
                │
                └──► future knowledge tools
```

Canonical data: **messages, attachments, metadata, relationships, curation
records.** Derived data: **search indexes, renderings and AI output**, all
rebuildable, all disposable.

## One process

OpenConvo is a single Go binary that runs the HTTP API, serves the compiled
React frontend, ingests from sources, and executes background jobs. There are
no microservices and no external queue: PostgreSQL holds the archive, the
full-text search index, and the job queue.

Deployment is two containers (`openconvo`, `postgres`). Only OpenConvo's
HTTP port is exposed.

The official application image also contains PostgreSQL's `pg_dump` client.
This is the deliberate exception to the otherwise-static runtime image: custom
dump format is PostgreSQL-owned and reimplementing it in Go would be a far
larger durability risk. It is an invoked subprocess, not another service;
bare installations need it only when database backups are used.

The standard-library-first dependency rule has four exceptions, all direct
requirements in `go.mod`, all compiled into the binary, none of them a runtime
service:

- `jackc/pgx/v5`: the PostgreSQL driver. Explicit SQL everywhere means no ORM,
  but the wire protocol, connection pooling, and type mapping are not worth
  reimplementing.
- `aws/aws-sdk-go-v2` (plus its `credentials` and `s3` modules): S3
  authentication, endpoint resolution, retries, and per-provider compatibility
  behavior are security- and durability-sensitive. Maintaining a local
  Signature V4 implementation would be a larger long-term operational risk.
- `coder/websocket`: the Discord Gateway client (`internal/discord/gateway.go`)
  needs RFC 6455 framing, masking, ping/pong, and close-code handling. The
  standard library ships no WebSocket client, and a hand-written one under a
  connection that must resume correctly for years is not a saving.
- `modelcontextprotocol/go-sdk`: the official MCP protocol implementation.
  MCP version negotiation, JSON-RPC framing, schema validation, and backwards
  compatibility are an evolving interoperability boundary; maintaining a
  partial wire implementation locally would be more fragile than this
  compiled-only dependency. OpenConvo uses its stdio and Streamable HTTP
  server transports.

The base deployment's volumes provide persistence, not disaster recovery.
OpenConvo can schedule logical PostgreSQL dumps to S3-compatible off-machine
storage from the dedicated Backups page; see [backup and recovery](backup-architecture.md).
Database dumps do not protect filesystem attachments or provide PITR, so the
product must not describe the whole archive as protected on their strength
alone.

```text
cmd/openconvo        CLI entry point (commands below)
internal/app          wiring and process lifecycle
internal/config       environment configuration
internal/version      build version, injected at link time
internal/database     pgx pool, migration runner (embedded SQL, up-only)
internal/archive      canonical model + PostgreSQL store   ← the heart
internal/storage      content-addressed blob store (filesystem or S3-compatible)
internal/attachments  download pipeline and orphaned-blob reclamation
internal/jobs         PostgreSQL-backed job queue and worker
internal/backups      scheduled pg_dump execution, S3 storage and retention
internal/preservation portable export, verification, deletion-ledger replay
internal/embeddings   optional OpenAI generation + disposable pgvector index
internal/mcpserver    one read-only MCP search tool over stdio or HTTP
internal/updates      cached, read-only GitHub release checking
internal/discord      Discord source: REST client, rate limiting, Gateway, normalization
internal/ingest       the single archive write path shared by live sync and backfill
internal/syncer       backfill, reconciliation and their scheduling (as jobs)
internal/http         HTTP server, middleware, API handlers, SPA serving
internal/web          embedded frontend assets
internal/arch         import-direction rules, enforced as a test
internal/testutil     shared test helpers (real-PostgreSQL fixtures)
migrations/           numbered SQL schema migrations
web/                  React + TypeScript + Vite frontend
```

The binary is also the operational toolkit. Besides `serve`, it offers
`migrate` and `status` for schema and archive state, `export` and `verify` for
portable archives and their independent verification, `replay-deletions` to
apply a newer export's deletion ledger to an older restore, `healthcheck` for
the container probe, `version`, and `mcp` for local read-only archive search.
`openconvo help` prints the authoritative list and arguments.

Update checks are deliberately separated from update execution. The
authenticated dashboard may compare the running semantic version with the
latest stable GitHub release, but the application has no Docker socket, cannot
replace its own container, and never applies an update. The administrator runs
the displayed command on the host.

If scale ever demands separate processes, the same image can grow
`openconvo web|gateway|worker` subcommands. Not before.

## Source-agnostic archive

Nothing in the archive schema is Discord-specific. Identity on the source
platform is always `(source, external_id)`:

```text
source      = "discord"
external_id = "1140384234567890123"
```

Discord data that the normalized model doesn't capture lives in `raw_payload`
(jsonb) so it is never lost, but the application never depends exclusively on
`raw_payload` for primary behavior.

A deliberate non-goal: there is **no generic integration framework**. A
source boundary will be extracted when a second source (Slack, Discourse,
Matrix, ...) actually needs it. Discord must be excellent first.

## Data model

Tables (see `migrations/0001_initial.sql` for authoritative DDL):

| Table | Purpose |
| --- | --- |
| `communities` | A Discord guild today; any community container later |
| `channels` | Text/forum channels and threads (threads have `parent_channel_id`) |
| `actors` | Message authors; deliberately minimal, no emails/IPs |
| `messages` | The core table; tombstoned on deletion, `search_vector` generated |
| `blobs` | Physical files, content-addressed by SHA-256, deduplicated |
| `attachments` | Message ↔ blob relationships; metadata survives failed downloads |
| `message_reactions` | Aggregate counts only, no reactor identities |
| `bookmarks` | Manual curation (titles, descriptions, tags, collections) |
| `sync_states` | Per-channel sync status and resumable checkpoints |
| `deletion_ledger` | Every deletion, exported for audit and optional replay |
| `jobs` | Background job queue |
| `settings` | Small KV store for app state (never secrets) |
| `database_backups` | Logical dump run history and non-secret destination snapshots |
| `derived.embedding_generations` | Provider/model/dimension/input provenance for rebuildable indexes |
| `derived.message_embeddings` | Disposable vectors keyed to canonical messages |

Write semantics, enforced by the store and covered by tests:

1. **Idempotent.** Every upsert can run any number of times: duplicate Gateway
   events, overlapping backfills and reconciliation are normal, not errors.
2. **Defensive.** Partial update events never erase known fields (Discord
   `MESSAGE_UPDATE` payloads can be partial).
3. **Deletion-safe.** Deleting a message tombstones it (content and
   `raw_payload` scrubbed, dependents removed, ledger written). Stale events
   arriving later cannot resurrect it.

## Message lifecycle

```text
create:  normalize → upsert actor → upsert message → record attachment
         metadata → update reactions → (search vector updates itself)
         → best-effort embedding job (when explicitly enabled)

edit:    upsert message (COALESCE semantics; omitted fields keep old values)
         → transactionally discard stale derived embedding

delete:  content := NULL, deleted_at := now(), raw_payload := {}
         delete attachments/reactions/bookmarks rows
         discard derived embedding
         write deletion_ledger entry
         (orphaned blobs removed by cleanup job)
```

Fetching the attachment files themselves is deliberately not part of this
path. A separate sweep schedules those downloads as background jobs, so
ingestion never waits on the network and a slow or failing CDN cannot
stall the record of what was said.

The archive keeps a tombstone (external ID + deletion time), never hidden
copies of deleted content.

## Attachments

Discord CDN URLs are not preservation: they are signed and expire in about
a day. One `attachment.download` job per file does the work.

```text
refresh URL if the ex= expiry has passed → stream while hashing SHA-256
→ dedupe into sha256/<aa>/<digest> (atomic rename or S3 PutObject)
→ link blob to attachment → verify the object is still there
```

Files are never held fully in memory, and one blob may back many attachments.
The filesystem driver streams to a temporary file and atomically renames it.
The S3 driver also stages one size-bounded file locally because the digest is
the final object key, then uploads it and removes the staging file. Downloading
is off unless `OPENCONVO_ATTACHMENTS_ENABLED` says otherwise: storing a
community's files is an open-ended commitment, and attachment metadata is
archived either way.

Refreshing needs only the URL's path, so an archive whose links all
expired years ago is still recoverable. A refreshed URL is not guaranteed
a full fresh lifetime, so refreshing happens immediately before the
download rather than as a separate stage, which is also why refreshes
are not batched.

Failures are classified by whose problem they are. A file that is gone at
source, or larger than the size limit, fails immediately: no number of
retries changes either answer, so the attachment is marked `failed` with
a reason on the first attempt. A download that merely goes wrong (a size
mismatch, a transport error) retries with backoff and is marked `failed`
only once the job's attempts run out. Storage-side failures, a full disk
above all, leave it `pending` however many attempts are burned, so the
work resumes by itself rather than condemning thousands of files over a
condition the operator can fix.

Deleting a message deletes its files too: the `storage.gc` job reclaims
blobs nothing references any more, every hour, with a one-hour grace
period so a download cannot have its blob reclaimed between storing it
and linking it. It deletes the row before the file, because `ON DELETE
RESTRICT` makes that first step a live re-check: a blob that gained a
reference since it was listed keeps both its row and its bytes. A second
check on the digest follows, for the case where a download deduplicates
onto content whose row was just deleted. Interrupted after that, the cost
is a leaked file rather than a lost one. This runs regardless of
`OPENCONVO_ATTACHMENTS_ENABLED`: downloading is the operator's choice,
but honoring a deletion is not.

Hourly reclamation works from blob rows, so an object whose row was never
written (a download interrupted or failed between committing the object and
recording it) is not reachable by it, nor is a staging file left behind by an
unclean stop. That is not only wasted storage: if such an attachment's
message is later deleted, its bytes survive in storage with nothing
referencing them. `openconvo verify` enumerates the configured backend to
find this inverse case. Its default pass is read-only; `openconvo verify --repair`
removes untracked objects and filesystem staging files only after a
one-hour grace period, protecting downloads that committed their bytes but
have not linked a row yet.

Downloads follow the operator's current channel selection, like every
other path that fetches from Discord: disabling a channel stops fetching
its files (threads inherit their parent's setting). What is already
stored is kept; disabling has never meant deleting.

Attachment reads go through the authenticated OpenConvo API rather than
public object-store URLs. The handler resolves the original filename and media
type from canonical PostgreSQL metadata, opens the digest through the selected
storage driver, and forces a `nosniff` download. Raw S3 keys and storage
credentials are never exposed to browsers.

## Jobs

PostgreSQL-backed queue (`FOR UPDATE SKIP LOCKED`), bounded-concurrency worker
in-process. Failed jobs retry with capped exponential backoff; jobs left
`running` by a crash are requeued at startup by the worker that owns their
kind: one worker per kind, so nobody requeues work another worker is
already executing (documented in code). `dedupe_key` prevents duplicate live
work such as two backfills of one channel.

## Synchronization strategy

```text
1. connect Gateway; live events start flowing
2. historical backfill newest → oldest (resumable, checkpointed)
3. UPSERT by external ID makes overlap harmless
4. reconciliation pass after backfill and after gaps/disconnects
```

Live sync starts before backfill so nothing falls into a gap between them.
Every event and every backfilled page goes through the same `internal/ingest`
write path, which is also where the privacy gate lives: content is written
only for channels the operator enabled (threads inherit their parent's
setting).

A Gateway dispatch advances the resumable Discord sequence only after that
write path succeeds; a transient database failure reconnects from the previous
durable sequence. Backfill checkpoints only complete, parseable pages and
stops without advancing when a channel is disabled mid-run. Reconciliation
continues to its full sync-window boundary rather than abandoning older gaps
behind an arbitrary page cap. Source edit timestamps prevent an older REST
snapshot from replacing a newer live edit.

Gateway delivery is not trusted as a perfect event stream: reconciliation
re-fetches recent history on a schedule and after any resume failure, filling
gaps, applying missed edits and tombstoning messages that disappeared. Rate
limiting lives centrally in the Discord REST client: proactive per-route
buckets from Discord's own headers, with 429 handling as the backstop, never
as scattered sleeps. The one deliberate delay outside it is a named constant
pacing backfill pages: correctness beats import speed.

## Archive browsing

Archive read APIs expose normalized canonical fields rather than source
payloads. They never return an attachment's source URL or a blob object key,
and tombstoned messages are absent from both timelines and context windows.
All of them are behind the administrator session and use private, no-store
responses:

```text
GET /api/v1/channels
GET /api/v1/channels/:id/messages?before=:messageID&limit=50
GET /api/v1/messages/:id?before=20&after=20
```

Channel timelines use message IDs as cursors and return chronological pages.
The web routes `/channels/:id` and `/messages/:id` use internal UUIDs rather
than source names, so renaming a channel cannot break a saved archive link.
Disabling a channel stops future ingestion but leaves its retained messages
browsable, matching the archive-preservation contract.

## Search

PostgreSQL full-text search. `messages.search_vector` is a stored generated
column using the `simple` configuration; communities are frequently not
English-speaking, and `simple` behaves predictably across languages.
Language-aware stemming can be added later as a derived, rebuildable index.

`GET /api/v1/search` uses `websearch_to_tsquery` for plain text and quoted
phrases, ranks matching live messages, and supports channel, author, date and
attachment filters. PostgreSQL also generates bounded highlighted excerpts.
The authenticated `/search` UI renders those excerpts as inert text and links
each result to the stable `/messages/:id` conversation-context route.

When `mode=semantic`, the derived embeddings service sends only the search
query to the configured OpenAI embeddings endpoint, then orders the active
generation with pgvector cosine distance. The same canonical-message joins,
privacy rules, filters, pagination limits, and context links apply. Keyword
mode remains the default and stays entirely local; semantic provider failure
does not affect full-text search.

The MCP adapter exposes those two search paths as a single `search_messages`
tool. `openconvo mcp` uses stdio in a separate process that does not start
migrations, ingestion, jobs, an HTTP listener, or embedding generation. An
opt-in Streamable HTTP transport mounts `/mcp` on the existing application
listener. It sits outside browser session authentication and instead requires
its own bearer token; when disabled, the route is an explicit 404 rather than
the SPA fallback. It uses a separate, bounded read-only PostgreSQL pool and
must be exposed through a TLS-terminating reverse proxy.

Both transports offer no raw SQL, resources, prompts, or write tools, and
return a reduced result that omits actor UUIDs and avatar URLs. This is an
operator-controlled reader integration, not an LLM-generated canonical or
derived archive feature.

## Curation

Bookmarks are durable, operator-authored records attached to canonical live
messages. Their title, description, normalized tags and lightweight collection
name live in PostgreSQL and export with the archive. A collection is deliberately
a string on a bookmark, not a separate hierarchy or service: grouping remains
portable and empty derived objects cannot become load-bearing.

The authenticated UI can create, edit, filter and remove bookmarks. Discord also
registers an administrator-only **Archive** message context-menu command when
`DISCORD_APPLICATION_ID` is configured. That interaction resolves only an
already-ingested `(source, channel external ID, message external ID)` record. It
never fetches content and therefore cannot bypass `internal/ingest`'s enabled-
channel privacy gate. Repeated interactions return the existing bookmark without
overwriting curated metadata.

Message deletion still wins: the bookmark foreign key cascades when a message is
tombstoned, so curation can never preserve or resurrect deleted content.

## Privacy and deletion

Built in from the start, not bolted on:

- only explicitly selected channels are archived; nothing by default; never DMs
- platform deletions are honored (tombstones + scrubbed payloads)
- a deletion received before backfill reaches its message creates a content-free
  tombstone first, so the delayed source payload cannot resurrect it
- `openconvo replay-deletions` can apply a newer export's deletion ledger to
  an older database restore when the operator wants those later deletions too;
  see [restoring a database backup](self-hosting.md#restoring-a-database-backup)
- backups are immutable point-in-time snapshots; OpenConvo does not rewrite
  them after later edits or deletions
- archives are private by default; all administrator APIs require a signed,
  HTTP-only session and same-origin state-changing requests
- actors store no emails, IPs, or profile data OpenConvo doesn't need

## Security posture

- Discord tokens are configuration (environment), never archive data, never logged
- S3 credentials are configuration too and never reach a log line or an error
  string; endpoints are not logged either. Bucket names are: backup and storage
  log lines and errors name the bucket, so treat a bucket name as operational
  metadata, not a secret
- PostgreSQL is not exposed outside the Compose network
- blob digests are validated (`^[0-9a-f]{64}$`) before any path use; no traversal
- attachment content is never executed or trusted; MIME types are metadata
- HTTP server has timeouts and body/header limits; errors don't leak internals
- non-root container user; static binary; minimal runtime image

## Failure model

Assumed normal, not exceptional: Discord disconnects, API failures, rate
limits, vanished attachments, PostgreSQL restarts, full disks, process death
mid-import. The response is always the same toolkit: idempotent writes,
persistent checkpoints, persistent jobs with retries, reconciliation. No
operation may ever require restarting a complete import from zero.

## Embeddings (derived and optional)

OpenConvo has one deliberately narrow semantic-index preset: OpenAI
`text-embedding-3-small` at 256 dimensions over exact message content. It is
off by default and requires an explicit administrator opt-in because enabling
it sends the text of every non-empty archived message to OpenAI. Candidate
selection excludes tombstoned messages, but selection and the provider call are
not one transaction: a message deleted in the interval between them may already
have been sent. Before storing, the vector's transaction re-reads the message
and discards the result if it was deleted or edited, which protects the index,
not the transfer. A send cannot be recalled.

Vectors live in a separate `derived` PostgreSQL schema using pgvector. The
generation row records provider, model, dimensions, and input version so a
future model change cannot silently mix incompatible vectors. Edits and
tombstones delete a message's vectors in the same canonical transaction. A
background sweep rebuilds missing rows, while provider failures never block
or roll back archive ingestion. Logical backups omit the vector values and
rebuild them from canonical messages after restore.

The authenticated search API may embed an administrator's query and retrieve
the nearest active message vectors. It uses the same filters and result shape
as local full-text search, but never silently falls back: disabled, building,
or unavailable semantic search reports its state clearly. Summaries and other
intelligence remain outside the current product. Any such layer must consume
the canonical archive and write only disposable data.
`Discord → LLM → summary → discard originals` is the exact architecture
OpenConvo exists to prevent.
