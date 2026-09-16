#!/usr/bin/env bash

# Two things, both idempotent and both invisible on every boot but the ones
# that need them:
#
#   1. the site admin (`admin` — docs/vocabulary.md, never `mate`) and an API
#      token for the broker, published as this service's own environment
#      variables the way init.sh publishes Gitea's secrets;
#   2. the `zerops` OIDC login source, pointing at this account's broker.
#
# Nothing is printed and, where the CLI allows it, nothing is passed on argv,
# so no credential reaches a log or the process list.
#
# Runs from the start command, not from initCommands. Init commands do run on
# every container start, but when the start command exits non-zero the platform
# re-runs the start command alone — and that is exactly what happens here on
# the first boot, where start.sh exits until the secrets init.sh just wrote have
# propagated. From initCommands this would get one look at an empty environment
# and never be reached again.
#
# https://docs.gitea.com/administration/command-line#admin

set -euo pipefail

cd /var/www
: "${GITEA_BIN:=/var/www/bin/gitea}"
CONF=/etc/gitea/app.ini
USERNAME="${GITEA_ADMIN_USERNAME:-admin}"
EMAIL="${GITEA_ADMIN_EMAIL:-$USERNAME@localhost}"

# The very first boot has none of these yet: init.sh has only just written
# them, and a variable written now reaches processes started later, not this
# one. start.sh is about to exit for the same reason, and the boot after this
# one has everything.
for secret in JWT_SECRET LFS_JWT_SECRET SECRET_KEY INTERNAL_TOKEN; do
  if [ -z "${!secret:-}" ]; then
    echo "admin-init.sh: $secret not set yet, nothing to do on this boot"
    exit 0
  fi
done

# The admin commands read app.ini and talk to the database directly, so both
# have to exist before the web server has ever run. `gitea migrate` is the
# documented way: initDB opens the database, only migrate creates the schema.
echo "admin-init.sh: rendering $CONF and migrating the database ..."
zsc envReplace --silent gitea/app.ini /tmp/app.ini
sudo install -m 660 -o root -g zerops /tmp/app.ini "$CONF"
"$GITEA_BIN" migrate --config "$CONF"

provision_admin() {
  if [ -n "${GITEA_ADMIN_TOKEN:-}" ]; then
    echo "admin-init.sh: the site admin is already provisioned"
    return 0
  fi

  local password token created
  if "$GITEA_BIN" admin user list --config "$CONF" 2>/dev/null | awk 'NR>1{print $2}' | grep -qx "$USERNAME"; then
    # The user survived but the variable did not. A token's value is readable
    # only at creation, so it cannot be recovered — mint a new one and reset
    # the password.
    echo "admin-init.sh: $USERNAME exists, re-minting its credentials ..."
    password="$(head -c 24 /dev/urandom | base64 | tr -d '/+=' | cut -c1-28)"
    # --password is argv-only on this subcommand; there is no file or stdin
    # form. The value is generated here and never printed.
    "$GITEA_BIN" admin user change-password --config "$CONF" --username "$USERNAME" \
      --password "$password" --must-change-password=false
    token="$("$GITEA_BIN" admin user generate-access-token --config "$CONF" --username "$USERNAME" \
      --token-name "automation-$(date +%s)" --scopes all --raw)"
    echo "admin-init.sh: NOTE the previous access token is still valid — revoke it if you are rotating after a leak"
  else
    # --random-password and --access-token both print their value, which is why
    # neither is passed as an argument: the output is captured here and never
    # echoed.
    echo "admin-init.sh: creating the site admin $USERNAME ..."
    created="$("$GITEA_BIN" admin user create --config "$CONF" \
      --admin --username "$USERNAME" --email "$EMAIL" \
      --random-password --must-change-password=false \
      --access-token --access-token-name automation --access-token-scopes all)"
    password="$(printf '%s' "$created" | sed -n "s/^generated random password is '\(.*\)'\$/\1/p")"
    token="$(printf '%s' "$created" | sed -n 's/^Access token was successfully created\.\.\. //p')"
  fi

  if [ -z "${password:-}" ] || [ -z "${token:-}" ]; then
    echo "admin-init.sh: could not read the generated credentials, aborting"
    return 1
  fi

  # Published before anything else can fail. The user and the token exist in
  # the database by now, and a token's value is readable only at creation —
  # losing it here would mean a Gitea nobody holds the credentials for.
  #
  # The broker needs BOTH: Gitea's token routes (/users/{login}/tokens) answer
  # 401 "auth required" to an API token, however privileged (measured on
  # 1.27.2), so minting a Mate bot's credential takes the admin's basic auth.
  #
  # Values go in on stdin, like init.sh does: a generated value can begin with
  # a dash, which zsc would otherwise parse as a flag.
  echo "admin-init.sh: publishing the site admin's credentials ..."
  printf '%s' "$password" | zsc setEnv --sensitive GITEA_ADMIN_PASSWORD -
  printf '%s' "$token"    | zsc setEnv --sensitive GITEA_ADMIN_TOKEN -
}

add_oidc_source() {
  if [ -z "${BROKER_PUBLIC_URL:-}" ] || [ -z "${OIDC_CLIENT_SECRET:-}" ]; then
    echo "admin-init.sh: BROKER_PUBLIC_URL or OIDC_CLIENT_SECRET is not set yet, leaving the OIDC source for a later boot"
    return 0
  fi
  if "$GITEA_BIN" admin auth list --config "$CONF" 2>/dev/null | awk 'NR>1{print $2}' | grep -qx zerops; then
    echo "admin-init.sh: the zerops login source already exists"
    return 0
  fi

  echo "admin-init.sh: adding the zerops login source ..."
  # --secret is argv-only: `admin auth add-oauth` offers no file or stdin form
  # for it. The value comes from this service's sensitive environment and is
  # never echoed; it is visible in this container's process list for the
  # moment the command runs, and nowhere else.
  #
  # --admin-group org:owner makes Gitea's site admins exactly the org's owners
  # (D10): the broker puts `org:owner` in the groups claim for them and for
  # nobody else.
  "$GITEA_BIN" admin auth add-oauth --config "$CONF" \
    --name zerops \
    --provider openidConnect \
    --key gitea \
    --secret "$OIDC_CLIENT_SECRET" \
    --auto-discover-url "$BROKER_PUBLIC_URL/.well-known/openid-configuration" \
    --scopes "openid email profile groups" \
    --group-claim-name groups \
    --admin-group org:owner
}

provision_admin
add_oidc_source

echo "admin-init.sh: done"
