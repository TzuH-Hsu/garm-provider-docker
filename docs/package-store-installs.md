# Package-store installs and the host filesystem

The persistent pnpm store (`/opt/pnpm-store`) and the job workspace
(`/actions-runner/_work`) are two separate Docker volumes (ADR-003). That is
what keeps the store alive across jobs while the workspace is destroyed with
the job, but it has a cost on some hosts: whether `pnpm install` is nearly
free or rewrites the whole `node_modules` tree depends on the filesystem under
Docker's data root.

- [What happens on every install](#what-happens-on-every-install)
- [Recommended host setup: XFS with reflink, or btrfs](#recommended-host-setup-xfs-with-reflink-or-btrfs)
- [Checking your host](#checking-your-host)
- [If the host is ext4](#if-the-host-is-ext4)
- [The global virtual store (opt-in, with a caveat)](#the-global-virtual-store-opt-in-with-a-caveat)
- [The pnpm metadata cache](#the-pnpm-metadata-cache)
- [pnpm switching its own version (pnpm 11 before 11.22)](#pnpm-switching-its-own-version-pnpm-11-before-1122)
- [Tools that are not affected](#tools-that-are-not-affected)

## What happens on every install

pnpm materialises each package into `node_modules` from the store. It prefers a
cheap link over a copy, and falls back to a full copy when no link is possible.

Two volumes are two mounts, and **a hardlink cannot cross a mount**, even when
both mounts sit on the same filesystem: `link()` fails with `EXDEV`. Any
layout with the store and the workspace on separate mounts therefore loses
hardlinks. On ext4 there is no reflink either, so pnpm copies every file in the
package set out of the store, on every job, before the job's own work starts.

Tools with the same hardlink-or-copy design (uv and bun, for example) can
behave the same way if their cache is placed on a volume separate from the
workspace. The provider does not mount a cache for them, and they were not
measured.

## Recommended host setup: XFS with reflink, or btrfs

A reflink (copy-on-write clone) does cross mounts, as long as both mounts are
the same filesystem and the filesystem supports it. Put Docker's data root (or
at least `/var/lib/docker/volumes`) on:

- **XFS with `reflink=1`**, the default of `mkfs.xfs` on current xfsprogs; or
- **btrfs**,

with a **Linux kernel of 5.18 or newer**, which allows cloning across mounts of
one filesystem. pnpm's default import method (`auto`) then clones with **no
configuration**: nothing to set in the provider, the pool, or the repository.
Even a repository that pins `package-import-method=copy` ends up sharing
extents on such a host, because the kernel's `copy_file_range` reflinks within
one filesystem.

Measured effect, for a public monorepo workload with a warm store (about 1,190
packages, 36.8k files, 500 MiB of `node_modules`), pnpm 10.16.1 and 11.28.6,
two volumes on one filesystem:

| Host filesystem | Import method | Written per install (container I/O accounting) | New space used |
|---|---|---|---|
| ext4 | full copy | about 610 MiB | about 640 MiB |
| XFS, reflink=1 | clone | about 3 MiB | about 12 MiB |
| btrfs | clone | about 4 MiB | about 86 MiB |

Prefer XFS where there is a choice: btrfs writes several times more filesystem
metadata per install. Bytes written is the figure that carries over between
machines; install time on a fast disk only went from about 3.1 s to 1.8 s, but
a copy of this size is what turns into multi-minute stalls on a slow or shared
disk, particularly when several runners start at once. Bytes written scale with
the size of `node_modules`.

Reflink is a `mkfs` option. An XFS filesystem created without it cannot
generally be switched over later, so moving the data root onto a suitable
filesystem means stopping Docker and migrating `/var/lib/docker` (the cache
volumes are cheap to rebuild, so it is also acceptable to start them cold).
Synology DSM uses btrfs, so reflink should apply there with a sufficiently new
kernel, but DSM kernels are often older than 5.18 and this has not been
measured.

pnpm 12 tries a hardlink before a clone on Linux (pnpm 10 and 11 clone first).
Across two mounts the hardlink fails with `EXDEV` and pnpm then clones, so the
guidance is the same.

## Checking your host

```sh
# Where Docker keeps volumes, and what filesystem that is.
docker info --format '{{.DockerRootDir}}'
findmnt -no FSTYPE,OPTIONS --target "$(docker info --format '{{.DockerRootDir}}')"

# XFS: look for reflink=1.
xfs_info "$(docker info --format '{{.DockerRootDir}}')" | grep -o 'reflink=[01]'

# Kernel: 5.18 or newer.
uname -r
```

To confirm it end to end, compare the runner container's `io.stat` write bytes
across an install, or on pnpm 11 run `pnpm install --reporter=ndjson` and look
at the `pnpm:package-import-method` events, which report `clone` or `copy`.

## If the host is ext4

The full copy is expected and nothing the provider can set avoids it. The store
still saves the network download and the tarball extraction, which is the main
win, but not the materialisation of `node_modules`.

On I/O-constrained hosts (a small VM, a NAS, a shared or spinning disk), limit
how many installs overlap by capping concurrent jobs: lower the pool's maximum
number of runners. The per-job bytes written scale with the size of
`node_modules`, so large monorepos are hit first.

## The global virtual store (opt-in, with a caveat)

pnpm's global virtual store keeps the linked package directories inside the
store and leaves only symlinks in the workspace, which removes the copy on ext4
as well: in the same measurements a warm install took about 1.0 s and wrote
about 3 MiB. It is an experimental pnpm feature and is **not** enabled or
recommended by the provider.

It needs **pnpm 11 or newer**. The runners set `CI=true`, and pnpm 10 treats a
detected CI environment as always turning the feature off, even when it is set
explicitly, so on pnpm 10 the opt-in is silently ignored and installs keep
copying. Respecting an explicit setting in CI was added in pnpm 11.0.0
([pnpm/pnpm#10836](https://github.com/pnpm/pnpm/pull/10836)); it was measured
here on 11.28.6 only. Do not set `CI=false` to work around this on pnpm 10,
because other tools change behaviour with it. The setting is
`virtualStoreType: global` from pnpm 11.23 (`enableGlobalVirtualStore` before
that); on 11.28.6 the environment variable
`pnpm_config_virtual_store_type=global` also works, which an operator can pass
through `[extra_specs].allowed_env` and a pool's `extra_env`.

Two reasons to be careful:

- **It is not safe when runners install concurrently into one store.** With
  three runners sharing one store, 14 of 27 installs failed with
  `ERR_PNPM_ENOENT` and left orphaned `*_tmp_*` files in the store (no package
  content was lost). A store without the global virtual store handled 36 of 36
  concurrent installs without error. Because a store is shared by every runner
  of one repository, consider it only for a pool whose repository never has
  two jobs running at once.
- **The symlinks in `node_modules` point into the store path.** A nested DinD
  container that bind-mounts the workspace does not see `/opt/pnpm-store`, so
  those links dangle there.

pnpm 11 also keeps its own version switches in the same `links` directory,
whatever this setting says; see
[pnpm switching its own version](#pnpm-switching-its-own-version-pnpm-11-before-1122).

## The pnpm metadata cache

The store holds package contents. pnpm also keeps package **metadata** (the
registry's per-package documents) under its `cache-dir`, which defaults to
`$XDG_CACHE_HOME/pnpm` and so lives in the runner container's writable layer,
gone with the runner. Without help, every job starts with an empty metadata
cache.

The provider therefore keeps it on the pnpm store volume. Whenever it mounts
the store and sets the store-dir variables, it also sets `npm_config_cache_dir`
(read by pnpm 10 and earlier) and `pnpm_config_cache_dir` (read by pnpm 11), to
`<store mount>/metadata-cache`, `/opt/pnpm-store/metadata-cache` by default.
Nothing is set for a cache-ineligible allocation or when the cache feature is
off. There is no new volume: the metadata shares the store's repository scope,
its lifecycle and its manual cleanup. A repository's own `.npmrc` cannot
redirect it, for the same reason it cannot redirect the store.

### Why it matters on pnpm 11

pnpm 11 checks every entry of the lockfile against supply-chain policies before
it installs anything, even with `--frozen-lockfile`. The default
`minimumReleaseAge` of 1440 minutes (one day) is enough to trigger it, and the
check reads each package's metadata. With an empty cache that is one registry
request per package name, tens of megabytes in total.

Lab measurements: `--frozen-lockfile`, warm store, a public monorepo with about
1,300 lockfile entries (1,110 package names), Docker Desktop on a laptop, pnpm
11.28.8 and 10.34.9, medians of five runs. Absolute seconds depend on the
registry link and the disk; the request counts and ratios carry over better.

| pnpm | Metadata cache | Install time | Received |
|---|---|---|---|
| 11 | empty (before this change) | 10.3 s | 72 MB |
| 11 | persisted, lockfile not seen before | 3.9 s | 1.1 MB (all `304 Not Modified`) |
| 11 | persisted, same lockfile verified by an earlier job | 1.6 s | none |
| 10 | empty or persisted | 1.3 to 2.0 s | none |

pnpm 11 records each lockfile that passed the check in
`<cache-dir>/lockfile-verified.jsonl`, keyed by a hash of the lockfile
contents, and skips the check for the same lockfile later; that is the 1.6 s
row. For a changed lockfile the persisted documents turn the downloads into
conditional requests, the 3.9 s row. A frozen install on pnpm 10 makes no
registry request at all, so for it the cache changes nothing; it only helps
commands that resolve (`pnpm add`, `pnpm update`, an install without a frozen
lockfile). Writing the cache the first time cost nothing measurable.

### Concurrency, staleness and size

- **Concurrent runners.** Runners of one repository share the volume. In the
  lab, 72 concurrent pnpm 11 installs on one volume produced no errors and no
  corrupt files: pnpm writes a temporary file and renames it, and treats an
  unreadable file as a cache miss.
- **Staleness.** A persisted cache does not pin a job to an old resolution.
  After a lockfile change a frozen install resolved exactly as with an empty
  cache, and an entry whose version has moved on at the registry is re-fetched.
  Older metadata is served only on request (`--prefer-offline`) or by design:
  pnpm 11 uses a cache file younger than its release-age window (24 hours by
  default) without asking the registry. A job running `pnpm add` can therefore
  see a dist-tag up to a day behind.
- **Size.** About 250 MiB per 1,100 package names in the lab, roughly half the
  size of the store for that workload. It grows with the number of distinct
  packages the repository has ever resolved, not with the number of jobs, since
  each package's file is rewritten in place. A repository that sets
  `trustPolicy=no-downgrade` makes pnpm cache the full documents, which was 669
  MiB for the same lockfile. The provider never cleans inside a volume, so the
  metadata is removed with the store volume (see the cache volume cleanup in the
  README). `pnpm store prune` also removes it, which is harmless: it is
  re-fetched on demand.
- **Same-repository trust.** A job can write to this directory, as it can to
  the store. In particular, a job can add a forged `lockfile-verified.jsonl`
  record, which makes later jobs of the same repository skip pnpm 11's policy
  check for that exact lockfile. The frozen install still checks every package
  against the integrity hashes in the lockfile. This is the same-repository
  boundary ADR-003 already accepts for the store; nothing crosses repositories,
  and org-scoped pools get no persistent cache unless `allow_org_shared` is on.
- **npm 11 warning.** Like `npm_config_store_dir`, `npm_config_cache_dir` makes
  npm 11 print an `Unknown env config "cache-dir"` warning on each npm or npx
  call. It is cosmetic and goes away when pnpm 10 support is dropped.

### Why a sub-directory

The cache directory is never the store mount itself. On pnpm 11, `pnpm store
prune` deletes what it finds under `cache-dir`, and with `cache-dir` set to the
store root it deleted the whole store in a lab run. In `metadata-cache/` it can
only remove the metadata. The runner user creates the directory on first use
inside the store mount it already owns.

## pnpm switching its own version (pnpm 11 before 11.22)

pnpm 11 can install another pnpm version by itself: `pnpm self-update`, and
the automatic switch to a version pinned in `packageManager` or
`devEngines.packageManager`. `pnpm/action-setup` relies on it. It installs a
bootstrap pnpm 11 and runs `pnpm self-update <version>` to reach the version
the workflow asks for. The bootstrap is the standalone `@pnpm/exe` build when
the workflow sets `standalone: true` or when the `node` on `PATH` is older than
22.13. The Ubuntu 24.04 runner image ships Node 18, so the standalone build is
what a job gets there unless `actions/setup-node` installs a newer Node first.

pnpm puts the version it switches to in its global virtual store, the `links`
directory of the store (`<store>/v11/links`), and records the job's install
under `<store>/v11/projects`. Both paths are derived from `store-dir`, and
pnpm 11 has no setting that moves them; setting
`pnpm_config_global_virtual_store_dir` or `pnpm_config_virtual_store_dir` was
tried and changes nothing. With the persistent store, the switched pnpm
therefore lands on the repository's pnpm volume and is reused by later jobs.

### What goes wrong before pnpm 11.22.0

This was reproduced with `pnpm self-update`, the path `pnpm/action-setup`
takes; the automatic `packageManager` switch was not tested. The first job
against a store works. In a later job, a bootstrap pnpm older than 11.22.0
reuses the stored `@pnpm/exe` entry and writes the package's empty
placeholder over its `pnpm` binary. That file is a hardlink to the
platform package's binary (`@pnpm/linux-x64` or `@pnpm/linux-arm64`), which is
itself a hardlink to the content-addressable part of the store (`v11/files`).
The write empties all three. From then on every job of the repository gets a
0-byte `pnpm`:

- `pnpm --version` prints nothing and exits 0;
- `actions/setup-node` with `cache: pnpm` fails at `pnpm store path --silent`
  with "Could not get pnpm cache folder path".

A newer pnpm does not repair a store in this state: it trusts the emptied file
in `v11/files` and links it again.

Lab results, two consecutive jobs on one fresh store volume, switching to pnpm
10.16.1 with the provider's store environment (linux/amd64 runner image and
linux/arm64):

| Bootstrap `@pnpm/exe` | Second job |
|---|---|
| 11.5.0, 11.15.0, 11.19.0, 11.20.0, 11.21.0 | 0-byte `pnpm` |
| 11.22.0, 11.25.0, 11.28.10 | works |
| any version, after an older one emptied the file | 0-byte `pnpm` |

`pnpm/action-setup` v6.0.10 bootstraps 11.19.0. v6.1.0 bootstraps 11.25.0.

Each switch also leaves a symlink in `<store>/v11/projects` that points into the
finished job's home directory. These are a few bytes each, dangle in every
later job, and do not break installs.

### What to do

- **Repositories:** use `pnpm/action-setup` v6.1.0 or newer, or any setup whose
  bootstrap pnpm is 11.22.0 or newer. A repository that has to stay on an older
  setup can keep the switch off the persistent store by pointing the store at
  a job-local directory for that step only:

  ```yaml
  - name: Set up pnpm
    uses: pnpm/action-setup@<sha>
    env:
      npm_config_store_dir: ${{ runner.temp }}/pnpm-bootstrap-store
      pnpm_config_store_dir: ${{ runner.temp }}/pnpm-bootstrap-store
  ```

  Later steps still use the persistent store. `cache: pnpm` on
  `actions/setup-node` is not needed when the provider persists the store.
- **The provider** sets no extra variable for this: no pnpm 11 setting
  separates the switch from the store, and the store itself has to stay on the
  volume. See ADR-003, amendment of 2026-11-02.
- **Operators:** check for stores that are already damaged. The listing below
  is read-only. Replace `<your-controller-id>` as for the cache purge in the
  README:

  ```sh
  for v in $(docker volume ls -q \
      --filter label=garm.docker/cache-kind=pnpm \
      --filter label=garm.docker/controller-id=<your-controller-id>); do
    hits=$(docker run --rm -v "$v":/store:ro alpine:3.20 \
      find /store/v11/links/@pnpm -path '*/node_modules/@pnpm/*/pnpm' \
        -type f -size 0 2>/dev/null)
    [ -n "$hits" ] && printf '%s\n%s\n\n' "$v" "$hits"
  done
  ```

  A damaged store lists entries such as
  `/store/v11/links/@pnpm/exe/<version>/<hash>/node_modules/@pnpm/exe/pnpm`
  and the matching `@pnpm/linux-x64` (or `linux-arm64`) entry. Remove such a
  volume while none of the repository's runners is up:

  ```sh
  docker volume rm <volume>
  ```

  Docker refuses to remove a volume that a container still uses. The next job
  of the repository starts with an empty store and fills it again. Removing
  only the `links` entries is not enough, because the emptied file in
  `v11/files` stays and pnpm links it again.

## Tools that are not affected

Go, Gradle, Maven and Cargo read their caches in place rather than
materialising them into the workspace, so a separate cache volume costs them
nothing.
