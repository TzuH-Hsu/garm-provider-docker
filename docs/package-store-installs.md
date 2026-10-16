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

## Tools that are not affected

Go, Gradle, Maven and Cargo read their caches in place rather than
materialising them into the workspace, so a separate cache volume costs them
nothing.
