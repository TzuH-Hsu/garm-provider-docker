# Package-store installs and the host filesystem

The persistent pnpm store (`/opt/pnpm-store`) and the job workspace (`/actions-runner/_work`) are two separate
Docker volumes ([ADR-003](adr/ADR-003-cache-keying-and-lifecycle.md)). That keeps the store alive across jobs
while the workspace is destroyed with the job. It also means that whether `pnpm install` is nearly free or
rewrites the whole `node_modules` tree depends on the filesystem under Docker's data root.

pnpm materialises each package into `node_modules` from the store. It prefers a cheap link to a copy and falls
back to a full copy when no link is possible. Two volumes are two mounts, and **a hardlink cannot cross a
mount**, even on one filesystem: `link()` fails with `EXDEV`. On ext4 there is no reflink either, so pnpm copies
every file of the package set out of the store, on every job, before the job's own work starts.

## Recommended host setup: XFS with reflink, or btrfs

A reflink (copy-on-write clone) does cross mounts if both mounts are one filesystem that supports it. Put
Docker's data root (or at least `/var/lib/docker/volumes`) on **XFS with `reflink=1`** (the default of
`mkfs.xfs` on current xfsprogs) or on **btrfs**, with a **Linux kernel of 5.18 or newer**, which allows cloning
across mounts of one filesystem. pnpm's default import method (`auto`) then clones with no configuration. Even
a repository that pins `package-import-method=copy` ends up sharing extents there, because the kernel's
`copy_file_range` reflinks within one filesystem. A btrfs host with an older kernel has not been measured.

Measured with a public monorepo and a warm store (about 1,190 packages, 36.8k files, 500 MiB of
`node_modules`), pnpm 10.16.1 and 11.28.6, two volumes on one filesystem:

| Host filesystem | Import method | Written per install (container I/O accounting) | New space used |
|---|---|---|---|
| ext4 | full copy | about 610 MiB | about 640 MiB |
| XFS, reflink=1 | clone | about 3 MiB | about 12 MiB |
| btrfs | clone | about 4 MiB | about 86 MiB |

Prefer XFS where there is a choice, because btrfs writes several times more filesystem metadata per install.
Bytes written carries over between machines. Install time on a fast disk only went from about 3.1 s to 1.8 s,
but a copy of this size becomes a multi-minute stall on a slow or shared disk, particularly when several
runners start at once. Reflink is a `mkfs` option and generally cannot be switched on later, so moving the data
root means stopping Docker and migrating `/var/lib/docker`.

To check a host, find Docker's data root with `docker info --format '{{.DockerRootDir}}'`, then run
`findmnt -no FSTYPE,OPTIONS --target <that directory>`, `xfs_info <that directory>` (look for `reflink=1`) and
`uname -r`. To confirm end to end on pnpm 11, run `pnpm install --reporter=ndjson` and look at the
`pnpm:package-import-method` events, which report `clone` or `copy`.

**If the host is ext4,** the full copy is expected, and nothing the provider can set avoids it. The store still saves the network
download and the tarball extraction, which is the main gain. On hosts with limited I/O (a small VM, a shared or
spinning disk), limit how many installs overlap by lowering the pool's maximum number of runners.

## The global virtual store (opt-in, with a caveat)

pnpm's global virtual store keeps the linked package directories inside the store and leaves only symlinks in the
workspace, which removes the copy on ext4 as well (a warm install took about 1.0 s and wrote about 3 MiB). It is
experimental, and the provider neither enables nor recommends it. It needs **pnpm 11 or newer**: the runners set
`CI=true`, and pnpm 10 ignores the opt-in in a detected CI environment
([pnpm/pnpm#10836](https://github.com/pnpm/pnpm/pull/10836) changed that in 11.0.0; measured on 11.28.6 only).
The setting is `virtualStoreType: global` from pnpm 11.23, and `pnpm_config_virtual_store_type=global` also works
on 11.28.6 through `[extra_specs].allowed_env` and `extra_env`.

- **It is not safe when runners install concurrently into one store.** With three runners sharing a store, 14 of
  27 installs failed with `ERR_PNPM_ENOENT` and left orphaned `*_tmp_*` files (no package content was lost). A
  store without it handled 36 of 36. A store is shared by every runner of a repository, so consider it only for a
  pool that never has two jobs of that repository at once.
- **The symlinks point into the store path.** A nested DinD container that bind-mounts the workspace does not
  see `/opt/pnpm-store`, so those links dangle there.

## The pnpm metadata cache

The store holds package contents. pnpm also keeps package **metadata** (the registry's per-package documents)
under its `cache-dir`, which defaults to a path inside the runner container and is gone with the runner. The
provider therefore keeps it on the pnpm store volume: whenever it mounts the store, it also sets
`npm_config_cache_dir` (pnpm 10 and earlier) and `pnpm_config_cache_dir` (pnpm 11) to `<store mount>/metadata-cache`
(`/opt/pnpm-store/metadata-cache` by default), and nothing for a cache-ineligible allocation. A repository's own
`.npmrc` cannot redirect it. It is a sub-directory and never the store root, because `pnpm store prune` removes
what it finds under `cache-dir` and deleted a whole store in a test.

**Why it matters on pnpm 11.** pnpm 11 checks every lockfile entry against supply-chain policies before it
installs anything, even with `--frozen-lockfile`, and the check reads each package's metadata. With an empty
cache that is one registry request per package name, tens of megabytes. Measured with `--frozen-lockfile`, a warm
store and a public monorepo of about 1,300 lockfile entries (medians of five runs on a development machine). Seconds depend
on the registry link and disk, and request counts and ratios carry over better.

| pnpm | Metadata cache | Install time | Received |
|---|---|---|---|
| 11.28.8 | empty | 10.3 s | 72 MB |
| 11.28.8 | persisted, lockfile not seen before | 3.9 s | 1.1 MB (all `304 Not Modified`) |
| 11.28.8 | persisted, same lockfile verified by an earlier job | 1.6 s | none |
| 10.34.9 | empty or persisted | 1.3 to 2.0 s | none |

pnpm 11 records each lockfile that passed the check in `<cache-dir>/lockfile-verified.jsonl` and skips the check
for it later. A frozen install on pnpm 10 makes no registry request, so the cache changes nothing for it. It only
helps commands that resolve (`pnpm add`, `pnpm update`, an install without a frozen lockfile).

- **Concurrent runners.** 72 concurrent pnpm 11 installs on one volume produced no errors or corrupt files, since
  pnpm writes a temporary file and renames it, and treats an unreadable file as a miss.
- **Staleness.** pnpm revalidates with conditional requests, so a persisted cache does not pin a job to an old
  resolution. On pnpm 11 a file younger than its release-age window (24 hours by default) is used without asking
  the registry, so `pnpm add` can see a dist-tag up to a day behind.
- **Size.** About 250 MiB per 1,100 package names, growing with the distinct packages a repository has resolved
  and not with the number of jobs (669 MiB for the same lockfile with `trustPolicy=no-downgrade`). The metadata
  goes with the store volume. The provider never cleans inside a volume.
- **Same-repository trust.** A job can write to this directory as it can to the store, including a forged
  `lockfile-verified.jsonl` record that makes later jobs of the same repository skip pnpm 11's policy check for
  that lockfile. The frozen install still checks package integrity. Nothing crosses repositories. npm 11 also
  prints a cosmetic `Unknown env config "cache-dir"` warning while these variables are set.

## pnpm switching its own version (pnpm 11 before 11.22)

pnpm 11 can install another pnpm version by itself (`pnpm self-update`, and the automatic switch to a version
pinned in `packageManager` or `devEngines.packageManager`). `pnpm/action-setup` relies on it: it installs a
bootstrap pnpm 11 and runs `pnpm self-update <version>`. The bootstrap is the standalone `@pnpm/exe` build when
the workflow sets `standalone: true` or the `node` on `PATH` is older than 22.13, as on the Ubuntu 24.04 runner
image (Node 18) unless `actions/setup-node` installs a newer Node first. pnpm puts the version it switches to in
`<store>/v11/links` and records the install under `<store>/v11/projects`. Both paths derive from `store-dir`,
and pnpm 11 has no setting that moves them, so with the persistent store the switched pnpm lands on the
repository's pnpm volume and is reused by later jobs.

**What goes wrong before 11.22.0.** Reproduced with `pnpm self-update` (the automatic `packageManager` switch was
not tested). The first job against a store works. In a later job, a bootstrap older than 11.22.0 reuses the
stored `@pnpm/exe` entry and writes the package's empty placeholder over its `pnpm` binary. That file is a
hardlink to the platform package's binary, itself a hardlink to the content-addressable file in `v11/files`, so
all three are emptied. Every later job of the repository then gets a 0-byte `pnpm`: `pnpm --version` prints
nothing and exits 0, and `actions/setup-node` with `cache: pnpm` fails with "Could not get pnpm cache folder
path". A newer pnpm does not repair this, because it trusts the emptied file in `v11/files` and links it again.

In two consecutive jobs on one fresh store, bootstraps 11.5.0, 11.15.0, 11.19.0, 11.20.0 and 11.21.0 left a
0-byte `pnpm` in the second job, and 11.22.0, 11.25.0 and 11.28.10 worked. `pnpm/action-setup` v6.0.10
bootstraps 11.19.0 and v6.1.0 bootstraps 11.25.0. Each switch also leaves a dangling symlink in
`<store>/v11/projects`, which does not affect installs.

**The runner image now checks for it.** A damaged store used to turn every job green without running anything.
The runner images ([`runner-images/README.md`](../runner-images/README.md#job-started-hook)) run a check before
the first step of every job. If the store has a 0-byte `pnpm` binary, the job fails at "Set up runner", no step
runs, and the run shows an error annotation titled "Damaged pnpm store" that names the cause and the fix. The
first job that starts on a damaged store is the one that fails, and so does every later job until the store is
replaced. The check reads the store from `pnpm_config_store_dir` (or `npm_config_store_dir`), which the provider
sets only while the persistent store is mounted, and it costs a few milliseconds. It looks for exactly the entry
that the manual check below does.

To keep jobs running while you fix a store, the operator can set `GARM_JOB_HOOKS` to `warn` (print the annotation
and run the job) or `off` (do not check) for a pool. The name goes on `[extra_specs].allowed_env`, and the pool's
`extra_specs` carries `{"extra_env": {"GARM_JOB_HOOKS": "warn"}}`
([config reference](config-reference.md#ceilings-the-extra_env-allowlist-and-the-hard-reserved-set)). A custom
runner image has the check only if it is built from these images or copies the hook.

**What to do.**

- **Repositories:** use `pnpm/action-setup` v6.1.0 or newer, or any setup whose bootstrap pnpm is 11.22.0 or
  newer. A repository that has to stay on an older setup can keep the switch off the persistent store by giving
  that step a job-local store: set `npm_config_store_dir` and `pnpm_config_store_dir` to
  `${{ runner.temp }}/pnpm-bootstrap-store` in the `env` of the `pnpm/action-setup` step. Later steps still use
  the persistent store.
- **Operators:** the provider can set no variable for this, because the store has to stay on the volume. To find
  damaged stores, run this read-only check (replace `<your-controller-id>`):

  ```sh
  for v in $(docker volume ls -q --filter label=garm.docker/cache-kind=pnpm \
      --filter label=garm.docker/controller-id=<your-controller-id>); do
    hits=$(docker run --rm -v "$v":/store:ro alpine:3.20 \
      find /store/v11/links/@pnpm -path '*/node_modules/@pnpm/*/pnpm' -type f -size 0 2>/dev/null)
    [ -n "$hits" ] && printf '%s\n%s\n\n' "$v" "$hits"
  done
  ```

  A damaged store lists entries under `/store/v11/links/@pnpm/exe/`. Remove such a volume with
  `docker volume rm <volume>` while none of the repository's runners is up. The next job starts with an empty
  store. Removing only the `links` entries is not enough, because the emptied file in `v11/files` stays and pnpm
  links it again.

Go, Gradle, Maven and Cargo read their caches in place, so they are not affected.
