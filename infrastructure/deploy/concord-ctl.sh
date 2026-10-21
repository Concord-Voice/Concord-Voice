#!/usr/bin/env bash
# Concord service management helper
# Usage: ./concord-ctl.sh <command>
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="${SCRIPT_DIR}/../.."
CONFIG_ROOT="${DEPLOY_DIR}"
CONFIG_DEPLOY_DIR="${SCRIPT_DIR}"
STAGE_RELEASE_MODE=0

# The staged app helper invokes only a verified, immutable source snapshot.
# Recognize that one closed path shape here; never accept a caller-supplied
# source/config root. A malformed path in the reserved release namespace must
# fail closed instead of falling through to the ordinary managed deployment.
if [[ "$SCRIPT_DIR" == /opt/concord-west-stage/releases/*/infrastructure/deploy ]]; then
  if [[ ! "$SCRIPT_DIR" =~ ^/opt/concord-west-stage/releases/[0-9a-f]{40}/infrastructure/deploy$ \
     || -L "${BASH_SOURCE[0]}" || ! -f "${BASH_SOURCE[0]}" ]]; then
    echo 'west-stage: malformed immutable release controller path; refusing deployment' >&2
    exit 1
  fi
  STAGE_RELEASE_MODE=1
  CONFIG_ROOT=/opt/concord
  CONFIG_DEPLOY_DIR=/opt/concord/infrastructure/deploy
elif [[ "$SCRIPT_DIR" == /opt/concord-west-stage/releases/* ]]; then
  echo 'west-stage: controller is outside the admitted immutable release layout; refusing deployment' >&2
  exit 1
fi
if (( STAGE_RELEASE_MODE )) && [[ "${1:-help}" == freshstart ]]; then
  echo 'west-stage: freshstart is not admitted for an immutable stage release' >&2
  exit 1
fi

# The snapshot contains immutable source/build inputs; runtime config and state
# stay in the canonical managed deployment. Preserve ordinary managed path bytes.

# Capture the caller-requested contract before sourcing any host file. A
# managed deploy.env.local must never be able to switch the controller into the
# self-host single-writer mode (or switch an explicit wrapper request out of
# it). The wrapper also attests the only allowed Compose override/profile.
ACTIVITY_HISTORY_REQUESTED_CONTRACT_MODE="${ACTIVITY_HISTORY_CONTRACT_MODE-}"
ACTIVITY_HISTORY_REQUESTED_COMPOSE_OVERRIDE="${COMPOSE_OVERRIDE-}"
ACTIVITY_HISTORY_REQUESTED_COMPOSE_PROFILES="${COMPOSE_PROFILES-}"

if (( STAGE_RELEASE_MODE )); then
  if [[ -n "$ACTIVITY_HISTORY_REQUESTED_CONTRACT_MODE" \
     && "$ACTIVITY_HISTORY_REQUESTED_CONTRACT_MODE" != managed ]]; then
    echo 'west-stage: immutable stage release refuses caller contract-mode overrides' >&2
    exit 1
  fi
  if [[ -n "$ACTIVITY_HISTORY_REQUESTED_COMPOSE_OVERRIDE" \
     && "$ACTIVITY_HISTORY_REQUESTED_COMPOSE_OVERRIDE" != docker-compose.west-staging.yml ]]; then
    echo 'west-stage: immutable stage release refuses caller Compose overrides' >&2
    exit 1
  fi
fi

if [[ "$ACTIVITY_HISTORY_REQUESTED_CONTRACT_MODE" == selfhost \
   && -e "${CONFIG_DEPLOY_DIR}/deploy.env.local" ]]; then
  echo "activity-history: self-host contract refuses deploy.env.local" >&2
  exit 1
fi

# Load runtime config. deploy.env (committed defaults like COMPOSE_OVERRIDE)
# is read first, then deploy.env.local (machine-specific overrides like
# VM_IP, DEPLOY_BRANCH) layers on top.
if [[ -f "${SCRIPT_DIR}/deploy.env" ]]; then
  source "${SCRIPT_DIR}/deploy.env"
fi
if [[ -f "${CONFIG_DEPLOY_DIR}/deploy.env.local" ]]; then
  source "${CONFIG_DEPLOY_DIR}/deploy.env.local"
fi

# Keep the values sourced from the canonical application-env writer separate
# from Compose's root .env. Activity History rollout commands require both
# writers and the effective rendered Compose config to agree.
ACTIVITY_HISTORY_DEPLOY_WRITER_GATE="${ACTIVITY_HISTORY_CLUSTER_ENABLED-}"
ACTIVITY_HISTORY_DEPLOY_WRITER_REPLICAS="${CONTROL_PLANE_REPLICA_COUNT-}"
ACTIVITY_HISTORY_DEPLOY_WRITER_OPERATOR_NAME="${ACTIVITY_HISTORY_OPERATOR_NAME-}"
ACTIVITY_HISTORY_DEPLOY_WRITER_PRIVACY_POLICY_URL="${ACTIVITY_HISTORY_PRIVACY_POLICY_URL-}"

# Read one simple inventory identity from either canonical writer file without
# sourcing or decoding shell/Compose expressions. Writers may wrap these
# restricted tokens in @Q single quotes (deploy.env.local) or Compose double
# quotes (.env); no escapes or interpolation are admitted for identity fields.
canonical_identity_value() {
  local file="$1" key="$2" line count=0 value=""
  [[ -f "$file" && ! -L "$file" ]] || return 1
  while IFS= read -r line || [[ -n "$line" ]]; do
    case "$line" in
      "${key}"=*) count=$((count + 1)); value="${line#*=}" ;;
    esac
  done < "$file"
  [[ "$count" == 1 && -n "$value" ]] || return 1
  if [[ "$value" =~ ^\'([A-Za-z0-9._-]+)\'$ ]]; then
    value="${BASH_REMATCH[1]}"
  elif [[ "$value" =~ ^\"([A-Za-z0-9._-]+)\"$ ]]; then
    value="${BASH_REMATCH[1]}"
  elif [[ "$value" =~ ^[A-Za-z0-9._-]+$ ]]; then
    :
  else
    return 1
  fi
  printf '%s' "$value"
}

ACTIVITY_HISTORY_CONTRACT_MODE="${ACTIVITY_HISTORY_REQUESTED_CONTRACT_MODE:-managed}"
if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]]; then
  # Caller environment wins over Compose .env interpolation. Remove every
  # self-host-owned value (and any ad-hoc image override) before rendering or
  # running containers; Compose then loads the installer-owned root .env.
  unset INSTANCE_TYPE ACTIVITY_HISTORY_CLUSTER_ENABLED \
    CONTROL_PLANE_REPLICA_COUNT ACTIVITY_HISTORY_OPERATOR_NAME \
    ACTIVITY_HISTORY_PRIVACY_POLICY_URL ATTACHMENT_WRITE_BACKEND CONTROL_PLANE_IMAGE \
    COMPOSE_ENV_FILES COMPOSE_DISABLE_ENV_FILE
  COMPOSE_OVERRIDE=docker-compose.production.yml
  unset COMPOSE_PROFILES
fi

DEPLOY_REPO="${DEPLOY_REPO:-https://github.com/Concord-Voice/Concord-Voice.git}"
COMPOSE_OVERRIDE="${COMPOSE_OVERRIDE:-docker-compose.staging.yml}"
COMPOSE_ENV_FILE_ARG=""
COMPOSE_PRODUCTION_ARG=""
[[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]] \
  && COMPOSE_ENV_FILE_ARG="--env-file ${CONFIG_ROOT}/.env"
if (( STAGE_RELEASE_MODE )); then
  COMPOSE_OVERRIDE=docker-compose.west-staging.yml
  COMPOSE_ENV_FILE_ARG="--env-file /opt/concord/.env"
fi
# Self-host published-port posture (#3538). Reset AFTER deploy.env/deploy.env.local
# are sourced, so neither can set or suppress it.
COMPOSE_SELFHOST_ARG=""
[[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]] \
  && COMPOSE_SELFHOST_ARG=" -f ${DEPLOY_DIR}/docker-compose.selfhost.yml"
if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]]; then
  MEDIA_DEPLOY_PROFILE=media-single
  COMPOSE_PROJECT_ARG=""
elif [[ "$COMPOSE_OVERRIDE" == docker-compose.west-staging.yml ]]; then
  # A third Compose layer is privileged West-only state. Bind it to both
  # canonical writer files before any controller command can use it.
  stage_identity_value() {
    canonical_identity_value "$1" "$2"
  }
  stage_env_file="${CONFIG_ROOT}/.env"
  stage_deploy_file="${CONFIG_DEPLOY_DIR}/deploy.env.local"
  stage_identity_keys=(DEPLOY_TARGET_ID DEPLOY_ROLE SITE_ID DEPLOY_CANONICAL_HOST PUBLIC_IP ENVIRONMENT DOMAIN STAGE_PRIVATE_ADDRESS)
  stage_values=()
  for stage_key in "${stage_identity_keys[@]}"; do
    if ! stage_value="$(stage_identity_value "$stage_env_file" "$stage_key")" \
       || ! deploy_value="$(stage_identity_value "$stage_deploy_file" "$stage_key")"; then
      echo 'west-stage: canonical deployment identity is missing or ambiguous; refusing Compose selection' >&2
      exit 1
    fi
    [[ "$stage_value" == "$deploy_value" ]] \
      || { echo "west-stage: $stage_key differs between canonical writer files; refusing Compose selection" >&2; exit 1; }
    stage_values+=("$stage_value")
  done
  stage_target=${stage_values[0]}
  stage_role=${stage_values[1]}
  stage_site=${stage_values[2]}
  stage_host=${stage_values[3]}
  stage_public_ip=${stage_values[4]}
  stage_environment=${stage_values[5]}
  stage_domain=${stage_values[6]}
  stage_private_address=${stage_values[7]}
  [[ "$(stage_identity_value "$stage_deploy_file" COMPOSE_OVERRIDE)" == docker-compose.west-staging.yml ]] \
    || { echo 'west-stage: deploy.env.local does not select the West staging overlay; refusing Compose selection' >&2; exit 1; }
  if [[ "$stage_target" != west-stage-1 || "$stage_role" != stage \
     || "$stage_site" != us-west || "$stage_host" != 15.204.77.113 \
     || "$stage_public_ip" != 15.204.77.113 || "$stage_environment" != production \
     || "$stage_domain" != staging.concordvoice.chat || "$stage_private_address" != 10.246.6.1 ]]; then
    echo 'west-stage: canonical target, role, site, host, domain, alias or runtime mode does not match; refusing Compose selection' >&2
    exit 1
  fi
  MEDIA_DEPLOY_PROFILE=media-single
  COMPOSE_PROJECT_ARG=" -p concord-west-stage"
  COMPOSE_PRODUCTION_ARG=" -f ${DEPLOY_DIR}/docker-compose.production.yml"
elif [[ "$COMPOSE_OVERRIDE" == docker-compose.production.yml \
     || "$COMPOSE_OVERRIDE" == docker-compose.staging.yml ]]; then
  MEDIA_DEPLOY_PROFILE="${MEDIA_DEPLOY_PROFILE:-media-single}"
  COMPOSE_PROJECT_ARG=""
  case "$MEDIA_DEPLOY_PROFILE" in
    media-single|media-bluegreen) ;;
    *) echo 'media-bluegreen: invalid media deployment profile' >&2; exit 2 ;;
  esac
else
  MEDIA_DEPLOY_PROFILE=media-single
  COMPOSE_PROJECT_ARG=""
fi
if [[ "$MEDIA_DEPLOY_PROFILE" == media-bluegreen ]]; then
  COMPOSE_PROFILE_ARGS="--profile media-bluegreen"
  COMPOSE_PROFILES=media-bluegreen
else
  COMPOSE_PROFILE_ARGS="--profile services"
  if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" != selfhost ]]; then
    COMPOSE_PROFILES=services
  fi
fi
[[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]] || export COMPOSE_PROFILES
if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" != selfhost ]]; then
  # Managed filesystem and daemon operations must address the same host.
  unset DOCKER_CONTEXT DOCKER_TLS DOCKER_TLS_VERIFY DOCKER_CERT_PATH
  export DOCKER_HOST=unix:///var/run/docker.sock
fi
COMPOSE_CMD="docker compose ${COMPOSE_ENV_FILE_ARG}${COMPOSE_PROJECT_ARG} -f ${DEPLOY_DIR}/docker-compose.yml${COMPOSE_PRODUCTION_ARG} -f ${DEPLOY_DIR}/${COMPOSE_OVERRIDE}${COMPOSE_SELFHOST_ARG} ${COMPOSE_PROFILE_ARGS}"

# Read managed fleet identity as inert data. These values are intentionally
# simple inventory tokens; do not source either canonical writer file here.
managed_identity_value() {
  canonical_identity_value "$1" "$2"
}

managed_core_identity_admit() {
  local key root_value deploy_value expected
  [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == managed \
     && "$COMPOSE_OVERRIDE" == docker-compose.production.yml ]] || {
    echo 'managed-core: production nginx requires the managed production Compose selection' >&2
    return 1
  }
  for key in DEPLOY_TARGET_ID DEPLOY_ROLE SITE_ID DEPLOY_CANONICAL_HOST PUBLIC_IP ENVIRONMENT DOMAIN; do
    case "$key" in
      DEPLOY_TARGET_ID) expected=east-core-1 ;;
      DEPLOY_ROLE) expected=core ;;
      SITE_ID) expected=us-east ;;
      DEPLOY_CANONICAL_HOST|PUBLIC_IP) expected=15.204.207.82 ;;
      ENVIRONMENT) expected=production ;;
      DOMAIN) expected=concordvoice.chat ;;
    esac
    if ! root_value="$(managed_identity_value "${CONFIG_ROOT}/.env" "$key")" \
       || ! deploy_value="$(managed_identity_value "${CONFIG_DEPLOY_DIR}/deploy.env.local" "$key")"; then
      echo 'managed-core: canonical deployment identity is missing or ambiguous; refusing nginx activation' >&2
      return 1
    fi
    [[ "$root_value" == "$expected" && "$deploy_value" == "$expected" ]] || {
      echo "managed-core: $key does not match the admitted East core identity in both canonical writer files; refusing nginx activation" >&2
      return 1
    }
  done
}
# Self-host commands, including internal renders and probes, dispatch through
# their existing argv-based runner.
if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]]; then
  if [[ "$ACTIVITY_HISTORY_REQUESTED_COMPOSE_OVERRIDE" != docker-compose.production.yml \
     || "$ACTIVITY_HISTORY_REQUESTED_COMPOSE_PROFILES" != services ]]; then
    echo "activity-history: self-host contract requires the production Compose override and services profile" >&2
    exit 1
  fi
  command -v python3 >/dev/null 2>&1 \
    || { echo 'selfhost-posture: python3 is required' >&2; exit 1; }
  # shellcheck source=selfhost-storage.sh
  source "${SCRIPT_DIR}/selfhost-storage.sh"
  selfhost_storage_resolve "$DEPLOY_DIR" || exit 1
  # Direct Docker discovery/inspection must inherit no caller storage controls
  # either; the captured attestation above remains the contract authority.
  selfhost_storage_scrub_environment || exit 1
  COMPOSE_CMD=selfhost_storage_compose
fi

selfhost_storage_start_guard() {
  [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]] || return 0
  selfhost_storage_assert_no_running_minio
}

# Load the TLS caller API only on verbs that need it. Help, stop/down, logs,
# exposure checks, and recovery remain usable if the observer is unavailable.
selfhost_tls_load() {
  [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]] || return 0
  declare -F selfhost_tls_admit >/dev/null 2>&1 && return 0
  local helper="${SCRIPT_DIR}/selfhost-tls.sh"
  [[ -f "$helper" ]] || { echo 'selfhost-tls: observer prerequisite unavailable' >&2; return 1; }
  # shellcheck source=selfhost-tls.sh
  source "$helper"
}

selfhost_tls_admit_gate() {
  [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]] || return 0
  selfhost_tls_load || return 1
  selfhost_tls_admit
}

selfhost_tls_ready_gate() {
  [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]] || return 0
  selfhost_tls_load || return 1
  selfhost_tls_ready "${1:-15}"
}

selfhost_storage_target_guard() {
  [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]] || return 0
  [[ "$SELFHOST_STORAGE_MODE_RESOLVED" == byo-s3 ]] || return 0
  local target
  for target in "$@"; do
    if [[ "$target" == minio ]]; then
      echo 'selfhost-storage: explicit MinIO target refused' >&2
      return 1
    fi
  done
}

# ── Self-host published-port posture (#3538) ─────────────────────────────────
# Engine floor: before 29.8.2, malicious registry DNS responses can disclose
# registry credentials (CVE-2026-92543). Before 28.0.0, hosts on the same L2 segment can reach ports
# published on 127.0.0.1; from 28.2.0 to 28.3.2 a firewalld reload drops the rule
# that blocks them (CVE-2025-54388). Must equal MIN_ENGINE in install-selfhost.sh
# (pinned by [internal]tests/test-selfhost-port-posture.sh).
SELFHOST_MIN_ENGINE=29.8.2
SELFHOST_DOCKER_DAEMON_JSON="${SELFHOST_DOCKER_DAEMON_JSON:-/etc/docker/daemon.json}"

# selfhost_posture_verdict MODE JSON -> 0 ok | 1 refused (prints violations).
# Never echoes JSON: it carries .env-derived secrets.
selfhost_posture_verdict() {
  local mode="$1" json="$2" violations rc=0
  violations="$(printf '%s' "$json" | python3 "${SCRIPT_DIR}/selfhost-port-posture.py" "$mode" 2>/dev/null)" || rc=$?
  case "$rc" in
    0) return 0 ;;
    1)
      echo "selfhost-posture: refusing — outside the self-host allowlist:" >&2
      printf '%s\n' "$violations" | sed 's/^/  /' >&2
      # A publication is fixed by the compose model or by up; a network attachment is not.
      if grep -qvE ' network(_mode)?=' <<<"$violations"; then
        if [[ "$mode" == --rendered ]]; then
          echo "  Either Compose is too old to honour !override in docker-compose.selfhost.yml (upgrade to >= 2.24.5)," >&2
          echo "  or a compose file was changed to publish this. Revert that change." >&2
        else
          echo "  Run ./infrastructure/deploy/concord-selfhost up to recreate the listed services (never a two-file docker compose up)." >&2
          echo "  If up itself reported this, the container is one up does not manage (an orphan): remove it with docker rm -f." >&2
        fi
      fi
      if grep -qE ' network(_mode)?=' <<<"$violations"; then
        echo "  A listed network makes the container reachable with nothing published. Attach services only to a bridge network" >&2
        echo "  on the default nat gateway mode, and detach any network added by hand: docker network disconnect <network> <container>." >&2
      fi
      return 1
      ;;
    *)
      echo "selfhost-posture: refusing — the ${mode#--} port model could not be judged" >&2
      return 1
      ;;
  esac
}

# Refuses, before any verb runs, an Engine older than SELFHOST_MIN_ENGINE and a
# rendered model that publishes anything outside the self-host allowlist.
selfhost_port_posture_gate() {
  local engine rendered direct rc engine_part
  local -a engine_response=()
  command -v python3 >/dev/null 2>&1 \
    || { echo "selfhost-posture: python3 is required" >&2; return 1; }
  # Retain every LF and the query status without command substitution. Extra
  # NUL-delimited fields also refuse NUL bytes inside the server response.
  while IFS= read -r -d '' engine_part; do
    engine_response+=("$engine_part")
  done < <(
    query_status=0
    docker version --format '{{.Server.Version}}' 2>/dev/null || query_status=$?
    printf '\0%d\0' "$query_status"
  )
  if (( ${#engine_response[@]} != 2 )) || [[ ${engine_response[1]} != 0 ]]; then
    echo "selfhost-posture: Docker Engine server version unavailable or invalid — refusing" >&2
    return 1
  fi
  engine="${engine_response[0]%$'\n'}"
  if [[ ! "$engine" =~ ^((0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*))(\+[A-Za-z0-9.+-]+)?$ ]]; then
    echo "selfhost-posture: Docker Engine server version unavailable or invalid — refusing" >&2
    return 1
  fi
  engine="${BASH_REMATCH[1]}"
  if [[ "$(printf '%s\n%s\n' "$SELFHOST_MIN_ENGINE" "$engine" | sort -V | head -1)" != "$SELFHOST_MIN_ENGINE" ]]; then
    echo "selfhost-posture: Docker Engine $engine is older than $SELFHOST_MIN_ENGINE; require registry credential protection (CVE-2026-92543) and loopback-published port protection (CVE-2025-54388) — refusing" >&2
    return 1
  fi
  # Compose's error is NOT echoed: compose-go's dotenv errors can quote a whole .env
  # line, value included. validate-config cannot show it either, because this gate runs
  # before any verb, so name the command that does.
  if ! rendered="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD config --format json 2>/dev/null)"; then
    echo "selfhost-posture: could not render the compose model — refusing; check the installed environment and Compose files" >&2
    return 1
  fi
  # allow-direct-routing opens every published port to direct routing, so a port published
  # on 127.0.0.1 is reachable through the container's own address.
  direct=0
  if [[ -e "$SELFHOST_DOCKER_DAEMON_JSON" ]]; then
    if [[ ! -r "$SELFHOST_DOCKER_DAEMON_JSON" ]]; then
      echo "selfhost-posture: cannot read $SELFHOST_DOCKER_DAEMON_JSON to rule out allow-direct-routing — refusing" >&2
      return 1
    fi
    # 0 set, 1 unset or false, 2 unparseable. dockerd reads the file only at start, so a
    # broken file can sit beside a daemon still running with the setting: refuse.
    rc=0
    python3 -c 'import json,sys
try: v = json.load(open(sys.argv[1])).get("allow-direct-routing")
except Exception: sys.exit(2)
sys.exit(1 if v is None or v is False else 0)' "$SELFHOST_DOCKER_DAEMON_JSON" 2>/dev/null || rc=$?
    case $rc in
      0) direct=1 ;;
      1) ;;
      *) echo "selfhost-posture: cannot parse $SELFHOST_DOCKER_DAEMON_JSON to rule out allow-direct-routing — refusing" >&2
         return 1 ;;
    esac
  fi
  pgrep -f 'dockerd.*--allow-direct-routing' >/dev/null 2>&1 && direct=1
  if [[ $direct -eq 1 ]]; then
    echo "selfhost-posture: Docker is configured with allow-direct-routing, which exposes ports published on 127.0.0.1 — refusing" >&2
    return 1
  fi
  selfhost_posture_verdict --rendered "$rendered"
}

# Live bindings of every container in this compose project, stopped ones and
# orphans included, judged against the same allowlist.
selfhost_live_port_check() {
  local rendered project ids inspected net_ids networks
  rendered="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD config --format json 2>/dev/null)" || rendered=""
  project="$(printf '%s' "$rendered" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("name",""))' 2>/dev/null || true)"
  if [[ ! "$project" =~ ^[a-z0-9][a-z0-9_-]*$ ]]; then
    echo "selfhost-posture: cannot resolve the compose project name — refusing" >&2
    return 1
  fi
  # Label filter, not `compose ps`: it includes STOPPED containers (they keep their
  # bindings) and orphans, whose `compose ps` handling varies by Compose version.
  if ! ids="$(docker ps -aq --filter "label=com.docker.compose.project=${project}")"; then
    echo "selfhost-posture: could not list containers for project ${project} — refusing" >&2
    return 1
  fi
  if [[ -z "$ids" ]]; then
    echo "selfhost-posture: no containers for project ${project} — is the stack up?" >&2
    return 1
  fi
  # shellcheck disable=SC2086  # one argument per container id
  if ! inspected="$(docker inspect $ids 2>/dev/null)"; then
    echo "selfhost-posture: docker inspect failed — refusing" >&2
    return 1
  fi
  # The networks those containers are attached to: a macvlan or unprotected bridge makes a
  # container reachable with nothing published, which PortBindings cannot show.
  # shellcheck disable=SC2086  # one argument per container id
  if ! net_ids="$(docker inspect --format '{{range .NetworkSettings.Networks}}{{.NetworkID}} {{end}}' $ids 2>/dev/null)"; then
    echo "selfhost-posture: docker inspect failed — refusing" >&2
    return 1
  fi
  networks="[]"
  # shellcheck disable=SC2046,SC2086  # one argument per network id
  if [[ -n "${net_ids// /}" ]] && ! networks="$(docker network inspect $(printf '%s\n' $net_ids | sort -u) 2>/dev/null)"; then
    echo "selfhost-posture: docker network inspect failed — refusing" >&2
    return 1
  fi
  # printf is a builtin: the inspect JSON (which carries Config.Env) never reaches argv.
  selfhost_posture_verdict --live "$(printf '{"containers":%s,"networks":%s}' "$inspected" "$networks")"
}

# Run before every rebuild's "Done.": a container the rebuild did not recreate keeps
# whatever it was published with. A no-op outside self-host mode.
selfhost_post_up_check() {
  [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]] || return 0
  selfhost_storage_assert_no_running_minio || return 1
  selfhost_live_port_check
}

if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]]; then
  selfhost_port_posture_gate || exit 1
fi
ACTIVITY_HISTORY_IMAGE_SELECTOR="${CONFIG_DEPLOY_DIR}/.env.activity-history-image.local"
ACTIVITY_HISTORY_ACTIVATION_RECEIPT="${CONFIG_DEPLOY_DIR}/.env.activity-history-receipt.local"
ACTIVITY_HISTORY_IMAGE_SELECTOR_LOADED=0
ACTIVITY_HISTORY_IMAGE_SELECTOR_SELECTED=0
ACTIVITY_HISTORY_ROLLBACK_CHECKPOINT_IMAGE=""
ACTIVITY_HISTORY_ROLLBACK_ADMIN_COMPLETE=""
ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_REQUESTED=""
ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE=""

# Rollback pins an immutable old control-plane image and the completed admin
# phase in a dedicated local checkpoint. Parse it as data rather than sourcing
# it. restart/status keep consuming the pin; an explicit ordinary rebuild or
# freshstart removes it before building the current source again.
load_activity_history_image_selector() {
  local line value
  local image_count=0 admin_count=0 requested_count=0 state_count=0
  [[ -f "$ACTIVITY_HISTORY_IMAGE_SELECTOR" ]] || return 0
  ACTIVITY_HISTORY_ROLLBACK_CHECKPOINT_IMAGE=""
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -z "$line" ]] && continue
    value="${line#*=}"
    case "$line" in
      CONTROL_PLANE_IMAGE=*)
        image_count=$((image_count + 1))
        ACTIVITY_HISTORY_ROLLBACK_CHECKPOINT_IMAGE="$value"
        ;;
      ACTIVITY_HISTORY_ROLLBACK_ADMIN_COMPLETE=*)
        admin_count=$((admin_count + 1))
        ACTIVITY_HISTORY_ROLLBACK_ADMIN_COMPLETE="$value"
        ;;
      ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_REQUESTED=*)
        requested_count=$((requested_count + 1))
        ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_REQUESTED="$value"
        ;;
      ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE=*)
        state_count=$((state_count + 1))
        ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE="$value"
        ;;
      *)
        echo "activity-history: rollback checkpoint is malformed; refusing to continue" >&2
        return 1
        ;;
    esac
  done <"$ACTIVITY_HISTORY_IMAGE_SELECTOR"
  if [[ $image_count -ne 1 || $admin_count -ne 1 \
     || $requested_count -ne 1 || $state_count -ne 1 ]]; then
    echo "activity-history: rollback checkpoint is malformed; refusing to continue" >&2
    return 1
  fi
  if [[ "$ACTIVITY_HISTORY_ROLLBACK_CHECKPOINT_IMAGE" == *[[:space:]]* \
     || ! "$ACTIVITY_HISTORY_ROLLBACK_CHECKPOINT_IMAGE" =~ ^(sha256:[[:xdigit:]]{64}|.+@sha256:[[:xdigit:]]{64})$ ]]; then
    echo "activity-history: rollback checkpoint image is not immutable; refusing to continue" >&2
    return 1
  fi
  if [[ "$ACTIVITY_HISTORY_ROLLBACK_ADMIN_COMPLETE" != true \
     || ! "$ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_REQUESTED" =~ ^(true|false)$ ]]; then
    echo "activity-history: rollback checkpoint completion state is invalid; refusing to continue" >&2
    return 1
  fi
  if [[ "$ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_REQUESTED" == true ]]; then
    [[ "$ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE" =~ ^(attempting|complete|failed)$ ]] || {
      echo "activity-history: rollback checkpoint completion state is invalid; refusing to continue" >&2
      return 1
    }
  elif [[ "$ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE" != not_requested ]]; then
    echo "activity-history: rollback checkpoint completion state is invalid; refusing to continue" >&2
    return 1
  fi
  ACTIVITY_HISTORY_IMAGE_SELECTOR_LOADED=1
  if [[ "$ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE" =~ ^(complete|not_requested)$ ]]; then
    CONTROL_PLANE_IMAGE="$ACTIVITY_HISTORY_ROLLBACK_CHECKPOINT_IMAGE"
    ACTIVITY_HISTORY_IMAGE_SELECTOR_SELECTED=1
    export CONTROL_PLANE_IMAGE
    if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]]; then
      selfhost_storage_select_image "$CONTROL_PLANE_IMAGE" || return 1
    fi
  fi
}

clear_activity_history_image_selector() {
  if [[ -f "$ACTIVITY_HISTORY_IMAGE_SELECTOR" ]]; then
    rm -f "$ACTIVITY_HISTORY_IMAGE_SELECTOR"
    echo "Activity History rollback image pin cleared; rebuilding the current source."
  fi
  if [[ $ACTIVITY_HISTORY_IMAGE_SELECTOR_SELECTED -eq 1 ]]; then
    unset CONTROL_PLANE_IMAGE
  fi
  if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]]; then
    selfhost_storage_select_image '' || return 1
  fi
  ACTIVITY_HISTORY_IMAGE_SELECTOR_LOADED=0
  ACTIVITY_HISTORY_IMAGE_SELECTOR_SELECTED=0
  ACTIVITY_HISTORY_ROLLBACK_CHECKPOINT_IMAGE=""
  ACTIVITY_HISTORY_ROLLBACK_ADMIN_COMPLETE=""
  ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_REQUESTED=""
  ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE=""
}

load_activity_history_image_selector

# Load the managed recovery writer only for its explicit managed dispatch.
if [[ "${1:-}" == sync-releases ]]; then
  [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == managed ]] || {
    echo 'sync-releases is managed-host recovery only' >&2
    exit 1
  }
  # shellcheck source=sync-releases.sh
  source "${SCRIPT_DIR}/sync-releases.sh"
fi

dump_health_diagnostics() {
  echo "" >&2
  echo "=== docker compose ps (safe fields) ===" >&2
  if ! (cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --all --format 'table {{.Service}}\t{{.State}}\t{{.Health}}\t{{.ExitCode}}') >&2; then
    echo "  ⚠ failed to collect docker compose ps" >&2
  fi

  echo "" >&2
  echo "=== docker compose logs ===" >&2
  echo "  omitted from CI diagnostics; runtime logs may contain host-side secrets" >&2
  echo "  use 'concord-ctl.sh logs <service>' on the host for manual incident diagnostics" >&2
}

activity_history_read_root_value() {
  local key="$1" file="${CONFIG_ROOT}/.env" count value
  [[ -f "$file" ]] || return 1
  count="$(grep -cE "^${key}=" "$file" 2>/dev/null || true)"
  [[ "$count" == 1 ]] || return 1
  value="$(grep -E "^${key}=" "$file" | cut -d= -f2-)"
  if [[ ${#value} -ge 2 && ( "$value" == \'*\' || "$value" == \"*\" ) ]]; then
    value="${value:1:${#value}-2}"
  fi
  printf '%s' "$value"
}

install_attachment_probe_cron() {
  local marker='concord-ctl.sh attachment-probe'
  local current
  local line="30 2 * * * ${DEPLOY_DIR}/[internal]concord-ctl.sh attachment-probe --backend r2-useast >> ${CONFIG_ROOT}/logs/attachment-probe.log 2>&1 && rm -f ${CONFIG_ROOT}/logs/attachment-probe-alert.flag || echo \"attachment-probe failed at \$(date -Is)\" > ${CONFIG_ROOT}/logs/attachment-probe-alert.flag"
  if ! current="$(LC_ALL=C crontab -l 2>&1)"; then
    if [[ "$current" != *"no crontab for"* ]]; then
      echo "attachment-probe-cron-install: could not read the existing crontab: $current" >&2
      return 1
    fi
    current=""
  fi
  {
    if [[ -n "$current" ]]; then
      grep -Fv "$marker" <<<"$current" || true
    fi
    printf '%s\n' "$line"
  } | crontab -
  echo "  ✓ nightly attachment probe cron installed"
}

activity_history_render_contract() {
  local rendered parsed
  # Resolve the root .env contract independently of deploy.env.local. The
  # latter is sourced for concord-ctl settings, and inherited shell values
  # otherwise override Compose's root dotenv values during rendering.
  if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]]; then
    if ! rendered="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD config --format json 2>/dev/null)"; then
      echo "activity-history: failed to render Compose configuration" >&2
      return 1
    fi
  elif ! rendered="$(cd "$DEPLOY_DIR" && env \
      -u ACTIVITY_HISTORY_CLUSTER_ENABLED \
      -u CONTROL_PLANE_REPLICA_COUNT \
      -u ACTIVITY_HISTORY_OPERATOR_NAME \
      -u ACTIVITY_HISTORY_PRIVACY_POLICY_URL \
      $COMPOSE_CMD config --format json 2>/dev/null)"; then
    echo "activity-history: failed to render Compose configuration" >&2
    return 1
  fi
  if ! parsed="$(python3 -c '
import json, sys
try:
    service = json.load(sys.stdin)["services"]["control-plane"]
    environment = service.get("environment", {})
    if isinstance(environment, list):
        environment = dict(item.split("=", 1) for item in environment)
    gate = str(environment["ACTIVITY_HISTORY_CLUSTER_ENABLED"])
    replicas = str(environment["CONTROL_PLANE_REPLICA_COUNT"])
    operator_name = str(environment["ACTIVITY_HISTORY_OPERATOR_NAME"])
    privacy_policy_url = str(environment["ACTIVITY_HISTORY_PRIVACY_POLICY_URL"])
    instance_type = str(environment.get("INSTANCE_TYPE", ""))
    image = str(service["image"])
    values = (gate, replicas, image, instance_type, operator_name, privacy_policy_url)
    if any(any(ord(char) < 32 or ord(char) == 127 for char in value) for value in values):
        raise ValueError("invalid control character")
    print("\x1f".join(values))
except (KeyError, TypeError, ValueError, json.JSONDecodeError):
    sys.exit(1)
' <<<"$rendered" 2>/dev/null)"; then
    echo "activity-history: rendered Compose contract is missing or invalid" >&2
    return 1
  fi
  IFS=$'\x1f' read -r ACTIVITY_HISTORY_RENDERED_GATE \
    ACTIVITY_HISTORY_RENDERED_REPLICAS ACTIVITY_HISTORY_RENDERED_IMAGE \
    ACTIVITY_HISTORY_RENDERED_INSTANCE_TYPE \
    ACTIVITY_HISTORY_RENDERED_OPERATOR_NAME \
    ACTIVITY_HISTORY_RENDERED_PRIVACY_POLICY_URL <<<"$parsed"
  if [[ ! "$ACTIVITY_HISTORY_RENDERED_GATE" =~ ^(true|false)$ \
     || ! "$ACTIVITY_HISTORY_RENDERED_REPLICAS" =~ ^[1-9][0-9]*$ \
     || -z "$ACTIVITY_HISTORY_RENDERED_IMAGE" \
     || "$ACTIVITY_HISTORY_RENDERED_IMAGE" == *[[:space:]]* ]]; then
    echo "activity-history: rendered Compose contract has invalid values" >&2
    return 1
  fi
}

activity_history_load_contract() {
  local root_gate root_replicas instance_type
  local mode="${ACTIVITY_HISTORY_CONTRACT_MODE:-managed}"
  local deploy_file="${CONFIG_DEPLOY_DIR}/deploy.env.local"
  if ! root_gate="$(activity_history_read_root_value ACTIVITY_HISTORY_CLUSTER_ENABLED)" \
     || ! root_replicas="$(activity_history_read_root_value CONTROL_PLANE_REPLICA_COUNT)" \
     || [[ "$(grep -cE '^ACTIVITY_HISTORY_OPERATOR_NAME=' "${CONFIG_ROOT}/.env" 2>/dev/null || true)" != 1 ]] \
     || [[ "$(grep -cE '^ACTIVITY_HISTORY_PRIVACY_POLICY_URL=' "${CONFIG_ROOT}/.env" 2>/dev/null || true)" != 1 ]]; then
    echo "activity-history: canonical root .env contract is missing or duplicated" >&2
    return 1
  fi
  if [[ ! "$root_gate" =~ ^(true|false)$ || ! "$root_replicas" =~ ^[1-9][0-9]*$ ]]; then
    echo "activity-history: canonical rollout values are invalid" >&2
    return 1
  fi

  case "$mode" in
    managed)
      if [[ ! -f "$deploy_file" \
         || "$(grep -cE '^ACTIVITY_HISTORY_CLUSTER_ENABLED=' "$deploy_file" 2>/dev/null || true)" != 1 \
         || "$(grep -cE '^CONTROL_PLANE_REPLICA_COUNT=' "$deploy_file" 2>/dev/null || true)" != 1 \
         || "$(grep -cE '^ACTIVITY_HISTORY_OPERATOR_NAME=' "$deploy_file" 2>/dev/null || true)" != 1 \
         || "$(grep -cE '^ACTIVITY_HISTORY_PRIVACY_POLICY_URL=' "$deploy_file" 2>/dev/null || true)" != 1 ]]; then
        echo "activity-history: canonical deploy.env.local contract is missing or duplicated" >&2
        return 1
      fi
      if [[ ! "$ACTIVITY_HISTORY_DEPLOY_WRITER_GATE" =~ ^(true|false)$ \
         || ! "$ACTIVITY_HISTORY_DEPLOY_WRITER_REPLICAS" =~ ^[1-9][0-9]*$ ]]; then
        echo "activity-history: canonical rollout values are invalid" >&2
        return 1
      fi
      if [[ "$root_gate" != "$ACTIVITY_HISTORY_DEPLOY_WRITER_GATE" \
         || "$root_replicas" != "$ACTIVITY_HISTORY_DEPLOY_WRITER_REPLICAS" ]]; then
        echo "activity-history: canonical rollout writers disagree; rerun provision-secrets" >&2
        return 1
      fi
      ;;
    selfhost)
      if [[ "$ACTIVITY_HISTORY_REQUESTED_COMPOSE_OVERRIDE" != docker-compose.production.yml \
         || "$ACTIVITY_HISTORY_REQUESTED_COMPOSE_PROFILES" != services ]]; then
        echo "activity-history: self-host contract requires the production Compose override and services profile" >&2
        return 1
      fi
      if ! instance_type="$(activity_history_read_root_value INSTANCE_TYPE)" \
         || [[ "$instance_type" != self-hosted ]]; then
        echo "activity-history: self-host contract requires INSTANCE_TYPE=self-hosted in root .env" >&2
        return 1
      fi
      if [[ -e "$deploy_file" ]]; then
        echo "activity-history: self-host contract refuses deploy.env.local" >&2
        return 1
      fi
      ;;
    *)
      echo "activity-history: unknown rollout contract mode; refusing to continue" >&2
      return 1
      ;;
  esac

  activity_history_render_contract || return 1
  if [[ "$mode" == selfhost \
     && "$ACTIVITY_HISTORY_RENDERED_INSTANCE_TYPE" != self-hosted ]]; then
    echo "activity-history: rendered self-host instance type is invalid" >&2
    return 1
  fi
  if [[ "$root_gate" != "$ACTIVITY_HISTORY_RENDERED_GATE" \
     || "$root_replicas" != "$ACTIVITY_HISTORY_RENDERED_REPLICAS" ]]; then
    echo "activity-history: rendered Compose rollout contract disagrees with canonical writers" >&2
    return 1
  fi
  if [[ "$mode" == managed \
     && ( "$ACTIVITY_HISTORY_DEPLOY_WRITER_OPERATOR_NAME" != "$ACTIVITY_HISTORY_RENDERED_OPERATOR_NAME" \
       || "$ACTIVITY_HISTORY_DEPLOY_WRITER_PRIVACY_POLICY_URL" != "$ACTIVITY_HISTORY_RENDERED_PRIVACY_POLICY_URL" ) ]]; then
    echo "activity-history: canonical disclosure writers disagree; rerun provision-secrets" >&2
    return 1
  fi
}

ACTIVITY_HISTORY_DAEMON_IDS=()
activity_history_refresh_daemon_ids() {
  local output id
  ACTIVITY_HISTORY_DAEMON_IDS=()
  if ! output="$(docker ps \
      --filter label=com.docker.compose.service=control-plane \
      --format '{{.ID}}' 2>/dev/null)"; then
    echo "activity-history: failed to inspect daemon-wide control-plane state" >&2
    return 1
  fi
  while IFS= read -r id; do
    [[ -n "$id" ]] && ACTIVITY_HISTORY_DAEMON_IDS+=("$id")
  done <<<"$output"
  return 0
}

activity_history_container_gate_matches() {
  local container_id="$1" expected="$2" container_env gate_count
  [[ "$expected" =~ ^(true|false)$ ]] || return 1
  if ! container_env="$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' \
      "$container_id" 2>/dev/null)"; then
    echo "activity-history: failed to inspect the running control-plane contract" >&2
    return 1
  fi
  gate_count="$(grep -cE '^ACTIVITY_HISTORY_CLUSTER_ENABLED=' <<<"$container_env" || true)"
  [[ "$gate_count" == 1 ]] \
    && grep -qxF "ACTIVITY_HISTORY_CLUSTER_ENABLED=$expected" <<<"$container_env"
}

activity_history_container_image_matches() {
  local container_id="$1" expected="$2" actual
  if ! actual="$(docker inspect --format '{{.Config.Image}}' "$container_id" 2>/dev/null)"; then
    echo "activity-history: failed to inspect the running control-plane image" >&2
    return 1
  fi
  [[ -n "$expected" && "$actual" == "$expected" ]]
}

# Optional $1 selects receipt handling: the default 'consult-receipt' lets a
# valid durable receipt continue a previously activated deployment at zero
# running containers; any other value (freshstart passes 'skip-receipt') keeps
# the pre-receipt fail-closed behavior — the receipt proves activation of the
# EXISTING durable state, so it never authorizes a wipe-and-recreate (#2326).
activity_history_routine_guard() {
  local receipt_mode="${1:-consult-receipt}"
  activity_history_load_contract || return 1
  [[ "$ACTIVITY_HISTORY_RENDERED_GATE" == false ]] && return 0
  if [[ "$ACTIVITY_HISTORY_RENDERED_REPLICAS" != 1 ]]; then
    echo "activity-history: gate=true requires exactly one configured control-plane replica" >&2
    return 1
  fi
  activity_history_refresh_daemon_ids || return 1
  if [[ ${#ACTIVITY_HISTORY_DAEMON_IDS[@]} -eq 1 ]] \
     && activity_history_container_gate_matches "${ACTIVITY_HISTORY_DAEMON_IDS[0]:-}" true; then
    # Backfill: a stack activated before the receipt feature proves itself with
    # live one-container gate-true evidence; mint the durable receipt so
    # stop→up / down→up recovery works after this deploy. Best-effort — a
    # failed write warns (the writer's stderr) but never blocks the deploy,
    # whose authorization comes from the live container, not the receipt.
    activity_history_activation_receipt_valid \
      || activity_history_write_activation_receipt \
      || true
    return 0
  fi
  if [[ ${#ACTIVITY_HISTORY_DAEMON_IDS[@]} -eq 0 && "$receipt_mode" == consult-receipt ]]; then
    if activity_history_activation_receipt_valid; then
      echo "Activity History: durable activation receipt accepted; resuming the previously activated deployment."
      return 0
    fi
    if [[ -e "$ACTIVITY_HISTORY_ACTIVATION_RECEIPT" ]]; then
      echo "activity-history: activation receipt is malformed or bound to a different deployment; refusing to continue" >&2
      return 1
    fi
  fi
  echo "activity-history: routine deployment cannot perform first activation or repair ambiguous state" >&2
  echo "Run concord-ctl.sh activity-history-activate --confirm-drained after draining traffic." >&2
  return 1
}

activity_history_image_is_local() {
  local image="$1"
  if ! docker image inspect "$image" >/dev/null 2>&1; then
    echo "activity-history: required control-plane image is not present locally" >&2
    return 1
  fi
}

activity_history_run_admin() {
  local verb="$1"
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD run --rm --no-deps --pull never \
    control-plane ./main activity-history "$verb" --confirm-drained)
}

activity_history_start_control_plane() {
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD up -d --no-deps --no-build --pull never control-plane)
}

activity_history_stop_control_plane() {
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD stop --timeout 45 control-plane)
}

privacy_defaults_require_local_control_plane_drained() {
  # Compose stop addresses only this project. A second project on the same
  # daemon must not keep an old binary serving while startup applies the new
  # privacy and presence defaults. This check cannot inspect another host.
  activity_history_refresh_daemon_ids || return 1
  if [[ ${#ACTIVITY_HISTORY_DAEMON_IDS[@]} -ne 0 ]]; then
    echo "privacy-defaults: control-plane still runs on this Docker daemon; refusing startup migrations" >&2
    return 1
  fi
}

privacy_defaults_refuse_foreign_control_plane() {
  # The drain check above is daemon-wide, so a control plane from another
  # Compose project fails it only after this project's serving instance has
  # stopped, leaving the service down. Refuse while ours still serves. The
  # post-stop check stays: it covers one started after this preflight.
  local own id line owned
  activity_history_refresh_daemon_ids || return 1
  if ! own="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q -- control-plane 2>/dev/null)"; then
    echo "privacy-defaults: failed to inspect this project's control-plane" >&2
    return 1
  fi
  for id in ${ACTIVITY_HISTORY_DAEMON_IDS[@]+"${ACTIVITY_HISTORY_DAEMON_IDS[@]}"}; do
    # docker ps prints short IDs; compose ps -q prints full IDs.
    owned=0
    while IFS= read -r line; do
      [[ -n "$line" && "$line" == "$id"* ]] && { owned=1; break; }
    done <<<"$own"
    if (( ! owned )); then
      echo "privacy-defaults: another Compose project runs a control-plane on this Docker daemon; refusing before stopping this one" >&2
      return 1
    fi
  done
}

activity_history_wait_control_plane() {
  local timeout="${ACTIVITY_HISTORY_HEALTH_TIMEOUT:-60}"
  local interval="${ACTIVITY_HISTORY_HEALTH_INTERVAL:-2}"
  local deadline now remaining probe_limit sleep_for probed=0
  if [[ ! "$timeout" =~ ^[1-9][0-9]*$ || ! "$interval" =~ ^[0-9]+$ ]]; then
    echo "activity-history: health timeout/interval must be integer seconds" >&2
    return 1
  fi
  deadline=$(( $(date +%s) + timeout ))
  while true; do
    now="$(date +%s)"
    remaining=$((deadline - now))
    if (( remaining <= 0 )); then
      # ALWAYS probe at least once. `date +%s` is whole-second granularity, so the
      # clock can tick between computing `deadline` above and reading `now` here.
      # With a small timeout that made `remaining` 0 on the FIRST pass, and the
      # function returned "timed out" having never called curl at all — a health
      # wait that reports a timeout without probing is wrong on its own terms.
      #
      # Reproduced deterministically with a `date` stub controlling the tick:
      #   no tick  -> rc=0, curl invoked once
      #   one tick -> rc=1, curl invoked ZERO times
      # It surfaced as a rare flake in test-activity-history-config.sh, which pins
      # ACTIVITY_HISTORY_HEALTH_TIMEOUT=1 and so straddles the boundary most often.
      (( probed )) && break
      remaining=1
    fi
    probed=1
    probe_limit="$remaining"
    if (( interval > 0 && interval < probe_limit )); then
      probe_limit="$interval"
    fi
    if curl --connect-timeout "$probe_limit" --max-time "$probe_limit" \
      -sf 'http://127.0.0.1:8080/health' >/dev/null 2>&1; then
      return 0
    fi
    (( interval == 0 )) && break
    now="$(date +%s)"
    remaining=$((deadline - now))
    (( remaining > 0 )) || break
    sleep_for="$interval"
    (( sleep_for > remaining )) && sleep_for="$remaining"
    (( sleep_for > 0 )) && sleep "$sleep_for"
  done
  echo "activity-history: control-plane health check timed out" >&2
  return 1
}

activity_history_write_rollback_checkpoint() {
  local image="$1" requested="$2" state="$3"
  local temp="${ACTIVITY_HISTORY_IMAGE_SELECTOR}.new.$$"
  if [[ ! "$requested" =~ ^(true|false)$ ]] \
    || { [[ "$requested" == true ]] && [[ ! "$state" =~ ^(attempting|complete|failed)$ ]]; } \
    || { [[ "$requested" == false ]] && [[ "$state" != not_requested ]]; }; then
    echo "activity-history: invalid internal rollback checkpoint transition" >&2
    return 1
  fi
  umask 077
  if {
       printf 'CONTROL_PLANE_IMAGE=%s\n' "$image"
       printf 'ACTIVITY_HISTORY_ROLLBACK_ADMIN_COMPLETE=true\n'
       printf 'ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_REQUESTED=%s\n' "$requested"
       printf 'ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE=%s\n' "$state"
     } >"$temp" \
     && chmod 600 "$temp" \
     && mv "$temp" "$ACTIVITY_HISTORY_IMAGE_SELECTOR"; then
    :
  else
    rm -f "$temp"
    echo "activity-history: failed to persist rollback checkpoint state" >&2
    return 1
  fi
  ACTIVITY_HISTORY_IMAGE_SELECTOR_LOADED=1
  ACTIVITY_HISTORY_ROLLBACK_CHECKPOINT_IMAGE="$image"
  ACTIVITY_HISTORY_ROLLBACK_ADMIN_COMPLETE=true
  ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_REQUESTED="$requested"
  ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE="$state"
}

# Durable activation receipt (#2256). Written only after a guarded activation's
# health checks and final gate-true daemon invariant succeed. Parsed as data
# (never sourced); consulted by the routine guard only when zero control-plane
# containers are running, so it can never override live container evidence.
# Rollback and freshstart invalidate it before their destructive mutation.
activity_history_write_activation_receipt() {
  local temp="${ACTIVITY_HISTORY_ACTIVATION_RECEIPT}.new.$$"
  umask 077
  if {
       printf 'ACTIVITY_HISTORY_ACTIVATION_STATE=activated\n'
       printf 'ACTIVITY_HISTORY_ACTIVATION_CONTRACT_MODE=%s\n' "$ACTIVITY_HISTORY_CONTRACT_MODE"
       printf 'ACTIVITY_HISTORY_ACTIVATION_INSTANCE_TYPE=%s\n' "$ACTIVITY_HISTORY_RENDERED_INSTANCE_TYPE"
     } >"$temp" \
     && chmod 600 "$temp" \
     && mv "$temp" "$ACTIVITY_HISTORY_ACTIVATION_RECEIPT"; then
    :
  else
    rm -f "$temp"
    echo "activity-history: control-plane is running and healthy, but the activation receipt could not be persisted; stop→up / down→up recovery will require re-activation" >&2
    return 1
  fi
}

# Valid only when the receipt exists, is owned by the invoking user with mode
# 0600 (a restored/hand-copied receipt with looser permissions or foreign
# ownership is NOT trusted), parses cleanly (exactly the three known keys, each
# once), and is bound to the current contract mode and rendered instance type.
# Any deviation is invalid — callers fail closed.
activity_history_activation_receipt_valid() {
  local line value state="" mode="" instance="" perms
  local state_count=0 mode_count=0 instance_count=0
  [[ -f "$ACTIVITY_HISTORY_ACTIVATION_RECEIPT" \
     && -O "$ACTIVITY_HISTORY_ACTIVATION_RECEIPT" ]] || return 1
  perms="$(stat -c '%a' "$ACTIVITY_HISTORY_ACTIVATION_RECEIPT" 2>/dev/null \
    || stat -f '%Lp' "$ACTIVITY_HISTORY_ACTIVATION_RECEIPT" 2>/dev/null)" || true
  [[ "$perms" == 600 ]] || return 1
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -z "$line" ]] && continue
    value="${line#*=}"
    case "$line" in
      ACTIVITY_HISTORY_ACTIVATION_STATE=*)
        state_count=$((state_count + 1)); state="$value" ;;
      ACTIVITY_HISTORY_ACTIVATION_CONTRACT_MODE=*)
        mode_count=$((mode_count + 1)); mode="$value" ;;
      ACTIVITY_HISTORY_ACTIVATION_INSTANCE_TYPE=*)
        instance_count=$((instance_count + 1)); instance="$value" ;;
      *)
        return 1
        ;;
    esac
  done <"$ACTIVITY_HISTORY_ACTIVATION_RECEIPT"
  [[ $state_count -eq 1 && $mode_count -eq 1 && $instance_count -eq 1 ]] || return 1
  [[ "$state" == activated ]] || return 1
  [[ "$mode" == "$ACTIVITY_HISTORY_CONTRACT_MODE" ]] || return 1
  [[ "$instance" == "$ACTIVITY_HISTORY_RENDERED_INSTANCE_TYPE" ]]
}

activity_history_invalidate_activation_receipt() {
  rm -f "$ACTIVITY_HISTORY_ACTIVATION_RECEIPT" 2>/dev/null || true
  if [[ -e "$ACTIVITY_HISTORY_ACTIVATION_RECEIPT" ]]; then
    echo "activity-history: failed to invalidate the activation receipt; refusing to continue" >&2
    return 1
  fi
}

activity_history_select_checkpoint_image() {
  if [[ $ACTIVITY_HISTORY_IMAGE_SELECTOR_LOADED -ne 1 \
     || ! "$ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE" =~ ^(complete|not_requested)$ ]]; then
    echo "activity-history: rollback checkpoint is not resumable" >&2
    return 1
  fi
  CONTROL_PLANE_IMAGE="$ACTIVITY_HISTORY_ROLLBACK_CHECKPOINT_IMAGE"
  ACTIVITY_HISTORY_IMAGE_SELECTOR_SELECTED=1
  export CONTROL_PLANE_IMAGE
  if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]]; then
    selfhost_storage_select_image "$CONTROL_PLANE_IMAGE" || return 1
  fi
}

activity_history_finish_rollback_start() {
  if ! activity_history_start_control_plane; then
    activity_history_stop_control_plane >/dev/null 2>&1 || true
    echo "activity-history: rollback old-image start failed; checkpoint retained for exact retry" >&2
    return 1
  fi
  if ! activity_history_wait_control_plane; then
    activity_history_stop_control_plane >/dev/null 2>&1 || true
    echo "activity-history: rollback health failed; checkpoint retained for exact retry" >&2
    return 1
  fi
  if ! activity_history_refresh_daemon_ids \
     || [[ ${#ACTIVITY_HISTORY_DAEMON_IDS[@]} -ne 1 ]] \
     || ! activity_history_container_image_matches \
       "${ACTIVITY_HISTORY_DAEMON_IDS[0]:-}" "$ACTIVITY_HISTORY_ROLLBACK_CHECKPOINT_IMAGE" \
     || ! activity_history_container_gate_matches \
       "${ACTIVITY_HISTORY_DAEMON_IDS[0]:-}" false; then
    activity_history_stop_control_plane >/dev/null 2>&1 || true
    echo "activity-history: rollback post-start invariant failed; checkpoint retained for exact retry" >&2
    return 1
  fi
  echo "Activity History rollback completed with one healthy control-plane."
}

activity_history_activate() {
  if [[ $# -ne 1 || "$1" != --confirm-drained ]]; then
    echo "Usage: concord-ctl.sh activity-history-activate --confirm-drained" >&2
    return 2
  fi
  activity_history_load_contract || return 1
  if [[ "$ACTIVITY_HISTORY_RENDERED_GATE" != true \
     || "$ACTIVITY_HISTORY_RENDERED_REPLICAS" != 1 ]]; then
    echo "activity-history: activation requires rendered gate=true and replica count=1" >&2
    return 1
  fi
  activity_history_refresh_daemon_ids || return 1
  if [[ ${#ACTIVITY_HISTORY_DAEMON_IDS[@]} -ne 0 ]]; then
    echo "activity-history: activation requires zero running control-plane containers daemon-wide" >&2
    return 1
  fi
  activity_history_image_is_local "$ACTIVITY_HISTORY_RENDERED_IMAGE" || return 1
  echo "Activity History activation: zero daemon-wide control-planes confirmed; running non-serving preflight, CP-only start, health, and final count."
  activity_history_run_admin preflight || return 1
  if ! activity_history_start_control_plane; then
    activity_history_stop_control_plane >/dev/null 2>&1 || true
    echo "activity-history: control-plane start failed; cleanup attempted" >&2
    return 1
  fi
  if ! activity_history_wait_control_plane; then
    activity_history_stop_control_plane >/dev/null 2>&1 || true
    return 1
  fi
  if ! activity_history_refresh_daemon_ids \
     || [[ ${#ACTIVITY_HISTORY_DAEMON_IDS[@]} -ne 1 ]] \
     || ! activity_history_container_gate_matches "${ACTIVITY_HISTORY_DAEMON_IDS[0]:-}" true; then
    activity_history_stop_control_plane >/dev/null 2>&1 || true
    echo "activity-history: activation post-start invariant failed; control-plane stopped" >&2
    return 1
  fi
  activity_history_write_activation_receipt || return 1
  echo "Activity History activation completed with one healthy control-plane."
}

activity_history_parse_rollback_args() {
  if [[ $# -ne 3 && $# -ne 4 ]]; then
    echo "Usage: concord-ctl.sh activity-history-rollback --confirm-drained --old-image <immutable-image-ref> [--downgrade-schema]" >&2
    return 2
  fi
  if [[ "$1" != --confirm-drained || "$2" != --old-image \
     || -z "$3" || "$3" == --* \
     || ( $# -eq 4 && "$4" != --downgrade-schema ) ]]; then
    echo "activity-history-rollback: flags must match the documented command exactly" >&2
    return 2
  fi
  ACTIVITY_HISTORY_ROLLBACK_OLD_IMAGE="$3"
  ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE=$(( $# == 4 ? 1 : 0 ))
}

activity_history_rollback() {
  local old_image requested=false
  activity_history_parse_rollback_args "$@" || return $?
  old_image="$ACTIVITY_HISTORY_ROLLBACK_OLD_IMAGE"
  [[ $ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE -eq 1 ]] && requested=true
  if [[ "$old_image" == *[[:space:]]* \
     || ! "$old_image" =~ ^(sha256:[[:xdigit:]]{64}|.+@sha256:[[:xdigit:]]{64})$ ]]; then
    echo "activity-history: --old-image must be an immutable sha256 digest reference" >&2
    return 2
  fi

  # A checkpoint exists only after disable-all completed. Exact matching
  # retries resume the old-image start and never repeat maintenance. An
  # attempting state is deliberately ambiguous across host loss: the exact
  # step may already have reached 86, so repeating it is unsafe, while an older
  # migrator may not recognize v87. Automatic recovery therefore refuses both
  # downgrade and old-image start.
  if [[ $ACTIVITY_HISTORY_IMAGE_SELECTOR_LOADED -eq 1 ]]; then
    if [[ "$old_image" != "$ACTIVITY_HISTORY_ROLLBACK_CHECKPOINT_IMAGE" ]]; then
      echo "activity-history: checkpoint does not match the requested old image; refusing before mutation" >&2
      return 1
    fi
    if [[ "$requested" != "$ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_REQUESTED" ]]; then
      echo "activity-history: checkpoint downgrade intent does not match the requested command; refusing before mutation" >&2
      return 1
    fi
    if [[ "$ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE" == failed ]]; then
      echo "activity-history: checkpoint records an explicit downgrade failure; inspect recovery state or rebuild current source before retrying" >&2
      return 1
    fi
    if [[ "$ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE" == attempting ]]; then
      echo "activity-history: downgrade completion is ambiguous; automatic resume is refused" >&2
      echo "Keep the current image selected and inspect migration state; use a guarded current-source rebuild for recovery. Do not repeat exact downgrade automatically." >&2
      return 1
    fi
    activity_history_image_is_local "$old_image" || return 1
    activity_history_load_contract || return 1
    if [[ "$ACTIVITY_HISTORY_RENDERED_GATE" != false ]]; then
      echo "activity-history: rollback retry requires rendered gate=false" >&2
      return 1
    fi
    activity_history_refresh_daemon_ids || return 1
    if [[ ${#ACTIVITY_HISTORY_DAEMON_IDS[@]} -ne 0 ]]; then
      echo "activity-history: rollback retry requires zero running control-plane containers" >&2
      return 1
    fi
    voice_enforcement_rollout_stop_media || return 1
    voice_enforcement_rollout_require_rollback_zero_state || return 1
    voice_enforcement_rollout_require_inactive_current_image || return 1
    echo "Activity History rollback checkpoint matches: admin work is complete; resuming pinned old-image start, health, and final count without repeating disable or downgrade."
    activity_history_invalidate_activation_receipt || return 1
    activity_history_finish_rollback_start
    return
  fi

  activity_history_image_is_local "$old_image" || return 1
  activity_history_load_contract || return 1
  if [[ "$ACTIVITY_HISTORY_RENDERED_GATE" != false ]]; then
    echo "activity-history: rollback requires rendered gate=false" >&2
    return 1
  fi
  # The currently rendered image contains the administrative subcommands used
  # while the service is drained. Refuse before stopping if it is unavailable.
  activity_history_image_is_local "$ACTIVITY_HISTORY_RENDERED_IMAGE" || return 1
  echo "Activity History rollback: stopping/verifying local media, proving voice zero-state, deactivating durable enforcement, then stopping CP with a 45s drain, disabling durable state, journaling optional exact downgrade, selecting the pinned old image, and requiring one healthy CP."
  voice_enforcement_rollout_stop_media || return 1
  voice_enforcement_rollout_require_rollback_zero_state || return 1
  voice_enforcement_rollout_deactivate_for_rollback || return 1
  activity_history_invalidate_activation_receipt || return 1
  activity_history_stop_control_plane || return 1
  activity_history_refresh_daemon_ids || return 1
  if [[ ${#ACTIVITY_HISTORY_DAEMON_IDS[@]} -ne 0 ]]; then
    echo "activity-history: rollback requires zero running control-plane containers after stop" >&2
    return 1
  fi
  # The pre-stop attest admits the planned drain; this one is the authority for
  # old-image safety. With no serving current CP left, no request can enqueue a
  # reconciliation between the proof and disable-all.
  voice_enforcement_rollout_require_rollback_zero_state || return 1
  activity_history_run_admin disable-all || return 1
  if [[ $ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE -eq 1 ]]; then
    activity_history_write_rollback_checkpoint \
      "$old_image" true attempting || return 1
    if ! activity_history_run_admin downgrade-schema; then
      if ! activity_history_write_rollback_checkpoint \
        "$old_image" true failed; then
        echo "activity-history: exact downgrade failed and failure-state journaling also failed; attempting checkpoint retained" >&2
      else
        echo "activity-history: exact downgrade failed; failed checkpoint retained and automatic resume is refused" >&2
      fi
      return 1
    fi
    activity_history_write_rollback_checkpoint \
      "$old_image" true complete || return 1
  else
    activity_history_write_rollback_checkpoint \
      "$old_image" false not_requested || return 1
  fi
  activity_history_select_checkpoint_image || return 1
  activity_history_finish_rollback_start
}

# The R01 overlay is a lab candidate only. Consume a config-only render on
# stdin, never an env file, and report no values from that secret-bearing JSON.
media_shared_listeners_check_model() {
  python3 -c '
import json, re, sys

def refuse():
    print("media-shared-listeners: candidate render refused; require one published UDP listener per worker, loopback HTTP, TCP disabled and bridge networking", file=sys.stderr)
    raise SystemExit(1)

def ports(value):
    value = str(value)
    if not re.fullmatch(r"[1-9][0-9]{0,4}(?:-[1-9][0-9]{0,4})?", value):
        refuse()
    ends = [int(part) for part in value.split("-")]
    lower, upper = ends[0], ends[-1]
    if not 1 <= lower <= upper <= 65535:
        refuse()
    return list(range(lower, upper + 1))

try:
    model = json.load(sys.stdin)
    media = model["services"]["media-plane"]
    environment = media["environment"]
    if not isinstance(environment, dict):
        refuse()
    if environment.get("MEDIASOUP_SHARED_WEBRTC_SERVER") != "true" or environment.get("MEDIASOUP_ENABLE_TCP") != "false" or environment.get("PORT") != "3000":
        refuse()
    workers = environment["NUM_WORKERS"]
    if not isinstance(workers, str) or not re.fullmatch(r"[1-9][0-9]?", workers) or not 1 <= int(workers) <= 32:
        refuse()
    allocation = environment["MEDIASOUP_WEBRTC_SERVER_PORTS"]
    if not isinstance(allocation, str) or not re.fullmatch(r"[1-9][0-9]{0,4}-[1-9][0-9]{0,4}", allocation):
        refuse()
    listeners = ports(allocation)
    if listeners[0] < 1024 or len(listeners) != int(workers):
        refuse()
    if media.get("network_mode") or not isinstance(media.get("networks"), (dict, list)) or not media["networks"]:
        refuse()
    for name in media["networks"]:
        network = model["networks"][name]
        options = network.get("driver_opts") or dict()
        if network.get("external") or network.get("name") == "host" or network.get("driver", "bridge") != "bridge":
            refuse()
        if "com.docker.network.bridge.trusted_host_interfaces" in options:
            refuse()
        for family in ("ipv4", "ipv6"):
            if options.get("com.docker.network.bridge.gateway_mode_" + family, "nat") not in ("nat", "isolated"):
                refuse()
    actual = []
    for binding in media["ports"]:
        if not isinstance(binding, dict):
            refuse()
        targets, published = ports(binding["target"]), ports(binding["published"])
        if targets != published:
            refuse()
        protocol, host = binding.get("protocol", "tcp"), binding.get("host_ip", "")
        if protocol == "tcp" and targets == [3000] and host == "127.0.0.1":
            actual.append(("http", 3000))
        elif protocol == "udp" and host in ("", "0.0.0.0", "::"):
            actual.extend(("udp", port) for port in targets)
        else:
            refuse()
    if sorted(actual) != sorted([("http", 3000)] + [("udp", port) for port in listeners]):
        refuse()
except (KeyError, TypeError, ValueError, AttributeError, OverflowError, RecursionError):
    refuse()
print("media-shared-listeners: lab render matches the shared listener contract; deployment remains unadmitted")
'
}

media_shared_listeners_routine_guard() {
  local rendered ids id environment verdict
  if ! rendered="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD config --format json 2>/dev/null)"; then
    echo 'media-shared-listeners: unable to inspect desired media mode; refusing service mutation' >&2
    return 1
  fi
  if ! verdict="$(python3 -c '
import json, sys
try:
    environment = json.load(sys.stdin)["services"].get("media-plane", dict()).get("environment", dict())
    value = environment.get("MEDIASOUP_SHARED_WEBRTC_SERVER", "false")
    if value not in ("false", "true"):
        raise ValueError()
    print(value)
except (KeyError, TypeError, ValueError, AttributeError, RecursionError):
    raise SystemExit(1)
' <<<"$rendered" 2>/dev/null)"; then
    echo 'media-shared-listeners: desired media mode is ambiguous; refusing service mutation' >&2
    return 1
  fi
  if [[ "$verdict" == true ]]; then
    echo 'media-shared-listeners: shared deployment is not admitted; coordinated migration support is required' >&2
    return 1
  fi
  # Include stopped containers and other projects: a routine deploy/rollback
  # must not silently replace a shared generation after an operator stopped it.
  if ! ids="$(docker ps --all --quiet --no-trunc --filter label=com.docker.compose.service=media-plane 2>/dev/null)"; then
    echo 'media-shared-listeners: unable to discover existing media mode; refusing service mutation' >&2
    return 1
  fi
  while IFS= read -r id; do
    [[ -n "$id" ]] || continue
    if [[ ! "$id" =~ ^[[:xdigit:]]{64}$ ]] \
      || ! environment="$(docker inspect --format '{{json .Config.Env}}' "$id" 2>/dev/null)"; then
      echo 'media-shared-listeners: unable to inspect existing media mode; refusing service mutation' >&2
      return 1
    fi
    if ! verdict="$(python3 -c '
import json, sys
try:
    environment = json.load(sys.stdin)
    if not isinstance(environment, list) or any(not isinstance(item, str) for item in environment):
        raise ValueError()
    values = [item.split("=", 1)[1] for item in environment if item.startswith("MEDIASOUP_SHARED_WEBRTC_SERVER=")]
    if len(values) > 1 or (values and values[0] not in ("false", "true")):
        raise ValueError()
    print(values[0] if values else "false")
except (TypeError, ValueError, RecursionError):
    raise SystemExit(1)
' <<<"$environment" 2>/dev/null)"; then
      echo 'media-shared-listeners: existing media mode is ambiguous; refusing service mutation' >&2
      return 1
    fi
    if [[ "$verdict" == true ]]; then
      echo 'media-shared-listeners: existing shared media requires coordinated rollback support; routine service mutation refused' >&2
      return 1
    fi
  done <<<"$ids"
}

# Voice-enforcement rollout (#3140).  The control-plane owns the durable
# activation bit; the deploy controller only invokes its CLI and verifies the
# media-plane capability boundary.  Never replace either operation with shell
# SQL or a health-only probe: /health predates the targeted subscription.
voice_enforcement_rollout_cli() {
  local verb="$1"
  shift
  case "$verb" in
    activate|deactivate|status) ;;
    *)
      echo "voice-enforcement-rollout: invalid CLI verb '$verb'" >&2
      return 2
      ;;
  esac
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD run --rm --no-deps --pull never \
    control-plane ./main voice-enforcement-rollout "$verb" "$@")
}

voice_enforcement_rollout_deactivate_for_rollback() {
  if ! voice_enforcement_rollout_cli deactivate; then
    echo "activity-history: unable to deactivate voice enforcement with the current control-plane; refusing old-image rollback" >&2
    return 1
  fi
}

voice_enforcement_rollout_require_inactive_current_image() {
  local status
  # A resumable rollback has no current CP container left to execute the
  # command. Do not run the status probe through the pinned old image: an old
  # binary may not contain this CLI. Unsetting the selector makes Compose
  # resolve the current image from the canonical environment instead.
  if ! status="$(unset CONTROL_PLANE_IMAGE
    if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]]; then
      selfhost_storage_select_image '' || exit 1
    fi
    voice_enforcement_rollout_cli status 2>&1)"; then
    echo "activity-history: unable to verify inactive voice enforcement with the current control-plane; refusing old-image rollback" >&2
    return 1
  fi
  if [[ "$status" != *"voice-enforcement-rollout: inactive"* ]]; then
    echo "activity-history: voice enforcement is not inactive; refusing old-image rollback" >&2
    return 1
  fi
}

voice_enforcement_rollout_require_rollback_zero_state() {
  local status evidence
  # The current image must attest every durable obligation before an old CP
  # can start. Shell SQL is intentionally forbidden here; absent evidence is
  # a refusal, not an invitation to guess from worker health.
  if ! status="$(unset CONTROL_PLANE_IMAGE
    if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]]; then
      selfhost_storage_select_image '' || exit 1
    fi
    voice_enforcement_rollout_cli status 2>&1)"; then
    echo "activity-history: unable to obtain current-image voice-enforcement zero-state evidence; refusing old-image rollback" >&2
    return 1
  fi
  for evidence in registryRows=0 dmParentRows=0 credentialParentRows=0 dmReconciliationRows=0; do
    if [[ "$status" != *"voice-enforcement-rollout: $evidence"* ]]; then
      echo "activity-history: current-image status did not attest $evidence; refusing old-image rollback" >&2
      return 1
    fi
  done
  echo "activity-history: current-image registry/outbox/reconciliation zero-state verified; rollback evidence is single-host only (multi-host media is unsupported)"
}

voice_enforcement_rollout_activate() {
  # The application requires this operator receipt.  This helper is private
  # to the ordered rollout paths below and is called only after old-media
  # stop/verification and the replacement media protocol gate have passed.
  voice_enforcement_rollout_cli activate --confirm-drained
}

voice_enforcement_rollout_wait_control_plane() {
  local timeout="${VOICE_ENFORCEMENT_HEALTH_TIMEOUT:-180}"
  local deadline status
  [[ "$timeout" =~ ^[1-9][0-9]*$ ]] || {
    echo "voice-enforcement-rollout: health timeout must be a positive integer" >&2
    return 1
  }
  deadline=$(( $(date +%s) + timeout ))
  # Probe BEFORE testing the deadline, as activity_history_wait_control_plane does:
  # `date +%s` is whole-second, so with a small timeout the clock can reach
  # `deadline` before the first pass and the wait would fail without probing.
  while :; do
    if curl -sf --max-time 2 http://127.0.0.1:8080/readyz >/dev/null 2>&1; then
      return 0
    fi
    (( $(date +%s) < deadline )) || break
    sleep 2
  done
  echo "voice-enforcement-rollout: control-plane did not become ready" >&2
  return 1
}

voice_enforcement_rollout_wait_media() {
  local timeout="${VOICE_ENFORCEMENT_HEALTH_TIMEOUT:-180}"
  local deadline body status
  [[ "$timeout" =~ ^[1-9][0-9]*$ ]] || {
    echo "voice-enforcement-rollout: health timeout must be a positive integer" >&2
    return 1
  }
  body="$(mktemp)"
  deadline=$(( $(date +%s) + timeout ))
  while :; do  # probe before the deadline test; see voice_enforcement_rollout_wait_control_plane
    : >"$body"
    status="$(curl -sS --max-time 2 -o "$body" -w '%{http_code}' \
      http://127.0.0.1:3000/readyz 2>/dev/null || true)"
    if [[ "$status" == 200 ]] && python3 -c '
import json
import sys

try:
    with open(sys.argv[1], encoding="utf-8") as stream:
        value = json.load(stream)
except (OSError, ValueError):
    raise SystemExit(1)
raise SystemExit(0 if isinstance(value, dict) and value.get("ready") is True
                 and value.get("voiceEnforcementProtocol") == 3 else 1)
' "$body"; then
      rm -f "$body"
      return 0
    fi
    (( $(date +%s) < deadline )) || break
    sleep 2
  done
  rm -f "$body"
  echo "voice-enforcement-rollout: media-plane did not advertise protocol 3 readiness" >&2
  return 1
}

voice_enforcement_rollout_stop_media() {
  local running
  echo "voice-enforcement-rollout: draining the old media-plane"
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD stop --timeout 45 media-plane)
  if ! running="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q media-plane 2>/dev/null)"; then
    echo "voice-enforcement-rollout: cannot verify that the old media-plane stopped" >&2
    return 1
  fi
  if [[ -n "$running" ]]; then
    echo "voice-enforcement-rollout: old media-plane is still running; refusing to start a replacement" >&2
    return 1
  fi
}

voice_enforcement_rollout_deploy() {
  [[ $# -eq 0 ]] || {
    echo "Usage: concord-ctl.sh voice-enforcement-rollout deploy" >&2
    return 2
  }
  local -a restore_pull=()
  [[ ${STAGE_RESTORE_MODE:-0} -eq 0 ]] || restore_pull=(--pull never)

  # The paired rollout always replaces the control-plane image.  Do not let a
  # stale Activity History rollback pin silently select an old digest here.
  clear_activity_history_image_selector

  # Build first so the new image owns the migration and the compatibility
  # default before any media handoff.  The explicit deactivation follows CP
  # readiness because the rollout singleton is created by CP startup migration.
  if [[ ${STAGE_RESTORE_MODE:-0} -eq 0 ]]; then
    echo "voice-enforcement-rollout: building the new control-plane image"
    (cd "$DEPLOY_DIR" && $COMPOSE_CMD build control-plane)
  fi
  # Migrations 000165–000166 change privacy and presence defaults. Stop and
  # drain the previous local binary before startup can apply them; older
  # writers must not run beside the new policy. Multi-host deployments must
  # drain the other replicas before invoking this rollout.
  echo "voice-enforcement-rollout: draining the previous control-plane"
  privacy_defaults_refuse_foreign_control_plane || return 1
  activity_history_stop_control_plane || return 1
  privacy_defaults_require_local_control_plane_drained || return 1
  echo "voice-enforcement-rollout: starting the control-plane in compatibility mode"
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD up -d --no-deps --no-build "${restore_pull[@]}" --wait \
    --wait-timeout 180 control-plane)
  voice_enforcement_rollout_wait_control_plane
  voice_enforcement_rollout_cli deactivate

  # The old media container is explicitly stopped before the new image is
  # created or started.  A single compose up for CP+MP has ambiguous ordering
  # and can leave old sockets unregistered during the handoff.
  voice_enforcement_rollout_stop_media
  if [[ ${STAGE_RESTORE_MODE:-0} -eq 0 ]]; then
    echo "voice-enforcement-rollout: building the new media-plane image"
    (cd "$DEPLOY_DIR" && $COMPOSE_CMD build media-plane)
  fi
  echo "voice-enforcement-rollout: starting the new media-plane"
  echo "voice-enforcement-rollout: waiting for media-plane liveness"
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD up -d --no-deps --no-build "${restore_pull[@]}" --wait \
    --wait-timeout 180 media-plane)
  echo "voice-enforcement-rollout: waiting for media-plane protocol 3 readiness"
  voice_enforcement_rollout_wait_media

  echo "voice-enforcement-rollout: activating durable enforcement"
  voice_enforcement_rollout_activate
  echo "voice-enforcement-rollout: completed"
}

voice_enforcement_rollout_media_rebuild() {
  voice_enforcement_rollout_cli deactivate
  voice_enforcement_rollout_stop_media
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD build media-plane)
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD up -d --no-deps --no-build --wait \
    --wait-timeout 180 media-plane)
  voice_enforcement_rollout_wait_media
  voice_enforcement_rollout_activate
}

# The blue/green rail is enabled only when the rendered model contains both
# closed color services with their expected loopback and RTC port assignments.
# A partial overlay must never fall back to a service-less Compose invocation.
media_bluegreen_render() {
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD config --format json 2>/dev/null)
}

media_bluegreen_model_present() {
  local rendered
  rendered="$(media_bluegreen_render)" || return 2
  python3 -c '
import json, sys
try:
    services = json.load(sys.stdin).get("services", {})
    placement = services.get("control-plane", {}).get("environment", {}).get("ROOM_PLACEMENT_ENABLED")
except (ValueError, TypeError, AttributeError):
    raise SystemExit(2)
raise SystemExit(0 if placement not in (None, "false") or any(n in services for n in ("media-plane-blue", "media-plane-green")) else 1)
' <<<"$rendered"
}

media_bluegreen_validate_model() {
  local rendered
  rendered="$(media_bluegreen_render)" || {
    echo 'media-bluegreen: cannot render the Compose model; refusing before mutation' >&2
    return 1
  }
  python3 -c '
import json, sys
from urllib.parse import urlsplit
try:
    services = json.load(sys.stdin)["services"]
    if "media-plane" in services:
        raise ValueError("legacy media-plane is active alongside the color model")
    if services["control-plane"].get("environment", {}).get("ROOM_PLACEMENT_ENABLED") != "true":
        raise ValueError("control-plane room placement is not armed")
    for colour, host_port, first_rtc in (("blue", "3000", "40000"), ("green", "3001", "42000")):
        name = "media-plane-" + colour
        service = services[name]
        env = service.get("environment", {})
        if (env.get("MEDIA_INSTANCE_COLOR") != colour or env.get("ROOM_PLACEMENT_ENABLED") != "true"
                or env.get("INITIAL_NUM_WORKERS") != "1"
                or env.get("NUM_WORKERS") != "3"):
            raise ValueError(name + " has an invalid identity or initial worker count")
        if "media-bluegreen" not in service.get("profiles", []) or "media-single" in service.get("profiles", []):
            raise ValueError(name + " must be limited to the bluegreen profile")
        if env.get("RTC_MIN_PORT") != first_rtc or env.get("RTC_MAX_PORT") != str(int(first_rtc) + 2):
            raise ValueError(name + " has an invalid RTC window")
        if service.get("network_mode"):
            raise ValueError(name + " must use bridge networking")
        ports = service.get("ports", [])
        signal = [p for p in ports if p.get("target") == 3000 and p.get("protocol") == "tcp"]
        rtc = [p for p in ports if p.get("protocol") == "udp" and int(p["target"]) >= int(first_rtc)]
        if len(signal) != 1 or signal[0].get("host_ip") != "127.0.0.1" or signal[0].get("published") != host_port:
            raise ValueError(name + " signaling must be published on its assigned loopback port")
        expected = {str(p) for p in range(int(first_rtc), int(first_rtc) + 3)}
        if {str(p["target"]) for p in rtc} != expected or {str(p["published"]) for p in rtc} != expected:
            raise ValueError(name + " RTC publication does not match its three-port WebRtcServer window")
        if not {"nats", "redis", "control-plane"}.issubset(service.get("depends_on", {})):
            raise ValueError(name + " is missing a voice dependency gate")
    if sys.argv[1] == "docker-compose.staging.yml":
        proxy = services["staging-media-proxy"]
        endpoint = urlsplit(services["control-plane"]["environment"]["MEDIA_PLANE_URL"])
        published = [p for p in proxy.get("ports", []) if p.get("protocol") == "tcp"]
        if (endpoint.scheme != "http" or len(published) != 1
                or published[0].get("host_ip") != endpoint.hostname
                or published[0].get("published") != str(endpoint.port)
                or published[0].get("target") != 3000
                or "media-bluegreen" not in proxy.get("profiles", [])
                or "concordvoice-services" not in proxy.get("networks", {})):
            raise ValueError("staging media ingress does not match the advertised URL")
    print("bluegreen")
except (KeyError, TypeError, ValueError, json.JSONDecodeError) as exc:
    print("media-bluegreen: rendered model rejected: " + str(exc), file=sys.stderr)
    raise SystemExit(1)
' "$COMPOSE_OVERRIDE" <<<"$rendered"
}

media_bluegreen_verify_standby_image() {
  local colour="$1" container="$2" rendered project service expected actual config_output expected_config actual_config
  [[ -n "$container" && "$container" != *$'\n'* ]] || {
    echo 'media-bluegreen: running standby must resolve to exactly one container' >&2
    return 1
  }
  service="media-plane-${colour}"
  rendered="$(media_bluegreen_render)" || return 1
  project="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["name"])' <<<"$rendered")" || return 1
  [[ -n "$project" && "$project" != *[[:space:]]* ]] || return 1
  # Build the current source without replacing the running empty standby, then
  # compare immutable image IDs before allowing an interrupted cutover to resume.
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD build --quiet "$service") || {
    echo "media-bluegreen: cannot build current ${service} image; refusing to reuse the running standby" >&2
    return 1
  }
  expected="$(docker image inspect --format '{{.Id}}' "${project}-${service}")" || {
    echo "media-bluegreen: cannot inspect the current ${service} image; refusing standby reuse" >&2
    return 1
  }
  actual="$(docker inspect --format '{{.Image}}' "$container")" || {
    echo "media-bluegreen: cannot inspect the running ${service}; refusing standby reuse" >&2
    return 1
  }
  config_output="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD config --hash="$service")" || {
    echo "media-bluegreen: cannot verify current ${service} configuration; refusing standby reuse" >&2
    return 1
  }
  expected_config="$(awk -v service="$service" '$1 == service { print $2 }' <<<"$config_output")"
  actual_config="$(docker inspect --format '{{ index .Config.Labels "com.docker.compose.config-hash" }}' "$container")" || {
    echo "media-bluegreen: cannot inspect the running ${service} configuration; refusing standby reuse" >&2
    return 1
  }
  [[ -n "$expected" && "$actual" == "$expected" && -n "$expected_config" \
     && "$actual_config" == "$expected_config" ]] || {
    echo "media-bluegreen: running ${service} image or configuration is stale; remove the empty standby under an approved operator procedure before retrying" >&2
    return 1
  }
}

media_bluegreen_redis() {
  local password
  password="$(activity_history_read_root_value REDIS_PASSWORD)" || {
    echo 'media-bluegreen: canonical REDIS_PASSWORD is unavailable; refusing' >&2
    return 1
  }
  if [[ -z "$password" || ! "$password" =~ ^[A-Za-z0-9._~-]+$ ]]; then
    echo 'media-bluegreen: canonical REDIS_PASSWORD violates the URI-safe password contract; refusing' >&2
    return 1
  fi
  local REDISCLI_AUTH="$password"
  export REDISCLI_AUTH
  # A Redis error reply must fail the command before a cutover arms the drain.
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD exec -T -e REDISCLI_AUTH redis redis-cli -e --raw "$@" 2>/dev/null)
}

media_bluegreen_port() {
  case "$1" in
    blue) printf '3000' ;;
    green) printf '3001' ;;
    *) return 1 ;;
  esac
}

media_bluegreen_active_ready() {
  local colour port
  colour="$(media_bluegreen_redis GET media:active_colour)" || return 1
  port="$(media_bluegreen_port "$colour")" || return 1
  media_bluegreen_probe_ready "$colour" true "$port"
}

media_bluegreen_dependencies_ready() {
  local service rows
  for service in nats redis control-plane "$@"; do
    rows="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --format json -- "$service")" || {
      echo "media-bluegreen: cannot inspect ${service} dependency; refusing" >&2
      return 1
    }
    if ! python3 -c '
import json, sys
raw = sys.stdin.read().strip()
try:
    rows = json.loads(raw) if raw.startswith("[") else [json.loads(line) for line in raw.splitlines() if line.strip()]
    valid = bool(rows) and all(row.get("State") == "running" and row.get("Health") == "healthy" for row in rows)
except (TypeError, ValueError):
    valid = False
raise SystemExit(0 if valid else 1)
' <<<"$rows"; then
      echo "media-bluegreen: ${service} dependency is absent or unhealthy; refusing cutover" >&2
      return 1
    fi
  done
}

media_bluegreen_probe_ready() {
  local colour="$1" expected_accept="$2" port="$3" expected_workers="${4:-}" expected_empty="${5:-false}" status body
  body="$(mktemp)" || return 1
  status="$(curl -sS --max-time 3 -o "$body" -w '%{http_code}' \
    "http://127.0.0.1:${port}/readyz" 2>/dev/null || true)"
  if ! python3 -c '
import json, sys
try:
    value = json.load(open(sys.argv[1], encoding="utf-8"))
    count = value["rooms"]
    creates = value["creates_in_flight"]
    generation = value["generation"]
    valid = (value["colour"] == sys.argv[2]
             and value.get("ready") is (sys.argv[3] == "true")
             and (sys.argv[3] == "false" or value.get("voiceEnforcementProtocol") == 3)
             and value["accepting_new_rooms"] is (sys.argv[3] == "true")
             and type(count) is int and count >= 0
             and type(creates) is int and creates >= 0
             and (sys.argv[5] != "true" or (count == 0 and creates == 0))
             and (type(generation) is int or (isinstance(generation, str) and generation)))
    if sys.argv[4] == "200":
        valid = valid and sys.argv[3] == "true"
    elif sys.argv[4] != "503" or sys.argv[3] != "false":
        valid = False
except (OSError, ValueError, KeyError, TypeError):
    valid = False
raise SystemExit(0 if valid else 1)
' "$body" "$colour" "$expected_accept" "$status" "$expected_empty"; then
    rm -f "$body"
    return 1
  fi
  if [[ -n "$expected_workers" ]]; then
    if [[ "$status" != 200 ]] || ! curl -sS --max-time 3 -o "$body" \
      "http://127.0.0.1:${port}/health" 2>/dev/null \
      || ! python3 -c '
import json, sys
try:
    value = json.load(open(sys.argv[1], encoding="utf-8"))
    valid = value.get("colour") == sys.argv[2] and type(value.get("workers")) is int \
        and value["workers"] == int(sys.argv[3])
except (OSError, ValueError, TypeError):
    valid = False
raise SystemExit(0 if valid else 1)
' "$body" "$colour" "$expected_workers"; then
      rm -f "$body"
      return 1
    fi
  fi
  rm -f "$body"
}

media_bluegreen_preflight() {
  local rendered active project legacy colour ids standby tombstone recover_control_plane="${1:-0}"
  rendered="$(media_bluegreen_render)" || {
    echo 'media-bluegreen: cannot render Compose; refusing before mutation' >&2
    return 1
  }
  media_bluegreen_validate_model >/dev/null || return 1
  active="$(media_bluegreen_redis GET media:active_colour)" || {
    echo 'media-bluegreen: active colour cannot be read; refusing before mutation' >&2
    return 1
  }
  project="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["name"])' <<<"$rendered")" || return 1
  [[ -n "$project" && "$project" != *[[:space:]]* ]] || return 1
  legacy="$(docker ps --filter "label=com.docker.compose.project=${project}" \
    --filter 'label=com.docker.compose.service=media-plane' --format '{{.ID}}')" || {
    echo 'media-bluegreen: legacy container inspection failed; refusing' >&2
    return 1
  }
  if [[ -n "$legacy" ]]; then
    echo 'media-bluegreen: live legacy media-plane blocks first activation; an approved quiescent migration is required' >&2
    return 1
  fi
  case "$active" in
    '') standby=blue ;;
    blue) standby=green ;;
    green) standby=blue ;;
    *) echo 'media-bluegreen: unknown active colour; refusing' >&2; return 1 ;;
  esac
  for colour in blue green; do
    ids="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q "media-plane-${colour}")" || {
      echo "media-bluegreen: cannot inspect ${colour} container; refusing" >&2
      return 1
    }
    if [[ -z "$active" && -n "$ids" && "$colour" != "$standby" ]]; then
      echo 'media-bluegreen: colour container exists without promotion; refusing' >&2
      return 1
    fi
    if [[ "$colour" == "$standby" && -n "$ids" ]]; then
      if ! media_bluegreen_probe_ready "$colour" true "$(media_bluegreen_port "$colour")" 1 true; then
        echo "media-bluegreen: running standby ${colour} is not empty and ready for promotion" >&2
        return 1
      fi
    fi
  done
  tombstone="$(media_bluegreen_redis EXISTS "media:colour_draining:${standby}")" || return 1
  [[ "$tombstone" == 0 ]] || {
    echo "media-bluegreen: standby ${standby} has a drain tombstone; reap before another cutover" >&2
    return 1
  }
  if [[ -n "$active" ]]; then
    ids="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q "media-plane-${active}")" || return 1
    [[ -n "$ids" ]] || {
      echo "media-bluegreen: active ${active} is not ready; refusing" >&2
      return 1
    }
    if [[ "$recover_control_plane" != 1 ]]; then
      media_bluegreen_probe_ready "$active" true "$(media_bluegreen_port "$active")" || {
        echo "media-bluegreen: active ${active} is not ready; refusing" >&2
        return 1
      }
      media_bluegreen_dependencies_ready || return 1
    fi
  fi
  printf '%s:%s\n' "${active:-none}" "$standby"
}

media_bluegreen_cutover_locked() {
  local active standby old_colour active_port standby_port tombstone old_generation status deadline running
  media_bluegreen_preflight >/dev/null || return 1
  if ! active="$(media_bluegreen_redis GET media:active_colour)"; then
    echo 'media-bluegreen: Redis promotion read failed; refusing cutover' >&2
    return 1
  fi
  if [[ -z "$active" ]]; then
    active=''
    standby=blue
    # A bootstrap with an old single-plane container has no safe automatic
    # color attribution; migration is an operator-controlled drain.
    local project legacy
    project="$(media_bluegreen_render | python3 -c 'import json,sys; print(json.load(sys.stdin)["name"])')" || return 1
    if ! legacy="$(docker ps --filter "label=com.docker.compose.project=${project}" \
      --filter 'label=com.docker.compose.service=media-plane' --format '{{.ID}}')"; then
      echo 'media-bluegreen: cannot inspect the legacy media service; refusing bootstrap' >&2
      return 1
    fi
    if [[ -n "$legacy" ]]; then
      echo 'media-bluegreen: legacy media-plane is still running; drain it before first color activation' >&2
      return 1
    fi
  elif [[ "$active" == blue ]]; then
    standby=green
  elif [[ "$active" == green ]]; then
    standby=blue
  else
    echo 'media-bluegreen: Redis names an unknown active color; refusing cutover' >&2
    return 1
  fi
  old_colour="$active"
  active_port=''
  if [[ -n "$active" ]]; then
    active_port="$(media_bluegreen_port "$active")" || return 1
    if ! media_bluegreen_probe_ready "$active" true "$active_port"; then
      echo "media-bluegreen: active ${active} does not satisfy the ready contract; refusing cutover" >&2
      return 1
    fi
    old_generation="$(curl -sf --max-time 3 "http://127.0.0.1:${active_port}/readyz" \
      | python3 -c 'import json,sys; v=json.load(sys.stdin); print(v["generation"])')" || {
      echo 'media-bluegreen: cannot read active generation before cutover' >&2
      return 1
    }
    [[ -n "$old_generation" && "$old_generation" != *[[:space:]]* ]] || {
      echo 'media-bluegreen: active generation is malformed; refusing cutover' >&2
      return 1
    }
  fi
  if ! tombstone="$(media_bluegreen_redis EXISTS "media:colour_draining:${standby}")" \
     || [[ "$tombstone" != 0 ]]; then
    echo "media-bluegreen: standby ${standby} has a drain tombstone or Redis is unavailable; run media-reap first" >&2
    return 1
  fi
  standby_port="$(media_bluegreen_port "$standby")" || return 1
  if ! running="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q "media-plane-${standby}")"; then
    echo "media-bluegreen: cannot inspect standby ${standby}; refusing cutover" >&2
    return 1
  fi
  if [[ -n "$running" ]]; then
    echo "media-bluegreen: verifying running empty standby ${standby} image provenance"
    media_bluegreen_verify_standby_image "$standby" "$running" || return 1
  else
    echo "media-bluegreen: starting standby ${standby}"
    (cd "$DEPLOY_DIR" && $COMPOSE_CMD up -d --build --no-deps --wait \
      --wait-timeout 180 "media-plane-${standby}") || return 1
  fi
  if ! media_bluegreen_probe_ready "$standby" true "$standby_port" 1 true; then
    echo "media-bluegreen: standby ${standby} failed its identity, worker, or readiness gate" >&2
    return 1
  fi

  if ! media_bluegreen_redis SADD media:colours:registered "$standby" >/dev/null; then
    echo 'media-bluegreen: registry write failed before promotion; active color is unchanged' >&2
    return 1
  fi
  if ! media_bluegreen_redis SET media:active_colour "$standby" >/dev/null; then
    # An unacknowledged SET can have committed; resolve its outcome before any
    # drain action, and refuse if Redis cannot prove the result.
    active="$(media_bluegreen_redis GET media:active_colour)" || {
      echo 'media-bluegreen: promotion outcome is unknown; refusing to arm a drain' >&2
      return 1
    }
    [[ "$active" == "$standby" ]] || {
      echo 'media-bluegreen: promotion was not confirmed; old color remains undrained' >&2
      return 1
    }
  fi
  # A successful SET reply confirms promotion. If its acknowledgement was
  # lost, the branch above reads back and confirms the committed value.
  if [[ -n "$active_port" ]]; then
    if ! media_bluegreen_redis SET "media:colour_draining:${old_colour}" "$old_generation" >/dev/null; then
      tombstone="$(media_bluegreen_redis GET "media:colour_draining:${old_colour}")" || return 1
      [[ "$tombstone" == "$old_generation" ]] || {
        echo 'media-bluegreen: drain tombstone was not confirmed; old color remains running' >&2
        return 1
      }
    fi
    deadline=$(( $(date +%s) + 90 ))
    while :; do
      status="$(curl -sS --max-time 3 -o /dev/null -w '%{http_code}' \
        "http://127.0.0.1:${active_port}/readyz" 2>/dev/null || true)"
      if media_bluegreen_probe_ready "$old_colour" false "$active_port"; then
        break
      fi
      if (( $(date +%s) >= deadline )); then
        echo "media-bluegreen: ${old_colour} did not acknowledge draining within 90 seconds; tombstone remains" >&2
        return 1
      fi
      sleep 2
    done
  fi
  echo "media-bluegreen: promoted ${standby}; prior color is serving existing rooms until reap"
}

media_bluegreen_sweep_markers() {
  local colour="$1" active="$2" result owner_keys live_keys marker_keys marker_key channel_id channel_ids owner_key live_key
  local -a scanned_channels=()
  result="$(media_bluegreen_redis SISMEMBER media:colours:registered "$active")" || return 1
  [[ "$result" == 1 ]] || {
    echo "media-bluegreen: active color ${active} is absent from the registry; refusing reap" >&2
    return 1
  }
  owner_keys="$(media_bluegreen_redis --scan --pattern 'voice:room_owner:*')" || return 1
  live_keys="$(media_bluegreen_redis --scan --pattern 'voice:room_live:*')" || return 1
  marker_keys="$(printf '%s\n%s' "$owner_keys" "$live_keys")"
  while IFS= read -r marker_key; do
    [[ -n "$marker_key" ]] || continue
    if [[ ! "$marker_key" =~ ^voice:room_(owner|live):[0-9a-fA-F-]{36}$ ]]; then
      echo 'media-bluegreen: marker-key scan returned an unexpected key; refusing before registry mutation' >&2
      return 1
    fi
    channel_id="${marker_key#voice:room_owner:}"
    channel_id="${channel_id#voice:room_live:}"
    scanned_channels+=("$channel_id")
  done <<<"$marker_keys"
  channel_ids="$(printf '%s\n' "${scanned_channels[@]}" | LC_ALL=C sort -u)" || return 1
  result="$(media_bluegreen_redis SREM media:colours:registered "$colour")" || return 1
  [[ "$result" =~ ^[01]$ ]] || return 1
  # Keep the target fenced after a partial sweep; the tombstone makes retry safe.
  while IFS= read -r channel_id; do
    [[ -n "$channel_id" ]] || continue
    owner_key="voice:room_owner:${channel_id}"
    live_key="voice:room_live:${channel_id}"
    result="$(media_bluegreen_redis EVAL \
      'local removed = 0; if redis.call("GET", KEYS[1]) == ARGV[1] then removed = removed + redis.call("DEL", KEYS[1]) end; local live = redis.call("GET", KEYS[2]); if live and string.sub(live, 1, string.len(ARGV[1]) + 1) == ARGV[1] .. "-" then removed = removed + redis.call("DEL", KEYS[2]) end; return removed' \
      2 "$owner_key" "$live_key" "$colour")" || return 1
    [[ "$result" =~ ^[012]$ ]] || return 1
  done <<<"$channel_ids"
}

media_bluegreen_reap_one() {
  local colour="$1" force="${2:-0}" active port body status rooms creates owner_key channel_id live_key result generation
  port="$(media_bluegreen_port "$colour")" || return 1
  active="$(media_bluegreen_redis GET media:active_colour)" || return 1
  [[ -n "$active" && "$active" =~ ^(blue|green)$ ]] || {
    echo 'media-bluegreen: active color is absent or invalid; refusing reap' >&2
    return 1
  }
  [[ "$active" != "$colour" ]] || {
    echo "media-bluegreen: refusing to reap active color ${colour}" >&2
    return 1
  }
  result="$(media_bluegreen_redis SISMEMBER media:colours:registered "$colour")" || return 1
  [[ "$result" =~ ^[01]$ ]] || return 1
  if [[ "$result" == 0 ]]; then
    result="$(media_bluegreen_redis EXISTS "media:colour_draining:${colour}")" || return 1
    [[ "$result" == 1 ]] || {
      echo "media-bluegreen: ${colour} is unregistered without a drain tombstone; refusing reap" >&2
      return 1
    }
    result="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --all --format json -- "media-plane-${colour}")" || return 1
    if python3 -c '
import json, sys
raw = sys.stdin.read().strip()
try:
    rows = json.loads(raw) if raw.startswith("[") else [json.loads(line) for line in raw.splitlines() if line.strip()]
    stopped = isinstance(rows, list) and all(isinstance(row, dict)
        and row.get("Service") == sys.argv[1] and row.get("State") in ("exited", "dead")
        for row in rows)
except (TypeError, ValueError):
    stopped = False
raise SystemExit(0 if stopped else 1)
' "media-plane-${colour}" <<<"$result"; then
      # R7 may have stopped/removed the container before losing its acknowledgement.
      # Repeat the idempotent marker sweep before removing the tombstone.
      media_bluegreen_sweep_markers "$colour" "$active" || return 1
      (cd "$DEPLOY_DIR" && $COMPOSE_CMD rm -f "media-plane-${colour}") || return 1
      result="$(media_bluegreen_redis DEL "media:colour_draining:${colour}")" || return 1
      [[ "$result" =~ ^[01]$ ]] || return 1
      echo "media-bluegreen: resumed reap of stopped color ${colour}"
      return 0
    fi
  fi
  body="$(mktemp)" || return 1
  status=0
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD exec -T "media-plane-${colour}" \
    node -e 'fetch("http://127.0.0.1:3000/readyz", {signal: AbortSignal.timeout(3000)}).then(async response => { process.stdout.write(await response.text()); process.exitCode = response.status === 503 ? 8 : 1; }).catch(() => { process.exitCode = 1; });' >"$body" 2>/dev/null) || status=$?
  # Require HTTP 503 as well as the matching drain body; a 200 liveness response is not R2.
  if [[ "$status" -ne 8 ]] || ! python3 -c '
import json, sys
try:
    value = json.load(open(sys.argv[1], encoding="utf-8"))
    generation = value.get("generation")
    generation_ok = (type(generation) is int or
                     (isinstance(generation, str) and generation and not any(c.isspace() for c in generation)))
    valid = (value.get("colour") == sys.argv[2]
             and generation_ok
             and value.get("accepting_new_rooms") is False
             and type(value.get("rooms")) is int and value["rooms"] >= 0
             and type(value.get("creates_in_flight")) is int and value["creates_in_flight"] >= 0
             and (type(value.get("generation")) is int or bool(value.get("generation"))))
    if not valid:
        raise SystemExit(1)
except (OSError, ValueError, TypeError):
    raise SystemExit(1)
' "$body" "$colour"; then
    rm -f "$body"
    echo "media-bluegreen: ${colour} readiness did not confirm a drained color" >&2
    return 1
  fi
  read -r rooms creates generation < <(python3 -c '
import json, sys
value=json.load(open(sys.argv[1], encoding="utf-8"))
print(value["rooms"], value["creates_in_flight"], value["generation"])
' "$body")
  rm -f "$body"
  echo "media-bluegreen: ${colour} reported generation ${generation} (diagnostic only; restart generations are not compared)"
  if (( rooms != 0 || creates != 0 )); then
    echo "media-bluegreen: ${colour} reports ${rooms} live rooms and ${creates} in-flight creates"
    if (( creates != 0 )); then
      echo 'media-bluegreen: in-flight creates must reach zero; --force cannot bypass this check' >&2
      return 2
    fi
    if (( force != 1 )); then
      echo 'media-bluegreen: live rooms remain; reap skipped (manual --force is available after the 2-hour drain window)' >&2
      return 2
    fi
    echo 'media-bluegreen: verify from deployment/operations records that this color has drained for at least 2 hours.' >&2
    local confirmation
    if ! read -r -p "Type REAP ${colour} WITH ${rooms} LIVE ROOMS to continue: " confirmation; then
      echo 'media-bluegreen: force confirmation was not received; color remains running' >&2
      return 2
    fi
    [[ "$confirmation" == "REAP ${colour} WITH ${rooms} LIVE ROOMS" ]] || {
      echo 'media-bluegreen: force confirmation did not match; color remains running' >&2
      return 2
    }
  fi
  # Validate the complete scan before the registry transition, then keep R4
  # adjacent to the owner-matched R5 sweep so placement cannot race it.
  media_bluegreen_sweep_markers "$colour" "$active" || return 1
  # Re-read the own-namespace readiness immediately before the adjacent stop/rm.
  body="$(mktemp)" || return 1
  status=0
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD exec -T "media-plane-${colour}" \
    node -e 'fetch("http://127.0.0.1:3000/readyz", {signal: AbortSignal.timeout(3000)}).then(async response => { process.stdout.write(await response.text()); process.exitCode = response.status === 503 ? 8 : 1; }).catch(() => { process.exitCode = 1; });' >"$body" 2>/dev/null) || status=$?
  if [[ "$status" -ne 8 ]] || ! python3 -c '
import json, sys
try:
    value=json.load(open(sys.argv[1], encoding="utf-8"))
    generation=value.get("generation")
    generation_ok=(type(generation) is int or
                   (isinstance(generation, str) and generation and not any(c.isspace() for c in generation)))
    ok=(value.get("colour")==sys.argv[2] and value.get("accepting_new_rooms") is False
        and generation_ok
        and type(value.get("rooms")) is int and value["rooms"]>=0
        and type(value.get("creates_in_flight")) is int and value["creates_in_flight"]>=0)
except (OSError, ValueError, TypeError):
    ok=False
raise SystemExit(0 if ok else 1)
' "$body" "$colour"; then
    rm -f "$body"
    echo "media-bluegreen: ${colour} changed after marker sweep; it remains running" >&2
    return 1
  fi
  read -r rooms creates < <(python3 -c '
import json, sys
value=json.load(open(sys.argv[1], encoding="utf-8"))
print(value["rooms"], value["creates_in_flight"])
' "$body")
  if (( rooms != 0 || creates != 0 )); then
    echo "media-bluegreen: ${colour} changed after marker sweep: ${rooms} live rooms, ${creates} in-flight creates" >&2
    if (( creates != 0 || force != 1 )); then
      rm -f "$body"
      echo "media-bluegreen: ${colour} remains running" >&2
      return 2
    fi
    echo 'media-bluegreen: re-confirm the 2-hour drain window from deployment/operations records.' >&2
    local confirmation
    if ! read -r -p "Type REAP ${colour} WITH ${rooms} LIVE ROOMS to continue: " confirmation; then
      rm -f "$body"
      echo "media-bluegreen: ${colour} remains running" >&2
      return 2
    fi
    if [[ "$confirmation" != "REAP ${colour} WITH ${rooms} LIVE ROOMS" ]]; then
      rm -f "$body"
      echo "media-bluegreen: ${colour} remains running" >&2
      return 2
    fi
  fi
  rm -f "$body"
  if ! (cd "$DEPLOY_DIR" && $COMPOSE_CMD stop --timeout 45 "media-plane-${colour}"); then
    echo "media-bluegreen: stop failed; ${colour} remains fenced; rerun media-reap to resume" >&2
    return 1
  fi
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD rm -f "media-plane-${colour}") || return 1
  result="$(media_bluegreen_redis DEL "media:colour_draining:${colour}")" || return 1
  [[ "$result" =~ ^[01]$ ]] || return 1
  echo "media-bluegreen: reaped drained color ${colour}"
}

media_bluegreen_reap() {
  local force="${1:-0}" keys key colour active rc=0 project colours
  if [[ "${MEDIA_DEPLOY_PROFILE:-media-single}" != media-bluegreen ]]; then
    project="$(media_bluegreen_render | python3 -c 'import json,sys; print(json.load(sys.stdin)["name"])')" || return 1
    colours="$(docker ps --filter "label=com.docker.compose.project=${project}" \
      --filter 'label=com.docker.compose.service=media-plane-blue' --format '{{.ID}}')" || return 1
    colours+="$(docker ps --filter "label=com.docker.compose.project=${project}" \
      --filter 'label=com.docker.compose.service=media-plane-green' --format '{{.ID}}')" || return 1
    [[ -z "$colours" ]] || {
      echo 'media-bluegreen: colour container exists while legacy profile is selected; refusing reap' >&2
      return 1
    }
    echo 'media-bluegreen: legacy profile selected; no colour to reap'
    return 0
  fi
  media_bluegreen_validate_model || return 1
  keys="$(media_bluegreen_redis --scan --pattern 'media:colour_draining:*')" || {
    echo 'media-bluegreen: cannot enumerate drain tombstones' >&2
    return 1
  }
  [[ -n "$keys" ]] || {
    rm -f "${CONFIG_ROOT}/logs/media-reap-alert.flag" 2>/dev/null || true
    echo 'media-bluegreen: no drained colors to reap'
    return 0
  }
  active="$(media_bluegreen_redis GET media:active_colour)" || return 1
  [[ "$active" =~ ^(blue|green)$ ]] || {
    echo 'media-bluegreen: active color is absent or invalid; refusing reap' >&2
    return 1
  }
  # A corrupt later tombstone must not allow an earlier colour to be reaped.
  while IFS= read -r key; do
    [[ "$key" =~ ^media:colour_draining:(blue|green)$ ]] || {
      echo 'media-bluegreen: unexpected drain tombstone key; refusing reap' >&2
      return 1
    }
    colour="${BASH_REMATCH[1]}"
    [[ "$colour" != "$active" ]] || {
      echo "media-bluegreen: refusing to reap active color ${colour}" >&2
      return 1
    }
  done <<<"$keys"
  while IFS= read -r key; do
    colour="${key#media:colour_draining:}"
    media_bluegreen_reap_one "$colour" "$force" || {
      _reap_rc=$?
      if [[ $_reap_rc -eq 2 ]]; then
        continue
      fi
      rc=1; break
    }
  done <<<"$keys"
  if (( rc == 0 )); then
    rm -f "${CONFIG_ROOT}/logs/media-reap-alert.flag" 2>/dev/null || true
  else
    mkdir -p "${CONFIG_ROOT}/logs" 2>/dev/null || true
    printf 'media reaper failed at %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
      >"${CONFIG_ROOT}/logs/media-reap-alert.flag" 2>/dev/null || true
    echo 'media-bluegreen: reap aborted; color remains running for operator inspection' >&2
  fi
  return "$rc"
}

# Reconcile before managed production image builds; immutable West staging and
# canonical staging must not pull or start production NATS.
managed_production_nats_reconcile() {
  [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == managed \
     && $STAGE_RELEASE_MODE -eq 0 \
     && "$COMPOSE_OVERRIDE" != docker-compose.staging.yml ]] || return 0
  local container host_config
  if ! (cd "$DEPLOY_DIR" && $COMPOSE_CMD config --format json 2>/dev/null) \
      | python3 -c 'import json, sys; sys.exit(bool(json.load(sys.stdin)["services"]["nats"].get("ports")))' 2>/dev/null; then
    echo 'rebuild: cannot prove production NATS has no published ports; refusing' >&2
    return 1
  fi
  # freshstart checks the model before wiping, then starts fresh NATS afterward.
  [[ "${1:-}" != model-only ]] || return 0
  if ! (cd "$DEPLOY_DIR" && $COMPOSE_CMD up -d --no-deps --no-build --pull missing --wait --wait-timeout 180 nats) >/dev/null 2>&1; then
    echo 'rebuild: cannot reconcile production NATS; refusing' >&2
    return 1
  fi
  container="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps -q nats 2>/dev/null)" || {
    echo 'rebuild: cannot select production NATS container; refusing' >&2
    return 1
  }
  if [[ ! "$container" =~ ^[[:xdigit:]]{12,64}$ ]]; then
    echo 'rebuild: production NATS container is missing or ambiguous; refusing' >&2
    return 1
  fi
  host_config="$(docker inspect --format '{{json .HostConfig.PortBindings}}|{{.HostConfig.PublishAllPorts}}' "$container" 2>/dev/null)" || {
    echo 'rebuild: cannot inspect production NATS port bindings; refusing' >&2
    return 1
  }
  if ! python3 -c 'import json, sys; ports, publish = sys.argv[1].rsplit("|", 1); sys.exit(publish != "false" or json.loads(ports) not in ({}, None))' "$host_config" 2>/dev/null; then
    echo 'rebuild: production NATS still publishes host ports; refusing' >&2
    return 1
  fi
}

# Build only explicit services. In particular, neither the active colour nor
# the unselected legacy service may be pulled in by an implicit Compose up.
media_bluegreen_rebuild_locked() {
  local snapshot active standby rendered plan deps rest scope_token ingress_ids start_cp=0 start_media=0 full=0
  local -a deps_arr=() rest_arr=()
  if [[ $# -eq 0 ]]; then
    start_cp=1
  else
    for scope_token in "$@"; do
      [[ "$scope_token" == control-plane ]] && start_cp=1
    done
  fi
  snapshot="$(media_bluegreen_preflight "$start_cp")" || return 1
  active="${snapshot%%:*}"
  standby="${snapshot#*:}"
  rendered="$(media_bluegreen_render)" || return 1
  if [[ $# -eq 0 ]]; then
    full=1; start_cp=1; start_media=1
  fi
  for scope_token in "$@"; do
    case "$scope_token" in
      control-plane) start_cp=1 ;;
      media-plane) start_media=1 ;;
      media-plane-blue|media-plane-green)
        if [[ "$scope_token" != "media-plane-${standby}" ]]; then
          echo "media-bluegreen: ${scope_token} is not the resolved standby; refusing" >&2
          return 1
        fi
        start_media=1 ;;
      ops-agent|coturn|postgres|redis|nats|minio) ;;
      *) echo "rebuild: invalid or unknown service '${scope_token}'" >&2; return 2 ;;
    esac
  done
  # A direct non-voice scope still resolves and fences the active colour, but
  # does not spuriously promote a new colour.
  if (( full == 1 )); then
    plan="$(python3 -c '
import json, sys
s = json.load(sys.stdin)["services"]
standby = "media-plane-" + sys.argv[1]
excluded = {"control-plane", "media-plane", standby, "media-plane-blue", "media-plane-green", "staging-media-proxy"}
closure, todo = set(), ["control-plane", standby]
while todo:
    for dep in s[todo.pop()].get("depends_on") or {}:
        if dep in s and dep not in excluded and dep not in closure:
            closure.add(dep)
            todo.append(dep)
print(" ".join(sorted(closure)))
print(" ".join(sorted(set(s) - closure - excluded)))
' "$standby" <<<"$rendered")" || {
      echo 'media-bluegreen: cannot resolve full service order; refusing' >&2
      return 1
    }
    { IFS= read -r deps || true; IFS= read -r rest || true; } <<<"$plan"
    read -r -a deps_arr <<<"$deps"
    read -r -a rest_arr <<<"$rest"
  fi
  # Refusal above is before the Activity History guard: that guard can write a
  # receipt, and a rejected cutover must leave the receipt and image pin intact.
  activity_history_routine_guard || return 1
  managed_production_nats_reconcile || return 1
  if (( full == 1 )) && [[ ${#deps_arr[@]} -gt 0 ]]; then
    (cd "$DEPLOY_DIR" && $COMPOSE_CMD up -d --build --no-deps --wait --wait-timeout 180 -- "${deps_arr[@]}") || return 1
  fi
  if (( start_cp == 1 )); then
    clear_activity_history_image_selector || return 1
    (cd "$DEPLOY_DIR" && $COMPOSE_CMD build control-plane) || return 1
    privacy_defaults_refuse_foreign_control_plane || return 1
    activity_history_stop_control_plane || return 1
    privacy_defaults_require_local_control_plane_drained || return 1
    (cd "$DEPLOY_DIR" && $COMPOSE_CMD up -d --no-build --no-deps --wait --wait-timeout 180 control-plane) || return 1
    voice_enforcement_rollout_wait_control_plane || return 1
  fi
  if (( start_media == 1 )); then
    if [[ "${COMPOSE_OVERRIDE:-}" == docker-compose.staging.yml ]]; then
      ingress_ids="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q staging-media-proxy)" || return 1
      if [[ -z "$ingress_ids" ]]; then
        (cd "$DEPLOY_DIR" && $COMPOSE_CMD up -d --no-deps --wait --wait-timeout 180 staging-media-proxy) || return 1
      fi
      media_bluegreen_dependencies_ready staging-media-proxy || return 1
    else
      media_bluegreen_dependencies_ready || return 1
    fi
    media_bluegreen_cutover_locked || return 1
    voice_enforcement_rollout_activate || return 1
  fi
  if (( full == 1 )) && [[ ${#rest_arr[@]} -gt 0 ]]; then
    (cd "$DEPLOY_DIR" && $COMPOSE_CMD up -d --build --no-deps -- "${rest_arr[@]}") || return 1
  elif (( full == 0 )); then
    local -a extra=()
    for scope_token in "$@"; do
      case "$scope_token" in
        control-plane|media-plane|media-plane-blue|media-plane-green) ;;
        *) extra+=("$scope_token") ;;
      esac
    done
    if [[ ${#extra[@]} -gt 0 ]]; then
      (cd "$DEPLOY_DIR" && $COMPOSE_CMD up -d --build --no-deps --wait --wait-timeout 180 -- "${extra[@]}") || return 1
    fi
  fi
  echo 'media-bluegreen: rebuild completed with active colour excluded'
}

# Unscoped rebuild (#3524). Every service, ordered so the paired voice handoff
# stays intact: (1) everything control-plane and media-plane depend on,
# transitively, health-gated; (2) the paired handoff, unchanged; (3) every
# remaining service. Each compose call names its services. A service-less `up`
# would recreate control-plane and media-plane in one unordered call, which is
# the handoff #3140 exists to prevent; and `--no-deps` is safe in stage 1
# because the list is transitively complete, so Compose cannot pull
# control-plane in early.
west_stage_volume_owned() {
  local name=$1 inspected
  inspected="$(docker volume inspect --format '{{json .}}' "$name" 2>/dev/null)" || return 1
  printf '%s' "$inspected" | python3 -c '
import json, sys
try:
    volume = json.load(sys.stdin)
    name = sys.argv[1]
    labels = volume.get("Labels") or {}
    if volume.get("Name") != name or volume.get("Driver") != "local":
        raise ValueError("volume identity")
    if labels.get("com.docker.compose.project") != "concord-west-stage" \
            or labels.get("com.docker.compose.volume") != name.removeprefix("concord-west-stage_") \
            or labels.get("com.concordvoice.environment") != "west-stage":
        raise ValueError("volume ownership")
except (AttributeError, TypeError, ValueError, json.JSONDecodeError):
    sys.exit(1)
' "$name"
}

west_stage_prepare_owned_stack() {
  (( STAGE_RELEASE_MODE )) || return 0

  local network_name='concord-west-stage_concordvoice-services'
  local network_json inspected service container volume known_volumes
  local -a services=(postgres redis nats minio coturn control-plane media-plane)
  local -a volumes=(postgres-data redis-data nats-data minio-data)

  known_volumes="$(docker volume ls --format '{{.Name}}')" || return 1
  for volume in "${volumes[@]}"; do
    if grep -Fxq "concord-west-stage_$volume" <<<"$known_volumes" \
       && ! west_stage_volume_owned "concord-west-stage_$volume"; then
      echo "west-stage: foreign stage volume exists; refusing Compose create" >&2
      return 1
    fi
  done

  # Create only stopped containers after the lifecycle/admission gates and
  # image build. This materializes Compose's actual network and container
  # configuration without starting, replacing, or removing running resources.
  if ! (cd "$DEPLOY_DIR" && $COMPOSE_CMD create --no-build --no-recreate); then
    echo 'west-stage: could not create the stopped stage resource set; refusing to start services' >&2
    return 1
  fi

  for volume in "${volumes[@]}"; do
    if ! west_stage_volume_owned "concord-west-stage_$volume"; then
      echo "west-stage: stage volume ownership is invalid; refusing to start services" >&2
      return 1
    fi
  done

  if ! network_json="$(docker network inspect --format '{{json .}}' "$network_name" 2>/dev/null)"; then
    echo 'west-stage: cannot inspect the stage bridge network; refusing to start services' >&2
    return 1
  fi
  if ! printf '%s' "$network_json" | python3 -c '
import ipaddress, json, sys
try:
    network = json.load(sys.stdin)
    labels = network.get("Labels") or {}
    options = network.get("Options") or {}
    containers = network.get("Containers") or {}
    expected = {
        "Name": "concord-west-stage_concordvoice-services",
        "Driver": "bridge",
    }
    if any(network.get(key) != value for key, value in expected.items()):
        raise ValueError("network identity")
    if labels.get("com.docker.compose.project") != "concord-west-stage":
        raise ValueError("project label")
    if labels.get("com.docker.compose.network") != "concordvoice-services":
        raise ValueError("network label")
    if options.get("com.docker.network.bridge.name") != "cvstage0":
        raise ValueError("bridge option")
    alias = ipaddress.ip_address("10.246.6.1")
    subnets = (network.get("IPAM") or {}).get("Config") or []
    if not subnets or any(alias in ipaddress.ip_network(item["Subnet"], strict=False)
                          for item in subnets):
        raise ValueError("stage alias overlaps bridge subnet")
    allowed = {"concordvoice-" + name for name in (
        "postgres", "redis", "nats", "minio", "coturn", "control-plane", "media-plane"
    )}
    if not isinstance(containers, dict):
        raise ValueError("network membership")
    for member in containers.values():
        name = str(member.get("Name", "")).lstrip("/")
        if name not in allowed:
            raise ValueError("foreign network member")
except (AttributeError, TypeError, ValueError, json.JSONDecodeError):
    sys.exit(1)
'; then
    echo 'west-stage: stage bridge identity or membership is not exclusively owned; refusing to start services' >&2
    return 1
  fi

  if ! ip link show cvstage0 >/dev/null 2>&1; then
    echo 'west-stage: expected cvstage0 Linux bridge is absent; refusing to start services' >&2
    return 1
  fi

  for service in "${services[@]}"; do
    container="concordvoice-${service}"
    if ! inspected="$(docker inspect --format '{{json .}}' "$container" 2>/dev/null)"; then
      echo "west-stage: cannot inspect the admitted ${service} container; refusing to start services" >&2
      return 1
    fi
    if ! printf '%s' "$inspected" | python3 -c '
import json, sys
try:
    container = json.load(sys.stdin)
    name = str(container.get("Name", "")).lstrip("/")
    service = sys.argv[1]
    labels = (container.get("Config") or {}).get("Labels") or {}
    networks = ((container.get("NetworkSettings") or {}).get("Networks") or {})
    if name != "concordvoice-" + service:
        raise ValueError("container name")
    if labels.get("com.docker.compose.project") != "concord-west-stage":
        raise ValueError("project label")
    if labels.get("com.docker.compose.service") != service:
        raise ValueError("service label")
    if set(networks) != {"concord-west-stage_concordvoice-services"}:
        raise ValueError("network membership")
except (AttributeError, TypeError, ValueError, json.JSONDecodeError):
    sys.exit(1)
' "$service"; then
      echo "west-stage: ${service} container ownership or network membership is invalid; refusing to start services" >&2
      return 1
    fi
  done
}

rebuild_unscoped() {
  local rendered plan closure rest
  local -a closure_arr=() rest_arr=()
  local -a build_flags=(--build)
  [[ ${STAGE_RESTORE_MODE:-0} -eq 0 ]] || build_flags=(--no-build --pull never)
  # stderr is NOT merged: Compose warns on stderr while exiting 0, and a merged
  # warning would corrupt the JSON (same rule as the scoped dependency probe).
  if ! rendered="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD config --format json)"; then
    echo "rebuild: cannot render the Compose configuration; refusing to guess which services to start." >&2
    return 1
  fi
  if ! plan="$(python3 -c '
import json, sys
svcs = json.load(sys.stdin)["services"]
voice = ("control-plane", "media-plane")
closure, todo = set(), [v for v in voice if v in svcs]
while todo:
    # depends_on is a dict or a list; iterating either yields the names.
    for d in svcs[todo.pop()].get("depends_on") or {}:
        # Only rendered services: Compose keeps an optional (required: false)
        # dependency on an inactive-profile service, and naming it enables it.
        if d in svcs and d not in voice and d not in closure:
            closure.add(d)
            todo.append(d)
print(" ".join(sorted(closure)))
print(" ".join(sorted(set(svcs) - closure - set(voice))))
' <<<"$rendered")"; then
    echo "rebuild: cannot resolve the service order from the Compose configuration." >&2
    return 1
  fi
  # `|| true`: command substitution strips the trailing newline, so an EMPTY
  # second line leaves `read` at EOF, which returns 1 and would abort under
  # `set -e` — an empty remainder is a legitimate plan, not an error.
  { IFS= read -r closure || true; IFS= read -r rest || true; } <<<"$plan"
  read -r -a closure_arr <<<"$closure"
  read -r -a rest_arr <<<"$rest"

  # An empty list SKIPS its stage. "${arr[@]}" of an empty array would expand to
  # a bare `up`, which recreates the whole project. `--` for the same reason: a
  # service name may start with `-`, and Compose would read it as a flag.
  if [[ ${#closure_arr[@]} -gt 0 ]]; then
    echo "rebuild: starting the voice services' dependencies: ${closure_arr[*]}"
    (cd "$DEPLOY_DIR" && $COMPOSE_CMD up -d "${build_flags[@]}" --no-deps --wait --wait-timeout 180 -- \
      "${closure_arr[@]}")
  fi
  voice_enforcement_rollout_deploy
  if [[ ${#rest_arr[@]} -gt 0 ]]; then
    # No --wait: coturn's STUN probe flaking would abort a deploy that
    # otherwise succeeded, and post-rsync's wait-healthy follows anyway.
    echo "rebuild: starting the remaining services: ${rest_arr[*]}"
    (cd "$DEPLOY_DIR" && $COMPOSE_CMD up -d "${build_flags[@]}" --no-deps -- "${rest_arr[@]}")
  fi
}

# nginx-reload and sudoers-sync both use fixed root-owned candidate paths.
# Serialize them on one host-global inode so a copied or symlinked controller
# cannot overlap post-rsync (or another direct invocation). Provisioning creates
# this file inside a root-owned directory, owned by concord but not unlinkable by
# concord. Keeping that inode is load-bearing: kernel lock ownership, not file
# existence, controls admission.
readonly DEPLOY_TRANSACTION_LOCK_PATH="/var/lib/concord/deploy-transaction.lock"
# Non-blocking by default; the deploy verbs raise it just before acquiring.
DEPLOY_LOCK_WAIT_SECS=0
readonly LOCKF_REENTRY_MARKER="--concord-ctl-lockf-reentry-v1"
CONCORD_CTL_LOCKF_REENTRY_INTERNAL=0

validate_lockf_reentry_parent() {
  local probe_rc=0

  # The outer wrapper alone opens status descriptor 8. It also holds the fixed
  # pathname through lockf, so an immediate second non-blocking acquisition
  # must report EX_TEMPFAIL. This avoids relying on ps(1), which is unavailable
  # in several hardened CI/macOS sandboxes.
  [[ -e /dev/fd/8 ]] || return 1
  /usr/bin/lockf -s -k -t 0 "$DEPLOY_TRANSACTION_LOCK_PATH" /usr/bin/true \
    || probe_rc=$?
  [[ "$probe_rc" -eq 75 ]]
}

# Normalize the guarded macOS re-entry before ordinary command dispatch. This
# is an argv marker rather than a caller-controlled environment bypass, and its
# lockf ancestry is revalidated before it is honored.
if [[ "${1:-}" == "$LOCKF_REENTRY_MARKER" ]]; then
  if ! validate_lockf_reentry_parent; then
    echo "  ✗ rejected invalid internal deploy-lock re-entry" >&2
    exit 1
  fi
  CONCORD_CTL_LOCKF_REENTRY_INTERNAL=1
  shift
fi

acquire_deploy_transaction_lock() {
  local operation="$1" lock_rc=0 lock_backend=""

  # SELF-HOSTED HOSTS HAVE NO LOCK, AND MUST NOT BE REFUSED FOR IT.
  # /var/lib/concord is created by provision-production.sh's
  # install_sudoers_boundary -- a MANAGED-host step. A self-hosted operator runs
  # install-selfhost.sh instead and never has that directory, so requiring the
  # lock here would break `concord-selfhost up|stop|down|freshstart` outright.
  #
  # Skipping is correct rather than merely tolerable: the same provisioning step
  # that creates this directory is the one that installs the #3107 healthwatch
  # actor and its /var/lib/concord/healthwatch state. No directory means no
  # actor, which means no contender -- the interlock is protecting against
  # something that cannot exist on that host.
  #
  # nginx-reload and sudoers-sync are managed-only paths that require the lock,
  # even though they now wait for a contending healthwatch cycle.
  # The skip is ANNOUNCED, never silent. A deploy that quietly runs without the
  # interlock it was told it has is the worse failure of the two: it looks
  # identical to one that took the lock. Self-hosted hosts are single-operator
  # and have no healthwatch actor today, so the exposure is nil -- but that is a
  # property of TODAY'S self-host topology, not a guarantee, and the notice is
  # what makes it re-checkable when that changes.
  if [[ "$operation" != nginx-reload && "$operation" != sudoers-sync ]] \
     && (( ${DEPLOY_LOCK_WAIT_SECS:-0} > 0 )) \
     && [[ ! -d "$(dirname "$DEPLOY_TRANSACTION_LOCK_PATH")" ]]; then
    echo "  ℹ ${operation}: no $(dirname "$DEPLOY_TRANSACTION_LOCK_PATH") — running WITHOUT the deploy transaction lock." >&2
    echo "    Expected on a self-hosted host: that directory and the #3107 healthwatch actor are both" >&2
    echo "    created by provision-production.sh, which self-hosted installs never run. No actor means" >&2
    echo "    no contender. A self-hosted interlock of its own is tracked at #3161 (epic #1615)." >&2
    return 0
  fi
  # Bounded wait in seconds, taken from DEPLOY_LOCK_WAIT_SECS rather than a
  # positional argument: callers invoke this as `"$@"`, and the lockf backend
  # re-executes the controller with that same argv, so a trailing wait argument
  # would both be misread ($2 is the VERB's first argument) and corrupt the
  # re-exec. Defaulted to 0 at declaration so an inherited environment value
  # cannot silently make a deploy block.
  local wait_secs="${DEPLOY_LOCK_WAIT_SECS:-0}"
  [[ "$wait_secs" =~ ^[0-9]+$ ]] || wait_secs=0
  local lock_path="$DEPLOY_TRANSACTION_LOCK_PATH"
  local status_path="" child_status=""

  if (( CONCORD_CTL_LOCKF_REENTRY_INTERNAL )); then
    return 0
  fi

  if [[ -x /usr/bin/flock ]]; then
    lock_backend=flock
    if ! exec 9>"$lock_path"; then
      echo "  ✗ ${operation}: cannot open deploy transaction lock at $lock_path" >&2
      exit 1
    fi
    if (( wait_secs > 0 )); then
      /usr/bin/flock -w "$wait_secs" 9 || lock_rc=$?
    else
      /usr/bin/flock -n 9 || lock_rc=$?
    fi
  elif [[ -x /usr/bin/lockf ]]; then
    lock_backend=lockf
    # BSD/macOS lockf portably locks a pathname while it runs a command. Let
    # that parent retain the lock while a guarded child runs the controller.
    # -k retains the inode. The wrapper records the controller status on an
    # inherited descriptor and exits zero, so a real child exit 75 cannot be
    # confused with lockf's EX_TEMPFAIL contention status.
    status_path="$(/usr/bin/mktemp /tmp/concord-ctl-lock-status.XXXXXX)" || {
      echo "  ✗ ${operation}: cannot create lock status channel" >&2
      exit 1
    }
    /bin/chmod 0600 "$status_path"
    exec 8>"$status_path"
    local -a lockf_child=("$0" "$LOCKF_REENTRY_MARKER" "$@")
    if [[ "${ACTIVITY_HISTORY_CONTRACT_MODE:-managed}" == selfhost ]]; then
      # The resolver scrubbed these exported controls after validating the
      # wrapper attestation. Restore only that attestation for the fresh child;
      # it repeats root/model admission and validates the lock parent itself.
      lockf_child=(/usr/bin/env
        "COMPOSE_OVERRIDE=$ACTIVITY_HISTORY_REQUESTED_COMPOSE_OVERRIDE"
        "COMPOSE_PROFILES=$ACTIVITY_HISTORY_REQUESTED_COMPOSE_PROFILES"
        "${lockf_child[@]}")
    fi
    /usr/bin/lockf -s -k -t 0 "$lock_path" /bin/bash -c '
      "$@"
      child_rc=$?
      printf "%s\n" "$child_rc" >&8 || exit 125
      exit 0
    ' _ "${lockf_child[@]}" || lock_rc=$?
    exec 8>&-

    if [[ -s "$status_path" ]]; then
      IFS= read -r child_status < "$status_path" || child_status=""
      /bin/rm -f -- "$status_path"
      if [[ ! "$child_status" =~ ^([0-9]|[1-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-5])$ ]]; then
        echo "  ✗ ${operation}: invalid child status from deploy lock" >&2
        exit 1
      fi
      exit "$child_status"
    fi
    /bin/rm -f -- "$status_path"
    if (( lock_rc == 0 )); then
      lock_rc=125
    fi
  else
    echo "  ✗ ${operation}: no supported deploy lock utility found" >&2
    echo "    Expected /usr/bin/flock (Linux) or /usr/bin/lockf (macOS)." >&2
    exit 1
  fi

  if (( lock_rc != 0 )); then
    if [[ ( "$lock_backend" == flock && "$lock_rc" -eq 1 ) \
       || ( "$lock_backend" == lockf && "$lock_rc" -eq 75 ) ]]; then
      echo "  ✗ ${operation}: deploy transaction lock is busy at $lock_path" >&2
      echo "    Another deploy, nginx-reload, sudoers-sync, or the healthwatch actor" >&2
      echo "    (#3107) holds it; no changes were made." >&2
    else
      echo "  ✗ ${operation}: failed to acquire deploy transaction lock at $lock_path (rc=$lock_rc)" >&2
    fi
    exit 1
  fi
}

# Stale bundled storage is separate from the exposure-only port gate. Refuse
# before rollout, receipt/checkpoint changes, or any service mutation.
if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]]; then
  case "${1:-help}" in
    rebuild|restart)
      selfhost_storage_target_guard "${@:2}" || exit 1
      selfhost_storage_start_guard || exit 1
      ;;
    activity-history-activate|activity-history-rollback|activity-history-routine-guard|voice-enforcement-rollout|wait-healthy)
      selfhost_storage_start_guard || exit 1
      ;;
    freshstart)
      if [[ "$SELFHOST_STORAGE_MODE_RESOLVED" == byo-s3 ]]; then
        echo 'selfhost-storage: BYO freshstart refused; external storage has no volume-wipe lifecycle' >&2
        exit 1
      fi
      ;;
  esac
fi

if [[ "$MEDIA_DEPLOY_PROFILE" == media-bluegreen ]]; then
  case "${1:-help}" in
    freshstart)
      if [[ $# -ne 2 || "$2" != --allow-active-colour-recreate ]]; then
        echo 'freshstart: bluegreen active colour requires --allow-active-colour-recreate' >&2
        exit 2
      fi
      echo 'freshstart: bluegreen volume wipe requires an explicit single-colour bootstrap procedure; refusing service-less up' >&2
      exit 1
      ;;
    restart)
      if [[ $# -ne 2 || "$2" != --allow-active-colour-recreate ]]; then
        echo 'restart: bluegreen active colour requires --allow-active-colour-recreate' >&2
        exit 2
      fi
      ;;
  esac
fi

case "${1:-help}" in
  rebuild|stage-restore|restart|stop|activity-history-activate|activity-history-rollback|voice-enforcement-rollout|media-reap|nginx-reload|sudoers-sync)
    # The two activity-history verbs belong here for the SAME reason the deploy
    # verbs do, and leaving them out was an omission rather than a decision: both
    # stop and start the control plane, and both then gate on health and image
    # checks afterwards. An actor cycle holding pre-transition unhealthy evidence
    # can sample the freshly started container and restart it mid-gate, failing a
    # guarded activation or rollback for a reason nothing in their output would
    # explain.
    #
    # Bounded wait, NOT fail-fast. The healthwatch actor (#3107) takes this same
    # inode with `flock -n` and yields immediately on contention, so its hold is
    # bounded by one cycle -- but that cycle can include a control-plane restart
    # carrying a 45s stop_grace_period. Failing the deploy there would surface as
    # an unexplained "lock is busy" on a few percent of deploys and read as CI
    # flake. 90s comfortably exceeds the actor's worst-case hold.
    # The early post-rsync sudoers and nginx transactions need the same wait:
    # a healthy actor cycle must not fail Main CD before the rebuild begins.
    DEPLOY_LOCK_WAIT_SECS=90
    acquire_deploy_transaction_lock "$@"
    ;;
esac

case "${1:-help}" in
  validate-config|rebuild|stage-restore|restart|freshstart|activity-history-activate|activity-history-rollback)
    media_shared_listeners_routine_guard || exit 1
    ;;
esac

case "${1:-help}" in
  status)
    _storage_status_rc=0
    selfhost_storage_start_guard || _storage_status_rc=1
    echo "=== Concord Service Status ==="
    # NOTE: Branch + commit info no longer surfaced — server has no .git/ post-#848.
    # See `gh api repos/Concord-Voice/Concord-Voice-Alpha/deployments` for deploy history.
    if [[ -f "${CONFIG_ROOT}/.deploy-meta" ]]; then
      echo "Last deploy:"
      cat "${CONFIG_ROOT}/.deploy-meta" | sed 's/^/  /'
    else
      echo "Last deploy: (no metadata file — first deploy not yet completed)"
    fi
    echo ""
    cd "$DEPLOY_DIR" && $COMPOSE_CMD ps
    echo ""
    echo "=== Health Checks ==="
    VM_IP="${VM_IP:-localhost}"
    _media_status_rc=0
    _media_status_port=3000
    if [[ "$MEDIA_DEPLOY_PROFILE" == media-bluegreen ]]; then
      if _active_colour="$(media_bluegreen_redis GET media:active_colour)"; then
        _media_status_port="$(media_bluegreen_port "$_active_colour")" || _media_status_port=''
      else
        _media_status_port=''
      fi
    fi
    for svc in "Control Plane:8080" "Media Plane:${_media_status_port}"; do
      name="${svc%%:*}"
      port="${svc##*:}"
      if [[ -z "$port" ]]; then
        echo "  ✗ ${name} (active color unavailable)"
        _media_status_rc=1
      elif curl -sf "http://127.0.0.1:${port}/health" > /dev/null 2>&1; then
        if [[ "$MEDIA_DEPLOY_PROFILE" == media-bluegreen && "$name" == 'Media Plane' ]]; then
          echo "  ✓ ${name} (http://127.0.0.1:${port})"
        else
          echo "  ✓ ${name} (http://${VM_IP}:${port})"
        fi
      else
        echo "  ✗ ${name} (port ${port})"
        [[ "$MEDIA_DEPLOY_PROFILE" == media-bluegreen && "$name" == 'Media Plane' ]] && _media_status_rc=1
      fi
    done
    # coturn check — verify STUN port is listening
    if nc -z -w2 localhost 3478 2>/dev/null; then
      echo "  ✓ coturn TCP liveness (${VM_IP}:3478; not TLS proof)"
    else
      echo "  ✗ coturn TCP liveness (port 3478; not TLS proof)"
    fi
    _tls_status_rc=0
    if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]]; then
      if selfhost_tls_ready_gate; then
        echo '  ✓ coturn TLS listener serves the selected certificate'
      else
        echo '  ✗ coturn TLS readiness failed (see fixed selfhost-tls reason above)' >&2
        _tls_status_rc=1
      fi
    fi
    # Release sync status
    echo ""
    echo "=== Release Sync ==="
    if [[ -f "${CONFIG_ROOT}/releases/.tag" ]]; then
      echo "  Cached: $(cat "${CONFIG_ROOT}/releases/.tag") ($(ls "${CONFIG_ROOT}/releases/" 2>/dev/null | wc -l) files)"
    else
      echo "  No releases synced"
    fi
    [[ $_storage_status_rc -eq 0 ]] || exit 1
    [[ $_tls_status_rc -eq 0 ]] || exit 1
    [[ $_media_status_rc -eq 0 ]] || exit 1
    ;;

  logs)
    shift || true
    cd "$DEPLOY_DIR" && $COMPOSE_CMD logs -f --tail=100 "$@"
    ;;

  media-bluegreen-preflight)
    [[ $# -eq 1 ]] || { echo 'Usage: concord-ctl.sh media-bluegreen-preflight' >&2; exit 2; }
    if [[ "${MEDIA_DEPLOY_PROFILE:-media-single}" == media-bluegreen ]]; then
      media_bluegreen_preflight >/dev/null
      echo bluegreen
    fi
    ;;

  restart)
    # #1015: sync_releases no longer auto-called. The sync-releases.yml
    # workflow is the canonical writer for /opt/concord/releases/.
    # If you need to refresh release assets, trigger the workflow OR
    # invoke `concord-ctl.sh sync-releases` (manual recovery).
    if [[ "${MEDIA_DEPLOY_PROFILE:-media-single}" == media-bluegreen ]]; then
      if [[ $# -ne 2 || "$2" != --allow-active-colour-recreate ]]; then
        echo 'restart: bluegreen active colour requires --allow-active-colour-recreate' >&2
        exit 2
      fi
      media_bluegreen_preflight >/dev/null || exit 1
    fi
    echo "Restarting services..."
    if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == managed ]]; then
      echo "Note: release assets NOT refreshed automatically (#1015). Run 'concord-ctl.sh sync-releases' or trigger sync-releases.yml for a refresh."
    fi
    selfhost_tls_admit_gate || exit 1
    cd "$DEPLOY_DIR" && $COMPOSE_CMD restart
    selfhost_post_up_check || exit 1
    selfhost_tls_ready_gate || exit 1
    echo "Done."
    ;;

  rebuild|stage-restore)
    STAGE_RESTORE_MODE=0
    if [[ $1 == stage-restore ]]; then
      [[ $EUID -eq 0 && $STAGE_RELEASE_MODE -eq 1 && $# -eq 1 ]] || {
        echo 'stage-restore: root and the admitted West stage release are required' >&2
        exit 2
      }
      STAGE_RESTORE_MODE=1
    fi
    shift
    # Optional service scope (#2178). `rebuild` with NO arguments rebuilds every
    # service (#3524): the voice services' dependencies, then the paired voice
    # handoff, then the rest. Every out-of-band caller (operator SSH,
    # provision-production.sh, concord-selfhost up) therefore keeps working.
    #
    # Scope tokens originate from a `git diff` on the CI runner and cross an SSH
    # env boundary before landing here, then get interpolated into a `docker
    # compose` invocation. Validate and REJECT — never sanitize.
    REBUILD_SCOPE=()
    for _svc in "$@"; do
      if [[ ! "$_svc" =~ ^[a-z][a-z0-9-]*$ ]]; then
        echo "rebuild: invalid service name '$_svc'" >&2
        exit 2
      fi
      case "$_svc" in
        control-plane|media-plane|ops-agent|coturn|postgres|redis|nats|minio) ;;
        media-plane-blue|media-plane-green)
          [[ "${MEDIA_DEPLOY_PROFILE:-media-single}" == media-bluegreen ]] || {
            echo "rebuild: unknown compose service '$_svc'" >&2
            exit 2
          } ;;
        *)
          echo "rebuild: unknown compose service '$_svc'" >&2
          exit 2
          ;;
      esac
      REBUILD_SCOPE+=("$_svc")
    done
    if [[ "${MEDIA_DEPLOY_PROFILE:-media-single}" == media-bluegreen ]]; then
      media_bluegreen_rebuild_locked "${REBUILD_SCOPE[@]}"
      exit $?
    fi
    # The ordinary production selector must never accidentally render colours.
    _bluegreen_model_rc=0
    media_bluegreen_model_present || _bluegreen_model_rc=$?
    if [[ $_bluegreen_model_rc -eq 0 ]]; then
      echo 'rebuild: room placement or blue/green services detected; refusing the legacy rebuild path before image-pin or container changes' >&2
      exit 1
    fi
    if [[ $_bluegreen_model_rc -ne 1 ]]; then
      echo 'rebuild: cannot determine whether the Compose model uses blue/green services; refusing before image-pin or container changes' >&2
      exit 1
    fi

    # The image-selector clear below IS scope-gated (#2178). It rm's the
    # control-plane rollback image pin and its own echo asserts "rebuilding the
    # current source" — an assertion a media-only scope does not honor. Clearing
    # it unscoped would delete an operator's active pin while leaving the running
    # control-plane on the pinned image: a silent divergence that surfaces later
    # as an unexplained control-plane jump.
    #
    # Membership is tested in pure bash, NOT `printf ... | grep -qx`. Under
    # `set -o pipefail` (line 4) `grep -q` exits on first match and closes the
    # read end, so a producer still writing takes SIGPIPE and the pipeline
    # reports 141 despite the match. The window is vanishingly small for a
    # builtin writing a few short tokens, but the failure DIRECTION is severe: a
    # false negative skips the clear while control-plane IS in scope, leaving
    # CONTROL_PLANE_IMAGE exported, so the rebuild deploys the pinned ROLLBACK
    # image instead of the new code — and then stamps .deploy-meta with the new
    # SHA, hiding the change from every future scope diff.
    _has_control_plane=0
    _has_media_plane=0
    _mixed_extra=()
    for _s in ${REBUILD_SCOPE[@]+"${REBUILD_SCOPE[@]}"}; do
      case "$_s" in
        control-plane) _has_control_plane=1 ;;
        media-plane) _has_media_plane=1 ;;
        *) _mixed_extra+=("$_s") ;;
      esac
    done
    # A media-plane replacement is the paired voice handoff, which rebuilds
    # control-plane and media-plane and nothing else (#3524). Any other service
    # in the same scope used to be dropped while the command exited 0. Refuse
    # before the guard and before any compose call.
    if [[ $_has_media_plane -eq 1 && ${#_mixed_extra[@]} -gt 0 ]]; then
      echo "rebuild: media-plane is rebuilt only through the paired voice handoff, which would skip: ${_mixed_extra[*]}" >&2
      echo "rebuild: run 'rebuild ${_mixed_extra[*]}' separately, or run 'rebuild' with no services." >&2
      exit 2
    fi

    # `--no-deps` skips dependency startup AND the `depends_on` conditions, so it
    # is only correct when the dependency containers are already up. Escalate to
    # a full rebuild otherwise; the fail-safe direction is always "rebuild more",
    # never "fail". Since #3524 a full rebuild starts the stopped dependency
    # first, so escalation restores what depends_on guaranteed rather than
    # routing around it.
    #
    # Checking only the scoped service is NOT sufficient (#2178 review, HIGH —
    # found independently by three reviewers). media-plane declares
    # `nats: service_healthy`, and that gate was doing real work: the initial
    # NATS connect is one-shot (services/media-plane/src/lib/nats.ts assigns
    # `this.nc` only inside the try, and nothing retries it), so a media-plane
    # that starts while NATS is down runs PERMANENTLY with all four
    # `voice.enforce.*` subscriptions silently absent — server mute/deafen,
    # mid-session RBAC revocation, temporary-SBAC revocation, and DM membership
    # removal all stop arriving. Voice audio keeps working, so a banned or
    # server-muted member keeps transmitting until they leave voluntarily.
    # `/health` answers 200 throughout, so neither `--wait` nor `wait-healthy`
    # can see it.
    #
    # Escalating on a down dependency restores what `depends_on` used to
    # guarantee. Dependencies are read from the rendered compose config rather
    # than hardcoded, so this cannot drift from the compose files.
    # ERROR HANDLING DIRECTION — read before editing. Every failure to DETERMINE
    # the dependency set must escalate to a full rebuild. Suppressing an error
    # here does NOT mean "check fewer things"; it means fewer chances for the
    # probe below to fire, so the scoped `--no-deps` path runs — which is exactly
    # the fail-open this block exists to close. This mirrors the same operation
    # done correctly for the Activity History contract ~750 lines above.
    _rebuild_scoped=0
    if [[ ${#REBUILD_SCOPE[@]} -gt 0 ]]; then
      _rebuild_scoped=1
      _scope_closure=()
      # stderr goes to a SEPARATE file, never merged into the JSON. `docker
      # compose` writes warnings to stderr while still exiting 0, so `2>&1`
      # would prefix the JSON with a routine warning, fail the parse, and
      # escalate to a full rebuild on EVERY deploy on that host — the scoping
      # would silently never engage, with nothing in the log to say why.
      # A good diagnostic and a parseable stream are different jobs.
      _stderr_file="$(mktemp)"
      if ! _rendered="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD config --format json 2>"$_stderr_file")"; then
        echo "rebuild: cannot render Compose configuration to resolve dependencies — escalating to a full rebuild." >&2
        [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]] || sed 's/^/         /' "$_stderr_file" >&2
        rm -f "$_stderr_file"
        _rebuild_scoped=0
      else
        rm -f "$_stderr_file"
        _dep_err="$(mktemp)"
        for _svc in "${REBUILD_SCOPE[@]}"; do
          _scope_closure+=("$_svc")
          # `depends_on` renders as an object (service -> {condition:...}) under
          # `config --format json`, or as a plain list in the short form. Emit
          # "<name> <condition>" pairs so the probe can require health where the
          # compose file requires health.
          if ! _deps="$(python3 -c '
import json, sys
svc = json.load(sys.stdin)["services"][sys.argv[1]]
dep = svc.get("depends_on") or {}
if isinstance(dep, dict):
    for name, spec in dep.items():
        cond = (spec or {}).get("condition", "service_started") if isinstance(spec, dict) else "service_started"
        print(name, cond)
else:
    for name in dep:
        print(name, "service_started")
' "$_svc" <<<"$_rendered" 2>"$_dep_err")"; then
            echo "rebuild: cannot resolve depends_on for '$_svc' — escalating to a full rebuild." >&2
            [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]] || sed 's/^/         /' "$_dep_err" >&2
            rm -f "$_dep_err"
            _rebuild_scoped=0
            break
          fi
          rm -f "$_dep_err"
          while IFS= read -r _dep_line; do
            [[ -n "$_dep_line" ]] && _scope_closure+=("$_dep_line")
          done <<<"$_deps"
        done
      fi
    fi

    # Probe every member of the closure. A dependency declared `service_healthy`
    # must be HEALTHY, not merely running — `ps --status running` carries no
    # health axis, and a running-but-unhealthy NATS would otherwise pass while
    # media-plane came up against a broken broker.
    if [[ $_rebuild_scoped -eq 1 ]]; then
      for _entry in "${_scope_closure[@]}"; do
        _svc="${_entry%% *}"
        _cond="${_entry#* }"
        [[ "$_cond" == "$_svc" ]] && _cond="service_started"
        # stderr separated here too, and for a sharper reason than the JSON
        # cases: `_ids` is tested with `-z`, so a Compose warning merged into it
        # would make the result NON-EMPTY and the service would read as running
        # even when it is not — a fail-open in the guard's own liveness check.
        _probe_err="$(mktemp)"
        if ! _ids="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q -- "$_svc" 2>"$_probe_err")"; then
          echo "rebuild: cannot probe '$_svc' — escalating to a full rebuild." >&2
          [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]] || sed 's/^/         /' "$_probe_err" >&2
          rm -f "$_probe_err"
          _rebuild_scoped=0
          break
        fi
        rm -f "$_probe_err"
        if [[ -z "$_ids" ]]; then
          echo "rebuild: '$_svc' (scoped service or one of its dependencies) has no running container — escalating to a full rebuild." >&2
          _rebuild_scoped=0
          break
        fi
        if [[ "$_cond" == "service_healthy" ]]; then
          # Same stderr discipline as the config render above: a Compose warning
          # merged into this stream would fail the JSON parse and escalate every
          # deploy, making the scoping permanently inert.
          _health_err="$(mktemp)"
          if ! _health="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --format json -- "$_svc" 2>"$_health_err")"; then
            echo "rebuild: cannot read health for '$_svc' — escalating to a full rebuild." >&2
            [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]] || sed 's/^/         /' "$_health_err" >&2
            rm -f "$_health_err"
            _rebuild_scoped=0
            break
          fi
          # EVERY row must be healthy. Reading only rows[0] lets one healthy
          # container mask an unhealthy sibling if a dependency is ever scaled.
          if ! _state="$(python3 -c '
import json, sys
raw = sys.stdin.read().strip()
rows = json.loads(raw) if raw.startswith("[") else [
    json.loads(line) for line in raw.splitlines() if line.strip()
]
if not rows:
    print("")
else:
    bad = [r.get("Health", "") for r in rows if r.get("Health", "") != "healthy"]
    print(bad[0] if bad else "healthy")
' <<<"$_health" 2>"$_health_err")"; then
            echo "rebuild: cannot parse health for '$_svc' — escalating to a full rebuild." >&2
            [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]] || sed 's/^/         /' "$_health_err" >&2
            rm -f "$_health_err"
            _rebuild_scoped=0
            break
          fi
          rm -f "$_health_err"
          if [[ "$_state" != "healthy" ]]; then
            echo "rebuild: '$_svc' is declared service_healthy but reports '${_state:-unknown}' — escalating to a full rebuild." >&2
            _rebuild_scoped=0
            break
          fi
        fi
      done
    fi

    # Decide from the effective scope, after dependency discovery can escalate
    # to a full rebuild. Admission must precede the routine receipt/image guard.
    _rebuild_starts_coturn=0
    if [[ $_rebuild_scoped -eq 0 ]]; then
      _rebuild_starts_coturn=1
    else
      for _entry in "${_scope_closure[@]}"; do
        [[ "${_entry%% *}" == coturn ]] && _rebuild_starts_coturn=1
      done
      for _svc in "${REBUILD_SCOPE[@]}"; do
        [[ "$_svc" == coturn ]] && _rebuild_starts_coturn=1
      done
    fi
    if [[ $_rebuild_starts_coturn -eq 1 ]]; then
      selfhost_tls_admit_gate || exit 1
    fi

    # Unconditional, and deliberately BEFORE any scope branch: this is the
    # Activity History first-activation boundary. A scope-conditional guard here
    # would be a first-activation bypass. It takes an optional receipt_mode
    # argument, so the scope list must never reach it.
    activity_history_routine_guard
    managed_production_nats_reconcile || exit 1

    if (( STAGE_RELEASE_MODE )); then
      # Build every active stage image before creating the stopped resource
      # set. The ownership check then observes Compose's actual configuration
      # before any service can start; all ordinary managed/self-host paths keep
      # their established build ordering below.
      if [[ $STAGE_RESTORE_MODE -eq 0 ]] && ! (cd "$DEPLOY_DIR" && $COMPOSE_CMD build); then
        echo 'west-stage: stage image build failed; refusing to start services' >&2
        exit 1
      fi
      west_stage_prepare_owned_stack || exit 1
    fi

    # Unscoped — or escalated from a scoped call whose peers were not up — runs
    # every service in order (#3524). The image-selector clear happens inside
    # the paired handoff, immediately before the control-plane build.
    if [[ $_rebuild_scoped -eq 0 ]]; then
      rebuild_unscoped
      selfhost_post_up_check || exit 1
      [[ $_rebuild_starts_coturn -eq 0 ]] || selfhost_tls_ready_gate || exit 1
      echo "Done."
      exit 0
    fi

    # A scoped media-plane replacement is a protocol transition, not an
    # ordinary scoped rebuild. The mixed-scope refusal above guarantees nothing
    # but control-plane can be in this scope beside it.
    if [[ $_has_media_plane -eq 1 ]]; then
      if [[ $_has_control_plane -eq 1 ]]; then
        voice_enforcement_rollout_deploy
      else
        voice_enforcement_rollout_media_rebuild
      fi
      selfhost_post_up_check || exit 1
      [[ $_rebuild_starts_coturn -eq 0 ]] || selfhost_tls_ready_gate || exit 1
      echo "Done."
      exit 0
    fi

    if [[ $_has_control_plane -eq 1 ]]; then
      clear_activity_history_image_selector
    else
      echo "Keeping the Activity History rollback image pin: control-plane is not in this rebuild's scope."
    fi

    # #1015: sync_releases no longer auto-called (see restart case comment).
    echo "Rebuilding and restarting..."
    if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == managed ]]; then
      echo "Note: release assets NOT refreshed automatically (#1015). Run 'concord-ctl.sh sync-releases' or trigger sync-releases.yml for a refresh."
    fi
    # --wait-timeout is MANDATORY, not decorative: `--wait` installs a deadline
    # only when WaitTimeout > 0, so a bare --wait blocks forever and would hang
    # the deploy's SSH session with the stack in an indeterminate state.
    echo "Scope: ${REBUILD_SCOPE[*]} (--no-deps --wait)"
    if [[ $_has_control_plane -eq 1 ]]; then
      # Build while the previous control-plane is still serving. A slow or
      # failed image build must not extend the migration drain or leave it down.
      (cd "$DEPLOY_DIR" && $COMPOSE_CMD build "${REBUILD_SCOPE[@]}") || exit 1
      echo "rebuild: draining the previous control-plane before its startup migrations"
      privacy_defaults_refuse_foreign_control_plane || exit 1
      activity_history_stop_control_plane || exit 1
      privacy_defaults_require_local_control_plane_drained || exit 1
      cd "$DEPLOY_DIR" && $COMPOSE_CMD up -d --no-build --no-deps --wait --wait-timeout 180 "${REBUILD_SCOPE[@]}"
    else
      cd "$DEPLOY_DIR" && $COMPOSE_CMD up -d --build --no-deps --wait --wait-timeout 180 "${REBUILD_SCOPE[@]}"
    fi
    selfhost_post_up_check || exit 1
    [[ $_rebuild_starts_coturn -eq 0 ]] || selfhost_tls_ready_gate || exit 1
    echo "Done."
    ;;

  media-reap)
    shift
    _media_reap_force=0
    if [[ $# -eq 1 && "$1" == --force ]]; then
      _media_reap_force=1
    elif [[ $# -ne 0 ]]; then
      echo 'Usage: concord-ctl.sh media-reap [--force]' >&2
      exit 2
    fi
    media_bluegreen_reap "$_media_reap_force"
    ;;

  activity-history-routine-guard)
    shift
    if [[ $# -ne 0 ]]; then
      echo "Usage: concord-ctl.sh activity-history-routine-guard" >&2
      exit 2
    fi
    activity_history_routine_guard
    ;;

  activity-history-activate)
    shift
    activity_history_activate "$@"
    selfhost_post_up_check || exit 1
    ;;

  activity-history-rollback)
    shift
    activity_history_rollback "$@"
    selfhost_post_up_check || exit 1
    ;;

  voice-enforcement-rollout)
    shift
    case "${1:-}" in
      deploy)
        shift
        if [[ $# -ne 0 ]]; then
          echo 'Usage: concord-ctl.sh voice-enforcement-rollout deploy' >&2
          exit 2
        fi
        media_shared_listeners_routine_guard || exit 1
        managed_production_nats_reconcile || exit 1
        voice_enforcement_rollout_deploy "$@"
        selfhost_post_up_check || exit 1
        ;;
      *)
        echo "Usage: concord-ctl.sh voice-enforcement-rollout deploy" >&2
        exit 2
        ;;
    esac
    ;;

  selfhost-port-check)
    if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" != selfhost ]]; then
      echo "selfhost-port-check runs only in the self-host contract mode (use concord-selfhost health)" >&2
      exit 2
    fi
    selfhost_live_port_check || exit 1
    echo "selfhost-posture: live port bindings match the self-host allowlist"
    ;;

  media-shared-listeners-check)
    [[ $# -eq 1 ]] || { echo 'Usage: concord-ctl.sh media-shared-listeners-check < rendered-compose.json' >&2; exit 2; }
    media_shared_listeners_check_model
    ;;

  validate-config)
    # §F validation guard — invoke `docker compose config --quiet` to detect
    # missing env vars BEFORE attempting `up -d --build`. Exits non-zero with
    # operator-friendly message if interpolation fails.
    #
    # Spec ref: 2026-05-06-848-push-based-deploys-design.md §5.4
    if cd "$DEPLOY_DIR" && $COMPOSE_CMD config --quiet; then
      echo "  ✓ docker compose config validates"
    else
      echo "" >&2
      echo "  ✗ docker compose config failed — invalid YAML, missing compose file, or missing env var." >&2
      echo "    See docker output above for the specific failure." >&2
      echo "    If the error is a missing env var, re-run \`gh workflow run provision-secrets.yml\` to provision." >&2
      exit 1
    fi
    ;;

  attachment-probe)
    shift
    attachment_write_backend=legacy
    if [[ -f "${CONFIG_ROOT}/.env" ]]; then
      attachment_write_backend_count="$(grep -cE '^ATTACHMENT_WRITE_BACKEND=' "${CONFIG_ROOT}/.env" 2>/dev/null || true)"
      if [[ "$attachment_write_backend_count" == 1 ]]; then
        if ! attachment_write_backend="$(activity_history_read_root_value ATTACHMENT_WRITE_BACKEND)"; then
          echo "attachment-probe: could not read ATTACHMENT_WRITE_BACKEND from ${CONFIG_ROOT}/.env" >&2
          exit 1
        fi
        attachment_write_backend="${attachment_write_backend#"${attachment_write_backend%%[![:space:]]*}"}"
        attachment_write_backend="${attachment_write_backend%"${attachment_write_backend##*[![:space:]]}"}"
        attachment_write_backend="${attachment_write_backend:-legacy}"
      elif [[ "$attachment_write_backend_count" != 0 ]]; then
        echo "attachment-probe: ATTACHMENT_WRITE_BACKEND is duplicated in ${CONFIG_ROOT}/.env" >&2
        exit 1
      fi
    fi
    # With explicit arguments, storage-probe owns validation and backend selection.
    if [[ $# -eq 0 ]]; then
      if [[ "$attachment_write_backend" == "legacy" ]]; then
        echo "  ✓ attachment probe dormant: ATTACHMENT_WRITE_BACKEND=legacy"
        exit 0
      fi
      if [[ "$attachment_write_backend" != "legacy" && "$attachment_write_backend" != "r2-useast" ]]; then
        echo "attachment-probe: invalid ATTACHMENT_WRITE_BACKEND '$attachment_write_backend'" >&2
        exit 1
      fi
    fi
    cd "$DEPLOY_DIR" && $COMPOSE_CMD run --rm --no-deps --pull never control-plane ./main storage-probe "$@"
    ;;

  attachment-probe-cron-install)
    shift
    if [[ $# -ne 0 ]]; then
      echo "Usage: concord-ctl.sh attachment-probe-cron-install" >&2
      exit 2
    fi
    install_attachment_probe_cron
    ;;

  nginx-reload)
    # §5.9 (#919): render the source-of-truth nginx config with the validated
    # API HSTS policy, transactionally activate the fixed main/admin paths,
    # validate, reload/start, and probe the active public-edge policy.
    #
    # Sequence:
    #   a. Validate/decode all unprivileged input and render inactive files.
    #   b. Root-stage fixed candidates, hard-link prior inodes, atomically swap.
    #   c. nginx -t, graceful reload/start, and local /nginx-health HSTS probe.
    #   d. On any failure after swapping, restore both exact prior files and
    #      reload/start the old configuration when activation was attempted.
    #
    # Idempotency: re-running with no source change is a no-op at the
    # filesystem level (fixed candidates replace the same live paths) and at
    # the service level (systemctl reload SIGHUPs nginx workers).
    #
    # Spec ref: 2026-05-09-919-spa-deploy-trust-chain-design.md §5.9
    NGINX_STAGE=0
    NGINX_STAGE_BOOTSTRAP=0
    if [[ "$COMPOSE_OVERRIDE" == docker-compose.west-staging.yml ]]; then
      NGINX_STAGE=1
      if [[ $# -eq 2 && "$2" == --stage-bootstrap ]]; then
        NGINX_STAGE_BOOTSTRAP=1
      elif [[ $# -ne 1 ]]; then
        echo 'Usage: concord-ctl.sh nginx-reload [--stage-bootstrap]' >&2
        exit 2
      fi
    elif [[ $# -ne 1 ]]; then
      echo 'Usage: concord-ctl.sh nginx-reload' >&2
      exit 2
    fi
    if (( !NGINX_STAGE )) && [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" != selfhost ]]; then
      managed_core_identity_admit || exit 1
    fi
    if (( NGINX_STAGE )); then
      NGINX_SRC="${DEPLOY_DIR}/[internal]nginx/west-staging.conf.template"
    else
      NGINX_SRC="${DEPLOY_DIR}/[internal]nginx/concordvoice.conf"
    fi
    NGINX_RENDER_DIR="${CONFIG_DEPLOY_DIR}/nginx/.rendered"
    if (( NGINX_STAGE )); then
      NGINX_RENDER="${NGINX_RENDER_DIR}/west-staging.conf"
    else
      NGINX_RENDER="${NGINX_RENDER_DIR}/concordvoice.conf"
    fi
    NGINX_DST="/etc/nginx/sites-available/concordvoice"
    NGINX_CANDIDATE="/etc/nginx/sites-available/.concordvoice.candidate"
    NGINX_BACKUP="/etc/nginx/sites-available/.concordvoice.backup"
    ADMIN_TMPL="${DEPLOY_DIR}/[internal]nginx/admin-console.conf.template"
    ADMIN_RENDER="${NGINX_RENDER_DIR}/concord-admin.conf"
    ADMIN_DST="/etc/nginx/conf.d/concord-admin.conf"
    ADMIN_CANDIDATE="/etc/nginx/conf.d/.concord-admin.candidate"
    ADMIN_BACKUP="/etc/nginx/conf.d/.concord-admin.backup"
    ADMIN_ORIGIN_CERT="${ADMIN_ORIGIN_CERT:-/etc/nginx/cloudflare/origin.pem}"
    ADMIN_ORIGIN_KEY="${ADMIN_ORIGIN_KEY:-/etc/nginx/cloudflare/origin.key}"
    CONCORD_ENV_FILE="${CONCORD_ENV_FILE:-/opt/concord/.env}"
    HSTS_DEFAULT='max-age=63072000; includeSubDomains; preload'
    if [[ ! -f "$NGINX_SRC" ]]; then
      echo "" >&2
      echo "  ✗ nginx-reload: source-of-truth config not found at $NGINX_SRC" >&2
      echo "    Did the immutable release copy of [internal]nginx/ complete?" >&2
      exit 1
    fi

    render_west_stage_config() {
      local tmp cert_dir archive_dir cert key mode san actual expected public_key cert_key cert_path_prefix cert_path_suffix cert_link key_link cert_generation
      cert_dir=/etc/letsencrypt/live/staging.concordvoice.chat
      archive_dir=/etc/letsencrypt/archive/staging.concordvoice.chat
      cert_path_prefix=/etc/letsencrypt/live
      cert_path_suffix=/staging.concordvoice.chat
      cert="$cert_dir/fullchain.pem"
      key="$cert_dir/privkey.pem"
      if (( NGINX_STAGE_BOOTSTRAP )); then
        :
      else
        cert_link="$(readlink -- "$cert" 2>/dev/null)" || return 1
        key_link="$(readlink -- "$key" 2>/dev/null)" || return 1
        [[ -L "$cert" && -L "$key" \
           && "$cert_link" =~ ^\.\./\.\./archive/staging\.concordvoice\.chat/fullchain([1-9][0-9]*)\.pem$ ]] || {
          echo '  ✗ nginx-reload: stage certificate or key is missing/unsafe; refusing TLS render' >&2
          return 1
        }
        cert_generation="${BASH_REMATCH[1]}"
        [[ "$key_link" == "../../archive/staging.concordvoice.chat/privkey${cert_generation}.pem" \
           && -d "$cert_dir" && ! -L "$cert_dir" && -d "$archive_dir" && ! -L "$archive_dir" \
           && -f "$archive_dir/fullchain${cert_generation}.pem" \
           && ! -L "$archive_dir/fullchain${cert_generation}.pem" \
           && -f "$archive_dir/privkey${cert_generation}.pem" \
           && ! -L "$archive_dir/privkey${cert_generation}.pem" ]] || {
          echo '  ✗ nginx-reload: stage certificate lineage is inconsistent' >&2
          return 1
        }
        mode="$(stat -Lc %a -- "$key" 2>/dev/null || stat -L -f %Lp -- "$key" 2>/dev/null)" || return 1
        [[ "$mode" == 600 ]] || {
          echo '  ✗ nginx-reload: stage private key must have mode 0600' >&2
          return 1
        }
        openssl x509 -in "$cert" -noout -checkend 0 >/dev/null 2>&1 || {
          echo '  ✗ nginx-reload: stage certificate is expired or unreadable' >&2
          return 1
        }
        for cert_name in staging.concordvoice.chat api.staging.concordvoice.chat media.staging.concordvoice.chat turn.staging.concordvoice.chat; do
          openssl x509 -in "$cert" -noout -checkhost "$cert_name" >/dev/null 2>&1 || {
            echo '  ✗ nginx-reload: stage certificate SAN set is incomplete' >&2
            return 1
          }
        done
        san="$(openssl x509 -in "$cert" -noout -ext subjectAltName 2>/dev/null)" || return 1
        actual="$(printf '%s\n' "$san" | grep -oE 'DNS:[A-Za-z0-9.-]+' | LC_ALL=C sort -u)" || true
        expected=$'DNS:api.staging.concordvoice.chat\nDNS:media.staging.concordvoice.chat\nDNS:staging.concordvoice.chat\nDNS:turn.staging.concordvoice.chat'
        [[ "$actual" == "$expected" ]] || {
          echo '  ✗ nginx-reload: stage certificate must contain exactly the four admitted DNS names' >&2
          return 1
        }
        public_key="$(openssl x509 -in "$cert" -pubkey -noout 2>/dev/null | openssl pkey -pubin -outform DER 2>/dev/null | sha256sum | awk '{print $1}')" || return 1
        cert_key="$(openssl pkey -in "$key" -pubout -outform DER 2>/dev/null | sha256sum | awk '{print $1}')" || return 1
        [[ -n "$public_key" && "$public_key" == "$cert_key" ]] || {
          echo '  ✗ nginx-reload: stage private key does not match the certificate' >&2
          return 1
        }
      fi
      mkdir -p "$NGINX_RENDER_DIR" || return 1
      tmp="$(mktemp "${NGINX_RENDER}.tmp.XXXXXX")" || return 1
      if (( NGINX_STAGE_BOOTSTRAP )); then
        awk '/^# WEST_STAGE_HTTPS_BEGIN$/ { exit } { print }' "$NGINX_SRC" >"$tmp" || { rm -f "$tmp"; return 1; }
      else
        awk '!/^# WEST_STAGE_HTTPS_(BEGIN|END)$/' "$NGINX_SRC" \
          | sed "s|${cert_path_prefix}${cert_path_suffix}|$cert_dir|g" >"$tmp" \
          || { rm -f "$tmp"; return 1; }
      fi
      if ! chmod 0644 "$tmp" || ! mv -f "$tmp" "$NGINX_RENDER"; then
        rm -f "$tmp"
        return 1
      fi
    }

    # Decode only canonical writer data forms; never source the runtime env
    # file. Supports plain data, Bash @Q single/ANSI-C quoting, and Compose's
    # double-quoted dotenv form (\", \\, and $$). No eval or expansion.
    decode_concord_env_value() {
      local raw="$1" decoded="" char escape inner
      local i=0 length=${#1}
      if [[ "$raw" == \'* ]]; then
        (( length >= 2 )) || return 1
        i=1
        while (( i < length )); do
          char="${raw:i:1}"
          if [[ "$char" != "'" ]]; then
            decoded+="$char"
            i=$((i + 1))
          elif (( i == length - 1 )); then
            printf '%s' "$decoded"
            return 0
          elif [[ "${raw:i:4}" == "'\\''" ]]; then
            decoded+="'"
            i=$((i + 4))
          else
            return 1
          fi
        done
        return 1
      fi
      if [[ "$raw" == \$\'* ]]; then
        (( length >= 3 )) && [[ "${raw: -1}" == "'" ]] || return 1
        inner="${raw:2:$((length - 3))}"
        length=${#inner}
        i=0
        while (( i < length )); do
          char="${inner:i:1}"
          if [[ "$char" == \\ ]]; then
            (( i + 1 < length )) || return 1
            escape="${inner:i+1:1}"
            case "$escape" in
              t) decoded+=$'\t' ;;
              "'") decoded+="'" ;;
              *) return 1 ;;
            esac
            i=$((i + 2))
          elif [[ "$char" == "'" ]]; then
            return 1
          else
            decoded+="$char"
            i=$((i + 1))
          fi
        done
        printf '%s' "$decoded"
        return 0
      fi
      if [[ "$raw" == '"'* ]]; then
        (( length >= 2 )) && [[ "${raw: -1}" == '"' ]] || return 1
        inner="${raw:1:$((length - 2))}"
        length=${#inner}
        i=0
        while (( i < length )); do
          char="${inner:i:1}"
          if [[ "$char" == \\ ]]; then
            (( i + 1 < length )) || return 1
            escape="${inner:i+1:1}"
            case "$escape" in
              \\) decoded+=\\ ;;
              '"') decoded+='"' ;;
              *) return 1 ;;
            esac
            i=$((i + 2))
          elif [[ "$char" == '$' ]]; then
            (( i + 1 < length )) && [[ "${inner:i+1:1}" == '$' ]] || return 1
            decoded+='$'
            i=$((i + 2))
          elif [[ "$char" == '"' ]]; then
            return 1
          else
            decoded+="$char"
            i=$((i + 1))
          fi
        done
        printf '%s' "$decoded"
        return 0
      fi
      printf '%s' "$raw"
    }

    read_concord_env_value() {
      local key="$1" line raw="" count=0
      [[ -f "$CONCORD_ENV_FILE" ]] || { printf ''; return 0; }
      cat "$CONCORD_ENV_FILE" >/dev/null 2>&1 || return 1
      while IFS= read -r line || [[ -n "$line" ]]; do
        case "$line" in
          "${key}"=*)
            count=$((count + 1))
            raw="${line#*=}"
            ;;
        esac
      done < "$CONCORD_ENV_FILE"
      (( count <= 1 )) || return 1
      (( count == 1 )) || { printf ''; return 0; }
      decode_concord_env_value "$raw"
    }

    # Keep this in lockstep with config.invalidHSTSHeaderValue: printable ASCII
    # plus TAB, semicolon-separated unique directives, exactly one decimal
    # max-age, valueless includeSubDomains/preload, and token/quoted values.
    validate_hsts_header_value() {
      local value="$1" remaining directive name val inner lower
      local has_value last=false
      local LC_ALL=C
      local printable_re=$'^[\t -~]*$'
      local -A seen=()
      [[ -z "$value" ]] && return 0
      [[ "$value" =~ $printable_re ]] || return 1
      remaining="$value"
      while :; do
        if [[ "$remaining" == *';'* ]]; then
          directive="${remaining%%\;*}"
          remaining="${remaining#*\;}"
        else
          directive="$remaining"
          last=true
        fi
        directive="${directive#"${directive%%[![:blank:]]*}"}"
        directive="${directive%"${directive##*[![:blank:]]}"}"
        if [[ -n "$directive" ]]; then
          has_value=0
          if [[ "$directive" == *=* ]]; then
            name="${directive%%=*}"
            val="${directive#*=}"
            has_value=1
          else
            name="$directive"
            val=""
          fi
          [[ "$name" =~ ^[A-Za-z0-9-]+$ ]] || return 1
          if (( has_value )); then
            if [[ "$val" =~ ^[A-Za-z0-9._-]+$ ]]; then
              :
            elif (( ${#val} >= 2 )) \
              && [[ "${val:0:1}" == '"' && "${val: -1}" == '"' ]]; then
              inner="${val:1:$(( ${#val} - 2 ))}"
              [[ "$inner" != *'"'* && "$inner" != *\\* ]] || return 1
            else
              return 1
            fi
          fi
          lower="${name,,}"
          [[ -z "${seen[$lower]+present}" ]] || return 1
          seen["$lower"]=1
          if [[ "$lower" == max-age ]]; then
            (( has_value )) && [[ "$val" =~ ^[0-9]+$ ]] || return 1
          elif (( has_value )) && [[ "$lower" == includesubdomains || "$lower" == preload ]]; then
            return 1
          fi
        fi
        [[ "$last" == true ]] && break
      done
      [[ -n "${seen[max-age]+present}" ]]
    }

    encode_hsts_for_nginx() {
      local value="$1" encoded="" char i
      for ((i = 0; i < ${#value}; i++)); do
        char="${value:i:1}"
        # Append the literal nginx variable; do not expand it in Bash.
        # shellcheck disable=SC2016
        case "$char" in
          '"') encoded+='\"' ;;
          '$') encoded+='${concord_hsts_dollar}' ;;
          *) encoded+="$char" ;;
        esac
      done
      printf '%s' "$encoded"
    }

    render_nginx_config() {
      local policy="$1" encoded line tmp
      local in_api=0 scoped_replacements=0 owner_markers=0
      encoded="$(encode_hsts_for_nginx "$policy")" || return 1
      mkdir -p "$NGINX_RENDER_DIR" || return 1
      tmp="$(mktemp "${NGINX_RENDER}.tmp.XXXXXX")" || return 1
      if while IFS= read -r line || [[ -n "$line" ]]; do
        case "$line" in
          '# HTTPS — API + WebSocket Hub'*) in_api=1 ;;
          '# HTTPS — Media Plane'*) in_api=0 ;;
        esac
        if (( in_api )) && [[ "$line" == "    # CONCORD_MANAGED_HSTS_OWNER" ]]; then
          printf '%s\n' "$line"
          printf '    proxy_hide_header Strict-Transport-Security;\n'
          printf '    add_header Strict-Transport-Security "%s" always;\n' "$encoded"
          owner_markers=$((owner_markers + 1))
        elif (( in_api )) && [[ "$line" == "        add_header Strict-Transport-Security "* ]]; then
          printf '        add_header Strict-Transport-Security "%s" always;\n' "$encoded"
          scoped_replacements=$((scoped_replacements + 1))
        else
          printf '%s\n' "$line"
        fi
      done < "$NGINX_SRC" > "$tmp"; then
        :
      else
        rm -f "$tmp"
        return 1
      fi
      if (( owner_markers != 1 || scoped_replacements != 3 )); then
        rm -f "$tmp"
        return 1
      fi
      if ! chmod 0644 "$tmp" || ! mv -f "$tmp" "$NGINX_RENDER"; then
        rm -f "$tmp"
        return 1
      fi
    }

    if (( NGINX_STAGE )); then
      if ! render_west_stage_config; then
        echo '  ✗ nginx-reload: stage vhost render/certificate validation failed — refusing to stage nginx' >&2
        exit 1
      fi
    elif ! hsts_header_value="$(read_concord_env_value HSTS_HEADER_VALUE)"; then
      echo "  ✗ nginx-reload: could not safely read HSTS_HEADER_VALUE — REFUSING to stage nginx" >&2
      exit 1
    else
      hsts_header_value="${hsts_header_value:-$HSTS_DEFAULT}"
      if ! validate_hsts_header_value "$hsts_header_value"; then
        echo "  ✗ nginx-reload: HSTS_HEADER_VALUE is malformed — REFUSING to stage nginx" >&2
        exit 1
      fi
      if ! render_nginx_config "$hsts_header_value"; then
        echo "  ✗ nginx-reload: failed to atomically render the nginx HSTS policy — REFUSING to stage nginx" >&2
        exit 1
      fi
    fi

    # #1692: render/remove the admin-console vhost from runtime env. The
    # codename host stays out of git; ADMIN_WEBAUTHN_RP_ID in /opt/concord/.env
    # is the deploy-time source, and only ${ADMIN_DOMAIN} is envsubst-expanded.
    if (( NGINX_STAGE )); then
      admin_desired=absent
    elif ! admin_enabled="$(read_concord_env_value ADMIN_CONSOLE_ENABLED)" \
       || ! admin_domain="$(read_concord_env_value ADMIN_WEBAUTHN_RP_ID)"; then
      echo "  ✗ nginx-reload: could not safely read admin-console settings — REFUSING to stage nginx" >&2
      exit 1
    fi

    if (( ! NGINX_STAGE )); then
    admin_enabled="${admin_enabled:-false}"
    if [[ ! "$admin_enabled" =~ ^(true|false)$ ]]; then
      echo "  ✗ nginx-reload: ADMIN_CONSOLE_ENABLED must be true or false — REFUSING to stage nginx" >&2
      exit 1
    fi

    admin_desired=absent
    if [[ "$admin_enabled" == true ]]; then
      # envsubst is deliberately limited to ADMIN_DOMAIN, but validating the
      # value first is still required: nginx metacharacters must never reach a
      # generated directive. Accept only an ordinary DNS hostname here.
      if [[ -z "$admin_domain" \
         || ! "$admin_domain" =~ ^([A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?\.)+[A-Za-z]{2,}$ ]]; then
        echo "  ✗ nginx-reload: ADMIN_WEBAUTHN_RP_ID must be a valid FQDN — REFUSING to stage nginx" >&2
        exit 1
      fi
      if [[ ! -e "$ADMIN_ORIGIN_CERT" || ! -e "$ADMIN_ORIGIN_KEY" ]]; then
        echo "  ⚠ admin vhost requested but Cloudflare Origin Certificate files are missing:" >&2
        echo "    cert: $ADMIN_ORIGIN_CERT" >&2
        echo "    key:  $ADMIN_ORIGIN_KEY" >&2
        echo "    Keeping admin closed; install the cert/key and rerun deploy to enable it." >&2
        # ponytail: fail closed but keep service deploys green; hard-fail here if admin availability becomes deploy-critical.
      elif [[ ! -f "$ADMIN_TMPL" ]]; then
        echo "  ✗ nginx-reload: admin template not found at $ADMIN_TMPL" >&2
        exit 1
      else
        mkdir -p "$NGINX_RENDER_DIR"
        if ! ADMIN_DOMAIN="$admin_domain" envsubst '${ADMIN_DOMAIN}' < "$ADMIN_TMPL" > "$ADMIN_RENDER" \
           || ! chmod 0644 "$ADMIN_RENDER"; then
          echo "  ✗ nginx-reload: failed to render admin vhost template" >&2
          exit 1
        fi
        admin_desired=present
      fi
    fi
    fi

    # Root-owned candidates and hard-link backups live beside their targets.
    # The candidates are inactive until mv(1) atomically replaces each fixed
    # live pathname; hard links preserve the exact previous inode for rollback.
    main_swapped=0
    main_was_present=0
    admin_mutated=0
    admin_was_present=0
    activation_attempted=0
    transaction_pending=0

    cleanup_nginx_candidates() {
      local cleanup_failed=0
      sudo /bin/rm -f -- "$NGINX_CANDIDATE" || cleanup_failed=1
      if (( ! NGINX_STAGE )); then
        sudo /bin/rm -f -- "$ADMIN_CANDIDATE" || cleanup_failed=1
      fi
      return "$cleanup_failed"
    }

    cleanup_nginx_backups() {
      local cleanup_failed=0
      sudo /bin/rm -f -- "$NGINX_BACKUP" || cleanup_failed=1
      if (( ! NGINX_STAGE )); then
        sudo /bin/rm -f -- "$ADMIN_BACKUP" || cleanup_failed=1
      fi
      return "$cleanup_failed"
    }

    require_nginx_backup_absent() {
      local backup="$1"
      if sudo /usr/bin/test ! -e "$backup"; then
        return 0
      fi
      if sudo /usr/bin/test -e "$backup"; then
        echo "  ✗ nginx-reload: prior rollback artifact exists at $backup" >&2
        echo "    Refusing to mutate nginx; inspect and restore/remove it as root." >&2
        return 1
      fi
      echo "  ✗ nginx-reload: could not determine rollback-artifact state for $backup" >&2
      return 1
    }

    restore_nginx_transaction() {
      local restore_failed=0
      echo "  Restoring the previous nginx configuration" >&2
      if (( main_swapped )); then
        if (( main_was_present )); then
          sudo /usr/bin/mv -fT -- "$NGINX_BACKUP" "$NGINX_DST" || restore_failed=1
        else
          sudo /bin/rm -f -- "$NGINX_DST" || restore_failed=1
        fi
      fi
      if (( admin_mutated )); then
        if (( admin_was_present )); then
          sudo /usr/bin/mv -fT -- "$ADMIN_BACKUP" "$ADMIN_DST" || restore_failed=1
        else
          sudo /bin/rm -f -- "$ADMIN_DST" || restore_failed=1
        fi
      fi

      # A successful reload/start can make the rejected transaction active.
      # After restoring the files, load the old configuration back into nginx.
      if (( activation_attempted )) && (( restore_failed == 0 )); then
        if (( NGINX_STAGE_BOOTSTRAP && !main_was_present )); then
          sudo /bin/systemctl stop nginx || restore_failed=1
        elif ! sudo /usr/sbin/nginx -t; then
          restore_failed=1
        elif systemctl is-active --quiet nginx; then
          sudo /bin/systemctl reload nginx || restore_failed=1
        else
          sudo /bin/systemctl start nginx || restore_failed=1
        fi
      fi
      if (( restore_failed )); then
        # Candidates are never recovery material, but backups are. Preserve
        # every surviving backup when any restore/re-activation step fails so
        # an operator still has the exact prior inode.
        cleanup_nginx_candidates || true
        echo "  ✗ nginx-reload: automatic rollback was incomplete; operator intervention is required" >&2
        return 1
      fi
      cleanup_nginx_candidates || restore_failed=1
      cleanup_nginx_backups || restore_failed=1
      if (( restore_failed )); then
        echo "  ✗ nginx-reload: rollback succeeded, but transaction cleanup was incomplete" >&2
        return 1
      fi
      echo "  ✓ previous nginx configuration restored" >&2
    }

    rollback_nginx_on_exit() {
      local rc=$?
      trap - EXIT INT TERM
      if (( transaction_pending )); then
        restore_nginx_transaction || true
      fi
      exit "$rc"
    }

    verify_active_hsts() {
      local expected="$1" probe_host=${2:-api.concordvoice.chat} probe_ip=${3:-127.0.0.1} headers line value
      local count=0
      # HTTP field values use optional whitespace at their boundaries. curl (or
      # nginx) may normalize that OWS, so compare the semantic value rather
      # than requiring the deploy-time spelling to survive byte-for-byte.
      expected="${expected#"${expected%%[![:blank:]]*}"}"
      expected="${expected%"${expected##*[![:blank:]]}"}"
      if ! headers="$(curl --noproxy '*' --silent --show-error --insecure \
          --head --http1.1 --max-time 10 \
          --resolve "$probe_host:443:$probe_ip" \
          "https://$probe_host/nginx-health")"; then
        return 1
      fi
      while IFS= read -r line || [[ -n "$line" ]]; do
        line="${line%$'\r'}"
        if [[ "${line,,}" == strict-transport-security:* ]]; then
          count=$((count + 1))
          value="${line#*:}"
          value="${value#"${value%%[![:blank:]]*}"}"
          value="${value%"${value##*[![:blank:]]}"}"
          [[ "$value" == "$expected" ]] || return 1
        fi
      done <<< "$headers"
      (( count == 1 ))
    }

    verify_stage_active() {
      if (( NGINX_STAGE_BOOTSTRAP )); then
        curl --noproxy '*' --silent --show-error --output /dev/null --max-time 10 \
          --resolve staging.concordvoice.chat:80:127.0.0.1 \
          http://staging.concordvoice.chat/.well-known/acme-challenge/concord-stage-probe
      else
        verify_active_hsts "$HSTS_DEFAULT" api.staging.concordvoice.chat 10.246.6.1
      fi
    }

    # A backup can be the only remaining copy after an interrupted rollback.
    # Never classify it as debris: fail closed before any privileged mutation.
    if ! require_nginx_backup_absent "$NGINX_BACKUP" \
       || { (( ! NGINX_STAGE )) && ! require_nginx_backup_absent "$ADMIN_BACKUP"; }; then
      exit 1
    fi

    # Candidates are inactive and never recovery material, so stale candidates
    # may be cleared after both backup paths are proven absent.
    if ! cleanup_nginx_candidates; then
      echo "  ✗ nginx-reload: could not clear stale candidate files" >&2
      exit 1
    fi
    echo "  Staging $NGINX_RENDER → $NGINX_CANDIDATE"
    if ! sudo /usr/bin/install -o root -g root -m 0644 "$NGINX_RENDER" "$NGINX_CANDIDATE"; then
      echo "  ✗ nginx-reload: main candidate staging failed (sudoers entry missing or wrong permissions?)" >&2
      cleanup_nginx_candidates || true
      exit 1
    fi
    if [[ "$admin_desired" == present ]]; then
      echo "  Staging admin vhost $ADMIN_RENDER → $ADMIN_CANDIDATE"
      if ! sudo /usr/bin/install -o root -g root -m 0644 "$ADMIN_RENDER" "$ADMIN_CANDIDATE"; then
        echo "  ✗ nginx-reload: admin candidate staging failed (sudoers entry missing or wrong permissions?)" >&2
        cleanup_nginx_candidates || true
        exit 1
      fi
    fi
    if (( NGINX_STAGE_BOOTSTRAP )); then
      if sudo /usr/bin/test -e "$NGINX_DST"; then
        main_was_present=1
      elif ! sudo /usr/bin/test ! -e "$NGINX_DST" || [[ -L "$NGINX_DST" ]]; then
        echo "  ✗ nginx-reload: could not preserve the current main config" >&2
        cleanup_nginx_candidates || true
        cleanup_nginx_backups || true
        exit 1
      fi
    else
      main_was_present=1
    fi
    if (( main_was_present )) && ! sudo /usr/bin/ln -fT -- "$NGINX_DST" "$NGINX_BACKUP"; then
      echo "  ✗ nginx-reload: could not preserve the current main config" >&2
      cleanup_nginx_candidates || true
      cleanup_nginx_backups || true
      exit 1
    fi
    if (( NGINX_STAGE )); then
      :
    elif sudo /usr/bin/test -e "$ADMIN_DST"; then
      admin_was_present=1
      if ! sudo /usr/bin/ln -fT -- "$ADMIN_DST" "$ADMIN_BACKUP"; then
        echo "  ✗ nginx-reload: could not preserve the current admin config" >&2
        cleanup_nginx_candidates || true
        cleanup_nginx_backups || true
        exit 1
      fi
    elif ! sudo /usr/bin/test ! -e "$ADMIN_DST"; then
      # Distinguish an absent file from sudo/test execution failure. Treating
      # a denied/failed existence check as "absent" could destroy an admin
      # config without first preserving it.
      echo "  ✗ nginx-reload: could not determine current admin config state" >&2
      cleanup_nginx_candidates || true
      cleanup_nginx_backups || true
      exit 1
    fi

    transaction_pending=1
    trap rollback_nginx_on_exit EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM

    # Set each rollback flag before the mutating command so INT/TERM cannot
    # land after a successful syscall but before the transaction records it.
    # Restoring a backup is safe even when the attempted mutation fails.
    main_swapped=1
    if ! sudo /usr/bin/mv -fT -- "$NGINX_CANDIDATE" "$NGINX_DST"; then
      echo "  ✗ nginx-reload: could not atomically activate the main candidate" >&2
      exit 1
    fi
    if (( ! NGINX_STAGE )); then
      admin_mutated=1
      if [[ "$admin_desired" == present ]]; then
        if ! sudo /usr/bin/mv -fT -- "$ADMIN_CANDIDATE" "$ADMIN_DST"; then
          echo "  ✗ nginx-reload: could not atomically activate the admin candidate" >&2
          exit 1
        fi
      else
        echo "  Removing disabled admin vhost $ADMIN_DST"
        if ! sudo /bin/rm -f -- "$ADMIN_DST"; then
          echo "  ✗ nginx-reload: admin vhost removal failed" >&2
          exit 1
        fi
      fi
    fi

    echo "  Validating with nginx -t"
    if ! sudo /usr/sbin/nginx -t 2>&1; then
      echo "  ✗ nginx-reload: nginx -t rejected the candidate; rolling back" >&2
      exit 1
    fi

    activation_attempted=1
    echo "  Reloading nginx"
    if sudo /bin/systemctl reload nginx; then
      activation_verb=reloaded
    elif systemctl is-active --quiet nginx; then
      echo "  ✗ nginx-reload: systemctl reload nginx failed; rolling back" >&2
      exit 1
    else
      echo "  nginx service is inactive; starting nginx"
      if ! sudo /bin/systemctl start nginx; then
        echo "  ✗ nginx-reload: systemctl start nginx failed; rolling back" >&2
        exit 1
      fi
      activation_verb=started
    fi

    if (( NGINX_STAGE )); then
      echo "  Probing active West stage vhost"
      if ! verify_stage_active; then
        echo "  ✗ nginx-reload: active West stage probe failed; rolling back" >&2
        exit 1
      fi
    else
      echo "  Probing active HSTS ownership"
      if ! verify_active_hsts "$hsts_header_value"; then
        echo "  ✗ nginx-reload: active /nginx-health did not return exactly one expected HSTS field; rolling back" >&2
        exit 1
      fi
    fi

    transaction_pending=0
    trap - EXIT INT TERM
    cleanup_failed=0
    cleanup_nginx_candidates || cleanup_failed=1
    cleanup_nginx_backups || cleanup_failed=1
    if (( cleanup_failed )); then
      echo "  ✗ nginx config is active, but transaction-file cleanup failed" >&2
      echo "    Active configuration was not rolled back; resolve the stale artifact before retrying." >&2
      exit 1
    fi
    if (( NGINX_STAGE )); then
      echo "  ✓ West staging vhost staged + validated + $activation_verb + probed"
    else
      echo "  ✓ nginx config staged + validated + $activation_verb + probed"
    fi
    ;;

  sudoers-sync)
    # The only delegated capability is one fixed, root-owned helper. It reads a
    # root-owned approved snapshot under /usr/local/share; /opt/concord is never
    # a runtime sudoers input. Policy changes require explicit operator action.
    if ! sudo -n /usr/local/sbin/concord-sudoers-sync; then
      echo "  ✗ sudoers-sync: root-owned sync helper is unavailable or failed" >&2
      echo "    Install from a reviewed root-owned staging directory; see:" >&2
      echo "      [internal]deploys.md#sudoers-boundary-bootstrap-and-updates" >&2
      exit 1
    fi
    echo "  ✓ sudoers-sync: root-approved policy is active"
    ;;

  security-event-logging)
    if ! sudo -n /usr/local/sbin/concord-security-event-logging; then
      echo "  ✗ security-event-logging: root-owned preparation helper is unavailable or failed" >&2
      echo "    Bootstrap from a reviewed root-owned staging directory before rebuilding; see:" >&2
      echo "      [internal]deploys.md#sudoers-boundary-bootstrap-and-updates" >&2
      exit 1
    fi
    echo "  ✓ security-event-logging: dedicated application log boundary is prepared"
    ;;

  wait-healthy)
    # Server-side health gate for #848 deploy.yml. Wraps the loop pattern
    # from the now-deleted deploy.sh:117-134 with a CLI flag for timeout.
    #
    # Healthy = ALL of:
    #   - control-plane /health returns 200
    #   - media-plane /health returns 200
    #   - coturn STUN port (3478) accepts TCP connection
    #     (smoke test only — STUN is UDP, but coturn also listens TCP-3478;
    #      a TCP handshake confirms the daemon is alive but does not validate
    #      STUN responsiveness. For protocol-level STUN testing use stunclient.)
    #
    # Spec ref: 2026-05-06-848-push-based-deploys-design.md §5.4
    shift  # consume "wait-healthy"
    TIMEOUT=60
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --timeout) TIMEOUT="$2"; shift 2 ;;
        --timeout=*) TIMEOUT="${1#--timeout=}"; shift ;;
        *) echo "wait-healthy: unknown arg '$1'" >&2; exit 2 ;;
      esac
    done
    if ! [[ "$TIMEOUT" =~ ^[0-9]+$ ]]; then
      echo "wait-healthy: --timeout must be integer seconds, got '$TIMEOUT'" >&2
      exit 2
    fi
    INTERVAL=2
    DEADLINE=$(( $(date +%s) + TIMEOUT ))
    # Single assignment so the blue/green colour flip (#3108) has ONE line to
    # make colour-aware rather than one per probe site. Every HTTP probe in this
    # file targets 127.0.0.1, never `localhost`: the self-host overlay binds 8080
    # and 3000 to 127.0.0.1 only, so [::1] is free for any local user to answer a
    # `localhost` probe (curl tries it first) and fake readiness (#3538).
    CP_PROBE_BASE="http://127.0.0.1:8080"
    COTURN_PROBE_HOST="${stage_private_address:-127.0.0.1}"
    cp_ok=0; mp_ok=0; coturn_ok=0; tls_ok=0
    cp_refresh_skipped=0; mp_refresh_skipped=0; coturn_refresh_skipped=0
    while [[ $(date +%s) -lt $DEADLINE ]]; do
      cp_ok=0; mp_ok=0; coturn_ok=0; tls_ok=0
      cp_refresh_skipped=0; mp_refresh_skipped=0; coturn_refresh_skipped=0
      # Gates on READINESS, not liveness (#3106). wait-healthy is the real
      # answer to "did this deploy succeed", and a control-plane that bound the
      # port but cannot reach Redis returns 401 on every authenticated request
      # while /health answers 200 throughout — a failed deploy this previously
      # called green. The compose healthcheck keeps probing /health, because
      # that bit is #3107's restart predicate; see
      # [internal]tests/test-compose-health-contract.sh.
      curl -sf "${CP_PROBE_BASE}/readyz" >/dev/null 2>&1 && cp_ok=1
      if [[ "${MEDIA_DEPLOY_PROFILE:-media-single}" == media-bluegreen ]]; then
        media_bluegreen_active_ready && mp_ok=1
      else
        curl -sf "http://127.0.0.1:3000/health" >/dev/null 2>&1 && mp_ok=1
      fi
      nc -z -w2 "$COTURN_PROBE_HOST" 3478 >/dev/null 2>&1 && coturn_ok=1
      if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" != selfhost ]]; then
        tls_ok=1
      else
        _tls_remaining=$(( DEADLINE - $(date +%s) ))
        if (( _tls_remaining > 0 )) && selfhost_tls_ready_gate "$_tls_remaining"; then
          tls_ok=1
        fi
        # The TLS observer may use nearly all of the remaining budget. Its
        # result cannot be combined with readiness samples taken before it.
        cp_ok=0; mp_ok=0; coturn_ok=0
        _cp_remaining=$(( DEADLINE - $(date +%s) ))
        if (( _cp_remaining > 0 )); then
          curl -sf --max-time "$_cp_remaining" "${CP_PROBE_BASE}/readyz" >/dev/null 2>&1 && cp_ok=1
        else
          cp_refresh_skipped=1
        fi
        _mp_remaining=$(( DEADLINE - $(date +%s) ))
        if (( _mp_remaining > 0 )); then
          if [[ "${MEDIA_DEPLOY_PROFILE:-media-single}" == media-bluegreen ]]; then
            media_bluegreen_active_ready && mp_ok=1
          else
            curl -sf --max-time "$_mp_remaining" "http://127.0.0.1:3000/health" >/dev/null 2>&1 && mp_ok=1
          fi
        else
          mp_refresh_skipped=1
        fi
        _coturn_remaining=$(( DEADLINE - $(date +%s) ))
        if (( _coturn_remaining > 0 )); then
          _coturn_probe_limit=2
          (( _coturn_remaining < _coturn_probe_limit )) && _coturn_probe_limit="$_coturn_remaining"
          nc -z -w "$_coturn_probe_limit" "$COTURN_PROBE_HOST" 3478 >/dev/null 2>&1 && coturn_ok=1
        else
          coturn_refresh_skipped=1
        fi
      fi
      if (( $(date +%s) < DEADLINE )) &&
          [[ $cp_ok -eq 1 && $mp_ok -eq 1 && $coturn_ok -eq 1 && $tls_ok -eq 1 ]]; then
        selfhost_post_up_check || exit 1
        if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]]; then
          echo "  ✓ all services healthy (control-plane + media-plane + coturn TLS)"
        else
          echo "  ✓ all services healthy (control-plane + media-plane + coturn)"
        fi
        exit 0
      fi
      _sleep_remaining=$(( DEADLINE - $(date +%s) ))
      (( _sleep_remaining > 0 )) || break
      if (( _sleep_remaining < INTERVAL )); then
        sleep "$_sleep_remaining"
      else
        sleep "$INTERVAL"
      fi
    done
    echo "  ✗ wait-healthy timed out after ${TIMEOUT}s" >&2
    if [[ $cp_ok -eq 0 ]]; then
      if [[ $cp_refresh_skipped -eq 1 ]]; then
        echo "    control-plane (:8080/readyz) not rechecked (deadline)" >&2
      else
        echo "    control-plane (:8080/readyz) failed" >&2
        # Liveness on the DIAGNOSTIC path only: it distinguishes "the process is
        # dead or not listening" from "the process is listening but not ready",
        # which is a materially better first line for whoever reads this.
        if curl -sf "${CP_PROBE_BASE}/health" >/dev/null 2>&1; then
          echo "    ...but :8080/health answers, so the process is UP and NOT READY." >&2
          # The body names which dependency is down, so a failed deploy is
          # diagnosable from the workflow log without logging into the host. It
          # carries dependency names and up/down only — never values.
          # Capture BEFORE printing: piping straight to head meant a failed or
          # timed-out fetch printed the label followed by a blank line, which is
          # indistinguishable from the endpoint returning an empty body -- a
          # diagnostic silent about its own failure.
          # `|| true` is load-bearing, not defensive. The script runs under
          # `set -euo pipefail` (line 4), and errexit DOES fire on a failing
          # command substitution in an ASSIGNMENT (it does not in an argument --
          # the same asymmetry [internal]rules/deployment.md documents for the
          # Compose dotenv writer). So a curl that fails or times out, or a head
          # that closes early on a >512B body, would abort the whole script here
          # -- turning a diagnostic into a fatal, and skipping the very branches
          # below that exist to explain the failure.
          readyz_body="$(curl -s --max-time 2 "${CP_PROBE_BASE}/readyz" 2>/dev/null | head -c 512 || true)"
          if [[ -n "$readyz_body" ]]; then
            echo "    /readyz reported: ${readyz_body}" >&2
          else
            echo "    /readyz returned no body within 2s (fetch failed or timed out)." >&2
          fi
        else
          echo "    ...and :8080/health does not answer either, so the process is DOWN." >&2
        fi
      fi
    fi
    if [[ $mp_ok -eq 0 ]]; then
      _mp_probe_label='media-plane (:3000/health)'
      [[ "${MEDIA_DEPLOY_PROFILE:-media-single}" == media-bluegreen ]] \
        && _mp_probe_label='active media colour (/readyz)'
      if [[ $mp_refresh_skipped -eq 1 ]]; then
        echo "    ${_mp_probe_label} not rechecked (deadline)" >&2
      else
        echo "    ${_mp_probe_label} failed" >&2
      fi
    fi
    if [[ $coturn_ok -eq 0 ]]; then
      if [[ $coturn_refresh_skipped -eq 1 ]]; then
        echo "    coturn STUN (:3478) not rechecked (deadline)" >&2
      else
        echo "    coturn STUN (:3478) failed" >&2
      fi
    fi
    [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" != selfhost || $tls_ok -eq 1 ]] \
      || echo "    coturn TLS listener proof failed" >&2
    dump_health_diagnostics
    exit 1
    ;;

  freshstart)
    # skip-receipt: the receipt never authorizes freshstart's volume wipe — a
    # gate-true freshstart still requires live one-container proof (#2326).
    if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" != selfhost ]]; then
      activity_history_routine_guard skip-receipt
    fi
    # lockf re-enters under the lock after the parent confirmed and admitted TLS.
    if (( ! CONCORD_CTL_LOCKF_REENTRY_INTERNAL )); then
    echo "=== Fresh Start: wiping all data and rebuilding ==="
    echo "This will DELETE all databases, caches, and user data."
    read -rp "Are you sure? (y/N) " confirm
    if [[ "${confirm,,}" != "y" ]]; then
      echo "Aborted."
      exit 0
    fi
    # Consume confirmation first, then admit TLS before the receipt guard or
    # any rollback/image/volume mutation. BYO has already refused pre-dispatch.
    if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]]; then
      selfhost_tls_admit_gate || exit 1
      activity_history_routine_guard skip-receipt
    fi
    # ACQUIRED HERE, not in the pre-dispatch block, and the difference is an
    # unbounded hold. `flock -w` bounds how long acquisition WAITS; once taken,
    # the descriptor-form lock is held by this shell until it exits. Acquiring
    # before the prompt above meant an operator who paused or walked away from
    # the confirmation left healthwatch deferring every cycle and every other
    # deploy verb timing out at 90s, indefinitely, with no indication why.
    # After the confirmation and before the first mutation is the only correct
    # place: freshstart still holds the interlock for everything destructive.
    DEPLOY_LOCK_WAIT_SECS=90
    acquire_deploy_transaction_lock "$@"
    fi

    managed_production_nats_reconcile model-only || exit 1
    clear_activity_history_image_selector
    activity_history_invalidate_activation_receipt
    echo ""
    echo "Stopping all services and removing volumes..."
    cd "$DEPLOY_DIR" && $COMPOSE_CMD down -v
    managed_production_nats_reconcile || exit 1
    echo ""
    # #1015: sync_releases no longer auto-called. If the server's
    # /opt/concord/releases/ is empty or stale, trigger sync-releases.yml
    # OR run `concord-ctl.sh sync-releases` before invoking freshstart.
    echo "Rebuilding and starting fresh..."
    echo "Note: release assets NOT refreshed automatically (#1015). Run 'concord-ctl.sh sync-releases' or trigger sync-releases.yml AFTER freshstart completes to populate /opt/concord/releases/."
    cd "$DEPLOY_DIR" && $COMPOSE_CMD up -d --build
    if ! voice_enforcement_rollout_wait_control_plane \
      || ! voice_enforcement_rollout_wait_media \
      || ! voice_enforcement_rollout_activate; then
      echo "Fresh start failed the voice-enforcement readiness/activation gate; enforcement remains inactive." >&2
      exit 1
    fi
    selfhost_post_up_check || exit 1
    selfhost_tls_ready_gate || exit 1
    echo ""
    echo "Fresh start complete. All data has been wiped."
    ;;

  sync-releases)
    sync_releases
    ;;

  verify-registration)
    echo "=== Registration Flow Verification ==="
    failed=0

    # 1. pending_registrations table exists
    if docker exec concordvoice-postgres psql -U concord -d concord -Atc \
         "SELECT 1 FROM information_schema.tables WHERE table_name = 'pending_registrations';" \
         2>/dev/null | grep -q "1"; then
      echo "  ✓ pending_registrations table exists"
    else
      echo "  ✗ pending_registrations table missing"
      failed=$((failed + 1))
    fi

    # 2. No leftover unverified users
    UNVERIFIED=$(docker exec concordvoice-postgres psql -U concord -d concord -Atc \
                  "SELECT COUNT(*) FROM users WHERE email_verified = FALSE;" 2>/dev/null)
    if [[ "$UNVERIFIED" == "0" ]]; then
      echo "  ✓ no unverified users in users table"
    else
      echo "  ✗ ${UNVERIFIED} unverified users still present"
      failed=$((failed + 1))
    fi

    # 3. /register endpoint responds (400 expected on empty body)
    HTTP=$(curl -sf -o /dev/null -w "%{http_code}" \
             -X POST http://127.0.0.1:8080/api/v1/auth/register \
             -H "Content-Type: application/json" -d '{}' 2>/dev/null || echo "000")
    if [[ "$HTTP" == "400" ]]; then
      echo "  ✓ /register endpoint responding (400 on empty body expected)"
    else
      echo "  ✗ /register endpoint returned ${HTTP} (expected 400)"
      failed=$((failed + 1))
    fi

    if [[ $failed -gt 0 ]]; then
      echo ""
      echo "verify-registration: ${failed} check(s) failed"
      exit 1
    fi
    ;;

  docker-clean)
    # Reject unknown arguments loudly — a typo like `--agressive` must not
    # silently downgrade to conservative mode during incident response.
    case "${2:-}" in
      "") AGGRESSIVE=0 ;;
      --aggressive) AGGRESSIVE=1 ;;
      *) echo "docker-clean: unknown argument '${2}'. Only --aggressive is supported." >&2; exit 2 ;;
    esac

    # Timestamp in the banner so week-to-week cron log entries are distinguishable.
    echo "=== Docker Cleanup — $(date '+%Y-%m-%d %H:%M:%S %Z') ==="
    echo "Before:"
    docker system df
    echo ""

    if [[ $AGGRESSIVE -eq 1 ]]; then
      echo "Mode: aggressive (no time filter)"
      docker image prune -af
      docker builder prune -af
      # 24h filter gives operators a rollback window on containers they
      # may have stopped minutes earlier for debugging.
      docker container prune -f --filter "until=24h"
    else
      echo "Mode: conservative (retain last 168h)"
      docker image prune -af --filter "until=168h"
      docker builder prune -af --filter "until=168h"
    fi

    echo ""
    echo "After:"
    docker system df
    # Completion marker so operators/monitoring can grep for successful runs
    # versus runs that aborted mid-prune via set -euo pipefail.
    echo "=== Docker Cleanup — completed successfully ==="
    ;;

  stop)
    if [[ "$MEDIA_DEPLOY_PROFILE" == media-bluegreen ]]; then
      echo 'stop: refusing to remove colour containers while bluegreen ownership is active' >&2
      exit 1
    fi
    echo "Stopping all services..."
    if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]]; then
      selfhost_storage_stop_stale_minio || exit 1
    fi
    cd "$DEPLOY_DIR" && $COMPOSE_CMD down
    echo "Done."
    ;;

  *)
    echo "Concord Service Manager"
    echo ""
    echo "Usage: $0 <command>"
    echo ""
    echo "Commands:"
    echo "  status         Show running containers, health checks, last deploy metadata"
    echo "  logs           Tail service logs (pass service name to filter, e.g. 'logs control-plane')"
    echo "  restart        Restart all services (no auto-sync since #1015)"
    echo "  rebuild [SERVICE...]"
    echo "                 Force rebuild + restart (no auto-sync since #1015). With no"
    echo "                 arguments, rebuilds every service: the voice services'"
    echo "                 dependencies (health-gated), then control-plane and"
    echo "                 media-plane through the paired voice handoff, then the rest."
    echo "                 With a service list, rebuilds only those with --no-deps --wait,"
    echo "                 escalates to a full rebuild if any named service or dependency"
    echo "                 is not running, and refuses media-plane mixed with a non-voice"
    echo "                 service. Clears a persisted Activity History rollback image pin"
    echo "                 only when unscoped or when control-plane is in scope"
    echo "  activity-history-routine-guard"
    echo "                 Verify canonical/rendered config and safe routine-deploy state"
    echo "  activity-history-activate --confirm-drained"
    echo "                 First activation: require zero CPs, preflight, CP-only start + health"
    echo "  activity-history-rollback --confirm-drained --old-image <immutable-image-ref> [--downgrade-schema]"
    echo "                 Drain/disable, optionally downgrade, and start one pinned old CP image"
    echo "  voice-enforcement-rollout deploy"
    echo "                 Build CP, drain/rebuild MP, verify protocol 3, activate"
    echo "  media-reap [--force]"
    echo "                 Reap only a confirmed draining color; force requires operator confirmation"
    echo "  validate-config  §F: pre-rebuild guard via 'docker compose config --quiet'"
    echo "  media-shared-listeners-check  validate a lab Compose JSON render on stdin; does not admit deployment"
    echo "  selfhost-port-check  Self-host only: refuse live port bindings outside the posture allowlist (#3538)"
    echo "  attachment-probe  Validate the armed attachment backend without pulling images"
    echo "  attachment-probe-cron-install  Idempotently install the nightly attachment probe"
    echo "  nginx-reload   Validate/render HSTS, atomically swap main/admin nginx files,"
    echo "                 nginx -t, reload/start, active-probe, and rollback on failure"
    echo "  wait-healthy   Block until all services healthy (--timeout SECONDS, default 60)"
    echo "  freshstart     Wipe all data, clear rollback image pin, rebuild from scratch"
    echo "  sync-releases  Manual operator-recovery — normal sync happens via GitHub Actions sync-releases.yml"
    echo "  verify-registration  Post-deploy smoke test for registration flow"
    echo "  docker-clean   Prune unused images + builder cache (weekly cron target)"
    echo "                 Add --aggressive to drop 7-day retention + prune stopped containers"
    echo "  stop           Stop all services"
    # An unknown verb must fail: a caller (concord-selfhost health) treats exit 0 as
    # "the check ran and passed", so usage on a typo or a missing verb read as success.
    [[ "${1:-help}" =~ ^(help|-h|--help)$ ]] || exit 2
    ;;
esac
