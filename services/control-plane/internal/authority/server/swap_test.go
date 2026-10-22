package server

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M4 (fix round 1): the credential and trust-input readers read only from
// what they judged. Each case swaps something at swapHook, between a check
// and the open it guards, for a decoy that passes every rule, and the read
// must refuse rather than return the decoy.

// swapReaders are the two readers, each reading dir/f.
var swapReaders = map[string]func(dir string) ([]byte, error){
	"credential": func(dir string) ([]byte, error) { return readCredential(dir, "f") },
	"trust file": func(dir string) ([]byte, error) { return readTrustFile("V", filepath.Join(dir, "f"), 64) },
}

// onSwap runs swap once, at stage, and restores swapHook at cleanup.
func onSwap(t *testing.T, stage string, swap func() error) {
	t.Helper()
	hook := swapHook
	t.Cleanup(func() { swapHook = hook })
	swapHook = func(s string) {
		if s == stage {
			if err := swap(); err != nil {
				t.Error(err)
			}
		}
	}
}

// swapDir is a 0700 directory holding f, which reads as body.
func swapDir(t *testing.T, p, body string) {
	t.Helper()
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCred(t, p, "f", []byte(body), 0o400)
}

// The parent swapped between its Lstat and its open: renamed away with a
// decoy renamed into its place, or with a symlink to the decoy there.
func TestReadersRefuseASwappedDirectory(t *testing.T) {
	for rname, read := range swapReaders {
		for how, put := range map[string]func(decoy, dir string) error{"renamed": os.Rename, "symlinked": os.Symlink} {
			t.Run(rname+"/"+how, func(t *testing.T) {
				base := t.TempDir()
				dir, decoy := filepath.Join(base, "d"), filepath.Join(base, "decoy")
				swapDir(t, dir, "real")
				swapDir(t, decoy, "decoy")
				onSwap(t, "dir", func() error {
					if err := os.Rename(dir, filepath.Join(base, "old")); err != nil {
						return err
					}
					return put(decoy, dir)
				})
				if b, err := read(dir); !errors.Is(err, errDirSwapped) {
					t.Fatalf("read %q, %v; want errDirSwapped", b, err)
				}
			})
		}
	}
}

// f swapped between its Lstat in the opened directory and its open: a
// relative symlink to the in-directory decoy g, which os.Root follows (an
// absolute one it refuses as an escape), or g renamed over it.
func TestReadersRefuseASwappedFile(t *testing.T) {
	relink := func(_, f string) error { return os.Symlink("g", f) }
	for rname, read := range swapReaders {
		for how, put := range map[string]func(g, f string) error{"renamed": os.Rename, "symlinked": relink} {
			t.Run(rname+"/"+how, func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), "d")
				swapDir(t, dir, "real")
				writeCred(t, dir, "g", []byte("decoy"), 0o400)
				f := filepath.Join(dir, "f")
				onSwap(t, "file", func() error {
					if err := os.Remove(f); err != nil {
						return err
					}
					return put(filepath.Join(dir, "g"), f)
				})
				if b, err := read(dir); !errors.Is(err, errFileSwapped) {
					t.Fatalf("read %q, %v; want errFileSwapped", b, err)
				}
			})
		}
	}
}

// f is a relative symlink to g in the same directory. os.Root follows it
// where O_NOFOLLOW would not, so the Lstat check is what refuses it.
func TestReadersRefuseAnInRootSymlink(t *testing.T) {
	for rname, read := range swapReaders {
		t.Run(rname, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "d")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			writeCred(t, dir, "g", []byte("decoy"), 0o400)
			if err := os.Symlink("g", filepath.Join(dir, "f")); err != nil {
				t.Fatal(err)
			}
			if b, err := read(dir); err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("read %q, %v; want a symlink refusal", b, err)
			}
		})
	}
}

// The directory's mode changed between its Lstat and its open: the same
// directory, now group-writable, is refused on the opened directory's stat.
func TestReadersJudgeTheOpenedDirectory(t *testing.T) {
	for rname, read := range swapReaders {
		t.Run(rname, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "d")
			swapDir(t, dir, "real")
			onSwap(t, "dir", func() error { return os.Chmod(dir, 0o770) }) // #nosec G302 -- the group-writable parent is the case under test
			if b, err := read(dir); err == nil || !strings.Contains(err.Error(), "writable") {
				t.Fatalf("read %q, %v; want the opened directory refused as writable", b, err)
			}
		})
	}
}
