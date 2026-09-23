package rbac

import "database/sql"

// SetAuthorityCommitForTest injects an acknowledgement-lost commit for an
// external integration test while leaving production construction unchanged.
func SetAuthorityCommitForTest(h *Handler, commit func(*sql.Tx) error) {
	h.authorityCommit = commit
}

// SetSyncedCategoryPreflightForTest observes the category child-set read before
// the visibility lock is acquired, making a set-race regression deterministic.
func SetSyncedCategoryPreflightForTest(h *Handler, observe func()) {
	h.syncedCategoryPreflight = observe
}
