//go:build !linux

package server

import (
	"errors"
	"os"
)

// errNotLinux: serve, and a named passfile's ACL check, run only on Linux;
// elsewhere they exit 1, reason=config (DoR:1649, B2-D16).
var errNotLinux = errors.New("the account authority runs only on Linux")

func aclOnlyOwner(*os.File, uint32) error   { return errNotLinux }
func statfs(string) (uint64, uint64, error) { return 0, 0, errNotLinux }
func clockSynced() (bool, error)            { return false, errNotLinux }

// makeUndumpable has nothing to do off Linux: Open refuses there at the
// first credential read, before any key is in memory (B2-D16).
func makeUndumpable() error { return nil }
