package store

import "context"

// InsertLegacyPopulation is PR-B3's; B1 returns ErrNotImplemented (B1-D6).
func (s *Store) InsertLegacyPopulation(context.Context, []byte) error { return ErrNotImplemented }

// ReconcileRequired is PR-B3's; B1 returns ErrNotImplemented (B1-D6).
func (s *Store) ReconcileRequired(context.Context, []byte) error { return ErrNotImplemented }

// ReconcileImport is PR-B3's; B1 returns ErrNotImplemented (B1-D6).
func (s *Store) ReconcileImport(context.Context, []byte) (Outcome, error) {
	return Outcome{}, ErrNotImplemented
}

// ReconcileHold is PR-B3's; B1 returns ErrNotImplemented (B1-D6).
func (s *Store) ReconcileHold(context.Context, []byte) (Outcome, error) {
	return Outcome{}, ErrNotImplemented
}

// RestoreBegin is PR-B3's; B1 returns ErrNotImplemented (B1-D6).
func (s *Store) RestoreBegin(context.Context) error { return ErrNotImplemented }

// ReconcileComplete is PR-B3's; B1 returns ErrNotImplemented (B1-D6).
func (s *Store) ReconcileComplete(context.Context) error { return ErrNotImplemented }

// ClearHeld is PR-B3's; B1 returns ErrNotImplemented (B1-D6).
func (s *Store) ClearHeld(context.Context, string) error { return ErrNotImplemented }

// SealLegacy is PR-B3's; B1 returns ErrNotImplemented (B1-D6).
func (s *Store) SealLegacy(context.Context, []byte) error { return ErrNotImplemented }

// EndDrain is PR-B3's; B1 returns ErrNotImplemented (B1-D6).
func (s *Store) EndDrain(context.Context, uint64, uint64) error { return ErrNotImplemented }

// RevokeKey is PR-B3's; B1 returns ErrNotImplemented (B1-D6).
func (s *Store) RevokeKey(context.Context, string, []byte) error { return ErrNotImplemented }

// ReplicationGate is PR-B3's; B1 returns ErrNotImplemented (B1-D6).
func (s *Store) ReplicationGate(context.Context, bool) error { return ErrNotImplemented }
