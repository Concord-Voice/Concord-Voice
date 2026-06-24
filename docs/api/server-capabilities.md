# Server Capabilities Discovery — `GET /api/v1/server/capabilities`

A **public, pre-auth** endpoint that advertises what the server supports. Clients
fetch it before/at login to render the correct auth form and to clamp their
feature surface to what the server advertises (important when an auto-updated
desktop client connects to an older self-hosted server).

- **Auth:** none required. The response is identical with or without an
  `Authorization` header — it carries no user-specific data.
- **Caching:** `Cache-Control: public, max-age=300` (5-minute TTL; a config flag
  flip propagates within the TTL).
- **Rate limit:** 30 requests/minute/IP (matches the sibling `/client/config`).
  The descriptor is constant and auth-state-independent, so there is nothing to
  enumerate; the limit is abuse/DoS throttling. It is deliberately not tighter
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
| `features.entitlementMode` | string | `"saas"` or `"self-hosted-unlocked"` (derived from `instanceType`). |
| `policyVersion` | string | Bumped when the server policy set changes. |

## Additive-evolution contract

The schema is **versioned by addition**: new fields are optional; **old clients
ignore unknown fields, new clients tolerate missing fields**. Clients MUST validate
at the boundary (zod per `[internal]rules/frontend.md`) and degrade gracefully rather
than erroring on an unexpected shape. This is the single handshake that the
self-hosted epic's SSO-suppression (#1619) and entitlement-unlock (#1620) children
ride — they read `auth.oauthProviders` and `features.entitlementMode` respectively,
rather than adding their own round-trips.

## Example — SaaS instance

```http
GET /api/v1/server/capabilities
```

```json
{
  "server": { "name": "Concord Voice", "version": "0.2.0-Beta", "instanceType": "saas" },
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
    "entitlementMode": "saas"
  },
  "policyVersion": "2026-06-01"
}
```

## Example — self-hosted instance (no SMTP, no SSO)

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
    "entitlementMode": "self-hosted-unlocked"
  },
  "policyVersion": "2026-06-01"
}
```

## Configuration

| Env var | Default | Meaning |
|---|---|---|
| `INSTANCE_TYPE` | `saas` | `saas` or `self-hosted`. The self-hosted deploy sets `self-hosted`; unknown values normalize to `saas` at the handler. |
| `SERVER_VERSION` | `dev` | Advertised server version. The SaaS deploy pipeline sets the release tag. |

Both are non-secret, safe-defaulted, and provisioned through `provision-secrets.yml`
(GitHub Actions `vars.*`). All other fields derive from existing server config
(SMTP, SSO, WebAuthn presence).
