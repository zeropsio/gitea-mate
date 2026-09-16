#!/usr/bin/env bash

# Renders app.ini from this service's environment and runs Gitea.
#
# On the very first boot the four secrets init.sh has just written have not
# reached this process yet — a variable written now reaches processes started
# later. Exiting non-zero is how that is waited out: the platform re-runs the
# start command alone, and the boot after this one has everything.

set -euo pipefail

cd /var/www
: "${GITEA_BIN:=/var/www/bin/gitea}"
CONF=/etc/gitea/app.ini

for secret in JWT_SECRET LFS_JWT_SECRET SECRET_KEY INTERNAL_TOKEN; do
  if [ -z "${!secret:-}" ]; then
    echo "start.sh: secret $secret not set yet, restarting ..."
    exit 1
  fi
done

echo "start.sh: rendering $CONF ..."
zsc envReplace --silent gitea/app.ini /tmp/app.ini
sudo install -m 660 -o root -g zerops /tmp/app.ini "$CONF"

echo "start.sh: starting gitea ..."
exec "$GITEA_BIN" web --config "$CONF"
