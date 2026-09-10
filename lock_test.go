package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLockPathOutsideTemp(t *testing.T) {
	path, err := lockPath()
	if err != nil {
		t.Fatal(err)
	}
	tmp := os.TempDir()
	if strings.HasPrefix(path, tmp) {
		t.Errorf("lock %q lives under %s, which the system clears", path, tmp)
	}
	data, err := dataDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != data {
		t.Errorf("lock dir = %q, want %q", filepath.Dir(path), data)
	}
}

// isolateLockDir points dataDir at a temp directory. The real lock lives
// beside the live database, so acquiring it here would collide with an agent
// already running on this machine and fail for a reason unrelated to the test.
func isolateLockDir(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func TestAcquireDaemonLockIsExclusive(t *testing.T) {
	isolateLockDir(t)
	first, err := acquireDaemonLock()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	// A second sampler must fail rather than run alongside the first.
	second, err := acquireDaemonLock()
	if err == nil {
		second.Close()
		t.Fatal("second acquire should have failed")
	}
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("err = %v, want ErrAlreadyRunning", err)
	}
}

func TestAcquireDaemonLockReleased(t *testing.T) {
	isolateLockDir(t)
	first, err := acquireDaemonLock()
	if err != nil {
		t.Fatal(err)
	}
	first.Close()

	// Closing releases it, so the next start can take it.
	second, err := acquireDaemonLock()
	if err != nil {
		t.Fatalf("lock not released after close: %v", err)
	}
	second.Close()
}
