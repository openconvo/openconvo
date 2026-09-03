#!/bin/sh
set -eu

# Create both forms of preservation: a fast PostgreSQL restore point and a
# self-contained, implementation-independent OpenConvo export. Run from the
# directory containing compose.yaml:
#
#   ./scripts/backup.sh [destination-directory]
#
# This writes a second full copy of the archive, so plan for the space twice.
# Inside the container the export is staged under /data — the mounted volume,
# not the container's writable layer, which a large archive would otherwise
# fill — so that volume needs room for one more copy of every blob, and so
# does the destination directory here on the host. Set OPENCONVO_EXPORT_STAGE
# to stage somewhere else inside the container; it must be a writable path on
# a mounted volume.
umask 077

if [ ! -f compose.yaml ]; then
  printf 'error: no compose.yaml here; run this from the deployment directory\n' >&2
  exit 1
fi

backup_root=${1:-./backup}
backup_stamp=$(date -u +%Y%m%dT%H%M%SZ)
final_dir=${backup_root}/openconvo-${backup_stamp}
if [ -e "$final_dir" ]; then
  printf 'error: %s already exists; refusing to overwrite it\n' "$final_dir" >&2
  exit 1
fi
mkdir -p "$backup_root"
work_dir=$(mktemp -d "${backup_root}/.openconvo-${backup_stamp}.XXXXXX")
container_stage=${OPENCONVO_EXPORT_STAGE:-/data/exports}
container_export=${container_stage}/openconvo-export-${backup_stamp}-$$

cleanup() {
  docker compose exec -T openconvo rm -r "$container_export" >/dev/null 2>&1 || true
  rm -r "$work_dir" >/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

docker compose exec -T postgres pg_dump -U openconvo -Fc openconvo > "$work_dir/openconvo.dump"
docker compose exec -T openconvo openconvo export --output "$container_export"
docker compose exec -T openconvo openconvo verify "$container_export"
docker compose exec -T openconvo tar -C "$container_stage" -czf - "$(basename "$container_export")" > "$work_dir/openconvo-export.tar.gz"
docker compose exec -T openconvo rm -r "$container_export"

mv "$work_dir" "$final_dir"
trap - EXIT INT TERM
printf 'backup written to %s\n' "$final_dir"
