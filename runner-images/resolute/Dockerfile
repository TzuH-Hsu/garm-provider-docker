# syntax=docker/dockerfile:1
#
# Runner image, Ubuntu 26.04 Resolute base.
#
# For repositories that need the 26.04 toolchain (GCC 15 as the default
# compiler, LLVM 22). It is published alongside the Noble image, not instead of
# it (runner-images/README.md).
#
# The build context is runner-images/, so the entrypoint shared with the Noble
# image can be copied from runner-images/common/:
#   docker build -f runner-images/resolute/Dockerfile runner-images
#
# The digest pin is the manifest-list (index) digest, not a single-platform
# manifest, so the multi-platform build in images.yml resolves the right image
# per architecture:
#   docker buildx imagetools inspect myoung34/github-runner:ubuntu-resolute
FROM myoung34/github-runner:ubuntu-resolute@sha256:68259be6cff0e40b231ba0f0793de918fca9999f932e3ad2bfa07f17c3b2af86

# garm.docker/os-release is read by the provider at create time and keys the
# persistent toolcache and pnpm store volumes: contents built on one Ubuntu
# release must never be reused on another. Change it only together with the
# FROM line's Ubuntu release.
LABEL org.opencontainers.image.source="https://github.com/TzuH-Hsu/garm-provider-docker" \
      org.opencontainers.image.description="GARM external Docker provider runner image (Ubuntu 26.04 Resolute base, ADR-002 JIT-aware entrypoint)" \
      org.opencontainers.image.licenses="Apache-2.0" \
      garm.docker/os-release="ubuntu-26.04"

# LLVM release for clang-format/clang-tidy. The unversioned packages point at
# LLVM 21, but the archive also ships 22, and repositories that format against
# 22 would see spurious diffs from 21. 22 is installed by its versioned names
# and made the unversioned default below.
ARG LLVM_VERSION=22

# Commonly needed apt packages (see runner-images/README.md), so jobs do not
# apt-get install anything themselves. The base already ships the usual CLI
# tooling; only the gaps are added:
#   - shared libraries for a headless Chrome / Chromium for Testing
#     (Puppeteer, Playwright, Lighthouse CI download one at job time);
#   - fontconfig and a minimal font set, so pages render text and emoji;
#   - `file`, which build scripts call and the base lacks;
#   - pipx, because PEP 668 blocks `pip install --user` on this release;
#   - clang-format and clang-tidy, registered under their unversioned names
#     with update-alternatives as GitHub-hosted images do;
#   - cmake and ninja-build, which GitHub-hosted images carry.
# Keep this list in step with runner-images/noble/Dockerfile. Every package
# must exist for both amd64 and arm64.
RUN set -eux; \
    export DEBIAN_FRONTEND=noninteractive; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      "clang-format-${LLVM_VERSION}" \
      "clang-tidy-${LLVM_VERSION}" \
      cmake \
      file \
      fontconfig \
      fonts-liberation \
      fonts-noto-color-emoji \
      libasound2t64 \
      libatk-bridge2.0-0t64 \
      libatk1.0-0t64 \
      libatspi2.0-0t64 \
      libcairo2 \
      libcups2t64 \
      libdbus-1-3 \
      libgbm1 \
      libnspr4 \
      libnss3 \
      libpango-1.0-0 \
      libx11-6 \
      libxcb1 \
      libxcomposite1 \
      libxdamage1 \
      libxext6 \
      libxfixes3 \
      libxkbcommon0 \
      libxrandr2 \
      ninja-build \
      pipx; \
    for tool in clang-format clang-tidy; do \
      update-alternatives --install "/usr/bin/${tool}" "${tool}" "/usr/bin/${tool}-${LLVM_VERSION}" 100; \
    done; \
    rm -rf /var/lib/apt/lists/*; \
    clang-format --version; \
    clang-tidy --version

# The base ships Node but not pnpm, npm or corepack. PNPM_VERSION's major must
# match the provider's [cache].pnpm_major (default "9"), because the pnpm
# store layout is tied to the major. Same pin as the Noble image. Installed
# lean: the registry tarball is extracted to /opt/pnpm and run by the base
# image's own Node.
ARG PNPM_VERSION=9.15.9
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "${arch}" in \
      amd64|arm64) : ;; \
      *) echo "unsupported architecture ${arch} for the pnpm install" >&2; exit 1 ;; \
    esac; \
    mkdir -p /opt/pnpm; \
    curl -fsSL "https://registry.npmjs.org/pnpm/-/pnpm-${PNPM_VERSION}.tgz" \
      | tar -xz -C /opt/pnpm --strip-components=1; \
    printf '#!/bin/sh\nexec /usr/bin/node /opt/pnpm/dist/pnpm.cjs "$@"\n' > /usr/local/bin/pnpm; \
    chmod 0755 /usr/local/bin/pnpm; \
    pnpm --version

# Create the pnpm store mount point owned by the runner user (uid/gid 1001).
# Docker copies an image directory's ownership onto an empty volume only when
# the path already exists in the image; otherwise the volume mounts root-owned
# and `pnpm install` fails with EACCES. The entrypoint also chowns a custom
# [cache].pnpm_store_path.
RUN mkdir -p /opt/pnpm-store && chown 1001:1001 /opt/pnpm-store

# Let the runner user install tools into /usr/local/bin and /usr/local/lib
# without sudo, as CI scripts written for GitHub-hosted runners expect. Only
# the two directories change owner, not their contents.
RUN chown runner:runner /usr/local/bin /usr/local/lib

# Bypass the base image's config.sh-based entrypoint. The shared entrypoint
# (runner-images/common/entrypoint.sh) never fetches JIT config itself; it
# waits for provider-delivered credential files (and Docker readiness in DinD
# modes) before execing the runner.
COPY --chmod=0755 common/entrypoint.sh /entrypoint.sh

# Job-started hook (ADR-002): the runner runs ACTIONS_RUNNER_HOOK_JOB_STARTED
# before the first step of every job. The dispatcher runs each check in
# job-started.d, and the shipped check fails a job whose pnpm store holds a
# 0-byte pnpm binary instead of letting it pass green without running
# anything. Root-owned and not writable by the runner user, so a job cannot
# replace a hook. The path must end in .sh. A value set in the container's
# environment replaces this one. The provider sets GARM_JOB_HOOKS (fail, warn or
# off) from its job_hooks setting (runner-images/README.md).
COPY --chmod=0755 common/hooks/job-started.sh /opt/garm/hooks/job-started.sh
COPY --chmod=0755 common/hooks/job-started.d/ /opt/garm/hooks/job-started.d/
ENV ACTIONS_RUNNER_HOOK_JOB_STARTED=/opt/garm/hooks/job-started.sh

ENTRYPOINT ["/entrypoint.sh"]
# CMD is documentation only: the entrypoint always execs ./run.sh.
CMD ["./run.sh"]
