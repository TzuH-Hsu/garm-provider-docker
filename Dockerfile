# syntax=docker/dockerfile:1
#
# garm-provider-docker provider binary image (M4-W1, plan.md M4 item 1).
#
# THIS IMAGE IS A DELIVERY MECHANISM FOR THE BINARY, NOT A RUNTIME. It is
# not, and cannot be, a way to "run the provider as a container":
#
#   GARM does not start a container for an external provider. It execs the
#   configured `provider_executable` FILESYSTEM PATH directly, handing the
#   request over in environment variables and on stdin
#   (garm-provider-common v0.1.9: `exec.CommandContext(ctx, providerBin)`).
#   An OCI image reference is not a path to an executable, so it can never
#   be a provider_executable, and there is no hook by which GARM would run
#   `docker run` on an operator's behalf. Separately, the distroless
#   nonroot process below (uid 65532) could not open a root:docker host
#   socket even if something did start it.
#
# The image exists so operators can obtain a pinned, reproducible,
# attested build of the binary without a Go toolchain — either extracted
# with `docker create` + `docker cp` onto the path GARM will exec, or
# pulled into their own GARM image with
# `COPY --from=ghcr.io/tzuh-hsu/garm-provider-docker:<tag>`. See the root
# README's "The provider image is a delivery mechanism, not a runtime"
# section for both recipes. A tested `docker run` exec-wrapper could make
# the image usable as a provider_executable in future; none is shipped
# here, deliberately.
#
# Trust-topology note (see runner-images/noble/README.md for the full
# version): the PROVIDER is the trusted party that legitimately needs host
# Docker access to create/destroy the containers, networks, and volumes a
# job needs — it holds that access in GARM's own process context, wherever
# GARM runs. The host socket must NEVER be mounted into a runner or
# DinD-sidecar container: those stay isolated from the host daemon in every
# mode (ADR-001). This image ships no `docker` CLI and has no way to reach
# a socket on its own.

# --platform=$BUILDPLATFORM pins the BUILDER stage to the host's own
# platform, never a target platform. Pure-Go cross-compilation only needs
# GOOS/GOARCH env vars — it does NOT need a target-arch toolchain — so
# without this pin, a multi-platform `--platform linux/amd64,linux/arm64`
# build would instead pull and run the golang:1.25.0 TOOLCHAIN itself under
# QEMU emulation for whichever platform isn't the host's, and `go build`
# (and even `go mod download`) can crash under QEMU user-mode emulation
# (verified locally: the amd64 leg of this stage panics under QEMU on an
# arm64 host without this pin). Pinning the builder to $BUILDPLATFORM keeps
# the compiler itself running natively and fast on every host, with only
# the tiny final distroless stage below actually varying per target
# platform (a plain COPY of a static binary — no emulation needed there).
FROM --platform=$BUILDPLATFORM golang:1.25.0@sha256:5502b0e56fca23feba76dbc5387ba59c593c02ccc2f0f7355871ea9a0852cebe AS builder

WORKDIR /src

# Module download is its own layer, cached independently of source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static cross-compile: CGO_ENABLED=0 plus this repo's pure-Go dependency
# set (no cgo / `import "C"` anywhere in the tree — verified with a repo-wide
# grep) means the resulting binary has no libc dependency, so it runs
# unmodified on the scratch-like distroless/static final stage below.
# TARGETOS/TARGETARCH are populated automatically by buildx for each
# platform in a --platform build (see .github/workflows/images.yml); VERSION
# is the same -ldflags injection point internal/version/version.go documents
# for the standalone release binaries in .github/workflows/release.yml.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=v0.0.0-unknown
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w -X github.com/TzuH-Hsu/garm-provider-docker/internal/version.Version=${VERSION}" \
      -o /out/garm-provider-docker .

# distroless/static: no shell, no package manager, no libc — nothing an
# attacker could pivot to even with code execution inside the binary
# itself. The "nonroot" variant (uid/gid 65532), not the bare "static" tag,
# so the process never runs as UID 0.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

LABEL org.opencontainers.image.source="https://github.com/TzuH-Hsu/garm-provider-docker" \
      org.opencontainers.image.description="GARM external Docker provider binary. Delivery mechanism only: extract the binary (docker cp, or COPY --from) onto the path GARM execs as provider_executable. GARM does not run providers as containers." \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=builder /out/garm-provider-docker /garm-provider-docker

USER nonroot:nonroot
ENTRYPOINT ["/garm-provider-docker"]
