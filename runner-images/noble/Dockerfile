# syntax=docker/dockerfile:1
#
# garm-provider-docker runner image (Ubuntu Noble base) - ADR-002.
#
# Digest pin below is the MANIFEST-LIST (index) digest for
# myoung34/github-runner:ubuntu-noble, not a single-platform manifest,
# resolved via:
#   docker buildx imagetools inspect myoung34/github-runner:ubuntu-noble
# M0 pinned the linux/amd64-only platform manifest (plan.md M0 scope); M4-W1
# re-pins to this manifest-list digest — confirmed upstream publishes both
# linux/amd64 and linux/arm64 manifests under it — so buildx's
# .github/workflows/images.yml multi-platform build resolves the correct
# per-platform image automatically instead of needing a build matrix here.
FROM myoung34/github-runner:ubuntu-noble@sha256:2299e3ced2c54ff07d3ca48db09f1e8e5b28d02bafe8007b10ba0b8e060204bf

LABEL org.opencontainers.image.source="https://github.com/TzuH-Hsu/garm-provider-docker" \
      org.opencontainers.image.description="GARM external Docker provider runner image (Ubuntu Noble base, ADR-002 JIT-aware entrypoint)" \
      org.opencontainers.image.licenses="Apache-2.0"

# Commonly needed apt-level dependencies (README "What is in the image"). Runner
# jobs have NO passwordless sudo, so a job cannot apt-get install anything itself:
# system packages must be in the image, while languages and toolchains come from
# the setup-* actions. The base already ships the usual CLI tooling (jq, zip,
# unzip, xz, zstd, rsync, ssh, gpg, git, curl, wget, gh, build-essential, python3
# with venv and pip), so only the verified gaps are added here:
#   - the shared libraries a headless Chrome / Chromium for Testing needs
#     (Puppeteer, Playwright's Chromium and Lighthouse CI download one at job
#     time; the list is the missing set `ldd` reports for Chrome for Testing 155
#     on this base, which is also what Puppeteer documents for Debian/Ubuntu);
#   - fontconfig plus a minimal font set (fonts-liberation, fonts-noto-color-emoji)
#     so pages render text and emoji instead of tofu boxes;
#   - `file`, which the base lacks and build scripts routinely call.
# Every package exists for both amd64 and arm64 (images.yml builds both).
# --no-install-recommends and the apt list cleanup keep the layer small.
RUN set -eux; \
    export DEBIAN_FRONTEND=noninteractive; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
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
      libxrandr2; \
    rm -rf /var/lib/apt/lists/*

# Provide pnpm so W1's persistent pnpm store (npm_config_store_dir=/opt/pnpm-store,
# ADR-003) is actually usable — the myoung34 base ships Node but NOT pnpm, npm, or
# corepack (verified: Node 18 present, npm/corepack absent), so W1's store mount
# was inert without this. PNPM_VERSION's MAJOR must match the provider's
# [cache].pnpm_major (default "9"); bumping the major there is the signal to bump
# this pin too (the pnpm store's on-disk layout is tied to the pnpm major).
#
# Installed lean: the pnpm npm-registry tarball (~20MB of JS) extracted to
# /opt/pnpm and run by the base image's own Node via a tiny wrapper — no bundled
# second Node runtime and no apt npm/build-toolchain bloat. Verified on Docker
# Engine 29.6.1: `pnpm --version` works and, with npm_config_store_dir set,
# `pnpm config get store-dir` resolves to /opt/pnpm-store.
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

# Create the default pnpm store mount point OWNED BY THE RUNNER USER (uid/gid
# 1001) so a fresh named volume mounted here inherits runner ownership: Moby
# copies an image directory's ownership onto an empty volume ONLY when the target
# path already EXISTS in the image. Without this the mount point is absent, the
# fresh volume mounts root-owned 0755, and a real `pnpm install` as the non-root
# runner fails EACCES (H4 — verified on Docker Engine 29.6.1). The entrypoint
# additionally chowns the CONFIGURED store path before the gosu drop, covering an
# operator's custom [cache].pnpm_store_path; this handles the default path lean.
RUN mkdir -p /opt/pnpm-store && chown 1001:1001 /opt/pnpm-store

# Bypass the base image's config.sh-based entrypoint entirely (ADR-002):
# this entrypoint never fetches JIT config itself, it only waits for
# provider-delivered credential files (and, in DinD modes, Docker
# readiness) before execing the runner. No extra packages for M0 - the
# base image already ships everything the entrypoint needs (gosu,
# docker-cli, coreutils).
COPY --chmod=0755 entrypoint.sh /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
# CMD is documentation only: the entrypoint always execs ./run.sh
# directly regardless of "$@" (ADR-002 step 3).
CMD ["./run.sh"]
