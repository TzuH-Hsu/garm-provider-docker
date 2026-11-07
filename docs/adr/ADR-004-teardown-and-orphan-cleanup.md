# ADR-004: Teardown and orphan cleanup

Status: Accepted

## Context

The provider is a one-shot subprocess ([`garm-contract.md`](../garm-contract.md)). GARM starts it once per
call and it exits after writing a result. No process holds ownership state, and the provider can be killed
between any two calls without losing track of what it owns or provisioning an instance twice.

An allocation spans a job network, a runner container, in DinD modes a sidecar, a socket volume and a
`dind-state` volume ([ADR-001](ADR-001-dind-strategy.md)), and a workspace volume. All of it must be removed
when the job ends, is cancelled or times out. Cleanup must never touch anything the provider does not own: no
`docker system prune` and no unscoped deletion.

`GARM_INSTANCE_ID` can be the provider's `provider_id` or the instance name, because GARM falls back to the
name when `provider_id` is empty.

GARM also runs provider processes concurrently, and nothing serialises `CreateInstance` calls. During one
`CreateInstance(A)`, A's network and volumes exist before its runner container does. A sweep started by an
unrelated `CreateInstance(B)` or `ListInstances` that treated "a managed network or volume with no live
container" as orphaned would delete A's resources mid-create, or make A's duplicate check have nothing to
key on. The design closes that structurally instead of making the sweep less aggressive.

## Decision

### Ownership is labels

Ownership is stateless and label-driven, with idempotent, allowlist-scoped teardown and no local state
store. Every "what do I own" question is a Docker label filter. Every resource carries:

- `garm.docker/managed=true`, the master switch that all cleanup filters on;
- `garm.docker/controller-id`, so several controllers can share one host;
- on job-scoped resources only, `pool-id` and `instance-name`;
- `role=runner|dind` on containers and `resource=job-network|workspace|socket|dind-state` on networks and
  volumes;
- `created-at` (RFC3339) and `create-nonce` (one random value per `CreateInstance` attempt).

The single rule for teardown, the orphan sweep and `RemoveAllInstances` is:

```
managed=true AND has instance-name AND NOT cache=true
```

It is defined in one place in the code. Cache volumes ([ADR-003](ADR-003-cache-keying-and-lifecycle.md))
never carry `instance-name`, so they cannot match. `RemoveAllInstances` therefore removes a controller's
job-scoped resources and never its caches. Purging caches is an operator action (`docker volume prune`
with the cache labels), not a provider command.

### Identity

`CreateInstance` returns the instance name as `provider_id`. `GetInstance` and `ListInstances` return the
same, so GARM never replaces it with a container ID. Every allocation resource is labelled with the instance
name, so `DeleteInstance` can find and remove the whole allocation, including the privileged sidecar, even
when the runner container was removed out of band. If `provider_id` were the container ID, the stale ID
could not be mapped back, `DeleteInstance` would return 30 as if the allocation were gone, and the sidecar,
network and volumes would leak.

`DeleteInstance`, `GetInstance`, `StartInstance` and `StopInstance` resolve `GARM_INSTANCE_ID` solely by the
`instance-name` label, restricted to `role=runner` so the sidecar that shares the label is never selected, and
validate ownership. There is no inspect-by-ID fallback. Since `provider_id` and the name are the same value,
the fallback serves no real case, and it would let an instance name that looks like a container ID resolve a
foreign or other-generation container. If the runner is gone but a network, volume or sidecar remains, those
leftovers are removed before the call returns.

### Creation and the claim marker

The first resource of every allocation is the job network, stamped with `instance-name`, `created-at` and
the attempt's nonce. It is the **claim marker**. `NetworkCreate` fails with 409 on a duplicate name, which is
the atomic duplicate primitive. A same-controller, same-name collision returns exit 31. A network with that
name that is not ours is a hard error and is never claimed.

The marker closes both races:

1. **The sweep's exclusion window.** The sweep skips any allocation whose marker is younger than the
   **in-flight create deadline** of 20 minutes, aligned with GARM's `runner_bootstrap_timeout`. A cold image
   pull plus credential fetch must never look abandoned. Only an allocation with a marker past that deadline
   and no runner container is treated as a create that never completed.
2. **Duplicate detection** keys off the marker, which exists from the first moment, and not off a runner
   container, which may not exist yet. Two concurrent creates of one name get 31 for the loser.

Any error after the marker exists runs a **creation guard**: a best-effort teardown of this attempt's resources,
selected by the nonce, under a context detached from the caller's with a 30 second limit. It never touches a
concurrent peer's resources, and a rollback failure is logged and joined to the original error.

Volume names embed the attempt's nonce (`<instance>-<nonce>-workspace`, `-socket`, `-dind-state`). The
network keeps the stable name `<instance>-net`, since that is what makes the 409 work. Teardown finds volumes
by label and removes each by the name it read, and a generation-unique name means a stale teardown can never
name a newer generation's volume. A pre-flight check rejects an instance name long enough to push a volume
name past 255 bytes, before the first Docker call. The daemon's name check has no length limit, but a local volume's
name is a directory name, which Linux filesystems cap at 255 bytes.

### Teardown order

Runner, then sidecar, then volumes, then the network last. The daemon rejects removing a network with active
endpoints and a volume that a container references, so containers go first. The network goes last because it is
the claim marker: while it exists, a concurrent same-name `CreateInstance` cannot claim a new generation and
create volumes that this still-running teardown would delete. Teardown lists once per resource kind and removes
each resource by the name or ID it read. Before each removal it re-asserts ownership on the labels
in hand, and the generation, which protects the stable-named network. Every step tolerates "not found", so
teardown is idempotent. `DeleteInstance` on an instance that is already gone returns 30, which GARM treats as
success. The same order applies to `DeleteInstance`, the sweep, the creation guard and `RemoveAllInstances`.

### Teardown scope

`DeleteInstance` and the sweep run with no operator watching, so they must not delete a concurrent generation.
At teardown start they read the nonce each instance's claim network holds and remove only resources carrying
it, so a generation that claims the name mid-teardown is left alone. If no claim network exists at the start,
there is no generation to key to and what remains is a leftover to reap. Even then, the live claim nonce is read
again immediately before each removal, so a generation that claimed the name after the resource list was taken
keeps its resources. The creation guard removes only its own attempt's nonce.

`RemoveAllInstances` is not scoped to a generation. It removes everything that matches the predicate, live or
not. A scoped rescue removes only the generation a claim network names, so while a newer generation holds the name,
an older generation's residue, such as a volume whose removal failed while its network was removed, is skipped. Dropping
the scope cannot remove the wrong volume, because each generation's volume is removed by its own unique name. The
operator who runs the rescue accepts that it also removes live allocations.

### Orphan sweep

There is no background process, so the sweep runs at the start of `CreateInstance` and in `ListInstances`. It
removes:

- an allocation whose runner has been exited for more than **2 minutes**, measured from the container's
  `FinishedAt`. Runners are single-job, so an exited runner is a finished or failed job, and the grace avoids
  racing GARM's own `DeleteInstance`;
- an allocation with no live container whose marker is past the 20 minute deadline.

A running, paused or restarting runner counts as live at any age, since a paused runner can still hold a job
and neither clock describes it. If the sweep cannot inspect a runner that is not live, it logs the error and
removes nothing, so a later sweep decides.

Both windows exist so the sweep races neither GARM's own delete nor a peer create that has not yet reached the
runner. The sweep before a create is not throttled, because a stale allocation of the instance being created
must go before the create reuses its names. The sweep in `ListInstances` runs at most once per **3 minutes** per
controller, across processes, using the stamp claim described in ADR-003 (a never-started container,
`garm-sweep-stamp-<16 hex of sha256(controller ID)>`, with `role=throttle-stamp` and no `instance-name`).
Running the sweep less often delays a collection and never changes what is collected. An exited runner's
leftovers wait about the grace plus the interval, 5 minutes at most. GARM calls `ListInstances` once per
pool or scale set on each consolidation pass, each in a new process, so without the throttle a controller with
a dozen scale sets runs the same host-wide sweep a dozen times in a row, which queues badly on a slow daemon.

### Recording why a runner died

Teardown destroys the container's exit state and logs, which are the only record of why a runner stopped.
The sweep logs them before it removes an exited runner (`orphan sweep: runner exited on its own`). A runner
that crashes mid-job usually reaches `DeleteInstance` first, because GARM marks the instance for deletion as
soon as the job's completed message arrives, so `DeleteInstance` logs the same fields
(`teardown: runner was not running at delete`) before it stops anything, for a runner whose state is not
`running`. Nothing is sent to GARM. Both lines carry `runner_phase` (`before_listening`, `listening_no_job`,
`job_started`, `job_completed` or `unknown`), classified from the listener's own lines in the log tail
before the tail is screened for credentials, so a marker on a withheld line still counts. A tail with no
marker is `before_listening` only if it could have shown one: a full tail may have scrolled past the markers,
and the 64 KiB cap keeps the oldest lines, so on a cut tail anything short of `job_completed` is `unknown`.
Markers are matched at the start of a line or after `: `, so a job's display name cannot pass for one. A normal
ephemeral end (`job_completed`, exit 0, not OOM-killed) logs at debug, anything else at warn. A running runner
gets no line and no extra Docker call. The report costs at most 2 seconds per teardown, in total.

This happens at teardown and not in the status watcher ([ADR-002](ADR-002-runner-image-and-jit-delivery.md)),
because keeping the watcher alive for hours would hold a process and a log stream per idle runner, and it
could not report to GARM anyway: GARM v0.2.1's instance middleware rejects an instance token once the runner
has left `installing` or `pending`. Docker keeps the exit state until teardown, so reading it there is free
and survives a provider restart. The cost is timing: the line appears at teardown and not at the exit. Two
paths that overlap before either has removed the container can each log it. A job that writes a
marker-shaped line to the listener's stdout can misclassify the phase, which is why the tail is logged beside it.

### Timeouts belong to GARM

GARM's reaper (every 5 minutes) and `runner_bootstrap_timeout` decide when a stuck instance is deleted, and
GARM calls `DeleteInstance`. The provider has no runner timers, only idempotency.

## Rationale

Label-driven ownership survives restarts without extra machinery, fits the one-shot model, and keeps cleanup
auditable as a few label-filtered queries. The claim marker adds one label and one timestamp to a resource
that was already created first, instead of a lock service. Leaving timeouts to GARM avoids two sources of
truth.

## Alternatives considered

- A background GC daemon: contradicts the one-shot model.
- An external state database: fragile across restarts, and Docker labels already give the lookup.
- Provider-side runner timeouts: GARM owns them.
- A sweep keyed only on "no live runner container": this is the concurrent-create race above.
- Duplicate detection keyed on the runner container: a legitimate create in progress has none yet.

## Consequences

- Orphans can linger until the next call that sweeps, plus the grace. GARM's call rate bounds this in practice.
- The sweep must tolerate races with GARM's own `DeleteInstance` and with peer creates.
- The two grace windows are independent constants (20 minutes and 2 minutes) and are not configurable.
- Each controller keeps up to three never-started stamp containers (sweep, cache GC, diag prune). They mount
  nothing, are excluded from the predicate, and can be removed by hand. The next pass then runs one round early.

## Open questions

- Whether to expose the grace windows and the throttle intervals as config.
- Whether to add a switch that disables opportunistic sweeping.
- A worked multi-controller example for the README.
- Whether the claim marker should be a dedicated resource and not the network, if a mode ever exists in which
  the network is not created first.
