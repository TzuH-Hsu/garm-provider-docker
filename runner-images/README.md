# Runner images

`garm-provider-docker` publishes two GitHub Actions runner images. Both are built `FROM` a digest-pinned
`myoung34/github-runner` base, carry the same package set, and replace the base image's `config.sh` entrypoint
with the shared, JIT-aware [`common/entrypoint.sh`](common/entrypoint.sh). The design is in
[ADR-002](../docs/adr/ADR-002-runner-image-and-jit-delivery.md), and the Docker-in-Docker (DinD) wait in
[ADR-001](../docs/adr/ADR-001-dind-strategy.md).

| Image | Ubuntu | Default compilers | Directory |
|---|---|---|---|
| `ghcr.io/tzuh-hsu/garm-runner-noble` | 24.04 (Noble) | GCC 13, LLVM 18 (`clang-format`, `clang-tidy`) | [`noble/`](noble/README.md) |
| `ghcr.io/tzuh-hsu/garm-runner-resolute` | 26.04 (Resolute) | GCC 15, LLVM 22 (`clang-format`, `clang-tidy`) | [`resolute/`](resolute/README.md) |

## Choosing an image

Use the Resolute image for workflows that target Ubuntu 26.04 and its toolchain (GCC 15 as the default `gcc`,
LLVM 22 for formatting and static analysis, Python 3.14). Use the Noble image for workflows that target
Ubuntu 24.04. Both are published on every release.

The provider runs one image per pool. Set the config-wide default with `runner_image`, and give pools that need the
other release a [flavor](../docs/config-reference.md#flavors) whose `runner_image` points at it. A pool selects the
flavor through `extra_specs` (`{"flavor": "resolute"}`). Pin both images by digest.

Runners on different Ubuntu releases never share a toolcache or pnpm store, because the provider keys those
volumes on the image's `garm.docker/os-release` label (`ubuntu-24.04`, `ubuntu-26.04`;
[ADR-003](../docs/adr/ADR-003-cache-keying-and-lifecycle.md)). A derived image inherits the label from its base.
An image built from another base should set it too. Otherwise its caches are keyed on its exact image digest and
start cold whenever it is rebuilt.

### Image digests

`runner_image` must be pinned by digest. Resolve the digest of a published image with
`docker buildx imagetools inspect ghcr.io/tzuh-hsu/garm-runner-noble:<tag>` (or
`skopeo inspect docker://ghcr.io/tzuh-hsu/garm-runner-noble:<tag>`), and use the multi-arch index digest it
prints, so one reference works on `linux/amd64` and `linux/arm64`. The `linux/arm64` images are built by the
release workflow and have not been verified end to end.

## Why a custom entrypoint

`myoung34/github-runner` has no JIT (just-in-time) registration support: its entrypoint only knows
`config.sh --token`. GARM's metadata service serves JIT credentials as three files (`.runner`, `.credentials`,
`.credentials_rsaparams`) and not as a single `--jitconfig` blob. The provider fetches the files itself over
HTTPS and delivers them into the container, which never talks to GARM and never receives the instance token. The
entrypoint waits for the delivery, installs the files (or, without JIT, registers with a token) and starts the
runner.

## How credentials arrive

The provider fetches the credential files before it creates anything, creates the runner with a memory-backed
tmpfs at `/run/garm` (`mode=0700`, owned by the runner user, uid and gid 1001), starts it, and streams the files
into the tmpfs with a `docker exec` of `tar -x`. The last entry is a `.delivered` marker, so the entrypoint never
sees a partial set. `docker cp` cannot be used, because Docker resolves its archive paths in a view that excludes a
running container's user tmpfs mounts. The entrypoint symlinks the files from `/run/garm` into `/actions-runner`
and never copies them to the writable layer. They exist only in container memory: not in the image, not in
`docker inspect` output, not on host disk. The job's user can read them, since they are its own runner's
credentials. The registration token that non-JIT pools receive is the exception: it is not scoped to one runner,
so the entrypoint deletes it as soon as it has read it, before `config.sh` runs and before any job starts.

## Environment contract

Always set by the provider:

| Variable | Purpose |
|---|---|
| `JIT_CONFIG_ENABLED` | `true` or `false`. Selects the JIT or non-JIT path. |
| `RUNNER_WORKDIR` | The job work directory. The entrypoint creates and chowns it. |
| `GITHUB_URL` | The GitHub base URL. Informational with JIT, and the host part of the registration URL without it. |
| `DISABLE_RUNNER_UPDATE` | Always `true`. |
| `DOCKER_HOST`, `DOCKER_SOCK_GID` | DinD modes only. `DOCKER_HOST` starts the Docker readiness wait. `DOCKER_SOCK_GID` (fixed at `2000`) is the group the entrypoint adds the `runner` user to, so an unprivileged `docker` call reaches the sidecar's socket. Unset in `none` mode. |
| `RUNNER_TOOL_CACHE` | Set whenever `[cache].enabled` is true. The entrypoint creates and chowns the directory before dropping privileges. |
| `npm_config_store_dir`, `pnpm_config_store_dir` | The same path, only when a persistent pnpm store is mounted. pnpm 10 and earlier read the first name and pnpm 11 the second, and each ignores the other. |
| `npm_config_cache_dir`, `pnpm_config_cache_dir` | `<pnpm store path>/metadata-cache`, set exactly when the two store variables are. See [`package-store-installs.md`](../docs/package-store-installs.md#the-pnpm-metadata-cache). |
| `GARM_DIAG_DIR` | Set when a diagnostic-logs volume is mounted, so the entrypoint owns that directory before dropping privileges. |
| `GARM_EXTERNALS_SEEDED_MARKER` | Set only when the shared externals volume is mounted. The entrypoint waits for this marker before it starts the runner. |
| `GARM_JOB_HOOKS` | Always set: `fail`, `warn` or `off`, from the `job_hooks` setting. Read by the [job-started hook](#job-started-hook), not by the entrypoint. |

With npm 11, the `npm_config_` names make every npm or npx call print a harmless "Unknown env config" warning.

With JIT the provider sets nothing else. GARM bakes the runner's name, labels, group and ephemeral flag into the
credential files, so `RUNNER_ORG`, `RUNNER_REPO`, `RUNNER_ENTERPRISE`, `RUNNER_GROUP`, `RUNNER_NAME`,
`RUNNER_LABELS`, `RUNNER_NO_DEFAULT_LABELS` and `RUNNER_EPHEMERAL` are never set. Without JIT (pools with
`disable_jit_config`) the provider also sets whichever of `RUNNER_ORG`, `RUNNER_REPO` and `RUNNER_ENTERPRISE`
applies, plus `RUNNER_GROUP`, `RUNNER_NAME`, `RUNNER_LABELS`, `RUNNER_NO_DEFAULT_LABELS` and `RUNNER_EPHEMERAL`, all
consumed by `config.sh`. The instance token, the metadata URL and the callback URL are never in the container.

Entrypoint tuning, not part of the provider's contract:

| Variable | Default | Purpose |
|---|---|---|
| `GARM_CRED_WAIT_SECONDS` | `120` | How long to wait for the `.delivered` marker. |
| `WAIT_FOR_DOCKER_SECONDS` | `120` | How long to wait for the Docker daemon, in DinD modes. |
| `GARM_EXTERNALS_WAIT_SECONDS` | `600` | How long to wait for the externals marker. On a warm volume it is already there. |
| `RUN_AS_ROOT` | unset | `true` runs the runner, and so the job, as root. If unset and `gosu` is missing, the entrypoint refuses to start. `extra_specs` cannot set it. |

## Entrypoint steps

1. **Waits.** Wait for `/run/garm/.delivered`, and in DinD modes (`DOCKER_HOST` set) poll `docker info` at the same
   time. If either fails the other is stopped.
2. **Root preparation.** Raise the open-file soft limit to 65536 or the hard limit, whichever is lower (best
   effort). Add the `runner` user to the DinD socket group. Create and chown the mounted cache and diagnostic
   directories, which a fresh named volume mounts as root-owned. Each step does nothing when its variable is unset.
3. **Install credentials.**
   - With JIT, link `/actions-runner/.runner`, `.credentials` and `.credentials_rsaparams` to the tmpfs files. Run
     no `config.sh`.
   - Without JIT, first read `/run/garm/registration-token` and delete it, before any other step. The entrypoint
     fails if the token is missing, empty or unreadable, or if the file cannot be removed. Then run
     `./config.sh --unattended --ephemeral --disableupdate --url ... --token ... --name ...` as the
     runner user through `gosu`, and only after it succeeds move the generated files onto the tmpfs and replace them
     with symlinks. Linking before `config.sh` would leave dangling links, and the runner's `configure` step
     crashes on one (`FileNotFoundException`, exit 134). A trap removes credential files and the registration
     token if the read or `config.sh` fails. This path has not been verified against a real GARM pool.
4. **Externals wait.** If `GARM_EXTERNALS_SEEDED_MARKER` is set, wait for it, so the runner never starts on a
   half-seeded externals tree.
5. **Start.** Drop to the `runner` user with `gosu` and `exec ./run.sh` in the foreground, so it receives signals.
   Without `gosu` and without `RUN_AS_ROOT`, the entrypoint exits with an error and does not run as root.

The script never echoes credential contents.

## What is in the images

**Jobs have passwordless sudo.** The `myoung34/github-runner` bases ship `%sudo ALL=(ALL) NOPASSWD: ALL` and put
the `runner` user in the `sudo` group, and the provider sets no `no-new-privileges`, so a job can run `sudo` as root
inside its own container, as on GitHub-hosted runners. The container is unprivileged and has no host socket in
`none` mode, but a job in a DinD mode also reaches a root-equivalent daemon
([ADR-001](../docs/adr/ADR-001-dind-strategy.md)). To remove sudo, use a derived image without the rule.
Installing system packages on every job is slow anyway. The policy:

- **Languages and toolchains** (Node, Python, Go, Java) come from the `setup-*` actions at job time, through the
  toolcache.
- **Commonly needed apt-level dependencies live in the images.** They do not mirror GitHub-hosted runner images and
  carry only the system packages ordinary jobs assume.
- **Single-binary tools a job installs itself go into `/usr/local`.** CI scripts written for GitHub-hosted runners
  install pinned tools into `/usr/local/bin` and `/usr/local/lib` and use `sudo` only when those are not
  writable. Both directories are owned by the runner user, so such scripts work without root, and in a derived
  image without sudo. Only the two directories change owner, not the files the base ships in them, and each
  runner container serves one job and is removed with its writable layer.
- **Unusual needs get a derived image**, selected through a flavor: `FROM` one of these images, `apt-get install`
  what the repository needs, publish it, and point a flavor at it.

Already in the `myoung34/github-runner` bases: `git`, `git-lfs`, `gh`, `curl`, `wget`, `jq`, `yq`, `zip`, `unzip`,
`xz-utils`, `zstd`, `bzip2`, `rsync`, `openssh-client`, `gnupg`, `build-essential` (gcc, g++, make, libc6-dev),
`libssl-dev`, `pkg-config`, `python3` with `venv` and `pip`, `perl`, `patch`, `tzdata`, `locales`, the AWS CLI, Node
and the Docker CLI with Compose. Both images add the following, for both `linux/amd64` and `linux/arm64`:

| Packages | Why |
|---|---|
| `libnss3`, `libnspr4`, `libatk1.0-0t64`, `libatk-bridge2.0-0t64`, `libatspi2.0-0t64`, `libcups2t64`, `libdbus-1-3`, `libgbm1`, `libxkbcommon0`, `libasound2t64`, `libcairo2`, `libpango-1.0-0`, `libx11-6`, `libxcb1`, `libxext6`, `libxfixes3`, `libxcomposite1`, `libxdamage1`, `libxrandr2` | The shared libraries a headless Chrome or Chromium for Testing links against. Puppeteer, Playwright and Lighthouse CI download a browser at job time but not its system libraries. This is the set `ldd` reports as missing for Chrome for Testing on the Noble base, which matches Puppeteer's Debian and Ubuntu list. `libgbm1` brings Mesa and its own LLVM runtime. |
| `fontconfig`, `fonts-liberation`, `fonts-noto-color-emoji` | A minimal font set so headless pages render text and emoji (screenshots, visual tests, PDF output). It is not a full CJK or symbol font set. |
| `file` | Not in the bases. Build and packaging scripts call it. |
| `pipx` | PEP 668 marks the system Python as externally managed on both releases, so `pip install --user` fails. CI scripts install Python CLI tools with `pipx` into `~/.local/bin`. |
| `clang-format-<N>`, `clang-tidy-<N>` | C and C++ formatting and static analysis, LLVM 18 on Noble and LLVM 22 on Resolute. The unversioned names point at them through `update-alternatives`, as on GitHub-hosted images. About 300 MB per image for the LLVM libraries `clang-tidy` needs. |
| `cmake`, `ninja-build` | GitHub-hosted images carry both. Repositories that pin their own versions install them into `/usr/local`, which comes first on `PATH`. About 50 MB. |

Not added: `xvfb` and a headed-browser stack, `sqlite3`, database clients, network debugging tools, and LLVM
releases that are not in the Ubuntu archive of that release (no third-party apt repositories).

**Open-file limit.** Docker starts containers with a soft limit of 1024 open files, and a GitHub-hosted runner
gives jobs about 65536. The entrypoint raises the soft limit to 65536 (clamped to the hard limit) as root, before
the `gosu` drop, so `run.sh` and every step inherit it. A runner already at or above the target is left alone. It
is done in the entrypoint and not as a `HostConfig.Ulimits` entry so that images built `FROM` these also get it.

**pnpm.** The `myoung34/github-runner` bases ship Node (18 on Noble, 22 on Resolute) but not `pnpm`, `npm` or
`corepack`, so the provider's persistent pnpm store would be unused. Both images install a pinned pnpm
(`ARG PNPM_VERSION`, default `9.15.9`): the registry tarball is extracted to `/opt/pnpm` and run by the base
image's own Node through a small `/usr/local/bin/pnpm` wrapper. The pnpm **major** version must match the
provider's `[cache].pnpm_major` (default `9`), because the store layout is tied to it. When you change one,
change the other.

## Job-started hook

A job can leave a directory that the provider persists (the pnpm store, the toolcache) in a state that makes
later jobs of the same repository pass without doing their work. The images set
`ACTIONS_RUNNER_HOOK_JOB_STARTED=/opt/garm/hooks/job-started.sh`, so the runner runs that script before the first
step of every job and shows its output in the job log. The script is a dispatcher: it runs every
`/opt/garm/hooks/job-started.d/*.sh` in name order, each as `bash <file>` under a 30 second timeout, and then decides what the job
does. The hook files are root-owned `0755`,
so a job cannot replace them. The step costs a few milliseconds when nothing is wrong.

The shipped check is `10-pnpm-store.sh`. It reads the store from `pnpm_config_store_dir` (else
`npm_config_store_dir`), does nothing if that is unset or not a directory, and reports damage if the store holds a
0-byte pnpm binary, the state that `pnpm/action-setup` before v6.1.0 can leave behind
([`package-store-installs.md`](../docs/package-store-installs.md#pnpm-switching-its-own-version-pnpm-11-before-1122)).

**What happens on damage: `GARM_JOB_HOOKS`.** The provider sets this variable on every runner from the `job_hooks` setting (provider config, overridable per pool in `extra_specs`; [config reference](../docs/config-reference.md#job-hooks-job_hooks)).

| Value | Effect |
|---|---|
| `fail` (default; also unset or empty) | After all checks have run, the hook exits 1. "Set up runner" fails, no step runs, and the job is red. |
| `warn` | The annotations are printed and the job runs. |
| `off` | No check runs. |

An unknown value acts as `fail` and prints a warning that names the value, so a typo cannot switch the checks off quietly. An image built on these, or a runner started without the provider, can set `GARM_JOB_HOOKS` itself (for example with an `ENV` line) and gets the same behaviour. `extra_env` cannot set it, because the `GARM_` prefix is reserved.

**The drop-in contract.** A check is a file named `NN-name.sh` in `job-started.d/`, run as
`bash --noprofile --norc <file>` as the runner user, with the job's environment.

| Exit status | Meaning |
|---|---|
| `0` | Nothing wrong, or nothing to check. |
| `10` | Damage detected. The check has already printed an annotation, for example `::error title=...::<what is wrong, why it matters, how to fix it>`, on one line. |
| anything else, or a timeout | The check itself malfunctioned. The dispatcher prints a `::warning::` that names the check, ignores the result and continues, so a buggy check cannot fail every job. |

The runner executes the dispatcher as `bash --noprofile --norc -e -o pipefail <file>` and ignores its shebang. The
dispatcher guards its own commands for that. A check should stay read-only, cheap and silent when nothing is
wrong, and should treat a missing directory as "nothing to check". Keep every check's output to what the
developer reading the job log needs.

**Adding a check in a derived image.** Copy a file into the directory, root-owned and not writable by the runner
user, and leave the rest of the hook in place:

```dockerfile
FROM ghcr.io/tzuh-hsu/garm-runner-resolute@sha256:...
COPY --chmod=0755 20-my-check.sh /opt/garm/hooks/job-started.d/20-my-check.sh
```

**Replacing the hook.** The path in `ACTIONS_RUNNER_HOOK_JOB_STARTED` must end in `.sh`, `.ps1` or `.js`. A value in
the container's environment (an `ENV` line in a derived image, or a name the operator allowlists in
`extra_env`) replaces ours, and then neither the dispatcher nor the pnpm check runs, unless your hook calls them.
An image built from another base does not carry the hook until it copies `common/hooks/` and sets the variable.

## Build

The build context is this directory, so both Dockerfiles can copy the shared entrypoint from `common/`:

```sh
docker build -t garm-runner-noble:dev    -f runner-images/noble/Dockerfile    runner-images
docker build -t garm-runner-resolute:dev -f runner-images/resolute/Dockerfile runner-images
```

`docker build runner-images/noble` on its own does not work, because that context lacks `common/entrypoint.sh`.
