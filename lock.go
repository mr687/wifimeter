package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockPath names the lock file. The uid keeps two accounts on one machine
// from blocking each other.
func lockPath() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("wifimeter-%d.lock", os.Getuid()))
}

// acquireDaemonLock holds an exclusive lock until the caller closes the
// returned file. Only one sampler may run: two writing the same database lose
// samples to SQLITE_BUSY and duplicate the rest. launchd starts a second one
// readily, because an agent registered under an older label keeps running even
// after its plist file is gone.
func acquireDaemonLock() (*os.File, error) {
	f, err := os.OpenFile(lockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another wifimeter is already running: %w", err)
	}
	return f, nil
}
