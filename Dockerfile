# syntax=docker/dockerfile:1
#
# Provider binary image. This is a delivery mechanism for the binary, not a
# runtime: GARM execs the configured `provider_executable` file path directly,
# so an image reference cannot be one, and the distroless nonroot process here
# (uid 65532) could not open a root:docker host socket anyway.
#
# Get the binary onto the path GARM execs with `docker create` + `docker cp`,
# or `COPY --from=ghcr.io/tzuh-hsu/garm-provider-docker:<tag>`. See the root
# README for both recipes.
#
# The provider needs host Docker access, and holds it in GARM's own process
# context. Never mount the host socket into a runner or DinD-sidecar
# container. This image ships no `docker` CLI.

# Pin the builder stage to the host platform. Pure-Go cross-compilation needs
# only GOOS/GOARCH, whereas running the Go toolchain under QEMU for the other
# platform can crash (`go build` panics under emulation).
FROM --platform=$BUILDPLATFORM golang:1.25.0@sha256:5502b0e56fca23feba76dbc5387ba59c593c02ccc2f0f7355871ea9a0852cebe AS builder

WORKDIR /src

# Module download is its own layer, cached independently of source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static build: CGO_ENABLED=0 (no cgo in the tree) so the binary runs on the
# distroless/static final stage. TARGETOS/TARGETARCH are set by buildx per
# platform; VERSION is the -ldflags injection point from
# internal/version/version.go.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=v0.0.0-unknown
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w -X github.com/TzuH-Hsu/garm-provider-docker/internal/version.Version=${VERSION}" \
      -o /out/garm-provider-docker .

# The `nonroot` variant (uid/gid 65532), so the process never runs as UID 0.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

LABEL org.opencontainers.image.source="https://github.com/TzuH-Hsu/garm-provider-docker" \
      org.opencontainers.image.description="GARM external Docker provider binary. Delivery mechanism only: extract the binary (docker cp, or COPY --from) onto the path GARM execs as provider_executable. GARM does not run providers as containers." \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=builder /out/garm-provider-docker /garm-provider-docker

USER nonroot:nonroot
ENTRYPOINT ["/garm-provider-docker"]
