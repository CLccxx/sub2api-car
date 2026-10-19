#!/bin/sh
# Initialize a fresh production bundle. Existing secrets must never be rotated
# implicitly, because JWT sessions and encrypted TOTP data depend on them.
set -eu
umask 077
cd "$(dirname "$0")"

domain=${1:-}
case "$domain" in
  ''|*[!A-Za-z0-9.-]*|.*|*..*|*.)
    echo "Usage: sh init-car.sh your.domain.example (hostname only)" >&2
    exit 1
    ;;
esac
case "$domain" in
  *.*) ;;
  *) echo "A fully qualified domain name is required" >&2; exit 1 ;;
esac

if [ -e .env ]; then
  echo ".env already exists; keeping all existing secrets. Initialization stopped." >&2
  exit 1
fi
if [ ! -s ../sub2api ] || [ ! -s ../BUILD_VERSION ]; then
  echo "Use the compiled deployment bundle containing sub2api and BUILD_VERSION." >&2
  exit 1
fi
command -v openssl >/dev/null
version=$(cat ../BUILD_VERSION)
postgres_password=$(openssl rand -hex 32)
redis_password=$(openssl rand -hex 32)
jwt_secret=$(openssl rand -hex 32)
totp_key=$(openssl rand -hex 32)
admin_suffix=$(openssl rand -hex 4)
admin_password=$(openssl rand -hex 24)

{
  printf 'SUB2API_CAR_VERSION=%s\n' "$version"
  printf 'SUB2API_DOMAIN=%s\n' "$domain"
  printf 'POSTGRES_PASSWORD=%s\n' "$postgres_password"
  printf 'REDIS_PASSWORD=%s\n' "$redis_password"
  printf 'JWT_SECRET=%s\n' "$jwt_secret"
  printf 'TOTP_ENCRYPTION_KEY=%s\n' "$totp_key"
  printf 'ADMIN_EMAIL=admin-%s@%s\n' "$admin_suffix" "$domain"
  printf 'ADMIN_PASSWORD=%s\n' "$admin_password"
} > .env

echo "Created private .env for $domain. No services have been started."
echo "Keep this file backed up. Read ADMIN_EMAIL and ADMIN_PASSWORD locally after startup."
