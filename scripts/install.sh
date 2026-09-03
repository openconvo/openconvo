#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
project_dir=$(cd "${script_dir}/.." && pwd)
env_file="${project_dir}/.env"
env_tmp=""
release_version="0.1.0"
release_image="ghcr.io/openconvo/openconvo:${release_version}"
build_from_source=false

usage() {
  cat <<'EOF'
Usage: ./scripts/install.sh [--build-from-source]

By default the installer pulls the exact published OpenConvo release selected
by this checkout. --build-from-source deliberately builds the working tree
instead; registry, authentication, and network errors never select it
automatically.
EOF
}

case "${1:-}" in
  '') ;;
  --build-from-source) build_from_source=true ;;
  -h|--help) usage; exit 0 ;;
  *) usage >&2; exit 2 ;;
esac
[[ $# -le 1 ]] || { usage >&2; exit 2; }

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

# Quote a value for the .env file that Docker Compose reads. Compose's dotenv
# parser is not a shell: inside single quotes it does no escape processing at
# all, so an apostrophe cannot be escaped there, and the shell's '\'' idiom makes
# the parser reject the whole file. Double quotes are the one form that
# round-trips every character: the parser unescapes \\, \" and \$, and the
# escaped dollar is what stops it interpolating another variable into a secret.
quote_env_value() {
  local value=$1
  value=${value//\\/\\\\}
  value=${value//\"/\\\"}
  value=${value//\$/\\\$}
  printf '"%s"' "${value}"
}

prompt_value() {
  local variable_name=$1
  local label=$2
  local default_value=$3
  local response

  read -r -p "${label} [${default_value}]: " response
  printf -v "${variable_name}" '%s' "${response:-${default_value}}"
}

prompt_required() {
  local variable_name=$1
  local label=$2
  local entered_value

  while true; do
    read -r -p "${label}: " entered_value
    if [[ -n "${entered_value}" ]]; then
      printf -v "${variable_name}" '%s' "${entered_value}"
      return
    fi
    printf 'A value is required.\n' >&2
  done
}

prompt_secret() {
  local variable_name=$1
  local label=$2
  local required=$3
  local entered_value

  while true; do
    read -r -s -p "${label}: " entered_value
    printf '\n'
    if [[ -n "${entered_value}" || "${required}" == false ]]; then
      printf -v "${variable_name}" '%s' "${entered_value}"
      return
    fi
    printf 'A value is required.\n' >&2
  done
}

prompt_yes_no() {
  local variable_name=$1
  local label=$2
  local default_value=$3
  local hint answer

  if [[ "${default_value}" == true ]]; then
    hint='Y/n'
  else
    hint='y/N'
  fi

  while true; do
    read -r -p "${label} [${hint}]: " answer
    case "${answer}" in
      '') printf -v "${variable_name}" '%s' "${default_value}"; return ;;
      y|Y|yes|Yes|YES) printf -v "${variable_name}" '%s' true; return ;;
      n|N|no|No|NO) printf -v "${variable_name}" '%s' false; return ;;
      *) printf 'Please answer yes or no.\n' >&2 ;;
    esac
  done
}

prompt_number() {
  local variable_name=$1
  local label=$2
  local default_value=$3
  local minimum=$4
  local maximum=$5
  local entered_value

  while true; do
    prompt_value entered_value "${label}" "${default_value}"
    if [[ "${entered_value}" =~ ^[0-9]+$ ]] &&
      (( 10#${entered_value} >= minimum && 10#${entered_value} <= maximum )); then
      printf -v "${variable_name}" '%s' "$((10#${entered_value}))"
      return
    fi
    printf 'Enter a number between %s and %s.\n' "${minimum}" "${maximum}" >&2
  done
}

prompt_provider() {
  local variable_name=$1
  local label=$2
  local choice

  printf '%s\n' "${label}"
  printf '  1) Cloudflare R2\n'
  printf '  2) Amazon S3\n'
  printf '  3) Backblaze B2\n'
  printf '  4) Other S3-compatible service\n'
  while true; do
    read -r -p 'Choose a provider [1]: ' choice
    case "${choice:-1}" in
      1) printf -v "${variable_name}" '%s' r2; return ;;
      2) printf -v "${variable_name}" '%s' s3; return ;;
      3) printf -v "${variable_name}" '%s' backblaze; return ;;
      4) printf -v "${variable_name}" '%s' custom; return ;;
      *) printf 'Choose 1, 2, 3, or 4.\n' >&2 ;;
    esac
  done
}

[[ -t 0 ]] || fail "interactive input is required"
[[ ! -e "${env_file}" ]] || fail ".env already exists; leaving it unchanged"
command -v docker >/dev/null 2>&1 || fail "Docker is not installed or not on PATH"
docker compose version >/dev/null 2>&1 || fail "Docker Compose is not available"

printf 'OpenConvo interactive installation\n'
printf 'Press Enter to accept any value shown in brackets.\n\n'

while true; do
  prompt_secret admin_password 'Administrator password (at least 12 characters)' true
  if (( ${#admin_password} < 12 )); then
    printf 'Password must be at least 12 characters. Please try again.\n\n' >&2
    continue
  fi
  if [[ "${admin_password}" == *$'\r'* ]]; then
    printf 'Password cannot contain a carriage return. Please try again.\n\n' >&2
    continue
  fi
  # The server trims whitespace around every configuration value, so a password
  # that starts or ends with one would not be the password it was typed as.
  if [[ "${admin_password}" =~ ^[[:space:]]|[[:space:]]$ ]]; then
    printf 'Password cannot begin or end with a space or tab; OpenConvo ignores\n' >&2
    printf 'them, so the password you typed would not be the one that works.\n\n' >&2
    continue
  fi
  # A trailing backslash or double quote is the one thing quote_env_value cannot
  # represent for every Docker Compose release: older ones either reject the
  # .env file or drop the character. Refuse it here instead.
  if [[ "${admin_password}" == *\\ || "${admin_password}" == *'"' ]]; then
    printf 'Password cannot end with a backslash or a double quote. Please try again.\n\n' >&2
    continue
  fi

  prompt_secret admin_password_confirmation 'Confirm administrator password' true
  if [[ "${admin_password}" != "${admin_password_confirmation}" ]]; then
    printf 'Passwords do not match. Please try again.\n\n' >&2
    continue
  fi
  break
done

prompt_number openconvo_port 'Web port' 8080 1 65535

# Where that port is published decides who can reach the archive. Docker's
# iptables rules are consulted before ufw, so a published port answers whatever
# can route to this machine whatever the host firewall was told.
printf '\nA reverse proxy (Caddy, nginx, Traefik) is how OpenConvo gets HTTPS on a\n'
printf 'domain. Answering yes publishes the web port on 127.0.0.1, where only this\n'
printf 'machine can reach it, and the proxy serves the archive in front of it.\n'
prompt_yes_no behind_proxy 'Will a reverse proxy on this machine serve OpenConvo?' true
public_hostname=''
expose_plaintext=false
if [[ "${behind_proxy}" == true ]]; then
  publish_address=127.0.0.1
  read -r -p 'Domain the proxy will serve (press Enter to decide later): ' public_hostname
  # A pasted address arrives with the scheme attached often enough to be worth
  # handling; printing https://https://archive.example.com back is a poor first
  # impression.
  public_hostname=${public_hostname#http://}
  public_hostname=${public_hostname#https://}
  public_hostname=${public_hostname%%/*}
else
  printf '\nWithout a reverse proxy, remote access sends the administrator password and\n'
  printf 'private archive over unencrypted HTTP. The safe default keeps OpenConvo on\n'
  printf '127.0.0.1; use an SSH tunnel until TLS is configured.\n'
  prompt_yes_no expose_plaintext 'Expose plaintext HTTP to every network interface anyway?' false
  if [[ "${expose_plaintext}" == true ]]; then
    publish_address=0.0.0.0
  else
    publish_address=127.0.0.1
  fi
fi

# This installer usually runs over SSH on a server, where localhost is the one
# address certainly wrong to print. The || true matters: pipefail with set -e
# would abort the run on any system whose hostname has no -I, macOS included.
host_address=$(hostname -I 2>/dev/null | awk '{print $1}' || true)
[[ -n "${host_address}" ]] || host_address=localhost
if [[ -n "${public_hostname}" ]]; then
  web_address="https://${public_hostname}"
elif [[ "${behind_proxy}" == true ]]; then
  web_address="http://127.0.0.1:${openconvo_port}"
elif [[ "${expose_plaintext}" == true ]]; then
  web_address="http://${host_address}:${openconvo_port}"
else
  web_address="http://127.0.0.1:${openconvo_port}"
fi

discord_token='' # gitleaks:allow -- empty until the operator enters it interactively
discord_application_id=''
prompt_yes_no configure_discord 'Configure the Discord bot now?' false
if [[ "${configure_discord}" == true ]]; then
  printf 'Create the bot at https://discord.com/developers/applications first.\n'
  prompt_required discord_application_id 'Discord application ID'
  prompt_secret discord_token 'Discord bot token' true
fi

storage_driver=filesystem
storage_path=/data/attachments
s3_endpoint=''
s3_region=''
s3_bucket=''
s3_access_key=''
s3_secret_key=''
s3_session_token=''
s3_force_path_style=false
attachment_max_bytes=104857600
attachment_storage_summary='local disk'
prompt_yes_no attachments_enabled 'Download and preserve message attachments?' false
if [[ "${attachments_enabled}" == true ]]; then
  prompt_number attachment_max_mib 'Maximum size of one attachment, in MiB' 100 1 1048576
  attachment_max_bytes=$((attachment_max_mib * 1048576))
  prompt_yes_no use_s3_storage 'Store attachments in S3-compatible object storage?' false
  if [[ "${use_s3_storage}" == true ]]; then
    storage_driver=s3
    prompt_provider attachment_provider 'Attachment storage provider:'
    case "${attachment_provider}" in
      r2)
        prompt_required s3_endpoint 'R2 endpoint (https://<account-id>.r2.cloudflarestorage.com)'
        s3_region=auto
        attachment_storage_summary='Cloudflare R2'
        ;;
      s3)
        prompt_value s3_region 'AWS region' us-east-1
        attachment_storage_summary='Amazon S3'
        ;;
      backblaze)
        prompt_required s3_endpoint 'Backblaze S3 endpoint'
        prompt_required s3_region 'Backblaze region'
        attachment_storage_summary='Backblaze B2'
        ;;
      custom)
        prompt_required s3_endpoint 'S3-compatible endpoint'
        prompt_required s3_region 'Signing region'
        prompt_yes_no s3_force_path_style 'Does this service require path-style bucket URLs?' false
        attachment_storage_summary='custom S3-compatible storage'
        ;;
    esac
    prompt_required s3_bucket 'Private bucket name'
    prompt_required s3_access_key 'Access key ID'
    prompt_secret s3_secret_key 'Secret access key' true
    prompt_secret s3_session_token 'Session token (optional; press Enter to skip)' false
  fi
fi

backup_enabled=false
backup_provider=r2
backup_s3_endpoint=''
backup_s3_region=auto
backup_s3_bucket=''
backup_s3_prefix=openconvo/database-backups
backup_s3_access_key=''
backup_s3_secret_key=''
backup_s3_session_token=''
backup_s3_force_path_style=false
backup_interval_hours=24
backup_retention_count=30
backup_summary='off'
prompt_yes_no backup_enabled 'Configure automatic off-site database backups?' false
if [[ "${backup_enabled}" == true ]]; then
  prompt_provider backup_provider 'Backup storage provider:'
  case "${backup_provider}" in
    r2)
      prompt_required backup_s3_endpoint 'R2 endpoint (https://<account-id>.r2.cloudflarestorage.com)'
      backup_s3_region=auto
      backup_summary='Cloudflare R2'
      ;;
    s3)
      backup_s3_endpoint=''
      prompt_value backup_s3_region 'AWS region' us-east-1
      backup_summary='Amazon S3'
      ;;
    backblaze)
      prompt_required backup_s3_endpoint 'Backblaze S3 endpoint'
      prompt_required backup_s3_region 'Backblaze region'
      backup_summary='Backblaze B2'
      ;;
    custom)
      prompt_required backup_s3_endpoint 'S3-compatible endpoint'
      prompt_required backup_s3_region 'Signing region'
      prompt_yes_no backup_s3_force_path_style 'Does this service require path-style bucket URLs?' false
      backup_summary='custom S3-compatible storage'
      ;;
  esac
  prompt_required backup_s3_bucket 'Private backup bucket name'
  prompt_value backup_s3_prefix 'Object key prefix' openconvo/database-backups
  prompt_required backup_s3_access_key 'Access key ID'
  prompt_secret backup_s3_secret_key 'Secret access key' true
  prompt_secret backup_s3_session_token 'Session token (optional; press Enter to skip)' false
  prompt_number backup_interval_hours 'Hours between backups' 24 1 744
  prompt_number backup_retention_count 'Successful backups to retain' 30 1 1000
fi

openai_api_key=''
printf '\nSemantic search sends archived message text to OpenAI. Keyword search stays local.\n'
prompt_yes_no embeddings_enabled 'Enable semantic search?' false
if [[ "${embeddings_enabled}" == true ]]; then
  prompt_secret openai_api_key 'OpenAI API key' true
fi

printf '\nInstallation summary\n'
if [[ "${build_from_source}" == true ]]; then
  printf '  Application:       this checkout (development build)\n'
else
  printf '  Application:       OpenConvo %s published image\n' "${release_version}"
fi
printf '  Web address:       %s\n' "${web_address}"
if [[ "${behind_proxy}" == true ]]; then
  printf '  Published on:      127.0.0.1 only, for the reverse proxy\n'
elif [[ "${expose_plaintext}" == true ]]; then
  printf '  Published on:      every interface (unencrypted HTTP)\n'
else
  printf '  Published on:      127.0.0.1 only; use an SSH tunnel\n'
fi
if [[ "${configure_discord}" == true ]]; then
  printf '  Discord:           configured\n'
else
  printf '  Discord:           configure later\n'
fi
if [[ "${attachments_enabled}" == true ]]; then
  printf '  Attachments:       enabled, %s, %s MiB limit\n' "${attachment_storage_summary}" "${attachment_max_mib}"
else
  printf '  Attachments:       off\n'
fi
if [[ "${backup_enabled}" == true ]]; then
  printf '  Database backups:  enabled, %s, every %s hours\n' "${backup_summary}" "${backup_interval_hours}"
else
  printf '  Database backups:  off\n'
fi
if [[ "${embeddings_enabled}" == true ]]; then
  printf '  Semantic search:   enabled (OpenAI)\n'
else
  printf '  Semantic search:   off\n'
fi
printf '  Database password: generated automatically\n\n'

prompt_yes_no continue_install 'Write .env and start OpenConvo?' true
if [[ "${continue_install}" != true ]]; then
  printf 'Installation cancelled; no files were written.\n'
  exit 0
fi

database_password=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
[[ ${#database_password} -eq 64 ]] || fail "could not generate a database password"

umask 077
env_tmp=$(mktemp "${project_dir}/.env.tmp.XXXXXX")
{
  printf '# Generated by scripts/install.sh. Never commit this file.\n\n'
  printf 'POSTGRES_PASSWORD=%s\n' "${database_password}"
  printf 'OPENCONVO_ADMIN_PASSWORD=%s\n' "$(quote_env_value "${admin_password}")"
  printf 'OPENCONVO_IMAGE=%s\n' "${release_image}"
  printf 'OPENCONVO_PORT=%s\n' "${openconvo_port}"
  printf 'OPENCONVO_PUBLISH_ADDRESS=%s\n\n' "${publish_address}"
  printf 'DISCORD_TOKEN=%s\n' "$(quote_env_value "${discord_token}")"
  printf 'DISCORD_APPLICATION_ID=%s\n\n' "$(quote_env_value "${discord_application_id}")"
  printf 'STORAGE_DRIVER=%s\n' "${storage_driver}"
  printf 'STORAGE_PATH=%s\n' "$(quote_env_value "${storage_path}")"
  printf 'OPENCONVO_ATTACHMENTS_ENABLED=%s\n' "${attachments_enabled}"
  printf 'OPENCONVO_ATTACHMENT_MAX_BYTES=%s\n' "${attachment_max_bytes}"
  printf 'S3_ENDPOINT=%s\n' "$(quote_env_value "${s3_endpoint}")"
  printf 'S3_REGION=%s\n' "$(quote_env_value "${s3_region}")"
  printf 'S3_BUCKET=%s\n' "$(quote_env_value "${s3_bucket}")"
  printf 'S3_ACCESS_KEY=%s\n' "$(quote_env_value "${s3_access_key}")"
  printf 'S3_SECRET_KEY=%s\n' "$(quote_env_value "${s3_secret_key}")"
  printf 'S3_SESSION_TOKEN=%s\n' "$(quote_env_value "${s3_session_token}")"
  printf 'S3_FORCE_PATH_STYLE=%s\n\n' "${s3_force_path_style}"
  printf 'BACKUP_ENABLED=%s\n' "${backup_enabled}"
  printf 'BACKUP_PROVIDER=%s\n' "${backup_provider}"
  printf 'BACKUP_S3_ENDPOINT=%s\n' "$(quote_env_value "${backup_s3_endpoint}")"
  printf 'BACKUP_S3_REGION=%s\n' "$(quote_env_value "${backup_s3_region}")"
  printf 'BACKUP_S3_BUCKET=%s\n' "$(quote_env_value "${backup_s3_bucket}")"
  printf 'BACKUP_S3_PREFIX=%s\n' "$(quote_env_value "${backup_s3_prefix}")"
  printf 'BACKUP_S3_ACCESS_KEY=%s\n' "$(quote_env_value "${backup_s3_access_key}")"
  printf 'BACKUP_S3_SECRET_KEY=%s\n' "$(quote_env_value "${backup_s3_secret_key}")"
  printf 'BACKUP_S3_SESSION_TOKEN=%s\n' "$(quote_env_value "${backup_s3_session_token}")"
  printf 'BACKUP_S3_FORCE_PATH_STYLE=%s\n' "${backup_s3_force_path_style}"
  printf 'BACKUP_INTERVAL_HOURS=%s\n' "${backup_interval_hours}"
  printf 'BACKUP_RETENTION_COUNT=%s\n\n' "${backup_retention_count}"
  printf 'OPENCONVO_EMBEDDINGS_ENABLED=%s\n' "${embeddings_enabled}"
  printf 'OPENAI_API_KEY=%s\n\n' "$(quote_env_value "${openai_api_key}")"
  printf 'LOG_LEVEL=info\n'
  printf 'LOG_FORMAT=text\n'
} >"${env_tmp}"

chmod 600 "${env_tmp}"
mv "${env_tmp}" "${env_file}"
env_tmp=""
unset admin_password admin_password_confirmation database_password
unset discord_token s3_secret_key s3_session_token
unset backup_s3_secret_key backup_s3_session_token openai_api_key

printf '\nConfiguration written to .env (mode 0600).\n'
printf 'Starting OpenConvo...\n\n'

cd "${project_dir}"
if [[ "${build_from_source}" == true ]]; then
  printf 'Building this checkout because --build-from-source was selected.\n'
  docker compose -f compose.yaml -f compose.dev.yaml up -d --build
else
  printf 'Pulling %s...\n' "${release_image}"
  if ! docker compose pull openconvo; then
    fail "could not pull ${release_image}; check registry access and the network, or rerun with --build-from-source to deliberately build this checkout"
  fi
  docker compose up -d
fi

# The first run pulls or builds an image and applies every migration, so give
# it minutes rather than seconds before calling the deployment unhealthy.
readiness_timeout_seconds=300
readiness_deadline=$((SECONDS + readiness_timeout_seconds))
printf '\nWaiting for OpenConvo to become ready (the first run can take a few minutes)...\n'
openconvo_ready=false
while (( SECONDS < readiness_deadline )); do
  if docker compose exec -T openconvo openconvo healthcheck >/dev/null 2>&1; then
    openconvo_ready=true
    break
  fi
  sleep 1
done
if [[ "${openconvo_ready}" != true ]]; then
  printf '\nOpenConvo has not answered its health check after %s seconds.\n' \
    "${readiness_timeout_seconds}" >&2
  printf 'Configuration was written to .env and the containers were started, so the\n' >&2
  printf 'deployment may simply still be coming up. Check on it with:\n' >&2
  printf '  docker compose logs -f openconvo\n' >&2
  printf '  docker compose exec openconvo openconvo status\n' >&2
  exit 1
fi

if [[ "${behind_proxy}" == true ]]; then
  printf '\nOpenConvo is running, published on 127.0.0.1:%s for your reverse proxy.\n' \
    "${openconvo_port}"
  if [[ -n "${public_hostname}" ]]; then
    printf 'Point the proxy at that address to serve https://%s\n' "${public_hostname}"
  fi
  printf 'docs/self-hosting.md has the three-line Caddyfile for exactly this.\n'
elif [[ "${expose_plaintext}" == true ]]; then
  printf '\nOpenConvo is running at %s over unencrypted HTTP.\n' "${web_address}"
  printf 'Configure TLS as soon as possible; the administrator login is private data.\n'
else
  printf '\nOpenConvo is running on 127.0.0.1:%s only.\n' "${openconvo_port}"
  printf 'From your workstation, open an SSH tunnel with:\n'
  printf '  ssh -L %s:127.0.0.1:%s <your-server>\n' "${openconvo_port}" "${openconvo_port}"
  printf 'Then open http://127.0.0.1:%s locally.\n' "${openconvo_port}"
fi
if [[ "${configure_discord}" != true ]]; then
  printf 'Add the Discord credentials to .env when you are ready to begin archiving.\n'
fi
