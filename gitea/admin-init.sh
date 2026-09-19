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

# A test sources this file to drive one function on its own; a boot does not
# set it and runs the whole thing at the bottom.
if [ "${ADMIN_INIT_SOURCE_ONLY:-}" != 1 ]; then
  cd /var/www
fi
: "${GITEA_BIN:=/var/www/bin/gitea}"
: "${CONF:=/etc/gitea/app.ini}"
USERNAME="${GITEA_ADMIN_USERNAME:-admin}"
EMAIL="${GITEA_ADMIN_EMAIL:-$USERNAME@localhost}"

# The very first boot has none of these yet: init.sh has only just written
# them, and a variable written now reaches processes started later, not this
# one. start.sh is about to exit for the same reason, and the boot after this
# one has everything.
if [ "${ADMIN_INIT_SOURCE_ONLY:-}" != 1 ]; then
  for secret in JWT_SECRET LFS_JWT_SECRET SECRET_KEY INTERNAL_TOKEN; do
    if [ -z "${!secret:-}" ]; then
      echo "admin-init.sh: $secret not set yet, nothing to do on this boot"
      exit 0
    fi
  done
fi

# The admin commands read app.ini and talk to the database directly, so both
# have to exist before the web server has ever run. `gitea migrate` is the
# documented way: initDB opens the database, only migrate creates the schema.
echo "admin-init.sh: rendering $CONF and migrating the database ..."
zsc envReplace --silent gitea/app.ini /tmp/app.ini
sudo install -m 660 -o root -g zerops /tmp/app.ini "$CONF"
"$GITEA_BIN" migrate --config "$CONF"

# A credential this container generated or a CLI returned whole, never one
# scraped out of prose.
#
# Gitea's own `create` subcommand can print a generated password and token in
# sentences, and those sentences are Gitea's to change. A run of 2026-09-19 on
# Gitea 1.27.2 published a pair the same Gitea then refused on every call —
# 401 from the broker's first request onward, for the life of the account —
# with no abort, because the `sed` that reads them produced something
# non-empty. Both values are now known by construction: the password is made
# here and passed in, and the token comes back from `--raw`, which prints the
# token and nothing else.
mint_credentials() {
  password="$(head -c 24 /dev/urandom | base64 | tr -d '/+=' | cut -c1-28)"
  # --password and --token-name are argv-only on these subcommands; there is no
  # file or stdin form. Values are generated here and never printed.
  if "$GITEA_BIN" admin user list --config "$CONF" 2>/dev/null | awk 'NR>1{print $2}' | grep -qx "$USERNAME"; then
    echo "admin-init.sh: $USERNAME exists, re-minting its credentials ..."
    "$GITEA_BIN" admin user change-password --config "$CONF" --username "$USERNAME" \
      --password "$password" --must-change-password=false
    echo "admin-init.sh: NOTE the previous access token is still valid — revoke it if you are rotating after a leak"
  else
    echo "admin-init.sh: creating the site admin $USERNAME ..."
    "$GITEA_BIN" admin user create --config "$CONF" \
      --admin --username "$USERNAME" --email "$EMAIL" \
      --password "$password" --must-change-password=false
  fi
  token="$("$GITEA_BIN" admin user generate-access-token --config "$CONF" --username "$USERNAME" \
    --token-name "automation-$(date +%s)" --scopes all --raw)"
}

provision_admin() {
  local password token
  # `resolved`, not `-n`: a reference the platform has not filled in reaches
  # this container as the literal `${…}` and is not empty, so `-n` would call
  # an unpublished pair "already provisioned" and never mint one. The broker
  # applies the same rule to the same pair (internal/siteadmin, `Arrived`).
  if resolved "${GITEA_ADMIN_TOKEN:-}" && resolved "${GITEA_ADMIN_PASSWORD:-}"; then
    echo "admin-init.sh: the site admin is already provisioned"
    return 0
  fi

  mint_credentials

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

# resolved: a value the platform has actually filled in. `OIDC_CLIENT_SECRET`
# is `${broker_OIDC_CLIENT_SECRET}` in the import, and that reference reaches
# this container VERBATIM until the platform resolves the sibling — 28
# characters, not empty, so an emptiness guard lets it through. A login source
# written with it is one Gitea can never authenticate with: every sign-in ends
# `Failed OAuth callback: (internal) oauth2: "invalid_client"`, measured on a
# real Mate 2026-09-16, and nothing repairs it because the source exists.
resolved() {
  case "${1:-}" in
    "") return 1 ;;
    '${'*'}') return 1 ;;
  esac
  return 0
}

add_oidc_source() {
  if ! resolved "${BROKER_PUBLIC_URL:-}" || ! resolved "${OIDC_CLIENT_SECRET:-}"; then
    echo "admin-init.sh: BROKER_PUBLIC_URL or OIDC_CLIENT_SECRET has not resolved yet; start.sh restarts this boot until it has"
    return 0
  fi
  # Repair, not skip. The secret this boot holds is the authoritative one, and
  # a source written on an earlier boot may carry the unresolved reference —
  # the one state no sign-in can recover from and no other boot would fix.
  # update-oauth with an unchanged secret is a no-op write.
  source_id=$("$GITEA_BIN" admin auth list --config "$CONF" 2>/dev/null | awk '$2=="zerops"{print $1}' | head -n 1)
  if [ -n "${source_id:-}" ]; then
    echo "admin-init.sh: refreshing the zerops login source's client secret ..."
    "$GITEA_BIN" admin auth update-oauth --config "$CONF" \
      --id "$source_id" --key gitea --secret "$OIDC_CLIENT_SECRET"
    return 0
  fi

  # add-oauth reads the discovery document as it runs. Both services are
  # imported at once, so on a fresh account the broker is usually still
  # building — and a source added then is written with no provider behind it
  # ("Failed to create OpenID Connect Provider ... Non-success code for
  # Discovery URL: 502", measured 2026-09-16): the sign-in page offers no
  # Zerops link and, since the guard above then sees the source, no later boot
  # ever repairs it. So wait; on a broker that never answers, return without
  # the source and let start.sh refuse to serve, which restarts this boot.
  echo "admin-init.sh: waiting for the broker's discovery URL ..."
  if ! ./gitea/wait-for-url.sh "$BROKER_PUBLIC_URL/.well-known/openid-configuration" 900; then
    echo "admin-init.sh: the broker never answered; start.sh restarts this boot and this runs again"
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

if [ "${ADMIN_INIT_SOURCE_ONLY:-}" != 1 ]; then
  provision_admin
  add_oidc_source
  echo "admin-init.sh: done"
fi
