# syntax=docker/dockerfile:1
#
# garm-provider-docker runner image (Ubuntu Noble base) - ADR-002.
#
# Digest pin below is the linux/amd64 platform manifest resolved from the
# myoung34/github-runner:ubuntu-noble manifest list via:
#   docker buildx imagetools inspect myoung34/github-runner:ubuntu-noble
# M0 targets linux/amd64 only (plan.md M0 scope); re-pin to a multi-arch
# manifest-list digest (or a per-platform build matrix) at M4 release.
FROM myoung34/github-runner:ubuntu-noble@sha256:24c831c159720f05607ef45e8ca1cd8d59b87d36664275bddc9e38c2e2be9ba5

LABEL org.opencontainers.image.source="https://github.com/TzuH-Hsu/garm-provider-docker" \
      org.opencontainers.image.description="GARM external Docker provider runner image (Ubuntu Noble base, ADR-002 JIT-aware entrypoint)" \
      org.opencontainers.image.licenses="Apache-2.0"

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
