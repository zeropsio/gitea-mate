# The broker's HTTP API

Five endpoints and an OIDC provider. **No endpoint takes a Zerops key**, and being inside the
project proves nothing — the runners share its network — so every call is checked against the party
it claims to be. JSON in and out; errors are `{"error": "<code>", "message": "<plain words>"}`.

A Mate's own Gitea access is not an endpoint at all: the rights loop delivers it (below, *A Mate's
Gitea access*).

Names used below are in `docs/vocabulary.md`; the rights function in `docs/roles.md`.

## Proving a person: the throwaway check

`POST /oidc/complete` takes a **throwaway Zerops integration token** the Mate app minted as the
person, `Authorization: Bearer <token>`. The broker accepts it only when
every one of these holds, in this order, and refuses with `throwaway_invalid` plus a `reason`
otherwise:

1. `GET /user/info` as the token answers — its `id` is the token's own id (`token_dead` otherwise).
2. `GET /client/{org}/integration-token/{id}` as the token answers `200`, where `org` is **the
   receiver's own org** — the broker's `MATE_ZEROPS_CLIENT_ID`, a Mate's project read with its own key —
   never anything the presented token said about itself (`wrong_org` otherwise — a token from
   another org cannot read this org's tokens).
3. `roleCode == NO_ACCESS`, `projects` empty, and every flag false: `canCreateProjects`,
   `canManageFinances`, `canManageFinance`, `hasFinances` — the spellings both implementations
   refuse (`has_rights`).
4. `name` starts with `gitea-signin:{host}:` where `host` is this broker's Gitea host
   (`GITEA_PUBLIC_URL` without scheme) (`wrong_name`).
5. `created` is within five minutes of the **Zerops API's `Date` response header** — never the
   container's clock (`stale`).
6. `createdByUser` is an `ACTIVE` member of the org, read with the broker's own token
   (`not_member`).

The caller is `createdByUser`. The app deletes the throwaway right after the call; the broker
never stores it.

## A Mate's Gitea access — delivered by the rights loop (guide 1.5)

Nobody asks for it. The registry says which projects are Mates (`mate:gm:{group}:{project}:mate`,
written by the org's owner), and the loop makes every registered Mate's access true on each pass,
the way it makes teams and bots true:

1. **The bot** — `mate-{projectId}`, restricted, in its group's `read` team (as today).
2. **A live token** — when the bot has no token named `mate/{bot}/{n}`, or the token the Mate's
   container holds is not the bot's newest generation (compared through Gitea's `token_last_eight`
   against the value the platform returns in clear), the loop mints generation *n+1* (scopes
   `write:repository,read:user`). Without the second clause a crash between mint and write would
   leave the container on generation *n* while the grace rule revokes it ten minutes later.
3. **The Mate's environment** — with the broker's Zerops token, which the app granted `BASIC_USER`
   on the Mate's project when it registered it, the loop finds the project's `zcp@1` service and
   writes three service variables on it: `GITEA_URL` and `MATE_BROKER_URL` (plain) and
   `GITEA_TOKEN` (sensitive). A variable already holding the right value is not written; a
   `GITEA_URL` naming another Gitea means the token there is not ours, so the loop mints a new
   generation and writes all three; a stale `MATE_BROKER_URL` alone is put right as a plain write. The loop never restarts the container: zcp reads these from the
   container's live env store, which the platform rewrites within seconds of the write (measured
   2026-09-16 and 2026-09-17).

Ordering makes this safe at sign-up. The registry entry is written before the Mate's project has a
container, and a write onto a service that is still `NEW` or `READY_TO_DEPLOY` is accepted and
present once it is `ACTIVE` (measured 2026-09-17), so the variables are usually there before zcp
first looks. When they are not — the account's first project, where this broker is itself still
building — zcp waits with backoff and the loop catches up on its first pass. A Mate registered
later (an older project tagged into a group) is served on the next pass the same way.

What the loop cannot do it reports and retries: a Mate project the broker's token does not reach
(the app has not granted it yet), a project with no `zcp@1` service (nothing to write to), a
platform refusal. None of these stops the pass for the other Mates.

Rotation — a compromised Mate, a leaver — is the same mechanism: the loop mints generation *n+1*
and writes it; older generations are revoked by the loop only once the newest is ten minutes old,
never on the same pass, so a crash between mint and write leaves two live tokens for ten minutes
and never a dead Mate.

## `POST /mate/repository` — a service repository for a Mate (guide 1.5, 2.1)

Caller: a Mate (zcp), `Authorization: token <its bot's Gitea token>`.

```json
{ "name": "api" }
```

The broker resolves the token against Gitea (`GET /api/v1/user`): the login must be a
`mate-{projectId}` bot (`not_a_bot`) whose project is registered (`not_registered`); the org is the
project's group. It creates `{org}/{name}` — private, `auto_init` with default branch `main`,
`main` protected (no direct push, merge by the `write` team) — and adds the bot as a collaborator
with write. An existing service repository of the group answers `200` with `created: false`, and a
bot that does not collaborate on it yet is made a collaborator with write first: that is how a
group's second Mate joins its app, since the recipe's `buildFromGit` names the same repository for
every Mate it creates (D24). `group` — the group repository — answers `409 taken`, made yet or not:
a Mate's recipe reaches it only as a pull request from its bot's fork (D23).

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
(`404 unknown_environment`); `service` a service of it (`404 unknown_service`). `not_a_job` is `401`,
`wrong_repository` `403`. **The caller picks the
environment, never a commit or a ref:** a stage deploys the head of its source ref; production the
commits the newest tag whose `mate/release/{tag}` status is `success` lists. The request is queued per environment,
newest wins.

`202`:

```json
{ "id": "d_8f2a…", "environment": "stage", "service": "api", "sha": "3f9c…", "status": "queued" }
```

`GET /deploy/{id}` (same authentication, the same job or any job of the same repository):

```json
{ "id": "d_8f2a…", "status": "running", "sha": "3f9c…", "versionId": "…", "message": "" }
```

`status` is `queued`, `running`, `active` or `failed`. Both answers are the same record — `environment`
and `service` are always present, `versionId` appears once there is one. A request superseded by a
newer one for the same environment keeps its record and follows the newer job's outcome. The broker
keeps deploys in memory: after a restart an id it no longer holds is `404 unknown_deploy`, and the action then reads the commit status
`mate/deploy/{environment}/{service}` on the sha instead — the same result, written by the broker
on every outcome.

## `POST /hooks/gitea` — Gitea's webhooks (guide 1.6, 5.3, 5.5)

Caller: Gitea. `X-Gitea-Signature` is the hex HMAC-SHA256 of the raw body with
`GITEA_WEBHOOK_SECRET`; anything else is `401 bad_signature`. `X-Gitea-Event` selects:

| Event | What the broker does |
|---|---|
| `push` to a source branch of an environment | deploys the environment (per 5.3) |
| `push` to `env/*` | nothing — the broker wrote it |
| `create` of a tag `v*` on the group repo | re-checks the pusher's production rights in Zerops, writes `mate/release/{tag}` `success` (approved) or `failure` (refused) on the tagged commit, deploys an approved tag's commits |
| `pull_request` merged on the group repo | imports the recipe delta into each environment built from it, re-reads environments |
| `pull_request` opened on the group repo by a Mate's bot | nudges the rights loop, whose next pass merges it — a Mate's recipe proposal lands by itself (D23) |
| `workflow_job` `queued` | starts the group's runner service if it is stopped |
| `workflow_job` `completed` | notes the time; a quiet spell (default 15 min) stops the runner |
| anything else | `204`, ignored |

Always `204` once the signature is good, whatever the payload; the work runs after the response.

## `POST /person/token` — a person's own Gitea access, for the app (guide 4.4)

Caller: the Mate app, from the browser, as the person — proved by a `gitea-signin` throwaway
exactly as *Proving a person* above (`Authorization: Bearer <throwaway>`, no body). The app
drives Gitea as the person; this is how it gets the token to do that without a single Gitea
screen: the same proof it makes at a Mate's door.

What the broker does, in order: checks the throwaway (`401 throwaway_invalid` with a `reason`
otherwise); reads the person's rights live and refuses anyone who is not an active member
(`403 not_a_member`); makes sure the person's Gitea account exists — `u-{id}` per
`docs/vocabulary.md`, bound to the OIDC source (`source_id` = `GITEA_OIDC_SOURCE_ID`,
`login_name` = the Zerops user id, no password), so a later *Sign in with Zerops* on Gitea's own
pages lands on the same account; when it had to create the account, runs one pass of the rights
loop before answering, so the person is in their teams by the app's first read; mints a token for
them with the site admin's basic auth (Gitea's token routes take nothing else — measured), named
`mate-app/{unix nanoseconds}`, scopes `read:user read:organization write:repository write:issue`.

```json
{ "token": "…", "login": "u-…", "expiresIn": 43200 }
```

The token is the person's: Gitea enforces the mirrored rights on every call, and the app can widen
nothing by holding it. Gitea gives a token no expiry, so the rights loop retires every `mate-app/*`
token older than `APP_TOKEN_TTL` (12 h); the app keeps the value in memory for the tab's life and
mints again on the first `401`. `502 upstream` when Zerops cannot be reached, `502 gitea` when
Gitea cannot; `424 gitea_refused` with Gitea's own words when Gitea said no — a login source that
does not exist, a name it will not take — so the app shows the reason once instead of retrying.
(The platform's edge replaces an upstream `502` with its own HTML page, measured 2026-09-17, so a
`502` carries no words of the broker's; a refusal must not be one.) CORS: the `POST` and its preflight are answered for every origin (`*`), as is Gitea's
own API — the throwaway in the header is the proof and no cookie is involved, so the app drives
both from mate.zerops.io, a developer's localhost or a shell alike (D22).

Measured on Gitea 1.27.2 (2026-09-17, the lab): an account with no source needs a password
(`400 PasswordIsRequired`); one created with `source_id` and `login_name` needs none and is active;
the site admin's basic auth mints it a token; that token answers `GET /user` as the person and sees
only what the person may; a token name a user already holds is refused (`400`), and a token is
deleted by name.

## OIDC provider — Gitea's *Sign in with Zerops* (guide 3.6)

Issuer `BROKER_PUBLIC_URL`. One client, `client_id` `gitea`, `client_secret` `OIDC_CLIENT_SECRET`,
one allowed `redirect_uri`: `{GITEA_PUBLIC_URL}/user/oauth2/zerops/callback`.

| Route | |
|---|---|
| `GET /.well-known/openid-configuration` | discovery: `code` flow, `ES256`, scopes `openid email profile groups`, claims `sub email name preferred_username groups` |
| `GET /oidc/jwks` | the ES256 key derived from `OIDC_SEED` (`kid` = the first 8 hex of its SHA-256) |
| `GET /oidc/authorize` | validates `client_id`, `redirect_uri`, `response_type=code`; stores `{redirect_uri, state, nonce, scope}` under a random `rid` for ten minutes; `302` to `{MATE_APP_URL}/gitea-signin?rid={rid}&broker={BROKER_PUBLIC_URL}` |
| `POST /oidc/complete` | body `{"rid": "…"}`, `Authorization: Bearer <gitea-signin throwaway>` (the check above); computes the person's claims; issues a one-use code (ten minutes, in memory) bound to the `rid`; answers `{"redirect": "<redirect_uri>?code=…&state=…"}`, which the app follows with a full-page navigation. CORS: the `POST` and its preflight are answered for `MATE_APP_URL` only |
| `POST /oidc/token` | `client_secret_basic` or `_post`; `grant_type=authorization_code`; answers `access_token` (opaque, five minutes), `id_token` (ES256; `iss`, `aud=gitea`, `sub`, `nonce`, `email`, `name`, `preferred_username`, `groups`), `token_type=Bearer`, `expires_in` |
| `GET /oidc/userinfo` | `Authorization: Bearer <access_token>` → the same claims |

Claims come from the role function on the caller's live roles; `sub` is the Zerops user id;
`preferred_username` per `docs/vocabulary.md`. Gitea's source is added at import with
`--admin-group org:owner`; teams are written by the rights loop, which runs a pass a few seconds
after every token issued, so a person is in the right teams by the time they see the page.

A restart forgets requests, codes and access tokens; the person signs in again.

## What the broker never does

Takes a Zerops key from a caller · executes repository code (it moves archives from Gitea to
Zerops; Zerops builds; the one `git` it runs merges refs into `env/*` in a throwaway directory with
`core.hooksPath=/dev/null`) · reads a sibling's variables from the container (the two Gitea secrets it
needs arrive as explicit `${web_…}` references) · trusts the private network · starts a pass
because something a job can reach asked it to (there is no poke endpoint; the timer and the signed
webhooks are the only triggers).
