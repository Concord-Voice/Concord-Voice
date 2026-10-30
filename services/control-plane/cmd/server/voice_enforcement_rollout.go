package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/api"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/database"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/google/uuid"
)

const (
	retireBootCommand = "retire-boot"
	nodeBootIDFlag    = "--node-boot-id"
)

// This is the privileged current-image operator boundary. The lifecycle wrapper
// verifies physical drain, exact configured inventory and current artifacts;
// these SQL predicates cannot attest a stopped host or native worker.
type familyRolloutCounts struct {
	LegacyUsableSIDs          int64 `json:"legacyUsableSIDs"`
	RegistryRows              int64 `json:"registryRows"`
	AuthConnectionRows        int64 `json:"authConnectionRows"`
	AuthRoomBindingRows       int64 `json:"authRoomBindingRows"`
	DMParentRows              int64 `json:"dmParentRows"`
	CredentialParentRows      int64 `json:"credentialParentRows"`
	AuthParentRows            int64 `json:"authParentRows"`
	DMReconciliationRows      int64 `json:"dmReconciliationRows"`
	AuthConnectionClosureRows int64 `json:"authConnectionClosureRows"`
	AuthRoomClosureRows       int64 `json:"authRoomClosureRows"`
	RetiredBootRows           int64 `json:"retiredBootRows"`
}
type familyRolloutSlot struct {
	Slot           string `json:"slot"`
	ArtifactSHA256 string `json:"artifactSHA256"`
	NodeBootID     string `json:"nodeBootId"`
}
type familyActivationReceipt struct {
	Schema                     string              `json:"schema"`
	Version                    int                 `json:"version"`
	ActivationID               string              `json:"activationId"`
	Protocol                   int                 `json:"protocol"`
	ControlPlaneArtifactSHA256 string              `json:"controlPlaneArtifactSHA256"`
	MediaSlots                 []familyRolloutSlot `json:"mediaSlots"`
	familyRolloutCounts
}
type voiceFamilyRolloutStatus struct {
	Schema             string  `json:"schema"`
	Version            int     `json:"version"`
	Protocol           int     `json:"protocol"`
	FamilyActivation   string  `json:"familyActivation"`
	FamilyActivationID *string `json:"familyActivationId"`
	familyRolloutCounts
	FamilyRows              int64  `json:"familyRows"`
	FamilySIDRows           int64  `json:"familySIDRows"`
	OldestUnsettledSeconds  *int64 `json:"oldestUnsettledSeconds"`
	SecurityRoundFresh      bool   `json:"securityRoundFresh"`
	ProductiveGrantEligible bool   `json:"productiveGrantEligible"`
}

var familyRolloutUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var familyRolloutDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var errFamilyRolloutEvidence = errors.New("invalid or incomplete family rollout evidence")

type familyRolloutQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func runVoiceEnforcementRollout(args []string) int {
	if len(args) == 0 || !validFamilyRolloutArgs(args) {
		fmt.Fprintln(os.Stderr, "usage: control-plane voice-enforcement-rollout activate --confirm-drained --receipt-file <path>|deactivate|status [--json]|retire-boot <exact runtime evidence>|verify-retired-boot --node-boot-id UUID")
		return 64
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "load configuration failed")
		return 1
	}
	var retirement voiceBootRetirementEvidence
	if args[0] == retireBootCommand {
		retirement, err = parseVoiceBootRetirementArgs(args, cfg.RoomPlacementEnabled)
		if err != nil {
			fmt.Fprintln(os.Stderr, "usage: invalid boot retirement evidence")
			return 64
		}
	}
	var receipt familyActivationReceipt
	if args[0] == "activate" {
		if len(args) < 4 {
			return 64
		}
		receipt, err = readFamilyActivationReceipt(args[3], cfg.RoomPlacementEnabled)
		if err != nil {
			fmt.Fprintln(os.Stderr, "invalid family activation receipt")
			return 1
		}
	}
	db, err := database.New(cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open database failed")
		return 1
	}
	defer func() {
		if err := db.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "close rollout database failed")
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if args[0] == retireBootCommand {
		if err := api.RetireStoppedVoiceAuthBoot(ctx, db, retirement.NodeBootID); err != nil {
			fmt.Fprintln(os.Stderr, "boot retirement refused")
			return 1
		}
		return 0
	}
	if args[0] == "verify-retired-boot" {
		if len(args) < 3 {
			return 64
		}
		verified, err := api.VerifyRetiredVoiceAuthBoot(ctx, db, uuid.MustParse(args[2]))
		if err != nil {
			fmt.Fprintln(os.Stderr, "observe retired boot failed")
			return 1
		}
		if !verified {
			return 3
		}
		return 0
	}
	if args[0] != "status" {
		if err := mutateFamilyRollout(ctx, db, args[0], receipt); err != nil {
			fmt.Fprintln(os.Stderr, "family rollout mutation refused")
			return 1
		}
	}
	status, err := readFamilyRolloutStatus(ctx, db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "observe family rollout failed")
		return 1
	}
	if len(args) >= 2 && args[1] == "--json" {
		if err := json.NewEncoder(os.Stdout).Encode(status); err != nil {
			return 1
		}
		return 0
	}
	fmt.Printf("voice-enforcement-rollout: %s\n", status.FamilyActivation)
	fmt.Printf("voice-enforcement-rollout: registryRows=%d\n", status.RegistryRows)
	fmt.Printf("voice-enforcement-rollout: dmParentRows=%d\n", status.DMParentRows)
	fmt.Printf("voice-enforcement-rollout: credentialParentRows=%d\n", status.CredentialParentRows)
	fmt.Printf("voice-enforcement-rollout: dmReconciliationRows=%d\n", status.DMReconciliationRows)
	return 0
}
func validFamilyRolloutArgs(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "activate":
		if len(args) != 4 {
			return false
		}
		a := [4]string(args) // fixed shape: index checks are compile-time, not runtime
		return a[1] == "--confirm-drained" && a[2] == "--receipt-file" && a[3] != ""
	case "verify-retired-boot":
		if len(args) != 3 {
			return false
		}
		a := [3]string(args)
		return a[1] == nodeBootIDFlag && familyRolloutUUID.MatchString(a[2])
	case retireBootCommand:
		_, err := parseVoiceBootRetirementArgs(args, false)
		if err == nil {
			return true
		}
		_, err = parseVoiceBootRetirementArgs(args, true)
		return err == nil
	case "deactivate":
		return len(args) == 1
	case "status":
		return len(args) == 1 || (len(args) == 2 && args[1] == "--json")
	default:
		return false
	}
}

// These values bind a trusted operator's host observation. Parsing does not
// independently prove process containment; only the host wrapper can do that.
type voiceBootRetirementEvidence struct {
	NodeBootID uuid.UUID
	Slot       string
	Values     map[string]string
}

func parseVoiceBootRetirementArgs(args []string, placement bool) (voiceBootRetirementEvidence, error) {
	var evidence voiceBootRetirementEvidence
	if len(args) != 13 && len(args) != 11 {
		return evidence, errFamilyRolloutEvidence
	}
	if args[0] != retireBootCommand {
		return evidence, errFamilyRolloutEvidence
	}
	bounds := map[string]int{nodeBootIDFlag: 36, "--slot": 32, "--container-id": 64,
		"--image-sha256": 64, "--started-at": 64, "--finished-at": 64,
		"--process-id": 20, "--process-start": 128, "--process-cwd": 4096}
	values := make(map[string]string, 6)
	for i := 1; i < len(args); i += 2 {
		key, value := args[i], args[i+1]
		bound, known := bounds[key]
		if !known || value == "" || len(value) > bound || !utf8.ValidString(value) ||
			strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return evidence, errFamilyRolloutEvidence
		}
		if _, duplicate := values[key]; duplicate {
			return evidence, errFamilyRolloutEvidence
		}
		values[key] = value
	}
	if !familyRolloutUUID.MatchString(values[nodeBootIDFlag]) {
		return evidence, errFamilyRolloutEvidence
	}
	slot := values["--slot"]
	if (!placement && slot != "media-plane") || (placement && slot != "media-plane-blue" && slot != "media-plane-green") {
		return evidence, errFamilyRolloutEvidence
	}
	if !validVoiceBootRuntimeEvidence(values, placement) {
		return evidence, errFamilyRolloutEvidence
	}
	return voiceBootRetirementEvidence{NodeBootID: uuid.MustParse(values[nodeBootIDFlag]), Slot: slot, Values: values}, nil
}

func validVoiceBootRuntimeEvidence(values map[string]string, placement bool) bool {
	if len(values) == 6 {
		if !familyRolloutDigest.MatchString(values["--container-id"]) || !familyRolloutDigest.MatchString(values["--image-sha256"]) {
			return false
		}
		started, startErr := time.Parse(time.RFC3339Nano, values["--started-at"])
		finished, finishErr := time.Parse(time.RFC3339Nano, values["--finished-at"])
		return startErr == nil && finishErr == nil && !started.IsZero() && finished.After(started)
	}
	if len(values) != 5 || placement || values["--process-start"] == "" || !filepath.IsAbs(values["--process-cwd"]) {
		return false
	}
	pidText := values["--process-id"]
	pid, err := strconv.ParseUint(pidText, 10, 64)
	return err == nil && pid > 1 && strconv.FormatUint(pid, 10) == pidText
}

func readFamilyActivationReceipt(path string, placement bool) (familyActivationReceipt, error) {
	var receipt familyActivationReceipt
	// #nosec G304,G703 -- explicit privileged --receipt-file path, never remote input; read bounded to 4097 bytes.
	f, err := os.Open(path)
	if err != nil {
		return receipt, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, 4097))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil {
		return receipt, errors.Join(readErr, closeErr)
	}
	return decodeFamilyActivationReceipt(raw, placement)
}

// Token walking catches duplicate decoded keys before encoding/json can erase
// them. Every accepted string is subsequently constrained to closed ASCII values.
func familyRolloutJSONValue(d *json.Decoder, depth int) (any, error) {
	if depth > 8 {
		return nil, errFamilyRolloutEvidence
	}
	token, err := d.Token()
	if err != nil || token == nil {
		return nil, errFamilyRolloutEvidence
	}
	delimiter, container := token.(json.Delim)
	if !container {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := map[string]any{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, errFamilyRolloutEvidence
			}
			name, ok := key.(string)
			if !ok {
				return nil, errFamilyRolloutEvidence
			}
			if _, present := object[name]; present {
				return nil, errFamilyRolloutEvidence
			}
			value, err := familyRolloutJSONValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			object[name] = value
		}
		if _, err := d.Token(); err != nil {
			return nil, errFamilyRolloutEvidence
		}
		return object, nil
	case '[':
		array := []any{}
		for d.More() {
			value, err := familyRolloutJSONValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if _, err := d.Token(); err != nil {
			return nil, errFamilyRolloutEvidence
		}
		return array, nil
	default:
		return nil, errFamilyRolloutEvidence
	}
}
func familyRolloutClosedObject(value, exemplar any) bool {
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	raw, err := json.Marshal(exemplar)
	if err != nil {
		return false
	}
	var expected map[string]json.RawMessage
	if json.Unmarshal(raw, &expected) != nil || len(expected) != len(object) {
		return false
	}
	for key := range expected {
		if _, present := object[key]; !present {
			return false
		}
	}
	return true
}
func decodeFamilyActivationReceipt(raw []byte, placement bool) (familyActivationReceipt, error) {
	var receipt familyActivationReceipt
	if len(raw) > 4096 || !utf8.Valid(raw) || !json.Valid(raw) {
		return receipt, errFamilyRolloutEvidence
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := familyRolloutJSONValue(decoder, 0)
	if err != nil || !familyRolloutClosedObject(value, receipt) {
		return receipt, errFamilyRolloutEvidence
	}
	if json.Unmarshal(raw, &receipt) != nil {
		return receipt, errFamilyRolloutEvidence
	}
	if receipt.Schema != "concord.login-family-activation" || receipt.Version != 1 || receipt.Protocol != 4 || !familyRolloutUUID.MatchString(receipt.ActivationID) || !familyRolloutDigest.MatchString(receipt.ControlPlaneArtifactSHA256) {
		return receipt, errFamilyRolloutEvidence
	}
	if !familyRolloutReceiptSlotsValid(value, receipt.MediaSlots, placement) {
		return receipt, errFamilyRolloutEvidence
	}
	if !receipt.drained() || receipt.AuthConnectionClosureRows < 0 || receipt.AuthRoomClosureRows < 0 || receipt.RetiredBootRows < 0 {
		return receipt, errFamilyRolloutEvidence
	}
	return receipt, nil
}

// familyRolloutReceiptSlotsValid requires exactly the expected media slots,
// each a closed object with a digest and a distinct boot ID.
func familyRolloutReceiptSlotsValid(value any, decoded []familyRolloutSlot, placement bool) bool {
	required := map[string]bool{"media-plane": true}
	if placement {
		required = map[string]bool{"media-plane-blue": true, "media-plane-green": true}
	}
	slots, ok := value.(map[string]any)["mediaSlots"].([]any)
	if !ok || len(slots) != len(required) || len(decoded) != len(slots) {
		return false
	}
	boots := map[string]bool{}
	for i, slot := range decoded {
		if !familyRolloutClosedObject(slots[i], familyRolloutSlot{}) || !required[slot.Slot] || !familyRolloutDigest.MatchString(slot.ArtifactSHA256) || !familyRolloutUUID.MatchString(slot.NodeBootID) || boots[slot.NodeBootID] {
			return false
		}
		delete(required, slot.Slot)
		boots[slot.NodeBootID] = true
	}
	return true
}
func (c familyRolloutCounts) drained() bool {
	return c.LegacyUsableSIDs == 0 && c.RegistryRows == 0 && c.AuthConnectionRows == 0 && c.AuthRoomBindingRows == 0 && c.DMParentRows == 0 && c.CredentialParentRows == 0 && c.AuthParentRows == 0 && c.DMReconciliationRows == 0
}

const familyRolloutCountsSQL = `
 (SELECT count(*) FROM refresh_tokens r WHERE r.revoked_at IS NULL AND r.expires_at>clock_timestamp() AND NOT EXISTS(SELECT 1 FROM login_family_sids s WHERE s.sid=r.id)),
 (SELECT count(*) FROM voice_enforcement_sessions),
 (SELECT count(*) FROM voice_auth_connections),
 (SELECT count(*) FROM voice_auth_room_bindings),
 (SELECT count(*) FROM dm_block_voice_ejections),
 (SELECT count(*) FROM credential_epoch_voice_ejections),
 (SELECT count(*) FROM login_family_revocations WHERE completed_at IS NULL),
 (SELECT count(*) FROM dm_block_reconciliations),
 (SELECT count(*) FROM voice_auth_connection_closures),
 (SELECT count(*) FROM voice_auth_room_closures),
 (SELECT count(*) FROM voice_auth_boot_fences WHERE retired_at IS NOT NULL)`

func (c *familyRolloutCounts) destinations() []any {
	return []any{&c.LegacyUsableSIDs, &c.RegistryRows, &c.AuthConnectionRows, &c.AuthRoomBindingRows, &c.DMParentRows, &c.CredentialParentRows, &c.AuthParentRows, &c.DMReconciliationRows, &c.AuthConnectionClosureRows, &c.AuthRoomClosureRows, &c.RetiredBootRows}
}
func readFamilyRolloutStatus(ctx context.Context, db familyRolloutQuerier) (voiceFamilyRolloutStatus, error) {
	status := voiceFamilyRolloutStatus{Schema: "concord.voice-enforcement-status", Version: 1, Protocol: 4, FamilyActivation: "prepared"}
	var active bool
	var receipt []byte
	var oldest sql.NullInt64
	destinations := append([]any{&active, &receipt}, status.destinations()...)
	destinations = append(destinations, &status.FamilyRows, &status.FamilySIDRows, &oldest)
	err := db.QueryRowContext(ctx, `SELECT activated_at IS NOT NULL,receipt,`+familyRolloutCountsSQL+`,
 (SELECT count(*) FROM login_families),(SELECT count(*) FROM login_family_sids),
 (SELECT CASE WHEN min(created_at) IS NULL THEN NULL ELSE greatest(0,floor(extract(epoch FROM clock_timestamp()-min(created_at))))::bigint END FROM (
 SELECT created_at FROM login_family_revocations WHERE completed_at IS NULL
 UNION ALL SELECT created_at FROM credential_epoch_voice_ejections
 UNION ALL SELECT created_at FROM dm_block_voice_ejections
 UNION ALL SELECT created_at FROM dm_block_reconciliations
 ) pending) FROM login_family_rollout WHERE id=TRUE`).Scan(destinations...)
	if err != nil {
		return status, err
	}
	if active {
		var recorded familyActivationReceipt
		if json.Unmarshal(receipt, &recorded) != nil {
			return status, errFamilyRolloutEvidence
		}
		if _, err := decodeFamilyActivationReceipt(receipt, len(recorded.MediaSlots) == 2); err != nil {
			return status, err
		}
		status.FamilyActivation = "active"
		status.FamilyActivationID = &recorded.ActivationID
	}
	if oldest.Valid {
		status.OldestUnsettledSeconds = &oldest.Int64
	}
	// This command is not the running executor. SQL timestamps from another
	// process cannot establish either current-process freshness or eligibility.
	return status, nil
}

func mutateFamilyRollout(ctx context.Context, db *sql.DB, verb string, receipt familyActivationReceipt) (retErr error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			retErr = errors.Join(retErr, err)
		}
	}()
	// This exclusive barrier is the FIRST authority lock. All mint, refresh,
	// terminal and productive creators hold its shared counterpart beforehand.
	var active bool
	var original []byte
	if err = tx.QueryRowContext(ctx, `SELECT activated_at IS NOT NULL,receipt FROM login_family_rollout WHERE id=TRUE FOR UPDATE`).Scan(&active, &original); err != nil {
		return err
	}
	if verb == "deactivate" {
		if active {
			return errFamilyRolloutEvidence
		}
		if _, err = tx.ExecContext(ctx, `UPDATE voice_enforcement_rollout SET activated_at=NULL,updated_at=clock_timestamp() WHERE id=TRUE`); err != nil {
			return err
		}
		return tx.Commit()
	}
	if active {
		first, err := decodeFamilyActivationReceipt(original, len(receipt.MediaSlots) == 2)
		if err != nil || !reflect.DeepEqual(first, receipt) {
			return errFamilyRolloutEvidence
		}
		return tx.Commit()
	}
	if err = normalizeFamilyLegacyTx(ctx, tx); err != nil {
		return err
	}
	if err = verifyFamilyActivationTx(ctx, tx, receipt); err != nil {
		return err
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE voice_enforcement_rollout SET activated_at=COALESCE(activated_at,clock_timestamp()),updated_at=clock_timestamp() WHERE id=TRUE`); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE login_family_rollout SET activated_at=clock_timestamp(),receipt=$1::jsonb,updated_at=clock_timestamp() WHERE id=TRUE AND activated_at IS NULL`, string(raw))
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errFamilyRolloutEvidence
	}
	return tx.Commit()
}
func normalizeFamilyLegacyTx(ctx context.Context, tx *sql.Tx) error {
	var corrupt bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM refresh_tokens r JOIN login_family_sids s ON s.sid=r.id LEFT JOIN login_families f ON f.family_id=s.family_id AND f.user_id=s.user_id WHERE r.revoked_at IS NULL AND (r.user_id<>s.user_id OR f.family_id IS NULL))`).Scan(&corrupt); err != nil {
		return err
	}
	if corrupt {
		return errFamilyRolloutEvidence
	}
	// Expired unrevoked rows still match displacement. Normalize ALL genuinely
	// unmapped rows, without changing the usable-only legacy status definition.
	if _, err := tx.ExecContext(ctx, `UPDATE refresh_tokens r SET revoked_at=clock_timestamp() WHERE r.revoked_at IS NULL AND NOT EXISTS(SELECT 1 FROM login_family_sids s WHERE s.sid=r.id)`); err != nil {
		return err
	}
	var remains bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM refresh_tokens r WHERE r.revoked_at IS NULL AND NOT EXISTS(SELECT 1 FROM login_family_sids s WHERE s.sid=r.id))`).Scan(&remains); err != nil {
		return err
	}
	if remains {
		return errFamilyRolloutEvidence
	}
	return nil
}
func verifyFamilyActivationTx(ctx context.Context, tx *sql.Tx, receipt familyActivationReceipt) error {
	// Boot SHARE prevents retirement between verification and activation commit.
	// Sorting is unnecessary for at most two compatible SHARE locks; retirement
	// takes only one boot and never acquires the earlier rollout authority.
	for _, slot := range receipt.MediaSlots {
		var retired sql.NullTime
		if err := tx.QueryRowContext(ctx, `SELECT retired_at FROM voice_auth_boot_fences WHERE node_boot_id=$1 FOR SHARE`, slot.NodeBootID).Scan(&retired); err != nil {
			return err
		}
		if retired.Valid {
			return errFamilyRolloutEvidence
		}
	}
	var counts familyRolloutCounts
	if err := tx.QueryRowContext(ctx, `SELECT `+familyRolloutCountsSQL).Scan(counts.destinations()...); err != nil {
		return err
	}
	if counts != receipt.familyRolloutCounts || !counts.drained() {
		return errFamilyRolloutEvidence
	}
	return nil
}
