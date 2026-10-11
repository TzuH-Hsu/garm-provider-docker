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
# persistent caches, extra_specs, v0.1.1 self-description). v0.2.0 is the recorded
# failing baseline against GARM v0.2.x; an end-to-end pass is claimed only for
# a published release that passes this gate (v0.2.1 once published; until then
# use --from-source). The script keeps
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
#               config.toml; smoke-test the binary against it; install
#               the binary, config, gate-modes (and the --from-source
#               runner image tag) only after every step succeeded
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
#                          Auto-detected from `uname -m` when unset. If set,
#                          it must equal the host's architecture (the
#                          provider does no cross-architecture runner
#                          selection); the script exits otherwise. It
#                          selects the release binary asset in "release"
#                          mode.
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

# Everything this script creates (state directories, the generated provider
# config GARM reads, the downloaded binary, the saved scale-set name) must not
# be writable by group or others: a group-writable provider config would let
# another account change runner_image. Hosts commonly default to umask 002, so
# set it explicitly instead of inheriting it.
umask 022

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# --- Configuration (env-overridable) ----------------------------------------

GARM_URL="${GARM_URL:-http://localhost:8080}"
REPO="${REPO:-}"

# Which modes the operator chose explicitly (env here, flags in the argument
# loop); the rest may be loaded from the modes saved by an earlier
# preflight (see load_saved_modes).
EXPL_PROVIDER_SOURCE="${PROVIDER_SOURCE:-}"
EXPL_PROVIDER_VERSION="${PROVIDER_VERSION:-}"
EXPL_PROVIDER_INTERFACE_VERSION="${PROVIDER_INTERFACE_VERSION:-}"
EXPL_GARM_TARGET="${GARM_TARGET:-}"
EXPL_GATE_VARIANT="${GATE_VARIANT:-}"

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
# auto-detected from the host (and must match it), not hardcoded to amd64.
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
    # Quoted: tags/names are allowlisted to [A-Za-z0-9._-], so a plain double
    # quote is a complete YAML string escape, and it keeps tokens such as
    # true / null / 123 from being parsed as non-strings by GitHub Actions.
    if [[ "${first}" -eq 1 ]]; then
      out+="\"${tag}\""
      first=0
    else
      out+=", \"${tag}\""
    fi
  done
  out+="]"
  printf '%s' "${out}"
}

require_repo() {
  [[ -n "${REPO}" ]] || die "REPO is not set. export REPO=<garm-repo-id-or-owner/name> (see 'garm-cli repo list') and re-run."
}

# --- Input validation --------------------------------------------------------
#
# Every operator- or file-supplied value that reaches a printed copy-paste
# command, a generated file (TOML/YAML) or a docker/garm-cli invocation is
# checked against a strict allowlist here, so no value can carry shell
# metacharacters, quotes, newlines or arithmetic. The allowlists contain no
# quote, backslash, space, $, ;, &, |, <, >, backtick or newline, which also
# makes the values safe inside the generated TOML strings and YAML scalars.
# (Values derived from tool output - image digests - are format-checked where
# they are produced.)

readonly MAX_NAME_LEN=64

# require_format NAME VALUE ERE MAXLEN DESCRIPTION: dies unless VALUE matches
# ERE and is at most MAXLEN characters.
require_format() {
  local name="$1" value="$2" re="$3" max="$4" what="$5"
  if (( ${#value} > max )) || ! [[ "${value}" =~ ${re} ]]; then
    die "${name} is invalid (${what}; at most ${max} characters)"
  fi
}

# validate_inputs checks every env-overridable value used in a sink.
validate_inputs() {
  local name_re='^[A-Za-z0-9][A-Za-z0-9._-]*$'
  local path_re='^/[A-Za-z0-9._/+@-]*$'
  require_format PROVIDER_NAME "${PROVIDER_NAME}" '^[A-Za-z0-9][A-Za-z0-9_-]*$' "${MAX_NAME_LEN}" "letters, digits, '_' and '-'"
  require_format PROVIDER_VERSION "${PROVIDER_VERSION}" "${name_re}" "${MAX_NAME_LEN}" "letters, digits, '.', '_' and '-'"
  require_format PROVIDER_SOURCE "${PROVIDER_SOURCE}" '^(release|source)$' 16 "'release' or 'source'"
  require_format POOL_TAGS "${POOL_TAGS}" '^[A-Za-z0-9._-]+(,[A-Za-z0-9._-]+)*$' 256 "comma-separated tags of letters, digits, '.', '_' and '-'"
  require_format POOL_IMAGE "${POOL_IMAGE}" '^[A-Za-z0-9][A-Za-z0-9._:/@-]*$' 256 "letters, digits and '._:/@-'"
  require_format POOL_FLAVOR "${POOL_FLAVOR}" '^[A-Za-z0-9][A-Za-z0-9._-]*$' "${MAX_NAME_LEN}" "letters, digits, '.', '_' and '-'"
  require_format DIND_IMAGE_SOURCE "${DIND_IMAGE_SOURCE}" '^[A-Za-z0-9][A-Za-z0-9._:/-]*$' 256 "an image reference of letters, digits and '._:/-'"
  require_format RUNNER_IMAGE_TAG "${RUNNER_IMAGE_TAG}" '^[A-Za-z0-9][A-Za-z0-9._:/-]*$' 256 "an image tag of letters, digits and '._:/-'"
  require_format GARM_URL "${GARM_URL}" '^https?://[][A-Za-z0-9._:-]+(/[A-Za-z0-9._~/-]*)?$' 256 "an http(s) URL without credentials, query or special characters"
  require_format WATCH_INTERVAL_SECONDS "${WATCH_INTERVAL_SECONDS}" '^[0-9]{1,6}$' 6 "a non-negative integer"
  require_format WATCH_MAX_ITERATIONS "${WATCH_MAX_ITERATIONS}" '^[0-9]{1,6}$' 6 "a non-negative integer"
  require_format POST_JOB_WAIT_SECONDS "${POST_JOB_WAIT_SECONDS}" '^[0-9]{1,6}$' 6 "a non-negative integer"
  [[ -z "${REPO}" ]] || require_format REPO "${REPO}" '^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9._-]+)?$' 128 "a repo ID or owner/name of letters, digits, '.', '_', '-' and one '/'"
  [[ -z "${SCALESET_NAME}" ]] || require_format SCALESET_NAME "${SCALESET_NAME}" "${name_re}" "${MAX_NAME_LEN}" "letters, digits, '.', '_' and '-', starting with a letter or digit"
  [[ -z "${SCALESET_ID}" ]] || require_format SCALESET_ID "${SCALESET_ID}" '^[A-Za-z0-9][A-Za-z0-9-]*$' 64 "an ID of letters, digits and '-'"
  require_format PROVIDER_BIN_DIR "${PROVIDER_BIN_DIR}" "${path_re}" 200 "an absolute path of letters, digits and '._/+@-' (no spaces; set PROVIDER_BIN_DIR explicitly if the checkout path contains other characters)"
  require_format PROVIDER_CONFIG_DIR "${PROVIDER_CONFIG_DIR}" "${path_re}" 200 "an absolute path of letters, digits and '._/+@-' (no spaces; set PROVIDER_CONFIG_DIR explicitly)"
  [[ "/${PROVIDER_BIN_DIR}/" != */../* && "/${PROVIDER_CONFIG_DIR}/" != */../* ]] \
    || die "PROVIDER_BIN_DIR / PROVIDER_CONFIG_DIR must not contain '..' segments"
}

# path_is_private succeeds when "$1" is a real (non-symlink) object of find(1)
# type "$2" (d or f) owned by the invoking user and writable by neither group
# nor others. Portable across Linux and macOS find.
path_is_private() {
  [[ -n "$(find "$1" -maxdepth 0 -type "$2" -user "$(id -u)" ! -perm -020 ! -perm -002 2>/dev/null)" ]]
}

# path_is_owned_regular succeeds when "$1" is a real (non-symlink) regular file
# owned by the invoking user, whatever its mode.
path_is_owned_regular() {
  [[ -n "$(find "$1" -maxdepth 0 -type f -user "$(id -u)" 2>/dev/null)" ]]
}

# check_private_dir dies when directory "$1" ("$2" names it) exists but is not
# a real directory owned by the invoking user and closed to group/others:
# another local account could otherwise pre-create or tamper with it (the
# default config dir lives under /tmp).
check_private_dir() {
  [[ -e "$1" || -L "$1" ]] || return 0
  path_is_private "$1" d \
    || die "$2 ($1) must be a real directory owned by you ($(id -un)) and not writable by group or others. Remove it or fix it (chmod go-w), or set $2 to a directory you own."
}

# ensure_private_dir creates directory "$1" ("$2" names it) when missing,
# enforces check_private_dir, and then normalizes it to mode "$3" whether it
# was just created or reused: a directory left at 0700 by an earlier run would
# otherwise pass the ownership/no-group-write check yet stay unreadable to a
# GARM service running under another account.
ensure_private_dir() {
  if [[ ! -e "$1" && ! -L "$1" ]]; then
    mkdir -p "$1" || die "could not create $1"
  fi
  check_private_dir "$1" "$2"
  chmod "$3" "$1" || die "could not set mode $3 on $1 ($2); fix it manually (chmod $3 $1)"
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
  validate_inputs
  check_private_dir "${PROVIDER_BIN_DIR}" PROVIDER_BIN_DIR
  check_private_dir "${PROVIDER_CONFIG_DIR}" PROVIDER_CONFIG_DIR
}

# --- Saved modes -------------------------------------------------------------
#
# The selected modes are remembered in $PROVIDER_CONFIG_DIR/gate-modes so the
# documented single-step flow (preflight, then pool, watch, verify, ...) in
# separate invocations keeps using the same source, version, dispatch target,
# variant and interface version. Format: KEY=value lines, parsed WITHOUT
# sourcing or eval, keys and values strictly allowlisted.

readonly MODE_KEYS="PROVIDER_SOURCE PROVIDER_VERSION GARM_TARGET GATE_VARIANT PROVIDER_INTERFACE_VERSION"

modes_file() { printf '%s/gate-modes' "${PROVIDER_CONFIG_DIR}"; }

# mode_value_valid KEY VALUE succeeds when VALUE is allowed for KEY.
mode_value_valid() {
  case "$1" in
    PROVIDER_SOURCE)            [[ "$2" =~ ^(release|source)$ ]] ;;
    PROVIDER_VERSION)           (( ${#2} <= MAX_NAME_LEN )) && [[ "$2" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] ;;
    GARM_TARGET)                [[ "$2" =~ ^(scaleset|pool)$ ]] ;;
    GATE_VARIANT)               [[ "$2" =~ ^(none|dind)$ ]] ;;
    PROVIDER_INTERFACE_VERSION) [[ "$2" =~ ^v0\.1\.[01]$ ]] ;;
    *) return 1 ;;
  esac
}

# explicit_mode_value prints the value the operator chose explicitly for KEY
# (empty when none).
explicit_mode_value() {
  case "$1" in
    PROVIDER_SOURCE)            printf '%s' "${EXPL_PROVIDER_SOURCE}" ;;
    PROVIDER_VERSION)           printf '%s' "${EXPL_PROVIDER_VERSION}" ;;
    GARM_TARGET)                printf '%s' "${EXPL_GARM_TARGET}" ;;
    GATE_VARIANT)               printf '%s' "${EXPL_GATE_VARIANT}" ;;
    PROVIDER_INTERFACE_VERSION) printf '%s' "${EXPL_PROVIDER_INTERFACE_VERSION}" ;;
  esac
}

# stage_modes writes the effective modes (called by preflight and all) to
# STAGE_MODES, a file in the config staging directory; publish_staged_artifacts
# renames it to modes_file() together with the binary and config, so every
# fallible write happens before anything live is replaced.
stage_modes() {
  local key content=""
  for key in ${MODE_KEYS}; do
    content+="${key}=${!key}"$'\n'
  done
  STAGE_MODES="${STAGE_CFG_DIR}/gate-modes"
  if ! printf '%s' "${content}" > "${STAGE_MODES}" || ! chmod 0600 "${STAGE_MODES}"; then
    die "could not stage ${STAGE_MODES}"
  fi
}

# load_saved_modes applies the modes saved by an earlier preflight to every
# mode the operator did not choose explicitly, and dies when an explicit
# choice conflicts with the saved one (never silently switching modes).
load_saved_modes() {
  local f key line k v expl cur
  f="$(modes_file)"
  [[ -e "${f}" || -L "${f}" ]] || return 0
  path_is_owned_regular "${f}" \
    || die "${f} must be a regular file owned by you (not a symlink). Remove it and re-run."
  if ! path_is_private "${f}" f; then
    chmod 0600 "${f}" || die "could not set mode 0600 on ${f}; remove it and re-run."
    warn "${f} was writable by group or others; reset its mode to 0600."
  fi

  local -a seen=()
  local count=0
  while IFS= read -r line || [[ -n "${line}" ]]; do
    [[ -n "${line}" ]] || continue
    (( ++count <= 16 )) || die "${f} has too many lines. Remove it and re-run preflight."
    [[ "${line}" == *=* ]] || die "${f} has a malformed line. Remove it and re-run preflight."
    k="${line%%=*}"
    v="${line#*=}"
    [[ " ${MODE_KEYS} " == *" ${k} "* ]] || die "${f} has an unknown key. Remove it and re-run preflight."
    [[ " ${seen[*]-} " != *" ${k} "* ]] || die "${f} repeats the key ${k}. Remove it and re-run preflight."
    mode_value_valid "${k}" "${v}" || die "${f} has an invalid value for ${k}. Remove it and re-run preflight."
    seen+=("${k}")
    printf -v "SAVED_${k}" '%s' "${v}"
  done < <(head -c 2048 "${f}")

  for key in ${MODE_KEYS}; do
    [[ " ${seen[*]-} " == *" ${key} "* ]] || continue
    v="SAVED_${key}"; v="${!v}"
    expl="$(explicit_mode_value "${key}")"
    cur="${!key}"
    if [[ -n "${expl}" ]]; then
      [[ "${expl}" == "${v}" ]] \
        || die "${key}=${expl} conflicts with the saved ${key}=${v} (${f}). Re-run preflight with the new modes, or remove the file."
    elif [[ "${cur}" != "${v}" ]]; then
      printf -v "${key}" '%s' "${v}"
      log "using saved ${key}=${v} from ${f}"
    fi
  done
}

# resolve_scaleset_name fixes SCALESET_NAME for this run: the operator's
# value, else one remembered from an earlier step in the config dir, else a
# fresh garm-docker-gate-<6 hex>. It is persisted so `pool`, `watch` and
# `cleanup` run as separate invocations all agree (scale set names - and
# labels - cannot be changed after creation, and the workflow's runs-on must
# match exactly).
resolve_scaleset_name() {
  local f="${PROVIDER_CONFIG_DIR}/scaleset-name"
  if [[ -n "${SCALESET_NAME}" ]]; then
    # An explicit name is persisted too, so later invocations that do not
    # repeat it (watch, verify, postjob) agree with the scale set that was
    # actually created. SCALESET_ID is never cached on disk (it is looked up
    # by this name on each run), so nothing else needs invalidating.
    [[ "${SCALESET_NAME}" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] \
      || die "SCALESET_NAME '${SCALESET_NAME}' is invalid: use letters, digits, '.', '_' and '-', starting with a letter or digit"
    # The old value is only read from a regular file we own (never through a
    # symlink), and is only used for the "changed" log line.
    local saved=""
    if path_is_owned_regular "${f}"; then
      saved="$(head -c 256 "${f}")"
    fi
    ensure_private_dir "${PROVIDER_CONFIG_DIR}" PROVIDER_CONFIG_DIR 0755
    # Always (re)write: cheap, and it also normalizes the mode of a file left
    # group-writable by an older version of this script.
    write_file_atomic "${f}" 0600 <<< "${SCALESET_NAME}"
    if [[ -n "${saved}" && "${saved}" != "${SCALESET_NAME}" ]]; then
      log "saved scale set name changed: '${saved}' -> '${SCALESET_NAME}'"
    fi
    return 0
  fi
  if [[ -s "${f}" ]]; then
    # The cached name feeds printed commands and the workflow, so it gets the
    # same allowlist as an explicit one, and the file must be ours and closed
    # to others (the config dir can live under /tmp).
    # Wrong owner, a symlink or a non-regular file is fatal. A regular file we
    # own that is merely group/other-writable (e.g. left by an older version
    # of this script) is normalized to 0600: ownership is the trust boundary,
    # and the content is allowlist-validated below either way, so a foreign
    # edit can at worst yield a rejected or different valid name.
    path_is_owned_regular "${f}" \
      || die "${f} must be a regular file owned by you (not a symlink). Remove it and re-run."
    if ! path_is_private "${f}" f; then
      chmod 0600 "${f}" || die "could not set mode 0600 on ${f}; remove it and re-run."
      warn "${f} was writable by group or others; reset its mode to 0600."
    fi
    local cached
    cached="$(head -c 256 "${f}")"
    cached="${cached%%$'\n'*}"
    if (( ${#cached} > MAX_NAME_LEN )) || ! [[ "${cached}" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
      die "the saved scale set name in ${f} is invalid (letters, digits, '.', '_' and '-' only; at most ${MAX_NAME_LEN} characters). Remove the file and re-run."
    fi
    SCALESET_NAME="${cached}"
    return 0
  fi
  ensure_private_dir "${PROVIDER_CONFIG_DIR}" PROVIDER_CONFIG_DIR 0755
  SCALESET_NAME="garm-docker-gate-$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')"
  write_file_atomic "${f}" 0600 <<< "${SCALESET_NAME}"
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
# OS_ARCH may be set explicitly, but only to the host's own architecture: the
# provider creates containers with no platform override (internal/provider/
# create.go passes a nil platform), so it never selects a cross-architecture
# runner, and the release asset / source build must match the host to execute.
detect_arch() {
  local machine
  machine="$(uname -m)"
  case "${machine}" in
    x86_64|amd64)   printf 'amd64' ;;
    aarch64|arm64)  printf 'arm64' ;;
    *) die "unsupported host architecture '${machine}': this provider only supports linux/amd64 and linux/arm64 (internal/provider/create.go's supportedOSArches)" ;;
  esac
}
HOST_ARCH=""
OS_ARCH="${OS_ARCH:-}"

# init_arch resolves HOST_ARCH and OS_ARCH. It runs from main, after argument
# parsing and only for real commands, so --help never depends on arch
# detection. detect_arch runs in a command-substitution subshell, where its
# die only exits the subshell, so the status is checked explicitly here.
init_arch() {
  HOST_ARCH="$(detect_arch)" || exit 1
  [[ -n "${HOST_ARCH}" ]] || die "could not determine the host architecture"
  OS_ARCH="${OS_ARCH:-${HOST_ARCH}}"
  if [[ "${OS_ARCH}" != "${HOST_ARCH}" ]]; then
    die "cross-architecture runners are not supported; OS_ARCH must match the host (${HOST_ARCH}), got '${OS_ARCH}'"
  fi
}

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

# write_file_atomic writes stdin to "$1" with mode "$2" via a temp file in the
# same directory and rename(2). The rename replaces a pre-planted symlink at
# the destination instead of following it, and gives the file the intended
# owner (us) and mode even when an older file with a different mode existed
# (umask and directory mode only affect NEW files).
write_file_atomic() {
  local dest="$1" mode="$2" tmp
  [[ ! -d "${dest}" ]] || die "${dest} is a directory; remove it and re-run."
  tmp="$(mktemp "${dest}.XXXXXX")" || die "could not create a temporary file next to ${dest}"
  if ! cat > "${tmp}" || ! chmod "${mode}" "${tmp}" || ! mv -f "${tmp}" "${dest}"; then
    rm -f "${tmp}"
    die "could not write ${dest}"
  fi
}

# --- Staging -----------------------------------------------------------------
#
# GARM executes PROVIDER_BIN and reads PROVIDER_CONFIG_FILE directly, so
# preflight must not touch either until everything it does has succeeded: the
# download/build, the runner and DinD digest resolution, config generation,
# the smoke test and the preparation of gate-modes. Until then the new binary,
# config and gate-modes live in private (0700) staging directories created
# INSIDE their target directories, so the final rename(2)s never cross a
# filesystem and are atomic. In source mode the runner image is built under a
# temporary tag for the same reason. A trap removes the staging directories
# and the temporary tag on any exit; a SIGKILL can leave a .stage.* directory
# behind, which is safe to delete (the temporary tag is garm-demo-runner-stage:*).

STAGE_BIN_DIR=""
STAGE_CFG_DIR=""
STAGE_BIN=""
STAGE_CFG=""
STAGE_MODES=""
STAGE_IMAGE_TAG=""

# PUBLISHED lists the publish steps completed so far, newest first (image,
# bin, cfg, modes), and PREV_IMAGE_ID is the image RUNNER_IMAGE_TAG pointed at
# before the retag; both exist only so a failed publish can be rolled back.
PUBLISHED=""
PREV_IMAGE_ID=""

# cleanup_staging rolls back a half-finished publish (a signal can interrupt
# one), then removes the staging directories and the temporary image tag
# (idempotent; never fails).
cleanup_staging() {
  [[ -z "${PUBLISHED}" ]] || rollback_published || true
  [[ -z "${STAGE_BIN_DIR}" ]] || rm -rf "${STAGE_BIN_DIR}"
  [[ -z "${STAGE_CFG_DIR}" ]] || rm -rf "${STAGE_CFG_DIR}"
  [[ -z "${STAGE_IMAGE_TAG}" ]] || docker image rm "${STAGE_IMAGE_TAG}" >/dev/null 2>&1 || true
  STAGE_BIN_DIR=""
  STAGE_CFG_DIR=""
  STAGE_IMAGE_TAG=""
}

# create_staging ensures both target directories and creates the staging
# directories inside them (mktemp -d creates them with mode 0700). The traps
# make the EXIT cleanup run on Ctrl+C / termination as well as on die.
create_staging() {
  ensure_private_dir "${PROVIDER_BIN_DIR}" PROVIDER_BIN_DIR 0755
  ensure_private_dir "${PROVIDER_CONFIG_DIR}" PROVIDER_CONFIG_DIR 0755
  trap cleanup_staging EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 143' TERM
  STAGE_BIN_DIR="$(mktemp -d "${PROVIDER_BIN_DIR}/.stage.XXXXXX")" \
    || die "could not create a staging directory in ${PROVIDER_BIN_DIR}"
  STAGE_CFG_DIR="$(mktemp -d "${PROVIDER_CONFIG_DIR}/.stage.XXXXXX")" \
    || die "could not create a staging directory in ${PROVIDER_CONFIG_DIR}"
  STAGE_BIN="${STAGE_BIN_DIR}/garm-provider-docker"
  STAGE_CFG="${STAGE_CFG_DIR}/config.toml"
}

# stage_provider_binary makes the verified or freshly built binary "$1" the
# staged binary (mode 0755). Nothing under PROVIDER_BIN_DIR other than the
# staging directory is touched.
stage_provider_binary() {
  chmod 0755 "$1" || die "could not chmod $1"
  mv -f "$1" "${STAGE_BIN}" || die "could not stage ${STAGE_BIN}"
  path_is_private "${STAGE_BIN}" f \
    || die "${STAGE_BIN} must be a regular file owned by you and not writable by group or others."
}

# keep_rollback_copy "$1" "$2" saves the current live object "$1" (if any) as
# "$2" inside a staging directory on the same filesystem: a hard link to the
# object itself (a symlink stays a symlink), else a copy. The rename that later
# replaces "$1" leaves the saved object intact.
keep_rollback_copy() {
  [[ -e "$1" || -L "$1" ]] || return 0
  ln -P "$1" "$2" 2>/dev/null || cp -pP "$1" "$2"
}

# restore_live "$1" "$2" puts the saved object "$2" back as "$1" (atomic
# rename), or removes "$1" when there was nothing there before.
restore_live() {
  if [[ -e "$2" || -L "$2" ]]; then
    mv -f "$2" "$1"
  else
    rm -f "$1"
  fi
}

# restore_image points RUNNER_IMAGE_TAG back at the previous image (or removes
# the tag when it did not exist).
restore_image() {
  if [[ -n "${PREV_IMAGE_ID}" ]]; then
    docker tag "${PREV_IMAGE_ID}" "${RUNNER_IMAGE_TAG}"
  else
    docker image rm "${RUNNER_IMAGE_TAG}" >/dev/null
  fi
}

# rollback_published undoes the completed publish steps, newest first. It
# returns non-zero when any restore failed.
rollback_published() {
  local step failed=0
  for step in ${PUBLISHED}; do
    case "${step}" in
      modes) restore_live "$(modes_file)" "${STAGE_CFG_DIR}/prev-gate-modes" || failed=1 ;;
      cfg)   restore_live "${PROVIDER_CONFIG_FILE}" "${STAGE_CFG_DIR}/prev-config.toml" || failed=1 ;;
      bin)   restore_live "${PROVIDER_BIN}" "${STAGE_BIN_DIR}/prev-bin" || failed=1 ;;
      image) restore_image || failed=1 ;;
    esac
  done
  PUBLISHED=""
  return "${failed}"
}

# publish_fail "$1" rolls back and exits with message "$1".
publish_fail() {
  if rollback_published; then
    die "$1; the previous binary, config, gate-modes and runner image tag were restored."
  fi
  die "$1; restoring the previous artifacts was incomplete - re-run preflight to bring binary, config and gate-modes back in line."
}

# publish_staged_artifacts installs the staged artifacts and drops the staging
# directories and cleanup traps. Everything fallible that can be prepared is
# done BEFORE the first change (staged files, destination and ownership
# checks, rollback copies of the live binary, config and gate-modes), so the
# publish itself is the runner image retag followed by three rename(2)s, each
# replacing a symlink rather than following it. If a step fails, the completed
# ones are rolled back and the run exits non-zero.
#
# Order: (1) retag the runner image (source mode only) - the only step that
# talks to the Docker daemon, hence the likeliest to fail, and failing it
# leaves every file untouched; (2) the binary before (3) the config: GARM runs
# the binary with the config on every provider call, so the brief window
# between the renames is NEW binary + OLD config, and a newer binary is
# expected to read the previous config (omitted keys take their defaults)
# whereas an older binary can silently ignore keys a newer config relies on (it
# decodes the config non-strictly); (4) gate-modes last, because it only
# records what the other artifacts are.
#
# Residual window, stated plainly: between the renames GARM can observe a mix
# of old and new artifacts, and a SIGKILL or power loss in that window leaves
# the mix in place (the rollback is a best-effort restore, not a transaction).
# Re-running preflight always brings the set back in line.
#
# Signals: the publish below is a few renames plus at most one `docker tag`, so
# HUP, INT and TERM are ignored from the first change until the publish (or,
# on failure, the rollback) has finished. Otherwise a signal landing right
# after a rename but before it is recorded in PUBLISHED would make the
# rollback skip that step and leave a half-restored set; finishing a
# consistent publish or rollback is preferable to a prompt exit. bash discards
# an ignored signal rather than deferring it, so a Ctrl+C during this window
# has no effect (it lasts well under a second); child processes such as
# `docker tag` inherit the ignored disposition, which is what we want here.
# The EXIT trap is untouched, so a genuine failure still rolls back and cleans
# up. SIGKILL cannot be ignored and remains the residual window above.
publish_staged_artifacts() {
  local modes_live
  modes_live="$(modes_file)"
  [[ ! -d "${PROVIDER_BIN}" ]] || die "${PROVIDER_BIN} is a directory; remove it and re-run."
  [[ ! -d "${PROVIDER_CONFIG_FILE}" ]] || die "${PROVIDER_CONFIG_FILE} is a directory; remove it and re-run."
  [[ ! -d "${modes_live}" ]] || die "${modes_live} is a directory; remove it and re-run."
  if ! path_is_private "${STAGE_BIN}" f || [[ ! -f "${STAGE_CFG}" ]] || [[ ! -f "${STAGE_MODES}" ]]; then
    die "internal error: the staged artifacts are missing or have the wrong owner or mode."
  fi
  keep_rollback_copy "${PROVIDER_BIN}" "${STAGE_BIN_DIR}/prev-bin" \
    || die "could not save a rollback copy of ${PROVIDER_BIN}; nothing was changed."
  keep_rollback_copy "${PROVIDER_CONFIG_FILE}" "${STAGE_CFG_DIR}/prev-config.toml" \
    || die "could not save a rollback copy of ${PROVIDER_CONFIG_FILE}; nothing was changed."
  keep_rollback_copy "${modes_live}" "${STAGE_CFG_DIR}/prev-gate-modes" \
    || die "could not save a rollback copy of ${modes_live}; nothing was changed."

  trap '' HUP INT TERM
  if [[ -n "${STAGE_IMAGE_TAG}" ]]; then
    PREV_IMAGE_ID="$(docker image inspect --format '{{.Id}}' "${RUNNER_IMAGE_TAG}" 2>/dev/null)" || PREV_IMAGE_ID=""
    docker tag "${STAGE_IMAGE_TAG}" "${RUNNER_IMAGE_TAG}" \
      || die "could not tag the staged runner image as ${RUNNER_IMAGE_TAG}; nothing was changed."
    PUBLISHED="image ${PUBLISHED}"
  fi
  mv -f "${STAGE_BIN}" "${PROVIDER_BIN}" || publish_fail "could not install ${PROVIDER_BIN}"
  PUBLISHED="bin ${PUBLISHED}"
  mv -f "${STAGE_CFG}" "${PROVIDER_CONFIG_FILE}" || publish_fail "could not install ${PROVIDER_CONFIG_FILE}"
  PUBLISHED="cfg ${PUBLISHED}"
  mv -f "${STAGE_MODES}" "${modes_live}" || publish_fail "could not install ${modes_live}"
  PUBLISHED=""

  cleanup_staging
  trap - EXIT HUP INT TERM
  log "Installed ${PROVIDER_BIN}, ${PROVIDER_CONFIG_FILE} and ${modes_live}"
}

# --- Step 0: preflight -------------------------------------------------------

build_provider_binary() {
  log "Building provider binary from source (go build -ldflags version stamp)..."
  local version
  version="$(git -C "${REPO_ROOT}" describe --tags --always --dirty 2>/dev/null || echo dev)"
  local bin_tmp
  bin_tmp="$(mktemp "${STAGE_BIN_DIR}/build.XXXXXX")" || die "could not create a temporary file in ${STAGE_BIN_DIR}"
  if ! ( cd "${REPO_ROOT}" && go build \
      -ldflags "-X github.com/TzuH-Hsu/garm-provider-docker/internal/version.Version=${version}" \
      -o "${bin_tmp}" . ); then
    die "go build failed"
  fi
  stage_provider_binary "${bin_tmp}"
  log "Built ${STAGE_BIN} (version ${version})"
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

  check_release_published
  warn_if_known_incompatible_version

  local asset="garm-provider-docker_${PROVIDER_VERSION}_linux_${OS_ARCH}"
  local asset_path sums_path
  asset_path="$(mktemp "${STAGE_BIN_DIR}/${asset}.XXXXXX")" || die "could not create a temporary file in ${STAGE_BIN_DIR}"
  sums_path="$(mktemp "${STAGE_BIN_DIR}/SHA256SUMS.XXXXXX")" || die "could not create a temporary file in ${STAGE_BIN_DIR}"

  log "Downloading ${asset} (release ${PROVIDER_VERSION})..."
  curl -fsSL -o "${asset_path}" "${RELEASE_BASE_URL}/${PROVIDER_VERSION}/${asset}" \
    || { rm -f "${asset_path}" "${sums_path}"; die "failed to download ${asset} from the ${PROVIDER_VERSION} release - check PROVIDER_VERSION, and that this host can reach github.com"; }
  curl -fsSL -o "${sums_path}" "${RELEASE_BASE_URL}/${PROVIDER_VERSION}/SHA256SUMS" \
    || { rm -f "${asset_path}" "${sums_path}"; die "failed to download SHA256SUMS from the ${PROVIDER_VERSION} release"; }

  local expected
  expected="$(awk -v f="${asset}" '$2 == f { print $1; found=1 } END { exit !found }' "${sums_path}")" \
    || die "SHA256SUMS from release ${PROVIDER_VERSION} has no entry for ${asset} - refusing to trust an unverifiable download"

  local actual
  actual="$(compute_sha256 "${asset_path}")" || exit 1
  if [[ "${expected}" != "${actual}" ]]; then
    rm -f "${asset_path}" "${sums_path}"
    die "checksum mismatch for ${asset}: SHA256SUMS says ${expected}, downloaded file hashes to ${actual} - refusing to use a corrupted or tampered binary"
  fi
  ok "SHA256SUMS verified for ${asset} (${expected})"

  rm -f "${sums_path}"
  stage_provider_binary "${asset_path}"
  log "Staged verified binary: ${STAGE_BIN}"
}

# build_runner_image builds the runner image under a unique temporary tag. The
# generated source-mode config references RUNNER_IMAGE_TAG itself (a mutable
# tag; allow_unpinned_runner_image), so that tag must keep pointing at the
# previous image until preflight has succeeded: publish_staged_artifacts
# retags the staged image to RUNNER_IMAGE_TAG, and cleanup_staging removes
# the temporary tag on every exit.
build_runner_image() {
  STAGE_IMAGE_TAG="garm-demo-runner-stage:$$-${RANDOM}${RANDOM}"
  log "Building runner image from runner-images/noble as ${STAGE_IMAGE_TAG} (${RUNNER_IMAGE_TAG} is retagged only after preflight succeeds) ..."
  docker build -t "${STAGE_IMAGE_TAG}" "${REPO_ROOT}/runner-images/noble" \
    || die "docker build of the runner image failed"
}

# imagetools_digest prints the sha256:<64 hex> digest of image reference "$1"
# via `docker buildx imagetools inspect`. It deliberately does NOT use
# `--format '{{.Manifest.Digest}}'`: some buildx versions ignore that
# template and print the multi-line Name/MediaType/Digest table instead
# (docker/buildx#1175; the fix, docker/buildx#4006, was still open when this
# was written). `{{json .Manifest}}` is honored consistently, and the FIRST
# "digest" key in its output is the top-level manifest (index) digest -
# it precedes the nested "manifests" array. Extracted without jq.
imagetools_digest() {
  local ref="$1" out digest
  out="$(docker buildx imagetools inspect "${ref}" --format '{{json .Manifest}}' 2>/dev/null)" \
    || die "failed to resolve a digest for ${ref} - is 'docker buildx' available, and can this host reach its registry?"
  digest="$(printf '%s\n' "${out}" | grep -m1 -oE '"digest": *"sha256:[0-9a-f]{64}"' | grep -oE 'sha256:[0-9a-f]{64}')" || true
  [[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]] \
    || die "unexpected output from 'docker buildx imagetools inspect ${ref}': no top-level sha256 digest found (an old or unusual buildx version may format this differently; try upgrading docker buildx)"
  printf '%s' "${digest}"
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
  digest="$(imagetools_digest "${ref}")" || exit 1
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
  digest="$(imagetools_digest "${DIND_IMAGE_SOURCE}")" || exit 1
  log "Resolved ${DIND_IMAGE_SOURCE} -> ${digest}" >&2
  printf '%s@%s' "${DIND_IMAGE_SOURCE}" "${digest}"
}

# generate_provider_config writes the demo provider config.toml to its staging
# path (publish_staged_artifacts installs it). In
# "release" mode it pins runner_image by digest and leaves digest-pin
# enforcement ON (no allow_unpinned_runner_image) - this is meant to be
# exactly what a real operator's minimal config looks like (see
# examples/config.minimal.toml). In "source" mode it points at the local
# mutable dev tag, which requires the dev-only escape hatch.
generate_provider_config() {
  local runner_image="$1" dind_image="${2:-}"
  local cfg_tmp="${STAGE_CFG}"

  if [[ "${PROVIDER_SOURCE}" == "release" ]]; then
    cat > "${cfg_tmp}" <<EOF
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
    cat > "${cfg_tmp}" <<EOF
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
    cat >> "${cfg_tmp}" <<EOF

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
  # config.toml holds no secrets and must be readable by GARM's service
  # account, so it is 0644 regardless of the mode of any earlier file.
  chmod 0644 "${cfg_tmp}" || die "could not chmod ${cfg_tmp}"
  log "Staged provider config: ${STAGE_CFG}"
}

# smoke_test_binary runs the STAGED binary once with GARM_COMMAND=GetVersion
# against the STAGED config, so a broken download (wrong arch, a
# corrupted binary that passed the checksum check for the wrong reason -
# it should not, but this is cheap insurance) or a config the CURRENT
# loader rejects surfaces here, immediately, rather than opaquely inside
# GARM's own provider-subprocess log later.
smoke_test_binary() {
  log "Smoke-testing the staged provider binary (GARM_COMMAND=GetVersion) against the staged config..."
  # main.go writes its structured log lines to stderr and the GetVersion
  # result (a bare version string) to stdout (see main.go's run()), so stdout
  # alone is the clean "reported version" value; stderr is captured
  # separately and surfaced only on failure, for a readable error.
  local out err rc=0
  out="$(GARM_COMMAND=GetVersion \
      GARM_CONTROLLER_ID=demo-preflight \
      GARM_PROVIDER_CONFIG_FILE="${STAGE_CFG}" \
      "${STAGE_BIN}" 2>"${STAGE_CFG_DIR}/.smoke-test-stderr")" || rc=$?
  err="$(cat "${STAGE_CFG_DIR}/.smoke-test-stderr" 2>/dev/null)"
  rm -f "${STAGE_CFG_DIR}/.smoke-test-stderr"
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
        GARM_PROVIDER_CONFIG_FILE="${STAGE_CFG}" \
        GARM_POOL_EXTRASPECS="${DIND_EXTRA_SPECS}" \
        "${STAGE_BIN}" 2>&1 </dev/null)" || vrc=$?
    if [[ "${vrc}" -ne 0 ]]; then
      die "the provider rejected the DinD extra_specs ${DIND_EXTRA_SPECS} (exit ${vrc}): ${err}"
    fi
    ok "provider accepts the DinD extra_specs ${DIND_EXTRA_SPECS} (ValidatePoolInfo)"
  fi
}

# warn_if_unservice_readable_paths warns (never fails) when the provider
# binary or config directory sits under /tmp or the invoking user's home:
# GARM's service user typically cannot read a home directory, and /tmp is
# usually cleared on reboot. The defaults are kept for backward
# compatibility (they work when GARM runs as the same user, e.g. a
# containerised dev GARM); set PROVIDER_BIN_DIR/PROVIDER_CONFIG_DIR to a
# directory readable by GARM's service user otherwise (docs/demo-m0.md,
# Prerequisites).
warn_if_unservice_readable_paths() {
  local var dir
  for var in PROVIDER_BIN_DIR PROVIDER_CONFIG_DIR; do
    dir="${!var}"
    case "${dir}" in
      /tmp|/tmp/*|/private/tmp|/private/tmp/*)
        warn "${var} (${dir}) is under /tmp: it is usually cleared on reboot and may not be readable by GARM's service user. Use a directory readable by GARM's service user (see docs/demo-m0.md, Prerequisites)." ;;
      "${HOME}"|"${HOME}"/*)
        warn "${var} (${dir}) is under \$HOME: GARM's service user typically cannot read a home directory, so GARM may fail to execute the provider or read its config. Use a directory readable by GARM's service user (see docs/demo-m0.md, Prerequisites)." ;;
    esac
  done
}

step_preflight() {
  log "=== Step 0: preflight (source: ${PROVIDER_SOURCE}, target: ${GARM_TARGET}, variant: ${GATE_VARIANT}, arch: ${OS_ARCH}) ==="

  require_cmd docker
  warn_if_unservice_readable_paths

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

  # Everything below is staged; the live binary and config (which GARM may
  # already be using) are replaced only once every step has succeeded.
  create_staging

  local runner_image
  case "${PROVIDER_SOURCE}" in
    release)
      require_linux_host_for_release_mode
      download_provider_binary
      runner_image="$(resolve_runner_image_digest)" || exit 1
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
    dind_image="$(resolve_dind_image_digest)" || exit 1
  fi

  generate_provider_config "${runner_image}" "${dind_image}"
  smoke_test_binary
  # The modes describe the artifacts preflight just produced (binary, runner and
  # DinD image, config.toml), so they are published together with them and never
  # on their own: a failed preflight must not leave modes saved that the
  # installed config does not match. For `all`, later steps in this process
  # already use the effective modes held in the variables.
  stage_modes
  publish_staged_artifacts

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
  log "script); --os-arch ${OS_ARCH} was auto-detected from this host (OS_ARCH, if"
  log "set, must match it)."
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
    # Quoted YAML string (name is allowlisted to [A-Za-z0-9._-]): a name such
    # as true / null / 123 must stay a string for runs-on.
    runs_on="\"${SCALESET_NAME}\""
  else
    runs_on="$(tags_to_yaml_array "${POOL_TAGS}")" || exit 1
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
      - name: shared workspace is visible to, and writable by, the sidecar daemon
        run: |
          echo ok > "$GITHUB_WORKSPACE/gate-shared.txt"
          out="$(docker run --rm -v "$GITHUB_WORKSPACE":/w alpine:3.20 cat /w/gate-shared.txt)"
          echo "sidecar saw: ${out}"
          test "${out}" = ok
          docker run --rm -v "$GITHUB_WORKSPACE":/w alpine:3.20 sh -c 'echo nested > /w/gate-nested.txt'
          back="$(cat "$GITHUB_WORKSPACE/gate-nested.txt")"
          echo "runner read back: ${back}"
          test "${back}" = nested
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
# (RunnerWorkDir in mounts.go, DindSocketDir, DindStateDir and
# DindSocketGID in dind.go, RunnerExternalsDir in externals.go, CredentialDir
# in mounts.go).
RUNNER_WORKDIR="/actions-runner/_work"
DIND_SOCKET_DIR="/run"
DIND_STATE_DIR="/var/lib/docker"
RUNNER_EXTERNALS_DIR="/actions-runner/externals"
RUNNER_CRED_DIR="/run/garm"
DIND_SOCKET_GID="2000"

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

# container_networks_are_exactly succeeds when container "$1" is attached to
# exactly one network and it is named "$2" (the spec sets the per-job network
# as the SOLE attachment via NetworkMode).
container_networks_are_exactly() {
  [[ "$(container_networks "$1" | sed '/^$/d')" == "$2" ]]
}

# container_mount_dests prints the sorted mount destinations of container "$1".
container_mount_dests() {
  docker inspect -f '{{range .Mounts}}{{.Destination}}{{println}}{{end}}' "$1" | sed '/^$/d' | sort
}

# container_mount_rw prints "true"/"false" for the RW flag of the mount of
# container "$1" at destination "$2", or nothing when there is no such mount.
container_mount_rw() {
  docker inspect -f '{{range .Mounts}}{{.Destination}}|{{.RW}}{{println}}{{end}}' "$1" \
    | awk -F'|' -v d="$2" '$1 == d { print $2; exit }'
}

# verify_workspace_mount_rw checks that container "$1" ("$2" names it) has its
# workspace volume mounted READ-WRITE at RUNNER_WORKDIR. Both the runner and
# the DinD sidecar get the same NamedWorkspaceMount (internal/spec/mounts.go,
# NamedWorkspaceMount: no ReadOnly; used for the runner in mounts.go and for the
# sidecar in dind.go), and jobs, including nested containers started through
# the sidecar's daemon, write into the job workspace.
verify_workspace_mount_rw() {
  local rw
  rw="$(container_mount_rw "$1" "${RUNNER_WORKDIR}")"
  case "${rw}" in
    true)  ok "$2 mounts its workspace volume read-write at ${RUNNER_WORKDIR}" ;;
    false) bad "$2 mounts its workspace volume READ-ONLY at ${RUNNER_WORKDIR} (the spec mounts it read-write; job and nested-container writes into the workspace would fail)" ;;
    *)     bad "$2 has no mount at ${RUNNER_WORKDIR}, so its workspace read-write flag cannot be checked" ;;
  esac
}

# verify_no_extra_privileges checks "$1" ("$2" names it) has none of the
# privilege widenings the spec never sets on the runner: no added
# capabilities, devices or security options, and no host PID/IPC/UTS/user
# namespace.
verify_no_extra_privileges() {
  local cid="$1" what="$2" extras ns
  extras="$(docker inspect -f '{{len .HostConfig.CapAdd}} {{len .HostConfig.Devices}} {{len .HostConfig.SecurityOpt}}' "${cid}")"
  if [[ "${extras}" == "0 0 0" ]]; then
    ok "${what}: no added capabilities, devices or security options"
  else
    bad "${what}: unexpected CapAdd/Devices/SecurityOpt counts '${extras}' (spec sets none)"
  fi
  ns="$(docker inspect -f '{{.HostConfig.PidMode}}|{{.HostConfig.IpcMode}}|{{.HostConfig.UTSMode}}|{{.HostConfig.UsernsMode}}' "${cid}")"
  if printf '%s\n' "${ns}" | grep -qw host; then
    bad "${what}: shares a host namespace (${ns})"
  else
    ok "${what}: no host PID/IPC/UTS/user namespace"
  fi
}

# verify_ipv6_disabled checks "$1" ("$2" names it) cannot use ::1 as
# `localhost`. The job network has no IPv6; with ::1 live, a server bound to
# `localhost` would sit on ::1, unreachable for clients that skip it (Node,
# AI_ADDRCONFIG). Normally that holds because the runner is created with IPv6
# disabled in its network namespace (internal/spec/mounts.go, RunnerIPv6Sysctl),
# which leaves IPv6 off on lo. On a Docker host whose kernel has no IPv6 at all
# the daemon cannot apply that sysctl and the provider recreates the runner
# without it; there /proc/sys/net/ipv6 is absent, ::1 cannot exist, and both
# checks are reported as such rather than failed. /etc/hosts is only reported:
# Engine 25 and earlier keep a dead `::1 localhost` line, 26+ omits it.
verify_ipv6_disabled() {
  local cid="$1" what="$2" sysctl lo_ipv6 hosts_line
  sysctl="$(docker inspect -f '{{index .HostConfig.Sysctls "net.ipv6.conf.all.disable_ipv6"}}' "${cid}" 2>/dev/null)"
  # "1"/"0" from lo, "absent" when the kernel has no IPv6, "" if exec failed.
  lo_ipv6="$(docker exec "${cid}" sh -c 'if [ -d /proc/sys/net/ipv6 ]; then cat /proc/sys/net/ipv6/conf/lo/disable_ipv6; else echo absent; fi' 2>/dev/null)"

  if [[ "${sysctl}" == "1" ]]; then
    ok "${what}: net.ipv6.conf.all.disable_ipv6=1"
  elif [[ "${lo_ipv6}" == "absent" ]]; then
    warn "${what}: created without net.ipv6.conf.all.disable_ipv6: the Docker host has no IPv6, so the provider retried without the sysctl"
  else
    bad "${what}: net.ipv6.conf.all.disable_ipv6 is '${sysctl}' (the spec sets 1)"
  fi

  case "${lo_ipv6}" in
    1) ok "${what}: IPv6 is disabled on lo, so ::1 is not usable as localhost" ;;
    absent) ok "${what}: the host kernel has no IPv6, so ::1 is not usable as localhost" ;;
    *) bad "${what}: lo disable_ipv6 is '${lo_ipv6}' (want 1; ::1 would be usable as localhost)" ;;
  esac

  hosts_line="$(docker exec "${cid}" grep '^::1' /etc/hosts 2>/dev/null)"
  log "${what}: /etc/hosts ::1 line: ${hosts_line:-(none)}"
}

# cache_disabled_in_config succeeds when the provider config file sets
# `enabled = false` inside its [cache] table. [cache].enabled defaults to true
# (internal/config/cache.go defaultCache, wired in config.go), and when the
# cache is enabled CreateInstance provisions and mounts the shared externals
# volume for EVERY allocation regardless of entity scope
# (internal/provider/create.go, the "4b. Externals cache" block gated only on
# p.cfg.Cache.Enabled). The generated gate config never touches [cache].
cache_disabled_in_config() {
  [[ -f "${PROVIDER_CONFIG_FILE}" ]] || return 1
  awk '
    /^[[:space:]]*\[/ { in_cache = ($0 ~ /^[[:space:]]*\[cache\][[:space:]]*(#.*)?$/) }
    in_cache && /^[[:space:]]*enabled[[:space:]]*=[[:space:]]*false([[:space:]]*(#.*)?)?$/ { found = 1 }
    END { exit !found }
  ' "${PROVIDER_CONFIG_FILE}"
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
    if container_networks_are_exactly "${cid}" "${net_name}"; then
      ok "runner container's only network is the per-job network"
    else
      bad "runner container is attached to more than the per-job network"
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
  verify_workspace_mount_rw "${cid}" "runner container"

  local tmpfs_opts
  tmpfs_opts="$(docker inspect -f '{{index .HostConfig.Tmpfs "'"${RUNNER_CRED_DIR}"'"}}' "${cid}" 2>/dev/null)"
  if [[ -n "${tmpfs_opts}" ]] \
      && [[ ",${tmpfs_opts}," == *,noexec,* && ",${tmpfs_opts}," == *,nosuid,* && ",${tmpfs_opts}," == *,nodev,* \
            && ",${tmpfs_opts}," == *,size=16777216,* && ",${tmpfs_opts}," == *,mode=0700,* && ",${tmpfs_opts}," == *,uid=1001,* && ",${tmpfs_opts}," == *,gid=1001,* ]]; then
    ok "credential tmpfs at ${RUNNER_CRED_DIR} is noexec,nosuid,nodev, size=16777216 (16 MiB), mode 0700, owned by the runner uid/gid"
  else
    bad "credential tmpfs at ${RUNNER_CRED_DIR} has unexpected options: '${tmpfs_opts}'"
  fi

  local ext_rw
  ext_rw="$(container_mount_rw "${cid}" "${RUNNER_EXTERNALS_DIR}")"
  case "${ext_rw}" in
    false) ok "shared externals volume is mounted read-only at ${RUNNER_EXTERNALS_DIR}" ;;
    true)  bad "shared externals volume at ${RUNNER_EXTERNALS_DIR} is mounted READ-WRITE (it is shared across repositories and must be read-only)" ;;
    *)
      if cache_disabled_in_config; then
        log "  (informational, not PASS/FAIL: no volume mounted at ${RUNNER_EXTERNALS_DIR} - [cache] enabled = false in ${PROVIDER_CONFIG_FILE})"
      else
        bad "no volume mounted at ${RUNNER_EXTERNALS_DIR}, but the cache is enabled (the default) and externals are mounted for every allocation when it is"
      fi
      ;;
  esac

  verify_no_extra_privileges "${cid}" "runner container"
  verify_ipv6_disabled "${cid}" "runner container"

  local cache_vols scoped_cache
  scoped_cache="$(job_scoped_cache_volumes)"
  if [[ -n "${scoped_cache}" ]]; then
    bad "job-scoped volume(s) also carry garm.docker/cache=true (teardown would skip them, so they would leak): $(printf '%s' "${scoped_cache}" | tr '\n' ' ')"
  else
    ok "no job-scoped volume carries garm.docker/cache=true (cache volumes have no instance-name)"
  fi
  cache_vols="$(persistent_cache_volumes | sed '/^$/d' | wc -l | tr -d ' ')"
  log "  (informational, not PASS/FAIL: ${cache_vols} persistent cache volume(s) present - toolcache/pnpm/externals/diag persist across jobs by design, ADR-003)"
}

# job_scoped_cache_volumes prints the names of managed volumes that carry
# garm.docker/cache=true AND garm.docker/instance-name. That combination must
# never exist: a persistent cache volume carries cache=true and NO
# instance-name (internal/spec/labels.go: LabelCache, MatchesPredicate), while
# the job-scoped workspace/socket/dind-state volumes carry an instance-name and
# must be torn down. A job-scoped volume that also gained cache=true would be
# skipped by teardown (MatchesPredicate rejects cache=true), i.e. leak.
job_scoped_cache_volumes() {
  docker volume ls \
    --filter 'label=garm.docker/managed=true' \
    --filter 'label=garm.docker/cache=true' \
    --filter 'label=garm.docker/instance-name' \
    --format '{{.Name}}'
}

# persistent_cache_volumes prints the names of the legitimate persistent cache
# volumes (ADR-003): managed, cache=true, and no instance-name. Volumes that
# carry an instance-name are deliberately NOT exempted here, whatever else
# they are labeled.
persistent_cache_volumes() {
  local all scoped
  all="$(docker volume ls --filter 'label=garm.docker/managed=true' --filter 'label=garm.docker/cache=true' --format '{{.Name}}')"
  scoped="$(job_scoped_cache_volumes)"
  if [[ -n "${scoped}" ]]; then
    printf '%s\n' "${all}" | command grep -vxF -f <(printf '%s\n' "${scoped}") || true
  else
    printf '%s\n' "${all}"
  fi
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

  local ws_vol
  ws_vol="$(instance_volume_name "${instance}" workspace)"
  if [[ -n "${ws_vol}" ]] && container_has_volume_mount "${did}" "${ws_vol}" "${RUNNER_WORKDIR}"; then
    ok "sidecar mounts the labeled workspace volume ${ws_vol} at ${RUNNER_WORKDIR}"
  else
    bad "sidecar does not mount the labeled workspace volume '${ws_vol}' at ${RUNNER_WORKDIR} (a nested 'docker run -v \"\$PWD\":/work' would bind an empty path)"
  fi
  verify_workspace_mount_rw "${did}" "sidecar"

  if [[ "$(container_mount_dests "${did}" | tr '\n' ' ')" == "${RUNNER_WORKDIR} ${DIND_SOCKET_DIR} ${DIND_STATE_DIR} " ]]; then
    ok "sidecar has exactly the socket, dind-state and workspace mounts (no cache volumes)"
  else
    bad "sidecar mount destinations are not exactly {${RUNNER_WORKDIR}, ${DIND_SOCKET_DIR}, ${DIND_STATE_DIR}}: $(container_mount_dests "${did}" | tr '\n' ' ')"
  fi
  if docker inspect -f '{{range .HostConfig.GroupAdd}}{{.}}{{println}}{{end}}' "${cid}" | grep -qx "${DIND_SOCKET_GID}"; then
    ok "runner has supplementary group ${DIND_SOCKET_GID} (the dockerd socket group)"
  else
    bad "runner is missing supplementary group ${DIND_SOCKET_GID} (the unprivileged runner user could not use the socket)"
  fi
  local sns
  sns="$(docker inspect -f '{{.HostConfig.PidMode}}|{{.HostConfig.IpcMode}}|{{.HostConfig.UTSMode}}|{{.HostConfig.UsernsMode}}' "${did}")"
  if printf '%s\n' "${sns}" | grep -qw host; then
    bad "sidecar shares a host namespace (${sns})"
  else
    ok "sidecar: no host PID/IPC/UTS/user namespace (privileged, but not sharing host namespaces)"
  fi

  local state_vol
  state_vol="$(instance_volume_name "${instance}" dind-state)"
  if [[ -n "${state_vol}" ]] && container_has_volume_mount "${did}" "${state_vol}" "${DIND_STATE_DIR}"; then
    ok "sidecar mounts the labeled dind-state volume ${state_vol} at ${DIND_STATE_DIR}"
  else
    bad "sidecar does not mount the labeled dind-state volume '${state_vol}' at ${DIND_STATE_DIR}"
  fi

  local net_name sock_vol
  net_name="$(job_network_name "${instance}")"
  if [[ -n "${net_name}" ]] && container_networks "${did}" | grep -qxF "${net_name}"; then
    ok "sidecar is attached to the same labeled per-job network ${net_name}"
  else
    bad "sidecar is not attached to the labeled per-job network '${net_name}'"
  fi
  if [[ -n "${net_name}" ]] && container_networks_are_exactly "${did}" "${net_name}"; then
    ok "sidecar's only network is the per-job network (not attached to the default 'bridge')"
  else
    bad "sidecar is attached to more than the per-job network (or to the default 'bridge'): $(container_networks "${did}" | tr '\n' ' ')"
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

  # Persistent cache volumes (garm.docker/cache=true and NO instance-name,
  # ADR-003) are excluded by label, not by name: they survive jobs by design
  # and are reported informationally. A volume that carries an instance-name is
  # job-scoped even if it also has cache=true (a labeling regression), so it is
  # NOT exempted and shows up as leftover. Everything else managed must be gone.
  local leftover_c leftover_n leftover_v cache_v
  leftover_c="$(docker ps -a --filter 'label=garm.docker/managed=true' --format '{{.Names}}')"
  leftover_n="$(docker network ls --filter 'label=garm.docker/managed=true' --format '{{.Name}}')"
  cache_v="$(persistent_cache_volumes)"
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
    log "info: persistent cache volumes (garm.docker/cache=true, no instance-name, kept by design):"
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

The modes chosen for preflight (or all) are remembered, so later single steps
(pool, watch, verify, ...) use them without repeating the flags; an explicit
flag that conflicts with the remembered mode is an error (re-run preflight).

See docs/demo-m0.md for prerequisites, environment variables, what each
check proves, and a troubleshooting table. No secret is ever read,
generated, or printed by this script.
EOF
}

COMMAND="all"

while [[ $# -gt 0 ]]; do
  case "$1" in
    -y|--yes) ASSUME_YES=1; shift ;;
    --from-source) PROVIDER_SOURCE="source"; EXPL_PROVIDER_SOURCE="source"; shift ;;
    --version)
      [[ $# -ge 2 ]] || die "--version requires an argument, e.g. --version v0.2.1"
      PROVIDER_VERSION="$2"
      EXPL_PROVIDER_VERSION="$2"
      shift 2
      ;;
    --pool) GARM_TARGET="pool"; EXPL_GARM_TARGET="pool"; shift ;;
    --dind) GATE_VARIANT="dind"; EXPL_GATE_VARIANT="dind"; shift ;;
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
  init_arch
  validate_modes
  case "${COMMAND}" in
    preflight|all) ;; # these define the modes; step_preflight saves them on success
    *) load_saved_modes; validate_modes ;;
  esac
  # In scale-set mode the scale set name is fixed up front, by preflight/all
  # (generating and saving one if needed) or by any command given an explicit
  # SCALESET_NAME, so every later step agrees from the start. Unlike the
  # modes, the name describes no generated artifact (it is only the label the
  # workflow targets), so saving it before preflight's downloads and builds
  # cannot leave anything inconsistent. Pool mode has no scale set name and
  # never creates the file.
  if [[ "${GARM_TARGET}" == "scaleset" ]]; then
    case "${COMMAND}" in
      preflight|all) resolve_scaleset_name ;;
      *) [[ -z "${SCALESET_NAME}" ]] || resolve_scaleset_name ;;
    esac
  fi
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
