#!/usr/bin/env bash
# Runs the full Go test suite (including PostgreSQL-backed tests)
# against an ephemeral postgres container. Usage:
#
#   ./scripts/test-db.sh [go test args...]
#
set -euo pipefail

PORT="${TEST_PG_PORT:-54329}"
NAME="openconvo-test-pg"

cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; }
cleanup
trap cleanup EXIT

docker run -d --name "$NAME" \
  -e POSTGRES_USER=test -e POSTGRES_PASSWORD=test -e POSTGRES_DB=test \
  -p "127.0.0.1:${PORT}:5432" \
  pgvector/pgvector:0.8.6-pg17-bookworm >/dev/null

echo "waiting for postgres on port ${PORT}..."
ready=false
for _ in $(seq 1 60); do
  if docker exec "$NAME" pg_isready -U test >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 0.5
done
if [[ "$ready" != true ]]; then
  echo "postgres did not become ready within 30s; last container logs:" >&2
  docker logs --tail 20 "$NAME" >&2 || true
  exit 1
fi

export TEST_DATABASE_URL="postgres://test:test@127.0.0.1:${PORT}/test?sslmode=disable"
go test ./... "$@"
