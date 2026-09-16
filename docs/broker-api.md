# The broker's HTTP API

Five endpoints and an OIDC provider. **No endpoint takes a Zerops key**, and being inside the
project proves nothing — the runners share its network — so every call is checked against the party
it claims to be. JSON in and out; errors are `{"error": "<code>", "message": "<plain words>"}`.

Names used below are in `docs/vocabulary.md`; the rights function in `docs/roles.md`.

## Proving a person: the throwaway check

`POST /mate/credential` and `POST /oidc/complete` take a **throwaway Zerops integration token**
the Mate app minted as the person, `Authorization: Bearer <token>`. The broker accepts it only when
every one of these holds, in this order, and refuses with `throwaway_invalid` plus a `reason`
otherwise:

1. `GET /user/info` as the token answers — its `id` is the token's own id (`token_dead` otherwise).
2. `GET /client/{ZEROPS_CLIENT_ID}/integration-token/{id}` as the token answers `200`
   (`wrong_org` otherwise — a token from another org cannot read this org's tokens).
3. `roleCode == NO_ACCESS`, `projects` empty, `canCreateProjects == false` and no finance flag
   (`has_rights`).
4. `name` starts with `gitea-signin:{host}:` where `host` is this broker's Gitea host
   (`GITEA_PUBLIC_URL` without scheme) (`wrong_name`).
5. `created` is within five minutes of the **Zerops API's `Date` response header** — never the
   container's clock (`stale`).
6. `createdByUser` is an `ACTIVE` member of the org, read with the broker's own token
   (`not_member`).

The caller is `createdByUser`. The app deletes the throwaway right after the call; the broker
never stores it.

## `POST /mate/credential` — a Mate's Gitea access (guide 1.5)

Caller: the Mate app, as the person, by a throwaway.

```json
{ "project": "<Zerops project id>", "mode": "ensure" }
```

`mode` is `ensure` (default) or `rotate`.

Checks: the project is a `mate` entry of the registry (`not_registered`); the caller is its
effective `OWNER`, or an org `OWNER`/`ADMIN` (`not_owner` — the role function decides). Then the
broker makes the bot if it is missing (in the group's org, in its `read` team) and:

- `ensure`: when a live token of the bot exists (any `mate/{bot}/{n}`), nothing new is minted;
- `ensure` with none, or `rotate`: mints generation *n+1* (`mate/{bot}/{n+1}`, scopes
  `write:repository,read:user`). Older generations are revoked by the rights loop only once the
  newest is ten minutes old — never here.

```json
{ "url": "https://web-1234-3000.prg1.zerops.app", "org": "acme", "bot": "mate-p1",
  "generation": 2, "minted": true, "token": "<value>" }
```

`token` is present only when `minted` is true. The app writes `GITEA_URL` and `GITEA_TOKEN` onto
the Mate's `zcp` service and restarts it.

## `POST /mate/repository` — a service repository for a Mate (guide 1.5, 2.1)

Caller: a Mate (zcp), `Authorization: token <its bot's Gitea token>`.

```json
{ "name": "api" }
```

The broker resolves the token against Gitea (`GET /api/v1/user`): the login must be a
`mate-{projectId}` bot (`not_a_bot`) whose project is registered (`not_registered`); the org is the
project's group. It creates `{org}/{name}` — private, `auto_init` with default branch `main`,
`main` protected (no direct push, merge by the `write` team) — and adds the bot as a collaborator
with write. Idempotent: an existing repository the bot already collaborates on answers `200` with
`created: false`; one it does not answers `409 taken`.

```json
{ "fullName": "acme/api", "cloneUrl": "https://web-1234-3000.prg1.zerops.app/acme/api",
  "defaultBranch": "main", "created": true }
```

`cloneUrl` never carries a `.git` suffix (the platform's clone preflight fails on one). The Mate
pushes to a branch of its own (`mate/{bot name}`) and lands on `main` through pull requests.

## `POST /deploy`, `GET /deploy/{id}` — a workflow asks for a deploy (guide 5.3, 5.4)

Caller: a job on the group's runner, `Authorization: token <github.token>`.

```json
{ "environment": "stage", "service": "api", "repository": "acme/api" }
```

How the broker knows the caller (measured 2026-09-16): `GET /api/v1/user` with the token answers
`login: gitea-actions`, `login_name: @gitea-actions/{taskId}` (`not_a_job` otherwise); then
`GET /repos/{repository}/actions/jobs/{taskId}` with the same token must answer `200`
(`wrong_repository` — it is `404` for every repository but the job's own, public ones included)
and gives `run_id`, `head_sha`, `head_branch`. `GET /repos/{repository}` alone proves nothing.
The workflow needs default permissions or `actions: read` for that call.

The repository's org names the group; `environment` must be one of that group's environments
(`unknown_environment`); `service` a service of it (`unknown_service`). **The caller picks the
environment, never a commit or a ref:** a stage deploys the head of its source ref; production the
commits the newest tag with `mate/release: approved` lists. The request is queued per environment,
newest wins.

`202`:

```json
{ "id": "d_8f2a…", "environment": "stage", "service": "api", "sha": "3f9c…", "status": "queued" }
```

`GET /deploy/{id}` (same authentication, the same job or any job of the same repository):

```json
{ "id": "d_8f2a…", "status": "running", "sha": "3f9c…", "versionId": "…", "message": "" }
```

`status` is `queued`, `running`, `active` or `failed`. The broker keeps deploys in memory: after a
restart an unknown id is `404`, and the action then reads the commit status
`mate/deploy/{environment}/{service}` on the sha instead — the same result, written by the broker
on every outcome.

## `POST /hooks/gitea` — Gitea's webhooks (guide 1.6, 5.3, 5.5)

Caller: Gitea. `X-Gitea-Signature` is the hex HMAC-SHA256 of the raw body with
`GITEA_WEBHOOK_SECRET`; anything else is `401 bad_signature`. `X-Gitea-Event` selects:

| Event | What the broker does |
|---|---|
| `push` to a source branch of an environment | deploys the environment (per 5.3) |
| `push` to `env/*` | nothing — the broker wrote it |
| `create` of a tag `v*` on the group repo | re-checks the pusher's production rights in Zerops, writes `mate/release: approved` or `refused` on the tagged commit, deploys an approved tag's commits |
| `pull_request` merged on the group repo | imports the recipe delta into each environment built from it, re-reads environments |
| `workflow_job` `queued` | starts the group's runner service if it is stopped |
| `workflow_job` `completed` | notes the time; a quiet spell (default 15 min) stops the runner |
| anything else | `204`, ignored |

Always `204` once the signature is good, whatever the payload; the work runs after the response.

## OIDC provider — Gitea's *Sign in with Zerops* (guide 3.6)

Issuer `BROKER_PUBLIC_URL`. One client, `client_id` `gitea`, `client_secret` `OIDC_CLIENT_SECRET`,
one allowed `redirect_uri`: `{GITEA_PUBLIC_URL}/user/oauth2/zerops/callback`.

| Route | |
|---|---|
| `GET /.well-known/openid-configuration` | discovery: `code` flow, `ES256`, scopes `openid email profile groups`, claims `sub email name preferred_username groups` |
| `GET /oidc/jwks` | the ES256 key derived from `OIDC_SEED` (`kid` = the first 8 hex of its SHA-256) |
| `GET /oidc/authorize` | validates `client_id`, `redirect_uri`, `response_type=code`; stores `{redirect_uri, state, nonce, scope}` under a random `rid` for ten minutes; `302` to `{MATE_APP_URL}/gitea-signin?rid={rid}&broker={BROKER_PUBLIC_URL}` |
| `POST /oidc/complete` | body `{"rid": "…"}`, `Authorization: Bearer <gitea-signin throwaway>` (the check above); computes the person's claims; issues a one-use code (ten minutes, in memory) bound to the `rid`; answers `{"redirect": "<redirect_uri>?code=…&state=…"}`. CORS: `MATE_APP_URL` only |
| `POST /oidc/token` | `client_secret_basic` or `_post`; `grant_type=authorization_code`; answers `access_token` (opaque, five minutes), `id_token` (ES256; `iss`, `aud=gitea`, `sub`, `nonce`, `email`, `name`, `preferred_username`, `groups`), `token_type=Bearer`, `expires_in` |
| `GET /oidc/userinfo` | `Authorization: Bearer <access_token>` → the same claims |

Claims come from the role function on the caller's live roles; `sub` is the Zerops user id;
`preferred_username` per `docs/vocabulary.md`. Gitea's source is added at import with
`--admin-group org:owner`; teams are written by the rights loop, which runs a pass a few seconds
after every token issued, so a person is in the right teams by the time they see the page.

A restart forgets requests, codes and access tokens; the person signs in again.

## What the broker never does

Takes a Zerops key from a caller · executes repository code (it moves archives from Gitea to
Zerops; Zerops builds) · reads a sibling's variables from the container (the two Gitea secrets it
needs arrive as explicit `${web_…}` references) · trusts the private network · starts a pass
because something a job can reach asked it to (there is no poke endpoint; the timer and the signed
webhooks are the only triggers).
