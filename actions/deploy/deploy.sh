#!/usr/bin/env bash

# Deploys what the broker allows this job to deploy, with `zcli push` (D27).
#
# The job holds no Zerops credential of its own. It tells the broker which
# commit it has checked out; the broker answers the environment's deploy token
# only to a proved job of the default branch's own workflow, holding exactly
# the commit protected state wants there — or says why there is nothing for
# this job to do, which ends it green.
#
# What is pushed is the commit's tree (`--workspace-state clean`), never the
# working directory: a test step runs the repository's code before this, and
# nothing it left behind may ride along on a key it was never given.
#
# The token lives in one process's environment for one `zcli push`, with a
# throwaway HOME that is removed afterwards. It is masked in the log and never
# written to a file by this script.

set -euo pipefail

# No apostrophes inside ${…:?…}: bash 3.2 reads one there as an opening quote.
: "${BROKER:?BROKER is the URL of the broker}"
: "${REPOSITORY:?REPOSITORY is owner/name of the repository this job runs in}"
: "${JOB_TOKEN:?JOB_TOKEN is the token of this job}"
ENVIRONMENT="${ENVIRONMENT:-}"
SERVICE="${SERVICE:-}"
WORKDIR="${WORKDIR:-.}"
# A job started by a push is granted one environment at a time; this bounds a
# broker that would never say "nothing".
MAX_GRANTS="${MAX_GRANTS:-10}"

for tool in git curl node zcli; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "deploy: this runner has no $tool; the group's runner image ships it" >&2
    exit 1
  fi
done

cd "$WORKDIR"
sha="$(git rev-parse HEAD)"

# field <key>: one top-level value of the JSON on stdin, or nothing.
field() {
  node -e '
    let raw = ""; process.stdin.on("data", (c) => (raw += c)).on("end", () => {
      try { const v = JSON.parse(raw)[process.argv[1]]; process.stdout.write(v == null ? "" : String(v)); }
      catch { process.stdout.write(""); }
    });' "$1"
}

# json <key> <value> …: an object of strings, escaped properly.
json() {
  node -e '
    const out = {}; const a = process.argv.slice(1);
    for (let i = 0; i + 1 < a.length; i += 2) if (a[i + 1] !== "") out[a[i]] = a[i + 1];
    process.stdout.write(JSON.stringify(out));' "$@"
}

# api <path> <body>: prints the body, then the status code on its own last line.
api() {
  curl -sS -X POST "$BROKER$1" \
    -H "Authorization: token $JOB_TOKEN" \
    -H 'Content-Type: application/json' \
    -d "$2" -w '\n%{http_code}'
}

report() { # report <id> <status> <message>
  api "/deploy/$1/result" "$(json repository "$REPOSITORY" status "$2" message "$3")" >/dev/null 2>&1 || true
}

home=""
cleanup() { [ -n "$home" ] && rm -rf "$home"; return 0; }
trap cleanup EXIT

deployed=0
for _ in $(seq 1 "$MAX_GRANTS"); do
  answer="$(api /deploy/grant "$(json repository "$REPOSITORY" sha "$sha" environment "$ENVIRONMENT" service "$SERVICE")")"
  code="$(printf '%s' "$answer" | tail -n1)"
  body="$(printf '%s' "$answer" | sed '$d')"

  if [ "$code" != "200" ]; then
    echo "deploy: the broker refused ($code $(printf '%s' "$body" | field error)): $(printf '%s' "$body" | field message)" >&2
    exit 1
  fi

  status="$(printf '%s' "$body" | field status)"
  if [ "$status" != "granted" ]; then
    # live, nothing, superseded, in_progress: nothing for this job to do.
    echo "deploy: $status — $(printf '%s' "$body" | field message)"
    break
  fi

  token="$(printf '%s' "$body" | field token)"
  echo "::add-mask::$token"
  id="$(printf '%s' "$body" | field id)"
  environment="$(printf '%s' "$body" | field environment)"
  service="$(printf '%s' "$body" | field service)"
  granted="$(printf '%s' "$body" | field sha)"

  if [ "$(git rev-parse HEAD)" != "$granted" ]; then
    report "$id" failure "the job's checkout moved off $granted before the push"
    echo "deploy: the checkout is no longer at $granted" >&2
    exit 1
  fi

  echo "deploy: $service of $environment ← $granted"
  home="$(mktemp -d)"
  set +e
  HOME="$home" ZEROPS_TOKEN="$token" zcli push \
    --project-id "$(printf '%s' "$body" | field projectId)" \
    --service-id "$(printf '%s' "$body" | field serviceId)" \
    --setup "$(printf '%s' "$body" | field setup)" \
    --version-name "$(printf '%s' "$body" | field versionName)" \
    --workspace-state clean
  pushed=$?
  set -e
  rm -rf "$home"; home=""
  unset token

  if [ "$pushed" -ne 0 ]; then
    report "$id" failure "zcli push exited $pushed; the job's log has the build"
    echo "deploy: zcli push failed for $service of $environment" >&2
    exit 1
  fi
  report "$id" success ""
  deployed=$((deployed + 1))

  # A dispatched job was told which environment; a push's job asks again for
  # whatever else its branch feeds.
  [ -n "$ENVIRONMENT" ] && break
done

echo "deploy: $deployed deployed"
