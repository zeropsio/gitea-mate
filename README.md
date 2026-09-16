# gitea-mate

Two halves of one thing: **Gitea on Zerops**, and the **broker** that makes a Zerops account's roles
the only source of rights inside it.

- **The broker** — a small stateless Go service (`cmd/broker`). It mirrors Zerops permissions into
  Gitea on a timer, signs people in to Gitea as an OIDC provider, hands each Mate its own Gitea bot
  token, imports and wakes a group's Actions runner, and deploys what protected branches and tags
  allow: a stage follows the head of its source branches, production the commits of the newest
  release tag whose pusher it approved. It holds the account's only deploy key, and it executes no
  repository code — it moves a Gitea commit archive to Zerops and Zerops builds. It has no
  database, cache or queue of its own: everything it must know already lives somewhere
  authoritative (the registry tags, the group repo's `main`, the commit statuses it writes and the
  commit sha in each app version's name), so a restart is always safe and the next pass catches up
  whatever a webhook could not deliver.
- **Gitea's deployment** — `zerops.yaml` with three setups (`gitea`, `broker`, `runner`), Gitea's
  `app.ini` and init scripts under `gitea/`, and the import files the Mate app and the broker send
  under `import/`.

## The contracts

Decided elsewhere, read here; none of this repository invents a name or a rule of its own.

| What | Where |
| --- | --- |
| The broker's HTTP API, every refusal code | [`docs/broker-api.md`](docs/broker-api.md) |
| Every name three codebases agree on — tags, logins, teams, variables | [`docs/vocabulary.md`](docs/vocabulary.md) |
| The role function: Zerops roles → Gitea teams, Mate scopes, OIDC claims | [`docs/roles.md`](docs/roles.md) |
| Its fixtures, byte-identical with the Mate fork's copy | [`internal/roles/fixtures.json`](internal/roles/fixtures.json) |

## Layout

```
cmd/broker          the binary
internal/config     every variable of docs/vocabulary.md § "The broker's environment"
internal/roles      the Go twin of the role function
internal/zerops     the Zerops REST client the broker needs
internal/throwaway  the six-step check that proves a person
internal/gitea      the Gitea admin client
internal/registry   the group registry, parsed from the Gitea project's tags
internal/mirror     the rights loop: plan Gitea writes, then apply them
internal/environments the group repo's environments.yaml and its tiers
internal/deploy     the decision, the per-environment queue and the executor
internal/pipeline   the webhooks and the catch-up pass that drive them
internal/oidc       the ES256 OIDC provider Gitea signs people in through
internal/server     the routes
gitea/              app.ini and the init scripts that run in the container
import/             the service imports: the Gitea project, a group's runner
actions/deploy      the composite action a workflow calls to deploy
```

## Running the broker locally

Every variable in `docs/vocabulary.md` § "The broker's environment" is required; a missing one is
named in the start-up error and no value is ever logged.

```sh
export ZEROPS_TOKEN=…            ZEROPS_API_URL=https://api.app-prg1.zerops.io
export ZEROPS_CLIENT_ID=…        ZEROPS_PROJECT_ID=…
export GITEA_URL=http://127.0.0.1:3301   GITEA_PUBLIC_URL=http://127.0.0.1:3301
export GITEA_ADMIN_TOKEN=…       GITEA_ADMIN_PASSWORD=…    GITEA_WEBHOOK_SECRET=…
export OIDC_CLIENT_SECRET=…      OIDC_SEED=…
export BROKER_PUBLIC_URL=http://127.0.0.1:8080
export MATE_APP_URL=http://127.0.0.1:5173
go run ./cmd/broker
```

Optional tunables: `LISTEN_ADDR` (`:8080`), `GITEA_ADMIN_USERNAME` (`admin`), `MIRROR_INTERVAL`
(`3m`), `MIRROR_CAP` (`10`), `RUNNER_QUIET_PERIOD` (`15m`).

`GITEA_ADMIN_PASSWORD` is not in `docs/vocabulary.md`'s table but guide 1.3 names it: Gitea's token
routes (`/users/{login}/tokens`) answer `401 auth required` to an API token, however privileged
(measured on 1.27.2), so minting a Mate bot's credential needs the site admin's basic auth.

## Gitea on Zerops

`zerops.yaml` carries three setups. `gitea` fetches the pinned Gitea release in the **build** step
and verifies it against a `sha256` written beside the version, so the runtime has no
network-dependent prepare and a container that restarts at three in the morning does not depend on
`dl.gitea.com`; `broker` is a static `go build` of `cmd/broker`; `runner` is `act_runner`, fetched
and checksum-verified the same way, registered to one group's Gitea org and carrying no Zerops
credential at all.

`import/gitea-project.yaml` is the services-only import the Mate app sends — `db`, `volume`, `web`
and `broker` — and `import/runner.yaml` the one the broker sends when a group's first workflow
appears. Both document their placeholders at the top. The project keeps the platform's default
`envIsolation`, which is what stops a runner job reading Gitea's admin token.

`actions/deploy/action.yml` is the composite action a workflow calls instead of holding a deploy
credential: it asks the broker, polls, and falls back to the commit status if the broker forgot the
deploy across a restart. `import/runner.yaml` is also embedded into the binary (`embed.go`), so the
broker imports a runner with no file to find at run time and the document keeps one home.

## Tests

```sh
make test      # go test ./...
make lint      # gofmt -l, go vet
make build     # go build ./cmd/broker
```

The merge of a multi-source stage runs real `git` in a temporary directory; those tests skip
themselves when `git` is not on the path.

`internal/gitea` also carries an integration test against a real Gitea. It is skipped unless both
variables are set, and the token reaches it only through the environment — never a file, a fixture
or a commit:

```sh
GITEA_LAB_URL=http://127.0.0.1:3301 GITEA_LAB_ADMIN_TOKEN=… \
GITEA_LAB_ADMIN_USER=… GITEA_LAB_ADMIN_PASSWORD=… make lab-test
```

The two basic-auth variables are needed only by the sub-tests that mint a token; without them those
skip and say why.
