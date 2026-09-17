#!/usr/bin/env bash

# Renders app.ini from this service's environment and runs Gitea.
#
# Exiting non-zero is how this script waits: the platform re-runs the start
# command alone — admin-init.sh and then this — and the boot after this one
# may have what this one lacked. On the very first boot that is the four
# secrets init.sh has just written: a variable written now reaches processes
# started later. After that it is the zerops login source, the only way in
# (sign-in is through the broker): admin-init.sh can add it only on a boot
# where the broker's secret has resolved and its discovery answers, and a Gitea
# that started without it would serve nobody and never get another look —
# measured on the owner's run of 2026-09-17, where the source was left "for a
# later boot" at 11:20:38 and no boot came until a restart by hand.

set -euo pipefail

: "${GITEA_BIN:=/var/www/bin/gitea}"
: "${CONF:=/etc/gitea/app.ini}"

# require_zerops_source fails when Gitea has no login source named zerops —
# or cannot list its sources at all — so the boot is retried rather than served.
require_zerops_source() {
  if "$GITEA_BIN" admin auth list --config "$CONF" 2>/dev/null | awk '$2=="zerops"{found=1} END{exit !found}'; then
    return 0
  fi
  echo "start.sh: the zerops login source is not there yet, restarting ..."
  return 1
}

# A test sources this file to drive one function on its own; a boot does not
# set it and runs the whole thing below.
if [ "${START_SOURCE_ONLY:-}" = 1 ]; then
  return 0 2>/dev/null || exit 0
fi

cd /var/www

for secret in JWT_SECRET LFS_JWT_SECRET SECRET_KEY INTERNAL_TOKEN; do
  if [ -z "${!secret:-}" ]; then
    echo "start.sh: secret $secret not set yet, restarting ..."
    exit 1
  fi
done

echo "start.sh: rendering $CONF ..."
zsc envReplace --silent gitea/app.ini /tmp/app.ini
sudo install -m 660 -o root -g zerops /tmp/app.ini "$CONF"

require_zerops_source || exit 1

echo "start.sh: starting gitea ..."
exec "$GITEA_BIN" web --config "$CONF"
