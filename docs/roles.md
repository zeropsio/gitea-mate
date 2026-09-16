# The role function

One pure function, implemented twice — Go in `internal/roles` here, TypeScript in the Mate fork's
`packages/shared/src/zeropsRoles.ts` — and one fixture file, `internal/roles/fixtures.json`, copied
byte-for-byte into the fork as `packages/shared/src/zeropsRoles.fixtures.json`. Each side's tests
replay every case; a change to the rules is a change to the fixture first, in both repositories.

Zerops roles are the only source of rights. The Mate app, each Mate's door and the broker all call
this function on the same inputs and never invent a rule of their own.

## Input

| Field | From |
|---|---|
| `person.id`, `orgRole`, `status`, `canCreateProjects` | the org's member list (`GET /client/{org}/user/list`) |
| `overrides` — `{ projectId: role }` | each project's `userRoles` (a per-project override for this person) |
| `registry` — groups with `id`, `slug` and their projects with `kind` `mate` / `stage` / `production` | the registry tags on the org's Gitea project (`docs/vocabulary.md`) |

Roles rank `NO_ACCESS` < `READ_ONLY` < `BASIC_USER` < `ADMIN` < `OWNER`. A person's **effective role**
on a project is its override for them when there is one, their org role otherwise.

## Rules

- `active` — `status == ACTIVE`. An inactive person gets nothing: no claims, every Mate hidden,
  every group flag false, no site admin.
- `siteAdmin` — active and org `OWNER` (D10: admins get teams by role, never site admin).
- `canCreate` — active and (org `ADMIN`/`OWNER` or `canCreateProjects`). The app's *Add Mate* gate.
- Per group `g`:
  - `write` — the effective role on **any** of the group's projects is `BASIC_USER` or above. The
    creator of a Mate is its `OWNER`; a releaser holds `BASIC_USER` on production; an org
    `BASIC_USER`/`ADMIN`/`OWNER` holds it everywhere.
  - `read` — `write`, or an org role of `READ_ONLY` or above.
  - `release` — when the group has a production project: effective role on it `BASIC_USER` or
    above; until it has one: org `ADMIN` or `OWNER` (the recipe is merged before production
    exists).
- Per Mate project `p`: `open` when the effective role is `BASIC_USER` or above (its owner, org
  owners and admins, org basic users); `listed` when it is `READ_ONLY` (D5: seen in the list, the
  door refuses with `zerops_read_only`); `hidden` when `NO_ACCESS`.
- `claims` — the OIDC `groups` claim, sorted: `org:owner` when `siteAdmin`, and per group
  `g:{slug}:read`, `g:{slug}:write`, `g:{slug}:release` for each flag that is true.

## Output

```json
{
  "active": true,
  "siteAdmin": false,
  "canCreate": true,
  "claims": ["g:acme:read", "g:acme:write"],
  "mates": { "p-fen": "open", "p-nova": "listed" },
  "groups": { "acme": { "read": true, "write": true, "release": false } }
}
```

`mates` names every Mate-kind project in the registry, `groups` every group, so a consumer never
has to know which projects exist to read the answer. What each consumer does with it:

| Consumer | Uses |
|---|---|
| a Mate's door (fork, server) | `mates[own project]` → `open` = the standard client scopes plus `exec:operate`; `listed` = refuse `zerops_read_only`; `hidden` = refuse `zerops_project_membership_required` |
| the Mate app (fork, client) | `mates` for the list's row state, `canCreate` for *Add Mate*, `groups[g].release` for *Release* |
| the broker's rights mirror | `siteAdmin`, `groups[g]` → Gitea teams `read`/`write`/`release`, `active` → disable and delete tokens |
| the broker's sign-in | `claims` |
