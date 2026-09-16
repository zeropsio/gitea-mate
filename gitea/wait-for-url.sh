#!/usr/bin/env bash

# Waits for a URL to answer 200.
#
#   wait-for-url.sh <url> [deadline seconds] [interval seconds]
#
# Why it exists: Gitea's `admin auth add-oauth` reads the provider's discovery
# document there and then. A source added while the broker is still building
# is written to the database with no provider behind it — Gitea logs "Failed to
# create OpenID Connect Provider … Non-success code for Discovery URL: 502"
# (measured on 1.27.2, 2026-09-16) — the sign-in page offers no Zerops link,
# and only a restart of Gitea ever creates the provider. Both services are
# imported together, so on a fresh account this is the normal case, not the
# unlucky one.
#
# Nothing of the answer is printed: a discovery document is public, but this
# script is also the shape anything else here would wait with.

set -uo pipefail

url="${1:?usage: wait-for-url.sh <url> [deadline seconds] [interval seconds]}"
deadline="${2:-900}"
interval="${3:-5}"

end=$(( $(date +%s) + deadline ))
while :; do
  status="$(curl -fsS -o /dev/null -w '%{http_code}' --max-time 10 "$url" 2>/dev/null || true)"
  if [ "$status" = "200" ]; then
    exit 0
  fi
  if [ "$(date +%s)" -ge "$end" ]; then
    echo "wait-for-url.sh: gave up after ${deadline}s (last status ${status:-none})" >&2
    exit 1
  fi
  sleep "$interval"
done
