#!/usr/bin/env bash
# entrypoint.sh - garm-provider-docker runner image entrypoint.
#
# Replaces the myoung34/github-runner base image's own config.sh-based
# entrypoint entirely (ADR-002). This script never fetches JIT config or a
# registration token itself: the provider fetches those out-of-band and
# streams them into this container's memory-backed tmpfs (/run/garm) via a
# `docker exec`-run `tar -x` after start (ADR-002 F1). The contract
# implemented here is:
#
#   1. Wait for the provider's atomic delivery marker (/run/garm/.delivered).
#   2. In DinD modes (DOCKER_HOST set), wait for the Docker daemon to answer,
#      independently of and concurrently with step 1.
#   3. Install the credentials and exec the runner:
#      - JIT: symlink the tmpfs files into the install dir and exec run.sh
#        directly (no config.sh, no --jitconfig).
#      - non-JIT: pre-create the credential symlinks onto the tmpfs, then run
#        config.sh (as the runner user via gosu) to register so it writes the
#        generated credentials THROUGH the symlinks onto the tmpfs directly.
#        A scrub-on-failure trap guards the phase so no failure path leaves
#        credentials on the writable layer.
#
# Credentials never land on the container's disk-backed writable layer: they
# stay resident on the memory-backed tmpfs, reached from the install dir only
# through symlinks (ADR-002 F2). Never echo/log credential file contents.

set -euo pipefail

readonly CRED_DIR="/run/garm"
readonly RUNNER_DIR="/actions-runner"
readonly RUNNER_USER="runner"

# ReadyMarker: the provider writes this file as the LAST entry of the
# credential tar, so it appears only once every credential file is fully
# delivered (ADR-002 F1). Waiting for it — instead of the individual files —
# means this entrypoint never observes a partial credential set.
readonly READY_MARKER=".delivered"

readonly CRED_WAIT_SECONDS="${GARM_CRED_WAIT_SECONDS:-120}"
readonly DOCKER_WAIT_SECONDS="${WAIT_FOR_DOCKER_SECONDS:-120}"
readonly POLL_INTERVAL_SECONDS=1

log() {
  printf '[entrypoint] %s\n' "$*" >&2
}

fail() {
  log "ERROR: $*"
  exit 1
}

# wait_for_files polls CRED_DIR, bounded by $1 seconds, until every filename
# in the remaining arguments exists there.
wait_for_files() {
  local timeout="$1"
  shift
  local elapsed=0
  local all_present
  local f

  while true; do
    all_present=true
    for f in "$@"; do
      [[ -e "${CRED_DIR}/${f}" ]] || { all_present=false; break; }
    done
    [[ "${all_present}" == "true" ]] && return 0
    (( elapsed >= timeout )) && return 1
    sleep "${POLL_INTERVAL_SECONDS}"
    elapsed=$(( elapsed + POLL_INTERVAL_SECONDS ))
  done
}

# wait_for_docker_ready polls `docker info`, bounded by $1 seconds, until the
# daemon reachable via DOCKER_HOST answers successfully.
wait_for_docker_ready() {
  local timeout="$1"
  local elapsed=0

  while ! docker info >/dev/null 2>&1; do
    (( elapsed >= timeout )) && return 1
    sleep "${POLL_INTERVAL_SECONDS}"
    elapsed=$(( elapsed + POLL_INTERVAL_SECONDS ))
  done
  return 0
}

# require_gosu fails CLOSED: if gosu is not on PATH we cannot drop privileges,
# so we refuse to run rather than silently continue as root (ADR-002 F11).
require_gosu() {
  command -v gosu >/dev/null 2>&1 && return 0
  fail "gosu is required to drop privileges to ${RUNNER_USER} but was not found on PATH; refusing to run as root (set RUN_AS_ROOT=true to override deliberately)"
}

# Step 1: credential wait. Both modes wait for the single atomic delivery
# marker the provider writes last (ADR-002 F1).
wait_for_credentials() {
  log "waiting up to ${CRED_WAIT_SECONDS}s for the delivery marker ${CRED_DIR}/${READY_MARKER}"
  wait_for_files "${CRED_WAIT_SECONDS}" "${READY_MARKER}" \
    || fail "timed out waiting for credential delivery marker ${READY_MARKER} under ${CRED_DIR}"
}

# Step 2: Docker readiness, DinD modes only. Independent of, and run
# concurrently with, step 1 (ADR-001, ADR-002) - skipped entirely when
# DOCKER_HOST is unset (none mode).
maybe_wait_for_docker() {
  if [[ -z "${DOCKER_HOST:-}" ]]; then
    log "DOCKER_HOST unset (none mode): skipping Docker readiness wait"
    return 0
  fi

  log "waiting up to ${DOCKER_WAIT_SECONDS}s for Docker daemon at ${DOCKER_HOST}"
  wait_for_docker_ready "${DOCKER_WAIT_SECONDS}" \
    || fail "timed out waiting for Docker daemon readiness at ${DOCKER_HOST}"
}

# run_concurrent_waits backgrounds both waits and fails fast: as soon as
# either job fails, the other is killed rather than left to run out its own
# timeout.
run_concurrent_waits() {
  wait_for_credentials &
  local cred_pid=$!
  maybe_wait_for_docker &
  local docker_pid=$!

  local status=0
  wait -n || status=$?
  if (( status != 0 )); then
    kill "${cred_pid}" "${docker_pid}" 2>/dev/null || true
    exit "${status}"
  fi

  status=0
  wait -n || status=$?
  (( status != 0 )) && exit "${status}"
  return 0
}

# resolve_workdir mirrors the base image's own convention: an absolute
# RUNNER_WORKDIR is used as-is, a relative one is relative to RUNNER_DIR
# (which is also the entrypoint's cwd by the time this runs).
resolve_workdir() {
  local workdir="${RUNNER_WORKDIR:-_work}"
  case "${workdir}" in
    /*) printf '%s' "${workdir}" ;;
    *) printf '%s/%s' "${RUNNER_DIR}" "${workdir}" ;;
  esac
}

# prepare_workdir honors RUNNER_WORKDIR (always set per ADR-002) by ensuring
# the directory exists and is owned by the runner user, even in JIT mode where
# the actual job workdir is baked into the JIT config rather than read from
# this env var at run.sh startup.
prepare_workdir() {
  local workdir
  workdir="$(resolve_workdir)"
  mkdir -p "${workdir}"
  chown "${RUNNER_USER}:${RUNNER_USER}" "${workdir}"
}

# install_jit_credentials points the runner install dir at the provider-
# delivered credential files via SYMLINKS. The credential files themselves
# stay resident on the memory-backed tmpfs — only the symlinks live on the
# container's writable layer, so no credential ever lands on host-backed disk
# (ADR-002 F2). No config.sh, no --jitconfig. run.sh reads .runner /
# .credentials / .credentials_rsaparams and follows the symlinks; the tmpfs
# files are owned by (and readable by) the runner user the provider's delivery
# exec created them as.
install_jit_credentials() {
  log "linking JIT credential files from ${CRED_DIR} into ${RUNNER_DIR}"
  ln -sfn "${CRED_DIR}/runner" "${RUNNER_DIR}/.runner"
  ln -sfn "${CRED_DIR}/credentials" "${RUNNER_DIR}/.credentials"
  ln -sfn "${CRED_DIR}/credentials_rsaparams" "${RUNNER_DIR}/.credentials_rsaparams"
  prepare_workdir
}

# build_non_jit_url reconstructs the GitHub scope URL config.sh expects from
# GITHUB_URL (host only, per internal/spec/env.go's BuildRunnerEnv) plus
# whichever of RUNNER_ENTERPRISE / RUNNER_ORG+RUNNER_REPO / RUNNER_ORG the
# provider set for this entity scope.
build_non_jit_url() {
  local host="${GITHUB_URL:?GITHUB_URL is required}"
  host="${host%/}"

  if [[ -n "${RUNNER_ENTERPRISE:-}" ]]; then
    printf '%s/enterprises/%s' "${host}" "${RUNNER_ENTERPRISE}"
  elif [[ -n "${RUNNER_REPO:-}" ]]; then
    printf '%s/%s/%s' "${host}" "${RUNNER_ORG:?RUNNER_ORG is required with RUNNER_REPO}" "${RUNNER_REPO}"
  elif [[ -n "${RUNNER_ORG:-}" ]]; then
    printf '%s/%s' "${host}" "${RUNNER_ORG}"
  else
    fail "none of RUNNER_ENTERPRISE, RUNNER_ORG/RUNNER_REPO, or RUNNER_ORG set for non-JIT registration"
  fi
}

# own_runner_dir_as_runner chowns the install dir's top-level entries, skipping
# the large image-shipped bin/externals trees that already ship runner-owned
# (mirrors the base image's own optimization: recursing over those defeats
# overlay copy-up performance for no ownership benefit). It runs before
# config.sh so the runner user (via gosu) can write .runner/.credentials* into
# the install dir.
own_runner_dir_as_runner() {
  chown "${RUNNER_USER}:${RUNNER_USER}" "${RUNNER_DIR}"
  find "${RUNNER_DIR}" -mindepth 1 -maxdepth 1 \
    ! -name bin ! -name externals \
    -exec chown -R "${RUNNER_USER}:${RUNNER_USER}" {} +
}

# run_config_sh runs config.sh with privilege handling that satisfies the base
# runner's own root guard (ADR-002 F4): by default it drops to the runner user
# via gosu (config.sh refuses to run as root without RUNNER_ALLOW_RUNASROOT);
# only when RUN_AS_ROOT=true is set deliberately does it run as root with
# RUNNER_ALLOW_RUNASROOT=1. Fails CLOSED when gosu is missing (F11).
run_config_sh() {
  if [[ "${RUN_AS_ROOT:-false}" == "true" ]]; then
    log "RUN_AS_ROOT=true: running config.sh as $(id -un) with RUNNER_ALLOW_RUNASROOT=1"
    RUNNER_ALLOW_RUNASROOT=1 ./config.sh "$@"
    return
  fi
  require_gosu
  log "running config.sh as ${RUNNER_USER} via gosu"
  gosu "${RUNNER_USER}" ./config.sh "$@"
}

# prelink_non_jit_credentials pre-creates the dotted-name credential symlinks
# (pointing at the tmpfs bare-name targets, exactly like install_jit_credentials)
# BEFORE config.sh runs, so config.sh writes its generated
# .runner/.credentials/.credentials_rsaparams THROUGH the symlinks directly onto
# the memory-backed tmpfs. This makes the relocation failure-atomic: there is no
# window in which the credentials sit on the disk-backed writable layer waiting
# to be moved — they land on tmpfs from the first write (ADR-002 F2).
#
# M0-demo verification item: this assumes the .NET runner's config.sh writes
# these files in place (open+write through the symlink) rather than
# rename-over-target, which would replace the symlink with a real file on the
# writable layer. This is to be verified on the real runner during the M0 demo,
# alongside the existing JIT symlink-compatibility item; the
# scrub-on-failure trap below is the belt-and-braces safety net for the failure
# path, and that demo verification is the check for the success path.
prelink_non_jit_credentials() {
  ln -sfn "${CRED_DIR}/runner" "${RUNNER_DIR}/.runner"
  ln -sfn "${CRED_DIR}/credentials" "${RUNNER_DIR}/.credentials"
  ln -sfn "${CRED_DIR}/credentials_rsaparams" "${RUNNER_DIR}/.credentials_rsaparams"
}

# scrub_non_jit_credentials_on_failure is the EXIT-trap belt-and-braces for the
# non-JIT config phase: if config.sh fails or is interrupted, it removes any
# credential-pattern files — the dotted install-dir names AND their tmpfs
# bare-name counterparts — so no failure path leaves credentials on the
# container's disk-backed writable layer (ADR-002 F2). It preserves the
# triggering exit code so a failed config still exits non-zero for GARM.
scrub_non_jit_credentials_on_failure() {
  local code=$?
  if (( code == 0 )); then
    return 0
  fi
  local f
  for f in .runner .credentials .credentials_rsaparams; do
    # ${f#.} strips the leading dot to the tmpfs bare name (runner, etc.).
    rm -f "${RUNNER_DIR}/${f}" "${CRED_DIR}/${f#.}"
  done
  log "config.sh failed (exit ${code}); scrubbed credential-pattern files from the writable layer"
  return "${code}"
}

# install_non_jit_registration registers the runner with config.sh (as the
# runner user via gosu by default, ADR-002 F4), appending --disableupdate
# unconditionally because the image is digest-managed (ADR-002 F9). It
# pre-links the credential paths onto the tmpfs BEFORE config.sh so the
# generated credentials are written through onto tmpfs directly (never
# relocated after the fact), and guards the config phase with a scrub-on-failure
# trap so no failure path leaves credentials on the writable layer.
install_non_jit_registration() {
  local token url
  token="$(cat "${CRED_DIR}/registration-token")"
  url="$(build_non_jit_url)"

  local args=(
    --unattended
    --url "${url}"
    --token "${token}"
    --name "${RUNNER_NAME:?RUNNER_NAME is required}"
    --work "${RUNNER_WORKDIR:-_work}"
    --disableupdate
  )
  [[ -n "${RUNNER_GROUP:-}" ]] && args+=(--runnergroup "${RUNNER_GROUP}")
  [[ -n "${RUNNER_LABELS:-}" ]] && args+=(--labels "${RUNNER_LABELS}")
  [[ "${RUNNER_NO_DEFAULT_LABELS:-false}" == "true" ]] && args+=(--no-default-labels)
  [[ "${RUNNER_EPHEMERAL:-false}" == "true" ]] && args+=(--ephemeral)

  own_runner_dir_as_runner
  prelink_non_jit_credentials

  # Scrub credential-pattern files from the writable layer (and tmpfs) if
  # config.sh fails or is interrupted, then clear the trap on success so the
  # tmpfs-resident credentials are kept.
  trap scrub_non_jit_credentials_on_failure EXIT
  log "registering runner via config.sh (non-JIT)"
  run_config_sh "${args[@]}"
  trap - EXIT

  prepare_workdir
}

# exec_as_runner drops root via gosu (mirroring the base image's own
# convention) unless RUN_AS_ROOT=true, then execs the runner in the foreground
# so it receives signals directly. Fails CLOSED when gosu is missing (F11).
exec_as_runner() {
  if [[ "${RUN_AS_ROOT:-false}" == "true" ]]; then
    log "RUN_AS_ROOT=true: running as $(id -un)"
    exec "$@"
  fi

  require_gosu
  log "dropping privileges to ${RUNNER_USER} via gosu"
  exec gosu "${RUNNER_USER}" "$@"
}

main() {
  cd "${RUNNER_DIR}"

  run_concurrent_waits

  if [[ "${JIT_CONFIG_ENABLED:-false}" == "true" ]]; then
    install_jit_credentials
  else
    install_non_jit_registration
  fi

  exec_as_runner ./run.sh
}

main "$@"
