package managedredis

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const maxJournalBytes = 128 * 1024

type revokedIdentity struct {
	Generation uint64 `json:"generation"`
}

type rotationJournal struct {
	Version           int                          `json:"version"`
	Effective         Policy                       `json:"effective"`
	Phase             string                       `json:"phase"`
	Intent            string                       `json:"intent"`
	Revoked           map[string]revokedIdentity   `json:"revoked"`
	Expected          map[string][]string          `json:"expected,omitempty"`
	Acks              []AppliedAck                 `json:"acks,omitempty"`
	Bindings          map[string][]InstanceBinding `json:"bindings,omitempty"`
	PriorExpected     map[string][]string          `json:"priorExpected,omitempty"`
	PriorAcks         []AppliedAck                 `json:"priorAcks,omitempty"`
	PriorRotationID   string                       `json:"priorRotationId,omitempty"`
	PriorTargets      map[string]uint64            `json:"priorTargets,omitempty"`
	RecoveryAt        time.Time                    `json:"recoveryAt,omitempty"`
	ValidatedAt       time.Time                    `json:"validatedAt"`
	LastRequiredAckAt time.Time                    `json:"lastRequiredAckAt,omitempty"`
	OldUsernames      map[string]string            `json:"oldUsernames,omitempty"`
	FailureCode       string                       `json:"failureCode,omitempty"`
}

func checkedPrivateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return errController
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return errController
	}
	return nil
}

func checkedJournalFile(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() < 1 || info.Size() > maxJournalBytes {
		return false, errController
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return false, errController
	}
	return true, nil
}

func decodeJournal(raw []byte, journal *rotationJournal) error {
	if len(raw) == 0 || len(raw) > maxJournalBytes {
		return errController
	}
	check := json.NewDecoder(bytes.NewReader(raw))
	if err := rejectDuplicateKeys(check); err != nil {
		return errController
	}
	if _, err := check.Token(); !errors.Is(err, io.EOF) {
		return errController
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(journal); err != nil {
		return errController
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errController
	}
	return nil
}

func (c *Controller) validateJournal(j rotationJournal) error {
	if !validJournalHeader(j) {
		return errController
	}
	if j.ValidatedAt.IsZero() || j.ValidatedAt.After(c.now()) {
		return errController
	}
	if err := c.validatePolicyAt(j.Effective, j.ValidatedAt); err != nil {
		return err
	}
	if err := validateJournalRevoked(j); err != nil {
		return err
	}
	if err := validateJournalIntent(j); err != nil {
		return err
	}
	if err := validateJournalExpected(j); err != nil {
		return err
	}
	if err := validateJournalBindings(j); err != nil {
		return err
	}
	if err := validateJournalAcks(j); err != nil {
		return err
	}
	return validateJournalPriorAcks(j)
}

func validJournalHeader(j rotationJournal) bool {
	return j.Version == 1 && (j.Phase == "prepared" || j.Phase == "promoted" || j.Phase == "drained" || j.Phase == "finalized") &&
		(j.Intent == "" || j.Intent == "reconcile" || j.Intent == "revoke" || j.Intent == "recovery") &&
		(j.FailureCode == "" || j.FailureCode == "provider" || j.FailureCode == "policy" || j.FailureCode == "policy-stale" || j.FailureCode == "reconcile") &&
		len(j.Revoked) == 2 && len(j.Acks) <= 128 && len(j.PriorAcks) <= 128
}

func validateJournalRevoked(j rotationJournal) error {
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		floor, ok := j.Revoked[service]
		if !ok || floor.Generation >= j.Effective.Services[service].TargetGeneration {
			return errController
		}
		for _, generation := range j.Effective.Services[service].Generations {
			if generation.Generation <= floor.Generation {
				return errController
			}
		}
	}
	return nil
}

func validateJournalIntent(j rotationJournal) error {
	if err := validateJournalRevokeIntent(j); err != nil {
		return err
	}
	if j.Intent == "recovery" && (!singleGenerationPolicy(j.Effective) || j.RecoveryAt.IsZero()) ||
		j.Phase == "finalized" && !singleGenerationPolicy(j.Effective) {
		return errController
	}
	return nil
}

func validateJournalRevokeIntent(j rotationJournal) error {
	if j.Intent == "revoke" && (j.Phase != "drained" || len(j.OldUsernames) != 2) || j.Intent != "revoke" && len(j.OldUsernames) != 0 {
		return errController
	}
	if j.Intent != "revoke" {
		return nil
	}
	if !singleGenerationPolicy(j.Effective) {
		return errController
	}
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		old := j.OldUsernames[service]
		if !validRevokedPriorUsername(service, old, j.Effective.Services[service].Generations[0].Username, j.Revoked[service].Generation) {
			return errController
		}
	}
	return nil
}

func validRevokedPriorUsername(service, old, current string, floor uint64) bool {
	return old != current && canonicalUsernameBefore(service, floor, old)
}

func validateJournalExpected(j rotationJournal) error {
	if len(j.Expected) != 0 && len(j.Expected) != 2 {
		return errController
	}
	if len(j.PriorExpected) != 0 && len(j.PriorExpected) != 2 {
		return errController
	}
	if len(j.PriorExpected) > 0 && (!rotationID.MatchString(j.PriorRotationID) || len(j.PriorTargets) != 2) {
		return errController
	}
	if len(j.PriorExpected) == 0 && (j.PriorRotationID != "" || len(j.PriorTargets) != 0 || len(j.PriorAcks) != 0) {
		return errController
	}
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		if err := validateJournalExpectedService(j, service); err != nil {
			return err
		}
	}
	return nil
}

func validateJournalExpectedService(j rotationJournal, service string) error {
	if len(j.Expected) > 0 && (len(j.Expected[service]) == 0 || len(j.Expected[service]) > 64) {
		return errController
	}
	if err := validateJournalHosts(j.Expected[service]); err != nil {
		return err
	}
	if len(j.PriorExpected[service]) > 64 {
		return errController
	}
	if len(j.PriorExpected) > 0 && j.PriorTargets[service] == 0 {
		return errController
	}
	return validateJournalHosts(j.PriorExpected[service])
}

func validateJournalHosts(hosts []string) error {
	seen := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		if !validInstanceID(host) {
			return errController
		}
		if _, duplicate := seen[host]; duplicate {
			return errController
		}
		seen[host] = struct{}{}
	}
	return nil
}

func validateJournalBindings(j rotationJournal) error {
	if len(j.Bindings) > 4 {
		return errController
	}
	seenContainers := make(map[string]struct{})
	seenHosts := make(map[string]struct{})
	for slot, history := range j.Bindings {
		if slotService(slot) == "" || len(history) == 0 || len(history) > 64 {
			return errController
		}
		for _, binding := range history {
			if err := validateJournalBinding(binding, seenContainers, seenHosts); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateJournalBinding(binding InstanceBinding, seenContainers, seenHosts map[string]struct{}) error {
	if len(binding.ContainerID) != 64 || !boundHostname.MatchString(binding.InstanceID) ||
		strings.ToLower(binding.ContainerID) != binding.ContainerID {
		return errController
	}
	if _, err := hex.DecodeString(binding.ContainerID); err != nil {
		return errController
	}
	if _, exists := seenContainers[binding.ContainerID]; exists {
		return errController
	}
	if _, exists := seenHosts[binding.InstanceID]; exists {
		return errController
	}
	seenContainers[binding.ContainerID] = struct{}{}
	seenHosts[binding.InstanceID] = struct{}{}
	return nil
}

func validateJournalAcks(j rotationJournal) error {
	seenAck := make(map[string]struct{}, len(j.Acks))
	for _, ack := range j.Acks {
		if (ack.Service != controlPlaneService && ack.Service != mediaPlaneService) || ack.Version != 1 || ack.RotationID != j.Effective.RotationID || ack.Generation != j.Effective.Services[ack.Service].TargetGeneration || ack.Username != policyTargetUsername(j.Effective.Services[ack.Service]) {
			return errController
		}
		if _, expected := slicesContains(j.Expected[ack.Service], ack.InstanceID); !expected {
			return errController
		}
		if !journalAckBound(j, ack) {
			return errController
		}
		key := ack.Service + ":" + ack.InstanceID
		if _, duplicate := seenAck[key]; duplicate {
			return errController
		}
		seenAck[key] = struct{}{}
	}
	return nil
}

func validateJournalPriorAcks(j rotationJournal) error {
	for _, ack := range j.PriorAcks {
		if (ack.Service != controlPlaneService && ack.Service != mediaPlaneService) || ack.Version != 1 || ack.RotationID != j.PriorRotationID || !validInstanceID(ack.InstanceID) || ack.Generation != j.PriorTargets[ack.Service] || !aclUsername.MatchString(ack.Username) {
			return errController
		}
		if _, expected := slicesContains(j.PriorExpected[ack.Service], ack.InstanceID); !expected {
			return errController
		}
		if !journalAckBound(j, ack) {
			return errController
		}
	}
	return nil
}

func journalAckBound(j rotationJournal, ack AppliedAck) bool {
	for slot, history := range j.Bindings {
		if slotService(slot) != ack.Service {
			continue
		}
		for _, binding := range history {
			if binding.InstanceID == ack.InstanceID {
				return true
			}
		}
	}
	return false
}

func slicesContains(values []string, needle string) (int, bool) {
	for i, value := range values {
		if value == needle {
			return i, true
		}
	}
	return -1, false
}

func (c *Controller) persistJournal(j rotationJournal) (retErr error) {
	if err := c.validateJournal(j); err != nil {
		return err
	}
	exists, err := checkedJournalFile(c.journalPath)
	if err != nil || !exists && c.policy.Version != 0 {
		return errController
	}
	raw, err := json.Marshal(j)
	if err != nil || len(raw) > maxJournalBytes {
		return errController
	}
	dir := filepath.Dir(c.journalPath)
	if err := checkedPrivateDir(dir); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".rotation-*")
	if err != nil {
		return errController
	}
	defer func() {
		if err := os.Remove(file.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			retErr = errController
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return closeFailedJournal(file)
	}
	if _, err := file.Write(raw); err != nil {
		return closeFailedJournal(file)
	}
	if err := file.Sync(); err != nil {
		return closeFailedJournal(file)
	}
	if err := file.Close(); err != nil {
		return errController
	}
	if err := os.Rename(file.Name(), c.journalPath); err != nil {
		return errController
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return errController
	}
	directory, err := root.Open(".")
	if err != nil {
		return closeFailedRoot(root)
	}
	if err := directory.Sync(); err != nil {
		if directory.Close() != nil {
			return errController
		}
		return closeFailedRoot(root)
	}
	if err := directory.Close(); err != nil {
		return errController
	}
	if err := root.Close(); err != nil {
		return errController
	}
	return nil
}

func closeFailedRoot(root *os.Root) error {
	if err := root.Close(); err != nil {
		return errController
	}
	return errController
}

func closeFailedJournal(file *os.File) error {
	if err := file.Close(); err != nil {
		return errController
	}
	return errController
}

func (c *Controller) snapshotJournalLocked() rotationJournal {
	j := rotationJournal{Version: 1, Effective: c.policy, Phase: c.state, Intent: c.intent,
		Revoked: make(map[string]revokedIdentity, 2), FailureCode: c.failureCode,
		LastRequiredAckAt: c.lastRequiredAckAt, RecoveryAt: c.recoveryAt, ValidatedAt: c.validatedAt}
	for service, floor := range c.revoked {
		j.Revoked[service] = revokedIdentity{Generation: floor.Generation}
	}
	j.Expected = c.snapshotExpectedLocked()
	j.Acks = c.snapshotAcksLocked()
	for service, username := range c.oldUsernames {
		if j.OldUsernames == nil {
			j.OldUsernames = make(map[string]string, 2)
		}
		j.OldUsernames[service] = username
	}
	if len(c.priorExpected) == 2 {
		j.PriorExpected = make(map[string][]string, 2)
		for service, hosts := range c.priorExpected {
			j.PriorExpected[service] = append([]string(nil), hosts...)
		}
	}
	j.PriorAcks = append([]AppliedAck(nil), c.priorAcks...)
	j.Bindings = c.snapshotBindingsLocked()
	j.PriorRotationID = c.priorRotationID
	if len(c.priorTargets) == 2 {
		j.PriorTargets = map[string]uint64{controlPlaneService: c.priorTargets[controlPlaneService], mediaPlaneService: c.priorTargets[mediaPlaneService]}
	}
	return j
}

func (c *Controller) snapshotExpectedLocked() map[string][]string {
	if len(c.expected) == 2 {
		expected := make(map[string][]string, 2)
		for _, service := range []string{controlPlaneService, mediaPlaneService} {
			for host := range c.expected[service] {
				expected[service] = append(expected[service], host)
			}
			sort.Strings(expected[service])
		}
		return expected
	}
	return nil
}

func (c *Controller) snapshotAcksLocked() []AppliedAck {
	var acks []AppliedAck
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		for _, ack := range c.acks[service] {
			acks = append(acks, ack)
		}
	}
	sort.Slice(acks, func(i, k int) bool {
		if acks[i].Service == acks[k].Service {
			return acks[i].InstanceID < acks[k].InstanceID
		}
		return acks[i].Service < acks[k].Service
	})
	return acks
}

func (c *Controller) snapshotBindingsLocked() map[string][]InstanceBinding {
	if len(c.bindings) > 0 {
		bindings := make(map[string][]InstanceBinding, len(c.bindings))
		for slot, history := range c.bindings {
			bindings[slot] = append([]InstanceBinding(nil), history...)
		}
		return bindings
	}
	return nil
}

func (c *Controller) applyJournalLocked(j rotationJournal) {
	c.policy, c.state, c.intent, c.failureCode = j.Effective, j.Phase, j.Intent, j.FailureCode
	c.revoked = j.Revoked
	c.lastRequiredAckAt = j.LastRequiredAckAt
	c.oldUsernames = j.OldUsernames
	c.priorExpected = j.PriorExpected
	c.priorAcks = j.PriorAcks
	c.bindings = j.Bindings
	c.priorRotationID = j.PriorRotationID
	c.priorTargets = j.PriorTargets
	c.recoveryAt = j.RecoveryAt
	c.validatedAt = j.ValidatedAt
	c.expected = nil
	if len(j.Expected) == 2 {
		c.expected = make(map[string]map[string]struct{}, 2)
		for service, hosts := range j.Expected {
			c.expected[service] = make(map[string]struct{}, len(hosts))
			for _, host := range hosts {
				c.expected[service][host] = struct{}{}
			}
		}
	}
	c.acks = make(map[string]map[string]AppliedAck, 2)
	for _, ack := range j.Acks {
		if c.acks[ack.Service] == nil {
			c.acks[ack.Service] = make(map[string]AppliedAck)
		}
		c.acks[ack.Service][ack.InstanceID] = ack
	}
}
