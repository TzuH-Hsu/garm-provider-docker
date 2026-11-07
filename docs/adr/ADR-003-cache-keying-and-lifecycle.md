# ADR-003: Cache keying and lifecycle

Status: Accepted

## Context

CI jobs benefit from caches that outlive a single job: a language toolcache (the equivalent of GitHub-hosted
runners' `hostedtoolcache`), the pnpm package store, and the runner's bundled "externals" (Node runtimes).
Every job otherwise gets its own network, runner and sidecar, all destroyed at job end
([ADR-001](ADR-001-dind-strategy.md), [ADR-004](ADR-004-teardown-and-orphan-cleanup.md)). Caches are the
exception, but one repository's job must never see another repository's cached data.

GARM's `pool_id` cannot be the key. For scale sets it is a synthetic string that changes shape when a scale
set is renamed ([`garm-contract.md`](../garm-contract.md)). The provider is also a one-shot process, so cache
housekeeping cannot run on a timer. It piggybacks on calls the provider already receives.

## Decision

### Keys and scope

Caches are keyed on the normalized repository URL: lowercased, scheme removed, `.git` and trailing slashes
removed. The key is `<slug>-<hash>`, where the slug is a readable, Docker-name-safe form of the path (at most
40 characters) and the hash is the first 32 hex characters (128 bits) of the SHA-256 of the normalized URL.
Every cache volume also carries the full 64-character digest in a `repo-url-digest` label, and adoption of
an existing volume checks it, so two repositories that collided on the shorter key would not share a volume.

`repo_url` is the entity's URL, and for an organization or enterprise pool it is shared by every member
repository, so keying on it cannot separate those repositories. The provider classifies the URL by shape
(`owner/repo` is a repository, a single segment is an organization, `/enterprises/<name>` is an
enterprise; GitHub URL shapes only):

- **Repository pools** get per-repository toolcache, pnpm store and diagnostic-log volumes.
- **Organization and enterprise pools** get none, unless the operator sets `[cache].allow_org_shared = true`.
  That setting treats the whole organization as one trust domain for caches.
- **A URL that cannot be classified** never gets a persistent cache, even with `allow_org_shared`.

Classification uses the URL because the bootstrap `labels` exist only without JIT, and GARM provides no scope
field. The URL must be an absolute http(s) URL with a host, so a scheme-less `github.com/acme` cannot be
mistaken for a repository.

Caches are never shared across repositories, with two exceptions: the externals volume (no repository data),
and organization-keyed caches when `allow_org_shared` is set.

### Volumes

| Kind | Name | Mount in the runner |
|---|---|---|
| toolcache | `garm-cache-toolcache-<repokey>-<oskey>-<generation>` | `/opt/hostedtoolcache` (`RUNNER_TOOL_CACHE`) |
| pnpm store | `garm-cache-pnpm-<repokey>-<oskey>-<pnpm major>` | `/opt/pnpm-store` |
| externals | `garm-cache-externals-<image content ID>` | `/actions-runner/externals`, read-only |
| diagnostic logs | `garm-cache-diag-logs-<repokey>` | `/actions-runner/_diag` |

The paths of the first two are `[cache].toolcache_path` and `pnpm_store_path`. `generation` and `pnpm_major`
are operator salts: changing one points new jobs at a new, empty volume. The cache volume names omit the
controller ID, so two controllers on one host that serve the same repository share a volume, which is one
trust domain. The externals volume is keyed on the runner image's content ID, which exists for both
digest-pinned and tag references, and it is shared across repositories and controllers.

**OS release key.** One repository can run pools on two Ubuntu releases
([ADR-002](ADR-002-runner-image-and-jit-delivery.md)). `actions/setup-python` and `setup-node` download builds
made for one release, and pnpm's side-effects cache keeps native addons linked against that release's glibc,
so neither cache is portable across releases. The `<oskey>` is read from the runner image's
`garm.docker/os-release` label (`ubuntu-24.04`, `ubuntu-26.04`, matching `^[a-z0-9][a-z0-9._-]{0,63}$`; a
malformed value fails the allocation). An image without the label is keyed `image-` plus 32 hex characters of
its content ID, which is always correct but starts cold on every rebuild. Diagnostic logs are plain text and
stay shared across releases.

**Labels.** Every cache volume carries `garm.docker/managed=true`, `controller-id`, `cache=true`,
`cache-kind`, `last-used` and its identity keys (`repo`, `repo-url-digest`, `generation`, `pnpm-major`,
`os-release`, `image-digest`, depending on kind). It never carries `instance-name` or `create-nonce`. The
teardown and sweep predicate of ADR-004 requires an `instance-name`, so no teardown, sweep or rollback path
can match a cache volume. This is a structural exclusion, not a value check that one call site could get wrong.

**Same-repository poisoning is an accepted risk.** Any workflow that runs against a repository can write to
that repository's toolcache and pnpm store. A malicious pull request from a fork, on a pool that runs
untrusted pull requests, could plant a tool that a later trusted run of the same repository uses. The
repository is the trust boundary, as it is for GitHub's own cache action. Do not run untrusted workflows
against a repository whose pool shares a cache with trusted runs.

### A cache's identity is its label set, not its name

A volume name is only a slot. Deleting whatever unlabelled volume sits on a cache's name fails in three
ways. The daemon creates a missing named mount as an empty, unlabelled volume, and that is indistinguishable
from a foreign volume, so deletion by name can never be foreign-safe. Refusing such a name instead would
block creation permanently. And a provider-side "seed before create" step does not guarantee that a runner
never sees a half-seeded tree. The rules are therefore:

- `EnsureCacheVolume` finds its cache **by label** (kind, repository or image digest, salt, OS key) and reuses
  it under whatever name it has. If none exists and the preferred name is free, it creates the volume there.
  If the preferred name is occupied by something that is not a validated cache of ours, it **routes around**
  it to a deterministic alternate name (`<name>-x<8 hex>`) and never deletes it.
- After the runner is created and has pinned its volumes, the provider re-inspects each against the full
  expected identity. If one is not provably ours, the allocation fails closed, only this attempt's resources
  are rolled back, and the volume is **not** deleted. GARM retries, and the retry routes around the slot. The
  unprovable volume may remain as disk cruft, which is logged once.
- The provider never deletes a volume it cannot prove is its own. A `VolumeRemove` is by name with no atomic
  label-qualified form, so any inspect-then-delete is a race in which a concurrent delete plus a foreign
  volume taking the freed name could remove the foreign volume.
- A volume's labels cannot change after creation, so `last-used` records **creation time** and is not
  refreshed on reuse.

### Externals

The externals volume holds Node runtimes and nothing repository-specific. Because it is shared by every
repository, a writable mount would let one job overwrite `externals/<ver>/bin/node`, which every other
repository's runner then executes. So it is mounted **read-only** into every runner, and into the DinD
sidecar for container jobs (ADR-001). In `privileged-sidecar` mode this boundary is only as strong as the
host isolation described in ADR-001's residual risk.

A short-lived helper container, which no job runs in or influences, seeds the volume once. It takes `flock`
on a lock file in the volume and, if the atomic marker `.garm-seeded` is absent, copies the image's externals
and writes the marker last. Two concurrent first jobs lock the same inode, so one copies and the other finds
the marker. The provider runs the helper to completion (10 minute limit) before the runner mounts the volume,
and the runner's entrypoint also waits for the marker before it starts `run.sh`, so a half-seeded start is
impossible whatever the timing. An image without externals seeds an empty, marked volume.

### pnpm

The pnpm store is a named volume at `/opt/pnpm-store`. The provider sets both `npm_config_store_dir` (read by
pnpm 10 and earlier) and `pnpm_config_store_dir` (read by pnpm 11). Each version ignores the other's name,
so setting only one would leave repositories on the other version with a cold store. The environment is used
rather than an `.npmrc` so a repository's own `.npmrc` cannot redirect it. npm 11 prints a harmless
`Unknown env config "store-dir"` warning while `npm_config_store_dir` is present. The runner image installs
a pinned pnpm (default 9.15.9), and its major version must match `[cache].pnpm_major`, because the store
layout is tied to the major.

The metadata cache (package documents, and on pnpm 11 a record of lockfiles that passed its supply-chain
check) would be lost with the runner. The provider therefore also sets `npm_config_cache_dir` and
`pnpm_config_cache_dir` to `<store mount>/metadata-cache`. It is a sub-directory, never the mount root,
because `pnpm store prune` removes what it finds under `cache-dir` and deleted a whole store in a test when
`cache-dir` was the root. A job can write a forged `lockfile-verified.jsonl` there, so later jobs of the same
repository skip pnpm 11's policy check for that lockfile. The frozen install still verifies package
integrity. This is the same-repository boundary above. Measurements are in
[`package-store-installs.md`](../package-store-installs.md).

The store and the workspace are separate volumes, so pnpm cannot hardlink between them, and on a filesystem
without reflink every install copies `node_modules`. This is host guidance, in the same document. pnpm 11
also installs a pnpm version it switches to inside the store volume, and bootstraps older than 11.22.0 can
empty the shared binary. The provider cannot move that location with an environment variable, so this too is
guidance in that document.

### Garbage collection

There is no daemon, so housekeeping runs on `CreateInstance` and `ListInstances`. It never fails the call.

- **Cache volumes are never deleted automatically.** The pass finds stale ones (superseded generation, pnpm
  major or image digest, each past a 30 minute grace, or older than `stale_cache_eviction_days`, default 30,
  measured from creation) and logs them with the command an operator can run when no jobs are active:
  `docker volume prune -a --filter label=garm.docker/cache=true --filter label=garm.docker/controller-id=<id>`.
  `docker volume prune` skips volumes that a container uses. No GARM command exists for this, and adding it
  to `RemoveAllInstances` would reintroduce the race. Disk use is therefore bounded by operator maintenance.
- **Diagnostic logs are pruned by the provider**, never by the runner's entrypoint, which the job can reach.
  A helper container that mounts one diag volume runs `find <dir> -type f -mmin +<days*1440> -delete`,
  deleting files and not the volume. Retention is `diagnostic_log_retention_days` (default 7). `-mmin` is used
  because `-mtime +7` would keep files for 8 days. The prune is gated twice by a strict per-kind identity
  check: at enumeration and again after the helper pins the volume.
- **Throttling.** GARM calls `ListInstances` often, once per pool or scale set, each in a fresh process. An
  unthrottled pass would start one helper per diag volume on every call, which saturates a slow disk. Each
  pass therefore takes a claim by creating a never-started container with a fixed name per controller, for
  example `garm-diagprune-stamp-<16 hex of sha256(controller ID)>`, labelled with the round's time. Docker
  refuses a second container of that name, so exactly one process wins. The diag prune runs at most every
  6 hours and the cache GC pass at most every 5 minutes. An expired stamp is replaced by ID, an unreadable
  one is replaced without running, and a container at that name that is not ours is left alone. A daemon
  error skips the round. A round prunes at most 20 volumes, in random order. Without the runner image locally
  no stamp can be created, and the pass runs unthrottled. A host-side state file was rejected: the provider
  often runs in a container with a read-only or private filesystem, and Docker already provides an atomic
  claim.
- **Helpers** (seed, prune) carry `managed=true`, `controller-id` and `role=cache-helper`, and no
  `instance-name`. They are removed in a defer. The pass also reaps a leaked helper by state and age: a
  running helper is left alone until it exceeds the seed limit plus a margin, so a crashed seeder cannot hold
  the lock for ever, and a terminal helper after a short grace.

## Alternatives considered

- Keying on `pool_id`: unstable for scale sets.
- One global cache: breaks isolation.
- Size-based eviction: per-volume size cannot be queried portably.
- Recency from a sentinel file's mtime: on Docker Desktop the provider cannot stat a volume's filesystem from
  outside the VM. Age is measured from creation instead, so even an active cache is eventually flagged, and if
  an operator prunes it the next job re-creates it empty.
- Organization-keyed caches by default: silently pools every member repository.
- A writable externals volume: a cross-repository execution path.
- Pruning diagnostic logs from the entrypoint: retention logic a job can reach is retention a job can disable.
- Deleting stale cache volumes automatically: see "Garbage collection".

## Consequences

- Caches accumulate until an operator prunes them. Plan disk space for it, especially on hosts with limited storage.
- Organization and enterprise pools get no persistent cache by default.
- The first job on a new image generation pays for seeding the externals volume.
- Upgrading from a release that did not key caches on the OS release leaves the old toolcache and pnpm volumes
  in place, logged as superseded, and each repository starts one cold cache.
- The externals supersession check compares with the config-wide `runner_image` only, so the externals volume
  of a flavor's image is logged as superseded once past the grace window. Nothing is removed, and pruning it
  between jobs just means it is seeded again.

## Open questions

- Whether the toolcache generation should also follow runner minor-version bumps.
- Whether a diagnostic-log shipping path (syslog, Loki) is worth adding.
- The entity-scope heuristic is tested against GitHub URLs only. Other forges need their own check.
