# ADR-006: Cross-job image cache for DinD runners

Status: Proposed

This is a design for a feature that is not implemented. Implementation is gated on a measurement (phase 0,
below) whose results have not been collected. The per-job log line that the measurement needs is in the
provider; the feature itself is not. Nothing here changes behaviour. If it is built, it amends ADR-001 (the
`dind-state` volume stops being strictly per job for opted-in pools), ADR-003 (a new cache kind), ADR-004 (a
new job-scoped container role and a teardown ordering rule) and ADR-005 (a new config table and `extra_specs`
key).

## Context

In the DinD modes each allocation gets its own sidecar and its own `dind-state` volume at `/var/lib/docker`
([ADR-001](ADR-001-dind-strategy.md)). The volume is created fresh by `CreateInstance` and removed with the other
job-scoped resources at teardown ([ADR-004](ADR-004-teardown-and-orphan-cleanup.md)). No image layer,
container, volume or build-cache entry survives a job.

A registry mirror (`registry_mirrors`, ADR-001) removes the upstream download, but not the local work: every
job still extracts every image it uses into an empty data root, rebuilds every Dockerfile step, and deletes all
of it at teardown. On a host with slow or shared disks that local work is the bottleneck. A typical job pulls a
database image, a Debian base and one or two build bases, extracts gigabytes, builds on top of them, and the
teardown deletes the lot.

Constraints:

- **The provider is a one-shot process.** There is no daemon and no state store. Ownership is Docker labels,
  and mutual exclusion is a Docker name conflict ([ADR-004](ADR-004-teardown-and-orphan-cleanup.md), and the
  stamp claim in [ADR-003](ADR-003-cache-keying-and-lifecycle.md)).
- **The provider does not know the job.** Runners are JIT-configured and created before GitHub assigns a job.
  At `CreateInstance` it knows the repository URL, the pool or scale-set ID, `extra_specs` and the flavor. It
  does not know the workflow, the event, the branch, or whether the job comes from a fork.
- **Cross-repository isolation is mandatory.** Same-repository sharing is the accepted trust boundary for
  caches (ADR-003).
- **The provider never deletes a volume it cannot prove is its own**, and never deletes a cache volume by name
  opportunistically (ADR-003). Deleting files inside a pinned, identity-checked volume is allowed.
- **Privileged DinD is not a containment boundary** against a malicious job (ADR-001, residual risk).

## What Docker supports

Checked against the Docker documentation and the Moby source for the Engine 29 line (tag `docker-v29.9.2`).

1. **A data root belongs to one daemon.** The `dockerd` reference lists `--data-root` among the options to set
   separately for each daemon ([Run multiple daemons](https://docs.docker.com/reference/cli/dockerd/#run-multiple-daemons)).
   The volume store opens `volumes/metadata.db` with a one-second bbolt lock timeout
   ([`store.go`](https://github.com/moby/moby/blob/docker-v29.9.2/daemon/volume/service/store.go#L106-L108)), so a
   second daemon on a data root in use should fail to start. That is a safety net and not a mechanism to rely on.
2. **The sidecars use the classic graph driver.** Engine 29 defaults to the containerd image store for fresh
   installs, but an explicit `--storage-driver` selects the graph driver
   ([`image_store_choice.go`](https://github.com/moby/moby/blob/docker-v29.9.2/daemon/image_store_choice.go#L136-L139)).
   The provider always passes it, so every sidecar runs overlay2 or vfs. The graph driver keeps extracted
   layers only, so a cached image costs its uncompressed size.
3. **Changing the storage driver or image store hides existing data**
   ([overlayfs](https://docs.docker.com/engine/storage/drivers/overlayfs-driver/),
   [containerd store](https://docs.docker.com/engine/storage/containerd/)), so a persistent data root has to be
   tied to its driver.
4. **There is no shared or read-only image store for `dockerd`.** Several daemons can share one external
   containerd through separate namespaces, but containerd says namespaces are "an administrative construct
   that is not meant to be used as a security feature"
   ([containerd namespaces](https://github.com/containerd/containerd/blob/main/docs/namespaces.md)).
5. **Restart policies survive a daemon restart**
   ([docs](https://docs.docker.com/engine/containers/start-containers-automatically/)). A persistent data root
   lets a job leave a container that starts by itself when the next job's daemon starts.
6. **Prune has no LRU.** `docker image prune -a` removes images no container references. Its `until` filter
   compares the image's build time, not when it was pulled or last used
   ([`image_prune.go`](https://github.com/moby/moby/blob/docker-v29.9.2/daemon/images/image_prune.go#L79)).
   `docker builder prune` takes a storage limit, and BuildKit drops records by its own last-used time.
7. **BuildKit cache export needs the containerd store** on the default driver, apart from `inline`
   ([cache backends](https://docs.docker.com/build/cache/backends/)).

## Options

- **(a) A persistent `/var/lib/docker` per slot.** A slot is a named volume keyed by repository, pool and daemon
  configuration, plus an index. An allocation leases one slot exclusively, so two daemons never share a data
  root, and the sidecar mounts the slot in place of a fresh `dind-state` volume. Leftover job state is scrubbed
  before the next job and images are kept. This is the only option that removes both the download and the
  extraction for images a repository already used. It costs an exclusive lease, a scrub, a size cap with GC,
  and a weaker per-job isolation statement for opted-in pools.
- **(b) A shared read-only image store or shared containerd.** Not available (facts 1 and 4). One external
  containerd serving every sidecar would be a long-running privileged service outside the one-shot model, and
  every privileged sidecar would hold its socket, so cross-repository isolation would rest on a construct
  containerd says is not a security feature. Using the host's own containerd breaks the host-socket rule.
  Rejected.
- **(c) BuildKit cache only.** It helps Dockerfile builds and nothing else, and with these sidecars needs a
  `docker-container` builder whose own state is ephemeral. A shared registry for `cache-to` is a cross-job write
  channel. This stays job-side advice.
- **(d) Pre-seeded tarballs (`docker load`).** It writes every layer into the fresh data root each job, so it
  saves network but no disk writes. Rejected.
- **(e) Mirror only.** Implemented. It fixes upstream latency and rate limits and nothing local.

## Decision

Adopt option (a) as an opt-in, two-step feature: off by default, enabled by the operator as a ceiling and by a
pool's `extra_specs`, for `privileged-sidecar` allocations of repository-scoped pools only. Everything else
keeps the per-job `dind-state` volume. An allocation that cannot lease a slot falls back to a per-job volume, so
the cache never fails a create on its own.

### Keying

A slot belongs to one **(repository, pool, storage driver, DinD mode)** tuple and has an index
`0 .. slots_per_pool-1`.

- **Repository:** ADR-003's `repokey`. Only repository-scoped pools are eligible. Organization and enterprise
  pools get a per-job volume even with `allow_org_shared`, because a shared image store on a privileged daemon is
  a larger exposure than a shared toolcache. Cross-repository sharing is impossible by construction, because the
  key and the volume labels both carry the repository.
- **Pool:** a hash of the pool ID GARM sends. Keying per pool lets an operator separate trust levels inside one
  repository: a scale set that runs fork pull requests and one that runs only protected branches never share an
  image store, provided they are separate pools that workflows pick by `runs-on` label. The provider cannot do
  that separation itself. The scale-set pseudo ID changes when a scale set is renamed, or when GARM moves to
  UUIDv5 IDs. Each change costs one cold slot, and the abandoned one is emptied by the idle rule.
- **Storage driver and DinD mode:** part of the key (fact 3). Only `privileged-sidecar` is eligible. `sysbox-runc`
  remaps ownership in the data root and cannot be verified without Sysbox.
- **Engine version is not part of the key.** A change of the engine's `major.minor` wipes the slot instead, so an
  update reuses the volume and does not strand a full one.

### Slot volume

`garm-dind-slot-<repokey>-<poolkey>-<n>`, where `<poolkey>` is the first 8 hex characters of
`sha256(pool ID, storage driver, DinD mode)`. It is a cache volume in ADR-003's sense: labels `managed`,
`controller-id`, `cache=true`, `cache-kind=dind-state`, `repo`, `repo-url-digest`, `pool-key`, `storage-driver`,
`dind-mode`, `slot` and `last-used`, and no `instance-name`. Teardown, the sweep, the creation guard and
`RemoveAllInstances` never match it. Discovery is by label identity with ADR-003's reconcile-around-squatters
rule, and the volume is revalidated after it is pinned. The provider never removes a slot volume. It only deletes
files inside one it has leased and validated.

### Lease

The lease is a container named `garm-dind-lease-<first 16 hex of sha256(slot volume name)>`. Docker refuses a
second container of that name with 409, so creating it is an atomic claim across provider processes and
controllers, the same primitive as the stamps in ADR-003. The name omits the controller ID, as cache names do.

- **Shape:** a minimal running container from `dind_image` (`sleep`, `NetworkMode=none`, 16 MiB, unprivileged)
  with the slot mounted read-only. It runs, unlike a stamp, because `docker container prune` removes stopped
  containers, and a routine prune must not drop a lease in use. The mount pins the slot against
  `docker volume prune`.
- **Labels:** the allocation's job-scoped set plus `role=dind-slot-lease`, so it is a job-scoped resource.
  `ListInstances` and the leaked-helper reaper never touch it.
- **Acquire:** after the dind image pull and before the runner exists, ensure the volume for index 0 and create
  the lease. On 409 try the next index. Then list the containers that reference the slot. If anything other than
  this lease does, the invariant was broken out of band, so release the lease, warn and fall back. Any other
  daemon error also falls back.
- **Release:** remove the lease container, after the sidecar. Teardown skips the lease while any sidecar of the
  allocation could not be removed, so a lease is never released while a daemon may still run on the slot. The
  sidecar gets `StopTimeout = 30` so `dockerd` can close its databases (its own `shutdown-timeout` is 15 s).

### Crash recovery

The lease carries the allocation's `instance-name` and `create-nonce`, so the existing cleanup releases it with no
new machinery: the nonce-keyed rollback for a failed create, `DeleteInstance` or the sweep for a finished or
crashed job, the sweep after the 20 minute deadline for a create that died early, and `RemoveAllInstances`. Two
daemons are never on one slot in these paths, because the scrub helper bounds itself (it exits after
`scrub_timeout_seconds`, default 300, shorter than the in-flight deadline), and the leaked-helper reaper's
wedged-age bound (15 minutes) is longer than the scrub timeout.

### Scrub at lease

The scrub runs once per lease, before the runner exists, so no code of the new job runs before it finishes, and
it does not count against the entrypoint's credential and Docker waits. It runs in a **quarantine helper**: a
short-lived privileged container from `dind_image` (privileged because overlay2 mounts), with `NetworkMode=none`,
only the slot mounted, its own socket path, a fixed provider-written script, `role=cache-helper`, and the existing
helper lifecycle. Its `dockerd` starts with `--bridge=none`. Running the scrub here and not in the job's sidecar
matters because of fact 5: a restart-policy container a previous job left starts as soon as a daemon opens the
data root. In the job's sidecar it would start inside the runner's network namespace with the workspace mounted,
before the scrub could remove it. In the quarantine helper it starts with no network, no workspace and no runner.

The script:

1. Compares the marker `.garm-slot` (engine `major.minor`, storage driver) with the current values. On a mismatch,
   or with no marker on a non-empty data root, it **wipes** the data root's contents, never the volume.
2. Starts `dockerd`. If it does not become ready, stop it, wipe, and start again on the empty data root.
3. Removes state:

   | State | Action | Why |
   |---|---|---|
   | Containers, every state | `docker rm -f` on all | their env (secrets passed with `-e`), logs, writable layers, restart policies |
   | Volumes | `docker volume prune -a -f` | database files and anything a job stored |
   | User-defined networks | `docker network prune -f` | leftover topology |
   | Plugins | `docker plugin rm -f` on all | they live in the data root and return with the daemon |
   | Swarm membership | `docker swarm leave --force` if in a swarm | swarm state lives in the data root |
   | Build cache | `retain = "pulled"`: `docker builder prune -af`. `"all"`: kept, trimmed by the cap | may hold build arguments and copied files |
   | Images without a registry digest | `"pulled"`: removed. `"all"`: kept | built, committed or loaded images may hold secrets and can shadow registry names |
   | Tags not matching a pulled image's `RepoDigests` | untagged | undoes `docker tag attacker/evil postgres:16` |
   | Pulled images | **kept** | the point of the cache |

4. Applies the size cap, writes the marker, stops `dockerd` with SIGTERM and waits, and prints a one-line JSON
   summary that the provider logs.

If the helper fails or times out, the provider wipes the slot with a second helper under the same lease and
continues with the empty slot. If that also fails, it releases the lease and falls back.

### Size cap and GC

Docker has no image LRU (fact 6), so the cap is coarse. At each lease, after the scrub, the script reads usage
from the inner daemon's accounting and the free space of the filesystem. With `retain = "all"`, build cache above
half of `max_size_gib` is trimmed with `docker builder prune`. If images plus build cache still exceed
`max_size_gib`, all images are removed (`until` is not used, because it measures build dates). If the filesystem
has less than `min_free_gib` free, the slot is wiped.

A **burst guard** serialises large deletes: a prune or wipe first takes a host-wide claim (the same create-409
primitive, fixed name `garm-dind-slot-prune`, removed when done and reaped if its provider dies). A lease that
cannot get it skips pruning this time and uses the slot over its cap for one more job. At most one large delete
runs on the disk at a time. **Idle slots:** a slot not leased for `max_idle_days` is wiped by the throttled GC
round (at most every 6 hours per controller, a bounded number of slots per round, only slots no container
references, using the slot's lease name as the claim). The last-lease time is the marker's mtime. Emptied slots
for keys no longer in use stay as small cruft, reported by ADR-003's log-only GC. The disk budget is
`slots_per_pool * max_size_gib` per opted-in pool, plus the per-job volumes of allocations that fall back.

### When a slot is not used

| Condition | Result |
|---|---|
| `[dind_cache].enabled = false`, or the pool did not opt in | per-job volume |
| organization or enterprise pool, or `dind_mode = "sysbox-runc"` | per-job volume, info log |
| every slot of the key is leased | per-job volume, info log naming the key |
| slot volume squatted or of the wrong identity | reconcile to an alternate name, else per-job volume |
| another container references the slot | release the lease, per-job volume, warning |
| scrub and wipe both fail | release the lease, per-job volume, warning |
| the sidecar fails to start on the slot | normal create failure, rolled back with the lease |

### Configuration

```toml
[dind_cache]
enabled = false            # ceiling; nothing persists unless true
slots_per_pool = 1         # 1-8; set to the pool's max runners
max_size_gib = 20          # images + build cache per slot, checked at lease
min_free_gib = 10          # wipe a slot when its filesystem has less free space
max_idle_days = 14         # wipe a slot not leased for this long
retain = "pulled"          # "pulled" | "all" (also keep local images and build cache)
scrub_timeout_seconds = 300
```

Retention is operator-only: a pool cannot widen it, because keeping locally built images or build cache would
carry one pull request's build output into the next job. Pool `extra_specs` gains `"dind_cache": true`
(default `false`), rejected when `[dind_cache].enabled` is false or the effective `dind_mode` is not
`privileged-sidecar`, so `ValidatePoolInfo` reports it when the pool is edited. Scope eligibility depends on the
repository URL, which `ValidatePoolInfo` does not see, so an ineligible scope falls back at create time.

### Security

For an opted-in pool, a job can see what earlier jobs of the same repository and pool left in the image store.

- **Removed before every job:** containers (with their environment and logs), volumes, networks, plugins and
  swarm state, and with the default `retain = "pulled"` also locally built images and the build cache.
- **Kept:** images pulled from a registry. With `retain = "all"` also locally built images and build cache, which
  can contain whatever earlier builds put in layers. `retain = "all"` is for pools whose workflows are all trusted.
- **Tag shadowing:** retagging a pulled image is undone by the scrub. Tags still go stale: a cached `postgres:16`
  is what was pulled last, not the upstream's current tag. Jobs that need an exact image should pull by digest.
- **Not mitigated:** a privileged job can write into its data root directly and plant modified layers that later
  jobs of the same repository and pool use. This is ADR-003's accepted same-repository poisoning on a privileged
  daemon. A job that can do this can also reach the host (ADR-001), so it adds little.
- **Fork pull requests:** the provider cannot tell them apart. Enable `dind_cache` only on pools whose workflows
  are trusted, and keep untrusted workflows on separate pools without it.
- **Cross-repository isolation** is structural: the key, the volume labels and the lease name derive from the
  repository, the slot's identity is checked before it is mounted, and organization pools are excluded.
- **Concurrent daemons** are prevented by the lease and the reference check, with the daemon's own database lock
  as a last line.

### IO impact and observability

Steady state: pulls of cached images write nothing, and with `retain = "all"` cached build steps write nothing.
Added per job: one quarantine daemon start and stop (metadata reads, a few seconds on a slow disk) and deleting
the previous job's containers and volumes, which today's teardown deletes anyway. A prune or wipe deletes up to
`max_size_gib` at once, serialised by the burst guard. An engine `major.minor` update wipes each slot once.

One log line per DinD create gives `dind_state=slot|ephemeral`, the fallback reason, the slot, scrub duration,
usage before and after, actions and free space. Sidecar labels `garm.docker/dind-state` and `garm.docker/dind-slot`
show which sidecars use a slot. Slots are listed with
`docker volume ls --filter label=garm.docker/cache-kind=dind-state`.

## Alternatives considered

- **Scrub inside the job's sidecar before credential delivery.** It saves one daemon start per job, but
  restart-policy containers would start in the runner's namespace before the scrub removes them.
- **Edit the data root before the daemon starts.** It depends on file layouts that change between engine
  versions and orphans layers.
- **A never-started lease container, like a stamp.** `docker container prune` removes it while the slot is in use.
- **The sidecar as the lease.** In `privileged-sidecar` mode it can only be created after the runner, too late for
  a scrub before the runner exists.
- **Engine version in the key.** It strands a full slot on every engine update, and the GC never deletes volumes.
- **Keying per repository across pools.** It shares more but erases the only trust separation an operator can
  express. Kept as an open question.
- **Pruning at teardown instead of at lease.** It does not run for allocations the sweep removes.

## Consequences

- "Isolated per job" becomes, for opted-in pools, "shared by the jobs of one repository and pool, scrubbed before
  each job".
- A new job-scoped container role, a helper script, a cache kind and a config table, plus one teardown ordering rule.
- A DinD create on a slot takes a few seconds longer and saves the pull and extraction of every cached image.
- Same-repository, same-pool state crosses jobs on a privileged daemon. That is acceptable only behind two
  explicit opt-ins, with `retain = "pulled"` as the default.

## Rollout

0. **Measure first.** Collect, per DinD job, the bytes the sidecar wrote and the size of the images it held. The
   provider logs one `teardown: DinD sidecar IO` line per DinD allocation for this
   ([`dind-image-pulls.md`](../dind-image-pulls.md#measuring-what-a-job-writes-the-teardown-log-line)). The gate is
   to implement the feature only if pulls and extraction account for more than half of the bytes a DinD job
   writes. The comparison is in bytes and not durations, because a degraded disk distorts timings. Phase 0 is not
   complete.
1. Implement behind both opt-ins, `privileged-sidecar` only, with unit tests and live tests on a development
   daemon and not a production host.
2. Run the real-job gate with a `--dind-cache` option and one runner, so consecutive jobs share the slot: job A
   pulls and builds an image and leaves a restart-policy container, a volume and a network; job B (`needs: A`)
   finds the pulled image before any pull and none of A's leftovers; the sidecar mounts the slot volume and no
   per-job `dind-state` volume exists; the lease is gone afterwards and the slot remains. A variant with
   `max_size_gib = 1` shows the image prune and an empty start.
3. Release in a minor version with matching updates to ADR-001, ADR-003, ADR-004 and ADR-005 and to
   `docs/config-reference.md`.
4. Enable for one trusted repository with `slots_per_pool = 1` and `retain = "pulled"`, compare with the phase 0
   numbers, then decide on `retain = "all"` and other repositories.

Rollback is a config change: set the pool's `dind_cache` to `false` or the ceiling to off, and new allocations
use per-job volumes. Slot volumes stay until an operator prunes them.

## Settled design choices

Keying per repository and pool; retention `"pulled"` by default and operator-only; a no-network quarantine daemon
per lease, accepting a few seconds per create; a wipe on any engine `major.minor` change; organization and
enterprise pools excluded; defaults `slots_per_pool = 1`, `max_size_gib = 20`, `min_free_gib = 10`,
`max_idle_days = 14`, `scrub_timeout_seconds = 300`; `sysbox-runc` out of scope until it can be verified.

## References

- ADR-001 (topology, residual risk, registry mirrors), ADR-003 (cache identity, log-only GC, stamp claims),
  ADR-004 (labels, teardown order, sweep windows), ADR-005 (trust tiers, `extra_specs` ceilings).
- Docker: [dockerd](https://docs.docker.com/reference/cli/dockerd/),
  [containerd image store](https://docs.docker.com/engine/storage/containerd/),
  [restart policies](https://docs.docker.com/engine/containers/start-containers-automatically/),
  [image prune](https://docs.docker.com/reference/cli/docker/image/prune/),
  [volume prune](https://docs.docker.com/reference/cli/docker/volume/prune/),
  [builder prune](https://docs.docker.com/reference/cli/docker/builder/prune/),
  [cache backends](https://docs.docker.com/build/cache/backends/).
- ARC's `gha-runner-scale-set` DinD mode keeps `/var/lib/docker` in the pod and loses it with the pod.
