# Licensing Authority Service

**Status:** 📋 PLANNED (Phase 3 - Self-Hosted Deployments)

Cryptographic license generation and validation service for self-hosted Concord Voice deployments.

## Overview

The licensing authority is responsible for:
- Generating cryptographically signed licenses for self-hosted instances
- Validating license check-ins from self-hosted servers
- Managing license revocation (emergency "break glass" capability)
- Tracking license usage and compliance

## Architecture

### License Structure

Licenses are JWT tokens signed with Ed25519 (or RSA) containing:
- License ID (UUID)
- Customer/organization info
- Features enabled
- Expiration date (if applicable)
- Version access permissions
- Support tier

### Validation Flow

```
Self-Hosted Instance Boots
    ↓
Validates license signature locally
    ↓
Periodically checks in (every 24-72h)
    ↓
Licensing Authority validates & responds
    ↓
If revoked → Instance enters restricted mode
```

## Security Model

**Important Design Decision:**
Based on the architectural review, the "break glass" remote kill switch has been **softened** to address trust concerns:

### Recommended Approach: Offline-Capable Validation

- License includes time-limited signed tokens
- Server caches last N tokens (90-day buffer)
- Can run 90+ days without phone-home
- Renewal is check-in, not hard validation
- No remote instant shutdown
- Security patches always available regardless of license status

This maintains compliance enforcement while respecting user sovereignty.

## Database Schema

### Licenses
- id (UUID, PK)
- customer_id (UUID, FK)
- license_key (text, unique)
- features (JSONB)
- tier (text)
- expires_at (timestamp, nullable for perpetual)
- revoked (boolean)
- created_at, updated_at

### License Check-ins
- id (UUID, PK)
- license_id (FK)
- instance_id (UUID)
- version (text)
- checked_in_at (timestamp)
- ip_address (inet)

### Customers
- id (UUID, PK)
- name (text)
- email (text)
- created_at, updated_at

## API Endpoints

### Generate License (Internal Admin)
```
POST /api/v1/licenses
Authorization: Admin-Key

{
  "customer_id": "uuid",
  "tier": "enterprise",
  "features": ["high-bitrate", "federation", "api-access"],
  "expires_at": "2025-12-31T23:59:59Z"
}

Response: { "license_key": "signed-jwt-token" }
```

### Validate License (Public)
```
POST /api/v1/licenses/validate

{
  "license_key": "signed-jwt-token",
  "instance_id": "uuid",
  "version": "0.1.0"
}

Response: {
  "valid": true,
  "features": [...],
  "expires_at": "...",
  "revoked": false
}
```

### Check-in (Public)
```
POST /api/v1/licenses/checkin

{
  "license_key": "signed-jwt-token",
  "instance_id": "uuid",
  "version": "0.1.0"
}

Response: {
  "status": "ok",
  "next_checkin": "2024-03-15T00:00:00Z"
}
```

### Revoke License (Admin)
```
POST /api/v1/licenses/:id/revoke
Authorization: Admin-Key

Response: { "success": true }
```

## Key Management

**Critical:** Private keys must be secured!

- Use HSM (Hardware Security Module) in production
- Or AWS KMS / Google Cloud KMS
- Never commit private keys to version control
- Rotate keys annually
- Use Ed25519 for smaller signatures

### Key Generation

```bash
# Generate Ed25519 key pair
openssl genpkey -algorithm ed25519 -out private_key.pem
openssl pkey -in private_key.pem -pubout -out public_key.pem
```

## Development

### Prerequisites

- Go 1.26.1+
- PostgreSQL 16+
- Private/public key pair (Ed25519 or RSA)

### Setup

```bash
# Install dependencies
go mod download

# Generate keys for development
mkdir -p keys
openssl genpkey -algorithm ed25519 -out keys/private_key.pem
openssl pkey -in keys/private_key.pem -pubout -out keys/public_key.pem

# Set environment variables
export PRIVATE_KEY_PATH=./keys/private_key.pem
export DATABASE_URL=postgres://concord:password@localhost:5432/concord

# Run the service
go run cmd/server/main.go
```

## Deployment Considerations

1. **High Availability**
   - Run multiple instances behind load balancer
   - Database replication
   - Key backup and recovery plan

2. **Rate Limiting**
   - Prevent brute force validation attempts
   - Limit check-ins per license

3. **Monitoring**
   - Track check-in frequency
   - Alert on unusual patterns
   - Monitor revocation requests

4. **Compliance**
   - Log all license generations and revocations
   - Audit trail for legal purposes
   - Data retention policy

## Future Enhancements

- [ ] Implement main.go entry point
- [ ] License generation handlers
- [ ] Validation and check-in logic
- [ ] Database models and migrations
- [ ] Crypto utilities for signing/verifying
- [ ] Admin dashboard
- [ ] License analytics
- [ ] Automated expiration warnings
- [ ] Trial license generation
- [ ] License upgrades/downgrades
- [ ] Multi-instance tracking per license
- [ ] Geographic restrictions (optional)
- [ ] Feature flag system
