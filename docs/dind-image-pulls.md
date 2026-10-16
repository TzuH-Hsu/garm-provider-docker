# Image pulls in DinD jobs

In `privileged-sidecar` and `sysbox-runc` modes every job pulls the images it
needs from the upstream registry, every time. This page explains why, and how
to make those pulls faster and more reliable with a registry mirror. It
applies only to the DinD modes: in `dind_mode = "none"` there is no daemon in
the job, so nothing is pulled.

- [Why every DinD job starts with an empty image store](#why-every-dind-job-starts-with-an-empty-image-store)
- [Recommended: a shared Docker Hub pull-through cache](#recommended-a-shared-docker-hub-pull-through-cache)
- [Pointing the sidecar at the mirror](#pointing-the-sidecar-at-the-mirror)
  (the `registry_mirrors` setting, and the custom-image alternative)
- [Zero-infrastructure alternative: `mirror.gcr.io`](#zero-infrastructure-alternative-mirrorgcrio)
- [Registries other than Docker Hub](#registries-other-than-docker-hub)
- [What a mirror does not fix](#what-a-mirror-does-not-fix)
- [Job-side advice](#job-side-advice)

## Why every DinD job starts with an empty image store

Each allocation gets its own DinD sidecar and its own `dind-state` volume,
mounted at `/var/lib/docker`. The volume is created for the job and removed at
teardown, together with the sidecar and the other job-scoped resources. This
is the isolation design of [ADR-001](adr/ADR-001-dind-strategy.md) (see also
[ADR-004](adr/ADR-004-teardown-and-orphan-cleanup.md) for teardown): one job's
images, containers and build cache are never visible to the next job.

The consequence is that no image layer survives a job. The host daemon caches
only the sidecar image itself (`dind_image`); every image a job pulls *inside*
the sidecar goes to the job's own `dind-state` volume and is gone when the job
ends. Two jobs that start the same database image each download it in full.

That is expected, not a malfunction, but it makes jobs sensitive to upstream
latency, timeouts and Docker Hub's pull rate limits (at the time of writing,
100 pulls per 6 hours for anonymous clients, counted per IPv4 address or IPv6
/64, and 200 for a Personal account; see
[Docker Hub pull usage](https://docs.docker.com/docker-hub/usage/pulls/) for
the current numbers). Every runner on one host shares one address.

## Recommended: a shared Docker Hub pull-through cache

Run one pull-through cache per deployment, on the Docker host or elsewhere on
the LAN, and let every repository and every runner use it. The official
[`registry`](https://distribution.github.io/distribution/recipes/mirror/)
image (distribution) can run in *proxy mode*, where it fetches content from
Docker Hub on first request, stores it, and serves later requests from local
storage.

Sharing one mirror across repositories is safe. In proxy mode the registry
rejects pushes, so no job can write into the cache, and everything in it was
fetched from the upstream registry and is addressed by digest. A job cannot
plant an image that another repository's job would then receive.

### Compose snippet

```yaml
# compose.yaml - a Docker Hub pull-through cache.
services:
  registry-mirror:
    image: registry:3  # pin by digest in a real deployment
    restart: unless-stopped
    ports:
      # Bind to one address the Docker host and the job networks can reach,
      # not 0.0.0.0 (see "Reachability" and the credentials note below).
      - "<lan-ip>:5000:5000"
    volumes:
      - ./config.yml:/etc/distribution/config.yml:ro
      - mirror-data:/var/lib/registry

volumes:
  mirror-data:
```

### `config.yml`

```yaml
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
  # Optional Docker Hub credentials, see the warning below.
  # username: <hub-user>
  # password: <hub-access-token>
```

- `proxy.remoteurl` is the one upstream this instance mirrors. A proxy
  instance mirrors exactly one upstream.
- Use the `filesystem` storage driver; the distribution documentation
  recommends it for a pull-through cache. Without `storage.delete.enabled:
  true` the TTL scheduler cannot clean up, and the cache only grows.
- `proxy.ttl` defaults to `168h` (seven days). Size the volume for the set of
  images your jobs use, and set the TTL to taste.
- Docker Hub credentials are optional, and **leaving them out is the
  recommended default**. They raise the pull allowance (see the numbers
  above) and let the mirror pull private images, but **they also make every
  private image that account can read available through the mirror to anyone
  who can reach it**. Do not try to protect the mirror with HTTP
  authentication: dockerd does not send credentials to a `registry-mirrors`
  endpoint for Docker Hub image names (the long-standing, still open
  [moby/moby#30880](https://github.com/moby/moby/issues/30880)), so a
  password-protected mirror is skipped and every pull falls back to Docker
  Hub. If you do set `username`/`password`, restrict access at the network
  level instead: publish the mirror only on an address that the Docker host
  and the job networks can reach, limit the host firewall to the Docker
  address pools, and never expose it beyond the host or LAN segment. Treat
  the mirror's storage as holding that account's private content, protect the
  registry configuration file that holds the credentials, and use an access
  token with read-only scope rather than the account password.

### How dockerd uses the mirror

- A mirror is consulted for pulls from Docker Hub (`docker pull postgres`,
  `FROM node:22`) and is tried before Docker Hub itself. If the mirror is
  down or does not have the image, dockerd falls back to the upstream, so the
  mirror is not a single point of failure for jobs.
- Pulls of content the mirror already holds are served locally. A pull by
  tag still asks the upstream whether the tag has moved, so the mirror lowers
  but does not remove the dependence on Docker Hub. Pulling by digest is the
  most cache-friendly form.
- A mirror URL must be a bare origin: scheme, host and optional port, no
  path.

### Reachability

The sidecar's daemon reaches the mirror over the per-job bridge network:

- Use a name or address that is routable from the job network, such as a LAN
  DNS name or the host's LAN IP with the published port. The job network's
  gateway address is **not** a usable address: it is different for every
  job. Container DNS on the user-defined job network forwards to the host's
  resolvers, so a name your host resolves normally also resolves in the job.
- The host firewall must allow traffic from the job networks (the Docker
  address pools) to the mirror's port.
- A mirror is unreachable when `[network].internal = true`, because the job
  network then has no route out. With such a configuration the daemon cannot
  use a mirror (or the upstream registry).
- A plain-HTTP mirror needs no `insecure_registries` entry: dockerd takes the
  scheme from the mirror URL (see [Pointing the sidecar at the
  mirror](#pointing-the-sidecar-at-the-mirror)). An insecure-registries entry
  is for a TLS mirror whose certificate the sidecar does not trust. The
  alternative is a TLS mirror whose CA the sidecar trusts, which means baking
  the CA into a custom image.

## Pointing the sidecar at the mirror

Set `registry_mirrors` in the provider config
([#26](https://github.com/TzuH-Hsu/garm-provider-docker/issues/26)). It is a
top-level key next to `dind_image` and `storage_driver`, and the provider
passes each entry to the sidecar's `dockerd` as `--registry-mirror=<url>`:

```toml
registry_mirrors = ["http://mirror.lan.example:5000"]
```

- Each entry is a bare origin: `http://` or `https://`, a host, an optional
  port, no path, query, fragment or userinfo. Anything else fails the config
  load with a message naming the entry.
- It is global and config-only. A pool's `extra_specs` cannot set or override
  it, and it is not a per-flavor setting: a mirror belongs to the Docker
  host's network, not to a pool. The provider reads its config on every
  invocation, so each new sidecar picks up the current value; a sidecar that
  already exists keeps the flags it was created with.
- **An `http://` mirror does not need `insecure_registries`.** dockerd takes
  the scheme from the mirror URL, so `http://mirror.lan.example:5000` is
  contacted over plain HTTP by itself. This was checked in the Moby source for
  the Engine 29.x line (release `docker-v29.8.2`): the mirror endpoint is built
  from the URL as written (`daemon/pkg/registry/service_v2.go`), and the
  containerd image-store pull path copies the URL's scheme into the registry
  host (`daemon/hosts.go`); `insecure-registries` only switches off
  certificate verification and enables an HTTP fallback for the hosts it
  lists. Set `insecure_registries = ["mirror.lan.example:5000"]` only when the
  mirror serves HTTPS with a certificate the sidecar does not trust, and for a
  private HTTP registry that jobs pull from by name. Entries are `host[:port]`
  or a CIDR, never a URL; each becomes `--insecure-registry=<entry>`.
- The effective values are logged when each sidecar is created (`DinD sidecar
  registry settings`). Duplicate and malformed entries fail the config load.

To check the result, run `docker info` in a job and look for "Registry
Mirrors" (and "Insecure Registries" if you set any). Mirrors are listed with
a trailing `/`. The mirror's own access log shows whether pulls reach it; the
real-job gate does both, see [`demo-m0.md`](demo-m0.md#registry-mirror-check-opt-in).

> **Warning: do not combine these keys with a custom `dind_image` whose
> `daemon.json` also sets `registry-mirrors` or `insecure-registries`.**
> dockerd refuses to start when an option is set both as a flag and in the
> configuration file ("the following directives are specified both as a flag
> and in the configuration file"), so the sidecar would never become ready and
> every DinD job would fail waiting for the daemon. Use the provider keys, or
> the image's `daemon.json`, not both.

### Alternative: a custom `dind_image` (older provider versions)

On a provider version without `registry_mirrors`, the same result needs a
custom sidecar image: `dind_image` accepts any digest-pinned image, and
`dockerd` reads `/etc/docker/daemon.json` by default.

`daemon.json`:

```json
{
  "registry-mirrors": ["http://mirror.lan.example:5000"]
}
```

Add an `"insecure-registries"` list only for the cases described above (an
HTTPS mirror with an untrusted certificate, or a private HTTP registry).

`Dockerfile`:

```dockerfile
# Use the same base you would otherwise set as dind_image, pinned by digest.
FROM docker:dind@sha256:<digest>
COPY daemon.json /etc/docker/daemon.json
```

Build it, push it to a registry the Docker host can pull from, and set
`dind_image` to the pushed image **by digest**
(`registry.example/garm-dind@sha256:<digest>`), as for any other
`dind_image` (see the README on digest pinning). Rebuild it when you update
the base image.

> **Warning: do not set `hosts`, `storage-driver` or `group` in this
> `daemon.json`.** The provider already passes those as `dockerd` flags
> (`--host`, `--storage-driver`, `--group`). Docker refuses to start when an
> option is set both as a flag and in the configuration file, regardless of
> the values, so the sidecar would never become ready and every DinD job
> would fail waiting for the daemon. Keep the file to `registry-mirrors` and
> `insecure-registries`, and drop those too once you move to the provider's
> `registry_mirrors` / `insecure_registries` keys. Never add a TCP `hosts`
> entry in any case: the ADR-001 rule that nothing listens on TCP is not
> configurable.

## Zero-infrastructure alternative: `mirror.gcr.io`

Google operates a public cache of popular Docker Hub images at
`mirror.gcr.io`. It needs nothing to run: set

```toml
registry_mirrors = ["https://mirror.gcr.io"]
```

(or, on a provider version without that key, the equivalent
`"registry-mirrors": ["https://mirror.gcr.io"]` in the custom image's
`daemon.json`).

It caches frequently requested public images only, and Google gives no
guarantee that a given image stays cached (see
[Google's documentation](https://docs.cloud.google.com/artifact-registry/docs/pull-cached-dockerhub-images)).
Anything it does not hold falls back to Docker Hub. It cannot serve private
images, and you do not control its contents, so it suits popular base and
service images better than a deployment-wide guarantee. It also needs outbound
internet access from the job, so it is incompatible with
`[network].internal = true`.

## Registries other than Docker Hub

`registry_mirrors` (dockerd's `registry-mirrors`) applies **only to Docker Hub**. Pulls from `ghcr.io`,
`quay.io`, `gcr.io` and other registries never go through it, and a
distribution proxy instance mirrors exactly one upstream. Options:

- One distribution proxy instance per upstream registry (each with its own
  `remoteurl`, port and storage).
- [Harbor proxy-cache projects](https://goharbor.io/docs/main/administration/configure-proxy-cache/),
  one project per upstream registry.
- [zot](https://zotregistry.dev/v2.1.2/articles/mirroring/) with `sync` in
  `onDemand` mode, which can front several upstreams in a single instance.

All of these are addressed by the mirror's own name, so jobs must reference
images through it, for example `mirror.lan.example:5001/owner/image:tag`
instead of `ghcr.io/owner/image:tag`. They are not transparent. Unlike a
Docker Hub mirror, these are ordinary registries to dockerd, not entries in
`registry_mirrors`: if one is plain HTTP, the sidecar needs it in
`insecure_registries` (`mirror.lan.example:5001`).

## What a mirror does not fix

A mirror cuts download latency and reduces the number of pulls that reach
Docker Hub, so it lowers your exposure to its rate limits. It does not
eliminate them: a cold or expired entry, or several jobs requesting the same
uncached image at once, still pulls from Docker Hub and counts against its
limits, and the mirror remains subject to Docker's fair-use policy. Set Hub
credentials on the mirror (with the caveats above) if you need a higher
allowance. A mirror also does not remove the per-job work of unpacking each
image: layers are still extracted
into the job's fresh `dind-state` volume, so every job still writes each image
in full to local disk. Large images stay slow to start even from a warm
mirror. Avoiding that cost would need an image store that outlives a job,
which the per-job design of ADR-001 deliberately does not provide.

## Job-side advice

The provider can make pulls faster; it cannot make them guaranteed. Jobs that
depend on service containers should:

- Pull explicitly, with retries, before starting anything that has a
  readiness deadline:

  ```sh
  for attempt in 1 2 3 4 5; do
    docker compose pull && break
    [ "$attempt" -eq 5 ] && exit 1
    sleep $((attempt * 5))
  done
  docker compose up --wait
  ```

- Start the services only after the pull has finished, and set health-check
  `start_period` and any wait loop to cover start-up only. Image download time
  should not count against a service's readiness window.
- Reference images by digest where possible, which is also the friendliest
  form for a pull-through cache.
