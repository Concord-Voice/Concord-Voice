# Database Migrations

This directory contains versioned database migrations for the Concord Control Plane service using [golang-migrate](https://github.com/golang-migrate/migrate).

## Migration File Naming

Migrations are named with the following pattern:
```
{version}_{description}.{direction}.sql
```

Examples:
- `000001_create_users.up.sql` - Creates users table
- `000001_create_users.down.sql` - Drops users table (rollback)
- `000002_create_auth_tables.up.sql` - Creates auth-related tables
- `000002_create_auth_tables.down.sql` - Drops auth tables (rollback)

## Creating New Migrations

### Using Make (Recommended)

```bash
cd services/control-plane
make migrate-create NAME=add_user_status
```

This will create two files:
- `migrations/000004_add_user_status.up.sql` - Forward migration
- `migrations/000004_add_user_status.down.sql` - Rollback migration

### Manual Creation

You can also manually create migration files following the naming pattern above.

## Running Migrations

### Apply All Pending Migrations

```bash
make migrate-up
```

This is automatically run when the control-plane server starts.

### Rollback Last Migration

```bash
make migrate-down
```

### Check Current Version

```bash
make migrate-version
```

## Migration Best Practices

1. **Always create both UP and DOWN migrations** - Every migration must be reversible
2. **Test rollbacks** - Ensure your down migration properly reverses the up migration
3. **One logical change per migration** - Don't mix unrelated schema changes
4. **Use transactions implicitly** - Each migration file runs in its own transaction
5. **Avoid data migrations in schema migrations** - Consider separate data migration scripts
6. **Never modify committed migrations** - Create new migrations to fix issues

## Migration Structure

### Up Migration (`*.up.sql`)
```sql
-- Migration: add_user_status (up)
-- Purpose: Add status field to users table

ALTER TABLE users ADD COLUMN status VARCHAR(20) DEFAULT 'active';
CREATE INDEX idx_users_status ON users(status);
```

### Down Migration (`*.down.sql`)
```sql
-- Migration: add_user_status (down)
-- Purpose: Remove status field from users table

DROP INDEX IF EXISTS idx_users_status;
ALTER TABLE users DROP COLUMN IF EXISTS status;
```

## Existing Migrations (000001–000109)

### Phase 1A — Authentication & E2EE
| # | Name | Tables/Changes |
|---|------|---------------|
| 000001 | create_users | `users`, `user_keys` |
| 000002 | create_auth_tables | `refresh_tokens` |
| 000004 | public_keys | `public_keys` |
| 000011 | session_revoked_at | Adds `revoked_at` to sessions |
| 000012 | remember_me | Remember-me persistent sessions |

### Phase 1B — Channels & Text Chat
| # | Name | Tables/Changes |
|---|------|---------------|
| 000003 | create_servers_and_channels | `servers`, `channels`, `server_members` |
| 000005 | server_icon_url | Adds `icon_url` to servers |
| 000006 | messages_table | `messages` |
| 000007 | user_profile_fields | Profile fields (display_name, bio, avatar, links) |
| 000008 | channel_fields | Channel emoji, description, position |
| 000009 | server_invites | `server_invites` |
| 000010 | channel_read_states | `channel_read_states` |
| 000013 | channel_encryption | E2EE channel enforcement |
| 000014 | channel_keys | `channel_keys`, `pending_key_requests` |
| 000015 | fix_invite_timestamps | Timestamp corrections |
| 000016 | user_preferences | `user_preferences` |
| 000017 | fix_timestamps | Additional timestamp fixes |
| 000018 | message_composite_index | Performance index on messages |
| 000019 | header_image_url | Header/banner images |

### Phase 1C — Voice, Media & Desktop Hardening
| # | Name | Tables/Changes |
|---|------|---------------|
| 000020 | voice_participants | `voice_participants` |
| 000021 | channel_voice_settings | Voice channel settings |
| 000022 | channel_groups | Channel groups/categories |
| 000023 | voice_text_channels | Voice text chat support |
| 000024 | user_color_scheme | Per-user color schemes |
| 000025 | revocation_mode | Session revocation modes |
| 000026 | dm_system | `dm_conversations`, `dm_participants`, `dm_messages`, `dm_read_states`, `dm_channel_keys`, `dm_pending_key_requests`, `dm_voice_participants` |
| 000027 | friend_codes_and_privacy | `friendships`, `friend_codes`, `privacy_settings` |
| 000028 | machine_id_and_key_version | Machine ID tracking, key versioning |

### Phase 2A — Foundations & Security
| # | Name | Tables/Changes |
|---|------|---------------|
| 000029 | mfa | MFA tables (TOTP, WebAuthn) |
| 000030 | mfa_recovery_columns | MFA recovery code columns |
| 000031 | backup_email | Backup email for account recovery |
| 000032 | dm_privacy_level | DM privacy level settings |
| 000033 | mfa_enabled_at | Timestamp for MFA enablement |
| 000034 | key_derivation_alg | Key derivation algorithm tracking |
| 000035 | rbac_system | RBAC roles, permissions, overrides |
| 000036 | category_overrides | Channel category permission overrides |
| 000037 | server_banner_url | Server banner/header image URL |
| 000038 | allow_embedded_content | Embedded content preference |
| 000039 | embed_suppression | Per-message embed suppression |
| 000040 | email_verification | Email verification tokens and state |
| 000041 | dm_key_revocations | DM key epoch revocation tracking |
| 000042 | create_media_files | `media_files` table (object storage) |
| 000043 | recovery_keys | Account recovery keys |
| 000044 | trusted_devices | Trusted device tracking for MFA |
| 000045 | recovery_circles | Social recovery circles |
| 000046 | username_change_tracking | `username_changed_at` column on users, `username_history` table |
| 000047 | ownership_transfer | Server ownership transfer records |

### Phase 2B — Chat Enhancements & GIF Integration

| # | Name | Tables/Changes |
| --- | --- | --- |
| 000048 | message_reactions | `message_reactions` table |
| 000049 | message_replies | Reply threading on messages |
| 000050 | message_pinning | `pinned_messages` table |
| 000051 | message_attachments | `message_attachments` table |
| 000052 | attachment_position_unique | Unique constraint on attachment position |
| 000053 | group_dm_admin_roles | Admin roles for group DMs |
| 000054 | server_mute_deafen | Server-level mute/deafen state |
| 000055 | klipy_gif_integration | Klipy GIF integration tables |
| 000056 | klipy_privacy_settings | Klipy privacy preference settings |
| 000057 | dm_message_pinning | DM message pinning support |

### Phase 2B (cont.) — Registration, Privacy, DM, Attestation & Entitlements

| # | Name | Tables/Changes |
| --- | --- | --- |
| 000058 | pending_registrations | `pending_registrations` (email-verification holding) |
| 000059 | account_deletions_and_cascade_fix | `account_deletions` + refresh_tokens ON DELETE CASCADE fix |
| 000060 | drop_sentry_delete_attempted_column | Drop `sentry_delete_attempted` column |
| 000061 | user_sso_identities | `user_sso_identities` + users SSO columns |
| 000062 | remove_is_encrypted | Drop `is_encrypted` (E2EE-everywhere, #201) |
| 000063 | create_notification_preferences | `notification_preferences` |
| 000064 | dm_messages_call_event_kind | `dm_messages.kind` (call-event) |
| 000065 | dm_messages_call_event_use_type_column | `dm_messages.type` call-event column |
| 000066 | client_attestation_registry | Client attestation registry |
| 000067 | temp_sbac_overrides | Temporary SBAC channel_permission_overrides |
| 000068 | drop_servers_e2ee_default | Drop per-server `e2ee_default` opt-out (E2EE-everywhere residual, #1655) |
| 000069 | age_verification | Age-verification signed-claim records (ADR-0025, #1653) |
| 000070 | subscriptions | `subscriptions` (one active per user) — entitlements foundation #1295 |
| 000071 | redemption_codes | `redemption_codes` registry (hashed, single-use/promo) #1295 |
| 000072 | code_redemptions | `code_redemptions` ledger (UNIQUE(code_id,user_id)) #1295 |
| 000073 | drop_users_e2ee_preference | Drop per-user `e2ee_preference` from users + pending_registrations (E2EE-everywhere residual, #1648) |
| 000074 | user_presence_settings | `user_presence_settings` — per-user presence + custom-text-status preferences (#1233) |
| 000075 | create_friend_organization | `friend_organization` — per-user zero-knowledge AES-256-GCM friend-category blob; server stores ciphertext + version only (#324) |
| 000076 | redemption_code_issuance | `redemption_code_issuance` — platform audit trail for redemption-code generation (CLI + admin HTTP), one row per issue/batch in-txn with minted codes (#1303) |
| 000077 | admin_auth | `admin_users` / `admin_webauthn_credentials` / `admin_audit_log` (append-only via `concord_admin_rt` role) — platform-admin auth for the Admin/Ops console #1688 |
| 000078 | default_load_gifs_automatically_true | Default GIF autoload preference to true for new users (#1766) |
| 000079 | username_case_normalization | Lowercase usernames + unique `LOWER(username)` index (#1931) |
| 000080 | add_member_timeout | Member timeout moderation state (#549) |
| 000081 | dm_message_reactions | `dm_message_reactions` table (#1713) |
| 000082 | channel_group_same_server | Composite `(group_id, server_id)` FK enforcing a channel's category is on the SAME server (CV-CAN-010/011/012) |
| 000083 | subscription_expired_status | Adds `'expired'` to the `subscriptions.status` enum for the expiry sweeper's terminal/idempotency marker (#2158) |
| 000084 | user_presence_overrides | Encrypted `custom_text` preference plus separately materialized recipient exclusions (#1234) |
| 000085 | serialize_message_edits_with_key_revocations | Serializes channel/DM ciphertext edits with key-revocation ledger inserts on the stable parent row (#1741) |
| 000086 | ops_metrics | Aggregate-only operations samples and hourly rollups (#1689) |
| 000087 | presence_history | Opt-in activity intervals, consent settings, and durable presence-write reconciliation markers (#1235) |
| 000088 | ops_metrics_reader | Restricted admin metrics reader role (#1690) |
| 000089 | add_presence_category_settings | Master, Server Voice, and Private Call persisted privacy controls (#2229) |
| 000090 | message_purge | Privacy-safe purge audit, receiver-hide ranges, and confirmed attachment reap markers (#1352) |
| 000091 | account_activity_metrics | Private latest-qualified activity marker plus aggregate account/upload metric keys (#2291) |
| 000092 | mfa_totp_key_version | Stamp MFA TOTP seeds with the encryption-key version used for keyring rotation (#2307) |
| 000093 | voice_lifecycle_watermarks | Add nullable lifecycle-event watermarks and defaults while preserving old-replica writes (#2231) |
| 000094 | backfill_voice_lifecycle_watermarks | Backfill lifecycle watermarks from stable `joined_at` without taking `ACCESS EXCLUSIVE` (#2231) |
| 000095 | guard_voice_lifecycle_watermarks_not_null | Add unvalidated non-null guards after the backfill (#2231) |
| 000096 | validate_voice_lifecycle_watermarks_not_null | Validate lifecycle guards without blocking ordinary writes (#2231) |
| 000097 | enforce_voice_lifecycle_watermarks_not_null | Revalidate rollback proofs, then convert lifecycle guards to metadata-native `NOT NULL` constraints (#2231) |
| 000098 | add_activity_settings_cleanup_marker | Durable before/after policy evidence and completed-suppression receipts for retrying/finalizing Rich Presence settings cleanup (#2231) |
| 000099 | user_credential_epoch | Durable per-user credential-epoch fence for destructive access-token revocation (#2201) |
| 000100 | add_refresh_token_predecessor | Exact predecessor-to-successor lineage for grace-period refresh recovery (#2428) |
| 000101 | add_presence_offline_fences | Durable Rich Presence emission-denial fences after disconnects |
| 000102 | add_channel_initial_key_distributions | Creator-only fence for incomplete channel-key distributions (#2503) |
| 000103 | bind_rotation_distributor | Durable first-distributor claim for every batched successor-epoch key distribution (#2503) |
| 000104 | validate_rotation_distributor_fk | Validate the distributor foreign key after the schema-change transaction (#2503) |
| 000105 | enforce_rotation_distributor_writer | Block old handlers from bypassing a successor epoch's distributor claim (#2503) |
| 000106 | bind_rotation_key_fingerprint | Pin the client-asserted CSK fingerprint across rotation batches, fence old-replica initial writers, and default future omitted claims to recoverable unclaimed state (#2503) |
| 000107 | recover_unwritten_rotation_claims | Backfill the recovery default for databases that applied 000106 before that default moved there (#2503) |
| 000108 | reject_unissued_rotation_epochs | Reject old-replica wrapped-key writes for successor epochs without a recorded revocation (#2503) |
| 000109 | reject_revoked_distribution_epochs | Reject new wrapped-key writes for channel epochs that have already been revoked (#2534) |

## Troubleshooting

### Dirty Migration State

If a migration fails mid-execution, the database may be in a "dirty" state:

```bash
# Check if dirty
make migrate-version
# To fix, manually correct the database and force version
# OR rollback to previous version and reapply
```

### Migration Conflicts

If working in a team and migrations conflict:
1. Pull latest changes
2. Rename your migration to have a higher version number
3. Update any references to the migration

## Environment Variables

Migrations use the `DATABASE_URL` environment variable:

```bash
# .env file
DATABASE_URL=postgres://concord:concord_dev_password@localhost:5432/concord?sslmode=disable
```

## CI/CD Integration

In production, migrations should run automatically before deploying new code:

```yaml
# .github/workflows/deploy.yml
- name: Run migrations
  run: |
    cd services/control-plane
    make migrate-up
```
