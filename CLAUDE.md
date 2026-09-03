# OpenConvo

Open-source, self-hosted archive that keeps a community's own copy of its
knowledge: a faithful, private record of the chat it lives in (Discord first).
Single Go binary + PostgreSQL, React/Vite frontend embedded in the binary,
two-container Docker Compose deployment.

## First principles

Every decision (feature, dependency, schema, scope) passes these filters.
When a task fights one of them, stop and surface the conflict instead of
pushing through.

1. **The faithful archive is the foundation.** If every derived system
   disappeared, the canonical data (messages, attachments, metadata,
   relationships) must still faithfully reconstruct the conversations.
   Search, exports and AI are derived data: rebuildable, disposable, never
   load-bearing. Knowledge features build on the source material; they never
   replace it.
2. **Never lose, never leak.** Losing archived data is the unforgivable
   failure; so is archiving anything not explicitly enabled, or
   resurrecting anything deleted. Durability and privacy outrank every
   other concern, including import speed and features.
3. **Boring to run.** Installed once, left alone for years. Self-hosters
   pay forever for every dependency, service and knob we add, so the default
   answer to a new one is no.
4. **No lock-in.** Everything exports to documented, boring formats. The
   archive must stay useful if Discord, OpenConvo, or its maintainers
   disappear.
5. **OSS stays complete.** Capture, browse, search, curate and export are
   the real product, not a teaser. A future hosted product may add managed
   operations and knowledge intelligence on top. Never cripple OSS to sell
   hosting.
6. **Current milestone only.** Build the vertical slice in progress and
   nothing speculative: no second source until Discord is excellent, no AI
   in core, no generic framework before a second consumer exists. The
   non-goals in docs/roadmap.md are binding.

## Commands

```bash
make build       # frontend + binary → bin/openconvo
make test        # Go tests; PostgreSQL-backed tests SKIP without TEST_DATABASE_URL
make test-db     # full suite against ephemeral postgres container (needs Docker)
make lint        # gofmt check + go vet + tsc
make web         # frontend only → internal/web/dist (embedded via internal/web)
```

Single package: `go test ./internal/archive/ -run TestMessageLifecycle`
(export `TEST_DATABASE_URL=postgres://test:test@127.0.0.1:54329/test?sslmode=disable`
after starting a postgres like scripts/test-db.sh does).

## Architecture invariants (docs/architecture.md is authoritative)

- Canonical archive tables are source-agnostic: identity is
  `(source, external_id)`. Never add `discord_*` columns; platform extras go
  in `raw_payload` jsonb.
- Archive writes must be idempotent (duplicate events are normal), defensive
  (partial updates never erase known fields), and deletion-safe (tombstones
  are never resurrected; every deletion writes to `deletion_ledger`).
- Search/index/AI anything = derived data, rebuildable from canonical tables.
- No new runtime dependencies without strong justification. The direct Go
  dependencies are pgx (PostgreSQL), coder/websocket (the Discord gateway),
  the AWS SDK v2 S3 client (S3-compatible blob storage and off-site backups)
  and modelcontextprotocol/go-sdk (the optional read-only MCP search
  adapter); go.mod is the list. Each does something the standard library
  does not, and a fifth has to clear the same bar. No Redis, no ORM, no
  router framework.
- Style: standard library first, explicit SQL, plain CSS, small interfaces
  that grow only when a second implementation needs them (CONTRIBUTING.md
  has the full conventions).
- Migrations: `migrations/NNNN_name.sql`, up-only, never edit applied ones.
- Discord rate limiting lives ONLY in internal/discord's client, which paces
  discord.com/api per route from Discord's own rate-limit headers. No ad-hoc
  sleeps, and exactly two deliberate exceptions: `backfillPagePause` in
  internal/syncer, and attachment downloads in internal/attachments, which
  fetch cdn.discordapp.com through a plain http.Client because the CDN
  returns no rate-limit headers and is not part of the REST budget (the URL
  refresh a download needs does go through the limited client).
  Discord tests never touch the real API: use the in-process fake REST +
  Gateway server in internal/discord/discordtest, plus payload fixtures in
  internal/discord/testdata.
- internal/ingest is the single archive write path for live events, backfill
  and reconciliation, and is where the "only enabled channels" privacy gate
  lives. Do not write message content to the store from anywhere else.
- internal/discord must not import internal/ingest (ingest depends on discord
  for the normalized types); Source.Run takes a local Ingester interface.
- Jobs assume one worker per kind, in one process: at startup each worker
  requeues only the `running` jobs of its own kinds (`ResetRunning` takes
  those kinds). Two workers sharing a kind, or a second process, needs
  lease-based expiry instead.
- Import direction between internal packages is enforced by
  `go test ./internal/arch/`. If an architecture change legitimately trips it,
  update its rules table in the same commit and say why.
- internal/attachments owns the download pipeline and blob reclamation.
  Downloads are gated by OPENCONVO_ATTACHMENTS_ENABLED; reclamation is
  not, because it enforces deletion. Failures are classified source-side
  (mark the attachment failed) or storage-side (leave it pending; a full
  disk is not the file's fault).
- Blob reclamation deletes the row BEFORE the file. ON DELETE RESTRICT
  makes that a live re-check against a concurrent download that
  deduplicated onto the same content; the reverse order can delete bytes
  a live attachment points at.

## State

All eight roadmap milestones have shipped. What exists today: Discord sync
(gateway, live events, resumable backfill, reconciliation, channel
selection), the attachment download pipeline over filesystem or S3 blob
storage, the archive UI behind administrator authentication, keyword search
plus an optional pgvector semantic index, preservation (JSONL and Markdown
exports, `verify`, `replay-deletions`, scheduled off-site
database backups), curation (bookmarks, the in-Discord "Archive" message
action), the guided installer (`scripts/install.sh`), dashboard-assisted
upgrade detection, and the Discord administration page.

docs/roadmap.md holds the per-milestone status, the known limitations and
the deferred non-goals; it is the file kept current, so read status there
rather than trusting a summary here. Principle 6 ("current milestone only")
now reads against that file's open items and non-goals rather than against a
milestone number.
