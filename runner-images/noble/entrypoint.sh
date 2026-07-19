#!/usr/bin/env bash
# entrypoint.sh - garm-provider-docker runner image entrypoint.
#
# Replaces the myoung34/github-runner base image's own config.sh-based
# entrypoint entirely (ADR-002). This script never fetches JIT config or
# a registration token itself: the provider fetches those out-of-band and
# streams them into this container's tmpfs via `docker cp` after start
# (ADR-002 step 2). The contract implemented here is:
#
#   1. Wait for the provider to deliver credential files under /run/garm.
#   2. In DinD modes (DOCKER_HOST set), wait for the Docker daemon to
#      answer, independently of and concurrently with step 1.
#   3. Install the credentials and exec the runner: JIT mode execs
#      run.sh directly (no config.sh, no --jitconfig); non-JIT mode runs
#      config.sh with a registration token first.
#
# Never echo/log credential file contents.

set -euo pipefail

readonly CRED_DIR="/run/garm"
readonly RUNNER_DIR="/actions-runner"
readonly RUNNER_USER="runner"

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

# wait_for_files polls CRED_DIR, bounded by $1 seconds, until every
# filename in the remaining arguments exists there.
wait_for_files() {
  local timeout="$1"
  shift
  local elapsed=0
  local all_present
  local f

  while true; do
    all_present=true
    for f in "$@"; do
      [[ -f "${CRED_DIR}/${f}" ]] || { all_present=false; break; }
    done
    [[ "${all_present}" == "true" ]] && return 0
    (( elapsed >= timeout )) && return 1
    sleep "${POLL_INTERVAL_SECONDS}"
    elapsed=$(( elapsed + POLL_INTERVAL_SECONDS ))
  done
}

# wait_for_docker_ready polls `docker info`, bounded by $1 seconds, until
# the daemon reachable via DOCKER_HOST answers successfully.
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

# Step 1: credential wait. JIT mode waits for the three files the
# provider docker-cp's in; non-JIT waits for a single registration token.
wait_for_credentials() {
  if [[ "${JIT_CONFIG_ENABLED:-false}" == "true" ]]; then
    log "waiting up to ${CRED_WAIT_SECONDS}s for JIT credential files under ${CRED_DIR}"
    wait_for_files "${CRED_WAIT_SECONDS}" runner credentials credentials_rsaparams \
      || fail "timed out waiting for JIT credential files (runner, credentials, credentials_rsaparams) under ${CRED_DIR}"
  else
    log "waiting up to ${CRED_WAIT_SECONDS}s for registration token under ${CRED_DIR}"
    wait_for_files "${CRED_WAIT_SECONDS}" registration-token \
      || fail "timed out waiting for registration-token under ${CRED_DIR}"
  fi
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
# either job fails, the other is killed rather than left to run out its
# own timeout.
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

# prepare_workdir honors RUNNER_WORKDIR (always set per ADR-002) by
# ensuring the directory exists and is owned by the runner user, even in
# JIT mode where the actual job workdir is baked into the JIT config
# rather than read from this env var at run.sh startup.
prepare_workdir() {
  local workdir
  workdir="$(resolve_workdir)"
  mkdir -p "${workdir}"
  chown "${RUNNER_USER}:${RUNNER_USER}" "${workdir}"
}

# install_jit_credentials copies the three provider-delivered credential
# files into the runner's install dir under the filenames run.sh expects,
# owned by the runner user. No config.sh, no --jitconfig (ADR-002).
install_jit_credentials() {
  log "installing JIT credential files into ${RUNNER_DIR}"
  install -o "${RUNNER_USER}" -g "${RUNNER_USER}" -m 0600 \
    "${CRED_DIR}/runner" "${RUNNER_DIR}/.runner"
  install -o "${RUNNER_USER}" -g "${RUNNER_USER}" -m 0600 \
    "${CRED_DIR}/credentials" "${RUNNER_DIR}/.credentials"
  install -o "${RUNNER_USER}" -g "${RUNNER_USER}" -m 0600 \
    "${CRED_DIR}/credentials_rsaparams" "${RUNNER_DIR}/.credentials_rsaparams"
  prepare_workdir
}

# build_non_jit_url reconstructs the GitHub scope URL config.sh expects
# from GITHUB_URL (host only, per internal/spec/env.go's BuildRunnerEnv)
# plus whichever of RUNNER_ENTERPRISE / RUNNER_ORG+RUNNER_REPO / RUNNER_ORG
# the provider set for this entity scope.
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

# own_runner_dir_as_runner chowns the install dir's top-level entries,
# skipping the large image-shipped bin/externals trees that already ship
# runner-owned (mirrors the base image's own optimization: recursing over
# those defeats overlay copy-up performance for no ownership benefit).
own_runner_dir_as_runner() {
  chown "${RUNNER_USER}:${RUNNER_USER}" "${RUNNER_DIR}"
  find "${RUNNER_DIR}" -mindepth 1 -maxdepth 1 \
    ! -name bin ! -name externals \
    -exec chown -R "${RUNNER_USER}:${RUNNER_USER}" {} +
}

# install_non_jit_registration runs config.sh with a registration token
# fetched the same provider-side, never-a-host-temp-file way as JIT mode
# (ADR-002's non-JIT fallback), then execs run.sh.
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
  )
  [[ -n "${RUNNER_GROUP:-}" ]] && args+=(--runnergroup "${RUNNER_GROUP}")
  [[ -n "${RUNNER_LABELS:-}" ]] && args+=(--labels "${RUNNER_LABELS}")
  [[ "${RUNNER_NO_DEFAULT_LABELS:-false}" == "true" ]] && args+=(--no-default-labels)
  [[ "${RUNNER_EPHEMERAL:-false}" == "true" ]] && args+=(--ephemeral)

  log "registering runner via config.sh (non-JIT)"
  ./config.sh "${args[@]}"

  own_runner_dir_as_runner
  prepare_workdir
}

# exec_as_runner drops root via gosu (mirroring the base image's own
# convention) unless RUN_AS_ROOT=true, then execs the runner in the
# foreground so it receives signals directly.
exec_as_runner() {
  if [[ "${RUN_AS_ROOT:-false}" == "true" ]]; then
    log "RUN_AS_ROOT=true: running as $(id -un)"
    exec "$@"
  fi

  if command -v gosu >/dev/null 2>&1; then
    log "dropping privileges to ${RUNNER_USER} via gosu"
    exec gosu "${RUNNER_USER}" "$@"
  fi

  # TODO(M0): gosu not found on PATH; falling back to root rather than
  # failing the runner outright. Revisit before release - every other
  # code path assumes gosu is present (it ships in the base image).
  log "WARNING: gosu unavailable, running as $(id -un) (TODO(M0): fix before release)"
  exec "$@"
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
