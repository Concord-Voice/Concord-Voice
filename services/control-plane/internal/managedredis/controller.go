package managedredis

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ClientIdentity identifies one Redis connection for targeted drain checks.
type ClientIdentity struct {
	ID       string
	Username string
}

// RedisAdmin is the narrow privileged command surface used for ACL rotation.
type RedisAdmin interface {
	LoadACL(context.Context, string) error
	ListClients(context.Context) ([]ClientIdentity, error)
	ProbeAuth(context.Context, string, string) error
	UserExists(context.Context, string) (bool, error)
	KillUser(context.Context, string) error
}

// ControllerOptions contains the checked ACL path, bootstrap identity, and peers.
type ControllerOptions struct {
	Admin        RedisAdmin
	ACLPath      string
	JournalPath  string
	MarkerPath   string
	Bootstrap    AdminBootstrap
	Now          func() time.Time
	ExpectedUIDs map[string]uint32
	ACLUID       int
	ACLGID       int
}

// AppliedAck binds an instance to an exact rotation and generation.
type AppliedAck struct {
	Version    int    `json:"version"`
	RotationID string `json:"rotationId"`
	Service    string `json:"service"`
	InstanceID string `json:"instanceId"`
	Generation uint64 `json:"generation"`
	Username   string `json:"username"`
}

// InstanceBinding is a root-inspected container's exclusive ACK socket slot.
type InstanceBinding struct {
	ContainerID string `json:"containerId"`
	InstanceID  string `json:"instanceId"`
}

// PriorAckProof ties one accepted prior ACK to its root-inspected container.
type PriorAckProof struct {
	Service     string `json:"service"`
	InstanceID  string `json:"instanceId"`
	Slot        string `json:"slot"`
	ContainerID string `json:"containerId"`
}

var boundHostname = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

const (
	controlPlaneService = "control-plane"
	mediaPlaneService   = "media-plane"
)

func slotService(slot string) string {
	if slot == controlPlaneService {
		return controlPlaneService
	}
	if slot == mediaPlaneService || slot == "media-plane-blue" || slot == "media-plane-green" {
		return mediaPlaneService
	}
	return ""
}

// Controller holds the in-memory ACL rotation state and expected acknowledgements.
type Controller struct {
	admin             RedisAdmin
	aclPath           string
	journalPath       string
	markerPath        string
	bootstrap         AdminBootstrap
	now               func() time.Time
	uids              map[string]uint32
	aclUID            int
	aclGID            int
	mu                sync.Mutex
	policy            Policy
	expected          map[string]map[string]struct{}
	acks              map[string]map[string]AppliedAck
	bindings          map[string][]InstanceBinding
	state             string
	loaded            bool
	failureCode       string
	lastRequiredAckAt time.Time
	intent            string
	revoked           map[string]revokedIdentity
	oldUsernames      map[string]string
	priorExpected     map[string][]string
	priorAcks         []AppliedAck
	priorRotationID   string
	priorTargets      map[string]uint64
	recoveryAt        time.Time
	validatedAt       time.Time
}

var errController = errors.New("managed Redis controller rejected operation")

// ErrPolicyStale identifies a provider policy that would restore a revoked user.
var ErrPolicyStale = errors.New("managed Redis policy stale")

// NewController validates the privileged rotation dependencies before use.
func NewController(options ControllerOptions) (*Controller, error) {
	if options.Admin == nil || options.Now == nil || options.ACLPath == "" || options.JournalPath == "" || options.MarkerPath == "" ||
		options.Bootstrap.Version != 1 || !validScope(options.Bootstrap.Scope) ||
		!aclUsername.MatchString(options.Bootstrap.Username) || options.Bootstrap.Username == "default" || reservedServiceUsername(options.Bootstrap.Username) || options.Bootstrap.Password == "" ||
		len(options.ExpectedUIDs) != 2 {
		return nil, errController
	}
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		if value, ok := options.ExpectedUIDs[service]; !ok || value == 0 {
			return nil, errController
		}
	}
	if info, err := os.Lstat(filepath.Dir(options.ACLPath)); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errController
	}
	if err := checkedPrivateDir(filepath.Dir(options.JournalPath)); err != nil {
		return nil, errController
	}
	if err := checkedPrivateDir(filepath.Dir(options.MarkerPath)); err != nil {
		return nil, errController
	}
	uids := map[string]uint32{controlPlaneService: options.ExpectedUIDs[controlPlaneService], mediaPlaneService: options.ExpectedUIDs[mediaPlaneService]}
	return &Controller{admin: options.Admin, aclPath: options.ACLPath, journalPath: options.JournalPath, markerPath: options.MarkerPath, bootstrap: options.Bootstrap,
		now: options.Now, uids: uids, aclUID: options.ACLUID, aclGID: options.ACLGID, state: "failed",
		revoked: map[string]revokedIdentity{controlPlaneService: {}, mediaPlaneService: {}}}, nil
}

func (c *Controller) validatePolicy(policy Policy) error {
	return c.validatePolicyAt(policy, c.now())
}

func (c *Controller) validatePolicyAt(policy Policy, at time.Time) error {
	raw, err := json.Marshal(policy)
	if err != nil {
		return errController
	}
	if _, err := ParsePolicy(raw, policy.Scope, at); err != nil ||
		policy.Scope.Project != c.bootstrap.Scope.Project ||
		policy.Scope.Environment != c.bootstrap.Scope.Environment ||
		policy.Scope.Path != c.bootstrap.Scope.Path ||
		policy.Scope.Name != "REDIS_AUTH_POLICY" || c.bootstrap.Scope.Name != "REDIS_ADMIN_BOOTSTRAP" {
		return errController
	}
	for _, service := range policy.Services {
		for _, generation := range service.Generations {
			if generation.Username == c.bootstrap.Username {
				return errController
			}
		}
	}
	return nil
}

// PrepareInitial writes a single-generation policy and journal before Redis starts.
func (c *Controller) PrepareInitial(policy Policy) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.validatePolicy(policy); err != nil {
		return err
	}
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		if len(policy.Services[service].Generations) != 1 {
			return errController
		}
	}
	marker, err := c.markerState()
	if err != nil || marker == "activated" {
		return errController
	}
	journalExists, err := checkedJournalFile(c.journalPath)
	if err != nil {
		return errController
	}
	aclExists, err := c.checkedACLFile()
	if err != nil || marker == "" && (journalExists || aclExists) || marker == "initializing" && aclExists && !journalExists {
		return errController
	}
	if marker == "" {
		if err := c.createMarker(initializingMarkerContents); err != nil {
			return err
		}
	}
	if journalExists {
		if err := c.restoreInitialJournalLocked(policy); err != nil {
			return err
		}
	} else {
		j := c.snapshotJournalLocked()
		j.Effective, j.Phase, j.Intent, j.ValidatedAt = policy, "prepared", "", c.now()
		if err := c.persistJournal(j); err != nil {
			return err
		}
		c.applyJournalLocked(j)
	}
	if err := c.writePolicy(policy); err != nil {
		return err
	}
	return c.promoteInitializingMarker()
}

func (c *Controller) restoreInitialJournalLocked(policy Policy) error {
	raw, err := os.ReadFile(c.journalPath)
	if err != nil {
		return errController
	}
	var saved rotationJournal
	if decodeJournal(raw, &saved) != nil || c.validateJournal(saved) != nil ||
		saved.Phase != "prepared" || saved.Intent != "" || saved.FailureCode != "" ||
		len(saved.Expected) != 0 || len(saved.Acks) != 0 || len(saved.Bindings) != 0 ||
		len(saved.PriorExpected) != 0 || len(saved.PriorAcks) != 0 ||
		!samePolicy(saved.Effective, policy) {
		return errController
	}
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		if saved.Revoked[service].Generation != 0 {
			return errController
		}
	}
	c.applyJournalLocked(saved)
	return nil
}

type lostStateProof struct {
	Version           int                      `json:"version"`
	Scope             SecretScope              `json:"scope"`
	RotationID        string                   `json:"rotationId"`
	InventoryComplete bool                     `json:"inventoryComplete"`
	ReviewedBy        string                   `json:"reviewedBy"`
	ReviewedAt        time.Time                `json:"reviewedAt"`
	Services          map[string]lostStateRole `json:"services"`
}

type lostStateRole struct {
	MaxGeneration uint64 `json:"maxGeneration"`
}

func (c *Controller) validLostStateProof(policy Policy, proof lostStateProof) bool {
	if proof.Version != 1 || proof.Scope != policy.Scope || proof.RotationID != policy.RotationID ||
		!proof.InventoryComplete || proof.ReviewedAt.IsZero() || proof.ReviewedAt.After(c.now()) ||
		len(proof.ReviewedBy) == 0 || len(proof.ReviewedBy) > 128 || len(proof.Services) != 2 {
		return false
	}
	for _, r := range proof.ReviewedBy {
		if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._@-", r) {
			return false
		}
	}
	return true
}

func (c *Controller) applyLostStateInventory(j *rotationJournal, policy Policy, proof lostStateProof) error {
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		prior, ok := proof.Services[service]
		if !ok || prior.MaxGeneration == 0 ||
			policy.Services[service].TargetGeneration <= prior.MaxGeneration {
			return errController
		}
		j.Revoked[service] = revokedIdentity{Generation: prior.MaxGeneration}
	}
	return nil
}

// PrepareLostStateRecovery accepts separately reviewed prior-generation floors
// when both host-RAM state files have been lost. Its caller stops all
// applications and Redis before invoking this non-serving operation.
func (c *Controller) PrepareLostStateRecovery(policy Policy, rawProof []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.intent == "recovery" {
		return c.completeCurrentRecoveryIntentLocked(policy)
	}
	if c.policy.Version != 0 {
		return errController
	}
	if resumed, err := c.resumePersistedRecoveryIntentLocked(policy); resumed || err != nil {
		return err
	}
	if c.validatePolicy(policy) != nil || !singleGenerationPolicy(policy) {
		return errController
	}
	if exists, err := checkedJournalFile(c.journalPath); err != nil || exists {
		return errController
	}
	if _, err := os.Lstat(c.aclPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		return errController
	}
	var proof lostStateProof
	if DecodeBoundedJSON(rawProof, &proof) != nil || !c.validLostStateProof(policy, proof) {
		return errController
	}
	j := c.snapshotJournalLocked()
	if err := c.applyLostStateInventory(&j, policy, proof); err != nil {
		return err
	}
	if exists, err := c.activationMarker(); err != nil {
		return errController
	} else if !exists {
		if err := c.createActivationMarker(); err != nil {
			return err
		}
	}
	j.Effective, j.Phase, j.Intent, j.ValidatedAt = policy, "prepared", "recovery", c.now()
	j.FailureCode, j.RecoveryAt = "reconcile", c.now()
	if err := c.persistJournal(j); err != nil {
		return err
	}
	c.applyJournalLocked(j)
	return c.completeRecoveryIntentLocked(&j)
}

// PrepareRecovery replaces a stopped cohort with fresh single-generation users.
// Its caller must hold the deployment interlock and prove every app and Redis stopped.
func (c *Controller) PrepareRecovery(policy Policy) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.requireActivationMarker(); err != nil {
		return err
	}
	if c.intent == "recovery" {
		return c.completeCurrentRecoveryIntentLocked(policy)
	}
	if c.policy.Version == 0 || !rotationID.MatchString(policy.RotationID) {
		return errController
	}
	if samePolicy(c.policy, policy) && !c.recoveryAt.IsZero() && c.state == "prepared" && c.failureCode == "" &&
		len(c.expected) == 0 && len(c.acks) == 0 {
		return nil
	}
	if policy.RotationID == c.policy.RotationID {
		return errController
	}
	if !c.recoveryAt.IsZero() && (c.state != "finalized" || !c.allExpectedAckedLocked()) {
		return errController
	}
	if err := c.validatePolicy(policy); err != nil {
		return err
	}
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		if len(policy.Services[service].Generations) != 1 || policy.Services[service].TargetGeneration <= c.policy.Services[service].TargetGeneration {
			return errController
		}
	}
	j := c.snapshotJournalLocked()
	c.advanceRecoveryFloors(&j)
	if recoveryReusesRevoked(j, policy) {
		return errController
	}
	c.prepareRecoveryJournal(&j, policy)
	if err := c.persistJournal(j); err != nil {
		return err
	}
	c.applyJournalLocked(j)
	return c.completeRecoveryIntentLocked(&j)
}

func (c *Controller) resumePersistedRecoveryIntentLocked(policy Policy) (bool, error) {
	exists, err := checkedJournalFile(c.journalPath)
	if err != nil || !exists {
		return false, err
	}
	raw, err := os.ReadFile(c.journalPath)
	if err != nil {
		return true, errController
	}
	var journal rotationJournal
	if decodeJournal(raw, &journal) != nil || c.validateJournal(journal) != nil || journal.Intent != "recovery" ||
		!samePolicy(journal.Effective, policy) || c.requireActivationMarker() != nil {
		return true, errController
	}
	c.applyJournalLocked(journal)
	return true, c.completeRecoveryIntentLocked(&journal)
}

func (c *Controller) completeCurrentRecoveryIntentLocked(policy Policy) error {
	journal := c.snapshotJournalLocked()
	if !samePolicy(journal.Effective, policy) {
		return errController
	}
	return c.completeRecoveryIntentLocked(&journal)
}

func (c *Controller) completeRecoveryIntentLocked(journal *rotationJournal) error {
	if journal.Intent != "recovery" || journal.FailureCode != "reconcile" || !singleGenerationPolicy(journal.Effective) ||
		c.validatePolicy(journal.Effective) != nil {
		return errController
	}
	if err := c.writePolicy(journal.Effective); err != nil {
		return err
	}
	journal.Intent, journal.FailureCode = "", ""
	if err := c.persistJournal(*journal); err != nil {
		return err
	}
	c.applyJournalLocked(*journal)
	c.loaded = false
	return nil
}

func (c *Controller) advanceRecoveryFloors(j *rotationJournal) {
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		floor := j.Revoked[service]
		for _, gen := range c.policy.Services[service].Generations {
			if gen.Generation > floor.Generation {
				floor.Generation = gen.Generation
			}
		}
		j.Revoked[service] = floor
	}
}

func recoveryReusesRevoked(j rotationJournal, policy Policy) bool {
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		if policy.Services[service].TargetGeneration <= j.Revoked[service].Generation {
			return true
		}
	}
	return false
}

func (c *Controller) prepareRecoveryJournal(j *rotationJournal, policy Policy) {
	j.Effective = policy
	j.Phase = "prepared"
	j.Intent = "recovery"
	j.ValidatedAt = c.now()
	j.FailureCode = "reconcile"
	j.PriorExpected = j.Expected
	j.PriorAcks = j.Acks
	if len(j.PriorExpected) == 2 {
		j.PriorRotationID = c.policy.RotationID
		j.PriorTargets = map[string]uint64{
			controlPlaneService: c.policy.Services[controlPlaneService].TargetGeneration,
			mediaPlaneService:   c.policy.Services[mediaPlaneService].TargetGeneration,
		}
	} else {
		j.PriorRotationID = ""
		j.PriorTargets = nil
	}
	j.Expected = nil
	j.Acks = nil
	j.LastRequiredAckAt = time.Time{}
	j.OldUsernames = nil
	j.RecoveryAt = c.now()
}

// Restore recovers the journaled state without reading the Redis-group ACL.
func (c *Controller) Restore() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.requireActivationMarker(); err != nil {
		return err
	}
	exists, err := checkedJournalFile(c.journalPath)
	if err != nil || !exists {
		return errController
	}
	raw, err := os.ReadFile(c.journalPath)
	if err != nil {
		return errController
	}
	var j rotationJournal
	if err := decodeJournal(raw, &j); err != nil || c.validateJournal(j) != nil {
		return errController
	}
	if j.Intent == "recovery" {
		return c.completeRecoveryIntentLocked(&j)
	}
	if exists, err := c.checkedACLFile(); err != nil {
		return err
	} else if !exists || j.Intent != "" {
		if err := c.validatePolicy(j.Effective); err != nil {
			return errController
		}
		if err := c.writePolicy(j.Effective); err != nil {
			return err
		}
	}
	c.applyJournalLocked(j)
	return nil
}

// ValidateRestoredPolicy admits Redis startup only for the policy already
// journaled and rendered before Redis reads the ACL file.
func (c *Controller) ValidateRestoredPolicy(policy Policy) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.validatePolicy(policy) != nil || c.policy.Version == 0 || c.failureCode != "" || c.intent != "" ||
		c.includesRevoked(policy) || !samePolicy(c.policy, policy) {
		return errController
	}
	return nil
}

// RevocationPending admits only the journaled deny-ACL recovery path.
func (c *Controller) RevocationPending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.policy.Version != 0 && c.state == "drained" && c.intent == "revoke" &&
		c.failureCode == "reconcile" && len(c.oldUsernames) == 2
}

// ReconciliationPending identifies an accepted policy whose ACL load was interrupted.
func (c *Controller) ReconciliationPending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.policy.Version != 0 && c.intent == "reconcile" && c.failureCode == "reconcile"
}

// ResumeReconcile reloads only the already journaled policy before serving apps.
func (c *Controller) ResumeReconcile(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.intent != "reconcile" || c.failureCode != "reconcile" || c.policy.Version == 0 ||
		c.validatePolicy(c.policy) != nil || c.includesRevoked(c.policy) {
		return errController
	}
	return c.applyReconciledPolicyLocked(ctx, c.policy, true)
}

func (c *Controller) checkedACLFile() (bool, error) {
	info, err := os.Lstat(c.aclPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o440 {
		return false, errController
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	uid, gid := c.aclUID, c.aclGID
	if uid == 0 && gid == 0 {
		uid, gid = os.Geteuid(), os.Getegid()
	}
	if !ok || int(stat.Uid) != uid || int(stat.Gid) != gid {
		return false, errController
	}
	return true, nil
}

// Reconcile loads a monotonic policy while retaining registered progress.
func (c *Controller) Reconcile(ctx context.Context, policy Policy) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.validatePolicy(policy); err != nil {
		return err
	}
	if c.policy.Version == 0 || c.intent == "revoke" {
		return errController
	}
	if c.includesRevoked(policy) {
		c.failureCode = "policy-stale"
		return ErrPolicyStale
	}
	sameBefore := c.policy.Version != 0 && samePolicy(c.policy, policy)
	if !sameBefore && !c.hasFreshOverlapLocked(policy) {
		return errController
	}
	if c.loaded && c.intent == "" && c.policy.RotationID == policy.RotationID && sameBefore {
		c.failureCode = ""
		return nil
	}
	if err := c.validateReconcileTransitionLocked(policy, sameBefore); err != nil {
		return err
	}
	return c.applyReconciledPolicyLocked(ctx, policy, sameBefore)
}

func (c *Controller) applyReconciledPolicyLocked(ctx context.Context, policy Policy, sameBefore bool) error {
	j := c.snapshotJournalLocked()
	j.Effective = policy
	j.Intent = "reconcile"
	j.FailureCode = "reconcile"
	if !sameBefore {
		if c.policy.RotationID != policy.RotationID && len(j.Expected) == 2 {
			var err error
			j.PriorExpected, j.PriorAcks, j.Bindings, err = livePriorBindingProof(j)
			if err != nil {
				return err
			}
			j.PriorRotationID = c.policy.RotationID
			j.PriorTargets = map[string]uint64{
				controlPlaneService: c.policy.Services[controlPlaneService].TargetGeneration,
				mediaPlaneService:   c.policy.Services[mediaPlaneService].TargetGeneration,
			}
		}
		j.ValidatedAt = c.now()
		j.Phase = "prepared"
		j.Expected = nil
		j.Acks = nil
		j.LastRequiredAckAt = time.Time{}
	}
	if err := c.persistJournal(j); err != nil {
		return err
	}
	c.applyJournalLocked(j)
	if err := c.writePolicy(policy); err != nil {
		c.failureCode = "reconcile"
		return err
	}
	if err := c.admin.LoadACL(ctx, c.aclPath); err != nil {
		j.FailureCode = "reconcile"
		if c.persistJournal(j) == nil {
			c.applyJournalLocked(j)
		}
		return errController
	}
	j.Intent = ""
	j.FailureCode = ""
	if err := c.persistJournal(j); err != nil {
		c.failureCode = "reconcile"
		return err
	}
	c.applyJournalLocked(j)
	c.loaded = true
	return nil
}

// A new rotation needs proof for live slot heads, not retired containers from the old rotation.
func livePriorBindingProof(j rotationJournal) (map[string][]string, []AppliedAck, map[string][]InstanceBinding, error) {
	expected := map[string][]string{controlPlaneService: {}, mediaPlaneService: {}}
	acks := make([]AppliedAck, 0, len(j.Bindings))
	bindings := make(map[string][]InstanceBinding, len(j.Bindings))
	for slot, history := range j.Bindings {
		if len(history) == 0 {
			return nil, nil, nil, errController
		}
		head := history[len(history)-1]
		service := slotService(slot)
		index := slices.IndexFunc(j.Acks, func(ack AppliedAck) bool {
			return ack.Service == service && ack.InstanceID == head.InstanceID
		})
		if index < 0 {
			return nil, nil, nil, errController
		}
		expected[service] = append(expected[service], head.InstanceID)
		acks = append(acks, j.Acks[index])
		bindings[slot] = []InstanceBinding{head}
	}
	if len(expected[controlPlaneService]) == 0 || len(expected[mediaPlaneService]) == 0 {
		return nil, nil, nil, errController
	}
	for service := range expected {
		sort.Strings(expected[service])
	}
	sort.Slice(acks, func(i, k int) bool {
		if acks[i].Service != acks[k].Service {
			return acks[i].Service < acks[k].Service
		}
		return acks[i].InstanceID < acks[k].InstanceID
	})
	return expected, acks, bindings, nil
}

func (c *Controller) hasFreshOverlapLocked(policy Policy) bool {
	deadline := c.now().Add(MinimumOverlap)
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		generations := policy.Services[service].Generations
		if len(generations) != 2 {
			continue
		}
		for _, generation := range generations {
			if generation.ValidUntil.Before(deadline) {
				return false
			}
		}
	}
	return true
}

func (c *Controller) validateReconcileTransitionLocked(policy Policy, sameBefore bool) error {
	if c.policy.RotationID != policy.RotationID {
		if err := c.validateNextRotationLocked(policy); err != nil {
			return err
		}
	}
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		if !reconcileGenerationsCompatible(c.policy.Services[service], policy.Services[service]) {
			return errController
		}
	}
	if c.policy.RotationID == policy.RotationID && !sameBefore {
		// A prepared single-generation policy may gain its candidate once.
		if len(c.expected) > 0 || len(c.policy.Services[controlPlaneService].Generations) != 1 || len(c.policy.Services[mediaPlaneService].Generations) != 1 {
			return errController
		}
		for _, service := range []string{controlPlaneService, mediaPlaneService} {
			if policy.Services[service].TargetGeneration <= c.policy.Services[service].TargetGeneration {
				return errController
			}
		}
	}
	return nil
}

func (c *Controller) validateNextRotationLocked(policy Policy) error {
	if c.state != "finalized" {
		return errController
	}
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		if policy.Services[service].TargetGeneration <= c.policy.Services[service].TargetGeneration {
			return errController
		}
	}
	if len(c.expected) == 0 && len(c.priorExpected) != 0 {
		return errController
	}
	if len(c.expected) > 0 && !c.allExpectedAckedLocked() {
		return errController
	}
	return nil
}

func reconcileGenerationsCompatible(prior, candidate PolicyService) bool {
	if candidate.TargetGeneration < prior.TargetGeneration {
		return false
	}
	activePresent := false
	for _, oldGeneration := range prior.Generations {
		for _, newGeneration := range candidate.Generations {
			if oldGeneration.Generation == prior.TargetGeneration && oldGeneration.Generation == newGeneration.Generation {
				activePresent = true
			}
			if oldGeneration.Generation == newGeneration.Generation &&
				(oldGeneration.Username != newGeneration.Username || oldGeneration.PasswordHash != newGeneration.PasswordHash || // pragma: allowlist secret
					!oldGeneration.ValidFrom.Equal(newGeneration.ValidFrom) || !oldGeneration.ValidUntil.Equal(newGeneration.ValidUntil)) {
				return false
			}
		}
	}
	return activePresent
}

func (c *Controller) includesRevoked(policy Policy) bool {
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		floor := c.revoked[service]
		for _, generation := range policy.Services[service].Generations {
			if generation.Generation <= floor.Generation {
				return true
			}
		}
	}
	return false
}

func samePolicy(left, right Policy) bool {
	if left.Version != right.Version || left.Scope != right.Scope ||
		left.RotationID != right.RotationID || len(left.Services) != len(right.Services) {
		return false
	}
	for name, prior := range left.Services {
		candidate, ok := right.Services[name]
		if !ok || prior.TargetGeneration != candidate.TargetGeneration || len(prior.Generations) != len(candidate.Generations) {
			return false
		}
		byGeneration := make(map[uint64]PolicyGeneration, len(candidate.Generations))
		for _, generation := range candidate.Generations {
			byGeneration[generation.Generation] = generation
		}
		for _, oldGeneration := range prior.Generations {
			newGeneration, ok := byGeneration[oldGeneration.Generation]
			if !ok || oldGeneration.Username != newGeneration.Username ||
				oldGeneration.PasswordHash != newGeneration.PasswordHash || // pragma: allowlist secret -- policy field, no literal
				!oldGeneration.ValidFrom.Equal(newGeneration.ValidFrom) || !oldGeneration.ValidUntil.Equal(newGeneration.ValidUntil) {
				return false
			}
		}
	}
	return true
}

// Probe proves a candidate credential on a fresh Redis connection.
func (c *Controller) Probe(ctx context.Context, username, password string) error {
	if username != c.bootstrap.Username || subtle.ConstantTimeCompare([]byte(password), []byte(c.bootstrap.Password)) != 1 {
		return errors.New("managed Redis probe denied")
	}
	if err := c.admin.ProbeAuth(ctx, username, password); err != nil {
		return errors.New("managed Redis probe failed")
	}
	return nil
}

// ProbeConfigured proves each configured target credential.
func (c *Controller) ProbeConfigured(ctx context.Context) error {
	return c.Probe(ctx, c.bootstrap.Username, c.bootstrap.Password)
}

// RegisterExpected fixes the complete hostname set for this rotation.
func (c *Controller) RegisterExpected(_ context.Context, rotationID string, instances map[string][]string, targets map[string]uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.policy.Version == 0 || c.failureCode != "" || c.intent != "" || c.state == "drained" ||
		c.state == "finalized" && !singleGenerationPolicy(c.policy) ||
		rotationID != c.policy.RotationID || len(instances) != 2 || len(targets) != 2 {
		return errController
	}
	next := make(map[string]map[string]struct{}, 2)
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		set, err := c.validatedExpectedService(service, instances[service], targets[service])
		if err != nil {
			return err
		}
		next[service] = set
	}
	j := c.snapshotJournalLocked()
	j.Expected = make(map[string][]string, 2)
	for service, hosts := range next {
		for host := range hosts {
			j.Expected[service] = append(j.Expected[service], host)
		}
		sort.Strings(j.Expected[service])
	}
	pruneRetiredPriorBindingProof(&j)
	if !journalAllAcked(j) {
		j.Phase = "prepared"
		j.LastRequiredAckAt = time.Time{}
	} else if singleGenerationPolicy(j.Effective) {
		j.Phase = "finalized"
	}
	if err := c.persistJournal(j); err != nil {
		return err
	}
	c.applyJournalLocked(j)
	return nil
}

func (c *Controller) validatedExpectedService(service string, instances []string, target uint64) (map[string]struct{}, error) {
	if target != c.policy.Services[service].TargetGeneration || len(instances) == 0 || len(instances) > 64 {
		return nil, errController
	}
	set := make(map[string]struct{}, len(instances))
	for _, instance := range instances {
		if !validInstanceID(instance) {
			return nil, errController
		}
		if _, duplicate := set[instance]; duplicate {
			return nil, errController
		}
		set[instance] = struct{}{}
	}
	if len(c.expected) > 0 {
		for oldInstance := range c.expected[service] {
			if _, kept := set[oldInstance]; !kept {
				return nil, errController
			}
		}
	}
	return set, nil
}

func validInstanceID(instance string) bool {
	return instance != "" && len(instance) <= 64 && !strings.ContainsAny(instance, "\r\n\x00")
}

func singleGenerationPolicy(policy Policy) bool {
	return len(policy.Services[controlPlaneService].Generations) == 1 && len(policy.Services[mediaPlaneService].Generations) == 1
}

func journalAllAcked(j rotationJournal) bool {
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		if !journalServiceAllAcked(j, service) {
			return false
		}
	}
	return true
}

func journalServiceAllAcked(j rotationJournal, service string) bool {
	if len(j.Expected[service]) == 0 {
		return false
	}
	for _, host := range j.Expected[service] {
		found := false
		for _, ack := range j.Acks {
			if ack.Service == service && ack.InstanceID == host && ack.Generation == j.Effective.Services[service].TargetGeneration &&
				ack.Username == policyTargetUsername(j.Effective.Services[service]) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (c *Controller) allExpectedAckedLocked() bool {
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		if len(c.expected[service]) == 0 || len(c.acks[service]) != len(c.expected[service]) {
			return false
		}
		for host := range c.expected[service] {
			if c.acks[service][host].Generation != c.policy.Services[service].TargetGeneration ||
				c.acks[service][host].Username != policyTargetUsername(c.policy.Services[service]) {
				return false
			}
		}
	}
	return true
}

// RecordAck accepts a peer-bound acknowledgement for an expected instance.
func (c *Controller) RecordAck(_ context.Context, slot string, peerUID uint32, ack AppliedAck) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if slotService(slot) != ack.Service || len(c.bindings[slot]) == 0 ||
		c.bindings[slot][len(c.bindings[slot])-1].InstanceID != ack.InstanceID {
		return errController
	}
	if c.state == "finalized" && !singleGenerationPolicy(c.policy) || c.state == "drained" || c.intent != "" || ack.Version != 1 || !rotationID.MatchString(ack.RotationID) ||
		ack.RotationID != c.policy.RotationID || ack.Generation != c.policy.Services[ack.Service].TargetGeneration ||
		ack.Username != policyTargetUsername(c.policy.Services[ack.Service]) ||
		peerUID != c.uids[ack.Service] {
		return errController
	}
	if _, ok := c.expected[ack.Service][ack.InstanceID]; !ok {
		return errController
	}
	if previous, exists := c.acks[ack.Service][ack.InstanceID]; exists && previous != ack {
		return errController
	}
	wasComplete := c.allExpectedAckedLocked()
	j := c.ackJournalLocked(ack, wasComplete)
	if j.Phase == "promoted" && !wasComplete && !c.hasFreshOverlapLocked(j.Effective) {
		return errController
	}
	if err := c.persistJournal(j); err != nil {
		return err
	}
	c.applyJournalLocked(j)
	return nil
}

func (c *Controller) ackJournalLocked(ack AppliedAck, wasComplete bool) rotationJournal {
	j := c.snapshotJournalLocked()
	if previous, exists := c.acks[ack.Service][ack.InstanceID]; !exists || previous != ack {
		j.Acks = append(j.Acks, ack)
	}
	pruneRetiredPriorBindingProof(&j)
	if journalAllAcked(j) {
		if singleGenerationPolicy(j.Effective) {
			j.Phase = "finalized"
		} else {
			j.Phase = "promoted"
		}
		if !wasComplete {
			j.LastRequiredAckAt = c.now()
		}
	} else {
		j.Phase = "prepared"
	}
	return j
}

// BindInstance journals a root-inspected stopped container before it starts.
// A replacement preserves the prior ACK and must name the exact removed container.
func (c *Controller) BindInstance(_ context.Context, slot, containerID, instanceID, replacedContainerID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.validBindingInputLocked(slot, containerID, instanceID) {
		return errController
	}
	history := c.bindings[slot]
	if len(history) > 0 && history[len(history)-1].ContainerID == containerID &&
		history[len(history)-1].InstanceID == instanceID && replacedContainerID == "" {
		return nil
	}
	if !c.validBindingReplacementLocked(slot, instanceID, replacedContainerID, history) || c.bindingExistsLocked(containerID, instanceID) {
		return errController
	}
	j := c.snapshotJournalLocked()
	if j.Bindings == nil {
		j.Bindings = make(map[string][]InstanceBinding, 4)
	}
	j.Bindings[slot] = append(j.Bindings[slot], InstanceBinding{ContainerID: containerID, InstanceID: instanceID})
	pruneRetiredPriorBindingProof(&j)
	if len(j.Bindings[slot]) > 64 {
		j.Bindings[slot] = compactBindingHistory(j, slot)
		if len(j.Bindings[slot]) > 64 {
			return errController
		}
	}
	if err := c.persistJournal(j); err != nil {
		return err
	}
	c.applyJournalLocked(j)
	return nil
}

func (c *Controller) validBindingInputLocked(slot, containerID, instanceID string) bool {
	if slotService(slot) == "" || len(containerID) != 64 || !boundHostname.MatchString(instanceID) ||
		c.policy.Version == 0 || c.failureCode != "" || c.intent != "" || c.state == "drained" {
		return false
	}
	_, err := hex.DecodeString(containerID)
	return err == nil && strings.ToLower(containerID) == containerID
}

func (c *Controller) validBindingReplacementLocked(slot, instanceID, replacedContainerID string, history []InstanceBinding) bool {
	if len(history) == 0 {
		return replacedContainerID == ""
	}
	prior := history[len(history)-1]
	return replacedContainerID == prior.ContainerID && instanceID != prior.InstanceID && c.bindingAckedLocked(slot, prior)
}

func compactBindingHistory(j rotationJournal, slot string) []InstanceBinding {
	history := j.Bindings[slot]
	referenced := make(map[string]struct{}, len(j.Acks)+len(j.PriorAcks))
	for _, ack := range append(append([]AppliedAck(nil), j.Acks...), j.PriorAcks...) {
		if ack.Service == slotService(slot) {
			referenced[ack.InstanceID] = struct{}{}
		}
	}
	kept := make([]InstanceBinding, 0, len(history))
	for index, binding := range history {
		_, needed := referenced[binding.InstanceID]
		if needed || index == len(history)-1 {
			kept = append(kept, binding)
		}
	}
	return kept
}

func pruneRetiredPriorBindingProof(j *rotationJournal) {
	if len(j.Expected) != 2 || len(j.PriorExpected) == 0 {
		return
	}
	current := make(map[string]struct{}, len(j.Bindings))
	for slot, history := range j.Bindings {
		if len(history) > 0 {
			current[slotService(slot)+":"+history[len(history)-1].InstanceID] = struct{}{}
		}
	}
	for _, ack := range j.Acks {
		delete(current, ack.Service+":"+ack.InstanceID)
	}
	kept := make([]AppliedAck, 0, len(j.PriorAcks))
	for _, ack := range j.PriorAcks {
		if _, needed := current[ack.Service+":"+ack.InstanceID]; needed {
			kept = append(kept, ack)
		}
	}
	j.PriorAcks = kept
	if len(kept) == 0 {
		j.PriorExpected, j.PriorTargets, j.PriorRotationID = nil, nil, ""
	}
	for slot := range j.Bindings {
		j.Bindings[slot] = compactBindingHistory(*j, slot)
	}
}

func (c *Controller) bindingExistsLocked(containerID, instanceID string) bool {
	for _, bindings := range c.bindings {
		for _, prior := range bindings {
			if prior.ContainerID == containerID || prior.InstanceID == instanceID {
				return true
			}
		}
	}
	return false
}

func (c *Controller) bindingAckedLocked(slot string, binding InstanceBinding) bool {
	if ack, ok := c.acks[slotService(slot)][binding.InstanceID]; ok && ack.InstanceID == binding.InstanceID {
		return true
	}
	for _, ack := range c.priorAcks {
		if ack.Service == slotService(slot) && ack.InstanceID == binding.InstanceID {
			return true
		}
	}
	return false
}

// ReplaceReady gates a destructive Compose recreate before the old slot is removed.
func (c *Controller) ReplaceReady(slot, containerID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	history := c.bindings[slot]
	if slotService(slot) == "" || c.failureCode != "" || c.intent != "" || len(history) == 0 || history[len(history)-1].ContainerID != containerID ||
		!c.bindingAckedLocked(slot, history[len(history)-1]) {
		return errController
	}
	return nil
}

// BindingSnapshot exposes only container identity metadata to the root control socket.
func (c *Controller) BindingSnapshot() map[string]InstanceBinding {
	c.mu.Lock()
	defer c.mu.Unlock()
	current := make(map[string]InstanceBinding, len(c.bindings))
	for slot, history := range c.bindings {
		if len(history) > 0 {
			current[slot] = history[len(history)-1]
		}
	}
	return current
}

// PriorRegistrationProof exposes only prior host and binding evidence before fresh registration.
func (c *Controller) PriorRegistrationProof() (string, map[string][]string, []PriorAckProof) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.priorExpected) != 2 || len(c.expected) != 0 {
		return "", nil, nil
	}
	expected := make(map[string][]string, 2)
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		expected[service] = append([]string(nil), c.priorExpected[service]...)
		sort.Strings(expected[service])
	}
	proofs := c.priorAckProofsLocked()
	sort.Slice(proofs, func(i, j int) bool {
		if proofs[i].Service != proofs[j].Service {
			return proofs[i].Service < proofs[j].Service
		}
		return proofs[i].InstanceID < proofs[j].InstanceID
	})
	return c.priorRotationID, expected, proofs
}

func (c *Controller) priorAckProofsLocked() []PriorAckProof {
	proofs := make([]PriorAckProof, 0, len(c.priorAcks))
	for _, ack := range c.priorAcks {
		for slot, history := range c.bindings {
			if slotService(slot) != ack.Service {
				continue
			}
			for _, binding := range history {
				if binding.InstanceID == ack.InstanceID {
					proofs = append(proofs, PriorAckProof{Service: ack.Service, InstanceID: ack.InstanceID, Slot: slot, ContainerID: binding.ContainerID})
				}
			}
		}
	}
	return proofs
}

// Finalize revokes old users only after complete ACKs and connection drain.
func (c *Controller) Finalize(ctx context.Context, rotationID string, oldUsernames map[string]string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.validateFinalizeLocked(rotationID, oldUsernames); err != nil {
		return err
	}
	if err := c.drainOldClientsLocked(ctx, oldUsernames); err != nil {
		return err
	}
	if err := c.validateFinalizeLocked(rotationID, oldUsernames); err != nil {
		return errController
	}
	next := c.finalizedPolicyLocked()
	j := c.drainedJournalLocked(next, oldUsernames)
	if err := c.persistJournal(j); err != nil {
		return err
	}
	c.applyJournalLocked(j)
	if err := c.writePolicy(next); err != nil {
		c.failureCode = "reconcile"
		return err
	}
	if err := c.admin.LoadACL(ctx, c.aclPath); err != nil {
		c.failureCode = "reconcile"
		return errController
	}
	return c.finishRevocationLocked(ctx)
}

func (c *Controller) validateFinalizeLocked(rotationID string, oldUsernames map[string]string) error {
	if c.failureCode != "" || rotationID != c.policy.RotationID || len(oldUsernames) != 2 ||
		!c.validationFreshLocked() {
		return errController
	}
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		if c.validateFinalizeServiceLocked(service, oldUsernames[service]) != nil {
			return errController
		}
	}
	return nil
}

func (c *Controller) validateFinalizeServiceLocked(service, old string) error {
	policy := c.policy.Services[service]
	for _, generation := range policy.Generations {
		if generation.Generation == policy.TargetGeneration && generation.ValidUntil.Before(c.now().Add(MinimumOverlap)) {
			return errController
		}
	}
	if len(c.expected[service]) == 0 || len(c.acks[service]) != len(c.expected[service]) {
		return errController
	}
	for instance := range c.expected[service] {
		if c.acks[service][instance].Generation != policy.TargetGeneration || c.acks[service][instance].Username != policyTargetUsername(policy) {
			return errController
		}
	}
	if !policyHasOld(policy, old) {
		return errController
	}
	return nil
}

func (c *Controller) validationFreshLocked() bool {
	now := c.now()
	return !c.validatedAt.IsZero() && !now.Before(c.validatedAt) && now.Before(c.validatedAt.Add(5*time.Minute))
}

func oldClientPresent(clients []ClientIdentity, oldUsernames map[string]string) bool {
	for _, client := range clients {
		if client.Username == oldUsernames[controlPlaneService] || client.Username == oldUsernames[mediaPlaneService] {
			return true
		}
	}
	return false
}

func (c *Controller) drainOldClientsLocked(ctx context.Context, oldUsernames map[string]string) error {
	clients, err := c.admin.ListClients(ctx)
	if err != nil {
		return errController
	}
	if !c.validationFreshLocked() {
		return errController
	}
	if oldClientPresent(clients, oldUsernames) && (c.lastRequiredAckAt.IsZero() || c.now().Sub(c.lastRequiredAckAt) < 30*time.Second) {
		return errController
	}
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		old := oldUsernames[service]
		for _, client := range clients {
			if client.Username == old {
				if !c.validationFreshLocked() {
					return errController
				}
				if err := c.admin.KillUser(ctx, old); err != nil {
					return errController
				}
				break
			}
		}
	}
	clients, err = c.admin.ListClients(ctx)
	if err != nil {
		return errController
	}
	if oldClientPresent(clients, oldUsernames) {
		return errController
	}
	return nil
}

func (c *Controller) finalizedPolicyLocked() Policy {
	next := c.policy
	next.Services = make(map[string]PolicyService, 2)
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		current := c.policy.Services[service]
		for _, gen := range current.Generations {
			if gen.Generation == current.TargetGeneration {
				next.Services[service] = PolicyService{TargetGeneration: current.TargetGeneration, Generations: []PolicyGeneration{gen}}
			}
		}
	}
	return next
}

func (c *Controller) drainedJournalLocked(next Policy, oldUsernames map[string]string) rotationJournal {
	j := c.snapshotJournalLocked()
	j.Effective = next
	j.Phase = "drained"
	j.Intent = "revoke"
	j.FailureCode = "reconcile"
	j.OldUsernames = make(map[string]string, 2)
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		old := oldUsernames[service]
		floor := j.Revoked[service]
		for _, gen := range c.policy.Services[service].Generations {
			if gen.Username == old {
				if gen.Generation > floor.Generation {
					floor.Generation = gen.Generation
				}
				break
			}
		}
		j.Revoked[service] = floor
		j.OldUsernames[service] = old
	}
	return j
}

// ResumeFinalize retries a journaled deny intent after an ambiguous ACL load.
func (c *Controller) ResumeFinalize(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.intent != "revoke" || len(c.oldUsernames) != 2 {
		return errController
	}
	if err := c.writePolicy(c.policy); err != nil {
		c.failureCode = "reconcile"
		return err
	}
	if err := c.admin.LoadACL(ctx, c.aclPath); err != nil {
		c.failureCode = "reconcile"
		return errController
	}
	return c.finishRevocationLocked(ctx)
}

func (c *Controller) finishRevocationLocked(ctx context.Context) error {
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		present, err := c.admin.UserExists(ctx, c.oldUsernames[service])
		if err != nil || present {
			c.failureCode = "reconcile"
			return errController
		}
		target := c.policy.Services[service].Generations[0].Username
		present, err = c.admin.UserExists(ctx, target)
		if err != nil || !present {
			c.failureCode = "reconcile"
			return errController
		}
	}
	clients, err := c.admin.ListClients(ctx)
	if err != nil {
		c.failureCode = "reconcile"
		return errController
	}
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		for _, client := range clients {
			if client.Username == c.oldUsernames[service] {
				if err := c.admin.KillUser(ctx, c.oldUsernames[service]); err != nil {
					c.failureCode = "reconcile"
					return errController
				}
				break
			}
		}
	}
	clients, err = c.admin.ListClients(ctx)
	if err != nil {
		c.failureCode = "reconcile"
		return errController
	}
	if oldClientPresent(clients, c.oldUsernames) {
		c.failureCode = "reconcile"
		return errController
	}
	j := c.snapshotJournalLocked()
	j.Phase = "finalized"
	j.Intent = ""
	j.OldUsernames = nil
	j.FailureCode = ""
	if err := c.persistJournal(j); err != nil {
		c.failureCode = "reconcile"
		return err
	}
	c.applyJournalLocked(j)
	return nil
}

// ReportFailure exposes only bounded source categories while retaining the
// last loaded ACL, policy and acknowledged progress.
func (c *Controller) ReportFailure(code string) {
	if code != "provider" && code != "policy" && code != "policy-stale" && code != "reconcile" {
		code = "reconcile"
	}
	c.mu.Lock()
	c.failureCode = code
	c.mu.Unlock()
}

func policyHasOld(service PolicyService, username string) bool {
	for _, gen := range service.Generations {
		if gen.Username == username && gen.Generation != service.TargetGeneration {
			return true
		}
	}
	return false
}

func policyTargetUsername(service PolicyService) string {
	for _, generation := range service.Generations {
		if generation.Generation == service.TargetGeneration {
			return generation.Username
		}
	}
	return ""
}

func (c *Controller) writePolicy(policy Policy) error {
	lines := []string{"user default off resetpass -@all"}
	// Redis ACL files require SHA-256 password digests; a password KDF is incompatible with AUTH.
	hash := sha256.Sum256([]byte(c.bootstrap.Password))
	lines = append(lines, "user "+c.bootstrap.Username+" on resetpass #"+hex.EncodeToString(hash[:])+" -@all +auth +hello +ping +client|setinfo +acl|load +acl|getuser +client|list +client|kill "+operatorACLRule())
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		for _, gen := range policy.Services[service].Generations {
			lines = append(lines, fmt.Sprintf("user %s on resetpass %s -@all %s", gen.Username, gen.PasswordHash, serviceACLRule(service)))
		}
	}
	return c.writeACL([]byte(strings.Join(lines, "\n") + "\n"))
}

func (c *Controller) writeACL(contents []byte) (retErr error) {
	dir := filepath.Dir(c.aclPath)
	file, err := os.CreateTemp(dir, ".users-acl-*")
	if err != nil {
		return errController
	}
	defer func() {
		// A successful rename removes the temporary pathname.
		if err := os.Remove(file.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			retErr = errController
		}
	}()
	if err := file.Chmod(0o440); err != nil {
		if file.Close() != nil {
			return errController
		}
		return errController
	}
	info, err := file.Stat()
	if err != nil {
		if file.Close() != nil {
			return errController
		}
		return errController
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	uid, gid := c.aclUID, c.aclGID
	if uid == 0 && gid == 0 {
		uid, gid = os.Geteuid(), os.Getegid()
	}
	if !ok || int(stat.Uid) != uid || int(stat.Gid) != gid {
		if file.Close() != nil {
			return errController
		}
		return errController
	}
	if _, err := file.Write(contents); err != nil {
		if file.Close() != nil {
			return errController
		}
		return errController
	}
	if err := file.Sync(); err != nil {
		if file.Close() != nil {
			return errController
		}
		return errController
	}
	if err := file.Close(); err != nil {
		return errController
	}
	if err := os.Rename(file.Name(), c.aclPath); err != nil {
		return errController
	}
	return nil
}

// Status returns the bounded phase, rotation, and target generations.
func (c *Controller) Status() (string, string, map[string]uint64) {
	state, rotationID, targets, _ := c.StatusSnapshot()
	return state, rotationID, targets
}

// StatusSnapshot includes only registered instance identities, never ACL data.
// A nil instances map means the expected set is not yet known.
func (c *Controller) StatusSnapshot() (string, string, map[string]uint64, map[string][]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	targets := map[string]uint64{}
	for service, policy := range c.policy.Services {
		targets[service] = policy.TargetGeneration
	}
	var instances map[string][]string
	if len(c.expected) == 2 {
		instances = make(map[string][]string, 2)
		for _, service := range []string{controlPlaneService, mediaPlaneService} {
			for host := range c.expected[service] {
				instances[service] = append(instances[service], host)
			}
			sort.Strings(instances[service])
		}
	}
	state := c.state
	if c.failureCode != "" {
		state = "failed"
	}
	return state, c.policy.RotationID, targets, instances
}
