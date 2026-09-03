---
name: running-locally
description: Use when running, starting, or smoke-testing the OpenConvo server on a dev machine outside Docker; covers the local database, the storage path override, logging in to the private API, enabling a channel, and the attachment download flags.
---

# Run OpenConvo locally

Docker Compose is the deployment path; this is the fast loop against a
local PostgreSQL. With the default `OPENCONVO_AUTO_MIGRATE=true`, migrations
apply at startup.

## Setup (once)

The server needs pgvector: the initial migration runs `CREATE EXTENSION vector`
unconditionally, so startup aborts on a PostgreSQL without it. Check first;
an empty result means it is missing (`brew install pgvector` on macOS):

```bash
psql -d postgres -tAc "select 1 from pg_available_extensions where name='vector'"
createdb openconvo
go build -o bin/openconvo ./cmd/openconvo   # `make build` if you need the UI + version stamp
```

Otherwise run the image compose and the tests already use:

```bash
docker run -d --name openconvo-dev-pg -p 127.0.0.1:5433:5432 \
  -e POSTGRES_USER=openconvo -e POSTGRES_PASSWORD=openconvo \
  -e POSTGRES_DB=openconvo pgvector/pgvector:0.8.6-pg17-bookworm
```

Its `DATABASE_URL` is
`postgres://openconvo:openconvo@127.0.0.1:5433/openconvo?sslmode=disable`;
use it in place of the one below.

## Run

```bash
set -a; . .env; set +a                         # DISCORD_TOKEN, etc.
export DATABASE_URL="postgres://$USER@127.0.0.1:5432/openconvo?sslmode=disable"
export OPENCONVO_ADMIN_PASSWORD=local-dev-password   # >=12 chars, or serve
                                               # exits; .env ships it empty
export STORAGE_PATH="$PWD/tmp/attachments"     # .env ships the container path, /data/attachments
export LOG_LEVEL=debug
./bin/openconvo serve
```

`curl -s localhost:8080/health` to confirm it is up; `./bin/openconvo
status` (same env) prints counts, storage and per-channel sync state.

## Archiving something

Nothing is archived until a channel is explicitly enabled, and enabling
one stores real message content, so confirm the channel with your human
partner first.

Every `/api/` route needs an administrator session (`/health` is the only
public endpoint), so log in first and reuse the cookie. Requests that change
state also need a same-origin `Origin` header:

```bash
curl -s -c /tmp/ck.cookies -X POST -H 'content-type: application/json' \
  -H 'Origin: http://localhost:8080' \
  -d "{\"password\":\"$OPENCONVO_ADMIN_PASSWORD\"}" \
  localhost:8080/api/v1/auth/session

curl -s -b /tmp/ck.cookies localhost:8080/api/v1/communities
curl -s -b /tmp/ck.cookies localhost:8080/api/v1/communities/{id}/channels
curl -s -b /tmp/ck.cookies -X PUT -H 'content-type: application/json' \
  -H 'Origin: http://localhost:8080' -d '{"enabled":true}' \
  localhost:8080/api/v1/channels/{id}/archive
```

Sessions last 12 hours and are signed with a per-process key, so a restart
invalidates them: a `401 authentication required` means log in again.

## Attachment downloads

Off by default. All three are required or files sit at `pending` forever:

- `OPENCONVO_ATTACHMENTS_ENABLED=true`
- `DISCORD_TOKEN` set; with no URL refresher the pipeline stays off even
  when enabled, and says so at startup
- a writable `STORAGE_PATH`

The sweep enqueues up to 500 per pass, once a minute; blobs land
content-addressed by sha256. Track it with `./bin/openconvo status`:
`N stored, N pending, N failed`.

## Teardown

Run this only for the dedicated development resources created above.

```bash
pkill -f 'bin/openconvo serve'
dropdb openconvo && rm -rf tmp/attachments /tmp/ck.cookies
docker rm -f openconvo-dev-pg                 # only if you used the container
```
