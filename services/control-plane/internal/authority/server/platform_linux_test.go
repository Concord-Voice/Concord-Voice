//go:build linux

package server

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// F2's ACL rule through a real system.posix_acl_access (DoR:1649, 1651). It MUST
// NOT skip in CI: a filesystem without ACLs fails the test.
func TestCredentialACLIsEnforced(t *testing.T) {
	dir := t.TempDir()
	euid, err := geteuid()
	if err != nil {
		t.Fatal(err)
	}
	const undef = 0xffffffff
	set := func(name string, named ...[3]uint32) {
		writeCred(t, dir, name, []byte("key"), 0o400)
		entries := append([][3]uint32{{0x01, 4, undef}, {0x04, 0, undef}, {0x10, 0, undef}, {0x20, 0, undef}}, named...)
		// The kernel refuses an ACL out of tag order (posix_acl_valid):
		// USER_OBJ, USER, GROUP_OBJ, GROUP, MASK, OTHER is ascending tag value.
		slices.SortFunc(entries, func(a, b [3]uint32) int { return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[2], b[2])) })
		if err := syscall.Setxattr(filepath.Join(dir, name), aclXattr, aclBytes(entries...), 0); err != nil {
			t.Fatalf("setxattr: %v (the test filesystem must support POSIX ACLs)", err)
		}
	}
	set("own", [3]uint32{aclTagUser, 4, euid})
	if _, err := readCredential(dir, "own"); err != nil {
		t.Fatalf("an ACL naming only the euid: %v", err)
	}
	set("other", [3]uint32{aclTagUser, 4, euid + 1})
	if _, err := readCredential(dir, "other"); err == nil {
		t.Fatal("an ACL granting another user passed")
	}
	set("group", [3]uint32{aclTagGroup, 4, 0})
	if _, err := readCredential(dir, "group"); err == nil {
		t.Fatal("an ACL granting a named group passed")
	}
	// group::r-- under mask::--- leaves the mode's group bits
	// clear, so only the ACL rule refuses it.
	writeCred(t, dir, "groupobj", []byte("key"), 0o400)
	if err := syscall.Setxattr(filepath.Join(dir, "groupobj"), aclXattr,
		aclBytes([3]uint32{0x01, 4, undef}, [3]uint32{aclTagGroupObj, 4, undef}, [3]uint32{0x10, 0, undef}, [3]uint32{0x20, 0, undef}), 0); err != nil {
		t.Fatalf("setxattr: %v", err)
	}
	if _, err := readCredential(dir, "groupobj"); err == nil {
		t.Fatal("an ACL granting the owning group under an empty mask passed")
	}
}

// The passfile is judged as the credentials are, ACL included.
// Each ACL keeps the mask empty, so the mode reads 0600 and only the ACL rule
// can refuse; the euid-only ACL is the control. It MUST NOT skip in CI.
func TestPassfileACLIsEnforced(t *testing.T) {
	dir := t.TempDir()
	euid, err := geteuid()
	if err != nil {
		t.Fatal(err)
	}
	const undef = 0xffffffff
	put := func(name string, extra ...[3]uint32) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("*:*:*:authority_app:x\n"), 0o600); err != nil { // pragma: allowlist secret
			t.Fatal(err)
		}
		entries := append([][3]uint32{{0x01, 6, undef}, {0x04, 0, undef}, {0x10, 0, undef}, {0x20, 0, undef}}, extra...)
		slices.SortFunc(entries, func(a, b [3]uint32) int { return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[2], b[2])) })
		if err := syscall.Setxattr(p, aclXattr, aclBytes(entries...), 0); err != nil {
			t.Fatalf("setxattr: %v (the test filesystem must support POSIX ACLs)", err)
		}
		return p
	}
	if err := checkPassfile(put("own", [3]uint32{aclTagUser, 6, euid})); err != nil {
		t.Fatalf("control: an ACL naming only the euid: %v", err)
	}
	if err := checkPassfile(put("other", [3]uint32{aclTagUser, 4, euid + 1})); err == nil ||
		!strings.Contains(err.Error(), "ACL") || strings.Contains(err.Error(), dir) {
		t.Fatalf("an ACL granting another user: %v, want an ACL refusal without the path", err)
	}
}

func TestStatfsAndClock(t *testing.T) {
	avail, total, err := statfs(t.TempDir())
	if err != nil || total == 0 || avail > total {
		t.Fatalf("statfs: %d of %d, %v", avail, total, err)
	}
	if _, err := clockSynced(); err != nil { // the value depends on the runner; the read must not fail
		t.Fatalf("clockSynced: %v", err)
	}
}

// prGetDumpable is <linux/prctl.h>'s PR_GET_DUMPABLE.
const prGetDumpable = 3

// serve's start, Open, leaves the process non-dumpable, and the
// credentials it then reads through /proc/self/fd still pass (a
// non-dumpable process's /proc entries turn root-owned; its own fds stay
// readable to it). The test process stays non-dumpable afterwards.
func TestOpenMakesTheProcessUndumpable(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	_, closeFn, err := Open(openConfig(t), quiet)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer closeFn()
	r, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prGetDumpable, 0, 0)
	if e != 0 || r != 0 {
		t.Fatalf("PR_GET_DUMPABLE after Open: %d, %v; want 0", r, e)
	}
}
