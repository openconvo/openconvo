#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
project_dir=$(cd "${script_dir}/.." && pwd)
env_file="${project_dir}/.env"
env_tmp=""

cleanup() {
  if [[ -n "${env_tmp}" && -f "${env_tmp}" ]]; then
    rm -f -- "${env_tmp}"
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

[[ $# -eq 1 ]] || fail "usage: ./scripts/upgrade.sh <version> (for example, 0.1.1)"
target_version=${1#v}
[[ "${target_version}" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
  fail "version must be a stable semantic version such as 0.1.1"

target_image="ghcr.io/openconvo/openconvo:${target_version}"
[[ -f "${env_file}" ]] || fail ".env does not exist; run this from an installed OpenConvo release"
command -v docker >/dev/null 2>&1 || fail "Docker is not installed or not on PATH"
docker compose version >/dev/null 2>&1 || fail "Docker Compose is not available"

cd "${project_dir}"
printf 'Pulling %s before changing the installation...\n' "${target_image}"
docker pull "${target_image}"

printf 'Creating the required pre-upgrade database and portable backups...\n'
"${script_dir}/backup.sh"

umask 077
env_tmp=$(mktemp "${project_dir}/.env.tmp.XXXXXX")
awk -v replacement="OPENCONVO_IMAGE=${target_image}" '
  BEGIN { replaced = 0 }
  /^OPENCONVO_IMAGE=/ {
    if (!replaced) print replacement
    replaced = 1
    next
  }
  { print }
  END { if (!replaced) print replacement }
' "${env_file}" >"${env_tmp}"
chmod 600 "${env_tmp}"
mv "${env_tmp}" "${env_file}"
env_tmp=""

printf 'Recreating OpenConvo with %s...\n' "${target_image}"
docker compose up -d --force-recreate openconvo
docker compose exec -T openconvo openconvo healthcheck
docker compose exec -T openconvo openconvo version
