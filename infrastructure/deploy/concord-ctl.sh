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

# TURN_CREDENTIAL_MODE (#3931) comes ONLY from the canonical writer pair (root
# .env + deploy.env.local). A caller-supplied value is captured and cleared here
# so an exported variable can never select the managed model by itself; the
# selector below refuses any disagreement with the canonical value.
TURN_CREDENTIAL_MODE_REQUESTED="${TURN_CREDENTIAL_MODE-}"
unset TURN_CREDENTIAL_MODE

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
REDIS_CREDENTIAL_MODE="${REDIS_CREDENTIAL_MODE:-legacy}"
redis_managed_writer_consistency_guard() {
  local root_file="${CONFIG_ROOT}/.env" deploy_file="${CONFIG_DEPLOY_DIR}/deploy.env.local"
  local key line root_value deploy_value root_count deploy_count
  local keys=(REDIS_CREDENTIAL_MODE)
  [[ -f "$root_file" && ! -L "$root_file" && -f "$deploy_file" && ! -L "$deploy_file" ]] || {
    echo 'redis-managed: canonical writer pair is missing; refusing before Compose' >&2
    return 1
  }
  if [[ "$REDIS_CREDENTIAL_MODE" == managed ]]; then
    keys+=(REDIS_ENDPOINT REDIS_TLS REDIS_TLS_CA_FILE REDIS_INFISICAL_URL
      REDIS_SERVER_PROJECT_ID REDIS_SERVER_ENVIRONMENT REDIS_SERVER_PATH REDIS_SERVER_CLIENT_ID
      REDIS_CONTROL_PLANE_PROJECT_ID REDIS_CONTROL_PLANE_ENVIRONMENT REDIS_CONTROL_PLANE_PATH REDIS_CONTROL_PLANE_CLIENT_ID
      REDIS_MEDIA_PLANE_PROJECT_ID REDIS_MEDIA_PLANE_ENVIRONMENT REDIS_MEDIA_PLANE_PATH REDIS_MEDIA_PLANE_CLIENT_ID
      REDIS_CONTROL_UID REDIS_MEDIA_UID REDIS_ACL_UID REDIS_ACL_GID)
  fi
  for key in "${keys[@]}"; do
    root_count=0 deploy_count=0 root_value="" deploy_value=""
    while IFS= read -r line || [[ -n "$line" ]]; do
      if [[ "$line" == "${key}="* ]]; then
        root_count=$((root_count + 1))
        root_value="${line#*=}"
      fi
    done < "$root_file"
    while IFS= read -r line || [[ -n "$line" ]]; do
      if [[ "$line" == "${key}="* ]]; then
        deploy_count=$((deploy_count + 1))
        deploy_value="${line#*=}"
      fi
    done < "$deploy_file"
    if [[ "$key" == REDIS_CREDENTIAL_MODE && "$REDIS_CREDENTIAL_MODE" == legacy \
       && "$root_count" == 0 && "$deploy_count" == 0 ]]; then
      return 0 # Existing legacy hosts may predate the mode selector in both writers.
    fi
    # The canonical writer emits raw Compose values and Bash's @Q form respectively.
    if [[ "$root_count" != 1 || "$deploy_count" != 1 || "$root_value" != "${!key:-}" || "$deploy_value" != "${!key@Q}" ]]; then
      echo "redis-managed: canonical writers disagree on ${key}; refusing before Compose" >&2
      return 1
    fi
  done
}
case "$REDIS_CREDENTIAL_MODE" in
  legacy) COMPOSE_REDIS_MANAGED_ARG="" ;;
  managed)
    if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]]; then
      echo 'redis-managed: managed Redis credentials are unavailable to self-host installs' >&2
      exit 2
    fi
    for _redis_selector in REDIS_ENDPOINT REDIS_TLS REDIS_INFISICAL_URL \
      REDIS_SERVER_PROJECT_ID REDIS_SERVER_ENVIRONMENT REDIS_SERVER_PATH REDIS_SERVER_CLIENT_ID \
      REDIS_CONTROL_PLANE_PROJECT_ID REDIS_CONTROL_PLANE_ENVIRONMENT REDIS_CONTROL_PLANE_PATH REDIS_CONTROL_PLANE_CLIENT_ID \
      REDIS_MEDIA_PLANE_PROJECT_ID REDIS_MEDIA_PLANE_ENVIRONMENT REDIS_MEDIA_PLANE_PATH REDIS_MEDIA_PLANE_CLIENT_ID \
      REDIS_CONTROL_UID REDIS_MEDIA_UID REDIS_ACL_UID REDIS_ACL_GID; do
      if [[ -z "${!_redis_selector:-}" ]]; then
        echo "redis-managed: required selector ${_redis_selector} is missing; refusing before Compose mutation" >&2
        exit 1
      fi
    done
    [[ "$REDIS_TLS" == true || "$REDIS_TLS" == false ]] || {
      echo 'redis-managed: REDIS_TLS must be true or false' >&2; exit 1;
    }
    [[ "$REDIS_ENDPOINT" =~ ^[A-Za-z0-9.-]+:[0-9]{1,5}$ ]] || {
      echo 'redis-managed: REDIS_ENDPOINT must be a bare host:port' >&2; exit 1;
    }
    [[ "$REDIS_INFISICAL_URL" == https://* && "$REDIS_INFISICAL_URL" != *'@'* ]] || {
      echo 'redis-managed: REDIS_INFISICAL_URL must be HTTPS without userinfo' >&2; exit 1;
    }
    for _redis_uid in REDIS_CONTROL_UID REDIS_MEDIA_UID REDIS_ACL_UID REDIS_ACL_GID; do
      [[ "${!_redis_uid}" =~ ^[0-9]+$ ]] || {
        echo "redis-managed: ${_redis_uid} must come from selected-image inspection" >&2; exit 1;
      }
      if [[ "$_redis_uid" != REDIS_ACL_UID ]] &&
          ! python3 -c 'import sys; n = int(sys.argv[1]); sys.exit(n < 1 or n > 65535)' "${!_redis_uid}" 2>/dev/null; then
        echo "redis-managed: ${_redis_uid} must be in 1..65535" >&2; exit 1
      fi
    done
    # Builtin test compares in base 10, so 010006 equals 10006.
    [ "$REDIS_CONTROL_UID" -ne "$REDIS_MEDIA_UID" ] || {
      echo 'redis-managed: REDIS_CONTROL_UID and REDIS_MEDIA_UID must differ; peer UID tells the ACK slots apart' >&2; exit 1;
    }
    COMPOSE_REDIS_MANAGED_ARG=" -f ${DEPLOY_DIR}/docker-compose.redis-managed.yml"
    ;;
  *) echo 'redis-managed: REDIS_CREDENTIAL_MODE must be legacy or managed' >&2; exit 2 ;;
esac

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
# Managed TURN credential custody (#3931). Same shape as the Redis selector:
# legacy keeps the static TURN_SECRET path; managed layers
# docker-compose.turn-managed.yml. PR-A ships this dormant: the mode is read
# only from the canonical writer pair, and only provisioning (Task 3) can write
# it there, so nothing activates it by accident.
TURN_CREDENTIAL_MODE="${TURN_CREDENTIAL_MODE:-legacy}"
turn_managed_writer_consistency_guard() {
  local root_file="${CONFIG_ROOT}/.env" deploy_file="${CONFIG_DEPLOY_DIR}/deploy.env.local"
  local key line root_value deploy_value root_count deploy_count
  # The managed model has no other selectors yet; provisioning appends any it
  # introduces here so both canonical writers must agree on them as well.
  local keys=(TURN_CREDENTIAL_MODE)
  [[ -f "$root_file" && ! -L "$root_file" && -f "$deploy_file" && ! -L "$deploy_file" ]] || {
    echo 'turn-managed: canonical writer pair is missing; refusing before Compose' >&2
    return 1
  }
  for key in "${keys[@]}"; do
    root_count=0 deploy_count=0 root_value="" deploy_value=""
    while IFS= read -r line || [[ -n "$line" ]]; do
      if [[ "$line" == "${key}="* ]]; then
        root_count=$((root_count + 1))
        root_value="${line#*=}"
      fi
    done < "$root_file"
    while IFS= read -r line || [[ -n "$line" ]]; do
      if [[ "$line" == "${key}="* ]]; then
        deploy_count=$((deploy_count + 1))
        deploy_value="${line#*=}"
      fi
    done < "$deploy_file"
    if [[ "$key" == TURN_CREDENTIAL_MODE && "$TURN_CREDENTIAL_MODE" == legacy \
       && "$root_count" == 0 && "$deploy_count" == 0 ]]; then
      return 0 # Existing legacy hosts predate the mode selector in both writers.
    fi
    # The canonical writer emits raw Compose values and Bash's @Q form respectively.
    if [[ "$root_count" != 1 || "$deploy_count" != 1 || "$root_value" != "${!key:-}" || "$deploy_value" != "${!key@Q}" ]]; then
      echo "turn-managed: canonical writers disagree on ${key}; refusing before Compose" >&2
      return 1
    fi
  done
}
if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost \
   && ( "$TURN_CREDENTIAL_MODE" != legacy || "$TURN_CREDENTIAL_MODE_REQUESTED" == managed ) ]]; then
  echo 'turn-managed: managed TURN credentials are unavailable to self-host installs' >&2
  exit 2
fi
if [[ -n "$TURN_CREDENTIAL_MODE_REQUESTED" && "$TURN_CREDENTIAL_MODE_REQUESTED" != "$TURN_CREDENTIAL_MODE" ]]; then
  echo 'turn-managed: TURN_CREDENTIAL_MODE is read only from the canonical writer pair; refusing the caller environment value' >&2
  exit 2
fi
case "$TURN_CREDENTIAL_MODE" in
  legacy) COMPOSE_TURN_MANAGED_ARG="" ;;
  managed)
    if [[ "$COMPOSE_OVERRIDE" != docker-compose.production.yml ]]; then
      echo 'turn-managed: managed TURN credentials require the production Compose override' >&2
      exit 2
    fi
    # Dormant gate: refuse before any Compose model is composed unless BOTH
    # canonical writers record the mode.
    turn_managed_writer_consistency_guard || exit 1
    COMPOSE_TURN_MANAGED_ARG=" -f ${DEPLOY_DIR}/docker-compose.turn-managed.yml"
    ;;
  *) echo 'turn-managed: TURN_CREDENTIAL_MODE must be legacy or managed' >&2; exit 2 ;;
esac
COMPOSE_CMD="docker compose ${COMPOSE_ENV_FILE_ARG}${COMPOSE_PROJECT_ARG} -f ${DEPLOY_DIR}/docker-compose.yml${COMPOSE_PRODUCTION_ARG} -f ${DEPLOY_DIR}/${COMPOSE_OVERRIDE}${COMPOSE_SELFHOST_ARG}${COMPOSE_REDIS_MANAGED_ARG}${COMPOSE_TURN_MANAGED_ARG} ${COMPOSE_PROFILE_ARGS}"

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

# A merged caller stays direct until a root operator has completed the separate
# commissioning ceremony. The receipt is public metadata, never policy input.
readonly EGRESS_RECEIPT=/etc/concord/egress-commissioned.json
readonly EGRESS_ENGINE=/usr/local/sbin/nightwatch-egress
readonly EGRESS_COMPANION=/usr/local/sbin/concord-egress
readonly EGRESS_BUILD_IMAGE='moby/buildkit:v0.34.1-rootless@sha256:f68752a5a94acfcdad2d07867b7075601a421638e17fc5f28efc1c814d0c4f89'

managed_egress_role() {
  case "$COMPOSE_OVERRIDE" in
    docker-compose.production.yml) printf '%s' production ;;
    docker-compose.staging.yml|docker-compose.west-staging.yml) printf '%s' staging ;;
    *) echo 'managed-egress: deployment role is unknown' >&2; return 1 ;;
  esac
}

managed_egress_receipt_valid() {
  local role="$1"
  [[ ! -L /etc/concord && -d /etc/concord \
     && "$(stat -c '%u:%g:%a:%F' /etc/concord 2>/dev/null)" == '0:0:755:directory' \
     && ! -L "$EGRESS_RECEIPT" && -f "$EGRESS_RECEIPT" \
     && "$(stat -c '%u:%g:%a:%F' "$EGRESS_RECEIPT" 2>/dev/null)" == '0:0:644:regular file' ]] || return 1
  python3 -c '
import json, re, sys
def strict_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate key")
        result[key] = value
    return result
try:
    with open(sys.argv[1], encoding="utf-8") as stream:
        receipt = json.load(stream, object_pairs_hook=strict_object)
except (OSError, ValueError, TypeError):
    raise SystemExit(1)
expected = {
    "schema", "status", "role", "project", "manifest_sha256", "build_image",
    "host_admission_id", "companion_admission_id", "host_source_sha256",
    "companion_source_sha256", "host_package_sha256", "companion_package_sha256",
    "host_policy_sha256", "companion_policy_sha256",
}
uuid_v4 = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}")
name = re.compile(r"[a-z][a-z0-9_-]{0,62}")
digest_fields = (
    "manifest_sha256", "host_source_sha256", "companion_source_sha256",
    "host_package_sha256", "companion_package_sha256", "host_policy_sha256",
    "companion_policy_sha256",
)
if (not isinstance(receipt, dict) or set(receipt) != expected or
        type(receipt["schema"]) is not int or receipt["schema"] != 1 or receipt["status"] != "active" or
        receipt["role"] != sys.argv[2] or
        not isinstance(receipt["project"], str) or not name.fullmatch(receipt["project"]) or
        any(not isinstance(receipt[key], str) or not re.fullmatch(r"[0-9a-f]{64}", receipt[key]) for key in digest_fields) or
        any(not isinstance(receipt[key], str) or not uuid_v4.fullmatch(receipt[key]) for key in ("host_admission_id", "companion_admission_id")) or
        receipt["build_image"] != sys.argv[3]):
    raise SystemExit(1)
' "$EGRESS_RECEIPT" "$role" "$EGRESS_BUILD_IMAGE"
}

managed_egress_receipt_project() {
  local role="$1"
  managed_egress_receipt_valid "$role" || return 1
  python3 -c 'import json, sys; print(json.load(open(sys.argv[1], encoding="utf-8"))["project"])' "$EGRESS_RECEIPT"
}

managed_egress_project_matches_receipt() {
  local role="$1" project rendered
  project="$(managed_egress_receipt_project "$role")" || return 1
  rendered="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD config --format json)" || {
    echo 'managed-egress: cannot render the commissioned Compose project' >&2
    return 1
  }
  python3 -c '
import json, sys
try:
    model = json.load(sys.stdin)
except (ValueError, TypeError):
    raise SystemExit(1)
raise SystemExit(0 if isinstance(model, dict) and model.get("name") == sys.argv[1] else 1)
' "$project" <<<"$rendered" || {
    echo 'managed-egress: rendered Compose project differs from the commissioned project' >&2
    return 1
  }
}

managed_egress_mode() {
  if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]]; then
    printf '%s\n' direct
    return 0
  fi
  local receipt_present=0 engine_present=0 companion_present=0 role
  # Classify complete absence before deriving a role. This preserves direct CD
  # on every uncommissioned overlay, including immutable West staging.
  [[ -e "$EGRESS_RECEIPT" || -L "$EGRESS_RECEIPT" ]] && receipt_present=1
  [[ -e "$EGRESS_ENGINE" || -L "$EGRESS_ENGINE" ]] && engine_present=1
  [[ -e "$EGRESS_COMPANION" || -L "$EGRESS_COMPANION" ]] && companion_present=1
  if [[ "$receipt_present:$engine_present:$companion_present" == 0:0:0 ]]; then
    printf '%s\n' direct
    return 0
  fi
  role="$(managed_egress_role)" || return 1
  # Inspect and authenticate the receipt before accepting a commissioned state.
  if [[ $receipt_present -eq 1 ]] && ! managed_egress_receipt_valid "$role"; then
    echo 'managed-egress: commissioning receipt is invalid; refusing deployment' >&2
    return 1
  fi
  if [[ "$receipt_present:$engine_present:$companion_present" != 1:1:1 \
     || -L "$EGRESS_ENGINE" || -L "$EGRESS_COMPANION" \
     || ! -f "$EGRESS_ENGINE" || ! -x "$EGRESS_ENGINE" \
     || ! -f "$EGRESS_COMPANION" || ! -x "$EGRESS_COMPANION" ]]; then
    echo 'managed-egress: commissioning is partial; refusing deployment' >&2
    return 1
  fi
  printf '%s\n' managed
}

managed_egress_gate() {
  [[ $# -eq 1 && ( $1 == verify || $1 == ready ) ]] || return 1
  local mode role status
  mode="$(managed_egress_mode)" || return 1
  [[ "$mode" == direct ]] && return 0
  role="$(managed_egress_role)" || return 1
  managed_egress_project_matches_receipt "$role" || return 1
  status="$(sudo -n "$EGRESS_COMPANION" deployment-status --role "$role")" || {
    echo 'managed-egress: commissioned deployment status is unavailable' >&2
    return 1
  }
  [[ ${#status} -le 4096 ]] || return 1
  python3 -c '
import json, re, sys
def strict_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate key")
        result[key] = value
    return result
try:
    with open(sys.argv[1], encoding="utf-8") as stream:
        receipt = json.load(stream, object_pairs_hook=strict_object)
    status = json.load(sys.stdin, object_pairs_hook=strict_object)
except (OSError, ValueError, TypeError):
    raise SystemExit(1)
expected = {"schema", "status", "role", "manifest_sha256", "build_image"}
if (not isinstance(status, dict) or set(status) != expected or
        type(status.get("schema")) is not int or status.get("schema") != 1 or status.get("status") != "active" or
        status.get("role") != sys.argv[2] or
        not isinstance(status.get("manifest_sha256"), str) or
        not re.fullmatch(r"[0-9a-f]{64}", status["manifest_sha256"]) or
        status.get("build_image") != sys.argv[3] or
        status != {key: receipt[key] for key in expected}):
    raise SystemExit(1)
' "$EGRESS_RECEIPT" "$role" "$EGRESS_BUILD_IMAGE" <<<"$status" || {
    echo 'managed-egress: commissioned deployment status is invalid' >&2
    return 1
  }
  sudo -n "$EGRESS_ENGINE" "$1" --role "$role" >/dev/null || {
    echo 'managed-egress: root-admitted role policy is unavailable or not ready' >&2
    return 1
  }
  if [[ "$1" == ready ]]; then
    sudo -n "$EGRESS_COMPANION" ready --role "$role" >/dev/null || {
      echo 'managed-egress: admitted workload graph is incomplete or not ready' >&2
      return 1
    }
  fi
}

managed_egress_reconcile() {
  local mode
  mode="$(managed_egress_mode)" || return 1
  [[ "$mode" == direct ]] && return 0
  sudo -n "$EGRESS_COMPANION" refresh >/dev/null || {
    echo 'managed-egress: namespace reconciliation failed' >&2
    return 1
  }
  managed_egress_gate verify
}

# Reconcile every namespace mutation, including a partially failed Compose
# operation. Health waits run only after the new namespaces have been admitted.
managed_compose_mutate() (
  voice_enforcement_rollout_guard_mutation "$@" || return 1
  cd "$DEPLOY_DIR" || return
  local mode command wait=0 wait_timeout=180 result=0 ids id states state health deadline
  local -a original=("$@") args=() services=() containers=()
  mode="$(managed_egress_mode)" || return 1
  if [[ "$mode" == direct ]]; then
    $COMPOSE_CMD "$@"
    return
  fi
  command="$1"
  shift
  if [[ "$command" == up || "$command" == start ]]; then
    while (( $# )); do
      case "$1" in
        --wait|--wait=true) wait=1; shift ;;
        --wait=false) wait=0; shift ;;
        -d|--detach|--detach=*)
          [[ "$command" == up ]] || args+=("$1")
          shift ;;
        --wait-timeout)
          [[ $# -ge 2 ]] || { echo 'managed-egress: --wait-timeout requires seconds' >&2; return 2; }
          wait_timeout="$2"; shift 2 ;;
        --wait-timeout=*) wait_timeout="${1#*=}"; shift ;;
        --pull|--timeout|-t|--scale|--attach|--no-attach|--exit-code-from)
          [[ $# -ge 2 ]] || { echo 'managed-egress: Compose option requires a value' >&2; return 2; }
          args+=("$1" "$2"); shift 2 ;;
        --) args+=("$@"); shift; services+=("$@"); break ;;
        -*) args+=("$1"); shift ;;
        *) args+=("$1"); services+=("$1"); shift ;;
      esac
    done
  fi
  if (( wait )); then
    [[ "$wait_timeout" =~ ^[0-9]+$ && "$wait_timeout" -gt 0 ]] || {
      echo 'managed-egress: health wait requires a positive bounded timeout' >&2
      return 2
    }
    # --wait implies detached up even when the caller omitted -d.
    if [[ "$command" == up ]]; then
      $COMPOSE_CMD up --detach "${args[@]}" || result=$?
    else
      $COMPOSE_CMD start "${args[@]}" || result=$?
    fi
  else
    $COMPOSE_CMD "${original[@]}" || result=$?
  fi
  managed_egress_reconcile || return 1
  [[ "$result" -eq 0 ]] || return "$result"
  if (( wait )); then
    # Compose start --wait also starts dependencies; inspect only the exact
    # selected containers so --no-deps cannot activate a stopped dependency.
    ids="$($COMPOSE_CMD ps --all -q -- "${services[@]}")" || return 1
    [[ -n "$ids" ]] || { echo 'managed-egress: no containers to wait for' >&2; return 1; }
    while IFS= read -r id; do
      [[ "$id" =~ ^[a-f0-9]{64}$ ]] || return 1
      containers+=("$id")
    done <<<"$ids"
    deadline=$((SECONDS + wait_timeout))
    result=1
    while (( SECONDS < deadline )); do
      states="$(docker inspect --format '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "${containers[@]}")" || break
      result=0
      while read -r state health; do
        case "$state:$health" in
          running:healthy|running:none) ;;
          *) result=1 ;;
        esac
      done <<<"$states"
      [[ "$result" -eq 0 ]] && break
      sleep 1
    done
    # A restart during the wait can replace a namespace without replacing its ID.
    managed_egress_reconcile || return 1
    [[ "$result" -eq 0 ]] || echo 'managed-egress: selected containers did not become running or healthy' >&2
    return "$result"
  fi
)

managed_compose_build() (
  cd "$DEPLOY_DIR" || return
  local mode image metadata
  mode="$(managed_egress_mode)" || return 1
  if [[ "$mode" == direct ]]; then
    voice_enforcement_rollout_guard_mutation "$@" || return 1
    $COMPOSE_CMD "$@"
    return
  fi
  managed_egress_gate verify || return
  image="$($COMPOSE_CMD config --images egress-build)" || return
  [[ "$image" == "$EGRESS_BUILD_IMAGE" ]] || {
    echo 'managed-egress: rendered builder image is not admitted' >&2
    return 1
  }
  managed_compose_mutate up -d --no-deps --no-build --pull never --wait --wait-timeout 90 egress-build >&2 || return
  metadata="$(mktemp -d)" || return
  trap 'rm -rf -- "$metadata"' EXIT
  export BUILDX_CONFIG="$metadata" BUILDX_BUILDER=concord-egress
  unset BUILDKIT_HOST
  docker buildx create --name concord-egress --driver remote --driver-opt default-load=true \
    docker-container://concord-egress-build >/dev/null || return
  managed_compose_mutate "$@"
)

control_plane_cli() (
  local mode running temporary
  mode="$(managed_egress_mode)" || return 1
  if [[ "$mode" == direct ]]; then
    cd "$DEPLOY_DIR" && $COMPOSE_CMD run --rm --no-deps --pull never control-plane ./main "$@"
    return
  fi
  managed_egress_gate verify || return 1
  running="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q control-plane)" || return 1
  if [[ "${1:-}" == voice-enforcement-rollout ]]; then
    running=''
  fi
  temporary=''
  if [[ -z "$running" ]]; then
    temporary="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD run --rm --detach --no-deps --pull never \
      --entrypoint /bin/sleep control-plane 2147483647)" || return 1
    [[ "$temporary" =~ ^[a-f0-9]{64}$ ]] || { echo 'managed-egress: invalid maintenance container identity' >&2; return 1; }
    trap 'result=$?; docker rm -f "$temporary" >/dev/null 2>&1 || result=1; managed_egress_reconcile >/dev/null 2>&1 || result=1; exit "$result"' EXIT
    trap 'exit 1' HUP INT TERM
    managed_egress_reconcile || return 1
  fi
  managed_egress_gate verify || return 1
  if [[ -n "$temporary" ]]; then
    docker exec -i "$temporary" ./main "$@"
  else
    cd "$DEPLOY_DIR" && $COMPOSE_CMD exec -T control-plane ./main "$@"
  fi
)

selfhost_storage_start_guard() {
  [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]] || return 0
  selfhost_storage_assert_no_running_minio
}

redis_managed_controller_host_identity() {
  local user_by_name user_by_id enumerated_user group_by_name group_by_id enumerated_group name password_field uid gid gecos home shell group_name group_gid members lock_status
  user_by_name="$(getent passwd redis-auth-controller 2>/dev/null)" || {
    echo 'redis-managed: dedicated locked host controller account is unavailable' >&2; return 1;
  }
  user_by_id="$(getent passwd 10004 2>/dev/null)" || {
    echo 'redis-managed: controller host UID lookup failed' >&2; return 1;
  }
  group_by_name="$(getent group redis-auth-controller 2>/dev/null)" || {
    echo 'redis-managed: dedicated host controller group is unavailable' >&2; return 1;
  }
  group_by_id="$(getent group 10004 2>/dev/null)" || {
    echo 'redis-managed: controller host GID lookup failed' >&2; return 1;
  }
  enumerated_user="$(getent passwd 2>/dev/null | awk -F: '$3 == "10004" { print }')" || {
    echo 'redis-managed: cannot enumerate host UID reservations' >&2; return 1;
  }
  enumerated_group="$(getent group 2>/dev/null | awk -F: '$3 == "10004" { print }')" || {
    echo 'redis-managed: cannot enumerate host GID reservations' >&2; return 1;
  }
  IFS=: read -r name password_field uid gid gecos home shell <<<"$user_by_name"
  IFS=: read -r group_name password_field group_gid members <<<"$group_by_name"
  [[ "$user_by_name" == "$user_by_id" && "$user_by_name" == "$enumerated_user" && \
     "$group_by_name" == "$group_by_id" && "$group_by_name" == "$enumerated_group" && \
     "$name" == redis-auth-controller && "$uid" == 10004 && "$gid" == 10004 && \
     "$group_name" == redis-auth-controller && "$group_gid" == 10004 && -z "$members" && \
     ( "$shell" == /usr/sbin/nologin || "$shell" == /sbin/nologin ) ]] || {
    echo 'redis-managed: host controller UID/GID must be reserved to its private non-login account' >&2; return 1;
  }
  lock_status="$(LC_ALL=C passwd -S redis-auth-controller 2>/dev/null)" || {
    echo 'redis-managed: cannot verify host controller password lock' >&2; return 1;
  }
  [[ "$lock_status" =~ ^redis-auth-controller[[:space:]]+(L|LK)[[:space:]] ]] || {
    echo 'redis-managed: host controller password is not locked' >&2; return 1;
  }
  # The app identity files are owned by the image UIDs, so no host account may
  # hold either one.
  local app_uid
  for app_uid in "$REDIS_CONTROL_UID" "$REDIS_MEDIA_UID"; do
    enumerated_user="$(getent passwd 2>/dev/null | awk -F: -v uid="$app_uid" '$3 == uid { print }')" || {
      echo 'redis-managed: cannot enumerate host UID reservations' >&2; return 1;
    }
    [[ -z "$enumerated_user" ]] || {
      echo 'redis-managed: a host account holds an app UID that owns a provider identity' >&2; return 1;
    }
  done
}

redis_managed_host_preflight() {
  [[ "$REDIS_CREDENTIAL_MODE" == managed ]] || return 0
  redis_managed_controller_host_identity || return 1
  redis_managed_marker_preflight "${REDIS_MANAGED_MARKER_MODE:-required}" || return 1
  if [[ "${REDIS_MANAGED_INITIAL_PREFLIGHT:-0}" == 1 ]]; then
    redis_managed_host_preflight_initial
    return $?
  fi
  command -v findmnt >/dev/null 2>&1 || {
    echo 'redis-managed: findmnt is required to prove the ACL bind is on host RAM' >&2
    return 1
  }
  local auth_dir=/run/concord/redis-auth fstype role uid gid slot_gid
  # findmnt cannot resolve a missing path, which used to read as a disk fallback.
  [[ -d "$auth_dir" ]] || {
    echo 'redis-managed: /run/concord/redis-auth is absent (host RAM lost?); see the lost-state recovery runbook' >&2
    return 1
  }
  fstype="$(findmnt -n -T "$auth_dir" -o FSTYPE 2>/dev/null || true)"
  [[ "$fstype" == tmpfs ]] || {
    echo 'redis-managed: /run/concord/redis-auth is not on tmpfs; refusing disk fallback' >&2
    return 1
  }
  [[ ! -L "$auth_dir" && "$(stat -c '%u:%g:%a' "$auth_dir" 2>/dev/null || true)" == '10004:0:2710' && \
     ! -L "$auth_dir/state" && "$(stat -c '%u:%a' "$auth_dir/state" 2>/dev/null || true)" == '10004:700' ]] || {
    echo 'redis-managed: controller RAM directories are not owned by the dedicated account' >&2; return 1;
  }
  for required in "$auth_dir/acl/users.acl" "$auth_dir/acks/control-plane" "$auth_dir/acks/media-plane" \
    "$auth_dir/acks/media-plane-blue" "$auth_dir/acks/media-plane-green" \
    /run/concord/redis-identities/controller/redis-infisical-client-secret \
    /run/concord/redis-identities/control-plane/redis-infisical-client-secret \
    /run/concord/redis-identities/media-plane/redis-infisical-client-secret; do
    [[ -e "$required" ]] || {
      echo "redis-managed: required preflight path is absent: $required" >&2
      return 1
    }
  done
  [[ -d "$auth_dir/acl" && -f "$auth_dir/acl/users.acl" ]] || {
    echo 'redis-managed: ACL directory and ACL file must exist' >&2
    return 1
  }
  [[ "$(stat -c '%u:%a' "$auth_dir/acks" 2>/dev/null || true)" == '10004:700' && ! -L "$auth_dir/acks" ]] || {
    echo 'redis-managed: ACK slot parent is not controller-private' >&2
    return 1
  }
  local slot
  for slot in control-plane media-plane media-plane-blue media-plane-green; do
    slot_gid="$(redis_managed_app_gid "$slot")" || return 1
    [[ "$slot_gid" =~ ^[1-9][0-9]*$ && "$slot_gid" != 10004 ]] || {
      echo 'redis-managed: inspected app GID is unsafe for an ACK slot' >&2; return 1;
    }
    [[ -d "$auth_dir/acks/$slot" && ! -L "$auth_dir/acks/$slot" && \
       "$(stat -c '%u:%g:%a' "$auth_dir/acks/$slot" 2>/dev/null || true)" == "10004:${slot_gid}:2710" ]] || {
      echo 'redis-managed: ACK slot ownership or isolation is invalid' >&2
      return 1
    }
  done
  for role in controller control-plane media-plane; do
    local identity_dir="/run/concord/redis-identities/${role}"
    local identity_file="${identity_dir}/redis-infisical-client-secret"
    [[ "$(findmnt -n -T "$identity_dir" -o FSTYPE 2>/dev/null || true)" == tmpfs ]] || {
      echo "redis-managed: ${role} identity directory is not on host RAM" >&2
      return 1
    }
    [[ ! -L "$identity_dir" && ! -L "$identity_file" ]] || {
      echo "redis-managed: ${role} identity path must not be a symlink" >&2
      return 1
    }
    uid=10004
    case "$role" in
      control-plane) uid="$REDIS_CONTROL_UID" ;;
      media-plane) uid="$REDIS_MEDIA_UID" ;;
    esac
    [[ "$(stat -c '%u:%a' "$identity_dir" 2>/dev/null)" == "${uid}:500" && \
       "$(stat -c '%u:%a' "$identity_file" 2>/dev/null)" == "${uid}:400" ]] || {
      echo "redis-managed: ${role} identity ownership/mode must match inspected UID and private read-only modes" >&2
      return 1
    }
  done
  [[ ! -L "$auth_dir/acl" && ! -L "$auth_dir/acl/users.acl" ]] || {
    echo 'redis-managed: ACL bind source must not be a symlink' >&2
    return 1
  }
  [[ "$(stat -c '%u:%g:%a' "$auth_dir/acl" 2>/dev/null || true)" == "10004:${REDIS_ACL_GID}:2710" ]] || {
    echo 'redis-managed: ACL directory must be controller-owned, Redis-group traversable, and private' >&2
    return 1
  }
  gid="$(stat -c '%u:%g:%a' "$auth_dir/acl/users.acl" 2>/dev/null || true)"
  [[ "$gid" == "10004:${REDIS_ACL_GID}:440" ]] || {
    echo 'redis-managed: ACL ownership/mode must match controller UID and inspected Redis GID' >&2
    return 1
  }
}

redis_managed_marker_preflight() {
  local mode="${1:-required}" dir=/var/lib/concord/redis-auth marker=/var/lib/concord/redis-auth/activated
  redis_managed_controller_host_identity || return 1
  [[ ! -L /var/lib/concord && ! -L "$dir" ]] || {
    echo 'redis-managed: persistent marker path is a symlink' >&2; return 1;
  }
  if [[ "$mode" == first || "$mode" == lost ]]; then
    [[ ! -e "$dir" || "$(stat -c '%u:%a' "$dir" 2>/dev/null || true)" == '10004:700' ]] || {
      echo 'redis-managed: legacy root-owned marker requires a separately reviewed migration' >&2; return 1;
    }
    install -d -o 10004 -g 10004 -m 00700 "$dir" || return 1
  fi
  [[ -d "$dir" && "$(stat -c '%u:%a' "$dir" 2>/dev/null || true)" == '10004:700' ]] || {
    echo 'redis-managed: persistent marker parent is not controller-private' >&2; return 1;
  }
  if [[ ! -e "$marker" && ! -L "$marker" ]]; then
    [[ "$mode" == first || "$mode" == lost ]] || {
      echo 'redis-managed: activated-host marker is missing; refusing ordinary startup' >&2; return 1;
    }
    return 0
  fi
  if [[ "$mode" == first ]]; then
    [[ ! -L "$marker" && -f "$marker" && "$(stat -c '%u:%a' "$marker" 2>/dev/null || true)" == '10004:600' ]] \
      && cmp -s "$marker" <(printf 'concord-managed-redis-initializing-v1\n') || {
        echo 'redis-managed: first activation marker is not an interrupted initialization' >&2; return 1;
      }
    return 0
  fi
  [[ ! -L "$marker" && -f "$marker" && \
     "$(stat -c '%u:%a:%s' "$marker" 2>/dev/null || true)" == '10004:600:25' && \
     "$(cat "$marker" 2>/dev/null || true)" == concord-managed-redis-v1 ]] || {
    echo 'redis-managed: activated-host marker is invalid or already present' >&2; return 1;
  }
}

redis_managed_app_gid() {
  local service="$1" model image image_id
  if [[ "$service" == media-plane* ]]; then
    if [[ "${MEDIA_DEPLOY_PROFILE:-media-single}" == media-bluegreen ]]; then
      service=media-plane-blue
    else
      service=media-plane
    fi
  fi
  model="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD --profile services config --format json)" || return 1
  # A build-only service (media-plane) has no image key; Compose tags it <project>-<service>.
  image="$(python3 -c 'import json,sys; m=json.load(sys.stdin); print(m["services"][sys.argv[1]].get("image") or m["name"] + "-" + sys.argv[1])' "$service" <<<"$model")" || return 1
  [[ -n "$image" ]] || return 1
  image_id="$(docker image inspect --format '{{.Id}}' "$image")" || return 1
  docker run --rm --network none --read-only --cap-drop ALL --security-opt no-new-privileges \
    --cpus 0.1 --memory 64m --entrypoint id "$image_id" -g appuser
}

# ── Managed TURN runtime (#3931) ─────────────────────────────────────────────
# The TURN shared key lives only in a dedicated host tmpfs and is written only
# by the turn-secret-writer container (UID/GID 10005). coturn (65534:65533) and
# the control plane mount their halves read-only. These paths are plain
# assignments so the isolated fixtures can retarget them; they are never read
# from the environment.
TURN_RUNTIME_ROOT=/run/concord/turn-runtime
TURN_RUNTIME_STATE_DIR=/var/lib/concord/turn-runtime
TURN_WRITER_ID=10005
TURN_COTURN_GID=65533

# The writer UID/GID must be reserved to a private, locked, non-login host
# account so no other principal can ever own the RAM key store (same posture as
# the Redis controller identity).
turn_managed_writer_host_identity() {
  local user_by_name user_by_id enumerated_user group_by_name group_by_id enumerated_group
  local name password_field uid gid gecos home shell group_name group_gid members lock_status
  user_by_name="$(getent passwd turn-secret-writer 2>/dev/null)" || {
    echo 'turn-managed: dedicated locked host writer account is unavailable' >&2; return 1;
  }
  user_by_id="$(getent passwd "$TURN_WRITER_ID" 2>/dev/null)" || {
    echo 'turn-managed: writer host UID lookup failed' >&2; return 1;
  }
  group_by_name="$(getent group turn-secret-writer 2>/dev/null)" || {
    echo 'turn-managed: dedicated host writer group is unavailable' >&2; return 1;
  }
  group_by_id="$(getent group "$TURN_WRITER_ID" 2>/dev/null)" || {
    echo 'turn-managed: writer host GID lookup failed' >&2; return 1;
  }
  enumerated_user="$(getent passwd 2>/dev/null | awk -F: -v id="$TURN_WRITER_ID" '$3 == id { print }')" || {
    echo 'turn-managed: cannot enumerate host UID reservations' >&2; return 1;
  }
  enumerated_group="$(getent group 2>/dev/null | awk -F: -v id="$TURN_WRITER_ID" '$3 == id { print }')" || {
    echo 'turn-managed: cannot enumerate host GID reservations' >&2; return 1;
  }
  IFS=: read -r name password_field uid gid gecos home shell <<<"$user_by_name"
  IFS=: read -r group_name password_field group_gid members <<<"$group_by_name"
  [[ "$user_by_name" == "$user_by_id" && "$user_by_name" == "$enumerated_user" && \
     "$group_by_name" == "$group_by_id" && "$group_by_name" == "$enumerated_group" && \
     "$name" == turn-secret-writer && "$uid" == "$TURN_WRITER_ID" && "$gid" == "$TURN_WRITER_ID" && \
     "$group_name" == turn-secret-writer && "$group_gid" == "$TURN_WRITER_ID" && -z "$members" && \
     ( "$shell" == /usr/sbin/nologin || "$shell" == /sbin/nologin ) ]] || {
    echo 'turn-managed: host writer UID/GID must be reserved to its private non-login account' >&2; return 1;
  }
  lock_status="$(LC_ALL=C passwd -S turn-secret-writer 2>/dev/null)" || {
    echo 'turn-managed: cannot verify host writer password lock' >&2; return 1;
  }
  [[ "$lock_status" =~ ^turn-secret-writer[[:space:]]+(L|LK)[[:space:]] ]] || {
    echo 'turn-managed: host writer password is not locked' >&2; return 1;
  }
}

# The control plane reads the active key through its image's GID. Read it from
# the SELECTED image rather than trusting a configured number (as the Redis
# preflight does). The GID must not alias the writer, coturn or root.
# With "build", an absent routine image is built first, as the start that
# follows would build it (a pruned host has none). A pinned checkpoint image
# (CONTROL_PLANE_IMAGE exported) is never built under its tag.
turn_managed_control_gid() {
  local build="${1:-}" model image image_id gid
  model="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD config --format json)" || {
    echo 'turn-managed: cannot render the Compose model to resolve the control-plane image' >&2; return 1;
  }
  image="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["services"]["control-plane"].get("image", ""))' <<<"$model")" || return 1
  [[ -n "$image" ]] || {
    echo 'turn-managed: selected control-plane image is unavailable in the Compose model' >&2; return 1;
  }
  if ! image_id="$(docker image inspect --format '{{.Id}}' "$image" 2>/dev/null)"; then
    [[ "$build" == build && -z "${CONTROL_PLANE_IMAGE:-}" ]] \
      && managed_compose_build build control-plane </dev/null >&2 \
      && image_id="$(docker image inspect --format '{{.Id}}' "$image")" || {
      echo 'turn-managed: selected control-plane image is unavailable locally' >&2; return 1;
    }
  fi
  gid="$(docker run --rm --network none --read-only --cap-drop ALL --security-opt no-new-privileges \
    --cpus 0.1 --memory 64m --entrypoint id "$image_id" -g appuser 2>/dev/null)" || {
    echo 'turn-managed: cannot resolve the selected control-plane image GID' >&2; return 1;
  }
  [[ "$gid" =~ ^[1-9][0-9]*$ && "$gid" -le 65535 \
     && "$gid" != "$TURN_WRITER_ID" && "$gid" != "$TURN_COTURN_GID" && "$gid" != 65534 ]] || {
    echo 'turn-managed: control-plane GID is unsafe for the active-key directory' >&2; return 1;
  }
  printf '%s\n' "$gid"
}

# turn_managed_path_check PATH KIND MODES-LIST... -- verifies a non-symlink
# directory or regular file whose "uid:gid:mode" is one of the listed values.
turn_managed_path_check() {
  local path="$1" kind="$2" actual
  shift 2
  [[ ! -L "$path" ]] || {
    echo "turn-managed: ${path} must not be a symlink" >&2; return 1;
  }
  if [[ "$kind" == dir ]]; then
    [[ -d "$path" ]] || { echo "turn-managed: required directory is absent: ${path}" >&2; return 1; }
  else
    [[ -f "$path" ]] || { echo "turn-managed: ${path} must be a regular file" >&2; return 1; }
  fi
  actual="$(stat -c '%u:%g:%a' "$path" 2>/dev/null || true)"
  local expected
  for expected in "$@"; do
    [[ "$actual" == "$expected" ]] && return 0
  done
  echo "turn-managed: ${path} ownership/mode ${actual:-unknown} does not match the RAM layout" >&2
  return 1
}

# Host RAM and ownership preflight. Runs before every writer operation and
# before coturn can start: a key store that is not on tmpfs, or whose
# ownership/modes are wrong, is refused rather than repaired.
turn_managed_host_preflight() {
  [[ "$TURN_CREDENTIAL_MODE" == managed ]] || return 0
  local active_gid="$1" root="$TURN_RUNTIME_ROOT" state_dir="$TURN_RUNTIME_STATE_DIR"
  local target fstype size writer="$TURN_WRITER_ID" coturn="$TURN_COTURN_GID"
  turn_managed_writer_host_identity || return 1
  command -v findmnt >/dev/null 2>&1 || {
    echo 'turn-managed: findmnt is required to prove the key store is on host RAM' >&2
    return 1
  }
  read -r target fstype size < <(findmnt -n -b -T "$root" -o TARGET,FSTYPE,SIZE 2>/dev/null || true) || true
  [[ "${fstype:-}" == tmpfs && "${target:-}" == "$root" ]] || {
    echo "turn-managed: ${root} is not a dedicated tmpfs mount; refusing disk fallback" >&2
    return 1
  }
  # The store is bounded to the reviewed 8 MiB; a larger tmpfs lets database or
  # journal growth take more host RAM than intended.
  if ! [[ "${size:-}" =~ ^[1-9][0-9]{0,8}$ ]] || (( size > 8388608 )); then
    echo "turn-managed: ${root} must be a tmpfs of at most 8 MiB (size=8m)" >&2
    return 1
  fi
  turn_managed_path_check "$root" dir '0:0:700' || return 1
  # db/ and active/ must be setgid (2750): the writer runs as 10005 with no
  # capabilities, so new files can only get the coturn / control-plane group by
  # inheriting it from the directory. Plain 750 is refused.
  turn_managed_path_check "$root/db" dir "${writer}:${coturn}:2750" || return 1
  turn_managed_path_check "$root/active" dir "${writer}:${active_gid}:2750" || return 1
  # The files are absent on a cold host; once present they carry the exact modes.
  if [[ -e "$root/db/turndb" || -L "$root/db/turndb" ]]; then
    turn_managed_path_check "$root/db/turndb" file "${writer}:${coturn}:640" || return 1
  fi
  if [[ -e "$root/active/turn-key" || -L "$root/active/turn-key" ]]; then
    turn_managed_path_check "$root/active/turn-key" file "${writer}:${active_gid}:440" || return 1
  fi
  [[ ! -L "${state_dir%/*}" ]] || {
    echo 'turn-managed: persistent state parent is a symlink' >&2; return 1;
  }
  turn_managed_path_check "$state_dir" dir "${writer}:${writer}:700" || return 1
  if [[ -e "$state_dir/state.json" || -L "$state_dir/state.json" ]]; then
    turn_managed_path_check "$state_dir/state.json" file "${writer}:${writer}:600" || return 1
  fi
}

# Resolve the control-plane GID and prove the host layout. Never reads stdin.
# An optional "build" is passed to turn_managed_control_gid.
turn_managed_prepare_writer() {
  [[ "$TURN_CREDENTIAL_MODE" == managed ]] || return 0
  local gid
  gid="$(turn_managed_control_gid "${1:-}" </dev/null)" || return 1
  turn_managed_host_preflight "$gid" </dev/null || return 1
  TURN_MANAGED_ACTIVE_GID="$gid"
}

# One writer invocation. The caller's stdin reaches the container untouched: the
# key never passes through a Bash variable, heredoc, here-string, argv or the
# environment. Only a fixed op name and an optional numeric --generation are
# ever placed on the command line.
turn_managed_exec_writer() {
  local -x TURN_ACTIVE_GID="${TURN_MANAGED_ACTIVE_GID:?}"
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD --profile turn-writer run --rm -T --no-deps turn-secret-writer "$@")
}

turn_managed_usage() {
  echo 'Usage: concord-ctl.sh turn-managed-{init-schema|status}' >&2
  echo '       concord-ctl.sh turn-managed-{bootstrap|add-candidate|promote|rollback|recover} [--generation N]' >&2
  echo '       concord-ctl.sh turn-managed-discard-unrecorded [--generation N]' >&2
  echo '       key material is read from standard input only (init-schema, status and discard-unrecorded read none)' >&2
}

turn_managed_verb_locked() {
  local verb="$1" op
  shift
  op="${verb#turn-managed-}"
  local -a args=()
  case "$op" in
    init-schema|status)
      [[ $# -eq 0 ]] || { turn_managed_usage; return 2; }
      ;;
    bootstrap|add-candidate|promote|rollback|recover|discard-unrecorded)
      if [[ $# -eq 2 && "$1" == --generation && "$2" =~ ^[0-9]{1,18}$ ]]; then
        args=(--generation "$2")
      elif [[ $# -ne 0 ]]; then
        turn_managed_usage; return 2
      fi
      ;;
    *) turn_managed_usage; return 2 ;;
  esac
  [[ "$TURN_CREDENTIAL_MODE" == managed ]] || {
    echo "turn-managed: ${verb} requires TURN_CREDENTIAL_MODE=managed in the canonical writer pair" >&2
    return 2
  }
  turn_managed_prepare_writer || return 1
  case "$op" in
    init-schema|status)
      turn_managed_exec_writer "$op" </dev/null
      ;;
    discard-unrecorded)
      # Deletes unpublished, unrecorded rows left by a failed bootstrap or
      # add-candidate. It takes no key: stdin is closed, never forwarded.
      turn_managed_exec_writer "$op" ${args[@]+"${args[@]}"} </dev/null
      ;;
    *)
      # `compose run -T` hands the container a pipe, so the writer's own
      # terminal check cannot see a terminal here; a typed key would echo.
      if [[ -t 0 ]]; then
        echo 'turn-managed: keys must be piped on stdin, never typed at a terminal' >&2
        return 2
      fi
      turn_managed_exec_writer "$op" ${args[@]+"${args[@]}"}
      ;;
  esac
}

# Managed `up`/restart: coturn must never start against a missing database
# (coturn-entrypoint.sh refuses it). init-schema is idempotent and never touches
# rows, so a restart over intact RAM keeps the key set and a cold reboot starts
# coturn with zero rows, authenticating nothing until an exact-set recover.
turn_managed_init_schema_before_coturn() {
  [[ "$TURN_CREDENTIAL_MODE" == managed ]] || return 0
  turn_managed_prepare_writer build || return 1
  managed_compose_build --profile turn-writer build turn-secret-writer </dev/null || {
    echo 'turn-managed: writer image build failed; refusing to start coturn' >&2
    return 1
  }
  turn_managed_exec_writer init-schema </dev/null
}

# `compose restart` keeps each container's creation-time config, so after a
# legacy-to-managed switch coturn and the control plane would keep the static
# secret path. A managed restart therefore requires both containers to match
# the effective model already; the first managed start goes through rebuild.
turn_managed_restart_guard() {
  local svc
  if [[ "$TURN_CREDENTIAL_MODE" != managed ]]; then
    # Switching back to legacy needs a rebuild too: restart would keep the
    # managed --userdb and TURN_SECRET_FILE.
    turn_managed_legacy_pair_clear ' and restart cannot remove it; use rebuild'
    return
  fi
  for svc in coturn control-plane; do
    turn_managed_runs_effective_config "$svc" || {
      echo "turn-managed: ${svc} does not run the managed model and restart cannot apply it; use rebuild" >&2
      return 1
    }
  done
}

# True only when exactly one container of SVC exists and carries the config
# hash Compose would create now. Any read failure is false, never true.
turn_managed_runs_effective_config() {
  local svc="$1" want id have
  want="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD config --hash "$svc" </dev/null)" || return 1
  want="${want##* }"
  id="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps -a -q "$svc" </dev/null)" || return 1
  [[ -n "$id" && "$id" != *$'\n'* ]] || return 1
  have="$(docker inspect --format '{{index .Config.Labels "com.docker.compose.config-hash"}}' "$id" </dev/null)" || return 1
  [[ -n "$want" && "$have" == "$want" ]]
}

# 0 when SVC's single container runs the managed model (coturn with --userdb,
# the control plane with TURN_SECRET_FILE), 1 when it does not, 2 when that
# cannot be read. The template prints only "managed", never an env value.
turn_managed_container_is_managed() {
  local svc="$1" id fmt out
  case "$svc" in
    coturn) fmt='{{range .Config.Cmd}}{{if eq (index (split . "=") 0) "--userdb"}}managed{{end}}{{end}}' ;;
    control-plane) fmt='{{range .Config.Env}}{{if eq (index (split . "=") 0) "TURN_SECRET_FILE"}}managed{{end}}{{end}}' ;;
    *) return 2 ;;
  esac
  id="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps -a -q "$svc" </dev/null)" || return 2
  [[ -n "$id" && "$id" != *$'\n'* ]] || return 2
  out="$(docker inspect --format "$fmt" "$id" </dev/null)" || return 2
  [[ "$out" == *managed* ]] && return 0
  return 1
}

# Legacy mode: 0 only when neither coturn nor the control plane still runs the
# managed model; an unreadable container counts as managed. Only a host
# provisioned for managed TURN has the RAM root, so other legacy hosts skip
# the check and are unchanged. HINT finishes the refusal message.
turn_managed_legacy_pair_clear() {
  local hint="$1" svc rc
  [[ -e "$TURN_RUNTIME_ROOT" ]] || return 0
  for svc in coturn control-plane; do
    rc=0
    turn_managed_container_is_managed "$svc" || rc=$?
    (( rc == 1 )) || {
      echo "turn-managed: ${svc} still runs the managed model${hint}" >&2
      return 1
    }
  done
}

# Lifecycle verbs other than rebuild and restart (Redis-managed prepare and
# rebuild, Activity History activate and rollback, the voice-enforcement
# deploy) can recreate the control plane alone. Under managed TURN they run
# only once both halves already run the managed model, so the first managed
# start stays an unscoped rebuild.
turn_managed_pair_guard() {
  if [[ "$TURN_CREDENTIAL_MODE" != managed ]]; then
    # Switching back to legacy: a control plane recreated alone would issue
    # static-secret credentials that a managed coturn rejects.
    turn_managed_legacy_pair_clear '; run an unscoped rebuild first'
    return
  fi
  local svc
  for svc in coturn control-plane; do
    turn_managed_container_is_managed "$svc" || {
      echo "turn-managed: ${svc} does not run the managed model; run an unscoped rebuild first" >&2
      return 1
    }
  done
}

# coturn and the control plane must change credential model together. A scoped
# rebuild of one of them succeeds only when the other, left untouched, already
# runs the selected model; a nonzero return means the pair would split. Only
# the peer is checked: the rebuilt service may differ for routine reasons, such
# as a new image tag.
turn_managed_scoped_rebuild_pair() {
  local has_coturn=0 has_cp=0 svc peer rc
  for svc in "$@"; do
    [[ "$svc" == coturn ]] && has_coturn=1
    [[ "$svc" == control-plane ]] && has_cp=1
  done
  (( has_coturn != has_cp )) || return 0
  peer=coturn
  (( has_coturn )) && peer=control-plane
  if [[ "$TURN_CREDENTIAL_MODE" == managed ]]; then
    turn_managed_runs_effective_config "$peer"
    return
  fi
  # Legacy: only a host provisioned for managed TURN has the RAM root, and
  # only there can the peer still be managed. Other legacy hosts skip the
  # check, so their routine scoped rebuilds are unchanged.
  [[ -e "$TURN_RUNTIME_ROOT" ]] || return 0
  rc=0
  turn_managed_container_is_managed "$peer" || rc=$?
  (( rc == 1 ))
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
REDIS_MANAGED_REBUILD_MARKER="${CONFIG_DEPLOY_DIR}/.redis-managed-rebuild.local"
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
  control_plane_cli activity-history "$verb" --confirm-drained
}

activity_history_start_control_plane() {
  (cd "$DEPLOY_DIR" && managed_compose_mutate up -d --no-deps --no-build --pull never control-plane)
}

activity_history_stop_control_plane() {
  (cd "$DEPLOY_DIR" && managed_compose_mutate stop --timeout 45 control-plane)
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
  # Validate arguments before the status probe starts any container.
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
    # The status probe starts a container, so it follows every static refusal.
    voice_enforcement_rollout_require_inactive_current_image || return 1
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
  # The status probe starts a container, so it follows every static refusal.
  voice_enforcement_rollout_require_inactive_current_image || return 1
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
    activate|deactivate|status|retire-boot|verify-retired-boot) ;;
    *)
      echo "voice-enforcement-rollout: invalid CLI verb '$verb'" >&2
      return 2
      ;;
  esac
  control_plane_cli voice-enforcement-rollout "$verb" "$@"
}

# Always resolve the privileged observer through the current selected source
# image, even when a resumable Activity History checkpoint pins an older one.
voice_enforcement_rollout_status() (
  unset CONTROL_PLANE_IMAGE
  if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]]; then
    selfhost_storage_select_image '' || return 1
  fi
  local observed
  observed="$(voice_enforcement_rollout_cli status --json)" || return 1
  printf '%s' "$observed" | python3 "$SCRIPT_DIR/voice-boot-containment.py" status "$@"
)

voice_enforcement_rollout_deactivate_for_rollback() {
  local activation
  activation="$(voice_enforcement_rollout_status --field familyActivation)" || {
    echo 'activity-history: unable to read voice-enforcement status with the current control-plane; refusing old-image rollback' >&2
    return 1
  }
  if [[ "$activation" != prepared ]]; then
    echo 'activity-history: immutable active family floor refuses older-image rollback' >&2
    return 1
  fi
  voice_enforcement_rollout_cli deactivate || {
    echo 'activity-history: unable to deactivate voice enforcement with the current control-plane; refusing old-image rollback' >&2
    return 1
  }
}

voice_enforcement_rollout_require_inactive_current_image() {
  local activation
  activation="$(voice_enforcement_rollout_status --field familyActivation)" || {
    echo 'activity-history: unable to verify inactive voice enforcement with the current control-plane; refusing old-image rollback' >&2
    return 1
  }
  [[ "$activation" == prepared ]] || {
    echo 'activity-history: immutable active family floor refuses older-image rollback' >&2
    return 1
  }
}

voice_enforcement_rollout_require_rollback_zero_state() {
  # Retained closures/retired fences are proof, not live liabilities. Never
  # substitute an inactive substring or a failed observation for exact counts.
  voice_enforcement_rollout_status --drained >/dev/null || {
    echo 'activity-history: current-image voice-enforcement zero-state evidence is missing or nonzero; refusing old-image rollback' >&2
    return 1
  }
  echo 'activity-history: current-image voice-enforcement zero-state verified; rollback evidence is single-host only (multi-host media is unsupported)'
}

# These are private per-invocation state, initialized here rather than accepted
# from the caller's environment. The deploy lock spans their entire lifetime.
VOICE_RESTART_AUTH_FD=''
VOICE_RETIRING_CONTAINER=''
VOICE_NEVER_STARTED_CANDIDATES=''

# A controlled --no-start create has no predecessor boot to retire. Bind its
# immutable identity separately from retired boots, and only admit first start
# while the exact candidate is still provably never started.
voice_enforcement_rollout_created_identity() {
  local project="$1" slot="$2" id="$3" inspected
  inspected="$(docker inspect --type container --format '{"Id":{{json .Id}},"Image":{{json .Image}},"Created":{{json .Created}},"RestartCount":{{json .RestartCount}},"State":{"Status":{{json .State.Status}},"Running":{{json .State.Running}},"Paused":{{json .State.Paused}},"Restarting":{{json .State.Restarting}},"Dead":{{json .State.Dead}},"Pid":{{json .State.Pid}},"StartedAt":{{json .State.StartedAt}},"FinishedAt":{{json .State.FinishedAt}}},"Labels":{"project":{{json (index .Config.Labels "com.docker.compose.project")}},"service":{{json (index .Config.Labels "com.docker.compose.service")}}}}' "$id")" || return 1
  [[ ${#inspected} -le 4096 ]] || return 1
  python3 -c '
import json,re,sys
def strict_object(pairs):
    result={}
    for key,value in pairs:
        if key in result: raise ValueError()
        result[key]=value
    return result
try:
    c=json.load(sys.stdin,object_pairs_hook=strict_object)
    project,slot,cid=sys.argv[1:]
    if set(c)!={"Id","Image","Created","RestartCount","State","Labels"}: raise ValueError()
    if c["Id"]!=cid or not re.fullmatch(r"[0-9a-f]{64}",cid): raise ValueError()
    if c["Labels"]!={"project":project,"service":slot}: raise ValueError()
    if not re.fullmatch(r"sha256:[0-9a-f]{64}",c["Image"]): raise ValueError()
    if not re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?Z",c["Created"]): raise ValueError()
    if type(c["RestartCount"]) is not int or c["RestartCount"]!=0: raise ValueError()
    state=c["State"]
    if type(state["Pid"]) is not int or any(type(state[k]) is not bool for k in ("Running","Paused","Restarting","Dead")): raise ValueError()
    if state!={"Status":"created","Running":False,"Paused":False,"Restarting":False,"Dead":False,"Pid":0,
               "StartedAt":"0001-01-01T00:00:00Z","FinishedAt":"0001-01-01T00:00:00Z"}: raise ValueError()
    print(json.dumps([project,slot,cid,c["Image"],c["Created"]],separators=(",",":")))
except (KeyError,TypeError,ValueError,RecursionError):
    raise SystemExit(1)
' "$project" "$slot" "$id" <<<"$inspected"
}

voice_enforcement_rollout_bind_created() {
  local slot="$1" inventory project id identity
  inventory="$(voice_enforcement_rollout_inventory)" || return 1
  project="${inventory%%$'\n'*}"
  case "$slot" in
    control-plane) ;;
    media-plane|media-plane-blue|media-plane-green)
      [[ $'\n'"${inventory#*$'\n'}"$'\n' == *$'\n'"$slot"$'\n'* ]] || return 1 ;;
    *) return 1 ;;
  esac
  id="$(voice_enforcement_rollout_slot_id "$project" "$slot")" || return 1
  identity="$(voice_enforcement_rollout_created_identity "$project" "$slot" "$id")" || return 1
  VOICE_NEVER_STARTED_CANDIDATES+=$'\n'"$identity"$'\n'
}

voice_enforcement_rollout_retire_named() {
  local slot="$1" inventory project configured found=0 id
  inventory="$(voice_enforcement_rollout_inventory)" || return 1
  project="${inventory%%$'\n'*}"
  while IFS= read -r configured; do
    [[ "$configured" != "$slot" ]] || found=1
  done <<<"${inventory#*$'\n'}"
  [[ "$found" -eq 1 ]] || return 1
  id="$(voice_enforcement_rollout_slot_id "$project" "$slot")" || return 1
  voice_enforcement_rollout_retire_slot "$project" "$slot" || return 1
}

# A successful SQL observation is always made with the current admitted image.
voice_enforcement_rollout_verify_retired() (
  unset CONTROL_PLANE_IMAGE
  if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]]; then
    selfhost_storage_select_image '' || return 1
  fi
  voice_enforcement_rollout_cli verify-retired-boot --node-boot-id "$1"
)

voice_enforcement_rollout_association() {
  local operation="$1" project="$2" slot="$3"
  shift 3
  python3 "$SCRIPT_DIR/voice-boot-containment.py" "$operation" \
    --lock-path "$DEPLOY_TRANSACTION_LOCK_PATH" --compose-project "$project" --slot "$slot" "$@" || {
    if [[ "$operation" == association-state ]]; then
      echo 'voice-enforcement-rollout: restart state unavailable or unsafe; install the trusted provisioning boundary before lifecycle operations' >&2
    fi
    return 1
  }
}

voice_enforcement_rollout_read_retired() {
  local project="$1" slot="$2" id="$3" record boot
  shift 3
  record="$(voice_enforcement_rollout_association association-read "$project" "$slot" --container-id "$id" "$@")" || return 1
  boot="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["nodeBootId"])' <<<"$record")" || return 1
  voice_enforcement_rollout_verify_retired "$boot" >/dev/null || return 1
  printf '%s\n' "$record"
}

voice_enforcement_rollout_guard_mutation() {
  local command="${1:-}" token inventory project slot ids id identity record selected all=0 i
  local -a named=() records=() record_slots=() record_ids=() allow_dead=()
  case "$command" in
    up|start|restart|stop|down|rm|kill) ;;
    *) return 0 ;;
  esac
  shift
  while (( $# )); do
    case "$1" in
      --wait-timeout|--pull|--timeout|-t|--scale|--attach|--no-attach|--exit-code-from)
        [[ $# -ge 2 ]] || return 1
        shift 2 ;;
      --) shift; named+=("$@"); break ;;
      -*) shift ;;
      *) named+=("$1"); shift ;;
    esac
  done
  [[ ${#named[@]} -gt 0 ]] || all=1
  selected=0
  for token in "${named[@]}"; do
    case "$token" in
      media-plane|media-plane-blue|media-plane-green) selected=1 ;;
    esac
  done
  (( all || selected )) || return 0
  case "$command" in
    up|down|rm) allow_dead=(--allow-dead) ;;
  esac
  inventory="$(voice_enforcement_rollout_inventory)" || return 1
  project="${inventory%%$'\n'*}"
  while IFS= read -r slot; do
    selected="$all"
    for token in "${named[@]}"; do
      [[ "$token" != "$slot" ]] || selected=1
    done
    (( selected )) || continue
    ids="$(docker ps -a --no-trunc --filter "label=com.docker.compose.project=$project" \
      --filter "label=com.docker.compose.service=$slot" --format '{{.ID}}')" || return 1
    [[ -n "$ids" ]] || continue
    [[ "$ids" =~ ^[0-9a-f]{64}$ ]] || return 1
    id="$ids"
    if [[ "$command" == stop && "$id" == "$VOICE_RETIRING_CONTAINER" ]]; then
      continue
    fi
    if [[ "$command" == start && "$all" -eq 0 ]] \
      && identity="$(voice_enforcement_rollout_created_identity "$project" "$slot" "$id")" \
      && [[ "$VOICE_NEVER_STARTED_CANDIDATES" == *$'\n'"$identity"$'\n'* ]]; then
      voice_enforcement_rollout_association association-invalidate "$project" "$slot" || return 1
      continue
    fi
    # Reading this anonymous pipe spends the capability in every shell copy.
    # It is issued only after durable consumption before a managed CP start.
    if [[ -n "$VOICE_RESTART_AUTH_FD" ]]; then
      [[ "$command" == start && "$all" -eq 0 && ${#named[@]} -eq 1 ]] || return 1
      IFS= read -r -t 1 record <&"$VOICE_RESTART_AUTH_FD" || return 1
      printf '%s' "$record" | python3 "$SCRIPT_DIR/voice-boot-containment.py" association-match \
        --compose-project "$project" --slot "$slot" --container-id "$id" || return 1
      continue
    fi
    record="$(voice_enforcement_rollout_read_retired "$project" "$slot" "$id" "${allow_dead[@]}")" || {
      echo "voice-enforcement-rollout: $slot lacks its exact retired association; refusing $command" >&2
      return 1
    }
    records+=("$record"); record_slots+=("$slot"); record_ids+=("$id")
  done <<<"${inventory#*$'\n'}"
  # Check every selected slot before consuming any, and consume all before the
  # first runtime mutation. Stop-only inspection retains the durable records.
  if [[ "$command" != stop ]]; then
    for (( i=0; i<${#records[@]}; i++ )); do
      voice_enforcement_rollout_association association-consume "$project" "${record_slots[i]}" \
        --container-id "${record_ids[i]}" "${allow_dead[@]}" <<<"${records[i]}" || return 1
    done
  fi
}

voice_enforcement_rollout_inventory() {
  local rendered
  rendered="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD config --format json)" || return 1
  printf '%s' "$rendered" | python3 "$SCRIPT_DIR/voice-boot-containment.py" inventory
}

voice_enforcement_rollout_slot_id() {
  local project="$1" slot="$2" ids
  ids="$(docker ps -a --no-trunc --filter "label=com.docker.compose.project=$project" \
    --filter "label=com.docker.compose.service=$slot" --format '{{.ID}}')" || return 1
  [[ "$ids" =~ ^[0-9a-f]{64}$ ]] || return 1
  printf '%s' "$ids"
}

# An optional second argument limits the probe to that one configured slot.
voice_enforcement_rollout_probe_slots() {
  local inventory project slot id observed expected active='' only="${2:-}"
  inventory="$(voice_enforcement_rollout_inventory)" || return 1
  project="${inventory%%$'\n'*}"
  if [[ "${1:-either}" == productive && "${inventory#*$'\n'}" != media-plane ]]; then
    active="$(media_bluegreen_redis GET media:active_colour)" || return 1
    [[ -n "$active" ]] || active=blue
    [[ "$active" == blue || "$active" == green ]] || return 1
  fi
  while IFS= read -r slot; do
    [[ -n "$slot" ]] || continue
    [[ -z "$only" || "$slot" == "$only" ]] || continue
    expected="${1:-either}"
    if [[ "$expected" == productive ]]; then
      expected=either
      [[ "$slot" != media-plane && "$slot" != "media-plane-$active" ]] || expected=true
    fi
    id="$(voice_enforcement_rollout_slot_id "$project" "$slot")" || return 1
    observed="$(python3 "$SCRIPT_DIR/voice-boot-containment.py" probe-container \
      --container-id "$id" --slot "$slot" --compose-project "$project" --grant "$expected")" || return 1
    [[ -n "$observed" ]] || return 1
  done <<<"${inventory#*$'\n'}"
}

voice_enforcement_rollout_activate() (
  local status activation inventory project slot id observed boot image artifact control_id control_image control_artifact receipt
  local -a evidence=()
  status="$(voice_enforcement_rollout_status)" || return 1
  activation="$(printf '%s' "$status" | python3 "$SCRIPT_DIR/voice-boot-containment.py" status --field familyActivation)" || return 1
  if [[ "$activation" == active ]]; then
    # Maintenance reproves capability and preserves the first committed receipt.
    voice_enforcement_rollout_probe_slots either
    return
  fi
  inventory="$(voice_enforcement_rollout_inventory)" || return 1
  project="${inventory%%$'\n'*}"
  control_id="$(docker ps --no-trunc --filter "label=com.docker.compose.project=$project" \
    --filter label=com.docker.compose.service=control-plane --format '{{.ID}}')" || return 1
  [[ "$control_id" =~ ^[0-9a-f]{64}$ ]] || return 1
  control_image="$(docker inspect --format '{{.Image}}' "$control_id")" || return 1
  control_artifact="$(python3 "$SCRIPT_DIR/voice-boot-containment.py" image-artifact "$control_image")" || return 1
  while IFS= read -r slot; do
    [[ -n "$slot" ]] || continue
    id="$(voice_enforcement_rollout_slot_id "$project" "$slot")" || return 1
    observed="$(python3 "$SCRIPT_DIR/voice-boot-containment.py" probe-container \
      --container-id "$id" --slot "$slot" --compose-project "$project" --grant false)" || return 1
    boot="${observed%%$'\n'*}"; image="${observed#*$'\n'}"
    artifact="$(python3 "$SCRIPT_DIR/voice-boot-containment.py" image-artifact "$image")" || return 1
    [[ "$(docker inspect --format '{{.Image}}' "$id")" == "$image" ]] || return 1
    evidence+=(--slot "$slot" "$artifact" "$boot")
  done <<<"${inventory#*$'\n'}"
  [[ "$(docker inspect --format '{{.Image}}' "$control_id")" == "$control_image" ]] || return 1
  receipt="$(mktemp)" || return 1
  trap 'rm -f -- "$receipt"' EXIT
  # Legacy SID normalization belongs to the serialized CP transaction. All
  # other live/pending counts must already be zero; retained proof is copied.
  status="$(voice_enforcement_rollout_status)" || return 1
  printf '%s' "$status" | python3 "$SCRIPT_DIR/voice-boot-containment.py" receipt \
    --control-plane-artifact "$control_artifact" "${evidence[@]}" >"$receipt" || return 1
  # /dev/stdin is the receipt-file path inside either the maintenance container
  # or the actual current-image exec. The private host tempfile is never guessed
  # to exist in another filesystem and contains only the frozen receipt schema.
  voice_enforcement_rollout_cli activate --confirm-drained --receipt-file /dev/stdin <"$receipt" || return 1
  [[ "$(voice_enforcement_rollout_status --field familyActivation)" == active ]]
)

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
  local timeout="${VOICE_ENFORCEMENT_HEALTH_TIMEOUT:-180}" deadline
  [[ "$timeout" =~ ^[1-9][0-9]*$ ]] || return 1
  deadline=$(( $(date +%s) + timeout ))
  while :; do
    voice_enforcement_rollout_probe_slots "${1:-either}" && return 0
    (( $(date +%s) < deadline )) || break
    sleep 2
  done
  echo 'voice-enforcement-rollout: every configured slot must prove protocol 4 transport' >&2
  return 1
}

voice_enforcement_rollout_retire_slot() (
  local project="$1" slot="$2" id boot capture_read capture_write capture_pid rc=0 value state record
  local -a evidence=()
  id="$(voice_enforcement_rollout_slot_id "$project" "$slot")" || {
    echo 'voice-enforcement-rollout: absent or ambiguous predecessor association' >&2
    return 1
  }
  voice_enforcement_rollout_association association-state "$project" "$slot" || return 1
  state="$(docker inspect --type container --format '{{.State.Status}}' "$id")" || return 1
  if [[ "$state" != running ]]; then
    # Truthful stopped history can only use the durable consumer; never capture
    # a new boot or reconstruct physical authority after its owner was lost.
    voice_enforcement_rollout_read_retired "$project" "$slot" "$id" --allow-dead >/dev/null
    return
  fi
  # One live owner retains its actual kernel descriptors across stop and the
  # CP commit. No pathname/inode/argv reconstruction can stand in for this.
  coproc VOICE_BOOT_CAPTURE { python3 "$SCRIPT_DIR/voice-boot-containment.py" hold-container \
    --container-id "$id" --slot "$slot" --compose-project "$project" --timeout-seconds 45 \
    --lock-path "$DEPLOY_TRANSACTION_LOCK_PATH"; }
  capture_read="${VOICE_BOOT_CAPTURE[0]}"; capture_write="${VOICE_BOOT_CAPTURE[1]}"; capture_pid="$VOICE_BOOT_CAPTURE_PID"
  trap 'exec {capture_write}>&-; exec {capture_read}<&-; kill "$capture_pid" 2>/dev/null || true; wait "$capture_pid" 2>/dev/null || true' EXIT
  IFS= read -r -t 10 boot <&"$capture_read" || {
    echo "voice-enforcement-rollout: $slot has no retirement route; refusing before stop (media left running)" >&2
    return 1
  }
  [[ "$boot" =~ ^[0-9a-f-]{36}$ ]] || return 1
  VOICE_RETIRING_CONTAINER="$id"
  (cd "$DEPLOY_DIR" && managed_compose_mutate stop --timeout 45 "$slot") || return 1
  voice_enforcement_rollout_verify_retired "$boot" || rc=$?
  case "$rc" in
    0) ;;
    3)
      printf 'verify-stopped\n' >&"$capture_write" || return 1
      for (( rc=0; rc<12; rc++ )); do
        IFS= read -r -t 10 value <&"$capture_read" || return 1
        evidence+=("$value")
      done
      voice_enforcement_rollout_cli retire-boot "${evidence[@]}" || return 1
      voice_enforcement_rollout_verify_retired "$boot" || return 1
      ;;
    *) echo 'voice-enforcement-rollout: retired-boot observation failed' >&2; return 1 ;;
  esac
  printf 'publish-stopped\n' >&"$capture_write" || return 1
  IFS= read -r -t 10 record <&"$capture_read" || return 1
  [[ ${#record} -le 4096 ]] || return 1
  printf 'release\n' >&"$capture_write" || return 1
  exec {capture_write}>&-
  wait "$capture_pid" || return 1
  exec {capture_read}<&-
  trap - EXIT
)

# No-mutation retirement preflight (#2272). Stopping a running slot is safe only
# when the stop can end in a published association: the boot commits its own
# protocol-4 fence during shutdown, or this controller holds the host-root
# capture the physical fallback needs. A protocol-3 predecessor reached through
# the non-root CD account has neither, and a stopped slot without an association
# can never be restarted, so every configured slot is checked before any stop.
# hold-container rechecks the same route, so a change after this check still
# refuses before its own stop.
voice_enforcement_rollout_preflight_slot() {
  local project="$1" slot="$2" id="$3" state
  state="$(docker inspect --type container --format '{{.State.Status}}' "$id")" || return 1
  if [[ "$state" != running ]]; then
    voice_enforcement_rollout_read_retired "$project" "$slot" "$id" --allow-dead >/dev/null || {
      echo "voice-enforcement-rollout: $slot lacks its exact retired association; refusing before any media stop" >&2
      return 1
    }
    return 0
  fi
  python3 "$SCRIPT_DIR/voice-boot-containment.py" retirement-preflight \
    --container-id "$id" --slot "$slot" --compose-project "$project" >/dev/null || {
    echo "voice-enforcement-rollout: $slot has no retirement route; refusing before any media stop (media left running)." \
      "Run the first protocol-4 cutover as host root; see [internal]deploys.md (#2272)" >&2
    return 1
  }
}

# Checks every configured slot without mutating anything. The rollout runs it
# before it replaces the control plane, so a slot that cannot retire refuses
# while both planes still run their previous images; stop_media rechecks it.
voice_enforcement_rollout_preflight_media() {
  local inventory project slot ids
  inventory="$(voice_enforcement_rollout_inventory)" || return 1
  project="${inventory%%$'\n'*}"
  while IFS= read -r slot; do
    [[ -n "$slot" ]] || continue
    voice_enforcement_rollout_association association-state "$project" "$slot" || return 1
    ids="$(docker ps -a --no-trunc --filter "label=com.docker.compose.project=$project" \
      --filter "label=com.docker.compose.service=$slot" --format '{{.ID}}')" || return 1
    [[ -z "$ids" || "$ids" =~ ^[0-9a-f]{64}$ ]] || return 1
    [[ -z "$ids" ]] || voice_enforcement_rollout_preflight_slot "$project" "$slot" "$ids" || return 1
  done <<<"${inventory#*$'\n'}"
}

voice_enforcement_rollout_stop_media() {
  local inventory project slot ids
  voice_enforcement_rollout_preflight_media || return 1
  inventory="$(voice_enforcement_rollout_inventory)" || return 1
  project="${inventory%%$'\n'*}"
  while IFS= read -r slot; do
    [[ -n "$slot" ]] || continue
    ids="$(docker ps -a --no-trunc --filter "label=com.docker.compose.project=$project" \
      --filter "label=com.docker.compose.service=$slot" --format '{{.ID}}')" || return 1
    # No configured runtime is a fresh start, never proof about a lost boot.
    [[ -n "$ids" ]] || continue
    voice_enforcement_rollout_retire_slot "$project" "$slot" || return 1
  done <<<"${inventory#*$'\n'}"
}

voice_enforcement_rollout_deploy() {
  [[ $# -eq 0 ]] || {
    echo "Usage: concord-ctl.sh voice-enforcement-rollout deploy" >&2
    return 2
  }
  local -a restore_pull=()
  [[ ${STAGE_RESTORE_MODE:-0} -eq 0 ]] || restore_pull=(--pull never)
  # Refuse an unretirable media slot before the control plane is replaced.
  voice_enforcement_rollout_preflight_media || return 1

  # The paired rollout always replaces the control-plane image.  Do not let a
  # stale Activity History rollback pin silently select an old digest here.
  clear_activity_history_image_selector || return 1

  # Build first so the new image owns migrations. Prepared denies productive
  # admission; an active family floor remains authoritative during maintenance.
  if [[ ${STAGE_RESTORE_MODE:-0} -eq 0 ]]; then
    echo "voice-enforcement-rollout: building the new control-plane image"
    managed_compose_build build control-plane || return 1
  fi
  # Migrations 000165–000166 change privacy and presence defaults. Stop and
  # drain the previous local binary before startup can apply them; older
  # writers must not run beside the new policy. Multi-host deployments must
  # drain the other replicas before invoking this rollout.
  echo "voice-enforcement-rollout: draining the previous control-plane"
  privacy_defaults_refuse_foreign_control_plane || return 1
  activity_history_stop_control_plane || return 1
  privacy_defaults_require_local_control_plane_drained || return 1
  echo "voice-enforcement-rollout: starting the current control-plane with its durable family floor"
  (cd "$DEPLOY_DIR" && managed_compose_mutate up -d --no-deps --no-build "${restore_pull[@]}" --wait \
    --wait-timeout 180 control-plane) || return 1
  voice_enforcement_rollout_wait_control_plane || return 1
  # Keep the old media serving while an uncached image build runs.
  if [[ ${STAGE_RESTORE_MODE:-0} -eq 0 ]]; then
    echo "voice-enforcement-rollout: building the new media-plane image"
    managed_compose_build build media-plane || return 1
  fi
  voice_enforcement_rollout_status >/dev/null || return 1

  # The old media container is explicitly stopped before its replacement is
  # created or started. A single compose up for CP+MP has ambiguous ordering
  # and can leave old sockets unregistered during the handoff.
  voice_enforcement_rollout_stop_media || return 1
  echo "voice-enforcement-rollout: starting the new media-plane"
  echo "voice-enforcement-rollout: waiting for media-plane liveness"
  (cd "$DEPLOY_DIR" && managed_compose_mutate up -d --no-deps --no-build "${restore_pull[@]}" --wait \
    --wait-timeout 180 media-plane) || return 1
  echo "voice-enforcement-rollout: waiting for media-plane protocol 4 transport capability"
  voice_enforcement_rollout_wait_media || return 1

  echo "voice-enforcement-rollout: activating durable enforcement"
  voice_enforcement_rollout_activate || return 1
  voice_enforcement_rollout_wait_media productive || return 1
  echo "voice-enforcement-rollout: completed"
}

# First protocol-4 cutover from a protocol-3 media predecessor (#2272). The
# released protocol-3 image reports no boot identity, so neither retirement
# route can bind it and the rollout preflight refuses even as root. A database
# that has never applied migration 000194 holds no protocol-4 boot fence and no
# family authority, so a drained, truthfully stopped legacy container leaves
# nothing to retire. Host root removes exactly that container; the rollout then
# treats the empty slot as a fresh start.
voice_enforcement_rollout_legacy_schema() {
  local row
  row="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD exec -T postgres sh -c \
    'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -At -c "SELECT version, dirty, pg_catalog.to_regclass('"'"'public.login_family_rollout'"'"') IS NULL, (SELECT pg_catalog.count(*) FROM public.voice_enforcement_sessions) FROM public.schema_migrations"')" || {
    echo 'voice-enforcement-rollout: schema observation failed; refusing' >&2
    return 1
  }
  if [[ ! "$row" =~ ^([0-9]+)\|f\|t\|([0-9]+)$ ]] || (( BASH_REMATCH[1] >= 194 )); then
    echo 'voice-enforcement-rollout: migration 000194 is applied, dirty or unreadable; a protocol-4 boot may exist, so the legacy predecessor cannot be removed' >&2
    return 1
  fi
  printf '%s\n' "${BASH_REMATCH[2]}"
}

voice_enforcement_rollout_retire_legacy() {
  local value window t0 deadline state first second reason inventory project id identity sessions
  [[ "$(id -u)" -eq 0 ]] || {
    echo 'voice-enforcement-rollout: retire-legacy-predecessor requires host root' >&2
    return 1
  }
  [[ "$MEDIA_DEPLOY_PROFILE" == media-single ]] || {
    echo 'voice-enforcement-rollout: retire-legacy-predecessor supports only the media-single profile' >&2
    return 1
  }
  voice_enforcement_rollout_legacy_schema >/dev/null || return 1
  inventory="$(voice_enforcement_rollout_inventory)" || return 1
  project="${inventory%%$'\n'*}"
  [[ "${inventory#*$'\n'}" == media-plane ]] || return 1
  voice_enforcement_rollout_association association-state "$project" media-plane || return 1
  state="$(media_legacy_state)" || return 1
  case "$state" in
    absent)
      echo 'voice-enforcement-rollout: no legacy media-plane container; nothing to retire'
      return 0 ;;
    running)
      # Only a proved join pause may end calls: the slot is empty, or the
      # pause's own cutoff has passed.
      value="$(media_bluegreen_redis GET "$MEDIA_LEGACY_PAUSE_KEY")" || return 1
      window="$(media_legacy_parse_pause "$value")" || {
        echo 'voice-enforcement-rollout: run media-legacy-pause and drain before retiring legacy media' >&2
        return 1
      }
      read -r t0 deadline <<<"$window"
      first="$(media_legacy_counts)" || first=''
      second=''
      if [[ "$first" == '0 0' ]]; then
        sleep 2
        second="$(media_legacy_counts)" || second=''
      fi
      if [[ "$first" == '0 0' && "$second" == '0 0' ]]; then
        reason=empty
      elif (( $(date +%s) >= deadline )); then
        reason=deadline
      else
        echo "voice-enforcement-rollout: legacy media is not drained (counts ${second:-${first:-unknown}}); refusing before stop" >&2
        return 1
      fi
      # The mutation guard admits a stop without a retired association only
      # for the exact container this owner is retiring.
      VOICE_RETIRING_CONTAINER="$(voice_enforcement_rollout_slot_id "$project" media-plane)" || return 1
      (cd "$DEPLOY_DIR" && managed_compose_mutate stop --timeout 45 media-plane) || return 1
      VOICE_RETIRING_CONTAINER=''
      echo "voice-enforcement-rollout: stopped legacy media reason=${reason} t0=${t0}" ;;
    exited) ;;
    *) return 1 ;;
  esac
  id="$(voice_enforcement_rollout_slot_id "$project" media-plane)" || return 1
  identity="$(python3 "$SCRIPT_DIR/voice-boot-containment.py" stopped-identity \
    --container-id "$id" --slot media-plane --compose-project "$project")" || return 1
  # Recheck after the stop: the schema must still predate 000194, and the
  # stopped protocol-3 boot must have released every enforcement session,
  # because activation refuses while any remain. Keep the container on refusal
  # so `compose start media-plane` and media-legacy-resume roll back.
  sessions="$(voice_enforcement_rollout_legacy_schema)" || return 1
  [[ "$sessions" == 0 ]] || {
    echo "voice-enforcement-rollout: ${sessions} voice enforcement sessions remain after the legacy stop; refusing to remove it" >&2
    return 1
  }
  docker rm "$id" >/dev/null || return 1
  echo "voice-enforcement-rollout: removed legacy media predecessor ${identity}"
  echo 'voice-enforcement-rollout: next: media-legacy-resume, then voice-enforcement-rollout deploy as host root'
}

voice_enforcement_rollout_media_rebuild() {
  voice_enforcement_rollout_preflight_media || return 1
  managed_compose_build build media-plane || return 1
  voice_enforcement_rollout_status >/dev/null || return 1
  voice_enforcement_rollout_stop_media || return 1
  (cd "$DEPLOY_DIR" && managed_compose_mutate up -d --no-deps --no-build --wait \
    --wait-timeout 180 media-plane) || return 1
  voice_enforcement_rollout_wait_media || return 1
  voice_enforcement_rollout_activate || return 1
  voice_enforcement_rollout_wait_media productive || return 1
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
  managed_compose_build build --quiet "$service" || {
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
  if [[ "$REDIS_CREDENTIAL_MODE" == managed ]]; then
    local request response
    request="$(python3 -c 'import json,sys; print(json.dumps({"operation":"redis","args":sys.argv[1:]}, separators=(",",":")))' "$@")" || return 1
    response="$(printf '%s\n' "$request" | (cd "$DEPLOY_DIR" && $COMPOSE_CMD exec -T --user 0 redis-auth-controller redis-auth-controller request))" || {
      echo 'media-bluegreen: managed Redis operator request failed; refusing state mutation' >&2
      return 1
    }
    python3 -c '
import json, sys
try:
    raw = sys.stdin.read()
    if len(raw) > 65536:
        raise ValueError("response too large")
    value = json.loads(raw)
    if not isinstance(value, dict) or set(value) != {"output"} or not isinstance(value["output"], str):
        raise ValueError("invalid response")
    sys.stdout.write(value["output"])
except (ValueError, TypeError, json.JSONDecodeError):
    raise SystemExit("media-bluegreen: invalid managed Redis response; refusing")
' <<<"$response"
    return $?
  fi
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
  local inventory project id observed boot
  inventory="$(voice_enforcement_rollout_inventory)" || return 1
  project="${inventory%%$'\n'*}"
  id="$(voice_enforcement_rollout_slot_id "$project" "media-plane-$colour")" || return 1
  observed="$(python3 "$SCRIPT_DIR/voice-boot-containment.py" probe-container \
    --container-id "$id" --slot "media-plane-$colour" --compose-project "$project" --grant either)" || return 1
  boot="${observed%%$'\n'*}"
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
             and value.get("nodeBootId") == sys.argv[6]
             and value.get("ready") is (sys.argv[3] == "true")
             and type(value.get("voiceEnforcementProtocol")) is int
             and value["voiceEnforcementProtocol"] == 4
             and value.get("voiceEnforcementTransportOK") is True
             and type(value.get("voiceEnforcementAdmissionGranted")) is bool
             and (sys.argv[3] == "false" or value.get("voiceEnforcementAdmissionGranted") is True)
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
' "$body" "$colour" "$expected_accept" "$status" "$expected_empty" "$boot"; then
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

# Activation proves every configured slot, so a first promotion's slot
# preparation runs both colours. Retire the colour that promotion does not
# use, so the single-standby preflight sees the state it expects.
media_bluegreen_retire_unpromoted() {
  local active ids inventory
  active="$(media_bluegreen_redis GET media:active_colour)" || return 1
  [[ -z "$active" ]] || return 0
  ids="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q media-plane-green)" || return 1
  [[ -n "$ids" ]] || return 0
  inventory="$(voice_enforcement_rollout_inventory)" || return 1
  voice_enforcement_rollout_retire_slot "${inventory%%$'\n'*}" media-plane-green
}

voice_enforcement_rollout_prepare_slots() {
  local activation inventory slot
  local -a slots=()
  activation="$(voice_enforcement_rollout_status --field familyActivation)" || return 1
  [[ "$activation" == prepared ]] || return 0
  inventory="$(voice_enforcement_rollout_inventory)" || return 1
  voice_enforcement_rollout_stop_media || return 1
  while IFS= read -r slot; do
    [[ -z "$slot" ]] || slots+=("$slot")
  done <<<"${inventory#*$'\n'}"
  [[ ${#slots[@]} -gt 0 ]] || return 1
  managed_compose_build build "${slots[@]}" || return 1
  (cd "$DEPLOY_DIR" && managed_compose_mutate up -d --no-deps --no-build --wait --wait-timeout 180 "${slots[@]}") || return 1
  voice_enforcement_rollout_wait_media || return 1
  voice_enforcement_rollout_activate || return 1
  voice_enforcement_rollout_wait_media productive || return 1
  media_bluegreen_retire_unpromoted || return 1
}

media_bluegreen_cutover_locked() {
  local active standby old_colour active_port standby_port tombstone old_generation status deadline running drain_started result
  # Refuse before a prepared family's slot preparation stops any media; the
  # second preflight re-reads the state that preparation replaced.
  media_bluegreen_preflight >/dev/null || return 1
  voice_enforcement_rollout_prepare_slots || return 1
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
    managed_compose_build up -d --build --no-deps --wait \
      --wait-timeout 180 "media-plane-${standby}" || return 1
  fi
  if ! media_bluegreen_probe_ready "$standby" true "$standby_port" 1 true; then
    echo "media-bluegreen: standby ${standby} failed its identity, worker, or readiness gate" >&2
    return 1
  fi
  drain_started="$(date +%s)" || return 1
  [[ "$drain_started" =~ ^[1-9][0-9]{9,14}$ ]] || {
    echo 'media-bluegreen: cannot record the drain deadline; refusing promotion' >&2
    return 1
  }

  if [[ -n "$active_port" ]]; then
    voice_enforcement_rollout_probe_slots either || return 1
  else
    # Slot preparation retired the colour a first promotion does not use, so
    # only the standby runs; probing the exited colour would always refuse.
    voice_enforcement_rollout_probe_slots either "media-plane-${standby}" || return 1
  fi
  if ! media_bluegreen_redis SADD media:colours:registered "$standby" >/dev/null; then
    echo 'media-bluegreen: registry write failed before promotion; active color is unchanged' >&2
    return 1
  fi
  if [[ -n "$active_port" ]]; then
    # Promotion and its deadline are one Redis transaction, including on crash.
    if ! result="$(media_bluegreen_redis EVAL '
      if redis.call("GET", KEYS[1]) ~= ARGV[1] then return 0 end
      redis.call("SET", KEYS[1], ARGV[2])
      redis.call("SET", KEYS[2], ARGV[3])
      return 1
    ' 2 media:active_colour "media:colour_draining:${old_colour}" "$old_colour" "$standby" "${drain_started}:${old_generation}")"; then
      active="$(media_bluegreen_redis GET media:active_colour)" || return 1
      tombstone="$(media_bluegreen_redis GET "media:colour_draining:${old_colour}")" || return 1
      [[ "$active" == "$standby" && "$tombstone" == "${drain_started}:${old_generation}" ]] || {
        echo 'media-bluegreen: promotion and drain deadline were not confirmed' >&2
        return 1
      }
    elif [[ "$result" != 1 ]]; then
      echo 'media-bluegreen: active color changed before atomic promotion; refusing cutover' >&2
      return 1
    fi
  elif ! media_bluegreen_redis SET media:active_colour "$standby" >/dev/null; then
    # The first colour has no prior colour to drain; resolve an unacknowledged SET.
    active="$(media_bluegreen_redis GET media:active_colour)" || return 1
    [[ "$active" == "$standby" ]] || {
      echo 'media-bluegreen: first-colour promotion was not confirmed' >&2
      return 1
    }
  fi
  if [[ -n "$active_port" ]]; then
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
  local colour="$1" active port body status rooms creates result generation tombstone started_at now
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
      (cd "$DEPLOY_DIR" && managed_compose_mutate rm -f "media-plane-${colour}") || return 1
      result="$(media_bluegreen_redis DEL "media:colour_draining:${colour}")" || return 1
      [[ "$result" =~ ^[01]$ ]] || return 1
      echo "media-bluegreen: resumed reap of stopped color ${colour}"
      return 0
    fi
  fi
  tombstone="$(media_bluegreen_redis GET "media:colour_draining:${colour}")" || return 1
  started_at=''
  if [[ "$tombstone" =~ ^([1-9][0-9]{9,14}):[^[:space:]]+$ ]]; then
    started_at="${BASH_REMATCH[1]}"
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
    [[ -n "$started_at" ]] || {
      echo "media-bluegreen: ${colour} has no valid drain deadline; refusing automatic cutoff" >&2
      return 1
    }
    now="$(date +%s)" || return 1
    [[ "$now" =~ ^[1-9][0-9]{9,14}$ ]] && (( now >= started_at )) || {
      echo "media-bluegreen: ${colour} drain clock is invalid; refusing automatic cutoff" >&2
      return 1
    }
    if (( now - started_at < 7200 )); then
      echo 'media-bluegreen: drain grace period remains; reap skipped' >&2
      return 2
    fi
    # A live room can renew its marker until stopped, so sweep only afterward.
    if ! media_bluegreen_probe_ready "$active" true "$(media_bluegreen_port "$active")"; then
      echo "media-bluegreen: active ${active} is not ready; refusing deadline cutoff" >&2
      return 1
    fi
    result="$(media_bluegreen_redis SISMEMBER media:colours:registered "$active")" || return 1
    [[ "$result" == 1 ]] || return 1
    result="$(media_bluegreen_redis SREM media:colours:registered "$colour")" || return 1
    [[ "$result" =~ ^[01]$ ]] || return 1
    echo "media-bluegreen: ${colour} reached the 2-hour deadline; disconnecting remaining rooms"
    if ! voice_enforcement_rollout_retire_named "media-plane-${colour}"; then
      echo "media-bluegreen: deadline stop failed; ${colour} remains fenced for retry" >&2
      return 1
    fi
    media_bluegreen_sweep_markers "$colour" "$active" || return 1
    (cd "$DEPLOY_DIR" && managed_compose_mutate rm -f "media-plane-${colour}") || return 1
    result="$(media_bluegreen_redis DEL "media:colour_draining:${colour}")" || return 1
    [[ "$result" =~ ^[01]$ ]] || return 1
    echo "media-bluegreen: reaped ${colour} after the deadline"
    return 0
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
    rm -f "$body"
    echo "media-bluegreen: ${colour} remains running" >&2
    return 2
  fi
  rm -f "$body"
  if ! voice_enforcement_rollout_retire_named "media-plane-${colour}"; then
    echo "media-bluegreen: stop failed; ${colour} remains fenced; rerun media-reap to resume" >&2
    return 1
  fi
  (cd "$DEPLOY_DIR" && managed_compose_mutate rm -f "media-plane-${colour}") || return 1
  result="$(media_bluegreen_redis DEL "media:colour_draining:${colour}")" || return 1
  [[ "$result" =~ ^[01]$ ]] || return 1
  echo "media-bluegreen: reaped drained color ${colour}"
}

# ── First legacy-to-colour conversion (#2466) ──────────────────────────────
# The control plane refuses new voice admissions while this key exists; it
# never writes it. Value: `<t0>:<deadline>[:<test-user-uuid>]`.
readonly MEDIA_LEGACY_PAUSE_KEY='media:legacy_join_pause'
readonly MEDIA_LEGACY_CUTOFF_SECS=7200

media_legacy_staging() {
  [[ "$COMPOSE_OVERRIDE" == docker-compose.staging.yml \
     || "$COMPOSE_OVERRIDE" == docker-compose.west-staging.yml ]]
}

# Production's window is fixed. Staging may shorten it to rehearse the cutoff.
media_legacy_window_valid() {
  local window="$1"
  (( window == MEDIA_LEGACY_CUTOFF_SECS )) && return 0
  media_legacy_staging && (( window >= 60 && window < MEDIA_LEGACY_CUTOFF_SECS ))
}

# Prints "<t0> <deadline>" for a well-formed pause value.
media_legacy_parse_pause() {
  [[ "$1" =~ ^([0-9]{1,12}):([0-9]{1,12})(:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})?$ ]] \
    || return 1
  local t0="${BASH_REMATCH[1]}" deadline="${BASH_REMATCH[2]}"
  media_legacy_window_valid "$(( deadline - t0 ))" || return 1
  echo "$t0 $deadline"
}

# running | exited | absent; anything else fails.
media_legacy_state() {
  local rows
  rows="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --all --format json -- media-plane)" || return 1
  python3 -c '
import json, sys
raw = sys.stdin.read().strip()
try:
    rows = json.loads(raw) if raw.startswith("[") else [json.loads(line) for line in raw.splitlines() if line.strip()]
except ValueError:
    raise SystemExit(1)
if not rows:
    print("absent")
elif len(rows) == 1 and isinstance(rows[0], dict) and rows[0].get("Service") == "media-plane" \
        and rows[0].get("State") in ("running", "exited"):
    print(rows[0]["State"])
else:
    raise SystemExit(1)
' <<<"$rows"
}

# Prints "<rooms> <creates>" from legacy /health; any doubt is a failure.
media_legacy_counts() {
  local body
  body="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD exec -T media-plane \
    node -e 'fetch("http://127.0.0.1:3000/health", {signal: AbortSignal.timeout(3000)}).then(async response => { process.stdout.write(await response.text()); process.exitCode = response.ok ? 0 : 1; }).catch(() => { process.exitCode = 1; });' 2>/dev/null)" \
    || return 1
  python3 -c '
import json, sys
try:
    value = json.loads(sys.argv[1])
    rooms, creates = value["rooms"], value["creates_in_flight"]
    if type(rooms) is not int or type(creates) is not int or rooms < 0 or creates < 0:
        raise ValueError
except (KeyError, TypeError, ValueError):
    raise SystemExit(1)
print(rooms, creates)
' "$body"
}

# The every-minute actuator. Stops legacy at zero rooms and zero creates, or at
# the deadline whatever the counts; never enables a colour or lifts the pause.
media_legacy_actuate() {
  local value window t0 deadline state now first second rooms creates reason
  value="$(media_bluegreen_redis GET "$MEDIA_LEGACY_PAUSE_KEY")" || return 1
  if [[ -z "$value" ]]; then
    echo 'media-bluegreen: legacy profile selected; no colour to reap'
    return 0
  fi
  window="$(media_legacy_parse_pause "$value")" || {
    echo 'media-legacy: join pause value is malformed; refusing to act' >&2
    return 1
  }
  read -r t0 deadline <<<"$window"
  state="$(media_legacy_state)" || {
    echo 'media-legacy: cannot read the legacy container state' >&2
    return 1
  }
  case "$state" in
    exited)
      echo 'media-legacy: legacy stopped; awaiting colour activation'
      return 0 ;;
    running) ;;
    *)
      echo "media-legacy: legacy container is ${state}; refusing to act" >&2
      return 1 ;;
  esac
  now="$(date +%s)"
  first="$(media_legacy_counts)" || first=''
  second=''
  if [[ "$first" == '0 0' ]]; then
    # A create landing between two field reads must not look empty.
    sleep 2
    second="$(media_legacy_counts)" || second=''
  fi
  if [[ "$first" == '0 0' && "$second" == '0 0' ]]; then
    reason=empty
  elif (( now >= deadline )); then
    reason=deadline
  elif [[ -z "$first" ]]; then
    echo "media-legacy: counts unknown, $(( deadline - now ))s to cutoff" >&2
    return 1
  else
    read -r rooms creates <<<"${second:-$first}"
    echo "media-legacy: rooms=${rooms} creates_in_flight=${creates} remaining=$(( deadline - now ))s"
    return 0
  fi
  read -r rooms creates <<<"${second:-${first:-unknown unknown}}"
  voice_enforcement_rollout_stop_media || {
    echo 'media-legacy: legacy stop failed; retrying next run' >&2
    return 1
  }
  echo "media-legacy: stopped legacy reason=${reason} rooms=${rooms} creates_in_flight=${creates} t0=${t0} at=${now}"
  mkdir -p "${CONFIG_ROOT}/logs" 2>/dev/null || true
  printf 'legacy media stopped (%s) at %s; activate the colour\n' "$reason" "$now" \
    >"${CONFIG_ROOT}/logs/media-legacy-stopped-alert.flag" 2>/dev/null || true
}

# Arms the pause, then proves the control plane sees it before the clock counts.
media_legacy_pause() {
  local allow_user="${1:-}" value current now window body result
  [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" != selfhost ]] || {
    echo 'media-legacy: the join pause applies only to a managed host' >&2
    return 1
  }
  # The test-user exception is added after the colour profile is enabled.
  if [[ -n "$allow_user" ]]; then
    current="$(media_bluegreen_redis GET "$MEDIA_LEGACY_PAUSE_KEY")" || return 1
    media_legacy_parse_pause "$current" >/dev/null || {
      echo 'media-legacy: no valid pause to add a test user to' >&2
      return 1
    }
    # T0 and the deadline are kept byte for byte; only the exception changes.
    value="$(cut -d: -f1-2 <<<"$current"):${allow_user}"
    media_legacy_parse_pause "$value" >/dev/null || {
      echo 'media-legacy: test user must be a lowercase UUID' >&2
      return 1
    }
    result="$(media_bluegreen_redis SET "$MEDIA_LEGACY_PAUSE_KEY" "$value" XX)" || return 1
    [[ "$result" == OK ]] || return 1
    echo "media-legacy: test user ${allow_user} may join; deadline unchanged"
    return 0
  fi
  [[ "${MEDIA_DEPLOY_PROFILE:-media-single}" != media-bluegreen ]] || {
    echo 'media-legacy: a new join pause applies only to a legacy host' >&2
    return 1
  }
  window="${MEDIA_LEGACY_CUTOFF_SECONDS:-$MEDIA_LEGACY_CUTOFF_SECS}"
  [[ "$window" =~ ^[0-9]{1,5}$ ]] && media_legacy_window_valid "$window" || {
    echo "media-legacy: cutoff window ${window}s is not allowed here" >&2
    return 1
  }
  now="$(date +%s)"
  value="${now}:$(( now + window ))"
  result="$(media_bluegreen_redis SET "$MEDIA_LEGACY_PAUSE_KEY" "$value" NX)" || return 1
  [[ "$result" == OK ]] || {
    echo 'media-legacy: a join pause already exists; refusing to restart its clock' >&2
    return 1
  }
  body="$(mktemp)" || return 1
  for _ in $(seq 1 30); do
    if curl -sS --max-time 2 -o "$body" http://127.0.0.1:8080/voice-join-pause 2>/dev/null \
      && python3 -c '
import json, sys
value = json.load(open(sys.argv[1], encoding="utf-8"))["voice_join_pause"]
raise SystemExit(0 if value["active"] is True and value["deadline"] == int(sys.argv[2]) else 1)
' "$body" "$(( now + window ))" 2>/dev/null; then
      rm -f "$body"
      echo "media-legacy: joins paused T0=$(date -u -d "@${now}" '+%Y-%m-%dT%H:%M:%SZ') cutoff=$(date -u -d "@$(( now + window ))" '+%Y-%m-%dT%H:%M:%SZ')"
      return 0
    fi
    sleep 2
  done
  rm -f "$body"
  media_legacy_resume_value "$value" >/dev/null || true
  echo 'media-legacy: control plane did not confirm the pause; removed it' >&2
  return 1
}

# Compare-and-delete: only the value this run read is removed.
media_legacy_resume_value() {
  local result
  result="$(media_bluegreen_redis EVAL \
    "if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end return -1" \
    1 "$MEDIA_LEGACY_PAUSE_KEY" "$1")" || return 1
  [[ "$result" == 1 ]]
}

media_legacy_resume() {
  local value
  value="$(media_bluegreen_redis GET "$MEDIA_LEGACY_PAUSE_KEY")" || return 1
  if [[ -z "$value" ]]; then
    echo 'media-legacy: no join pause to lift'
    return 0
  fi
  media_legacy_resume_value "$value" || {
    echo 'media-legacy: pause changed while lifting it; nothing removed' >&2
    return 1
  }
  rm -f "${CONFIG_ROOT}/logs/media-legacy-stopped-alert.flag" 2>/dev/null || true
  echo 'media-legacy: voice joins resumed'
}

media_bluegreen_reap() {
  local keys key colour active rc=0 project colours
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
    if media_legacy_actuate; then
      rm -f "${CONFIG_ROOT}/logs/media-reap-alert.flag" 2>/dev/null || true
      return 0
    fi
    mkdir -p "${CONFIG_ROOT}/logs" 2>/dev/null || true
    printf 'media reaper failed at %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
      >"${CONFIG_ROOT}/logs/media-reap-alert.flag" 2>/dev/null || true
    return 1
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
    media_bluegreen_reap_one "$colour" || {
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
    echo 'media-bluegreen: reap aborted; drain tombstone remains for retry and operator inspection' >&2
  fi
  return "$rc"
}

redis_managed_control_request() {
  local request="$1"
  printf '%s\n' "$request" | (cd "$DEPLOY_DIR" && $COMPOSE_CMD exec -T --user 0 redis-auth-controller redis-auth-controller request)
}

redis_managed_bind_stopped_instance() {
  local slot="$1" id="$2" inspected hostname status previous replaced request response
  [[ "$slot" == control-plane || "$slot" == media-plane || "$slot" == media-plane-blue || "$slot" == media-plane-green ]] || return 1
  [[ "$id" =~ ^[a-f0-9]{64}$ ]] || {
    echo 'redis-managed: exactly one full container ID is required per ACK slot' >&2; return 1;
  }
  inspected="$(docker inspect --type container --format '{"Id":{{json .Id}},"State":{"Running":{{json .State.Running}}},"Config":{"Hostname":{{json .Config.Hostname}},"User":{{json .Config.User}},"Labels":{"com.docker.compose.service":{{json (index .Config.Labels "com.docker.compose.service")}}}},"Mounts":{{json .Mounts}}}' "$id")" || return 1
  hostname="$(python3 -c '
import json,re,sys
try:
    c=json.load(sys.stdin); slot=sys.argv[1]; cid=sys.argv[2]
    if c["Id"]!=cid or c["State"]["Running"] or c["Config"]["Labels"].get("com.docker.compose.service")!=slot:
        raise ValueError()
    if c["Config"]["User"]!="appuser": raise ValueError()
    host=c["Config"]["Hostname"]
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,62}",host): raise ValueError()
    mounts=c["Mounts"]
    ack=[m for m in mounts if m.get("Destination")=="/run/redis-auth"]
    if len(ack)!=1 or ack[0].get("Source")!="/run/concord/redis-auth/acks/"+slot or ack[0].get("RW") is not False:
        raise ValueError()
    identity=[m for m in mounts if m.get("Destination")=="/run/secrets"]
    role="control-plane" if slot=="control-plane" else "media-plane"
    if len(identity)!=1 or identity[0].get("Source")!="/run/concord/redis-identities/"+role or identity[0].get("RW") is not False:
        raise ValueError()
    if any(m.get("Source","").startswith("/run/concord/redis-auth") and m is not ack[0] for m in mounts):
        raise ValueError()
    print(host)
except (KeyError,TypeError,ValueError,json.JSONDecodeError):
    raise SystemExit("redis-managed: inspected container is not isolated in its ACK slot")
' "$slot" "$id" <<<"$inspected")" || return 1
  status="$(redis_managed_control_request '{"operation":"status"}')" || return 1
  previous="$(python3 -c '
import json,re,sys
s=json.load(sys.stdin); b=s.get("bindings",{}).get(sys.argv[1])
if b is None: print(""); raise SystemExit(0)
cid=b.get("containerId")
if not isinstance(cid,str) or not re.fullmatch(r"[a-f0-9]{64}",cid): raise SystemExit(1)
print(cid)
' "$slot" <<<"$status")" || return 1
  if [[ -n "$previous" && "$previous" != "$id" ]]; then
    if docker inspect --type container "$previous" >/dev/null 2>&1; then
      echo 'redis-managed: prior slot container still exists; refusing rebind' >&2
      return 1
    fi
  fi
  replaced="$previous"
  [[ "$previous" == "$id" ]] && replaced=''
  request="$(python3 -c 'import json,sys; print(json.dumps({"operation":"bind","slot":sys.argv[1],"containerId":sys.argv[2],"instanceId":sys.argv[3],"replacedContainerId":sys.argv[4]},separators=(",",":")))' "$slot" "$id" "$hostname" "$replaced")" || return 1
  response="$(redis_managed_control_request "$request")" || return 1
  python3 -c 'import json,sys; s=json.load(sys.stdin); b=s.get("bindings",{}).get(sys.argv[1]); raise SystemExit(0 if b=={"containerId":sys.argv[2],"instanceId":sys.argv[3]} else 1)' "$slot" "$id" "$hostname" <<<"$response" || return 1
  printf '%s\n' "$hostname"
}

redis_managed_replace_ready() {
  local slot="$1" status previous request
  status="$(redis_managed_control_request '{"operation":"status"}')" || return 1
  previous="$(python3 -c '
import json,re,sys
b=json.load(sys.stdin).get("bindings",{}).get(sys.argv[1])
if b is None: print(""); raise SystemExit(0)
cid=b.get("containerId")
if not isinstance(cid,str) or not re.fullmatch(r"[a-f0-9]{64}",cid): raise SystemExit(1)
print(cid)
' "$slot" <<<"$status")" || return 1
  [[ -n "$previous" ]] || return 0
  request="$(python3 -c 'import json,sys; print(json.dumps({"operation":"replace-ready","slot":sys.argv[1],"containerId":sys.argv[2]},separators=(",",":")))' "$slot" "$previous")" || return 1
  redis_managed_control_request "$request" >/dev/null || {
    echo 'redis-managed: prior ACK slot has not acknowledged; refusing container replacement' >&2
    return 1
  }
}

redis_managed_require_registered_state() {
  local status="${1:-}"
  if [[ -z "$status" ]]; then
    status="$(redis_managed_control_request '{"operation":"status"}')" || {
      echo 'redis-managed: controller status unavailable; refusing before app creation or traffic drain' >&2
      return 1
    }
  fi
  python3 -c '
import json, re, sys
try:
    valid_host = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$")
    status = json.load(sys.stdin)
    if set(status) != {"state", "rotationId", "targets", "instances", "bindings"} or not isinstance(status["rotationId"], str) or not status["rotationId"]:
        raise ValueError()
    targets = status["targets"]
    if not isinstance(targets, dict) or set(targets) != {"control-plane", "media-plane"}:
        raise ValueError()
    if any(type(value) is not int or value < 1 for value in targets.values()):
        raise ValueError()
    instances = status["instances"]
    if not isinstance(instances, dict) or set(instances) != {"control-plane", "media-plane"}:
        raise ValueError()
    for hosts in instances.values():
        if not isinstance(hosts, list) or not hosts or len(hosts) > 64 or len(set(hosts)) != len(hosts):
            raise ValueError()
        if any(not isinstance(host, str) or not valid_host.fullmatch(host) for host in hosts):
            raise ValueError()
except (ValueError, TypeError, KeyError, json.JSONDecodeError):
    raise SystemExit("redis-managed: controller has no known registered instance set; operator-reviewed registration is required before app creation or traffic drain")
' <<<"$status"
}

redis_managed_require_candidate_state() {
  python3 -c '
import json, re, sys
host = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$")
try:
    s=json.load(sys.stdin)
    common={"state","rotationId","targets","bindings"}
    if s.get("state") not in ("finalized","prepared","promoted") or not isinstance(s.get("rotationId"),str) or not s["rotationId"]:
        raise ValueError()
    if set(s.get("targets",{}))!={"control-plane","media-plane"} or any(type(v) is not int or v<1 for v in s["targets"].values()): raise ValueError()
    if not isinstance(s.get("bindings"),dict): raise ValueError()
    if set(s)==common|{"instances"}:
        instances=s["instances"]
        if set(instances)!={"control-plane","media-plane"}: raise ValueError()
        for hosts in instances.values():
            if not isinstance(hosts,list) or not hosts or len(hosts)>64 or len(set(hosts))!=len(hosts) or any(not isinstance(h,str) or not host.fullmatch(h) for h in hosts): raise ValueError()
    elif set(s)==common|{"priorRotationId","priorExpected","priorAckProofs"}:
        if not isinstance(s["priorRotationId"],str) or not s["priorRotationId"] or s["priorRotationId"]==s["rotationId"]: raise ValueError()
        previous=s["priorExpected"]
        if not isinstance(previous,dict) or set(previous)!={"control-plane","media-plane"}: raise ValueError()
        for hosts in previous.values():
            if not isinstance(hosts,list) or len(hosts)>64 or len(set(hosts))!=len(hosts) or any(not isinstance(h,str) or not host.fullmatch(h) for h in hosts): raise ValueError()
        proofs=s["priorAckProofs"]
        if not isinstance(proofs,list) or len(proofs)>128: raise ValueError()
        seen_proofs=set()
        seen_containers=set()
        for p in proofs:
            if not isinstance(p,dict) or set(p)!={"service","instanceId","slot","containerId"} or not isinstance(p["instanceId"],str) or not host.fullmatch(p["instanceId"]) or not isinstance(p["containerId"],str) or not re.fullmatch(r"[a-f0-9]{64}",p["containerId"]): raise ValueError()
            if p["service"] == "control-plane":
                if p["slot"] != "control-plane" or p["instanceId"] not in previous["control-plane"]: raise ValueError()
            elif p["service"] == "media-plane":
                if p["slot"] not in ("media-plane","media-plane-blue","media-plane-green") or p["instanceId"] not in previous["media-plane"]: raise ValueError()
            else: raise ValueError()
            # The controller derives this proof from the accepted ACK and its
            # journaled historical binding. The current slot may already have
            # been rebound to the N+1 candidate by the time status is reread.
            proof_key=(p["service"],p["instanceId"])
            if proof_key in seen_proofs or p["containerId"] in seen_containers: raise ValueError()
            seen_proofs.add(proof_key)
            seen_containers.add(p["containerId"])
    else: raise ValueError()
except (ValueError,TypeError,KeyError,json.JSONDecodeError):
    raise SystemExit("redis-managed: controller has no valid candidate registration state")
' <<<"$1"
}

redis_managed_assert_compatible_image() {
  local container="$1" role="$2" modes
  # Read the label the container inherited: under the containerd image store a
  # preflight rebuild retags the image and its old ID stops resolving.
  modes="$(docker inspect --type container --format '{{ index .Config.Labels "org.concord.redis-credential-modes" }}' "$container" 2>/dev/null || true)"
  [[ "$modes" == legacy,managed ]] || {
    echo "redis-managed: ${role} image lacks managed credential-mode rollback support; refusing before drain" >&2
    return 1
  }
}

redis_managed_prepare_registered_apps() {
  local VOICE_NEVER_STARTED_CANDIDATES=''
  local standby="$1" active="$2" full="$3" start_cp="$4" status service ids hostname cp_running=0
  local candidate_service="media-plane-${standby}" active_service=""
  local -a cp_hosts=() media_hosts=() prior_cp_hosts=() prior_media_hosts=() prior_cp_containers=() prior_media_containers=() start_services=()

  [[ "$REDIS_CREDENTIAL_MODE" == managed ]] || return 0
  [[ "$active" == none ]] || active_service="media-plane-${active}"

  # Recheck immediately before inspecting or creating application containers.
  status="$(redis_managed_control_request '{"operation":"status"}')" || {
    echo 'redis-managed: controller status unavailable; refusing before app creation or traffic drain' >&2
    return 1
  }
  redis_managed_require_candidate_state "$status" || return 1

  ids="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q control-plane)" || return 1
  if [[ -n "$ids" ]]; then
    [[ "$ids" =~ ^[a-f0-9]{64}$ ]] || { echo 'redis-managed: unsupported control-plane ACK slot scale' >&2; return 1; }
    cp_running=1
    if [[ "$full" == 1 || "$start_cp" == 1 ]]; then
      echo 'redis-managed: control-plane image replacement cannot proceed without a registered candidate; refusing before drain' >&2
      return 1
    fi
    while IFS= read -r id; do
      [[ -n "$id" ]] || continue
      hostname="$(docker inspect --format '{{.Config.Hostname}}' "$id")" || return 1
      [[ "$hostname" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$ ]] || return 1
      redis_managed_assert_compatible_image "$id" control-plane || return 1
      cp_hosts+=("$hostname")
      prior_cp_hosts+=("$hostname")
      prior_cp_containers+=("$id")
    done <<<"$ids"
  else
    service=control-plane
    redis_managed_replace_ready "$service" || return 1
    managed_compose_build build "$service" || return 1
    voice_enforcement_rollout_guard_mutation up "$service" || return 1
    (cd "$DEPLOY_DIR" && $COMPOSE_CMD up --no-start --no-build --no-deps --force-recreate "$service") || return 1
    voice_enforcement_rollout_bind_created "$service" || return 1
    managed_egress_reconcile || return 1
    ids="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps -a -q "$service")" || return 1
    [[ "$ids" =~ ^[a-f0-9]{64}$ ]] || return 1
    hostname="$(redis_managed_bind_stopped_instance "$service" "$ids")" || return 1
    redis_managed_assert_compatible_image "$ids" control-plane || return 1
    [[ "$hostname" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$ ]] || return 1
    cp_hosts+=("$hostname")
  fi

  if [[ -n "$active_service" ]]; then
    ids="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q "$active_service")" || return 1
    [[ "$ids" =~ ^[a-f0-9]{64}$ ]] || {
      echo 'redis-managed: active media instance is absent; refusing expected-set registration' >&2
      return 1
    }
    while IFS= read -r id; do
      [[ -n "$id" ]] || continue
      hostname="$(docker inspect --format '{{.Config.Hostname}}' "$id")" || return 1
      [[ "$hostname" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$ ]] || return 1
      redis_managed_assert_compatible_image "$id" active-media || return 1
      media_hosts+=("$hostname")
      prior_media_hosts+=("$hostname")
      prior_media_containers+=("$id")
    done <<<"$ids"
  fi

  ids="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q "$candidate_service")" || return 1
  [[ -z "$ids" ]] || {
    echo 'redis-managed: standby media instance is already running before expected-set registration' >&2
    return 1
  }
  redis_managed_replace_ready "$candidate_service" || return 1
  managed_compose_build build "$candidate_service" || return 1
  voice_enforcement_rollout_guard_mutation up "$candidate_service" || return 1
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD up --no-start --no-build --no-deps --force-recreate "$candidate_service") || return 1
  voice_enforcement_rollout_bind_created "$candidate_service" || return 1
  managed_egress_reconcile || return 1
  ids="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps -a -q "$candidate_service")" || return 1
  [[ "$ids" =~ ^[a-f0-9]{64}$ ]] || return 1
  hostname="$(redis_managed_bind_stopped_instance "$candidate_service" "$ids")" || return 1
  redis_managed_assert_compatible_image "$ids" candidate-media || return 1
  [[ "$hostname" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$ ]] || return 1
  media_hosts+=("$hostname")
  start_services+=("$candidate_service")

  # Container creation can span an activation rotation, which replaces the
  # expected-instance set. Register against the current controller generation.
  status="$(redis_managed_control_request '{"operation":"status"}')" || return 1
  redis_managed_require_candidate_state "$status" || return 1

  local registration
  registration="$(python3 -c '
import json, re, sys
try:
    valid_host = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$")
    status = json.loads(sys.argv[1])
    common = {"state", "rotationId", "targets", "bindings"}
    if set(status) not in (common | {"instances"}, common | {"priorRotationId", "priorExpected", "priorAckProofs"}) or not isinstance(status["rotationId"], str) or not status["rotationId"]:
        raise ValueError()
    targets = status["targets"]
    if not isinstance(targets, dict) or set(targets) != {"control-plane", "media-plane"}:
        raise ValueError()
    if any(type(value) is not int or value < 1 for value in targets.values()):
        raise ValueError()
    proofs_to_check = []
    if "instances" in status:
        previous = status["instances"]
        if not isinstance(previous, dict) or set(previous) != {"control-plane", "media-plane"}:
            raise ValueError()
        old_cp, old_media = previous["control-plane"], previous["media-plane"]
        if not isinstance(old_cp, list) or not isinstance(old_media, list):
            raise ValueError()
        if any(not isinstance(host, str) or not valid_host.fullmatch(host) for host in old_cp + old_media):
            raise ValueError()
        if len(old_cp) > 64 or len(old_media) > 64 or len(set(old_cp)) != len(old_cp) or len(set(old_media)) != len(old_media):
            raise ValueError()
    else:
        if not isinstance(status["priorRotationId"], str) or not status["priorRotationId"] or status["priorRotationId"] == status["rotationId"]:
            raise ValueError()
        previous = status["priorExpected"]
        if not isinstance(previous, dict) or set(previous) != {"control-plane", "media-plane"}:
            raise ValueError()
        old_cp, old_media = previous["control-plane"], previous["media-plane"]
        if any(not isinstance(hosts, list) or len(hosts) > 64 or len(set(hosts)) != len(hosts) or any(not isinstance(host, str) or not valid_host.fullmatch(host) for host in hosts) for hosts in (old_cp, old_media)):
            raise ValueError()
        active_cp, active_media = set(sys.argv[4].splitlines()), set(sys.argv[5].splitlines())
        active_cp_ids, active_media_ids = set(sys.argv[6].splitlines()), set(sys.argv[7].splitlines())
        proofs = status["priorAckProofs"]
        if not isinstance(proofs, list) or len(proofs) > 128:
            raise ValueError()
        by_host = {}
        for proof in proofs:
            if not isinstance(proof, dict) or set(proof) != {"service", "instanceId", "slot", "containerId"}:
                raise ValueError()
            service, host, slot, cid = (proof[k] for k in ("service", "instanceId", "slot", "containerId"))
            if not isinstance(host, str) or not valid_host.fullmatch(host) or not isinstance(cid, str) or not re.fullmatch(r"[a-f0-9]{64}", cid):
                raise ValueError()
            if service == "control-plane":
                if slot != "control-plane" or host not in old_cp: raise ValueError()
            elif service == "media-plane":
                if slot not in ("media-plane", "media-plane-blue", "media-plane-green") or host not in old_media: raise ValueError()
            else: raise ValueError()
            if host in by_host: raise ValueError()
            by_host[host] = cid
        for host in old_cp:
            if host not in active_cp and host not in by_host: raise ValueError()
            if host in by_host and (host not in active_cp or by_host[host] not in active_cp_ids):
                proofs_to_check.append(by_host[host])
        for host in old_media:
            if host not in active_media and host not in by_host: raise ValueError()
            if host in by_host and (host not in active_media or by_host[host] not in active_media_ids):
                proofs_to_check.append(by_host[host])
        bindings=status["bindings"]
        for host, cid in zip(sys.argv[4].splitlines(), sys.argv[6].splitlines()):
            if host not in old_cp or bindings.get("control-plane") != {"containerId":cid,"instanceId":host}: raise ValueError()
        media_slot=sys.argv[8]
        for host, cid in zip(sys.argv[5].splitlines(), sys.argv[7].splitlines()):
            if host not in old_media or bindings.get(media_slot) != {"containerId":cid,"instanceId":host}: raise ValueError()
    cp = sys.argv[2].splitlines()
    media = sys.argv[3].splitlines()
    if not cp or not media or any(not valid_host.fullmatch(host) for host in cp + media):
        raise ValueError()
    if "instances" in status:
        expected_cp, expected_media = old_cp + cp, old_media + media
    else:
        expected_cp, expected_media = cp, media
    request = {"operation":"register", "rotationId":status["rotationId"],
      "instances":{"control-plane":sorted(set(expected_cp)), "media-plane":sorted(set(expected_media))}, "targets":targets}
    if any(len(request["instances"][service]) > 64 for service in request["instances"]):
        raise ValueError()
    print(json.dumps({"request": request, "priorProofContainers": sorted(set(proofs_to_check))}, separators=(",",":")))
except (ValueError, TypeError, KeyError, json.JSONDecodeError):
    raise SystemExit("redis-managed: controller policy or inspected instance set is invalid")
 ' "$status" "$(printf '%s\n' "${cp_hosts[@]}")" "$(printf '%s\n' "${media_hosts[@]}")" "$(printf '%s\n' "${prior_cp_hosts[@]}")" "$(printf '%s\n' "${prior_media_hosts[@]}")" "$(printf '%s\n' "${prior_cp_containers[@]}")" "$(printf '%s\n' "${prior_media_containers[@]}")" "$active_service")" || return 1
  local proof_ids proof_id proof_state proof_matches
  proof_ids="$(python3 -c 'import json,sys; print("\n".join(json.loads(sys.stdin.read())["priorProofContainers"]))' <<<"$registration")" || return 1
  while IFS= read -r proof_id; do
    [[ -n "$proof_id" ]] || continue
    proof_matches="$(docker ps -a --no-trunc -q --filter "id=$proof_id")" || return 1
    if [[ -n "$proof_matches" ]]; then
      [[ "$proof_matches" == "$proof_id" ]] || { echo 'redis-managed: prior ACK container lookup was ambiguous' >&2; return 1; }
      proof_state="$(docker inspect --format '{{.State.Running}}' "$proof_id")" || return 1
      [[ "$proof_state" == false ]] || { echo 'redis-managed: prior ACK instance is not active or stopped; refusing candidate registration' >&2; return 1; }
    fi
  done <<<"$proof_ids"
  registration="$(python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin)["request"],separators=(",",":")))' <<<"$registration")" || return 1
  local registered
  registered="$(redis_managed_control_request "$registration")" || {
    echo 'redis-managed: controller rejected expected-instance registration; apps remain stopped' >&2
    return 1
  }
  python3 -c 'import json,sys; v=json.load(sys.stdin); raise SystemExit(0 if isinstance(v,dict) and v.get("state") in ("prepared","promoted") else 1)' <<<"$registered" || {
    echo 'redis-managed: controller did not confirm expected-instance registration' >&2
    return 1
  }

  if [[ "$cp_running" == 0 ]]; then
    start_services+=(control-plane)
  fi
  if [[ ${#start_services[@]} -gt 0 ]]; then
    (cd "$DEPLOY_DIR" && managed_compose_mutate start --wait --wait-timeout 180 -- "${start_services[@]}") || return 1
  fi

  local deadline=$((SECONDS + 90)) current
  while (( SECONDS < deadline )); do
    current="$(redis_managed_control_request '{"operation":"status"}')" || return 1
    if python3 -c 'import json,sys; v=json.load(sys.stdin); raise SystemExit(0 if v.get("state") in ("promoted","finalized") else 1)' <<<"$current"; then
      return 0
    fi
    sleep 3
  done
  echo 'redis-managed: expected instances did not prove the target generation; traffic remains unchanged' >&2
  return 1
}

redis_managed_policy_preflight() {
  [[ "$REDIS_CREDENTIAL_MODE" == managed && "$ACTIVITY_HISTORY_CONTRACT_MODE" == managed ]] || {
    echo 'redis-managed: prepare/rebuild require an explicitly selected managed deployment' >&2
    return 1
  }
  redis_managed_controller_host_identity || return 1
  redis_managed_marker_preflight "${REDIS_MANAGED_MARKER_MODE:-required}" || return 1
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD config --quiet) || {
    echo 'redis-managed: selected Compose model is invalid; refusing maintenance' >&2
    return 1
  }
  redis_managed_validate_mount_model || return 1
  managed_production_nats_reconcile || return 1
  redis_managed_image_preflight
}

redis_managed_validate_transport() {
  local interpolation_env
  interpolation_env="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD config --environment)" || {
    echo 'redis-managed: cannot inspect Compose interpolation inputs; refusing maintenance' >&2
    return 1
  }
  python3 -c '
import sys
values = {key: value for line in sys.stdin for key, sep, value in [line.rstrip("\n").partition("=")] if sep}
if values.get("REDIS_ENDPOINT", "redis:6379") != "redis:6379" or values.get("REDIS_TLS", "false") != "false" or values.get("REDIS_TLS_CA_FILE", "") != "":
    raise SystemExit("redis-managed: only in-stack Redis without TLS or a custom CA is supported; refusing before Compose mutation")
' <<<"$interpolation_env" || return 1
}

redis_managed_validate_mount_model() {
  redis_managed_validate_transport || return 1
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD --profile services --profile media-bluegreen --env-file /dev/null config --no-interpolate --no-env-resolution --format json) | python3 -c '
import json,sys
try:
    services=json.load(sys.stdin)["services"]
    app_slots={"control-plane","media-plane","media-plane-blue","media-plane-green"}
    def overlaps(source, protected):
        return isinstance(source,str) and source.startswith("/") and (source==protected or source.startswith(protected+"/") or protected.startswith(source.rstrip("/")+"/"))
    marker=[m for m in services["redis-auth-controller"]["volumes"] if m.get("target")=="/var/lib/concord/redis-auth"]
    if len(marker)!=1 or marker[0].get("source")!="/var/lib/concord/redis-auth" or marker[0].get("read_only") or marker[0].get("type")!="bind" or marker[0].get("bind",{}).get("create_host_path") is not False:
        raise ValueError()
    for name, spec in services.items():
        if name!="redis-auth-controller" and any(overlaps(m.get("source"),"/var/lib/concord/redis-auth") for m in spec.get("volumes",[])):
            raise ValueError()
        if name not in app_slots and set(spec.get("depends_on",{})) & app_slots:
            raise ValueError()
    for slot in app_slots:
        mounts=services[slot]["volumes"]
        ack=[m for m in mounts if m.get("target")=="/run/redis-auth"]
        if len(ack)!=1 or ack[0].get("source")!="/run/concord/redis-auth/acks/"+slot or not ack[0].get("read_only"):
            raise ValueError()
        identity=[m for m in mounts if m.get("target")=="/run/secrets"]
        role="control-plane" if slot=="control-plane" else "media-plane"
        if len(identity)!=1 or identity[0].get("source")!="/run/concord/redis-identities/"+role or not identity[0].get("read_only"):
            raise ValueError()
        if any(overlaps(m.get("source"),"/run/concord/redis-auth") and m is not ack[0] for m in mounts):
            raise ValueError()
        if any(overlaps(m.get("source"),"/run/concord/redis-identities") and m is not identity[0] for m in mounts):
            raise ValueError()
        if any(overlaps(m.get("source"),"/var/lib/concord/redis-auth") for m in mounts):
            raise ValueError()
except (KeyError,TypeError,ValueError,json.JSONDecodeError):
    raise SystemExit("redis-managed: ACK slot Compose mounts are not isolated")
' || return 1
}

redis_managed_image_preflight() {
  local model service image image_id account actual_uid actual_gid
  local -a image_services=(control-plane redis-auth-controller)
  if [[ "${MEDIA_DEPLOY_PROFILE:-media-single}" == media-bluegreen ]]; then
    image_services+=(media-plane-blue media-plane-green)
  else
    image_services+=(media-plane)
  fi
  managed_compose_build --profile services build "${image_services[@]}" || return 1
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD pull redis) || return 1
  model="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD --profile services config --format json)" || return 1
  image_services+=(redis)
  for service in "${image_services[@]}"; do
    image="$(python3 -c 'import json,sys; m=json.load(sys.stdin); print(m["services"][sys.argv[1]].get("image") or m["name"] + "-" + sys.argv[1])' "$service" <<<"$model")" || return 1
    [[ -n "$image" ]] || { echo "redis-managed: selected ${service} image is unavailable in Compose model" >&2; return 1; }
    image_id="$(docker image inspect --format '{{.Id}}' "$image")" || {
      echo "redis-managed: selected ${service} image is unavailable locally after build/pull" >&2; return 1;
    }
    case "$service" in
      control-plane|media-plane|media-plane-blue|media-plane-green) account=appuser ;;
      redis) account=redis ;;
      redis-auth-controller) account=redis-auth-controller ;;
    esac
    actual_uid="$(docker run --rm --network none --read-only --cap-drop ALL \
      --security-opt no-new-privileges --cpus 0.1 --memory 64m --entrypoint id \
      "$image_id" -u "$account" 2>/dev/null)" || {
      echo "redis-managed: cannot resolve selected ${service} image UID" >&2; return 1;
    }
    actual_gid="$(docker run --rm --network none --read-only --cap-drop ALL \
      --security-opt no-new-privileges --cpus 0.1 --memory 64m --entrypoint id \
      "$image_id" -g "$account" 2>/dev/null)" || {
      echo "redis-managed: cannot resolve selected ${service} image GID" >&2; return 1;
    }
    [[ "$actual_uid" =~ ^(0|[1-9][0-9]*)$ && "$actual_gid" =~ ^(0|[1-9][0-9]*)$ ]] || {
      echo "redis-managed: selected ${service} image returned nonnumeric UID/GID" >&2; return 1;
    }
    case "$service" in
      control-plane)
        [[ "$actual_uid" == "$REDIS_CONTROL_UID" ]] || { echo 'redis-managed: control-plane UID selector differs from inspected image' >&2; return 1; }
        [[ "$actual_gid" != 0 ]] || { echo 'redis-managed: control-plane GID must not be root' >&2; return 1; }
        REDIS_CONTROL_GID="$actual_gid" ;;
      media-plane|media-plane-blue|media-plane-green)
        [[ "$actual_uid" == "$REDIS_MEDIA_UID" && "$actual_gid" != 0 && ( -z "${REDIS_MEDIA_GID:-}" || "$actual_gid" == "$REDIS_MEDIA_GID" ) ]] || {
          echo "redis-managed: ${service} UID/GID differs from inspected media image" >&2; return 1;
        }
        REDIS_MEDIA_GID="$actual_gid" ;;
      redis) [[ "$actual_uid" == "$REDIS_ACL_UID" && "$actual_gid" == "$REDIS_ACL_GID" ]] || { echo 'redis-managed: Redis ACL UID/GID selectors differ from inspected image' >&2; return 1; } ;;
      redis-auth-controller) [[ "$actual_uid" == 10004 && "$actual_gid" == 10004 ]] || { echo 'redis-managed: controller image must use the dedicated UID/GID' >&2; return 1; } ;;
    esac
    if [[ "$service" != redis-auth-controller && ( "$actual_uid" == 10004 || "$actual_gid" == 10004 ) ]]; then
      echo "redis-managed: ${service} account collides with the controller identity" >&2; return 1;
    fi
  done
}

redis_managed_prepare_ram_state() {
  local root=/run/concord/redis-auth
  redis_managed_controller_host_identity || return 1
  if [[ "${REDIS_MANAGED_RECOVER:-0}" != 1 && ( -e "$root/state/rotation.json" || -e "$root/acl/users.acl" ) ]] \
    && { [[ "${REDIS_MANAGED_RECOVER:-0}" != 3 ]] || ! cmp -s /var/lib/concord/redis-auth/activated <(printf 'concord-managed-redis-initializing-v1\n'); }; then
    echo 'redis-managed: existing journal or ACL requires redis-managed-rebuild or explicit --recover' >&2
    return 1
  fi
  if [[ "${REDIS_MANAGED_RECOVER:-0}" == 1 && -e "$root/acl/users.acl" && ! -e "$root/state/rotation.json" ]]; then
    echo 'redis-managed: ACL exists without a journal; refusing to invent revoked-user history' >&2
    return 1
  fi
  [[ ( ! -e "$root" && ! -L "$root" ) || ( ! -L "$root" && "$(stat -c '%u:%g:%a' "$root" 2>/dev/null || true)" == '10004:0:2710' ) ]] || {
    echo 'redis-managed: legacy root-owned RAM state requires a separately reviewed migration' >&2; return 1;
  }
  [[ "${REDIS_CONTROL_GID:-}" =~ ^[1-9][0-9]*$ && "${REDIS_MEDIA_GID:-}" =~ ^[1-9][0-9]*$ ]] || return 1
  local path expected slot slot_gid
  for path in "$root/state" "$root/acks" "$root/acl" \
    "$root/acks/control-plane" "$root/acks/media-plane" "$root/acks/media-plane-blue" "$root/acks/media-plane-green"; do
    [[ -e "$path" || -L "$path" ]] || continue
    case "$path" in
      "$root/state"|"$root/acks") expected='10004:10004:700' ;;
      "$root/acl") expected="10004:${REDIS_ACL_GID}:2710" ;;
      "$root/acks/control-plane") expected="10004:${REDIS_CONTROL_GID}:2710" ;;
      *) expected="10004:${REDIS_MEDIA_GID}:2710" ;;
    esac
    [[ -d "$path" && ! -L "$path" && "$(stat -c '%u:%g:%a' "$path" 2>/dev/null || true)" == "$expected" ]] || {
      echo 'redis-managed: existing RAM state needs a separately reviewed ownership migration' >&2; return 1;
    }
  done
  install -d -o 10004 -g 0 -m 2710 "$root" || return 1
  install -d -o 10004 -g 10004 -m 00700 "$root/state" "$root/acks" \
    || return 1
  install -d -o 10004 -g "$REDIS_ACL_GID" -m 2710 "$root/acl" \
    || return 1
  for slot in control-plane media-plane media-plane-blue media-plane-green; do
    slot_gid="$REDIS_MEDIA_GID"
    [[ "$slot" == control-plane ]] && slot_gid="$REDIS_CONTROL_GID"
    install -d -o 10004 -g "$slot_gid" -m 2710 "$root/acks/$slot" || return 1
  done
}

redis_managed_prepare_controller() {
  local recover="${1:-0}" proof="${2:-}"
  local -a args=(run --rm --no-deps redis-auth-controller prepare)
  [[ "$recover" == 3 ]] && args+=(--first-activation)
  [[ "$recover" == 1 ]] && args+=(--recover)
  if [[ "$recover" == 2 ]]; then
    args=(run -T --rm --no-deps redis-auth-controller prepare --recover-lost-state)
    redis_managed_read_lost_state_proof "$proof" | (cd "$DEPLOY_DIR" && $COMPOSE_CMD "${args[@]}")
    return $?
  fi
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD "${args[@]}")
}

redis_managed_read_lost_state_proof() {
  python3 -c '
import os, stat, sys
path = sys.argv[1]
if not path.startswith("/"):
    raise SystemExit("redis-managed: lost-state proof must have an absolute path")
try:
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
except OSError:
    raise SystemExit("redis-managed: lost-state proof is unavailable or is a symlink")
try:
    info = os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or stat.S_IMODE(info.st_mode) != 0o600 or not 0 < info.st_size <= 16384:
        raise SystemExit("redis-managed: lost-state proof requires a root-owned 0600 regular file of at most 16 KiB")
    data = os.read(fd, 16385)
    if len(data) != info.st_size:
        raise SystemExit("redis-managed: lost-state proof changed during read")
    sys.stdout.buffer.write(data)
finally:
    os.close(fd)
' "$1"
}

redis_managed_stopped_apps() {
  local service ids
  for service in control-plane media-plane media-plane-blue media-plane-green; do
    ids="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q "$service" 2>/dev/null)" || return 1
    [[ -z "$ids" ]] || {
      echo "redis-managed: ${service} is still running; refusing the next maintenance step" >&2
      return 1
    }
  done
}

redis_managed_select_media_service() {
  local mode="${1:-}" active response
  if [[ "${MEDIA_DEPLOY_PROFILE:-media-single}" == media-single ]]; then
    echo media-plane
    return 0
  fi
  response="$(redis_managed_control_request '{"operation":"redis","args":["GET","media:active_colour"]}')" || {
    echo 'redis-managed: cannot read the active media colour; refusing service selection' >&2
    return 1
  }
  active="$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("output", ""))' <<<"$response")" || return 1
  [[ -n "$active" || "$mode" != initial ]] || active=blue
  [[ "$active" == blue || "$active" == green ]] || {
    echo 'redis-managed: active media colour is missing or invalid; operator selection is required' >&2
    return 1
  }
  echo "media-plane-${active}"
}

redis_managed_wait_apps() {
  local media_service="${1:-media-plane}" mode="${2:-}" deadline=$((SECONDS + 180)) status media_port=3000 active confirmed
  while (( SECONDS < deadline )); do
    status="$(redis_managed_control_request '{"operation":"status"}')" || { sleep 3; continue; }
    if python3 -c 'import json,sys; s=json.load(sys.stdin); raise SystemExit(0 if s.get("state")=="finalized" else 1)' <<<"$status"; then
      local cp media
      cp="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q control-plane)" || return 1
      media="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q "$media_service")" || return 1
      [[ -n "$cp" && -n "$media" ]] || { sleep 3; continue; }
      curl -fsS --max-time 3 http://127.0.0.1:8080/readyz >/dev/null || { sleep 3; continue; }
      # Final Redis ACKs and HTTP success do not prove the replacement's
      # protocol or boot. Reuse the exact configured-container gate for the
      # restarted slot before promotion or success; stop_media already retired
      # every other slot's boot, and this rebuild does not start them.
      voice_enforcement_rollout_probe_slots productive "$media_service" || { sleep 3; continue; }
      if [[ "${MEDIA_DEPLOY_PROFILE:-media-single}" == media-bluegreen ]]; then
        active="$(media_bluegreen_redis GET media:active_colour)" || { sleep 3; continue; }
        if [[ -z "$active" ]]; then
          [[ "$mode" == initial && "$media_service" == media-plane-blue ]] || {
            echo 'redis-managed: missing active colour for a noninitial media selection' >&2; return 1;
          }
          media_bluegreen_probe_ready blue true 3000 || { sleep 3; continue; }
          media_bluegreen_redis SADD media:colours:registered blue >/dev/null || return 1
          if ! media_bluegreen_redis SET media:active_colour blue >/dev/null; then
            confirmed="$(media_bluegreen_redis GET media:active_colour)" || return 1
            [[ "$confirmed" == blue ]] || {
              echo 'redis-managed: initial active colour write was not confirmed' >&2; return 1;
            }
          fi
          active="$(media_bluegreen_redis GET media:active_colour)" || return 1
          [[ "$active" == blue ]] || {
            echo 'redis-managed: initial active colour readback disagrees with selected media' >&2; return 1;
          }
        fi
        [[ "$media_service" == "media-plane-${active}" ]] || {
          echo 'redis-managed: active colour changed during startup; refusing success' >&2; return 1;
        }
        media_port="$(media_bluegreen_port "$active")" || return 1
        media_bluegreen_probe_ready "$active" true "$media_port" || { sleep 3; continue; }
      else
        curl -fsS --max-time 3 "http://127.0.0.1:${media_port}/readyz" >/dev/null || { sleep 3; continue; }
      fi
      redis_managed_control_request '{"operation":"redis","args":["GET","media:active_colour"]}' >/dev/null || { sleep 3; continue; }
      return 0
    fi
    sleep 3
  done
  echo 'redis-managed: expected Redis acknowledgments and app health did not complete; services remain for operator recovery' >&2
  return 1
}

redis_managed_register_stopped_apps() {
  local media_service="${1:-media-plane}" status service ids hostname registration response
  local -a cp_hosts=() media_hosts=()
  status="$(redis_managed_control_request '{"operation":"status"}')" || return 1
  for service in control-plane "$media_service"; do
    ids="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps -a -q "$service")" || return 1
    [[ -n "$ids" ]] || { echo "redis-managed: no stopped ${service} container was created" >&2; return 1; }
    [[ "$ids" =~ ^[a-f0-9]{64}$ ]] || { echo 'redis-managed: unsupported ACK slot scale' >&2; return 1; }
    hostname="$(redis_managed_bind_stopped_instance "$service" "$ids")" || return 1
    redis_managed_assert_compatible_image "$ids" "$service" || return 1
    if [[ "$service" == control-plane ]]; then cp_hosts+=("$hostname"); else media_hosts+=("$hostname"); fi
  done
  registration="$(python3 -c '
import json, re, sys
s=json.loads(sys.argv[1]); valid=re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$")
if s.get("state") not in ("finalized", "prepared", "promoted") or not s.get("rotationId"): raise SystemExit(1)
targets=s.get("targets",{}); old=s.get("instances",{})
cp=sys.argv[2].splitlines(); media=sys.argv[3].splitlines()
if set(targets)!={"control-plane","media-plane"} or any(type(v) is not int or v < 1 for v in targets.values()): raise SystemExit(1)
if not cp or not media or any(not valid.fullmatch(h) for h in cp+media): raise SystemExit(1)
if old and set(old)!={"control-plane","media-plane"}: raise SystemExit(1)
instances={k:sorted(set(old.get(k,[])+v)) for k,v in (("control-plane",cp),("media-plane",media))}
print(json.dumps({"operation":"register","rotationId":s["rotationId"],"instances":instances,"targets":targets},separators=(",",":")))
' "$status" "$(printf '%s\n' "${cp_hosts[@]}")" "$(printf '%s\n' "${media_hosts[@]}")")" || return 1
  response="$(redis_managed_control_request "$registration")" || return 1
  python3 -c 'import json,sys; v=json.load(sys.stdin); raise SystemExit(0 if v.get("state") in ("prepared","promoted","finalized") else 1)' <<<"$response"
}

redis_managed_rollback_locked() {
  local ids
  [[ $# -eq 0 ]] || { echo 'Usage: concord-ctl.sh redis-managed-rollback' >&2; return 2; }
  # The controller exists only in the managed overlay, so a legacy `up` would
  # leave it running. Stop every Redis client first, then remove the
  # controller and stop Redis. The marker, journal and ACL stay as history.
  voice_enforcement_rollout_stop_media || return 1
  (cd "$DEPLOY_DIR" && managed_compose_mutate stop control-plane) || return 1
  redis_managed_stopped_apps || return 1
  (cd "$DEPLOY_DIR" && managed_compose_mutate stop redis-auth-controller) || return 1
  (cd "$DEPLOY_DIR" && managed_compose_mutate rm -f redis-auth-controller) || return 1
  (cd "$DEPLOY_DIR" && managed_compose_mutate stop redis) || return 1
  ids="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps -a -q redis-auth-controller)" || return 1
  [[ -z "$ids" ]] || { echo 'redis-managed: controller container remains; refusing to report rollback ready' >&2; return 1; }
  ids="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q redis)" || return 1
  [[ -z "$ids" ]] || { echo 'redis-managed: Redis is still running; refusing to report rollback ready' >&2; return 1; }
  echo 'redis-managed: apps and Redis stopped and the controller removed.'
  echo 'Next: set REDIS_CREDENTIAL_MODE=legacy in both canonical writers, then run the approved legacy deploy.'
}

# In-place rotation on the single-media profile: register the running, already
# bound control-plane and media-plane instances as the new rotation's expected
# set, so their ACKs for the new target count. No container is created, stopped
# or started. Each instance must be the slot's current binding and must hold an
# accepted ACK proof from the prior rotation; anything else needs redis-managed-rebuild.
redis_managed_register_running_locked() {
  local status service ids hostname registration response
  local -a cp_hosts=() media_hosts=() cp_ids=() media_ids=()
  [[ $# -eq 0 ]] || { echo 'Usage: concord-ctl.sh redis-managed-register-running' >&2; return 2; }
  [[ "${MEDIA_DEPLOY_PROFILE:-media-single}" == media-single ]] || {
    echo 'redis-managed: register-running is for the single-media profile; blue/green deploys register their own candidates' >&2; return 1;
  }
  [[ ! -e "$REDIS_MANAGED_REBUILD_MARKER" && ! -L "$REDIS_MANAGED_REBUILD_MARKER" ]] || {
    echo 'redis-managed: interrupted rebuild requires redis-managed-rebuild --resume; refusing registration' >&2; return 1;
  }
  redis_managed_host_preflight || return 1
  status="$(redis_managed_control_request '{"operation":"status"}')" || return 1
  redis_managed_require_candidate_state "$status" || return 1
  python3 -c 'import json,sys; s=json.load(sys.stdin); raise SystemExit(0 if s.get("state")=="prepared" and "instances" not in s and "priorAckProofs" in s else 1)' <<<"$status" || {
    echo 'redis-managed: register-running needs a new rotation awaiting registration' >&2; return 1;
  }
  for service in control-plane media-plane; do
    ids="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q "$service")" || return 1
    [[ "$ids" =~ ^[a-f0-9]{64}$ ]] || { echo "redis-managed: expected one running ${service}" >&2; return 1; }
    hostname="$(docker inspect --type container --format '{{.Config.Hostname}}' "$ids")" || return 1
    [[ "$hostname" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$ ]] || return 1
    redis_managed_assert_compatible_image "$ids" "$service" || return 1
    if [[ "$service" == control-plane ]]; then cp_hosts+=("$hostname"); cp_ids+=("$ids"); else media_hosts+=("$hostname"); media_ids+=("$ids"); fi
  done
  registration="$(python3 -c '
import json, sys
s=json.loads(sys.argv[1])
running={"control-plane":(sys.argv[2],sys.argv[3]),"media-plane":(sys.argv[4],sys.argv[5])}
proofs={(p["slot"],p["instanceId"],p["containerId"]) for p in s["priorAckProofs"]}
for slot,(host,cid) in running.items():
    if s["bindings"].get(slot)!={"containerId":cid,"instanceId":host} or (slot,host,cid) not in proofs: raise SystemExit(1)
print(json.dumps({"operation":"register","rotationId":s["rotationId"],
  "instances":{slot:[host] for slot,(host,_) in running.items()},"targets":s["targets"]},separators=(",",":")))
' "$status" "${cp_hosts[0]}" "${cp_ids[0]}" "${media_hosts[0]}" "${media_ids[0]}")" || {
    echo 'redis-managed: a running instance is not the bound, previously acknowledged slot head; use redis-managed-rebuild' >&2; return 1;
  }
  response="$(redis_managed_control_request "$registration")" || return 1
  python3 -c 'import json,sys; v=json.load(sys.stdin); raise SystemExit(0 if v.get("state") in ("prepared","promoted") else 1)' <<<"$response"
}

redis_managed_prepare_locked() {
  local VOICE_NEVER_STARTED_CANDIDATES=''
  local recover=0 proof='' media_service mode=initial redis_running controller_running status cp_id media_id
  [[ "${1:-}" == --first-activation && $# -eq 1 ]] && recover=3
  [[ "${1:-}" == --recover ]] && recover=1
  if [[ "${1:-}" == --recover-lost-state && $# -eq 2 ]]; then
    recover=2
    proof="$2"
  fi
  [[ ( $# -eq 1 && ( "$recover" == 1 || "$recover" == 3 ) ) || ( $# -eq 2 && "$recover" == 2 ) ]] || {
    echo 'Usage: concord-ctl.sh redis-managed-prepare --first-activation | --recover | --recover-lost-state <proof.json>' >&2; return 2;
  }
  REDIS_MANAGED_RECOVER="$recover"
  REDIS_MANAGED_MARKER_MODE=required
  [[ "$recover" == 3 ]] && REDIS_MANAGED_MARKER_MODE=first
  [[ "$recover" == 2 ]] && REDIS_MANAGED_MARKER_MODE=lost
  if [[ "$recover" == 2 ]]; then
    [[ ! -e /run/concord/redis-auth/state/rotation.json && ! -L /run/concord/redis-auth/state/rotation.json && \
       ! -e /run/concord/redis-auth/acl/users.acl && ! -L /run/concord/redis-auth/acl/users.acl ]] || {
      echo 'redis-managed: lost-state recovery requires both RAM files absent; refusing to discard known state' >&2; return 1;
    }
    redis_managed_read_lost_state_proof "$proof" >/dev/null || return 1
  fi
  redis_managed_policy_preflight || return 1
  REDIS_MANAGED_INITIAL_PREFLIGHT=1
  redis_managed_host_preflight || return 1
  if [[ "$recover" == 1 ]]; then
    [[ -f /run/concord/redis-auth/state/rotation.json && ! -L /run/concord/redis-auth/state/rotation.json ]] || {
      echo 'redis-managed: recovery requires an intact journal; refusing to infer history' >&2; return 1;
    }
  elif [[ -e /run/concord/redis-auth/state/rotation.json || -e /run/concord/redis-auth/acl/users.acl ]] \
    && { [[ "$recover" != 3 ]] || ! cmp -s /var/lib/concord/redis-auth/activated <(printf 'concord-managed-redis-initializing-v1\n'); }; then
    echo 'redis-managed: state already exists; use redis-managed-rebuild or --recover' >&2; return 1
  fi
  voice_enforcement_rollout_stop_media || return 1
  (cd "$DEPLOY_DIR" && managed_compose_mutate stop control-plane) || return 1
  (cd "$DEPLOY_DIR" && managed_compose_mutate stop redis-auth-controller) || return 1
  (cd "$DEPLOY_DIR" && managed_compose_mutate stop redis) || return 1
  redis_managed_stopped_apps || return 1
  controller_running="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q redis-auth-controller)" || return 1
  [[ -z "$controller_running" ]] || {
    echo 'redis-managed: controller is still running; refusing concurrent journal recovery' >&2; return 1;
  }
  redis_running="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q redis)" || return 1
  [[ -z "$redis_running" ]] || {
    echo 'redis-managed: Redis is still running; refusing ACL preparation' >&2; return 1;
  }
  redis_managed_prepare_ram_state || return 1
  redis_managed_prepare_controller "$recover" "$proof" || return 1
  REDIS_MANAGED_MARKER_MODE=required
  redis_managed_marker_preflight required || return 1
  (cd "$DEPLOY_DIR" && managed_compose_mutate up -d --no-deps redis-auth-controller) || return 1
  (cd "$DEPLOY_DIR" && managed_compose_mutate up -d --no-deps --wait redis) || return 1
  [[ "$recover" == 1 ]] && mode=recover
  media_service="$(redis_managed_select_media_service "$mode")" || return 1
  if [[ "$recover" == 1 ]]; then
    status="$(redis_managed_control_request '{"operation":"status"}')" || return 1
    cp_id="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps -a -q control-plane)" || return 1
    media_id="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps -a -q "$media_service")" || return 1
    if python3 -c '
import json,re,sys
try:
    bindings=json.load(sys.stdin)["bindings"]
    pairs=(("control-plane",sys.argv[2]),(sys.argv[1],sys.argv[3]))
    valid=all(re.fullmatch(r"[a-f0-9]{64}", cid) and bindings.get(slot,{}).get("containerId")==cid for slot,cid in pairs)
except (KeyError,TypeError,ValueError,json.JSONDecodeError):
    valid=False
raise SystemExit(0 if valid and sys.argv[2]!=sys.argv[3] else 1)
' "$media_service" "$cp_id" "$media_id" <<<"$status"; then
      redis_managed_resume_stopped_rebuild created
      return $?
    fi
  fi
  for service in control-plane "$media_service"; do
    if [[ "$recover" == 1 ]]; then
      # A first activation that failed after the marker was promoted may have
      # bound this slot's stopped candidate already. Reuse it: replacing an
      # unacknowledged binding is refused.
      local existing bound
      existing="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps -a -q "$service")" || return 1
      bound="$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("bindings",{}).get(sys.argv[1],{}).get("containerId",""))' "$service" <<<"$status")" || return 1
      if [[ -n "$existing" && "$existing" == "$bound" ]]; then
        voice_enforcement_rollout_bind_created "$service" || return 1
        continue
      fi
    fi
    redis_managed_replace_ready "$service" || return 1
    voice_enforcement_rollout_guard_mutation up "$service" || return 1
    (cd "$DEPLOY_DIR" && $COMPOSE_CMD up --no-start --no-build --no-deps --force-recreate "$service") || return 1
    voice_enforcement_rollout_bind_created "$service" || return 1
    managed_egress_reconcile || return 1
  done
  redis_managed_register_stopped_apps "$media_service" || return 1
  (cd "$DEPLOY_DIR" && managed_compose_mutate start --wait --wait-timeout 180 control-plane) || return 1
  (cd "$DEPLOY_DIR" && managed_compose_mutate start --wait --wait-timeout 180 "$media_service") || return 1
  redis_managed_wait_apps "$media_service" "$mode"
}

redis_managed_host_preflight_initial() {
  [[ "$REDIS_CREDENTIAL_MODE" == managed ]] || return 1
  command -v findmnt >/dev/null 2>&1 || { echo 'redis-managed: findmnt is required' >&2; return 1; }
  [[ "$(findmnt -n -T /run/concord -o FSTYPE 2>/dev/null || true)" == tmpfs ]] || {
    echo 'redis-managed: /run/concord must be on host RAM; refusing disk fallback' >&2; return 1;
  }
  for role in controller control-plane media-plane; do
    local path="/run/concord/redis-identities/${role}"
    local expected_uid=10004
    [[ "$role" == control-plane ]] && expected_uid="$REDIS_CONTROL_UID"
    [[ "$role" == media-plane ]] && expected_uid="$REDIS_MEDIA_UID"
    [[ -d "$path" && ! -L "$path" && -f "$path/redis-infisical-client-secret" && ! -L "$path/redis-infisical-client-secret" ]] || {
      echo "redis-managed: ${role} provider identity is unavailable" >&2; return 1;
    }
    [[ "$(findmnt -n -T "$path" -o FSTYPE 2>/dev/null || true)" == tmpfs ]] || {
      echo "redis-managed: ${role} provider identity is not on host RAM" >&2; return 1;
    }
    [[ "$(stat -c '%u:%a' "$path" 2>/dev/null || true)" == "${expected_uid}:500" && \
       "$(stat -c '%u:%a' "$path/redis-infisical-client-secret" 2>/dev/null || true)" == "${expected_uid}:400" ]] || {
      echo "redis-managed: ${role} provider identity ownership/mode is invalid" >&2; return 1;
    }
  done
  [[ "$REDIS_ACL_GID" != 10001 ]] || { echo 'redis-managed: ACL GID collides with reserved host group 10001' >&2; return 1; }
}

# shortcut: a crash after successful health can leave this marker, add verified-complete retry if recurring.
redis_managed_rebuild_marker() {
  python3 - "$REDIS_MANAGED_REBUILD_MARKER" "$@" <<'PY'
import json, os, re, stat, sys, tempfile

path, action, raw_status, media_service, cp_id, media_id = sys.argv[1:]
try:
    status = json.loads(raw_status)
    bindings = status["bindings"]
    if not isinstance(status["rotationId"], str) or not status["rotationId"] or media_service not in ("media-plane", "media-plane-blue", "media-plane-green"):
        raise ValueError()
    if any(not re.fullmatch(r"[a-f0-9]{64}", cid) for cid in (cp_id, media_id)) or cp_id == media_id:
        raise ValueError()
    if bindings["control-plane"]["containerId"] != cp_id or bindings[media_service]["containerId"] != media_id:
        raise ValueError()
    if action == "begin":
        if status["state"] != "finalized" or os.path.lexists(path):
            raise ValueError()
        marker = {"rotationId": status["rotationId"], "mediaService": media_service,
                  "containers": {"control-plane": cp_id, "media-plane": media_id}}
        fd, temp = tempfile.mkstemp(prefix=".redis-managed-rebuild.", dir=os.path.dirname(path))
        try:
            with os.fdopen(fd, "w") as output:
                json.dump(marker, output, separators=(",", ":"))
                output.write("\n")
                output.flush()
                os.fsync(output.fileno())
            os.link(temp, path)
            os.unlink(temp)
            temp = None
            directory = os.open(os.path.dirname(path), os.O_RDONLY)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
        finally:
            if temp is not None:
                os.unlink(temp)
    elif action == "verify":
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(fd) as source:
            info = os.fstat(source.fileno())
            if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600 or info.st_uid != os.geteuid() or info.st_nlink != 1:
                raise ValueError()
            marker = json.load(source)
        if set(marker) != {"rotationId", "mediaService", "containers"} or marker["rotationId"] != status["rotationId"] or marker["mediaService"] != media_service:
            raise ValueError()
        old = marker["containers"]
        if set(old) != {"control-plane", "media-plane"} or any(not isinstance(cid, str) or not re.fullmatch(r"[a-f0-9]{64}", cid) for cid in old.values()):
            raise ValueError()
        if not {cp_id, media_id}.isdisjoint(old.values()):
            raise ValueError()
    else:
        raise ValueError()
except (OSError, ValueError, KeyError, TypeError):
    raise SystemExit("redis-managed: rebuild marker is missing, invalid, or does not prove both replacements")
PY
}

redis_managed_rebuild_locked() {
  local VOICE_NEVER_STARTED_CANDIDATES=''
  local status service media_service cp_id='' media_id=''
  if [[ $# -eq 1 && "$1" == --resume ]]; then
    redis_managed_resume_stopped_rebuild rebuild
    return $?
  fi
  [[ $# -eq 0 ]] || { echo 'Usage: concord-ctl.sh redis-managed-rebuild [--resume]' >&2; return 2; }
  [[ ! -e "$REDIS_MANAGED_REBUILD_MARKER" && ! -L "$REDIS_MANAGED_REBUILD_MARKER" ]] || {
    echo 'redis-managed: interrupted rebuild marker exists; use redis-managed-rebuild --resume or operator-reviewed recovery' >&2; return 1;
  }
  redis_managed_policy_preflight || return 1
  redis_managed_host_preflight || return 1
  status="$(redis_managed_control_request '{"operation":"status"}')" || return 1
  python3 -c 'import json,sys; s=json.load(sys.stdin); t=s.get("targets",{}); raise SystemExit(0 if s.get("state")=="finalized" and set(t)=={"control-plane","media-plane"} and all(type(v) is int and v > 0 for v in t.values()) else 1)' <<<"$status" || {
    echo 'redis-managed: rebuild requires finalized single-generation state; use operator-reviewed recovery' >&2; return 1;
  }
  redis_managed_require_registered_state "$status" || return 1
  media_service="$(redis_managed_select_media_service rebuild)" || return 1
  for service in control-plane "$media_service"; do
    local ids
    ids="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q "$service")" || return 1
    [[ "$ids" =~ ^[a-f0-9]{64}$ ]] || { echo "redis-managed: expected one running ${service} before rebuild" >&2; return 1; }
    redis_managed_assert_compatible_image "$ids" "$service" || return 1
    if [[ "$service" == control-plane ]]; then cp_id="$ids"; else media_id="$ids"; fi
  done
  redis_managed_status_acks_preflight "$status" || return 1
  local initial_status="$status"
  redis_managed_assert_same_rotation "$initial_status" || return 1
  redis_managed_rebuild_marker begin "$status" "$media_service" "$cp_id" "$media_id" || return 1
  voice_enforcement_rollout_stop_media || return 1
  (cd "$DEPLOY_DIR" && managed_compose_mutate stop control-plane) || return 1
  redis_managed_stopped_apps || return 1
  redis_managed_assert_same_rotation "$initial_status" || return 1
  local cp_running
  cp_running="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q control-plane)" || return 1
  [[ -z "$cp_running" ]] || { echo 'redis-managed: zero-control-plane migration gate failed' >&2; return 1; }
  for service in control-plane "$media_service"; do
    redis_managed_replace_ready "$service" || return 1
    managed_compose_build build "$service" || return 1
    voice_enforcement_rollout_guard_mutation up "$service" || return 1
    (cd "$DEPLOY_DIR" && $COMPOSE_CMD up --no-start --no-build --no-deps --force-recreate "$service") || return 1
    voice_enforcement_rollout_bind_created "$service" || return 1
    managed_egress_reconcile || return 1
  done
  redis_managed_assert_same_rotation "$initial_status" || return 1
  redis_managed_register_stopped_apps "$media_service" || return 1
  redis_managed_assert_same_rotation "$initial_status" || return 1
  (cd "$DEPLOY_DIR" && managed_compose_mutate start --wait --wait-timeout 180 control-plane) || return 1
  redis_managed_assert_same_rotation "$initial_status" || return 1
  (cd "$DEPLOY_DIR" && managed_compose_mutate start --wait --wait-timeout 180 "$media_service") || return 1
  redis_managed_wait_apps "$media_service" || return 1
  rm -- "$REDIS_MANAGED_REBUILD_MARKER"
}

redis_managed_resume_stopped_rebuild() {
  local VOICE_NEVER_STARTED_CANDIDATES='' VOICE_RESTART_AUTH_FD=''
  local inventory project cp_identity='' media_record='' current_identity
  local mode="${1:-restart}" status media_service service ids bound hostname cp_id='' media_id=''
  case "$mode" in
    restart|rebuild|created) ;;
    *) return 2 ;;
  esac
  redis_managed_marker_preflight required || return 1
  redis_managed_host_preflight || return 1
  redis_managed_validate_mount_model || return 1
  redis_managed_stopped_apps || return 1
  status="$(redis_managed_control_request '{"operation":"status"}')" || return 1
  python3 -c 'import json,sys; s=json.load(sys.stdin); t=s.get("targets",{}); raise SystemExit(0 if s.get("state") in ("finalized","prepared","promoted") and s.get("rotationId") and set(t)=={"control-plane","media-plane"} and all(type(v) is int and v>0 for v in t.values()) else 1)' <<<"$status" || return 1
  media_service="$(redis_managed_select_media_service rebuild)" || return 1
  inventory="$(voice_enforcement_rollout_inventory)" || return 1
  project="${inventory%%$'\n'*}"
  for service in control-plane "$media_service"; do
    ids="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps -a -q "$service")" || return 1
    [[ "$ids" =~ ^[a-f0-9]{64}$ ]] || { echo 'redis-managed: resume requires exactly one stopped container per slot' >&2; return 1; }
    bound="$(python3 -c 'import json,sys; s=json.load(sys.stdin); b=s.get("bindings",{}).get(sys.argv[1],{}); print(b.get("containerId",""))' "$service" <<<"$status")" || return 1
    [[ "$ids" == "$bound" ]] || { echo 'redis-managed: resume container differs from durable ACK binding' >&2; return 1; }
    if [[ "$mode" != restart ]]; then
      voice_enforcement_rollout_bind_created "$service" || return 1
    elif [[ "$service" == control-plane ]]; then
      cp_identity="$(python3 "$SCRIPT_DIR/voice-boot-containment.py" stopped-identity \
        --compose-project "$project" --slot "$service" --container-id "$ids")" || return 1
    else
      media_record="$(voice_enforcement_rollout_read_retired "$project" "$service" "$ids")" || return 1
    fi
    redis_managed_assert_compatible_image "$ids" "$service" || return 1
    if [[ "$service" == control-plane ]]; then cp_id="$ids"; else media_id="$ids"; fi
  done
  if [[ "$mode" == rebuild ]]; then
    redis_managed_rebuild_marker verify "$status" "$media_service" "$cp_id" "$media_id" || return 1
  fi
  for service in control-plane "$media_service"; do
    if [[ "$service" == control-plane ]]; then ids="$cp_id"; else ids="$media_id"; fi
    hostname="$(redis_managed_bind_stopped_instance "$service" "$ids")" || return 1
    [[ -n "$hostname" ]] || return 1
  done
  redis_managed_assert_same_rotation "$status" || return 1
  redis_managed_register_stopped_apps "$media_service" || return 1
  redis_managed_assert_same_rotation "$status" || return 1
  if [[ "$mode" == restart ]]; then
    current_identity="$(python3 "$SCRIPT_DIR/voice-boot-containment.py" stopped-identity \
      --compose-project "$project" --slot control-plane --container-id "$cp_id")" || return 1
    [[ "$cp_identity" == "$current_identity" ]] || return 1
    voice_enforcement_rollout_association association-consume "$project" "$media_service" \
      --container-id "$media_id" <<<"$media_record" || return 1
    # The read end is shared by subshells. A failed or ambiguous attempt cannot
    # reuse a parent-shell variable after the durable record has been consumed.
    exec {VOICE_RESTART_AUTH_FD}< <(printf '%s\n' "$media_record") || return 1
  fi
  (cd "$DEPLOY_DIR" && managed_compose_mutate start --wait --wait-timeout 180 control-plane) || return 1
  redis_managed_assert_same_rotation "$status" || return 1
  (cd "$DEPLOY_DIR" && managed_compose_mutate start --wait --wait-timeout 180 "$media_service") || return 1
  if [[ -n "$VOICE_RESTART_AUTH_FD" ]]; then exec {VOICE_RESTART_AUTH_FD}<&-; fi
  redis_managed_wait_apps "$media_service" || return 1
  [[ "$mode" != rebuild ]] || rm -- "$REDIS_MANAGED_REBUILD_MARKER"
}

redis_managed_restart_bound_stack() {
  local services service ids cp_running media_running status
  local -a supporting=() oneshot=()
  [[ ! -e "$REDIS_MANAGED_REBUILD_MARKER" && ! -L "$REDIS_MANAGED_REBUILD_MARKER" ]] || {
    echo 'redis-managed: interrupted rebuild requires redis-managed-rebuild --resume; refusing restart' >&2; return 1;
  }
  redis_managed_host_preflight || return 1
  redis_managed_validate_mount_model || return 1
  services="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD config --services)" || return 1
  while IFS= read -r service; do
    case "$service" in
      control-plane|media-plane|media-plane-blue|media-plane-green|redis-auth-controller|redis) ;;
      ops-agent)
        # It exits 0 when metrics are disabled, which `start --wait` reports as a failure.
        ids="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps -a -q "$service")" || return 1
        [[ -z "$ids" ]] || oneshot+=("$service")
        ;;
      postgres|minio|nats|coturn|pgadmin|redis-commander)
        ids="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps -a -q "$service")" || return 1
        [[ -z "$ids" ]] || supporting+=("$service")
        ;;
      *) echo 'redis-managed: unsupported service in stopped stack; refusing restart' >&2; return 1 ;;
    esac
  done <<<"$services"
  [[ ${#supporting[@]} -gt 0 ]] || return 1
  cp_running="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q control-plane)" || return 1
  media_running="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q media-plane)" || return 1
  if [[ -n "$cp_running" || -n "$media_running" ]]; then
    [[ -n "$cp_running" && -n "$media_running" ]] || {
      echo 'redis-managed: partial app outage requires operator-reviewed recovery' >&2; return 1;
    }
    status="$(redis_managed_control_request '{"operation":"status"}')" || return 1
    redis_managed_require_registered_state "$status" || return 1
    redis_managed_status_acks_preflight "$status" || return 1
    # Keep CP/Redis transport alive until every exact media predecessor has
    # completed retirement; the all-service stop guard requires that proof.
    voice_enforcement_rollout_stop_media || return 1
    (cd "$DEPLOY_DIR" && managed_compose_mutate stop) || return 1
  fi
  redis_managed_stopped_apps || return 1
  (cd "$DEPLOY_DIR" && managed_compose_mutate start redis-auth-controller) || return 1
  (cd "$DEPLOY_DIR" && managed_compose_mutate start --wait --wait-timeout 180 redis) || return 1
  (cd "$DEPLOY_DIR" && managed_compose_mutate start --wait --wait-timeout 180 redis-auth-controller) || return 1
  (cd "$DEPLOY_DIR" && managed_compose_mutate start --wait --wait-timeout 180 "${supporting[@]}") || return 1
  if (( ${#oneshot[@]} )); then
    (cd "$DEPLOY_DIR" && managed_compose_mutate start "${oneshot[@]}") || return 1
  fi
  redis_managed_resume_stopped_rebuild
}

redis_managed_status_acks_preflight() {
  local status="$1"
  # The status endpoint intentionally exposes no ACK detail; require the
  # controller's finalized state and a live Redis probe before any outage.
  python3 -c 'import json,sys; s=json.loads(sys.argv[1]); raise SystemExit(0 if s.get("state")=="finalized" and set(s.get("instances",{}))=={"control-plane","media-plane"} else 1)' "$status" || return 1
  redis_managed_control_request '{"operation":"redis","args":["GET","media:active_colour"]}' >/dev/null
}

redis_managed_assert_same_rotation() {
  local before="$1" after
  after="$(redis_managed_control_request '{"operation":"status"}')" || {
    echo 'redis-managed: controller state unavailable at maintenance boundary; apps remain stopped' >&2
    return 1
  }
  python3 -c 'import json,sys; a=json.loads(sys.argv[1]); b=json.loads(sys.argv[2]); raise SystemExit(0 if a.get("rotationId")==b.get("rotationId") and a.get("targets")==b.get("targets") else 1)' "$before" "$after" || {
    echo 'redis-managed: policy rotation changed during outage; use operator-reviewed redis-managed-prepare --recover' >&2
    return 1
  }
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
  if ! (cd "$DEPLOY_DIR" && managed_compose_mutate up -d --no-deps --no-build --pull missing --wait --wait-timeout 180 nats) >/dev/null 2>&1; then
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
  local snapshot active standby rendered plan deps rest scope_token ingress_ids start_cp=0 start_media=0 full=0 managed_apps_prepared=0 touches_turn_pair=0
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
      control-plane) start_cp=1; touches_turn_pair=1 ;;
      media-plane) start_media=1 ;;
      media-plane-blue|media-plane-green)
        if [[ "$scope_token" != "media-plane-${standby}" ]]; then
          echo "media-bluegreen: ${scope_token} is not the resolved standby; refusing" >&2
          return 1
        fi
        start_media=1 ;;
      coturn) touches_turn_pair=1 ;;
      ops-agent|postgres|redis|nats|minio) ;;
      *) echo "rebuild: invalid or unknown service '${scope_token}'" >&2; return 2 ;;
    esac
  done
  # Same coturn/control-plane pairing as the single-colour path, but this path
  # cannot escalate a scope, so it refuses.
  if (( full == 0 && touches_turn_pair == 1 )) && ! turn_managed_scoped_rebuild_pair "$@"; then
    echo 'media-bluegreen: rebuild coturn and control-plane together, or run an unscoped rebuild' >&2
    return 1
  fi
  if [[ "$REDIS_CREDENTIAL_MODE" == managed ]]; then
    if (( start_cp == 1 )); then
      echo 'redis-managed: ordinary control-plane rebuild requires redis-managed-rebuild for the explicit full outage' >&2
      return 1
    fi
    if (( $# != 1 || start_media != 1 )); then
      echo 'redis-managed: ordinary blue/green rebuild supports one media-colour scope only' >&2
      return 1
    fi
  fi
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
  if [[ "$REDIS_CREDENTIAL_MODE" == managed ]] && (( start_media == 1 || start_cp == 1 )); then
    redis_managed_require_registered_state || return 1
  fi
  # A full rebuild starts coturn in its deps or rest stage, a scoped one only
  # when named; either way the RAM database must exist first (#3931).
  local starts_coturn=$full
  for scope_token in "$@"; do
    [[ "$scope_token" == coturn ]] && starts_coturn=1
  done
  if (( starts_coturn == 1 )); then
    turn_managed_init_schema_before_coturn || return 1
  fi
  activity_history_routine_guard || return 1
  managed_production_nats_reconcile || return 1
  if (( full == 1 )) && [[ ${#deps_arr[@]} -gt 0 ]]; then
    managed_compose_build up -d --build --no-deps --wait --wait-timeout 180 -- "${deps_arr[@]}" || return 1
  fi
  if [[ "$REDIS_CREDENTIAL_MODE" == managed && "$start_media" == 1 ]]; then
    if (( start_cp == 1 )); then
      clear_activity_history_image_selector || return 1
      privacy_defaults_refuse_foreign_control_plane || return 1
    fi
    redis_managed_prepare_registered_apps "$standby" "${active:-none}" "$full" "$start_cp" || return 1
    managed_apps_prepared=1
  elif [[ "$REDIS_CREDENTIAL_MODE" == managed && "$start_cp" == 1 ]]; then
    echo 'redis-managed: control-plane-only replacement requires a registered candidate; refusing before drain' >&2
    return 1
  fi
  if (( start_cp == 1 )); then
    if [[ "$managed_apps_prepared" == 1 ]]; then
      : # cold prepare already created, registered, started, and proved this instance
    else
    clear_activity_history_image_selector || return 1
    managed_compose_build build control-plane || return 1
    privacy_defaults_refuse_foreign_control_plane || return 1
    activity_history_stop_control_plane || return 1
    privacy_defaults_require_local_control_plane_drained || return 1
    (cd "$DEPLOY_DIR" && managed_compose_mutate up -d --no-build --no-deps --wait --wait-timeout 180 control-plane) || return 1
    voice_enforcement_rollout_wait_control_plane || return 1
    fi
  fi
  if (( start_media == 1 )); then
    if [[ "${COMPOSE_OVERRIDE:-}" == docker-compose.staging.yml ]]; then
      ingress_ids="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q staging-media-proxy)" || return 1
      if [[ -z "$ingress_ids" ]]; then
        (cd "$DEPLOY_DIR" && managed_compose_mutate up -d --no-deps --wait --wait-timeout 180 staging-media-proxy) || return 1
      fi
      media_bluegreen_dependencies_ready staging-media-proxy || return 1
    else
      media_bluegreen_dependencies_ready || return 1
    fi
    media_bluegreen_cutover_locked || return 1
    voice_enforcement_rollout_activate || return 1
  fi
  if (( full == 1 )) && [[ ${#rest_arr[@]} -gt 0 ]]; then
    managed_compose_build up -d --build --no-deps -- "${rest_arr[@]}" || return 1
  elif (( full == 0 )); then
    local -a extra=()
    for scope_token in "$@"; do
      case "$scope_token" in
        control-plane|media-plane|media-plane-blue|media-plane-green) ;;
        *) extra+=("$scope_token") ;;
      esac
    done
    if [[ ${#extra[@]} -gt 0 ]]; then
      managed_compose_build up -d --build --no-deps --wait --wait-timeout 180 -- "${extra[@]}" || return 1
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
    if [[ ${STAGE_RESTORE_MODE:-0} -eq 0 ]]; then
      managed_compose_build up -d --build --no-deps --wait --wait-timeout 180 -- "${closure_arr[@]}"
    else
      (cd "$DEPLOY_DIR" && managed_compose_mutate up -d --no-build --pull never --no-deps --wait --wait-timeout 180 -- \
        "${closure_arr[@]}")
    fi
  fi
  voice_enforcement_rollout_deploy || return 1
  if [[ ${#rest_arr[@]} -gt 0 ]]; then
    # No --wait: coturn's STUN probe flaking would abort a deploy that
    # otherwise succeeded, and post-rsync's wait-healthy follows anyway.
    echo "rebuild: starting the remaining services: ${rest_arr[*]}"
    if [[ ${STAGE_RESTORE_MODE:-0} -eq 0 ]]; then
      managed_compose_build up -d --build --no-deps -- "${rest_arr[@]}"
    else
      (cd "$DEPLOY_DIR" && managed_compose_mutate up -d --no-build --pull never --no-deps -- "${rest_arr[@]}")
    fi
  fi
}

# nginx-reload and sudoers-sync both use fixed root-owned candidate paths.
# Serialize them on one host-global inode so a copied or symlinked controller
# cannot overlap post-rsync (or another direct invocation). Provisioning creates
# this file inside a root-owned directory, owned by concord but not unlinkable by
# concord. Keeping that inode is load-bearing: kernel lock ownership, not file
# existence, controls admission.
DEPLOY_TRANSACTION_LOCK_PATH="/var/lib/concord/deploy-transaction.lock"
if [[ ! -d /var/lib/concord ]]; then
  DEPLOY_TRANSACTION_LOCK_PATH="$(cd "$CONFIG_DEPLOY_DIR" && pwd -P)/.deploy-transaction.lock" || exit 1
fi
readonly DEPLOY_TRANSACTION_LOCK_PATH
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

  # Self-hosted aliases/copies using this physical configuration directory
  # contend on the same retained fallback inode. Never unlink it on release.
  # The managed-only verbs never take that fallback: their root-owned inode
  # under /var/lib/concord is the only admission they accept.
  if [[ "$operation" == nginx-reload || "$operation" == sudoers-sync ]] \
     && [[ "$DEPLOY_TRANSACTION_LOCK_PATH" != /var/lib/concord/* ]]; then
    echo "  ✗ ${operation}: managed deploy transaction lock is missing (no /var/lib/concord)" >&2
    exit 1
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

if [[ "${1:-help}" == docker-clean ]]; then
  # Reject typos before waiting for the lock or invoking the admission gate.
  case "${2:-}" in
    "") AGGRESSIVE=0 ;;
    --aggressive) AGGRESSIVE=1 ;;
    *) echo "docker-clean: unknown argument '${2}'. Only --aggressive is supported." >&2; exit 2 ;;
  esac
  [[ $# -le 2 ]] || { echo 'docker-clean: unexpected extra arguments' >&2; exit 2; }
fi

case "${1:-help}" in
  nginx-reload|sudoers-sync)
    # These early post-rsync transactions wait for an in-progress actor cycle.
    DEPLOY_LOCK_WAIT_SECS=90
    acquire_deploy_transaction_lock "$@"
    ;;
  rebuild|stage-restore|restart|stop|docker-clean|activity-history-activate|activity-history-rollback|voice-enforcement-rollout|media-reap|media-legacy-pause|media-legacy-resume|redis-managed-prepare|redis-managed-rebuild|redis-managed-register-running|redis-managed-rollback|turn-managed-init-schema|turn-managed-bootstrap|turn-managed-add-candidate|turn-managed-promote|turn-managed-rollback|turn-managed-recover|turn-managed-discard-unrecorded|turn-managed-status)
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
    DEPLOY_LOCK_WAIT_SECS=90
    acquire_deploy_transaction_lock "$@"
    if [[ "${1:-}" != docker-clean && "$ACTIVITY_HISTORY_CONTRACT_MODE" == managed \
       && "$COMPOSE_OVERRIDE" == docker-compose.production.yml ]]; then
      redis_managed_writer_consistency_guard || exit 1
      # Re-read the canonical pair under the lock (#3931), exactly as Redis does.
      turn_managed_writer_consistency_guard || exit 1
    fi
    # This is deliberately inside the existing deploy transaction lock. A
    # commissioned policy may change only outside a running deployment; all
    # partial states refuse before NATS or Compose can mutate namespaces.
    managed_egress_gate verify || exit 1
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
    redis_managed_host_preflight || exit 1
    turn_managed_restart_guard || exit 1
    turn_managed_init_schema_before_coturn || exit 1
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
    if [[ "$REDIS_CREDENTIAL_MODE" == managed && "$MEDIA_DEPLOY_PROFILE" == media-single ]]; then
      redis_managed_restart_bound_stack || exit 1
      selfhost_post_up_check || exit 1
      selfhost_tls_ready_gate || exit 1
      echo "Done."
      exit 0
    fi
    voice_enforcement_rollout_stop_media || exit 1
    (cd "$DEPLOY_DIR" && managed_compose_mutate restart) || exit 1
    voice_enforcement_rollout_wait_control_plane || exit 1
    voice_enforcement_rollout_wait_media || exit 1
    voice_enforcement_rollout_activate || exit 1
    voice_enforcement_rollout_wait_media productive || exit 1
    selfhost_post_up_check || exit 1
    selfhost_tls_ready_gate || exit 1
    echo "Done."
    ;;

  redis-managed-prepare)
    [[ "$REDIS_CREDENTIAL_MODE" == managed ]] || { echo 'redis-managed-prepare requires REDIS_CREDENTIAL_MODE=managed' >&2; exit 2; }
    turn_managed_pair_guard || exit 1
    redis_managed_prepare_locked "${@:2}" || exit $?
    ;;

  redis-managed-rebuild)
    [[ "$REDIS_CREDENTIAL_MODE" == managed ]] || { echo 'redis-managed-rebuild requires REDIS_CREDENTIAL_MODE=managed' >&2; exit 2; }
    turn_managed_pair_guard || exit 1
    redis_managed_rebuild_locked "${@:2}" || exit $?
    ;;

  redis-managed-register-running)
    [[ "$REDIS_CREDENTIAL_MODE" == managed ]] || { echo 'redis-managed-register-running requires REDIS_CREDENTIAL_MODE=managed' >&2; exit 2; }
    turn_managed_pair_guard || exit 1
    redis_managed_register_running_locked "${@:2}" || exit $?
    ;;

  redis-managed-rollback)
    [[ "$REDIS_CREDENTIAL_MODE" == managed ]] || { echo 'redis-managed-rollback requires REDIS_CREDENTIAL_MODE=managed' >&2; exit 2; }
    redis_managed_rollback_locked "${@:2}" || exit $?
    ;;

  turn-managed-init-schema|turn-managed-bootstrap|turn-managed-add-candidate|turn-managed-promote|turn-managed-rollback|turn-managed-recover|turn-managed-discard-unrecorded|turn-managed-status)
    turn_managed_verb_locked "$@" || exit $?
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
    redis_managed_host_preflight || exit 1
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
    if [[ "$REDIS_CREDENTIAL_MODE" == managed ]]; then
      echo 'redis-managed: ordinary rebuild is refused; use redis-managed-rebuild for the explicit full outage' >&2
      exit 1
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

    if [[ $_rebuild_scoped -eq 1 ]] && ! turn_managed_scoped_rebuild_pair "${REBUILD_SCOPE[@]}"; then
      echo "rebuild: coturn and control-plane must run one TURN credential model — escalating to a full rebuild." >&2
      _rebuild_scoped=0
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
      # Managed TURN: the RAM database must exist (empty on a cold host) before
      # coturn starts; this is the only place that creates it (#3931).
      turn_managed_init_schema_before_coturn || exit 1
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
      if [[ $STAGE_RESTORE_MODE -eq 0 ]] && ! managed_compose_build build; then
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
        voice_enforcement_rollout_deploy || exit 1
      else
        voice_enforcement_rollout_media_rebuild || exit 1
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
      managed_compose_build build "${REBUILD_SCOPE[@]}" || exit 1
      echo "rebuild: draining the previous control-plane before its startup migrations"
      privacy_defaults_refuse_foreign_control_plane || exit 1
      activity_history_stop_control_plane || exit 1
      privacy_defaults_require_local_control_plane_drained || exit 1
      cd "$DEPLOY_DIR" && managed_compose_mutate up -d --no-build --no-deps --wait --wait-timeout 180 "${REBUILD_SCOPE[@]}"
    else
      managed_compose_build up -d --build --no-deps --wait --wait-timeout 180 "${REBUILD_SCOPE[@]}"
    fi
    selfhost_post_up_check || exit 1
    [[ $_rebuild_starts_coturn -eq 0 ]] || selfhost_tls_ready_gate || exit 1
    echo "Done."
    ;;

  media-reap)
    shift
    if [[ $# -ne 0 ]]; then
      echo 'Usage: concord-ctl.sh media-reap' >&2
      exit 2
    fi
    media_bluegreen_reap
    ;;

  media-legacy-pause)
    shift
    if [[ $# -eq 2 && "$1" == --allow-user ]]; then
      media_legacy_pause "$2" || exit 1
    elif [[ $# -eq 0 ]]; then
      media_legacy_pause || exit 1
    else
      echo 'Usage: concord-ctl.sh media-legacy-pause [--allow-user <uuid>]' >&2
      exit 2
    fi
    ;;

  media-legacy-resume)
    shift
    if [[ $# -ne 0 ]]; then
      echo 'Usage: concord-ctl.sh media-legacy-resume' >&2
      exit 2
    fi
    media_legacy_resume || exit 1
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
    turn_managed_pair_guard || exit 1
    activity_history_activate "$@"
    selfhost_post_up_check || exit 1
    ;;

  activity-history-rollback)
    shift
    turn_managed_pair_guard || exit 1
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
        turn_managed_pair_guard || exit 1
        media_shared_listeners_routine_guard || exit 1
        managed_production_nats_reconcile || exit 1
        voice_enforcement_rollout_deploy "$@" || exit 1
        selfhost_post_up_check || exit 1
        ;;
      retire-legacy-predecessor)
        shift
        if [[ $# -ne 0 ]]; then
          echo 'Usage: concord-ctl.sh voice-enforcement-rollout retire-legacy-predecessor' >&2
          exit 2
        fi
        voice_enforcement_rollout_retire_legacy || exit 1
        ;;
      *)
        echo "Usage: concord-ctl.sh voice-enforcement-rollout deploy|retire-legacy-predecessor" >&2
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
    control_plane_cli storage-probe "$@"
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
    egress_mode="$(managed_egress_mode)" || exit 1
    cp_ok=0; mp_ok=0; coturn_ok=0; tls_ok=0; egress_ok=1
    cp_refresh_skipped=0; mp_refresh_skipped=0; coturn_refresh_skipped=0
    while [[ $(date +%s) -lt $DEADLINE ]]; do
      cp_ok=0; mp_ok=0; coturn_ok=0; tls_ok=0; egress_ok=1
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
      if [[ "$egress_mode" == managed ]]; then
        egress_ok=0
        managed_egress_gate ready && egress_ok=1
      fi
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
          [[ $cp_ok -eq 1 && $mp_ok -eq 1 && $coturn_ok -eq 1 && $tls_ok -eq 1 && $egress_ok -eq 1 ]]; then
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
    [[ "$egress_mode" != managed || $egress_ok -eq 1 ]] \
      || echo '    managed egress readiness failed' >&2
    [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" != selfhost || $tls_ok -eq 1 ]] \
      || echo "    coturn TLS listener proof failed" >&2
    dump_health_diagnostics
    exit 1
    ;;

  freshstart)
    if [[ "$REDIS_CREDENTIAL_MODE" == managed ]]; then
      echo 'redis-managed: freshstart bypasses the reviewed Redis identity recovery path; refusing volume wipe' >&2
      exit 1
    fi
    if [[ "$TURN_CREDENTIAL_MODE" == managed ]]; then
      echo 'turn-managed: freshstart bypasses the reviewed RAM key-store lifecycle; refusing volume wipe' >&2
      exit 1
    fi
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
    if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == managed \
       && "$COMPOSE_OVERRIDE" == docker-compose.production.yml ]]; then
      redis_managed_writer_consistency_guard || exit 1
      # The prompt above is unbounded: a TURN mode written meanwhile must not
      # reach the wipe with the model computed before it (#3931).
      turn_managed_writer_consistency_guard || exit 1
    fi
    fi

    managed_egress_gate verify || exit 1
    managed_production_nats_reconcile model-only || exit 1
    clear_activity_history_image_selector
    activity_history_invalidate_activation_receipt
    echo ""
    echo "Stopping all services and removing volumes..."
    voice_enforcement_rollout_stop_media || exit 1
    (cd "$DEPLOY_DIR" && managed_compose_mutate down -v) || exit 1
    managed_production_nats_reconcile || exit 1
    echo ""
    # #1015: sync_releases no longer auto-called. If the server's
    # /opt/concord/releases/ is empty or stale, trigger sync-releases.yml
    # OR run `concord-ctl.sh sync-releases` before invoking freshstart.
    echo "Rebuilding and starting fresh..."
    echo "Note: release assets NOT refreshed automatically (#1015). Run 'concord-ctl.sh sync-releases' or trigger sync-releases.yml AFTER freshstart completes to populate /opt/concord/releases/."
    managed_compose_build up -d --build
    if ! voice_enforcement_rollout_wait_control_plane \
      || ! voice_enforcement_rollout_wait_media \
      || ! voice_enforcement_rollout_activate; then
      echo "Fresh start failed the voice-enforcement capability/activation gate; client admission is unverified." >&2
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
    # Stale-MinIO recovery refuses on its own faults; it runs before media retirement.
    if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]]; then
      selfhost_storage_stop_stale_minio || exit 1
    fi
    voice_enforcement_rollout_stop_media || exit 1
    if [[ "$REDIS_CREDENTIAL_MODE" == managed ]]; then
      cd "$DEPLOY_DIR" && managed_compose_mutate stop || exit 1
    else
      cd "$DEPLOY_DIR" && managed_compose_mutate down || exit 1
    fi
    echo "Done."
    ;;

  *)
    echo "Concord Service Manager"
    echo ""
    echo "Usage: $0 <command>"
    echo ""
    echo "Commands:"
    echo "  redis-managed-prepare --first-activation | --recover | --recover-lost-state <proof.json>"
    echo "                 Explicit stopped activation or reviewed recovery"
    echo "  redis-managed-rebuild [--resume]   Planned full app outage or resume bound stopped containers"
    echo "  redis-managed-register-running     Register the running single-media apps for an in-place rotation"
    echo "  redis-managed-rollback             Stop managed Redis clients, remove the controller, stop Redis"
    echo "  turn-managed-{init-schema|bootstrap|add-candidate|promote|rollback|recover|discard-unrecorded|status} [--generation N]"
    echo "                 Managed TURN key custody (#3931). Key material is read from stdin only;"
    echo "                 the canonical writer pair must record TURN_CREDENTIAL_MODE=managed"
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
    echo "                 Build CP, retire/rebuild MP, verify protocol 4, activate"
    echo "  media-reap"
    echo "                 Reap a draining color when empty or at its automatic 2-hour cutoff;"
    echo "                 on a paused legacy host, stop legacy when empty or at the cutoff"
    echo "  media-legacy-pause [--allow-user <uuid>]"
    echo "                 Pause new voice joins on legacy media and start the 2-hour clock"
    echo "  media-legacy-resume"
    echo "                 Lift the legacy join pause (abort, or after the colour is proved)"
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
