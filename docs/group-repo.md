# The group repo — `{slug}/group`

One repository per group, made by the broker's rights loop when the group is registered. It holds
the recipe (every tier's import), which Zerops project each environment is and what feeds it, and
the release tags. `main` is protected: no direct push for anyone, merges by anyone with write —
the `write` and `release` teams, and every registered Mate of the group, whose bot the rights loop
makes a collaborator with write here (D31, 2026-09-30: the first Mate stands the recipe up, and
every later one may change it) — and a Mate's recipe pull request that only adds files is merged by the broker on arrival (D23; until 2026-09-17 the `release` team alone
merged, and every Mate's recipe waited for a releaser). One that modifies, removes or renames a file
`main` carries waits for a person with write, who merges it or asks a Mate to, and each pass reports
it: on 2026-09-26 a second Mate's re-proposal, merged by the broker, replaced the hand-written tiers
and the next release built production with the dev setup. A recipe request Gitea calls empty — its branch carries nothing `main` lacks, so Gitea
answers every merge `405` — is closed by the broker rather than retried every pass (measured
2026-09-17: a Mate re-proposed a recipe `main` already had). `env/*` is written by the broker
alone; tags `v*` are created by the `release` team alone.

## Layout

```
README.md                        the recipe's root README — zcp's published layout
0 — AI Agent/import.yaml         the Mate tier: a dev/stage pair per codebase and the managed services
0 — AI Agent/README.md
3 — Stage/import.yaml            the stage tier (the production transform without HA)
3 — Stage/README.md
4 — Small Production/import.yaml the production tier: HA, minContainers ≥ 2, startWithoutCode
4 — Small Production/README.md
environments.yaml                the environments — see below
.gitea/workflows/release.yml     optional: the release workflow, orchestrating through the deploy action
```

The tier directories are exactly what `zcp/internal/recipe/layout.go` emits (the em dash is part of
the name). A Mate proposes the tiers `main` lacks by pull request (guide 2.2), and the broker merges
such a proposal by itself (D23, D30); a change to a tier `main` has is a pull request too, merged by a
person with write or by a Mate its person asks (D31). Every runtime service in a tier's `import.yaml` names its code repository
in `buildFromGit` (the canonical clone URL the broker returned, `{slug}/{name}` on this Gitea) and
its setup in `zeropsSetup`; that pair is how the broker and the app map a service hostname to a
repository. Nothing in the group repo is executed by anyone.

## `environments.yaml`

Written by the app at *Add stage* / *Add production* (as the person — a direct commit for someone
with merge rights, a pull request otherwise), read by the broker on every pass.

```yaml
version: 1
environments:
  stage:                    # the name a workflow's deploy step asks for
    tier: stage             # which tier's import.yaml created it: stage | production
    project: m1VrZPJlSnAmnYAvfuEZEg   # the Zerops project (also tagged mate:gm:… in the registry)
    sources: [main]         # branches of every service repository that feed it
    deploy: on-push         # on-push (default) or on-request — only when a workflow asks
  stage-client-x:
    tier: stage
    project: …
    sources: [main, feature/invoices]   # several: the broker merges them into env/stage-client-x
  production:
    tier: production
    project: …
    sources: release        # the newest approved v* tag of this repository, nothing else
    gates:
      requireOnStage: stage # optional: every listed commit must already be live on this stage;
                            # a stage the broker cannot read counts as not met
```

Rules the broker enforces (guide 5.1, 5.3):

- A stage deploys the head of its source branch. With several sources the broker rebuilds
  `env/{name}` in a throwaway working copy by merging them in order whenever one moves; a conflict
  leaves the environment on its last good merge and reports it as a commit status on the head of
  the first source.
- Production deploys only what an approved release tag lists (below). `sources: release` is the
  only value a production environment may have.
- Only environments in this file exist to the broker; a `POST /deploy/grant` for any other name is
  `unknown_environment`. The `service` must be a runtime service of the environment's tier.
- The broker's Zerops token holds `BASIC_USER` on each `project` here — the app adds the grant
  when it adds the environment. An environment whose project the broker cannot reach is reported,
  never guessed at.

## Release tags

A release is an annotated tag `v{semver}` on this repository, created **as the person** through
the Mate app (Gitea's tag protection lets only the `release` team create `v*`). Its target is the
`main` commit whose recipe production should run; its message lists each runtime service's commit:

```
api 3f9c1b2e5d7a4c6f8e0b1d2a3c4f5e6d7a8b9c0d
web 77ab0e1f2d3c4b5a69788796a5b4c3d2e1f0a9b8
```

One line per service, `{service hostname} {full 40-hex sha}`; blank lines are tolerated, anything
else — a short sha included, which would never compare equal to a version's name and so redeploy for
ever — makes the tag unparseable and it is refused. A rollback is a new tag
listing an earlier tag's commits — a tag name is never reused, and a grant is given for the commit
protected state wants, never for one a job picks.

When the tag's webhook arrives the broker re-checks the pusher's production rights in Zerops
(the mirror lags a role change by minutes, so Gitea's team is not the last word) and writes a
commit status on the tagged commit: context `mate/release/{tag}`, state `success` (approved) or
`failure` (refused), description naming the pusher. A refused tag stays in Gitea as a record and
deploys nothing, ever — a restart, a catch-up pass and a later `POST /deploy/grant {production}` all
read the statuses, never the tag list. "Newest" among approved tags is by the tag's tagger date
(`GET /repos/{o}/{r}/git/tags/{sha}`), ties broken by semver.

D8: a tag pushed by a Mate's bot is approved only when the group's switch is on
(`mate:release:{groupId}:mates` in the registry); a person's tag is checked as the person, switch
or no switch.

## What is deployed, and how the broker names it

A job deploys, with `zcli push` (D27); the broker tells it what to call the version. Every Zerops
app version is named by what a person reads it by and the short sha of the commit it was built from:
a stage's by `{branch} {short sha}` ("main 7e2d4c1"), production's by `{tag} {short sha}` ("v0.1.0
7e2d4c1"); the short sha is its first seven hex characters, and who tagged a release is the
release's to say, not the name's. Branches and tags hold no space, so a name is always two tokens.
Names written before 2026-09-30 still stand on services and read the same: a bare full sha (stage)
or `{sha} {tag} {tagger login}` (production) — one token or three and more, the sha first. zcp names
its own pushes the same way, `{branch} {short sha}`, and a push of a working tree with uncommitted
changes `{branch} {short sha}-dirty`, which was built from no commit. Any other name was typed by
hand and names no commit. The catch-up pass compares each environment's desired head with the sha in
the deployed version's name — the same commit when equal, or when the name's sha is a hex prefix of
at least seven characters — and starts a job for the difference. Production is built from the
release's commits — nothing is promoted from a stage. Deploy outcomes are commit statuses on the
service repository's commit: context `mate/deploy/{environment}/{service}`, state `pending`
(`dispatched`, then `deploying · job {id}` once a job holds the key) / `success` (`live`) /
`failure` (the job's or the broker's reason).

## Recipe deltas

When `main` changes a tier's `import.yaml`, the next pass imports into every environment built from
that tier what is missing — new services, with `buildFromGit` + `zeropsSetup` converted to
`startWithoutCode: true` (the platform cannot clone a private repository) — before deploying there.
Changed scaling on an existing service is reported, not applied — a re-import with `override`
restarts the service and its semantics are unmeasured; a service that disappeared from the recipe is
reported, never deleted by the broker. A group removed from the registry loses its runner service.
