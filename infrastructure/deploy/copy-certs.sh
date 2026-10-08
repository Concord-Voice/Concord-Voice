#!/usr/bin/env bash
# /etc/letsencrypt/renewal-hooks/deploy/copy-certs.sh
#
# Certbot deploy hook — runs after every successful certificate issuance or
# renewal. Copies the renewed certificate files into service-specific
# directories so each container is only bind-mounted the files it needs (no
# access to the full /etc/letsencrypt tree). Real file copies rather than
# symlinks ensure the files remain readable inside the container even if
# certbot rotates the archive path.
#
# Also restarts coturn (if the container is running) so it picks up the new
# TLS material without waiting for a manual redeploy. coturn does not reload
# certificates on its own.
#
# Directories created:
#   /opt/concord/certs/coturn/   cert.pem + key.pem  (for coturn DTLS/TLS TURN)
#
# Extend with additional service directories here as needed (e.g. custom
# nginx certs, media-plane TLS, etc.).
#
# provision-cert.sh installs a copy bound to the selected lineage at:
#   /etc/letsencrypt/renewal-hooks/deploy/copy-certs.sh
# The bound executable ignores other Certbot lineages before this helper runs.
# Direct source invocation remains the explicit copy-only delivery retry.

set -euo pipefail

# Certbot provides RENEWED_LINEAGE as the canonical path to the renewed cert's
# live directory. Prefer it over deriving from RENEWED_DOMAINS, which is
# brittle when the cert name doesn't match the first domain or when domain
# ordering changes. Fall back to RENEWED_DOMAINS only if RENEWED_LINEAGE is
# unset (e.g. when the script is invoked manually for initial provisioning).
if [[ -n "${RENEWED_LINEAGE:-}" ]]; then
  LIVE="${RENEWED_LINEAGE}"
else
  DOMAIN="${RENEWED_DOMAINS%% *}"   # first domain in the renewed set
  LIVE="/etc/letsencrypt/live/${DOMAIN}"
fi

# Sanity check — cert files don't exist yet.
#
# Legitimate during initial provisioning: provision-production.sh invokes the
# deploy hook manually before certbot has issued the first cert. Exit 0 is
# correct (no error, nothing to do). The stderr line leaves a forensic trail
# so an operator investigating "why did the cert renew but coturn still has
# the old cert?" can grep journalctl and see WHY the hook short-circuited
# (helps catch RENEWED_LINEAGE typos, filesystem-full anomalies, etc.).
if [[ ! -f "${LIVE}/fullchain.pem" || ! -s "${LIVE}/fullchain.pem" ||
      ! -f "${LIVE}/privkey.pem" || ! -s "${LIVE}/privkey.pem" ]]; then
  exit 0
fi

fail() { printf '%s\n' "$1" >&2; exit 1; }

[[ "$EUID" -eq 0 ]] || fail 'copy-certs: root required; delivery not started'
command -v docker >/dev/null 2>&1 ||
  fail 'copy-certs: Docker unavailable; delivery not complete'

CERT="${LIVE}/fullchain.pem"
KEY="${LIVE}/privkey.pem"
if [[ "$(stat -Lc '%u:%g:%a' "$KEY" 2>/dev/null)" != '0:0:600' ]] ||
    ! openssl x509 -in "$CERT" -noout >/dev/null 2>&1 ||
    ! openssl x509 -in "$CERT" -noout -checkend 0 >/dev/null 2>&1 ||
    ! openssl pkey -in "$KEY" -noout >/dev/null 2>&1; then
  fail 'copy-certs: certificate source invalid; delivery not complete'
fi
# checkend covers expiration only. Decode the leaf with native SSL before
# discovery or fan-out so a future-dated pair cannot replace working material.
if ! python3 -c '
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
' "$CERT" >/dev/null 2>&1; then
  fail 'copy-certs: certificate source invalid; delivery not complete'
fi
CERT_PUB="$(openssl x509 -in "$CERT" -noout -pubkey 2>/dev/null)" || CERT_PUB=''
KEY_PUB="$(openssl pkey -in "$KEY" -pubout 2>/dev/null)" || KEY_PUB=''
[[ -n "$CERT_PUB" && "$CERT_PUB" == "$KEY_PUB" ]] ||
  fail 'copy-certs: certificate source invalid; delivery not complete'

# Discover every state for the canonical global container name and coturn
# service, then validate the actual Compose project. The JSON ps projection
# contains summary fields only, never container configuration or environment.
if DISCOVERY="$(docker ps --all \
    --filter 'name=^/?concordvoice-coturn$' \
    --filter 'label=com.docker.compose.service=coturn' \
    --format '{"ID":{{json .ID}},"Names":{{json .Names}},"Labels":{"com.docker.compose.project":{{json (.Label "com.docker.compose.project")}},"com.docker.compose.service":{{json (.Label "com.docker.compose.service")}}}}' 2>/dev/null)"; then
  :
else
  docker_rc=$?
  [[ "$docker_rc" -eq 127 ]] && fail 'copy-certs: Docker unavailable; delivery not complete'
  fail 'copy-certs: coturn discovery failed; delivery not complete'
fi
if ! CANDIDATES="$(python3 -c '
import json, sys
import re

raw = sys.stdin.read()
if not raw.strip():
    sys.exit(0)
try:
    records = [json.loads(line) for line in raw.splitlines() if line.strip()]
except (ValueError, TypeError):
    sys.exit(2)
found = []
for item in records:
    if not isinstance(item, dict):
        sys.exit(2)
    labels = item.get("Labels")
    name = item.get("Names", item.get("Name"))
    ident = item.get("ID", item.get("Id"))
    if not isinstance(name, str) or not isinstance(ident, str) or not isinstance(labels, dict):
        sys.exit(2)
    project = labels.get("com.docker.compose.project")
    if (not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", project or "") or
        labels.get("com.docker.compose.service") != "coturn" or
        not re.fullmatch(r"[0-9a-f]{12,64}", ident)):
        sys.exit(2)
    if name.startswith("/"):
        name = name[1:]
    if name != "concordvoice-coturn":
        sys.exit(2)
    found.append((ident, name, project))
for ident, name, project in found:
    print(f"{ident}\t{name}\t{project}")
 ' <<<"$DISCOVERY" 2>/dev/null)"; then
  fail 'copy-certs: coturn discovery failed; delivery not complete'
fi

CONSUMER_STATE=absent
if [[ -n "$CANDIDATES" ]]; then
  [[ "$CANDIDATES" != *$'\n'* ]] || fail 'copy-certs: coturn identity ambiguous; delivery not complete'
  IFS=$'\t' read -r CONTAINER_ID CONTAINER_NAME CONTAINER_PROJECT <<<"$CANDIDATES"
  [[ "$CONTAINER_NAME" == concordvoice-coturn ]] ||
    fail 'copy-certs: coturn state invalid; delivery not complete'
  INSPECT_TEMPLATE='{{printf "{\"Name\":%q,\"Image\":%q,\"Project\":%q,\"Service\":%q,\"Status\":%q,\"Running\":%t,\"Paused\":%t,\"Restarting\":%t}" .Name .Config.Image (index .Config.Labels "com.docker.compose.project") (index .Config.Labels "com.docker.compose.service") .State.Status .State.Running .State.Paused .State.Restarting}}'
  if INSPECTED="$(docker inspect --format "$INSPECT_TEMPLATE" "$CONTAINER_ID" 2>/dev/null)"; then
    :
  else
    docker_rc=$?
    [[ "$docker_rc" -eq 127 ]] && fail 'copy-certs: Docker unavailable; delivery not complete'
    fail 'copy-certs: coturn discovery failed; delivery not complete'
  fi
  if CONSUMER_STATE="$(python3 -c '
import json, sys
import re

try:
    value = json.load(sys.stdin)
except (ValueError, TypeError):
    sys.exit(2)
if not isinstance(value, dict):
    sys.exit(2)
name, image = value.get("Name"), value.get("Image")
project, service = value.get("Project"), value.get("Service")
expected_project = sys.argv[1]
state = value
if isinstance(name, str) and name.startswith("/"):
    name = name[1:]
if (not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", expected_project) or
    name != "concordvoice-coturn" or image != "coturn/coturn:4.6.3-alpine" or
    project != expected_project or service != "coturn" or not isinstance(state, dict)):
    sys.exit(3)
status = state.get("Status")
running, paused, restarting = state.get("Running"), state.get("Paused"), state.get("Restarting")
if not isinstance(running, bool) or not isinstance(paused, bool) or not isinstance(restarting, bool):
    sys.exit(2)
if status == "running" and running and not paused and not restarting:
    print("running")
elif status in ("created", "exited") and not running and not paused and not restarting:
    print(status)
else:
    sys.exit(3)
' "$CONTAINER_PROJECT" <<<"$INSPECTED" 2>/dev/null)"; then
    :
  else
    parse_rc=$?
    [[ "$parse_rc" -eq 2 ]] && fail 'copy-certs: coturn discovery failed; delivery not complete'
    fail 'copy-certs: coturn state invalid; delivery not complete'
  fi
fi

# ── coturn (DTLS/TLS TURN on port 5349) ──────────────────────────────────────
# Test-only seam; do not set in production.
COTURN_CERT_ROOT="${COTURN_CERT_ROOT:-/opt/concord/certs}"
COTURN_CERT_DIR="${COTURN_CERT_ROOT}/coturn"
if [[ -L "$COTURN_CERT_ROOT" || ( -e "$COTURN_CERT_ROOT" && ! -d "$COTURN_CERT_ROOT" ) ||
      -L "$COTURN_CERT_DIR" || ( -e "$COTURN_CERT_DIR" && ! -d "$COTURN_CERT_DIR" ) ]]; then
  fail 'copy-certs: certificate delivery failed; delivery not complete'
fi
# The pinned coturn image runs as 65534:65533 (nobody:nogroup). Execute-only
# directories let that identity reach known files without allowing enumeration.
if ! install -d -o root -g root -m 0711 "${COTURN_CERT_ROOT}" 2>/dev/null ||
    ! install -d -o root -g root -m 0711 "${COTURN_CERT_DIR}" 2>/dev/null ||
    ! install -o root -g root -m 0444 "$CERT" "${COTURN_CERT_DIR}/cert.pem" 2>/dev/null ||
    ! install -o 65534 -g 65533 -m 0400 "$KEY" "${COTURN_CERT_DIR}/key.pem" 2>/dev/null; then
  fail 'copy-certs: certificate delivery failed; delivery not complete'
fi
if [[ ! -d "$COTURN_CERT_ROOT" || -L "$COTURN_CERT_ROOT" ||
      ! -d "$COTURN_CERT_DIR" || -L "$COTURN_CERT_DIR" ||
      "$(stat -Lc '%u:%g:%a' "$COTURN_CERT_ROOT" 2>/dev/null)" != '0:0:711' ||
      "$(stat -Lc '%u:%g:%a' "$COTURN_CERT_DIR" 2>/dev/null)" != '0:0:711' ||
      ! -f "$COTURN_CERT_DIR/cert.pem" || -L "$COTURN_CERT_DIR/cert.pem" ||
      ! -f "$COTURN_CERT_DIR/key.pem" || -L "$COTURN_CERT_DIR/key.pem" ||
      "$(stat -Lc '%u:%g:%a' "$COTURN_CERT_DIR/cert.pem" 2>/dev/null)" != '0:0:444' ||
      "$(stat -Lc '%u:%g:%a' "$COTURN_CERT_DIR/key.pem" 2>/dev/null)" != '65534:65533:400' ]]; then
  fail 'copy-certs: certificate delivery failed; delivery not complete'
fi
if ! cmp -s "$CERT" "$COTURN_CERT_DIR/cert.pem" ||
    ! cmp -s "$KEY" "$COTURN_CERT_DIR/key.pem"; then
  fail 'copy-certs: certificate delivery failed; delivery not complete'
fi

# ── Restart coturn so it picks up the new certs ──────────────────────────────
# coturn doesn't reload certs on its own. Restart the container if it's
# running; skip if the container isn't up (e.g. during initial provisioning
# before the first compose up). Docker discovery above must succeed.
if [[ "$CONSUMER_STATE" == running ]]; then
  docker restart "$CONTAINER_ID" >/dev/null 2>&1 ||
    fail 'copy-certs: coturn restart failed; delivery not complete'
fi

# ── nginx (host-side reverse proxy) ──────────────────────────────────────────
# Per #881: nginx config references /etc/letsencrypt/live/${DOMAIN}/fullchain.pem
# directly (no copy needed). But nginx caches the cert in memory at startup
# and won't pick up the renewed file until reload. systemctl reload sends
# SIGHUP, which is graceful — existing connections survive, new ones use the
# fresh cert. Skip silently if nginx isn't running (e.g., during initial
# provisioning before nginx is started).
if systemctl is-active --quiet nginx 2>/dev/null; then
  systemctl reload nginx 2>/dev/null ||
    fail 'copy-certs: nginx reload failed; delivery not complete'
fi

printf '%s\n' 'copy-certs: certificate pair delivery complete.'
if [[ "$CONSUMER_STATE" == running ]]; then
  printf '%s\n' 'copy-certs: coturn-stage=restarted'
else
  printf '%s\n' 'copy-certs: coturn-stage=prepared'
fi
