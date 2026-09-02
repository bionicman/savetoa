package targetlock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireExcludesSameTargetAcrossManagers(t *testing.T) {
	rootPath := t.TempDir()
	firstManager := newTestManager(t, rootPath)
	secondManager := newTestManager(t, rootPath)
	first, err := firstManager.Acquire(context.Background(), "example-mariadb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Release() })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := secondManager.Acquire(ctx, "example-mariadb"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Acquire() error = %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := secondManager.Acquire(context.Background(), "example-mariadb")
	if err != nil {
		t.Fatalf("Acquire(after release) error = %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestDifferentTargetsCanLockConcurrently(t *testing.T) {
	manager := newTestManager(t, t.TempDir())
	first, err := manager.Acquire(context.Background(), "example-mariadb")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	second, err := manager.Acquire(context.Background(), "example-redis")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
}

func TestAcquireRejectsUnsafeTargetAndSymlink(t *testing.T) {
	rootPath := t.TempDir()
	manager := newTestManager(t, rootPath)
	if _, err := manager.Acquire(context.Background(), "../escape"); err == nil {
		t.Fatal("Acquire(unsafe target) error = nil")
	}

	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("do not lock"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rootPath, "example-mariadb.lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Acquire(context.Background(), "example-mariadb"); err == nil {
		t.Fatal("Acquire(symlink) error = nil")
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	manager := newTestManager(t, t.TempDir())
	lock, err := manager.Acquire(context.Background(), "example-mariadb")
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("second Release() error = %v", err)
	}
}

func TestNewRejectsUnsafeRoot(t *testing.T) {
	if _, err := New("relative"); err == nil {
		t.Fatal("New(relative) error = nil")
	}
	if _, err := New(string(filepath.Separator)); err == nil {
		t.Fatal("New(root) error = nil")
	}
}

func newTestManager(t *testing.T, rootPath string) *Manager {
	t.Helper()
	manager, err := New(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return manager
}
