# Testing Database Migrations

This document provides instructions for testing the database migration system.

## Prerequisites

1. Docker Desktop running
2. PostgreSQL container from docker-compose

## Test Procedure

### 1. Start PostgreSQL

From the project root:

```bash
cd /path/to/Concord
docker-compose up -d postgres
```

Wait for postgres to be healthy:
```bash
docker-compose ps postgres
```

### 2. Set Environment Variables

From the control-plane directory:

```bash
cd services/control-plane

# Create .env file if it doesn't exist
cat > .env << 'EOF'
DATABASE_URL=postgres://concord:concord_dev_password@localhost:5432/concord?sslmode=disable
REDIS_URL=redis://localhost:6379
JWT_SECRET=dev_jwt_secret_change_in_production
PORT=8080
ENVIRONMENT=development
EOF
```

### 3. Test Migration Commands

#### Check Initial Version (should be empty)
```bash
make migrate-version
```

Expected output:
```
Checking migration version...
Building migrate tool...
./bin/migrate -command=version
No migrations applied yet
```

#### Apply All Migrations
```bash
make migrate-up
```

Expected output:
```
Applying migrations...
Building migrate tool...
./bin/migrate -command=up
Migrations applied successfully
```

#### Verify Version
```bash
make migrate-version
```

Expected output:
```
Checking migration version...
Building migrate tool...
./bin/migrate -command=version
Current version: 57 (clean)
```

#### Test Rollback
```bash
make migrate-down
```

Expected output:
```
Rolling back migration...
Building migrate tool...
./bin/migrate -command=down -steps=1
Rolled back 1 migration(s) successfully
```

#### Verify Version After Rollback
```bash
make migrate-version
```

Expected output:
```
Current version: 56 (clean)
```

#### Re-apply Migration
```bash
make migrate-up
```

### 4. Verify Database Schema

Connect to postgres and check tables:

```bash
docker exec -it concord-postgres psql -U concord -d concord
```

Then run:
```sql
-- List all tables
\dt

-- Expected tables (40+ total, including):
-- users, user_keys, public_keys, refresh_tokens, user_preferences
-- servers, channels, server_members, server_invites
-- messages, channel_read_states, channel_keys, pending_key_requests
-- voice_participants, channel_groups
-- friendships, friend_codes, privacy_settings
-- dm_conversations, dm_participants, dm_messages, dm_read_states
-- dm_channel_keys, dm_pending_key_requests, dm_voice_participants
-- mfa_credentials, mfa_recovery_codes, trusted_devices
-- roles, role_permissions, channel_permission_overrides, category_permission_overrides
-- media_files, recovery_keys, recovery_circles
-- email_verifications, dm_key_revocations
-- username_history, ownership_transfers
-- schema_migrations

-- Check schema_migrations table
SELECT * FROM schema_migrations;

-- Expected: version = 57, dirty = false

-- Verify users table structure
\d users

-- Verify foreign key relationships
SELECT
    tc.table_name,
    kcu.column_name,
    ccu.table_name AS foreign_table_name,
    ccu.column_name AS foreign_column_name
FROM information_schema.table_constraints AS tc
JOIN information_schema.key_column_usage AS kcu
  ON tc.constraint_name = kcu.constraint_name
JOIN information_schema.constraint_column_usage AS ccu
  ON ccu.constraint_name = tc.constraint_name
WHERE tc.constraint_type = 'FOREIGN KEY';

-- Exit psql
\q
```

### 5. Test Creating New Migration

```bash
make migrate-create NAME=add_test_field
```

Expected output:
```
Creating migration: add_test_field
Building migrate tool...
./bin/migrate -command=create -name=add_test_field
Created migration files:
  - migrations/000004_add_test_field.up.sql
  - migrations/000004_add_test_field.down.sql
```

Verify files were created:
```bash
ls -l migrations/000004_*
```

### 6. Test Server Startup with Auto-Migration

```bash
make run
```

Expected to see in logs:
```
INFO Starting Control Plane server port=8080 env=development
```

Migrations should run automatically before the server starts.

### 7. Test Complete Rollback

Stop the server (Ctrl+C), then:

```bash
# Rollback all 57 migrations one by one (repeat 57 times)
# Each `make migrate-down` rolls back 1 migration
make migrate-down  # Rolls back to version 46
make migrate-down  # Rolls back to version 45
# ... (repeat until version 0)

# Verify
make migrate-version
```

Expected:
```
No migrations applied yet
```

> **Tip:** For a full reset, use `docker-compose down -v` to destroy the volume instead of rolling back one-by-one.

### 8. Verify Clean State

```bash
docker exec -it concord-postgres psql -U concord -d concord -c "\dt"
```

Should only show `schema_migrations` table (or possibly empty).

### 9. Test Complete Migration Flow

```bash
# Apply all migrations
make migrate-up

# Start server
make run
```

Server should start successfully with all tables created.

## Cleanup

To reset database completely:

```bash
# Stop containers
docker-compose down

# Remove volumes (WARNING: destroys all data)
docker-compose down -v

# Start fresh
docker-compose up -d postgres
```

## Common Issues

### Issue: "could not create migrate instance"

**Cause**: DATABASE_URL not set or incorrect

**Fix**:
```bash
# Verify .env file exists
cat .env | grep DATABASE_URL

# Test connection manually
psql "postgres://concord:concord_dev_password@localhost:5432/concord?sslmode=disable"
```

### Issue: "dirty database version"

**Cause**: Migration failed mid-execution

**Fix**:
```bash
# Connect to database
docker exec -it concord-postgres psql -U concord -d concord

# Check schema_migrations table
SELECT * FROM schema_migrations;

# If dirty = true, manually fix the schema and update:
UPDATE schema_migrations SET dirty = false;
```

### Issue: "migration file not found"

**Cause**: Running from wrong directory

**Fix**:
```bash
# Always run from control-plane directory
cd services/control-plane
make migrate-up
```

## Success Criteria

✅ All migration commands work without errors
✅ Version tracking is accurate
✅ Rollbacks successfully reverse changes
✅ Server starts with auto-migration
✅ All expected tables exist with correct schema
✅ Foreign key constraints are properly enforced
✅ Indexes are created
