#!/usr/bin/env bash

# Registers this container as an Actions runner of ONE group's Gitea org.
#
# The registration token arrives as this service's own sensitive variable —
# the broker puts it there at import, and a container reads only its own
# service's variables (ledger 2026-09-16), so a job cannot read the Gitea
# admin's token or the database password from here.
#
# Nothing else about Zerops is on this container: no zcli, no ZEROPS_TOKEN, no
# deploy key. A workflow that wants to deploy asks the broker.
#
# https://docs.gitea.com/runner/registration/

set -euo pipefail

cd /var/www
export HOME="${HOME:-/home/zerops}"
: "${RUNNER_BIN:=/var/www/bin/gitea-runner}"

for var in GITEA_INSTANCE_URL RUNNER_LABELS; do
  if [ -z "${!var:-}" ]; then
    echo "runner-init.sh: $var is not set, aborting"
    exit 1
  fi
done

if [ -z "${RUNNER_REGISTRATION_TOKEN:-}" ]; then
  echo "runner-init.sh: RUNNER_REGISTRATION_TOKEN is not set yet, restarting ..."
  exit 1
fi

echo "runner-init.sh: waiting for $GITEA_INSTANCE_URL ..."
for _ in $(seq 1 20); do
  curl -fsS -o /dev/null "$GITEA_INSTANCE_URL/api/healthz" && break
  sleep 3
done
if ! curl -fsS -o /dev/null "$GITEA_INSTANCE_URL/api/healthz"; then
  echo "runner-init.sh: $GITEA_INSTANCE_URL is not reachable, restarting ..."
  exit 1
fi

# Each container registers itself as a separate runner named after its
# hostname. The .runner file lives in the deploy dir, so a plain restart reuses
# the registration while a recreated container registers fresh (the old
# registration stays behind as an offline runner).
if [ ! -f .runner ]; then
  echo "runner-init.sh: registering runner $HOSTNAME ..."
  # --token is argv-only: act_runner's register offers no file or stdin form.
  # The value is this container's own sensitive variable and is never echoed;
  # it is visible in this container's process list for the moment the command
  # runs, and a registration token is single-purpose and scoped to one org.
  "$RUNNER_BIN" register \
    --no-interactive \
    --instance "$GITEA_INSTANCE_URL" \
    --token "$RUNNER_REGISTRATION_TOKEN" \
    --name "$HOSTNAME" \
    --labels "$RUNNER_LABELS"
fi

echo "runner-init.sh: done"
