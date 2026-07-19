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
