#!/usr/bin/env bash
# Shared read-only self-host storage contract. Sourcing performs no discovery or I/O.
# Results and checkpoint state never inherit caller interpolation authority.
unset SELFHOST_STORAGE_MODE_RESOLVED SELFHOST_STORAGE_PROJECT_ROOT \
  SELFHOST_STORAGE_PROJECT_NAME SELFHOST_STORAGE_COMPOSE_ARGS \
  SELFHOST_STORAGE_MINIO_IDS SELFHOST_STORAGE_TRUSTED_IMAGE SELFHOST_STORAGE_IMAGE
SELFHOST_STORAGE_MODE_RESOLVED=''
SELFHOST_STORAGE_PROJECT_ROOT=''
SELFHOST_STORAGE_PROJECT_NAME=''
SELFHOST_STORAGE_COMPOSE_ARGS=()
SELFHOST_STORAGE_MINIO_IDS=()
SELFHOST_STORAGE_TRUSTED_IMAGE=''

_selfhost_storage_refuse() {
  printf '%s\n' "$1" >&2
  return 1
}

_selfhost_storage_clear_resolution() {
  SELFHOST_STORAGE_MODE_RESOLVED=''
  SELFHOST_STORAGE_PROJECT_ROOT=''
  SELFHOST_STORAGE_PROJECT_NAME=''
  SELFHOST_STORAGE_COMPOSE_ARGS=()
  SELFHOST_STORAGE_MINIO_IDS=()
  export -n SELFHOST_STORAGE_MODE_RESOLVED SELFHOST_STORAGE_PROJECT_ROOT \
    SELFHOST_STORAGE_PROJECT_NAME SELFHOST_STORAGE_COMPOSE_ARGS SELFHOST_STORAGE_MINIO_IDS
}

_selfhost_storage_root() {
  [[ $# -eq 1 && "$1" == /* && -d "$1" ]] || return 1
  (cd -P -- "$1" 2>/dev/null && pwd -P) || return 1
}

# Only validated mode/identity/IDs/verdicts leave this data boundary. Credentials
# enter through stdin (or the sanctioned file), never argv or exported variables.
# Reader and installer validation share this single validator and endpoint parser.
_selfhost_storage_data() {
  python3 -c '
import ipaddress, json, re, sys, unicodedata

FIELDS = ("STORAGE_BACKEND", "STORAGE_ENDPOINT", "STORAGE_REGION",
          "STORAGE_ACCESS_KEY", "STORAGE_SECRET_KEY", "STORAGE_USE_SSL", "STORAGE_BUCKET")
PROJECT = r"[a-z0-9][a-z0-9_-]*"
PUBLIC_PROJECT = r"concord-selfhost-[a-z0-9][a-z0-9_-]*"
# Retained data under these logical keys is ambiguous even in another Compose
# project. Refuse it rather than authorize a fresh installation over prior data.
CONCORD_DATA_VOLUMES = {"postgres-data", "redis-data", "minio-data", "nats-data"}
CONCORD_CONTAINERS = {"concordvoice-" + service for service in (
    "postgres", "redis", "nats", "coturn", "minio", "ops-agent",
    "control-plane", "media-plane", "pgadmin", "redis-commander")}

def require(test):
    if not test:
        raise ValueError()

def endpoint_host(value):
    require(value and value.isascii() and not re.search(r"[\s\\/@?#]", value))
    if value.startswith("["):
        match = re.fullmatch(r"\[([^\[\]]+)\](?::([0-9]+))?", value)
        require(match is not None)
        host, port = match.groups()
        require("%" not in host)
        address = ipaddress.IPv6Address(host)
    else:
        match = re.fullmatch(r"([^:\[\]]+)(?::([0-9]+))?", value)
        require(match is not None)
        host, port = match.groups()
        try:
            address = ipaddress.IPv4Address(host)
        except ValueError:
            require(not re.fullmatch(r"[0-9.]+", host))
            dns = host[:-1] if host.endswith(".") else host
            require(len(dns) <= 253 and all(re.fullmatch(
                r"[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?", label)
                for label in dns.split(".")))
            address = None
    require(port is None or 1 <= int(port) <= 65535)
    # TLS does not make a non-host destination usable from the control-plane
    # container. Normalize mapped IPv4 before checking its address class.
    destination = address
    if isinstance(destination, ipaddress.IPv6Address) and destination.ipv4_mapped is not None:
        destination = destination.ipv4_mapped
    require(destination is None or not (destination.is_unspecified or destination.is_link_local
            or destination.is_multicast or destination == ipaddress.IPv4Address("255.255.255.255")))
    return host, address

def private_destination(host, address):
    if address is None:
        host = host.lower()
        if host.endswith("."):
            host = host[:-1]
        return host in ("minio", "localhost")
    if isinstance(address, ipaddress.IPv6Address) and address.ipv4_mapped is not None:
        address = address.ipv4_mapped
    ranges = (("127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16")
              if address.version == 4 else ("::1/128", "fc00::/7"))
    return any(address in ipaddress.ip_network(network) for network in ranges)

def validate(values):
    require(len(values) == 8)
    mode, backend, endpoint, region, access, secret, tls, bucket = values
    require(all("\x00" not in value and "\r" not in value and "\n" not in value
                for value in values))
    require(mode in ("bundled", "byo-s3"))
    if mode == "bundled":
        require(not any(values[1:]))
        return
    require(backend in ("minio", "s3", "r2", "b2") and tls in ("true", "false"))
    require(all((endpoint, access, secret, bucket)))
    require(all(unicodedata.category(c) != "Cc" for value in
                (mode, backend, endpoint, region, tls, bucket) for c in value))
    # Match minio-go/v7 v7.3.0 s3utils.CheckValidBucketName, the non-strict
    # consumer shared by all supported backends. Uppercase, _ and : are valid.
    require(re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.\-_:]{1,61}[A-Za-z0-9]", bucket) is not None)
    require(not any(part in bucket for part in ("..", ".-", "-.")))
    require(re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+", bucket) is None)
    host, address = endpoint_host(endpoint)
    require(tls == "true" or private_destination(host, address))

def decode(value):
    if not value.startswith("\""):
        require(re.fullmatch(r"[A-Za-z0-9_./:@+,\[\]-]*", value) is not None)
        return value
    require(len(value) >= 2 and value.endswith("\""))
    body, result, i = value[1:-1], [], 0
    while i < len(body):
        c = body[i]
        if c == "\\":
            require(i + 1 < len(body) and body[i + 1] in ("\\", "\""))
            result.append(body[i + 1]); i += 2
        elif c == "$":
            require(i + 1 < len(body) and body[i + 1] == "$")
            result.append("$"); i += 2
        else:
            require(c != "\"")
            result.append(c); i += 1
    return "".join(result)

def project_name(value):
    require(isinstance(value, str) and re.fullmatch(PROJECT, value) is not None)
    return value

def container_metadata(text):
    decoder = json.JSONDecoder()
    labels, end = decoder.raw_decode(text)
    remainder = text[end:]
    require(remainder.startswith("|"))
    state = json.loads(remainder[1:])
    require(isinstance(labels, dict) and isinstance(state, dict))
    require(isinstance(state.get("Running"), bool))
    project_name(labels.get("com.docker.compose.project"))
    return labels, state

def minio_active(state):
    require(all(isinstance(state.get(key), bool)
                for key in ("Running", "Paused", "Restarting")))
    status = state.get("Status")
    running, paused, restarting = (state[key] for key in ("Running", "Paused", "Restarting"))
    if status in ("created", "exited"):
        require(not (running or paused or restarting))
        return False
    if status == "restarting":
        require(restarting and not paused)
    elif status == "paused":
        require(running and paused and not restarting)
    elif status == "running":
        require(running and not paused and not restarting)
    else:
        raise ValueError()
    return True

try:
    operation, *args = sys.argv[1:]
    if operation == "validate":
        parts = sys.stdin.buffer.read().split(b"\x00")
        require(parts[-1] == b"")
        validate([part.decode("utf-8") for part in parts[:-1]])
    elif operation in ("read", "read-tls"):
        text = open(args[0], "rb").read().decode("utf-8")
        require("\x00" not in text and "\r" not in text)
        found = {}
        key_pattern = (r"SELFHOST_STORAGE_MODE|STORAGE_[A-Za-z0-9_]+"
                       if operation == "read" else r"DOMAIN|COTURN_CERTS_DIR")
        for line in text.split("\n"):
            if not line.strip() or line.lstrip().startswith("#"):
                continue
            match = re.match(r"\s*(?:export\s+)?(" + key_pattern + r")\s*(.*)", line)
            if match is None:
                continue
            key, remainder = match.groups()
            if operation == "read":
                require(key == "SELFHOST_STORAGE_MODE" or key in FIELDS)
            else:
                require(key in ("DOMAIN", "COTURN_CERTS_DIR"))
            require(key not in found and remainder.startswith("="))
            found[key] = decode(remainder[1:])
        if operation == "read-tls":
            domain = found.get("DOMAIN", "")
            selector = found.get("COTURN_CERTS_DIR", "")
            require(set(found) == {"DOMAIN", "COTURN_CERTS_DIR"})
            require(re.fullmatch(r"([A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?\.)+[A-Za-z]{2,}", domain) is not None)
            require(not domain.startswith(("media.", "api.", "turn.")))
            require(selector == "/opt/concord/certs/coturn")
            print(json.dumps({"domain": domain, "cert_root": selector}, separators=(",", ":")))
            sys.exit(0)
        mode = found.get("SELFHOST_STORAGE_MODE", "bundled")
        supplied = [key for key in FIELDS if key in found]
        require(not supplied if mode == "bundled" else
                "SELFHOST_STORAGE_MODE" in found and len(supplied) == len(FIELDS))
        values = [mode] + [found.get(key, "") for key in FIELDS]
        validate(values)
        print(mode)
    elif operation == "identity":
        model = json.load(sys.stdin)
        require(isinstance(model, dict))
        print(project_name(model.get("name")))
    elif operation in ("ids", "volumes"):
        values = sys.stdin.read().splitlines()
        pattern = r"[0-9a-f]{12,64}" if operation == "ids" else r"[A-Za-z0-9][A-Za-z0-9_.-]*"
        require(len(values) <= 1000 and len(values) == len(set(values)))
        require(all(re.fullmatch(pattern, value) for value in values))
        if values:
            print("\n".join(values))
    elif operation == "volume":
        labels = json.load(sys.stdin)
        require(isinstance(labels, dict))
        project = project_name(labels.get("com.docker.compose.project"))
        print("match" if project == args[0] else "other")
    elif operation == "fresh-host-volume":
        labels = json.load(sys.stdin)
        require(isinstance(labels, dict))
        project = project_name(labels.get("com.docker.compose.project"))
        # A renamed checkout can leave only volumes after down. Require typed
        # logical metadata for every discovered Compose-project volume before
        # judging the exact persistent keys; never infer identity from its name.
        volume = labels.get("com.docker.compose.volume")
        require(isinstance(volume, str) and re.fullmatch(
            r"[A-Za-z0-9][A-Za-z0-9_.-]*", volume) is not None)
        match = re.fullmatch(PUBLIC_PROJECT, project) is not None or volume in CONCORD_DATA_VOLUMES
        print("match" if match else "other")
    elif operation == "fresh-host-container":
        # Only Name/Labels/State are queried; never read Config.Env or emit
        # metadata. A stopped renamed release container still owns its project.
        decoder, remaining, objects = json.JSONDecoder(), sys.stdin.read(), []
        for index in range(3):
            value, end = decoder.raw_decode(remaining)
            objects.append(value)
            remaining = remaining[end:]
            if index < 2:
                require(remaining.startswith("|"))
                remaining = remaining[1:]
        require(not remaining.strip())
        name, labels, state = objects
        require(isinstance(name, str) and re.fullmatch(
            r"/[A-Za-z0-9][A-Za-z0-9_.-]{0,254}", name) is not None)
        require(labels is None or isinstance(labels, dict))
        require(isinstance(state, dict) and isinstance(state.get("Running"), bool))
        labels = {} if labels is None else labels
        project = None
        if "com.docker.compose.project" in labels:
            project = project_name(labels["com.docker.compose.project"])
        match = name[1:] in CONCORD_CONTAINERS or (
            project is not None and re.fullmatch(PUBLIC_PROJECT, project) is not None)
        print("match" if match else "other")
    elif operation in ("fresh-container", "minio-container"):
        labels, state = container_metadata(sys.stdin.read())
        running = state["Running"]
        project = labels["com.docker.compose.project"]
        if operation == "fresh-container":
            working = labels.get("com.docker.compose.project.working_dir")
            require(isinstance(working, str) and working.startswith("/"))
            match = project == args[0] or working == args[1]
        else:
            service = labels.get("com.docker.compose.service")
            require(isinstance(service, str) and re.fullmatch(PROJECT, service) is not None)
            match = project == args[0] and service == "minio"
            if match:
                running = minio_active(state)
        print(("running" if running else "stopped") if match else "other")
    else:
        raise ValueError()
except (ValueError, TypeError, KeyError, IndexError, OSError, UnicodeError):
    sys.exit(1)
' "$@" 2>/dev/null
}

selfhost_storage_validate() {
  [[ $# -eq 8 ]] || { _selfhost_storage_refuse 'selfhost-storage: invalid storage configuration'; return 1; }
  if ! printf '%s\0' "$@" | _selfhost_storage_data validate; then
    _selfhost_storage_refuse 'selfhost-storage: invalid storage configuration'
    return 1
  fi
}

selfhost_storage_read() {
  SELFHOST_STORAGE_MODE_RESOLVED=''
  export -n SELFHOST_STORAGE_MODE_RESOLVED
  local root mode
  if [[ $# -ne 1 ]] || ! root="$(_selfhost_storage_root "$1")" \
    || ! mode="$(_selfhost_storage_data read "$root/.env")"; then
    _selfhost_storage_clear_resolution
    _selfhost_storage_refuse 'selfhost-storage: storage environment refused'
    return 1
  fi
  SELFHOST_STORAGE_MODE_RESOLVED="$mode"
}

selfhost_storage_scrub_environment() {
  # --env-file is lower precedence than shell interpolation. These values,
  # including the complete MFA keyring, must come from durable .env.
  unset ENVIRONMENT DOMAIN PUBLIC_IP POSTGRES_PASSWORD REDIS_PASSWORD \
    JWT_SECRET MFA_ENCRYPTION_KEY MFA_ENCRYPTION_KEY_VERSION MFA_ENCRYPTION_KEYS_RETIRED \
    TURN_SECRET TURN_REALM TRUSTED_PROXY_CIDRS \
    SMTP_HOST SMTP_PORT SMTP_USERNAME SMTP_PASSWORD SMTP_FROM FEEDBACK_REPO FEEDBACK_PAT \
    RESEND_API_KEY || return 1
  unset SELFHOST_STORAGE_MODE STORAGE_BACKEND STORAGE_ENDPOINT STORAGE_REGION \
    STORAGE_ACCESS_KEY STORAGE_SECRET_KEY STORAGE_USE_SSL STORAGE_BUCKET \
    MINIO_ENDPOINT MINIO_ACCESS_KEY MINIO_SECRET_KEY MINIO_USE_SSL MINIO_BUCKET \
    MINIO_ROOT_USER MINIO_ROOT_PASSWORD COMPOSE_FILE COMPOSE_PROFILES \
    COMPOSE_PROJECT_NAME COMPOSE_ENV_FILES COMPOSE_DISABLE_ENV_FILE COMPOSE_OVERRIDE \
    INSTANCE_TYPE ACTIVITY_HISTORY_CLUSTER_ENABLED CONTROL_PLANE_REPLICA_COUNT \
    ACTIVITY_HISTORY_OPERATOR_NAME ACTIVITY_HISTORY_PRIVACY_POLICY_URL \
    ATTACHMENT_WRITE_BACKEND CONTROL_PLANE_IMAGE COTURN_CERTS_DIR
}

selfhost_storage_project_identity() {
  SELFHOST_STORAGE_PROJECT_NAME=''
  export -n SELFHOST_STORAGE_PROJECT_NAME
  local root model name
  if [[ $# -ne 1 ]] || ! root="$(_selfhost_storage_root "$1")"; then
    _selfhost_storage_refuse 'selfhost-storage: project identity unavailable'
    return 1
  fi
  if ! model="$(
    selfhost_storage_scrub_environment || exit 1
    cd -- "$root" || exit 1
    docker compose --project-directory "$root" --env-file /dev/null -f - config --format json 2>/dev/null <<'YAML'
services:
  identity:
    image: scratch
YAML
  )" || ! name="$(printf '%s' "$model" | _selfhost_storage_data identity)"; then
    _selfhost_storage_refuse 'selfhost-storage: project identity unavailable'
    return 1
  fi
  SELFHOST_STORAGE_PROJECT_NAME="$name"
}

selfhost_storage_resolve() {
  _selfhost_storage_clear_resolution
  local root model rendered_name file
  if [[ $# -ne 1 ]] || ! root="$(_selfhost_storage_root "$1")" \
    || ! selfhost_storage_read "$root" || ! selfhost_storage_project_identity "$root"; then
    _selfhost_storage_clear_resolution
    _selfhost_storage_refuse 'selfhost-storage: model resolution refused'
    return 1
  fi
  SELFHOST_STORAGE_PROJECT_ROOT="$root"
  SELFHOST_STORAGE_COMPOSE_ARGS=(--env-file "$root/.env" --project-directory "$root"
    --project-name "$SELFHOST_STORAGE_PROJECT_NAME" --profile services)
  for file in docker-compose.yml docker-compose.production.yml docker-compose.selfhost.yml; do
    SELFHOST_STORAGE_COMPOSE_ARGS+=(-f "$root/$file")
  done
  if [[ "$SELFHOST_STORAGE_MODE_RESOLVED" == byo-s3 ]]; then
    SELFHOST_STORAGE_COMPOSE_ARGS+=(-f "$root/docker-compose.byo-s3.yml")
  fi
  if ! model="$(selfhost_storage_compose config --format json 2>/dev/null)" \
    || ! rendered_name="$(printf '%s' "$model" | _selfhost_storage_data identity)" \
    || [[ "$rendered_name" != "$SELFHOST_STORAGE_PROJECT_NAME" ]]; then
    _selfhost_storage_clear_resolution
    _selfhost_storage_refuse 'selfhost-storage: model resolution refused'
    return 1
  fi
}

selfhost_storage_select_image() {
  SELFHOST_STORAGE_TRUSTED_IMAGE=''
  export -n SELFHOST_STORAGE_TRUSTED_IMAGE
  if [[ $# -ne 1 || "$1" == *[[:space:]]* ]] \
    || [[ -n "$1" && ! "$1" =~ ^(sha256:[[:xdigit:]]{64}|.+@sha256:[[:xdigit:]]{64})$ ]]; then
    _selfhost_storage_refuse 'selfhost-storage: checkpoint image refused'
    return 1
  fi
  SELFHOST_STORAGE_TRUSTED_IMAGE="$1"
}

_selfhost_storage_compose_diagnostic() {
  python3 -c '
import os, sys

patterns = (b"cannot connect to the docker daemon",
            b"permission denied while trying to connect to the docker daemon socket",
            b"permission denied while trying to connect to the docker api")
overlap = max(map(len, patterns)) - 1
tail = b""
daemon = False
try:
    while True:
        chunk = os.read(0, 4096)
        if not chunk:
            break
        if daemon:
            continue
        data = tail + chunk.lower()
        daemon = any(pattern in data for pattern in patterns)
        tail = b"" if daemon else data[-overlap:]
    print("daemon" if daemon else "other")
except OSError:
    sys.exit(1)
' 2>/dev/null
}

selfhost_storage_compose() {
  if [[ -z "$SELFHOST_STORAGE_PROJECT_ROOT" || -z "$SELFHOST_STORAGE_PROJECT_NAME" \
    || ${#SELFHOST_STORAGE_COMPOSE_ARGS[@]} -eq 0 ]]; then
    _selfhost_storage_refuse 'selfhost-storage: model is unresolved'
    return 1
  fi
  (
    selfhost_storage_scrub_environment || exit 1
    cd -- "$SELFHOST_STORAGE_PROJECT_ROOT" || exit 1
    if [[ -n "$SELFHOST_STORAGE_TRUSTED_IMAGE" ]]; then
      CONTROL_PLANE_IMAGE="$SELFHOST_STORAGE_TRUSTED_IMAGE"
      export CONTROL_PLANE_IMAGE
    fi
    local rc=0 diagnostic stdout_fd
    exec {stdout_fd}>&1
    diagnostic="$(
      docker compose "${SELFHOST_STORAGE_COMPOSE_ARGS[@]}" "$@" 2>&1 1>&"$stdout_fd" \
        | _selfhost_storage_compose_diagnostic
      exit "${PIPESTATUS[0]}"
    )" || rc=$?
    exec {stdout_fd}>&-
    if [[ "$rc" -ne 0 ]]; then
      if [[ "$diagnostic" == daemon ]]; then
        printf '%s\n' 'selfhost-storage: Docker daemon unavailable or inaccessible; check Docker service and socket permissions' >&2
      else
        printf '%s\n' 'selfhost-storage: Compose command failed' >&2
      fi
    fi
    exit "$rc"
  )
}

# Discovery/inspection output is private. Ordinary Docker lookup preserves the
# fail-closed test stub; never select an absolute binary or bypass PATH.
_selfhost_storage_docker() (
  selfhost_storage_scrub_environment || exit 1
  docker "$@" 2>/dev/null
)

_selfhost_storage_inspect_container() {
  local metadata
  metadata="$(_selfhost_storage_docker inspect --type container --format \
    '{{json .Config.Labels}}|{{json .State}}' "$1")" || return 1
  printf '%s' "$metadata" | _selfhost_storage_data "$2" "${@:3}" || return 1
}

selfhost_storage_assert_fresh() {
  _selfhost_storage_clear_resolution
  local root phase artifact ids volumes id verdict rc=0 filter
  if [[ $# -ne 2 || ( "$2" != early && "$2" != final ) ]] \
    || ! root="$(_selfhost_storage_root "$1")"; then
    _selfhost_storage_refuse 'selfhost-storage: fresh installation check refused'
    return 1
  fi
  phase="$2"
  # Both root artifacts and the controller existing deploy-local artifacts count.
  for artifact in "$root/.env" \
    "$root/.env.activity-history-image.local" "$root/.env.activity-history-receipt.local" \
    "$root/[internal].env.activity-history-image.local" \
    "$root/[internal].env.activity-history-receipt.local"; do
    if [[ -e "$artifact" || -L "$artifact" ]]; then
      _selfhost_storage_refuse 'selfhost-storage: existing installation refused'
      return 1
    fi
  done
  _selfhost_storage_docker info >/dev/null || rc=$?
  if [[ "$rc" -ne 0 ]]; then
    [[ "$phase" == early && "$rc" -eq 127 ]] && return 0
    _selfhost_storage_refuse 'selfhost-storage: installation discovery unavailable'
    return 1
  fi
  if ! selfhost_storage_project_identity "$root"; then
    _selfhost_storage_clear_resolution
    return 1
  fi
  for filter in "label=com.docker.compose.project=$SELFHOST_STORAGE_PROJECT_NAME" \
    "label=com.docker.compose.project.working_dir=$root"; do
    if ! ids="$(_selfhost_storage_docker ps --all --quiet --no-trunc --filter "$filter")" \
      || ! ids="$(printf '%s' "$ids" | _selfhost_storage_data ids)"; then
      _selfhost_storage_clear_resolution
      _selfhost_storage_refuse 'selfhost-storage: installation discovery unavailable'
      return 1
    fi
    if [[ -n "$ids" ]]; then
      while IFS= read -r id; do
        if ! verdict="$(_selfhost_storage_inspect_container "$id" fresh-container \
          "$SELFHOST_STORAGE_PROJECT_NAME" "$root")"; then
          _selfhost_storage_clear_resolution
          _selfhost_storage_refuse 'selfhost-storage: installation discovery unavailable'
          return 1
        fi
        if [[ "$verdict" != other ]]; then
          _selfhost_storage_clear_resolution
          _selfhost_storage_refuse 'selfhost-storage: existing installation refused'
          return 1
        fi
      done <<<"$ids"
    fi
  done
  if ! volumes="$(_selfhost_storage_docker volume ls --quiet --filter \
    "label=com.docker.compose.project=$SELFHOST_STORAGE_PROJECT_NAME")" \
    || ! volumes="$(printf '%s' "$volumes" | _selfhost_storage_data volumes)"; then
    _selfhost_storage_clear_resolution
    _selfhost_storage_refuse 'selfhost-storage: installation discovery unavailable'
    return 1
  fi
  if [[ -n "$volumes" ]]; then
    while IFS= read -r id; do
      if ! verdict="$(_selfhost_storage_docker volume inspect --format '{{json .Labels}}' "$id")" \
        || ! verdict="$(printf '%s' "$verdict" | _selfhost_storage_data volume "$SELFHOST_STORAGE_PROJECT_NAME")"; then
        _selfhost_storage_clear_resolution
        _selfhost_storage_refuse 'selfhost-storage: installation discovery unavailable'
        return 1
      fi
      if [[ "$verdict" == match ]]; then
        _selfhost_storage_clear_resolution
        _selfhost_storage_refuse 'selfhost-storage: existing installation refused'
        return 1
      fi
    done <<<"$volumes"
  fi
  # Versioned release roots have different default Compose projects. Retained
  # volumes and stopped/renamed containers must not authorize a fresh install.
  # Reserve the public release project namespace and the fixed Concord names.
  # Persistent logical volume keys do not prove application ownership; refuse
  # that ambiguous retained data under any typed Compose project to preserve it.
  if ! ids="$(_selfhost_storage_docker ps --all --quiet --no-trunc)" \
    || ! ids="$(printf '%s' "$ids" | _selfhost_storage_data ids)"; then
    _selfhost_storage_clear_resolution
    _selfhost_storage_refuse 'selfhost-storage: installation discovery unavailable'
    return 1
  fi
  if [[ -n "$ids" ]]; then
    while IFS= read -r id; do
      if ! verdict="$(_selfhost_storage_docker inspect --type container --format \
        '{{json .Name}}|{{json .Config.Labels}}|{{json .State}}' "$id")" \
        || ! verdict="$(printf '%s' "$verdict" | _selfhost_storage_data fresh-host-container)"; then
        _selfhost_storage_clear_resolution
        _selfhost_storage_refuse 'selfhost-storage: installation discovery unavailable'
        return 1
      fi
      if [[ "$verdict" == match ]]; then
        _selfhost_storage_clear_resolution
        _selfhost_storage_refuse 'selfhost-storage: existing installation refused'
        return 1
      fi
    done <<<"$ids"
  fi
  if ! volumes="$(_selfhost_storage_docker volume ls --quiet --filter label=com.docker.compose.project)" \
    || ! volumes="$(printf '%s' "$volumes" | _selfhost_storage_data volumes)"; then
    _selfhost_storage_clear_resolution
    _selfhost_storage_refuse 'selfhost-storage: installation discovery unavailable'
    return 1
  fi
  if [[ -n "$volumes" ]]; then
    while IFS= read -r id; do
      if ! verdict="$(_selfhost_storage_docker volume inspect --format '{{json .Labels}}' "$id")" \
        || ! verdict="$(printf '%s' "$verdict" | _selfhost_storage_data fresh-host-volume)"; then
        _selfhost_storage_clear_resolution
        _selfhost_storage_refuse 'selfhost-storage: installation discovery unavailable'
        return 1
      fi
      if [[ "$verdict" == match ]]; then
        _selfhost_storage_clear_resolution
        _selfhost_storage_refuse 'selfhost-storage: existing installation refused'
        return 1
      fi
    done <<<"$volumes"
  fi
  _selfhost_storage_clear_resolution
}

selfhost_storage_find_minio() {
  SELFHOST_STORAGE_MINIO_IDS=()
  export -n SELFHOST_STORAGE_MINIO_IDS
  local ids id verdict
  local -a found=()
  if [[ -z "$SELFHOST_STORAGE_PROJECT_NAME" || -z "$SELFHOST_STORAGE_PROJECT_ROOT" ]] \
    || ! ids="$(_selfhost_storage_docker ps --all --quiet --no-trunc \
      --filter "label=com.docker.compose.project=$SELFHOST_STORAGE_PROJECT_NAME" \
      --filter 'label=com.docker.compose.service=minio')" \
    || ! ids="$(printf '%s' "$ids" | _selfhost_storage_data ids)"; then
    _selfhost_storage_refuse 'selfhost-storage: MinIO discovery unavailable'
    return 1
  fi
  if [[ -n "$ids" ]]; then
    while IFS= read -r id; do
      if ! verdict="$(_selfhost_storage_inspect_container "$id" minio-container "$SELFHOST_STORAGE_PROJECT_NAME")"; then
        _selfhost_storage_refuse 'selfhost-storage: MinIO discovery unavailable'
        return 1
      fi
      [[ "$verdict" != running ]] || found+=("$id")
    done <<<"$ids"
  fi
  SELFHOST_STORAGE_MINIO_IDS=("${found[@]}")
}

selfhost_storage_assert_no_running_minio() {
  SELFHOST_STORAGE_MINIO_IDS=()
  export -n SELFHOST_STORAGE_MINIO_IDS
  [[ "$SELFHOST_STORAGE_MODE_RESOLVED" != bundled ]] || return 0
  if [[ "$SELFHOST_STORAGE_MODE_RESOLVED" != byo-s3 ]] || ! selfhost_storage_find_minio; then
    SELFHOST_STORAGE_MINIO_IDS=()
    _selfhost_storage_refuse 'selfhost-storage: MinIO state unavailable'
    return 1
  fi
  if [[ ${#SELFHOST_STORAGE_MINIO_IDS[@]} -ne 0 ]]; then
    SELFHOST_STORAGE_MINIO_IDS=()
    _selfhost_storage_refuse 'selfhost-storage: running bundled MinIO refused'
    return 1
  fi
}

selfhost_storage_stop_stale_minio() {
  SELFHOST_STORAGE_MINIO_IDS=()
  export -n SELFHOST_STORAGE_MINIO_IDS
  [[ "$SELFHOST_STORAGE_MODE_RESOLVED" != bundled ]] || return 0
  if [[ "$SELFHOST_STORAGE_MODE_RESOLVED" != byo-s3 ]] || ! selfhost_storage_find_minio; then
    SELFHOST_STORAGE_MINIO_IDS=()
    _selfhost_storage_refuse 'selfhost-storage: MinIO recovery refused'
    return 1
  fi
  [[ ${#SELFHOST_STORAGE_MINIO_IDS[@]} -ne 0 ]] || return 0
  local id verdict
  local -a stopped=("${SELFHOST_STORAGE_MINIO_IDS[@]}")
  SELFHOST_STORAGE_MINIO_IDS=()
  if ! _selfhost_storage_docker stop "${stopped[@]}" >/dev/null; then
    _selfhost_storage_refuse 'selfhost-storage: MinIO recovery refused'
    return 1
  fi
  for id in "${stopped[@]}"; do
    if ! verdict="$(_selfhost_storage_inspect_container "$id" minio-container "$SELFHOST_STORAGE_PROJECT_NAME")" \
      || [[ "$verdict" != stopped ]]; then
      _selfhost_storage_refuse 'selfhost-storage: MinIO recovery verification refused'
      return 1
    fi
  done
  if ! selfhost_storage_find_minio || [[ ${#SELFHOST_STORAGE_MINIO_IDS[@]} -ne 0 ]]; then
    SELFHOST_STORAGE_MINIO_IDS=()
    _selfhost_storage_refuse 'selfhost-storage: MinIO recovery verification refused'
    return 1
  fi
}
