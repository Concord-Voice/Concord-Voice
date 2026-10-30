package loginfamily

// Discovery never supplies deletion authority. Each stream uses an immutable
// UUID key and a bounded page; the unit transaction rechecks its entire scope.
var gcCandidateQueries = [...]string{
	`SELECT operation_id FROM login_family_revocations WHERE operation_id > $1 AND operation_id <= $2 ORDER BY operation_id LIMIT $3`,
	`SELECT family_id FROM login_families WHERE auth_operation_id IS NULL AND family_id > $1 AND family_id <= $2 ORDER BY family_id LIMIT $3`,
	`SELECT session_generation FROM voice_auth_room_closures WHERE session_generation > $1 AND session_generation <= $2 ORDER BY session_generation LIMIT $3`,
	`SELECT connection_generation FROM voice_auth_connection_closures WHERE connection_generation > $1 AND connection_generation <= $2 ORDER BY connection_generation LIMIT $3`,
}

const gcFamilyScope = ` FROM login_families f WHERE
 (($1 AND f.auth_operation_id = $2) OR
  (NOT $1 AND f.family_id = $2 AND f.auth_operation_id IS NULL))`

const gcFamilyPage = `SELECT f.family_id, f.user_id` + gcFamilyScope +
	` AND f.family_id > $3 ORDER BY f.family_id LIMIT $4 FOR NO KEY UPDATE NOWAIT`

const gcFamilyEligible = `WITH cohort AS (SELECT f.*` + gcFamilyScope + `)
 SELECT NOT EXISTS (
  SELECT 1 FROM cohort f WHERE
   EXISTS (SELECT 1 FROM login_family_sids s WHERE s.family_id = f.family_id AND
    (s.access_expires_at > clock_timestamp() OR EXISTS
     (SELECT 1 FROM refresh_tokens r WHERE r.id = s.sid OR r.predecessor_id = s.sid)))
   OR EXISTS (SELECT 1 FROM voice_auth_connections c WHERE c.family_id = f.family_id)
   OR EXISTS (SELECT 1 FROM voice_enforcement_sessions r
    WHERE r.user_id = f.user_id AND r.credential_epoch = f.credential_epoch)
   OR EXISTS (SELECT 1 FROM voice_command_outbox c
    WHERE c.user_id = f.user_id AND c.credential_epoch = f.credential_epoch)
   OR EXISTS (SELECT 1 FROM dm_block_voice_ejections d WHERE d.user_id = f.user_id)
   OR EXISTS (SELECT 1 FROM credential_epoch_voice_ejections e WHERE e.generation = f.epoch_generation)
   OR (f.auth_operation_id IS NOT NULL AND NOT EXISTS
    (SELECT 1 FROM login_family_revocations o WHERE o.operation_id = f.auth_operation_id
     AND o.user_id = f.user_id AND o.sealed_at IS NOT NULL AND o.completed_at IS NOT NULL))
 )`

const gcCohortConsistent = `SELECT
 (SELECT count(*) FROM login_families WHERE auth_operation_id = $1) = $3
 AND (SELECT count(*) FROM login_family_revocation_targets WHERE operation_id = $1) = $3
 AND NOT EXISTS (
  SELECT 1 FROM login_family_revocation_targets t LEFT JOIN login_families f USING (family_id)
  WHERE t.operation_id = $1 AND (t.user_id <> $2 OR f.user_id IS DISTINCT FROM t.user_id
   OR f.auth_operation_id IS DISTINCT FROM t.operation_id))`

const gcDeleteSIDs = `DELETE FROM login_family_sids WHERE family_id IN
 (SELECT f.family_id` + gcFamilyScope + `)`
const gcDeleteFamilies = `DELETE FROM login_families WHERE family_id IN
 (SELECT f.family_id` + gcFamilyScope + `)`

// A closure's copied identity can outlive its family. Check both its retained
// family owner and direct principal/epoch delivery locators, never requiring a
// live principal to establish release authority.
const gcClosureEligible = `SELECT
 EXISTS (SELECT 1 FROM voice_auth_boot_fences WHERE node_boot_id = $1 AND retired_at IS NOT NULL)
 AND NOT EXISTS (SELECT 1 FROM voice_auth_connections WHERE node_boot_id = $1 OR connection_generation = $2)
 AND NOT EXISTS (SELECT 1 FROM voice_enforcement_sessions WHERE node_boot_id = $1 OR session_generation = $3)
 AND NOT EXISTS (SELECT 1 FROM voice_auth_room_bindings WHERE connection_generation = $2 OR session_generation = $3)
 AND NOT EXISTS (SELECT 1 FROM login_family_revocations WHERE user_id = $4 AND completed_at IS NULL)
 AND NOT EXISTS (SELECT 1 FROM credential_epoch_voice_ejections WHERE user_id = $4 AND superseded_credential_epoch = $6)
 AND NOT EXISTS (SELECT 1 FROM voice_command_outbox WHERE node_boot_id = $1 OR session_generation = $3)
 AND NOT EXISTS (SELECT 1 FROM dm_block_voice_ejections WHERE user_id = $4)
 AND NOT EXISTS (SELECT 1 FROM login_families f
  LEFT JOIN login_family_revocations o ON o.operation_id = f.auth_operation_id
  WHERE f.family_id = $5 AND ((f.auth_operation_id IS NOT NULL AND o.completed_at IS NULL)
   OR EXISTS (SELECT 1 FROM credential_epoch_voice_ejections e WHERE e.generation = f.epoch_generation)))`
