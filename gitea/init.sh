#!/usr/bin/env bash

# Runs once per container lifetime, through `zsc execOnce prepare-gitea`:
# creates Gitea's database role and database, prepares the work directory on
# the volume, and generates the four secrets Gitea signs and encrypts with —
# publishing them as this service's own environment variables so a rebuilt
# container keeps every existing session and LFS token.
#
# https://docs.gitea.com/installation/database-prep/

set -euo pipefail

cd /var/www
: "${GITEA_BIN:=/var/www/bin/gitea}"

for var in DB_HOST DB_PORT DB_NAME DB_USER DB_PASSWORD DB_ADMIN_USER DB_ADMIN_PASSWORD DB_ADMIN_DB GITEA_WORK_DIR; do
  if [ -z "${!var:-}" ]; then
    echo "init.sh: $var is not set, aborting"
    exit 1
  fi
done

echo "init.sh: creating role $DB_USER and database $DB_NAME ..."
# init.sh is retried after a failure, so every statement survives a second run.
# The heredoc is quoted and psql reads the values itself, so neither the shell
# nor the SQL parser can trip over a generated password — and no password ever
# reaches argv.
export DB_NAME DB_USER DB_PASSWORD
PGPASSWORD="$DB_ADMIN_PASSWORD" psql -X -v ON_ERROR_STOP=1 -h "$DB_HOST" -p "$DB_PORT" -U "$DB_ADMIN_USER" -d "$DB_ADMIN_DB" <<'SQL'
\getenv db_name DB_NAME
\getenv db_user DB_USER
\getenv db_password DB_PASSWORD

SELECT format('CREATE ROLE %I', :'db_user')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = :'db_user')
\gexec

SELECT format('ALTER ROLE %I WITH LOGIN PASSWORD %L', :'db_user', :'db_password')
\gexec

SELECT format('CREATE DATABASE %I WITH OWNER %I TEMPLATE template0 ENCODING UTF8 LC_COLLATE ''en_US.UTF-8'' LC_CTYPE ''en_US.UTF-8''', :'db_name', :'db_user')
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = :'db_name')
\gexec
SQL

echo "init.sh: preparing $GITEA_WORK_DIR ..."
sudo mkdir -p "$GITEA_WORK_DIR"/{custom,data,indexers,public,log}
sudo mkdir -p "${GITEA_DUMP_DIR:-/mnt/volume/dumps}"
sudo chown -R zerops:zerops "$GITEA_WORK_DIR" "${GITEA_DUMP_DIR:-/mnt/volume/dumps}"
sudo chmod -R 750 "$GITEA_WORK_DIR" "${GITEA_DUMP_DIR:-/mnt/volume/dumps}"

for secret in JWT_SECRET LFS_JWT_SECRET SECRET_KEY INTERNAL_TOKEN; do
  echo "init.sh: generating and setting secret $secret ..."
  # The generated values are base64url and can start with a dash, which zsc
  # would parse as a flag, so they go in on stdin. Nothing is echoed.
  value="$("$GITEA_BIN" generate secret "$secret")"
  printf '%s' "$value" | zsc setEnv --sensitive "$secret" -
done

echo "init.sh: done"
