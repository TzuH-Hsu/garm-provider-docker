# Upstream provider-list submission kit

Everything a maintainer needs to open a pull request against
[`cloudbase/garm`](https://github.com/cloudbase/garm) adding this project to
both of its `## Supported providers` tables — the root `README.md` and
`doc/providers.md`. **Nothing here has been sent upstream** — this is
preparation only; submitting the actual PR is a separate, deliberate step
(see the exact commands in the checklist below).

Research for this refresh was done 2026-09-26 against `cloudbase/garm`
`main` at commit `20a0c4b` (a fresh shallow clone) plus `gh api`/`gh pr`
queries against the live repository and this repo's own `v0.2.0` GitHub
Release.

## (a) Proposed table rows

`cloudbase/garm` maintains **two** provider tables, kept in sync with each
other, both sorted alphabetically by provider display name and both
currently listing the same 10 providers, unchanged in count and order
since this kit was first drafted: Akamai/Linode, Amazon EC2, Azure,
CloudStack, GCP, Incus, Kubernetes, LXD, OpenStack, Oracle OCI. "Docker"
sorts between **CloudStack** and **GCP** in both.

- `doc/providers.md`'s table has 3 columns (`Provider | Repository |
  Notes`) and bolds provider names.
- The root `README.md`'s table has 2 columns (`Provider | Repository`, no
  separate Notes column) and does not bold provider names.

**`doc/providers.md` row:**

```markdown
| **Docker** | [TzuH-Hsu/garm-provider-docker](https://github.com/TzuH-Hsu/garm-provider-docker) | Single-host, NAS-oriented, optional per-job DinD |
```

Slice of that table with the row applied (unchanged rows abbreviated with
`...`):

```markdown
| Provider | Repository | Notes |
| ---------- | ----------- | ------- |
| **Azure** | [cloudbase/garm-provider-azure](https://github.com/cloudbase/garm-provider-azure) | |
| **CloudStack** | [nexthop-ai/garm-provider-cloudstack](https://github.com/nexthop-ai/garm-provider-cloudstack) | |
| **Docker** | [TzuH-Hsu/garm-provider-docker](https://github.com/TzuH-Hsu/garm-provider-docker) | Single-host, NAS-oriented, optional per-job DinD |
| **GCP** | [cloudbase/garm-provider-gcp](https://github.com/cloudbase/garm-provider-gcp) | |
| ... | ... | ... |
```

**Root `README.md` row** (its own 2-column format, no Notes cell, names
not bolded — matching every other row except Akamai/Linode's inline
`(experimental)` parenthetical, which no other row carries):

```markdown
| Docker | [TzuH-Hsu/garm-provider-docker](https://github.com/TzuH-Hsu/garm-provider-docker) |
```

Slice of that table with the row applied:

```markdown
| Provider | Repository |
| ---------- | ------------ |
| Azure | [cloudbase/garm-provider-azure](https://github.com/cloudbase/garm-provider-azure) |
| CloudStack | [nexthop-ai/garm-provider-cloudstack](https://github.com/nexthop-ai/garm-provider-cloudstack) |
| Docker | [TzuH-Hsu/garm-provider-docker](https://github.com/TzuH-Hsu/garm-provider-docker) |
| GCP | [cloudbase/garm-provider-gcp](https://github.com/cloudbase/garm-provider-gcp) |
| ... | ... |
```

No other part of either file needs to change: the `[[provider]]`
registration example, the environment-variable-passthrough section, and the
execution-timeout section in `doc/providers.md` are all provider-agnostic
and already describe this provider's registration shape correctly (a plain
`[[provider]]` + `[provider.external]` block with `provider_executable`
and `config_file`; see this repo's own root `README.md` "Register with
GARM" section for a worked example including `interface_version`).

**Why two tables:** `doc/providers.md` is a dedicated provider-list page
split out of `README.md` in an April-2026 docs restructure; the two have
been kept in sync ever since. The submission adds the row to both in the
same commit so they stay that way.

## (b) PR title + body

**Title:**

```
Add garm-provider-docker to the list of supported providers
```

**Body:**

```markdown
## Summary

Adds `garm-provider-docker` (https://github.com/TzuH-Hsu/garm-provider-docker,
Apache-2.0, `v0.2.0` tagged and released) as a new row in each of the two
provider tables — the root `README.md` and `doc/providers.md` — alphabetically
between CloudStack and GCP, keeping them in sync.

`garm-provider-docker` is an external provider that runs ephemeral GitHub
Actions runners as Docker containers on a single Docker host, targeting
NAS/homelab-style deployments (Synology DSM, Unraid, Raspberry Pi, and
generic Linux) on both `linux/amd64` and `linux/arm64`. Every allocation
gets its own bridge network, runner container, and workspace volume,
created and torn down together with the runner. Docker-in-Docker is
opt-in: three `dind_mode` values (`none`, `privileged-sidecar`,
`sysbox-runc`) are available per pool, bounded by an operator-configured
ceiling that a pool's `extra_specs` can never exceed — and that ceiling
itself defaults to `["none"]` (fail-closed), so no pool can obtain a
privileged DinD sidecar unless an operator explicitly widens it.

This is a documentation-only change: one new row in each of the two
provider tables, `README.md` and `doc/providers.md`, kept in sync, in
each table's own existing format and alphabetical position — no other
file touched.

## Test plan

- [ ] Both tables render correctly (row count, column alignment) when
      previewed as Markdown
- [ ] The linked repository (https://github.com/TzuH-Hsu/garm-provider-docker)
      is public, reachable, and its `v0.2.0` release is still the latest
- [ ] `README.md` and `doc/providers.md` list the same set of providers in
      the same order
- [ ] No other content in either file was modified
```

Note what the body deliberately does **not** claim: unlike the comparable
CloudStack PR ("We've been running this provider for a few weeks now and
we've done thousands of builds with it" — see below), this body makes no
production-usage claim, because none can be substantiated yet. See the
checklist's end-to-end-testing item.

## (c) Upstream requirements found

- **No `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, or PR template.** A search
  of `cloudbase/garm`'s tree (outside `vendor/`) found none at the top
  level or under `.github/`. There is no documented contribution process
  beyond "fork, branch, open a PR."
- **DCO is not required.** Verified against the most recent third-party
  provider addition, [PR #612 "Add CloudStack
  provider"](https://github.com/cloudbase/garm/pull/612) (merged
  2026-02-11): its single commit carries no `Signed-off-by` trailer, and
  `statusCheckRollup` for that PR shows only CodeQL and Go Tests/Linters —
  no DCO check ran. It was reviewed and merged by the primary maintainer
  (`gabriel-samfira`) about 6.5 hours after opening, with a one-word
  "Thanks!" comment. Some of the maintainer's own commits do carry
  `Signed-off-by` (his personal habit), and one prior external contributor
  ([PR #428](https://github.com/cloudbase/garm/pull/428), Akamai/Linode)
  added it voluntarily — but it is optional, not enforced, so the
  submission commit does not need a `Signed-off-by` trailer.
- **Commit-message style:** a short imperative headline, no
  conventional-commit prefix, for the large majority of provider-list
  commits — e.g. "Add CloudStack provider", "Add GCP to the list of
  providers", "Add the k8s provider to the list", "Add OCI to provider
  list" — optionally followed by a short body or bullet list. PR titles
  generally match the commit headline. The prepared commit and this kit's
  PR title follow that convention.
- **Ordering rule:** both tables are sorted alphabetically by provider
  display name (confirmed unchanged since this kit's first draft and again
  on this 2026-09-26 refresh).
- **Row format:** `doc/providers.md` uses `| **Name** | [org/repo](url) |
  Notes |`, with a terse Notes cell (0–4 words: blank, "Experimental",
  "Fork of LXD", "By Mercedes-Benz", "Easiest to get started"). The root
  `README.md` uses the plainer `| Name | [org/repo](url) |` — no bold, no
  Notes column (Akamai/Linode's "(experimental)" is the only inline note
  anywhere in that table).
- **Maintainer expectations:** neither comparable third-party PR (#612
  CloudStack, #428 Akamai/Linode) was asked for anything beyond the diff
  itself plus a short rationale in the PR body; both were merged the same
  day/within hours off a single maintainer review. #612 volunteered
  real-world usage evidence ("thousands of builds") that was not required
  by any documented policy but was plainly persuasive — this repo cannot
  make an equivalent claim yet (see the checklist below).

## (d) Pre-submission checklist

1. **Re-fetch and re-check the slot.** Immediately before opening the PR,
   re-fetch `doc/providers.md` from `cloudbase/garm` `main` and confirm no
   other provider has been inserted between CloudStack and GCP (or that the
   table hasn't been restructured again). As of 2026-09-26 it still has
   exactly the 10 rows listed in (a).
2. **Re-verify the release facts.** As of 2026-09-26: `v0.2.0` is published
   (`draft: false`, `prerelease: false`) at
   https://github.com/TzuH-Hsu/garm-provider-docker/releases/tag/v0.2.0,
   with `linux/amd64` and `linux/arm64` binaries and a `SHA256SUMS` file;
   `ghcr.io/tzuh-hsu/garm-provider-docker` and
   `ghcr.io/tzuh-hsu/garm-runner-noble` are both tagged `:v0.2.0` and
   `:latest` on GHCR. Re-run the equivalent `gh release view` / package
   API checks at submission time in case a newer tag has since shipped.
3. **End-to-end validation — state this plainly.** As of 2026-09-26, **no
   end-to-end run of this provider against a live GARM instance has been
   recorded in this repository.** `docs/demo-m0.md`'s own "Known unverified
   until this demo passes" section lists exactly this as open, and the
   repository's history shows only the commit that added the demo runbook
   (`9adf693`), never one recording that it was executed. The M0–M4 unit
   and `docker:dind`-backed integration-test suite passes, but that is a
   different claim from "this has run a real job through a real GARM
   instance." Either run `docs/demo-m0.md`'s runbook before submitting, or
   disclose this status honestly in the PR if you submit first — do not
   imply production usage (the way the CloudStack PR's "thousands of
   builds" line does) that has not happened.
4. **This repo's own `README.md` "Status" section** has been refreshed
   (companion commit `docs(readme): update release status for v0.2.0`) to
   state the current `v0.2.0` release facts instead of the old `v0.1.0`
   draft-release wording, so the linked repository no longer contradicts
   this PR's own claims.
5. **Submission commands** (instructions only — not executed as part of
   preparing this kit):

   ```sh
   # 1. Fork upstream (this kit does not create the fork)
   gh repo fork cloudbase/garm --clone=false

   # 2. Clone your fork
   git clone https://github.com/<your-username>/garm.git
   cd garm

   # 3. Create the branch off an up-to-date main and insert the two rows
   #    from (a), each directly after the CloudStack row of its table.
   #    If upstream has added or reordered providers since this kit was
   #    written, re-check the alphabetical slot from (a) first.
   git checkout -b add-garm-provider-docker origin/main
   awk '{print} /^\| \*\*CloudStack\*\* \|/ {print "| **Docker** | [TzuH-Hsu/garm-provider-docker](https://github.com/TzuH-Hsu/garm-provider-docker) | Single-host, NAS-oriented, optional per-job DinD |"}' \
     doc/providers.md > doc/providers.md.new && mv doc/providers.md.new doc/providers.md
   awk '{print} /^\| CloudStack \|/ {print "| Docker | [TzuH-Hsu/garm-provider-docker](https://github.com/TzuH-Hsu/garm-provider-docker) |"}' \
     README.md > README.md.new && mv README.md.new README.md

   #    Check: exactly one Docker row per file, one line added per file
   grep -c 'garm-provider-docker' doc/providers.md README.md   # expect 1 and 1
   git diff --stat                                           # expect 2 files, 2 insertions

   git commit -am "Add garm-provider-docker to the list of supported providers"

   # 4. Push to your fork
   git push -u origin add-garm-provider-docker

   # 5. Open the PR against cloudbase/garm, using the title and body from
   #    (b) above
   gh pr create --repo cloudbase/garm \
     --base main --head <your-username>:add-garm-provider-docker \
     --title "Add garm-provider-docker to the list of supported providers" \
     --body-file <path to a file containing the (b) body>
   ```
