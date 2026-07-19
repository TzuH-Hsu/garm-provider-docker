#!/usr/bin/env bash
#
# scripts/demo-m0.sh - M0 end-to-end demo harness for garm-provider-docker.
#
# Drives docs/plan.md's M0 item 9: point the built provider at a real GARM
# instance and a throwaway test repository running a trivial echo workflow,
# and walk the full lifecycle - CreateInstance -> JIT registration -> job
# runs -> DeleteInstance -> runner confirmed gone from GitHub - while
# checking the M0 go/no-go gate (docs/plan.md §3 M0) and the two ADR-002
# M0-demo verification items (exec-tar-into-tmpfs credential delivery,
# actions/runner symlink-compat) along the way.
#
# See docs/demo-m0.md for the full runbook: prerequisites, what each check
# proves, and a troubleshooting table. This script only prints instructions
# and runs read-mostly checks; it never modifies GARM's own config or your
# GitHub repository for you - those steps are always a manual, confirmed
# action, by design.
#
# Assumptions:
#   - You are on a Linux Docker host (or macOS + Docker Desktop).
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
#   scripts/demo-m0.sh [-y|--yes] [COMMAND]
#
# COMMAND (default: all):
#   preflight   Step 0: environment checks; build provider binary + runner
#               image; generate a demo provider config.toml
#   config      Step 1: print the config.toml [[provider]] snippet to
#               register with GARM, and pause for confirmation
#   pool        Step 2: print the garm-cli pool add command for your test
#               repo, and pause for confirmation
#   watch       Step 3: watch docker ps / garm-cli runner list; print the
#               sample workflow yaml to trigger
#   verify      Step 4 (a-d): PASS/FAIL checks against the live runner
#               container
#   postjob     Step 4 (e): PASS/FAIL checks after the job has finished
#   cleanup     Print manual cleanup commands
#   all         Run every step above, in order
#
# Flags:
#   -y, --yes   Auto-confirm every pause (non-interactive)
#   -h, --help  Show usage
#
# Environment variables (all optional unless noted - see docs/demo-m0.md):
#   GARM_URL              Base URL of the running GARM server. Informational
#                          only (Step 0 reachability probe); garm-cli itself
#                          authenticates via its own stored profile.
#                          Default: http://localhost:8080
#   REPO                  GARM repo identifier (UUID from `garm-cli repo
#                          list`, or "owner/name") of the THROWAWAY test
#                          repository. Required for pool/watch/all.
#   PROVIDER_NAME         Name this provider is registered under in GARM's
#                          config.toml [[provider]] section.
#                          Default: docker_demo
#   PROVIDER_BIN_DIR      Directory the provider binary is built into.
#                          Default: <repo>/bin
#   PROVIDER_CONFIG_DIR   Directory the generated demo provider config.toml
#                          is written into. Default: ${TMPDIR:-/tmp}/garm-provider-docker-demo
#   RUNNER_IMAGE_TAG      Local tag for the runner image build.
#                          Default: garm-runner-noble:demo
#   POOL_TAGS             Comma-separated tags applied to the pool; matched
#                          by the sample workflow's runs-on.
#                          Default: docker,garm-demo
#   POOL_IMAGE, POOL_FLAVOR
#                          Values for garm-cli pool add's required --image/
#                          --flavor flags. Informational only for this
#                          provider in M0 (see Step 2's own note).
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

PROVIDER_NAME="${PROVIDER_NAME:-docker_demo}"

PROVIDER_BIN_DIR="${PROVIDER_BIN_DIR:-${REPO_ROOT}/bin}"
PROVIDER_BIN="${PROVIDER_BIN_DIR}/garm-provider-docker"

# Fixed (but overridable) default so re-running this script points GARM at
# the same provider config path every time - see docs/demo-m0.md.
PROVIDER_CONFIG_DIR="${PROVIDER_CONFIG_DIR:-${TMPDIR:-/tmp}/garm-provider-docker-demo}"
PROVIDER_CONFIG_FILE="${PROVIDER_CONFIG_DIR}/config.toml"

# Deliberately NOT digest-pinned: allow_unpinned_runner_image=true is
# required in the generated provider config for a local dev tag like this
# one (ADR-002/ADR-005 F10; internal/config/config.go's validateRunnerImage).
RUNNER_IMAGE_TAG="${RUNNER_IMAGE_TAG:-garm-runner-noble:demo}"

POOL_TAGS="${POOL_TAGS:-docker,garm-demo}"

# --image/--flavor are REQUIRED by `garm-cli pool add` but are provider-
# specific and, for THIS provider in M0, purely informational:
# CreateInstance always uses the config's runner_image and ignores
# bootstrap.Image entirely (internal/provider/create.go's ensureImage).
POOL_IMAGE="${POOL_IMAGE:-unused-by-this-provider}"
POOL_FLAVOR="${POOL_FLAVOR:-default}"

# Fixed by design, not env-overridable: the M0 go/no-go gate only needs one
# runner to ever exist at a time, and M0 supports linux/amd64 only
# (internal/provider/create.go's validatePlatform).
readonly MIN_IDLE_RUNNERS=0
readonly MAX_RUNNERS=1
readonly OS_ARCH=amd64
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

# --- Step 0: preflight -------------------------------------------------------

build_provider_binary() {
  log "Building provider binary (go build -ldflags version stamp)..."
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

build_runner_image() {
  log "Building runner image ${RUNNER_IMAGE_TAG} from runner-images/noble ..."
  docker build -t "${RUNNER_IMAGE_TAG}" "${REPO_ROOT}/runner-images/noble" \
    || die "docker build of the runner image failed"
}

generate_provider_config() {
  mkdir -p "${PROVIDER_CONFIG_DIR}" || die "could not create ${PROVIDER_CONFIG_DIR}"
  cat > "${PROVIDER_CONFIG_FILE}" <<EOF
# Generated by scripts/demo-m0.sh - M0 demo ONLY, do not use in production.
#
# allow_unpinned_runner_image=true is a dev-only escape hatch
# (internal/config/config.go; ADR-002/ADR-005 F10): it lets runner_image be
# a mutable local tag instead of a sha256-digest-pinned reference. Never set
# this in a real deployment.
docker_host = "unix:///var/run/docker.sock"
runner_image = "${RUNNER_IMAGE_TAG}"
allow_unpinned_runner_image = true
EOF
  log "Wrote provider config: ${PROVIDER_CONFIG_FILE}"
}

step_preflight() {
  log "=== Step 0: preflight ==="

  require_cmd docker
  require_cmd go

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

  build_provider_binary
  build_runner_image
  generate_provider_config

  log "Preflight complete."
  log "  Provider binary : ${PROVIDER_BIN}"
  log "  Provider config : ${PROVIDER_CONFIG_FILE}"
  log "  Runner image    : ${RUNNER_IMAGE_TAG}"
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
  description = "garm-provider-docker M0 demo"
  disable_jit_config = false
  [provider.external]
    provider_executable = "${PROVIDER_BIN}"
    config_file = "${PROVIDER_CONFIG_FILE}"
    interface_version = "v0.1.0"

EOF
  log "interface_version is shown explicitly for clarity; an empty/omitted"
  log "interface_version defaults to v0.1.0 in GARM as well (docs/research.md"
  log "§1.H), which is the only interface surface this M0 provider implements"
  log "(internal/provider/provider.go)."
  log "Verify registration with: garm-cli provider list"

  confirm "Once the [[provider]] section above is added and GARM has been restarted/reloaded, confirm with 'garm-cli provider list' that '${PROVIDER_NAME}' appears."
}

# --- Step 2: create a pool --------------------------------------------------

step_pool() {
  log "=== Step 2: create a pool against your throwaway test repo ==="
  require_repo
  [[ -n "${POOL_TAGS}" ]] || die "POOL_TAGS must not be empty."

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
  --tags ${POOL_TAGS}

EOF
  log "--image/--flavor are required by garm-cli but are informational only"
  log "for this provider in M0 (see the note above POOL_IMAGE/POOL_FLAVOR in"
  log "this script)."
  log "min-idle-runners=0 / max-runners=1 by design: this demo only ever"
  log "needs one runner to exist at a time."
  log "List pools with: garm-cli pool list --repo ${REPO}"

  confirm "Once the pool is created, confirm it with 'garm-cli pool list --repo ${REPO}'."
}

# --- Step 3: watch --------------------------------------------------------

sample_workflow_yaml() {
  local runs_on
  runs_on="$(tags_to_yaml_array "${POOL_TAGS}")"
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
          # Keep the job alive briefly so 'scripts/demo-m0.sh verify' has a
          # window to inspect the live runner container before it exits -
          # this runner is single-job ephemeral by design (ADR-002).
          sleep 30
EOF
}

step_watch() {
  log "=== Step 3: trigger the workflow and watch the runner come up ==="
  require_repo

  log "Copy the following into <your-test-repo>/.github/workflows/garm-m0-demo.yml,"
  log "commit/push it, then trigger it (Actions tab > Run workflow, or"
  log "'gh workflow run garm-m0-demo.yml' if you use the gh CLI):"
  echo
  sample_workflow_yaml
  echo

  log "Watching docker ps (managed containers) + garm-cli runner list, every ${WATCH_INTERVAL_SECONDS}s"
  log "(Ctrl+C to stop early and move on to Step 4; stops on its own after ${WATCH_MAX_ITERATIONS} iterations, 0 = unbounded)."

  trap 'log "stopping watch (Ctrl+C)"; trap - INT; return 0' INT

  local iterations=0
  while (( WATCH_MAX_ITERATIONS == 0 || iterations < WATCH_MAX_ITERATIONS )); do
    echo "--- $(date -u +%FT%TZ) ---"
    docker ps --filter 'label=garm.docker/managed=true' \
      --format 'table {{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Labels}}' || true
    garm-cli runner list --repo "${REPO}" || true
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

  local mounts_json
  mounts_json="$(docker inspect -f '{{json .Mounts}}' "${cid}")"
  if printf '%s' "${mounts_json}" | grep -q '/var/run/docker.sock'; then
    bad "found a /var/run/docker.sock mount on the runner container"
  else
    ok "no host docker.sock mount"
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
    ok "/actions-runner/.runner is a symlink -> /run/garm/runner (ADR-002 M0-demo verification item)"
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
  verify_no_credentials_in_inspect "${cid}"
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

  local leftover_c leftover_v
  leftover_c="$(docker ps -a --filter 'label=garm.docker/managed=true' --format '{{.Names}}')"
  leftover_v="$(docker volume ls --filter 'label=garm.docker/managed=true' --format '{{.Name}}')"
  if [[ -z "${leftover_c}" && -z "${leftover_v}" ]]; then
    ok "no leftover garm.docker/managed=true containers or volumes"
  else
    bad "leftover managed resources found"
    [[ -n "${leftover_c}" ]] && printf '    containers: %s\n' "${leftover_c}"
    [[ -n "${leftover_v}" ]] && printf '    volumes: %s\n' "${leftover_v}"
  fi
  log "Note: M0's per-job workspace volume is anonymous (carries no"
  log "garm.docker/* label - internal/spec/mounts.go's WorkspaceMount); it is"
  log "removed automatically with its container (RemoveVolumes=true), not"
  log "swept independently, so it never shows up in the volume check above."

  return "${VERIFY_FAILED}"
}

# --- Cleanup -----------------------------------------------------------------

step_cleanup() {
  log "=== Cleanup ==="
  cat <<EOF

Manual cleanup once you're done with the demo:

  # 1. Disable and delete the demo pool (garm-cli requires it to be empty first)
  garm-cli pool list --repo ${REPO:-<your-repo>}
  garm-cli pool update <POOL_ID> --enabled=false
  garm-cli runner list <POOL_ID>              # confirm no runners remain
  garm-cli pool delete <POOL_ID>

  # 2. Confirm no leftover managed Docker resources for this controller
  #    (should already be empty if Step 4(e) passed)
  docker ps -a --filter 'label=garm.docker/managed=true'
  docker volume ls --filter 'label=garm.docker/managed=true'

  # 3. Remove the demo-local build artifacts this script created
  docker image rm ${RUNNER_IMAGE_TAG}
  rm -rf ${PROVIDER_CONFIG_DIR}
  rm -rf ${PROVIDER_BIN_DIR}

  # 4. Remove the [[provider]] section for '${PROVIDER_NAME}' from GARM's
  #    own config.toml and restart/reload GARM.

EOF
}

# --- Entry point --------------------------------------------------------------

usage() {
  cat <<'EOF'
Usage: scripts/demo-m0.sh [-y|--yes] [COMMAND]

COMMAND (default: all):
  preflight   Step 0: environment checks, build provider binary + runner image, generate demo config
  config      Step 1: print the config.toml [[provider]] snippet to register with GARM
  pool        Step 2: print the garm-cli pool add command for your test repo
  watch       Step 3: watch docker ps / garm-cli runner list; print the sample workflow yaml
  verify      Step 4(a-d): PASS/FAIL checks against the live runner container
  postjob     Step 4(e): PASS/FAIL checks after the job has finished
  cleanup     Print manual cleanup commands
  all         Run preflight, config, pool, watch, verify, postjob, cleanup in order

Flags:
  -y, --yes   Skip confirmation pauses (auto-confirm)
  -h, --help  Show this help

See docs/demo-m0.md for prerequisites, environment variables, what each
check proves, and a troubleshooting table. No secret is ever read,
generated, or printed by this script.
EOF
}

COMMAND="all"

while [[ $# -gt 0 ]]; do
  case "$1" in
    -y|--yes) ASSUME_YES=1; shift ;;
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
