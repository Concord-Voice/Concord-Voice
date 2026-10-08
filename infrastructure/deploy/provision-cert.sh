#!/usr/bin/env bash
# Concord Voice — self-hosted TLS/cert provisioning (#1617).
# Root-only deploy script. Three modes converge on the canonical cert dir
# /etc/letsencrypt/live/${DOMAIN}/ (consumed identically by the rendered nginx
# vhost and copy-certs.sh -> coturn). No custom crypto (openssl/certbot only).
# All modes require Python 3 with native SSL certificate decoding support.
# Local/import activation requires Linux renameat2 support and flock.
# No private key / PEM bytes are ever logged.
#
# Usage:
#   provision-cert.sh local       --host <h> [--ip <a>] [--days <n>]
#   provision-cert.sh letsencrypt --domain <d> --email <e> [--managed-cloudflare-upload]
#   provision-cert.sh import      --cert <p> --key <p> --host <h>
#
# Exit codes: 0 ok | 1 runtime/validation | 2 usage
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
: "${LE_LIVE_ROOT:=/etc/letsencrypt/live}"   # overridable for tests
: "${HOOK_DIR:=/etc/letsencrypt/renewal-hooks/deploy}"

readonly RED='\033[0;31m' GREEN='\033[0;32m' YELLOW='\033[1;33m' NC='\033[0m'
log()  { echo -e "${GREEN}$*${NC}"; }
warn() { echo -e "${YELLOW}$*${NC}" >&2; }
err()  { echo -e "${RED}$*${NC}" >&2; }

usage() {
  cat <<'USAGE'
Concord Voice TLS/cert provisioning

Usage:
  provision-cert.sh local       --host <h> [--ip <a>] [--days <n>]
  provision-cert.sh letsencrypt --domain <d> --email <e> [--managed-cloudflare-upload]
  provision-cert.sh import      --cert <p> --key <p> --host <h>

Let's Encrypt defaults to self-host copy-only delivery. The managed Cloudflare
upload option is reserved for explicitly authorized managed deployments.
For import, --host is the deployment base. The pair must cover that name and
api.<host>, media.<host> and turn.<host>, exactly or with a one-label wildcard.
Certificate validity checks require Python 3 with native SSL certificate decoding support.

Exit codes: 0 ok | 1 runtime/validation | 2 usage
USAGE
}

# derive_sans HOST [IP] — SAN string covering the host + api/media/turn/spa
# subdomains (superset of the names nginx presents), plus IP when non-empty.
derive_sans() {
  local h="$1" ip="${2:-}"
  local s="DNS:${h},DNS:api.${h},DNS:media.${h},DNS:turn.${h},DNS:spa.${h}"
  [[ -n "$ip" ]] && s="${s},IP:${ip}"
  printf '%s' "$s"
}

# validate_host HOST — strict FQDN (mirrors install-selfhost.sh validate_domain).
# This is the single charset gate for the standalone entry point: rejecting
# anything that isn't an FQDN blocks '/' (openssl -subj RDN injection), ',' and
# ':' (SAN-list injection via derive_sans / -addext), regex metachars (the SAN
# ERE-match bypass), whitespace, and single-label hosts — at ONE point, before
# the value reaches any openssl/grep. install-selfhost.sh pre-validates CFG_DOMAIN,
# but provision-cert.sh is also invoked directly by operators (per the runbook).
validate_host() {
  local host="${1:-}"
  # All generated names must fit DNS limits; media. adds the longest prefix.
  [[ ${#host} -le 247 && "$host" =~ ^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z]{2,63}$ ]] || {
    err "invalid host: $host (expected an FQDN, e.g. example.com or homelab.lan)"; return 1; }
}

# validate_ip IP — literal IPv4 or IPv6 only, so a value with ',' / 'DNS:' /
# metachars can never inject extra SAN entries through derive_sans.
# Certificate SANs support IPv6 separately from basic self-host's advertised
# IPv4 TURN address. This shape check establishes no runtime network support.
validate_ip() {
  local ip="${1:-}"
  [[ -z "$ip" || "$ip" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ || "$ip" =~ ^[0-9A-Fa-f:]*:[0-9A-Fa-f:.]*$ ]] || {
    err "invalid --ip: $ip (expected an IPv4 or IPv6 address)"; return 1; }
}

# canonical_dir DOMAIN — the single cert dir all three modes write/read.
canonical_dir() { printf '%s/%s' "$LE_LIVE_ROOT" "$1"; }

# fingerprint CERT — SHA-256 of the cert (safe to log; never the key).
fingerprint() { openssl x509 -in "$1" -noout -fingerprint -sha256 2>/dev/null | cut -d= -f2; }

# run_copy_certs DIR — invoke the existing deploy hook to fan certs to coturn
# + reload nginx (its documented manual-invocation path). COPY_CERTS_HOOK is
# overridable for tests (default: the real sibling copy-certs.sh).
run_copy_certs() {
  local dir="$1" hook="${COPY_CERTS_HOOK:-${SCRIPT_DIR}/copy-certs.sh}"
  local output
  if ! output="$(RENEWED_LINEAGE="$dir" bash "$hook")"; then
    return 1
  fi
  case "$output" in
    $'copy-certs: certificate pair delivery complete.\ncopy-certs: coturn-stage=prepared')
      COPY_COTURN_STAGE=prepared ;;
    $'copy-certs: certificate pair delivery complete.\ncopy-certs: coturn-stage=restarted')
      COPY_COTURN_STAGE=restarted ;;
    *)
      err 'tls-provision: coturn delivery result unavailable; delivery not complete'
      return 1 ;;
  esac
}

require_root() {
  [[ "$EUID" -eq 0 ]] || {
    err 'tls-provision: root required; certificate delivery not started'
    return 1
  }
}

# All modes write the same Certbot lineage. A retained managed deploy hook
# can upload its active key on a later renewal, regardless of how it was made.
# Refuse public provisioning before reading or changing certificate material;
# leave the managed hook intact for its explicitly authorized caller.
require_public_certificate_authority() {
  if [[ -e "${HOOK_DIR}/upload-cert-to-cloudflare.sh" ||
      -L "${HOOK_DIR}/upload-cert-to-cloudflare.sh" ]]; then
    err 'tls-provision: managed renewal hook present; self-host issuance not started'
    return 1
  fi
}

require_copy_hook() {
  local hook="${COPY_CERTS_HOOK:-${SCRIPT_DIR}/copy-certs.sh}"
  [[ -f "$hook" && -r "$hook" && -f "${SCRIPT_DIR}/copy-certs.sh" && -r "${SCRIPT_DIR}/copy-certs.sh" ]] || {
    err 'tls-provision: required copy hook unavailable; delivery not complete'
    return 1
  }
}

install_required_renewal_hook() {
  local source="${SCRIPT_DIR}/copy-certs.sh" target="${HOOK_DIR}/copy-certs.sh"
  local lineage staging candidate prepared
  lineage="$(canonical_dir "$1")"
  mkdir -p "$HOOK_DIR" 2>/dev/null || {
    err 'tls-provision: renewal hook installation failed; delivery not complete'
    return 1
  }
  staging="$(mktemp -d "${HOOK_DIR}/.copy-certs.XXXXXX")" || {
    err 'tls-provision: renewal hook installation failed; delivery not complete'
    return 1
  }
  candidate="${staging}/candidate"
  prepared="${staging}/installed"
  # Directory hooks run for every Certbot certificate. Bind this executable to
  # the complete selected lineage before the shared delivery helper can act.
  # Bash %q preserves an overridden live root as literal shell data. Prepare
  # inside a private directory on the hook filesystem so Certbot cannot run a
  # staging executable. Verify it before atomically replacing the working hook.
  if ! {
      printf '%s\n' '#!/usr/bin/env bash' '# Generated by provision-cert.sh for the selected Concord lineage.'
      printf '[[ "${RENEWED_LINEAGE:-}" == %q ]] || exit 0\n' "$lineage"
      cat "$source"
    } >"$candidate" ||
      ! install -o root -g root -m 0755 "$candidate" "$prepared" 2>/dev/null ||
      [[ ! -f "$prepared" || -L "$prepared" ]] ||
      ! cmp -s "$candidate" "$prepared" ||
      [[ "$source" -ef "$prepared" || "$candidate" -ef "$prepared" ]] ||
      [[ "$(stat -Lc '%u:%g:%a' "$prepared" 2>/dev/null)" != '0:0:755' ]] ||
      ! mv -Tf -- "$prepared" "$target" 2>/dev/null ||
      [[ ! -f "$target" || -L "$target" ]] ||
      ! cmp -s "$candidate" "$target" ||
      [[ "$source" -ef "$target" || "$candidate" -ef "$target" ]] ||
      [[ "$(stat -Lc '%u:%g:%a' "$target" 2>/dev/null)" != '0:0:755' ]]; then
    rm -rf -- "$staging"
    err 'tls-provision: renewal hook installation failed; delivery not complete'
    return 1
  fi
  rm -rf -- "$staging"
}

certificate_valid_now() {
  python3 -c '
import ssl, sys, time
try:
    decode = getattr(getattr(ssl, "_ssl", None), "_test_decode_cert", None)
    if not callable(decode):
        raise ValueError()
    certificate = decode(sys.argv[1])
    now = time.time()
    if not (ssl.cert_time_to_seconds(certificate["notBefore"]) <= now <
            ssl.cert_time_to_seconds(certificate["notAfter"])):
        raise ValueError()
except Exception:
    sys.exit(1)
' "$1" >/dev/null 2>&1
}

validate_source_pair() {
  local dir="$1" cert="${1}/fullchain.pem" key="${1}/privkey.pem"
  local check_expiry="${2:-true}"
  local cert_pub key_pub
  if [[ ! -f "$cert" || ! -s "$cert" || ! -f "$key" || ! -s "$key" || -L "$cert" && ! -f "$cert" ]]; then
    err 'tls-provision: certificate source incomplete; delivery not complete'
    return 1
  fi
  if [[ "$(stat -Lc '%u:%g:%a' "$key" 2>/dev/null)" != '0:0:600' ]] ||
      ! openssl x509 -in "$cert" -noout >/dev/null 2>&1 ||
      ! openssl pkey -in "$key" -noout >/dev/null 2>&1; then
    err 'tls-provision: certificate source incomplete; delivery not complete'
    return 1
  fi
  if [[ "$check_expiry" == true ]] && {
      ! openssl x509 -in "$cert" -noout -checkend 0 >/dev/null 2>&1 ||
      ! certificate_valid_now "$cert";
    }; then
    err 'tls-provision: certificate source incomplete; delivery not complete'
    return 1
  fi
  cert_pub="$(openssl x509 -in "$cert" -noout -pubkey 2>/dev/null)" || cert_pub=''
  key_pub="$(openssl pkey -in "$key" -pubout 2>/dev/null)" || key_pub=''
  if [[ -z "$cert_pub" || -z "$key_pub" || "$cert_pub" != "$key_pub" ]]; then
    err 'tls-provision: certificate source incomplete; delivery not complete'
    return 1
  fi
}

complete_delivery() {
  local dir="$1" root="${COTURN_CERT_ROOT:-/opt/concord/certs}" target="${COTURN_CERT_ROOT:-/opt/concord/certs}/coturn"
  validate_source_pair "$dir" || return 1
  run_copy_certs "$dir" || return 1
  if [[ ! -d "$root" || -L "$root" || ! -d "$target" || -L "$target" ||
        "$(stat -Lc '%u:%g:%a' "$root" 2>/dev/null)" != '0:0:711' ||
        "$(stat -Lc '%u:%g:%a' "$target" 2>/dev/null)" != '0:0:711' ||
        ! -f "$target/cert.pem" || -L "$target/cert.pem" ||
        ! -f "$target/key.pem" || -L "$target/key.pem" ||
        "$(stat -Lc '%u:%g:%a' "$target/cert.pem" 2>/dev/null)" != '0:0:444' ||
        "$(stat -Lc '%u:%g:%a' "$target/key.pem" 2>/dev/null)" != '65534:65533:400' ]]; then
    err 'tls-provision: certificate source incomplete; delivery not complete'
    return 1
  fi
  if ! cmp -s "$dir/fullchain.pem" "$target/cert.pem" ||
      ! cmp -s "$dir/privkey.pem" "$target/key.pem"; then
    err 'tls-provision: certificate source incomplete; delivery not complete'
    return 1
  fi
}

delivery_complete() {
  case "${COPY_COTURN_STAGE:-}" in
    prepared|restarted) ;;
    *)
      err 'tls-provision: coturn delivery result unavailable; delivery not complete'
      return 1 ;;
  esac
  printf '%s\n' 'Certificate source and coturn delivery complete.'
  case "$COPY_COTURN_STAGE" in
    prepared)
      printf '%s\n' 'coturn is not started; delivery is prepared for first start.'
      printf '%s\n' 'TLS readiness has not been checked.'
      printf '%s\n' 'Install and reload the rendered nginx vhost, then run concord-selfhost up.' ;;
    restarted)
      printf '%s\n' 'coturn was restarted; TLS readiness has not been checked.'
      printf '%s\n' 'Run concord-selfhost health after installing and reloading the rendered nginx vhost.' ;;
  esac
}

# The source namespace must be root-controlled before creating a lock or stage.
# Read/traverse access is compatible with Certbot; group/other writes are not.
trusted_generation_directory() {
  local dir="$1" metadata
  [[ -d "$dir" && ! -L "$dir" ]] || return 1
  metadata="$(stat -Lc '%u:%g:%a' "$dir" 2>/dev/null)" || return 1
  [[ "$metadata" =~ ^0:0:([0-7]{3,4})$ ]] || return 1
  (( (8#${BASH_REMATCH[1]} & 0022) == 0 ))
}

# Fresh local/import hosts need no Certbot installation. Build absent namespace
# parents only beneath an existing trusted directory, validating each component
# instead of allowing mkdir -p to traverse an unsafe parent or symlink.
prepare_generation_namespace() {
  local namespace="$1" parent="${1%/*}" path
  local -a missing=()
  [[ -n "$parent" ]] || parent=/
  while [[ ! -e "$parent" && ! -L "$parent" ]]; do
    missing=("$parent" "${missing[@]}")
    parent="${parent%/*}"
    [[ -n "$parent" ]] || parent=/
  done
  trusted_generation_directory "$parent" || return 1
  for path in "${missing[@]}"; do
    mkdir -- "$path" 2>/dev/null || return 1
    trusted_generation_directory "$path" || return 1
  done
  if [[ ! -e "$namespace" && ! -L "$namespace" ]]; then
    mkdir -- "$namespace" 2>/dev/null || return 1
  fi
  trusted_generation_directory "$namespace"
}

# One Linux namespace operation replaces the entire generation, never its two
# files separately. Expected directory identities also cover a signal arriving
# after the syscall completed but before Bash recorded its result. No fallback
# may remove the live directory or emulate exchange with multiple renames.
atomic_generation_rename() {
  python3 -c '
import ctypes, os, stat, sys
try:
    if sys.platform != "linux":
        raise ValueError()
    root, source, target, source_id, target_id = sys.argv[1:]
    if any(name in ("", ".", "..") or "/" in name for name in (source, target)):
        raise ValueError()
    directory = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    def identity(name):
        value = os.stat(name, dir_fd=directory, follow_symlinks=False)
        if not stat.S_ISDIR(value.st_mode):
            raise ValueError()
        return f"{value.st_dev}:{value.st_ino}"
    if identity(source) != source_id:
        raise ValueError()
    if target_id == "absent":
        try:
            identity(target)
        except FileNotFoundError:
            pass
        else:
            raise ValueError()
        flags = 1  # RENAME_NOREPLACE
    else:
        if identity(target) != target_id:
            raise ValueError()
        flags = 2  # RENAME_EXCHANGE
    rename = getattr(ctypes.CDLL(None, use_errno=True), "renameat2", None)
    if rename is None:
        raise ValueError()
    rename.argtypes = (ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint)
    rename.restype = ctypes.c_int
    if rename(directory, os.fsencode(source), directory, os.fsencode(target), flags) != 0:
        raise OSError()
    os.close(directory)
except Exception:
    sys.exit(1)
' "$@" 2>/dev/null
}

# Local/import generations are private until validation succeeds. A directory
# exchange keeps every resolved canonical generation complete; independent
# file opens spanning the exchange are not a consumer snapshot guarantee.
# The exchanged previous directory is retained, including after failed recovery.
replace_source_generation() {
  local generation_mode="$1" generation_host="$2" generation_first="$3" generation_second="$4"
  local generation_dir generation_lock generation_lock_fd generation_candidate generation_candidate_id generation_previous_id=absent generation_committed=false
  local generation_root="${LE_LIVE_ROOT%/}" renewal_root
  renewal_root="${generation_root%/*}/renewal"
  generation_dir="$(canonical_dir "$generation_host")"
  umask 077
  if [[ "$generation_root" != /* || -L "$generation_root" ]] ||
      ! prepare_generation_namespace "$generation_root"; then
    err 'tls-provision: certificate namespace untrusted; delivery not started'
    return 1
  fi
  generation_lock="${generation_root}/.${generation_host}.lock"
  # noclobber creates a fresh inode without following an existing symlink.
  # The trusted parent prevents substitution between validation and opening.
  if [[ ! -e "$generation_lock" && ! -L "$generation_lock" ]]; then
    (set -o noclobber; : >"$generation_lock") 2>/dev/null || {
      err 'tls-provision: certificate transaction lock unavailable; delivery not started'
      return 1
    }
  fi
  if [[ ! -f "$generation_lock" || -L "$generation_lock" ||
      "$(stat -Lc '%u:%g:%a' "$generation_lock" 2>/dev/null)" != '0:0:600' ]] ||
      ! { exec {generation_lock_fd}<>"$generation_lock"; } 2>/dev/null ||
      ! flock -n "$generation_lock_fd" 2>/dev/null; then
    err 'tls-provision: certificate transaction lock unavailable; delivery not started'
    return 1
  fi
  # Certbot owns symlink/archive and renewal metadata. Do not replace that
  # lineage with a second writer, even when its certificate is expired.
  if [[ -L "$generation_dir" || -L "$generation_dir/fullchain.pem" || -L "$generation_dir/privkey.pem" ||
      -e "${renewal_root}/${generation_host}.conf" || -L "${renewal_root}/${generation_host}.conf" ]]; then
    err 'tls-provision: Certbot-managed lineage present; replacement not started'
    return 1
  fi
  if [[ -e "$generation_dir" ]]; then
    if ! trusted_generation_directory "$generation_dir" ||
        [[ "$(stat -Lc '%u:%g:%a' "$generation_dir/fullchain.pem" 2>/dev/null)" != '0:0:644' ]] ||
        ! validate_source_pair "$generation_dir" false; then
      err 'tls-provision: previous certificate generation invalid; replacement not started'
      return 1
    fi
    generation_previous_id="$(stat -c '%d:%i' "$generation_dir" 2>/dev/null)" || return 1
  fi
  generation_candidate="$(mktemp -d "${generation_root}/.${generation_host}.generation.XXXXXX" 2>/dev/null)" || {
    err 'tls-provision: certificate staging failed; delivery not complete'
    return 1
  }
  generation_candidate_id="$(stat -c '%d:%i' "$generation_candidate" 2>/dev/null)" || {
    err 'tls-provision: certificate staging failed; delivery not complete'
    return 1
  }
  finish_source_generation() {
    local status="$1" live_id stage_id
    trap - EXIT
    trap '' INT TERM
    [[ "$generation_committed" == true ]] && return "$status"
    live_id="$(stat -c '%d:%i' "$generation_dir" 2>/dev/null)" || live_id=absent
    stage_id="$(stat -c '%d:%i' "$generation_candidate" 2>/dev/null)" || stage_id=absent
    if [[ "$live_id" == "$generation_candidate_id" ]]; then
      if [[ "$generation_previous_id" == absent ]]; then
        err 'tls-provision: certificate recovery unavailable; no previous generation; delivery not complete'
        return 1
      fi
      if [[ "$stage_id" != "$generation_previous_id" ]] ||
          ! atomic_generation_rename "$generation_root" "${generation_candidate##*/}" "$generation_host" "$generation_previous_id" "$generation_candidate_id" ||
          ! complete_delivery "$generation_dir"; then
        err 'tls-provision: certificate recovery failed; generations retained; delivery not complete'
        return 1
      fi
      # Recovery succeeded. Keep the failed new generation as well as the
      # restored old pair; never claim that the requested activation completed.
      err 'tls-provision: previous certificate generation restored; delivery not complete'
      return "$status"
    fi
    if [[ "$live_id" == "$generation_previous_id" && "$stage_id" == "$generation_candidate_id" ]]; then
      if ! rm -rf -- "$generation_candidate" 2>/dev/null; then
        err 'tls-provision: certificate staging cleanup failed; delivery not complete'
        return 1
      fi
      return "$status"
    fi
    err 'tls-provision: certificate recovery state unavailable; generations retained; delivery not complete'
    return 1
  }
  trap 'finish_source_generation "$?"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  build_and_activate_source_generation() {
    if [[ "$generation_mode" == local ]]; then
      if ! openssl req -x509 -newkey rsa:4096 -sha256 -nodes -days "$generation_first" \
          -keyout "$generation_candidate/privkey.pem" -out "$generation_candidate/fullchain.pem" \
          -subj "/CN=${generation_host}" -addext "subjectAltName=${generation_second}" >/dev/null 2>&1; then
        err 'local: openssl self-signed generation failed'
        return 1
      fi
    elif ! cp "$generation_first" "$generation_candidate/fullchain.pem" 2>/dev/null ||
        ! cp "$generation_second" "$generation_candidate/privkey.pem" 2>/dev/null; then
      err 'tls-provision: certificate staging failed; delivery not complete'
      return 1
    fi
    if ! chown root:root "$generation_candidate" "$generation_candidate/fullchain.pem" "$generation_candidate/privkey.pem" 2>/dev/null ||
        ! chmod 0700 "$generation_candidate" 2>/dev/null ||
        ! chmod 0600 "$generation_candidate/privkey.pem" 2>/dev/null ||
        ! chmod 0644 "$generation_candidate/fullchain.pem" 2>/dev/null ||
        ! trusted_generation_directory "$generation_candidate" ||
        [[ -L "$generation_candidate/fullchain.pem" || -L "$generation_candidate/privkey.pem" ||
          "$(stat -Lc '%u:%g:%a' "$generation_candidate/fullchain.pem" 2>/dev/null)" != '0:0:644' ]] ||
        ! validate_source_pair "$generation_candidate"; then
      err 'tls-provision: certificate staging failed; delivery not complete'
      return 1
    fi
    if [[ "$generation_mode" == import ]] &&
        ! validate_import_pair "$generation_candidate/fullchain.pem" "$generation_candidate/privkey.pem" "$generation_host"; then
      return 1
    fi
    if ! atomic_generation_rename "$generation_root" "${generation_candidate##*/}" "$generation_host" "$generation_candidate_id" "$generation_previous_id"; then
      err 'tls-provision: atomic certificate activation unavailable or failed; delivery not complete'
      return 1
    fi
    if [[ "$generation_previous_id" != absent ]] && ! chmod 0700 "$generation_candidate" 2>/dev/null; then
      err 'tls-provision: previous certificate retention failed; delivery not complete'
      return 1
    fi
    complete_delivery "$generation_dir" || return 1
    generation_committed=true
    if [[ "$generation_mode" == local ]]; then
      log "✓ self-signed cert written to ${generation_dir} (SHA-256: $(fingerprint "$generation_dir/fullchain.pem"))"
      warn '⚠ self-signed: NO MITM protection on untrusted networks — trusted LAN/homelab only.'
      warn '  For any internet-reachable host use: provision-cert.sh letsencrypt --domain <d> --email <e>'
    else
      log "✓ imported cert installed to ${generation_dir} (SHA-256: $(fingerprint "$generation_dir/fullchain.pem"))"
    fi
    delivery_complete
  }
  local generation_status=0
  build_and_activate_source_generation || generation_status=$?
  finish_source_generation "$generation_status" || generation_status=$?
  trap - INT TERM
  if ! { exec {generation_lock_fd}>&-; } 2>/dev/null; then
    err 'tls-provision: certificate transaction lock release failed; delivery not complete'
    return 1
  fi
  return "$generation_status"
}

# mode_local --host <h> [--ip <a>] [--days <n>] — generate a self-signed leaf
# (LAN/homelab convenience; NO MITM protection on untrusted networks).
mode_local() {
  local host="" ip="" days=825
  while (( $# )); do case "$1" in
    --host) host="$2"; shift 2 ;;
    --ip)   ip="$2";   shift 2 ;;
    --days) days="$2"; shift 2 ;;
    *) err "local: unknown flag $1"; return 2 ;;
  esac; done
  [[ -n "$host" ]] || { err "local: --host required"; return 2; }
  validate_host "$host" || return 2
  [[ -n "$ip" ]] && { validate_ip "$ip" || return 2; }
  { [[ "$days" =~ ^[0-9]+$ ]] && (( days >= 1 )); } || { err "local: --days must be a positive integer (>= 1), got: $days"; return 2; }
  require_public_certificate_authority || return 1
  require_copy_hook || return 1

  local sans; sans="$(derive_sans "$host" "$ip")"
  replace_source_generation local "$host" "$days" "$sans"
}
# mode_letsencrypt --domain <d> --email <e> — behavior-preserving extraction of
# the inline certbot flow from provision-production.sh. --reuse-key is
# LOAD-BEARING (SPKI-pin stability, #658). DO NOT remove. certonly --webroot
# (#881) avoids the in-place nginx ssl_certificate auto-edit. Suppress directory
# hooks for this command only: complete_delivery below performs the required
# copy/restart/reload once, including when Certbot reuses an existing lineage.
mode_letsencrypt() (
  local domain="" email="" managed_upload=false
  while (( $# )); do case "$1" in
    --domain) domain="$2"; shift 2 ;;
    --email)  email="$2";  shift 2 ;;
    --managed-cloudflare-upload) managed_upload=true; shift ;;
    *) err "letsencrypt: unknown flag $1"; return 2 ;;
  esac; done
  [[ -n "$domain" && -n "$email" ]] || { err "letsencrypt: --domain and --email required"; return 2; }
  validate_host "$domain" || return 2

  # Only this mode's parsed flag authorizes retained managed renewal authority.
  if [[ "$managed_upload" == false ]]; then
    require_public_certificate_authority || return 1
  fi

  require_copy_hook || return 1
  # Keep the prior executable on the hook filesystem until this entire
  # issuance/delivery attempt succeeds. Atomic installation still fails before
  # Certbot, while every later refusal restores the prior lineage binding.
  local renewal_target="${HOOK_DIR}/copy-certs.sh" renewal_backup
  mkdir -p "$HOOK_DIR" || return 1
  renewal_backup="$(mktemp -d "${HOOK_DIR}/.copy-certs.previous.XXXXXX")" || return 1
  if [[ -e "$renewal_target" || -L "$renewal_target" ]]; then
    if [[ ! -f "$renewal_target" || -L "$renewal_target" ]] ||
        ! python3 -c 'import os, sys; os.link(sys.argv[1], sys.argv[2], follow_symlinks=False)' \
          "$renewal_target" "${renewal_backup}/previous"; then
      rm -rf -- "$renewal_backup"
      err 'tls-provision: renewal hook backup failed; issuance not attempted'
      return 1
    fi
  fi
  finish_renewal_hook_transaction() {
    local status="$?"
    trap - EXIT
    trap '' INT TERM
    if (( status != 0 )); then
      if [[ -f "${renewal_backup}/previous" ]]; then
        if ! python3 -c 'import os, sys; os.replace(sys.argv[1], sys.argv[2])' \
            "${renewal_backup}/previous" "$renewal_target"; then
          err 'tls-provision: renewal hook restoration failed; private backup retained'
          exit 1
        fi
      elif ! rm -f -- "$renewal_target"; then
        err 'tls-provision: renewal hook restoration failed; delivery not complete'
        exit 1
      fi
    fi
    rm -rf -- "$renewal_backup"
    exit "$status"
  }
  trap finish_renewal_hook_transaction EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  install_required_renewal_hook "$domain" || return 1

  if certbot certonly \
      --no-directory-hooks \
      --webroot -w /var/www/html \
      -d "$domain" -d "api.${domain}" -d "media.${domain}" -d "turn.${domain}" \
      --email "$email" --agree-tos --non-interactive --reuse-key >/dev/null 2>&1; then
    log 'TLS certificate obtained.'
  else
    err 'tls-provision: certificate issuance failed; delivery not complete'
    return 1
  fi

  # Only the explicit managed caller authorizes this optional upload hook.
  if [[ "$managed_upload" == true && -f "${SCRIPT_DIR}/upload-cert-to-cloudflare.sh" ]]; then
    install -m 0755 "${SCRIPT_DIR}/upload-cert-to-cloudflare.sh" "${HOOK_DIR}/upload-cert-to-cloudflare.sh" 2>/dev/null || true
  fi

  # Directory hooks were suppressed for this certonly invocation. Fan out
  # once through the strict path; Certbot success alone cannot prove delivery.
  local dir; dir="$(canonical_dir "$domain")"
  if [[ -f "${dir}/fullchain.pem" && -f "${dir}/privkey.pem" ]]; then
    complete_delivery "$dir" || return 1
    # Explicitly managed CF upload (SaaS SPKI-pin, #658). Preserve the managed
    # flow's non-fatal upload observability without granting public authority.
    if [[ "$managed_upload" == true && -f "${HOOK_DIR}/upload-cert-to-cloudflare.sh" ]]; then
      if RENEWED_LINEAGE="$dir" bash "${HOOK_DIR}/upload-cert-to-cloudflare.sh"; then
        log "✓ CloudFlare custom cert uploaded (or managed upload skipped)"
      else
        warn "CloudFlare upload failed. After creating /etc/concord/cloudflare.env run:"
        warn "  sudo RENEWED_LINEAGE=${dir} bash ${HOOK_DIR}/upload-cert-to-cloudflare.sh"
      fi
    fi
    delivery_complete
  else
    err 'tls-provision: certificate source incomplete; delivery not complete'
    return 1
  fi
)
# mode_import --cert <p> --key <p> --host <h> — validate a BYO PEM pair, then
# install. Fail-closed: ALL checks pass before any file is written (no partial
# install). OpenSSL key/expiry checks and Python native SSL SAN decoding; no
# custom ASN.1 parsing.
mode_import() {
  local cert="" key="" host=""
  while (( $# )); do case "$1" in
    --cert) cert="$2"; shift 2 ;;
    --key)  key="$2";  shift 2 ;;
    --host) host="$2"; shift 2 ;;
    *) err "import: unknown flag $1"; return 2 ;;
  esac; done
  [[ -n "$cert" && -n "$key" && -n "$host" ]] || { err "import: --cert, --key, --host all required"; return 2; }
  validate_host "$host" || return 2
  require_public_certificate_authority || return 1
  [[ -f "$cert" && -f "$key" ]] || { err "import: cert/key file not found"; return 1; }
  validate_import_pair "$cert" "$key" "$host" || return 1
  require_copy_hook || return 1
  replace_source_generation import "$host" "$cert" "$key"
}

# Validate both the submitted inputs and the private staged bytes. Rechecking
# SANs after copying prevents a changing input from publishing a different pair.
validate_import_pair() {
  local cert="$1" key="$2" host="$3"

  # (a) key/cert SPKI match — algorithm-agnostic (RSA + EC). openssl only.
  # Extract the SPKI PEM in a SEPARATE step and assert it is non-empty BEFORE
  # hashing — so a silent openssl failure on either side cannot make both
  # produce sha256("") (= e3b0c442…, a non-empty *string* that would otherwise
  # satisfy `==`). This does not rely on `set -o pipefail` to stay fail-closed.
  local cspki kspki cpub kpub
  cspki="$(openssl x509 -in "$cert" -noout -pubkey 2>/dev/null)" || true
  kspki="$(openssl pkey -in "$key" -pubout 2>/dev/null)" || true
  [[ -n "$cspki" ]] || { err "import: cannot read cert public key"; return 1; }
  [[ -n "$kspki" ]] || { err "import: cannot read private key"; return 1; }
  cpub="$(printf '%s' "$cspki" | openssl sha256 2>/dev/null)"
  kpub="$(printf '%s' "$kspki" | openssl sha256 2>/dev/null)"
  [[ -n "$cpub" && "$cpub" == "$kpub" ]] || { err "import: key does not match cert (SPKI mismatch)"; return 1; }

  # (b) not expired (and not expiring within 0s).
  openssl x509 -in "$cert" -noout -checkend 0 >/dev/null 2>&1 || { err "import: certificate is expired"; return 1; }

  # (c) SAN covers every nginx/coturn consumer — literal token comparison.
  # --host is the deployment base, not one service's leaf hostname. Avoids two
  # bypasses: (1) interpolating $host into an ERE makes `.` match any char
  # (DNS:exampleXcom would satisfy example.com); (2) deriving `*.${host#*.}`
  # yields an over-broad `*.com` for an apex host, accepting a *.tld cert. A
  # `*.parent` wildcard covers host only when host is a SINGLE-label child of
  # parent AND parent is itself a domain (has a dot) — so *.com / *.lan never
  # cover a bare apex, and a wildcard matches exactly one label.
  local dns_names required entry parent label covered
  # Printed GeneralNames are ambiguous when URI values contain commas. The
  # native decoder preserves actual DNS types and ASN.1 lengths, including NUL.
  # Its private CPython entry point is feature-checked; unsupported runtimes
  # refuse. Decode metadata only, without hostname, CN or trust-chain inference.
  if ! dns_names="$(python3 -c '
import sys
try:
    import ssl
    decode = getattr(getattr(ssl, "_ssl", None), "_test_decode_cert", None)
    if not callable(decode):
        raise ValueError()
    certificate = decode(sys.argv[1])
    if not isinstance(certificate, dict):
        raise ValueError()
    names = certificate.get("subjectAltName", ())
    if not isinstance(names, tuple):
        raise ValueError()
    dns_names = []
    for name in names:
        if not isinstance(name, tuple) or len(name) != 2:
            raise ValueError()
        kind, value = name
        if kind != "DNS":
            continue
        if not isinstance(value, str) or any(c in value for c in "\x00\r\n"):
            raise ValueError()
        dns_names.append(value)
    # Validate all names before anything enters the Bash line channel.
    sys.stdout.write("\n".join(dns_names))
except Exception:
    sys.exit(1)
' "$cert" 2>/dev/null)"; then
    err 'import: cannot read certificate DNS SANs'
    return 1
  fi
  for required in "$host" "api.$host" "media.$host" "turn.$host"; do
    covered=0
    while IFS= read -r entry; do
      [[ -z "$entry" ]] && continue
      if [[ "$entry" == "$required" ]]; then covered=1; break; fi
      if [[ "$entry" == '*.'* ]]; then
        parent="${entry#\*.}"
        label="${required%."$parent"}"
        if [[ "$parent" == *.* && "$required" == *."$parent" && -n "$label" && "$label" != "$required" && "$label" != *.* ]]; then
          covered=1; break
        fi
      fi
    done <<<"$dns_names"
    [[ "$covered" -eq 1 ]] || { err "import: certificate SAN does not cover ${required}"; return 1; }
  done
  certificate_valid_now "$cert" || { err 'import: certificate validity unavailable or not yet valid'; return 1; }
}

main() {
  case "${1:-}" in
    --help|-h) usage; return 0 ;;
    "")        usage >&2; return 2 ;;
    local|letsencrypt|import)
      local mode="$1"; shift
      require_root || return 1
      case "$mode" in
        local) mode_local "$@" ;;
        letsencrypt) mode_letsencrypt "$@" ;;
        import) mode_import "$@" ;;
      esac
      ;;
    *)            err "unknown mode: $1"; usage >&2; return 2 ;;
  esac
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
