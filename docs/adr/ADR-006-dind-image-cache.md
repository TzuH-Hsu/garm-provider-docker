# ADR-006: Cross-job Image Cache for DinD Runners

Status: Proposed (2026-11-02). Nothing in this ADR changes behaviour until it is accepted and implemented. If accepted, the implementation amends ADR-001 (the `dind-state` volume stops being strictly per job for opted-in pools), ADR-003 (a new cache kind), ADR-004 (a new job-scoped container role and a teardown ordering rule) and ADR-005 (a new config table and `extra_specs` key).

## Context

In the DinD modes each allocation gets its own sidecar and its own `dind-state` volume at `/var/lib/docker` (ADR-001). The volume name carries the create-nonce (`<instance>-<nonce>-dind-state`, `spec.DindStateVolumeName`), it is created fresh in `CreateInstance` (`topology.CreateDindStateVolume`), and it is removed with the other job-scoped resources at teardown (ADR-004). No image layer, container, volume or build-cache entry survives a job.

Issue #26 asked for cheaper and more reliable pulls. Its recommendation was a registry mirror first and persistent per-repository `dind-state` slots only after measuring. The mirror shipped in v0.2.5 as `registry_mirrors` (ADR-001, 2026-10-17 amendment). A mirror removes the upstream download but not the local work: every job still extracts every image it uses into an empty data root, rebuilds every Dockerfile step, and deletes all of it at teardown.

On the reference deployment that local work is the bottleneck. The disk is IO-bound, so each DinD scale set runs at most one runner, and a Docker Hub pull-through cache already serves pulls. A typical job pulls a database image, a Debian base and one or two build bases, extracts them (often gigabytes of writes), builds on top of them, and the teardown deletes the lot. Concurrency per scale set may go back to two or more once the disk is replaced.

Constraints that shape the design:

- **One-shot provider.** GARM execs the provider once per call. There is no daemon and no state store; ownership is Docker labels, and mutual exclusion is a Docker name conflict (ADR-004, and the diag-prune stamp of ADR-003's 2026-10-26 amendment).
- **The provider does not know the job.** Runners are JIT-configured and created before GitHub assigns a job. At `CreateInstance` the provider knows the repository URL, the pool or scale set ID, `extra_specs` and the flavor. It does not know the workflow, the event, the branch, or whether the job comes from a fork.
- **Cross-repository isolation is mandatory** (ADR-001, ADR-003). Same-repository sharing is the accepted trust boundary for caches (ADR-003, "same-repo cache poisoning is an accepted risk").
- **The provider never deletes a volume it cannot prove is its own**, and it never deletes a cache volume by name opportunistically (ADR-003, structural redesign and cache-GC safety amendments). Deleting files inside a pinned, identity-checked volume is allowed (the diag prune).
- **Privileged DinD is not a containment boundary** against a malicious job (ADR-001, Residual risk).

## What Docker supports

These facts were checked against the Docker documentation and the Moby source for the Engine 29 line (tag `docker-v29.9.2`) and decide which options are viable.

1. **A data root belongs to one daemon.** The `dockerd` reference lists `--data-root` among the options that must be set separately for each daemon on one host: "To avoid any conflict with other daemons, set this parameter separately for each daemon" ([dockerd, Run multiple daemons](https://docs.docker.com/reference/cli/dockerd/#run-multiple-daemons)). Docker has no supported way for two running daemons to share one. The daemon guards some of its state with file locks: the volume store opens `volumes/metadata.db` with a one-second bbolt lock timeout ([`daemon/volume/service/store.go` L106-L108](https://github.com/moby/moby/blob/docker-v29.9.2/daemon/volume/service/store.go#L106-L108)), so a second daemon on a data root that is in use should fail to start rather than share it. That is a safety net, not a mechanism this design relies on, and the live harness has to confirm it (see Verification).
2. **The sidecars use the classic graph driver, not the containerd image store.** Engine 29 makes the containerd image store the default for fresh installs ([containerd image store](https://docs.docker.com/engine/storage/containerd/)), but a storage driver set explicitly to a registered graph driver selects the graph driver: `determineImageStoreChoice` returns `graphdriver-explicit` when `--storage-driver` names one ([`daemon/image_store_choice.go` L136-L139](https://github.com/moby/moby/blob/docker-v29.9.2/daemon/image_store_choice.go#L136-L139)). The provider always passes `--storage-driver=overlay2` or `vfs` (`spec.DindCommand`), so every sidecar today runs the overlay2 (or vfs) graph driver, whatever the host daemon uses. This answers open question 1 of #26. The graph driver keeps extracted layers only, without the compressed blobs, so a cached image costs its uncompressed size.
3. **Changing the storage driver or image store hides existing data.** "Changing the storage driver makes existing containers and images inaccessible on the local system" ([OverlayFS storage driver](https://docs.docker.com/engine/storage/drivers/overlayfs-driver/)), and switching between the classic and the containerd store "temporarily hides images and containers created with the other backend" ([containerd image store](https://docs.docker.com/engine/storage/containerd/)). A persistent data root therefore has to be tied to its driver.
4. **There is no shared or read-only image store for `dockerd`.** `dockerd` has no option for an additional read-only image store (that is a `containers/storage` feature of Podman and CRI-O, `additionalimagestores`). Several `dockerd` instances can use one external containerd (`--containerd`, each with its own `--containerd-namespace`), and containerd shares image content across namespaces ("Underlying image content is still shared via content addresses"), but containerd also states that "namespaces, as implemented, is an administrative construct that is not meant to be used as a security feature" ([containerd namespaces](https://github.com/containerd/containerd/blob/main/docs/namespaces.md)).
5. **Restart policies survive a daemon restart.** A container with `restart: always` is restarted "when Docker daemon restarts", even if it had been stopped ([Start containers automatically](https://docs.docker.com/engine/containers/start-containers-automatically/)). A persistent data root therefore lets a job leave a container that starts by itself when the next job's daemon starts.
6. **Prune semantics.** `docker image prune -a` removes "all images not referenced by any container"; its `until` filter compares the image's `Created` time ([`daemon/images/image_prune.go` L79](https://github.com/moby/moby/blob/docker-v29.9.2/daemon/images/image_prune.go#L79)), which is when the image was built, not when it was pulled or last used. Image prune filters only on `until` and `label`, so there is no image LRU to lean on. `docker volume prune` removes only anonymous volumes unless `-a` is given ([volume prune](https://docs.docker.com/reference/cli/docker/volume/prune/)). `docker builder prune` takes a storage limit ([builder prune](https://docs.docker.com/reference/cli/docker/builder/prune/)), and BuildKit chooses what to drop by its own last-used time and usage count ([`cache/manager.go`, `sortDeleteRecords`](https://github.com/moby/buildkit/blob/v0.34.1/cache/manager.go#L1743)).
7. **BuildKit cache export needs the containerd store on the default driver.** The default `docker` build driver supports the `local`, `registry` and `gha` cache backends "only if you have enabled the containerd image store"; otherwise only `inline` ([Cache storage backends](https://docs.docker.com/build/cache/backends/)).

## Options

**(a) Persistent `/var/lib/docker` per slot.** A slot is a named volume keyed by repository, pool and daemon configuration, plus a slot index. An allocation leases one slot exclusively, so two daemons never share a data root, and its sidecar mounts the slot instead of a fresh `dind-state` volume. Leftover job state is scrubbed before the next job; images (and optionally build cache) are kept. This is the only option that removes both the download and the extraction for images a repository already used. Cost: an exclusive lease, a scrub, a size cap with GC, and a weaker per-job isolation statement for opted-in pools.

**(b) A shared read-only image store or shared containerd snapshotter.** Not available: `dockerd` has no read-only store (fact 4), concurrent daemons cannot share a data root (fact 1), and a data root cannot be seeded from a read-only overlay lower layer, because overlay2 does not run on an overlayfs upper (#26, option (c)). The variant that does exist, one external containerd serving every sidecar through namespaces, would need a long-running privileged service outside the provider's one-shot model, and every privileged sidecar would hold that containerd's socket, which reaches every namespace: cross-repository isolation would rest on a construct containerd says is not a security feature. Using the host's own containerd would be the host-socket red line (ADR-001). Rejected.

**(c) BuildKit cache only** (`cache-from`/`cache-to`, local or registry). This helps Dockerfile builds and nothing else: a database or service image is still pulled and extracted every job. With the sidecars' graph driver, `local` and `registry` export need a `docker-container` builder created inside the job (fact 7), whose own state is ephemeral, so every job imports the cache and writes its layers again. A shared registry for `cache-to` is a cross-job write channel that needs per-repository access control. It stays job-side advice, not a provider feature.

**(d) Pre-seeded tarballs (`docker load`).** `docker load` writes every layer of every listed image into the fresh data root each job, so it saves network but no disk writes, and the mirror already saves the network. The list would be operator config, since the host daemon would pull it. Rejected, as in #26.

**(e) Mirror only (status quo).** Done (v0.2.5). It fixes upstream latency and rate limits; every job still writes each image in full and deletes it at teardown.

| Option | Avoids download | Avoids extraction writes | Avoids rebuilds | Isolation impact | Fit |
|---|---|---|---|---|---|
| (a) slots | yes, for cached images | yes, for cached images | with `retain = "all"` | state of earlier jobs of the same repository and pool survives, scrubbed | **recommended**, opt-in |
| (b) shared store | — | — | — | not supported, or breaks cross-repository isolation | rejected |
| (c) BuildKit cache | no | no (import writes layers) | partly | depends on backend | job-side advice |
| (d) tarballs | yes, for listed images | no | no | none | rejected |
| (e) mirror | yes (LAN) | no | no | none | in place |

## Decision (proposed)

Adopt option (a) as an opt-in, two-step feature: off by default, enabled by the operator as a ceiling and by a pool's `extra_specs`, for `privileged-sidecar` allocations of repository-scoped pools only. Everything else keeps today's per-job `dind-state` volume. An allocation that cannot lease a slot falls back to a per-job volume; the cache never fails a create on its own.

### Keying

A slot belongs to one **(repository, pool, storage driver, DinD mode)** tuple and has an index `0 … slots_per_pool-1`.

- **Repository:** ADR-003's `repokey` of the normalized repository URL. Only repository-scoped pools are eligible (ADR-003 entity-scope rule). Organization and enterprise pools get a per-job volume even with `allow_org_shared = true`, because a shared image store on a privileged daemon is a larger exposure than a shared toolcache. Cross-repository sharing is impossible by construction: the key and the volume labels both carry the repository.
- **Pool:** a hash of the pool ID GARM sends with `CreateInstance` (a pool UUID, or the scale-set pseudo ID). Keying per pool, not just per repository, lets an operator separate trust levels within one repository: a scale set that runs fork pull requests and one that runs only pushes to protected branches never share an image store, provided they are separate pools and the workflows pick them by event (through `runs-on` labels). The provider cannot make that separation itself, because it does not know the job. The scale-set pseudo ID changes when a scale set is renamed or when GARM moves to its planned UUIDv5 IDs; each change costs one cold slot, and the abandoned slot is emptied by the idle rule below. On the reference deployment each repository has one DinD scale set, so per-pool keying loses nothing there.
- **Storage driver and DinD mode:** part of the key, because a data root written by one driver is invisible to another (fact 3). In this version only `privileged-sidecar` is eligible; `sysbox-runc` remaps ownership in the data root, and no development or target host can run Sysbox to verify it.
- **Engine version: not part of the key.** It is handled by wipe-on-change (below), so an engine update reuses the slot volume instead of stranding a full one.

### Slot volume

`garm-dind-slot-<repokey>-<poolkey>-<n>`, where `<poolkey>` is the first 8 hex characters of `sha256(pool ID, storage driver, DinD mode)` and `<n>` the slot index. It is a cache volume in ADR-003's sense: labels `managed=true`, `controller-id`, `cache=true`, `cache-kind=dind-state`, `repo`, `repo-url-digest`, `pool-key`, `storage-driver`, `dind-mode`, `slot`, `last-used` (creation time), and **no `instance-name`**. Teardown, the orphan sweep, the creation-guard rollback and `RemoveAllInstances` therefore never match it (ADR-004 predicate). Discovery is by label identity with ADR-003's reconcile-around-squatters rule, and the volume is revalidated after it is pinned. The strict per-kind identity schema (`spec.ValidateCacheVolumeKind`) gains the `dind-state` kind. The provider never removes a slot volume; it only deletes files inside one it has leased and validated.

### Lease

The lease is a container named `garm-dind-lease-<first 16 hex of sha256(slot volume name)>`. Docker refuses a second container of that name with 409 Conflict, so creating it is an atomic, host-wide claim across provider processes and controllers, the same primitive as the diag-prune stamp (#31, #33). The name omits the controller ID, as cache volume names do, so two controllers on one host exclude each other too.

- **Shape:** a minimal running container from `dind_image` (`sleep` as its command, `NetworkMode=none`, a 16 MiB memory limit, unprivileged), with the slot volume mounted read-only. Running, unlike the never-started diag stamp, because `docker container prune` removes stopped and created containers: an operator's routine prune must not drop a lease while a job is using the slot. Mounting the slot pins it against `docker volume prune`.
- **Labels:** the allocation's job-scoped set (`managed`, `controller-id`, `pool-id`, `instance-name`, `create-nonce`, `created-at`) plus `role=dind-slot-lease`. It is a job-scoped resource: `ListInstances` (role `runner`) never reports it, and the leaked-helper reaper (role `cache-helper`) never touches it.
- **Acquire** (in `CreateInstance`, after the dind image pull and before the runner is created): ensure the slot volume for index 0, create the lease; on 409 try the next index. Then list the containers that reference the slot volume (`ContainerList` filtered by `volume=`): if anything other than this lease references it, the lease invariant has been broken out of band (a lease removed by hand while its sidecar still runs), so the provider releases the lease, logs a warning and falls back. Any other daemon error also falls back.
- **Release:** removing the lease container. Teardown removes it **after** the sidecar: `roleRank` gains a rank for `dind-slot-lease` after `dind`, and teardown skips the lease while any sidecar of the allocation could not be removed, so a lease is never released while a daemon may still run on the slot. The next `DeleteInstance` or sweep retries. The sidecar gets `Config.StopTimeout = 30` seconds, so the `ContainerStop` in teardown lets `dockerd` stop its containers (its own `shutdown-timeout` defaults to 15 s, [dockerd](https://docs.docker.com/reference/cli/dockerd/)) and close its databases instead of being killed at Docker's default 10 s. An unclean stop is recoverable, but the slot should not depend on it.

### Crash recovery

Because the lease carries the allocation's `instance-name` and `create-nonce`, every existing cleanup path releases it with no new machinery:

- a failed create: the nonce-keyed creation-guard rollback (ADR-004);
- a finished or crashed job: `DeleteInstance`, or the orphan sweep once the runner has been exited for the short grace (about 2 minutes after `FinishedAt`);
- a create that died before its runner existed: the sweep after the in-flight-create deadline (about 20 minutes);
- a rescue: `RemoveAllInstances`.

A slot is never reachable by two daemons in these paths: the scrub helper's own command is bounded (it exits by itself after `scrub_timeout_seconds`, default 300 s), which is shorter than the in-flight-create deadline, so a lease cannot be swept while its scrub may still run; the helper's wedged-age bound in the reaper (`helperRunningMaxAge`, 15 minutes) is longer than the scrub timeout, so the reaper never kills a live scrub.

### Scrub at lease

The scrub runs once per lease, before the runner exists, so no code of the new job runs before it finishes, and its duration does not count against the runner entrypoint's credential and Docker waits.

It runs in a **quarantine helper**: a short-lived privileged container from `dind_image` (privileged because the overlay2 graph driver mounts), with `NetworkMode=none`, only the slot volume mounted, its own socket path, a fixed provider-written script, labels `managed`, `controller-id`, `role=cache-helper` and `created-at`, and the existing helper lifecycle (`runHelperContainer`: create, start, wait with a timeout, force-remove in a defer). Its `dockerd` starts with `--bridge=none`. Running the scrub here instead of in the job's sidecar matters because of fact 5: a container a previous job left with a restart policy starts as soon as a daemon opens the data root. In the job's sidecar it would start inside the runner's network namespace, with the job's workspace mounted, before the scrub could remove it; in the quarantine helper it starts with no network, no workspace and no runner, and lives for the seconds the scrub takes.

The script:

1. Compares the marker `.garm-slot` in the data root (engine `major.minor` from `dockerd --version`, and the storage driver) with the current values. On a mismatch, or with no marker on a non-empty data root, it **wipes** the data root (deletes its contents, never the volume).
2. Starts `dockerd` on the data root. If the daemon does not become ready, it stops it, wipes, and starts it again on the empty data root.
3. Removes state, in this order:

   | State | Action | Why |
   |---|---|---|
   | Containers, every state | `docker rm -f` on all | their env (including secrets passed with `-e`), logs and writable layers; restart-policy persistence |
   | Volumes, named and anonymous | `docker volume prune -a -f` | database files and anything else a job stored |
   | User-defined networks | `docker network prune -f` | leftover topology |
   | Plugins | `docker plugin rm -f` on all | plugins live in the data root and come back with the daemon |
   | Swarm membership | `docker swarm leave --force`, if the daemon is in a swarm | swarm state lives in the data root |
   | Build cache | `retain = "pulled"`: `docker builder prune -af`; `retain = "all"`: kept, trimmed by the cap | may hold build arguments and copied files of earlier builds |
   | Images without a registry digest (built, committed or loaded locally) | `retain = "pulled"`: removed; `retain = "all"`: kept | may hold secrets of earlier builds; local images can shadow registry names |
   | Tags of pulled images that do not match one of the image's `RepoDigests` repositories | untagged | removes `docker tag attacker/evil postgres:16`-style shadowing of a pulled image |
   | Pulled images | **kept** | the point of the cache |

4. Applies the size cap (below), writes the marker, stops `dockerd` with SIGTERM and waits for it to exit, and prints a one-line JSON summary (actions, sizes, durations) that the provider logs.

The daemon's exec root (`/run/docker`) lives on the job-scoped socket volume, and the daemon configuration is in the image, so neither persists.

If the helper fails or times out, the provider wipes the slot with a second helper (`rm` of the data root's contents, under the same lease) and continues with the empty slot. If that also fails, it releases the lease and falls back.

### Size cap and GC

Docker has no image LRU (fact 6), so the cap is coarse on purpose:

- **At each lease**, after step 3: the script reads the slot's usage from the inner daemon's own accounting (`docker system df`; after the scrub there are no containers or volumes left to walk, so this reads metadata only) and the free space of the filesystem under the data root (`df`, a `statfs` call, no tree walk).
  - With `retain = "all"`, build cache above half of `max_size_gib` is trimmed with `docker builder prune` to that limit; BuildKit drops its least recently used records first.
  - If images plus build cache still exceed `max_size_gib`, all images are removed (`docker image prune -af`). The next jobs pull them again through the mirror. An `until` filter is not used, because it measures build dates.
  - If the filesystem has less than `min_free_gib` free, the slot is wiped.
- **Burst guard.** A prune or wipe takes a host-wide claim first, the same create-409 primitive with a fixed name (`garm-dind-slot-prune`, role `cache-helper`, removed when done and reaped if its provider dies). A lease that cannot get it skips pruning this time and uses the slot over its cap for one more job. At most one large delete runs on the disk at a time, which is the lesson of the diag-prune IO storm (#31).
- **Idle slots.** A slot not leased for `max_idle_days` is wiped by the throttled GC round: at most once per 6 hours per controller, with the stamp pattern of the diag prune, at most a bounded number of slots per round, and only for slot volumes no container references. The GC helper takes the slot's lease name for the duration of the wipe, so it can never run while a job holds the slot. The last-lease time is the marker's mtime; a job can change it, which at worst wipes its own repository's slot early or keeps it until the cap.
- Emptied slot volumes of keys no longer in use stay as small cruft and are reported by ADR-003's log-only GC; operators reclaim them with `docker volume prune -a --filter label=garm.docker/cache-kind=dind-state`, which skips any slot that is leased.

Disk budget: the sum over opted-in pools of `slots_per_pool × max_size_gib`, plus the per-job volumes of allocations that fall back.

### When a slot is not used

| Condition | Result |
|---|---|
| `[dind_cache].enabled = false`, or the pool did not opt in | per-job `dind-state` volume (today's behaviour) |
| organization or enterprise pool, or `dind_mode = "sysbox-runc"` | per-job volume, info log |
| every slot of the key is leased | per-job volume, info log naming the key and `slots_per_pool` |
| slot volume squatted or of the wrong identity | reconcile to an alternate name (ADR-003); if that fails, per-job volume |
| another container references the slot | release the lease, per-job volume, warning |
| scrub and wipe both fail | release the lease, per-job volume, warning |
| the sidecar fails to start on the slot | normal create failure, rolled back with the lease; the retry's scrub wipes a data root its daemon cannot open |

The per-job volume is created only on fallback.

### Configuration

Provider config (operator ceiling, config-only):

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

Pool `extra_specs`: `"dind_cache": true` (boolean, default `false`). It is rejected, like a `dind_mode` outside the ceiling (ADR-005), when `[dind_cache].enabled` is false or the effective `dind_mode` is not `privileged-sidecar`, so `ValidatePoolInfo` reports it when the pool is edited. Scope eligibility depends on the repository URL, which `ValidatePoolInfo` does not see, so an ineligible scope falls back at create time instead of failing.

### Security

What changes, for an opted-in pool only: a job can see what earlier jobs of the same repository and pool left in the image store. Within that boundary:

- **Removed before every job:** containers (with their environment and logs), volumes, networks, plugins and swarm state, and with the default `retain = "pulled"` also locally built images and the build cache. A secret that an earlier job passed to `docker run -e` or wrote into a volume does not reach the next job.
- **Kept:** images pulled from a registry, which are public or registry-scoped content, plus, with `retain = "all"`, locally built images and the build cache, which can contain whatever earlier builds put in layers (build arguments, copied files). `retain = "all"` is for pools whose every workflow is trusted.
- **Tag shadowing:** retagging a pulled image to a name it was not pulled under is undone by the scrub; with `retain = "pulled"` a locally built image tagged `postgres:16` is removed. Tags still go stale: a cached `postgres:16` is what was pulled last, not the upstream's current tag. Jobs that need an exact upstream image should pull by digest, or run `docker pull` before `docker run`; with a warm slot that is one manifest request and no extraction.
- **Not mitigated:** a privileged job can write into its data root directly and plant modified layers that later jobs of the same repository and pool use. This is ADR-003's accepted same-repository poisoning, on a privileged daemon, and it is bounded by ADR-001's residual risk anyway: a job that can do this can also reach the host.
- **Fork pull requests.** The provider cannot tell them apart. Enable `dind_cache` only on pools whose workflows are trusted: no fork pull requests, or fork runs gated by approval, and keep untrusted workflows on separate pools without it. ADR-001 already restricts `privileged-sidecar` to trusted repositories and branches; this feature makes that guidance load-bearing for a quieter failure mode (poisoning without a host escape).
- **Cross-repository isolation** is structural: the key, the volume labels and the lease name all derive from the repository, the slot's identity is checked against the allocation's before it is mounted, and organization pools are excluded.
- **Concurrent daemons** are prevented by the lease and the reference check, with the daemon's own database lock as a last line (fact 1).

### IO impact

- **Steady state:** pulls of cached images write nothing (a manifest check at most), and with `retain = "all"` cached build steps write nothing. Today each job writes every image in full and deletes it at teardown, so both the extraction writes and the delete storm go away for the common images.
- **Per job, added:** one quarantine daemon start and stop (mostly metadata reads, a few seconds on a slow disk), and deleting the previous job's containers and volumes, which today's teardown deletes anyway.
- **Bursts:** a prune or wipe deletes up to `max_size_gib` at once, at lease time. The burst guard serializes them host-wide; a cap sized to hold the regular image set keeps them rare. An engine `major.minor` update wipes each slot once.
- **Fallbacks** cost what every job costs today.

### Observability

- One log line per DinD create: `dind_state=slot|ephemeral`, the fallback reason, the slot (repokey, pool key, index), scrub duration, usage before and after, actions (`wipe:engine-change|daemon-failure|low-free-space|idle`, `prune:build-cache|images`) and free space.
- Sidecar labels `garm.docker/dind-state=slot|ephemeral` and `garm.docker/dind-slot=<volume>`, so `docker ps` shows which sidecars use a slot.
- At teardown, before the sidecar stops, one log line with its cumulative block IO from a one-shot stats call. This is also the before-and-after measurement for the rollout.
- The GC round logs idle wipes and slots over their cap.
- Operators list slots with `docker volume ls --filter label=garm.docker/cache-kind=dind-state`. `docker system df -v` reports their sizes but walks every volume on the host, so it should not run routinely on a slow disk.

### Verification

- **Unit tests** (fake client): key and name derivation; slot order and 409 handling; all-busy fallback; the reference check; lease labels; teardown removes the lease after the sidecar and keeps it when the sidecar removal fails; rollback and the sweep release it; `extra_specs` and config validation; sidecar `StopTimeout`.
- **Live harness** (`dockerverify`, on a development daemon): a second `dockerd` on a leased slot fails to start and the first is unaffected; the scrub removes a `--restart always` container, a named volume and a network, and keeps a pulled image; `retain = "pulled"` removes a built image and a shadowing tag; a marker with another engine version wipes; a tiny cap prunes images; two concurrent creates with one slot get one slot and one per-job volume; a create killed after its lease releases it through the sweep (with a shortened grace).
- **Real-job gate** (a new `--dind-cache` option on the DinD variant, one runner, so consecutive jobs share the slot):
  - job A pulls an image, builds one, leaves a `--restart always` container, a named volume and a network;
  - job B (`needs: A`) finds the pulled image before any pull, and finds none of A's container, volume, network or built image;
  - step 4 checks that the sidecar mounts the slot volume (`cache=true`, `cache-kind=dind-state`, no `instance-name`) at `/var/lib/docker`, that no per-job `dind-state` volume exists, and that the lease is present;
  - step 4(e) checks that the lease is gone and the slot volume remains (listed like the caches), with no other leftovers;
  - a variant with `max_size_gib = 1` shows the image prune in the provider log, and the next job starts with no images.

## Alternatives considered

- **Scrub inside the job's sidecar before credential delivery.** It saves one daemon start per job, but containers with restart policies would start in the runner's network namespace, with the workspace mounted, before the scrub removes them. Rejected in favour of the quarantine helper.
- **Edit the data root before the daemon starts** (delete `containers/`, rewrite restart policies in `hostconfig.json`). It depends on internal file layouts that change between engine versions and orphans layers. Rejected.
- **A never-started lease container**, like the diag-prune stamp. It costs no process, but `docker container prune` removes it while the slot is in use. Rejected for a running one.
- **The sidecar as the lease.** Its name is `<instance>-dind`, which `sysbox-runc` users reach on the job network, and in `privileged-sidecar` mode it can only be created after the runner, too late for a scrub before the runner exists. Rejected.
- **Engine version in the key.** It strands a full slot on every engine update, and the GC never deletes volumes. Rejected for wipe-on-change.
- **Per-repository keying across pools.** It shares more between pools of one repository, but erases the only trust separation an operator can express. Kept as an open question.
- **Pruning at teardown** instead of at lease. It moves bursts off the job start, but does not run for allocations the sweep removes, so the lease-time pass is needed anyway.

## Consequences

- ADR-001's "isolated per job, destroyed at teardown" becomes "isolated per job by default; for opted-in pools, shared by the jobs of one repository and pool, scrubbed before each job". The amendment lands with the implementation.
- A new job-scoped container role (`dind-slot-lease`), a new helper script, a new cache kind and a new config table. Teardown gains one ordering rule.
- A DinD create on a slot takes a few seconds longer (the quarantine scrub) and saves the pull and extraction of every cached image.
- Disk use grows by up to `slots_per_pool × max_size_gib` per opted-in pool, bounded by the cap, the free-space floor and the idle wipe.
- Same-repository, same-pool state crosses jobs on a privileged daemon. That is accepted only behind two explicit opt-ins, with `retain = "pulled"` as the default.

## Rollout

0. **Measure first** (as #26 recommended). Before any slot code, ship the teardown block-IO log line, and collect per-job sidecar writes and pull durations on the reference deployment for about a week. Decide with those numbers whether the extraction writes justify the feature.
1. Implement behind both opt-ins, `privileged-sidecar` only, with unit tests and the live harness on a development daemon, never the production host.
2. Run the real-job gate with `--dind-cache` on a non-production host.
3. Release in a minor version, with the ADR-001, ADR-003, ADR-004 and ADR-005 amendments, `docs/config-reference.md`, and a section in `docs/dind-image-pulls.md` ("What a mirror does not fix" points to it).
4. Enable for one trusted repository's scale set with `slots_per_pool = 1` and `retain = "pulled"`, compare with the phase 0 numbers for a week, then decide on `retain = "all"` and on other repositories.
5. When concurrency per scale set returns to two or more, set `slots_per_pool` to the scale set's maximum and check the disk budget.

Rollback is a config change: set the pool's `dind_cache` to `false` or the ceiling to off, and the next allocations use per-job volumes. Slot volumes stay until an operator prunes them.

## Open questions for the owner

1. **Keying:** per repository and pool (proposed), or per repository across all its pools?
2. **Default retention:** `"pulled"` (proposed) or `"all"`? And should a pool be able to choose its own retention within the operator's ceiling, or is it operator-only?
3. **Scrub placement:** a quarantine daemon per lease (proposed, a few seconds per create), or inside the job's sidecar before credentials are delivered (faster, with the restart-policy window above)?
4. **Engine updates:** wipe on any engine `major.minor` change (proposed), or keep the data root on upgrades and wipe only on a downgrade?
5. **Organization and enterprise pools:** excluded (proposed), or allowed under `allow_org_shared`?
6. **Defaults:** `slots_per_pool = 1`, `max_size_gib = 20`, `min_free_gib = 10`, `max_idle_days = 14`, `scrub_timeout_seconds = 300`. Do these fit the reference deployment's disk?
7. **Go/no-go:** which phase 0 result justifies implementing? For example, image extraction making up more than half of a DinD job's writes.
8. **`sysbox-runc`:** out of scope until it can be verified (proposed)?

## References

- Issue #26 (registry mirror, and the slot design sketched there), #31 and #33 (diag-prune IO storm and throttle).
- ADR-001 (DinD topology, residual risk, registry mirrors), ADR-003 (cache identity, log-only GC, diag prune throttle), ADR-004 (labels, teardown order, sweep grace windows), ADR-005 (trust tiers, `extra_specs` ceilings).
- Code: `spec.DindCommand`, `spec.DindStateVolumeName` and `spec.BuildDindContainer` (`internal/spec`), `topology.CreateDindStateVolume`, `removeContainers` and `roleRank` (`internal/topology`), `Provider.CreateInstance`, `startDindSidecar`, `claimDiagPruneRound`, `reapLeakedHelpers` and `runHelperContainer` (`internal/provider`).
- Docker: [dockerd reference](https://docs.docker.com/reference/cli/dockerd/), [containerd image store](https://docs.docker.com/engine/storage/containerd/), [OverlayFS storage driver](https://docs.docker.com/engine/storage/drivers/overlayfs-driver/), [restart policies](https://docs.docker.com/engine/containers/start-containers-automatically/), [image prune](https://docs.docker.com/reference/cli/docker/image/prune/), [volume prune](https://docs.docker.com/reference/cli/docker/volume/prune/), [builder prune](https://docs.docker.com/reference/cli/docker/builder/prune/), [cache backends](https://docs.docker.com/build/cache/backends/), [build cache GC](https://docs.docker.com/build/cache/garbage-collection/).
- Moby `docker-v29.9.2`: [`daemon/image_store_choice.go`](https://github.com/moby/moby/blob/docker-v29.9.2/daemon/image_store_choice.go), [`daemon/volume/service/store.go`](https://github.com/moby/moby/blob/docker-v29.9.2/daemon/volume/service/store.go), [`daemon/images/image_prune.go`](https://github.com/moby/moby/blob/docker-v29.9.2/daemon/images/image_prune.go). BuildKit `v0.34.1`: [`cache/manager.go`](https://github.com/moby/buildkit/blob/v0.34.1/cache/manager.go). containerd: [namespaces](https://github.com/containerd/containerd/blob/main/docs/namespaces.md).
- Prior art: ARC's `gha-runner-scale-set` DinD mode keeps `/var/lib/docker` in the pod and loses it with the pod; Forgejo's act_runner documentation notes the same for a DinD container without a persistent volume; CircleCI's [Docker Layer Caching](https://circleci.com/docs/guides/optimize/docker-layer-caching/) keeps image layers across one project's jobs, with separate caches for parallel jobs.
