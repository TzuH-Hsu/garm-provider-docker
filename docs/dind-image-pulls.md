# Image pulls in DinD jobs

In `privileged-sidecar` and `sysbox-runc` modes every job pulls the images it needs, every time. This page
explains why, and how a registry mirror makes pulls faster and more reliable. It does not apply to
`dind_mode = "none"`, where there is no daemon in the job.

## Why every DinD job starts with an empty image store

Each allocation gets its own sidecar and its own `dind-state` volume at `/var/lib/docker`. The volume is
removed with the sidecar at teardown ([ADR-001](adr/ADR-001-dind-strategy.md),
[ADR-004](adr/ADR-004-teardown-and-orphan-cleanup.md)), so one job's images, containers and build cache are
never visible to the next. The host daemon caches only the sidecar image itself. Two jobs that start the same
database image each download it in full.

That makes jobs sensitive to upstream latency, timeouts and Docker Hub's pull rate limits. Anonymous pulls are
counted per IP address, and every runner on one host shares one. See
[Docker Hub pull usage](https://docs.docker.com/docker-hub/usage/pulls/).

## A shared Docker Hub pull-through cache

Run one pull-through cache per deployment, on the Docker host or elsewhere on the LAN. The official
[`registry`](https://distribution.github.io/distribution/recipes/mirror/) image can run in proxy mode: it
fetches from Docker Hub on first request, stores the content and serves later requests locally. Sharing one
mirror across repositories is safe. A proxy-mode registry rejects pushes, so no job can write into it, and
everything in it was fetched from the upstream and is addressed by digest.

```sh
# Publish on one address the Docker host and the job networks can reach, not 0.0.0.0.
# Pin the image by digest in a real deployment.
docker run -d --restart unless-stopped -p <lan-ip>:5000:5000 \
  -v "$PWD/config.yml:/etc/distribution/config.yml:ro" -v mirror-data:/var/lib/registry registry:3
```

```yaml
# config.yml
version: 0.1
storage:
  filesystem:
    rootdirectory: /var/lib/registry
  delete:
    enabled: true      # required for the TTL scheduler to expire entries
http:
  addr: :5000
proxy:
  remoteurl: https://registry-1.docker.io
  ttl: 168h            # how long cached entries live; 0 disables expiry
```

- A proxy instance mirrors exactly one upstream. Use the `filesystem` storage driver. Without
  `storage.delete.enabled: true` the cache only grows.
- Leave Docker Hub credentials out. They raise the pull allowance, but they make every private image that
  account can read available through the mirror to anyone who can reach it. dockerd does not send credentials
  to a `registry-mirrors` endpoint ([moby/moby#30880](https://github.com/moby/moby/issues/30880)), so a
  password-protected mirror is skipped. If you set `proxy.username` and `proxy.password`, restrict access at
  the network level and use a read-only access token.
- dockerd tries the mirror before Docker Hub and falls back to Hub if the mirror is down or lacks the image, so
  the mirror is not a single point of failure. A pull by tag still asks the upstream whether the tag moved;
  pulling by digest is the most cache-friendly form.

## Pointing the sidecar at the mirror

Set `registry_mirrors` in the provider config:

```toml
registry_mirrors = ["http://mirror.lan.example:5000"]
```

The provider passes each entry to the sidecar's `dockerd` as `--registry-mirror=<url>`. Each entry is a bare
origin (scheme, host, optional port, no path). The setting is global and config-only: a pool's `extra_specs`
cannot change it, and a sidecar that already exists keeps the flags it was created with. An `http://` mirror
needs no `insecure_registries` entry, because dockerd takes the scheme from the URL. The rules and the
`insecure_registries` key are in the [config reference](config-reference.md#registry-mirrors-for-the-sidecar).

To check it, run `docker info` in a job and look for "Registry Mirrors", then read the mirror's access log (the
real-job gate does both, see [`live-gate.md`](live-gate.md#registry-mirror-check-opt-in)).

**Do not combine these keys with a custom `dind_image` whose `daemon.json` also sets `registry-mirrors` or
`insecure-registries`.** dockerd refuses to start when an option is set both as a flag and in the file, so
every DinD job would fail waiting for the daemon.

### Reachability

- Use a name or address routable from the job network, such as a LAN DNS name or the host's LAN IP. The job
  network's gateway address differs for every job and is not usable.
- **Keep the address out of the nested daemon's default bridge range, `172.17.0.0/16`.** The sidecar shares
  the runner's network namespace, and its own `dockerd` creates a `docker0` bridge there with `172.17.0.1/16`.
  A mirror written as `http://172.17.0.1:5000` (the host's `docker0` gateway) therefore resolves to the nested
  bridge, the dial is refused, and `dockerd` silently falls back to Docker Hub. **No job fails**: `docker info`
  still lists the mirror and pulls still succeed, they just bypass it. The symptom is a cold mirror and an
  access log with no pull requests. Use the host's LAN IP or a DNS name.
- The host firewall must allow traffic from the job networks to the mirror's port. With
  `[network].internal = true` the job network has no route out, so no mirror can be reached.

## Alternatives and limits

- **`mirror.gcr.io`** needs nothing to run: `registry_mirrors = ["https://mirror.gcr.io"]`. It caches
  frequently requested public images only, with no guarantee that an image stays cached, and it needs outbound
  access from the job
  ([Google's documentation](https://docs.cloud.google.com/artifact-registry/docs/pull-cached-dockerhub-images)).
- **Registries other than Docker Hub.** `registry_mirrors` applies only to Docker Hub. For `ghcr.io`,
  `quay.io` and others, run one proxy instance per upstream, a
  [Harbor proxy-cache project](https://goharbor.io/docs/main/administration/configure-proxy-cache/) per
  upstream, or [zot](https://zotregistry.dev/v2.1.2/articles/mirroring/) with `sync` in `onDemand` mode. Jobs
  then reference images through the mirror's own name, and a plain-HTTP one needs an `insecure_registries` entry.
- **What a mirror does not fix.** It lowers exposure to Docker Hub's limits but does not remove them: a cold
  entry, or several jobs requesting the same uncached image at once, still reaches Hub. It also leaves the local
  work of unpacking each image into the job's fresh `dind-state` volume, so large images stay slow to start even
  from a warm mirror. A cache that outlives a job is a proposal in [ADR-006](adr/ADR-006-dind-image-cache.md).

## Measuring what a job writes: the teardown log line

To judge how much of a job's disk writing is image pulls and extraction (the question
[ADR-006](adr/ADR-006-dind-image-cache.md) depends on), `DeleteInstance` logs one line per DinD allocation
before it stops any container. It reads the sidecar's cgroup v2 `io.stat`, then asks the sidecar's daemon which
images it holds. The line is log-only and best-effort, takes at most four seconds, never changes what is
removed, and is not produced by the sweep, by `RemoveAllInstances` or in `none` mode. The written bytes come
from `io.stat` and not from Docker's stats endpoint, which cannot sample a container whose network namespace
owner has stopped. The stats sample is only a fallback. GARM keeps a provider's stderr only when a call fails,
so **set `[logging] file`** ([config reference](config-reference.md#logging-persistent-log-file)) to collect it.

```text
level=INFO msg="teardown: DinD sidecar IO" instance=gha-7f3a pool=0f6c2e1a-... repo=acme/widgets dind_mode=privileged-sidecar since_create_s=412 written_bytes=3187671040 written_source=io.stat written_by_device="8:0=3187671040" storage_driver=overlay2 image_store=classic images=4 image_bytes=1974321152
```

(Synthetic values.) `written_bytes` is the sum of `wbytes` over every device in `io.stat`, recursive over nested
containers, builds, volumes and `dockerd`'s own metadata. `written_by_device` splits it, so a swap device can be
subtracted. `images` and `image_bytes` are the distinct images the daemon holds at teardown and the sum of their
unpacked sizes; a containerd image store also reports `content_bytes` (the compressed blobs). `since_create_s`
runs from allocation create to this line, including the wait for GARM's delete. `io_stat_error`, `probe_error`
and the `teardown: DinD sidecar IO unavailable` warning say why a field is missing.

To estimate the share of writes caused by pulls, aggregate over many jobs as a ratio of sums:

```sh
grep 'teardown: DinD sidecar IO"' /var/log/garm-provider-docker/provider.log | awk '/image_bytes=/ {
  delete v; for (i = 1; i <= NF; i++) { split($i, kv, "="); v[kv[1]] = kv[2] }
  w += v["written_bytes"]; p += v["image_bytes"] + v["content_bytes"]; n++ }
  END { printf "jobs=%d written=%d pull_estimate=%d share=%.2f\n", n, w, p, p / w }'
```

The result is an estimate with a direction. `image_bytes` counts a shared layer once per image and counts images
the job built, which overstates the pull share. Images removed before teardown, build cache and the compressed
download on the classic store are missing from it, which understates the share. `written_bytes` counts only
bytes that reached the block device, so dirty pages are missed, which can push the ratio above 1.

## Job-side advice

Jobs that start service containers should run `docker compose pull` in a retry loop before anything that has a
readiness deadline, and start the services only after the pull has finished. Set health-check `start_period`
and wait loops to cover start-up only, so that download time does not count against a service's readiness
window. Reference images by digest where possible.
