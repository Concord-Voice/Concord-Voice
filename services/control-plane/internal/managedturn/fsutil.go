package managedturn

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// lockPollInterval paces the non-blocking flock retries so a waiting
// transition still honours its context.
const lockPollInterval = 20 * time.Millisecond

// checkPrivateDir requires dir to be a real directory (not a symlink), owned
// by the effective user, that no group or other user can write into, so
// nobody but the writer can swap the files it publishes there.
func checkPrivateDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("managedturn: inspect directory: %w", err)
	}
	if !info.IsDir() || !ownedByEUID(info) || info.Mode().Perm()&0o022 != 0 {
		return ErrUnsafePermission
	}
	return nil
}

// ownedByEUID reports whether info belongs to the effective user, so a
// directory or database another account planted is never adopted.
func ownedByEUID(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int64(st.Uid) == int64(os.Geteuid())
}

// dirLock is an exclusive flock held on a directory.
type dirLock struct{ f *os.File }

// lockDir takes an exclusive flock on dir. flock locks belong to the open file
// description, so two lockDir calls conflict even inside one process.
func lockDir(ctx context.Context, dir string) (*dirLock, error) {
	if err := checkPrivateDir(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0) // #nosec G304 -- the caller's configured state directory
	if err != nil {
		return nil, fmt.Errorf("managedturn: open lock directory: %w", err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &dirLock{f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return nil, errors.Join(fmt.Errorf("managedturn: lock directory: %w", err), closeFile(f))
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), closeFile(f))
		case <-time.After(lockPollInterval):
		}
	}
}

// release drops the lock; closing the descriptor releases the flock. Linux
// frees the descriptor, and so the lock, even when close reports an error, and
// a read-only directory descriptor has nothing to flush, so there is nothing
// for a caller to act on.
func (l *dirLock) release() { _ = l.f.Close() }

// syncDir fsyncs dir so a completed rename survives a crash.
func syncDir(dir string) (err error) {
	d, err := os.Open(dir) // #nosec G304 -- dir is the caller's own configured directory
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeFile(d)) }()
	return d.Sync()
}

// writeFileAtomic replaces dir/name with data: same-directory temp file,
// optional group, explicit mode, fsync, rename, directory fsync. Readers see
// the old file or the new one, never a partial or missing file. The rename
// replaces a directory entry and never follows a symlink at the target.
func writeFileAtomic(dir, name string, data []byte, perm os.FileMode, gid int) (err error) {
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return fmt.Errorf("managedturn: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err == nil {
			return
		}
		if cErr := tmp.Close(); cErr != nil && !errors.Is(cErr, os.ErrClosed) {
			err = errors.Join(err, fmt.Errorf("managedturn: close temp file: %w", cErr))
		}
		// A leftover temp file may hold key material: say so, never hide it.
		if rErr := os.Remove(tmpName); rErr != nil && !errors.Is(rErr, fs.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("managedturn: temp file left in %s: %w", dir, rErr))
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("managedturn: write temp file: %w", err)
	}
	if gid >= 0 {
		if err = chownGroup(tmp, gid); err != nil {
			return err
		}
	}
	if err = tmp.Chmod(perm); err != nil {
		return fmt.Errorf("managedturn: chmod temp file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("managedturn: sync temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("managedturn: close temp file: %w", err)
	}
	if err = os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("managedturn: rename into place: %w", err)
	}
	if serr := syncDir(dir); serr != nil {
		// The rename is already visible; report the durability failure
		// without removing a file that is now in place.
		return fmt.Errorf("managedturn: sync directory: %w", serr)
	}
	return nil
}

// chownGroup sets f's group to gid. When the directory's setgid bit already
// produced that group no chown is attempted, so an unprivileged writer works
// without CAP_CHOWN; otherwise a refused chown fails the write closed.
func chownGroup(f *os.File, gid int) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("managedturn: stat temp file: %w", err)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int64(st.Gid) == int64(gid) {
		return nil
	}
	if err := f.Chown(-1, gid); err != nil {
		return fmt.Errorf("managedturn: set file group: %w", err)
	}
	return nil
}
