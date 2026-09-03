# Contributing to OpenConvo

**OpenConvo is open source, but not open to external code contributions.**

Bug reports, feature requests, implementation feedback and use-case
discussion are genuinely wanted, and GitHub Issues is the place for all of
them. Development itself is done by the maintainers.

Pull request creation is restricted to collaborators, so GitHub won't let you
open one. That restriction is a courtesy, not a slight: it stops you writing a
patch that was never going to be merged. Please don't paste proposed patches
into issues either.

This is the same model [SQLite](https://sqlite.org/copyright.html) uses. The
license governs what you may do with the code you receive; it doesn't oblige
the project to take code back.

## Why

One reason, stated plainly: the copyright in OpenConvo has a single owner, and
keeping it that way keeps the licensing options open. AGPL-3.0-or-later is the
license today. A hosted product on top of the same code is the intended
business model, and that requires being able to license the code more than one
way. Merging outside patches forecloses that permanently: licensing the code
differently would need every past contributor's permission, and one person who
can't be reached is enough to freeze it for good.

The trade-off is real: you cannot submit your own fix upstream. You remain
free to modify or fork the project under the AGPL. In exchange, OpenConvo stays
free software under a copyleft license that a hosted fork cannot keep closed,
and the maintainer has a reason to keep the project healthy.

## What helps most

- **Bug reports** with steps to reproduce, what you expected, what happened,
  and the version (`openconvo version`). Logs, config with secrets removed,
  stack traces and minimal reproductions are all welcome; diagnostics are
  not patches.
- **Feature requests** framed as the problem you hit and what you were trying
  to do, not as a designed solution. The use case is the useful part.
- **Feedback on decisions already made**: schema, export format, deployment
  shape, docs that mislead. This is the highest-value input the project gets
  and the hardest to obtain.
- **Telling us it worked.** Which platform, what scale of guild, how long the
  backfill took.

## What can't be accepted

- Pull requests, including one-line and typo fixes. GitHub won't let you open
  one; open an issue instead. A typo is faster for a maintainer to fix
  directly than to review.
- Patches, diffs or proposed implementations pasted into issues or
  discussions. Describe the change; don't write it.

To keep the codebase's provenance unambiguous, maintainers cannot review or
use submitted code as the basis for a fix. If an issue identifies a valid
underlying problem, the maintainers may implement a solution independently and
will credit the issue.

This policy can change. If it does, this file will say so and the pull request
restriction will be lifted at the same time.

## Security issues

Never in a public issue. See [SECURITY.md](SECURITY.md), which routes reports
to a private advisory.

## Forking

The [AGPL-3.0-or-later](LICENSE) grants you the right to run, study, modify
and redistribute OpenConvo, and nothing here restricts it. If the project's
direction doesn't suit you, fork it. The rest of this file is the handbook
you'll need. The one obligation the license adds: offer your users the source
of your modified version if you let them reach it over a network.

---

# How OpenConvo is built

The conventions below are for maintainers, forks, and coding agents working in
this repository.

## Ground rules

- Read [docs/architecture.md](docs/architecture.md) first. The most important
  rule: **canonical archive data is sacred; everything else is derived and
  rebuildable.** Changes that couple the archive schema to one platform, or
  that make deletion/privacy handling weaker, don't land.
- Keep the stack boring. New runtime dependencies (Go modules, services,
  containers) need a strong justification; the deployment story is one
  binary + PostgreSQL, and self-hosters bear every dependency we add.
- Match the existing style: standard library first, explicit SQL (no ORM),
  plain CSS, small interfaces that grow only when a second implementation
  needs them.

## Development setup

Requirements: Go ≥ 1.26.6 (the version in `go.mod`), Node ≥ 22, Docker (for
database tests).

```bash
make help        # all targets
make build       # frontend + binary → bin/openconvo
make test        # fast tests (PostgreSQL-backed tests skip)
make test-db     # full suite against an ephemeral postgres container
make lint        # gofmt + go vet + tsc
make screenshots # documentation images from synthetic data; needs Chrome
```

Documentation screenshots render the real compiled frontend against the fixed
synthetic API in `scripts/screenshots`. They never read a local archive or
credentials. Set `OPENCONVO_SCREENSHOT_CHROME` when Chrome or Chromium is not
installed in a conventional location.

Working on the backend and frontend together:

```bash
# terminal 1: a PostgreSQL to develop against. The initial migration creates
# the `vector` extension, so stock postgres images cannot run the schema; use
# the same pgvector image as Compose, CI and scripts/test-db.sh.
docker run -d --name oc-dev -e POSTGRES_PASSWORD=dev -p 5432:5432 \
  pgvector/pgvector:0.8.6-pg17-bookworm

# terminal 2: the API on :8080 (`serve` requires an admin password of at
# least 12 characters, and fails closed before touching the database)
DATABASE_URL=postgres://postgres:dev@localhost:5432/postgres?sslmode=disable \
OPENCONVO_ADMIN_PASSWORD=local-dev-password \
STORAGE_PATH=/tmp/oc-attachments make dev

# terminal 3: Vite with hot reload on :5173, proxying /api → :8080
make dev-web
```

## Coding agents

`CLAUDE.md` (symlinked as `AGENTS.md`) carries the architecture invariants an
agent has to respect. `.claude/` holds the shared Claude Code setup:

- `settings.json`: allows this repo's build and test commands to run without
  a prompt, and denies reads of `.env` and `tmp/`, which hold credentials and
  locally archived attachments. The deny is a guardrail against an incidental
  read, not a sandbox: a shell command can still reach either path.
- `skills/running-locally/`: running the server outside Docker.

`.mcp.json` at the root *describes* one server (OpenConvo's own read-only
`search_messages`, over `docker compose exec` against your local install),
but describing is not enabling: nothing runs until you approve it.

Beyond that, the checked-in setup enables no plugins and no MCP servers; those
are personal choices, not repository ones. Keep yours in
`.claude/settings.local.json`, which is gitignored.

Most of OpenConvo is written with agent assistance. That doesn't loosen any of
the rules below: the diff is explainable, the tests are real, and the checks
pass, or it doesn't land.

## Tests

- Pure logic is tested without external services.
- Anything touching PostgreSQL uses `testutil.NewDB(t)`, which creates a
  throwaway database per test (skipped unless `TEST_DATABASE_URL` is set;
  `make test-db` and CI set it). Don't mock SQL.
- Discord behavior is tested with recorded/synthetic payload fixtures in
  `internal/discord/testdata/`; tests never call Discord.
- High-value behaviors (idempotent upserts, deletion semantics, resumable
  jobs) must keep their tests when the code around them changes.

## Migrations

- Files are `migrations/NNNN_description.sql`, applied in order, up-only.
- Never edit an applied migration; add a new one. Rewriting or squashing the
  migration history was a pre-1.0 privilege; now that releases are public that
  window is closed for good.
- Schema DDL belongs in a schema migration. A one-off data repair gets its
  own file, because it is not part of the schema a fresh install creates.
- Schema changes to canonical tables must stay source-agnostic
  (`source` + `external_id`, never `discord_*` columns).

## Before a change lands

- One focused change per commit or branch, with a clear description of the
  behavior change and tests alongside it.
- `make lint` and `make test-db` must pass. CI does not invoke the make targets
  themselves; it runs the equivalent steps (gofmt, `go vet`, `go test ./...`
  against a PostgreSQL service, `go build ./...`, and the frontend build, which
  type-checks).
- Public-facing changes (config, API, export format) need a docs update in
  the same change. The export format (docs/archive-format.md) is a public
  interface; breaking it needs a format version bump and very good reasons.

## License

OpenConvo is licensed under the [GNU Affero General Public License v3.0 or
later](LICENSE). Copyleft is deliberate: a fork that is offered to users over a
network has to publish its source too.
