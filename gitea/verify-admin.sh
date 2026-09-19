#!/usr/bin/env bash

# Proves the published site-admin token against the Gitea this boot is
# serving, and mints a new one when Gitea refuses it.
#
# admin-init.sh runs before the web server exists, so it can publish a pair but
# never try one. That gap is what made a bad pair permanent: on the run of
# 2026-09-19 the first boot published a token Gitea answered 401 to from the
# broker's first call onward, and every later boot took the `already
# provisioned` path and left it exactly as it was. The broker re-reads a pair
# Gitea refused (internal/siteadmin) — and kept reading the same one.
#
# So this runs in the background of the start command, after Gitea is up: one
# authenticated call, and a re-mint when it fails. It never logs a value.
#
# The very first boot has nothing to prove: admin-init.sh has only just
# published the pair and a variable written now reaches processes started
# later. That boot exits here and the next one checks.

set -euo pipefail

: "${GITEA_BIN:=/var/www/bin/gitea}"
: "${CONF:=/etc/gitea/app.ini}"
: "${GITEA_LOCAL_URL:=http://localhost:3000}"
USERNAME="${GITEA_ADMIN_USERNAME:-admin}"

# The same rule admin-init.sh and the broker apply: a reference the platform
# has not filled in arrives verbatim and is not empty.
resolved() {
  case "${1:-}" in
    "") return 1 ;;
    '${'*'}') return 1 ;;
  esac
  return 0
}

# authenticates answers 0 when Gitea accepts the token for the admin's own
# route. Only 401/403 count as a refusal: a connection error or a 5xx is this
# boot being early, not a bad credential, and must never cost a re-mint.
authenticates() {
  local code
  code="$(curl -sS -o /dev/null -w '%{http_code}' \
    -H "Authorization: token $1" "$GITEA_LOCAL_URL/api/v1/user" || echo 000)"
  case "$code" in
    200) return 0 ;;
    401|403) return 1 ;;
    *) return 2 ;;
  esac
}

if [ "${VERIFY_ADMIN_SOURCE_ONLY:-}" = 1 ]; then
  return 0 2>/dev/null || exit 0
fi

cd /var/www

if ! resolved "${GITEA_ADMIN_TOKEN:-}"; then
  echo "verify-admin.sh: no published token on this boot yet, the next boot proves it"
  exit 0
fi

./gitea/wait-for-url.sh "$GITEA_LOCAL_URL/api/v1/version" 300 || {
  echo "verify-admin.sh: gitea did not answer locally, nothing proved this boot"
  exit 0
}

# `set -e` would take a refusal as a failure of the script, and `$?` after an
# `if` is the `if`'s, not the function's — so the status is taken directly.
set +e
authenticates "$GITEA_ADMIN_TOKEN"
verdict=$?
set -e
case "$verdict" in
  0)
    echo "verify-admin.sh: the published site admin token authenticates"
    exit 0
    ;;
  2)
    echo "verify-admin.sh: gitea answered neither yes nor no, nothing proved this boot"
    exit 0
    ;;
esac

echo "verify-admin.sh: gitea refused the published site admin token, minting a new pair ..."
password="$(head -c 24 /dev/urandom | base64 | tr -d '/+=' | cut -c1-28)"
"$GITEA_BIN" admin user change-password --config "$CONF" --username "$USERNAME" \
  --password "$password" --must-change-password=false
token="$("$GITEA_BIN" admin user generate-access-token --config "$CONF" --username "$USERNAME" \
  --token-name "automation-$(date +%s)" --scopes all --raw)"

set +e
authenticates "${token:-}"
minted=$?
set -e
if [ -z "${token:-}" ] || [ "$minted" -ne 0 ]; then
  echo "verify-admin.sh: the newly minted token does not authenticate either, publishing nothing"
  exit 1
fi

printf '%s' "$password" | zsc setEnv --sensitive GITEA_ADMIN_PASSWORD -
printf '%s' "$token"    | zsc setEnv --sensitive GITEA_ADMIN_TOKEN -
echo "verify-admin.sh: a working pair is published; the broker reads it on its next pass"
