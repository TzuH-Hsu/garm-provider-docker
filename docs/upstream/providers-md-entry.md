# Upstream `doc/providers.md` entry (draft)

M4-W3 deliverable (`docs/plan.md` M4 item 5): the exact text proposed for a
future pull request against
[`cloudbase/garm`'s `doc/providers.md`](https://github.com/cloudbase/garm/blob/main/doc/providers.md),
plus a draft PR title/body. **This is a draft only** — submitting the actual
pull request is an owner-gated action; nothing here has been sent upstream.

The current upstream table (fetched via `gh api
repos/cloudbase/garm/contents/doc/providers.md` on 2026-07-26) lists ten
providers, roughly alphabetically by provider name, each with a `Repository`
link and an optional one-line `Notes` cell (e.g. `Experimental`, `Fork of
LXD`, `By Mercedes-Benz`, `Easiest to get started`). The proposed row below
follows that exact format and alphabetical position (`Docker` sorts after
`CloudStack` and before `GCP`).

## (a) Proposed table row

Insert this row into the existing `## Supported providers` table, between
the `CloudStack` and `GCP` rows:

```markdown
| **Docker** | [TzuH-Hsu/garm-provider-docker](https://github.com/TzuH-Hsu/garm-provider-docker) | Single-host, NAS-first, per-job Docker-in-Docker isolation |
```

For context, the relevant slice of the table with the new row applied
(unchanged rows abbreviated with `...`):

```markdown
| Provider | Repository | Notes |
| ---------- | ----------- | ------- |
| **Akamai/Linode** | [flatcar/garm-provider-linode](https://github.com/flatcar/garm-provider-linode) | Experimental |
| **Amazon EC2** | [cloudbase/garm-provider-aws](https://github.com/cloudbase/garm-provider-aws) | |
| **Azure** | [cloudbase/garm-provider-azure](https://github.com/cloudbase/garm-provider-azure) | |
| **CloudStack** | [nexthop-ai/garm-provider-cloudstack](https://github.com/nexthop-ai/garm-provider-cloudstack) | |
| **Docker** | [TzuH-Hsu/garm-provider-docker](https://github.com/TzuH-Hsu/garm-provider-docker) | Single-host, NAS-first, per-job Docker-in-Docker isolation |
| **GCP** | [cloudbase/garm-provider-gcp](https://github.com/cloudbase/garm-provider-gcp) | |
| ... | ... | ... |
```

No other part of `doc/providers.md` needs to change: the `[[provider]]`
registration example, the environment-variable-passthrough section, and the
execution-timeout section are all provider-agnostic and already describe
this provider's registration shape correctly (a plain `[[provider]]` +
`[provider.external]` block with `provider_executable` and `config_file`;
see this repo's own root `README.md` "Register with GARM" section for a
worked example including `interface_version`).

## (b) Draft PR title + body

**Title:**

```
doc/providers.md: add garm-provider-docker to the supported providers table
```

**Body:**

```markdown
## Summary

Adds `garm-provider-docker` (https://github.com/TzuH-Hsu/garm-provider-docker,
Apache-2.0) to the supported-providers table in `doc/providers.md`.

`garm-provider-docker` is an external provider that runs ephemeral GitHub
Actions runners as Docker containers on a single Docker host, targeting
NAS/homelab-style deployments (Synology DSM, Unraid, Raspberry Pi, and
generic Linux) on both `linux/amd64` and `linux/arm64`. It differs from the
existing cloud-VM- and LXD-oriented providers in this list by giving every
job its own isolated network, runner container, and (optionally) a
Docker-in-Docker sidecar — all created and torn down together — with three
config-selectable DinD strategies (`none`, `privileged-sidecar`,
`sysbox-runc`) bounded by an operator-controlled ceiling that a pool's
`extra_specs` can never escalate past.

This is a documentation-only change: one new row in the existing table,
in the existing format and alphabetical position, no other file touched.

## Test plan

- [ ] Table renders correctly (row count, column alignment) when previewed
      as Markdown
- [ ] The linked repository (https://github.com/TzuH-Hsu/garm-provider-docker)
      is public and reachable
- [ ] No other content in `doc/providers.md` was modified
```

## Notes for whoever submits this

- Confirm at submission time that no other new provider has been added to
  the table in the interim (re-fetch the current file and re-check the
  alphabetical slot before opening the PR).
- Confirm `garm-provider-docker` has at least one tagged release (or is
  otherwise judged stable enough to list) before submitting — as of this
  draft (2026-07-26) no version has been tagged yet (see the root
  `README.md`'s Status section).
