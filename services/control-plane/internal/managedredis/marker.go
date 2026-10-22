package managedredis

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const activationMarkerContents = "concord-managed-redis-v1\n"
const initializingMarkerContents = "concord-managed-redis-initializing-v1\n"

func (c *Controller) markerState() (string, error) {
	if err := checkedPrivateDir(filepath.Dir(c.markerPath)); err != nil {
		return "", errController
	}
	info, err := os.Lstat(c.markerPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 ||
		(info.Size() != int64(len(activationMarkerContents)) && info.Size() != int64(len(initializingMarkerContents))) {
		return "", errController
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return "", errController
	}
	raw, err := os.ReadFile(c.markerPath)
	if err != nil {
		return "", errController
	}
	switch string(raw) {
	case activationMarkerContents:
		return "activated", nil
	case initializingMarkerContents:
		return "initializing", nil
	default:
		return "", errController
	}
}

func (c *Controller) activationMarker() (bool, error) {
	state, err := c.markerState()
	if err != nil || state == "initializing" {
		return false, errController
	}
	return state == "activated", nil
}

func (c *Controller) requireActivationMarker() error {
	exists, err := c.activationMarker()
	if err != nil || !exists {
		return errController
	}
	return nil
}

func (c *Controller) createActivationMarker() error {
	return c.createMarker(activationMarkerContents)
}

func (c *Controller) createMarker(contents string) error {
	state, err := c.markerState()
	if err != nil || state != "" {
		return errController
	}
	file, err := os.OpenFile(c.markerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return errController
	}
	if _, err := io.WriteString(file, contents); err != nil {
		return closeFailedJournal(file)
	}
	if err := file.Sync(); err != nil {
		return closeFailedJournal(file)
	}
	if err := file.Close(); err != nil {
		return errController
	}
	if err := syncMarkerDirectory(filepath.Dir(c.markerPath)); err != nil {
		return errController
	}
	state, err = c.markerState()
	want := "activated"
	if contents == initializingMarkerContents {
		want = "initializing"
	}
	if err != nil || state != want {
		return errController
	}
	return nil
}

func (c *Controller) promoteInitializingMarker() (retErr error) {
	state, err := c.markerState()
	if err != nil || state != "initializing" {
		return errController
	}
	dir := filepath.Dir(c.markerPath)
	file, err := os.CreateTemp(dir, ".activated-*")
	if err != nil {
		return errController
	}
	defer func() {
		if err := os.Remove(file.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			retErr = errController
		}
	}()
	if _, err := io.WriteString(file, activationMarkerContents); err != nil {
		return closeFailedJournal(file)
	}
	if err := file.Sync(); err != nil {
		return closeFailedJournal(file)
	}
	if err := file.Close(); err != nil {
		return errController
	}
	if err := os.Rename(file.Name(), c.markerPath); err != nil {
		return errController
	}
	if err := syncMarkerDirectory(dir); err != nil {
		return errController
	}
	return c.requireActivationMarker()
}

func syncMarkerDirectory(dir string) error {
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
