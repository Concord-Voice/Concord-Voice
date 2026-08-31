# Server Capabilities Discovery — `GET /api/v1/server/capabilities`

A **public, pre-auth** endpoint that advertises what the server supports. Clients
fetch it before/at login to render the correct auth form and to clamp their
feature surface to what the server advertises (important when an auto-updated
desktop client connects to an older self-hosted server).

- **Auth:** none required. The response is identical with or without an
  `Authorization` header — it carries no user-specific data.
- **Caching:** `Cache-Control: no-store, no-cache, must-revalidate, max-age=0`;
  `Pragma: no-cache` (capability changes must be visible immediately during
  rollouts).
- **Rate limit:** 30 requests/minute/IP (matches the sibling `/client/config`).
  The descriptor is constant and auth-state-independent, so there is nothing to
  enumerate. The limit is abuse/DoS throttling. It is deliberately not tighter
  than `/client/config` because this is the first pre-auth request and
  self-hosted deployments commonly egress many clients through one NAT IP.
- **Introduced:** #662 (child of epic #1615, self-hosted deployment).

## Response schema

| Field | Type | Notes |
|---|---|---|
| `server.name` | string | Constant `"Concord Voice"`. |
| `server.version` | string | Advertised server version (`SERVER_VERSION`; `"dev"` when unset). Used for client version-skew clamping. |
| `server.instanceType` | string | `"saas"` or `"self-hosted"` (`INSTANCE_TYPE`; unknown/empty normalizes to `"saas"`). |
| `auth.emailVerificationRequired` | boolean | Always `true` — password registration always requires email verification; SMTP only changes delivery (real email vs the dev stdout/Redis code path). |
| `auth.mfaEnabled` | boolean | Always `true` (MFA is structurally available). |
| `auth.mfaMethods` | string[] | `["totp"]`, plus `"webauthn"` when a WebAuthn RP is configured. Always present (never `null`). |
| `auth.oauthProviders` | string[] | Subset of `["google","apple"]` per server SSO config. Empty array (not `null`) suppresses SSO. |
| `auth.ldapEnabled` | boolean | Currently always `false` (no backend yet; additive). |
| `auth.samlEnabled` | boolean | Currently always `false` (no backend yet; additive). |
| `features.voiceTiersSupported` | boolean | `true` on SaaS; `false` on self-hosted (all features unlocked, tiers moot). |
| `features.e2eeEnforcedEverywhere` | boolean | Always `true` (E2EE-everywhere, #201). |
| `features.maxMembersPerServer` | integer | Advisory ceiling. |
| `features.entitlementMode` | string | `"saas"` or `"self-hosted-unlocked"` (derived from `instanceType`). On self-hosted, the control-plane entitlement resolver returns the maximal current entitlement set for every user. |
| `features.activityHistorySupported` | boolean, optional | Emitted as `true` only when the cluster gate is enabled and the replica count was explicitly set to one. Otherwise omitted; the backend does not emit `false`. |
| `features.chunkedAttachmentUpload` | boolean | Whether the chunked upload session routes are reachable on this deployment (#2157). Always emitted — deliberately not `omitempty`, because an explicit `false` is the useful answer when object storage or the session Redis is absent. Mirrors the route-registration condition exactly (`internal/api/router.go:816`). A server that predates the field omits it; the client's zero value is `false`, so it fails closed to the single-shot path. |
| `policyVersion` | string | Bumped when the server policy set changes. |

### Activity History support state

The desktop treats a schema-valid `true` as supported and a schema-valid
missing/`false` value as confirmed unsupported. A fetch failure, non-2xx
response, or malformed payload is an error, not an unsupported result, and
preserves the last confirmed support state. Capability `true` does not prove
that operator disclosure is usable: `GET /users/me/presence-history/settings`
can separately return `available:false`, which the client presents as an
operator/disclosure-unavailable state.

## Additive-evolution contract

The schema is **versioned by addition**: new fields are optional. **Old clients
ignore unknown fields, and new clients tolerate missing *optional* fields**. The
rule covers additive fields only — the table above marks exactly one field optional,
and the rest are always emitted. A client MUST NOT treat a missing
`auth.mfaEnabled` or `features.e2eeEnforcedEverywhere` as a tolerable absence and
degrade its posture; an absent non-optional field is a malformed response. The
one exception is `features.chunkedAttachmentUpload`, which any server predating
#2157 omits: a client reads that absence as `false` and keeps the single-shot
upload path rather than calling the response malformed. Clients MUST validate
at the boundary (zod per `[internal]rules/frontend.md`) and degrade gracefully rather
than erroring on an unexpected shape. This is the single handshake that the
self-hosted epic's SSO-suppression (#1619) and entitlement-unlock (#1620) children
ride — they read `auth.oauthProviders` and `features.entitlementMode` respectively,
rather than adding their own round-trips. `features.entitlementMode = "self-hosted-unlocked"`
means `/api/v1/entitlements`, entitlement JWT claims, `entitlements_changed`,
server entitlements, channel audio ceilings, and media join authorization resolve
the current maximal entitlement set server-side.

## Example — SaaS instance

```http
GET /api/v1/server/capabilities
```

```json
{
  "server": { "name": "Concord Voice", "version": "dev", "instanceType": "saas" },
  "auth": {
    "emailVerificationRequired": true,
    "mfaEnabled": true,
    "mfaMethods": ["totp", "webauthn"],
    "oauthProviders": ["google", "apple"],
    "ldapEnabled": false,
    "samlEnabled": false
  },
  "features": {
    "voiceTiersSupported": true,
    "e2eeEnforcedEverywhere": true,
    "maxMembersPerServer": 500,
    "entitlementMode": "saas",
    "activityHistorySupported": true,
    "chunkedAttachmentUpload": true
  },
  "policyVersion": "2026-06-01"
}
```

## Example — self-hosted instance (no SMTP, no SSO, no object storage)

```json
{
  "server": { "name": "Concord Voice", "version": "1.4.0", "instanceType": "self-hosted" },
  "auth": {
    "emailVerificationRequired": true,
    "mfaEnabled": true,
    "mfaMethods": ["totp"],
    "oauthProviders": [],
    "ldapEnabled": false,
    "samlEnabled": false
  },
  "features": {
    "voiceTiersSupported": false,
    "e2eeEnforcedEverywhere": true,
    "maxMembersPerServer": 500,
    "entitlementMode": "self-hosted-unlocked",
    "chunkedAttachmentUpload": false
  },
  "policyVersion": "2026-06-01"
}
```

## Configuration

| Env var | Default | Meaning |
|---|---|---|
| `INSTANCE_TYPE` | `saas` | `saas` or `self-hosted`. The self-hosted deploy sets `self-hosted`; unknown values normalize to `saas` before capabilities and entitlement resolution. |
| `SERVER_VERSION` | `dev` | Advertised server version. Sourced from the optional `vars.SERVER_VERSION` repository/environment variable and resolved to `dev` when unset (`provision-secrets.yml:499`). The variable is **currently unset, so production advertises `"dev"`** — the deploy pipeline does not stamp the release tag. |
| `ACTIVITY_HISTORY_CLUSTER_ENABLED` | `false` | Activity History rollout gate. Support is advertised only when this is `true`. |
| `CONTROL_PLANE_REPLICA_COUNT` | unset | Must be explicitly set to `1`, together with the enabled cluster gate, before Activity History support is advertised. Independently of that gate (#2178), any explicitly set value other than `1` now fails control-plane startup unconditionally — `validateControlPlaneReplicaCount()` rejects it regardless of environment or feature flags, because the WebSocket hub holds per-connection session state in process memory. Horizontal scaling is tracked in [#2757](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2757). |

These values are non-secret and flow through the deployment configuration.
Other fields derive from existing server config (SMTP, SSO, WebAuthn presence).
Activity History operator disclosure has its own settings-level availability
contract and is not collapsed into this capability bit.
