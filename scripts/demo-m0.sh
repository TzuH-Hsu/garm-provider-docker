#!/usr/bin/env bash
#
# scripts/demo-m0.sh - end-to-end real-job gate for garm-provider-docker.
#
# Points the provider at a real GARM instance and a throwaway test
# repository running a trivial echo workflow, and walks the full
# lifecycle - CreateInstance -> JIT registration -> job runs ->
# DeleteInstance -> runner confirmed gone from GitHub - while checking the
# headline safety property (docs/plan.md §4: no host Docker socket, no
# GARM credentials, no GitHub App key visible on the runner) and the two
# ADR-002 verification items (exec-tar-into-tmpfs credential delivery,
# actions/runner symlink-compat) along the way.
#
# This originated as the M0 go/no-go gate (docs/plan.md §3 M0: pool mode,
# dind_mode=none, linux/amd64 only). M1-M4 have since shipped the full
# per-job topology (job network, named workspace volume, DinD modes,
# persistent caches, extra_specs, v0.1.1 self-description) and a v0.2.0
# release. This script now validates that release end to end; it keeps
# dind_mode=none as its own default (the smallest, most portable slice -
# DinD modes are a deliberate operator opt-in and out of scope for this
# gate, see docs/demo-m0.md), but everything else it exercises is the
# CURRENT provider, not the M0 slice.
#
# See docs/demo-m0.md for the full runbook: prerequisites, what each check
# proves, and a troubleshooting table. This script only prints instructions
# and runs read-mostly checks; it never modifies GARM's own config or your
# GitHub repository for you - those steps are always a manual, confirmed
# action, by design.
#
# Two modes (see PROVIDER_SOURCE below):
#   release    (default) Downloads the published vX.Y.Z release binary for
#               this host's OS/arch, verifies it against the release's
#               SHA256SUMS, and pins runner_image to the published
#               ghcr.io/tzuh-hsu/garm-runner-noble image BY DIGEST
#               (resolved at run time via `docker buildx imagetools
#               inspect`) - so this run validates exactly what a real
#               operator installs, with digest-pin enforcement left ON
#               (no allow_unpinned_runner_image).
#   source     Builds the provider binary and the runner image from this
#               checkout (the original M0-era behavior): a local `go
#               build`, a local `docker build runner-images/noble`, and
#               allow_unpinned_runner_image=true (required for a mutable
#               local tag). For development against uncommitted changes.
#
# Two dispatch targets (see GARM_TARGET below):
#   scaleset   (default) Creates a GARM SCALE SET: GARM long-polls GitHub's
#               message queue for jobs, so NO inbound webhook is needed and
#               the GARM host can sit behind a NAT. The workflow targets the
#               scale set by NAME in `runs-on`. Provider JIT delivery is
#               forced on for scale sets (docs/research.md §1.D).
#   pool       Creates a webhook-driven pool (the original flow). Needs
#               GitHub to be able to reach GARM's webhook endpoint.
#
# Two topology variants (see GATE_VARIANT below):
#   none       (default) dind_mode=none: the smallest, most portable slice.
#   dind       Optional: allowed_dind_modes=["none","privileged-sidecar"], a
#               "dind" flavor (runner 6GiB / dind 4GiB), extra_specs that
#               select privileged-sidecar, and a test workflow that runs
#               `docker run` and `docker build` inside the job. NOTE:
#               privileged-sidecar runs a Privileged=true daemon on this
#               host (ADR-001 "Residual risk") - use a disposable VM.
#
# Assumptions:
#   - You are on a Linux Docker host (or macOS + Docker Desktop),
#     linux/amd64 or linux/arm64.
#   - A GARM server is ALREADY RUNNING and reachable, with garm-cli
#     installed and authenticated against it (docs/demo-m0.md).
#   - You have already registered a throwaway GitHub repository with GARM
#     (`garm-cli repo add ...`) to point the demo pool at.
#
# Secrets: this script never reads, generates, or prints a GitHub PAT/App
# key, a GARM API token, or a runner bearer token. garm-cli's own stored
# profile supplies GARM authentication; nothing sensitive is ever passed on
# argv or committed to an env var this script echoes.
#
# Usage:
#   scripts/demo-m0.sh [-y|--yes] [--from-source] [--version vX.Y.Z] [--pool] [--dind] [COMMAND]
#
# COMMAND (default: all):
#   preflight   Step 0: environment checks; obtain the provider binary +
#               runner image (release: download+verify / resolve digest;
#               source: build both locally); generate a demo provider
#               config.toml; smoke-test the binary against it
#   config      Step 1: print the config.toml [[provider]] snippet to
#               register with GARM, and pause for confirmation
#   pool        Step 2: print the garm-cli scaleset add (or, with --pool,
#               pool add) command for your test repo, and pause for
#               confirmation
#   watch       Step 3: watch docker ps / garm-cli runner list; print the
#               sample workflow yaml to trigger
#   verify      Step 4 (a-d): PASS/FAIL checks against the live runner
#               container, job network, and workspace volume
#   postjob     Step 4 (e): PASS/FAIL checks after the job has finished
#   cleanup     Print manual cleanup commands
#   all         Run every step above, in order
#
# Flags:
#   -y, --yes       Auto-confirm every pause (non-interactive)
#   --from-source   Build the provider binary and runner image from this
#                   checkout instead of using a published release
#                   (equivalent to PROVIDER_SOURCE=source)
#   --version vX.Y.Z
#                   Release to validate in the default "release" mode
#                   (equivalent to PROVIDER_VERSION=vX.Y.Z). Ignored with
#                   --from-source. Default: v0.2.1
#   --pool          Dispatch via a webhook-driven pool instead of a scale set
#                   (equivalent to GARM_TARGET=pool)
#   --dind          Run the DinD variant (privileged-sidecar) instead of
#                   dind_mode=none (equivalent to GATE_VARIANT=dind)
#   -h, --help      Show usage
#
# Environment variables (all optional unless noted - see docs/demo-m0.md):
#   GARM_URL              Base URL of the running GARM server. Informational
#                          only (Step 0 reachability probe); garm-cli itself
#                          authenticates via its own stored profile.
#                          Default: http://localhost:8080
#   REPO                  GARM repo identifier (UUID from `garm-cli repo
#                          list`, or "owner/name") of the THROWAWAY test
#                          repository. Required for pool/watch/all.
#   PROVIDER_SOURCE       "release" (default) or "source" - see the two
#                          modes above. --from-source sets this to "source".
#   PROVIDER_VERSION      The vX.Y.Z release to download and validate in
#                          "release" mode (also selects the
#                          ghcr.io/tzuh-hsu/garm-runner-noble tag whose
#                          digest gets resolved). Ignored in "source" mode.
#                          Default: v0.2.1
#   GARM_TARGET           "scaleset" (default) or "pool" - see above.
#   GATE_VARIANT          "none" (default) or "dind" - see above.
#   SCALESET_NAME         Scale set name (= the workflow's runs-on value).
#                          Default: garm-docker-gate-<6 random hex>, generated
#                          once and remembered in the config dir so every
#                          step agrees on it. Scale set labels/names cannot
#                          be changed after creation.
#   SCALESET_ID           GARM's scale set ID, for `scaleset runner list` and
#                          cleanup. Looked up by name from `garm-cli scaleset
#                          list` when unset.
#   DIND_IMAGE_SOURCE     Mutable reference whose digest is resolved and
#                          pinned as dind_image in the dind variant.
#                          Default: docker:dind
#   PROVIDER_NAME         Name this provider is registered under in GARM's
#                          config.toml [[provider]] section.
#                          Default: docker_demo
#   PROVIDER_INTERFACE_VERSION
#                         interface_version printed in Step 1's [[provider]]
#                          snippet: "v0.1.0" (default) or "v0.1.1". Keep
#                          v0.1.0 on GARM <= v0.2.1 (see step_config and
#                          docs/demo-m0.md); switch to v0.1.1 once a GARM
#                          release includes cloudbase/garm#878.
#   PROVIDER_BIN_DIR      Directory the provider binary is downloaded/built
#                          into. Default: <repo>/bin
#   PROVIDER_CONFIG_DIR   Directory the generated demo provider config.toml
#                          is written into. Default: ${TMPDIR:-/tmp}/garm-provider-docker-demo
#   RUNNER_IMAGE_TAG      Local tag for the runner image build. "source"
#                          mode only. Default: garm-runner-noble:demo
#   OS_ARCH               Runner OS architecture: "amd64" or "arm64".
#                          Auto-detected from `uname -m` when unset; also
#                          selects which release binary asset is
#                          downloaded in "release" mode.
#   POOL_TAGS             Comma-separated tags applied to the pool; matched
#                          by the sample workflow's runs-on.
#                          Default: docker,garm-demo
#   POOL_IMAGE, POOL_FLAVOR
#                          Values for garm-cli pool add's required --image/
#                          --flavor flags. Informational only for this
#                          provider (see Step 2's own note).
#   WATCH_INTERVAL_SECONDS, WATCH_MAX_ITERATIONS
#                          Step 3 poll cadence and bounded loop length
#                          (0 = run until Ctrl+C). Defaults: 5, 120.
#   POST_JOB_WAIT_SECONDS Step 4(e) bounded wait for GARM's DeleteInstance
#                          to remove the runner container. Default: 300.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# --- Configuration (env-overridable) ----------------------------------------

GARM_URL="${GARM_URL:-http://localhost:8080}"
REPO="${REPO:-}"

PROVIDER_SOURCE="${PROVIDER_SOURCE:-release}"
PROVIDER_VERSION="${PROVIDER_VERSION:-v0.2.1}"

PROVIDER_NAME="${PROVIDER_NAME:-docker_demo}"
PROVIDER_INTERFACE_VERSION="${PROVIDER_INTERFACE_VERSION:-v0.1.0}"

PROVIDER_BIN_DIR="${PROVIDER_BIN_DIR:-${REPO_ROOT}/bin}"
PROVIDER_BIN="${PROVIDER_BIN_DIR}/garm-provider-docker"

# Fixed (but overridable) default so re-running this script points GARM at
# the same provider config path every time - see docs/demo-m0.md.
PROVIDER_CONFIG_DIR="${PROVIDER_CONFIG_DIR:-${TMPDIR:-/tmp}/garm-provider-docker-demo}"
PROVIDER_CONFIG_FILE="${PROVIDER_CONFIG_DIR}/config.toml"

# "source" mode only: a mutable local dev tag, which is why "source" mode
# needs allow_unpinned_runner_image=true in the generated config (ADR-002/
# ADR-005 F10; internal/config/config.go's validateDigestPinnedImage).
# "release" mode never uses this - it pins runner_image by digest instead.
RUNNER_IMAGE_TAG="${RUNNER_IMAGE_TAG:-garm-runner-noble:demo}"

# The published runner image "release" mode resolves a digest for.
RUNNER_IMAGE_REPO="ghcr.io/tzuh-hsu/garm-runner-noble"

# The GitHub release this repo's own release.yml publishes assets to.
RELEASE_REPO_SLUG="TzuH-Hsu/garm-provider-docker"
RELEASE_BASE_URL="https://github.com/${RELEASE_REPO_SLUG}/releases/download"

POOL_TAGS="${POOL_TAGS:-docker,garm-demo}"

# Dispatch target and topology variant (see the header comment).
GARM_TARGET="${GARM_TARGET:-scaleset}"
GATE_VARIANT="${GATE_VARIANT:-none}"
SCALESET_NAME="${SCALESET_NAME:-}"
SCALESET_ID="${SCALESET_ID:-}"

# DinD variant only. dind_image must be digest-pinned like runner_image
# (internal/config/dind.go's validateDindMode), so a mutable reference is
# resolved to name@sha256:<digest> at preflight. The "dind" flavor carries the
# memory sizing; the extra_specs below select it plus privileged-sidecar
# (field names per internal/extraspecs/schema.json; privileged-sidecar must be
# within the config's allowed_dind_modes ceiling, which the generated config
# widens to exactly ["none", "privileged-sidecar"]).
DIND_IMAGE_SOURCE="${DIND_IMAGE_SOURCE:-docker:dind}"
readonly DIND_FLAVOR_NAME=dind
readonly DIND_RUNNER_MEMORY=6GiB
readonly DIND_DIND_MEMORY=4GiB
readonly DIND_EXTRA_SPECS='{"flavor":"dind","dind_mode":"privileged-sidecar"}'

# --image/--flavor are REQUIRED by `garm-cli pool add` but are provider-
# specific and, for THIS provider, purely informational: CreateInstance
# always uses the config's (or flavor's) runner_image and ignores
# bootstrap.Image entirely (internal/provider/create.go's ensureImage).
POOL_IMAGE="${POOL_IMAGE:-unused-by-this-provider}"
POOL_FLAVOR="${POOL_FLAVOR:-default}"

# Fixed by design, not env-overridable: this gate only needs one runner to
# ever exist at a time. OS_TYPE is fixed to linux - the only OS this
# provider supports (internal/provider/create.go's validatePlatform). Both
# linux/amd64 and linux/arm64 are supported as of M1; OS_ARCH below is
# auto-detected (or operator-overridden), not hardcoded to amd64.
readonly MIN_IDLE_RUNNERS=0
readonly MAX_RUNNERS=1
readonly OS_TYPE=linux

WATCH_INTERVAL_SECONDS="${WATCH_INTERVAL_SECONDS:-5}"
WATCH_MAX_ITERATIONS="${WATCH_MAX_ITERATIONS:-120}"
POST_JOB_WAIT_SECONDS="${POST_JOB_WAIT_SECONDS:-300}"

ASSUME_YES=0
VERIFY_FAILED=0

# --- Small helpers -----------------------------------------------------------

log()  { printf '[demo] %s\n' "$*" >&2; }
warn() { printf '[demo][WARN] %s\n' "$*" >&2; }
die()  { printf '[demo][ERROR] %s\n' "$*" >&2; exit 1; }
ok()   { printf '  PASS  %s\n' "$*"; }
bad()  { printf '  FAIL  %s\n' "$*"; VERIFY_FAILED=1; }

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command '$1' not found on PATH"
}

confirm() {
  local prompt="$1"
  log "${prompt}"
  if [[ "${ASSUME_YES}" == "1" ]]; then
    log "auto-confirmed (--yes)"
    return 0
  fi
  local _confirm_input
  read -r -p "[demo] Press Enter to continue once done (Ctrl+C to abort)... " _confirm_input || true
}

# tags_to_yaml_array turns a comma-separated tag list ("docker,garm-demo")
# into a YAML flow-sequence ("[docker, garm-demo]") for the sample
# workflow's runs-on.
tags_to_yaml_array() {
  local csv="$1" out="[" first=1 tag
  local -a parts
  IFS=',' read -ra parts <<< "${csv}"
  for tag in "${parts[@]}"; do
    if [[ "${first}" -eq 1 ]]; then
      out+="${tag}"
      first=0
    else
      out+=", ${tag}"
    fi
  done
  out+="]"
  printf '%s' "${out}"
}

require_repo() {
  [[ -n "${REPO}" ]] || die "REPO is not set. export REPO=<garm-repo-id-or-owner/name> (see 'garm-cli repo list') and re-run."
}

# validate_modes rejects an unknown GARM_TARGET / GATE_VARIANT /
# PROVIDER_INTERFACE_VERSION early.
validate_modes() {
  case "${PROVIDER_INTERFACE_VERSION}" in
    v0.1.0|v0.1.1) ;;
    *) die "PROVIDER_INTERFACE_VERSION must be 'v0.1.0' or 'v0.1.1', got '${PROVIDER_INTERFACE_VERSION}'" ;;
  esac
  case "${GARM_TARGET}" in
    scaleset|pool) ;;
    *) die "GARM_TARGET must be 'scaleset' or 'pool', got '${GARM_TARGET}'" ;;
  esac
  case "${GATE_VARIANT}" in
    none|dind) ;;
    *) die "GATE_VARIANT must be 'none' or 'dind', got '${GATE_VARIANT}'" ;;
  esac
}

# resolve_scaleset_name fixes SCALESET_NAME for this run: the operator's
# value, else one remembered from an earlier step in the config dir, else a
# fresh garm-docker-gate-<6 hex>. It is persisted so `pool`, `watch` and
# `cleanup` run as separate invocations all agree (scale set names - and
# labels - cannot be changed after creation, and the workflow's runs-on must
# match exactly).
resolve_scaleset_name() {
  [[ -z "${SCALESET_NAME}" ]] || return 0
  local f="${PROVIDER_CONFIG_DIR}/scaleset-name"
  if [[ -s "${f}" ]]; then
    SCALESET_NAME="$(cat "${f}")"
    return 0
  fi
  mkdir -p "${PROVIDER_CONFIG_DIR}" || die "could not create ${PROVIDER_CONFIG_DIR}"
  SCALESET_NAME="garm-docker-gate-$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')"
  printf '%s\n' "${SCALESET_NAME}" > "${f}"
}

# lookup_scaleset_id fills SCALESET_ID (when unset) by finding this scale
# set's row in `garm-cli scaleset list --repo` and taking its ID column.
# GARM v0.2.x scale-set IDs are small integers (the table's first column,
# `ID`), not UUIDs, so the row is matched on the EXACT value of the
# `Scale Set Name` column (a substring match could hit another scale set
# whose name merely contains this one) and the ID must be all digits.
# Best-effort: leaves SCALESET_ID empty when the scale set is not there yet
# (callers handle that).
lookup_scaleset_id() {
  [[ -z "${SCALESET_ID}" ]] || return 0
  SCALESET_ID="$(garm-cli scaleset list --repo "${REPO}" 2>/dev/null \
    | awk -F'|' -v name="${SCALESET_NAME}" '
        {
          id = $2; n = $3
          gsub(/^[ \t]+|[ \t]+$/, "", id)
          gsub(/^[ \t]+|[ \t]+$/, "", n)
          if (n == name && id ~ /^[0-9]+$/) { print id; exit }
        }')" || true
}

# detect_arch maps `uname -m` to the amd64/arm64 vocabulary this provider,
# GARM's --os-arch flag, and this repo's release-asset naming all use.
# OS_ARCH can still be set explicitly (env) to override the detected value.
detect_arch() {
  local machine
  machine="$(uname -m)"
  case "${machine}" in
    x86_64|amd64)   printf 'amd64' ;;
    aarch64|arm64)  printf 'arm64' ;;
    *) die "unsupported host architecture '${machine}': this provider only supports linux/amd64 and linux/arm64 (internal/provider/create.go's supportedOSArches)" ;;
  esac
}
OS_ARCH="${OS_ARCH:-$(detect_arch)}"

# require_linux_host_for_release_mode fails closed, with a clear next step,
# when PROVIDER_SOURCE=release is requested on a non-Linux host: this repo's
# release.yml build matrix publishes linux/amd64 and linux/arm64 binaries
# ONLY (no darwin build), because the provider binary is meant to run as a
# HOST process wherever GARM itself runs (see the root README's "The
# provider image is a delivery mechanism, not a runtime" section) - macOS +
# Docker Desktop is a supported DOCKER HOST for the containers this provider
# creates, but there is no macOS release asset for the provider binary
# itself. Verified empirically: downloading and running the linux/arm64
# release asset directly on a macOS host fails with "cannot execute binary
# file" (wrong OS for the ELF), which is exactly what this check heads off
# with an actionable message instead of an opaque exec error.
require_linux_host_for_release_mode() {
  local os
  os="$(uname -s)"
  if [[ "${os}" != "Linux" ]]; then
    die "PROVIDER_SOURCE=release has no published binary for this host OS ('${os}') - only linux/amd64 and linux/arm64 are built and released. Use --from-source instead (it builds a native binary for THIS host with 'go build', which is how a non-Linux dev host is expected to run this gate), or run this gate from a Linux host/VM that can reach this Docker daemon."
  fi
}

# compute_sha256 prints the lowercase hex sha256 of file "$1", preferring
# sha256sum (Linux) and falling back to shasum -a 256 (macOS).
compute_sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -- "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 -- "$1" | awk '{print $1}'
  else
    die "neither sha256sum nor shasum is on PATH; cannot verify the release download's checksum"
  fi
}

# --- Step 0: preflight -------------------------------------------------------

build_provider_binary() {
  log "Building provider binary from source (go build -ldflags version stamp)..."
  mkdir -p "${PROVIDER_BIN_DIR}" || die "could not create ${PROVIDER_BIN_DIR}"
  local version
  version="$(git -C "${REPO_ROOT}" describe --tags --always --dirty 2>/dev/null || echo dev)"
  if ! ( cd "${REPO_ROOT}" && go build \
      -ldflags "-X github.com/TzuH-Hsu/garm-provider-docker/internal/version.Version=${version}" \
      -o "${PROVIDER_BIN}" . ); then
    die "go build failed"
  fi
  log "Built ${PROVIDER_BIN} (version ${version})"
}

# check_release_published fails early, before any download, when the
# PROVIDER_VERSION release has no published SHA256SUMS asset (an HTTP HEAD
# request; no GitHub token or gh CLI needed).
check_release_published() {
  if ! curl -fsSIL -o /dev/null "${RELEASE_BASE_URL}/${PROVIDER_VERSION}/SHA256SUMS"; then
    die "release ${PROVIDER_VERSION} not published yet (or unreachable: ${RELEASE_BASE_URL}/${PROVIDER_VERSION}/SHA256SUMS) - use --from-source to build from this checkout"
  fi
}

# warn_if_known_incompatible_version warns (never blocks) when a release below
# v0.2.1 is combined with scale-set dispatch: those releases are known to
# fail against GARM v0.2.x scale sets (CreateInstance rejects GARM's injected
# runner_install_template; Get/DeleteInstance fail with "missing pool ID").
# Non vX.Y.Z version strings are not compared.
warn_if_known_incompatible_version() {
  [[ "${GARM_TARGET}" == "scaleset" ]] || return 0
  local v="${PROVIDER_VERSION#v}" major minor patch
  [[ "${v}" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]] || return 0
  major="${BASH_REMATCH[1]}"; minor="${BASH_REMATCH[2]}"; patch="${BASH_REMATCH[3]}"
  if (( major == 0 && (minor < 2 || (minor == 2 && patch < 1)) )); then
    warn "provider ${PROVIDER_VERSION} is known to FAIL against GARM v0.2.x scale sets (runner_install_template rejected; Get/DeleteInstance 'missing pool ID'). Use v0.2.1 or later, or --from-source. Continuing anyway (older GARM or --pool may still work)."
  fi
}

# download_provider_binary fetches the PROVIDER_VERSION release's binary
# asset for this host's OS_ARCH plus its SHA256SUMS file, and verifies the
# binary against the matching SHA256SUMS line before it is ever executed -
# failing hard, not just warning, on any mismatch or missing entry. Asset
# naming matches .github/workflows/release.yml's
# "garm-provider-docker_<version>_<goos>_<goarch>" convention exactly.
download_provider_binary() {
  require_cmd curl
  mkdir -p "${PROVIDER_BIN_DIR}" || die "could not create ${PROVIDER_BIN_DIR}"

  check_release_published
  warn_if_known_incompatible_version

  local asset="garm-provider-docker_${PROVIDER_VERSION}_linux_${OS_ARCH}"
  local asset_path="${PROVIDER_BIN_DIR}/${asset}"
  local sums_path="${PROVIDER_BIN_DIR}/SHA256SUMS"

  log "Downloading ${asset} (release ${PROVIDER_VERSION})..."
  curl -fsSL -o "${asset_path}" "${RELEASE_BASE_URL}/${PROVIDER_VERSION}/${asset}" \
    || die "failed to download ${asset} from the ${PROVIDER_VERSION} release - check PROVIDER_VERSION and OS_ARCH, and that this host can reach github.com"
  curl -fsSL -o "${sums_path}" "${RELEASE_BASE_URL}/${PROVIDER_VERSION}/SHA256SUMS" \
    || die "failed to download SHA256SUMS from the ${PROVIDER_VERSION} release"

  local expected
  expected="$(awk -v f="${asset}" '$2 == f { print $1; found=1 } END { exit !found }' "${sums_path}")" \
    || die "SHA256SUMS from release ${PROVIDER_VERSION} has no entry for ${asset} - refusing to trust an unverifiable download"

  local actual
  actual="$(compute_sha256 "${asset_path}")"
  if [[ "${expected}" != "${actual}" ]]; then
    rm -f "${asset_path}"
    die "checksum mismatch for ${asset}: SHA256SUMS says ${expected}, downloaded file hashes to ${actual} - refusing to use a corrupted or tampered binary"
  fi
  ok "SHA256SUMS verified for ${asset} (${expected})"

  mv -f "${asset_path}" "${PROVIDER_BIN}"
  chmod +x "${PROVIDER_BIN}"
  log "Installed verified binary: ${PROVIDER_BIN}"
}

build_runner_image() {
  log "Building runner image ${RUNNER_IMAGE_TAG} from runner-images/noble ..."
  docker build -t "${RUNNER_IMAGE_TAG}" "${REPO_ROOT}/runner-images/noble" \
    || die "docker build of the runner image failed"
}

# resolve_runner_image_digest resolves ghcr.io/tzuh-hsu/garm-runner-noble's
# PROVIDER_VERSION tag to its current manifest-list digest via `docker
# buildx imagetools inspect` (no pull required) and prints the resulting
# name@sha256:<digest> reference. This is the SAME digest for every
# platform (it names the multi-arch index; the daemon selects
# linux/amd64 vs linux/arm64 at pull time), so it is resolved once
# regardless of OS_ARCH.
resolve_runner_image_digest() {
  require_cmd docker
  local ref="${RUNNER_IMAGE_REPO}:${PROVIDER_VERSION}"
  log "Resolving digest for ${ref} (docker buildx imagetools inspect)..." >&2
  local digest
  digest="$(docker buildx imagetools inspect "${ref}" --format '{{.Manifest.Digest}}' 2>/dev/null)" \
    || die "failed to resolve a digest for ${ref} - is 'docker buildx' available, and can this host reach ghcr.io?"
  [[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]] \
    || die "unexpected digest format from 'docker buildx imagetools inspect ${ref}': '${digest}'"
  log "Resolved ${ref} -> ${digest}" >&2
  printf '%s@%s' "${RUNNER_IMAGE_REPO}" "${digest}"
}

# resolve_dind_image_digest resolves DIND_IMAGE_SOURCE (default docker:dind,
# a mutable tag) to name:tag@sha256:<digest> so the generated config's
# dind_image is digest-pinned exactly like runner_image.
resolve_dind_image_digest() {
  require_cmd docker
  log "Resolving digest for ${DIND_IMAGE_SOURCE} (docker buildx imagetools inspect)..." >&2
  local digest
  digest="$(docker buildx imagetools inspect "${DIND_IMAGE_SOURCE}" --format '{{.Manifest.Digest}}' 2>/dev/null)" \
    || die "failed to resolve a digest for ${DIND_IMAGE_SOURCE} - can this host reach its registry?"
  [[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]] \
    || die "unexpected digest format from 'docker buildx imagetools inspect ${DIND_IMAGE_SOURCE}': '${digest}'"
  log "Resolved ${DIND_IMAGE_SOURCE} -> ${digest}" >&2
  printf '%s@%s' "${DIND_IMAGE_SOURCE}" "${digest}"
}

# generate_provider_config writes the demo provider config.toml. In
# "release" mode it pins runner_image by digest and leaves digest-pin
# enforcement ON (no allow_unpinned_runner_image) - this is meant to be
# exactly what a real operator's minimal config looks like (see
# examples/config.minimal.toml). In "source" mode it points at the local
# mutable dev tag, which requires the dev-only escape hatch.
generate_provider_config() {
  local runner_image="$1" dind_image="${2:-}"
  mkdir -p "${PROVIDER_CONFIG_DIR}" || die "could not create ${PROVIDER_CONFIG_DIR}"

  if [[ "${PROVIDER_SOURCE}" == "release" ]]; then
    cat > "${PROVIDER_CONFIG_FILE}" <<EOF
# Generated by scripts/demo-m0.sh (release mode, ${PROVIDER_VERSION}) -
# gate config for garm-provider-docker. Every key besides runner_image
# falls back to its documented default (docs/config-reference.md):
# dind_mode = "none", docker_host = "unix:///var/run/docker.sock", and so
# on - see examples/config.minimal.toml, which this mirrors.
#
# runner_image is pinned by digest, resolved from
# ${RUNNER_IMAGE_REPO}:${PROVIDER_VERSION} at preflight
# time - digest-pin enforcement stays ON (no allow_unpinned_runner_image).
runner_image = "${runner_image}"
EOF
  else
    cat > "${PROVIDER_CONFIG_FILE}" <<EOF
# Generated by scripts/demo-m0.sh (--from-source mode) - dev/CI gate
# config ONLY, do not use in production.
#
# allow_unpinned_runner_image=true is a dev-only escape hatch
# (internal/config/config.go; ADR-002/ADR-005 F10): it lets runner_image be
# a mutable local tag instead of a sha256-digest-pinned reference. Never set
# this in a real deployment.
docker_host = "unix:///var/run/docker.sock"
runner_image = "${runner_image}"
allow_unpinned_runner_image = true
EOF
  fi
  if [[ "${GATE_VARIANT}" == "dind" ]]; then
    [[ -n "${dind_image}" ]] || die "internal error: dind variant requires a resolved dind_image"
    # Top-level keys must precede any table in TOML; the base config above
    # has no tables, so appending keys and then the flavor table is valid.
    cat >> "${PROVIDER_CONFIG_FILE}" <<EOF

# DinD variant (--dind): widen the operator ceiling so a pool/scale set's
# extra_specs MAY select privileged-sidecar (the default ceiling is
# ["none"], fail-closed). dind_mode itself stays at its default "none";
# only the scale set/pool below opts in, via extra_specs. dind_image is
# digest-pinned. privileged-sidecar runs a Privileged=true daemon on this
# host (ADR-001 "Residual risk") - disposable hosts only.
allowed_dind_modes = ["none", "privileged-sidecar"]
dind_image = "${dind_image}"

[flavors.${DIND_FLAVOR_NAME}]
  runner_memory = "${DIND_RUNNER_MEMORY}"
  dind_memory = "${DIND_DIND_MEMORY}"
EOF
  fi
  log "Wrote provider config: ${PROVIDER_CONFIG_FILE}"
}

# smoke_test_binary runs the obtained binary once with GARM_COMMAND=GetVersion
# against the just-generated config, so a broken download (wrong arch, a
# corrupted binary that passed the checksum check for the wrong reason -
# it should not, but this is cheap insurance) or a config the CURRENT
# loader rejects surfaces here, immediately, rather than opaquely inside
# GARM's own provider-subprocess log later.
smoke_test_binary() {
  log "Smoke-testing the provider binary (GARM_COMMAND=GetVersion) against the generated config..."
  # main.go writes its structured log lines to stderr and the GetVersion
  # result (a bare version string) to stdout (see main.go's run()), so stdout
  # alone is the clean "reported version" value; stderr is captured
  # separately and surfaced only on failure, for a readable error.
  local out err rc=0
  out="$(GARM_COMMAND=GetVersion \
      GARM_CONTROLLER_ID=demo-preflight \
      GARM_PROVIDER_CONFIG_FILE="${PROVIDER_CONFIG_FILE}" \
      "${PROVIDER_BIN}" 2>"${PROVIDER_CONFIG_DIR}/.smoke-test-stderr")" || rc=$?
  err="$(cat "${PROVIDER_CONFIG_DIR}/.smoke-test-stderr" 2>/dev/null)"
  rm -f "${PROVIDER_CONFIG_DIR}/.smoke-test-stderr"
  if [[ "${rc}" -ne 0 ]]; then
    die "the provider binary failed GetVersion against the generated config (exit ${rc}): ${err}"
  fi
  ok "provider binary runs and loads the generated config (reported version: ${out})"

  if [[ "${GATE_VARIANT}" == "dind" ]]; then
    # Same Parse+Resolve path CreateInstance runs (internal/provider/v011.go's
    # ValidatePoolInfo): proves the exact extra_specs this gate will attach
    # is accepted by the generated config (flavor exists, privileged-sidecar
    # is within allowed_dind_modes) BEFORE any Docker work.
    local vrc=0
    err="$(GARM_INTERFACE_VERSION=v0.1.1 GARM_COMMAND=ValidatePoolInfo \
        GARM_CONTROLLER_ID=demo-preflight \
        GARM_PROVIDER_CONFIG_FILE="${PROVIDER_CONFIG_FILE}" \
        GARM_POOL_EXTRASPECS="${DIND_EXTRA_SPECS}" \
        "${PROVIDER_BIN}" 2>&1 </dev/null)" || vrc=$?
    if [[ "${vrc}" -ne 0 ]]; then
      die "the provider rejected the DinD extra_specs ${DIND_EXTRA_SPECS} (exit ${vrc}): ${err}"
    fi
    ok "provider accepts the DinD extra_specs ${DIND_EXTRA_SPECS} (ValidatePoolInfo)"
  fi
}

step_preflight() {
  log "=== Step 0: preflight (source: ${PROVIDER_SOURCE}, target: ${GARM_TARGET}, variant: ${GATE_VARIANT}, arch: ${OS_ARCH}) ==="

  require_cmd docker

  if docker info >/dev/null 2>&1; then
    ok "docker is reachable"
  else
    die "docker is not reachable - is the daemon running, and is this user allowed to talk to it (e.g. in the docker group)?"
  fi

  if command -v garm-cli >/dev/null 2>&1; then
    ok "garm-cli is on PATH"
  else
    die "garm-cli not found on PATH. Install it per the official GARM docs: https://github.com/cloudbase/garm/blob/main/doc/quickstart-docker.md#6-install-garm-cli"
  fi

  if garm-cli controller show >/dev/null 2>&1; then
    ok "garm-cli appears authenticated against a GARM controller"
  else
    die "garm-cli is not authenticated (or has no active profile). Run 'garm-cli init ...' the first time, or 'garm-cli profile add/switch ...' for an existing controller: https://github.com/cloudbase/garm/blob/main/doc/quickstart-docker.md#7-initialize-garm"
  fi

  if command -v curl >/dev/null 2>&1; then
    if curl -sS -o /dev/null -m 5 "${GARM_URL}"; then
      ok "GARM_URL (${GARM_URL}) responded (informational probe only)"
    else
      warn "could not reach GARM_URL (${GARM_URL}) directly - this probe is informational only; garm-cli's own profile is what actually matters for every other step"
    fi
  else
    warn "curl not found; skipping the GARM_URL reachability probe"
  fi

  local runner_image
  case "${PROVIDER_SOURCE}" in
    release)
      require_linux_host_for_release_mode
      download_provider_binary
      runner_image="$(resolve_runner_image_digest)"
      ;;
    source)
      require_cmd go
      build_provider_binary
      build_runner_image
      runner_image="${RUNNER_IMAGE_TAG}"
      ;;
    *)
      die "PROVIDER_SOURCE must be 'release' or 'source', got '${PROVIDER_SOURCE}'"
      ;;
  esac

  local dind_image=""
  if [[ "${GATE_VARIANT}" == "dind" ]]; then
    dind_image="$(resolve_dind_image_digest)"
  fi

  generate_provider_config "${runner_image}" "${dind_image}"
  smoke_test_binary

  log "Preflight complete."
  log "  Mode            : ${PROVIDER_SOURCE}"
  log "  Provider binary : ${PROVIDER_BIN}"
  log "  Provider config : ${PROVIDER_CONFIG_FILE}"
  log "  Runner image    : ${runner_image}"
  [[ -z "${dind_image}" ]] || log "  DinD image      : ${dind_image}"
}

# --- Step 1: register the provider with GARM --------------------------------

step_config() {
  log "=== Step 1: register the provider with GARM ==="
  log "Add the following [[provider]] section to GARM's own config.toml"
  log "(commonly /etc/garm/config.toml), then restart or reload GARM:"
  cat <<EOF

[[provider]]
  name = "${PROVIDER_NAME}"
  provider_type = "external"
  description = "garm-provider-docker real-job gate (${PROVIDER_SOURCE} ${PROVIDER_VERSION})"
  disable_jit_config = false
  [provider.external]
    provider_executable = "${PROVIDER_BIN}"
    config_file = "${PROVIDER_CONFIG_FILE}"
    interface_version = "${PROVIDER_INTERFACE_VERSION}"

EOF
  log "interface_version defaults to v0.1.0 here (override with"
  log "PROVIDER_INTERFACE_VERSION=v0.1.1). On GARM <= v0.2.1, GARM's own v0.1.1"
  log "ListInstances wrapper inverts its error check (if err == nil in"
  log "runner/providers/v0.1.1/external.go), so every successful list is"
  log "reported as 'returned error: %!s(<nil>)' and GARM's runner"
  log "reconciliation keeps failing. Fixed upstream by cloudbase/garm#878"
  log "(commit 488cf98b), not in any GARM release as of 2026-10-11. GARM v0.2.1"
  log "never calls the v0.1.1-only self-description methods, so v0.1.0 loses"
  log "nothing; switch to v0.1.1 once a GARM release includes #878."
  log "The provider runs on THIS host and fetches runner credentials from GARM's"
  log "metadata URL: that URL must be reachable from this host (GitHub never needs"
  log "to reach it in scale-set mode)."
  log "Verify registration with: garm-cli provider list"

  confirm "Once the [[provider]] section above is added and GARM has been restarted/reloaded, confirm with 'garm-cli provider list' that '${PROVIDER_NAME}' appears."
}

# --- Step 2: create a pool --------------------------------------------------

step_pool() {
  require_repo
  [[ -n "${POOL_TAGS}" ]] || die "POOL_TAGS must not be empty."

  # DinD variant: attach the extra_specs (validated in preflight) as the LAST
  # option, so the printed command stays one valid continued shell command.
  local extra_tail=""
  if [[ "${GATE_VARIANT}" == "dind" ]]; then
    extra_tail=" \\"$'\n'"  --extra-specs '${DIND_EXTRA_SPECS}'"
  fi

  if [[ "${GARM_TARGET}" == "scaleset" ]]; then
    log "=== Step 2: create a scale set against your throwaway test repo ==="
    resolve_scaleset_name
    log "Run the following against your THROWAWAY test repository (never a production one):"
    cat <<EOF

garm-cli scaleset add \\
  --repo ${REPO} \\
  --provider-name ${PROVIDER_NAME} \\
  --name ${SCALESET_NAME} \\
  --image ${POOL_IMAGE} \\
  --flavor ${POOL_FLAVOR} \\
  --min-idle-runners ${MIN_IDLE_RUNNERS} \\
  --max-runners ${MAX_RUNNERS} \\
  --os-arch ${OS_ARCH} \\
  --os-type ${OS_TYPE} \\
  --enabled${extra_tail}

EOF
    log "The scale set NAME (${SCALESET_NAME}) is what the workflow's runs-on"
    log "targets (Step 3); it is remembered in ${PROVIDER_CONFIG_DIR}/scaleset-name so"
    log "later steps agree. Scale set names and labels cannot be changed after"
    log "creation - delete and re-create to change them. No webhook is needed:"
    log "GARM long-polls GitHub's message queue for jobs."
    log "List scale sets with: garm-cli scaleset list --repo ${REPO}"
    confirm "Once the scale set is created, confirm it with 'garm-cli scaleset list --repo ${REPO}'."
  else
    log "=== Step 2: create a pool against your throwaway test repo ==="
    log "Pool mode needs GitHub to reach GARM's webhook endpoint (the repo must"
    log "have a webhook installed, e.g. 'garm-cli repo add ... --install-webhook')."
    log "Run the following against your THROWAWAY test repository (never a production one):"
    cat <<EOF

garm-cli pool add \\
  --repo ${REPO} \\
  --enabled \\
  --provider-name ${PROVIDER_NAME} \\
  --image ${POOL_IMAGE} \\
  --flavor ${POOL_FLAVOR} \\
  --min-idle-runners ${MIN_IDLE_RUNNERS} \\
  --max-runners ${MAX_RUNNERS} \\
  --os-arch ${OS_ARCH} \\
  --os-type ${OS_TYPE} \\
  --tags ${POOL_TAGS}${extra_tail}

EOF
    log "List pools with: garm-cli pool list --repo ${REPO}"
    confirm "Once the pool is created, confirm it with 'garm-cli pool list --repo ${REPO}'."
  fi
  log "--image/--flavor are required by garm-cli but are informational only"
  log "for this provider (see the note above POOL_IMAGE/POOL_FLAVOR in this"
  log "script); --os-arch ${OS_ARCH} was auto-detected from this host (override"
  log "with OS_ARCH=amd64|arm64)."
  log "min-idle-runners=0 / max-runners=1 by design: this gate only ever"
  log "needs one runner to exist at a time."
  if [[ "${GATE_VARIANT}" == "dind" ]]; then
    log "extra_specs selects the '${DIND_FLAVOR_NAME}' flavor (runner ${DIND_RUNNER_MEMORY} / dind ${DIND_DIND_MEMORY})"
    log "and privileged-sidecar; both are bounded by the generated config."
  fi
}

# --- Step 3: watch --------------------------------------------------------

sample_workflow_yaml() {
  local runs_on
  if [[ "${GARM_TARGET}" == "scaleset" ]]; then
    resolve_scaleset_name
    runs_on="${SCALESET_NAME}"
  else
    runs_on="$(tags_to_yaml_array "${POOL_TAGS}")"
  fi
  cat <<EOF
name: garm-m0-demo
on:
  workflow_dispatch: {}
jobs:
  demo:
    runs-on: ${runs_on}
    steps:
      - name: prove we are on a GARM-managed runner
        run: |
          echo hello
          env | sort | head
EOF
  if [[ "${GATE_VARIANT}" == "dind" ]]; then
    cat <<'EOF'
      - name: docker run inside the job (DinD sidecar)
        run: |
          docker version
          docker run --rm hello-world
      - name: docker build inside the job (DinD sidecar)
        run: |
          mkdir -p "$RUNNER_TEMP/dind-gate"
          cd "$RUNNER_TEMP/dind-gate"
          printf 'FROM alpine:3.20\nRUN echo built-inside-dind\n' > Dockerfile
          docker build -t dind-gate:test .
      - name: hold the runner briefly for scripts/demo-m0.sh verify
        run: sleep 60
EOF
  else
    cat <<'EOF'
          # Keep the job alive briefly so 'scripts/demo-m0.sh verify' has a
          # window to inspect the live runner container before it exits -
          # this runner is single-job ephemeral by design (ADR-002).
          sleep 30
EOF
  fi
}

# watch_runner_list prints GARM's own view of this gate's runners: the scale
# set's runners (`garm-cli scaleset runner list <id>`) in scale-set mode, the
# repo's runners in pool mode.
watch_runner_list() {
  if [[ "${GARM_TARGET}" == "scaleset" ]]; then
    lookup_scaleset_id
    if [[ -n "${SCALESET_ID}" ]]; then
      garm-cli scaleset runner list "${SCALESET_ID}" || true
    else
      warn "scale set '${SCALESET_NAME}' not found yet in 'garm-cli scaleset list --repo ${REPO}' (set SCALESET_ID to skip the lookup)"
    fi
  else
    garm-cli runner list --repo "${REPO}" || true
  fi
}

step_watch() {
  log "=== Step 3: trigger the workflow and watch the runner come up ==="
  require_repo
  if [[ "${GARM_TARGET}" == "scaleset" ]]; then
    resolve_scaleset_name
  fi

  log "Copy the following into <your-test-repo>/.github/workflows/garm-m0-demo.yml,"
  log "commit/push it, then trigger it (Actions tab > Run workflow, or"
  log "'gh workflow run garm-m0-demo.yml' if you use the gh CLI):"
  echo
  sample_workflow_yaml
  echo

  log "Watching docker ps (managed runner containers) + GARM's runner list, every ${WATCH_INTERVAL_SECONDS}s"
  log "(Ctrl+C to stop early and move on to Step 4; stops on its own after ${WATCH_MAX_ITERATIONS} iterations, 0 = unbounded)."

  trap 'log "stopping watch (Ctrl+C)"; trap - INT; return 0' INT

  local iterations=0
  while (( WATCH_MAX_ITERATIONS == 0 || iterations < WATCH_MAX_ITERATIONS )); do
    echo "--- $(date -u +%FT%TZ) ---"
    docker ps --filter 'label=garm.docker/managed=true' --filter 'label=garm.docker/role=runner' \
      --format 'table {{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Labels}}' || true
    watch_runner_list
    sleep "${WATCH_INTERVAL_SECONDS}"
    iterations=$(( iterations + 1 ))
  done

  trap - INT
  log "watch loop ended; continuing."
}

# --- Step 4(a-d): verify -----------------------------------------------------

resolve_runner_container() {
  docker ps -a --filter 'label=garm.docker/managed=true' --filter 'label=garm.docker/role=runner' \
    --format '{{.CreatedAt}}\t{{.ID}}' | sort -r | head -n1 | cut -f2
}

label_value() {
  docker inspect -f "{{ index .Config.Labels \"$2\" }}" "$1" 2>/dev/null
}

verify_labels() {
  local cid="$1"
  log "-- (a) runner container labels --"
  if [[ "$(label_value "${cid}" garm.docker/managed)" == "true" ]]; then
    ok "garm.docker/managed=true"
  else
    bad "garm.docker/managed is not 'true'"
  fi
  if [[ "$(label_value "${cid}" garm.docker/role)" == "runner" ]]; then
    ok "garm.docker/role=runner"
  else
    bad "garm.docker/role is not 'runner'"
  fi
  if [[ -n "$(label_value "${cid}" garm.docker/controller-id)" ]]; then
    ok "garm.docker/controller-id is set"
  else
    bad "garm.docker/controller-id is missing"
  fi
  if [[ -n "$(label_value "${cid}" garm.docker/instance-name)" ]]; then
    ok "garm.docker/instance-name is set"
  else
    bad "garm.docker/instance-name is missing"
  fi
}

# Runner workdir and DinD socket dir; keep in sync with internal/spec
# (RunnerWorkDir in mounts.go, DindSocketDir in dind.go).
RUNNER_WORKDIR="/actions-runner/_work"
DIND_SOCKET_DIR="/run"

# job_network_name prints the name of the labeled per-job network of instance "$1".
job_network_name() {
  docker network ls \
    --filter "label=garm.docker/managed=true" \
    --filter "label=garm.docker/resource=job-network" \
    --filter "label=garm.docker/instance-name=$1" \
    --format '{{.Name}}' | head -n1
}

# instance_volume_name prints the name of instance "$1"'s labeled volume of
# resource kind "$2" (workspace, socket, dind-state).
instance_volume_name() {
  docker volume ls \
    --filter "label=garm.docker/managed=true" \
    --filter "label=garm.docker/resource=$2" \
    --filter "label=garm.docker/instance-name=$1" \
    --format '{{.Name}}' | head -n1
}

# container_networks prints the names of the networks container "$1" is attached to.
container_networks() {
  docker inspect -f '{{range $name, $_ := .NetworkSettings.Networks}}{{println $name}}{{end}}' "$1"
}

# container_has_volume_mount succeeds when container "$1" has a named-volume
# mount of volume "$2" at destination "$3".
container_has_volume_mount() {
  docker inspect -f '{{range .Mounts}}{{.Type}}|{{.Name}}|{{.Destination}}{{println}}{{end}}' "$1" \
    | grep -qxF "volume|$2|$3"
}

# verify_topology checks the two OTHER per-allocation resources every
# CreateInstance provisions alongside the runner container as of M1
# (ADR-001/ADR-004): the per-job network (the claim marker; ADR-004's F4/
# nonce-scoped teardown all hinges on it existing and being labeled) and the
# named, labeled workspace volume (replacing M0's anonymous one). Both are
# looked up by label rather than by name, since the workspace volume's name
# embeds a create-nonce this script does not otherwise learn
# (internal/spec/names.go's WorkspaceVolumeName).
verify_topology() {
  local cid="$1"
  local instance
  instance="$(label_value "${cid}" garm.docker/instance-name)"
  log "-- (a, continued) job network + workspace volume (labeled AND wired to the runner) --"
  if [[ -z "${instance}" ]]; then
    bad "cannot check the job network/workspace volume - garm.docker/instance-name was missing on the runner container"
    return
  fi

  local net_name
  net_name="$(job_network_name "${instance}")"
  if [[ -z "${net_name}" ]]; then
    bad "no labeled per-job network found for instance ${instance}"
  else
    ok "per-job network exists and carries garm.docker/resource=job-network (${net_name}; ADR-001/ADR-004 claim marker)"
    if container_networks "${cid}" | grep -qxF "${net_name}"; then
      ok "runner container is attached to the labeled per-job network ${net_name}"
    else
      bad "runner container is NOT attached to the labeled per-job network ${net_name} (attached to: $(container_networks "${cid}" | tr '\n' ' '))"
    fi
    if container_networks "${cid}" | grep -qx bridge; then
      bad "runner container is attached to the default 'bridge' network"
    else
      ok "runner container is not attached to the default 'bridge' network"
    fi
  fi

  local vol_name
  vol_name="$(instance_volume_name "${instance}" workspace)"
  if [[ -z "${vol_name}" ]]; then
    bad "no labeled workspace volume found for instance ${instance}"
  else
    ok "named workspace volume exists and carries garm.docker/resource=workspace (${vol_name})"
    if container_has_volume_mount "${cid}" "${vol_name}" "${RUNNER_WORKDIR}"; then
      ok "runner container mounts the labeled workspace volume ${vol_name} at ${RUNNER_WORKDIR}"
    else
      bad "runner container does not mount the labeled workspace volume ${vol_name} at ${RUNNER_WORKDIR}"
    fi
  fi

  local cache_vols
  cache_vols="$(docker volume ls --filter 'label=garm.docker/cache=true' --format '{{.Name}}' | wc -l | tr -d ' ')"
  log "  (informational, not PASS/FAIL: ${cache_vols} garm.docker/cache=true volume(s) present - toolcache/pnpm/externals/diag persist across jobs by design, ADR-003)"
}

# verify_no_host_mounts fails if container "$1" ("$2" names it in output) has
# ANY bind mount or any docker.sock mount. This provider creates only named
# volumes and a tmpfs - no code path emits a bind mount (git grep
# 'TypeBind|Binds' internal/) - so "no bind mount at all" is the stronger,
# still-true form of "no host docker.sock", and it applies to the DinD
# sidecar too (it serves its own daemon over a named socket volume).
verify_no_host_mounts() {
  local cid="$1" what="$2" mount_types
  mount_types="$(docker inspect -f '{{range .Mounts}}{{.Type}}{{"\n"}}{{end}}' "${cid}")"
  if printf '%s\n' "${mount_types}" | grep -qx bind; then
    bad "${what}: found a bind mount (this provider only creates named volumes and a tmpfs) - check for a docker.sock bind"
  else
    ok "${what}: no host bind mount, no host docker.sock"
  fi
}

# verify_dind_topology checks the privileged-sidecar allocation (ADR-001):
# the role=dind sidecar container and its labels, Privileged=true (the one
# deliberate exception, documented residual risk), no bind mounts, the
# runner's DOCKER_HOST, the labeled socket and dind-state volumes, and the
# flavor's memory limits. For the none variant it instead asserts that NO
# sidecar exists.
verify_dind_topology() {
  local cid="$1" instance did
  instance="$(label_value "${cid}" garm.docker/instance-name)"
  did="$(docker ps -a --filter 'label=garm.docker/managed=true' \
    --filter 'label=garm.docker/role=dind' \
    --filter "label=garm.docker/instance-name=${instance}" -q | head -n1)"

  if [[ "${GATE_VARIANT}" != "dind" ]]; then
    log "-- (b, continued) no DinD sidecar in dind_mode=none --"
    if [[ -z "${did}" ]]; then
      ok "no role=dind sidecar exists for instance ${instance}"
    else
      bad "unexpected DinD sidecar ${did} in the none variant"
    fi
    return
  fi

  log "-- (b, continued) DinD sidecar topology (privileged-sidecar) --"
  if [[ -z "${did}" ]]; then
    bad "no role=dind sidecar container found for instance ${instance}"
    return
  fi
  ok "DinD sidecar container exists (${did})"
  if [[ "$(label_value "${did}" garm.docker/managed)" == "true" ]]; then
    ok "sidecar: garm.docker/managed=true"
  else
    bad "sidecar: garm.docker/managed is not 'true'"
  fi
  if [[ -n "$(label_value "${did}" garm.docker/controller-id)" ]]; then
    ok "sidecar: garm.docker/controller-id is set"
  else
    bad "sidecar: garm.docker/controller-id is missing"
  fi
  if [[ "$(label_value "${did}" garm.docker/instance-name)" == "${instance}" ]]; then
    ok "sidecar: garm.docker/instance-name matches the runner's"
  else
    bad "sidecar: garm.docker/instance-name does not match the runner's"
  fi

  if [[ "$(docker inspect -f '{{.State.Running}}' "${did}")" == "true" ]]; then
    ok "sidecar is running"
  else
    bad "sidecar is not running"
  fi
  if [[ "$(docker inspect -f '{{.HostConfig.Privileged}}' "${did}")" == "true" ]]; then
    ok "sidecar is Privileged=true (expected for privileged-sidecar; ADR-001 residual risk)"
  else
    bad "sidecar is not Privileged=true - not the privileged-sidecar mode this variant requests"
  fi
  verify_no_host_mounts "${did}" "DinD sidecar"

  if docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "${cid}" | grep -qx 'DOCKER_HOST=unix:///run/docker.sock'; then
    ok "runner DOCKER_HOST=unix:///run/docker.sock (the shared socket volume, not a host socket)"
  else
    bad "runner DOCKER_HOST is not unix:///run/docker.sock"
  fi
  if docker exec "${cid}" docker version --format '{{.Server.Version}}' >/dev/null 2>&1; then
    ok "runner container reaches the sidecar's daemon via DOCKER_HOST"
  else
    bad "runner container cannot reach the sidecar's daemon ('docker exec ${cid} docker version' failed)"
  fi

  local res
  for res in socket dind-state; do
    if [[ -n "$(docker volume ls --filter 'label=garm.docker/managed=true' \
        --filter "label=garm.docker/resource=${res}" \
        --filter "label=garm.docker/instance-name=${instance}" -q | head -n1)" ]]; then
      ok "labeled ${res} volume exists"
    else
      bad "no labeled ${res} volume for instance ${instance}"
    fi
  done

  local net_name sock_vol
  net_name="$(job_network_name "${instance}")"
  if [[ -n "${net_name}" ]] && container_networks "${did}" | grep -qxF "${net_name}"; then
    ok "sidecar is attached to the same labeled per-job network ${net_name}"
  else
    bad "sidecar is not attached to the labeled per-job network '${net_name}'"
  fi
  sock_vol="$(instance_volume_name "${instance}" socket)"
  if [[ -n "${sock_vol}" ]] && container_has_volume_mount "${cid}" "${sock_vol}" "${DIND_SOCKET_DIR}"; then
    ok "runner mounts the labeled socket volume ${sock_vol} at ${DIND_SOCKET_DIR}"
  else
    bad "runner does not mount the labeled socket volume '${sock_vol}' at ${DIND_SOCKET_DIR}"
  fi
  if [[ -n "${sock_vol}" ]] && container_has_volume_mount "${did}" "${sock_vol}" "${DIND_SOCKET_DIR}"; then
    ok "sidecar mounts the labeled socket volume ${sock_vol} at ${DIND_SOCKET_DIR}"
  else
    bad "sidecar does not mount the labeled socket volume '${sock_vol}' at ${DIND_SOCKET_DIR}"
  fi

  local runner_mem dind_mem
  runner_mem="$(docker inspect -f '{{.HostConfig.Memory}}' "${cid}")"
  dind_mem="$(docker inspect -f '{{.HostConfig.Memory}}' "${did}")"
  if [[ "${runner_mem}" == "6442450944" ]]; then
    ok "runner memory limit is ${DIND_RUNNER_MEMORY} (flavor ${DIND_FLAVOR_NAME})"
  else
    bad "runner memory limit is ${runner_mem}, expected 6442450944 (${DIND_RUNNER_MEMORY})"
  fi
  if [[ "${dind_mem}" == "4294967296" ]]; then
    ok "sidecar memory limit is ${DIND_DIND_MEMORY} (flavor ${DIND_FLAVOR_NAME})"
  else
    bad "sidecar memory limit is ${dind_mem}, expected 4294967296 (${DIND_DIND_MEMORY})"
  fi
}

verify_no_credentials_in_inspect() {
  local cid="$1"
  log "-- (b) docker inspect: no credentials, no host docker.sock --"

  local env_lines
  env_lines="$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "${cid}")"

  log "  Config.Env (values redacted for any TOKEN/METADATA_URL/CALLBACK_URL-like name; display only, never the source of the PASS/FAIL check below):"
  printf '%s\n' "${env_lines}" \
    | sed -E 's/^([A-Za-z0-9_]*(TOKEN|METADATA_URL|CALLBACK_URL)[A-Za-z0-9_]*)=.*/\1=<redacted-by-demo-script>/' \
    | sed 's/^/    /'

  if printf '%s\n' "${env_lines}" | grep -qiE '^[A-Za-z0-9_]*(BEARER_TOKEN|METADATA_URL|CALLBACK_URL)[A-Za-z0-9_]*='; then
    bad "found a BEARER_TOKEN/METADATA_URL/CALLBACK_URL-like variable NAME in Config.Env (value withheld above)"
  else
    ok "no BEARER_TOKEN/METADATA_URL/CALLBACK_URL-like variable name in Config.Env"
  fi

  verify_no_host_mounts "${cid}" "runner container"

  if [[ "$(docker inspect -f '{{.HostConfig.Privileged}}' "${cid}")" == "false" ]]; then
    ok "runner container is not privileged"
  else
    bad "runner container is Privileged=true (only the DinD sidecar may ever be)"
  fi
}

verify_tmpfs_delivery() {
  local cid="$1"
  log "-- (c) tmpfs credential delivery (/run/garm) --"
  log "  docker exec ${cid} ls -la /run/garm  (filenames/permissions only, never contents):"
  docker exec "${cid}" ls -la /run/garm 2>&1 | sed 's/^/    /'

  local jit_enabled
  jit_enabled="$(printf '%s\n' "$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "${cid}")" \
    | grep -m1 '^JIT_CONFIG_ENABLED=' | cut -d= -f2)"

  local -a expected
  if [[ "${jit_enabled}" == "true" ]]; then
    expected=(runner credentials credentials_rsaparams .delivered)
  else
    expected=(registration-token .delivered)
    log "  JIT_CONFIG_ENABLED=false: checking the non-JIT registration-token path instead"
  fi

  local f
  for f in "${expected[@]}"; do
    if docker exec "${cid}" test -e "/run/garm/${f}"; then
      ok "/run/garm/${f} present"
    else
      bad "/run/garm/${f} MISSING"
    fi
  done
}

verify_symlink_compat() {
  local cid="$1"
  log "-- (d) actions/runner symlink-compat (/actions-runner/.runner) --"
  local ls_out
  ls_out="$(docker exec "${cid}" ls -la /actions-runner/.runner 2>&1)"
  printf '    %s\n' "${ls_out}"
  if printf '%s' "${ls_out}" | grep -q -- '-> /run/garm/runner'; then
    ok "/actions-runner/.runner is a symlink -> /run/garm/runner (ADR-002 verification item)"
  else
    bad "/actions-runner/.runner is NOT a symlink to /run/garm/runner - see ADR-002's documented fallback (docs/adr/ADR-002-runner-image-and-jit-delivery.md)"
  fi

  if [[ "$(docker inspect -f '{{.State.Running}}' "${cid}" 2>/dev/null)" == "true" ]]; then
    if docker top "${cid}" 2>/dev/null | grep -qiE 'run\.sh|Runner\.Listener'; then
      ok "runner process appears to be running (docker top matched run.sh/Runner.Listener)"
    else
      warn "container is Running but docker top did not match run.sh/Runner.Listener - it may still be starting, or the process name differs; check manually with 'docker top ${cid}'"
    fi
  else
    bad "container is not in the Running state"
  fi
}

step_verify() {
  log "=== Step 4 (a-d): verification against the live runner container ==="
  VERIFY_FAILED=0

  local cid
  cid="$(resolve_runner_container)"
  if [[ -z "${cid}" ]]; then
    bad "no managed runner container found (labels garm.docker/managed=true,garm.docker/role=runner) - has the pool created one yet? (Step 3)"
    return 1
  fi
  local cname
  cname="$(docker inspect -f '{{.Name}}' "${cid}" 2>/dev/null | sed 's#^/##')"
  log "Checking container: ${cid} (${cname})"

  verify_labels "${cid}"
  verify_topology "${cid}"
  verify_no_credentials_in_inspect "${cid}"
  verify_dind_topology "${cid}"
  verify_tmpfs_delivery "${cid}"
  verify_symlink_compat "${cid}"

  echo
  if [[ "${VERIFY_FAILED}" -eq 0 ]]; then
    log "All checks PASSED for container ${cid}."
  else
    log "One or more checks FAILED for container ${cid}. See docs/demo-m0.md's troubleshooting table."
  fi
  return "${VERIFY_FAILED}"
}

# --- Step 4(e): post-job teardown check -------------------------------------

step_postjob() {
  log "=== Step 4 (e): post-job teardown check ==="
  VERIFY_FAILED=0

  log "Confirm in the GitHub UI (Settings > Actions > Runners) that the"
  log "runner is gone. This script deliberately never asks for a GitHub"
  log "token, so it cannot check this for you."
  confirm "Confirmed the runner is gone from GitHub?"

  log "Waiting up to ${POST_JOB_WAIT_SECONDS}s for GARM's DeleteInstance to remove the managed runner container..."
  local waited=0
  while docker ps -a --filter 'label=garm.docker/managed=true' --filter 'label=garm.docker/role=runner' -q | grep -q .; do
    if (( waited >= POST_JOB_WAIT_SECONDS )); then
      bad "a managed runner container is still present after ${POST_JOB_WAIT_SECONDS}s"
      break
    fi
    sleep 5
    waited=$(( waited + 5 ))
  done
  if ! docker ps -a --filter 'label=garm.docker/managed=true' --filter 'label=garm.docker/role=runner' -q | grep -q .; then
    ok "no managed runner container remains"
  fi

  # Persistent cache volumes (garm.docker/cache=true, ADR-003) are excluded by
  # label, not by name: they survive jobs by design and are reported
  # informationally. Everything else managed must be gone.
  local leftover_c leftover_n leftover_v cache_v
  leftover_c="$(docker ps -a --filter 'label=garm.docker/managed=true' --format '{{.Names}}')"
  leftover_n="$(docker network ls --filter 'label=garm.docker/managed=true' --format '{{.Name}}')"
  cache_v="$(docker volume ls --filter 'label=garm.docker/cache=true' --format '{{.Name}}')"
  leftover_v="$(docker volume ls --filter 'label=garm.docker/managed=true' --format '{{.Name}}' \
    | { if [[ -n "${cache_v}" ]]; then command grep -vxF -f <(printf '%s\n' "${cache_v}") || true; else cat; fi; })"
  if [[ -z "${leftover_c}" && -z "${leftover_n}" && -z "${leftover_v}" ]]; then
    ok "no leftover job-scoped garm.docker/managed=true containers, networks, or volumes"
  else
    bad "leftover managed resources found"
    [[ -n "${leftover_c}" ]] && printf '    containers: %s\n' "${leftover_c}"
    [[ -n "${leftover_n}" ]] && printf '    networks: %s\n' "${leftover_n}"
    [[ -n "${leftover_v}" ]] && printf '    volumes: %s\n' "${leftover_v}"
  fi
  if [[ -n "${cache_v}" ]]; then
    log "info: persistent cache volumes (garm.docker/cache=true, kept by design):"
    printf '    %s\n' "${cache_v}"
  fi
  log "Note: the per-job network and the named workspace volume both carry"
  log "garm.docker/managed=true (ADR-001/ADR-004), so DeleteInstance's"
  log "teardown removes both explicitly - they are NOT anonymous/implicit"
  log "resources, and a leftover one IS a real failure the check above"
  log "would catch. This differs from the persistent, repo-scoped cache"
  log "volumes (toolcache/pnpm/externals/diag, ADR-003), which carry"
  log "garm.docker/cache=true instead and are DELIBERATELY never swept by"
  log "any teardown path - they are meant to survive across jobs. See the"
  log "root README's Cache section for how to reclaim them manually."

  return "${VERIFY_FAILED}"
}

# --- Cleanup -----------------------------------------------------------------

step_cleanup() {
  log "=== Cleanup ==="
  local image_cleanup_line
  if [[ "${PROVIDER_SOURCE}" == "source" ]]; then
    image_cleanup_line="  docker image rm ${RUNNER_IMAGE_TAG}"
  else
    image_cleanup_line="  # (release mode pulled ${RUNNER_IMAGE_REPO}@sha256:... by digest; 'docker image ls' / 'docker image rm' it by digest if you want the disk space back)"
  fi
  local target_cleanup
  if [[ "${GARM_TARGET}" == "scaleset" ]]; then
    local sid="${SCALESET_ID:-<SCALESET_ID>}"
    target_cleanup="  # 1. Disable and delete the demo scale set (it must have no runners left)
  garm-cli scaleset list --repo ${REPO:-<your-repo>}
  garm-cli scaleset update ${sid} --enabled=false
  garm-cli scaleset runner list ${sid}        # confirm no runners remain
  garm-cli scaleset delete ${sid}"
  else
    target_cleanup="  # 1. Disable and delete the demo pool (garm-cli requires it to be empty first)
  garm-cli pool list --repo ${REPO:-<your-repo>}
  garm-cli pool update <POOL_ID> --enabled=false
  garm-cli runner list <POOL_ID>              # confirm no runners remain
  garm-cli pool delete <POOL_ID>"
  fi
  cat <<EOF

Manual cleanup once you're done with the demo:

${target_cleanup}

  # 2. Confirm no leftover managed Docker resources for this controller
  #    (should already be empty if Step 4(e) passed)
  docker ps -a --filter 'label=garm.docker/managed=true'
  docker network ls --filter 'label=garm.docker/managed=true'
  docker volume ls --filter 'label=garm.docker/managed=true'

  # 3. Remove the demo-local build/download artifacts this script created
${image_cleanup_line}
  rm -rf ${PROVIDER_CONFIG_DIR}
  rm -rf ${PROVIDER_BIN_DIR}

  # 4. Remove the [[provider]] section for '${PROVIDER_NAME}' from GARM's
  #    own config.toml and restart/reload GARM.

  # Persistent caches (toolcache/pnpm/externals/diag, ADR-003) are NOT
  # touched by any of the above - they are deliberately never auto-deleted.
  # Reclaim them explicitly, once no job is using them, with:
  #   docker volume prune -a \\
  #     --filter label=garm.docker/cache=true \\
  #     --filter label=garm.docker/controller-id=<your-controller-id>

EOF
}

# --- Entry point --------------------------------------------------------------

usage() {
  cat <<'EOF'
Usage: scripts/demo-m0.sh [-y|--yes] [--from-source] [--version vX.Y.Z] [--pool] [--dind] [COMMAND]

COMMAND (default: all):
  preflight   Step 0: environment checks; obtain the provider binary + runner image, generate demo config, smoke-test it
  config      Step 1: print the config.toml [[provider]] snippet to register with GARM
  pool        Step 2: print the garm-cli scaleset add (or, with --pool, pool add) command for your test repo
  watch       Step 3: watch docker ps / garm-cli runner list; print the sample workflow yaml
  verify      Step 4(a-d): PASS/FAIL checks against the live runner container, job network, and workspace volume
  postjob     Step 4(e): PASS/FAIL checks after the job has finished
  cleanup     Print manual cleanup commands
  all         Run preflight, config, pool, watch, verify, postjob, cleanup in order

Flags:
  -y, --yes           Skip confirmation pauses (auto-confirm)
  --from-source       Build the provider binary + runner image from this checkout
                       instead of downloading/pinning a published release
                       (equivalent to PROVIDER_SOURCE=source)
  --version vX.Y.Z    Release to validate in the default "release" mode
                       (equivalent to PROVIDER_VERSION=vX.Y.Z). Default: v0.2.1
  --pool              Dispatch via a webhook-driven pool instead of the default
                       scale set (equivalent to GARM_TARGET=pool). Needs GitHub
                       to be able to reach GARM's webhook endpoint.
  --dind              DinD variant: privileged-sidecar + docker run/build in the
                       job, instead of dind_mode=none (GATE_VARIANT=dind)
  -h, --help          Show this help

See docs/demo-m0.md for prerequisites, environment variables, what each
check proves, and a troubleshooting table. No secret is ever read,
generated, or printed by this script.
EOF
}

COMMAND="all"

while [[ $# -gt 0 ]]; do
  case "$1" in
    -y|--yes) ASSUME_YES=1; shift ;;
    --from-source) PROVIDER_SOURCE="source"; shift ;;
    --version)
      [[ $# -ge 2 ]] || die "--version requires an argument, e.g. --version v0.2.1"
      PROVIDER_VERSION="$2"
      shift 2
      ;;
    --pool) GARM_TARGET="pool"; shift ;;
    --dind) GATE_VARIANT="dind"; shift ;;
    -h|--help) usage; exit 0 ;;
    preflight|config|pool|watch|verify|postjob|cleanup|all) COMMAND="$1"; shift ;;
    *) usage >&2; die "unknown argument: $1" ;;
  esac
done

run_all() {
  local rc=0
  step_preflight || rc=$?
  step_config || rc=$?
  step_pool || rc=$?
  step_watch || rc=$?
  step_verify || rc=$?
  step_postjob || rc=$?
  step_cleanup || rc=$?
  return "${rc}"
}

main() {
  validate_modes
  case "${COMMAND}" in
    preflight) step_preflight ;;
    config)    step_config ;;
    pool)      step_pool ;;
    watch)     step_watch ;;
    verify)    step_verify ;;
    postjob)   step_postjob ;;
    cleanup)   step_cleanup ;;
    all)       run_all ;;
  esac
}

main
