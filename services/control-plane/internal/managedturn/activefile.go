package managedturn

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// activeFileMode passes the control-plane issuer's 0o027 check: owner and
// group read only.
const activeFileMode os.FileMode = 0o440

// WriteActiveFile atomically replaces dir/turn-key with key (mode 0440, group
// gid, no trailing newline). The control plane reopens the file on every
// admission, so a rename is the linearization point of a promotion. dir must
// be a private directory, and an existing non-regular entry at the target
// (a planted symlink, say) is refused rather than replaced.
func WriteActiveFile(dir string, gid int, key Key) error {
	if !key.valid() {
		return ErrInvalidKey
	}
	if gid < 0 {
		return fmt.Errorf("%w: negative group id", ErrInvalidConfig)
	}
	if err := checkPrivateDir(dir); err != nil {
		return err
	}
	info, err := os.Lstat(filepath.Join(dir, ActiveFileName))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Errorf("managedturn: inspect active file: %w", err)
	case !info.Mode().IsRegular():
		return ErrUnsafePermission
	}
	data := []byte(key.value)
	defer clear(data)
	return writeFileAtomic(dir, ActiveFileName, data, activeFileMode, gid)
}
