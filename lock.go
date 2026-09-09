package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrAlreadyRunning means another sampler holds the lock.
var ErrAlreadyRunning = errors.New("another wifimeter is already running")

// lockPath places the lock beside the database. /tmp is cleared periodically,
// and a lock file removed while the process lives lets a second sampler take
// a fresh inode and run alongside the first.
func lockPath() (string, error) {
	dir, err := dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "wifimeter.lock"), nil
}

// acquireDaemonLock holds an exclusive lock until the caller closes the
// returned file. Only one sampler may run: two writing the same database lose
// samples to SQLITE_BUSY and duplicate the rest. launchd starts a second one
// readily, because an agent registered under an older label keeps running even
// after its plist file is gone.
func acquireDaemonLock() (*os.File, error) {
	path, err := lockPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("%w: %v", ErrAlreadyRunning, err)
	}
	return f, nil
}
