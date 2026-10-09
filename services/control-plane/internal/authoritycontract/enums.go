package authoritycontract

import "slices"

// Ops (§2.5 intent #6, LD-32, LD-34) and the scope-only value.
const (
	OpCancelPending      = "cancel_pending"
	OpEnrollExisting     = "enroll_existing"
	OpEnrollNew          = "enroll_new"
	OpErase              = "erase"
	OpRecover            = "recover"
	OpReplaceRoot        = "replace_root"
	OpRevokeRoot         = "revoke_root"
	OpRotateEK           = "rotate_ek"
	OpRotateRoot         = "rotate_root"
	OpSuspend            = "suspend"
	OpUnsuspend          = "unsuspend"
	OpAddDevice          = "add_device"
	OpBindRecovery       = "bind_recovery"
	OpCancelRecoveryBind = "cancel_recovery_bind"
	ScopeStatus          = "status"
)

// States (§2.5 result #10) and assurances (result #23).
const (
	StateAbsent    = "absent"
	StateActive    = "active"
	StateSuspended = "suspended"
	StatePending   = "pending"
	StateDisputed  = "disputed"
	StateErased    = "erased"

	AssuranceInitial    = "initial"
	AssuranceContinuity = "continuity"
	AssuranceRecovery   = "recovery"
	AssuranceUnverified = "unverified"
)

// cp-authz actors, sig roles, recovery-bind kinds and bind waits.
const (
	ActorOperator = "operator"
	ActorRestore  = "restore"
	ActorUser     = "user"

	RoleActorDevice = "actor-device"
	RoleCandidate   = "candidate"
	RoleCurrentRoot = "current-root"
	RoleNewDevice   = "new-device"
	RoleOperator    = "operator"
	RoleRecoveryKey = "recovery-key"

	BindKindBind   = "bind"
	BindKindCancel = "cancel"
	BindKindUnbind = "unbind"

	BindWaitDelta   = "delta"
	BindWaitNow     = "now"
	BindWaitReplace = "replace"
)

// The 14-name factor vocabulary (cp-evidence #4; LD-33, LD-41).
const (
	FactorAdminWebAuthn = "admin-webauthn"
	FactorBackupCode    = "backup-code"
	FactorDeviceKey     = "device-key"
	FactorEmail         = "email"
	FactorEmailBackup   = "email-backup"
	FactorErasureList   = "erasure-list"
	FactorPasskey       = "passkey"
	FactorPassword      = "password" // pragma: allowlist secret
	FactorRecoveryKey   = "recovery-key"
	FactorRegistration  = "registration"
	FactorSession       = "session"
	FactorSSO           = "sso"
	FactorTOTP          = "totp"
	FactorWebAuthn      = "webauthn"
)

// Refusal is a §3.3 recorded refusal code (27).
type Refusal string

// The 27 recorded refusal codes (§3.3), in the order the DoR lists them.
const (
	RefusalAuthzOutOfWindow          Refusal = "authz_out_of_window"
	RefusalDeviceKeyBound            Refusal = "device_key_bound"
	RefusalDeviceLimit               Refusal = "device_limit"
	RefusalDeviceTooNew              Refusal = "device_too_new"
	RefusalDeviceUnknown             Refusal = "device_unknown"
	RefusalEKGeneration              Refusal = "ek_generation"
	RefusalEKMismatch                Refusal = "ek_mismatch"
	RefusalEKReused                  Refusal = "ek_reused"
	RefusalExpired                   Refusal = "expired"
	RefusalExpiryOutOfRange          Refusal = "expiry_out_of_range"
	RefusalHeld                      Refusal = "held"
	RefusalInPopulation              Refusal = "in_population"
	RefusalInsufficientAuthorization Refusal = "insufficient_authorization"
	RefusalLegacyMismatch            Refusal = "legacy_mismatch"
	RefusalMalformedForOp            Refusal = "malformed_for_op"
	RefusalPolicyDisabled            Refusal = "policy_disabled"
	RefusalPredecessorMismatch       Refusal = "predecessor_mismatch"
	RefusalRecoveryKeyBound          Refusal = "recovery_key_bound"
	RefusalRecoveryKeyUnbound        Refusal = "recovery_key_unbound"
	RefusalRootBoundElsewhere        Refusal = "root_bound_elsewhere"
	RefusalRootRevoked               Refusal = "root_revoked"
	RefusalRootReused                Refusal = "root_reused"
	RefusalStaleDeviceSeq            Refusal = "stale_device_seq"
	RefusalStaleHead                 Refusal = "stale_head"
	RefusalStaleRecoveryGen          Refusal = "stale_recovery_gen"
	RefusalStaleSeq                  Refusal = "stale_seq"
	RefusalWrongState                Refusal = "wrong_state"
)

// ErrorCode is a §3.3 error-enum value (15).
type ErrorCode string

// The 15 error-enum values (§3.3), in the order the DoR lists them.
const (
	ErrorMalformed       ErrorCode = "malformed"
	ErrorBadSignature    ErrorCode = "bad_signature"
	ErrorBadAuthz        ErrorCode = "bad_authz"
	ErrorRequestConflict ErrorCode = "request_conflict"
	ErrorTooLarge        ErrorCode = "too_large"
	ErrorStore           ErrorCode = "store"
	ErrorSigner          ErrorCode = "signer"
	ErrorClock           ErrorCode = "clock"
	ErrorReconcile       ErrorCode = "reconcile"
	ErrorNotSealed       ErrorCode = "not_sealed"
	ErrorBusy            ErrorCode = "busy"
	ErrorNotReady        ErrorCode = "not_ready"
	ErrorNotLeader       ErrorCode = "not_leader"
	ErrorReplication     ErrorCode = "replication"
	ErrorDisk            ErrorCode = "disk"
)

var (
	publishedOps = []string{OpCancelPending, OpEnrollExisting, OpEnrollNew, OpErase, OpRecover,
		OpReplaceRoot, OpRevokeRoot, OpRotateEK, OpRotateRoot, OpSuspend, OpUnsuspend}
	deviceOps = []string{OpAddDevice, OpBindRecovery, OpCancelRecoveryBind, OpEnrollExisting, OpEnrollNew,
		OpRecover, OpReplaceRoot, OpRotateEK, OpRotateRoot, OpUnsuspend}
	scopeValues       = append(slices.Clone(publishedOps), OpAddDevice, OpBindRecovery, OpCancelRecoveryBind, ScopeStatus)
	stateValues       = []string{StateAbsent, StateActive, StateSuspended, StatePending, StateDisputed, StateErased}
	priorStateValues  = []string{"", StateAbsent, StateActive, StateSuspended}
	assuranceValues   = []string{"", AssuranceInitial, AssuranceContinuity, AssuranceRecovery, AssuranceUnverified}
	segmentAssurances = []string{AssuranceInitial, AssuranceContinuity, AssuranceRecovery, AssuranceUnverified}
	suspenders        = []string{ActorOperator, ActorUser}
	actorValues       = []string{ActorOperator, ActorRestore, ActorUser}
	sigRoles          = []string{RoleActorDevice, RoleCandidate, RoleCurrentRoot, RoleNewDevice, RoleOperator, RoleRecoveryKey}
	factorNames       = []string{FactorAdminWebAuthn, FactorBackupCode, FactorDeviceKey, FactorEmail, FactorEmailBackup,
		FactorErasureList, FactorPasskey, FactorPassword, FactorRecoveryKey, FactorRegistration, FactorSession,
		FactorSSO, FactorTOTP, FactorWebAuthn}
	bindKinds    = []string{BindKindBind, BindKindCancel, BindKindUnbind}
	bindWaits    = []string{"", BindWaitDelta, BindWaitNow, BindWaitReplace}
	signedTypes  = []string{"delegation", "device-status", "status", "succession"}
	secretsOps   = []string{OpAddDevice, OpRotateEK, OpRotateRoot}
	nodeRoles    = []string{"leader", "standby"}
	ekAlgs       = []string{"rsa-oaep-4096-sha256"}
	refusalCodes = []string{string(RefusalAuthzOutOfWindow), string(RefusalDeviceKeyBound), string(RefusalDeviceLimit),
		string(RefusalDeviceTooNew), string(RefusalDeviceUnknown), string(RefusalEKGeneration), string(RefusalEKMismatch),
		string(RefusalEKReused), string(RefusalExpired), string(RefusalExpiryOutOfRange), string(RefusalHeld),
		string(RefusalInPopulation), string(RefusalInsufficientAuthorization), string(RefusalLegacyMismatch),
		string(RefusalMalformedForOp), string(RefusalPolicyDisabled), string(RefusalPredecessorMismatch),
		string(RefusalRecoveryKeyBound), string(RefusalRecoveryKeyUnbound), string(RefusalRootBoundElsewhere),
		string(RefusalRootRevoked), string(RefusalRootReused), string(RefusalStaleDeviceSeq), string(RefusalStaleHead),
		string(RefusalStaleRecoveryGen), string(RefusalStaleSeq), string(RefusalWrongState)}
	errorCodes = []string{string(ErrorMalformed), string(ErrorBadSignature), string(ErrorBadAuthz),
		string(ErrorRequestConflict), string(ErrorTooLarge), string(ErrorStore), string(ErrorSigner), string(ErrorClock),
		string(ErrorReconcile), string(ErrorNotSealed), string(ErrorBusy), string(ErrorNotReady), string(ErrorNotLeader),
		string(ErrorReplication), string(ErrorDisk)}
)
