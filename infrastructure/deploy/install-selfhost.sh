#!/usr/bin/env bash
# Concord Voice — Basic self-hosted installer (#1616).
# Build-from-source. Run from a repo clone on a Debian-12 / Ubuntu-24.04 host.
#
# Usage:
#   ./infrastructure/deploy/install-selfhost.sh            # interactive wizard
#   ./infrastructure/deploy/install-selfhost.sh --env-file answers.env   # non-interactive
#   ./infrastructure/deploy/install-selfhost.sh --help
#
# Exit codes: 0 ok | 1 runtime | 2 preflight failure
set -euo pipefail

# ── Constants ──────────────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
readonly PROJECT_ROOT
: "${NGINX_SRC:=$SCRIPT_DIR/nginx/concordvoice.conf}"    # overridable for tests
: "${ENV_OUT:=$PROJECT_ROOT/.env}"                       # overridable for tests
: "${NGINX_OUT:=$SCRIPT_DIR/nginx/concordvoice.rendered.conf}"  # overridable for tests

# shellcheck source=[internal]selfhost-storage.sh
source "$SCRIPT_DIR/selfhost-storage.sh" || {
  printf '%s\n' 'installer: storage helper unavailable' >&2
  return 1 2>/dev/null || exit 1
}
readonly INSTALLER_ANSWER_KEYS=(
  CFG_DOMAIN CFG_TLS_MODE CFG_TURN_REALM CFG_PUBLIC_IP
  CFG_SMTP_HOST CFG_SMTP_PORT CFG_SMTP_USER CFG_SMTP_PASS CFG_SMTP_FROM
  CFG_TRUSTED_CIDRS CFG_ACTIVITY_HISTORY_OPERATOR_NAME CFG_ACTIVITY_HISTORY_PRIVACY_POLICY_URL
  CFG_FEEDBACK_ENABLED CFG_FEEDBACK_REPO CFG_FEEDBACK_PAT
  CFG_STORAGE_MODE CFG_STORAGE_BACKEND CFG_STORAGE_ENDPOINT CFG_STORAGE_REGION
  CFG_STORAGE_ACCESS_KEY CFG_STORAGE_SECRET_KEY CFG_STORAGE_USE_SSL CFG_STORAGE_BUCKET
)

readonly RED='\033[0;31m'
readonly GREEN='\033[0;32m'
readonly YELLOW='\033[1;33m'
readonly BLUE='\033[0;34m'
readonly NC='\033[0m'

# ── Usage ──────────────────────────────────────────────────────────────────────

usage() {
  cat <<'USAGE'
Concord Voice self-hosted installer

Usage: install-selfhost.sh [--env-file <path>] [--help]
  (no args)        interactive install wizard
  --env-file PATH  non-interactive install from an answers file
  --help           show this help

Exit codes: 0 ok | 1 runtime | 2 preflight failure
USAGE
}

# ── Preflight checks ────────────────────────────────────────────────────────────

# port_in_use PORT — returns 0 if a TCP listener is bound on the port.
# Prefer ss (iproute2), fall back to lsof (macOS / older Linux).
port_in_use() {
  local p="$1"
  if command -v ss >/dev/null 2>&1; then
    ss -ltn 2>/dev/null | grep -qE "[:.]${p}[[:space:]]"
  else
    lsof -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1
  fi
}

# Only HOST-BOUND ports (#3538): Postgres/Redis/NATS/MinIO have no host socket on
# self-host, so a host-run Postgres or Redis must not block the install.
readonly REQUIRED_TCP_PORTS=(8080 3000 3478 5349)
# Inclusive UDP host publications of base + production + selfhost, with or
# without BYO storage. Kept in parity with both rendered Compose contracts;
# installer answers do not offer a different RTC or TURN publication window.
readonly REQUIRED_UDP_RANGES=(3478:3478 40000:41999 49152:49252)

preflight_udp_ports() {
  local inventory verdict
  # All UDP sockets includes connected sockets as well as unconnected binds,
  # on both address families. One numeric inventory covers every range port.
  if ! command -v ss >/dev/null 2>&1 ||
     ! inventory="$(LC_ALL=C ss -H -a -u -n 2>/dev/null)"; then
    printf '%s\n' 'installer: UDP socket inventory unavailable or unparseable' >&2
    return 2
  fi
  # Host socket text stays on stdin, never in exported variables or diagnostics.
  # Refuse the complete inventory if any row cannot be judged, even when other
  # rows are known unrelated or already reveal a required-port conflict.
  if ! verdict="$(printf '%s' "$inventory" | python3 -c '
import ipaddress, re, sys

def endpoint(value, local=False):
    host, separator, port = value.rpartition(":")
    if not separator or not host:
        raise ValueError()
    # Native ss places IPv6 zones after the bracket; older forms put them inside.
    if host.startswith("[") and "]%" in host:
        host, scope = host.rsplit("%", 1)
        if "%" in host or not re.fullmatch(r"[A-Za-z0-9_.-]+", scope):
            raise ValueError()
    if host.startswith("["):
        if not host.endswith("]"):
            raise ValueError()
        host = host[1:-1]
    elif "[" in host or "]" in host:
        raise ValueError()
    if "%" in host:
        host, scope = host.rsplit("%", 1)
        if not re.fullmatch(r"[A-Za-z0-9_.-]+", scope):
            raise ValueError()
    if host != "*":
        ipaddress.ip_address(host)
    if port == "*" and not local:
        return None
    if not re.fullmatch(r"[0-9]{1,5}", port) or int(port) > 65535:
        raise ValueError()
    return int(port)

try:
    ranges = [tuple(int(piece) for piece in value.split(":")) for value in sys.argv[1:]]
    if not ranges or any(len(value) != 2 or not 0 < value[0] <= value[1] <= 65535 for value in ranges):
        raise ValueError()
    text = sys.stdin.read(4 * 1024 * 1024 + 1)
    if len(text) > 4 * 1024 * 1024:
        raise ValueError()
    busy = False
    for line in text.splitlines():
        if not line.strip():
            continue
        fields = line.split()
        if len(fields) != 5:
            raise ValueError()
        state, recvq, sendq, local, peer = fields
        if state not in ("UNCONN", "ESTAB") or not re.fullmatch(r"[0-9]+", recvq) or not re.fullmatch(r"[0-9]+", sendq):
            raise ValueError()
        port = endpoint(local, local=True)
        endpoint(peer)
        busy = busy or any(low <= port <= high for low, high in ranges)
except Exception:
    sys.exit(2)
print("busy" if busy else "free")
' "${REQUIRED_UDP_RANGES[@]}" 2>/dev/null)"; then
    printf '%s\n' 'installer: UDP socket inventory unavailable or unparseable' >&2
    return 2
  fi
  case "$verdict" in
    free) return 0 ;;
    busy) printf '%s\n' 'installer: required UDP ports already in use' >&2; return 2 ;;
    *) printf '%s\n' 'installer: UDP socket inventory unavailable or unparseable' >&2; return 2 ;;
  esac
}

preflight_ports() {
  local busy=() p
  for p in "${REQUIRED_TCP_PORTS[@]}"; do
    port_in_use "$p" && busy+=("$p") || true
  done
  if (( ${#busy[@]} > 0 )); then
    echo -e "${RED}✗ preflight: ports already in use: ${busy[*]}${NC}" >&2
    return 2
  fi
  preflight_udp_ports || return $?
  echo -e "${GREEN}✓ ports free${NC}"
}

preflight_os() {
  local id ver
  # shellcheck source=/dev/null
  id="$(. /etc/os-release 2>/dev/null && echo "${ID:-}")" || true
  # shellcheck source=/dev/null
  ver="$(. /etc/os-release 2>/dev/null && echo "${VERSION_ID:-}")" || true
  case "$id:$ver" in
    debian:12|ubuntu:24.04)
      echo -e "${GREEN}✓ OS $id $ver${NC}"
      ;;
    *)
      echo -e "${YELLOW}⚠ unsupported/untested OS '$id $ver' (supported: Debian 12, Ubuntu 24.04)${NC}" >&2
      return 2
      ;;
  esac
}

# Version floors (#3538). Engine: before 28.0.0, LAN neighbours can reach ports
# published on 127.0.0.1, and from 28.2.0 to 28.3.2 a firewalld reload drops the
# rule that blocks them (CVE-2025-54388). Compose: 2.24.5 is the oldest release
# measured honouring `!override` in docker-compose.selfhost.yml (2.20 and 2.21
# ignore it and merge the lists back to 0.0.0.0). MIN_ENGINE must equal
# SELFHOST_MIN_ENGINE in concord-ctl.sh (pinned by test-selfhost-port-posture.sh).
readonly MIN_ENGINE=28.3.3
readonly MIN_COMPOSE=2.24.5

# version_at_least HAVE NEED — dotted-version comparison via sort -V.
version_at_least() { [[ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -1)" == "$2" ]]; }

preflight_docker() {
  local engine compose
  command -v docker >/dev/null 2>&1 || {
    echo -e "${RED}✗ Docker Engine not found${NC}" >&2
    return 2
  }
  docker compose version >/dev/null 2>&1 || {
    echo -e "${RED}✗ Docker Compose v2 plugin not found${NC}" >&2
    return 2
  }
  engine="$(docker version --format '{{.Server.Version}}' 2>/dev/null | grep -oE '^[0-9]+\.[0-9]+\.[0-9]+' || true)"
  if [[ -z "$engine" ]] || ! version_at_least "$engine" "$MIN_ENGINE"; then
    echo -e "${RED}✗ Docker Engine ${engine:-unknown (is the daemon running and accessible?)} — need >= $MIN_ENGINE (older engines can let LAN neighbours reach loopback-published ports, CVE-2025-54388). Install Docker CE from download.docker.com.${NC}" >&2
    return 2
  fi
  compose="$(docker compose version --short 2>/dev/null | tr -d 'v' | grep -oE '^[0-9]+\.[0-9]+\.[0-9]+' || true)"
  if [[ -z "$compose" ]] || ! version_at_least "$compose" "$MIN_COMPOSE"; then
    echo -e "${RED}✗ Docker Compose ${compose:-unknown} — need >= $MIN_COMPOSE (older Compose ignores !override in docker-compose.selfhost.yml)${NC}" >&2
    return 2
  fi
  command -v python3 >/dev/null 2>&1 || {
    echo -e "${RED}✗ python3 not found (the deploy controller needs it)${NC}" >&2
    return 2
  }
  echo -e "${GREEN}✓ docker ${engine} + compose ${compose} + python3${NC}"
}

# Thresholds: 2 vCPU, 2GiB RAM, 10GiB free disk (conservative BASIC floor).
preflight_resources() {
  local cpus mem_kb disk_kb fail=0
  cpus="$(nproc 2>/dev/null || echo 1)"
  mem_kb="$(awk '/MemTotal/{print $2}' /proc/meminfo 2>/dev/null || echo 0)"
  disk_kb="$(df -Pk "$PROJECT_ROOT" 2>/dev/null | awk 'NR==2{print $4}' || echo 0)"
  (( cpus >= 2 ))          || { echo -e "${YELLOW}⚠ <2 vCPU ($cpus)${NC}" >&2; fail=1; }
  (( mem_kb >= 2000000 ))  || { echo -e "${YELLOW}⚠ <2GiB RAM${NC}" >&2; fail=1; }
  (( disk_kb >= 10000000 )) || { echo -e "${RED}✗ <10GiB free disk${NC}" >&2; fail=1; }
  (( fail == 0 )) && echo -e "${GREEN}✓ resources ok${NC}" || return 2
}

# preflight_logrotate — the security-event log boundary's rotation timer runs
# /usr/sbin/logrotate, and minimal images omit it. Without it the timer fails
# silently and events are dropped once a stream fills (#3561). Checked by path,
# not PATH: /usr/sbin is often absent from an unprivileged PATH.
preflight_logrotate() {
  local bin="${LOGROTATE_BIN_OVERRIDE:-/usr/sbin/logrotate}"
  [[ -x "$bin" ]] || {
    echo -e "${RED}✗ logrotate not found at $bin — sudo apt-get install logrotate${NC}" >&2
    return 2
  }
  echo -e "${GREEN}✓ logrotate${NC}"
}

run_preflight() {
  echo -e "${BLUE}Running preflight (no files written yet)...${NC}"
  if ! { preflight_os && preflight_docker && preflight_resources && preflight_logrotate && preflight_ports; }; then
    echo -e "${RED}Preflight failed — aborting before any change.${NC}" >&2
    exit 2
  fi
}

# ── Secret generation ──────────────────────────────────────────────────────────

# gen_secret — CSPRNG, 32 random bytes → 64 safe hexadecimal characters (256 bits).
gen_secret()  { openssl rand -hex 32; }
gen_hex_key() { openssl rand -hex 32; }                                # 32-byte hex (MFA key shape)

# ── Validators ─────────────────────────────────────────────────────────────────

# validate_domain DOMAIN — lowercase per-label FQDN; reject empty, leading/trailing/
# consecutive dots, leading/trailing hyphens, and media./api./turn.-prefixed apex.
# Labels fit 63 bytes; media.<apex>, the longest generated name, fits 253.
# Browser origins use lowercase hosts and the server compares origins exactly.
validate_domain() {
  local d="${1:-}"
  [[ -n "$d" ]] || { echo -e "${RED}domain required${NC}" >&2; return 1; }
  [[ ${#d} -le 247 && "$d" =~ ^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$ ]] || {
    printf '%s\n' 'installer: invalid domain' >&2
    return 1
  }
  case "$d" in
    media.*|api.*|turn.*)
      echo -e "${RED}use the apex (e.g. example.com), not a media./api./turn. host${NC}" >&2
      return 1
      ;;
  esac
  return 0
}

# validate_media_subdomain URL — assert the derived MEDIA_PLANE_URL is a media.<apex>
# subdomain, not the root zone (mirrors config.go #725 guard).
validate_media_subdomain() {
  local url="${1:-}" host
  host="${url#https://}"
  host="${host%%/*}"
  [[ "$host" == media.*.* ]] || {
    printf '%s\n' 'installer: media URL must be a media subdomain' >&2
    return 1
  }
  return 0
}

# validate_cidrs LIST — mirror the consumer comma split, Go whitespace trim and
# net.ParseCIDR address/prefix grammar; REJECT all-traffic CIDRs
# (0.0.0.0/0, ::/0 and equivalent spellings) which would trust every source —
# a spoofable X-Forwarded-For / rate-limit-bypass fail-open (CWE-348). Empty is OK
# (the writer applies the private-range default).
validate_cidrs() {
  local cidrs="${1:-}" cidr_status=0
  [[ -z "$cidrs" ]] && return 0
  python3 -c '
import ipaddress, re, sys
# Python strip() also removes U+001C..U+001F; Go strings.TrimSpace does not.
go_space = "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"
try:
    parsed = False
    effective_v4, native_v6 = [], []
    for raw in sys.argv[1].split(","):
        cidr = raw.strip(go_space)
        if not cidr:
            continue
        if cidr.count("/") != 1:
            raise ValueError()
        address, prefix = cidr.split("/")
        if "%" in address or re.fullmatch(r"[0-9]+", prefix) is None:
            raise ValueError()
        # net.ParseCIDR accepts leading-zero decimal masks, unlike ParsePrefix.
        # Bound the significant digits before int() to preserve arbitrary zeros.
        significant = prefix.lstrip("0") or "0"
        if len(significant) > 3:
            raise ValueError()
        network = ipaddress.ip_network((address, int(significant)), strict=False)
        # Go IPNet.Contains normalizes mapped networks and masks to IPv4:
        # mapped /96 is effective IPv4 /0; /97 and longer remain restricted.
        mapped_all = (network.version == 6 and network.prefixlen == 96
                      and network.network_address.ipv4_mapped is not None)
        if network.prefixlen == 0 or mapped_all:
            sys.exit(2)
        if network.version == 4:
            effective_v4.append(network)
        elif network.prefixlen >= 96 and network.network_address.ipv4_mapped is not None:
            effective_v4.append(ipaddress.IPv4Network(
                (int(network.network_address.ipv4_mapped), network.prefixlen - 96)))
        else:
            native_v6.append(network)
        parsed = True
    if not parsed:
        raise ValueError()
    # Match the consumer-effective families, including mixed native/mapped
    # IPv4 unions. Native IPv6 peers exclude the mapped IPv4 /96 namespace;
    # filling that hole here tests completeness without trusting those IPv4 peers.
    if any(n.prefixlen == 0 for n in ipaddress.collapse_addresses(effective_v4)):
        sys.exit(2)
    native_v6.append(ipaddress.IPv6Network("::ffff:0:0/96"))
    if any(n.prefixlen == 0 for n in ipaddress.collapse_addresses(native_v6)):
        sys.exit(2)
except ValueError:
    sys.exit(1)
' "$cidrs" 2>/dev/null || cidr_status=$?
  case "$cidr_status" in
    0) return 0 ;;
    2)
      echo -e "${RED}TRUSTED_PROXY_CIDRS must not include 0.0.0.0/0 or ::/0 (would trust every source — X-Forwarded-For spoofing / rate-limit bypass)${NC}" >&2
      return 1
      ;;
    *) printf '%s\n' 'installer: invalid trusted proxy CIDR' >&2; return 1 ;;
  esac
}

# SMTP_HOST is a bare Go-compatible DNS/LAN name or zoneless IP literal,
# not a URL, authority with port, or display address. Preserve admitted bytes.
validate_smtp_host() {
  local host="${1:-}"
  if [[ -z "$host" ]]; then
    printf '%s\n' 'installer: SMTP host is required' >&2
    return 1
  fi
  if python3 - "$host" 2>/dev/null <<'PY'
import ipaddress, re, sys
host = sys.argv[1]
try:
    if "%" in host:
        raise ValueError()
    try:
        address = ipaddress.ip_address(host)
    except ValueError:
        # Go domain grammar permits underscores and a terminal root dot, but
        # requires a nonnumeric name and labels no longer than 63 bytes.
        if (not host.isascii() or len(host) > 254
                or (len(host) == 254 and not host.endswith("."))):
            raise ValueError()
        name = host[:-1] if host.endswith(".") else host
        if not re.search(r"[A-Za-z_-]", name):
            raise ValueError()
        for label in name.split("."):
            if (not 1 <= len(label) <= 63
                    or re.fullmatch(r"[A-Za-z0-9_-]+", label) is None
                    or label.startswith("-") or label.endswith("-")):
                raise ValueError()
    else:
        if isinstance(address, ipaddress.IPv6Address) and address.ipv4_mapped is not None:
            address = address.ipv4_mapped
        if (address.is_unspecified or address.is_multicast
                or address == ipaddress.IPv4Address("255.255.255.255")):
            raise ValueError()
except ValueError:
    sys.exit(1)
PY
  then
    return 0
  fi
  printf '%s\n' 'installer: invalid SMTP host' >&2
  return 1
}

# validate_smtp_port PORT — numeric 1-65535 (empty OK, writer defaults to 587).
validate_smtp_port() {
  local p="${1:-}"
  [[ -z "$p" ]] && return 0
  { [[ "$p" =~ ^[0-9]{1,5}$ ]] && (( 10#$p >= 1 && 10#$p <= 65535 )); } || {
    printf '%s\n' 'installer: invalid SMTP port' >&2
    return 1
  }
  return 0
}

# validate_https_privacy_url URL — lowercase absolute HTTPS with a real host,
# no userinfo, and an optional valid TCP port. Mirrors the managed workflow.
validate_https_privacy_url() {
  local url="${1:-}" authority host port
  [[ -n "$url" && "$url" == https://* && "$url" != *[[:space:]]* ]] || return 1
  authority="${url#https://}"
  authority="${authority%%[/?#]*}"
  [[ -n "$authority" && "$authority" != *@* && "$authority" != *\\* ]] || return 1
  # ponytail: reject bracketed literal hosts; add an IP parser if raw IPv6
  # privacy-policy URLs become a real operator requirement.
  [[ "$authority" != \[* && "$authority" != *:*:* ]] || return 1
  host="${authority%%:*}"
  [[ "$host" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ \
     && "$host" != *..* && "$host" != *.-* && "$host" != *-.* ]] || return 1
  if [[ "$authority" == *:* ]]; then
    port="${authority#*:}"
    [[ "$port" =~ ^[0-9]{1,5}$ ]] || return 1
    (( 10#$port >= 1 && 10#$port <= 65535 )) || return 1
  fi
}

# validate_activity_history_disclosure OPERATOR URL — both values are optional,
# so an unconfigured self-host remains visibly unavailable. A non-empty privacy
# URL must be absolute HTTPS. Values are single-line because they cross a dotenv
# boundary; arbitrary printable punctuation in the operator name is preserved.
validate_activity_history_disclosure() {
  local operator="${1:-}" url="${2:-}"
  if [[ "$operator" == *$'\n'* || "$operator" == *$'\r'* \
     || "$url" == *$'\n'* || "$url" == *$'\r'* ]]; then
    echo -e "${RED}Activity History disclosure values must be single-line${NC}" >&2
    return 1
  fi
  [[ -z "$url" ]] && return 0
  validate_https_privacy_url "$url" || {
    echo -e "${RED}Activity History privacy URL must be absolute HTTPS with a valid hostname${NC}" >&2
    return 1
  }
}

# validate_tls_mode MODE — one of letsencrypt|local|import (TLS provisioning
# mode handed to provision-cert.sh; #1617). A CLI/wizard choice, never an .env
# var (DR-1: no config.go consumer, no 5-surface discipline).
validate_tls_mode() {
  case "${1:-}" in
    letsencrypt|local|import) return 0 ;;
    *) printf '%s\n' 'installer: invalid TLS mode (letsencrypt|local|import)' >&2; return 1 ;;
  esac
}

# warn_public_ip IP — non-fatal advisory for an empty/private/NAT PUBLIC_IP.
warn_public_ip() {
  local ip="${1:-}"
  if [[ -z "$ip" ]]; then
    echo -e "${YELLOW}⚠ PUBLIC_IP is empty — TURN relay needs a routable public IP${NC}" >&2
  elif [[ "$ip" =~ ^(10\.|192\.168\.|172\.(1[6-9]|2[0-9]|3[01])\.|127\.) ]]; then
    echo -e "${YELLOW}⚠ PUBLIC_IP looks private/NAT — TURN relay will not function without a routable public IP${NC}" >&2
  fi
}

# validate_public_ip IP — required host IPv4 address, including explicit LAN IPv4.
# The basic runtime's TURN bridge/listener has no supported IPv6 relay path.
# The value is written to .env and printed inside
# a root command the operator pastes, and its default comes from an external echo
# service, so a shell metacharacter must never get through (#3561). The rejected
# value is not echoed: it is untrusted terminal input.
# Preserve the restricted spelling (no brackets, scope IDs or metacharacters),
# then parse the complete address before any durable environment publication.
validate_public_ip() {
  local ip="${1:-}"
  if [[ -n "$ip" && "$ip" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] &&
    python3 - "$ip" 2>/dev/null <<'PY'
import ipaddress, sys
try:
    address = ipaddress.IPv4Address(sys.argv[1])
    if address.is_unspecified or address.is_multicast or int(address) == 0xffffffff:
        sys.exit(1)
except ValueError:
    sys.exit(1)
PY
  then
    return 0
  fi
  echo -e "${RED}invalid public IP: expected an IPv4 address for basic self-host${NC}" >&2
  return 1
}

# validate_collected_config — single validation gate for BOTH the interactive and
# --env-file paths, run before any mutation. Field shape checks that the wizard's
# per-prompt validation would catch but the --env-file path otherwise would not.
validate_collected_config() {
  local key text_status=0
  local -a values=()
  : "${CFG_FEEDBACK_ENABLED=no}"
  for key in "${INSTALLER_ANSWER_KEYS[@]}"; do values+=("${!key-}"); done
  # All text is checked before it can enter diagnostics or the metadata review.
  printf '%s\0' "${values[@]}" | python3 -c '
import re, sys, unicodedata
# Classification matches Go strings.TrimSpace without altering published bytes.
go_space = "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"
try:
    raw = sys.stdin.buffer.read().split(b"\x00")
    if raw.pop() != b"" or len(raw) != len(sys.argv) - 1:
        raise ValueError()
    feedback_mode = feedback_repo = feedback_pat = smtp_from = ""
    for key, value in zip(sys.argv[1:], raw):
        text = value.decode("utf-8")
        if "\r" in text or "\n" in text:
            raise ValueError()
        if key not in ("CFG_SMTP_PASS", "CFG_FEEDBACK_PAT", "CFG_STORAGE_ACCESS_KEY", "CFG_STORAGE_SECRET_KEY"):
            if any(unicodedata.category(c) == "Cc" for c in text):
                raise ValueError()
        if key == "CFG_FEEDBACK_ENABLED":
            feedback_mode = text
        elif key == "CFG_FEEDBACK_REPO":
            feedback_repo = text
        elif key == "CFG_FEEDBACK_PAT":
            feedback_pat = text
        elif key == "CFG_SMTP_FROM":
            smtp_from = text
    if feedback_mode not in ("yes", "no"):
        sys.exit(2)
    if feedback_mode == "yes" and feedback_repo.strip().casefold() == "selfhost/disabled":
        sys.exit(3)
    if feedback_mode == "yes":
        repo = feedback_repo.strip(go_space)
        if not repo or not feedback_pat.strip(go_space):
            sys.exit(4)
        # Preserve Unicode segments, refusing URL syntax and path-normalizing dots.
        parts = repo.split("/")
        if (len(parts) != 2 or any(c in repo for c in " \t\r\n?#%") or not all(parts)
                or any(part in (".", "..") for part in parts)):
            sys.exit(5)
    # This raw field serves Go mail.ParseAddress and Certbot contact email.
    # Support their common bare-address subset; preserve exact-empty defaults.
    if smtp_from:
        if re.fullmatch(r"[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+", smtp_from) is None:
            sys.exit(6)
        if any(part.startswith(".") or part.endswith(".") or ".." in part
               for part in smtp_from.split("@")):
            sys.exit(6)
except (ValueError, UnicodeError):
    sys.exit(1)
' "${INSTALLER_ANSWER_KEYS[@]}" 2>/dev/null || text_status=$?
  case "$text_status" in
    0) ;;
    2) printf '%s\n' 'installer: invalid feedback mode (yes|no)' >&2; return 1 ;;
    3) printf '%s\n' 'installer: disabled feedback repository is reserved' >&2; return 1 ;;
    4) printf '%s\n' 'installer: enabled feedback requires a repository and PAT' >&2; return 1 ;;
    5) printf '%s\n' 'installer: invalid feedback repository' >&2; return 1 ;;
    6) printf '%s\n' 'installer: invalid SMTP sender' >&2; return 1 ;;
    *) printf '%s\n' 'installer: invalid configuration text' >&2; return 1 ;;
  esac
  validate_domain "${CFG_DOMAIN:-}" || return 1
  validate_tls_mode "${CFG_TLS_MODE:-letsencrypt}" || return 1
  validate_cidrs "${CFG_TRUSTED_CIDRS:-}" || return 1
  validate_smtp_port "${CFG_SMTP_PORT:-}" || return 1
  validate_activity_history_disclosure "${CFG_ACTIVITY_HISTORY_OPERATOR_NAME:-}" \
    "${CFG_ACTIVITY_HISTORY_PRIVACY_POLICY_URL:-}" || return 1
  validate_public_ip "${CFG_PUBLIC_IP:-}" || return 1
  validate_smtp_host "${CFG_SMTP_HOST:-}" || return 1
  : "${CFG_STORAGE_MODE=bundled}"
  if [[ "$CFG_STORAGE_MODE" == bundled ]]; then
    for key in BACKEND ENDPOINT REGION ACCESS_KEY SECRET_KEY USE_SSL BUCKET; do
      key="CFG_STORAGE_$key"
      if [[ -v $key ]]; then
        printf '%s\n' 'installer: bundled storage connection fields refused' >&2
        return 1
      fi
    done
  elif [[ "$CFG_STORAGE_MODE" == byo-s3 ]]; then
    : "${CFG_STORAGE_BACKEND=s3}" "${CFG_STORAGE_REGION=}" \
      "${CFG_STORAGE_USE_SSL=true}" "${CFG_STORAGE_BUCKET=concord-media}"
  fi
  selfhost_storage_validate "$CFG_STORAGE_MODE" "${CFG_STORAGE_BACKEND:-}" \
    "${CFG_STORAGE_ENDPOINT:-}" "${CFG_STORAGE_REGION:-}" "${CFG_STORAGE_ACCESS_KEY:-}" \
    "${CFG_STORAGE_SECRET_KEY:-}" "${CFG_STORAGE_USE_SSL:-}" "${CFG_STORAGE_BUCKET:-}" || return 1
  warn_public_ip "${CFG_PUBLIC_IP:-}"
}

# ── Public IP detection ────────────────────────────────────────────────────────

# detect_public_ip — auto-detect via HTTPS echo service; warn on private/empty.
# Honors PUBLIC_IP env var override.
detect_public_ip() {
  local ip="${PUBLIC_IP:-}"
  [[ -z "$ip" ]] && ip="$(curl -fsS --max-time 5 https://api.ipify.org 2>/dev/null || true)"
  # A non-IP response must never become the wizard's default (#3561). Its own
  # message: the operator typed nothing, so "invalid public IP" would mislead.
  [[ -z "$ip" ]] || validate_public_ip "$ip" 2>/dev/null || {
    echo -e "${YELLOW}⚠ auto-detected public IP is not an IP address; ignoring it${NC}" >&2
    ip=""
  }
  warn_public_ip "$ip"
  printf '%s' "$ip"
}

# ── Config collection ──────────────────────────────────────────────────────────

# load_answers PATH — fixed literal keys, first equals, historical aliases once.
load_answers() {
  local f="${1:-}"
  [[ -f "$f" && -r "$f" ]] || { printf '%s\n' 'installer: answers file unavailable' >&2; return 1; }
  # Bash cannot represent NUL: check original bytes before the first Bash read.
  if ! python3 -c '
import sys
try:
    data = open(sys.argv[1], "rb").read()
    if b"\x00" in data or b"\r" in data:
        raise ValueError()
    data.decode("utf-8")
except (OSError, ValueError, UnicodeError):
    sys.exit(1)
' "$f" 2>/dev/null; then
    printf '%s\n' 'installer: invalid answers encoding' >&2
    return 1
  fi
  local line key val allowed candidate status number=0
  local -A seen=()
  for key in "${INSTALLER_ANSWER_KEYS[@]}" CFG_MINIO_MODE CFG_MINIO_ENDPOINT CFG_MINIO_USER CFG_MINIO_PASS; do
    unset "$key"
  done
  while :; do
    line=''
    if IFS= read -r line; then status=0; else status=$?; fi
    if [[ $status -ne 0 ]]; then
      [[ $status -eq 1 ]] || { printf '%s\n' 'installer: answers read failed' >&2; return 1; }
      [[ -n "$line" ]] || break
    fi
    ((number += 1))
    [[ -z "$line" || "$line" == \#* ]] && continue
    [[ "$line" =~ ^CFG_[A-Z_]+=.*$ ]] || { printf 'installer: malformed answers line %d\n' "$number" >&2; return 1; }
    key="${line%%=*}"; val="${line#*=}"
    case "$key" in
      CFG_MINIO_MODE) key=CFG_STORAGE_MODE ;;
      CFG_MINIO_ENDPOINT) key=CFG_STORAGE_ENDPOINT ;;
      CFG_MINIO_USER) key=CFG_STORAGE_ACCESS_KEY ;;
      CFG_MINIO_PASS) key=CFG_STORAGE_SECRET_KEY ;;
    esac
    allowed=no
    for candidate in "${INSTALLER_ANSWER_KEYS[@]}"; do
      [[ "$key" != "$candidate" ]] || { allowed=yes; break; }
    done
    [[ "$allowed" == yes ]] || { printf 'installer: unknown answers field at line %d\n' "$number" >&2; return 1; }
    [[ ! -v seen[$key] ]] || { printf 'installer: duplicate answers field at line %d\n' "$number" >&2; return 1; }
    seen[$key]=yes
    printf -v "$key" '%s' "$val" || return 1
  done <"$f"
  : "${CFG_FEEDBACK_ENABLED=no}" "${CFG_TURN_REALM:=${CFG_DOMAIN:-}}" \
    "${CFG_TLS_MODE:=letsencrypt}" "${CFG_ACTIVITY_HISTORY_OPERATOR_NAME:=}" \
    "${CFG_ACTIVITY_HISTORY_PRIVACY_POLICY_URL:=}" "${CFG_STORAGE_MODE=bundled}"
  if [[ "$CFG_STORAGE_MODE" == byo-s3 ]]; then
    : "${CFG_STORAGE_BACKEND=s3}" "${CFG_STORAGE_REGION=}" \
      "${CFG_STORAGE_USE_SSL=true}" "${CFG_STORAGE_BUCKET=concord-media}"
  fi
}

# ask PROMPT [DEFAULT] — read a non-secret value from stdin; return default if blank.
ask() {
  local p="$1" d="${2:-}" v
  IFS= read -r -p "$p${d:+ [$d]}: " v || return $?
  printf '%s' "${v:-$d}"
}

# ask_secret PROMPT — read a SECRET with terminal echo OFF (read -rs). The prompt
# and the trailing newline go to stderr so only the captured value reaches stdout.
ask_secret() {
  local p="$1" v
  IFS= read -rs -p "$p: " v || return $?
  printf '\n' >&2 || return 1
  printf '%s' "$v"
}

# collect_config — storage metadata precedes echo-off credentials.
collect_config() {
  echo -e "${BLUE}Concord Voice self-hosted install${NC}"
  local detected_ip key
  for key in "${INSTALLER_ANSWER_KEYS[@]}"; do unset "$key"; done
  CFG_DOMAIN="$(ask 'Primary domain (apex, e.g. example.com)')" || return $?
  validate_domain "$CFG_DOMAIN" || return 1
  CFG_TLS_MODE="$(ask 'TLS mode (letsencrypt recommended / local / import)' letsencrypt)" || return $?
  validate_tls_mode "$CFG_TLS_MODE" || return 1
  if [[ "$CFG_TLS_MODE" == local ]]; then
    echo -e "${YELLOW}⚠ 'local' self-signed has NO MITM protection on untrusted networks — trusted LAN/homelab only. Use 'letsencrypt' for any internet-reachable host.${NC}" >&2
  fi
  CFG_TURN_REALM="$(ask 'TURN realm' "$CFG_DOMAIN")" || return $?
  detected_ip="$(detect_public_ip)" || return $?
  CFG_PUBLIC_IP="$(ask 'Public IPv4 address (blank = auto-detect)' "$detected_ip")" || return $?
  while :; do
    CFG_STORAGE_MODE="$(ask 'Object storage (bundled / byo-s3)' bundled)" || return $?
    case "$CFG_STORAGE_MODE" in bundled|byo-s3) break ;; esac
    printf '%s\n' 'installer: select bundled or byo-s3' >&2
  done
  if [[ "$CFG_STORAGE_MODE" == byo-s3 ]]; then
    CFG_STORAGE_BACKEND=s3
    while :; do
      CFG_STORAGE_ENDPOINT="$(ask 'Object storage endpoint (bare host, optional port)')" || return $?
      if selfhost_storage_validate byo-s3 s3 "$CFG_STORAGE_ENDPOINT" '' pending pending true concord-media; then break; fi
    done
    while :; do
      CFG_STORAGE_USE_SSL="$(ask 'Object storage TLS (true / false)' true)" || return $?
      if selfhost_storage_validate byo-s3 s3 "$CFG_STORAGE_ENDPOINT" '' pending pending "$CFG_STORAGE_USE_SSL" concord-media; then break; fi
    done
    CFG_STORAGE_BUCKET="$(ask 'Object storage bucket' concord-media)" || return $?
    CFG_STORAGE_REGION="$(ask 'Object storage region (optional)')" || return $?
    CFG_STORAGE_ACCESS_KEY="$(ask_secret 'Object storage access key')" || return $?
    CFG_STORAGE_SECRET_KEY="$(ask_secret 'Object storage secret key')" || return $?
  fi
  CFG_SMTP_HOST="$(ask 'SMTP host (required for email verification)')" || return $?
  CFG_SMTP_PORT="$(ask 'SMTP port' 587)" || return $?
  CFG_SMTP_USER="$(ask 'SMTP username')" || return $?
  CFG_SMTP_PASS="$(ask_secret 'SMTP password')" || return $?
  CFG_SMTP_FROM="$(ask 'SMTP from address' "noreply@$CFG_DOMAIN")" || return $?
  CFG_TRUSTED_CIDRS="$(ask 'Trusted proxy CIDRs' '172.16.0.0/12,10.0.0.0/8')" || return $?
  CFG_ACTIVITY_HISTORY_OPERATOR_NAME="$(ask 'Activity History operator name (optional)')" || return $?
  CFG_ACTIVITY_HISTORY_PRIVACY_POLICY_URL="$(ask 'Activity History privacy policy URL (optional, HTTPS)')" || return $?
  CFG_FEEDBACK_ENABLED="$(ask 'Enable in-app feedback (posts to a GitHub repo)? yes/no' no)" || return $?
  if [[ "$CFG_FEEDBACK_ENABLED" == yes ]]; then
    CFG_FEEDBACK_REPO="$(ask 'Feedback repo (owner/repo)')" || return $?
    CFG_FEEDBACK_PAT="$(ask_secret 'Feedback PAT')" || return $?
  fi
}

# ── .env writer ────────────────────────────────────────────────────────────────

# compose_dotenv_quote VALUE — Compose double-quoted dotenv literal. Encode one
# character at a time so adjacent backslashes/quotes cannot interact through
# Bash replacement semantics. Compose uses $$ for a literal dollar inside a
# double-quoted value; backslash and double quote use their standard escapes.
compose_dotenv_quote() {
  local value="${1:-}" encoded="" char i
  if ! printf '%s' "$value" | python3 -c '
import sys
try:
    text = sys.stdin.buffer.read().decode("utf-8")
    if "\x00" in text or "\r" in text or "\n" in text:
        raise ValueError()
except (UnicodeError, ValueError):
    sys.exit(1)
' 2>/dev/null; then
    printf '%s\n' 'installer: invalid dotenv text' >&2
    return 1
  fi
  for ((i = 0; i < ${#value}; i++)); do
    char="${value:i:1}"
    case "$char" in
      \\) encoded+="\\\\" ;;
      \") encoded+="\\\"" ;;
      '$') encoded+="\$\$" ;;
      *) encoded+="$char" ;;
    esac
  done
  printf '"%s"' "$encoded"
}

# write_env_file — stage checked values at 0600, publish without replacement.
# Logs var NAMES only, never values (observability rule — no secret values to stdout/log).
write_env_file() (
  # A subshell scopes cleanup traps and umask to this writer.
  validate_collected_config || return 1
  validate_media_subdomain "https://media.${CFG_DOMAIN}" || return 1
  if [[ -e "$ENV_OUT" || -L "$ENV_OUT" ]]; then
    printf '%s\n' 'installer: environment publication refused' >&2
    return 1
  fi
  local env_dir env_in_git=no
  env_dir="$(dirname "$ENV_OUT")" || return 1
  if git -C "$env_dir" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    env_in_git=yes
    if ! git -C "$env_dir" check-ignore -q "$ENV_OUT"; then
      printf '%s\n' 'installer: environment destination is not gitignored' >&2
      return 1
    fi
  fi
  local feedback_repo feedback_pat disabled_token
  if [[ "${CFG_FEEDBACK_ENABLED:-no}" == yes ]]; then
    feedback_repo="${CFG_FEEDBACK_REPO:-}"
    feedback_pat="${CFG_FEEDBACK_PAT:-}"
  else
    feedback_repo=selfhost/disabled
    disabled_token="$(gen_secret)" || return 1
    feedback_pat="disabled-$disabled_token"
  fi
  local postgres_password redis_password jwt_secret mfa_key turn_secret minio_password
  postgres_password="$(gen_secret)" || return 1
  redis_password="$(gen_secret)" || return 1
  jwt_secret="$(gen_secret)" || return 1
  mfa_key="$(gen_hex_key)" || return 1
  turn_secret="$(gen_secret)" || return 1
  minio_password="$(gen_secret)" || return 1
  # All operator values use one literal grammar, including older writer fields.
  local domain public_ip realm cidrs smtp_host smtp_port smtp_user smtp_pass smtp_from
  local quoted_repo quoted_pat operator privacy coturn_certs
  domain="$(compose_dotenv_quote "$CFG_DOMAIN")" || return 1
  public_ip="$(compose_dotenv_quote "${CFG_PUBLIC_IP:-}")" || return 1
  realm="$(compose_dotenv_quote "${CFG_TURN_REALM:-$CFG_DOMAIN}")" || return 1
  cidrs="$(compose_dotenv_quote "${CFG_TRUSTED_CIDRS:-172.16.0.0/12,10.0.0.0/8}")" || return 1
  smtp_host="$(compose_dotenv_quote "${CFG_SMTP_HOST:-}")" || return 1
  smtp_port="$(compose_dotenv_quote "${CFG_SMTP_PORT:-587}")" || return 1
  smtp_user="$(compose_dotenv_quote "${CFG_SMTP_USER:-}")" || return 1
  smtp_pass="$(compose_dotenv_quote "${CFG_SMTP_PASS:-}")" || return 1
  smtp_from="$(compose_dotenv_quote "${CFG_SMTP_FROM:-noreply@$CFG_DOMAIN}")" || return 1
  quoted_repo="$(compose_dotenv_quote "$feedback_repo")" || return 1
  quoted_pat="$(compose_dotenv_quote "$feedback_pat")" || return 1
  operator="$(compose_dotenv_quote "${CFG_ACTIVITY_HISTORY_OPERATOR_NAME:-}")" || return 1
  privacy="$(compose_dotenv_quote "${CFG_ACTIVITY_HISTORY_PRIVACY_POLICY_URL:-}")" || return 1
  coturn_certs="$(compose_dotenv_quote '/opt/concord/certs/coturn')" || return 1
  local backend endpoint region access secret tls bucket timestamp
  if [[ "$CFG_STORAGE_MODE" == byo-s3 ]]; then
    backend="$(compose_dotenv_quote "$CFG_STORAGE_BACKEND")" || return 1
    endpoint="$(compose_dotenv_quote "$CFG_STORAGE_ENDPOINT")" || return 1
    region="$(compose_dotenv_quote "$CFG_STORAGE_REGION")" || return 1
    access="$(compose_dotenv_quote "$CFG_STORAGE_ACCESS_KEY")" || return 1
    secret="$(compose_dotenv_quote "$CFG_STORAGE_SECRET_KEY")" || return 1
    tls="$(compose_dotenv_quote "$CFG_STORAGE_USE_SSL")" || return 1
    bucket="$(compose_dotenv_quote "$CFG_STORAGE_BUCKET")" || return 1
  fi
  timestamp="$(date -u +%FT%TZ)" || return 1
  local -a lines=(
    "# Concord Voice self-hosted env — generated by install-selfhost.sh ($timestamp)"
    "# Self-hosted writer surface; managed provisioning is separate."
    ENVIRONMENT=production INSTANCE_TYPE=self-hosted
    "SELFHOST_STORAGE_MODE=$CFG_STORAGE_MODE" "DOMAIN=$domain" "PUBLIC_IP=$public_ip"
    "COTURN_CERTS_DIR=$coturn_certs"
    "POSTGRES_PASSWORD=$postgres_password" "REDIS_PASSWORD=$redis_password"
    "JWT_SECRET=$jwt_secret" "MFA_ENCRYPTION_KEY=$mfa_key" "TURN_SECRET=$turn_secret"
    "TURN_REALM=$realm" MINIO_ROOT_USER=concord "MINIO_ROOT_PASSWORD=$minio_password"
    MINIO_BUCKET=concord-media "TRUSTED_PROXY_CIDRS=$cidrs"
    "SMTP_HOST=$smtp_host" "SMTP_PORT=$smtp_port" "SMTP_USERNAME=$smtp_user"
    "SMTP_PASSWORD=$smtp_pass" "SMTP_FROM=$smtp_from"
    "FEEDBACK_REPO=$quoted_repo" "FEEDBACK_PAT=$quoted_pat"
    ACTIVITY_HISTORY_CLUSTER_ENABLED=false CONTROL_PLANE_REPLICA_COUNT=1
    "ACTIVITY_HISTORY_OPERATOR_NAME=$operator" "ACTIVITY_HISTORY_PRIVACY_POLICY_URL=$privacy"
  )
  if [[ "$CFG_STORAGE_MODE" == byo-s3 ]]; then
    lines+=("STORAGE_BACKEND=$backend" "STORAGE_ENDPOINT=$endpoint" "STORAGE_REGION=$region"
      "STORAGE_ACCESS_KEY=$access" "STORAGE_SECRET_KEY=$secret" "STORAGE_USE_SSL=$tls" "STORAGE_BUCKET=$bucket")
  fi
  local staged_env=''
  trap 'if [[ -n "$staged_env" ]] && ! rm -f -- "$staged_env" 2>/dev/null; then printf "%s\n" "installer: staging cleanup failed" >&2; exit 1; fi' EXIT
  trap 'exit 1' HUP INT TERM
  staged_env="$(umask 077; mktemp "${ENV_OUT}.new.XXXXXX.env")" || return 1
  if [[ "$env_in_git" == yes ]] && ! git -C "$env_dir" check-ignore -q "$staged_env"; then
    printf '%s\n' 'installer: environment staging is not gitignored' >&2
    return 1
  fi
  chmod 600 "$staged_env" || return 1
  if ! printf '%s\n' "${lines[@]}" >"$staged_env"; then
    printf '%s\n' 'installer: environment staging failed' >&2
    return 1
  fi
  # -T refuses existing directories and symlinks to directories as well.
  if ! ln -T -- "$staged_env" "$ENV_OUT" 2>/dev/null; then
    printf '%s\n' 'installer: environment publication refused' >&2
    return 1
  fi
  rm -f -- "$staged_env" || return 1
  staged_env=''
  printf '%s\n' 'installer: wrote environment (mode 0600)'
)

# Validated storage metadata only; no credential bytes leave this function.
print_storage_review() {
  selfhost_storage_validate "${CFG_STORAGE_MODE:-bundled}" "${CFG_STORAGE_BACKEND:-}" \
    "${CFG_STORAGE_ENDPOINT:-}" "${CFG_STORAGE_REGION:-}" "${CFG_STORAGE_ACCESS_KEY:-}" \
    "${CFG_STORAGE_SECRET_KEY:-}" "${CFG_STORAGE_USE_SSL:-}" "${CFG_STORAGE_BUCKET:-}" || return 1
  printf 'Object storage mode: %s\n' "${CFG_STORAGE_MODE:-bundled}" || return 1
  if [[ "${CFG_STORAGE_MODE:-bundled}" == byo-s3 ]]; then
    printf 'Endpoint: %s\nBackend: %s\nRegion: %s\nBucket: %s\nTLS: %s\n' \
      "$CFG_STORAGE_ENDPOINT" "$CFG_STORAGE_BACKEND" "${CFG_STORAGE_REGION:-(default / empty)}" \
      "$CFG_STORAGE_BUCKET" "$CFG_STORAGE_USE_SSL" || return 1
    printf '%s\n' 'Access key: provided (hidden)' 'Secret key: provided (hidden)' || return 1
  fi
}

# ── nginx render ───────────────────────────────────────────────────────────────

# render_nginx — substitute concordvoice.chat -> $CFG_DOMAIN across the canonical
# vhost (single source of truth; managed deploy path untouched). The domain charset
# is restricted by validate_domain, so the sed substitution is injection-safe.
# nginx -t is a manual/integration step (needs cert files present), per the runbook.
render_nginx() {
  [[ -f "$NGINX_SRC" ]] || {
    echo -e "${RED}nginx source not found: $NGINX_SRC${NC}" >&2
    return 1
  }
  sed "s/concordvoice\.chat/${CFG_DOMAIN}/g" "$NGINX_SRC" >"$NGINX_OUT" || return 1
  echo -e "${GREEN}✓ rendered nginx vhost -> $NGINX_OUT${NC} (run: sudo cp it into /etc/nginx/... then nginx -t)"
}

# ── TLS cert provisioning (#1617) ────────────────────────────────────────────

# shq VALUE — shell-quote VALUE for a command the operator pastes into a root
# shell (#3561). Domain and IP are validated, but $SCRIPT_DIR comes from the
# checkout path (spaces are ordinary) and SMTP_FROM is free text.
shq() { printf '%q' "$1"; }

# say_root_cmd TEXT — print a ready-to-paste root command. printf, never echo -e,
# so a backslash that shq produced is not re-interpreted.
say_root_cmd() { printf '%b    %s%b\n' "$YELLOW" "$1" "$NC" >&2; }

# provision_tls — print the provision-cert.sh command for the selected mode; it
# never runs it. Every mode writes root-owned paths (/etc/letsencrypt, coturn
# certs, an nginx reload) and this installer runs unprivileged (#3561). Mode is
# a wizard answer / CLI arg, NEVER written to .env (DR-1). PROVISION_CERT is
# overridable for tests (default: the sibling script).
provision_tls() {
  local pc="${PROVISION_CERT:-$SCRIPT_DIR/provision-cert.sh}"
  case "${CFG_TLS_MODE:-letsencrypt}" in
    local)
      # Self-signed needs no webroot or port 80, so it can run straight away.
      echo -e "${YELLOW}⚠ local: run as root now (self-signed, trusted LAN only):${NC}" >&2
      say_root_cmd "sudo $(shq "$pc") local --host $(shq "$CFG_DOMAIN")${CFG_PUBLIC_IP:+ --ip $(shq "$CFG_PUBLIC_IP")}"
      ;;
    letsencrypt)
      # certbot --webroot needs DNS pointing here and /var/www/html served on port
      # 80, which nginx's stock default site does. Every mode runs before the vhost
      # is installed (summary step 3): nginx -t needs the cert files. Unlike
      # provision-production.sh, which bootstraps ufw/webroot/nginx-on-80 before
      # delegating, the installer does not.
      echo -e "${YELLOW}⚠ letsencrypt: run as root once DNS points here and port 80 is open, before installing the vhost (steps below):${NC}" >&2
      say_root_cmd "sudo $(shq "$pc") letsencrypt --domain $(shq "$CFG_DOMAIN") --email $(shq "${CFG_SMTP_FROM:-admin@$CFG_DOMAIN}")"
      ;;
    import)
      # Path placeholders, not <cert.pem>: pasted as-is, '<' and '>' are
      # redirections that create files named after the next flag.
      echo -e "${YELLOW}⚠ import: run as root with your PEM pair, before installing the vhost:${NC}" >&2
      say_root_cmd "sudo $(shq "$pc") import --cert /path/to/fullchain.pem --key /path/to/privkey.pem --host $(shq "$CFG_DOMAIN")"
      ;;
  esac
}

# ── Summary ────────────────────────────────────────────────────────────────────

print_summary() {
  # Paste-safe paths (#3561): shq output reaches the terminal byte-for-byte
  # through a heredoc. echo -e re-read it, so a backslash in the checkout path
  # vanished and the pasted command named a different file.
  local nginx_q boundary_q
  nginx_q="$(shq "$NGINX_OUT")"
  boundary_q="$(shq "$SCRIPT_DIR/selfhost-security-event-logging.sh")"
  if [[ "${CFG_STORAGE_MODE:-bundled}" == byo-s3 ]]; then
    printf '%s\n' 'Install configured. External storage has not been checked. The control-plane checks it when the stack starts.' || return 1
  fi
  printf '\n%bInstall configured. Certificate delivery and coturn TLS checks are pending.%b Next steps:\n' "$GREEN" "$NC"
  cat <<EOF
  1. Firewall (UFW). UFW filters HOST processes (nginx, sshd):
       sudo ufw allow 80,443/tcp
     Docker-published ports are not filtered by UFW on IPv4 (Docker's rules run first).
     The stack publishes only TURN and media publicly; allow them so IPv6 and UFW's own
     view stay correct:
       sudo ufw allow 3478,5349/tcp
       sudo ufw allow 3478/udp
       sudo ufw allow 49152:49252/udp
       sudo ufw allow 40000:41999/udp
     Postgres, Redis and NATS have no host socket; 8080/3000 listen on 127.0.0.1 only.
     Check: docs/runbooks/selfhost-quickstart.md § Verify your exposure
  2. TLS certs (mode '${CFG_TLS_MODE:-letsencrypt}'): run the provision-cert.sh command printed
     above, as root, BEFORE step 3: nginx -t needs the cert files. 'letsencrypt' also needs
     DNS pointing here and port 80 open (step 1); nginx's stock default site answers the
     challenge. Self-signed 'local' is trusted-LAN-only (no MITM protection).
     Guide: docs/runbooks/selfhost-quickstart.md
  3. Install the rendered nginx vhost:
       sudo cp $nginx_q /etc/nginx/conf.d/ && sudo nginx -t && sudo systemctl reload nginx
  4. Prepare the security-event log boundary (root, once per host; safe to re-run):
       sudo $boundary_q security-event-logging
     'up' refuses until it exists: the control-plane cannot start without its log stream.
  5. Start the stack:  ./infrastructure/deploy/concord-selfhost up
  6. Check health:     ./infrastructure/deploy/concord-selfhost health
EOF
}

# ── Main ───────────────────────────────────────────────────────────────────────

main() {
  case "${1:-}" in
    --help|-h) usage; return $? ;;
    --env-file)
      [[ $# -eq 2 && -n "$2" ]] || { printf '%s\n' 'installer: --env-file needs one path' >&2; return 2; }
      ;;
    "") [[ $# -eq 0 ]] || return 2 ;;
    *) printf '%s\n' 'installer: unknown argument' >&2; usage >&2; return 2 ;;
  esac
  selfhost_storage_assert_fresh "$PROJECT_ROOT" early || return 1
  if [[ "${1:-}" == --env-file ]]; then
    load_answers "$2" || return $?
  else
    collect_config || return $?
  fi
  validate_collected_config || return $?
  run_preflight || return $?
  selfhost_storage_assert_fresh "$PROJECT_ROOT" final || return 1
  print_storage_review || return $?
  render_nginx || return $?
  write_env_file || return $?
  provision_tls || return $?
  print_summary || return $?
}

# ── Source guard ───────────────────────────────────────────────────────────────

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
