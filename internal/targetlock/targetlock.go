// Package targetlock provides cross-process exclusion for named targets.
package targetlock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var targetPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

type Manager struct {
	root *os.Root
}

type Lock struct {
	file     *os.File
	mutex    sync.Mutex
	released bool
}

func New(rootPath string) (*Manager, error) {
	if rootPath == "" || !filepath.IsAbs(rootPath) || filepath.Clean(rootPath) != rootPath {
		return nil, errors.New("lock directory must be a clean absolute path")
	}
	if rootPath == string(filepath.Separator) {
		return nil, errors.New("lock directory must not be the filesystem root")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open lock directory: %w", err)
	}
	return &Manager{root: root}, nil
}

func (manager *Manager) Close() error {
	if manager == nil || manager.root == nil {
		return nil
	}
	return manager.root.Close()
}

func (manager *Manager) Acquire(ctx context.Context, target string) (*Lock, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if !targetPattern.MatchString(target) {
		return nil, fmt.Errorf("target %q must match %s", target, targetPattern.String())
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("acquire target lock: %w", err)
	}

	filename := target + ".lock"
	file, err := manager.openLockFile(filename)
	if err != nil {
		return nil, err
	}
	if err := waitForLock(ctx, file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("acquire target lock %q: %w", target, err)
	}
	return &Lock{file: file}, nil
}

func (manager *Manager) openLockFile(filename string) (*os.File, error) {
	file, err := manager.root.OpenFile(filename, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		return file, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create lock file: %w", err)
	}
	info, err := manager.root.Lstat(filename)
	if err != nil {
		return nil, fmt.Errorf("inspect lock file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("lock path is not a regular file")
	}
	file, err = manager.root.OpenFile(filename, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	return file, nil
}

func (lock *Lock) Release() error {
	if lock == nil {
		return nil
	}
	lock.mutex.Lock()
	defer lock.mutex.Unlock()
	if lock.released {
		return nil
	}
	lock.released = true
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	if unlockErr != nil {
		return fmt.Errorf("release target lock: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close target lock: %w", closeErr)
	}
	return nil
}

func waitForLock(ctx context.Context, file *os.File) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
