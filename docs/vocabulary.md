# Names, tags and variables

Every name three codebases have to agree on. The Mate fork, zcp and this repository read this file;
none of them invents a name it does not list.

## Registry — tags on the org's Gitea project

Written only by org `OWNER`/`ADMIN` (the platform enforces it), read by the app and the broker.
`PUT /project/{id}` with `name`, `description`, `tagList`, `publicIpV4Shared`, `maxCreditLimit` —
never `userRoles`: the call **replaces** `userRoles` when it is sent, so every tag write omits it
and *Assign* (a role override) is the one write that carries it. Budget: 65 534 bytes of compact JSON per project; entries stay short.

| Tag | Meaning |
|---|---|
| `mate:gn:{groupId}:{slug}` | a group exists; `slug` is its Gitea org name |
| `mate:gm:{groupId}:{projectId}:{kind}` | a project belongs to the group as `mate`, `stage` or `production` (one production per group) |
| `mate:leaving:{userId}` | a member is being removed; the projects-screen reconcile finishes it |
| `mate:release:{groupId}:mates` | D8's switch: the group's Mate bots may release |

`groupId`: the app's existing group id (today's `mate:g:{id}` on member projects stays as a display
hint). `slug`: `^[a-z][a-z0-9-]{1,29}$`, unique in the account, derived from the group's name by the
app at registration — on a collision the app numbers it (`acme-2`, `acme-3`) — and never changed
(it is the Gitea org).

## Tags on a Mate's project (unchanged from today, listed for completeness)

`mate:g:{groupId}`, `mate:role:{dev|stage|prod}`, `mate:bot:{name}`, `mate:name:{name}`,
`mate:tool:gitea` (the Gitea project itself), and D6's `mate:signer:{agent}:{userId}`.

## Gitea

| Thing | Name |
|---|---|
| org per group | the group's `slug` |
| the group repo (recipe, environments, release tags) | `{slug}/group` |
| a service repo | `{slug}/{name}`, `name` `^[a-z][a-z0-9-]{0,39}$` |
| teams per org | `read`, `write`, `release` (units: all repositories; permissions read / write / write + tag protection) |
| a Mate's bot user | `mate-{projectId}` (login), full name the Mate's name; restricted, `max_repo_creation 0`, no org creation |
| a bot's token | `mate/{bot}/{generation}`, generation from 1; scopes `write:repository,read:user` |
| a person's login | `u-` + the Zerops user id, lower-cased, every character outside `[a-z0-9]` dropped (deterministic, valid for Gitea, never an e-mail's local part); full name from Zerops |
| the site admin | `admin` (never `mate`) |
| the OIDC source | `zerops` (callback `{ROOT_URL}/user/oauth2/zerops/callback`) |
| protected branches | `main` on every repository: no direct push for anyone, merge by the `write` team; `env/*` on the group repo: the broker only; tags `v*` on the group repo: the `release` team. `main` is born with an initial commit, so a Mate's branch must descend from it (zcp fetches `main` before it branches) — an unrelated history merges only by rebase (measured 2026-09-16). The API merge is refused to the site admin when a merge whitelist is set; only a member of the whitelisted team merges |
| commit statuses the broker writes | `mate/release/{tag}` on the tagged group-repo commit (`success` = approved, `failure` = refused — per tag, since several tags may point at one commit), `mate/deploy/{environment}/{service}` on the service repo's commit (`pending` / `success` / `failure`, description = the Zerops app version id) |
| the group repo's files | `environments.yaml` and the tier directories — `docs/group-repo.md` |
| runner per group | registered at org scope, labels `ubuntu-latest:host,ubuntu-26.04:host`, named after its container hostname |
| webhooks | one org hook per group, made by the broker, events `push`, `create`, `delete`, `pull_request`, `workflow_job`, `workflow_run`; secret `GITEA_WEBHOOK_SECRET` |

## Zerops

| Thing | Name |
|---|---|
| the Gitea project | tagged `mate:tool:gitea`; services `db`, `volume`, `web`, `broker`, and `runner{slugcompact}` per group |
| a runner service's hostname | `runner` + the slug with `-` removed, cut to 25 characters (Zerops hostnames are `[a-z0-9]`, 25 max) |
| the broker's token | `mate-broker`: org `READ_ONLY`, `BASIC_USER` on the Gitea project, `BASIC_USER` on each group stage and production as they are created |
| a Mate's token | `zcp-{project}` (the platform's), lowered to `NO_ACCESS` + `BASIC_USER` on its project; replacements `zcp-{project}/{generation}` |
| a door throwaway | `mate-door:{projectId}:{nonce}` — `NO_ACCESS`, no grants, no flags |
| a Gitea throwaway | `gitea-signin:{gitea host}:{nonce}` — the same shape; used for sign-in consent and for `POST /mate/credential` |
| an app version | named by the full commit sha; production's `{sha} {tag} {tagger login}` — the sha is always the first token |

## Gitea's environment that the app fills in (service `web`, plain)

| Variable | Holds |
|---|---|
| `GITEA_DOMAIN` | `web-${zeropsSubdomainHost}-3000.{region}.zerops.app` — Gitea derives `ROOT_URL` from it |
| `GITEA_CORS_ALLOW_DOMAIN` | every origin the Mate app runs from, comma-separated, spelled literally with the port (`[cors] ALLOW_DOMAIN` matches strings) |
| `BROKER_PUBLIC_URL` | the broker's public origin — the OIDC issuer `admin-init.sh` registers |
| `OIDC_CLIENT_SECRET` | reference `${broker_OIDC_CLIENT_SECRET}` |

## The broker's environment (its own service, sensitive unless noted)

The platform refuses any imported variable whose name starts with `ZEROPS_`, case-insensitively
(`400 userDataZeropsPrefixForbidden`, measured 2026-09-16) — hence the `MATE_` prefix on the four
Zerops ones.

| Variable | Set by | Holds |
|---|---|---|
| `MATE_ZEROPS_TOKEN` | the app, at import | the broker's Zerops token |
| `MATE_ZEROPS_API_URL` | import (plain) | `https://api.app-prg1.zerops.io` (the region's API) |
| `MATE_ZEROPS_CLIENT_ID` | the app, at import (plain) | the org id |
| `MATE_ZEROPS_PROJECT_ID` | the app, at import (plain) | the Gitea project's id — where the registry lives |
| `GITEA_URL` | import (plain) | `http://web:3000` |
| `GITEA_PUBLIC_URL` | import (plain) | `https://{GITEA_DOMAIN}` |
| `GITEA_ADMIN_TOKEN` | reference `${web_GITEA_ADMIN_TOKEN}` | Gitea's site-admin API token |
| `GITEA_WEBHOOK_SECRET` | import preprocessor | HMAC secret the broker puts on the hooks it creates |
| `OIDC_CLIENT_SECRET` | import preprocessor | Gitea's client secret for the `zerops` source (`web` references it as `${broker_OIDC_CLIENT_SECRET}`) |
| `OIDC_SEED` | import preprocessor | 64 random characters; the ES256 signing key is derived from it deterministically |
| `BROKER_PUBLIC_URL` | import (plain) | `https://{BROKER_DOMAIN}` — the OIDC issuer |
| `MATE_APP_URL` | the app, at import (plain) | the web app's origin; the sign-in consent page lives there |
| `MATE_APP_ORIGINS` | the app, at import (plain) | every origin the app runs from, comma-separated — the same list `web` gets as `GITEA_CORS_ALLOW_DOMAIN`. The app's OAuth2 client is registered for `{origin}/gitea/callback` of each, and `GET /gitea/oauth-client` answers them CORS. `MATE_APP_URL` is one of them whether or not the value names it, so an unset variable is that origin alone |
| `LISTEN_ADDR` | import (plain) | `:8080` |

## A Mate's environment (service `zcp`, written by the app — `giteaCredential.ts`)

`GITEA_URL` (plain, Gitea's public origin), `MATE_BROKER_URL` (plain, the broker's public origin —
where zcp asks for repositories), `GITEA_TOKEN` (sensitive, the bot's token), and `ZCP_API_KEY`
moved here from the project as a sensitive service variable (guide 0.10). zcp reads all four from
its process environment and nothing else; at sign-up the first three can land minutes after the
Mate is up, so zcp waits for them with backoff. The old `GITEA_REPO` is gone: a Mate asks the
broker for its repositories and holds as many as it has dev pairs.
