package managedturn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"syscall"
)

const (
	stateVersion  = 1
	maxStateBytes = 64 << 10

	// PhaseActive means every key in the set has been proven.
	PhaseActive = "active"
	// PhaseCandidate means the set holds one key that has never been proven.
	PhaseCandidate = "candidate"
)

var keyIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// State is the durable, non-secret coordinator record (root, 0600).
type State struct {
	Version     int      `json:"version"`
	Generation  uint64   `json:"generation"`
	ActiveKeyID string   `json:"active_key_id"`
	KeyIDs      []string `json:"key_ids"`
	Phase       string   `json:"phase"`
}

// validate checks the structural invariants of a persisted record.
func (s State) validate() error {
	switch {
	case s.Version != stateVersion:
		return fmt.Errorf("%w: unsupported version", ErrInvalidState)
	case s.Generation == 0:
		return fmt.Errorf("%w: generation must be at least 1", ErrInvalidState)
	case len(s.KeyIDs) == 0 || len(s.KeyIDs) > MaxKeys:
		return fmt.Errorf("%w: key set size outside 1..%d", ErrInvalidState, MaxKeys)
	case s.Phase != PhaseActive && s.Phase != PhaseCandidate:
		return fmt.Errorf("%w: unknown phase", ErrInvalidState)
	case s.Phase == PhaseCandidate && len(s.KeyIDs) != MaxKeys:
		return fmt.Errorf("%w: candidate phase without a candidate", ErrInvalidState)
	}
	seen := make(map[string]bool, len(s.KeyIDs))
	for _, id := range s.KeyIDs {
		if !keyIDPattern.MatchString(id) || seen[id] {
			return fmt.Errorf("%w: malformed or duplicate key id", ErrInvalidState)
		}
		seen[id] = true
	}
	if !seen[s.ActiveKeyID] {
		return fmt.Errorf("%w: active key id not in key set", ErrInvalidState)
	}
	return nil
}

// hasKey reports whether id is in the recorded set.
func (s State) hasKey(id string) bool { return slices.Contains(s.KeyIDs, id) }

// LoadState reads path; ok is false when it does not exist. A symlink,
// non-regular or group/other-writable file, an oversized file, malformed JSON,
// unknown fields or a structurally invalid record are all errors.
func LoadState(path string) (state State, ok bool, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0) // #nosec G304 -- configured state path
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, false, nil
	}
	if errors.Is(err, syscall.ELOOP) {
		return State{}, false, ErrUnsafePermission
	}
	if err != nil {
		return State{}, false, fmt.Errorf("managedturn: open state: %w", err)
	}
	defer func() { err = errors.Join(err, closeFile(f)) }()
	info, err := f.Stat()
	if err != nil {
		return State{}, false, fmt.Errorf("managedturn: stat state: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return State{}, false, ErrUnsafePermission
	}
	data, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil {
		return State{}, false, fmt.Errorf("managedturn: read state: %w", err)
	}
	if len(data) > maxStateBytes {
		return State{}, false, fmt.Errorf("%w: state exceeds %d bytes", ErrInvalidState, maxStateBytes)
	}
	st, err := decodeState(data)
	if err != nil {
		return State{}, false, err
	}
	return st, true, nil
}

// decodeState parses exactly one JSON object with no unknown fields.
func decodeState(data []byte) (State, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var st State
	if err := dec.Decode(&st); err != nil {
		return State{}, fmt.Errorf("%w: malformed JSON", ErrInvalidState)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return State{}, fmt.Errorf("%w: trailing data", ErrInvalidState)
	}
	if err := st.validate(); err != nil {
		return State{}, err
	}
	return st, nil
}

// SaveState writes next only if the file still holds prevGeneration
// (journal-first temp file, fsync, rename, directory fsync). A missing file
// holds generation 0. next must be valid and keep or advance the generation by
// exactly one. It serialises with every other writer through an exclusive
// flock on the state directory.
func SaveState(path string, prevGeneration uint64, next State) error {
	lock, err := lockDir(context.Background(), filepath.Dir(path))
	if err != nil {
		return err
	}
	defer lock.release()
	return saveStateLocked(path, prevGeneration, next)
}

// saveStateLocked is SaveState for a caller that already holds the lock.
func saveStateLocked(path string, prevGeneration uint64, next State) error {
	if err := next.validate(); err != nil {
		return err
	}
	if next.Generation != prevGeneration && next.Generation != prevGeneration+1 {
		return fmt.Errorf("%w: generation may only stay or advance by one", ErrInvalidState)
	}
	cur, ok, err := LoadState(path)
	if err != nil {
		return err
	}
	if !ok {
		cur.Generation = 0
	}
	if cur.Generation != prevGeneration {
		return ErrStaleGeneration
	}
	next.KeyIDs = slices.Clone(next.KeyIDs)
	data, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("managedturn: encode state: %w", err)
	}
	return writeFileAtomic(filepath.Dir(path), filepath.Base(path), append(data, '\n'), 0o600, -1)
}
