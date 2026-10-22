//go:build linux

package server

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// staUnsync is <linux/timex.h>'s STA_UNSYNC.
const staUnsync = 0x0040

// prSetDumpable is <linux/prctl.h>'s PR_SET_DUMPABLE.
const prSetDumpable = 4

// makeUndumpable clears the process's dumpable flag, PR_SET_DUMPABLE 0
// (CWE-528): the kernel then writes no core for serve under the
// default fs.suid_dumpable, and a process of the same uid can neither ptrace
// it nor read its /proc/<pid>/mem, so neither reaches the RSK or the TLS
// key. fs.suid_dumpable=2 still writes a root-only core: #3593's unit sets
// LimitCORE=0 (DoR §3.3).
func makeUndumpable() error {
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); e != 0 {
		return fmt.Errorf("prctl PR_SET_DUMPABLE: %w", e)
	}
	return nil
}

// aclOnlyOwner reads the open file's ACL through /proc/self/fd, so the check
// and the read are the same file. No ACL, or a filesystem without ACLs,
// passes.
func aclOnlyOwner(f *os.File, euid uint32) error {
	buf := make([]byte, 1024)
	n, err := syscall.Getxattr(fmt.Sprintf("/proc/self/fd/%d", f.Fd()), aclXattr, buf)
	if errors.Is(err, syscall.ENODATA) || errors.Is(err, syscall.ENOTSUP) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("the ACL: %w", err)
	}
	return checkACL(buf[:n], euid)
}

// statfs is free and total bytes as Bavail × Frsize and Blocks × Frsize:
// blocks reserved for root are not usable by postgres (DoR:196).
func statfs(path string) (avail, total uint64, err error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, 0, err
	}
	if s.Frsize <= 0 {
		return 0, 0, errors.New("statfs reports no fragment size")
	}
	frsize := uint64(s.Frsize)
	return s.Bavail * frsize, s.Blocks * frsize, nil
}

// clockSynced is the kernel's view, STA_UNSYNC clear (DoR:1786). A read
// that fails is returned, not folded into "unsynced" here: clockWatch logs
// it.
func clockSynced() (bool, error) {
	var tx syscall.Timex
	if _, err := syscall.Adjtimex(&tx); err != nil {
		return false, fmt.Errorf("adjtimex: %w", err)
	}
	return tx.Status&staUnsync == 0, nil
}
